package group

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// bindingProfile создаёт полный профиль с конечным аудиокодеком kind поверх прежнего рецепта H.264 SDR.
// t сообщает о повреждении полной fixture до проверки границ группы.
func bindingProfile(t *testing.T, kind core.Codec) core.GroupProfile {
	t.Helper()
	// profile сохраняет размеры, цвет, бюджеты кодирования и полный AAC LC профиль при наследовании.
	profile := switchAcceptanceConfig().Profile
	profile.LiveAudioMode = core.LiveAudioTranscode
	profile.Encoding = core.DefaultEncoding(core.ModeTranscode, core.VideoEncoderX264, core.RateControlVBR)
	// code задаёт профиль кодека, encoder выбирает кодировщик, bitrate задаёт целевой битрейт.
	code, encoder, bitrate := "LC", core.AudioEncoderAAC, uint64(64000)
	switch kind {
	case core.CodecOpus:
		code, encoder, bitrate = "celt", core.AudioEncoderOpus, 128000
	case core.CodecFLAC:
		code, encoder, bitrate = "s16", core.AudioEncoderFLAC, 0
	case core.CodecMP3:
		code, encoder, bitrate = "mpeg1-layer3", core.AudioEncoderMP3, 128000
	case core.CodecAC3:
		code, encoder, bitrate = "ac3", core.AudioEncoderAC3, 192000
	}
	profile.Format.Audio = core.AudioFormat{Codec: kind, CodecProfile: code, SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}
	profile.Encoding.AudioEncoder, profile.AudioEncoding.BitrateBPS = encoder, bitrate
	// err сообщает о непригодной полной настройке тестового профиля.
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	return profile
}

// bindingAudio создаёт фактическую конфигурацию kind; t сообщает о неверных контрольных данных.
// Для FLAC STREAMINFO описывает фиксированный блок4608; MP3 получает четыре байта заголовка кадра.
func bindingAudio(t *testing.T, kind core.Codec) media.CodecConfig {
	t.Helper()
	// headers содержит нормативную запись без признака происхождения или начального исключения.
	headers := []byte{0x11, 0x90}
	switch kind {
	case core.CodecOpus:
		headers = append([]byte("OpusHead"), 1, 2, 0, 0, 0x80, 0xbb, 0, 0, 0, 0, 0)
	case core.CodecFLAC:
		headers = make([]byte, 34)
		binary.BigEndian.PutUint16(headers[:2], 4608)
		binary.BigEndian.PutUint16(headers[2:4], 4608)
		binary.BigEndian.PutUint64(headers[10:18], uint64(48000)<<44|uint64(1)<<41|uint64(15)<<36)
	case core.CodecMP3:
		headers = []byte{0xff, 0xfb, 0x94, 0}
	case core.CodecAC3:
		headers = []byte{0x10, 0x11, 0x40}
	}
	// actual восстанавливается из headers; err сообщает о неверной записи выбранного кодека.
	actual, err := codec.AudioFormatHeaders(kind, headers)
	if err != nil {
		t.Fatal(err)
	}
	// audio содержит неизменяемую конфигурацию общей эпохи50 со шкалой декодера48k.
	audio, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 50, Revision: 3, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: actual}, headers)
	if err != nil {
		t.Fatal(err)
	}
	return audio
}

