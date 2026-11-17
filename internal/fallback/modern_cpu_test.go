package fallback

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// TestCPUPreparedHEVCVP9HDR проверяет ProductionBuilder→нормализацию→Go fMP4→декодирование на CPU.
// Синтетический сырой 10-битный источник исключает зависимость от пользовательских файлов; метаданные берутся
// из настоящих заголовков источника, обе дорожки проверяются на полные периоды и неизменность сжатых AU.
// t сообщает об ошибках проверяемого сценария.
func TestCPUPreparedHEVCVP9HDR(t *testing.T) {
	// executable разрешает только явно выбранный локальный FFmpeg; runner ограничивает каждый процесс.
	executable := os.Getenv("GOST_MEDIA_FFMPEG")
	if executable == "" {
		t.Skip("CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	runner := selfRunner()
	// prober использует реальный ffprobe с прежними конечными бюджетами.
	prober := Prober{Binary: filepath.Join(filepath.Dir(executable), "ffprobe"), Runner: runner, Limits: selfProbeLimits()}
	// source содержит исходное HDR10 видео с SEI MDCV/CLL из синтетических плоскостей.
	source := modernCPUSource(t, executable, runner)
	// observed/err устанавливают фактический формат источника до единственного прохода сжатых HDR-сведений.
	observed, err := prober.Source(context.Background(), source, core.AssetVideo)
	if err != nil {
		t.Fatal("source probe", err)
	}
	observed, err = prober.SourceHDR(context.Background(), source, observed)
	if err != nil || !observed.HDRVerified || !observed.HDR.MDCV.Present || observed.HDR.CLL.MaxCLL.Raw != 1000 {
		t.Fatal("source HDR", observed.HDR, err)
	}
	// encoder выбирает новый HEVC/VP9 либо применимый прежний программный кодировщик HDR.
	for _, encoder := range []string{core.VideoEncoderX265, core.VideoEncoderVP9, core.VideoEncoderX264, core.VideoEncoderAOMAV1, core.VideoEncoderSVTAV1} {
		// t сообщает об ошибках проверяемого сценария.
		t.Run(encoder, func(t *testing.T) {
			// Кодирует реальный HDR-вариант, проверяет полную петлю и декодирует видео и AAC.
			// profile сохраняет общие 128×128/30 и AAC LC, явно включая новый рецепт.
			profile := selfProfile(core.CodecH264)
			profile.LiveAudioMode = core.LiveAudioCopy
			profile.Encoding = core.DefaultEncoding(profile.Mode, encoder, core.RateControlCRF)
			profile.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: 2357}
			profile.VideoEncoding.TargetBitrateBPS, profile.VideoEncoding.MaxBitrateBPS, profile.VideoEncoding.BufferBits = 0, 0, 0
			profile.Format.Video.PixelFormat, profile.Format.Video.BitDepth = "yuv420p10le", 10
			profile.Format.Video.Color = observed.Format.Video.Color
			switch encoder {
			case core.VideoEncoderX265:
				profile.Format.Video.Codec, profile.Format.Video.CodecProfile = core.CodecHEVC, "main10"
			case core.VideoEncoderVP9:
				profile.Format.Video.Codec, profile.Format.Video.CodecProfile = core.CodecVP9, "profile2"
				profile.Encoding.CRF.ValueHundredths = 2300
				profile.Format.Audio.Channels, profile.Format.Audio.ChannelLayout = 8, "7.1"
				profile.AudioEncoding.BitrateBPS = 256000
			case core.VideoEncoderX264:
				profile.Format.Video.CodecProfile = "high10"
			case core.VideoEncoderAOMAV1, core.VideoEncoderSVTAV1:
				profile.Mode, profile.Encoding.RescaleOutput = core.ModeCopy, false
				profile.Format.Video.Codec, profile.Format.Video.CodecProfile, profile.Format.Video.Tier = core.CodecAV1, "main", "main"
				profile.Encoding.CRF.ValueHundredths = 3500
				if encoder == core.VideoEncoderSVTAV1 {
					profile.Encoding.VideoCustom = "lp=1"
				}
			}
			// loop задаёт точную общую сетку видео/AAC; command вызывает рабочий кодировщик без подмены.
			// err сохраняет отказ вычисления общего периода или последующего построения команды этого подтеста.
			loop, err := EffectiveLoopDuration(observed.Duration, profile, prober.Limits.MaxDuration)
			if err != nil {
				t.Fatal(err)
			}
			command, err := ProductionBuilder(executable)(GenerateRequest{Source: observed, Profile: profile, LoopDuration: loop})
			if err != nil {
				t.Fatal("builder", err)
			}
			// raw и prepared принадлежат только этому сценарию и закрываются тестом.
			raw, prepared := expandedCPUFile(t, "native.mp4"), expandedCPUFile(t, "prepared.mp4")
			// result сохраняет ограниченный по объёму stderr, err — исходный отказ нативного кодировщика.
			result, err := runner.Run(context.Background(), command, expandedCPURewind(t, source), raw)
			if err != nil {
				t.Fatal("encode", err, result.Diagnostics)
			}
			if err = prober.NormalizePreparedForSource(context.Background(), raw, prepared, profile, observed); err != nil {
				t.Fatal("normalize", err)
			}
			// actual получен из реального артефакта, а не из profile.
			actual, err := prober.Verify(context.Background(), prepared, profile)
			if err != nil || core.CompatibleProfileFormat(actual, profile) != nil {
				t.Fatal("verify", actual, err)
			}
			if err = prober.VerifyLoopDuration(context.Background(), prepared, loop); err != nil {
				t.Fatal("period", err)
			}
			modernCPUComparePackets(t, prober, raw, prepared, observed.HDR)
			// demux восстанавливает HDR во время выполнения из стандартных дескрипторов при чтении готового артефакта.
			demux, err := fmp4.NewDemuxer(expandedCPURewind(t, prepared), fmp4.DemuxOptions{Limits: prober.Limits.Container})
			if err != nil || !demux.Init().Video.Info().HDR.MDCV.Present || !demux.Init().Video.Info().HDR.CLL.Present {
				t.Fatal("HDR reopen", err)
			}
			if encoder == core.VideoEncoderVP9 {
				// vp9Source содержит измерения сохранённого VP9 по фактическому vpcC, sourceErr — ошибку измерения.
				vp9Source, sourceErr := prober.Source(context.Background(), prepared, core.AssetVideo)
				if sourceErr != nil {
					t.Fatal("VP9 source", sourceErr)
				}
				vp9Source, sourceErr = prober.SourceHDR(context.Background(), prepared, vp9Source)
				if sourceErr != nil || !vp9Source.HDRVerified || !vp9Source.HDR.MDCV.Present {
					t.Fatal("VP9 source metadata", sourceErr, vp9Source.HDR)
				}
			}
			// video принимает несжатые десятибитные пиксели после декодирования; размер подтверждает глубину и все 48 кадров.
			var video bytes.Buffer
			result, err = runner.Run(context.Background(), Command{Binary: executable, Args: []string{"-v", "error", "-nostdin", "-xerror", "-fd", "3", "-i", "fd:", "-map", "0:v:0", "-pix_fmt", "yuv420p10le", "-f", "rawvideo", "pipe:1"}}, expandedCPURewind(t, prepared), &boundedWriter{w: &video, remaining: 8 << 20})
			if err != nil || video.Len() != 128*128*3*48 {
				t.Fatal("decode10", video.Len(), err, result.Diagnostics)
			}
			// first и last сверяют реальные десятибитные отсчёты градиента с допустимой погрешностью сжатия с потерями.
			first, last := int(binary.LittleEndian.Uint16(video.Bytes())), int(binary.LittleEndian.Uint16(video.Bytes()[254:]))
			if first < 32 || first > 96 || last < 921 || last > 985 {
				t.Fatalf("HDR10 градиент изменён: %d..%d", first, last)
			}
			// pcm содержит две полные AAC-петли; цифровая тишина проверяется по каждому сэмплу.
			var pcm bytes.Buffer
			result, err = runner.Run(context.Background(), Command{Binary: executable, Args: []string{"-v", "error", "-nostdin", "-xerror", "-stream_loop", "1", "-fd", "3", "-i", "fd:", "-map", "0:a:0", "-vn", "-c:a", "pcm_s16le", "-f", "s16le", "pipe:1"}}, expandedCPURewind(t, prepared), &boundedWriter{w: &pcm, remaining: 4 << 20})
			if err != nil || int64(pcm.Len()) != int64(loop)*48000/1_000_000_000*int64(profile.Format.Audio.Channels)*2*2 || bytes.Count(pcm.Bytes(), []byte{0}) != pcm.Len() {
				t.Fatal("AAC period", pcm.Len(), err, result.Diagnostics)
			}
		})
	}
}

// modernCPUSource создаёт 48 кадров сырого 10-битного градиента и кодирует HEVC HDR с настоящими MDCV/CLL.
// t закрывает временные файлы; executable/runner задают разрешённый процесс и конечные бюджеты.
func modernCPUSource(t *testing.T, executable string, runner Runner) *os.File {
	t.Helper()
	// input содержит отдельные десятибитные плоскости; output принимает настоящий источник MOV/MP4.
	input, output := expandedCPUFile(t, "source.yuv"), expandedCPUFile(t, "source.mp4")
	// frame хранит Y 128×128 и две нейтральные 64×64 плоскости по два байта на значение.
	frame := make([]byte, 128*128*3)
	// index задаёт десятибитное значение одного пикселя Y либо нейтральной цветности.
	for index := 0; index < len(frame)/2; index++ {
		// value сохраняет градиент Y в ограниченном десятибитном диапазоне либо нейтральные U/V=512.
		value := uint16(512)
		if index < 128*128 {
			value = uint16(64 + index%128*7)
		}
		binary.LittleEndian.PutUint16(frame[2*index:], value)
	}
	// index задаёт номер синтетического исходного кадра.
	for index := 0; index < 48; index++ {
		if _, err := input.Write(frame); err != nil {
			t.Fatal(err)
		}
	}
	// command использует несжатый вход без источника lavfi, и конечный x265 однослойный рецепт без B-кадров.
	command := Command{Binary: executable, Args: []string{"-v", "error", "-nostdin", "-xerror", "-f", "rawvideo", "-pixel_format", "yuv420p10le", "-video_size", "128x128", "-framerate", "30", "-fd", "3", "-i", "fd:", "-an", "-vf", "setparams=range=limited:color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc", "-c:v", "libx265", "-preset", "medium", "-profile:v", "main10", "-bf", "0", "-refs", "1", "-g", "15", "-x265-params", "open-gop=0:scenecut=0:bframes=0:ref=1:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1):max-cll=1000,400", "-color_range", "tv", "-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "pipe:1"}}
	// result/err сохраняют настоящий исход кодирования без подмены нативного кодировщика.
	result, err := runner.Run(context.Background(), command, expandedCPURewind(t, input), output)
	if err != nil {
		t.Fatal("raw source", err, result.Diagnostics)
	}
	return output
}

// modernCPUReadPackets читает фактические AU без подстановки желаемых значений. sourceHDR разрешает только узкую проверку
// свежего нативного результата, а nil требует строгого чтения уже нормализованного артефакта.
func modernCPUReadPackets(t *testing.T, prober Prober, file *os.File, sourceHDR *media.HDRMetadata) []media.Packet {
	t.Helper()
	// actual получен из реального ffprobe; err сохраняет отказ фактического измерения.
	actual, err := prober.observedFormat(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	// demux/err восстанавливают конфигурации из собственного артефакта.
	demux, err := fmp4.NewDemuxer(expandedCPURewind(t, file), fmp4.DemuxOptions{Limits: prober.Limits.Container, FreshHDRSource: sourceHDR,
		ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
			// Соединяет реальные заголовки кодека контейнера и измеренные дорожки без подстановки желаемых значений.
			// info сохраняет фактический формат и начальную идентичность только этого сравнения.
			info := media.CodecConfigInfo{Track: desc.Track, TimeBase: desc.TimeBase, SourceEpoch: 1, Revision: 1}
			if desc.Track == media.TrackVideo {
				info.Video, info.HDR = actual.Video, desc.HDR
			} else {
				info.Audio = actual.Audio
			}
			return media.NewCodecConfig(info, desc.Headers)
		}})
	if err != nil {
		t.Fatal(err)
	}
	// packets сохраняет все AU в исходном порядке без изменения сжатого содержимого или меток.
	var packets []media.Packet
	for {
		// packet/err содержат очередную AU либо конец ограниченного тестового файла.
		packet, err := demux.Next()
		if err == io.EOF {
			return packets
		}
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, packet)
	}
}

// modernCPUComparePackets проверяет сохранность всех исходных AU и временных меток; из raw удаляется ровно одна
// первая AAC AU, уже подтверждённая NormalizePreparedForSource как полный пакет задержки кодировщика.
func modernCPUComparePackets(t *testing.T, prober Prober, raw, prepared *os.File, source media.HDRMetadata) {
	t.Helper()
	// before и after содержат исходный и нормализованный поток сжатых данных.
	before := modernCPUReadPackets(t, prober, raw, &source)
	after := modernCPUReadPackets(t, prober, prepared, nil)
	// index и packet находят ровно первую AAC AU, не обрезая её сжатое содержимое.
	for index, packet := range before {
		if packet.Config().Info().Track == media.TrackAudio {
			before = append(before[:index], before[index+1:]...)
			break
		}
	}
	if len(before) != len(after) {
		t.Fatalf("число AU изменилось: %d/%d", len(before), len(after))
	}
	// index связывает одну и ту же исходную AU по положению в сериализованном потоке.
	for index := range before {
		if before[index].Info() != after[index].Info() || before[index].Config().Info().TimeBase != after[index].Config().Info().TimeBase ||
			before[index].Config().Info().Track != after[index].Config().Info().Track || !bytes.Equal(before[index].Payload(), after[index].Payload()) {
			t.Fatalf("исходная AU %d или её время изменились", index)
		}
	}
}
