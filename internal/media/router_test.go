package media_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media"
)

func routerFor(t *testing.T) (*media.Router, *media.Source, media.CodecConfig, media.CodecConfig) {
	t.Helper()
	r, err := media.NewRouter(media.RouterLimits{MaxConsumers: 8, MaxRetainedBytes: 16384, MaxConfigBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	source, err := r.BeginSource()
	if err != nil {
		t.Fatal(err)
	}
	v, a := routerConfigs(t, source.Epoch())
	if err := source.UpdateConfigs(v, a); err != nil {
		t.Fatal(err)
	}
	return r, source, v, a
}

func routerConfigs(t *testing.T, epoch uint64) (media.CodecConfig, media.CodecConfig) {
	t.Helper()
	v, a := videoInfo(), audioInfo()
	v.SourceEpoch, a.SourceEpoch = epoch, epoch
	return configFor(t, v), configFor(t, a)
}

func routerLimits() media.QueueLimits {
	return media.QueueLimits{MaxPackets: 16, MaxBytes: 1024, MaxPacketBytes: 128, MaxConfigBytes: 64, ResyncTimeout: time.Second}
}

func subscribeRouter(t *testing.T, r *media.Router, l media.QueueLimits) *media.Subscription {
	t.Helper()
	s, err := r.Subscribe(l)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func routerPacket(t *testing.T, c media.CodecConfig, pts, dts int64, key bool, size int) media.Packet {
	t.Helper()
	p, err := media.NewPacket(media.PacketInfo{PTS: pts, DTS: dts, DurationTicks: 1, Keyframe: key}, c, make([]byte, size))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func publishRouter(t *testing.T, s *media.Source, p media.Packet) {
	t.Helper()
	if err := s.Publish(p); err != nil {
		t.Fatal(err)
	}
}

func nextRouter(t *testing.T, s *media.Subscription) media.Delivery {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRouterAlignedAudioAndSignedPacketTransfer(t *testing.T) {
	r, source, v, a := routerFor(t)
	sub := subscribeRouter(t, r, routerLimits())
	publishRouter(t, source, routerPacket(t, a, -1024, -1024, false, 1))
	publishRouter(t, source, routerPacket(t, a, 0, 0, false, 1))
	publishRouter(t, source, routerPacket(t, v, 0, -3000, true, 1))
	video, audio := nextRouter(t, sub), nextRouter(t, sub)
	if video.Packet.Config().Info().Track != media.TrackVideo || audio.Packet.Info().PTS != 0 || audio.Packet.Config().Info().Track != media.TrackAudio {
		t.Fatal("неверная согласованная точка начала")
	}
	if !video.IsCurrent() || !sub.IsCurrent(audio) {
		t.Fatal("текущая доставка признана устаревшей")
	}
	publishRouter(t, source, routerPacket(t, v, -5000, -3000, false, 1))
	got := nextRouter(t, sub)
	if got.Packet.Info().PTS != -5000 || got.Packet.Info().DTS != -3000 {
		t.Fatal("маршрутизатор изменил знаковые PTS/DTS либо запретил равные DTS")
	}
	if err := source.Publish(routerPacket(t, v, 0, -3001, false, 1)); !errors.Is(err, media.ErrDTSRegression) {
		t.Fatalf("уменьшение DTS принято: %v", err)
	}
	if !video.IsCurrent() {
		t.Fatal("выданный пакет ошибочно отозван после удаления из очереди")
	}
}

func TestRouterOverflowAndOversizeAffectOnlyOneConsumer(t *testing.T) {
	r, source, v, a := routerFor(t)
	small := routerLimits()
	small.MaxPackets, small.MaxPacketBytes, small.MaxConfigBytes = 2, 8, 3
	slow := subscribeRouter(t, r, small)
	fast := subscribeRouter(t, r, routerLimits())
	publishRouter(t, source, routerPacket(t, v, 0, 0, true, 1))
	publishRouter(t, source, routerPacket(t, a, 0, 0, false, 1))
	nextRouter(t, fast)
	nextRouter(t, fast)
	publishRouter(t, source, routerPacket(t, v, 1, 1, false, 1))
	if nextRouter(t, fast).Packet.Info().PTS != 1 {
		t.Fatal("переполнение соседа потеряло пакет")
	}
	publishRouter(t, source, routerPacket(t, v, 2, 2, true, 20))
	if len(nextRouter(t, fast).Packet.Payload()) != 20 {
		t.Fatal("ограничение соседа стало глобальным")
	}
	publishRouter(t, source, routerPacket(t, a, 48000, 48000, false, 1))
	publishRouter(t, source, routerPacket(t, v, 90000, 90000, true, 1))
	d1, d2 := nextRouter(t, slow), nextRouter(t, slow)
	if !d1.Packet.Info().Keyframe || d1.Packet.Info().PTS != 90000 || d2.Packet.Config().Info().Track != media.TrackAudio || d2.Packet.Info().PTS != 48000 {
		t.Fatal("после переполнения выпущен старый или зависимый пакет")
	}
}

func TestRouterConfigPairAndPublicationFences(t *testing.T) {
	r, source, v, a := routerFor(t)
	sub := subscribeRouter(t, r, routerLimits())
	publishRouter(t, source, routerPacket(t, v, 0, 0, true, 1))
	publishRouter(t, source, routerPacket(t, a, 0, 0, false, 1))
	issued := nextRouter(t, sub)
	ai := a.Info()
	ai.Revision++
	a2 := configFor(t, ai)
	if err := source.UpdateConfigs(v, a2); err != nil {
		t.Fatal(err)
	}
	if issued.IsCurrent() {
		t.Fatal("смена аудиоревизии не отозвала идентичность A/V")
	}
	if err := source.Publish(routerPacket(t, a, 0, 0, false, 1)); !errors.Is(err, media.ErrStaleSource) {
		t.Fatal("принята прежняя аудиоревизия", err)
	}
	vi := v.Info()
	vi.Revision++
	v2 := configFor(t, vi)
	if err := source.UpdateConfigs(v2, a); !errors.Is(err, media.ErrStaleSource) {
		t.Fatal("частично устаревшая пара принята", err)
	}
	if err := source.Publish(routerPacket(t, v2, 0, 0, true, 1)); !errors.Is(err, media.ErrStaleSource) {
		t.Fatal("неуспешная замена частично изменила пару", err)
	}
	publishRouter(t, source, routerPacket(t, v, 1, 1, true, 1))
	publishRouter(t, source, routerPacket(t, a2, 1, 1, false, 1))
	fresh := nextRouter(t, sub)
	if fresh.Packet.Info().PTS != 1 || fresh.AudioConfig.Info().Revision != ai.Revision {
		t.Fatal("из очереди вышел старый снимок")
	}
	replacement, err := r.ReplaceSource()
	if err != nil || replacement.Epoch() <= source.Epoch() {
		t.Fatal("epoch переиспользована", err)
	}
	if err := source.Disconnect(); !errors.Is(err, media.ErrStaleSource) {
		t.Fatal("старый Disconnect принят", err)
	}
	if err := source.UpdateConfigs(v, a2); !errors.Is(err, media.ErrStaleSource) {
		t.Fatal("старые заголовки приняты", err)
	}
	if err := source.Publish(routerPacket(t, v, 2, 2, true, 1)); !errors.Is(err, media.ErrStaleSource) {
		t.Fatal("старый пакет принят", err)
	}
	rv, ra := routerConfigs(t, replacement.Epoch())
	if err := replacement.UpdateConfigs(rv, ra); err != nil {
		t.Fatal(err)
	}
	publishRouter(t, replacement, routerPacket(t, rv, 0, 0, true, 1))
	publishRouter(t, replacement, routerPacket(t, ra, 0, 0, false, 1))
	if nextRouter(t, sub).Packet.Config().Info().SourceEpoch != replacement.Epoch() || fresh.IsCurrent() {
		t.Fatal("старый источник нарушил новую публикацию")
	}
}

func TestRouterRevisionIdentityAndExactDTSAcrossTimeBases(t *testing.T) {
	_, source, v, a := routerFor(t)
	vi := v.Info()
	vi.TimeBase = media.TimeBase{Numerator: math.MaxUint32, Denominator: math.MaxUint32}
	v1 := configFor(t, vi)
	if err := source.UpdateConfigs(v1, a); !errors.Is(err, media.ErrStaleSource) {
		t.Fatal("ревизия получила другое значение", err)
	}
	vi.Revision++
	v1 = configFor(t, vi)
	if err := source.UpdateConfigs(v1, a); err != nil {
		t.Fatal(err)
	}
	publishRouter(t, source, routerPacket(t, v1, math.MinInt64, math.MinInt64, true, 1))
	vi.Revision++
	vi.TimeBase = media.TimeBase{Numerator: 1, Denominator: 1}
	v2 := configFor(t, vi)
	if err := source.UpdateConfigs(v2, a); err != nil {
		t.Fatal(err)
	}
	publishRouter(t, source, routerPacket(t, v2, math.MinInt64, math.MinInt64, true, 1))
	publishRouter(t, source, routerPacket(t, v2, math.MaxInt64, math.MaxInt64, true, 1))
	vi.Revision++
	vi.TimeBase = media.TimeBase{Numerator: 1, Denominator: math.MaxUint32}
	v3 := configFor(t, vi)
	if err := source.UpdateConfigs(v3, a); err != nil {
		t.Fatal(err)
	}
	if err := source.Publish(routerPacket(t, v3, math.MaxInt64, math.MaxInt64, true, 1)); !errors.Is(err, media.ErrDTSRegression) {
		t.Fatalf("сравнение разных шкал переполнилось: %v", err)
	}
}

func TestRouterRetainedBudgetIncludesHeadersAndPendingAudio(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, source, v, a := routerFor(t)
		limits := routerLimits()
		limits.MaxBytes, limits.MaxPacketBytes, limits.MaxConfigBytes = 14, 4, 3
		sub := subscribeRouter(t, r, limits)
		publishRouter(t, source, routerPacket(t, a, 0, 0, false, 1))
		publishRouter(t, source, routerPacket(t, v, 0, 0, true, 1))
		if nextRouter(t, sub).Packet.Config().Info().Track != media.TrackVideo {
			t.Fatal("точный бюджет не принят")
		}
		nextRouter(t, sub)
		limits.MaxBytes = 13
		tooSmall := subscribeRouter(t, r, limits)
		publishRouter(t, source, routerPacket(t, a, 48000, 48000, false, 1))
		publishRouter(t, source, routerPacket(t, v, 90000, 90000, true, 1))
		if _, err := tooSmall.Next(context.Background()); !errors.Is(err, media.ErrResyncTimeout) {
			t.Fatal("пара превысила бюджет вместе с заголовками", err)
		}
	})
}

func TestRouterResyncDeadlineSurvivesConfigUpdatesAndDuplicates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, source, v, a := routerFor(t)
		sub := subscribeRouter(t, r, routerLimits())
		time.Sleep(400 * time.Millisecond)
		if err := source.UpdateConfigs(v, a); err != nil {
			t.Fatal(err)
		}
		vi := v.Info()
		vi.Revision++
		v2 := configFor(t, vi)
		if err := source.UpdateConfigs(v2, a); err != nil {
			t.Fatal(err)
		}
		publishRouter(t, source, routerPacket(t, v2, 0, 0, true, 1))
		time.Sleep(400 * time.Millisecond)
		publishRouter(t, source, routerPacket(t, v2, 0, 0, true, 1))
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		select {
		case <-sub.Done():
		default:
			t.Fatal("таймер не завершил подписку без Next")
		}
		if !errors.Is(sub.Err(), media.ErrResyncTimeout) {
			t.Fatal("потеряна причина завершения", sub.Err())
		}
		if _, err := sub.Next(context.Background()); !errors.Is(err, media.ErrResyncTimeout) {
			t.Fatal(err)
		}
	})
}

