package vkvideolive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/coder/websocket"
)

// fixtureWorkerToken возвращает непрозрачный идентификатор одной попытки для
// проверки всех вызовов приёмника.
func fixtureWorkerToken() core.ChatWorkerToken { return core.ChatWorkerToken{0x46, 0x21} }

// awaitFixture возвращает очередное значение из results или завершает t,
// если сценарий перестал выполнять ожидаемые действия.
func awaitFixture[T any](t *testing.T, results <-chan T) T {
	t.Helper()
	// timer ограничивает ожидание сообщения между тестом и горутиной фикстуры.
	timer := time.NewTimer(fixtureTimeout)
	defer timer.Stop()
	select {
	// result содержит полученное значение ожидаемого этапа.
	case result := <-results:
		return result
	case <-timer.C:
		t.Fatal("Истекло ожидание этапа сетевой проверки")
		// zero нужен для компилятора после Fatal, который завершает горутину t.
		var zero T
		return zero
	}
}

// waitFixtureSignal дожидается завершения этапа, которое обозначает signal.
func waitFixtureSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	awaitFixture(t, signal)
}

// fixtureReadRequest формирует запрос чтения с исходным app-токеном и
// действительным токеном попытки. HTTP-допуск можно заменить в конкретном тесте.
func fixtureReadRequest() core.ChatReadRequest {
	return core.ChatReadRequest{
		Token: fixtureWorkerToken(),
		Key: core.ChatWorkerKey{
			Provider: core.ChatVKVideoLive, AccountID: "vk-account", ChannelID: fixtureChannelID,
		},
		Credentials: fixtureCredentials(fixtureAppToken),
		// Фикстура без конкурентного HTTP-бюджета выдаёт допуск до первой отмены.
		AcquireHTTP: func(ctx context.Context) (func(), error) {
			// Функция освобождения ничего не делает, поскольку фикстура не резервирует слот.
			return func() {}, ctx.Err()
		},
	}
}

// fixtureSinkEvent хранит одно наблюдаемое событие с границы адаптер–Manager.
type fixtureSinkEvent struct {
	// state содержит статус при вызове Status.
	state core.ChatSourceState
	// reason объясняет статус или отдельное событие Gap.
	reason core.ChatReason
	// gap отмечает вызов Gap, у которого нет состояния источника.
	gap bool
	// message содержит нормализованные данные только при вызове Message.
	message *core.ChatIncomingMessage
}

// fixtureReadSink проверяет токен попытки и записывает события без блокировки
// на тесте. При необходимости удерживает Connected до отмены его контекста.
type fixtureReadSink struct {
	// t регистрирует ошибки контракта обратного вызова из любой горутины.
	t *testing.T
	// events хранит ограниченную историю вызовов для итоговых проверок.
	events chan fixtureSinkEvent
	// connectedStarted закрывается при входе в удерживаемый Connected.
	connectedStarted chan struct{}
	// connectedExited закрывается при выходе из удерживаемого Connected.
	connectedExited chan struct{}
	// connectedRelease при наличии удерживает выход Connected даже после отмены.
	connectedRelease <-chan struct{}
	// connectedFailure моделирует отказ Manager принять управляющий вызов.
	connectedFailure error
	// rejectMessageID указывает одно сообщение, которое приёмник не принимает.
	rejectMessageID string
}

// newFixtureReadSink создаёт для t приёмник с отдельной очередью событий.
func newFixtureReadSink(t *testing.T) *fixtureReadSink {
	t.Helper()
	return &fixtureReadSink{t: t, events: make(chan fixtureSinkEvent, 32)}
}

// record проверяет token и записывает event или сообщает, что адаптер выдал
// больше событий, чем предусмотрено ограниченным сценарием.
func (s *fixtureReadSink) record(token core.ChatWorkerToken, event fixtureSinkEvent) {
	s.t.Helper()
	if token != fixtureWorkerToken() {
		s.t.Error("Обратный вызов получил другой токен попытки")
	}
	select {
	case s.events <- event:
	default:
		s.t.Error("Адаптер отправил слишком много событий")
	}
}

// Message записывает message для последующей проверки и при необходимости
// отклоняет один известный ID, сохраняя возможность принимать другие сообщения.
func (s *fixtureReadSink) Message(_ context.Context, token core.ChatWorkerToken, message core.ChatIncomingMessage) error {
	s.record(token, fixtureSinkEvent{message: &message})
	if s.rejectMessageID != "" && message.ProviderMessageID == s.rejectMessageID {
		return core.ErrInvalid
	}
	return nil
}

// Status записывает состояние с причиной и при необходимости удерживает
// Connected, пока адаптер не отменит переданный контекст ctx.
func (s *fixtureReadSink) Status(ctx context.Context, token core.ChatWorkerToken, state core.ChatSourceState, reason core.ChatReason) error {
	s.record(token, fixtureSinkEvent{state: state, reason: reason})
	if state != core.ChatConnected {
		return nil
	}
	if s.connectedStarted != nil {
		close(s.connectedStarted)
		defer close(s.connectedExited)
		<-ctx.Done()
		if s.connectedRelease != nil {
			<-s.connectedRelease
		}
		return ctx.Err()
	}
	return s.connectedFailure
}

// Gap записывает безопасную причину разрыва и проверяет, завершился ли ранее
// удерживаемый Connected к моменту сообщения о пробеле.
func (s *fixtureReadSink) Gap(_ context.Context, token core.ChatWorkerToken, reason core.ChatReason) error {
	if s.connectedExited != nil {
		select {
		case <-s.connectedExited:
		default:
			s.t.Error("Пробел опубликован до выхода отменённого Connected")
		}
	}
	s.record(token, fixtureSinkEvent{gap: true, reason: reason})
	return nil
}

// readFixtureEvents извлекает оставшиеся события после завершившегося Read.
func readFixtureEvents(sink *fixtureReadSink) []fixtureSinkEvent {
	// events собирает последовательность вызовов с их безопасными полями.
	var events []fixtureSinkEvent
	for {
		select {
		// event содержит следующий зафиксированный вызов приёмника.
		case event := <-sink.events:
			events = append(events, event)
		default:
			return events
		}
	}
}

// fixtureWSCommand содержит только идентификатор и ожидаемые параметры команды.
// Фикстура возвращает захваченный идентификатор вместо требования к его выбору.
type fixtureWSCommand struct {
	// ID соответствует полю id команды Centrifugo v4.
	ID int64 `json:"id"`
	// Connect содержит параметры подтверждаемого подключения.
	Connect *struct {
		// Token содержит отдельный короткоживущий токен WebSocket.
		Token string `json:"token"`
	} `json:"connect"`
	// Subscribe содержит параметры подтверждаемой подписки.
	Subscribe *struct {
		// Channel содержит точное имя канала платформы.
		Channel string `json:"channel"`
		// Token остаётся пустым для публичной подписки VK.
		Token string `json:"token"`
	} `json:"subscribe"`
}

