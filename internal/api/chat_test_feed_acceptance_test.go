package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestChatTestFeedIntervalHTTP проверяет границы точного JSON-числа, значение по умолчанию,
// повторный старт и отказы для неизвестного потока через действующие маршруты Huma.
func TestChatTestFeedIntervalHTTP(t *testing.T) {
	// fixture и handler обслуживают команды через один Service с временным хранилищем.
	fixture, handler, _ := newChatStreamSelfFixture(t, core.DefaultChatLimits())
	// stream адресует поток без эфира, err сообщает об отказе его создания.
	stream, err := fixture.service.CreateStream(t.Context(), core.Stream{Name: "Тест интервала", PublicationKey: "test-interval"})
	if err != nil {
		t.Fatal(err)
	}
	// root задаёт путь потока внутри префикса API.
	root := "/chat/streams/" + string(stream.ID)
	// body перечисляет корректные JSON-тела с недопустимым типом или значением интервала.
	for _, body := range []string{
		`{"interval_ms":99}`, `{"interval_ms":60001}`, `{"interval_ms":0}`,
		`{"interval_ms":-100}`, `{"interval_ms":null}`, `{"interval_ms":"100"}`,
		`{"interval_ms":100.00000000000000001}`, `{"interval_ms":1e1000000}`,
	} {
		apiAcceptanceRequest(t, handler, http.MethodPost, root+"/test/start", body, http.StatusUnprocessableEntity)
	}
	apiAcceptanceRequest(t, handler, http.MethodPost, root+"/test/start", `{"interval_ms":`, http.StatusBadRequest)
	// afterReject проверяет, что ошибочные команды не открыли тестовую ленту.
	afterReject := chatTestFeedSnapshot(t, handler, root)
	if !reflect.DeepEqual(afterReject["messages"], []any{}) {
		t.Fatalf("Отклонённый интервал создал сообщения: %v", afterReject)
	}
	// variant связывает вход с ожидаемым интервалом, возвращаемым успешным стартом.
	for _, variant := range []struct {
		// body задаёт пропуск поля или точную числовую запись.
		body string
		// interval задаёт фактический интервал в миллисекундах.
		interval string
	}{
		{`{}`, "1000"}, {`{"interval_ms":100.000}`, "100"}, {`{"interval_ms":6e4}`, "60000"},
	} {
		requireChatTestInterval(t, handler, root, variant.body, variant.interval)
		requireChatTestInterval(t, handler, root, `{"interval_ms":250}`, variant.interval)
		apiAcceptanceRequest(t, handler, http.MethodPost, root+"/test/stop", "", http.StatusNoContent)
	}
	apiAcceptanceRequest(t, handler, http.MethodPost, "/chat/streams/unknown/test/start", `{}`, http.StatusNotFound)
	// command адресует одну из операций потока, которого нет в SQLite.
	for _, command := range []string{"/test/stop", "/clear"} {
		apiAcceptanceRequest(t, handler, http.MethodPost, "/chat/streams/unknown"+command, "", http.StatusNotFound)
	}
}

// TestChatTestFeedProviderOpenAPI проверяет, что схема сообщения разрешает обозначить
// любую поддерживаемую площадку в тестовой ленте и пропустить поле в сообщении эфира.
func TestChatTestFeedProviderOpenAPI(t *testing.T) {
	// handler публикует OpenAPI тех же маршрутов, которые обслуживают снимок и SSE.
	_, handler, _ := newChatStreamSelfFixture(t, core.DefaultChatLimits())
	// response содержит фактический ответ OpenAPI, а spec позволяет обратиться к общей модели сообщения.
	response := apiAcceptanceRequest(t, handler, http.MethodGet, "/openapi.json", "", http.StatusOK)
	spec := numericJSON(t, response.Body.Bytes())
	// required содержит обязательные поля сообщения, platforms содержит разрешённые имитации площадок.
	required := numericAt(t, spec, "components.schemas.MessageDTO.required").([]any)
	platforms := numericAt(t, spec, "components.schemas.MessageDTO.properties.test_provider.enum").([]any)
	if slices.Contains(required, any("test_provider")) || len(platforms) != 3 ||
		!slices.Contains(platforms, any("youtube")) || !slices.Contains(platforms, any("twitch")) ||
		!slices.Contains(platforms, any("vk_video_live")) {
		t.Fatalf("Схема должна объявлять необязательную площадку теста: обязательные поля %v, площадки %v", required, platforms)
	}
}

