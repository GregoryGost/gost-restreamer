package twitch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestTwitchDiscoversExactlyTheConfiguredChat проверяет разрешение ID без сетевого поиска.
// При пустой настройке используется ID авторизованного владельца, а чужие полномочия отклоняются.
func TestTwitchDiscoversExactlyTheConfiguredChat(t *testing.T) {
	// adapter использует реальный путь разрешения канала. err сообщает об ошибке создания клиента.
	adapter, err := New(core.DefaultChatLimits())
	if err != nil {
		t.Fatal(err)
	}
	// overlaps не используется при поиске. account содержит совместимые синтетические полномочия.
	var overlaps atomic.Int32
	account := socketTestReadRequest(&overlaps).Credentials
	// source описывает одну настройку Twitch для этого аккаунта.
	source := core.ChatSource{AccountID: account.AccountID, Provider: adapter.Provider()}
	// own содержит канал авторизованного пользователя.
	own, err := adapter.Discover(t.Context(), source, account)
	if err != nil || own.ChannelID != testReaderID {
		t.Fatalf("Не найден собственный чат: %+v, %v", own, err)
	}
	source.ChannelHint = "9012345678901234567890"
	// explicit проверяет сохранение десятичных ID без машинного числового преобразования.
	explicit, err := adapter.Discover(t.Context(), source, account)
	if err != nil || explicit.ChannelID != source.ChannelHint {
		t.Fatalf("Изменён явный ID канала: %+v, %v", explicit, err)
	}
	source.ChannelHint = "channel-name"
	// invalid описывает подтверждённую ошибку настройки канала.
	invalid, err := adapter.Discover(t.Context(), source, account)
	if err != nil || invalid.ChannelID != "" || invalid.State != core.ChatFailed {
		t.Fatalf("Нет отказа от неверного канала: %+v, %v", invalid, err)
	}
	source.ChannelHint = ""
	account.Bundle.Twitch.Scopes = nil
	// denied сигнализирует отсутствие обязательного разрешения.
	denied, err := adapter.Discover(t.Context(), source, account)
	if err != nil || denied.State != core.ChatAuthRequired {
		t.Fatalf("Поиск принял недействительные полномочия: %+v, %v", denied, err)
	}
}

// TestEventSubHelixFailureClassification подтверждает, что отказ канала 403 при действующем
// аккаунте отличается от истёкшего доступа, временной квоты и отключённой версии подписки.
func TestEventSubHelixFailureClassification(t *testing.T) {
	// cases связывает ответ Helix и результат актуальной проверки аккаунта с публичным исходом.
	cases := []struct {
		// name называет наблюдаемую причину отказа.
		name string
		// status задаёт HTTP-ответ на создание подписки.
		status int
		// validation моделирует отказ уже сериализованной проверки или успешное получение пары.
		validation error
		// state содержит обещанное источнику состояние.
		state core.ChatSourceState
		// reason объясняет его безопасным типизированным значением.
		reason core.ChatReason
	}{
		{"канал", http.StatusForbidden, nil, core.ChatFailed, core.ChatReasonUnavailable},
		{"аккаунт", http.StatusUnauthorized, core.ErrInvalid, core.ChatAuthRequired, core.ChatReasonCredentials},
		{"ограничение проверки", http.StatusUnauthorized, &core.ChatReadRetry{RetryAfter: 12 * time.Second}, core.ChatRetrying, core.ChatReasonQuota},
		{"квота подписки", http.StatusTooManyRequests, nil, core.ChatRetrying, core.ChatReasonQuota},
		{"отключённая версия", http.StatusGone, nil, core.ChatFailed, core.ChatReasonConfiguration},
		{"ответ сервера", http.StatusServiceUnavailable, nil, core.ChatRetrying, core.ChatReasonNetwork},
	}
	// scenario задаёт один отказ на отдельном реальном сокете.
	for _, scenario := range cases {
		// Подтест проверяет итог попытки, которая ещё не имела подтверждённой подписки.
		t.Run(scenario.name, func(t *testing.T) {
			// ctx ограничивает обе сетевые границы. cancel прекращает тест при раннем отказе.
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			// fixture принимает welcome и один запрос Helix на синтетическом сервере.
			fixture := newSocketTestFixture(t, ctx, core.DefaultChatLimits())
			// overlaps остаётся нулём, поскольку подписка ещё не была подтверждена.
			var overlaps atomic.Int32
			// request содержит синтетические полномочия и ответ владельца на дополнительную проверку.
			request := socketTestReadRequest(&overlaps)
			// Функция даёт текущую пару либо заранее известный отказ проверки.
			request.RefreshCredentials = func(context.Context) (core.ChatCredentials, error) {
				return request.Credentials, scenario.validation
			}
			// sink проверяет состояние, finished подтверждает закрытие попытки.
			sink, finished := startSocketTestRead(t, ctx, fixture, request)
			// peer предоставляет первое соединение для согласования подписки.
			peer := awaitTwitchSignal(t, ctx, fixture.peers)
			peer.welcome(t, ctx, "opening-session", 10)
			// call принимает публично классифицируемый отказ Helix.
			call := awaitTwitchSignal(t, ctx, fixture.calls)
			call.replies <- socketTestReply{status: scenario.status, body: `{"message":"synthetic-request-data"}`}
			// result содержит единственный переход после отказа, err возвращает указание менеджеру.
			result := awaitTwitchSignal(t, ctx, sink.statuses)
			err := awaitTwitchSignal(t, ctx, finished)
			awaitTwitchSignal(t, ctx, peer.closed)
			if result.state != scenario.state || result.reason != scenario.reason ||
				len(sink.gaps) != 0 || sink.prematureConnected.Load() {
				t.Fatalf("Отказ дал состояние %+v и ошибку %v", result, err)
			}
			// retry содержит минимальный срок ожидания, если OAuth ограничил частоту проверки.
			var retry *core.ChatReadRetry
			if scenario.validation != nil && scenario.reason == core.ChatReasonQuota &&
				(!errors.As(err, &retry) || retry.RetryAfter != 12*time.Second) {
				t.Fatalf("Потеряна нижняя граница повтора проверки: %v", err)
			}
		})
	}
}

