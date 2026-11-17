package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
)

// reasonCommandPort подставляет только результат запуска, сохраняя настоящие регистрацию и транспорт.
type reasonCommandPort struct {
	// Commands предоставляет остальные команды, которые данные сценарии не вызывают.
	api.Commands
	// failure задаёт проверяемую исходную ошибку границы исполнения.
	failure error
	// calls считает допущенные вызовы, чтобы ошибки разбора не исполняли команду.
	calls int
}

// StartSession возвращает заданный отказ без создания квитанции; ctx и request здесь не изменяются.
func (p *reasonCommandPort) StartSession(ctx context.Context, request core.StartSessionRequest) (core.CommandReceipt, error) {
	p.calls++
	return core.CommandReceipt{}, p.failure
}

// requirePublicProblem проверяет стандартный контракт и независимые поля причины и действия.
// response содержит настоящий ответ, status и code задают ожидаемые публичные результаты.
func requirePublicProblem(t *testing.T, response *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	// problem содержит разобранные поля, включая точное числовое значение HTTP-статуса.
	problem := apiAcceptanceJSON(t, response)
	if response.Code != status || problem["status"] != json.Number(strconv.Itoa(status)) || problem["code"] != code || problem["type"] == "" || problem["title"] == "" || problem["detail"] == "" {
		t.Fatalf("некорректный Problem Details: status=%d body=%s", response.Code, response.Body)
	}
	if !strings.HasPrefix(response.Header().Get("Content-Type"), "application/problem+json") || problem["errors"] != nil {
		t.Fatalf("небезопасное представление ошибки: headers=%v body=%s", response.Header(), response.Body)
	}
	// action содержит необязательное действие; exists отличает отсутствие от ошибочной пустой строки.
	if action, exists := problem["action"]; exists && (action == "" || action == problem["detail"]) {
		t.Fatalf("действие смешано с причиной: %s", response.Body)
	}
	apiAcceptanceNoSecrets(t, response.Body.String(), "synthetic-rejected-secret", "synthetic-exception-secret", "/private/internal/file")
	return problem
}

// TestProblemPrehandlerAndDirectRoutesRetainDistinctSafeReasons проверяет ошибки до бизнес-обработчика,
// прямые маршруты Chi и SSE; присланные значения и неизвестные имена не становятся публичными полями.
func TestProblemPrehandlerAndDirectRoutesRetainDistinctSafeReasons(t *testing.T) {
	// port фиксирует отсутствие исполнения команды при транспортном отказе.
	port := &reasonCommandPort{failure: core.ErrBusy}
	// fixture создаёт настоящие API и временную SQLite с ограниченным JSON-телом.
	fixture := newAPIAcceptanceFixture(t, func(config *api.Config) {
		// Подключает наблюдаемый порт вместо запуска внешних процессов.
		config.Commands = port
	})
	// scenario описывает независимые классы ошибок реальной HTTP-регистрации.
	for _, scenario := range []struct {
		// name даёт читаемое имя сценария; method и path адресуют зарегистрированный HTTP-путь.
		name, method, path string
		// body и contentType задают присланные данные и заявленный формат.
		body, contentType string
		// status и code задают сохранённую транспортную семантику и точную различимую причину.
		status int
		code   string
		// field задаёт единственное допустимое публичное имя известного поля.
		field string
	}{
		{"json", "POST", "/streams/known/start", `{"request_id":"synthetic-rejected-secret",`, "application/json", 400, "invalid_json", ""},
		{"known field", "POST", "/streams/known/start", `{"request_id":false,"destination_ids":[]}`, "application/json", 422, "invalid_field", "request_id"},
		{"unknown property", "POST", "/streams/known/start", `{"request_id":"request","synthetic-rejected-secret":true,"destination_ids":[]}`, "application/json", 422, "invalid_parameters", ""},
		{"cursor", "GET", "/streams?cursor=synthetic-rejected-secret", "", "", 400, "invalid_cursor", "cursor"},
		{"sse cursor", "GET", "/events?cursor=synthetic-rejected-secret", "", "", 400, "invalid_cursor", "cursor"},
		{"route", "GET", "/synthetic-rejected-secret", "", "", 404, "route_not_found", ""},
		{"schema", "GET", "/schemas/synthetic-rejected-secret.json", "", "", 404, "schema_not_found", ""},
		{"method", "DELETE", "/openapi.json", "", "", 405, "method_not_allowed", ""},
		{"content type", "POST", "/streams/known/start", `{"request_id":"request","destination_ids":[]}`, "text/plain", 415, "unsupported_media_type", ""},
		{"body limit", "POST", "/streams/known/start", strings.Repeat(" ", int(fixture.limits.JSONBytes)) + `{}`, "application/json", 413, "body_too_large", ""},
	} {
		// Подтест проверяет безопасный отказ scenario через настоящий HTTP-обработчик.
		t.Run(scenario.name, func(t *testing.T) {
			// request передаёт данный набор байтов без реального сетевого соединения.
			request := httptest.NewRequest(scenario.method, api.Prefix+scenario.path, strings.NewReader(scenario.body))
			request.Header.Set("Content-Type", scenario.contentType)
			// response сохраняет ответ всех настоящих промежуточных обработчиков.
			response := httptest.NewRecorder()
			fixture.api.ServeHTTP(response, request)
			// problem содержит безопасную причину для проверки допустимого поля и отсутствия контекста.
			problem := requirePublicProblem(t, response, scenario.status, scenario.code)
			if scenario.field == "" {
				if problem["field"] != nil {
					t.Fatalf("неизвестное поле отражено в ответе: %s", response.Body)
				}
			} else if problem["field"] != scenario.field {
				t.Fatalf("известное поле потеряно: %s", response.Body)
			}
			if problem["context"] != nil {
				t.Fatalf("контекст придуман без чтения объекта: %s", response.Body)
			}
		})
	}
	if port.calls != 0 {
		t.Fatalf("отклонённый запрос исполнил команду %d раз", port.calls)
	}
}