// TestChatTestFeedWaitingClientsAndClearIsolation проверяет один предэфирный SSE для двух
// клиентов с общими площадкой и текстом реплики, очистку ленты и независимость соседнего Stream.
func TestChatTestFeedWaitingClientsAndClearIsolation(t *testing.T) {
	// fixture и handler разделяют SQLite и Manager без внешних адаптеров.
	fixture, handler, _ := newChatStreamSelfFixture(t, core.DefaultChatLimits())
	// stream и other принадлежат разным лентам, err сообщает об отказе создания.
	stream, err := fixture.service.CreateStream(t.Context(), core.Stream{Name: "Первый чат", PublicationKey: "first-feed"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := fixture.service.CreateStream(t.Context(), core.Stream{Name: "Второй чат", PublicationKey: "second-feed"})
	if err != nil {
		t.Fatal(err)
	}
	// root и otherRoot адресуют два независимых набора маршрутов.
	root := "/chat/streams/" + string(stream.ID)
	otherRoot := "/chat/streams/" + string(other.ID)
	// before содержит пустой снимок без Session, eventPath открывает подписку с его точного cursor.
	before := chatTestFeedSnapshot(t, handler, root)
	eventPath := api.Prefix + root + "/events?cursor=" + url.QueryEscape(before["cursor"].(string))
	// first и second получают общие события через два уже открытых HTTP-запроса.
	first, _, _ := chatStreamSelfServe(t, handler, eventPath)
	second, _, _ := chatStreamSelfServe(t, handler, eventPath)
	apiAcceptanceAwait(t, first.flushed)
	apiAcceptanceAwait(t, second.flushed)
	apiAcceptanceRequest(t, handler, http.MethodPost, root+"/clear", "", http.StatusNoContent)
	// emptyClear фиксирует действие очистки до создания тестовой ленты, secondClear проверяет второй клиент.
	emptyClear := chatTestFeedFrame(t, first, "chat.cleared")
	secondClear := chatTestFeedFrame(t, second, "chat.cleared")
	// hasSession проверяет, что управляющий кадр до эфира не присвоен сессии.
	if _, hasSession := emptyClear["session_id"]; hasSession || !reflect.DeepEqual(emptyClear, secondClear) ||
		emptyClear["cursor"] == before["cursor"] || emptyClear["message"] != nil {
		t.Fatalf("Очистка пустой ленты вернула неверную границу: %v / %v", emptyClear, secondClear)
	}

	// Интервал в минуту позволяет получить первый кадр до наступления срока второго.
	requireChatTestInterval(t, handler, root, `{"interval_ms":60000}`, "60000")
	// firstMsg и secondMsg содержат первое наблюдаемое сообщение на тех же подписках.
	firstMsg := chatTestFeedFrame(t, first, "chat.message")
	secondMsg := chatTestFeedFrame(t, second, "chat.message")
	// message содержит только серверную запись без выдуманных идентификаторов площадки или Session.
	message := firstMsg["message"].(map[string]any)
	// hasSession проверяет отсутствие сессии в оболочке события тестовой ленты.
	if _, hasSession := firstMsg["session_id"]; hasSession || !reflect.DeepEqual(firstMsg, secondMsg) ||
		message["seq"] != json.Number("1") || message["provider"] != "test" || message["kind"] != "message" {
		t.Fatalf("Подписки получили разные сообщения или неверные данные: %v / %v", firstMsg, secondMsg)
	}
	switch message["test_provider"] {
	case "youtube", "twitch", "vk_video_live":
	default:
		t.Fatalf("В тестовом кадре нет поддерживаемой площадки для значка: %v", message)
	}
	// line и ok проверяют, что JSON передал строку с текстом тестовой реплики.
	if line, ok := message["text"].(string); !ok || strings.TrimSpace(line) == "" {
		t.Fatalf("В тестовом кадре нет текста реплики: %v", message)
	}
	// key перечисляет идентификаторы, которые по общему формату остаются пустыми у тестового сообщения.
	for _, key := range []string{"session_id", "chat_source_id", "channel_id", "provider_message_id"} {
		if message[key] != "" {
			t.Fatalf("Тестовое сообщение содержит неверное поле %s: %v", key, message)
		}
	}
	apiAcceptanceRequest(t, handler, http.MethodPost, root+"/test/stop", "", http.StatusNoContent)
	// stopped содержит накопленный хвост после остановки.
	stopped := chatTestFeedSnapshot(t, handler, root)
	if stopped["session"] != nil || !reflect.DeepEqual(stopped["messages"], []any{message}) {
		t.Fatalf("Остановка потеряла тестовую историю: %v", stopped)
	}

	// otherEmpty и otherFeed связывают подписку второго потока с его собственной пустой историей.
	otherEmpty := chatTestFeedSnapshot(t, handler, otherRoot)
	otherFeed, _, _ := chatStreamSelfServe(t, handler, api.Prefix+otherRoot+"/events?cursor="+url.QueryEscape(otherEmpty["cursor"].(string)))
	apiAcceptanceAwait(t, otherFeed.flushed)
	requireChatTestInterval(t, handler, otherRoot, `{"interval_ms":60000}`, "60000")
	chatTestFeedFrame(t, otherFeed, "chat.message")
	apiAcceptanceRequest(t, handler, http.MethodPost, otherRoot+"/test/stop", "", http.StatusNoContent)
	// otherBefore содержит историю и курсор другого потока перед очисткой первого.
	otherBefore := chatTestFeedSnapshot(t, handler, otherRoot)
	if len(otherBefore["messages"].([]any)) != 1 {
		t.Fatalf("Вторая лента не получила первое тестовое сообщение: %v", otherBefore)
	}
	apiAcceptanceRequest(t, handler, http.MethodPost, root+"/clear", "", http.StatusNoContent)
	// cleared содержит пустую историю, oldPath пытается продолжить с доочистной позиции.
	cleared := chatTestFeedSnapshot(t, handler, root)
	oldPath := api.Prefix + root + "/events?cursor=" + url.QueryEscape(before["cursor"].(string))
	if !reflect.DeepEqual(cleared["messages"], []any{}) || cleared["cursor"] == stopped["cursor"] ||
		!reflect.DeepEqual(chatTestFeedSnapshot(t, handler, otherRoot), otherBefore) {
		t.Fatalf("Очистка неверно изменила ленты потоков: %v", cleared)
	}
	// replay получает кадры для старой позиции, cancel отменяет запрос, а done подтверждает выход обработчика.
	replay, cancel, done := chatStreamSelfServe(t, handler, oldPath)
	requireChatTestClearedReplay(t, replay, cancel, done)
}

// TestChatTestFeedHandsOffToLiveHTTP проверяет уход тестовой истории при RUNNING, переход
// существующей подписки, отсутствие тестовой площадки у живых сообщений и очистку живой истории.
func TestChatTestFeedHandsOffToLiveHTTP(t *testing.T) {
	// fixture содержит изолированную SQLite, а stream, profile, group и destination задают один локальный выход.
	fixture := newAPIAcceptanceFixture(t, nil)
	stream, profile, group, destination := apiAcceptanceSeedConfiguration(t, fixture)
	// feed подключается через порт действующего адаптера без сети.
	feed := &acceptanceChatFeed{ready: make(chan acceptanceChatReader, 1)}
	// account и err фиксируют создание учётной записи с синтетическими полномочиями.
	account, err := fixture.service.CreateChatAccount(t.Context(), core.ChatAccount{Provider: core.ChatYouTube},
		core.ChatCredentialBundle{YouTube: &core.YouTubeChatCredentials{APIKey: acceptancePrivatePrefix + "live-clear"}}, "live-clear")
	if err != nil {
		t.Fatal(err)
	}
	// source определяет атрибуцию сообщения текущего выхода.
	source, err := fixture.service.CreateChatSource(t.Context(), core.ChatSource{DestinationID: destination.ID, AccountID: account.Account.ID,
		Provider: core.ChatYouTube, ChannelHint: "UCsynthetic", Enabled: true,
		Options: core.ChatOptions{Version: 1, YouTube: &core.YouTubeChatOptions{}}})
	if err != nil {
		t.Fatal(err)
	}
	// handler обслуживает те же маршруты до и после появления подтверждённой Session.
	handler, _ := newChatAcceptanceHandler(t, fixture, core.DefaultChatLimits(), []core.ChatAdapterPort{feed})
	// root и path адресуют общую ленту, before хранит исходную позицию ожидания.
	root := "/chat/streams/" + string(stream.ID)
	path := api.Prefix + root + "/events"
	before := chatTestFeedSnapshot(t, handler, root)
	// waiting остаётся открытым до появления RUNNING, waitingDone подтверждает выход обработчика.
	waiting, _, waitingDone := chatStreamSelfServe(t, handler, path+"?cursor="+url.QueryEscape(before["cursor"].(string)))
	apiAcceptanceAwait(t, waiting.flushed)
	requireChatTestInterval(t, handler, root, `{"interval_ms":60000}`, "60000")
	chatTestFeedFrame(t, waiting, "chat.message")

	// sessionID адресует точную RUNNING Session, пока у которой ещё нет выходов.
	sessionID, _ := seedChatStreamSelfSession(t, fixture, stream)
	apiAcceptanceAwait(t, waitingDone)
	// resync разрешает клиентам перечитать прежний URL снимка.
	resync := chatTestFeedFrame(t, waiting, "resync_required")
	if resync["message"] != nil {
		t.Fatalf("Пересинхронизация содержит сообщение: %v", resync)
	}
	apiAcceptanceRequest(t, handler, http.MethodPost, root+"/test/start", `{}`, http.StatusConflict)
	apiAcceptanceRequest(t, handler, http.MethodPost, root+"/test/stop", "", http.StatusNoContent)
	// active связывает новый пустой снимок с реальной Session без публикаций.
	active := chatTestFeedSnapshot(t, handler, root)
	if !reflect.DeepEqual(active["session"], map[string]any{"session_id": string(sessionID)}) ||
		!reflect.DeepEqual(active["messages"], []any{}) {
		t.Fatalf("RUNNING сохранил тестовую историю или вернул чужую Session: %v", active)
	}
	acceptanceChatSeedOutput(t, fixture, sessionID, stream, profile, group, destination)
	// reader получает право публиковать только после подтверждения действующего выхода.
	var reader acceptanceChatReader
	select {
	case reader = <-feed.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("Адаптер не получил подтверждённый выход")
	}
	// live получает события повторным подключением по прежнему адресу; preClear и postClear различают сообщения по границе.
	live, _, _ := chatStreamSelfServe(t, handler, path)
	const preClear = "Сообщение до очистки"
	const postClear = "Сообщение после очистки"
	apiAcceptanceAwait(t, live.flushed)
	reader.send(t, t.Context(), "live-before", preClear)
	apiAcceptanceAwaitFrame(t, live, preClear)
	// first является последним доочистным сообщением с первого реального выхода.
	first := chatTestFeedFrame(t, live, "chat.message")
	assertAcceptanceChatMessage(t, first, sessionID, source.ID, "live-before", preClear, "1")
	// hasTestProvider отличает полное отсутствие поля в SSE от значения null или пустой строки.
	if _, hasTestProvider := first["message"].(map[string]any)["test_provider"]; hasTestProvider {
		t.Fatalf("В живом кадре появилась площадка тестовой реплики: %v", first)
	}
	apiAcceptanceRequest(t, handler, http.MethodPost, root+"/clear", "", http.StatusNoContent)
	// marker подтверждает очистку в той же Session до приёма новых сообщений.
	marker := chatTestFeedFrame(t, live, "chat.cleared")
	if marker["session_id"] != string(sessionID) || marker["message"] != nil {
		t.Fatalf("Очистка эфира потеряла идентичность Session: %v", marker)
	}
	// empty содержит пустую историю, сохраняя конфигурацию и оперативный статус источника.
	empty := chatTestFeedSnapshot(t, handler, root)
	if !reflect.DeepEqual(empty["messages"], []any{}) || !reflect.DeepEqual(empty["session"], active["session"]) ||
		len(empty["sources"].([]any)) != 1 || empty["statuses"].([]any)[0].(map[string]any)["state"] != "CONNECTED" {
		t.Fatalf("Очистка эфира изменила состояние источника или сохранила историю: %v", empty)
	}
	reader.send(t, t.Context(), "live-before", "Повтор идентификатора до очистки")
	reader.send(t, t.Context(), "live-after", postClear)
	apiAcceptanceAwaitFrame(t, live, postClear)
	// after содержит только первое новое сообщение и продолжает прежнюю последовательность.
	after := chatTestFeedSnapshot(t, handler, root)
	// messages содержит согласованное текущее представление истории после очистки.
	messages := after["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["text"] != postClear ||
		messages[0].(map[string]any)["seq"] != json.Number("2") {
		t.Fatalf("Очистка сбросила дедупликацию или порядковый номер: %v", after)
	}
	// hasTestProvider отличает отсутствие поля в живом снимке от переданного пустого значения.
	if _, hasTestProvider := messages[0].(map[string]any)["test_provider"]; hasTestProvider {
		t.Fatalf("В снимке живого сообщения появилась площадка тестовой реплики: %v", after)
	}
	apiAcceptanceRequest(t, handler, http.MethodPost, root+"/test/stop", "", http.StatusNoContent)
	if !reflect.DeepEqual(chatTestFeedSnapshot(t, handler, root), after) {
		t.Fatal("Остановка генератора изменила ленту действующей Session")
	}
}

// requireChatTestInterval проводит реальный запрос старта через обработчик и проверяет фактически действующий интервал.
func requireChatTestInterval(t *testing.T, handler *api.API, root, body, interval string) {
	t.Helper()
	// response содержит проверенные статус и JSON успешного запуска.
	response := apiAcceptanceJSON(t, apiAcceptanceRequest(t, handler, http.MethodPost, root+"/test/start", body, http.StatusOK))
	if response["interval_ms"] != json.Number(interval) {
		t.Fatalf("Интервал запроса %s вернул %v, ожидается %s", body, response, interval)
	}
}

// chatTestFeedSnapshot читает снимок потока по пути root через handler. Помощник проверяет
// статус, срок записи и обязательные поля снимка, а числа возвращает в точной записи JSON.
func chatTestFeedSnapshot(t *testing.T, handler *api.API, root string) map[string]any {
	t.Helper()
	// writer поддерживает SetWriteDeadline и Flush, которые нужны обработчику снимка.
	writer := newAPIAcceptanceStreamWriter()
	handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, api.Prefix+root+"/snapshot", nil).WithContext(t.Context()))
	// body содержит фактический ответ, status хранит HTTP-статус, unsafe отмечает запись или Flush без срока.
	body, status, unsafe := writer.snapshot()
	if status != http.StatusOK || unsafe || !writer.deadline.IsZero() {
		t.Fatalf("Некорректная запись снимка %s: статус %d, запись без срока %t, срок после ответа %v, тело %q",
			root, status, unsafe, writer.deadline, body)
	}
	// snapshot хранит поля ответа в исходном представлении JSON, включая точные числа.
	var snapshot map[string]any
	// err сообщает об ошибке разбора тела снимка.
	if err := apiAcceptanceDecodeJSON([]byte(body), &snapshot); err != nil {
		t.Fatal(err)
	}
	// present показывает, что ответ явно передал session, в том числе со значением null.
	if _, present := snapshot["session"]; !present || snapshot["cursor"] == "" {
		t.Fatalf("Снимок без обязательного session или cursor: %v", snapshot)
	}
	return snapshot
}

// chatTestFeedFrame возвращает первый кадр указанного kind у writer и сверяет SSE id с JSON cursor.
func chatTestFeedFrame(t *testing.T, writer *apiAcceptanceStreamWriter, kind string) map[string]any {
	t.Helper()
	// eventLine адресует служебную строку искомого события.
	eventLine := "\nevent: " + kind + "\n"
	apiAcceptanceAwaitFrame(t, writer, eventLine)
	// body содержит полные записи выдачи, status и unsafe отражают условия записи транспорта.
	body, status, unsafe := writer.snapshot()
	if status != http.StatusOK || unsafe {
		t.Fatalf("Некорректная SSE-запись: %d %v %q", status, unsafe, body)
	}
	// frame отделяет один кадр от heartbeat и других событий.
	for _, frame := range strings.Split(body, "\n\n") {
		if !strings.Contains(frame, eventLine) {
			continue
		}
		// lines содержит ожидаемые строки id, event и data.
		lines := strings.Split(frame, "\n")
		if len(lines) != 3 || !strings.HasPrefix(lines[0], "id: ") || !strings.HasPrefix(lines[2], "data: ") {
			t.Fatalf("Повреждён кадр %s: %q", kind, frame)
		}
		// value содержит JSON-оболочку, err сообщает о неполной или невалидной записи.
		var value map[string]any
		if err := apiAcceptanceDecodeJSON([]byte(strings.TrimPrefix(lines[2], "data: ")), &value); err != nil {
			t.Fatal(err)
		}
		if value["kind"] != kind || value["cursor"] != strings.TrimPrefix(lines[0], "id: ") || value["cursor"] == "" {
			t.Fatalf("SSE id расходится с JSON события: %q", frame)
		}
		return value
	}
	t.Fatalf("Не удалось извлечь кадр %s из %q", kind, body)
	return nil
}

// requireChatTestClearedReplay принимает кадр chat.cleared или resync_required и проверяет отсутствие кадров старой истории.
// cancel прекращает подписку после кадра, done подтверждает, что транспорт больше не пишет.
func requireChatTestClearedReplay(t *testing.T, writer *apiAcceptanceStreamWriter, cancel func(), done <-chan struct{}) {
	t.Helper()
	// timer ограничивает ожидание управляющего кадра даже при сбое повторной доставки.
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		// body содержит согласованные байты до первого управляющего кадра.
		body, _, _ := writer.snapshot()
		if strings.Contains(body, "\nevent: chat.cleared\n") || strings.Contains(body, "\nevent: resync_required\n") {
			break
		}
		select {
		case <-writer.changed:
		case <-timer.C:
			t.Fatalf("Нет границы очистки при старом курсоре: %q", body)
		}
	}
	cancel()
	apiAcceptanceAwait(t, done)
	// final содержит все байты, записанные до полного выхода обработчика.
	final, _, _ := writer.snapshot()
	if strings.Contains(final, "\nevent: chat.message\n") {
		t.Fatalf("Повторная доставка вернула историю до очистки: %q", final)
	}
}
