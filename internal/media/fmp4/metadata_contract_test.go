package fmp4_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// contractBits независимо накапливает биты синтетического SPS для проверки восстановления метаданных.
type contractBits struct {
	// data хранит байты записываемого битового потока, включая ещё не заполненный последний байт.
	data []byte
	// count считает уже записанные биты для выбора позиции в текущем байте.
	count uint
}

// Дописывает биты значения от старшего к младшему в независимый тестовый поток SPS.
func (b *contractBits) write(v uint64, count uint) {
	// i выбирает очередной бит значения от старшего к младшему в записываемом поле.
	for i := count; i > 0; i-- {
		if b.count%8 == 0 {
			b.data = append(b.data, 0)
		}
		b.data[len(b.data)-1] |= byte((v>>(i-1))&1) << (7 - b.count%8)
		b.count++
	}
}

// Записывает логическое поле одним битом в тестовый поток SPS.
func (b *contractBits) flag(v bool) {
	if v {
		b.write(1, 1)
	} else {
		b.write(0, 1)
	}
}

// Записывает uint32 кодом Exp-Golomb через uint64, сохраняя представимость v+1.
func (b *contractBits) ue(v uint32) {
	// n содержит v+1 в uint64, чтобы кодирование Exp-Golomb не переполнило исходный uint32.
	n := uint64(v) + 1
	// width считает значащие биты n для записи нулевого префикса Exp-Golomb.
	width := uint(0)
	// x содержит сдвигаемую копию n для подсчёта его битовой ширины без изменения самого кодируемого
	// числа.
	for x := n; x != 0; x >>= 1 {
		width++
	}
	b.write(0, width-1)
	b.write(n, width)
}

// contractSPS задаёт управляемые поля синтетического SPS, позволяя независимо менять профиль, размеры
// обрезки, время и цвет.
type contractSPS struct {
	// profile задаёт profile_idc синтетического SPS, constraints — байт ограничений совместимости
	// профиля.
	profile, constraints byte
	// chroma задаёт схему цветовой субдискретизации; lumaDepth и chromaDepth содержат прибавку к
	// базовым восьми битам яркости и цветности.
	chroma, lumaDepth, chromaDepth uint32
	// interlaced включает чересстрочный синтаксис, separate — отдельные цветовые плоскости; noVUI
	// убирает VUI, noTiming — сведения о времени, noSignal — сигнализацию видео, noColor — описание
	// цвета; fullRange выбирает полный диапазон при наличии сигнализации.
	interlaced, separate, noVUI, noTiming, noSignal, noColor, fullRange bool
	// crop хранит левую, правую, верхнюю и нижнюю обрезку в синтаксических единицах SPS, зависящих
	// от цветовой субдискретизации.
	crop [4]uint32
	// units задаёт num_units_in_tick, scale — time_scale для восстановления рациональной частоты
	// кадров из VUI.
	units, scale uint32
	// primaries задаёт код основных цветов, transfer — передаточной характеристики, matrix — матрицы
	// преобразования в VUI.
	primaries, transfer, matrix byte
}

// Возвращает исходные параметры High 4:2:0 с VUI 30000/1001 и цветом BT.709 для независимого
// построения SPS.
func contractSPSDefaults() contractSPS {
	return contractSPS{profile: 100, chroma: 1, units: 1001, scale: 60000, primaries: 1, transfer: 1, matrix: 1}
}

