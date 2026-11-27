package api_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// acceptanceChatFeed подключает единственный синтетический читатель к наблюдаемому выходу.
type acceptanceChatFeed struct {
	// ready доставляет точный токен Manager вместе с портом, который принимает входящие сообщения.
	ready chan acceptanceChatReader
}

// acceptanceChatReader сохраняет порт и его токен до закрытия выбранного чтения.
type acceptanceChatReader struct {
	// sink принимает сообщения и пробелы синтетической площадки.
	sink core.ChatAdapterSink
	// token связывает callbacks с действующим читателем конкретной Session.
	token core.ChatWorkerToken
}

// Provider регистрирует фикстуру на месте чтения YouTube.
func (*acceptanceChatFeed) Provider() core.ChatProvider { return core.ChatYouTube }

// Discover разрешает постоянный ChannelHint источника в одну синтетическую беседу.
func (*acceptanceChatFeed) Discover(context.Context, core.ChatSource, core.ChatCredentials) (core.ChatDiscovery, error) {
	return core.ChatDiscovery{ChannelID: "synthetic-live-chat", State: core.ChatConnected}, nil
}

// Read подтверждает подписку, выдаёт тесту токен request и ждёт отмену владельца.
func (a *acceptanceChatFeed) Read(ctx context.Context, request core.ChatReadRequest, sink core.ChatAdapterSink) error {
	if err := sink.Status(ctx, request.Token, core.ChatConnected, core.ChatReasonNone); err != nil {
		// err указывает, что читатель уже потерял право на публикацию.
		return err
	}
	select {
	case a.ready <- acceptanceChatReader{sink: sink, token: request.Token}:
	case <-ctx.Done():
		return ctx.Err()
	}
	<-ctx.Done()
	return ctx.Err()
}

// send передаёт исходный идентификатор id и Unicode-текст text через реальную границу адаптера.
func (a acceptanceChatReader) send(t *testing.T, ctx context.Context, id, text string) {
	t.Helper()
	// incoming содержит наблюдаемые исходные атрибуты, отправитель не назначает порядок и Session.
	incoming := core.ChatIncomingMessage{
		Provider: core.ChatYouTube, ChannelID: "synthetic-live-chat", ProviderMessageID: id,
		Kind: core.ChatMessageText, Text: text, Author: core.ChatAuthor{ID: "author", DisplayName: "Автор 🙂"},
	}
	if err := a.sink.Message(ctx, a.token, incoming); err != nil {
		// err сообщает об отказе приёма сообщения для ожидаемого действующего читателя.
		t.Fatal(err)
	}
}

