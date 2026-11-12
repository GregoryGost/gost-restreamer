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

// Создаёт Router с текущим источником и парой конфигураций; закрытие маршрутизатора регистрирует в
// очистке теста.
func routerFor(t *testing.T) (*media.Router, *media.Source, media.CodecConfig, media.CodecConfig) {
	t.Helper()
	// r содержит создаваемый тестовый маршрутизатор с конечными бюджетами. err сообщает об отказе
	// его создания.
	r, err := media.NewRouter(media.RouterLimits{MaxConsumers: 8, MaxRetainedBytes: 16384, MaxConfigBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает созданный маршрутизатор после теста и сообщает ошибку закрытия.

		// err сообщает об отказе закрытия тестового маршрутизатора.
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	// source содержит начальный допуск публикации, выданный маршрутизатором.
	source, err := r.BeginSource()
	if err != nil {
		t.Fatal(err)
	}
	// v содержит конфигурацию AV1 текущего сегмента. a содержит конфигурацию AAC текущего сегмента.
	v, a := routerConfigs(t, source.Epoch())
	// err сообщает об отказе установки проверяемой пары конфигураций.
	if err := source.UpdateConfigs(v, a); err != nil {
		t.Fatal(err)
	}
	return r, source, v, a
}

// Создаёт тестовую пару AV1/AAC, относящуюся к заданному SourceEpoch.
func routerConfigs(t *testing.T, epoch uint64) (media.CodecConfig, media.CodecConfig) {
	t.Helper()
	// v содержит исходное описание AV1. a содержит исходное описание AAC; обоим назначается
	// переданный SourceEpoch.
	v, a := videoInfo(), audioInfo()
	v.SourceEpoch, a.SourceEpoch = epoch, epoch
	return configFor(t, v), configFor(t, a)
}

// Возвращает ограничения тестовой очереди на 16 пакетов и 1024 байта со сроком синхронизации в одну
// секунду.
func routerLimits() media.QueueLimits {
	return media.QueueLimits{MaxPackets: 16, MaxBytes: 1024, MaxPacketBytes: 128, MaxConfigBytes: 64, ResyncTimeout: time.Second}
}

// Создаёт подписку с заданными ограничениями и регистрирует её закрытие в очистке теста.
func subscribeRouter(t *testing.T, r *media.Router, l media.QueueLimits) *media.Subscription {
	t.Helper()
	// s содержит созданную тестовую подписку. err сообщает об отказе резервирования её очереди.
	s, err := r.Subscribe(l)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает созданную подписку после теста и сообщает ошибку закрытия.

		// err сообщает об отказе закрытия тестовой подписки.
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

// Создаёт пакет с заданными PTS/DTS, ключевым флагом и размером буфера, заполненного нулями;
// длительность равна одному отсчёту.
func routerPacket(t *testing.T, c media.CodecConfig, pts, dts int64, key bool, size int) media.Packet {
	t.Helper()
	// p содержит тестовый пакет с заданными метками и размером нагрузки. err сообщает о некорректных
	// параметрах пакета.
	p, err := media.NewPacket(media.PacketInfo{PTS: pts, DTS: dts, DurationTicks: 1, Keyframe: key}, c, make([]byte, size))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Публикует тестовый пакет и завершает тест при отказе источника.
func publishRouter(t *testing.T, s *media.Source, p media.Packet) {
	t.Helper()
	// err сообщает об отказе публикации тестового пакета.
	if err := s.Publish(p); err != nil {
		t.Fatal(err)
	}
}

// Получает следующую доставку с таймаутом в одну секунду и завершает тест при ошибке.
func nextRouter(t *testing.T, s *media.Subscription) media.Delivery {
	t.Helper()
	// ctx ограничивает получение тестового пакета одной секундой. cancel освобождает ресурсы этого
	// ожидания.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// d содержит следующую доставку подписки. err сообщает об отказе или истечении срока её
	// получения.
	d, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Проверяет согласованный старт A/V, сохранение отрицательных PTS/DTS, допустимость равных DTS и отказ
// при их уменьшении.
func TestRouterAlignedAudioAndSignedPacketTransfer(t *testing.T) {
	// r содержит тестовый маршрутизатор. source обозначает его текущий допуск публикации. v содержит
	// конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
	r, source, v, a := routerFor(t)
	// sub содержит подписку для проверки согласования аудио и сохранения знаковых меток.
	sub := subscribeRouter(t, r, routerLimits())
	publishRouter(t, source, routerPacket(t, a, -1024, -1024, false, 1))
	publishRouter(t, source, routerPacket(t, a, 0, 0, false, 1))
	publishRouter(t, source, routerPacket(t, v, 0, -3000, true, 1))
	// video содержит начальный независимый видеокадр. audio содержит первую согласованную с ним
	// аудиоединицу.
	video, audio := nextRouter(t, sub), nextRouter(t, sub)
	if video.Packet.Config().Info().Track != media.TrackVideo || audio.Packet.Info().PTS != 0 || audio.Packet.Config().Info().Track != media.TrackAudio {
		t.Fatal("неверная согласованная точка начала")
	}
	if !video.IsCurrent() || !sub.IsCurrent(audio) {
		t.Fatal("текущая доставка признана устаревшей")
	}
	publishRouter(t, source, routerPacket(t, v, -5000, -3000, false, 1))
	// got содержит доставку видеопакета с отрицательными метками и повторённым DTS.
	got := nextRouter(t, sub)
	if got.Packet.Info().PTS != -5000 || got.Packet.Info().DTS != -3000 {
		t.Fatal("маршрутизатор изменил знаковые PTS/DTS либо запретил равные DTS")
	}
	// err содержит результат публикации пакета с уменьшившимся DTS.
	if err := source.Publish(routerPacket(t, v, 0, -3001, false, 1)); !errors.Is(err, media.ErrDTSRegression) {
		t.Fatalf("уменьшение DTS принято: %v", err)
	}
	if !video.IsCurrent() {
		t.Fatal("выданный пакет ошибочно отозван после удаления из очереди")
	}
}

// Проверяет локальность переполнения и слишком большого пакета: сосед получает данные, а пострадавший
// потребитель восстанавливается с нового ключевого A/V.
func TestRouterOverflowAndOversizeAffectOnlyOneConsumer(t *testing.T) {
	// r содержит тестовый маршрутизатор. source обозначает его текущий допуск публикации. v содержит
	// конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
	r, source, v, a := routerFor(t)
	// small задаёт бюджеты медленной очереди на два пакета и восемь байтов на пакет.
	small := routerLimits()
	small.MaxPackets, small.MaxPacketBytes, small.MaxConfigBytes = 2, 8, 3
	// slow содержит подписку, чья очередь должна переполняться независимо от соседа.
	slow := subscribeRouter(t, r, small)
	// fast содержит подписку, которая непрерывно получает пакеты при переполнении соседа.
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
	// d1 содержит независимый видеокадр после восстановления медленной очереди. d2 содержит
	// согласованное с ним аудио.
	d1, d2 := nextRouter(t, slow), nextRouter(t, slow)
	if !d1.Packet.Info().Keyframe || d1.Packet.Info().PTS != 90000 || d2.Packet.Config().Info().Track != media.TrackAudio || d2.Packet.Info().PTS != 48000 {
		t.Fatal("после переполнения выпущен старый или зависимый пакет")
	}
}

// Проверяет атомарность пары конфигураций и отзыв прежних доставок, ревизий и Source после смены аудио
// или публикации.
func TestRouterConfigPairAndPublicationFences(t *testing.T) {
	// r содержит тестовый маршрутизатор. source обозначает его текущий допуск публикации. v содержит
	// конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
	r, source, v, a := routerFor(t)
	// sub содержит подписку для проверки отзыва доставок при смене конфигурации или публикации.
	sub := subscribeRouter(t, r, routerLimits())
	publishRouter(t, source, routerPacket(t, v, 0, 0, true, 1))
	publishRouter(t, source, routerPacket(t, a, 0, 0, false, 1))
	// issued содержит доставку, выданную до смены аудиоконфигурации.
	issued := nextRouter(t, sub)
	// ai содержит описание аудио для увеличения его ревизии.
	ai := a.Info()
	ai.Revision++
	// a2 содержит аудиоконфигурацию следующей ревизии.
	a2 := configFor(t, ai)
	// err сообщает об отказе установки проверяемой пары конфигураций.
	if err := source.UpdateConfigs(v, a2); err != nil {
		t.Fatal(err)
	}
	if issued.IsCurrent() {
		t.Fatal("смена аудиоревизии не отозвала идентичность A/V")
	}
	// err содержит результат публикации пакета прежней аудиоревизии.
	if err := source.Publish(routerPacket(t, a, 0, 0, false, 1)); !errors.Is(err, media.ErrStaleSource) {
		t.Fatal("принята прежняя аудиоревизия", err)
	}
	// vi содержит описание видео для увеличения ревизии.
	vi := v.Info()
	vi.Revision++
	// v2 содержит новую видеоконфигурацию для проверки атомарного отказа устаревшей пары.
	v2 := configFor(t, vi)
	// err содержит результат установки пары с новой видеоревизией и устаревшей аудиоревизией.
	if err := source.UpdateConfigs(v2, a); !errors.Is(err, media.ErrStaleSource) {
		t.Fatal("частично устаревшая пара принята", err)
	}
	// err содержит результат публикации видеоревизии, установка которой была отклонена.
	if err := source.Publish(routerPacket(t, v2, 0, 0, true, 1)); !errors.Is(err, media.ErrStaleSource) {
		t.Fatal("неуспешная замена частично изменила пару", err)
	}
	publishRouter(t, source, routerPacket(t, v, 1, 1, true, 1))
	publishRouter(t, source, routerPacket(t, a2, 1, 1, false, 1))
	// fresh содержит доставку после успешного восстановления A/V с новой аудиоревизией.
	fresh := nextRouter(t, sub)
	if fresh.Packet.Info().PTS != 1 || fresh.AudioConfig.Info().Revision != ai.Revision {
		t.Fatal("из очереди вышел старый снимок")
	}
	// replacement содержит допуск нового сегмента публикации. err сообщает об отказе замены
	// источника.
	replacement, err := r.ReplaceSource()
	if err != nil || replacement.Epoch() <= source.Epoch() {
		t.Fatal("epoch переиспользована", err)
	}
	// err содержит результат отключения уже заменённого допуска.
	if err := source.Disconnect(); !errors.Is(err, media.ErrStaleSource) {
		t.Fatal("старый Disconnect принят", err)
	}
	// err содержит результат установки конфигураций через уже заменённый допуск.
	if err := source.UpdateConfigs(v, a2); !errors.Is(err, media.ErrStaleSource) {
		t.Fatal("старые заголовки приняты", err)
	}
	// err содержит результат публикации пакета через уже заменённый допуск.
	if err := source.Publish(routerPacket(t, v, 2, 2, true, 1)); !errors.Is(err, media.ErrStaleSource) {
		t.Fatal("старый пакет принят", err)
	}
	// rv содержит видеоконфигурацию нового сегмента. ra содержит аудиоконфигурацию нового сегмента.
	rv, ra := routerConfigs(t, replacement.Epoch())
	// err сообщает об отказе установки проверяемой пары конфигураций.
	if err := replacement.UpdateConfigs(rv, ra); err != nil {
		t.Fatal(err)
	}
	publishRouter(t, replacement, routerPacket(t, rv, 0, 0, true, 1))
	publishRouter(t, replacement, routerPacket(t, ra, 0, 0, false, 1))
	if nextRouter(t, sub).Packet.Config().Info().SourceEpoch != replacement.Epoch() || fresh.IsCurrent() {
		t.Fatal("старый источник нарушил новую публикацию")
	}
}

// Проверяет неизменность значения одной ревизии и точное сравнение DTS разных шкал на крайних
// значениях int64.
func TestRouterRevisionIdentityAndExactDTSAcrossTimeBases(t *testing.T) {
	// source обозначает текущий тестовый допуск. v содержит его конфигурацию AV1. a содержит его
	// конфигурацию AAC.
	_, source, v, a := routerFor(t)
	// vi содержит описание видео для последовательной смены шкал и ревизий.
	vi := v.Info()
	vi.TimeBase = media.TimeBase{Numerator: math.MaxUint32, Denominator: math.MaxUint32}
	// v1 содержит конфигурацию с большой дробью времени для проверки неизменности значения ревизии.
	v1 := configFor(t, vi)
	// err содержит результат попытки изменить шкалу без увеличения ревизии.
	if err := source.UpdateConfigs(v1, a); !errors.Is(err, media.ErrStaleSource) {
		t.Fatal("ревизия получила другое значение", err)
	}
	vi.Revision++
	v1 = configFor(t, vi)
	// err сообщает об отказе установки проверяемой пары конфигураций.
	if err := source.UpdateConfigs(v1, a); err != nil {
		t.Fatal(err)
	}
	publishRouter(t, source, routerPacket(t, v1, math.MinInt64, math.MinInt64, true, 1))
	vi.Revision++
	vi.TimeBase = media.TimeBase{Numerator: 1, Denominator: 1}
	// v2 содержит следующую видеоревизию со шкалой в одну секунду на отсчёт.
	v2 := configFor(t, vi)
	// err сообщает об отказе установки проверяемой пары конфигураций.
	if err := source.UpdateConfigs(v2, a); err != nil {
		t.Fatal(err)
	}
	publishRouter(t, source, routerPacket(t, v2, math.MinInt64, math.MinInt64, true, 1))
	publishRouter(t, source, routerPacket(t, v2, math.MaxInt64, math.MaxInt64, true, 1))
	vi.Revision++
	vi.TimeBase = media.TimeBase{Numerator: 1, Denominator: math.MaxUint32}
	// v3 содержит видеоревизию с очень малым отсчётом времени для проверки точного сравнения DTS.
	v3 := configFor(t, vi)
	// err сообщает об отказе установки проверяемой пары конфигураций.
	if err := source.UpdateConfigs(v3, a); err != nil {
		t.Fatal(err)
	}
	// err содержит результат публикации метки, которая в новой шкале меньше предыдущей.
	if err := source.Publish(routerPacket(t, v3, math.MaxInt64, math.MaxInt64, true, 1)); !errors.Is(err, media.ErrDTSRegression) {
		t.Fatalf("сравнение разных шкал переполнилось: %v", err)
	}
}

// Проверяет, что точный бюджет включает обе конфигурации и ожидающее аудио: 14 байтов достаточно для
// пары, 13 — приводит к истечению срока синхронизации.
func TestRouterRetainedBudgetIncludesHeadersAndPendingAudio(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Сопоставляет точный бюджет пары A/V с недостаточным на один байт бюджетом, учитывая заголовки и
		// ожидающее аудио.

		// r содержит тестовый маршрутизатор. source обозначает его текущий допуск публикации. v
		// содержит конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
		r, source, v, a := routerFor(t)
		// limits задаёт точный четырнадцатибайтовый бюджет для двух пакетов и заголовков.
		limits := routerLimits()
		limits.MaxBytes, limits.MaxPacketBytes, limits.MaxConfigBytes = 14, 4, 3
		// sub содержит подписку с точно достаточным бюджетом согласованной пары.
		sub := subscribeRouter(t, r, limits)
		publishRouter(t, source, routerPacket(t, a, 0, 0, false, 1))
		publishRouter(t, source, routerPacket(t, v, 0, 0, true, 1))
		if nextRouter(t, sub).Packet.Config().Info().Track != media.TrackVideo {
			t.Fatal("точный бюджет не принят")
		}
		nextRouter(t, sub)
		limits.MaxBytes = 13
		// tooSmall содержит подписку с бюджетом на один байт меньше необходимого для пары.
		tooSmall := subscribeRouter(t, r, limits)
		publishRouter(t, source, routerPacket(t, a, 48000, 48000, false, 1))
		publishRouter(t, source, routerPacket(t, v, 90000, 90000, true, 1))
		// err содержит результат ожидания A/V в очереди, в которую не помещается согласованная
		// пара.
		if _, err := tooSmall.Next(context.Background()); !errors.Is(err, media.ErrResyncTimeout) {
			t.Fatal("пара превысила бюджет вместе с заголовками", err)
		}
	})
}

// Проверяет неизменность исходного срока синхронизации при повторе и смене конфигураций и закрытие
// таймером без вызова Next.
func TestRouterResyncDeadlineSurvivesConfigUpdatesAndDuplicates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// При управляемом времени проверяет завершение подписки по исходному сроку после замены конфигурации и
		// повторных видеокадров без аудио.

		// r содержит тестовый маршрутизатор. source обозначает его текущий допуск публикации. v
		// содержит конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
		r, source, v, a := routerFor(t)
		// sub содержит подписку для проверки исходного срока синхронизации без аудио.
		sub := subscribeRouter(t, r, routerLimits())
		time.Sleep(400 * time.Millisecond)
		// err сообщает об отказе повторной установки неизменённой пары конфигураций.
		if err := source.UpdateConfigs(v, a); err != nil {
			t.Fatal(err)
		}
		// vi содержит описание видео, ревизия которого меняется во время синхронизации.
		vi := v.Info()
		vi.Revision++
		// v2 содержит видеоконфигурацию с новой ревизией.
		v2 := configFor(t, vi)
		// err сообщает об отказе установки проверяемой пары конфигураций.
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
		// err содержит результат Next после завершения подписки исходным таймером.
		if _, err := sub.Next(context.Background()); !errors.Is(err, media.ErrResyncTimeout) {
			t.Fatal(err)
		}
	})
}

