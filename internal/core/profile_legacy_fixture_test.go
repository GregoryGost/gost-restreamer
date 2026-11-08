package core_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

type frozenProfileCase struct {
	Name                   string
	Payload, Canonical     []byte
	ProfileKey, VariantKey string
}

func frozenProfileCases(t *testing.T) []frozenProfileCase {
	t.Helper()
	b, err := os.ReadFile("testdata/encoding_legacy_v1/profiles.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []frozenProfileCase
	if err = json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 3 {
		t.Fatalf("Нужно три независимых эталона, получено %d", len(cases))
	}
	return cases
}

func TestFrozenLegacyProfilesPreserveBytesIdentityAndAcceptance(t *testing.T) {
	for _, fixture := range frozenProfileCases(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			p, err := core.DecodeProfile(fixture.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if p.Encoding != (core.EncodingSettings{}) {
				t.Fatalf("Старому профилю добавлены настройки: %+v", p.Encoding)
			}
			if err = p.Validate(); err != nil {
				t.Fatal(err)
			}
			if p.Format.Video.FrameRate != (core.FrameRate{Numerator: 60000, Denominator: 2002}) {
				t.Fatal("Сохранённая дробь FPS изменилась")
			}
			got, err := core.EncodeProfile(p)
			if err != nil || !bytes.Equal(got, fixture.Payload) {
				t.Fatalf("Изменились байты v1: %s, %v", got, err)
			}
			key, canonical, err := core.ProfileIdentity(p)
			if err != nil || key != fixture.ProfileKey || !bytes.Equal(canonical, fixture.Canonical) {
				t.Fatalf("Изменилась прежняя идентичность: %q, %v", key, err)
			}
			variant, err := core.VariantIdentity("frozen-source", p, "generator-v1")
			if err != nil || variant != fixture.VariantKey {
				t.Fatalf("Изменилась идентичность варианта: %q, %v", variant, err)
			}
			reduced := p
			reduced.Format.Video.FrameRate = core.FrameRate{Numerator: 30000, Denominator: 1001}
			if !p.Equal(reduced) {
				t.Fatal("Математически равные FPS перестали совпадать")
			}
			reducedKey, _, err := core.ProfileIdentity(reduced)
			if err != nil || reducedKey != fixture.ProfileKey {
				t.Fatal("Канонизация FPS изменилась")
			}
			if fixture.Name == "exact-bytes" {
				raw := string([]byte{' ', 0xff, 0, 'x', ' '})
				for _, s := range []string{p.Format.Video.CodecProfile, p.Format.Video.Level, p.Format.Video.Tier, p.Format.Video.PixelFormat, p.Format.Video.Color.Range, p.Format.Video.Color.Primaries, p.Format.Video.Color.Transfer, p.Format.Video.Color.Matrix, p.Format.Audio.CodecProfile, p.Format.Audio.ChannelLayout} {
					if s != raw {
						t.Fatalf("Исходные байты строки потеряны: %x", s)
					}
				}
				if p.VideoEncoding.TargetBitrateBPS != math.MaxUint64-1 || p.VideoEncoding.MaxBitrateBPS != math.MaxUint64 || p.VideoEncoding.BufferBits != math.MaxUint64 || p.AudioEncoding.BitrateBPS != math.MaxUint64 {
					t.Fatal("Полный диапазон uint64 потерян")
				}
				if p.Format.Video.Width != 65535 || p.Format.Video.Height != 1 {
					t.Fatal("Прежние допустимые размеры изменены")
				}
			}
		})
	}
}

