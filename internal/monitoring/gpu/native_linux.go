//go:build linux && cgo

package gpu

import (
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

type nvmlBackend struct{}

// NativeProvider использует NVML только на Linux и допускает одного физического владельца.
func NativeProvider() Provider { return newNativeProvider(nvmlBackend{}) }

func (nvmlBackend) Init() Reason { return nativeReason(nvml.Init()) }

func (nvmlBackend) Poll(limits Limits) Sample {
	count, result := nvml.DeviceGetCount()
	if result != nvml.SUCCESS {
		return Sample{Reason: nativeReason(result)}
	}
	if count < 0 || count > limits.MaxDevices {
		return Sample{Reason: BudgetExceeded}
	}
	sample := Sample{Reason: Available, Devices: make([]DeviceSample, 0, count)}
	for i := range count {
		device, result := nvml.DeviceGetHandleByIndex(i)
		if result != nvml.SUCCESS {
			return Sample{Reason: nativeReason(result)}
		}
		uuid, result := device.GetUUID()
		if result != nvml.SUCCESS {
			return Sample{Reason: nativeReason(result)}
		}
		if len(uuid) == 0 || len(uuid) > limits.MaxUUIDBytes {
			return Sample{Reason: InvalidSample}
		}
		d := DeviceSample{UUID: uuid}
		utilization, result := device.GetUtilizationRates()
		at := time.Now()
		d.GPU = nativeReading(uint64(utilization.Gpu), 0, result, at)
		d.Memory = nativeReading(uint64(utilization.Memory), 0, result, at)
		encoder, period, result := device.GetEncoderUtilization()
		d.Encoder = nativeReading(uint64(encoder), period, result, time.Now())
		decoder, period, result := device.GetDecoderUtilization()
		d.Decoder = nativeReading(uint64(decoder), period, result, time.Now())
		memory, result := device.GetMemoryInfo()
		at = time.Now()
		d.MemoryTotalBytes = nativeReading(memory.Total, 0, result, at)
		d.MemoryUsedBytes = nativeReading(memory.Used, 0, result, at)
		d.MemoryFreeBytes = nativeReading(memory.Free, 0, result, at)
		sample.Devices = append(sample.Devices, d)
	}
	return sample
}

func (nvmlBackend) Shutdown() Reason { return nativeReason(nvml.Shutdown()) }

func nativeReading(value uint64, period uint32, result nvml.Return, at time.Time) Reading {
	return Reading{Value: value, SamplingPeriodUS: period, Reason: nativeReason(result), SampledAt: at}
}

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
