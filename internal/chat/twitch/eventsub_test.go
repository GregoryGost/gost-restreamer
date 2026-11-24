package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/coder/websocket"
)

// socketTestCall представляет запрос создания подписки, ответ на который задаёт тест.
type socketTestCall struct {
	// sessionID содержит адресат подписки, полученный в POST.
	sessionID string
	// replies передаёт единственный ответ ожидающему обработчику Helix.
	replies chan socketTestReply
}

// socketTestReply задаёт HTTP-статус и описание подписки, возвращаемые настоящему адаптеру.
type socketTestReply struct {
	// status содержит статус HTTP.
	status int
	// body содержит полное JSON-тело ответа.
	body string
}

// socketTestPeer представляет принятое тестовым Twitch физическое соединение.
type socketTestPeer struct {
	// conn отправляет серверные сообщения и управляющие кадры клиенту.
	conn *websocket.Conn
	// path содержит исходный путь с query, позволяя проверить URL управляемого перехода.
	path string
	// closed сигнализирует завершение чтения управляющих кадров.
	closed <-chan struct{}
}

// socketTestFixture связывает реальный адаптер с изолированными WebSocket и Helix.
// Тест управляет входящими кадрами и ответами HTTP через каналы.
type socketTestFixture struct {
	// adapter содержит экземпляр адаптера с локальными адресами.
	adapter *Adapter
	// peers передаёт каждое принятое физическое соединение.
	peers chan socketTestPeer
	// calls передаёт каждый запрос создания подписки, который ждёт ответа теста.
	calls chan socketTestCall
	// reconnectURL содержит выданный Twitch адрес для управляемого перехода.
	reconnectURL string
}

// newSocketTestFixture направляет запросы адаптера к серверу на отдельном порту TLS.
// Срок ctx ограничивает управление соединениями, а t закрывает сервер по окончании теста.
func newSocketTestFixture(t *testing.T, ctx context.Context, limits core.ChatLimits) *socketTestFixture {
	t.Helper()
	// fixture выделяет очереди для соединений и HTTP-запросов одного сценария.
	fixture := &socketTestFixture{peers: make(chan socketTestPeer, 4), calls: make(chan socketTestCall, 4)}
	// fixtureCtx завершает принятые сокеты и ожидающие ответы HTTP. cancel вызывается до закрытия сервера.
	fixtureCtx, cancel := context.WithCancel(ctx)
	// server принимает только тестовые пути и проверяет исходную авторизацию Helix.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ws", "/handover":
			fixture.accept(t, fixtureCtx, w, r)
		case "/helix/eventsub/subscriptions":
			fixture.subscribe(t, fixtureCtx, w, r)
		default:
			t.Errorf("Неожиданный путь %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(cancel)
	// adapter использует официальное поведение с локальными URL, а err сообщает об отказе настройки.
	adapter, err := New(limits)
	if err != nil {
		t.Fatal(err)
	}
	// socketOrigin заменяет только схему заранее известного адреса тестового сервера.
	socketOrigin := "wss" + strings.TrimPrefix(server.URL, "https")
	adapter.socketURL = socketOrigin + "/ws"
	adapter.helixBase = server.URL
	adapter.oauthBase = server.URL
	// transport сохраняет ограничения производства, testTransport предоставляет доверие локальному сертификату.
	transport := adapter.httpClient.Transport.(*http.Transport)
	testTransport := server.Client().Transport.(*http.Transport)
	transport.TLSClientConfig = testTransport.TLSClientConfig.Clone()
	t.Cleanup(transport.CloseIdleConnections)
	fixture.adapter = adapter
	fixture.reconnectURL = socketOrigin + "/handover?session=a%2Bb%2Fc"
	return fixture
}

// accept переводит HTTP-запрос r в серверное соединение и передаёт его тесту.
// Чтение управляющих кадров завершается по отмене ctx либо при закрытии соединения клиентом.
func (f *socketTestFixture) accept(t *testing.T, ctx context.Context, w http.ResponseWriter, r *http.Request) {
	// conn содержит открытый сокет теста, а err сообщает об отказе рукопожатия.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		t.Error(err)
		return
	}
	// Функция закрывает принадлежащий тесту сокет при завершении обработчика.
	defer func() { _ = conn.CloseNow() }()
	// watch читает управляющие кадры, чтобы сервер мог ждать Pong и закрытие.
	watch := conn.CloseRead(ctx)
	select {
	case f.peers <- socketTestPeer{conn: conn, path: r.URL.RequestURI(), closed: watch.Done()}:
	case <-ctx.Done():
	}
	<-watch.Done()
}

