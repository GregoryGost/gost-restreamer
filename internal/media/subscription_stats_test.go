package media_test

import (
	"context"
	"sync"
	"testing"
)

// Проверяет отзыв выданной доставки после переполнения без смены заголовков, точный вес очереди и
// отсутствие потери вновь принятого AU.
func TestSubscriptionStatsOverflowInvalidatesIssuedDelivery(t *testing.T) {
	// r содержит тестовый маршрутизатор. source обозначает его текущий допуск публикации. v содержит
	// конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
	r, source, v, a := routerFor(t)
	// limits содержит ограничения очереди, у которой проверяется переполнение по числу пакетов.
	limits := routerLimits()
	limits.MaxPackets = 2
	// sub содержит подписку с очередью на два пакета.
	sub := subscribeRouter(t, r, limits)
	publishRouter(t, source, routerPacket(t, v, 0, 0, true, 1))
	publishRouter(t, source, routerPacket(t, a, 0, 0, false, 1))
	// issued содержит доставку, выданную до переполнения и подлежащую отзыву новым поколением.
	issued := nextRouter(t, sub)
	publishRouter(t, source, routerPacket(t, v, 1, 1, false, 1))
	publishRouter(t, source, routerPacket(t, v, 2, 2, true, 1))
	// got содержит снимок очереди и потерь сразу после переполнения.
	got := sub.Snapshot()
	if issued.IsCurrent() || got.Generation <= issued.Generation || got.Drops.Overflow != 2 || got.Drops.AwaitingKeyframe != 0 || got.QueuedPackets != 1 || got.PendingPackets != 0 || got.PacketBytes != 4 || got.ConfigBytes != 6 || !got.Resync {
		t.Fatalf("снимок после переполнения: %+v", got)
	}
	publishRouter(t, source, routerPacket(t, a, 48000, 48000, false, 1))
	// fresh содержит доставку из нового поколения после восстановления аудио и видео.
	fresh := nextRouter(t, sub)
	if !fresh.IsCurrent() || fresh.Generation != got.Generation || fresh.Packet.Info().PTS != 2 {
		t.Fatal("принятый AU потерян или прежнее поколение ожило")
	}
	if sub.Snapshot().Drops.Overflow != 2 {
		t.Fatal("принятый AU посчитан потерей")
	}
}

// Проверяет отдельный учёт ожидания ключевого кадра, раннего аудио, смены конфигурации и источника,
// без повторного учёта при Close.
func TestSubscriptionStatsEveryQueueDropHasOneReason(t *testing.T) {
	// r содержит тестовый маршрутизатор. source обозначает его текущий допуск публикации. v содержит
	// конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
	r, source, v, a := routerFor(t)
	// sub содержит подписку для раздельного учёта причин потерь.
	sub := subscribeRouter(t, r, routerLimits())
	publishRouter(t, source, routerPacket(t, v, 0, 0, false, 1))
	publishRouter(t, source, routerPacket(t, a, 0, 0, false, 1))
	// pending содержит снимок аудио, ожидающего независимого видеокадра.
	pending := sub.Snapshot()
	if pending.PendingPackets != 1 || pending.QueuedPackets != 0 || pending.PacketBytes != 4 || pending.Drops.AwaitingKeyframe != 1 {
		t.Fatal(pending)
	}
	publishRouter(t, source, routerPacket(t, v, 90000, 90000, true, 1))
	publishRouter(t, source, routerPacket(t, a, 1, 1, false, 1))
	publishRouter(t, source, routerPacket(t, a, 48000, 48000, false, 1))
	// got содержит снимок после согласованного запуска, обновляемый при последующих сбросах.
	got := sub.Snapshot()
	if got.Resync || got.Drops.AudioBeforeCut != 2 || got.QueuedPackets != 2 || got.PacketBytes != 8 || got.PendingPackets != 0 {
		t.Fatalf("граница A/V: %+v", got)
	}
	// vi содержит описание видео, ревизия которого увеличивается для отзыва очереди.
	vi := v.Info()
	vi.Revision++
	// v2 содержит видеоконфигурацию с новой ревизией.
	v2 := configFor(t, vi)
	// err сообщает об отказе установки проверяемой пары конфигураций.
	if err := source.UpdateConfigs(v2, a); err != nil {
		t.Fatal(err)
	}
	got = sub.Snapshot()
	if got.Drops.ConfigReset != 2 || got.Drops.SourceReset != 0 || got.Drops.Closed != 0 || got.PacketBytes != 0 {
		t.Fatal(got)
	}
	publishRouter(t, source, routerPacket(t, a, 48001, 48001, false, 1))
	// err сообщает об отказе отключения текущего источника.
	if err := source.Disconnect(); err != nil {
		t.Fatal(err)
	}
	got = sub.Snapshot()
	if got.Drops.SourceReset != 1 || got.ConfigBytes != 0 || got.PacketBytes != 0 {
		t.Fatal(got)
	}
	// err сообщает об отказе закрытия проверяемой подписки.
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе повторного закрытия уже завершённой подписки.
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	if sub.Snapshot().Drops != got.Drops {
		t.Fatal("повторный Close повторно учёл потери")
	}
}