// readFixtureCommand читает из conn одну клиентскую команду. При сетевом или
// структурном отказе сообщает об ошибке t и возвращает false.
func readFixtureCommand(t *testing.T, ctx context.Context, conn *websocket.Conn) (fixtureWSCommand, bool) {
	t.Helper()
	// kind указывает вид сообщения, body содержит команду, err сообщает об ошибке чтения.
	kind, body, err := conn.Read(ctx)
	// command хранит декодированные параметры команды.
	var command fixtureWSCommand
	if err != nil || kind != websocket.MessageText || json.Unmarshal(body, &command) != nil || command.ID <= 0 {
		t.Errorf("Ожидалась текстовая команда Centrifugo с положительным ID, ошибка чтения %v", err)
		return fixtureWSCommand{}, false
	}
	return command, true
}

// writeFixtureText отправляет body одним текстовым сообщением. При ошибке
// сообщает её в t, чтобы сценарий мог завершить обработчик без ожидания ACK.
func writeFixtureText(t *testing.T, ctx context.Context, conn *websocket.Conn, body string) bool {
	t.Helper()
	// err содержит возможную ошибку локальной отправки.
	if err := conn.Write(ctx, websocket.MessageText, []byte(body)); err != nil {
		t.Errorf("Фикстура не смогла отправить текст WebSocket: %v", err)
		return false
	}
	return true
}

// fixtureSocketReply проверяет путь, версию, Origin и отсутствие Bearer/Basic
// при Upgrade запроса r, затем передаёт conn локальному сценарию play.
func fixtureSocketReply(t *testing.T, w http.ResponseWriter, r *http.Request, play func(context.Context, *websocket.Conn)) {
	t.Helper()
	if r.URL.Path != "/connection/websocket" || r.URL.Query().Encode() != "cf_protocol_version=v2" ||
		r.Header.Get("Origin") != "https://vkvideo.ru" || r.Header.Get("Authorization") != "" {
		t.Error("Upgrade должен запросить Centrifugo v2 с Origin VK без HTTP-полномочий")
		http.Error(w, "fixture upgrade contract", http.StatusBadRequest)
		return
	}
	// conn принадлежит обработчику, err означает несостоявшийся Upgrade.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"vkvideo.ru"}})
	if err != nil {
		t.Errorf("Локальный Upgrade завершился ошибкой: %v", err)
		return
	}
	// По завершении сценария обработчик закрывает собственный сокет при любом результате.
	defer func() { _ = conn.CloseNow() }()
	// ctx ограничивает обмен фикстуры, cancel освобождает таймер после закрытия.
	ctx, cancel := context.WithTimeout(t.Context(), fixtureTimeout)
	defer cancel()
	play(ctx, conn)
}

// newSocketProtocolFixture направляет выдачу токена в tokenReply с переданными
// limits, а подтверждённый Upgrade отдаёт сценарию play.
func newSocketProtocolFixture(t *testing.T, limits core.ChatLimits, tokenReply http.HandlerFunc, play func(context.Context, *websocket.Conn)) *Adapter {
	t.Helper()
	// active ждёт выхода обработчиков Upgrade, которые httptest не закрывает после hijack.
	var active sync.WaitGroup
	t.Cleanup(active.Wait)
	// Обработчик различает выдачу токена и начальный HTTP-запрос WebSocket.
	return newProtocolFixture(t, limits, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/websocket/token" {
			tokenReply(w, r)
			return
		}
		active.Add(1)
		defer active.Done()
		fixtureSocketReply(t, w, r, play)
	}))
}

// fixtureDefaultSocketToken выдаёт локальный WS-токен только после проверки
// Bearer в запросе r.
func fixtureDefaultSocketToken(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	assertFixtureBearer(t, r, fixtureAppToken)
	fixtureHTTPReply(w, http.StatusOK, `{"data":{"token":"`+fixtureSocketToken+`"}}`)
}

// fixtureConnectReply строит ответ connect с захваченным id и включённым
// прикладным ping/pong, который поддерживает Centrifugo v4.
func fixtureConnectReply(id int64) string {
	return fmt.Sprintf(`{"id":%d,"connect":{"client":"fixture-client","version":"4","ping":5,"pong":true}}`, id)
}

// fixtureSubscribeReply строит пустой успешный ответ на подписку с id команды.
func fixtureSubscribeReply(id int64) string {
	return fmt.Sprintf(`{"id":%d,"subscribe":{}}`, id)
}

// fixtureAcceptConnect подтверждает connect через reply. Возвращает
// захваченную subscribe, если клиент запросил точный публичный канал.
func fixtureAcceptConnect(t *testing.T, ctx context.Context, conn *websocket.Conn, reply func(int64) bool) (fixtureWSCommand, bool) {
	t.Helper()
	// connect содержит первую команду клиента, ok отмечает успешное чтение.
	connect, ok := readFixtureCommand(t, ctx, conn)
	if !ok {
		return fixtureWSCommand{}, false
	}
	if connect.Connect == nil || connect.Connect.Token != fixtureSocketToken || connect.Subscribe != nil {
		t.Error("Первой должна идти команда connect с новым WebSocket-токеном")
		return fixtureWSCommand{}, false
	}
	if !reply(connect.ID) {
		return fixtureWSCommand{}, false
	}
	// subscribe содержит команду подписки после подтверждения предыдущего этапа.
	subscribe, ok := readFixtureCommand(t, ctx, conn)
	if !ok {
		return fixtureWSCommand{}, false
	}
	if subscribe.Subscribe == nil || subscribe.ID == connect.ID || subscribe.Connect != nil ||
		subscribe.Subscribe.Channel != fixtureChannel || subscribe.Subscribe.Token != "" {
		t.Error("Подписка должна иметь отдельный ID и адресовать точный публичный канал")
		return fixtureWSCommand{}, false
	}
	return subscribe, true
}

// fixtureMessagePush строит публикацию наблюдаемого сообщения с указанными
// channel, kind и флагами. Совпадение полей проверяется после нормализации.
func fixtureMessagePush(channel, kind string, id int64, private, deleted bool) string {
	return fmt.Sprintf(`{"push":{"channel":%q,"pub":{"data":{"type":%q,"data":{
		"id":%d,"isPrivate":%t,"isDeleted":%t,
		"author":{"id":9007199254740997,"displayName":"Автор","nick":"fixture-nick","avatarUrl":"http://example.test/avatar","badges":[{"id":"founder","name":"Организатор","smallUrl":"https://example.test/badge"}]},
		"data":[{"type":"text","content":"Привет "},{"type":"smile","id":"smile-id","name":":wave:","smallUrl":"http://example.test/emote","mediumUrl":"https://example.test/emote"},{"type":"link","content":" ссылка","url":"https://example.test/link"},{"type":"link","content":" без ссылки","url":"javascript:void(0)"}]
	}}}}}`, channel, kind, id, private, deleted)
}

