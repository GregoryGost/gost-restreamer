package media_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media"
)

func TestAcceptanceRouterAudioCutUsesPresentationTimeAcrossClocks(t *testing.T) {
	r, source, v, a := routerFor(t)
	sub := subscribeRouter(t, r, routerLimits())
	// Видео показывается в -1/2 с, а декодируется в -3/5 с.
	// Первый аудиопакет начинается на один отсчёт раньше точки показа.
	before := routerPacket(t, a, -24001, -24001, false, 1)
	aligned := routerPacket(t, a, -24000, -24000, false, 1)
	video := routerPacket(t, v, -45000, -54000, true, 1)
	for _, p := range []media.Packet{before, aligned, video} {
		publishRouter(t, source, p)
	}
	if got := nextRouter(t, sub); got.Packet.Info() != video.Info() || !got.IsCurrent() {
		t.Fatalf("неверный видеосрез: %+v", got.Packet.Info())
	}
	if got := nextRouter(t, sub); got.Packet.Info() != aligned.Info() {
		t.Fatalf("на срез попало аудио до точки показа: %+v", got.Packet.Info())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sub.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("отмена ожидания проигнорирована:", err)
	}
}

func TestAcceptanceRouterHeadersAndPacketFloodCannotRenewDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, source, v, a := routerFor(t)
		q := routerLimits()
		q.MaxPackets = 2
		sub := subscribeRouter(t, r, q)
		for i := range 9 {
			<-time.After(100 * time.Millisecond)
			vi := v.Info()
			vi.Revision += uint64(i + 1)
			v = configFor(t, vi)
			if err := source.UpdateConfigs(v, a); err != nil {
				t.Fatal(err)
			}
			publishRouter(t, source, routerPacket(t, v, int64(i), int64(i), true, 1))
			publishRouter(t, source, routerPacket(t, v, int64(i), int64(i), true, 1))
		}
		<-time.After(100 * time.Millisecond)
		synctest.Wait()
		if !errors.Is(sub.Err(), media.ErrResyncTimeout) {
			t.Fatal("неполный поток продлил исходный срок resync:", sub.Err())
		}
		select {
		case <-sub.Done():
		default:
			t.Fatal("терминальная подписка не закрыла Done")
		}
		if _, err := sub.Next(context.Background()); !errors.Is(err, media.ErrResyncTimeout) {
			t.Fatal("причина timeout потеряна:", err)
		}
	})
}

func TestAcceptanceRouterSlowQueueCannotHoldHealthyConsumer(t *testing.T) {
	r, source, v, a := routerFor(t)
	q := routerLimits()
	q.MaxPackets = 2
	slow := subscribeRouter(t, r, q)
	fast := subscribeRouter(t, r, routerLimits())
	for _, p := range []media.Packet{routerPacket(t, v, 0, 0, true, 1), routerPacket(t, a, 0, 0, false, 1)} {
		publishRouter(t, source, p)
	}
	nextRouter(t, fast)
	nextRouter(t, fast)
	for i := int64(1); i <= 100; i++ {
		p := routerPacket(t, v, i, i, false, 1)
		publishRouter(t, source, p)
		if got := nextRouter(t, fast); got.Packet.Info() != p.Info() || !got.IsCurrent() {
			t.Fatalf("переполнение соседа изменило здоровую доставку %d", i)
		}
	}
	// Аудио 1 с совпадает с видео 1 с, несмотря на разные часы.
	key := routerPacket(t, v, 90000, 90000, true, 1)
	publishRouter(t, source, key)
	publishRouter(t, source, routerPacket(t, a, 48000, 48000, false, 1))
	if got := nextRouter(t, slow); got.Packet.Info() != key.Info() {
		t.Fatal("медленная очередь выпустила зависимый пакет после переполнения")
	}
	if got := nextRouter(t, slow); got.Packet.Config().Info().Track != media.TrackAudio || got.Packet.Info().PTS != 48000 {
		t.Fatal("восстановление не сохранило согласованное аудио")
	}
}
