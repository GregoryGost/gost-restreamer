package codec

import (
	"bytes"
	"encoding/hex"
	"errors"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"testing"
)

// Встраивает флаг и значение initial_display_delay в фиксированную последовательность AV1 и
// оборачивает её в av1C.
func delaySequence(present bool, delay uint8) []byte {
	// base содержит фиксированный заголовок последовательности AV1 до вставки задержки показа.
	base := []byte{0, 0, 0, 66, 167, 191, 228, 96, 13, 0, 64}
	// bits накапливает отдельные биты исходной последовательности для вставки новых полей.
	var bits []byte
	// b содержит очередной байт исходного заголовка последовательности.
	for _, b := range base {
		// i задаёт позицию очередного бита внутри исходного байта, от старшего к младшему.
		for i := 7; i >= 0; i-- {
			bits = append(bits, b>>uint(i)&1)
		}
	}
	bits = bits[:82]
	bits[6] = 1
	// extra содержит бит присутствия задержки и её четыре бита при наличии.
	extra := []byte{0}
	if present {
		extra = []byte{1, delay >> 3 & 1, delay >> 2 & 1, delay >> 1 & 1, delay & 1}
	}
	// payloadBits содержит заголовок последовательности после вставки полей задержки.
	payloadBits := append(append(append([]byte(nil), bits[:30]...), extra...), bits[30:]...)
	// out принимает упакованные байты изменённого заголовка последовательности.
	out := make([]byte, (len(payloadBits)+7)/8)
	// i задаёт позицию упаковываемого бита в последовательности. b содержит значение этого бита.
	for i, b := range payloadBits {
		out[i/8] |= b << uint(7-i%8)
	}
	return append([]byte{0x81, 8, 0x0c, 0, 0x0a, byte(len(out))}, out...)
}

// Проверяет сохранение полей задержки последовательности и исходных байтов при независимом TU;
// задержка в префиксе av1C остаётся запрещённой.
func TestAV1SequenceDisplayDelayPreservesRawAndRandomAccess(t *testing.T) {
	// present выбирает наличие значения задержки показа для рабочей точки.
	for _, present := range []bool{false, true} {
		// delay задаёт крайнее проверяемое значение задержки показа.
		for _, delay := range []uint8{0, 15} {
			// raw содержит av1C с выбранными полями задержки в последовательности.
			raw := delaySequence(present, delay)
			// original сохраняет исходную запись av1C для проверки неизменности входа.
			original := bytes.Clone(raw)
			// d содержит разобранное описание AV1 с задержкой показа. err сообщает об отказе
			// разбора.
			d, err := ParseAV1Config(raw)
			if err != nil {
				t.Fatal(err)
			}
			if !d.seq.InitialDisplayDelayPresentFlag || d.seq.InitialDisplayPresentForThisOp[0] != present || present && d.seq.InitialDisplayDelayMinus1[0] != delay {
				t.Fatalf("потеряны сведения delay: %+v", d.seq)
			}
			// tu содержит ту же последовательность и независимый показанный кадр.
			tu := append(append([]byte(nil), raw[4:]...), 0x1a, 1, 0x10)
			// format содержит подтверждённый формат TU. key показывает независимость его
			// первого кадра.
			format, key, err := d.Inspect(tu, core.FrameRate{Numerator: 30, Denominator: 1})
			if err != nil || !key || format.Width != 1920 || format.Height != 804 || !bytes.Equal(raw, original) {
				t.Fatalf("изменён независимый TU/header: %+v %v %v", format, key, err)
			}
			raw[3] = 0x10
			if _, err = ParseAV1Config(raw); !errors.Is(err, ErrCodec) {
				t.Fatal("av1C delay перестал отвергаться", err)
			}
		}
	}
}

// Проверяет отказ при усечении последовательности AV1, повреждении завершающих битов и добавлении
// лишнего хвоста.
func TestAV1SequenceDelayAndTailTruncationFailClosed(t *testing.T) {
	// raw содержит запись av1C с присутствующей задержкой для проверки усечений и хвоста.
	raw := delaySequence(true, 9)
	// length задаёт длину проверяемого префикса тела последовательности в байтах.
	for length := 0; length < len(raw)-6; length++ {
		// payload ссылается на усечённый заголовок последовательности.
		payload := raw[6 : 6+length]
		// err содержит результат разбора усечённого заголовка последовательности.
		if _, err := parseSequence(payload); !errors.Is(err, ErrCodec) {
			t.Fatalf("обрезанный sequence принят: len=%d err=%v", length, err)
		}
	}
	// payload содержит изменяемую копию тела последовательности для повреждения завершающих битов.
	payload := bytes.Clone(raw[6:])
	payload[len(payload)-1] ^= 1
	// err содержит результат разбора последовательности с повреждённым хвостом.
	if _, err := parseSequence(payload); !errors.Is(err, ErrCodec) {
		t.Fatal(err)
	}
	payload = append(bytes.Clone(raw[6:]), 0)
	// err содержит результат разбора последовательности с лишним нулевым байтом.
	if _, err := parseSequence(payload); !errors.Is(err, ErrCodec) {
		t.Fatal("лишний хвост принят", err)
	}
}

