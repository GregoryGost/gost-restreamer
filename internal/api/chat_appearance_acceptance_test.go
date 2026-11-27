package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

const (
	// appearanceDefaults фиксирует оформление для потока, которому пользователь ещё не назначил профиль.
	appearanceDefaults = `{
		"dashboard_background":{"enabled":true,"color":"#101823","transparency":0},
		"overlay_background":{"enabled":false,"color":"#0d131d","transparency":0},
		"overlay_transparency":0,
		"message_background":{"enabled":true,"color":"#0c121b","transparency":23},
		"text_outline":{"enabled":false,"color":"#000000","width_px":1,"transparency":0},
		"message_text":{"color":"#eff4fb","size_px":16,"transparency":0}
	}`
	// appearanceCustom различает все цвета и использует крайние значения, включая явно заданные false и 0.
	appearanceCustom = `{
		"dashboard_background":{"enabled":false,"color":"#123456","transparency":100},
		"overlay_background":{"enabled":true,"color":"#234567","transparency":0},
		"overlay_transparency":100,
		"message_background":{"enabled":false,"color":"#345678","transparency":19},
		"text_outline":{"enabled":false,"color":"#456789","width_px":0,"transparency":55},
		"message_text":{"color":"#567890","size_px":72,"transparency":9}
	}`
)

// chatAppearancePutBody связывает полную форму appearance с исходной JSON-лексемой revision.
// Тест передаёт лексему напрямую, чтобы проверять дробные, экспоненциальные и строковые записи.
func chatAppearancePutBody(revision, appearance string) string {
	return `{"expected_revision":` + revision + `,"appearance":` + appearance + `}`
}

// requireChatAppearance сверяет response с точным публичным набором полей, полным профилем
// appearance и числовой revision. Неизвестные свойства в ответе также приводят к ошибке.
func requireChatAppearance(t *testing.T, response *httptest.ResponseRecorder, revision, appearance string) {
	t.Helper()
	apiAcceptanceNoSecrets(t, response.Body.String())
	// got сохраняет реальные JSON-типы и точность ревизии из HTTP-ответа.
	got := apiAcceptanceJSON(t, response)
	// want содержит только два поля узкой проекции и полный независимый эталон настроек.
	want := map[string]any{
		"revision":   json.Number(revision),
		"appearance": numericJSON(t, []byte(appearance)),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Не совпадает ресурс оформления: получено %v, ожидается %v", got, want)
	}
}

// insertChatAppearanceLegacyStream вставляет в databasePath прежнюю версионированную нагрузку
// Stream без поля оформления. SQL-соединение закрывается до чтения строки рабочим API.
func insertChatAppearanceLegacyStream(t *testing.T, databasePath string) {
	t.Helper()
	const (
		// timestamp задаёт значения обеих меток времени SQL в микросекундах для 2026-03-02 12:00:00 UTC.
		timestamp = 1772452800000000
		// payload воспроизводит прежние метаданные с ревизией 2^53 + 1 и синтетическим ключом.
		payload = `{"Version":1,"Payload":{"ID":"legacy-chat-stream","Revision":9007199254740993,"CreatedAt":"2026-03-02T12:00:00Z","UpdatedAt":"2026-03-02T12:00:00Z","Name":"Сохранённый поток","PublicationKey":"synthetic-publication-secret","Fallback":{"AssetID":"","Watchdog":{"VideoTimeout":0,"AudioTimeout":0,"RecoveryWindow":0,"KeyframeTimeout":0}}}}`
	)
	// db обращается только к временной базе фикстуры. err сообщает об отказе создания пула.
	db, err := sql.Open("sqlite3", databasePath+"?_busy_timeout=1000")
	if err != nil {
		t.Fatal(err)
	}
	// err получает результат вставки одной подготовленной строки без преобразования её нагрузки.
	_, err = db.ExecContext(t.Context(), `INSERT INTO streams
		(id, revision, created_at, updated_at, payload, publication_key) VALUES (?, ?, ?, ?, ?, ?)`,
		"legacy-chat-stream", "00009007199254740993", timestamp, timestamp, []byte(payload), "synthetic-publication-secret")
	if err = errors.Join(err, db.Close()); err != nil {
		t.Fatal(err)
	}
}

