package gpu

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// testConfig задаёт конечные бюджеты двух устройств и короткие сроки свежести и завершения для
// изолированных сценариев.
func testConfig() Config {
	return Config{MaxDevices: 2, MaxUUIDBytes: 64, MaxSnapshotBytes: 4096, StaleAfter: 3 * time.Second, ShutdownTimeout: time.Second}
}

// controlledProvider подставляет исходы нативного жизненного цикла и опроса с атомарными счётчиками
// вызовов.
type controlledProvider struct {
	// initReason задаёт результат Init, возвращаемый сборщику.
	initReason Reason
	// stopReason задаёт результат единственного нативного Shutdown.
	stopReason Reason
	// poll выполняет управляемый тестом сценарий получения измерений с переданными Limits.
	poll func(Limits) Sample
	// starts считает вызовы Init для проверки однократности.
	starts atomic.Int32
	// polls считает вызовы Poll для проверки последовательности и отсутствия повторов при зависании.
	polls atomic.Int32
	// stops считает вызовы Shutdown для проверки освобождения после завершения Poll.
	stops atomic.Int32
}

// Init учитывает запуск управляемого источника и возвращает настроенную причину доступности.
func (p *controlledProvider) Init() Reason { p.starts.Add(1); return p.initReason }

// Poll учитывает обращение и передаёт полученные пределы тестовому сценарию опроса.
func (p *controlledProvider) Poll(l Limits) Sample {
	p.polls.Add(1)
	return p.poll(l)
}

// Shutdown учитывает физическое освобождение источника и возвращает настроенный результат.
func (p *controlledProvider) Shutdown() Reason { p.stops.Add(1); return p.stopReason }

// successfulDevice создаёт полный набор известных процентов, байтов памяти и периодов NVENC/NVDEC для
// проверки единиц.
func successfulDevice(uuid string) DeviceSample {
	// d собирает полный снимок одного устройства с заданным UUID.
	d := DeviceSample{UUID: uuid}
	// values задаёт четыре процента и три объёма памяти в байтах для проверки единиц.
	values := [7]uint64{73, 28, 44, 19, 17179869184, 6442450944, 10737418240}
	// i указывает показатель в values; r указывает соответствующее исходное измерение устройства.
	for i, r := range readings(&d) {
		*r = Reading{Value: values[i], Reason: Available}
	}
	d.Encoder.SamplingPeriodUS = 123456
	d.Decoder.SamplingPeriodUS = 654321
	return d
}

// Проверяет независимость копий Snapshot и сохранение последнего значения и времени успеха
// неподдержанного поля при обновлении остальных.
func TestCollectorPartialAvailabilityPreservesLastSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет сохранение последнего успешного значения при частичной недоступности GPU в управляемом
		// времени.

		// publications передаёт последовательные снимки сборщика тесту без удержания его
		// блокировки.
		publications := make(chan Snapshot, 4)
		// polls различает первый успешный опрос и последующее частично недоступное измерение.
		var polls int
		// p задаёт управляемый источник с изменяемой доступностью кодировщика.
		p := &controlledProvider{initReason: Available, stopReason: Available}
		p.poll = func(l Limits) Sample {
			// При повторном опросе делает кодировщик недоступным, одновременно обновляя доступную загрузку GPU.

			if l.MaxDevices != 2 || l.MaxUUIDBytes != 64 {
				panic("Provider не получил ограничения")
			}
			polls++
			// d содержит текущий успешный набор, который при повторном опросе получает отказ
			// Encoder и новое значение GPU.
			d := successfulDevice("GPU-synthetic")
			if polls > 1 {
				d.Encoder = Reading{Reason: NotSupported}
				d.GPU.Value = 81
			}
			return Sample{Devices: []DeviceSample{d}, Reason: Available}
		}
		// removed считает вызовы внешнего удаления наблюдения после завершения сборщика.
		var removed atomic.Int32
		// c содержит проверяемый Collector; err содержит отказ его запуска с управляемым
		// источником.
		c, err := Start(testConfig(), p, func(s Snapshot) {
			// Передаёт опубликованные снимки в канал для проверки последовательных измерений.
			publications <- s
		}, func() {
			// Считает удаления владельца GPU для проверки однократного освобождения.
			removed.Add(1)
		})
		if err != nil {
			t.Fatal(err)
		}
		// first содержит первый полный успешный снимок для проверки значений и независимости
		// копии.
		first := <-publications
		if len(first.Devices) != 1 || first.Devices[0].GPU.Value != 73 || !first.Devices[0].GPU.Available || first.Devices[0].MemoryTotalBytes.Value != 17179869184 || first.Devices[0].Encoder.SamplingPeriodUS != 123456 || first.Devices[0].Decoder.SamplingPeriodUS != 654321 {
			t.Fatalf("единицы или значения изменены: %+v", first)
		}
		// firstAt сохраняет время первого успеха кодировщика для проверки отказа без обновления
		// времени.
		firstAt := first.Devices[0].Encoder.LastSuccess
		first.Devices[0].GPU.Value = 0
		if c.Snapshot().Devices[0].GPU.Value != 73 {
			t.Fatal("публичный Snapshot изменяет Collector")
		}
		time.Sleep(PollInterval)
		// second содержит снимок повторного опроса с частично неподдержанным кодировщиком.
		second := <-publications
		// device содержит единственное устройство повторного снимка для сравнения независимой
		// доступности полей.
		device := second.Devices[0]
		if device.Encoder.Available || device.Encoder.Reason != NotSupported || device.Encoder.Value != 44 || !device.Encoder.LastSuccess.Equal(firstAt) || !device.GPU.Available || device.GPU.Value != 81 || !device.GPU.LastSuccess.After(firstAt) {
			t.Fatalf("частичный NOT_SUPPORTED обнулил измерения: %+v", device)
		}
		// err содержит результат полного закрытия сборщика после двух опросов.
		if err := c.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if removed.Load() != 1 || p.starts.Load() != 1 || p.stops.Load() != 1 || len(c.Snapshot().Devices) != 0 || c.Snapshot().Reason != Closed {
			t.Fatal("владелец или его значения не освобождены")
		}
	})
}

