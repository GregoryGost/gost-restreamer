package core_test

import (
	"errors"
	"math"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

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

func requireCategory(t *testing.T, err, category error) {
	t.Helper()
	if !errors.Is(err, category) {
		t.Fatalf("ошибка = %v, нужна категория %v", err, category)
	}
	if err.Error() == category.Error() {
		t.Errorf("ошибка %v не содержит контекста поля", err)
	}
}

func TestContractValues(t *testing.T) {
	if core.CodecAV1 != "av1" || core.CodecH264 != "h264" || core.CodecAAC != "aac" || core.ModeCopy != "copy" || core.ModeTranscode != "transcode" {
		t.Fatal("изменились строковые значения контрактов")
	}
}

func TestFrameRateMathematicalEquality(t *testing.T) {
	cases := []struct {
		name string
		a, b core.FrameRate
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
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Equal(tc.b); got != tc.want || tc.b.Equal(tc.a) != tc.want {
				t.Fatalf("Equal(%v, %v) = %v, нужно %v в обоих направлениях", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestFormatRequiredFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*core.MediaFormat)
	}{
		{"video_codec_empty", func(f *core.MediaFormat) { f.Video.Codec = "" }},
		{"video_codec_audio", func(f *core.MediaFormat) { f.Video.Codec = core.CodecAAC }},
		{"video_codec_unknown", func(f *core.MediaFormat) { f.Video.Codec = "vp9" }},
		{"width", func(f *core.MediaFormat) { f.Video.Width = 0 }},
		{"height", func(f *core.MediaFormat) { f.Video.Height = 0 }},
		{"rate_numerator", func(f *core.MediaFormat) { f.Video.FrameRate.Numerator = 0 }},
		{"rate_denominator", func(f *core.MediaFormat) { f.Video.FrameRate.Denominator = 0 }},
		{"video_profile", func(f *core.MediaFormat) { f.Video.CodecProfile = "" }},
		{"video_profile_whitespace", func(f *core.MediaFormat) { f.Video.CodecProfile = " \t\n" }},
		{"pixel_format", func(f *core.MediaFormat) { f.Video.PixelFormat = "" }},
		{"pixel_format_whitespace", func(f *core.MediaFormat) { f.Video.PixelFormat = "\u2003" }},
		{"bit_depth", func(f *core.MediaFormat) { f.Video.BitDepth = 0 }},
		{"range", func(f *core.MediaFormat) { f.Video.Color.Range = "" }},
		{"primaries", func(f *core.MediaFormat) { f.Video.Color.Primaries = "\t" }},
		{"transfer", func(f *core.MediaFormat) { f.Video.Color.Transfer = "" }},
		{"matrix", func(f *core.MediaFormat) { f.Video.Color.Matrix = " " }},
		{"audio_codec_empty", func(f *core.MediaFormat) { f.Audio.Codec = "" }},
		{"audio_codec_video", func(f *core.MediaFormat) { f.Audio.Codec = core.CodecAV1 }},
		{"audio_codec_unknown", func(f *core.MediaFormat) { f.Audio.Codec = "opus" }},
		{"audio_profile", func(f *core.MediaFormat) { f.Audio.CodecProfile = "" }},
		{"audio_profile_whitespace", func(f *core.MediaFormat) { f.Audio.CodecProfile = "\t" }},
		{"sample_rate", func(f *core.MediaFormat) { f.Audio.SampleRateHz = 0 }},
		{"channels", func(f *core.MediaFormat) { f.Audio.Channels = 0 }},
		{"layout", func(f *core.MediaFormat) { f.Audio.ChannelLayout = "" }},
		{"layout_whitespace", func(f *core.MediaFormat) { f.Audio.ChannelLayout = " \n" }},
		{"missing_audio", func(f *core.MediaFormat) { f.Audio = core.AudioFormat{} }},
		{"missing_video", func(f *core.MediaFormat) { f.Video = core.VideoFormat{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := validProfile().Format
			tc.mutate(&f)
			requireCategory(t, f.Validate(), core.ErrInvalidFormat)
			if f.Equal(f) || f.Equal(validProfile().Format) || validProfile().Format.Equal(f) {
				t.Fatal("некорректный формат участвует в равенстве")
			}
		})
	}
	for _, v := range []interface{ Validate() error }{core.FrameRate{}, core.ColorInfo{}, core.VideoFormat{}, core.AudioFormat{}, core.MediaFormat{}} {
		requireCategory(t, v.Validate(), core.ErrInvalidFormat)
	}
}

func TestStructuralValidationPreservesDescriptions(t *testing.T) {
	for _, codec := range []core.Codec{core.CodecAV1, core.CodecH264} {
		p := validProfile()
		p.Format.Video.Codec = codec
		p.Format.Video.CodecProfile = " future-profile "
		p.Format.Video.Level = "future-level"
		p.Format.Video.Tier = "future-tier"
		p.Format.Video.PixelFormat = "future-pixel-format"
		p.Format.Audio.ChannelLayout = "future-layout"
		before := p
		if err := p.Validate(); err != nil {
			t.Fatalf("структурный профиль %s отклонён: %v", codec, err)
		}
		if p != before {
			t.Fatal("валидация изменила описание")
		}
	}
	p := validProfile()
	p.Mode = core.ModeTranscode
	p.VideoEncoding.TargetBitrateBPS = 1
	p.VideoEncoding.MaxBitrateBPS = 1
	p.VideoEncoding.BufferBits = 1
	p.VideoEncoding.GOPFrames = 1
	p.VideoEncoding.MaxBFrames = 0
	p.VideoEncoding.ClosedGOP = false
	p.AudioEncoding.BitrateBPS = 1
	if err := p.Validate(); err != nil {
		t.Fatalf("положительная граница отклонена: %v", err)
	}
	p.Format.Video.Width, p.Format.Video.Height = 1, 1
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 1, Denominator: 1}
	p.Format.Video.BitDepth = 1
	p.Format.Audio.SampleRateHz, p.Format.Audio.Channels = 1, 1
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
	if err := p.Validate(); err != nil {
		t.Fatalf("верхние границы типов не должны вводить ограничений адаптера: %v", err)
	}
}

func TestProfileValidationCategories(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*core.GroupProfile)
	}{
		{"mode_empty", func(p *core.GroupProfile) { p.Mode = "" }},
		{"mode_unknown", func(p *core.GroupProfile) { p.Mode = "other" }},
		{"target_zero", func(p *core.GroupProfile) { p.VideoEncoding.TargetBitrateBPS = 0 }},
		{"max_zero", func(p *core.GroupProfile) { p.VideoEncoding.MaxBitrateBPS = 0 }},
		{"target_exceeds_max", func(p *core.GroupProfile) { p.VideoEncoding.TargetBitrateBPS = 8_000_001 }},
		{"buffer_zero", func(p *core.GroupProfile) { p.VideoEncoding.BufferBits = 0 }},
		{"gop_zero", func(p *core.GroupProfile) { p.VideoEncoding.GOPFrames = 0 }},
		{"audio_bitrate_zero", func(p *core.GroupProfile) { p.AudioEncoding.BitrateBPS = 0 }},
		{"nested_format", func(p *core.GroupProfile) { p.Format.Audio.Channels = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validProfile()
			tc.mutate(&p)
			requireCategory(t, p.Validate(), core.ErrInvalidProfile)
			if p.Equal(p) || p.Equal(validProfile()) || validProfile().Equal(p) {
				t.Fatal("некорректный профиль участвует в равенстве")
			}
		})
	}
	for _, v := range []interface{ Validate() error }{core.VideoEncoding{}, core.AudioEncoding{}, core.GroupProfile{}} {
		requireCategory(t, v.Validate(), core.ErrInvalidProfile)
	}
}

