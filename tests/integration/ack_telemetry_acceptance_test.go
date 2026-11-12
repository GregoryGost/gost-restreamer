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

// Отправляет заданные абсолютные значения RTMP ACK по единственному активному соединению приёмника;
// отсутствие или множественность соединений завершает тест ошибкой.
func ackTelemetrySend(t *testing.T, r *publisherReceiver, values ...uint32) {
	t.Helper()
	r.mu.Lock()
	// connections содержит снимок активных соединений приёмника, собранный под его блокировкой.
	connections := make([]net.Conn, 0, len(r.connections))
	// c обозначает очередное активное соединение, добавляемое в снимок для отправки ACK.
	for c := range r.connections {
		connections = append(connections, c)
	}
	r.mu.Unlock()
	if len(connections) != 1 {
		t.Fatalf("ожидалось одно активное соединение: %d", len(connections))
	}
	// conn — единственное соединение приёмника, в которое тест отправляет ACK.
	conn := connections[0]
	// err сообщает, удалось ли ограничить время записи ACK в соединение.
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// v задаёт очередное абсолютное число подтверждённых RTMP-байтов для синтетического ACK.
	for _, v := range values {
		// wire содержит RTMP-сообщение ACK; последние четыре байта заполняются абсолютным
		// счётчиком.
		wire := []byte{2, 0, 0, 0, 0, 0, 4, 3, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(wire[12:], v)
		// err сообщает об отказе записи синтетического ACK в соединение.
		if _, err := io.Copy(conn, bytes.NewReader(wire)); err != nil {
			t.Fatal(err)
		}
	}
}

// Извлекает метрики единственного выхода из публичного снимка; пока снимок, выход или его метрики
// недоступны, возвращает nil для повторного ожидания.
func ackTelemetryOutput(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	if response["availability"] != "available" {
		return nil
	}
	// s содержит публичный снимок с доступной телеметрией сессии.
	s := publicTelemetryObject(t, response["snapshot"])
	// values содержит выходы снимка, из которых ожидается ровно один.
	values := s["outputs"].([]any)
	if len(values) == 0 {
		return nil
	}
	if len(values) != 1 {
		t.Fatalf("неверное число выходов: %d", len(values))
	}
	// value содержит метрики единственного выхода либо nil до их появления.
	value := publicTelemetryObject(t, values[0])["metrics"]
	if value == nil {
		return nil
	}
	return publicTelemetryObject(t, value)
}

// Проверяет точные строковые счётчики аномалий ACK, отсутствие их дубликатов в counters и доступность
// времени аномалии в UTC.
func ackTelemetryAssert(t *testing.T, m map[string]any, total, last, max string, absent bool) {
	t.Helper()
	// key называет проверяемое поле аномалий ACK, а want содержит его ожидаемое точное строковое
	// значение.
	for key, want := range map[string]string{"ack_ahead_events_total": total, "ack_ahead_bytes_last": last, "ack_ahead_bytes_max": max} {
		if m[key] != want {
			t.Fatalf("метрика %s=%#v, ожидается %s; полный набор=%v", key, m[key], want, m)
		}
		// ok показывает, продублировано ли отдельное поле ACK во вложенном наборе counters.
		if _, ok := publicTelemetryObject(t, m["counters"])[key]; ok {
			t.Fatal("ACK повторяется в counters", key)
		}
	}
	// at содержит время последней аномалии ACK, а exists показывает наличие этого поля даже при
	// значении null.
	at, exists := m["ack_last_anomaly_at"]
	if !exists || absent && at != nil || !absent && at == nil {
		t.Fatal("неверная доступность времени ACK", m)
	}
	if !absent {
		// value содержит строковое представление времени аномалии, а ok подтверждает строковый
		// тип поля.
		value, ok := at.(string)
		if !ok {
			t.Fatal("время ACK не строка", at)
		}
		// parsed содержит разобранное время аномалии ACK, а err сообщает о нарушении формата
		// RFC3339Nano.
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil || parsed.IsZero() || value[len(value)-1] != 'Z' {
			t.Fatal("время ACK не UTC", at, err)
		}
	}
}

// Проверяет передачу аномалий RTMP ACK через REST и SSE, сохранение счётчиков при переподключении и их
// сброс при новом запуске выхода. ACK нового соединения сопоставляется только с байтами этого
// соединения.
func TestACKTelemetryAcceptanceNativeSessionRESTSSEAndReconnect(t *testing.T) {
	// ctx используется для управляющих операций с тестовой сессией.
	ctx := context.Background()
	// receiver принимает публикацию выхода и позволяет отправлять синтетические ACK в его
	// соединение.
	receiver, _ := newPublisherReceiver(t, false)
	// f объединяет сессию, хранилище и медиапроцессы для проверки ACK через публичный API.
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	// Штатный бюджет управления исключает истечение короткого срока фикстуры при инструментировании покрытия.
	f.config.OperationTimeout = config.Default().Session.OperationTimeout
	f.stream.Fallback.Watchdog.VideoTimeout = time.Minute
	f.stream.Fallback.Watchdog.AudioTimeout = time.Minute
	// err хранит результат обновления настроек тестового потока и последующих управляющих операций.
	var err error
	f.stream, err = f.service.UpdateStream(ctx, f.stream, f.stream.Revision)
	if err != nil {
		t.Fatal(err)
	}
	f.config.Group.FFmpeg.VideoTimeout, f.config.Group.FFmpeg.AudioTimeout = time.Minute, time.Minute
	// finished задаёт путь маркера окончания конечного тестового источника медиа.
	finished := filepath.Join(f.directory, "ack-finite-source-finished")
	f.config.Group.Player = func(_ context.Context, _ string, _ *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Создаёт команду конечного тестового медиапроцесса с маркером завершения для проверки ACK при
		// стабильном числе записанных байтов.

		// binary содержит путь текущего тестового бинарника для запуска дочернего источника, а
		// err сообщает об ошибке определения этого пути.
		binary, err := os.Executable()
		return ffmpeg.Command{Binary: binary, Args: []string{"-test.run=^TestPublicTelemetryFiniteMediaChild$", "--", "--public-telemetry", a.OutputPath, finished}}, err
	}
	f.launch(t)
	// a обслуживает управляющие запросы к сессии из фикстуры.
	a := integrationAPI(t, f)
	// server предоставляет локальное HTTP-соединение для проверки SSE этой сессии.
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	// current содержит сессию после подтверждённого перехода в рабочее состояние.
	current := f.running(t, f.start(t, "start-ack-metrics", f.destinations[0].ID))
	// path задаёт общий префикс HTTP-маршрутов запущенной сессии.
	path := "/sessions/" + string(current.ID)
	sessionAcceptanceWait(t, func() bool {
		// Ожидает появления маркера окончания конечного источника медиа.
		// err показывает, появился ли файл маркера окончания тестового источника.
		_, err := os.Stat(finished)
		return err == nil
	})
	// metrics читает через HTTP текущие метрики единственного выхода и возвращает nil до их
	// доступности.
	metrics := func() map[string]any {
		// Читает метрики единственного выхода через настоящий HTTP API.

		return ackTelemetryOutput(t, integrationAPIRequest(t, a, "GET", path+"/metrics", "", 200))
	}
	// m хранит последний прочитанный набор метрик выхода при ожидании изменений ACK.
	var m map[string]any
	sessionAcceptanceWait(t, func() bool {
		// Ожидает согласованное число переданных байтов и измеренное нулевое окно FPS после окончания
		// источника.

		m = metrics()
		return m != nil && publicTelemetryMedia(t, m)["written_bytes"] == strconv.FormatUint(receiver.bytes.Load(), 10) && publicTelemetryObject(t, publicTelemetryObject(t, publicTelemetryMedia(t, m)["rates"])["fps"])["value"] == float64(0)
	})
	ackTelemetryAssert(t, m, "0", "0", "0", true)
	// anchor содержит курсор снимка, с которого начинается подписка на последующие события ACK.
	anchor := integrationAPIRequest(t, a, "GET", "/events/snapshot", "", 200)
	// stream читает SSE после исходного снимка для сопоставления значений ACK с REST.
	stream := publicTelemetrySubscribe(t, server, "/events?cursor="+url.QueryEscape(anchor["cursor"].(string)), "")
	// baseline фиксирует число всех прочитанных приёмником RTMP-байтов до отправки аномальных ACK,
	// включая рукопожатие и команды.
	baseline := receiver.bytes.Load()
	// before ограничивает снизу допустимое время первой проверяемой аномалии ACK.
	before := time.Now().UTC()
	// extra задаёт превышение подтверждённых байтов над записанными для каждой из десяти аномалий.
	for extra := uint32(4); extra <= 13; extra++ {
		ackTelemetrySend(t, receiver, uint32(baseline)+extra)
	}
	ackTelemetrySend(t, receiver, uint32(baseline)+13, uint32(baseline)+12, uint32(baseline)+13+1<<31)
	sessionAcceptanceWait(t, func() bool {
		// Ожидает учёт десяти опережающих ACK без влияния повторных и устаревших значений.
		m = metrics()
		return m != nil && m["ack_ahead_events_total"] == "10"
	})
	ackTelemetryAssert(t, m, "10", "13", "13", false)
	// parsed содержит время аномалии из REST для проверки попадания в интервал отправки ACK.
	parsed, err := time.Parse(time.RFC3339Nano, m["ack_last_anomaly_at"].(string))
	if err != nil || parsed.Before(before) || parsed.After(time.Now()) {
		t.Fatal("время не относится к реальному ACK", m, err)
	}
	for {
		// frame содержит очередное событие SSE, а ok показывает, удалось ли прочитать его до
		// закрытия подписки.
		frame, ok := stream.next(t)
		if !ok || frame.kind == "resync_required" {
			t.Fatal("SSE потерял ACK", frame)
		}
		if frame.data["output"] != nil {
			// event содержит метрики выхода из SSE для сравнения аномалий ACK с REST.
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
		// Ожидает новое соединение и согласованные накопленные байты после разрыва назначения.

		m = metrics()
		return receiver.accepted.Load() == 2 && m != nil && publicTelemetryObject(t, m["counters"])["connections"] == "2" && publicTelemetryMedia(t, m)["written_bytes"] == strconv.FormatUint(receiver.bytes.Load(), 10)
	})
	ackTelemetryAssert(t, m, "10", "13", "13", false)
	// Новый ACK сравнивается с байтами нового соединения, а не суммой двух попыток.
	// freshBytes содержит число прочитанных RTMP-байтов нового соединения без накопления предыдущей
	// попытки.
	freshBytes := receiver.bytes.Load() - baseline
	ackTelemetrySend(t, receiver, uint32(freshBytes)+20)
	sessionAcceptanceWait(t, func() bool {
		// Ожидает следующую аномалию ACK, рассчитанную по байтам нового соединения.
		m = metrics()
		return m != nil && m["ack_ahead_events_total"] == "11"
	})
	ackTelemetryAssert(t, m, "11", "20", "20", false)
	// counters содержит счётчики попыток и переподключений для проверки отсутствия разрыва из-за
	// числовых ACK.
	if counters := publicTelemetryObject(t, m["counters"]); counters["attempts"] != "2" || counters["reconnects"] != "1" {
		t.Fatal("числовые ACK вызвали переподключение", counters)
	}
	// output содержит сохранённый выход сессии, который тест останавливает и запускает повторно.
	output := f.outputs(t, current.ID)[0]
	// outputPath задаёт HTTP-префикс команд проверяемого выхода.
	outputPath := path + "/outputs/" + string(output.ID)
	// stopped содержит квитанцию команды остановки выхода перед проверкой сброса ACK.
	stopped := integrationAPIRequest(t, a, "POST", outputPath+"/stop", `{"request_id":"stop-ack-output"}`, 202)
	sessionAcceptanceReceipt(t, f.service, core.ID(stopped["id"].(string)), core.CommandSucceeded)
	sessionAcceptanceWait(t, func() bool {
		// Ожидает подтверждение очистки остановленного выхода перед его ручным запуском.

		// value содержит актуальное состояние остановленного выхода, а err сообщает об ошибке
		// его чтения при ожидании очистки.
		value, err := f.service.GetOutput(ctx, output.ID)
		return err == nil && !value.Issued && !value.CleanupPending
	})
	receiver.waitClosed(t)
	// started содержит квитанцию повторного запуска выхода с независимыми счётчиками ACK.
	started := integrationAPIRequest(t, a, "POST", outputPath+"/start", `{"request_id":"restart-ack-output"}`, 202)
	sessionAcceptanceReceipt(t, f.service, core.ID(started["id"].(string)), core.CommandSucceeded)
	sessionAcceptanceWait(t, func() bool {
		// Ожидает новый запуск выхода с независимыми счётчиками соединений и ACK.

		m = metrics()
		return receiver.accepted.Load() == 3 && m != nil && publicTelemetryObject(t, m["counters"])["connections"] == "1"
	})
	ackTelemetryAssert(t, m, "0", "0", "0", true)
	// stop содержит квитанцию остановки сессии после завершения проверки ACK.
	stop := integrationAPIRequest(t, a, "POST", path+"/stop", `{"request_id":"stop-ack-metrics"}`, 202)
	sessionAcceptanceReceipt(t, f.service, core.ID(stop["id"].(string)), core.CommandSucceeded)
	sessionAcceptanceWait(t, func() bool {
		// Ожидает недоступность оперативных метрик после остановки Session.

		// response содержит ответ метрик после остановки для проверки их недоступности.
		response := integrationAPIRequest(t, a, "GET", path+"/metrics", "", 200)
		return response["availability"] == "unavailable" && response["snapshot"] == nil
	})
}

// Проверяет точное представление больших счётчиков ACK, null до первой аномалии и перевод времени в
// UTC. Повторное измерение и запись от заменённого наблюдателя отклоняются, а замена владельца удаляет
// прежние метрики.
func TestACKTelemetryAcceptanceExactLargeValuesUTCNullAndStaleOwner(t *testing.T) {
	// ctx используется для регистрации наблюдателей и чтения тестового состояния телеметрии.
	ctx := context.Background()
	// f предоставляет выход сессии для прямой записи синтетических больших значений ACK.
	f := newPublisherPipeline(t, []core.GroupProfile{publisherProfile(core.CodecAV1, 640, 360)}, []string{"rtmp://receiver.invalid/live"}, []string{"synthetic"}, []int{0})
	// err сообщает об ошибке закрытия исходного наблюдателя фикстуры перед настройкой телеметрии.
	if err := f.observer.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// limits задаёт конечные бюджеты событий и GPU, а также срок устаревания медиаметрик для теста.
	limits := core.TelemetryLimits{Events: core.EventLimits{MaxEvents: 32, MaxBytes: 1 << 20, MaxEventBytes: 65536, MaxPageSize: 8}, MediaStaleAfter: time.Minute, MaxGPUDevices: 1, MaxGPUUUIDBytes: 64, MaxGPUBytes: 4096}
	// err сообщает об отказе применения тестовых ограничений телеметрии.
	if err := f.service.ConfigureTelemetry(ctx, limits); err != nil {
		t.Fatal(err)
	}
	// receipts содержит квитанции сессии для поиска команды запуска выхода, а err сообщает об ошибке
	// их чтения.
	receipts, err := f.service.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 32, SessionID: f.parent.SessionID}})
	if err != nil {
		t.Fatal(err)
	}
	// fence сохраняет разрешение команды запуска выхода для регистрации точного владельца его
	// метрик.
	var fence core.ExecutionFence
	// receipt обозначает очередную квитанцию, среди которых ищется запуск выхода.
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
	// observer записывает метрики от текущего владельца выхода до его замены.
	observer, err := f.service.OpenOutputObserver(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает первоначального наблюдателя выхода после проверки точности больших значений ACK.
		_ = observer.Close(ctx)
	})
	// a предоставляет публичные маршруты телеметрии поверх сервиса фикстуры.
	a := publicTelemetryAPI(t, f.service)
	// server обслуживает локальную подписку SSE для проверки точности сериализации ACK.
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	// path задаёт маршрут чтения метрик проверяемой сессии.
	path := "/sessions/" + string(f.parent.SessionID) + "/metrics"
	// local задаёт синтетическое время со смещением UTC+3 для проверки нормализации в UTC.
	local := time.Date(2026, 11, 8, 18, 35, 27, 123456789, time.FixedZone("synthetic-offset", 3*60*60))
	// value содержит последовательно публикуемые измерения выхода, включая большие счётчики ACK.
	value := core.OutputMetrics{SampledAt: local}
	if err = observer.ObserveMetrics(ctx, value); err != nil {
		t.Fatal(err)
	}
	// m содержит публичное представление метрик выхода для проверки точных десятичных строк.
	m := ackTelemetryOutput(t, integrationAPIRequest(t, a, "GET", path, "", 200))
	ackTelemetryAssert(t, m, "0", "0", "0", true)
	// anchor содержит курсор снимка перед публикацией больших значений ACK.
	anchor := integrationAPIRequest(t, a, "GET", "/events/snapshot", "", 200)
	// stream читает события после исходного снимка для проверки больших значений ACK в SSE.
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
	// frame содержит событие измерений с большими счётчиками, а ok подтверждает успешное чтение
	// события из подписки.
	frame, ok := stream.next(t)
	if !ok || frame.data["output"] == nil {
		t.Fatal("SSE потерял метрики", frame)
	}
	ackTelemetryAssert(t, publicTelemetryObject(t, frame.data["output"]), "18446744073709551615", "9007199254740993", "18446744073709551615", false)
	if err = observer.ObserveMetrics(ctx, value); !errors.Is(err, core.ErrStale) {
		t.Fatal("повторное измерение принято", err)
	}
	// replacement заменяет первоначального наблюдателя выхода и отзывает его право публиковать
	// метрики.
	replacement, err := f.service.OpenOutputObserver(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает нового наблюдателя, заменившего прежнего владельца метрик выхода.
		_ = replacement.Close(ctx)
	})
	value.SampledAt = value.SampledAt.Add(time.Second)
	if err = observer.ObserveMetrics(ctx, value); !errors.Is(err, core.ErrStale) {
		t.Fatal("отозванный владелец обновил метрики", err)
	}
	// snapshot содержит оперативное состояние после замены наблюдателя для проверки удаления прежних
	// метрик.
	snapshot, err := f.service.RuntimeSnapshot(ctx, core.RuntimeSessionFence{SessionID: f.parent.SessionID, Incarnation: f.parent.Incarnation, Lifecycle: f.parent.Lifecycle})
	if err != nil || len(snapshot.Outputs) != 1 || snapshot.Outputs[0].Metrics != nil {
		t.Fatal("замена владельца сохранила старые метрики", snapshot, err)
	}
	stream.close(t)
}