// TestProblemExecutionFailuresPreserveStatusPriorityAndDoNotLeak проверяет доменные классы
// через зарегистрированный запуск; объединение ошибок не меняет прежний приоритет статусов.
func TestProblemExecutionFailuresPreserveStatusPriorityAndDoNotLeak(t *testing.T) {
	// port позволяет менять ошибку между последовательными независимыми запросами.
	port := &reasonCommandPort{}
	// fixture использует реальную регистрацию и управляемый результат команды.
	fixture := newAPIAcceptanceFixture(t, func(config *api.Config) {
		// Подменяет только границу команды, не Huma или преобразователь ошибок.
		config.Commands = port
	})
	// scenario сопоставляет исходный отказ с независимо закреплённой публичной семантикой.
	for _, scenario := range []struct {
		// name обозначает исходный класс; failure содержит исходную ошибку либо errors.Join.
		name    string
		failure error
		// status и code задают публичный класс и устойчивую причину.
		status int
		code   string
	}{
		{"unknown", errors.New("synthetic-exception-secret /private/internal/file"), 500, "internal_error"},
		{"busy", core.ErrBusy, 503, "service_busy"},
		{"closed", core.ErrClosed, 503, "service_unavailable"},
		{"canceled", context.Canceled, 503, "operation_canceled"},
		{"deadline", context.DeadlineExceeded, 503, "operation_timeout"},
		{"not found before conflict", errors.Join(core.ErrNotFound, core.ErrConflict), 404, "resource_not_found"},
		{"conflict before invalid", errors.Join(core.ErrConflict, core.ErrInvalid), 409, "state_conflict"},
		{"body before not found", errors.Join(&http.MaxBytesError{Limit: 5}, core.ErrNotFound), 413, "body_too_large"},
		{"preparation quota", fallback.ErrLimit, 413, "preparation_limit"},
	} {
		// Подтест сопоставляет исходную ошибку scenario с публичным статусом и объяснением.
		t.Run(scenario.name, func(t *testing.T) {
			port.failure = scenario.failure
			// response содержит отказ handler, прошедший API.problem и Transformer.
			response := apiAcceptanceRequest(t, fixture.api, "POST", "/streams/known/start", `{"request_id":"synthetic-rejected-secret","destination_ids":[]}`, scenario.status)
			// problem подтверждает различимую причину и отсутствие недоказанного объекта.
			problem := requirePublicProblem(t, response, scenario.status, scenario.code)
			if problem["context"] != nil {
				t.Fatalf("sentinel породил выдуманный контекст: %s", response.Body)
			}
			if scenario.status == 500 && problem["action"] != nil {
				t.Fatalf("неизвестному исключению приписано действие: %s", response.Body)
			}
			if scenario.code == "preparation_limit" && strings.Contains(strings.ToLower(problem["detail"].(string)), "тела запроса") {
				t.Fatalf("лимит подготовки ошибочно объявлен размером HTTP-тела: %s", response.Body)
			}
		})
	}
}

