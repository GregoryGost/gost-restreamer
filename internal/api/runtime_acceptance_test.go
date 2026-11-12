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

// apiAcceptanceSystem получает точного системного наблюдателя общей RuntimeAuthority и регистрирует
// его закрытие при очистке теста.
func apiAcceptanceSystem(t *testing.T, f *apiAcceptanceFixture) *core.SystemObserver {
	t.Helper()
	// o содержит точного системного наблюдателя общей RuntimeAuthority; err содержит отказ
	// регистрации.
	o, err := f.service.OpenSystemObserver(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Освобождает системного наблюдателя GPU после теста HTTP или SSE.
		_ = o.Close(context.Background())
	})
	return o
}

// apiAcceptanceGPU создаёт известный системный снимок с точными байтами памяти, измеренным нулём
// загрузки и неподдержанными отдельными полями.
func apiAcceptanceGPU(n uint64, at time.Time) core.GPUSnapshot {
	// available задаёт доступное измеренное нулевое значение GPU с известным временем и периодом.
	available := core.GPUMetric{Status: core.MetricAvailable, Value: 0, SampledAt: at, LastSuccessAt: at, SamplingPeriodUS: 2000000}
	// unavailable задаёт неподдержанное поле GPU без численного измерения.
	unavailable := core.GPUMetric{Status: core.MetricUnsupported, Reason: core.ReasonNotSupported, SampledAt: at}
	// memory копирует доступность и времена, получая точное проверяемое число байтов памяти.
	memory := available
	memory.Value = n
	return core.GPUSnapshot{Status: core.MetricAvailable, SampledAt: at, StaleAfter: time.Minute, Devices: []core.GPUDevice{{UUID: "synthetic-gpu", Utilization: available, MemoryUtilization: available, EncoderUtilization: unavailable, DecoderUtilization: unavailable, MemoryUsedBytes: memory, MemoryTotalBytes: memory, MemoryFreeBytes: memory}}}
}

// apiAcceptanceStreamWriter моделирует SSE-транспорт с наблюдаемыми сроками записи, блокировкой и
// согласованным буфером.
type apiAcceptanceStreamWriter struct {
	// header хранит HTTP-заголовки, устанавливаемые настоящим обработчиком SSE.
	header http.Header
	// mu защищает тело, статус, срок и признаки использования транспорта между обработчиком и
	// тестом.
	mu sync.Mutex
	// body накапливает целые байты успешных записей SSE.
	body bytes.Buffer
	// status сохраняет первый HTTP-статус, включая неявный 200 при Write.
	status int
	// deadline сохраняет текущий установленный срок записи для проверки до Write и Flush.
	deadline time.Time
	// unsafeWrite запоминает попытку Write или Flush без установленного срока.
	unsafeWrite bool
	// block включает удержание Write до истечения установленного срока.
	block bool
	// flushed закрывается после первого Flush для проверки начальной готовности подписки.
	flushed chan struct{}
	// written закрывается при первой попытке Write, включая заблокированную запись.
	written chan struct{}
	// changed неблокирующе уведомляет проверку о новых байтах в согласованном буфере.
	changed chan struct{}
	// flushOnce обеспечивает единственное закрытие flushed; writeOnce обеспечивает единственное
	// закрытие written.
	flushOnce, writeOnce sync.Once
}

// newAPIAcceptanceStreamWriter создаёт управляемый HTTP-получатель с наблюдением сроков, начала записи
// и сброса буфера для проверки SSE.
func newAPIAcceptanceStreamWriter() *apiAcceptanceStreamWriter {
	return &apiAcceptanceStreamWriter{header: make(http.Header), flushed: make(chan struct{}), written: make(chan struct{}), changed: make(chan struct{}, 1)}
}

// Header возвращает заголовки управляемого HTTP-получателя для настоящего обработчика API.
func (w *apiAcceptanceStreamWriter) Header() http.Header { return w.header }

// WriteHeader сохраняет первый HTTP-статус под блокировкой, не заменяя его поздней записью.
func (w *apiAcceptanceStreamWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = status
	}
}

// SetWriteDeadline сохраняет срок физической записи для проверки ограничения первого кадра и сброса
// буфера.
func (w *apiAcceptanceStreamWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadline = deadline
	return nil
}

