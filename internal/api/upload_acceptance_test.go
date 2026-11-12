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

// apiAcceptanceUploadBody учитывает чтения и однократное закрытие управляемого тела HTTP-загрузки.
type apiAcceptanceUploadBody struct {
	// reader предоставляет исходные байты загрузки.
	reader io.Reader
	// reads атомарно считает обращения к телу для проверки отказа до потребления.
	reads atomic.Int32
	// closed подтверждает закрытие тела после передачи или отмены.
	closed chan struct{}
	// once обеспечивает однократное закрытие closed.
	once sync.Once
}

// Read учитывает обращения HTTP-загрузки к телу и передаёт чтение настроенному источнику байтов.
func (b *apiAcceptanceUploadBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	return b.reader.Read(p)
}

// Close однократно подтверждает закрытие тела загрузки без владения дополнительными ресурсами.
func (b *apiAcceptanceUploadBody) Close() error {
	b.once.Do(func() {
		// Однократно сообщает о закрытии тела загрузки.
		close(b.closed)
	})
	return nil
}

// apiAcceptanceUploadInspection выполняет настоящий PNG-анализ либо управляемо задерживает подготовку
// после HTTP-передачи.
type apiAcceptanceUploadInspection struct {
	// limits задаёт реальные пределы анализа PNG для незаблокированного сценария.
	limits fallback.ProbeLimits
	// blocked атомарно включает задержанную проверку источника.
	blocked atomic.Bool
	// entered сообщает о начале удерживаемой проверки исходника.
	entered chan struct{}
	// release разрешает удерживаемой проверке вернуть управляемую ошибку.
	release <-chan struct{}
	// once обеспечивает единственное уведомление entered.
	once sync.Once
}

// Source выполняет настоящую проверку PNG либо удерживает инспекцию до разрешения теста или отмены
// подготовки, возвращая управляемый отказ.
func (i *apiAcceptanceUploadInspection) Source(ctx context.Context, f *os.File, kind core.AssetKind) (fallback.Source, error) {
	if i.blocked.Load() {
		i.once.Do(func() {
			// Однократно сообщает о входе в заблокированную проверку исходного файла.
			close(i.entered)
		})
		select {
		case <-i.release:
			return fallback.Source{}, errors.New("synthetic-url-secret: source inspection failed")
		case <-ctx.Done():
			return fallback.Source{}, ctx.Err()
		}
	}
	return (fallback.Prober{Limits: i.limits}).Source(ctx, f, kind)
}

// Verify возвращает ожидаемый формат тестового подготовленного файла без запуска медиапроверки.
func (*apiAcceptanceUploadInspection) Verify(_ context.Context, _ *os.File, p core.GroupProfile) (core.MediaFormat, error) {
	return p.Format, nil
}

// apiAcceptanceUploadRunner имитирует генерацию подготовленного файла записью известных байтов без
// FFmpeg.
type apiAcceptanceUploadRunner struct{}

// Run записывает известные байты подготовленного файла в предоставленный поток вывода без запуска
// FFmpeg.
func (apiAcceptanceUploadRunner) Run(_ context.Context, _ fallback.Command, _ *os.File, w io.Writer) (fallback.RunResult, error) {
	// err содержит отказ записи известных байтов подготовленного файла.
	_, err := w.Write([]byte("verified-prepared-file"))
	return fallback.RunResult{}, err
}

// apiAcceptancePreparation прозрачно передаёт операции настоящему менеджеру и показывает принятые
// задания тесту.
type apiAcceptancePreparation struct {
	// manager содержит настоящего владельца загрузки и подготовки.
	manager *fallback.Manager
	// jobs передаёт тесту успешно принятые задания без изменения результата Upload.
	jobs chan *fallback.Job
}

// Upload передаёт тело настоящему FallbackManager и сохраняет успешно принятое задание в наблюдаемой
// тестом очереди.
func (p *apiAcceptancePreparation) Upload(ctx context.Context, r core.PreparationRequest, body io.ReadCloser) (*fallback.Job, error) {
	// j содержит задание настоящего менеджера; err содержит отказ его допуска.
	j, err := p.manager.Upload(ctx, r, body)
	if err == nil {
		p.jobs <- j
	}
	return j, err
}

// Rebuild передаёт запрос повторной подготовки настоящему FallbackManager без изменения его контекста
// и результата.
func (p *apiAcceptancePreparation) Rebuild(ctx context.Context, r core.PreparationRequest) (*fallback.Job, error) {
	return p.manager.Rebuild(ctx, r)
}

