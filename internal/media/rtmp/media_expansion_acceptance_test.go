package rtmp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/bluenviron/gortmplib/pkg/bytecounter"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// expansionFaultConn добавляет точные ошибки ввода-вывода к соединению в памяти,
// позволяя отличить реальный отказ транспорта от одинакового текста ошибки парсера.
type expansionFaultConn struct {
	// memoryConn предоставляет байты входного RTMP и не создаёт сетевых ресурсов.
	memoryConn
	// readError выдаётся после всех входных байтов; writeError заменяет результат любой записи.
	readError, writeError error
	// terminalWithData возвращает readError вместе с последними успешно прочитанными байтами.
	terminalWithData bool
}

// Read возвращает входные байты, затем точный readError без изменения идентичности ошибки.
func (c *expansionFaultConn) Read(p []byte) (int, error) {
	if c.Len() == 0 && c.readError != nil {
		return 0, c.readError
	}
	// n и err сохраняют фактическое чтение, которое может завершиться полными данными и ошибкой.
	n, err := c.Reader.Read(p)
	if n > 0 && c.Len() == 0 && c.terminalWithData {
		return n, c.readError
	}
	return n, err
}

// Write возвращает точный writeError либо успешное принятие p без записи внешним получателям.
func (c *expansionFaultConn) Write(p []byte) (int, error) {
	if c.writeError != nil {
		return 0, c.writeError
	}
	return len(p), nil
}

