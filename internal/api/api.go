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

// Prefix задаёт единый публичный префикс REST, SSE, спецификаций и схем управляющего API.
const Prefix = "/api/v1"

// Commands принимает управляющие команды через владельца сессий и возвращает долговечные квитанции их
// принятия.
type Commands interface {

	// StartSession долговечно принимает запуск потока и возвращает квитанцию; её получение не подтверждает
	// готовность медиатракта. Повтор смыслового запроса с тем же RequestID возвращает прежний результат.
	StartSession(context.Context, core.StartSessionRequest) (core.CommandReceipt, error)

	// StopSession принимает остановку Session и возвращает долговечную квитанцию для отдельного чтения
	// результата.
	StopSession(context.Context, core.SessionCommand) (core.CommandReceipt, error)

	// PauseSession принимает ручную паузу Session без превращения принятия команды в подтверждение
	// завершения переключения.
	PauseSession(context.Context, core.SessionCommand) (core.CommandReceipt, error)

	// ResumeSession принимает возобновление Session и возвращает квитанцию выполнения команды.
	ResumeSession(context.Context, core.SessionCommand) (core.CommandReceipt, error)

	// AddOutput принимает присоединение назначения с новым самостоятельным снимком настроек и возвращает
	// квитанцию.
	AddOutput(context.Context, core.AddOutputRequest) (core.CommandReceipt, error)

	// StartOutput принимает повторный запуск прикреплённого выхода с его сохранённым снимком и возвращает
	// квитанцию.
	StartOutput(context.Context, core.OutputCommand) (core.CommandReceipt, error)

	// StopOutput принимает остановку выхода, сохраняя прикрепление и возможность следующего запуска с
	// прежним снимком.
	StopOutput(context.Context, core.OutputCommand) (core.CommandReceipt, error)

	// DetachOutput принимает отсоединение выхода, после которого прежний выход больше не запускается;
	// освобождение зависимостей требует подтверждения очистки.
	DetachOutput(context.Context, core.OutputCommand) (core.CommandReceipt, error)
}

// Preparation передаёт загрузку и повторную подготовку заглушок отдельному владельцу заданий.
type Preparation interface {

	// Upload принимает задание и после успешного допуска передаёт источник байтов во владение подготовки.
	// HTTP ждёт Job.WaitUpload до подтверждения передачи; дальнейшая подготовка имеет собственного
	// владельца и срок.
	Upload(context.Context, core.PreparationRequest, io.ReadCloser) (*fallback.Job, error)

	// Rebuild резервирует повторную подготовку уже сохранённого оригинала и возвращает задание без новой
	// HTTP-передачи файла.
	Rebuild(context.Context, core.PreparationRequest) (*fallback.Job, error)
}

// Limits задаёт конечные размеры, сроки и число одновременно обслуживаемых SSE-подписок.
type Limits struct {
	// JSONBytes ограничивает тело типизированного JSON-запроса в байтах; UploadBytes ограничивает
	// потоковое тело файла заглушки в байтах.
	JSONBytes, UploadBytes int64
	// UploadReadTimeout ограничивает передачу файла; OperationTimeout ограничивает управляющую
	// операцию; ShutdownTimeout ограничивает ожидание обработчиков при Close.
	UploadReadTimeout, OperationTimeout, ShutdownTimeout time.Duration
	// SSEClients ограничивает число подписок; SSEPageSize задаёт число событий за чтение;
	// SSEFrameBytes ограничивает размер одного кадра в байтах.
	SSEClients, SSEPageSize, SSEFrameBytes int
	// SSEWriteTimeout ограничивает запись очередного SSE-кадра; SSEHeartbeat задаёт период служебных
	// кадров поддержания подписки.
	SSEWriteTimeout, SSEHeartbeat time.Duration
}

