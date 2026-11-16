package fallback

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// audioBindingProfile создаёт профиль выбранного kind с x264-видео для теста t.
// Все параметры берутся из конечной публичной привязки, без подмены кодека в рабочем профиле.
func audioBindingProfile(t *testing.T, kind core.Codec) core.GroupProfile {
	t.Helper()
	// profile сохраняет размер, FPS и GOP базового профиля, явно выбирая аудиокодировщик.
	profile := selfProfile(core.CodecH264)
	profile.LiveAudioMode = core.LiveAudioTranscode
	profile.Encoding = core.DefaultEncoding(profile.Mode, core.VideoEncoderX264, core.RateControlVBR)
	profile.Format.Audio.Codec = kind
	switch kind {
	case core.CodecAAC:
		profile.Format.Audio.Channels, profile.Format.Audio.ChannelLayout, profile.AudioEncoding.BitrateBPS = 8, "7.1", 256000
	case core.CodecOpus:
		profile.Format.Audio.CodecProfile, profile.Encoding.AudioEncoder, profile.AudioEncoding.BitrateBPS = "celt", core.AudioEncoderOpus, 128000
	case core.CodecFLAC:
		profile.Format.Audio.CodecProfile, profile.Encoding.AudioEncoder, profile.AudioEncoding.BitrateBPS = "s16", core.AudioEncoderFLAC, 0
	case core.CodecMP3:
		profile.Format.Audio.CodecProfile, profile.Encoding.AudioEncoder, profile.AudioEncoding.BitrateBPS = "mpeg1-layer3", core.AudioEncoderMP3, 128000
	case core.CodecAC3:
		profile.Format.Audio.CodecProfile, profile.Encoding.AudioEncoder, profile.AudioEncoding.BitrateBPS = "ac3", core.AudioEncoderAC3, 192000
	}
	// err сообщает об ошибке самого тестового профиля до запуска компонентов.
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	return profile
}

// TestPreparedAudioBindingsUseFullLCMPeriods проверяет округление и границу maximum до процесса.
// t обнаруживает неверный выбор сетки, включая отдельный FLAC quantum4608 вместо AAC1024.
func TestPreparedAudioBindingsUseFullLCMPeriods(t *testing.T) {
	// scenario задаёт независимый ожидаемый период для30fps и source1s.
	for _, scenario := range []struct {
		// kind выбирает аудиокодек; period задаёт точную минимальную полную длину >=1s.
		kind   core.Codec
		period time.Duration
	}{
		{core.CodecOpus, time.Second},
		{core.CodecFLAC, 2400 * time.Millisecond},
		{core.CodecMP3, 1200 * time.Millisecond},
		{core.CodecAC3, 1600 * time.Millisecond},
	} {
		// profile задаёт новую конечную привязку; got и err показывают действительное выравнивание.
		profile := audioBindingProfile(t, scenario.kind)
		got, err := EffectiveLoopDuration(time.Second, profile, 10*time.Second)
		if err != nil || got != scenario.period {
			t.Fatal(scenario.kind, got, scenario.period, err)
		}
		if _, err = EffectiveLoopDuration(time.Second, profile, scenario.period-time.Microsecond); err == nil {
			t.Fatal("не соблюдён maximum", scenario.kind)
		}
	}
}