// acceptanceChatSeedOutput добавляет к существующей Session медиавыход и квитанцию его запуска.
// Профиль и артефакт служат только валидным снимком, без открытия файлов и запуска FFmpeg.
func acceptanceChatSeedOutput(t *testing.T, fixture *apiAcceptanceFixture, sessionID core.ID, stream core.Stream, profile core.TranscodeProfile, group core.TranscodeGroup, destination core.Destination) {
	t.Helper()
	// ctx ограничивает операции при завершении сценария.
	ctx := t.Context()
	// asset даёт снимку обязательный идентификатор заглушки, err сообщает об отказе фикстуры.
	asset, err := fixture.service.CreateAsset(ctx, "Заглушка тестового чата")
	if err != nil {
		t.Fatal(err)
	}
	// key и canonical закрепляют идентичность профиля тестового выхода.
	key, canonical, err := core.ProfileIdentity(profile.Profile)
	if err != nil {
		t.Fatal(err)
	}
	// now содержит представимое в SQLite время вставки.
	now := time.Now().UTC().Truncate(time.Microsecond)
	// fallback уточняет действующую политику только в тестовом снимке выхода.
	fallback := stream.Fallback
	fallback.AssetID = asset.ID
	fallback.Watchdog = core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second,
		RecoveryWindow: time.Second, KeyframeTimeout: time.Second}
	// fence связывает выход с точной сохранённой попыткой в запущенной Session.
	fence := core.ExecutionFence{CommandID: "chat-output-start", SessionID: sessionID, Incarnation: fixture.store.Incarnation(),
		Lifecycle: 1, OutputID: "chat-output", OutputGeneration: 1, Attempt: 1}
	// output представляет реально выданный выход с синтетическим медиаснимком.
	output := core.SessionOutput{
		Record:    core.Record{ID: fence.OutputID, Revision: 1, CreatedAt: now, UpdatedAt: now},
		SessionID: sessionID, Generation: 1, Attached: true, DesiredRunning: true, Attempt: 1, Issued: true,
		Snapshot: core.OutputSnapshot{Destination: destination, Profile: profile, Group: group, Fallback: fallback,
			Artifact: core.ArtifactManifest{GenerationID: "chat-generation", AssetID: asset.ID, VariantID: "chat-variant",
				Profile: profile.Profile, ProfileKey: key, CanonicalProfile: canonical, VerifiedFormat: profile.Profile.Format,
				SourceHash: "synthetic-source", GeneratorVersion: core.CurrentFallbackGeneratorVersion,
				ManagedPath: "variants/synthetic.mp4", SizeBytes: 512, ContentHash: "synthetic-content"}},
	}
	// identity и digest дают согласованные тело и отпечаток синтетической команды.
	identity := []byte("chat-output-start")
	digest := sha256.Sum256(identity)
	// receipt подтверждает выданную попытку для открытия оперативного наблюдателя.
	receipt := core.CommandReceipt{
		Record:    core.Record{ID: fence.CommandID, Revision: 1, CreatedAt: now, UpdatedAt: now},
		RequestID: string(fence.CommandID), Kind: core.CommandStartOutput, TargetID: output.ID,
		Identity: identity, Fingerprint: hex.EncodeToString(digest[:]), SessionID: sessionID,
		OutputID: output.ID, Incarnation: fence.Incarnation, Lifecycle: 1, OutputGeneration: 1,
		Attempt: 1, Status: core.CommandSucceeded,
	}
	if err = fixture.store.Write(ctx, func(r core.Repositories) error {
		// r сохраняет выход и успешный запуск в одной транзакции.
		if err := r.Runtime.InsertOutput(ctx, output); err != nil {
			// err останавливает транзакцию до вставки квитанции при отказе записи выхода.
			return err
		}
		return r.Runtime.InsertReceipt(ctx, receipt)
	}); err != nil {
		t.Fatal(err)
	}
	// observer представляет выход до первого подтверждённого PUBLISHING.
	observer, err := fixture.service.OpenOutputObserver(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Освобождает точного наблюдателя после закрытия локальных HTTP-подписок.
		// cleanup ограничивает отзыв при раннем отказе, cancel освобождает таймер.
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		// err сообщает о незавершённом отзыве этого медианаблюдателя.
		if err := observer.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	if err = observer.Observe(ctx, core.OutputObservation{State: core.OutputPublishing}); err != nil {
		t.Fatal(err)
	}
}

// acceptanceChatEventFrames читает события из TCP-ответа и сверяет SSE-обрамление с JSON.
type acceptanceChatEventFrames struct {
	// reader читает тело, ограниченное временем жизни HTTP-клиента.
	reader *bufio.Reader
}

