package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/chat"
	"github.com/GregoryGost/gost-restreamer/internal/chatwire"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
)

const (
	// chatStreamPath задаёт общий путь снимка и потока событий чата.
	chatStreamPath = "/chat/streams/{streamId}"
	// chatConnected сообщает клиенту о принятом HTTP-соединении до первого события.
	chatConnected = ": connected\n\n"
	// chatHeartbeat поддерживает соединение во время ожидания без изменения позиции клиента.
	chatHeartbeat = ": heartbeat\n\n"
)

// ValidateChatLimits проверяет транспортные бюджеты вместе с доменными. Даже самый
// длинный SessionID помещается в управляющий кадр, а пустой снимок вмещает штатные ID.
// limits задаёт проверяемые размеры снимков, курсоров и кадров.
func ValidateChatLimits(limits core.ChatLimits) error {
	if limits.Validate() != nil || limits.CursorBytes < chat.EventCursorBytes ||
		limits.RESTBodyBytes >= math.MaxInt64 || limits.SnapshotBytes > math.MaxInt || limits.SSEFrameBytes > math.MaxInt {
		return core.ErrInvalid
	}
	// cursor занимает столько же байтов, сколько выдаваемая подписанная позиция.
	cursor := core.ChatCursor(strings.Repeat("A", chat.EventCursorBytes))
	// generatedID представляет штатный 128-битный идентификатор core.newID в шестнадцатеричной записи.
	generatedID := core.ID(strings.Repeat("0", 32))
	// data содержит пустой активный снимок, err сообщает об отказе его кодирования.
	data, err := json.Marshal(chatwire.Snapshot(core.ChatSnapshot{
		StreamID: generatedID, Session: &core.RuntimeSessionFence{SessionID: generatedID}, Cursor: cursor,
	}))
	if err != nil || uint64(len(data)) > limits.SnapshotMetadataBytes {
		return core.ErrInvalid
	}
	// sessionID представляет максимум экранирования допустимого идентификатора в JSON.
	sessionID := core.ID(strings.Repeat("\x01", core.ChatIDBytes))
	// event проверяет каждый управляющий кадр с той же сериализацией, что при отправке.
	for _, event := range []core.ChatEvent{
		{Kind: core.ChatEventSessionClosed, Cursor: cursor, SessionID: sessionID},
		{Kind: core.ChatEventCleared, Cursor: cursor},
		{Kind: core.ChatEventResync, Cursor: cursor, SessionID: sessionID, Reason: core.ChatReasonLimit},
		{Kind: core.ChatEventGap, Cursor: cursor, SessionID: sessionID, Reason: core.ChatReasonUpstreamGap, Lost: math.MaxUint64},
	} {
		if _, err = chatwire.Frame(event, limits.SSEFrameBytes); err != nil {
			return core.ErrInvalid
		}
	}
	return nil
}

// chatSnapshotInput получает согласованный снимок до начала успешного ответа.
type chatSnapshotInput struct {
	// StreamID выбирает поток, для которого открывается лента.
	StreamID string `path:"streamId"`
	// snapshot содержит точный JSON, который HTTP отправляет под транспортным допуском.
	snapshot []byte
}

// Resolve сохраняет согласованное представление и удерживает допуск во время
// сериализации и до физического Flush. До записи заголовков возможен Problem Details.
func (i *chatSnapshotInput) Resolve(ctx huma.Context) []error {
	// state содержит владельца операции и очистки, ok проверяет наличие промежуточного обработчика.
	state, ok := ctx.Context().Value(apiContextKey{}).(*requestState)
	if !ok {
		return []error{huma.NewError(http.StatusServiceUnavailable, safeDetail(http.StatusServiceUnavailable))}
	}
	// err содержит отказ чтения снимка либо транспорта до отправки ответа.
	if err := state.api.prepareChatSnapshot(ctx, state, i); err != nil {
		return chatStreamProblem(state, err)
	}
	return nil
}

// prepareChatSnapshot фиксирует согласованную ленту и передаёт её слот промежуточному
// обработчику Huma. JSON подготавливается целиком до первого байта успешного ответа.
func (a *API) prepareChatSnapshot(ctx huma.Context, state *requestState, input *chatSnapshotInput) error {
	// operation ограничивает чтение хранилища, finish освобождает его таймер.
	operation, finish := a.operation(ctx.Context())
	defer finish()
	// snapshot содержит ленту, release освобождает её допуск, err сообщает об отказе чтения.
	snapshot, release, err := a.config.Chat.SnapshotResponse(operation, core.ID(input.StreamID))
	if err != nil {
		return chatReadError(err)
	}
	state.cleanup = release
	// writer содержит тот же транспорт, в который поступит JSON.
	_, writer := humachi.Unwrap(ctx)
	if !supportsFlush(writer) || http.NewResponseController(writer).SetWriteDeadline(time.Time{}) != nil {
		return unavailable()
	}
	// body проходит общий преобразователь Huma, который добавляет ссылку на схему ответа.
	body, err := a.huma.Transform(ctx, "200", chatwire.Snapshot(snapshot))
	if err != nil {
		return err
	}
	input.snapshot, err = json.Marshal(body)
	if err != nil {
		return err
	}
	if uint64(len(input.snapshot)) > a.config.ChatLimits.SnapshotBytes {
		return unavailable()
	}
	ctx.SetHeader("Content-Type", "application/json")
	return nil
}

