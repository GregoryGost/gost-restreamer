package group

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// Возвращает полный профиль H.264/AAC и конечные бюджеты переключения для приёмочных тестов.
func switchAcceptanceConfig() SwitcherConfig {
	return SwitcherConfig{Profile: core.GroupProfile{Mode: core.ModeTranscode, Format: core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecH264, Width: 128, Height: 128, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: "high", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 200000, MaxBitrateBPS: 300000, BufferBits: 400000, GOPFrames: 30, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 64000}}, Watchdog: media.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: 100 * time.Millisecond, KeyframeTimeout: time.Second}, Queue: media.QueueLimits{MaxPackets: 100, MaxBytes: 1 << 20, MaxPacketBytes: 1 << 16, MaxConfigBytes: 1 << 12, ResyncTimeout: time.Second}, Router: media.RouterLimits{MaxConsumers: 4, MaxRetainedBytes: 5 << 20, MaxConfigBytes: 1 << 12}, MaxAVSkew: 100 * time.Millisecond, MaxTransitionGap: 100 * time.Millisecond}
}

// Создаёт согласованную пару конфигураций H.264/AAC с заданными эпохой и ревизией и контрольными
// заголовками.
func switchAcceptanceConfigs(t *testing.T, epoch, revision uint64) (media.CodecConfig, media.CodecConfig) {
	t.Helper()
	// Заголовки libx264 из транспортного тестового набора описывают High/yuv420p, 128×128, 30 FPS.
	// headers содержит контрольный avcC; err сохраняет результаты декодирования заголовков и
	// создания конфигураций.
	headers, err := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	if err != nil {
		t.Fatal(err)
	}
	// f содержит ожидаемый контрольный H.264/AAC-формат.
	f := switchAcceptanceConfig().Profile.Format
	// v содержит H.264-конфигурацию заданной эпохи и ревизии со шкалой 1/30.
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: epoch, Revision: revision, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30}, Video: f.Video}, headers)
	if err != nil {
		t.Fatal(err)
	}
	// a содержит AAC-конфигурацию той же эпохи и ревизии со шкалой 1/48000.
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: epoch, Revision: revision, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: f.Audio}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	return v, a
}

// Создаёт контрольную единицу H.264 либо AAC с заданными метками; видеосрезы проверяют независимость,
// не устанавливая декодируемость изображения.
func switchAcceptancePacket(t *testing.T, c media.CodecConfig, pts, dts, duration int64, key bool) media.Packet {
	t.Helper()
	// payload выбирает контрольный H.264-срез либо полный AAC AU по дорожке и независимости.
	payload := []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}
	if c.Info().Track == media.TrackAudio {
		payload = switchAcceptanceAACPayload()
	} else if !key {
		payload = []byte{0, 0, 0, 2, 0x41, 0xe1}
	}
	// Срезы содержат поля независимости по H.264; декодируемость изображения здесь не утверждается.
	// p содержит созданную единицу доступа с заданными метками; err сообщает об отказе создания.
	p, err := media.NewPacket(media.PacketInfo{PTS: pts, DTS: dts, DurationTicks: duration, Keyframe: key}, c, payload)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Нативный AAC-кодировщик FFmpeg 7.1.5 создал этот стереофонический AAC-LC AU при 48 kHz;
// независимое декодирование одного AU даёт 1024 нулевых сэмпла каждого канала.
func switchAcceptanceAACPayload() []byte { return []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c} }