// TestProblemTypedJoinsKeepOneWinningReason проверяет, что приоритет HTTP отделён от выбора
// конкретной причины: несовместимые причины одного класса дают общий ответ без смеси контекстов.
func TestProblemTypedJoinsKeepOneWinningReason(t *testing.T) {
	// first и second содержат два самостоятельных объяснения одного статуса 409.
	first := core.FailureReason{Code: core.FailureFallbackNotConfigured, Field: core.FailureFieldFallback, Context: core.FailureContext{StreamID: "known-stream"}}
	second := core.FailureReason{Code: core.FailureSessionActive, Context: core.FailureContext{SessionID: "known-session"}}
	// port и fixture оставляют реальными Huma, Transformer и публичные схемы.
	port := &reasonCommandPort{}
	fixture := newAPIAcceptanceFixture(t, func(config *api.Config) {
		// Передаёт тестовое дерево причин через штатный обработчик запуска.
		config.Commands = port
	})
	// scenario задаёт дерево ошибок и независимый ожидаемый выбор целого ответа.
	for _, scenario := range []struct {
		// name называет порядок и структуру причин; failure передаёт исходное дерево.
		name    string
		failure error
		// status и code задают победивший класс и однозначную причину.
		status int
		code   string
		// contextual определяет наличие доказанного контекста единственной причины.
		contextual bool
	}{
		{"single", core.WithFailureReason(core.ErrNotReady, first), 409, "fallback_not_configured", true},
		{"same twice", errors.Join(core.WithFailureReason(core.ErrNotReady, first), core.WithFailureReason(core.ErrNotReady, first)), 409, "fallback_not_configured", true},
		{"different", errors.Join(core.WithFailureReason(core.ErrNotReady, first), core.WithFailureReason(core.ErrInUse, second)), 409, "state_conflict", false},
		{"different reversed", errors.Join(core.WithFailureReason(core.ErrInUse, second), core.WithFailureReason(core.ErrNotReady, first)), 409, "state_conflict", false},
		{"not found wins", errors.Join(core.WithFailureReason(core.ErrInUse, second), core.ErrNotFound), 404, "resource_not_found", false},
		{"incompatible class", core.WithFailureReason(core.ErrBusy, first), 503, "service_busy", false},
		{"unknown exception", core.WithFailureReason(errors.New("synthetic-exception-secret"), first), 500, "internal_error", false},
	} {
		// Подтест проверяет выбор одной целой причины для дерева ошибок scenario.
		t.Run(scenario.name, func(t *testing.T) {
			port.failure = scenario.failure
			// response содержит настоящий отказ handler после всех преобразователей.
			response := apiAcceptanceRequest(t, fixture.api, "POST", "/streams/known/start", `{"request_id":"request","destination_ids":[]}`, scenario.status)
			// problem содержит неделимый выбранный результат.
			problem := requirePublicProblem(t, response, scenario.status, scenario.code)
			if scenario.contextual {
				// known содержит контекст единственной проверенной ветви.
				known, _ := problem["context"].(map[string]any)
				if known["stream_id"] != "known-stream" || known["session_id"] != nil || problem["field"] != "fallback" || problem["action"] == nil {
					t.Fatal("поля доверенной причины смешаны или потеряны", problem)
				}
			} else if problem["context"] != nil || problem["field"] != nil {
				t.Fatal("общий ответ унаследовал поля проигравшей причины", problem)
			}
		})
	}
}

