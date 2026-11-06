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

type eventCursor struct {
	Version  int     `json:"v"`
	Epoch    string  `json:"epoch"`
	Sequence Decimal `json:"sequence"`
}

func encodeCursor(c core.RuntimeCursor) string {
	b, _ := json.Marshal(eventCursor{1, string(c.Epoch), Decimal(c.Sequence)})
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodeCursor(s string, current core.RuntimeCursor) (core.RuntimeCursor, error) {
	if s == "" || len(s) > 512 {
		return core.RuntimeCursor{}, badRequest()
	}
	b, e := base64.RawURLEncoding.Strict().DecodeString(s)
	if e != nil {
		return core.RuntimeCursor{}, badRequest()
	}
	var v struct {
		Version  int      `json:"v"`
		Epoch    string   `json:"epoch"`
		Sequence *Decimal `json:"sequence"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&v) != nil || dec.Decode(new(any)) != io.EOF || v.Version != 1 || v.Sequence == nil || v.Epoch == "" || len(v.Epoch) > 128 || core.ID(v.Epoch).Validate() != nil {
		return core.RuntimeCursor{}, badRequest()
	}
	return core.RuntimeCursor{Incarnation: current.Incarnation, Epoch: core.ID(v.Epoch), Sequence: uint64(*v.Sequence)}, nil
}

type apiContextKey struct{}
type requestState struct {
	api     *API
	cleanup func()
}
type EventsInput struct {
	Cursor      string `query:"cursor"`
	LastEventID string `header:"Last-Event-ID"`
	page        core.RuntimeEventPage
}

func (i *EventsInput) Resolve(ctx huma.Context) []error {
	state, ok := ctx.Context().Value(apiContextKey{}).(*requestState)
	if !ok {
		return []error{huma.NewError(503, safeDetail(503))}
	}
	a := state.api
	if e := a.preflightEvents(ctx, i); e != nil {
		return []error{a.problem(e)}
	}
	return nil
}
func (a *API) preflightEvents(ctx huma.Context, i *EventsInput) error {
	if i.Cursor != "" && i.LastEventID != "" && i.Cursor != i.LastEventID {
		return badRequest()
	}
	requested := i.Cursor
	if requested == "" {
		requested = i.LastEventID
	}
	op, finish := a.operation(ctx.Context())
	defer finish()
	base, e := a.config.Core.RuntimeEvents(op, core.RuntimeCursor{}, a.config.Limits.SSEPageSize)
	if e != nil {
		if errors.Is(e, core.ErrNotReady) {
			return unavailable()
		}
		return e
	}
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
	_, w := humachi.Unwrap(ctx)
	if !supportsFlush(w) {
		return unavailable()
	}
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
	var once sync.Once
	release := func() {
		once.Do(func() { a.mu.Lock(); a.streams--; a.mu.Unlock(); _ = control.SetWriteDeadline(time.Time{}) })
	}
	ctx.Context().Value(apiContextKey{}).(*requestState).cleanup = release
	i.page = base
	ctx.SetHeader("Content-Type", "text/event-stream")
	ctx.SetHeader("Cache-Control", "no-cache")
	ctx.SetHeader("X-Accel-Buffering", "no")
	return nil
}
func supportsFlush(w http.ResponseWriter) bool {
	for {
		if _, ok := w.(http.Flusher); ok {
			return true
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = u.Unwrap()
	}
}
func (a *API) registerRuntime() {
	register(a, operation("GET", "/sessions/{id}/metrics", "get-session-metrics", "Текущие метрики точного исполнения запуска."), func(ctx context.Context, i *IDInput) (*Result[SessionMetricsDTO], error) {
		s, e := a.config.Core.GetSession(ctx, core.ID(i.ID))
		if e != nil {
			return nil, e
		}
		snapshot, e := a.config.Core.RuntimeSnapshot(ctx, core.RuntimeSessionFence{SessionID: s.ID, Incarnation: s.Incarnation, Lifecycle: s.Lifecycle})
		if errors.Is(e, core.ErrNotReady) || errors.Is(e, core.ErrRuntimeCapability) {
			reason := "not_observed"
			if s.State.Terminal() {
				reason = "session_terminal"
			}
			return &Result[SessionMetricsDTO]{SessionMetricsDTO{"unavailable", reason, nil}}, nil
		}
		if e != nil {
			return nil, e
		}
		v := runtimeSessionDTO(snapshot)
		return &Result[SessionMetricsDTO]{SessionMetricsDTO{"available", "", &v}}, nil
	})
	register(a, operation("GET", "/events/snapshot", "get-event-snapshot", "Атомарный снимок RAM и граница подписки."), func(ctx context.Context, _ *struct{}) (*Result[AnchorDTO], error) {
		anchor, e := a.config.Core.RuntimeAnchor(ctx)
		if e != nil {
			if errors.Is(e, core.ErrNotReady) {
				return nil, unavailable()
			}
			return nil, e
		}
		v := AnchorDTO{encodeCursor(anchor.Cursor), make([]RuntimeSessionDTO, 0, len(anchor.Sessions)), gpuDTO(anchor.GPU)}
		for _, s := range anchor.Sessions {
			v.Sessions = append(v.Sessions, runtimeSessionDTO(s))
		}
		return &Result[AnchorDTO]{v}, nil
	})
	op := operation("GET", "/events", "subscribe-events", "Ограниченный поток текущих событий RAM.")
	op.Responses = map[string]*huma.Response{"200": {Description: "Поток событий; при пропуске требуется новый снимок.", Content: map[string]*huma.MediaType{"text/event-stream": {Schema: &huma.Schema{Type: "string"}}}}}
	huma.Register(a.huma, op, func(_ context.Context, i *EventsInput) (*huma.StreamResponse, error) {
		return &huma.StreamResponse{Body: func(ctx huma.Context) { page := i.page; i.page = core.RuntimeEventPage{}; a.streamEvents(ctx, page) }}, nil
	})
}
func (a *API) streamEvents(ctx huma.Context, page core.RuntimeEventPage) {
	_, w := humachi.Unwrap(ctx)
	rc := http.NewResponseController(w)
	write := func(frame []byte) error {
		if len(frame) > a.config.Limits.SSEFrameBytes {
			return core.ErrObservationLimit
		}
		if e := ctx.Context().Err(); e != nil {
			return e
		}
		if e := rc.SetWriteDeadline(time.Now().Add(a.config.Limits.SSEWriteTimeout)); e != nil {
			return e
		}
		if _, e := w.Write(frame); e != nil {
			return e
		}
		return rc.Flush()
	}
	control := func(reason string) {
		frame, e := makeFrame("resync_required", "", struct {
			Reason      string `json:"reason"`
			SnapshotURL string `json:"snapshot_url"`
		}{reason, Prefix + "/events/snapshot"}, a.config.Limits.SSEFrameBytes)
		if e == nil {
			_ = write(frame)
		}
	}
	if e := write([]byte(": connected\n\n")); e != nil {
		return
	}
	ticker := time.NewTicker(a.config.Limits.SSEHeartbeat)
	defer ticker.Stop()
	for {
		if page.Gap || page.Reset {
			reason := "gap"
			if page.Reset {
				reason = "reset"
			}
			control(reason)
			return
		}
		for _, event := range page.Events {
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
		cursor := page.Cursor
		if cursor.Sequence < page.HighWater.Sequence {
			op, finish := a.operation(ctx.Context())
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
			if e := write([]byte(": heartbeat\n\n")); e != nil {
				return
			}
			continue
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
func makeFrame(event, id string, value any, limit int) ([]byte, error) {
	if strings.ContainsAny(event+id, "\r\n\x00") {
		return nil, core.ErrInvalid
	}
	data, e := json.Marshal(value)
	if e != nil {
		return nil, e
	}
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