// Создаёт переключатель с подпиской и регистрирует закрытие переключателя после теста.
func newSwitchAcceptance(t *testing.T) (*Switcher, *media.Subscription) {
	t.Helper()
	// s содержит переключатель приёмочного сценария; err сохраняет результаты создания и подписки.
	s, err := NewSwitcher(switchAcceptanceConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает тестовый Switcher после сценария и сообщает ошибку через t.Error.

		// err сообщает об отказе закрытия переключателя при резервной очистке.
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	// sub содержит подписку на постоянный выход приёмочного переключателя.
	sub, err := s.Router().Subscribe(switchAcceptanceConfig().Queue)
	if err != nil {
		t.Fatal(err)
	}
	return s, sub
}

// Передаёт тестовый пакет переключателю, допуская временную недоступность до готовности источника.
func pushSwitchAcceptance(t *testing.T, s *Switcher, f Feed, attempt uint64, p media.Packet, v, a media.CodecConfig, now time.Time) {
	t.Helper()
	// err содержит результат передачи контрольного пакета с допустимой начальной недоступностью.
	err := s.Push(f, attempt, p, v, a, now)
	if err != nil && !errors.Is(err, ErrUnavailable) {
		t.Fatalf("подача %v: %v", f, err)
	}
}

// Получает следующую выдачу подписки в конечный срок либо завершает тест ошибкой.
func switchAcceptanceNext(t *testing.T, s *media.Subscription) media.Delivery {
	t.Helper()
	// ctx ограничивает ожидание выдачи одной секундой; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// d содержит очередную выдачу подписки; err сообщает об отказе её получения.
	d, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Преобразует знаковую метку в точное рациональное число секунд для сравнения разных временных баз.
func tickSeconds(ticks int64, base media.TimeBase) *big.Rat {
	return new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(ticks), new(big.Int).SetUint64(uint64(base.Numerator))), new(big.Int).SetUint64(uint64(base.Denominator)))
}