// TestProblemRealCASAndResourceUseRemainAtomic проверяет доказанные причины на настоящей
// транзакционной границе: устаревшая ревизия и внешняя ссылка не меняют конфигурацию.
func TestProblemRealCASAndResourceUseRemainAtomic(t *testing.T) {
	// fixture содержит настоящие API/Core/SQLite, без подмены бизнес-ошибок.
	fixture := newAPIAcceptanceFixture(t, nil)
	// stream и profile адресуют реальные записи; группа и назначение создают внешнюю ссылку.
	stream, profile, _, _ := apiAcceptanceSeedConfiguration(t, fixture)
	// response содержит отказ условного обновления с заведомо устаревшей ревизией.
	response := apiAcceptanceRequest(t, fixture.api, "PATCH", "/streams/"+string(stream.ID), `{"expected_revision":999,"name":"Изменённый"}`, 409)
	// problem проверяет различимый конфликт ревизии и известный исходный объект.
	problem := requirePublicProblem(t, response, 409, "revision_conflict")
	// known содержит только прочитанные сервером идентификаторы.
	known, _ := problem["context"].(map[string]any)
	if problem["field"] != "expected_revision" || known["stream_id"] != string(stream.ID) || problem["action"] == nil {
		t.Fatal("конфликт CAS потерял поле, объект или действие", problem)
	}
	// unchanged содержит текущую конфигурацию после отказа CAS; err сообщает об ошибке чтения.
	unchanged, err := fixture.service.GetStream(context.Background(), stream.ID)
	if err != nil || unchanged.Revision != stream.Revision || unchanged.Name != stream.Name {
		t.Fatal("отказ CAS изменил Stream", unchanged, err)
	}
	response = apiAcceptanceRequest(t, fixture.api, "DELETE", "/transcode-profiles/"+string(profile.ID)+"?expected_revision="+strconv.FormatUint(profile.Revision, 10), "", 409)
	problem = requirePublicProblem(t, response, 409, "resource_in_use")
	known, _ = problem["context"].(map[string]any)
	if known["profile_id"] != string(profile.ID) || problem["action"] == nil {
		t.Fatal("доказанная внешняя ссылка потеряла профиль", problem)
	}
	if _, err = fixture.service.GetProfile(context.Background(), profile.ID); err != nil {
		t.Fatal("отказ удаления потерял профиль", err)
	}
}

