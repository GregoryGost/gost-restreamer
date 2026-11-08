// Package api предоставляет управляющий HTTP API и ограниченный поток событий.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"log/slog"
	"net/http"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
)

const Prefix = "/api/v1"

type Commands interface {
	StartSession(context.Context, core.StartSessionRequest) (core.CommandReceipt, error)
	StopSession(context.Context, core.SessionCommand) (core.CommandReceipt, error)
	PauseSession(context.Context, core.SessionCommand) (core.CommandReceipt, error)
	ResumeSession(context.Context, core.SessionCommand) (core.CommandReceipt, error)
	AddOutput(context.Context, core.AddOutputRequest) (core.CommandReceipt, error)
	StartOutput(context.Context, core.OutputCommand) (core.CommandReceipt, error)
	StopOutput(context.Context, core.OutputCommand) (core.CommandReceipt, error)
	DetachOutput(context.Context, core.OutputCommand) (core.CommandReceipt, error)
}
type Preparation interface {
	Upload(context.Context, core.PreparationRequest, io.ReadCloser) (*fallback.Job, error)
	Rebuild(context.Context, core.PreparationRequest) (*fallback.Job, error)
}
type Limits struct {
	JSONBytes, UploadBytes                               int64
	UploadReadTimeout, OperationTimeout, ShutdownTimeout time.Duration
	SSEClients, SSEPageSize, SSEFrameBytes               int
	SSEWriteTimeout, SSEHeartbeat                        time.Duration
}

func (l Limits) Validate() error {
	if l.JSONBytes <= 0 || l.UploadBytes <= 0 || l.UploadBytes == int64(^uint64(0)>>1) || l.UploadReadTimeout <= 0 || l.OperationTimeout <= 0 || l.ShutdownTimeout <= 0 || l.SSEClients <= 0 || l.SSEPageSize <= 0 || l.SSEPageSize > core.MaxPageLimit || l.SSEFrameBytes < 256 || l.SSEWriteTimeout <= 0 || l.SSEHeartbeat <= 0 {
		return core.ErrInvalid
	}
	return nil
}

type Config struct {
	Core         *core.Service
	Commands     Commands
	Fallback     Preparation
	Capabilities CapabilitiesProvider
	Logger       *slog.Logger
	Limits       Limits
}
type API struct {
	config          Config
	router          *chi.Mux
	huma            huma.API
	ctx             context.Context
	cancel          context.CancelFunc
	mu              sync.Mutex
	closed          bool
	active, streams int
	drained         chan struct{}
}

