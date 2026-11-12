package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// Настраивает базовый профиль выбранного кодека на заданные размеры и 60 FPS с GOP 60 для структурных
// сценариев Publisher.
func publisherProfile(c core.Codec, width, height uint32) core.GroupProfile {
	// p содержит базовый профиль выбранного кодека, дополненный размерами и частотой 60 кадров в
	// секунду.
	p := fallbackPipelineProfile(c)
	p.Format.Video.Width, p.Format.Video.Height = width, height
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 60, Denominator: 1}
	p.VideoEncoding.GOPFrames = 60
	return p
}

// Возвращает ограниченную очередь пакетов для интеграционного медиатракта с двухсекундным сроком
// восстановления.
func publisherQueue() media.QueueLimits {
	return media.QueueLimits{MaxPackets: 512, MaxBytes: 8 << 20, MaxPacketBytes: 1 << 20, MaxConfigBytes: 4096, ResyncTimeout: 2 * time.Second}
}

// Возвращает явные бюджеты потребителей, удерживаемых байтов и конфигураций общего тестового Router.
func publisherRouterLimits() media.RouterLimits {
	return media.RouterLimits{MaxConsumers: 12, MaxRetainedBytes: 128 << 20, MaxConfigBytes: 4096}
}

// Возвращает явные пределы соединений, RTMP/AMF-разбора и сроков обслуживания локального тестового
// сервера приёма.
func publisherRTMPLimits() rtmp.Limits {
	return rtmp.Limits{MaxConnections: 8, MaxChunkStreams: 16, MaxMessageBytes: 1 << 20, MaxChunkBytes: 65536, MaxFragments: 8192, MaxRetainedBytes: 32 << 20, MaxControlBytes: 64, MaxConfigBytes: 4096, MaxAMFBytes: 4096, MaxAMFDepth: 8, MaxAMFElements: 128, MaxReadAheadBytes: 65554, HandshakeTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 2 * time.Second, ShutdownTimeout: 3 * time.Second}
}

// Создаёт ограниченные параметры исходящего RTMP с чанком 128 байт и переданной конфигурацией доверия
// TLS для локального приёмника.
func publisherTransportOptions(trust *tls.Config) rtmp.OutboundOptions {
	return rtmp.OutboundOptions{TLSConfig: trust, Limits: rtmp.OutboundLimits{MaxChunkStreams: 16, MaxMessageBytes: 1 << 20, MaxChunkBytes: 65536, MaxFragments: 8192, MaxControlBytes: 64, MaxConfigBytes: 4096, MaxAMFBytes: 4096, MaxAMFDepth: 8, MaxAMFElements: 128, ChunkSize: 128, MaxControlPending: 32, MaxRetainedBytes: 32 << 20, DialTimeout: time.Second, TLSTimeout: time.Second, StartupTimeout: 3 * time.Second, MessageTimeout: time.Second, WriteTimeout: time.Second}}
}

// publisherPipeline объединяет доменную сессию, ожидающие выходы и общий вход для структурных проверок
// публикации.
type publisherPipeline struct {
	// service предоставляет доменные операции сессии, выходов и их владельцев выполнения.
	service *core.Service
	// stream содержит конфигурацию потока и постоянный ключ тестового RTMP-входа.
	stream core.Stream
	// parent содержит разрешение работающего родительского выполнения сессии.
	parent core.ExecutionFence
	// observer публикует наблюдения групп от имени текущей сессии.
	observer *core.SessionObserver
	// outputs содержит сохранённые снимки назначений с ожидающими командами запуска выходов.
	outputs []core.SessionOutput
	// live предоставляет общий маршрутизатор принятого LIVE для всех тестовых групп.
	live *media.Router
	// config задаёт бюджеты групп и точки подстановки управляемых медиапроцессов.
	config group.RuntimeConfig
}

