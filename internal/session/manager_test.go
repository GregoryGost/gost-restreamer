package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// lifecycleOwner изображает внешнего владельца, у которого тест наблюдает закрытие допуска.
type lifecycleOwner struct { /* closed отмечает вызов закрытия допуска или владельца. */
	closed atomic.Bool
}

// CloseAdmission отмечает закрытие приёма в проверочном владельце жизненного цикла.
func (o *lifecycleOwner) CloseAdmission() { o.closed.Store(true) }

// Reconcile изображает успешную сверку подготовки без дополнительных действий.
func (o *lifecycleOwner) Reconcile(context.Context) error { return nil }

// Close отмечает закрытие проверочного владельца подготовки и возвращает успех.
func (o *lifecycleOwner) Close(context.Context) error { o.closed.Store(true); return nil }

// Shutdown отмечает закрытие проверочного приёма публикаций OBS и возвращает успех.
func (o *lifecycleOwner) Shutdown(context.Context) error { o.closed.Store(true); return nil }

// managerFixture объединяет временные зависимости менеджера и подготовленное назначение для сценариев
// жизненного цикла.
type managerFixture struct {
	// databasePath указывает файл временной SQLite для повторного открытия в проверках
	// восстановления.
	databasePath string
	// manager предоставляет проверяемые операции управления и согласования сессий.
	manager *Manager
	// service читает и изменяет долговечное состояние тестовой базы.
	service *core.Service
	// files владеет каталогом подготовленных файлов заглушки.
	files *fallback.FileStore
	// config сохраняет настройки для создания следующего менеджера над теми же зависимостями.
	config Config
	// stream задаёт поток, запускаемый командами сценария.
	stream core.Stream
	// destination задаёт назначение подготовленной группы, включаемое параметром фикстуры.
	destination core.Destination
	// binding связывает подготовленный вариант заглушки с профилем группы.
	binding core.PreparedBinding
	// ingress изображает владельца входного приёма; preparation изображает владельца подготовки
	// заглушки.
	ingress, preparation *lifecycleOwner
	// store сохраняет доступ к временному хранилищу для обёрток, моделирующих неоднозначный
	// результат транзакции.
	store *storage.Store
}

// fixtureProfile создаёт согласованные конфигурации AV1 и AAC и профиль копирования для проверочных
// пакетов и подготовленного варианта.
func fixtureProfile(t testing.TB) (core.GroupProfile, media.CodecConfig, media.CodecConfig) {
	t.Helper()
	// headers задаёт AV1-конфигурацию для проверяемых медиапакетов.
	headers := []byte{0x81, 8, 0x0c, 0, 0x0a, 11, 0, 0, 0, 66, 167, 191, 228, 96, 13, 0, 64}
	// payload объединяет заголовок последовательности AV1 и независимый кадр для определения
	// формата.
	payload := append(append([]byte(nil), headers[4:]...), 0x1a, 1, 0x10)
	// video содержит распознанный формат AV1 при 30 кадрах в секунду; err сообщает отказ разбора
	// тестовой временной единицы.
	video, _, err := codec.InspectAV1TU(headers, payload, core.FrameRate{Numerator: 30, Denominator: 1})
	if err != nil {
		t.Fatal(err)
	}
	// audio содержит формат AAC, извлечённый из тестового AudioSpecificConfig.
	audio, err := codec.AudioFormatASC([]byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	// base задаёт временную шкалу в долях 1/48000 секунды для обеих дорожек.
	base := media.TimeBase{Numerator: 1, Denominator: 48000}
	// v содержит видеоконфигурацию первой эпохи и ревизии, связанную с тестовыми AV1-заголовками.
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: base, Video: video}, headers)
	if err != nil {
		t.Fatal(err)
	}
	// a содержит аудиоконфигурацию первой эпохи и ревизии с тестовым AAC-заголовком.
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: base, Audio: audio}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	// profile задаёт режим копирования, проверенные форматы и параметры кодирования подготовленной
	// группы.
	profile := core.GroupProfile{Mode: core.ModeCopy, Format: core.MediaFormat{Video: video, Audio: audio}, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 1000, MaxBitrateBPS: 1000, BufferBits: 1000, GOPFrames: 30, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 128000}}
	return profile, v, a
}

