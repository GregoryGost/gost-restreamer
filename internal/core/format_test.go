package core_test

import (
	"errors"
	"math"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Создаёт самостоятельный допустимый прежний профиль AV1/AAC без блока Encoding для доменных проверок.
func validProfile() core.GroupProfile {
	return core.GroupProfile{
		Mode: core.ModeCopy,
		Format: core.MediaFormat{
			Video: core.VideoFormat{
				Codec: core.CodecAV1, Width: 2560, Height: 1440,
				FrameRate:    core.FrameRate{Numerator: 60, Denominator: 1},
				CodecProfile: "main", PixelFormat: "yuv420p", BitDepth: 8,
				Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"},
			},
			Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"},
		},
		VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 6_000_000, MaxBitrateBPS: 8_000_000, BufferBits: 12_000_000, GOPFrames: 120, ClosedGOP: true},
		AudioEncoding: core.AudioEncoding{BitrateBPS: 192_000},
	}
}

// Проверяет категорию ошибки через errors.Is и наличие дополнительного контекста поля.
func requireCategory(t *testing.T, err, category error) {
	t.Helper()
	if !errors.Is(err, category) {
		t.Fatalf("ошибка = %v, нужна категория %v", err, category)
	}
	if err.Error() == category.Error() {
		t.Errorf("ошибка %v не содержит контекста поля", err)
	}
}

// Проверяет точные строковые значения кодеков и режимов группы.
func TestContractValues(t *testing.T) {
	if core.CodecAV1 != "av1" || core.CodecH264 != "h264" || core.CodecAAC != "aac" || core.ModeCopy != "copy" || core.ModeTranscode != "transcode" {
		t.Fatal("изменились строковые значения контрактов")
	}
}