// Write фиксирует наличие срока и начало записи; при включённой блокировке ждёт истечения срока и
// возвращает ошибку, иначе сохраняет целые байты кадра.
func (w *apiAcceptanceStreamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	// deadline сохраняет срок конкретной попытки Write после снятия блокировки состояния.
	deadline := w.deadline
	w.unsafeWrite = w.unsafeWrite || deadline.IsZero()
	// block сохраняет режим задержки конкретного Write после снятия блокировки.
	block := w.block
	if w.status == 0 {
		w.status = 200
	}
	w.mu.Unlock()
	w.writeOnce.Do(func() {
		// Однократно сообщает о первой попытке записи, включая запись, заблокированную до срока.
		close(w.written)
	})
	if block {
		if deadline.IsZero() {
			return 0, errors.New("запись без срока")
		}
		// timer удерживает тестовый Write ровно до установленного срока перед
		// os.ErrDeadlineExceeded.
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		<-timer.C
		return 0, os.ErrDeadlineExceeded
	}
	w.mu.Lock()
	// n содержит число сохранённых байтов кадра; err содержит отказ записи в тестовый буфер.
	n, err := w.body.Write(p)
	w.mu.Unlock()
	select {
	case w.changed <- struct{}{}:
	default:
	}
	return n, err
}

// Flush фиксирует отсутствие срока как нарушение и однократно сообщает о первом сбросе буфера.
func (w *apiAcceptanceStreamWriter) Flush() {
	w.mu.Lock()
	w.unsafeWrite = w.unsafeWrite || w.deadline.IsZero()
	w.mu.Unlock()
	w.flushOnce.Do(func() {
		// Однократно сообщает о первой принудительной передаче данных клиенту.
		close(w.flushed)
	})
}

// snapshot возвращает согласованную копию байтов, статуса и признака записи без установленного срока.
func (w *apiAcceptanceStreamWriter) snapshot() (string, int, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String(), w.status, w.unsafeWrite
}

// apiAcceptanceStartStream запускает настоящий SSE-обработчик и возвращает отмену и подтверждение
// завершения, регистрируя их при очистке теста.
func apiAcceptanceStartStream(t *testing.T, f *apiAcceptanceFixture, path string, w http.ResponseWriter) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	// ctx управляет настоящим SSE-запросом; cancel прекращает подписку после проверки или при
	// очистке.
	ctx, cancel := context.WithCancel(context.Background())
	// done подтверждает возврат настоящего HTTP-обработчика подписки.
	done := make(chan struct{})
	go func() {
		// Выполняет запрос SSE параллельно и закрывает done после возврата обработчика.

		defer close(done)
		f.api.ServeHTTP(w, httptest.NewRequest("GET", api.Prefix+path, nil).WithContext(ctx))
	}()
	t.Cleanup(func() {
		// Отменяет тестовую подписку и ожидает завершения её HTTP-обработчика.
		cancel()
		apiAcceptanceAwait(t, done)
	})
	return cancel, done
}

// apiAcceptanceAwait ограниченно ждёт подтверждения этапа потока, завершая сценарий при превышении
// срока.
func apiAcceptanceAwait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("поток не завершил этап до срока")
	}
}