func TestRouterRationalAudioAlignmentAtExtremeTimestamp(t *testing.T) {
	r, source, v, a := routerFor(t)
	vi, ai := v.Info(), a.Info()
	vi.Revision++
	ai.Revision++
	vi.TimeBase = media.TimeBase{Numerator: math.MaxUint32, Denominator: 1}
	ai.TimeBase = vi.TimeBase
	v, a = configFor(t, vi), configFor(t, ai)
	if err := source.UpdateConfigs(v, a); err != nil {
		t.Fatal(err)
	}
	sub := subscribeRouter(t, r, routerLimits())
	publishRouter(t, source, routerPacket(t, a, math.MinInt64, math.MinInt64, false, 1))
	publishRouter(t, source, routerPacket(t, a, math.MaxInt64, math.MaxInt64, false, 1))
	publishRouter(t, source, routerPacket(t, v, math.MaxInt64, math.MaxInt64, true, 1))
	nextRouter(t, sub)
	if nextRouter(t, sub).Packet.Info().PTS != math.MaxInt64 {
		t.Fatal("переполнение пропустило несогласованное аудио")
	}
}

func TestRouterConsumerReservationsAndClose(t *testing.T) {
	limits := media.RouterLimits{MaxConsumers: 1, MaxRetainedBytes: 14, MaxConfigBytes: 3}
	r, err := media.NewRouter(limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	q := media.QueueLimits{MaxPackets: 2, MaxBytes: 8, MaxPacketBytes: 4, MaxConfigBytes: 3, ResyncTimeout: time.Second}
	s := subscribeRouter(t, r, q)
	if _, err := r.Subscribe(q); !errors.Is(err, media.ErrRouterBudget) {
		t.Fatal("превышено число подписчиков", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next := subscribeRouter(t, r, q)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := next.Next(context.Background()); !errors.Is(err, media.ErrRouterClosed) {
		t.Fatal(err)
	}
	if _, err := r.BeginSource(); !errors.Is(err, media.ErrRouterClosed) {
		t.Fatal(err)
	}
	if _, err := r.Subscribe(q); !errors.Is(err, media.ErrRouterClosed) {
		t.Fatal(err)
	}
}

func TestRouterConcurrentPublishReplaceAndCancellation(t *testing.T) {
	r, source, v, a := routerFor(t)
	sub := subscribeRouter(t, r, routerLimits())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, err := sub.Next(ctx)
			if err != nil {
				return
			}
		}
	}()
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 100 {
			err := source.Publish(routerPacket(t, v, 0, 0, true, 1))
			if err != nil && !errors.Is(err, media.ErrStaleSource) {
				t.Error(err)
			}
			err = source.Publish(routerPacket(t, a, 0, 0, false, 1))
			if err != nil && !errors.Is(err, media.ErrStaleSource) {
				t.Error(err)
			}
		}
	})
	wg.Go(func() {
		for range 20 {
			replacement, err := r.ReplaceSource()
			if err != nil {
				t.Error(err)
				return
			}
			rv, ra := routerConfigs(t, replacement.Epoch())
			err = replacement.UpdateConfigs(rv, ra)
			if err != nil {
				t.Error(err)
				return
			}
			publishRouter(t, replacement, routerPacket(t, rv, 0, 0, true, 1))
			publishRouter(t, replacement, routerPacket(t, ra, 0, 0, false, 1))
		}
	})
	wg.Wait()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("отмена оставила Next заблокированным")
	}
}

