package core_test

import (
	"errors"
	"math"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// desiredBoundaryProfile возвращает прежний рецепт с числами вне матрицы v2.
// Такой рецепт отделяет новые строковые ограничения от прежних числовых контрактов.
func desiredBoundaryProfile() core.GroupProfile {
	return core.GroupProfile{
		Mode: core.ModeCopy,
		Format: core.MediaFormat{
			Video: core.VideoFormat{Codec: core.CodecAV1, Width: 641, Height: 359,
				FrameRate:    core.FrameRate{Numerator: 60000, Denominator: 2002},
				CodecProfile: "main", PixelFormat: "yuv420p", BitDepth: 8,
				Color: core.ColorInfo{Range: "tv", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}},
			Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 12345, Channels: 2, ChannelLayout: "stereo"},
		},
		VideoEncoding: core.VideoEncoding{TargetBitrateBPS: math.MaxUint64 - 1, MaxBitrateBPS: math.MaxUint64, BufferBits: math.MaxUint64, GOPFrames: 17, MaxBFrames: 19},
		AudioEncoding: core.AudioEncoding{BitrateBPS: 9007199254740993},
	}
}

// TestDesiredStringsCannotUseLegacyEncodingBypass проверяет закрытые строки
// и несовместимые пары при нулевом Encoding. t сообщает о нарушениях контракта.
func TestDesiredStringsCannotUseLegacyEncodingBypass(t *testing.T) {
	// cases задаёт отдельные мутации корректного профиля; change получает изменяемую копию p.
	cases := []struct {
		// name называет нарушаемое поле или сочетание.
		name string
		// change вносит только проверяемое нарушение в профиль p.
		change func(p *core.GroupProfile)
	}{
		{"mode_codec", func(p *core.GroupProfile) { p.Mode = core.ModeTranscode }},
		{"video_profile", func(p *core.GroupProfile) { p.Format.Video.CodecProfile = "high" }},
		{"video_profile_case", func(p *core.GroupProfile) { p.Format.Video.CodecProfile = "MAIN" }},
		{"video_level", func(p *core.GroupProfile) { p.Format.Video.Level = "7.4" }},
		{"video_tier", func(p *core.GroupProfile) { p.Format.Video.Tier = "high" }},
		{"pixel_format", func(p *core.GroupProfile) { p.Format.Video.PixelFormat = "yuv420p10le" }},
		{"bit_depth", func(p *core.GroupProfile) { p.Format.Video.BitDepth = 10 }},
		{"range", func(p *core.GroupProfile) { p.Format.Video.Color.Range = "unspecified" }},
		{"primaries", func(p *core.GroupProfile) { p.Format.Video.Color.Primaries = "bt2020" }},
		{"transfer", func(p *core.GroupProfile) { p.Format.Video.Color.Transfer = "smpte2084" }},
		{"matrix", func(p *core.GroupProfile) { p.Format.Video.Color.Matrix = "bt2020nc" }},
		{"audio_profile", func(p *core.GroupProfile) { p.Format.Audio.CodecProfile = "lc" }},
		{"layout", func(p *core.GroupProfile) { p.Format.Audio.ChannelLayout = "5.1" }},
		{"layout_channels", func(p *core.GroupProfile) { p.Format.Audio.Channels = 1 }},
		{"channels", func(p *core.GroupProfile) { p.Format.Audio.Channels = 6 }},
		{"live_audio", func(p *core.GroupProfile) { p.LiveAudioMode = core.LiveAudioTranscode }},
		{"invalid_utf8", func(p *core.GroupProfile) { p.Format.Video.Level = string([]byte{0xff}) }},
		{"baseline_b", func(p *core.GroupProfile) {
			p.Mode, p.Format.Video.Codec, p.Format.Video.CodecProfile = core.ModeTranscode, core.CodecH264, "baseline"
		}},
	}
	// scenario выбирает одно независимое нарушение; callback проверяет ошибку без изменения исходного рецепта.
	for _, scenario := range cases {
		t.Run(scenario.name, func(t *testing.T) {
			// profile содержит отдельную копию допустимого legacy-рецепта.
			profile := desiredBoundaryProfile()
			scenario.change(&profile)
			// err должен относиться к профилю независимо от ветви Encoding.
			err := profile.Validate()
			if !errors.Is(err, core.ErrInvalidProfile) {
				t.Fatalf("Неподдержанное desired прошло legacy-проверку: %v", err)
			}
		})
	}
}

// TestClosedDesiredPreservesLegacyNumbersAndLevelAliases проверяет, что
// строгие строки не материализуют Encoding и не применяют числовые пределы v2.
// t регистрирует отказ допустимого рецепта или потерю точных значений.
func TestClosedDesiredPreservesLegacyNumbersAndLevelAliases(t *testing.T) {
	// mode выбирает два существующих сочетания видеорежима и кодека.
	for _, mode := range []core.GroupMode{core.ModeCopy, core.ModeTranscode} {
		// levels перечисляет контрактные псевдонимы и границы выбранного кодека.
		levels := []string{"", "auto", "2.0", "7.3"}
		if mode == core.ModeTranscode {
			levels = []string{"", "auto", "1", "1.0", "1b", "1.0b", "2", "2.0", "3", "4", "5", "5.2"}
		}
		// level сохраняется побайтно после кодирования; callback проверяет полный профиль.
		for _, level := range levels {
			t.Run(string(mode)+"/"+level, func(t *testing.T) {
				// profile задаёт прежний рецепт с допустимым псевдонимом уровня.
				profile := desiredBoundaryProfile()
				profile.Mode, profile.Format.Video.Level = mode, level
				if mode == core.ModeTranscode {
					profile.Format.Video.Codec, profile.Format.Video.CodecProfile = core.CodecH264, "high"
				}
				// payload содержит внутреннее представление; err сообщает о недопустимом отклонении рецепта.
				payload, err := core.EncodeProfile(profile)
				if err != nil {
					t.Fatal(err)
				}
				// restored должен сохранить каждое число, псевдоним и нулевой Encoding.
				restored, err := core.DecodeProfile(payload)
				if err != nil || restored != profile {
					t.Fatalf("Прежний рецепт изменён: %+v, %v", restored, err)
				}
			})
		}
	}
}
