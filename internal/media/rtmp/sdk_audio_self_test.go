package rtmp

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/bytecounter"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// sdkFailureConn предоставляет настоящий поток SDK и управляемые отказы транспорта в памяти.
type sdkFailureConn struct {
	// memoryConn читает заданные байты и реализует остальные операции net.Conn без сокета.
	memoryConn
	// readFailure заменяет EOF после исчерпания входа; writeFailure отказывает при записи ACK/control.
	readFailure, writeFailure error
	// readDeadlineFailure и writeDeadlineFailure проверяют отдельную причинную границу сроков.
	readDeadlineFailure, writeDeadlineFailure error
	// completeReadFailure сопровождает последние реальные байты причиной отказа того же Read.
	completeReadFailure error
}

// Read выдаёт реальные байты, затем фактическую ошибку readFailure вместо EOF при её наличии.
func (c *sdkFailureConn) Read(p []byte) (int, error) {
	// n и err сохраняют результат чтения исходных байтов без фабрикации данных.
	n, err := c.memoryConn.Read(p)
	if n > 0 && c.Len() == 0 && c.completeReadFailure != nil {
		return n, c.completeReadFailure
	}
	if err == io.EOF && c.readFailure != nil {
		err = c.readFailure
	}
	return n, err
}

// Write отказывает с заданной причиной до записи; успешная ветка принимает весь буфер.
func (c *sdkFailureConn) Write(p []byte) (int, error) {
	if c.writeFailure != nil {
		return 0, c.writeFailure
	}
	return len(p), nil
}

// SetReadDeadline возвращает заданный отказ срока без изменения входных байтов.
func (c *sdkFailureConn) SetReadDeadline(time.Time) error { return c.readDeadlineFailure }

// SetWriteDeadline возвращает заданный отказ срока без изменения записываемого буфера.
func (c *sdkFailureConn) SetWriteDeadline(time.Time) error { return c.writeDeadlineFailure }

// TestSDKConfig12TransportPreservesRawBytes проводит реальный native AAC ASC/AU через
// штатную исходящую Enhanced Audio форму и тот же SDK reader, который используется при ingress.
func TestSDKConfig12TransportPreservesRawBytes(t *testing.T) {
	// headers содержит config12 после узкого изменения сигнала native AAC FFmpeg 7.1.5.
	headers := []byte{0x11, 0xe0, 0x56, 0xe5, 0}
	// format и err задают фактический формат общего парсера, без SDK remarshal.
	format, err := codec.AudioFormatASC(headers)
	if err != nil {
		t.Fatal(err)
	}
	// audio связывает исходный ASC с точной шкалой 48 kHz.
	audio, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: format}, headers)
	if err != nil {
		t.Fatal(err)
	}
	// memory и conn выполняют штатную сериализацию RTMP в изолированный буфер.
	memory := &transportSelfMemory{}
	conn := transportSelfConn(t, memory)
	conn.target.profile.Audio = format
	// video предоставляет независимый H.264 старт прежнего исходящего тракта.
	video, _ := transportSelfConfigs(t, audio.Info().TimeBase)
	if err = conn.WriteHeaders(context.Background(), video, audio); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.WritePacket(context.Background(), transportSelfPacket(t, video, 0, 0, true)); err != nil {
		t.Fatal(err)
	}
	// input и frame извлекают третье сообщение: фактический Enhanced Audio SequenceStart.
	input := bytes.NewReader(memory.output.Bytes())
	var frame transportSelfFrame
	for range 3 {
		frame, err = transportSelfReadFrame(input, 128)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(frame.body, append([]byte{0x90, 'm', 'p', '4', 'a'}, headers...)) {
		t.Fatalf("ASC изменён при записи: %x", frame.body)
	}
	// wire и counter передают те же bytes существующему типизированному SDK reader.
	wire := guarded(t, chunk0(4, 8, frame.body), testLimits())
	counter := bytecounter.NewReader(wire)
	// sdk использует обычный Reader без окна ACK; callback не выполняет дополнительных действий.
	sdk := message.NewReader(counter, counter, func(uint32) error { return nil })
	// decoded и err проверяют единственное адаптированное чтение без второго reader.
	decoded, err := wire.readMessage(sdk.Read)
	if err != nil {
		t.Fatal(err)
	}
	// start и ok подтверждают настоящий descriptor config12 со всеми фактическими полями.
	start, ok := decoded.(*message.AudioExSequenceStart)
	if !ok || start.AACConfig == nil {
		t.Fatalf("неверная форма descriptor: %+v", decoded)
	}
	// channels проверяет сохранение compatibility-поля реального descriptor без повторного Marshal.
	channels := start.AACConfig.ChannelCount //nolint:staticcheck // SA1019: проверяется обязательная совместимость descriptor pinned SDK.
	if start.AACConfig.ChannelConfig != 12 || channels != 8 || start.AACConfig.SampleRate != 48000 || !bytes.Equal(wire.lastConfig[5:], headers) {
		t.Fatalf("неверный descriptor или raw ASC: %+v", decoded)
	}
	// payload содержит настоящий первый AU anullsrc7.1/aac/-aac_pce0 FFmpeg 7.1.5, включая FIL.
	payload, err := hex.DecodeString("de02004c61766336312e31392e313031000230400211004608c0462008c11809440118230308c00038")
	if err != nil {
		t.Fatal(err)
	}
	// packet задаёт исходную AAC AU и точные PTS/DTS/duration без перекодирования.
	packet, err := media.NewPacket(media.PacketInfo{PTS: 1024, DTS: 1024, DurationTicks: 1024}, audio, payload)
	if err != nil {
		t.Fatal(err)
	}
	memory.output.Reset()
	if _, err = conn.WritePacket(context.Background(), packet); err != nil {
		t.Fatal(err)
	}
	frame, err = transportSelfReadFrame(bytes.NewReader(memory.output.Bytes()), 128)
	if err != nil || !bytes.Equal(frame.body, append([]byte{0x91, 'm', 'p', '4', 'a'}, payload...)) {
		t.Fatalf("изменён исходный AAC AU: %x, %v", frame.body, err)
	}
}

