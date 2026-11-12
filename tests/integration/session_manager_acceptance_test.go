package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
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

// sessionAcceptanceFixture связывает реальные домен, SQLite, файлы и владельцев процессов с управляемым
// медиатрактом SessionManager.
type sessionAcceptanceFixture struct {
	// service предоставляет доменные команды, историю и общую RuntimeAuthority фикстуры.
	service *core.Service
	// store владеет временной SQLite и предоставляет идентификатор текущего экземпляра хранилища.
	store *storage.Store
	// files создаёт и проверяет принадлежащие поколениям файлы заглушек.
	files *fallback.FileStore
	// processes учитывает реальные дочерние процессы и объекты выполнения теста.
	processes *processowner.Registry
	// stream содержит основной поток с постоянным синтетическим ключом и политикой заглушки.
	stream core.Stream
	// asset содержит заглушку основного потока.
	asset core.FallbackAsset
	// groups содержит две сохранённые группы одного исходного профиля.
	groups []core.TranscodeGroup
	// destinations содержит сохранённые назначения с указанными при создании адресами и группами.
	destinations []core.Destination
	// generation сохраняет опубликованное тестовое поколение с точными файловыми манифестами.
	generation core.FallbackGeneration
	// config содержит изменяемые до launch рабочие зависимости, пределы и управляемые построители
	// медиакоманд.
	config session.Config
	// manager содержит фактически запущенного владельца жизненного цикла Session.
	manager *session.Manager
	// directory указывает частный короткий корень БД, файлов и журналов физических процессов.
	directory string
	// starts атомарно считает построения проигрывателей для проверки отсутствия лишних физических
	// запусков.
	starts atomic.Int32
	// logs сохраняет ограниченный журнал менеджера для диагностики отказов.
	logs sessionAcceptanceLog
	// diagnosed атомарно запрещает повторное сохранение одного диагностического снимка.
	diagnosed atomic.Bool
}

// sessionAcceptanceLog сохраняет целые диагностические записи фикстуры в конечном байтовом бюджете.
type sessionAcceptanceLog struct {
	// mu защищает байты журнала и число отброшенных байтов между медиагорутинами и диагностикой.
	mu sync.Mutex
	// data накапливает полные записи в пределах одного MiB.
	data bytes.Buffer
	// dropped считает весь объём записей, не поместившихся в диагностический буфер.
	dropped int
}

// Write сохраняет полную запись под блокировкой в пределах одного MiB. При нехватке места учитывает
// весь отброшенный объём, но сообщает успешное принятие, чтобы тестовый журнал не останавливал
// медиатракт.
func (l *sessionAcceptanceLog) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(data) > (1<<20)-l.data.Len() {
		l.dropped += len(data)
		return len(data), nil
	}
	return l.data.Write(data)
}