// apiAcceptanceAwaitFrame ждёт появления заданного содержимого в согласованном буфере получателя с
// конечным сроком.
func apiAcceptanceAwaitFrame(t *testing.T, w *apiAcceptanceStreamWriter, content string) {
	t.Helper()
	// timer ограничивает ожидание нужного содержимого SSE тремя секундами.
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		// body содержит согласованную текущую копию накопленных кадров для поиска нужной строки.
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

// Проверяет установку срока до первого сброса буфера SSE, ограничение числа клиентов и освобождение
// места после отмены запроса без закрытия Store.
func TestAcceptanceSSEEstablishesDeadlineBeforeInitialFlushAndReleasesClientSlot(t *testing.T) {
	// f содержит API с редким heartbeat для проверки именно первого кадра.
	f := newAPIAcceptanceFixture(t, func(c *api.Config) {
		// Увеличивает интервал служебных сообщений, чтобы проверка первого кадра не зависела от периодической
		// отправки.
		c.Limits.SSEHeartbeat = time.Minute
	})
	// w содержит первый управляемый SSE-получатель.
	w := newAPIAcceptanceStreamWriter()
	// cancel отменяет первую подписку; done подтверждает её возврат и освобождение клиентского
	// места.
	cancel, done := apiAcceptanceStartStream(t, f, "/events", w)
	apiAcceptanceAwait(t, w.flushed)
	// body содержит начальные кадры; status содержит HTTP-статус; unsafe отмечает Write или Flush
	// без срока.
	body, status, unsafe := w.snapshot()
	if unsafe || status != 200 || !strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("первый flush не ограничен: status=%d unsafe=%v body=%s", status, unsafe, body)
	}
	apiAcceptanceRequest(t, f.api, "GET", "/events", "", 503)
	cancel()
	apiAcceptanceAwait(t, done)
	// w2 содержит второй SSE-получатель после освобождения единственного клиентского места.
	w2 := newAPIAcceptanceStreamWriter()
	// secondCancel отменяет вторую подписку; secondDone подтверждает её завершение.
	secondCancel, secondDone := apiAcceptanceStartStream(t, f, "/events", w2)
	apiAcceptanceAwait(t, w2.flushed)
	secondCancel()
	apiAcceptanceAwait(t, secondDone)
	// err содержит результат доменного чтения для проверки, что закрытие SSE не затронуло общий
	// Store.
	if _, err := f.service.ListStreams(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 1}}); err != nil {
		t.Fatal("SSE закрыл общий Store", err)
	}
}

// Проверяет отказ до начала SSE для ResponseWriter без необходимой поддержки ограниченной потоковой
// записи.
func TestAcceptanceSSEUnsupportedWriterIsRejectedBeforeHeaders(t *testing.T) {
	// f содержит API сценария неподдержанного потокового транспорта.
	f := newAPIAcceptanceFixture(t, nil)
	// w содержит безопасный отказ 503 ResponseRecorder без нужных транспортных возможностей.
	w := apiAcceptanceRequest(t, f.api, "GET", "/events", "", 503)
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatal("недоступный transport начал SSE", w.Header())
	}
}

// Проверяет, что ошибки валидации SSE не занимают клиентские места и не раскрывают отклонённые
// значения.
func TestAcceptanceSSEValidationFailureDoesNotConsumeClientSlot(t *testing.T) {
	// f содержит API сценария повторных ошибок SSE-валидации без занятия мест.
	f := newAPIAcceptanceFixture(t, nil)
	for range 3 {
		// w содержит управляемый транспорт очередного отклонённого запроса.
		w := newAPIAcceptanceStreamWriter()
		f.api.ServeHTTP(w, httptest.NewRequest("GET", api.Prefix+"/events?unexpected=synthetic-rejected-secret", nil))
		// body содержит ответ ошибки валидации; status содержит его фактический HTTP-код.
		body, status, _ := w.snapshot()
		if status != 422 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
			t.Fatalf("ошибка validation начала stream: %d %s", status, body)
		}
		apiAcceptanceNoSecrets(t, body, "synthetic-rejected-secret")
	}
	// w содержит SSE-получатель допустимой подписки после трёх отказов валидации.
	w := newAPIAcceptanceStreamWriter()
	// cancel отменяет допущенную подписку; done подтверждает её завершение.
	cancel, done := apiAcceptanceStartStream(t, f, "/events", w)
	apiAcceptanceAwait(t, w.flushed)
	// status содержит код допустимой подписки для проверки свободного SSEClients.
	_, status, _ := w.snapshot()
	if status != 200 {
		t.Fatal("отклонённые запросы исчерпали SSEClients", status)
	}
	cancel()
	apiAcceptanceAwait(t, done)
}

