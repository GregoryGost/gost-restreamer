package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/config"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
	"github.com/GregoryGost/gost-restreamer/internal/session"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// Возвращает три профиля сценария: AV1 и H.264 1440p, а также H.264 1080p.
func applicationFlowProfiles() []core.GroupProfile {
	return []core.GroupProfile{publisherProfile(core.CodecAV1, 2560, 1440), publisherProfile(core.CodecH264, 2560, 1440), publisherProfile(core.CodecH264, 1920, 1080)}
}

// Подстановка ограничена кодированием; файлы, очереди, публикация и проверка AU остаются настоящими.
// applicationFlowGenerator заменяет кодирование управляемыми структурными fMP4-пакетами, сохраняя
// рабочую файловую подготовку.
type applicationFlowGenerator struct { /* t передаёт тестовую проверку ошибок при создании синтетических конфигураций дорожек. */
	t *testing.T
}

// Run проверяет изображение и переданный JSON-профиль, затем записывает синтетический fMP4 из 96
// видеопакетов и 75 AAC-пакетов. Кодирование заменено управляемыми данными; ошибки контекста, входа и
// мультиплексора возвращаются вызывающему коду.
func (g applicationFlowGenerator) Run(ctx context.Context, command fallback.Command, input *os.File, output io.Writer) (fallback.RunResult, error) {
	// err содержит причину отмены задания до чтения исходного изображения.
	if err := ctx.Err(); err != nil {
		return fallback.RunResult{}, err
	}
	// err содержит отказ разбора параметров исходного изображения перед генерацией.
	if _, _, err := image.DecodeConfig(input); err != nil {
		return fallback.RunResult{}, err
	}
	if len(command.Args) != 1 {
		return fallback.RunResult{}, core.ErrInvalid
	}
	// profile принимает полный профиль из единственного JSON-аргумента управляемой команды.
	var profile core.GroupProfile
	// err содержит отказ разбора JSON-профиля управляемого генератора.
	if err := json.Unmarshal([]byte(command.Args[0]), &profile); err != nil {
		return fallback.RunResult{}, err
	}
	// v содержит синтетическую видеоконфигурацию профиля; a содержит согласованную AAC-конфигурацию
	// эпохи 1.
	v, a := publisherConfigs(g.t, profile, 1)
	// mux записывает структурные пакеты в выходной fMP4; err содержит отказ создания мультиплексора.
	mux, err := fmp4.NewMuxer(output, fmp4.Init{Video: v, Audio: a}, integrationLimits())
	if err != nil {
		return fallback.RunResult{}, err
	}
	// audio содержит следующую AAC-метку в шкале 1/48000 и увеличивается по 1024 отсчёта.
	var audio int64
	// video задаёт очередную из 96 видеометок в шкале частоты кадров тестового профиля.
	for video := range int64(96) {
		// packet содержит текущую единицу доступа видео или аудио; err содержит отказ её
		// создания для записи в контейнер.
		packet, err := media.NewPacket(media.PacketInfo{PTS: video, DTS: video, DurationTicks: 1, Keyframe: true}, v, publisherPayload(v, 10))
		if err != nil {
			return fallback.RunResult{}, err
		}
		if err = mux.WritePacket(packet); err != nil {
			return fallback.RunResult{}, err
		}
		for audio*60 < (video+1)*48000 {
			packet, err = media.NewPacket(media.PacketInfo{PTS: audio, DTS: audio, DurationTicks: 1024}, a, publisherPayload(a, 0))
			if err != nil {
				return fallback.RunResult{}, err
			}
			if err = mux.WritePacket(packet); err != nil {
				return fallback.RunResult{}, err
			}
			audio += 1024
		}
	}
	return fallback.RunResult{}, nil
}

// applicationFlowInspection связывает настоящий анализ исходника с проверкой структурных AU управляемого
// fMP4.
type applicationFlowInspection struct { /* probe выполняет реальный анализ исходного изображения в заданных пределах. */
	probe fallback.Prober
}