// Сохраняются только полные диагностические записи собственной фикстуры, без медиа и бинарников.
func (f *sessionAcceptanceFixture) captureFailure(t *testing.T) {
	t.Helper()
	if !f.diagnosed.CompareAndSwap(false, true) {
		return
	}
	// artifact указывает отдельный временный каталог диагностики отказа; err содержит отказ его
	// создания.
	artifact, err := os.MkdirTemp("", "gost-session-failure-")
	if err != nil {
		t.Log("не удалось сохранить диагностику:", err)
		return
	}
	// report накапливает снимок журналов, квитанций и текущих наблюдений до очистки фикстуры.
	var report bytes.Buffer
	_, _ = fmt.Fprintf(&report, "test=%s\ntime=%s\nfixture=%s\nbuilders=%d\n", t.Name(), time.Now().UTC().Format(time.RFC3339Nano), f.directory, f.starts.Load())
	f.logs.mu.Lock()
	_, _ = fmt.Fprintf(&report, "manager_log_overflow=%v dropped_bytes=%d\n", f.logs.dropped != 0, f.logs.dropped)
	_, _ = report.Write(f.logs.data.Bytes())
	f.logs.mu.Unlock()
	_, _ = fmt.Fprintln(&report, "\nSupervisor.NextDiagnostic недоступен через публичные Manager/Group интерфейсы; stdout/stderr его частной очереди не получены.")
	// Resolve/RecoverPage изменяют журнал и поэтому не используются для снятия диагностики.
	// journalRoot указывает только каталог scopes собственного реестра процессов для чтения
	// диагностических JSON.
	journalRoot := filepath.Join(f.directory, "p", "scopes")
	// remaining ограничивает суммарно читаемые JSON восемью MiB; entries ограничивает число
	// посещённых объектов 4096.
	remaining, entries := int64(8<<20), 0
	err = filepath.WalkDir(journalRoot, func(name string, entry fs.DirEntry, walkErr error) error {
		// Читает только обычные JSON-записи журнала процессов с пределами числа файлов и суммарных байтов для
		// диагностики отказа.

		if walkErr != nil {
			_, _ = fmt.Fprintf(&report, "journal_read_error path=%q err=%v\n", name, walkErr)
			return nil
		}
		entries++
		if entries > 4096 {
			_, _ = fmt.Fprintln(&report, "journal_overflow=true reason=entry_budget")
			return fs.SkipAll
		}
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			return nil
		}
		// info описывает текущий объект журнала; readErr содержит отказ получения сведений для
		// допуска только обычного файла.
		info, readErr := entry.Info()
		if readErr != nil || !info.Mode().IsRegular() {
			_, _ = fmt.Fprintf(&report, "journal_read_error path=%q err=%v regular=false\n", name, readErr)
			return nil
		}
		if info.Size() > remaining {
			_, _ = fmt.Fprintf(&report, "journal_overflow=true omitted_path=%q bytes=%d\n", name, info.Size())
			return fs.SkipAll
		}
		// file содержит открытый JSON журнала процессов для ограниченного чтения.
		file, readErr := os.Open(name)
		if readErr != nil {
			_, _ = fmt.Fprintf(&report, "journal_read_error path=%q err=%v\n", name, readErr)
			return nil
		}
		// data содержит прочитанные байты текущего JSON в пределах оставшегося бюджета плюс один
		// проверочный байт.
		data, readErr := io.ReadAll(io.LimitReader(file, remaining+1))
		readErr = errors.Join(readErr, file.Close())
		if readErr != nil {
			_, _ = fmt.Fprintf(&report, "journal_read_error path=%q err=%v\n", name, readErr)
			return nil
		}
		if int64(len(data)) > remaining {
			_, _ = fmt.Fprintf(&report, "journal_overflow=true omitted_path=%q\n", name)
			return fs.SkipAll
		}
		remaining -= int64(len(data))
		_, _ = fmt.Fprintf(&report, "journal_record=%q\n%s\n", name, data)
		return nil
	})
	if err != nil {
		_, _ = fmt.Fprintln(&report, "journal_walk_error:", err)
	}
	// ctx ограничивает доменную часть диагностики двумя секундами; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// query задаёт первую ограниченную страницу диагностики квитанций и затем Session основного
	// потока.
	query := core.PageRequest{Query: core.ListQuery{Limit: 64}}
	// receipts содержит страницу квитанций; receiptErr содержит отказ диагностического чтения.
	receipts, receiptErr := f.service.ListReceipts(ctx, query)
	_, _ = fmt.Fprintf(&report, "receipts=%+v err=%v receipt_page_overflow=%v\n", receipts, receiptErr, receipts.NextCursor != "")
	query.Query.StreamID = f.stream.ID
	// sessions содержит страницу Session потока; sessionErr содержит отказ её чтения.
	sessions, sessionErr := f.service.ListSessions(ctx, query)
	_, _ = fmt.Fprintf(&report, "sessions=%+v err=%v session_page_overflow=%v\n", sessions, sessionErr, sessions.NextCursor != "")
	// current содержит очередную сохранённую Session для чтения её выходов и точного оперативного
	// снимка.
	for _, current := range sessions.Items {
		// outputs содержит страницу выходов текущей Session; outputErr содержит отказ её
		// диагностического чтения.
		outputs, outputErr := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 64, SessionID: current.ID}})
		// snapshot содержит текущие наблюдения точного исполнения; snapshotErr содержит отказ их
		// чтения.
		snapshot, snapshotErr := f.service.RuntimeSnapshot(ctx, core.RuntimeSessionFence{SessionID: current.ID, Incarnation: current.Incarnation, Lifecycle: current.Lifecycle})
		// encoded содержит JSON оперативного снимка; encodeErr содержит отказ его сериализации
		// для диагностики.
		encoded, encodeErr := json.Marshal(snapshot)
		_, _ = fmt.Fprintf(&report, "session_id=%s outputs=%+v err=%v output_page_overflow=%v\nruntime=%s err=%v encode_error=%v\n", current.ID, outputs, outputErr, outputs.NextCursor != "", encoded, snapshotErr, encodeErr)
	}
	// name указывает единственный diagnostics.log собственного временного каталога отказа.
	name := filepath.Join(artifact, "diagnostics.log")
	// err содержит отказ сохранения собранной диагностики с ограниченными правами.
	if err := os.WriteFile(name, report.Bytes(), 0600); err != nil {
		t.Log("не удалось записать диагностику:", err)
		return
	}
	t.Log("диагностика до очистки фикстуры:", name)
}

// Возвращает явные пределы SQLite и RAM-владельцев для интеграционных сценариев SessionManager.
func sessionAcceptanceOptions() storage.Options {
	return storage.Options{OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 16, MaxIdentifierBytes: 1024, MaxBytes: 1 << 20}, PreparationLimits: core.PreparationLimits{MaxQueued: 8, MaxProfiles: 16}, ObservationLimits: core.ObservationLimits{MaxSessions: 8, MaxGroups: 32, MaxOutputs: 64, MaxFFmpegWorkers: 32, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 20 * time.Millisecond, RetryAttempts: 4, RetryTimeout: time.Second, RetryDelay: time.Millisecond}
}

// Запускает локальный TCP-приёмник, который сразу закрывает каждое принятое соединение, и возвращает
// его RTMP URL. t.Cleanup закрывает listener и ожидает завершения обработчика.
func sessionAcceptanceRejectingEndpoint(t *testing.T) string {
	t.Helper()
	// listener содержит локальный TCP-приёмник, немедленно закрывающий принятые соединения; err
	// содержит отказ открытия слушателя.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// done подтверждает завершение горутины отказывающего приёмника после закрытия слушателя.
	done := make(chan struct{})
	go func() {
		// Принимает и немедленно закрывает соединения, имитируя недоступное назначение RTMP.

		defer close(done)
		for {
			// connection содержит очередное принятое соединение для немедленного закрытия;
			// err содержит отказ Accept при завершении.
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_ = connection.Close()
		}
	}()
	t.Cleanup(func() {
		// Закрывает отказывающий слушатель и ограниченно ожидает завершения приёма соединений.

		// err содержит результат закрытия отказывающего слушателя при очистке.
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("отказывающий RTMP-приёмник не завершился")
		}
	})
	return "rtmp://" + listener.Addr().String() + "/live"
}

