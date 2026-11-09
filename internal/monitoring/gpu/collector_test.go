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

func testConfig() Config {
	return Config{MaxDevices: 2, MaxUUIDBytes: 64, MaxSnapshotBytes: 4096, StaleAfter: 3 * time.Second, ShutdownTimeout: time.Second}
}

type controlledProvider struct {
	initReason Reason
	stopReason Reason
	poll       func(Limits) Sample
	starts     atomic.Int32
	polls      atomic.Int32
	stops      atomic.Int32
}

func (p *controlledProvider) Init() Reason { p.starts.Add(1); return p.initReason }
func (p *controlledProvider) Poll(l Limits) Sample {
	p.polls.Add(1)
	return p.poll(l)
}
func (p *controlledProvider) Shutdown() Reason { p.stops.Add(1); return p.stopReason }

func successfulDevice(uuid string) DeviceSample {
	d := DeviceSample{UUID: uuid}
	values := [7]uint64{73, 28, 44, 19, 17179869184, 6442450944, 10737418240}
	for i, r := range readings(&d) {
		*r = Reading{Value: values[i], Reason: Available}
	}
	d.Encoder.SamplingPeriodUS = 123456
	d.Decoder.SamplingPeriodUS = 654321
	return d
}

func TestCollectorPartialAvailabilityPreservesLastSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		publications := make(chan Snapshot, 4)
		var polls int
		p := &controlledProvider{initReason: Available, stopReason: Available}
		p.poll = func(l Limits) Sample {
			if l.MaxDevices != 2 || l.MaxUUIDBytes != 64 {
				panic("Provider не получил ограничения")
			}
			polls++
			d := successfulDevice("GPU-synthetic")
			if polls > 1 {
				d.Encoder = Reading{Reason: NotSupported}
				d.GPU.Value = 81
			}
			return Sample{Devices: []DeviceSample{d}, Reason: Available}
		}
		var removed atomic.Int32
		c, err := Start(testConfig(), p, func(s Snapshot) { publications <- s }, func() { removed.Add(1) })
		if err != nil {
			t.Fatal(err)
		}
		first := <-publications
		if len(first.Devices) != 1 || first.Devices[0].GPU.Value != 73 || !first.Devices[0].GPU.Available || first.Devices[0].MemoryTotalBytes.Value != 17179869184 || first.Devices[0].Encoder.SamplingPeriodUS != 123456 || first.Devices[0].Decoder.SamplingPeriodUS != 654321 {
			t.Fatalf("единицы или значения изменены: %+v", first)
		}
		firstAt := first.Devices[0].Encoder.LastSuccess
		first.Devices[0].GPU.Value = 0
		if c.Snapshot().Devices[0].GPU.Value != 73 {
			t.Fatal("публичный Snapshot изменяет Collector")
		}
		time.Sleep(PollInterval)
		second := <-publications
		device := second.Devices[0]
		if device.Encoder.Available || device.Encoder.Reason != NotSupported || device.Encoder.Value != 44 || !device.Encoder.LastSuccess.Equal(firstAt) || !device.GPU.Available || device.GPU.Value != 81 || !device.GPU.LastSuccess.After(firstAt) {
			t.Fatalf("частичный NOT_SUPPORTED обнулил измерения: %+v", device)
		}
		if err := c.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if removed.Load() != 1 || p.starts.Load() != 1 || p.stops.Load() != 1 || len(c.Snapshot().Devices) != 0 || c.Snapshot().Reason != Closed {
			t.Fatal("владелец или его значения не освобождены")
		}
	})
}

func TestPendingPollCannotOverlapShutdownOrPublishAfterClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		p := &controlledProvider{initReason: Available, stopReason: Available, poll: func(Limits) Sample {
			close(entered)
			<-release
			return Sample{Reason: Available, Devices: []DeviceSample{successfulDevice("GPU-pending")}}
		}}
		var published, removed atomic.Int32
		c, err := Start(testConfig(), p, func(Snapshot) { published.Add(1) }, func() { removed.Add(1) })
		if err != nil {
			t.Fatal(err)
		}
		<-entered
		for range 2 {
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
		if err := c.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if p.stops.Load() != 1 || removed.Load() != 1 || published.Load() != 0 {
			t.Fatal("поздний Sample опубликован после закрытия допуска")
		}
	})
}

func TestFailureAndStaleDoNotRefreshSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		publications := make(chan Snapshot, 4)
		entered, release := make(chan struct{}), make(chan struct{})
		var calls int
		p := &controlledProvider{initReason: Available, stopReason: Available, poll: func(Limits) Sample {
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
		c, err := Start(testConfig(), p, func(s Snapshot) { publications <- s }, func() {})
		if err != nil {
			t.Fatal(err)
		}
		first := <-publications
		time.Sleep(PollInterval)
		<-entered
		time.Sleep(2 * time.Second)
		stale := c.Snapshot().Devices[0].GPU
		if stale.Available || stale.Reason != Stale || stale.Value != 73 || !stale.LastSuccess.Equal(first.Devices[0].GPU.LastSuccess) {
			t.Fatalf("свежесть зависшего Poll неверна: %+v", stale)
		}
		close(release)
		failed := <-publications
		if field := failed.Devices[0].GPU; field.Available || field.Reason != DeviceLost || !field.LastSuccess.Equal(stale.LastSuccess) {
			t.Fatalf("ошибка продлила успех: %+v", field)
		}
		if err := c.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestInvalidAndOverBudgetSamplesPreserveValues(t *testing.T) {
	c := &Collector{config: testConfig(), current: Snapshot{Reason: Available}, publish: func(Snapshot) {}}
	d := successfulDevice("GPU-1")
	c.accept(Sample{Reason: Available, Devices: []DeviceSample{d}})
	for _, sample := range []Sample{
		{Reason: Available, Devices: []DeviceSample{d, d}},
		{Reason: Available, Devices: []DeviceSample{{UUID: ""}}},
		{Reason: Available, Devices: []DeviceSample{d, d, d}},
		{Reason: Reason("arbitrary unsafe reason")},
	} {
		c.accept(sample)
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
	s := c.Snapshot()
	if s.Devices[0].GPU.Reason != InvalidSample || s.Devices[0].GPU.Value != 73 || s.Devices[0].Memory.Reason != InvalidSample || !s.Devices[0].Decoder.Available {
		t.Fatalf("неверное значение не изолировано: %+v", s)
	}
	c.accept(Sample{Reason: Available})
	if len(c.Snapshot().Devices) != 0 {
		t.Fatal("отсутствующие GPU остались текущими")
	}
}

func TestUnavailableInitShutdownFailureAndRepeatedLifecycle(t *testing.T) {
	for range 3 {
		published := make(chan Snapshot, 1)
		p := &controlledProvider{initReason: LibraryUnavailable, stopReason: ProviderError, poll: func(Limits) Sample { panic("Poll после ошибки Init") }}
		c, err := Start(testConfig(), p, func(s Snapshot) { published <- s }, func() {})
		if err != nil {
			t.Fatal(err)
		}
		if s := <-published; s.Reason != LibraryUnavailable || len(s.Devices) != 0 {
			t.Fatal("недоступный NVML подменён нулями")
		}
		if err := c.Close(context.Background()); !errors.Is(err, ErrShutdown) {
			t.Fatal(err)
		}
		if err := c.Close(context.Background()); !errors.Is(err, ErrShutdown) || p.starts.Load() != 1 || p.stops.Load() != 1 {
			t.Fatal("повторное закрытие повторяет нативный вызов")
		}
	}
}

func TestConfigValidationAndCanceledClose(t *testing.T) {
	for _, c := range []Config{{}, {MaxDevices: math.MaxInt, MaxUUIDBytes: 64, MaxSnapshotBytes: 4096, StaleAfter: time.Second, ShutdownTimeout: time.Second}} {
		if _, err := Start(c, NativeProvider(), func(Snapshot) {}, func() {}); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if _, err := Start(testConfig(), nil, func(Snapshot) {}, func() {}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		p := &controlledProvider{initReason: Available, stopReason: Available, poll: func(Limits) Sample { close(entered); <-release; return Sample{Reason: Available} }}
		c, err := Start(testConfig(), p, func(Snapshot) {}, func() {})
		if err != nil {
			t.Fatal(err)
		}
		<-entered
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := c.Close(ctx); !errors.Is(err, ErrPending) || !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		close(release)
		if err := c.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}
