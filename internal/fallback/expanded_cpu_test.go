package fallback

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestCPUSoftwarePreparedProfiles проверяет новые рецепты через настоящий ProductionBuilder,
// нормализацию, Inspector и полный период обеих дорожек. Ни один кодировщик не подменяется тестом.
// Все кодеки и плоскости проходят настоящее декодирование; собственная AAC 7.1 проверяется на трёх петлях
// по числу сэмплов и цифровым нулям, а не по одному сообщённому имени layout.
// t сообщает об ошибках проверяемого сценария.
func TestCPUSoftwarePreparedProfiles(t *testing.T) {
	// binary включает разрешённые локальные процессы; runner ограничивает время и вывод.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	runner := selfRunner()
	// prober использует реальный ffprobe с теми же ограничениями и декодерами приложения.
	prober := Prober{Binary: filepath.Join(filepath.Dir(binary), "ffprobe"), Runner: runner, Limits: selfProbeLimits()}
	// source содержит независимое входное H.264 с явным фактическим цветом и нулевым началом.
	source := expandedCPUFile(t, "source.mp4")
	// sourceCommand создаёт только тестовый вход, не заменяя проверяемый рабочий генератор.
	sourceCommand := Command{Binary: binary, Args: []string{"-v", "error", "-nostdin", "-xerror", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=30:duration=1.6", "-an", "-vf", "setparams=range=limited:color_primaries=bt709:color_trc=bt709:colorspace=bt709", "-c:v", "libx264", "-preset", "ultrafast", "-bf:v", "0", "-color_range:v", "tv", "-color_primaries:v", "bt709", "-color_trc:v", "bt709", "-colorspace:v", "bt709", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "pipe:1"}}
	// result содержит конечную диагностику создания источника; err сообщает об отказе процесса.
	result, err := runner.Run(context.Background(), sourceCommand, nil, source)
	if err != nil {
		t.Fatal(err, result.Diagnostics)
	}
	// input содержит измеренный источник с подтверждёнными CICP, форматом пикселей и глубиной.
	input, err := prober.Source(context.Background(), source, core.AssetVideo)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("исходное видео: %+v", input.Format.Video)
	// scenario выбирает encoder/profile/pixel кодировщика, rc и качество; tier добавляет SVT high.
	for _, scenario := range []struct {
		// encoder, profile и pixel задают проверяемое видеокодирование без подмены формата.
		encoder, profile, pixel string
		// rc задаёт регулирование, tier — необязательный high, quality — QP либо сотые CRF.
		rc, tier string
		// level задаёт явный уровень H.264 кодировщика либо пустой автоматический выбор.
		level   string
		quality int
	}{
		{core.VideoEncoderX264, "constrained-baseline", "yuv420p", "crf", "", "", 2357},
		{core.VideoEncoderX264, "main", "yuv420p", "vbr", "", "", 0},
		{core.VideoEncoderX264, "high", "yuv420p", "cbr", "", "", 0},
		{core.VideoEncoderX264, "high10", "yuv420p10le", "crf", "", "", 5100},
		{core.VideoEncoderX264, "high422", "yuv422p", "crf", "", "", 2357},
		{core.VideoEncoderX264, "high422", "yuv422p10le", "crf", "", "", 2357},
		{core.VideoEncoderX264, "high444", "yuv444p", "cqp", "", "", 0},
		{core.VideoEncoderX264, "high444", "yuv444p10le", "crf", "", "", 0},
		{core.VideoEncoderAOMAV1, "main", "yuv420p", "cbr", "", "", 0},
		{core.VideoEncoderAOMAV1, "main", "yuv420p10le", "vbr", "", "", 0},
		{core.VideoEncoderAOMAV1, "main", "gray", "crf", "", "", 0},
		{core.VideoEncoderAOMAV1, "high", "yuv444p", "crf", "", "", 3500},
		{core.VideoEncoderAOMAV1, "high", "yuv444p10le", "crf", "", "", 3500},
		{core.VideoEncoderAOMAV1, "professional", "yuv422p", "crf", "", "", 6300},
		{core.VideoEncoderAOMAV1, "professional", "yuv422p10le", "crf", "", "", 3500},
		{core.VideoEncoderSVTAV1, "main", "yuv420p10le", "cbr", "high", "", 0},
		{core.VideoEncoderSVTAV1, "main", "yuv420p", "crf", "", "", 3500},
		{core.VideoEncoderX264, "high", "yuv420p", "vbr", "", "6.0", 0},
		{core.VideoEncoderX264, "high", "yuv420p", "vbr", "", "6.1", 0},
		{core.VideoEncoderX264, "high", "yuv420p", "vbr", "", "6.2", 0},
	} {
		// t сообщает об ошибках проверяемого сценария.
		t.Run(fmt.Sprintf("%s/%s/%s/%s/%s", scenario.encoder, scenario.profile, scenario.pixel, scenario.rc, scenario.level), func(t *testing.T) {
			// Готовит один реальный файл и проверяет байты AU, формат, обе временные сетки и декодирование.
			// p задаёт весь материализованный рецепт, codecName — единственный соответствующий кодек.
			codecName := core.CodecH264
			if scenario.encoder != core.VideoEncoderX264 {
				codecName = core.CodecAV1
			}
			p := selfProfile(codecName)
			p.LiveAudioMode = core.LiveAudioCopy
			p.Encoding = core.DefaultEncoding(p.Mode, scenario.encoder, scenario.rc)
			p.Format.Video.CodecProfile, p.Format.Video.PixelFormat = scenario.profile, scenario.pixel
			p.Format.Video.Level = scenario.level
			if scenario.encoder == core.VideoEncoderSVTAV1 {
				p.Encoding.VideoCustom = "lp=1"
			}
			if scenario.pixel == "yuv420p10le" || scenario.pixel == "yuv422p10le" || scenario.pixel == "yuv444p10le" {
				p.Format.Video.BitDepth = 10
			}
			if scenario.rc == core.RateControlCQP || scenario.rc == core.RateControlCRF {
				p.VideoEncoding.TargetBitrateBPS, p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 0, 0, 0
				if scenario.rc == core.RateControlCQP {
					p.Encoding.QP = core.OptionalQP{Set: true, Value: scenario.quality}
				} else {
					p.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: scenario.quality}
				}
			} else if scenario.rc == core.RateControlVBR && codecName == core.CodecAV1 {
				p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 0, 0
			} else if scenario.rc == core.RateControlCBR {
				p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = p.VideoEncoding.TargetBitrateBPS, p.VideoEncoding.TargetBitrateBPS
			}
			if scenario.tier == "high" {
				p.Format.Video.Level, p.Format.Video.Tier = "4.0", "high"
				p.Encoding.VideoCustom = "lp=1"
			}
			if scenario.pixel == "gray" || scenario.pixel == "yuv444p10le" {
				p.Format.Video.Color = core.ColorInfo{Range: "full", Primaries: "bt2020", Transfer: "gamma22", Matrix: "bt2020nc"}
				p.Format.Audio.ChannelLayout, p.Format.Audio.Channels = "7.1", 8
			}
			// loop задаёт точно выровненную длину обеих дорожек; err сохраняет отказы этого сценария.
			loop, err := EffectiveLoopDuration(input.Duration, p, prober.Limits.MaxDuration)
			if err != nil {
				t.Fatal(err)
			}
			// command содержит рабочий программный рецепт без тестовой замены кодировщика.
			command, err := ProductionBuilder(binary)(GenerateRequest{Source: input, Profile: p, LoopDuration: loop})
			if err != nil {
				t.Fatal(err)
			}
			// raw содержит свежий результат кодировщика; normalized — файл после разрешённой нормализации.
			raw, normalized := expandedCPUFile(t, "raw.mp4"), expandedCPUFile(t, "prepared.mp4")
			if _, err = source.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			// result сохраняет stderr текущего кодировщика; все процессы ограничены runner.
			result, err := runner.Run(context.Background(), command, source, &boundedWriter{w: raw, remaining: 16 << 20})
			if err != nil {
				t.Fatal(err, result.Diagnostics)
			}
			if err = prober.NormalizePreparedForProfile(context.Background(), raw, normalized, p); err != nil {
				t.Fatal("normalize", err)
			}
			selfComparePackets(t, prober, raw, normalized, true)
			// actual измерен из реально созданного файла и проверен всеми AU приложения.
			actual, err := prober.Verify(context.Background(), normalized, p)
			if err != nil || core.CompatibleProfileFormat(actual, p) != nil {
				t.Fatal("verify", actual, err)
			}
			if err = prober.VerifyLoopDuration(context.Background(), normalized, loop); err != nil {
				t.Fatal("period", err)
			}
			// frames принимает контрольные суммы реального video decode после нормализации.
			var frames bytes.Buffer
			result, err = runner.Run(context.Background(), Command{Binary: binary, Args: []string{"-v", "error", "-nostdin", "-xerror", "-protocol_whitelist", "fd,pipe", "-fd", "3", "-i", "fd:", "-map", "0:v:0", "-f", "framemd5", "pipe:1"}}, expandedCPURewind(t, normalized), &boundedWriter{w: &frames, remaining: 1 << 20})
			if err != nil || bytes.Count(frames.Bytes(), []byte("\n0,")) != 48 {
				t.Fatal("video decode", err, result.Diagnostics, frames.String())
			}
			// pcm принимает три полных периода AAC без принудительной подмены карты каналов.
			var pcm bytes.Buffer
			result, err = runner.Run(context.Background(), Command{Binary: binary, Args: []string{"-v", "error", "-nostdin", "-xerror", "-strict", "2", "-protocol_whitelist", "fd,pipe", "-stream_loop", "2", "-fd", "3", "-i", "fd:", "-map", "0:a:0", "-vn", "-c:a", "pcm_s16le", "-f", "s16le", "pipe:1"}}, expandedCPURewind(t, normalized), &boundedWriter{w: &pcm, remaining: 8 << 20})
			// wantBytes задаёт точное число сэмплов трёх периодов во всех каналах по два байта.
			wantBytes := int64(loop) * int64(p.Format.Audio.SampleRateHz) / 1_000_000_000 * int64(p.Format.Audio.Channels) * 2 * 3
			if err != nil || int64(pcm.Len()) != wantBytes || bytes.Count(pcm.Bytes(), []byte{0}) != pcm.Len() {
				t.Fatal("AAC zero/period", err, pcm.Len(), wantBytes, result.Diagnostics)
			}
			if scenario.pixel == "gray" {
				expandedCPUGraySource(t, binary, runner, prober, normalized)
			}
		})
	}
}

// expandedCPUGraySource подаёт реально подготовленный AOM gray8 файл source обратно в Source
// и в рабочую подготовку x264 4:4:4. t владеет файлами, binary/runner/prober сохраняют ограничения
// исходного теста; проверяется настоящее расширение числа плоскостей, а не подставленное описание gray.
func expandedCPUGraySource(t *testing.T, binary string, runner Runner, prober Prober, source *os.File) {
	t.Helper()
	// observed измерен из настоящего монохромного AV1; err сообщает об отказе проверки источника.
	observed, err := prober.Source(context.Background(), source, core.AssetVideo)
	if err != nil || observed.Format.Video == nil || observed.Format.Video.PixelFormat != "gray" {
		t.Fatal("gray source", observed, err)
	}
	// p задаёт новый x264 high444 рецепт с исходными цветами, но с тремя плоскостями.
	p := selfProfile(core.CodecH264)
	p.LiveAudioMode = core.LiveAudioCopy
	p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderX264, core.RateControlCRF)
	p.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: 2357}
	p.VideoEncoding.TargetBitrateBPS, p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 0, 0, 0
	p.Format.Video.CodecProfile, p.Format.Video.PixelFormat = "high444", "yuv444p"
	p.Format.Video.Color = core.ColorInfo{Range: "full", Primaries: "bt2020", Transfer: "gamma22", Matrix: "bt2020nc"}
	// loop сохраняет полный период; command вызывает ProductionBuilder с настоящим gray source.
	loop, err := EffectiveLoopDuration(observed.Duration, p, prober.Limits.MaxDuration)
	if err != nil {
		t.Fatal("gray period", observed.Duration, p.Validate(), err)
	}
	command, err := ProductionBuilder(binary)(GenerateRequest{Source: observed, Profile: p, LoopDuration: loop})
	if err != nil {
		t.Fatal("gray builder", observed.Duration, loop, p.Validate(), err)
	}
	// raw содержит новое кодирование; prepared принимает только разрешённую временную нормализацию.
	raw, prepared := expandedCPUFile(t, "gray-lift.mp4"), expandedCPUFile(t, "gray-lift-prepared.mp4")
	// result сохраняет ограниченную диагностику реального gray→YUV кодирования.
	result, err := runner.Run(context.Background(), command, expandedCPURewind(t, source), raw)
	if err != nil {
		t.Fatal(err, result.Diagnostics)
	}
	if err = prober.NormalizePreparedForProfile(context.Background(), raw, prepared, p); err != nil {
		// doc содержит независимую диагностику дорожек; probeErr сообщает об отказе ffprobe.
		doc, probeErr := prober.inspect(context.Background(), raw)
		t.Fatal("gray normalization", err, doc, probeErr)
	}
	if _, err = prober.Verify(context.Background(), prepared, p); err != nil {
		t.Fatal("gray verification", err)
	}
	if err = prober.VerifyLoopDuration(context.Background(), prepared, loop); err != nil {
		t.Fatal("gray loop", err)
	}
}

// expandedCPUFile создаёт отдельный временный файл name с закрытием в конце теста t.
func expandedCPUFile(t *testing.T, name string) *os.File {
	t.Helper()
	// file содержит дескриптор, err сообщает об отказе создания.
	file, err := os.CreateTemp(t.TempDir(), name)
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, file)
	return file
}

// expandedCPURewind возвращает file после перемотки, оставляя его закрытие тесту t.
func expandedCPURewind(t *testing.T, file *os.File) *os.File {
	t.Helper()
	// err сообщает об отказе перемотки собственного тестового файла.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	return file
}
