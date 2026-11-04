package group

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

func switchAcceptanceConfig() SwitcherConfig {
	return SwitcherConfig{Profile: core.GroupProfile{Mode: core.ModeTranscode, Format: core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecH264, Width: 128, Height: 128, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: "high", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 200000, MaxBitrateBPS: 300000, BufferBits: 400000, GOPFrames: 30, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 64000}}, Watchdog: media.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: 100 * time.Millisecond, KeyframeTimeout: time.Second}, Queue: media.QueueLimits{MaxPackets: 100, MaxBytes: 1 << 20, MaxPacketBytes: 1 << 16, MaxConfigBytes: 1 << 12, ResyncTimeout: time.Second}, Router: media.RouterLimits{MaxConsumers: 4, MaxRetainedBytes: 5 << 20, MaxConfigBytes: 1 << 12}, MaxAVSkew: 100 * time.Millisecond, MaxTransitionGap: 100 * time.Millisecond}
}

func switchAcceptanceConfigs(t *testing.T, epoch, revision uint64) (media.CodecConfig, media.CodecConfig) {
	t.Helper()
	// Заголовки libx264 из транспортного тестового набора описывают High/yuv420p, 128×128, 30 FPS.
	headers, err := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	if err != nil {
		t.Fatal(err)
	}
	f := switchAcceptanceConfig().Profile.Format
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: epoch, Revision: revision, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30}, Video: f.Video}, headers)
	if err != nil {
		t.Fatal(err)
	}
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: epoch, Revision: revision, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: f.Audio}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	return v, a
}

func switchAcceptancePacket(t *testing.T, c media.CodecConfig, pts, dts, duration int64, key bool) media.Packet {
	t.Helper()
	payload := []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}
	if c.Info().Track == media.TrackAudio {
		payload = switchAcceptanceAACPayload()
	} else if !key {
		payload = []byte{0, 0, 0, 2, 0x41, 0xe1}
	}
	// Срезы содержат поля независимости по H.264; декодируемость изображения здесь не утверждается.
	p, err := media.NewPacket(media.PacketInfo{PTS: pts, DTS: dts, DurationTicks: duration, Keyframe: key}, c, payload)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Нативный AAC-кодировщик FFmpeg 7.1.5 создал этот стереофонический AAC-LC AU при 48 kHz;
// независимое декодирование одного AU даёт 1024 нулевых сэмпла каждого канала.
func switchAcceptanceAACPayload() []byte { return []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c} }

func newSwitchAcceptance(t *testing.T) (*Switcher, *media.Subscription) {
	t.Helper()
	s, err := NewSwitcher(switchAcceptanceConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	sub, err := s.Router().Subscribe(switchAcceptanceConfig().Queue)
	if err != nil {
		t.Fatal(err)
	}
	return s, sub
}

func pushSwitchAcceptance(t *testing.T, s *Switcher, f Feed, attempt uint64, p media.Packet, v, a media.CodecConfig, now time.Time) {
	t.Helper()
	err := s.Push(f, attempt, p, v, a, now)
	if err != nil && !errors.Is(err, ErrUnavailable) {
		t.Fatalf("подача %v: %v", f, err)
	}
}

func switchAcceptanceNext(t *testing.T, s *media.Subscription) media.Delivery {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func tickSeconds(ticks int64, base media.TimeBase) *big.Rat {
	return new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(ticks), new(big.Int).SetUint64(uint64(base.Numerator))), new(big.Int).SetUint64(uint64(base.Denominator)))
}

