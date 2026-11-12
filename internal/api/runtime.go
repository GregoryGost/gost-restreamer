package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
)

// eventCursor задаёт публичную версию, эпоху и точную последовательность позиции SSE без внутреннего
// incarnation.
type eventCursor struct {
	// Version указывает версию формата публичного курсора, поддерживаемое значение равно 1.
	Version int `json:"v"`
	// Epoch содержит публичный идентификатор эпохи потока оперативных событий.
	Epoch string `json:"epoch"`
	// Sequence содержит точный номер события, представленный Decimal без потери разрядов JSON.
	Sequence Decimal `json:"sequence"`
}

// encodeCursor кодирует публичный epoch и точную последовательность RAM-потока без раскрытия
// внутреннего incarnation.
func encodeCursor(c core.RuntimeCursor) string {
	// b содержит JSON публичной позиции до кодирования base64url.
	b, _ := json.Marshal(eventCursor{1, string(c.Epoch), Decimal(c.Sequence)})
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor проверяет структуру и размер публичной позиции и восстанавливает текущий внутренний
// incarnation отдельно от пользовательских данных.
func decodeCursor(s string, current core.RuntimeCursor) (core.RuntimeCursor, error) {
	if s == "" || len(s) > 512 {
		return core.RuntimeCursor{}, badRequest()
	}
	// b содержит декодированные байты курсора; e содержит отказ строгого base64url-декодирования.
	b, e := base64.RawURLEncoding.Strict().DecodeString(s)
	if e != nil {
		return core.RuntimeCursor{}, badRequest()
	}
	// v принимает публичные поля курсора для строгой проверки версии, эпохи и обязательной
	// последовательности.
	var v struct {
		// Version принимает номер формата, который должен равняться 1.
		Version int `json:"v"`
		// Epoch принимает публичную эпоху, проверяемую как непустой ограниченный идентификатор.
		Epoch string `json:"epoch"`
		// Sequence отличает отсутствующую последовательность от допустимого нуля посредством
		// указателя.
		Sequence *Decimal `json:"sequence"`
	}
	// dec разбирает ровно один JSON-объект курсора с запрещёнными неизвестными полями.
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&v) != nil || dec.Decode(new(any)) != io.EOF || v.Version != 1 || v.Sequence == nil || v.Epoch == "" || len(v.Epoch) > 128 || core.ID(v.Epoch).Validate() != nil {
		return core.RuntimeCursor{}, badRequest()
	}
	return core.RuntimeCursor{Incarnation: current.Incarnation, Epoch: core.ID(v.Epoch), Sequence: uint64(*v.Sequence)}, nil
}

// apiContextKey выделяет собственный тип ключа контекста для состояния HTTP-запроса.
type apiContextKey struct{}

// requestState связывает запрос с API и освобождением занятого места SSE.
type requestState struct {
	// api указывает API, обслуживающий текущий запрос.
	api *API
	// cleanup освобождает ресурсы, занятые при предварительной проверке запроса, после возврата
	// обработчика.
	cleanup func()
}

// EventsInput принимает публичную позицию подписки из query или Last-Event-ID и переносит подготовленную
// страницу в потоковый ответ.
type EventsInput struct {
	// Cursor задаёт публичную позицию SSE в query; одновременный LastEventID должен совпадать с ней.
	Cursor string `query:"cursor"`
	// LastEventID принимает позицию из одноимённого заголовка для возобновления подписки.
	LastEventID string `header:"Last-Event-ID"`
	// page сохраняет первую проверенную страницу до начала SSE и очищается при передаче в
	// streamEvents.
	page core.RuntimeEventPage
}

// Resolve выполняет предварительные проверки SSE до начала ответа и передаёт ошибки через безопасное
// HTTP-представление.
func (i *EventsInput) Resolve(ctx huma.Context) []error {
	// state содержит состояние текущего запроса; ok подтверждает, что промежуточный обработчик API
	// установил его.
	state, ok := ctx.Context().Value(apiContextKey{}).(*requestState)
	if !ok {
		return []error{huma.NewError(503, safeDetail(503))}
	}
	// a содержит API из состояния запроса для предварительного допуска подписки.
	a := state.api
	// e содержит отказ предварительной проверки курсора, источника событий или SSE-транспорта.
	if e := a.preflightEvents(ctx, i); e != nil {
		return []error{a.problem(e)}
	}
	return nil
}

