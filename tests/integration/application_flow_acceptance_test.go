package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/message"
)

func applicationFlowRequest[T any](t *testing.T, a http.Handler, method, path string, body any, status int) T {
	t.Helper()
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	result := integrationAPIRequest(t, a, method, path, string(encoded), status)
	encoded, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var value T
	if err = json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func applicationFlowUpload(t *testing.T, a http.Handler, asset api.AssetDTO, data []byte) api.PreparationDTO {
	t.Helper()
	r := httptest.NewRequest("PUT", fmt.Sprintf("%s/fallback-assets/%s/content?kind=image&expected_revision=%d", api.Prefix, asset.ID, asset.Revision), bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatalf("загрузка: status=%d body=%s", w.Code, w.Body)
	}
	var v api.PreparationDTO
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.AssetID != asset.ID || v.GenerationID == "" || w.Header().Get("Location") != api.Prefix+"/fallback-assets/"+asset.ID+"/generations/"+v.GenerationID {
		t.Fatal("неверная квитанция загрузки", v, w.Header())
	}
	return v
}

func applicationFlowGeneration(t *testing.T, a http.Handler, upload api.PreparationDTO, status string) api.GenerationDTO {
	t.Helper()
	var v api.GenerationDTO
	sessionAcceptanceWait(t, func() bool {
		v = applicationFlowRequest[api.GenerationDTO](t, a, "GET", "/fallback-assets/"+upload.AssetID+"/generations/"+upload.GenerationID, nil, 200)
		if v.Status == "error" && status != "error" {
			t.Fatalf("подготовка завершилась ошибкой: %+v", v)
		}
		return v.Status == status
	})
	return v
}

func applicationFlowReceipt(t *testing.T, a http.Handler, r api.ReceiptDTO) api.ReceiptDTO {
	t.Helper()
	var v api.ReceiptDTO
	sessionAcceptanceWait(t, func() bool {
		v = applicationFlowRequest[api.ReceiptDTO](t, a, "GET", "/commands/"+r.ID, nil, 200)
		if v.Status == string(core.CommandFailed) || v.Status == string(core.CommandInterrupted) {
			t.Fatalf("команда завершилась ошибкой: %+v", v)
		}
		return v.Status == string(core.CommandSucceeded)
	})
	return v
}

func applicationFlowSource(t *testing.T, a http.Handler, sessionID, source string) {
	t.Helper()
	var snapshot api.SessionMetricsDTO
	sessionAcceptanceWait(t, func() bool {
		snapshot = applicationFlowRequest[api.SessionMetricsDTO](t, a, "GET", "/sessions/"+sessionID+"/metrics", nil, 200)
		if snapshot.Snapshot == nil || len(snapshot.Snapshot.Groups) != 3 {
			return false
		}
		for _, g := range snapshot.Snapshot.Groups {
			if g.Source != source || g.State != "READY" {
				return false
			}
		}
		return true
	}, func() {
		t.Logf("последний снимок при ожидании %s: %+v", source, snapshot.Snapshot)
	})
}

func applicationFlowPublish(t *testing.T, rawURL, key string) func() {
	t.Helper()
	packets, init := publisherStructuralInput(t, 1800)
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Fragment = key
	ctx, cancel := context.WithCancel(context.Background())
	client := &gortmplib.Client{URL: u, Publish: true}
	if err = client.Initialize(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	if err = client.Write(&message.SetChunkSize{Value: 128}); err != nil {
		client.Close()
		cancel()
		t.Fatal(err)
	}
	metadata := &message.DataAMF0{ChunkStreamID: 5, MessageStreamID: 1, Payload: amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: float64(60)}}}}
	if err = client.Write(metadata); err != nil {
		client.Close()
		cancel()
		t.Fatal(err)
	}
	for _, header := range []struct {
		typ  byte
		body []byte
	}{{9, append([]byte{0x90, 'a', 'v', '0', '1'}, init.Video.Headers()...)}, {8, append([]byte{0xaf, 0}, init.Audio.Headers()...)}} {
		if err = publisherWriteMessage(client.NetConn(), header.typ, 1, 0, header.body); err != nil {
			client.Close()
			cancel()
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer client.Close()
		start := time.Now()
		for _, p := range packets {
			at := publisherNearestMS(p.Info().DTS, p.Config().Info().TimeBase)
			timer := time.NewTimer(max(time.Until(start.Add(time.Duration(at)*time.Millisecond)), 0))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			typ := byte(8)
			body := append([]byte{0xaf, 1}, p.Payload()...)
			if p.Config().Info().Track == media.TrackVideo {
				typ = 9
				body = append([]byte{0x91, 'a', 'v', '0', '1'}, p.Payload()...)
			}
			if err := publisherWriteMessage(client.NetConn(), typ, 1, uint32(at+200), body); err != nil {
				return
			}
		}
		<-ctx.Done()
	}()
	stop := func() {
		cancel()
		client.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("источник OBS не завершился")
		}
	}
	t.Cleanup(stop)
	return stop
}