// Проверяет сохранение PTS−DTS и общего A/V-смещения при смене эпох и расположение нового
// декодирования после максимального прежнего горизонта представления.
func TestSwitcherPreservesCompositionAndAVOffsetAcrossFreshEpochs(t *testing.T) {
	// config включает два B-кадра для проверки композиционного смещения и горизонта представления.
	config := switchAcceptanceConfig()
	config.Profile.VideoEncoding.MaxBFrames = 2
	// s содержит проверяемый переключатель; err сохраняет результаты создания, подписки и
	// пересоздания B-кадра.
	s, err := NewSwitcher(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает Switcher после проверки композиционного и междорожечного смещения, игнорируя ошибку.
		_ = s.Close()
	})
	// sub содержит выходную подписку для сравнения временных меток разных эпох.
	sub, err := s.Router().Subscribe(config.Queue)
	if err != nil {
		t.Fatal(err)
	}
	// now задаёт исходное контрольное время появления заглушки.
	now := time.Unix(100, 0)
	// v содержит исходную H.264-конфигурацию эпохи 91; a содержит согласованную AAC-конфигурацию.
	v, a := switchAcceptanceConfigs(t, 91, 7)
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, v, 2, 0, 1, true), v, a, now)
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, a, 3200, 3200, 1024, false), v, a, now)
	// vd содержит очередную выдачу видео; ad содержит согласованную выдачу аудио для проверки общего
	// сдвига.
	vd, ad := switchAcceptanceNext(t, sub), switchAcceptanceNext(t, sub)
	if vd.Packet.Config().Info().Track != media.TrackVideo || ad.Packet.Config().Info().Track != media.TrackAudio {
		t.Fatal("потеряна согласованная пара A/V")
	}
	// composition содержит точную разницу PTS и DTS выходного видео в секундах.
	composition := tickSeconds(vd.Packet.Info().PTS-vd.Packet.Info().DTS, vd.Packet.Config().Info().TimeBase)
	if composition.Cmp(big.NewRat(1, 15)) != 0 {
		t.Fatalf("PTS−DTS изменён: %s", composition)
	}
	if tickSeconds(vd.Packet.Info().PTS, vd.Packet.Config().Info().TimeBase).Cmp(tickSeconds(ad.Packet.Info().PTS, ad.Packet.Config().Info().TimeBase)) != 0 {
		t.Fatal("общий A/V origin разошёлся")
	}
	// firstEpoch сохраняет первую выходную эпоху до перехода на LIVE.
	firstEpoch := vd.VideoConfig.Info().SourceEpoch
	// firstRevision сохраняет первую выходную ревизию конфигураций до перехода.
	firstRevision := vd.VideoConfig.Info().Revision
	// Будущий P-кадр задаёт верхнюю границу отображения; следующий B-кадр её не уменьшает.
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, v, 6, 1, 1, false), v, a, now.Add(10*time.Millisecond))
	// futureFrame содержит выходной P-кадр с будущей границей представления.
	futureFrame := switchAcceptanceNext(t, sub)
	// horizon задаёт точный конец представления будущего P-кадра в секундах.
	horizon := tickSeconds(futureFrame.Packet.Info().PTS+futureFrame.Packet.Info().DurationTicks, futureFrame.Packet.Config().Info().TimeBase)
	// bframe содержит B-кадр с более ранним PTS для проверки сохранения максимального горизонта.
	bframe := switchAcceptancePacket(t, v, 3, 2, 1, false)
	bframe, err = media.NewPacket(bframe.Info(), v, []byte{0, 0, 0, 3, 0x01, 0xa8, 0x40})
	if err != nil {
		t.Fatal(err)
	}
	pushSwitchAcceptance(t, s, Fallback, 1, bframe, v, a, now.Add(15*time.Millisecond))
	// lastDisplayed содержит выдачу B-кадра, чей конец представления ниже уже достигнутого
	// горизонта.
	lastDisplayed := switchAcceptanceNext(t, sub)
	if tickSeconds(lastDisplayed.Packet.Info().PTS+lastDisplayed.Packet.Info().DurationTicks, lastDisplayed.Packet.Config().Info().TimeBase).Cmp(horizon) >= 0 {
		t.Fatal("вход не создал перестановку presentation PTS для проверки верхней границы")
	}
	// lv содержит H.264-конфигурацию нового LIVE; la содержит согласованную AAC-конфигурацию с
	// независимой исходной эпохой.
	lv, la := switchAcceptanceConfigs(t, 1, 1)
	pushSwitchAcceptance(t, s, Live, 1, switchAcceptancePacket(t, lv, -10, -12, 1, true), lv, la, now.Add(20*time.Millisecond))
	pushSwitchAcceptance(t, s, Live, 1, switchAcceptancePacket(t, la, -16000, -16000, 1024, false), lv, la, now.Add(20*time.Millisecond))
	pushSwitchAcceptance(t, s, Live, 1, switchAcceptancePacket(t, lv, -9, -11, 1, true), lv, la, now.Add(150*time.Millisecond))
	pushSwitchAcceptance(t, s, Live, 1, switchAcceptancePacket(t, la, -14400, -14400, 1024, false), lv, la, now.Add(150*time.Millisecond))
	vd, ad = switchAcceptanceNext(t, sub), switchAcceptanceNext(t, sub)
	if s.Status().Source != Live || !s.Status().Available {
		t.Fatalf("устойчивый LIVE не выбран: %+v", s.Status())
	}
	if tickSeconds(vd.Packet.Info().DTS, vd.Packet.Config().Info().TimeBase).Cmp(horizon) < 0 {
		t.Fatalf("новый decode наложился на прежний presentation horizon: %+v", vd.Packet.Info())
	}
	if tickSeconds(vd.Packet.Info().PTS-vd.Packet.Info().DTS, vd.Packet.Config().Info().TimeBase).Cmp(big.NewRat(1, 15)) != 0 {
		t.Fatal("композиционное смещение потеряно при переключении")
	}
	if tickSeconds(vd.Packet.Info().PTS, vd.Packet.Config().Info().TimeBase).Cmp(tickSeconds(ad.Packet.Info().PTS, ad.Packet.Config().Info().TimeBase)) != 0 {
		t.Fatal("переключение независимо обнулило дорожки")
	}
	if vd.VideoConfig.Info().SourceEpoch <= firstEpoch || vd.VideoConfig.Info().Revision <= firstRevision || vd.VideoConfig.Info().SourceEpoch != ad.AudioConfig.Info().SourceEpoch || !vd.IsCurrent() || !ad.IsCurrent() {
		t.Fatal("новая выходная epoch/config несогласованна")
	}
}

