package rtmp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/amf0"
)

// audioBindingOutbound создаёт для t реальный сериализатор над изолированным транспортом в памяти.
// kind выбирает конечный кодек; FLACOwned имитирует проверенное локальное происхождение тестовых данных.
// Возвращаются соединение, транспорт, конфигурации видео/аудио и первая AU с положительным временем.
func audioBindingOutbound(t *testing.T, kind core.Codec) (*OutboundConn, *transportSelfMemory, media.CodecConfig, media.CodecConfig, media.Packet) {
	t.Helper()
	// headers, payload и samples сохраняют независимые заголовки, сжатые данные и длительность в отсчётах декодера.
	headers, payload, samples := audioBindingFixture(t, kind)
	// format и err подтверждают фактические заголовки без подстановки желаемого формата.
	format, err := codec.AudioFormatHeaders(kind, headers)
	if err != nil {
		t.Fatal(err)
	}
	// video предоставляет прежний независимый H.264 старт в точной48k шкале.
	video, _ := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 48000})
	// audio связывает фактические заголовки с тем же источником; err сохраняет отказ конфигурации.
	audio, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: video.Info().TimeBase, Audio: format, FLACOwned: kind == core.CodecFLAC}, headers)
	if err != nil {
		t.Fatal(err)
	}
	// memory и conn используют прежние допуски, ACK, счётчики и сериализатор тестового транспорта.
	memory := &transportSelfMemory{}
	conn := transportSelfConn(t, memory)
	conn.target, err = ResolveOutbound("rtmp://example.test/app", "key", core.MediaFormat{Video: video.Info().Video, Audio: format})
	if err != nil {
		t.Fatal(err)
	}
	// packet содержит AU на+10с с точной положительной длительностью; err сохраняет отказ структуры.
	packet, err := media.NewPacket(media.PacketInfo{PTS: 480000, DTS: 480000, DurationTicks: int64(samples)}, audio, payload)
	if err != nil {
		t.Fatal(err)
	}
	return conn, memory, video, audio, packet
}