// Source передаёт проверку исходного файла настоящему Prober, сохраняя его результат и ошибку.
func (i applicationFlowInspection) Source(ctx context.Context, file *os.File, kind core.AssetKind) (fallback.Source, error) {
	return i.probe.Source(ctx, file, kind)
}

// Перематывает файл и открывает fMP4 с разрешением AV1-конфигурации по размерам тестового профиля.
// Остальные дорожки используют штатный ResolveCodecConfig.
func applicationFlowDemux(file *os.File) (*fmp4.Demuxer, error) {
	// err содержит отказ перемотки подготовленного файла перед открытием демультиплексора.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return fmp4.NewDemuxer(file, fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(d fmp4.TrackDescription) (media.CodecConfig, error) {
		// Разбирает обычные конфигурации дорожек штатным способом, а для структурного AV1 использует формат
		// тестового профиля.

		if d.Codec != core.CodecAV1 {
			return fmp4.ResolveCodecConfig(d, 1, 1)
		}
		// p задаёт тестовое полное описание AV1 по фактическим размерам заголовка контейнера.
		p := publisherProfile(core.CodecAV1, d.Width, d.Height)
		return media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: d.TimeBase, Video: p.Format.Video}, d.Headers)
	}})
}

// Verify проверяет совместимость формата и каждую единицу доступа подготовленного файла; ожидает ровно
// 96 видеопакетов и 75 аудиопакетов. Ошибка чтения, проверки или отмена прерывает проверку, а неверное
// количество пакетов возвращает ErrNotReady.
func (i applicationFlowInspection) Verify(ctx context.Context, file *os.File, profile core.GroupProfile) (core.MediaFormat, error) {
	// d содержит демультиплексор подготовленного файла; err содержит отказ его открытия.
	d, err := applicationFlowDemux(file)
	if err != nil {
		return core.MediaFormat{}, err
	}
	// actual содержит фактические описания обеих дорожек контейнера для проверки совместимости с
	// профилем.
	actual := core.MediaFormat{Video: d.Init().Video.Info().Video, Audio: d.Init().Audio.Info().Audio}
	if err = codec.Compatible(actual, profile.Format); err != nil {
		return actual, err
	}
	// inspector проверяет структуру каждой единицы доступа подготовленного файла.
	var inspector codec.Inspector
	// video считает видеопакеты; audio считает аудиопакеты для проверки полного ожидаемого комплекта
	// 96/75.
	var video, audio int
	for {
		// err содержит причину отмены проверки подготовленных AU.
		if err := ctx.Err(); err != nil {
			return actual, err
		}
		// p содержит очередной пакет контейнера; err содержит отказ чтения или EOF для окончания
		// проверки.
		p, err := d.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return actual, err
		}
		if _, err = inspector.Inspect(p); err != nil {
			return actual, err
		}
		if p.Config().Info().Track == media.TrackVideo {
			video++
		} else {
			audio++
		}
	}
	if video != 96 || audio != 75 {
		return actual, core.ErrNotReady
	}
	return actual, nil
}

