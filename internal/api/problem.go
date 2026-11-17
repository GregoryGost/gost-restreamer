package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/danielgtaylor/huma/v2"
)

// ProblemContextDTO содержит только известные публичные идентификаторы из проверенных доменных записей.
// Значения запроса, URL, пути и внутренние признаки исполнения сюда не переносятся.
type ProblemContextDTO struct {
	// StreamID указывает прочитанный поток.
	StreamID string `json:"stream_id,omitempty"`
	// SessionID указывает сохранённый запуск.
	SessionID string `json:"session_id,omitempty"`
	// OutputID указывает сохранённый выход.
	OutputID string `json:"output_id,omitempty"`
	// GroupID указывает выбранную сохранённую группу.
	GroupID string `json:"group_id,omitempty"`
	// ProfileID указывает сохранённый профиль группы.
	ProfileID string `json:"profile_id,omitempty"`
	// AssetID указывает прочитанную заглушку.
	AssetID string `json:"asset_id,omitempty"`
	// GenerationID указывает прочитанное поколение заглушки.
	GenerationID string `json:"generation_id,omitempty"`
	// VariantID указывает закреплённый вариант заглушки.
	VariantID string `json:"variant_id,omitempty"`
	// DestinationID указывает сохранённое назначение.
	DestinationID string `json:"destination_id,omitempty"`
	// CommandID указывает созданную сервером квитанцию.
	CommandID string `json:"command_id,omitempty"`
}

// ProblemDTO расширяет стандартный Problem Details безопасной причиной и отдельной подсказкой.
type ProblemDTO struct {
	// ErrorModel сохраняет стандартные поля Problem Details и механизм Content-Type Huma.
	huma.ErrorModel
	// Code содержит стабильный машинный код проверенной причины.
	Code string `json:"code" doc:"Стабильный машинный код причины отказа."`
	// Action содержит подготовленный следующий шаг, когда он достоверно известен.
	Action string `json:"action,omitempty" doc:"Подготовленный следующий шаг; отсутствует, если достоверного действия нет."`
	// Field содержит только путь объявленного публичного поля.
	Field string `json:"field,omitempty"`
	// Context содержит известные идентификаторы; при отсутствии контекста поле опускается.
	Context *ProblemContextDTO `json:"context,omitempty" doc:"Известные публичные идентификаторы из проверенных записей и снимков."`
}

// ProfileProblemDTO сохраняет публичное имя схемы подробных ошибок профиля.
type ProfileProblemDTO struct {
	// ErrorModel сохраняет стандартные поля прежнего профильного Problem Details.
	huma.ErrorModel
	// Code сохраняет прежний закрытый каталог проверенных ошибок профиля.
	Code string `json:"code,omitempty" doc:"Стабильный код проверенной ошибки профиля." enum:"invalid_profile,invalid_custom,duplicate_custom,custom_conflict,unknown_custom"`
	// Field сохраняет необязательный публичный путь недопустимой настройки.
	Field string `json:"field,omitempty" doc:"Публичный путь к недопустимому полю профиля."`
	// Action предлагает отдельное подготовленное исправление параметров профиля.
	Action string `json:"action,omitempty" doc:"Подготовленный следующий шаг; отсутствует, если достоверного действия нет."`
	// Context содержит только проверенные публичные идентификаторы, когда они известны.
	Context *ProblemContextDTO `json:"context,omitempty" doc:"Известные публичные идентификаторы из проверенных записей и снимков."`
}

// trustedProblem отделяет собственное подготовленное представление от произвольного ErrorModel.
type trustedProblem struct {
	// ProblemDTO содержит только значения, выбранные каталогом API.
	ProblemDTO
}

// trustedProfileProblem сохраняет прежний доверенный путь профильных ошибок 422.
type trustedProfileProblem struct {
	// ProfileProblemDTO содержит проверенные code, field и detail профиля.
	ProfileProblemDTO
}

// problemText связывает машинную причину с русским объяснением и отдельной подсказкой.
type problemText struct {
	// detail объясняет проверенный отказ; action предлагает достоверный следующий шаг.
	detail, action string
}