// Создаёт изолированные SQLite, работающую Session, ожидающие команды выходов и общий LIVE Router.
// Непустой profiles задаёт группы, endpoints, keys и groupIndices должны соответствовать друг другу, а
// индексы указывают на profiles; данные подготовленных файлов заменены тестовыми манифестами,
// медиаресурсы настраивает вызывающий сценарий.
func newPublisherPipeline(t *testing.T, profiles []core.GroupProfile, endpoints []string, keys []string, groupIndices []int) publisherPipeline {
	t.Helper()
	// ctx используется для построения доменного состояния тестового медиатракта.
	ctx := context.Background()
	// store содержит временное файловое хранилище медиатракта, а err сообщает об ошибке его
	// открытия.
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "publisher.sqlite"), storage.Options{OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 16, MaxIdentifierBytes: 1024, MaxBytes: 1 << 20}, PreparationLimits: core.PreparationLimits{MaxQueued: 4, MaxProfiles: 16}, ObservationLimits: core.ObservationLimits{MaxSessions: 8, MaxGroups: 32, MaxOutputs: 64, MaxFFmpegWorkers: 32, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 20 * time.Millisecond, RetryAttempts: 4, RetryTimeout: time.Second, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает временное хранилище после проверки независимых назначений.

		// e сообщает об ошибке закрытия временного хранилища медиатракта.
		if e := store.Close(); e != nil {
			t.Error(e)
		}
	})
	// svc предоставляет доменные операции над изолированным хранилищем сценария.
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// diagnostics содержит фактические версии SQLite и схемы Goose для диагностики фикстуры.
	diagnostics, err := store.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SQLite=%s Goose=%d", diagnostics.SQLiteVersion, diagnostics.SchemaVersion)
	// asset содержит заглушку с синтетически публикуемыми манифестами вариантов.
	asset, err := svc.CreateAsset(ctx, "Заглушка")
	if err != nil {
		t.Fatal(err)
	}
	// watchdog задаёт сроки пропадания дорожек, восстановления и ожидания ключевого кадра тестовых
	// групп.
	watchdog := core.WatchdogPolicy{VideoTimeout: 300 * time.Millisecond, AudioTimeout: 300 * time.Millisecond, RecoveryWindow: 70 * time.Millisecond, KeyframeTimeout: time.Second}
	// stream содержит поток с постоянным синтетическим ключом и политикой контроля источника.
	stream, err := svc.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "synthetic-ingest-key", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: watchdog}})
	if err != nil {
		t.Fatal(err)
	}
	// groups собирает сохранённые группы в порядке переданных профилей.
	var groups []core.TranscodeGroup
	profiles = append([]core.GroupProfile(nil), profiles...)
	// i задаёт позицию профиля в исходном списке, а p содержит профиль для сохранения отдельной
	// группы.
	for i, p := range profiles {
		// profile содержит сохранённый профиль с доменной нормализацией, а e сообщает об ошибке
		// его создания.
		profile, e := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: p})
		if e != nil {
			t.Fatal(e)
		}
		profiles[i] = profile.Profile
		// g содержит сохранённую группу, связывающую поток с текущим профилем.
		g, e := svc.CreateGroup(ctx, core.TranscodeGroup{Name: "Группа", StreamID: stream.ID, ProfileID: profile.ID})
		if e != nil {
			t.Fatal(e)
		}
		groups = append(groups, g)
	}
	// i задаёт позицию назначения в согласованных списках, а endpoint содержит его исходящий адрес.
	for i, endpoint := range endpoints {
		// e сообщает об отказе создания назначения с выбранной группой и синтетическим ключом.
		_, e := svc.CreateDestination(ctx, core.Destination{Name: "Назначение", StreamID: stream.ID, GroupID: groups[groupIndices[i]].ID, Platform: "custom", Endpoint: endpoint, StreamKey: keys[i], Enabled: true})
		if e != nil {
			t.Fatal(e)
		}
	}
	// gen содержит резерв подготовки и обязательные варианты всех используемых профилей.
	gen, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "controlled-generator"})
	if err != nil {
		t.Fatal(err)
	}
	// pf разрешает синтетическую публикацию подготовленного поколения.
	pf, err := svc.ClaimPreparation(ctx, gen.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkPreparationIssued(ctx, pf); err != nil {
		t.Fatal(err)
	}
	// publication собирает синтетические свойства исходника и вариантов без генерации медиаданных.
	publication := core.GenerationPublication{SourceHash: "controlled-source", SizeBytes: 1, SourceFormat: core.SourceFormat{ImageFormat: "png"}}
	// r обозначает очередной требуемый профиль, для которого добавляется синтетический вариант.
	for _, r := range gen.RequiredProfiles {
		publication.Variants = append(publication.Variants, core.FallbackVariant{ID: r.VariantID, GenerationID: gen.ID, AssetID: asset.ID, SourceHash: publication.SourceHash, Profile: r.Profile, VerifiedFormat: r.Profile.Format, GeneratorVersion: gen.GeneratorVersion, ManagedPath: r.ManagedPath, SizeBytes: 1, ContentHash: "controlled-result"})
	}
	if _, err = svc.PublishPreparation(ctx, pf, publication); err != nil {
		t.Fatal(err)
	}
	if err = svc.AcknowledgePreparationCleanup(ctx, pf); err != nil {
		t.Fatal(err)
	}
	// request собирает запуск сессии с точными подготовленными привязками всех групп.
	request := core.StartSessionRequest{RequestID: "start-publisher-pipeline", StreamID: stream.ID}
	// i задаёт позицию профиля группы, а g содержит группу для привязки к подходящему варианту.
	for i, g := range groups {
		// r обозначает требуемый вариант поколения при поиске профиля текущей группы.
		for _, r := range gen.RequiredProfiles {
			if r.Profile.Equal(profiles[i]) {
				request.Bindings = append(request.Bindings, core.PreparedBinding{GroupID: g.ID, GenerationID: gen.ID, VariantID: r.VariantID, GeneratorVersion: gen.GeneratorVersion})
			}
		}
	}
	// receipt содержит принятую квитанцию запуска сессии со всеми привязками.
	receipt, err := svc.StartSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// parent содержит разрешение запуска сессии, которое фикстура переводит в успешное выполнение.
	parent, err := svc.Claim(ctx, receipt.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkIssued(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err = svc.Complete(ctx, parent, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	// observer регистрирует текущего владельца наблюдений родительской сессии.
	observer, err := svc.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает наблюдателя тестовой Session после завершения выходов и групп.
		_ = observer.Close(context.Background())
	})
	// outputs содержит сохранённые выходы с ожидающими командами запуска для активации сценариями.
	outputs, err := svc.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 32, SessionID: parent.SessionID}})
	if err != nil {
		t.Fatal(err)
	}
	// live предоставляет общий ограниченный маршрутизатор входной публикации.
	live, err := media.NewRouter(publisherRouterLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает входной маршрутизатор после освобождения его потребителей.
		_ = live.Close()
	})
	// base задаёт частный короткий каталог рабочих файлов и UDS тестовых групп.
	base, err := os.MkdirTemp("/tmp", "publisher-group-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Удаляет частный каталог процессов групп после завершения супервизоров.

		// e сообщает об ошибке удаления частного каталога процессов групп.
		if e := os.RemoveAll(base); e != nil {
			t.Error(e)
		}
	})
	// config задаёт общие сроки и медиабюджеты групп, а исполняемые процессы подставляются
	// вызывающим сценарием.
	config := group.RuntimeConfig{Switcher: group.SwitcherConfig{Profile: profiles[0], Watchdog: watchdog, Queue: publisherQueue(), Router: publisherRouterLimits(), MaxAVSkew: 100 * time.Millisecond, MaxTransitionGap: 100 * time.Millisecond}, Container: integrationLimits(), Binary: "controlled", ResubscribeBackoff: 5 * time.Millisecond, ControlInterval: 5 * time.Millisecond, ShutdownTimeout: 3 * time.Second, FFmpeg: ffmpeg.Config{BaseDir: base, StartupTimeout: 3 * time.Second, VideoTimeout: 2 * time.Second, AudioTimeout: 2 * time.Second, WatchdogInterval: 10 * time.Millisecond, MaxRestarts: 0, RestartBackoff: time.Millisecond, GracePeriod: 50 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 3 * time.Second, DiagnosticEvents: 64, DiagnosticBytes: 1 << 16, DiagnosticChunkBytes: 1024, MaxCommandBytes: 8192, MaxCommandArgs: 128}}
	return publisherPipeline{svc, stream, parent, observer, outputs.Items, live, config}
}