// Создаёт изолированные хранилище, журналы файлов и процессов, Stream, две группы одного профиля и
// выбранные назначения с управляемой заглушкой. endpoints и groupIndices должны соответствовать друг
// другу; Manager ещё не запущен, а освобождение общих ресурсов зарегистрировано в t.Cleanup.
func newSessionAcceptanceFixture(t *testing.T, helper string, endpoints []string, groupIndices []int) *sessionAcceptanceFixture {
	t.Helper()
	// ctx задаёт общий контекст создания реальных ресурсов и конфигурационных записей фикстуры.
	ctx := context.Background()
	// base выделяет короткий частный каталог SessionManager; err содержит отказ создания.
	base, err := os.MkdirTemp("/tmp", "session-acceptance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Удаляет частный каталог SessionManager после освобождения процессов и хранилищ.

		// err содержит отказ удаления собственного каталога после завершения владельцев.
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	// f собирает общие ресурсы и сохранённые объекты сценария до запуска менеджера.
	f := &sessionAcceptanceFixture{directory: base}
	f.store, err = storage.Open(ctx, filepath.Join(base, "state.sqlite"), sessionAcceptanceOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает временное хранилище SessionManager после зависимых компонентов.

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
		// Освобождает файловое хранилище вариантов заглушки.

		// err содержит результат закрытия файлового хранилища вариантов.
		if err := f.files.Close(); err != nil {
			t.Error(err)
		}
	})
	f.processes, err = processowner.New(processowner.Config{Root: filepath.Join(base, "p"), RuntimeRoot: filepath.Join(base, "r"), HelperPath: helper, GracePeriod: 50 * time.Millisecond, Limits: sessionAcceptanceProcessLimits()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает реестр владения тестовыми процессами.

		// err содержит результат закрытия реестра процессов после медиакомпонентов.
		if err := f.processes.Close(); err != nil {
			t.Error(err)
		}
	})
	f.asset, err = f.service.CreateAsset(ctx, "Локальная заглушка")
	if err != nil {
		t.Fatal(err)
	}
	// watchdog задаёт сроки отсутствия дорожек и восстановления источника основного Stream.
	watchdog := core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: 100 * time.Millisecond, KeyframeTimeout: time.Second}
	f.stream, err = f.service.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "synthetic-session-key", Fallback: core.FallbackPolicy{AssetID: f.asset.ID, Watchdog: watchdog}})
	if err != nil {
		t.Fatal(err)
	}
	// profile задаёт полный AV1 640×360 профиль обеих исходных групп.
	profile := publisherProfile(core.CodecAV1, 640, 360)
	// saved содержит созданный именованный профиль для связей двух групп.
	saved, err := f.service.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	// i задаёт номер одной из двух сохраняемых групп.
	for i := range 2 {
		// g содержит созданную группу общего профиля; err содержит отказ её создания.
		g, err := f.service.CreateGroup(ctx, core.TranscodeGroup{Name: fmt.Sprintf("Группа %d", i), StreamID: f.stream.ID, ProfileID: saved.ID})
		if err != nil {
			t.Fatal(err)
		}
		f.groups = append(f.groups, g)
	}
	// i указывает назначение и соответствующий groupIndices; endpoint содержит его точный
	// RTMP-адрес.
	for i, endpoint := range endpoints {
		// d содержит созданное назначение выбранной группы; err содержит отказ сохранения.
		d, err := f.service.CreateDestination(ctx, core.Destination{Name: fmt.Sprintf("Назначение %d", i), StreamID: f.stream.ID, GroupID: f.groups[groupIndices[i]].ID, Platform: "custom", Endpoint: endpoint, StreamKey: fmt.Sprintf("synthetic-key-%d", i), Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		f.destinations = append(f.destinations, d)
	}
	f.seedFallback(t)
	f.config = session.Config{Core: f.service, Files: f.files, Processes: f.processes, GeneratorVersion: "controlled-generator", Router: publisherRouterLimits(), Publisher: publisher.Options{Queue: publisherQueue(), RetryInitial: 10 * time.Millisecond, RetryMax: 50 * time.Millisecond, ResubscribeDelay: 5 * time.Millisecond, Transport: publisherTransportOptions(nil)}, Logger: slog.New(slog.NewJSONHandler(&f.logs, nil)), MaxSessions: 1, MaxOutputs: 8, MaxCommands: 8, PageSize: 8, MaxGroups: 4, MaxReferences: 8, ReconcileInterval: 5 * time.Millisecond, StartupTimeout: 4 * time.Second, OperationTimeout: 300 * time.Millisecond, ShutdownTimeout: 4 * time.Second,
		Group: group.RuntimeConfig{Switcher: group.SwitcherConfig{Profile: profile, Watchdog: watchdog, Queue: publisherQueue(), Router: publisherRouterLimits(), MaxAVSkew: 100 * time.Millisecond, MaxTransitionGap: 100 * time.Millisecond}, Container: integrationLimits(), Binary: helper, ResubscribeBackoff: 5 * time.Millisecond, ControlInterval: 5 * time.Millisecond, ShutdownTimeout: 3 * time.Second, FFmpeg: ffmpeg.Config{BaseDir: filepath.Join(base, "r"), StartupTimeout: 3 * time.Second, VideoTimeout: 2 * time.Second, AudioTimeout: 2 * time.Second, WatchdogInterval: 10 * time.Millisecond, MaxRestarts: 0, RestartBackoff: time.Millisecond, GracePeriod: 50 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 3 * time.Second, DiagnosticEvents: 64, DiagnosticBytes: 65536, DiagnosticChunkBytes: 1024, MaxCommandBytes: 8192, MaxCommandArgs: 128}}}
	f.config.Monitoring = monitoring.SamplerConfig{MaxSources: 128, StaleAfter: 5 * time.Second, ShutdownTimeout: 4 * time.Second}
	f.config.Telemetry = core.TelemetryLimits{Events: core.EventLimits{MaxEvents: 128, MaxBytes: 1 << 20, MaxEventBytes: 65536, MaxPageSize: 64}, MediaStaleAfter: 5 * time.Second, MaxGPUDevices: 8, MaxGPUUUIDBytes: 256, MaxGPUBytes: 1 << 20}
	f.config.Group.FFmpeg.Progress = ffmpeg.ProgressConfig{MaxLineBytes: 4096, MaxReportBytes: 65536, MaxFields: 64}
	f.configureOwners(t, helper)
	f.config.Group.Player = func(_ context.Context, _ string, _ *os.File, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Считает запуски проигрывателя и создаёт управляемый источник заглушки.

		f.starts.Add(1)
		return publisherControlledCommand(profile, attempt, 10, "")
	}
	t.Cleanup(func() {
		// Сохраняет диагностический снимок фикстуры при неудаче теста.

		if t.Failed() {
			f.captureFailure(t)
		}
	})
	return f
}

// Создаёт владельцев подготовки и приёма RTMP поверх текущих service и files и связывает их с
// конфигурацией Manager. Вызывается до запуска Manager либо после завершения приёма прежними
// владельцами.
func (f *sessionAcceptanceFixture) configureOwners(t *testing.T, helper string) {
	t.Helper()
	// preparation содержит реальный FallbackManager общих файлов и процессов; err содержит отказ его
	// создания.
	preparation, err := fallback.NewManager(f.service, f.files, fallback.Config{ReconcileLimit: 16, GeneratorVersion: "controlled-generator", FFmpeg: helper, FFprobe: helper, ProcessRegistry: f.processes, MaxUploadBytes: 1 << 20, MaxOutputBytes: 1 << 20, PrepareTimeout: 5 * time.Second, ShutdownTimeout: 3 * time.Second, QueueInterval: time.Millisecond, Runner: fallback.RunnerLimits{Timeout: 3 * time.Second, MaxArgs: 128, MaxCommandBytes: 8192, MaxDiagnosticBytes: 65536}, Probe: fallback.ProbeLimits{MaxJSONBytes: 65536, MaxWidth: 4096, MaxHeight: 4096, MaxPixels: 4096 * 4096, MaxDuration: time.Minute, ImageDuration: time.Second, MaxPackets: 1000, Container: integrationLimits()}})
	if err != nil {
		t.Fatal(err)
	}
	// ingress содержит RTMP-сервер, обращающийся за Feed к текущему менеджеру фикстуры.
	ingress, err := rtmp.NewServer(rtmp.Options{Core: f.service, Route: rtmp.ConventionalRoute, Limits: publisherRTMPLimits(), Feed: func(current core.Session) (*media.Router, error) {
		// Передаёт текущую Session менеджеру для получения её входного маршрутизатора.
		return f.manager.Feed(current)
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.config.Core, f.config.Files, f.config.Preparation, f.config.Ingress = f.service, f.files, preparation, ingress
}

// Создаёт принадлежащие поколению файлы через FileStore и публикует тестовые манифесты с точными
// размерами и хешами. Содержимое служит управляемому проигрывателю и не является закодированным
// медиапотоком; завершение подготовки подтверждается отдельно.
func (f *sessionAcceptanceFixture) seedFallback(t *testing.T) {
	t.Helper()
	// ctx задаёт контекст резервирования и публикации управляемого поколения заглушки.
	ctx := context.Background()
	// g содержит зарезервированное поколение; err содержит отказ допуска подготовки.
	g, err := f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: f.asset.ID, ExpectedRevision: f.asset.Revision, Kind: core.AssetImage, GeneratorVersion: "controlled-generator"})
	if err != nil {
		t.Fatal(err)
	}
	// pf содержит точную захваченную попытку подготовки для выдачи, публикации и подтверждения
	// очистки.
	pf, err := f.service.ClaimPreparation(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.MarkPreparationIssued(ctx, pf); err != nil {
		t.Fatal(err)
	}
	// ledger содержит записи принадлежности файлов нового поколения.
	ledger, err := f.service.ListGenerationFiles(ctx, core.PageRequest{Query: core.ListQuery{Limit: 32, GenerationID: g.ID}})
	if err != nil {
		t.Fatal(err)
	}
	// file содержит очередной объект журнала принадлежности для создания каталога поколения.
	for _, file := range ledger.Items {
		if file.Kind == core.OwnedDirectory {
			if err = f.files.CreateOwnedGeneration(file); err != nil {
				t.Fatal(err)
			}
		}
	}
	// data задаёт известные неизменяемые байты файлов, используемых только управляемым
	// проигрывателем.
	data := []byte("immutable local fallback fixture")
	// hash содержит SHA-256 точных тестовых байтов для файловых манифестов.
	hash := sha256.Sum256(data)
	// digest содержит шестнадцатеричное представление хеша оригинала и вариантов.
	digest := hex.EncodeToString(hash[:])
	// file содержит очередной оригинал или вариант, который создаётся через FileStore.
	for _, file := range ledger.Items {
		if file.Kind != core.OwnedOriginal && file.Kind != core.OwnedVariant {
			continue
		}
		// writer содержит файл с проверенной принадлежностью; err содержит отказ его создания.
		writer, err := f.files.CreateOwned(file)
		if err != nil {
			t.Fatal(err)
		}
		// writeErr сохраняет отказ записи содержимого до Sync и Close.
		_, writeErr := writer.Write(data)
		if err = errors.Join(writeErr, writer.Sync(), writer.Close()); err != nil {
			t.Fatal(err)
		}
	}
	// publication собирает метаданные исходника и манифесты всех требуемых вариантов.
	publication := core.GenerationPublication{SourceHash: digest, SizeBytes: uint64(len(data)), SourceFormat: core.SourceFormat{ImageFormat: "png"}}
	// required содержит очередной обязательный профиль и принадлежащие ему ID и путь варианта.
	for _, required := range g.RequiredProfiles {
		publication.Variants = append(publication.Variants, core.FallbackVariant{ID: required.VariantID, GenerationID: g.ID, AssetID: g.AssetID, SourceHash: digest, Profile: required.Profile, VerifiedFormat: required.Profile.Format, GeneratorVersion: g.GeneratorVersion, ManagedPath: required.ManagedPath, SizeBytes: uint64(len(data)), ContentHash: digest})
	}
	f.generation, err = f.service.PublishPreparation(ctx, pf, publication)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.AcknowledgePreparationCleanup(ctx, pf); err != nil {
		t.Fatal(err)
	}
}

// Запускает Manager с текущей конфигурацией и регистрирует ограниченное закрытие в t.Cleanup. При
// ошибке запуска или завершения сохраняет диагностику фикстуры.
func (f *sessionAcceptanceFixture) launch(t *testing.T) {
	t.Helper()
	// err принимает результат создания SessionManager с текущими зависимостями и пределами.
	var err error
	f.manager, err = session.New(context.Background(), f.config)
	if err != nil {
		f.captureFailure(t)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Сохраняет диагностику отказа и ограниченно завершает SessionManager перед освобождением общих
		// ресурсов.

		if t.Failed() {
			f.captureFailure(t)
		}
		// ctx ограничивает очистку менеджера восемью секундами; cancel освобождает таймер.
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		// err содержит результат закрытия менеджера, при отказе сохраняется диагностика.
		if err := f.manager.Close(ctx); err != nil {
			t.Error("закрытие Manager:", err)
			f.captureFailure(t)
		}
	})
}

// Принимает команду запуска Stream через Manager с указанными RequestID и назначениями и возвращает
// квитанцию без ожидания RUNNING. Ошибка принятия сохраняет диагностику и завершает тест.
func (f *sessionAcceptanceFixture) start(t *testing.T, requestID string, selected ...core.ID) core.CommandReceipt {
	t.Helper()
	// r содержит квитанцию запуска через Manager; err содержит отказ принятия смыслового запроса.
	r, err := f.manager.StartSession(context.Background(), core.StartSessionRequest{RequestID: requestID, StreamID: f.stream.ID, DestinationIDs: selected})
	f.config.Logger.Info("fixture_start_result", "request_id", requestID, "receipt", r, "error", err)
	if err != nil {
		f.captureFailure(t)
		t.Fatalf("принятие start RequestID=%q receipt=%+v: %v", requestID, r, err)
	}
	return r
}

// Ожидает заданное состояние сохранённой квитанции; иной терминальный результат или ошибка чтения
// завершают тест сразу. Диагностические функции вызываются при отказе и истечении общего срока
// ожидания.
func sessionAcceptanceReceipt(t *testing.T, svc *core.Service, id core.ID, status core.CommandStatus, failureDiagnostics ...func()) core.CommandReceipt {
	t.Helper()
	// result сохраняет последнюю прочитанную квитанцию для ожидания и диагностики.
	var result core.CommandReceipt
	// diagnose вызывает переданные диагностические функции и выводит последнюю известную квитанцию.
	diagnose := func() {
		// Вызывает переданные сборщики диагностики и записывает последнюю квитанцию при ошибке ожидания.

		// capture содержит очередную переданную функцию диагностики отказа ожидания квитанции.
		for _, capture := range failureDiagnostics {
			capture()
		}
		t.Logf("ожидание квитанции id=%s RequestID=%q status=%s: %+v", id, result.RequestID, status, result)
	}
	sessionAcceptanceWait(t, func() bool {
		// Ожидает выбранный статус квитанции и немедленно сообщает неожиданный терминальный исход.

		// current содержит актуальную квитанцию; err содержит отказ её чтения.
		current, err := svc.GetReceipt(context.Background(), id)
		if err != nil {
			diagnose()
			t.Fatalf("чтение квитанции id=%s RequestID=%q: %v", id, result.RequestID, err)
		}
		result = current
		if result.Status.Terminal() && result.Status != status {
			diagnose()
			t.Fatalf("квитанция %s RequestID=%q: ожидалось %s, получено %+v", id, result.RequestID, status, result)
		}
		return result.Status == status
	}, diagnose)
	return result
}

// Проверяет predicate каждые пять миллисекунд до успеха либо десятисекундного срока. Перед ошибкой
// срока вызывает переданные диагностические функции.
func sessionAcceptanceWait(t *testing.T, predicate func() bool, timeoutDiagnostics ...func()) {
	t.Helper()
	// timer ограничивает ожидание условия жизненного цикла десятью секундами.
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	// ticker задаёт пятимиллисекундный период повторной проверки условия.
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !predicate() {
		select {
		case <-timer.C:
			// capture содержит очередной диагностический обработчик истечения срока условия.
			for _, capture := range timeoutDiagnostics {
				capture()
			}
			t.Fatal("условие lifecycle не выполнено до срока")
		case <-ticker.C:
		}
	}
}

// Ожидает успешную квитанцию, проверяет RUNNING и доступность Feed именно этой Session. При отказе
// сохраняет диагностику и завершает тест.
func (f *sessionAcceptanceFixture) running(t *testing.T, receipt core.CommandReceipt) core.Session {
	t.Helper()
	// diagnose сохраняет снимок собственной фикстуры при отказе перехода Session в RUNNING.
	diagnose := func() {
		// Сохраняет диагностический снимок при отказе перехода Session к работе.
		f.captureFailure(t)
	}
	sessionAcceptanceReceipt(t, f.service, receipt.ID, core.CommandSucceeded, diagnose)
	// current содержит сохранённую Session после успешной квитанции; err содержит отказ чтения.
	current, err := f.service.GetSession(context.Background(), receipt.SessionID)
	if err != nil || current.State != core.SessionRunning {
		diagnose()
		t.Fatalf("RequestID=%q receipt=%+v Session=%+v: %v", receipt.RequestID, receipt, current, err)
	}
	// feedErr сохраняет последний результат получения Feed для ограниченного ожидания и диагностики.
	var feedErr error
	sessionAcceptanceWait(t, func() bool {
		// Повторно проверяет доступность Feed после успешной квитанции запуска.
		_, feedErr = f.manager.Feed(current)
		return feedErr == nil
	}, func() {
		// Записывает последнюю ошибку Feed и сохраняет диагностику при истечении ожидания.

		t.Logf("Feed RequestID=%q receipt=%+v Session=%+v: %v", receipt.RequestID, receipt, current, feedErr)
		diagnose()
	})
	return current
}

// Возвращает первую страницу до 32 сохранённых выходов выбранной Session; ошибка чтения завершает
// тест.
func (f *sessionAcceptanceFixture) outputs(t *testing.T, sessionID core.ID) []core.SessionOutput {
	t.Helper()
	// page содержит первую страницу выходов Session; err содержит отказ чтения.
	page, err := f.service.ListOutputs(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 32, SessionID: sessionID}})
	if err != nil {
		t.Fatal(err)
	}
	return page.Items
}