// catalogText возвращает подготовленные тексты; неизвестные коды не становятся публичной диагностикой.
func catalogText(code string) (problemText, bool) {
	// text содержит выбранные статические причину и действие.
	var text problemText
	switch code {
	case "fallback_not_configured":
		text = problemText{"Для запуска не настроена политика заглушки.", "Выберите заглушку в политике потока или назначения."}
	case "fallback_not_prepared":
		text = problemText{"У выбранной заглушки нет опубликованного готового поколения.", "Завершите загрузку и подготовку заглушки."}
	case "fallback_profile_not_prepared":
		text = problemText{"Заглушка не подготовлена для точного сохранённого профиля группы.", "Повторно подготовьте заглушку для сохранённого профиля и дождитесь завершения подготовки."}
	case "fallback_variant_not_ready":
		text = problemText{"Закреплённый вариант заглушки непригоден для запуска.", "Проверьте состояние закреплённого варианта и журнал приложения."}
	case "preparation_active":
		text = problemText{"Запуск запрещён во время активной подготовки заглушки.", "Дождитесь завершения подготовки."}
	case "session_active":
		text = problemText{"У потока уже есть активный запуск.", "Проверьте текущую сессию потока."}
	case "request_id_conflict":
		text = problemText{"Этот request_id уже использован для другой команды.", "Для новой команды используйте новый request_id; прежнюю команду проверяйте по её квитанции."}
	case "revision_conflict":
		text = problemText{"Ожидаемая ревизия не совпадает с текущей ревизией объекта.", "Прочитайте объект заново и используйте его текущую ревизию."}
	case "session_state_conflict":
		text = problemText{"Текущее состояние сессии не допускает эту команду.", "Прочитайте состояние сессии перед следующей командой."}
	case "output_detached":
		text = problemText{"Выход уже отсоединён от сессии.", "Добавьте назначение к сессии для создания нового выхода."}
	case "output_already_running":
		text = problemText{"Для выхода уже задано работающее состояние.", "Проверьте состояние выхода и результат предыдущей команды."}
	case "output_cleanup_pending":
		text = problemText{"Очистка предыдущего исполнения выхода ещё не подтверждена.", "Дождитесь завершения очистки выхода."}
	case "destination_attached":
		text = problemText{"Назначение уже прикреплено к этой сессии.", "Используйте существующий выход назначения."}
	case "resource_in_use":
		text = problemText{"Объект используется и не допускает запрошенного изменения.", "Проверьте связанные объекты и активные сессии."}
	case "resource_ownership_conflict":
		text = problemText{"Объекты принадлежат разным потокам или сессиям.", "Выберите связанные объекты одного потока или сессии."}
	case "session_start_failed":
		text = problemText{"Не удалось выполнить запуск сессии.", "Проверьте журнал приложения перед новым запуском."}
	case "output_start_failed":
		text = problemText{"Не удалось выполнить запуск выхода.", "Проверьте журнал приложения и состояние выхода."}
	case "session_stop_failed":
		text = problemText{"Не удалось завершить остановку сессии.", "Проверьте состояние сессии и журнал приложения."}
	case "output_stop_failed":
		text = problemText{"Не удалось завершить остановку выхода.", "Проверьте состояние выхода и журнал приложения."}
	case "session_pause_failed":
		text = problemText{"Не удалось выполнить ручную паузу сессии.", "Проверьте состояние сессии и журнал приложения."}
	case "session_resume_failed":
		text = problemText{"Не удалось выполнить возобновление сессии.", "Проверьте состояние сессии и журнал приложения."}
	case "execution_interrupted":
		text = problemText{"Исполнение прервано при смене владельца управления.", "Проверьте текущее состояние сессии и квитанцию команды."}
	case "command_superseded":
		text = problemText{"Команда прервана последующим изменением управления.", "Проверьте текущую сессию и результат последующей команды."}
	case "empty_upload":
		text = problemText{"Передан пустой файл заглушки.", "Передайте непустой файл изображения или видео."}
	case "upload_canceled":
		text = problemText{"Передача файла заглушки отменена.", "Проверьте состояние поколения заглушки перед новой загрузкой."}
	case "invalid_time":
		text = problemText{"Временная граница списка имеет недопустимый формат.", "Передайте время в формате RFC3339Nano."}
	case "invalid_json":
		text = problemText{"Тело запроса не содержит допустимый JSON требуемой структуры.", "Проверьте JSON и схему тела операции."}
	case "invalid_request":
		text = problemText{"Некорректная структура запроса.", "Проверьте параметры операции по её схеме."}
	case "invalid_cursor":
		text = problemText{"Курсор страницы или событий недопустим.", "Начните чтение без курсора и используйте полученную новую позицию."}
	case "sse_cursor_conflict":
		text = problemText{"Позиции cursor и Last-Event-ID различаются.", "Передайте одну позицию или одинаковые значения в обоих полях."}
	case "resource_not_found":
		text = problemText{"Объект не найден.", "Проверьте тип и идентификатор запрашиваемого объекта."}
	case "nested_resource_not_found":
		text = problemText{"Вложенный объект не найден у указанного родителя.", "Проверьте список объектов родительского ресурса."}
	case "route_not_found":
		text = problemText{"Маршрут API не найден.", "Проверьте путь по спецификации API."}
	case "schema_not_found":
		text = problemText{"Публичная схема не найдена.", "Используйте ссылку на схему из спецификации API."}
	case "method_not_allowed":
		text = problemText{"Метод не поддерживается для этого маршрута.", "Используйте метод, объявленный в спецификации API."}
	case "body_read_timeout":
		text = problemText{"Истекло время чтения тела запроса.", "Проверьте передачу тела запроса клиентом."}
	case "upload_timeout":
		text = problemText{"Истекло время передачи файла заглушки.", "Проверьте соединение и состояние поколения заглушки."}
	case "state_conflict":
		text = problemText{"Операция конфликтует с текущим состоянием или версией.", "Прочитайте актуальное состояние объекта."}
	case "body_too_large":
		text = problemText{"Превышен допустимый размер тела запроса.", "Уменьшите размер передаваемого тела."}
	case "preparation_limit":
		text = problemText{"Превышено допустимое ограничение подготовки заглушки.", "Проверьте размер и длительность исходного материала."}
	case "request_limit":
		text = problemText{"Превышено допустимое ограничение запроса или подготовки.", "Проверьте ограничения выбранной операции."}
	case "unsupported_media_type":
		text = problemText{"Тип содержимого не поддерживается этой операцией.", "Используйте Content-Type, объявленный в схеме операции."}
	case "invalid_parameters":
		text = problemText{"Параметры запроса недопустимы.", "Проверьте параметры по схеме операции."}
	case "invalid_field":
		text = problemText{"Указано недопустимое значение публичного поля.", "Исправьте указанное поле по схеме операции."}
	case "service_unavailable":
		text = problemText{"Операция временно недоступна.", "Проверьте готовность управляющей части сервиса."}
	case "api_closed":
		text = problemText{"Управляющий API завершает работу.", "Дождитесь готовности сервиса после запуска."}
	case "service_busy":
		text = problemText{"Сервис занят и не может принять операцию.", "Проверьте текущее состояние перед следующим запросом."}
	case "runtime_unavailable":
		text = problemText{"Необходимая возможность исполнения недоступна.", "Проверьте возможности сервера и журнал приложения."}
	case "resource_limit":
		text = problemText{"Достигнут предел ресурсов управляющей операции.", "Проверьте состояние сервиса и журнал приложения."}
	case "operation_canceled":
		text = problemText{"Выполнение операции отменено.", "Проверьте состояние объекта и квитанцию принятой команды, если она существует."}
	case "operation_timeout":
		text = problemText{"Истекло время выполнения управляющей операции.", "Проверьте состояние объекта и квитанцию принятой команды, если она существует."}
	case "sse_client_limit":
		text = problemText{"Достигнут предел одновременных подписок на события.", "Закройте ненужные подписки перед новым подключением."}
	case "sse_transport_unavailable":
		text = problemText{"Транспорт не поддерживает необходимую передачу событий.", "Проверьте поддержку SSE и сроков записи HTTP-транспортом."}
	case "internal_error":
		text = problemText{"Внутренняя ошибка сервиса.", ""}
	default:
		return problemText{}, false
	}
	return text, true
}

