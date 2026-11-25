package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/GregoryGost/gost-restreamer/internal/chat"
	"github.com/GregoryGost/gost-restreamer/internal/chatwire"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
)

// chatEventsInput принимает позицию клиента и удерживает выбранную границу до начала SSE.
type chatEventsInput struct {
	// StreamID идентифицирует поток, для которого открывается лента.
	StreamID string `path:"streamId"`
	// Cursor передаёт позицию из query и должен совпадать с LastEventID при наличии обоих.
	Cursor string `query:"cursor" doc:"Непрозрачная позиция начального снимка или последнего полученного события."`
	// LastEventID передаёт позицию из заголовка Last-Event-ID.
	LastEventID string `header:"Last-Event-ID" doc:"Непрозрачная позиция для возобновления подписки."`
	// start содержит атомарную границу ожидания либо подписки активной эпохи.
	start chat.ChatStreamStart
}

// Resolve допускает SSE до начала записи, сохраняя возможность ответить Problem Details.
func (i *chatEventsInput) Resolve(ctx huma.Context) []error {
	// state содержит владельца HTTP-обработчика, ok отмечает наличие его промежуточного обработчика.
	state, ok := ctx.Context().Value(apiContextKey{}).(*requestState)
	if !ok {
		return []error{huma.NewError(http.StatusServiceUnavailable, safeDetail(http.StatusServiceUnavailable))}
	}
	// err сообщает о транспортном отказе либо невозможности открыть представление чата.
	if err := state.api.prepareChatEvents(ctx, state, i); err != nil {
		return chatStreamProblem(state, err)
	}
	return nil
}

// chatRequestCursor проверяет однозначность и размер query и заголовка до обращения к Manager.
// Подпись проверяет Hub, поэтому повреждённая непрозрачная позиция приводит к resync.
func chatRequestCursor(request *http.Request, limit uint64) (core.ChatCursor, error) {
	// query сохраняет все значения параметров, err сообщает о некорректном query-кодировании.
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return "", reasonError(badRequest(), "invalid_cursor", "cursor")
	}
	// queryValues и headerValues содержат каждое переданное значение для обнаружения повторений.
	queryValues := query["cursor"]
	headerValues := request.Header.Values("Last-Event-ID")
	if len(queryValues) > 1 || len(headerValues) > 1 {
		return "", reasonError(badRequest(), "invalid_cursor", "cursor")
	}
	// cursor принимает единственную общую позицию или остаётся пустым при её отсутствии.
	var cursor string
	// values содержит каждое из двух представлений позиции.
	for _, values := range [][]string{queryValues, headerValues} {
		if len(values) == 0 {
			continue
		}
		// value содержит позицию без преобразования её байтов после разбора HTTP.
		value := values[0]
		// Функция отличает разделители и управляющие символы от единственной непрозрачной позиции.
		if value == "" || uint64(len(value)) > limit || !utf8.ValidString(value) ||
			strings.IndexFunc(value, func(char rune) bool {
				return char == ',' || unicode.IsControl(char) || unicode.IsSpace(char)
			}) >= 0 {
			return "", reasonError(badRequest(), "invalid_cursor", "cursor")
		}
		if cursor != "" && cursor != value {
			return "", reasonError(badRequest(), "sse_cursor_conflict", "cursor")
		}
		cursor = value
	}
	return core.ChatCursor(cursor), nil
}

// prepareChatEvents проверяет курсор и транспорт, затем закрепляет за HTTP-запросом
// общий и потоковый допуски вплоть до физического выхода обработчика.
func (a *API) prepareChatEvents(ctx huma.Context, state *requestState, input *chatEventsInput) error {
	// request сохраняет заголовки и все экземпляры исходных параметров, writer передаёт SSE.
	request, writer := humachi.Unwrap(ctx)
	// cursor содержит позицию клиента, err сообщает о неоднозначном запросе.
	cursor, err := chatRequestCursor(request, a.config.ChatLimits.CursorBytes)
	if err != nil {
		return err
	}
	// control проверяет поддержку срока записи, оставляя его снятым во время чтения Store.
	control := http.NewResponseController(writer)
	if !supportsFlush(writer) || control.SetWriteDeadline(time.Time{}) != nil {
		return reasonError(unavailable(), "sse_transport_unavailable", "")
	}
	// operation ограничивает предварительную проверку, finish освобождает её срок до начала SSE.
	operation, finish := a.operation(ctx.Context())
	defer finish()
	// start содержит границу Stream, release освобождает квоту и очередь, err сообщает об отказе.
	start, release, err := a.config.Chat.OpenEventStream(operation, core.ID(input.StreamID), cursor)
	if errors.Is(err, core.ErrBusy) {
		return reasonError(unavailable(), "sse_client_limit", "")
	}
	if err != nil {
		return chatReadError(err)
	}
	state.cleanup = release
	input.start = start
	ctx.SetHeader("Content-Type", "text/event-stream")
	ctx.SetHeader("Cache-Control", "no-cache")
	ctx.SetHeader("X-Accel-Buffering", "no")
	return nil
}