// newAPIAcceptanceUpload связывает настоящий API и FallbackManager с управляемыми генератором и
// инспекцией. Возвращённая функция снимает блокировку инспекции; владельцы ресурсов закрываются
// тестовой очисткой.
func newAPIAcceptanceUpload(t *testing.T, readTimeout time.Duration) (*apiAcceptanceFixture, *apiAcceptancePreparation, *apiAcceptanceUploadInspection, func()) {
	t.Helper()
	// release удерживает инспекцию исходника до явного завершения тестового сценария.
	release := make(chan struct{})
	// once обеспечивает однократное освобождение инспекции при очистке и явном вызове.
	var once sync.Once
	// finish снимает блокировку инспекции безопасно для повторных вызовов.
	finish := func() {
		// Освобождает заблокированную проверку файла только при первом вызове.

		once.Do(func() {
			// Закрывает канал, разрешающий продолжить проверку исходного файла.
			close(release)
		})
	}
	t.Cleanup(finish)
	// preparation сохраняет прозрачный порт настоящего менеджера, собираемого внутри настройки API.
	var preparation *apiAcceptancePreparation
	// inspect сохраняет управляемый анализатор, которым тест меняет последующую подготовку.
	var inspect *apiAcceptanceUploadInspection
	// f содержит API и временную SQLite с настоящим менеджером подготовки и заданным сроком чтения.
	f := newAPIAcceptanceFixture(t, func(c *api.Config) {
		// Собирает управляемую подготовку заглушек с заданным сроком чтения HTTP и тестовыми средствами
		// обработки файла.

		c.Limits.UploadReadTimeout = readTimeout
		// files содержит временный FileStore; err содержит отказ открытия корня файлов.
		files, err := fallback.OpenFileStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// Закрывает временное файловое хранилище после остановки подготовки.

			// err содержит результат закрытия FileStore после завершения подготовки.
			if err := files.Close(); err != nil {
				t.Error(err)
			}
		})
		// config задаёт конечные размеры, сроки и версию управляемого генератора тестовой
		// подготовки.
		config := fallback.Config{ReconcileLimit: 16, GeneratorVersion: "controlled-generator", FFmpeg: "controlled", FFprobe: "controlled", MaxUploadBytes: 1024, MaxOutputBytes: 1024, PrepareTimeout: 5 * time.Second, ShutdownTimeout: time.Second, QueueInterval: time.Millisecond, Runner: fallback.RunnerLimits{Timeout: time.Second, MaxArgs: 128, MaxCommandBytes: 4096, MaxDiagnosticBytes: 4096}, Probe: fallback.ProbeLimits{MaxJSONBytes: 4096, MaxWidth: 4096, MaxHeight: 4096, MaxPixels: 4096 * 4096, MaxDuration: time.Minute, ImageDuration: time.Second, MaxPackets: 1000, Container: fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 1 << 16, MaxMdatBytes: 1 << 20, MaxSampleBytes: 1 << 18, MaxExtradataBytes: 1 << 12, MaxSamples: 100}}}
		inspect = &apiAcceptanceUploadInspection{limits: config.Probe, entered: make(chan struct{}), release: release}
		// manager содержит настоящий FallbackManager с подставленными генерацией и инспекцией.
		manager, err := fallback.NewManagerWithTools(c.Core, files, config, fallback.Tools{Builder: func(fallback.GenerateRequest) (fallback.Command, error) {
			// Возвращает управляемую команду для тестового исполнителя без запуска FFmpeg.

			return fallback.Command{Binary: "controlled"}, nil
		}, Inspector: inspect, Runner: apiAcceptanceUploadRunner{}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// Останавливает менеджер подготовки с отдельным ограниченным контекстом.

			// ctx ограничивает очистку менеджера двумя секундами; cancel освобождает таймер.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = manager.Close(ctx)
		})
		preparation = &apiAcceptancePreparation{manager: manager, jobs: make(chan *fallback.Job, 8)}
		c.Fallback = preparation
	})
	return f, preparation, inspect, finish
}

