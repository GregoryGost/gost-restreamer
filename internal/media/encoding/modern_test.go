package encoding

import (
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// modernProfile создаёт явный тестовый HEVC/VP9 профиль кодировщика encoder и режима качества/битрейта rc.
func modernProfile(encoder, rc string) core.GroupProfile {
	// profile сохраняет общие формат, AAC и GOP старого тестового профиля.
	profile := compilerProfile(core.CodecH264)
	profile.LiveAudioMode = core.LiveAudioCopy
	profile.Encoding = core.DefaultEncoding(profile.Mode, encoder, rc)
	profile.Format.Video.Codec, profile.Format.Video.CodecProfile = core.CodecHEVC, "main"
	profile.VideoEncoding.TargetBitrateBPS, profile.VideoEncoding.MaxBitrateBPS, profile.VideoEncoding.BufferBits = 0, 0, 0
	if encoder == core.VideoEncoderVP9 {
		profile.Format.Video.Codec, profile.Format.Video.CodecProfile = core.CodecVP9, "profile0"
	}
	switch rc {
	case core.RateControlCRF:
		profile.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: 2357}
		if encoder == core.VideoEncoderVP9 {
			profile.Encoding.CRF.ValueHundredths = 2300
		}
	case core.RateControlCQP:
		profile.Encoding.QP = core.OptionalQP{Set: true, Value: 0}
	}
	return profile
}

