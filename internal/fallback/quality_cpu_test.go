package fallback

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет настоящий путь подготовки с крайними QP/CRF и каждым tuning: нормализацию,
// сохранность AU, фактический ASC/layout, полную длительность и декодированную цифровую тишину.
// FFmpeg запускается только при явно заданном GOST_MEDIA_FFMPEG; каждый файл принадлежит тесту.
// t сообщает об ошибках проверяемого сценария.
func TestCPUQualityAndMultichannelPreparation(t *testing.T) {
	// binary содержит явно выбранный FFmpeg; без него тест не запускает внешние процессы.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("локальная CPU-проверка не включена")
	}
	// source содержит собственный PNG для всех рецептов; err сообщает об отказе его создания.
	source, err := os.CreateTemp(t.TempDir(), "source.png")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, source)
	if err = png.Encode(source, image.NewRGBA(image.Rect(0, 0, 128, 128))); err != nil {
		t.Fatal(err)
	}
	// runner выполняет последовательные процессы с конечным сроком и ограниченной диагностикой.
	runner := selfRunner()
	// prober измеряет настоящий выход, не подставляя желаемый layout из профиля.
	prober := Prober{Binary: filepath.Join(filepath.Dir(binary), "ffprobe"), Runner: runner, Limits: selfProbeLimits()}
	// input содержит проверенное описание PNG с конечной длительностью 1,6 секунды.
	input, err := prober.Source(context.Background(), source, core.AssetImage)
	if err != nil {
		t.Fatal(err)
	}
	// scenario задаёт режим, качество, tuning и независимую целевую карту каналов.
	for _, scenario := range []struct {
		// rc выбирает управление качеством. tuning выбирает публичное имя настройки SVT.
		rc, tuning string
		// value задаёт целое качество на границе диапазона либо среднее качество для tuning.
		value int
		// layout задаёт карту каналов; channels — согласованное с ней число каналов AAC.
		layout   string
		channels uint16
	}{
		{"cqp", "psnr", 1, "mono", 1}, {"cqp", "psnr", 63, "stereo", 2},
		{"crf", "psnr", 1, "2.1", 3}, {"crf", "psnr", 63, "3.0", 3},
		{"crf", "vq", 35, "4.0", 4}, {"crf", "ssim", 35, "5.0", 5},
		{"crf", "ms_ssim", 35, "5.1", 6}, {"crf", "vmaf", 35, "stereo", 2},
		{"crf", "psnr", 35, "7.1", 8},
	} {
		// t сообщает об ошибках проверяемого сценария.
		t.Run(scenario.rc+"/"+scenario.tuning+"/"+scenario.layout, func(t *testing.T) {
			// Готовит один реальный вариант и проверяет формат, временную сетку и декодированную тишину.
			// profile содержит новый AV1-рецепт с нулевыми ограничениями скорости и точной картой AAC.
			profile := selfProfile(core.CodecAV1)
			profile.LiveAudioMode = core.LiveAudioCopy
			profile.Encoding = core.DefaultEncoding(profile.Mode, core.VideoEncoderSVTAV1, scenario.rc)
			profile.Encoding.VideoCustom = "lp=1"
			profile.Encoding.SVTAV1.Tuning = scenario.tuning
			profile.VideoEncoding.TargetBitrateBPS, profile.VideoEncoding.MaxBitrateBPS, profile.VideoEncoding.BufferBits = 0, 0, 0
			profile.Format.Audio.ChannelLayout, profile.Format.Audio.Channels = scenario.layout, scenario.channels
			if scenario.rc == core.RateControlCQP {
				profile.Encoding.QP = core.OptionalQP{Set: true, Value: scenario.value}
			} else {
				profile.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: scenario.value * 100}
			}
			// loop содержит вычисленный общий период полных видеокадров и AAC AU;
			// err сохраняет отказы вычисления периода и последующих операций подготовки в этом подтесте.
			loop, err := EffectiveLoopDuration(input.Duration, profile, prober.Limits.MaxDuration)
			if err != nil {
				t.Fatal(err)
			}
			// command содержит настоящий рабочий рецепт без тестовой замены кодировщика.
			command, err := ProductionBuilder(binary)(GenerateRequest{Source: input, Profile: profile, LoopDuration: loop})
			if err != nil {
				t.Fatal(err)
			}
			// raw принимает исходный fMP4 с явно отмеченным пакетом задержки AAC.
			raw, err := os.CreateTemp(t.TempDir(), "raw.mp4")
			if err != nil {
				t.Fatal(err)
			}
			selfClose(t, raw)
			if _, err = source.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			// result сохраняет диагностику фактического кодирования для сообщения об отказе.
			result, err := runner.Run(context.Background(), command, source, &boundedWriter{w: raw, remaining: 8 << 20})
			if err != nil {
				t.Fatalf("encode: %v: %s", err, result.Diagnostics)
			}
			// normalized принимает файл после удаления только целой AAC AU задержки.
			normalized, err := os.CreateTemp(t.TempDir(), "normalized.mp4")
			if err != nil {
				t.Fatal(err)
			}
			selfClose(t, normalized)
			if scenario.layout == "7.1" {
				// observed подтверждает реальную широкую карту исходного ASC до коррекции;
				// err сообщает об отказах измерения и проверки прежнего API внутри этой ветви 7.1.
				observed, err := prober.observedFormat(context.Background(), raw)
				if err != nil || observed.Audio.ChannelLayout != "7.1(wide-back)" {
					t.Fatal("исходная карта native AAC", observed.Audio, err)
				}
				// untouched принимает результат прежнего API без разрешения исправлять ASC.
				untouched, err := os.CreateTemp(t.TempDir(), "untrusted.mp4")
				if err != nil {
					t.Fatal(err)
				}
				selfClose(t, untouched)
				if err = prober.NormalizePrepared(context.Background(), raw, untouched); err == nil {
					// oldActual измеряется после прежней нормализации, которая не вправе менять карту;
					// err сообщает об отказе этого измерения и последующей проверки неисправленного файла.
					oldActual, err := prober.observedFormat(context.Background(), untouched)
					if err != nil || oldActual.Audio != observed.Audio {
						t.Fatal("прежний API изменил ASC-карту", oldActual.Audio, err)
					}
					if _, err = prober.Verify(context.Background(), untouched, profile); err == nil {
						t.Fatal("неисправленный native config7 принят как обычная 7.1")
					}
				}
			}
			if err = prober.NormalizePreparedForProfile(context.Background(), raw, normalized, profile); err != nil {
				t.Fatal("normalize", err)
			}
			selfComparePackets(t, prober, raw, normalized, true)
			// actual содержит подтверждённый формат всех AU и независимых AV1-кадров.
			actual, err := prober.Verify(context.Background(), normalized, profile)
			if err != nil || actual.Audio != profile.Format.Audio {
				t.Fatal("verify", actual, err)
			}
			if err = prober.VerifyLoopDuration(context.Background(), normalized, loop); err != nil {
				t.Fatal("loop", err)
			}
			if _, err = normalized.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			// pcm принимает все декодированные сэмплы без принудительного изменения карты каналов.
			var pcm bytes.Buffer
			// args задаёт строгий AAC-декодер: config7 не получает совместимую подмену карты FFmpeg.
			args := []string{"-v", "error", "-nostdin", "-xerror", "-strict", "2", "-protocol_whitelist", "fd,pipe"}
			// repeats задаёт число полных воспроизведений; для новой 7.1 проверяются три петли.
			repeats := int64(1)
			if scenario.layout == "7.1" {
				args = append(args, "-stream_loop", "2")
				repeats = 3
			}
			args = append(args, "-fd", "3", "-i", "fd:", "-map", "0:a:0", "-vn", "-c:a", "pcm_s16le", "-f", "s16le", "pipe:1")
			result, err = runner.Run(context.Background(), Command{Binary: binary, Args: args}, normalized, &boundedWriter{w: &pcm, remaining: 4 << 20})
			if err != nil {
				t.Fatalf("decode: %v: %s", err, result.Diagnostics)
			}
			// expectedBytes выражает полный период в сэмплах всех каналов по два байта на сэмпл.
			expectedBytes := int64(loop) * int64(profile.Format.Audio.SampleRateHz) / 1_000_000_000 * int64(scenario.channels) * 2 * repeats
			if int64(pcm.Len()) != expectedBytes {
				t.Fatalf("PCM %d bytes, ожидалось %d", pcm.Len(), expectedBytes)
			}
			// index задаёт позицию PCM, sample — её фактическое значение для проверки цифровой тишины.
			for index, sample := range pcm.Bytes() {
				if sample != 0 {
					t.Fatalf("ненулевая тишина PCM[%d]=%d", index, sample)
				}
			}
		})
	}
}
