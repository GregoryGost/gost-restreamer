package media_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// Проверяет срез аудио по PTS ключевого видео в разных шкалах с отрицательными метками и отмену
// последующего ожидания Next.
func TestAcceptanceRouterAudioCutUsesPresentationTimeAcrossClocks(t *testing.T) {
	// r содержит тестовый маршрутизатор. source обозначает его текущий допуск публикации. v содержит
	// конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
	r, source, v, a := routerFor(t)
	// sub содержит подписку для проверки среза аудио по времени показа видео.
	sub := subscribeRouter(t, r, routerLimits())
	// Видео показывается в -1/2 с, а декодируется в -3/5 с.
	// Первый аудиопакет начинается на один отсчёт раньше точки показа.
	// before содержит аудио, начинающееся на один отсчёт раньше точки показа независимого кадра.
	before := routerPacket(t, a, -24001, -24001, false, 1)
	// aligned содержит аудио, начинающееся точно в точке показа независимого кадра.
	aligned := routerPacket(t, a, -24000, -24000, false, 1)
	// video содержит независимый кадр с различными отрицательными PTS и DTS.
	video := routerPacket(t, v, -45000, -54000, true, 1)
	// p обозначает очередной пакет сценария среза аудио.
	for _, p := range []media.Packet{before, aligned, video} {
		publishRouter(t, source, p)
	}
	// got содержит первую доставку, которая должна совпасть с независимым видеокадром.
	if got := nextRouter(t, sub); got.Packet.Info() != video.Info() || !got.IsCurrent() {
		t.Fatalf("неверный видеосрез: %+v", got.Packet.Info())
	}
	// got содержит следующую доставку, которая должна совпасть с согласованным аудио.
	if got := nextRouter(t, sub); got.Packet.Info() != aligned.Info() {
		t.Fatalf("на срез попало аудио до точки показа: %+v", got.Packet.Info())
	}
	// ctx передаёт отмену ожидающему Next. cancel заранее отменяет этот контекст.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// err содержит результат Next с уже отменённым контекстом.
	if _, err := sub.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("отмена ожидания проигнорирована:", err)
	}
}

// Проверяет завершение синхронизации в исходный срок несмотря на частые ревизии заголовков и поток
// ключевых видеопакетов без аудио.
func TestAcceptanceRouterHeadersAndPacketFloodCannotRenewDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// При управляемом времени проверяет, что повторные заголовки и видеопакеты без согласованного аудио не
		// продлевают исходный срок синхронизации.

		// r содержит тестовый маршрутизатор. source обозначает его текущий допуск публикации. v
		// содержит конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
		r, source, v, a := routerFor(t)
		// q содержит ограничения очереди на два пакета для проверки повторных сбросов.
		q := routerLimits()
		q.MaxPackets = 2
		// sub содержит подписку, срок которой проверяется при непрерывном потоке заголовков и
		// видео.
		sub := subscribeRouter(t, r, q)
		// i задаёт номер очередного обновления в серии из девяти интервалов по 100 мс.
		for i := range 9 {
			<-time.After(100 * time.Millisecond)
			// vi содержит описание видео для назначения очередной большей ревизии.
			vi := v.Info()
			vi.Revision += uint64(i + 1)
			v = configFor(t, vi)
			// err сообщает об отказе установки проверяемой пары конфигураций.
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
		// err содержит результат Next после истечения первоначального срока синхронизации.
		if _, err := sub.Next(context.Background()); !errors.Is(err, media.ErrResyncTimeout) {
			t.Fatal("причина timeout потеряна:", err)
		}
	})
}

// Проверяет независимую доставку ста пакетов исправному потребителю при переполнении соседа и
// восстановление медленной очереди с согласованного A/V.
func TestAcceptanceRouterSlowQueueCannotHoldHealthyConsumer(t *testing.T) {
	// r содержит тестовый маршрутизатор. source обозначает его текущий допуск публикации. v содержит
	// конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
	r, source, v, a := routerFor(t)
	// q содержит ограничения медленной очереди на два пакета.
	q := routerLimits()
	q.MaxPackets = 2
	// slow содержит переполняемую подписку без непрерывного чтения.
	slow := subscribeRouter(t, r, q)
	// fast содержит независимо читаемую подписку с большим бюджетом.
	fast := subscribeRouter(t, r, routerLimits())
	// p обозначает пакет исходной согласованной пары для запуска обеих очередей.
	for _, p := range []media.Packet{routerPacket(t, v, 0, 0, true, 1), routerPacket(t, a, 0, 0, false, 1)} {
		publishRouter(t, source, p)
	}
	nextRouter(t, fast)
	nextRouter(t, fast)
	// i задаёт последовательную временную метку очередного из ста видеопакетов.
	for i := int64(1); i <= 100; i++ {
		// p содержит очередной зависимый видеопакет, который должен получить быстрый подписчик.
		p := routerPacket(t, v, i, i, false, 1)
		publishRouter(t, source, p)
		// got содержит доставку быстрого подписчика для проверки точного сохранения пакета.
		if got := nextRouter(t, fast); got.Packet.Info() != p.Info() || !got.IsCurrent() {
			t.Fatalf("переполнение соседа изменило здоровую доставку %d", i)
		}
	}
	// Аудио 1 с совпадает с видео 1 с, несмотря на разные часы.
	// key содержит новый независимый видеокадр для восстановления медленной очереди.
	key := routerPacket(t, v, 90000, 90000, true, 1)
	publishRouter(t, source, key)
	publishRouter(t, source, routerPacket(t, a, 48000, 48000, false, 1))
	// got содержит первую доставку восстановленной очереди, которая должна совпасть с key.
	if got := nextRouter(t, slow); got.Packet.Info() != key.Info() {
		t.Fatal("медленная очередь выпустила зависимый пакет после переполнения")
	}
	// got содержит согласованную аудиодоставку восстановленной очереди.
	if got := nextRouter(t, slow); got.Packet.Config().Info().Track != media.TrackAudio || got.Packet.Info().PTS != 48000 {
		t.Fatal("восстановление не сохранило согласованное аудио")
	}
}