func explicitFixtureProfile(t *testing.T, encoder string) core.GroupProfile {
	t.Helper()
	for _, f := range frozenProfileCases(t) {
		if f.Name == "h264" && encoder == "h264_nvenc" || f.Name == "av1" && encoder == "libsvtav1" {
			p, err := core.DecodeProfile(f.Payload)
			if err != nil {
				t.Fatal(err)
			}
			p.Encoding = core.DefaultEncoding(p.Mode, encoder, "vbr")
			if encoder == "libsvtav1" {
				p.VideoEncoding.MaxBitrateBPS = 0
				p.VideoEncoding.BufferBits = 0
			} else {
				p.VideoEncoding.MaxBitrateBPS = p.VideoEncoding.TargetBitrateBPS
				p.Encoding.NVENC.EnableSpatialAQ = true
			}
			if err = p.Validate(); err != nil {
				t.Fatal(err)
			}
			return p
		}
	}
	t.Fatal("Не найден исходный профиль")
	return core.GroupProfile{}
}

func TestExplicitEncodingFieldsChangeFullIdentity(t *testing.T) {
	for _, encoder := range []string{"h264_nvenc", "libsvtav1"} {
		t.Run(encoder, func(t *testing.T) {
			p := explicitFixtureProfile(t, encoder)
			changes := []struct {
				name  string
				apply func(*core.GroupProfile)
			}{
				{"audio_custom", func(p *core.GroupProfile) { p.Encoding.AudioCustom = "aac_coder=fast" }},
			}
			if encoder == "h264_nvenc" {
				changes = append(changes, []struct {
					name  string
					apply func(*core.GroupProfile)
				}{
					{"rate_control", func(p *core.GroupProfile) { p.Encoding.RateControl = "cbr" }},
					{"rescale", func(p *core.GroupProfile) { p.Encoding.RescaleOutput = false }},
					{"video_custom", func(p *core.GroupProfile) { p.Encoding.VideoCustom = "nonref_p=1" }},
					{"preset", func(p *core.GroupProfile) { p.Encoding.NVENC.Preset = "p7" }},
					{"tuning", func(p *core.GroupProfile) { p.Encoding.NVENC.Tuning = "ll" }},
					{"multipass", func(p *core.GroupProfile) { p.Encoding.NVENC.Multipass = "fullres" }},
					{"lookahead", func(p *core.GroupProfile) { p.Encoding.NVENC.LookaheadFrames = 29 }},
					{"spatial_aq", func(p *core.GroupProfile) { p.Encoding.NVENC.EnableSpatialAQ = false }},
					{"temporal_aq", func(p *core.GroupProfile) { p.Encoding.NVENC.EnableTemporalAQ = true }},
					{"aq_strength", func(p *core.GroupProfile) { p.Encoding.NVENC.AQStrength = 15 }},
				}...)
			} else {
				changes = append(changes, []struct {
					name  string
					apply func(*core.GroupProfile)
				}{
					{"video_custom", func(p *core.GroupProfile) { p.Encoding.VideoCustom = "lp=0" }},
					{"preset_zero", func(p *core.GroupProfile) { p.Encoding.SVTAV1.Preset = 0 }},
					{"tuning", func(p *core.GroupProfile) { p.Encoding.SVTAV1.Tuning = "vq" }},
					{"lookahead", func(p *core.GroupProfile) { p.Encoding.SVTAV1.LookaheadFrames = 6 }},
				}...)
			}
			key, canonical, err := core.ProfileIdentity(p)
			if err != nil {
				t.Fatal(err)
			}
			vk, err := core.VariantIdentity("frozen-source", p, "generator-v1")
			if err != nil {
				t.Fatal(err)
			}
			for _, change := range changes {
				t.Run(change.name, func(t *testing.T) {
					other := p
					change.apply(&other)
					if err := other.Validate(); err != nil {
						t.Fatalf("Допустимый вариант отвергнут: %v", err)
					}
					if p.Equal(other) {
						t.Fatal("Полное равенство проигнорировало настройку")
					}
					otherKey, otherCanonical, err := core.ProfileIdentity(other)
					if err != nil || otherKey == key || bytes.Equal(otherCanonical, canonical) {
						t.Fatalf("Настройка не участвует в canonical identity: %v", err)
					}
					otherVK, err := core.VariantIdentity("frozen-source", other, "generator-v1")
					if err != nil || otherVK == vk {
						t.Fatalf("Настройка не участвует в variant identity: %v", err)
					}
					payload, err := core.EncodeProfile(other)
					if err != nil {
						t.Fatal(err)
					}
					decoded, err := core.DecodeProfile(payload)
					if err != nil || decoded != other {
						t.Fatalf("Материализованные значения изменились при чтении: %+v, %v", decoded, err)
					}
				})
			}
		})
	}
}

