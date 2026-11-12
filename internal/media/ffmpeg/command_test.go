package ffmpeg

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// Создаёт контрольный вход AV1 и профиль H.264/AAC с дробной частотой, явным преобразованием аудио и
// фактическими шкалами дорожек.
func nvidiaCommandConfig() NVIDIAConfig {
	// color задаёт общее описание SDR bt709 контрольного входа и выхода.
	color := core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}
	// format содержит целевой H.264/AAC-формат с частотой 30000/1001.
	format := core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecH264, Width: 1920, Height: 1080, FrameRate: core.FrameRate{Numerator: 30000, Denominator: 1001}, CodecProfile: "high", Level: "4.1", PixelFormat: "yuv420p", BitDepth: 8, Color: color}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 44100, Channels: 2, ChannelLayout: "stereo"}}
	// input содержит контрольный AV1-вход с остальными параметрами целевого формата.
	input := format
	input.Video.Codec = core.CodecAV1
	input.Video.CodecProfile = "main"
	return NVIDIAConfig{Binary: "/usr/bin/ffmpeg", Input: input, Profile: core.GroupProfile{Mode: core.ModeTranscode, LiveAudioMode: core.LiveAudioTranscode, Format: format, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 6000000, MaxBitrateBPS: 7000000, BufferBits: 12000000, GOPFrames: 60, MaxBFrames: 2, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 192000}}, VideoTimeBase: media.TimeBase{Numerator: 1, Denominator: 30000}, AudioTimeBase: media.TimeBase{Numerator: 1, Denominator: 44100}}
}

// Проверяет точные параметры NVDEC/NVENC, аудио, цвета и временных шкал и размещение skip_manual до
// входа без второго регулятора FPS или программной замены кодировщика.
func TestNVIDIACommandPreservesExplicitEncodingAndTiming(t *testing.T) {
	// c содержит контрольную конфигурацию для проверки полного набора аргументов кодирования.
	c := nvidiaCommandConfig()
	// cmd содержит построенную команду; err сообщает об отказе построения или последующей проверки
	// режима copy.
	cmd, err := BuildNVIDIACommand(c, Attempt{ID: 1, InputPath: "/private/tmp/in socket", OutputPath: "/private/tmp/out socket"})
	if err != nil {
		t.Fatal(err)
	}
	// values связывает флаги команды с их следующими аргументами для проверки точных значений.
	values := map[string]string{}
	// i обозначает позицию аргумента при сборе пар флагов и значений.
	for i := 0; i+1 < len(cmd.Args); i++ {
		if strings.HasPrefix(cmd.Args[i], "-") {
			values[cmd.Args[i]] = cmd.Args[i+1]
		}
	}
	// key задаёт проверяемый флаг; want задаёт его ожидаемое строковое значение.
	for key, want := range map[string]string{"-hwaccel": "cuda", "-hwaccel_output_format": "cuda", "-c:v": "h264_nvenc", "-b:v": "6000000", "-maxrate:v": "7000000", "-bufsize:v": "12000000", "-g:v": "60", "-bf:v": "2", "-vf": "fps=fps=30000/1001:round=near:eof_action=round,scale_cuda=w=1920:h=1080:format=yuv420p", "-fps_mode:v": "passthrough", "-enc_time_base:v": "filter", "-profile:v": "high", "-level:v": "4.1", "-color_range:v": "tv", "-color_primaries:v": "bt709", "-color_trc:v": "bt709", "-colorspace:v": "bt709", "-ar:a": "44100", "-ac:a": "2", "-channel_layout:a": "stereo", "-b:a": "192000", "-profile:a": "aac_low", "-min_frag_duration": "1", "-use_editlist": "1", "-movie_timescale": "4410000", "-video_track_timescale": "30000", "-avoid_negative_ts": "disabled"} {
		if values[key] != want {
			t.Errorf("%s=%q, ожидалось %q", key, values[key], want)
		}
	}
	// input обозначает позицию -i для проверки области действия входных параметров.
	input := slices.Index(cmd.Args, "-i")
	// flags2 обозначает позицию -flags2:a, сохраняющего полные AAC AU перед входом.
	flags2 := slices.Index(cmd.Args, "-flags2:a")
	// flagCount считает вхождения -flags2:a для проверки единственности параметра.
	flagCount := 0
	// arg содержит очередной аргумент при подсчёте -flags2:a.
	for _, arg := range cmd.Args {
		if arg == "-flags2:a" {
			flagCount++
		}
	}
	if flagCount != 1 || flags2 < 0 || flags2+1 >= len(cmd.Args) || cmd.Args[flags2+1] != "+skip_manual" || input < 0 || flags2 >= input {
		t.Fatalf("-flags2:a +skip_manual должен один раз настраивать аудиодекодер перед входом: %v", cmd.Args)
	}
	// ok показывает наличие запрещённого дополнительного регулятора частоты -r:v.
	if _, ok := values["-r:v"]; ok {
		t.Fatal("частота уже задана фильтром fps")
	}
	if cmd.Args[len(cmd.Args)-1] != "unix:/private/tmp/out socket" {
		t.Fatal(cmd.Args)
	}
	// joined объединяет аргументы для поиска неподдержанных флагов контейнера и программного
	// кодировщика.
	joined := strings.Join(cmd.Args, " ")
	if strings.Contains(joined, "empty_moov") || strings.Contains(joined, "negative_cts_offsets") || strings.Contains(joined, "libx264") {
		t.Fatal(joined)
	}
	c.Profile.Mode = core.ModeCopy
	if _, err = BuildNVIDIACommand(c, Attempt{InputPath: "/a", OutputPath: "/b"}); !errors.Is(err, ErrInvalidSupervisor) {
		t.Fatal(err)
	}
}