// Повторно проверяет условие медиатракта с интервалом одна миллисекунда и завершает тест ошибкой после
// восьми секунд ожидания.
func publisherWait(t *testing.T, condition func() bool) {
	t.Helper()
	// ctx ограничивает ожидание условия медиатракта восемью секундами, а cancel освобождает его
	// таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// ticker задаёт интервал повторной проверки условия медиатракта в одну миллисекунду.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-ctx.Done():
			t.Fatal("условие медиатракта не выполнено до срока")
		case <-ticker.C:
		}
	}
}

// Выдаёт собственную ожидающую команду выхода и подготавливает Publisher с точным владельцем, затем
// подтверждает команду, открывает наблюдателя и активирует сеть. t.Cleanup останавливает Publisher и
// отдельно подтверждает разрешение исполнения перед освобождением резерва.
func publisherActivate(t *testing.T, f publisherPipeline, o core.SessionOutput, router *media.Router, trust *tls.Config, configure ...func(*publisher.Options)) *publisher.Publisher {
	t.Helper()
	// ctx используется для подготовки и подтверждения запуска выбранного выхода.
	ctx := context.Background()
	// receipts содержит квитанции текущей сессии, а err сообщает об ошибке их чтения при поиске
	// команды выхода.
	receipts, err := f.service.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 64, SessionID: f.parent.SessionID}})
	if err != nil {
		t.Fatal(err)
	}
	// requestID сохраняет идентификатор ожидающей команды запуска выбранного выхода.
	var requestID string
	// r обозначает очередную квитанцию сессии для поиска собственного запуска выхода.
	for _, r := range receipts.Items {
		if r.OutputID == o.ID && r.Kind == core.CommandStartOutput && r.Status == core.CommandPending {
			requestID = r.RequestID
		}
	}
	if requestID == "" {
		t.Fatal("нет собственной начальной квитанции выхода")
	}
	// fence содержит точное разрешение выбранной команды запуска выхода.
	fence, err := f.service.Claim(ctx, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// owner связывает отправитель с текущим исполнением выхода через RuntimeLease.
	owner, err := f.service.BindExecutionOwner(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	// options задаёт ограниченные очередь, повторы и транспорт отправителя до дополнительных
	// настроек сценария.
	options := publisher.Options{Queue: publisherQueue(), RetryInitial: 10 * time.Millisecond, RetryMax: 80 * time.Millisecond, ResubscribeDelay: 5 * time.Millisecond, Transport: rtmp.OutboundOptions{}}
	options.Transport = publisherTransportOptions(trust)
	// configure обозначает очередную переданную функцию изменения параметров отправителя перед
	// подготовкой.
	for _, configure := range configure {
		configure(&options)
	}
	// p содержит подготовленный отправитель, который активируется после успешной квитанции и
	// регистрации наблюдателя.
	p, err := publisher.Prepare(ctx, f.service, owner, o.Snapshot, router, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Останавливает отправителя, сохраняет намерение остановки выхода и освобождает его резерв только
		// после подтверждения завершения сети.

		// cleanup ограничивает остановку отправителя и подтверждение управления четырьмя
		// секундами, а cancel освобождает таймер очистки.
		cleanup, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		// err сообщает об ошибке остановки отправителя при очистке фикстуры.
		if err := p.Stop(cleanup); err != nil {
			t.Error(err)
		}
		// err сообщает об отказе сохранения намерения остановки выхода при очистке.
		if _, err := f.service.StopOutput(cleanup, core.OutputCommand{RequestID: "stop-" + string(o.ID), OutputID: o.ID}); err != nil {
			t.Error(err)
		}
		// err сообщает об отказе подтверждения завершения исполнения перед освобождением
		// резерва.
		if err := f.service.ConfirmOutputExecutionResolution(cleanup, p.Reservation()); err != nil {
			t.Error(err)
		}
		// err сообщает об ошибке освобождения резерва завершённого отправителя.
		if err := p.ReleaseJoined(); err != nil {
			t.Error(err)
		}
	})
	if err = f.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	// observer связывает активируемый отправитель с текущим владельцем наблюдений выхода.
	observer, err := f.service.OpenOutputObserver(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(context.Background(), observer); err != nil {
		t.Fatal(err)
	}
	return p
}

// Создаёт структурный AAC, AV1 либо H.264 AU для проверки передачи; marker отличает видео LIVE от
// заглушки. Декодируемость этих нагрузок не устанавливается.
func publisherPayload(c media.CodecConfig, marker byte) []byte {
	if c.Info().Track == media.TrackAudio {
		return []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c}
	}
	if c.Info().Video.Codec == core.CodecAV1 {
		return append(append(c.Headers()[4:], 0x1a, 1, 0x10), 0x2a, 1, marker)
	}
	return []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40, 0, 0, 0, 2, 0x0c, marker}
}

// Строит команду повторного запуска текущего тестового бинарника как управляемого медиапроцесса с
// UDS-путями, форматом, маркером и необязательным файлом остановки.
func publisherControlledCommand(profile core.GroupProfile, a ffmpeg.Attempt, marker byte, control string) (ffmpeg.Command, error) {
	// binary содержит путь текущего тестового бинарника для управляемого медиапроцесса, а err
	// сообщает об ошибке определения пути.
	binary, err := os.Executable()
	if err != nil {
		return ffmpeg.Command{}, err
	}
	return ffmpeg.Command{Binary: binary, Args: []string{"-test.run=^TestPublisherControlledMediaProcess$", "--", a.OutputPath, string(profile.Format.Video.Codec), fmt.Sprint(profile.Format.Video.Width), fmt.Sprint(profile.Format.Video.Height), a.InputPath, fmt.Sprint(marker), control}}, nil
}