func TestSwitcherPreservesCompositionAndAVOffsetAcrossFreshEpochs(t *testing.T) {
	config := switchAcceptanceConfig()
	config.Profile.VideoEncoding.MaxBFrames = 2
	s, err := NewSwitcher(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sub, err := s.Router().Subscribe(config.Queue)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	v, a := switchAcceptanceConfigs(t, 91, 7)
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, v, 2, 0, 1, true), v, a, now)
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, a, 3200, 3200, 1024, false), v, a, now)
	vd, ad := switchAcceptanceNext(t, sub), switchAcceptanceNext(t, sub)
	if vd.Packet.Config().Info().Track != media.TrackVideo || ad.Packet.Config().Info().Track != media.TrackAudio {
		t.Fatal("потеряна согласованная пара A/V")
	}
	composition := tickSeconds(vd.Packet.Info().PTS-vd.Packet.Info().DTS, vd.Packet.Config().Info().TimeBase)
	if composition.Cmp(big.NewRat(1, 15)) != 0 {
		t.Fatalf("PTS−DTS изменён: %s", composition)
	}
	if tickSeconds(vd.Packet.Info().PTS, vd.Packet.Config().Info().TimeBase).Cmp(tickSeconds(ad.Packet.Info().PTS, ad.Packet.Config().Info().TimeBase)) != 0 {
		t.Fatal("общий A/V origin разошёлся")
	}
	firstEpoch := vd.VideoConfig.Info().SourceEpoch
	firstRevision := vd.VideoConfig.Info().Revision
	// Будущий P-кадр задаёт верхнюю границу отображения; следующий B-кадр её не уменьшает.
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, v, 6, 1, 1, false), v, a, now.Add(10*time.Millisecond))
	futureFrame := switchAcceptanceNext(t, sub)
	horizon := tickSeconds(futureFrame.Packet.Info().PTS+futureFrame.Packet.Info().DurationTicks, futureFrame.Packet.Config().Info().TimeBase)
	bframe := switchAcceptancePacket(t, v, 3, 2, 1, false)
	bframe, err = media.NewPacket(bframe.Info(), v, []byte{0, 0, 0, 3, 0x01, 0xa8, 0x40})
	if err != nil {
		t.Fatal(err)
	}
	pushSwitchAcceptance(t, s, Fallback, 1, bframe, v, a, now.Add(15*time.Millisecond))
	lastDisplayed := switchAcceptanceNext(t, sub)
	if tickSeconds(lastDisplayed.Packet.Info().PTS+lastDisplayed.Packet.Info().DurationTicks, lastDisplayed.Packet.Config().Info().TimeBase).Cmp(horizon) >= 0 {
		t.Fatal("вход не создал перестановку presentation PTS для проверки верхней границы")
	}
	lv, la := switchAcceptanceConfigs(t, 1, 1)
	pushSwitchAcceptance(t, s, Live, 1, switchAcceptancePacket(t, lv, -10, -12, 1, true), lv, la, now.Add(20*time.Millisecond))
	pushSwitchAcceptance(t, s, Live, 1, switchAcceptancePacket(t, la, -16000, -16000, 1024, false), lv, la, now.Add(20*time.Millisecond))
	pushSwitchAcceptance(t, s, Live, 1, switchAcceptancePacket(t, lv, -9, -11, 1, true), lv, la, now.Add(150*time.Millisecond))
	pushSwitchAcceptance(t, s, Live, 1, switchAcceptancePacket(t, la, -14400, -14400, 1024, false), lv, la, now.Add(150*time.Millisecond))
	vd, ad = switchAcceptanceNext(t, sub), switchAcceptanceNext(t, sub)
	if s.Status().Source != Live || !s.Status().Available {
		t.Fatalf("устойчивый LIVE не выбран: %+v", s.Status())
	}
	if tickSeconds(vd.Packet.Info().DTS, vd.Packet.Config().Info().TimeBase).Cmp(horizon) < 0 {
		t.Fatalf("новый decode наложился на прежний presentation horizon: %+v", vd.Packet.Info())
	}
	if tickSeconds(vd.Packet.Info().PTS-vd.Packet.Info().DTS, vd.Packet.Config().Info().TimeBase).Cmp(big.NewRat(1, 15)) != 0 {
		t.Fatal("композиционное смещение потеряно при переключении")
	}
	if tickSeconds(vd.Packet.Info().PTS, vd.Packet.Config().Info().TimeBase).Cmp(tickSeconds(ad.Packet.Info().PTS, ad.Packet.Config().Info().TimeBase)) != 0 {
		t.Fatal("переключение независимо обнулило дорожки")
	}
	if vd.VideoConfig.Info().SourceEpoch <= firstEpoch || vd.VideoConfig.Info().Revision <= firstRevision || vd.VideoConfig.Info().SourceEpoch != ad.AudioConfig.Info().SourceEpoch || !vd.IsCurrent() || !ad.IsCurrent() {
		t.Fatal("новая выходная epoch/config несогласованна")
	}
}

