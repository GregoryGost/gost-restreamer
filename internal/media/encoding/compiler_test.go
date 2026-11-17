package encoding

import (
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Проверяет точные режимы качества SVT: чистый CQP/CRF не получает скрытых ограничений битрейта,
// сохраняет независимые AU и передаёт выбранные AQ, tuning и lookahead без изменения профиля.
// t сверяет параметры кодировщика с выбранным режимом качества.
func TestCompilerSVTQualityArguments(t *testing.T) {
	// scenario задаёт допустимую комбинацию режима качества, tuning и его числового значения SVT.
	for _, scenario := range []struct {
		// rc выбирает CQP либо CRF. tuning задаёт публичное имя настройки. tune задаёт числовой код SVT.
		rc, tuning, tune string
	}{
		{"cqp", "psnr", "1"}, {"crf", "psnr", "1"}, {"crf", "vq", "0"},
		{"crf", "ssim", "2"}, {"crf", "ms_ssim", "4"}, {"crf", "vmaf", "5"},
	} {
		// value выбирает обе крайние разрешённые настройки качества.
		for _, value := range []int{1, 63} {
			// t сообщает об ошибках проверяемого сценария.
			t.Run(scenario.rc+"/"+scenario.tuning+"/"+strconv.Itoa(value), func(t *testing.T) {
				// Компилирует крайнее качество выбранного режима и проверяет его argv и независимость AU.
				// p содержит отдельный профиль нового AV1-рецепта без битрейта или VBV.
				p := compilerProfile(core.CodecAV1)
				p.LiveAudioMode = core.LiveAudioCopy
				p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderSVTAV1, scenario.rc)
				p.Encoding.SVTAV1.Tuning = scenario.tuning
				p.Encoding.SVTAV1.LookaheadFrames = 6
				p.VideoEncoding.TargetBitrateBPS, p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 0, 0, 0
				// flag выбирает единственный публичный параметр качества FFmpeg. aq задаёт ожидаемый AQ.
				flag, aq := "-crf", "2"
				if scenario.rc == core.RateControlCQP {
					p.Encoding.QP = core.OptionalQP{Set: true, Value: value}
					flag, aq = "-qp", "0"
				} else {
					p.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: value * 100}
				}
				// before сохраняет профиль для проверки отсутствия неявной материализации.
				before := p
				// plan содержит скомпилированные кодеки; err сообщает о нарушении нового контракта.
				plan, err := Compile(p, Prepared)
				if err != nil {
					t.Fatal(err)
				}
				// video объединяет аргументы для поиска обязательных параметров.
				video := strings.Join(plan.Video, " ")
				// want содержит очередной обязательный параметр качества или независимости AU.
				for _, want := range []string{flag + " " + strconv.Itoa(value), "pred-struct=2", "scd=0", "enable-overlays=0", "enable-dg=0", "hierarchical-levels=0", "enable-tf=0", "aq-mode=" + aq, "tune=" + scenario.tune, "lookahead=6"} {
					if !strings.Contains(video, want) {
						t.Fatal(want, video)
					}
				}
				// forbidden перечисляет целевые ограничения скорости, недопустимые в чистом режиме качества.
				for _, forbidden := range []string{"-b:v", "-maxrate", "-bufsize", "rc=", "mbr="} {
					if strings.Contains(video, forbidden) {
						t.Fatal(forbidden, video)
					}
				}
				if p != before {
					t.Fatal("компиляция изменила профиль")
				}
			})
		}
	}
}

// Проверяет отключение PCE только для кодирования обычной 7.1: LIVE copy сохраняет исходные байты,
// а подготовка и LIVE transcode используют один и тот же точный рецепт AAC.
// t проверяет равенство собственных рецептов и неизменность LIVE copy.
func TestCompilerSevenChannelPairsDisablePCEWithoutChangingLiveCopy(t *testing.T) {
	// p содержит новый H.264-профиль с целевой обычной 7.1 и явным кодированием LIVE-аудио.
	p := compilerProfile(core.CodecH264)
	p.LiveAudioMode = core.LiveAudioTranscode
	p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderNVENC, core.RateControlVBR)
	p.Format.Audio.Channels, p.Format.Audio.ChannelLayout = 8, "7.1"
	// prepared содержит рецепт AAC-тишины; err сообщает об отказе его компиляции.
	prepared, err := Compile(p, Prepared)
	if err != nil {
		t.Fatal(err)
	}
	// live содержит рецепт явного перекодирования LIVE в тот же AAC.
	live, err := Compile(p, Live)
	if err != nil || !reflect.DeepEqual(prepared.Audio, live.Audio) || !strings.Contains(strings.Join(live.Audio, " "), "-aac_pce 0") {
		t.Fatal(prepared, live, err)
	}
	p.LiveAudioMode = core.LiveAudioCopy
	live, err = Compile(p, Live)
	if err != nil || !reflect.DeepEqual(live.Audio, []string{"-c:a", "copy"}) {
		t.Fatal(live, err)
	}
}

