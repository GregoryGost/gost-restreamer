package vkvideolive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	// connectID связывает команду открытия сеанса с её ответом.
	connectID = 1
	// subscribeID связывает команду подписки на точный канал с её ответом.
	subscribeID = 2
	// pingGrace даёт серверной посылке Centrifugo время на доставку сверх объявленного интервала.
	pingGrace = 10 * time.Second
)

// command содержит одну исходящую команду протокола Centrifugo.
type command struct {
	// ID связывает запрос с подтверждением в том же соединении.
	ID int `json:"id"`
	// Connect отправляет токен только при установлении сеанса.
	Connect *connectCommand `json:"connect,omitempty"`
	// Subscribe задаёт единственный публичный канал этой попытки.
	Subscribe *subscribeCommand `json:"subscribe,omitempty"`
}

// connectCommand предъявляет выданное DevAPI право на открытие соединения.
type connectCommand struct {
	// Token содержит одно краткоживущее полномочие WebSocket.
	Token string `json:"token"`
}

// subscribeCommand выбирает канал, для которого требуется подтверждение подписки.
type subscribeCommand struct {
	// Channel содержит точное имя подписки, построенное по ID VK.
	Channel string `json:"channel"`
}

// protocolReply принимает необходимые для чтения варианты сообщений Centrifugo.
type protocolReply struct {
	// ID сопоставляет ответ с ожидаемой командой. Для push и ping идентификатор отсутствует.
	ID int `json:"id"`
	// Connect содержит подтверждение сеанса и параметры heartbeat.
	Connect *connectAck `json:"connect"`
	// Subscribe подтверждает подписку даже пустым JSON-объектом.
	Subscribe *struct{} `json:"subscribe"`
	// Error содержит код отказа в ответе на команду.
	Error *protocolCode `json:"error"`
	// Push содержит событие без идентификатора команды.
	Push *protocolPush `json:"push"`
}

// connectAck содержит минимальный результат открытия сеанса Centrifugo.
type connectAck struct {
	// Client подтверждает назначение сервером идентификатора клиента.
	Client string `json:"client"`
	// Ping указывает период серверных ping в секундах либо ноль при отключённом heartbeat.
	Ping int64 `json:"ping"`
	// Pong указывает, требует ли сервер явного ответа на ping протокола JSON.
	Pong bool `json:"pong"`
}

// protocolPush содержит публикацию или управляющее событие текущего соединения.
type protocolPush struct {
	// Channel ограничивает обработку публикаций и unsubscribe подтверждённой подпиской.
	Channel string `json:"channel"`
	// Publication содержит отдельные данные сообщения площадки.
	Publication *struct {
		// Data сохраняет исходный JSON VK до разбора по формату чата.
		Data json.RawMessage `json:"data"`
	} `json:"pub"`
	// Unsubscribe описывает отзыв подписки канала.
	Unsubscribe *protocolCode `json:"unsubscribe"`
	// Disconnect описывает закрытие соединения или требование открыть новое.
	Disconnect *protocolCode `json:"disconnect"`
}

// protocolCode содержит только машинный код из управляющего события.
type protocolCode struct {
	// Code выбирает итог чтения по контракту Centrifugo v4.
	Code int `json:"code"`
}

// socketSession хранит состояние одной подписки в горутине чтения.
// pending переходит от connectID к subscribeID, затем к нулю после ACK.
// Сообщения аудитории передаются только после последнего перехода.
type socketSession struct {
	// conn принадлежит попытке до выхода из readSocket.
	conn *websocket.Conn
	// channel содержит полное имя ожидаемого канала Centrifugo.
	channel string
	// pending указывает единственную команду, ожидающую подтверждения.
	pending int
	// idleTimeout содержит объявленный период heartbeat с запасом доставки либо ноль.
	idleTimeout time.Duration
	// pong определяет необходимость ответа на пустое сообщение Centrifugo.
	pong bool
}

