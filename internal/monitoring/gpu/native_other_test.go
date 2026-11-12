//go:build !linux

package gpu

import "testing"

// Проверяет явную недоступность NVML вне Linux без подмены другим источником GPU и без ошибки
// освобождения.
func TestNativeProviderIsExplicitlyUnsupported(t *testing.T) {
	// p содержит настоящий платформенный Provider для проверки явного unsupported_platform вне
	// Linux.
	p := NativeProvider()
	if p.Init() != UnsupportedPlatform || p.Poll(Limits{MaxDevices: 1, MaxUUIDBytes: 64}).Reason != UnsupportedPlatform || p.Shutdown() != Available {
		t.Fatal("неподдерживаемая платформа подменена другим GPU")
	}
}