// TestEventSubOnlyAcceptsTrustedReconnectOrigin проверяет право открыть соединение по URL.
// Адрес с реквизитами, сменой схемы или хоста не может перенести доверие действующего WebSocket.
func TestEventSubOnlyAcceptsTrustedReconnectOrigin(t *testing.T) {
	// origin соответствует настоящей фиксированной точке входа в Twitch.
	const origin = "wss://eventsub.wss.twitch.tv/ws"
	// target содержит очередное изменение адреса, которое запрещено при переносе.
	for _, target := range []string{
		"", "ws://eventsub.wss.twitch.tv/ws", "wss://other.invalid/ws",
		"wss://user@eventsub.wss.twitch.tv/ws", "wss://eventsub.wss.twitch.tv:444/ws",
		"wss://eventsub.wss.twitch.tv/ws#fragment",
	} {
		if trustedReconnectURL(origin, target) {
			t.Errorf("Одобрен посторонний адрес переноса %q", target)
		}
	}
	if !trustedReconnectURL(origin, "wss://eventsub.wss.twitch.tv:443/ws?session=a%2Bb") {
		t.Fatal("Отклонён исходный WSS-сервис со стандартным портом и исходной строкой запроса")
	}
}

// TestEventSubInvalidHandoverWaitsForTransportLoss проверяет, что неправильное приветствие
// второго сокета освобождает временный допуск, пока прежний канал ещё передаёт сообщения.
func TestEventSubInvalidHandoverWaitsForTransportLoss(t *testing.T) {
	// ctx ограничивает две сетевые попытки. cancel освобождает их при раннем отказе теста.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// fixture предоставляет исходный и запасной сокеты в одном доверенном TLS-сервисе.
	fixture := newSocketTestFixture(t, ctx, core.DefaultChatLimits())
	// overlaps отражает допуск второго соединения во время переноса.
	var overlaps atomic.Int32
	// sink получает только события уже подтверждённой исходной подписки.
	sink, finished := startSocketTestRead(t, ctx, fixture, socketTestReadRequest(&overlaps))
	// original открывает подписку до команды переноса.
	original := awaitTwitchSignal(t, ctx, fixture.peers)
	original.welcome(t, ctx, "socket-one", 10)
	// call подтверждает подписку старого сокета.
	call := awaitTwitchSignal(t, ctx, fixture.calls)
	sink.allowConnected.Store(true)
	call.acknowledge("socket-one", "subscription-one")
	sink.awaitState(t, ctx, core.ChatConnected)
	original.send(t, ctx, fmt.Sprintf(`{"metadata":{"message_id":"handover","message_type":"session_reconnect","message_timestamp":%q},"payload":{"session":{"id":"socket-one","status":"reconnecting","reconnect_url":%q}}}`, testTimestamp, fixture.reconnectURL))
	// replacement адресует сокет переноса, который нарушает обязательную форму приветствия.
	replacement := awaitTwitchSignal(t, ctx, fixture.peers)
	replacement.welcome(t, ctx, "socket-two", 0)
	awaitTwitchSignal(t, ctx, replacement.closed)
	original.sendChat(t, ctx, "socket-one", "subscription-one", testBroadcasterID, "still-active")
	// delivered доказывает, что отказ второго сокета не прервал действующую подписку.
	delivered := awaitTwitchSignal(t, ctx, sink.messages)
	if delivered.ProviderMessageID != "still-active" || len(sink.gaps) != 0 {
		t.Fatal("Ошибка переноса отозвала ещё действующую подписку")
	}
	// err сообщает об отказе тестового сервера закрыть исходный сокет.
	if err := original.conn.CloseNow(); err != nil {
		t.Fatal(err)
	}
	// gap появляется только после потери последнего подтверждённого соединения.
	gap := awaitTwitchSignal(t, ctx, sink.gaps)
	// err и retry проверяют, что потеря исходного сокета разрешает новую подписку.
	err := awaitTwitchSignal(t, ctx, finished)
	var retry *core.ChatReadRetry
	if !errors.As(err, &retry) {
		t.Fatalf("После разрыва ожидается новая попытка, получено %v", err)
	}
	if gap != core.ChatReasonUpstreamGap || overlaps.Load() != 0 || len(fixture.calls) != 0 {
		t.Fatal("Потеря обоих сокетов скрыла пробел либо оставила временный сетевой допуск")
	}
}

