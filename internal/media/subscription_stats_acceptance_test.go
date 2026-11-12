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

// Суммирует все причины потерь очереди для проверки баланса AU в тестовых значениях без насыщения.
func acceptanceQueueLosses(d media.SubscriptionDrops) uint64 {
	return d.Overflow + d.Oversized + d.ConfigRejected + d.AwaitingKeyframe + d.AudioBeforeCut + d.SourceReset + d.ConfigReset + d.Closed
}

// Проверяет отзыв старого поколения при переполнении, сохранение пакета, вызвавшего сброс,
// независимость соседа и точный баланс выданных и потерянных AU.
func TestSubscriptionAcceptanceOverflowInvalidatesTransferredGeneration(t *testing.T) {
	// r содержит тестовый маршрутизатор. src обозначает его текущий допуск публикации. v содержит
	// конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
	r, src, v, a := routerFor(t)
	// limits содержит бюджеты очереди, переполнение которой проверяется на трёх пакетах.
	limits := routerLimits()
	limits.MaxPackets = 3
	// s содержит подписку, старые доставки которой должны отзываться при переполнении.
	s := subscribeRouter(t, r, limits)
	// neighbor содержит независимую подписку для проверки локальности переполнения.
	neighbor := subscribeRouter(t, r, routerLimits())
	publishRouter(t, src, routerPacket(t, a, 0, 0, false, 2))
	publishRouter(t, src, routerPacket(t, a, 48000, 48000, false, 3))
	// got содержит снимок двух аудиоединиц, ожидающих независимого видеокадра.
	if got := s.Snapshot(); got.PendingPackets != 2 || got.QueuedPackets != 0 || got.PacketBytes != 11 {
		t.Fatal(got)
	}
	publishRouter(t, src, routerPacket(t, v, 90000, 90000, true, 4))
	// old содержит видеодоставку, выданную до переполнения.
	old := nextRouter(t, s)
	if old.Packet.Info().PTS != 90000 || !old.IsCurrent() {
		t.Fatal("не выдан независимый видеокадр")
	}
	// got содержит снимок после выдачи видео и отсева раннего аудио.
	if got := s.Snapshot(); got.PendingPackets != 0 || got.QueuedPackets != 1 || got.PacketBytes != 6 || got.Drops.AudioBeforeCut != 1 {
		t.Fatal(got)
	}
	publishRouter(t, src, routerPacket(t, v, 90001, 90001, false, 5))
	publishRouter(t, src, routerPacket(t, v, 90002, 90002, false, 6))
	publishRouter(t, src, routerPacket(t, v, 90003, 90003, true, 7))
	// got содержит снимок после переполнения и приёма нового независимого видеокадра.
	got := s.Snapshot()
	if old.IsCurrent() || got.Generation <= old.Generation || !got.Resync || got.QueuedPackets != 1 || got.PacketBytes != 10 || got.ConfigBytes != 6 || got.Drops.Overflow != 3 || acceptanceQueueLosses(got.Drops) != 4 {
		t.Fatalf("переполнение без смены заголовков: %+v", got)
	}
	publishRouter(t, src, routerPacket(t, a, 48002, 48002, false, 8))
	// video содержит новую видеодоставку после синхронизации. audio содержит согласованную с ней
	// аудиодоставку.
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
	// err сообщает об отказе закрытия проверяемой подписки.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got = s.Snapshot(); acceptanceQueueLosses(got.Drops) != 4 {
		t.Fatal("Next передал владение, Close повторно посчитал AU", got)
	}
}

// Проверяет баланс пяти AU при срезе A/V, выдаче, смене конфигурации и отключении; повторный Close не
// меняет потери.
func TestSubscriptionAcceptanceResetAndCloseConserveOwnedPackets(t *testing.T) {
	// r содержит тестовый маршрутизатор. src обозначает его текущий допуск публикации. v содержит
	// конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
	r, src, v, a := routerFor(t)
	// s содержит подписку для проверки баланса выданных и потерянных единиц доступа.
	s := subscribeRouter(t, r, routerLimits())
	publishRouter(t, src, routerPacket(t, v, 0, 0, false, 1))
	publishRouter(t, src, routerPacket(t, a, 0, 0, false, 2))
	publishRouter(t, src, routerPacket(t, v, 90000, 90000, true, 3))
	publishRouter(t, src, routerPacket(t, a, 48000, 48000, false, 4))
	// issued содержит единственную доставку, переданную до смены конфигурации.
	issued := nextRouter(t, s)
	// vi содержит описание видео для создания следующей ревизии.
	vi := v.Info()
	vi.Revision++
	// v2 содержит видеоконфигурацию, смена которой отзывает прежнюю доставку.
	v2 := configFor(t, vi)
	// err сообщает об отказе установки проверяемой пары конфигураций.
	if err := src.UpdateConfigs(v2, a); err != nil {
		t.Fatal(err)
	}
	if issued.IsCurrent() {
		t.Fatal("смена конфигурации сохранила старую выдачу")
	}
	publishRouter(t, src, routerPacket(t, a, 48001, 48001, false, 5))
	// err сообщает об отказе отключения источника после смены конфигурации.
	if err := src.Disconnect(); err != nil {
		t.Fatal(err)
	}
	// got содержит снимок потерь после отключения источника.
	got := s.Snapshot()
	// want задаёт ожидаемое распределение четырёх потерянных единиц доступа по причинам.
	want := media.SubscriptionDrops{AwaitingKeyframe: 1, AudioBeforeCut: 1, ConfigReset: 1, SourceReset: 1}
	if got.Drops != want || got.PacketBytes != 0 || got.ConfigBytes != 0 || got.QueuedPackets+got.PendingPackets != 0 {
		t.Fatal(got)
	}
	if acceptanceQueueLosses(got.Drops)+1 != 5 {
		t.Fatal("нарушен баланс принятых AU и выданного пакета")
	}
	// err сообщает об отказе закрытия проверяемой подписки.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе повторного закрытия подписки.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got = s.Snapshot(); got.Drops != want {
		t.Fatal("повторно учтены выданные/потерянные AU", got)
	}
}

