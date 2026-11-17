package ffmpeg

import (
	"errors"
	"slices"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// modelContractArgument возвращает единственное значение flag из args, сохраняя порядок argv.
// t отклоняет отсутствие и повторы: последний аргумент не должен скрывать переопределение кодека или кадров.
func modelContractArgument(t *testing.T, args []string, flag string) string {
	t.Helper()
	// found сохраняет найденное значение; count считает все появления проверяемого флага.
	found, count := "", 0
	// index и argument обходят фактические отдельные аргументы команды.
	for index, argument := range args {
		if argument == flag {
			count++
			if index+1 >= len(args) {
				t.Fatalf("Флаг %s без значения: %v", flag, args)
			}
			found = args[index+1]
		}
	}
	if count != 1 {
		t.Fatalf("Нужно одно значение %s, найдено %d: %v", flag, count, args)
	}
	return found
}

// TestProfileModelLiveAudioBindingControlsPhysicalRoute проверяет независимость аудио от видеорецепта.
// t удерживает один AV1→NVENC-видеопрофиль: новые аудиопривязки должны выбрать программное декодирование,
// сохранить NVENC и единственный yuv420p, а AAC — ограниченный CUDA с единственным cuda.
// Обработчик подтеста проверяет также запрет несовместимого copy и разрешённое точное копирование AU.
func TestProfileModelLiveAudioBindingControlsPhysicalRoute(t *testing.T) {
	// binding задаёт независимый контракт каждой конечной аудиопривязки и допустимость CUDA.
	for _, binding := range []struct {
		// codec и profile описывают выходное аудио; encoder и bitrate задают выбранный рецепт.
		codec            core.Codec
		profile, encoder string
		bitrate          uint64
		// cuda разрешён только для AAC при сохранённом ограниченном видеосочетании.
		cuda bool
	}{
		{core.CodecAAC, "LC", "aac", 128000, true},
		{core.CodecOpus, "celt", "libopus", 128000, false},
		{core.CodecFLAC, "s16", "flac", 0, false},
		{core.CodecMP3, "mpeg1-layer3", "libmp3lame", 128000, false},
		{core.CodecAC3, "ac3", "ac3", 192000, false},
	} {
		// t сообщает об ошибках проверяемого сценария.
		t.Run(string(binding.codec), func(t *testing.T) {
			// config содержит наблюдаемый AAC44100 и выбранную полную аудиопривязку 48000/stereo.
			config := nvidiaCommandConfig()
			config.Profile.Format.Audio = core.AudioFormat{Codec: binding.codec, CodecProfile: binding.profile, SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}
			config.Profile.Encoding.AudioEncoder, config.Profile.AudioEncoding.BitrateBPS = binding.encoder, binding.bitrate
			// before защищает исходные фактические параметры и выбранный профиль от скрытой проекции.
			before := config
			// command и err проверяют реальный построитель команды без запуска процессов или аппаратуры.
			command, err := BuildNVIDIACommand(config, Attempt{InputPath: "/private/tmp/model-audio-in", OutputPath: "/private/tmp/model-audio-out"})
			if err != nil || config != before {
				t.Fatal("Привязка изменила вход или не построилась", err)
			}
			if modelContractArgument(t, command.Args, "-c:a") != binding.encoder || slices.Contains(command.Args, "-hwaccel") != binding.cuda {
				t.Fatal("Аудиорецепт или физический маршрут подменён", command.Args)
			}
			// output содержит только параметры после входа, исключая законный отдельный выбор декодера CUDA.
			output := command.Args[slices.Index(command.Args, "-i")+2:]
			if modelContractArgument(t, output, "-c:v") != "h264_nvenc" {
				t.Fatal("Builder автоматически заменил выбранный кодировщик", command.Args)
			}
			if binding.cuda {
				if modelContractArgument(t, output, "-pix_fmt") != "cuda" || slices.Contains(output, "-pix_fmt:v") {
					t.Fatal("CUDA-кадры переопределены планом кодека", output)
				}
			} else if modelContractArgument(t, output, "-pix_fmt:v") != "yuv420p" || slices.Contains(output, "-pix_fmt") {
				t.Fatal("Программный формат кадров неоднозначен", output)
			}
			config.Profile.LiveAudioMode = core.LiveAudioCopy
			if _, err = BuildNVIDIACommand(config, Attempt{InputPath: "/a", OutputPath: "/b"}); !errors.Is(err, ErrInvalidSupervisor) {
				t.Fatal("Несовместимый LIVE copy принят", err)
			}
			if binding.codec == core.CodecFLAC {
				return
			}
			config.Input.Audio = config.Profile.Format.Audio
			config.AudioTimeBase = media.TimeBase{Numerator: 1, Denominator: 48000}
			command, err = BuildNVIDIACommand(config, Attempt{InputPath: "/a", OutputPath: "/b"})
			if err != nil || modelContractArgument(t, command.Args, "-c:a") != "copy" {
				t.Fatal("Точное LIVE copy отвергнуто", err)
			}
			// flag выбирает параметр, который мог бы незаметно перекодировать копируемые AU.
			for _, flag := range []string{"-b:a", "-ar:a", "-ac:a", "-af", "-frame_duration", "-reservoir", "-sample_fmt:a"} {
				if slices.Contains(command.Args, flag) {
					t.Fatal("LIVE copy содержит параметры перекодирования", flag, command.Args)
				}
			}
		})
	}
}
