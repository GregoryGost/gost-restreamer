package twitch

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/coder/websocket"
)

// Discover возвращает один чат Twitch по явному ID вещателя либо ID пользователя
// из подтверждённых полномочий его аккаунта. Сетевых запросов не выполняет.
func (a *Adapter) Discover(ctx context.Context, source core.ChatSource, credentials core.ChatCredentials) (core.ChatDiscovery, error) {
	if ctx == nil {
		return core.ChatDiscovery{}, core.ErrInvalid
	}
	// err передаёт отмену поиска вызывающему коду.
	if err := ctx.Err(); err != nil {
		return core.ChatDiscovery{}, err
	}
	if source.Provider != core.ChatTwitch || source.AccountID == "" || source.AccountID != credentials.AccountID ||
		credentials.Provider != core.ChatTwitch || source.ChannelHint != "" && !validTwitchUserID(source.ChannelHint) {
		return core.ChatDiscovery{State: core.ChatFailed, Reason: core.ChatReasonConfiguration}, nil
	}
	if credentials.Bundle.Validate(core.ChatTwitch) != nil {
		return core.ChatDiscovery{State: core.ChatAuthRequired, Reason: core.ChatReasonCredentials}, nil
	}
	// channelID выбирает явный чат. Без подсказки читается чат пользователя аккаунта.
	channelID := source.ChannelHint
	if channelID == "" {
		channelID = credentials.Bundle.Twitch.UserID
	}
	return core.ChatDiscovery{ChannelID: channelID}, nil
}

// Read открывает один чат req и передаёт события sink с токеном текущей попытки.
// Сначала Twitch подтверждает подписку через Helix. Во время чтения Twitch
// может перенести её на другой сокет. При полном разрыве Manager решает, когда
// начинать новую физическую попытку. Отмена закрывает сокеты и останавливает
// все работы чтения, проверки полномочий и переноса до возврата.
func (a *Adapter) Read(ctx context.Context, req core.ChatReadRequest, sink core.ChatAdapterSink) error {
	if ctx == nil || sink == nil {
		return core.ErrInvalid
	}
	// err прерывает попытку до сетевого запроса.
	if err := ctx.Err(); err != nil {
		return err
	}
	// validationDue отсчитывает срок следующей проверки уже с начала чтения.
	validationDue := time.Now().Add(a.validationInterval)
	if req.Key.Provider != core.ChatTwitch || req.Key.AccountID == "" || !validTwitchUserID(req.Key.ChannelID) {
		return finishEventSub(ctx, req, sink, eventSubFailure{state: core.ChatFailed, reason: core.ChatReasonConfiguration})
	}
	if req.Credentials.Provider != core.ChatTwitch || req.Credentials.AccountID != req.Key.AccountID ||
		req.Credentials.Bundle.Validate(core.ChatTwitch) != nil {
		return finishEventSub(ctx, req, sink, eventSubFailure{state: core.ChatAuthRequired, reason: core.ChatReasonCredentials})
	}
	// credentials содержит начальный набор, полученный Manager перед открытием сокета.
	credentials := req.Credentials.Bundle.Twitch
	// condition привязывает подписку к настроенному чату и подтверждённому пользователю.
	condition := eventSubCondition{BroadcasterID: req.Key.ChannelID, ReaderID: credentials.UserID}
	// opening и stopOpening ограничивают соединение и первое приветствие.
	opening, stopOpening := context.WithTimeout(ctx, a.limits.ConnectTimeout)
	// conn и session описывают первый сокет, failure содержит безопасную причину отказа.
	conn, session, failure := a.openEventSub(opening, a.socketURL)
	stopOpening()
	if conn == nil {
		return finishEventSub(ctx, req, sink, failure)
	}

	// socket начинает принимать кадры и отвечать на Ping до завершения Helix.
	socket := a.startEventSubSocket(ctx, conn, session)
	// subscribeCtx и stopSubscribe ограничивают запрос, включая ожидание HTTP-слота.
	subscribeCtx, stopSubscribe := context.WithDeadline(ctx, socket.welcomeAt.Add(socket.keepalive))
	// subscription устанавливает точную идентичность. failure и status описывают ответ Helix.
	subscription, failure, status := a.subscribeEventSub(subscribeCtx, req, session.ID, condition, credentials)
	stopSubscribe()
	if failure.state != "" {
		socket.stop()
		// err передаёт отмену или отзыв попытки владельцем полномочий.
		failure, err := a.subscriptionAuthorization(ctx, req, status, failure)
		if err != nil {
			return err
		}
		return finishEventSub(ctx, req, sink, failure)
	}
	// err показывает, подтвердил ли sink активность точного worker token.
	if err := sink.Status(ctx, req.Token, core.ChatConnected, core.ChatReasonNone); err != nil {
		socket.stop()
		return err
	}
	// err отделяет отмену или отказ sink от классифицированной upstream-потери.
	failure, err := a.readSubscribed(ctx, req, sink, socket, subscription, validationDue)
	if err != nil {
		return err
	}
	return finishEventSub(ctx, req, sink, failure)
}

