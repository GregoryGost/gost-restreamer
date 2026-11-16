package rtmp

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// TestExternalCICPIngestAndNativeOutbound проходит настоящий приём AV1 HDR с unspecified в заголовках,
// Router/Delivery, Inspector и штатный сериализатор. t проверяет CICP, исходные байты и снятие прежнего переопределения.
func TestExternalCICPIngestAndNativeOutbound(t *testing.T) {
	// sequence задаёт десятибитный AV1 с действительно неопределёнными CICP в заголовке последовательности.
	sequence := contractSDRSequence()
	sequence.highDepth, sequence.primaries, sequence.transfer, sequence.matrix = true, 2, 2, 2
	// headers и payload сохраняют синтаксически корректные заголовки последовательности/TU без ретегирования на входе.
	headers := contractAV1Config(sequence)
	payload := contractAV1TU(headers)
	// p и sub предоставляют штатную публикацию с отдельным Router и актуальной подпиской.
	p, sub := newContractPublication(t)
	p.wire = &wireConn{lastConfig: append([]byte{0x90, 'a', 'v', '0', '1'}, headers...)}
	// err сохраняет результат последовательной настройки и публикации AU.
	var err error
	if err = p.videoConfig(message.FourCCAV1); err != nil {
		t.Fatal(err)
	}
	p.wire.lastConfig = []byte{0xaf, 0, 0x11, 0x90}
	if err = p.audioConfig(2); err != nil {
		t.Fatal(err)
	}
	// color содержит только явный внешний CICP, без MDCV/CLL, которые могли бы скрыть отсутствие признака происхождения.
	color := amf0.Object{{Key: "colorConfig", Value: amf0.Object{{Key: "bitDepth", Value: float64(10)}, {Key: "colorPrimaries", Value: float64(9)}, {Key: "transferCharacteristics", Value: float64(16)}, {Key: "matrixCoefficients", Value: float64(9)}}}}
	if err = p.videoMetadata(amf0.Data{"colorInfo", color}); err != nil {
		t.Fatal(err)
	}
	if err = p.videoPacket(0, payload); err != nil {
		t.Fatal(err)
	}
	if err = p.audioPacket(0, []byte{0xbe, 0xef}); err != nil {
		t.Fatal(err)
	}
	// selected содержит фактический внешний HDR и неизменённые заголовки кодека/TU.
	selected := delivery(t, sub)
	_ = delivery(t, sub)
	if selected.VideoConfig.Info().HDR.ColorConfig.Source != media.HDRSourceERTMP || selected.VideoConfig.Info().HDR.MDCV.Present || selected.VideoConfig.Info().HDR.CLL.Present || !bytes.Equal(selected.VideoConfig.Headers(), headers) || !bytes.Equal(selected.Packet.Payload(), payload) {
		t.Fatal(selected.VideoConfig.Info())
	}
	// inspector выполняет проверку кодека на следующей ступени, где строгое сравнение отвергало unspecified.
	var inspector codec.Inspector
	if _, _, err = inspector.InspectMetadata(selected.Packet); err != nil {
		t.Fatal(err)
	}
	// memory и conn дают настоящий штатный сериализатор без сетевого подключения.
	memory := &transportSelfMemory{}
	conn := transportSelfConn(t, memory)
	conn.target.profile = core.MediaFormat{Video: selected.VideoConfig.Info().Video, Audio: selected.AudioConfig.Info().Audio}
	if err = conn.WriteHeaders(context.Background(), selected.VideoConfig, selected.AudioConfig); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.WriteDelivery(context.Background(), selected); err != nil {
		t.Fatal(err)
	}
	// reader и first извлекают реальный первый colorInfo до SequenceStart и AU.
	reader := bytes.NewReader(memory.output.Bytes())
	first, err := transportSelfReadFrame(reader, 128)
	if err != nil || first.body[0]&15 != 4 {
		t.Fatal(first, err)
	}
	// metadata и restored проверяют реальные AMF CICP, а не только признак происхождения.
	metadata, err := amf0.Unmarshal(first.body[5:])
	if err != nil || len(metadata) != 2 {
		t.Fatal(metadata, err)
	}
	restored, err := parseColorOverride(metadata[1])
	if err != nil || restored.config != [4]uint8{10, 9, 16, 9} || restored.known != [4]bool{true, true, true, true} {
		t.Fatal(restored, err)
	}
	if err = p.videoMetadata(amf0.Data{"colorInfo", amf0.Object{{Key: "hdrCll", Value: amf0.Object{{Key: "maxCLL", Value: float64(500)}, {Key: "maxFall", Value: float64(200)}}}}}); err != nil || selected.IsCurrent() {
		t.Fatal("Полная замена не отозвала прежнюю выдачу", err)
	}
	if err = p.videoPacket(100*time.Millisecond, payload); err != nil {
		t.Fatal(err)
	}
	if err = p.audioPacket(100*time.Millisecond, []byte{0xbe, 0xef}); err != nil {
		t.Fatal(err)
	}
	// replaced не наследует внешний цвет из прежнего colorConfig при новом объекте только с CLL.
	replaced := delivery(t, sub)
	_ = delivery(t, sub)
	if replaced.VideoConfig.Info().HDR.ColorConfig != (media.ColorConfigProvenance{}) || replaced.VideoConfig.Info().Video.Color.Transfer != "unspecified" {
		t.Fatal("Унаследован прежний внешний CICP", replaced.VideoConfig.Info())
	}
	if err = p.videoMetadata(amf0.Data{"colorInfo", amf0.Undefined{}}); err != nil {
		t.Fatal(err)
	}
	if err = p.videoPacket(200*time.Millisecond, payload); err != nil {
		t.Fatal(err)
	}
	if err = p.audioPacket(200*time.Millisecond, []byte{0xbe, 0xef}); err != nil {
		t.Fatal(err)
	}
	// reset содержит только действительные сведения последовательности без старых метаданных и внешнего происхождения.
	reset := delivery(t, sub)
	if reset.VideoConfig.Info().HDR != (media.HDRMetadata{}) {
		t.Fatal(reset.VideoConfig.Info())
	}
}