// TestModernCompilerNativeRecipes проверяет чистое качество, фиксированный GOP и точные параметры кодировщика,
// включая настройки HEVC NVENC и отсутствие физического pix_fmt либо скрытого VBV.
// t проверяет нативные параметры без запуска аппаратуры.
func TestModernCompilerNativeRecipes(t *testing.T) {
	// scenario задаёт один нативный рецепт и обязательный фрагмент аргументов.
	for _, scenario := range []struct {
		// encoder, rc и want выбирают кодировщик, режим и его обязательный точный аргумент.
		encoder, rc, want string
	}{
		{core.VideoEncoderX265, core.RateControlCRF, "-crf 23.57"},
		{core.VideoEncoderX265, core.RateControlCQP, "-qp 0"},
		{core.VideoEncoderHEVCNVENC, core.RateControlCQP, "-init_qpI 0 -init_qpP 0 -init_qpB 0"},
		{core.VideoEncoderVP9, core.RateControlCRF, "-b:v 0 -crf 23"},
	} {
		// t сообщает об ошибках проверяемого сценария.
		t.Run(scenario.encoder+"/"+scenario.rc, func(t *testing.T) {
			// Проверяет реальный Compile и неизменность исходного профиля без запуска аппаратуры.
			// profile задаёт выход HDR10 и соответствующие ему профиль и десятибитные плоскости.
			profile := modernProfile(scenario.encoder, scenario.rc)
			profile.Format.Video.BitDepth, profile.Format.Video.PixelFormat = 10, "yuv420p10le"
			profile.Format.Video.CodecProfile = "main10"
			if scenario.encoder == core.VideoEncoderVP9 {
				profile.Format.Video.CodecProfile = "profile2"
			}
			profile.Format.Video.Color = core.ColorInfo{Range: "limited", Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc"}
			// before обнаруживает неявную материализацию; plan/err содержат результат компиляции.
			before := profile
			plan, err := Compile(profile, Prepared)
			if err != nil {
				t.Fatal(err)
			}
			// video позволяет сверить связанные пары аргументов без изменения порядка массива.
			video := strings.Join(plan.Video, " ")
			if profile != before || !strings.Contains(video, scenario.want) || strings.Contains(video, "-maxrate") || strings.Contains(video, "-bufsize") {
				t.Fatal(video)
			}
			if scenario.encoder == core.VideoEncoderHEVCNVENC && (strings.Contains(video, "-pix_fmt") || !strings.Contains(video, "-preset p4 -tune hq") || profile.Encoding.NVENC != (core.NVENCSettings{})) {
				t.Fatal(video)
			}
			if scenario.encoder == core.VideoEncoderVP9 && (!strings.Contains(video, "-profile:v 2") || !strings.Contains(video, "-keyint_min:v 15") || !strings.Contains(video, "-lag-in-frames 0 -auto-alt-ref 0")) {
				t.Fatal(video)
			}
			if scenario.encoder == core.VideoEncoderX265 && !strings.Contains(video, "open-gop=0:scenecut=0:bframes=0:ref=1") {
				t.Fatal(video)
			}
		})
	}
}

// TestModernCompilerBitrateAndAACGuards проверяет нативные CBR/VBR и допуск коррекции только свежего AAC 7.1.
func TestModernCompilerBitrateAndAACGuards(t *testing.T) {
	// encoder выбирает один из трёх новых фиксированных рецептов.
	for _, encoder := range []string{core.VideoEncoderX265, core.VideoEncoderHEVCNVENC, core.VideoEncoderVP9} {
		// profile содержит точно представимый CBR и обычный AAC7.1.
		profile := modernProfile(encoder, core.RateControlCBR)
		profile.VideoEncoding.TargetBitrateBPS, profile.VideoEncoding.MaxBitrateBPS, profile.VideoEncoding.BufferBits = 100000, 100000, 200000
		profile.Format.Audio.Channels, profile.Format.Audio.ChannelLayout = 8, "7.1"
		// plan/err проверяют отсутствие неявного копирования аудио при подготовке.
		plan, err := Compile(profile, Prepared)
		if err != nil || !UsesNativeAAC71(profile, Prepared) || UsesNativeAAC71(profile, Live) || !strings.Contains(strings.Join(plan.Audio, " "), "-aac_pce 0") {
			t.Fatal(plan, err)
		}
		if encoder == core.VideoEncoderX265 && !strings.Contains(strings.Join(plan.Video, " "), "strict-cbr=1") {
			t.Fatal(plan.Video)
		}
		if encoder == core.VideoEncoderVP9 && !strings.Contains(strings.Join(plan.Video, " "), "-minrate:v 100000") {
			t.Fatal(plan.Video)
		}
		profile.Encoding.RateControl = core.RateControlVBR
		if encoder == core.VideoEncoderVP9 {
			profile.VideoEncoding.MaxBitrateBPS, profile.VideoEncoding.BufferBits = 0, 0
		}
		plan, err = Compile(profile, Live)
		if err != nil || strings.Join(plan.Audio, " ") != "-c:a copy" || strings.Contains(strings.Join(plan.Video, " "), "strict-cbr") {
			t.Fatal(plan, err)
		}
		if encoder == core.VideoEncoderVP9 && (strings.Contains(strings.Join(plan.Video, " "), "-maxrate") || strings.Contains(strings.Join(plan.Video, " "), "-bufsize")) {
			t.Fatal(plan.Video)
		}
	}
}

// TestHDRConverterPreservesSignal проверяет сохранение CICP, диапазона и глубины 10 бит и явные отказы
// при PQ↔HLG, изменении диапазона и глубины.
func TestHDRConverterPreservesSignal(t *testing.T) {
	// source задаёт известный PQ10, target изменяет только десятибитные плоскости и размер.
	source := modernProfile(core.VideoEncoderX265, core.RateControlCRF).Format.Video
	source.PixelFormat, source.BitDepth = "yuv420p10le", 10
	source.Color = core.ColorInfo{Range: "limited", Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc"}
	target := source
	target.Width, target.Height, target.PixelFormat, target.Color.Range = 64, 64, "yuv444p10le", "tv"
	// filter/err проверяют согласованные CICP кодировщика на обоих концах zscale.
	filter, err := SoftwareVideoFilter(source, target, true)
	if err != nil || !strings.Contains(filter, "transferin=16:matrixin=9:rangein=limited:primaries=9:transfer=16:matrix=9:range=limited") || !strings.Contains(filter, "format=yuv444p10le") {
		t.Fatal(filter, err)
	}
	// mutate создаёт отдельное запрещённое преобразование target.
	for _, mutate := range []func(*core.VideoFormat){
		func(v *core.VideoFormat) { /* Меняет PQ на HLG. */ v.Color.Transfer = "arib-std-b67" },
		func(v *core.VideoFormat) { /* Требует HDR→SDR. */ v.Color.Transfer = "bt709" },
		func(v *core.VideoFormat) { /* Меняет диапазон значений. */ v.Color.Range = "full" },
		func(v *core.VideoFormat) { /* Теряет глубину сигнала. */
			v.BitDepth, v.PixelFormat = 8, "yuv444p"
		},
	} {
		// invalid сохраняет остальные HDR-поля без изменения source.
		invalid := target
		mutate(&invalid)
		if _, err = SoftwareVideoFilter(source, invalid, true); err == nil {
			t.Fatal(invalid)
		}
	}
	if _, err = SoftwareVideoFilter(source, target, false); err == nil {
		t.Fatal("размеры изменились при запрещённом rescale")
	}
}
