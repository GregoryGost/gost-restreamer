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

// gpuBridgeUOW предоставляет сервису изолированную RuntimeAuthority без операций долговечного хранения.
type gpuBridgeUOW struct {
	// UnitOfWork встраивает контракт хранилища, не используемый сценарием системных наблюдений.
	core.UnitOfWork
	// authority содержит общий реестр, возвращаемый сервису для публикации GPU.
	authority *core.RuntimeAuthority
}

// RuntimeAuthority возвращает общий реестр наблюдений тестового UnitOfWork для системных измерений.
func (u gpuBridgeUOW) RuntimeAuthority() *core.RuntimeAuthority { return u.authority }

// gpuBridgeService создаёт изолированную RuntimeAuthority с конечными бюджетами наблюдений, событий и
// GPU для проверки передачи снимков.
func gpuBridgeService(t *testing.T) *core.Service {
	t.Helper()
	// a содержит реестр с конечными бюджетами; err содержит отказ его создания.
	a, err := core.NewRuntimeAuthority("test-owner", core.ObservationLimits{MaxSessions: 1, MaxGroups: 2, MaxOutputs: 2, MaxFFmpegWorkers: 2, MaxIdentifierBytes: 64, MaxErrorBytes: 256, MaxBytes: 32768}, core.OutputExecutionLimits{MaxReservations: 2, MaxIdentifierBytes: 64, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	// s содержит доменный сервис того же реестра для чтения системных снимков.
	s, err := core.NewService(gpuBridgeUOW{authority: a}, "test-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfigureTelemetry(context.Background(), core.TelemetryLimits{MediaStaleAfter: 5 * time.Second, Events: core.EventLimits{MaxEvents: 8, MaxBytes: 32768, MaxEventBytes: 8192, MaxPageSize: 8}, MaxGPUDevices: 2, MaxGPUUUIDBytes: 64, MaxGPUBytes: 4096}); err != nil {
		t.Fatal(err)
	}
	return s
}

// bridgeProvider задаёт известные показатели GPU и управляемую задержку повторных опросов.
type bridgeProvider struct {
	// polls считает опросы, чтобы задерживать только последующие чтения.
	polls int
	// release удерживает повторный Poll до разрешения теста; nil оставляет чтение свободным.
	release chan struct{}
	// closed отмечает вызванный Shutdown для проверки отсутствия перекрытия с Poll.
	closed bool
}

// Init объявляет тестовый источник доступным без загрузки NVML.
func (p *bridgeProvider) Init() gpu.Reason { return gpu.Available }

// Poll возвращает известные показатели и максимальные байты памяти; последующие чтения могут
// удерживаться тестовым каналом.
func (p *bridgeProvider) Poll(gpu.Limits) gpu.Sample {
	p.polls++
	if p.polls > 1 && p.release != nil {
		<-p.release
	}
	// r задаёт доступное измерение 50% с фактическим временем опроса.
	r := gpu.Reading{Reason: gpu.Available, SampledAt: time.Now(), Value: 50}
	// memory копирует успешное измерение и получает MaxUint64 для проверки точности байтов памяти.
	memory := r
	memory.Value = math.MaxUint64
	// device содержит известные поля одного UUID с неподдержанным Encoder и максимальными байтами
	// памяти.
	device := gpu.DeviceSample{UUID: "GPU-test", GPU: r, Memory: r, Encoder: gpu.Reading{Reason: gpu.NotSupported}, Decoder: r, MemoryUsedBytes: memory, MemoryTotalBytes: memory, MemoryFreeBytes: memory}
	return gpu.Sample{Reason: gpu.Available, Devices: []gpu.DeviceSample{device}}
}

// Shutdown отмечает освобождение управляемого источника и возвращает успех.
func (p *bridgeProvider) Shutdown() gpu.Reason { p.closed = true; return gpu.Available }

// Проверяет точность передачи GPU в core, устаревание при зависшем Poll и сохранение владельца до
// подтверждённого завершения.
func TestGPUBridgeCopiesExactValuesKeepsOwnerThroughPending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет точное копирование значений GPU и сохранение владельца core во время незавершённого
		// нативного опроса.

		// service предоставляет изолированный доменный реестр наблюдений для проверяемого моста.
		service := gpuBridgeService(t)
		// provider удерживает повторный опрос GPU для проверки свежести и сохранения владельца.
		provider := &bridgeProvider{release: make(chan struct{})}
		// cfg задаёт конечные размеры и секундные сроки свежести и остановки.
		cfg := gpu.Config{MaxDevices: 2, MaxUUIDBytes: 64, MaxSnapshotBytes: 4096, StaleAfter: time.Second, ShutdownTimeout: time.Second}
		// collector содержит мост нативного сборщика в core; err содержит отказ регистрации или
		// запуска.
		collector, err := StartGPU(context.Background(), service, cfg, provider)
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		// anchor содержит согласованный снимок core для проверки точных значений, устаревания и
		// удаления GPU.
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

// Проверяет отображение известных и произвольных причин GPU, а также отказ запуска с отсутствующими
// зависимостями или неверными бюджетами.
func TestGPUStatusMappingAndInvalidStart(t *testing.T) {
	// reason задаёт очередную известную или произвольную причину GPU для проверки безопасного
	// отображения.
	for _, reason := range []gpu.Reason{gpu.Available, gpu.WarmingUp, gpu.Stale, gpu.NotSupported, gpu.UnsupportedPlatform, gpu.CGODisabled, gpu.LibraryUnavailable, gpu.DriverUnavailable, gpu.PermissionDenied, gpu.DeviceLost, gpu.InvalidSample, gpu.BudgetExceeded, gpu.Busy, gpu.Closed, gpu.ProviderError, "arbitrary"} {
		// status содержит отображённую доступность core; r содержит ограниченную причину,
		// которая не должна сохранять произвольную строку.
		status, r := gpuStatus(reason)
		if status == "" || string(r) == "arbitrary" {
			t.Fatalf("небезопасная причина: %s/%s", status, r)
		}
	}
	// err содержит результат запуска без доменного сервиса, ожидается core.ErrInvalid.
	if _, err := StartGPU(context.Background(), nil, gpu.Config{}, &bridgeProvider{}); !errors.Is(err, core.ErrInvalid) {
		t.Fatal(err)
	}
	// s предоставляет допустимый сервис для отдельных проверок неверных настроек и отсутствующего
	// Provider.
	s := gpuBridgeService(t)
	// err содержит отказ запуска с недопустимой конфигурацией GPU.
	if _, err := StartGPU(context.Background(), s, gpu.Config{}, &bridgeProvider{}); !errors.Is(err, gpu.ErrInvalid) {
		t.Fatal(err)
	}
	// err содержит отказ запуска без Provider.
	if _, err := StartGPU(context.Background(), s, gpu.Config{}, nil); !errors.Is(err, core.ErrInvalid) {
		t.Fatal(err)
	}
}