// Проверяет симметричное равенство дробных частот, отклонение нулевых значений и отсутствие коллизии
// при больших числителях.
func TestFrameRateMathematicalEquality(t *testing.T) {
	// cases перечисляет сценарии проверки допустимости или различимости полей профиля.
	cases := []struct {
		// name задаёт имя текущего сценария проверки поля профиля.
		name string
		// a задаёт левую частоту кадров, а b — правую для математического сравнения дробей.
		a, b core.FrameRate
		// want задаёт ожидаемое математическое равенство двух частот.
		want bool
	}{
		{"reduced", core.FrameRate{Numerator: 60, Denominator: 1}, core.FrameRate{Numerator: 120, Denominator: 2}, true},
		{"fractional", core.FrameRate{Numerator: 30000, Denominator: 1001}, core.FrameRate{Numerator: 60000, Denominator: 2002}, true},
		{"different", core.FrameRate{Numerator: 30000, Denominator: 1001}, core.FrameRate{Numerator: 30, Denominator: 1}, false},
		{"large_equal", core.FrameRate{Numerator: math.MaxUint32, Denominator: math.MaxUint32}, core.FrameRate{Numerator: 1, Denominator: 1}, true},
		// Перекрёстные произведения совпадут при усечении до uint32.
		{"overflow_collision", core.FrameRate{Numerator: math.MaxUint32, Denominator: 1}, core.FrameRate{Numerator: 1, Denominator: math.MaxUint32}, false},
		{"zero", core.FrameRate{}, core.FrameRate{}, false},
		{"zero_numerator", core.FrameRate{Numerator: 0, Denominator: 1}, core.FrameRate{Numerator: 1, Denominator: 1}, false},
		{"zero_denominator", core.FrameRate{Numerator: 1, Denominator: 0}, core.FrameRate{Numerator: 1, Denominator: 1}, false},
	}
	// tc содержит параметры и ожидаемый результат текущего сценария.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет симметричность математического равенства FPS для равных дробей, различий, нулей и случая
			// переполнения uint32.

			// got содержит фактический результат сравнения двух частот кадров.
			if got := tc.a.Equal(tc.b); got != tc.want || tc.b.Equal(tc.a) != tc.want {
				t.Fatalf("Equal(%v, %v) = %v, нужно %v в обоих направлениях", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// Проверяет отказ для незаданных или некорректных обязательных полей дорожек и исключение таких
// форматов из равенства.
func TestFormatRequiredFields(t *testing.T) {
	// cases перечисляет сценарии проверки допустимости или различимости полей профиля.
	cases := []struct {
		// name задаёт имя текущего сценария проверки поля профиля.
		name string
		// mutate вносит выбранное нарушение или различие в копию профиля или медиаформата.
		mutate func(*core.MediaFormat)
	}{
		{"video_codec_empty", func(f *core.MediaFormat) {
			// Убирает обязательный видеокодек.
			f.Video.Codec = ""
		}},
		{"video_codec_audio", func(f *core.MediaFormat) {
			// Подставляет аудиокодек AAC вместо видеокодека.
			f.Video.Codec = core.CodecAAC
		}},
		{"video_codec_unknown", func(f *core.MediaFormat) {
			// Подставляет неподдерживаемый видеокодек vp9.
			f.Video.Codec = "vp9"
		}},
		{"width", func(f *core.MediaFormat) {
			// Обнуляет обязательную ширину кадра.
			f.Video.Width = 0
		}},
		{"height", func(f *core.MediaFormat) {
			// Обнуляет обязательную высоту кадра.
			f.Video.Height = 0
		}},
		{"rate_numerator", func(f *core.MediaFormat) {
			// Обнуляет числитель FPS.
			f.Video.FrameRate.Numerator = 0
		}},
		{"rate_denominator", func(f *core.MediaFormat) {
			// Обнуляет знаменатель FPS.
			f.Video.FrameRate.Denominator = 0
		}},
		{"video_profile", func(f *core.MediaFormat) {
			// Убирает обязательный профиль видеокодека.
			f.Video.CodecProfile = ""
		}},
		{"video_profile_whitespace", func(f *core.MediaFormat) {
			// Заменяет профиль видеокодека одними пробельными символами.
			f.Video.CodecProfile = " \t\n"
		}},
		{"pixel_format", func(f *core.MediaFormat) {
			// Убирает обязательный PixelFormat.
			f.Video.PixelFormat = ""
		}},
		{"pixel_format_whitespace", func(f *core.MediaFormat) {
			// Заменяет PixelFormat одним пробельным символом Unicode.
			f.Video.PixelFormat = "\u2003"
		}},
		{"bit_depth", func(f *core.MediaFormat) {
			// Обнуляет глубину цвета.
			f.Video.BitDepth = 0
		}},
		{"range", func(f *core.MediaFormat) {
			// Убирает обязательный диапазон цвета.
			f.Video.Color.Range = ""
		}},
		{"primaries", func(f *core.MediaFormat) {
			// Заменяет цветовые первичные координаты табуляцией.
			f.Video.Color.Primaries = "\t"
		}},
		{"transfer", func(f *core.MediaFormat) {
			// Убирает обязательную функцию переноса цвета.
			f.Video.Color.Transfer = ""
		}},
		{"matrix", func(f *core.MediaFormat) {
			// Заменяет цветовую матрицу пробелом.
			f.Video.Color.Matrix = " "
		}},
		{"audio_codec_empty", func(f *core.MediaFormat) {
			// Убирает обязательный аудиокодек.
			f.Audio.Codec = ""
		}},
		{"audio_codec_video", func(f *core.MediaFormat) {
			// Подставляет видеокодек AV1 вместо аудиокодека.
			f.Audio.Codec = core.CodecAV1
		}},
		{"audio_codec_unknown", func(f *core.MediaFormat) {
			// Подставляет неподдерживаемый аудиокодек opus.
			f.Audio.Codec = "opus"
		}},
		{"audio_profile", func(f *core.MediaFormat) {
			// Убирает обязательный профиль аудиокодека.
			f.Audio.CodecProfile = ""
		}},
		{"audio_profile_whitespace", func(f *core.MediaFormat) {
			// Заменяет профиль аудиокодека табуляцией.
			f.Audio.CodecProfile = "\t"
		}},
		{"sample_rate", func(f *core.MediaFormat) {
			// Обнуляет частоту дискретизации.
			f.Audio.SampleRateHz = 0
		}},
		{"channels", func(f *core.MediaFormat) {
			// Обнуляет число аудиоканалов.
			f.Audio.Channels = 0
		}},
		{"layout", func(f *core.MediaFormat) {
			// Убирает обязательную раскладку аудиоканалов.
			f.Audio.ChannelLayout = ""
		}},
		{"layout_whitespace", func(f *core.MediaFormat) {
			// Заменяет раскладку аудиоканалов пробельными символами.
			f.Audio.ChannelLayout = " \n"
		}},
		{"missing_audio", func(f *core.MediaFormat) {
			// Убирает весь обязательный аудиоформат.
			f.Audio = core.AudioFormat{}
		}},
		{"missing_video", func(f *core.MediaFormat) {
			// Убирает весь обязательный видеоформат.
			f.Video = core.VideoFormat{}
		}},
	}
	// tc содержит параметры и ожидаемый результат текущего сценария.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет ErrInvalidFormat для выбранного нарушения и исключает равенство некорректного формата
			// самому себе или корректному формату.

			// f содержит отдельную копию медиаформата для проверки обязательных полей.
			f := validProfile().Format
			tc.mutate(&f)
			requireCategory(t, f.Validate(), core.ErrInvalidFormat)
			if f.Equal(f) || f.Equal(validProfile().Format) || validProfile().Format.Equal(f) {
				t.Fatal("некорректный формат участвует в равенстве")
			}
		})
	}
	// v содержит очередной нулевой структурный формат, который обязан быть отклонён.
	for _, v := range []interface {
		// Validate проверяет обязательные поля структурного формата, сохраняя категорию ErrInvalidFormat.
		Validate() error
	}{core.FrameRate{}, core.ColorInfo{}, core.VideoFormat{}, core.AudioFormat{}, core.MediaFormat{}} {
		requireCategory(t, v.Validate(), core.ErrInvalidFormat)
	}
}

