package core_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func encodedProfile(encoder, rc string) core.GroupProfile {
	p := validProfile()
	p.Encoding = core.DefaultEncoding(p.Mode, encoder, rc)
	if encoder == core.VideoEncoderNVENC {
		p.Mode = core.ModeTranscode
		p.Format.Video.Codec = core.CodecH264
		p.Format.Video.CodecProfile = "high"
		p.Encoding = core.DefaultEncoding(p.Mode, encoder, rc)
	}
	if rc == core.RateControlCBR {
		p.VideoEncoding.MaxBitrateBPS = p.VideoEncoding.TargetBitrateBPS
	}
	if encoder == core.VideoEncoderSVTAV1 && rc == core.RateControlVBR {
		p.VideoEncoding.MaxBitrateBPS = 0
		p.VideoEncoding.BufferBits = 0
	}
	return p
}

func TestEncodingDefaultsAndImmutableRoundTrip(t *testing.T) {
	for _, encoder := range []string{core.VideoEncoderNVENC, core.VideoEncoderSVTAV1} {
		for _, rc := range []string{core.RateControlCBR, core.RateControlVBR} {
			t.Run(encoder+"/"+rc, func(t *testing.T) {
				p := encodedProfile(encoder, rc)
				if err := p.Validate(); err != nil {
					t.Fatal(err)
				}
				before := p
				p.Format.Video.FrameRate = core.FrameRate{Numerator: 60000, Denominator: 2002}
				data, err := core.EncodeProfile(p)
				if err != nil {
					t.Fatal(err)
				}
				got, err := core.DecodeProfile(data)
				if err != nil || got != p {
					t.Fatalf("Профиль изменился после хранения: %v", err)
				}
				var header struct{ Version int }
				if err := json.Unmarshal(data, &header); err != nil || header.Version != 2 || core.MetadataVersion != 1 {
					t.Fatalf("Версии форматов изменены: %s", data)
				}
				key, canonical, err := core.ProfileIdentity(p)
				if err != nil {
					t.Fatal(err)
				}
				reduced := p
				reduced.Format.Video.FrameRate = core.FrameRate{Numerator: 30000, Denominator: 1001}
				otherKey, otherCanonical, err := core.ProfileIdentity(reduced)
				if err != nil || key != otherKey || !bytes.Equal(canonical, otherCanonical) || p.Format.Video.FrameRate.Denominator != 2002 {
					t.Fatal("Канонизация FPS изменила профиль или идентичность")
				}
				if before.Encoding.VideoEncoder == core.VideoEncoderSVTAV1 && rc == core.RateControlVBR && before.Encoding.SVTAV1.LookaheadFrames != -1 {
					t.Fatal("VBR требует auto lookahead")
				}
			})
		}
	}
}