// Проверяет, что медленный получатель не удерживает RuntimeAuthority, завершается по сроку записи и не
// мешает ограниченному закрытию API.
func TestAcceptanceSSESlowWriterDoesNotHoldRuntimeAndCloseIsBounded(t *testing.T) {
	// f задаёт API с коротким сроком физической записи SSE.
	f := newAPIAcceptanceFixture(t, func(c *api.Config) {
		// Задаёт короткий срок записи для проверки медленного клиента SSE.
		c.Limits.SSEWriteTimeout = 50 * time.Millisecond
	})
	// o содержит точного системного наблюдателя для конкурентной публикации при медленном клиенте.
	o := apiAcceptanceSystem(t, f)
	// w содержит намеренно блокирующий SSE-транспорт.
	w := newAPIAcceptanceStreamWriter()
	w.block = true
	// done подтверждает завершение медленной подписки по сроку записи.
	_, done := apiAcceptanceStartStream(t, f, "/events", w)
	apiAcceptanceAwait(t, w.written)
	// ctx ограничивает проверки RuntimeAuthority и Close секундой; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// err содержит результат публикации GPU при заблокированной записи SSE, чтобы проверить
	// доступность RuntimeAuthority для других операций.
	if err := o.ObserveGPU(ctx, apiAcceptanceGPU(9007199254740993, time.Now())); err != nil {
		t.Fatal("медленный writer удерживает runtime authority", err)
	}
	// err содержит результат атомарного снимка во время медленной записи.
	if _, err := f.service.RuntimeAnchor(ctx); err != nil {
		t.Fatal("медленный writer блокирует anchor", err)
	}
	apiAcceptanceAwait(t, done)
	// err содержит результат ограниченного Close API после медленной подписки.
	if err := f.api.Close(ctx); err != nil {
		t.Fatal("HTTP shutdown не завершён", err)
	}
	// unsafe отмечает запись без срока в завершённой медленной подписке.
	if _, _, unsafe := w.snapshot(); unsafe {
		t.Fatal("запись начата до deadline")
	}
	apiAcceptanceRequest(t, f.api, "GET", "/health/ready", "", 503)
	// err содержит результат чтения общей RuntimeAuthority после Close API.
	if _, err := f.service.RuntimeAnchor(ctx); err != nil {
		t.Fatal("Close API закрыл общую authority", err)
	}
}

// Проверяет отмену принадлежащего API потока SSE при Close, отказ новых запросов и сохранение общей
// RuntimeAuthority.
func TestAcceptanceAPICloseCancelsOwnedSSEAndPreservesSharedStore(t *testing.T) {
	// f содержит API сценария принадлежащей ему отмены SSE.
	f := newAPIAcceptanceFixture(t, nil)
	// w содержит управляемый получатель активной подписки перед Close.
	w := newAPIAcceptanceStreamWriter()
	// done подтверждает возврат подписки, отменённой самим API.
	_, done := apiAcceptanceStartStream(t, f, "/events", w)
	apiAcceptanceAwait(t, w.flushed)
	// ctx ограничивает Close API секундой; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// err содержит результат закрытия API с активной SSE-подпиской.
	if err := f.api.Close(ctx); err != nil {
		t.Fatal(err)
	}
	apiAcceptanceAwait(t, done)
	apiAcceptanceRequest(t, f.api, "GET", "/events", "", 503)
	// err содержит результат чтения общей RuntimeAuthority после отмены принадлежащей API подписки.
	if _, err := f.service.RuntimeAnchor(ctx); err != nil {
		t.Fatal("Close API уничтожил общую authority", err)
	}
}

// Проверяет, что слишком большой кадр приводит к resync_required без частичной публикации исходного
// GPU-события.
func TestAcceptanceSSEOversizedFrameRequiresResyncWithoutPartialEvent(t *testing.T) {
	// f задаёт API с кадром не более 256 байтов для проверки resync_required.
	f := newAPIAcceptanceFixture(t, func(c *api.Config) {
		// Уменьшает предел кадра, чтобы событие GPU потребовало повторного снимка.
		c.Limits.SSEFrameBytes = 256
	})
	// snapshot сохраняет позицию потока до публикации большого GPU-события.
	snapshot := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/events/snapshot", "", 200))
	// o содержит системного владельца большого GPU-события.
	o := apiAcceptanceSystem(t, f)
	// err содержит отказ публикации исходного GPU-снимка в RuntimeAuthority.
	if err := o.ObserveGPU(context.Background(), apiAcceptanceGPU(9007199254740993, time.Now())); err != nil {
		t.Fatal(err)
	}
	// w содержит SSE-получатель события, не помещающегося в размер кадра.
	w := newAPIAcceptanceStreamWriter()
	// done подтверждает возврат подписки после уведомления о необходимости нового снимка.
	_, done := apiAcceptanceStartStream(t, f, "/events?cursor="+url.QueryEscape(snapshot["cursor"].(string)), w)
	apiAcceptanceAwait(t, done)
	// body содержит переданные кадры; status содержит HTTP-статус для проверки целого resync без
	// части GPU.
	body, status, _ := w.snapshot()
	if status != 200 || !strings.Contains(body, "event: resync_required") || !strings.Contains(body, "frame_limit") || strings.Contains(body, "9007199254740993") || strings.Contains(body, "event: gpu") {
		t.Fatal("большой frame опубликован частично", status, body)
	}
}