// Проверяет сохранение произвольных структурно допустимых описаний и полного диапазона прежних
// параметров без ограничений конкретного адаптера.
func TestStructuralValidationPreservesDescriptions(t *testing.T) {
	// codec выбирает поддерживаемый видеокодек для структурной проверки.
	for _, codec := range []core.Codec{core.CodecAV1, core.CodecH264} {
		// p содержит отдельный полный профиль текущего сценария проверки.
		p := validProfile()
		p.Format.Video.Codec = codec
		p.Format.Video.CodecProfile = " future-profile "
		p.Format.Video.Level = "future-level"
		p.Format.Video.Tier = "future-tier"
		p.Format.Video.PixelFormat = "future-pixel-format"
		p.Format.Audio.ChannelLayout = "future-layout"
		// before сохраняет исходный профиль для проверки отсутствия изменения.
		before := p
		// err сообщает об ошибке проверки допустимости профиля.
		if err := p.Validate(); err != nil {
			t.Fatalf("структурный профиль %s отклонён: %v", codec, err)
		}
		if p != before {
			t.Fatal("валидация изменила описание")
		}
	}
	// p содержит отдельный полный профиль текущего сценария проверки.
	p := validProfile()
	p.Mode = core.ModeTranscode
	p.VideoEncoding.TargetBitrateBPS = 1
	p.VideoEncoding.MaxBitrateBPS = 1
	p.VideoEncoding.BufferBits = 1
	p.VideoEncoding.GOPFrames = 1
	p.VideoEncoding.MaxBFrames = 0
	p.VideoEncoding.ClosedGOP = false
	p.AudioEncoding.BitrateBPS = 1
	// err сообщает об ошибке проверки допустимости профиля.
	if err := p.Validate(); err != nil {
		t.Fatalf("положительная граница отклонена: %v", err)
	}
	p.Format.Video.Width, p.Format.Video.Height = 1, 1
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 1, Denominator: 1}
	p.Format.Video.BitDepth = 1
	p.Format.Audio.SampleRateHz, p.Format.Audio.Channels = 1, 1
	// err сообщает об ошибке проверки допустимости профиля.
	if err := p.Validate(); err != nil {
		t.Fatalf("минимальные положительные размеры формата отклонены: %v", err)
	}
	p.Format.Video.Width, p.Format.Video.Height = math.MaxUint32, math.MaxUint32
	p.Format.Video.BitDepth = math.MaxUint8
	p.Format.Audio.SampleRateHz, p.Format.Audio.Channels = math.MaxUint32, math.MaxUint16
	p.VideoEncoding.TargetBitrateBPS, p.VideoEncoding.MaxBitrateBPS = math.MaxUint64, math.MaxUint64
	p.VideoEncoding.BufferBits = math.MaxUint64
	p.VideoEncoding.GOPFrames, p.VideoEncoding.MaxBFrames = math.MaxUint32, math.MaxUint32
	p.AudioEncoding.BitrateBPS = math.MaxUint64
	// err сообщает об ошибке проверки допустимости профиля.
	if err := p.Validate(); err != nil {
		t.Fatalf("верхние границы типов не должны вводить ограничений адаптера: %v", err)
	}
}