// flattenFixturePush убирает переносы и отступы из публикации, чтобы она
// занимала одну строку в сообщении Centrifugo с несколькими командами.
func flattenFixturePush(body string) string {
	// compact собирает JSON в одну строку, не меняя строковые значения.
	compact := strings.ReplaceAll(strings.ReplaceAll(body, "\n", ""), "\t", "")
	return compact
}

// TestReadProcessesAcknowledgedBatchWhileConnectedIsBlocked проверяет
// изоляцию фаз, фильтры событий, точный перенос данных и pong независимо
// от удерживаемого Connected. Фикстура имитирует отклонение одного сообщения.
func TestReadProcessesAcknowledgedBatchWhileConnectedIsBlocked(t *testing.T) {
	// sink блокирует статус и сообщает точное время входа и выхода.
	sink := newFixtureReadSink(t)
	sink.connectedStarted, sink.connectedExited = make(chan struct{}), make(chan struct{})
	sink.rejectMessageID = "9501"
	// adapter подключается к фикстуре, где правильные события идут в одном сообщении с ACK.
	adapter := newSocketProtocolFixture(t, core.DefaultChatLimits(),
		// Обработчик проверяет назначение Bearer перед выдачей короткого токена.
		func(w http.ResponseWriter, r *http.Request) { fixtureDefaultSocketToken(t, w, r) },
		// Сценарий отправляет публикации до и после ACK, затем ждёт pong и разрывает подписку.
		func(ctx context.Context, conn *websocket.Conn) {
			// subscribe содержит точный ID команды, ok означает, что connect принят и подписка отправлена.
			subscribe, ok := fixtureAcceptConnect(t, ctx, conn, func(id int64) bool {
				// Первую публикацию сервер отправляет до подтверждения connect.
				return writeFixtureText(t, ctx, conn, flattenFixturePush(fixtureMessagePush(fixtureChannel, "message", 9001, false, false))+"\n"+fixtureConnectReply(id))
			})
			if !ok {
				return
			}
			// withoutID представляет штатное сообщение, у которого площадка опустила необязательный ID.
			withoutID := strings.Replace(fixtureMessagePush(fixtureChannel, "message", 9502, false, false), `"id":9502,`, "", 1)
			// batch смешивает публикации разных фаз, каналов и видимости в одном сообщении WebSocket.
			batch := strings.Join([]string{
				flattenFixturePush(fixtureMessagePush(fixtureChannel, "message", 9002, false, false)),
				fixtureSubscribeReply(subscribe.ID),
				flattenFixturePush(fixtureMessagePush("channel-chat:17", "message", 9003, false, false)),
				flattenFixturePush(fixtureMessagePush(fixtureChannel, "message", 9004, true, false)),
				flattenFixturePush(fixtureMessagePush(fixtureChannel, "message", 9005, false, true)),
				flattenFixturePush(fixtureMessagePush(fixtureChannel, "message_v8", 9006, false, false)),
				flattenFixturePush(fixtureMessagePush(fixtureChannel, "unknown_event", 9007, false, false)),
				flattenFixturePush(fixtureMessagePush(fixtureChannel, "message", 9501, false, false)),
				flattenFixturePush(withoutID),
				flattenFixturePush(fixtureMessagePush(fixtureChannel, "message", 9007199254740995, false, false)),
			}, "\n")
			if !writeFixtureText(t, ctx, conn, batch) {
				return
			}
			select {
			case <-sink.connectedStarted:
			case <-ctx.Done():
				t.Error("После совпадающего subscribe ACK адаптер не сообщил Connected")
				return
			}
			if !writeFixtureText(t, ctx, conn, "{}") {
				return
			}
			// kind, body и err позволяют отличить ровно один прикладной pong от других команд.
			kind, body, err := conn.Read(ctx)
			if err != nil || kind != websocket.MessageText || string(body) != "{}" {
				t.Errorf("Во время удерживаемого Connected ожидался прикладной pong, ошибка %v", err)
				return
			}
			writeFixtureText(t, ctx, conn, `{"push":{"disconnect":{"code":3005,"reason":"`+fixtureSocketToken+`"}}}`)
		})
	// ctx ограничивает чтение, cancel завершает попытку при ошибке теста.
	ctx, cancel := context.WithTimeout(t.Context(), fixtureTimeout+time.Second)
	defer cancel()
	// err должен сообщить временную потерю подписки после disconnect.
	err := adapter.Read(ctx, fixtureReadRequest(), sink)
	assertNoFixtureSecrets(t, err)
	// retry подтверждает, что истечение токена требует новой физической попытки.
	var retry *core.ChatReadRetry
	if !errors.As(err, &retry) || retry.Reason != core.ChatReasonNetwork {
		t.Fatalf("Disconnect 3005 должен дать сетевой повтор, получено %v", err)
	}
	waitFixtureSignal(t, sink.connectedExited)
	// events сохраняет полный порядок наблюдаемых событий после завершения Read.
	events := readFixtureEvents(sink)
	// messageIDs перечисляет все публикации, которые получили право дойти до Manager.
	var messageIDs []string
	// connected и gaps считают подтверждение подписки и пробелы отдельно от сообщений.
	var connected, gaps int
	// lastGap показывает позицию пробела, чтобы сравнить её с последующим статусом.
	lastGap := -1
	// index указывает позицию, event содержит очередной вызов приёмника.
	for index, event := range events {
		assertNoFixtureSecrets(t, event.reason)
		switch {
		case event.state == core.ChatConnected:
			connected++
		case event.gap:
			gaps++
			lastGap = index
			if event.reason != core.ChatReasonUpstreamGap {
				t.Errorf("Пробел должен иметь причину %q", core.ChatReasonUpstreamGap)
			}
		case event.state == core.ChatRetrying:
			if lastGap < 0 || lastGap >= index {
				t.Error("Статус повтора появился до сообщения о пробеле")
			}
		case event.message != nil:
			messageIDs = append(messageIDs, event.message.ProviderMessageID)
			assertFixtureNormalized(t, *event.message)
		}
	}
	if connected != 1 || gaps != 1 || !slices.Equal(messageIDs, []string{"9501", "", "9007199254740995"}) {
		t.Fatalf("Ожидались одно подтверждение, один пробел и три публичные публикации после ACK, получено %d, %d и %v", connected, gaps, messageIDs)
	}
}