// compilerProfile создаёт полный профиль codec для H.264 transcode либо AV1 copy с размером 128×128/30.
func compilerProfile(codec core.Codec) core.GroupProfile {
	// p содержит базовый тестовый профиль, который адаптируется к выбранному H.264 или AV1.
	p := core.GroupProfile{Mode: core.ModeTranscode, Format: core.MediaFormat{Video: core.VideoFormat{Codec: codec, Width: 128, Height: 128, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: "high", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 100000, MaxBitrateBPS: 200000, BufferBits: 200000, GOPFrames: 15, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 64000}}
	if codec == core.CodecAV1 {
		p.VideoEncoding.MaxBitrateBPS = p.VideoEncoding.TargetBitrateBPS
		p.Mode = core.ModeCopy
		p.Format.Video.CodecProfile = "main"
		p.Format.Video.Tier = "main"
	}
	p.LiveAudioMode = core.LiveAudioCopy
	if codec == core.CodecAV1 {
		p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderSVTAV1, core.RateControlCBR)
	} else {
		p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderNVENC, core.RateControlVBR)
	}
	return p
}

// Проверяет передачу явных CBR/VBR, NVENC/SVT/custom и аудиопараметров без изменения профиля; для AV1
// VBR ограничения maxrate и bufsize отсутствуют.
func TestCompilerExplicitRateControlAndEveryEncoderOption(t *testing.T) {
	// codec выбирает проверяемый видеокодек.
	for _, codec := range []core.Codec{core.CodecH264, core.CodecAV1} {
		// rc выбирает явный режим битрейта cbr или vbr.
		for _, rc := range []string{"cbr", "vbr"} {
			t.Run(string(codec)+"/"+rc, func(t *testing.T) {
				// Для выбранных кодека и режима битрейта проверяет параметры видео и аудио, различия LIVE и подготовки
				// и неизменность исходного профиля.

				// p содержит отдельный профиль текущего сочетания кодека и режима
				// битрейта.
				p := compilerProfile(codec)
				if codec == core.CodecH264 {
					p.LiveAudioMode = core.LiveAudioTranscode
				}
				// encoder содержит имя кодировщика, соответствующее проверяемому кодеку.
				encoder := "h264_nvenc"
				if codec == core.CodecAV1 {
					encoder = "libsvtav1"
					if rc == "vbr" {
						p.VideoEncoding.MaxBitrateBPS = 0
						p.VideoEncoding.BufferBits = 0
					}
				} else if rc == "cbr" {
					p.VideoEncoding.MaxBitrateBPS = p.VideoEncoding.TargetBitrateBPS
				}
				p.Encoding = core.DefaultEncoding(p.Mode, encoder, rc)
				p.Encoding.AudioCustom = "aac_coder=fast"
				if codec == core.CodecH264 {
					p.Encoding.NVENC = core.NVENCSettings{Preset: "p6", Tuning: "ll", Multipass: "fullres", LookaheadFrames: 12, EnableSpatialAQ: true, EnableTemporalAQ: true, AQStrength: 9}
					p.Encoding.VideoCustom = "nonref_p=1 strict_gop=1"
				} else {
					p.Encoding.SVTAV1.Preset = 10
					p.Encoding.VideoCustom = "lp=2"
					if rc == "vbr" {
						p.Encoding.SVTAV1.Tuning = "vq"
						p.Encoding.SVTAV1.LookaheadFrames = 6
					}
				}
				// before сохраняет профиль до компиляции для проверки его неизменности.
				before := p
				// prepared содержит аргументы файловой подготовки. err сообщает об
				// отказе их построения.
				prepared, err := Compile(p, Prepared)
				if err != nil {
					t.Fatal(err)
				}
				// audio содержит объединённые аудиоаргументы для проверки
				// пользовательского параметра AAC.
				audio := strings.Join(prepared.Audio, " ")
				if !strings.Contains(audio, "-aac_coder fast") {
					t.Fatal(audio)
				}
				// video содержит объединённые видеоаргументы для проверки параметров
				// выбранного кодировщика.
				video := strings.Join(prepared.Video, " ")
				if codec == core.CodecH264 {
					// want содержит очередной обязательный фрагмент аргументов
					// NVENC.
					for _, want := range []string{"-rc " + rc, "-preset p6", "-tune ll", "-multipass fullres", "-rc-lookahead 12", "-spatial-aq 1", "-temporal-aq 1", "-aq-strength 9", "-nonref_p 1", "-strict_gop 1"} {
						if !strings.Contains(video, want) {
							t.Fatal(want, video)
						}
					}
					// live содержит аргументы текущего эфира H.264. e сообщает об
					// отказе их построения.
					live, e := Compile(p, Live)
					if e != nil || !reflect.DeepEqual(live, prepared) {
						t.Fatal(live, prepared, e)
					}
				} else {
					// params содержит ожидаемое сочетание режима битрейта и
					// структуры предсказания SVT-AV1.
					params := "rc=2:mbr=0:pred-struct=1"
					if rc == "vbr" {
						params = "rc=1:mbr=0:pred-struct=2"
					}
					// want содержит очередной обязательный фрагмент аргументов
					// SVT-AV1.
					for _, want := range []string{params, "hierarchical-levels=0", "scd=0", "enable-tf=0", "enable-dg=0", "enable-overlays=0", "aq-mode=2", "lp=2", "-preset 10"} {
						if !strings.Contains(video, want) {
							t.Fatal(want, video)
						}
					}
					if rc == "vbr" && (strings.Contains(video, "-maxrate") || strings.Contains(video, "-bufsize") || !strings.Contains(video, "tune=0:lookahead=6")) {
						t.Fatal(video)
					}
					if rc == "cbr" && !strings.Contains(video, "tune=1:lookahead=0") {
						t.Fatal(video)
					}
				}
				if p != before {
					t.Fatal("compiler изменил профиль")
				}
			})
		}
	}
}