// Проверяет категорию и контекст ошибок режима, кодирования и вложенного формата, а также неравенство
// некорректных профилей.
func TestProfileValidationCategories(t *testing.T) {
	// cases перечисляет сценарии проверки допустимости или различимости полей профиля.
	cases := []struct {
		// name задаёт имя текущего сценария проверки поля профиля.
		name string
		// mutate вносит выбранное нарушение или различие в копию профиля или медиаформата.
		mutate func(*core.GroupProfile)
	}{
		{"mode_empty", func(p *core.GroupProfile) {
			// Убирает обязательный режим профиля.
			p.Mode = ""
		}},
		{"mode_unknown", func(p *core.GroupProfile) {
			// Подставляет неизвестный режим профиля other.
			p.Mode = "other"
		}},
		{"target_zero", func(p *core.GroupProfile) {
			// Обнуляет целевой видеобитрейт.
			p.VideoEncoding.TargetBitrateBPS = 0
		}},
		{"max_zero", func(p *core.GroupProfile) {
			// Обнуляет максимальный видеобитрейт.
			p.VideoEncoding.MaxBitrateBPS = 0
		}},
		{"target_exceeds_max", func(p *core.GroupProfile) {
			// Поднимает целевой битрейт на единицу выше максимального 8000000.
			p.VideoEncoding.TargetBitrateBPS = 8_000_001
		}},
		{"buffer_zero", func(p *core.GroupProfile) {
			// Обнуляет размер видеобуфера.
			p.VideoEncoding.BufferBits = 0
		}},
		{"gop_zero", func(p *core.GroupProfile) {
			// Обнуляет длину GOP.
			p.VideoEncoding.GOPFrames = 0
		}},
		{"audio_bitrate_zero", func(p *core.GroupProfile) {
			// Обнуляет аудиобитрейт.
			p.AudioEncoding.BitrateBPS = 0
		}},
		{"nested_format", func(p *core.GroupProfile) {
			// Обнуляет число каналов вложенного аудиоформата.
			p.Format.Audio.Channels = 0
		}},
	}
	// tc содержит параметры и ожидаемый результат текущего сценария.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет ErrInvalidProfile для выбранного нарушения и исключает равенство некорректного профиля
			// самому себе или корректному профилю.

			// p содержит отдельный полный профиль текущего сценария проверки.
			p := validProfile()
			tc.mutate(&p)
			requireCategory(t, p.Validate(), core.ErrInvalidProfile)
			if p.Equal(p) || p.Equal(validProfile()) || validProfile().Equal(p) {
				t.Fatal("некорректный профиль участвует в равенстве")
			}
		})
	}
	// v содержит очередную нулевую настройку или профиль, которые обязаны быть отклонены.
	for _, v := range []interface {
		// Validate проверяет настройки кодирования либо полный профиль, сохраняя категорию ErrInvalidProfile.
		Validate() error
	}{core.VideoEncoding{}, core.AudioEncoding{}, core.GroupProfile{}} {
		requireCategory(t, v.Validate(), core.ErrInvalidProfile)
	}
}

