//go:build linux && !cgo

package gpu

// NativeProvider требует CGO для привязки NVML на Linux.
func NativeProvider() Provider { return unavailableProvider{reason: CGODisabled} }
