package integration

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// Задаёт CPU-сценарий с выбранными кодеком и частотой кадров: SVT-AV1 VBR без ограничителя потока либо
// H.264 с двумя B-кадрами.
func encodingPipelineProfile(codec core.Codec, rate core.FrameRate) core.GroupProfile {
	// p содержит профиль подготовки, дополненный частотой кадров и параметрами выбранного сценария
	// кодирования.
	p := fallbackPipelineProfile(codec)
	p.Format.Video.FrameRate = rate
	// encoder задаёт кодировщик профиля: libsvtav1 для AV1 либо заменяемый в CPU-команде h264_nvenc.
	encoder := "h264_nvenc"
	if codec == core.CodecAV1 {
		encoder = "libsvtav1"
		p.VideoEncoding.MaxBitrateBPS = 0
		p.VideoEncoding.BufferBits = 0
	} else {
		p.VideoEncoding.MaxBFrames = 2
	}
	p.Encoding = core.DefaultEncoding(p.Mode, encoder, "vbr")
	if codec == core.CodecAV1 {
		p.Encoding.VideoCustom = "lp=1"
	}
	return p
}

// Только H.264 заменяется CPU-кодировщиком; AV1 использует рабочие параметры SVT-AV1 VBR.
func encodingPipelineCPUBuilder(binary string) fallback.CommandBuilder {
	return func(req fallback.GenerateRequest) (fallback.Command, error) {
		// Сохраняет рабочие параметры SVT-AV1, заменяя только NVENC параметрами CPU-кодирования H.264.

		// c содержит штатную команду подготовки варианта, а err сообщает об ошибке её
		// построения.
		c, err := fallback.ProductionBuilder(binary)(req)
		if err != nil || req.Profile.Format.Video.Codec == core.CodecAV1 {
			return c, err
		}
		// args накапливает аргументы H.264 без параметров NVENC перед добавлением CPU-настроек.
		var args []string
		// i указывает позицию обрабатываемого аргумента штатной команды; значения пропускаемых
		// параметров также исключаются.
		for i := 0; i < len(c.Args); i++ {
			switch c.Args[i] {
			case "-rc", "-preset", "-tune", "-multipass", "-rc-lookahead", "-spatial-aq", "-temporal-aq", "-aq-strength", "-no-scenecut", "-forced-idr":
				i++
				continue
			}
			// arg содержит текущий сохраняемый аргумент с заменой h264_nvenc на libx264.
			arg := c.Args[i]
			if arg == "h264_nvenc" {
				arg = "libx264"
			}
			args = append(args, arg)
		}
		// output сохраняет выходной путь, чтобы добавить CPU-настройки перед ним.
		output := args[len(args)-1]
		args = args[:len(args)-1]
		c.Args = append(args, "-sc_threshold", "0", "-threads:v", "1", output)
		return c, nil
	}
}

// Демультиплексирует подготовленный fMP4 в последовательности пакетов по дорожкам, используя
// проверенный формат варианта для конфигураций. Ошибка контейнера прерывает чтение без частичного
// результата.
func encodingPreparedPackets(data []byte, format core.MediaFormat) (map[media.Track][]media.Packet, error) {
	// d читает пакеты подготовленного fMP4, а err сообщает об ошибке создания демультиплексора.
	d, err := fmp4.NewDemuxer(bytes.NewReader(data), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		// Связывает дорожки подготовленного контейнера с проверенным форматом варианта и сохраняет их
		// заголовки.

		// info связывает описание дорожки контейнера с проверенным форматом варианта и временной
		// базой.
		info := media.CodecConfigInfo{Track: desc.Track, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase}
		if desc.Track == media.TrackVideo {
			info.Video = format.Video
		} else {
			info.Audio = format.Audio
		}
		return media.NewCodecConfig(info, desc.Headers)
	}})
	if err != nil {
		return nil, err
	}
	// result собирает пакеты подготовленного варианта отдельно для видео и аудио.
	result := map[media.Track][]media.Packet{}
	for {
		// p содержит очередной пакет подготовленного файла, а e сообщает об окончании или ошибке
		// чтения контейнера.
		p, e := d.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return nil, e
		}
		// track определяет дорожку, в последовательность которой добавляется прочитанный пакет.
		track := p.Config().Info().Track
		result[track] = append(result[track], p)
	}
	return result, nil
}