func TestProfileEqualityAccountsForEveryField(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*core.GroupProfile)
	}{
		{"mode", func(p *core.GroupProfile) { p.Mode = core.ModeTranscode }},
		{"video_codec", func(p *core.GroupProfile) { p.Format.Video.Codec = core.CodecH264 }},
		{"width", func(p *core.GroupProfile) { p.Format.Video.Width++ }},
		{"height", func(p *core.GroupProfile) { p.Format.Video.Height++ }},
		{"frame_rate", func(p *core.GroupProfile) { p.Format.Video.FrameRate.Numerator++ }},
		{"video_profile", func(p *core.GroupProfile) { p.Format.Video.CodecProfile = "Main" }},
		{"level", func(p *core.GroupProfile) { p.Format.Video.Level = "4.1" }},
		{"tier", func(p *core.GroupProfile) { p.Format.Video.Tier = "main" }},
		{"pixel_format", func(p *core.GroupProfile) { p.Format.Video.PixelFormat = "yuv420p10le" }},
		{"bit_depth", func(p *core.GroupProfile) { p.Format.Video.BitDepth = 10 }},
		{"color_range", func(p *core.GroupProfile) { p.Format.Video.Color.Range = "full" }},
		{"color_primaries", func(p *core.GroupProfile) { p.Format.Video.Color.Primaries = "bt2020" }},
		{"color_transfer", func(p *core.GroupProfile) { p.Format.Video.Color.Transfer = "smpte2084" }},
		{"color_matrix", func(p *core.GroupProfile) { p.Format.Video.Color.Matrix = "bt2020nc" }},
		{"audio_profile", func(p *core.GroupProfile) { p.Format.Audio.CodecProfile = "HE" }},
		{"sample_rate", func(p *core.GroupProfile) { p.Format.Audio.SampleRateHz = 44100 }},
		{"channels", func(p *core.GroupProfile) { p.Format.Audio.Channels = 1 }},
		{"channel_layout", func(p *core.GroupProfile) { p.Format.Audio.ChannelLayout = "mono" }},
		{"target_bitrate", func(p *core.GroupProfile) { p.VideoEncoding.TargetBitrateBPS++ }},
		{"max_bitrate", func(p *core.GroupProfile) { p.VideoEncoding.MaxBitrateBPS++ }},
		{"buffer", func(p *core.GroupProfile) { p.VideoEncoding.BufferBits++ }},
		{"gop", func(p *core.GroupProfile) { p.VideoEncoding.GOPFrames++ }},
		{"b_frames", func(p *core.GroupProfile) { p.VideoEncoding.MaxBFrames = 2 }},
		{"closed_gop", func(p *core.GroupProfile) { p.VideoEncoding.ClosedGOP = false }},
		{"audio_bitrate", func(p *core.GroupProfile) { p.AudioEncoding.BitrateBPS++ }},
	}
	a := validProfile()
	if !a.Equal(a) || !a.Format.Equal(a.Format) {
		t.Fatal("корректное значение не равно себе")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := a
			tc.mutate(&b)
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
