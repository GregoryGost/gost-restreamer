package core_test

import (
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// desiredBoundaryProfile создаёт полный AV1/SVT VBR с несокращённым FPS для проверки строк.
func desiredBoundaryProfile() core.GroupProfile {
	// p содержит самостоятельные текущие настройки без неявных исторических ограничений.
	p := encodedProfile(core.VideoEncoderSVTAV1, core.RateControlVBR)
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 60000, Denominator: 2002}
	p.Format.Video.Color.Range = "tv"
	return p
}

// TestDesiredStringsRejectUnsupportedTuples проверяет закрытые строки
// и несовместимые пары полного Encoding. t сообщает о нарушениях контракта.
func TestDesiredStringsRejectUnsupportedTuples(t *testing.T) {
	// cases задаёт отдельные мутации корректного профиля; change получает изменяемую копию p.
	cases := []struct {
		// name называет нарушаемое поле или сочетание.
		name string
		// change вносит только проверяемое нарушение в профиль p.
		change func(p *core.GroupProfile)
	}{
		// p получает transcode при сохранённом видеокодеке AV1.
		{"mode_codec", func(p *core.GroupProfile) { p.Mode = core.ModeTranscode }},
		// В p профиль AV1 меняется с main на неприменимый high.
		{"video_profile", func(p *core.GroupProfile) { p.Format.Video.CodecProfile = "high" }},
		// В p профиль AV1 меняется на MAIN для проверки точного регистра.
		{"video_profile_case", func(p *core.GroupProfile) { p.Format.Video.CodecProfile = "MAIN" }},
		// В p уровень видео меняется на неподдержанный 7.4.
		{"video_level", func(p *core.GroupProfile) { p.Format.Video.Level = "7.4" }},
		// В p категория AV1 меняется на high без остальных необходимых настроек.
		{"video_tier", func(p *core.GroupProfile) { p.Format.Video.Tier = "high" }},
		// В p пиксели меняются на yuv420p10le при сохранённой восьмибитной глубине.
		{"pixel_format", func(p *core.GroupProfile) { p.Format.Video.PixelFormat = "yuv420p10le" }},
		// В p глубина меняется на 10 бит при сохранённых пикселях yuv420p.
		{"bit_depth", func(p *core.GroupProfile) { p.Format.Video.BitDepth = 10 }},
		// В p диапазон цвета меняется на запрещённое в желаемых настройках значение unspecified.
		{"range", func(p *core.GroupProfile) { p.Format.Video.Color.Range = "unspecified" }},
		// В p первичные координаты цвета меняются на неизвестное значение future.
		{"primaries", func(p *core.GroupProfile) { p.Format.Video.Color.Primaries = "future" }},
		// В p передаточная характеристика меняется на PQ без десятибитного HDR-формата.
		{"transfer", func(p *core.GroupProfile) { p.Format.Video.Color.Transfer = "smpte2084" }},
		// В p матрица цвета меняется на неизвестное значение future.
		{"matrix", func(p *core.GroupProfile) { p.Format.Video.Color.Matrix = "future" }},
		// В p профиль AAC меняется с LC на lc для проверки точного регистра.
		{"audio_profile", func(p *core.GroupProfile) { p.Format.Audio.CodecProfile = "lc" }},
		// В p раскладка звука меняется на 5.1 без согласования числа каналов.
		{"layout", func(p *core.GroupProfile) { p.Format.Audio.ChannelLayout = "5.1" }},
		// В p число аудиоканалов меняется на 1 при сохранённом stereo.
		{"layout_channels", func(p *core.GroupProfile) { p.Format.Audio.Channels = 1 }},
		// В p число аудиоканалов меняется на 6 при сохранённом stereo.
		{"channels", func(p *core.GroupProfile) { p.Format.Audio.Channels = 6 }},
		// В p обработка LIVE-аудио меняется на transcode при видео copy.
		{"live_audio", func(p *core.GroupProfile) { p.LiveAudioMode = core.LiveAudioTranscode }},
		// В p уровень видео заменяется строкой с неверным UTF-8.
		{"invalid_utf8", func(p *core.GroupProfile) { p.Format.Video.Level = string([]byte{0xff}) }},
		// p получает H.264 baseline с одним B-кадром и NVENC/VBR.
		{"baseline_b", func(p *core.GroupProfile) {
			p.Mode, p.Format.Video.Codec, p.Format.Video.CodecProfile = core.ModeTranscode, core.CodecH264, "baseline"
			p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderNVENC, core.RateControlVBR)
			p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits, p.VideoEncoding.MaxBFrames = p.VideoEncoding.TargetBitrateBPS, 12000000, 1
		}},
	}
	// scenario выбирает одно независимое нарушение; callback проверяет ошибку без изменения исходного рецепта.
	for _, scenario := range cases {
		// t сообщает об ошибках проверяемого сценария.
		t.Run(scenario.name, func(t *testing.T) {
			// profile содержит отдельную копию допустимого текущего рецепта.
			profile := desiredBoundaryProfile()
			scenario.change(&profile)
			// err должен относиться к профилю независимо от ветви Encoding.
			err := profile.Validate()
			if !errors.Is(err, core.ErrInvalidProfile) {
				t.Fatalf("Неподдержанное desired прошло профильную проверку: %v", err)
			}
		})
	}
}

// TestClosedDesiredPreservesExactNumbersAndLevelAliases проверяет, что
// чтение сохраняет выбранный уровень и точную дробь полного профиля.
// t регистрирует отказ допустимого рецепта или потерю точных значений.
func TestClosedDesiredPreservesExactNumbersAndLevelAliases(t *testing.T) {
	// mode выбирает два существующих сочетания видеорежима и кодека.
	for _, mode := range []core.GroupMode{core.ModeCopy, core.ModeTranscode} {
		// levels перечисляет контрактные псевдонимы и границы выбранного кодека.
		levels := []string{"", "auto", "2.0", "7.3"}
		if mode == core.ModeTranscode {
			levels = []string{"", "auto", "1", "1.0", "1b", "1.0b", "2", "2.0", "3", "4", "5", "5.2"}
		}
		// level сохраняется побайтно после кодирования; callback проверяет полный профиль.
		for _, level := range levels {
			// t сообщает об ошибках проверяемого сценария.
			t.Run(string(mode)+"/"+level, func(t *testing.T) {
				// profile задаёт текущий рецепт с допустимым псевдонимом уровня.
				profile := desiredBoundaryProfile()
				profile.Mode, profile.Format.Video.Level = mode, level
				if mode == core.ModeTranscode {
					profile.Format.Video.Codec, profile.Format.Video.CodecProfile = core.CodecH264, "high"
					profile.Encoding = core.DefaultEncoding(mode, core.VideoEncoderNVENC, core.RateControlVBR)
					profile.VideoEncoding.MaxBitrateBPS, profile.VideoEncoding.BufferBits = profile.VideoEncoding.TargetBitrateBPS, 12000000
				}
				// payload содержит внутреннее представление; err сообщает о недопустимом отклонении рецепта.
				payload, err := core.EncodeProfile(profile)
				if err != nil {
					t.Fatal(err)
				}
				// restored должен сохранить каждое число, псевдоним и полный Encoding.
				restored, err := core.DecodeProfile(payload)
				if err != nil || restored != profile {
					t.Fatalf("Полный рецепт изменён: %+v, %v", restored, err)
				}
			})
		}
	}
}