// Проверяет отказ, когда встроенный заголовок TU меняет инструменты декодирования относительно
// принятой конфигурации.
func TestAV1RejectsChangedInlineSequence(t *testing.T) {
	// a содержит исходный av1C без включённого CDEF.
	a, _ := hex.DecodeString("81000c000a0e0200000499bfff00be0404040410")
	// b содержит av1C с изменённым признаком CDEF.
	b, _ := hex.DecodeString("81000c000a0e0200000499bfff1abe6404040410")
	// first содержит исходное описание последовательности. err сообщает об отказе его разбора.
	first, err := ParseAV1Config(a)
	if err != nil {
		t.Fatal(err)
	}
	// second содержит описание последовательности с включённым CDEF.
	second, err := ParseAV1Config(b)
	if err != nil {
		t.Fatal(err)
	}
	if first.seq.EnableCdef || !second.seq.EnableCdef {
		t.Fatal("потеряна фактическая смена decoding tools")
	}
	// tu содержит изменённую последовательность и кадр для проверки несовпадения с first.
	tu := append(append([]byte(nil), b[4:]...), 0x1a, 1, 0x10)
	if _, _, err = first.Inspect(tu, core.FrameRate{Numerator: 30, Denominator: 1}); !errors.Is(err, ErrCodec) {
		t.Fatal("смена sequence не обнаружена", err)
	}
}

// Проверяет построение av1C из фактического первого независимого TU без изменения его байтов и отказ
// при отсутствии, повторении или непригодности последовательности.
func TestAV1ConfigFromActualFirstTUUsesTrueSequence(t *testing.T) {
	// template содержит исходный av1C, чей префикс используется при построении фактической
	// конфигурации.
	template, _ := hex.DecodeString("81000c000a0e0200000499bfff00be0404040410")
	// tu содержит полный независимый TU с собственной фактической последовательностью.
	tu, _ := hex.DecodeString("0a0e0200000499bfff1abe6404040410328401100086800410208005e010867aff68da307d4a1db42fe03b916b8008ee5bb9d4ec58d98d41619596b86567475babe5b4b1bd10f07a853f622967ac8dd6d23c722b5828e9adb9664b07e7ec971eade5375099eedda16c77b1d75e2ea2121a087d1156695662548b288b16ea2c931c2d50cae25c0c9f790c92234a87eac96b05f1bb8d0180")
	// original сохраняет байты первого TU для проверки неизменности входа.
	original := bytes.Clone(tu)
	// headers содержит конфигурацию из фактической последовательности TU. actual содержит
	// подтверждённый формат. err сообщает об отказе построения конфигурации.
	headers, actual, err := AV1ConfigFromTU(template, tu, core.FrameRate{Numerator: 30, Denominator: 1})
	if err != nil || actual.Width != 128 || actual.Height != 128 || actual.Color != (core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}) {
		t.Fatalf("фактический SVT TU не распознан: %+v %v", actual, err)
	}
	if !bytes.Equal(headers[4:], tu[:16]) || !bytes.Equal(tu, original) || bytes.Equal(headers, template) {
		t.Fatal("исходный sequence подменён или вход изменён")
	}
	// key показывает независимость исходного TU относительно построенных заголовков. err сообщает об
	// отказе его повторной проверки.
	if _, key, err := InspectAV1TU(headers, tu, actual.FrameRate); err != nil || !key {
		t.Fatal(key, err)
	}
	// bad содержит вариант первого TU без подходящей единственной последовательности.
	for _, bad := range [][]byte{nil, {0x1a, 1, 0x10}, append(bytes.Clone(tu[:16]), tu...)} {
		if _, _, err = AV1ConfigFromTU(template, bad, actual.FrameRate); err == nil {
			t.Fatal("неподходящий первый TU принят")
		}
	}
	if _, _, err = AV1ConfigFromTU(nil, tu, actual.FrameRate); err == nil {
		t.Fatal("невалидный av1C принят")
	}
}