// subscribe проверяет POST r для чтения заданного канала от имени выбранного читателя.
// Запрос остаётся открытым до независимого решения теста или отмены ctx.
func (f *socketTestFixture) subscribe(t *testing.T, ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("Client-Id") != testClientID ||
		r.Header.Get("Authorization") != "Bearer "+testAccessToken {
		t.Errorf("Неверные метод или полномочия Helix %s, %v", r.Method, r.Header)
	}
	// body получает тип подписки, пару ID и адресат, заданный запросом адаптера.
	var body struct {
		// Type задаёт название принимаемого события.
		Type string
		// Version содержит версию события.
		Version string
		// Condition содержит идентификаторы канала и читателя.
		Condition map[string]string
		// Transport содержит метод доставки и ID сокета.
		Transport map[string]string
	}
	// err сообщает, удалось ли разобрать отправленный адаптером JSON.
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if body.Type != "channel.chat.message" || body.Version != "1" ||
		body.Condition["broadcaster_user_id"] != testBroadcasterID || body.Condition["user_id"] != testReaderID ||
		body.Transport["method"] != "websocket" || body.Transport["session_id"] == "" {
		t.Errorf("Подписка не привязана к ожидаемому каналу, читателю и сокету %+v", body)
	}
	// call передаёт тесту запрос, который пока ожидает ответа.
	call := socketTestCall{sessionID: body.Transport["session_id"], replies: make(chan socketTestReply, 1)}
	select {
	case f.calls <- call:
	case <-ctx.Done():
		return
	case <-r.Context().Done():
		return
	}
	select {
	// reply содержит независимое решение тестового сервера о создании подписки.
	case reply := <-call.replies:
		writeTwitchResponse(t, w, reply.status, reply.body)
	case <-ctx.Done():
	case <-r.Context().Done():
	}
}

// socketTestSubscription возвращает объект EventSub с заданными session, status и id.
// Идентичность канала и пользователя совпадает с ожидаемым запросом POST.
func socketTestSubscription(session, status, id string) string {
	return fmt.Sprintf(`{"id":%q,"status":%q,"type":"channel.chat.message","version":"1","condition":{"broadcaster_user_id":%q,"user_id":%q},"transport":{"method":"websocket","session_id":%q},"created_at":%q,"cost":0}`,
		id, status, testBroadcasterID, testReaderID, session, testTimestamp)
}

// acknowledge отправляет типичный ответ 202 с подтверждением заданных session и id.
func (c socketTestCall) acknowledge(session, id string) {
	c.replies <- socketTestReply{status: http.StatusAccepted, body: `{"data":[` + socketTestSubscription(session, "enabled", id) + `],"total":1,"total_cost":0,"max_total_cost":10}`}
}

// send записывает протокольный JSON body в сокет с ограничением ctx.
func (p socketTestPeer) send(t *testing.T, ctx context.Context, body string) {
	t.Helper()
	// err сообщает, доставлен ли кадр в сокет.
	if err := p.conn.Write(ctx, websocket.MessageText, []byte(body)); err != nil {
		t.Fatal(err)
	}
}

// welcome подтверждает session на этом сокете и передаёт срок keepalive в секундах.
func (p socketTestPeer) welcome(t *testing.T, ctx context.Context, session string, keepalive int) {
	t.Helper()
	p.send(t, ctx, fmt.Sprintf(`{"metadata":{"message_id":"welcome","message_type":"session_welcome","message_timestamp":%q},"payload":{"session":{"id":%q,"status":"connected","keepalive_timeout_seconds":%d,"reconnect_url":null,"connected_at":%q}}}`,
		testTimestamp, session, keepalive, testTimestamp))
}

// sendChat отправляет одно синтетическое сообщение с конкретными session, subscription,
// broadcaster и messageID. Различные ID чата и конверта проверяют выбор источника идентификатора.
func (p socketTestPeer) sendChat(t *testing.T, ctx context.Context, session, subscription, broadcaster, messageID string) {
	t.Helper()
	p.send(t, ctx, fmt.Sprintf(`{"metadata":{"message_id":"sent-by-eventsub","message_type":"notification","message_timestamp":%q,"subscription_type":"channel.chat.message","subscription_version":"1"},"payload":{"subscription":%s,"event":{"broadcaster_user_id":%q,"chatter_user_id":"455","chatter_user_login":"viewer","chatter_user_name":"Зритель","message_id":%q,"message":{"text":"Привет 🙂 Kappa","fragments":[{"type":"text","text":"Привет 🙂 "},{"type":"emote","text":"Kappa","emote":{"id":"25","emote_set_id":"0","owner_id":"0","format":["static"]}}]},"color":"#123ABC","badges":[{"set_id":"moderator","id":"1","info":""}],"message_type":"text","source_broadcaster_user_id":null}}}`,
		testTimestamp, socketTestSubscription(session, "enabled", subscription), broadcaster, messageID))
}

// readerTestStatus связывает наблюдаемое состояние источника с его типизированной причиной.
type readerTestStatus struct {
	// state содержит состояние в момент обратного вызова.
	state core.ChatSourceState
	// reason содержит код без исходного текста Twitch.
	reason core.ChatReason
}

