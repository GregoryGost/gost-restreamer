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

// acceptanceGPUAuthority предоставляет тестовому сервису одну общую RuntimeAuthority для системных
// наблюдений.
type acceptanceGPUAuthority struct {
	// UnitOfWork встраивает неиспользуемый здесь контракт долговечных операций.
	core.UnitOfWork
	// authority содержит точный реестр системного владельца проверяемого сервиса.
	authority *core.RuntimeAuthority
}

// RuntimeAuthority предоставляет тестовому сервису общий реестр оперативных наблюдений.
func (u acceptanceGPUAuthority) RuntimeAuthority() *core.RuntimeAuthority { return u.authority }

// acceptanceGPUProvider выдаёт известный первый снимок и удерживает второй опрос до проверки pending.
type acceptanceGPUProvider struct {
	// polls различает первый успешный снимок и задержанный повторный опрос.
	polls int
	// entered подтверждает начало задержанного повторного Poll.
	entered chan struct{}
	// release разрешает повторному Poll вернуть потерю устройства после начала закрытия.
	release chan struct{}
}

// Init объявляет управляемый источник доступным без обращения к NVML.
func (*acceptanceGPUProvider) Init() gpu.Reason { return gpu.Available }

// Shutdown завершает управляемый источник без нативных ресурсов.
func (*acceptanceGPUProvider) Shutdown() gpu.Reason { return gpu.Available }

// Poll сначала возвращает известные проценты, точные байты памяти и частичную недоступность, затем
// удерживает чтение до явного разрешения теста.
func (p *acceptanceGPUProvider) Poll(gpu.Limits) gpu.Sample {
	p.polls++
	if p.polls > 1 {
		close(p.entered)
		<-p.release
		return gpu.Sample{Reason: gpu.DeviceLost}
	}
	// percent задаёт известное доступное значение 37% для GPU и активности памяти.
	percent := gpu.Reading{Reason: gpu.Available, Value: 37}
	return gpu.Sample{Reason: gpu.Available, Devices: []gpu.DeviceSample{{UUID: "GPU-controlled", GPU: percent, Memory: percent, Encoder: gpu.Reading{Reason: gpu.Available, Value: 19, SamplingPeriodUS: 750000}, Decoder: gpu.Reading{Reason: gpu.NotSupported}, MemoryTotalBytes: gpu.Reading{Reason: gpu.Available, Value: 1 << 34}, MemoryUsedBytes: gpu.Reading{Reason: gpu.Available, Value: 1 << 32}, MemoryFreeBytes: gpu.Reading{Reason: gpu.Available, Value: 3 << 32}}}}
}

// Проверяет передачу единиц и точных значений GPU в core, самостоятельное устаревание при зависшем
// Poll и удержание системного владельца до позднего завершения.
func TestAcceptanceGPUCoreOwnerKeepsUnitsStalenessAndPendingJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет единицы, устаревание и удержание системного наблюдателя core до фактического возврата
		// Poll.

		// a содержит реестр с явными пределами; err содержит отказ создания RuntimeAuthority.
		a, err := core.NewRuntimeAuthority("owner", core.ObservationLimits{MaxSessions: 1, MaxGroups: 1, MaxOutputs: 1, MaxFFmpegWorkers: 1, MaxIdentifierBytes: 64, MaxErrorBytes: 64, MaxBytes: 65536}, core.OutputExecutionLimits{MaxReservations: 1, MaxIdentifierBytes: 64, MaxBytes: 65536})
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		if err = a.ConfigureTelemetry(context.Background(), core.TelemetryLimits{MediaStaleAfter: 3 * time.Second, Events: core.EventLimits{MaxEvents: 8, MaxPageSize: 8, MaxBytes: 65536, MaxEventBytes: 8192}, MaxGPUDevices: 1, MaxGPUUUIDBytes: 64, MaxGPUBytes: 2048}); err != nil {
			t.Fatal(err)
		}
		// svc содержит доменный сервис общей authority для чтения GPU и проверки системного
		// владельца.
		svc, err := core.NewService(acceptanceGPUAuthority{authority: a}, "owner")
		if err != nil {
			t.Fatal(err)
		}
		// provider содержит управляемый источник с сигналами второго опроса.
		provider := &acceptanceGPUProvider{entered: make(chan struct{}), release: make(chan struct{})}
		// cfg задаёт бюджеты одного устройства и трёхсекундный срок свежести его полей.
		cfg := gpu.Config{MaxDevices: 1, MaxUUIDBytes: 64, MaxSnapshotBytes: 2048, StaleAfter: 3 * time.Second, ShutdownTimeout: time.Second}
		// collector содержит мост в core для проверки единиц, свежести и позднего завершения.
		collector, err := StartGPU(context.Background(), svc, cfg, provider)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			// Разблокирует опрос и завершает сборщик GPU перед закрытием RuntimeAuthority.

			select {
			case <-provider.release:
			default:
				close(provider.release)
			}
			_ = collector.Close(context.Background())
		}()
		synctest.Wait()
		// first содержит первый согласованный снимок GPU RuntimeAuthority.
		first, err := svc.RuntimeAnchor(context.Background())
		if err != nil || first.GPU == nil || len(first.GPU.Devices) != 1 {
			t.Fatal("GPU не дошёл до RuntimeAuthority", first, err)
		}
		// d сохраняет исходные значения устройства и времена успеха до изменения возвращённой
		// копии.
		d := first.GPU.Devices[0]
		if d.Utilization.Value != 37 || d.EncoderUtilization.Value != 19 || d.EncoderUtilization.SamplingPeriodUS != 750000 || d.MemoryTotalBytes.Value != 1<<34 || d.DecoderUtilization.Status != core.MetricUnsupported || d.DecoderUtilization.Reason != core.ReasonNotSupported {
			t.Fatal("единицы или частичная недоступность изменены", d)
		}
		first.GPU.Devices[0].UUID = "caller-changed"
		time.Sleep(2 * time.Second)
		<-provider.entered
		time.Sleep(2 * time.Second)
		// stale содержит снимок после превышения срока свежести при удерживаемом Poll.
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
		// final содержит общий снимок после подтверждённого завершения, в котором GPU должен
		// отсутствовать.
		final, err := svc.RuntimeAnchor(context.Background())
		if err != nil || final.GPU != nil || collector.RejectedSamples() != 0 {
			t.Fatal("завершение сохранило GPU либо опубликовало позднее измерение", final, err)
		}
	})
}
