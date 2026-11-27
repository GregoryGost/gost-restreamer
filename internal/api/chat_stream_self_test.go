package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/chat"
	"github.com/GregoryGost/gost-restreamer/internal/chatwire"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"go.yaml.in/yaml/v3"
)

// TestChatStreamOpenAPI проверяет схемы реальных отказов, нижние границы целых и каталог событий.
func TestChatStreamOpenAPI(t *testing.T) {
	// handler публикует спецификацию тех же зарегистрированных операций, что обслуживают HTTP.
	_, handler, _ := newChatStreamSelfFixture(t, core.DefaultChatLimits())
	// jsonResponse и yamlResponse содержат два официальных представления спецификации.
	jsonResponse := apiAcceptanceRequest(t, handler, "GET", "/openapi.json", "", 200)
	yamlResponse := apiAcceptanceRequest(t, handler, "GET", "/openapi.yaml", "", 200)
	// spec сохраняет точные числовые лексемы, yamlSpec принимает ту же схему из YAML.
	spec := numericJSON(t, jsonResponse.Body.Bytes())
	var yamlSpec map[string]any
	// err обнаруживает ошибку синтаксиса отдельного представления.
	if err := yaml.Unmarshal(yamlResponse.Body.Bytes(), &yamlSpec); err != nil {
		t.Fatal(err)
	}
	// document перебирает независимо разобранные спецификации.
	for _, document := range []map[string]any{spec, yamlSpec} {
		// operation выбирает снимок либо подписку с тем же публичным каталогом ошибок.
		for _, operation := range []string{"snapshot", "events"} {
			// status проверяет ошибочный запрос, отсутствующий поток и временную недоступность.
			for _, status := range []string{"400", "404", "503"} {
				// prefix адресует опубликованную схему ошибки конкретной операции.
				prefix := "paths./api/v1/chat/streams/{streamId}/" + operation + ".get.responses." + status + ".content.application/problem+json.schema"
				if numericAt(t, document, prefix+".$ref") != "#/components/schemas/ProblemDTO" {
					t.Fatalf("%s %s использует неверную схему ошибки", operation, status)
				}
			}
		}
	}
	// prefix адресует реестр публичных моделей, совместных для REST и SSE.
	const prefix = "components.schemas."
	// field адресует положительную ревизию или последовательность и их точный uint64-диапазон.
	for _, field := range []string{"SourceDTO.properties.revision", "MessageDTO.properties.seq"} {
		numericWant(t, spec, prefix+field+".minimum", "1")
		numericWant(t, spec, prefix+field+".maximum", "18446744073709551615")
	}
	// reason включает пустую безопасную причину, поддерживаемую событиями и оперативными статусами.
	reason := numericAt(t, spec, prefix+"StatusDTO.properties.reason.enum").([]any)
	if len(reason) != 12 || reason[0] != "" {
		t.Fatalf("Каталог причин статуса неполон: %v", reason)
	}
	// kinds содержит закрытый перечень кадров, воспринимаемый клиентом.
	kinds := numericAt(t, spec, prefix+"EventDTO.properties.kind.enum").([]any)
	// expectedKinds фиксирует совместимый список SSE, включая очистку, без привязки к порядку в схеме.
	expectedKinds := []any{"chat.message", "chat.source_status", "chat.gap", "resync_required", "chat.session_closed", "chat.cleared"}
	if len(kinds) != len(expectedKinds) {
		t.Fatalf("Каталог событий неполон: %v", kinds)
	}
	// expectedKind обозначает вид события, который обязан встречаться в схеме ровно один раз.
	for _, expectedKind := range expectedKinds {
		// occurrences считает объявления одного значения и обнаруживает его отсутствие или дубликат.
		occurrences := 0
		// kind содержит очередное объявленное значение SSE.
		for _, kind := range kinds {
			if kind == expectedKind {
				occurrences++
			}
		}
		if occurrences != 1 {
			t.Fatalf("Каталог событий содержит %q %d раз: %v", expectedKind, occurrences, kinds)
		}
	}
	// expectedProviders фиксирует каталог реальных площадок и отдельное значение сообщения генератора.
	expectedProviders := []any{"youtube", "twitch", "vk_video_live"}
	if !reflect.DeepEqual(numericAt(t, spec, prefix+"SourceDTO.properties.provider.enum"), expectedProviders) ||
		!reflect.DeepEqual(numericAt(t, spec, prefix+"MessageDTO.properties.provider.enum"), append(expectedProviders, "test")) {
		t.Fatal("Значение test должно быть доступно только сообщениям")
	}
}

