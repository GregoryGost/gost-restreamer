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

type contractBits struct {
	data  []byte
	count uint
}

func (b *contractBits) write(v uint64, count uint) {
	for i := count; i > 0; i-- {
		if b.count%8 == 0 {
			b.data = append(b.data, 0)
		}
		b.data[len(b.data)-1] |= byte((v>>(i-1))&1) << (7 - b.count%8)
		b.count++
	}
}
func (b *contractBits) flag(v bool) {
	if v {
		b.write(1, 1)
	} else {
		b.write(0, 1)
	}
}
func (b *contractBits) ue(v uint32) {
	n := uint64(v) + 1
	width := uint(0)
	for x := n; x != 0; x >>= 1 {
		width++
	}
	b.write(0, width-1)
	b.write(n, width)
}

type contractSPS struct {
	profile, constraints                                                byte
	chroma, lumaDepth, chromaDepth                                      uint32
	interlaced, separate, noVUI, noTiming, noSignal, noColor, fullRange bool
	crop                                                                [4]uint32
	units, scale                                                        uint32
	primaries, transfer, matrix                                         byte
}

func contractSPSDefaults() contractSPS {
	return contractSPS{profile: 100, chroma: 1, units: 1001, scale: 60000, primaries: 1, transfer: 1, matrix: 1}
}

// SPS записывается по H.264 7.3.2.1.1 и VUI из приложения E.
// Размер без обрезки — 128×128; значения crop и VUI задаются самим тестом.
// Первичный независимый разбор: FFmpeg n7.1.5, libavcodec/h264_ps.c.
func contractSPSBytes(s contractSPS) []byte {
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
	nalu := []byte{0x67}
	zeros := 0
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

func contractAVCC(s contractSPS) []byte {
	sps := contractSPSBytes(s)
	// PPS имеет id=0 и ссылку на SPS id=0; декодируемость видеопакетов здесь не проверяется.
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	b := []byte{1, s.profile, s.constraints, 31, 0xff, 0xe1, byte(len(sps) >> 8), byte(len(sps))}
	b = append(b, sps...)
	b = append(b, 1, 0, byte(len(pps)))
	return append(b, pps...)
}

func contractVideoDescription(headers []byte) fmp4.TrackDescription {
	return fmp4.TrackDescription{ID: 9, Track: media.TrackVideo, Codec: core.CodecH264, Width: 128, Height: 128, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30000}, Headers: headers}
}

func TestContractH264RationalTimingCroppingAndColor(t *testing.T) {
	s := contractSPSDefaults()
	s.crop = [4]uint32{1, 2, 3, 4}
	s.fullRange, s.primaries, s.transfer, s.matrix = true, 9, 16, 9
	desc := contractVideoDescription(contractAVCC(s))
	// В 4:2:0 обрезаются 6 горизонтальных и 14 вертикальных яркостных отсчётов.
	desc.Width, desc.Height = 122, 114
	c, err := fmp4.ResolveCodecConfig(desc, 51, 7)
	if err != nil {
		t.Fatal(err)
	}
	want := core.VideoFormat{Codec: core.CodecH264, Width: 122, Height: 114, FrameRate: core.FrameRate{Numerator: 30000, Denominator: 1001}, CodecProfile: "high", Level: "3.1", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "full", Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc"}}
	if c.Info().Video != want || c.Info().SourceEpoch != 51 || c.Info().Revision != 7 || c.Info().TimeBase != desc.TimeBase || !bytes.Equal(c.Headers(), desc.Headers) {
		t.Fatalf("SPS не восстановлен точно: %+v", c.Info())
	}
	desc.Headers[8] = 0
	if c.Headers()[8] != 0x67 {
		t.Fatal("resolver заимствовал входные headers")
	}
}

func TestContractH264ProfilesAndChromaHaveExplicitMetadata(t *testing.T) {
	for _, tc := range []struct {
		name                string
		profile, constraint byte
		chroma, depth       uint32
		wantProfile, pixel  string
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
			s := contractSPSDefaults()
			s.profile, s.constraints, s.chroma, s.lumaDepth, s.chromaDepth = tc.profile, tc.constraint, tc.chroma, tc.depth, tc.depth
			c, err := fmp4.ResolveCodecConfig(contractVideoDescription(contractAVCC(s)), 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			v := c.Info().Video
			if v.CodecProfile != tc.wantProfile || v.PixelFormat != tc.pixel || v.BitDepth != uint8(8+tc.depth) || v.FrameRate != (core.FrameRate{Numerator: 30000, Denominator: 1001}) {
				t.Fatalf("неверное описание: %+v", v)
			}
		})
	}
}

func TestContractH264CropUnitsFollowChromaSampling(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		chroma, width, height uint32
	}{
		{"monochrome", 0, 125, 121},
		{"422", 2, 122, 121},
		{"444", 3, 125, 121},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := contractSPSDefaults()
			s.profile, s.chroma, s.crop = 244, tc.chroma, [4]uint32{1, 2, 3, 4}
			desc := contractVideoDescription(contractAVCC(s))
			desc.Width, desc.Height = tc.width, tc.height
			c, err := fmp4.ResolveCodecConfig(desc, 1, 1)
			if err != nil || c.Info().Video.Width != tc.width || c.Info().Video.Height != tc.height {
				t.Fatalf("неверные единицы обрезки: %+v, %v", c.Info().Video, err)
			}
		})
	}
}