// Validate проверяет положительные бюджеты HTTP, загрузки файлов и SSE, допустимый размер страницы
// событий и представимость предела тела загрузки.
func (l Limits) Validate() error {
	if l.JSONBytes <= 0 || l.UploadBytes <= 0 || l.UploadBytes == int64(^uint64(0)>>1) || l.UploadReadTimeout <= 0 || l.OperationTimeout <= 0 || l.ShutdownTimeout <= 0 || l.SSEClients <= 0 || l.SSEPageSize <= 0 || l.SSEPageSize > core.MaxPageLimit || l.SSEFrameBytes < 256 || l.SSEWriteTimeout <= 0 || l.SSEHeartbeat <= 0 {
		return core.ErrInvalid
	}
	return nil
}

// Config связывает HTTP-слой с доменными сервисами, владельцами операций, журналом и явными
// ограничениями.
type Config struct {
	// Core предоставляет доменные операции и согласованное чтение оперативных наблюдений.
	Core *core.Service
	// Commands передаёт асинхронные команды владельцу сессий; отсутствие зависимости обрабатывается
	// маршрутами команд.
	Commands Commands
	// Fallback передаёт подготовку заглушок владельцу заданий; nil делает соответствующие операции
	// недоступными.
	Fallback Preparation
	// Capabilities предоставляет снимок возможностей серверной среды для системного маршрута.
	Capabilities CapabilitiesProvider
	// Logger получает исходные ошибки операций перед преобразованием в безопасные публичные ответы.
	Logger *slog.Logger
	// Limits содержит проверенные бюджеты запросов, загрузок, SSE и остановки обработчиков.
	Limits Limits
}

// API связывает маршруты Huma/Chi и учитывает запросы, чтобы закрыть допуск и дождаться обработчиков.
type API struct {
	// config сохраняет зависимости и ограничения на срок жизни HTTP API.
	config Config
	// router обслуживает зарегистрированные REST, SSE и подготовленные статические документы.
	router *chi.Mux
	// huma хранит типизированную регистрацию операций и общую спецификацию их схем.
	huma huma.API
	// ctx отменяется при закрытии API и распространяет остановку на принятые запросы.
	ctx context.Context
	// cancel отменяет общий контекст API при первом закрытии допуска.
	cancel context.CancelFunc
	// mu защищает закрытие допуска и счётчики запросов и SSE-подписок.
	mu sync.Mutex
	// closed запрещает принимать новые запросы и новые SSE-подписки после начала Close.
	closed bool
	// active учитывает все принятые HTTP-обработчики; streams учитывает занятые места подписчиков
	// SSE.
	active, streams int
	// drained закрывается после ухода последнего принятого обработчика при закрытом допуске.
	drained chan struct{}
}