// bindingFLACFrame возвращает фиксированный кадр7 на4608 отсчётов с двумя постоянными нулевыми подкадрами.
// CRC вычисляются независимо в тесте, сохраняя внешний номер7 исходного полного кадра.
func bindingFLACFrame() []byte {
	// frame содержит заголовок блока4608/48k/stereoPCM16 и номер7 перед CRC8.
	frame := []byte{0xff, 0xf8, 0x7a, 0x18, 7, 0x11, 0xff}
	// crc8 накапливает остаток полинома0x07; value перебирает байты заголовка.
	crc8 := byte(0)
	for _, value := range frame {
		crc8 ^= value
		for range 8 {
			if crc8&0x80 != 0 {
				crc8 = crc8<<1 ^ 7
			} else {
				crc8 <<= 1
			}
		}
	}
	frame = append(frame, crc8, 0, 0, 0, 0, 0, 0)
	// crc16 проверяет весь frame полиномом0x8005; value перебирает заголовок и оба подкадра.
	crc16 := uint16(0)
	for _, value := range frame {
		crc16 ^= uint16(value) << 8
		for range 8 {
			if crc16&0x8000 != 0 {
				crc16 = crc16<<1 ^ 0x8005
			} else {
				crc16 <<= 1
			}
		}
	}
	return append(frame, byte(crc16>>8), byte(crc16))
}

// TestGroupSourceAudioActualBoundary проверяет все текущие аудиопривязки по фактическим заголовкам.
// t проверяет также ложное описание, ненормализованный Opus и запрещённый FLAC copy.
func TestGroupSourceAudioActualBoundary(t *testing.T) {
	// kind перебирает все новые входы и полный прежний AAC LC.
	for _, kind := range []core.Codec{core.CodecAAC, core.CodecOpus, core.CodecFLAC, core.CodecMP3, core.CodecAC3} {
		// profile задаёт перекодирование в AAC, audio — фактическую входную конфигурацию каждого кодека.
		profile, audio := bindingProfile(t, core.CodecAAC), bindingAudio(t, kind)
		// err сообщает об отказе фактического входа текущего профиля.
		if err := validateSourceAudio(profile, audio); err != nil {
			t.Fatalf("audio input %s: %v", kind, err)
		}

	}
	// profile использует допустимое перекодирование AAC и прежний многоканальный вход44.1k/7.1.
	profile := bindingProfile(t, core.CodecAAC)
	// headers содержит фактический ASC44.1k/7.1wide, допустимый прежнему разборщику AAC.
	headers := []byte{0x12, 0x38}
	// actual и err определяют прежний вход AAC без требования48k/stereo.
	actual, err := codec.AudioFormatASC(headers)
	if err != nil {
		t.Fatal(err)
	}
	// audio содержит фактическое описание многоканального AAC.
	audio, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 44100}, Audio: actual}, headers)
	if err != nil || validateSourceAudio(profile, audio) != nil {
		t.Fatal("прежний полный AAC input сужен", err)
	}
	audio = bindingAudio(t, core.CodecOpus)
	headers = audio.Headers()
	headers[10] = 120
	audio, err = media.NewCodecConfig(audio.Info(), headers)
	if err != nil || validateSourceAudio(profile, audio) == nil {
		t.Fatal("необработанное начало Opus принято", err)
	}
	profile.LiveAudioMode = core.LiveAudioCopy
	if validateSourceAudio(profile, bindingAudio(t, core.CodecFLAC)) == nil {
		t.Fatal("FLAC copy input принят")
	}
}