// finishEventSub публикует итог попытки req. Подтверждённый разрыв сначала
// попадает в ленту как Gap, поскольку конечный Status закрывает допуск sink.
// При отмене callbacks больше не выполняются.
func finishEventSub(ctx context.Context, req core.ChatReadRequest, sink core.ChatAdapterSink, failure eventSubFailure) error {
	// err передаёт отмену без новых callbacks в sink.
	if err := ctx.Err(); err != nil {
		return err
	}
	if failure.interrupted {
		// err означает, что событие о пробеле не принято текущим поколением.
		if err := sink.Gap(ctx, req.Token, core.ChatReasonUpstreamGap); err != nil {
			return err
		}
	}
	// err сообщает об отказе установить итоговый статус для точной попытки.
	if err := sink.Status(ctx, req.Token, failure.state, failure.reason); err != nil {
		return err
	}
	if failure.state == core.ChatRetrying {
		return &core.ChatReadRetry{RetryAfter: failure.retryAfter}
	}
	return nil
}

// eventSubPacket передаёт один принятый кадр или безопасную причину конца чтения.
type eventSubPacket struct {
	// frame содержит разобранную оболочку EventSub.
	frame eventSubFrame
	// reason отличает последний отказ чтения от успешно разобранного кадра.
	reason core.ChatReason
	// observedAt позволяет сопоставить отказ старого сокета с подтверждением нового.
	observedAt time.Time
}

// eventSubSocket принимает данные одного сокета. Вызывающий читает frames
// и останавливает чтение до освобождения своего слота подключения.
type eventSubSocket struct {
	// conn принадлежит этой физической попытке WebSocket.
	conn *websocket.Conn
	// frames передаёт кадры в порядке чтения. Допускается один готовый кадр.
	frames <-chan eventSubPacket
	// cancel снимает ожидание сетевого сокета и передачу кадра.
	cancel context.CancelFunc
	// done закрывается при завершении горутины чтения.
	done <-chan struct{}
	// welcomeAt задаёт начало срока, в который Helix должен подтвердить начальную подписку.
	welcomeAt time.Time
	// keepalive ограничивает тишину в активном EventSub-сеансе.
	keepalive time.Duration
}

// startEventSubSocket запускает приём кадров conn после проверенного session.
// Пока Read ждёт Helix или перенос, библиотека может отвечать на Ping
// в горутине приёма. Эта горутина ничего не публикует в sink.
func (a *Adapter) startEventSubSocket(parent context.Context, conn *websocket.Conn, session eventSubSession) *eventSubSocket {
	// ctx и cancel принадлежат приёму на одном сокете.
	ctx, cancel := context.WithCancel(parent)
	// frames передаёт принятые кадры и причину последнего отказа владельцу.
	frames := make(chan eventSubPacket, 1)
	// done подтверждает освобождение горутины приёма.
	done := make(chan struct{})
	// Цикл читает кадры по одному и отдаёт владельцу либо завершается при отмене.
	go func() {
		defer close(done)
		defer close(frames)
		for ctx.Err() == nil {
			// frame содержит один проверенный конверт. reason завершает цикл при отказе чтения.
			frame, reason := readEventSubFrame(ctx, conn, a.limits.UpstreamMessageBytes)
			select {
			case frames <- eventSubPacket{frame: frame, reason: reason, observedAt: time.Now()}:
			case <-ctx.Done():
				return
			}
			if reason != core.ChatReasonNone {
				return
			}
		}
	}()
	return &eventSubSocket{
		conn: conn, frames: frames, cancel: cancel, done: done,
		welcomeAt: session.welcomeAt, keepalive: time.Duration(session.KeepaliveSeconds) * time.Second,
	}
}

