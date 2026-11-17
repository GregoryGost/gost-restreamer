package codec

import (
	"bytes"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestMP3ActualHeaderReservoirAndPerAUFields проверяет дескриптор фактического кадра и независимость AU.
// t проверяет поддержанные стерео и joint-stereo, локальное дополнение, флаги и ограничивающие поля.
func TestMP3ActualHeaderReservoirAndPerAUFields(t *testing.T) {
	// raw содержит кадр MPEG1L3, 128 кбит/с, 48 кГц, стерео с пустыми независимыми основными данными.
	raw := make([]byte, 384)
	copy(raw, []byte{0xff, 0xfb, 0x94, 0})
	// headers содержит рабочий дескриптор из фактических первых 4 байтов; err сообщает об отказе кадра.
	headers, err := MP3Headers(raw)
	if err != nil || !bytes.Equal(headers, raw[:4]) {
		t.Fatal(err)
	}
	// changed имеет совместимые локальные флаги AU, joint-stereo и законный байт дополнения.
	changed := append(bytes.Clone(raw), 0)
	changed[2] |= 3
	changed[3] = 0x7d
	if _, err = AudioSamples(core.CodecMP3, headers, changed); err != nil {
		t.Fatal("AU-local fields ошибочно стали конфигурацией", err)
	}
	if raw[2] != 0x94 || raw[3] != 0 {
		t.Fatal("изменён source descriptor")
	}
	// mutate задаёт недопустимую форму при сохранении прочих полей исходного кадра.
	for _, mutate := range []struct {
		// index выбирает изменяемый байт, value задаёт его неподдержанное значение.
		index int
		value byte
	}{{1, 0xfa}, {1, 0xf3}, {2, 0x90}, {2, 0xa4}, {3, 0x80}, {3, 0xc0}, {4, 1}, {5, 0x80}, {6, 0x0f}} {
		// bad сохраняет независимую копию для каждого отрицательного сценария.
		bad := bytes.Clone(raw)
		bad[mutate.index] = mutate.value
		if _, err = MP3Headers(bad); err == nil {
			t.Fatalf("invalid MP3 byte%d=%x принят", mutate.index, mutate.value)
		}
	}
	if _, err = MP3Headers(raw[:383]); err == nil {
		t.Fatal("усечённая frame принята")
	}
	// format и completed проверяют ограниченное уточнение пустой сетевой конфигурации MP3.
	format, completed, err := CompleteAudioConfig(core.CodecMP3, nil, raw)
	if err != nil || format != audioBinding(core.CodecMP3) || !bytes.Equal(completed, headers) {
		t.Fatalf("completion=%+v/%x err=%v", format, completed, err)
	}
	completed[0] = 0
	if raw[0] != 0xff {
		t.Fatal("completion вернул alias source AU")
	}
}