// chatHTTPWriter отправляет снимок или кадр под отдельным сроком и сбрасывает его после Flush.
type chatHTTPWriter struct {
	// writer передаёт байты HTTP-ответа.
	writer http.ResponseWriter
	// control выполняет Flush и устанавливает физический срок транспорта.
	control *http.ResponseController
	// timeout ограничивает запись и Flush одной порции ответа.
	timeout time.Duration
	// limit ограничивает полный размер снимка либо кадра.
	limit uint64
}

// chatWriter связывает запись ответа с реальным транспортом Chi и проверенными бюджетами.
func (a *API) chatWriter(ctx huma.Context, limit uint64) chatHTTPWriter {
	// writer содержит исходный HTTP-транспорт для записи, установки сроков и Flush.
	_, writer := humachi.Unwrap(ctx)
	return chatHTTPWriter{
		writer: writer, control: http.NewResponseController(writer),
		timeout: a.config.ChatLimits.SSEWriteTimeout, limit: limit,
	}
}

// write проверяет размер и отмену до записи. Flush выполняется под тем же сроком,
// а перед ожиданием heartbeat или нового события срок снимается.
func (w chatHTTPWriter) write(ctx context.Context, frame []byte) (result error) {
	if uint64(len(frame)) > w.limit {
		return core.ErrObservationLimit
	}
	// err сообщает об отмене клиента или закрытии API до начала кадра.
	if err := ctx.Err(); err != nil {
		return err
	}
	// err сообщает об отказе установки срока текущего кадра.
	if err := w.control.SetWriteDeadline(time.Now().Add(w.timeout)); err != nil {
		return err
	}
	// Функция снимает срок даже после частичной записи либо ошибки Flush.
	defer func() {
		result = errors.Join(result, w.control.SetWriteDeadline(time.Time{}))
	}()
	// n учитывает записанные байты, err сообщает об отказе записи транспорта.
	n, err := w.writer.Write(frame)
	if err != nil {
		return err
	}
	if n != len(frame) {
		return io.ErrShortWrite
	}
	return w.control.Flush()
}

// event передаёт один кадр. Если полезная нагрузка превышает предел, отправляет
// управляющее событие и возвращает отказ для завершения прежней подписки.
func (w chatHTTPWriter) event(ctx context.Context, event core.ChatEvent) error {
	// frame содержит готовый кадр, err сообщает о превышении бюджета или невозможности кодирования.
	frame, err := chatwire.Frame(event, w.limit)
	if err != nil {
		if event.Kind != core.ChatEventResync {
			// resync сохраняет позицию проблемного события, чтобы потребитель перечитал снимок.
			resync := core.ChatEvent{Kind: core.ChatEventResync, Cursor: event.Cursor, SessionID: event.SessionID, Reason: core.ChatReasonLimit}
			// fallback содержит кадр управления, failure сообщает о невозможности его сформировать.
			fallback, failure := chatwire.Frame(resync, w.limit)
			if failure == nil {
				return errors.Join(err, w.write(ctx, fallback))
			}
		}
		return err
	}
	return w.write(ctx, frame)
}

// serveChatEvents подтверждает принятие потока и обслуживает выбранную страницу
// или досылает кадры активной подписки без управляющего срока всего соединения.
func (a *API) serveChatEvents(ctx huma.Context, input *chatEventsInput) {
	// output связывает запись каждого кадра с транспортными бюджетами чатов.
	output := a.chatWriter(ctx, a.config.ChatLimits.SSEFrameBytes)
	// request ограничивает SSE временем жизни HTTP-клиента и API.
	request := ctx.Context()
	// err сообщает о невозможности сразу передать заголовки и начальный комментарий.
	if err := output.write(request, []byte(chatConnected)); err != nil {
		return
	}
	// ticker задаёт время между служебными кадрами во время ожидания.
	ticker := time.NewTicker(a.config.ChatLimits.SSEHeartbeat)
	defer ticker.Stop()
	if input.start.Subscription == nil {
		a.streamWaitingChat(request, output, core.ID(input.StreamID), input.start.Waiting, ticker.C)
		return
	}
	streamActiveChat(request, output, input.start.Subscription, ticker.C)
}

