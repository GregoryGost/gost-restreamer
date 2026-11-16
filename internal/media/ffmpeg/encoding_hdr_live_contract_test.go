package ffmpeg

import (
	"slices"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// liveVideoConfig задаёт фактический вход source и явно выбранный выход encoder/transfer.
// Пустой transfer означает SDR BT.709; HDR сохраняет 10 бит и исходные CICP без преобразования сигнала.
func liveVideoConfig(source core.Codec, encoder, transfer string) NVIDIAConfig {
	// config использует проверенные шкалы и AAC прежнего контрольного профиля.
	config := nvidiaCommandConfig()
	config.Profile.LiveAudioMode = core.LiveAudioCopy
	config.Profile.VideoEncoding.MaxBFrames = 0
	config.Profile.VideoEncoding.TargetBitrateBPS, config.Profile.VideoEncoding.MaxBitrateBPS, config.Profile.VideoEncoding.BufferBits = 0, 0, 0
	config.Profile.Encoding = core.DefaultEncoding(core.ModeTranscode, encoder, core.RateControlCQP)
	config.Profile.Encoding.QP = core.OptionalQP{Set: true, Value: 23}
	config.Profile.Format.Video.Level, config.Profile.Format.Video.Tier = "", ""
	if encoder == core.VideoEncoderX265 || encoder == core.VideoEncoderVP9 {
		config.Profile.Encoding = core.DefaultEncoding(core.ModeTranscode, encoder, core.RateControlCRF)
		config.Profile.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: 2300}
	}
	switch encoder {
	case core.VideoEncoderX265, core.VideoEncoderHEVCNVENC:
		config.Profile.Format.Video.Codec, config.Profile.Format.Video.CodecProfile = core.CodecHEVC, "main"
	case core.VideoEncoderVP9:
		config.Profile.Format.Video.Codec, config.Profile.Format.Video.CodecProfile = core.CodecVP9, "profile0"
	}
	if transfer != "" {
		config.Profile.Format.Video.PixelFormat, config.Profile.Format.Video.BitDepth = "yuv420p10le", 10
		config.Profile.Format.Video.Color = core.ColorInfo{Range: "limited", Primaries: "bt2020", Transfer: transfer, Matrix: "bt2020nc"}
		switch encoder {
		case core.VideoEncoderX264:
			config.Profile.Format.Video.CodecProfile = "high10"
		case core.VideoEncoderVP9:
			config.Profile.Format.Video.CodecProfile = "profile2"
		default:
			config.Profile.Format.Video.CodecProfile = "main10"
		}
	}
	config.Input.Video = config.Profile.Format.Video
	config.Input.Video.Codec, config.Input.Video.Level, config.Input.Video.Tier = source, "", ""
	switch source {
	case core.CodecAV1:
		config.Input.Video.CodecProfile = "main"
	case core.CodecHEVC:
		config.Input.Video.CodecProfile = "main"
		if transfer != "" {
			config.Input.Video.CodecProfile = "main10"
		}
	case core.CodecVP9:
		config.Input.Video.CodecProfile = "profile0"
		if transfer != "" {
			config.Input.Video.CodecProfile = "profile2"
		}
	}
	return config
}

// TestDefaultLiveBuilderSupportsExplicitVideoBindings проверяет штатный LIVE-построитель без процесса/GPU.
// t сравнивает argv с выбранным кодировщиком и сохранёнными HDR CICP, глубиной и временной семантикой.
func TestDefaultLiveBuilderSupportsExplicitVideoBindings(t *testing.T) {
	// encoder перечисляет фиксированные привязки HEVC, VP9 и HDR AVC.
	for _, encoder := range []string{core.VideoEncoderX265, core.VideoEncoderHEVCNVENC, core.VideoEncoderVP9, core.VideoEncoderX264} {
		// transfer выбирает SDR, PQ или HLG; H.264 здесь проверяет именно новые HDR-пары.
		for _, transfer := range []string{"", "smpte2084", "arib-std-b67"} {
			if encoder == core.VideoEncoderX264 && transfer == "" {
				continue
			}
			// source перечисляет разрешённые видеокодеки LIVE ingest с известным фактическим форматом.
			for _, source := range []core.Codec{core.CodecAV1, core.CodecHEVC, core.CodecVP9} {
				// Подтест через t строит одну команду, проверяет неизменность входа и буквальные значения argv.
				t.Run(encoder+"/"+transfer+"/"+string(source), func(t *testing.T) {
					// config и before сохраняют валидный вход и неизменяемый профиль до построения команды.
					config := liveVideoConfig(source, encoder, transfer)
					before := config
					if config.Input.Validate() != nil || config.Profile.Validate() != nil {
						t.Fatal("Некорректная фикстура", config)
					}
					// command и err отражают реальную границу, вызываемую штатным работником Group.
					command, err := BuildNVIDIACommand(config, Attempt{InputPath: "/private/tmp/live in", OutputPath: "/private/tmp/live out"})
					if err != nil {
						t.Fatalf("Допустимый LIVE %s → %s/%s отвергнут: %v", source, encoder, transfer, err)
					}
					if config != before {
						t.Fatal("Построитель изменил фактический вход либо профиль")
					}
					// flag и want задают независимые контрактные значения без вызова encoding.Compile.
					for flag, want := range map[string]string{"-c:v": encoder, "-c:a": "copy", "-fps_mode:v": "passthrough", "-enc_time_base:v": "filter", "-avoid_negative_ts": "disabled", "-i": "async:unix:/private/tmp/live in"} {
						// position указывает на флаг в готовой команде, а не на внутренний план encoder.
						position := slices.Index(command.Args, flag)
						if position < 0 || position+1 >= len(command.Args) || command.Args[position+1] != want {
							t.Errorf("%s: ожидалось %q, argv=%v", flag, want, command.Args)
						}
					}
					// args позволяет проверить отсутствие скрытого CUDA decode и понижения до восьми бит.
					args := strings.Join(command.Args, " ")
					if strings.Contains(args, "av1_cuvid") || strings.Contains(args, "scale_cuda") || strings.Contains(args, "-hwaccel") || command.Args[len(command.Args)-1] != "unix:/private/tmp/live out" {
						t.Fatal(args)
					}
					if transfer != "" && (!strings.Contains(args, "format=yuv420p10le") || !strings.Contains(args, "-color_trc:v "+transfer) || !strings.Contains(args, "-color_primaries:v bt2020") || !strings.Contains(args, "-colorspace:v bt2020nc")) {
						t.Fatal("Потеряны HDR-плоскости либо CICP", args)
					}
				})
			}
		}
	}
}

// TestDefaultLiveBuilderKeepsSelectedSoftwareBinding защищает явный программный маршрут.
// t проверяет прежний AV1 → x264 SDR и конкретный выбранный encoder без запуска FFmpeg.
func TestDefaultLiveBuilderKeepsSelectedSoftwareBinding(t *testing.T) {
	// config содержит прежний разрешённый SDR-рецепт с программным декодированием.
	config := nvidiaCommandConfig()
	config.Profile.Encoding = core.DefaultEncoding(core.ModeTranscode, core.VideoEncoderX264, core.RateControlVBR)
	// command и err проверяют положительный контроль той же публичной функции.
	command, err := BuildNVIDIACommand(config, Attempt{InputPath: "/private/tmp/in", OutputPath: "/private/tmp/out"})
	if err != nil || !strings.Contains(strings.Join(command.Args, " "), "-c:v libx264") {
		t.Fatal(command, err)
	}
}
