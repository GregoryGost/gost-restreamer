package codec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
)

// ac3BindingFixture возвращает копию независимо размеченной AU кодировщика без coupling.
// t сообщает об ошибке точной шестнадцатеричной записи; отдельная проба установила SHA256 d470f928….
func ac3BindingFixture(t *testing.T) []byte {
	t.Helper()
	// raw хранит неизменённую AU из 768 байтов; err сообщает об ошибке статической записи.
	raw, err := hex.DecodeString("0b771afb144043e106a0b861ff3abe7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7c7fceaf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f197f872640000000078db6db6db6db78f1e3bbbbbbbbbbbbbbbbbbbbbbbbbbbbc78f1b6db7cf9ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b4000000078db6db6db6db78f1e3bbbbbbbbbbbbbbbbbbbbbbbbbbbbc78f1b6db7cf9ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b4c0000000000f1b6db6db6db6f1e3c777777777777777777777777777778f1e36db6f9f35ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad680000000f1b6db6db6db6f1e3c777777777777777777777777777778f1e36db6f9f35ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6980000000001e36db6db6db6de3c78eeeeeeeeeeeeeeeeeeeeeeeeeeeef1e3c6db6df3e6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad00000001e36db6db6db6de3c78eeeeeeeeeeeeeeeeeeeeeeeeeeeef1e3c6db6df3e6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad300000000003c6db6db6db6dbc78f1dddddddddddddddddddddddddddde3c78db6dbe7cd6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5a00000003c6db6db6db6dbc78f1dddddddddddddddddddddddddddde3c78db6dbe7cd6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5a6000000000078db6db6db6db78f1e3bbbbbbbbbbbbbbbbbbbbbbbbbbbbc78f1b6db7cf9ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b4000000078db6db6db6db78f1e3bbbbbbbbbbbbbbbbbbbbbbbbbbbbc78f1b6db7cf9ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b4c0000000000f1b6db6db6db6f1e3c777777777777777777777777777778f1e36db6f9f35ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad680000000f1b6db6db6db6f1e3c777777777777777777777777777778f1e36db6f9f35ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad680c3c0")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestAC3ActualBindingAndWholeSilence проверяет dac3, CRC и точную независимо измеренную нормализацию.
// t принадлежит изолированному модульному тесту; декодер FFmpeg этому тесту не требуется.
func TestAC3ActualBindingAndWholeSilence(t *testing.T) {
	// raw содержит размеченную AU; original сохраняет её независимую копию для проверки неизменяемости.
	raw := ac3BindingFixture(t)
	original := bytes.Clone(raw)
	// headers содержит фактический dac3; err сообщает о непригодном кадре либо CRC.
	headers, err := AC3Headers(raw)
	if err != nil || !bytes.Equal(headers, []byte{0x10, 0x11, 0x40}) {
		t.Fatalf("dac3=%x err=%v", headers, err)
	}
	// normalized содержит результат полной синтаксической нормализации.
	normalized, err := NormalizeAC3Silence(headers, raw)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(normalized)) != "c7bd89bf376fe9f765dc7789b95bec3186384cc3ee2ab1b8efc4edcd7002e448" {
		t.Fatalf("unexpected normalized SHA256=%x", sha256.Sum256(normalized))
	}
	if !bytes.Equal(raw, original) {
		t.Fatal("исходная AU изменена")
	}
	if _, err = AC3Headers(normalized); err != nil {
		t.Fatal(err)
	}
	// repeated проверяет идемпотентность уже нормализованной формы.
	repeated, err := NormalizeAC3Silence(headers, normalized)
	if err != nil || !bytes.Equal(repeated, normalized) {
		t.Fatalf("idempotence err=%v", err)
	}
	// bit перечисляет независимо размеченные коэффициенты BAP>0 разных блоков и неподдержанный coupling.
	for _, bit := range []int{1150, 1981, 2812, 3643, 4474, 5305, 73} {
		// bad изменяет использованный коэффициент либо структуру с корректными заново вычисленными CRC.
		bad := bytes.Clone(raw)
		bad[bit/8] ^= 1 << uint(7-bit%8)
		ac3UpdateCRC(bad)
		if _, err = AC3Headers(bad); err != nil {
			t.Fatal(err)
		}
		if _, err = NormalizeAC3Silence(headers, bad); err == nil {
			t.Fatalf("неподдержанная/ненулевая AU bit%d принята", bit)
		}
	}
	// index выбирает байт внутри каждой области CRC, чтобы повреждение не скрывалось нормализацией.
	for _, index := range []int{2, 100, 500, 767} {
		// bad сохраняет исходные CRC после повреждения фактических данных.
		bad := bytes.Clone(raw)
		bad[index] ^= 1
		if _, err = NormalizeAC3Silence(headers, bad); err == nil {
			t.Fatalf("bad CRC byte%d принят", index)
		}
	}
}
