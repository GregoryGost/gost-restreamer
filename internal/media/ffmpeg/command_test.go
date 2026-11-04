package ffmpeg

import (
	"errors"
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
	return NVIDIAConfig{Binary: "/usr/bin/ffmpeg", Input: input, Profile: core.GroupProfile{Mode: core.ModeTranscode, Format: format, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 6000000, MaxBitrateBPS: 7000000, BufferBits: 12000000, GOPFrames: 60, MaxBFrames: 2, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 192000}}, VideoTimeBase: media.TimeBase{Numerator: 1, Denominator: 30000}}
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
	for key, want := range map[string]string{"-hwaccel": "cuda", "-hwaccel_output_format": "cuda", "-c:v": "h264_nvenc", "-b:v": "6000000", "-maxrate:v": "7000000", "-bufsize:v": "12000000", "-g:v": "60", "-bf:v": "2", "-r:v": "30000/1001", "-fps_mode:v": "passthrough", "-profile:v": "high", "-level:v": "4.1", "-color_range:v": "tv", "-color_primaries:v": "bt709", "-color_trc:v": "bt709", "-colorspace:v": "bt709", "-ar:a": "44100", "-ac:a": "2", "-channel_layout:a": "stereo", "-b:a": "192000", "-profile:a": "aac_low", "-min_frag_duration": "1", "-use_editlist": "1", "-movie_timescale": "4410000", "-avoid_negative_ts": "disabled"} {
		if values[key] != want {
			t.Errorf("%s=%q, ожидалось %q", key, values[key], want)
		}
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
		{"FPS", func(c *NVIDIAConfig) { c.Profile.Format.Video.FrameRate.Numerator = 25 }},
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