func TestEncodingValidationRejectsUnsupportedRequests(t *testing.T) {
	cases := []struct {
		name, encoder, rc string
		change            func(*core.GroupProfile)
	}{
		{"partial_legacy", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Encoding.Version = 0 }},
		{"unknown_version", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Encoding.Version = 3 }},
		{"mode", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Mode = "other" }},
		{"rc", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Encoding.RateControl = "cq" }},
		{"audio_encoder", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Encoding.AudioEncoder = "opus" }},
		{"video_encoder", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Encoding.VideoEncoder = "libx264" }},
		{"codec", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Video.Codec = core.CodecAV1 }},
		{"dimensions_zero", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Video.Width = 0 }},
		{"dimensions_odd", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Video.Height = 1079 }},
		{"h264_dimensions", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Video.Width = 65536 }},
		{"pixel", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Video.PixelFormat = "yuv444p" }},
		{"bit_depth", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Video.BitDepth = 10 }},
		{"color", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Video.Color.Transfer = "smpte2084" }},
		{"fps_zero", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Video.FrameRate.Denominator = 0 }},
		{"fps_range", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Video.FrameRate.Numerator = math.MaxUint32 }},
		{"fps_scale", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Video.FrameRate.Numerator = 65521 }},
		{"open_gop", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.VideoEncoding.ClosedGOP = false }},
		{"gop_zero", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.VideoEncoding.GOPFrames = 0 }},
		{"gop_overflow", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.VideoEncoding.GOPFrames = math.MaxUint32 }},
		{"b_equal_gop", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.VideoEncoding.GOPFrames = 2; p.VideoEncoding.MaxBFrames = 2 }},
		{"b_limit", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.VideoEncoding.MaxBFrames = 5 }},
		{"baseline_b", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Video.CodecProfile = "baseline"; p.VideoEncoding.MaxBFrames = 1 }},
		{"profile", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Video.CodecProfile = "future" }},
		{"tier", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Video.Tier = "main" }},
		{"level", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Video.Level = "6.0" }},
		{"target_zero", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.VideoEncoding.TargetBitrateBPS = 0 }},
		{"target_gt_max", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.VideoEncoding.TargetBitrateBPS = p.VideoEncoding.MaxBitrateBPS + 1 }},
		{"bitrate_overflow", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.VideoEncoding.MaxBitrateBPS = math.MaxUint32 + 1 }},
		{"buffer_zero", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.VideoEncoding.BufferBits = 0 }},
		{"buffer_overflow", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.VideoEncoding.BufferBits = math.MaxUint64 }},
		{"cbr_mismatch", core.VideoEncoderNVENC, "cbr", func(p *core.GroupProfile) { p.VideoEncoding.MaxBitrateBPS++ }},
		{"foreign_svt", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Encoding.SVTAV1.Preset = 8 }},
		{"preset", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Encoding.NVENC.Preset = "p8" }},
		{"tuning", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Encoding.NVENC.Tuning = "lossless" }},
		{"multipass", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Encoding.NVENC.Multipass = "future" }},
		{"lookahead_negative", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Encoding.NVENC.LookaheadFrames = -1 }},
		{"lookahead_sum", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Encoding.NVENC.LookaheadFrames = 31; p.VideoEncoding.MaxBFrames = 1 }},
		{"aq_strength", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Encoding.NVENC.EnableSpatialAQ = true; p.Encoding.NVENC.AQStrength = 16 }},
		{"aq_disabled_strength", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Encoding.NVENC.AQStrength = 7 }},
		{"aac_codec", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Audio.Codec = "opus" }},
		{"aac_profile", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Audio.CodecProfile = "HE" }},
		{"aac_rate", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Audio.SampleRateHz = 96000 }},
		{"aac_layout", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Format.Audio.ChannelLayout = "mono" }},
		{"aac_zero", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.AudioEncoding.BitrateBPS = 0 }},
		{"aac_clamp", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.AudioEncoding.BitrateBPS = 576001 }},
		{"custom_not_normalized", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.Encoding.VideoCustom = "strict_gop=01 nonref_p=0" }},
		{"copy_rescale", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.Encoding.RescaleOutput = true }},
		{"foreign_nvenc", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.Encoding.NVENC.Preset = "p4" }},
		{"svt_small", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.Format.Video.Width = 62 }},
		{"svt_odd", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.Format.Video.Width = 65 }},
		{"svt_large", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.Format.Video.Height = 8706 }},
		{"svt_fps", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.Format.Video.FrameRate.Numerator = 241 }},
		{"svt_profile", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.Format.Video.CodecProfile = "high" }},
		{"svt_level", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.Format.Video.Level = "7.4" }},
		{"svt_tier", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.Format.Video.Tier = "high" }},
		{"svt_gop", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.VideoEncoding.GOPFrames = 1 }},
		{"svt_b", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.VideoEncoding.MaxBFrames = 1 }},
		{"svt_bitrate_zero", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.VideoEncoding.TargetBitrateBPS = 0 }},
		{"svt_bitrate_high", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.VideoEncoding.TargetBitrateBPS = 100000001 }},
		{"svt_vbr_cap", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.VideoEncoding.MaxBitrateBPS = 6000000 }},
		{"svt_vbr_buffer", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.VideoEncoding.BufferBits = 1 }},
		{"svt_cbr_nominal", core.VideoEncoderSVTAV1, "cbr", func(p *core.GroupProfile) { p.VideoEncoding.MaxBitrateBPS++ }},
		{"svt_buffer_low", core.VideoEncoderSVTAV1, "cbr", func(p *core.GroupProfile) { p.VideoEncoding.BufferBits = 60000 }},
		{"svt_buffer_fractional", core.VideoEncoderSVTAV1, "cbr", func(p *core.GroupProfile) { p.VideoEncoding.BufferBits++ }},
		{"svt_buffer_overflow", core.VideoEncoderSVTAV1, "cbr", func(p *core.GroupProfile) { p.VideoEncoding.BufferBits = math.MaxUint64 }},
		{"svt_preset", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.Encoding.SVTAV1.Preset = 14 }},
		{"svt_cbr_vq", core.VideoEncoderSVTAV1, "cbr", func(p *core.GroupProfile) { p.Encoding.SVTAV1.Tuning = "vq" }},
		{"svt_aq", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.Encoding.SVTAV1.AQMode = 0 }},
		{"svt_cbr_lookahead", core.VideoEncoderSVTAV1, "cbr", func(p *core.GroupProfile) { p.Encoding.SVTAV1.LookaheadFrames = -1 }},
		{"svt_vbr_lookahead_low", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.Encoding.SVTAV1.LookaheadFrames = 5 }},
		{"svt_vbr_lookahead_high", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.Encoding.SVTAV1.LookaheadFrames = 121 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := encodedProfile(tc.encoder, tc.rc)
			tc.change(&p)
			before := p
			err := p.Validate()
			var profileErr *core.ProfileError
			if !errors.Is(err, core.ErrInvalidProfile) || !errors.As(err, &profileErr) || profileErr.Code() == "" || !strings.HasPrefix(profileErr.Field(), "profile.") || profileErr.Detail() == "" {
				t.Fatalf("Профиль принят или потерял типизированную ошибку: %v", err)
			}
			if p != before || p.Equal(p) {
				t.Fatal("Валидация изменила профиль либо некорректное значение равно себе")
			}
		})
	}
}