func TestContractH264VUIColorIdentifiersAndAbsence(t *testing.T) {
	// Идентификаторы цвета заданы в FFmpeg n7.1.5, libavutil/pixfmt.h.
	for _, tc := range []struct {
		name                                   string
		p, tr, m                               byte
		rangeName, primaries, transfer, matrix string
		noSignal, noColor                      bool
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
			s := contractSPSDefaults()
			s.primaries, s.transfer, s.matrix, s.noSignal, s.noColor = tc.p, tc.tr, tc.m, tc.noSignal, tc.noColor
			c, err := fmp4.ResolveCodecConfig(contractVideoDescription(contractAVCC(s)), 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			want := core.ColorInfo{Range: tc.rangeName, Primaries: tc.primaries, Transfer: tc.transfer, Matrix: tc.matrix}
			if c.Info().Video.Color != want {
				t.Fatalf("цвет %+v вместо %+v", c.Info().Video.Color, want)
			}
		})
	}
}

func TestContractH264UnavailableTimingAndUnsupportedSyntax(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*contractSPS)
		want   error
	}{
		{"no-vui", func(s *contractSPS) { s.noVUI = true }, fmp4.ErrMetadataUnavailable},
		{"no-timing", func(s *contractSPS) { s.noTiming = true }, fmp4.ErrMetadataUnavailable},
		{"zero-timescale", func(s *contractSPS) { s.scale = 0 }, fmp4.ErrMetadataUnavailable},
		{"zero-tick", func(s *contractSPS) { s.units = 0 }, fmp4.ErrMetadataUnavailable},
		{"interlaced", func(s *contractSPS) { s.interlaced = true }, fmp4.ErrMetadataUnavailable},
		{"separate-planes", func(s *contractSPS) { s.chroma, s.separate = 3, true }, fmp4.ErrMetadataUnavailable},
		{"unequal-depths", func(s *contractSPS) { s.chromaDepth = 2 }, fmp4.ErrMetadataUnavailable},
		{"unrepresentable-fps", func(s *contractSPS) { s.units, s.scale = 0xffffffff, 1 }, fmp4.ErrUnsupportedTiming},
		{"crop-removes-width", func(s *contractSPS) { s.crop[1] = 64 }, fmp4.ErrInvalid},
		{"crop-removes-height", func(s *contractSPS) { s.crop[3] = 64 }, fmp4.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := contractSPSDefaults()
			tc.change(&s)
			_, err := fmp4.ResolveCodecConfig(contractVideoDescription(contractAVCC(s)), 1, 1)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ожидалась %v, получена %v", tc.want, err)
			}
		})
	}
}

func TestContractH264ConfigurationRejectsBrokenParameterSetFraming(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"short-avcc", func(b []byte) []byte { return b[:7] }},
		{"wrong-version", func(b []byte) []byte { b[0] = 0; return b }},
		{"two-byte-nal-length", func(b []byte) []byte { b[4] = 0xfd; return b }},
		{"multiple-sps", func(b []byte) []byte { b[5] = 0xe2; return b }},
		{"zero-sps-size", func(b []byte) []byte { b[6], b[7] = 0, 0; return b }},
		{"oversized-sps", func(b []byte) []byte { b[6], b[7] = 0xff, 0xff; return b }},
		{"wrong-sps-nalu", func(b []byte) []byte { b[8] = 0x68; return b }},
		{"truncated-sps-syntax", func(b []byte) []byte { return []byte{1, 100, 0, 31, 0xff, 0xe1, 0, 1, 0x67, 1, 0, 1, 0x68} }},
		{"missing-pps", func(b []byte) []byte { n := int(binary.BigEndian.Uint16(b[6:8])); b[8+n] = 0; return b }},
		{"truncated-pps-size", func(b []byte) []byte { n := int(binary.BigEndian.Uint16(b[6:8])); return b[:8+n+2] }},
		{"zero-pps-size", func(b []byte) []byte { n := int(binary.BigEndian.Uint16(b[6:8])); b[9+n], b[10+n] = 0, 0; return b }},
		{"truncated-pps-body", func(b []byte) []byte { return b[:len(b)-1] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc := contractVideoDescription(tc.change(contractAVCC(contractSPSDefaults())))
			_, err := fmp4.ResolveCodecConfig(desc, 1, 1)
			if !errors.Is(err, fmp4.ErrInvalid) {
				t.Fatalf("повреждённые headers приняты: %v", err)
			}
		})
	}
}

func TestContractAACResolverUsesActualASCAndRejectsUnsupportedDescriptions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		headers  []byte
		rate     uint32
		channels uint16
		layout   string
		reject   bool
	}{
		{"mono-44100", []byte{0x12, 0x08}, 44100, 1, "mono", false},
		{"stereo-48000", []byte{0x11, 0x90}, 48000, 2, "stereo", false},
		{"truncated", []byte{0x11}, 48000, 2, "", true},
		{"aac-main", []byte{0x09, 0x90}, 48000, 2, "", true},
		{"multichannel", []byte{0x11, 0xb0}, 48000, 6, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc := fmp4.TrackDescription{Track: media.TrackAudio, Codec: core.CodecAAC, SampleRateHz: tc.rate, Channels: tc.channels, Headers: tc.headers, TimeBase: media.TimeBase{Numerator: 1, Denominator: tc.rate}}
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