func TestSwitcherRequiresBothTracksAndBitstreamRandomAccess(t *testing.T) {
	for _, mode := range []string{"video-only", "audio-only", "false-key-flag", "missing-pps", "truncated-aac", "aac-without-end"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := newSwitchAcceptance(t)
			now := time.Unix(200, 0)
			v, a := switchAcceptanceConfigs(t, 1, 1)
			p := switchAcceptancePacket(t, v, 0, 0, 1, true)
			if mode == "false-key-flag" {
				var err error
				p, err = media.NewPacket(p.Info(), v, []byte{0, 0, 0, 2, 0x41, 0xe1})
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-pps" {
				headers := v.Headers()
				headers = headers[:33]
				headers[32] = 0
				var err error
				v, err = media.NewCodecConfig(v.Info(), headers)
				if err != nil {
					t.Fatal(err)
				}
				p = switchAcceptancePacket(t, v, 0, 0, 1, true)
			}
			if mode != "audio-only" {
				err := s.Push(Fallback, 1, p, v, a, now)
				if err == nil && mode == "missing-pps" {
					t.Fatal("повреждённый avcC принят")
				}
			}
			if mode != "video-only" {
				audio := switchAcceptancePacket(t, a, 0, 0, 1024, false)
				if mode == "truncated-aac" {
					var err error
					audio, err = media.NewPacket(audio.Info(), a, []byte{0})
					if err != nil {
						t.Fatal(err)
					}
				}
				if mode == "aac-without-end" {
					var err error
					audio, err = media.NewPacket(audio.Info(), a, []byte{0x20, 0, 0, 0, 0, 0, 0})
					if err != nil {
						t.Fatal(err)
					}
				}
				err := s.Push(Fallback, 1, audio, v, a, now)
				if (mode == "truncated-aac" || mode == "aac-without-end") && !errors.Is(err, codec.ErrCodec) {
					t.Fatalf("обрезанный AAC SCE принят как полная единица доступа: %v", err)
				}
			}
			if status := s.Status(); status.Available {
				t.Fatalf("непригодный источник объявлен доступным: %+v", status)
			}
		})
	}
}

func TestSwitcherRejectsRegressingAttemptAndKeepsSubscriptionThroughPause(t *testing.T) {
	s, sub := newSwitchAcceptance(t)
	now := time.Unix(300, 0)
	v, a := switchAcceptanceConfigs(t, 1, 1)
	pushSwitchAcceptance(t, s, Fallback, 2, switchAcceptancePacket(t, v, 0, 0, 1, true), v, a, now)
	pushSwitchAcceptance(t, s, Fallback, 2, switchAcceptancePacket(t, a, 0, 0, 1024, false), v, a, now)
	switchAcceptanceNext(t, sub)
	switchAcceptanceNext(t, sub)
	if err := s.Push(Fallback, 1, switchAcceptancePacket(t, v, 1, 1, 1, true), v, a, now); !errors.Is(err, ErrFeed) {
		t.Fatalf("старый attempt принят: %v", err)
	}
	if err := s.SetPaused(true, now); err != nil {
		t.Fatal(err)
	}
	lv, la := switchAcceptanceConfigs(t, 2, 2)
	for i := int64(0); i < 3; i++ {
		at := now.Add(time.Duration(i+1) * 150 * time.Millisecond)
		pushSwitchAcceptance(t, s, Live, 1, switchAcceptancePacket(t, lv, i, i, 1, true), lv, la, at)
		pushSwitchAcceptance(t, s, Live, 1, switchAcceptancePacket(t, la, i*1600, i*1600, 1024, false), lv, la, at)
	}
	if status := s.Status(); status.Source != Fallback || !status.Paused || !status.Available {
		t.Fatalf("OBS снял ручную паузу: %+v", status)
	}
	pushSwitchAcceptance(t, s, Fallback, 2, switchAcceptancePacket(t, v, 1, 1, 1, true), v, a, now.Add(500*time.Millisecond))
	pushSwitchAcceptance(t, s, Fallback, 2, switchAcceptancePacket(t, a, 1600, 1600, 1024, false), v, a, now.Add(500*time.Millisecond))
	if d := switchAcceptanceNext(t, sub); !bytes.Equal(d.Packet.Payload(), switchAcceptancePacket(t, v, 1, 1, 1, true).Payload()) {
		t.Fatal("пауза подменила fallback")
	}
	select {
	case <-sub.Done():
		t.Fatal("переход закрыл подписку")
	default:
	}
	if err := s.Fail(Fallback, 2, errors.New("Файл утрачен"), now.Add(500*time.Millisecond)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("pause без fallback должна сообщить unavailable: %v", err)
	}
	if s.Status().Available {
		t.Fatal("пауза без пригодного fallback объявлена успешной")
	}
}

