// Package encoding преобразует проверенный профиль в параметры кодировщиков FFmpeg.
package encoding

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

type Target uint8

const (
	Live Target = iota + 1
	Prepared
)

// Plan содержит только параметры кодеков; транспортом и фильтрами владеет вызывающий.
type Plan struct{ Video, Audio []string }

// Compile сохраняет различия прежних профилей через единый семантический адаптер.
func Compile(p core.GroupProfile, target Target) (Plan, error) {
	if err := p.Validate(); err != nil {
		return Plan{}, err
	}
	if target != Live && target != Prepared || target == Live && p.Mode != core.ModeTranscode {
		return Plan{}, core.ErrInvalid
	}
	v, a, e, settings := p.Format.Video, p.Format.Audio, p.VideoEncoding, p.Encoding
	legacy := settings.Version == 0
	n := func(v uint64) string { return strconv.FormatUint(v, 10) }
	encoder := settings.VideoEncoder
	if legacy {
		encoder = "h264_nvenc"
		if v.Codec == core.CodecAV1 {
			encoder = "libsvtav1"
		}
	}
	video := []string{"-c:v", encoder, "-b:v", n(e.TargetBitrateBPS)}
	if v.Codec != core.CodecAV1 || legacy || settings.RateControl == "cbr" {
		video = append(video, "-maxrate:v", n(e.MaxBitrateBPS), "-bufsize:v", n(e.BufferBits))
	}
	video = append(video, "-g:v", n(uint64(e.GOPFrames)), "-flags:v", "+cgop")
	switch v.Codec {
	case core.CodecH264:
		video = append(video, "-profile:v", v.CodecProfile, "-bf:v", n(uint64(e.MaxBFrames)), "-no-scenecut", "1", "-forced-idr", "1")
		if !legacy || target == Live {
			rc := settings.RateControl
			if legacy {
				rc = "vbr"
			}
			video = append(video, "-rc", rc)
		}
		if !legacy {
			nv := settings.NVENC
			flag := func(v bool) string {
				if v {
					return "1"
				}
				return "0"
			}
			video = append(video, "-preset", nv.Preset, "-tune", nv.Tuning, "-multipass", nv.Multipass,
				"-rc-lookahead", strconv.Itoa(nv.LookaheadFrames), "-spatial-aq", flag(nv.EnableSpatialAQ),
				"-temporal-aq", flag(nv.EnableTemporalAQ), "-aq-strength", strconv.Itoa(nv.AQStrength))
			video = appendCustom(video, settings.VideoCustom)
		}
	case core.CodecAV1:
		if target != Prepared {
			return Plan{}, core.ErrInvalid
		}
		preset, params := "8", "rc=2:mbr=0:pred-struct=1:scd=0:enable-overlays=0:enable-dg=0:hierarchical-levels=0:enable-tf=0"
		if !legacy {
			svt := settings.SVTAV1
			preset = strconv.Itoa(svt.Preset)
			rc, prediction := "2", "1"
			if settings.RateControl == "vbr" {
				rc, prediction = "1", "2"
			}
			tuning := 1
			if svt.Tuning == "vq" {
				tuning = 0
			}
			params = fmt.Sprintf("rc=%s:mbr=0:pred-struct=%s:scd=0:enable-overlays=0:enable-dg=0:hierarchical-levels=0:enable-tf=0:aq-mode=%d:tune=%d", rc, prediction, svt.AQMode, tuning)
			if svt.LookaheadFrames >= 0 {
				params += ":lookahead=" + strconv.Itoa(svt.LookaheadFrames)
			}
			if settings.VideoCustom != "" {
				params += ":" + strings.ReplaceAll(settings.VideoCustom, " ", ":")
			}
		}
		video = append(video, "-svtav1-params", params, "-preset", preset)
	default:
		return Plan{}, core.ErrInvalid
	}
	if v.Level != "" && (target == Live || v.Level != "auto") {
		video = append(video, "-level:v", v.Level)
	}
	rangeName := v.Color.Range
	if rangeName == "limited" {
		rangeName = "tv"
	}
	if rangeName == "full" {
		rangeName = "pc"
	}
	video = append(video, "-color_range:v", rangeName, "-color_primaries:v", v.Color.Primaries, "-color_trc:v", v.Color.Transfer, "-colorspace:v", v.Color.Matrix)
	audio := []string{"-c:a", "aac", "-profile:a", "aac_low", "-b:a", n(p.AudioEncoding.BitrateBPS), "-ar:a", n(uint64(a.SampleRateHz)), "-ac:a", n(uint64(a.Channels)), "-channel_layout:a", a.ChannelLayout}
	if !legacy {
		audio = appendCustom(audio, settings.AudioCustom)
	}
	return Plan{Video: video, Audio: audio}, nil
}

func appendCustom(args []string, custom string) []string {
	for pair := range strings.FieldsSeq(custom) {
		name, value, _ := strings.Cut(pair, "=")
		args = append(args, "-"+name, value)
	}
	return args
}
