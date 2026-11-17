package fallback

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestCurrentPreparationPreservesBoundedUnknownSourceRendering проверяет PNG/JPEG и
// SDR 8/420 без CICP через единственный builder. t требует пригодный output, сохраняя
// отсутствие исходного цвета и отклоняя расширения за пределами этого рендерера.
func TestCurrentPreparationPreservesBoundedUnknownSourceRendering(t *testing.T) {
	// profile задаёт подтверждённую SVT/AAC-подготовку BT.709 8/420.
	profile := selfProfile(core.CodecAV1)
	// unknown содержит структурное описание видео с отсутствующими CICP.
	unknown := profile.Format.Video
	unknown.Color = core.ColorInfo{Range: "unspecified", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}
	// source выбирает подтверждённый вид исходного материала без приписывания ему desired.
	for _, source := range []Source{
		{Kind: core.AssetImage, Duration: time.Second, Demuxer: "png_pipe", Format: core.SourceFormat{ImageFormat: "png"}},
		{Kind: core.AssetImage, Duration: time.Second, Demuxer: "jpeg_pipe", Format: core.SourceFormat{ImageFormat: "jpeg"}},
		{Kind: core.AssetVideo, Duration: time.Second, Demuxer: "mov", Format: core.SourceFormat{Video: &unknown}},
	} {
		// before сохраняет наблюдаемые значения для независимого сравнения после builder.
		before := source
		// command и err содержат результат построения команды без запуска FFmpeg.
		command, err := ProductionBuilder("ffmpeg")(GenerateRequest{Source: source, Profile: profile, LoopDuration: 1600 * time.Millisecond})
		if err != nil || !strings.Contains(strings.Join(command.Args, " "), "format=yuv420p,setparams=range=limited:color_primaries=bt709") || !reflect.DeepEqual(source, before) || unknown.Color.Primaries != "unspecified" {
			t.Fatal(source.Kind, command, err)
		}
		// expanded меняет только кодировщик; AOM не входит в ограниченный renderer.
		expanded := profile
		expanded.Encoding = core.DefaultEncoding(profile.Mode, core.VideoEncoderAOMAV1, core.RateControlCRF)
		expanded.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: 3500}
		expanded.VideoEncoding.TargetBitrateBPS, expanded.VideoEncoding.MaxBitrateBPS, expanded.VideoEncoding.BufferBits = 0, 0, 0
		if _, err = ProductionBuilder("ffmpeg")(GenerateRequest{Source: source, Profile: expanded, LoopDuration: 1600 * time.Millisecond}); err == nil {
			t.Fatal("Неподтверждённый цвет разрешил произвольный кодировщик", source.Kind)
		}
	}
}

// TestPreparedHEVCNVENCUsesOnePhysicalP010Format проверяет физическое представление
// кадра до NVENC без GPU-запуска. t требует единственный pix_fmt после общего codec plan.
func TestPreparedHEVCNVENCUsesOnePhysicalP010Format(t *testing.T) {
	// profile выбирает HEVC Main10 SDR и фиксированное качество NVENC.
	profile := selfProfile(core.CodecH264)
	profile.Format.Video.Codec, profile.Format.Video.CodecProfile = core.CodecHEVC, "main10"
	profile.Format.Video.PixelFormat, profile.Format.Video.BitDepth = "yuv420p10le", 10
	profile.Format.Video.Level = "auto"
	profile.Encoding = core.DefaultEncoding(profile.Mode, core.VideoEncoderHEVCNVENC, core.RateControlCQP)
	profile.Encoding.QP = core.OptionalQP{Set: true, Value: 23}
	profile.VideoEncoding.TargetBitrateBPS, profile.VideoEncoding.MaxBitrateBPS, profile.VideoEncoding.BufferBits = 0, 0, 0
	// source содержит известный SDR-формат до преобразования в десятибитный target.
	source := selfProfile(core.CodecH264).Format.Video
	// command и err задают результат единственного компилятора и физического builder.
	command, err := ProductionBuilder("ffmpeg")(GenerateRequest{Source: Source{Kind: core.AssetVideo, Duration: time.Second, Demuxer: "mov", Format: core.SourceFormat{Video: &source}}, Profile: profile, LoopDuration: 1600 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// args сохраняет полную последовательность параметров для проверки фильтра и pix_fmt.
	args := strings.Join(command.Args, " ")
	if strings.Count(args, "-pix_fmt") != 1 || !strings.Contains(args, "-pix_fmt:v p010le") || !strings.Contains(args, ",format=p010le") {
		t.Fatal(args)
	}
}