// New связывает маршрутизатор с доменом и заданными владельцами команд и подготовки. После регистрации
// он однократно формирует неизменяемые спецификации, схемы и документацию; обработчики и SSE затем
// завершает Close.
func New(config Config) (*API, error) {
	if config.Core == nil || config.Logger == nil {
		return nil, core.ErrInvalid
	}
	// err содержит отказ проверки HTTP-бюджетов до создания маршрутизатора и контекста API.
	if err := config.Limits.Validate(); err != nil {
		return nil, err
	}
	// ctx задаёт общий срок жизни обработчиков API; cancel прекращает его при Close или ошибке
	// регистрации.
	ctx, cancel := context.WithCancel(context.Background())
	// a содержит создаваемый API с общим маршрутизатором, зависимостями и сигналом завершения
	// обработчиков.
	a := &API{config: config, router: chi.NewRouter(), ctx: ctx, cancel: cancel, drained: make(chan struct{})}
	// hc задаёт конфигурацию Huma с собственными статическими маршрутами спецификаций и безопасным
	// преобразованием ошибок.
	hc := huma.DefaultConfig("Управление трансляциями", "1.0.0")
	hc.OpenAPIPath = ""
	hc.DocsPath = ""
	hc.SchemasPath = ""
	hc.CreateHooks = nil
	hc.RejectUnknownQueryParameters = true
	hc.Transformers = []huma.Transformer{func(ctx huma.Context, status string, v any) (any, error) {
		// Добавляет ссылку на схему ответа и заменяет ошибки безопасным представлением, сохраняя проверенные
		// детали ошибок профиля для статуса 422.

		// op содержит операцию текущего ответа для поиска его схемы.
		if op := ctx.Operation(); op != nil {
			// response содержит описание текущего статуса, из которого выбирается схема для
			// заголовка Link.
			if response := op.Responses[status]; response != nil {
				// media содержит очередное представление ответа при поиске ссылки на
				// публичную схему.
				for _, media := range response.Content {
					if media.Schema != nil && strings.HasPrefix(media.Schema.Ref, "#/components/schemas/") {
						ctx.AppendHeader("Link", "<"+Prefix+"/schemas/"+path.Base(media.Schema.Ref)+".json>; rel=\"describedBy\"")
						break
					}
				}
			}
		}
		// n содержит численный HTTP-статус для применения безопасного преобразования ошибок.
		n, _ := strconv.Atoi(status)
		if n >= 400 {
			// problem содержит доверенную ошибку профиля; ok подтверждает её тип для
			// сохранения деталей только при 422.
			if problem, ok := v.(*trustedProfileProblem); ok && n == 422 {
				return problem, nil
			}
			// problem содержит стандартную ошибку Huma; ok подтверждает её тип перед
			// проверкой ошибки live_audio_mode.
			if problem, ok := v.(*huma.ErrorModel); ok && n == 422 {
				// op содержит текущую операцию для ограничения специальной ошибки
				// режимов создания и изменения профиля.
				if op := ctx.Operation(); op != nil && (op.OperationID == "create-profile" || op.OperationID == "update-profile") {
					// detail содержит очередное нарушение валидации Huma при поиске
					// поля profile.live_audio_mode.
					for _, detail := range problem.Errors {
						if detail != nil && detail.Location == "body.profile.live_audio_mode" {
							return newProfileProblem("invalid_profile", "profile.live_audio_mode", "Требуется явное значение copy или transcode."), nil
						}
					}
				}
			}
			return safeProblem(n), nil
		}
		return v, nil
	}}
	// Формат буферизует JSON до записи, чтобы ошибка кодирования не оставляла частичный ответ.
	// format буферизует сериализацию JSON до записи ответа, исключая частичный JSON при ошибке
	// кодирования.
	format := huma.Format{Marshal: func(w io.Writer, v any) error {
		// Кодирует весь JSON до записи, чтобы ошибка сериализации не оставила частичный ответ.

		// data содержит полностью сериализованный ответ; err содержит отказ JSON-кодирования до
		// записи в транспорт.
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
		// Передаёт обработчику состояние запроса и освобождает закреплённые за ним ресурсы после возврата.

		// state сохраняет владельца API и освобождение ресурсов текущего запроса в его
		// контексте.
		state := &requestState{api: a}
		defer func() {
			// Вызывает зарегистрированное освобождение ресурсов запроса, если обработчик успел его установить.

			if state.cleanup != nil {
				state.cleanup()
			}
		}()
		next(huma.WithContext(ctx, context.WithValue(ctx.Context(), apiContextKey{}, state)))
	})
	a.router.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		// Возвращает безопасный ответ 404 для незарегистрированного маршрута.
		writeProblem(w, http.StatusNotFound)
	})
	a.router.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		// Возвращает безопасный ответ 405 при неподдерживаемом методе известного маршрута.
		writeProblem(w, http.StatusMethodNotAllowed)
	})
	a.registerConfiguration()
	a.registerCommands()
	a.registerFallback()
	a.registerRuntime()
	a.registerSystem()
	// schemas хранит подготовленные JSON-байты публичных схем по имени файла для дальнейшего
	// неизменяемого чтения.
	schemas := map[string][]byte{}
	// name задаёт имя компонента OpenAPI; schema содержит его схему для однократной сериализации.
	for name, schema := range a.huma.OpenAPI().Components.Schemas.Map() {
		// data содержит сериализованную схему; err содержит отказ подготовки её байтов при
		// создании API.
		data, err := json.Marshal(schema)
		if err != nil {
			cancel()
			return nil, err
		}
		data = replaceSchemaRefs(data)
		schemas[name+".json"] = data
	}
	a.router.Get(Prefix+"/schemas/{schema}", func(w http.ResponseWriter, r *http.Request) {
		// Выдаёт подготовленные при регистрации байты схемы или безопасный ответ 404 для неизвестного имени.

		// data содержит подготовленные байты запрошенной схемы; ok отмечает наличие такого
		// публичного имени.
		data, ok := schemas[chi.URLParam(r, "schema")]
		if !ok {
			writeProblem(w, 404)
			return
		}
		w.Header().Set("Content-Type", "application/schema+json")
		_, _ = w.Write(data)
	})
	// spec содержит подготовленный OpenAPI JSON; err содержит отказ его сериализации до публикации
	// маршрута.
	spec, err := json.Marshal(a.huma.OpenAPI())
	if err != nil {
		cancel()
		return nil, err
	}
	// yaml содержит подготовленную спецификацию OpenAPI YAML.
	yaml, err := a.huma.OpenAPI().YAML()
	if err != nil {
		cancel()
		return nil, err
	}
	a.router.Get(Prefix+"/openapi.json", immutable("application/openapi+json", spec))
	a.router.Get(Prefix+"/openapi.yaml", immutable("application/openapi+yaml", yaml))
	// docs накапливает локальную HTML-документацию из зарегистрированных путей и публичных схем.
	var docs bytes.Buffer
	docs.WriteString(`<!doctype html><html lang="ru"><meta charset="utf-8"><title>Управление трансляциями</title><h1>Управляющий API</h1><p><a href="` + Prefix + `/openapi.json">OpenAPI JSON</a> · <a href="` + Prefix + `/openapi.yaml">OpenAPI YAML</a></p><ul>`)
	// paths собирает пути OpenAPI для упорядоченного вывода в HTML-документации.
	paths := make([]string, 0, len(a.huma.OpenAPI().Paths))
	// path содержит очередной ключ пути OpenAPI, добавляемый в сортируемый список.
	for path := range a.huma.OpenAPI().Paths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	// path содержит очередной отсортированный путь для вывода его операций.
	for _, path := range paths {
		// item содержит операции OpenAPI текущего публичного пути.
		item := a.huma.OpenAPI().Paths[path]
		// method задаёт HTTP-метод строки документации; op содержит соответствующую
		// зарегистрированную операцию либо nil.
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
	// names собирает имена подготовленных схем для упорядоченного вывода ссылок.
	names := make([]string, 0, len(schemas))
	// name содержит очередное имя подготовленной схемы, добавляемое в сортируемый список.
	for name := range schemas {
		names = append(names, name)
	}
	sort.Strings(names)
	// name содержит очередное отсортированное имя схемы для HTML-ссылки.
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

// immutable копирует байты опубликованного ресурса и создаёт обработчик, возвращающий их с
// фиксированным Content-Type.
func immutable(kind string, data []byte) http.HandlerFunc {
	data = bytes.Clone(data)
	return func(w http.ResponseWriter, _ *http.Request) {
		// Записывает сохранённую копию документа с фиксированным типом содержимого.

		w.Header().Set("Content-Type", kind)
		_, _ = w.Write(data)
	}
}

// Handler возвращает общий HTTP-обработчик API с учётом активных запросов и закрытия допуска.
func (a *API) Handler() http.Handler { return a }

// ServeHTTP отклоняет новые запросы после Close и учитывает все принятые обработчики до их возврата.
// Контекст каждого запроса отменяется также при закрытии API.
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
		// Уменьшает число активных запросов под блокировкой и сообщает Close об уходе последнего обработчика.

		a.mu.Lock()
		a.active--
		if a.closed && a.active == 0 {
			close(a.drained)
		}
		a.mu.Unlock()
	}()
	// ctx соединяет отмену исходного запроса с закрытием API; cancel освобождает дочерний контекст
	// после возврата обработчика.
	ctx, cancel := context.WithCancel(r.Context())
	// stop снимает связь с общей отменой API после завершения текущего запроса.
	stop := context.AfterFunc(a.ctx, cancel)
	defer cancel()
	defer stop()
	a.router.ServeHTTP(w, r.WithContext(ctx))
}