// Проверяет отсутствие готовности при одной дорожке, ложном ключевом флаге, отсутствующем PPS и
// неполном AAC.
func TestSwitcherRequiresBothTracksAndBitstreamRandomAccess(t *testing.T) {
	// mode выбирает отсутствие дорожки, ложную независимость либо повреждение H.264/AAC.
	for _, mode := range []string{"video-only", "audio-only", "false-key-flag", "missing-pps", "truncated-aac", "aac-without-end"} {
		t.Run(mode, func(t *testing.T) {
			// Проверяет отсутствие подачи при выбранной недостающей дорожке, ложном ключевом кадре или
			// повреждённых H.264/AAC данных.

			// s содержит переключатель проверки непригодного входного начала.
			s, _ := newSwitchAcceptance(t)
			// now задаёт одинаковое время контрольных пакетов выбранного нарушения.
			now := time.Unix(200, 0)
			// v содержит проверяемую H.264-конфигурацию; a содержит AAC-конфигурацию того же
			// источника.
			v, a := switchAcceptanceConfigs(t, 1, 1)
			// p содержит стартовый видеопакет, при необходимости заменяемый зависимым
			// срезом.
			p := switchAcceptancePacket(t, v, 0, 0, 1, true)
			if mode == "false-key-flag" {
				// err сообщает об отказе создания видеопакета с намеренно ложным
				// ключевым признаком.
				var err error
				p, err = media.NewPacket(p.Info(), v, []byte{0, 0, 0, 2, 0x41, 0xe1})
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-pps" {
				// headers содержит изменяемую копию avcC, из которой исключается PPS.
				headers := v.Headers()
				headers = headers[:33]
				headers[32] = 0
				// err сообщает об отказе пересоздания видеоконфигурации с отсутствующим
				// PPS.
				var err error
				v, err = media.NewCodecConfig(v.Info(), headers)
				if err != nil {
					t.Fatal(err)
				}
				p = switchAcceptancePacket(t, v, 0, 0, 1, true)
			}
			if mode != "audio-only" {
				// err содержит результат передачи проверяемого видео до проверки общей
				// недоступности.
				err := s.Push(Fallback, 1, p, v, a, now)
				if err == nil && mode == "missing-pps" {
					t.Fatal("повреждённый avcC принят")
				}
			}
			if mode != "video-only" {
				// audio содержит стартовый AAC-пакет, заменяемый повреждёнными данными
				// выбранного сценария.
				audio := switchAcceptancePacket(t, a, 0, 0, 1024, false)
				if mode == "truncated-aac" {
					// err сообщает об отказе создания пакета с обрезанным AAC.
					var err error
					audio, err = media.NewPacket(audio.Info(), a, []byte{0})
					if err != nil {
						t.Fatal(err)
					}
				}
				if mode == "aac-without-end" {
					// err сообщает об отказе создания пакета AAC без завершающего
					// элемента.
					var err error
					audio, err = media.NewPacket(audio.Info(), a, []byte{0x20, 0, 0, 0, 0, 0, 0})
					if err != nil {
						t.Fatal(err)
					}
				}
				// err содержит результат инспекции передаваемого AAC для проверки
				// codec.ErrCodec.
				err := s.Push(Fallback, 1, audio, v, a, now)
				if (mode == "truncated-aac" || mode == "aac-without-end") && !errors.Is(err, codec.ErrCodec) {
					t.Fatalf("обрезанный AAC SCE принят как полная единица доступа: %v", err)
				}
			}
			// status содержит итоговую доступность после неполной либо повреждённой пары
			// дорожек.
			if status := s.Status(); status.Available {
				t.Fatalf("непригодный источник объявлен доступным: %+v", status)
			}
		})
	}
}