// chatReadError переводит исчерпание бюджета чтения чата во временную недоступность.
func chatReadError(err error) error {
	if errors.Is(err, core.ErrNotReady) || errors.Is(err, core.ErrStale) {
		return unavailable()
	}
	return err
}

// chatStreamProblem сохраняет выбранное безопасное представление при обработке ошибки Resolver в Huma.
func chatStreamProblem(state *requestState, err error) []error {
	// problem содержит безопасный статус и код причины после общего преобразования ошибок.
	problem := state.api.problem(err)
	state.problem, _ = problem.(huma.StatusError)
	return []error{problem}
}

// registerChatStream регистрирует оба ресурса и публикует схемы JSON независимо от
// текстового обрамления событий. При отсутствии Manager ресурсы не регистрируются.
func (a *API) registerChatStream() {
	if a.config.Chat == nil {
		return
	}
	// registry хранит общие типизированные модели ответа ленты.
	registry := a.huma.OpenAPI().Components.Schemas
	// optionsSchema ссылается на настройки в снимке и в ответах REST. Те же
	// закрытые варианты применяются при приёме настроек источника.
	optionsSchema := registry.Schema(reflect.TypeFor[chatwire.OptionsDTO](), true, "")
	*registry.SchemaFromRef(optionsSchema.Ref) = *(chatOptionsBody{}).Schema(registry)
	// snapshotOp описывает получение согласованного начального состояния.
	snapshotOp := operation(http.MethodGet, chatStreamPath+"/snapshot", "get-chat-snapshot", "Согласованный снимок чата и позиция подписки.")
	// snapshotSchema ссылается на компонент с фактическими полями JSON.
	snapshotSchema := registry.Schema(reflect.TypeFor[chatwire.SnapshotDTO](), true, "")
	// snapshotFields содержит свойства схемы снимка, sessionSchema описывает идентификатор известной сессии.
	snapshotFields := registry.SchemaFromRef(snapshotSchema.Ref).Properties
	sessionSchema := snapshotFields["session"]
	// Huma не поддерживает nullable для ссылки на объект. Снимок всегда содержит ключ session.
	snapshotFields["session"] = &huma.Schema{AnyOf: []*huma.Schema{sessionSchema, {Type: "null"}}}
	snapshotOp.Responses = map[string]*huma.Response{"200": {
		Description: "Снимок чата, в том числе до запуска трансляции.",
		Content:     map[string]*huma.MediaType{"application/json": {Schema: snapshotSchema}},
	}}
	a.setProblemResponses(&snapshotOp)
	huma.Register(a.huma, snapshotOp, func(_ context.Context, input *chatSnapshotInput) (*huma.StreamResponse, error) {
		// Записывает подготовленный снимок с конечным сроком, даже если клиент читает медленно.
		return &huma.StreamResponse{Body: func(ctx huma.Context) {
			// output отвечает за физическую запись и Flush всего JSON-ответа.
			output := a.chatWriter(ctx, a.config.ChatLimits.SnapshotBytes)
			_ = output.write(ctx.Context(), input.snapshot)
		}}, nil
	})
	// Huma регистрирует стандартный ErrorModel. Возвращаем в спецификацию модель API.
	a.setProblemResponses(&snapshotOp)

	// eventsOp описывает SSE отдельно от медиасобытий и JSON-снимка.
	eventsOp := operation(http.MethodGet, chatStreamPath+"/events", "subscribe-chat-events", "Поток сообщений и состояний чата.")
	eventsOp.Responses = map[string]*huma.Response{"200": {
		Description: "Поток событий чата. При resync_required клиент получает новый снимок.",
		Content:     map[string]*huma.MediaType{"text/event-stream": {Schema: &huma.Schema{Type: "string"}}},
	}}
	// eventSchema описывает данные внутри каждого SSE-кадра.
	eventSchema := registry.Schema(reflect.TypeFor[chatwire.EventDTO](), true, "")
	eventsOp.Extensions = map[string]any{"x-sse-events": map[string]any{
		string(core.ChatEventMessage): eventSchema, string(core.ChatEventSourceStatus): eventSchema,
		string(core.ChatEventGap): eventSchema, string(core.ChatEventResync): eventSchema,
		string(core.ChatEventSessionClosed): eventSchema, string(core.ChatEventCleared): eventSchema,
	}}
	a.setProblemResponses(&eventsOp)
	huma.Register(a.huma, eventsOp, func(_ context.Context, input *chatEventsInput) (*huma.StreamResponse, error) {
		// Возвращает потоковый ответ с установленным до отправки заголовков допуском.
		return &huma.StreamResponse{Body: func(ctx huma.Context) {
			// Передаёт события и комментарии SSE до завершения эпохи или запроса.
			a.serveChatEvents(ctx, input)
		}}, nil
	})
	a.setProblemResponses(&eventsOp)
	a.registerChatTestFeed()
}
