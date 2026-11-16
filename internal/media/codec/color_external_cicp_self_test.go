package codec

import (
	"encoding/hex"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// TestExternalCICPConstraintsRemainNarrow проверяет обязательные ограничения из заголовков кодека:
// внешний дескриптор уточняет только присутствующие unspecified CICP, сохраняя остальные поля.
// t получает все ошибки изолированного сравнения значений без сети и файлов.
func TestExternalCICPConstraintsRemainNarrow(t *testing.T) {
	// coded задаёт полный структурно допустимый фактический формат HEVC с неопределёнными CICP.
	coded := core.VideoFormat{Codec: core.CodecHEVC, CodecProfile: "main10", Width: 64, Height: 64, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, Level: "3.0", Tier: "main", PixelFormat: "yuv420p10le", BitDepth: 10, Color: core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}}
	// declared содержит проверяемое внешнее сочетание HDR; provenance подтверждает все три его поля.
	declared := coded
	declared.Color = CICPColor(9, 16, 9, false)
	provenance := media.ColorConfigProvenance{Source: media.HDRSourceERTMP, Primaries: true, Transfer: true, Matrix: true}
	// err сообщает об отказе допустимого уточнения внешних CICP.
	if err := CompatibleVideoConstraints(coded, declared, provenance); err != nil {
		t.Fatal(err)
	}
	if CompatibleVideoActual(coded, declared) == nil || CompatibleVideoConstraints(coded, declared, media.ColorConfigProvenance{}) == nil {
		t.Fatal("Уточнение разрешено без внешнего дескриптора")
	}
	// changed выбирает одно ограничение; обработчик mutate меняет только эту характеристику копии coded.
	for _, changed := range []struct {
		// name подписывает контрпример; mutate меняет переданную копию v формата coded перед сравнением.
		name   string
		mutate func(*core.VideoFormat)
	}{
		{"primaries", func(v *core.VideoFormat) { v.Color.Primaries = "bt709" }},
		{"transfer", func(v *core.VideoFormat) { v.Color.Transfer = "bt709" }},
		{"matrix", func(v *core.VideoFormat) { v.Color.Matrix = "bt709" }},
		{"reserved", func(v *core.VideoFormat) { v.Color.Transfer = "iso-0" }},
		{"unknown", func(v *core.VideoFormat) { v.Color.Transfer = "UNKNOWN" }},
		{"profile", func(v *core.VideoFormat) { v.CodecProfile = "main" }},
		{"depth", func(v *core.VideoFormat) { v.BitDepth, v.PixelFormat = 8, "yuv420p" }},
		{"chroma", func(v *core.VideoFormat) { v.PixelFormat = "yuv422p10le" }},
		{"range", func(v *core.VideoFormat) { v.Color.Range = "full" }},
		{"width", func(v *core.VideoFormat) { v.Width = 128 }},
		{"height", func(v *core.VideoFormat) { v.Height = 128 }},
		{"fps", func(v *core.VideoFormat) { v.FrameRate.Numerator = 60 }},
	} {
		// actual содержит один независимый контрпример, не меняющий базовый coded.
		actual := coded
		changed.mutate(&actual)
		if CompatibleVideoConstraints(actual, declared, provenance) == nil {
			t.Fatal("Потеряно ограничение", changed.name)
		}
	}
	provenance.Transfer = false
	if CompatibleVideoConstraints(coded, declared, provenance) == nil {
		t.Fatal("Отсутствующее внешнее поле разрешило уточнение transfer")
	}
	provenance.Transfer, provenance.Source = true, media.HDRSourceHEVCSEI
	if CompatibleVideoConstraints(coded, declared, provenance) == nil {
		t.Fatal("Неверное происхождение разрешило уточнение CICP")
	}
	provenance.Source = media.HDRSourceERTMP
	coded.Codec, declared.Codec = core.CodecAV1, core.CodecAV1
	coded.CodecProfile, declared.CodecProfile = "main", "main"
	declared.Color = CICPColor(1, 1, 1, false)
	if CompatibleVideoConstraints(coded, declared, provenance) == nil {
		t.Fatal("Прежний SDR AV1 получил ослабленное сравнение")
	}
	coded.Codec, declared.Codec = core.CodecH264, core.CodecH264
	coded.CodecProfile, declared.CodecProfile = "high10", "high10"
	coded.Tier, declared.Tier = "", ""
	if coded.Validate() != nil || declared.Validate() != nil || CompatibleVideoConstraints(coded, declared, provenance) == nil {
		t.Fatal("Прежний SDR H.264 получил ослабленное сравнение либо неверно задан контрпример")
	}
}