// Проверяет участие всех прежних полей в равенстве профиля, математическое равенство FPS и сохранение
// точных пробелов строк.
func TestProfileEqualityAccountsForEveryField(t *testing.T) {
	// cases перечисляет сценарии проверки допустимости или различимости полей профиля.
	cases := []struct {
		// name задаёт имя текущего сценария проверки поля профиля.
		name string
		// mutate вносит выбранное нарушение или различие в копию профиля или медиаформата.
		mutate func(*core.GroupProfile)
	}{
		{"mode", func(p *core.GroupProfile) {
			// Меняет режим на ModeTranscode для проверки учёта поля в Equal.
			p.Mode = core.ModeTranscode
		}},
		{"video_codec", func(p *core.GroupProfile) {
			// Меняет видеокодек на CodecH264 для проверки учёта поля в Equal.
			p.Format.Video.Codec = core.CodecH264
		}},
		{"width", func(p *core.GroupProfile) {
			// Увеличивает ширину кадра для проверки учёта поля в Equal.
			p.Format.Video.Width++
		}},
		{"height", func(p *core.GroupProfile) {
			// Увеличивает высоту кадра для проверки учёта поля в Equal.
			p.Format.Video.Height++
		}},
		{"frame_rate", func(p *core.GroupProfile) {
			// Увеличивает числитель FPS для проверки учёта поля в Equal.
			p.Format.Video.FrameRate.Numerator++
		}},
		{"video_profile", func(p *core.GroupProfile) {
			// Меняет регистр профиля видеокодека на Main для проверки точного строкового сравнения.
			p.Format.Video.CodecProfile = "Main"
		}},
		{"level", func(p *core.GroupProfile) {
			// Меняет Level на 4.1 для проверки учёта поля в Equal.
			p.Format.Video.Level = "4.1"
		}},
		{"tier", func(p *core.GroupProfile) {
			// Меняет Tier на main для проверки учёта поля в Equal.
			p.Format.Video.Tier = "main"
		}},
		{"pixel_format", func(p *core.GroupProfile) {
			// Меняет PixelFormat на yuv420p10le для проверки учёта поля в Equal.
			p.Format.Video.PixelFormat = "yuv420p10le"
		}},
		{"bit_depth", func(p *core.GroupProfile) {
			// Меняет глубину цвета на 10 для проверки учёта поля в Equal.
			p.Format.Video.BitDepth = 10
		}},
		{"color_range", func(p *core.GroupProfile) {
			// Меняет диапазон цвета на full для проверки учёта поля в Equal.
			p.Format.Video.Color.Range = "full"
		}},
		{"color_primaries", func(p *core.GroupProfile) {
			// Меняет цветовые первичные координаты на bt2020 для проверки учёта поля в Equal.
			p.Format.Video.Color.Primaries = "bt2020"
		}},
		{"color_transfer", func(p *core.GroupProfile) {
			// Меняет функцию переноса на smpte2084 для проверки учёта поля в Equal.
			p.Format.Video.Color.Transfer = "smpte2084"
		}},
		{"color_matrix", func(p *core.GroupProfile) {
			// Меняет цветовую матрицу на bt2020nc для проверки учёта поля в Equal.
			p.Format.Video.Color.Matrix = "bt2020nc"
		}},
		{"audio_profile", func(p *core.GroupProfile) {
			// Меняет аудиопрофиль на HE для проверки учёта поля в Equal.
			p.Format.Audio.CodecProfile = "HE"
		}},
		{"sample_rate", func(p *core.GroupProfile) {
			// Меняет частоту дискретизации на 44100 Гц для проверки учёта поля в Equal.
			p.Format.Audio.SampleRateHz = 44100
		}},
		{"channels", func(p *core.GroupProfile) {
			// Меняет число аудиоканалов на 1 для проверки учёта поля в Equal.
			p.Format.Audio.Channels = 1
		}},
		{"channel_layout", func(p *core.GroupProfile) {
			// Меняет раскладку каналов на mono для проверки учёта поля в Equal.
			p.Format.Audio.ChannelLayout = "mono"
		}},
		{"target_bitrate", func(p *core.GroupProfile) {
			// Увеличивает целевой видеобитрейт для проверки учёта поля в Equal.
			p.VideoEncoding.TargetBitrateBPS++
		}},
		{"max_bitrate", func(p *core.GroupProfile) {
			// Увеличивает максимальный видеобитрейт для проверки учёта поля в Equal.
			p.VideoEncoding.MaxBitrateBPS++
		}},
		{"buffer", func(p *core.GroupProfile) {
			// Увеличивает размер видеобуфера для проверки учёта поля в Equal.
			p.VideoEncoding.BufferBits++
		}},
		{"gop", func(p *core.GroupProfile) {
			// Увеличивает длину GOP для проверки учёта поля в Equal.
			p.VideoEncoding.GOPFrames++
		}},
		{"b_frames", func(p *core.GroupProfile) {
			// Меняет число B-кадров на 2 для проверки учёта поля в Equal.
			p.VideoEncoding.MaxBFrames = 2
		}},
		{"closed_gop", func(p *core.GroupProfile) {
			// Отключает закрытый GOP для проверки учёта поля в Equal.
			p.VideoEncoding.ClosedGOP = false
		}},
		{"audio_bitrate", func(p *core.GroupProfile) {
			// Увеличивает аудиобитрейт для проверки учёта поля в Equal.
			p.AudioEncoding.BitrateBPS++
		}},
	}
	// a задаёт исходный корректный профиль для сравнения.
	a := validProfile()
	if !a.Equal(a) || !a.Format.Equal(a.Format) {
		t.Fatal("корректное значение не равно себе")
	}
	// tc содержит параметры и ожидаемый результат текущего сценария.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет, что выбранное допустимое изменение различается обоими направлениями Equal профиля и, при
			// изменении формата, его Equal.

			// b содержит копию исходного профиля с выбранным изменением.
			b := a
			tc.mutate(&b)
			// err сообщает об ошибке проверки допустимости профиля.
			if err := b.Validate(); err != nil {
				t.Fatalf("проверка равенства получила некорректный вход: %v", err)
			}
			if a.Equal(b) || b.Equal(a) {
				t.Fatal("различие поля не учтено")
			}
			if a.Format != b.Format && (a.Format.Equal(b.Format) || b.Format.Equal(a.Format)) {
				t.Fatal("различие формата не учтено")
			}
		})
	}
	// b содержит копию исходного профиля с выбранным изменением.
	b := a
	b.Format.Video.FrameRate = core.FrameRate{Numerator: 120, Denominator: 2}
	if !a.Equal(b) || !b.Equal(a) || !a.Format.Equal(b.Format) {
		t.Fatal("полный профиль не учитывает математическое равенство частоты")
	}
	b = a
	b.Format.Video.CodecProfile = " main "
	if a.Equal(b) || a.Format.Equal(b.Format) {
		t.Fatal("равенство автоматически удалило пробелы из описания")
	}
}