// Проверяет барьер текущих дорожек заглушки: одно видео оставляет Session в STARTING без Feed, OBS и
// исходящей сети. После допуска аудио запускается только выбранная группа, а второй Manager и
// повторный физический запуск по тому же RequestID исключены.
func TestSessionManagerRequiresCurrentAudioVideoFallback(t *testing.T) {
	// helper содержит настоящий бинарник физического владельца процессов.
	helper := sessionAcceptanceBuildHelper(t)
	// receiver содержит локальное RTMP-назначение для проверки отсутствия исходящей сети до
	// готовности.
	receiver, _ := newPublisherReceiver(t, false)
	// f содержит фикстуру с единственным выбранным выходом первой группы.
	f := newSessionAcceptanceFixture(t, helper, []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	// gate указывает файл допуска аудио дочернего источника; started указывает маркер начала видео.
	gate, started := filepath.Join(f.directory, "audio-enabled"), filepath.Join(f.directory, "video-started")
	f.config.Group.Player = func(_ context.Context, _ string, _ *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Создаёт дочерний источник с файловым разрешением аудио для проверки готовности обеих дорожек.

		f.starts.Add(1)
		// binary содержит путь текущего тестового исполняемого файла для управляемого дочернего
		// источника; err содержит отказ определения пути.
		binary, err := os.Executable()
		return ffmpeg.Command{Binary: binary, Args: []string{"-test.run=^TestSessionAcceptanceMediaChild$", "--", a.OutputPath, gate, started}}, err
	}
	f.launch(t)
	// r содержит квитанцию запуска с задержанным аудио выбранной группы.
	r := f.start(t, "start-with-track-barrier", f.destinations[0].ID)
	sessionAcceptanceWait(t, func() bool {
		// Ожидает маркер начала передачи видео до допуска аудиодорожки.
		// err содержит результат проверки маркера первого видеопакета дочернего источника.
		_, err := os.Stat(started)
		return err == nil
	})
	// current содержит Session до допуска аудио; err содержит отказ её чтения.
	current, err := f.service.GetSession(context.Background(), r.SessionID)
	if err != nil || current.State != core.SessionStarting || current.AllowsOBS(f.stream.PublicationKey) {
		t.Fatalf("видео без аудио прошло barrier: %+v %v", current, err)
	}
	if _, err = f.manager.Feed(current); err == nil {
		t.Fatal("Feed доступен до обеих дорожек")
	}
	if receiver.accepted.Load() != 0 {
		t.Fatal("сеть началась до parent readiness")
	}
	if err = os.WriteFile(gate, []byte{1}, 0600); err != nil {
		t.Fatal(err)
	}
	current = f.running(t, r)
	receiver.wait(t, func(m []publisherWireMessage) bool {
		// Ожидает медиа после подтверждённой готовности аудио и видео.
		return publisherMediaCount(m) >= 8
	})
	if len(current.Snapshot.Groups) != 2 || f.starts.Load() != 1 || len(f.outputs(t, current.ID)) != 1 {
		t.Fatal("невыбранная группа запущена либо потеряна в снимке")
	}
	if _, err = session.New(context.Background(), f.config); !errors.Is(err, core.ErrInUse) {
		t.Fatalf("второй диспетчер: %v", err)
	}
	// path указывает текущий файл варианта для временного удаления доступности перед повтором
	// RequestID.
	path := filepath.Join(f.directory, f.generation.RequiredProfiles[0].ManagedPath)
	if err = os.Rename(path, path+".temporarily-unavailable"); err != nil {
		t.Fatal(err)
	}
	// duplicate содержит квитанцию повторного смыслового запуска, которая должна совпасть с
	// исходной.
	duplicate := f.start(t, "start-with-track-barrier", f.destinations[0].ID)
	if duplicate.ID != r.ID || f.starts.Load() != 1 {
		t.Fatal("повтор RequestID повторил физический startup")
	}
	if err = os.Rename(path+".temporarily-unavailable", path); err != nil {
		t.Fatal(err)
	}
}

// Дочерний процесс выдаёт структурные AU; декодируемость изображения проверяется отдельно.
func TestSessionAcceptanceMediaChild(t *testing.T) {
	// index ищет разделитель -- между флагами тестового процесса и аргументами дочернего источника.
	index := -1
	// i указывает позицию аргумента процесса; arg содержит текущий аргумент при поиске --.
	for i, arg := range os.Args {
		if arg == "--" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	// args содержит путь выходного UDS, файл допуска аудио и маркер начала видео.
	args := os.Args[index+1:]
	if len(args) != 3 {
		os.Exit(61)
	}
	// conn содержит подключение дочернего источника к выходному UDS; err содержит отказ подключения.
	conn, err := net.Dial("unix", args[0])
	if err != nil {
		os.Exit(62)
	}
	defer func() {
		// Закрывает UDS дочернего источника при завершении его передачи.
		_ = conn.Close()
	}()
	// v содержит AV1 640×360 конфигурацию; a содержит AAC-конфигурацию той же эпохи.
	v, a := publisherConfigs(t, publisherProfile(core.CodecAV1, 640, 360), 1)
	// mux передаёт структурные A/V-пакеты через выходной UDS в fMP4.
	mux, err := fmp4.NewMuxer(conn, fmp4.Init{Video: v, Audio: a}, integrationLimits())
	if err != nil {
		os.Exit(63)
	}
	// ticker задаёт темп 60 видеопакетов за секунду управляемого источника.
	ticker := time.NewTicker(time.Second / 60)
	defer ticker.Stop()
	// audio хранит следующую AAC-метку и привязывается к текущему видео при первом допуске аудио.
	var audio int64
	// video задаёт непрерывную видеометку дочернего источника.
	for video := int64(0); ; video++ {
		// packet содержит текущую структурную единицу доступа; err содержит отказ её создания
		// или записи.
		packet, err := media.NewPacket(media.PacketInfo{PTS: video, DTS: video, DurationTicks: 1, Keyframe: true}, v, publisherPayload(v, 10))
		if err != nil || mux.WritePacket(packet) != nil {
			return
		}
		if video == 0 {
			if err = os.WriteFile(args[2], []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
				os.Exit(64)
			}
		}
		if _, err = os.Stat(args[1]); err == nil {
			if audio == 0 {
				audio = video * 800
			}
			for audio*60 < (video+1)*48000 {
				packet, err = media.NewPacket(media.PacketInfo{PTS: audio, DTS: audio, DurationTicks: 1024}, a, publisherPayload(a, 0))
				if err != nil || mux.WritePacket(packet) != nil {
					return
				}
				audio += 1024
			}
		}
		<-ticker.C
	}
}

// Проверяет, что отказ локального проигрывателя завершает запуск Session с ошибкой, отзывает Feed и в
// итоге снимает Issued и CleanupPending.
func TestSessionManagerCommonFailureEndsSessionAndJoinsResources(t *testing.T) {
	// f содержит фикстуру, чей проигрыватель намеренно завершается через /usr/bin/false.
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), []string{sessionAcceptanceRejectingEndpoint(t)}, []int{0})
	f.config.Group.Player = func(context.Context, string, *os.File, ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Создаёт заведомо неуспешный проигрыватель для проверки общего отказа запуска.

		return ffmpeg.Command{Binary: "/usr/bin/false"}, nil
	}
	f.launch(t)
	// r содержит квитанцию запуска с общим локальным отказом.
	r := f.start(t, "start-with-local-failure")
	sessionAcceptanceReceipt(t, f.service, r.ID, core.CommandFailed)
	// current содержит Session после отказа старта; err содержит отказ чтения её FAILED-состояния.
	current, err := f.service.GetSession(context.Background(), r.SessionID)
	if err != nil || current.State != core.SessionFailed || current.Failure == "" {
		t.Fatalf("общий startup failure: %+v %v", current, err)
	}
	if _, err = f.manager.Feed(current); err == nil {
		t.Fatal("Feed сохранён после startup failure")
	}
	sessionAcceptanceWait(t, func() bool {
		// Ожидает подтверждение очистки Session после ошибки локального проигрывателя.

		// current содержит очередное состояние Session при ожидании снятия Issued и
		// CleanupPending; err содержит отказ чтения.
		current, err := f.service.GetSession(context.Background(), r.SessionID)
		return err == nil && !current.CleanupPending && !current.Issued
	})
}