func TestRouterHeadersBudgetAndReservationReleaseOnTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, err := media.NewRouter(media.RouterLimits{MaxConsumers: 2, MaxRetainedBytes: 22, MaxConfigBytes: 3})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := r.Close(); err != nil {
				t.Error(err)
			}
		})
		source, err := r.BeginSource()
		if err != nil {
			t.Fatal(err)
		}
		v, a := routerConfigs(t, source.Epoch())
		if err := source.UpdateConfigs(v, a); err != nil {
			t.Fatal(err)
		}
		q := media.QueueLimits{MaxPackets: 4, MaxBytes: 16, MaxPacketBytes: 4, MaxConfigBytes: 2, ResyncTimeout: time.Second}
		s := subscribeRouter(t, r, q)
		if _, err := r.Subscribe(q); !errors.Is(err, media.ErrRouterBudget) {
			t.Fatal("сумма резервов превысила общий бюджет", err)
		}
		publishRouter(t, source, routerPacket(t, v, 0, 0, true, 1))
		publishRouter(t, source, routerPacket(t, a, 0, 0, false, 1))
		if _, err := s.Next(context.Background()); !errors.Is(err, media.ErrResyncTimeout) {
			t.Fatal("заголовки превысили бюджет подписчика", err)
		}
		replacement := subscribeRouter(t, r, q)
		if replacement.Err() != nil {
			t.Fatal("истечение срока не освободило резерв")
		}
	})
}
