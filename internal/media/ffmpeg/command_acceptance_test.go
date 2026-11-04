package ffmpeg_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

func TestAcceptanceNVIDIAArgumentsBindDecoderAndEncoderOnCorrectSides(t *testing.T) {
	color := core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}
	input := core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecAV1, Width: 1280, Height: 720, FrameRate: core.FrameRate{Numerator: 25, Denominator: 1}, CodecProfile: "main", PixelFormat: "yuv420p", BitDepth: 8, Color: color}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 44100, Channels: 2, ChannelLayout: "stereo"}}
	output := input
	output.Video.Codec = core.CodecH264
	output.Video.CodecProfile = "high"
	cfg := ffmpeg.NVIDIAConfig{Binary: "/usr/bin/ffmpeg", Input: input, Profile: core.GroupProfile{Mode: core.ModeTranscode, Format: output, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 3000000, MaxBitrateBPS: 4000000, BufferBits: 6000000, GOPFrames: 50, MaxBFrames: 2, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 128000}}, VideoTimeBase: media.TimeBase{Numerator: 1, Denominator: 1000}, AudioTimeBase: media.TimeBase{Numerator: 1, Denominator: 44100}}
	inputPath, outputPath := "/private/tmp/input literal;$(x)", "/private/tmp/output literal;$(x)"
	cmd, err := ffmpeg.BuildNVIDIACommand(cfg, ffmpeg.Attempt{ID: 1, InputPath: inputPath, OutputPath: outputPath})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(cmd.Args, "-i")
	if cmd.Binary != cfg.Binary || i < 1 || i+1 >= len(cmd.Args) || cmd.Args[i+1] != "async:unix:"+inputPath || cmd.Args[len(cmd.Args)-1] != "unix:"+outputPath {
		t.Fatal("argv потерял границы аргументов или вход/выход")
	}
	decoder, encoder := -1, -1
	for j := 0; j+1 < len(cmd.Args); j++ {
		if cmd.Args[j] == "-c:v" && cmd.Args[j+1] == "av1_cuvid" {
			decoder = j
		}
		if cmd.Args[j] == "-c:v" && cmd.Args[j+1] == "h264_nvenc" {
			encoder = j
		}
	}
	if decoder < 0 || decoder >= i || encoder <= i {
		t.Fatal("декодер и кодировщик назначены неверным сторонам -i")
	}
	flags := slices.Index(cmd.Args, "-fflags")
	if flags < 0 || flags >= i || flags+1 >= len(cmd.Args) || cmd.Args[flags+1] != "+ignidx" {
		t.Fatal("вход MP4 через Unix потерял проверенный запрет обратного перехода")
	}
	for _, pair := range [][2]string{{"-hwaccel", "cuda"}, {"-bf:v", "2"}, {"-g:v", "50"}, {"-movie_timescale", "441000"}, {"-min_frag_duration", "1"}, {"-avoid_negative_ts", "disabled"}} {
		pos := slices.Index(cmd.Args, pair[0])
		if pos < 0 || pos+1 >= len(cmd.Args) || cmd.Args[pos+1] != pair[1] {
			t.Errorf("потерян контрактный argv %v", pair)
		}
	}
	cfg.Profile.Mode = core.ModeCopy
	if _, err := ffmpeg.BuildNVIDIACommand(cfg, ffmpeg.Attempt{InputPath: inputPath, OutputPath: outputPath}); !errors.Is(err, ffmpeg.ErrInvalidSupervisor) {
		t.Fatal("copy создал команду кодирования:", err)
	}
}