// TestCompilerRejectsMissingSettingsAndKeepsAutoLookahead проверяет явную текущую модель,
// отсутствие override при auto lookahead и ранний отказ неверного target или конфликтующего custom.
// t сообщает о нарушении входных ограничений либо аргументов SVT.
func TestCompilerRejectsMissingSettingsAndKeepsAutoLookahead(t *testing.T) {
	// p задаёт полный AV1 copy с VBR без VBV и без явного lookahead.
	p := compilerProfile(core.CodecAV1)
	p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderSVTAV1, core.RateControlVBR)
	p.Encoding.SVTAV1.LookaheadFrames = -1
	p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 0, 0
	// prepared содержит аргументы подготовки; err сохраняет отказ компиляции.
	prepared, err := Compile(p, Prepared)
	if err != nil || strings.Contains(strings.Join(prepared.Video, " "), "lookahead=") {
		t.Fatal(prepared, err)
	}
	if _, err = Compile(p, Live); err == nil {
		t.Fatal("copy запустил encoder LIVE")
	}
	if _, err = Compile(p, 0); err == nil {
		t.Fatal("неизвестная цель принята")
	}
	p.Encoding.VideoCustom = "rc=1"
	if _, err = Compile(p, Prepared); err == nil {
		t.Fatal("конфликт прошёл compiler")
	}
	p.Encoding = core.EncodingSettings{}
	if _, err = Compile(p, Prepared); err == nil {
		t.Fatal("нулевые настройки получили исполнимый рецепт")
	}
}

// Проверяет, что LIVE copy выдаёт только -c:a copy для аудио, сохраняя видеопараметры и прежний рецепт
// кодирования подготовленной тишины.
func TestCompilerCopiesLiveAudioWithoutChangingPreparedEncoding(t *testing.T) {
	// p содержит профиль H.264 для сравнения режимов копирования и кодирования LIVE-аудио.
	p := compilerProfile(core.CodecH264)
	p.Encoding = core.DefaultEncoding(p.Mode, "h264_nvenc", "vbr")
	p.Encoding.AudioCustom = "aac_coder=fast"
	p.LiveAudioMode = core.LiveAudioTranscode
	// encoded содержит аргументы эфира с кодированием AAC. err сообщает об отказе компиляции.
	encoded, err := Compile(p, Live)
	if err != nil {
		t.Fatal(err)
	}
	// prepared содержит исходный рецепт подготовки AAC-тишины.
	prepared, err := Compile(p, Prepared)
	if err != nil {
		t.Fatal(err)
	}
	p.LiveAudioMode = core.LiveAudioCopy
	// live содержит аргументы эфира после переключения LIVE-аудио в copy.
	live, err := Compile(p, Live)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(live.Audio, []string{"-c:a", "copy"}) || !reflect.DeepEqual(live.Video, encoded.Video) {
		t.Fatal(live, encoded)
	}
	// silent содержит повторно построенный рецепт подготовки для проверки его неизменности при смене
	// LIVE-аудио.
	silent, err := Compile(p, Prepared)
	if err != nil || !reflect.DeepEqual(silent, prepared) {
		t.Fatal(silent, prepared, err)
	}
	if !reflect.DeepEqual(encoded.Audio, prepared.Audio) || !strings.Contains(strings.Join(encoded.Audio, " "), "-aac_coder fast") {
		t.Fatal(encoded, prepared)
	}
}