// defaultProblemCode выбирает общий безопасный код класса без догадок о конкретной причине.
func defaultProblemCode(status int) string {
	switch status {
	case 400:
		return "invalid_request"
	case 404:
		return "resource_not_found"
	case 405:
		return "method_not_allowed"
	case 408:
		return "body_read_timeout"
	case 409:
		return "state_conflict"
	case 413:
		return "request_limit"
	case 415:
		return "unsupported_media_type"
	case 422:
		return "invalid_parameters"
	case 503:
		return "service_unavailable"
	default:
		return "internal_error"
	}
}

// newProblem создаёт собственный ответ из закрытого каталога; неизвестный код заменяется общим кодом статуса.
func newProblem(status int, code string) *trustedProblem {
	// text и known содержат подготовленные тексты и признак принадлежности каталогу.
	text, known := catalogText(code)
	if !known {
		code = defaultProblemCode(status)
		text, _ = catalogText(code)
	}
	return &trustedProblem{ProblemDTO: ProblemDTO{ErrorModel: huma.ErrorModel{Type: "about:blank", Title: http.StatusText(status), Status: status, Detail: text.detail}, Code: code, Action: text.action}}
}

// safeDetail возвращает общее подготовленное объяснение класса без исходных значений.
func safeDetail(status int) string { return safeProblem(status).Detail }