// Проверяет, что зависший Poll удерживает владельца, не перекрывается Shutdown и не публикует поздний
// результат после Close.
func TestPendingPollCannotOverlapShutdownOrPublishAfterClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет отсутствие конкурентного Shutdown и поздней публикации при задержанном Poll.

		// entered подтверждает начало задержанного Poll; release разрешает вернуть уже поздний
		// результат.
		entered, release := make(chan struct{}), make(chan struct{})
		// p моделирует Poll, который завершается только после начала Close.
		p := &controlledProvider{initReason: Available, stopReason: Available, poll: func(Limits) Sample {
			// Удерживает Poll до разрешения теста, возвращая успешное измерение уже после начала закрытия.

			close(entered)
			<-release
			return Sample{Reason: Available, Devices: []DeviceSample{successfulDevice("GPU-pending")}}
		}}
		// published считает внешние публикации позднего результата; removed считает снятия
		// владельца после завершения Poll.
		var published, removed atomic.Int32
		// c содержит сборщик с удерживаемым опросом; err содержит отказ его запуска.
		c, err := Start(testConfig(), p, func(Snapshot) {
			// Считает публикации, которые должны отсутствовать после закрытия допуска.
			published.Add(1)
		}, func() {
			// Считает удаления, которые допустимы только после возврата Poll.
			removed.Add(1)
		})
		if err != nil {
			t.Fatal(err)
		}
		<-entered
		for range 2 {
			// err содержит очередной результат Close при удерживаемом Poll, ожидаются
			// ErrPending и истечение срока.
			if err := c.Close(context.Background()); !errors.Is(err, ErrPending) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		}
		if p.stops.Load() != 0 || p.polls.Load() != 1 || removed.Load() != 0 {
			t.Fatal("владелец освобождён до возврата нативного вызова")
		}
		select {
		case <-c.Done():
			t.Fatal("ложное подтверждение завершения")
		default:
		}
		close(release)
		// err содержит результат Close после возврата позднего Poll, который уже не должен
		// публиковаться.
		if err := c.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if p.stops.Load() != 1 || removed.Load() != 1 || published.Load() != 0 {
			t.Fatal("поздний Sample опубликован после закрытия допуска")
		}
	})
}