// Close закрывает допуск, отменяет обработчики и ограниченно ждёт их завершения. Он не закрывает общий
// Store и не заменяет остановку транспорта http.Server.
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
	// shutdown ограничивает ожидание обработчиков независимым сроком; cancel освобождает таймер и
	// связывается с отменой вызывающего Close.
	shutdown, cancel := context.WithTimeout(context.Background(), a.config.Limits.ShutdownTimeout)
	defer cancel()
	// stop снимает обработчик отмены вызывающего контекста после окончания Close.
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	select {
	case <-a.drained:
		return nil
	case <-shutdown.Done():
		return errors.Join(shutdown.Err(), ctx.Err())
	}
}

// operation создаёт дочерний контекст с пределом времени одной управляющей операции; вызывающий код
// освобождает его cancel.
func (a *API) operation(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, a.config.Limits.OperationTimeout)
}

// problem журналирует исходную ошибку и возвращает безопасное HTTP-представление. Только проверенные
// ошибки профиля сохраняют подготовленные code, field и detail.
func (a *API) problem(err error) error {
	if err == nil {
		return nil
	}
	// status содержит выбранный HTTP-статус исходной ошибки для журнала и публичного ответа.
	status := classify(err)
	a.config.Logger.Error("api_operation", "component", "api", "status", status, "error", err)
	// profileError принимает проверенную доменную ошибку профиля для сохранения её публичных деталей
	// при 422.
	var profileError *core.ProfileError
	if status == 422 && errors.As(err, &profileError) {
		return newProfileProblem(profileError.Code(), profileError.Field(), profileError.Detail())
	}
	// inputError принимает проверенную ошибку преобразования входного профиля для безопасных code,
	// field и detail.
	var inputError profileInputError
	if status == 422 && errors.As(err, &inputError) {
		return newProfileProblem("invalid_profile", inputError.field, inputError.detail)
	}
	return huma.NewError(status, safeDetail(status))
}

