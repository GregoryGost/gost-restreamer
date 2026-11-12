package group

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring/logging"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// Проверка требует немедленного наблюдения ошибки завершённой попытки до повторного запуска.
func TestManagedFallbackNativeFailureIsObservedBeforeRetry(t *testing.T) {
	// f предоставляет сохранённую работающую Session и настройки управляемого fallback-сценария.
	f := runtimeUnitFixture(t)
	// ctx ограничивает проверку native-отказа пятнадцатью секундами; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// err сообщает об отказе закрытия первоначального наблюдателя перед настройкой событий.
	if err := f.config.Observer.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе настройки ограниченного реестра телеметрии для проверки раннего
	// события.
	if err := f.config.Core.ConfigureTelemetry(ctx, core.TelemetryLimits{
		MediaStaleAfter: 5 * time.Second,
		Events:          core.EventLimits{MaxEvents: 64, MaxPageSize: 64, MaxBytes: 1 << 20, MaxEventBytes: 16384},
		MaxGPUDevices:   1, MaxGPUUUIDBytes: 64, MaxGPUBytes: 4096,
	}); err != nil {
		t.Fatal(err)
	}
	// observer содержит повторно открытого наблюдателя Session; err сохраняет результаты создания
	// ресурсов и чтения наблюдений сценария.
	observer, err := f.config.Core.OpenSessionObserver(ctx, f.config.Parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает повторно открытого наблюдателя Session и сообщает ошибку через t.Error.

		// err сообщает об отказе резервного закрытия наблюдателя Session.
		if err := observer.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	f.config.Observer = observer
	// helper задаёт временный путь настоящего вспомогательного процесса.
	helper := filepath.Join(t.TempDir(), "gost-process-helper")
	// args собирает аргументы сборки helper с необязательной инструментацией покрытия.
	args := []string{"build", "-o", helper}
	// directory содержит явно переданный каталог покрытия дочерних процессов.
	if directory := os.Getenv("GOST_TEST_PROCESS_HELPER_COVERDIR"); directory != "" {
		t.Setenv("GOCOVERDIR", directory)
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=github.com/GregoryGost/gost-restreamer/...")
	}
	// output содержит диагностику сборки helper; err сообщает об отказе сборки.
	if output, err := exec.Command("go", append(args, "../../../cmd/gost-process-helper")...).CombinedOutput(); err != nil {
		t.Fatalf("сборка настоящего helper: %v %s", err, output)
	}
	// parent задаёт короткий временный корень управляемых UDS с учётом Darwin.
	parent := os.TempDir()
	if runtime.GOOS == "darwin" {
		// Короткий путь нужен для вложенных UDS на macOS.
		parent = "/tmp"
	}
	// runtimeRoot содержит отдельный каталог временных ресурсов процессов.
	runtimeRoot, err := os.MkdirTemp(parent, "gfa-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Удаляет временный корень управляемых UDS и сообщает ошибку через t.Error.

		// err сообщает об отказе удаления управляемого runtime-каталога после теста.
		if err := os.RemoveAll(runtimeRoot); err != nil {
			t.Error(err)
		}
	})
	// journalRoot задаёт изолированный корень долговечного журнала процессов.
	journalRoot := t.TempDir()
	// processes содержит настоящий реестр процессов с собранным helper и конечными пределами.
	processes, err := processowner.New(processowner.Config{Root: journalRoot, RuntimeRoot: runtimeRoot, HelperPath: helper,
		GracePeriod: 30 * time.Millisecond,
		Limits:      processowner.Limits{MaxOperations: 100, MaxRecordBytes: 16384, MaxCommandArgs: 128, MaxCommandBytes: 4096, MaxExtraFiles: 1, PageSize: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает тестовый реестр процессов и сообщает ошибку через t.Error.

		// err сообщает об отказе резервного закрытия реестра процессов.
		if err := processes.Close(); err != nil {
			t.Error(err)
		}
	})
	// scope содержит точную область операций проверяемого отказа запуска.
	scope, err := processes.Create(ctx, "synthetic-launch-failure")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Запечатывает и закрывает область, проверяет физическое завершение и только затем подтверждает его
		// реестру; ошибки сообщает тесту.

		// cleanup ограничивает проверку и подтверждение завершения области тремя секундами; stop
		// освобождает таймер.
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		// err сообщает об отказе Seal при резервной очистке области.
		if err := scope.Seal(cleanup); err != nil {
			t.Error(err)
		}
		// err сообщает об отказе Close области при резервной очистке.
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
		// proof содержит доказательство физического завершения; err сообщает об отказе его
		// получения.
		proof, err := scope.Resolve(cleanup)
		t.Logf("cleanup scope=%s joined=%v pending=%d err=%v", scope.ID(), proof.Joined, proof.PendingOperations, err)
		if err != nil || !proof.Joined {
			t.Error("физическое завершение не подтверждено", proof, err)
			return
		}
		// err сообщает об отказе подтверждения уже присоединённой области реестру.
		if err := processes.AcknowledgeScope(cleanup, scope.ID()); err != nil {
			t.Error(err)
		}
	})
	// mediaPath задаёт обычный контрольный файл, передаваемый неуспешной попытке как FD 3.
	mediaPath := filepath.Join(t.TempDir(), "synthetic-media")
	// err сообщает об отказе записи контрольного входного файла.
	if err := os.WriteFile(mediaPath, []byte("synthetic-regular-file"), 0600); err != nil {
		t.Fatal(err)
	}
	// missing задаёт заведомо отсутствующий исполняемый файл FFmpeg для настоящего отказа Start.
	missing := filepath.Join(t.TempDir(), "absent-ffmpeg")
	// err содержит результат проверки отсутствия выбранного бинарника.
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("исполняемый файл должен отсутствовать", err)
	}
	// commandWitness сохраняет локальный номер и выходной путь команды для проверки очистки до
	// повтора.
	type commandWitness struct {
		// id содержит локальный номер физической попытки Supervisor.
		id uint64
		// path содержит точный выходной UDS этой попытки.
		path string
	}
	// commands передаёт сведения о построенных командах до отказа native Start.
	commands := make(chan commandWitness, 2)
	// builders считает вызовы построителя для проверки отсутствия повтора до раннего наблюдения
	// ошибки.
	var builders atomic.Uint64
	f.config.Runtime.Scope = scope
	f.config.Runtime.Binary = missing
	f.config.Runtime.FFmpeg.StartupTimeout = 5 * time.Second
	f.config.Runtime.ShutdownTimeout = 3 * time.Second
	f.config.Runtime.FFmpeg.ShutdownTimeout = 3 * time.Second
	f.config.Runtime.FFmpeg.MaxRestarts = 1
	f.config.Runtime.FFmpeg.RestartBackoff = 30 * time.Second
	f.config.Runtime.OpenFile = func(context.Context, core.ArtifactManifest) (*os.File, error) {
		// Открывает отдельный дескриптор условного подготовленного файла для каждой попытки проигрывателя.
		return os.Open(mediaPath)
	}
	f.config.Runtime.Player = func(_ context.Context, _ string, file *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Проверяет существование выходного сокета, считает вызовы и возвращает команду с отсутствующим
		// бинарником, сохраняя входной файл как ExtraFiles.

		// err сообщает об отсутствии ожидаемого выходного сокета при построении команды.
		if _, err := os.Stat(a.OutputPath); err != nil {
			return ffmpeg.Command{}, err
		}
		builders.Add(1)
		commands <- commandWitness{a.ID, a.OutputPath}
		return ffmpeg.Command{Binary: missing, ExtraFiles: []*os.File{file}}, nil
	}
	// live содержит входной Router группы, не используемый для восстановления отказавшего
	// проигрывателя.
	live, err := media.NewRouter(f.config.Runtime.Switcher.Router)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает LIVE Router после проверки отказа запуска, игнорируя ошибку.
		_ = live.Close()
	})
	// registry содержит реестр группы с настоящим ProcessScope и отсутствующим FFmpeg.
	registry, err := NewRegistry(ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает Registry после проверки результата попытки и сообщает ошибку через t.Error.

		// err сообщает об отказе резервного закрытия реестра группы.
		if err := registry.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	// anchor сохраняет курсор оперативных событий до выдачи ссылки и первой попытки.
	anchor, err := f.config.Core.RuntimeAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// reference удерживает группу во время ожидания повторной попытки.
	reference, err := registry.Acquire(ctx, f.snapshot, live)
	if err != nil {
		t.Fatal(err)
	}
	// command сохраняет сведения о первой построенной команде для проверки удалённого Namespace.
	var command commandWitness
	select {
	case command = <-commands:
	case <-ctx.Done():
		t.Fatal("построитель не завершился", ctx.Err())
	}
	// ticker задаёт период ожидания исчезновения каталога завершившейся попытки.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		// err содержит результат проверки удаления каталога первой физической попытки.
		if _, err := os.Stat(filepath.Dir(command.path)); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("попытка не завершила namespace", ctx.Err())
		}
	}
	if builders.Load() != 1 || command.id != 1 {
		t.Fatal("наблюдение вышло за первую попытку", builders.Load(), command.id)
	}
	select {
	case <-reference.Group().Done():
		t.Fatal("worker завершён вместо ожидания повторной попытки")
	default:
	}
	// operations содержит пути записей операций точного Scope для поиска результата native Start.
	operations, err := filepath.Glob(filepath.Join(journalRoot, "scopes", scope.ID(), "operations", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	// native сохраняет прочитанный результат запуска отсутствующего бинарника из журнала процессов.
	var native struct {
		// Kind различает native-операцию; State содержит стадию её завершения; Binary содержит
		// исполняемый путь; Error содержит полную причину отказа.
		Kind, State, Binary, Error string
		// ExitCode содержит сохранённый код завершения native-операции, равный -1 при отказе
		// запуска.
		ExitCode int
	}
	// foundNative отмечает найденную запись native для конкретного отсутствующего бинарника.
	foundNative := false
	// path содержит очередной файл операции журнала проверяемой области.
	for _, path := range operations {
		// data содержит полные байты записи операции; err сообщает об отказе чтения.
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// op содержит декодируемую запись очередной операции для поиска native-отказа.
		var op struct {
			// Kind задаёт вид прочитанной операции; State задаёт её стадию; Binary содержит
			// путь команды; Error содержит сохранённую полную ошибку.
			Kind, State, Binary, Error string
			// ExitCode содержит код завершения прочитанной операции.
			ExitCode int
		}
		// err сообщает об отказе разбора JSON-записи операции.
		if err := json.Unmarshal(data, &op); err != nil {
			t.Fatal(err)
		}
		if op.Kind == "native" && op.Binary == missing {
			native = op
			foundNative = true
		}
	}
	if !foundNative || native.State != "JOINED_RESULT" || native.ExitCode != -1 || !strings.Contains(native.Error, missing) {
		t.Fatalf("ошибка native Start не подтверждена: found=%v op=%+v", foundNative, native)
	}
	// fence содержит точную идентичность Session для проверки принадлежности оперативного события.
	fence := observer.SessionFence()
	// worker сохраняет внутреннее FFmpeg-наблюдение, соответствующее найденному событию отказа.
	var worker core.FFmpegObservationSnapshot
	// cursor задаёт текущую позицию чтения событий после исходного anchor.
	cursor := anchor.Cursor
	// failureEvent отмечает найденный отказ первой попытки до повторного запуска.
	failureEvent := false
	for !failureEvent {
		// page содержит очередную страницу оперативных событий; err сообщает об отказе её
		// чтения.
		page, err := f.config.Core.RuntimeEvents(ctx, cursor, 64)
		if err != nil || page.Gap || page.Reset {
			t.Fatal("неполная страница событий", page, err)
		}
		// event содержит очередное событие для поиска точной роли проигрывателя и причины
		// отказа.
		for _, event := range page.Events {
			if event.Owner != core.RuntimeFFmpegOwner || event.Role != core.FFmpegFallbackPlayer || event.Reason != core.ReasonObservationFailed {
				continue
			}
			if event.Session != fence || event.GroupInstanceID != reference.Group().observer.Token() || event.Stamp.AttemptID != command.id {
				t.Fatal("ошибка приписана чужому владельцу", event)
			}
			// snapshot содержит текущие внутренние наблюдения Session; err сообщает об
			// отказе их чтения.
			snapshot, err := f.config.Core.RuntimeSnapshot(ctx, fence)
			if err != nil {
				t.Fatal(err)
			}
			// value содержит очередное FFmpeg-наблюдение при поиске токена найденного
			// события.
			for _, value := range snapshot.FFmpeg {
				if value.Stamp.Token == event.Stamp.Token {
					worker = value
				}
			}
			// encoded содержит JSON оперативного события для проверки отсутствия
			// произвольной native-диагностики.
			encoded, err := json.Marshal(event)
			if err != nil || strings.Contains(string(encoded), missing) || strings.Contains(string(encoded), native.Error) {
				t.Fatal("безопасное событие содержит диагностику процесса", err)
			}
			failureEvent = true
		}
		cursor = page.Cursor
		if !failureEvent {
			select {
			case <-page.Changed:
			case <-ctx.Done():
				t.Fatal("завершённая native-попытка не опубликовала ошибку до retry", ctx.Err())
			}
		}
	}
	if worker.Value == nil || worker.Stamp.AttemptID != 1 || (worker.Value.State != core.ObservationFailed && worker.Value.State != core.ObservationRetrying) || !strings.Contains(worker.Value.Error, native.Error) {
		t.Fatalf("внутреннее наблюдение потеряло точный результат попытки: %+v, native=%+v", worker, native)
	}
	if builders.Load() != 1 {
		t.Fatal("повторная попытка началась до публикации ошибки", builders.Load())
	}
	select {
	case <-reference.Group().Done():
		t.Fatal("ошибка появилась только после общего завершения worker")
	default:
	}
	// err сообщает об отказе закрытия последней ссылки и присоединения группы во время ожидания
	// повтора.
	if err := reference.CloseContext(ctx); err != nil {
		t.Fatal("ожидание повтора не отменилось с физическим join", err)
	}
	select {
	case <-reference.Group().Done():
	default:
		t.Fatal("закрытие ссылки не присоединило группу")
	}
	if builders.Load() != 1 {
		t.Fatal("после отмены запущена новая попытка", builders.Load())
	}
}

// Проверяет сопоставление локальной и доменной попытки и невозможность позднего результата
// перезаписать другую попытку; штатная отмена наблюдается как STOPPED без ошибки выполнения.
func TestFFmpegAttemptResultCannotOverwriteOtherAttemptBinding(t *testing.T) {
	// f предоставляет точное выполнение и наблюдателя Session для проверки запоздалых результатов
	// попыток.
	f := runtimeUnitFixture(t)
	// ctx задаёт общий контекст операций наблюдения попыток.
	ctx := context.Background()
	// groupObserver содержит наблюдателя группы; err сохраняет результаты открытия и чтения точных
	// наблюдений.
	groupObserver, err := f.config.Observer.OpenGroup(ctx, f.snapshot.Group.ID, f.snapshot.Profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает наблюдателя группы после проверки устаревших результатов, игнорируя ошибку.
		_ = groupObserver.Close(context.Background())
	})
	// workerObserver содержит наблюдателя роли LIVE-перекодирования с последовательными доменными
	// попытками.
	workerObserver, err := groupObserver.OpenFFmpegRole(ctx, core.FFmpegLiveTranscode)
	if err != nil {
		t.Fatal(err)
	}
	// g содержит настройки публикации результатов FFmpeg-попыток.
	g := &Group{config: f.config.Runtime}
	// binding атомарно хранит текущую пару локального и доменного номера попытки.
	var binding atomic.Pointer[attemptBinding]
	// callback принимает результат только физической попытки, совпавшей с текущей привязкой.
	callback := g.observeAttemptResult(&binding)
	// failure задаёт контрольную полную ошибку native-попытки.
	failure := errors.New("синтетический отказ native-попытки")
	callback(ffmpeg.AttemptResult{ID: 1, Err: failure})
	// first содержит наблюдателя доменной попытки 37, связанной с локальной попыткой 1.
	first, err := workerObserver.BeginAttempt(ctx, 37)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе первоначального наблюдения STARTING попытки 37.
	if err := first.Observe(ctx, core.FFmpegObservation{State: core.ObservationStarting}); err != nil {
		t.Fatal(err)
	}
	binding.Store(bindFFmpegAttempt(1, 37, first, nil))
	// read возвращает текущее наблюдение точного FFmpeg-работника из реестра Session.
	read := func() core.FFmpegObservationSnapshot {
		// Читает текущее наблюдение точного работника из RuntimeSnapshot; отсутствие работника немедленно
		// завершает тест.

		t.Helper()
		// snapshot содержит текущие наблюдения Session; err сообщает об отказе чтения реестра.
		snapshot, err := f.config.Core.RuntimeSnapshot(ctx, f.config.Observer.SessionFence())
		if err != nil {
			t.Fatal(err)
		}
		// item содержит очередное FFmpeg-наблюдение при поиске токена проверяемого работника.
		for _, item := range snapshot.FFmpeg {
			if item.Stamp.Token == workerObserver.Token() {
				return item
			}
		}
		t.Fatal("исчез текущий worker")
		return core.FFmpegObservationSnapshot{}
	}
	// before сохраняет состояние до запоздалого или несовпадающего результата.
	before := read()
	callback(ffmpeg.AttemptResult{ID: 2, Err: failure})
	// got содержит наблюдение после результата локальной попытки, для которой ещё нет новой
	// привязки.
	if got := read(); got.Stamp != before.Stamp || got.Value == nil || *got.Value != *before.Value {
		t.Fatal("до построения новой команды перезаписана предыдущая попытка", got)
	}
	callback(ffmpeg.AttemptResult{ID: 1, Err: failure})
	// got содержит наблюдение после совпавшего результата local=1 и core=37.
	if got := read(); got.Stamp.AttemptID != 37 || got.Value == nil || got.Value.State != core.ObservationFailed || got.Value.Error != failure.Error() {
		t.Fatal("потеряно сопоставление local/core attempt", got)
	}
	// replacement содержит новую доменную попытку 38 того же работника.
	replacement, err := workerObserver.BeginAttempt(ctx, 38)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе начального STARTING новой доменной попытки.
	if err := replacement.Observe(ctx, core.FFmpegObservation{State: core.ObservationStarting}); err != nil {
		t.Fatal(err)
	}
	before = read()
	callback(ffmpeg.AttemptResult{ID: 1, Err: failure})
	// got содержит наблюдение после обращения прежнего точного наблюдателя к уже заменённой доменной
	// попытке.
	if got := read(); got.Stamp != before.Stamp || got.Value == nil || *got.Value != *before.Value {
		t.Fatal("старый exact observer изменил новую core-попытку", got)
	}
	binding.Store(bindFFmpegAttempt(2, 38, replacement, nil))
	callback(ffmpeg.AttemptResult{ID: 1, Err: failure})
	// got содержит наблюдение после запоздалого local=1 при новой привязке local=2.
	if got := read(); got.Stamp != before.Stamp || got.Value == nil || *got.Value != *before.Value {
		t.Fatal("поздний local result изменил новое сопоставление", got)
	}
	callback(ffmpeg.AttemptResult{ID: 2, Err: context.Canceled, Stopped: true})
	// got содержит наблюдение после обычной отмены новой попытки для проверки STOPPED без ошибки
	// выполнения.
	if got := read(); got.Stamp.AttemptID != 38 || got.Value == nil || got.Value.State != core.ObservationStopped || got.Value.Error != "" {
		t.Fatal("отмена логического worker представлена как ошибка", got)
	}
}