// next пропускает комментарии до следующего события и возвращает фактические данные и байты кадра.
func (s acceptanceChatEventFrames) next(t *testing.T, expected string) (map[string]any, string) {
	t.Helper()
	// raw собирает весь кадр до пустого разделителя, включая переводы строк.
	var raw strings.Builder
	for {
		// line содержит одну строку SSE, err сообщает о разрыве либо таймауте чтения.
		line, err := s.reader.ReadString('\n')
		if err != nil {
			t.Fatalf("Во время ожидания %s поток завершился: %v", expected, err)
		}
		if line == "\n" {
			if raw.Len() == 0 || strings.HasPrefix(raw.String(), ":") {
				raw.Reset()
				continue
			}
			raw.WriteString(line)
			break
		}
		raw.WriteString(line)
	}
	// lines выделяет id, имя события, JSON и пустой разделитель кадра.
	lines := strings.Split(raw.String(), "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], "id: ") || lines[1] != "event: "+expected ||
		!strings.HasPrefix(lines[2], "data: ") || lines[3] != "" || lines[4] != "" {
		t.Fatalf("Некорректный кадр %s: %q", expected, raw.String())
	}
	// actual принимает JSON с исходными целочисленными лексемами.
	var actual map[string]any
	if err := apiAcceptanceDecodeJSON([]byte(strings.TrimPrefix(lines[2], "data: ")), &actual); err != nil {
		// err сообщает об отказе разобрать единственную JSON-нагрузку кадра.
		t.Fatal(err)
	}
	// id содержит непрозрачную позицию, которая должна совпадать с JSON cursor.
	id := strings.TrimPrefix(lines[0], "id: ")
	if id == "" || actual["cursor"] != id || actual["kind"] != expected || strings.Contains(raw.String(), acceptancePrivatePrefix) {
		t.Fatalf("Неверная публичная позиция или содержимое события %s", expected)
	}
	return actual, raw.String()
}

// requireEnd подтверждает закрытие после терминального кадра или resync без повторного события.
func (s acceptanceChatEventFrames) requireEnd(t *testing.T) {
	t.Helper()
	// err проверяет немедленный конец тела после целого последнего кадра.
	if _, err := s.reader.ReadByte(); err != io.EOF {
		t.Fatalf("После терминального кадра ожидался EOF, получено %v", err)
	}
}

// acceptanceChatHTTP передаёт GET с исходными экземплярами Last-Event-ID через реальный TCP.
func acceptanceChatHTTP(t *testing.T, ctx context.Context, server *httptest.Server, path string, lastID []string, slow bool) *http.Response {
	t.Helper()
	// req содержит запрос к принадлежащему тесту серверу и общую отмену сценария.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// value задаёт один экземпляр заголовка позиции, включая проверки повторяющегося заголовка.
	for _, value := range lastID {
		req.Header.Add("Last-Event-ID", value)
	}
	if slow {
		req.Header.Set("X-Test-Slow", "true")
	}
	// response содержит сетевую запись заголовков и потоковое тело, err сообщает об отказе транспорта.
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Освобождает HTTP-тело перед завершением локального сервера.
		_ = response.Body.Close()
	})
	return response
}

// acceptanceOpenChatFrames проверяет тип ответа и возвращает читатель существующего SSE.
func acceptanceOpenChatFrames(t *testing.T, response *http.Response) acceptanceChatEventFrames {
	t.Helper()
	// mediaType содержит MIME-тип без параметров, err сообщает об ошибке заголовка Content-Type.
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != 200 || err != nil || mediaType != "text/event-stream" ||
		response.Header.Get("Cache-Control") != "no-cache" || response.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("Неверный HTTP-ответ SSE: %d %v", response.StatusCode, response.Header)
	}
	return acceptanceChatEventFrames{reader: bufio.NewReader(response.Body)}
}

// acceptanceSlowChatWriter удерживает первый кадр одного клиента до снятия блокировки тестом.
type acceptanceSlowChatWriter struct {
	// ResponseWriter передаёт фактические кадры сетевому HTTP-серверу.
	http.ResponseWriter
	// ctx прекращает удержание при закрытии тестового запроса.
	ctx context.Context
	// blocked подтверждает, что обработчик уже дошёл до записи сообщения.
	blocked chan<- struct{}
	// resume разрешает доставку удержанного кадра и наблюдение resync.
	resume <-chan struct{}
	// held отличает первую запись сообщения от всех следующих кадров.
	held bool
}