// Процесс выдаёт структурные AU: этот сценарий не утверждает декодируемость изображения.
func TestPublisherControlledMediaProcess(t *testing.T) {
	// position хранит индекс разделителя режима дочернего процесса; -1 означает обычный запуск
	// теста.
	position := -1
	// i задаёт позицию аргумента процесса, а a содержит его текст для поиска разделителя --.
	for i, a := range os.Args {
		if a == "--" {
			position = i
			break
		}
	}
	if position < 0 {
		return
	}
	// args содержит параметры UDS, формата, маркера и файлового сигнала управляемого источника.
	args := os.Args[position+1:]
	if len(args) != 7 {
		os.Exit(31)
	}
	// w содержит ширину видеокадра из аргумента процесса, а e сообщает об ошибке её разбора.
	w, e := strconv.ParseUint(args[2], 10, 32)
	if e != nil {
		os.Exit(32)
	}
	// h содержит высоту видеокадра из аргумента процесса.
	h, e := strconv.ParseUint(args[3], 10, 32)
	if e != nil {
		os.Exit(33)
	}
	// marker задаёт байтовый маркер видеонагрузки, отличающий LIVE от заглушки.
	marker, e := strconv.ParseUint(args[5], 10, 8)
	if e != nil {
		os.Exit(34)
	}
	// output связывает управляемый источник с выходным UDS супервизора.
	output, e := net.Dial("unix", args[0])
	if e != nil {
		os.Exit(35)
	}
	defer func() {
		// Закрывает выходной UDS управляемого медиапроцесса.
		_ = output.Close()
	}()
	if args[4] != "" {
		// input предоставляет входной UDS управляемого транскодировщика, а e сообщает об ошибке
		// подключения.
		input, e := net.Dial("unix", args[4])
		if e != nil {
			os.Exit(36)
		}
		defer func() {
			// Закрывает входной UDS при выходе из функции управляемого медиапроцесса.
			_ = input.Close()
		}()
		go func() {
			// Вычитывает входной поток, позволяя проверить транскодировщик с управляемым выходом без декодирования
			// медиа.
			_, _ = io.Copy(io.Discard, input)
		}()
	}
	// p содержит профиль структурного источника по переданным кодеку и размерам.
	p := publisherProfile(core.Codec(args[1]), uint32(w), uint32(h))
	// v содержит видеоконфигурацию структурного источника, а a — соответствующую AAC-конфигурацию.
	v, a := publisherConfigs(t, p, 1)
	// mux записывает структурные A/V-пакеты управляемого источника в fMP4 через выходной UDS.
	mux, e := fmp4.NewMuxer(output, fmp4.Init{Video: v, Audio: a}, integrationLimits())
	if e != nil {
		os.Exit(37)
	}
	// ticker задаёт темп шестидесяти видеоинтервалов в секунду для управляемого источника.
	ticker := time.NewTicker(time.Second / 60)
	defer ticker.Stop()
	// audio хранит следующую AAC-метку в отсчётах 48000 Гц с шагом 1024.
	var audio int64
	// video задаёт номер очередного видеоинтервала и его DTS/PTS при частоте 60 кадров в секунду.
	for video := int64(0); ; video++ {
		if args[6] != "" {
			if _, e = os.Stat(args[6]); e == nil {
				return
			}
		}
		// packet содержит очередную структурную единицу доступа для fMP4, а e сообщает об ошибке
		// её создания.
		packet, e := media.NewPacket(media.PacketInfo{PTS: video, DTS: video, DurationTicks: 1, Keyframe: true}, v, publisherPayload(v, byte(marker)))
		if e != nil || mux.WritePacket(packet) != nil {
			return
		}
		for audio*60 < (video+1)*48000 {
			packet, e = media.NewPacket(media.PacketInfo{PTS: audio, DTS: audio, DurationTicks: 1024}, a, publisherPayload(a, 0))
			if e != nil || mux.WritePacket(packet) != nil {
				return
			}
			audio += 1024
		}
		<-ticker.C
	}
}