// TestProblemSchemasDescribeActualErrorsIncludingInternalFailures проверяет опубликованные схемы
// всех значимых статусов на зарегистрированном запуске и сохранение имени профильной схемы 422.
func TestProblemSchemasDescribeActualErrorsIncludingInternalFailures(t *testing.T) {
	// fixture предоставляет неизменяемые документы после настоящей регистрации всех маршрутов.
	fixture := newAPIAcceptanceFixture(t, nil)
	// spec содержит публичную спецификацию, полученную тем же HTTP-маршрутизатором.
	spec := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", "/openapi.json", "", 200))
	// paths содержит объявленные операции API.
	paths := spec["paths"].(map[string]any)
	// operation и responses содержат контракт запуска, включая ошибки до обработчика и 500.
	operation := paths[api.Prefix+"/streams/{id}/start"].(map[string]any)["post"].(map[string]any)
	responses := operation["responses"].(map[string]any)
	// status перебирает обязательные реально применимые классы ответа.
	for _, status := range []string{"400", "404", "408", "409", "413", "415", "422", "500", "503"} {
		// response содержит описание конкретного HTTP-статуса; ok подтверждает его наличие.
		response, ok := responses[status].(map[string]any)
		if !ok {
			t.Fatalf("схема статуса %s отсутствует", status)
		}
		// content и schema адресуют публичную модель фактического Problem Details.
		content := response["content"].(map[string]any)["application/problem+json"].(map[string]any)
		schema := content["schema"].(map[string]any)
		if schema["$ref"] != "#/components/schemas/ProblemDTO" {
			t.Fatalf("статус %s использует иную схему: %+v", status, schema)
		}
	}
	// schema содержит непосредственно опубликованную JSON Schema общей ошибки.
	schema := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", "/schemas/ProblemDTO.json", "", 200))
	// properties содержит обязательные расширения действительного wire-контракта.
	properties := schema["properties"].(map[string]any)
	// field задаёт каждое значимое свойство, которое клиент должен найти в схеме.
	for _, field := range []string{"type", "title", "status", "detail", "code", "action", "field", "context"} {
		if properties[field] == nil {
			t.Fatalf("публичная схема не описывает %s", field)
		}
	}
	// profileSchema сохраняет прежнее публичное имя и закрытый каталог пяти профильных кодов.
	profileSchema := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", "/schemas/ProfileProblemDTO.json", "", 200))
	// profileCodes содержит фактически опубликованные варианты code профильного 422.
	profileCodes := profileSchema["properties"].(map[string]any)["code"].(map[string]any)["enum"].([]any)
	if len(profileCodes) != 5 {
		t.Fatalf("каталог профильного 422 изменён: %v", profileCodes)
	}
	// expected задаёт каждый прежний поддержанный код подробной ошибки профиля.
	for _, expected := range []string{"invalid_profile", "invalid_custom", "duplicate_custom", "custom_conflict", "unknown_custom"} {
		// found подтверждает наличие прежнего кода независимо от порядка enum.
		found := false
		// actual содержит очередной опубликованный код.
		for _, actual := range profileCodes {
			if actual == expected {
				found = true
			}
		}
		if !found {
			t.Fatalf("схема потеряла профильный код %s", expected)
		}
	}
	// request предлагает неподдержанный формат ответа и проверяет реальную negotiation-ветвь Huma.
	request := httptest.NewRequest("GET", api.Prefix+"/streams", nil)
	request.Header.Set("Accept", "application/xml")
	// response сохраняет прежний JSON-ответ при неподдержанном Accept, поскольку Huma допускает fallback.
	response := httptest.NewRecorder()
	fixture.api.ServeHTTP(response, request)
	if response.Code != 200 || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("прежний выбор JSON при Accept изменён: status=%d body=%s", response.Code, response.Body)
	}
}

// TestProblemFallbackAdmissionSeparatesMissingPolicyGenerationAndGlobalPreparation проверяет
// три разные доказанные причины одного 409 и отсутствие квитанций после отказа допуска.
func TestProblemFallbackAdmissionSeparatesMissingPolicyGenerationAndGlobalPreparation(t *testing.T) {
	// fixture предоставляет настоящую границу доменных ошибок без фоновой подготовки.
	fixture := newAPIAcceptanceFixture(t, nil)
	// ctx задаёт контекст создания только тестовой конфигурации.
	ctx := context.Background()
	// stream содержит поток без настроенной политики заглушки; err сообщает об отказе создания.
	stream, err := fixture.service.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "synthetic-publication-secret"})
	if err != nil {
		t.Fatal(err)
	}
	// response и problem содержат конкретный отказ при отсутствующей настройке.
	response := apiAcceptanceRequest(t, fixture.api, "POST", "/streams/"+string(stream.ID)+"/start", `{"request_id":"no-policy","destination_ids":[]}`, 409)
	problem := requirePublicProblem(t, response, 409, "fallback_not_configured")
	if problem["action"] == nil || problem["field"] != "fallback" {
		t.Fatal("не указан способ настройки", problem)
	}
	// asset представляет выбранную заглушку без готового поколения.
	asset, err := fixture.service.CreateAsset(ctx, "Без поколения")
	if err != nil {
		t.Fatal(err)
	}
	stream.Fallback = core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}
	stream, err = fixture.service.UpdateStream(ctx, stream, stream.Revision)
	if err != nil {
		t.Fatal(err)
	}
	response = apiAcceptanceRequest(t, fixture.api, "POST", "/streams/"+string(stream.ID)+"/start", `{"request_id":"no-generation","destination_ids":[]}`, 409)
	problem = requirePublicProblem(t, response, 409, "fallback_not_prepared")
	// known содержит достоверную выбранную заглушку, но не выдуманный профиль или вариант.
	known, _ := problem["context"].(map[string]any)
	if known["asset_id"] != string(asset.ID) || known["variant_id"] != nil || known["profile_id"] != nil || problem["action"] == nil {
		t.Fatal("контекст неготового поколения недостоверен", problem)
	}
	// unrelated создаёт глобальную активную подготовку другой заглушки.
	unrelated, err := fixture.service.CreateAsset(ctx, "Другая подготовка")
	if err != nil {
		t.Fatal(err)
	}
	// generation сохраняет именно фактически активную подготовку текущего владельца.
	generation, err := fixture.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: unrelated.ID, ExpectedRevision: unrelated.Revision, Kind: core.AssetImage, GeneratorVersion: core.CurrentFallbackGeneratorVersion})
	if err != nil {
		t.Fatal(err)
	}
	response = apiAcceptanceRequest(t, fixture.api, "POST", "/streams/"+string(stream.ID)+"/start", `{"request_id":"global-preparation","destination_ids":[]}`, 409)
	problem = requirePublicProblem(t, response, 409, "preparation_active")
	known, _ = problem["context"].(map[string]any)
	if known["asset_id"] != string(unrelated.ID) || known["generation_id"] != string(generation.ID) || known["stream_id"] != nil {
		t.Fatal("глобальная подготовка приписана выбранному потоку", problem)
	}
	// requestID перебирает отклонённые намерения, которые не должны создавать долговечную команду.
	for _, requestID := range []string{"no-policy", "no-generation", "global-preparation"} {
		if _, err = fixture.service.FindReceipt(ctx, requestID); !errors.Is(err, core.ErrNotFound) {
			t.Fatal("отказ создал квитанцию", requestID, err)
		}
	}
}