// Проверяет отказ старой попытки, сохранение подписки при ручной паузе и недоступность при утрате её
// заглушки несмотря на приход LIVE.
func TestSwitcherRejectsRegressingAttemptAndKeepsSubscriptionThroughPause(t *testing.T) {
	// s содержит переключатель; sub содержит сохраняемую выходную подписку сценария ручной паузы.
	s, sub := newSwitchAcceptance(t)
	// now задаёт время первоначальной готовности заглушки второй попытки.
	now := time.Unix(300, 0)
	// v содержит конфигурацию видео заглушки; a содержит конфигурацию её AAC.
	v, a := switchAcceptanceConfigs(t, 1, 1)
	pushSwitchAcceptance(t, s, Fallback, 2, switchAcceptancePacket(t, v, 0, 0, 1, true), v, a, now)
	pushSwitchAcceptance(t, s, Fallback, 2, switchAcceptancePacket(t, a, 0, 0, 1024, false), v, a, now)
	switchAcceptanceNext(t, sub)
	switchAcceptanceNext(t, sub)
	// err содержит ожидаемый отказ первой попытки после принятой второй.
	if err := s.Push(Fallback, 1, switchAcceptancePacket(t, v, 1, 1, 1, true), v, a, now); !errors.Is(err, ErrFeed) {
		t.Fatalf("старый attempt принят: %v", err)
	}
	// err содержит результат включения ручной паузы при пригодной заглушке.
	if err := s.SetPaused(true, now); err != nil {
		t.Fatal(err)
	}
	// lv содержит конфигурацию проверяемого LIVE; la содержит согласованную AAC-конфигурацию LIVE.
	lv, la := switchAcceptanceConfigs(t, 2, 2)
	// i задаёт номер очередной контрольной LIVE-пары во время ручной паузы.
	for i := range int64(3) {
		// at задаёт локальное время поступления этой LIVE-пары.
		at := now.Add(time.Duration(i+1) * 150 * time.Millisecond)
		pushSwitchAcceptance(t, s, Live, 1, switchAcceptancePacket(t, lv, i, i, 1, true), lv, la, at)
		pushSwitchAcceptance(t, s, Live, 1, switchAcceptancePacket(t, la, i*1600, i*1600, 1024, false), lv, la, at)
	}
	// status содержит итог после LIVE-пакетов, которые не должны снимать ручную паузу.
	if status := s.Status(); status.Source != Fallback || !status.Paused || !status.Available {
		t.Fatalf("OBS снял ручную паузу: %+v", status)
	}
	pushSwitchAcceptance(t, s, Fallback, 2, switchAcceptancePacket(t, v, 1, 1, 1, true), v, a, now.Add(500*time.Millisecond))
	pushSwitchAcceptance(t, s, Fallback, 2, switchAcceptancePacket(t, a, 1600, 1600, 1024, false), v, a, now.Add(500*time.Millisecond))
	// d содержит следующую выходную выдачу для проверки неизменных байтов заглушки.
	if d := switchAcceptanceNext(t, sub); !bytes.Equal(d.Packet.Payload(), switchAcceptancePacket(t, v, 1, 1, 1, true).Payload()) {
		t.Fatal("пауза подменила fallback")
	}
	select {
	case <-sub.Done():
		t.Fatal("переход закрыл подписку")
	default:
	}
	// err содержит ожидаемую недоступность после потери заглушки при ручной паузе.
	if err := s.Fail(Fallback, 2, errors.New("Файл утрачен"), now.Add(500*time.Millisecond)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("pause без fallback должна сообщить unavailable: %v", err)
	}
	if s.Status().Available {
		t.Fatal("пауза без пригодного fallback объявлена успешной")
	}
}

