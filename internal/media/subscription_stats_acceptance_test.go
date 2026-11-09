package media_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media"
)

func acceptanceQueueLosses(d media.SubscriptionDrops) uint64 {
	return d.Overflow + d.Oversized + d.ConfigRejected + d.AwaitingKeyframe + d.AudioBeforeCut + d.SourceReset + d.ConfigReset + d.Closed
}

func TestSubscriptionAcceptanceOverflowInvalidatesTransferredGeneration(t *testing.T) {
	r, src, v, a := routerFor(t)
	limits := routerLimits()
	limits.MaxPackets = 3
	s := subscribeRouter(t, r, limits)
	neighbor := subscribeRouter(t, r, routerLimits())
	publishRouter(t, src, routerPacket(t, a, 0, 0, false, 2))
	publishRouter(t, src, routerPacket(t, a, 48000, 48000, false, 3))
	if got := s.Snapshot(); got.PendingPackets != 2 || got.QueuedPackets != 0 || got.PacketBytes != 11 {
		t.Fatal(got)
	}
	publishRouter(t, src, routerPacket(t, v, 90000, 90000, true, 4))
	old := nextRouter(t, s)
	if old.Packet.Info().PTS != 90000 || !old.IsCurrent() {
		t.Fatal("не выдан независимый видеокадр")
	}
	if got := s.Snapshot(); got.PendingPackets != 0 || got.QueuedPackets != 1 || got.PacketBytes != 6 || got.Drops.AudioBeforeCut != 1 {
		t.Fatal(got)
	}
	publishRouter(t, src, routerPacket(t, v, 90001, 90001, false, 5))
	publishRouter(t, src, routerPacket(t, v, 90002, 90002, false, 6))
	publishRouter(t, src, routerPacket(t, v, 90003, 90003, true, 7))
	got := s.Snapshot()
	if old.IsCurrent() || got.Generation <= old.Generation || !got.Resync || got.QueuedPackets != 1 || got.PacketBytes != 10 || got.ConfigBytes != 6 || got.Drops.Overflow != 3 || acceptanceQueueLosses(got.Drops) != 4 {
		t.Fatalf("переполнение без смены заголовков: %+v", got)
	}
	publishRouter(t, src, routerPacket(t, a, 48002, 48002, false, 8))
	video, audio := nextRouter(t, s), nextRouter(t, s)
	if !video.IsCurrent() || !audio.IsCurrent() || video.Generation != got.Generation || audio.Generation != got.Generation || video.Packet.Info().PTS != 90003 || audio.Packet.Info().PTS != 48002 {
		t.Fatal("потерян пакет, вызвавший сброс, либо повторено старое поколение")
	}
	// Сосед сохраняет исходный кадр: переполнение принадлежит одной Subscription.
	if nextRouter(t, neighbor).Packet.Info().PTS != 90000 {
		t.Fatal("переполнение отозвало соседнюю очередь")
	}
	if got = s.Snapshot(); got.PacketBytes != 0 || got.QueuedPackets != 0 || acceptanceQueueLosses(got.Drops) != 4 {
		t.Fatal(got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got = s.Snapshot(); acceptanceQueueLosses(got.Drops) != 4 {
		t.Fatal("Next передал владение, Close повторно посчитал AU", got)
	}
}

func TestSubscriptionAcceptanceResetAndCloseConserveOwnedPackets(t *testing.T) {
	r, src, v, a := routerFor(t)
	s := subscribeRouter(t, r, routerLimits())
	publishRouter(t, src, routerPacket(t, v, 0, 0, false, 1))
	publishRouter(t, src, routerPacket(t, a, 0, 0, false, 2))
	publishRouter(t, src, routerPacket(t, v, 90000, 90000, true, 3))
	publishRouter(t, src, routerPacket(t, a, 48000, 48000, false, 4))
	issued := nextRouter(t, s)
	vi := v.Info()
	vi.Revision++
	v2 := configFor(t, vi)
	if err := src.UpdateConfigs(v2, a); err != nil {
		t.Fatal(err)
	}
	if issued.IsCurrent() {
		t.Fatal("смена конфигурации сохранила старую выдачу")
	}
	publishRouter(t, src, routerPacket(t, a, 48001, 48001, false, 5))
	if err := src.Disconnect(); err != nil {
		t.Fatal(err)
	}
	got := s.Snapshot()
	want := media.SubscriptionDrops{AwaitingKeyframe: 1, AudioBeforeCut: 1, ConfigReset: 1, SourceReset: 1}
	if got.Drops != want || got.PacketBytes != 0 || got.ConfigBytes != 0 || got.QueuedPackets+got.PendingPackets != 0 {
		t.Fatal(got)
	}
	if acceptanceQueueLosses(got.Drops)+1 != 5 {
		t.Fatal("нарушен баланс принятых AU и выданного пакета")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got = s.Snapshot(); got.Drops != want {
		t.Fatal("повторно учтены выданные/потерянные AU", got)
	}
}

func TestSubscriptionAcceptanceRepeatedResetCannotRenewTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, src, v, a := routerFor(t)
		limits := routerLimits()
		limits.MaxPackets = 1
		limits.ResyncTimeout = 100 * time.Millisecond
		s := subscribeRouter(t, r, limits)
		time.Sleep(40 * time.Millisecond)
		vi := v.Info()
		vi.Revision++
		v2 := configFor(t, vi)
		if err := src.UpdateConfigs(v2, a); err != nil {
			t.Fatal(err)
		}
		publishRouter(t, src, routerPacket(t, a, 0, 0, false, 1))
		time.Sleep(40 * time.Millisecond)
		publishRouter(t, src, routerPacket(t, a, 1, 1, false, 1))
		time.Sleep(20 * time.Millisecond)
		synctest.Wait()
		select {
		case <-s.Done():
		default:
			t.Fatal("повторный сброс продлил первоначальный срок")
		}
		if !errors.Is(s.Err(), media.ErrResyncTimeout) {
			t.Fatal(s.Err())
		}
		got := s.Snapshot()
		if got.Drops.Overflow != 1 || got.Drops.Closed != 1 || acceptanceQueueLosses(got.Drops) != 2 || got.PacketBytes != 0 || got.ConfigBytes != 0 {
			t.Fatal(got)
		}
		if _, err := s.Next(context.Background()); !errors.Is(err, media.ErrResyncTimeout) {
			t.Fatal(err)
		}
	})
}

