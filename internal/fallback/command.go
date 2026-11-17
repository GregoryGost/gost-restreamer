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
	// HDR сохраняет проверенные исходные авторские сведения только на время подготовки поколения.
	HDR media.HDRMetadata
	// HDRVerified подтверждает проверку неизменности HDR на всей используемой области исходника.
	HDRVerified bool
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

// ProductionBuilder создаёт для binary построитель, который проверяет req и использует
// явно выбранный кодировщик без автоматической замены.
func ProductionBuilder(binary string) CommandBuilder {
	return func(req GenerateRequest) (Command, error) {
		// Строит команду подготовки с выбранным binary и рабочим кодировщиком для профиля запроса.
		return buildCommand(binary, req, "")
	}
}

// buildCommand собирает для binary проверенный req с входом FD 3 и отдельной собственной
// аудиотишиной. Неверный профиль, неподдержанный источник или неточная петля отклоняются
// до запуска процесса. testEncoder задаёт явную подстановку только для компонентных тестов.
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
	if v.Width%2 != 0 || v.Height%2 != 0 || v.Width > math.MaxInt32 || v.Height > math.MaxInt32 || !e.ClosedGOP || e.GOPFrames > math.MaxInt32 || e.MaxBFrames > math.MaxInt32 || e.BufferBits > math.MaxInt32 || e.TargetBitrateBPS > math.MaxInt64 || e.MaxBitrateBPS > math.MaxInt64 || p.AudioEncoding.BitrateBPS > math.MaxInt64 {
		return Command{}, ErrInvalid
	}
	if a.SampleRateHz == 0 || a.Codec == core.CodecAAC && (a.CodecProfile != "LC" || !core.ValidAACLayout(a.ChannelLayout, a.Channels)) {
		return Command{}, ErrInvalid
	}
	// conversion задаёт физическое преобразование по фактическому источнику либо узкую
	// политику рендеринга изображения/SDR с неполными CICP; source при этом не изменяется.
	// err сообщает о несовместимом источнике или невозможности построить требуемое преобразование.
	conversion, err := preparedConversion(req.Source, p)
	if err != nil {
		return Command{}, err
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
	// n преобразует числовой параметр v кодировщика FFmpeg в десятичную строку.
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
	// encoder сохраняет явный выбор для ограниченной тестовой подстановки NVENC.
	encoder := p.Encoding.VideoEncoder
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
	// filter добавляет FPS и продолжение последнего кадра перед выбранным преобразованием.
	filter := fmt.Sprintf("fps=fps=%d/%d:round=near:eof_action=round,tpad=stop_mode=clone:stop=-1,%s", fps.Numerator, fps.Denominator, conversion)
	// pixel задаёт физические плоскости кодировщика отдельно от кодекового плана.
	pixel := v.PixelFormat
	if encoder == core.VideoEncoderHEVCNVENC && v.BitDepth == 10 {
		pixel = "p010le"
		filter += ",format=p010le"
	}
	args = append(args, "-vf", filter, "-fps_mode:v", "passthrough", "-enc_time_base:v", "filter")
	args = append(args, plan.Video...)
	args = append(args, "-pix_fmt:v", pixel)
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

// preparedConversion строит фильтр для source и полного profile. Известное видео проходит
// общий конвертер; неподтверждённый цвет не подменяется desired. Узкий SDR-рендерер допускает
// PNG/JPEG и восьмибитное 4:2:0 видео с неполными CICP только для SVT/NVENC с AAC и BT.709.
func preparedConversion(source Source, profile core.GroupProfile) (string, error) {
	// target содержит желаемую геометрию и цвет результата, а не наблюдение источника.
	target := profile.Format.Video
	if source.Kind != core.AssetImage && source.Kind != core.AssetVideo {
		return "", ErrInvalid
	}
	if source.Kind == core.AssetVideo && source.Format.Video == nil {
		return "", fmt.Errorf("%w: отсутствует фактический видеоформат", ErrInvalid)
	}
	if isHDRVideo(target) && (source.Demuxer != "mov" || !source.HDRVerified) {
		return "", fmt.Errorf("%w: нужен проверенный статический HDR-источник MOV/MP4", ErrInvalid)
	}
	if source.Kind == core.AssetVideo {
		// actual содержит исходные CICP; полное описание проверяет общий конвертер без запасного пути.
		actual := *source.Format.Video
		if actual.Color.Range != "" && actual.Color.Range != "unspecified" && actual.Color.Primaries != "" && actual.Color.Primaries != "unspecified" && actual.Color.Transfer != "" && actual.Color.Transfer != "unspecified" && actual.Color.Matrix != "" && actual.Color.Matrix != "unspecified" {
			// filter содержит проверенный граф; err объясняет несовместимые исходные или целевые параметры.
			filter, err := encoding.SoftwareVideoFilter(actual, target, true)
			if err != nil {
				return "", fmt.Errorf("%w: %w", ErrInvalid, err)
			}
			return filter, nil
		}
		if actual.BitDepth != 8 || actual.PixelFormat != "yuv420p" || isHDRVideo(actual) {
			return "", fmt.Errorf("%w: неполный цвет разрешён только для SDR 8-bit 4:2:0", ErrInvalid)
		}
	}
	if target.BitDepth != 8 || target.PixelFormat != "yuv420p" || target.Color.Primaries != "bt709" || target.Color.Transfer != "bt709" || target.Color.Matrix != "bt709" || profile.Format.Audio.Codec != core.CodecAAC || profile.Encoding.AudioEncoder != core.AudioEncoderAAC || profile.Encoding.VideoEncoder != core.VideoEncoderNVENC && profile.Encoding.VideoEncoder != core.VideoEncoderSVTAV1 {
		return "", fmt.Errorf("%w: источник без полных CICP требует ограниченный BT.709 SDR-рендерер", ErrInvalid)
	}
	if source.Kind == core.AssetImage && (source.Demuxer != "png_pipe" && source.Demuxer != "jpeg_pipe") {
		return "", ErrInvalid
	}
	// rangeName задаёт диапазон собственного результата; отсутствующие source CICP остаются неизвестными.
	rangeName := "limited"
	if target.Color.Range == "full" || target.Color.Range == "pc" {
		rangeName = "full"
	}
	return fmt.Sprintf("scale=w=%d:h=%d:out_range=%s,format=yuv420p,setparams=range=%s:color_primaries=bt709:color_trc=bt709:colorspace=bt709", target.Width, target.Height, rangeName, rangeName), nil
}
