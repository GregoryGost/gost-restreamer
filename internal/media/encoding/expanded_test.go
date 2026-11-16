package encoding

import (
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestExpandedCompilerRecipes проверяет точные нулевые и дробные значения качества, параметры
// кодировщиков и отсутствие скрытого VBV для чистого качества всех новых программных кодировщиков.
// t сверяет точное качество и ограничения аргументов каждого кодировщика.
func TestExpandedCompilerRecipes(t *testing.T) {
	// scenario задаёт encoder, rc, качество value в его публичных единицах и обязательные argv.
	for _, scenario := range []struct {
		// encoder выбирает кодировщик, rc — способ качества, want — точную часть аргументов.
		encoder, rc, want string
		// value содержит QP либо сотые доли CRF в зависимости от rc.
		value int
	}{
		{core.VideoEncoderX264, core.RateControlCRF, "-crf 23.57", 2357},
		{core.VideoEncoderX264, core.RateControlCRF, "-crf 0", 0},
		{core.VideoEncoderX264, core.RateControlCQP, "-qp 0", 0},
		{core.VideoEncoderAOMAV1, core.RateControlCRF, "-b:v 0 -crf 0", 0},
		{core.VideoEncoderAOMAV1, core.RateControlCRF, "-b:v 0 -crf 63", 6300},
		{core.VideoEncoderSVTAV1, core.RateControlCRF, "-crf 35", 3500},
		{core.VideoEncoderNVENC, core.RateControlCQP, "-init_qpI 0 -init_qpP 0 -init_qpB 0", 0},
	} {
		// t сообщает об ошибках проверяемого сценария.
		t.Run(scenario.encoder+"/"+scenario.rc+"/"+scenario.want, func(t *testing.T) {
			// Компилирует отдельный профиль и сверяет рецепт без изменения входного значения.
			// p задаёт полный целевой профиль; codecName выбирает соответствующий видеокодек.
			codecName := core.CodecH264
			if scenario.encoder == core.VideoEncoderAOMAV1 || scenario.encoder == core.VideoEncoderSVTAV1 {
				codecName = core.CodecAV1
			}
			p := compilerProfile(codecName)
			p.LiveAudioMode = core.LiveAudioCopy
			p.Encoding = core.DefaultEncoding(p.Mode, scenario.encoder, scenario.rc)
			p.VideoEncoding.TargetBitrateBPS, p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 0, 0, 0
			if scenario.encoder == core.VideoEncoderX264 || scenario.encoder == core.VideoEncoderNVENC {
				p.Format.Video.CodecProfile, p.Format.Video.PixelFormat = "high444", "yuv444p"
			}
			if scenario.rc == core.RateControlCQP {
				p.Encoding.QP = core.OptionalQP{Set: true, Value: scenario.value}
			} else {
				p.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: scenario.value}
			}
			if scenario.encoder == core.VideoEncoderNVENC {
				p.Encoding.NVENC.Tuning = "lossless"
			}
			// before сохраняет входной профиль для обнаружения неявного назначения настроек.
			before := p
			// plan содержит результат компиляции; err сообщает об отказе проверенного рецепта.
			plan, err := Compile(p, Prepared)
			if err != nil {
				t.Fatal(err)
			}
			// args облегчает проверку параметров кодировщика без привязки к вспомогательным функциям.
			args := strings.Join(plan.Video, " ")
			if !strings.Contains(args, scenario.want) || strings.Contains(args, "-maxrate") || strings.Contains(args, "-bufsize") || p != before {
				t.Fatal(args)
			}
			if scenario.encoder != core.VideoEncoderAOMAV1 && strings.Contains(args, "-b:v") {
				t.Fatal("quality получил целевой битрейт", args)
			}
			if scenario.encoder == core.VideoEncoderNVENC && (!strings.Contains(args, "-coder cavlc") || !strings.Contains(args, "-tune lossless") || !strings.Contains(args, "-rc constqp")) {
				t.Fatal(args)
			}
			if scenario.encoder == core.VideoEncoderAOMAV1 && (!strings.Contains(args, "-keyint_min:v 15") || !strings.Contains(args, "-lag-in-frames 0") || strings.Contains(args, "-level")) {
				t.Fatal(args)
			}
		})
	}
}

