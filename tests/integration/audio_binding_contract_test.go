package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math"
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
	"github.com/GregoryGost/gost-restreamer/internal/storage"
	"github.com/bluenviron/gortmplib/pkg/amf0"
)

// audioContractProfile задаёт полный CPU-профиль для kind; t проверяет согласованность фикстуры.
// copyAudio оставляет LIVE аудио неизменным, FLAC явно использует обязательный transcode.
func audioContractProfile(t *testing.T, kind core.Codec, copyAudio bool) core.GroupProfile {
	t.Helper()
	// profile наследует прежние геометрию128×128, SDR и60FPS, выбирая настоящий x264.
	profile := publisherProfile(core.CodecH264, 128, 128)
	profile.Encoding = core.DefaultEncoding(core.ModeTranscode, core.VideoEncoderX264, core.RateControlVBR)
	profile.VideoEncoding.GOPFrames = 6
	profile.LiveAudioMode = core.LiveAudioTranscode
	if copyAudio {
		profile.LiveAudioMode = core.LiveAudioCopy
	}
	profile.Format.Audio = core.AudioFormat{Codec: kind, SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}
	switch kind {
	case core.CodecOpus:
		profile.Format.Audio.CodecProfile, profile.Encoding.AudioEncoder, profile.AudioEncoding.BitrateBPS = "celt", core.AudioEncoderOpus, 128000
	case core.CodecFLAC:
		profile.Format.Audio.CodecProfile, profile.Encoding.AudioEncoder, profile.AudioEncoding.BitrateBPS = "s16", core.AudioEncoderFLAC, 0
	case core.CodecMP3:
		profile.Format.Audio.CodecProfile, profile.Encoding.AudioEncoder, profile.AudioEncoding.BitrateBPS = "mpeg1-layer3", core.AudioEncoderMP3, 128000
	case core.CodecAC3:
		profile.Format.Audio.CodecProfile, profile.Encoding.AudioEncoder, profile.AudioEncoding.BitrateBPS = "ac3", core.AudioEncoderAC3, 192000
	}
	// err выявляет ошибку тестового профиля до запуска любого процесса.
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	return profile
}

