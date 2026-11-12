package ffmpeg_test

import (
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

// Создаёт вход AV1 1440p60 и выход H.264 1080p30 с явным преобразованием AAC 48 kHz в 44,1 kHz.
func conversionCommandConfig() ffmpeg.NVIDIAConfig {
	// color задаёт общее описание SDR bt709 для проверки преобразования размеров и частоты.
	color := core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}
	// input содержит контрольный AV1-вход 1440p60 со стерео AAC 48000 Гц.
	input := core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecAV1, Width: 2560, Height: 1440, FrameRate: core.FrameRate{Numerator: 60, Denominator: 1}, CodecProfile: "main", PixelFormat: "yuv420p", BitDepth: 8, Color: color}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}
	// output содержит целевой H.264 1080p30 со стерео AAC 44100 Гц.
	output := input
	output.Video.Codec, output.Video.CodecProfile = core.CodecH264, "high"
	output.Video.Width, output.Video.Height = 1920, 1080
	output.Video.FrameRate = core.FrameRate{Numerator: 30, Denominator: 1}
	output.Audio.SampleRateHz = 44100
	return ffmpeg.NVIDIAConfig{Binary: "/usr/bin/ffmpeg", Input: input, VideoTimeBase: media.TimeBase{Numerator: 1, Denominator: 60000}, AudioTimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Profile: core.GroupProfile{Mode: core.ModeTranscode, LiveAudioMode: core.LiveAudioTranscode, Format: output, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 6000000, MaxBitrateBPS: 8000000, BufferBits: 12000000, GOPFrames: 60, MaxBFrames: 2, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 192000}}}
}

// Возвращает единственное значение заданного флага команды либо завершает тест при отсутствии, повторе
// или неполной паре.
func conversionArgument(t *testing.T, args []string, flag string) string {
	t.Helper()
	// i обозначает позицию запрошенного флага для проверки единственного полного аргумента.
	i := slices.Index(args, flag)
	if i < 0 || i+1 == len(args) || slices.Contains(args[i+1:], flag) {
		t.Fatalf("аргумент %s должен встречаться ровно один раз: %v", flag, args)
	}
	return args[i+1]
}