// TestCPUPreparedAudioBindingsDecodeToExactZeroPeriods проходит рабочие builder/normalizer/verify
// и повторно открывает реальные файлы новых кодеков и унаследованного AAC7.1. t владеет синтетическими файлами;
// нативные процессы включаются только через явный GOST_MEDIA_FFMPEG и ограничены Runner.
func TestCPUPreparedAudioBindingsDecodeToExactZeroPeriods(t *testing.T) {
	// executable задаёт разрешённый нативный FFmpeg; отсутствие переменной оставляет обычный Go suite без него.
	executable := os.Getenv("GOST_MEDIA_FFMPEG")
	if executable == "" {
		t.Skip("CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	// runner ограничивает процессы; prober проверяет реальный ffprobe и AU приложения.
	runner := selfRunner()
	prober := Prober{Binary: filepath.Join(filepath.Dir(executable), "ffprobe"), Runner: runner, Limits: selfProbeLimits()}
	// source содержит видео и слышимый входной тон, который подготовка обязана полностью игнорировать.
	source := expandedCPUFile(t, "source.mp4")
	// command создаёт изолированный тестовый источник с явным SDR-цветом и AAC-тоном.
	command := Command{Binary: executable, Args: []string{"-v", "error", "-nostdin", "-xerror", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=30:duration=1.6", "-f", "lavfi", "-i", "sine=frequency=997:sample_rate=48000:duration=1.6", "-vf", "setparams=range=limited:color_primaries=bt709:color_trc=bt709:colorspace=bt709", "-c:v", "libx264", "-preset", "ultrafast", "-bf:v", "0", "-color_range:v", "tv", "-color_primaries:v", "bt709", "-color_trc:v", "bt709", "-colorspace:v", "bt709", "-c:a", "aac", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "pipe:1"}}
	// result сохраняет диагностику; err сообщает об отказе генерации источника.
	result, err := runner.Run(context.Background(), command, nil, source)
	if err != nil {
		t.Fatal(err, string(result.Diagnostics))
	}
	// observed подтверждает исходные дорожки и нулевой A/V offset через настоящий ffprobe.
	observed, err := prober.Source(context.Background(), source, core.AssetVideo)
	if err != nil {
		t.Fatal(err)
	}
	// kind выбирает отдельную проверку полной конечной привязки и цифровой тишины трёх петель.
	for _, kind := range []core.Codec{core.CodecAAC, core.CodecOpus, core.CodecFLAC, core.CodecMP3, core.CodecAC3} {
		t.Run(string(kind), func(t *testing.T) {
			// profile содержит настоящий новый рецепт; loop задаёт полную общую сетку,
			// alignErr сообщает об отказе выравнивания до запуска процесса.
			profile := audioBindingProfile(t, kind)
			loop, alignErr := EffectiveLoopDuration(observed.Duration, profile, prober.Limits.MaxDuration)
			if alignErr != nil {
				t.Fatal(alignErr)
			}
			// generated использует ProductionBuilder без подмены какого-либо кодировщика;
			// buildErr сообщает о неподдержанных параметрах до процесса.
			generated, buildErr := ProductionBuilder(executable)(GenerateRequest{Source: observed, Profile: profile, LoopDuration: loop})
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			// raw принимает результат своего нативного кодировщика; prepared получает проверенный самостоятельный файл.
			raw, prepared := expandedCPUFile(t, "raw.mp4"), expandedCPUFile(t, "prepared.mp4")
			result, err = runner.Run(context.Background(), generated, expandedCPURewind(t, source), raw)
			if err != nil {
				t.Fatal("encode", err, string(result.Diagnostics))
			}
			if err = prober.NormalizePreparedForSource(context.Background(), raw, prepared, profile, observed); err != nil {
				audioBindingRawDiagnostics(t, prober, raw)
				t.Fatal("normalize", err)
			}
			if _, err = prober.Verify(context.Background(), prepared, profile); err != nil {
				t.Fatal("verify", err)
			}
			if err = prober.VerifyLoopDuration(context.Background(), prepared, loop); err != nil {
				t.Fatal("period", err)
			}
			// demux заново выводит конфигурации из байтов артефакта; openErr сообщает об отказе чтения.
			demux, openErr := fmp4.NewDemuxer(expandedCPURewind(t, prepared), fmp4.DemuxOptions{Limits: prober.Limits.Container})
			if openErr != nil {
				t.Fatal("reopen", openErr)
			}
			if demux.Init().Audio.Info().FLACOwned {
				t.Fatal("generic reopen назначил FLACOwned")
			}
			// audioCount проверяет фактические AU, actual samples и нулевое начало каждой дорожки.
			audioCount := uint64(0)
			for {
				// packet и readErr возвращают очередную реальную AU либо чистый конец файла.
				packet, readErr := demux.Next()
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					t.Fatal(readErr)
				}
				if packet.Config().Info().Track != media.TrackAudio {
					continue
				}
				// samples подтверждает размер из самой AU; sampleErr сообщает об ошибке codec parser.
				samples, sampleErr := codec.AudioSamples(kind, packet.Config().Headers(), packet.Payload())
				if sampleErr != nil || samples != preparedAudioSamples(kind) || packet.Info().DTS != int64(audioCount)*int64(samples) {
					t.Fatal(samples, audioCount, packet.Info(), sampleErr)
				}
				audioCount++
			}
			// pcm содержит f32le, поэтому ненулевые малые коэффициенты не теряются при округлении к s16.
			var pcm bytes.Buffer
			result, err = runner.Run(context.Background(), Command{Binary: executable, Args: []string{"-v", "error", "-nostdin", "-xerror", "-stream_loop", "2", "-fd", "3", "-i", "fd:", "-map", "0:a:0", "-vn", "-c:a", "pcm_f32le", "-f", "f32le", "pipe:1"}}, expandedCPURewind(t, prepared), &boundedWriter{w: &pcm, remaining: 32 << 20})
			// want задаёт точный размер трёх полных периодов48k/float32 во всех каналах профиля.
			want := int64(loop) * 48000 / int64(time.Second) * int64(profile.Format.Audio.Channels) * 4 * 3
			if err != nil || int64(pcm.Len()) != want || bytes.Count(pcm.Bytes(), []byte{0}) != pcm.Len() {
				t.Fatal("PCM zero/period", pcm.Len(), want, err, string(result.Diagnostics))
			}
		})
	}
}

// audioBindingRawDiagnostics выводит исходные крайние метки только при отказе native-нормализации.
// t получает диагностику реального file; prober сохраняет те же сроки и бюджеты ffprobe.
func audioBindingRawDiagnostics(t *testing.T, prober Prober, file *os.File) {
	t.Helper()
	// output принимает ограниченный JSON ffprobe с исходными метками аудио.
	var output bytes.Buffer
	// command запрашивает реальное время без заполнения отсутствующих duration.
	command := Command{Binary: prober.Binary, Args: []string{"-v", "error", "-fflags", "+nofillin", "-fd", "3", "-i", "fd:", "-select_streams", "a:0", "-show_packets", "-show_entries", "packet=codec_type,stream_index,pts,dts,duration", "-of", "json"}}
	// err сообщает об отказе ограниченного диагностического процесса.
	if _, err := prober.Runner.Run(context.Background(), command, expandedCPURewind(t, file), &boundedWriter{w: &output, remaining: prober.Limits.MaxJSONBytes}); err != nil {
		t.Log(err)
		return
	}
	// document содержит крайние исходные временные метки native encoder output.
	var document packetDocument
	// err сообщает о повреждении диагностического JSON.
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Log(err)
		return
	}
	// index и stamp выбирают несколько первых/последних меток без полного дампа всех AU.
	for index, stamp := range document.Packets {
		if index < 3 || index >= len(document.Packets)-3 {
			if stamp.PTS != nil && stamp.DTS != nil {
				t.Logf("audio[%d/%d] PTS=%d DTS=%d", index, len(document.Packets), *stamp.PTS, *stamp.DTS)
			}
			if stamp.Duration != nil {
				t.Logf("duration=%d", *stamp.Duration)
			}
		}
	}
}

// TestCPUPreparedAudioBindingPreservesSelectedHDRSource проверяет FLAC вместе с
// унаследованным selected-source HDR-проходом и настоящим HEVC10. t владеет синтетическим источником;
// процессы включаются только через GOST_MEDIA_FFMPEG, пользовательские файлы не читаются.
func TestCPUPreparedAudioBindingPreservesSelectedHDRSource(t *testing.T) {
	// executable задаёт разрешённый локальный FFmpeg для этого нативного сценария.
	executable := os.Getenv("GOST_MEDIA_FFMPEG")
	if executable == "" {
		t.Skip("CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	// runner ограничивает процессы; prober подтверждает headers, цвет и статичность HDR источника.
	runner := selfRunner()
	prober := Prober{Binary: filepath.Join(filepath.Dir(executable), "ffprobe"), Runner: runner, Limits: selfProbeLimits()}
	// source содержит реальное HEVC HDR10 с MDCV/CLL, созданное из собственного сырого градиента.
	source := modernCPUSource(t, executable, runner)
	// observed восстанавливает фактический источник; err сообщает о неподтверждённых исходных сведениях.
	observed, err := prober.Source(context.Background(), source, core.AssetVideo)
	if err != nil {
		t.Fatal(err)
	}
	observed, err = prober.SourceHDR(context.Background(), source, observed)
	if err != nil {
		t.Fatal(err)
	}
	// profile выбирает новый FLAC и прежний HEVC10-рецепт с исходными CICP без tone mapping.
	profile := audioBindingProfile(t, core.CodecFLAC)
	profile.Encoding = core.DefaultEncoding(profile.Mode, core.VideoEncoderX265, core.RateControlVBR)
	profile.Encoding.AudioEncoder = core.AudioEncoderFLAC
	profile.Format.Video.Codec, profile.Format.Video.CodecProfile = core.CodecHEVC, "main10"
	profile.Format.Video.PixelFormat, profile.Format.Video.BitDepth = "yuv420p10le", 10
	profile.Format.Video.Color = observed.Format.Video.Color
	if !needsHDRSource([]core.RequiredProfile{{Profile: profile}}) {
		t.Fatal("Encoding6 потерял проверку выбранного HDR-источника")
	}
	// loop задаёт полный период FLAC4608 и30fps; err сообщает об отказе выравнивания.
	loop, err := EffectiveLoopDuration(observed.Duration, profile, prober.Limits.MaxDuration)
	if err != nil {
		t.Fatal(err)
	}
	// command использует рабочий видеокодировщик и собственную FLAC-тишину.
	command, err := ProductionBuilder(executable)(GenerateRequest{Source: observed, Profile: profile, LoopDuration: loop})
	if err != nil {
		t.Fatal(err)
	}
	// raw и prepared принадлежат только этому сценарию и закрываются тестом.
	raw, prepared := expandedCPUFile(t, "hdr-native.mp4"), expandedCPUFile(t, "hdr-prepared.mp4")
	// result сохраняет ограниченную диагностику нативного кодирования; err сообщает об отказе процесса.
	result, err := runner.Run(context.Background(), command, expandedCPURewind(t, source), raw)
	if err != nil {
		t.Fatal(err, string(result.Diagnostics))
	}
	if err = prober.NormalizePreparedForSource(context.Background(), raw, prepared, profile, observed); err != nil {
		t.Fatal(err)
	}
	if _, err = prober.Verify(context.Background(), prepared, profile); err != nil {
		t.Fatal(err)
	}
	if err = prober.VerifyLoopDuration(context.Background(), prepared, loop); err != nil {
		t.Fatal(err)
	}
	// demux восстанавливает HDR артефакта из стандартных boxes без внешнего sourceHDR-параметра.
	demux, err := fmp4.NewDemuxer(expandedCPURewind(t, prepared), fmp4.DemuxOptions{Limits: prober.Limits.Container})
	if err != nil {
		t.Fatal(err)
	}
	if !demux.Init().Video.Info().HDR.MDCV.Present || demux.Init().Video.Info().HDR.CLL.MaxCLL.Raw != 1000 {
		t.Fatal("исходные HDR-сведения потеряны", demux.Init().Video.Info().HDR)
	}
}