// Проверяет, что зависший опрос делает поле устаревшим, а последующая ошибка не обновляет время
// последнего успеха.
func TestFailureAndStaleDoNotRefreshSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет, что задержанный опрос и последующая ошибка не обновляют время последнего успеха.

		// publications сохраняет успешный и ошибочный снимки для сравнения времени последнего
		// успеха.
		publications := make(chan Snapshot, 4)
		// entered отмечает начало второго удерживаемого опроса; release разрешает ему вернуть
		// DeviceLost.
		entered, release := make(chan struct{}), make(chan struct{})
		// calls выбирает первый успех, задержанный второй опрос и последующие отказы устройства.
		var calls int
		// p задаёт последовательность успешного измерения и задержанной потери устройства.
		p := &controlledProvider{initReason: Available, stopReason: Available, poll: func(Limits) Sample {
			// Возвращает первое успешное измерение, затем задерживает опрос и сообщает потерю устройства.

			calls++
			if calls == 1 {
				return Sample{Reason: Available, Devices: []DeviceSample{successfulDevice("GPU-stale")}}
			}
			if calls == 2 {
				close(entered)
				<-release
			}
			return Sample{Reason: DeviceLost}
		}}
		// c содержит сборщик контролируемой свежести; err содержит отказ его запуска.
		c, err := Start(testConfig(), p, func(s Snapshot) {
			// Сохраняет последовательные публикации для сравнения успешного и ошибочного снимков.
			publications <- s
		}, func() {
			// Оставляет удаление внешнего наблюдателя пустым, поскольку проверяется только локальная свежесть
			// снимка.
		})
		if err != nil {
			t.Fatal(err)
		}
		// first содержит исходное успешное значение и время для сравнения после задержки.
		first := <-publications
		time.Sleep(PollInterval)
		<-entered
		time.Sleep(2 * time.Second)
		// stale содержит поле после превышения срока свежести при ещё задержанном Poll.
		stale := c.Snapshot().Devices[0].GPU
		if stale.Available || stale.Reason != Stale || stale.Value != 73 || !stale.LastSuccess.Equal(first.Devices[0].GPU.LastSuccess) {
			t.Fatalf("свежесть зависшего Poll неверна: %+v", stale)
		}
		close(release)
		// failed содержит опубликованный снимок потери устройства после снятия блокировки.
		failed := <-publications
		// field содержит GPU-показатель ошибочного снимка для проверки сохранённого LastSuccess.
		if field := failed.Devices[0].GPU; field.Available || field.Reason != DeviceLost || !field.LastSuccess.Equal(stale.LastSuccess) {
			t.Fatalf("ошибка продлила успех: %+v", field)
		}
		// err содержит итог закрытия сборщика после проверки stale и DeviceLost.
		if err := c.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

// Проверяет отклонение неверных и слишком больших снимков без потери прежних значений и изоляцию
// ошибок отдельных полей.
func TestInvalidAndOverBudgetSamplesPreserveValues(t *testing.T) {
	// c содержит локальный Collector для непосредственной проверки приёма неверных снимков.
	c := &Collector{config: testConfig(), current: Snapshot{Reason: Available}, publish: func(Snapshot) {
		// Оставляет внешнюю публикацию пустой при непосредственной проверке приёма недопустимых измерений.
	}}
	// d задаёт последнее корректное устройство, чьи значения должны сохраниться при последующих
	// отказах.
	d := successfulDevice("GPU-1")
	c.accept(Sample{Reason: Available, Devices: []DeviceSample{d}})
	// sample содержит очередной недопустимый снимок с дублями, пустым UUID, превышением числа
	// устройств или неизвестной причиной.
	for _, sample := range []Sample{
		{Reason: Available, Devices: []DeviceSample{d, d}},
		{Reason: Available, Devices: []DeviceSample{{UUID: ""}}},
		{Reason: Available, Devices: []DeviceSample{d, d, d}},
		{Reason: Reason("arbitrary unsafe reason")},
	} {
		c.accept(sample)
		// got содержит состояние после отклонения снимка для проверки сохранённого значения с
		// недоступным статусом.
		if got := c.Snapshot(); len(got.Devices) != 1 || got.Devices[0].GPU.Value != 73 || got.Devices[0].GPU.Available {
			t.Fatalf("отклонённый Sample изменил значение: %+v", got)
		}
	}
	c.config.MaxSnapshotBytes = snapshotBytes + deviceBytes
	c.accept(Sample{Reason: Available, Devices: []DeviceSample{d}})
	if c.Snapshot().Reason != BudgetExceeded {
		t.Fatal("байтовый бюджет не соблюдён")
	}
	c.config = testConfig()
	d.GPU.Value = 101
	d.Memory.SampledAt = time.Now().Add(time.Second)
	c.accept(Sample{Reason: Available, Devices: []DeviceSample{d}})
	// s содержит снимок после отдельных недопустимых полей для проверки изоляции от доступного
	// Decoder.
	s := c.Snapshot()
	if s.Devices[0].GPU.Reason != InvalidSample || s.Devices[0].GPU.Value != 73 || s.Devices[0].Memory.Reason != InvalidSample || !s.Devices[0].Decoder.Available {
		t.Fatalf("неверное значение не изолировано: %+v", s)
	}
	c.accept(Sample{Reason: Available})
	if len(c.Snapshot().Devices) != 0 {
		t.Fatal("отсутствующие GPU остались текущими")
	}
}

