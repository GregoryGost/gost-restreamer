package group

import (
	"context"
	"errors"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"math"
	"testing"
	"time"
)

// Возвращает av1C ограниченного тестового потока для проверки независимого старта AV1.
func av1Headers() []byte {
	return []byte{0x81, 8, 0x0c, 0, 0x0a, 11, 0, 0, 0, 66, 167, 191, 228, 96, 13, 0, 64}
}

// Создаёт показанный ключевой TU с заголовком последовательности либо зависимый TU по параметру key.
func av1TU(key bool) []byte {
	if !key {
		return []byte{0x1a, 1, 0x30}
	}
	return append(append([]byte(nil), av1Headers()[4:]...), 0x1a, 1, 0x10)
}

// Создаёт переключатель AV1/AAC с проверенным форматом и конечными бюджетами и регистрирует закрытие
// после теста.
func switcherFixture(t *testing.T) (*Switcher, media.CodecConfig, media.CodecConfig, time.Time) {
	t.Helper()
	// vf содержит формат, подтверждённый инспекцией контрольного AV1 TU; err сохраняет результаты
	// инспекции и создания Switcher.
	vf, _, err := codec.InspectAV1TU(av1Headers(), av1TU(true), core.FrameRate{Numerator: 30, Denominator: 1})
	if err != nil {
		t.Fatal(err)
	}
	// af содержит аудиоформат контрольного ASC AAC.
	af, _ := codec.AudioFormatASC([]byte{0x11, 0x90})
	// base задаёт общую исходную шкалу 1/48000 для обеих контрольных дорожек.
	base := media.TimeBase{Numerator: 1, Denominator: 48000}
	// v содержит AV1-конфигурацию контрольной эпохи 50 и ревизии 2.
	v, _ := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 50, Revision: 2, TimeBase: base, Video: vf}, av1Headers())
	// a содержит AAC-конфигурацию той же эпохи и ревизии 3.
	a, _ := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 50, Revision: 3, TimeBase: base, Audio: af}, []byte{0x11, 0x90})
	// c задаёт контрольный copy-профиль, сроки восстановления и конечные бюджеты переключателя.
	c := SwitcherConfig{Profile: core.GroupProfile{Mode: core.ModeCopy, Format: core.MediaFormat{Video: vf, Audio: af}, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 1000, MaxBitrateBPS: 1000, BufferBits: 1000, GOPFrames: 30, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 128000}}, Watchdog: media.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: 100 * time.Millisecond, KeyframeTimeout: 200 * time.Millisecond}, Queue: media.QueueLimits{MaxPackets: 64, MaxBytes: 65536, MaxPacketBytes: 4096, MaxConfigBytes: 512, ResyncTimeout: time.Second}, Router: media.RouterLimits{MaxConsumers: 8, MaxRetainedBytes: 1 << 20, MaxConfigBytes: 512}, MaxAVSkew: time.Second, MaxTransitionGap: time.Second}
	// s содержит созданный переключатель AV1/AAC для конкретного теста.
	s, err := NewSwitcher(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает тестовый Switcher после сценария, игнорируя ошибку.
		_ = s.Close()
	})
	return s, v, a, time.Now()
}

// Создаёт тестовый пакет выбранной дорожки с заданными метками, длительностью и независимостью и
// передаёт его источнику переключателя.
func pushPacket(t *testing.T, s *Switcher, f Feed, attempt uint64, c, v, a media.CodecConfig, dts, pts, duration int64, key bool, now time.Time) error {
	t.Helper()
	// payload содержит контрольный AAC AU либо выбранный AV1 TU для видеопакета.
	payload := []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c}
	if c.Info().Track == media.TrackVideo {
		payload = av1TU(key)
	}
	// p содержит пакет с заданными метками и длительностью; err сообщает об отказе его создания.
	p, err := media.NewPacket(media.PacketInfo{DTS: dts, PTS: pts, DurationTicks: duration, Keyframe: key}, c, payload)
	if err != nil {
		t.Fatal(err)
	}
	return s.Push(f, attempt, p, v, a, now)
}

