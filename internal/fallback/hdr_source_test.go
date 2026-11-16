package fallback

import (
	"context"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// TestHDRSourceExactRationals проверяет точное восстановление исходных сеток FFmpeg без преобразования через float64,
// включая десятичный MP4 mdcv при AV1 и двоичный SmDm при VP9.
func TestHDRSourceExactRationals(t *testing.T) {
	// scenario задаёт исходную рациональную запись и ожидаемое исходное кодовое слово.
	for _, scenario := range []struct {
		// text задаёт исходный рационал; grid — предполагаемую сетку кодека, actual — фактическую.
		text         string
		grid, actual media.HDRGrid
		// raw сохраняет ожидаемое кодовое слово без округления.
		raw uint64
	}{
		{"34000/50000", media.HDRGridBinaryChromaticity, media.HDRGridSEIChromaticity, 34000},
		{"44564/65536", media.HDRGridBinaryChromaticity, media.HDRGridBinaryChromaticity, 44564},
		{"10000000/10000", media.HDRGridBinaryMaxLuminance, media.HDRGridSEILuminance, 10000000},
		{"256000/256", media.HDRGridBinaryMaxLuminance, media.HDRGridBinaryMaxLuminance, 256000},
	} {
		// value/err содержат восстановленное исходное число или отказ потери точности.
		value, err := sourceHDRValue(scenario.text, scenario.grid)
		if err != nil || value.Raw != scenario.raw || value.Grid != scenario.actual || !value.Known {
			t.Fatal(scenario, value, err)
		}
	}
	// text обозначает число, которое не должно округляться либо превращаться в известный ноль.
	for _, text := range []string{"", "N/A", "-1/50000", "1/3", "NaN", "18446744073709551616/50000"} {
		// err должен отклонить отсутствующее, некорректное или неточно представимое исходное число.
		if _, err := sourceHDRValue(text, media.HDRGridSEIChromaticity); err == nil {
			t.Fatal(text)
		}
	}
}

// TestHDRSourceAbsenceAndAuthority проверяет отсутствие выдуманных блоков, неизвестный CLL0,
// приоритет авторитетной сигнализации кодека и отказ противоречащему внешнему дескриптору.
func TestHDRSourceAbsenceAndAuthority(t *testing.T) {
	// empty/err проверяют отсутствие метаданных по умолчанию при отсутствии дескрипторов.
	empty, err := sourceHDRMetadata(nil, core.CodecHEVC)
	if err != nil || empty != (media.HDRMetadata{}) {
		t.Fatal(empty, err)
	}
	// zero и maximum задают неизвестный CLL и известную границу яркости.
	zero, maximum := uint64(0), uint64(1000)
	// external содержит фактический контейнерный CLL с одним неизвестным компонентом.
	external, err := sourceHDRMetadata([]sourceHDRSide{{Type: "Content light level metadata", MaxCLL: &maximum, MaxFALL: &zero}}, core.CodecHEVC)
	if err != nil || !external.CLL.Present || external.CLL.MaxFALL.Known || external.CLL.MaxFALL.Grid != media.HDRGridInteger {
		t.Fatal(external, err)
	}
	// inband сохраняет те же исходные кодовые слова с независимым SEI происхождением.
	inband := external
	inband.CLL.Source = media.HDRSourceHEVCSEI
	// combined обязан сохранять авторитетную сигнализацию кодека, не выдавая контейнерные сведения за внутрипоточное доказательство.
	combined, err := combineSourceHDR(inband, external)
	if err != nil || combined != inband {
		t.Fatal(combined, err)
	}
	external.CLL.MaxCLL.Raw++
	if _, err = combineSourceHDR(inband, external); err == nil {
		t.Fatal("противоречащий CLL принят")
	}
}

// TestHDRSourcePassRequiresHDRTarget проверяет через t, что дополнительный проход нужен
// только HDR-результату; неподдержанные изображения и контейнеры отклоняются до ffprobe.
func TestHDRSourcePassRequiresHDRTarget(t *testing.T) {
	// profile задаёт текущий SDR-профиль; required содержит его обязательный вариант.
	profile := selfProfile(core.CodecH264)
	required := []core.RequiredProfile{{Profile: profile}}
	if needsHDRSource(required) {
		t.Fatal("SDR получил ненужный проход HDR-источника")
	}
	profile.Encoding = core.DefaultEncoding(profile.Mode, core.VideoEncoderX264, core.RateControlCRF)
	profile.Format.Video.Color.Transfer = "smpte2084"
	required[0].Profile = profile
	if !needsHDRSource(required) {
		t.Fatal("HDR не получил обязательный проход источника")
	}
	// prober не имеет runner: запрещённые формы источника должны завершаться до внешнего процесса.
	prober := Prober{Limits: selfProbeLimits()}
	// err подтверждает отказ HDR-изображению до запуска внешнего процесса.
	if _, err := prober.SourceHDR(context.Background(), nil, Source{Kind: core.AssetImage}); err == nil {
		t.Fatal("изображение HDR принято без сведений об источнике")
	}
	// err подтверждает отказ видеоконтейнеру вне поддержанной привязки MOV/MP4.
	if _, err := prober.SourceHDR(context.Background(), nil, Source{Kind: core.AssetVideo, Demuxer: "matroska", Format: core.SourceFormat{Video: &profile.Format.Video}}); err == nil {
		t.Fatal("HDR вне MOV/MP4 принят")
	}
}
