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

func integrationAPI(t *testing.T, f *sessionAcceptanceFixture) *api.API {
	t.Helper()
	preparation, ok := f.config.Preparation.(api.Preparation)
	if !ok {
		t.Fatal("фикстура не предоставляет рабочий API подготовки")
	}
	a, err := api.New(api.Config{Core: f.service, Commands: f.manager, Fallback: preparation, Capabilities: api.ExecutableCapabilities{FFmpeg: filepath.Join(t.TempDir(), "missing-ffmpeg")}, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Limits: api.Limits{JSONBytes: 16 << 10, UploadBytes: 1 << 20, UploadReadTimeout: time.Second, OperationTimeout: 2 * time.Second, ShutdownTimeout: 2 * time.Second, SSEClients: 2, SSEPageSize: 32, SSEFrameBytes: 128 << 10, SSEWriteTimeout: time.Second, SSEHeartbeat: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := a.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return a
}

func integrationAPIRequest(t *testing.T, a http.Handler, method, path, body string, status int) map[string]any {
	t.Helper()
	r := httptest.NewRequest(method, api.Prefix+path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, w.Code, status, w.Body)
	}
	for _, secret := range []string{"synthetic-session-key", "synthetic-key-0", `"incarnation"`, `"attempt_token"`, `"managed_path"`, `"fingerprint"`, `"identity"`, `"endpoint"`, `"stream_key"`, `"publication_key"`} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("публичный ответ раскрыл %q", secret)
		}
	}
	var v map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if status == 202 {
		if w.Header().Get("Location") != api.Prefix+"/commands/"+v["id"].(string) {
			t.Fatal("Location не указывает точную квитанцию", w.Header())
		}
		stored := integrationAPIRequest(t, a, "GET", "/commands/"+v["id"].(string), "", 200)
		if stored["id"] != v["id"] || stored["request_id"] != v["request_id"] {
			t.Fatal("квитанция не читается независимо от SSE")
		}
	}
	return v
}