// Допускает успех либо ожидаемую недоступность до готовности обеих дорожек, завершая тест для других
// ошибок.
func allowUnavailable(t *testing.T, err error) {
	t.Helper()
	if err != nil && !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

// Получает следующий пакет подписки в пределах одной секунды или завершает тест ошибкой.
func next(t *testing.T, sub *media.Subscription) media.Packet {
	t.Helper()
	// ctx ограничивает получение следующего пакета одной секундой; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// d содержит следующую выдачу выходной подписки; err сообщает об отказе её получения.
	d, err := sub.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return d.Packet
}

// Проверяет общий непрерывный горизонт и A/V-смещение при передаче заглушки, восстановлении LIVE и
// ручной паузе, а также недоступность при утрате заглушки на паузе.
func TestSwitcherFallbackPauseRecoveryAndContinuousHorizon(t *testing.T) {
	// s содержит переключатель; v и a задают дорожки; now задаёт начало сценария заглушки,
	// восстановления LIVE и ручной паузы.
	s, v, a, now := switcherFixture(t)
	// sub содержит выходную подписку для проверки непрерывных меток; err сообщает об отказе
	// подписки.
	sub, err := s.Router().Subscribe(s.config.Queue)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает выходную подписку после проверки паузы и непрерывной шкалы, игнорируя ошибку.
		_ = sub.Close()
	}()
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, -48000, -48000, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, -47520, -47520, 1024, false, now))
	// p содержит очередной выходной видеопакет; q содержит согласованный аудиопакет для проверки
	// общего сдвига.
	p, q := next(t, sub), next(t, sub)
	if p.Info().DTS != 0 || q.Info().DTS != 480 || s.Status().Source != Fallback {
		t.Fatalf("не сохранён A/V offset: %+v %+v %+v", p.Info(), q.Info(), s.Status())
	}
	// Одинаковые значения SourceEpoch разных источников не задают общую идентичность.
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 100000, 100000, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 100480, 100480, 1024, false, now))
	if s.Status().Source != Fallback {
		t.Fatal("LIVE вернулся до стабильного окна")
	}
	// later задаёт время после окна стабильного восстановления LIVE и последующей ручной паузы.
	later := now.Add(120 * time.Millisecond)
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 101600, 101600, 1600, true, later))
	allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 102080, 102080, 1024, false, later))
	p, q = next(t, sub), next(t, sub)
	if p.Info().DTS != 1600 || q.Info().DTS != 2080 || s.Status().Source != Live || p.Config().Info().SourceEpoch != 2 {
		t.Fatalf("неверный cut: %+v %+v %+v", p.Info(), q.Info(), s.Status())
	}
	allowUnavailable(t, s.SetPaused(true, later))
	if s.Status().Source == Live && s.Status().Available {
		t.Fatal("пауза оставила LIVE")
	}
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, -46400, -46400, 1600, true, later))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, -45920, -45920, 1024, false, later))
	p, q = next(t, sub), next(t, sub)
	if p.Info().DTS != 3200 || q.Info().DTS != 3680 || s.Status().Source != Fallback || !s.Status().Paused {
		t.Fatalf("пауза сбросила шкалу: %+v %+v %+v", p.Info(), q.Info(), s.Status())
	}
	// err содержит ожидаемую недоступность после потери заглушки при включённой паузе.
	if err := s.Fail(Fallback, 1, errors.New("пропал файл"), later); !errors.Is(err, ErrUnavailable) || s.Status().Available {
		t.Fatalf("ложный fallback: %v %+v", err, s.Status())
	}
}

