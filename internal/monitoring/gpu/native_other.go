//go:build !linux

package gpu

// NativeProvider на этой платформе явно сообщает отсутствие NVML.
func NativeProvider() Provider { return unavailableProvider{reason: UnsupportedPlatform} }