// readSocket открывает WebSocket по свежему token и владеет им до выхода.
// Сообщения идут в sink в порядке строк, включая публикации в кадре с ACK.
// Ожидание CONNECTED выполняется отдельно и при отказе отменяет чтение.
// Перед возвратом отменяет управляющий вызов и дожидается его выхода.
func (a *Adapter) readSocket(ctx context.Context, req core.ChatReadRequest, sink core.ChatAdapterSink, token string) (readFailure, bool, error) {
	// attempt и cancel принадлежат сокету и управляющему вызову.
	attempt, cancel := context.WithCancel(ctx)
	// opening ограничивает Upgrade и оба подтверждения одним сроком. stopOpening снимает таймер.
	opening, stopOpening := context.WithTimeout(attempt, a.limits.ConnectTimeout)
	defer stopOpening()
	// client сохраняет запрет перенаправлений для исходного Upgrade.
	client := *a.httpClient
	client.CheckRedirect = refuseRedirect
	// conn передаёт владение сокетом. response содержит статус HTTP при отклонении, err сообщает об отказе Upgrade.
	conn, response, err := websocket.Dial(opening, a.socketURL, &websocket.DialOptions{
		HTTPClient: &client,
		HTTPHeader: http.Header{"Origin": {socketOrigin}},
	})
	if err != nil {
		cancel()
		if response != nil {
			return a.httpFailure(httpResult{status: response.StatusCode, retryAfter: response.Header.Get("Retry-After")}, false), false, nil
		}
		return networkFailure(), false, nil
	}
	conn.SetReadLimit(int64(a.limits.UpstreamMessageBytes))
	// session связывает подтверждения и публикации только с текущим сокетом и req.
	session := socketSession{conn: conn, channel: "channel-chat:" + req.Key.ChannelID, pending: connectID}

	// controlResult принимает результат CONNECTED. nil у канала означает, что ACK подписки ещё не получен.
	var controlResult chan error
	// acknowledge запускает ровно один вызов CONNECTED после ACK. Буфер позволяет
	// сообщить об отказе сразу и разбудить чтение отменой, даже если оно ждёт сокет.
	acknowledge := func() {
		controlResult = make(chan error, 1)
		// Горутина передаёт статус текущей попытки. Отмена при закрытии снимает
		// ожидание конфигурационного барьера и не считается отказом sink.
		go func() {
			// statusErr содержит отказ sink до отмены попытки либо результат штатного завершения.
			statusErr := sink.Status(attempt, req.Token, core.ChatConnected, core.ChatReasonNone)
			if attempt.Err() != nil {
				statusErr = nil
			}
			if statusErr != nil {
				cancel()
			}
			controlResult <- statusErr
		}()
	}
	// failure содержит единственный отказ протокола либо транспорта, который закончил текущую подписку.
	failure := networkFailure()
	if wsjson.Write(opening, conn, command{ID: connectID, Connect: &connectCommand{Token: token}}) == nil {
		failure = a.receive(attempt, opening, req, sink, &session, acknowledge)
	}

	_ = conn.CloseNow()
	cancel()
	if controlResult != nil {
		// statusErr имеет приоритет над сетевым отказом, поскольку sink отозвал управляющий допуск.
		if statusErr := <-controlResult; statusErr != nil {
			return readFailure{}, true, statusErr
		}
	}
	return failure, session.pending == 0, nil
}

// receive читает ограниченные сообщения WebSocket в session. opening применяется
// до ACK подписки, затем чтение подчиняется сроку heartbeat и отмене attempt.
// req и sink задают неизменную привязку обратных вызовов. acknowledge запускает
// публикацию CONNECTED сразу после протокольного подтверждения.
func (a *Adapter) receive(attempt, opening context.Context, req core.ChatReadRequest, sink core.ChatAdapterSink, session *socketSession, acknowledge func()) readFailure {
	for attempt.Err() == nil {
		// readCtx выбирает срок текущей фазы. cancel освобождает таймер сердцебиения при получении кадра.
		readCtx := attempt
		if session.pending != 0 {
			readCtx = opening
		}
		// cancel завершает только срок ожидания текущего кадра, если heartbeat включён.
		var cancel context.CancelFunc
		if session.idleTimeout > 0 {
			readCtx, cancel = context.WithTimeout(readCtx, session.idleTimeout)
		}
		// kind обозначает тип кадра, data содержит полное WebSocket-сообщение, err описывает отказ чтения.
		kind, data, err := session.conn.Read(readCtx)
		if cancel != nil {
			cancel()
		}
		if err != nil {
			return socketReadFailure(err)
		}
		if kind != websocket.MessageText || !utf8.Valid(data) {
			return networkFailure()
		}
		// consumed отражает наличие хотя бы одной строки протокола в кадре.
		consumed := false
		// line содержит одну JSON-запись из последовательности Centrifugo. Пустой хвост после LF пропускается.
		for line := range bytes.SplitSeq(data, []byte{'\n'}) {
			if attempt.Err() != nil {
				return networkFailure()
			}
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			consumed = true
			// reply несёт проверяемую оболочку команды, push либо heartbeat.
			var reply protocolReply
			if line[0] != '{' || json.Unmarshal(line, &reply) != nil {
				return networkFailure()
			}
			// failure появляется, когда запись прерывает протокол или завершает подписку.
			failure := a.receiveReply(attempt, opening, req, sink, session, reply, len(bytes.TrimSpace(line[1:len(line)-1])) == 0, acknowledge)
			if failure.state != "" {
				return failure
			}
		}
		if !consumed {
			return networkFailure()
		}
	}
	return networkFailure()
}