// Проверяет отказ дубликата и регрессии DTS, восстановление новой попыткой и игнорирование пакетов и
// отказов прежнего номера.
func TestSwitcherRejectsStaleRegressionAndRevokesOldPending(t *testing.T) {
	// s содержит переключатель; v и a задают дорожки; now задаёт время проверки дубликатов,
	// регрессии и старых попыток.
	s, v, a, now := switcherFixture(t)
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 1024, false, now))
	// err содержит ожидаемый отказ равного DTS без отзыва всей попытки.
	if err := pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 1600, true, now); !errors.Is(err, ErrFeed) {
		t.Fatal(err)
	}
	// err содержит ожидаемый ErrDTSRegression при убывающем DTS.
	if err := pushPacket(t, s, Fallback, 1, v, v, a, -1, -1, 1600, true, now); !errors.Is(err, media.ErrDTSRegression) {
		t.Fatal(err)
	}
	if s.Status().Available {
		t.Fatal("регрессия не отозвала попытку")
	}
	allowUnavailable(t, pushPacket(t, s, Fallback, 2, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 2, a, v, a, 0, 0, 1024, false, now))
	if !s.Status().Available || s.Status().Epoch != 2 {
		t.Fatal(s.Status())
	}
	// err содержит ожидаемый ErrFeed для пакета прежней попытки после восстановления новой.
	if err := pushPacket(t, s, Fallback, 1, v, v, a, 10, 10, 1600, true, now); !errors.Is(err, ErrFeed) {
		t.Fatal(err)
	}
	// err содержит ожидаемый ErrFeed для запоздалого Fail прежней попытки.
	if err := s.Fail(Fallback, 1, ErrFeed, now); !errors.Is(err, ErrFeed) {
		t.Fatal(err)
	}
}

// Проверяет отсутствие готовности до определения длительности обеих дорожек по следующему DTS и отзыв
// доступности по сроку без прогресса.
func TestSwitcherMissingTrackUnknownDurationAndLookahead(t *testing.T) {
	// s содержит переключатель; v и a задают дорожки; now задаёт время проверки отложенных
	// неизвестных длительностей.
	s, v, a, now := switcherFixture(t)
	// sub содержит выходную подписку для чтения пакетов после определения длительностей.
	sub, _ := s.Router().Subscribe(s.config.Queue)
	defer func() {
		// Закрывает выходную подписку после проверки неизвестных длительностей и упреждающего чтения,
		// игнорируя ошибку.
		_ = sub.Close()
	}()
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 0, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 0, false, now))
	if s.Status().Available {
		t.Fatal("неизвестный tail получил duration")
	}
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 1600, 1600, 1600, false, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 1024, 1024, 1024, false, now))
	// got содержит четыре опубликованных пакета после получения следующих DTS обеих дорожек.
	got := []media.Packet{next(t, sub), next(t, sub), next(t, sub), next(t, sub)}
	// dur сохраняет вычисленные длительности начальных пакетов отдельно для видео и аудио.
	dur := map[media.Track]int64{}
	// p содержит очередной выходной пакет при поиске начальных длительностей.
	for _, p := range got {
		if p.Info().DTS == 0 {
			dur[p.Config().Info().Track] = p.Info().DurationTicks
		}
	}
	if dur[media.TrackVideo] != 1600 || dur[media.TrackAudio] != 1024 {
		t.Fatal(dur)
	}
	// err содержит ожидаемый отказ доступности после двух секунд без продвижения.
	if err := s.Tick(now.Add(2 * time.Second)); !errors.Is(err, ErrUnavailable) || s.Status().Available {
		t.Fatal(err, s.Status())
	}
}

// Проверяет неверные настройки и Feed, точный пересчёт времени, защиту переполнений и запрет операций
// после Close.
func TestSwitcherInvalidInputsAndExactArithmetic(t *testing.T) {
	// err содержит ожидаемый отказ создания переключателя с пустыми бюджетами.
	if _, err := NewSwitcher(SwitcherConfig{}); err == nil {
		t.Fatal("приняты пустые бюджеты")
	}
	// tc задаёт исходную метку, временные базы и ожидаемую допустимость точного пересчёта.
	for _, tc := range []struct {
		// value содержит исходную знаковую временную метку.
		value int64
		// from задаёт исходную временную базу; to задаёт целевую временную базу.
		from, to media.TimeBase
		// want содержит точный ожидаемый результат допустимого пересчёта.
		want int64
		// valid показывает, должен ли пересчёт быть точным и представимым в int64.
		valid bool
	}{{-3, media.TimeBase{Numerator: 1, Denominator: 3}, media.TimeBase{Numerator: 1, Denominator: 48000}, -48000, true}, {1, media.TimeBase{Numerator: 1, Denominator: 7}, media.TimeBase{Numerator: 1, Denominator: 48000}, 0, false}, {math.MaxInt64, media.TimeBase{Numerator: 2, Denominator: 1}, media.TimeBase{Numerator: 1, Denominator: 1}, 0, false}} {
		// n содержит результат пересчёта; err сообщает об остатке или выходе за int64.
		n, err := rescale(tc.value, tc.from, tc.to)
		if (err == nil) != tc.valid || tc.valid && n != tc.want {
			t.Fatal(n, err)
		}
	}
	// err содержит ожидаемый отказ сложения с переполнением int64.
	if _, err := sum(math.MaxInt64, 1); err == nil {
		t.Fatal("переполнение сложения")
	}
	// err содержит ожидаемый отказ вычитания с переполнением int64.
	if _, err := difference(math.MinInt64, 1); err == nil {
		t.Fatal("переполнение вычитания")
	}
	// s содержит переключатель; v и a задают контрольный пакет; now задаёт время проверки неверного
	// Feed и закрытия.
	s, v, a, now := switcherFixture(t)
	// err содержит ожидаемый отказ неизвестного номера Feed.
	if err := pushPacket(t, s, Feed(99), 1, v, v, a, 0, 0, 1600, true, now); err == nil {
		t.Fatal("неверный feed")
	}
	// err сообщает об отказе закрытия переключателя перед проверкой запрещённых операций.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(s.Tick(now), media.ErrRouterClosed) || !errors.Is(s.SetPaused(true, now), media.ErrRouterClosed) {
		t.Fatal("закрытый switcher активен")
	}
}