// stop закрывает физическое соединение и дожидается конца чтения сокета s.
// Если приём заблокирован передачей кадра, отмена снимает это ожидание.
func (s *eventSubSocket) stop() {
	s.cancel()
	_ = s.conn.CloseNow()
	<-s.done
}

// eventSubHandover содержит результат установки второго сокета для переноса.
type eventSubHandover struct {
	// conn присутствует после успешного приветствия на новом сокете.
	conn *websocket.Conn
	// session задаёт параметры приёма после переноса подписки.
	session eventSubSession
	// release освобождает дополнительный слот после закрытия одного из двух сокетов.
	release func()
	// failure содержит безопасное объяснение неудавшегося переноса.
	failure eventSubFailure
}

// discard закрывает подготовленный сокет и отдаёт дополнительный слот, если
// результат переноса уже нельзя принять в текущее чтение.
func (h eventSubHandover) discard() {
	if h.conn != nil {
		_ = h.conn.CloseNow()
	}
	if h.release != nil {
		h.release()
	}
}

// openHandover ждёт допуска к дополнительному соединению в пределах grace
// и принимает первое приветствие по неизменённому socketURL. При отказе
// освобождает слот здесь. При успехе передаёт владение сокетом и слотом Read.
func (a *Adapter) openHandover(grace context.Context, req core.ChatReadRequest, socketURL string) eventSubHandover {
	// release занят до фактического закрытия одного из сокетов. В прямом тесте его может не быть.
	var release func()
	if req.AcquireConnection != nil {
		// err сообщает об отказе получить дополнительный слот до закрытия старого.
		var err error
		release, err = req.AcquireConnection(grace)
		if err != nil || release == nil {
			return eventSubHandover{failure: eventSubFailure{state: core.ChatRetrying, reason: core.ChatReasonLimit}}
		}
	}
	// opening и cancel ограничивают новое рукопожатие и приветствие также локальным пределом.
	opening, cancel := context.WithTimeout(grace, a.limits.ConnectTimeout)
	defer cancel()
	// conn и session передают готовый сокет, failure объясняет отказ без платформенных данных.
	conn, session, failure := a.openEventSub(opening, socketURL)
	if conn == nil && release != nil {
		release()
		release = nil
	}
	return eventSubHandover{conn: conn, session: session, release: release, failure: failure}
}

