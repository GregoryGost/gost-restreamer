package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func apiAcceptanceSystem(t *testing.T, f *apiAcceptanceFixture) *core.SystemObserver {
	t.Helper()
	o, err := f.service.OpenSystemObserver(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close(context.Background()) })
	return o
}

func apiAcceptanceGPU(n uint64, at time.Time) core.GPUSnapshot {
	available := core.GPUMetric{Status: core.MetricAvailable, Value: 0, SampledAt: at, LastSuccessAt: at, SamplingPeriodUS: 2000000}
	unavailable := core.GPUMetric{Status: core.MetricUnsupported, Reason: core.ReasonNotSupported, SampledAt: at}
	memory := available
	memory.Value = n
	return core.GPUSnapshot{Status: core.MetricAvailable, SampledAt: at, StaleAfter: time.Minute, Devices: []core.GPUDevice{{UUID: "synthetic-gpu", Utilization: available, MemoryUtilization: available, EncoderUtilization: unavailable, DecoderUtilization: unavailable, MemoryUsedBytes: memory, MemoryTotalBytes: memory, MemoryFreeBytes: memory}}}
}

type apiAcceptanceStreamWriter struct {
	header               http.Header
	mu                   sync.Mutex
	body                 bytes.Buffer
	status               int
	deadline             time.Time
	unsafeWrite          bool
	block                bool
	flushed              chan struct{}
	written              chan struct{}
	changed              chan struct{}
	flushOnce, writeOnce sync.Once
}

func newAPIAcceptanceStreamWriter() *apiAcceptanceStreamWriter {
	return &apiAcceptanceStreamWriter{header: make(http.Header), flushed: make(chan struct{}), written: make(chan struct{}), changed: make(chan struct{}, 1)}
}
func (w *apiAcceptanceStreamWriter) Header() http.Header { return w.header }
func (w *apiAcceptanceStreamWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = status
	}
}
func (w *apiAcceptanceStreamWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadline = deadline
	return nil
}
func (w *apiAcceptanceStreamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	deadline := w.deadline
	w.unsafeWrite = w.unsafeWrite || deadline.IsZero()
	block := w.block
	if w.status == 0 {
		w.status = 200
	}
	w.mu.Unlock()
	w.writeOnce.Do(func() { close(w.written) })
	if block {
		if deadline.IsZero() {
			return 0, errors.New("запись без срока")
		}
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		<-timer.C
		return 0, os.ErrDeadlineExceeded
	}
	w.mu.Lock()
	n, err := w.body.Write(p)
	w.mu.Unlock()
	select {
	case w.changed <- struct{}{}:
	default:
	}
	return n, err
}
func (w *apiAcceptanceStreamWriter) Flush() {
	w.mu.Lock()
	w.unsafeWrite = w.unsafeWrite || w.deadline.IsZero()
	w.mu.Unlock()
	w.flushOnce.Do(func() { close(w.flushed) })
}
func (w *apiAcceptanceStreamWriter) snapshot() (string, int, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String(), w.status, w.unsafeWrite
}

func apiAcceptanceStartStream(t *testing.T, f *apiAcceptanceFixture, path string, w http.ResponseWriter) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.api.ServeHTTP(w, httptest.NewRequest("GET", api.Prefix+path, nil).WithContext(ctx))
	}()
	t.Cleanup(func() { cancel(); apiAcceptanceAwait(t, done) })
	return cancel, done
}
func apiAcceptanceAwait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("поток не завершил этап до срока")
	}
}

func apiAcceptanceAwaitFrame(t *testing.T, w *apiAcceptanceStreamWriter, content string) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		body, _, _ := w.snapshot()
		if strings.Contains(body, content) {
			return
		}
		select {
		case <-w.changed:
		case <-timer.C:
			t.Fatalf("не получен frame %q: %s", content, body)
		}
	}
}

func TestAcceptanceSSEEstablishesDeadlineBeforeInitialFlushAndReleasesClientSlot(t *testing.T) {
	f := newAPIAcceptanceFixture(t, func(c *api.Config) { c.Limits.SSEHeartbeat = time.Minute })
	w := newAPIAcceptanceStreamWriter()
	cancel, done := apiAcceptanceStartStream(t, f, "/events", w)
	apiAcceptanceAwait(t, w.flushed)
	body, status, unsafe := w.snapshot()
	if unsafe || status != 200 || !strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("первый flush не ограничен: status=%d unsafe=%v body=%s", status, unsafe, body)
	}
	apiAcceptanceRequest(t, f.api, "GET", "/events", "", 503)
	cancel()
	apiAcceptanceAwait(t, done)
	w2 := newAPIAcceptanceStreamWriter()
	secondCancel, secondDone := apiAcceptanceStartStream(t, f, "/events", w2)
	apiAcceptanceAwait(t, w2.flushed)
	secondCancel()
	apiAcceptanceAwait(t, secondDone)
	if _, err := f.service.ListStreams(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 1}}); err != nil {
		t.Fatal("SSE закрыл общий Store", err)
	}
}