// Unwrap предоставляет ResponseController исходный транспорт со сроками записи и Flush.
func (w *acceptanceSlowChatWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Write удерживает первый кадр сообщения и затем посылает исходный frame без изменений.
func (w *acceptanceSlowChatWriter) Write(frame []byte) (int, error) {
	if !w.held && strings.Contains(string(frame), "\nevent: chat.message\n") {
		w.held = true
		close(w.blocked)
		select {
		case <-w.resume:
		case <-w.ctx.Done():
			return 0, w.ctx.Err()
		}
	}
	return w.ResponseWriter.Write(frame)
}

// TestChatSSEReplaysAndEvictsSlowClientOverTCP проводит настоящие GET по локальному TCP.
// Оракул различает ошибку оформления позиции, replay после точного курсора, пересинхронизацию
// при потерянном хвосте, её однократность и сохранение быстрого подписчика при отставании другого.
func TestChatSSEReplaysAndEvictsSlowClientOverTCP(t *testing.T) {
	// ctx ограничивает все запросы и отменяет удерживаемый транспорт при выходе.
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	// fixture содержит отдельную мигрированную SQLite.
	fixture := newAPIAcceptanceFixture(t, nil)
	// stream, profile, group и destination связывают синтетический источник с настоящим наблюдаемым выходом.
	stream, profile, group, destination := apiAcceptanceSeedConfiguration(t, fixture)
	// account хранит синтетический ключ, err сообщает об отказе записи до открытия ленты.
	account, err := fixture.service.CreateChatAccount(ctx, core.ChatAccount{Provider: core.ChatYouTube},
		core.ChatCredentialBundle{YouTube: &core.YouTubeChatCredentials{APIKey: acceptancePrivatePrefix + "feed-key"}}, "feed-initial")
	if err != nil {
		t.Fatal(err)
	}
	// source задаёт ID, которым сервер атрибутирует фактические сообщения адаптера.
	source, err := fixture.service.CreateChatSource(ctx, core.ChatSource{DestinationID: destination.ID, AccountID: account.Account.ID,
		Provider: core.ChatYouTube, ChannelHint: "UCsynthetic", Enabled: true,
		Options: core.ChatOptions{Version: 1, YouTube: &core.YouTubeChatOptions{}}})
	if err != nil {
		t.Fatal(err)
	}
	// limits задаёт раздельные маленькие очереди и достаточный хвост для краткого replay.
	limits := core.DefaultChatLimits()
	limits.SSEQueue.Count, limits.Replay.Count, limits.FlushMessages = 2, 4, 1
	limits.SSEClients, limits.SSEClientsPerStream = 2, 2
	limits.SSEWriteTimeout = 8 * time.Second
	// feed выделяет отдельного синтетического читателя после действительного PUBLISHING.
	feed := &acceptanceChatFeed{ready: make(chan acceptanceChatReader, 1)}
	// handler содержит стандартный middleware, Manager и реальную маршрутизацию Huma.
	handler, _ := newChatAcceptanceHandler(t, fixture, limits, []core.ChatAdapterPort{feed})
	// sessionID и observer определяют текущую подтверждённую Session.
	sessionID, observer := seedChatStreamSelfSession(t, fixture, stream)
	acceptanceChatSeedOutput(t, fixture, sessionID, stream, profile, group, destination)
	// reader сохраняет точный токен после открытия платформенного чтения.
	var reader acceptanceChatReader
	select {
	case reader = <-feed.ready:
	case <-ctx.Done():
		t.Fatal("Адаптер не получил действующий выход", ctx.Err())
	}

	// blocked сообщает о физическом входе в запись медленного клиента, resume возобновляет её.
	blocked, resume := make(chan struct{}), make(chan struct{})
	// server открывает принадлежащий тесту loopback и даёт одному соединению управляемую задержку записи.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// w передаёт кадры в TCP, r отмечает только одну намеренно медленную подписку.
		if r.Header.Get("X-Test-Slow") == "true" {
			w = &acceptanceSlowChatWriter{ResponseWriter: w, ctx: r.Context(), blocked: blocked, resume: resume}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	// root адресует снимок и SSE того же Stream в публичном HTTP API.
	root := api.Prefix + "/chat/streams/" + string(stream.ID)
	// snapshot содержит согласованную исходную границу активной Session после подтверждения читателя.
	snapshot := acceptanceChatSnapshot(t, ctx, server, root)
	// cursor — точная позиция снимка, передаваемая обратно без преобразования.
	cursor, _ := snapshot["cursor"].(string)
	if cursor == "" || !reflect.DeepEqual(snapshot["session"], map[string]any{"session_id": string(sessionID)}) ||
		!reflect.DeepEqual(snapshot["messages"], []any{}) {
		t.Fatalf("Неверная активная граница снимка: %v", snapshot)
	}
	// eventPath адресует единственный публичный ресурс этого потока.
	eventPath := root + "/events"
	assertAcceptanceChatBadCursor(t, ctx, server, eventPath+"?cursor="+url.QueryEscape(cursor), []string{"wrong"}, "sse_cursor_conflict")
	assertAcceptanceChatBadCursor(t, ctx, server, eventPath+"?cursor="+url.QueryEscape(cursor)+"&cursor="+url.QueryEscape(cursor), nil, "invalid_cursor")
	assertAcceptanceChatBadCursor(t, ctx, server, eventPath, []string{cursor, cursor}, "invalid_cursor")
	assertAcceptanceChatBadCursor(t, ctx, server, eventPath+"?cursor="+strings.Repeat("x", int(limits.CursorBytes+1)), nil, "invalid_cursor")
	assertAcceptanceChatResync(t, ctx, server, eventPath+"?cursor=bad-signature", nil)

	// fastResponse открывает подключение с совпавшими query и Last-Event-ID.
	fastResponse := acceptanceChatHTTP(t, ctx, server, eventPath+"?cursor="+url.QueryEscape(cursor), []string{cursor}, false)
	// slowResponse открывает второй поток, чья первая доставка будет удерживаться.
	slowResponse := acceptanceChatHTTP(t, ctx, server, eventPath, []string{cursor}, true)
	// fast читает ленту на соединении с немедленной записью.
	fast := acceptanceOpenChatFrames(t, fastResponse)
	// slow читает ту же ленту на соединении с задержанной записью.
	slow := acceptanceOpenChatFrames(t, slowResponse)
	// sample включает Unicode, угловые скобки и перевод строки для проверки JSON-экранирования.
	sample := "Зритель 🛰️ <b>текст</b>\nследующая строка"
	reader.send(t, ctx, "message-1", sample)
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("Медленная запись первого сообщения не началась", ctx.Err())
	}
	// first и firstWire сохраняют первый id и полный кадр для точного сравнения после replay.
	first, firstWire := fast.next(t, "chat.message")
	assertAcceptanceChatMessage(t, first, sessionID, source.ID, "message-1", sample, "1")
	// last остаётся точной публичной позицией последнего сообщения быстрого подписчика.
	last := first["cursor"].(string)
	// penultimate хранит предпоследнюю позицию, lastWire — байты сообщения после неё.
	penultimate, lastWire := "", ""
	// number задаёт очередной ожидаемый порядковый номер. Каждый кадр читается до следующей отправки.
	for number := 2; number <= 6; number++ {
		// id отличает сообщение и одновременно помогает проверить порядок replay.
		id := fmt.Sprintf("message-%d", number)
		reader.send(t, ctx, id, id)
		// event и wire содержат данные и байты очередного живого кадра.
		event, wire := fast.next(t, "chat.message")
		assertAcceptanceChatMessage(t, event, sessionID, source.ID, id, id, fmt.Sprint(number))
		penultimate, last, lastWire = last, event["cursor"].(string), wire
	}

	close(resume)
	// slowWire содержит первое сообщение, уже переданное транспортом при исчерпании очереди.
	_, slowWire := slow.next(t, "chat.message")
	if slowWire != firstWire {
		t.Fatal("Медленный и быстрый клиент получили разные публичные кадры одного сообщения")
	}
	// resync сообщает потерю локального хвоста только для отставшего клиента.
	resync, _ := slow.next(t, "resync_required")
	if resync["session_id"] != string(sessionID) || resync["reason"] != "limit" || resync["message"] != nil {
		t.Fatalf("Медленный клиент не получил корректный запрос нового снимка: %v", resync)
	}
	slow.requireEnd(t)

	assertAcceptanceChatResync(t, ctx, server, eventPath+"?cursor="+url.QueryEscape(cursor), nil)
	// replayResponse возобновляет TCP-подписку по заголовку после предпоследнего id.
	replayResponse := acceptanceChatHTTP(t, ctx, server, eventPath, []string{penultimate}, false)
	// replay читает кадры возобновлённого соединения.
	replay := acceptanceOpenChatFrames(t, replayResponse)
	// repeated содержит первую доставленную запись после указанной позиции, replayWire — её точные байты.
	repeated, replayWire := replay.next(t, "chat.message")
	if repeated["cursor"] != last || replayWire != lastWire {
		t.Fatalf("Replay повторил неверный кадр после позиции %s", penultimate)
	}
	if err = replayResponse.Body.Close(); err != nil {
		t.Fatal(err)
	}
	// latest подтверждает согласование новой границы с последним событием и полной историей.
	latest := acceptanceChatSnapshot(t, ctx, server, root)
	// tail содержит JSON-историю без служебных событий и токенов.
	tail, _ := latest["messages"].([]any)
	if latest["cursor"] != last || len(tail) != 6 || !reflect.DeepEqual(tail[0], first["message"]) ||
		!reflect.DeepEqual(tail[len(tail)-1], repeated["message"]) {
		t.Fatal("Снимок и SSE расходятся в общей публичной истории или границе")
	}

	if err = reader.sink.Gap(ctx, reader.token, core.ChatReasonUpstreamGap); err != nil {
		t.Fatal(err)
	}
	// gap относится к внешней потере, которую нельзя представлять как сообщение с вымышленным seq.
	gap, _ := fast.next(t, "chat.gap")
	if gap["reason"] != "upstream_gap" || gap["lost"] != json.Number("0") || gap["message"] != nil {
		t.Fatalf("Неверное представление upstream-пробела: %v", gap)
	}
	if err = observer.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// terminal подтверждает закрытие той же Session после последней применимой записи.
	terminal, _ := fast.next(t, "chat.session_closed")
	if terminal["session_id"] != string(sessionID) || terminal["message"] != nil {
		t.Fatalf("Закрытие Session потеряло её идентичность: %v", terminal)
	}
	fast.requireEnd(t)
	assertAcceptanceChatResync(t, ctx, server, eventPath, []string{last})

	// waiting проверяет пустую ленту между завершённым и новым запусками одного Stream.
	waiting := acceptanceChatSnapshot(t, ctx, server, root)
	// waitingCursor связывает фактическое TCP-ожидание с предсессионным снимком.
	waitingCursor, _ := waiting["cursor"].(string)
	if waitingCursor == "" || waiting["session"] != nil || !reflect.DeepEqual(waiting["messages"], []any{}) {
		t.Fatalf("Предыдущая лента сохранилась между запусками: %v", waiting)
	}
	// waitingFrames остаётся на позиции без Session до подтверждения следующего запуска.
	waitingFrames := acceptanceOpenChatFrames(t, acceptanceChatHTTP(t, ctx, server, eventPath, []string{waitingCursor}, false))
	acceptanceChatNextSession(t, fixture, sessionID)
	waitingFrames.next(t, "resync_required")
	waitingFrames.requireEnd(t)
	assertAcceptanceChatResync(t, ctx, server, eventPath, []string{last})
	// next содержит свежую эпоху того же Stream, без сообщений предыдущей Session.
	next := acceptanceChatSnapshot(t, ctx, server, root)
	if !reflect.DeepEqual(next["session"], map[string]any{"session_id": "chat-next-session"}) ||
		!reflect.DeepEqual(next["messages"], []any{}) || next["cursor"] == waitingCursor || next["cursor"] == last {
		t.Fatalf("Следующая Session не получила новый пустой снимок: %v", next)
	}
}

// acceptanceChatNextSession сохраняет завершение previous и следующий запуск того же потока.
// Для новой ленты регистрируется точный наблюдатель без запуска медиапроцессов.
func acceptanceChatNextSession(t *testing.T, fixture *apiAcceptanceFixture, previous core.ID) {
	t.Helper()
	// ctx прекращает операции временной SQLite при окончании теста.
	ctx := t.Context()
	// now задаёт представимые в SQLite времена завершения и нового старта.
	now := time.Now().UTC().Truncate(time.Microsecond)
	// fence адресует новую попытку запуска на том же открытии Store.
	fence := core.ExecutionFence{CommandID: "chat-next-start", SessionID: "chat-next-session",
		Incarnation: fixture.store.Incarnation(), Lifecycle: 1, Attempt: 1}
	// identity содержит тело синтетической команды запуска.
	identity := []byte("chat-next-start")
	// digest содержит соответствующий отпечаток команды для сохранённой квитанции.
	digest := sha256.Sum256(identity)
	if err := fixture.store.Write(ctx, func(r core.Repositories) error {
		// r завершает предыдущую запись и сохраняет новый выданный запуск вместе с квитанцией.
		// old хранит предыдущий запуск и исходный снимок Stream, err сообщает об отказе записи.
		old, err := r.Runtime.Session(ctx, previous)
		if err != nil {
			return err
		}
		old.State, old.EndedAt, old.UpdatedAt = core.SessionStopped, &now, now
		old.Revision++
		if err = r.Runtime.UpdateSession(ctx, old, old.Revision-1); err != nil {
			return err
		}
		// current содержит новую RUNNING Session с тем же сохранённым Stream.
		current := core.Session{Record: core.Record{ID: fence.SessionID, Revision: 1, CreatedAt: now, UpdatedAt: now},
			StreamID: old.StreamID, Incarnation: fence.Incarnation, Lifecycle: 1,
			State: core.SessionRunning, Issued: true, Snapshot: old.Snapshot}
		if err = r.Runtime.InsertSession(ctx, current); err != nil {
			return err
		}
		return r.Runtime.InsertReceipt(ctx, core.CommandReceipt{
			Record:    core.Record{ID: fence.CommandID, Revision: 1, CreatedAt: now, UpdatedAt: now},
			RequestID: string(fence.CommandID), Kind: core.CommandStartSession, TargetID: old.StreamID,
			Identity: identity, Fingerprint: hex.EncodeToString(digest[:]), SessionID: fence.SessionID,
			Incarnation: fence.Incarnation, Lifecycle: 1, Attempt: 1, Status: core.CommandSucceeded,
		})
	}); err != nil {
		// err сообщает об отказе одной из записей транзакции смены Session.
		t.Fatal(err)
	}
	// observer будит подписку после подтверждения нового запуска, err сообщает об отказе его регистрации.
	observer, err := fixture.service.OpenSessionObserver(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Отзывает временного наблюдателя перед закрытием SQLite.
		// cleanup и cancel ограничивают ожидание освобождения наблюдателя.
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		// err сообщает об отказе отозвать наблюдателя последней активной Session.
		if err := observer.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	observer.NotifyChatRunning()
}

// acceptanceChatSnapshot читает публичный снимок root по TCP и сохраняет точность JSON-чисел.
func acceptanceChatSnapshot(t *testing.T, ctx context.Context, server *httptest.Server, root string) map[string]any {
	t.Helper()
	// response содержит снимок, который сервер действительно записал в TCP.
	response := acceptanceChatHTTP(t, ctx, server, root+"/snapshot", nil, false)
	// data сохраняет исходные JSON-байты для проверки закрытых значений и строгого разбора.
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || strings.Contains(string(data), acceptancePrivatePrefix) {
		t.Fatalf("Не удалось прочитать публичный снимок: %d %v", response.StatusCode, err)
	}
	// snapshot принимает поля HTTP-снимка без зависимости от производственных DTO.
	var snapshot map[string]any
	if err = apiAcceptanceDecodeJSON(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// assertAcceptanceChatBadCursor различает ошибку HTTP-оформления позиции и успешный SSE resync.
func assertAcceptanceChatBadCursor(t *testing.T, ctx context.Context, server *httptest.Server, path string, ids []string, code string) {
	t.Helper()
	// response содержит Problem Details до попытки открыть поток.
	response := acceptanceChatHTTP(t, ctx, server, path, ids, false)
	// data содержит отклонение текущего транспорта, err сообщает об отказе чтения тела.
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	// problem принимает безопасные поля ошибки, не выдавая курсор или реквизиты площадки.
	var problem map[string]any
	if err = apiAcceptanceDecodeJSON(data, &problem); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 400 || problem["status"] != json.Number("400") || problem["code"] != code ||
		!strings.HasPrefix(response.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("HTTP-оформление курсора вернуло %d %s", response.StatusCode, data)
	}
}

// assertAcceptanceChatResync подтверждает, что повреждённая или недоступная позиция
// получает один управляющий кадр и закрывает прежнюю подписку без служебного сообщения.
func assertAcceptanceChatResync(t *testing.T, ctx context.Context, server *httptest.Server, path string, ids []string) {
	t.Helper()
	// frames читает настоящую подписку с позицией, для которой сообщения уже не выдаются.
	frames := acceptanceOpenChatFrames(t, acceptanceChatHTTP(t, ctx, server, path, ids, false))
	// event содержит единственный запрос нового снимка.
	event, _ := frames.next(t, "resync_required")
	if event["message"] != nil {
		t.Fatal("Пересинхронизация передала служебное сообщение в ленту")
	}
	frames.requireEnd(t)
}

// assertAcceptanceChatMessage сверяет идентичность и точный JSON сообщения с переданным входом.
func assertAcceptanceChatMessage(t *testing.T, event map[string]any, session, source core.ID, inputID, text, seq string) {
	t.Helper()
	// message содержит единственную публичную запись кадра, ok исключает массив и служебный кадр.
	message, ok := event["message"].(map[string]any)
	if !ok || event["session_id"] != string(session) || message["session_id"] != string(session) ||
		message["chat_source_id"] != string(source) || message["provider_message_id"] != inputID ||
		message["channel_id"] != "synthetic-live-chat" || message["text"] != text ||
		message["seq"] != json.Number(seq) || message["kind"] != "message" || message["provider"] != "youtube" ||
		!reflect.DeepEqual(message["author"], map[string]any{"id": "author", "display_name": "Автор 🙂"}) ||
		!reflect.DeepEqual(message["parts"], []any{}) || !reflect.DeepEqual(message["badges"], []any{}) {
		t.Fatalf("Неверное публичное сообщение %s: %v", inputID, message)
	}
}