// preflightEvents согласует cursor и Last-Event-ID, читает ограниченную страницу и проверяет
// возможности транспорта до занятия клиентского места. Место и срок записи освобождаются
// зарегистрированной очисткой запроса.
func (a *API) preflightEvents(ctx huma.Context, i *EventsInput) error {
	if i.Cursor != "" && i.LastEventID != "" && i.Cursor != i.LastEventID {
		return badRequest()
	}
	// requested выбирает позицию из query или заголовка после проверки их согласованности.
	requested := i.Cursor
	if requested == "" {
		requested = i.LastEventID
	}
	// op ограничивает предварительные чтения событий; finish освобождает срок операции после допуска
	// или отказа.
	op, finish := a.operation(ctx.Context())
	defer finish()
	// base содержит исходную страницу и границу текущего RAM-потока; e содержит отказ чтения
	// оперативных событий.
	base, e := a.config.Core.RuntimeEvents(op, core.RuntimeCursor{}, a.config.Limits.SSEPageSize)
	if e != nil {
		if errors.Is(e, core.ErrNotReady) {
			return unavailable()
		}
		return e
	}
	// cursor задаёт позицию подписки: текущую верхнюю границу либо разобранный запрос клиента.
	cursor := base.HighWater
	if requested != "" {
		cursor, e = decodeCursor(requested, base.HighWater)
		if e != nil {
			return e
		}
		base, e = a.config.Core.RuntimeEvents(op, cursor, a.config.Limits.SSEPageSize)
		if e != nil {
			if errors.Is(e, core.ErrInvalid) {
				return badRequest()
			}
			return e
		}
	} else {
		base = core.RuntimeEventPage{Cursor: cursor, HighWater: cursor, Changed: base.Changed}
	}
	// w содержит HTTP-транспорт, у которого проверяются Flush и сроки записи.
	_, w := humachi.Unwrap(ctx)
	if !supportsFlush(w) {
		return unavailable()
	}
	// control устанавливает и сбрасывает конечный срок записи SSE через HTTP-транспорт.
	control := http.NewResponseController(w)
	if e = control.SetWriteDeadline(time.Now().Add(a.config.Limits.SSEWriteTimeout)); e != nil {
		return unavailable()
	}
	a.mu.Lock()
	if a.closed || a.streams >= a.config.Limits.SSEClients {
		a.mu.Unlock()
		_ = control.SetWriteDeadline(time.Time{})
		return unavailable()
	}
	a.streams++
	a.mu.Unlock()
	// once обеспечивает однократное освобождение клиентского места и сброс срока записи.
	var once sync.Once
	// release возвращает место SSE и снимает транспортный срок только один раз при завершении
	// запроса.
	release := func() {
		// Освобождает место подписчика SSE и сбрасывает срок записи только при первом вызове.

		once.Do(func() {
			// Уменьшает число подписчиков SSE под блокировкой, затем снимает установленный срок записи.
			a.mu.Lock()
			a.streams--
			a.mu.Unlock()
			_ = control.SetWriteDeadline(time.Time{})
		})
	}
	ctx.Context().Value(apiContextKey{}).(*requestState).cleanup = release
	i.page = base
	ctx.SetHeader("Content-Type", "text/event-stream")
	ctx.SetHeader("Cache-Control", "no-cache")
	ctx.SetHeader("X-Accel-Buffering", "no")
	return nil
}

// supportsFlush проверяет поддержку Flush, проходя через обёртки ResponseWriter с Unwrap.
func supportsFlush(w http.ResponseWriter) bool {
	for {
		// ok отмечает прямую поддержку Flush текущей обёрткой HTTP-ответа.
		if _, ok := w.(http.Flusher); ok {
			return true
		}
		// u содержит обёртку с Unwrap; ok показывает возможность продолжить поиск Flush во
		// внутреннем транспорте.
		u, ok := w.(interface {
			// Unwrap возвращает внутренний ResponseWriter для проверки возможностей Flush через транспортные
			// обёртки.
			Unwrap() http.ResponseWriter
		})
		if !ok {
			return false
		}
		w = u.Unwrap()
	}
}

