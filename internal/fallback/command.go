package fallback

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/encoding"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// Source содержит фактически проверенный тип входа и конечную продолжительность.
type Source struct {
	// Kind определяет, является ли проверенный вход изображением или видео.
	Kind core.AssetKind
	// Format содержит фактически наблюдаемый формат входных дорожек.
	Format core.SourceFormat
	// Duration задаёт проверенную конечную длительность источника.
	Duration time.Duration
	// Width задаёт ширину входного изображения или видео в пикселях. Height задаёт его высоту в
	// пикселях.
	Width, Height uint32
	// Demuxer содержит проверенное имя демультиплексора FFmpeg для входного файла.
	Demuxer string
}

// GenerateRequest объединяет проверенный источник, выходной профиль и точную длительность создаваемой
// петли.
type GenerateRequest struct {
	// Source содержит результаты проверки входного файла.
	Source Source
	// Profile задаёт выходной формат и параметры кодирования заглушки.
	Profile core.GroupProfile
	// LoopDuration задаёт длительность петли, выровненную по сеткам полных видео- и аудиоединиц.
	LoopDuration time.Duration
}

// CommandBuilder строит команду подготовки для проверенного запроса и сообщает о неподдержанных
// параметрах.
type CommandBuilder func(GenerateRequest) (Command, error)

// ProductionBuilder выбирает libsvtav1 либо h264_nvenc без неявной замены кодировщика.
func ProductionBuilder(binary string) CommandBuilder {
	return func(req GenerateRequest) (Command, error) {
		// Строит команду подготовки с выбранным binary и рабочим кодировщиком для профиля запроса.
		return buildCommand(binary, req, "")
	}
}

