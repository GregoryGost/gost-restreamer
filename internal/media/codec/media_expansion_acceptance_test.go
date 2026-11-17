package codec

import (
	"bytes"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestMediaExpansionASCSpeakerLayouts проверяет независимые ASC native AAC FFmpeg 7.1.5
// и нормативное отличие channel_configuration=7 от configuration12 обычного 7.1.
// t сверяет фактическое расположение динамиков с независимыми записями ASC.
func TestMediaExpansionASCSpeakerLayouts(t *testing.T) {
	// tc задаёт исходные байты ASC, ожидаемые динамики и число каналов.
	for _, tc := range []struct {
		// raw содержит hex исходного ASC без повторной сериализации.
		raw string
		// layout задаёт реальную раскладку, независимо от desired-профиля.
		layout string
		// channels задаёт полное число аудиоканалов.
		channels uint16
	}{
		{"118004c4010020000d4c61766336312e31392e31303156e500", "2.1", 3},
		{"119856e500", "3.0", 3},
		{"11a056e500", "4.0", 4},
		{"11a856e500", "5.0", 5},
		{"11b056e500", "5.1", 6},
		{"11e056e500", "7.1", 8},
		{"11b856e500", "7.1(wide-back)", 8},
	} {
		// raw хранит самостоятельные байты ASC; before контролирует неизменность парсера.
		raw := aacHex(t, tc.raw)
		before := bytes.Clone(raw)
		// actual содержит фактическую карту каналов; err — отказ разбора ASC.
		actual, err := AudioFormatASC(raw)
		if err != nil || actual.Codec != core.CodecAAC || actual.CodecProfile != "LC" || actual.SampleRateHz != 48000 || actual.ChannelLayout != tc.layout || actual.Channels != tc.channels || !bytes.Equal(raw, before) {
			t.Fatalf("ASC %s: %+v, %v", tc.layout, actual, err)
		}
		if tc.layout == "7.1(wide-back)" {
			// desired задаёт обычные задние и боковые динамики, несовместимые с wide-back.
			desired := actual
			desired.ChannelLayout = "7.1"
			// video содержит валидную общую дорожку, чтобы отказ мог доказывать несовместимость аудио.
			video := core.VideoFormat{Codec: core.CodecAV1, Width: 128, Height: 128, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: "main", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}}
			// err проверяет пригодность положительной контрольной пары перед отрицательным сравнением.
			if err := core.CompatibleActualFormat(core.MediaFormat{Video: video, Audio: actual}, core.MediaFormat{Video: video, Audio: actual}); err != nil {
				t.Fatal("Некорректна положительная контрольная пара", err)
			}
			if core.CompatibleActualFormat(core.MediaFormat{Video: video, Audio: actual}, core.MediaFormat{Video: video, Audio: desired}) == nil {
				t.Fatal("7.1 wide подменён обычным 7.1")
			}
		}
	}
}

// expansionPCE71 строит независимый нормативный PCE с FL/FR/FC, SL/SR, BL/BR и LFE.
// Комментарий из трёх байтов позволяет проверять полный хвост без привязки к encoder-комментарию.
func expansionPCE71() []byte {
	// b записывает поля ASC/PCE непосредственно по синтаксису AAC, без production парсера/normalizer.
	b := ppsBits{}
	b.put(2, 5)
	b.put(3, 4)
	b.put(0, 4)
	b.put(0, 3)
	b.put(0, 4)
	b.put(1, 2)
	b.put(3, 4)
	b.put(2, 4)
	b.put(1, 4)
	b.put(1, 4)
	b.put(1, 2)
	b.put(0, 3)
	b.put(0, 4)
	b.put(0, 3)
	b.put(0, 1)
	b.put(0, 4)
	b.put(1, 1)
	b.put(0, 4)
	b.put(1, 1)
	b.put(1, 4)
	b.put(1, 1)
	b.put(2, 4)
	b.put(0, 4)
	for b.count%8 != 0 {
		b.put(0, 1)
	}
	b.put(3, 8)
	b.put('a', 8)
	b.put('b', 8)
	b.put('c', 8)
	return b.raw
}

