package rtmp

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/bluenviron/gortmplib/pkg/amf0"
)

// selfColorInfo содержит полный сетевой colorInfo без неизвестных чисел или неявных значений по умолчанию.
// maxCLL выбирает различимые снимки при одинаковых заголовках кодека.
func selfColorInfo(maxCLL float64) amf0.Object {
	return amf0.Object{
		{Key: "colorConfig", Value: amf0.Object{{Key: "bitDepth", Value: float64(10)}, {Key: "colorPrimaries", Value: float64(9)}, {Key: "transferCharacteristics", Value: float64(16)}, {Key: "matrixCoefficients", Value: float64(9)}}},
		{Key: "hdrCll", Value: amf0.Object{{Key: "maxCLL", Value: maxCLL}, {Key: "maxFall", Value: float64(400)}}},
		{Key: "hdrMdcv", Value: amf0.Object{{Key: "redX", Value: 0.7080}, {Key: "redY", Value: 0.2920}, {Key: "greenX", Value: 0.1700}, {Key: "greenY", Value: 0.7970}, {Key: "blueX", Value: 0.1310}, {Key: "blueY", Value: 0.0460}, {Key: "whitePointX", Value: 0.3127}, {Key: "whitePointY", Value: 0.3290}, {Key: "maxLuminance", Value: float64(1000)}, {Key: "minLuminance", Value: 0.0050}}},
	}
}

// TestColorInfoReplacementResetAndValidation проверяет полную замену, сброс и конечные числа AMF.
func TestColorInfoReplacementResetAndValidation(t *testing.T) {
	// full содержит исходный полный объект; parsed сохраняет точные поля binary64 до проекции.
	// err сообщает об отказе разбора и последующих изменений метаданных публикации.
	full := selfColorInfo(1000)
	parsed, err := parseColorOverride(full)
	if err != nil || !parsed.hdr.MDCV.FullyKnown() || !parsed.hdr.CLL.FullyKnown() {
		t.Fatalf("Полный объект отклонён: %+v %v", parsed, err)
	}
	// publication принимает смены colorInfo без смешивания с предыдущим полным объектом.
	publication := &publication{}
	if err = publication.videoMetadata(amf0.Data{"colorInfo", full}); err != nil {
		t.Fatal(err)
	}
	if err = publication.videoMetadata(amf0.Data{"colorInfo", amf0.Object{{Key: "hdrCll", Value: amf0.Object{{Key: "maxCLL", Value: float64(500)}, {Key: "maxFall", Value: float64(200)}}}}}); err != nil || publication.override.hdr.MDCV.Present || publication.override.known[0] {
		t.Fatalf("Старые поля подмешаны в новый объект: %+v %v", publication.override, err)
	}
	if err = publication.videoMetadata(amf0.Data{"colorInfo", amf0.Undefined{}}); err != nil || publication.override.active {
		t.Fatalf("Undefined не снял override: %v", err)
	}
	// bad выбирает NaN/Inf, неверный диапазон, строковый индекс и неполную секцию.
	for _, bad := range []amf0.Object{
		{{Key: "hdrCll", Value: amf0.Object{{Key: "maxCLL", Value: math.NaN()}, {Key: "maxFall", Value: float64(1)}}}},
		{{Key: "hdrCll", Value: amf0.Object{{Key: "maxCLL", Value: math.Inf(1)}, {Key: "maxFall", Value: float64(1)}}}},
		{{Key: "hdrCll", Value: amf0.Object{{Key: "maxCLL", Value: float64(0)}, {Key: "maxFall", Value: float64(1)}}}},
		{{Key: "hdrCll", Value: amf0.Object{{Key: "maxCLL", Value: float64(1000)}}}},
		{{Key: "colorConfig", Value: amf0.Object{{Key: "bitDepth", Value: "10"}}}},
		{{Key: "colorConfig", Value: amf0.Object{{Key: "colorPrimaries", Value: 256.0}}}},
	} {
		if _, err = parseColorOverride(bad); err == nil {
			t.Fatalf("Принят неподдержанный colorInfo: %+v", bad)
		}
	}
}