// TestAudioBindingOutboundLiteralWireAndMetadata проверяет исходные сетевые байты всех четырёх кодеков.
// t сравнивает числовые метаданные UI32, точные байты SequenceStart и AU без сериализации SDK.
func TestAudioBindingOutboundLiteralWireAndMetadata(t *testing.T) {
	// expected задаёт независимые нормативные числа FourCC, которым значение AMF должно равняться точно.
	expected := map[core.Codec]float64{core.CodecOpus: 1332770163, core.CodecFLAC: 1716281667, core.CodecMP3: 778924083, core.CodecAC3: 1633889587}
	// kind и code выбирают фактическую привязку и независимое числовое ожидание.
	for kind, code := range expected {
		// conn/memory/video/audio/packet содержат изолированное соединение, транспорт, конфигурации и AU штатного исходящего пути.
		conn, memory, video, audio, packet := audioBindingOutbound(t, kind)
		// err сохраняет отказ локальной проверки назначения и его бюджетов.
		if err := conn.target.Validate(conn.target.profile, conn.opts.Limits); err != nil {
			t.Fatal(err)
		}
		// err сохраняет отказ проверки и регистрации заголовков до сетевой записи.
		if err := conn.WriteHeaders(context.Background(), video, audio); err != nil {
			t.Fatal(err)
		}
		// err сохраняет результат полной записи проверенного пакета штатным сериализатором.
		if _, err := conn.WritePacket(context.Background(), transportSelfPacket(t, video, 480000, 480000, true)); err != nil {
			t.Fatal(err)
		}
		// result подтверждает исходный размер нагрузки; err сохраняет отказ отправки AU.
		result, err := conn.WritePacket(context.Background(), packet)
		if err != nil || result.PayloadBytes != uint64(len(packet.Payload())) {
			t.Fatalf("%s: result=%+v err=%v", kind, result, err)
		}
		// input читает записанные сообщения; metadata, start и coded выбираются по прежнему порядку.
		input := bytes.NewReader(memory.output.Bytes())
		// metadata содержит первое записанное сообщение RTMP, err сообщает об отказе его обрамления.
		metadata, err := transportSelfReadFrame(input, 128)
		if err != nil {
			t.Fatal(err)
		}
		// values и object содержат разобранные метаданные, err сохраняет отказ AMF.
		// actual и ok проверяют числовой тип и точное UI32 значение.
		values, err := amf0.Unmarshal(metadata.body)
		if err != nil {
			t.Fatal(err)
		}
		object, ok := values[2].(amf0.ECMAArray)
		if !ok {
			t.Fatalf("неверная metadata форма: %T", values[2])
		}
		actual, ok := amf0.Object(object).GetFloat64("audiocodecid")
		if !ok || actual != code {
			t.Fatalf("%s: audiocodecid=%v/%t", kind, actual, ok)
		}
		if _, err = transportSelfReadFrame(input, 128); err != nil {
			t.Fatal(err)
		}
		// start содержит третье сообщение, Audio SequenceStart; err сохраняет отказ чтения обрамления.
		start, err := transportSelfReadFrame(input, 128)
		if err != nil {
			t.Fatal(err)
		}
		// want содержит независимый внешний префикс; тело MP3/AC3 остаётся пустым.
		want := append([]byte{0x90}, []byte(outboundAudioFourCC(kind))...)
		switch kind {
		case core.CodecOpus:
			want = append(want, audioBindingHex(t, "4f707573486561640102000044ac0000000000")...)
		case core.CodecFLAC:
			want = append(want, audioBindingHex(t, "664c614380000022003012000000000000000bb802f00000000000000000000000000000000000000000")...)
		}
		if start.typ != 8 || !bytes.Equal(start.body, want) {
			t.Fatalf("%s: SequenceStart=%x want=%x", kind, start.body, want)
		}
		if _, err = transportSelfReadFrame(input, 128); err != nil {
			t.Fatal(err)
		}
		// coded содержит последнюю аудио AU; err сообщает об отказе независимого разбора RTMP.
		coded, err := transportSelfReadFrame(input, 128)
		if err != nil || coded.typ != 8 || coded.stamp != 0 || !bytes.Equal(coded.body[:5], append([]byte{0x91}, []byte(outboundAudioFourCC(kind))...)) {
			t.Fatalf("%s: CodedFrames=%+v err=%v", kind, coded, err)
		}
		if kind != core.CodecFLAC && !bytes.Equal(coded.body[5:], packet.Payload()) {
			t.Fatal("copy payload изменён")
		}
		// connect/advertised проверяют только применимый новый аудио FourCC без иных возможностей.
		connect := outboundConnectData(conn.target)
		advertised, _ := connect[2].(amf0.Object).Get("fourCcList")
		if !reflect.DeepEqual(advertised, amf0.StrictArray{"av01", outboundAudioFourCC(kind)}) {
			t.Fatalf("неверная capability: %+v", advertised)
		}
	}
}