// SPS записывается по H.264 7.3.2.1.1 и VUI из приложения E.
// Размер прогрессивного кадра без обрезки — 128×128; значения crop и VUI задаются самим тестом.
// Первичный независимый разбор: FFmpeg n7.1.5, libavcodec/h264_ps.c.
func contractSPSBytes(s contractSPS) []byte {
	// b накапливает биты SPS до добавления заголовка NAL и защитных байтов.
	b := &contractBits{}
	b.write(uint64(s.profile), 8)
	b.write(uint64(s.constraints), 8)
	b.write(31, 8)
	b.ue(0)
	switch s.profile {
	case 100, 110, 122, 244, 44:
		b.ue(s.chroma)
		if s.chroma == 3 {
			b.flag(s.separate)
		}
		b.ue(s.lumaDepth)
		b.ue(s.chromaDepth)
		b.flag(false)
		b.flag(false)
	}
	b.ue(0)
	b.ue(0)
	b.ue(0)
	b.ue(1)
	b.flag(false)
	b.ue(7)
	b.ue(7)
	b.flag(!s.interlaced)
	if s.interlaced {
		b.flag(false)
	}
	b.flag(true)
	b.flag(s.crop != [4]uint32{})
	if s.crop != [4]uint32{} {
		// v обозначает очередное значение обрезки, записываемое кодом Exp-Golomb.
		for _, v := range s.crop {
			b.ue(v)
		}
	}
	b.flag(!s.noVUI)
	if !s.noVUI {
		b.flag(false)
		b.flag(false)
		b.flag(!s.noSignal)
		if !s.noSignal {
			b.write(5, 3)
			b.flag(s.fullRange)
			b.flag(!s.noColor)
			if !s.noColor {
				b.write(uint64(s.primaries), 8)
				b.write(uint64(s.transfer), 8)
				b.write(uint64(s.matrix), 8)
			}
		}
		b.flag(false)
		b.flag(!s.noTiming)
		if !s.noTiming {
			b.write(uint64(s.units), 32)
			b.write(uint64(s.scale), 32)
			b.flag(true)
		}
		b.flag(false)
		b.flag(false)
		b.flag(false)
		b.flag(false)
	}
	b.flag(true)
	for b.count%8 != 0 {
		b.flag(false)
	}
	// Защитный байт 03 вставляется после двух нулей перед значениями 00..03.
	// nalu накапливает SPS с заголовком NAL и защитными байтами перед опасными последовательностями.
	nalu := []byte{0x67}
	// zeros считает последовательные нулевые байты перед очередным байтом для вставки
	// emulation_prevention_three_byte.
	zeros := 0
	// value содержит очередной байт SPS до проверки необходимости защитного байта.
	for _, value := range b.data {
		if zeros == 2 && value <= 3 {
			nalu = append(nalu, 3)
			zeros = 0
		}
		nalu = append(nalu, value)
		if value == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return nalu
}

// Оборачивает независимо построенный SPS и фиксированный PPS в avcC с четырёхбайтовыми длинами NAL.
func contractAVCC(s contractSPS) []byte {
	// sps содержит независимо построенный набор параметров последовательности для включения в avcC.
	sps := contractSPSBytes(s)
	// PPS имеет id=0 и ссылку на SPS id=0; декодируемость видеопакетов здесь не проверяется.
	// pps содержит фиксированный набор параметров изображения с идентификатором 0 и ссылкой на SPS
	// 0.
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	// b накапливает avcC с длинами и байтами единственного SPS и PPS.
	b := []byte{1, s.profile, s.constraints, 31, 0xff, 0xe1, byte(len(sps) >> 8), byte(len(sps))}
	b = append(b, sps...)
	b = append(b, 1, 0, byte(len(pps)))
	return append(b, pps...)
}

// Создаёт описание записи H.264 128×128 со шкалой 1/30000 и переданными заголовками.
func contractVideoDescription(headers []byte) fmp4.TrackDescription {
	return fmp4.TrackDescription{ID: 9, Track: media.TrackVideo, Codec: core.CodecH264, Width: 128, Height: 128, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30000}, Headers: headers}
}