// newManagerFixture создаёт временную SQLite, подготовленный файл и настоящий gost-process-helper,
// затем собирает Manager с управляемым медиапроцессом. Зарегистрированные завершители закрывают
// Manager раньше общих зависимостей.
func newManagerFixture(t *testing.T, enabled bool, modify func(*Config)) managerFixture {
	t.Helper()
	// ctx задаёт независимый от запроса контекст создания временных зависимостей.
	ctx := context.Background()
	// directory изолирует базу, подготовленные файлы и записи процессов этого теста.
	directory := t.TempDir()
	// options задаёт явные лимиты хранилища, наблюдений и выполнения назначений, а также бюджеты
	// SQLite.
	options := storage.Options{PreparationLimits: core.PreparationLimits{MaxQueued: 2, MaxProfiles: 8}, OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 32, MaxIdentifierBytes: 1024, MaxBytes: 4 << 20}, ObservationLimits: core.ObservationLimits{MaxSessions: 8, MaxGroups: 32, MaxOutputs: 32, MaxFFmpegWorkers: 32, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond}
	// store владеет открытым временным хранилищем; err сообщает отказ его открытия и начальной
	// настройки.
	store, err := storage.Open(ctx, filepath.Join(directory, "session.sqlite"), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает временное хранилище при завершении проверки и сообщает тесту об ошибке закрытия.

		// err сообщает отказ закрытия временного хранилища при очистке теста.
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	// service предоставляет доменные операции в инкарнации открытого хранилища.
	service, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// files предоставляет управляемые файлы заглушки в каталоге фикстуры.
	files, err := fallback.OpenFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает FileStore после вызова Close у Manager и сообщает тесту об ошибке.

		// err сообщает отказ закрытия файлового хранилища при очистке теста.
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})
	// profile содержит совместимые форматы и настройки группы для подготовки заглушки.
	profile, _, _ := fixtureProfile(t)
	// asset задаёт созданный объект заглушки, для которого резервируется поколение.
	asset, err := service.CreateAsset(ctx, "Заглушка")
	if err != nil {
		t.Fatal(err)
	}
	// policy связывает поток с заглушкой и задаёт сроки обнаружения потери и восстановления дорожек.
	policy := core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: 20 * time.Millisecond, KeyframeTimeout: time.Second}}
	// stream содержит созданный поток с ключом публикации и политикой заглушки.
	stream, err := service.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "session-key", Fallback: policy})
	if err != nil {
		t.Fatal(err)
	}
	// p содержит сохранённый профиль, включая назначенные сервисом значения настроек.
	p, err := service.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	profile = p.Profile
	// g содержит созданную группу потока, ссылающуюся на сохранённый профиль.
	g, err := service.CreateGroup(ctx, core.TranscodeGroup{Name: "Группа", StreamID: stream.ID, ProfileID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	// destination содержит назначение группы с недоступным локальным RTMP-адресом и заданным
	// состоянием включения.
	destination, err := service.CreateDestination(ctx, core.Destination{Name: "Выход", StreamID: stream.ID, GroupID: g.ID, Platform: "custom", Endpoint: "rtmp://127.0.0.1:1/live", StreamKey: "test-key", Enabled: enabled})
	if err != nil {
		t.Fatal(err)
	}
	// generation содержит зарезервированное поколение подготовки и требуемые файлы профилей.
	generation, err := service.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "test-generator"})
	if err != nil {
		t.Fatal(err)
	}
	// preparationFence удостоверяет захваченную попытку подготовки перед её выдачей.
	preparationFence, err := service.ClaimPreparation(ctx, generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.MarkPreparationIssued(ctx, preparationFence); err != nil {
		t.Fatal(err)
	}
	if err = files.CreateGeneration(generation.ID); err != nil {
		t.Fatal(err)
	}
	// required задаёт первый требуемый вариант поколения и его управляемый путь.
	required := generation.RequiredProfiles[0]
	// file предоставляет запись содержимого тестового варианта заглушки.
	file, err := files.Create(required.ManagedPath)
	if err != nil {
		t.Fatal(err)
	}
	// content задаёт проверяемые байты файла, публикуемого как подготовленный вариант.
	content := []byte("локальный проверяемый файл")
	if _, err = file.Write(content); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	// digest содержит SHA-256 записанных байтов заглушки.
	digest := sha256.Sum256(content)
	// hash содержит шестнадцатеричное представление контрольной суммы публикуемого файла.
	hash := hex.EncodeToString(digest[:])
	_, err = service.PublishPreparation(ctx, preparationFence, core.GenerationPublication{SourceHash: "source", SizeBytes: 1, SourceFormat: core.SourceFormat{ImageFormat: "png"}, Variants: []core.FallbackVariant{{ID: required.VariantID, GenerationID: generation.ID, AssetID: asset.ID, SourceHash: "source", Profile: profile, VerifiedFormat: profile.Format, GeneratorVersion: generation.GeneratorVersion, ManagedPath: required.ManagedPath, SizeBytes: uint64(len(content)), ContentHash: hash}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.AcknowledgePreparationCleanup(ctx, preparationFence); err != nil {
		t.Fatal(err)
	}
	// helper указывает временный исполняемый файл владельца дочерних процессов.
	helper := filepath.Join(t.TempDir(), "gost-process-helper")
	// args задаёт параметры сборки вспомогательного процесса с возможным включением покрытия.
	args := []string{"build", "-o", helper}
	// directory задаёт переданный окружением каталог покрытия вспомогательного процесса.
	if directory := os.Getenv("GOST_TEST_PROCESS_HELPER_COVERDIR"); directory != "" {
		t.Setenv("GOCOVERDIR", directory)
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=github.com/GregoryGost/gost-restreamer/...")
	}
	// command собирает вспомогательный процесс из исходников проекта во временный файл.
	command := exec.Command("go", append(args, "../../cmd/gost-process-helper")...)
	// output содержит объединённую диагностику сборки вспомогательного процесса; err сообщает отказ
	// этой сборки.
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v %s", err, output)
	}
	// runtimeParent задаёт родительский каталог коротких путей Unix-сокетов.
	runtimeParent := os.TempDir()
	if runtime.GOOS == "darwin" {
		// Системный TMPDIR macOS слишком длинный для вложенных путей UDS.
		runtimeParent = "/private/tmp"
	}
	// runtimeDirectory изолирует Unix-сокеты и временные ресурсы процессов фикстуры.
	runtimeDirectory, err := os.MkdirTemp(runtimeParent, "sm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Удаляет принадлежащий проверке короткий каталог UDS при завершении проверки.

		// err сообщает отказ удаления каталога ресурсов процессов при очистке теста.
		if err := os.RemoveAll(runtimeDirectory); err != nil {
			t.Error(err)
		}
	})
	// processes владеет тестовыми процессами, записями операций и подтверждением их очистки.
	processes, err := processowner.New(processowner.Config{Root: filepath.Join(directory, "processes"), RuntimeRoot: runtimeDirectory, HelperPath: helper, Limits: processowner.Limits{MaxOperations: 512, MaxRecordBytes: 1 << 20, MaxCommandArgs: 128, MaxCommandBytes: 16384, MaxExtraFiles: 8, PageSize: 32}, GracePeriod: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает реестр процессов после вызова Close у Manager и сообщает тесту об ошибке.

		// err сообщает отказ закрытия реестра процессов при очистке теста.
		if err := processes.Close(); err != nil {
			t.Error(err)
		}
	})
	// ingress изображает владельца входного приёма; preparation изображает владельца подготовки для
	// проверки порядка закрытия.
	ingress, preparation := &lifecycleOwner{}, &lifecycleOwner{}
	// queue ограничивает число и логический объём пакетов, размер заголовков и срок ресинхронизации
	// тестовых потребителей.
	queue := media.QueueLimits{MaxPackets: 64, MaxBytes: 65536, MaxPacketBytes: 4096, MaxConfigBytes: 512, ResyncTimeout: time.Second}
	// router ограничивает потребителей и удерживаемые байты тестового медиамаршрутизатора.
	router := media.RouterLimits{MaxConsumers: 16, MaxRetainedBytes: 2 << 20, MaxConfigBytes: 512}
	// c объединяет зависимости, лимиты и сроки менеджера, отправителей и локальных групп фикстуры.
	c := Config{Core: service, Files: files, Processes: processes, GeneratorVersion: generation.GeneratorVersion, Ingress: ingress, Preparation: preparation, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxSessions: 2, MaxOutputs: 8, MaxCommands: 8, PageSize: 8, MaxGroups: 8, MaxReferences: 8, Router: router, ReconcileInterval: 5 * time.Millisecond, StartupTimeout: 3 * time.Second, OperationTimeout: time.Second, ShutdownTimeout: 3 * time.Second, Publisher: publisher.Options{Queue: queue, RetryInitial: time.Millisecond, RetryMax: 20 * time.Millisecond, ResubscribeDelay: time.Millisecond, Transport: rtmp.OutboundOptions{Limits: rtmp.OutboundLimits{MaxChunkStreams: 8, MaxMessageBytes: 65536, MaxChunkBytes: 4096, MaxFragments: 1024, MaxControlBytes: 4096, MaxConfigBytes: 4096, MaxAMFBytes: 8192, MaxAMFDepth: 8, MaxAMFElements: 128, ChunkSize: 4096, MaxControlPending: 32, MaxRetainedBytes: 4 << 20, DialTimeout: time.Second, TLSTimeout: time.Second, StartupTimeout: time.Second, MessageTimeout: time.Second, WriteTimeout: time.Second}}}, Group: group.RuntimeConfig{Switcher: group.SwitcherConfig{Profile: profile, Watchdog: policy.Watchdog, Queue: queue, Router: router, MaxAVSkew: time.Second, MaxTransitionGap: time.Second}, Container: fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 65536, MaxMdatBytes: 1 << 20, MaxSampleBytes: 65536, MaxExtradataBytes: 4096, MaxSamples: 100}, Binary: "controlled", ResubscribeBackoff: time.Millisecond, ControlInterval: 5 * time.Millisecond, ShutdownTimeout: 2 * time.Second, FFmpeg: ffmpeg.Config{BaseDir: runtimeDirectory, StartupTimeout: time.Second, VideoTimeout: time.Second, AudioTimeout: time.Second, WatchdogInterval: 10 * time.Millisecond, RestartBackoff: time.Millisecond, GracePeriod: 20 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 2 * time.Second, DiagnosticEvents: 32, DiagnosticBytes: 65536, DiagnosticChunkBytes: 1024, MaxCommandBytes: 8192, MaxCommandArgs: 64}}}
	c.Monitoring = monitoring.SamplerConfig{MaxSources: 128, StaleAfter: 5 * time.Second, ShutdownTimeout: 3 * time.Second}
	c.Telemetry = core.TelemetryLimits{Events: core.EventLimits{MaxEvents: 128, MaxBytes: 1 << 20, MaxEventBytes: 65536, MaxPageSize: 64}, MediaStaleAfter: 5 * time.Second, MaxGPUDevices: 8, MaxGPUUUIDBytes: 256, MaxGPUBytes: 1 << 20}
	c.Group.FFmpeg.Progress = ffmpeg.ProgressConfig{MaxLineBytes: 4096, MaxReportBytes: 65536, MaxFields: 64}
	c.Group.Player = func(_ context.Context, _ string, _ *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Строит команду запуска текущего тестового бинарника в режиме записи fMP4 в путь конкретной попытки
		// FFmpeg.

		// binary указывает текущий тестовый исполняемый файл для управляемого медиапроцесса; err
		// сообщает отказ определения его пути.
		binary, err := os.Executable()
		return ffmpeg.Command{Binary: binary, Args: []string{"-test.run=^TestSessionMediaProcess$", "--", a.OutputPath}}, err
	}
	if modify != nil {
		modify(&c)
	}
	// manager содержит созданный диспетчер над подготовленными зависимостями.
	manager, err := New(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает созданный Manager до освобождения его общих зависимостей и сообщает тесту об ошибке.

		// err сообщает отказ завершения менеджера при очистке теста.
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return managerFixture{filepath.Join(directory, "session.sqlite"), manager, service, files, c, stream, destination, core.PreparedBinding{GroupID: g.ID, GenerationID: generation.ID, VariantID: required.VariantID, GeneratorVersion: generation.GeneratorVersion}, ingress, preparation, store}
}

// TestSessionMediaProcess при специальном запуске дочернего тестового процесса непрерывно пишет AV1 и
// AAC в fMP4 через UDS до ошибки записи.
func TestSessionMediaProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	// output соединяет управляемый медиапроцесс с Unix-сокетом группы; err сообщает отказ этого
	// соединения.
	output, err := net.Dial("unix", os.Args[len(os.Args)-1])
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает соединение дочернего медиапроцесса с UDS при возврате из проверочного режима.
		_ = output.Close()
	}()
	// v содержит тестовую видеоконфигурацию AV1; a содержит тестовую аудиоконфигурацию AAC.
	_, v, a := fixtureProfile(t)
	// mux записывает инициализацию и фрагменты обеих дорожек в соединение группы.
	mux, err := fmp4.NewMuxer(output, fmp4.Init{Video: v, Audio: a}, fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 65536, MaxMdatBytes: 1 << 20, MaxSampleBytes: 65536, MaxExtradataBytes: 4096, MaxSamples: 100})
	if err != nil {
		t.Fatal(err)
	}
	// ticker задаёт интервал выдачи очередной пары медиапакетов управляемым процессом.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	// tick считает выдаваемые пары и определяет их временные метки с шагом 1600 тиков.
	for tick := int64(0); ; tick++ {
		// video содержит очередной независимый AV1-кадр длительностью 1600 тиков тестовой шкалы.
		video, _ := media.NewPacket(media.PacketInfo{PTS: tick * 1600, DTS: tick * 1600, DurationTicks: 1600, Keyframe: true}, v, append(v.Headers()[4:], 0x1a, 1, 0x10))
		// audio содержит очередной AAC-пакет с длительностью 1024 тика и меткой текущей пары.
		audio, _ := media.NewPacket(media.PacketInfo{PTS: tick * 1600, DTS: tick * 1600, DurationTicks: 1024}, a, []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c})
		if mux.WritePacket(video) != nil || mux.WritePacket(audio) != nil {
			return
		}
		<-ticker.C
	}
}