// TestGroupFLACOwnershipAndPrivateInput проверяет доверие к выходу и отдельный физический входной поток.
// t подтверждает исходный номер7, заголовки, данные, время и отдельный счётчик каждой попытки.
func TestGroupFLACOwnershipAndPrivateInput(t *testing.T) {
	// fixture предоставляет бюджеты транспорта без запуска FFmpeg; video содержит фактическую конфигурацию AV1.
	fixture, video, _, _ := switcherFixture(t)
	// profile описывает нативный выход FLAC, audio — внешний вход без доверенного происхождения.
	profile, audio := bindingProfile(t, core.CodecFLAC), bindingAudio(t, core.CodecFLAC)
	// payload и packet сохраняют исходный кадр7 с положительным началом10s; err сообщает об отказе пакета.
	payload := bindingFLACFrame()
	packet, err := media.NewPacket(media.PacketInfo{PTS: 480000, DTS: 480000, DurationTicks: 4608}, audio, payload)
	if err != nil {
		t.Fatal(err)
	}
	// owned подтверждает доверенный собственный кодировщик; err сообщает об отказе actual проверки.
	owned, err := ownFLACOutput(audio, packet, profile, true)
	if err != nil || !owned.Info().FLACOwned || validateGroupAudio(profile, owned) != nil {
		t.Fatal("доверенный actual encoder не принят", err)
	}
	// unowned сохраняет конфигурацию при отсутствии подтверждения вызывающего компонента.
	unowned, err := ownFLACOutput(audio, packet, profile, false)
	if err != nil || unowned.Info().FLACOwned || validateGroupAudio(profile, unowned) == nil {
		t.Fatal("внешний FLAC получил output authority", err)
	}
	// badPacket сохраняет полную AU с недопустимой нулевой длительностью; packetErr сообщает об отказе структуры.
	badPacket, packetErr := media.NewPacket(media.PacketInfo{PTS: 480000, DTS: 480000}, audio, payload)
	if packetErr != nil {
		t.Fatal(packetErr)
	}
	if _, packetErr = ownFLACOutput(audio, badPacket, profile, true); packetErr == nil {
		t.Fatal("доверенный caller обошёл actual duration")
	}
	// limits ограничивает fMP4 в памяти, включая уточнение фактических сведений по первой AU.
	limits := fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 65536, MaxMdatBytes: 1 << 20, MaxSampleBytes: 65536, MaxExtradataBytes: 4096, MaxSamples: 100}
	// attempt повторяет независимые приватные мультиплексоры, каждый начинает счётчик отсчётов с0.
	for attempt := range 2 {
		// wire содержит приватный поток; mux не получает FLACOwned источника; muxErr сохраняет отказ.
		var wire bytes.Buffer
		mux, muxErr := newTranscodeInputMuxer(&wire, fmp4.Init{Video: video, Audio: audio}, limits, profile)
		if muxErr != nil {
			t.Fatal(muxErr)
		}
		if muxErr = mux.WritePacket(packet); muxErr != nil {
			t.Fatal(muxErr)
		}
		// demux читает фактический приватный поток; demuxErr сообщает об отказе его разбора.
		demux, demuxErr := fmp4.NewDemuxer(bytes.NewReader(wire.Bytes()), fmp4.DemuxOptions{Limits: limits})
		if demuxErr != nil {
			t.Fatal(demuxErr)
		}
		// output содержит прочитанную AU, readErr — ошибку чтения; frame и frameErr проверяют её счётчик.
		output, readErr := demux.Next()
		if readErr != nil {
			t.Fatal(readErr)
		}
		frame, frameErr := codec.FLACFrameInfo(output.Config().Headers(), output.Payload())
		if frameErr != nil || !frame.Variable || frame.Number != 0 || output.Info() != packet.Info() || output.Config().Info().FLACOwned {
			t.Fatalf("attempt%d: %+v %+v %v", attempt, frame, output.Info(), frameErr)
		}
	}
	if audio.Info().FLACOwned || !bytes.Equal(audio.Headers(), bindingAudio(t, core.CodecFLAC).Headers()) || !bytes.Equal(packet.Payload(), payload) || fixture.Status().Available {
		t.Fatal("private input изменил исходный source")
	}
}

