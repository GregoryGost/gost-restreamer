package api_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

type apiAcceptanceUploadBody struct {
	reader io.Reader
	reads  atomic.Int32
	closed chan struct{}
	once   sync.Once
}

func (b *apiAcceptanceUploadBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	return b.reader.Read(p)
}
func (b *apiAcceptanceUploadBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

type apiAcceptanceUploadInspection struct {
	limits  fallback.ProbeLimits
	blocked atomic.Bool
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (i *apiAcceptanceUploadInspection) Source(ctx context.Context, f *os.File, kind core.AssetKind) (fallback.Source, error) {
	if i.blocked.Load() {
		i.once.Do(func() { close(i.entered) })
		select {
		case <-i.release:
			return fallback.Source{}, errors.New("synthetic-url-secret: source inspection failed")
		case <-ctx.Done():
			return fallback.Source{}, ctx.Err()
		}
	}
	return (fallback.Prober{Limits: i.limits}).Source(ctx, f, kind)
}
func (*apiAcceptanceUploadInspection) Verify(_ context.Context, _ *os.File, p core.GroupProfile) (core.MediaFormat, error) {
	return p.Format, nil
}

type apiAcceptanceUploadRunner struct{}

func (apiAcceptanceUploadRunner) Run(_ context.Context, _ fallback.Command, _ *os.File, w io.Writer) (fallback.RunResult, error) {
	_, err := w.Write([]byte("verified-prepared-file"))
	return fallback.RunResult{}, err
}

type apiAcceptancePreparation struct {
	manager *fallback.Manager
	jobs    chan *fallback.Job
}

func (p *apiAcceptancePreparation) Upload(ctx context.Context, r core.PreparationRequest, body io.ReadCloser) (*fallback.Job, error) {
	j, err := p.manager.Upload(ctx, r, body)
	if err == nil {
		p.jobs <- j
	}
	return j, err
}
func (p *apiAcceptancePreparation) Rebuild(ctx context.Context, r core.PreparationRequest) (*fallback.Job, error) {
	return p.manager.Rebuild(ctx, r)
}

func newAPIAcceptanceUpload(t *testing.T, readTimeout time.Duration) (*apiAcceptanceFixture, *apiAcceptancePreparation, *apiAcceptanceUploadInspection, func()) {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	t.Cleanup(finish)
	var preparation *apiAcceptancePreparation
	var inspect *apiAcceptanceUploadInspection
	f := newAPIAcceptanceFixture(t, func(c *api.Config) {
		c.Limits.UploadReadTimeout = readTimeout
		files, err := fallback.OpenFileStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := files.Close(); err != nil {
				t.Error(err)
			}
		})
		config := fallback.Config{ReconcileLimit: 16, GeneratorVersion: "controlled-generator", FFmpeg: "controlled", FFprobe: "controlled", MaxUploadBytes: 1024, MaxOutputBytes: 1024, PrepareTimeout: 5 * time.Second, ShutdownTimeout: time.Second, QueueInterval: time.Millisecond, Runner: fallback.RunnerLimits{Timeout: time.Second, MaxArgs: 128, MaxCommandBytes: 4096, MaxDiagnosticBytes: 4096}, Probe: fallback.ProbeLimits{MaxJSONBytes: 4096, MaxWidth: 4096, MaxHeight: 4096, MaxPixels: 4096 * 4096, MaxDuration: time.Minute, ImageDuration: time.Second, MaxPackets: 1000, Container: fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 1 << 16, MaxMdatBytes: 1 << 20, MaxSampleBytes: 1 << 18, MaxExtradataBytes: 1 << 12, MaxSamples: 100}}}
		inspect = &apiAcceptanceUploadInspection{limits: config.Probe, entered: make(chan struct{}), release: release}
		manager, err := fallback.NewManagerWithTools(c.Core, files, config, fallback.Tools{Builder: func(fallback.GenerateRequest) (fallback.Command, error) {
			return fallback.Command{Binary: "controlled"}, nil
		}, Inspector: inspect, Runner: apiAcceptanceUploadRunner{}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = manager.Close(ctx)
		})
		preparation = &apiAcceptancePreparation{manager: manager, jobs: make(chan *fallback.Job, 8)}
		c.Fallback = preparation
	})
	return f, preparation, inspect, finish
}

func apiAcceptancePNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func apiAcceptanceUploadRequest(t *testing.T, f *apiAcceptanceFixture, ctx context.Context, id string, revision uint64, body io.ReadCloser, status int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("PUT", api.Prefix+"/fallback-assets/"+id+"/content?kind=image&expected_revision="+strconv.FormatUint(revision, 10), body).WithContext(ctx)
	r.ContentLength = -1
	r.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()
	f.api.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("upload: status=%d want=%d body=%s", w.Code, status, w.Body)
	}
	apiAcceptanceNoSecrets(t, w.Body.String())
	return w
}