// Задерживает построение медиакоманды и принимает Stop либо Detach выхода до завершения запуска.
// Проверяет отсутствие выдачи остановленного выхода, сохранение истории и работоспособность
// оставшегося соседа; пустой набор выходов также допускает RUNNING.
func TestSessionManagerStopDuringStartupExcludesOnlyItsOutput(t *testing.T) {
	// helper содержит путь настоящего вспомогательного процесса для вариантов stop и detach во время
	// запуска.
	helper := sessionAcceptanceBuildHelper(t)
	// scenario задаёт число оставшихся выходов и вид исключения текущего выхода.
	for _, scenario := range []struct {
		// remaining задаёт ноль либо один здоровый выход после исключения соседа.
		remaining int
		// detach выбирает отсоединение вместо остановки с сохранённым прикреплением.
		detach bool
	}{{0, false}, {1, false}, {0, true}, {1, true}} {
		t.Run(fmt.Sprintf("remaining-%d-detach-%v", scenario.remaining, scenario.detach), func(t *testing.T) {
			// Проверяет исключение одного выхода во время запуска при выбранных остановке или отсоединении и числе
			// оставшихся выходов.

			// receiver содержит общий исправный RTMP-приёмник сценария.
			receiver, _ := newPublisherReceiver(t, false)
			// endpoints задаёт один либо два выхода на приёмник согласно remaining.
			endpoints := []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}
			if scenario.remaining == 1 {
				endpoints = append(endpoints, endpoints[0])
			}
			// f содержит фикстуру с выходами одной группы и задерживаемым построителем
			// проигрывателя.
			f := newSessionAcceptanceFixture(t, helper, endpoints, make([]int, len(endpoints)))
			// entered подтверждает начало построения медиакоманды; release разрешает
			// продолжить после исключения выхода.
			entered, release := make(chan struct{}, 1), make(chan struct{})
			f.config.Group.Player = func(ctx context.Context, _ string, _ *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
				// Задерживает создание проигрывателя до исключения выхода, сохраняя отменяемость ожидания.

				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-ctx.Done():
					return ffmpeg.Command{}, ctx.Err()
				case <-release:
				}
				return publisherControlledCommand(publisherProfile(core.CodecAV1, 640, 360), a, 10, "")
			}
			f.launch(t)
			// r содержит принятую команду запуска до исключения первого выхода.
			r := f.start(t, "start-before-output-exclusion")
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("builder не начался")
			}
			// outputs содержит сохранённые предварительно созданные выходы стартующей
			// Session.
			outputs := f.outputs(t, r.SessionID)
			// stopped сохраняет выход первого назначения, который исключается до выдачи
			// исполнения.
			var stopped core.SessionOutput
			// o содержит очередной предварительно созданный выход при поиске первого
			// назначения.
			for _, o := range outputs {
				if o.Snapshot.Destination.ID == f.destinations[0].ID {
					stopped = o
				}
			}
			// command задаёт запрос stop или detach точного предварительно созданного
			// выхода.
			command := core.OutputCommand{RequestID: "stop-during-startup", OutputID: stopped.ID}
			// stop принимает квитанцию выбранной команды исключения выхода.
			var stop core.CommandReceipt
			// err содержит ошибку stop или detach до продолжения запуска.
			var err error
			if scenario.detach {
				stop, err = f.manager.DetachOutput(context.Background(), command)
			} else {
				stop, err = f.manager.StopOutput(context.Background(), command)
			}
			if err != nil {
				t.Fatal(err)
			}
			close(release)
			// current содержит успешно запущенную Session после исключения выбранного
			// выхода.
			current := f.running(t, r)
			sessionAcceptanceReceipt(t, f.service, stop.ID, core.CommandSucceeded)
			// output содержит сохранённый исключённый выход для проверки отсутствия выдачи и
			// правильного прикрепления.
			output, err := f.service.GetOutput(context.Background(), stopped.ID)
			if err != nil || output.DesiredRunning || output.CleanupPending || output.Issued || output.Attempt != 0 {
				t.Fatalf("фиктивная выдача/неочищенный provisional Output: %+v %v", output, err)
			}
			if output.Attached == scenario.detach {
				t.Fatal("stop/detach потерял контракт прикрепления")
			}
			if len(current.Snapshot.Groups) != 2 || len(f.outputs(t, current.ID)) != len(endpoints) {
				t.Fatal("startup переписал историю")
			}
			if scenario.remaining == 0 {
				if receiver.accepted.Load() != 0 {
					t.Fatal("остановленный выход открыл сеть")
				}
			} else {
				receiver.wait(t, func(m []publisherWireMessage) bool {
					// Ожидает передачу оставшегося выхода после исключения соседнего во время запуска.
					return publisherMediaCount(m) >= 8
				})
				if receiver.accepted.Load() != 1 {
					t.Fatal("соседний выход пересоздан либо остановленный запущен")
				}
			}
		})
	}
}