func TestExplicitStoredEncodingRejectsIncompleteValuesWithoutDefaults(t *testing.T) {
	p := explicitFixtureProfile(t, "h264_nvenc")
	payload, err := core.EncodeProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["Version"]) != "2" {
		t.Fatalf("Нужен отдельный профильный discriminator v2: %s", payload)
	}
	var encoding map[string]json.RawMessage
	if err = json.Unmarshal(fields["Encoding"], &encoding); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"Version", "VideoEncoder", "AudioEncoder", "RateControl", "RescaleOutput", "VideoCustom", "AudioCustom", "NVENC", "SVTAV1"} {
		t.Run(field, func(t *testing.T) {
			var mutated map[string]json.RawMessage
			if err := json.Unmarshal(fields["Encoding"], &mutated); err != nil {
				t.Fatal(err)
			}
			delete(mutated, field)
			b, err := json.Marshal(mutated)
			if err != nil {
				t.Fatal(err)
			}
			envelope := map[string]json.RawMessage{}
			for k, v := range fields {
				envelope[k] = v
			}
			envelope["Encoding"] = b
			b, err = json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = core.DecodeProfile(b); !errors.Is(err, core.ErrInvalidMetadata) {
				t.Fatalf("Частичный stored v2 получил defaults вместо отказа: %v", err)
			}
		})
	}
}