// apiAcceptancePNG создаёт в памяти корректное небольшое изображение PNG для потоковой передачи.
func apiAcceptancePNG(t *testing.T) []byte {
	t.Helper()
	// b собирает корректный PNG небольшого изображения в памяти.
	var b bytes.Buffer
	// err содержит отказ кодирования PNG-фикстуры.
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// apiAcceptanceUploadRequest передаёт тело неизвестной длины через настоящий PUT загрузки, проверяет
// статус и отсутствие запрещённых данных в ответе.
func apiAcceptanceUploadRequest(t *testing.T, f *apiAcceptanceFixture, ctx context.Context, id string, revision uint64, body io.ReadCloser, status int) *httptest.ResponseRecorder {
	t.Helper()
	// r содержит настоящий PUT загрузки с видом image, точной ревизией и неизвестной длиной тела.
	r := httptest.NewRequest("PUT", api.Prefix+"/fallback-assets/"+id+"/content?kind=image&expected_revision="+strconv.FormatUint(revision, 10), body).WithContext(ctx)
	r.ContentLength = -1
	r.Header.Set("Content-Type", "application/octet-stream")
	// w сохраняет ответ потоковой загрузки для проверки статуса и безопасных полей.
	w := httptest.NewRecorder()
	f.api.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("upload: status=%d want=%d body=%s", w.Code, status, w.Body)
	}
	apiAcceptanceNoSecrets(t, w.Body.String())
	return w
}

// Проверяет завершение HTTP-передачи до 202, независимость дальнейшей подготовки от клиента и
// сохранение прежнего текущего поколения при отказе замены.
func TestAcceptanceHTTPUploadOwnsTransferAndPreservesCurrentAfterIndependentFailure(t *testing.T) {
	// f содержит API; p показывает принятые задания; inspect управляет проверкой источника; release
	// снимает её блокировку.
	f, p, inspect, release := newAPIAcceptanceUpload(t, time.Second)
	defer release()
	// asset содержит ответ создания заглушки для двух последовательных поколений.
	asset := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/fallback-assets", `{"name":"Фоновая заглушка"}`, 201))
	// id содержит идентификатор заглушки, чей текущий комплект должен сохраниться при отказе замены.
	id := asset["id"].(string)
	// ctx ограничивает ожидания поколений тремя секундами; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// firstBody содержит корректный PNG первого успешного поколения и сигнал закрытия передачи.
	firstBody := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	// firstResponse сохраняет 202 и Location первого точного поколения.
	firstResponse := apiAcceptanceUploadRequest(t, f, ctx, id, 1, firstBody, 202)
	apiAcceptanceAwait(t, firstBody.closed)
	// first содержит первое принятое задание подготовки, от которого тест ожидает успешной
	// публикации текущего поколения заглушки.
	first := <-p.jobs
	// result содержит итог первого задания; err содержит отказ подготовки первого поколения.
	result, err := first.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// firstID сохраняет идентификатор первого рабочего поколения для сравнения после отказа замены.
	firstID := string(result.Generation.ID)
	if firstResponse.Header().Get("Location") != api.Prefix+"/fallback-assets/"+id+"/generations/"+firstID {
		t.Fatal("Location не указывает точное поколение")
	}
	// current содержит актуальную заглушку и её ревизию между этапами подготовки.
	current, err := f.service.GetAsset(ctx, core.ID(id))
	if err != nil {
		t.Fatal(err)
	}
	inspect.blocked.Store(true)
	// requestCtx управляет только второй HTTP-передачей; requestCancel отменяет клиента после
	// подтверждения закрытия тела.
	requestCtx, requestCancel := context.WithCancel(context.Background())
	defer requestCancel()
	// body содержит PNG второй передачи, после которой инспекция намеренно остаётся заблокированной.
	body := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	// response сохраняет 202 второй передачи до завершения фоновой подготовки.
	response := apiAcceptanceUploadRequest(t, f, requestCtx, id, current.Revision, body, 202)
	apiAcceptanceAwait(t, body.closed)
	// job содержит принятое задание замены, которое должно пережить отмену уже завершённой
	// HTTP-передачи.
	job := <-p.jobs
	apiAcceptanceAwait(t, inspect.entered)
	requestCancel()
	select {
	case <-job.Done():
		t.Fatal("запрос отменил подготовку после передачи")
	default:
	}
	// accepted содержит идентификаторы принятой второй подготовки.
	accepted := apiAcceptanceJSON(t, response)
	// generationPath адресует второе поколение по принятому generation_id, даже пока текущим
	// остаётся первое поколение.
	generationPath := "/fallback-assets/" + id + "/generations/" + accepted["generation_id"].(string)
	// preparing содержит публичное состояние ещё выполняющейся второй подготовки.
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
	// stale содержит тело дополнительной отвергаемой загрузки, которое не должно читаться при отказе
	// в приёме задания.
	stale := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	apiAcceptanceUploadRequest(t, f, ctx, id, current.Revision, stale, 409)
	if stale.reads.Load() != 0 {
		t.Fatal("отклонённая загрузка читает тело до admission")
	}
	release()
	if _, err = job.Wait(ctx); err == nil {
		t.Fatal("ошибка подготовки не сохранена")
	}
	// failed содержит сохранённый безопасный результат отказа второго поколения.
	failed := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", generationPath, "", 200))
	if failed["status"] != "error" || failed["failure"] == nil {
		t.Fatal("точный результат пропал после очистки pending", failed)
	}
	current, err = f.service.GetAsset(ctx, core.ID(id))
	if err != nil || string(current.CurrentGenerationID) != firstID || current.PendingGenerationID != "" || current.State != core.AssetReady {
		t.Fatal("неудачная замена испортила current", current, err)
	}
	apiAcceptanceRequest(t, f.api, "GET", "/fallback-assets/"+id+"/generations/"+firstID, "", 200)
	// foreign содержит ID другой заглушки для проверки принадлежности поколений и вариантов.
	foreign := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/fallback-assets", `{"name":"Другой Asset"}`, 201))["id"].(string)
	apiAcceptanceRequest(t, f.api, "GET", "/fallback-assets/"+foreign+"/generations/"+firstID, "", 404)
	apiAcceptanceRequest(t, f.api, "GET", "/fallback-assets/"+foreign+"/variants?generation_id="+firstID, "", 404)
}