// Добавляет недоступное назначение к работающей Session, дожидается RETRYING и отсоединяет его во
// время паузы. Здоровое соединение, Feed, допуск OBS и история запуска сохраняются.
func TestSessionManagerDynamicOutputsPreserveHealthyConnection(t *testing.T) {
	// receiver содержит исправное исходное назначение динамического сценария.
	receiver, _ := newPublisherReceiver(t, false)
	// f содержит работающую Session с исправным и отдельно недоступным назначениями разных групп.
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), []string{"rtmp://" + receiver.listener.Addr().String() + "/live", sessionAcceptanceRejectingEndpoint(t)}, []int{0, 1})
	f.launch(t)
	// r содержит квитанцию запуска только исправного назначения.
	r := f.start(t, "start-healthy", f.destinations[0].ID)
	// current содержит подтверждённую RUNNING Session с доступным Feed.
	current := f.running(t, r)
	receiver.wait(t, func(m []publisherWireMessage) bool {
		// Ожидает исходную передачу исправного выхода перед добавлением недоступного назначения.
		return publisherMediaCount(m) >= 8
	})
	// add содержит квитанцию добавления недоступного назначения; err содержит отказ принятия
	// команды.
	add, err := f.manager.AddOutput(context.Background(), core.AddOutputRequest{RequestID: "add-unavailable-destination", SessionID: current.ID, DestinationID: f.destinations[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	sessionAcceptanceReceipt(t, f.service, add.ID, core.CommandSucceeded)
	// second содержит сохранённый добавленный выход для поиска его RETRYING-наблюдения.
	second, err := f.service.GetOutput(context.Background(), add.OutputID)
	if err != nil {
		t.Fatal(err)
	}
	sessionAcceptanceWait(t, func() bool {
		// Ожидает состояние повторного подключения только у добавленного недоступного выхода.

		// snapshot содержит текущие наблюдения точного исполнения Session; err содержит отказ
		// чтения.
		snapshot, err := f.service.RuntimeSnapshot(context.Background(), core.RuntimeSessionFence{SessionID: current.ID, Incarnation: current.Incarnation, Lifecycle: current.Lifecycle})
		if err != nil {
			return false
		}
		// output содержит очередного наблюдателя при поиске добавленного повторяющего
		// подключение выхода.
		for _, output := range snapshot.Outputs {
			if output.Execution.OutputID == second.ID && output.Value != nil && output.Value.State == core.OutputRetrying {
				return true
			}
		}
		return false
	})
	// before сохраняет число медиасообщений исправного соединения перед паузой и detach соседа.
	before := publisherMediaCount(receiver.snapshot())
	// pause содержит квитанцию ручной паузы той же Session.
	pause, err := f.manager.PauseSession(context.Background(), core.SessionCommand{RequestID: "pause-with-retrying-destination", SessionID: current.ID})
	if err != nil {
		t.Fatal(err)
	}
	sessionAcceptanceReceipt(t, f.service, pause.ID, core.CommandSucceeded)
	// paused содержит сохранённую паузу для проверки Feed и допуска OBS.
	paused, err := f.service.GetSession(context.Background(), current.ID)
	if err != nil || !paused.DesiredPaused || !paused.AllowsOBS(f.stream.PublicationKey) {
		t.Fatalf("pause: %+v %v", paused, err)
	}
	// detach содержит квитанцию отсоединения недоступного повторяющего подключение выхода.
	detach, err := f.manager.DetachOutput(context.Background(), core.OutputCommand{RequestID: "detach-retrying-destination", OutputID: second.ID})
	if err != nil {
		t.Fatal(err)
	}
	sessionAcceptanceReceipt(t, f.service, detach.ID, core.CommandSucceeded)
	receiver.wait(t, func(m []publisherWireMessage) bool {
		// Проверяет продолжение медиа исправного выхода после паузы и отсоединения недоступного соседа.
		return publisherMediaCount(m) >= before+8
	})
	if receiver.accepted.Load() != 1 {
		t.Fatal("изменения соседа пересоздали здоровый socket")
	}
	if _, err = f.manager.Feed(paused); err != nil {
		t.Fatal("изолированный отказ потерял Feed:", err)
	}
	if len(current.Snapshot.Groups) != 2 || len(f.outputs(t, current.ID)) != 2 {
		t.Fatal("Add/Detach переписали историю запуска")
	}
}