// waitReceipt опрашивает сохранённую квитанцию до терминального статуса и завершает тест ошибкой при
// истечении восьми секунд.
func waitReceipt(t *testing.T, f managerFixture, receipt core.CommandReceipt) core.CommandReceipt {
	t.Helper()
	// ctx ограничивает ожидание терминальной квитанции восемью секундами; cancel освобождает ресурсы
	// этого срока.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// ticker задаёт период повторного чтения квитанции в ожидании её завершения.
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		// current содержит последнее состояние ожидаемой квитанции; err сообщает отказ её
		// чтения.
		current, err := f.service.GetReceipt(ctx, receipt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status.Terminal() {
			return current
		}
		select {
		case <-ctx.Done():
			t.Fatalf("квитанция не завершилась: %+v", current)
		case <-ticker.C:
		}
	}
}

// TestManagerZeroOutputsOwnsLifecycleAndExactFeed проверяет запуск без выходов, отклонение чужого
// Lifecycle, выполнение паузы и закрытие приёма после Close.
func TestManagerZeroOutputsOwnsLifecycleAndExactFeed(t *testing.T) {
	// f предоставляет поток без включённых назначений для проверки самостоятельного жизненного цикла
	// сессии.
	f := newManagerFixture(t, false, nil)
	// ctx задаёт контекст команд и чтений сценария сессии без назначений.
	ctx := context.Background()
	// receipt подтверждает приём команды запуска без назначений; err сообщает отказ её приёма.
	receipt, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "start-zero", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит терминальный результат запуска сессии без назначений.
	if current := waitReceipt(t, f, receipt); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	// session содержит сохранённую сессию для проверки точного доступа к её медиавходу.
	session, err := f.service.GetSession(ctx, receipt.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// router содержит медиамаршрутизатор запущенной сессии до изменения проверяемой ревизии.
	router, err := f.manager.Feed(session)
	if err != nil || router == nil {
		t.Fatal(err)
	}
	session.Lifecycle++
	if _, err = f.manager.Feed(session); !errors.Is(err, core.ErrStale) {
		t.Fatal("принят чужой lifecycle", err)
	}
	// pause содержит принятую команду ручной паузы сессии без назначений.
	pause, err := f.manager.PauseSession(ctx, core.SessionCommand{RequestID: "pause-zero", SessionID: receipt.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит терминальный результат ручной паузы.
	if current := waitReceipt(t, f, pause); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	if err = f.manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.ingress.closed.Load() || !f.preparation.closed.Load() {
		t.Fatal("владельцы не закрыты")
	}
	if _, err = f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "closed", StreamID: f.stream.ID}); !errors.Is(err, core.ErrClosed) {
		t.Fatal(err)
	}
}

// TestManagerFallbackBarrierAndPriorReceipt проверяет текущую готовность групп перед успехом запуска и
// возврат прежней квитанции после удаления закреплённого файла.
func TestManagerFallbackBarrierAndPriorReceipt(t *testing.T) {
	// f предоставляет включённое назначение с локальной заглушкой для проверки барьера готовности.
	f := newManagerFixture(t, true, nil)
	// ctx задаёт контекст запуска и проверки подготовленной сессии.
	ctx := context.Background()
	// receipt подтверждает приём запуска с включённым назначением; err сообщает отказ приёма
	// команды.
	receipt, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "start-media", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит результат запуска после прохождения начального барьера готовности.
	current := waitReceipt(t, f, receipt)
	if current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	f.manager.mu.Lock()
	// node указывает локального владельца ресурсов запущенной сессии.
	node := f.manager.sessions[receipt.SessionID]
	f.manager.mu.Unlock()
	if node == nil {
		t.Fatal("нет исполнения")
	}
	node.mu.Lock()
	// member указывает очередного участника начального барьера, чья ссылка группы проверяется на
	// готовность.
	for _, member := range node.members {
		member.mu.Lock()
		// ref содержит ссылку группы участника, полученную под его блокировкой.
		ref := member.reference
		member.mu.Unlock()
		if ref == nil || ref.Group().Ready() != nil {
			t.Fatal("RUNNING до текущего fallback")
		}
	}
	node.mu.Unlock()
	// Повтор не открывает файл заново и не выбирает новую конфигурацию.
	// variant содержит подготовленный вариант для удаления файла после успешного запуска.
	variant, err := f.service.GetVariant(ctx, f.binding.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.files.RemoveExact(ctx, variant.ManagedPath, variant.SizeBytes, variant.ContentHash); err != nil {
		t.Fatal(err)
	}
	// duplicate содержит повторную квитанцию того же запроса запуска после удаления файла.
	duplicate, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "start-media", StreamID: f.stream.ID})
	if err != nil || duplicate.ID != receipt.ID {
		t.Fatal(duplicate, err)
	}
}

