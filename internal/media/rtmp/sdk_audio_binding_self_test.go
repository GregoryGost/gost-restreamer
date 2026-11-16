package rtmp

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/bluenviron/gortmplib/pkg/bytecounter"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// audioBindingHex декодирует фиксированные байты raw для t; ошибка тестовых данных завершает проверку.
func audioBindingHex(t *testing.T, raw string) []byte {
	t.Helper()
	// decoded содержит отдельные исходные байты, err сообщает об ошибке шестнадцатеричной записи.
	decoded, err := hex.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

// audioBindingFixture возвращает заголовки медиатракта, одну AU и число отсчётов кодека kind.
// t проверяет данные; FLAC содержит постоянные стереоканалы и номер7, AC3 — исходную AU FFmpeg без coupling.
func audioBindingFixture(t *testing.T, kind core.Codec) ([]byte, []byte, uint32) {
	t.Helper()
	switch kind {
	case core.CodecOpus:
		return audioBindingHex(t, "4f707573486561640102000044ac0000000000"), []byte{0xe4, 0xff, 0xfe}, 120
	case core.CodecFLAC:
		return audioBindingHex(t, "003012000000000000000bb802f00000000000000000000000000000000000000000"), audioBindingHex(t, "fff87a180711ff0700000000000074b2"), 4608
	case core.CodecMP3:
		// payload содержит один независимый кадр MPEG1L3/128кбит/с/48кГц с main_data_begin0.
		payload := make([]byte, 384)
		copy(payload, []byte{0xff, 0xfb, 0x94, 0})
		return bytes.Clone(payload[:4]), payload, 1152
	case core.CodecAC3:
		return []byte{0x10, 0x11, 0x40}, audioBindingHex(t, "0b771afb144043e106a0b861ff3abe7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7c7fceaf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f197f872640000000078db6db6db6db78f1e3bbbbbbbbbbbbbbbbbbbbbbbbbbbbc78f1b6db7cf9ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b4000000078db6db6db6db78f1e3bbbbbbbbbbbbbbbbbbbbbbbbbbbbc78f1b6db7cf9ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b4c0000000000f1b6db6db6db6f1e3c777777777777777777777777777778f1e36db6f9f35ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad680000000f1b6db6db6db6f1e3c777777777777777777777777777778f1e36db6f9f35ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6980000000001e36db6db6db6de3c78eeeeeeeeeeeeeeeeeeeeeeeeeeeef1e3c6db6df3e6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad00000001e36db6db6db6de3c78eeeeeeeeeeeeeeeeeeeeeeeeeeeef1e3c6db6df3e6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad300000000003c6db6db6db6dbc78f1dddddddddddddddddddddddddddde3c78db6dbe7cd6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5a00000003c6db6db6db6dbc78f1dddddddddddddddddddddddddddde3c78db6dbe7cd6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5a6000000000078db6db6db6db78f1e3bbbbbbbbbbbbbbbbbbbbbbbbbbbbc78f1b6db7cf9ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b4000000078db6db6db6db78f1e3bbbbbbbbbbbbbbbbbbbbbbbbbbbbc78f1b6db7cf9ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b4c0000000000f1b6db6db6db6f1e3c777777777777777777777777777778f1e36db6f9f35ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad680000000f1b6db6db6db6f1e3c777777777777777777777777777778f1e36db6f9f35ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad680c3c0"), 1536
	}
	t.Fatalf("неизвестный codec fixture: %s", kind)
	return nil, nil, 0
}

// audioBindingSDKReader создаёт единственный настоящий читатель SDK над ограничителем wire.
// Обработчик ACK принимает уведомление в памяти; управление размером чанков остаётся у SDK.
func audioBindingSDKReader(wire *wireConn) *message.Reader {
	// counter сохраняет счётчики именно данного wire, без второго чтения транспорта.
	counter := bytecounter.NewReader(wire)
	return message.NewReader(counter, counter, func(uint32) error {
		// Локальная проверка парсера не отправляет ACK в сеть.
		return nil
	})
}

// TestAudioBindingSDKRawLayouts проверяет нормативные байты OpusLE/FLAC42 через настоящий SDK.
// t также подтверждает запрет ошибочной формы SDK с34байтами FLAC и OpusBE/preSkip за допустимыми границами.
func TestAudioBindingSDKRawLayouts(t *testing.T) {
	// selected выбирает одну из двух записей, которые SDK интерпретирует с нарушением сетевого формата.
	for _, selected := range []core.Codec{core.CodecOpus, core.CodecFLAC} {
		// headers содержат заголовки медиатракта, body — нормативный SequenceStart для selected.
		headers, _, _ := audioBindingFixture(t, selected)
		body := append([]byte{0x90}, []byte(outboundAudioFourCC(selected))...)
		if selected == core.CodecOpus {
			headers[10] = 120
		} else {
			body = append(body, []byte{'f', 'L', 'a', 'C', 0x80, 0, 0, 34}...)
		}
		body = append(body, headers...)
		// wire и sdk выполняют реальное чтение одного сообщения с проверкой его завершения.
		wire := guarded(t, chunk0(4, 8, body), testLimits())
		sdk := audioBindingSDKReader(wire)
		// decoded и err сохраняют результат штатного адаптера, start и ok проверяют типизированное описание.
		decoded, err := wire.readMessage(sdk.Read)
		start, ok := decoded.(*message.AudioExSequenceStart)
		if err != nil || !ok {
			t.Fatalf("%s: %T %v", selected, decoded, err)
		}
		if selected == core.CodecOpus && (start.OpusConfig == nil || start.OpusConfig.PreSkip != 120 || start.OpusConfig.InputSampleRate != 44100) {
			t.Fatalf("Opus LE потерян: %+v", start.OpusConfig)
		}
		if selected == core.CodecFLAC && (start.FlacConfig == nil || start.FlacConfig.SampleRate != 48000) {
			t.Fatalf("FLAC record потерян: %+v", start.FlacConfig)
		}
		if !bytes.Equal(wire.lastConfig, body) {
			t.Fatal("raw record изменён")
		}
	}
	// body содержит ошибочные записи, которые успешный разбор SDK не должен легализовать.
	for _, body := range [][]byte{
		append([]byte{0x90, 'f', 'L', 'a', 'C'}, audioBindingHex(t, "003012000000000000000bb802f00000000000000000000000000000000000000000")...),
		append([]byte{0x90, 'O', 'p', 'u', 's'}, audioBindingHex(t, "4f70757348656164010200780000ac44000000")...),
	} {
		// wire и sdk выполняют новый независимый вызов; err должен сохранить отказ формы.
		wire := guarded(t, chunk0(4, 8, body), testLimits())
		sdk := audioBindingSDKReader(wire)
		// err сохраняет фактический результат SDK и адаптера после одного чтения сообщения.
		if _, err := wire.readMessage(sdk.Read); err == nil {
			t.Fatal("ошибочный SDK layout принят")
		}
	}
}

// TestAudioBindingSDKCompletionAndIOCause исключает повторное использование старого сообщения и потерю ошибки после успешного разбора SDK.
// t проверяет реальный отказ записи ACK и ошибку последнего Read с полными данными.
func TestAudioBindingSDKCompletionAndIOCause(t *testing.T) {
	// headers и raw задают нормативный Opus, который закреплённый SDK успешно разбирает с неверной частотой.
	headers, _, _ := audioBindingFixture(t, core.CodecOpus)
	raw := chunk0(4, 8, append([]byte{0x90, 'O', 'p', 'u', 's'}, headers...))
	// mode выбирает отказ последнего Read либо записи ACK.
	for _, mode := range []string{"read", "ack"} {
		// input содержит одно сообщение; для проверки ACK предварительно устанавливается окно1.
		input := bytes.Clone(raw)
		if mode == "ack" {
			input = append(chunk0(2, 5, []byte{0, 0, 0, 1}), input...)
		}
		// cause и conn задают фактическую ошибку транспорта; wire позволяет сохранить её исходное значение.
		cause := errors.New("изолированный отказ Opus транспорта")
		conn := &sdkFailureConn{memoryConn: memoryConn{bytes.NewReader(input)}}
		// wire ограничивает чтение conn; err сообщает о некорректных бюджетах до начала SDK.
		wire, err := newWireConn(conn, testLimits())
		if err != nil {
			t.Fatal(err)
		}
		wire.handshake = 0
		// counter и sdk сохраняют штатное чтение и запись ACK и управляющих сообщений.
		counter := bytecounter.NewReadWriter(wire)
		sdk := message.NewReadWriter(counter, counter, false)
		if mode == "ack" {
			if _, err = wire.readMessage(sdk.Read); err != nil {
				t.Fatal(err)
			}
			conn.writeFailure = cause
		} else {
			conn.completeReadFailure = cause
		}
		if _, err = wire.readMessage(sdk.Read); !errors.Is(err, cause) {
			t.Fatalf("%s: I/O cause скрыта: %v", mode, err)
		}
	}
	// wire позволяет сначала завершить прежнее сообщение без вызова адаптера.
	wire := guarded(t, raw, testLimits())
	// sdk разбирает тот же транспорт, которому уже принадлежит захваченное сообщение.
	sdk := audioBindingSDKReader(wire)
	// err сохраняет фактический результат SDK и адаптера после одного чтения сообщения.
	if _, err := sdk.Read(); err != nil {
		t.Fatal(err)
	}
	// unexpected подтверждает отсутствие прежнего сообщения, err сохраняет настоящий EOF транспорта.
	unexpected, err := wire.readMessage(sdk.Read)
	if !errors.Is(err, io.EOF) || unexpected != nil {
		t.Fatalf("stale completion: %T %v", unexpected, err)
	}
}
