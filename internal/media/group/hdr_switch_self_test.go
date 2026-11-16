package group

import (
	"bytes"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// switchHDRConfig создаёт отдельный runtime snapshot той же SDR последовательности с авторскими
// CLL. Тест проверяет перенос внешних сведений, не объявляя SDR пиксели HDR-пикселями.
func switchHDRConfig(t *testing.T, config media.CodecConfig, maximum uint64) media.CodecConfig {
	t.Helper()
	// info сохраняет формат, identity/timebase и headers, добавляя только внешний static snapshot.
	info := config.Info()
	info.HDR.CLL = media.HDRContentLight{Present: true, Source: media.HDRSourceContainer, MaxCLL: media.HDRValue{Raw: maximum, Grid: media.HDRGridInteger, Known: true}, MaxFALL: media.HDRValue{Raw: 100, Grid: media.HDRGridInteger, Known: true}}
	// result и err содержат immutable конфигурацию источника и отказ структурной проверки.
	result, err := media.NewCodecConfig(info, config.Headers())
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// TestSwitcherCarriesSelectedMetadataAcrossLiveFallback проверяет разные CLL при одном профиле.
// Последовательность FALLBACK→LIVE→FALLBACK сохраняет metadata выбранного source, headers и AU.
func TestSwitcherCarriesSelectedMetadataAcrossLiveFallback(t *testing.T) {
	// switcher, baseVideo, audio и now предоставляют изолированный выход и точные шкалы обеих дорожек.
	switcher, baseVideo, audio, now := switcherFixture(t)
	// fallback и live различаются только авторскими CLL, не desired-профилем или codec headers.
	fallback := switchHDRConfig(t, baseVideo, 1000)
	live := switchHDRConfig(t, baseVideo, 500)
	// subscription и err создают независимого получателя выходной пары.
	subscription, err := switcher.Router().Subscribe(switcher.config.Queue)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает единственную принадлежащую тесту подписку после завершения сценария.
		_ = subscription.Close()
	})
	allowUnavailable(t, pushPacket(t, switcher, Fallback, 1, fallback, fallback, audio, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, switcher, Fallback, 1, audio, fallback, audio, 0, 0, 1024, false, now))
	// packet содержит фактически выбранный fallback snapshot после нового выходного epoch.
	packet := next(t, subscription)
	_ = next(t, subscription)
	if packet.Config().Info().HDR != fallback.Info().HDR || !bytes.Equal(packet.Payload(), av1TU(true)) {
		t.Fatal("Fallback metadata либо AU потеряны")
	}
	allowUnavailable(t, pushPacket(t, switcher, Live, 1, live, live, audio, 100000, 100000, 1600, true, now))
	allowUnavailable(t, pushPacket(t, switcher, Live, 1, audio, live, audio, 100000, 100000, 1024, false, now))
	// later завершает существующее окно стабильного восстановления LIVE.
	later := now.Add(120 * time.Millisecond)
	allowUnavailable(t, pushPacket(t, switcher, Live, 1, live, live, audio, 101600, 101600, 1600, true, later))
	allowUnavailable(t, pushPacket(t, switcher, Live, 1, audio, live, audio, 101600, 101600, 1024, false, later))
	packet = next(t, subscription)
	_ = next(t, subscription)
	if packet.Config().Info().HDR != live.Info().HDR || !bytes.Equal(packet.Config().Headers(), baseVideo.Headers()) || !bytes.Equal(packet.Payload(), av1TU(true)) {
		t.Fatal("LIVE унаследовал stale metadata предыдущего источника")
	}
	allowUnavailable(t, switcher.SetPaused(true, later))
	allowUnavailable(t, pushPacket(t, switcher, Fallback, 1, fallback, fallback, audio, 1600, 1600, 1600, true, later))
	allowUnavailable(t, pushPacket(t, switcher, Fallback, 1, audio, fallback, audio, 1600, 1600, 1024, false, later))
	packet = next(t, subscription)
	_ = next(t, subscription)
	if packet.Config().Info().HDR != fallback.Info().HDR || !bytes.Equal(packet.Payload(), av1TU(true)) {
		t.Fatal("Возврат к fallback оставил stale LIVE metadata")
	}
}