// assertFixtureNormalized сверяет incoming с исходным публичным сообщением
// и проверяет удаление небезопасного URL аватара.
func assertFixtureNormalized(t *testing.T, incoming core.ChatIncomingMessage) {
	t.Helper()
	assertNoFixtureSecrets(t, incoming)
	if incoming.Provider != core.ChatVKVideoLive || incoming.ChannelID != fixtureChannelID ||
		incoming.Kind != core.ChatMessageText || incoming.Author.ID != "9007199254740997" ||
		incoming.Author.DisplayName != "Автор" || incoming.Author.AvatarURL != "" ||
		incoming.ProviderTimestamp != nil || incoming.Author.Color != "" || incoming.Validate() != nil {
		t.Error("Адаптер исказил идентичность сообщения, данные автора или безопасные поля")
	}
	if !slices.Equal(incoming.Parts, []core.ChatPart{
		{Kind: core.ChatPartText, Text: "Привет "},
		{Kind: core.ChatPartEmote, Text: ":wave:", EmoteID: "smile-id", ImageURL: "https://example.test/emote"},
		{Kind: core.ChatPartText, Text: " ссылка", URL: "https://example.test/link"},
		{Kind: core.ChatPartText, Text: " без ссылки"},
	}) || !slices.Equal(incoming.Badges, []core.ChatBadge{
		{ID: "founder", Name: "Организатор", ImageURL: "https://example.test/badge"},
	}) || incoming.Text != "Привет :wave: ссылка без ссылки" {
		t.Error("Адаптер исказил исходный текст, эмодзи, метку автора или правила обработки ссылок")
	}
}

// TestReadRecoversRejectedAppTokenOutsideHTTPSlot проверяет порядок повторной
// выдачи после 401 с открытым телом. Восстановление само берёт единственный
// HTTP-слот, а второй 401 сохраняет Retry-After и завершает физическую попытку.
func TestReadRecoversRejectedAppTokenOutsideHTTPSlot(t *testing.T) {
	// rejectAgain выбирает ответ на запрос с новым подтверждённым токеном.
	for _, rejectAgain := range []bool{false, true} {
		// Подтест проверяет число выдач и Upgrade для одного сценария восстановления.
		t.Run(fmt.Sprintf("reject_again_%t", rejectAgain), func(t *testing.T) {
			// slot моделирует единственное доступное место для конечных HTTP-запросов.
			slot := make(chan struct{}, 1)
			// closed сообщает, что запрос с отвергнутым Bearer освободил локальное HTTP-соединение.
			closed := make(chan struct{})
			// tokenRequests и sockets считают серверные запросы, renewals считает обращения к владельцу.
			var tokenRequests, sockets, renewals atomic.Int32
			// adapter выполняет выдачу WS-токена в реальном TLS и открывает не более одного сокета.
			adapter := newSocketProtocolFixture(t, core.DefaultChatLimits(),
				// Обработчик требует исходный Bearer перед 401 и только новый после обновления.
				func(w http.ResponseWriter, r *http.Request) {
					switch tokenRequests.Add(1) {
					case 1:
						assertFixtureBearer(t, r, fixtureAppToken)
						defer close(closed)
						fixtureHoldHTTPBody(w, r, http.StatusUnauthorized, "13")
						return
					case 2:
						assertFixtureBearer(t, r, fixtureRenewedToken)
						if renewals.Load() != 1 {
							t.Error("Повторный запрос отправлен до обновления набора")
						}
						if !rejectAgain {
							fixtureHTTPReply(w, http.StatusOK, `{"data":{"token":"`+fixtureSocketToken+`"}}`)
							return
						}
					default:
						t.Error("Одна физическая попытка выполнила больше двух запросов WS-токена")
					}
					w.Header().Set("Retry-After", "31")
					fixtureHTTPReply(w, http.StatusUnauthorized, `{"error":"unauthorized","error_description":"`+fixtureAppToken+`"}`)
				},
				// Сценарий завершает чтение восстанавливаемой ошибкой connect после проверки его токена.
				func(ctx context.Context, conn *websocket.Conn) {
					sockets.Add(1)
					// command содержит попытку connect, ok отмечает корректную JSON-команду.
					command, ok := readFixtureCommand(t, ctx, conn)
					if ok && command.Connect != nil && command.Connect.Token == fixtureSocketToken {
						writeFixtureText(t, ctx, conn, fmt.Sprintf(`{"id":%d,"error":{"code":109,"message":"`+fixtureSocketToken+`"}}`, command.ID))
						return
					}
					t.Error("Сокет должен использовать короткоживущий токен, выданный с новым Bearer")
				})
			// ctx ограничивает токены и Upgrade, cancel освобождает срок фикстуры.
			ctx, cancel := context.WithTimeout(t.Context(), fixtureTimeout)
			defer cancel()
			// request несёт исходные реквизиты и функции допуска текущей попытки.
			request := fixtureReadRequest()
			// Функция приобретает единственный слот или выходит после отмены запроса.
			request.AcquireHTTP = func(ctx context.Context) (func(), error) {
				select {
				case slot <- struct{}{}:
					// Функция освобождает ровно один занятый слот.
					return func() { <-slot }, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			// Восстановление получает тот же слот до выдачи новых реквизитов.
			request.RefreshRejectedCredentials = func(ctx context.Context) (core.ChatCredentials, error) {
				renewals.Add(1)
				// release освобождает допуск обновления, err показывает удержание слота исходным запросом.
				release, err := request.AcquireHTTP(ctx)
				if err != nil {
					t.Error("Восстановление не получило единственный HTTP-слот")
					return core.ChatCredentials{}, err
				}
				defer release()
				// refreshed содержит новый Bearer и изменённую идентичность сохранённого набора.
				refreshed := fixtureCredentials(fixtureRenewedToken)
				refreshed.CredentialRevision = 2
				refreshed.CredentialWriteID = "vk-renewed-write"
				return refreshed, nil
			}
			// sink принимает только события текущей попытки.
			sink := newFixtureReadSink(t)
			// err содержит сетевой повтор после второго 401 или ошибки Centrifugo 109.
			err := adapter.Read(ctx, request, sink)
			assertNoFixtureSecrets(t, err)
			// retry сохраняет типизированный временный отказ без причины credentials_required.
			var retry *core.ChatReadRetry
			if !errors.As(err, &retry) || retry.Reason != core.ChatReasonNetwork {
				t.Fatalf("Ожидался временный отказ физической попытки, получено %v", err)
			}
			// wantSockets равен нулю, если новый app-токен также отвергнут.
			wantSockets := int32(1)
			// wantDelay отражает задержку второго 401. При успешной выдаче её заменяет исход connect.
			var wantDelay time.Duration
			if rejectAgain {
				wantSockets = 0
				wantDelay = 31 * time.Second
			}
			if retry.RetryAfter != wantDelay {
				t.Fatalf("Задержка следующей попытки должна составлять %s, получено %s", wantDelay, retry.RetryAfter)
			}
			if renewals.Load() != 1 || tokenRequests.Load() != 2 || sockets.Load() != wantSockets || len(slot) != 0 {
				t.Fatalf("Ожидались одна выдача, два запроса токена, %d Upgrade и свободный слот, получено %d, %d, %d и %d занятых слотов", wantSockets, renewals.Load(), tokenRequests.Load(), sockets.Load(), len(slot))
			}
			waitFixtureSignal(t, closed)
			assertFixturePreSubscriptionFailure(t, sink)
		})
	}
}

// TestReadHandlesCredentialRecoveryFailure проверяет реакцию на отказ владельца
// после HTTP 401. При временном отказе повтор сохраняет причину и больший срок,
// подтверждённый отказ пары требует авторизации, а отзыв попытки прекращает её.
func TestReadHandlesCredentialRecoveryFailure(t *testing.T) {
	// testCase связывает отказ владельца с ожидаемым статусом и исходом Read.
	for _, testCase := range []struct {
		name       string               // name различает причины отказа восстановления.
		refreshErr error                // refreshErr содержит результат владельца полномочий.
		state      core.ChatSourceState // state задаёт статус до подписки или пустое значение при отзыве.
		reason     core.ChatReason      // reason объясняет опубликованный статус.
		delay      time.Duration        // delay задаёт минимальную задержку для временного отказа.
	}{
		{"quota", &core.ChatReadRetry{Reason: core.ChatReasonQuota, RetryAfter: 47 * time.Second}, core.ChatRetrying, core.ChatReasonQuota, 47 * time.Second},
		{"legacy_retry", &core.ChatReadRetry{RetryAfter: 29 * time.Second}, core.ChatRetrying, core.ChatReasonNetwork, 41 * time.Second},
		{"temporary", core.ErrNotReady, core.ChatRetrying, core.ChatReasonNetwork, 41 * time.Second},
		{"invalid_pair", core.ErrInvalid, core.ChatAuthRequired, core.ChatReasonCredentials, 0},
		{"revoked_attempt", core.ErrStale, "", core.ChatReasonNone, 0},
	} {
		// Подтест завершает одну физическую попытку после отказа обновления.
		t.Run(testCase.name, func(t *testing.T) {
			// requests считает запросы исходного и возможного повторного WS-токена.
			var requests atomic.Int32
			// adapter получает отказ Bearer по локальному TLS. Запрос Upgrade означает ошибку сценария.
			adapter := newProtocolFixture(t, core.DefaultChatLimits(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/websocket/token" {
					t.Error("После отказа восстановления не должно быть Upgrade")
				}
				assertFixtureBearer(t, r, fixtureAppToken)
				requests.Add(1)
				w.Header().Set("Retry-After", "41")
				w.WriteHeader(http.StatusUnauthorized)
			}))
			// request привязывает результат владельца к исходному отвергнутому токену.
			request := fixtureReadRequest()
			// refreshCalls считает обращения к владельцу текущего набора.
			var refreshCalls atomic.Int32
			// Владелец возвращает выбранный отказ без нового Bearer.
			request.RefreshRejectedCredentials = func(context.Context) (core.ChatCredentials, error) {
				refreshCalls.Add(1)
				return core.ChatCredentials{}, testCase.refreshErr
			}
			// sink записывает результат попытки до протокольной подписки.
			sink := newFixtureReadSink(t)
			// ctx ограничивает весь обмен, cancel освобождает таймер подтеста.
			ctx, cancel := context.WithTimeout(t.Context(), fixtureTimeout)
			defer cancel()
			// err содержит итог попытки после вызова владельца.
			err := adapter.Read(ctx, request, sink)
			// events содержит все вызовы приёмника по текущей попытке.
			events := readFixtureEvents(sink)
			if requests.Load() != 1 || refreshCalls.Load() != 1 {
				t.Fatalf("Ожидались один запрос токена и одно восстановление, получено %d и %d", requests.Load(), refreshCalls.Load())
			}
			if testCase.state == "" {
				if !errors.Is(err, core.ErrStale) || len(events) != 0 {
					t.Fatalf("Отозванная попытка должна завершиться без событий с ErrStale, получено %d событий и %v", len(events), err)
				}
				return
			}
			if !slices.Equal(events, []fixtureSinkEvent{{state: testCase.state, reason: testCase.reason}}) {
				t.Fatalf("Ожидался единственный статус %q с причиной %q, получено %+v", testCase.state, testCase.reason, events)
			}
			if testCase.state == core.ChatAuthRequired {
				if err != nil {
					t.Fatalf("Подтверждённый отказ пары должен завершить чтение после статуса AUTH_REQUIRED, получено %v", err)
				}
				return
			}
			// retry переносит причину и задержку временного отказа до Manager.
			var retry *core.ChatReadRetry
			if !errors.As(err, &retry) || retry.Reason != testCase.reason || retry.RetryAfter != testCase.delay {
				t.Fatalf("Временный отказ должен передать повтор с причиной %q и задержкой %s, получено %v", testCase.reason, testCase.delay, err)
			}
		})
	}
}