// safeProblem создаёт общее представление класса ошибки без реконструкции неизвестной причины.
func safeProblem(status int) *trustedProblem { return newProblem(status, defaultProblemCode(status)) }

// newProfileProblem сохраняет подготовленные проверенные поля прежнего профильного контракта.
func newProfileProblem(code, field, detail string) *trustedProfileProblem {
	// action объясняет следующий шаг; отсутствие поля не приписывает клиенту неизвестное нарушение.
	action := "Проверьте параметры профиля по схеме операции."
	if field != "" {
		action = "Исправьте указанное поле профиля и повторите запрос."
	}
	return &trustedProfileProblem{ProfileProblemDTO: ProfileProblemDTO{ErrorModel: huma.ErrorModel{Type: "about:blank", Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity, Detail: detail}, Code: code, Field: field, Action: action}}
}

// contextDTO копирует закрытый контекст домена; полностью пустой контекст не публикуется.
func contextDTO(value core.FailureContext) *ProblemContextDTO {
	if value == (core.FailureContext{}) {
		return nil
	}
	return &ProblemContextDTO{StreamID: string(value.StreamID), SessionID: string(value.SessionID), OutputID: string(value.OutputID), GroupID: string(value.GroupID), ProfileID: string(value.ProfileID), AssetID: string(value.AssetID), GenerationID: string(value.GenerationID), VariantID: string(value.VariantID), DestinationID: string(value.DestinationID), CommandID: string(value.CommandID)}
}

// failureReasonDTO использует только валидированную сохранённую причину; legacy остаётся прежним общим отказом.
func failureReasonDTO(reason *core.FailureReason, legacy string) *FailureDTO {
	if reason == nil || reason.Validate() != nil {
		return failure(legacy)
	}
	// text и known связывают закрытый код с публичными текстами API.
	text, known := catalogText(string(reason.Code))
	if !known {
		return failure(legacy)
	}
	return &FailureDTO{Code: string(reason.Code), Message: text.detail, Action: text.action, Field: string(reason.Field), Context: contextDTO(reason.Context)}
}

// apiReasonError фиксирует проверенную причину транспорта, сохраняя прежнюю классификацию cause.
type apiReasonError struct {
	// cause сохраняет прежнюю ошибку и её приоритет классификации.
	cause error
	// code и field задают подготовленную причину и известное публичное поле.
	code, field string
}

// Error возвращает локальное описание исходного отказа без изменения его причинной цепочки.
func (e *apiReasonError) Error() string { return e.cause.Error() }

// Unwrap сохраняет прежние errors.Is и classify для транспортной причины.
func (e *apiReasonError) Unwrap() error { return e.cause }

// reasonError связывает ошибку с установленной проверкой транспорта без отражения входных значений.
func reasonError(cause error, code, field string) error {
	return &apiReasonError{cause: cause, code: code, field: field}
}