// Проверяет точное восстановление 30000/1001, обрезки, HDR-сигнализации цвета и идентичности из SPS, а
// также копирование входных заголовков.
func TestContractH264RationalTimingCroppingAndColor(t *testing.T) {
	// s содержит исходные параметры SPS, которые сценарий меняет для проверки обрезки и
	// HDR-сигнализации цвета.
	s := contractSPSDefaults()
	s.crop = [4]uint32{1, 2, 3, 4}
	s.fullRange, s.primaries, s.transfer, s.matrix = true, 9, 16, 9
	// desc содержит описание записи MP4 с фактическими после обрезки размерами и созданным avcC.
	desc := contractVideoDescription(contractAVCC(s))
	// В 4:2:0 обрезаются 6 горизонтальных и 14 вертикальных яркостных отсчётов.
	desc.Width, desc.Height = 122, 114
	// c содержит восстановленную конфигурацию с заданными эпохой и ревизией; err сообщает о её
	// недопустимости.
	c, err := fmp4.ResolveCodecConfig(desc, 51, 7)
	if err != nil {
		t.Fatal(err)
	}
	// want задаёт полный ожидаемый видеоформат, включая точные размеры, частоту, профиль,
	// разрядность и цвет.
	want := core.VideoFormat{Codec: core.CodecH264, Width: 122, Height: 114, FrameRate: core.FrameRate{Numerator: 30000, Denominator: 1001}, CodecProfile: "high", Level: "3.1", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "full", Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc"}}
	if c.Info().Video != want || c.Info().SourceEpoch != 51 || c.Info().Revision != 7 || c.Info().TimeBase != desc.TimeBase || !bytes.Equal(c.Headers(), desc.Headers) {
		t.Fatalf("SPS не восстановлен точно: %+v", c.Info())
	}
	desc.Headers[8] = 0
	if c.Headers()[8] != 0x67 {
		t.Fatal("resolver заимствовал входные headers")
	}
}

// Проверяет восстановление профиля H.264, цветовой субдискретизации, разрядности и частоты для восьми
// независимо построенных SPS.
func TestContractH264ProfilesAndChromaHaveExplicitMetadata(t *testing.T) {
	// tc выбирает сочетание полей SPS и ожидаемого профиля и формата пикселей.
	for _, tc := range []struct {
		// name именует проверяемый профиль для отдельного подтеста.
		name string
		// profile задаёт входной код профиля SPS, constraint — байт ограничений, влияющий на его
		// точное имя.
		profile, constraint byte
		// chroma задаёт входную субдискретизацию; depth определяет прибавку к базовой
		// восьмибитной глубине.
		chroma, depth uint32
		// wantProfile задаёт ожидаемое имя профиля, pixel — ожидаемый формат пикселей
		// восстановленного видео.
		wantProfile, pixel string
	}{
		{"baseline", 66, 0, 1, 0, "baseline", "yuv420p"},
		{"constrained-baseline", 66, 0x40, 1, 0, "constrained-baseline", "yuv420p"},
		{"main", 77, 0, 1, 0, "main", "yuv420p"},
		{"extended", 88, 0, 1, 0, "extended", "yuv420p"},
		{"high10", 110, 0, 1, 2, "high10", "yuv420p10le"},
		{"high422", 122, 0, 2, 2, "high422", "yuv422p10le"},
		{"high444", 244, 0, 3, 4, "high444", "yuv444p12le"},
		{"monochrome", 100, 0, 0, 0, "high", "gray"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет профиль, формат пикселей, глубину и дробную частоту кадров для выбранного сочетания полей
			// SPS.

			// s содержит отдельные параметры SPS выбранного сценария профиля и цветовой
			// дискретизации.
			s := contractSPSDefaults()
			s.profile, s.constraints, s.chroma, s.lumaDepth, s.chromaDepth = tc.profile, tc.constraint, tc.chroma, tc.depth, tc.depth
			// c содержит результат восстановления изменённого SPS; err сообщает об отказе,
			// недопустимом для поддержанного сценария.
			c, err := fmp4.ResolveCodecConfig(contractVideoDescription(contractAVCC(s)), 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			// v хранит восстановленный видеоформат для сравнения профиля, пикселей, глубины
			// и дробной частоты.
			v := c.Info().Video
			if v.CodecProfile != tc.wantProfile || v.PixelFormat != tc.pixel || v.BitDepth != uint8(8+tc.depth) || v.FrameRate != (core.FrameRate{Numerator: 30000, Denominator: 1001}) {
				t.Fatalf("неверное описание: %+v", v)
			}
		})
	}
}

// Проверяет единицы обрезки H.264 для монохромного видео, 4:2:2 и 4:4:4 по независимо заданным
// размерам.
func TestContractH264CropUnitsFollowChromaSampling(t *testing.T) {
	// tc выбирает цветовую субдискретизацию и ожидаемые размеры после одной и той же синтаксической
	// обрезки.
	for _, tc := range []struct {
		// name именует сценарий единиц обрезки для отдельного подтеста.
		name string
		// chroma задаёт входную субдискретизацию; width и height задают ожидаемые размеры видео
		// в пикселях после обрезки.
		chroma, width, height uint32
	}{
		{"monochrome", 0, 125, 121},
		{"422", 2, 122, 121},
		{"444", 3, 125, 121},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет размеры после обрезки SPS с выбранной схемой цветовой дискретизации.

			// s содержит параметры синтетического SPS с выбранной субдискретизацией и
			// заданными смещениями обрезки.
			s := contractSPSDefaults()
			s.profile, s.chroma, s.crop = 244, tc.chroma, [4]uint32{1, 2, 3, 4}
			// desc содержит запись MP4, чьи размеры согласованы с ожидаемым результатом
			// обрезки SPS.
			desc := contractVideoDescription(contractAVCC(s))
			desc.Width, desc.Height = tc.width, tc.height
			// c содержит восстановленную конфигурацию обрезанного видео; err сообщает об
			// отказе её построения.
			c, err := fmp4.ResolveCodecConfig(desc, 1, 1)
			if err != nil || c.Info().Video.Width != tc.width || c.Info().Video.Height != tc.height {
				t.Fatalf("неверные единицы обрезки: %+v, %v", c.Info().Video, err)
			}
		})
	}
}

