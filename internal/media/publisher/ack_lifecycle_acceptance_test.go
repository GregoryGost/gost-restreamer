package publisher

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
)

// Снимок транспорта намеренно повторяет уже переданное событие: Publisher не должен суммировать его второй раз.
// ackAcceptanceConnection дополняет условный транспорт повторяемым снимком ACK и обработчиком перед
// возвратом Wait.
type ackAcceptanceConnection struct {
	// testConnection предоставляет базовые операции записи, закрытия и управляемого ожидания.
	*testConnection
	// snapshot задаёт транспортные накопления, уже переданные обработчиком ACK, для проверки
	// отсутствия двойного счёта.
	snapshot rtmp.OutboundSnapshot
	// beforeJoin позволяет передать позднее событие непосредственно перед подтверждением завершения
	// Wait.
	beforeJoin func()
}

// Snapshot возвращает заданные накопления транспорта, в том числе уже переданные через обработчик
// события ACK.
func (c *ackAcceptanceConnection) Snapshot() rtmp.OutboundSnapshot { return c.snapshot }

// Wait ждёт завершения базового соединения и затем выполняет управляемый обработчик перед возвратом.
func (c *ackAcceptanceConnection) Wait(ctx context.Context) error {
	// err сообщает об отказе ожидания базового соединения до вызова beforeJoin.
	if err := c.testConnection.Wait(ctx); err != nil {
		return err
	}
	if c.beforeJoin != nil {
		c.beforeJoin()
	}
	return nil
}

// Проверяет сохранение событий ACK при неудачном согласовании, переподключении и завершении,
// отсутствие повторного счёта из Snapshot и нулевые накопления нового Publisher.
func TestACKLifecycleAcceptanceFailedStartupReconnectAndNewRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет накопление ACK-событий при отказе согласования, переподключении и позднем событии перед
		// Wait; новый Publisher должен начать с нулевых метрик.

		// router содержит Router для проверки накоплений ACK на протяжении нескольких сетевых
		// попыток.
		router, _, _, _ := testRouter(t)
		// origin задаёт исходное время последовательности контрольных ACK-событий.
		origin := time.Now().UTC()
		// attempts считает вызовы подключения для выбора ошибки согласования и двух успешных
		// соединений.
		attempts := 0
		// first содержит первое успешное соединение со снимком уже учтённого ACK-события.
		first := &ackAcceptanceConnection{testConnection: newTestConnection(), snapshot: rtmp.OutboundSnapshot{AckAheadEventsTotal: 1, AckAheadBytesLast: 8, AckAheadBytesMax: 8, AckLastAnomalyAt: origin.Add(time.Second)}}
		// second содержит следующее успешное соединение после сетевого разрыва.
		second := &ackAcceptanceConnection{testConnection: newTestConnection()}
		// p выполняет отказ согласования, два успешных соединения и накопление ACK через
		// установленный обработчик.
		p, _ := testPublisher(t, router, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			// Вводит ACK до ошибки первого согласования, затем выдаёт два соединения с отдельными событиями,
			// сохраняя установленный OnACKAnomaly.

			attempts++
			if o.OnACKAnomaly == nil {
				t.Error("OnACKAnomaly не установлен до DialOutbound")
				return nil, errors.New("нет OnACKAnomaly")
			}
			switch attempts {
			case 1:
				o.OnACKAnomaly(4, origin)
				return nil, errors.New("согласование прервано после ACK")
			case 2:
				first.opts = o
				o.OnACKAnomaly(8, origin.Add(time.Second))
				return first, nil
			default:
				second.opts = o
				o.OnACKAnomaly(2, origin.Add(3*time.Second))
				return second, nil
			}
		})
		first.beforeJoin = func() {
			// Передаёт позднее ACK-событие первого успешного соединения непосредственно перед подтверждением его
			// Wait.
			first.opts.OnACKAnomaly(6, origin.Add(2*time.Second))
		}
		startTestPublisher(p)
		synctest.Wait()
		// s содержит снимок после ACK неуспешного согласования до первого успешного соединения.
		if s := p.Snapshot(); s.AckAheadEventsTotal != 1 || s.AckAheadBytesLast != 4 || s.Connections != 0 {
			t.Fatalf("событие установления соединения потеряно: %+v", s)
		}
		time.Sleep(p.options.RetryInitial)
		synctest.Wait()
		for range 3 {
			// s содержит повторно читаемый снимок первого успешного соединения для проверки
			// отсутствия двойного учёта ACK.
			if s := p.Snapshot(); s.AckAheadEventsTotal != 2 || s.AckAheadBytesLast != 8 || s.AckAheadBytesMax != 8 || s.Connections != 1 {
				t.Fatalf("быстрые попытки/повторный Snapshot: %+v", s)
			}
		}
		first.mu.Lock()
		first.err = errors.New("разрыв сети")
		first.mu.Unlock()
		_ = first.Close()
		synctest.Wait()
		time.Sleep(2 * p.options.RetryInitial)
		synctest.Wait()
		// s содержит снимок после переподключения и позднего ACK первого транспорта.
		if s := p.Snapshot(); s.AckAheadEventsTotal != 4 || s.AckAheadBytesLast != 2 || s.AckAheadBytesMax != 8 || s.AckLastAnomalyAt != origin.Add(3*time.Second) || s.Connections != 2 || s.Reconnects != 1 {
			t.Fatalf("переподключение или завершение потеряло либо повторило события: %+v", s)
		}
		// err сообщает об отказе остановки Publisher после проверки накоплений ACK.
		if err := p.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !p.Result().NetworkJoined {
			t.Fatal("Stop не дождался завершения OnACKAnomaly")
		}
		// fresh содержит новый Publisher для проверки сброса измерений между запусками.
		fresh, _ := testPublisher(t, router, func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error) {
			// Возвращает новое условное соединение для проверки отсутствия у нового запуска накоплений ACK.

			return newTestConnection(), nil
		})
		// s содержит снимок нового Publisher с нулевыми накоплениями ACK.
		if s := fresh.Snapshot(); s.AckAheadEventsTotal != 0 || s.AckAheadBytesLast != 0 || s.AckAheadBytesMax != 0 || !s.AckLastAnomalyAt.IsZero() {
			t.Fatalf("новый запуск унаследовал метрики: %+v", s)
		}
	})
}

