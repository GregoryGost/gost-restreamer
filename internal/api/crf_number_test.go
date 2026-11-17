package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestCRFNumberExactDecimal проверяет сотые, эквивалентные степени и числовой ответ
// без округления. t получает несовпадения независимо заданных десятичных значений.
func TestCRFNumberExactDecimal(t *testing.T) {
	// tc сопоставляет исходный token, значение centi и каноническую числовую запись wire.
	for _, tc := range []struct {
		// token содержит исходное JSON-число; wire — ожидаемую запись без кавычек.
		token, wire string
		// centi задаёт независимо рассчитанное число сотых долей.
		centi int
	}{
		{"0", "0", 0}, {"-0.0", "0", 0}, {"0.01", "0.01", 1},
		{"23.10", "23.1", 2310}, {"2351e-2", "23.51", 2351},
		{"2.351e1", "23.51", 2351}, {"51.000", "51", 5100},
		{"6300e-2", "63", 6300}, {"35e0", "35", 3500},
		{"0e" + strings.Repeat("9", 4096), "0", 0},
		{"0." + strings.Repeat("0", 4096) + "1e4095", "0.01", 1},
	} {
		// got начинает с ненулевого контрольного качества, чтобы проверить явный ноль.
		got := CRFNumber(730)
		// err содержит отказ разбора допустимого исходного токена.
		if err := json.Unmarshal([]byte(tc.token), &got); err != nil || int(got) != tc.centi {
			t.Fatalf("токен %.80q: got=%d want=%d err=%v", tc.token, got, tc.centi, err)
		}
		// wire содержит реальный JSON; err сообщает отказ сериализации точных сотых.
		wire, err := json.Marshal(got)
		if err != nil || string(wire) != tc.wire {
			t.Fatalf("числовой ответ: %s want=%s err=%v", wire, tc.wire, err)
		}
	}
	// token задаёт дробь меньше шага, переполнение, неверную грамматику либо JSON-тип.
	for _, token := range []string{"0.001", "1.0000000000000001", "23.510000000000001", "63.000000000000001", "63.01", "-0.01", "1e99999999", "1e-99999999", `"35"`, "true", "null", "{}", "[]", "01", "+1", "1 2"} {
		// got должен сохранить исходное значение при любом отказе.
		got := CRFNumber(730)
		// err отличает обязательный отказ от ошибочного принятия или частичного присваивания.
		if err := json.Unmarshal([]byte(token), &got); err == nil || got != 730 {
			t.Fatalf("неверный токен %q: got=%d err=%v", token, got, err)
		}
	}
	// invalid проверяет, что внутренний ошибочный ответ не публикуется как допустимый CRF.
	for _, invalid := range []CRFNumber{-1, 6301} {
		// err подтверждает отказ до записи JSON-ответа.
		if _, err := json.Marshal(invalid); err == nil {
			t.Fatal("неверный CRF опубликован", invalid)
		}
	}
}

// TestMeasuredExpandedCICPAndCodecProfiles проверяет независимость каталогов CICP,
// профиля кодека и UNKNOWN. t сравнивает DTO без изменения исходного наблюдения.
func TestMeasuredExpandedCICPAndCodecProfiles(t *testing.T) {
	// format содержит распознаваемые AV1 high и отдельные обозначения каждого CICP-поля.
	format := core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecAV1, CodecProfile: "high", Tier: "high", PixelFormat: "yuv444p10le", Color: core.ColorInfo{Range: "unspecified", Primaries: "film", Transfer: "gamma28", Matrix: "fcc"}}}
	// got содержит публичную проекцию, сохраняющую распознанные значения.
	got := measuredFormatDTO(format)
	if got.Video.CodecProfile != "high" || got.Video.Tier != "high" || got.Video.PixelFormat != "yuv444p10le" || got.Video.Color != (MeasuredColorDTO{"unspecified", "film", "gamma28", "fcc"}) {
		t.Fatal(got)
	}
	format.Video.CodecProfile = "professional"
	if measuredFormatDTO(format).Video.CodecProfile != "professional" {
		t.Fatal("AV1 professional потерян")
	}
	format.Video.Codec = core.CodecH264
	if measuredFormatDTO(format).Video.CodecProfile != "UNKNOWN" {
		t.Fatal("профиль AV1 опубликован для H264")
	}
	format.Video.Color = core.ColorInfo{Primaries: "gamma22", Transfer: "film", Matrix: "cicp:255"}
	got = measuredFormatDTO(format)
	if got.Video.Color.Primaries != "UNKNOWN" || got.Video.Color.Transfer != "UNKNOWN" || got.Video.Color.Matrix != "UNKNOWN" {
		t.Fatal("каталоги CICP смешаны", got)
	}
}