// diagnosticProblem выбирает причину только после исходной классификации. Различающиеся доверенные
// ветви победившего класса дают общий ответ; поля и контексты разных ветвей никогда не смешиваются.
func diagnosticProblem(err error, status int) error {
	if status == 500 {
		return safeProblem(status)
	}
	// profileError сохраняет прежний выбор проверенной ошибки полного профиля.
	var profileError *core.ProfileError
	if status == 422 && errors.As(err, &profileError) {
		return newProfileProblem(profileError.Code(), profileError.Field(), profileError.Detail())
	}
	// inputError сохраняет прежний выбор ошибки преобразования входного профиля.
	var inputError profileInputError
	if status == 422 && errors.As(err, &inputError) {
		return newProfileProblem("invalid_profile", inputError.field, inputError.detail)
	}

	// selected хранит единственное согласованное представление; conflict запрещает выбор разных причин.
	var selected *trustedProblem
	conflict := false
	// consider принимает целый подготовленный ответ и сравнивает его с уже выбранным ответом.
	consider := func(candidate *trustedProblem) {
		if selected == nil {
			selected = candidate
			return
		}
		if !sameProblemReason(selected, candidate) {
			conflict = true
		}
	}
	// visit обходит исходное дерево ошибок, сохраняя принадлежность причины непосредственному cause.
	var visit func(error)
	visit = func(current error) {
		if current == nil {
			return
		}
		// typed содержит конкретную оболочку или узел дерева причин.
		switch typed := current.(type) {
		case *core.ReasonError:
			// reason содержит копию проверенной причины данного узла.
			reason := typed.Reason()
			if classify(typed.Unwrap()) == status && reason.Validate() == nil && reasonHTTPCompatible(reason.Code, status) {
				// candidate связывает тексты, поле и контекст ровно одной причины.
				candidate := newProblem(status, string(reason.Code))
				if candidate.Code == string(reason.Code) {
					candidate.Field = string(reason.Field)
					candidate.Context = contextDTO(reason.Context)
					consider(candidate)
				}
			}
		case *apiReasonError:
			if classify(typed.cause) == status { // candidate содержит целую транспортную причину.
				candidate := newProblem(status, typed.code)
				candidate.Field = typed.field
				consider(candidate)
			}
		}

		// builtin содержит проверенное объяснение известной причины именно этого узла.
		if builtin := builtinProblem(current, status); builtin != nil {
			consider(builtin)
		}
		// branches содержит объединённые причины; ok подтверждает такой узел дерева; child обозначает очередную ветвь.
		if branches, ok := current.(interface {
			// Unwrap возвращает независимые исходные ветви объединённой ошибки.
			Unwrap() []error
		}); ok {
			for _, child := range branches.Unwrap() {
				visit(child)
			}
			return
		}
		// wrapped содержит одну причинную связь; ok подтверждает, что текущий узел является оболочкой.
		if wrapped, ok := current.(interface {
			// Unwrap возвращает исходную ошибку обычной диагностической оболочки.
			Unwrap() error
		}); ok {
			visit(wrapped.Unwrap())
		}
	}
	visit(err)
	if conflict {
		return safeProblem(status)
	}
	if selected != nil {
		return selected
	}
	return safeProblem(status)
}