// expansionSDK создаёт один настоящий reader/writer SDK с исходными bytecounter,
// ACK и control-обработчиками над проверяемым wire; стадия handshake тесту блоков не нужна.
func expansionSDK(t *testing.T, c *expansionFaultConn) (*wireConn, *message.ReadWriter) {
	t.Helper()
	// wire содержит рабочую защитную границу; err сообщает об отказе её настройки.
	wire, err := newWireConn(c, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	wire.handshake = 0
	// counter сохраняет реальные прочитанные и записанные байты этого же соединения.
	counter := bytecounter.NewReadWriter(wire)
	return wire, message.NewReadWriter(counter, counter, false)
}

// TestMediaExpansionSDKCurrentConfigSurvivesFragments проверяет два разных config12
// в однобайтовых чанках: описание каждого Read должно соответствовать именно текущему ASC.
func TestMediaExpansionSDKCurrentConfigSurvivesFragments(t *testing.T) {
	// first и second содержат независимые нормативные AAC LC config12 при 48 и 44,1 кГц.
	first := []byte{0x90, 'm', 'p', '4', 'a', 0x11, 0xe0, 0x56, 0xe5, 0}
	second := []byte{0x90, 'm', 'p', '4', 'a', 0x12, 0x60, 0x56, 0xe5, 0}
	// data включает SetChunkSize=1, затем две полностью самостоятельные конфигурации.
	data := append(chunk0(2, 1, []byte{0, 0, 0, 1}), fragmented(4, 8, first, 1)...)
	data = append(data, fragmented(4, 8, second, 1)...)
	// wire и sdk используют один исходный reader и счётчик; conn хранит только байты этого теста.
	conn := &expansionFaultConn{memoryConn: memoryConn{bytes.NewReader(data)}}
	wire, sdk := expansionSDK(t, conn)
	// err сообщает об отказе допустимого объявления размера чанка.
	if _, err := wire.readMessage(sdk.Read); err != nil {
		t.Fatal(err)
	}
	// index и rate задают независимую последовательность ожидаемых конфигураций.
	for index, rate := range []int{48000, 44100} {
		// msg и err содержат результат ровно одного рабочего SDK Read.
		msg, err := wire.readMessage(sdk.Read)
		if err != nil {
			t.Fatal(err)
		}
		// audio и ok проверяют именно Enhanced Audio описание, без его Marshal.
		audio, ok := msg.(*message.AudioExSequenceStart)
		if !ok || audio.AACConfig.ChannelConfig != 12 || audio.AACConfig.SampleRate != rate || audio.ChunkStreamID != 4 || audio.MessageStreamID != 1 {
			t.Fatalf("Неверный текущий descriptor: %+v", msg)
		}
		// Внутреннее описание pinned SDK обязано содержать реальное число каналов без Marshal ASC.
		if audio.AACConfig.ChannelCount != 8 { //nolint:staticcheck // SA1019: Поле требуется для совместимого descriptor библиотеки с реальным числом каналов.
			t.Fatal("SDK descriptor потерял число каналов")
		}
		// expected выбирает реальные байты того заголовка, которому принадлежит это описание.
		expected := first
		if index == 1 {
			expected = second
		}
		if !bytes.Equal(wire.lastConfig, expected) {
			t.Fatal("SDK перемаршалил или опередил текущий ASC")
		}
	}
	// msg должен остаться nil; err подтверждает EOF после прошлого успешно обработанного ASC:
	// старую конфигурацию нельзя выдать повторно.
	if msg, err := wire.readMessage(sdk.Read); !errors.Is(err, io.EOF) || msg != nil {
		t.Fatalf("Старый ASC скрыл EOF: %T %v", msg, err)
	}
}

// TestMediaExpansionSDKPreservesTransportErrorsWithParserText проверяет I/O, ACK и
// ping/control write errors, полностью совпадающие с parse error по тексту и цепочке Unwrap.
func TestMediaExpansionSDKPreservesTransportErrorsWithParserText(t *testing.T) {
	// scenario выбирает независимую причину отказа реального reader/writer SDK.
	for _, scenario := range []string{"stale-io", "partial-config", "complete-read-error", "ack-write", "ping-write"} {
		t.Run(scenario, func(t *testing.T) {
			// Проверяет сохранение идентичности точной ошибки вместо выдачи описания прошлого ASC.
			// sentinel повторяет pinned parse error, но является отдельной реальной ошибкой транспорта.
			sentinel := fmt.Errorf("invalid MPEG-4 audio config: %w", errors.New("unsupported channel configuration (12)"))
			// config содержит новый нормативный config12; first сохраняет прежний корректный stereo ASC.
			config := chunk0(4, 8, []byte{0x90, 'm', 'p', '4', 'a', 0x11, 0xe0, 0x56, 0xe5, 0})
			first := chunk0(4, 8, []byte{0x90, 'm', 'p', '4', 'a', 0x11, 0x90})
			// data задаёт входной сценарий; reads — число успешных сообщений до ожидаемой ошибки.
			data := bytes.Clone(first)
			reads := 1
			switch scenario {
			case "partial-config":
				data = append(data, config[:len(config)-1]...)
			case "complete-read-error":
				data = append(data, config...)
			case "ack-write":
				data = append(data, chunk0(2, 5, []byte{0, 0, 0, 1})...)
				data = append(data, config...)
				reads = 2
			case "ping-write":
				data = append(data, chunk0(2, 4, []byte{0, 6, 0, 0, 0, 9})...)
			}
			// conn возвращает одинаковый текст при настоящем I/O или записи ACK/control.
			conn := &expansionFaultConn{memoryConn: memoryConn{bytes.NewReader(data)}, readError: sentinel}
			conn.terminalWithData = scenario == "complete-read-error"
			if scenario == "ack-write" || scenario == "ping-write" {
				conn.writeError = sentinel
			}
			// wire и sdk сохраняют единый bytecounter и штатные ответные сообщения библиотеки.
			wire, sdk := expansionSDK(t, conn)
			for range reads {
				// err сообщает об отказе подготовительного сообщения до исследуемой транспортной ошибки.
				if _, err := wire.readMessage(sdk.Read); err != nil {
					t.Fatal("Ошибка подготовительного сообщения", err)
				}
			}
			// msg и err содержат отказ исследуемого чтения; ошибку нельзя заменить ни nil, ни описанием.
			msg, err := wire.readMessage(sdk.Read)
			if msg != nil || err != sentinel {
				t.Fatalf("SDK скрыл реальный отказ: %T %v", msg, err)
			}
		})
	}
}

// TestMediaExpansionSDKRejectsMalformedConfig12Tail не даёт обойти общий парсер ASC
// на той же pinned ошибке channel_configuration12 при испорченных GASpecificConfig или хвосте.
func TestMediaExpansionSDKRejectsMalformedConfig12Tail(t *testing.T) {
	// raw задаёт усечённое либо структурно несовместимое представление config12.
	for _, raw := range [][]byte{{0x11, 0xe1}, {0x11, 0xe0, 0xff}, {0x11, 0xe0, 0x56, 0xe5}} {
		// data содержит единственный действительно полностью доставленный RTMP audio header.
		data := chunk0(4, 8, append([]byte{0x90, 'm', 'p', '4', 'a'}, raw...))
		// conn, wire и sdk обеспечивают реальное чтение без искусственного callback-отказа.
		conn := &expansionFaultConn{memoryConn: memoryConn{bytes.NewReader(data)}}
		wire, sdk := expansionSDK(t, conn)
		// msg и err проверяют, что одинаковый текст pinned парсера не разрешает ошибочный ASC.
		msg, err := wire.readMessage(sdk.Read)
		if err == nil || msg != nil {
			t.Fatalf("Повреждённый ASC12 принят: %x %T %v", raw, msg, err)
		}
	}
}