// assertFixturePreSubscriptionFailure проверяет отсутствие подписки, данных
// и пробела, пока площадка не подтвердила subscribe через ACK.
func assertFixturePreSubscriptionFailure(t *testing.T, sink *fixtureReadSink) {
	t.Helper()
	// event содержит очередное событие чтения до успешной подписки.
	for _, event := range readFixtureEvents(sink) {
		if event.state == core.ChatConnected || event.state == core.ChatAuthRequired || event.message != nil || event.gap {
			t.Fatal("До ACK подписки неожиданно появился статус Connected/AuthRequired, сообщение или пробел")
		}
	}
}

// TestReadRequiresMatchingAcknowledgements проверяет, что неверный ID
// подтверждения connect или subscribe не разрешает подписку и приём сообщений.
func TestReadRequiresMatchingAcknowledgements(t *testing.T) {
	// wrongConnect позволяет по отдельности нарушить соответствие ID каждой команды.
	for _, wrongConnect := range []bool{true, false} {
		// Подтест имитирует успешный по форме ACK, относящийся к другой команде.
		t.Run(fmt.Sprintf("wrong_connect_%t", wrongConnect), func(t *testing.T) {
			// adapter получает неверный ID непосредственно на соответствующей фазе рукопожатия.
			adapter := newSocketProtocolFixture(t, core.DefaultChatLimits(),
				// Обработчик выдаёт действительный WS-токен для проверки ACK.
				func(w http.ResponseWriter, r *http.Request) { fixtureDefaultSocketToken(t, w, r) },
				// Сценарий отправляет ACK с чужим ID и завершается после закрытия клиента.
				func(ctx context.Context, conn *websocket.Conn) {
					if wrongConnect {
						// connect содержит команду, ok подтверждает её получение до отправки чужого ACK.
						connect, ok := readFixtureCommand(t, ctx, conn)
						if !ok || !writeFixtureText(t, ctx, conn, fixtureConnectReply(connect.ID+20)) {
							return
						}
						// err должен сообщить о закрытии без следующей команды subscribe.
						if _, _, err := conn.Read(ctx); err == nil {
							t.Error("После чужого ACK connect клиент перешёл к следующей команде")
						}
						return
					}
					// subscribe указывает настоящий ID, ok подтверждает чтение команды до отправки чужого ACK.
					subscribe, ok := fixtureAcceptConnect(t, ctx, conn, func(id int64) bool {
						return writeFixtureText(t, ctx, conn, fixtureConnectReply(id))
					})
					if ok && writeFixtureText(t, ctx, conn, fixtureSubscribeReply(subscribe.ID+20)+"\n"+
						flattenFixturePush(fixtureMessagePush(fixtureChannel, "message", 62, false, false))) {
						_, _, _ = conn.Read(ctx)
					}
				})
			// sink позволяет заметить преждевременную подписку в любой из фаз.
			sink := newFixtureReadSink(t)
			// ctx ограничивает проверку ответа, cancel завершает её при выходе.
			ctx, cancel := context.WithTimeout(t.Context(), fixtureTimeout)
			defer cancel()
			// err сообщает отказ неподтверждённого рукопожатия.
			err := adapter.Read(ctx, fixtureReadRequest(), sink)
			// retry должен сохранять сетевую причину неподтверждённого подключения.
			var retry *core.ChatReadRetry
			if !errors.As(err, &retry) || retry.Reason != core.ChatReasonNetwork {
				t.Fatalf("Чужой ACK должен завершить попытку сетевым повтором, получено %v", err)
			}
			assertFixturePreSubscriptionFailure(t, sink)
		})
	}
}