// Кодирует изображение 2×2 в PNG для загрузки через рабочую подготовку; ошибка кодирования завершает
// тест.
func applicationFlowPNG(t *testing.T) []byte {
	t.Helper()
	// out собирает корректный PNG исходного изображения 2×2 в памяти.
	var out bytes.Buffer
	// err содержит отказ PNG-кодирования изображения для рабочей загрузки.
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// Создаёт и запускает изолированные хранилище, подготовку, менеджер сессий и сервер приёма RTMP с
// управляемыми медиапроцессами. Освобождение ресурсов и удаление временного каталога зарегистрированы
// в t.Cleanup.
func newApplicationFlowFixture(t *testing.T, helper string) *sessionAcceptanceFixture {
	t.Helper()
	// base выделяет короткий частный каталог полной фикстуры; err содержит отказ его создания.
	base, err := os.MkdirTemp("/tmp", "application-flow-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Удаляет частный каталог полного сценария после освобождения файлов и процессов.

		// err содержит отказ удаления частного каталога после завершения всех владельцев.
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	// f сохраняет общие ресурсы, менеджер и диагностику полного управляемого сценария.
	f := &sessionAcceptanceFixture{directory: base}
	f.store, err = storage.Open(context.Background(), filepath.Join(base, "state.sqlite"), sessionAcceptanceOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает временное хранилище полного сценария после зависимых компонентов.

		// err содержит результат закрытия общего Store фикстуры.
		if err := f.store.Close(); err != nil {
			t.Error(err)
		}
	})
	f.service, err = core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	f.files, err = fallback.OpenFileStore(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Освобождает файловое хранилище подготовленных заглушек.

		// err содержит результат закрытия файлового хранилища заглушек.
		if err := f.files.Close(); err != nil {
			t.Error(err)
		}
	})
	f.processes, err = processowner.New(processowner.Config{Root: filepath.Join(base, "p"), RuntimeRoot: filepath.Join(base, "r"), HelperPath: helper, GracePeriod: 50 * time.Millisecond, Limits: sessionAcceptanceProcessLimits()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает реестр владения тестовыми процессами после завершения медиакомпонентов.

		// err содержит результат закрытия реестра физических процессов.
		if err := f.processes.Close(); err != nil {
			t.Error(err)
		}
	})
	// watchdog задаёт короткие сроки перехода LIVE/заглушка и восстановления для управляемого
	// сценария.
	watchdog := core.WatchdogPolicy{VideoTimeout: 300 * time.Millisecond, AudioTimeout: 300 * time.Millisecond, RecoveryWindow: 70 * time.Millisecond, KeyframeTimeout: time.Second}
	// profile задаёт исходный AV1 1440p профиль настроек переключателя до выбора сохранённых групп.
	profile := applicationFlowProfiles()[0]
	// Функциональный сценарий использует стандартный бюджет операции сервера.
	f.config = session.Config{Core: f.service, Files: f.files, Processes: f.processes, GeneratorVersion: "controlled-generator", Router: publisherRouterLimits(), Publisher: publisher.Options{Queue: publisherQueue(), RetryInitial: 10 * time.Millisecond, RetryMax: 50 * time.Millisecond, ResubscribeDelay: 5 * time.Millisecond, Transport: publisherTransportOptions(nil)}, Logger: slog.New(slog.NewJSONHandler(&f.logs, nil)), MaxSessions: 1, MaxOutputs: 8, MaxCommands: 8, PageSize: 8, MaxGroups: 4, MaxReferences: 8, ReconcileInterval: 5 * time.Millisecond, StartupTimeout: 4 * time.Second, OperationTimeout: config.Default().Session.OperationTimeout, ShutdownTimeout: 4 * time.Second,
		Monitoring: monitoring.SamplerConfig{MaxSources: 128, StaleAfter: 5 * time.Second, ShutdownTimeout: 4 * time.Second}, Telemetry: core.TelemetryLimits{Events: core.EventLimits{MaxEvents: 128, MaxBytes: 1 << 20, MaxEventBytes: 65536, MaxPageSize: 64}, MediaStaleAfter: 5 * time.Second, MaxGPUDevices: 8, MaxGPUUUIDBytes: 256, MaxGPUBytes: 1 << 20},
		Group: group.RuntimeConfig{Switcher: group.SwitcherConfig{Profile: profile, Watchdog: watchdog, Queue: publisherQueue(), Router: publisherRouterLimits(), MaxAVSkew: 100 * time.Millisecond, MaxTransitionGap: 100 * time.Millisecond}, Container: integrationLimits(), Binary: helper, ResubscribeBackoff: 5 * time.Millisecond, ControlInterval: 5 * time.Millisecond, ShutdownTimeout: 3 * time.Second, FFmpeg: ffmpeg.Config{BaseDir: filepath.Join(base, "r"), StartupTimeout: 3 * time.Second, VideoTimeout: 2 * time.Second, AudioTimeout: 2 * time.Second, WatchdogInterval: 10 * time.Millisecond, MaxRestarts: 0, RestartBackoff: time.Millisecond, GracePeriod: 50 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 3 * time.Second, DiagnosticEvents: 64, DiagnosticBytes: 65536, DiagnosticChunkBytes: 1024, MaxCommandBytes: 8192, MaxCommandArgs: 128, Progress: ffmpeg.ProgressConfig{MaxLineBytes: 4096, MaxReportBytes: 65536, MaxFields: 64}}}}
	// probe задаёт конечные пределы реальной проверки исходника и структурного выходного контейнера.
	probe := fallback.ProbeLimits{MaxJSONBytes: 65536, MaxWidth: 4096, MaxHeight: 4096, MaxPixels: 4096 * 4096, MaxDuration: time.Minute, ImageDuration: 1600 * time.Millisecond, MaxPackets: 1000, Container: integrationLimits()}
	// preparation содержит рабочий FallbackManager с заменённым кодированием и настоящими файлами.
	preparation, err := fallback.NewManagerWithTools(f.service, f.files, fallback.Config{ReconcileLimit: 16, GeneratorVersion: "controlled-generator", FFmpeg: helper, FFprobe: helper, MaxUploadBytes: 1 << 20, MaxOutputBytes: 1 << 20, PrepareTimeout: 5 * time.Second, ShutdownTimeout: 3 * time.Second, QueueInterval: time.Millisecond, Runner: fallback.RunnerLimits{Timeout: 3 * time.Second, MaxArgs: 128, MaxCommandBytes: 8192, MaxDiagnosticBytes: 65536}, Probe: probe}, fallback.Tools{Builder: func(r fallback.GenerateRequest) (fallback.Command, error) {
		// Сериализует профиль в аргумент управляемого генератора, сохраняя настоящую файловую подготовку.

		// data содержит JSON полного профиля, передаваемый генератору; err содержит отказ
		// сериализации.
		data, err := json.Marshal(r.Profile)
		return fallback.Command{Binary: "controlled-generator", Args: []string{string(data)}}, err
	}, Inspector: applicationFlowInspection{fallback.Prober{Limits: probe}}, Runner: applicationFlowGenerator{t}})
	if err != nil {
		t.Fatal(err)
	}
	f.config.Preparation = preparation
	// ingress содержит настоящий RTMP-сервер, получающий входной Router текущей Session через
	// менеджер.
	ingress, err := rtmp.NewServer(rtmp.Options{Core: f.service, Route: rtmp.ConventionalRoute, Limits: publisherRTMPLimits(), Feed: func(current core.Session) (*media.Router, error) {
		// Передаёт Session настоящему SessionManager для получения входного маршрутизатора.
		return f.manager.Feed(current)
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.config.Ingress = ingress
	f.config.Group.Player = func(_ context.Context, _ string, file *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Читает формат подготовленного контейнера, возвращает файл к началу и создаёт управляемый
		// проигрыватель с маркером заглушки.

		// d содержит демультиплексор выбранного подготовленного варианта; err содержит отказ его
		// открытия.
		d, err := applicationFlowDemux(file)
		if err != nil {
			return ffmpeg.Command{}, err
		}
		// v содержит фактический видеоформат файла для выбора управляемого проигрывателя
		// соответствующих размеров.
		v := d.Init().Video.Info().Video
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return ffmpeg.Command{}, err
		}
		f.starts.Add(1)
		return publisherControlledCommand(publisherProfile(v.Codec, v.Width, v.Height), a, 10, "")
	}
	f.config.Group.Transcode = func(_ context.Context, n ffmpeg.NVIDIAConfig, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Создаёт управляемый транскодировщик с форматом заданного профиля и отличимым маркером LIVE.

		return publisherControlledCommand(n.Profile, a, 20, "")
	}
	t.Cleanup(func() {
		// Сохраняет диагностический снимок только при отказе полного сценария.

		if t.Failed() {
			f.captureFailure(t)
		}
	})
	f.launch(t)
	return f
}

// Переводит формат и параметры кодирования профиля в DTO для запросов API, сохраняя точные строковые
// представления численных полей.
func applicationFlowProfileJSON(p core.GroupProfile) api.GroupProfileDTO {
	// v содержит формат видео; a содержит формат аудио; e содержит общие видеонастройки для точного
	// API-представления профиля.
	v, a, e := p.Format.Video, p.Format.Audio, p.VideoEncoding
	return api.GroupProfileDTO{
		Mode: string(p.Mode),
		Format: api.FormatDTO{
			Video: api.VideoFormatDTO{
				Codec: string(v.Codec), Width: v.Width, Height: v.Height,
				FrameRate:    api.FrameRateDTO{Numerator: v.FrameRate.Numerator, Denominator: v.FrameRate.Denominator},
				CodecProfile: api.ProfileText(v.CodecProfile), Level: api.ProfileText(v.Level), Tier: api.ProfileText(v.Tier),
				PixelFormat: api.ProfileText(v.PixelFormat), BitDepth: v.BitDepth,
				Color: api.ColorDTO{Range: api.ProfileText(v.Color.Range), Primaries: api.ProfileText(v.Color.Primaries), Transfer: api.ProfileText(v.Color.Transfer), Matrix: api.ProfileText(v.Color.Matrix)},
			},
			Audio: api.AudioFormatDTO{Codec: string(a.Codec), CodecProfile: api.ProfileText(a.CodecProfile), SampleRateHz: a.SampleRateHz, Channels: a.Channels, ChannelLayout: api.ProfileText(a.ChannelLayout)},
		},
		VideoEncoding: api.VideoEncodingDTO{TargetBitrateBPS: api.Decimal(e.TargetBitrateBPS), MaxBitrateBPS: api.Decimal(e.MaxBitrateBPS), BufferBits: api.Decimal(e.BufferBits), GOPFrames: e.GOPFrames, MaxBFrames: e.MaxBFrames, ClosedGOP: e.ClosedGOP},
		AudioEncoding: api.AudioEncodingDTO{BitrateBPS: api.Decimal(p.AudioEncoding.BitrateBPS)},
	}
}

// Запускает сервер приёма RTMP фикстуры на свободном локальном порту и возвращает адрес публикации для
// её Stream. t.Cleanup ограниченно останавливает сервер и ожидает завершения Serve.
func applicationFlowServeIngest(t *testing.T, f *sessionAcceptanceFixture) string {
	t.Helper()
	// l содержит локальный RTMP-слушатель; err содержит отказ выделения свободного порта.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// server содержит уже собранный RTMP-владелец из конфигурации фикстуры.
	server := f.config.Ingress.(*rtmp.Server)
	// done передаёт результат отдельного Serve для присоединения при очистке.
	done := make(chan error, 1)
	go func() {
		// Выполняет Serve приёмника RTMP отдельно от управляющих HTTP-запросов теста.
		done <- server.Serve(context.Background(), l)
	}()
	t.Cleanup(func() {
		// Останавливает приёмник и ограниченно ожидает завершения Serve.

		// ctx ограничивает остановку ingress четырьмя секундами; cancel освобождает таймер.
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		// err содержит результат Shutdown RTMP-сервера при очистке.
		if err := server.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		select {
		// err содержит фактический результат завершённого Serve для проверки штатного закрытия
		// слушателя.
		case err := <-done:
			if err != nil && !errors.Is(err, net.ErrClosed) {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Error("ingest не завершился")
		}
	})
	return fmt.Sprintf("rtmp://%s/%s", l.Addr(), f.stream.ID)
}