func TestAcceptanceHTTPUploadOwnsTransferAndPreservesCurrentAfterIndependentFailure(t *testing.T) {
	f, p, inspect, release := newAPIAcceptanceUpload(t, time.Second)
	defer release()
	asset := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/fallback-assets", `{"name":"Фоновая заглушка"}`, 201))
	id := asset["id"].(string)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	firstBody := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	firstResponse := apiAcceptanceUploadRequest(t, f, ctx, id, 1, firstBody, 202)
	apiAcceptanceAwait(t, firstBody.closed)
	first := <-p.jobs
	result, err := first.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	firstID := string(result.Generation.ID)
	if firstResponse.Header().Get("Location") != api.Prefix+"/fallback-assets/"+id+"/generations/"+firstID {
		t.Fatal("Location не указывает точное поколение")
	}
	current, err := f.service.GetAsset(ctx, core.ID(id))
	if err != nil {
		t.Fatal(err)
	}
	inspect.blocked.Store(true)
	requestCtx, requestCancel := context.WithCancel(context.Background())
	defer requestCancel()
	body := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	response := apiAcceptanceUploadRequest(t, f, requestCtx, id, current.Revision, body, 202)
	apiAcceptanceAwait(t, body.closed)
	job := <-p.jobs
	apiAcceptanceAwait(t, inspect.entered)
	requestCancel()
	select {
	case <-job.Done():
		t.Fatal("запрос отменил подготовку после передачи")
	default:
	}
	accepted := apiAcceptanceJSON(t, response)
	generationPath := "/fallback-assets/" + id + "/generations/" + accepted["generation_id"].(string)
	preparing := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", generationPath, "", 200))
	if preparing["status"] != "preparing" {
		t.Fatal("результат передачи ошибочно означает готовый комплект", preparing)
	}
	current, err = f.service.GetAsset(ctx, core.ID(id))
	if err != nil || string(current.CurrentGenerationID) != firstID || current.PendingGenerationID != job.Generation().ID || current.State != core.AssetReady {
		t.Fatal("pending изменил прежний current", current, err)
	}
	apiAcceptanceRequest(t, f.api, "DELETE", "/fallback-assets/"+id+"?expected_revision="+strconv.FormatUint(current.Revision, 10), "", 409)
	apiAcceptanceRequest(t, f.api, "POST", "/fallback-assets/"+id+"/rebuild", `{"expected_revision":"`+strconv.FormatUint(current.Revision, 10)+`"}`, 409)
	stale := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	apiAcceptanceUploadRequest(t, f, ctx, id, current.Revision, stale, 409)
	if stale.reads.Load() != 0 {
		t.Fatal("отклонённая загрузка читает тело до admission")
	}
	release()
	if _, err = job.Wait(ctx); err == nil {
		t.Fatal("ошибка подготовки не сохранена")
	}
	failed := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", generationPath, "", 200))
	if failed["status"] != "error" || failed["failure"] == nil {
		t.Fatal("точный результат пропал после очистки pending", failed)
	}
	current, err = f.service.GetAsset(ctx, core.ID(id))
	if err != nil || string(current.CurrentGenerationID) != firstID || current.PendingGenerationID != "" || current.State != core.AssetReady {
		t.Fatal("неудачная замена испортила current", current, err)
	}
	apiAcceptanceRequest(t, f.api, "GET", "/fallback-assets/"+id+"/generations/"+firstID, "", 200)
	foreign := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/fallback-assets", `{"name":"Другой Asset"}`, 201))["id"].(string)
	apiAcceptanceRequest(t, f.api, "GET", "/fallback-assets/"+foreign+"/generations/"+firstID, "", 404)
	apiAcceptanceRequest(t, f.api, "GET", "/fallback-assets/"+foreign+"/variants?generation_id="+firstID, "", 404)
}

type apiAcceptanceBlockedUpload struct {
	closed chan struct{}
	once   sync.Once
}

func (b *apiAcceptanceBlockedUpload) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *apiAcceptanceBlockedUpload) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

func TestAcceptanceHTTPUploadLimitsTimeoutAndMediaType(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		status int
	}{{"empty", "", 422}, {"oversized", strings.Repeat("x", 1025), 413}} {
		t.Run(tc.name, func(t *testing.T) {
			f, p, _, _ := newAPIAcceptanceUpload(t, time.Second)
			asset, err := f.service.CreateAsset(context.Background(), "Файл")
			if err != nil {
				t.Fatal(err)
			}
			body := &apiAcceptanceUploadBody{reader: strings.NewReader(tc.body), closed: make(chan struct{})}
			apiAcceptanceUploadRequest(t, f, context.Background(), string(asset.ID), asset.Revision, body, tc.status)
			apiAcceptanceAwait(t, body.closed)
			job := <-p.jobs
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err = job.Wait(ctx); err == nil {
				t.Fatal("некорректная передача опубликована")
			}
		})
	}
	f, p, _, _ := newAPIAcceptanceUpload(t, 50*time.Millisecond)
	asset, err := f.service.CreateAsset(context.Background(), "Медленный файл")
	if err != nil {
		t.Fatal(err)
	}
	body := &apiAcceptanceBlockedUpload{closed: make(chan struct{})}
	defer func() { _ = body.Close() }()
	apiAcceptanceUploadRequest(t, f, context.Background(), string(asset.ID), asset.Revision, body, 408)
	apiAcceptanceAwait(t, body.closed)
	job := <-p.jobs
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err = job.Wait(ctx); err == nil {
		t.Fatal("таймаут передачи опубликовал файл")
	}
	unread := &apiAcceptanceUploadBody{reader: strings.NewReader("payload"), closed: make(chan struct{})}
	r := httptest.NewRequest("PUT", api.Prefix+"/fallback-assets/"+string(asset.ID)+"/content?kind=image&expected_revision=1", unread)
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	f.api.ServeHTTP(w, r)
	if w.Code != 415 || unread.reads.Load() != 0 {
		t.Fatal("тип содержимого проверяется после чтения", w.Code, unread.reads.Load())
	}
}