func TestExplicitEncodingCheckedBoundaries(t *testing.T) {
	tests := []struct {
		name, encoder string
		change        func(*core.GroupProfile)
		valid         bool
	}{
		{"nvenc_min_dimensions", "h264_nvenc", func(p *core.GroupProfile) { p.Format.Video.Width = 2; p.Format.Video.Height = 2 }, true},
		{"nvenc_max_dimensions", "h264_nvenc", func(p *core.GroupProfile) { p.Format.Video.Width = 65534; p.Format.Video.Height = 65534 }, true},
		{"nvenc_container_overflow", "h264_nvenc", func(p *core.GroupProfile) { p.Format.Video.Width = 65536 }, false},
		{"nvenc_b_lookahead_limit", "h264_nvenc", func(p *core.GroupProfile) { p.VideoEncoding.MaxBFrames = 4; p.Encoding.NVENC.LookaheadFrames = 27 }, true},
		{"nvenc_b_lookahead_overflow", "h264_nvenc", func(p *core.GroupProfile) { p.VideoEncoding.MaxBFrames = 4; p.Encoding.NVENC.LookaheadFrames = 28 }, false},
		{"nvenc_baseline_b", "h264_nvenc", func(p *core.GroupProfile) { p.Format.Video.CodecProfile = "baseline" }, false},
		{"nvenc_max_bitrate", "h264_nvenc", func(p *core.GroupProfile) {
			p.VideoEncoding.TargetBitrateBPS = math.MaxUint32
			p.VideoEncoding.MaxBitrateBPS = math.MaxUint32
			p.VideoEncoding.BufferBits = math.MaxInt32
		}, true},
		{"nvenc_bitrate_overflow", "h264_nvenc", func(p *core.GroupProfile) { p.VideoEncoding.MaxBitrateBPS = math.MaxUint32 + 1 }, false},
		{"nvenc_cbr_mismatch", "h264_nvenc", func(p *core.GroupProfile) { p.Encoding.RateControl = "cbr"; p.VideoEncoding.MaxBitrateBPS++ }, false},
		{"svt_min_dimensions", "libsvtav1", func(p *core.GroupProfile) { p.Format.Video.Width = 64; p.Format.Video.Height = 64 }, true},
		{"svt_odd_dimensions", "libsvtav1", func(p *core.GroupProfile) { p.Format.Video.Width = 65 }, false},
		{"svt_max_dimensions", "libsvtav1", func(p *core.GroupProfile) { p.Format.Video.Width = 16384; p.Format.Video.Height = 8704 }, true},
		{"svt_dimensions_overflow", "libsvtav1", func(p *core.GroupProfile) { p.Format.Video.Height = 8706 }, false},
		{"svt_vbr_no_caps", "libsvtav1", func(p *core.GroupProfile) { p.VideoEncoding.MaxBitrateBPS = 1 }, false},
		{"svt_vbr_no_buffer", "libsvtav1", func(p *core.GroupProfile) { p.VideoEncoding.BufferBits = 1 }, false},
		{"svt_lookahead_min", "libsvtav1", func(p *core.GroupProfile) { p.Encoding.SVTAV1.LookaheadFrames = 6 }, true},
		{"svt_lookahead_max", "libsvtav1", func(p *core.GroupProfile) { p.Encoding.SVTAV1.LookaheadFrames = 120 }, true},
		{"svt_lookahead_below", "libsvtav1", func(p *core.GroupProfile) { p.Encoding.SVTAV1.LookaheadFrames = 5 }, false},
		{"svt_lookahead_zero", "libsvtav1", func(p *core.GroupProfile) { p.Encoding.SVTAV1.LookaheadFrames = 0 }, false},
		{"svt_lookahead_above", "libsvtav1", func(p *core.GroupProfile) { p.Encoding.SVTAV1.LookaheadFrames = 121 }, false},
		{"svt_cbr_20ms", "libsvtav1", func(p *core.GroupProfile) {
			p.Encoding = core.DefaultEncoding(p.Mode, "libsvtav1", "cbr")
			p.VideoEncoding.MaxBitrateBPS = 6000000
			p.VideoEncoding.BufferBits = 120000
		}, true},
		{"svt_cbr_19ms", "libsvtav1", func(p *core.GroupProfile) {
			p.Encoding = core.DefaultEncoding(p.Mode, "libsvtav1", "cbr")
			p.VideoEncoding.MaxBitrateBPS = 6000000
			p.VideoEncoding.BufferBits = 114000
		}, false},
		{"svt_cbr_10000ms", "libsvtav1", func(p *core.GroupProfile) {
			p.Encoding = core.DefaultEncoding(p.Mode, "libsvtav1", "cbr")
			p.VideoEncoding.MaxBitrateBPS = 6000000
			p.VideoEncoding.BufferBits = 60000000
		}, true},
		{"svt_cbr_10001ms", "libsvtav1", func(p *core.GroupProfile) {
			p.Encoding = core.DefaultEncoding(p.Mode, "libsvtav1", "cbr")
			p.VideoEncoding.MaxBitrateBPS = 6000000
			p.VideoEncoding.BufferBits = 60006000
		}, false},
		{"svt_cbr_fractional_ms", "libsvtav1", func(p *core.GroupProfile) {
			p.Encoding = core.DefaultEncoding(p.Mode, "libsvtav1", "cbr")
			p.VideoEncoding.MaxBitrateBPS = 6000000
			p.VideoEncoding.BufferBits = 120001
		}, false},
		{"svt_cbr_buffer_uint64", "libsvtav1", func(p *core.GroupProfile) {
			p.Encoding = core.DefaultEncoding(p.Mode, "libsvtav1", "cbr")
			p.VideoEncoding.MaxBitrateBPS = 6000000
			p.VideoEncoding.BufferBits = math.MaxUint64
		}, false},
		{"aac_max_bitrate", "h264_nvenc", func(p *core.GroupProfile) { p.AudioEncoding.BitrateBPS = 576000 }, true},
		{"aac_bitrate_clamp", "h264_nvenc", func(p *core.GroupProfile) { p.AudioEncoding.BitrateBPS = 576001 }, false},
		{"aac_unrepresentable_sample_entry", "h264_nvenc", func(p *core.GroupProfile) { p.Format.Audio.SampleRateHz = 96000 }, false},
		{"copy_cannot_rescale", "libsvtav1", func(p *core.GroupProfile) { p.Encoding.RescaleOutput = true }, false},
		{"inapplicable_svt", "h264_nvenc", func(p *core.GroupProfile) { p.Encoding.SVTAV1.Preset = 8 }, false},
		{"partial_legacy_block", "h264_nvenc", func(p *core.GroupProfile) { p.Encoding = core.EncodingSettings{RateControl: "vbr"} }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := explicitFixtureProfile(t, tt.encoder)
			tt.change(&p)
			err := p.Validate()
			if tt.valid {
				if err != nil {
					t.Fatalf("Допустимая граница отвергнута: %v", err)
				}
			} else {
				var pe *core.ProfileError
				if !errors.Is(err, core.ErrInvalidProfile) || !errors.As(err, &pe) || pe.Code() == "" || pe.Field() == "" || pe.Detail() == "" {
					t.Fatalf("Нужен доверенный отказ профиля: %v", err)
				}
			}
		})
	}
}

