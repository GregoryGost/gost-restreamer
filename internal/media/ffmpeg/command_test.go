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

func nvidiaCommandConfig() NVIDIAConfig {
	color := core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}
	format := core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecH264, Width: 1920, Height: 1080, FrameRate: core.FrameRate{Numerator: 30000, Denominator: 1001}, CodecProfile: "high", Level: "4.1", PixelFormat: "yuv420p", BitDepth: 8, Color: color}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 44100, Channels: 2, ChannelLayout: "stereo"}}
	input := format
	input.Video.Codec = core.CodecAV1
	input.Video.CodecProfile = "main"
	return NVIDIAConfig{Binary: "/usr/bin/ffmpeg", Input: input, Profile: core.GroupProfile{Mode: core.ModeTranscode, LiveAudioMode: core.LiveAudioTranscode, Format: format, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 6000000, MaxBitrateBPS: 7000000, BufferBits: 12000000, GOPFrames: 60, MaxBFrames: 2, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 192000}}, VideoTimeBase: media.TimeBase{Numerator: 1, Denominator: 30000}, AudioTimeBase: media.TimeBase{Numerator: 1, Denominator: 44100}}
}
func TestNVIDIACommandPreservesExplicitEncodingAndTiming(t *testing.T) {
	c := nvidiaCommandConfig()
	cmd, err := BuildNVIDIACommand(c, Attempt{ID: 1, InputPath: "/private/tmp/in socket", OutputPath: "/private/tmp/out socket"})
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for i := 0; i+1 < len(cmd.Args); i++ {
		if strings.HasPrefix(cmd.Args[i], "-") {
			values[cmd.Args[i]] = cmd.Args[i+1]
		}
	}
	for key, want := range map[string]string{"-hwaccel": "cuda", "-hwaccel_output_format": "cuda", "-c:v": "h264_nvenc", "-b:v": "6000000", "-maxrate:v": "7000000", "-bufsize:v": "12000000", "-g:v": "60", "-bf:v": "2", "-vf": "fps=fps=30000/1001:round=near:eof_action=round,scale_cuda=w=1920:h=1080:format=yuv420p", "-fps_mode:v": "passthrough", "-enc_time_base:v": "filter", "-profile:v": "high", "-level:v": "4.1", "-color_range:v": "tv", "-color_primaries:v": "bt709", "-color_trc:v": "bt709", "-colorspace:v": "bt709", "-ar:a": "44100", "-ac:a": "2", "-channel_layout:a": "stereo", "-b:a": "192000", "-profile:a": "aac_low", "-min_frag_duration": "1", "-use_editlist": "1", "-movie_timescale": "4410000", "-video_track_timescale": "30000", "-avoid_negative_ts": "disabled"} {
		if values[key] != want {
			t.Errorf("%s=%q, ожидалось %q", key, values[key], want)
		}
	}
	input := slices.Index(cmd.Args, "-i")
	flags2 := slices.Index(cmd.Args, "-flags2:a")
	flagCount := 0
	for _, arg := range cmd.Args {
		if arg == "-flags2:a" {
			flagCount++
		}
	}
	if flagCount != 1 || flags2 < 0 || flags2+1 >= len(cmd.Args) || cmd.Args[flags2+1] != "+skip_manual" || input < 0 || flags2 >= input {
		t.Fatalf("-flags2:a +skip_manual должен один раз настраивать аудиодекодер перед входом: %v", cmd.Args)
	}
	if _, ok := values["-r:v"]; ok {
		t.Fatal("частота уже задана фильтром fps")
	}
	if cmd.Args[len(cmd.Args)-1] != "unix:/private/tmp/out socket" {
		t.Fatal(cmd.Args)
	}
	joined := strings.Join(cmd.Args, " ")
	if strings.Contains(joined, "empty_moov") || strings.Contains(joined, "negative_cts_offsets") || strings.Contains(joined, "libx264") {
		t.Fatal(joined)
	}
	c.Profile.Mode = core.ModeCopy
	if _, err = BuildNVIDIACommand(c, Attempt{InputPath: "/a", OutputPath: "/b"}); !errors.Is(err, ErrInvalidSupervisor) {
		t.Fatal(err)
	}
}
func TestNVIDIACommandRejectsUnsupportedContractFields(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*NVIDIAConfig)
	}{
		{"цвет", func(c *NVIDIAConfig) { c.Profile.Format.Video.Color.Matrix = "bt470bg" }},
		{"шкала видео", func(c *NVIDIAConfig) { c.VideoTimeBase = media.TimeBase{} }},
		{"шкала аудио", func(c *NVIDIAConfig) { c.AudioTimeBase = media.TimeBase{} }},
		{"битность", func(c *NVIDIAConfig) { c.Profile.Format.Video.BitDepth = 10 }},
		{"Tier", func(c *NVIDIAConfig) { c.Profile.Format.Video.Tier = "main" }},
		{"открытый GOP", func(c *NVIDIAConfig) { c.Profile.VideoEncoding.ClosedGOP = false }},
		{"channel layout", func(c *NVIDIAConfig) { c.Profile.Format.Audio.ChannelLayout = "5.1" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := nvidiaCommandConfig()
			test.change(&c)
			if _, err := BuildNVIDIACommand(c, Attempt{InputPath: "/a", OutputPath: "/b"}); !errors.Is(err, ErrInvalidSupervisor) {
				t.Fatal(err)
			}
		})
	}
}