// Проверяет, что запоздавшие метки управления и таймера не сбрасывают окно восстановления LIVE после
// более поздних пакетов.
func TestSwitcherDelayedControlCannotRewindRecovery(t *testing.T) {
	// s содержит переключатель; v и a задают дорожки; now задаёт исходное время восстановления LIVE.
	s, v, a, now := switcherFixture(t)
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 1024, false, now))
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 0, 0, 1024, false, now))
	// Событие таймера ожидало медленного чтения управления; пакеты пришли позже его метки.
	// later задаёт время последующих пакетов, опережающих запоздалые управляющие события.
	later := now.Add(60 * time.Millisecond)
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 1600, 1600, 1600, false, later))
	allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 1024, 1024, 1024, false, later))
	allowUnavailable(t, s.SetPaused(false, now.Add(time.Millisecond)))
	allowUnavailable(t, s.Tick(now.Add(2*time.Millisecond)))
	later = now.Add(120 * time.Millisecond)
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 3200, 3200, 1600, true, later))
	allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 3200, 3200, 1024, false, later))
	// status содержит итог после запоздалых SetPaused и Tick для проверки сохранения окна
	// восстановления.
	if status := s.Status(); status.Source != Live || !status.Available {
		t.Fatalf("запаздывающее управление сбросило стабильный источник: %+v", status)
	}
}

// Проверяет ограничение очереди невыбранного LIVE и включение отложенного пакета в общий бюджет без
// повреждения исправной заглушки.
func TestSwitcherBoundedUnselectedBacklogAndPendingBudget(t *testing.T) {
	// s содержит переключатель; v и a задают дорожки; now задаёт время проверки бюджета невыбранного
	// LIVE.
	s, v, a, now := switcherFixture(t)
	s.config.Queue.MaxPackets = 4
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 1024, false, now))
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 0, 0, 1600, true, now))
	// i задаёт последовательный номер зависимого видеопакета, заполняющего очередь LIVE.
	for i := int64(1); i <= 5; i++ {
		allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, i*1600, i*1600, 1600, false, now.Add(time.Duration(i)*time.Millisecond)))
	}
	if len(s.feeds[0].queue) > 4 || s.Status().Source != Fallback || !s.Status().Available {
		t.Fatal("переполнение чужой очереди повредило fallback", s.Status())
	}
	s.config.Queue.MaxBytes = 50
	s.config.Queue.MaxPacketBytes = 40
	s.config.Queue.MaxConfigBytes = 20
	// err содержит ожидаемый ErrRouterBudget для отложенного пакета без длительности.
	if err := pushPacket(t, s, Live, 2, v, v, a, 0, 0, 0, true, now); !errors.Is(err, media.ErrRouterBudget) {
		t.Fatalf("pending не вошёл в общий бюджет: %v", err)
	}
	if s.feeds[0].pending[0] != nil || s.feeds[0].bytes != 0 || !s.Status().Available {
		t.Fatal("отказ бюджета сохранил данные либо повредил fallback")
	}
}