// Проверяет раздельные причины потерь для непригодных заголовков, переполнения, слишком больших AU и
// закрытия с обнулением удерживаемых байтов.
func TestSubscriptionStatsRejectionsAndOwnerClose(t *testing.T) {
	// r содержит тестовый маршрутизатор. source обозначает его текущий допуск публикации. v содержит
	// конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
	r, source, v, a := routerFor(t)
	// limits содержит бюджеты, поочерёдно ограничивающие заголовки и размер пакета.
	limits := routerLimits()
	limits.MaxConfigBytes = 2
	// rejected содержит подписку, в бюджет которой не помещаются заголовки дорожек.
	rejected := subscribeRouter(t, r, limits)
	publishRouter(t, source, routerPacket(t, v, 0, 0, true, 1))
	publishRouter(t, source, routerPacket(t, a, 0, 0, false, 1))
	// got содержит снимок подписки после отказа по размеру заголовков.
	if got := rejected.Snapshot(); got.Drops.ConfigRejected != 2 || got.PacketBytes != 0 || got.ConfigBytes != 0 || got.QueuedPackets != 0 {
		t.Fatal(got)
	}
	limits = routerLimits()
	limits.MaxPacketBytes, limits.MaxConfigBytes = 8, 3
	// small содержит подписку, для которой проверяется слишком большой пакет.
	small := subscribeRouter(t, r, limits)
	publishRouter(t, source, routerPacket(t, v, 1, 1, true, 1))
	publishRouter(t, source, routerPacket(t, a, 1, 1, false, 1))
	publishRouter(t, source, routerPacket(t, v, 2, 2, true, 10))
	// got содержит потери и удерживаемые байты после слишком большого пакета.
	got := small.Snapshot()
	if got.Drops.Overflow != 2 || got.Drops.Oversized != 1 || got.PacketBytes != 0 {
		t.Fatal(got)
	}
	publishRouter(t, source, routerPacket(t, a, 2, 2, false, 1))
	// err сообщает об отказе закрытия проверяемой подписки.
	if err := small.Close(); err != nil {
		t.Fatal(err)
	}
	got = small.Snapshot()
	if got.Drops.Closed != 1 || got.Drops.Oversized != 1 || got.Drops.Overflow != 2 || got.PacketBytes != 0 || got.ConfigBytes != 0 || got.PendingPackets != 0 {
		t.Fatal(got)
	}
}

// Проверяет соблюдение бюджетов согласованного Snapshot при параллельной публикации и передаче пакетов
// через Next.
func TestSubscriptionStatsConcurrentSnapshotsAndTransfers(t *testing.T) {
	// r содержит тестовый маршрутизатор. source обозначает его текущий допуск публикации. v содержит
	// конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
	r, source, v, a := routerFor(t)
	// sub содержит подписку, с которой одновременно работают издатель, получатель и читатель
	// снимков.
	sub := subscribeRouter(t, r, routerLimits())
	// wg позволяет дождаться окончания публикации пакетов и проверок снимков.
	var wg sync.WaitGroup
	wg.Go(func() {
		// Публикует 100 пар A/V одновременно с выдачей пакетов и чтением снимков подписки.

		for range 100 {
			publishRouter(t, source, routerPacket(t, v, 0, 0, true, 1))
			publishRouter(t, source, routerPacket(t, a, 0, 0, false, 1))
		}
	})
	wg.Go(func() {
		// Проверяет ограничения числа пакетов и логического объёма в снимках во время одновременной публикации
		// и выдачи.

		for range 100 {
			// got содержит согласованный снимок подписки во время параллельного обмена
			// пакетами.
			got := sub.Snapshot()
			if got.QueuedPackets+got.PendingPackets > 16 || got.PacketBytes+got.ConfigBytes > 1024 {
				t.Error("несогласованный снимок", got)
			}
		}
	})
	// ctx ограничивает ожидание Next отменой. cancel завершает это ожидание после окончания
	// параллельных проверок.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// done закрывается после выхода получателя из цикла Next.
	done := make(chan struct{})
	go func() {
		// Читает подписку до ошибки Next и закрывает done при выходе, чтобы тест мог дождаться реакции на
		// отмену.

		defer close(done)
		for {
			// err сообщает о завершении ожидания Next, после которого горутина получателя
			// выходит.
			if _, err := sub.Next(ctx); err != nil {
				return
			}
		}
	}()
	wg.Wait()
	cancel()
	<-done
}