func TestCustomEncodingCanonicalizationAndConflictBoundary(t *testing.T) {
	for _, raw := range []string{"strict_gop=+01 nonref_p=000", " nonref_p=0  strict_gop=1 "} {
		normalized, err := core.NormalizeCustom("h264_nvenc", raw)
		if err != nil || normalized != "nonref_p=0 strict_gop=1" {
			t.Fatalf("Канонизация %q: %q, %v", raw, normalized, err)
		}
	}
	base := explicitFixtureProfile(t, "h264_nvenc")
	base.Encoding.VideoCustom = "nonref_p=0 strict_gop=1"
	key, _, err := core.ProfileIdentity(base)
	if err != nil {
		t.Fatal(err)
	}
	same := base
	canonical, err := core.NormalizeCustom("h264_nvenc", "strict_gop=01 nonref_p=+0")
	if err != nil {
		t.Fatal(err)
	}
	same.Encoding.VideoCustom = canonical
	sameKey, _, err := core.ProfileIdentity(same)
	if err != nil || sameKey != key || !same.Equal(base) {
		t.Fatal("Порядок custom изменил полную идентичность")
	}
	for _, input := range []struct{ encoder, value, code string }{
		{"h264_nvenc", "rc=vbr", "custom_conflict"}, {"h264_nvenc", "b:v=6000000", "custom_conflict"}, {"h264_nvenc", "preset=p4", "custom_conflict"},
		{"libsvtav1", "pred-struct=1", "custom_conflict"}, {"libsvtav1", "lookahead=6", "custom_conflict"}, {"aac", "ar=48000", "custom_conflict"},
		{"h264_nvenc", "nonref_p=0 nonref_p=1", "duplicate_custom"}, {"h264_nvenc", "lp=1", "unknown_custom"}, {"libsvtav1", "nonref_p=1", "unknown_custom"},
		{"aac", "aac_coder=fast\n", "invalid_custom"}, {"h264_nvenc", "nonref_p='1'", "invalid_custom"}, {"h264_nvenc", "nonref_p=1=1", "invalid_custom"},
		{"h264_nvenc", "nonref_p=2", "invalid_custom"}, {"libsvtav1", "lp=7", "invalid_custom"},
	} {
		t.Run(input.encoder+"/"+input.value, func(t *testing.T) {
			_, err := core.NormalizeCustom(input.encoder, input.value)
			var pe *core.ProfileError
			if !errors.As(err, &pe) || pe.Code() != input.code || pe.Field() == "" || pe.Detail() == "" {
				t.Fatalf("Неверная категория отказа: %v", err)
			}
		})
	}
}