// audioContractSession создаёт для t настоящие SQLite, manager, current manifest и default Registry.
// ctx ограничивает подготовку, binary задаёт FFmpeg, profile — точный сохраняемый профиль,
// endpoint указывает только на локальный receiver теста.
// Возвращаются реестр, группа, LIVE Router и подготовленный файл; очистка выполняется в обратном порядке.
func audioContractSession(t *testing.T, ctx context.Context, binary string, profile core.GroupProfile, endpoint string) (*group.Registry, *group.Reference, publisherPipeline, []byte) {
	t.Helper()
	// store и err описывают изолированное хранилище с рабочими Goose и RuntimeAuthority.
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "audio.sqlite"), storage.Options{PreparationLimits: core.PreparationLimits{MaxQueued: 2, MaxProfiles: 4}, OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 16, MaxIdentifierBytes: 1024, MaxBytes: 1 << 20}, ObservationLimits: core.ObservationLimits{MaxSessions: 4, MaxGroups: 8, MaxOutputs: 8, MaxFFmpegWorkers: 8, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 20 * time.Millisecond, RetryAttempts: 4, RetryTimeout: time.Second, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// После остановки процессов callback закрывает только SQLite данного теста.
	t.Cleanup(func() { _ = store.Close() })
	// service предоставляет рабочий домен поверх временной БД.
	service, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// asset получает синтетическое видео через настоящий менеджер подготовки.
	asset, err := service.CreateAsset(ctx, "Аудиозаглушка")
	if err != nil {
		t.Fatal(err)
	}
	// watchdog задаёт действительные сроки контроля источника и короткое восстановление.
	watchdog := core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: 50 * time.Millisecond, KeyframeTimeout: time.Second}
	// stream связывает сессию с выбранным материалом.
	stream, err := service.CreateStream(ctx, core.Stream{Name: "Аудиопоток", PublicationKey: "audio-native-key", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: watchdog}})
	if err != nil {
		t.Fatal(err)
	}
	// saved сохраняет полный профиль через рабочий домен.
	saved, err := service.CreateProfile(ctx, core.TranscodeProfile{Name: "Аудиопрофиль", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	// configured связывает поток с единственным требуемым профилем.
	configured, err := service.CreateGroup(ctx, core.TranscodeGroup{Name: "Аудиогруппа", StreamID: stream.ID, ProfileID: saved.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.CreateDestination(ctx, core.Destination{Name: "Локальный снимок", StreamID: stream.ID, GroupID: configured.ID, Platform: "custom", Endpoint: endpoint, StreamKey: "native-audio", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// files хранит реальные исходник, current и варианты только в каталоге теста.
	files, err := fallback.OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Callback освобождает файловое хранилище после менеджера и проигрывателя.
	t.Cleanup(func() { _ = files.Close() })
	// config включает штатные builder/prober/normalizer и конечные native бюджеты.
	config := fallback.Config{ReconcileLimit: 8, GeneratorVersion: core.CurrentFallbackGeneratorVersion, FFmpeg: binary, FFprobe: filepath.Join(filepath.Dir(binary), "ffprobe"), MaxUploadBytes: 1 << 20, MaxOutputBytes: 8 << 20, PrepareTimeout: 20 * time.Second, ShutdownTimeout: 4 * time.Second, QueueInterval: time.Millisecond, Runner: fallback.RunnerLimits{Timeout: 15 * time.Second, MaxArgs: 256, MaxCommandBytes: 65536, MaxDiagnosticBytes: 1 << 20}, Probe: fallback.ProbeLimits{MaxJSONBytes: 1 << 20, MaxWidth: 1024, MaxHeight: 1024, MaxPixels: 1 << 20, MaxDuration: 10 * time.Second, ImageDuration: 1600 * time.Millisecond, MaxPackets: 8192, Container: integrationLimits()}}
	// runner и probe используют настоящие процессы и измерения без управляемого process-helper.
	runner := fallback.Runner{Limits: config.Runner}
	probe := fallback.Prober{Binary: config.FFprobe, Runner: runner, Limits: config.Probe}
	// manager использует production tools без подстановки кодирования или измерения.
	manager, err := fallback.NewManagerWithTools(service, files, config, fallback.Tools{Builder: fallback.ProductionBuilder(binary), Inspector: probe, Runner: runner, NormalizeForProfile: probe.NormalizePreparedForProfile, NormalizeForSource: probe.NormalizePreparedForSource, VerifyLoopDuration: probe.VerifyLoopDuration})
	if err != nil {
		t.Fatal(err)
	}
	// Callback отменяет очередь и ждёт завершения собственного менеджера.
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	// sourceData содержит короткое SDR-видео с тоном; production preparation обязана заменить его тишиной.
	sourceData := mediaCommand(t, binary, nil, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=60:duration=1.6", "-f", "lavfi", "-i", "sine=frequency=743:sample_rate=48000:duration=1.6", "-c:v", "libx264", "-threads:v", "1", "-g", "6", "-bf", "0", "-vf", "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709", "-color_range:v", "tv", "-color_primaries:v", "bt709", "-color_trc:v", "bt709", "-colorspace:v", "bt709", "-c:a", "aac", "-ar:a", "48000", "-ac:a", "2", "-b:a", "64000", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "pipe:1")
	// job принимает загрузку через штатные source/probe/generator/publication.
	job, err := manager.Upload(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetVideo}, io.NopCloser(bytes.NewReader(sourceData)))
	if err != nil {
		t.Fatal(err)
	}
	// result требует и публикации полного комплекта, и доказанного cleanup подготовки.
	result, err := job.Wait(ctx)
	if err != nil || !result.Clean || len(result.Generation.RequiredProfiles) != 1 {
		t.Fatalf("production preparation: clean=%v err=%v", result.Clean, err)
	}
	// required закрепляет точный подготовленный вариант в сессии.
	required := result.Generation.RequiredProfiles[0]
	// receipt содержит действительный запуск Session со снимком published artifact.
	receipt, err := service.StartSession(ctx, core.StartSessionRequest{RequestID: "native-audio-start", StreamID: stream.ID, Bindings: []core.PreparedBinding{{GroupID: configured.ID, GenerationID: result.Generation.ID, VariantID: required.VariantID, GeneratorVersion: config.GeneratorVersion}}})
	if err != nil {
		t.Fatal(err)
	}
	// parent переводится в выданное успешное исполнение, из которого Registry получает lease.
	parent, err := service.Claim(ctx, receipt.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.MarkIssued(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err = service.Complete(ctx, parent, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	// observer обслуживает реальные наблюдения player/transcode данной Session.
	observer, err := service.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	// Callback освобождает наблюдателя после всех Group workers.
	t.Cleanup(func() { _ = observer.Close(context.Background()) })
	// outputs содержит точный сохранённый manifest, а не подставленный desired формат.
	outputs, err := service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 4, SessionID: parent.SessionID}})
	if err != nil || len(outputs.Items) != 1 {
		t.Fatal("снимок выхода", err)
	}
	// verified и prepared проверяют реальный размер/SHA через используемый приложением FileStore.
	verified, err := files.OpenVerified(ctx, outputs.Items[0].Snapshot.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := io.ReadAll(verified)
	if err = errors.Join(err, verified.Close()); err != nil {
		t.Fatal(err)
	}
	// base нужен для коротких UDS-путей Darwin; все его процессы завершаются до удаления.
	base, err := os.MkdirTemp("/private/tmp", "ac-")
	if err != nil {
		t.Fatal(err)
	}
	// Callback удаляет только собственный каталог сокетов.
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	// live предоставляет настоящий входной Router для source cuts и worker restarts.
	live, err := media.NewRouter(publisherRouterLimits())
	if err != nil {
		t.Fatal(err)
	}
	// Callback закрывает LIVE после остановки всех его подписчиков.
	t.Cleanup(func() { _ = live.Close() })
	// runtimeConfig оставляет Transcode/Player nil и разрешает штатному компилятору CPU x264.
	runtimeConfig := group.RuntimeConfig{Switcher: group.SwitcherConfig{Profile: saved.Profile, Watchdog: watchdog, Queue: publisherQueue(), Router: publisherRouterLimits(), MaxAVSkew: 100 * time.Millisecond, MaxTransitionGap: 100 * time.Millisecond}, Container: integrationLimits(), Binary: binary, OpenFile: files.OpenVerified, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), ResubscribeBackoff: 10 * time.Millisecond, ControlInterval: 5 * time.Millisecond, ShutdownTimeout: 4 * time.Second, FFmpeg: ffmpeg.Config{BaseDir: base, StartupTimeout: 8 * time.Second, VideoTimeout: 5 * time.Second, AudioTimeout: 5 * time.Second, WatchdogInterval: 10 * time.Millisecond, RestartBackoff: time.Millisecond, GracePeriod: 50 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 4 * time.Second, DiagnosticEvents: 256, DiagnosticBytes: 1 << 20, DiagnosticChunkBytes: 4096, MaxCommandBytes: 65536, MaxCommandArgs: 256}}
	// registry исполняет проверку сохранённого snapshot и создаёт реальные Group/Supervisor.
	registry, err := group.NewRegistry(ctx, group.RegistryConfig{Core: service, Parent: parent, Observer: observer, Runtime: runtimeConfig, MaxGroups: 2, MaxReferences: 2})
	if err != nil {
		t.Fatal(err)
	}
	// Callback ждёт завершения workers до закрытия нижележащих ресурсов.
	t.Cleanup(func() {
		// closeErr сообщает о недоказанной физической очистке.
		if closeErr := registry.Close(context.Background()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	// reference запускает реальные файловый player и обработчик LIVE.
	reference, err := registry.Acquire(ctx, outputs.Items[0].Snapshot, live)
	if err != nil {
		t.Fatal(err)
	}
	return registry, reference, publisherPipeline{service, stream, parent, observer, outputs.Items, live, runtimeConfig}, prepared
}

// audioContractDecode оборачивает audio packets в fMP4 без изменения AU/headers/time и декодирует их.
// t проверяет ошибки, binary задаёт независимый FFmpeg; отсутствие video samples не скрывает audio frames.
func audioContractDecode(t *testing.T, binary string, packets []media.Packet) []byte {
	t.Helper()
	if len(packets) == 0 {
		t.Fatal("нет пакетов для decoder oracle")
	}
	// video и audio берутся из проверенной полной пары первого Delivery.
	var video, audio media.CodecConfig
	// packet выбирает первый actual config каждой дорожки.
	for _, packet := range packets {
		if packet.Config().Info().Track == media.TrackVideo && video.Validate() != nil {
			video = packet.Config()
		}
		if packet.Config().Info().Track == media.TrackAudio && audio.Validate() != nil {
			audio = packet.Config()
		}
	}
	// videoInfo связывает заголовки видео с выбранной audio epoch без изменения сжатых байтов.
	videoInfo := video.Info()
	videoInfo.SourceEpoch = audio.Info().SourceEpoch
	// aligned сохраняет фактический формат видео в отдельном контейнере декодера;
	// err сообщает о несогласованной конфигурации.
	aligned, err := media.NewCodecConfig(videoInfo, video.Headers())
	if err != nil {
		t.Fatal(err)
	}
	// container сохраняет исходные AU и их метки для отдельного стандартного decoder input.
	var container bytes.Buffer
	// mux сохраняет фактические номера и subframes FLAC без назначения нового runtime права.
	mux, err := fmp4.NewMuxer(&container, fmp4.Init{Video: aligned, Audio: audio}, integrationLimits())
	if err != nil {
		t.Fatal(err)
	}
	// packet переносится в одну config identity лишь для декодирования; bytes и timing сохраняются.
	for _, packet := range packets {
		if packet.Config().Info().Track != media.TrackAudio {
			continue
		}
		// current меняет идентичность конфигурации без изменения фактического формата;
		// packetErr сообщает о невозможности создать или записать пакет.
		current, packetErr := media.NewPacket(packet.Info(), audio, packet.Payload())
		if packetErr != nil {
			t.Fatal(packetErr)
		}
		if packetErr = mux.WritePacket(current); packetErr != nil {
			t.Fatal(packetErr)
		}
	}
	return mediaCommand(t, binary, container.Bytes(), "-v", "error", "-xerror", "-i", "pipe:0", "-map", "0:a:0", "-c:a", "pcm_f32le", "-f", "f32le", "pipe:1")
}

// TestNativeDefaultAudioRegistryLoopsAndSourceRestart проверяет весь стандартный Group путь четырёх кодеков.
// t включает native процессы только явно; prepared manifest/hash, FD-player, несколько петель,
// +10s LIVE, source cut и повторный worker проверяются через реальные выходные AU и PCM.
func TestNativeDefaultAudioRegistryLoopsAndSourceRestart(t *testing.T) {
	// binary разрешает только отдельный opt-in native запуск.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("нужен GOST_MEDIA_FFMPEG")
	}
	// kind выбирает одну из четырёх проверяемых пар входного и выходного аудиокодека.
	for _, kind := range []core.Codec{core.CodecOpus, core.CodecFLAC, core.CodecMP3, core.CodecAC3} {
		// Подтест проверяет подготовку, петли и переключения выбранного кодека; t сообщает об отказах.
		t.Run(string(kind), func(t *testing.T) {
			// ctx ограничивает подготовку, три петли и две LIVE attempts; cancel освобождает таймер.
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			// profile копирует конечный audio binding; внешний FLAC явно перекодируется.
			profile := audioContractProfile(t, kind, kind != core.CodecFLAC)
			// nativePackets/init содержат слышимый actual source; FLAC начинает с внешнего frameN.
			nativePackets, init := audioContractNativeSource(t, binary, kind)
			// receiver независимо читает буквальные RTMP chunks на loopback без SDK serializer.
			receiver, _ := newPublisherReceiver(t, false)
			// registry/reference/pipeline/prepared содержат действительное приложение и опубликованный файл.
			registry, reference, pipeline, prepared := audioContractSession(t, ctx, binary, profile, "rtmp://"+receiver.listener.Addr().String()+"/audio")
			// publisher использует сохранённый snapshot и настоящий исходящий сериализатор.
			publisher := publisherActivate(t, pipeline, pipeline.outputs[0], reference.Router(), nil)
			// capture собирает bounded actual output; stopCapture закрывает и ждёт его подписку.
			capture, stopCapture := liveAudioStartCapture(t, reference.Router())
			defer stopCapture()
			// readinessErr сообщает о завершении player до подтверждённой фактической готовности.
			if readinessErr := reference.Group().WaitReady(ctx); readinessErr != nil {
				t.Fatalf("default player readiness: %v status=%+v", readinessErr, reference.Group().Status())
			}
			// original содержит пакеты одного полного подготовленного периода;
			// err сообщает об ошибке его контейнера или формата.
			original, err := encodingPreparedPackets(prepared, profile.Format)
			if err != nil {
				t.Fatal(err)
			}
			// periodSamples независимо суммирует фактическое число samples одного опубликованного периода.
			periodSamples := uint64(0)
			// packet задаёт очередную actual AU файла.
			for _, packet := range original[media.TrackAudio] {
				// samples содержит фактическое число сэмплов полной AU;
				// sampleErr сообщает об отказе разбора сжатых данных.
				samples, sampleErr := codec.AudioSamples(kind, packet.Config().Headers(), packet.Payload())
				if sampleErr != nil {
					t.Fatal(sampleErr)
				}
				periodSamples += uint64(samples)
			}
			publisherWait(t, func() bool {
				// Callback ждёт более трёх полных периодов по фактическим AU выхода player.
				return audioContractSamples(capture.snapshot()) >= 3*periodSamples
			})
			// silentPackets сохраняет только начальный fallback до любого слышимого source.
			silentPackets := capture.snapshot()
			// pcm должен содержать точные цифровые нули, включая границы всех петель.
			pcm := audioContractDecode(t, binary, silentPackets)
			if len(pcm) != int(audioContractSamples(silentPackets))*2*4 || liveAudioNonzeroSamples(pcm) != 0 {
				t.Fatalf("default player PCM: bytes=%d samples=%d nonzero=%d", len(pcm), audioContractSamples(silentPackets), liveAudioNonzeroSamples(pcm))
			}
			// round проверяет новый physical input и новый worker после реального source cut.
			for round := range 2 {
				// before отделяет output новой attempt от прежнего fallback.
				before := len(capture.snapshot())
				// stop и done отменяют paced source и подтверждают завершение его горутины.
				stop, done := audioContractSource(t, ctx, pipeline.live, nativePackets, init, 10+int64(round)*10)
				// livePackets закрепляет смешанное окно успешного ожидания; currentPackets содержит
				// только выбранную LIVE-эпоху из того же снимка для отдельной проверки её звука.
				var livePackets, currentPackets []media.Packet
				publisherWait(t, func() bool {
					// Callback ждёт захвата обеих дорожек выбранной эпохи и сохраняет именно
					// проверенные пакеты: следующий снимок мог бы содержать уже другой участок.
					// status связывает выбор LIVE с эпохой выходных конфигураций Router.
					status := reference.Group().Status()
					// window отделяет новые поступления от уже проверенной начальной тишины.
					window := capture.snapshot()[before:]
					// current отбирает эту LIVE-эпоху; ready требует захваченных видео и аудио.
					current, ready := audioContractLiveEpoch(status, window)
					if !ready || kind == core.CodecFLAC && len(window) <= 150 || kind != core.CodecFLAC && audioContractMatches(nativePackets, current) < 20 {
						return false
					}
					livePackets, currentPackets = window, current
					t.Logf("LIVE round=%d epoch=%d mixed=%d current=%d", round, status.Epoch, len(livePackets), len(currentPackets))
					return true
				})
				if kind != core.CodecFLAC {
					audioContractCopy(t, nativePackets, livePackets, original[media.TrackAudio])
				}
				// audible декодируется внешним decoder, исключая принятие одних codec tags.
				audible := audioContractDecode(t, binary, livePackets)
				if audioContractPeak(audible) < 0.01 {
					t.Fatal("actual transcode потерял слышимый source")
				}
				// currentAudible исключает звук запоздалой прежней LIVE-доставки из доказательства
				// слышимости текущей попытки; декодер и порог совпадают с проверкой смешанного окна.
				currentAudible := audioContractDecode(t, binary, currentPackets)
				if audioContractPeak(currentAudible) < 0.01 {
					t.Fatal("текущая LIVE-эпоха потеряла слышимый source")
				}
				stop()
				<-done
				publisherWait(t, func() bool {
					// Callback ждёт возврата исправного player после отзыва LIVE source.
					return reference.Group().Status().Available && reference.Group().Status().Source == group.Fallback
				})
			}
			if err = publisher.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			receiver.waitClosed(t)
			if receiver.accepted.Load() != 1 {
				t.Fatal("переключения разорвали исправное физическое соединение")
			}
			audioContractWire(t, kind, receiver.snapshot())
			if err = registry.Close(ctx); err != nil || !registry.Joined() {
				t.Fatal("Group workers не завершены", err)
			}
		})
	}
}

// audioContractLiveEpoch отбирает из packets только эпоху доступного LIVE в status.
// Возвращаемый отдельный список сохраняет порядок, конфигурации, AU и время исходных пакетов.
// Готовность требует реально захваченных аудио и видео: один статус Router не подтверждает,
// что независимый сборщик уже добавил обе дорожки в свой снимок. Звук проверяет декодер вызывающего.
func audioContractLiveEpoch(status group.Status, packets []media.Packet) ([]media.Packet, bool) {
	if !status.Available || status.Source != group.Live || status.Epoch == 0 {
		return nil, false
	}
	// current накапливает только выбранную эпоху, не изменяя список смешанного захвата.
	var current []media.Packet
	// hasVideo и hasAudio подтверждают наличие фактической конфигурации каждой дорожки для декодера.
	hasVideo, hasAudio := false, false
	// packet обозначает очередной захваченный пакет; прежние эпохи не доказывают текущий LIVE.
	for _, packet := range packets {
		if packet.Config().Info().SourceEpoch != status.Epoch {
			continue
		}
		current = append(current, packet)
		hasVideo = hasVideo || packet.Config().Info().Track == media.TrackVideo
		hasAudio = hasAudio || packet.Config().Info().Track == media.TrackAudio
	}
	return current, hasVideo && hasAudio
}

// audioContractMatches считает побайтно совпавшие source audio AU в output для bounded ожидания.
func audioContractMatches(source, output []media.Packet) int {
	// known содержит только фактические source payload, count считает их присутствие в output.
	known := make(map[[32]byte]bool)
	count := 0
	// packet обозначает исходную audio AU.
	for _, packet := range source {
		if packet.Config().Info().Track == media.TrackAudio {
			known[sha256.Sum256(packet.Payload())] = true
		}
	}
	// packet обозначает выбранную output AU; fallback не считается совпадением без равенства bytes.
	for _, packet := range output {
		if packet.Config().Info().Track == media.TrackAudio && known[sha256.Sum256(packet.Payload())] {
			count++
		}
	}
	return count
}

// audioContractWire проверяет для t raw FourCC, numeric metadata и непрерывный FLAC physical counter.
// kind выбирает буквальный внешний контракт; messages получены независимым chunk parser локального receiver.
func audioContractWire(t *testing.T, kind core.Codec, messages []publisherWireMessage) {
	t.Helper()
	// fourCC и metadata задаются буквально, независимо от production mapper и SDK serializer.
	fourCC, metadata := "Opus", float64(1332770163)
	switch kind {
	case core.CodecFLAC:
		fourCC, metadata = "fLaC", 1716281667
	case core.CodecMP3:
		fourCC, metadata = ".mp3", 778924083
	case core.CodecAC3:
		fourCC, metadata = "ac-3", 1633889587
	}
	// headers хранит фактическую wire config; counter не сбрасывается при новых SequenceStart.
	var headers []byte
	counter := uint64(0)
	// starts/coded/metas подтверждают содержательные сообщения всех обязательных типов.
	starts, coded, metas := 0, 0, 0
	// message обозначает следующее сообщение одного неизменного физического RTMP connection.
	for _, message := range messages {
		if message.typ == 18 {
			// values и err разбирают только AMF, без SDK audio serializer.
			values, err := amf0.Unmarshal(message.body)
			if err != nil || len(values) < 3 {
				t.Fatal("AMF metadata", err)
			}
			// object и ok подтверждают тип ECMAArray числового audiocodecid.
			object, ok := values[2].(amf0.ECMAArray)
			if !ok {
				t.Fatalf("AMF metadata type=%T", values[2])
			}
			// actual и found требуют именно numeric UI32 ASCII, а не строковое значение.
			actual, found := amf0.Object(object).GetFloat64("audiocodecid")
			if !found || actual != metadata {
				t.Fatalf("numeric audiocodecid=%v/%t want=%v", actual, found, metadata)
			}
			metas++
		}
		if message.typ != 8 {
			continue
		}
		if len(message.body) < 5 || string(message.body[1:5]) != fourCC {
			t.Fatalf("raw Enhanced FourCC=%x want=%q", message.body, fourCC)
		}
		switch message.body[0] {
		case 0x90:
			starts++
			headers = bytes.Clone(message.body[5:])
			switch kind {
			case core.CodecOpus:
				if len(headers) != 19 || !bytes.Equal(headers[:12], []byte{'O', 'p', 'u', 's', 'H', 'e', 'a', 'd', 1, 2, 0, 0}) || binary.LittleEndian.Uint32(headers[12:16]) != 48000 {
					t.Fatalf("normative Opus Head0=%x", headers)
				}
			case core.CodecFLAC:
				if len(headers) != 42 || !bytes.Equal(headers[:8], []byte{'f', 'L', 'a', 'C', 0x80, 0, 0, 34}) || binary.BigEndian.Uint16(headers[8:10]) != 48 || binary.BigEndian.Uint16(headers[10:12]) != 4608 || binary.BigEndian.Uint64(headers[18:26])&((1<<36)-1) != 0 || !bytes.Equal(headers[26:], make([]byte, 16)) {
					t.Fatalf("normative FLAC42=%x", headers)
				}
			case core.CodecMP3, core.CodecAC3:
				if len(headers) != 0 {
					t.Fatal("MP3/AC3 SequenceStart получил внутренний descriptor")
				}
			}
		case 0x91:
			coded++
			if kind == core.CodecFLAC {
				// frame и err проверяют фактический CRC/sample number каждой physical AU.
				frame, err := codec.FLACFrameInfo(headers[8:], message.body[5:])
				if err != nil || !frame.Variable || frame.Number != counter {
					t.Fatalf("FLAC physical counter: frame=%+v want=%d err=%v", frame, counter, err)
				}
				counter += uint64(frame.Samples)
			}
		default:
			t.Fatalf("неожиданный Enhanced packet type=%x", message.body[0])
		}
	}
	if starts < 3 || coded < 20 || metas < 3 {
		t.Fatalf("не проверены переключения wire: starts=%d coded=%d metas=%d", starts, coded, metas)
	}
}

// audioContractSamples суммирует actual длительности audio packets на clock48000.
// Возвращает0 при неподтверждённой AU, чтобы bounded ожидание не приняло неверный поток.
func audioContractSamples(packets []media.Packet) uint64 {
	// count накапливает число полных отсчётов на канал.
	count := uint64(0)
	// packet обозначает очередной actual output.
	for _, packet := range packets {
		if packet.Config().Info().Track != media.TrackAudio {
			continue
		}
		// samples и err получаются из полной AU, а не из настроек кодировщика.
		samples, err := codec.AudioSamples(packet.Config().Info().Audio.Codec, packet.Config().Headers(), packet.Payload())
		if err != nil {
			return 0
		}
		count += uint64(samples)
	}
	return count
}

// audioContractPeak возвращает наибольшую абсолютную амплитуду полного float32 PCM в raw.
func audioContractPeak(raw []byte) float64 {
	// peak хранит максимальную амплитуду среди декодированных каналов.
	peak := float64(0)
	// offset указывает начало очередного полного PCM sample.
	for offset := 0; offset+4 <= len(raw); offset += 4 {
		peak = max(peak, math.Abs(float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[offset:])))))
	}
	return peak
}

// audioContractSource публикует в router реальные packets/init с общим положительным originSeconds.
// t проверяет setup; parent ограничивает paced передачу; stop отменяет source, done подтверждает cut/cleanup.
func audioContractSource(t *testing.T, parent context.Context, router *media.Router, packets []media.Packet, init fmp4.Init, originSeconds int64) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	// source и err создают настоящий новый input epoch, отзывающий прежние Delivery.
	source, err := router.BeginSource()
	if err != nil {
		t.Fatal(err)
	}
	// videoInfo/audioInfo сохраняют реальные параметры и связывают пару с новым source.
	videoInfo, audioInfo := init.Video.Info(), init.Audio.Info()
	videoInfo.SourceEpoch, audioInfo.SourceEpoch = source.Epoch(), source.Epoch()
	// video и audio содержат отдельные immutable source configs.
	video, err := media.NewCodecConfig(videoInfo, init.Video.Headers())
	if err != nil {
		t.Fatal(err)
	}
	audio, err := media.NewCodecConfig(audioInfo, init.Audio.Headers())
	if err != nil {
		t.Fatal(err)
	}
	if err = source.UpdateConfigs(video, audio); err != nil {
		t.Fatal(err)
	}
	// ctx/cancel ограничивают одну волну LIVE; done подтверждает освобождение source.
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	// Cleanup гарантирует завершение источника и при ошибке независимого decoder oracle.
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		// Callback передаёт source по его действительным DTS; отмена всегда отзывает epoch.
		defer close(done)
		defer func() {
			// Callback отзывает текущий source; уже отозванная эпоха при замене не является ошибкой cleanup.
			// disconnectErr сохраняет настоящий отказ освобождения Router source.
			if disconnectErr := source.Disconnect(); disconnectErr != nil && !errors.Is(disconnectErr, media.ErrStaleSource) && !errors.Is(disconnectErr, media.ErrRouterClosed) {
				t.Error(disconnectErr)
			}
		}()
		// start задаёт момент начала передачи по системным часам, сохраняя междорожечный сдвиг DTS.
		start := time.Now()
		// packet переносит только source identity и общий origin; AU и duration прежние.
		for _, packet := range packets {
			// config выбирает immutable config дорожки; info хранит исходные временные поля.
			config, info := video, packet.Info()
			if packet.Config().Info().Track == media.TrackAudio {
				config = audio
			}
			// base содержит единицу времени дорожки; at задаёт срок передачи относительно start.
			base := config.Info().TimeBase
			at := time.Duration(info.DTS) * time.Duration(base.Numerator) * time.Second / time.Duration(base.Denominator)
			// timer ограничен следующим DTS; не используется произвольная пауза готовности.
			timer := time.NewTimer(max(time.Until(start.Add(at)), 0))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			info.PTS += originSeconds * int64(base.Denominator) / int64(base.Numerator)
			info.DTS += originSeconds * int64(base.Denominator) / int64(base.Numerator)
			// current сохраняет сжатые байты и время пакета в новой эпохе источника;
			// packetErr сообщает об ошибке создания или публикации пакета.
			current, packetErr := media.NewPacket(info, config, packet.Payload())
			if packetErr != nil {
				t.Error(packetErr)
				return
			}
			if packetErr = source.Publish(current); packetErr != nil {
				if ctx.Err() == nil {
					t.Error(packetErr)
				}
				return
			}
		}
		<-ctx.Done()
	}()
	return cancel, done
}

// audioContractNativeSource кодирует ограниченный AV1/tone source кодека kind установленным binary.
// t проверяет полные actual AU; терминальный encoder tail не включается в продолжаемую LIVE подачу.
// Для FLAC сохраняются внешние frame numbers после точки7, а output authority остаётся false.
func audioContractNativeSource(t *testing.T, binary string, kind core.Codec) ([]media.Packet, fmp4.Init) {
	t.Helper()
	// args задаёт фактические размеры, CFR, CICP, канализацию и явный конечный audio encoder.
	args := []string{"-v", "error", "-xerror", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=60:duration=5", "-f", "lavfi", "-i", "sine=frequency=997:sample_rate=48000:duration=5", "-map", "0:v", "-map", "1:a", "-c:v", "libaom-av1", "-cpu-used", "8", "-threads:v", "1", "-lag-in-frames", "0", "-g:v", "6", "-b:v", "250000", "-vf", "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709", "-color_range:v", "tv", "-color_primaries:v", "bt709", "-color_trc:v", "bt709", "-colorspace:v", "bt709", "-enc_time_base:v", "1:60", "-video_track_timescale", "60", "-ar:a", "48000", "-ac:a", "2"}
	switch kind {
	case core.CodecOpus:
		args = append(args, "-c:a", "libopus", "-b:a", "128000", "-application", "lowdelay", "-frame_duration", "2.5", "-vbr", "on", "-fec", "0")
	case core.CodecFLAC:
		args = append(args, "-c:a", "flac", "-sample_fmt:a", "s16")
	case core.CodecMP3:
		args = append(args, "-c:a", "libmp3lame", "-b:a", "128000", "-reservoir", "0", "-abr", "0")
	case core.CodecAC3:
		args = append(args, "-c:a", "ac3", "-b:a", "192000")
	}
	args = append(args, "-strict", "-2", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-avoid_negative_ts", "disabled", "pipe:1")
	// raw содержит самостоятельный native MP4 без UUID или runtime признака владения.
	raw := mediaCommand(t, binary, nil, args...)
	// demux восстанавливает фактические аудиопараметры и проверяемую геометрию AV1 CFR60;
	// err сообщает об ошибке контейнера или конфигурации дорожек.
	demux, err := fmp4.NewDemuxer(bytes.NewReader(raw), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(description fmp4.TrackDescription) (media.CodecConfig, error) {
		// Callback сохраняет actual audio headers и ограниченно дополняет AV1 из известного native recipe.
		if description.Codec != core.CodecAV1 {
			return fmp4.ResolveCodecConfig(description, 1, 1)
		}
		return media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: description.TimeBase, Video: publisherProfile(core.CodecAV1, 128, 128).Format.Video}, description.Headers)
	}})
	if err != nil {
		t.Fatal("native source completion", err)
	}
	// packets удерживает продолжаемый full-AU префикс; inspector проверяет actual bytes/CICP/duration.
	var packets []media.Packet
	var inspector codec.Inspector
	// flacFirst отмечает первую retained FLAC AU для независимой проверки исходного frameN.
	flacFirst := false
	for {
		// packet и readErr содержат следующую native AU до хвоста конечного encoder файла.
		packet, readErr := demux.Next()
		if readErr != nil {
			t.Fatal("native source prefix", readErr)
		}
		if fallbackActualTime(packet.Info().DTS, packet.Config().Info().TimeBase).Cmp(fallbackActualTime(4, media.TimeBase{Numerator: 1, Denominator: 1})) >= 0 {
			break
		}
		if _, readErr = inspector.Inspect(packet); readErr != nil {
			t.Fatal("actual native source", readErr)
		}
		if kind == core.CodecFLAC {
			if fallbackActualTime(packet.Info().DTS, packet.Config().Info().TimeBase).Cmp(fallbackActualTime(7*4608, media.TimeBase{Numerator: 1, Denominator: 48000})) < 0 {
				continue
			}
			if packet.Config().Info().Track == media.TrackAudio && !flacFirst {
				// frame подтверждает внешний номер кадра FLAC для входа перекодирования;
				// frameErr сообщает об ошибке заголовков, сжатого кадра или CRC.
				frame, frameErr := codec.FLACFrameInfo(packet.Config().Headers(), packet.Payload())
				if frameErr != nil || frame.Number < 7 || frame.Variable || packet.Config().Info().FLACOwned {
					t.Fatalf("внешний frameN не подтверждён: %+v err=%v", frame, frameErr)
				}
				flacFirst = true
			}
		}
		packets = append(packets, packet)
		if len(packets) > 4096 {
			t.Fatal("native source вышел за bounded prefix")
		}
	}
	if len(packets) == 0 || kind == core.CodecFLAC && !flacFirst {
		t.Fatal("native source не содержит нужный префикс")
	}
	return packets, demux.Init()
}

// audioContractCopy проверяет для t сохранность actual audio из source в output.
// source и prepared задают полные допустимые payload, output содержит выбранную смесь.
// Источники могут различаться общим сдвигом; точные относительные DTS/PTS/duration остаются прежними.
func audioContractCopy(t *testing.T, source, output, prepared []media.Packet) {
	t.Helper()
	// known связывает полные исходные audio bytes с их actual временем.
	known := make(map[[32]byte]media.Packet)
	// quiet позволяет отличить законную тишину перехода от изменённой source AU.
	quiet := make(map[[32]byte]bool)
	// packet задаёт полный payload опубликованного fallback, который FD-player копирует как есть.
	for _, packet := range prepared {
		quiet[sha256.Sum256(packet.Payload())] = true
	}
	// packet индексирует только слышимый LIVE, исключая видео и prepared silence.
	for _, packet := range source {
		if packet.Config().Info().Track == media.TrackAudio {
			known[sha256.Sum256(packet.Payload())] = packet
		}
	}
	// matches считает неизменённые AU; firstSource/firstOutput фиксируют общий временной сдвиг.
	matches := 0
	var firstSource, firstOutput media.Packet
	// packet проверяется по полным исходным bytes, а не codec tag.
	for _, packet := range output {
		if packet.Config().Info().Track != media.TrackAudio {
			continue
		}
		// original и exists находят фактическую исходную AU по полному содержимому.
		original, exists := known[sha256.Sum256(packet.Payload())]
		if !exists {
			if !quiet[sha256.Sum256(packet.Payload())] {
				t.Fatal("audio copy изменил AU либо передал неразрешённый payload")
			}
			continue
		}
		if matches == 0 {
			firstSource, firstOutput = original, packet
		}
		if fallbackActualTime(original.Info().DurationTicks, original.Config().Info().TimeBase).Cmp(fallbackActualTime(packet.Info().DurationTicks, packet.Config().Info().TimeBase)) != 0 || fallbackActualTime(original.Info().DTS-firstSource.Info().DTS, original.Config().Info().TimeBase).Cmp(fallbackActualTime(packet.Info().DTS-firstOutput.Info().DTS, packet.Config().Info().TimeBase)) != 0 || fallbackActualTime(original.Info().PTS-firstSource.Info().PTS, original.Config().Info().TimeBase).Cmp(fallbackActualTime(packet.Info().PTS-firstOutput.Info().PTS, packet.Config().Info().TimeBase)) != 0 {
			t.Fatalf("copy изменил время retained AU: original=%+v output=%+v", original.Info(), packet.Info())
		}
		matches++
	}
	if matches < 10 {
		t.Fatalf("не подтверждён retained copy source: matched=%d", matches)
	}
}