// Проверяет сохранение ошибки очистки и при STOPPED, безопасный состав оперативного события и полные
// раздельные ошибки и идентичности в JSON-журнале.
func TestFFmpegAttemptResultKeepsStoppedCleanupFailureAndFullJSON(t *testing.T) {
	// stopped выбирает штатную остановку вместо отказа выполнения при общей ошибке очистки.
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "stopped_cleanup_failed"}[stopped], func(t *testing.T) {
			// Проверяет отдельное сохранение ошибки очистки при обычном отказе и остановке, полные ошибки JSON и
			// отсутствие диагностики в оперативном событии.

			// f предоставляет точную Session и конфигурацию для проверки RAM, события и
			// полного JSON результата.
			f := runtimeUnitFixture(t)
			// ctx задаёт общий контекст публикации и чтения результата попытки.
			ctx := context.Background()
			// err сообщает об отказе закрытия первоначального наблюдателя перед настройкой
			// событий.
			if err := f.config.Observer.Close(ctx); err != nil {
				t.Fatal(err)
			}
			// limits задаёт конечные пределы реестра и событий для проверки единственного
			// результата.
			limits := core.TelemetryLimits{MediaStaleAfter: 5 * time.Second, Events: core.EventLimits{MaxEvents: 16, MaxPageSize: 16, MaxBytes: 1 << 20, MaxEventBytes: 16384}, MaxGPUDevices: 1, MaxGPUUUIDBytes: 64, MaxGPUBytes: 4096}
			// err сообщает об отказе настройки телеметрии сценария.
			if err := f.config.Core.ConfigureTelemetry(ctx, limits); err != nil {
				t.Fatal(err)
			}
			// observer содержит повторно открытого наблюдателя Session; err сохраняет
			// результаты открытия работников, публикации и чтения результата.
			observer, err := f.config.Core.OpenSessionObserver(ctx, f.config.Parent)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает наблюдателя Session после проверки результата и JSON, игнорируя ошибку.
				_ = observer.Close(context.Background())
			})
			// groupObserver содержит наблюдателя конкретного экземпляра группы.
			groupObserver, err := observer.OpenGroup(ctx, f.snapshot.Group.ID, f.snapshot.Profile.ID)
			if err != nil {
				t.Fatal(err)
			}
			// worker содержит наблюдателя роли проигрывателя заглушки.
			worker, err := groupObserver.OpenFFmpegRole(ctx, core.FFmpegFallbackPlayer)
			if err != nil {
				t.Fatal(err)
			}
			// attempt содержит наблюдателя доменной попытки 37.
			attempt, err := worker.BeginAttempt(ctx, 37)
			if err != nil {
				t.Fatal(err)
			}
			if err = attempt.Observe(ctx, core.FFmpegObservation{State: core.ObservationStarting}); err != nil {
				t.Fatal(err)
			}
			// output накапливает полные JSON-записи исходной ошибки и отдельной ошибки
			// очистки.
			var output bytes.Buffer
			// sink сохраняет полный структурированный журнал результата попытки в
			// контрольный буфер.
			sink, err := logging.New(logging.Config{MaxRecords: 16, MaxBytes: 1 << 20, MaxRecordBytes: 65536, ShutdownTimeout: time.Second, Level: slog.LevelInfo}, &output)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает приёмник журнала после проверки JSON, игнорируя ошибку резервного закрытия.
				_ = sink.Close(context.Background())
			})
			// g содержит группу с точной корреляцией Session и Group для журнала FFmpeg.
			g := &Group{config: f.config.Runtime, parent: f.config.Parent, id: f.snapshot.Group.ID, observer: groupObserver}
			g.config.Logger = sink.Logger().With("stream_id", "synthetic-stream")
			// binding связывает локальную попытку 3 с доменной попыткой 37 и её
			// наблюдателем.
			var binding atomic.Pointer[attemptBinding]
			binding.Store(bindFFmpegAttempt(3, 37, attempt, g.ffmpegLogger(worker, core.FFmpegFallbackPlayer)))
			// anchor сохраняет курсор событий до публикации проверяемого результата.
			anchor, err := f.config.Core.RuntimeAnchor(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// native задаёт полную контрольную ошибку выполнения с синтетическими
			// спецсимволами.
			native := errors.New("native synthetic-native-token=full/+?\"\ncomplete")
			// cleanup задаёт отдельную полную ошибку очистки с синтетическими спецсимволами.
			cleanup := errors.New("cleanup synthetic-cleanup-token=full/+?\"\ncomplete")
			g.observeAttemptResult(&binding)(ffmpeg.AttemptResult{ID: 3, Err: native, CleanupErr: cleanup, Stopped: stopped})
			// snapshot содержит внутренние наблюдения Session после результата попытки.
			snapshot, err := f.config.Core.RuntimeSnapshot(ctx, observer.SessionFence())
			if err != nil {
				t.Fatal(err)
			}
			// value сохраняет точное оперативное наблюдение проверяемой доменной попытки.
			var value *core.FFmpegObservation
			// item содержит очередное FFmpeg-наблюдение при поиске токена и номера попытки.
			for _, item := range snapshot.FFmpeg {
				if item.Stamp.Token == worker.Token() && item.Stamp.AttemptID == 37 {
					value = item.Value
				}
			}
			// expectedState задаёт FAILED либо STOPPED в соответствии с проверяемым типом
			// завершения.
			expectedState := core.ObservationFailed
			if stopped {
				expectedState = core.ObservationStopped
			}
			if value == nil || value.State != expectedState || !strings.Contains(value.Error, cleanup.Error()) {
				t.Fatal("cleanup не сохранён в точном RAM-наблюдении", value)
			}
			if stopped && strings.Contains(value.Error, native.Error()) {
				t.Fatal("остановленная попытка сохранила ошибку выполнения вместо cleanup", value)
			}
			if !stopped && !strings.Contains(value.Error, native.Error()) {
				t.Fatal("потеряна полная ошибка native", value)
			}
			// page содержит страницу единственного события результата после anchor.
			page, err := f.config.Core.RuntimeEvents(ctx, anchor.Cursor, 16)
			if err != nil || page.Gap || page.Reset || len(page.Events) != 1 {
				t.Fatal("событие результата потеряно", page, err)
			}
			// event содержит проверяемое событие с безопасной причиной и точной
			// принадлежностью.
			event := page.Events[0]
			if event.Kind != core.RuntimeStateEvent || event.Owner != core.RuntimeFFmpegOwner || event.Session != observer.SessionFence() || event.Stamp.Token != worker.Token() || event.Stamp.AttemptID != 37 || event.GroupInstanceID != groupObserver.Token() || event.Role != core.FFmpegFallbackPlayer || event.State != expectedState || event.Reason != core.ReasonObservationFailed {
				t.Fatal("событие имеет неверную принадлежность или причину", event)
			}
			// encoded содержит сериализованное событие для проверки отсутствия произвольных
			// полных ошибок.
			encoded, err := json.Marshal(event)
			if err != nil || strings.Contains(string(encoded), "synthetic-native-token") || strings.Contains(string(encoded), "synthetic-cleanup-token") {
				t.Fatal("оперативное событие содержит произвольную ошибку", err)
			}
			if err = sink.Close(ctx); err != nil {
				t.Fatal(err)
			}
			// decoder разбирает полные JSON-записи контрольного журнала.
			decoder := json.NewDecoder(&output)
			// resultRecords считает записи operation=attempt_result для проверки
			// единственности результата.
			resultRecords := 0
			for {
				// record содержит очередную JSON-запись для проверки идентичности и
				// раздельных полных ошибок.
				var record map[string]any
				err = decoder.Decode(&record)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal("неполная JSON-запись", err)
				}
				if record["stream_id"] != "synthetic-stream" || record["component"] != "ffmpeg" || record["session_id"] != string(f.config.Parent.SessionID) || record["group_id"] != string(f.snapshot.Group.ID) || record["group_instance_id"] != float64(groupObserver.Token()) || record["worker_instance_id"] != float64(worker.Token()) || record["worker_role"] != string(core.FFmpegFallbackPlayer) || record["attempt"] != float64(3) || record["core_attempt"] != float64(37) {
					t.Fatal("JSON потерял точную корреляцию", record)
				}
				if record["operation"] != "attempt_result" {
					continue
				}
				resultRecords++
				if record["msg"] != "ffmpeg_attempt" || record["level"] != "ERROR" || record["error"] != native.Error() || record["cleanup_error"] != cleanup.Error() || record["stopped"] != stopped {
					t.Fatal("JSON изменил полные раздельные ошибки результата", record)
				}
			}
			if resultRecords != 1 {
				t.Fatal("результат попытки записан не один раз", resultRecords)
			}
		})
	}
}
