package group

import (
	"bytes"
	"context"
	"errors"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runtimeFixture объединяет точный сохранённый выход, настройки реестра и контрольные конфигурации
// медиадорожек.
type runtimeFixture struct {
	// config задаёт доменный сервис, наблюдателя, бюджеты и управляемые порты групп.
	config RegistryConfig
	// snapshot содержит единственный закреплённый снимок выхода работающей Session.
	snapshot core.OutputSnapshot
	// v содержит контрольную AV1-конфигурацию; a содержит согласованную AAC-конфигурацию.
	v, a media.CodecConfig
}

// Создаёт временную SQLite и завершённый допуск Session с удержанным снимком выхода, наблюдателем и
// настройками групп без доступного FFmpeg.
func runtimeUnitFixture(t *testing.T) runtimeFixture {
	t.Helper()
	// sw предоставляет контрольные настройки переключателя; v и a содержат проверенные видео- и
	// аудиоконфигурации.
	sw, v, a, _ := switcherFixture(t)
	// ctx задаёт общий контекст подготовки доменных данных теста.
	ctx := context.Background()
	// options задаёт конечные бюджеты SQLite, подготовок, наблюдений и исполнений выходов.
	options := storage.Options{PreparationLimits: core.PreparationLimits{MaxQueued: 2, MaxProfiles: 8}, OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 128, MaxIdentifierBytes: 1024, MaxBytes: 4 << 20}, ObservationLimits: core.ObservationLimits{MaxSessions: 8, MaxGroups: 16, MaxOutputs: 16, MaxFFmpegWorkers: 16, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond}
	// store содержит временное SQLite-хранилище; err сохраняет результаты его открытия и последующей
	// подготовки доменных сущностей.
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "group.sqlite"), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает временное SQLite-хранилище тестового Session, игнорируя ошибку.
		_ = store.Close()
	})
	// svc предоставляет доменные операции над временным хранилищем и общей RuntimeAuthority.
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// asset содержит исходный Asset контрольной заглушки.
	asset, err := svc.CreateAsset(ctx, "Заглушка")
	if err != nil {
		t.Fatal(err)
	}
	// stream содержит Stream с постоянным тестовым ключом и политикой заглушки.
	stream, err := svc.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "unit-key", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: sw.config.Watchdog}})
	if err != nil {
		t.Fatal(err)
	}
	// profile содержит сохранённый copy-профиль контрольных AV1/AAC.
	profile, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: sw.config.Profile})
	if err != nil {
		t.Fatal(err)
	}
	// group связывает Stream с сохранённым профилем.
	group, err := svc.CreateGroup(ctx, core.TranscodeGroup{Name: "Группа", StreamID: stream.ID, ProfileID: profile.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.CreateDestination(ctx, core.Destination{Name: "Назначение", StreamID: stream.ID, GroupID: group.ID, Platform: "custom", Endpoint: "rtmp://example.invalid/live", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// gen содержит зарезервированное поколение подготовки контрольного артефакта.
	gen, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "unit-generator"})
	if err != nil {
		t.Fatal(err)
	}
	// prep содержит точный допуск исполнения подготовки для публикации и подтверждения очистки.
	prep, err := svc.ClaimPreparation(ctx, gen.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkPreparationIssued(ctx, prep); err != nil {
		t.Fatal(err)
	}
	// required содержит требуемый вариант единственного профиля зарезервированного поколения.
	required := gen.RequiredProfiles[0]
	// publication задаёт готовый контрольный вариант с точным профилем и манифестом.
	publication := core.GenerationPublication{SourceHash: "unit-source", SizeBytes: 1, SourceFormat: core.SourceFormat{ImageFormat: "png"}, Variants: []core.FallbackVariant{{ID: required.VariantID, GenerationID: gen.ID, AssetID: asset.ID, SourceHash: "unit-source", Profile: required.Profile, VerifiedFormat: required.Profile.Format, GeneratorVersion: gen.GeneratorVersion, ManagedPath: required.ManagedPath, SizeBytes: 1, ContentHash: "unit-result"}}}
	if _, err = svc.PublishPreparation(ctx, prep, publication); err != nil {
		t.Fatal(err)
	}
	if err = svc.AcknowledgePreparationCleanup(ctx, prep); err != nil {
		t.Fatal(err)
	}
	// receipt содержит принятую квитанцию запуска Session с выбранной подготовленной привязкой.
	receipt, err := svc.StartSession(ctx, core.StartSessionRequest{RequestID: "unit-start", StreamID: stream.ID, Bindings: []core.PreparedBinding{{GroupID: group.ID, GenerationID: gen.ID, VariantID: required.VariantID, GeneratorVersion: gen.GeneratorVersion}}})
	if err != nil {
		t.Fatal(err)
	}
	// parent содержит точную идентичность исполнения команды запуска Session.
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
	// observer содержит действующего наблюдателя запущенной Session.
	observer, err := svc.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает наблюдателя Session с фоновым контекстом после сценария, игнорируя ошибку.
		_ = observer.Close(context.Background())
	})
	// outputs содержит страницу единственного выхода с закреплённым снимком артефакта.
	outputs, err := svc.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: parent.SessionID}})
	if err != nil || len(outputs.Items) != 1 {
		t.Fatal(outputs, err)
	}
	// c содержит настройки реестра групп с недоступным файловым проигрывателем для модульных
	// сценариев.
	c := RegistryConfig{Core: svc, Parent: parent, Observer: observer, MaxGroups: 2, MaxReferences: 2, Runtime: RuntimeConfig{Switcher: sw.config, Container: fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 65536, MaxMdatBytes: 1 << 20, MaxSampleBytes: 65536, MaxExtradataBytes: 4096, MaxSamples: 100}, Binary: "unavailable-unit-ffmpeg", OpenFile: func(context.Context, core.ArtifactManifest) (*os.File, error) {
		// Возвращает os.ErrNotExist вместо открытия подготовленного файла, исключая запуск проигрывателя в
		// модульном сценарии.
		return nil, os.ErrNotExist
	}, ResubscribeBackoff: time.Millisecond, ControlInterval: time.Millisecond, ShutdownTimeout: time.Second, FFmpeg: ffmpeg.Config{BaseDir: filepath.Join(t.TempDir(), "sockets"), StartupTimeout: time.Second, VideoTimeout: time.Second, AudioTimeout: time.Second, WatchdogInterval: time.Millisecond, RestartBackoff: time.Millisecond, GracePeriod: 10 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: time.Second, DiagnosticEvents: 32, DiagnosticBytes: 65536, DiagnosticChunkBytes: 1024, MaxCommandBytes: 8192, MaxCommandArgs: 64}}}
	return runtimeFixture{c, outputs.Items[0].Snapshot, v, a}
}