// Проверяет точные длительности AV1 30000/1001 и AAC, непрерывные DTS и неизменную эпоху на
// последовательности повторяющихся независимых кадров.
func TestSwitcherAV1RationalCadenceRemainsContinuousOverRepeatedLoops(t *testing.T) {
	// c задаёт AV1-профиль 640×360 с рациональным FPS 30000/1001.
	c := switchAcceptanceConfig()
	c.Profile = acceptanceRuntimeProfile(core.CodecAV1, 640, 360)
	c.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 30000, Denominator: 1001}
	// s содержит переключатель рациональной последовательности; err сохраняет результаты создания и
	// конфигураций.
	s, err := NewSwitcher(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает Switcher после проверки непрерывных рациональных петель, игнорируя ошибку.
		_ = s.Close()
	}()
	// sub содержит выходную подписку для проверки всех кадров и AAC AU последовательности.
	sub, err := s.Router().Subscribe(c.Queue)
	if err != nil {
		t.Fatal(err)
	}
	// v содержит AV1-конфигурацию с исходной шкалой 1/30000.
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 700, Revision: 90, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30000}, Video: c.Profile.Format.Video}, acceptanceVideoHeadersAtRate(core.CodecAV1, 640, 360, c.Profile.Format.Video.FrameRate))
	if err != nil {
		t.Fatal(err)
	}
	// a содержит AAC-конфигурацию той же эпохи со шкалой 1/48000.
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 700, Revision: 90, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: c.Profile.Format.Audio}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	// now задаёт исходное время многократного повторения независимых кадров.
	now := time.Unix(1000, 0)
	// audio хранит следующую AAC-метку с шагом 1024 в шкале 1/48000.
	var audio int64
	// outputEpoch сохраняет первую выходную эпоху для проверки её неизменности во всех 180 кадрах.
	var outputEpoch uint64
	// last сохраняет предыдущий выходной DTS каждой дорожки как точное число секунд.
	last := map[media.Track]*big.Rat{}
	// frame задаёт номер очередного AV1-кадра из контрольной последовательности.
	for frame := range int64(180) {
		// at задаёт время кадра по точной рациональной сетке 30000/1001.
		at := now.Add(time.Duration(frame) * 1001 * time.Second / 30000)
		// p содержит очередной AV1-пакет длительностью 1001; err сообщает об отказе его
		// создания.
		p, err := media.NewPacket(media.PacketInfo{PTS: frame * 1001, DTS: frame * 1001, DurationTicks: 1001, Keyframe: false}, v, append(v.Headers()[4:], 0x1a, 1, 0x10))
		if err != nil {
			t.Fatal(err)
		}
		pushSwitchAcceptance(t, s, Fallback, 1, p, v, a, at)
		// count считает выходные пакеты текущего видеокадра и относящихся к его интервалу AAC
		// AU.
		count := 1
		for audio*30000 < (frame+1)*1001*48000 {
			// p содержит очередной AAC AU длительностью 1024 для текущего видеоинтервала.
			p := switchAcceptancePacket(t, a, audio, audio, 1024, false)
			pushSwitchAcceptance(t, s, Fallback, 1, p, v, a, at)
			audio += 1024
			count++
		}
		// i обозначает номер очередного ожидаемого выходного пакета текущего кадра.
		for i := 0; i < count; i++ {
			// d содержит очередную выходную выдачу рациональной последовательности.
			d := switchAcceptanceNext(t, sub)
			// info содержит выходные DTS и длительность проверяемого пакета.
			info := d.Packet.Info()
			// track задаёт дорожку проверяемого пакета для выбора ожидаемого шага.
			track := d.Packet.Config().Info().Track
			// base содержит выходную временную базу проверяемой дорожки.
			base := d.Packet.Config().Info().TimeBase
			// stamp содержит точный выходной DTS в секундах.
			stamp := tickSeconds(info.DTS, base)
			// duration содержит точную выходную длительность в секундах.
			duration := tickSeconds(info.DurationTicks, base)
			// want задаёт ожидаемую длительность 1001/30000 для видео либо 1024/48000 для
			// аудио.
			want := big.NewRat(1001, 30000)
			if track == media.TrackAudio {
				want = big.NewRat(1024, 48000)
			}
			if duration.Cmp(want) != 0 {
				t.Fatalf("rational duration %s: %s != %s", track, duration, want)
			}
			// previous содержит предыдущий DTS той же дорожки для проверки непрерывного
			// шага.
			if previous := last[track]; previous != nil && new(big.Rat).Sub(stamp, previous).Cmp(want) != 0 {
				t.Fatalf("разрыв рациональной петли %s на кадре %d", track, frame)
			}
			last[track] = stamp
			if outputEpoch == 0 {
				outputEpoch = d.VideoConfig.Info().SourceEpoch
			}
			if d.VideoConfig.Info().SourceEpoch != outputEpoch {
				t.Fatal("обычная непрерывная петля сменила epoch")
			}
		}
	}
}