// Проверяет требование нового снимка при пропуске журнала и смене epoch без выдачи неполного диапазона
// за непрерывную историю.
func TestAcceptanceSSEGapAndEpochResetRequireFreshSnapshot(t *testing.T) {
	// reset выбирает смену эпохи либо потерю диапазона в той же очереди.
	for _, reset := range []bool{false, true} {
		t.Run(map[bool]string{false: "gap", true: "reset"}[reset], func(t *testing.T) {
			// Проверяет требование повторного снимка при выбранной причине неполного диапазона: пропуске или смене
			// эпохи.

			// f содержит отдельный API текущего сценария разрыва потока.
			f := newAPIAcceptanceFixture(t, nil)
			// snapshot сохраняет начальный атомарный снимок текущей RuntimeAuthority вместе
			// с позицией потока событий.
			snapshot := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/events/snapshot", "", 200))
			// cursor содержит публичную позицию подписки; ok подтверждает её непустую
			// строку.
			cursor, ok := snapshot["cursor"].(string)
			if !ok || cursor == "" {
				t.Fatal("anchor не содержит публичный cursor", snapshot)
			}
			if strings.Contains(cursor, f.store.Incarnation()) {
				t.Fatal("cursor раскрывает внутреннего владельца")
			}
			if reset {
				// other содержит отдельную фикстуру API со своей RuntimeAuthority для
				// получения курсора чужой эпохи.
				other := newAPIAcceptanceFixture(t, nil)
				// foreign содержит снимок независимого API, чей курсор должен вызвать
				// требование повторной синхронизации.
				foreign := apiAcceptanceJSON(t, apiAcceptanceRequest(t, other.api, "GET", "/events/snapshot", "", 200))
				cursor = foreign["cursor"].(string)
			} else {
				// o содержит системного наблюдателя для вытеснения начального диапазона
				// событий.
				o := apiAcceptanceSystem(t, f)
				// at задаёт начало последовательных времён GPU-публикаций.
				at := time.Now()
				// i задаёт номер публикации сверх восьми сохраняемых событий для
				// создания gap.
				for i := range 12 {
					// err содержит отказ очередной публикации, заполняющей
					// ограниченную очередь событий.
					if err := o.ObserveGPU(context.Background(), apiAcceptanceGPU(uint64(i), at.Add(time.Duration(i)*time.Nanosecond))); err != nil {
						t.Fatal(err)
					}
				}
			}
			// w содержит получателя подписки с пропущенным либо чужим курсором.
			w := newAPIAcceptanceStreamWriter()
			// done подтверждает завершение подписки после требования повторного снимка.
			_, done := apiAcceptanceStartStream(t, f, "/events?cursor="+url.QueryEscape(cursor), w)
			apiAcceptanceAwait(t, done)
			// body содержит кадры ответа; status содержит HTTP-код; unsafe отмечает
			// отсутствие срока при записи.
			body, status, unsafe := w.snapshot()
			if status != 200 || unsafe || !strings.Contains(body, "event: resync_required") || !strings.Contains(body, api.Prefix+"/events/snapshot") || strings.Contains(body, "event: gpu") {
				t.Fatalf("неполный диапазон применён вместо resync: %d %s", status, body)
			}
			apiAcceptanceNoSecrets(t, body, f.store.Incarnation())
		})
	}
}