// transformProblem пропускает только собственные ответы с совпадающим статусом и очищает ошибки Huma.
// Resolver передаёт своё представление через ограниченный слот текущего запроса, поскольку Huma стирает его тип.
func transformProblem(ctx huma.Context, status int, value any) any {
	// profileOperation отмечает прежний контракт профильных 422 с отдельным закрытым каталогом.
	profileOperation := status == 422 && ctx.Operation() != nil && (ctx.Operation().OperationID == "create-profile" || ctx.Operation().OperationID == "update-profile")
	// problem содержит конкретное доверенное представление ответа.
	switch problem := value.(type) {
	case *trustedProblem:
		if problem.Status == status {
			if profileOperation {
				return newProfileProblem("invalid_profile", problem.Field, problem.Detail)
			}
			return problem
		}
	case *trustedProfileProblem:
		if problem.Status == status && status == 422 {
			return problem
		}
	}
	// state и ok выделяют ограниченный сроком запроса слот resolver.
	if state, ok := ctx.Context().Value(apiContextKey{}).(*requestState); ok && state.problem != nil {
		// problem содержит сохранённый resolver ответ; статус сверяется с фактическим выбором Huma.
		problem := state.problem
		state.problem = nil
		// typed допускает только собственное представление даже после стирания resolver-типа Huma.
		switch typed := problem.(type) {
		case *trustedProblem:
			if typed.Status == status {
				return typed
			}
		case *trustedProfileProblem:
			if typed.Status == status && status == 422 {
				return typed
			}
		}
	}
	// problem содержит стандартную ошибку Huma; ok подтверждает её тип. Message и Value не копируются.
	if problem, ok := value.(*huma.ErrorModel); ok {
		// detail содержит очередную ошибку схемы для проверки её Location по публичной операции.
		for _, detail := range problem.Errors {
			if detail == nil {
				continue
			}
			// field содержит только подтверждённый схемой публичный путь.
			field := publicErrorField(ctx, detail.Location)
			if field == "" {
				continue
			}
			if profileOperation && field == "profile.live_audio_mode" {
				return newProfileProblem("invalid_profile", field, "Требуется явное значение copy или transcode.")
			}
			if status == 422 { // result содержит подготовленную полевую ошибку без присланного значения.
				result := newProblem(status, "invalid_field")
				result.Field = field
				if profileOperation {
					return newProfileProblem("invalid_profile", field, result.Detail)
				}
				return result
			}
		}
	}
	if profileOperation {
		return newProfileProblem("invalid_profile", "", safeDetail(422))
	}
	switch status {
	case 400:
		return newProblem(status, "invalid_json")
	case 413:
		return newProblem(status, "body_too_large")
	default:
		return safeProblem(status)
	}
}

// publicErrorField допускает Location только при наличии каждого имени в публичной схеме операции.
// Индексы массивов проверяются, но не отражаются; динамические ключи additionalProperties запрещены.
// Имена полей ключей и адресов также сохраняют прежнее исключение из публичных ошибок.
func publicErrorField(ctx huma.Context, location string) string {
	// source обозначает источник поля; rest содержит его путь; found подтверждает разделитель.
	source, rest, found := strings.Cut(location, ".")
	if !found || rest == "" || ctx.Operation() == nil {
		return ""
	}
	// op содержит объявленную публичную операцию.
	op := ctx.Operation()
	if source != "body" {
		// parameter содержит очередной объявленный параметр операции.
		for _, parameter := range op.Parameters {
			if parameter.In == source && parameter.Name == rest {
				return rest
			}
		}
		return ""
	}
	if op.RequestBody == nil || op.RequestBody.Content["application/json"] == nil {
		return ""
	}
	// schema указывает публичную схему тела текущей операции.
	schema := op.RequestBody.Content["application/json"].Schema
	// parts содержит сегменты Location; names собирает только имена объявленных свойств.
	parts := strings.Split(rest, ".")
	names := make([]string, 0, len(parts))
	// part содержит очередное имя и необязательные индексы массива.
	for _, part := range parts {
		// name отделяет имя свойства; suffix содержит ещё не проверенные индексы.
		name, suffix, _ := strings.Cut(part, "[")
		if name == "" || name == "publication_key" || name == "stream_key" || name == "endpoint" {
			return ""
		}
		schema = schemaProperty(ctx, schema, name, 0)
		if schema == nil {
			return ""
		}
		names = append(names, name)
		if suffix != "" {
			suffix = "[" + suffix
			for suffix != "" {
				// index и tail выделяют очередной числовой индекс, closed подтверждает скобку.
				index, tail, closed := strings.Cut(strings.TrimPrefix(suffix, "["), "]")
				if !strings.HasPrefix(suffix, "[") || !closed || index == "" {
					return ""
				}
				// digit проверяет ASCII-цифры индекса без отражения его значения.
				for _, digit := range index {
					if digit < '0' || digit > '9' {
						return ""
					}
				}
				schema = resolvePublicSchema(ctx, schema)
				if schema == nil || schema.Items == nil {
					return ""
				}
				schema = schema.Items
				suffix = tail
			}
		} else if strings.Contains(part, "[") {
			return ""
		}
	}
	return strings.Join(names, ".")
}

