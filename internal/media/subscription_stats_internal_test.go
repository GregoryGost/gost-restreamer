package media

import (
	"errors"
	"math"
	"testing"
	"time"
)

// Проверяет насыщение потерь только при переполнении, неизменность срока синхронизации и терминальное
// завершение при исчерпании поколений.
func TestSubscriptionStatsIdentityExhaustionAndSaturation(t *testing.T) {
	// r содержит маршрутизатор для проверки крайних значений счётчиков подписки. err сообщает об
	// отказе его создания.
	r, err := NewRouter(RouterLimits{MaxConsumers: 1, MaxRetainedBytes: 100, MaxConfigBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает маршрутизатор после проверки исчерпания поколений и насыщения счётчиков, сообщая ошибку
		// закрытия.

		// err сообщает об отказе закрытия тестового маршрутизатора.
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	// sub содержит подписку, поколение и счётчик потерь которой доводятся до предела.
	sub, err := r.Subscribe(QueueLimits{MaxPackets: 2, MaxBytes: 16, MaxPacketBytes: 8, MaxConfigBytes: 3, ResyncTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	// originalDeadline сохраняет срок первой синхронизации для проверки, что сброс его не
	// продлевает.
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
	// got содержит итоговый снимок после исчерпания поколений и переполнения счётчика потерь.
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
