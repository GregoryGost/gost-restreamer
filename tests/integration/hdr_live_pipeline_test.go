package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
)

// hdrLiveProfile создаёт явный профиль encoder с исходной transfer без изменения сигнала.
// Пустая transfer выбирает SDR BT.709, остальные значения задают проверяемый PQ/HLG10.
func hdrLiveProfile(encoder, transfer string) core.GroupProfile {
	// profile сохраняет общий AAC LC48k/stereo64k и закрытый GOP исходной интеграционной фикстуры.
	profile := fallbackPipelineProfile(core.CodecH264)
	profile.LiveAudioMode = core.LiveAudioCopy
	profile.Encoding = core.DefaultEncoding(core.ModeTranscode, encoder, core.RateControlCRF)
	profile.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: 2357}
	profile.VideoEncoding.TargetBitrateBPS, profile.VideoEncoding.MaxBitrateBPS, profile.VideoEncoding.BufferBits = 0, 0, 0
	profile.VideoEncoding.GOPFrames, profile.VideoEncoding.MaxBFrames = 6, 0
	profile.Format.Video.Level, profile.Format.Video.Tier = "", ""
	switch encoder {
	case core.VideoEncoderX265:
		profile.Format.Video.Codec, profile.Format.Video.CodecProfile = core.CodecHEVC, "main"
	case core.VideoEncoderVP9:
		profile.Format.Video.Codec, profile.Format.Video.CodecProfile = core.CodecVP9, "profile0"
		profile.Encoding.CRF.ValueHundredths = 2300
	}
	if transfer != "" {
		profile.Format.Video.BitDepth, profile.Format.Video.PixelFormat = 10, "yuv420p10le"
		profile.Format.Video.Color = core.ColorInfo{Range: "limited", Primaries: "bt2020", Transfer: transfer, Matrix: "bt2020nc"}
		switch encoder {
		case core.VideoEncoderX265:
			profile.Format.Video.CodecProfile = "main10"
		case core.VideoEncoderVP9:
			profile.Format.Video.CodecProfile = "profile2"
		default:
			profile.Format.Video.CodecProfile = "high10"
		}
	}
	return profile
}