// resolvePublicSchema разыменовывает только внутреннюю ссылку объявленного компонента.
func resolvePublicSchema(ctx huma.Context, schema *huma.Schema) *huma.Schema {
	if schema == nil || schema.Ref == "" {
		return schema
	}
	// state и ok дают доступ к реестру именно обслуживающего API.
	state, ok := ctx.Context().Value(apiContextKey{}).(*requestState)
	if !ok || !strings.HasPrefix(schema.Ref, "#/components/schemas/") {
		return nil
	}
	return state.api.huma.OpenAPI().Components.Schemas.Map()[strings.TrimPrefix(schema.Ref, "#/components/schemas/")]
}

// schemaProperty ищет объявленное свойство, включая nullable-ветви публичной схемы.
// depth ограничивает обход составных схем; additionalProperties намеренно не используется.
func schemaProperty(ctx huma.Context, schema *huma.Schema, name string, depth int) *huma.Schema {
	if depth > 32 {
		return nil
	}
	schema = resolvePublicSchema(ctx, schema)
	if schema == nil {
		return nil
	}
	if schema.Properties[name] != nil {
		return schema.Properties[name]
	}
	// group содержит альтернативы либо пересечения; child содержит очередную составную схему.
	for _, group := range [][]*huma.Schema{schema.AnyOf, schema.OneOf, schema.AllOf} {
		for _, child := range group {
			// property содержит найденное объявленное свойство составной схемы.
			if property := schemaProperty(ctx, child, name, depth+1); property != nil {
				return property
			}
		}
	}
	return nil
}

// setProblemResponses задаёт одинаковые публичные схемы всех ошибок до регистрации Huma.
func (a *API) setProblemResponses(op *huma.Operation) {
	if op.Responses == nil {
		op.Responses = map[string]*huma.Response{}
	}
	// status содержит очередной поддержанный HTTP-класс ошибки.
	for _, status := range []int{400, 404, 405, 408, 409, 413, 415, 422, 500, 503} {
		// model сохраняет отдельное публичное имя схемы профильных 422.
		model := reflect.TypeFor[ProblemDTO]()
		if status == 422 && (op.OperationID == "create-profile" || op.OperationID == "update-profile") {
			model = reflect.TypeFor[ProfileProblemDTO]()
		}
		op.Responses[strconv.Itoa(status)] = &huma.Response{Description: http.StatusText(status), Content: map[string]*huma.MediaType{"application/problem+json": {Schema: a.huma.OpenAPI().Components.Schemas.Schema(model, true, "")}}}
	}
}

// reasonHTTPCompatible не позволяет причине одного класса объяснять статус другого класса.
// Причины исполнения принятой команды предназначены для FailureDTO, а неизвестный 500 остаётся общим.
func reasonHTTPCompatible(code core.FailureCode, status int) bool {
	switch code {
	case core.FailureFallbackNotConfigured, core.FailureFallbackNotPrepared, core.FailureFallbackProfileNotPrepared,
		core.FailureFallbackVariantNotReady, core.FailurePreparationActive, core.FailureSessionActive,
		core.FailureRequestIDConflict, core.FailureRevisionConflict, core.FailureSessionStateConflict,
		core.FailureOutputDetached, core.FailureOutputAlreadyRunning, core.FailureOutputCleanupPending,
		core.FailureDestinationAttached, core.FailureResourceInUse, core.FailureResourceOwnershipConflict:
		return status == 409
	default:
		return false
	}
}

// writeProblemCode записывает прямой HTTP-отказ через тот же каталог, что типизированные операции.
func writeProblemCode(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	// Ошибку записи уже начатого ответа нельзя заменить новым HTTP-статусом.
	_ = json.NewEncoder(w).Encode(newProblem(status, code))
}

