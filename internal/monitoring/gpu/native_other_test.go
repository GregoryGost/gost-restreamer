//go:build !linux

package gpu

import "testing"

func TestNativeProviderIsExplicitlyUnsupported(t *testing.T) {
	p := NativeProvider()
	if p.Init() != UnsupportedPlatform || p.Poll(Limits{MaxDevices: 1, MaxUUIDBytes: 64}).Reason != UnsupportedPlatform || p.Shutdown() != Available {
		t.Fatal("неподдерживаемая платформа подменена другим GPU")
	}
}
