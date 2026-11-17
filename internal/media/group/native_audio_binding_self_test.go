package group

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// TestNativeGroupDefaultAudioTranscodeUDS проверяет стандартный Group→FFmpeg→UDS→Switcher для четырёх
// выходов. t запускает только явно выбранный GOST_MEDIA_FFMPEG; источник остаётся AV1/AAC,
// а Transcode и Player в RuntimeConfig равны nil. Внешние назначения не используются.
func TestNativeGroupDefaultAudioTranscodeUDS(t *testing.T) {
	// binary разрешает запуск установленного нативного FFmpeg только отдельной явной командой.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("native group требует GOST_MEDIA_FFMPEG")
	}
	// fixture связывает Group с настоящими временными SQLite, Session и RuntimeLease.
	fixture := runtimeUnitFixture(t)
	// limits сохраняет бюджеты fMP4 для памяти и UDS.
	limits := fixture.config.Runtime.Container
	// ctx ограничивает нативную генерацию и четыре попытки процесса; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	// path содержит самостоятельный нативный источник AV1/AAC с полными фактическими заголовками.
	path := filepath.Join(t.TempDir(), "source.mp4")
	// command запускает ограниченный по времени кодировщик без оболочки и сетевых протоколов.
	command := exec.CommandContext(ctx, binary, "-v", "error", "-nostdin", "-copyts", "-f", "lavfi", "-i", "color=c=black:s=128x128:r=30", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo", "-t", "4", "-vf", "setparams=range=limited:color_primaries=bt709:color_trc=bt709:colorspace=bt709", "-lag-in-frames", "0", "-c:v", "libaom-av1", "-cpu-used", "8", "-threads", "1", "-g", "15", "-pix_fmt", "yuv420p", "-color_range", "tv", "-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709", "-c:a", "aac", "-b:a", "64000", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-avoid_negative_ts", "disabled", path)
	// output содержит диагностику генератора; err сообщает об отказе локальной команды.
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native source: %v %s", err, output)
	}
	// file и err содержат нативный источник и ошибку открытия перед уточнением формата.
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// closeErr сообщает об отказе освобождения ресурса file после проверки.
		if closeErr := file.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	// demux восстанавливает фактический формат по стандартным заголовкам и первой AU.
	demux, err := fmp4.NewDemuxer(file, fmp4.DemuxOptions{Limits: limits, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(description fmp4.TrackDescription) (media.CodecConfig, error) {
		// description даёт стандартные фактические заголовки и размеры; исходный кодировщик
		// задаёт FPS30. Inspector ниже дополнительно проверяет каждую нативную AU.
		if description.Codec != core.CodecAV1 {
			return fmp4.ResolveCodecConfig(description, 1, 1)
		}
		// actual содержит заявленные размеры и проверяемые по последовательности CICP и пиксели.
		actual := core.VideoFormat{Codec: core.CodecAV1, Width: description.Width, Height: description.Height, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: "main", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}}
		// headerErr сообщает о расхождении проверяемых полей с нативным заголовком.
		if headerErr := codec.ValidateAV1Config(description.Headers, actual); headerErr != nil {
			return media.CodecConfig{}, headerErr
		}
		return media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: description.TimeBase, Video: actual}, description.Headers)
	}})
	if err != nil {
		t.Fatal(err)
	}
	// init удерживает неизменяемую пару дорожек, packets — четыре секунды исходных данных.
	init := demux.Init()
	var packets []media.Packet
	// sourceInspector проверяет реальные нативные заголовки и AU до публикации тестового источника.
	var sourceInspector codec.Inspector
	// timing подтверждает прежний строгий допуск DTS до запуска Group, без изменения меток.
	timing := fmp4.NewFFmpegTimingValidatorForOrigin(fmp4.OriginMapping{OffsetTicks: 10, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1}})
	for {
		// packet и readErr содержат очередную полную нативную AU либо конец файла.
		packet, readErr := demux.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		// inspectErr сообщает об отказе фактического сжатого содержимого источника.
		if _, inspectErr := sourceInspector.Inspect(packet); inspectErr != nil {
			t.Fatal(inspectErr)
		}
		// timingErr сообщает о несовместимости исходной временной шкалы с портом FFmpeg.
		if timingErr := timing.Validate(packet); timingErr != nil {
			t.Fatalf("native source timing %s: %+v: %v", packet.Config().Info().Track, packet.Info(), timingErr)
		}
		packets = append(packets, packet)
		if len(packets) > 2048 {
			t.Fatal("native fixture вышла за заданный packet budget")
		}
	}
	if init.Video.Info().Video.Codec != core.CodecAV1 || init.Audio.Info().Audio.Codec != core.CodecAAC {
		t.Fatal("native source не AV1/AAC")
	}
	// kind выбирает фактический выходной кодек при неизменном входе AV1/AAC.
	for _, kind := range []core.Codec{core.CodecOpus, core.CodecFLAC, core.CodecMP3, core.CodecAC3} {
		// t подслучая проверяет отдельный кодек с независимым процессом и соединениями.
		t.Run(string(kind), func(t *testing.T) {
			// profile содержит полный V6 H.264 с перекодированием аудио без замены построителя команды.
			profile := bindingProfile(t, kind)
			// config сохраняет реальные зависимости группы; тестовая очередь вмещает четыре секунды данных.
			config := fixture.config.Runtime
			config.Binary, config.Switcher.Profile = binary, profile
			config.Transcode, config.Player = nil, nil
			config.Switcher.Queue.MaxPackets = 4096
			config.Switcher.Queue.MaxBytes = 16 << 20
			config.Switcher.Queue.ResyncTimeout = 15 * time.Second
			config.Switcher.Router.MaxRetainedBytes = 32 << 20
			config.Switcher.Watchdog.RecoveryWindow = time.Nanosecond
			config.Switcher.Watchdog.VideoTimeout, config.Switcher.Watchdog.AudioTimeout = 10*time.Second, 10*time.Second
			config.FFmpeg.StartupTimeout, config.FFmpeg.VideoTimeout, config.FFmpeg.AudioTimeout = 10*time.Second, 10*time.Second, 10*time.Second
			config.FFmpeg.MaxCommandArgs = 256
			// sockets задаёт короткий путь Darwin UDS; directoryErr сообщает об отказе создания каталога.
			sockets, directoryErr := os.MkdirTemp("/private/tmp", "ga-")
			if directoryErr != nil {
				t.Fatal(directoryErr)
			}
			t.Cleanup(func() {
				// Удаляет только собственный временный каталог после завершения процессов.
				_ = os.RemoveAll(sockets)
			})
			config.FFmpeg.BaseDir = sockets
			// sw выбирает источник, проверяет готовность и преобразует время выходных AU; buildErr хранит отказ.
			sw, buildErr := NewSwitcher(config.Switcher)
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			defer func() {
				// closeErr сообщает об отказе освобождения ресурса sw после проверки.
				if closeErr := sw.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			}()
			// live принимает входные пакеты, source обозначает его текущую публикацию.
			live, buildErr := media.NewRouter(config.Switcher.Router)
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			defer func() {
				// closeErr сообщает об отказе освобождения ресурса live после проверки.
				if closeErr := live.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			}()
			source, buildErr := live.BeginSource()
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			// videoInfo и audioInfo связывают фактические настройки с текущей эпохой источника.
			videoInfo, audioInfo := init.Video.Info(), init.Audio.Info()
			videoInfo.SourceEpoch, audioInfo.SourceEpoch = source.Epoch(), source.Epoch()
			// video и audio сохраняют нативные заголовки и исходную временную шкалу.
			video, buildErr := media.NewCodecConfig(videoInfo, init.Video.Headers())
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			audio, buildErr := media.NewCodecConfig(audioInfo, init.Audio.Headers())
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			if buildErr = source.UpdateConfigs(video, audio); buildErr != nil {
				t.Fatal(buildErr)
			}
			// input и output удерживают вход и выход Group без замены рабочего процесса.
			input, buildErr := live.Subscribe(config.Switcher.Queue)
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			defer func() {
				// closeErr сообщает об отказе освобождения ресурса input после проверки.
				if closeErr := input.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			}()
			output, buildErr := sw.Router().Subscribe(config.Switcher.Queue)
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			defer func() {
				// closeErr сообщает об отказе освобождения ресурса output после проверки.
				if closeErr := output.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			}()
			// packet переносится в текущую публикацию без изменения AU, с общим положительным началом10s.
			for _, packet := range packets {
				// c выбирает фактическую конфигурацию дорожки; info сохраняет исходные метки и длительность.
				c, info := video, packet.Info()
				if packet.Config().Info().Track == media.TrackAudio {
					c = audio
				}
				// offset точно выражает10s в исходной шкале; offsetErr сообщает об отказе представимости.
				offset, offsetErr := rescale(10, media.TimeBase{Numerator: 1, Denominator: 1}, c.Info().TimeBase)
				if offsetErr != nil {
					t.Fatal(offsetErr)
				}
				info.PTS, info.DTS = info.PTS+offset, info.DTS+offset
				// current сохраняет нативные байты и длительность; packetErr сообщает об отказе создания.
				current, packetErr := media.NewPacket(info, c, packet.Payload())
				if packetErr != nil {
					t.Fatal(packetErr)
				}
				if packetErr = source.Publish(current); packetErr != nil {
					t.Fatal(packetErr)
				}
			}
			// owner ограничивает процесс точным текущим выполнением Session.
			owner, buildErr := fixture.config.Core.BindExecutionOwner(ctx, fixture.config.Parent)
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			// observer принимает фактические наблюдения группы и открывает роль перекодирования.
			observer, buildErr := fixture.config.Observer.OpenGroup(ctx, fixture.snapshot.Group.ID, fixture.snapshot.Profile.ID)
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			defer func() {
				// closeErr сообщает об отказе освобождения ресурса observer после проверки.
				if closeErr := observer.Close(context.Background()); closeErr != nil {
					t.Error(closeErr)
				}
			}()
			// g использует стандартные BuildNVIDIACommand и readGeneratedLiveOutput.
			g := &Group{config: config, parent: fixture.config.Parent, owner: owner, observer: observer, id: fixture.snapshot.Group.ID, switcher: sw}
			// attemptCtx ограничивает один процесс; stop запускает прежнюю очистку Supervisor.
			attemptCtx, stop := context.WithTimeout(ctx, 12*time.Second)
			defer stop()
			// result сохраняет итог перекодирования после освобождения принадлежащих процессу ресурсов.
			// completed подтверждает завершение независимо от того, прочитан ли диагностический result.
			result := make(chan error, 1)
			completed := make(chan struct{})
			go func() {
				// Выполняет одну фактическую попытку и сообщает итог после Supervisor.Wait и joinDiagnostics.
				result <- g.transcode(attemptCtx, input, 1)
				close(completed)
			}()
			defer func() {
				// Любой ранний выход отменяет процесс и ждёт фактического освобождения его ресурсов.
				stop()
				select {
				case <-completed:
				case <-time.After(3 * time.Second):
					t.Error("native transcode не подтвердил cleanup")
				}
			}()
			// count считает проверенные аудиопакеты, inspector проверяет их фактические AU и длительность.
			count := 0
			var inspector codec.Inspector
			for count < 8 {
				// delivery и readErr содержат очередной выбранный выходной пакет либо ошибку чтения.
				delivery, readErr := output.Next(attemptCtx)
				if readErr != nil {
					select {
					case buildErr = <-result:
						t.Fatalf("output %s: %v worker=%v status=%+v", kind, readErr, buildErr, sw.Status())
					default:
						t.Fatalf("output %s: %v status=%+v", kind, readErr, sw.Status())
					}
				}
				if delivery.Packet.Config().Info().Track != media.TrackAudio {
					continue
				}
				if delivery.Packet.Config().Info().Audio.Codec != kind || len(delivery.Packet.Config().Headers()) == 0 {
					t.Fatal("output codec подменён desired")
				}
				if _, readErr = inspector.Inspect(delivery.Packet); readErr != nil {
					t.Fatal(readErr)
				}
				if kind == core.CodecFLAC && !delivery.Packet.Config().Info().FLACOwned {
					t.Fatal("native output потерял FLACOwned")
				}
				count++
			}
			stop()
			select {
			case workerErr := <-result:
				// workerErr содержит ожидаемую причину отмены; физическая очистка проверяется отдельно.
				t.Logf("итог отменённой попытки: %v", workerErr)
			case <-time.After(3 * time.Second):
				t.Fatal("worker не подтвердил cleanup после cancellation")
			}
			if g.result != nil && !errors.Is(g.result, context.Canceled) {
				t.Fatal("ошибка физической очистки", g.result)
			}
			if config.Transcode != nil || config.Player != nil || bytes.Equal(audio.Headers(), []byte{}) || !errors.Is(sw.FallbackReady(1, time.Now()), core.ErrNotReady) {
				t.Fatal("native default builder/source contract изменён")
			}
		})
	}
}