// newChatStreamSelfFixture поднимает штатный API и Manager на временной SQLite без внешних адаптеров.
func newChatStreamSelfFixture(t *testing.T, limits core.ChatLimits) (*apiAcceptanceFixture, *api.API, *chat.Manager) {
	t.Helper()
	// fixture владеет временной БД и общим Service для HTTP и наблюдателей чата.
	fixture := newAPIAcceptanceFixture(t, nil)
	// manager обрабатывает только локальные наблюдения, err сообщает об отказе запуска.
	manager, err := chat.New(t.Context(), fixture.service, fixture.store, limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// ctx ограничивает остановку Manager, cancel освобождает таймер после очистки.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// err сообщает об остановке до закрытия временной БД.
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	// handler содержит зарегистрированные маршруты и спецификацию, err сообщает об отказе сборки.
	handler, err := api.New(api.Config{
		Core: fixture.service, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Limits: fixture.limits, Chat: manager, ChatLimits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// ctx ограничивает завершение API, cancel освобождает таймер очистки.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// err сообщает, остались ли обработчики к моменту остановки.
		if err := handler.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return fixture, handler, manager
}

// chatStreamSelfServe запускает HTTP-запрос с наблюдаемым транспортом.
// Возвращаемые cancel и done позволяют проверять физическое освобождение квоты.
func chatStreamSelfServe(t *testing.T, handler *api.API, path string) (*apiAcceptanceStreamWriter, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	// writer записывает кадры и отмечает отправку без срока.
	writer := newAPIAcceptanceStreamWriter()
	// ctx ограничивает время жизни тестового HTTP-запроса, cancel закрывает подключение.
	ctx, cancel := context.WithCancel(t.Context())
	// done фиксирует полный выход из обработчика после очистки Huma.
	done := make(chan struct{})
	go func() {
		// Обслуживает настоящий HTTP-запрос независимо от ожиданий теста.
		defer close(done)
		handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
	}()
	t.Cleanup(func() {
		// Отменяет подписку и ждёт освобождения квоты даже при раннем отказе теста.
		cancel()
		apiAcceptanceAwait(t, done)
	})
	return writer, cancel, done
}

// seedChatStreamSelfSession регистрирует RUNNING и точного оперативного родителя без медиапроцессов.
func seedChatStreamSelfSession(t *testing.T, fixture *apiAcceptanceFixture, stream core.Stream) (core.ID, *core.SessionObserver) {
	t.Helper()
	// now задаёт сохраняемые времена Session и квитанции с точностью SQLite.
	now := time.Now().UTC().Truncate(time.Microsecond)
	// session содержит точный запускаемый поток и назначенного владельца базы.
	session := core.Session{
		Record:   core.Record{ID: "chat-test-session", Revision: 1, CreatedAt: now, UpdatedAt: now},
		StreamID: stream.ID, Incarnation: fixture.store.Incarnation(), Lifecycle: 1,
		State: core.SessionRunning, Issued: true, Snapshot: core.SessionSnapshot{Stream: stream},
	}
	// digest обеспечивает соответствие отпечатка содержимому подтверждённого старта.
	digest := sha256.Sum256([]byte("chat-test-start"))
	// receipt фиксирует успешный запуск для открытия точного наблюдателя.
	receipt := core.CommandReceipt{
		Record:    core.Record{ID: "chat-test-start", Revision: 1, CreatedAt: now, UpdatedAt: now},
		RequestID: "chat-test-start", Kind: core.CommandStartSession, TargetID: stream.ID,
		Identity: []byte("chat-test-start"), Fingerprint: hex.EncodeToString(digest[:]),
		SessionID: session.ID, Incarnation: session.Incarnation, Lifecycle: session.Lifecycle,
		Attempt: 1, Status: core.CommandSucceeded,
	}
	// err сообщает об отказе атомарного размещения Session и её подтверждения.
	if err := fixture.store.Write(t.Context(), func(r core.Repositories) error {
		// Сохраняет точную сессию и подтверждение её запуска в одной транзакции.
		// err сообщает об отказе записи запуска до записи квитанции.
		if err := r.Runtime.InsertSession(t.Context(), session); err != nil {
			return err
		}
		return r.Runtime.InsertReceipt(t.Context(), receipt)
	}); err != nil {
		t.Fatal(err)
	}
	// observer предоставляет действующий оперативный родитель, err сообщает об отказе его регистрации.
	observer, err := fixture.service.OpenSessionObserver(t.Context(), core.ExecutionFence{
		CommandID: receipt.ID, SessionID: session.ID, Incarnation: session.Incarnation, Lifecycle: session.Lifecycle, Attempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Освобождает временного наблюдателя, в том числе при ранней остановке проверки.
		_ = observer.Close(context.Background())
	})
	observer.NotifyChatRunning()
	return session.ID, observer
}

// TestChatStreamSnapshotWaitingAndTerminal проверяет физические сроки HTTP, общую
// квоту до Session, согласованный переход из ожидания и чтение терминала живого SSE.
func TestChatStreamSnapshotWaitingAndTerminal(t *testing.T) {
	// limits оставляет одно место SSE и устанавливает частые контролируемые heartbeat.
	limits := core.DefaultChatLimits()
	limits.SSEClients, limits.SSEClientsPerStream = 1, 1
	limits.SSEHeartbeat = 30 * time.Millisecond
	// fixture, handler и manager разделяют одну временную базу.
	fixture, handler, manager := newChatStreamSelfFixture(t, limits)
	// stream содержит поток без Session, err сообщает об отказе записи.
	stream, err := fixture.service.CreateStream(t.Context(), core.Stream{Name: "Пустой поток", PublicationKey: "local-test-key"})
	if err != nil {
		t.Fatal(err)
	}
	// path задаёт путь текущего потока, snapshotPath и eventPath адресуют его HTTP-операции.
	path := api.Prefix + "/chat/streams/" + string(stream.ID)
	snapshotPath, eventPath := path+"/snapshot", path+"/events"
	// writer наблюдает отдельную физическую запись JSON с Flush.
	writer := newAPIAcceptanceStreamWriter()
	handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, snapshotPath, nil))
	// body содержит фактический ответ, status определяет статус, unsafe отмечает запись без срока.
	body, status, unsafe := writer.snapshot()
	// snapshot принимает согласованный начальный JSON и непрозрачный курсор.
	var snapshot chatwire.SnapshotDTO
	if status != 200 || unsafe || !writer.deadline.IsZero() || json.Unmarshal([]byte(body), &snapshot) != nil ||
		snapshot.StreamID != stream.ID || snapshot.Session != nil || snapshot.Messages == nil || snapshot.Cursor == "" {
		t.Fatalf("неверный snapshot: %d %v %s", status, unsafe, body)
	}
	// waiting получает согласованный cursor и занимает слот до публикации Session.
	waiting, _, waitDone := chatStreamSelfServe(t, handler, eventPath+"?cursor="+string(snapshot.Cursor))
	apiAcceptanceAwait(t, waiting.flushed)
	apiAcceptanceAwaitFrame(t, waiting, ": heartbeat")
	// refused содержит безопасный HTTP-ответ при исчерпании квоты ещё до Session.
	refused := httptest.NewRecorder()
	// request использует транспорт со сроками, чтобы отказ был именно из-за клиентской квоты.
	limited := newAPIAcceptanceStreamWriter()
	request := httptest.NewRequest(http.MethodGet, eventPath, nil)
	handler.ServeHTTP(limited, request)
	body, status, _ = limited.snapshot()
	if status != 503 || !strings.Contains(body, `"code":"sse_client_limit"`) {
		t.Fatalf("ожидание Session обошло квоту: %d %s", status, body)
	}
	handler.ServeHTTP(refused, httptest.NewRequest(http.MethodGet, eventPath+"?cursor=one&cursor=two", nil))
	if refused.Code != 400 {
		t.Fatalf("неоднозначный курсор принят: %d %s", refused.Code, refused.Body)
	}
	// sessionID и observer относятся к реальной границе новой эпохи.
	sessionID, observer := seedChatStreamSelfSession(t, fixture, stream)
	apiAcceptanceAwait(t, waitDone)
	body, status, unsafe = waiting.snapshot()
	if status != 200 || unsafe || strings.Count(body, "event: resync_required\n") != 1 {
		t.Fatalf("переход из ожидания не запросил единственный новый снимок: %d %v %s", status, unsafe, body)
	}
	// current содержит новую позицию Manager, err сообщает об отказе подтверждения эпохи.
	current, err := manager.Snapshot(t.Context(), stream.ID)
	if err != nil || current.Session == nil || current.Session.SessionID != sessionID {
		t.Fatalf("не установлена активная эпоха: %+v %v", current, err)
	}
	// active проверяет подписку без cursor и доставку терминала после остановки Hub.
	active, _, activeDone := chatStreamSelfServe(t, handler, eventPath)
	apiAcceptanceAwait(t, active.flushed)
	// err подтверждает отзыв точного оперативного владельца текущей сессии.
	if err := observer.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	apiAcceptanceAwait(t, activeDone)
	body, status, unsafe = active.snapshot()
	if status != 200 || unsafe || !active.deadline.IsZero() || strings.Count(body, "event: chat.session_closed\n") != 1 ||
		!strings.Contains(body, `"session_id":"`+string(sessionID)+`"`) {
		t.Fatalf("не доставлен терминал либо срок записи остался активен: %d %v %s", status, unsafe, body)
	}
}