// Проверяет точный отсев аудио до точки видео при предельных знаковых метках и больших множителях
// шкалы.
func TestRouterRationalAudioAlignmentAtExtremeTimestamp(t *testing.T) {
	// r содержит тестовый маршрутизатор. source обозначает его текущий допуск публикации. v содержит
	// конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
	r, source, v, a := routerFor(t)
	// vi содержит описание видео для экстремальной шкалы. ai содержит описание аудио, которому
	// назначается та же шкала.
	vi, ai := v.Info(), a.Info()
	vi.Revision++
	ai.Revision++
	vi.TimeBase = media.TimeBase{Numerator: math.MaxUint32, Denominator: 1}
	ai.TimeBase = vi.TimeBase
	v, a = configFor(t, vi), configFor(t, ai)
	// err сообщает об отказе установки проверяемой пары конфигураций.
	if err := source.UpdateConfigs(v, a); err != nil {
		t.Fatal(err)
	}
	// sub содержит подписку для проверки точного среза аудио на предельных метках.
	sub := subscribeRouter(t, r, routerLimits())
	publishRouter(t, source, routerPacket(t, a, math.MinInt64, math.MinInt64, false, 1))
	publishRouter(t, source, routerPacket(t, a, math.MaxInt64, math.MaxInt64, false, 1))
	publishRouter(t, source, routerPacket(t, v, math.MaxInt64, math.MaxInt64, true, 1))
	nextRouter(t, sub)
	if nextRouter(t, sub).Packet.Info().PTS != math.MaxInt64 {
		t.Fatal("переполнение пропустило несогласованное аудио")
	}
}