// TestExternalCICPNewSDROutbound доставляет внешний BT.709 нового VP9 даже без HDR-блоков.
// t проверяет сетевой colorInfo и запрещает применять зарезервированные коды/UNKNOWN как unspecified.
func TestExternalCICPNewSDROutbound(t *testing.T) {
	// headers и payload принадлежат настоящему VP9 profile0 кадру, а его запись кодека не задаёт три CICP.
	headers, payload, _ := nativeVideoFixture(t, "vp9")
	headers[3], headers[4], headers[5] = 2, 2, 2
	// actual получает геометрию, глубину, субдискретизацию и диапазон из записи кодека и заголовка кадра;
	// err сохраняет отказ разбора исходных сжатых данных.
	actual, err := codec.VP9Format(headers, payload, core.FrameRate{Numerator: 30, Denominator: 1})
	if err != nil {
		t.Fatal(err)
	}
	// override содержит только проверенный внешний цвет нового SDR-пути.
	override, err := parseColorOverride(amf0.Object{{Key: "colorConfig", Value: amf0.Object{{Key: "colorPrimaries", Value: float64(1)}, {Key: "transferCharacteristics", Value: float64(1)}, {Key: "matrixCoefficients", Value: float64(1)}}}})
	if err != nil {
		t.Fatal(err)
	}
	// effective и metadata содержат фактический BT.709 и отдельное доказательство внешних полей.
	effective, metadata, err := override.apply(actual, media.HDRMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	// audio сохраняет прежнюю AAC-дорожку, video содержит неизменённый запись VP9 с внешним фактическим цветом.
	_, audio := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
	video, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: audio.Info().SourceEpoch, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30}, Video: effective, HDR: metadata}, headers)
	if err != nil {
		t.Fatal(err)
	}
	// packet хранит исходный ключевой кадр и точную длительность 1/30 секунды.
	packet, err := media.NewPacket(media.PacketInfo{DurationTicks: 1, Keyframe: true}, video, payload)
	if err != nil {
		t.Fatal(err)
	}
	// memory и conn пропускают SDR через штатные заголовки и реальную проверку кодека AU.
	memory := &transportSelfMemory{}
	conn := transportSelfConn(t, memory)
	conn.target.profile = core.MediaFormat{Video: effective, Audio: audio.Info().Audio}
	if err = conn.WriteHeaders(context.Background(), video, audio); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.WritePacket(context.Background(), packet); err != nil {
		t.Fatal(err)
	}
	// first проверяет, что отдельный colorConfig реально предшествует SDR SequenceStart.
	first, err := transportSelfReadFrame(bytes.NewReader(memory.output.Bytes()), 128)
	if err != nil || first.body[0]&15 != 4 {
		t.Fatal(first, err)
	}
	// bad выбирает неизвестное текстовое имя и зарезервированный числовой код с обязательным отказом.
	for _, bad := range []string{"UNKNOWN", "iso-0"} {
		actual.Color.Transfer = bad
		if _, _, err = override.apply(actual, media.HDRMetadata{}); err == nil {
			t.Fatal("Принята неизвестная кодированная передаточная характеристика", bad)
		}
	}
	// incoming хранит исходную запись отдельно от проверяемого override при последовательном приёме.
	incoming := publication{videoCodec: core.CodecVP9, fps: core.FrameRate{Numerator: 30, Denominator: 1}, override: override}
	incoming.vp9, err = codec.ParseVP9Config(headers)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = incoming.inspectVideo(payload); err != nil {
		t.Fatal(err)
	}
	incoming.override = colorOverride{}
	// reset возвращает исходные unspecified CICP записи кодека, а не предыдущие внешние BT.709.
	reset, _, err := incoming.inspectVideo(payload)
	if err != nil || reset.Color.Primaries != "unspecified" || reset.Color.Transfer != "unspecified" || reset.Color.Matrix != "unspecified" {
		t.Fatal(reset, err)
	}
	incoming.override, err = parseColorOverride(amf0.Object{{Key: "colorConfig", Value: amf0.Object{{Key: "colorPrimaries", Value: float64(9)}, {Key: "transferCharacteristics", Value: float64(16)}, {Key: "matrixCoefficients", Value: float64(9)}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = incoming.inspectVideo(payload); err == nil {
		t.Fatal("Приём опубликовал внешний HDR вопреки обобщённому BT.709 текущего кадра")
	}
}