func TestAcceptanceSSEUnsupportedWriterIsRejectedBeforeHeaders(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	w := apiAcceptanceRequest(t, f.api, "GET", "/events", "", 503)
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatal("недоступный transport начал SSE", w.Header())
	}
}

func TestAcceptanceSSEValidationFailureDoesNotConsumeClientSlot(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	for n := 0; n < 3; n++ {
		w := newAPIAcceptanceStreamWriter()
		f.api.ServeHTTP(w, httptest.NewRequest("GET", api.Prefix+"/events?unexpected=synthetic-rejected-secret", nil))
		body, status, _ := w.snapshot()
		if status != 422 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
			t.Fatalf("ошибка validation начала stream: %d %s", status, body)
		}
		apiAcceptanceNoSecrets(t, body, "synthetic-rejected-secret")
	}
	w := newAPIAcceptanceStreamWriter()
	cancel, done := apiAcceptanceStartStream(t, f, "/events", w)
	apiAcceptanceAwait(t, w.flushed)
	_, status, _ := w.snapshot()
	if status != 200 {
		t.Fatal("отклонённые запросы исчерпали SSEClients", status)
	}
	cancel()
	apiAcceptanceAwait(t, done)
}

func TestAcceptanceSSESlowWriterDoesNotHoldRuntimeAndCloseIsBounded(t *testing.T) {
	f := newAPIAcceptanceFixture(t, func(c *api.Config) { c.Limits.SSEWriteTimeout = 50 * time.Millisecond })
	o := apiAcceptanceSystem(t, f)
	w := newAPIAcceptanceStreamWriter()
	w.block = true
	_, done := apiAcceptanceStartStream(t, f, "/events", w)
	apiAcceptanceAwait(t, w.written)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := o.ObserveGPU(ctx, apiAcceptanceGPU(9007199254740993, time.Now())); err != nil {
		t.Fatal("медленный writer удерживает runtime authority", err)
	}
	if _, err := f.service.RuntimeAnchor(ctx); err != nil {
		t.Fatal("медленный writer блокирует anchor", err)
	}
	apiAcceptanceAwait(t, done)
	if err := f.api.Close(ctx); err != nil {
		t.Fatal("HTTP shutdown не завершён", err)
	}
	if _, _, unsafe := w.snapshot(); unsafe {
		t.Fatal("запись начата до deadline")
	}
	apiAcceptanceRequest(t, f.api, "GET", "/health/ready", "", 503)
	if _, err := f.service.RuntimeAnchor(ctx); err != nil {
		t.Fatal("Close API закрыл общую authority", err)
	}
}

func TestAcceptanceAPICloseCancelsOwnedSSEAndPreservesSharedStore(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	w := newAPIAcceptanceStreamWriter()
	_, done := apiAcceptanceStartStream(t, f, "/events", w)
	apiAcceptanceAwait(t, w.flushed)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := f.api.Close(ctx); err != nil {
		t.Fatal(err)
	}
	apiAcceptanceAwait(t, done)
	apiAcceptanceRequest(t, f.api, "GET", "/events", "", 503)
	if _, err := f.service.RuntimeAnchor(ctx); err != nil {
		t.Fatal("Close API уничтожил общую authority", err)
	}
}

func TestAcceptanceSSEOversizedFrameRequiresResyncWithoutPartialEvent(t *testing.T) {
	f := newAPIAcceptanceFixture(t, func(c *api.Config) { c.Limits.SSEFrameBytes = 256 })
	snapshot := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/events/snapshot", "", 200))
	o := apiAcceptanceSystem(t, f)
	if err := o.ObserveGPU(context.Background(), apiAcceptanceGPU(9007199254740993, time.Now())); err != nil {
		t.Fatal(err)
	}
	w := newAPIAcceptanceStreamWriter()
	_, done := apiAcceptanceStartStream(t, f, "/events?cursor="+url.QueryEscape(snapshot["cursor"].(string)), w)
	apiAcceptanceAwait(t, done)
	body, status, _ := w.snapshot()
	if status != 200 || !strings.Contains(body, "event: resync_required") || !strings.Contains(body, "frame_limit") || strings.Contains(body, "9007199254740993") || strings.Contains(body, "event: gpu") {
		t.Fatal("большой frame опубликован частично", status, body)
	}
}

