package monitoring

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring/gpu"
)

// GPUCollector связывает нативный коллектор с точным системным наблюдателем core.
type GPUCollector struct {
	// collector выполняет последовательные нативные опросы и подтверждает их окончательное
	// завершение.
	collector *gpu.Collector
	// owner публикует GPU в общей RuntimeAuthority и закрывается после завершения нативного
	// сборщика.
	owner *core.SystemObserver
	// rejected насыщаемо считает снимки, которые RuntimeAuthority отказалась принять.
	rejected atomic.Uint64
}

// StartGPU регистрирует точного системного наблюдателя и связывает его с отдельным сборщиком GPU.
// Наблюдатель освобождается после фактического завершения сборщика; отклонённые публикации учитываются
// отдельно.
func StartGPU(ctx context.Context, service *core.Service, cfg gpu.Config, provider gpu.Provider) (*GPUCollector, error) {
	if service == nil || provider == nil {
		return nil, core.ErrInvalid
	}
	// err содержит отказ проверки бюджетов GPU до регистрации системного наблюдателя.
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// owner содержит системного наблюдателя текущей authority; err содержит отказ его регистрации.
	owner, err := service.OpenSystemObserver(ctx)
	if err != nil {
		return nil, err
	}
	// result связывает наблюдателя, нативный сборщик и счётчик отклонённых публикаций.
	result := &GPUCollector{owner: owner}
	// collector содержит запущенного нативного владельца, чьи публикации направляются через owner.
	collector, err := gpu.Start(cfg, provider, func(snapshot gpu.Snapshot) {
		// Передаёт снимок GPU точному системному наблюдателю и насыщаемо считает отклонённые публикации.

		// err содержит отказ публикации снимка GPU точному системному наблюдателю.
		if err := owner.ObserveGPU(context.Background(), coreGPU(snapshot, cfg.StaleAfter)); err != nil {
			for {
				// v содержит текущее число отклонений для насыщаемого атомарного
				// увеличения без переполнения.
				v := result.rejected.Load()
				if v == math.MaxUint64 || result.rejected.CompareAndSwap(v, v+1) {
					break
				}
			}
		}
	}, func() {
		// Закрывает системного наблюдателя после завершения нативного сборщика GPU.
		_ = owner.Close(context.Background())
	})
	if err != nil {
		return nil, errors.Join(err, owner.Close(context.Background()))
	}
	result.collector = collector
	return result, nil
}

// Done возвращает подтверждение завершения нативного сборщика; nil-получатель возвращает nil.
func (c *GPUCollector) Done() <-chan struct{} {
	if c == nil {
		return nil
	}
	return c.collector.Done()
}

// Close закрывает допуск сборщика и ограниченно ждёт его завершения. Ошибка незавершённости сохраняет
// системного владельца до фактического выхода работника.
func (c *GPUCollector) Close(ctx context.Context) error {
	if c == nil {
		return core.ErrInvalid
	}
	return c.collector.Close(ctx)
}

// RejectedSamples возвращает насыщаемое число снимков GPU, отклонённых общей RuntimeAuthority.
func (c *GPUCollector) RejectedSamples() uint64 { return c.rejected.Load() }

// coreGPU переводит снимок GPU в типы core, сохраняя значения, времена последнего успеха и период
// аппаратной выборки в микросекундах.
func coreGPU(snapshot gpu.Snapshot, staleAfter time.Duration) core.GPUSnapshot {
	// status содержит публичную доступность общего снимка; reason содержит её ограниченную причину.
	status, reason := gpuStatus(snapshot.Reason)
	// result собирает снимок core с исходным временем опроса, сроком свежести и отдельным местом для
	// каждого устройства.
	result := core.GPUSnapshot{SampledAt: snapshot.SampledAt, Status: status, Reason: reason, StaleAfter: staleAfter, Devices: make([]core.GPUDevice, len(snapshot.Devices))}
	// field преобразует отдельное измерение GPU, сохраняя последнее успешное время и аппаратный
	// период выборки.
	field := func(f gpu.Field) core.GPUMetric {
		// Преобразует отдельное поле GPU, сохраняя единицы, последнее успешное время и явную доступность.

		// status содержит доступность отдельного измерения; reason содержит причину его
		// недоступности либо ReasonNone при успехе.
		status, reason := gpuStatus(f.Reason)
		if f.Available {
			status = core.MetricAvailable
			reason = core.ReasonNone
		}
		return core.GPUMetric{Status: status, Reason: reason, Value: f.Value, SampledAt: snapshot.SampledAt, LastSuccessAt: f.LastSuccess, SamplingPeriodUS: uint64(f.SamplingPeriodUS)}
	}
	// i указывает место устройства в результирующем снимке; d содержит исходные измерения этого
	// устройства.
	for i, d := range snapshot.Devices {
		result.Devices[i] = core.GPUDevice{UUID: d.UUID, Utilization: field(d.GPU), MemoryUtilization: field(d.Memory), EncoderUtilization: field(d.Encoder), DecoderUtilization: field(d.Decoder), MemoryTotalBytes: field(d.MemoryTotalBytes), MemoryUsedBytes: field(d.MemoryUsedBytes), MemoryFreeBytes: field(d.MemoryFreeBytes)}
	}
	return result
}

// gpuStatus переводит причины нативного сборщика в ограниченный набор статусов core; неизвестная
// причина становится provider_error.
func gpuStatus(reason gpu.Reason) (core.MetricStatus, core.MetricReason) {
	switch reason {
	case gpu.Available:
		return core.MetricAvailable, core.ReasonNone
	case gpu.WarmingUp:
		return core.MetricWarmingUp, core.ReasonWarmingUp
	case gpu.Stale:
		return core.MetricStale, core.ReasonStale
	case gpu.NotSupported:
		return core.MetricUnsupported, core.ReasonNotSupported
	case gpu.UnsupportedPlatform:
		return core.MetricUnsupported, core.ReasonUnsupportedPlatform
	case gpu.CGODisabled:
		return core.MetricUnsupported, core.ReasonCGODisabled
	case gpu.LibraryUnavailable:
		return core.MetricUnavailable, core.ReasonLibraryUnavailable
	case gpu.DriverUnavailable:
		return core.MetricUnavailable, core.ReasonDriverUnavailable
	case gpu.PermissionDenied:
		return core.MetricUnavailable, core.ReasonPermissionDenied
	case gpu.DeviceLost:
		return core.MetricUnavailable, core.ReasonDeviceLost
	case gpu.InvalidSample:
		return core.MetricUnavailable, core.ReasonInvalidSample
	case gpu.BudgetExceeded:
		return core.MetricUnavailable, core.ReasonBudgetExceeded
	case gpu.Busy:
		return core.MetricUnavailable, core.ReasonBusy
	case gpu.Closed:
		return core.MetricUnavailable, core.ReasonSourceAbsent
	default:
		return core.MetricUnavailable, core.ReasonProviderError
	}
}