func TestEncodingAcceptsExactBoundaries(t *testing.T) {
	cases := []struct {
		name, encoder, rc string
		change            func(*core.GroupProfile)
	}{
		{"h264_min", core.VideoEncoderNVENC, "cbr", func(p *core.GroupProfile) {
			p.Format.Video.Width = 2
			p.Format.Video.Height = 2
			p.VideoEncoding = core.VideoEncoding{TargetBitrateBPS: 1, MaxBitrateBPS: 1, BufferBits: 1, GOPFrames: 1, ClosedGOP: true}
			p.AudioEncoding.BitrateBPS = 1
		}},
		{"h264_max", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			p.Format.Video.Width = 65534
			p.Format.Video.Height = 65534
			p.VideoEncoding.TargetBitrateBPS = math.MaxUint32
			p.VideoEncoding.MaxBitrateBPS = math.MaxUint32
			p.VideoEncoding.BufferBits = math.MaxInt32
			p.VideoEncoding.GOPFrames = math.MaxInt32
			p.VideoEncoding.MaxBFrames = 4
			p.Encoding.NVENC.LookaheadFrames = 27
			p.Encoding.NVENC.EnableSpatialAQ = true
			p.Encoding.NVENC.AQStrength = 15
		}},
		{"vbr_equal", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.VideoEncoding.MaxBitrateBPS = p.VideoEncoding.TargetBitrateBPS }},
		{"svt_min", core.VideoEncoderSVTAV1, "cbr", func(p *core.GroupProfile) {
			p.Format.Video.Width = 64
			p.Format.Video.Height = 64
			p.VideoEncoding.GOPFrames = 2
			p.VideoEncoding.BufferBits = p.VideoEncoding.TargetBitrateBPS / 50
			p.Encoding.SVTAV1.Preset = 0
		}},
		{"svt_max", core.VideoEncoderSVTAV1, "cbr", func(p *core.GroupProfile) {
			p.Format.Video.Width = 16384
			p.Format.Video.Height = 8704
			p.Format.Video.FrameRate.Numerator = 240
			p.VideoEncoding.TargetBitrateBPS = 100000000
			p.VideoEncoding.MaxBitrateBPS = 100000000
			p.VideoEncoding.BufferBits = 1000000000
			p.Encoding.SVTAV1.Preset = 13
		}},
		{"svt_lookahead_low", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.Encoding.SVTAV1.LookaheadFrames = 6; p.Encoding.SVTAV1.Tuning = "vq" }},
		{"svt_lookahead_high", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) { p.Encoding.SVTAV1.LookaheadFrames = 120 }},
		{"aac_max", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) { p.AudioEncoding.BitrateBPS = 576000 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := encodedProfile(tc.encoder, tc.rc)
			tc.change(&p)
			if err := p.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEncodingFrameRatePreparedDurationBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rate  core.FrameRate
		valid bool
	}{
		{"last_low_step", core.FrameRate{Numerator: 1, Denominator: 44739}, true},
		{"first_overflowing_low_step", core.FrameRate{Numerator: 1, Denominator: 44740}, false},
		{"unreduced_overflowing_low_step", core.FrameRate{Numerator: 2000, Denominator: 89480000}, false},
		{"high_numerator", core.FrameRate{Numerator: 32749, Denominator: 65536}, true},
		{"high_numerator_large_denominator", core.FrameRate{Numerator: 32749, Denominator: 131072}, true},
		{"native_scale_threshold", core.FrameRate{Numerator: 1000, Denominator: 44741}, true},
	} {
		for _, encoder := range []string{core.VideoEncoderNVENC, core.VideoEncoderSVTAV1} {
			t.Run(tc.name+"/"+encoder, func(t *testing.T) {
				p := encodedProfile(encoder, core.RateControlVBR)
				p.Format.Video.FrameRate = tc.rate
				p.Format.Audio.SampleRateHz = 48000
				before := p
				err := p.Validate()
				if tc.valid {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					var typed *core.ProfileError
					if !errors.Is(err, core.ErrInvalidProfile) || !errors.As(err, &typed) || typed.Field() != "profile.format.video.frame_rate" {
						t.Fatalf("Непредставимая длительность кадра принята или потеряла путь поля: %v", err)
					}
				}
				if p != before {
					t.Fatal("Проверка длительности изменила профиль")
				}
			})
		}
	}
}