func TestAcceptanceSSEGapAndEpochResetRequireFreshSnapshot(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(map[bool]string{false: "gap", true: "reset"}[reset], func(t *testing.T) {
			f := newAPIAcceptanceFixture(t, nil)
			snapshot := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/events/snapshot", "", 200))
			cursor, ok := snapshot["cursor"].(string)
			if !ok || cursor == "" {
				t.Fatal("anchor не содержит публичный cursor", snapshot)
			}
			if strings.Contains(cursor, f.store.Incarnation()) {
				t.Fatal("cursor раскрывает внутреннего владельца")
			}
			if reset {
				other := newAPIAcceptanceFixture(t, nil)
				foreign := apiAcceptanceJSON(t, apiAcceptanceRequest(t, other.api, "GET", "/events/snapshot", "", 200))
				cursor = foreign["cursor"].(string)
			} else {
				o := apiAcceptanceSystem(t, f)
				at := time.Now()
				for i := 0; i < 12; i++ {
					if err := o.ObserveGPU(context.Background(), apiAcceptanceGPU(uint64(i), at.Add(time.Duration(i)*time.Nanosecond))); err != nil {
						t.Fatal(err)
					}
				}
			}
			w := newAPIAcceptanceStreamWriter()
			_, done := apiAcceptanceStartStream(t, f, "/events?cursor="+url.QueryEscape(cursor), w)
			apiAcceptanceAwait(t, done)
			body, status, unsafe := w.snapshot()
			if status != 200 || unsafe || !strings.Contains(body, "event: resync_required") || !strings.Contains(body, api.Prefix+"/events/snapshot") || strings.Contains(body, "event: gpu") {
				t.Fatalf("неполный диапазон применён вместо resync: %d %s", status, body)
			}
			apiAcceptanceNoSecrets(t, body, f.store.Incarnation())
		})
	}
}

func TestAcceptanceSnapshotAndEventsKeepUnavailableValuesDistinctFromZero(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	o := apiAcceptanceSystem(t, f)
	at := time.Now().UTC()
	if err := o.ObserveGPU(context.Background(), apiAcceptanceGPU(9007199254740993, at)); err != nil {
		t.Fatal(err)
	}
	w := apiAcceptanceRequest(t, f.api, "GET", "/events/snapshot", "", 200)
	v := apiAcceptanceJSON(t, w)
	gpu := v["gpu"].(map[string]any)
	device := gpu["devices"].([]any)[0].(map[string]any)
	used := device["memory_used_bytes"].(map[string]any)
	zero := device["utilization"].(map[string]any)
	unavailable := device["encoder_utilization"].(map[string]any)
	if used["value"] != "9007199254740993" || zero["value"] != "0" || zero["status"] != "available" || unavailable["value"] != nil || unavailable["status"] != "unsupported" || unavailable["reason"] != "not_supported" {
		t.Fatalf("потеряны точность/доступность: %s", w.Body)
	}
	cursor := v["cursor"].(string)
	if err := o.ObserveGPU(context.Background(), apiAcceptanceGPU(9007199254740994, at.Add(time.Nanosecond))); err != nil {
		t.Fatal(err)
	}
	stream := newAPIAcceptanceStreamWriter()
	cancel, done := apiAcceptanceStartStream(t, f, "/events?cursor="+url.QueryEscape(cursor), stream)
	apiAcceptanceAwaitFrame(t, stream, "9007199254740994")
	cancel()
	apiAcceptanceAwait(t, done)
	body, _, _ := stream.snapshot()
	if !strings.Contains(body, "9007199254740994") {
		t.Fatal("event после anchor потерян", body)
	}
	apiAcceptanceNoSecrets(t, body, f.store.Incarnation())
	conflict := httptest.NewRequest("GET", api.Prefix+"/events?cursor="+url.QueryEscape(cursor), nil)
	conflict.Header.Set("Last-Event-ID", "different-cursor")
	response := httptest.NewRecorder()
	f.api.ServeHTTP(response, conflict)
	if response.Code != 400 {
		t.Fatal("конфликт cursor не отклонён до stream", response.Code, response.Body)
	}
}