// apiAcceptanceBlockedUpload удерживает чтение тела до Close для проверки конечного срока передачи.
type apiAcceptanceBlockedUpload struct {
	// closed освобождает заблокированный Read и подтверждает закрытие тела.
	closed chan struct{}
	// once обеспечивает однократное закрытие канала отмены тела.
	once sync.Once
}

// Read удерживает передачу до закрытия тела и затем возвращает ошибку закрытого канала.
func (b *apiAcceptanceBlockedUpload) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.ErrClosedPipe
}

// Close однократно снимает блокировку чтения тела, моделируя отмену зависшей передачи.
func (b *apiAcceptanceBlockedUpload) Close() error {
	b.once.Do(func() {
		// Однократно закрывает канал, прерывающий заблокированное чтение тела.
		close(b.closed)
	})
	return nil
}

// Проверяет отклонение пустого и слишком большого тела, конечный срок чтения и проверку Content-Type
// до потребления загружаемых байтов.
func TestAcceptanceHTTPUploadLimitsTimeoutAndMediaType(t *testing.T) {
	// tc задаёт пустое либо превышающее бюджет тело и ожидаемый отказ HTTP.
	for _, tc := range []struct {
		// name задаёт название ошибки размера передачи.
		name string
		// body содержит исходные байты пустой или слишком большой загрузки.
		body string
		// status задаёт ожидаемый HTTP-отказ 422 либо 413.
		status int
	}{{"empty", "", 422}, {"oversized", strings.Repeat("x", 1025), 413}} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет отказ передачи пустого либо слишком большого файла и завершение задания с ошибкой.

			// f содержит API текущего случая размера; p передаёт принятое задание для
			// проверки его неуспеха.
			f, p, _, _ := newAPIAcceptanceUpload(t, time.Second)
			// asset содержит заглушку текущего случая; err содержит отказ создания.
			asset, err := f.service.CreateAsset(context.Background(), "Файл")
			if err != nil {
				t.Fatal(err)
			}
			// body содержит управляемый источник текущих ошибочных байтов загрузки.
			body := &apiAcceptanceUploadBody{reader: strings.NewReader(tc.body), closed: make(chan struct{})}
			apiAcceptanceUploadRequest(t, f, context.Background(), string(asset.ID), asset.Revision, body, tc.status)
			apiAcceptanceAwait(t, body.closed)
			// job содержит задание ошибочной передачи для проверки отсутствия публикации.
			job := <-p.jobs
			// ctx ограничивает ожидание отказа задания тремя секундами; cancel освобождает
			// таймер.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err = job.Wait(ctx); err == nil {
				t.Fatal("некорректная передача опубликована")
			}
		})
	}
	// f содержит API короткого срока передачи; p показывает задание задержанного тела.
	f, p, _, _ := newAPIAcceptanceUpload(t, 50*time.Millisecond)
	// asset содержит заглушку медленной загрузки; err содержит отказ создания.
	asset, err := f.service.CreateAsset(context.Background(), "Медленный файл")
	if err != nil {
		t.Fatal(err)
	}
	// body блокирует передачу до закрытия со стороны владельца при тайм-ауте.
	body := &apiAcceptanceBlockedUpload{closed: make(chan struct{})}
	defer func() {
		// Освобождает заблокированное тело после проверки срока передачи файла.
		_ = body.Close()
	}()
	apiAcceptanceUploadRequest(t, f, context.Background(), string(asset.ID), asset.Revision, body, 408)
	apiAcceptanceAwait(t, body.closed)
	// job содержит принятое задание передачи, завершившейся HTTP 408.
	job := <-p.jobs
	// ctx ограничивает ожидание неуспеха медленного задания; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err = job.Wait(ctx); err == nil {
		t.Fatal("таймаут передачи опубликовал файл")
	}
	// unread содержит тело с неверным Content-Type для проверки отсутствия чтений.
	unread := &apiAcceptanceUploadBody{reader: strings.NewReader("payload"), closed: make(chan struct{})}
	// r содержит PUT с text/plain, который должен отклоняться до потребления unread.
	r := httptest.NewRequest("PUT", api.Prefix+"/fallback-assets/"+string(asset.ID)+"/content?kind=image&expected_revision=1", unread)
	r.Header.Set("Content-Type", "text/plain")
	// w сохраняет ответ неверному типу содержимого.
	w := httptest.NewRecorder()
	f.api.ServeHTTP(w, r)
	if w.Code != 415 || unread.reads.Load() != 0 {
		t.Fatal("тип содержимого проверяется после чтения", w.Code, unread.reads.Load())
	}
}