// Запускает локальный сервер приёма RTMP и публикацию заданных AV1/AAC-пакетов с темпом по DTS и
// начальным сдвигом 200 мс. Возвращает клиента, отмену производителя и канал его завершения; t.Cleanup
// останавливает производителя и сервер.
func publisherStartIngest(t *testing.T, f publisherPipeline, packets []media.Packet, init fmp4.Init) (*gortmplib.Client, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	// ctx управляет сроком обслуживания локального RTMP-входа, а cancel запрашивает его остановку.
	ctx, cancel := context.WithCancel(context.Background())
	// server принимает тестовую RTMP-публикацию в общий LIVE-маршрутизатор, а err сообщает об ошибке
	// его создания.
	server, err := rtmp.NewServer(rtmp.Options{Limits: publisherRTMPLimits(), Core: f.service, Route: rtmp.ConventionalRoute, Feed: func(core.Session) (*media.Router, error) {
		// Возвращает общий тестовый маршрутизатор для принятой публикации RTMP.
		return f.live, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	// l предоставляет свободный локальный TCP-порт тестового RTMP-входа.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// serveDone передаёт результат Serve для ограниченного ожидания завершения сервера.
	serveDone := make(chan error, 1)
	go func() {
		// Обслуживает тестовый RTMP-вход параллельно публикации пакетов.
		serveDone <- server.Serve(ctx, l)
	}()
	t.Cleanup(func() {
		// Отменяет приёмник и ограниченно ожидает его завершения.

		cancel()
		// cleanup ограничивает завершение входного сервера четырьмя секундами, а stop
		// освобождает таймер очистки.
		cleanup, stop := context.WithTimeout(context.Background(), 4*time.Second)
		defer stop()
		// e сообщает об ошибке завершения активных обработчиков RTMP-входа.
		if e := server.Shutdown(cleanup); e != nil {
			t.Error(e)
		}
		select {
		// serveErr содержит итог Serve после отмены для проверки допустимых причин завершения.
		case serveErr := <-serveDone:
			if serveErr != nil && !errors.Is(serveErr, context.Canceled) && !errors.Is(serveErr, net.ErrClosed) {
				t.Errorf("ingest Serve: %v", serveErr)
			}
		case <-cleanup.Done():
			t.Error("ingest не присоединён")
		}
	})
	// client публикует пакеты на локальный RTMP-вход с идентификатором и постоянным ключом тестового
	// потока.
	client := &gortmplib.Client{URL: &url.URL{Scheme: "rtmp", Host: l.Addr().String(), Path: "/" + string(f.stream.ID), Fragment: f.stream.PublicationKey}, Publish: true}
	if err = client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err = client.Write(&message.SetChunkSize{Value: 128}); err != nil {
		t.Fatal(err)
	}
	if err = client.Write(&message.DataAMF0{ChunkStreamID: 5, MessageStreamID: 1, Payload: amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: float64(init.Video.Info().Video.FrameRate.Numerator) / float64(init.Video.Info().Video.FrameRate.Denominator)}}}}); err != nil {
		t.Fatal(err)
	}
	if err = publisherWriteMessage(client.NetConn(), 9, 1, 0, append([]byte{0x90, 'a', 'v', '0', '1'}, init.Video.Headers()...)); err != nil {
		t.Fatal(err)
	}
	if err = publisherWriteMessage(client.NetConn(), 8, 1, 0, append([]byte{0xaf, 0}, init.Audio.Headers()...)); err != nil {
		t.Fatal(err)
	}
	// done закрывается при завершении производителя RTMP-пакетов для отдельного ожидания его
	// горутины.
	done := make(chan struct{})
	// producer задаёт срок подачи исходных пакетов, а stop отменяет производителя после проверки
	// сценария.
	producer, stop := context.WithCancel(ctx)
	go func() {
		// Отправляет исходные пакеты по их времени, прекращает передачу при отмене или ошибке и закрывает
		// клиента при выходе.

		defer close(done)
		defer client.Close()
		// start задаёт начало реального времени подачи исходных пакетов по их DTS.
		start := time.Now()
		// p обозначает очередной исходный пакет, отправляемый через RTMP с сохранением темпа.
		for _, p := range packets {
			// at содержит RTMP-метку пакета в миллисекундах с начальным сдвигом 200 мс.
			at := publisherNearestMS(p.Info().DTS, p.Config().Info().TimeBase) + 200
			// delay содержит оставшееся время до подачи текущего пакета относительно начала
			// производителя.
			delay := time.Until(start.Add(time.Duration(at-200) * time.Millisecond))
			// timer ожидает срок подачи пакета и позволяет прекратить ожидание по отмене
			// производителя.
			timer := time.NewTimer(max(delay, 0))
			select {
			case <-producer.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			// body содержит RTMP-тело текущей единицы доступа с заголовком соответствующей
			// дорожки.
			body := append([]byte{0xaf, 1}, p.Payload()...)
			// typ задаёт RTMP-тип аудио 8 либо видео 9 для текущего пакета.
			typ := byte(8)
			if p.Config().Info().Track == media.TrackVideo {
				typ = 9
				// flag задаёт Enhanced RTMP-флаг видеопакета с учётом переданного
				// признака ключевого кадра.
				flag := byte(0xa1)
				if p.Info().Keyframe {
					flag = 0x91
				}
				body = append([]byte{flag, 'a', 'v', '0', '1'}, p.Payload()...)
			}
			// e сообщает об отказе записи текущего RTMP-пакета, после которого производитель
			// прекращает подачу.
			if e := publisherWriteMessage(client.NetConn(), typ, 1, uint32(at), body); e != nil {
				return
			}
		}
		<-producer.Done()
	}()
	t.Cleanup(func() {
		// Отменяет тестовый источник и ограниченно ожидает завершения отправки.

		stop()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("источник не завершён")
		}
	})
	return client, stop, done
}

// Создаёт заданное число независимых структурных AV1-кадров 1440p60 с соответствующим AAC и проверяет
// видеопакеты Inspector. Возвращает пакеты и fMP4-конфигурации для приёма RTMP.
func publisherStructuralInput(t *testing.T, frames int) ([]media.Packet, fmp4.Init) {
	t.Helper()
	// v содержит AV1-конфигурацию 1440p60, а a — AAC-конфигурацию структурного входа.
	v, a := publisherConfigs(t, publisherProfile(core.CodecAV1, 2560, 1440), 1)
	// result собирает структурные видео- и аудиопакеты для публикации на тестовый вход.
	var result []media.Packet
	// audio хранит следующую аудиометку в отсчётах 48000 Гц при построении структурного входа.
	var audio int64
	// inspector проверяет структуру AV1-пакетов и их независимость для тестового входа.
	var inspector codec.Inspector
	// video задаёт номер создаваемого видеокадра и его метки в шкале 60 Гц.
	for video := int64(0); video < int64(frames); video++ {
		// p содержит создаваемый структурный пакет, а e сообщает об ошибке его построения.
		p, e := media.NewPacket(media.PacketInfo{PTS: video, DTS: video, DurationTicks: 1, Keyframe: true}, v, publisherPayload(v, 20))
		if e != nil {
			t.Fatal(e)
		}
		// key содержит определённый Inspector признак независимого кадра, а e сообщает об ошибке
		// анализа структуры AV1.
		if key, e := inspector.Inspect(p); e != nil || !key {
			t.Fatalf("неверная структурная video фикстура: %v", e)
		}
		result = append(result, p)
		for audio*60 < (video+1)*48000 {
			p, e = media.NewPacket(media.PacketInfo{PTS: audio, DTS: audio, DurationTicks: 1024}, a, publisherPayload(a, 0))
			if e != nil {
				t.Fatal(e)
			}
			result = append(result, p)
			audio += 1024
		}
	}
	return result, fmp4.Init{Video: v, Audio: a}
}

// Подсчитывает сообщения с AAC AU либо H.264/Enhanced AV1 видеоданными, исключая конфигурации и
// служебные сообщения.
func publisherMediaCount(messages []publisherWireMessage) int {
	// n считает сообщения с медианагрузкой без конфигураций и служебных сообщений.
	n := 0
	// m обозначает очередное RTMP-сообщение при подсчёте аудио- и видеоданных.
	for _, m := range messages {
		if (m.typ == 8 && len(m.body) > 1 && m.body[1] == 1) || (m.typ == 9 && len(m.body) > 5 && m.body[1] == 1) || (m.typ == 9 && len(m.body) > 5 && m.body[0]&0x80 != 0 && m.body[0]&15 == 1) {
			n++
		}
	}
	return n
}

