package rtmp

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// audioBindingRaw оборачивает body в независимое аудиосообщение fmt0/3 с абсолютной меткой at.
// Чанки по128байт соответствуют размеру SDK и допускают полные MP3/AC3 AU.
func audioBindingRaw(at uint32, body []byte) []byte {
	// raw содержит полное сообщение, чей заголовок получает заданную миллисекундную метку.
	raw := fragmented(4, 8, body, 128)
	raw[1], raw[2], raw[3] = byte(at>>16), byte(at>>8), byte(at)
	return raw
}

// audioBindingStart строит исходные сетевые байты SequenceStart кодека kind для t.
// skip120 выбирает начальное исключение Opus; MP3/AC3 имеют пустое тело после FourCC.
func audioBindingStart(t *testing.T, kind core.Codec, skip120 bool) []byte {
	t.Helper()
	// headers содержит заголовки медиатракта, body начинается с точного внешнего FourCC.
	headers, _, _ := audioBindingFixture(t, kind)
	body := append([]byte{0x90}, []byte(outboundAudioFourCC(kind))...)
	switch kind {
	case core.CodecOpus:
		if skip120 {
			headers[10] = 120
		}
	case core.CodecFLAC:
		body = append(body, 'f', 'L', 'a', 'C', 0x80, 0, 0, 34)
	case core.CodecMP3, core.CodecAC3:
		headers = nil
	}
	return audioBindingRaw(0, append(body, headers...))
}

// audioBindingCoded строит исходные байты CodedFrames ровно одной au кодека kind с меткой at.
func audioBindingCoded(kind core.Codec, at uint32, au []byte) []byte {
	// body сохраняет внешний префикс Enhanced и неизменную исходную AU.
	body := append([]byte{0x91}, []byte(outboundAudioFourCC(kind))...)
	return audioBindingRaw(at, append(body, au...))
}

// audioBindingConsume проводит raw через тот же входной адаптер и SDK, что серверная публикация p.
// t создаёт ограничитель; функция возвращает первую ошибку, включая повреждённую AU до готовности видео.
func audioBindingConsume(t *testing.T, p *publication, raw []byte) error {
	t.Helper()
	// wire и sdk создают изолированный транспорт без потери состояния самой publication.
	wire := guarded(t, raw, testLimits())
	sdk := audioBindingSDKReader(wire)
	p.wire = wire
	for {
		// msg и err сохраняют фактическое завершение чтения и адаптации одного сообщения.
		msg, err := wire.readMessage(sdk.Read)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = p.message(msg); err != nil {
			return err
		}
	}
}

// TestAudioBindingIngestActualCompletionAndTiming проверяет каждый новый кодек через исходные входные байты.
// t подтверждает уточнение до готовности видео, непустые заголовки и неизменённую следующую AU на+10с.
func TestAudioBindingIngestActualCompletionAndTiming(t *testing.T) {
	// kind выбирает конечную однодорожечную привязку Enhanced Audio.
	for _, kind := range []core.Codec{core.CodecOpus, core.CodecFLAC, core.CodecMP3, core.CodecAC3} {
		// p и sub получают собственный Router; headers, au и samples задают независимые данные кодека.
		p, sub := newContractPublication(t)
		headers, au, samples := audioBindingFixture(t, kind)
		// config задаёт независимое видео с точной собственной частотой30000/1001.
		config := contractAV1Config(contractSDRSequence())
		// raw уточняет конфигурацию до готовности видео, затем передаёт видеостарт и аудио AU на+10с.
		raw := bytes.Join([][]byte{audioBindingStart(t, kind, false), audioBindingCoded(kind, 10000, au), contractVideoConfig(config), contractVideo(10000, contractAV1TU(config)), audioBindingCoded(kind, 10000, au)}, nil)
		// err сохраняет первую ошибку реального входного разбора и обработки публикации.
		if err := audioBindingConsume(t, p, raw); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		// video и audio содержат фактические доставки после готовности; info задаёт неизменённое время.
		video, audio := delivery(t, sub), delivery(t, sub)
		info := audio.Packet.Info()
		if video.Packet.Config().Info().Track != media.TrackVideo || !bytes.Equal(audio.AudioConfig.Headers(), headers) || !bytes.Equal(audio.Packet.Payload(), au) || audio.AudioConfig.Info().FLACOwned || info.PTS != info.DTS || info.Keyframe {
			t.Fatalf("%s: изменён config/AU: %+v", kind, info)
		}
		// base задаёт точную общую шкалу, actualDuration выражает samples без округления к мс.
		base := audio.AudioConfig.Info().TimeBase
		actualDuration := uint64(info.DurationTicks) * uint64(base.Numerator) * 48000
		if actualDuration != uint64(samples)*uint64(base.Denominator) || uint64(info.DTS)*uint64(base.Numerator) != 10*uint64(base.Denominator) {
			t.Fatalf("%s: потеряны samples или positive origin: %+v/%+v", kind, info, base)
		}
	}
}