// TestExpandedCompilerLevelAndNativeAudio проверяет явную передачу level/tier кодировщику и сохраняет
// допуск исправления только свежего AAC 7.1 текущего профиля, исключая LIVE copy.
// t контролирует параметры уровня и границу собственного AAC.
func TestExpandedCompilerLevelAndNativeAudio(t *testing.T) {
	// p задаёт SVT high CBR с точно представимым буфером в одну секунду.
	p := compilerProfile(core.CodecAV1)
	p.LiveAudioMode = core.LiveAudioCopy
	p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderSVTAV1, core.RateControlCBR)
	p.Format.Video.Level, p.Format.Video.Tier = "4.0", "high"
	p.VideoEncoding.BufferBits = p.VideoEncoding.TargetBitrateBPS
	// plan содержит реальные параметры кодировщика; err сообщает об отказе компиляции.
	plan, err := Compile(p, Prepared)
	if err != nil || !strings.Contains(strings.Join(plan.Video, " "), "tier=1:tile-columns=0:tile-rows=0") || !strings.Contains(strings.Join(plan.Video, " "), "-level:v 4.0") {
		t.Fatal(plan, err)
	}
	p = compilerProfile(core.CodecH264)
	p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderNVENC, core.RateControlVBR)
	p.LiveAudioMode = core.LiveAudioTranscode
	p.Format.Video.Level = "6.0"
	p.Format.Audio.Channels, p.Format.Audio.ChannelLayout = 8, "7.1"
	plan, err = Compile(p, Live)
	if err != nil || !strings.Contains(strings.Join(plan.Video, " "), "-dpb_size 1") || !UsesNativeAAC71(p, Live) || !strings.Contains(strings.Join(plan.Audio, " "), "-aac_pce 0") {
		t.Fatal(plan, err)
	}
	p.LiveAudioMode = core.LiveAudioCopy
	if UsesNativeAAC71(p, Live) || !UsesNativeAAC71(p, Prepared) {
		t.Fatal("неверная граница свежего AAC")
	}
}

// TestConverterRejectsUnknownEndpoints проверяет необходимые отказы до процесса и явные
// source/target CICP вместо ретегирования или неявного преобразования gray через scale.
func TestConverterRejectsUnknownEndpoints(t *testing.T) {
	// source содержит известный фактический BT709; target задаёт другой реальный цвет и глубину.
	source := compilerProfile(core.CodecAV1).Format.Video
	target := source
	target.PixelFormat, target.BitDepth = "yuv444p10le", 10
	target.Color = core.ColorInfo{Primaries: "bt2020", Transfer: "gamma22", Matrix: "bt2020nc", Range: "full"}
	// filter содержит программное преобразование; err сообщает о неожиданном отказе endpoint.
	filter, err := SoftwareVideoFilter(source, target, false)
	if err != nil || !strings.Contains(filter, "primariesin=1:transferin=1:matrixin=1:rangein=limited:primaries=9:transfer=4:matrix=9:range=full") || !strings.Contains(filter, "format=yuv444p10le") {
		t.Fatal(filter, err)
	}
	// mutate меняет один обязательный факт источника на неизвестный или несовместимый.
	for _, mutate := range []func(*core.VideoFormat){
		func(v *core.VideoFormat) { /* Удаляет подтверждённые primaries. */
			v.Color.Primaries = "unspecified"
		},
		func(v *core.VideoFormat) { /* Вносит неизвестную передаточную функцию. */
			v.Color.Transfer = "cicp:17"
		},
		func(v *core.VideoFormat) { /* Создаёт противоречие между плоскостями и глубиной. */
			v.BitDepth = 10
		},
		func(v *core.VideoFormat) { /* Создаёт запрещённое сочетание передаточной функции и матрицы с постоянной яркостью. */
			v.Color.Matrix, v.Color.Transfer = "bt2020c", "gamma22"
		},
		func(v *core.VideoFormat) { /* Нарушает запрет масштабирования. */
			v.Width += 2
		},
	} {
		// invalid сохраняет остальные исходные поля для изоляции причины отказа.
		invalid := source
		mutate(&invalid)
		if _, err = SoftwareVideoFilter(invalid, target, false); err == nil {
			t.Fatal(invalid)
		}
	}
	source.PixelFormat = "gray"
	target = source
	filter, err = SoftwareVideoFilter(source, target, false)
	if err != nil || !strings.Contains(filter, "mergeplanes=0x001020:format=yuv444p") || !strings.Contains(filter, "extractplanes=y") || strings.Contains(filter, ",scale=") {
		t.Fatal(filter, err)
	}
}
