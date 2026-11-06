// Package gpu собирает показатели всего GPU независимо от медиасессий.
package gpu

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"time"
)

const PollInterval = 2 * time.Second

type Reason string

const (
	Available           Reason = "available"
	WarmingUp           Reason = "warming_up"
	NotSupported        Reason = "not_supported"
	UnsupportedPlatform Reason = "unsupported_platform"
	CGODisabled         Reason = "cgo_disabled"
	LibraryUnavailable  Reason = "library_unavailable"
	DriverUnavailable   Reason = "driver_unavailable"
	PermissionDenied    Reason = "permission_denied"
	DeviceLost          Reason = "device_lost"
	ProviderError       Reason = "provider_error"
	InvalidSample       Reason = "invalid_sample"
	BudgetExceeded      Reason = "budget_exceeded"
	Stale               Reason = "stale"
	Busy                Reason = "busy"
	Closed              Reason = "closed"
)

var (
	ErrInvalid  = errors.New("неверная конфигурация сборщика GPU")
	ErrBusy     = errors.New("источник измерений GPU уже имеет владельца")
	ErrPending  = errors.New("завершение сборщика GPU ещё не подтверждено")
	ErrShutdown = errors.New("NVML не подтвердил освобождение ресурсов")
)

type Config struct {
	MaxDevices       int
	MaxUUIDBytes     int
	MaxSnapshotBytes uint64
	StaleAfter       time.Duration
	ShutdownTimeout  time.Duration
}

const snapshotBytes = uint64(128)
const deviceBytes = uint64(512)

func (c Config) Validate() error {
	if c.MaxDevices <= 0 || c.MaxUUIDBytes <= 0 || c.MaxSnapshotBytes < snapshotBytes || c.StaleAfter <= 0 || c.ShutdownTimeout <= 0 || uint64(c.MaxDevices) > uint64(math.MaxInt)/deviceBytes || uint64(c.MaxUUIDBytes) > math.MaxUint64-deviceBytes {
		return ErrInvalid
	}
	return nil
}

type Limits struct {
	MaxDevices   int
	MaxUUIDBytes int
}

// Reading содержит проценты либо байты согласно имени поля; период NVML задан в микросекундах.
type Reading struct {
	Value            uint64
	SamplingPeriodUS uint32
	Reason           Reason
	SampledAt        time.Time
}

type DeviceSample struct {
	UUID                                               string
	GPU, Memory, Encoder, Decoder                      Reading
	MemoryTotalBytes, MemoryUsedBytes, MemoryFreeBytes Reading
}

type Sample struct {
	Devices []DeviceSample
	Reason  Reason
}

// Provider вызывается последовательно одним владельцем, включая неотменяемые нативные вызовы.
// Poll соблюдает Limits до выделения памяти; возвращённый Sample передаётся во владение Collector.
type Provider interface {
	Init() Reason
	Poll(Limits) Sample
	Shutdown() Reason
}

// Field сохраняет LastSuccess и последнее значение при ошибке, не объявляя их свежими.
type Field struct {
	Value            uint64
	SamplingPeriodUS uint32
	Available        bool
	Reason           Reason
	LastSuccess      time.Time
}

type DeviceSnapshot struct {
	UUID                                               string
	GPU, Memory, Encoder, Decoder                      Field
	MemoryTotalBytes, MemoryUsedBytes, MemoryFreeBytes Field
}

type Snapshot struct {
	SampledAt time.Time
	Devices   []DeviceSnapshot
	Reason    Reason
}

// Collector владеет одной последовательностью Init/Poll/Shutdown и точным обработчиком удаления.
type Collector struct {
	config   Config
	provider Provider
	publish  func(Snapshot)
	remove   func()
	mu       sync.Mutex
	current  Snapshot
	closing  bool
	closeErr error
	stop     chan struct{}
	done     chan struct{}
}

// Start не ждёт завершения Init. Обработчики вызываются без удержания блокировок
// и выполняют только короткую операцию с RAM; публикация сохраняет монотонные часы для core.
func Start(c Config, provider Provider, publish func(Snapshot), remove func()) (*Collector, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if provider == nil || publish == nil || remove == nil {
		return nil, ErrInvalid
	}
	if owned, ok := provider.(interface{ claimCollector() bool }); ok && !owned.claimCollector() {
		return nil, ErrBusy
	}
	collector := &Collector{config: c, provider: provider, publish: publish, remove: remove, current: Snapshot{Reason: WarmingUp}, stop: make(chan struct{}), done: make(chan struct{})}
	go collector.run()
	return collector, nil
}

func (c *Collector) Done() <-chan struct{} { return c.done }

func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := freshSnapshot(c.current, time.Now(), c.config.StaleAfter)
	snapshot.SampledAt = snapshot.SampledAt.UTC()
	for i := range snapshot.Devices {
		for _, field := range fields(&snapshot.Devices[i]) {
			field.LastSuccess = field.LastSuccess.UTC()
		}
	}
	return snapshot
}

