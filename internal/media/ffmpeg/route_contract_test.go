package ffmpeg

import (
	"slices"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestCurrentLiveRouteKeepsSinglePhysicalPixelFormat проверяет детерминированный выбор CUDA
// для ограниченного AV1/BT.709/NVENC и программного преобразования остальных допустимых сочетаний.
// t сверяет фактические argv; аппаратный запуск и наличие GPU здесь не проверяются.
func TestCurrentLiveRouteKeepsSinglePhysicalPixelFormat(t *testing.T) {
	// scenario выбирает один действующий маршрут и ожидаемый формат выходных кадров.
	for _, scenario := range []struct {
		// name обозначает проверяемую привязку; cuda требует аппаратные входные кадры.
		name string
		cuda bool
		// pixel задаёт единственное ожидаемое физическое представление после фильтров.
		pixel string
	}{
		{"nvenc-vbr", true, "cuda"},
		{"nvenc-cbr", true, "cuda"},
		{"nvenc-cqp", false, "yuv420p"},
		{"x264", false, "yuv420p"},
		{"known-color-conversion", false, "yuv420p"},
		{"hevc-nvenc-p010", false, "p010le"},
	} {
		// Подтест t строит отдельную команду, не меняя общий шаблон конфигурации.
		t.Run(scenario.name, func(t *testing.T) {
			// config задаёт полный AV1-вход и явный H.264 NVENC VBR.
			config := nvidiaCommandConfig()
			switch scenario.name {
			case "nvenc-cbr":
				config.Profile.Encoding = core.DefaultEncoding(core.ModeTranscode, core.VideoEncoderNVENC, core.RateControlCBR)
				config.Profile.VideoEncoding.MaxBitrateBPS = config.Profile.VideoEncoding.TargetBitrateBPS
			case "nvenc-cqp":
				config.Profile.Encoding = core.DefaultEncoding(core.ModeTranscode, core.VideoEncoderNVENC, core.RateControlCQP)
				config.Profile.Encoding.QP = core.OptionalQP{Set: true, Value: 23}
				config.Profile.Format.Video.Level = ""
				config.Profile.VideoEncoding.TargetBitrateBPS, config.Profile.VideoEncoding.MaxBitrateBPS, config.Profile.VideoEncoding.BufferBits = 0, 0, 0
			case "x264":
				config.Profile.Encoding = core.DefaultEncoding(core.ModeTranscode, core.VideoEncoderX264, core.RateControlVBR)
			case "known-color-conversion":
				config.Profile.Format.Video.Color.Transfer = "gamma22"
			case "hevc-nvenc-p010":
				config = liveVideoConfig(core.CodecAV1, core.VideoEncoderHEVCNVENC, "smpte2084")
			}
			// before сохраняет все фактические и желаемые сведения для проверки неизменности.
			before := config
			// command содержит настоящий argv; err сообщает о недопустимой привязке.
			command, err := BuildNVIDIACommand(config, Attempt{InputPath: "/private/tmp/route-in", OutputPath: "/private/tmp/route-out"})
			if err != nil || config != before {
				t.Fatal(command, err)
			}
			// pixels считает все формы выходного pix_fmt; position задаёт позицию, argument — очередной флаг команды.
			pixels := 0
			for position, argument := range command.Args {
				if argument != "-pix_fmt" && argument != "-pix_fmt:v" {
					continue
				}
				pixels++
				if position+1 >= len(command.Args) || command.Args[position+1] != scenario.pixel {
					t.Fatalf("физический формат кадров переопределён: %v", command.Args)
				}
			}
			// arguments сохраняет порядок флагов для проверки декодера и фильтра.
			arguments := strings.Join(command.Args, " ")
			if pixels != 1 || slices.Contains(command.Args, "-hwaccel") != scenario.cuda || strings.Contains(arguments, "av1_cuvid") != scenario.cuda {
				t.Fatal("неоднозначный маршрут или формат кадров", arguments)
			}
			if !scenario.cuda && !strings.Contains(arguments, "zscale=") {
				t.Fatal("программный маршрут потерял фактическое преобразование", arguments)
			}
			if scenario.pixel == "p010le" && !strings.Contains(arguments, "format=p010le") {
				t.Fatal("P010 не закреплён после десятибитного фильтра", arguments)
			}
		})
	}
}