// Проверяет преобразование цветовых кодов VUI и сохранение unspecified при отсутствии сигнализации или
// описания цвета.
func TestContractH264VUIColorIdentifiersAndAbsence(t *testing.T) {
	// Идентификаторы цвета заданы в FFmpeg n7.1.5, libavutil/pixfmt.h.
	// tc выбирает входные цветовые коды либо отсутствие сигнализации и точное ожидаемое описание
	// цвета.
	for _, tc := range []struct {
		// name именует выбранное сочетание сигнализации цвета для подтеста.
		name string
		// p задаёт код основных цветов VUI, tr — передаточной характеристики, m — матрицы.
		p, tr, m byte
		// rangeName задаёт ожидаемый диапазон, primaries — основные цвета, transfer —
		// передаточную характеристику, matrix — матрицу результата.
		rangeName, primaries, transfer, matrix string
		// noSignal убирает сигнализацию видео, noColor — отдельное описание цвета при
		// сохранённой сигнализации.
		noSignal, noColor bool
	}{
		{"unspecified", 2, 2, 2, "limited", "unspecified", "unspecified", "unspecified", false, false},
		{"bt709", 1, 1, 1, "limited", "bt709", "bt709", "bt709", false, false},
		{"bt470bg-linear", 5, 8, 5, "limited", "bt470bg", "linear", "bt470bg", false, false},
		{"smpte170m", 6, 6, 6, "limited", "smpte170m", "smpte170m", "smpte170m", false, false},
		{"srgb-gbr", 1, 13, 0, "limited", "bt709", "iec61966-2-1", "gbr", false, false},
		{"bt2020-10", 9, 14, 10, "limited", "bt2020", "bt2020-10", "bt2020c", false, false},
		{"bt2020-12", 9, 15, 9, "limited", "bt2020", "bt2020-12", "bt2020nc", false, false},
		{"hlg", 9, 18, 9, "limited", "bt2020", "arib-std-b67", "bt2020nc", false, false},
		{"no-signal", 9, 16, 9, "limited", "unspecified", "unspecified", "unspecified", true, false},
		{"no-color", 9, 16, 9, "limited", "unspecified", "unspecified", "unspecified", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет перевод выбранных идентификаторов цвета VUI либо отсутствующих полей в точное описание
			// ColorInfo.

			// s содержит исходный SPS с подставленными кодами либо отсутствующими полями
			// цвета.
			s := contractSPSDefaults()
			s.primaries, s.transfer, s.matrix, s.noSignal, s.noColor = tc.p, tc.tr, tc.m, tc.noSignal, tc.noColor
			// c содержит конфигурацию, восстановленную из изменённого VUI; err сообщает об
			// отказе её разбора.
			c, err := fmp4.ResolveCodecConfig(contractVideoDescription(contractAVCC(s)), 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			// want задаёт точный ожидаемый ColorInfo с учётом отсутствующей сигнализации и
			// unspecified.
			want := core.ColorInfo{Range: tc.rangeName, Primaries: tc.primaries, Transfer: tc.transfer, Matrix: tc.matrix}
			if c.Info().Video.Color != want {
				t.Fatalf("цвет %+v вместо %+v", c.Info().Video.Color, want)
			}
		})
	}
}