// Проверяет точный удержанный снимок, общий предел ссылок, запрет другого LIVE Router, однократное
// закрытие и создание новой группы после завершения старой.
func TestRegistryRejectsUnheldSnapshotAndBoundsReferences(t *testing.T) {
	// f предоставляет сохранённый точный снимок и настройки реестра для проверки границ ссылок.
	f := runtimeUnitFixture(t)
	// ctx задаёт контекст последовательных операций получения и освобождения ссылок.
	ctx := context.Background()
	// err содержит ожидаемый отказ создания реестра без обязательных зависимостей.
	if _, err := NewRegistry(ctx, RegistryConfig{}); !errors.Is(err, core.ErrInvalid) {
		t.Fatal(err)
	}
	// registry содержит проверяемый реестр; err сохраняет результаты его создания, получения ссылок
	// и закрытия.
	registry, err := NewRegistry(ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает Registry после проверки ссылок с фоновым контекстом, игнорируя ошибку.
		_ = registry.Close(context.Background())
	})
	// live содержит первоначальный Router LIVE, привязываемый к общей группе.
	live, _ := media.NewRouter(f.config.Runtime.Switcher.Router)
	defer func() {
		// Закрывает исходный LIVE Router после сценария, игнорируя ошибку.
		_ = live.Close()
	}()
	if _, err = registry.Acquire(ctx, f.snapshot, nil); !errors.Is(err, core.ErrInvalid) {
		t.Fatal(err)
	}
	// forged содержит копию снимка с неподтверждённым поколением артефакта.
	forged := f.snapshot
	forged.Artifact.GenerationID = "unheld-generation"
	if _, err = registry.Acquire(ctx, forged, live); !errors.Is(err, core.ErrStale) {
		t.Fatal("принят чужой manifest", err)
	}
	// first удерживает первую ссылку на общую группу.
	first, err := registry.Acquire(ctx, f.snapshot, live)
	if err != nil {
		t.Fatal(err)
	}
	// other содержит другой Router для проверки конфликта LIVE одной и той же группы.
	other, _ := media.NewRouter(f.config.Runtime.Switcher.Router)
	defer func() {
		// Закрывает второй Router, использованный для проверки конфликта источника, игнорируя ошибку.
		_ = other.Close()
	}()
	if _, err = registry.Acquire(ctx, f.snapshot, other); !errors.Is(err, core.ErrConflict) {
		t.Fatal(err)
	}
	// second удерживает вторую ссылку на ту же группу и достигает общего лимита ссылок.
	second, err := registry.Acquire(ctx, f.snapshot, live)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registry.Acquire(ctx, f.snapshot, live); !errors.Is(err, media.ErrRouterBudget) {
		t.Fatal("превышен общий бюджет ссылок", err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	if err = second.Close(); err != nil {
		t.Fatal(err)
	}
	// third удерживает вновь созданную группу после завершения прежнего экземпляра.
	third, err := registry.Acquire(ctx, f.snapshot, live)
	if err != nil {
		t.Fatal(err)
	}
	if third.Group() == first.Group() {
		t.Fatal("отозванная группа ожила")
	}
	if err = third.Close(); err != nil {
		t.Fatal(err)
	}
	if err = registry.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = registry.Acquire(ctx, f.snapshot, live); !errors.Is(err, core.ErrStale) {
		t.Fatal(err)
	}
}

// Проверяет разрешение AV1 по подтверждённому формату, прогресс только проверенных пакетов и отзыв
// доступности при EOF, ошибке метаданных, AU или обработчика прогресса.
func TestGroupReadOutputResolvesActualAV1AndReportsOnlyValidProgress(t *testing.T) {
	// f предоставляет точный доменный допуск, контрольные конфигурации и проверенный AV1-формат.
	f := runtimeUnitFixture(t)
	// ctx задаёт общий контекст разбора контрольного выходного fMP4.
	ctx := context.Background()
	// owner содержит действующий допуск Session; err сообщает об отказе его привязки.
	owner, err := f.config.Core.BindExecutionOwner(ctx, f.config.Parent)
	if err != nil {
		t.Fatal(err)
	}
	// mode выбирает пригодный поток, отсутствие AV1-метаданных, неверный TU либо отклонённый
	// progress.
	for _, mode := range []string{"valid", "unknown-metadata", "invalid-au", "rejected-progress"} {
		t.Run(mode, func(t *testing.T) {
			// Проверяет выбранный вариант разбора AV1: прогресс сообщается только после пригодных метаданных и AU,
			// а отказ отчёта завершает подачу.

			// sw содержит отдельный переключатель выбранного сценария разбора.
			sw, _, _, _ := switcherFixture(t)
			// g связывает проверяемый выход fMP4 с точным владельцем и переключателем.
			g := &Group{config: f.config.Runtime, owner: owner, switcher: sw}
			// wire накапливает контрольный контейнер fMP4 до передачи в соединение.
			var wire bytes.Buffer
			// m записывает контрольный fMP4; err сохраняет результаты его создания, записи и
			// последующего разбора.
			m, err := fmp4.NewMuxer(&wire, fmp4.Init{Video: f.v, Audio: f.a}, g.config.Container)
			if err != nil {
				t.Fatal(err)
			}
			// payload содержит пригодный AV1 TU либо намеренно повреждённую единицу доступа.
			payload := av1TU(true)
			if mode == "invalid-au" {
				payload = []byte{0x1a, 0}
			}
			// video содержит контрольный независимый видеопакет с известной длительностью.
			video, _ := media.NewPacket(media.PacketInfo{PTS: 0, DTS: 0, DurationTicks: 1600, Keyframe: true}, f.v, payload)
			// audio содержит AAC-пакет с контрольным смещением PTS относительно видео.
			audio, _ := media.NewPacket(media.PacketInfo{PTS: 480, DTS: 480, DurationTicks: 1024}, f.a, []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c})
			if err = m.WritePacket(video); err != nil {
				t.Fatal(err)
			}
			if err = m.WritePacket(audio); err != nil {
				t.Fatal(err)
			}
			// data содержит независимую копию fMP4 с заменяемым типом служебного
			// metadata-блока.
			data := bytes.Clone(wire.Bytes())
			// position обозначает положение uuid в контейнере для исключения служебных
			// метаданных.
			position := bytes.Index(data, []byte("uuid"))
			if position < 0 {
				t.Fatal("нет metadata")
			}
			copy(data[position:], "free")
			// reader предоставляет вход разбора fMP4; writer передаёт в него контрольные
			// байты.
			reader, writer := net.Pipe()
			// done передаёт результат завершения записи контрольного соединения.
			done := make(chan error, 1)
			go func() {
				// Записывает тестовый fMP4 в net.Pipe, закрывает сторону записи и передаёт объединённую ошибку в done.
				// err объединяет результат записи fMP4 и закрытия стороны writer.
				_, err := writer.Write(data)
				err = errors.Join(err, writer.Close())
				done <- err
			}()
			// format задаёт подтверждённый AV1-формат либо пустое описание в сценарии
			// unknown-metadata.
			format := f.snapshot.Artifact.VerifiedFormat
			if mode == "unknown-metadata" {
				format = core.MediaFormat{}
			}
			// count считает вызовы progress после проверки метаданных и единиц доступа.
			count := 0
			err = g.readOutput(ctx, reader, Fallback, 1, format, func(ffmpeg.Track, int64) bool {
				// Считает вызовы отчёта прогресса и отклоняет их только в сценарии rejected-progress.
				count++
				return mode != "rejected-progress"
			})
			_ = reader.Close()
			// writeErr содержит окончательный исход записи контрольного fMP4 с допустимым
			// io.ErrClosedPipe.
			if writeErr := <-done; writeErr != nil && !errors.Is(writeErr, io.ErrClosedPipe) {
				t.Fatal(writeErr)
			}
			switch mode {
			case "valid":
				if !errors.Is(err, io.EOF) || count != 2 || sw.Status().Available {
					t.Fatalf("valid: %v %d %+v", err, count, sw.Status())
				}
			case "unknown-metadata", "invalid-au":
				if err == nil || count != 0 || sw.Status().Available {
					t.Fatalf("невалидный поток сообщил прогресс: %v %d", err, count)
				}
			case "rejected-progress":
				if !errors.Is(err, ErrFeed) || count != 1 || sw.Status().Available {
					t.Fatal(err, count)
				}
			}
		})
	}
}

