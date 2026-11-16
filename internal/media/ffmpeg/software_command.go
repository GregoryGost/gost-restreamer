package ffmpeg

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/encoding"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// buildSoftwareCommand преобразует фактический цвет c программным декодером и zscale,
// сохраняя входные временные шкалы и UDS из a. Кодировщик и аудиопривязку определяет полный профиль.
// HDR сохраняет transfer, диапазон и глубину; недостоверные исходные сведения отклоняются до процесса.
func buildSoftwareCommand(c NVIDIAConfig, a Attempt) (Command, error) {
	if c.Input.Validate() != nil || c.Profile.Validate() != nil || c.VideoTimeBase.Validate() != nil || c.AudioTimeBase.Validate() != nil ||
		c.Profile.Mode != core.ModeTranscode ||
		!validCommand(c.Binary, nil, 4096) || !filepath.IsAbs(a.InputPath) || !filepath.IsAbs(a.OutputPath) || strings.ContainsRune(a.InputPath, 0) || strings.ContainsRune(a.OutputPath, 0) {
		return Command{}, ErrInvalidSupervisor
	}
	if !oneOf(string(c.Input.Video.Codec), string(core.CodecAV1), string(core.CodecHEVC), string(core.CodecVP9)) ||
		!oneOf(string(c.Profile.Format.Video.Codec), string(core.CodecH264), string(core.CodecHEVC), string(core.CodecVP9)) {
		return Command{}, ErrInvalidSupervisor
	}

	// p содержит проверенный целевой профиль, v — его видео, au — целевое аудио.
	p, v, au := c.Profile, c.Profile.Format.Video, c.Profile.Format.Audio
	if p.EffectiveLiveAudioMode() == core.LiveAudioCopy && c.Input.Audio != au {
		if au.Codec == core.CodecAAC {
			return Command{}, fmt.Errorf("%w: несовместимый LIVE AAC copy", ErrInvalidSupervisor)
		}
		return Command{}, fmt.Errorf("%w: несовместимый LIVE audio copy", ErrInvalidSupervisor)
	}

	// conversion преобразует фактические плоскости и разрешённый цвет; err сообщает о неподдержанном преобразовании.
	conversion, err := encoding.SoftwareVideoFilter(c.Input.Video, v, p.Encoding.RescaleOutput)
	if err != nil {
		return Command{}, fmt.Errorf("%w: %w", ErrInvalidSupervisor, err)
	}
	// pixel задаёт физическое представление кадров после программного фильтра.
	pixel := v.PixelFormat
	if p.Encoding.VideoEncoder == core.VideoEncoderHEVCNVENC && v.BitDepth == 10 {
		// NVENC получает те же десятибитные отсчёты в полуплоскостном P010 без смены диапазона.
		pixel = "p010le"
		conversion += ",format=p010le"
	}
	// fps содержит сокращённую точную частоту целевого видео.
	fps, err := filterFrameRate(v.FrameRate)
	if err != nil {
		return Command{}, err
	}
	// videoScale задаёт точную шкалу видео; movieScale точно представляет вход и обе выходные дорожки.
	videoScale := fps.Numerator
	movieScale, err := fmp4.MovieTimeScale(c.VideoTimeBase, c.AudioTimeBase,
		media.TimeBase{Numerator: 1, Denominator: videoScale}, media.TimeBase{Numerator: 1, Denominator: c.Input.Audio.SampleRateHz}, media.TimeBase{Numerator: 1, Denominator: au.SampleRateHz})
	if err != nil || movieScale > math.MaxInt32 || videoScale > math.MaxInt32 {
		return Command{}, ErrInvalidSupervisor
	}
	// plan содержит рецепты явно выбранных видео- и аудиокодировщиков.
	plan, err := encoding.Compile(p, encoding.Live)
	if err != nil {
		return Command{}, fmt.Errorf("%w: %w", ErrInvalidSupervisor, err)
	}
	// filter задаёт FPS и реальное преобразование цвета с программным декодированием и фильтрацией.
	filter := fmt.Sprintf("fps=fps=%d/%d:round=near:eof_action=round,%s", fps.Numerator, fps.Denominator, conversion)
	// args сохраняет copyts, skip_manual, начало транспортного времени и фрагментацию прежнего порта.
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-progress", "pipe:1", "-stats_period", "2", "-nostats", "-xerror", "-copyts", "-flags2:a", "+skip_manual", "-f", "mp4", "-fflags", "+ignidx", "-i", "async:unix:" + a.InputPath,
		"-map", "0:v:0", "-map", "0:a:0", "-vf", filter, "-pix_fmt:v", pixel, "-fps_mode:v", "passthrough", "-enc_time_base:v", "filter"}
	args = append(args, plan.Video...)
	args = append(args, plan.Audio...)
	args = append(args, "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", strconv.FormatUint(uint64(movieScale), 10), "-video_track_timescale", strconv.FormatUint(uint64(videoScale), 10), "-avoid_negative_ts", "disabled", "unix:"+a.OutputPath)
	return Command{Binary: c.Binary, Args: args}, nil
}