// streamWaitingChat отправляет маркеры предсессионной ленты и ждёт смены её позиции либо закрытия Manager.
// При появлении активной Session он продолжает этот же HTTP-ответ на очереди реальной эпохи.
// ctx задаёт срок соединения, output передаёт кадры, streamID выбирает ленту, page задаёт её текущую позицию,
// а heartbeat передаёт канал служебных отметок ожидания.
func (a *API) streamWaitingChat(ctx context.Context, output chatHTTPWriter, streamID core.ID, page core.ChatEventPage, heartbeat <-chan time.Time) {
	for {
		// event содержит очередной маркер ожидания, который должен быть виден до новой эпохи.
		for _, event := range page.Events {
			if output.event(ctx, event) != nil || event.Kind == core.ChatEventResync || event.Kind == core.ChatEventSessionClosed {
				return
			}
		}
		// Удаляет уже отправленные маркеры, чтобы heartbeat не повторял chat.cleared.
		page.Events = nil
		if page.ResyncRequired {
			_ = output.event(ctx, core.ChatEvent{Kind: core.ChatEventResync, Cursor: page.Cursor})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-a.config.Chat.Done():
			return
		case <-heartbeat:
			if output.write(ctx, []byte(chatHeartbeat)) != nil {
				return
			}
		case <-page.Changed:
			// operation ограничивает повторное чтение, finish снимает таймер после его завершения.
			operation, finish := a.operation(ctx)
			// start содержит новую границу ожидания либо подписку активной эпохи; err сообщает об отказе продолжения.
			start, err := a.config.Chat.ContinueEventStream(operation, streamID, page.Cursor)
			finish()
			if err != nil {
				return
			}
			if start.Subscription != nil {
				streamContinuedChat(ctx, output, start.Subscription, heartbeat)
				return
			}
			page = start.Waiting
		}
	}
}

// streamContinuedChat обслуживает очередь, полученную при переходе ожидавшего клиента в активную эпоху.
// ctx ограничивает время ответа, output отправляет SSE-кадры, subscription закрывается после выхода,
// а heartbeat сохраняет служебные кадры до конца активной подписки.
func streamContinuedChat(ctx context.Context, output chatHTTPWriter, subscription *chat.ChatSubscription, heartbeat <-chan time.Time) {
	defer subscription.Close()
	streamActiveChat(ctx, output, subscription, heartbeat)
}

// streamActiveChat читает по одному кадру, освобождая элементы очереди после передачи.
// Done может закрыться между пустым чтением и ожиданием. В этом случае цикл снова
// читает очередь, чтобы передать терминальный кадр закрытой эпохи.
// ctx задаёт срок соединения, output передаёт кадры, subscription хранит очередь клиента,
// а heartbeat передаёт канал служебных отметок ожидания.
func streamActiveChat(ctx context.Context, output chatHTTPWriter, subscription *chat.ChatSubscription, heartbeat <-chan time.Time) {
	for {
		// ended показывает, завершилась ли подписка ещё до чтения очереди.
		ended := false
		select {
		case <-subscription.Done():
			ended = true
		default:
		}
		// page содержит до одного кадра и сигнал, err сообщает о закрытии или отмене чтения.
		page, err := subscription.Events(ctx, 1)
		if err != nil {
			return
		}
		if len(page.Events) != 0 {
			// event содержит очередной кадр, включая возможное завершение ленты.
			event := page.Events[0]
			if !subscription.Current(event) {
				continue
			}
			if output.event(ctx, event) != nil || event.Kind == core.ChatEventResync || event.Kind == core.ChatEventSessionClosed {
				return
			}
			continue
		}
		if ended {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-subscription.Done():
		case <-page.Changed:
		case <-heartbeat:
			if output.write(ctx, []byte(chatHeartbeat)) != nil {
				return
			}
		}
	}
}