// Проверяет различие точных больших значений, измеренного нуля и неподдержанных полей в REST/SSE и
// отклонение конфликтующих cursor и Last-Event-ID.
func TestAcceptanceSnapshotAndEventsKeepUnavailableValuesDistinctFromZero(t *testing.T) {
	// f содержит API сценария точных значений GPU и разных видов доступности.
	f := newAPIAcceptanceFixture(t, nil)
	// o содержит системного наблюдателя текущего GPU.
	o := apiAcceptanceSystem(t, f)
	// at задаёт точное UTC-время начального измерения и последующего события.
	at := time.Now().UTC()
	// err содержит отказ начальной GPU-публикации с большим uint64.
	if err := o.ObserveGPU(context.Background(), apiAcceptanceGPU(9007199254740993, at)); err != nil {
		t.Fatal(err)
	}
	// w сохраняет атомарный HTTP-снимок GPU.
	w := apiAcceptanceRequest(t, f.api, "GET", "/events/snapshot", "", 200)
	// v содержит разобранный JSON атомарного снимка.
	v := apiAcceptanceJSON(t, w)
	// gpu содержит публичный системный снимок GPU.
	gpu := v["gpu"].(map[string]any)
	// device содержит единственное публичное устройство для проверки отдельных полей.
	device := gpu["devices"].([]any)[0].(map[string]any)
	// used содержит точное большое измерение занятых байтов памяти.
	used := device["memory_used_bytes"].(map[string]any)
	// zero содержит доступный измеренный ноль загрузки GPU.
	zero := device["utilization"].(map[string]any)
	// unavailable содержит неподдержанный Encoder с null вместо измеренного нуля.
	unavailable := device["encoder_utilization"].(map[string]any)
	if used["value"] != "9007199254740993" || zero["value"] != "0" || zero["status"] != "available" || unavailable["value"] != nil || unavailable["status"] != "unsupported" || unavailable["reason"] != "not_supported" {
		t.Fatalf("потеряны точность/доступность: %s", w.Body)
	}
	// cursor сохраняет границу начального HTTP-снимка для следующего SSE-события.
	cursor := v["cursor"].(string)
	// err содержит отказ последующего GPU-измерения с новым точным значением.
	if err := o.ObserveGPU(context.Background(), apiAcceptanceGPU(9007199254740994, at.Add(time.Nanosecond))); err != nil {
		t.Fatal(err)
	}
	// stream содержит SSE-получателя события после сохранённого cursor.
	stream := newAPIAcceptanceStreamWriter()
	// cancel прекращает подписку после нужного значения; done подтверждает её возврат.
	cancel, done := apiAcceptanceStartStream(t, f, "/events?cursor="+url.QueryEscape(cursor), stream)
	apiAcceptanceAwaitFrame(t, stream, "9007199254740994")
	cancel()
	apiAcceptanceAwait(t, done)
	// body содержит полученное SSE-событие для проверки точного значения и отсутствия внутренних
	// данных.
	body, _, _ := stream.snapshot()
	if !strings.Contains(body, "9007199254740994") {
		t.Fatal("event после anchor потерян", body)
	}
	apiAcceptanceNoSecrets(t, body, f.store.Incarnation())
	// conflict содержит запрос с различающимися query cursor и Last-Event-ID.
	conflict := httptest.NewRequest("GET", api.Prefix+"/events?cursor="+url.QueryEscape(cursor), nil)
	conflict.Header.Set("Last-Event-ID", "different-cursor")
	// response сохраняет отказ конфликтующей позиции до начала SSE.
	response := httptest.NewRecorder()
	f.api.ServeHTTP(response, conflict)
	if response.Code != 400 {
		t.Fatal("конфликт cursor не отклонён до stream", response.Code, response.Body)
	}
}

