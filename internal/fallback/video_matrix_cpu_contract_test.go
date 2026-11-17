package fallback

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// videoCPUContract задаёт различимые сочетания кодировщика, плоскостей и управления качеством.
// Ожидаемое имя ffprobe задано независимо от преобразований имён в Prober.
type videoCPUContract struct {
	// name обозначает сочетание, видимое в отчёте последовательного прогона.
	name string
	// encoder выбирает настоящий программный кодировщик ProductionBuilder.
	encoder string
	// kind задаёт ожидаемый кодек результата.
	kind core.Codec
	// profile содержит публичное имя профиля, nativeProfile — буквальное имя ffprobe.
	profile, nativeProfile string
	// pixel задаёт ожидаемые реальные плоскости и глубину, а также формат контрольного декодирования.
	pixel string
	// rc выбирает отдельную нативную ветвь управления скоростью или качеством.
	rc string
	// quality задаёт целый QP либо сотые CRF; для битрейтных режимов равен нулю.
	quality int
	// transfer и rangeName задают независимые исходные и целевые цветовые параметры.
	transfer, rangeName string
}

// TestCPUVideoMatrixPreservesPreparedSignal проверяет различимые ветви программной подготовки.
// t владеет синтетическими файлами; каждый процесс имеет конечный срок, параллельных кодирований нет.
// Буквальный вход содержит 48 кадров и 76800 сэмплов на канал за 1,6 секунды.
// Проверяются фактические профиль, плоскости и CICP, HDR, сжатые AU, две декодированные петли и отрицательное сравнение цвета.
func TestCPUVideoMatrixPreservesPreparedSignal(t *testing.T) {
	// executable разрешает запуск только явно выбранного локального FFmpeg.
	executable := os.Getenv("GOST_MEDIA_FFMPEG")
	if executable == "" {
		t.Skip("CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	// runner ограничивает каждое нативное кодирование и измерение штатным тестовым сроком.
	runner := selfRunner()
	// prober использует настоящий ffprobe и существующие ограничения без их увеличения.
	prober := Prober{Binary: filepath.Join(filepath.Dir(executable), "ffprobe"), Runner: runner, Limits: selfProbeLimits()}
	// sources и observed удерживают только три собственных источника и независимо измеренные описания.
	sources := make(map[string]*os.File)
	observed := make(map[string]Source)
	// scenario выбирает одну существенно отличающуюся нативную ветвь; полного декартова произведения нет.
	for _, scenario := range []videoCPUContract{
		{"hevc-main-cbr", core.VideoEncoderX265, core.CodecHEVC, "main", "Main", "yuv420p", core.RateControlCBR, 0, "bt709", "limited"},
		{"hevc-main-vbr", core.VideoEncoderX265, core.CodecHEVC, "main", "Main", "yuv420p", core.RateControlVBR, 0, "bt709", "limited"},
		{"hevc-main-cqp", core.VideoEncoderX265, core.CodecHEVC, "main", "Main", "yuv420p", core.RateControlCQP, 23, "bt709", "limited"},
		{"hevc-main10-crf", core.VideoEncoderX265, core.CodecHEVC, "main10", "Main 10", "yuv420p10le", core.RateControlCRF, 2357, "bt709", "limited"},
		{"vp9-profile0-cbr", core.VideoEncoderVP9, core.CodecVP9, "profile0", "Profile 0", "yuv420p", core.RateControlCBR, 0, "bt709", "limited"},
		{"vp9-profile0-vbr", core.VideoEncoderVP9, core.CodecVP9, "profile0", "Profile 0", "yuv420p", core.RateControlVBR, 0, "bt709", "limited"},
		{"vp9-profile2-crf", core.VideoEncoderVP9, core.CodecVP9, "profile2", "Profile 2", "yuv420p10le", core.RateControlCRF, 2300, "bt709", "limited"},
		{"avc-high422-pq", core.VideoEncoderX264, core.CodecH264, "high422", "High 4:2:2", "yuv422p10le", core.RateControlCRF, 2357, "smpte2084", "limited"},
		{"avc-high444-pq", core.VideoEncoderX264, core.CodecH264, "high444", "High 4:4:4 Predictive", "yuv444p10le", core.RateControlCRF, 2357, "smpte2084", "limited"},
		{"av1-high-pq", core.VideoEncoderAOMAV1, core.CodecAV1, "high", "High", "yuv444p10le", core.RateControlCRF, 3500, "smpte2084", "limited"},
		{"av1-professional-pq", core.VideoEncoderAOMAV1, core.CodecAV1, "professional", "Professional", "yuv422p10le", core.RateControlCRF, 3500, "smpte2084", "limited"},
		{"av1-main-hlg-full", core.VideoEncoderSVTAV1, core.CodecAV1, "main", "Main", "yuv420p10le", core.RateControlCRF, 3500, "arib-std-b67", "full"},
		{"hevc-main10-hlg-full", core.VideoEncoderX265, core.CodecHEVC, "main10", "Main 10", "yuv420p10le", core.RateControlCRF, 2357, "arib-std-b67", "full"},
		{"vp9-profile2-hlg-full", core.VideoEncoderVP9, core.CodecVP9, "profile2", "Profile 2", "yuv420p10le", core.RateControlCRF, 2300, "arib-std-b67", "full"},
	} {
		// key объединяет только одинаковые исходные цвета, сохраняя раздельность SDR/PQ/HLG.
		key := scenario.transfer + "/" + scenario.rangeName
		if sources[key] == nil {
			// source и input создаются у внешнего t и остаются открытыми для всех сочетаний этих цветов.
			source, input := videoMatrixSource(t, executable, runner, prober, scenario.transfer, scenario.rangeName)
			sources[key], observed[key] = source, input
		}
		// Подтест t последовательно кодирует один профиль и освобождает его выходные файлы.
		t.Run(scenario.name, func(t *testing.T) {
			// input содержит измеренное описание источника; source открывается независимо для текущего сценария.
			input := observed[key]
			// source и err сохраняют собственный дескриптор, чтобы не делить позицию чтения с другими потребителями.
			source, err := os.Open(sources[key].Name())
			if err != nil {
				t.Fatal("открытие источника", err)
			}
			selfClose(t, source)
			// profile содержит полный текущий рецепт и буквальные параметры видео/AAC, без подстановки желаемого формата вместо наблюдения.
			profile := videoMatrixProfile(scenario)
			// loop должен совпасть с независимым исходным периодом, уже общим для 30 FPS и AAC 48 кГц.
			loop, err := EffectiveLoopDuration(input.Duration, profile, prober.Limits.MaxDuration)
			if err != nil || loop != 1600*time.Millisecond {
				t.Fatal("общий период", loop, err)
			}
			// command строится рабочим генератором без подмены кодировщика или фильтра.
			command, err := ProductionBuilder(executable)(GenerateRequest{Source: input, Profile: profile, LoopDuration: loop})
			if err != nil {
				t.Fatal("построение команды", err)
			}
			// raw и prepared содержат только результаты текущего сценария.
			raw, prepared := expandedCPUFile(t, "encoded.mp4"), expandedCPUFile(t, "prepared.mp4")
			// result и err сохраняют первый нативный отказ и конечную диагностику.
			result, err := runner.Run(context.Background(), command, expandedCPURewind(t, source), &boundedWriter{w: raw, remaining: 16 << 20})
			if err != nil {
				t.Fatal("кодирование", err, string(result.Diagnostics))
			}
			if err = prober.NormalizePreparedForSource(context.Background(), raw, prepared, profile, input); err != nil {
				t.Fatal("нормализация", err)
			}
			// actual устанавливается рабочей проверкой всех заголовков и AU фактического файла.
			actual, err := prober.Verify(context.Background(), prepared, profile)
			if err != nil {
				t.Fatal("проверка результата", actual, err)
			}
			if err = prober.VerifyLoopDuration(context.Background(), prepared, 1600*time.Millisecond); err != nil {
				t.Fatal("границы периода", err)
			}
			videoMatrixNativeFormat(t, prober, prepared, scenario)
			videoMatrixPackets(t, prober, raw, prepared, input.HDR, scenario.transfer != "bt709")
			videoMatrixDecode(t, executable, runner, prepared, scenario)
			// wrong меняет только одну цветовую ось корректного полного профиля: измеренный формат нельзя заменить желаемым.
			wrong := profile
			switch scenario.transfer {
			case "bt709":
				wrong.Format.Video.Color.Range = "full"
			case "smpte2084":
				wrong.Format.Video.Color.Transfer = "arib-std-b67"
			default:
				wrong.Format.Video.Color.Transfer = "smpte2084"
			}
			if wrong.Validate() != nil {
				t.Fatal("отрицательный профиль должен оставаться допустимым")
			}
			if _, err = prober.Verify(context.Background(), prepared, wrong); err == nil {
				t.Fatal("файл принят с чужой передаточной функцией или диапазоном")
			}
		})
	}
}

// videoMatrixProfile возвращает полный профиль scenario с точными единицами бит/с и бит.
// Чистое качество не содержит VBV; x265 CRF сохраняет сотые, VP9 CRF и QP остаются целыми.
func videoMatrixProfile(scenario videoCPUContract) core.GroupProfile {
	// profile содержит конечную геометрию 128×128/30 и стереофонический AAC LC 48 кГц.
	profile := core.GroupProfile{
		Mode: core.ModeTranscode, LiveAudioMode: core.LiveAudioCopy,
		Format: core.MediaFormat{
			Video: core.VideoFormat{Codec: scenario.kind, Width: 128, Height: 128, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: scenario.profile, PixelFormat: scenario.pixel, BitDepth: 10, Color: core.ColorInfo{Range: scenario.rangeName, Primaries: "bt2020", Transfer: scenario.transfer, Matrix: "bt2020nc"}},
			Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"},
		},
		VideoEncoding: core.VideoEncoding{GOPFrames: 15, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 64000},
	}
	if scenario.pixel == "yuv420p" {
		profile.Format.Video.BitDepth = 8
	}
	if scenario.transfer == "bt709" {
		profile.Format.Video.Color.Primaries, profile.Format.Video.Color.Matrix = "bt709", "bt709"
	}
	if scenario.kind == core.CodecAV1 {
		profile.Mode, profile.Format.Video.Tier = core.ModeCopy, "main"
	}
	profile.Encoding = core.DefaultEncoding(profile.Mode, scenario.encoder, scenario.rc)
	if scenario.encoder == core.VideoEncoderSVTAV1 {
		profile.Encoding.VideoCustom = "lp=1"
	}
	switch scenario.rc {
	case core.RateControlCQP:
		profile.Encoding.QP = core.OptionalQP{Set: true, Value: scenario.quality}
	case core.RateControlCRF:
		profile.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: scenario.quality}
	case core.RateControlCBR:
		profile.VideoEncoding.TargetBitrateBPS, profile.VideoEncoding.MaxBitrateBPS, profile.VideoEncoding.BufferBits = 100000, 100000, 200000
	case core.RateControlVBR:
		profile.VideoEncoding.TargetBitrateBPS = 100000
		if scenario.encoder == core.VideoEncoderX265 {
			profile.VideoEncoding.MaxBitrateBPS, profile.VideoEncoding.BufferBits = 200000, 200000
		}
	}
	return profile
}

// videoMatrixSource создаёт собственный HEVC Main10 MOV/MP4 с 48 одинаковыми цветными градиентами.
// t закрывает файлы; executable/runner/prober задают разрешённые локальные процессы и бюджеты.
// transfer/rangeName определяют действительные исходные CICP; SDR не получает MDCV/CLL.
func videoMatrixSource(t *testing.T, executable string, runner Runner, prober Prober, transfer, rangeName string) (*os.File, Source) {
	t.Helper()
	// raw и source содержат исходные плоскости и их стандартное кодированное представление.
	raw, source := expandedCPUFile(t, "source.yuv"), expandedCPUFile(t, "source.mp4")
	// frame хранит Y 128×128 и U/V 64×64 по два байта на десятибитное значение.
	frame := make([]byte, 128*128*3)
	// index выбирает отсчёт одной из трёх плоскостей.
	for index := 0; index < len(frame)/2; index++ {
		// value содержит постоянную U=256 либо V=768; яркость задаётся отдельным градиентом.
		value := uint16(256)
		if index >= 128*128+64*64 {
			value = 768
		} else if index < 128*128 {
			value = uint16(64 + index%128*7)
			if rangeName == "full" {
				value = uint16(index % 128 * 8)
			}
		}
		binary.LittleEndian.PutUint16(frame[index*2:], value)
	}
	// index задаёт один из 48 кадров продолжительностью ровно 1/30 секунды.
	for index := 0; index < 48; index++ {
		// err сообщает об отказе записи синтетических исходных пикселей.
		if _, err := raw.Write(frame); err != nil {
			t.Fatal(err)
		}
	}
	// primaries/matrix задают настоящий исходный охват, nativeRange — точный аргумент FFmpeg.
	primaries, matrix, nativeRange := "bt2020", "bt2020nc", "tv"
	if transfer == "bt709" {
		primaries, matrix = "bt709", "bt709"
	}
	if rangeName == "full" {
		nativeRange = "pc"
	}
	// params задаёт закрытые IDR и действительные HDR SEI только у HDR-источника.
	params := "open-gop=0:scenecut=0:bframes=0:ref=1"
	if transfer != "bt709" {
		params += ":master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1):max-cll=1000,400"
	}
	// filter назначает цвет непосредственно исходным AVFrame: одних выходных тегов для этого источника недостаточно.
	filter := "setparams=range=" + rangeName + ":color_primaries=" + primaries + ":color_trc=" + transfer + ":colorspace=" + matrix
	// command кодирует источник отдельно от проверяемого генератора; QP8 сохраняет узнаваемые значения градиента.
	command := Command{Binary: executable, Args: []string{"-v", "error", "-nostdin", "-xerror", "-f", "rawvideo", "-pixel_format", "yuv420p10le", "-video_size", "128x128", "-framerate", "30", "-fd", "3", "-i", "fd:", "-an", "-vf", filter, "-c:v", "libx265", "-preset", "medium", "-profile:v", "main10", "-qp", "8", "-bf", "0", "-refs", "1", "-g", "15", "-x265-params", params, "-color_range", nativeRange, "-color_primaries", primaries, "-color_trc", transfer, "-colorspace", matrix, "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "pipe:1"}}
	// result/err сохраняют исходную диагностику и отказ создания источника.
	result, err := runner.Run(context.Background(), command, expandedCPURewind(t, raw), &boundedWriter{w: source, remaining: 8 << 20})
	if err != nil {
		t.Fatal("создание источника", err, string(result.Diagnostics))
	}
	// input измеряется рабочим портом; его цвета независимо заданы аргументами и исходными плоскостями.
	input, err := prober.Source(context.Background(), source, core.AssetVideo)
	if err != nil || input.Format.Video == nil || input.Duration != 1600*time.Millisecond || input.Format.Video.BitDepth != 10 || input.Format.Video.PixelFormat != "yuv420p10le" || input.Format.Video.Color != (core.ColorInfo{Range: rangeName, Primaries: primaries, Transfer: transfer, Matrix: matrix}) {
		t.Fatalf("измерение исходника: source=%+v, video=%+v, err=%v", input, input.Format.Video, err)
	}
	if transfer != "bt709" {
		input, err = prober.SourceHDR(context.Background(), source, input)
		if err != nil || !input.HDRVerified || input.HDR.MDCV.Source != media.HDRSourceHEVCSEI || input.HDR.CLL.Source != media.HDRSourceHEVCSEI {
			t.Fatal("исходные SEI", input.HDR, err)
		}
		videoMatrixHDRValues(t, input.HDR)
	}
	return source, input
}

// videoMatrixNativeFormat сверяет сырой JSON ffprobe файла file с буквальным контрактом scenario.
// t сообщает расхождение; prober предоставляет только ограниченный запуск и разбор JSON без video()-проекции.
func videoMatrixNativeFormat(t *testing.T, prober Prober, file *os.File, scenario videoCPUContract) {
	t.Helper()
	// document/err содержат независимые поля ffprobe, не преобразованные в MediaFormat.
	document, err := prober.inspect(context.Background(), file)
	if err != nil || len(document.Streams) != 2 {
		t.Fatal("ffprobe", err, document)
	}
	// count требует ровно одну дорожку каждого вида.
	count := map[string]int{}
	// stream обозначает одну фактически прочитанную дорожку.
	for _, stream := range document.Streams {
		count[stream.Type]++
		if stream.Type == "audio" {
			if stream.Codec != "aac" || stream.Profile != "LC" || stream.SampleRate != "48000" || stream.Channels != 2 || stream.Layout != "stereo" {
				t.Fatal("фактический AAC", stream)
			}
			continue
		}
		// nativeRange и primaries/matrix задают буквальное написание ffprobe для выбранного сигнала.
		nativeRange, primaries, matrix := "tv", "bt2020", "bt2020nc"
		if scenario.rangeName == "full" {
			nativeRange = "pc"
		}
		if scenario.transfer == "bt709" {
			primaries, matrix = "bt709", "bt709"
		}
		if stream.Type != "video" || stream.Codec != string(scenario.kind) || stream.Profile != scenario.nativeProfile || stream.Pixel != scenario.pixel || stream.Width != 128 || stream.Height != 128 || stream.Rate != "30/1" || stream.ColorRange != nativeRange || stream.Primaries != primaries || stream.Transfer != scenario.transfer || stream.Matrix != matrix {
			t.Fatal("фактическое видео", stream)
		}
	}
	if count["video"] != 1 || count["audio"] != 1 {
		t.Fatal("состав дорожек", count)
	}
}

// videoMatrixHDRValues проверяет буквальные числа SEI и их заранее вычисленные проекции UQ.
// t сообщает точный компонент; metadata может происходить из кодека либо контейнера, но не из желаемого профиля.
func videoMatrixHDRValues(t *testing.T, metadata media.HDRMetadata) {
	t.Helper()
	if !metadata.MDCV.Present || !metadata.CLL.Present {
		t.Fatal("потеря исходных MDCV/CLL", metadata)
	}
	// decimal содержит RGBxy, white xy и яркости исходного SEI; binaryGrid — независимые округлённые UQ-коды.
	decimal := [10]uint64{34000, 16000, 13250, 34500, 7500, 3000, 15635, 16450, 10000000, 1}
	binaryGrid := [10]uint64{44564, 20972, 17367, 45220, 9830, 3932, 20493, 21561, 256000, 2}
	// index и value выбирают конкретный измеренный компонент; ожидаемое значение не вычисляется рабочим вспомогательным кодом.
	for index, value := range metadata.MDCV.Values() {
		// want и grid задают точные числа исходного SEI либо направленной двоичной проекции.
		want, grid := decimal[index], media.HDRGridSEIChromaticity
		if index >= 8 {
			grid = media.HDRGridSEILuminance
		}
		if value.Grid == media.HDRGridBinaryChromaticity || value.Grid == media.HDRGridBinaryMaxLuminance || value.Grid == media.HDRGridBinaryMinLuminance {
			want, grid = binaryGrid[index], media.HDRGridBinaryChromaticity
			switch index {
			case 8:
				grid = media.HDRGridBinaryMaxLuminance
			case 9:
				grid = media.HDRGridBinaryMinLuminance
			}
		}
		if value != (media.HDRValue{Raw: want, Grid: grid, Known: true}) {
			t.Fatalf("компонент MDCV[%d]=%+v, ожидалось %d в сетке %d", index, value, want, grid)
		}
	}
	if metadata.CLL.MaxCLL != (media.HDRValue{Raw: 1000, Grid: media.HDRGridInteger, Known: true}) || metadata.CLL.MaxFALL != (media.HDRValue{Raw: 400, Grid: media.HDRGridInteger, Known: true}) {
		t.Fatal("исходные CLL изменились", metadata.CLL)
	}
}

// videoMatrixPackets проверяет сохранность всех сжатых AU raw после нормализации prepared.
// prober измеряет исходные дорожки; source разрешает только свежую HDR-нормализацию при hdr=true.
// Повторное открытие готового файла использует стандартные заголовки и полный Inspector без source-подстановки.
func videoMatrixPackets(t *testing.T, prober Prober, raw, prepared *os.File, source media.HDRMetadata, hdr bool) {
	t.Helper()
	// before/after содержат исходные AU и результат нормализации с реальными временами.
	before := modernCPUReadPackets(t, prober, raw, &source)
	after := modernCPUReadPackets(t, prober, prepared, nil)
	// index и packet находят единственную первую AAC AU задержки, проверенную производственной нормализацией.
	for index, packet := range before {
		if packet.Config().Info().Track == media.TrackAudio {
			before = append(before[:index], before[index+1:]...)
			break
		}
	}
	if len(before) != 123 || len(after) != 123 {
		t.Fatal("ожидались 48 видео и 75 AAC AU", len(before), len(after))
	}
	// inspector восстанавливает только действительные внутрипоточные сведения и проверяет все независимые старты.
	var inspector codec.Inspector
	// videoCount/audioCount задают независимые ожидаемые числа AU за один период.
	videoCount, audioCount := 0, 0
	// index и packet связывают одну и ту же неизменную AU по порядку сериализации.
	for index, packet := range after {
		if packet.Info() != before[index].Info() || packet.Config().Info().TimeBase != before[index].Config().Info().TimeBase || packet.Config().Info().Track != before[index].Config().Info().Track || !bytes.Equal(packet.Payload(), before[index].Payload()) {
			t.Fatalf("AU[%d] или её временные метки изменились", index)
		}
		if packet.Config().Info().Track == media.TrackAudio {
			audioCount++
			continue
		}
		// key и inband устанавливаются по реальному сжатому потоку, err сохраняет его первый отказ.
		key, inband, err := inspector.InspectMetadata(packet)
		if err != nil || key != (videoCount%15 == 0) {
			t.Fatal("независимый старт или фиксированный GOP", videoCount, key, err)
		}
		videoCount++
		// stored содержит восстановленные стандартные сведения, отдельно от действительной сигнализации SEI/OBU.
		stored := packet.Config().Info().HDR
		if !hdr {
			if stored.MDCV.Present || stored.CLL.Present || inband.MDCV.Present || inband.CLL.Present {
				t.Fatal("SDR получил выдуманные MDCV/CLL", stored, inband)
			}
			continue
		}
		videoMatrixHDRValues(t, stored)
		// headerMetadata содержит только сообщения фактической конфигурации кодека, не блоки стандартного контейнера.
		headerMetadata, err := codec.HDRFromConfig(packet.Config().Info().Video.Codec, packet.Config().Headers())
		if err != nil {
			t.Fatal("метаданные конфигурации кодека", err)
		}
		// expectedSource разрешает SEI/OBU происхождение конфигурации лишь при наличии этих сообщений в headers.
		// SEI отдельного AU проверяется независимо: его присутствие не меняет происхождение init-дескриптора.
		expectedSource := media.HDRSourceContainer
		if headerMetadata.MDCV.Present {
			expectedSource = headerMetadata.MDCV.Source
			if expectedSource != media.HDRSourceH264SEI && expectedSource != media.HDRSourceHEVCSEI && expectedSource != media.HDRSourceAV1OBU {
				t.Fatal("неверное происхождение сжатого MDCV", inband)
			}
		}
		if stored.MDCV.Source != expectedSource {
			t.Fatal("MDCV не подтверждён своим каналом", stored, inband)
		}
		expectedSource = media.HDRSourceContainer
		if headerMetadata.CLL.Present {
			expectedSource = headerMetadata.CLL.Source
		}
		if stored.CLL.Source != expectedSource {
			t.Fatal("CLL не подтверждён своим каналом", stored, inband)
		}
		if inband.MDCV.Present && inband.CLL.Present {
			videoMatrixHDRValues(t, inband)
		}
	}
	if videoCount != 48 || audioCount != 75 {
		t.Fatal("неполный период AU", videoCount, audioCount)
	}
	// reopened/err открывают отдельный дескриптор готового артефакта, без желаемого профиля или исходного снимка HDR.
	reopened, err := os.Open(prepared.Name())
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, reopened)
	// demux восстанавливает фактический формат из собственного стандартного контейнера, включая UUID-путь.
	demux, err := fmp4.NewDemuxer(reopened, fmp4.DemuxOptions{Limits: prober.Limits.Container})
	if err != nil {
		t.Fatal("повторное открытие", err)
	}
	if hdr {
		videoMatrixHDRValues(t, demux.Init().Video.Info().HDR)
	}
}

// videoMatrixDecode декодирует две петли file настоящим FFmpeg executable через ограниченный runner.
// scenario задаёт исходные отсчёты и число плоскостей; t проверяет 96 кадров и цифровую AAC-тишину.
func videoMatrixDecode(t *testing.T, executable string, runner Runner, file *os.File, scenario videoCPUContract) {
	t.Helper()
	// video содержит реальные декодированные отсчёты без подмены профиля или цвета.
	var video bytes.Buffer
	// command повторяет собственный файл ровно два раза; выходной pixel format уже отдельно проверен ffprobe.
	command := Command{Binary: executable, Args: []string{"-v", "error", "-nostdin", "-xerror", "-stream_loop", "1", "-fd", "3", "-i", "fd:", "-map", "0:v:0", "-pix_fmt", scenario.pixel, "-f", "rawvideo", "pipe:1"}}
	// result/err сохраняют полный исход декодирования обеих петель.
	result, err := runner.Run(context.Background(), command, expandedCPURewind(t, file), &boundedWriter{w: &video, remaining: 16 << 20})
	if err != nil {
		t.Fatal("декодирование видео", err, string(result.Diagnostics))
	}
	// bytesPerSample и chromaSamples задают реальные размеры плоскостей для разрешённых форматов.
	bytesPerSample, chromaSamples := 2, 64*64
	switch scenario.pixel {
	case "yuv420p":
		bytesPerSample = 1
	case "yuv422p10le":
		chromaSamples = 64 * 128
	case "yuv444p10le":
		chromaSamples = 128 * 128
	}
	// frameBytes задаёт число байтов одного полного кадра всех плоскостей.
	frameBytes := (128*128 + chromaSamples*2) * bytesPerSample
	if video.Len() != frameBytes*96 {
		t.Fatal("ожидались 96 полных декодированных кадров", video.Len(), frameBytes*96)
	}
	// expected содержит исходные Y первого/последнего столбца, U и V; tolerance учитывает сжатие с потерями.
	expected, tolerance := [4]int{64, 953, 256, 768}, 24
	if scenario.rangeName == "full" {
		expected[0], expected[1] = 0, 1016
	}
	if bytesPerSample == 1 {
		expected, tolerance = [4]int{16, 238, 64, 192}, 6
	}
	// frame выбирает каждый кадр, включая непосредственно обе стороны границы повторения 47→48.
	for frame := 0; frame < 96; frame++ {
		// component и offset выбирают по одному исходному отсчёту Y/Y/U/V без пространственного преобразования.
		for component, offset := range [4]int{0, 127, 128 * 128, 128*128 + chromaSamples} {
			// position задаёт байтовое смещение; actual содержит фактически декодированное значение.
			position := frame*frameBytes + offset*bytesPerSample
			actual := int(video.Bytes()[position])
			if bytesPerSample == 2 {
				actual = int(binary.LittleEndian.Uint16(video.Bytes()[position:]))
			}
			if actual < expected[component]-tolerance || actual > expected[component]+tolerance {
				t.Fatalf("кадр %d, компонент %d: %d, ожидалось %d ± %d", frame, component, actual, expected[component], tolerance)
			}
		}
	}
	// pcm принимает две полные стереопетли по 76800 сэмплов на канал, без переназначения карты или частоты.
	var pcm bytes.Buffer
	command.Args = []string{"-v", "error", "-nostdin", "-xerror", "-strict", "2", "-stream_loop", "1", "-fd", "3", "-i", "fd:", "-map", "0:a:0", "-vn", "-c:a", "pcm_s16le", "-f", "s16le", "pipe:1"}
	result, err = runner.Run(context.Background(), command, expandedCPURewind(t, file), &boundedWriter{w: &pcm, remaining: 1 << 20})
	if err != nil || pcm.Len() != 614400 || bytes.Count(pcm.Bytes(), []byte{0}) != pcm.Len() {
		t.Fatal("две AAC-петли должны содержать 614400 нулевых байтов PCM", pcm.Len(), err, string(result.Diagnostics))
	}
}