// TestChatAppearancePersistsWithStreamRevision проверяет чтение прежней нагрузки, точный CAS,
// независимость настройки соседнего потока, сохранение при PATCH и повторном открытии SQLite,
// затем условное удаление Stream и штатный ответ отсутствующего ресурса.
func TestChatAppearancePersistsWithStreamRevision(t *testing.T) {
	// f предоставляет временную SQLite, а handler и manager обслуживают чат над той же базой.
	f, handler, manager := newChatStreamSelfFixture(t, core.DefaultChatLimits())
	insertChatAppearanceLegacyStream(t, f.databasePath)
	// ctx применяется для синтетического потока, открытий и штатного закрытия хранилища.
	ctx := context.Background()
	// other изолирует профиль соседнего потока. err сообщает об отказе создания и открытия базы.
	other, err := f.service.CreateStream(ctx, core.Stream{Name: "Другой поток", PublicationKey: "synthetic-other-key"})
	if err != nil {
		t.Fatal(err)
	}
	const (
		// streamPath адресует прежнюю строку через общий ресурс Stream.
		streamPath = "/streams/legacy-chat-stream"
		// path адресует профиль того же потока через отдельный ресурс чата.
		path = "/chat" + streamPath + "/appearance"
	)
	// otherPath адресует соседний поток, профиль и ревизия которого должны остаться исходными.
	otherPath := "/chat/streams/" + string(other.ID) + "/appearance"
	requireChatAppearance(t, apiAcceptanceRequest(t, handler, http.MethodGet, path, "", http.StatusOK), "9007199254740993", appearanceDefaults)
	requireChatAppearance(t, apiAcceptanceRequest(t, handler, http.MethodGet, otherPath, "", http.StatusOK), "1", appearanceDefaults)
	apiAcceptanceNoSecrets(t, apiAcceptanceRequest(t, handler, http.MethodPut, path, chatAppearancePutBody("9007199254740992", appearanceCustom), http.StatusConflict).Body.String())
	requireChatAppearance(t, apiAcceptanceRequest(t, handler, http.MethodGet, path, "", http.StatusOK), "9007199254740993", appearanceDefaults)
	// exactIntegers передаёт допустимые целые с точкой и степенью, включая верхнюю границу процента.
	exactIntegers := strings.Replace(appearanceCustom, `"overlay_transparency":100`, `"overlay_transparency":1e2`, 1)
	requireChatAppearance(t, apiAcceptanceRequest(t, handler, http.MethodPut, path, chatAppearancePutBody("9007199254740993.0", exactIntegers), http.StatusOK), "9007199254740994", appearanceCustom)
	// patch показывает общую ревизию Stream и фактически сохранённое новое имя.
	patch := apiAcceptanceJSON(t, apiAcceptanceRequest(t, handler, http.MethodPatch, streamPath,
		`{"expected_revision":9007199254740994,"name":"Переименованный поток"}`, http.StatusOK))
	numericWant(t, patch, "revision", "9007199254740995")
	if patch["name"] != "Переименованный поток" || patch["publication_key"] != "synthetic-publication-secret" {
		t.Fatal("PATCH потерял имя или ключ потока")
	}
	apiAcceptanceRequest(t, handler, http.MethodPut, path, chatAppearancePutBody("9007199254740994", appearanceDefaults), http.StatusConflict)
	requireChatAppearance(t, apiAcceptanceRequest(t, handler, http.MethodGet, path, "", http.StatusOK), "9007199254740995", appearanceCustom)
	requireChatAppearance(t, apiAcceptanceRequest(t, handler, http.MethodGet, otherPath, "", http.StatusOK), "1", appearanceDefaults)
	if err = errors.Join(handler.Close(ctx), manager.Close(ctx), f.api.Close(ctx), f.store.Close()); err != nil {
		t.Fatal(err)
	}
	// store открывает тот же файл после закрытия первоначального пула и владельцев.
	store, err := storage.Open(ctx, f.databasePath, apiAcceptanceStorageOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает новый пул после зарегистрированного ниже API и Manager.
		// err содержит результат освобождения базы при любом исходе теста.
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	// service получает новый экземпляр полномочий базы после её открытия.
	service, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// reopened обслуживает тот же профиль через независимо собранный API.
	reopened, _ := newChatAcceptanceHandler(t, &apiAcceptanceFixture{service: service, store: store, limits: f.limits}, core.DefaultChatLimits(), nil)
	requireChatAppearance(t, apiAcceptanceRequest(t, reopened, http.MethodGet, path, "", http.StatusOK), "9007199254740995", appearanceCustom)
	apiAcceptanceRequest(t, reopened, http.MethodDelete, streamPath+"?expected_revision=9007199254740994", "", http.StatusConflict)
	requireChatAppearance(t, apiAcceptanceRequest(t, reopened, http.MethodGet, path, "", http.StatusOK), "9007199254740995", appearanceCustom)
	apiAcceptanceRequest(t, reopened, http.MethodDelete, streamPath+"?expected_revision=9007199254740995", "", http.StatusNoContent)
	apiAcceptanceNoSecrets(t, apiAcceptanceRequest(t, reopened, http.MethodGet, path, "", http.StatusNotFound).Body.String())
	apiAcceptanceNoSecrets(t, apiAcceptanceRequest(t, reopened, http.MethodPut, path, chatAppearancePutBody("9007199254740995", appearanceCustom), http.StatusNotFound).Body.String())
}

// TestChatAppearanceRejectsIncompleteAndInvalidReplacements проверяет обязательность полей,
// границы значений и сохранение прежней ревизии и профиля при каждом отказе.
func TestChatAppearanceRejectsIncompleteAndInvalidReplacements(t *testing.T) {
	// f предоставляет локальный доменный сервис, а handler регистрирует действующий маршрут чата.
	f, handler, _ := newChatStreamSelfFixture(t, core.DefaultChatLimits())
	// stream хранит профиль под отдельной ревизией. err сообщает об отказе создания Stream.
	stream, err := f.service.CreateStream(t.Context(), core.Stream{Name: "Настраиваемый поток", PublicationKey: "synthetic-publication-secret"})
	if err != nil {
		t.Fatal(err)
	}
	// path адресует профиль этого потока. body сохраняет полный допустимый запрос как основу замен.
	path := "/chat/streams/" + string(stream.ID) + "/appearance"
	body := chatAppearancePutBody("2", appearanceCustom)
	apiAcceptanceRequest(t, handler, http.MethodPut, path, chatAppearancePutBody("1", appearanceCustom), http.StatusOK)
	// cases составляют независимые нарушения формы, типов и опубликованных числовых границ.
	cases := []struct {
		// name описывает проверяемый отказ.
		name string
		// from указывает фрагмент полного допустимого запроса.
		from string
		// to заменяет только этот фрагмент, сохраняя синтаксически корректный JSON.
		to string
	}{
		{"без ревизии", `"expected_revision":2,`, ""},
		{"null вместо ревизии", `"expected_revision":2`, `"expected_revision":null`},
		{"строка вместо ревизии", `"expected_revision":2`, `"expected_revision":"2"`},
		{"нулевая ревизия", `"expected_revision":2`, `"expected_revision":0`},
		{"переполнение ревизии", `"expected_revision":2`, `"expected_revision":18446744073709551616`},
		{"без профиля", `,"appearance":` + appearanceCustom, ""},
		{"null вместо профиля", `"appearance":` + appearanceCustom, `"appearance":null`},
		{"без фона Dashboard", `"dashboard_background":{"enabled":false,"color":"#123456","transparency":100},`, ""},
		{"null вместо фона сообщений", `"message_background":{"enabled":false,"color":"#345678","transparency":19}`, `"message_background":null`},
		{"без флага включения", `"enabled":false,"color":"#123456"`, `"color":"#123456"`},
		{"null вместо флага включения", `"enabled":false,"color":"#123456"`, `"enabled":null,"color":"#123456"`},
		{"короткий цвет даже для выключенного фона", `"#123456"`, `"#123"`},
		{"не шестнадцатеричный цвет", `"#456789"`, `"#45678g"`},
		{"цвет с альфа-каналом", `"#567890"`, `"#56789000"`},
		{"пустой цвет", `"#234567"`, `""`},
		{"без общей прозрачности", `"overlay_transparency":100,`, ""},
		{"null вместо общей прозрачности", `"overlay_transparency":100`, `"overlay_transparency":null`},
		{"строка вместо процента", `"overlay_transparency":100`, `"overlay_transparency":"100"`},
		{"дробный процент", `"overlay_transparency":100`, `"overlay_transparency":99.5`},
		{"отрицательный процент", `"transparency":19`, `"transparency":-1`},
		{"процент выше максимума", `"overlay_transparency":100`, `"overlay_transparency":101`},
		{"без ширины обводки", `"width_px":0,`, ""},
		{"отрицательная ширина обводки", `"width_px":0`, `"width_px":-1`},
		{"ширина обводки выше максимума", `"width_px":0`, `"width_px":9`},
		{"null вместо размера текста", `"size_px":72`, `"size_px":null`},
		{"размер текста ниже минимума", `"size_px":72`, `"size_px":7`},
		{"размер текста выше максимума", `"size_px":72`, `"size_px":73`},
	}
	// tc проверяет одну замену на сохранённой ревизии 2 и повторно читает состояние после отказа.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет, что отклонённая замена оставила исходные данные доступными по GET.
			// invalid содержит исходный запрос с одним нарушением формы или границы.
			invalid := strings.Replace(body, tc.from, tc.to, 1)
			if invalid == body || !json.Valid([]byte(invalid)) {
				t.Fatalf("Фикстура не воспроизвела замену %q", tc.from)
			}
			apiAcceptanceNoSecrets(t, apiAcceptanceRequest(t, handler, http.MethodPut, path, invalid, http.StatusUnprocessableEntity).Body.String())
			requireChatAppearance(t, apiAcceptanceRequest(t, handler, http.MethodGet, path, "", http.StatusOK), "2", appearanceCustom)
		})
	}
	// otherBoundaries включает обводку максимальной толщины и уменьшает шрифт до допустимого минимума.
	otherBoundaries := strings.NewReplacer(`"enabled":false,"color":"#456789"`, `"enabled":true,"color":"#456789"`,
		`"width_px":0`, `"width_px":8`, `"size_px":72`, `"size_px":8`).Replace(appearanceCustom)
	requireChatAppearance(t, apiAcceptanceRequest(t, handler, http.MethodPut, path, chatAppearancePutBody("2", otherBoundaries), http.StatusOK), "3", otherBoundaries)
}

