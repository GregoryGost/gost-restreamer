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

func conversionCommandConfig() ffmpeg.NVIDIAConfig {
	color := core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}
	input := core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecAV1, Width: 2560, Height: 1440, FrameRate: core.FrameRate{Numerator: 60, Denominator: 1}, CodecProfile: "main", PixelFormat: "yuv420p", BitDepth: 8, Color: color}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}
	output := input
	output.Video.Codec, output.Video.CodecProfile = core.CodecH264, "high"
	output.Video.Width, output.Video.Height = 1920, 1080
	output.Video.FrameRate = core.FrameRate{Numerator: 30, Denominator: 1}
	output.Audio.SampleRateHz = 44100
	return ffmpeg.NVIDIAConfig{Binary: "/usr/bin/ffmpeg", Input: input, VideoTimeBase: media.TimeBase{Numerator: 1, Denominator: 60000}, AudioTimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Profile: core.GroupProfile{Mode: core.ModeTranscode, LiveAudioMode: core.LiveAudioTranscode, Format: output, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 6000000, MaxBitrateBPS: 8000000, BufferBits: 12000000, GOPFrames: 60, MaxBFrames: 2, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 192000}}}
}

func conversionArgument(t *testing.T, args []string, flag string) string {
	t.Helper()
	i := slices.Index(args, flag)
	if i < 0 || i+1 == len(args) || slices.Contains(args[i+1:], flag) {
		t.Fatalf("аргумент %s должен встречаться ровно один раз: %v", flag, args)
	}
	return args[i+1]
}

func TestAcceptanceNVIDIAConvertsProfileOnOneExplicitGrid(t *testing.T) {
	c := conversionCommandConfig()
	before := c
	a := ffmpeg.Attempt{ID: 1, InputPath: "/private/tmp/input ;$(literal)", OutputPath: "/private/tmp/output ;$(literal)"}
	cmd, err := ffmpeg.BuildNVIDIACommand(c, a)
	if err != nil {
		t.Fatal(err)
	}
	for flag, want := range map[string]string{"-vf": "fps=fps=30/1:round=near:eof_action=round,scale_cuda=w=1920:h=1080:format=yuv420p", "-fps_mode:v": "passthrough", "-enc_time_base:v": "filter", "-video_track_timescale": "30", "-movie_timescale": "35280000", "-c:a": "aac", "-ar:a": "44100", "-ac:a": "2", "-b:a": "192000", "-b:v": "6000000", "-maxrate:v": "8000000", "-bufsize:v": "12000000", "-g:v": "60", "-bf:v": "2", "-avoid_negative_ts": "disabled", "-i": "async:unix:" + a.InputPath} {
		if got := conversionArgument(t, cmd.Args, flag); got != want {
			t.Errorf("%s=%q, требуется %q", flag, got, want)
		}
	}
	for _, flag := range []string{"-r", "-r:v", "-shortest", "-start_at_zero"} {
		if slices.Contains(cmd.Args, flag) {
			t.Errorf("найден второй регулятор временной шкалы: %s", flag)
		}
	}
	if got := conversionArgument(t, cmd.Args, "-flags2:a"); got != "+skip_manual" || slices.Index(cmd.Args, "-flags2:a") >= slices.Index(cmd.Args, "-i") {
		t.Fatalf("полные AAC AU должны сохраняться аудиодекодером перед входом: %v", cmd.Args)
	}
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "-flags2") && arg != "-flags2:a" {
			t.Fatalf("настройка сохранения AAC AU вышла за область аудиодекодера: %v", cmd.Args)
		}
	}
	joined := strings.Join(cmd.Args, " ")
	for _, forbidden := range []string{"start_time=", "setpts", "asetpts", "aresample=async", "libx264", "scale="} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("рабочая команда содержит %q", forbidden)
		}
	}
	if !reflect.DeepEqual(c, before) || cmd.Args[len(cmd.Args)-1] != "unix:"+a.OutputPath {
		t.Fatal("построение изменило профиль либо границы пути")
	}
	other := c
	other.Profile.Format.Video.Width, other.Profile.Format.Video.Height = 2560, 1440
	other.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 60, Denominator: 1}
	other.Profile.VideoEncoding.TargetBitrateBPS = 7000000
	other.Profile.VideoEncoding.GOPFrames = 120
	second, err := ffmpeg.BuildNVIDIACommand(other, a)
	if err != nil {
		t.Fatal(err)
	}
	if conversionArgument(t, second.Args, "-vf") != "fps=fps=60/1:round=near:eof_action=round,scale_cuda=w=2560:h=1440:format=yuv420p" || conversionArgument(t, second.Args, "-b:v") != "7000000" || conversionArgument(t, second.Args, "-g:v") != "120" || conversionArgument(t, cmd.Args, "-b:v") != "6000000" {
		t.Fatal("независимые профили смешали параметры")
	}
}