// Проверяет точные параметры преобразования на одной временной сетке, сохранение полного AAC и
// независимость команд разных профилей без изменения входных данных.
func TestAcceptanceNVIDIAConvertsProfileOnOneExplicitGrid(t *testing.T) {
	// c содержит конфигурацию преобразования AV1/AAC в целевой H.264/AAC.
	c := conversionCommandConfig()
	// before сохраняет исходную конфигурацию для проверки отсутствия мутаций при построении.
	before := c
	// a задаёт попытку с буквальными путями UDS, содержащими символы оболочки.
	a := ffmpeg.Attempt{ID: 1, InputPath: "/private/tmp/input ;$(literal)", OutputPath: "/private/tmp/output ;$(literal)"}
	// cmd содержит построенную команду преобразования; err сохраняет результат построения исходной и
	// второй команды.
	cmd, err := ffmpeg.BuildNVIDIACommand(c, a)
	if err != nil {
		t.Fatal(err)
	}
	// flag задаёт проверяемый параметр; want содержит его точное ожидаемое значение.
	for flag, want := range map[string]string{"-vf": "fps=fps=30/1:round=near:eof_action=round,scale_cuda=w=1920:h=1080:format=yuv420p", "-fps_mode:v": "passthrough", "-enc_time_base:v": "filter", "-video_track_timescale": "30", "-movie_timescale": "35280000", "-c:a": "aac", "-ar:a": "44100", "-ac:a": "2", "-b:a": "192000", "-b:v": "6000000", "-maxrate:v": "8000000", "-bufsize:v": "12000000", "-g:v": "60", "-bf:v": "2", "-avoid_negative_ts": "disabled", "-i": "async:unix:" + a.InputPath} {
		// got содержит единственное фактическое значение проверяемого флага.
		if got := conversionArgument(t, cmd.Args, flag); got != want {
			t.Errorf("%s=%q, требуется %q", flag, got, want)
		}
	}
	// flag содержит очередной запрещённый второй регулятор временной шкалы.
	for _, flag := range []string{"-r", "-r:v", "-shortest", "-start_at_zero"} {
		if slices.Contains(cmd.Args, flag) {
			t.Errorf("найден второй регулятор временной шкалы: %s", flag)
		}
	}
	// got содержит значение -flags2:a для проверки сохранения полных AAC AU перед входом.
	if got := conversionArgument(t, cmd.Args, "-flags2:a"); got != "+skip_manual" || slices.Index(cmd.Args, "-flags2:a") >= slices.Index(cmd.Args, "-i") {
		t.Fatalf("полные AAC AU должны сохраняться аудиодекодером перед входом: %v", cmd.Args)
	}
	// arg содержит очередной аргумент для проверки области действия -flags2:a.
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "-flags2") && arg != "-flags2:a" {
			t.Fatalf("настройка сохранения AAC AU вышла за область аудиодекодера: %v", cmd.Args)
		}
	}
	// joined объединяет argv для поиска запрещённых преобразований временных меток и программных
	// замен.
	joined := strings.Join(cmd.Args, " ")
	// forbidden содержит очередную подстроку неподдержанного преобразования команды.
	for _, forbidden := range []string{"start_time=", "setpts", "asetpts", "aresample=async", "libx264", "scale="} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("рабочая команда содержит %q", forbidden)
		}
	}
	if !reflect.DeepEqual(c, before) || cmd.Args[len(cmd.Args)-1] != "unix:"+a.OutputPath {
		t.Fatal("построение изменило профиль либо границы пути")
	}
	// other содержит второй независимый профиль с иными размерами, FPS, битрейтом и GOP.
	other := c
	other.Profile.Format.Video.Width, other.Profile.Format.Video.Height = 2560, 1440
	other.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 60, Denominator: 1}
	other.Profile.VideoEncoding.TargetBitrateBPS = 7000000
	other.Profile.VideoEncoding.GOPFrames = 120
	// second содержит команду второго профиля для проверки независимости аргументов первой команды.
	second, err := ffmpeg.BuildNVIDIACommand(other, a)
	if err != nil {
		t.Fatal(err)
	}
	if conversionArgument(t, second.Args, "-vf") != "fps=fps=60/1:round=near:eof_action=round,scale_cuda=w=2560:h=1440:format=yuv420p" || conversionArgument(t, second.Args, "-b:v") != "7000000" || conversionArgument(t, second.Args, "-g:v") != "120" || conversionArgument(t, cmd.Args, "-b:v") != "6000000" {
		t.Fatal("независимые профили смешали параметры")
	}
}