// TestProblemRequestIdentityAndActiveSessionRemainDistinct проверяет настоящие RequestID/replay
// и занятый запуск: диагностика не выполняет новую команду и не выбирает изменённую конфигурацию.
func TestProblemRequestIdentityAndActiveSessionRemainDistinct(t *testing.T) {
	// fixture и preparation используют реальный менеджер файловой подготовки без медиапроцессов;
	// release снимает управляемую блокировку проверки исходного файла после теста.
	fixture, preparation, _, release := newAPIAcceptanceUpload(t, time.Second)
	defer release()
	// ctx ограничивает ожидание реального задания подготовки; cancel освобождает таймер контекста.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// asset содержит пустой объект до HTTP-загрузки небольшого изображения; err сообщает об отказе создания.
	asset, err := fixture.service.CreateAsset(ctx, "Заглушка")
	if err != nil {
		t.Fatal(err)
	}
	// body передаёт корректный PNG в настоящий управляемый FallbackManager.
	body := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	apiAcceptanceUploadRequest(t, fixture, ctx, string(asset.ID), asset.Revision, body, 202)
	if _, err = (<-preparation.jobs).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	// stream имеет готовую заглушку и не содержит назначений, поэтому не требует внешних соединений.
	stream, err := fixture.service.CreateStream(ctx, core.Stream{Name: "Готовый поток", PublicationKey: "synthetic-publication-secret", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	// accepted содержит отдельный от результата 202 и исходную серверную идентичность.
	accepted := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "POST", "/streams/"+string(stream.ID)+"/start", `{"request_id":"synthetic-rejected-secret","destination_ids":[]}`, 202))
	// response и problem содержат отказ другого RequestID из-за уже принятого активного запуска.
	response := apiAcceptanceRequest(t, fixture.api, "POST", "/streams/"+string(stream.ID)+"/start", `{"request_id":"new-start","destination_ids":[]}`, 409)
	problem := requirePublicProblem(t, response, 409, "session_active")
	// known подтверждает использование реально прочитанной Session, а не значения нового запроса.
	known, _ := problem["context"].(map[string]any)
	if known["session_id"] != accepted["session_id"] || known["stream_id"] != string(stream.ID) {
		t.Fatal("занятый запуск потерял известную сессию", problem)
	}
	response = apiAcceptanceRequest(t, fixture.api, "POST", "/streams/another-unread-stream/start", `{"request_id":"synthetic-rejected-secret","destination_ids":[]}`, 409)
	problem = requirePublicProblem(t, response, 409, "request_id_conflict")
	known, _ = problem["context"].(map[string]any)
	if problem["field"] != "request_id" || known["command_id"] != accepted["id"] || known["stream_id"] != nil {
		t.Fatal("конфликт идентичности отражает непрочитанную цель", problem)
	}
	// replay обязан вернуть принятую команду даже после удаления будущей политики из конфигурации.
	stream.Fallback = core.FallbackPolicy{}
	if _, err = fixture.service.UpdateStream(ctx, stream, stream.Revision); err != nil {
		t.Fatal(err)
	}
	// replayed содержит прежний результат, не новую оценку допуска.
	replayed := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "POST", "/streams/"+string(stream.ID)+"/start", `{"request_id":"synthetic-rejected-secret","destination_ids":[]}`, 202))
	if replayed["id"] != accepted["id"] || replayed["session_id"] != accepted["session_id"] {
		t.Fatal("повтор создал новый результат", replayed)
	}
	if _, err = fixture.service.FindReceipt(ctx, "new-start"); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("отказ занятости создал команду", err)
	}
	// fence содержит захваченную legacy-совместимую команду без нового FailureReason.
	fence, err := fixture.service.Claim(ctx, "synthetic-rejected-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.service.Complete(ctx, fence, core.Completion{Failure: "fallback_profile_not_prepared synthetic-exception-secret /private/internal/file"}); err != nil {
		t.Fatal(err)
	}
	// legacy читается успешно, но его диагностическая строка не разбирается как машинная причина.
	legacy := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", "/commands/"+accepted["id"].(string), "", 200))
	// failure содержит прежнюю безопасную общую причину без выдуманных деталей.
	failure := legacy["failure"].(map[string]any)
	if failure["code"] != "command_failed" || failure["context"] != nil {
		t.Fatal("legacy диагностика превратилась в предметную причину", failure)
	}
	// public содержит JSON публичного отказа; encodeErr сообщает об ошибке его сериализации.
	public, encodeErr := json.Marshal(failure)
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	apiAcceptanceNoSecrets(t, string(public), "synthetic-exception-secret", "/private/internal/file")
}

