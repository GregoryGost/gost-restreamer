package media_test

import (
	"errors"
	"math"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// TestHDRProjectionKeepsBinary64SideOfHalf проверяет направленное округление исходного AMF:
// точная половина округляется вверх, ближайшие соседи остаются по разные стороны границы.
// t получает отличия от буквальных результатов для двоично представимой величины 0.5.
func TestHDRProjectionKeepsBinary64SideOfHalf(t *testing.T) {
	// scenario задаёт исходное число и ожидаемый целый код без вычисления ожидания алгоритмом Project.
	for _, scenario := range []struct {
		// name различает три соседних числа; input сохраняет исходный AMF; want задаёт целую проекцию.
		name  string
		input float64
		want  uint64
	}{
		{"below", math.Nextafter(0.5, 0), 0},
		{"half", 0.5, 1},
		{"above", math.Nextafter(0.5, 1), 1},
	} {
		// Подтест сохраняет биты IEEE 754 и проверяет округление без предварительного десятичного округления.
		t.Run(scenario.name, func(t *testing.T) {
			// value и err содержат исходный binary64 и причину отказа его приёма.
			value, err := media.NewHDRAMFValue(scenario.input)
			if err != nil || value.Raw != math.Float64bits(scenario.input) {
				t.Fatalf("Потеряны исходные биты: %+v, %v", value, err)
			}
			// before защищает неизменяемое число от изменения во время вычисления проекции.
			before := value
			// actual содержит целую проекцию, err сохраняет отказ представимости.
			actual, err := value.Project(media.HDRGridInteger)
			if err != nil || actual != scenario.want || value != before {
				t.Fatalf("Project(%016x) = %d, %v; ожидалось %d", value.Raw, actual, err, scenario.want)
			}
		})
	}
	// original сохраняет точное AMF-число, err сообщает отказ его приёма.
	// rational является отдельным изменяемым результатом Rational.
	original, err := media.NewHDRAMFValue(0.5)
	if err != nil {
		t.Fatal(err)
	}
	rational, err := original.Rational()
	if err != nil {
		t.Fatal(err)
	}
	rational.SetInt64(999)
	// actual подтверждает отсутствие ссылки на изменённый rational внутри исходного значения.
	actual, err := original.Project(media.HDRGridInteger)
	if err != nil || actual != 1 {
		t.Fatalf("Rational изменил источник: %d, %v", actual, err)
	}
}

// TestHDRUnknownAndOverflowNeverBecomeDefaults проверяет отсутствие подстановки яркости
// для неизвестного компонента и отсутствие насыщения непредставимого исходного числа.
func TestHDRUnknownAndOverflowNeverBecomeDefaults(t *testing.T) {
	// unknown хранит присутствующее неопределённое кодовое слово CLL, а не известную нулевую яркость.
	unknown := media.HDRValue{Grid: media.HDRGridInteger}
	// target перечисляет сетки, в которых неизвестность обязана остаться ошибкой представимости.
	for _, target := range []media.HDRGrid{media.HDRGridInteger, media.HDRGridSEILuminance, media.HDRGridBinaryMaxLuminance} {
		// err должен сообщить недоступность числа без назначения значений по умолчанию.
		if _, err := unknown.Project(target); !errors.Is(err, media.ErrHDRUnavailable) {
			t.Fatalf("Неизвестное значение принято в сетке %d: %v", target, err)
		}
	}
	// huge является конечным AMF Number, но не помещается ни в один целочисленный кодовый порт.
	huge, err := media.NewHDRAMFValue(math.MaxFloat64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = huge.Project(media.HDRGridInteger); !errors.Is(err, media.ErrHDRUnavailable) {
		t.Fatalf("Переполнение превращено в число: %v", err)
	}
	if unknown == (media.HDRValue{}) || unknown.Known {
		t.Fatal("Присутствующий неопределённый компонент смешан с отсутствием")
	}
}

// TestCodecConfigOwnsHDRSnapshotAndRejectsAudioMetadata проверяет перенос HDR по значению:
// изменение исходного Info или результата Info не меняет сохранённый снимок, аудио не принимает HDR.
func TestCodecConfigOwnsHDRSnapshotAndRejectsAudioMetadata(t *testing.T) {
	// info задаёт фактический HEVC main10/PQ и частично известный CLL без выдуманного MaxCLL.
	info := media.CodecConfigInfo{
		Track: media.TrackVideo, SourceEpoch: 3, Revision: 7, TimeBase: media.TimeBase{Numerator: 1, Denominator: 24},
		Video: core.VideoFormat{Codec: core.CodecHEVC, Width: 320, Height: 180, FrameRate: core.FrameRate{Numerator: 24, Denominator: 1}, CodecProfile: "main10", PixelFormat: "yuv420p10le", BitDepth: 10, Color: core.ColorInfo{Range: "limited", Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc"}},
		HDR:   media.HDRMetadata{CLL: media.HDRContentLight{Present: true, Source: media.HDRSourceHEVCSEI, MaxCLL: media.HDRValue{Grid: media.HDRGridInteger}, MaxFALL: media.HDRValue{Raw: 400, Grid: media.HDRGridInteger, Known: true}}},
	}
	// expected хранит исходный снимок целиком; headers моделирует принадлежащий вызывающему буфер.
	expected := info
	headers := []byte{1, 2, 3}
	// config и err содержат сохранённую копию и результат структурной проверки.
	config, err := media.NewCodecConfig(info, headers)
	if err != nil {
		t.Fatal(err)
	}
	info.HDR.CLL.MaxFALL.Raw = 900
	headers[0] = 9
	// returned является ещё одной копией описания, которую вызывающий вправе изменять.
	returned := config.Info()
	returned.HDR.CLL.MaxFALL.Known = false
	if config.Info() != expected || config.Headers()[0] != 1 {
		t.Fatalf("CodecConfig разделяет изменяемые данные: %+v", config.Info())
	}
	info = expected
	info.Track, info.Video = media.TrackAudio, core.VideoFormat{}
	info.Audio = core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}
	if _, err = media.NewCodecConfig(info, []byte{0x11, 0x90}); !errors.Is(err, media.ErrInvalidConfig) {
		t.Fatalf("HDR принят для аудио: %v", err)
	}
	info.HDR = media.HDRMetadata{}
	if _, err = media.NewCodecConfig(info, []byte{0x11, 0x90}); err != nil {
		t.Fatalf("Обычный AAC отклонён: %v", err)
	}
}