func TestNVIDIACommandConvertsProfileWithoutChangingItsIdentity(t *testing.T) {
	c := nvidiaCommandConfig()
	c.Input.Video.Width, c.Input.Video.Height = 2560, 1440
	c.Input.Video.FrameRate = core.FrameRate{Numerator: 60, Denominator: 1}
	c.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 60000, Denominator: 2002}
	c.Profile.Format.Audio.SampleRateHz = 48000
	c.VideoTimeBase = media.TimeBase{Numerator: 2, Denominator: 90000}
	c.AudioTimeBase = media.TimeBase{Numerator: 2, Denominator: 96000}
	before := c
	cmd, err := BuildNVIDIACommand(c, Attempt{InputPath: "/a", OutputPath: "/b"})
	if err != nil {
		t.Fatal(err)
	}
	if c != before {
		t.Fatal("построение команды изменило входной профиль")
	}
	for key, want := range map[string]string{
		"-vf":                    "fps=fps=30000/1001:round=near:eof_action=round,scale_cuda=w=1920:h=1080:format=yuv420p",
		"-movie_timescale":       "35280000",
		"-video_track_timescale": "30000",
		"-ar:a":                  "48000",
	} {
		i := slices.Index(cmd.Args, key)
		if i < 0 || i+1 >= len(cmd.Args) || cmd.Args[i+1] != want {
			t.Errorf("%s: ожидалось %q, argv=%v", key, want, cmd.Args)
		}
	}
}

func TestNVIDIACommandRejectsUnrepresentableMovieScale(t *testing.T) {
	for _, base := range []media.TimeBase{
		{Numerator: 1, Denominator: math.MaxInt32},
		{Numerator: 1, Denominator: math.MaxUint32},
	} {
		c := nvidiaCommandConfig()
		c.AudioTimeBase = base
		if cmd, err := BuildNVIDIACommand(c, Attempt{InputPath: "/a", OutputPath: "/b"}); err == nil || cmd.Binary != "" || len(cmd.Args) != 0 {
			t.Errorf("непредставимая шкала %v создала команду: %v, %v", base, cmd, err)
		}
	}
}

func TestNVIDIACommandExplicitRescaleAndRateControl(t *testing.T) {
	for _, rc := range []string{"cbr", "vbr"} {
		c := nvidiaCommandConfig()
		c.Profile.Encoding = core.DefaultEncoding(core.ModeTranscode, "h264_nvenc", rc)
		if rc == "cbr" {
			c.Profile.VideoEncoding.MaxBitrateBPS = c.Profile.VideoEncoding.TargetBitrateBPS
		}
		c.Profile.Encoding.RescaleOutput = false
		cmd, err := BuildNVIDIACommand(c, Attempt{InputPath: "/a", OutputPath: "/b"})
		if err != nil {
			t.Fatal(err)
		}
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

func TestNVIDIACommandCopyAudioRejectsMismatchWithoutEncoding(t *testing.T) {
	c := nvidiaCommandConfig()
	c.Profile.LiveAudioMode = core.LiveAudioCopy
	c.Profile.Encoding = core.DefaultEncoding(c.Profile.Mode, "h264_nvenc", "vbr")
	c.Profile.Encoding.AudioCustom = "aac_coder=fast"
	command, err := BuildNVIDIACommand(c, Attempt{InputPath: "/a", OutputPath: "/b"})
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(command.Args, "-c:a"); i < 0 || command.Args[i+1] != "copy" {
		t.Fatal(command.Args)
	}
	for _, forbidden := range []string{"-af", "-ar:a", "-ac:a", "-channel_layout:a", "-b:a", "-profile:a", "-aac_coder"} {
		if slices.Contains(command.Args, forbidden) {
			t.Fatalf("LIVE copy содержит %s: %v", forbidden, command.Args)
		}
	}
	for _, change := range []func(*core.AudioFormat){
		func(a *core.AudioFormat) { a.SampleRateHz = 48000 },
		func(a *core.AudioFormat) { a.Channels, a.ChannelLayout = 1, "mono" },
	} {
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
