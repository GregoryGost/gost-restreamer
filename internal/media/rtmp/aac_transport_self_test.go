package rtmp

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/bytecounter"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// TestAACMultichannelRawTransport проверяет исходный PCE 2.1 и AU через исходящий RTMP,
// входной ограничитель и типизированный SDK. Повторное кодирование ASC библиотекой теряет PCE,
// поэтому приём обязан пользоваться сохранёнными исходными байтами wire.lastConfig.
func TestAACMultichannelRawTransport(t *testing.T) {
	// headers содержит настоящий ASC FFmpeg 7.1.5 с PCE 2.1 и комментарием кодировщика;
	// err сохраняет отказы разбора, создания конфигураций и последующих операций RTMP этого теста.
	headers, err := hex.DecodeString("118004c4010020000d4c61766336312e31392e31303156e500")
	if err != nil {
		t.Fatal(err)
	}
	// format содержит независимо разобранный формат, audio — его неизменяемую конфигурацию.
	format, err := codec.AudioFormatASC(headers)
	if err != nil {
		t.Fatal(err)
	}
	audio, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: format}, headers)
	if err != nil {
		t.Fatal(err)
	}
	// memory принимает реальные RTMP-байты без сетевого сокета; conn использует штатный исходящий тракт.
	memory := &transportSelfMemory{}
	conn := transportSelfConn(t, memory)
	conn.target.profile.Audio = format
	// video содержит контрольный H.264 для начала исходящего потока.
	video, _ := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 48000})
	if err = conn.WriteHeaders(context.Background(), video, audio); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.WritePacket(context.Background(), transportSelfPacket(t, video, 0, 0, true)); err != nil {
		t.Fatal(err)
	}
	// reader читает фактически записанные сообщения; frame последовательно принимает их тела.
	reader := bytes.NewReader(memory.output.Bytes())
	var frame transportSelfFrame
	for range 3 {
		frame, err = transportSelfReadFrame(reader, 128)
		if err != nil {
			t.Fatal(err)
		}
	}
	if frame.typ != 8 || !bytes.Equal(frame.body, append([]byte{0x90, 'm', 'p', '4', 'a'}, headers...)) {
		t.Fatalf("изменён исходный Enhanced ASC: %x", frame.body)
	}
	// wire проверяет то же исходное аудиосообщение перед передачей библиотеке.
	wire := guarded(t, chunk0(4, 8, frame.body), testLimits())
	// counter считает проверенные байты; sdk разбирает типизированное сообщение без перекодирования.
	counter := bytecounter.NewReader(wire)
	sdk := message.NewReader(counter, counter, func(uint32) error {
		// Локальная проверка конфигурации не отправляет подтверждений в сеть.
		return nil
	})
	// msg содержит сообщение Enhanced Audio, readErr сообщает об отказе SDK.
	msg, readErr := sdk.Read()
	if readErr != nil {
		t.Fatal(readErr)
	}
	// ok подтверждает, что библиотека распознала Enhanced Audio SequenceStart.
	if _, ok := msg.(*message.AudioExSequenceStart); !ok || !bytes.Equal(wire.lastConfig[5:], headers) {
		t.Fatal("входной адаптер потерял PCE", msg)
	}
	// payload содержит настоящий AU тишины с CPE0 и LFE0; packet сохраняет его в исходном формате.
	payload, err := hex.DecodeString("211004608c0c230000e0")
	if err != nil {
		t.Fatal(err)
	}
	packet, err := media.NewPacket(media.PacketInfo{PTS: 1024, DTS: 1024, DurationTicks: 1024}, audio, payload)
	if err != nil {
		t.Fatal(err)
	}
	memory.output.Reset()
	if _, err = conn.WritePacket(context.Background(), packet); err != nil {
		t.Fatal(err)
	}
	frame, err = transportSelfReadFrame(bytes.NewReader(memory.output.Bytes()), 128)
	if err != nil || frame.typ != 8 || !bytes.Equal(frame.body, append([]byte{0x91, 'm', 'p', '4', 'a'}, payload...)) {
		t.Fatalf("изменён Enhanced AU: %x, %v", frame.body, err)
	}
}