// Проверяет определение неизвестной длительности только следующим пакетом и отсутствие продления
// здоровья источника повторным DTS.
func TestSwitcherDoesNotInferUnknownTailOrHealthFromDuplicates(t *testing.T) {
	// s содержит переключатель проверки неизвестного хвоста и повторного DTS.
	s, _ := newSwitchAcceptance(t)
	// now задаёт исходное время пакетов без длительности.
	now := time.Unix(1100, 0)
	// v содержит H.264-конфигурацию; a содержит AAC-конфигурацию контрольного источника.
	v, a := switchAcceptanceConfigs(t, 1, 1)
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, v, 0, 0, 0, true), v, a, now)
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, a, 0, 0, 0, false), v, a, now)
	if s.Status().Available {
		t.Fatal("неизвестная длительность хвоста заменена догадкой")
	}
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, v, 1, 1, 1, false), v, a, now.Add(time.Millisecond))
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, a, 1024, 1024, 1024, false), v, a, now.Add(time.Millisecond))
	if !s.Status().Available {
		t.Fatal("bounded lookahead не завершил первые AU")
	}
	// duplicate содержит видеопакет с DTS, равным последнему принятому значению.
	duplicate := switchAcceptancePacket(t, v, 1, 1, 1, true)
	// err содержит ожидаемый отказ дубликата, пришедшего через 900 мс.
	if err := s.Push(Fallback, 1, duplicate, v, a, now.Add(900*time.Millisecond)); !errors.Is(err, ErrFeed) {
		t.Fatalf("дубликат принят как продвижение: %v", err)
	}
	// err содержит ожидаемую недоступность после истечения срока без настоящего продвижения.
	if err := s.Tick(now.Add(1100 * time.Millisecond)); !errors.Is(err, ErrUnavailable) || s.Status().Available {
		t.Fatalf("дубликат сохранил ложное здоровье: %+v error=%v", s.Status(), err)
	}
}

// Проверяет сохранение исправного выбранного LIVE при запоздавшем Tick и отказе невыбранной заглушки.
func TestSwitcherDelayedControlAndFallbackFailurePreserveHealthyLive(t *testing.T) {
	// s содержит переключатель для проверки сохранения здорового LIVE.
	s, _ := newSwitchAcceptance(t)
	// now задаёт исходное время готовности заглушки и восстановления LIVE.
	now := time.Unix(1200, 0)
	// v содержит конфигурацию видео заглушки; a содержит её аудиоконфигурацию.
	v, a := switchAcceptanceConfigs(t, 1, 1)
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, v, 0, 0, 1, true), v, a, now)
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, a, 0, 0, 1024, false), v, a, now)
	// lv содержит конфигурацию видео LIVE; la содержит его аудиоконфигурацию.
	lv, la := switchAcceptanceConfigs(t, 2, 2)
	// i обозначает номер LIVE-пары; offset задаёт её время относительно начала проверки.
	for i, offset := range []time.Duration{10 * time.Millisecond, 150 * time.Millisecond} {
		// stamp задаёт исходную видеометку проверяемой LIVE-пары.
		stamp := int64(i)
		// at содержит локальное время поступления выбранной LIVE-пары.
		at := now.Add(offset)
		pushSwitchAcceptance(t, s, Live, 1, switchAcceptancePacket(t, lv, stamp, stamp, 1, true), lv, la, at)
		pushSwitchAcceptance(t, s, Live, 1, switchAcceptancePacket(t, la, stamp*1600, stamp*1600, 1024, false), lv, la, at)
	}
	if s.Status().Source != Live || !s.Status().Available {
		t.Fatal("нет пригодного LIVE после recovery")
	}
	// before сохраняет состояние и эпоху исправного LIVE до запоздалого Tick.
	before := s.Status()
	// err содержит результат запоздалой проверки времени, которая должна сохранить здоровье LIVE.
	if err := s.Tick(now.Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	// after содержит состояние после запоздалого Tick для проверки источника, доступности и эпохи.
	if after := s.Status(); after.Source != Live || !after.Available || after.Epoch != before.Epoch {
		t.Fatalf("запаздавшее control-событие откатило часы здоровья: %+v", after)
	}
	// err содержит результат отказа невыбранной заглушки при продолжающемся здоровом LIVE.
	if err := s.Fail(Fallback, 1, errors.New("Недоступен файл заглушки"), now.Add(160*time.Millisecond)); err != nil {
		t.Fatalf("отказ fallback сломал пригодный LIVE: %v", err)
	}
	// after содержит состояние LIVE после отказа невыбранного источника.
	if after := s.Status(); after.Source != Live || !after.Available {
		t.Fatalf("отказ невыбранного источника изменил доступную подачу: %+v", after)
	}
}