// Проверяет отказ переполненной очереди до чтения, отмену ожидающих заданий и закрытие непрочитанного
// тела при отмене настоящего HTTP-запроса.
func TestAcceptanceHTTPUploadQueueRejectsBeforeReadAndCancellationClosesQueuedBody(t *testing.T) {
	// f содержит API очереди; p показывает задания; inspect удерживает первое задание; release
	// снимает блокировку.
	f, p, inspect, release := newAPIAcceptanceUpload(t, time.Second)
	defer release()
	inspect.blocked.Store(true)
	// ctx ограничивает сценарий очереди тремя секундами; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// assets содержит четыре независимые заглушки для заполнения и переполнения бюджета подготовки.
	assets := make([]core.FallbackAsset, 4)
	// n указывает позицию создаваемой заглушки в assets.
	for n := range assets {
		// a содержит очередную заглушку; err содержит отказ её создания.
		a, err := f.service.CreateAsset(ctx, "Очередь")
		if err != nil {
			t.Fatal(err)
		}
		assets[n] = a
	}
	// firstBody содержит корректный PNG первого задания, которое удерживается на инспекции.
	firstBody := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	apiAcceptanceUploadRequest(t, f, ctx, string(assets[0].ID), assets[0].Revision, firstBody, 202)
	// first содержит первое принятое задание, занимающее работника подготовки.
	first := <-p.jobs
	apiAcceptanceAwait(t, inspect.entered)
	// queued собирает два следующих задания, ожидающих за удерживаемым первым.
	var queued []*fallback.Job
	// a содержит очередную заглушку для прямого заполнения очереди подготовки.
	for _, a := range assets[1:3] {
		// body содержит ещё не прочитанный PNG очередного ожидающего задания.
		body := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
		// j содержит принятое ожидающее задание; err содержит отказ его допуска.
		j, err := p.Upload(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage}, body)
		if err != nil {
			t.Fatal(err)
		}
		queued = append(queued, j)
	}
	// unread содержит тело четвёртой загрузки, которое переполненный допуск не должен читать.
	unread := &apiAcceptanceUploadBody{reader: strings.NewReader("not consumed"), closed: make(chan struct{})}
	apiAcceptanceUploadRequest(t, f, ctx, string(assets[3].ID), assets[3].Revision, unread, 503)
	if unread.reads.Load() != 0 {
		t.Fatal("переполненная очередь читает тело до admission")
	}
	// j содержит очередное ожидающее задание для управляемой отмены передачи.
	for _, j := range queued {
		// cancelled задаёт уже отменённое ожидание передачи; stop отменяет его перед WaitUpload.
		cancelled, stop := context.WithCancel(ctx)
		stop()
		// err содержит результат отмены ожидающей передачи через WaitUpload.
		if err := j.WaitUpload(cancelled); !errors.Is(err, context.Canceled) {
			t.Fatal("queued transfer не отменён", err)
		}
		// err содержит итог ожидающего задания после отмены, ожидается context.Canceled.
		if _, err := j.Wait(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal("queued job не завершён", err)
		}
	}
	// Освободившийся бюджет проверяется через настоящий HTTP-обработчик и загрузку, ещё ожидающую допуска к подготовке.
	// queuedBody содержит PNG новой HTTP-загрузки в освободившийся бюджет, пока первый работник всё
	// ещё занят.
	queuedBody := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	// requestCtx управляет ожидающей HTTP-загрузкой; requestCancel отменяет её до начала чтения.
	requestCtx, requestCancel := context.WithCancel(context.Background())
	// done передаёт ответ параллельного HTTP-обработчика после отмены ожидающей передачи.
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		// Запускает HTTP-загрузку в очереди параллельно, чтобы проверить её отмену до чтения тела.

		// r содержит настоящий PUT ожидающей загрузки с неизвестной длиной тела.
		r := httptest.NewRequest("PUT", api.Prefix+"/fallback-assets/"+string(assets[3].ID)+"/content?kind=image&expected_revision=1", queuedBody).WithContext(requestCtx)
		r.ContentLength = -1
		r.Header.Set("Content-Type", "application/octet-stream")
		// w сохраняет ответ параллельной ожидающей загрузки.
		w := httptest.NewRecorder()
		f.api.ServeHTTP(w, r)
		done <- w
	}()
	defer requestCancel()
	// Первые два задания уже получены напрямую, но также сохранены прозрачным портом.
	<-p.jobs
	<-p.jobs
	// pending сохраняет принятое HTTP-задание до явной отмены его непрочитанного тела.
	var pending *fallback.Job
	select {
	case pending = <-p.jobs:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	requestCancel()
	select {
	// w содержит ответ отменённой передачи для проверки безопасного HTTP 408.
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
	// err содержит итог отменённого ожидающего HTTP-задания.
	if _, err := pending.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("отменённое HTTP-задание осталось в очереди", err)
	}
	release()
	// err содержит итог первого задания после освобождения инспекции, ожидается управляемая ошибка.
	if _, err := first.Wait(ctx); err == nil {
		t.Fatal("управляемая ошибка инспекции потеряна")
	}
}