// classify отображает ошибки домена и транспорта в HTTP-статусы с установленным порядком приоритетов
// для объединённых ошибок.
func classify(err error) int {
	// max принимает ошибку превышения тела HTTP для отображения в статус 413.
	var max *http.MaxBytesError
	// transport содержит заранее выбранный транспортный статус; ok отмечает наличие такой ошибки в
	// цепочке.
	if transport, ok := errors.AsType[transportError](err); ok {
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

// safeDetail возвращает подготовленное объяснение HTTP-ошибки без исходных значений запроса и
// диагностики.
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

// safeProblem формирует Problem Details для статуса с безопасным текстом и без подробностей исходной
// ошибки.
func safeProblem(status int) *huma.ErrorModel {
	return &huma.ErrorModel{Type: "about:blank", Title: http.StatusText(status), Status: status, Detail: safeDetail(status)}
}

// ProfileProblemDTO дополняет Problem Details стабильным кодом и публичным путём ошибочного поля
// профиля.
type ProfileProblemDTO struct {
	// ErrorModel содержит стандартные поля Problem Details с безопасными описаниями ошибки профиля.
	huma.ErrorModel
	Code  string `json:"code,omitempty" doc:"Стабильный код проверенной ошибки профиля."`
	Field string `json:"field,omitempty" doc:"Публичный путь к недопустимому полю профиля."`
}

// Обёртка отделяет проверенную доменную ошибку от произвольных ошибок Huma.
type trustedProfileProblem struct { /* ProfileProblemDTO содержит подготовленные проверенные детали, которым разрешено пройти преобразователь ответа. */
	ProfileProblemDTO
}

// newProfileProblem оборачивает проверенные поля ошибки профиля в отдельный доверенный тип,
// допускаемый преобразователем ответа.
func newProfileProblem(code, field, detail string) *trustedProfileProblem {
	return &trustedProfileProblem{ProfileProblemDTO{Type: "about:blank", Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity, Detail: detail, Code: code, Field: field}}
}

// writeProblem записывает безопасный Problem Details с заданным HTTP-статусом и Content-Type
// application/problem+json.
func writeProblem(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(safeProblem(status))
}

// operation задаёт общий префикс, идентификатор и объявленные ошибки типизированной HTTP-операции.
func operation(method, path, id, summary string) huma.Operation {
	return huma.Operation{Method: method, Path: Prefix + path, OperationID: id, Summary: summary, Errors: []int{400, 404, 408, 409, 413, 415, 422, 503}}
}

// register добавляет ограничение тела и срока чтения, контекст операции и безопасное преобразование
// ошибок. Ответ проверяется на представимость в JSON до записи; схема 422 профиля уточняется после
// регистрации Huma.
func register[I, O any](a *API, op huma.Operation, handler func(context.Context, *I) (*O, error)) {
	// profileErrorResponse хранит специальную схему ответа 422 для операций создания и изменения
	// профиля.
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
		// Ограничивает время операции, проверяет сериализацию результата и переводит ошибку обработчика в
		// публичный ответ.

		// cancel завершает контекст текущей управляющей операции после возврата обработчика.
		ctx, cancel := a.operation(ctx)
		defer cancel()
		// out содержит результат типизированного обработчика; err содержит его отказ или
		// последующую ошибку сериализации результата.
		out, err := handler(ctx, in)
		if err == nil && out != nil {
			// encodeErr содержит отказ пробной сериализации ответа до начала его записи
			// клиенту.
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

// transportError хранит HTTP-статус, уже выбранный проверкой транспортной границы.
type transportError int

// Error возвращает безопасное описание HTTP-статуса транспортной ошибки.
func (e transportError) Error() string { return safeDetail(int(e)) }

// statusError создаёт ошибку с уже выбранным HTTP-статусом для транспортных проверок.
func statusError(status int) error { return transportError(status) }

// badRequest обозначает некорректную структуру запроса или курсора HTTP-статусом 400.
func badRequest() error { return statusError(400) }

// unavailable обозначает временную недоступность управляющей операции HTTP-статусом 503.
func unavailable() error { return statusError(503) }

// pageOne создаёт запрос одной записи для ограниченной проверки доступности хранилища.
func pageOne() core.PageRequest { return core.PageRequest{Query: core.ListQuery{Limit: 1}} }

// replaceSchemaRefs переводит внутренние ссылки компонентов OpenAPI в публичные URL отдельных схем.
// При невозможности разбора или кодирования сохраняет исходные байты.
func replaceSchemaRefs(data []byte) []byte {
	// schema содержит разобранное JSON-дерево схемы для замены внутренних ссылок.
	var schema any
	if json.Unmarshal(data, &schema) != nil {
		return data
	}
	// rewrite рекурсивно обходит объекты и массивы схемы, меняя только внутренние ссылки $ref.
	var rewrite func(any)
	rewrite = func(value any) {
		// Рекурсивно заменяет внутренние ссылки $ref адресами опубликованных схем, сохраняя прочие значения.

		// v содержит текущее значение JSON-дерева в конкретном виде объекта или массива.
		switch v := value.(type) {
		case map[string]any:
			// key содержит имя свойства JSON-объекта; child содержит его значение для
			// проверки ссылки или дальнейшего обхода.
			for key, child := range v {
				if key == "$ref" {
					// ref содержит строковое значение $ref; ok отличает строку от
					// иных JSON-значений.
					if ref, ok := child.(string); ok && strings.HasPrefix(ref, "#/components/schemas/") {
						v[key] = Prefix + "/schemas/" + strings.TrimPrefix(ref, "#/components/schemas/") + ".json"
						continue
					}
				}
				rewrite(child)
			}
		case []any:
			// child содержит очередной элемент массива схемы, передаваемый рекурсивному
			// обходу.
			for _, child := range v {
				rewrite(child)
			}
		}
	}
	rewrite(schema)
	// result содержит JSON после замены ссылок; err содержит отказ повторной сериализации, при
	// котором сохраняются исходные байты.
	result, err := json.Marshal(schema)
	if err != nil {
		return data
	}
	return result
}
