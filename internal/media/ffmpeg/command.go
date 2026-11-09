package ffmpeg

import (
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/encoding"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// NVIDIAConfig задаёт наблюдаемый вход, выходной профиль и фактические шкалы обеих дорожек.
// FPS и размеры преобразуются по профилю; преобразование HDR не поддерживается.
type NVIDIAConfig struct {
	Binary        string
	Input         core.MediaFormat
	Profile       core.GroupProfile
	VideoTimeBase media.TimeBase
	AudioTimeBase media.TimeBase
}

// BuildNVIDIACommand строит отдельные argv для AV1 NVDEC → H.264 NVENC/AAC.
// Неподдержанные настройки отвергаются; автоматического CPU fallback нет.
func BuildNVIDIACommand(c NVIDIAConfig, a Attempt) (Command, error) {
	invalid := func(detail string) (Command, error) {
		return Command{}, fmt.Errorf("%w: %s", ErrInvalidSupervisor, detail)
	}
	if c.Input.Validate() != nil || c.Profile.Validate() != nil || c.VideoTimeBase.Validate() != nil || c.AudioTimeBase.Validate() != nil {
		return invalid("неполный медиапрофиль")
	}
	p := c.Profile
	v, au := p.Format.Video, p.Format.Audio
	if p.Mode != core.ModeTranscode {
		return invalid("copy не запускает encoder")
	}
	if c.Input.Video.Codec != core.CodecAV1 || v.Codec != core.CodecH264 || au.Codec != core.CodecAAC {
		return invalid("нужны AV1 → H.264 и AAC")
	}
	if p.EffectiveLiveAudioMode() == core.LiveAudioCopy && c.Input.Audio != au {
		return invalid(fmt.Sprintf("LIVE AAC copy: входной формат %+v несовместим с ожидаемым %+v", c.Input.Audio, au))
	}
	if v.PixelFormat != "yuv420p" || v.BitDepth != 8 || v.Width%2 != 0 || v.Height%2 != 0 || v.Width > math.MaxInt32 || v.Height > math.MaxInt32 {
		return invalid("поддержан только чётный H.264 yuv420p 8-bit")
	}
	if c.Input.Video.PixelFormat != "yuv420p" || c.Input.Video.BitDepth != 8 || c.Input.Video.Color != v.Color {
		return invalid("цвет/битность входа должны соответствовать выходу")
	}
	if v.Tier != "" {
		return invalid("H.264 не имеет Tier")
	}
	if !oneOf(v.CodecProfile, "baseline", "main", "high") || v.CodecProfile == "baseline" && p.VideoEncoding.MaxBFrames != 0 {
		return invalid("неподдержанный профиль H.264")
	}
	if !oneOf(v.Level, "", "auto", "1", "1.0", "1b", "1.0b", "1.1", "1.2", "1.3", "2", "2.0", "2.1", "2.2", "3", "3.0", "3.1", "3.2", "4", "4.0", "4.1", "4.2", "5", "5.0", "5.1", "5.2") {
		return invalid("неподдержанный Level")
	}
	if !p.VideoEncoding.ClosedGOP {
		return invalid("порт NVENC поддерживает закрытый GOP")
	}
	if p.VideoEncoding.GOPFrames > math.MaxInt32 || p.VideoEncoding.MaxBFrames > math.MaxInt32 || p.VideoEncoding.TargetBitrateBPS > math.MaxInt64 || p.VideoEncoding.MaxBitrateBPS > math.MaxInt64 || p.VideoEncoding.BufferBits > math.MaxInt32 || p.AudioEncoding.BitrateBPS > math.MaxInt64 {
		return invalid("параметры превышают диапазон FFmpeg")
	}
	validLayout := au.Channels == 1 && au.ChannelLayout == "mono" || au.Channels == 2 && au.ChannelLayout == "stereo"
	if au.CodecProfile != "LC" || !oneOf(strconv.FormatUint(uint64(au.SampleRateHz), 10), "96000", "88200", "64000", "48000", "44100", "32000", "24000", "22050", "16000", "12000", "11025", "8000", "7350") || !validLayout {
		return invalid("поддержан AAC LC mono/stereo с известной частотой")
	}
	if !oneOf(v.Color.Range, "limited", "full", "tv", "pc") || !oneOf(v.Color.Primaries, "bt709", "bt470m", "bt470bg", "smpte170m", "smpte240m", "film", "bt2020", "unspecified") || !oneOf(v.Color.Transfer, "bt709", "gamma22", "gamma28", "smpte170m", "smpte240m", "linear", "bt2020-10", "bt2020-12", "unspecified") || !oneOf(v.Color.Matrix, "bt709", "fcc", "bt470bg", "smpte170m", "smpte240m", "bt2020nc", "bt2020c", "unspecified") {
		return invalid("неподдержанное описание цвета SDR")
	}
	if !validCommand(c.Binary, nil, 4096) || !filepath.IsAbs(a.InputPath) || !filepath.IsAbs(a.OutputPath) || strings.ContainsRune(a.InputPath, 0) || strings.ContainsRune(a.OutputPath, 0) {
		return invalid("некорректные binary/UDS")
	}
	fps, err := filterFrameRate(v.FrameRate)
	if err != nil {
		return Command{}, err
	}
	videoScale := fps.Numerator
	movieScale, err := fmp4.MovieTimeScale(c.VideoTimeBase, c.AudioTimeBase,
		media.TimeBase{Numerator: 1, Denominator: videoScale},
		media.TimeBase{Numerator: 1, Denominator: c.Input.Audio.SampleRateHz},
		media.TimeBase{Numerator: 1, Denominator: au.SampleRateHz})
	if err != nil {
		return Command{}, err
	}
	if movieScale > math.MaxInt32 || videoScale > math.MaxInt32 {
		return invalid("timescale превышает диапазон FFmpeg")
	}
	n := func(x uint64) string { return strconv.FormatUint(x, 10) }
	plan, err := encoding.Compile(p, encoding.Live)
	if err != nil {
		return invalid("неподдержанный профиль кодирования")
	}
	filter := fmt.Sprintf("fps=fps=%d/%d:round=near:eof_action=round", fps.Numerator, fps.Denominator)
	if p.Encoding.Version == 0 || p.Encoding.RescaleOutput {
		filter += fmt.Sprintf(",scale_cuda=w=%d:h=%d:format=yuv420p", v.Width, v.Height)
	} else if c.Input.Video.Width != v.Width || c.Input.Video.Height != v.Height {
		return invalid("rescale_output=false требует совпадения входных и выходных размеров")
	}
	// async сохраняет конечное окно для обратного чтения заголовков; ignidx останавливает поиск на текущем mdat.
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-progress", "pipe:1", "-stats_period", "2", "-nostats", "-xerror", "-copyts", "-hwaccel", "cuda", "-hwaccel_output_format", "cuda", "-c:v", "av1_cuvid", "-flags2:a", "+skip_manual", "-f", "mp4", "-fflags", "+ignidx", "-i", "async:unix:" + a.InputPath,
		"-map", "0:v:0", "-map", "0:a:0", "-vf", filter, "-pix_fmt", "cuda", "-fps_mode:v", "passthrough", "-enc_time_base:v", "filter"}
	args = append(args, plan.Video...)
	args = append(args, plan.Audio...)
	args = append(args, "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", n(uint64(movieScale)), "-video_track_timescale", n(uint64(videoScale)), "-avoid_negative_ts", "disabled", "unix:"+a.OutputPath)
	return Command{Binary: c.Binary, Args: args}, nil
}

func oneOf(s string, values ...string) bool {
	return slices.Contains(values, s)
}