// Проверяет недоступный Init, однократный Shutdown и сохранение ошибки освобождения при повторном
// Close.
func TestUnavailableInitShutdownFailureAndRepeatedLifecycle(t *testing.T) {
	for range 3 {
		// published передаёт первый снимок недоступности после ошибочного Init.
		published := make(chan Snapshot, 1)
		// p задаёт отказ Init и Shutdown и запрещает любой последующий Poll.
		p := &controlledProvider{initReason: LibraryUnavailable, stopReason: ProviderError, poll: func(Limits) Sample {
			// Фиксирует запрещённый Poll после неудачного Init.
			panic("Poll после ошибки Init")
		}}
		// c содержит сборщик с недоступным источником; err содержит отказ его запуска.
		c, err := Start(testConfig(), p, func(s Snapshot) {
			// Передаёт снимок недоступности в канал для проверки ошибки инициализации.
			published <- s
		}, func() {
			// Оставляет внешнее удаление пустым при проверке повторных нативных жизненных циклов.
		})
		if err != nil {
			t.Fatal(err)
		}
		// s содержит полученную недоступность библиотеки без фиктивных устройств и нулевых
		// измерений.
		if s := <-published; s.Reason != LibraryUnavailable || len(s.Devices) != 0 {
			t.Fatal("недоступный NVML подменён нулями")
		}
		// err содержит результат первой остановки с ожидаемым ErrShutdown.
		if err := c.Close(context.Background()); !errors.Is(err, ErrShutdown) {
			t.Fatal(err)
		}
		// err содержит результат повторной остановки для проверки сохранения ErrShutdown без
		// повторного нативного вызова.
		if err := c.Close(context.Background()); !errors.Is(err, ErrShutdown) || p.starts.Load() != 1 || p.stops.Load() != 1 {
			t.Fatal("повторное закрытие повторяет нативный вызов")
		}
	}
}

// Проверяет недопустимые настройки, отсутствие Provider и ошибку ErrPending с отменённым контекстом до
// фактического завершения.
func TestConfigValidationAndCanceledClose(t *testing.T) {
	// c задаёт очередную недопустимую конфигурацию сборщика.
	for _, c := range []Config{{}, {MaxDevices: math.MaxInt, MaxUUIDBytes: 64, MaxSnapshotBytes: 4096, StaleAfter: time.Second, ShutdownTimeout: time.Second}} {
		// err содержит отказ Start с недопустимой конфигурацией до получения владельца.
		if _, err := Start(c, NativeProvider(), func(Snapshot) {
			// Оставляет публикацию пустой: некорректная конфигурация должна отклоняться до запуска сборщика.
		}, func() {
			// Оставляет удаление пустым: некорректная конфигурация должна отклоняться до появления владельца.
		}); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	// err содержит отказ Start без Provider.
	if _, err := Start(testConfig(), nil, func(Snapshot) {
		// Оставляет публикацию пустой при проверке отказа отсутствующему Provider.
	}, func() {
		// Оставляет удаление пустым при проверке отказа отсутствующему Provider.
	}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		// Проверяет возврат ErrPending при отменённом ожидании Close до окончания Poll.

		// entered подтверждает начало Poll; release удерживает опрос до проверки отменённого
		// ожидания.
		entered, release := make(chan struct{}), make(chan struct{})
		// p моделирует успешный нативный источник с задержанным Poll для отмены Close.
		p := &controlledProvider{initReason: Available, stopReason: Available, poll: func(Limits) Sample {
			// Удерживает опрос до явного разрешения теста отменённого Close.
			close(entered)
			<-release
			return Sample{Reason: Available}
		}}
		// c содержит сборщик отменяемого ожидания; err содержит отказ его запуска.
		c, err := Start(testConfig(), p, func(Snapshot) {
			// Не обрабатывает значения, поскольку проверяется только отмена ожидания и завершение сборщика.
		}, func() {
			// Не удаляет внешнее состояние, поскольку тест владеет только локальным сборщиком.
		})
		if err != nil {
			t.Fatal(err)
		}
		<-entered
		// ctx задаёт немедленно отменённый контекст Close; cancel отменяет его перед вызовом.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// err содержит результат отменённого Close с ErrPending при ещё живом работнике.
		if err := c.Close(ctx); !errors.Is(err, ErrPending) || !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		close(release)
		// err содержит итог повторного Close после разрешения Poll завершиться.
		if err := c.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}