// TestVP9ExternalCICPChecksCodedColorspace проверяет unspecified в записи VP9 вместе с реальным заголовком ключевого кадра.
// t фиксирует успех внешнего BT.709, отказ противоречия обобщённого BT.709 и сохранность записи кодека/AU.
func TestVP9ExternalCICPChecksCodedColorspace(t *testing.T) {
	// headers содержит profile0/8bit/limited без трёх CICP; payload — реальный красный кадр 64×64.
	// err сообщает о повреждении буквальной шестнадцатеричной записи payload.
	headers := []byte{0, 10, 0x82, 2, 2, 2, 0, 0}
	payload, err := hex.DecodeString("824983424003f003f60038241c184a00003060000078b9ffff57b0ffffff7c17fffffffc78c1cfd2e880")
	if err != nil {
		t.Fatal(err)
	}
	// actual берёт геометрию из кадра, FPS из точной шкалы теста; err сохраняет отказ синтаксиса.
	actual, err := VP9Format(headers, payload, core.FrameRate{Numerator: 30, Denominator: 1})
	if err != nil {
		t.Fatal(err)
	}
	actual.Color = CICPColor(1, 1, 1, false)
	// info содержит проверенный внешний цвет без дополнительных HDR-блоков.
	info := media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30}, Video: actual, HDR: media.HDRMetadata{ColorConfig: media.ColorConfigProvenance{Source: media.HDRSourceERTMP, Primaries: true, Transfer: true, Matrix: true}}}
	// config фиксирует исходную запись кодека и внешнее описание без ретегирования исходных байтов.
	config, err := media.NewCodecConfig(info, headers)
	if err != nil || ValidateVideoConfig(config) != nil || ValidateVP9Config(headers, actual) == nil {
		t.Fatal("Неверно разделены строгая проверка и проверка внешнего цвета", err)
	}
	// packet содержит исходный показанный KEY_FRAME с точной длительностью 1/30 секунды.
	packet, err := media.NewPacket(media.PacketInfo{DurationTicks: 1, Keyframe: true}, config, payload)
	if err != nil {
		t.Fatal(err)
	}
	// inspector проверяет реальный исходный кадр, а не только применённый внешний объект.
	var inspector Inspector
	// key подтверждает независимость кадра; inspectErr сохраняет отказ проверки его реальных заголовков.
	if key, inspectErr := inspector.Inspect(packet); inspectErr != nil || !key {
		t.Fatal(key, inspectErr)
	}
	info.Video.Color = CICPColor(9, 16, 9, false)
	config, err = media.NewCodecConfig(info, headers)
	if err != nil {
		t.Fatal(err)
	}
	packet, err = media.NewPacket(packet.Info(), config, payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = inspector.Inspect(packet); err == nil {
		t.Fatal("Внешний HDR скрыл обобщённый BT.709 самого кадра")
	}
}

// TestFreshHDRPreservesExternalColorProvenance проверяет перенос признака происхождения без выдачи его за внутрипоточные метаданные.
// t получает ошибку только при потере проверенного внешнего происхождения свежего результата.
func TestFreshHDRPreservesExternalColorProvenance(t *testing.T) {
	// container содержит реальное происхождение nclx при отсутствии MDCV/CLL.
	container := media.HDRMetadata{ColorConfig: media.ColorConfigProvenance{Source: media.HDRSourceContainer, Primaries: true, Transfer: true, Matrix: true}}
	// reconciled получает признак происхождения контейнера без внутрипоточных метаданных; err сохраняет отказ.
	reconciled, err := ReconcileFreshHDR(media.HDRMetadata{}, container, media.HDRMetadata{})
	if err != nil || reconciled != container {
		t.Fatal(reconciled, err)
	}
}