func TestAcceptanceNVIDIARationalRatesAndPrecisionRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		rate core.FrameRate
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
			c := conversionCommandConfig()
			c.Profile.Format.Video.FrameRate = tc.rate
			cmd, err := ffmpeg.BuildNVIDIACommand(c, ffmpeg.Attempt{InputPath: "/in", OutputPath: "/out"})
			if err != nil {
				t.Fatal(err)
			}
			if filter := conversionArgument(t, cmd.Args, "-vf"); !strings.HasPrefix(filter, "fps=fps="+tc.want+":round=near:eof_action=round,") || c.Profile.Format.Video.FrameRate != tc.rate {
				t.Fatalf("дробь изменена: %s, %+v", filter, c.Profile.Format.Video.FrameRate)
			}
		})
	}
	high := conversionCommandConfig()
	high.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 100000000, Denominator: 1}
	high.Profile.Format.Audio.SampleRateHz = 48000
	highCommand, err := ffmpeg.BuildNVIDIACommand(high, ffmpeg.Attempt{InputPath: "/in", OutputPath: "/out"})
	if err != nil || conversionArgument(t, highCommand.Args, "-movie_timescale") != "300000000" {
		t.Fatal("точно представимая частота с тем же числителем отклонена", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*ffmpeg.NVIDIAConfig)
	}{
		{"missing_audio_base", func(c *ffmpeg.NVIDIAConfig) { c.AudioTimeBase = media.TimeBase{} }},
		{"movie_scale_overflow", func(c *ffmpeg.NVIDIAConfig) { c.AudioTimeBase = media.TimeBase{Numerator: 1, Denominator: 2147483647} }},
		{"rate_outside_int32", func(c *ffmpeg.NVIDIAConfig) {
			c.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 2147483648, Denominator: 1}
		}},
		// Шкала времени этой дроби допустима: отказ должен объясняться точностью FPS.
		{"ambiguous_double", func(c *ffmpeg.NVIDIAConfig) {
			c.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 100000000, Denominator: 99999999}
			c.Profile.Format.Audio.SampleRateHz = 48000
		}},
		{"unsupported_color", func(c *ffmpeg.NVIDIAConfig) { c.Profile.Format.Video.Color.Transfer = "smpte2084" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := conversionCommandConfig()
			tc.change(&c)
			if _, err := ffmpeg.BuildNVIDIACommand(c, ffmpeg.Attempt{InputPath: "/in", OutputPath: "/out"}); err == nil {
				t.Fatal("непредставимый профиль принят")
			}
		})
	}
	c := conversionCommandConfig()
	c.Profile.Mode = core.ModeCopy
	if _, err := ffmpeg.BuildNVIDIACommand(c, ffmpeg.Attempt{InputPath: "/in", OutputPath: "/out"}); !errors.Is(err, ffmpeg.ErrInvalidSupervisor) {
		t.Fatal(err)
	}
}
