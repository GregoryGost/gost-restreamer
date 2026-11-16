package encoding

import (
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestAudioBindingsPreserveSVTHighCBRVideoRecipe проверяет независимость видеорецепта от аудиопривязки.
// t требует одинаковый действительный видеорецепт при AAC и Opus, без запуска FFmpeg.
func TestAudioBindingsPreserveSVTHighCBRVideoRecipe(t *testing.T) {
	// previous задаёт допустимый SVT high4.0/CBR с секундным VBV и прежним AAC.
	previous := compilerProfile(core.CodecAV1)
	previous.LiveAudioMode = core.LiveAudioCopy
	previous.Encoding = core.DefaultEncoding(previous.Mode, core.VideoEncoderSVTAV1, core.RateControlCBR)
	previous.Format.Video.Level, previous.Format.Video.Tier = "4.0", "high"
	previous.VideoEncoding.BufferBits = previous.VideoEncoding.TargetBitrateBPS
	// baseline и err фиксируют видеопараметры исходного профиля перед сменой аудиокодека.
	baseline, err := Compile(previous, Prepared)
	if err != nil {
		t.Fatal("валидный исходный видеопрофиль", err)
	}
	// kind меняет только аудио; видеоконтракт и параметры остаются прежними.
	for _, kind := range []core.Codec{core.CodecAAC, core.CodecOpus} {
		// t сообщает об ошибках проверяемого сценария.
		t.Run(string(kind), func(t *testing.T) {
			// current сохраняет весь исходный видеопрофиль и меняет только аудиопривязку.
			current := previous
			if kind == core.CodecOpus {
				current.Format.Audio = core.AudioFormat{Codec: kind, CodecProfile: "celt", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}
				current.Encoding.AudioEncoder, current.AudioEncoding.BitrateBPS = core.AudioEncoderOpus, 128000
			}
			// validationErr доказывает, что публично допустимый профиль не должен падать в compiler.
			if validationErr := current.Validate(); validationErr != nil {
				t.Fatal("наследованный профиль отклонён", validationErr)
			}
			// plan и compileErr проверяют новый путь относительно исходного видеорецепта.
			plan, compileErr := Compile(current, Prepared)
			if compileErr != nil || !reflect.DeepEqual(plan.Video, baseline.Video) {
				t.Fatalf("video/audio independence: error=%v video=%q baseline=%q", compileErr, plan.Video, baseline.Video)
			}
		})
	}
}