// TestChatAppearanceRejectsNonExactJSONKeys проверяет регистр и дубли имён на каждом уровне тела PUT.
// t предоставляет изолированный контекст теста и дочерние контексты для каждого нарушения.
func TestChatAppearanceRejectsNonExactJSONKeys(t *testing.T) {
	// f предоставляет временную базу, а handler обрабатывает запросы через зарегистрированный Huma API.
	f, handler, _ := newChatStreamSelfFixture(t, core.DefaultChatLimits())
	// stream задаёт исходную ревизию, которая не должна измениться после ошибок декодирования.
	// err сообщает об отказе создания тестового потока.
	stream, err := f.service.CreateStream(t.Context(), core.Stream{Name: "Поток точных ключей", PublicationKey: "synthetic-publication-secret"})
	if err != nil {
		t.Fatal(err)
	}
	// path адресует только профиль тестового потока.
	path := "/chat/streams/" + string(stream.ID) + "/appearance"
	// body содержит полный корректный профиль для независимых замен имён и дубликатов.
	body := chatAppearancePutBody("1", appearanceCustom)
	// cases покрывает смешанный регистр, неизвестный ключ и дубли в корневом, общем и вложенных объектах.
	cases := []struct {
		// name называет одно нарушение строгого набора ключей.
		name string
		// invalid содержит тело запроса с одним нарушением поверх корректной формы.
		invalid string
	}{
		{
			"дублирующее имя appearance в другом регистре",
			strings.TrimSuffix(body, "}") + `,"APPEARANCE":` + appearanceDefaults + `}`,
		},
		{
			"дублирующее имя enabled в другом регистре",
			strings.Replace(body, `"enabled":false`, `"enabled":false,"ENABLED":true`, 1),
		},
		{
			"имя overlay_transparency в другом регистре",
			strings.Replace(body, `"overlay_transparency":100`, `"OVERLAY_TRANSPARENCY":100`, 1),
		},
		{
			"неизвестное имя ширины обводки",
			strings.Replace(body, `"width_px":0`, `"stroke_width":0`, 1),
		},
		{
			"дублирующий точный ключ size_px",
			strings.Replace(body, `"size_px":72`, `"size_px":72,"size_px":16`, 1),
		},
	}
	// tc содержит очередной некорректный вариант, который не должен изменить сохранённый профиль.
	for _, tc := range cases {
		// Вложенная функция проверяет ответ на tc.invalid и неизменность настроек через дочерний контекст t.
		t.Run(tc.name, func(t *testing.T) {
			// invalid является конкретной JSON-лексемой, переданной декодеру API без предварительной нормализации.
			if !json.Valid([]byte(tc.invalid)) {
				t.Fatalf("Некорректная JSON-фикстура: %q", tc.name)
			}
			// response должен содержать безопасную Problem Details ошибку недопустимой формы полного тела.
			response := apiAcceptanceRequest(t, handler, http.MethodPut, path, tc.invalid, http.StatusUnprocessableEntity)
			apiAcceptanceNoSecrets(t, response.Body.String())
			requireChatAppearance(t, apiAcceptanceRequest(t, handler, http.MethodGet, path, "", http.StatusOK), "1", appearanceDefaults)
		})
	}
}