// Проверяет отказ для несогласованного цвета, пустых временных баз, неподдержанной битности, Tier,
// открытого GOP и раскладки аудио.
func TestNVIDIACommandRejectsUnsupportedContractFields(t *testing.T) {
	// test содержит название сценария и изменение, нарушающее один из контрактов команды.
	for _, test := range []struct {
		// name задаёт название подтеста для конкретного неподдержанного параметра.
		name string
		// change вносит проверяемое нарушение в контрольную конфигурацию перед построением
		// команды.
		change func(*NVIDIAConfig)
	}{
		{"цвет", func(c *NVIDIAConfig) {
			// Меняет выходную цветовую матрицу на bt470bg, создавая расхождение с входом.
			c.Profile.Format.Video.Color.Matrix = "bt470bg"
		}},
		{"шкала видео", func(c *NVIDIAConfig) {
			// Обнуляет шкалу времени видео для проверки отказа до запуска.
			c.VideoTimeBase = media.TimeBase{}
		}},
		{"шкала аудио", func(c *NVIDIAConfig) {
			// Обнуляет шкалу времени аудио для проверки отказа до запуска.
			c.AudioTimeBase = media.TimeBase{}
		}},
		{"битность", func(c *NVIDIAConfig) {
			// Устанавливает выходную глубину 10 бит, неподдержанную данным портом.
			c.Profile.Format.Video.BitDepth = 10
		}},
		{"Tier", func(c *NVIDIAConfig) {
			// Задаёт Tier=main для H.264, у которого этот параметр не поддерживается.
			c.Profile.Format.Video.Tier = "main"
		}},
		{"открытый GOP", func(c *NVIDIAConfig) {
			// Открывает GOP для проверки обязательного ClosedGOP.
			c.Profile.VideoEncoding.ClosedGOP = false
		}},
		{"channel layout", func(c *NVIDIAConfig) {
			// Меняет расположение каналов на 5.1 при прежнем числе каналов.
			c.Profile.Format.Audio.ChannelLayout = "5.1"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Применяет выбранное нарушение конфигурации и требует ErrInvalidSupervisor при построении команды.

			// c содержит отдельную конфигурацию с выбранным неподдержанным параметром.
			c := nvidiaCommandConfig()
			test.change(&c)
			// err содержит ожидаемый отказ построения команды после нарушения конфигурации.
			if _, err := BuildNVIDIACommand(c, Attempt{InputPath: "/a", OutputPath: "/b"}); !errors.Is(err, ErrInvalidSupervisor) {
				t.Fatal(err)
			}
		})
	}
}