func TestSubscriptionAcceptanceConcurrentTransferConservation(t *testing.T) {
	r, src, v, a := routerFor(t)
	limits := routerLimits()
	limits.MaxPackets = 4
	s := subscribeRouter(t, r, limits)
	var delivered atomic.Uint64
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for {
			_, err := s.Next(context.Background())
			if err != nil {
				return
			}
			delivered.Add(1)
		}
	}()
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 200 {
			publishRouter(t, src, routerPacket(t, v, 0, 0, true, 1))
			publishRouter(t, src, routerPacket(t, a, 0, 0, false, 2))
		}
	})
	wg.Go(func() {
		for range 400 {
			got := s.Snapshot()
			if got.QueuedPackets+got.PendingPackets > 4 || got.PacketBytes > 20 || got.ConfigBytes != 6 {
				t.Error("несогласованный снимок", got)
			}
		}
	})
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-consumerDone:
	case <-time.After(time.Second):
		t.Fatal("Close не присоединил ожидающий Next")
	}
	got := s.Snapshot()
	if got.QueuedPackets+got.PendingPackets != 0 || got.PacketBytes != 0 || acceptanceQueueLosses(got.Drops)+delivered.Load() != 400 {
		t.Fatalf("AU посчитан дважды или потерян: выдано=%d, %+v", delivered.Load(), got)
	}
}