// TestAudioBindingFLACPhysicalCounterAcrossHeaders проверяет независимые физические потоки.
// t сравнивает счётчики0/4608 после повтора заголовков и начало0 у позднего соединения.
func TestAudioBindingFLACPhysicalCounterAcrossHeaders(t *testing.T) {
	// conn/memory/video/audio/packet задают фиксированный кадр с номером7 и конфигурацию с подтверждением локального происхождения.
	conn, memory, video, audio, packet := audioBindingOutbound(t, core.CodecFLAC)
	// original сохраняет исходный кадр для сравнения байтов источника и сжатого тела.
	original := packet.Payload()
	// number задаёт ожидаемый счётчик на очередной записи одного физического соединения.
	for _, number := range []uint64{0, 4608} {
		memory.output.Reset()
		// err сохраняет отказ проверки и регистрации заголовков до сетевой записи.
		if err := conn.WriteHeaders(context.Background(), video, audio); err != nil {
			t.Fatal(err)
		}
		// err сохраняет результат полной записи проверенного пакета штатным сериализатором.
		if _, err := conn.WritePacket(context.Background(), transportSelfPacket(t, video, 480000+int64(number)*10, 480000+int64(number)*10, true)); err != nil {
			t.Fatal(err)
		}
		// next сохраняет исходные метки при повторе заголовков; err сообщает об отказе структуры пакета.
		next, err := media.NewPacket(media.PacketInfo{PTS: 480000 + int64(number)*10, DTS: 480000 + int64(number)*10, DurationTicks: 4608}, audio, original)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = conn.WritePacket(context.Background(), next); err != nil {
			t.Fatal(err)
		}
		// input и coded извлекают последнюю AU после метаданных, заголовков и видео.
		input := bytes.NewReader(memory.output.Bytes())
		var coded transportSelfFrame
		for range 5 {
			coded, err = transportSelfReadFrame(input, 128)
			if err != nil {
				t.Fatal(err)
			}
		}
		// frame и err проверяют CRC и счётчик выходной AU; sourceFrame и sourceErr разбирают исходную AU.
		// Их границы позволяют сравнить оба сжатых подкадра без заголовков и CRC.
		frame, err := codec.FLACFrameInfo(audio.Headers(), coded.body[5:])
		sourceFrame, sourceErr := codec.FLACFrameInfo(audio.Headers(), original)
		if err != nil || sourceErr != nil || !frame.Variable || frame.Number != number || conn.flacSamples != number+4608 || !bytes.Equal(coded.body[5+frame.HeaderBytes:len(coded.body)-2], original[sourceFrame.HeaderBytes:len(original)-2]) || !bytes.Equal(packet.Payload(), original) {
			t.Fatalf("FLAC physical stream: frame=%+v counter=%d err=%v/%v", frame, conn.flacSamples, err, sourceErr)
		}
	}
	// late начинает отдельное соединение; lateVideo/lateAudio и latePacket сохраняют его конфигурации и AU.
	late, _, lateVideo, lateAudio, latePacket := audioBindingOutbound(t, core.CodecFLAC)
	// err сохраняет отказ проверки и регистрации заголовков до сетевой записи.
	if err := late.WriteHeaders(context.Background(), lateVideo, lateAudio); err != nil {
		t.Fatal(err)
	}
	// err сохраняет результат полной записи проверенного пакета штатным сериализатором.
	if _, err := late.WritePacket(context.Background(), transportSelfPacket(t, lateVideo, 480000, 480000, true)); err != nil {
		t.Fatal(err)
	}
	// err сохраняет результат полной записи проверенного пакета штатным сериализатором.
	if _, err := late.WritePacket(context.Background(), latePacket); err != nil || late.flacSamples != 4608 {
		t.Fatal("late connection не начал свой FLAC stream0", err)
	}
}

// TestAudioBindingFLACRejectsUnownedOverflowAndPartial проверяет право перезаписи и сохранение состояния после полной записи.
// t убеждается, что фактические заголовки без FLACOwned не разрешают изменение выходных данных.
func TestAudioBindingFLACRejectsUnownedOverflowAndPartial(t *testing.T) {
	// conn, memory, video и audio задают проверенный FLAC-выход перед подменой внешней конфигурацией.
	conn, memory, video, audio, _ := audioBindingOutbound(t, core.CodecFLAC)
	// info и external сохраняют те же байты, но убирают подтверждение локального происхождения.
	info := audio.Info()
	info.FLACOwned = false
	// external и err проверяют структурно допустимую внешнюю конфигурацию без права перезаписи.
	external, err := media.NewCodecConfig(info, audio.Headers())
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.WriteHeaders(context.Background(), video, external); err == nil || memory.output.Len() != 0 {
		t.Fatal("внешний FLAC получил право output rewrite")
	}
	// mode выбирает переполнение, бюджет либо частичный отказ после допустимых заголовков и видео.
	for _, mode := range []string{"overflow", "partial", "packet-budget"} {
		// conn, memory, video, audio и packet создают отдельный транспорт и AU для текущего вида отказа.
		conn, memory, video, audio, packet := audioBindingOutbound(t, core.CodecFLAC)
		if err = conn.WriteHeaders(context.Background(), video, audio); err != nil {
			t.Fatal(err)
		}
		if _, err = conn.WritePacket(context.Background(), transportSelfPacket(t, video, 480000, 480000, true)); err != nil {
			t.Fatal(err)
		}
		memory.output.Reset()
		switch mode {
		case "overflow":
			conn.flacSamples = (1 << 36) - 4608
		case "packet-budget":
			conn.flacSamples = 1 << 30
			conn.opts.Limits.MaxMessageBytes = uint32(len(packet.Payload()) + 5)
		default:
			memory.failure, memory.short = io.ErrClosedPipe, 2
		}
		// before и clock сохраняют состояние до неуспешной отправки.
		before, clock := conn.flacSamples, conn.clock
		if _, err = conn.WritePacket(context.Background(), packet); err == nil || conn.flacSamples != before || conn.clock != clock || !memory.closed {
			t.Fatalf("%s: invalid commit counter=%d/%d closed=%t err=%v", mode, before, conn.flacSamples, memory.closed, err)
		}
		if mode != "partial" && memory.output.Len() != 0 || mode == "partial" && (memory.output.Len() != 2 || !errors.Is(err, io.ErrClosedPipe)) {
			t.Fatalf("%s: неправильная граница I/O bytes=%d err=%v", mode, memory.output.Len(), err)
		}
	}
}