// TestProblemUploadDeadlineAndClosedAPIRetainProvenTransportCause проверяет отдельную причину
// тайм-аута фактической передачи файла и доказанное закрытие API на прямой входной границе.
func TestProblemUploadDeadlineAndClosedAPIRetainProvenTransportCause(t *testing.T) {
	// fixture и preparation предоставляют настоящий upload-протокол с конечным сроком чтения;
	// release снимает управляемую блокировку проверки исходного файла после теста.
	fixture, preparation, _, release := newAPIAcceptanceUpload(t, 20*time.Millisecond)
	defer release()
	// asset адресует реальный объект загрузки; err сообщает об отказе создания.
	asset, err := fixture.service.CreateAsset(context.Background(), "Медленная передача")
	if err != nil {
		t.Fatal(err)
	}
	// body удерживает передачу до её штатной отмены и закрытия владельцем.
	body := &apiAcceptanceBlockedUpload{closed: make(chan struct{})}
	defer func() {
		// Закрывает управляемое тело также при раннем выходе теста.
		// closeErr сообщает об отказе освобождения удерживаемого чтения.
		if closeErr := body.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	// response содержит доказанный отказ передачи файла до принятия дальнейшей подготовки.
	response := apiAcceptanceUploadRequest(t, fixture, context.Background(), string(asset.ID), asset.Revision, body, 408)
	requirePublicProblem(t, response, 408, "upload_timeout")
	// ctx ограничивает присоединение задания после отмены передачи; cancel освобождает его таймер.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err = (<-preparation.jobs).Wait(ctx); err == nil {
		t.Fatal("тайм-аут передал файл в успешную подготовку")
	}
	if err = fixture.api.Close(ctx); err != nil {
		t.Fatal(err)
	}
	response = apiAcceptanceRequest(t, fixture.api, "GET", "/streams", "", 503)
	requirePublicProblem(t, response, 503, "api_closed")
}

