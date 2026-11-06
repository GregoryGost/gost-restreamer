package monitoring

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring/gpu"
)

type acceptanceGPUAuthority struct {
	core.UnitOfWork
	authority *core.RuntimeAuthority
}

func (u acceptanceGPUAuthority) RuntimeAuthority() *core.RuntimeAuthority { return u.authority }

type acceptanceGPUProvider struct {
	polls   int
	entered chan struct{}
	release chan struct{}
}

func (*acceptanceGPUProvider) Init() gpu.Reason     { return gpu.Available }
func (*acceptanceGPUProvider) Shutdown() gpu.Reason { return gpu.Available }
func (p *acceptanceGPUProvider) Poll(gpu.Limits) gpu.Sample {
	p.polls++
	if p.polls > 1 {
		close(p.entered)
		<-p.release
		return gpu.Sample{Reason: gpu.DeviceLost}
	}
	percent := gpu.Reading{Reason: gpu.Available, Value: 37}
	return gpu.Sample{Reason: gpu.Available, Devices: []gpu.DeviceSample{{UUID: "GPU-controlled", GPU: percent, Memory: percent, Encoder: gpu.Reading{Reason: gpu.Available, Value: 19, SamplingPeriodUS: 750000}, Decoder: gpu.Reading{Reason: gpu.NotSupported}, MemoryTotalBytes: gpu.Reading{Reason: gpu.Available, Value: 1 << 34}, MemoryUsedBytes: gpu.Reading{Reason: gpu.Available, Value: 1 << 32}, MemoryFreeBytes: gpu.Reading{Reason: gpu.Available, Value: 3 << 32}}}}
}

func TestAcceptanceGPUCoreOwnerKeepsUnitsStalenessAndPendingJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, err := core.NewRuntimeAuthority("owner", core.ObservationLimits{MaxSessions: 1, MaxGroups: 1, MaxOutputs: 1, MaxFFmpegWorkers: 1, MaxIdentifierBytes: 64, MaxErrorBytes: 64, MaxBytes: 65536}, core.OutputExecutionLimits{MaxReservations: 1, MaxIdentifierBytes: 64, MaxBytes: 65536})
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		if err = a.ConfigureTelemetry(context.Background(), core.TelemetryLimits{MediaStaleAfter: 3 * time.Second, Events: core.EventLimits{MaxEvents: 8, MaxPageSize: 8, MaxBytes: 65536, MaxEventBytes: 8192}, MaxGPUDevices: 1, MaxGPUUUIDBytes: 64, MaxGPUBytes: 2048}); err != nil {
			t.Fatal(err)
		}
		svc, err := core.NewService(acceptanceGPUAuthority{authority: a}, "owner")
		if err != nil {
			t.Fatal(err)
		}
		provider := &acceptanceGPUProvider{entered: make(chan struct{}), release: make(chan struct{})}
		cfg := gpu.Config{MaxDevices: 1, MaxUUIDBytes: 64, MaxSnapshotBytes: 2048, StaleAfter: 3 * time.Second, ShutdownTimeout: time.Second}
		collector, err := StartGPU(context.Background(), svc, cfg, provider)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			select {
			case <-provider.release:
			default:
				close(provider.release)
			}
			_ = collector.Close(context.Background())
		}()
		synctest.Wait()
		first, err := svc.RuntimeAnchor(context.Background())
		if err != nil || first.GPU == nil || len(first.GPU.Devices) != 1 {
			t.Fatal("GPU не дошёл до RuntimeAuthority", first, err)
		}
		d := first.GPU.Devices[0]
		if d.Utilization.Value != 37 || d.EncoderUtilization.Value != 19 || d.EncoderUtilization.SamplingPeriodUS != 750000 || d.MemoryTotalBytes.Value != 1<<34 || d.DecoderUtilization.Status != core.MetricUnsupported || d.DecoderUtilization.Reason != core.ReasonNotSupported {
			t.Fatal("единицы или частичная недоступность изменены", d)
		}
		first.GPU.Devices[0].UUID = "caller-changed"
		time.Sleep(2 * time.Second)
		<-provider.entered
		time.Sleep(2 * time.Second)
		stale, err := svc.RuntimeAnchor(context.Background())
		if err != nil || stale.GPU.Devices[0].UUID != "GPU-controlled" || stale.GPU.Devices[0].Utilization.Status != core.MetricStale || stale.GPU.Devices[0].Utilization.Value != 37 || !stale.GPU.Devices[0].Utilization.LastSuccessAt.Equal(d.Utilization.LastSuccessAt) {
			t.Fatal("зависший Poll выдан за свежее значение", stale, err)
		}
		if err = collector.Close(context.Background()); !errors.Is(err, gpu.ErrPending) {
			t.Fatal("неотменяемый Poll выдал ложное подтверждение завершения", err)
		}
		if _, err = svc.OpenSystemObserver(context.Background()); !errors.Is(err, core.ErrConflict) {
			t.Fatal("незавершённый владелец освободил системное место core", err)
		}
		close(provider.release)
		if err = collector.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		final, err := svc.RuntimeAnchor(context.Background())
		if err != nil || final.GPU != nil || collector.RejectedSamples() != 0 {
			t.Fatal("завершение сохранило GPU либо опубликовало позднее измерение", final, err)
		}
	})
}