// socketTestSink получает события чтения и сверяет идентификатор владельца каждого вызова.
type socketTestSink struct {
	// token идентифицирует проверяемого читателя.
	token core.ChatWorkerToken
	// statuses получает состояния независимо от потока сообщений.
	statuses chan readerTestStatus
	// messages получает нормализованные входящие сообщения.
	messages chan core.ChatIncomingMessage
	// gaps получает предупреждения о возможной потере.
	gaps chan core.ChatReason
	// allowConnected разрешает CONNECTED после выдачи подтверждения Helix.
	allowConnected atomic.Bool
	// prematureConnected фиксирует преждевременный вызов, даже если адаптер проигнорировал отказ sink.
	prematureConnected atomic.Bool
}

// Message принимает из ctx сообщение только для точного token и помещает данные в очередь теста.
func (s *socketTestSink) Message(ctx context.Context, token core.ChatWorkerToken, incoming core.ChatIncomingMessage) error {
	if s.token != token {
		return core.ErrStale
	}
	select {
	case s.messages <- incoming:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Status помещает state и reason в очередь теста для точного token.
// Если Helix ещё не подтвердил подписку, преждевременное CONNECTED прерывает сценарий.
func (s *socketTestSink) Status(ctx context.Context, token core.ChatWorkerToken, state core.ChatSourceState, reason core.ChatReason) error {
	if s.token != token {
		return core.ErrStale
	}
	if state == core.ChatConnected && !s.allowConnected.Load() {
		s.prematureConnected.Store(true)
		return errors.New("CONNECTED до подтверждения Helix")
	}
	select {
	case s.statuses <- readerTestStatus{state: state, reason: reason}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Gap принимает типизированный reason для точного token и передаёт его тесту до отмены ctx.
func (s *socketTestSink) Gap(ctx context.Context, token core.ChatWorkerToken, reason core.ChatReason) error {
	if s.token != token {
		return core.ErrStale
	}
	select {
	case s.gaps <- reason:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// awaitState дожидается состояния want и его причины до отмены ctx.
// Предшествующие переходы остаются допустимыми, а t прекращает тест при превышении срока.
func (s *socketTestSink) awaitState(t *testing.T, ctx context.Context, want core.ChatSourceState) readerTestStatus {
	t.Helper()
	for {
		// status содержит очередной полученный переход.
		status := awaitTwitchSignal(t, ctx, s.statuses)
		if status.state == want {
			return status
		}
	}
}

// socketTestReadRequest формирует полные синтетические полномочия и допуски читателя.
// overlaps учитывает только дополнительные сокеты во время управляемого перехода.
func socketTestReadRequest(overlaps *atomic.Int32) core.ChatReadRequest {
	return core.ChatReadRequest{
		Token: core.ChatWorkerToken{7},
		Key:   core.ChatWorkerKey{Provider: core.ChatTwitch, AccountID: "test-twitch-account", ChannelID: testBroadcasterID},
		Credentials: core.ChatCredentials{
			AccountID: "test-twitch-account", Provider: core.ChatTwitch, CredentialRevision: 1,
			CredentialUpdatedAt: testTime(), CredentialWriteID: "fixture-twitch-receipt",
			Bundle: core.ChatCredentialBundle{Twitch: &core.TwitchChatCredentials{
				ClientID: testClientID, UserID: testReaderID, AccessToken: testAccessToken, RefreshToken: testRefreshToken,
				Scopes: []string{"user:read:chat"}, ExpiresAt: time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour),
			}},
		},
		// Функция учитывает допуск временного второго сокета и возвращает функцию освобождения.
		AcquireConnection: func(ctx context.Context) (func(), error) {
			// err показывает, отменён ли переход до выдачи второго допуска.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			overlaps.Add(1)
			// Функция возвращает занятый допуск в момент закрытия одного из сокетов.
			return func() { overlaps.Add(-1) }, nil
		},
		// Функция предоставляет независимый допуск для единственного запроса Helix в попытке.
		AcquireHTTP: func(ctx context.Context) (func(), error) {
			// Функция представляет возврат допуска к сетевому запросу без дополнительного счётчика.
			return func() {}, ctx.Err()
		},
	}
}

// startSocketTestRead запускает реальное чтение fixture и возвращает наблюдателя и результат.
// t и ctx обеспечивают отмену и ограниченное ожидание завершения теста. request принадлежит сценарию.
func startSocketTestRead(t *testing.T, ctx context.Context, fixture *socketTestFixture, request core.ChatReadRequest) (*socketTestSink, <-chan error) {
	t.Helper()
	// sink получает события настоящего адаптера, не включая фоновый Manager.
	sink := &socketTestSink{
		token: request.Token, statuses: make(chan readerTestStatus, 32),
		messages: make(chan core.ChatIncomingMessage, 16), gaps: make(chan core.ChatReason, 8),
	}
	// readCtx и cancel связывают чтение с тестом и отменяют его при аварийном завершении сценария.
	readCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	// finished получает результат после закрытия принадлежащих адаптеру сокетов.
	finished := make(chan error, 1)
	// Горутина запускает фактическое чтение, пока тест управляет сервером.
	go func() { finished <- fixture.adapter.Read(readCtx, request, sink) }()
	return sink, finished
}

// TestEventSubAcknowledgementKeepaliveAndNormalization проверяет привязку подписки,
// отдельную обработку EventSub keepalive и WebSocket Ping и выдачу одного сообщения нужного канала.
// Сообщение другого канала не должно попасть в поток принятых событий.
func TestEventSubAcknowledgementKeepaliveAndNormalization(t *testing.T) {
	// ctx ограничивает время сценария, cancel закрывает все связанные операции.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// fixture отвечает на отдельный запрос подписки через локальный Helix.
	fixture := newSocketTestFixture(t, ctx, core.DefaultChatLimits())
	// readCtx и stopRead отменяют только адаптер. Тестовый сокет ждёт его закрытия.
	readCtx, stopRead := context.WithCancel(ctx)
	defer stopRead()
	// overlaps учитывает допуски для управляемого перехода, который здесь не нужен.
	var overlaps atomic.Int32
	// sink получает нормализованные сообщения, finished сообщает завершение читателя.
	sink, finished := startSocketTestRead(t, readCtx, fixture, socketTestReadRequest(&overlaps))
	// peer представляет первое физическое соединение.
	peer := awaitTwitchSignal(t, ctx, fixture.peers)
	peer.welcome(t, ctx, "socket-one", 10)
	// call показывает, какой session_id адаптер передал в Helix после welcome.
	call := awaitTwitchSignal(t, ctx, fixture.calls)
	if call.sessionID != "socket-one" {
		t.Fatalf("Запрос использовал session_id %q", call.sessionID)
	}
	sink.allowConnected.Store(true)
	call.acknowledge("socket-one", "subscription-one")
	sink.awaitState(t, ctx, core.ChatConnected)
	peer.send(t, ctx, fmt.Sprintf(`{"metadata":{"message_id":"keepalive","message_type":"session_keepalive","message_timestamp":%q},"payload":{}}`, testTimestamp))
	// err сообщает, получил ли сервер настоящий управляющий кадр Pong.
	if err := peer.conn.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	peer.sendChat(t, ctx, "socket-one", "subscription-one", "999", "чужое")
	peer.sendChat(t, ctx, "socket-one", "subscription-one", testBroadcasterID, "message-one")
	// got содержит первое принятое сообщение; отбрасывание чужого проверяется его идентичностью.
	got := awaitTwitchSignal(t, ctx, sink.messages)
	if got.Provider != core.ChatTwitch || got.ChannelID != testBroadcasterID || got.ProviderMessageID != "message-one" ||
		got.Kind != core.ChatMessageText || got.ProviderTimestamp == nil || !got.ProviderTimestamp.Equal(testTime()) ||
		got.Author != (core.ChatAuthor{ID: "455", DisplayName: "Зритель", Color: "#123ABC"}) || got.Text != "Привет 🙂 Kappa" {
		t.Fatalf("Сообщение нормализовано неверно %+v", got)
	}
	if len(got.Parts) != 2 || got.Parts[0].Kind != core.ChatPartText || got.Parts[0].Text != "Привет 🙂 " ||
		got.Parts[1].Kind != core.ChatPartEmote || got.Parts[1].Text != "Kappa" || got.Parts[1].EmoteID != "25" ||
		len(got.Badges) != 1 || !strings.Contains(got.Badges[0].ID, "moderator") || got.Validate() != nil {
		t.Fatalf("Повреждены фрагменты, бейдж или ссылки сообщения %+v", got)
	}
	stopRead()
	// err сообщает, как завершилось чтение после отмены отдельного сокета.
	if err := awaitTwitchSignal(t, ctx, finished); err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("Отменённое чтение завершилось ошибкой %v", err)
	}
	awaitTwitchSignal(t, ctx, peer.closed)
	if len(sink.gaps) != 0 || len(sink.messages) != 0 || overlaps.Load() != 0 || sink.prematureConnected.Load() {
		t.Fatal("Обычная отмена оставила допуск, лишнее сообщение или ошибочно сообщила о пробеле")
	}
}

// TestEventSubRejectsUnconfirmedSubscription проверяет, что ответ 202 сам по себе
// не переводит читателя в CONNECTED, если ответ относится к другому session_id.
func TestEventSubRejectsUnconfirmedSubscription(t *testing.T) {
	// ctx задаёт конечный срок отклонения неподходящего подтверждения, cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// fixture предлагает подтверждение с чужим ID сокета.
	fixture := newSocketTestFixture(t, ctx, core.DefaultChatLimits())
	// overlaps учитывает второй сокет, который здесь не требуется.
	var overlaps atomic.Int32
	// sink не разрешает CONNECTED, а finished возвращает итог этой попытки.
	sink, finished := startSocketTestRead(t, ctx, fixture, socketTestReadRequest(&overlaps))
	// peer получает корректное welcome для исходного сокета.
	peer := awaitTwitchSignal(t, ctx, fixture.peers)
	peer.welcome(t, ctx, "socket-one", 10)
	// call идентифицирует отдельный HTTP POST. Ответ содержит чужой идентификатор сокета.
	call := awaitTwitchSignal(t, ctx, fixture.calls)
	call.acknowledge("socket-other", "subscription-one")
	// err сообщает, отклонено ли неверное подтверждение до первого состояния CONNECTED.
	err := awaitTwitchSignal(t, ctx, finished)
	if err == nil || sink.prematureConnected.Load() || len(sink.gaps) != 0 || len(sink.messages) != 0 {
		t.Fatalf("Чужая подписка обработана как подтверждённая, ошибка %v", err)
	}
	awaitTwitchSignal(t, ctx, peer.closed)
}

// TestEventSubManagedHandoverKeepsOldReaderUntilWelcome проверяет приём сообщения
// старым сокетом во время перехода, точный адрес перехода, отсутствие новой подписки,
// непрерывность ленты и освобождение временного допуска после закрытия старого сокета.
func TestEventSubManagedHandoverKeepsOldReaderUntilWelcome(t *testing.T) {
	// ctx ограничивает обмен двумя сокетами, cancel останавливает чтение в конце.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// fixture выдаёт специальный URL с процентным кодированием в строке запроса.
	fixture := newSocketTestFixture(t, ctx, core.DefaultChatLimits())
	// readCtx и stopRead позволяют отменить чтение, пока второй сокет ждёт клиентского закрытия.
	readCtx, stopRead := context.WithCancel(ctx)
	defer stopRead()
	// overlaps отслеживает одновременное удержание второго сокета.
	var overlaps atomic.Int32
	// sink получает сообщения обоих соединений, finished возвращает итог единого читателя.
	sink, finished := startSocketTestRead(t, readCtx, fixture, socketTestReadRequest(&overlaps))
	// first представляет старый сокет, на котором создана подписка.
	first := awaitTwitchSignal(t, ctx, fixture.peers)
	first.welcome(t, ctx, "socket-one", 10)
	// call принимает запрос текущей подписки.
	call := awaitTwitchSignal(t, ctx, fixture.calls)
	sink.allowConnected.Store(true)
	call.acknowledge("socket-one", "subscription-one")
	sink.awaitState(t, ctx, core.ChatConnected)
	first.send(t, ctx, fmt.Sprintf(`{"metadata":{"message_id":"reconnect","message_type":"session_reconnect","message_timestamp":%q},"payload":{"session":{"id":"socket-one","status":"reconnecting","keepalive_timeout_seconds":null,"reconnect_url":%q,"connected_at":%q}}}`,
		testTimestamp, fixture.reconnectURL, testTimestamp))
	// next принят по URL перехода, но welcome ещё намеренно не отправлен.
	next := awaitTwitchSignal(t, ctx, fixture.peers)
	if next.path != "/handover?session=a%2Bb%2Fc" || overlaps.Load() != 1 {
		t.Fatalf("Переход открыл путь %q с числом временных допусков %d", next.path, overlaps.Load())
	}
	first.sendChat(t, ctx, "socket-one", "subscription-one", testBroadcasterID, "before-welcome")
	// previous подтверждает, что старый сокет остаётся читаемым в ожидании нового welcome.
	previous := awaitTwitchSignal(t, ctx, sink.messages)
	if previous.ProviderMessageID != "before-welcome" {
		t.Fatalf("Старый сокет не доставил сообщение во время перехода %+v", previous)
	}
	next.welcome(t, ctx, "socket-two", 10)
	awaitTwitchSignal(t, ctx, first.closed)
	next.sendChat(t, ctx, "socket-two", "subscription-one", testBroadcasterID, "after-welcome")
	// current приходит через новую физическую связь при той же подписке.
	current := awaitTwitchSignal(t, ctx, sink.messages)
	if current.ProviderMessageID != "after-welcome" || overlaps.Load() != 0 || len(fixture.calls) != 0 || len(sink.gaps) != 0 {
		t.Fatalf("Переход сменил логическую подписку, потерял доставку или удержал допуск %+v", current)
	}
	stopRead()
	awaitTwitchCancellation(t, ctx, finished)
	awaitTwitchSignal(t, ctx, next.closed)
}

// TestEventSubDisconnectRequiresFreshSubscription проверяет, что внезапный разрыв
// установленной связи публикует один upstream-пробел и возвращает управление для новой попытки.
// Повторная попытка использует исходный адрес и новый session_id без позиции replay.
func TestEventSubDisconnectRequiresFreshSubscription(t *testing.T) {
	// ctx задаёт предел двум последовательным читателям, cancel освобождает их ресурсы.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// fixture даёт два базовых подключения с независимо выданными ID.
	fixture := newSocketTestFixture(t, ctx, core.DefaultChatLimits())
	// overlaps учитывает временный второй сокет только при управляемом переходе.
	var overlaps atomic.Int32
	// request задаёт один логический чат, sink и finished относятся к первой попытке.
	request := socketTestReadRequest(&overlaps)
	sink, finished := startSocketTestRead(t, ctx, fixture, request)
	// first представляет сокет, который сервер закроет без выдачи session_reconnect.
	first := awaitTwitchSignal(t, ctx, fixture.peers)
	first.welcome(t, ctx, "socket-one", 10)
	// firstCall создаёт подписку первой попытки.
	firstCall := awaitTwitchSignal(t, ctx, fixture.calls)
	sink.allowConnected.Store(true)
	firstCall.acknowledge("socket-one", "subscription-first")
	sink.awaitState(t, ctx, core.ChatConnected)
	// err подтверждает, что тестовый сервер закрыл принадлежащий ему сокет.
	if err := first.conn.CloseNow(); err != nil {
		t.Fatal(err)
	}
	// gap сообщает о возможной потере сообщений, которые адаптер не получил.
	gap := awaitTwitchSignal(t, ctx, sink.gaps)
	// failure содержит результат физического чтения до запуска новой попытки.
	failure := awaitTwitchSignal(t, ctx, finished)
	if gap != core.ChatReasonUpstreamGap || failure == nil || len(sink.gaps) != 0 {
		t.Fatalf("Разрыв дал пробел %q, ошибку %v и дополнительные пробелы %d", gap, failure, len(sink.gaps))
	}
	// retry может содержать задержку, но Twitch не предоставляет позиции продолжения.
	var retry *core.ChatReadRetry
	if errors.As(failure, &retry) && retry.ResumeToken != "" {
		t.Fatal("Адаптер обещал позицию replay после полного разрыва Twitch")
	}
	// nextSink и nextFinished наблюдают вновь созданную попытку того же читателя.
	// nextCtx и stopNext позволяют отменить адаптер отдельно от сервера.
	nextCtx, stopNext := context.WithCancel(ctx)
	defer stopNext()
	nextSink, nextFinished := startSocketTestRead(t, nextCtx, fixture, request)
	// second представляет новую связь, созданную по базовому адресу.
	second := awaitTwitchSignal(t, ctx, fixture.peers)
	// target содержит путь исходного адреса, а err сообщает, если тест настроил непригодное значение.
	target, err := url.Parse(fixture.adapter.socketURL)
	if err != nil || second.path != target.RequestURI() {
		t.Fatalf("Новая попытка использовала %q вместо исходного адреса", second.path)
	}
	second.welcome(t, ctx, "socket-two", 10)
	// nextCall подтверждает запрос новой подписки на второй session_id.
	nextCall := awaitTwitchSignal(t, ctx, fixture.calls)
	if nextCall.sessionID != "socket-two" || overlaps.Load() != 0 {
		t.Fatalf("Новая подписка не связана со вторым базовым сокетом %+v", nextCall)
	}
	nextSink.allowConnected.Store(true)
	nextCall.acknowledge("socket-two", "subscription-second")
	nextSink.awaitState(t, ctx, core.ChatConnected)
	second.sendChat(t, ctx, "socket-two", "subscription-second", testBroadcasterID, "new-message")
	// latest подтверждает доставку после создания подписки заново.
	latest := awaitTwitchSignal(t, ctx, nextSink.messages)
	if latest.ProviderMessageID != "new-message" || len(nextSink.gaps) != 0 {
		t.Fatalf("Новая подписка получила неверное сообщение или повторила пробел %+v", latest)
	}
	stopNext()
	awaitTwitchCancellation(t, ctx, nextFinished)
	awaitTwitchSignal(t, ctx, second.closed)
}

// TestEventSubRevocationAndMessageLimit проверяет остановку при отзыве той же подписки
// и ограничение кадра от авторизованного сокета. Чужой отзыв не останавливает читателя.
func TestEventSubRevocationAndMessageLimit(t *testing.T) {
	// oversized различает превышение конечного бюджета и подтверждённый отзыв.
	for _, oversized := range []bool{false, true} {
		// name обозначает отказ, который моделирует сервер после принятия подписки.
		name := "отзыв полномочий"
		if oversized {
			name = "кадр превышает бюджет"
		}
		// Подтест открывает одну подписку и отправляет причину её завершения.
		t.Run(name, func(t *testing.T) {
			// ctx ограничивает всё чтение, cancel останавливает его при раннем выходе.
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// limits задаёт меньший бюджет кадра, который вмещает обычное сообщение.
			limits := core.DefaultChatLimits()
			limits.UpstreamMessageBytes = 2048
			// fixture предоставляет синтетический поток выбранного чата.
			fixture := newSocketTestFixture(t, ctx, limits)
			// overlaps учитывает временные соединения и должен остаться пустым.
			var overlaps atomic.Int32
			// sink получает безопасные переходы, а finished подтверждает прекращение чтения.
			sink, finished := startSocketTestRead(t, ctx, fixture, socketTestReadRequest(&overlaps))
			// peer отправляет welcome и отказ после согласованной подписки.
			peer := awaitTwitchSignal(t, ctx, fixture.peers)
			peer.welcome(t, ctx, "socket-one", 10)
			// call содержит привязку запроса к созданному сокету.
			call := awaitTwitchSignal(t, ctx, fixture.calls)
			sink.allowConnected.Store(true)
			call.acknowledge("socket-one", "subscription-one")
			sink.awaitState(t, ctx, core.ChatConnected)

			if oversized {
				peer.send(t, ctx, fmt.Sprintf(`{"metadata":{"message_id":"oversized","message_type":"session_keepalive","message_timestamp":%q},"payload":{"padding":%q}}`,
					testTimestamp, strings.Repeat("x", 2048)))
				// status должен сообщать о превышении лимита вместо принятия кадра.
				status := sink.awaitState(t, ctx, core.ChatRetrying)
				if status.reason != core.ChatReasonLimit || awaitTwitchSignal(t, ctx, sink.gaps) != core.ChatReasonUpstreamGap {
					t.Fatalf("Превышение лимита не отразилось в статусе и пробеле %+v", status)
				}
			} else {
				peer.revoke(t, ctx, "subscription-other")
				peer.sendChat(t, ctx, "socket-one", "subscription-one", testBroadcasterID, "after-other-revocation")
				// alive доказывает, что отзыв чужой подписки не прервал текущую доставку.
				alive := awaitTwitchSignal(t, ctx, sink.messages)
				if alive.ProviderMessageID != "after-other-revocation" {
					t.Fatalf("Чужой отзыв затронул логическую подписку %+v", alive)
				}
				peer.revoke(t, ctx, "subscription-one")
				// status сообщает о необходимости новой авторизации после подтверждённого revocation.
				status := sink.awaitState(t, ctx, core.ChatAuthRequired)
				if status.reason != core.ChatReasonCredentials {
					t.Fatalf("Отзыв не перевёл читателя в AUTH_REQUIRED с безопасной причиной %+v", status)
				}
			}
			// err различает исчерпание транспорта и явно отозванную подписку.
			err := awaitTwitchSignal(t, ctx, finished)
			// retry передаёт указание Manager, допустимое только после потери транспорта.
			var retry *core.ChatReadRetry
			if oversized && !errors.As(err, &retry) || !oversized && err != nil {
				t.Fatalf("Читатель завершился с неожиданной ошибкой %v", err)
			}
			awaitTwitchSignal(t, ctx, peer.closed)
			if len(fixture.calls) != 0 || len(sink.messages) != 0 || len(sink.gaps) != 0 || overlaps.Load() != 0 {
				t.Fatal("Завершённая попытка оставила повторную подписку, сообщение, пробел или допуск")
			}
		})
	}
}

// revoke отправляет отзыв полномочий для subscription на первом сокете фикстуры.
// ctx ограничивает запись кадра, t сообщает о нарушении сценария доставки.
func (p socketTestPeer) revoke(t *testing.T, ctx context.Context, subscription string) {
	t.Helper()
	p.send(t, ctx, fmt.Sprintf(`{"metadata":{"message_id":"revocation","message_type":"revocation","message_timestamp":%q,"subscription_type":"channel.chat.message","subscription_version":"1"},"payload":{"subscription":%s}}`,
		testTimestamp, socketTestSubscription("socket-one", "authorization_revoked", subscription)))
}

// TestEventSubValidatesQuietReader проверяет, что тишина на сокете не отменяет
// очередную проверку через владельца аккаунта. Успешная ротация сохраняет подписку,
// подтверждённая непригодность полномочий останавливает чтение.
func TestEventSubValidatesQuietReader(t *testing.T) {
	// ctx ограничивает обе проверки, cancel закрывает соединение при отказе теста.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// fixture заменяет только период проверки, сохраняя реальную работу сокета.
	fixture := newSocketTestFixture(t, ctx, core.DefaultChatLimits())
	fixture.adapter.validationInterval = 80 * time.Millisecond
	// overlaps считает временные подключения, которых успешная проверка не создаёт.
	var overlaps atomic.Int32
	// request предоставляет полномочия читателю, а updated описывает сохранённую ротацию.
	request := socketTestReadRequest(&overlaps)
	updated := socketTestReadRequest(&overlaps).Credentials
	updated.Bundle.Twitch.AccessToken = "fixture-updated-access"
	updated.Bundle.Twitch.RefreshToken = "fixture-updated-refresh"
	updated.CredentialRevision++
	updated.CredentialWriteID = "fixture-updated-receipt"
	// verified отмечает первую и вторую проверки, reject разрешает второй проверке вернуть отказ.
	verified, reject := make(chan int, 4), make(chan struct{})
	// checks нумерует обращения адаптера к владельцу одного аккаунта.
	var checks atomic.Int32
	// Функция имитирует сохранённую ротацию и последующий отзыв для того же аккаунта.
	request.RefreshCredentials = func(callCtx context.Context) (core.ChatCredentials, error) {
		// count различает успешную первую проверку и вторую, ожидающую разрешения теста.
		count := int(checks.Add(1))
		verified <- count
		if count == 1 {
			return updated, nil
		}
		select {
		case <-reject:
			return core.ChatCredentials{}, core.ErrInvalid
		case <-callCtx.Done():
			return core.ChatCredentials{}, callCtx.Err()
		}
	}
	// sink получает статусы и уведомления, а finished сообщает об завершении читателя после отзыва.
	sink, finished := startSocketTestRead(t, ctx, fixture, request)
	// peer представляет один сокет до и после успешной проверки.
	peer := awaitTwitchSignal(t, ctx, fixture.peers)
	peer.welcome(t, ctx, "socket-one", 10)
	// call задаёт подтверждение единственной подписки без первоначального входящего сообщения.
	call := awaitTwitchSignal(t, ctx, fixture.calls)
	sink.allowConnected.Store(true)
	call.acknowledge("socket-one", "subscription-one")
	sink.awaitState(t, ctx, core.ChatConnected)
	awaitTwitchSignal(t, ctx, verified)
	peer.sendChat(t, ctx, "socket-one", "subscription-one", testBroadcasterID, "after-token-rotation")
	// message доказывает сохранение действующей подписки после замены токенов.
	message := awaitTwitchSignal(t, ctx, sink.messages)
	if message.ProviderMessageID != "after-token-rotation" || overlaps.Load() != 0 {
		t.Fatalf("Ротация прервала подписку %+v", message)
	}
	awaitTwitchSignal(t, ctx, verified)
	close(reject)
	// status содержит публичный результат подтверждённого отказа владельца полномочий.
	status := sink.awaitState(t, ctx, core.ChatAuthRequired)
	if status.reason != core.ChatReasonCredentials {
		t.Fatalf("Необратимый отказ проверки дал неверную причину %+v", status)
	}
	// err сообщает, завершилась ли отозванная подписка без указания повторить её.
	if err := awaitTwitchSignal(t, ctx, finished); err != nil {
		t.Fatalf("Отозванная подписка вернула неожиданный результат %v", err)
	}
	awaitTwitchSignal(t, ctx, peer.closed)
	if len(fixture.calls) != 0 || len(fixture.peers) != 0 {
		t.Fatal("Проверка действующего читателя создала вторую подписку или сокет")
	}
}

// TestEventSubKeepaliveExtendsDeadlineAndSilenceReportsGap проверяет, что допустимые
// keepalive сохраняют подписку за пределами первого окна, а тишина завершает попытку с пробелом.
func TestEventSubKeepaliveExtendsDeadlineAndSilenceReportsGap(t *testing.T) {
	// ctx ограничивает проверку нескольких окон keepalive. cancel завершает фикстуры при отказе.
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	// fixture намеренно задаёт короткое окно тишины в welcome.
	fixture := newSocketTestFixture(t, ctx, core.DefaultChatLimits())
	// overlaps остаётся пустым без указания Twitch выполнить управляемый переход.
	var overlaps atomic.Int32
	// sink позволяет наблюдать продление жизни подписки, finished получает итог после тишины.
	sink, finished := startSocketTestRead(t, ctx, fixture, socketTestReadRequest(&overlaps))
	// peer представляет единственный сокет этого сценария.
	peer := awaitTwitchSignal(t, ctx, fixture.peers)
	peer.welcome(t, ctx, "socket-one", 1)
	// call подтверждает единственную подписку, для которой действует окно тишины.
	call := awaitTwitchSignal(t, ctx, fixture.calls)
	sink.allowConnected.Store(true)
	call.acknowledge("socket-one", "subscription-one")
	sink.awaitState(t, ctx, core.ChatConnected)

	// ticker выдаёт keepalive с запасом до конца окна. Пять отметок проходят за пределы первого окна.
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	// index различает идентификаторы оконных сообщений.
	for index := range 5 {
		awaitTwitchSignal(t, ctx, ticker.C)
		peer.send(t, ctx, fmt.Sprintf(`{"metadata":{"message_id":"keepalive-%d","message_type":"session_keepalive","message_timestamp":%q},"payload":{}}`, index, testTimestamp))
	}
	ticker.Stop()
	peer.sendChat(t, ctx, "socket-one", "subscription-one", testBroadcasterID, "after-original-timeout")
	// message доказывает, что исходный срок не оборвал согласованную подписку.
	message := awaitTwitchSignal(t, ctx, sink.messages)
	if message.ProviderMessageID != "after-original-timeout" || len(sink.gaps) != 0 {
		t.Fatalf("Keepalive не сохранил пригодную подписку %+v", message)
	}
	// status и gap подтверждают, что без следующих keepalive адаптер видит разрыв.
	status := sink.awaitState(t, ctx, core.ChatRetrying)
	gap := awaitTwitchSignal(t, ctx, sink.gaps)
	// err и retry подтверждают, что текущая попытка завершилась с разрешённым повтором.
	err := awaitTwitchSignal(t, ctx, finished)
	var retry *core.ChatReadRetry
	if status.reason != core.ChatReasonNetwork || gap != core.ChatReasonUpstreamGap ||
		!errors.As(err, &retry) || retry.ResumeToken != "" || len(sink.gaps) != 0 {
		t.Fatalf("Тишина дала статус %+v, пробел %q, ошибку %v", status, gap, err)
	}
	awaitTwitchSignal(t, ctx, peer.closed)
}
