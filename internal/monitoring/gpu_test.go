package monitoring

import (
	"context"
	"errors"
	"math"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring/gpu"
)

type gpuBridgeUOW struct {
	core.UnitOfWork
	authority *core.RuntimeAuthority
}

func (u gpuBridgeUOW) RuntimeAuthority() *core.RuntimeAuthority { return u.authority }
func gpuBridgeService(t *testing.T) *core.Service {
	t.Helper()
	a, err := core.NewRuntimeAuthority("test-owner", core.ObservationLimits{MaxSessions: 1, MaxGroups: 2, MaxOutputs: 2, MaxFFmpegWorkers: 2, MaxIdentifierBytes: 64, MaxErrorBytes: 256, MaxBytes: 32768}, core.OutputExecutionLimits{MaxReservations: 2, MaxIdentifierBytes: 64, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	s, err := core.NewService(gpuBridgeUOW{authority: a}, "test-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfigureTelemetry(context.Background(), core.TelemetryLimits{MediaStaleAfter: 5 * time.Second, Events: core.EventLimits{MaxEvents: 8, MaxBytes: 32768, MaxEventBytes: 8192, MaxPageSize: 8}, MaxGPUDevices: 2, MaxGPUUUIDBytes: 64, MaxGPUBytes: 4096}); err != nil {
		t.Fatal(err)
	}
	return s
}

type bridgeProvider struct {
	polls   int
	release chan struct{}
	closed  bool
}

func (p *bridgeProvider) Init() gpu.Reason { return gpu.Available }
func (p *bridgeProvider) Poll(gpu.Limits) gpu.Sample {
	p.polls++
	if p.polls > 1 && p.release != nil {
		<-p.release
	}
	r := gpu.Reading{Reason: gpu.Available, SampledAt: time.Now(), Value: 50}
	memory := r
	memory.Value = math.MaxUint64
	device := gpu.DeviceSample{UUID: "GPU-test", GPU: r, Memory: r, Encoder: gpu.Reading{Reason: gpu.NotSupported}, Decoder: r, MemoryUsedBytes: memory, MemoryTotalBytes: memory, MemoryFreeBytes: memory}
	return gpu.Sample{Reason: gpu.Available, Devices: []gpu.DeviceSample{device}}
}
func (p *bridgeProvider) Shutdown() gpu.Reason { p.closed = true; return gpu.Available }
func TestGPUBridgeCopiesExactValuesKeepsOwnerThroughPending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := gpuBridgeService(t)
		provider := &bridgeProvider{release: make(chan struct{})}
		cfg := gpu.Config{MaxDevices: 2, MaxUUIDBytes: 64, MaxSnapshotBytes: 4096, StaleAfter: time.Second, ShutdownTimeout: time.Second}
		collector, err := StartGPU(context.Background(), service, cfg, provider)
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		anchor, err := service.RuntimeAnchor(context.Background())
		if err != nil || anchor.GPU == nil || anchor.GPU.Devices[0].MemoryUsedBytes.Value != math.MaxUint64 || anchor.GPU.Devices[0].EncoderUtilization.Status != core.MetricUnsupported || collector.RejectedSamples() != 0 {
			t.Fatalf("передача данных: %+v %v", anchor.GPU, err)
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		anchor, err = service.RuntimeAnchor(context.Background())
		if err != nil || anchor.GPU.Devices[0].Utilization.Status != core.MetricStale {
			t.Fatalf("зависший нативный вызов сохранил свежесть: %+v %v", anchor.GPU, err)
		}
		if err = collector.Close(context.Background()); !errors.Is(err, gpu.ErrPending) {
			t.Fatalf("нет pending: %v", err)
		}
		if _, err = service.OpenSystemObserver(context.Background()); !errors.Is(err, core.ErrConflict) {
			t.Fatalf("старый владелец потерян: %v", err)
		}
		if provider.closed {
			t.Fatal("Shutdown одновременно с Poll")
		}
		close(provider.release)
		synctest.Wait()
		if err = collector.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-collector.Done():
		default:
			t.Fatal("нет подтверждения завершения")
		}
		anchor, err = service.RuntimeAnchor(context.Background())
		if err != nil || anchor.GPU != nil || !provider.closed {
			t.Fatalf("освобождение: %+v %v", anchor.GPU, err)
		}
	})
}
func TestGPUStatusMappingAndInvalidStart(t *testing.T) {
	for _, reason := range []gpu.Reason{gpu.Available, gpu.WarmingUp, gpu.Stale, gpu.NotSupported, gpu.UnsupportedPlatform, gpu.CGODisabled, gpu.LibraryUnavailable, gpu.DriverUnavailable, gpu.PermissionDenied, gpu.DeviceLost, gpu.InvalidSample, gpu.BudgetExceeded, gpu.Busy, gpu.Closed, gpu.ProviderError, "arbitrary"} {
		status, r := gpuStatus(reason)
		if status == "" || string(r) == "arbitrary" {
			t.Fatalf("небезопасная причина: %s/%s", status, r)
		}
	}
	if _, err := StartGPU(context.Background(), nil, gpu.Config{}, &bridgeProvider{}); !errors.Is(err, core.ErrInvalid) {
		t.Fatal(err)
	}
	s := gpuBridgeService(t)
	if _, err := StartGPU(context.Background(), s, gpu.Config{}, &bridgeProvider{}); !errors.Is(err, gpu.ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := StartGPU(context.Background(), s, gpu.Config{}, nil); !errors.Is(err, core.ErrInvalid) {
		t.Fatal(err)
	}
}