// TestNativeGroupDefaultFLACPlayerAcrossLoops проверяет штатный Player=nil и проверенный файл,
// FFmpeg/FD/UDS, непрерывные номера кадров FLAC через две границы петли и отзыв готовности.
// t использует только временный файл и выполнение Session из изолированной SQLite.
func TestNativeGroupDefaultFLACPlayerAcrossLoops(t *testing.T) {
	// binary разрешает отдельный нативный запуск; обычным проверкам установленный FFmpeg не нужен.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("native FLAC player требует GOST_MEDIA_FFMPEG")
	}
	// fixture задаёт точного владельца исполнения и пригодный шаблон manifest с действующими IDs.
	fixture := runtimeUnitFixture(t)
	// profile содержит полный конечный FLAC recipe; limits сохраняет контейнерные бюджеты.
	profile, limits := bindingProfile(t, core.CodecFLAC), fixture.config.Runtime.Container
	// ctx ограничивает подготовку и воспроизведение; cancel освобождает нативные процессы.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// path содержит самостоятельный файл2.4s с25 полными FLAC4608 и72 видеокадрами.
	path := filepath.Join(t.TempDir(), "prepared.mp4")
	// command создаёт нативный артефакт, а stderr и err сообщают об отказе выбранной привязки.
	command := exec.CommandContext(ctx, binary, "-v", "error", "-nostdin", "-copyts", "-f", "lavfi", "-i", "color=c=black:s=128x128:r=30", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo", "-t", "2.4", "-vf", "setparams=range=limited:color_primaries=bt709:color_trc=bt709:colorspace=bt709", "-c:v", "libx264", "-threads:v", "1", "-profile:v", "high", "-bf", "0", "-g", "30", "-b:v", "200000", "-maxrate", "300000", "-bufsize", "400000", "-pix_fmt", "yuv420p", "-c:a", "flac", "-sample_fmt:a", "s16", "-strict", "-2", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-avoid_negative_ts", "disabled", path)
	if stderr, err := command.CombinedOutput(); err != nil {
		t.Fatalf("FLAC artifact: %v %s", err, stderr)
	}
	// data и err содержат точные байты файла и отказ чтения для SHA256 и независимого разбора.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// demux проверяет заголовки и первую AU FLAC без признака происхождения или нормализации тишины.
	demux, err := fmp4.NewDemuxer(bytes.NewReader(data), fmp4.DemuxOptions{Limits: limits, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	// manifest связывает полный проверенный профиль, фактический формат и SHA256 единственного файла.
	manifest := fixture.snapshot.Artifact
	manifest.Profile, manifest.VerifiedFormat = profile, core.MediaFormat{Video: demux.Init().Video.Info().Video, Audio: demux.Init().Audio.Info().Audio}
	manifest.ProfileKey, manifest.CanonicalProfile, err = core.ProfileIdentity(profile)
	if err != nil {
		t.Fatal(err)
	}
	manifest.SizeBytes, manifest.ContentHash = uint64(len(data)), fmt.Sprintf("%x", sha256.Sum256(data))
	if err = manifest.ValidateUsable(); err != nil {
		t.Fatal(err)
	}
	// invalid перебирает подмену SHA256 и полного профиля до стандартного построителя player.
	for _, invalid := range []string{"hash", "profile"} {
		// checkedFile остаётся во владении теста, поскольку команда не запускается.
		checkedFile, openErr := os.Open(path)
		if openErr != nil {
			t.Fatal(openErr)
		}
		// expected и requested моделируют несоответствие выбранного проверенного артефакта.
		expected, requested := manifest, profile
		if invalid == "hash" {
			expected.ContentHash = strings.Repeat("0", 64)
		} else {
			requested.VideoEncoding.GOPFrames++
		}
		// proofErr подтверждает отказ до выполнения FFmpeg; дескриптор закрывает тест.
		_, proofErr := buildVerifiedFLACPlayer(ctx, binary, checkedFile, ffmpeg.Attempt{OutputPath: "/private/tmp/unused-player.sock"}, expected, requested, limits)
		_ = checkedFile.Close()
		if !errors.Is(proofErr, core.ErrCorrupt) {
			t.Fatalf("подмена%s принята: %v", invalid, proofErr)
		}
	}
	// config сохраняет штатные Player и Transcode, задавая лишь изолированные тестовые сроки.
	config := fixture.config.Runtime
	config.Switcher.Profile, config.Binary, config.Player, config.Transcode = profile, binary, nil, nil
	config.Switcher.Queue.ResyncTimeout = 15 * time.Second
	config.FFmpeg.StartupTimeout = 10 * time.Second
	config.FFmpeg.VideoTimeout, config.FFmpeg.AudioTimeout = 10*time.Second, 10*time.Second
	config.FFmpeg.MaxCommandArgs = 256
	// directory содержит короткий путь Darwin UDS и удаляется после завершения процесса.
	directory, err := os.MkdirTemp("/private/tmp", "gp-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// cleanupErr сообщает об отказе удаления только собственного временного каталога.
		if cleanupErr := os.RemoveAll(directory); cleanupErr != nil {
			t.Error(cleanupErr)
		}
	}()
	config.FFmpeg.BaseDir = directory
	config.OpenFile = func(context.Context, core.ArtifactManifest) (*os.File, error) {
		// Возвращает тестовый FD; проигрыватель сам проверяет SHA256, полный профиль и фактический FLAC.
		return os.Open(path)
	}
	// sw выбирает источник заглушки и предоставляет выходной Router.
	sw, err := NewSwitcher(config.Switcher)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// closeErr сообщает об отказе освобождения ресурса sw после проверки.
		if closeErr := sw.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	// output получает фактические AU штатного нативного проигрывателя после пересчёта времени.
	output, err := sw.Router().Subscribe(config.Switcher.Queue)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// closeErr сообщает об отказе освобождения ресурса output после проверки.
		if closeErr := output.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	// owner связывает процесс с точным выполнением Session.
	owner, err := fixture.config.Core.BindExecutionOwner(ctx, fixture.config.Parent)
	if err != nil {
		t.Fatal(err)
	}
	// observer предоставляет настоящие наблюдения роли и попыток FFmpeg.
	observer, err := fixture.config.Observer.OpenGroup(ctx, fixture.snapshot.Group.ID, fixture.snapshot.Profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// closeErr сообщает об отказе освобождения ресурса observer после проверки.
		if closeErr := observer.Close(context.Background()); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	// g запускает штатный проигрыватель с manifest и подтверждает очистку через done.
	g := &Group{config: config, parent: fixture.config.Parent, owner: owner, observer: observer, id: fixture.snapshot.Group.ID, switcher: sw, manifest: manifest}
	// playerCtx и stop ограничивают физическую попытку кодировщика через прежний Supervisor.
	playerCtx, stop := context.WithCancel(ctx)
	defer stop()
	// done закрывается только после полного g.play и joinDiagnostics.
	done := make(chan struct{})
	go func() {
		// Воспроизводит несколько петель и подтверждает физическое завершение после отмены.
		g.play(playerCtx)
		close(done)
	}()
	defer func() {
		// Останавливает проигрыватель и ждёт освобождения FD/UDS даже при раннем t.Fatal.
		stop()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("native player не подтвердил cleanup")
		}
	}()
	// count проверяет55 кадров: больше двух полных периодов по25 FLAC AU.
	count := uint64(0)
	for count < 55 {
		// delivery и readErr содержат следующий выбранный пакет либо ошибку срока или порта.
		delivery, readErr := output.Next(ctx)
		if readErr != nil {
			t.Fatalf("FLAC player: %v status=%+v", readErr, sw.Status())
		}
		if delivery.Packet.Config().Info().Track != media.TrackAudio {
			continue
		}
		// frame и frameErr проверяют фактические CRC, заголовок и непрерывный номер кадра.
		frame, frameErr := codec.FLACFrameInfo(delivery.Packet.Config().Headers(), delivery.Packet.Payload())
		if frameErr != nil || frame.Number != count || frame.Samples != 4608 || !delivery.Packet.Config().Info().FLACOwned {
			t.Fatalf("frame%d: %+v err=%v owned=%t", count, frame, frameErr, delivery.Packet.Config().Info().FLACOwned)
		}
		count++
	}
	if g.Ready() != nil || sw.Status().Source != Fallback {
		t.Fatal("default player не достиг actual готовности")
	}
	stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("native player не завершился")
	}
	if g.Ready() == nil || g.result != nil || config.Player != nil || config.Transcode != nil {
		t.Fatal("cleanup/readiness/default builder contract нарушен", g.result)
	}
}
