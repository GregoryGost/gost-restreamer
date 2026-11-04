package group

import (
	"context"
	"errors"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"math"
	"testing"
	"time"
)

func av1Headers() []byte {
	return []byte{0x81, 8, 0x0c, 0, 0x0a, 11, 0, 0, 0, 66, 167, 191, 228, 96, 13, 0, 64}
}
func av1TU(key bool) []byte {
	if !key {
		return []byte{0x1a, 1, 0x30}
	}
	return append(append([]byte(nil), av1Headers()[4:]...), 0x1a, 1, 0x10)
}
func switcherFixture(t *testing.T) (*Switcher, media.CodecConfig, media.CodecConfig, time.Time) {
	t.Helper()
	vf, _, err := codec.InspectAV1TU(av1Headers(), av1TU(true), core.FrameRate{Numerator: 30, Denominator: 1})
	if err != nil {
		t.Fatal(err)
	}
	af, _ := codec.AudioFormatASC([]byte{0x11, 0x90})
	base := media.TimeBase{Numerator: 1, Denominator: 48000}
	v, _ := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 50, Revision: 2, TimeBase: base, Video: vf}, av1Headers())
	a, _ := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 50, Revision: 3, TimeBase: base, Audio: af}, []byte{0x11, 0x90})
	c := SwitcherConfig{Profile: core.GroupProfile{Mode: core.ModeCopy, Format: core.MediaFormat{Video: vf, Audio: af}, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 1000, MaxBitrateBPS: 1000, BufferBits: 1000, GOPFrames: 30, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 128000}}, Watchdog: media.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: 100 * time.Millisecond, KeyframeTimeout: 200 * time.Millisecond}, Queue: media.QueueLimits{MaxPackets: 64, MaxBytes: 65536, MaxPacketBytes: 4096, MaxConfigBytes: 512, ResyncTimeout: time.Second}, Router: media.RouterLimits{MaxConsumers: 8, MaxRetainedBytes: 1 << 20, MaxConfigBytes: 512}, MaxAVSkew: time.Second, MaxTransitionGap: time.Second}
	s, err := NewSwitcher(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, v, a, time.Now()
}
func pushPacket(t *testing.T, s *Switcher, f Feed, attempt uint64, c, v, a media.CodecConfig, dts, pts, duration int64, key bool, now time.Time) error {
	t.Helper()
	payload := []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c}
	if c.Info().Track == media.TrackVideo {
		payload = av1TU(key)
	}
	p, err := media.NewPacket(media.PacketInfo{DTS: dts, PTS: pts, DurationTicks: duration, Keyframe: key}, c, payload)
	if err != nil {
		t.Fatal(err)
	}
	return s.Push(f, attempt, p, v, a, now)
}
func allowUnavailable(t *testing.T, err error) {
	t.Helper()
	if err != nil && !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}
