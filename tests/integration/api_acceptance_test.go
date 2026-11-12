package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Создаёт настоящий HTTP API поверх сервиса, менеджера сессий и подготовки фикстуры; его ограниченное
// завершение зарегистрировано в t.Cleanup.
func integrationAPI(t *testing.T, f *sessionAcceptanceFixture) *api.API {
	t.Helper()
	// preparation предоставляет подготовку заглушек через интерфейс API, а ok подтверждает поддержку
	// этого интерфейса фикстурой.
	preparation, ok := f.config.Preparation.(api.Preparation)
	if !ok {
		t.Fatal("фикстура не предоставляет рабочий API подготовки")
	}
	// a объединяет реальные HTTP-маршруты фикстуры, а err сообщает об ошибке создания API.
	a, err := api.New(api.Config{Core: f.service, Commands: f.manager, Fallback: preparation, Capabilities: api.ExecutableCapabilities{FFmpeg: filepath.Join(t.TempDir(), "missing-ffmpeg")}, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Limits: api.Limits{JSONBytes: 16 << 10, UploadBytes: 1 << 20, UploadReadTimeout: time.Second, OperationTimeout: 2 * time.Second, ShutdownTimeout: 2 * time.Second, SSEClients: 2, SSEPageSize: 32, SSEFrameBytes: 128 << 10, SSEWriteTimeout: time.Second, SSEHeartbeat: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Останавливает интеграционный API в отдельном ограниченном контексте.

		// ctx ограничивает завершение API тремя секундами, а cancel освобождает таймер этого
		// ожидания.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// err сообщает, удалось ли завершить API в контексте очистки теста.
		if err := a.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return a
}

// Выполняет JSON-запрос к API и проверяет статус и границы раскрытия служебных данных и ключей. Для
// принятой команды дополнительно проверяет Location и независимое чтение той же квитанции через GET.
func integrationAPIRequest(t *testing.T, a http.Handler, method, path, body string, status int) map[string]any {
	t.Helper()
	// r содержит тестовый HTTP-запрос с заданным методом, путём и JSON-телом.
	r := httptest.NewRequest(method, api.Prefix+path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	// w сохраняет HTTP-статус, заголовки и тело ответа для контрактных проверок.
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, w.Code, status, w.Body)
	}
	// secret обозначает очередное служебное поле, запрещённое во всех публичных ответах.
	for _, secret := range []string{`"incarnation"`, `"attempt_token"`, `"managed_path"`, `"fingerprint"`, `"identity"`} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("публичный ответ раскрыл %q", secret)
		}
	}
	// noKeys проверяет переданный текст ответа на отсутствие синтетических ключей и полей адресов
	// вне разрешённых представлений конфигурации.
	noKeys := func(data string) {
		// Проверяет отсутствие ключей и адресов в ответах вне конфигурации и сохранённых снимков.

		t.Helper()
		// secret обозначает очередной ключ или имя поля, которое проверяемый ответ не должен
		// раскрывать.
		for _, secret := range []string{"synthetic-session-key", "synthetic-key-0", `"endpoint"`, `"stream_key"`, `"publication_key"`} {
			if strings.Contains(data, secret) {
				t.Fatalf("ответ вне конфигурации раскрыл %q", secret)
			}
		}
	}
	// parts содержит сегменты маршрута без префикса API для определения допустимого раскрытия
	// конфигурации.
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, api.Prefix+"/"), "/")
	// configurationResponse показывает, разрешено ли этому успешному ответу возвращать
	// конфигурационные поля и ключи.
	configurationResponse := false
	switch parts[0] {
	case "streams", "destinations":
		switch {
		case len(parts) == 1:
			configurationResponse = method == "GET" && status == 200 || method == "POST" && status == 201
		case len(parts) == 2:
			configurationResponse = (method == "GET" || method == "PATCH") && status == 200
		case len(parts) == 3 && parts[0] == "streams" && (parts[2] == "destinations" || parts[2] == "sessions"):
			configurationResponse = method == "GET" && status == 200
		}
	case "sessions":
		// historyResponse показывает, относится ли маршрут чтения к истории сессий или
		// сохранённым выходам.
		historyResponse := len(parts) == 1 || len(parts) == 2 || len(parts) == 3 && parts[2] == "outputs" || len(parts) == 4 && parts[2] == "outputs"
		configurationResponse = method == "GET" && status == 200 && historyResponse
	}
	if !configurationResponse {
		noKeys(w.Body.String())
	}
	// v содержит разобранный JSON-объект проверяемого HTTP-ответа.
	var v map[string]any
	// err сообщает о невозможности разобрать тело HTTP-ответа как JSON-объект.
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	// runtime содержит оперативную часть ответа, где конфигурационные ключи запрещены независимо от
	// внешнего маршрута.
	if runtime := v["runtime"]; runtime != nil {
		// data содержит сериализованную оперативную часть для проверки раскрытия ключей, а err
		// сообщает об ошибке её сериализации.
		data, err := json.Marshal(runtime)
		if err != nil {
			t.Fatal(err)
		}
		noKeys(string(data))
	}
	if status == 202 {
		if w.Header().Get("Location") != api.Prefix+"/commands/"+v["id"].(string) {
			t.Fatal("Location не указывает точную квитанцию", w.Header())
		}
		// stored содержит ту же квитанцию, прочитанную отдельным GET-запросом после принятия
		// команды.
		stored := integrationAPIRequest(t, a, "GET", "/commands/"+v["id"].(string), "", 200)
		if stored["id"] != v["id"] || stored["request_id"] != v["request_id"] {
			t.Fatal("квитанция не читается независимо от SSE")
		}
	}
	return v
}

