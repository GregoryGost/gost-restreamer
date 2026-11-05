package media_test

import (
	"context"
	"sync"
	"testing"
)

func TestSubscriptionStatsOverflowInvalidatesIssuedDelivery(t *testing.T) {
	r, source, v, a := routerFor(t)
	limits := routerLimits()
	limits.MaxPackets = 2
	sub := subscribeRouter(t, r, limits)
	publishRouter(t, source, routerPacket(t, v, 0, 0, true, 1))
	publishRouter(t, source, routerPacket(t, a, 0, 0, false, 1))
	issued := nextRouter(t, sub)
	publishRouter(t, source, routerPacket(t, v, 1, 1, false, 1))
	publishRouter(t, source, routerPacket(t, v, 2, 2, true, 1))
	got := sub.Snapshot()
	if issued.IsCurrent() || got.Generation <= issued.Generation || got.Drops.Overflow != 2 || got.Drops.AwaitingKeyframe != 0 || got.QueuedPackets != 1 || got.PendingPackets != 0 || got.PacketBytes != 4 || got.ConfigBytes != 6 || !got.Resync {
		t.Fatalf("снимок после переполнения: %+v", got)
	}
	publishRouter(t, source, routerPacket(t, a, 48000, 48000, false, 1))
	fresh := nextRouter(t, sub)
	if !fresh.IsCurrent() || fresh.Generation != got.Generation || fresh.Packet.Info().PTS != 2 {
		t.Fatal("принятый AU потерян или прежнее поколение ожило")
	}
	if sub.Snapshot().Drops.Overflow != 2 {
		t.Fatal("принятый AU посчитан потерей")
	}
}

func TestSubscriptionStatsEveryQueueDropHasOneReason(t *testing.T) {
	r, source, v, a := routerFor(t)
	sub := subscribeRouter(t, r, routerLimits())
	publishRouter(t, source, routerPacket(t, v, 0, 0, false, 1))
	publishRouter(t, source, routerPacket(t, a, 0, 0, false, 1))
	pending := sub.Snapshot()
	if pending.PendingPackets != 1 || pending.QueuedPackets != 0 || pending.PacketBytes != 4 || pending.Drops.AwaitingKeyframe != 1 {
		t.Fatal(pending)
	}
	publishRouter(t, source, routerPacket(t, v, 90000, 90000, true, 1))
	publishRouter(t, source, routerPacket(t, a, 1, 1, false, 1))
	publishRouter(t, source, routerPacket(t, a, 48000, 48000, false, 1))
	got := sub.Snapshot()
	if got.Resync || got.Drops.AudioBeforeCut != 2 || got.QueuedPackets != 2 || got.PacketBytes != 8 || got.PendingPackets != 0 {
		t.Fatalf("граница A/V: %+v", got)
	}
	vi := v.Info()
	vi.Revision++
	v2 := configFor(t, vi)
	if err := source.UpdateConfigs(v2, a); err != nil {
		t.Fatal(err)
	}
	got = sub.Snapshot()
	if got.Drops.ConfigReset != 2 || got.Drops.SourceReset != 0 || got.Drops.Closed != 0 || got.PacketBytes != 0 {
		t.Fatal(got)
	}
	publishRouter(t, source, routerPacket(t, a, 48001, 48001, false, 1))
	if err := source.Disconnect(); err != nil {
		t.Fatal(err)
	}
	got = sub.Snapshot()
	if got.Drops.SourceReset != 1 || got.ConfigBytes != 0 || got.PacketBytes != 0 {
		t.Fatal(got)
	}
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	if sub.Snapshot().Drops != got.Drops {
		t.Fatal("повторный Close повторно учёл потери")
	}
}

func TestSubscriptionStatsRejectionsAndOwnerClose(t *testing.T) {
	r, source, v, a := routerFor(t)
	limits := routerLimits()
	limits.MaxConfigBytes = 2
	rejected := subscribeRouter(t, r, limits)
	publishRouter(t, source, routerPacket(t, v, 0, 0, true, 1))
	publishRouter(t, source, routerPacket(t, a, 0, 0, false, 1))
	if got := rejected.Snapshot(); got.Drops.ConfigRejected != 2 || got.PacketBytes != 0 || got.ConfigBytes != 0 || got.QueuedPackets != 0 {
		t.Fatal(got)
	}
	limits = routerLimits()
	limits.MaxPacketBytes, limits.MaxConfigBytes = 8, 3
	small := subscribeRouter(t, r, limits)
	publishRouter(t, source, routerPacket(t, v, 1, 1, true, 1))
	publishRouter(t, source, routerPacket(t, a, 1, 1, false, 1))
	publishRouter(t, source, routerPacket(t, v, 2, 2, true, 10))
	got := small.Snapshot()
	if got.Drops.Overflow != 2 || got.Drops.Oversized != 1 || got.PacketBytes != 0 {
		t.Fatal(got)
	}
	publishRouter(t, source, routerPacket(t, a, 2, 2, false, 1))
	if err := small.Close(); err != nil {
		t.Fatal(err)
	}
	got = small.Snapshot()
	if got.Drops.Closed != 1 || got.Drops.Oversized != 1 || got.Drops.Overflow != 2 || got.PacketBytes != 0 || got.ConfigBytes != 0 || got.PendingPackets != 0 {
		t.Fatal(got)
	}
}

func TestSubscriptionStatsConcurrentSnapshotsAndTransfers(t *testing.T) {
	r, source, v, a := routerFor(t)
	sub := subscribeRouter(t, r, routerLimits())
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 100 {
			publishRouter(t, source, routerPacket(t, v, 0, 0, true, 1))
			publishRouter(t, source, routerPacket(t, a, 0, 0, false, 1))
		}
	})
	wg.Go(func() {
		for range 100 {
			got := sub.Snapshot()
			if got.QueuedPackets+got.PendingPackets > 16 || got.PacketBytes+got.ConfigBytes > 1024 {
				t.Error("несогласованный снимок", got)
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := sub.Next(ctx); err != nil {
				return
			}
		}
	}()
	wg.Wait()
	cancel()
	<-done
}
