package api_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/chat"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// chatControlYouTube имитирует проверку ключа без соединений и платформенных данных.
type chatControlYouTube struct{}

// Provider связывает проверку фикстуры с маршрутом подключения YouTube.
func (chatControlYouTube) Provider() core.ChatProvider { return core.ChatYouTube }

// ValidateAPIKey принимает тестовый ключ или ждёт отмены проверки специально обозначенного ключа.
func (chatControlYouTube) ValidateAPIKey(ctx context.Context, key string) (core.ChatReason, error) {
	if key == "synthetic-pending-key" {
		<-ctx.Done()
		return core.ChatReasonNone, ctx.Err()
	}
	if key == "synthetic-rejected-key" {
		return core.ChatReasonCredentials, fmt.Errorf("исходный результат площадки: %s", key)
	}
	return core.ChatReasonNone, nil
}

// Discover возвращает отказ, если сценарий управления неожиданно пытается читать канал без запуска.
func (chatControlYouTube) Discover(context.Context, core.ChatSource, core.ChatCredentials) (core.ChatDiscovery, error) {
	return core.ChatDiscovery{}, core.ErrNotReady
}

// Read возвращает отказ, если сценарий управления неожиданно пытается читать сообщения.
func (chatControlYouTube) Read(context.Context, core.ChatReadRequest, core.ChatAdapterSink) error {
	return core.ErrNotReady
}