// TestReadClassifiesCentrifugoDisconnectsAndUnsubscribes проверяет границы
// повторяемых и конечных кодов v4 для закрытия сокета и потери подписки.
func TestReadClassifiesCentrifugoDisconnectsAndUnsubscribes(t *testing.T) {
	// testCase задаёт вид сигнала, код Centrifugo и требуемую возможность повтора.
	for _, testCase := range []struct {
		kind  string // kind выбирает WebSocket close либо push unsubscribe.
		code  int    // code содержит точное граничное значение кодов Centrifugo v4.
		retry bool   // retry показывает, должен ли Manager получить сетевой повтор.
	}{
		{"close", 3499, true},
		{"close", 3500, false},
		{"close", 3999, false},
		{"close", 4000, true},
		{"close", 4499, true},
		{"close", 4500, false},
		{"unsubscribe", 2499, false},
		{"unsubscribe", 2500, true},
	} {
		// Подтест теряет подтверждённую подписку и проверяет статус и единственный Gap.
		t.Run(fmt.Sprintf("%s_%d", testCase.kind, testCase.code), func(t *testing.T) {
			// sink сохраняет события и позволяет синхронизировать потерю с подтверждением.
			sink := newFixtureReadSink(t)
			sink.connectedStarted, sink.connectedExited = make(chan struct{}), make(chan struct{})
			// adapter ведёт одну физическую попытку до команды площадки или её WebSocket close.
			adapter := newSocketProtocolFixture(t, core.DefaultChatLimits(),
				// Обработчик токена запускает обычный обмен перед обновлением статуса.
				func(w http.ResponseWriter, r *http.Request) { fixtureDefaultSocketToken(t, w, r) },
				// Сценарий отправляет постороннюю отписку перед кодом ожидаемого канала.
				func(ctx context.Context, conn *websocket.Conn) {
					// subscribe содержит подтверждаемый ID, ok отмечает успешный запрос подписки.
					subscribe, ok := fixtureAcceptConnect(t, ctx, conn, func(id int64) bool {
						return writeFixtureText(t, ctx, conn, fixtureConnectReply(id))
					})
					if !ok || !writeFixtureText(t, ctx, conn, fixtureSubscribeReply(subscribe.ID)) {
						return
					}
					select {
					case <-sink.connectedStarted:
					case <-ctx.Done():
						t.Error("Подтверждённая подписка не дала Connected")
						return
					}
					if testCase.kind == "close" {
						_ = conn.Close(websocket.StatusCode(testCase.code), fixtureSocketToken)
						return
					}
					if writeFixtureText(t, ctx, conn, `{"push":{"channel":"channel-chat:other","unsubscribe":{"code":2000}}}`+"\n{}") {
						// kind, body и err проверяют pong после чужого unsubscribe.
						kind, body, err := conn.Read(ctx)
						if err != nil || kind != websocket.MessageText || string(body) != "{}" {
							t.Error("Отписка другого канала не должна прекращать чтение")
							return
						}
						writeFixtureText(t, ctx, conn, fmt.Sprintf(`{"push":{"channel":%q,"unsubscribe":{"code":%d,"reason":%q}}}`, fixtureChannel, testCase.code, fixtureSocketToken))
					}
				})
			// ctx ограничивает одну попытку, cancel завершает её при выходе из теста.
			ctx, cancel := context.WithTimeout(t.Context(), fixtureTimeout+time.Second)
			defer cancel()
			// err передаёт поведение Manager после потери подтверждённой подписки.
			err := adapter.Read(ctx, fixtureReadRequest(), sink)
			assertNoFixtureSecrets(t, err)
			// retry должен присутствовать только для восстанавливаемого кода.
			var retry *core.ChatReadRetry
			if testCase.retry {
				if !errors.As(err, &retry) || retry.Reason != core.ChatReasonNetwork {
					t.Fatalf("Код %d должен дать повтор по причине сети, получено %v", testCase.code, err)
				}
			} else if err != nil {
				t.Fatalf("Конечный код %d должен завершить чтение после статуса, получено %v", testCase.code, err)
			}
			// gaps считает пробелы, failed отмечает конечный статус после пробела.
			gaps, failed := 0, false
			// event содержит очередной вызов с безопасным объяснением.
			for _, event := range readFixtureEvents(sink) {
				if event.gap && event.reason == core.ChatReasonUpstreamGap {
					gaps++
				}
				if event.state == core.ChatFailed && event.reason == core.ChatReasonUnavailable {
					failed = true
					if gaps != 1 {
						t.Error("Конечный статус получен до единственного пробела")
					}
				}
				if event.state == core.ChatAuthRequired {
					t.Error("Разрыв WebSocket не проверяет пару Client ID/Secret")
				}
			}
			if gaps != 1 || failed == testCase.retry {
				t.Fatalf("Неверный итог потери подписки: пробелов %d, конечный статус %t", gaps, failed)
			}
		})
	}
}