func next(t *testing.T, sub *media.Subscription) media.Packet {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, err := sub.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return d.Packet
}
func TestSwitcherFallbackPauseRecoveryAndContinuousHorizon(t *testing.T) {
	s, v, a, now := switcherFixture(t)
	sub, err := s.Router().Subscribe(s.config.Queue)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, -48000, -48000, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, -47520, -47520, 1024, false, now))
	p, q := next(t, sub), next(t, sub)
	if p.Info().DTS != 0 || q.Info().DTS != 480 || s.Status().Source != Fallback {
		t.Fatalf("не сохранён A/V offset: %+v %+v %+v", p.Info(), q.Info(), s.Status())
	}
	// Одинаковые входные epoch разных источников не задают общую идентичность.
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 100000, 100000, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 100480, 100480, 1024, false, now))
	if s.Status().Source != Fallback {
		t.Fatal("LIVE вернулся до стабильного окна")
	}
	later := now.Add(120 * time.Millisecond)
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 101600, 101600, 1600, true, later))
	allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 102080, 102080, 1024, false, later))
	p, q = next(t, sub), next(t, sub)
	if p.Info().DTS != 1600 || q.Info().DTS != 2080 || s.Status().Source != Live || p.Config().Info().SourceEpoch != 2 {
		t.Fatalf("неверный cut: %+v %+v %+v", p.Info(), q.Info(), s.Status())
	}
	allowUnavailable(t, s.SetPaused(true, later))
	if s.Status().Source == Live && s.Status().Available {
		t.Fatal("пауза оставила LIVE")
	}
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, -46400, -46400, 1600, true, later))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, -45920, -45920, 1024, false, later))
	p, q = next(t, sub), next(t, sub)
	if p.Info().DTS != 3200 || q.Info().DTS != 3680 || s.Status().Source != Fallback || !s.Status().Paused {
		t.Fatalf("пауза сбросила шкалу: %+v %+v %+v", p.Info(), q.Info(), s.Status())
	}
	if err := s.Fail(Fallback, 1, errors.New("пропал файл"), later); !errors.Is(err, ErrUnavailable) || s.Status().Available {
		t.Fatalf("ложный fallback: %v %+v", err, s.Status())
	}
}
func TestSwitcherRejectsStaleRegressionAndRevokesOldPending(t *testing.T) {
	s, v, a, now := switcherFixture(t)
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 1024, false, now))
	if err := pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 1600, true, now); !errors.Is(err, ErrFeed) {
		t.Fatal(err)
	}
	if err := pushPacket(t, s, Fallback, 1, v, v, a, -1, -1, 1600, true, now); !errors.Is(err, media.ErrDTSRegression) {
		t.Fatal(err)
	}
	if s.Status().Available {
		t.Fatal("регрессия не отозвала попытку")
	}
	allowUnavailable(t, pushPacket(t, s, Fallback, 2, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 2, a, v, a, 0, 0, 1024, false, now))
	if !s.Status().Available || s.Status().Epoch != 2 {
		t.Fatal(s.Status())
	}
	if err := pushPacket(t, s, Fallback, 1, v, v, a, 10, 10, 1600, true, now); !errors.Is(err, ErrFeed) {
		t.Fatal(err)
	}
	if err := s.Fail(Fallback, 1, ErrFeed, now); !errors.Is(err, ErrFeed) {
		t.Fatal(err)
	}
}
func TestSwitcherMissingTrackUnknownDurationAndLookahead(t *testing.T) {
	s, v, a, now := switcherFixture(t)
	sub, _ := s.Router().Subscribe(s.config.Queue)
	defer func() { _ = sub.Close() }()
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 0, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 0, false, now))
	if s.Status().Available {
		t.Fatal("неизвестный tail получил duration")
	}
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 1600, 1600, 1600, false, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 1024, 1024, 1024, false, now))
	got := []media.Packet{next(t, sub), next(t, sub), next(t, sub), next(t, sub)}
	dur := map[media.Track]int64{}
	for _, p := range got {
		if p.Info().DTS == 0 {
			dur[p.Config().Info().Track] = p.Info().DurationTicks
		}
	}
	if dur[media.TrackVideo] != 1600 || dur[media.TrackAudio] != 1024 {
		t.Fatal(dur)
	}
	if err := s.Tick(now.Add(2 * time.Second)); !errors.Is(err, ErrUnavailable) || s.Status().Available {
		t.Fatal(err, s.Status())
	}
}
func TestSwitcherInvalidInputsAndExactArithmetic(t *testing.T) {
	if _, err := NewSwitcher(SwitcherConfig{}); err == nil {
		t.Fatal("приняты пустые бюджеты")
	}
	for _, tc := range []struct {
		value    int64
		from, to media.TimeBase
		want     int64
		valid    bool
	}{{-3, media.TimeBase{Numerator: 1, Denominator: 3}, media.TimeBase{Numerator: 1, Denominator: 48000}, -48000, true}, {1, media.TimeBase{Numerator: 1, Denominator: 7}, media.TimeBase{Numerator: 1, Denominator: 48000}, 0, false}, {math.MaxInt64, media.TimeBase{Numerator: 2, Denominator: 1}, media.TimeBase{Numerator: 1, Denominator: 1}, 0, false}} {
		n, err := rescale(tc.value, tc.from, tc.to)
		if (err == nil) != tc.valid || tc.valid && n != tc.want {
			t.Fatal(n, err)
		}
	}
	if _, err := sum(math.MaxInt64, 1); err == nil {
		t.Fatal("переполнение сложения")
	}
	if _, err := difference(math.MinInt64, 1); err == nil {
		t.Fatal("переполнение вычитания")
	}
	s, v, a, now := switcherFixture(t)
	if err := pushPacket(t, s, Feed(99), 1, v, v, a, 0, 0, 1600, true, now); err == nil {
		t.Fatal("неверный feed")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(s.Tick(now), media.ErrRouterClosed) || !errors.Is(s.SetPaused(true, now), media.ErrRouterClosed) {
		t.Fatal("закрытый switcher активен")
	}
}

func TestSwitcherDelayedControlCannotRewindRecovery(t *testing.T) {
	s, v, a, now := switcherFixture(t)
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 1024, false, now))
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 0, 0, 1024, false, now))
	// Событие таймера ожидало медленного чтения управления; пакеты пришли позже его метки.
	later := now.Add(60 * time.Millisecond)
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 1600, 1600, 1600, false, later))
	allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 1024, 1024, 1024, false, later))
	allowUnavailable(t, s.SetPaused(false, now.Add(time.Millisecond)))
	allowUnavailable(t, s.Tick(now.Add(2*time.Millisecond)))
	later = now.Add(120 * time.Millisecond)
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 3200, 3200, 1600, true, later))
	allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 3200, 3200, 1024, false, later))
	if status := s.Status(); status.Source != Live || !status.Available {
		t.Fatalf("запаздывающее управление сбросило стабильный источник: %+v", status)
	}
}
func TestSwitcherBoundedUnselectedBacklogAndPendingBudget(t *testing.T) {
	s, v, a, now := switcherFixture(t)
	s.config.Queue.MaxPackets = 4
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 1024, false, now))
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 0, 0, 1600, true, now))
	for i := int64(1); i <= 5; i++ {
		allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, i*1600, i*1600, 1600, false, now.Add(time.Duration(i)*time.Millisecond)))
	}
	if len(s.feeds[0].queue) > 4 || s.Status().Source != Fallback || !s.Status().Available {
		t.Fatal("переполнение чужой очереди повредило fallback", s.Status())
	}
	s.config.Queue.MaxBytes = 50
	s.config.Queue.MaxPacketBytes = 40
	s.config.Queue.MaxConfigBytes = 20
	if err := pushPacket(t, s, Live, 2, v, v, a, 0, 0, 0, true, now); !errors.Is(err, media.ErrRouterBudget) {
		t.Fatalf("pending не вошёл в общий бюджет: %v", err)
	}
	if s.feeds[0].pending[0] != nil || s.feeds[0].bytes != 0 || !s.Status().Available {
		t.Fatal("отказ бюджета сохранил данные либо повредил fallback")
	}
}
func TestSwitcherDiscardsPendingOnConfigChangeAndRejectsImpossibleCut(t *testing.T) {
	s, v, a, now := switcherFixture(t)
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 0, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 0, false, now))
	vi, ai := v.Info(), a.Info()
	vi.SourceEpoch++
	ai.SourceEpoch++
	freshV, _ := media.NewCodecConfig(vi, v.Headers())
	freshA, _ := media.NewCodecConfig(ai, a.Headers())
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, freshV, freshV, freshA, 100000, 100000, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, freshA, freshV, freshA, 100000, 100000, 1024, false, now))
	if !s.Status().Available || s.horizon != 1600 {
		t.Fatal("старый неизвестный хвост попал в новую конфигурацию", s.Status(), s.horizon)
	}
	if err := pushPacket(t, s, Fallback, 1, v, v, a, 1600, 1600, 1600, true, now); !errors.Is(err, ErrFeed) {
		t.Fatal("старые заголовки вернулись", err)
	}
	s.config.MaxAVSkew = time.Millisecond
	allowUnavailable(t, pushPacket(t, s, Fallback, 2, freshV, freshV, freshA, 0, 0, 1600, true, now))
	if err := pushPacket(t, s, Fallback, 2, freshA, freshV, freshA, 480, 480, 1024, false, now); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if s.Status().Available {
		t.Fatal("непредставимый согласованный cut объявлен доступным")
	}
}
func TestSwitcherRejectsUnknownSourceDurationOverflowWithoutPublication(t *testing.T) {
	s, v, a, now := switcherFixture(t)
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, math.MinInt64, math.MinInt64, 0, true, now))
	if err := pushPacket(t, s, Fallback, 1, v, v, a, math.MaxInt64, math.MaxInt64, 1600, true, now); !errors.Is(err, ErrTiming) {
		t.Fatal(err)
	}
	if s.hasHorizon || s.Status().Available {
		t.Fatal("переполнение lookahead опубликовано")
	}
}

func TestSwitcherInvalidTransitionCannotReplaceHealthyFallbackConfig(t *testing.T) {
	s, v, a, now := switcherFixture(t)
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 1024, false, now))
	epoch := s.Status().Epoch
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 0, 0, 1024, false, now))
	later := now.Add(120 * time.Millisecond)
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 1600, 1600, math.MaxInt64, true, later))
	allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 1600, 1600, 1024, false, later))
	if status := s.Status(); status.Source != Fallback || !status.Available || status.Epoch != epoch {
		t.Fatalf("невалидный сегмент сменил исправные заголовки: %+v", status)
	}
}
