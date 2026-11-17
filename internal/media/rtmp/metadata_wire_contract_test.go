package rtmp

import (
	"errors"
	"math"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/bluenviron/gortmplib/pkg/amf0"
)

// TestColorOverridePreservesOriginalAMFAndCodedConstraints проверяет точные биты IEEE 754,
// замену статических метаданных полным объектом и запрет ретегирования известных CICP/глубины.
func TestColorOverridePreservesOriginalAMFAndCodedConstraints(t *testing.T) {
	// video представляет фактически установленный HEVC main10/PQ, а coded — авторские SEI CLL 1000/400.
	video := core.VideoFormat{Codec: core.CodecHEVC, Width: 320, Height: 180, FrameRate: core.FrameRate{Numerator: 24, Denominator: 1}, CodecProfile: "main10", PixelFormat: "yuv420p10le", BitDepth: 10, Color: core.ColorInfo{Range: "limited", Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc"}}
	coded := media.HDRMetadata{CLL: media.HDRContentLight{Present: true, Source: media.HDRSourceHEVCSEI, MaxCLL: media.HDRValue{Raw: 1000, Grid: media.HDRGridInteger, Known: true}, MaxFALL: media.HDRValue{Raw: 400, Grid: media.HDRGridInteger, Known: true}}}
	// maximum является ближайшим binary64 выше 500: округление до целого или десятичной строки потеряет биты.
	maximum := math.Nextafter(500, 1000)
	// override и err сохраняют новый полный CLL без отдельного colorConfig.
	override, err := parseColorOverride(amf0.Object{{Key: "hdrCll", Value: amf0.Object{{Key: "maxCLL", Value: maximum}, {Key: "maxFall", Value: 200.0}}}})
	if err != nil {
		t.Fatal(err)
	}
	// appliedVideo и applied показывают, что отсутствие colorConfig не заменяет кодированные CICP значениями по умолчанию.
	appliedVideo, applied, err := override.apply(video, coded)
	if err != nil || appliedVideo != video || applied.CLL.Source != media.HDRSourceERTMP || applied.CLL.MaxCLL.Raw != math.Float64bits(maximum) || applied.CLL.MaxCLL.Grid != media.HDRGridAMFBinary64 {
		t.Fatalf("Снимок AMF потерял число или ограничения кодированного потока: %+v, %+v, %v", appliedVideo, applied, err)
	}
	// selector задаёт независимые конфликтующие поля кодированного потока, которые нельзя исправить сменой тега.
	for _, selector := range []struct {
		// name указывает поле colorConfig; value задаёт заведомое противоречие фактическому HEVC HDR.
		name  string
		value float64
	}{
		{"bitDepth", 8}, {"colorPrimaries", 1}, {"transferCharacteristics", 18}, {"matrixCoefficients", 1},
	} {
		// conflicting является корректным объектом с противоречащим индексом; err сообщает отказ его разбора.
		conflicting, err := parseColorOverride(amf0.Object{{Key: "colorConfig", Value: amf0.Object{{Key: selector.name, Value: selector.value}}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = conflicting.apply(video, coded); !errors.Is(err, ErrCodec) {
			t.Fatalf("Противоречие %s принято: %v", selector.name, err)
		}
	}
	// configOnly заменяет override полностью и не наследует CLL от прежнего объекта либо кодека.
	configOnly, err := parseColorOverride(amf0.Object{{Key: "colorConfig", Value: amf0.Object{{Key: "bitDepth", Value: 10.0}}}})
	if err != nil {
		t.Fatal(err)
	}
	if appliedVideo, applied, err = configOnly.apply(video, applied); err != nil || appliedVideo != video || applied != (media.HDRMetadata{}) {
		t.Fatalf("Замена подмешала старый HDR: %+v, %v", applied, err)
	}
	// reset выбирает обе нормативные формы снятия override, возвращающие первоначальные сведения кодека.
	for _, reset := range []any{amf0.Undefined{}, amf0.Object{}} {
		// cleared содержит неактивный override, который не должен означать известные нули.
		cleared, err := parseColorOverride(reset)
		if err != nil {
			t.Fatal(err)
		}
		if appliedVideo, applied, err = cleared.apply(video, coded); err != nil || appliedVideo != video || applied != coded {
			t.Fatalf("Сброс потерял метаданные кодека: %+v, %v", applied, err)
		}
	}
}

// TestColorOverrideRejectsAmbiguousAndIncompleteObjects проверяет отказ дубликатов и частичных секций.
// Ошибка не может дополняться прежним MaxFALL, нулём либо произвольной десятичной координатой.
func TestColorOverrideRejectsAmbiguousAndIncompleteObjects(t *testing.T) {
	// invalid содержит самостоятельные неверные AMF формы, включая Object и ECMAArray с повторным ключом.
	for _, invalid := range []any{
		amf0.Object{{Key: "hdrCll", Value: amf0.Object{{Key: "maxCLL", Value: 500.0}}}},
		amf0.Object{{Key: "hdrCll", Value: amf0.Object{{Key: "maxCLL", Value: 500.0}, {Key: "maxCLL", Value: 600.0}, {Key: "maxFall", Value: 200.0}}}},
		amf0.ECMAArray{{Key: "colorConfig", Value: amf0.Object{}}, {Key: "colorConfig", Value: amf0.Object{}}},
		amf0.Object{{Key: "colorConfig", Value: amf0.Object{{Key: "bitDepth", Value: 10.5}}}},
		amf0.Object{{Key: "hdrMdcv", Value: amf0.Object{{Key: "redX", Value: 0.70801}}}},
	} {
		// err должен обозначить некорректную секцию до её применения к публикации.
		if _, err := parseColorOverride(invalid); !errors.Is(err, ErrCodec) {
			t.Fatalf("Неоднозначный или неполный colorInfo принят: %#v, %v", invalid, err)
		}
	}
}

// TestAMFCoordinateBoundsAreCheckedBeforeRounding проверяет физический диапазон source до проекции.
// Число за пределом нельзя принять только потому, что округление вернуло допустимую координату.
func TestAMFCoordinateBoundsAreCheckedBeforeRounding(t *testing.T) {
	// scenario задаёт исходное кодовое слово SEI, допустимость и буквальную AMF-проекцию с четырьмя знаками.
	for _, scenario := range []struct {
		// raw задаёт исходную координату в 1/50000; allowed указывает её принадлежность AMF-порту.
		raw     uint64
		allowed bool
		// want является независимым точным результатом прямой проекции на 1/10000.
		want float64
	}{
		{4, false, 0}, {5, true, 0.0001}, {37000, true, 0.7400}, {37001, false, 0},
		{7, true, 0.0001}, {8, true, 0.0002},
	} {
		// value сохраняет известный источник даже для чисел, непредставимых на выбранном внешнем порте.
		value := media.HDRValue{Raw: scenario.raw, Grid: media.HDRGridSEIChromaticity, Known: true}
		// actual и err содержат прямую проекцию без насыщения и промежуточной сетки.
		actual, err := projectAMFValue(value, 0.0001, 0.7400, true)
		if scenario.allowed {
			if err != nil || actual != scenario.want {
				t.Fatalf("Допустимая координата %d: %.8f, %v", scenario.raw, actual, err)
			}
		} else if !errors.Is(err, media.ErrHDRUnavailable) {
			t.Fatalf("Недопустимая координата %d принята: %.8f, %v", scenario.raw, actual, err)
		}
	}
}