// Проверяет, что известная Session без RAM получает 200 с not_observed и null, а неизвестная Session —
// 404, сохраняя доступность квитанции.
func TestAcceptanceKnownSessionWithoutRAMIsDifferentFromUnknown(t *testing.T) {
	// f содержит API; p передаёт задания подготовки; release освобождает управляемые ресурсы после
	// проверки известной Session.
	f, p, _, release := newAPIAcceptanceUpload(t, time.Second)
	defer release()
	// ctx ограничивает подготовку и запуск тремя секундами; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// asset содержит заглушку для Session без оперативных наблюдений; err содержит отказ создания.
	asset, err := f.service.CreateAsset(ctx, "Готовая заглушка")
	if err != nil {
		t.Fatal(err)
	}
	// body содержит PNG для подготовки готового поколения заглушки.
	body := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	apiAcceptanceUploadRequest(t, f, ctx, string(asset.ID), asset.Revision, body, 202)
	if _, err = (<-p.jobs).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	// stream содержит поток, команда запуска которого принимается без открытия RAM-наблюдателя.
	stream, err := f.service.CreateStream(ctx, core.Stream{Name: "Ещё без наблюдений", PublicationKey: "synthetic-publication-secret", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	// accepted содержит долговечную квитанцию запуска с идентификатором известной Session.
	accepted := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/streams/"+string(stream.ID)+"/start", `{"request_id":"unobserved-start","destination_ids":[]}`, 202))
	// id содержит идентификатор принятой Session без текущего RAM-наблюдения.
	id := accepted["session_id"].(string)
	// unobserved содержит ответ метрик известной Session с unavailable, not_observed и null вместо
	// чужой телеметрии.
	unobserved := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/sessions/"+id+"/metrics", "", 200))
	if unobserved["availability"] != "unavailable" || unobserved["snapshot"] != nil || unobserved["reason"] != "not_observed" {
		t.Fatal("известная сессия без RAM подменена метриками или ошибкой", unobserved)
	}
	apiAcceptanceRequest(t, f.api, "GET", "/sessions/unknown/metrics", "", 404)
	apiAcceptanceRequest(t, f.api, "GET", "/commands/"+accepted["id"].(string), "", 200)
}