// TestManagerStopDuringStartingExcludesOutputAndWaitsLateBuilder проверяет исключение остановленного
// выхода из стартового барьера и сохранение CleanupPending до возврата задержанного построителя
// команды.
func TestManagerStopDuringStartingExcludesOutputAndWaitsLateBuilder(t *testing.T) {
	// entered сигнализирует вход в задержанный построитель команды; release разрешает ему
	// завершиться после остановки назначения.
	entered, release := make(chan struct{}), make(chan struct{})
	// enteredOnce не допускает повторного закрытия сигнала входа при повторных вызовах построителя.
	var enteredOnce atomic.Bool
	// f предоставляет включённое назначение с построителем, задержанным до разрешения теста.
	f := newManagerFixture(t, true, func(c *Config) {
		// Подменяет построение проигрывателя управляемой задержкой, сохраняя исходный построитель команды.

		// original сохраняет исходный построитель медиапроцесса для продолжения после задержки.
		original := c.Group.Player
		c.Group.Player = func(ctx context.Context, binary string, file *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
			// Уведомляет о первом входе в построитель, ждёт разрешения теста и затем строит прежнюю команду.

			if enteredOnce.CompareAndSwap(false, true) {
				close(entered)
			}
			<-release
			return original(ctx, binary, file, a)
		}
	})
	// released отмечает выдачу разрешения построителю, чтобы отложенная очистка не закрыла канал
	// повторно.
	var released atomic.Bool
	defer func() {
		// Освобождает задержанный построитель при любом завершении теста, если разрешение ещё не выдано.

		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}()
	// ctx задаёт контекст команд во время задержанной начальной подготовки.
	ctx := context.Background()
	// receipt подтверждает приём запуска с задержанным построителем; err сообщает отказ приёма
	// запуска.
	receipt, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "start-held", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("builder не начал работу")
	}
	if _, err = f.service.AuthorizeOBS(ctx, f.stream.ID, f.stream.PublicationKey); !errors.Is(err, core.ErrNotReady) {
		t.Fatal("OBS допущен до fallback", err)
	}
	// outputs содержит назначения запускаемой сессии для выбора останавливаемого участника.
	outputs, err := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: receipt.SessionID}})
	if err != nil || len(outputs.Items) != 1 {
		t.Fatal(outputs, err)
	}
	// stop содержит команду исключения задержанного назначения из начального запуска.
	stop, err := f.manager.StopOutput(ctx, core.OutputCommand{RequestID: "stop-held", OutputID: outputs.Items[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит результат родительского запуска после исключения назначения.
	if current := waitReceipt(t, f, receipt); current.Status != core.CommandSucceeded {
		t.Fatal("ноль оставшихся выходов должен допускать RUNNING", current)
	}
	// output содержит состояние остановленного назначения до возврата задержанного построителя.
	output, err := f.service.GetOutput(ctx, outputs.Items[0].ID)
	if err != nil || !output.CleanupPending {
		t.Fatal("поздний builder уже объявлен очищенным", output, err)
	}
	released.Store(true)
	close(release)
	// current содержит результат остановки назначения после возврата построителя и очистки ресурсов.
	if current := waitReceipt(t, f, stop); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	output, err = f.service.GetOutput(ctx, outputs.Items[0].ID)
	if err != nil || output.CleanupPending {
		t.Fatal(output, err)
	}
}

// TestManagerCommonStartupFailureEndsSession проверяет, что отказ построения проигрывателя завершает
// запуск и сессию ошибкой и не оставляет Feed.
func TestManagerCommonStartupFailureEndsSession(t *testing.T) {
	// f предоставляет назначение, построитель медиапроцесса которого возвращает отказ доступа.
	f := newManagerFixture(t, true, func(c *Config) {
		// Подменяет построитель проигрывателя воспроизводимым отказом доступа.

		c.Group.Player = func(context.Context, string, *os.File, ffmpeg.Attempt) (ffmpeg.Command, error) {
			// Возвращает os.ErrPermission вместо команды проигрывателя для проверки общего отказа запуска.

			return ffmpeg.Command{}, os.ErrPermission
		}
	})
	// receipt подтверждает приём запуска с отказавшей локальной группой; err сообщает отказ приёма
	// команды.
	receipt, err := f.manager.StartSession(context.Background(), core.StartSessionRequest{RequestID: "start-failed", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит терминальный результат запуска при общем отказе начальной подготовки.
	if current := waitReceipt(t, f, receipt); current.Status != core.CommandFailed {
		t.Fatal(current)
	}
	// session содержит терминальное состояние сессии после отказа барьера готовности.
	session, err := f.service.GetSession(context.Background(), receipt.SessionID)
	if err != nil || session.State != core.SessionFailed {
		t.Fatal(session, err)
	}
	if _, err = f.manager.Feed(session); err == nil {
		t.Fatal("неуспешный startup сохранил Feed")
	}
}

// TestManagerSecondDispatcherRejectedAndRequestContextDoesNotOwnRuntime проверяет единственность
// диспетчера и сохранение принятой сессии после отмены контекста запроса.
func TestManagerSecondDispatcherRejectedAndRequestContextDoesNotOwnRuntime(t *testing.T) {
	// f предоставляет менеджер без назначений для проверки единственного диспетчера и срока запроса.
	f := newManagerFixture(t, false, nil)
	// duplicate содержит результат попытки создать второго диспетчера; err проверяется на запрет
	// повторного владения.
	if duplicate, err := New(context.Background(), f.config); !errors.Is(err, core.ErrInUse) || duplicate != nil {
		t.Fatal(duplicate, err)
	}
	// request задаёт отменяемый контекст приёма команды; cancel отменяет его после принятия запуска.
	request, cancel := context.WithCancel(context.Background())
	// receipt подтверждает принятие запуска до отмены контекста запроса; err сообщает отказ приёма
	// команды.
	receipt, err := f.manager.StartSession(request, core.StartSessionRequest{RequestID: "start-context", StreamID: f.stream.ID})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	// current содержит результат запуска, завершившегося после отмены запроса.
	if current := waitReceipt(t, f, receipt); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	// session содержит запущенную сессию для проверки продолжающего работать медиавхода.
	session, err := f.service.GetSession(context.Background(), receipt.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.Feed(session); err != nil {
		t.Fatal("контекст запроса отозвал runtime", err)
	}
}

// TestManagerOldUnknownCleanupDoesNotConsumeSessionQuota проверяет новый запуск при бюджете одной
// сессии и терминальном прежнем исполнении с сохранёнными удержаниями.
func TestManagerOldUnknownCleanupDoesNotConsumeSessionQuota(t *testing.T) {
	// oldID сохраняет идентификатор прежней сессии с неподтверждённой очисткой.
	var oldID core.ID
	// f предоставляет единственный слот активной сессии рядом с прежним неизвестным исполнением.
	f := newManagerFixture(t, false, func(c *Config) {
		// Ограничивает Manager одной сессией и заранее сохраняет терминальное выданное исполнение с
		// неподтверждённой очисткой.

		c.MaxSessions = 1
		// receipt содержит команду прежнего запуска; err сообщает отказ её создания перед
		// моделированием неизвестного исполнения.
		receipt, err := c.Core.StartSession(context.Background(), core.StartSessionRequest{RequestID: "old-start", StreamID: func() core.ID {
			// Получает идентификатор единственного созданного потока для прежней команды запуска.

			// streams содержит первый сохранённый поток для прежнего запуска; e сообщает
			// отказ чтения этого потока.
			streams, e := c.Core.ListStreams(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 1}})
			if e != nil {
				t.Fatal(e)
			}
			return streams.Items[0].ID
		}()})
		if err != nil {
			t.Fatal(err)
		}
		// fence удостоверяет попытку прежнего запуска, которая помечается выданной перед
		// завершением с ошибкой.
		fence, err := c.Core.Claim(context.Background(), receipt.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		if err = c.Core.MarkIssued(context.Background(), fence); err != nil {
			t.Fatal(err)
		}
		if err = c.Core.Complete(context.Background(), fence, core.Completion{Failure: "старое неизвестное исполнение"}); err != nil {
			t.Fatal(err)
		}
		oldID = receipt.SessionID
	})
	// receipt содержит принятый новый запуск при сохранённом прежнем удержании; err сообщает отказ
	// его приёма.
	receipt, err := f.manager.StartSession(context.Background(), core.StartSessionRequest{RequestID: "new-start", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит результат нового запуска при прежней неподтверждённой очистке.
	if current := waitReceipt(t, f, receipt); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	// old содержит прежнюю терминальную сессию для проверки сохранения удержания и признака выдачи.
	old, err := f.service.GetSession(context.Background(), oldID)
	if err != nil || !old.CleanupPending || !old.Issued {
		t.Fatal("неизвестное старое исполнение потеряло удержания", old, err)
	}
}

// TestManagerDynamicOutputRestartKeepsSnapshot проверяет добавление, остановку и повторный запуск
// выхода с новым поколением и неизменным сохранённым снимком.
func TestManagerDynamicOutputRestartKeepsSnapshot(t *testing.T) {
	// f предоставляет сессию для динамического добавления, остановки и повторного запуска
	// назначения.
	f := newManagerFixture(t, false, func(c *Config) {
		// Согласует срок запуска FFmpeg со сроком запуска сессии для проверки динамического выхода.

		c.Group.FFmpeg.StartupTimeout = c.StartupTimeout
	})
	// ctx задаёт контекст команд динамического назначения.
	ctx := context.Background()
	// parent содержит команду запуска родительской сессии без включённых назначений; err сообщает
	// отказ её приёма.
	parent, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "dynamic-parent", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит успешный результат запуска родительской сессии.
	if current := waitReceipt(t, f, parent); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	// added содержит принятую команду добавления назначения в работающую сессию.
	added, err := f.manager.AddOutput(ctx, core.AddOutputRequest{RequestID: "dynamic-add", SessionID: parent.SessionID, DestinationID: f.destination.ID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит результат подготовки добавленного назначения.
	if current := waitReceipt(t, f, added); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	// before содержит снимок и поколение назначения до остановки.
	before, err := f.service.GetOutput(ctx, added.OutputID)
	if err != nil {
		t.Fatal(err)
	}
	// stopped содержит команду остановки динамически добавленного назначения.
	stopped, err := f.manager.StopOutput(ctx, core.OutputCommand{RequestID: "dynamic-stop", OutputID: added.OutputID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит терминальный результат остановки назначения.
	if current := waitReceipt(t, f, stopped); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	// restarted содержит команду повторного запуска прежнего назначения.
	restarted, err := f.manager.StartOutput(ctx, core.OutputCommand{RequestID: "dynamic-restart", OutputID: added.OutputID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит результат повторного запуска назначения.
	if current := waitReceipt(t, f, restarted); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	// after содержит снимок и новое поколение назначения после повторного запуска.
	after, err := f.service.GetOutput(ctx, added.OutputID)
	if err != nil {
		t.Fatal(err)
	}
	// left содержит кодированный снимок назначения до остановки для побайтового сравнения.
	left, _ := core.EncodeOutputSnapshot(before.Snapshot)
	// right содержит кодированный снимок назначения после повторного запуска для проверки
	// сохранности настроек.
	right, _ := core.EncodeOutputSnapshot(after.Snapshot)
	if string(left) != string(right) || after.Generation <= before.Generation {
		t.Fatal("перезапуск изменил снимок", before, after)
	}
}

// ambiguousCompletionStore моделирует сохранённый успех запуска с ошибкой ответа и возможным отказом
// повторного чтения.
type ambiguousCompletionStore struct {
	// Store выполняет настоящие транзакции временной базы под обёрткой неоднозначного результата.
	*storage.Store
	// mu защищает признаки однократной инъекции и контекст, для которого чтения искусственно
	// отменяются.
	mu sync.Mutex
	// poisoned сохраняет контекст транзакции, чей успешный результат скрыт ошибкой отмены.
	poisoned context.Context
	// injected отмечает уже введённую ошибку после сохранённого успеха.
	injected bool
	// failRecovery включает однократный отказ независимого чтения успешной квитанции.
	failRecovery bool
	// recoveryFailed отмечает уже выполненную инъекцию отказа при восстановительном чтении.
	recoveryFailed bool
}

// Write выполняет настоящую запись и однократно возвращает context.Canceled после обнаружения
// успешного запуска, изображая потерянное подтверждение уже сохранённого результата.
func (s *ambiguousCompletionStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	// err сообщает отказ настоящей транзакции до проверки возможности скрыть её успешный результат.
	err := s.Store.Write(ctx, callback)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.injected {
		return nil
	}
	// succeeded отмечает наличие сохранённого успешного запуска среди квитанций.
	var succeeded bool
	// check ограничивает независимую проверку сохранённого успеха одной секундой; cancel освобождает
	// ресурсы проверки.
	check, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// readErr сообщает отказ независимой проверки успешных квитанций после транзакции.
	readErr := s.Store.View(check, func(r core.Repositories) error {
		// Проверяет в настоящей БД наличие успешной команды запуска, после которой требуется изобразить
		// потерянное подтверждение записи.

		// receipts содержит успешные квитанции для поиска запуска сессии; e сообщает отказ их
		// чтения.
		receipts, e := r.Runtime.Receipts(check, core.ListQuery{Limit: 8, State: string(core.CommandSucceeded)})
		if e != nil {
			return e
		}
		// receipt содержит очередную успешную квитанцию, проверяемую на запуск сессии.
		for _, receipt := range receipts {
			if receipt.Kind == core.CommandStartSession {
				succeeded = true
			}
		}
		return nil
	})
	if readErr != nil {
		return readErr
	}
	if succeeded {
		s.injected = true
		s.poisoned = ctx
		return context.Canceled
	}
	return nil
}

// View отклоняет чтения с контекстом неоднозначной записи и оборачивает остальные чтения для проверки
// ошибки восстановления квитанции.
func (s *ambiguousCompletionStore) View(ctx context.Context, callback func(core.Repositories) error) error {
	s.mu.Lock()
	// poisoned указывает, что чтение использует контекст транзакции со скрытым успешным результатом.
	poisoned := s.poisoned != nil && s.poisoned == ctx
	s.mu.Unlock()
	if poisoned {
		return context.Canceled
	}
	return s.Store.View(ctx, func(r core.Repositories) error {
		// Подставляет репозиторий чтения квитанций с управляемым отказом восстановления и вызывает исходное
		// чтение.

		r.Runtime = ambiguousReceiptRepository{RuntimeRepository: r.Runtime, store: s}
		return callback(r)
	})
}

// ambiguousReceiptRepository добавляет однократный отказ восстановительного чтения к настоящему
// репозиторию выполнения.
type ambiguousReceiptRepository struct {
	// RuntimeRepository обслуживает операции выполнения, кроме перехваченного чтения квитанции по
	// идентификатору.
	core.RuntimeRepository
	// store содержит состояние инъекции неоднозначного завершения и восстановительного чтения.
	store *ambiguousCompletionStore
}

// ReceiptByID однократно возвращает ErrBusy при чтении успешного запуска после неоднозначной записи,
// если включён отказ восстановления.
func (r ambiguousReceiptRepository) ReceiptByID(ctx context.Context, id core.ID) (core.CommandReceipt, error) {
	// receipt содержит прочитанную квитанцию перед возможной инъекцией отказа; err сообщает отказ
	// настоящего чтения.
	receipt, err := r.RuntimeRepository.ReceiptByID(ctx, id)
	if err != nil {
		return receipt, err
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if r.store.injected && r.store.failRecovery && !r.store.recoveryFailed && receipt.Kind == core.CommandStartSession && receipt.Status == core.CommandSucceeded {
		r.store.recoveryFailed = true
		return core.CommandReceipt{}, core.ErrBusy
	}
	return receipt, nil
}

// TestManagerAmbiguousParentCompletionPreservesSucceededReceipt проверяет сохранение успешной
// квитанции при потерянном подтверждении записи, включая отказ первого чтения для восстановления
// результата.
func TestManagerAmbiguousParentCompletionPreservesSucceededReceipt(t *testing.T) {
	// failRecovery выбирает сценарий с успешным или однократно отказавшим независимым
	// восстановлением квитанции.
	for _, failRecovery := range []bool{false, true} {
		t.Run(fmt.Sprint(failRecovery), func(t *testing.T) {
			// Проверяет выбранный вариант восстановления потерянного подтверждения: успешное чтение либо
			// однократный отказ с последующим завершением сессии.

			// f предоставляет сессию без назначений для проверки неоднозначного
			// подтверждения запуска.
			f := newManagerFixture(t, false, nil)
			// err сообщает отказ закрытия исходного менеджера перед установкой обёртки
			// хранилища.
			if err := f.manager.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			// Повторный Service использует ту же общую RuntimeAuthority и временную БД.
			// store сохраняет настоящее хранилище, над которым создаётся обёртка
			// неоднозначного результата.
			store := f.store
			// wrapped скрывает сохранённый успех запуска и при необходимости однократно
			// отклоняет восстановительное чтение.
			wrapped := &ambiguousCompletionStore{Store: store, failRecovery: failRecovery}
			// service предоставляет доменные операции через обёртку хранилища; err сообщает
			// отказ его создания.
			service, err := core.NewService(wrapped, store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			// c сохраняет настройки фикстуры с заменённым доменным сервисом.
			c := f.config
			c.Core = service
			// manager содержит диспетчер, работающий через обёртку неоднозначного
			// завершения.
			manager, err := New(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает Manager с обёрнутым хранилищем перед освобождением общих зависимостей проверки.

				// err сообщает отказ закрытия диспетчера с обёрткой при очистке теста.
				if err := manager.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			f.manager = manager
			// receipt содержит принятую команду запуска с моделируемой неоднозначностью
			// подтверждения.
			receipt, err := manager.StartSession(context.Background(), core.StartSessionRequest{RequestID: "ambiguous-parent", StreamID: f.stream.ID})
			if err != nil {
				t.Fatal(err)
			}
			// current содержит сохранённый терминальный результат родительской команды.
			current := waitReceipt(t, f, receipt)
			if current.Status != core.CommandSucceeded {
				t.Fatal(current)
			}
			// ctx ограничивает ожидание локального запуска или компенсации пятью секундами;
			// cancel освобождает этот срок.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// ticker задаёт период проверки состояния сессии после неоднозначного
			// подтверждения.
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				// session содержит актуальное состояние проверяемой сессии; e сообщает
				// отказ его чтения.
				session, e := f.service.GetSession(ctx, receipt.SessionID)
				if e != nil {
					t.Fatal(e)
				}
				if !failRecovery {
					if _, e = manager.Feed(session); e == nil {
						break
					}
				} else if session.State.Terminal() {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("неоднозначный результат оставил Session без завершения", session)
				case <-ticker.C:
				}
			}
			// preserved содержит первоначальную квитанцию для проверки неизменности
			// сохранённого успеха.
			preserved, err := f.service.GetReceipt(context.Background(), receipt.ID)
			if err != nil || preserved.Status != core.CommandSucceeded {
				t.Fatal("исходная успешная квитанция переписана", preserved, err)
			}
		})
	}
}

// delayedPriorStore задерживает первое отсутствие квитанции выбранного запроса, позволяя конкурентному
// запуску сохраниться раньше.
type delayedPriorStore struct {
	// Store выполняет настоящие операции временной базы при задержке чтения квитанции.
	*storage.Store
	// requestID задаёт запрос, первое чтение отсутствующей квитанции которого задерживается.
	requestID string
	// entered сообщает о задержанном чтении; release разрешает ему вернуть первоначальное отсутствие
	// квитанции.
	entered, release chan struct{}
	// delayed допускает задержку чтения выбранной квитанции только один раз.
	delayed atomic.Bool
}

// View подставляет репозиторий, задерживающий первое чтение отсутствующей квитанции указанного
// запроса.
func (s *delayedPriorStore) View(ctx context.Context, callback func(core.Repositories) error) error {
	return s.Store.View(ctx, func(r core.Repositories) error {
		// Подставляет репозиторий с управляемой задержкой отсутствующей квитанции и вызывает исходное чтение.

		r.Runtime = delayedPriorRepository{RuntimeRepository: r.Runtime, store: s}
		return callback(r)
	})
}

// delayedPriorRepository перехватывает чтение квитанции запроса для управляемой гонки повторного приёма.
type delayedPriorRepository struct {
	// RuntimeRepository выполняет настоящие операции выполнения под обёрткой задержанного чтения.
	core.RuntimeRepository
	// store предоставляет выбранный запрос и сигналы управления задержкой.
	store *delayedPriorStore
}

// Receipt после первого отсутствия нужного RequestID задерживает возврат до разрешения тестом либо
// отмены контекста.
func (r delayedPriorRepository) Receipt(ctx context.Context, requestID string) (core.CommandReceipt, error) {
	// receipt содержит результат настоящего чтения квитанции; err сохраняет исходное отсутствие до
	// конкурентного запуска.
	receipt, err := r.RuntimeRepository.Receipt(ctx, requestID)
	if requestID == r.store.requestID && errors.Is(err, core.ErrNotFound) && r.store.delayed.CompareAndSwap(false, true) {
		close(r.store.entered)
		select {
		case <-r.store.release:
		case <-ctx.Done():
			return core.CommandReceipt{}, ctx.Err()
		}
	}
	return receipt, err
}

// TestManagerConcurrentPriorWinsOverFreshFileFailure проверяет, что параллельно принятая квитанция
// возвращается повторному запросу после исчезновения файла, даже если первое чтение ещё не видело эту
// квитанцию.
func TestManagerConcurrentPriorWinsOverFreshFileFailure(t *testing.T) {
	// f предоставляет включённое назначение для гонки повторного запроса и удаления файла заглушки.
	f := newManagerFixture(t, true, nil)
	// err сообщает отказ закрытия исходного менеджера перед установкой задержки чтения.
	if err := f.manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// wrapped задерживает первое чтение отсутствующей квитанции запроса concurrent-start.
	wrapped := &delayedPriorStore{Store: f.store, requestID: "concurrent-start", entered: make(chan struct{}), release: make(chan struct{})}
	// service предоставляет доменные операции через управляемую задержку чтения; err сообщает отказ
	// его создания.
	service, err := core.NewService(wrapped, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// c содержит настройки менеджера с сервисом, перехватывающим чтение прежнего запроса.
	c := f.config
	c.Core = service
	// manager содержит диспетчер для конкурентных попыток одного запроса запуска.
	manager, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает Manager проверки параллельных запросов и сообщает об ошибке завершения.

		// err сообщает отказ закрытия диспетчера управляемой гонки при очистке теста.
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	// request задаёт одинаковый идентификатор запроса и поток для обеих конкурентных попыток.
	request := core.StartSessionRequest{RequestID: wrapped.requestID, StreamID: f.stream.ID}
	// commandResult переносит квитанцию и отказ задержанного вызова запуска из горутины в тест.
	type commandResult struct {
		// receipt содержит квитанцию задержанного вызова запуска.
		receipt core.CommandReceipt
		// err сообщает отказ задержанного вызова запуска.
		err error
	}
	// result передаёт завершение задержанного вызова без блокировки отправляющей горутины.
	result := make(chan commandResult, 1)
	// ctx ограничивает гонку приёма команды пятью секундами; cancel освобождает ресурсы этого срока.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		// Отправляет первый запрос запуска и передаёт его квитанцию или ошибку тесту после снятия задержки
		// чтения.
		// receipt содержит квитанцию задержанного вызова; e сообщает отказ этого вызова.
		receipt, e := manager.StartSession(ctx, request)
		result <- commandResult{receipt, e}
	}()
	select {
	case <-wrapped.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// winner содержит квитанцию конкурентного запуска, сохранённую раньше возврата задержанного
	// чтения.
	winner, err := manager.StartSession(ctx, request)
	if err != nil {
		close(wrapped.release)
		t.Fatal(err)
	}
	// variant содержит подготовленный вариант для удаления файла после победившего приёма команды.
	variant, err := f.service.GetVariant(ctx, f.binding.VariantID)
	if err != nil {
		close(wrapped.release)
		t.Fatal(err)
	}
	if err = f.files.RemoveExact(ctx, variant.ManagedPath, variant.SizeBytes, variant.ContentHash); err != nil {
		close(wrapped.release)
		t.Fatal(err)
	}
	close(wrapped.release)
	select {
	// actual содержит результат задержанного запуска для сравнения с уже сохранённой квитанцией.
	case actual := <-result:
		if actual.err != nil || actual.receipt.ID != winner.ID {
			t.Fatal("параллельная квитанция потеряна после ошибки файла", actual.receipt, actual.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