// TestReadBoundsCompleteFragmentedMessage отправляет корректное фрагментированное
// рукопожатие, а затем одно слишком большое сообщение из допустимых частей.
// После пробела приёмник получает причину ограничения размера, а повтор
// оставляет необязательную причину пустой для сохранения этого статуса.
func TestReadBoundsCompleteFragmentedMessage(t *testing.T) {
	// limits разрешает отдельные части контрольного сообщения, но ограничивает их сумму.
	limits := core.DefaultChatLimits()
	limits.UpstreamMessageBytes = 1024
	// sink фиксирует прекращение подтверждённой подписки без приёма данных из лишнего сообщения.
	sink := newFixtureReadSink(t)
	sink.connectedStarted, sink.connectedExited = make(chan struct{}), make(chan struct{})
	// adapter использует уменьшенный входной бюджет полного сообщения WebSocket.
	adapter := newSocketProtocolFixture(t, limits,
		// Обработчик присылает отдельный WS-токен для рукопожатия.
		func(w http.ResponseWriter, r *http.Request) { fixtureDefaultSocketToken(t, w, r) },
		// Сценарий пишет connect ACK частями, затем отправляет сообщение больше предела.
		func(ctx context.Context, conn *websocket.Conn) {
			// subscribe содержит команду подписки, ok подтверждает успешное чтение фрагментированного ACK клиентом.
			subscribe, ok := fixtureAcceptConnect(t, ctx, conn, func(id int64) bool {
				// ack содержит одну корректную JSON-команду, разрезанную внутри текста.
				ack := fixtureConnectReply(id)
				// err показывает, допустил ли локальный сокет запись фрагментов.
				if err := writeFixtureFragments(ctx, conn, ack[:10], ack[10:]); err != nil {
					t.Errorf("Не удалось отправить фрагментированный connect ACK: %v", err)
					return false
				}
				return true
			})
			if !ok || !writeFixtureText(t, ctx, conn, fixtureSubscribeReply(subscribe.ID)) {
				return
			}
			select {
			case <-sink.connectedStarted:
			case <-ctx.Done():
				t.Error("Адаптер не подтвердил подписку после фрагментированного connect ACK")
				return
			}
			// padding делает каждую строку допустимой отдельно, а их суммарный размер больше 1024 байт.
			padding := strings.Repeat(" ", 650)
			_ = writeFixtureFragments(ctx, conn, "{}"+padding+"\n", "{}"+padding)
			// kind, body и err различают закрытие и недопустимый pong на слишком большое сообщение.
			kind, body, err := conn.Read(ctx)
			if err == nil && (kind == websocket.MessageText || string(body) == "{}") {
				t.Error("Адаптер принял строки из сообщения, превысившего целый бюджет")
			}
		})
	// ctx ограничивает сценарий, cancel освобождает срок при ошибке.
	ctx, cancel := context.WithTimeout(t.Context(), fixtureTimeout+time.Second)
	defer cancel()
	// err запрашивает повтор после превышения бюджета, сохраняя причину уже выданного статуса.
	err := adapter.Read(ctx, fixtureReadRequest(), sink)
	// retry подтверждает, что Manager может повторить потерянную подписку.
	var retry *core.ChatReadRetry
	if !errors.As(err, &retry) || retry.Reason != core.ChatReasonNone || retry.RetryAfter != 0 {
		t.Fatalf("После превышения размера ожидался повтор без причины и задержки, получено %v", err)
	}
	// gaps содержит число пробелов после закрытия сокета.
	gaps := 0
	// retryStatuses считает статусы повтора, следующие за единственным пробелом.
	retryStatuses := 0
	// event содержит наблюдаемый результат одного обратного вызова.
	for _, event := range readFixtureEvents(sink) {
		if event.gap {
			gaps++
		}
		if event.state == core.ChatRetrying {
			retryStatuses++
			if event.reason != core.ChatReasonLimit || gaps != 1 {
				t.Errorf("После единственного пробела ожидался RETRYING с причиной %q, получено %q после %d пробелов", core.ChatReasonLimit, event.reason, gaps)
			}
		}
		if event.message != nil {
			t.Error("Сценарий ограничения размера не должен выдавать публикаций")
		}
	}
	if gaps != 1 || retryStatuses != 1 {
		t.Fatalf("После превышения размера ожидались один пробел и один статус повтора, получено %d и %d", gaps, retryStatuses)
	}
}

// writeFixtureFragments записывает parts как фрагменты одного текстового
// сообщения conn, чей последний кадр отправляется при закрытии writer.
func writeFixtureFragments(ctx context.Context, conn *websocket.Conn, parts ...string) error {
	// writer владеет одной записью WebSocket, err возвращает отказ её открытия.
	writer, err := conn.Writer(ctx, websocket.MessageText)
	if err != nil {
		return err
	}
	// part содержит очередной фрагмент полезной нагрузки.
	for _, part := range parts {
		// err возвращает отказ записи, например после закрытия слишком большого сообщения.
		if _, err := io.WriteString(writer, part); err != nil {
			_ = writer.Close()
			return err
		}
	}
	return writer.Close()
}