// registerRuntime публикует текущие метрики, согласованный RAM-снимок с cursor и SSE. Известная
// Session без текущего наблюдения возвращается с явной недоступностью, а не с исторической
// телеметрией.
func (a *API) registerRuntime() {
	register(a, operation("GET", "/sessions/{id}/metrics", "get-session-metrics", "Текущие метрики точного исполнения запуска."), func(ctx context.Context, i *IDInput) (*Result[SessionMetricsDTO], error) {
		// Возвращает наблюдения точного исполнения Session либо явную причину их недоступности.

		// s содержит сохранённую Session; e содержит отказ чтения перед запросом наблюдения
		// точного исполнения.
		s, e := a.config.Core.GetSession(ctx, core.ID(i.ID))
		if e != nil {
			return nil, e
		}
		// snapshot содержит текущие наблюдения указанной Session, incarnation и lifecycle.
		snapshot, e := a.config.Core.RuntimeSnapshot(ctx, core.RuntimeSessionFence{SessionID: s.ID, Incarnation: s.Incarnation, Lifecycle: s.Lifecycle})
		if errors.Is(e, core.ErrNotReady) || errors.Is(e, core.ErrRuntimeCapability) {
			// reason обозначает отсутствие текущего наблюдения либо терминальную Session в
			// публичном ответе метрик.
			reason := "not_observed"
			if s.State.Terminal() {
				reason = "session_terminal"
			}
			return &Result[SessionMetricsDTO]{SessionMetricsDTO{"unavailable", reason, nil}}, nil
		}
		if e != nil {
			return nil, e
		}
		// v содержит публичное представление доступного текущего наблюдения Session.
		v := runtimeSessionDTO(snapshot)
		return &Result[SessionMetricsDTO]{SessionMetricsDTO{"available", "", &v}}, nil
	})
	register(a, operation("GET", "/events/snapshot", "get-event-snapshot", "Атомарный снимок RAM и граница подписки."), func(ctx context.Context, _ *struct{}) (*Result[AnchorDTO], error) {
		// Преобразует атомарный снимок оперативных наблюдений и его курсор в публичное представление.

		// anchor содержит согласованные RAM-наблюдения и курсор подписки; e содержит отказ
		// получения общего снимка.
		anchor, e := a.config.Core.RuntimeAnchor(ctx)
		if e != nil {
			if errors.Is(e, core.ErrNotReady) {
				return nil, unavailable()
			}
			return nil, e
		}
		// v собирает публичный снимок с курсором, сессиями и GPU без внутренних полномочий.
		v := AnchorDTO{encodeCursor(anchor.Cursor), make([]RuntimeSessionDTO, 0, len(anchor.Sessions)), gpuDTO(anchor.GPU)}
		// s содержит очередное наблюдение Session из атомарного снимка для преобразования в DTO.
		for _, s := range anchor.Sessions {
			v.Sessions = append(v.Sessions, runtimeSessionDTO(s))
		}
		return &Result[AnchorDTO]{v}, nil
	})
	// op описывает SSE-маршрут с потоковым типом text/event-stream вместо обычного JSON-ответа.
	op := operation("GET", "/events", "subscribe-events", "Ограниченный поток текущих событий RAM.")
	op.Responses = map[string]*huma.Response{"200": {Description: "Поток событий; при пропуске требуется новый снимок.", Content: map[string]*huma.MediaType{"text/event-stream": {Schema: &huma.Schema{Type: "string"}}}}}
	huma.Register(a.huma, op, func(_ context.Context, i *EventsInput) (*huma.StreamResponse, error) {
		// Создаёт потоковый ответ из страницы событий, подготовленной до отправки заголовков.

		return &huma.StreamResponse{Body: func(ctx huma.Context) {
			// Забирает подготовленную страницу, убирает её из EventsInput и запускает передачу SSE.
			// page забирает подготовленную страницу из входа перед очисткой ссылки в
			// EventsInput.
			page := i.page
			i.page = core.RuntimeEventPage{}
			a.streamEvents(ctx, page)
		}}, nil
	})
}

