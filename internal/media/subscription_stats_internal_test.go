package media

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestSubscriptionStatsIdentityExhaustionAndSaturation(t *testing.T) {
	r, err := NewRouter(RouterLimits{MaxConsumers: 1, MaxRetainedBytes: 100, MaxConfigBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	sub, err := r.Subscribe(QueueLimits{MaxPackets: 2, MaxBytes: 16, MaxPacketBytes: 8, MaxConfigBytes: 3, ResyncTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	originalDeadline := sub.deadline
	sub.generation = math.MaxUint64 - 1
	sub.drops.Overflow = math.MaxUint64 - 1
	sub.dropLocked(dropOverflow, 1)
	if sub.saturated || sub.drops.Overflow != math.MaxUint64 {
		t.Fatal("точное сложение ошибочно насыщено")
	}
	sub.dropLocked(dropOverflow, 2)
	sub.resetForLocked(time.Now(), dropConfigReset)
	if sub.generation != math.MaxUint64 || sub.deadline != originalDeadline {
		t.Fatal("сброс переиспользовал generation или продлил deadline")
	}
	sub.resetForLocked(time.Now(), dropSourceReset)
	r.mu.Unlock()
	got := sub.Snapshot()
	if got.Generation != math.MaxUint64 || !got.Saturated || got.Drops.Overflow != math.MaxUint64 || !errors.Is(sub.Err(), ErrSubscriptionGeneration) {
		t.Fatal(got, sub.Err())
	}
	select {
	case <-sub.Done():
	default:
		t.Fatal("исчерпание идентичностей не завершило подписку")
	}
}