// Проверяет лимит подписчиков, возврат резерва после Close и отказ операций источника и подписки после
// закрытия Router.
func TestRouterConsumerReservationsAndClose(t *testing.T) {
	// limits ограничивает маршрутизатор одним потребителем и четырнадцатью байтами общего резерва.
	limits := media.RouterLimits{MaxConsumers: 1, MaxRetainedBytes: 14, MaxConfigBytes: 3}
	// r содержит маршрутизатор для проверки резервов. err сообщает об отказе его создания.
	r, err := media.NewRouter(limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает маршрутизатор после проверки резервирования потребителей и сообщает ошибку закрытия.

		// err сообщает об отказе закрытия тестового маршрутизатора.
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	// q задаёт восьмибайтовый резерв отдельной очереди.
	q := media.QueueLimits{MaxPackets: 2, MaxBytes: 8, MaxPacketBytes: 4, MaxConfigBytes: 3, ResyncTimeout: time.Second}
	// s содержит первую подписку, занимающую единственный доступный резерв.
	s := subscribeRouter(t, r, q)
	// err содержит результат попытки создать второго подписчика сверх лимита.
	if _, err := r.Subscribe(q); !errors.Is(err, media.ErrRouterBudget) {
		t.Fatal("превышено число подписчиков", err)
	}
	// err сообщает об отказе закрытия первой подписки и освобождения её резерва.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// next содержит подписку, созданную после освобождения первого резерва.
	next := subscribeRouter(t, r, q)
	// err сообщает об отказе закрытия тестового маршрутизатора.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// err содержит результат Next после закрытия маршрутизатора.
	if _, err := next.Next(context.Background()); !errors.Is(err, media.ErrRouterClosed) {
		t.Fatal(err)
	}
	// err содержит результат создания источника после закрытия маршрутизатора.
	if _, err := r.BeginSource(); !errors.Is(err, media.ErrRouterClosed) {
		t.Fatal(err)
	}
	// err содержит результат создания подписки после закрытия маршрутизатора.
	if _, err := r.Subscribe(q); !errors.Is(err, media.ErrRouterClosed) {
		t.Fatal(err)
	}
}

// Проверяет совместную публикацию и замену источника с допустимыми ошибками устаревания и завершение
// ожидающего Next по отмене.
func TestRouterConcurrentPublishReplaceAndCancellation(t *testing.T) {
	// r содержит тестовый маршрутизатор. source обозначает его текущий допуск публикации. v содержит
	// конфигурацию AV1 этого допуска. a содержит конфигурацию AAC этого допуска.
	r, source, v, a := routerFor(t)
	// sub содержит подписку, читаемую одновременно с публикацией и заменами источника.
	sub := subscribeRouter(t, r, routerLimits())
	// ctx позволяет отменить ожидающий Next. cancel вызывается после окончания издателя и замен
	// источника.
	ctx, cancel := context.WithCancel(context.Background())
	// done закрывается при выходе получателя для проверки завершения после отмены.
	done := make(chan struct{})
	go func() {
		// Читает подписку до ошибки Next и закрывает done при выходе; завершения горутины ожидает вызывающий
		// тест.

		defer close(done)
		for {
			// err сообщает о завершении Next, после которого получатель выходит.
			_, err := sub.Next(ctx)
			if err != nil {
				return
			}
		}
	}()
	// wg позволяет дождаться окончания параллельных публикаций и замен источника.
	var wg sync.WaitGroup
	wg.Go(func() {
		// Публикует пакеты прежнего источника одновременно с его заменами, допуская только ErrStaleSource
		// после отзыва.

		for range 100 {
			// err содержит результат публикации через исходный допуск, который мог быть уже
			// заменён.
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
		// Последовательно заменяет источник и публикует согласованную пару A/V каждой новой эпохи.

		for range 20 {
			// replacement содержит очередной новый допуск источника. err сообщает об отказе
			// его замены.
			replacement, err := r.ReplaceSource()
			if err != nil {
				t.Error(err)
				return
			}
			// rv содержит видеоконфигурацию очередного нового сегмента. ra содержит его
			// аудиоконфигурацию.
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

// Проверяет отказ синхронизации при слишком больших заголовках и освобождение общего резерва подписки
// после таймаута.
func TestRouterHeadersBudgetAndReservationReleaseOnTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет отказ по бюджету заголовков и освобождение резерва подписки после истечения срока
		// синхронизации.

		// r содержит маршрутизатор с резервом одной шестнадцатибайтовой очереди сверх
		// заголовков. err сообщает об отказе его создания.
		r, err := media.NewRouter(media.RouterLimits{MaxConsumers: 2, MaxRetainedBytes: 22, MaxConfigBytes: 3})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// Закрывает маршрутизатор после сценария истечения срока и сообщает ошибку закрытия.

			// err сообщает об отказе закрытия тестового маршрутизатора.
			if err := r.Close(); err != nil {
				t.Error(err)
			}
		})
		// source содержит текущий допуск публикации тестового маршрутизатора.
		source, err := r.BeginSource()
		if err != nil {
			t.Fatal(err)
		}
		// v содержит конфигурацию AV1 текущего сегмента. a содержит конфигурацию AAC текущего
		// сегмента.
		v, a := routerConfigs(t, source.Epoch())
		// err сообщает об отказе установки проверяемой пары конфигураций.
		if err := source.UpdateConfigs(v, a); err != nil {
			t.Fatal(err)
		}
		// q задаёт очередь, в которой двухбайтовый предел заголовков меньше фактического размера
		// конфигураций.
		q := media.QueueLimits{MaxPackets: 4, MaxBytes: 16, MaxPacketBytes: 4, MaxConfigBytes: 2, ResyncTimeout: time.Second}
		// s содержит подписку, которая должна завершиться из-за невозможности синхронизации.
		s := subscribeRouter(t, r, q)
		// err содержит результат попытки зарезервировать ещё одну очередь сверх общего бюджета.
		if _, err := r.Subscribe(q); !errors.Is(err, media.ErrRouterBudget) {
			t.Fatal("сумма резервов превысила общий бюджет", err)
		}
		publishRouter(t, source, routerPacket(t, v, 0, 0, true, 1))
		publishRouter(t, source, routerPacket(t, a, 0, 0, false, 1))
		// err содержит результат Next после истечения срока синхронизации из-за непомещающихся
		// заголовков.
		if _, err := s.Next(context.Background()); !errors.Is(err, media.ErrResyncTimeout) {
			t.Fatal("заголовки превысили бюджет подписчика", err)
		}
		// replacement содержит подписку, созданную за счёт резерва, освобождённого после
		// таймаута.
		replacement := subscribeRouter(t, r, q)
		if replacement.Err() != nil {
			t.Fatal("истечение срока не освободило резерв")
		}
	})
}