// Проверяет поддержанные рациональные частоты, сокращение только в команде и отдельные отказы
// диапазона, общей шкалы, точности FPS, цвета и режима copy.
func TestAcceptanceNVIDIARationalRatesAndPrecisionRefusal(t *testing.T) {
	// tc содержит очередной сценарий точной рациональной частоты кадров.
	for _, tc := range []struct {
		// name задаёт название сценария поддержанной частоты.
		name string
		// rate задаёт исходную, возможно несокращённую частоту профиля.
		rate core.FrameRate
		// want содержит ожидаемую сокращённую дробь фильтра FPS.
		want string
	}{
		{"integer", core.FrameRate{Numerator: 30, Denominator: 1}, "30/1"},
		{"ntsc", core.FrameRate{Numerator: 30000, Denominator: 1001}, "30000/1001"},
		{"equivalent", core.FrameRate{Numerator: 120000, Denominator: 4004}, "30000/1001"},
		{"ntsc60", core.FrameRate{Numerator: 60000, Denominator: 1001}, "60000/1001"},
		{"small", core.FrameRate{Numerator: 1, Denominator: math.MaxInt32}, "1/2147483647"},
		{"large_equivalent", core.FrameRate{Numerator: 4000000000, Denominator: 2000000000}, "2/1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет сокращённую дробь единственного фильтра FPS без изменения исходного профиля.

			// c содержит отдельный профиль с частотой выбранного сценария.
			c := conversionCommandConfig()
			c.Profile.Format.Video.FrameRate = tc.rate
			// cmd содержит команду выбранной рациональной частоты; err сообщает об отказе её
			// построения.
			cmd, err := ffmpeg.BuildNVIDIACommand(c, ffmpeg.Attempt{InputPath: "/in", OutputPath: "/out"})
			if err != nil {
				t.Fatal(err)
			}
			// filter содержит единственную цепочку -vf для проверки сокращённого FPS и
			// сохранности исходной дроби.
			if filter := conversionArgument(t, cmd.Args, "-vf"); !strings.HasPrefix(filter, "fps=fps="+tc.want+":round=near:eof_action=round,") || c.Profile.Format.Video.FrameRate != tc.rate {
				t.Fatalf("дробь изменена: %s, %+v", filter, c.Profile.Format.Video.FrameRate)
			}
		})
	}
	// high содержит профиль с точно представимой частотой 100000000/1 и AAC 48000 Гц.
	high := conversionCommandConfig()
	high.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 100000000, Denominator: 1}
	high.Profile.Format.Audio.SampleRateHz = 48000
	// highCommand содержит команду контрольной высокой частоты; err сообщает об отказе её
	// построения.
	highCommand, err := ffmpeg.BuildNVIDIACommand(high, ffmpeg.Attempt{InputPath: "/in", OutputPath: "/out"})
	if err != nil || conversionArgument(t, highCommand.Args, "-movie_timescale") != "300000000" {
		t.Fatal("точно представимая частота с тем же числителем отклонена", err)
	}
	// tc содержит название и способ внесения неподдержанного параметра профиля.
	for _, tc := range []struct {
		// name задаёт название сценария отказа точности, временной шкалы или цвета.
		name string
		// change вносит конкретное нарушение профиля перед проверкой отказа команды.
		change func(*ffmpeg.NVIDIAConfig)
	}{
		{"missing_audio_base", func(c *ffmpeg.NVIDIAConfig) {
			// Обнуляет AudioTimeBase для проверки отказа неполной конфигурации.
			c.AudioTimeBase = media.TimeBase{}
		}},
		{"movie_scale_overflow", func(c *ffmpeg.NVIDIAConfig) {
			// Задаёт знаменатель AudioTimeBase 2147483647, вызывающий непредставимую общую шкалу фильма.
			c.AudioTimeBase = media.TimeBase{Numerator: 1, Denominator: 2147483647}
		}},
		{"rate_outside_int32", func(c *ffmpeg.NVIDIAConfig) {
			// Задаёт числитель FPS 2147483648 за пределом диапазона FFmpeg.

			c.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 2147483648, Denominator: 1}
		}},
		// Шкала времени этой дроби допустима: отказ должен объясняться точностью FPS.
		{"ambiguous_double", func(c *ffmpeg.NVIDIAConfig) {
			// Задаёт FPS 100000000/99999999 и 48000 Гц AAC для проверки отказа неоднозначного преобразования
			// binary64 при допустимой шкале.

			c.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 100000000, Denominator: 99999999}
			c.Profile.Format.Audio.SampleRateHz = 48000
		}},
		{"unsupported_color", func(c *ffmpeg.NVIDIAConfig) {
			// Устанавливает передаточную характеристику HDR smpte2084, неподдержанную портом SDR.
			c.Profile.Format.Video.Color.Transfer = "smpte2084"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Вносит выбранное нарушение точности или формата и требует отказа построения команды.

			// c содержит отдельную конфигурацию с выбранным нарушением точности или формата.
			c := conversionCommandConfig()
			tc.change(&c)
			// err содержит ожидаемый отказ построения непредставимого профиля.
			if _, err := ffmpeg.BuildNVIDIACommand(c, ffmpeg.Attempt{InputPath: "/in", OutputPath: "/out"}); err == nil {
				t.Fatal("непредставимый профиль принят")
			}
		})
	}
	// c содержит контрольный профиль, переведённый в запрещённый для аппаратного кодирования режим
	// copy.
	c := conversionCommandConfig()
	c.Profile.Mode = core.ModeCopy
	// err содержит ожидаемый ErrInvalidSupervisor при попытке построить кодирование в режиме copy.
	if _, err := ffmpeg.BuildNVIDIACommand(c, ffmpeg.Attempt{InputPath: "/in", OutputPath: "/out"}); !errors.Is(err, ffmpeg.ErrInvalidSupervisor) {
		t.Fatal(err)
	}
}