// Собирает команду подготовки с входом FD 3 и отдельной AAC-тишиной; отвергает неподдержанные профили
// и неточную длину петли. testEncoder задаёт явную подстановку для компонентных тестов.
func buildCommand(binary string, req GenerateRequest, testEncoder string) (Command, error) {
	// p содержит выходной профиль текущего запроса подготовки.
	p := req.Profile
	if p.Validate() != nil || binary == "" || req.Source.Duration <= 0 || req.LoopDuration < req.Source.Duration {
		return Command{}, ErrInvalid
	}
	// aligned содержит длительность после выравнивания по общей сетке. err сообщает о невозможности
	// точного выравнивания в заданный предел.
	if aligned, err := EffectiveLoopDuration(req.LoopDuration, p, req.LoopDuration); err != nil || aligned != req.LoopDuration {
		return Command{}, ErrInvalid
	}
	// v содержит выходной формат видео. a содержит выходной формат аудио. e содержит ограничения
	// видеокодирования.
	v, a, e := p.Format.Video, p.Format.Audio, p.VideoEncoding
	if v.Width%2 != 0 || v.Height%2 != 0 || v.Width > math.MaxInt32 || v.Height > math.MaxInt32 || v.PixelFormat != "yuv420p" || v.BitDepth != 8 || !e.ClosedGOP || e.GOPFrames > math.MaxInt32 || e.MaxBFrames > math.MaxInt32 || e.BufferBits > math.MaxInt32 || e.TargetBitrateBPS > math.MaxInt64 || e.MaxBitrateBPS > math.MaxInt64 || p.AudioEncoding.BitrateBPS > math.MaxInt64 {
		return Command{}, ErrInvalid
	}
	// validLayout показывает, согласованы ли число каналов и обозначение mono либо stereo.
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
	// source содержит необязательное описание входного видео для проверки ограничений цвета.
	if source := req.Source.Format.Video; source != nil && (source.Color.Transfer == "smpte2084" || source.Color.Transfer == "arib-std-b67" || source.BitDepth != 8 || source.PixelFormat != "yuv420p") {
		return Command{}, fmt.Errorf("%w: неподдержанный HDR/цветовой формат источника", ErrInvalid)
	}
	if req.Source.Demuxer != "mov" && req.Source.Demuxer != "matroska" && req.Source.Demuxer != "png_pipe" && req.Source.Demuxer != "jpeg_pipe" {
		return Command{}, ErrInvalid
	}
	// fps содержит сокращаемую частоту выходного видео.
	fps := v.FrameRate
	// g содержит общий делитель числителя и знаменателя FPS.
	g := gcd(uint64(fps.Numerator), uint64(fps.Denominator))
	fps.Numerator /= uint32(g)
	fps.Denominator /= uint32(g)
	// scale содержит общую точную шкалу времени контейнера. err сообщает о невозможности подобрать
	// представимую шкалу.
	scale, err := fmp4.MovieTimeScale(media.TimeBase{Numerator: 1, Denominator: fps.Numerator}, media.TimeBase{Numerator: 1, Denominator: a.SampleRateHz})
	if err != nil || scale > math.MaxInt32 || fps.Numerator > math.MaxInt32 {
		return Command{}, ErrInvalid
	}
	// videoScale содержит шкалу видеодорожки с точным шагом кадра, пригодную для FFmpeg.
	videoScale, err := preparedVideoScale(fps, scale)
	if err != nil {
		return Command{}, err
	}
	// n преобразует числовой параметр FFmpeg в десятичную строку.
	n := func(v uint64) string {
		// Представляет числовые параметры FFmpeg десятичными строками без знака.
		return strconv.FormatUint(v, 10)
	}
	// duration содержит длительность петли в секундах с точностью до микросекунды для аргументов
	// FFmpeg.
	duration := durationArgument(req.LoopDuration)
	// args накапливает отдельные аргументы FFmpeg для чтения входа, создания тишины и записи fMP4.
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-xerror", "-protocol_whitelist", "fd,pipe", "-format_whitelist", req.Source.Demuxer, "-f", req.Source.Demuxer}
	if req.Source.Kind == core.AssetImage {
		args = append(args, "-loop", "1", "-framerate", fmt.Sprintf("%d/%d", fps.Numerator, fps.Denominator))
	}
	args = append(args, "-fd", "3", "-i", "fd:")
	args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("anullsrc=r=%d:cl=%s", a.SampleRateHz, a.ChannelLayout))
	args = append(args, "-map", "0:v:0", "-map", "1:a:0")
	// encoder содержит рабочее имя видеокодировщика для выбранного кодека.
	encoder := "h264_nvenc"
	switch v.Codec {
	case core.CodecAV1:
		if v.CodecProfile != "main" || v.Tier != "main" && v.Tier != "" || e.MaxBFrames != 0 {
			return Command{}, ErrInvalid
		}
		encoder = "libsvtav1"
		if p.Encoding.Version == 0 && (e.TargetBitrateBPS != e.MaxBitrateBPS || e.BufferBits*1000%e.TargetBitrateBPS != 0 || e.BufferBits*1000/e.TargetBitrateBPS < 20 || e.BufferBits*1000/e.TargetBitrateBPS > 10000) {
			return Command{}, fmt.Errorf("%w: AV1 поддерживает CBR с точным буфером 20–10000 ms", ErrInvalid)
		}
	case core.CodecH264:
		if v.Tier != "" || v.CodecProfile != "baseline" && v.CodecProfile != "main" && v.CodecProfile != "high" || v.CodecProfile == "baseline" && e.MaxBFrames != 0 {
			return Command{}, ErrInvalid
		}
	default:
		return Command{}, ErrInvalid
	}
	// plan содержит аргументы кодировщиков, построенные по проверенному профилю для подготовки.
	plan, err := encoding.Compile(p, encoding.Prepared)
	if err != nil {
		return Command{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if testEncoder != "" {
		// Явная тестовая подстановка не меняет рабочий выбор кодировщика.
		plan.Video[1] = testEncoder
		if encoder == "h264_nvenc" && testEncoder == "libx264" {
			// adapted накапливает видеоаргументы после удаления параметров NVENC для явной
			// тестовой замены на libx264.
			var adapted []string
			// i задаёт позицию имени очередной пары аргументов видеокодировщика.
			for i := 0; i < len(plan.Video); i += 2 {
				switch plan.Video[i] {
				case "-no-scenecut", "-forced-idr", "-rc", "-preset", "-tune", "-multipass", "-rc-lookahead", "-spatial-aq", "-temporal-aq", "-aq-strength", "-nonref_p", "-strict_gop":
					continue
				}
				adapted = append(adapted, plan.Video[i:i+2]...)
			}
			plan.Video = append(adapted, "-sc_threshold", "0")
		}
	}
	args = append(args, "-vf", fmt.Sprintf("fps=fps=%d/%d:round=near:eof_action=round,tpad=stop_mode=clone:stop=-1,scale=w=%d:h=%d,format=yuv420p,setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709", fps.Numerator, fps.Denominator, v.Width, v.Height), "-fps_mode:v", "passthrough", "-enc_time_base:v", "filter")
	args = append(args, plan.Video...)
	args = append(args, "-af", "apad=pad_dur="+duration)
	args = append(args, plan.Audio...)
	args = append(args, "-t", duration, "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", n(uint64(scale)), "-video_track_timescale", n(uint64(videoScale)), "-avoid_negative_ts", "disabled", "pipe:1")
	return Command{Binary: binary, Args: args}, nil
}

// Возвращает наибольший общий делитель для сокращения частот и согласования временных сеток.
func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