// Проверяет ErrCleanupPending при незавершённых работниках, сохранение отмены ожидания и возврат
// ошибки физической очистки после закрытия done.
func TestGroupStopKeepsPhysicalJoinAndCleanupErrorsVisible(t *testing.T) {
	// g моделирует ещё не завершённую группу с коротким сроком Stop и отдельным сигналом done.
	g := &Group{config: RuntimeConfig{ShutdownTimeout: time.Millisecond}, cancel: func() {
		// Оставляет отмену пустой, чтобы тест отдельно проверял срок ожидания и сохранённые ошибки очистки
		// Group.Stop.
	}, done: make(chan struct{})}
	// err содержит ожидаемый ErrCleanupPending до завершения работников.
	if err := g.Stop(context.Background()); !errors.Is(err, ffmpeg.ErrCleanupPending) {
		t.Fatal(err)
	}
	// ctx передаёт уже отменённое ожидание; cancel инициирует отмену до Stop.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// err содержит ожидаемую отмену ожидания ещё не завершённой группы.
	if err := g.Stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// failure задаёт контрольную ошибку физической очистки, сохраняемую до и после done.
	failure := errors.New("не удалён собственный сокет")
	g.addCleanup(nil)
	g.addCleanup(failure)
	close(g.done)
	// err содержит сохранённую ошибку очистки после фактического завершения группы.
	if err := g.Stop(context.Background()); !errors.Is(err, failure) {
		t.Fatal("потеряно доказательство неполной очистки", err)
	}
}