// builtinProblem распознаёт доказанные встроенные причины отдельного узла дерева.
// Выбор между несколькими узлами выполняет diagnosticProblem; порядок switch не задаёт новый приоритет.
func builtinProblem(err error, status int) *trustedProblem {
	if status == 413 {
		// isBodyLimit подтверждает конкретную ошибку предела тела без обхода соседних ветвей.
		if _, isBodyLimit := err.(*http.MaxBytesError); isBodyLimit {
			return newProblem(status, "body_too_large")
		}
		if err == fallback.ErrLimit {
			return newProblem(status, "preparation_limit")
		}
	}
	if status == 400 && err == core.ErrInvalidCursor {
		// problem указывает известное поле курсора без его присланного значения.
		problem := newProblem(status, "invalid_cursor")
		problem.Field = "cursor"
		return problem
	}
	if status != 503 {
		return nil
	}
	switch err {
	case core.ErrClosed:
		return newProblem(status, "service_unavailable")
	case core.ErrBusy:
		return newProblem(status, "service_busy")
	case core.ErrRuntimeCapability:
		return newProblem(status, "runtime_unavailable")
	case core.ErrObservationLimit, core.ErrEventSequenceExhausted:
		return newProblem(status, "resource_limit")
	case context.DeadlineExceeded:
		return newProblem(status, "operation_timeout")
	case context.Canceled:
		return newProblem(status, "operation_canceled")
	default:
		return nil
	}
}

// problemCodes перечисляет закрытые машинные коды публичного Problem Details.
func problemCodes() []string {
	return []string{"fallback_not_configured", "fallback_not_prepared", "fallback_profile_not_prepared", "fallback_variant_not_ready", "preparation_active", "session_active", "request_id_conflict", "revision_conflict", "session_state_conflict", "output_detached", "output_already_running", "output_cleanup_pending", "destination_attached", "resource_in_use", "resource_ownership_conflict", "session_start_failed", "output_start_failed", "session_stop_failed", "output_stop_failed", "session_pause_failed", "session_resume_failed", "execution_interrupted", "command_superseded", "invalid_json", "invalid_request", "invalid_cursor", "sse_cursor_conflict", "resource_not_found", "nested_resource_not_found", "route_not_found", "schema_not_found", "method_not_allowed", "body_read_timeout", "upload_timeout", "state_conflict", "body_too_large", "preparation_limit", "request_limit", "unsupported_media_type", "invalid_parameters", "invalid_field", "service_unavailable", "api_closed", "service_busy", "runtime_unavailable", "resource_limit", "operation_canceled", "operation_timeout", "sse_client_limit", "sse_transport_unavailable", "internal_error", "empty_upload", "upload_canceled", "invalid_time", "invalid_profile", "invalid_custom", "duplicate_custom", "custom_conflict", "unknown_custom"}
}

// TransformSchema публикует закрытый каталог code общей модели Problem Details.
func (ProblemDTO) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	stringEnum(schema, "code", problemCodes())
	return schema
}

// TransformSchema сохраняет отдельное имя и прежний закрытый каталог профильных ошибок.
func (ProfileProblemDTO) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	stringEnum(schema, "code", []string{"invalid_profile", "invalid_custom", "duplicate_custom", "custom_conflict", "unknown_custom"})
	return schema
}

// TransformSchema публикует долговечные предметные причины вместе с прежними общими кодами.
func (FailureDTO) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	stringEnum(schema, "code", []string{"fallback_not_configured", "fallback_not_prepared", "fallback_profile_not_prepared", "fallback_variant_not_ready", "preparation_active", "session_active", "request_id_conflict", "revision_conflict", "session_state_conflict", "output_detached", "output_already_running", "output_cleanup_pending", "destination_attached", "resource_in_use", "resource_ownership_conflict", "session_start_failed", "output_start_failed", "session_stop_failed", "output_stop_failed", "session_pause_failed", "session_resume_failed", "execution_interrupted", "command_superseded", "session_failed", "session_interrupted", "command_failed", "command_interrupted", "preparation_failed", "preparation_interrupted", "observation_failed"})
	return schema
}

// sameProblemReason сравнивает целые подготовленные причины, не используя тождественность error.
// Тексты и необязательный контекст сравниваются вместе: разные ветви не могут дополнить друг друга.
func sameProblemReason(left, right *trustedProblem) bool {
	if left.Code != right.Code || left.Field != right.Field || left.Detail != right.Detail || left.Action != right.Action {
		return false
	}
	if left.Context == nil || right.Context == nil {
		return left.Context == nil && right.Context == nil
	}
	return *left.Context == *right.Context
}