// TestReadCancellationClosesSocketAndJoinsStatus проверяет штатную отмену
// после подписки. Сокет должен закрыться, а Read должен дождаться выхода
// Connected без публикации пробела или статуса повтора.
func TestReadCancellationClosesSocketAndJoinsStatus(t *testing.T) {
	// sink блокирует Connected до отмены, затем до отдельного разрешения выйти.
	sink := newFixtureReadSink(t)
	sink.connectedStarted, sink.connectedExited = make(chan struct{}), make(chan struct{})
	// release позволяет тесту удержать завершение Connected до наблюдаемого закрытия сокета.
	release := make(chan struct{}, 1)
	sink.connectedRelease = release
	defer close(release)
	// socketClosed сообщает о выходе серверного чтения после клиентской отмены.
	socketClosed := make(chan error, 1)
	// adapter держит подтверждённый сокет до отмены контекста.
	adapter := newSocketProtocolFixture(t, core.DefaultChatLimits(),
		// Обработчик выдаёт токен для подключения, которое будет отменено.
		func(w http.ResponseWriter, r *http.Request) { fixtureDefaultSocketToken(t, w, r) },
		// Сценарий подтверждает подписку и наблюдает закрытие со стороны клиента.
		func(ctx context.Context, conn *websocket.Conn) {
			// subscribe содержит команду с ID, ok подтверждает запрос нужного канала.
			subscribe, ok := fixtureAcceptConnect(t, ctx, conn, func(id int64) bool {
				return writeFixtureText(t, ctx, conn, fixtureConnectReply(id))
			})
			if !ok || !writeFixtureText(t, ctx, conn, fixtureSubscribeReply(subscribe.ID)) {
				return
			}
			// err подтверждает закрытие сокета до истечения срока фикстуры.
			_, _, err := conn.Read(ctx)
			socketClosed <- err
		})
	// ctx ограничивает время чтения, cancel моделирует отзыв попытки со стороны Manager.
	ctx, cancel := context.WithTimeout(t.Context(), fixtureTimeout)
	defer cancel()
	// done получает итоговую ошибку Read после его очистки.
	done := make(chan error, 1)
	// Горутина читает обычным входом адаптера до сигнала отзыва.
	go func() { done <- adapter.Read(ctx, fixtureReadRequest(), sink) }()
	waitFixtureSignal(t, sink.connectedStarted)
	cancel()
	// err должен подтвердить фактический разрыв WebSocket до окончания ожидания сервера.
	if err := awaitFixture(t, socketClosed); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Клиент не закрыл сокет после отмены, результат чтения %v", err)
	}
	// err сообщает о преждевременном завершении физического чтения.
	select {
	case err := <-done:
		t.Fatalf("Read завершился до выхода удерживаемого Connected с результатом %v", err)
	default:
	}
	// Передача разблокирует удерживаемый статус. defer закрывает канал при досрочном выходе.
	release <- struct{}{}
	// err сообщает точную штатную отмену без преобразования в сетевой повтор.
	if err := awaitFixture(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Ожидалась ошибка отмены чтения, получено %v", err)
	}
	waitFixtureSignal(t, sink.connectedExited)
	// event содержит очередной вызов приёмника, который не должен объявлять потерю при отмене.
	for _, event := range readFixtureEvents(sink) {
		if event.gap || event.state == core.ChatRetrying {
			t.Error("Штатная отмена не должна объявлять пробел или сетевой повтор")
		}
	}
}

// TestReadRequestsFreshSocketTokenAfterExpiry проверяет новую выдачу после
// connect 110. На второй подписке отказ Connected прекращает чтение и закрывает
// сокет без объявления разрыва действующему читателю.
func TestReadRequestsFreshSocketTokenAfterExpiry(t *testing.T) {
	// grants и sockets независимо считают запросы DevAPI и физические соединения.
	var grants, sockets atomic.Int32
	// closed получает ошибку серверного чтения после отказа управляющего вызова.
	closed := make(chan error, 1)
	// adapter обслуживает две попытки и на каждой требует новый WS-токен.
	adapter := newSocketProtocolFixture(t, core.DefaultChatLimits(),
		// Обработчик возвращает токен, привязанный к номеру запроса выдачи.
		func(w http.ResponseWriter, r *http.Request) {
			assertFixtureBearer(t, r, fixtureAppToken)
			fixtureHTTPReply(w, http.StatusOK, fmt.Sprintf(`{"data":{"token":"%s-%d"}}`, fixtureSocketToken, grants.Add(1)))
		},
		// Первый сокет отклоняет connect как просроченный, второй подтверждает подписку.
		func(ctx context.Context, conn *websocket.Conn) {
			// attempt задаёт номер физического соединения и соответствующего токена.
			attempt := sockets.Add(1)
			// command и ok подтверждают, что клиент прислал ожидаемый токен в connect.
			command, ok := readFixtureCommand(t, ctx, conn)
			if !ok || command.Connect == nil || command.Connect.Token != fmt.Sprintf("%s-%d", fixtureSocketToken, attempt) {
				t.Error("Каждое физическое соединение должно начать connect с новым токеном")
				return
			}
			if attempt == 1 {
				writeFixtureText(t, ctx, conn, fmt.Sprintf(`{"id":%d,"error":{"code":110,"message":"`+fixtureSocketToken+`"}}`, command.ID))
				return
			}
			if !writeFixtureText(t, ctx, conn, fixtureConnectReply(command.ID)) {
				return
			}
			// subscribe и ok проверяют, что новый токен дошёл до подписки на ожидаемый канал.
			subscribe, ok := readFixtureCommand(t, ctx, conn)
			if !ok || subscribe.Subscribe == nil || subscribe.Subscribe.Channel != fixtureChannel ||
				!writeFixtureText(t, ctx, conn, fixtureSubscribeReply(subscribe.ID)) {
				t.Error("После выдачи нового WS-токена ожидалась подтверждённая подписка")
				return
			}
			// err сообщает серверу о закрытии сокета после отказа Connected.
			_, _, err := conn.Read(ctx)
			closed <- err
		})
	// ctx ограничивает обе физические попытки, cancel освобождает срок теста.
	ctx, cancel := context.WithTimeout(t.Context(), fixtureTimeout)
	defer cancel()
	// request повторно использует только app-токен для двух вызовов Read.
	request := fixtureReadRequest()
	// Восстановление app-реквизитов из-за ошибки Centrifugo было бы ошибочным.
	request.RefreshRejectedCredentials = func(context.Context) (core.ChatCredentials, error) {
		t.Error("Истечение WS-токена не должно обновлять app-реквизиты")
		return core.ChatCredentials{}, core.ErrInvalid
	}
	// firstSink проверяет, что connect 110 завершил попытку до подписки.
	firstSink := newFixtureReadSink(t)
	// err классифицирует истечение короткого токена безопасным сетевым повтором.
	err := adapter.Read(ctx, request, firstSink)
	assertNoFixtureSecrets(t, err)
	// retry даёт Manager возможность запросить новый токен при следующем вызове.
	var retry *core.ChatReadRetry
	if !errors.As(err, &retry) || retry.Reason != core.ChatReasonNetwork || grants.Load() != 1 || sockets.Load() != 1 {
		t.Fatalf("Connect 110 должен дать сетевой повтор после одного сокета, результат %v", err)
	}
	assertFixturePreSubscriptionFailure(t, firstSink)
	// secondSink отзывает допуск при попытке опубликовать Connected.
	secondSink := newFixtureReadSink(t)
	secondSink.connectedFailure = core.ErrStale
	// err должен сохранить отказ управляющего вызова после закрытия сокета.
	if err := adapter.Read(ctx, request, secondSink); !errors.Is(err, core.ErrStale) || grants.Load() != 2 || sockets.Load() != 2 {
		t.Fatalf("Следующая попытка должна открыть ровно один новый сокет и сохранить ErrStale, результат %v", err)
	}
	// err означает, что сервер наблюдал закрытие из-за отказа до своего таймаута.
	if err := awaitFixture(t, closed); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Отказ Connected не закрыл сокет, результат чтения %v", err)
	}
	// event содержит очередной вызов, который не должен сообщать итог после отказа Connected.
	for _, event := range readFixtureEvents(secondSink) {
		if event.gap || event.state == core.ChatRetrying || event.state == core.ChatAuthRequired {
			t.Error("Отказ Connected должен остановить дальнейшие управляющие вызовы")
		}
	}
}