// Проверяет исключение старого неизвестного хвоста после смены конфигурации и отказ перехода с
// чрезмерным A/V-расхождением.
func TestSwitcherDiscardsPendingOnConfigChangeAndRejectsImpossibleCut(t *testing.T) {
	// s содержит переключатель; v и a задают исходные дорожки; now задаёт время проверки смены
	// конфигурации и невозможного перехода.
	s, v, a, now := switcherFixture(t)
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 0, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 0, false, now))
	// vi содержит описание видео; ai содержит описание аудио для назначения новой общей эпохи.
	vi, ai := v.Info(), a.Info()
	vi.SourceEpoch++
	ai.SourceEpoch++
	// freshV содержит видеоконфигурацию новой эпохи без старого неизвестного хвоста.
	freshV, _ := media.NewCodecConfig(vi, v.Headers())
	// freshA содержит аудиоконфигурацию новой эпохи.
	freshA, _ := media.NewCodecConfig(ai, a.Headers())
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, freshV, freshV, freshA, 100000, 100000, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, freshA, freshV, freshA, 100000, 100000, 1024, false, now))
	if !s.Status().Available || s.horizon != 1600 {
		t.Fatal("старый неизвестный хвост попал в новую конфигурацию", s.Status(), s.horizon)
	}
	// err содержит ожидаемый отказ прежних заголовков после смены эпохи.
	if err := pushPacket(t, s, Fallback, 1, v, v, a, 1600, 1600, 1600, true, now); !errors.Is(err, ErrFeed) {
		t.Fatal("старые заголовки вернулись", err)
	}
	s.config.MaxAVSkew = time.Millisecond
	allowUnavailable(t, pushPacket(t, s, Fallback, 2, freshV, freshV, freshA, 0, 0, 1600, true, now))
	// err содержит ожидаемую недоступность при чрезмерном A/V-расхождении новой попытки.
	if err := pushPacket(t, s, Fallback, 2, freshA, freshV, freshA, 480, 480, 1024, false, now); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if s.Status().Available {
		t.Fatal("непредставимый согласованный cut объявлен доступным")
	}
}

// Проверяет отказ переполненной разности DTS при определении неизвестной длительности до публикации и
// появления выходного горизонта.
func TestSwitcherRejectsUnknownSourceDurationOverflowWithoutPublication(t *testing.T) {
	// s содержит переключатель; v и a задают контрольные дорожки; now задаёт время проверки
	// переполнения неизвестной длительности.
	s, v, a, now := switcherFixture(t)
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, math.MinInt64, math.MinInt64, 0, true, now))
	// err содержит ожидаемый ErrTiming при непредставимой разности соседних DTS.
	if err := pushPacket(t, s, Fallback, 1, v, v, a, math.MaxInt64, math.MaxInt64, 1600, true, now); !errors.Is(err, ErrTiming) {
		t.Fatal(err)
	}
	if s.hasHorizon || s.Status().Available {
		t.Fatal("переполнение lookahead опубликовано")
	}
}

// Проверяет сохранение текущей заглушки и её эпохи при переполнении времени кандидата LIVE.
func TestSwitcherInvalidTransitionCannotReplaceHealthyFallbackConfig(t *testing.T) {
	// s содержит переключатель; v и a задают дорожки; now задаёт первоначальную пригодную заглушку
	// перед неверным LIVE-переходом.
	s, v, a, now := switcherFixture(t)
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 1024, false, now))
	// epoch сохраняет эпоху исправной заглушки до проверки непредставимого LIVE.
	epoch := s.Status().Epoch
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 0, 0, 1024, false, now))
	// later задаёт время после окна восстановления для проверки кандидата LIVE с переполняющей
	// длительностью.
	later := now.Add(120 * time.Millisecond)
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 1600, 1600, math.MaxInt64, true, later))
	allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 1600, 1600, 1024, false, later))
	// status содержит итог после отклонённого LIVE-перехода для проверки сохранения заглушки и её
	// эпохи.
	if status := s.Status(); status.Source != Fallback || !status.Available || status.Epoch != epoch {
		t.Fatalf("невалидный сегмент сменил исправные заголовки: %+v", status)
	}
}
