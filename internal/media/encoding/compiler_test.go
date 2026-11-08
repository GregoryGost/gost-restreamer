package encoding

import (
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"reflect"
	"strings"
	"testing"
)

func compilerProfile(codec core.Codec) core.GroupProfile {
	p := core.GroupProfile{Mode: core.ModeTranscode, Format: core.MediaFormat{Video: core.VideoFormat{Codec: codec, Width: 128, Height: 128, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: "high", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 100000, MaxBitrateBPS: 200000, BufferBits: 200000, GOPFrames: 15, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 64000}}
	if codec == core.CodecAV1 {
		p.VideoEncoding.MaxBitrateBPS = p.VideoEncoding.TargetBitrateBPS
		p.Mode = core.ModeCopy
		p.Format.Video.CodecProfile = "main"
		p.Format.Video.Tier = "main"
	}
	return p
}

func TestCompilerExplicitRateControlAndEveryEncoderOption(t *testing.T) {
	for _, codec := range []core.Codec{core.CodecH264, core.CodecAV1} {
		for _, rc := range []string{"cbr", "vbr"} {
			t.Run(string(codec)+"/"+rc, func(t *testing.T) {
				p := compilerProfile(codec)
				encoder := "h264_nvenc"
				if codec == core.CodecAV1 {
					encoder = "libsvtav1"
					if rc == "vbr" {
						p.VideoEncoding.MaxBitrateBPS = 0
						p.VideoEncoding.BufferBits = 0
					}
				} else if rc == "cbr" {
					p.VideoEncoding.MaxBitrateBPS = p.VideoEncoding.TargetBitrateBPS
				}
				p.Encoding = core.DefaultEncoding(p.Mode, encoder, rc)
				p.Encoding.AudioCustom = "aac_coder=fast"
				if codec == core.CodecH264 {
					p.Encoding.NVENC = core.NVENCSettings{Preset: "p6", Tuning: "ll", Multipass: "fullres", LookaheadFrames: 12, EnableSpatialAQ: true, EnableTemporalAQ: true, AQStrength: 9}
					p.Encoding.VideoCustom = "nonref_p=1 strict_gop=1"
				} else {
					p.Encoding.SVTAV1.Preset = 10
					p.Encoding.VideoCustom = "lp=2"
					if rc == "vbr" {
						p.Encoding.SVTAV1.Tuning = "vq"
						p.Encoding.SVTAV1.LookaheadFrames = 6
					}
				}
				before := p
				prepared, err := Compile(p, Prepared)
				if err != nil {
					t.Fatal(err)
				}
				audio := strings.Join(prepared.Audio, " ")
				if !strings.Contains(audio, "-aac_coder fast") {
					t.Fatal(audio)
				}
				video := strings.Join(prepared.Video, " ")
				if codec == core.CodecH264 {
					for _, want := range []string{"-rc " + rc, "-preset p6", "-tune ll", "-multipass fullres", "-rc-lookahead 12", "-spatial-aq 1", "-temporal-aq 1", "-aq-strength 9", "-nonref_p 1", "-strict_gop 1"} {
						if !strings.Contains(video, want) {
							t.Fatal(want, video)
						}
					}
					live, e := Compile(p, Live)
					if e != nil || !reflect.DeepEqual(live, prepared) {
						t.Fatal(live, prepared, e)
					}
				} else {
					params := "rc=2:mbr=0:pred-struct=1"
					if rc == "vbr" {
						params = "rc=1:mbr=0:pred-struct=2"
					}
					for _, want := range []string{params, "hierarchical-levels=0", "scd=0", "enable-tf=0", "enable-dg=0", "enable-overlays=0", "aq-mode=2", "lp=2", "-preset 10"} {
						if !strings.Contains(video, want) {
							t.Fatal(want, video)
						}
					}
					if rc == "vbr" && (strings.Contains(video, "-maxrate") || strings.Contains(video, "-bufsize") || !strings.Contains(video, "tune=0:lookahead=6")) {
						t.Fatal(video)
					}
					if rc == "cbr" && !strings.Contains(video, "tune=1:lookahead=0") {
						t.Fatal(video)
					}
				}
				if p != before {
					t.Fatal("compiler изменил профиль")
				}
			})
		}
	}
}
func TestCompilerLegacySemanticsAndAutoLookahead(t *testing.T) {
	p := compilerProfile(core.CodecH264)
	live, err := Compile(p, Live)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := Compile(p, Prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(live.Video, " "), "-rc vbr") || strings.Contains(strings.Join(prepared.Video, " "), "-rc ") {
		t.Fatal(live, prepared)
	}
	if strings.Contains(strings.Join(live.Video, " "), "-preset") {
		t.Fatal(live)
	}
	p = compilerProfile(core.CodecAV1)
	prepared, err = Compile(p, Prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(prepared.Video, " "), "rc=2:mbr=0:pred-struct=1") {
		t.Fatal(prepared)
	}
	p.Encoding = core.DefaultEncoding(p.Mode, "libsvtav1", "vbr")
	p.VideoEncoding.MaxBitrateBPS = 0
	p.VideoEncoding.BufferBits = 0
	prepared, err = Compile(p, Prepared)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(prepared.Video, " "), "lookahead=") {
		t.Fatal("auto требует отсутствия override", prepared)
	}
	if _, err = Compile(p, Live); err == nil {
		t.Fatal("copy запустил encoder LIVE")
	}
	if _, err = Compile(p, 0); err == nil {
		t.Fatal("неизвестная цель принята")
	}
	p.Encoding.VideoCustom = "rc=1"
	if _, err = Compile(p, Prepared); err == nil {
		t.Fatal("конфликт прошёл compiler")
	}
}