// TestAudioBindingOutboundRejectsUnnormalizedOpusAndUnknownDuration проверяет границу исходного адаптера.
// t подтверждает, что отправитель не удаляет AU сам и не подставляет желаемую длительность.
func TestAudioBindingOutboundRejectsUnnormalizedOpusAndUnknownDuration(t *testing.T) {
	// conn/memory/video/audio/packet задают нормализованную Opus последовательность в точной шкале.
	conn, memory, video, audio, packet := audioBindingOutbound(t, core.CodecOpus)
	// headers сохраняют отдельную копию Head с ещё не обработанным начальным исключением120.
	headers := audio.Headers()
	headers[10] = 120
	// unnormalized и err создают структурно допустимый Head, который отправитель обязан отклонить.
	unnormalized, err := media.NewCodecConfig(audio.Info(), headers)
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.WriteHeaders(context.Background(), video, unnormalized); err == nil || memory.output.Len() != 0 {
		t.Fatal("sender принял необработанный initial skip")
	}
	if err = conn.WriteHeaders(context.Background(), video, audio); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.WritePacket(context.Background(), transportSelfPacket(t, video, 480000, 480000, true)); err != nil {
		t.Fatal(err)
	}
	memory.output.Reset()
	// info сохраняет исходное время AU, но удаляет сведения о её длительности.
	info := packet.Info()
	info.DurationTicks = 0
	// unknown и err подтверждают, что общий Packet по-прежнему разрешает нулевую длительность.
	unknown, err := media.NewPacket(info, audio, packet.Payload())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.WritePacket(context.Background(), unknown); err == nil || memory.output.Len() != 0 {
		t.Fatal("новый codec получил фиктивную длительность при отправке")
	}
}

// TestAudioBindingStaleDeliveryDoesNotWrite проверяет неизменяемую Delivery до любой новой записи FLAC.
// t отзывает реальный Source, а не подменяет проверку актуальности тестовой функцией.
func TestAudioBindingStaleDeliveryDoesNotWrite(t *testing.T) {
	// conn/memory/video/audio/packet задают штатный отправитель и фактическую FLAC AU с локальным происхождением.
	conn, memory, video, audio, packet := audioBindingOutbound(t, core.CodecFLAC)
	// publication/sub создают настоящий Router с отзывом ранее выданной Delivery.
	publication, sub := newContractPublication(t)
	// err сообщает об отказе установки проверенной пары конфигураций источника.
	if err := publication.source.UpdateConfigs(video, audio); err != nil {
		t.Fatal(err)
	}
	// err сохраняет результат публикации пакета в реальном Router.
	if err := publication.source.Publish(transportSelfPacket(t, video, 480000, 480000, true)); err != nil {
		t.Fatal(err)
	}
	// err сохраняет результат публикации пакета в реальном Router.
	if err := publication.source.Publish(packet); err != nil {
		t.Fatal(err)
	}
	_ = delivery(t, sub)
	// stale содержит настоящую доставку исходного источника до его отзыва.
	stale := delivery(t, sub)
	// err сообщает об отказе создания нового источника, отзывающего прежние доставки.
	if _, err := publication.router.BeginSource(); err != nil {
		t.Fatal(err)
	}
	// err сохраняет отказ проверки и регистрации заголовков до сетевой записи.
	if err := conn.WriteHeaders(context.Background(), video, audio); err != nil {
		t.Fatal(err)
	}
	// err должен сообщить об отзыве доставки до начала записи.
	if _, err := conn.WriteDelivery(context.Background(), stale); !errors.Is(err, media.ErrStaleSource) || memory.output.Len() != 0 || memory.closed || conn.flacSamples != 0 {
		t.Fatalf("stale Delivery вызвала I/O либо закрытие: %v", err)
	}
}