// TestGroupOpusSwitcherKeepsFirstRetainedAU проверяет отсутствие второго исключения при положительном начале.
// t проверяет данные, время и длительность, а после завершения — отзыв источника и готовности.
func TestGroupOpusSwitcherKeepsFirstRetainedAU(t *testing.T) {
	// fixture предоставляет бюджеты переключателя; video содержит фактическую конфигурацию AV1.
	// now задаёт общую временную точку для подачи пакетов и последующего отказа источника.
	fixture, video, _, now := switcherFixture(t)
	// config задаёт новый выход Opus; фактическое видео берётся из проверенных заголовков AV1.
	config := fixture.config
	config.Profile.Encoding = core.DefaultEncoding(core.ModeCopy, core.VideoEncoderSVTAV1, core.RateControlCBR)
	config.Profile.LiveAudioMode = core.LiveAudioCopy
	config.Profile.Format.Audio = bindingAudio(t, core.CodecOpus).Info().Audio
	config.Profile.AudioEncoding.BitrateBPS = 128000
	config.Profile.Encoding.AudioEncoder = core.AudioEncoderOpus
	// sw содержит выходной Switcher; err сохраняет ошибки сборки и подписки.
	sw, err := NewSwitcher(config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// closeErr сообщает об отказе освобождения ресурса sw после проверки.
		if closeErr := sw.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	// sub читает фактический выход без повторного удаления начальных данных декодера.
	sub, err := sw.Router().Subscribe(config.Queue)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// closeErr сообщает об отказе освобождения ресурса sub после проверки.
		if closeErr := sub.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	// audio и payload содержат уже нормализованный Head0 и первую удержанную CELT120 AU.
	audio, payload := bindingAudio(t, core.CodecOpus), []byte{0xe4, 0xff, 0xfe}
	allowUnavailable(t, pushPacket(t, sw, Fallback, 1, video, video, audio, 480000, 480000, 1600, true, now))
	// packet начинает аудио ровно на PTS видео, поэтому общий срез не должен удалить его.
	packet, err := media.NewPacket(media.PacketInfo{PTS: 480000, DTS: 480000, DurationTicks: 120}, audio, payload)
	if err != nil {
		t.Fatal(err)
	}
	allowUnavailable(t, sw.Push(Fallback, 1, packet, video, audio, now))
	_ = next(t, sub)
	// retained содержит первую выходную AU на общем начале0 с исходными байтами и длительностью120.
	retained := next(t, sub)
	if !bytes.Equal(retained.Payload(), payload) || retained.Info().DTS != 0 || retained.Info().DurationTicks != 120 || !sw.Status().Available {
		t.Fatalf("первая удержанная Opus AU потеряна: %+v", retained.Info())
	}
	if err = sw.Fail(Fallback, 1, errors.New("конец attempt"), now.Add(time.Millisecond)); !errors.Is(err, ErrUnavailable) || sw.Status().Available {
		t.Fatal("готовность завершённой attempt не отозвана", err)
	}
	// ctx ограничивает ожидание уже отозванного источника; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err = sub.Next(ctx); err == nil {
		t.Fatal("после отзыва выдан устаревший пакет")
	}
}