// Проверяет, что принятый переход на LIVE не создаёт разрыв представления больше заданного предела.
func TestSwitcherRefusesCutBeyondConfiguredPresentationGap(t *testing.T) {
	// c ограничивает допустимый разрыв представления одной миллисекундой.
	c := switchAcceptanceConfig()
	c.MaxTransitionGap = time.Millisecond
	// s содержит переключатель проверки предела перехода; err сохраняет результаты создания и
	// подписки.
	s, err := NewSwitcher(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает Switcher после проверки предела разрыва отображения, игнорируя ошибку.
		_ = s.Close()
	}()
	// sub содержит выходную подписку для сравнения границ представления до и после возможного
	// перехода.
	sub, err := s.Router().Subscribe(c.Queue)
	if err != nil {
		t.Fatal(err)
	}
	// now задаёт исходное время контрольного перехода с композиционным смещением.
	now := time.Unix(1300, 0)
	// v содержит конфигурацию видео заглушки; a содержит согласованную AAC-конфигурацию.
	v, a := switchAcceptanceConfigs(t, 1, 1)
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, v, 0, 0, 1, true), v, a, now)
	pushSwitchAcceptance(t, s, Fallback, 1, switchAcceptancePacket(t, a, 0, 0, 1024, false), v, a, now)
	// first содержит первый выходной видеопакет пригодной заглушки.
	first := switchAcceptanceNext(t, sub)
	_ = switchAcceptanceNext(t, sub)
	// oldPresentationEnd задаёт точный конец представления первоначального видео в секундах.
	oldPresentationEnd := tickSeconds(first.Packet.Info().PTS+first.Packet.Info().DurationTicks, first.Packet.Config().Info().TimeBase)
	// lv содержит конфигурацию кандидата LIVE; la содержит согласованную AAC-конфигурацию.
	lv, la := switchAcceptanceConfigs(t, 2, 2)
	// i обозначает номер LIVE-пары; offset задаёт время её поступления относительно начала.
	for i, offset := range []time.Duration{10 * time.Millisecond, 150 * time.Millisecond} {
		// stamp задаёт исходный DTS видео кандидата LIVE.
		stamp := int64(i)
		// at задаёт локальное время подачи пакетов кандидата перехода.
		at := now.Add(offset)
		_ = s.Push(Live, 1, switchAcceptancePacket(t, lv, stamp+2, stamp, 1, true), lv, la, at)
		_ = s.Push(Live, 1, switchAcceptancePacket(t, la, (stamp+2)*1600, (stamp+2)*1600, 1024, false), lv, la, at)
	}
	// status содержит итог проверки кандидата LIVE при ограничении разрыва представления.
	if status := s.Status(); status.Source == Live && status.Available {
		// d содержит первый выходной пакет LIVE, если переход всё же был принят.
		d := switchAcceptanceNext(t, sub)
		// gap содержит точную разницу его PTS и прежнего конца представления в секундах.
		gap := new(big.Rat).Sub(tickSeconds(d.Packet.Info().PTS, d.Packet.Config().Info().TimeBase), oldPresentationEnd)
		if gap.Cmp(big.NewRat(1, 1000)) > 0 {
			t.Fatalf("принят cut с presentation gap %s при бюджете 1/1000 секунды", gap)
		}
	}
}