// Проверяет разделение обработки трёх профилей четырьмя RTMP/RTMPS-назначениями и переходы при паузе,
// возобновлении и замене входящей публикации. Отказ одной медиагруппы и переподключение одного
// назначения не пересоздают здоровые соединения; проверяются переданные заголовки, нагрузки и формат.
func TestPublisherIngestGroupsAndIndependentDestinations(t *testing.T) {
	// profiles задаёт три профиля: AV1 1440p60 и H.264 в размерах 1440p60 и 1080p60.
	profiles := []core.GroupProfile{publisherProfile(core.CodecAV1, 2560, 1440), publisherProfile(core.CodecH264, 2560, 1440), publisherProfile(core.CodecH264, 1920, 1080)}
	// receivers собирает четыре независимых локальных приёмника публикаций.
	var receivers []*publisherReceiver
	// trusts хранит TLS-доверие соответствующих приёмников; обычные RTMP-приёмники используют nil.
	var trusts []*tls.Config
	// endpoints собирает исходящие RTMP/RTMPS-адреса четырёх тестовых назначений.
	var endpoints []string
	// i задаёт номер приёмника; четвёртый приёмник использует TLS.
	for i := range 4 {
		// r содержит очередной локальный приёмник, а trust — TLS-конфигурацию доверия к нему
		// либо nil.
		r, trust := newPublisherReceiver(t, i == 3)
		receivers = append(receivers, r)
		trusts = append(trusts, trust)
		// scheme выбирает rtmp либо rtmps для адреса текущего приёмника.
		scheme := "rtmp"
		if trust != nil {
			scheme = "rtmps"
		}
		endpoints = append(endpoints, scheme+"://"+r.listener.Addr().String()+"/live%2Fapp?token=synthetic")
	}
	// f предоставляет сессию с тремя профилями и четырьмя назначениями для проверки общего
	// кодирования и изоляции отказов.
	f := newPublisherPipeline(t, profiles, endpoints, []string{"copy-key", "shared-one", "shared-two", "tls-key"}, []int{0, 1, 1, 2})
	// held сопоставляет открытые тестовые файлы с профилями для передачи профиля управляемому
	// проигрывателю.
	var held sync.Map
	// players считает созданные проигрыватели заглушки, а transcodes — управляемые транскодировщики
	// групп.
	var players, transcodes atomic.Int32
	// control задаёт путь файлового сигнала отказа транскодировщика 1080p.
	control := filepath.Join(t.TempDir(), "fail-worker")
	f.config.OpenFile = func(_ context.Context, m core.ArtifactManifest) (*os.File, error) {
		// Создаёт временный файл и связывает его с профилем для последующего управляемого проигрывания.

		// file содержит временный дескриптор тестового варианта, а e сообщает об ошибке его
		// создания перед привязкой профиля.
		file, e := os.CreateTemp(t.TempDir(), "held")
		if e == nil {
			held.Store(file, m.Profile)
		}
		return file, e
	}
	f.config.Player = func(_ context.Context, _ string, file *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Забирает профиль открытого файла, считает проигрыватели и создаёт источник заглушки с отличимым
		// маркером.

		// p содержит профиль ранее открытого файла, а ok подтверждает наличие точной привязки
		// для команды проигрывателя.
		p, ok := held.LoadAndDelete(file)
		if !ok {
			return ffmpeg.Command{}, core.ErrCorrupt
		}
		players.Add(1)
		return publisherControlledCommand(p.(core.GroupProfile), a, 10, "")
	}
	f.config.Transcode = func(_ context.Context, n ffmpeg.NVIDIAConfig, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Считает транскодировщики и добавляет файловый сигнал отказа только группе с шириной 1920.

		transcodes.Add(1)
		// path задаёт файловый сигнал завершения только для транскодировщика шириной 1920;
		// остальным группам передаётся пустая строка.
		path := ""
		if n.Profile.Format.Video.Width == 1920 {
			path = control
		}
		return publisherControlledCommand(n.Profile, a, 20, path)
	}
	// registry создаёт общие группы по снимкам выходов, а err сообщает об ошибке его создания.
	registry, err := group.NewRegistry(context.Background(), group.RegistryConfig{Core: f.service, Parent: f.parent, Observer: f.observer, Runtime: f.config, MaxGroups: 4, MaxReferences: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает реестр групп после остановки их отправителей.

		// e сообщает об ошибке завершения реестра общих групп.
		if e := registry.Close(context.Background()); e != nil {
			t.Error(e)
		}
	})
	// refs сопоставляет назначение и ключ со ссылкой на его общую группу.
	refs := map[string]*group.Reference{}
	// pubs хранит активированные отправители по адресам назначений для итогового завершения сети.
	pubs := map[string]*publisher.Publisher{}
	// o обозначает очередной сохранённый выход при получении ссылки на группу и активации
	// отправителя.
	for _, o := range f.outputs {
		// ref удерживает группу выбранного выхода, а e сообщает об отказе получения этой ссылки.
		ref, e := registry.Acquire(context.Background(), o.Snapshot, f.live)
		if e != nil {
			t.Fatal(e)
		}
		refs[o.Snapshot.Destination.Endpoint+o.Snapshot.Destination.StreamKey] = ref
		// index сохраняет позицию адреса выхода в списке приёмников для выбора TLS-доверия.
		index := -1
		// i задаёт позицию адреса приёмника, а endpoint содержит проверяемый адрес при поиске
		// соответствия выходу.
		for i, endpoint := range endpoints {
			if endpoint == o.Snapshot.Destination.Endpoint {
				index = i
			}
		}
		if index < 0 {
			t.Fatal("неизвестный endpoint")
		}
		// p содержит активированный отправитель текущего назначения с маршрутизатором его
		// группы.
		p := publisherActivate(t, f, o, ref.Router(), trusts[index])
		pubs[o.Snapshot.Destination.Endpoint] = p
	}
	// groups собирает уникальные экземпляры групп для проверки общего использования профиля двумя
	// назначениями.
	groups := map[*group.Group]bool{}
	// ref обозначает очередную ссылку выхода при построении множества уникальных групп.
	for _, ref := range refs {
		groups[ref.Group()] = true
	}
	if len(groups) != 3 {
		t.Fatalf("ожидались три shared группы: %d", len(groups))
	}
	if refs[endpoints[1]+"shared-one"].Group() != refs[endpoints[2]+"shared-two"].Group() {
		t.Fatal("два назначения H264-профиля не разделяют результат одного encoder")
	}
	// waitSource ожидает доступность всех уникальных групп на переданном источнике LIVE либо
	// FALLBACK.
	waitSource := func(want group.Feed) {
		// Ожидает готовность всех трёх групп на выбранном источнике.

		publisherWait(t, func() bool {
			// Проверяет готовность и источник каждой уникальной общей группы.

			// g обозначает очередную уникальную группу при ожидании общего перехода на
			// заданный источник.
			for g := range groups {
				if !g.Status().Available || g.Status().Source != want {
					return false
				}
			}
			return true
		})
	}
	waitSource(group.Fallback)
	// r обозначает очередной локальный приёмник при ожидании начальных медиасообщений.
	for _, r := range receivers {
		r.wait(t, func(m []publisherWireMessage) bool {
			// Ожидает начальную передачу на всех независимых назначениях.
			return publisherMediaCount(m) >= 8
		})
	}
	if players.Load() != 3 || transcodes.Load() != 0 {
		t.Fatalf("до OBS players=%d transcodes=%d", players.Load(), transcodes.Load())
	}
	// packets содержит структурный вход на 1200 видеокадров, а init — его видео- и аудиоконфигурации
	// для RTMP-публикации.
	packets, init := publisherStructuralInput(t, 1200)
	// stop отменяет первую входную публикацию после проверки паузы и продолжения.
	_, stop, _ := publisherStartIngest(t, f, packets, init)
	defer stop()
	waitSource(group.Live)
	if transcodes.Load() != 2 {
		t.Fatalf("для двух H264-профилей запущено %d workers", transcodes.Load())
	}
	// assertHealthy проверяет, что у каждого исправного назначения сохранилось единственное исходное
	// соединение.
	assertHealthy := func() {
		// Проверяет сохранение единственного соединения каждого исправного назначения при переключениях
		// источника.

		t.Helper()
		// i задаёт номер назначения для диагностики, а r содержит его приёмник при проверке
		// отсутствия переподключений.
		for i, r := range receivers {
			if r.accepted.Load() != 1 {
				t.Fatalf("здоровое назначение %d переподключено: %d", i, r.accepted.Load())
			}
		}
	}
	assertHealthy()
	if _, err = f.service.PauseSession(context.Background(), core.SessionCommand{RequestID: "pause-pipeline", SessionID: f.parent.SessionID}); err != nil {
		t.Fatal(err)
	}
	waitSource(group.Fallback)
	if _, err = f.service.ResumeSession(context.Background(), core.SessionCommand{RequestID: "resume-pipeline", SessionID: f.parent.SessionID}); err != nil {
		t.Fatal(err)
	}
	waitSource(group.Live)
	assertHealthy()
	stop()
	waitSource(group.Fallback)
	// stop2 отменяет вторую входную публикацию, открытую после разрыва первой.
	_, stop2, _ := publisherStartIngest(t, f, packets, init)
	defer stop2()
	waitSource(group.Live)
	assertHealthy()
	// epochs фиксирует выходную эпоху каждой группы перед заменой входной публикации.
	epochs := map[*group.Group]uint64{}
	// g обозначает очередную группу при сохранении эпох до замены входа.
	for g := range groups {
		epochs[g] = g.Status().Epoch
	}
	// stop3 отменяет третью входную публикацию, заменившую вторую.
	_, stop3, _ := publisherStartIngest(t, f, packets, init)
	defer stop3()
	publisherWait(t, func() bool {
		// Ожидает новую эпоху LIVE во всех группах после замены входной публикации.

		// g обозначает очередную группу при ожидании новой LIVE-эпохи после замены входа.
		for g := range groups {
			// s содержит текущее состояние группы для сравнения её эпохи с прежней.
			s := g.Status()
			if !s.Available || s.Source != group.Live || s.Epoch <= epochs[g] {
				return false
			}
		}
		return true
	})
	assertHealthy()
	if err = os.WriteFile(control, []byte("stop"), 0600); err != nil {
		t.Fatal(err)
	}
	// failedGroup содержит группу 1080p, чей транскодировщик завершается по файловому сигналу.
	failedGroup := refs[endpoints[3]+"tls-key"].Group()
	publisherWait(t, func() bool {
		// Ожидает переход группы отказавшего транскодировщика на готовую заглушку.
		// s содержит состояние группы отказавшего транскодировщика при ожидании готовой
		// заглушки.
		s := failedGroup.Status()
		return s.Source == group.Fallback && s.Available
	})
	// g обозначает очередную группу для проверки сохранения LIVE у соседей отказавшего
	// транскодировщика.
	for g := range groups {
		if g != failedGroup && (!g.Status().Available || g.Status().Source != group.Live) {
			t.Fatal("отказ 1080p worker изменил здоровую группу")
		}
	}
	assertHealthy()
	// before фиксирует число медиасообщений первого назначения до разрыва соседнего выхода.
	before := publisherMediaCount(receivers[0].snapshot())
	receivers[1].disconnect()
	receivers[1].wait(t, func(m []publisherWireMessage) bool {
		// Ожидает аудиопакет на новом соединении отключённого назначения.

		// v обозначает очередное принятое сообщение при поиске AAC на новом соединении
		// разорванного назначения.
		for _, v := range m {
			if v.connection >= 2 && v.typ == 8 && len(v.body) > 1 && v.body[1] == 1 {
				return true
			}
		}
		return false
	})
	receivers[0].wait(t, func(m []publisherWireMessage) bool {
		// Проверяет продолжение соседнего назначения во время переподключения.
		return publisherMediaCount(m) > before+4
	})
	// i задаёт индекс исправного назначения, которое не должно переподключаться из-за разрыва
	// соседа.
	for _, i := range []int{0, 2, 3} {
		if receivers[i].accepted.Load() != 1 {
			t.Fatalf("reconnect изменил соседа %d", i)
		}
	}
	// i задаёт позицию назначения для выбора ожидаемого профиля, а r содержит его приёмник для
	// проверки принятого RTMP.
	for i, r := range receivers {
		publisherAssertStructuralWire(t, r.snapshot(), profiles[[]int{0, 1, 1, 2}[i]])
		r.mu.Lock()
		// app обозначает очередное принятое значение RTMP-поля app для проверки сохранения
		// экранированного пути и строки запроса.
		for _, app := range r.apps {
			if app != "live%2Fapp?token=synthetic" {
				t.Errorf("escaped app изменён: %q", app)
			}
		}
		r.mu.Unlock()
	}
	// p обозначает очередной активированный отправитель при итоговой остановке публикаций.
	for _, p := range pubs {
		if err = p.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		// result содержит итог отправителя с подтверждением завершения сети, а e сообщает об
		// ошибке ожидания.
		result, e := p.Wait(context.Background())
		if e != nil || !result.NetworkJoined {
			t.Fatalf("нет physical join: %+v %v", result, e)
		}
	}
}