func TestLegacyProfilePreservesLongFrameDuration(t *testing.T) {
	p := encodedProfile(core.VideoEncoderNVENC, core.RateControlVBR)
	p.Encoding = core.EncodingSettings{}
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 1, Denominator: 44740}
	if err := p.Validate(); err != nil {
		t.Fatalf("Прежняя допустимость профиля изменилась: %v", err)
	}
	raw, err := core.EncodeProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := core.DecodeProfile(raw)
	if err != nil || got != p {
		t.Fatalf("Прежний профиль изменился после хранения: %v", err)
	}
	again, err := core.EncodeProfile(got)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatalf("Байты прежнего профиля изменились: %v", err)
	}
}

func TestCustomNormalizationAndSafeErrors(t *testing.T) {
	for _, tc := range []struct{ encoder, input, want string }{
		{"h264_nvenc", " strict_gop=+01  nonref_p=00 ", "nonref_p=0 strict_gop=1"},
		{"libsvtav1", "lp=0006", "lp=6"},
		{"aac", "aac_coder=fast", "aac_coder=fast"},
		{"aac", "aac_coder=twoloop", "aac_coder=twoloop"},
		{"h264_nvenc", "   ", ""},
	} {
		got, err := core.NormalizeCustom(tc.encoder, tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("Нормализация %q: %q, %v", tc.input, got, err)
		}
		again, err := core.NormalizeCustom(tc.encoder, got)
		if err != nil || got != again {
			t.Fatal("Нормализация не идемпотентна")
		}
	}
	inputs := []string{"lp=1", "nonref_p=2", "nonref_p=-1", "nonref_p=1 nonref_p=1", "nonref_p=1=1", "nonref_p=", "=1", "nonref_p=1\tstrict_gop=0", "nonref_p='1'", "nonref_p=1\\", "nonref_p=[1]", "nonref_p=99999999999999999999999999999999999", "future=private-sensitive-value", "nonref_p=1\n", "é=1", "nonref_p=1;", "not/a/name=1", strings.Repeat("x", 4097), strings.Repeat("x", 65) + "=1", "x=" + strings.Repeat("1", 129)}
	for _, input := range inputs {
		_, err := core.NormalizeCustom(core.VideoEncoderNVENC, input)
		var typed *core.ProfileError
		if !errors.As(err, &typed) || typed.Detail() == "" || strings.Contains(typed.Detail(), "private-sensitive-value") {
			t.Fatalf("Некорректные custom приняты либо раскрыты: %v", err)
		}
	}
	for _, encoder := range []string{core.VideoEncoderSVTAV1, core.AudioEncoderAAC} {
		_, err := core.NormalizeCustom(encoder, "nonref_p=1")
		var typed *core.ProfileError
		if !errors.As(err, &typed) || typed.Code() != "unknown_custom" {
			t.Fatal("Чужой каталог параметров принят")
		}
	}
	for _, value := range []string{"lp=7", "lp=-1"} {
		if _, err := core.NormalizeCustom(core.VideoEncoderSVTAV1, value); err == nil {
			t.Fatal("Граница lp не проверена")
		}
	}
	if _, err := core.NormalizeCustom(core.AudioEncoderAAC, "aac_coder=anmr"); err == nil {
		t.Fatal("Неподдержанный AAC coder принят")
	}
	if _, err := core.NormalizeCustom("future", ""); err == nil {
		t.Fatal("Неизвестный encoder принят")
	}
	for _, name := range []string{"rate_control", "rc", "b:v", "maxrate", "max_bitrate_bps", "vbv-bufsize", "averageBitRate", "vbvBufferSize", "enableLookahead", "enableAQ", "enableTemporalAQ", "aqStrength", "g", "keyint", "intra-period-length", "bf", "preset", "tune", "multipass", "rc-lookahead", "lookahead_frames", "spatial-aq", "temporal_aq", "aq-mode", "pred-struct", "hierarchical-levels", "enable-overlays", "enable-dg", "enable-tf", "scd", "profile:v", "level", "tier", "width", "pix_fmt", "fps", "color_primaries", "ar:a", "channel_layout", "i", "output", "filter_complex", "path", "pass", "stats", "svtav1-params"} {
		_, err := core.NormalizeCustom(core.VideoEncoderNVENC, name+"=1")
		var typed *core.ProfileError
		if !errors.As(err, &typed) || typed.Code() != "custom_conflict" {
			t.Fatalf("Конфликт %s не опознан: %v", name, err)
		}
	}
}