// readSubscribed читает подтверждённую подписку. Только эта горутина вызывает
// sink. Пока готовится замена сокета или проверяются полномочия, старый сокет
// продолжает получать уведомления. Все принадлежащие попытке работы
// останавливаются до возврата безопасного исхода. validationDue учитывает
// время, уже проведённое в начальном рукопожатии и запросе подписки.
func (a *Adapter) readSubscribed(ctx context.Context, req core.ChatReadRequest, sink core.ChatAdapterSink, socket *eventSubSocket, subscription eventSubSubscription, validationDue time.Time) (eventSubFailure, error) {
	// working отменяет вспомогательный перенос и проверку токенов при выходе.
	working, cancel := context.WithCancel(ctx)
	// workers ждёт запущенные в этом чтении операции и освобождение дополнительного слота.
	var workers sync.WaitGroup
	// Функция завершает вспомогательные работы и последний используемый сокет.
	defer func() {
		cancel()
		socket.stop()
		workers.Wait()
	}()

	// idle начинает окно тишины после подтверждения подписки и допуска к обработке кадров.
	// Срок от welcome уже использован для Helix. За время ожидания Manager мог накопиться keepalive.
	idle := time.NewTimer(socket.keepalive)
	defer idle.Stop()
	// idleAt отключает обычный контроль тишины на время переноса с отдельным сроком.
	idleAt := idle.C
	// closing отсчитывает крайний срок передачи. closeAt включён только после session_reconnect.
	var closing *time.Timer
	var closeAt <-chan time.Time
	// Функция убирает таймер переноса, если чтение завершилось до его срабатывания.
	defer func() {
		if closing != nil {
			closing.Stop()
		}
	}()
	// transfer получает один готовый сокет либо отказ переноса. nil отключает ожидание.
	var transfer <-chan eventSubHandover
	// frames указывает активную очередь. После закрытия старого сокета цикл
	// может дождаться уже начатого переноса в пределах оставшегося срока.
	frames := socket.frames
	// disconnectedAt хранит время отказа старого сокета, замеченного до результата переноса.
	var disconnectedAt time.Time
	// end объясняет итог потери сокета. Неудачный перенос может уточнить причину и задержку.
	end := eventSubFailure{state: core.ChatRetrying, reason: core.ChatReasonNetwork, interrupted: true}

	// validationTimer задаёт следующую проверку OAuth. validationAt выключается на время запроса.
	var validationTimer *time.Timer
	var validationAt <-chan time.Time
	// validated получает итог текущего запроса владельцу OAuth.
	var validated <-chan error
	// validationStarted задаёт начало проверки для расчёта следующего срока.
	var validationStarted time.Time
	if req.RefreshCredentials != nil && a.validationInterval > 0 {
		validationTimer = time.NewTimer(time.Until(validationDue))
		defer validationTimer.Stop()
		validationAt = validationTimer.C
	}
	// validationBackoff растёт при временном отказе до установленного предела ожидания.
	validationBackoff := a.limits.ReconnectMinBackoff

	for {
		select {
		case <-ctx.Done():
			return eventSubFailure{}, ctx.Err()
		case <-idleAt:
			return end, nil
		case <-closeAt:
			return end, nil
		case <-validationAt:
			// result передаёт только ошибку периодической проверки, без полномочий.
			result := make(chan error, 1)
			validationStarted = time.Now()
			// Функция сверяет набор через владельца аккаунта и возвращает итог в канал.
			workers.Go(func() {
				result <- a.checkReaderCredentials(working, req)
			})
			validationAt = nil
			validated = result
		// err содержит безопасный результат проверки текущего аккаунта.
		case err := <-validated:
			validated = nil
			if errors.Is(err, core.ErrInvalid) {
				return eventSubFailure{state: core.ChatAuthRequired, reason: core.ChatReasonCredentials}, nil
			}
			if errors.Is(err, core.ErrStale) {
				return eventSubFailure{}, err
			}
			// delay ограничивает частоту повторной проверки при временном отказе владельца.
			delay := time.Until(validationStarted.Add(a.validationInterval))
			if err == nil {
				validationBackoff = a.limits.ReconnectMinBackoff
			} else {
				delay = validationBackoff
				validationBackoff += min(validationBackoff, a.limits.ReconnectMaxBackoff-validationBackoff)
				// retry может содержать более долгий срок, указанный Twitch.
				var retry *core.ChatReadRetry
				if errors.As(err, &retry) {
					delay = max(delay, retry.RetryAfter)
				}
			}
			validationTimer.Reset(delay)
			validationAt = validationTimer.C

		// next приносит успешно перенесённый сокет либо причину неудавшегося переноса.
		case next := <-transfer:
			transfer = nil
			if next.conn == nil {
				end = next.failure
				end.interrupted = true
				if !disconnectedAt.IsZero() {
					return end, nil
				}
				continue
			}
			if !disconnectedAt.IsZero() && next.session.welcomeAt.After(disconnectedAt) {
				next.discard()
				return end, nil
			}
			// old остаётся источником всех кадров, уже принятых до закрытия сокета.
			old := socket
			socket = a.startEventSubSocket(ctx, next.conn, next.session)
			frames = socket.frames
			_ = old.conn.CloseNow()
			if next.release != nil {
				next.release()
			}
			// failure и err передают отзыв подписки или ошибку sink из уже принятых кадров.
			failure, err := drainEventSub(ctx, req, sink, old, subscription, next.session.welcomeAt)
			old.stop()
			if failure.state != "" || err != nil {
				return failure, err
			}
			subscription.Transport.SessionID = next.session.ID
			closing.Stop()
			closing, closeAt = nil, nil
			disconnectedAt = time.Time{}
			idle.Reset(socket.keepalive)
			idleAt = idle.C

		// packet передаёт очередной кадр, open показывает доступность канала приёма.
		case packet, open := <-frames:
			if !open || packet.reason != core.ChatReasonNone {
				if packet.reason == core.ChatReasonLimit {
					end.reason = packet.reason
					return end, nil
				}
				if open && transfer != nil {
					// Новый сокет мог подтвердиться раньше отказа старого.
					// Сравнение меток не зависит от порядка готовых ветвей select.
					disconnectedAt = packet.observedAt
					frames = nil
					continue
				}
				return end, nil
			}
			// frame различает подтверждение активности, уведомление и смену сокета.
			frame := packet.frame
			if frame.Metadata.Kind == "session_reconnect" {
				if closeAt != nil {
					continue
				}
				// session описывает перенос с текущего физического сокета.
				session := frame.Payload.Session
				if session == nil || session.ID != subscription.Transport.SessionID ||
					session.Status != "reconnecting" || !trustedReconnectURL(a.socketURL, session.ReconnectURL) {
					// Недействительная команда не меняет текущую подписку и её срок ожидания событий.
					continue
				}
				// result передаёт владение новым сокетом только при активном читателе.
				result := make(chan eventSubHandover)
				closing = time.NewTimer(reconnectGrace)
				closeAt, transfer = closing.C, result
				idle.Stop()
				idleAt = nil
				// Функция выполняет перенос и закрывает сокет, если читатель уже ушёл.
				workers.Go(func() {
					// grace и stop ограничивают допуск и рукопожатие общим сроком переноса.
					grace, stop := context.WithTimeout(working, reconnectGrace)
					defer stop()
					// next содержит новый сокет, пока его не принял основной цикл.
					next := a.openHandover(grace, req, session.ReconnectURL)
					select {
					case result <- next:
					case <-working.Done():
						next.discard()
					}
				})
				continue
			}
			// alive отмечает подтверждение активности текущего чата, failure и err завершают чтение при отказе.
			alive, failure, err := acceptEventSubFrame(ctx, req, sink, subscription, frame)
			if failure.state != "" || err != nil {
				return failure, err
			}
			if alive && idleAt != nil {
				idle.Reset(socket.keepalive)
			}
		}
	}
}

