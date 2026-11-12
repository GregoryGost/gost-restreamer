//go:build linux && cgo

package gpu

import (
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// nvmlBackend связывает последовательный Provider с нативной библиотекой NVIDIA на Linux с CGO.
type nvmlBackend struct{}

// NativeProvider использует NVML только на Linux и допускает одного физического владельца.
func NativeProvider() Provider { return newNativeProvider(nvmlBackend{}) }

// Init загружает NVML и переводит её результат в причину доступности сборщика.
func (nvmlBackend) Init() Reason { return nativeReason(nvml.Init()) }

// Poll читает ограниченное число NVIDIA-устройств и независимые показатели загрузки и памяти с
// фактическими временами чтения. Ошибка отдельного показателя не подменяет остальные нулями.
func (nvmlBackend) Poll(limits Limits) Sample {
	// count содержит число NVIDIA-устройств; result содержит код NVML до проверки конечного
	// MaxDevices.
	count, result := nvml.DeviceGetCount()
	if result != nvml.SUCCESS {
		return Sample{Reason: nativeReason(result)}
	}
	if count < 0 || count > limits.MaxDevices {
		return Sample{Reason: BudgetExceeded}
	}
	// sample собирает измерения устройств только после проверки их числа.
	sample := Sample{Reason: Available, Devices: make([]DeviceSample, 0, count)}
	// i указывает нативный индекс очередного устройства NVIDIA.
	for i := range count {
		// device содержит нативный дескриптор устройства; result содержит код его получения по
		// индексу.
		device, result := nvml.DeviceGetHandleByIndex(i)
		if result != nvml.SUCCESS {
			return Sample{Reason: nativeReason(result)}
		}
		// uuid содержит идентификатор устройства для проверки непустоты и длины до сохранения
		// снимка.
		uuid, result := device.GetUUID()
		if result != nvml.SUCCESS {
			return Sample{Reason: nativeReason(result)}
		}
		if len(uuid) == 0 || len(uuid) > limits.MaxUUIDBytes {
			return Sample{Reason: InvalidSample}
		}
		// d собирает независимые показатели одного проверенного UUID.
		d := DeviceSample{UUID: uuid}
		// utilization содержит проценты загрузки GPU и активности памяти, полученные одним
		// NVML-вызовом.
		utilization, result := device.GetUtilizationRates()
		// at фиксирует время общего чтения связанных показателей GPU/памяти и затем показателей
		// объёма памяти.
		at := time.Now()
		d.GPU = nativeReading(uint64(utilization.Gpu), 0, result, at)
		d.Memory = nativeReading(uint64(utilization.Memory), 0, result, at)
		// encoder содержит процент загрузки NVENC; period содержит аппаратный период этой
		// выборки в микросекундах.
		encoder, period, result := device.GetEncoderUtilization()
		d.Encoder = nativeReading(uint64(encoder), period, result, time.Now())
		// decoder содержит процент загрузки NVDEC с периодом, возвращённым тем же NVML-вызовом.
		decoder, period, result := device.GetDecoderUtilization()
		d.Decoder = nativeReading(uint64(decoder), period, result, time.Now())
		// memory содержит общий, занятый и свободный объёмы памяти устройства в байтах.
		memory, result := device.GetMemoryInfo()
		at = time.Now()
		d.MemoryTotalBytes = nativeReading(memory.Total, 0, result, at)
		d.MemoryUsedBytes = nativeReading(memory.Used, 0, result, at)
		d.MemoryFreeBytes = nativeReading(memory.Free, 0, result, at)
		sample.Devices = append(sample.Devices, d)
	}
	return sample
}

// Shutdown вызывает освобождение NVML и возвращает нормализованную причину результата.
func (nvmlBackend) Shutdown() Reason { return nativeReason(nvml.Shutdown()) }

// nativeReading сохраняет значение, аппаратный период в микросекундах и время чтения вместе с
// результатом NVML.
func nativeReading(value uint64, period uint32, result nvml.Return, at time.Time) Reading {
	return Reading{Value: value, SamplingPeriodUS: period, Reason: nativeReason(result), SampledAt: at}
}

// nativeReason переводит коды NVML в ограниченный набор причин доступности; остальные ошибки
// становятся ProviderError.
func nativeReason(result nvml.Return) Reason {
	switch result {
	case nvml.SUCCESS:
		return Available
	case nvml.ERROR_NOT_SUPPORTED, nvml.ERROR_FUNCTION_NOT_FOUND:
		return NotSupported
	case nvml.ERROR_LIBRARY_NOT_FOUND:
		return LibraryUnavailable
	case nvml.ERROR_DRIVER_NOT_LOADED:
		return DriverUnavailable
	case nvml.ERROR_NO_PERMISSION:
		return PermissionDenied
	case nvml.ERROR_GPU_IS_LOST:
		return DeviceLost
	default:
		return ProviderError
	}
}
