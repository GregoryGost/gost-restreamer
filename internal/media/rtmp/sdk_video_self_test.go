package rtmp

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/abema/go-mp4"
	"github.com/bluenviron/gortmplib/pkg/bytecounter"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// TestSDKVP9RecordPreservesMessageIdentity проверяет record без FullBox на настоящем SDK reader.
// Descriptor получает исходные поля, а сырой record, счётчик байтов и следующие сообщения сохраняются.
func TestSDKVP9RecordPreservesMessageIdentity(t *testing.T) {
	// record задаёт profile2, level31, 10-bit420, limited и PQ/BT.2020 без codec initialization.
	record := []byte{2, 31, 0xa0, 9, 16, 9, 0, 0}
	// config и tail образуют два последовательных сообщения одного исходного потока.
	config := chunk0(6, 9, append([]byte{0x90, 'v', 'p', '0', '9'}, record...))
	tail := chunk0(4, 8, []byte{0xaf, 1, 0xbe, 0xef})
	// wire ограничивает реальные байты, counter считает выдачу тому же SDK reader.
	wire := guarded(t, append(bytes.Clone(config), tail...), testLimits())
	counter := bytecounter.NewReader(wire)
	// reader использует существующий обработчик ACK без дополнительных действий в этом тесте.
	reader := message.NewReader(counter, counter, func(uint32) error { return nil })
	// decoded и err содержат результат единственного чтения нормативного VP9 record.
	decoded, err := wire.readMessage(reader.Read)
	if err != nil {
		t.Fatal(err)
	}
	// start и ok подтверждают правильный тип и поля восстановленного descriptor SDK.
	start, ok := decoded.(*message.VideoExSequenceStart)
	if !ok || start.VP9Config == nil || start.ChunkStreamID != 6 || start.VP9Config.Profile != 2 || start.VP9Config.BitDepth != 10 || start.VP9Config.TransferCharacteristics != 16 || !bytes.Equal(wire.lastConfig[5:], record) || counter.Count() != uint64(len(config)) {
		t.Fatalf("Неверный descriptor или учёт: %+v count=%d", decoded, counter.Count())
	}
	decoded, err = wire.readMessage(reader.Read)
	if err != nil {
		t.Fatal(err)
	}
	// audio и valid подтверждают, что адаптер не прочитал и не потерял следующее сообщение.
	audio, valid := decoded.(*message.Audio)
	if !valid || !bytes.Equal(audio.AU, []byte{0xbe, 0xef}) || counter.Count() != uint64(len(config)+len(tail)) {
		t.Fatalf("Нарушен следующий AU или счётчик: %+v %d", decoded, counter.Count())
	}
}

// TestSDKVP9RecordDoesNotHideTransportFailure проверяет причинные отказы на последних байтах record.
// Совпадение строки ошибки SDK не разрешает восстанавливать descriptor после сбоя транспорта.
func TestSDKVP9RecordDoesNotHideTransportFailure(t *testing.T) {
	// record содержит нормативные восемь байтов VP9, original принимает несовместимый FullBox разбор SDK.
	record := []byte{0, 31, 0x80, 1, 1, 1, 0, 0}
	original := mp4.VpcC{}
	// parseErr получает действительную причину pinned parser на этих исходных байтах.
	_, parseErr := mp4.Unmarshal(bytes.NewReader(record), uint64(len(record)), &original, mp4.Context{})
	if parseErr == nil {
		t.Fatal("Исходный record неожиданно разобран как FullBox")
	}
	// cause точно совпадает с ожидаемой одноуровневой ошибкой pinned FullBox parser.
	cause := fmt.Errorf("invalid VP9 configuration: %w", parseErr)
	// config содержит полный нормативный profile0 record, без FullBox-префикса.
	config := chunk0(6, 9, append([]byte{0x90, 'v', 'p', '0', '9'}, record...))
	// conn сопровождает последние реальные байты явной причинной ошибкой.
	conn := &sdkFailureConn{memoryConn: memoryConn{bytes.NewReader(config)}, completeReadFailure: cause}
	// wire и err создают штатный ограничитель без сетевого согласования.
	wire, err := newWireConn(conn, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	wire.handshake = 0
	// counter и reader используют тот же bytecounter и единственный reader SDK.
	counter := bytecounter.NewReader(wire)
	reader := message.NewReader(counter, counter, func(uint32) error { return nil })
	// decoded и readErr должны сохранить транспортную причину вместо ложного успеха.
	decoded, readErr := wire.readMessage(reader.Read)
	if decoded != nil || !errors.Is(readErr, cause) {
		t.Fatalf("Потеряна причинная ошибка: %+v %v", decoded, readErr)
	}
}