// hdrLiveFixture создаёт через binary независимый реальный источник выбранного kind без проверяемого компилятора.
// t принимает ошибки и выделяет временные файлы. transfer задаёт исходные HDR CICP;
// silent отличает AAC заглушки от LIVE-тона. Формат подтверждает
// ffprobe и рабочий Prober, а реальные headers/AU дополнительно проверяет Inspector.
func hdrLiveFixture(t *testing.T, binary string, kind core.Codec, transfer string, silent bool) ([]byte, []media.Packet, fmp4.Init) {
	t.Helper()
	// pixel и color задают независимые входные свойства lavfi до сжатия.
	pixel, color := "yuv420p", core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}
	if transfer != "" {
		pixel, color = "yuv420p10le", core.ColorInfo{Range: "limited", Primaries: "bt2020", Transfer: transfer, Matrix: "bt2020nc"}
	}
	// audio задаёт цифровую тишину файла либо различимый тон LIVE, оба ровно 1,6 секунды.
	audio := "sine=frequency=750:sample_rate=48000:duration=1.6"
	if silent {
		audio = "anullsrc=r=48000:cl=stereo:d=1.6"
	}
	// args создаёт CFR 128×128/30, фиксированный закрытый GOP без B-кадров и обычный AAC.
	args := []string{"-v", "error", "-nostdin", "-xerror", "-f", "lavfi", "-i", "testsrc2=s=128x128:r=30:d=1.6", "-f", "lavfi", "-i", audio, "-map", "0:v", "-map", "1:a", "-vf", "format=" + pixel + ",setparams=range=limited:color_primaries=" + color.Primaries + ":color_trc=" + color.Transfer + ":colorspace=" + color.Matrix, "-pix_fmt", pixel, "-threads:v", "1", "-g:v", "6", "-bf:v", "0"}
	switch kind {
	case core.CodecAV1:
		args = append(args, "-c:v", "libaom-av1", "-cpu-used", "8", "-lag-in-frames", "0", "-crf", "30")
	case core.CodecHEVC:
		args = append(args, "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error:pools=none:frame-threads=1:open-gop=0:scenecut=0:bframes=0:ref=1", "-crf", "23")
	case core.CodecVP9:
		args = append(args, "-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", "-lag-in-frames", "0", "-auto-alt-ref", "0", "-b:v", "0", "-crf", "23")
	case core.CodecH264:
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-x264-params", "open-gop=0:scenecut=0:ref=1", "-crf", "23")
	default:
		t.Fatal("Неизвестный кодек фикстуры", kind)
	}
	args = append(args, "-color_range:v", "tv", "-color_primaries:v", color.Primaries, "-color_trc:v", color.Transfer, "-colorspace:v", color.Matrix, "-c:a", "aac", "-b:a", "64000", "-ar:a", "48000", "-ac:a", "2", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-video_track_timescale", "30", "-avoid_negative_ts", "disabled", "pipe:1")
	// wire содержит действительные сжатые дорожки для независимой проверки через ffprobe.
	wire := mediaCommand(t, binary, nil, args...)
	// path ограничен временным каталогом этого теста; err сохраняет ошибки записи и открытия.
	path := filepath.Join(t.TempDir(), "source.mp4")
	if err := os.WriteFile(path, wire, 0600); err != nil {
		t.Fatal(err)
	}
	// file открывает исходный контейнер для Prober; err сохраняет отказ открытия и последующих проверок.
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { /* Закрывает исходный файл после ограниченных проверок метаданных. */
		_ = file.Close()
	}()
	// prober проверяет фактические CICP, глубину и заголовки без целевого профиля группы.
	prober := fallback.Prober{Binary: filepath.Join(filepath.Dir(binary), "ffprobe"), Runner: fallback.Runner{Limits: fallback.RunnerLimits{Timeout: 10 * time.Second, MaxArgs: 128, MaxCommandBytes: 16384, MaxDiagnosticBytes: 1 << 20}}, Limits: fallback.ProbeLimits{MaxJSONBytes: 1 << 20, MaxWidth: 1024, MaxHeight: 1024, MaxPixels: 1 << 20, MaxDuration: 10 * time.Second, ImageDuration: time.Second, MaxPackets: 4096, Container: integrationLimits()}}
	// measured содержит независимо измеренное описание дорожек; err сообщает об отказе его проверки.
	measured, err := prober.Source(context.Background(), file, core.AssetVideo)
	if err != nil || measured.Format.Video == nil {
		t.Fatal("Исходные фактические сведения", err)
	}
	if silent {
		// normalized удаляет подтверждённый целый AAC AU задержки перед штатным файловым проигрывателем.
		var normalized bytes.Buffer
		if err = prober.NormalizePrepared(context.Background(), file, &normalized); err != nil {
			t.Fatal("Нормализация тишины заглушки", err)
		}
		wire = normalized.Bytes()
	}
	// demux использует стандартный resolver HEVC/VP9/AAC; только AV1 получает измеренный Prober формат.
	demux, err := fmp4.NewDemuxer(bytes.NewReader(wire), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		// desc содержит реальную запись контейнера; AV1 использует отдельное измерение без desired-подстановки.
		if desc.Codec != core.CodecAV1 {
			return fmp4.ResolveCodecConfig(desc, 1, 1)
		}
		return media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase, Video: *measured.Format.Video, HDR: measured.HDR}, desc.Headers)
	}})
	if err != nil {
		t.Fatal(err)
	}
	// packets и init сохраняют реальные нагрузки, времена и полные исходные конфигурации.
	packets, init := readMediaPackets(t, demux), demux.Init()
	if transfer != "" && !silent {
		// info добавляет авторский CLL тестового контейнера без изменения сжатых AU.
		info := init.Video.Info()
		info.HDR.CLL = media.HDRContentLight{Present: true, Source: media.HDRSourceContainer, MaxCLL: media.HDRValue{Raw: 1000, Grid: media.HDRGridInteger, Known: true}, MaxFALL: media.HDRValue{Raw: 400, Grid: media.HDRGridInteger, Known: true}}
		init.Video, err = media.NewCodecConfig(info, init.Video.Headers())
		if err != nil {
			t.Fatal(err)
		}
		// index и packet выбирают видеопакет, который получает полный immutable snapshot того же источника.
		for index, packet := range packets {
			if packet.Config().Info().Track == media.TrackVideo {
				packets[index], err = media.NewPacket(packet.Info(), init.Video, packet.Payload())
				if err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// inspector сверяет каждый заголовок кадра с независимо измеренной конфигурацией.
	var inspector codec.Inspector
	// packet обозначает очередной полный AU источника.
	for _, packet := range packets {
		if _, err = inspector.Inspect(packet); err != nil {
			t.Fatal(err)
		}
	}
	return wire, packets, init
}

// TestDefaultGroupLiveVideoCPU проверяет настоящий Registry/Group с Transcode=nil и штатными
// Supervisor/UDS/fMP4. t запускает конечные CPU-сценарии: новые кодеки SDR/HDR и x264 HDR;
// фактические выходные пиксели, CICP, AAC payload и остановка проверяются после рабочего LIVE.
func TestDefaultGroupLiveVideoCPU(t *testing.T) {
	// binary включает только явно разрешённую локальную медиапроверку.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	// scenario задаёт исходный кодек, целевой кодировщик и сохранённую передаточную функцию.
	for _, scenario := range []struct {
		// input задаёт кодек настоящего LIVE; encoder и transfer выбирают независимый целевой профиль.
		input             core.Codec
		encoder, transfer string
	}{
		{core.CodecAV1, core.VideoEncoderX265, ""},
		{core.CodecHEVC, core.VideoEncoderVP9, ""},
		{core.CodecVP9, core.VideoEncoderX265, "smpte2084"},
		{core.CodecAV1, core.VideoEncoderVP9, "arib-std-b67"},
		{core.CodecHEVC, core.VideoEncoderX264, "smpte2084"},
	} {
		// Подтест через t сохраняет отдельные SQLite, процессы, UDS и временные файлы сценария.
		t.Run(string(scenario.input)+"/"+scenario.encoder+"/"+scenario.transfer, func(t *testing.T) {
			// profile задаёт явно выбранный выход; input и inputInit содержат независимо созданный LIVE.
			profile := hdrLiveProfile(scenario.encoder, scenario.transfer)
			_, input, inputInit := hdrLiveFixture(t, binary, scenario.input, scenario.transfer, false)
			// prepared содержит отдельную тишину выбранного выходного кодека для штатного проигрывателя.
			prepared, _, _ := hdrLiveFixture(t, binary, profile.Format.Video.Codec, scenario.transfer, true)
			// fixture создаёт настоящую временную доменную Session; адрес синтетический и не используется сетью.
			fixture := newPublisherPipeline(t, []core.GroupProfile{profile}, []string{"rtmp://127.0.0.1/unused"}, []string{"synthetic-key"}, []int{0})
			// path задаёт отдельный готовый файл; err сообщает об отказе его записи.
			path := filepath.Join(t.TempDir(), "fallback.mp4")
			if err := os.WriteFile(path, prepared, 0600); err != nil {
				t.Fatal(err)
			}
			fixture.config.Binary = binary
			fixture.config.OpenFile = func(context.Context, core.ArtifactManifest) (*os.File, error) { /* Открывает принадлежащий сценарию файл для стандартного проигрывателя. */
				return os.Open(path)
			}
			fixture.config.FFmpeg.StartupTimeout = 10 * time.Second
			fixture.config.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
			if fixture.config.Transcode != nil || fixture.config.Player != nil {
				t.Fatal("Сценарий требует стандартные построители Group")
			}
			// registry владеет реальными работниками; err сохраняет ошибки создания и исполнения.
			registry, err := group.NewRegistry(context.Background(), group.RegistryConfig{Core: fixture.service, Parent: fixture.parent, Observer: fixture.observer, Runtime: fixture.config, MaxGroups: 2, MaxReferences: 2})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { /* Завершает и присоединяет работников до удаления UDS и SQLite. */
				// err сообщает о неподтверждённой очистке процессов и файлов попытки.
				if err := registry.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			// reference удерживает действительную группу и её неизменный выходной Router.
			reference, err := registry.Acquire(context.Background(), fixture.outputs[0].Snapshot, fixture.live)
			if err != nil {
				t.Fatal(err)
			}
			// readyContext ограничивает первоначальную готовность файла; stopReady освобождает таймер.
			readyContext, stopReady := context.WithTimeout(context.Background(), 10*time.Second)
			err = reference.Group().WaitReady(readyContext)
			stopReady()
			if err != nil {
				t.Fatal("Штатная заглушка не готова", err)
			}
			// tap получает реальные выходные AU с ограниченной очередью; defer освобождает её резерв.
			tap, err := reference.Router().Subscribe(publisherQueue())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { /* Освобождает подписку после окончания захвата. */
				_ = tap.Close()
			}()
			// source выдаёт единственную входную публикацию с теми же реальными конфигурациями.
			source, err := fixture.live.BeginSource()
			if err != nil {
				t.Fatal(err)
			}
			if err = source.UpdateConfigs(inputInit.Video, inputInit.Audio); err != nil {
				t.Fatal(err)
			}
			// ctx ограничивает весь LIVE; cancel отзывает производителя даже при ошибке проверки.
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			// producerDone возвращает причинный отказ публикации и подтверждает окончание производителя.
			producerDone := make(chan error, 1)
			go func() {
				// Производитель подаёт пять реальных петель с возрастающими временными метками в реальном темпе.
				// start связывает транспортное начало с локальным временем.
				start := time.Now()
				// cycle задаёт сдвиг 1,6 секунды без изменения сжатых AU.
				for cycle := int64(0); cycle < 5; cycle++ {
					// packet обозначает очередной исходный AU обеих дорожек.
					for _, packet := range input {
						// info и base сохраняют время пакета и единицу его дорожки.
						info, base := packet.Info(), packet.Config().Info().TimeBase
						if packet.Config().Info().Track == media.TrackAudio && info.DTS < 0 {
							// Начальная задержка AAC не повторяется как дополнительный AU каждой петли.
							continue
						}
						// shift задаёт точные 1,6 секунды для обеих исходных шкал.
						shift := cycle * 8 * int64(base.Denominator) / (5 * int64(base.Numerator))
						info.PTS, info.DTS = info.PTS+shift, info.DTS+shift
						// at определяет срок подачи AU без отрицательного начального ожидания AAC.
						at := time.Duration(info.DTS) * time.Second * time.Duration(base.Numerator) / time.Duration(base.Denominator)
						// timer ждёт конкретный пакет и немедленно освобождается при отмене.
						timer := time.NewTimer(max(time.Until(start.Add(at)), 0))
						select {
						case <-ctx.Done():
							timer.Stop()
							producerDone <- nil
							return
						case <-timer.C:
						}
						// repeated сохраняет исходные bytes/config; publishErr передаёт отказ конструирования или Router.
						repeated, publishErr := media.NewPacket(info, packet.Config(), packet.Payload())
						if publishErr == nil {
							publishErr = source.Publish(repeated)
						}
						if publishErr != nil {
							producerDone <- publishErr
							return
						}
					}
				}
				producerDone <- nil
			}()
			defer func() { /* Отменяет подачу и подтверждает её остановку перед освобождением Router. */
				cancel()
				// producerErr сохраняет причинный отказ конечного производителя.
				if producerErr := <-producerDone; producerErr != nil {
					t.Error(producerErr)
				}
			}()
			// output накапливает независимый старт LIVE для последующего полного декодирования.
			var output []media.Packet
			// actual хранит фактическую пару выходных конфигураций; videoCount ограничивает захват.
			var actual fmp4.Init
			videoCount := 0
			for videoCount < 18 {
				// delivery и nextErr содержат реальный выход группы и причинный отказ её ожидания.
				delivery, nextErr := tap.Next(ctx)
				if nextErr != nil {
					t.Fatal("Group LIVE не достигнут", nextErr, reference.Group().Status())
				}
				if reference.Group().Status().Source != group.Live || !delivery.IsCurrent() {
					continue
				}
				if len(output) == 0 && (delivery.Packet.Config().Info().Track != media.TrackVideo || !delivery.Packet.Info().Keyframe) {
					continue
				}
				actual = fmp4.Init{Video: delivery.VideoConfig, Audio: delivery.AudioConfig}
				output = append(output, delivery.Packet)
				if delivery.Packet.Config().Info().Track == media.TrackVideo {
					videoCount++
				}
			}
			if err = core.CompatibleProfileFormat(core.MediaFormat{Video: actual.Video.Info().Video, Audio: actual.Audio.Info().Audio}, profile); err != nil {
				t.Fatal("Фактический выход Group", err)
			}
			if scenario.transfer != "" {
				// actualCLL и sourceCLL сравнивают авторские значения и известность в общей целой сетке.
				// Кодировщик может перенести container CLL в настоящий SEI, поэтому происхождение меняется.
				actualCLL, sourceCLL := actual.Video.Info().HDR.CLL, inputInit.Video.Info().HDR.CLL
				if !actualCLL.Present || actualCLL.MaxCLL != sourceCLL.MaxCLL || actualCLL.MaxFALL != sourceCLL.MaxFALL {
					t.Fatal("LIVE потерял авторский CLL источника", actualCLL, sourceCLL)
				}
			}
			if err = reference.Group().Ready(); err != nil {
				t.Fatal("Заглушка отказала во время LIVE", err)
			}
			// knownAudio связывает реально переданные AAC payload с неизменённым исходным содержимым.
			knownAudio := make(map[string]bool)
			// packet выбирает исходный AAC для проверки packet copy.
			for _, packet := range input {
				if packet.Config().Info().Track == media.TrackAudio {
					knownAudio[string(packet.Payload())] = true
				}
			}
			// encoded повторно упаковывает только захваченные AU; mux и err проверяют контейнерную границу.
			var encoded bytes.Buffer
			mux, err := fmp4.NewMuxer(&encoded, actual, integrationLimits())
			if err != nil {
				t.Fatal(err)
			}
			// inspector подтверждает каждый сжатый кадр; audioCount исключает ложный успех одного видео.
			var inspector codec.Inspector
			audioCount := 0
			// packet содержит фактический выход Group для независимых codec/copy проверок.
			for _, packet := range output {
				// inband восстанавливает сведения именно из реальных AU; inspectErr сохраняет отказ их разбора.
				_, inband, inspectErr := inspector.InspectMetadata(packet)
				if inspectErr != nil {
					t.Fatal(inspectErr)
				}
				if inband.CLL.Present {
					// expectedSource определяется типом реально разобранного SEI, независимо от Info.HDR.
					expectedSource := media.HDRSourceH264SEI
					if packet.Config().Info().Video.Codec == core.CodecHEVC {
						expectedSource = media.HDRSourceHEVCSEI
					}
					if inband.CLL.Source != expectedSource || actual.Video.Info().HDR.CLL.Source != expectedSource {
						t.Fatal("Реальный выходной SEI не стал авторитетным CLL", inband.CLL, actual.Video.Info().HDR.CLL)
					}
				}
				if err = packet.Validate(); err != nil {
					t.Fatal(err)
				}
				if packet.Config().Info().Track == media.TrackAudio {
					audioCount++
					if !knownAudio[string(packet.Payload())] {
						t.Fatal("LIVE AAC не сохранил исходный AU")
					}
				}
				if err = mux.WritePacket(packet); err != nil {
					t.Fatal(err)
				}
			}
			if audioCount < 10 {
				t.Fatal("Недостаточно фактических LIVE AAC AU", audioCount)
			}
			// decoded содержит реальный вывод декодера в исходной глубине без автоматического восьмибитного результата.
			decoded := mediaCommand(t, binary, encoded.Bytes(), "-v", "error", "-xerror", "-i", "pipe:0", "-map", "0:v", "-f", "framemd5", "pipe:1")
			if bytes.Count(decoded, []byte("\n0,")) != 18 {
				t.Fatal("Декодирован неполный LIVE", string(decoded))
			}
			// probe подтверждает реальные цветовые метки и глубину декодируемого результата отдельным ffprobe.
			var probe struct {
				// Streams содержит отдельно измеренные свойства единственной видеодорожки результата.
				Streams []struct {
					// Pixel, Primaries, Transfer и Matrix задают фактические плоскости и CICP потока.
					Pixel     string `json:"pix_fmt"`
					Primaries string `json:"color_primaries"`
					Transfer  string `json:"color_transfer"`
					Matrix    string `json:"color_space"`
				} `json:"streams"`
			}
			if err = json.Unmarshal(mediaCommand(t, filepath.Join(filepath.Dir(binary), "ffprobe"), encoded.Bytes(), "-v", "error", "-select_streams", "v:0", "-show_streams", "-of", "json", "pipe:0"), &probe); err != nil || len(probe.Streams) != 1 {
				t.Fatal(err, probe)
			}
			if probe.Streams[0].Pixel != profile.Format.Video.PixelFormat || probe.Streams[0].Primaries != profile.Format.Video.Color.Primaries || probe.Streams[0].Transfer != profile.Format.Video.Color.Transfer || probe.Streams[0].Matrix != profile.Format.Video.Color.Matrix {
				t.Fatal("Декодируемый LIVE потерял глубину или CICP", probe)
			}
			cancel()
			if err = registry.Close(context.Background()); err != nil || !registry.Joined() {
				t.Fatal("Group не подтвердил завершение", err)
			}
			// entries подтверждает удаление всех пространств UDS после присоединения работников.
			entries, err := os.ReadDir(fixture.config.FFmpeg.BaseDir)
			if err != nil || len(entries) != 0 {
				t.Fatal("Остались ресурсы UDS", entries, err)
			}
		})
	}
}

// TestLiveVideoBuilderRejectsUnsupportedSourceConversions проверяет отказы до процесса.
// t использует структурно допустимые source/desired, различающиеся одним запрещённым свойством.
func TestLiveVideoBuilderRejectsUnsupportedSourceConversions(t *testing.T) {
	// scenario связывает причину отказа с конкретным изменением фактического входа либо режима.
	for _, scenario := range []struct {
		// name различает контрактные отказы; mutate изменяет только выделенный конец config.
		name   string
		mutate func(config *ffmpeg.NVIDIAConfig)
	}{
		{"unknown_primaries", func(config *ffmpeg.NVIDIAConfig) { /* Неизвестный исходный CICP нельзя заменить целевым BT.2020. */
			config.Input.Video.Color.Primaries = "unspecified"
		}},
		{"different_hdr_transfer", func(config *ffmpeg.NVIDIAConfig) { /* PQ-цель не разрешает преобразование исходного HLG. */
			config.Input.Video.Color.Transfer = "arib-std-b67"
		}},
		{"different_hdr_range", func(config *ffmpeg.NVIDIAConfig) { /* Известный full range не становится limited при HDR. */
			config.Input.Video.Color.Range = "full"
		}},
		{"different_hdr_depth", func(config *ffmpeg.NVIDIAConfig) { /* Восьмибитный источник не выдаётся за фактический HDR10. */
			config.Input.Video.BitDepth, config.Input.Video.PixelFormat = 8, "yuv420p"
		}},
		{"different_hdr_gamut", func(config *ffmpeg.NVIDIAConfig) { /* Перекодирование не меняет исходные основные цвета HDR. */
			config.Input.Video.Color.Primaries = "bt709"
		}},
		{"rescale_disabled", func(config *ffmpeg.NVIDIAConfig) { /* Запрет масштабирования сохраняет исходные размеры обязательными. */
			config.Profile.Encoding.RescaleOutput = false
			config.Input.Video.Width += 2
		}},
		{"unsupported_live_input", func(config *ffmpeg.NVIDIAConfig) { /* H.264 не входит в текущий LIVE ingest AV1/HEVC/VP9. */
			config.Input.Video.Codec, config.Input.Video.CodecProfile = core.CodecH264, "high10"
		}},
		{"aac_copy_mismatch", func(config *ffmpeg.NVIDIAConfig) { /* LIVE audio copy сохраняет исходную частоту, а не выполняет скрытый resample. */
			config.Input.Audio.SampleRateHz = 44100
			config.AudioTimeBase = media.TimeBase{Numerator: 1, Denominator: 44100}
		}},
		{"copy_profile_has_no_encoder", func(config *ffmpeg.NVIDIAConfig) { /* Copy-профиль не должен запускать LIVE кодирование. */
			config.Profile.Mode = core.ModeCopy
			config.Profile.Encoding.RescaleOutput = false
		}},
	} {
		// Подтест через t отдельно проверяет целостность исходных данных и тип причинного отказа.
		t.Run(scenario.name, func(t *testing.T) {
			// profile задаёт валидный HEVC PQ10; input сохраняет независимый AV1-вход с тем же сигналом.
			profile := hdrLiveProfile(core.VideoEncoderX265, "smpte2084")
			input := profile.Format
			input.Video.Codec, input.Video.CodecProfile = core.CodecAV1, "main"
			// config связывает оба формата с точными фактическими шкалами и безопасными путями UDS.
			config := ffmpeg.NVIDIAConfig{Binary: "/usr/bin/ffmpeg", Input: input, Profile: profile, VideoTimeBase: media.TimeBase{Numerator: 1, Denominator: 30}, AudioTimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}}
			scenario.mutate(&config)
			if config.Input.Validate() != nil || config.Profile.Validate() != nil {
				t.Fatal("Проверка должна достигать source-aware границы", config)
			}
			// command и err показывают, что несовместимый вход не создаёт даже частичную команду.
			command, err := ffmpeg.BuildNVIDIACommand(config, ffmpeg.Attempt{InputPath: "/private/tmp/input", OutputPath: "/private/tmp/output"})
			if !errors.Is(err, ffmpeg.ErrInvalidSupervisor) || command.Binary != "" || len(command.Args) != 0 {
				t.Fatal("Несовместимый LIVE создал команду", command, err)
			}
		})
	}
}