// drainEventSub публикует из old уже разобранные кадры до переключения
// на уведомления нового сокета. Сетевой отказ закрываемого сокета ожидаем,
// если он произошёл после welcomeAt. Более ранняя потеря и превышение
// лимита кадра нарушают непрерывность. Отзыв и ошибки sink сохраняют силу.
func drainEventSub(ctx context.Context, req core.ChatReadRequest, sink core.ChatAdapterSink, old *eventSubSocket, subscription eventSubSubscription, welcomeAt time.Time) (eventSubFailure, error) {
	// packet содержит очередной кадр, принятый старым сокетом до его закрытия.
	for packet := range old.frames {
		if packet.reason != core.ChatReasonNone {
			if packet.observedAt.Before(welcomeAt) || packet.reason == core.ChatReasonLimit {
				return eventSubFailure{state: core.ChatRetrying, reason: packet.reason, interrupted: true}, nil
			}
			break
		}
		// failure и err сообщают о завершении подписки или запрете дальнейшей публикации.
		_, failure, err := acceptEventSubFrame(ctx, req, sink, subscription, packet.frame)
		if failure.state != "" || err != nil {
			return failure, err
		}
	}
	return eventSubFailure{}, ctx.Err()
}

// acceptEventSubFrame проверяет, что уведомление относится к subscription,
// и передаёт его в sink. Возвращает признак активности только для keepalive
// или подходящего уведомления. Отзыв точной подписки завершает чтение.
func acceptEventSubFrame(ctx context.Context, req core.ChatReadRequest, sink core.ChatAdapterSink, subscription eventSubSubscription, frame eventSubFrame) (bool, eventSubFailure, error) {
	switch frame.Metadata.Kind {
	case "session_keepalive":
		return frame.Payload.Session == nil && frame.Payload.Subscription == nil && frame.Payload.Event == nil, eventSubFailure{}, nil
	case "notification", "revocation":
		// actual связывает конверт с принятой подпиской текущего сокета и авторизованного пользователя.
		actual := frame.Payload.Subscription
		if frame.Metadata.Type != subscription.Type || frame.Metadata.Version != subscription.Version ||
			actual == nil || actual.ID != subscription.ID || !actual.matches(subscription) {
			return false, eventSubFailure{}, nil
		}
		if frame.Metadata.Kind == "revocation" {
			return false, revokedEventSub(actual.Status), nil
		}
		if actual.Status != "enabled" {
			return false, eventSubFailure{}, nil
		}
		// message получает только публичные поля сообщения, ok сообщает, что оно пригодно для публикации.
		message, ok := normalizeChatMessage(frame.Payload.Event, subscription.Condition.BroadcasterID, frame.Metadata.Timestamp)
		if !ok {
			return false, eventSubFailure{}, nil
		}
		// err может означать переполнение Hub или отказать в допуске отдельного сообщения.
		err := sink.Message(ctx, req.Token, message)
		// При подтверждении замены аккаунта Manager временно отклоняет сообщения.
		// Подтверждённый отзыв читателя отдельно отменяет его сетевой контекст.
		if errors.Is(err, core.ErrNotReady) || errors.Is(err, core.ErrStale) && ctx.Err() == nil {
			return true, eventSubFailure{}, nil
		}
		return true, eventSubFailure{}, err
	default:
		return false, eventSubFailure{}, nil
	}
}

