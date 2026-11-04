package fallback

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// Source содержит фактически проверенный тип входа и конечную продолжительность.
type Source struct {
	Kind          core.AssetKind
	Format        core.SourceFormat
	Duration      time.Duration
	Width, Height uint32
	Demuxer       string
}
type GenerateRequest struct {
	Source       Source
	Profile      core.GroupProfile
	LoopDuration time.Duration
}
type CommandBuilder func(GenerateRequest) (Command, error)

// ProductionBuilder выбирает libsvtav1 либо h264_nvenc без неявной замены кодировщика.
func ProductionBuilder(binary string) CommandBuilder {
	return func(req GenerateRequest) (Command, error) { return buildCommand(binary, req, "") }
}

func buildCommand(binary string, req GenerateRequest, testEncoder string) (Command, error) {
	p := req.Profile
	if p.Validate() != nil || binary == "" || req.Source.Duration <= 0 || req.LoopDuration < req.Source.Duration {
		return Command{}, ErrInvalid
	}
	if aligned, err := EffectiveLoopDuration(req.LoopDuration, p, req.LoopDuration); err != nil || aligned != req.LoopDuration {
		return Command{}, ErrInvalid
	}
	v, a, e := p.Format.Video, p.Format.Audio, p.VideoEncoding
	if v.Width%2 != 0 || v.Height%2 != 0 || v.Width > math.MaxInt32 || v.Height > math.MaxInt32 || v.PixelFormat != "yuv420p" || v.BitDepth != 8 || !e.ClosedGOP || e.GOPFrames > math.MaxInt32 || e.MaxBFrames > math.MaxInt32 || e.BufferBits > math.MaxInt32 || e.TargetBitrateBPS > math.MaxInt64 || e.MaxBitrateBPS > math.MaxInt64 || p.AudioEncoding.BitrateBPS > math.MaxInt64 {
		return Command{}, ErrInvalid
	}
	validLayout := a.Channels == 1 && a.ChannelLayout == "mono" || a.Channels == 2 && a.ChannelLayout == "stereo"
	if a.CodecProfile != "LC" || !validLayout || a.SampleRateHz == 0 {
		return Command{}, ErrInvalid
	}
	if v.Color.Primaries != "bt709" || v.Color.Transfer != "bt709" || v.Color.Matrix != "bt709" || (v.Color.Range != "limited" && v.Color.Range != "full" && v.Color.Range != "tv" && v.Color.Range != "pc") {
		return Command{}, fmt.Errorf("%w: поддержан SDR bt709", ErrInvalid)
	}
	if req.Source.Kind != core.AssetImage && req.Source.Kind != core.AssetVideo {
		return Command{}, ErrInvalid
	}
	if source := req.Source.Format.Video; source != nil && (source.Color.Transfer == "smpte2084" || source.Color.Transfer == "arib-std-b67" || source.BitDepth != 8 || source.PixelFormat != "yuv420p") {
		return Command{}, fmt.Errorf("%w: неподдержанный HDR/цветовой формат источника", ErrInvalid)
	}
	if req.Source.Demuxer != "mov" && req.Source.Demuxer != "matroska" && req.Source.Demuxer != "png_pipe" && req.Source.Demuxer != "jpeg_pipe" {
		return Command{}, ErrInvalid
	}
	fps := v.FrameRate
	g := gcd(uint64(fps.Numerator), uint64(fps.Denominator))
	fps.Numerator /= uint32(g)
	fps.Denominator /= uint32(g)
	scale, err := fmp4.MovieTimeScale(media.TimeBase{Numerator: 1, Denominator: fps.Numerator}, media.TimeBase{Numerator: 1, Denominator: a.SampleRateHz})
	if err != nil || scale > math.MaxInt32 || fps.Numerator > math.MaxInt32 {
		return Command{}, ErrInvalid
	}
	n := func(v uint64) string { return strconv.FormatUint(v, 10) }
	duration := durationArgument(req.LoopDuration)
	padding := req.LoopDuration - req.Source.Duration
	if padding%time.Microsecond != 0 {
		padding = (padding/time.Microsecond + 1) * time.Microsecond
	}
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-xerror", "-protocol_whitelist", "fd,pipe", "-format_whitelist", req.Source.Demuxer, "-f", req.Source.Demuxer}
	if req.Source.Kind == core.AssetImage {
		args = append(args, "-loop", "1", "-framerate", fmt.Sprintf("%d/%d", fps.Numerator, fps.Denominator))
	}
	args = append(args, "-fd", "3", "-i", "fd:")
	if req.Source.Format.Audio == nil {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("anullsrc=r=%d:cl=%s", a.SampleRateHz, a.ChannelLayout))
	}
	args = append(args, "-map", "0:v:0", "-map")
	if req.Source.Format.Audio == nil {
		args = append(args, "1:a:0")
	} else {
		args = append(args, "0:a:0")
	}
	encoder := "h264_nvenc"
	switch v.Codec {
	case core.CodecAV1:
		if v.CodecProfile != "main" || v.Tier != "main" && v.Tier != "" || e.MaxBFrames != 0 {
			return Command{}, ErrInvalid
		}
		encoder = "libsvtav1"
		if e.TargetBitrateBPS != e.MaxBitrateBPS || e.BufferBits*1000%e.TargetBitrateBPS != 0 || e.BufferBits*1000/e.TargetBitrateBPS < 20 || e.BufferBits*1000/e.TargetBitrateBPS > 10000 {
			return Command{}, fmt.Errorf("%w: AV1 поддерживает CBR с точным буфером 20–10000 ms", ErrInvalid)
		}
	case core.CodecH264:
		if v.Tier != "" || v.CodecProfile != "baseline" && v.CodecProfile != "main" && v.CodecProfile != "high" || v.CodecProfile == "baseline" && e.MaxBFrames != 0 {
			return Command{}, ErrInvalid
		}
	default:
		return Command{}, ErrInvalid
	}
	if testEncoder != "" {
		encoder = testEncoder
	}
	args = append(args, "-vf", fmt.Sprintf("fps=fps=%d/%d:round=near:eof_action=round,tpad=stop_mode=clone:stop_duration=%s,scale=w=%d:h=%d,format=yuv420p,setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709", fps.Numerator, fps.Denominator, durationArgument(padding), v.Width, v.Height), "-c:v", encoder, "-b:v", n(e.TargetBitrateBPS), "-maxrate:v", n(e.MaxBitrateBPS), "-bufsize:v", n(e.BufferBits), "-g:v", n(uint64(e.GOPFrames)), "-flags:v", "+cgop", "-fps_mode:v", "passthrough", "-enc_time_base:v", "filter")
	if v.Codec == core.CodecAV1 {
		args = append(args, "-svtav1-params", "rc=2:mbr=0:pred-struct=1:scd=0:enable-overlays=0:enable-dg=0:hierarchical-levels=0:enable-tf=0", "-preset", "8")
	} else {
		args = append(args, "-profile:v", v.CodecProfile, "-bf:v", n(uint64(e.MaxBFrames)))
		if encoder == "h264_nvenc" {
			args = append(args, "-no-scenecut", "1", "-forced-idr", "1")
		}
		if encoder == "libx264" {
			args = append(args, "-sc_threshold", "0")
		}
	}
	if v.Level != "" && v.Level != "auto" {
		args = append(args, "-level:v", v.Level)
	}
	args = append(args, "-af", "apad=pad_dur="+duration)
	rangeName := v.Color.Range
	if rangeName == "limited" {
		rangeName = "tv"
	}
	if rangeName == "full" {
		rangeName = "pc"
	}
	args = append(args, "-color_range:v", rangeName, "-color_primaries:v", v.Color.Primaries, "-color_trc:v", v.Color.Transfer, "-colorspace:v", v.Color.Matrix, "-c:a", "aac", "-profile:a", "aac_low", "-b:a", n(p.AudioEncoding.BitrateBPS), "-ar:a", n(uint64(a.SampleRateHz)), "-ac:a", n(uint64(a.Channels)), "-channel_layout:a", a.ChannelLayout, "-t", duration, "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", n(uint64(scale)), "-video_track_timescale", n(uint64(fps.Numerator)), "-avoid_negative_ts", "disabled", "pipe:1")
	return Command{Binary: binary, Args: args}, nil
}
func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