// TestProblemProfileValidationAndSSEConflictKeepPreparedDetails проверяет прежний подробный
// профильный 422 и прямой SSE-отказ до открытия потока; t сообщает нарушение публичных контрактов.
func TestProblemProfileValidationAndSSEConflictKeepPreparedDetails(t *testing.T) {
	// fixture предоставляет настоящие регистрацию и сохранённый полный профиль.
	fixture := newAPIAcceptanceFixture(t, nil)
	// profile адресует существующее DTO, остальные результаты нужны лишь для связанной фикстуры.
	_, profile, _, _ := apiAcceptanceSeedConfiguration(t, fixture)
	// saved содержит полное публичное представление действующего профиля.
	saved := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", "/transcode-profiles/"+string(profile.ID), "", 200))
	// value сохраняет полный профиль с явно недопустимым LIVE-селектором.
	value := saved["profile"].(map[string]any)
	value["live_audio_mode"] = "synthetic-rejected-secret"
	// body и err содержат JSON профиля с недопустимым селектором и отказ его кодирования.
	body, err := json.Marshal(map[string]any{"name": "Профиль", "profile": value})
	if err != nil {
		t.Fatal(err)
	}
	// response содержит 422, прошедший Huma и преобразователь доверенной профильной ошибки.
	response := apiAcceptanceRequest(t, fixture.api, "POST", "/transcode-profiles", string(body), 422)
	// problem подтверждает сохранённые подготовленные code, field, detail и новое отдельное действие.
	problem := requirePublicProblem(t, response, 422, "invalid_profile")
	if problem["field"] != "profile.live_audio_mode" || problem["detail"] != "Требуется явное значение copy или transcode." || problem["action"] == nil {
		t.Fatal("прежние детали профиля заменены общим ответом", problem)
	}
	// request передаёт конфликтующие позиции до открытия SSE, не создавая подписку.
	request := httptest.NewRequest("GET", api.Prefix+"/events?cursor=first-private-cursor", nil)
	request.Header.Set("Last-Event-ID", "second-private-cursor")
	response = httptest.NewRecorder()
	fixture.api.ServeHTTP(response, request)
	problem = requirePublicProblem(t, response, 400, "sse_cursor_conflict")
	if problem["field"] != "cursor" || strings.Contains(response.Body.String(), "private-cursor") || strings.Contains(response.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatal("SSE отказ раскрыл позиции либо открыл поток", response.Body)
	}
}

// reasonTimedOutBody представляет доказанный тайм-аут чтения JSON без реальной задержки или сети.
type reasonTimedOutBody struct{}

// Read возвращает штатный deadline с признаком net.Error.Timeout; buffer не заполняется.
func (reasonTimedOutBody) Read(buffer []byte) (int, error) { return 0, context.DeadlineExceeded }

// Close не освобождает ресурсов, поскольку тело не создаёт файл, сокет или работника.
func (reasonTimedOutBody) Close() error { return nil }

// TestProblemJSONReadDeadlineIsDifferentFromUploadDeadline проверяет ошибку до handler:
// t наблюдает известный тайм-аут тела JSON, который нельзя объявлять тайм-аутом передачи файла.
func TestProblemJSONReadDeadlineIsDifferentFromUploadDeadline(t *testing.T) {
	// port подтверждает, что не прочитанный JSON не допускается к команде.
	port := &reasonCommandPort{failure: core.ErrBusy}
	// fixture предоставляет настоящие Huma и Transformer.
	fixture := newAPIAcceptanceFixture(t, func(config *api.Config) {
		// Подключает наблюдаемую границу команды для проверки отсутствия вызовов.
		config.Commands = port
	})
	// request получает контролируемый deadline непосредственно от источника тела.
	request := httptest.NewRequest("POST", api.Prefix+"/streams/known/start", reasonTimedOutBody{})
	request.Header.Set("Content-Type", "application/json")
	// response содержит настоящий ответ Huma на отказ чтения перед обработчиком.
	response := httptest.NewRecorder()
	fixture.api.ServeHTTP(response, request)
	// problem содержит общую причину тела, без недоказанной атрибуции загрузке заглушки.
	problem := requirePublicProblem(t, response, 408, "body_read_timeout")
	if strings.Contains(problem["detail"].(string), "файл") || port.calls != 0 {
		t.Fatal("тайм-аут JSON подменён отказом файла либо исполнил команду", problem, port.calls)
	}
}