func TestEveryEncodingSettingParticipatesInIdentity(t *testing.T) {
	cases := []struct {
		name, encoder string
		change        func(*core.GroupProfile)
	}{
		{"rc", core.VideoEncoderNVENC, func(p *core.GroupProfile) { p.Encoding.RateControl = "cbr" }},
		{"rescale", core.VideoEncoderNVENC, func(p *core.GroupProfile) { p.Encoding.RescaleOutput = false }},
		{"video_custom", core.VideoEncoderNVENC, func(p *core.GroupProfile) { p.Encoding.VideoCustom = "nonref_p=1" }},
		{"audio_custom", core.VideoEncoderNVENC, func(p *core.GroupProfile) { p.Encoding.AudioCustom = "aac_coder=fast" }},
		{"nvenc_preset", core.VideoEncoderNVENC, func(p *core.GroupProfile) { p.Encoding.NVENC.Preset = "p7" }},
		{"nvenc_tuning", core.VideoEncoderNVENC, func(p *core.GroupProfile) { p.Encoding.NVENC.Tuning = "ll" }},
		{"nvenc_multipass", core.VideoEncoderNVENC, func(p *core.GroupProfile) { p.Encoding.NVENC.Multipass = "qres" }},
		{"nvenc_lookahead", core.VideoEncoderNVENC, func(p *core.GroupProfile) { p.Encoding.NVENC.LookaheadFrames = 1 }},
		{"nvenc_spatial_aq", core.VideoEncoderNVENC, func(p *core.GroupProfile) { p.Encoding.NVENC.EnableSpatialAQ = true }},
		{"nvenc_temporal_aq", core.VideoEncoderNVENC, func(p *core.GroupProfile) { p.Encoding.NVENC.EnableTemporalAQ = true }},
		{"nvenc_aq_strength", core.VideoEncoderNVENC, func(p *core.GroupProfile) { p.Encoding.NVENC.AQStrength = 9 }},
		{"svt_preset", core.VideoEncoderSVTAV1, func(p *core.GroupProfile) { p.Encoding.SVTAV1.Preset = 0 }},
		{"svt_tuning", core.VideoEncoderSVTAV1, func(p *core.GroupProfile) { p.Encoding.SVTAV1.Tuning = "vq" }},
		{"svt_lookahead", core.VideoEncoderSVTAV1, func(p *core.GroupProfile) { p.Encoding.SVTAV1.LookaheadFrames = 6 }},
		{"svt_custom", core.VideoEncoderSVTAV1, func(p *core.GroupProfile) { p.Encoding.VideoCustom = "lp=1" }},
		{"version", core.VideoEncoderNVENC, func(p *core.GroupProfile) { p.Encoding = core.EncodingSettings{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := encodedProfile(tc.encoder, "vbr")
			if tc.encoder == core.VideoEncoderNVENC {
				p.VideoEncoding.MaxBitrateBPS = p.VideoEncoding.TargetBitrateBPS
			}
			if tc.name == "nvenc_aq_strength" {
				p.Encoding.NVENC.EnableSpatialAQ = true
			}
			before := p
			other := p
			tc.change(&other)
			key, canonical, err := core.ProfileIdentity(p)
			if err != nil {
				t.Fatal(err)
			}
			otherKey, otherCanonical, err := core.ProfileIdentity(other)
			if err != nil || key == otherKey || bytes.Equal(canonical, otherCanonical) || p.Equal(other) || other.Equal(p) || p != before {
				t.Fatalf("Новое поле не различено: %v", err)
			}
			v1, err := core.VariantIdentity("source", p, "generator")
			if err != nil {
				t.Fatal(err)
			}
			v2, err := core.VariantIdentity("source", other, "generator")
			if err != nil || v1 == v2 {
				t.Fatalf("Варианты совпали: %v", err)
			}
		})
	}
}

func TestStoredEncodingRequiresEveryMaterializedField(t *testing.T) {
	p := encodedProfile(core.VideoEncoderSVTAV1, "vbr")
	raw, err := core.EncodeProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	var visit func(map[string]json.RawMessage, []string)
	visit = func(values map[string]json.RawMessage, path []string) {
		for name, value := range values {
			key := append(append([]string{}, path...), name)
			t.Run(strings.Join(key, "/"), func(t *testing.T) {
				var copyRoot map[string]json.RawMessage
				if err := json.Unmarshal(raw, &copyRoot); err != nil {
					t.Fatal(err)
				}
				var remove func(map[string]json.RawMessage, []string)
				remove = func(object map[string]json.RawMessage, keys []string) {
					if len(keys) == 1 {
						delete(object, keys[0])
						return
					}
					var child map[string]json.RawMessage
					if err := json.Unmarshal(object[keys[0]], &child); err != nil {
						t.Fatal(err)
					}
					remove(child, keys[1:])
					b, err := json.Marshal(child)
					if err != nil {
						t.Fatal(err)
					}
					object[keys[0]] = b
				}
				remove(copyRoot, key)
				corrupt, err := json.Marshal(copyRoot)
				if err != nil {
					t.Fatal(err)
				}
				receiver := validProfile()
				before := receiver
				err = receiver.UnmarshalJSON(corrupt)
				if !errors.Is(err, core.ErrInvalidMetadata) || receiver != before {
					t.Fatalf("Отсутствующее поле принято или частично записано: %v", err)
				}
			})
			var child map[string]json.RawMessage
			if len(value) > 0 && value[0] == '{' {
				if err := json.Unmarshal(value, &child); err != nil {
					t.Fatal(err)
				}
				visit(child, key)
			}
		}
	}
	visit(root, nil)
	for _, field := range []string{"RescaleOutput", "VideoCustom", "AudioCustom"} {
		var copyRoot map[string]json.RawMessage
		if err := json.Unmarshal(raw, &copyRoot); err != nil {
			t.Fatal(err)
		}
		var settings map[string]json.RawMessage
		if err := json.Unmarshal(copyRoot["Encoding"], &settings); err != nil {
			t.Fatal(err)
		}
		settings[field] = json.RawMessage("null")
		copyRoot["Encoding"], err = json.Marshal(settings)
		if err != nil {
			t.Fatal(err)
		}
		corrupt, err := json.Marshal(copyRoot)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := core.DecodeProfile(corrupt); err == nil {
			t.Fatalf("null поля %s принят как нулевое значение", field)
		}
	}
}

func TestEncodingValueOwnershipAndServiceTransition(t *testing.T) {
	var inspect func(reflect.Type)
	inspect = func(typ reflect.Type) {
		switch typ.Kind() {
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				inspect(typ.Field(i).Type)
			}
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface:
			t.Fatalf("Профиль удерживает разделяемое состояние %s", typ)
		}
	}
	inspect(reflect.TypeFor[core.GroupProfile]())
	_, output := contractSnapshots(t)
	for _, stale := range []bool{false, true} {
		current := output.Profile
		current.Profile = encodedProfile(core.VideoEncoderNVENC, "vbr")
		u := &configContractUOW{repository: configContractRepository{profile: current}}
		service, err := core.NewService(u, "owner")
		if err != nil {
			t.Fatal(err)
		}
		replacement := current
		replacement.Profile = validProfile()
		expected := current.Revision
		if stale {
			expected++
		}
		_, err = service.UpdateProfile(context.Background(), replacement, expected)
		if stale {
			if !errors.Is(err, core.ErrConflict) {
				t.Fatalf("CAS должен предшествовать проверке перехода: %v", err)
			}
		} else {
			var typed *core.ProfileError
			if !errors.As(err, &typed) || typed.Field() != "profile.encoding" {
				t.Fatalf("Downgrade принят: %v", err)
			}
		}
		if u.repository.writes != 0 || u.repository.profile != current {
			t.Fatal("Отклонённый переход записан")
		}
	}
	legacy := output.Profile
	legacy.Profile.Format.Video.Width = 1
	legacy.Profile.VideoEncoding.BufferBits = math.MaxUint64
	u := &configContractUOW{repository: configContractRepository{profile: legacy}}
	service, err := core.NewService(u, "owner")
	if err != nil {
		t.Fatal(err)
	}
	candidate := legacy
	candidate.Name = "Другое имя"
	if _, err = service.UpdateProfile(context.Background(), candidate, legacy.Revision); err != nil || u.repository.profile.Profile != legacy.Profile {
		t.Fatalf("Переименование применило новые ограничения к legacy: %v", err)
	}
	candidate = u.repository.profile
	candidate.Profile = encodedProfile(core.VideoEncoderNVENC, "vbr")
	if _, err = service.UpdateProfile(context.Background(), candidate, candidate.Revision); err != nil {
		t.Fatalf("Явное обновление legacy до v2 отклонено: %v", err)
	}
	candidate = u.repository.profile
	candidate.Name = "Профиль с новым именем"
	if _, err = service.UpdateProfile(context.Background(), candidate, candidate.Revision); err != nil {
		t.Fatalf("Переименование v2 отклонено: %v", err)
	}
}