func TestAcceptanceHTTPUploadQueueRejectsBeforeReadAndCancellationClosesQueuedBody(t *testing.T) {
	f, p, inspect, release := newAPIAcceptanceUpload(t, time.Second)
	defer release()
	inspect.blocked.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	assets := make([]core.FallbackAsset, 4)
	for n := range assets {
		a, err := f.service.CreateAsset(ctx, "Очередь")
		if err != nil {
			t.Fatal(err)
		}
		assets[n] = a
	}
	firstBody := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	apiAcceptanceUploadRequest(t, f, ctx, string(assets[0].ID), assets[0].Revision, firstBody, 202)
	first := <-p.jobs
	apiAcceptanceAwait(t, inspect.entered)
	var queued []*fallback.Job
	for _, a := range assets[1:3] {
		body := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
		j, err := p.Upload(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage}, body)
		if err != nil {
			t.Fatal(err)
		}
		queued = append(queued, j)
	}
	unread := &apiAcceptanceUploadBody{reader: strings.NewReader("not consumed"), closed: make(chan struct{})}
	apiAcceptanceUploadRequest(t, f, ctx, string(assets[3].ID), assets[3].Revision, unread, 503)
	if unread.reads.Load() != 0 {
		t.Fatal("переполненная очередь читает тело до admission")
	}
	for _, j := range queued {
		cancelled, stop := context.WithCancel(ctx)
		stop()
		if err := j.WaitUpload(cancelled); !errors.Is(err, context.Canceled) {
			t.Fatal("queued transfer не отменён", err)
		}
		if _, err := j.Wait(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal("queued job не завершён", err)
		}
	}
	// Освободившийся бюджет проверяется через настоящий HTTP и reader, ещё ожидающий claim.
	queuedBody := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	requestCtx, requestCancel := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest("PUT", api.Prefix+"/fallback-assets/"+string(assets[3].ID)+"/content?kind=image&expected_revision=1", queuedBody).WithContext(requestCtx)
		r.ContentLength = -1
		r.Header.Set("Content-Type", "application/octet-stream")
		w := httptest.NewRecorder()
		f.api.ServeHTTP(w, r)
		done <- w
	}()
	defer requestCancel()
	// Первые два задания уже получены напрямую, но также сохранены прозрачным портом.
	<-p.jobs
	<-p.jobs
	var pending *fallback.Job
	select {
	case pending = <-p.jobs:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	requestCancel()
	select {
	case w := <-done:
		if w.Code != 408 {
			t.Fatal("отмена передачи не дала безопасный timeout", w.Code, w.Body)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	apiAcceptanceAwait(t, queuedBody.closed)
	if queuedBody.reads.Load() != 0 {
		t.Fatal("queued body прочитан до claim")
	}
	if _, err := pending.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("отменённое HTTP-задание осталось в очереди", err)
	}
	release()
	if _, err := first.Wait(ctx); err == nil {
		t.Fatal("управляемая ошибка инспекции потеряна")
	}
}

func TestAcceptanceKnownSessionWithoutRAMIsDifferentFromUnknown(t *testing.T) {
	f, p, _, release := newAPIAcceptanceUpload(t, time.Second)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	asset, err := f.service.CreateAsset(ctx, "Готовая заглушка")
	if err != nil {
		t.Fatal(err)
	}
	body := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	apiAcceptanceUploadRequest(t, f, ctx, string(asset.ID), asset.Revision, body, 202)
	if _, err = (<-p.jobs).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	stream, err := f.service.CreateStream(ctx, core.Stream{Name: "Ещё без наблюдений", PublicationKey: "synthetic-publication-secret", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	accepted := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/streams/"+string(stream.ID)+"/start", `{"request_id":"unobserved-start","destination_ids":[]}`, 202))
	id := accepted["session_id"].(string)
	unobserved := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/sessions/"+id+"/metrics", "", 200))
	if unobserved["availability"] != "unavailable" || unobserved["snapshot"] != nil || unobserved["reason"] != "not_observed" {
		t.Fatal("известная сессия без RAM подменена метриками или ошибкой", unobserved)
	}
	apiAcceptanceRequest(t, f.api, "GET", "/sessions/unknown/metrics", "", 404)
	apiAcceptanceRequest(t, f.api, "GET", "/commands/"+accepted["id"].(string), "", 200)
}