// Проверяет согласованность числа, последнего и максимального опережения ACK и времени при
// конкурентной записи событий и чтении снимков.
func TestACKLifecycleAcceptanceConcurrentSnapshotsRemainCoherent(t *testing.T) {
	// router содержит Router для конкурентной проверки согласованности ACK.
	router, _, _, _ := testRouter(t)
	// options передаёт настройки активной попытки с её обработчиком OnACKAnomaly тесту.
	options := make(chan rtmp.OutboundOptions, 1)
	// c содержит условное соединение конкурентного сценария ACK.
	c := newTestConnection()
	// p выполняет сетевую попытку с обработчиком, вызываемым параллельно чтению снимков.
	p, _ := testPublisher(t, router, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
		// Сохраняет настройки и возвращает условное соединение, передавая OnACKAnomaly основному тесту через
		// options.

		c.opts = o
		options <- o
		return c, nil
	})
	startTestPublisher(p)
	// o содержит переданные транспорту настройки с действующим обработчиком OnACKAnomaly.
	o := <-options
	if o.OnACKAnomaly == nil {
		t.Fatal("нет OnACKAnomaly")
	}
	// origin задаёт исходное время, относительно которого кодируются последовательные номера
	// ACK-событий.
	origin := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// wg учитывает горутину последовательной отправки тысячи ACK-событий.
	var wg sync.WaitGroup
	wg.Go(func() {
		// Последовательно передаёт 1000 ACK-событий с согласованными размером и временем параллельно чтению
		// снимков.

		// n задаёт номер контрольного события, одновременно используемый как байтовое опережение
		// и смещение времени в наносекундах.
		for n := uint64(1); n <= 1000; n++ {
			o.OnACKAnomaly(n, origin.Add(time.Duration(n)*time.Nanosecond))
		}
	})
	for range 1000 {
		// s содержит очередной конкурентный снимок согласованных накоплений ACK.
		s := p.Snapshot()
		// n содержит число уже учтённых событий для проверки соответствующих last, max и
		// времени.
		n := s.AckAheadEventsTotal
		if s.AckAheadBytesLast != n || s.AckAheadBytesMax != n || n > 0 && s.AckLastAnomalyAt != origin.Add(time.Duration(n)*time.Nanosecond) {
			t.Fatalf("несогласованный набор ACK: %+v", s)
		}
	}
	wg.Wait()
	// s содержит окончательный снимок после завершения всех тысячи вызовов обработчика.
	if s := p.Snapshot(); s.AckAheadEventsTotal != 1000 || s.AckAheadBytesMax != 1000 {
		t.Fatalf("потеряны вызовы OnACKAnomaly: %+v", s)
	}
}

// Проверяет, что насыщение общего числа записанных байтов не отключает точный учёт новых событий ACK
// через обработчик транспорта.
func TestACKLifecycleAcceptanceSaturationDoesNotDisableCallback(t *testing.T) {
	// router содержит Router для проверки ACK после насыщения общего числа записанных байтов.
	router, _, _, _ := testRouter(t)
	// callback сохраняет установленный перед dial обработчик аномалий ACK даже при отказе
	// согласования.
	var callback func(uint64, time.Time)
	// p содержит Publisher, у которого счётчик WrittenBytes намеренно доводится до насыщения.
	p, _ := testPublisher(t, router, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
		// Сохраняет OnACKAnomaly до отказа согласования, позволяя проверить работу обработчика после насыщения
		// счётчика записанных байтов.

		callback = o.OnACKAnomaly
		return nil, errors.New("согласование прервано")
	})
	// err содержит ожидаемый отказ сетевой попытки после сохранения обработчика ACK.
	if _, err := p.attempt(context.Background()); err == nil {
		t.Fatal("ожидался отказ установления соединения")
	}
	if callback == nil {
		t.Fatal("нет OnACKAnomaly при установлении соединения")
	}
	// Предел W Publisher не должен лишать новые соединения точных наблюдений ACK.
	p.noteWrite(math.MaxInt)
	p.noteWrite(math.MaxInt)
	p.noteWrite(2)
	// at задаёт время первого события ACK после насыщения WrittenBytes.
	at := time.Now().UTC()
	callback(4, at)
	callback(9, at.Add(time.Nanosecond))
	callback(2, at.Add(2*time.Nanosecond))
	// s содержит снимок после трёх ACK-событий для проверки независимости их учёта от насыщенных
	// байтов.
	if s := p.Snapshot(); !s.Saturated || s.WrittenBytes != math.MaxUint64 || s.AckAheadEventsTotal != 3 || s.AckAheadBytesLast != 2 || s.AckAheadBytesMax != 9 || s.AckLastAnomalyAt != at.Add(2*time.Nanosecond) {
		t.Fatalf("насыщение W отключило ACK: %+v", s)
	}
}