// Проверяет атомарность публичного cursor и значения GPU при конкурентном обновлении и чтении
// RAM-снимка через HTTP.
func TestAcceptanceHTTPAnchorKeepsCursorAndGPUInOneAuthoritySnapshot(t *testing.T) {
	// f содержит API сценария атомарного совпадения курсора и GPU при конкурентном чтении.
	f := newAPIAcceptanceFixture(t, nil)
	// o содержит единственного системного публикующего владельца.
	o := apiAcceptanceSystem(t, f)
	// ctx ограничивает публикации и контрольные чтения тремя секундами; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// observed связывает публичный курсор HTTP-снимка с прочитанным в том же ответе значением GPU.
	type observed struct { /* cursor сохраняет публичную позицию ответа; value сохраняет точное значение памяти из того же ответа. */
		cursor, value string
	}
	// responses собирает пары курсора и значения от четырёх конкурентных читателей.
	responses := make(chan observed, 64)
	// failures передаёт ошибки конкурентных HTTP-чтений основному тесту.
	failures := make(chan error, 64)
	// start одновременно допускает читателей к запросам во время GPU-публикаций.
	start := make(chan struct{})
	// readers подтверждает завершение всех конкурентных HTTP-читателей.
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			// Параллельно собирает курсоры и значения GPU из HTTP-снимков во время публикации измерений.

			<-start
			for range 16 {
				// w сохраняет очередной атомарный HTTP-ответ одного читателя.
				w := httptest.NewRecorder()
				f.api.ServeHTTP(w, httptest.NewRequest("GET", api.Prefix+"/events/snapshot", nil))
				// v принимает только курсор и нужное поле GPU из атомарного JSON-ответа.
				var v struct {
					// Cursor принимает публичную позицию атомарного ответа.
					Cursor string `json:"cursor"`
					// GPU отличает отсутствие системного наблюдения от
					// присутствующего снимка.
					GPU *struct {
						// Devices принимает устройства системного снимка.
						Devices []struct {
							// Memory принимает публичный показатель memory_used_bytes
							// текущего устройства.
							Memory struct {
								// Value отличает точную строку измеренных байтов от null.
								Value *string `json:"value"`
							} `json:"memory_used_bytes"`
						} `json:"devices"`
					} `json:"gpu"`
				}
				if w.Code != 200 {
					failures <- errors.New("HTTP anchor недоступен")
					return
				}
				// err содержит отказ разбора атомарного JSON-ответа конкурентным
				// читателем.
				if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
					failures <- err
					return
				}
				// value сохраняет строку GPU из ответа либо пустоту до первой
				// публикации.
				value := ""
				if v.GPU != nil && len(v.GPU.Devices) > 0 && v.GPU.Devices[0].Memory.Value != nil {
					value = *v.GPU.Devices[0].Memory.Value
				}
				responses <- observed{v.Cursor, value}
			}
		})
	}
	// states сопоставляет номер события точному GPU-значению последовательного публикующего
	// владельца.
	states := map[string]string{"0": ""}
	// at задаёт начало уникальных времён последовательных GPU-публикаций.
	at := time.Now()
	close(start)
	// n задаёт номер одного из шестнадцати различимых GPU-измерений.
	for n := range 16 {
		// value задаёт точное большое число байтов текущего измерения.
		value := uint64(9007199254740993 + n*17)
		// err содержит отказ публикации текущего GPU-измерения.
		if err := o.ObserveGPU(ctx, apiAcceptanceGPU(value, at.Add(time.Duration(n)*time.Nanosecond))); err != nil {
			t.Fatal(err)
		}
		// anchor содержит контрольную атомарную позицию после публикации; err содержит отказ её
		// чтения.
		anchor, err := f.service.RuntimeAnchor(ctx)
		if err != nil {
			t.Fatal(err)
		}
		states[strconv.FormatUint(anchor.Cursor.Sequence, 10)] = strconv.FormatUint(value, 10)
	}
	readers.Wait()
	close(responses)
	close(failures)
	// err содержит очередной отказ конкурентного читателя, переданный основному тесту.
	for err := range failures {
		t.Fatal(err)
	}
	// result содержит очередную пару HTTP-курсора и GPU-значения для сравнения с states.
	for result := range responses {
		// raw содержит JSON-байты публичного курсора; err содержит отказ
		// base64url-декодирования.
		raw, err := base64.RawURLEncoding.DecodeString(result.cursor)
		if err != nil {
			t.Fatal(err)
		}
		// cursor принимает только публичную последовательность из декодированного курсора.
		var cursor struct {
			// Sequence содержит точную строку номера события, связывающую ответ с эталонным
			// состоянием.
			Sequence string `json:"sequence"`
		}
		if err = json.Unmarshal(raw, &cursor); err != nil {
			t.Fatal(err)
		}
		// expected содержит GPU-значение этой последовательности; ok подтверждает известное
		// контрольное состояние.
		expected, ok := states[cursor.Sequence]
		if !ok || expected != result.value {
			t.Fatalf("HTTP anchor смешал разные состояния authority: sequence=%s value=%s expected=%s", cursor.Sequence, result.value, expected)
		}
	}
}

