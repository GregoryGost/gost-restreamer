package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

func publicTelemetryObject(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("ожидался объект JSON: %#v", value)
	}
	return object
}

func publicTelemetryMedia(t *testing.T, value any) map[string]any {
	t.Helper()
	return publicTelemetryObject(t, publicTelemetryObject(t, value)["media"])
}

func publicTelemetryUint(t *testing.T, value any) uint64 {
	t.Helper()
	text, ok := value.(string)
	if !ok {
		t.Fatalf("точный счётчик не передан строкой: %#v", value)
	}
	n, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func publicTelemetryWireTotals(t *testing.T, receiver *publisherReceiver) (payload, units, videos uint64) {
	t.Helper()
	video, audio := publisherConfigs(t, publisherProfile(core.CodecAV1, 640, 360), 1)
	for _, message := range receiver.snapshot() {
		var data, expected []byte
		if message.typ == 8 && len(message.body) >= 2 && message.body[1] == 1 {
			data, expected = message.body[2:], publisherPayload(audio, 0)
		}
		if message.typ == 9 && len(message.body) >= 5 && message.body[0]&0x80 != 0 && message.body[0]&15 == 1 {
			data, expected = message.body[5:], publisherPayload(video, 10)
			videos++
		}
		if data != nil {
			if !bytes.Equal(data, expected) {
				t.Fatalf("приёмник получил изменённую полезную нагрузку: %x", data)
			}
			units++
			payload += uint64(len(data))
		}
	}
	return
}

func publicTelemetryCheckOutput(t *testing.T, metrics map[string]any, receiver *publisherReceiver) {
	t.Helper()
	payload, units, videos := publicTelemetryWireTotals(t, receiver)
	media := publicTelemetryMedia(t, metrics)
	counters := publicTelemetryObject(t, metrics["counters"])
	if videos < 8 || units <= videos {
		t.Fatalf("недостаточно реального видео и аудио для сверки: %d/%d", videos, units)
	}
	if publicTelemetryUint(t, media["payload_bytes"]) != payload || publicTelemetryUint(t, media["written_bytes"]) != receiver.bytes.Load() || publicTelemetryUint(t, media["video_frames"]) != videos || publicTelemetryUint(t, media["audio_units"]) != units-videos || publicTelemetryUint(t, counters["access_units"]) != units {
		t.Fatalf("REST/SSE не совпал с независимым RTMP-приёмником: media=%v counters=%v wire payload=%d units=%d video=%d bytes=%d", media, counters, payload, units, videos, receiver.bytes.Load())
	}
	if counters["attempts"] != "1" || counters["connections"] != "1" || counters["reconnects"] != "0" || receiver.accepted.Load() != 1 {
		t.Fatal("конечный источник вызвал лишнее подключение", counters)
	}
	for reason, count := range publicTelemetryObject(t, metrics["delivery_drops"]) {
		if count != "0" {
			t.Fatalf("неожиданная потеря отправки %s=%v", reason, count)
		}
	}
}

func publicTelemetryCheckGroup(t *testing.T, metrics map[string]any) {
	t.Helper()
	video, audio := publisherConfigs(t, publisherProfile(core.CodecAV1, 640, 360), 1)
	fallback := publicTelemetryObject(t, metrics["fallback_input"])
	live := publicTelemetryObject(t, metrics["live_input"])
	// За одну секунду источник выдаёт 60 видео-AU и 47 AAC-AU по 1024 сэмпла при 48 kHz.
	payload := strconv.Itoa(60*len(publisherPayload(video, 10)) + 47*len(publisherPayload(audio, 0)))
	if fallback["video_frames"] != "60" || fallback["audio_units"] != "47" || fallback["payload_bytes"] != payload || live["video_frames"] != "0" || live["audio_units"] != "0" || live["payload_bytes"] != "0" {
		t.Fatal("группа смешала фактические входы LIVE и FALLBACK", metrics)
	}
}

func TestAcceptancePublicTelemetryMatchesKnownIngressAndNativeWrites(t *testing.T) {
	ctx := context.Background()
	receiver, _ := newPublisherReceiver(t, false)
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	f.stream.Fallback.Watchdog.VideoTimeout = time.Minute
	f.stream.Fallback.Watchdog.AudioTimeout = time.Minute
	var err error
	f.stream, err = f.service.UpdateStream(ctx, f.stream, f.stream.Revision)
	if err != nil {
		t.Fatal(err)
	}
	f.config.Group.FFmpeg.VideoTimeout, f.config.Group.FFmpeg.AudioTimeout = time.Minute, time.Minute
	finished := filepath.Join(f.directory, "finite-source-finished")
	f.config.Group.Player = func(_ context.Context, _ string, _ *os.File, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
		binary, err := os.Executable()
		return ffmpeg.Command{Binary: binary, Args: []string{"-test.run=^TestPublicTelemetryFiniteMediaChild$", "--", "--public-telemetry", attempt.OutputPath, finished}}, err
	}
	f.launch(t)
	a := integrationAPI(t, f)
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	start := integrationAPIRequest(t, a, "POST", "/streams/"+string(f.stream.ID)+"/start", fmt.Sprintf(`{"request_id":"start-public-metrics","destination_ids":[%q]}`, f.destinations[0].ID), 202)
	r := sessionAcceptanceReceipt(t, f.service, core.ID(start["id"].(string)), core.CommandSucceeded)
	current := f.running(t, r)
	path := "/sessions/" + string(current.ID)
	anchor := integrationAPIRequest(t, a, "GET", "/events/snapshot", "", 200)
	stream := publicTelemetrySubscribe(t, server, "/events?cursor="+url.QueryEscape(anchor["cursor"].(string)), "")
	listener := acceptanceMonitoringIngest(t, f)
	videoConfig, audioConfig := publisherConfigs(t, publisherProfile(core.CodecAV1, 640, 360), 1)
	video, audio := publisherPayload(videoConfig, 20), publisherPayload(audioConfig, 0)
	client := acceptanceMonitoringClient(t, f, listener, videoConfig)
	acceptanceMonitoringBeforeReady(t, client, audioConfig, video, audio, 3)
	wantInputBytes := strconv.Itoa(3 * (len(video) + len(audio)))
	sessionAcceptanceWait(t, func() bool { _, err := os.Stat(finished); return err == nil })
	var snapshot, outputMetrics map[string]any
	sessionAcceptanceWait(t, func() bool {
		response := integrationAPIRequest(t, a, "GET", path+"/metrics", "", 200)
		if response["availability"] != "available" {
			return false
		}
		snapshot = publicTelemetryObject(t, response["snapshot"])
		input := publicTelemetryObject(t, snapshot["input"])
		if input["metrics"] == nil || publicTelemetryMedia(t, input["metrics"])["payload_bytes"] != wantInputBytes {
			return false
		}
		outputs := snapshot["outputs"].([]any)
		if len(outputs) != 1 || publicTelemetryObject(t, outputs[0])["metrics"] == nil {
			return false
		}
		outputMetrics = publicTelemetryObject(t, publicTelemetryObject(t, outputs[0])["metrics"])
		media := publicTelemetryMedia(t, outputMetrics)
		_, units, _ := publicTelemetryWireTotals(t, receiver)
		return units >= 16 && publicTelemetryUint(t, publicTelemetryObject(t, outputMetrics["counters"])["access_units"]) == units && publicTelemetryUint(t, media["written_bytes"]) == receiver.bytes.Load() && publicTelemetryObject(t, publicTelemetryObject(t, media["rates"])["fps"])["value"] == float64(0)
	})
	input := publicTelemetryObject(t, snapshot["input"])
	inputMedia := publicTelemetryMedia(t, input["metrics"])
	format := publicTelemetryObject(t, inputMedia["format"])
	if input["connected"] != true || inputMedia["video_frames"] != "3" || inputMedia["audio_units"] != "3" || inputMedia["payload_bytes"] != wantInputBytes || format["video_codec"] != "av1" || format["audio_codec"] != "aac" || format["width"] != float64(640) || format["height"] != float64(360) {
		t.Fatal("REST потерял известный вход до готовности LIVE", input)
	}
	groups := snapshot["groups"].([]any)
	if len(groups) != 1 {
		t.Fatal("неверное число наблюдаемых групп", groups)
	}
	group := publicTelemetryObject(t, groups[0])
	live := publicTelemetryObject(t, publicTelemetryObject(t, group["metrics"])["live_input"])
	if group["source"] != "FALLBACK" || live["video_frames"] != "0" || live["payload_bytes"] != "0" {
		t.Fatal("полученный OBS ошибочно посчитан потреблённым LIVE", group)
	}
	publicTelemetryCheckGroup(t, publicTelemetryObject(t, group["metrics"]))
	publicTelemetryCheckOutput(t, outputMetrics, receiver)
	fps := publicTelemetryObject(t, publicTelemetryObject(t, publicTelemetryMedia(t, outputMetrics)["rates"])["fps"])
	if fps["status"] != "available" || fps["value"] != float64(0) || publicTelemetryUint(t, fps["duration_ns"]) == 0 {
		t.Fatal("наблюдённый интервал без новых кадров не представлен измеренным нулём", fps)
	}
	seenInput, seenOutput, seenGroup := false, false, false
	for !seenInput || !seenOutput || !seenGroup {
		frame, ok := stream.next(t)
		if !ok || frame.kind == "resync_required" {
			t.Fatal("непрерывная подписка потеряла измерения", frame)
		}
		if frame.data["session_id"] != string(current.ID) || frame.kind != "metrics" {
			continue
		}
		if frame.data["input"] != nil {
			observed := publicTelemetryObject(t, frame.data["input"])
			if observed["metrics"] != nil {
				media := publicTelemetryMedia(t, observed["metrics"])
				if media["payload_bytes"] == wantInputBytes {
					if media["video_frames"] != "3" || media["audio_units"] != "3" || observed["connected"] != true {
						t.Fatal("SSE изменил известные входные AU", observed)
					}
					seenInput = true
				}
			}
		}
		if frame.data["group"] != nil {
			metrics := publicTelemetryObject(t, frame.data["group"])
			if publicTelemetryObject(t, metrics["fallback_input"])["video_frames"] == "60" {
				publicTelemetryCheckGroup(t, metrics)
				seenGroup = true
			}
		}
		if frame.data["output"] != nil {
			metrics := publicTelemetryObject(t, frame.data["output"])
			if publicTelemetryMedia(t, metrics)["written_bytes"] == publicTelemetryMedia(t, outputMetrics)["written_bytes"] {
				publicTelemetryCheckOutput(t, metrics, receiver)
				seenOutput = true
			}
		}
	}
	stream.close(t)
	client.Close()
	stop := integrationAPIRequest(t, a, "POST", path+"/stop", `{"request_id":"stop-public-metrics"}`, 202)
	sessionAcceptanceReceipt(t, f.service, core.ID(stop["id"].(string)), core.CommandSucceeded)
	sessionAcceptanceWait(t, func() bool {
		s, err := f.service.GetSession(ctx, current.ID)
		return err == nil && s.State.Terminal() && !s.CleanupPending && !s.Issued
	})
	terminal := integrationAPIRequest(t, a, "GET", path+"/metrics", "", 200)
	if terminal["availability"] != "unavailable" || terminal["reason"] != "session_terminal" || terminal["snapshot"] != nil {
		t.Fatal("терминальный запуск сохранил текущие метрики", terminal)
	}
	cleaned, err := f.service.GetSession(ctx, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	cleanedOutputs := f.outputs(t, current.ID)
	for _, output := range cleanedOutputs {
		if output.CleanupPending || output.Issued {
			t.Fatal("выход не подтвердил очистку", output)
		}
	}
	history := integrationAPIRequest(t, a, "GET", path, "", 200)
	receipt := integrationAPIRequest(t, a, "GET", "/commands/"+stop["id"].(string), "", 200)
	if err := f.manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(ctx, filepath.Join(f.directory, "state.sqlite"), sessionAcceptanceOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	service, err := core.NewService(reopened, reopened.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureTelemetry(ctx, f.config.Telemetry); err != nil {
		t.Fatal(err)
	}
	freshAPI := publicTelemetryAPI(t, service)
	if got := integrationAPIRequest(t, freshAPI, "GET", path, "", 200); !reflect.DeepEqual(got, history) {
		t.Fatal("история изменилась после открытия БД", got, history)
	}
	if got := integrationAPIRequest(t, freshAPI, "GET", "/commands/"+stop["id"].(string), "", 200); !reflect.DeepEqual(got, receipt) {
		t.Fatal("квитанция изменилась после открытия БД", got, receipt)
	}
	after, err := service.GetSession(ctx, current.ID)
	if err != nil || !reflect.DeepEqual(after, cleaned) {
		t.Fatal("долговечный lifecycle/cleanup изменился", after, cleaned, err)
	}
	for _, before := range cleanedOutputs {
		after, err := service.GetOutput(ctx, before.ID)
		if err != nil || !reflect.DeepEqual(after, before) {
			t.Fatal("снимок выхода/cleanup изменился", after, before, err)
		}
	}
	if got := integrationAPIRequest(t, freshAPI, "GET", path+"/metrics", "", 200); !reflect.DeepEqual(got, terminal) {
		t.Fatal("рестарт восстановил старые метрики", got)
	}
	fresh := integrationAPIRequest(t, freshAPI, "GET", "/events/snapshot", "", 200)
	if len(fresh["sessions"].([]any)) != 0 || fresh["gpu"] != nil || fresh["cursor"] == anchor["cursor"] {
		t.Fatal("старый runtime восстановлен из истории", fresh)
	}
}

func publicTelemetryGPU(value uint64, at time.Time) core.GPUSnapshot {
	metric := core.GPUMetric{Status: core.MetricAvailable, Value: value, SampledAt: at, LastSuccessAt: at}
	absent := core.GPUMetric{Status: core.MetricUnavailable, Reason: core.ReasonSourceAbsent, SampledAt: at}
	return core.GPUSnapshot{Status: core.MetricAvailable, SampledAt: at, StaleAfter: time.Minute, Devices: []core.GPUDevice{{UUID: "synthetic-gpu", Utilization: absent, MemoryUtilization: absent, EncoderUtilization: absent, DecoderUtilization: absent, MemoryUsedBytes: metric, MemoryTotalBytes: absent, MemoryFreeBytes: absent}}}
}

func publicTelemetryGPUValue(t *testing.T, value any) any {
	t.Helper()
	gpu := publicTelemetryObject(t, value)
	devices := gpu["devices"].([]any)
	if len(devices) != 1 {
		t.Fatal("потеряно точное системное наблюдение", gpu)
	}
	return publicTelemetryObject(t, publicTelemetryObject(t, devices[0])["memory_used_bytes"])["value"]
}

func publicTelemetryResync(t *testing.T, server *httptest.Server, cursor, reason string) {
	t.Helper()
	stream := publicTelemetrySubscribe(t, server, "/events?cursor="+url.QueryEscape(cursor), "")
	frame, ok := stream.next(t)
	if !ok || frame.kind != "resync_required" || frame.id != "" || frame.data["reason"] != reason || frame.data["snapshot_url"] != api.Prefix+"/events/snapshot" {
		t.Fatal("неполный диапазон не требует новый снимок", frame)
	}
	if frame, ok := stream.next(t); ok {
		t.Fatal("после resync выдано событие старого диапазона", frame)
	}
	stream.close(t)
}

func TestAcceptancePublicTelemetryResyncSnapshotAndResubscribe(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "events.sqlite")
	limits := core.TelemetryLimits{Events: core.EventLimits{MaxEvents: 8, MaxBytes: 1 << 20, MaxEventBytes: 65536, MaxPageSize: 8}, MediaStaleAfter: time.Minute, MaxGPUDevices: 2, MaxGPUUUIDBytes: 256, MaxGPUBytes: 16384}
	open := func() (*storage.Store, *core.Service, *api.API, *httptest.Server) {
		t.Helper()
		store, err := storage.Open(ctx, path, sessionAcceptanceOptions())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		service, err := core.NewService(store, store.Incarnation())
		if err != nil {
			t.Fatal(err)
		}
		if err := service.ConfigureTelemetry(ctx, limits); err != nil {
			t.Fatal(err)
		}
		a := publicTelemetryAPI(t, service)
		server := httptest.NewServer(a)
		t.Cleanup(server.Close)
		return store, service, a, server
	}
	store, service, a, server := open()
	observer, err := service.OpenSystemObserver(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close(ctx) })
	stale := integrationAPIRequest(t, a, "GET", "/events/snapshot", "", 200)
	at := time.Now()
	const base uint64 = 9007199254740993
	for i := uint64(0); i < 12; i++ {
		if err := observer.ObserveGPU(ctx, publicTelemetryGPU(base+i, at.Add(time.Duration(i)*time.Nanosecond))); err != nil {
			t.Fatal(err)
		}
	}
	publicTelemetryResync(t, server, stale["cursor"].(string), "gap")
	fresh := integrationAPIRequest(t, a, "GET", "/events/snapshot", "", 200)
	if fresh["cursor"] == stale["cursor"] || publicTelemetryGPUValue(t, fresh["gpu"]) != strconv.FormatUint(base+11, 10) {
		t.Fatal("новый снимок не восстановил последнее состояние", fresh)
	}
	stream := publicTelemetrySubscribe(t, server, "/events?cursor="+url.QueryEscape(fresh["cursor"].(string)), "")
	if err := observer.ObserveGPU(ctx, publicTelemetryGPU(base+12, at.Add(12*time.Nanosecond))); err != nil {
		t.Fatal(err)
	}
	frame, ok := stream.next(t)
	if !ok || frame.kind != "gpu" || publicTelemetryGPUValue(t, frame.data["gpu"]) != strconv.FormatUint(base+12, 10) || frame.id == fresh["cursor"] {
		t.Fatal("подписка после снимка потеряла следующее событие", frame)
	}
	lastID := frame.id
	stream.close(t)
	if err := observer.ObserveGPU(ctx, publicTelemetryGPU(base+13, at.Add(13*time.Nanosecond))); err != nil {
		t.Fatal(err)
	}
	resumed := publicTelemetrySubscribe(t, server, "/events", lastID)
	frame, ok = resumed.next(t)
	if !ok || frame.kind != "gpu" || publicTelemetryGPUValue(t, frame.data["gpu"]) != strconv.FormatUint(base+13, 10) || frame.id == lastID {
		t.Fatal("Last-Event-ID повторил прежнее либо пропустил следующее событие", frame)
	}
	resumed.close(t)
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, restarted, freshAPI, freshServer := open()
	publicTelemetryResync(t, freshServer, lastID, "reset")
	reset := integrationAPIRequest(t, freshAPI, "GET", "/events/snapshot", "", 200)
	if reset["gpu"] != nil || len(reset["sessions"].([]any)) != 0 {
		t.Fatal("новый процесс восстановил RAM-телеметрию", reset)
	}
	newObserver, err := restarted.OpenSystemObserver(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = newObserver.Close(ctx) })
	newStream := publicTelemetrySubscribe(t, freshServer, "/events?cursor="+url.QueryEscape(reset["cursor"].(string)), "")
	if err := newObserver.ObserveGPU(ctx, publicTelemetryGPU(base+14, time.Now())); err != nil {
		t.Fatal(err)
	}
	frame, ok = newStream.next(t)
	if !ok || frame.kind != "gpu" || publicTelemetryGPUValue(t, frame.data["gpu"]) != strconv.FormatUint(base+14, 10) {
		encoded, _ := json.Marshal(frame.data)
		t.Fatalf("новая подписка после reset не получила новое состояние: %s", encoded)
	}
	newStream.close(t)
}