func TestSwitcherAV1RationalCadenceRemainsContinuousOverRepeatedLoops(t *testing.T) {
	c := switchAcceptanceConfig()
	c.Profile = acceptanceRuntimeProfile(core.CodecAV1, 640, 360)
	c.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 30000, Denominator: 1001}
	s, err := NewSwitcher(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	sub, err := s.Router().Subscribe(c.Queue)
	if err != nil {
		t.Fatal(err)
	}
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 700, Revision: 90, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30000}, Video: c.Profile.Format.Video}, acceptanceVideoHeadersAtRate(core.CodecAV1, 640, 360, c.Profile.Format.Video.FrameRate))
	if err != nil {
		t.Fatal(err)
	}
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 700, Revision: 90, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: c.Profile.Format.Audio}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	var audio int64
	var outputEpoch uint64
	last := map[media.Track]*big.Rat{}
	for frame := int64(0); frame < 180; frame++ {
		at := now.Add(time.Duration(frame) * 1001 * time.Second / 30000)
		p, err := media.NewPacket(media.PacketInfo{PTS: frame * 1001, DTS: frame * 1001, DurationTicks: 1001, Keyframe: false}, v, append(v.Headers()[4:], 0x1a, 1, 0x10))
		if err != nil {
			t.Fatal(err)
		}
		pushSwitchAcceptance(t, s, Fallback, 1, p, v, a, at)
		count := 1
		for audio*30000 < (frame+1)*1001*48000 {
			p := switchAcceptancePacket(t, a, audio, audio, 1024, false)
			pushSwitchAcceptance(t, s, Fallback, 1, p, v, a, at)
			audio += 1024
			count++
		}
		for i := 0; i < count; i++ {
			d := switchAcceptanceNext(t, sub)
			info := d.Packet.Info()
			track := d.Packet.Config().Info().Track
			base := d.Packet.Config().Info().TimeBase
			stamp := tickSeconds(info.DTS, base)
			duration := tickSeconds(info.DurationTicks, base)
			want := big.NewRat(1001, 30000)
			if track == media.TrackAudio {
				want = big.NewRat(1024, 48000)
			}
			if duration.Cmp(want) != 0 {
				t.Fatalf("rational duration %s: %s != %s", track, duration, want)
			}
			if previous := last[track]; previous != nil && new(big.Rat).Sub(stamp, previous).Cmp(want) != 0 {
				t.Fatalf("разрыв рациональной петли %s на кадре %d", track, frame)
			}
			last[track] = stamp
			if outputEpoch == 0 {
				outputEpoch = d.VideoConfig.Info().SourceEpoch
			}
			if d.VideoConfig.Info().SourceEpoch != outputEpoch {
				t.Fatal("обычная непрерывная петля сменила epoch")
			}
		}
	}
}