// Проверяет преобразование размеров, частоты и аудио с общей точной шкалой без изменения исходного
// профиля и его несокращённой частоты.
func TestNVIDIACommandConvertsProfileWithoutChangingItsIdentity(t *testing.T) {
	// c задаёт различающиеся входные и выходные размеры, частоты и временные шкалы.
	c := nvidiaCommandConfig()
	c.Input.Video.Width, c.Input.Video.Height = 2560, 1440
	c.Input.Video.FrameRate = core.FrameRate{Numerator: 60, Denominator: 1}
	c.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 60000, Denominator: 2002}
	c.Profile.Format.Audio.SampleRateHz = 48000
	c.VideoTimeBase = media.TimeBase{Numerator: 2, Denominator: 90000}
	c.AudioTimeBase = media.TimeBase{Numerator: 2, Denominator: 96000}
	// before сохраняет конфигурацию до построения команды для проверки отсутствия изменений.
	before := c
	// cmd содержит команду преобразования профиля; err сообщает об отказе её построения.
	cmd, err := BuildNVIDIACommand(c, Attempt{InputPath: "/a", OutputPath: "/b"})
	if err != nil {
		t.Fatal(err)
	}
	if c != before {
		t.Fatal("построение команды изменило входной профиль")
	}
	// key задаёт проверяемый параметр преобразования; want содержит его точное ожидаемое значение.
	for key, want := range map[string]string{
		"-vf":                    "fps=fps=30000/1001:round=near:eof_action=round,scale_cuda=w=1920:h=1080:format=yuv420p",
		"-movie_timescale":       "35280000",
		"-video_track_timescale": "30000",
		"-ar:a":                  "48000",
	} {
		// i обозначает позицию проверяемого параметра преобразования в argv.
		i := slices.Index(cmd.Args, key)
		if i < 0 || i+1 >= len(cmd.Args) || cmd.Args[i+1] != want {
			t.Errorf("%s: ожидалось %q, argv=%v", key, want, cmd.Args)
		}
	}
}

// Проверяет, что непредставимая общая шкала возвращает ошибку и не создаёт частичную команду.
func TestNVIDIACommandRejectsUnrepresentableMovieScale(t *testing.T) {
	// base содержит аудиошкалу, вызывающую непредставимую общую шкалу MP4.
	for _, base := range []media.TimeBase{
		{Numerator: 1, Denominator: math.MaxInt32},
		{Numerator: 1, Denominator: math.MaxUint32},
	} {
		// c содержит конфигурацию с очередной непредставимой аудиошкалой.
		c := nvidiaCommandConfig()
		c.AudioTimeBase = base
		// cmd содержит результат отклонённого построения; err содержит ожидаемый отказ общей
		// шкалы.
		if cmd, err := BuildNVIDIACommand(c, Attempt{InputPath: "/a", OutputPath: "/b"}); err == nil || cmd.Binary != "" || len(cmd.Args) != 0 {
			t.Errorf("непредставимая шкала %v создала команду: %v, %v", base, cmd, err)
		}
	}
}