// TestHDRMetadataBoundaryAndNativeOrder проверяет отзыв выдачи при изменении одних метаданных
// и порядок colorInfo→SequenceStart→независимый AU в реальном сериализаторе без сетевого подключения.
func TestHDRMetadataBoundaryAndNativeOrder(t *testing.T) {
	// err сохраняет последовательные отказы настройки и передачи данного изолированного потока.
	var err error
	// sequence и config задают настоящий синтаксис AV1 main10/PQ/BT.2020 с точным timing_info.
	sequence := contractSDRSequence()
	sequence.highDepth, sequence.primaries, sequence.transfer, sequence.matrix = true, 9, 16, 9
	config := contractAV1Config(sequence)
	// incoming и sub предоставляют отдельный Router и текущую подписку.
	incoming, sub := newContractPublication(t)
	// wire сохраняет исходный av1C ровно как реальный завершённый SequenceStart.
	incoming.wire = &wireConn{lastConfig: append([]byte{0x90, 'a', 'v', '0', '1'}, config...)}
	if err = incoming.videoConfig(0x61763031); err != nil {
		t.Fatal(err)
	}
	incoming.wire.lastConfig = []byte{0xaf, 0, 0x11, 0x90}
	if err = incoming.audioConfig(2); err != nil {
		t.Fatal(err)
	}
	if err = incoming.videoMetadata(amf0.Data{"colorInfo", selfColorInfo(1000)}); err != nil {
		t.Fatal(err)
	}
	incoming.wire.lastConfig = append([]byte{0x90, 'a', 'v', '0', '1'}, config...)
	if err = incoming.videoConfig(0x61763031); err != nil || !incoming.override.active {
		t.Fatalf("SequenceStart сбросил действующий override: %v", err)
	}
	if err = incoming.videoPacket(0, contractAV1TU(config)); err != nil {
		t.Fatal(err)
	}
	if err = incoming.audioPacket(0, []byte{0xbe, 0xef}); err != nil {
		t.Fatal(err)
	}
	// first содержит выдачу до изменения метаданных; pairVideo/pairAudio сохраняют исходные конфигурации.
	first := delivery(t, sub)
	_ = delivery(t, sub)
	pairVideo, pairAudio := first.VideoConfig, first.AudioConfig
	if !pairVideo.Info().HDR.CLL.Present {
		t.Fatal("Override не сохранён в CodecConfig")
	}
	if err = incoming.videoMetadata(amf0.Data{"colorInfo", selfColorInfo(500)}); err != nil || first.IsCurrent() {
		t.Fatalf("Старая выдача не отозвана до видео: %v", err)
	}
	if err = incoming.videoPacket(100*time.Millisecond, contractAV1TU(config)); err != nil {
		t.Fatal(err)
	}
	if err = incoming.audioPacket(100*time.Millisecond, []byte{0xbe, 0xef}); err != nil {
		t.Fatal(err)
	}
	// fresh содержит новый фактический снимок при неизменных исходных заголовках.
	fresh := delivery(t, sub)
	if fresh.VideoConfig.Info().HDR == pairVideo.Info().HDR || !bytes.Equal(fresh.VideoConfig.Headers(), config) {
		t.Fatal("Новая граница не сохранила исходные headers или HDR")
	}
	// memory и outbound используют штатный сериализатор, счётчики и проверку актуальности.
	memory := &transportSelfMemory{}
	outbound := transportSelfConn(t, memory)
	outbound.target.profile = core.MediaFormat{Video: pairVideo.Info().Video, Audio: pairAudio.Info().Audio}
	if err = outbound.WriteHeaders(context.Background(), pairVideo, pairAudio); err != nil {
		t.Fatal(err)
	}
	// err подтверждает отказ устаревшей выдаче до записи любых байтов.
	if _, err := outbound.WriteDelivery(context.Background(), first); !errors.Is(err, media.ErrStaleSource) || memory.output.Len() != 0 {
		t.Fatalf("Stale delivery записала bytes: %v", err)
	}
	if err = outbound.WriteHeaders(context.Background(), fresh.VideoConfig, fresh.AudioConfig); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе передачи актуального независимого AU.
	if _, err := outbound.WriteDelivery(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	// reader и color извлекают первое настоящее сообщение Metadata.
	reader := bytes.NewReader(memory.output.Bytes())
	color, err := transportSelfReadFrame(reader, 128)
	if err != nil || color.body[0]&15 != 4 {
		t.Fatalf("Первым должен идти colorInfo: %x %v", color.body, err)
	}
	// metadata содержит декодированные байты AMF для проверки полного нового снимка.
	metadata, err := amf0.Unmarshal(color.body[5:])
	if err != nil || len(metadata) != 2 || metadata[0] != "colorInfo" {
		t.Fatalf("Неверное сообщение colorInfo: %+v %v", metadata, err)
	}
	// restored показывает, что старые MaxCLL не пережили полную замену.
	restored, err := parseColorOverride(metadata[1])
	if err != nil || restored.hdr.CLL.MaxCLL.Raw != math.Float64bits(500) {
		t.Fatalf("Неверные metadata: %+v %v", restored, err)
	}
	// streamMetadata сохраняет прежнее отдельное onMetaData между HDR и заголовками дорожек.
	streamMetadata, err := transportSelfReadFrame(reader, 128)
	if err != nil || streamMetadata.typ != 18 {
		t.Fatalf("Потеряны обычные metadata: %+v %v", streamMetadata, err)
	}
	// videoStart и audioStart предшествуют независимому сжатому AU под тем же сериализатором.
	videoStart, err := transportSelfReadFrame(reader, 128)
	if err != nil || videoStart.body[0]&15 != 0 || !bytes.Equal(videoStart.body[5:], config) {
		t.Fatalf("Неверный SequenceStart: %x %v", videoStart.body, err)
	}
	audioStart, err := transportSelfReadFrame(reader, 128)
	if err != nil || !bytes.Equal(audioStart.body, []byte{0xaf, 0, 0x11, 0x90}) {
		t.Fatalf("AAC SequenceStart изменён: %x %v", audioStart.body, err)
	}
	// videoAU должен сохранить исходный TU и нормативный CodedFrames type1.
	videoAU, err := transportSelfReadFrame(reader, 128)
	if err != nil || videoAU.body[0]&15 != 1 || !bytes.Equal(videoAU.body[5:], fresh.Packet.Payload()) {
		t.Fatalf("AU изменён: %x %v", videoAU.body, err)
	}
	if err = incoming.videoMetadata(amf0.Data{"colorInfo", amf0.Undefined{}}); err != nil || fresh.IsCurrent() {
		t.Fatalf("Reset не отозвал прежнюю выдачу: %v", err)
	}
	if err = incoming.videoPacket(200*time.Millisecond, contractAV1TU(config)); err != nil {
		t.Fatal(err)
	}
	if err = incoming.audioPacket(200*time.Millisecond, []byte{0xbe, 0xef}); err != nil {
		t.Fatal(err)
	}
	// reset получает действующие сведения кодека без прежних статических блоков внешнего переопределения.
	reset := delivery(t, sub)
	if reset.VideoConfig.Info().HDR != (media.HDRMetadata{}) {
		t.Fatal("Undefined сохранил прежние static metadata")
	}
	memory.output.Reset()
	if err = outbound.WriteHeaders(context.Background(), reset.VideoConfig, reset.AudioConfig); err != nil {
		t.Fatal(err)
	}
	if _, err = outbound.WriteDelivery(context.Background(), reset); err != nil {
		t.Fatal(err)
	}
	color, err = transportSelfReadFrame(bytes.NewReader(memory.output.Bytes()), 128)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err = amf0.Unmarshal(color.body[5:])
	if err != nil || len(metadata) != 2 {
		t.Fatalf("После reset отсутствует полный заменяющий объект: %+v %v", metadata, err)
	}
	restored, err = parseColorOverride(metadata[1])
	if err != nil || restored.hdr != (media.HDRMetadata{}) || !restored.known[0] {
		t.Fatalf("Reset унаследовал stale CLL/MDCV: %+v %v", restored, err)
	}
}

// TestAMFProjectionRoundsHalvesAway проверяет точную половину шага AMF и соседние кодовые слова AV1.
// Координата 2048/65536=0.03125 должна округлиться к 0.0313; исходное кодовое слово сохраняется.
func TestAMFProjectionRoundsHalvesAway(t *testing.T) {
	// scenario выбирает точную половину шага и соседние реальные значения двоичной сетки.
	for _, scenario := range []struct {
		// raw задаёт исходное кодовое слово AV1, expected — нормативную AMF координату.
		raw      uint64
		expected float64
	}{{2047, 0.0312}, {2048, 0.0313}, {2049, 0.0313}} {
		// value содержит собственное исходное кодовое слово без проекции в другую сетку кодека.
		value := media.HDRValue{Raw: scenario.raw, Grid: media.HDRGridBinaryChromaticity, Known: true}
		// actual содержит число, округлённое до четырёх десятичных знаков; точная половина округляется от нуля.
		// err сообщает о непредставимой проекции исходного компонента.
		actual, err := projectAMFValue(value, 0.0001, 0.7400, true)
		if err != nil || actual != scenario.expected || value.Raw != scenario.raw {
			t.Fatalf("Неверная прямая проекция: %.12f %v", actual, err)
		}
	}
}

// TestPartialHDRRequiresRealInbandChannel проверяет, что известный MaxCLL не теряется из-за неизвестного FALL.
// Блок только из контейнера отклоняется до записи в сеть, а исходный блок SEI сохраняется в AU.
func TestPartialHDRRequiresRealInbandChannel(t *testing.T) {
	// inband выбирает наличие реального SEI канала с тем же частично известным CLL.
	for _, inband := range []bool{false, true} {
		// video и audio предоставляют прежние корректные H.264/AAC без статических HDR-метаданных.
		video, audio := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
		// payload сохраняет существующий независимый IDR без изменения его сжатых байтов.
		payload := transportSelfPacket(t, video, 0, 0, true).Payload()
		// source маркирует реальное происхождение CLL; наличие in-band определяется AU, а не этим полем.
		source := media.HDRSourceContainer
		if inband {
			source = media.HDRSourceH264SEI
			payload = append([]byte{0, 0, 0, 8, 6, 144, 4, 3, 232, 0, 0, 128}, payload...)
		}
		// info добавляет присутствующий CLL без подстановки известного нулевого FALL.
		info := video.Info()
		info.HDR.CLL = media.HDRContentLight{Present: true, Source: source, MaxCLL: media.HDRValue{Raw: 1000, Grid: media.HDRGridInteger, Known: true}, MaxFALL: media.HDRValue{Grid: media.HDRGridInteger}}
		// err хранит отказы создания неизменяемых конфигурации и пакета и штатной сетевой записи.
		var err error
		video, err = media.NewCodecConfig(info, video.Headers())
		if err != nil {
			t.Fatal(err)
		}
		// packet содержит полный исходный SEI+IDR либо IDR без SEI, не меняя их содержимое.
		packet, err := media.NewPacket(media.PacketInfo{DTS: 0, PTS: 0, DurationTicks: 33}, video, payload)
		if err != nil {
			t.Fatal(err)
		}
		// memory и conn используют штатный сериализатор с действующими наблюдениями и счётчиками.
		memory := &transportSelfMemory{}
		conn := transportSelfConn(t, memory)
		if err = conn.WriteHeaders(context.Background(), video, audio); err != nil {
			t.Fatal(err)
		}
		_, err = conn.WritePacket(context.Background(), packet)
		if !inband {
			if !errors.Is(err, media.ErrHDRUnavailable) || memory.output.Len() != 0 || conn.Snapshot().WrittenBytes != 0 {
				t.Fatalf("Container-only partial HDR потерян либо отправлен частично: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		// reader и frame извлекают последнее сообщение со сжатым AU, которое обязано полностью сохранить SEI.
		reader := bytes.NewReader(memory.output.Bytes())
		var frame transportSelfFrame
		for reader.Len() > 0 {
			frame, err = transportSelfReadFrame(reader, 128)
			if err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(frame.body[8:], payload) {
			t.Fatal("In-band partial HDR SEI потерял исходные поля")
		}
	}
}