func TestAcceptanceAPIRealManagerReceiptsSnapshotsMetricsAndMembership(t *testing.T) {
	receiver, _ := newPublisherReceiver(t, false)
	extraReceiver, _ := newPublisherReceiver(t, false)
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	f.launch(t)
	a := integrationAPI(t, f)
	unreadyAsset, err := f.service.CreateAsset(context.Background(), "Без подготовленного поколения")
	if err != nil {
		t.Fatal(err)
	}
	unreadyStream, err := f.service.CreateStream(context.Background(), core.Stream{Name: "Неготовый поток", PublicationKey: "unready-key", Fallback: core.FallbackPolicy{AssetID: unreadyAsset.ID, Watchdog: f.stream.Fallback.Watchdog}})
	if err != nil {
		t.Fatal(err)
	}
	integrationAPIRequest(t, a, "POST", "/streams/"+string(unreadyStream.ID)+"/start", `{"request_id":"unready-start","destination_ids":[]}`, 409)
	startBody := fmt.Sprintf(`{"request_id":"http-start","destination_ids":[%q]}`, f.destinations[0].ID)
	responses := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Go(func() {
			r := httptest.NewRequest("POST", api.Prefix+"/streams/"+string(f.stream.ID)+"/start", strings.NewReader(startBody))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			a.ServeHTTP(w, r)
			responses <- w
		})
	}
	wg.Wait()
	close(responses)
	var accepted map[string]any
	for w := range responses {
		if w.Code != 202 {
			t.Fatalf("повтор concurrent RequestID не принят: %d %s", w.Code, w.Body)
		}
		var v map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if accepted != nil && accepted["id"] != v["id"] {
			t.Fatal("RequestID создал две команды")
		}
		accepted = v
	}
	receipt := sessionAcceptanceReceipt(t, f.service, core.ID(accepted["id"].(string)), core.CommandSucceeded, func() { f.captureFailure(t) })
	current := f.running(t, receipt)
	originalMessages := receiver.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= 8 })
	if receiver.accepted.Load() != 1 {
		t.Fatal("исходный выход открыл больше одного соединения")
	}
	originalConnection := originalMessages[0].connection
	output := f.outputs(t, current.ID)[0]
	apiSession := integrationAPIRequest(t, a, "GET", "/sessions/"+string(current.ID), "", 200)
	if apiSession["state"] != "RUNNING" {
		t.Fatal("HTTP не показывает долговечный lifecycle", apiSession)
	}
	if apiSession["metrics"] != nil {
		t.Fatal("оперативные метрики попали в историю Session")
	}
	var metrics map[string]any
	sessionAcceptanceWait(t, func() bool {
		metrics = integrationAPIRequest(t, a, "GET", "/sessions/"+string(current.ID)+"/metrics", "", 200)
		if metrics["availability"] != "available" {
			return false
		}
		snapshot := metrics["snapshot"].(map[string]any)
		outs := snapshot["outputs"].([]any)
		if len(outs) != 1 {
			return false
		}
		m := outs[0].(map[string]any)["metrics"]
		if m == nil {
			return false
		}
		media := m.(map[string]any)["media"].(map[string]any)
		return media["payload_bytes"] != "0"
	})
	before := integrationAPIRequest(t, a, "GET", "/sessions/"+string(current.ID)+"/outputs/"+string(output.ID), "", 200)
	destination := f.destinations[0]
	destination.Name = "Новое имя следующего запуска"
	if _, err := f.service.UpdateDestination(context.Background(), destination, destination.Revision); err != nil {
		t.Fatal(err)
	}
	after := integrationAPIRequest(t, a, "GET", "/sessions/"+string(current.ID)+"/outputs/"+string(output.ID), "", 200)
	if after["destination"].(map[string]any)["name"] != before["destination"].(map[string]any)["name"] {
		t.Fatal("исторический снимок заменён текущей конфигурацией")
	}
	// Новый выход разделяет готовую группу; HTTP передаёт только пользовательское намерение.
	createBody := fmt.Sprintf(`{"stream_id":%q,"group_id":%q,"name":"Дополнительное назначение","platform":"custom","endpoint":%q,"stream_key":"synthetic-key-extra","enabled":false}`, f.stream.ID, f.groups[0].ID, "rtmp://"+extraReceiver.listener.Addr().String()+"/live")
	extra := integrationAPIRequest(t, a, "POST", "/destinations", createBody, 201)
	addBody := fmt.Sprintf(`{"request_id":"http-add","destination_id":%q}`, extra["id"])
	starts := f.starts.Load()
	added := integrationAPIRequest(t, a, "POST", "/sessions/"+string(current.ID)+"/outputs", addBody, 202)
	sessionAcceptanceReceipt(t, f.service, core.ID(added["id"].(string)), core.CommandSucceeded)
	extraID := added["output_id"].(string)
	extraPath := "/sessions/" + string(current.ID) + "/outputs/" + extraID
	for _, suffix := range []string{"stop", "start", "detach"} {
		command := integrationAPIRequest(t, a, "POST", extraPath+"/"+suffix, `{"request_id":"http-extra-`+suffix+`"}`, 202)
		sessionAcceptanceReceipt(t, f.service, core.ID(command["id"].(string)), core.CommandSucceeded)
		stored := integrationAPIRequest(t, a, "GET", extraPath, "", 200)
		if stored["desired_running"] != (suffix == "start") || stored["attached"] != (suffix != "detach") {
			t.Fatal("команда выхода потеряла намерение или прикрепление", suffix, stored)
		}
	}
	if f.starts.Load() != starts {
		t.Fatal("управление дополнительным выходом пересоздало здоровую группу")
	}
	beforePackets := publisherMediaCount(receiver.snapshot())
	continued := receiver.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= beforePackets+8 })
	if receiver.accepted.Load() != 1 {
		t.Fatal("управление дополнительным выходом пересоздало исходное соединение")
	}
	for _, message := range continued {
		if message.connection != originalConnection {
			t.Fatal("продолжение медиа получено от другого соединения")
		}
	}
	if _, err := f.manager.Feed(current); err != nil {
		t.Fatal("управление дополнительным выходом потеряло здоровый runtime", err)
	}
	bindingsBody := strings.TrimSuffix(startBody, "}") + `,"bindings":[]}`
	integrationAPIRequest(t, a, "POST", "/streams/"+string(f.stream.ID)+"/start", bindingsBody, 422)
	for _, suffix := range []string{"pause", "resume"} {
		v := integrationAPIRequest(t, a, "POST", "/sessions/"+string(current.ID)+"/"+suffix, `{"request_id":"http-`+suffix+`"}`, 202)
		sessionAcceptanceReceipt(t, f.service, core.ID(v["id"].(string)), core.CommandSucceeded)
		if suffix == "pause" {
			asset, err := f.service.GetAsset(context.Background(), f.asset.ID)
			if err != nil {
				t.Fatal(err)
			}
			revision := fmt.Sprint(asset.Revision)
			integrationAPIRequest(t, a, "POST", "/fallback-assets/"+string(asset.ID)+"/rebuild", `{"expected_revision":"`+revision+`"}`, 409)
			integrationAPIRequest(t, a, "DELETE", "/fallback-assets/"+string(asset.ID)+"?expected_revision="+revision, "", 409)
			payload := strings.NewReader("unread active fallback")
			upload := httptest.NewRequest("PUT", api.Prefix+"/fallback-assets/"+string(asset.ID)+"/content?kind=image&expected_revision="+revision, payload)
			upload.Header.Set("Content-Type", "application/octet-stream")
			response := httptest.NewRecorder()
			a.ServeHTTP(response, upload)
			if response.Code != 409 || payload.Len() != len("unread active fallback") {
				t.Fatal("пауза разрешила подготовку или чтение до admission", response.Code, payload.Len())
			}
		}
	}
	integrationAPIRequest(t, a, "GET", "/health/ready", "", 200)
	integrationAPIRequest(t, a, "GET", "/sessions/"+string(current.ID)+"/outputs?state=PUBLISHING", "", 422)
	stop := integrationAPIRequest(t, a, "POST", "/sessions/"+string(current.ID)+"/stop", `{"request_id":"http-stop"}`, 202)
	sessionAcceptanceReceipt(t, f.service, core.ID(stop["id"].(string)), core.CommandSucceeded)
	sessionAcceptanceWait(t, func() bool {
		s, err := f.service.GetSession(context.Background(), current.ID)
		return err == nil && s.State.Terminal() && !s.CleanupPending
	})
	terminal := integrationAPIRequest(t, a, "GET", "/sessions/"+string(current.ID)+"/metrics", "", 200)
	if terminal["availability"] != "unavailable" || terminal["snapshot"] != nil || terminal["reason"] == "" {
		t.Fatal("терминальная сессия получила старые метрики или нули", terminal)
	}
	integrationAPIRequest(t, a, "GET", "/sessions/unknown/metrics", "", 404)
	original := filepath.Join(f.directory, f.generation.RequiredProfiles[0].ManagedPath)
	if err := os.Rename(original, original+".unavailable"); err != nil {
		t.Fatal(err)
	}
	renamed := true
	defer func() {
		if renamed {
			if err := os.Rename(original+".unavailable", original); err != nil {
				t.Error(err)
			}
		}
	}()
	replay := integrationAPIRequest(t, a, "POST", "/streams/"+string(f.stream.ID)+"/start", startBody, 202)
	if replay["id"] != accepted["id"] {
		t.Fatal("терминальный replay повторно связывает ресурсы")
	}
	integrationAPIRequest(t, a, "POST", "/sessions/"+string(current.ID)+"/stop", `{"request_id":"http-start"}`, 409)
	if err := os.Rename(original+".unavailable", original); err != nil {
		t.Fatal(err)
	}
	renamed = false
	// Следующий запуск даёт существующего чужого родителя для проверки принадлежности.
	nextBody := strings.Replace(startBody, "http-start", "http-next", 1)
	next := integrationAPIRequest(t, a, "POST", "/streams/"+string(f.stream.ID)+"/start", nextBody, 202)
	second := f.running(t, sessionAcceptanceReceipt(t, f.service, core.ID(next["id"].(string)), core.CommandSucceeded))
	for _, method := range []string{"GET", "POST"} {
		path := "/sessions/" + string(second.ID) + "/outputs/" + string(output.ID)
		body := ""
		if method == "POST" {
			path += "/stop"
			body = `{"request_id":"foreign-output"}`
		}
		integrationAPIRequest(t, a, method, path, body, 404)
	}
	if _, err := f.service.FindReceipt(context.Background(), "foreign-output"); err == nil {
		t.Fatal("чужой output создал команду")
	}
	final := integrationAPIRequest(t, a, "POST", "/sessions/"+string(second.ID)+"/stop", `{"request_id":"http-next-stop"}`, 202)
	sessionAcceptanceReceipt(t, f.service, core.ID(final["id"].(string)), core.CommandSucceeded)
}
