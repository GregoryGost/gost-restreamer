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
	collector *gpu.Collector
	owner     *core.SystemObserver
	rejected  atomic.Uint64
}

func StartGPU(ctx context.Context, service *core.Service, cfg gpu.Config, provider gpu.Provider) (*GPUCollector, error) {
	if service == nil || provider == nil {
		return nil, core.ErrInvalid
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	owner, err := service.OpenSystemObserver(ctx)
	if err != nil {
		return nil, err
	}
	result := &GPUCollector{owner: owner}
	collector, err := gpu.Start(cfg, provider, func(snapshot gpu.Snapshot) {
		if err := owner.ObserveGPU(context.Background(), coreGPU(snapshot, cfg.StaleAfter)); err != nil {
			for {
				v := result.rejected.Load()
				if v == math.MaxUint64 || result.rejected.CompareAndSwap(v, v+1) {
					break
				}
			}
		}
	}, func() { _ = owner.Close(context.Background()) })
	if err != nil {
		return nil, errors.Join(err, owner.Close(context.Background()))
	}
	result.collector = collector
	return result, nil
}
func (c *GPUCollector) Done() <-chan struct{} {
	if c == nil {
		return nil
	}
	return c.collector.Done()
}
func (c *GPUCollector) Close(ctx context.Context) error {
	if c == nil {
		return core.ErrInvalid
	}
	return c.collector.Close(ctx)
}
func (c *GPUCollector) RejectedSamples() uint64 { return c.rejected.Load() }
func coreGPU(snapshot gpu.Snapshot, staleAfter time.Duration) core.GPUSnapshot {
	status, reason := gpuStatus(snapshot.Reason)
	result := core.GPUSnapshot{SampledAt: snapshot.SampledAt, Status: status, Reason: reason, StaleAfter: staleAfter, Devices: make([]core.GPUDevice, len(snapshot.Devices))}
	field := func(f gpu.Field) core.GPUMetric {
		status, reason := gpuStatus(f.Reason)
		if f.Available {
			status = core.MetricAvailable
			reason = core.ReasonNone
		}
		return core.GPUMetric{Status: status, Reason: reason, Value: f.Value, SampledAt: snapshot.SampledAt, LastSuccessAt: f.LastSuccess, SamplingPeriodUS: uint64(f.SamplingPeriodUS)}
	}
	for i, d := range snapshot.Devices {
		result.Devices[i] = core.GPUDevice{UUID: d.UUID, Utilization: field(d.GPU), MemoryUtilization: field(d.Memory), EncoderUtilization: field(d.Encoder), DecoderUtilization: field(d.Decoder), MemoryTotalBytes: field(d.MemoryTotalBytes), MemoryUsedBytes: field(d.MemoryUsedBytes), MemoryFreeBytes: field(d.MemoryFreeBytes)}
	}
	return result
}
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