// TestChatAppearanceIsExcludedFromSessionHistory запускает Session с сохранённым оформлением
// и проверяет объект из SQLite, его HTTP-проекцию и доменную границу проверки снимка.
func TestChatAppearanceIsExcludedFromSessionHistory(t *testing.T) {
	// f и preparation используют действующий менеджер с синтетическими медиаинструментами.
	// release освобождает управляемую инспекцию при завершении теста.
	f, preparation, _, release := newAPIAcceptanceUpload(t, time.Second)
	defer release()
	// ctx ограничивает подготовку и запуск, cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// asset даёт готовое поколение для создания снимка. err сообщает об отказах домена и кодека.
	asset, err := f.service.CreateAsset(ctx, "Заглушка")
	if err != nil {
		t.Fatal(err)
	}
	// stream связывает настроенный поток с заглушкой, чтобы реальный StartSession смог создать историю.
	stream, err := f.service.CreateStream(ctx, core.Stream{Name: "Поток со снимком", PublicationKey: "synthetic-publication-secret",
		Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second,
			RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	// handler публикует настройку оформления над тем же временным хранилищем без адаптеров площадок.
	handler, _ := newChatAcceptanceHandler(t, f, core.DefaultChatLimits(), nil)
	// path адресует настройки потока, для которого будет сохранён снимок запуска.
	path := "/chat/streams/" + string(stream.ID) + "/appearance"
	requireChatAppearance(t, apiAcceptanceRequest(t, handler, http.MethodPut, path, chatAppearancePutBody("1", appearanceCustom), http.StatusOK), "2", appearanceCustom)
	// body содержит небольшой PNG для подготовки поколения без FFmpeg и внешних соединений.
	body := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	apiAcceptanceUploadRequest(t, f, ctx, string(asset.ID), asset.Revision, body, http.StatusAccepted)
	select {
	// job получает принятое менеджером задание, готовность которого нужна для StartSession.
	case job := <-preparation.jobs:
		if _, err = job.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// receipt содержит идентификатор настоящего STARTING-запуска, записанного без запуска медиапроцессов.
	receipt, err := f.service.StartSession(ctx, core.StartSessionRequest{RequestID: "chat-appearance-session", StreamID: stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// session содержит снимок запуска, прочитанный из репозитория SQLite обычным доменным путём.
	session, err := f.service.GetSession(ctx, receipt.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// current подтверждает, что активный профиль хранится в конфигурации потока в той же базе.
	current, err := f.service.GetStream(ctx, stream.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ChatAppearance == nil || session.Snapshot.Stream.ChatAppearance != nil || session.Snapshot.Stream.Revision != current.Revision {
		t.Fatal("Текущий профиль потерян либо попал в сохранённый снимок запуска")
	}
	// persisted содержит байты снимка для проверки отсутствия пользовательских данных оформления.
	persisted, err := core.EncodeSessionSnapshot(session.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	// response содержит публичное представление той же Session.
	response := apiAcceptanceRequest(t, f.api, http.MethodGet, "/sessions/"+string(receipt.SessionID), "", http.StatusOK)
	// data проходит по долговечной и публичной форме истории для независимой проверки отсутствия профиля.
	for _, data := range [][]byte{persisted, response.Body.Bytes()} {
		if bytes.Contains(data, []byte("#123456")) {
			t.Fatal("История запуска раскрывает цвет пользовательского профиля")
		}
	}
	// contaminated моделирует неверную запись текущей конфигурации потока в долговечную историю.
	contaminated := session.Snapshot
	contaminated.Stream = current
	if err = contaminated.Validate(); err == nil {
		t.Fatal("Валидация приняла профиль оформления в снимке запуска")
	}
	requireChatAppearance(t, apiAcceptanceRequest(t, handler, http.MethodGet, path, "", http.StatusOK), "2", appearanceCustom)
}

// TestChatAppearanceOpenAPI проверяет, что опубликованные GET и PUT используют объект настроек
// и показывают машинным клиентам реальные типы, обязательность полей и границы ревизии.
func TestChatAppearanceOpenAPI(t *testing.T) {
	// handler генерирует OpenAPI при обычной регистрации маршрутов API.
	_, handler, _ := newChatStreamSelfFixture(t, core.DefaultChatLimits())
	// spec сохраняет точные числовые лексемы схемы, включая верхнюю границу uint64.
	spec := apiAcceptanceJSON(t, apiAcceptanceRequest(t, handler, http.MethodGet, "/openapi.json", "", http.StatusOK))
	// operation задаёт путь обеих операций в опубликованной схеме.
	const operation = "paths./api/v1/chat/streams/{streamId}/appearance"
	// method обозначает очередную опубликованную операцию, которую клиенты читают из OpenAPI.
	for _, method := range []string{"get", "put"} {
		// output сверяет поля ответа независимо от имени компонента, выбранного Huma.
		output := chatAppearanceSchema(t, spec, operation+"."+method+".responses.200.content.application/json.schema")
		if len(output) != 2 || output["revision"] == nil || output["appearance"] == nil {
			t.Fatalf("%s: неверная схема ответа %v", method, output)
		}
	}
	// input описывает два поля полной атомарной замены, опубликованные для клиентских генераторов.
	input := chatAppearanceSchema(t, spec, operation+".put.requestBody.content.application/json.schema")
	if len(input) != 2 || input["appearance"] == nil {
		t.Fatalf("Неверная форма запроса: %v", input)
	}
	if numericAt(t, input, "expected_revision.type") != "integer" {
		t.Fatal("OpenAPI описывает ревизию иначе, чем целое число")
	}
	numericWant(t, input, "expected_revision.minimum", "1")
	numericWant(t, input, "expected_revision.maximum", "18446744073709551615")
}

// chatAppearanceSchema читает из spec свойства объекта по path и сверяет их обязательность.
// Ссылки Huma разрешаются через фактически опубликованный раздел components.schemas.
func chatAppearanceSchema(t *testing.T, spec map[string]any, path string) map[string]any {
	t.Helper()
	// schema содержит ссылку на компонент либо встроенную схему объекта ресурса.
	schema := numericAt(t, spec, path).(map[string]any)
	// reference получает опубликованное имя схемы, linked отличает ссылку от встроенного объекта.
	if reference, linked := schema["$ref"].(string); linked {
		// name содержит имя компонента. ok подтверждает локальную ссылку на схемы OpenAPI.
		name, ok := strings.CutPrefix(reference, "#/components/schemas/")
		if !ok {
			t.Fatalf("Неизвестная ссылка на схему %q", reference)
		}
		schema = numericAt(t, spec, "components.schemas."+name).(map[string]any)
	}
	// properties содержит фактические свойства объекта, required перечисляет обязательные.
	properties := numericAt(t, schema, "properties").(map[string]any)
	required := numericAt(t, schema, "required").([]any)
	if len(required) != len(properties) {
		t.Fatalf("Схема %s допускает частичный объект: %v", path, required)
	}
	// name содержит очередное имя обязательного поля в сгенерированной схеме.
	for _, name := range required {
		if properties[fmt.Sprint(name)] == nil {
			t.Fatalf("Схема %s требует необъявленное свойство %v", path, name)
		}
	}
	return properties
}