// Проверяет категории ошибок для недоступной частоты, неподдержанного SPS, непредставимого FPS и
// обрезки всего размера кадра.
func TestContractH264UnavailableTimingAndUnsupportedSyntax(t *testing.T) {
	// tc выбирает изменение SPS и категорию ошибки, которую оно должно вызвать.
	for _, tc := range []struct {
		// name именует проверяемое нарушение метаданных SPS для подтеста.
		name string
		// change изменяет отдельный синтетический SPS, создавая выбранную нехватку или
		// недопустимость сведений.
		change func(*contractSPS)
		// want задаёт ожидаемую категорию ошибки для проверки через errors.Is.
		want error
	}{
		{"no-vui", func(s *contractSPS) {
			// Удаляет VUI из создаваемого SPS.
			s.noVUI = true
		}, fmp4.ErrMetadataUnavailable},
		{"no-timing", func(s *contractSPS) {
			// Удаляет сведения о времени из VUI создаваемого SPS.
			s.noTiming = true
		}, fmp4.ErrMetadataUnavailable},
		{"zero-timescale", func(s *contractSPS) {
			// Обнуляет временную шкалу в VUI создаваемого SPS.
			s.scale = 0
		}, fmp4.ErrMetadataUnavailable},
		{"zero-tick", func(s *contractSPS) {
			// Обнуляет число единиц такта в VUI создаваемого SPS.
			s.units = 0
		}, fmp4.ErrMetadataUnavailable},
		{"interlaced", func(s *contractSPS) {
			// Включает чересстрочное кодирование в создаваемом SPS.
			s.interlaced = true
		}, fmp4.ErrMetadataUnavailable},
		{"separate-planes", func(s *contractSPS) {
			// Задаёт отдельные цветовые плоскости при дискретизации 4:4:4.
			s.chroma, s.separate = 3, true
		}, fmp4.ErrMetadataUnavailable},
		{"unequal-depths", func(s *contractSPS) {
			// Задаёт глубину цветности, отличную от глубины яркости.
			s.chromaDepth = 2
		}, fmp4.ErrMetadataUnavailable},
		{"unrepresentable-fps", func(s *contractSPS) {
			// Задаёт отношение временных полей, которое не помещается в поддержанное описание частоты кадров.
			s.units, s.scale = 0xffffffff, 1
		}, fmp4.ErrUnsupportedTiming},
		{"crop-removes-width", func(s *contractSPS) {
			// Задаёт горизонтальную обрезку, полностью удаляющую ширину кадра.
			s.crop[1] = 64
		}, fmp4.ErrInvalid},
		{"crop-removes-height", func(s *contractSPS) {
			// Задаёт вертикальную обрезку, полностью удаляющую высоту кадра.
			s.crop[3] = 64
		}, fmp4.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Применяет выбранное изменение SPS и проверяет соответствующую категорию недоступных, неподдержанных
			// либо некорректных метаданных.

			// s содержит частный SPS до применения выбранного повреждения времени, размера
			// либо неподдержанного синтаксиса.
			s := contractSPSDefaults()
			tc.change(&s)
			// err содержит возвращённую категорию отказа восстановления для сравнения с
			// ожидаемой.
			_, err := fmp4.ResolveCodecConfig(contractVideoDescription(contractAVCC(s)), 1, 1)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ожидалась %v, получена %v", tc.want, err)
			}
		})
	}
}

// Проверяет отказ повреждённого avcC, длин SPS/PPS и синтаксиса SPS с категорией ErrInvalid.
func TestContractH264ConfigurationRejectsBrokenParameterSetFraming(t *testing.T) {
	// tc выбирает повреждение обрамления avcC или наборов SPS/PPS, которое должно дать ErrInvalid.
	for _, tc := range []struct {
		// name именует конкретный вид повреждения заголовков для подтеста.
		name string
		// change возвращает изменённые байты avcC, нарушающие выбранный размер, версию или
		// обязательный набор параметров.
		change func([]byte) []byte
	}{
		{"short-avcc", func(b []byte) []byte {
			// Обрезает avcC до семи байтов, обрывая двухбайтовое поле размера SPS.
			return b[:7]
		}},
		{"wrong-version", func(b []byte) []byte {
			// Задаёт неподдержанную версию avcC.
			b[0] = 0
			return b
		}},
		{"two-byte-nal-length", func(b []byte) []byte {
			// Задаёт двухбайтовое поле длины NAL вместо поддержанного четырёхбайтового.
			b[4] = 0xfd
			return b
		}},
		{"multiple-sps", func(b []byte) []byte {
			// Объявляет два SPS при единственном записанном наборе.
			b[5] = 0xe2
			return b
		}},
		{"zero-sps-size", func(b []byte) []byte {
			// Обнуляет объявленный размер SPS.
			b[6], b[7] = 0, 0
			return b
		}},
		{"oversized-sps", func(b []byte) []byte {
			// Объявляет размер SPS 65535, превышающий имеющиеся байты.
			b[6], b[7] = 0xff, 0xff
			return b
		}},
		{"wrong-sps-nalu", func(b []byte) []byte {
			// Меняет тип первого NAL с SPS на PPS.
			b[8] = 0x68
			return b
		}},
		{"truncated-sps-syntax", func(b []byte) []byte {
			// Возвращает avcC с однобайтовым SPS, не содержащим обязательного синтаксиса.
			return []byte{1, 100, 0, 31, 0xff, 0xe1, 0, 1, 0x67, 1, 0, 1, 0x68}
		}},
		{"missing-pps", func(b []byte) []byte {
			// Обнуляет число PPS после записанного SPS.
			// n содержит объявленную длину SPS для перехода к полю количества PPS.
			n := int(binary.BigEndian.Uint16(b[6:8]))
			b[8+n] = 0
			return b
		}},
		{"truncated-pps-size", func(b []byte) []byte {
			// Обрезает avcC после первого байта размера PPS.
			// n содержит длину SPS для усечения двухбайтового размера следующего PPS.
			n := int(binary.BigEndian.Uint16(b[6:8]))
			return b[:8+n+2]
		}},
		{"zero-pps-size", func(b []byte) []byte {
			// Обнуляет объявленный размер PPS после записанного SPS.
			// n содержит длину SPS для адресации и обнуления размера следующего PPS.
			n := int(binary.BigEndian.Uint16(b[6:8]))
			b[9+n], b[10+n] = 0, 0
			return b
		}},
		{"truncated-pps-body", func(b []byte) []byte {
			// Удаляет последний байт тела PPS.
			return b[:len(b)-1]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет ErrInvalid для выбранного повреждения обрамления avcC и наборов параметров.

			// desc содержит запись H.264 с выбранным повреждением заголовков avcC.
			desc := contractVideoDescription(tc.change(contractAVCC(contractSPSDefaults())))
			// err содержит результат восстановления повреждённого описания для проверки
			// категории ErrInvalid.
			_, err := fmp4.ResolveCodecConfig(desc, 1, 1)
			if !errors.Is(err, fmp4.ErrInvalid) {
				t.Fatalf("повреждённые headers приняты: %v", err)
			}
		})
	}
}