// TestAudioBindingOpusInitialSkipBeforeVideoAndNoRepeat проверяет однократное потребление начальной AU120.
// t проверяет +10с, смену только видео, новую аудиопоследовательность и сохранение времени.
func TestAudioBindingOpusInitialSkipBeforeVideoAndNoRepeat(t *testing.T) {
	// p/sub содержат изолированную публикацию; au — полная CELT120, config — независимое видео.
	p, sub := newContractPublication(t)
	_, au, _ := audioBindingFixture(t, core.CodecOpus)
	config := contractAV1Config(contractSDRSequence())
	// raw сначала потребляет начальную AU120 до любого видео, затем передаёт следующую AU без удаления.
	raw := bytes.Join([][]byte{audioBindingStart(t, core.CodecOpus, true), audioBindingCoded(core.CodecOpus, 10000, au), contractVideoConfig(config), contractVideo(10003, contractAV1TU(config)), audioBindingCoded(core.CodecOpus, 10003, au)}, nil)
	// err сохраняет первую ошибку реального входного разбора и обработки публикации.
	if err := audioBindingConsume(t, p, raw); err != nil {
		t.Fatal(err)
	}
	_ = delivery(t, sub)
	// audio содержит именно следующую AU после видеостарта с нормализованным Head0.
	audio := delivery(t, sub)
	if !acceptanceWireTime(audio.Packet, 10003) || audio.AudioConfig.Headers()[10] != 0 || p.opusSkipPending {
		t.Fatal("initial skip не потреблён до video либо применён повторно")
	}
	// err сохраняет первую ошибку реального входного разбора и обработки публикации.
	if err := audioBindingConsume(t, p, bytes.Join([][]byte{contractVideoConfig(config), contractVideo(10033, contractAV1TU(config)), audioBindingCoded(core.CodecOpus, 10035, au)}, nil)); err != nil {
		t.Fatal(err)
	}
	_ = delivery(t, sub)
	audio = delivery(t, sub)
	if !acceptanceWireTime(audio.Packet, 10035) || !bytes.Equal(audio.Packet.Payload(), au) {
		t.Fatal("video-only смена повторно потребила Opus AU")
	}
	// err сохраняет первую ошибку реального входного разбора и обработки публикации.
	if err := audioBindingConsume(t, p, bytes.Join([][]byte{audioBindingStart(t, core.CodecOpus, true), audioBindingCoded(core.CodecOpus, 10040, au), contractVideo(10043, contractAV1TU(config)), audioBindingCoded(core.CodecOpus, 10043, au)}, nil)); err != nil {
		t.Fatal(err)
	}
	_ = delivery(t, sub)
	audio = delivery(t, sub)
	if !acceptanceWireTime(audio.Packet, 10043) || p.opusSkipPending {
		t.Fatal("настоящий audio SequenceStart не создал новый initial skip")
	}
}

// TestAudioBindingMalformedBeforeVideoFails проверяет, что ожидание видео не скрывает повреждённую AU.
// t также исключает незаявленную смену кодека и проверяет SequenceEnd прежнего общего пути.
func TestAudioBindingMalformedBeforeVideoFails(t *testing.T) {
	// kind перебирает четыре новых проверки фактической AU до любого видеопакета.
	for _, kind := range []core.Codec{core.CodecOpus, core.CodecFLAC, core.CodecMP3, core.CodecAC3} {
		// p получает новый источник; invalid содержит неполную AU, не пригодную для уточнения формата.
		p, _ := newContractPublication(t)
		invalid := []byte{0}
		// err сохраняет первую ошибку реального входного разбора и обработки публикации.
		if err := audioBindingConsume(t, p, append(audioBindingStart(t, kind, true), audioBindingCoded(kind, 10000, invalid)...)); err == nil {
			t.Fatalf("%s: pre-ready malformed AU скрыта", kind)
		}
	}
	// p хранит активную последовательность Opus; неверный FourCC не может подменить её кодек.
	p, _ := newContractPublication(t)
	// err сохраняет первую ошибку реального входного разбора и обработки публикации.
	if err := audioBindingConsume(t, p, audioBindingStart(t, core.CodecOpus, false)); err != nil {
		t.Fatal(err)
	}
	// err сохраняет результат обработки конкретной формы RTMP текущей публикацией.
	if err := p.message(&message.AudioExCodedFrames{FourCC: message.FourCCMP3, DTS: time.Second, Payload: []byte{0xff}}); err == nil {
		t.Fatal("codec switch без SequenceStart принят")
	}
	// err сохраняет результат обработки конкретной формы RTMP текущей публикацией.
	if err := p.message(&message.AudioExSequenceEnd{FourCC: message.FourCCOpus}); !errors.Is(err, ErrUnready) {
		t.Fatal("SequenceEnd не завершил sequence", err)
	}
}