// chatControlFixture подключает Manager и настоящий HTTP API к временной SQLite общей фикстуры.
func chatControlFixture(t *testing.T) (*apiAcceptanceFixture, *api.API) {
	t.Helper()
	// fixture владеет тестовой SQLite и доменной конфигурацией.
	fixture := newAPIAcceptanceFixture(t, nil)
	// limits содержит проверенные бюджеты чата с меньшим пределом тела для HTTP-сценария.
	limits := core.DefaultChatLimits()
	limits.RESTBodyBytes = 256
	// manager выполняет локальные проверки и завершается до закрытия SQLite. err сообщает об отказе запуска.
	manager, err := chat.New(context.Background(), fixture.service, fixture.store, limits, []core.ChatAdapterPort{chatControlYouTube{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Ждёт контролируемые тестом операции Manager, прежде чем общая фикстура закроет SQLite.
		// ctx ограничивает ожидание, cancel освобождает таймер.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		// err сообщает об отказе остановки Manager.
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	// apiLimits содержит меньший лимит общего REST для проверки независимого управления бюджетами.
	apiLimits := fixture.limits
	apiLimits.JSONBytes = 64
	// handler обслуживает настоящий API с чатом, err сообщает об отказе регистрации.
	handler, err := api.New(api.Config{
		Core: fixture.service, Chat: manager, ChatLimits: limits, Limits: apiLimits,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Завершает HTTP API перед Manager в обратном порядке регистрации владельцев.
		// ctx ограничивает ожидание, cancel освобождает таймер.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		// err сообщает об отказе остановки HTTP API.
		if err := handler.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return fixture, handler
}

// awaitChatControlAuthorization читает публичный результат попытки до ожидаемого состояния.
func awaitChatControlAuthorization(t *testing.T, handler *api.API, path, state string) map[string]any {
	t.Helper()
	// deadline ограничивает ожидание работы синтетического адаптера и локальной SQLite.
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	// interval разделяет чтения оперативного результата попытки.
	interval := time.NewTicker(5 * time.Millisecond)
	defer interval.Stop()
	for {
		// value содержит последнее публичное состояние по выданной ссылке авторизации.
		value := apiAcceptanceJSON(t, apiAcceptanceRequest(t, handler, "GET", path, "", 200))
		if value["state"] == state {
			return value
		}
		select {
		case <-deadline.C:
			t.Fatalf("Не получено состояние %s, последнее состояние %v.", state, value)
		case <-interval.C:
		}
	}
}

// TestChatControlSourceConfiguration проверяет закрытые Options, разрешённые пустые значения,
// привязку курсора к назначению, каскад зависимостей и отдельный REST-бюджет через Huma.
func TestChatControlSourceConfiguration(t *testing.T) {
	// fixture сохраняет временную конфигурацию, handler предоставляет маршруты чата.
	fixture, handler := chatControlFixture(t)
	// stream и group связывают оба назначения, destination задаёт родителя создаваемых источников.
	stream, _, group, destination := apiAcceptanceSeedConfiguration(t, fixture)
	// other использует тот же поток, но проверяет несовместимость курсоров разных назначений.
	// err сообщает об отказе подготовки родителя пустого списка.
	other, err := fixture.service.CreateDestination(t.Context(), core.Destination{
		Name: "Другое назначение", StreamID: stream.ID, GroupID: group.ID,
		Platform: "custom", Endpoint: "rtmps://localhost/other", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// account содержит тестовую конфигурацию, подготовленную без внешних запросов. err относится к её сохранению.
	account, err := fixture.service.CreateChatAccount(t.Context(), core.ChatAccount{Provider: core.ChatTwitch},
		core.ChatCredentialBundle{Twitch: &core.TwitchChatCredentials{
			ClientID: "client", UserID: "42", AccessToken: "test-token", RefreshToken: "test-refresh",
			Scopes: []string{"user:read:chat"}, ExpiresAt: time.Now().UTC().Truncate(time.Microsecond),
		}}, "test-write")
	if err != nil {
		t.Fatal(err)
	}
	// path адресует коллекцию назначения, otherPath адресует другую коллекцию с пустым результатом.
	path := "/destinations/" + string(destination.ID) + "/chat-sources"
	otherPath := "/destinations/" + string(other.ID) + "/chat-sources"
	// body содержит явное enabled и математически целую версию с допустимым пустым ChannelHint Twitch.
	body := fmt.Sprintf(`{"provider":"twitch","account_id":%q,"channel_hint":"","options":{"version":1e0,"twitch":{}},"enabled":true,"name":"Эфир"}`, account.Account.ID)
	apiAcceptanceRequest(t, handler, "POST", "/streams", `{"name":"`+strings.Repeat("x", 65)+`","publication_key":"local"}`, 413)
	// variant задаёт запрещённую форму одного свойства, сохраняя остальные поля запроса.
	for _, variant := range []string{
		strings.Replace(body, `,"enabled":true`, "", 1),
		strings.Replace(body, `"enabled":true`, `"enabled":null`, 1),
		strings.Replace(body, `"twitch":{}`, `"twitch":{"video_id":"лишнее"}`, 1),
		strings.Replace(body, `"twitch":{}`, `"twitch":{},"youtube":null`, 1),
		strings.Replace(body, `"twitch":{}`, `"twitch":{},"twitch":{}`, 1),
		strings.Replace(body, `"twitch":{}`, `"youtube":{}`, 1),
		strings.Replace(body, `"version":1e0`, `"version":"1"`, 1),
	} {
		apiAcceptanceRequest(t, handler, "POST", path, variant, 422)
	}
	apiAcceptanceRequest(t, handler, "POST", "/destinations/unknown/chat-sources", body, 404)
	apiAcceptanceRequest(t, handler, "GET", "/destinations/unknown/chat-sources", "", 404)
	apiAcceptanceRequest(t, handler, "POST", path, body+strings.Repeat(" ", 256), 413)
	// created содержит успешный ответ создания с Location и конфигурацией.
	created := apiAcceptanceRequest(t, handler, "POST", path, body, 201)
	// source содержит публичную конфигурацию, а sourcePath адресует созданный ресурс без общего префикса.
	source := apiAcceptanceJSON(t, created)
	sourcePath := "/chat-sources/" + source["id"].(string)
	if created.Header().Get("Location") != api.Prefix+sourcePath || source["enabled"] != true {
		t.Fatalf("Некорректный ответ создания источника: %s", created.Body.String())
	}
	apiAcceptanceRequest(t, handler, "POST", path, body, 201)
	// page сохраняет результат ограниченного чтения, cursor содержит выданную позицию.
	page := apiAcceptanceJSON(t, apiAcceptanceRequest(t, handler, "GET", path+"?limit=1", "", 200))
	cursor := url.QueryEscape(page["next_cursor"].(string))
	if len(page["items"].([]any)) != 1 || cursor == "" {
		t.Fatalf("Нет ожидаемой страницы и позиции: %v", page)
	}
	apiAcceptanceRequest(t, handler, "GET", otherPath+"?cursor="+cursor, "", 400)
	apiAcceptanceRequest(t, handler, "GET", path+"?cursor="+cursor, "", 200)
	apiAcceptanceRequest(t, handler, "PATCH", sourcePath, `{"expected_revision":1,"enabled":null}`, 422)
	apiAcceptanceRequest(t, handler, "PATCH", sourcePath, `{"expected_revision":9007199254740993,"enabled":false}`, 409)
	// patch содержит сохранённый false, пустое имя и новую точную ревизию.
	patch := apiAcceptanceJSON(t, apiAcceptanceRequest(t, handler, "PATCH", sourcePath, `{"expected_revision":1e0,"enabled":false,"name":""}`, 200))
	if patch["enabled"] != false || patch["name"] != "" || patch["channel_hint"] != "" {
		t.Fatalf("Потеряны явные нулевые значения PATCH: %v", patch)
	}
	numericWant(t, patch, "revision", "2")
	apiAcceptanceRequest(t, handler, "GET", sourcePath, "", 200)
	apiAcceptanceRequest(t, handler, "GET", sourcePath+"/broadcast-selection?session_id=obsolete", "", 409)
	apiAcceptanceRequest(t, handler, "GET", "/chat-sources/unknown/broadcast-selection?session_id=obsolete", "", 404)
	apiAcceptanceRequest(t, handler, "POST", sourcePath+"/broadcast-selection", `{"session_id":"obsolete","expected_revision":2,"discovery_generation":9007199254740993,"video_id":"video"}`, 409)
	// accountPath адресует аккаунт с зависимыми источниками для проверки запрета удаления.
	accountPath := "/chat/accounts/" + string(account.Account.ID)
	// conflict содержит безопасный ответ на удаление используемого аккаунта.
	conflict := apiAcceptanceJSON(t, apiAcceptanceRequest(t, handler, "DELETE", accountPath+"?expected_revision=1", "", 409))
	if conflict["code"] != "resource_in_use" {
		t.Fatalf("Некорректная причина запрета удаления: %v", conflict)
	}
	apiAcceptanceRequest(t, handler, "DELETE", sourcePath+"?expected_revision=2", "", 204)
	apiAcceptanceRequest(t, handler, "GET", sourcePath, "", 404)
}

// TestChatControlAuthorizationWire проверяет 202 и Location, читаемое состояние попыток,
// отмену, конфигурационные метаданные и отсутствие исходных реквизитов в публичных ответах.
func TestChatControlAuthorizationWire(t *testing.T) {
	// handler связывает роутер с временной SQLite и синтетическим адаптером.
	_, handler := chatControlFixture(t)
	// connectPath адресует единственную операцию подключения всех площадок.
	const connectPath = "/chat/accounts/connect"
	// body перебирает несовместимые публичные поля и неполную пару замены.
	for _, body := range []string{
		`{"provider":"youtube","api_key":"synthetic-key","client_id":"лишнее"}`,
		`{"provider":"twitch","client_id":"client","client_secret":"лишнее"}`,
		`{"provider":"vk_video_live","client_id":"client","client_secret":null}`,
		`{"provider":"youtube","api_key":"synthetic-key","account_id":"missing"}`,
		`{"provider":"youtube","api_key":"synthetic-key","expected_revision":1}`,
	} {
		// rejected содержит безопасный ответ вместо исходного запроса и его реквизитов.
		rejected := apiAcceptanceRequest(t, handler, "POST", connectPath, body, 422)
		if strings.Contains(rejected.Body.String(), "synthetic-key") || strings.Contains(rejected.Body.String(), "лишнее") {
			t.Fatal("Ответ содержит отклонённое значение запроса.")
		}
	}
	apiAcceptanceRequest(t, handler, "POST", connectPath, `{"provider":"twitch","client_id":"client"}`, 503)
	apiAcceptanceRequest(t, handler, "POST", connectPath, `{"provider":"vk_video_live","client_id":"client","client_secret":"synthetic-secret"}`, 503)
	// acceptance содержит ссылку и начальное состояние успешной RAM-попытки.
	acceptance := apiAcceptanceRequest(t, handler, "POST", connectPath, `{"provider":"youtube","api_key":"synthetic-accepted-key","name":"Канал"}`, 202)
	// pending содержит начальные поля, path адресует текущую попытку для отдельного чтения.
	pending := apiAcceptanceJSON(t, acceptance)
	path := "/chat/accounts/authorizations/" + pending["attempt_id"].(string)
	if acceptance.Header().Get("Location") != api.Prefix+path || pending["state"] != "PENDING" || pending["provider"] != "youtube" || pending["cancelable"] != true || strings.Contains(acceptance.Body.String(), "synthetic-accepted-key") {
		t.Fatalf("Некорректная начальная проекция: %s", acceptance.Body.String())
	}
	// succeeded содержит подтверждённый аккаунт, accountPath адресует читаемую конфигурацию.
	succeeded := awaitChatControlAuthorization(t, handler, path, "SUCCEEDED")
	accountPath := "/chat/accounts/" + succeeded["account_id"].(string)
	// metadata содержит публичную проекцию без реквизитов и их независимой ревизии.
	metadata := apiAcceptanceRequest(t, handler, "GET", accountPath, "", 200)
	if strings.Contains(metadata.Body.String(), "synthetic-accepted-key") || !strings.Contains(metadata.Body.String(), `"authorization_state":"CONFIGURED"`) {
		t.Fatalf("Некорректные метаданные аккаунта: %s", metadata.Body.String())
	}
	apiAcceptanceRequest(t, handler, "PATCH", accountPath, `{"expected_revision":1,"name":null}`, 422)
	apiAcceptanceRequest(t, handler, "PATCH", accountPath, `{"expected_revision":1,"name":""}`, 200)
	apiAcceptanceRequest(t, handler, "GET", "/chat/accounts?limit=1", "", 200)
	apiAcceptanceRequest(t, handler, "DELETE", accountPath+"?expected_revision=2", "", 204)
	apiAcceptanceRequest(t, handler, "DELETE", path, "", 409)
	// rejected содержит принятую попытку, чей синтетический адаптер вернёт отказ с исходной диагностикой.
	rejected := apiAcceptanceRequest(t, handler, "POST", connectPath, `{"provider":"youtube","api_key":"synthetic-rejected-key"}`, 202)
	// failed содержит публичную причину отказа вместо текста платформенной ошибки.
	failed := awaitChatControlAuthorization(t, handler, strings.TrimPrefix(rejected.Header().Get("Location"), api.Prefix), "FAILED")
	if failed["code"] != "credentials_required" || failed["cancelable"] != false || failed["account_id"] != nil {
		t.Fatalf("Некорректная проекция отказа: %v", failed)
	}
	// cancellable содержит принятую попытку с адаптером, ожидающим отмены.
	cancellable := apiAcceptanceRequest(t, handler, "POST", connectPath, `{"provider":"youtube","api_key":"synthetic-pending-key"}`, 202)
	// cancelPath адресует незавершённую попытку, для которой DELETE идемпотентен.
	cancelPath := strings.TrimPrefix(cancellable.Header().Get("Location"), api.Prefix)
	apiAcceptanceRequest(t, handler, "DELETE", cancelPath, "", 204)
	apiAcceptanceRequest(t, handler, "DELETE", cancelPath, "", 204)
	awaitChatControlAuthorization(t, handler, cancelPath, "CANCELLED")
	apiAcceptanceRequest(t, handler, "GET", "/chat/accounts/authorizations/unknown", "", 404)
}

// TestChatControlOpenAPI проверяет опубликованные схемы точных целых, закрытые Options и коды авторизации.
func TestChatControlOpenAPI(t *testing.T) {
	// handler публикует спецификацию настоящей регистрации Huma.
	_, handler := chatControlFixture(t)
	// spec содержит JSON OpenAPI с исходными числовыми лексемами.
	spec := apiAcceptanceJSON(t, apiAcceptanceRequest(t, handler, "GET", "/openapi.json", "", 200))
	// schemas адресует общий раздел компонентов, сформированный после регистрации REST.
	const schemas = "components.schemas."
	// path указывает проверяемый типизированный запрос или ответ, field содержит имя точного целого.
	for path, field := range map[string]string{
		"ChatSourcePatchBody": "expected_revision", "ChatConnectBody": "expected_revision",
		"ChatBroadcastSelectBody": "discovery_generation", "ChatBroadcastChoicesDTO": "source_revision",
	} {
		// property содержит опубликованные границы числового поля DTO.
		property := numericAt(t, spec, schemas+path+".properties."+field).(map[string]any)
		if property["type"] != "integer" {
			t.Fatalf("%s.%s должен быть целым: %v", path, field, property)
		}
		numericWant(t, property, "minimum", "1")
		numericWant(t, property, "maximum", "18446744073709551615")
	}
	// replacement содержит взаимные зависимости идентификатора и ревизии в запросе подключения.
	replacement := numericAt(t, spec, schemas+"ChatConnectBody.dependentRequired").(map[string]any)
	if fmt.Sprint(replacement["account_id"]) != "[expected_revision]" || fmt.Sprint(replacement["expected_revision"]) != "[account_id]" {
		t.Fatalf("В схеме подключения не выражена обязательная пара замены: %v", replacement)
	}
	// authorizationVariants описывает три точных набора исходных реквизитов.
	authorizationVariants := numericAt(t, spec, schemas+"ChatConnectBody.oneOf").([]any)
	if len(authorizationVariants) != 3 || fmt.Sprint(authorizationVariants[0].(map[string]any)["required"]) != "[provider api_key]" {
		t.Fatalf("Схема подключения не описывает разные наборы реквизитов: %v", authorizationVariants)
	}
	// options содержит закрытое объединение вариантов создания источника.
	options := numericAt(t, spec, schemas+"ChatSourceCreateBody.properties.options.oneOf").([]any)
	if len(options) != 3 {
		t.Fatalf("Неверный каталог настроек чатов: %v", options)
	}
	// variant содержит очередную независимо закрытую форму конкретной площадки.
	for _, variant := range options {
		if variant.(map[string]any)["additionalProperties"] != false {
			t.Fatalf("Вариант настроек допускает неизвестные поля: %v", variant)
		}
	}
	// yaml содержит ту же спецификацию, сериализованную сервером в YAML.
	yaml := apiAcceptanceRequest(t, handler, "GET", "/openapi.yaml", "", 200)
	if !strings.Contains(yaml.Body.String(), "18446744073709551615") || !strings.Contains(yaml.Body.String(), "credential_commit_unknown") {
		t.Fatal("В OpenAPI YAML отсутствуют точная граница uint64 или специальная причина авторизации.")
	}
}