// Проверяет метаданные, ASC и видеозаголовки и побайтную сохранность структурных LIVE/FALLBACK AU в
// принятом RTMP. Заголовки должны предшествовать данным, а видеометки внутри каждого соединения не
// убывают.
func publisherAssertStructuralWire(t *testing.T, messages []publisherWireMessage, p core.GroupProfile) {
	t.Helper()
	// v содержит ожидаемую видеоконфигурацию проверяемого профиля, а a — ожидаемую AAC-конфигурацию.
	v, a := publisherConfigs(t, p, 1)
	// seenVideo отмечает видеозаголовок, seenAudio — ASC, seenLive — нагрузку LIVE, seenFallback —
	// нагрузку заглушки, а seenMetadata — проверенные метаданные в принятом RTMP.
	seenVideo, seenAudio, seenLive, seenFallback, seenMetadata := false, false, false, false, false
	// last хранит последнюю видеометку отдельно для каждого соединения приёмника.
	last := map[string]uint32{}
	// m обозначает очередное принятое RTMP-сообщение для проверки метаданных, конфигураций и
	// нагрузки.
	for _, m := range messages {
		if m.typ == 18 {
			// values содержит разобранные AMF-значения метаданных, а e сообщает об ошибке их
			// разбора.
			values, e := publisherAMFValues(m.body)
			if e != nil || len(values) != 3 {
				t.Fatalf("неверная metadata: %v", e)
			}
			// object содержит объект метаданных RTMP, а ok подтверждает ожидаемый тип для
			// проверки формата.
			object, ok := values[2].(map[string]any)
			if !ok || object["width"] != float64(p.Format.Video.Width) || object["height"] != float64(p.Format.Video.Height) || object["framerate"] != float64(60) || object["audiosamplerate"] != float64(48000) {
				t.Fatalf("metadata не отражает проверенный формат: %+v", object)
			}
			seenMetadata = true
		}
		if m.typ != 8 && m.typ != 9 {
			continue
		}
		if m.stream != 1 {
			t.Fatalf("MessageStreamID не little-endian: %d", m.stream)
		}
		if m.typ == 8 {
			if len(m.body) < 2 || m.body[0] != 0xaf {
				t.Fatal("неверный AAC binding")
			}
			switch m.body[1] {
			case 0:
				if !bytes.Equal(m.body[2:], a.Headers()) {
					t.Fatal("ASC изменён")
				}
				seenAudio = true
			case 1:
				if !seenAudio || !bytes.Equal(m.body[2:], publisherPayload(a, 0)) {
					t.Fatal("AAC AU изменён или пришёл без ASC")
				}
			}
			continue
		}
		if len(m.body) < 5 {
			t.Fatal("неполное video body")
		}
		// header показывает, содержит ли видеосообщение конфигурацию кодека вместо единицы
		// доступа.
		header := m.body[1] == 0
		// payload содержит видеозаголовки либо полезную нагрузку без пятибайтовой RTMP-обвязки.
		payload := m.body[5:]
		if p.Format.Video.Codec == core.CodecAV1 {
			if !bytes.Equal(m.body[1:5], []byte("av01")) {
				t.Fatal("нет Enhanced av01")
			}
			header = m.body[0]&15 == 0
			if m.body[0]&15 != 0 && m.body[0]&15 != 1 {
				t.Fatal("неверный AV1 packet type")
			}
		} else if m.body[0] != 0x17 {
			t.Fatal("структурный независимый H264 AU не помечен keyframe")
		}
		if header {
			if !bytes.Equal(payload, v.Headers()) {
				t.Fatal("actual video config изменён")
			}
			seenVideo = true
			continue
		}
		if !seenVideo {
			t.Fatal("AU пришёл до sequence header")
		}
		if bytes.Equal(payload, publisherPayload(v, 10)) {
			seenFallback = true
		} else if bytes.Equal(payload, publisherPayload(v, 20)) {
			seenLive = true
		} else {
			t.Fatalf("video payload изменён: %x", payload)
		}
		// key выделяет видеошкалу конкретного соединения для проверки неубывающих меток.
		key := fmt.Sprintf("%d/video", m.connection)
		// prev содержит предыдущую видеометку этого соединения, а ok показывает наличие ранее
		// принятого видеопакета.
		if prev, ok := last[key]; ok && m.timestamp < prev {
			t.Fatalf("здоровый socket сбросил timeline: %d -> %d", prev, m.timestamp)
		}
		last[key] = m.timestamp
	}
	if !seenMetadata || !seenVideo || !seenAudio || !seenLive || !seenFallback {
		t.Fatalf("неполный выход: video=%v audio=%v live=%v fallback=%v", seenVideo, seenAudio, seenLive, seenFallback)
	}
}