func New(config Config) (*API, error) {
	if config.Core == nil || config.Logger == nil {
		return nil, core.ErrInvalid
	}
	if err := config.Limits.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &API{config: config, router: chi.NewRouter(), ctx: ctx, cancel: cancel, drained: make(chan struct{})}
	hc := huma.DefaultConfig("Управление трансляциями", "1.0.0")
	hc.OpenAPIPath = ""
	hc.DocsPath = ""
	hc.SchemasPath = ""
	hc.CreateHooks = nil
	hc.RejectUnknownQueryParameters = true
	hc.Transformers = []huma.Transformer{func(ctx huma.Context, status string, v any) (any, error) {
		if op := ctx.Operation(); op != nil {
			if response := op.Responses[status]; response != nil {
				for _, media := range response.Content {
					if media.Schema != nil && strings.HasPrefix(media.Schema.Ref, "#/components/schemas/") {
						ctx.AppendHeader("Link", "<"+Prefix+"/schemas/"+path.Base(media.Schema.Ref)+".json>; rel=\"describedBy\"")
						break
					}
				}
			}
		}
		n, _ := strconv.Atoi(status)
		if n >= 400 {
			if problem, ok := v.(*trustedProfileProblem); ok && n == 422 {
				return problem, nil
			}
			return safeProblem(n), nil
		}
		return v, nil
	}}
	// Формат буферизует JSON до записи, чтобы ошибка кодирования не оставляла частичный ответ.
	format := huma.Format{Marshal: func(w io.Writer, v any) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}, Unmarshal: json.Unmarshal}
	hc.Formats = map[string]huma.Format{"application/json": format, "json": format}
	a.huma = humachi.New(a.router, hc)
	a.huma.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		state := &requestState{api: a}
		defer func() {
			if state.cleanup != nil {
				state.cleanup()
			}
		}()
		next(huma.WithContext(ctx, context.WithValue(ctx.Context(), apiContextKey{}, state)))
	})
	a.router.NotFound(func(w http.ResponseWriter, _ *http.Request) { writeProblem(w, http.StatusNotFound) })
	a.router.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) { writeProblem(w, http.StatusMethodNotAllowed) })
	a.registerConfiguration()
	a.registerCommands()
	a.registerFallback()
	a.registerRuntime()
	a.registerSystem()
	schemas := map[string][]byte{}
	for name, schema := range a.huma.OpenAPI().Components.Schemas.Map() {
		data, err := json.Marshal(schema)
		if err != nil {
			cancel()
			return nil, err
		}
		data = replaceSchemaRefs(data)
		schemas[name+".json"] = data
	}
	a.router.Get(Prefix+"/schemas/{schema}", func(w http.ResponseWriter, r *http.Request) {
		data, ok := schemas[chi.URLParam(r, "schema")]
		if !ok {
			writeProblem(w, 404)
			return
		}
		w.Header().Set("Content-Type", "application/schema+json")
		_, _ = w.Write(data)
	})
	spec, err := json.Marshal(a.huma.OpenAPI())
	if err != nil {
		cancel()
		return nil, err
	}
	yaml, err := a.huma.OpenAPI().YAML()
	if err != nil {
		cancel()
		return nil, err
	}
	a.router.Get(Prefix+"/openapi.json", immutable("application/openapi+json", spec))
	a.router.Get(Prefix+"/openapi.yaml", immutable("application/openapi+yaml", yaml))
	var docs bytes.Buffer
	docs.WriteString(`<!doctype html><html lang="ru"><meta charset="utf-8"><title>Управление трансляциями</title><h1>Управляющий API</h1><p><a href="` + Prefix + `/openapi.json">OpenAPI JSON</a> · <a href="` + Prefix + `/openapi.yaml">OpenAPI YAML</a></p><ul>`)
	paths := make([]string, 0, len(a.huma.OpenAPI().Paths))
	for path := range a.huma.OpenAPI().Paths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		item := a.huma.OpenAPI().Paths[path]
		for method, op := range map[string]*huma.Operation{"GET": item.Get, "POST": item.Post, "PUT": item.Put, "PATCH": item.Patch, "DELETE": item.Delete} {
			if op == nil {
				continue
			}
			docs.WriteString("<li><code>")
			docs.WriteString(html.EscapeString(method + " " + path))
			docs.WriteString("</code> — ")
			docs.WriteString(html.EscapeString(op.Summary))
			docs.WriteString("</li>")
		}
	}
	docs.WriteString("</ul><h2>Схемы</h2><ul>")
	names := make([]string, 0, len(schemas))
	for name := range schemas {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		docs.WriteString(`<li><a href="` + Prefix + `/schemas/`)
		docs.WriteString(html.EscapeString(name))
		docs.WriteString(`">`)
		docs.WriteString(html.EscapeString(name))
		docs.WriteString(`</a></li>`)
	}
	docs.WriteString("</ul></html>")
	a.router.Get(Prefix+"/docs", immutable("text/html; charset=utf-8", docs.Bytes()))
	return a, nil
}
func immutable(kind string, data []byte) http.HandlerFunc {
	data = bytes.Clone(data)
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", kind)
		_, _ = w.Write(data)
	}
}
func (a *API) Handler() http.Handler { return a }
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		writeProblem(w, 503)
		return
	}
	a.active++
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.active--
		if a.closed && a.active == 0 {
			close(a.drained)
		}
		a.mu.Unlock()
	}()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(a.ctx, cancel)
	defer cancel()
	defer stop()
	a.router.ServeHTTP(w, r.WithContext(ctx))
}
func (a *API) Close(ctx context.Context) error {
	a.mu.Lock()
	if !a.closed {
		a.closed = true
		a.cancel()
		if a.active == 0 {
			close(a.drained)
		}
	}
	a.mu.Unlock()
	shutdown, cancel := context.WithTimeout(context.Background(), a.config.Limits.ShutdownTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	select {
	case <-a.drained:
		return nil
	case <-shutdown.Done():
		return errors.Join(shutdown.Err(), ctx.Err())
	}
}
func (a *API) operation(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, a.config.Limits.OperationTimeout)
}
func (a *API) problem(err error) error {
	if err == nil {
		return nil
	}
	status := classify(err)
	a.config.Logger.Error("api_operation", "component", "api", "status", status, "error", err)
	var profileError *core.ProfileError
	if status == 422 && errors.As(err, &profileError) {
		return newProfileProblem(profileError.Code(), profileError.Field(), profileError.Detail())
	}
	var inputError profileInputError
	if status == 422 && errors.As(err, &inputError) {
		return newProfileProblem("invalid_profile", inputError.field, inputError.detail)
	}
	return huma.NewError(status, safeDetail(status))
}
func classify(err error) int {
	var max *http.MaxBytesError
	var transport transportError
	if errors.As(err, &transport) {
		return int(transport)
	}
	switch {
	case errors.As(err, &max), errors.Is(err, fallback.ErrLimit):
		return 413
	case errors.Is(err, core.ErrInvalidCursor):
		return 400
	case errors.Is(err, core.ErrNotFound):
		return 404
	case errors.Is(err, core.ErrConflict), errors.Is(err, core.ErrInUse), errors.Is(err, core.ErrNotReady), errors.Is(err, core.ErrStale):
		return 409
	case errors.Is(err, core.ErrInvalid), errors.Is(err, core.ErrInvalidProfile), errors.Is(err, core.ErrInvalidFormat), errors.Is(err, core.ErrInvalidWatchdogPolicy), errors.Is(err, fallback.ErrInvalid):
		return 422
	case errors.Is(err, core.ErrClosed), errors.Is(err, core.ErrBusy), errors.Is(err, core.ErrRuntimeCapability), errors.Is(err, core.ErrObservationLimit), errors.Is(err, core.ErrEventSequenceExhausted):
		return 503
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return 503
	default:
		return 500
	}
}
func safeDetail(status int) string {
	switch status {
	case 400:
		return "Некорректный запрос."
	case 404:
		return "Объект не найден."
	case 405:
		return "Метод не поддерживается."
	case 408:
		return "Истекло время передачи файла."
	case 409:
		return "Операция конфликтует с текущим состоянием или версией."
	case 413:
		return "Превышен допустимый размер тела запроса."
	case 415:
		return "Тип содержимого не поддерживается."
	case 422:
		return "Параметры запроса недопустимы."
	case 503:
		return "Операция временно недоступна."
	default:
		return "Внутренняя ошибка сервиса."
	}
}
func safeProblem(status int) *huma.ErrorModel {
	return &huma.ErrorModel{Type: "about:blank", Title: http.StatusText(status), Status: status, Detail: safeDetail(status)}
}