// Проверяет, что смена конфигурации и переполнение не продлевают исходные 100 мс синхронизации, а
// оставшиеся AU учитываются ровно один раз.
func TestSubscriptionAcceptanceRepeatedResetCannotRenewTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// При управляемом времени проверяет исходный срок подписки после замены конфигурации и переполнения, а
		// также однократный учёт потерянных пакетов.

		// r содержит тестовый маршрутизатор. src обозначает его текущий допуск публикации. v
		// содержит конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
		r, src, v, a := routerFor(t)
		// limits задаёт очередь на один пакет с исходным сроком синхронизации 100 мс.
		limits := routerLimits()
		limits.MaxPackets = 1
		limits.ResyncTimeout = 100 * time.Millisecond
		// s содержит подписку, чей исходный срок не должны продлевать повторные сбросы.
		s := subscribeRouter(t, r, limits)
		time.Sleep(40 * time.Millisecond)
		// vi содержит описание видео для смены ревизии в течение ожидания синхронизации.
		vi := v.Info()
		vi.Revision++
		// v2 содержит обновлённую видеоконфигурацию для повторного сброса подписки.
		v2 := configFor(t, vi)
		// err сообщает об отказе установки проверяемой пары конфигураций.
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
		// got содержит снимок после истечения исходного срока синхронизации.
		got := s.Snapshot()
		if got.Drops.Overflow != 1 || got.Drops.Closed != 1 || acceptanceQueueLosses(got.Drops) != 2 || got.PacketBytes != 0 || got.ConfigBytes != 0 {
			t.Fatal(got)
		}
		// err содержит причину отказа Next после завершения подписки по сроку.
		if _, err := s.Next(context.Background()); !errors.Is(err, media.ErrResyncTimeout) {
			t.Fatal(err)
		}
	})
}

// Проверяет баланс 400 опубликованных AU между выдачей и потерями при параллельных Snapshot,
// публикации и закрытии подписки.
func TestSubscriptionAcceptanceConcurrentTransferConservation(t *testing.T) {
	// r содержит тестовый маршрутизатор. src обозначает его текущий допуск публикации. v содержит
	// конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
	r, src, v, a := routerFor(t)
	// limits задаёт очередь на четыре пакета для параллельной проверки баланса.
	limits := routerLimits()
	limits.MaxPackets = 4
	// s содержит подписку с параллельными публикацией, выдачей и чтением снимков.
	s := subscribeRouter(t, r, limits)
	// delivered атомарно считает пакеты, успешно переданные получателю через Next.
	var delivered atomic.Uint64
	// consumerDone закрывается при выходе получателя и позволяет дождаться его после Close.
	consumerDone := make(chan struct{})
	go func() {
		// Читает пакеты до ошибки подписки, считает успешные выдачи и закрывает consumerDone при выходе.

		defer close(consumerDone)
		for {
			// err сообщает об окончании выдачи пакетов закрытой подпиской.
			_, err := s.Next(context.Background())
			if err != nil {
				return
			}
			delivered.Add(1)
		}
	}()
	// wg позволяет дождаться окончания издателя и проверок снимков.
	var wg sync.WaitGroup
	wg.Go(func() {
		// Публикует 200 пар A/V одновременно с чтением и снятием снимков для проверки баланса выданных и
		// потерянных пакетов.

		for range 200 {
			publishRouter(t, src, routerPacket(t, v, 0, 0, true, 1))
			publishRouter(t, src, routerPacket(t, a, 0, 0, false, 2))
		}
	})
	wg.Go(func() {
		// Проверяет ограничения числа пакетов, байтов и заголовков в снимках во время одновременной публикации
		// и выдачи.

		for range 400 {
			// got содержит снимок очереди во время параллельного обмена пакетами.
			got := s.Snapshot()
			if got.QueuedPackets+got.PendingPackets > 4 || got.PacketBytes > 20 || got.ConfigBytes != 6 {
				t.Error("несогласованный снимок", got)
			}
		}
	})
	wg.Wait()
	// err сообщает об отказе закрытия проверяемой подписки.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-consumerDone:
	case <-time.After(time.Second):
		t.Fatal("Close не присоединил ожидающий Next")
	}
	// got содержит окончательный снимок для сверки баланса опубликованных, выданных и потерянных
	// пакетов.
	got := s.Snapshot()
	if got.QueuedPackets+got.PendingPackets != 0 || got.PacketBytes != 0 || acceptanceQueueLosses(got.Drops)+delivered.Load() != 400 {
		t.Fatalf("AU посчитан дважды или потерян: выдано=%d, %+v", delivered.Load(), got)
	}
}