// TestEventSubInvalidReconnectCommandPreservesSubscription подтверждает, что команда
// переноса на другой адрес не прерывает чтение действующей подписки.
func TestEventSubInvalidReconnectCommandPreservesSubscription(t *testing.T) {
	// ctx и cancel удерживают единственное тестовое подключение до конца сценария.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// fixture принимает только исходное TLS-соединение для выбранного чата.
	fixture := newSocketTestFixture(t, ctx, core.DefaultChatLimits())
	// overlaps проверяет отсутствие запроса второго сетевого допуска.
	var overlaps atomic.Int32
	// sink получает события подписки. finished сообщает о её фактической остановке.
	sink, finished := startSocketTestRead(t, ctx, fixture, socketTestReadRequest(&overlaps))
	// original получает действующую подписку на первом сокете.
	original := awaitTwitchSignal(t, ctx, fixture.peers)
	original.welcome(t, ctx, "socket-one", 10)
	// call отвечает подтверждением точной подписки, ожидаемой адаптером.
	call := awaitTwitchSignal(t, ctx, fixture.calls)
	sink.allowConnected.Store(true)
	call.acknowledge("socket-one", "subscription-one")
	sink.awaitState(t, ctx, core.ChatConnected)
	original.send(t, ctx, fmt.Sprintf(`{"metadata":{"message_id":"reconnect","message_type":"session_reconnect","message_timestamp":%q},"payload":{"session":{"id":"socket-one","status":"reconnecting","reconnect_url":"wss://127.0.0.1:1/ws"}}}`, testTimestamp))
	original.sendChat(t, ctx, "socket-one", "subscription-one", testBroadcasterID, "after-invalid-command")
	// delivered проверяет продолжение чтения с прежнего сокета.
	delivered := awaitTwitchSignal(t, ctx, sink.messages)
	if delivered.ProviderMessageID != "after-invalid-command" || len(sink.gaps) != 0 || overlaps.Load() != 0 {
		t.Fatal("Недействительная команда переноса изменила подписку или создала ложный пробел")
	}
	cancel()
	// closed и stop ограничивают ожидание закрытия после штатной отмены.
	closed, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	awaitTwitchCancellation(t, closed, finished)
	awaitTwitchSignal(t, closed, original.closed)
}

// temporarilyBlockedSink отклоняет сообщение, пришедшее во время замены аккаунта.
type temporarilyBlockedSink struct {
	// socketTestSink наблюдает остальные callbacks живого читателя.
	*socketTestSink
	// blocked сообщает о вызове, которому Manager временно не разрешил публикацию.
	blocked chan struct{}
}

// Message возвращает временный ErrStale для одного сообщения при ещё действующем ctx.
func (s *temporarilyBlockedSink) Message(ctx context.Context, token core.ChatWorkerToken, message core.ChatIncomingMessage) error {
	if message.ProviderMessageID == "account-mutation" {
		close(s.blocked)
		return core.ErrStale
	}
	return s.socketTestSink.Message(ctx, token, message)
}