// Проверяет фактические AAC-LC с одним или двумя каналами из ASC и отказ для усечения, AAC Main и
// многоканального описания без закрепления категории отказа.
func TestContractAACResolverUsesActualASCAndRejectsUnsupportedDescriptions(t *testing.T) {
	// tc выбирает поддержанное либо отклоняемое описание AAC и ожидаемые параметры принятого
	// формата.
	for _, tc := range []struct {
		// name именует сочетание формата AAC и частоты для отдельного подтеста.
		name string
		// headers содержит синтетический AudioSpecificConfig либо его неподдержанное или
		// усечённое представление.
		headers []byte
		// rate задаёт частоту дискретизации в герцах, объявляемую в записи MP4 и ожидаемую после
		// восстановления.
		rate uint32
		// channels задаёт объявленное и ожидаемое число аудиоканалов.
		channels uint16
		// layout задаёт ожидаемую схему каналов только для принимаемых сценариев.
		layout string
		// reject требует отказа восстановления без закрепления конкретной категории ошибки AAC.
		reject bool
	}{
		{"mono-44100", []byte{0x12, 0x08}, 44100, 1, "mono", false},
		{"stereo-48000", []byte{0x11, 0x90}, 48000, 2, "stereo", false},
		{"truncated", []byte{0x11}, 48000, 2, "", true},
		{"aac-main", []byte{0x09, 0x90}, 48000, 2, "", true},
		{"multichannel", []byte{0x11, 0xb0}, 48000, 6, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет точное описание поддержанного AAC LC по ASC либо отказ для выбранного обрезанного или
			// неподдержанного описания.

			// desc содержит запись аудиодорожки с выбранными заголовками, частотой, каналами
			// и шкалой.
			desc := fmp4.TrackDescription{Track: media.TrackAudio, Codec: core.CodecAAC, SampleRateHz: tc.rate, Channels: tc.channels, Headers: tc.headers, TimeBase: media.TimeBase{Numerator: 1, Denominator: tc.rate}}
			// c содержит результат восстановления AAC; err сохраняет отказ, который
			// проверяется согласно принимаемому либо отклоняемому сценарию.
			c, err := fmp4.ResolveCodecConfig(desc, 8, 4)
			if tc.reject {
				// Контракт требует отказа, но не закрепляет категорию ошибки неподдержанного описания AAC.
				if err == nil || c.Validate() == nil {
					t.Fatalf("неподдержанное описание AAC принято: %+v, %v", c.Info(), err)
				}
				return
			}
			if err != nil || c.Info().Audio != (core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: tc.rate, Channels: tc.channels, ChannelLayout: tc.layout}) || c.Info().Revision != 4 {
				t.Fatalf("неверный AAC: %+v, %v", c.Info(), err)
			}
		})
	}
}