func TestApplicationHTTPFlowPreparesThreeProfilesAndPreservesHistory(t *testing.T) {
	f := newApplicationFlowFixture(t, sessionAcceptanceBuildHelper(t))
	a := integrationAPI(t, f)
	asset := applicationFlowRequest[api.AssetDTO](t, a, "POST", "/fallback-assets", map[string]any{"name": "Заглушка полного потока"}, 201)
	policy := map[string]any{"asset_id": asset.ID, "watchdog": map[string]any{"video_timeout_ns": "300000000", "audio_timeout_ns": "300000000", "recovery_window_ns": "70000000", "keyframe_timeout_ns": "1000000000"}}
	stream := applicationFlowRequest[api.StreamDTO](t, a, "POST", "/streams", map[string]any{"name": "Полный поток", "publication_key": "synthetic-session-key", "fallback": policy}, 201)
	var err error
	f.stream, err = f.service.GetStream(context.Background(), core.ID(stream.ID))
	if err != nil {
		t.Fatal(err)
	}
	profiles := applicationFlowProfiles()
	var receivers []*publisherReceiver
	var destinations []api.DestinationDTO
	var groups []api.GroupDTO
	for i, p := range profiles {
		profile := applicationFlowRequest[api.ProfileDTO](t, a, "POST", "/transcode-profiles", map[string]any{"name": fmt.Sprintf("Профиль %d", i), "profile": applicationFlowProfileJSON(p)}, 201)
		g := applicationFlowRequest[api.GroupDTO](t, a, "POST", "/transcode-groups", map[string]any{"name": fmt.Sprintf("Группа %d", i), "stream_id": stream.ID, "profile_id": profile.ID}, 201)
		groups = append(groups, g)
		r, _ := newPublisherReceiver(t, false)
		receivers = append(receivers, r)
		d := applicationFlowRequest[api.DestinationDTO](t, a, "POST", "/destinations", map[string]any{"name": fmt.Sprintf("Назначение %d", i), "stream_id": stream.ID, "group_id": g.ID, "platform": "custom", "endpoint": "rtmp://" + r.listener.Addr().String() + "/live", "stream_key": fmt.Sprintf("synthetic-key-%d", i), "enabled": true}, 201)
		destinations = append(destinations, d)
	}
	startPath := "/streams/" + stream.ID + "/start"
	startBody := map[string]any{"request_id": "start-full-flow", "destination_ids": []string{}}
	applicationFlowRequest[map[string]any](t, a, "POST", startPath, startBody, 409)
	upload := applicationFlowUpload(t, a, asset, applicationFlowPNG(t))
	applicationFlowGeneration(t, a, upload, "ready")
	variants := applicationFlowRequest[api.PageDTO[api.VariantDTO]](t, a, "GET", "/fallback-assets/"+asset.ID+"/variants", nil, 200)
	if len(variants.Items) != 3 {
		t.Fatalf("подготовлено %d вариантов вместо трёх", len(variants.Items))
	}
	for _, p := range profiles {
		found := false
		for _, v := range variants.Items {
			if v.Format.Video.Codec == string(p.Format.Video.Codec) && v.Format.Video.Width == p.Format.Video.Width && v.Format.Video.Height == p.Format.Video.Height && v.Format.Video.FrameRate.Numerator == 60 && v.Format.Audio.SampleRateHz == 48000 && v.State == "READY" {
				found = true
			}
		}
		if !found {
			t.Fatalf("не подготовлен профиль %+v", p.Format)
		}
	}
	asset = applicationFlowRequest[api.AssetDTO](t, a, "GET", "/fallback-assets/"+asset.ID, nil, 200)
	badUpload := applicationFlowUpload(t, a, asset, []byte("invalid image"))
	applicationFlowGeneration(t, a, badUpload, "error")
	asset = applicationFlowRequest[api.AssetDTO](t, a, "GET", "/fallback-assets/"+asset.ID, nil, 200)
	if asset.CurrentGenerationID != upload.GenerationID || asset.State != "READY" {
		t.Fatalf("ошибка замены уничтожила current: %+v", asset)
	}
	readyGeneration, err := f.service.GetGeneration(context.Background(), core.ID(upload.GenerationID))
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(f.directory, readyGeneration.RequiredProfiles[0].ManagedPath)
	if err = os.Rename(missing, missing+".held"); err != nil {
		t.Fatal(err)
	}
	// Контракт требует отказа до принятия команды; файловый I/O не обещает определённый HTTP status.
	startJSON, err := json.Marshal(startBody)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", api.Prefix+startPath, bytes.NewReader(startJSON))
	request.Header.Set("Content-Type", "application/json")
	rejected := httptest.NewRecorder()
	a.ServeHTTP(rejected, request)
	if rejected.Code < 400 || rejected.Code >= 600 || rejected.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("отсутствующий вариант не отклонён безопасно: %d %s", rejected.Code, rejected.Body)
	}
	sessions := applicationFlowRequest[api.PageDTO[api.SessionDTO]](t, a, "GET", "/streams/"+stream.ID+"/sessions", nil, 200)
	if len(sessions.Items) != 0 || f.starts.Load() != 0 {
		t.Fatal("отсутствующий файл создал Session либо worker", sessions)
	}
	if err = os.Rename(missing+".held", missing); err != nil {
		t.Fatal(err)
	}
	start := applicationFlowRequest[api.ReceiptDTO](t, a, "POST", startPath, startBody, 202)
	start = applicationFlowReceipt(t, a, start)
	current := applicationFlowRequest[api.SessionDTO](t, a, "GET", "/sessions/"+start.SessionID, nil, 200)
	if current.State != "RUNNING" || len(current.Groups) != 3 || f.starts.Load() != 3 {
		t.Fatalf("неполный запуск: %+v players=%d", current, f.starts.Load())
	}
	applicationFlowSource(t, a, current.ID, "FALLBACK")
	for _, r := range receivers {
		r.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= 8 })
	}
	duplicate := applicationFlowRequest[api.ReceiptDTO](t, a, "POST", startPath, startBody, 202)
	if duplicate.ID != start.ID || f.starts.Load() != 3 {
		t.Fatal("повтор RequestID запустил новый runtime")
	}
	applicationFlowRequest[map[string]any](t, a, "POST", startPath, map[string]any{"request_id": "start-full-flow", "destination_ids": []string{destinations[0].ID}}, 409)
	inputURL := applicationFlowServeIngest(t, f)
	stopInput := applicationFlowPublish(t, inputURL, "synthetic-session-key")
	applicationFlowSource(t, a, current.ID, "LIVE")
	workers := applicationFlowRequest[api.SessionMetricsDTO](t, a, "GET", "/sessions/"+current.ID+"/metrics", nil, 200)
	roles := map[string]int{}
	for _, worker := range workers.Snapshot.FFmpeg {
		roles[worker.Role]++
	}
	if roles[string(core.FFmpegLiveTranscode)] != 2 || roles[string(core.FFmpegFallbackPlayer)] != 3 {
		t.Fatal("для трёх профилей нужны два транскодировщика и три проигрывателя заглушки", roles)
	}
	var controls []api.ReceiptDTO
	command := func(path, id string) api.ReceiptDTO {
		t.Helper()
		r := applicationFlowRequest[api.ReceiptDTO](t, a, "POST", path, map[string]any{"request_id": id}, 202)
		r = applicationFlowReceipt(t, a, r)
		controls = append(controls, r)
		return r
	}
	command("/sessions/"+current.ID+"/pause", "pause-full-flow")
	applicationFlowSource(t, a, current.ID, "FALLBACK")
	applicationFlowRequest[map[string]any](t, a, "POST", "/fallback-assets/"+asset.ID+"/rebuild", map[string]any{"expected_revision": fmt.Sprint(asset.Revision)}, 409)
	command("/sessions/"+current.ID+"/resume", "resume-full-flow")
	applicationFlowSource(t, a, current.ID, "LIVE")
	healthy := func() {
		t.Helper()
		for i, r := range receivers {
			if r.accepted.Load() != 1 {
				t.Fatalf("здоровый выход %d пересоздан: %d", i, r.accepted.Load())
			}
		}
	}
	healthy()
	for i, r := range receivers {
		r.wait(t, func(m []publisherWireMessage) bool { return applicationFlowHasMarker(m, 20) })
		publisherAssertStructuralWire(t, r.snapshot(), profiles[i])
	}
	additionalReceiver, _ := newPublisherReceiver(t, false)
	additional := applicationFlowRequest[api.DestinationDTO](t, a, "POST", "/destinations", map[string]any{"name": "Дополнительный выход", "stream_id": stream.ID, "group_id": groups[1].ID, "platform": "custom", "endpoint": "rtmp://" + additionalReceiver.listener.Addr().String() + "/live", "stream_key": "additional-key", "enabled": false}, 201)
	add := applicationFlowRequest[api.ReceiptDTO](t, a, "POST", "/sessions/"+current.ID+"/outputs", map[string]any{"request_id": "add-full-flow", "destination_id": additional.ID}, 202)
	add = applicationFlowReceipt(t, a, add)
	additionalReceiver.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= 8 })
	if f.starts.Load() != 3 {
		t.Fatal("добавление в общую группу запустило новый player")
	}
	command("/sessions/"+current.ID+"/outputs/"+add.OutputID+"/detach", "detach-full-flow")
	additionalReceiver.waitClosed(t)
	applicationFlowRequest[map[string]any](t, a, "POST", "/sessions/"+current.ID+"/outputs/"+add.OutputID+"/start", map[string]any{"request_id": "restart-detached-output"}, 409)
	healthy()
	before := publisherMediaCount(receivers[0].snapshot())
	receivers[1].disconnect()
	receivers[1].wait(t, func(m []publisherWireMessage) bool {
		for _, v := range m {
			if v.connection >= 2 && v.typ == 8 && len(v.body) > 1 && v.body[1] == 1 {
				return true
			}
		}
		return false
	})
	receivers[0].wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) > before+4 })
	if receivers[0].accepted.Load() != 1 || receivers[2].accepted.Load() != 1 {
		t.Fatal("reconnect изменил соседние выходы")
	}
	stopInput()
	applicationFlowSource(t, a, current.ID, "FALLBACK")
	command("/sessions/"+current.ID+"/stop", "stop-full-flow")
	for _, r := range receivers {
		r.waitClosed(t)
	}
	ended := applicationFlowRequest[api.SessionDTO](t, a, "GET", "/sessions/"+current.ID, nil, 200)
	if ended.State != "STOPPED" || ended.EndedAt == nil || len(ended.Groups) != 3 {
		t.Fatalf("неполная история завершения: %+v", ended)
	}
	sessionAcceptanceWait(t, func() bool {
		s, err := f.service.GetSession(context.Background(), core.ID(current.ID))
		return err == nil && !s.CleanupPending && !s.Issued
	})
	outputs := applicationFlowRequest[api.PageDTO[api.OutputDTO]](t, a, "GET", "/sessions/"+current.ID+"/outputs", nil, 200)
	if len(outputs.Items) != 4 {
		t.Fatalf("история выходов потеряна: %+v", outputs)
	}
	metrics := applicationFlowRequest[api.SessionMetricsDTO](t, a, "GET", "/sessions/"+current.ID+"/metrics", nil, 200)
	if metrics.Availability != "unavailable" || metrics.Snapshot != nil {
		t.Fatalf("терминальная Session сохранила runtime: %+v", metrics)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = f.manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(ctx, filepath.Join(f.directory, "state.sqlite"), sessionAcceptanceOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	service, err := core.NewService(reopened, reopened.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range append(controls, start, add) {
		stored, err := service.GetReceipt(ctx, core.ID(receipt.ID))
		if err != nil || string(stored.Status) != receipt.Status || stored.RequestID != receipt.RequestID {
			t.Fatalf("квитанция после рестарта: %+v %v", stored, err)
		}
	}
	history, err := service.GetSession(ctx, core.ID(current.ID))
	if err != nil || history.State != core.SessionStopped || len(history.Snapshot.Groups) != 3 || history.CleanupPending || history.Issued {
		t.Fatalf("история после рестарта: %+v %v", history, err)
	}
	storedOutputs, err := service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 32, SessionID: history.ID}})
	if err != nil || len(storedOutputs.Items) != 4 {
		t.Fatalf("выходы после рестарта: %+v %v", storedOutputs, err)
	}
	for _, output := range storedOutputs.Items {
		if output.CleanupPending || output.Issued || output.DesiredRunning {
			t.Fatal("очистка выхода не сохранилась после рестарта", output)
		}
	}
	if err = service.ConfigureTelemetry(ctx, f.config.Telemetry); err != nil {
		t.Fatal(err)
	}
	anchor, err := service.RuntimeAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(anchor.Sessions) != 0 {
		t.Fatal("рестарт восстановил старый runtime", anchor)
	}
}

func applicationFlowHasMarker(messages []publisherWireMessage, marker byte) bool {
	for _, m := range messages {
		if m.typ == 9 && len(m.body) > 5 && m.body[len(m.body)-1] == marker {
			return true
		}
	}
	return false
}
