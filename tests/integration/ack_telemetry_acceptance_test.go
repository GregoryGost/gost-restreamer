package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/config"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

func ackTelemetrySend(t *testing.T, r *publisherReceiver, values ...uint32) {
	t.Helper()
	r.mu.Lock()
	connections := make([]net.Conn, 0, len(r.connections))
	for c := range r.connections {
		connections = append(connections, c)
	}
	r.mu.Unlock()
	if len(connections) != 1 {
		t.Fatalf("ожидалось одно активное соединение: %d", len(connections))
	}
	conn := connections[0]
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, v := range values {
		wire := []byte{2, 0, 0, 0, 0, 0, 4, 3, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(wire[12:], v)
		if _, err := io.Copy(conn, bytes.NewReader(wire)); err != nil {
			t.Fatal(err)
		}
	}
}

func ackTelemetryOutput(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	if response["availability"] != "available" {
		return nil
	}
	s := publicTelemetryObject(t, response["snapshot"])
	values := s["outputs"].([]any)
	if len(values) == 0 {
		return nil
	}
	if len(values) != 1 {
		t.Fatalf("неверное число выходов: %d", len(values))
	}
	value := publicTelemetryObject(t, values[0])["metrics"]
	if value == nil {
		return nil
	}
	return publicTelemetryObject(t, value)
}
func ackTelemetryAssert(t *testing.T, m map[string]any, total, last, max string, absent bool) {
	t.Helper()
	for key, want := range map[string]string{"ack_ahead_events_total": total, "ack_ahead_bytes_last": last, "ack_ahead_bytes_max": max} {
		if m[key] != want {
			t.Fatalf("метрика %s=%#v, ожидается %s; полный набор=%v", key, m[key], want, m)
		}
		if _, ok := publicTelemetryObject(t, m["counters"])[key]; ok {
			t.Fatal("ACK повторяется в counters", key)
		}
	}
	at, exists := m["ack_last_anomaly_at"]
	if !exists || absent && at != nil || !absent && at == nil {
		t.Fatal("неверная доступность времени ACK", m)
	}
	if !absent {
		value, ok := at.(string)
		if !ok {
			t.Fatal("время ACK не строка", at)
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil || parsed.IsZero() || value[len(value)-1] != 'Z' {
			t.Fatal("время ACK не UTC", at, err)
		}
	}
}

func TestACKTelemetryAcceptanceNativeSessionRESTSSEAndReconnect(t *testing.T) {
	ctx := context.Background()
	receiver, _ := newPublisherReceiver(t, false)
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	// Штатный бюджет управления исключает истечение короткого срока фикстуры при инструментировании покрытия.
	f.config.OperationTimeout = config.Default().Session.OperationTimeout
	f.stream.Fallback.Watchdog.VideoTimeout = time.Minute
	f.stream.Fallback.Watchdog.AudioTimeout = time.Minute
	var err error
	f.stream, err = f.service.UpdateStream(ctx, f.stream, f.stream.Revision)
	if err != nil {
		t.Fatal(err)
	}
	f.config.Group.FFmpeg.VideoTimeout, f.config.Group.FFmpeg.AudioTimeout = time.Minute, time.Minute
	finished := filepath.Join(f.directory, "ack-finite-source-finished")
	f.config.Group.Player = func(_ context.Context, _ string, _ *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		binary, err := os.Executable()
		return ffmpeg.Command{Binary: binary, Args: []string{"-test.run=^TestPublicTelemetryFiniteMediaChild$", "--", "--public-telemetry", a.OutputPath, finished}}, err
	}
	f.launch(t)
	a := integrationAPI(t, f)
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	current := f.running(t, f.start(t, "start-ack-metrics", f.destinations[0].ID))
	path := "/sessions/" + string(current.ID)
	sessionAcceptanceWait(t, func() bool { _, err := os.Stat(finished); return err == nil })
	metrics := func() map[string]any {
		return ackTelemetryOutput(t, integrationAPIRequest(t, a, "GET", path+"/metrics", "", 200))
	}
	var m map[string]any
	sessionAcceptanceWait(t, func() bool {
		m = metrics()
		return m != nil && publicTelemetryMedia(t, m)["written_bytes"] == strconv.FormatUint(receiver.bytes.Load(), 10) && publicTelemetryObject(t, publicTelemetryObject(t, publicTelemetryMedia(t, m)["rates"])["fps"])["value"] == float64(0)
	})
	ackTelemetryAssert(t, m, "0", "0", "0", true)
	anchor := integrationAPIRequest(t, a, "GET", "/events/snapshot", "", 200)
	stream := publicTelemetrySubscribe(t, server, "/events?cursor="+url.QueryEscape(anchor["cursor"].(string)), "")
	baseline := receiver.bytes.Load()
	before := time.Now().UTC()
	for extra := uint32(4); extra <= 13; extra++ {
		ackTelemetrySend(t, receiver, uint32(baseline)+extra)
	}
	ackTelemetrySend(t, receiver, uint32(baseline)+13, uint32(baseline)+12, uint32(baseline)+13+1<<31)
	sessionAcceptanceWait(t, func() bool { m = metrics(); return m != nil && m["ack_ahead_events_total"] == "10" })
	ackTelemetryAssert(t, m, "10", "13", "13", false)
	parsed, err := time.Parse(time.RFC3339Nano, m["ack_last_anomaly_at"].(string))
	if err != nil || parsed.Before(before) || parsed.After(time.Now()) {
		t.Fatal("время не относится к реальному ACK", m, err)
	}
	for {
		frame, ok := stream.next(t)
		if !ok || frame.kind == "resync_required" {
			t.Fatal("SSE потерял ACK", frame)
		}
		if frame.data["output"] != nil {
			event := publicTelemetryObject(t, frame.data["output"])
			if event["ack_ahead_events_total"] == "10" {
				ackTelemetryAssert(t, event, "10", "13", "13", false)
				if event["ack_last_anomaly_at"] != m["ack_last_anomaly_at"] {
					t.Fatal("REST/SSE различаются", event, m)
				}
				break
			}
		}
	}
	stream.close(t)
	receiver.disconnect()
	sessionAcceptanceWait(t, func() bool {
		m = metrics()
		return receiver.accepted.Load() == 2 && m != nil && publicTelemetryObject(t, m["counters"])["connections"] == "2" && publicTelemetryMedia(t, m)["written_bytes"] == strconv.FormatUint(receiver.bytes.Load(), 10)
	})
	ackTelemetryAssert(t, m, "10", "13", "13", false)
	// Новый ACK сравнивается с байтами нового соединения, а не суммой двух попыток.
	freshBytes := receiver.bytes.Load() - baseline
	ackTelemetrySend(t, receiver, uint32(freshBytes)+20)
	sessionAcceptanceWait(t, func() bool { m = metrics(); return m != nil && m["ack_ahead_events_total"] == "11" })
	ackTelemetryAssert(t, m, "11", "20", "20", false)
	if counters := publicTelemetryObject(t, m["counters"]); counters["attempts"] != "2" || counters["reconnects"] != "1" {
		t.Fatal("числовые ACK вызвали переподключение", counters)
	}
	output := f.outputs(t, current.ID)[0]
	outputPath := path + "/outputs/" + string(output.ID)
	stopped := integrationAPIRequest(t, a, "POST", outputPath+"/stop", `{"request_id":"stop-ack-output"}`, 202)
	sessionAcceptanceReceipt(t, f.service, core.ID(stopped["id"].(string)), core.CommandSucceeded)
	sessionAcceptanceWait(t, func() bool {
		value, err := f.service.GetOutput(ctx, output.ID)
		return err == nil && !value.Issued && !value.CleanupPending
	})
	receiver.waitClosed(t)
	started := integrationAPIRequest(t, a, "POST", outputPath+"/start", `{"request_id":"restart-ack-output"}`, 202)
	sessionAcceptanceReceipt(t, f.service, core.ID(started["id"].(string)), core.CommandSucceeded)
	sessionAcceptanceWait(t, func() bool {
		m = metrics()
		return receiver.accepted.Load() == 3 && m != nil && publicTelemetryObject(t, m["counters"])["connections"] == "1"
	})
	ackTelemetryAssert(t, m, "0", "0", "0", true)
	stop := integrationAPIRequest(t, a, "POST", path+"/stop", `{"request_id":"stop-ack-metrics"}`, 202)
	sessionAcceptanceReceipt(t, f.service, core.ID(stop["id"].(string)), core.CommandSucceeded)
	sessionAcceptanceWait(t, func() bool {
		response := integrationAPIRequest(t, a, "GET", path+"/metrics", "", 200)
		return response["availability"] == "unavailable" && response["snapshot"] == nil
	})
}

func TestACKTelemetryAcceptanceExactLargeValuesUTCNullAndStaleOwner(t *testing.T) {
	ctx := context.Background()
	f := newPublisherPipeline(t, []core.GroupProfile{publisherProfile(core.CodecAV1, 640, 360)}, []string{"rtmp://receiver.invalid/live"}, []string{"synthetic"}, []int{0})
	if err := f.observer.Close(ctx); err != nil {
		t.Fatal(err)
	}
	limits := core.TelemetryLimits{Events: core.EventLimits{MaxEvents: 32, MaxBytes: 1 << 20, MaxEventBytes: 65536, MaxPageSize: 8}, MediaStaleAfter: time.Minute, MaxGPUDevices: 1, MaxGPUUUIDBytes: 64, MaxGPUBytes: 4096}
	if err := f.service.ConfigureTelemetry(ctx, limits); err != nil {
		t.Fatal(err)
	}
	receipts, err := f.service.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 32, SessionID: f.parent.SessionID}})
	if err != nil {
		t.Fatal(err)
	}
	var fence core.ExecutionFence
	for _, receipt := range receipts.Items {
		if receipt.Kind == core.CommandStartOutput {
			fence, err = f.service.Claim(ctx, receipt.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if fence.OutputID == "" {
		t.Fatal("нет разрешения выхода")
	}
	if err = f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	if err = f.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	observer, err := f.service.OpenOutputObserver(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close(ctx) })
	a := publicTelemetryAPI(t, f.service)
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	path := "/sessions/" + string(f.parent.SessionID) + "/metrics"
	local := time.Date(2026, 11, 8, 18, 35, 27, 123456789, time.FixedZone("synthetic-offset", 3*60*60))
	value := core.OutputMetrics{SampledAt: local}
	if err = observer.ObserveMetrics(ctx, value); err != nil {
		t.Fatal(err)
	}
	m := ackTelemetryOutput(t, integrationAPIRequest(t, a, "GET", path, "", 200))
	ackTelemetryAssert(t, m, "0", "0", "0", true)
	anchor := integrationAPIRequest(t, a, "GET", "/events/snapshot", "", 200)
	stream := publicTelemetrySubscribe(t, server, "/events?cursor="+url.QueryEscape(anchor["cursor"].(string)), "")
	value.SampledAt = local.Add(time.Second)
	value.AckAheadEventsTotal = math.MaxUint64
	value.AckAheadBytesLast = 9007199254740993
	value.AckAheadBytesMax = math.MaxUint64
	value.AckLastAnomalyAt = local
	if err = observer.ObserveMetrics(ctx, value); err != nil {
		t.Fatal(err)
	}
	m = ackTelemetryOutput(t, integrationAPIRequest(t, a, "GET", path, "", 200))
	ackTelemetryAssert(t, m, "18446744073709551615", "9007199254740993", "18446744073709551615", false)
	if m["ack_last_anomaly_at"] != "2026-11-08T15:35:27.123456789Z" {
		t.Fatal("потеря точного UTC-времени", m)
	}
	frame, ok := stream.next(t)
	if !ok || frame.data["output"] == nil {
		t.Fatal("SSE потерял метрики", frame)
	}
	ackTelemetryAssert(t, publicTelemetryObject(t, frame.data["output"]), "18446744073709551615", "9007199254740993", "18446744073709551615", false)
	if err = observer.ObserveMetrics(ctx, value); !errors.Is(err, core.ErrStale) {
		t.Fatal("повторное измерение принято", err)
	}
	replacement, err := f.service.OpenOutputObserver(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replacement.Close(ctx) })
	value.SampledAt = value.SampledAt.Add(time.Second)
	if err = observer.ObserveMetrics(ctx, value); !errors.Is(err, core.ErrStale) {
		t.Fatal("отозванный владелец обновил метрики", err)
	}
	snapshot, err := f.service.RuntimeSnapshot(ctx, core.RuntimeSessionFence{SessionID: f.parent.SessionID, Incarnation: f.parent.Incarnation, Lifecycle: f.parent.Lifecycle})
	if err != nil || len(snapshot.Outputs) != 1 || snapshot.Outputs[0].Metrics != nil {
		t.Fatal("замена владельца сохранила старые метрики", snapshot, err)
	}
	stream.close(t)
}