// revokedEventSub переводит точную причину status от Twitch в безопасный
// итог для источника. Полномочия аккаунта требуют замены только при отзыве.
func revokedEventSub(status string) eventSubFailure {
	switch status {
	case "authorization_revoked":
		return eventSubFailure{state: core.ChatAuthRequired, reason: core.ChatReasonCredentials}
	case "user_removed", "chat_user_banned":
		return eventSubFailure{state: core.ChatFailed, reason: core.ChatReasonUnavailable}
	case "version_removed":
		return eventSubFailure{state: core.ChatFailed, reason: core.ChatReasonConfiguration}
	default:
		return eventSubFailure{state: core.ChatRetrying, reason: core.ChatReasonNetwork}
	}
}

// checkReaderCredentials просит владельца проверить текущий аккаунт req.
// При ответе сверяет приложение и пользователя, чтобы в рамках этого
// сокета не принять набор другого читателя или приложения.
func (a *Adapter) checkReaderCredentials(ctx context.Context, req core.ChatReadRequest) error {
	// checkCtx и cancel ограничивают проверку и возможный обмен пары.
	checkCtx, cancel := context.WithTimeout(ctx, a.limits.HTTPTimeout)
	defer cancel()
	// current содержит проверенный набор текущего аккаунта. err отделяет отзыв от временного отказа.
	current, err := req.RefreshCredentials(checkCtx)
	if err != nil {
		return err
	}
	if current.Provider != core.ChatTwitch || current.AccountID != req.Key.AccountID ||
		current.Bundle.Validate(core.ChatTwitch) != nil ||
		current.Bundle.Twitch.UserID != req.Credentials.Bundle.Twitch.UserID ||
		current.Bundle.Twitch.ClientID != req.Credentials.Bundle.Twitch.ClientID {
		return core.ErrInvalid
	}
	return nil
}

// subscriptionAuthorization уточняет ответ 401 или 403 через владельца
// аккаунта. Валидный набор не устраняет запрет канала. Для 401 Manager
// может открыть новую попытку со свежим набором, без повторного POST здесь.
func (a *Adapter) subscriptionAuthorization(ctx context.Context, req core.ChatReadRequest, status int, failure eventSubFailure) (eventSubFailure, error) {
	if req.RefreshCredentials == nil || status != http.StatusUnauthorized && status != http.StatusForbidden {
		return failure, nil
	}
	// err содержит результат проверки в том же аккаунте, которому принадлежит reader.
	err := a.checkReaderCredentials(ctx, req)
	if ctx.Err() != nil {
		return eventSubFailure{}, ctx.Err()
	}
	switch {
	case errors.Is(err, core.ErrInvalid):
		return eventSubFailure{state: core.ChatAuthRequired, reason: core.ChatReasonCredentials}, nil
	case errors.Is(err, core.ErrStale):
		return eventSubFailure{}, err
	case err == nil && status == http.StatusForbidden:
		return failure, nil
	default:
		// retry принимает нижнюю границу ожидания, если временный отказ пришёл от Twitch.
		var retry *core.ChatReadRetry
		if errors.As(err, &retry) {
			return eventSubFailure{state: core.ChatRetrying, reason: core.ChatReasonQuota, retryAfter: retry.RetryAfter}, nil
		}
		return eventSubFailure{state: core.ChatRetrying, reason: core.ChatReasonNetwork}, nil
	}
}