// streamEvents передаёт ограниченные страницы событий без удержания RuntimeAuthority во время сети.
// Пропуск, смена эпохи или слишком большой кадр требуют нового снимка; отмена и конечный срок записи
// завершают подписку.
func (a *API) streamEvents(ctx huma.Context, page core.RuntimeEventPage) {
	// w содержит HTTP-транспорт текущей SSE-подписки.
	_, w := humachi.Unwrap(ctx)
	// rc обновляет срок записи и выполняет Flush каждого отправленного SSE-кадра.
	rc := http.NewResponseController(w)
	// write проверяет ограничение кадра и отмену, затем обновляет срок, пишет и принудительно
	// передаёт кадр клиенту.
	write := func(frame []byte) error {
		// Проверяет размер кадра и отмену, обновляет срок записи и отправляет кадр SSE с принудительной
		// передачей клиенту.

		if len(frame) > a.config.Limits.SSEFrameBytes {
			return core.ErrObservationLimit
		}
		// e содержит причину отмены контекста подписчика перед записью очередного кадра.
		if e := ctx.Context().Err(); e != nil {
			return e
		}
		// e содержит отказ установки срока записи очередного SSE-кадра.
		if e := rc.SetWriteDeadline(time.Now().Add(a.config.Limits.SSEWriteTimeout)); e != nil {
			return e
		}
		// e содержит отказ записи SSE-кадра в HTTP-транспорт.
		if _, e := w.Write(frame); e != nil {
			return e
		}
		return rc.Flush()
	}
	// control формирует ограниченное уведомление resync_required с причиной и адресом нового снимка.
	control := func(reason string) {
		// Пытается сообщить клиенту причину обязательного повторного снимка, соблюдая ограничение размера
		// кадра.

		// frame содержит кадр обязательной повторной синхронизации; e содержит отказ его
		// формирования в пределах бюджета.
		frame, e := makeFrame("resync_required", "", struct {
			// Reason сообщает причину обязательного повторного получения снимка.
			Reason string `json:"reason"`
			// SnapshotURL указывает публичный маршрут атомарного снимка для восстановления
			// позиции.
			SnapshotURL string `json:"snapshot_url"`
		}{reason, Prefix + "/events/snapshot"}, a.config.Limits.SSEFrameBytes)
		if e == nil {
			_ = write(frame)
		}
	}
	// e содержит отказ отправки начального служебного кадра подключённой подписки.
	if e := write([]byte(": connected\n\n")); e != nil {
		return
	}
	// ticker задаёт период отправки служебных heartbeat-кадров и останавливается при выходе
	// подписки.
	ticker := time.NewTicker(a.config.Limits.SSEHeartbeat)
	defer ticker.Stop()
	for {
		if page.Gap || page.Reset {
			// reason выбирает сообщение о пропуске событий либо смене эпохи перед
			// завершением подписки.
			reason := "gap"
			if page.Reset {
				reason = "reset"
			}
			control(reason)
			return
		}
		// event содержит очередное событие текущей ограниченной страницы для формирования
		// SSE-кадра.
		for _, event := range page.Events {
			// frame содержит кадр текущего события с публичным курсором; e содержит отказ
			// его ограниченного формирования.
			frame, e := makeFrame(string(event.Kind), encodeCursor(event.Cursor), runtimeEventDTO(event), a.config.Limits.SSEFrameBytes)
			if e != nil {
				control("frame_limit")
				return
			}
			if e = write(frame); e != nil {
				return
			}
		}
		page.Events = nil
		// cursor сохраняет позицию прочитанной страницы для следующего чтения после отправки
		// событий.
		cursor := page.Cursor
		if cursor.Sequence < page.HighWater.Sequence {
			// op ограничивает дочитывание уже доступных событий; finish освобождает контекст
			// сразу после чтения страницы.
			op, finish := a.operation(ctx.Context())
			// next содержит следующую страницу событий после cursor; e содержит отказ её
			// чтения, требующий нового снимка.
			next, e := a.config.Core.RuntimeEvents(op, cursor, a.config.Limits.SSEPageSize)
			finish()
			if e != nil {
				control("source_unavailable")
				return
			}
			page = next
			continue
		}
		select {
		case <-ctx.Context().Done():
			return
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			// e содержит отказ отправки служебного heartbeat-кадра.
			if e := write([]byte(": heartbeat\n\n")); e != nil {
				return
			}
			continue
		// op ограничивает чтение после сигнала изменения RAM-потока; finish освобождает контекст
		// после чтения.
		// next содержит страницу после уведомления об изменениях; e содержит отказ источника
		// событий.
		case <-page.Changed:
			op, finish := a.operation(ctx.Context())
			next, e := a.config.Core.RuntimeEvents(op, cursor, a.config.Limits.SSEPageSize)
			finish()
			if e != nil {
				control("source_unavailable")
				return
			}
			page = next
		}
	}
}

// makeFrame формирует целый ограниченный SSE-кадр с JSON-данными и отклоняет переводы строк или NUL в
// event и id до записи.
func makeFrame(event, id string, value any, limit int) ([]byte, error) {
	if strings.ContainsAny(event+id, "\r\n\x00") {
		return nil, core.ErrInvalid
	}
	// data содержит сериализованные данные события; e содержит отказ JSON-кодирования до
	// формирования кадра.
	data, e := json.Marshal(value)
	if e != nil {
		return nil, e
	}
	// b собирает полный SSE-кадр с необязательным id, именем события и JSON-данными до проверки
	// размера.
	var b bytes.Buffer
	if id != "" {
		b.WriteString("id: ")
		b.WriteString(id)
		b.WriteByte('\n')
	}
	b.WriteString("event: ")
	b.WriteString(event)
	b.WriteString("\ndata: ")
	b.Write(data)
	b.WriteString("\n\n")
	if b.Len() > limit {
		return nil, core.ErrObservationLimit
	}
	return b.Bytes(), nil
}