// Проверяет общий положительный сдвиг отрицательного DTS, отсутствие сдвига положительного начала и
// отказ для переполнения MinInt64.
func TestTransportOriginPreservesCommonSignedClockAndRejectsOverflow(t *testing.T) {
	// v содержит контрольную видеоконфигурацию; a содержит аудиоконфигурацию для общей транспортной
	// шкалы.
	_, v, a, _ := switcherFixture(t)
	// dt задаёт проверяемый положительный, отрицательный либо предельно отрицательный начальный DTS.
	for _, dt := range []int64{100, -100, math.MinInt64} {
		// p содержит начальный видеопакет с проверяемой знаковой меткой.
		p, _ := media.NewPacket(media.PacketInfo{PTS: dt, DTS: dt, Keyframe: true}, v, av1TU(true))
		// origin содержит вычисленный общий сдвиг; err сообщает об отказе точного пересчёта или
		// переполнении.
		origin, err := transportOrigin(media.Delivery{Packet: p, VideoConfig: v, AudioConfig: a})
		if dt == math.MinInt64 {
			if err == nil {
				t.Fatal("переполнение origin принято")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if dt >= 0 && origin.OffsetTicks != 0 || dt < 0 && origin.OffsetTicks != 100 {
			t.Fatal(origin)
		}
	}
}

// Проверяет отказ переполненного счётчика попыток LIVE и независимую выдачу первого номера заглушки.
func TestGroupAttemptIdentityCannotWrapOrAffectOtherFeed(t *testing.T) {
	// g содержит независимые атомарные счётчики попыток LIVE и заглушки.
	g := &Group{}
	g.attempts[0].Store(math.MaxUint64)
	// id содержит результат выделения LIVE-номера; err содержит ожидаемый отказ переполнения.
	if id, err := g.nextAttempt(Live); id != 0 || !errors.Is(err, ErrFeed) {
		t.Fatal(id, err)
	}
	// id содержит первый номер заглушки; err сообщает об отказе независимого выделения.
	if id, err := g.nextAttempt(Fallback); id != 1 || err != nil {
		t.Fatal(id, err)
	}
	if g.attempts[0].Load() != math.MaxUint64 {
		t.Fatal("исчерпанная identity переиспользована")
	}
}

// Проверяет отказ неподдержанного или несогласованного ASC до построителя перекодирования и наличие
// причины LIVE AAC copy в журнале.
func TestLiveAudioCopyRejectsASCBeforeSelectedBuilderAndReportsReason(t *testing.T) {
	// f предоставляет контрольные AAC-заголовки, профиль и точное выполнение для проверки раннего
	// отказа.
	f := runtimeUnitFixture(t)
	// scenario выбирает неподдержанный ASC, несовпадение метаданных либо несовпадение профиля.
	for _, scenario := range []string{"unsupported-asc", "metadata-mismatch", "profile-mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			// Вводит выбранную несовместимость AAC и проверяет отказ до построителя transcode с диагностикой LIVE
			// AAC copy.

			// live содержит Router контрольного LIVE; err сохраняет результаты создания,
			// настройки и проверки аудиосценария.
			live, err := media.NewRouter(f.config.Runtime.Switcher.Router)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				// Закрывает LIVE Router сценария несовместимого аудио, игнорируя ошибку.
				_ = live.Close()
			}()
			// source содержит текущую эпоху контрольной LIVE-публикации.
			source, err := live.BeginSource()
			if err != nil {
				t.Fatal(err)
			}
			// info содержит изменяемое описание AAC для выбранной несовместимости и привязки
			// к текущей эпохе.
			info := f.a.Info()
			info.SourceEpoch = source.Epoch()
			// headers содержит контрольный ASC либо намеренно неподдержанные байты.
			headers := f.a.Headers()
			switch scenario {
			case "unsupported-asc":
				headers = []byte{0}
			case "metadata-mismatch":
				info.Audio.SampleRateHz = 44100
			}
			// audio содержит проверяемую AAC-конфигурацию выбранного сценария.
			audio, err := media.NewCodecConfig(info, headers)
			if err != nil {
				t.Fatal(err)
			}
			// videoInfo содержит описание AV1, привязываемое к той же эпохе LIVE.
			videoInfo := f.v.Info()
			videoInfo.SourceEpoch = source.Epoch()
			// video содержит контрольную AV1-конфигурацию текущего источника.
			video, err := media.NewCodecConfig(videoInfo, f.v.Headers())
			if err != nil {
				t.Fatal(err)
			}
			if err = source.UpdateConfigs(video, audio); err != nil {
				t.Fatal(err)
			}
			// sub содержит подписку LIVE для подачи первой пары в transcode.
			sub, err := live.Subscribe(f.config.Runtime.Switcher.Queue)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				// Закрывает подписку сценария несовместимого аудио, игнорируя ошибку.
				_ = sub.Close()
			}()
			// config содержит очередную видеоконфигурацию либо аудиоконфигурацию
			// публикуемого контрольного пакета.
			for _, config := range []media.CodecConfig{video, audio} {
				// packet содержит контрольный пакет дорожки; err сохраняет результаты
				// его создания и публикации.
				packet, err := media.NewPacket(media.PacketInfo{Keyframe: config.Info().Track == media.TrackVideo, DurationTicks: 1024}, config, []byte{1})
				if err != nil {
					t.Fatal(err)
				}
				if err = source.Publish(packet); err != nil {
					t.Fatal(err)
				}
			}
			// observer содержит наблюдателя группы для коррелированной причины отказа AAC.
			observer, err := f.config.Observer.OpenGroup(context.Background(), f.snapshot.Group.ID, f.snapshot.Profile.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				// Закрывает наблюдателя группы после проверки аудио, игнорируя ошибку.
				_ = observer.Close(context.Background())
			}()
			// output накапливает JSON-журнал проверки несовместимого LIVE AAC copy.
			var output bytes.Buffer
			// g содержит группу с выбранным несовместимым аудио и контролируемым
			// построителем transcode.
			g := &Group{config: f.config.Runtime, parent: f.config.Parent, id: f.snapshot.Group.ID, observer: observer}
			g.config.Switcher.Profile.LiveAudioMode = core.LiveAudioCopy
			if scenario == "profile-mismatch" {
				g.config.Switcher.Profile.Format.Audio.SampleRateHz = 44100
			}
			g.config.Logger = slog.New(slog.NewJSONHandler(&output, nil))
			// called отмечает недопустимый вызов построителя после ошибки проверки AAC.
			called := false
			g.config.Transcode = func(context.Context, ffmpeg.NVIDIAConfig, ffmpeg.Attempt) (ffmpeg.Command, error) {
				// Отмечает недопустимый вызов построителя и возвращает ошибку, позволяя проверить отказ проверки AAC
				// до этой границы.

				called = true
				return ffmpeg.Command{}, errors.New("построитель не должен вызываться")
			}
			// ctx ограничивает проверку раннего отказа одной секундой; cancel освобождает
			// таймер.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err = g.transcode(ctx, sub, 1)
			if !errors.Is(err, codec.ErrCodec) || called || !strings.Contains(output.String(), "LIVE AAC copy") || !strings.Contains(output.String(), "validate_audio") {
				t.Fatal("несовместимое аудио дошло до построителя или потеряна причина", err, called, output.String())
			}
		})
	}
}