// TestSDKConfig12DoesNotHideIOCause проверяет Write/Read/deadline ошибки с точно таким
// же Error(), как у pinned ASC parser. ACK выполняется настоящим SDK ReadWriter поверх wire.
func TestSDKConfig12DoesNotHideIOCause(t *testing.T) {
	// cause сохраняет заведомо совпадающий текст и цепочку unmarshal ошибки; важна именно её I/O причина.
	cause := fmt.Errorf("invalid MPEG-4 audio config: %w", errors.New("unsupported channel configuration (12)"))
	// config содержит полный config12, window включает обязательную запись ACK при его чтении.
	config := chunk0(4, 8, []byte{0x90, 'm', 'p', '4', 'a', 0x11, 0xe0})
	window := chunk0(2, 5, []byte{0, 0, 0, 1})
	// mode выбирает причинную границу отказа существующего SDK Read.
	for _, mode := range []string{"write", "write-deadline", "read", "read-deadline", "partial", "complete-read"} {
		// Подтест проверяет неизменность конкретной ошибки и отсутствие ложного descriptor.
		t.Run(mode, func(t *testing.T) {
			// data задаёт окно ACK для записи либо старый ASC перед дальнейшим отказом чтения.
			data := append(bytes.Clone(window), config...)
			if mode == "read" || mode == "read-deadline" || mode == "partial" {
				data = bytes.Clone(config)
				if mode == "partial" {
					data = append(data, config[:len(config)-1]...)
				}
			}
			// conn предоставляет реальные байты и включаемый после первого сообщения отказ.
			conn := &sdkFailureConn{memoryConn: memoryConn{bytes.NewReader(data)}}
			// wire и err создают штатный ограничитель без сетевого согласования.
			wire, err := newWireConn(conn, testLimits())
			if err != nil {
				t.Fatal(err)
			}
			wire.handshake = 0
			// counter и sdk сохраняют настоящий reader/writer/ACK state и единый bytecounter.
			counter := bytecounter.NewReadWriter(wire)
			sdk := message.NewReadWriter(counter, counter, false)
			if _, err = wire.readMessage(sdk.Read); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "write":
				conn.writeFailure = cause
			case "write-deadline":
				conn.writeDeadlineFailure = cause
			case "read", "partial":
				conn.readFailure = cause
			case "read-deadline":
				conn.readDeadlineFailure = cause
			case "complete-read":
				conn.completeReadFailure = cause
			}
			// msg и readErr сохраняют фактический результат чтения с причинным отказом транспорта.
			msg, readErr := wire.readMessage(sdk.Read)
			if msg != nil || !errors.Is(readErr, cause) {
				t.Fatalf("замаскирована I/O ошибка %s: %T, %v", mode, msg, readErr)
			}
		})
	}
}