// receiveReply обрабатывает одну запись reply на сокете session. heartbeat
// отмечает пустой объект Centrifugo. Команды завершаются только подходящим ACK.
// После ACK подписки публикации передаются в sink с исходным req.Token.
func (a *Adapter) receiveReply(attempt, opening context.Context, req core.ChatReadRequest, sink core.ChatAdapterSink, session *socketSession, reply protocolReply, heartbeat bool, acknowledge func()) readFailure {
	if reply.ID != 0 || reply.Connect != nil || reply.Subscribe != nil || reply.Error != nil {
		return session.acceptAck(opening, reply, acknowledge)
	}
	if heartbeat && session.pong {
		// writeCtx ограничивает запись pong, cancel освобождает таймер после записи.
		writeCtx, cancel := context.WithTimeout(attempt, a.limits.ConnectTimeout)
		defer cancel()
		// err означает, что ответ на heartbeat не удалось отправить.
		if err := session.conn.Write(writeCtx, websocket.MessageText, []byte("{}")); err != nil {
			return networkFailure()
		}
		return readFailure{}
	}
	if reply.Push == nil {
		return readFailure{}
	}
	if reply.Push.Disconnect != nil {
		return disconnectFailure(reply.Push.Disconnect.Code)
	}
	if reply.Push.Channel != session.channel {
		return readFailure{}
	}
	if reply.Push.Unsubscribe != nil {
		return unsubscribeFailure(reply.Push.Unsubscribe.Code)
	}
	if session.pending == 0 && reply.Push.Publication != nil {
		// message сохраняет данные только текущей подписки. ok исключает неизвестное или неподходящее событие.
		if message, ok := normalizePublication(reply.Push.Publication.Data, req.Key.ChannelID); ok {
			// Отказ отдельного сообщения не отзывает право читать дальнейшие публикации.
			_ = sink.Message(attempt, req.Token, message)
		}
	}
	return readFailure{}
}

// acceptAck принимает reply только для ожидаемой команды и этапа подписки.
// opening ограничивает запись subscribe, acknowledge запускается после точного ACK.
func (s *socketSession) acceptAck(opening context.Context, reply protocolReply, acknowledge func()) readFailure {
	if opening.Err() != nil || s.pending == 0 || reply.ID != s.pending || reply.Push != nil {
		return networkFailure()
	}
	if reply.Error != nil {
		if reply.Connect != nil || reply.Subscribe != nil {
			return networkFailure()
		}
		return commandFailure(reply.Error.Code)
	}
	switch s.pending {
	case connectID:
		if reply.Connect == nil || reply.Subscribe != nil || !validRequiredText(reply.Connect.Client, core.ChatIDBytes) ||
			reply.Connect.Ping < 0 || reply.Connect.Ping > int64((time.Duration(math.MaxInt64)-pingGrace)/time.Second) ||
			reply.Connect.Ping == 0 && reply.Connect.Pong {
			return networkFailure()
		}
		if reply.Connect.Ping > 0 {
			s.idleTimeout = time.Duration(reply.Connect.Ping)*time.Second + pingGrace
		}
		s.pong = reply.Connect.Pong
		if wsjson.Write(opening, s.conn, command{ID: subscribeID, Subscribe: &subscribeCommand{Channel: s.channel}}) != nil {
			return networkFailure()
		}
		s.pending = subscribeID
	case subscribeID:
		if reply.Subscribe == nil || reply.Connect != nil {
			return networkFailure()
		}
		s.pending = 0
		acknowledge()
	}
	return readFailure{}
}

// networkFailure обозначает временный отказ без исходных диагностических данных.
func networkFailure() readFailure {
	return readFailure{state: core.ChatRetrying, reason: core.ChatReasonNetwork}
}

// commandFailure различает подтверждённый запрет по code и временный отказ.
// Просроченный WS-токен (109 или 110) обрабатывается следующей попыткой.
func commandFailure(code int) readFailure {
	switch code {
	case 102, 103:
		return readFailure{state: core.ChatFailed, reason: core.ChatReasonUnavailable}
	case 104, 107:
		return readFailure{state: core.ChatFailed, reason: core.ChatReasonConfiguration}
	case 111:
		return readFailure{state: core.ChatRetrying, reason: core.ChatReasonQuota}
	default:
		return networkFailure()
	}
}

// socketReadFailure переводит безопасную причину err в исход клиента.
// Код Close относится к WebSocket, в том числе к завершению Centrifugo.
func socketReadFailure(err error) readFailure {
	if errors.Is(err, websocket.ErrMessageTooBig) {
		return readFailure{state: core.ChatRetrying, reason: core.ChatReasonLimit}
	}
	return disconnectFailure(int(websocket.CloseStatus(err)))
}

// disconnectFailure применяет правила повторов Centrifugo v4 к коду закрытия
// code. Истечение 3005 и 3006 входит в диапазон повторного соединения.
func disconnectFailure(code int) readFailure {
	if code >= 3500 && code <= 3999 || code >= 4500 && code <= 4999 {
		return readFailure{state: core.ChatFailed, reason: core.ChatReasonUnavailable}
	}
	return networkFailure()
}

// unsubscribeFailure применяет правила повторов Centrifugo v4 к коду code
// для ожидаемого канала.
func unsubscribeFailure(code int) readFailure {
	if code >= 2000 && code <= 2499 {
		return readFailure{state: core.ChatFailed, reason: core.ChatReasonUnavailable}
	}
	return networkFailure()
}