// При заданном GOST_MEDIA_FFMPEG проверяет подготовку и три петли AV1 VBR и H.264 с B-кадрами через
// файловый проигрыватель и Group. Пакеты сохраняют содержимое и временные отношения с общим сдвигом
// A/V; сценарий H.264 с опережением 160 мс должен быть отклонён при MaxTransitionGap=100ms.
func TestEncodingProfilesPreparedManagerFDPlayerGroupLoops(t *testing.T) {
	// binary содержит явно заданный путь FFmpeg для необязательной CPU-проверки подготовки и
	// проигрывания.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("локальная CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	// scenario задаёт кодек, частоту кадров и ожидаемый результат выбранного CPU-сценария.
	for _, scenario := range []struct {
		// name задаёт имя подтеста соответствующего профиля и временного режима.
		name string
		// codec выбирает AV1 VBR либо H.264 с B-кадрами для подготовки варианта.
		codec core.Codec
		// rate задаёт рациональную частоту кадров подготовленного видео.
		rate core.FrameRate
		// reject показывает, должен ли Group отклонить перестановку кадров, превышающую бюджет
		// перехода.
		reject bool
	}{
		{"av1-vbr", core.CodecAV1, core.FrameRate{Numerator: 30, Denominator: 1}, false},
		{"h264-b", core.CodecH264, core.FrameRate{Numerator: 30, Denominator: 1}, false},
		{"av1-vbr-rational", core.CodecAV1, core.FrameRate{Numerator: 25, Denominator: 2}, false},
		{"h264-b-rational", core.CodecH264, core.FrameRate{Numerator: 75, Denominator: 2}, false},
		{"h264-b-reorder-exceeds-budget", core.CodecH264, core.FrameRate{Numerator: 25, Denominator: 2}, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// Проверяет выбранные кодек и дробную частоту кадров на трёх петлях настоящего проигрывателя либо
			// отказ при превышении допустимой перестановки.

			// ctx ограничивает выполнение CPU-сценария тридцатью секундами, а cancel
			// освобождает его таймер при завершении.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// options задаёт бюджеты выполнения и наблюдений, пул соединений и ограниченные
			// повторы временной SQLite.
			options := storage.Options{PreparationLimits: core.PreparationLimits{MaxQueued: 3, MaxProfiles: 8}, OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 128, MaxIdentifierBytes: 1024, MaxBytes: 4 << 20}, ObservationLimits: core.ObservationLimits{MaxSessions: 8, MaxGroups: 32, MaxOutputs: 64, MaxFFmpegWorkers: 32, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond}
			// store содержит временное файловое хранилище сценария, а err сообщает об ошибке
			// его открытия.
			store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "pipeline.sqlite"), options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает временное хранилище профиля после проверки подготовленного медиаварианта.

				// err сообщает об ошибке закрытия временного хранилища профиля.
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			})
			// svc предоставляет доменные операции подготовки и запуска сессии поверх
			// временного хранилища.
			svc, err := core.NewService(store, store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			// asset содержит заглушку, в которую загружается синтетическое CPU-изображение.
			asset, err := svc.CreateAsset(ctx, "CPU-заглушка")
			if err != nil {
				t.Fatal(err)
			}
			// watchdog задаёт сроки ожидания аудио, видео, восстановления и ключевого кадра
			// проверяемой группы.
			watchdog := core.WatchdogPolicy{VideoTimeout: 2 * time.Second, AudioTimeout: 2 * time.Second, RecoveryWindow: 100 * time.Millisecond, KeyframeTimeout: time.Second}
			// stream содержит поток с заглушкой и заданными сроками контроля источника.
			stream, err := svc.CreateStream(ctx, core.Stream{Name: "CPU-поток", PublicationKey: "cpu-fallback-key", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: watchdog}})
			if err != nil {
				t.Fatal(err)
			}
			// profile содержит сохранённый профиль выбранного кодека и рациональной частоты
			// кадров.
			profile, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "CPU-профиль", Profile: encodingPipelineProfile(scenario.codec, scenario.rate)})
			if err != nil {
				t.Fatal(err)
			}
			// configured содержит сохранённую группу, связывающую тестовый поток с профилем.
			configured, err := svc.CreateGroup(ctx, core.TranscodeGroup{Name: "CPU-группа", StreamID: stream.ID, ProfileID: profile.ID})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = svc.CreateDestination(ctx, core.Destination{Name: "CPU-выход", StreamID: stream.ID, GroupID: configured.ID, Platform: "custom", Endpoint: "rtmp://example.invalid/live", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			// files хранит исходное изображение и подготовленный вариант в отдельном
			// временном каталоге.
			files, err := fallback.OpenFileStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Освобождает файловое хранилище после остановки подготовки и проигрывания.

				// err сообщает об ошибке закрытия файлового хранилища после
				// медиапроверки.
				if err := files.Close(); err != nil {
					t.Error(err)
				}
			})
			// runner запускает подготовку с ограничениями времени, аргументов и
			// диагностического вывода.
			runner := fallback.Runner{Limits: fallback.RunnerLimits{Timeout: 10 * time.Second, MaxArgs: 256, MaxCommandBytes: 1 << 16, MaxDiagnosticBytes: 1 << 20}}
			// probeLimits ограничивает проверку изображения, контейнера и пакетов
			// подготовленного варианта.
			probeLimits := fallback.ProbeLimits{MaxJSONBytes: 1 << 20, MaxWidth: 1024, MaxHeight: 1024, MaxPixels: 1024 * 1024, MaxDuration: 10 * time.Second, ImageDuration: 1600 * time.Millisecond, MaxPackets: 4096, Container: integrationLimits()}
			// actualRunner передаёт команды рабочему runner с диагностикой теста при
			// отказах.
			actualRunner := fallbackPipelineRunner{t: t, runner: runner}
			// probe проверяет подготовленный вариант через ffprobe рядом с заданным FFmpeg.
			probe := fallback.Prober{Binary: filepath.Join(filepath.Dir(binary), "ffprobe"), Runner: actualRunner, Limits: probeLimits}
			// config задаёт версию генератора и бюджеты загрузки, подготовки и завершения
			// менеджера заглушек.
			config := fallback.Config{ReconcileLimit: 10, GeneratorVersion: "cpu-integration", FFmpeg: binary, FFprobe: probe.Binary, MaxUploadBytes: 1 << 20, MaxOutputBytes: 8 << 20, PrepareTimeout: 15 * time.Second, ShutdownTimeout: 3 * time.Second, QueueInterval: time.Millisecond, Runner: runner.Limits, Probe: probeLimits}
			// builder строит CPU-команды, сохраняя штатные параметры AV1 и заменяя NVENC для
			// H.264.
			builder := encodingPipelineCPUBuilder(binary)
			// manager выполняет загрузку, генерацию и публикацию варианта с рабочими
			// инструментами проверки.
			manager, err := fallback.NewManagerWithTools(svc, files, config, fallback.Tools{Builder: builder, Inspector: probe, Runner: actualRunner, Normalize: probe.NormalizePrepared, VerifyLoopDuration: probe.VerifyLoopDuration})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает менеджер подготовки после загрузки тестового изображения.
				_ = manager.Close(context.Background())
			})
			// img содержит синтетическое непрозрачное изображение для подготовки
			// медиазаглушки.
			img := image.NewRGBA(image.Rect(0, 0, 128, 128))
			// y задаёт строку пикселей синтетического изображения.
			for y := range 128 {
				// x задаёт столбец пикселя и красную составляющую синтетического
				// изображения.
				for x := range 128 {
					img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 40, A: 255})
				}
			}
			// pngData накапливает PNG-представление изображения для загрузки через менеджер.
			var pngData bytes.Buffer
			if err = png.Encode(&pngData, img); err != nil {
				t.Fatal(err)
			}
			// job представляет принятую задачу загрузки и подготовки изображения в нужном
			// профиле.
			job, err := manager.Upload(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, io.NopCloser(bytes.NewReader(pngData.Bytes())))
			if err != nil {
				t.Fatal(err)
			}
			// result содержит опубликованное поколение и результат подтверждения очистки
			// подготовки.
			result, err := job.Wait(ctx)
			if err != nil || !result.Clean {
				t.Fatalf("рабочие upload/generator/probe/publication: generation=%s clean=%v error=%v", result.Generation.ID, result.Clean, err)
			}
			if len(result.Generation.RequiredProfiles) != 1 {
				t.Fatal("не подготовлен точный профиль")
			}
			// prepared содержит единственную подготовленную привязку профиля к варианту
			// поколения.
			prepared := result.Generation.RequiredProfiles[0]
			// variant содержит сохранённые размер, хеш, путь и проверенный формат
			// подготовленного файла.
			variant, err := svc.GetVariant(ctx, prepared.VariantID)
			if err != nil {
				t.Fatal(err)
			}
			// preparedFile открывает точный подготовленный файл с проверкой размера и хеша.
			preparedFile, err := files.OpenExact(ctx, variant.ManagedPath, variant.SizeBytes, variant.ContentHash)
			if err != nil {
				t.Fatal(err)
			}
			// preparedBytes содержит байты подготовленного варианта, а readErr сообщает об
			// ошибке их чтения.
			preparedBytes, readErr := io.ReadAll(preparedFile)
			// closeErr сообщает об ошибке закрытия дескриптора подготовленного варианта.
			closeErr := preparedFile.Close()
			if readErr != nil || closeErr != nil {
				t.Fatal(errors.Join(readErr, closeErr))
			}
			// expected содержит эталонные пакеты каждой дорожки из подготовленного fMP4.
			expected, err := encodingPreparedPackets(preparedBytes, variant.VerifiedFormat)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.reject {
				// first содержит первый видеопакет сценария отказа для проверки
				// опережения декодирования на 160 мс.
				first := expected[media.TrackVideo][0]
				if first.Info().PTS != 0 || fallbackActualTime(first.Info().DTS, first.Config().Info().TimeBase).Cmp(big.NewRat(-4, 25)) != 0 {
					t.Fatal("фикстура не подтвердила reorder lead 160ms", first.Info())
				}
			}
			// loop задаёт эффективную длительность одной петли с учётом выбранной частоты
			// кадров.
			loop, err := fallback.EffectiveLoopDuration(probeLimits.ImageDuration, profile.Profile, probeLimits.MaxDuration)
			if err != nil {
				t.Fatal(err)
			}
			// request связывает запуск сессии с точным подготовленным поколением, вариантом
			// и версией генератора.
			request := core.StartSessionRequest{RequestID: "cpu-pipeline-start", StreamID: stream.ID, Bindings: []core.PreparedBinding{{GroupID: configured.ID, GenerationID: result.Generation.ID, VariantID: prepared.VariantID, GeneratorVersion: config.GeneratorVersion}}}
			// start содержит квитанцию запуска сессии для выдачи разрешения родительского
			// выполнения.
			start, err := svc.StartSession(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			// parent содержит разрешение сессии, от имени которой создаются наблюдатель и
			// реестр групп.
			parent, err := svc.Claim(ctx, start.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if err = svc.MarkIssued(ctx, parent); err != nil {
				t.Fatal(err)
			}
			if err = svc.Complete(ctx, parent, core.Completion{Success: true}); err != nil {
				t.Fatal(err)
			}
			// observer публикует наблюдения групп в рамках родительского выполнения сессии.
			observer, err := svc.OpenSessionObserver(ctx, parent)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает наблюдателя Session после остановки групп.
				_ = observer.Close(context.Background())
			})
			// outputs содержит сохранённый снимок единственного выхода для получения ссылки
			// на группу.
			outputs, err := svc.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 10, SessionID: parent.SessionID}})
			if err != nil || len(outputs.Items) != 1 {
				t.Fatalf("снимок выхода: %+v, %v", outputs, err)
			}

			// parentDirectory задаёт родительский каталог короткого пути UDS; на macOS
			// используется /tmp.
			parentDirectory := os.TempDir()
			if runtime.GOOS == "darwin" {
				parentDirectory = "/tmp"
			}
			// base задаёт отдельный короткий каталог сокетов и рабочих файлов FFmpeg для
			// сценария.
			base, err := os.MkdirTemp(parentDirectory, "fallback-cpu-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Удаляет короткий каталог UDS после завершения медиапроцессов.

				// err сообщает об ошибке удаления короткого каталога медиапроцессов.
				if err := os.RemoveAll(base); err != nil {
					t.Error(err)
				}
			})
			// queue ограничивает число и размер пакетов подписки и срок повторной
			// синхронизации.
			queue := media.QueueLimits{MaxPackets: 512, MaxBytes: 8 << 20, MaxPacketBytes: 1 << 20, MaxConfigBytes: 1 << 12, ResyncTimeout: 5 * time.Second}
			// routerLimits ограничивает число потребителей и удерживаемые байты
			// медиамаршрутизаторов.
			routerLimits := media.RouterLimits{MaxConsumers: 4, MaxRetainedBytes: 40 << 20, MaxConfigBytes: 1 << 12}
			// live предоставляет неиспользуемый вход LIVE, пока группа проигрывает
			// подготовленную заглушку.
			live, err := media.NewRouter(routerLimits)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает неиспользуемый вход LIVE после освобождения группы.
				_ = live.Close()
			})
			// runtimeConfig задаёт проигрыватель, медиабюджеты и допустимые сдвиги времени
			// для рабочей группы.
			runtimeConfig := group.RuntimeConfig{Switcher: group.SwitcherConfig{Profile: profile.Profile, Watchdog: watchdog, Queue: queue, Router: routerLimits, MaxAVSkew: 100 * time.Millisecond, MaxTransitionGap: 100 * time.Millisecond}, Binary: binary, OpenFile: files.OpenVerified, Container: integrationLimits(), ResubscribeBackoff: time.Millisecond, ControlInterval: 10 * time.Millisecond, ShutdownTimeout: 3 * time.Second, FFmpeg: ffmpeg.Config{BaseDir: base, StartupTimeout: 5 * time.Second, VideoTimeout: 2 * time.Second, AudioTimeout: 2 * time.Second, WatchdogInterval: 20 * time.Millisecond, RestartBackoff: 10 * time.Millisecond, GracePeriod: 100 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 3 * time.Second, DiagnosticEvents: 128, DiagnosticBytes: 1 << 20, DiagnosticChunkBytes: 1024, MaxCommandBytes: 8192, MaxCommandArgs: 128}}
			// registry создаёт и удерживает группы с точным родительским разрешением сессии.
			registry, err := group.NewRegistry(ctx, group.RegistryConfig{Core: svc, Parent: parent, Observer: observer, Runtime: runtimeConfig, MaxGroups: 4, MaxReferences: 4})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает реестр групп и их проигрывателей перед удалением файлов и каталога.

				// err сообщает об ошибке завершения реестра групп и его проигрывателей.
				if err := registry.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			// ref удерживает проверяемую группу и предоставляет её выходной маршрутизатор.
			ref, err := registry.Acquire(ctx, outputs.Items[0].Snapshot, live)
			if err != nil {
				t.Fatal(err)
			}
			// sub читает выходные пакеты группы для сравнения трёх петель с исходным
			// вариантом.
			sub, err := ref.Router().Subscribe(queue)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				// Закрывает подписку на выходные пакеты группы после проверки петель.
				_ = sub.Close()
			}()

			// counts считает проверенные пакеты отдельно по дорожкам для охвата трёх полных
			// петель.
			counts := map[media.Track]int{}
			// shift сохраняет общий рациональный сдвиг времени A/V, определённый по первому
			// выходному пакету.
			var shift *big.Rat
			// epoch фиксирует поколение источника первого пакета для проверки его сохранения
			// между петлями.
			var epoch uint64
			for counts[media.TrackVideo] < 3*len(expected[media.TrackVideo]) || counts[media.TrackAudio] < 3*len(expected[media.TrackAudio]) {
				// delivery содержит следующую доставку выхода группы, а err сообщает об
				// ошибке или истечении срока её ожидания.
				delivery, err := sub.Next(ctx)
				if err != nil {
					if scenario.reject && !ref.Group().Status().Available && errors.Is(ref.Group().Status().Err, group.ErrTiming) {
						t.Log("H.264 reorder lead 2×2/25 с превышает неизменный MaxTransitionGap=100ms; Group отказал")
						// closeErr сообщает об ошибке освобождения ссылки на
						// группу после ожидаемого отказа по времени.
						if closeErr := ref.Close(); closeErr != nil && !errors.Is(closeErr, context.Canceled) {
							t.Fatal(closeErr)
						}
						return
					}
					t.Fatalf("FD-player/UDS/Group: %v status=%+v", err, ref.Group().Status())
				}
				if !delivery.IsCurrent() {
					continue
				}
				// packet содержит актуальный выходной пакет группы для сравнения с
				// подготовленным вариантом.
				packet := delivery.Packet
				// track определяет дорожку проверяемого пакета и соответствующий
				// эталонный ряд.
				track := packet.Config().Info().Track
				if epoch == 0 {
					epoch = packet.Config().Info().SourceEpoch
				}
				if packet.Config().Info().SourceEpoch != epoch {
					t.Fatal("петля сменила epoch")
				}
				// pattern содержит последовательность пакетов одной дорожки за одну
				// подготовленную петлю.
				pattern := expected[track]
				// index задаёт порядковый номер проверяемого пакета дорожки с учётом
				// предыдущих петель.
				index := counts[track]
				// original содержит эталонный пакет на соответствующей позиции внутри
				// одной петли.
				original := pattern[index%len(pattern)]
				// pi содержит временные метки и флаги выходного пакета, а oi —
				// соответствующие свойства эталонного пакета.
				pi, oi := packet.Info(), original.Info()
				if pi.DurationTicks <= 0 || !bytes.Equal(packet.Payload(), original.Payload()) || pi.Keyframe != oi.Keyframe {
					t.Fatalf("изменена единица доступа %s[%d]", track, index)
				}
				// base задаёт временную базу выходного пакета, а originalBase —
				// временную базу эталона для сравнения в секундах.
				base, originalBase := packet.Config().Info().TimeBase, original.Config().Info().TimeBase
				// offset содержит рациональное смещение начала текущей петли
				// относительно первой в секундах.
				offset := new(big.Rat).Mul(new(big.Rat).SetInt64(int64(index/len(pattern))), big.NewRat(int64(loop), int64(time.Second)))
				// currentShift содержит сдвиг DTS выходного пакета относительно эталона
				// после вычитания временного смещения текущей петли.
				currentShift := new(big.Rat).Sub(fallbackActualTime(pi.DTS, base), fallbackActualTime(oi.DTS, originalBase))
				currentShift.Sub(currentShift, offset)
				if shift == nil {
					shift = currentShift
				}
				if shift.Cmp(currentShift) != 0 {
					t.Fatalf("непостоянный общий A/V shift %s[%d]: %s != %s", track, index, currentShift, shift)
				}
				// wantPTS содержит ожидаемый PTS эталонного пакета с учётом петли и
				// общего сдвига A/V.
				wantPTS := new(big.Rat).Add(fallbackActualTime(oi.PTS, originalBase), offset)
				wantPTS.Add(wantPTS, shift)
				if fallbackActualTime(pi.PTS, base).Cmp(wantPTS) != 0 || fallbackActualTime(pi.DurationTicks, base).Cmp(fallbackActualTime(oi.DurationTicks, originalBase)) != 0 {
					t.Fatalf("изменены PTS/CTS/duration %s[%d]: %+v != %+v", track, index, pi, oi)
				}
				counts[track]++
			}
			if scenario.reject {
				t.Fatal("Group принял reorder lead больше MaxTransitionGap")
			}
			if ref.Group().Status().Source != group.Fallback {
				t.Fatal(ref.Group().Status())
			}
			// video содержит эталонную видеодорожку для подтверждения отрицательного DTS и
			// перестановки H.264.
			video := expected[media.TrackVideo]
			if scenario.codec == core.CodecH264 && (video[0].Info().DTS >= 0 || video[0].Info().PTS != 0) {
				t.Fatal("не проверены отрицательное начало и перестановка H.264")
			}
			t.Logf("точные границы петли: video=%d audio=%d period=%s common_shift=%s", counts[media.TrackVideo], counts[media.TrackAudio], loop, shift)
			if err = ref.Close(); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