// Проверяет HTTP-управление настоящими сессиями и выходами: повтор RequestID, неизменяемые снимки,
// оперативные метрики и принадлежность выхода сессии. Изменение дополнительного выхода сохраняет
// исходное медиасоединение, а пауза запрещает подготовку заглушки до чтения тела загрузки.
func TestAcceptanceAPIRealManagerReceiptsSnapshotsMetricsAndMembership(t *testing.T) {
	// receiver принимает медиапубликацию исходного выхода для проверки сохранения его соединения.
	receiver, _ := newPublisherReceiver(t, false)
	// extraReceiver принимает публикацию дополнительного назначения при управлении выходами.
	extraReceiver, _ := newPublisherReceiver(t, false)
	// f предоставляет настоящий менеджер сессий, хранилище и медиатракт проверяемого потока.
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	f.launch(t)
	// a обслуживает HTTP-команды и чтение состояния поверх рабочей фикстуры.
	a := integrationAPI(t, f)
	// unreadyAsset содержит заглушку без подготовленного поколения, а err сообщает об ошибке её
	// создания.
	unreadyAsset, err := f.service.CreateAsset(context.Background(), "Без подготовленного поколения")
	if err != nil {
		t.Fatal(err)
	}
	// unreadyStream содержит поток с неподготовленной заглушкой для проверки отказа запуска через
	// HTTP.
	unreadyStream, err := f.service.CreateStream(context.Background(), core.Stream{Name: "Неготовый поток", PublicationKey: "unready-key", Fallback: core.FallbackPolicy{AssetID: unreadyAsset.ID, Watchdog: f.stream.Fallback.Watchdog}})
	if err != nil {
		t.Fatal(err)
	}
	integrationAPIRequest(t, a, "POST", "/streams/"+string(unreadyStream.ID)+"/start", `{"request_id":"unready-start","destination_ids":[]}`, 409)
	// startBody содержит запрос запуска с постоянным RequestID для проверки конкурентного повтора.
	startBody := fmt.Sprintf(`{"request_id":"http-start","destination_ids":[%q]}`, f.destinations[0].ID)
	// responses передаёт два HTTP-ответа из параллельных обработчиков в проверяющую горутину.
	responses := make(chan *httptest.ResponseRecorder, 2)
	// wg позволяет дождаться обоих запросов запуска перед закрытием канала ответов.
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			// Параллельно отправляет одинаковый RequestID для проверки единственной команды запуска.

			// r содержит один из двух одинаковых конкурентных запросов запуска потока.
			r := httptest.NewRequest("POST", api.Prefix+"/streams/"+string(f.stream.ID)+"/start", strings.NewReader(startBody))
			r.Header.Set("Content-Type", "application/json")
			// w собирает ответ конкретного конкурентного запроса для передачи в responses.
			w := httptest.NewRecorder()
			a.ServeHTTP(w, r)
			responses <- w
		})
	}
	wg.Wait()
	close(responses)
	// accepted сохраняет первую квитанцию запуска для сравнения с ответом повторного запроса.
	var accepted map[string]any
	// w обозначает очередной ответ одного из двух конкурентных запросов запуска.
	for w := range responses {
		if w.Code != 202 {
			t.Fatalf("повтор concurrent RequestID не принят: %d %s", w.Code, w.Body)
		}
		// v содержит JSON очередного ответа для сравнения идентичности квитанций.
		var v map[string]any
		// err сообщает об ошибке разбора ответа конкурентного запроса запуска.
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if accepted != nil && accepted["id"] != v["id"] {
			t.Fatal("RequestID создал две команды")
		}
		accepted = v
	}
	// receipt содержит успешно завершённую квитанцию единственного запуска сессии; функция обратного
	// вызова добавляет диагностику менеджера при неудачном ожидании.
	receipt := sessionAcceptanceReceipt(t, f.service, core.ID(accepted["id"].(string)), core.CommandSucceeded, func() {
		// Добавляет диагностический снимок SessionManager при неудачном ожидании квитанции.
		f.captureFailure(t)
	})
	// current содержит сессию после завершения запуска и перехода в рабочее состояние.
	current := f.running(t, receipt)
	// originalMessages содержит пакеты исходной публикации; функция обратного вызова ожидает не
	// менее восьми медиасообщений.
	originalMessages := receiver.wait(t, func(m []publisherWireMessage) bool {
		// Ожидает исходную передачу медиапакетов единственного выхода.
		return publisherMediaCount(m) >= 8
	})
	if receiver.accepted.Load() != 1 {
		t.Fatal("исходный выход открыл больше одного соединения")
	}
	// originalConnection фиксирует идентификатор исходного соединения для проверки его сохранения.
	originalConnection := originalMessages[0].connection
	// output содержит сохранённый исходный выход запущенной сессии.
	output := f.outputs(t, current.ID)[0]
	// apiSession содержит публичную историю сессии для проверки долговечного состояния без
	// оперативных метрик.
	apiSession := integrationAPIRequest(t, a, "GET", "/sessions/"+string(current.ID), "", 200)
	if apiSession["state"] != "RUNNING" {
		t.Fatal("HTTP не показывает долговечный lifecycle", apiSession)
	}
	if apiSession["metrics"] != nil {
		t.Fatal("оперативные метрики попали в историю Session")
	}
	// metrics хранит последний HTTP-ответ метрик при ожидании ненулевой передачи данных.
	var metrics map[string]any
	sessionAcceptanceWait(t, func() bool {
		// Ожидает ненулевые фактические счётчики передачи через HTTP-метрики Session.

		metrics = integrationAPIRequest(t, a, "GET", "/sessions/"+string(current.ID)+"/metrics", "", 200)
		if metrics["availability"] != "available" {
			return false
		}
		// snapshot содержит доступный оперативный снимок сессии из ответа метрик.
		snapshot := metrics["snapshot"].(map[string]any)
		// outs содержит выходы оперативного снимка, где ожидается единственный исходный выход.
		outs := snapshot["outputs"].([]any)
		if len(outs) != 1 {
			return false
		}
		// m содержит метрики исходного выхода либо nil до первого измерения.
		m := outs[0].(map[string]any)["metrics"]
		if m == nil {
			return false
		}
		// media содержит медиасчётчики выхода для проверки ненулевого числа переданных байтов
		// полезной нагрузки.
		media := m.(map[string]any)["media"].(map[string]any)
		return media["payload_bytes"] != "0"
	})
	// before фиксирует публичный сохранённый снимок назначения до изменения его текущей
	// конфигурации.
	before := integrationAPIRequest(t, a, "GET", "/sessions/"+string(current.ID)+"/outputs/"+string(output.ID), "", 200)
	// destination содержит копию текущего назначения, имя которого меняется для проверки
	// неизменяемости истории.
	destination := f.destinations[0]
	destination.Name = "Новое имя следующего запуска"
	// err сообщает об ошибке сохранения нового имени назначения.
	if _, err := f.service.UpdateDestination(context.Background(), destination, destination.Revision); err != nil {
		t.Fatal(err)
	}
	// after содержит историю выхода после переименования текущего назначения для сравнения
	// сохранённого имени.
	after := integrationAPIRequest(t, a, "GET", "/sessions/"+string(current.ID)+"/outputs/"+string(output.ID), "", 200)
	if after["destination"].(map[string]any)["name"] != before["destination"].(map[string]any)["name"] {
		t.Fatal("исторический снимок заменён текущей конфигурацией")
	}
	// Новый выход разделяет готовую группу; HTTP передаёт только пользовательское намерение.
	// createBody содержит конфигурацию дополнительного назначения, разделяющего уже готовую группу.
	createBody := fmt.Sprintf(`{"stream_id":%q,"group_id":%q,"name":"Дополнительное назначение","platform":"custom","endpoint":%q,"stream_key":"synthetic-key-extra","enabled":false}`, f.stream.ID, f.groups[0].ID, "rtmp://"+extraReceiver.listener.Addr().String()+"/live")
	// extra содержит HTTP-представление созданного дополнительного назначения.
	extra := integrationAPIRequest(t, a, "POST", "/destinations", createBody, 201)
	// addBody содержит команду добавления выхода для нового назначения в текущую сессию.
	addBody := fmt.Sprintf(`{"request_id":"http-add","destination_id":%q}`, extra["id"])
	// starts фиксирует число запусков группы до управления дополнительным выходом.
	starts := f.starts.Load()
	// added содержит квитанцию добавления выхода и идентификатор созданного выхода.
	added := integrationAPIRequest(t, a, "POST", "/sessions/"+string(current.ID)+"/outputs", addBody, 202)
	sessionAcceptanceReceipt(t, f.service, core.ID(added["id"].(string)), core.CommandSucceeded)
	// extraID содержит идентификатор дополнительного выхода из квитанции добавления.
	extraID := added["output_id"].(string)
	// extraPath задаёт HTTP-префикс команд и чтения дополнительного выхода.
	extraPath := "/sessions/" + string(current.ID) + "/outputs/" + extraID
	// suffix задаёт очередную команду остановки, запуска или отсоединения дополнительного выхода.
	for _, suffix := range []string{"stop", "start", "detach"} {
		// command содержит квитанцию очередной команды дополнительного выхода.
		command := integrationAPIRequest(t, a, "POST", extraPath+"/"+suffix, `{"request_id":"http-extra-`+suffix+`"}`, 202)
		sessionAcceptanceReceipt(t, f.service, core.ID(command["id"].(string)), core.CommandSucceeded)
		// stored содержит состояние дополнительного выхода после команды для проверки намерения
		// и прикрепления.
		stored := integrationAPIRequest(t, a, "GET", extraPath, "", 200)
		if stored["desired_running"] != (suffix == "start") || stored["attached"] != (suffix != "detach") {
			t.Fatal("команда выхода потеряла намерение или прикрепление", suffix, stored)
		}
	}
	if f.starts.Load() != starts {
		t.Fatal("управление дополнительным выходом пересоздало здоровую группу")
	}
	// beforePackets фиксирует число медиасообщений исходного выхода перед ожиданием продолжения.
	beforePackets := publisherMediaCount(receiver.snapshot())
	// continued содержит пакеты после управления дополнительным выходом; функция обратного вызова
	// ожидает ещё восемь медиасообщений.
	continued := receiver.wait(t, func(m []publisherWireMessage) bool {
		// Проверяет продолжение исходного выхода после управления дополнительным назначением.
		return publisherMediaCount(m) >= beforePackets+8
	})
	if receiver.accepted.Load() != 1 {
		t.Fatal("управление дополнительным выходом пересоздало исходное соединение")
	}
	// message обозначает очередное сообщение продолжившейся публикации для проверки исходного
	// соединения.
	for _, message := range continued {
		if message.connection != originalConnection {
			t.Fatal("продолжение медиа получено от другого соединения")
		}
	}
	// err сообщает, доступен ли прежний медиавход менеджера после управления дополнительным выходом.
	if _, err := f.manager.Feed(current); err != nil {
		t.Fatal("управление дополнительным выходом потеряло здоровый runtime", err)
	}
	// bindingsBody содержит запрещённое поле bindings в повторном запросе запуска для проверки
	// валидации HTTP-тела.
	bindingsBody := strings.TrimSuffix(startBody, "}") + `,"bindings":[]}`
	integrationAPIRequest(t, a, "POST", "/streams/"+string(f.stream.ID)+"/start", bindingsBody, 422)
	// suffix задаёт очередную команду паузы или продолжения сессии.
	for _, suffix := range []string{"pause", "resume"} {
		// v содержит квитанцию команды паузы или продолжения сессии.
		v := integrationAPIRequest(t, a, "POST", "/sessions/"+string(current.ID)+"/"+suffix, `{"request_id":"http-`+suffix+`"}`, 202)
		sessionAcceptanceReceipt(t, f.service, core.ID(v["id"].(string)), core.CommandSucceeded)
		if suffix == "pause" {
			// asset содержит актуальную заглушку при ручной паузе, а err сообщает об ошибке
			// её чтения.
			asset, err := f.service.GetAsset(context.Background(), f.asset.ID)
			if err != nil {
				t.Fatal(err)
			}
			// revision содержит строковую ревизию заглушки для запросов изменения с
			// контролем версии.
			revision := fmt.Sprint(asset.Revision)
			integrationAPIRequest(t, a, "POST", "/fallback-assets/"+string(asset.ID)+"/rebuild", `{"expected_revision":"`+revision+`"}`, 409)
			integrationAPIRequest(t, a, "DELETE", "/fallback-assets/"+string(asset.ID)+"?expected_revision="+revision, "", 409)
			// payload содержит тело загрузки, которое должно остаться непрочитанным при
			// запрете подготовки.
			payload := strings.NewReader("unread active fallback")
			// upload содержит запрос замены заглушки во время паузы для проверки раннего
			// отказа.
			upload := httptest.NewRequest("PUT", api.Prefix+"/fallback-assets/"+string(asset.ID)+"/content?kind=image&expected_revision="+revision, payload)
			upload.Header.Set("Content-Type", "application/octet-stream")
			// response сохраняет ответ загрузки для проверки конфликта до чтения её тела.
			response := httptest.NewRecorder()
			a.ServeHTTP(response, upload)
			if response.Code != 409 || payload.Len() != len("unread active fallback") {
				t.Fatal("пауза разрешила подготовку или чтение до admission", response.Code, payload.Len())
			}
		}
	}
	integrationAPIRequest(t, a, "GET", "/health/ready", "", 200)
	integrationAPIRequest(t, a, "GET", "/sessions/"+string(current.ID)+"/outputs?state=PUBLISHING", "", 422)
	// stop содержит квитанцию остановки первой сессии через HTTP.
	stop := integrationAPIRequest(t, a, "POST", "/sessions/"+string(current.ID)+"/stop", `{"request_id":"http-stop"}`, 202)
	sessionAcceptanceReceipt(t, f.service, core.ID(stop["id"].(string)), core.CommandSucceeded)
	sessionAcceptanceWait(t, func() bool {
		// Ожидает терминальное состояние Session с подтверждённой очисткой.

		// s содержит текущее долговечное состояние первой сессии, а err сообщает об ошибке
		// чтения при ожидании очистки.
		s, err := f.service.GetSession(context.Background(), current.ID)
		return err == nil && s.State.Terminal() && !s.CleanupPending
	})
	// terminal содержит ответ метрик остановленной сессии для проверки отсутствия устаревшего
	// снимка.
	terminal := integrationAPIRequest(t, a, "GET", "/sessions/"+string(current.ID)+"/metrics", "", 200)
	if terminal["availability"] != "unavailable" || terminal["snapshot"] != nil || terminal["reason"] == "" {
		t.Fatal("терминальная сессия получила старые метрики или нули", terminal)
	}
	integrationAPIRequest(t, a, "GET", "/sessions/unknown/metrics", "", 404)
	// original задаёт путь подготовленного варианта, временно скрываемого для проверки повтора
	// завершённой команды.
	original := filepath.Join(f.directory, f.generation.RequiredProfiles[0].ManagedPath)
	// err сообщает, удалось ли временно переименовать подготовленный вариант.
	if err := os.Rename(original, original+".unavailable"); err != nil {
		t.Fatal(err)
	}
	// renamed показывает, требуется ли отложенно восстановить временно скрытый вариант.
	renamed := true
	defer func() {
		// Восстанавливает временно переименованный файл варианта, если тест завершился до явного возврата.

		if renamed {
			// err сообщает об ошибке восстановления варианта при досрочном завершении теста.
			if err := os.Rename(original+".unavailable", original); err != nil {
				t.Error(err)
			}
		}
	}()
	// replay содержит ответ повтора исходного RequestID после завершения сессии и скрытия варианта.
	replay := integrationAPIRequest(t, a, "POST", "/streams/"+string(f.stream.ID)+"/start", startBody, 202)
	if replay["id"] != accepted["id"] {
		t.Fatal("терминальный replay повторно связывает ресурсы")
	}
	integrationAPIRequest(t, a, "POST", "/sessions/"+string(current.ID)+"/stop", `{"request_id":"http-start"}`, 409)
	// err сообщает об ошибке явного восстановления варианта перед новым запуском.
	if err := os.Rename(original+".unavailable", original); err != nil {
		t.Fatal(err)
	}
	renamed = false
	// Следующий запуск даёт существующего чужого родителя для проверки принадлежности.
	// nextBody содержит запрос следующего запуска с новым RequestID.
	nextBody := strings.Replace(startBody, "http-start", "http-next", 1)
	// next содержит квитанцию второго запуска для проверки принадлежности выхода сессии.
	next := integrationAPIRequest(t, a, "POST", "/streams/"+string(f.stream.ID)+"/start", nextBody, 202)
	// second содержит вторую работающую сессию, которой исходный выход не принадлежит.
	second := f.running(t, sessionAcceptanceReceipt(t, f.service, core.ID(next["id"].(string)), core.CommandSucceeded))
	// method задаёт чтение либо команду остановки чужого для второй сессии выхода.
	for _, method := range []string{"GET", "POST"} {
		// path задаёт маршрут чужого выхода под идентификатором второй сессии.
		path := "/sessions/" + string(second.ID) + "/outputs/" + string(output.ID)
		// body содержит JSON команды чужого выхода и остаётся пустым для запроса чтения.
		body := ""
		if method == "POST" {
			path += "/stop"
			body = `{"request_id":"foreign-output"}`
		}
		integrationAPIRequest(t, a, method, path, body, 404)
	}
	// err показывает отсутствие квитанции после отклонённой команды чужого выхода.
	if _, err := f.service.FindReceipt(context.Background(), "foreign-output"); err == nil {
		t.Fatal("чужой output создал команду")
	}
	// final содержит квитанцию остановки второй сессии после проверки принадлежности.
	final := integrationAPIRequest(t, a, "POST", "/sessions/"+string(second.ID)+"/stop", `{"request_id":"http-next-stop"}`, 202)
	sessionAcceptanceReceipt(t, f.service, core.ID(final["id"].(string)), core.CommandSucceeded)
}