// Проверяет явные CBR/VBR и управление scale_cuda; выключенное масштабирование требует совпадения
// входных и выходных размеров.
func TestNVIDIACommandExplicitRescaleAndRateControl(t *testing.T) {
	// rc задаёт проверяемый режим управления битрейтом cbr или vbr.
	for _, rc := range []string{"cbr", "vbr"} {
		// c содержит конфигурацию выбранного режима битрейта и переключаемого масштабирования.
		c := nvidiaCommandConfig()
		c.Profile.Encoding = core.DefaultEncoding(core.ModeTranscode, "h264_nvenc", rc)
		if rc == "cbr" {
			c.Profile.VideoEncoding.MaxBitrateBPS = c.Profile.VideoEncoding.TargetBitrateBPS
		}
		c.Profile.Encoding.RescaleOutput = false
		// cmd содержит команду для выбранного режима; err сохраняет результат построения до и
		// после изменения размеров.
		cmd, err := BuildNVIDIACommand(c, Attempt{InputPath: "/a", OutputPath: "/b"})
		if err != nil {
			t.Fatal(err)
		}
		// filter содержит цепочку видеофильтров для проверки отсутствия scale_cuda при
		// выключенном масштабировании.
		filter := cmd.Args[slices.Index(cmd.Args, "-vf")+1]
		if strings.Contains(filter, "scale_cuda") || cmd.Args[slices.Index(cmd.Args, "-rc")+1] != rc {
			t.Fatal(cmd.Args)
		}
		c.Input.Video.Width += 2
		if _, err = BuildNVIDIACommand(c, Attempt{InputPath: "/a", OutputPath: "/b"}); !errors.Is(err, ErrInvalidSupervisor) {
			t.Fatal("размеры без масштабирования приняты", err)
		}
		c.Profile.Encoding.RescaleOutput = true
		cmd, err = BuildNVIDIACommand(c, Attempt{InputPath: "/a", OutputPath: "/b"})
		if err != nil || !strings.Contains(strings.Join(cmd.Args, " "), "scale_cuda=w=1920:h=1080") {
			t.Fatal(cmd, err)
		}
	}
}

// Проверяет чистое копирование LIVE AAC без параметров кодировщика и отказ несовместимого входа,
// который допускается при явном transcode.
func TestNVIDIACommandCopyAudioRejectsMismatchWithoutEncoding(t *testing.T) {
	// c содержит конфигурацию LIVE AAC copy с намеренно заданными лишними настройками
	// аудиокодировщика.
	c := nvidiaCommandConfig()
	c.Profile.LiveAudioMode = core.LiveAudioCopy
	c.Profile.Encoding = core.DefaultEncoding(c.Profile.Mode, "h264_nvenc", "vbr")
	c.Profile.Encoding.AudioCustom = "aac_coder=fast"
	// command содержит построенную команду LIVE AAC copy; err сохраняет результаты построения при
	// проверке совместимости аудио.
	command, err := BuildNVIDIACommand(c, Attempt{InputPath: "/a", OutputPath: "/b"})
	if err != nil {
		t.Fatal(err)
	}
	// i обозначает позицию -c:a для проверки режима copy.
	if i := slices.Index(command.Args, "-c:a"); i < 0 || command.Args[i+1] != "copy" {
		t.Fatal(command.Args)
	}
	// forbidden содержит очередной параметр аудиокодирования, который отсутствует в режиме LIVE AAC
	// copy.
	for _, forbidden := range []string{"-af", "-ar:a", "-ac:a", "-channel_layout:a", "-b:a", "-profile:a", "-aac_coder"} {
		if slices.Contains(command.Args, forbidden) {
			t.Fatalf("LIVE copy содержит %s: %v", forbidden, command.Args)
		}
	}
	// change изменяет частоту либо раскладку каналов контрольного входного AAC.
	for _, change := range []func(*core.AudioFormat){
		func(a *core.AudioFormat) {
			// Меняет частоту входного AAC на 48000 Гц, создавая несовместимость с режимом copy.
			a.SampleRateHz = 48000
		},
		func(a *core.AudioFormat) {
			// Меняет входной AAC на mono с одним каналом для проверки несовместимости copy.
			a.Channels, a.ChannelLayout = 1, "mono"
		},
	} {
		// changed содержит отдельную конфигурацию с несовместимым AAC для проверки copy и явного
		// transcode.
		changed := c
		change(&changed.Input.Audio)
		if _, err = BuildNVIDIACommand(changed, Attempt{InputPath: "/a", OutputPath: "/b"}); !errors.Is(err, ErrInvalidSupervisor) || !strings.Contains(err.Error(), "LIVE AAC copy") {
			t.Fatal("несовместимый copy принят", err)
		}
		changed.Profile.LiveAudioMode = core.LiveAudioTranscode
		if _, err = BuildNVIDIACommand(changed, Attempt{InputPath: "/a", OutputPath: "/b"}); err != nil {
			t.Fatal("явное преобразование аудио отклонено", err)
		}
	}
}