// TestGroupRejectsSourceBeforeCustomTranscodeBuilder проверяет ранний отказ без создания FFmpeg-роли.
// t подаёт фактическую конфигурацию через Router; nil observer допустим благодаря раннему отказу.
func TestGroupRejectsSourceBeforeCustomTranscodeBuilder(t *testing.T) {
	// scenario перебирает ненормализованный Opus и ложные фактические заголовки.
	for _, scenario := range []string{"opus-start", "false-actual", "flac-copy", "h264-input"} {
		// fixture содержит бюджеты очереди и маршрутизатора; video содержит фактическую конфигурацию AV1.
		fixture, video, _, _ := switcherFixture(t)
		// profile задаёт перекодирование нового аудио, audio — фактический вход Opus.
		profile, audio := bindingProfile(t, core.CodecAAC), bindingAudio(t, core.CodecOpus)
		// headers и info получают отдельные копии исходной конфигурации для одного ошибочного случая.
		headers, info := audio.Headers(), audio.Info()
		switch scenario {
		case "opus-start":
			headers[10] = 120
		case "false-actual":
			info.Audio.CodecProfile = "unknown"
		case "flac-copy":
			audio = bindingAudio(t, core.CodecFLAC)
			headers, info = audio.Headers(), audio.Info()
			profile.LiveAudioMode = core.LiveAudioCopy
		case "h264-input":
			video, _ = switchAcceptanceConfigs(t, 50, 2)
		}
		// live получает источник до перекодирования; err сохраняет ошибки настройки и результат.
		live, err := media.NewRouter(fixture.config.Router)
		if err != nil {
			t.Fatal(err)
		}
		// source задаёт действующую эпоху Router; конфигурации и пакеты используют только её.
		source, err := live.BeginSource()
		if err != nil {
			t.Fatal(err)
		}
		info.SourceEpoch = source.Epoch()
		audio, err = media.NewCodecConfig(info, headers)
		if err != nil {
			t.Fatal(err)
		}
		// videoInfo переносит фактическое видео в новую эпоху, сохраняя заголовки и временную базу.
		videoInfo := video.Info()
		videoInfo.SourceEpoch = source.Epoch()
		video, err = media.NewCodecConfig(videoInfo, video.Headers())
		if err != nil {
			t.Fatal(err)
		}
		if err = source.UpdateConfigs(video, audio); err != nil {
			t.Fatal(err)
		}
		// sub удерживает первую актуальную выдачу для прямого вызова transcode.
		sub, err := live.Subscribe(fixture.config.Queue)
		if err != nil {
			t.Fatal(err)
		}
		// packet нужен для существующего допуска независимого начала в Router.
		packet, err := media.NewPacket(media.PacketInfo{Keyframe: true, DurationTicks: 1600}, video, av1TU(true))
		if err != nil {
			t.Fatal(err)
		}
		if err = source.Publish(packet); err != nil {
			t.Fatal(err)
		}
		// g имеет nil observer: попытка открыть роль до отказа вызвала бы panic.
		g := &Group{config: RuntimeConfig{Switcher: fixture.config}}
		g.config.Switcher.Profile = profile
		// called обнаруживает недопустимое достижение пользовательского построителя.
		called := false
		g.config.Transcode = func(context.Context, ffmpeg.NVIDIAConfig, ffmpeg.Attempt) (ffmpeg.Command, error) {
			// Любой вызов после отказа исходных настроек нарушает раннюю проверку.
			called = true
			return ffmpeg.Command{}, errors.New("построитель не должен вызываться")
		}
		// ctx ограничивает ожидание ошибочного источника; cancel освобождает таймер.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = g.transcode(ctx, sub, 1)
		cancel()
		_ = sub.Close()
		_ = live.Close()
		if err == nil || called {
			t.Fatalf("scenario=%s err=%v called=%t", scenario, err, called)
		}
	}
}

// TestGroupAAC71GeneratedHookRequiresAACBinding подтверждает допуск только полного собственного AAC LC.
// t проверяет фактическое нативное преобразование ASC→config12 без допуска кодировщика Opus.
func TestGroupAAC71GeneratedHookRequiresAACBinding(t *testing.T) {
	// profile содержит полный рецепт AAC LC7.1 с перекодированием LIVE.
	profile := bindingProfile(t, core.CodecAAC)
	profile.Format.Audio.Channels, profile.Format.Audio.ChannelLayout = 8, "7.1"
	// normalize является собственным обработчиком AAC; raw сохраняет нативный channel_configuration7.
	normalize := generatedLiveHeaders(profile)
	raw := []byte{0x11, 0xb8, 0x56, 0xe5, 0}
	if normalize == nil {
		t.Fatal("текущий профиль потерял обработчик AAC7.1")
	}
	// actual и err описывают исправленный ASC собственной попытки.
	actual, err := normalize(fmp4.TrackDescription{Track: media.TrackAudio, Codec: core.CodecAAC, Headers: raw})
	if err != nil || !bytes.Equal(actual, []byte{0x11, 0xe0, 0x56, 0xe5, 0}) || raw[1] != 0xb8 {
		t.Fatal(actual, err)
	}
	if generatedLiveHeaders(bindingProfile(t, core.CodecOpus)) != nil {
		t.Fatal("Opus получил нормализатор AAC")
	}
}