// TestEventSubTemporaryMutationPreservesReader подтверждает, что временно закрытый
// допуск аккаунта не разрывает Twitch-подписку после отклонённого изменения.
func TestEventSubTemporaryMutationPreservesReader(t *testing.T) {
	// ctx ограничивает подписку и ожидание восстановления допуска. cancel закрывает сокет.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// fixture принимает единственную физическую подписку, которую тест сохраняет до отмены.
	fixture := newSocketTestFixture(t, ctx, core.DefaultChatLimits())
	// overlaps и request задают обычные пределы и полномочия одного читателя.
	var overlaps atomic.Int32
	request := socketTestReadRequest(&overlaps)
	// sink имитирует краткий запрет Manager, оставляя сетевой контекст действующим.
	sink := &temporarilyBlockedSink{
		socketTestSink: &socketTestSink{
			token: request.Token, statuses: make(chan readerTestStatus, 4),
			messages: make(chan core.ChatIncomingMessage, 2), gaps: make(chan core.ChatReason, 2),
		},
		blocked: make(chan struct{}),
	}
	// finished получает итог единственного Read после освобождения связи.
	finished := make(chan error, 1)
	// Функция запускает реальный EventSub, пока тест управляет сервером.
	go func() { finished <- fixture.adapter.Read(ctx, request, sink) }()
	// peer представляет синтетический сервер для сохранённой подписки.
	peer := awaitTwitchSignal(t, ctx, fixture.peers)
	peer.welcome(t, ctx, "socket-one", 10)
	// call подтверждает точную подписку по принятому Helix POST.
	call := awaitTwitchSignal(t, ctx, fixture.calls)
	sink.allowConnected.Store(true)
	call.acknowledge("socket-one", "subscription-one")
	sink.awaitState(t, ctx, core.ChatConnected)
	peer.sendChat(t, ctx, "socket-one", "subscription-one", testBroadcasterID, "account-mutation")
	awaitTwitchSignal(t, ctx, sink.blocked)
	peer.sendChat(t, ctx, "socket-one", "subscription-one", testBroadcasterID, "after-rollback")
	// delivered подтверждает возобновление доставки через тот же сокет, когда допуск восстановился.
	delivered := awaitTwitchSignal(t, ctx, sink.messages)
	if delivered.ProviderMessageID != "after-rollback" || len(sink.gaps) != 0 || len(fixture.calls) != 0 {
		t.Fatal("Временное ограничение допуска прервало действующий Twitch-чат")
	}
	cancel()
	// closed и stop ограничивают доказательство физического закрытия после действительной отмены.
	closed, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	awaitTwitchCancellation(t, closed, finished)
	awaitTwitchSignal(t, closed, peer.closed)
}

// awaitTwitchCancellation ждёт завершения Read после остановки его отдельного контекста.
func awaitTwitchCancellation(t *testing.T, ctx context.Context, finished <-chan error) {
	t.Helper()
	// err подтверждает, что после отмены не было перехода к новой подписке.
	if err := awaitTwitchSignal(t, ctx, finished); !errors.Is(err, context.Canceled) {
		t.Fatalf("После отмены подписка вернула %v", err)
	}
}

// delayedConfirmationSink удерживает CONNECTED, пока тест имитирует временную блокировку аккаунта.
type delayedConfirmationSink struct {
	// socketTestSink принимает обычные события после снятия ограничения.
	*socketTestSink
	// waiting подтверждает, что Helix уже ответил и началось ожидание допуска.
	waiting chan struct{}
	// admitted разрешает публикацию подтверждения действующей подписки.
	admitted chan struct{}
}