type ProfileProblemDTO struct {
	huma.ErrorModel
	Code  string `json:"code,omitempty" doc:"Стабильный код проверенной ошибки профиля."`
	Field string `json:"field,omitempty" doc:"Публичный путь к недопустимому полю профиля."`
}

// Обёртка отделяет проверенную доменную ошибку от произвольных ошибок Huma.
type trustedProfileProblem struct{ ProfileProblemDTO }

func newProfileProblem(code, field, detail string) *trustedProfileProblem {
	return &trustedProfileProblem{ProfileProblemDTO{ErrorModel: huma.ErrorModel{Type: "about:blank", Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity, Detail: detail}, Code: code, Field: field}}
}
func writeProblem(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(safeProblem(status))
}
func operation(method, path, id, summary string) huma.Operation {
	return huma.Operation{Method: method, Path: Prefix + path, OperationID: id, Summary: summary, Errors: []int{400, 404, 408, 409, 413, 415, 422, 503}}
}
func register[I, O any](a *API, op huma.Operation, handler func(context.Context, *I) (*O, error)) {
	var profileErrorResponse *huma.Response
	if op.OperationID == "create-profile" || op.OperationID == "update-profile" {
		if op.Responses == nil {
			op.Responses = map[string]*huma.Response{}
		}
		profileErrorResponse = &huma.Response{Description: "Параметры профиля недопустимы.", Content: map[string]*huma.MediaType{"application/problem+json": {Schema: a.huma.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[ProfileProblemDTO](), true, "")}}}
	}
	op.MaxBodyBytes = a.config.Limits.JSONBytes
	op.BodyReadTimeout = a.config.Limits.OperationTimeout
	huma.Register(a.huma, op, func(ctx context.Context, in *I) (*O, error) {
		ctx, cancel := a.operation(ctx)
		defer cancel()
		out, err := handler(ctx, in)
		if err == nil && out != nil {
			if _, encodeErr := json.Marshal(out); encodeErr != nil {
				err = encodeErr
			}
		}
		return out, a.problem(err)
	})
	if profileErrorResponse != nil {
		// Huma добавляет стандартную схему ошибок при регистрации, поэтому уточняем её после этого шага.
		op.Responses["422"] = profileErrorResponse
	}
}

type transportError int

func (e transportError) Error() string { return safeDetail(int(e)) }
func statusError(status int) error     { return transportError(status) }
func badRequest() error                { return statusError(400) }
func unavailable() error               { return statusError(503) }
func pageOne() core.PageRequest        { return core.PageRequest{Query: core.ListQuery{Limit: 1}} }

func replaceSchemaRefs(data []byte) []byte {
	var schema any
	if json.Unmarshal(data, &schema) != nil {
		return data
	}
	var rewrite func(any)
	rewrite = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				if key == "$ref" {
					if ref, ok := child.(string); ok && strings.HasPrefix(ref, "#/components/schemas/") {
						v[key] = Prefix + "/schemas/" + strings.TrimPrefix(ref, "#/components/schemas/") + ".json"
						continue
					}
				}
				rewrite(child)
			}
		case []any:
			for _, child := range v {
				rewrite(child)
			}
		}
	}
	rewrite(schema)
	result, err := json.Marshal(schema)
	if err != nil {
		return data
	}
	return result
}
