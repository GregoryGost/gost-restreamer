package ffmpeg_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

// Проверяет размещение av1_cuvid до входа и h264_nvenc после него, точные аргументы fMP4 и буквальные
// пути UDS, а также отказ запуска кодирования для режима copy.
func TestAcceptanceNVIDIAArgumentsBindDecoderAndEncoderOnCorrectSides(t *testing.T) {
	// color задаёт одинаковое описание SDR bt709 для входного и выходного видео.
	color := core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}
	// input содержит контрольный вход AV1 720p25 со стерео AAC 44100 Гц.
	input := core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecAV1, Width: 1280, Height: 720, FrameRate: core.FrameRate{Numerator: 25, Denominator: 1}, CodecProfile: "main", PixelFormat: "yuv420p", BitDepth: 8, Color: color}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 44100, Channels: 2, ChannelLayout: "stereo"}}
	// output содержит целевой H.264-формат, полученный изменением контрольного входа.
	output := input
	output.Video.Codec = core.CodecH264
	output.Video.CodecProfile = "high"
	// cfg задаёт проверяемые параметры NVDEC/NVENC и временные шкалы команды.
	cfg := ffmpeg.NVIDIAConfig{Binary: "/usr/bin/ffmpeg", Input: input, Profile: core.GroupProfile{Mode: core.ModeTranscode, Format: output, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 3000000, MaxBitrateBPS: 4000000, BufferBits: 6000000, GOPFrames: 50, MaxBFrames: 2, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 128000}}, VideoTimeBase: media.TimeBase{Numerator: 1, Denominator: 1000}, AudioTimeBase: media.TimeBase{Numerator: 1, Denominator: 44100}}
	// inputPath задаёт входной путь, а outputPath задаёт выходной путь с символами оболочки для
	// проверки буквальных argv.
	inputPath, outputPath := "/private/tmp/input literal;$(x)", "/private/tmp/output literal;$(x)"
	// cmd содержит построенную аппаратную команду; err сообщает об отказе её построения.
	cmd, err := ffmpeg.BuildNVIDIACommand(cfg, ffmpeg.Attempt{ID: 1, InputPath: inputPath, OutputPath: outputPath})
	if err != nil {
		t.Fatal(err)
	}
	// i обозначает позицию -i, относительно которой проверяется назначение декодера и кодировщика.
	i := slices.Index(cmd.Args, "-i")
	if cmd.Binary != cfg.Binary || i < 1 || i+1 >= len(cmd.Args) || cmd.Args[i+1] != "async:unix:"+inputPath || cmd.Args[len(cmd.Args)-1] != "unix:"+outputPath {
		t.Fatal("argv потерял границы аргументов или вход/выход")
	}
	// decoder хранит позицию av1_cuvid; encoder хранит позицию h264_nvenc; -1 означает отсутствие
	// соответствующей пары.
	decoder, encoder := -1, -1
	// j обозначает позицию проверяемой пары аргументов кодека.
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
	// flags обозначает позицию входного -fflags для проверки +ignidx перед -i.
	flags := slices.Index(cmd.Args, "-fflags")
	if flags < 0 || flags >= i || flags+1 >= len(cmd.Args) || cmd.Args[flags+1] != "+ignidx" {
		t.Fatal("вход MP4 через Unix потерял проверенный запрет обратного перехода")
	}
	// pair содержит проверяемый флаг FFmpeg и его точное ожидаемое значение.
	for _, pair := range [][2]string{{"-hwaccel", "cuda"}, {"-bf:v", "2"}, {"-g:v", "50"}, {"-movie_timescale", "441000"}, {"-min_frag_duration", "1"}, {"-avoid_negative_ts", "disabled"}} {
		// pos обозначает позицию проверяемого флага в построенной команде.
		pos := slices.Index(cmd.Args, pair[0])
		if pos < 0 || pos+1 >= len(cmd.Args) || cmd.Args[pos+1] != pair[1] {
			t.Errorf("потерян контрактный argv %v", pair)
		}
	}
	cfg.Profile.Mode = core.ModeCopy
	// err содержит ожидаемый отказ построения команды кодирования для режима copy.
	if _, err := ffmpeg.BuildNVIDIACommand(cfg, ffmpeg.Attempt{InputPath: inputPath, OutputPath: outputPath}); !errors.Is(err, ffmpeg.ErrInvalidSupervisor) {
		t.Fatal("copy создал команду кодирования:", err)
	}
}