// TestMediaExpansionPCERejectsTruncatedTail проверяет весь comment_field PCE,
// а также расхождение sampling_frequency_index и object_type между ASC и PCE.
func TestMediaExpansionPCERejectsTruncatedTail(t *testing.T) {
	// raw содержит независимый нормативный PCE с полным комментарием.
	raw := expansionPCE71()
	// actual и err проверяют положительную контрольную конфигурацию перед отрицательными мутациями.
	actual, err := AudioFormatASC(raw)
	if err != nil || actual.ChannelLayout != "7.1" || actual.Channels != 8 {
		t.Fatal("Некорректна нормативная PCE-фикстура", actual, err)
	}
	// n выбирает каждый обрыв до конца обязательного comment_field включительно.
	for n := range len(raw) {
		// err сообщает, был ли ошибочно принят неполный хвост PCE.
		if _, err := AudioFormatASC(raw[:n]); err == nil {
			t.Fatalf("Принят усечённый PCE %d", n)
		}
	}
	// bit выбирает object_type PCE (20) либо sampling_frequency_index (22), считая от ASC.
	for _, bit := range []int{20, 22} {
		// damaged меняет поле PCE при прежнем внешнем ASC.
		damaged := bytes.Clone(raw)
		damaged[bit/8] ^= 1 << uint(7-bit%8)
		// err сообщает, было ли ошибочно принято противоречие ASC и PCE.
		if _, err := AudioFormatASC(damaged); err == nil {
			t.Fatalf("Принято расхождение PCE в бите %d", bit)
		}
	}
}

// TestMediaExpansionRejectsNativeForcedPCEWithoutLFE не позволяет объявить
// ошибочную native AAC карту обычным 7.1 только по числу каналов.
func TestMediaExpansionRejectsNativeForcedPCEWithoutLFE(t *testing.T) {
	// forced содержит исходный ASC FFmpeg7.1.5 -aac_pce1 без элемента LFE.
	forced := aacHex(t, "118004c848002000c6400d4c61766336312e31392e31303156e500")
	// err сообщает о поддержке недопустимой карты динамиков.
	if _, err := AudioFormatASC(forced); err == nil {
		t.Fatal("Forced PCE без LFE принят как 7.1")
	}
}

// TestMediaExpansionNativeSignalCorrectionIsNarrow проверяет точное изменение
// четырёх битов известной native-конфигурации и неизменность уже нормативных ASC/PCE.
func TestMediaExpansionNativeSignalCorrectionIsNarrow(t *testing.T) {
	// raw содержит известный native ASC config7; before сохраняет исходные байты для проверки владения.
	raw := aacHex(t, "11b856e500")
	before := bytes.Clone(raw)
	// normalized и err содержат результат единственной разрешённой коррекции сигнализации.
	normalized, err := NormalizeNativeAAC71ASC(raw)
	if err != nil || !bytes.Equal(raw, before) || !bytes.Equal(normalized, aacHex(t, "11e056e500")) {
		t.Fatal("Коррекция изменила лишние биты или вход", err)
	}
	// valid содержит уже нормативные формы, которым нельзя менять ни одного байта.
	for _, valid := range [][]byte{aacHex(t, "11e056e500"), expansionPCE71()} {
		// got и err проверяют идемпотентность на собственной копии нормативного header.
		got, err := NormalizeNativeAAC71ASC(valid)
		if err != nil || !bytes.Equal(got, valid) {
			t.Fatal("Изменён нормативный ASC", err)
		}
	}
	// invalid задаёт неподдержанное или повреждённое представление, которое нельзя чинить.
	for _, invalid := range [][]byte{aacHex(t, "11b8"), aacHex(t, "119056e500"), aacHex(t, "11b856e501"), aacHex(t, "118004c848002000c6400d4c61766336312e31392e31303156e500")} {
		// err сообщает, была ли ошибочно исправлена неизвестная native-конфигурация.
		if _, err := NormalizeNativeAAC71ASC(invalid); err == nil {
			t.Fatalf("Неизвестный ASC исправлен: %x", invalid)
		}
	}
}