func TestSwitcherDoesNotInferUnknownTailOrHealthFromDuplicates(t *testing.T) {
	s, _ := newSwitchAcceptance(t)
	now := time.Unix(1100, 0)
	v, a := switchAcceptanceConfigs(t, 1, 1)
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, v, 0, 0, 0, true), v, a, now)
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, a, 0, 0, 0, false), v, a, now)
	if s.Status().Available {
		t.Fatal("неизвестная длительность хвоста заменена догадкой")
	}
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, v, 1, 1, 1, false), v, a, now.Add(time.Millisecond))
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, a, 1024, 1024, 1024, false), v, a, now.Add(time.Millisecond))
	if !s.Status().Available {
		t.Fatal("bounded lookahead не завершил первые AU")
	}
	duplicate := switchAcceptancePacket(t, v, 1, 1, 1, true)
	if err := s.Push(Fallback, 1, duplicate, v, a, now.Add(900*time.Millisecond)); !errors.Is(err, ErrFeed) {
		t.Fatalf("дубликат принят как продвижение: %v", err)
	}
	if err := s.Tick(now.Add(1100 * time.Millisecond)); !errors.Is(err, ErrUnavailable) || s.Status().Available {
		t.Fatalf("дубликат сохранил ложное здоровье: %+v error=%v", s.Status(), err)
	}
}

func TestSwitcherDelayedControlAndFallbackFailurePreserveHealthyLive(t *testing.T) {
	s, _ := newSwitchAcceptance(t)
	now := time.Unix(1200, 0)
	v, a := switchAcceptanceConfigs(t, 1, 1)
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, v, 0, 0, 1, true), v, a, now)
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, a, 0, 0, 1024, false), v, a, now)
	lv, la := switchAcceptanceConfigs(t, 2, 2)
	for i, offset := range []time.Duration{10 * time.Millisecond, 150 * time.Millisecond} {
		stamp := int64(i)
		at := now.Add(offset)
		pushSwitchAcceptance(t, s, Live, 1, switchAcceptancePacket(t, lv, stamp, stamp, 1, true), lv, la, at)
		pushSwitchAcceptance(t, s, Live, 1, switchAcceptancePacket(t, la, stamp*1600, stamp*1600, 1024, false), lv, la, at)
	}
	if s.Status().Source != Live || !s.Status().Available {
		t.Fatal("нет пригодного LIVE после recovery")
	}
	before := s.Status()
	if err := s.Tick(now.Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if after := s.Status(); after.Source != Live || !after.Available || after.Epoch != before.Epoch {
		t.Fatalf("запаздавшее control-событие откатило часы здоровья: %+v", after)
	}
	if err := s.Fail(Fallback, 1, errors.New("Недоступен файл заглушки"), now.Add(160*time.Millisecond)); err != nil {
		t.Fatalf("отказ fallback сломал пригодный LIVE: %v", err)
	}
	if after := s.Status(); after.Source != Live || !after.Available {
		t.Fatalf("отказ невыбранного источника изменил доступную подачу: %+v", after)
	}
}

func TestSwitcherRefusesCutBeyondConfiguredPresentationGap(t *testing.T) {
	c := switchAcceptanceConfig()
	c.MaxTransitionGap = time.Millisecond
	s, err := NewSwitcher(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	sub, err := s.Router().Subscribe(c.Queue)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1300, 0)
	v, a := switchAcceptanceConfigs(t, 1, 1)
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, v, 0, 0, 1, true), v, a, now)
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, a, 0, 0, 1024, false), v, a, now)
	first := switchAcceptanceNext(t, sub)
	_ = switchAcceptanceNext(t, sub)
	oldPresentationEnd := tickSeconds(first.Packet.Info().PTS+first.Packet.Info().DurationTicks, first.Packet.Config().Info().TimeBase)
	lv, la := switchAcceptanceConfigs(t, 2, 2)
	for i, offset := range []time.Duration{10 * time.Millisecond, 150 * time.Millisecond} {
		stamp := int64(i)
		at := now.Add(offset)
		_ = s.Push(Live, 1, switchAcceptancePacket(t, lv, stamp+2, stamp, 1, true), lv, la, at)
		_ = s.Push(Live, 1, switchAcceptancePacket(t, la, (stamp+2)*1600, (stamp+2)*1600, 1024, false), lv, la, at)
	}
	if status := s.Status(); status.Source == Live && status.Available {
		d := switchAcceptanceNext(t, sub)
		gap := new(big.Rat).Sub(tickSeconds(d.Packet.Info().PTS, d.Packet.Config().Info().TimeBase), oldPresentationEnd)
		if gap.Cmp(big.NewRat(1, 1000)) > 0 {
			t.Fatalf("принят cut с presentation gap %s при бюджете 1/1000 секунды", gap)
		}
	}
}