// Проверяет актуальное разрешение исполняемых файлов без их запуска, независимость готовности от
// медиавозможностей и отказ проверки готовности после закрытия Store.
func TestAcceptanceReadinessAndCapabilitiesUseActualExecutableAvailability(t *testing.T) {
	// root выделяет каталог синтетических исполняемых файлов и маркера запрещённого запуска.
	root := t.TempDir()
	// executable указывает существующий инструмент, проверяемый без запуска.
	executable := filepath.Join(root, "media-tool")
	// marker указывает файл, который создал бы ошибочный запуск инструмента при capabilities.
	marker := filepath.Join(root, "must-not-execute")
	// err содержит отказ записи синтетического исполняемого сценария командной оболочки.
	if err := os.WriteFile(executable, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	// missing указывает заведомо отсутствующий инструмент для отдельной причины недоступности.
	missing := filepath.Join(root, "missing")
	// f связывает API с существующим FFmpeg, отсутствующим ffprobe и незаданным helper.
	f := newAPIAcceptanceFixture(t, func(c *api.Config) {
		// Настраивает существующий, отсутствующий и незаданный исполняемые файлы для проверки возможностей без
		// запуска процессов.

		c.Capabilities = api.ExecutableCapabilities{FFmpeg: executable, FFprobe: missing, Helper: ""}
	})
	// w сохраняет текущий ответ capabilities до и после снятия исполняемых прав.
	w := apiAcceptanceRequest(t, f.api, "GET", "/system/capabilities", "", 200)
	// v содержит начальные доступности и отдельный unverified NVIDIA.
	v := apiAcceptanceJSON(t, w)
	if v["ffmpeg"].(map[string]any)["status"] != "available" || v["ffprobe"].(map[string]any)["status"] != "unavailable" || v["helper"].(map[string]any)["status"] != "unavailable" || v["nvidia"].(map[string]any)["status"] != "unverified" {
		t.Fatal("capabilities подменяет проверенные факты", v)
	}
	// err содержит результат проверки отсутствия маркера, подтверждающего, что executable не
	// запускался.
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("проверка capabilities запустила executable", err)
	}
	apiAcceptanceNoSecrets(t, w.Body.String(), root)
	apiAcceptanceRequest(t, f.api, "GET", "/health/live", "", 200)
	apiAcceptanceRequest(t, f.api, "GET", "/health/ready", "", 200)
	// err содержит отказ снятия исполняемых прав для проверки актуальности последующего
	// capabilities.
	if err := os.Chmod(executable, 0600); err != nil {
		t.Fatal(err)
	}
	w = apiAcceptanceRequest(t, f.api, "GET", "/system/capabilities", "", 200)
	if apiAcceptanceJSON(t, w)["ffmpeg"].(map[string]any)["status"] != "unavailable" {
		t.Fatal("provider выдаёт устаревшую доступность executable")
	}
	apiAcceptanceRequest(t, f.api, "GET", "/health/ready", "", 200)
	// err содержит результат закрытия Store перед проверкой различия liveness и readiness.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	apiAcceptanceRequest(t, f.api, "GET", "/health/live", "", 200)
	apiAcceptanceRequest(t, f.api, "GET", "/health/ready", "", 503)
}

// Проверяет безопасный отказ для неверного, слишком длинного и будущего cursor и отсутствие
// incarnation в декодированной публичной позиции.
func TestAcceptanceRuntimeCursorCannotEchoUserInput(t *testing.T) {
	// f содержит API сценария безопасного отклонения публичных курсоров.
	f := newAPIAcceptanceFixture(t, nil)
	// cursor задаёт неверную или слишком длинную пользовательскую позицию.
	for _, cursor := range []string{"synthetic-rejected-secret", strings.Repeat("A", 2048)} {
		// w сохраняет безопасный отказ неверному cursor без отражения исходного значения.
		w := apiAcceptanceRequest(t, f.api, "GET", "/events?cursor="+url.QueryEscape(cursor), "", 400)
		// problem принимает Problem Details для проверки отсутствия внутренних ошибок валидации.
		var problem map[string]any
		// err содержит отказ разбора безопасного JSON-ответа ошибки курсора.
		if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
			t.Fatal(err)
		}
		if problem["errors"] != nil || strings.Contains(w.Body.String(), cursor) {
			t.Fatal("cursor попал в Problem Details")
		}
	}
	// snapshot содержит действительную атомарную позицию для отдельной проверки внутренней
	// идентичности.
	snapshot := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/events/snapshot", "", 200))
	// encoded содержит действительный публичный cursor из снимка.
	encoded := snapshot["cursor"].(string)
	// raw содержит декодированные публичные поля; err содержит отказ base64url.
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), f.store.Incarnation()) {
		t.Fatal("декодированный cursor раскрывает process incarnation")
	}
	// cursor принимает публичные поля позиции для подстановки будущего номера последовательности.
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