func TestAcceptanceHTTPAnchorKeepsCursorAndGPUInOneAuthoritySnapshot(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	o := apiAcceptanceSystem(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type observed struct{ cursor, value string }
	responses := make(chan observed, 64)
	failures := make(chan error, 64)
	start := make(chan struct{})
	var readers sync.WaitGroup
	for n := 0; n < 4; n++ {
		readers.Go(func() {
			<-start
			for range 16 {
				w := httptest.NewRecorder()
				f.api.ServeHTTP(w, httptest.NewRequest("GET", api.Prefix+"/events/snapshot", nil))
				var v struct {
					Cursor string `json:"cursor"`
					GPU    *struct {
						Devices []struct {
							Memory struct {
								Value *string `json:"value"`
							} `json:"memory_used_bytes"`
						} `json:"devices"`
					} `json:"gpu"`
				}
				if w.Code != 200 {
					failures <- errors.New("HTTP anchor недоступен")
					return
				}
				if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
					failures <- err
					return
				}
				value := ""
				if v.GPU != nil && len(v.GPU.Devices) > 0 && v.GPU.Devices[0].Memory.Value != nil {
					value = *v.GPU.Devices[0].Memory.Value
				}
				responses <- observed{v.Cursor, value}
			}
		})
	}
	states := map[string]string{"0": ""}
	at := time.Now()
	close(start)
	for n := 0; n < 16; n++ {
		value := uint64(9007199254740993 + n*17)
		if err := o.ObserveGPU(ctx, apiAcceptanceGPU(value, at.Add(time.Duration(n)*time.Nanosecond))); err != nil {
			t.Fatal(err)
		}
		anchor, err := f.service.RuntimeAnchor(ctx)
		if err != nil {
			t.Fatal(err)
		}
		states[strconv.FormatUint(anchor.Cursor.Sequence, 10)] = strconv.FormatUint(value, 10)
	}
	readers.Wait()
	close(responses)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	for result := range responses {
		raw, err := base64.RawURLEncoding.DecodeString(result.cursor)
		if err != nil {
			t.Fatal(err)
		}
		var cursor struct {
			Sequence string `json:"sequence"`
		}
		if err = json.Unmarshal(raw, &cursor); err != nil {
			t.Fatal(err)
		}
		expected, ok := states[cursor.Sequence]
		if !ok || expected != result.value {
			t.Fatalf("HTTP anchor смешал разные состояния authority: sequence=%s value=%s expected=%s", cursor.Sequence, result.value, expected)
		}
	}
}

func TestAcceptanceReadinessAndCapabilitiesUseActualExecutableAvailability(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "media-tool")
	marker := filepath.Join(root, "must-not-execute")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "missing")
	f := newAPIAcceptanceFixture(t, func(c *api.Config) {
		c.Capabilities = api.ExecutableCapabilities{FFmpeg: executable, FFprobe: missing, Helper: ""}
	})
	w := apiAcceptanceRequest(t, f.api, "GET", "/system/capabilities", "", 200)
	v := apiAcceptanceJSON(t, w)
	if v["ffmpeg"].(map[string]any)["status"] != "available" || v["ffprobe"].(map[string]any)["status"] != "unavailable" || v["helper"].(map[string]any)["status"] != "unavailable" || v["nvidia"].(map[string]any)["status"] != "unverified" {
		t.Fatal("capabilities подменяет проверенные факты", v)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("проверка capabilities запустила executable", err)
	}
	apiAcceptanceNoSecrets(t, w.Body.String(), root)
	apiAcceptanceRequest(t, f.api, "GET", "/health/live", "", 200)
	apiAcceptanceRequest(t, f.api, "GET", "/health/ready", "", 200)
	if err := os.Chmod(executable, 0600); err != nil {
		t.Fatal(err)
	}
	w = apiAcceptanceRequest(t, f.api, "GET", "/system/capabilities", "", 200)
	if apiAcceptanceJSON(t, w)["ffmpeg"].(map[string]any)["status"] != "unavailable" {
		t.Fatal("provider выдаёт устаревшую доступность executable")
	}
	apiAcceptanceRequest(t, f.api, "GET", "/health/ready", "", 200)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	apiAcceptanceRequest(t, f.api, "GET", "/health/live", "", 200)
	apiAcceptanceRequest(t, f.api, "GET", "/health/ready", "", 503)
}

func TestAcceptanceRuntimeCursorCannotEchoUserInput(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	for _, cursor := range []string{"synthetic-rejected-secret", strings.Repeat("A", 2048)} {
		w := apiAcceptanceRequest(t, f.api, "GET", "/events?cursor="+url.QueryEscape(cursor), "", 400)
		var problem map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
			t.Fatal(err)
		}
		if problem["errors"] != nil || strings.Contains(w.Body.String(), cursor) {
			t.Fatal("cursor попал в Problem Details")
		}
	}
	snapshot := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/events/snapshot", "", 200))
	encoded := snapshot["cursor"].(string)
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), f.store.Incarnation()) {
		t.Fatal("декодированный cursor раскрывает process incarnation")
	}
	var cursor map[string]any
	if err = json.Unmarshal(raw, &cursor); err != nil {
		t.Fatal(err)
	}
	cursor["sequence"] = "18446744073709551615"
	raw, err = json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	apiAcceptanceRequest(t, f.api, "GET", "/events?cursor="+base64.RawURLEncoding.EncodeToString(raw), "", 400)
}