func (c *Collector) run() {
	defer close(c.done)
	defer func() {
		c.mu.Lock()
		c.current = Snapshot{Reason: Closed}
		c.mu.Unlock()
		c.remove()
	}()
	reason := normalized(c.provider.Init())
	defer func() {
		if normalized(c.provider.Shutdown()) != Available {
			c.mu.Lock()
			c.closeErr = ErrShutdown
			c.mu.Unlock()
		}
	}()
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		if reason == Available {
			c.accept(c.provider.Poll(Limits{MaxDevices: c.config.MaxDevices, MaxUUIDBytes: c.config.MaxUUIDBytes}))
		} else {
			c.accept(Sample{Reason: reason})
		}
		select {
		case <-c.stop:
			return
		case <-ticker.C:
		}
	}
}

func (c *Collector) accept(sample Sample) {
	now := time.Now()
	reason := normalized(sample.Reason)
	if reason == Available {
		reason = c.validateSample(sample)
	}
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return
	}
	if reason != Available {
		c.current.Reason = reason
		c.current.SampledAt = now
		for i := range c.current.Devices {
			for _, field := range fields(&c.current.Devices[i]) {
				field.Available = false
				field.Reason = reason
			}
		}
	} else {
		next := Snapshot{SampledAt: now, Reason: Available, Devices: make([]DeviceSnapshot, len(sample.Devices))}
		for i, device := range sample.Devices {
			previous := c.find(device.UUID)
			next.Devices[i] = DeviceSnapshot{UUID: strings.Clone(device.UUID)}
			out := fields(&next.Devices[i])
			prior := fields(&previous)
			in := readings(&device)
			for j := range out {
				*out[j] = merge(*prior[j], *in[j], now, j < 4)
			}
		}
		c.current = next
	}
	published := freshSnapshot(c.current, now, c.config.StaleAfter)
	c.mu.Unlock()
	c.publish(published)
}

func (c *Collector) find(uuid string) DeviceSnapshot {
	for _, device := range c.current.Devices {
		if device.UUID == uuid {
			return device
		}
	}
	return DeviceSnapshot{}
}

func (c *Collector) validateSample(sample Sample) Reason {
	if len(sample.Devices) > c.config.MaxDevices {
		return BudgetExceeded
	}
	weight := snapshotBytes
	for i, device := range sample.Devices {
		if device.UUID == "" || len(device.UUID) > c.config.MaxUUIDBytes {
			return InvalidSample
		}
		for j := 0; j < i; j++ {
			if sample.Devices[j].UUID == device.UUID {
				return InvalidSample
			}
		}
		cost := deviceBytes + uint64(len(device.UUID))
		if cost > c.config.MaxSnapshotBytes-weight {
			return BudgetExceeded
		}
		weight += cost
	}
	return Available
}

func merge(previous Field, reading Reading, now time.Time, percent bool) Field {
	reason := normalized(reading.Reason)
	if reason == Available && (percent && reading.Value > 100 || reading.SampledAt.After(now)) {
		reason = InvalidSample
	}
	if reason != Available {
		previous.Available = false
		previous.Reason = reason
		return previous
	}
	at := reading.SampledAt
	if at.IsZero() {
		at = now
	}
	return Field{Value: reading.Value, SamplingPeriodUS: reading.SamplingPeriodUS, Available: true, Reason: Available, LastSuccess: at}
}

func freshSnapshot(snapshot Snapshot, now time.Time, staleAfter time.Duration) Snapshot {
	snapshot.Devices = append([]DeviceSnapshot(nil), snapshot.Devices...)
	for i := range snapshot.Devices {
		for _, field := range fields(&snapshot.Devices[i]) {
			if field.Available && now.Sub(field.LastSuccess) >= staleAfter {
				field.Available = false
				field.Reason = Stale
			}
		}
	}
	return snapshot
}

func fields(d *DeviceSnapshot) [7]*Field {
	return [7]*Field{&d.GPU, &d.Memory, &d.Encoder, &d.Decoder, &d.MemoryTotalBytes, &d.MemoryUsedBytes, &d.MemoryFreeBytes}
}

func readings(d *DeviceSample) [7]*Reading {
	return [7]*Reading{&d.GPU, &d.Memory, &d.Encoder, &d.Decoder, &d.MemoryTotalBytes, &d.MemoryUsedBytes, &d.MemoryFreeBytes}
}

func normalized(reason Reason) Reason {
	switch reason {
	case Available, WarmingUp, NotSupported, UnsupportedPlatform, CGODisabled, LibraryUnavailable, DriverUnavailable, PermissionDenied, DeviceLost, ProviderError, InvalidSample, BudgetExceeded, Stale, Busy, Closed:
		return reason
	default:
		return ProviderError
	}
}

// Close не запускает конкурентный Shutdown при зависшем Poll и допускает повторное ожидание.
func (c *Collector) Close(ctx context.Context) error {
	c.mu.Lock()
	if !c.closing {
		c.closing = true
		close(c.stop)
	}
	c.mu.Unlock()
	select {
	case <-c.done:
		return c.shutdownError()
	default:
	}
	timer := time.NewTimer(c.config.ShutdownTimeout)
	defer timer.Stop()
	select {
	case <-c.done:
		return c.shutdownError()
	case <-ctx.Done():
		return errors.Join(ErrPending, ctx.Err())
	case <-timer.C:
		return errors.Join(ErrPending, context.DeadlineExceeded)
	}
}

func (c *Collector) shutdownError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}