// Status ждёт допуска перед первым CONNECTED. При отмене ctx передаёт результат отмены.
func (s *delayedConfirmationSink) Status(ctx context.Context, token core.ChatWorkerToken, state core.ChatSourceState, reason core.ChatReason) error {
	if state == core.ChatConnected {
		close(s.waiting)
		select {
		case <-s.admitted:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.socketTestSink.Status(ctx, token, state, reason)
}

// TestEventSubLateConnectedDoesNotDiscardKeepalive удерживает подтверждение в Manager дольше
// одного окна keepalive. После снятия ограничения подписка доставляет уже принятые сообщения.
func TestEventSubLateConnectedDoesNotDiscardKeepalive(t *testing.T) {
	// ctx ограничивает тест с задержкой подтверждения. cancel останавливает сокет.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// fixture создаёт действительную подписку на локальном TLS-сервере.
	fixture := newSocketTestFixture(t, ctx, core.DefaultChatLimits())
	// overlaps считает временные сокеты, которых эта подписка создавать не должна.
	var overlaps atomic.Int32
	// request передаёт проверенные полномочия и токен текущего чтения.
	request := socketTestReadRequest(&overlaps)
	// sink управляет допуском подтверждения, не задерживая приём кадров в сокете.
	sink := &delayedConfirmationSink{
		socketTestSink: &socketTestSink{
			token: request.Token, statuses: make(chan readerTestStatus, 4),
			messages: make(chan core.ChatIncomingMessage, 2), gaps: make(chan core.ChatReason, 2),
		},
		waiting: make(chan struct{}), admitted: make(chan struct{}),
	}
	// done сообщает результат после освобождения физического соединения.
	done := make(chan error, 1)
	// Функция выполняет реальный адаптер, пока тест моделирует задержку Manager.
	go func() { done <- fixture.adapter.Read(ctx, request, sink) }()
	// peer обслуживает исходную сессию с коротким окном keepalive.
	peer := awaitTwitchSignal(t, ctx, fixture.peers)
	peer.welcome(t, ctx, "socket-one", 1)
	// call завершает сетевую часть подписки до приостановки её публичного состояния.
	call := awaitTwitchSignal(t, ctx, fixture.calls)
	sink.allowConnected.Store(true)
	call.acknowledge("socket-one", "subscription-one")
	awaitTwitchSignal(t, ctx, sink.waiting)

	// heartbeat поддерживает действующую подписку через границу первоначального окна.
	heartbeat := time.NewTicker(300 * time.Millisecond)
	defer heartbeat.Stop()
	// sequence формирует разные идентификаторы событий на том же сокете.
	for sequence := range 4 {
		awaitTwitchSignal(t, ctx, heartbeat.C)
		peer.send(t, ctx, fmt.Sprintf(`{"metadata":{"message_id":"early-keepalive-%d","message_type":"session_keepalive","message_timestamp":%q},"payload":{}}`, sequence, testTimestamp))
	}
	heartbeat.Stop()
	close(sink.admitted)
	sink.awaitState(t, ctx, core.ChatConnected)
	peer.sendChat(t, ctx, "socket-one", "subscription-one", testBroadcasterID, "after-delayed-confirmation")
	// message подтверждает чтение с прежней подписки после ожидания допуска.
	message := awaitTwitchSignal(t, ctx, sink.messages)
	if message.ProviderMessageID != "after-delayed-confirmation" || len(sink.gaps) != 0 || len(fixture.calls) != 0 {
		t.Fatal("Отложенное подтверждение прервало живую подписку")
	}
	cancel()
	// closed и stop ограничивают локальное завершение после штатной отмены.
	closed, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	awaitTwitchCancellation(t, closed, done)
	awaitTwitchSignal(t, closed, peer.closed)
}

// TestEventSubAppliesRateLimitBeforeWebSocketUpgrade проверяет отказ рукопожатия,
// когда точный срок из HTTP должен дойти до следующей физической попытки.
func TestEventSubAppliesRateLimitBeforeWebSocketUpgrade(t *testing.T) {
	// adapter выполняет официальный запрос WebSocket, err сообщает об ошибке подготовки.
	adapter, err := New(core.DefaultChatLimits())
	if err != nil {
		t.Fatal(err)
	}
	adapter.now = testTime
	// Функция возвращает rate limit до Upgrade, поэтому адаптер не начинает чтение сокета.
	adapter.httpClient.Transport = oauthRoundTrip(func(request *http.Request) (*http.Response, error) {
		// response несёт относительный срок и более строгий абсолютный срок Twitch.
		response := oauthResponse(http.StatusTooManyRequests, "")
		response.Header.Set("Ratelimit-Reset", fmt.Sprint(testTime().Add(32*time.Second).Unix()))
		if request.Header.Get("Upgrade") != "websocket" || request.URL.Host != "eventsub.wss.twitch.tv" {
			t.Error("Рукопожатие направлено не на исходный адрес EventSub")
		}
		return response, nil
	})
	// conn остаётся пустым, failure содержит указание отложить следующий запрос.
	conn, _, failure := adapter.openEventSub(t.Context(), adapter.socketURL)
	if conn != nil || failure.state != core.ChatRetrying || failure.reason != core.ChatReasonQuota ||
		failure.retryAfter < 31*time.Second || failure.retryAfter > 32*time.Second {
		t.Fatalf("HTTP-ограничение рукопожатия обработано неверно: %+v", failure)
	}
}
