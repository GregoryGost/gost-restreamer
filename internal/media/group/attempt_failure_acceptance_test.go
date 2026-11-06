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
	f := runtimeUnitFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := f.config.Observer.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.config.Core.ConfigureTelemetry(ctx, core.TelemetryLimits{
		MediaStaleAfter: 5 * time.Second,
		Events:          core.EventLimits{MaxEvents: 64, MaxPageSize: 64, MaxBytes: 1 << 20, MaxEventBytes: 16384},
		MaxGPUDevices:   1, MaxGPUUUIDBytes: 64, MaxGPUBytes: 4096,
	}); err != nil {
		t.Fatal(err)
	}
	observer, err := f.config.Core.OpenSessionObserver(ctx, f.config.Parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := observer.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	f.config.Observer = observer
	helper := filepath.Join(t.TempDir(), "gost-process-helper")
	args := []string{"build", "-o", helper}
	if directory := os.Getenv("GOST_TEST_PROCESS_HELPER_COVERDIR"); directory != "" {
		t.Setenv("GOCOVERDIR", directory)
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=github.com/GregoryGost/gost-restreamer/...")
	}
	if output, err := exec.Command("go", append(args, "../../../cmd/gost-process-helper")...).CombinedOutput(); err != nil {
		t.Fatalf("сборка настоящего helper: %v %s", err, output)
	}
	parent := os.TempDir()
	if runtime.GOOS == "darwin" {
		// Короткий путь нужен для вложенных UDS на macOS.
		parent = "/tmp"
	}
	runtimeRoot, err := os.MkdirTemp(parent, "gfa-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(runtimeRoot); err != nil {
			t.Error(err)
		}
	})
	journalRoot := t.TempDir()
	processes, err := processowner.New(processowner.Config{Root: journalRoot, RuntimeRoot: runtimeRoot, HelperPath: helper,
		GracePeriod: 30 * time.Millisecond,
		Limits:      processowner.Limits{MaxOperations: 100, MaxRecordBytes: 16384, MaxCommandArgs: 128, MaxCommandBytes: 4096, MaxExtraFiles: 1, PageSize: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := processes.Close(); err != nil {
			t.Error(err)
		}
	})
	scope, err := processes.Create(ctx, "synthetic-launch-failure")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := scope.Seal(cleanup); err != nil {
			t.Error(err)
		}
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
		proof, err := scope.Resolve(cleanup)
		t.Logf("cleanup scope=%s joined=%v pending=%d err=%v", scope.ID(), proof.Joined, proof.PendingOperations, err)
		if err != nil || !proof.Joined {
			t.Error("физическое завершение не подтверждено", proof, err)
			return
		}
		if err := processes.AcknowledgeScope(cleanup, scope.ID()); err != nil {
			t.Error(err)
		}
	})
	mediaPath := filepath.Join(t.TempDir(), "synthetic-media")
	if err := os.WriteFile(mediaPath, []byte("synthetic-regular-file"), 0600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "absent-ffmpeg")
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("исполняемый файл должен отсутствовать", err)
	}
	type commandWitness struct {
		id   uint64
		path string
	}
	commands := make(chan commandWitness, 2)
	var builders atomic.Uint64
	f.config.Runtime.Scope = scope
	f.config.Runtime.Binary = missing
	f.config.Runtime.FFmpeg.StartupTimeout = 5 * time.Second
	f.config.Runtime.ShutdownTimeout = 3 * time.Second
	f.config.Runtime.FFmpeg.ShutdownTimeout = 3 * time.Second
	f.config.Runtime.FFmpeg.MaxRestarts = 1
	f.config.Runtime.FFmpeg.RestartBackoff = 30 * time.Second
	f.config.Runtime.OpenFile = func(context.Context, core.ArtifactManifest) (*os.File, error) { return os.Open(mediaPath) }
	f.config.Runtime.Player = func(_ context.Context, _ string, file *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		if _, err := os.Stat(a.OutputPath); err != nil {
			return ffmpeg.Command{}, err
		}
		builders.Add(1)
		commands <- commandWitness{a.ID, a.OutputPath}
		return ffmpeg.Command{Binary: missing, ExtraFiles: []*os.File{file}}, nil
	}
	live, err := media.NewRouter(f.config.Runtime.Switcher.Router)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })
	registry, err := NewRegistry(ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := registry.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	anchor, err := f.config.Core.RuntimeAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := registry.Acquire(ctx, f.snapshot, live)
	if err != nil {
		t.Fatal(err)
	}
	var command commandWitness
	select {
	case command = <-commands:
	case <-ctx.Done():
		t.Fatal("построитель не завершился", ctx.Err())
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
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
	operations, err := filepath.Glob(filepath.Join(journalRoot, "scopes", scope.ID(), "operations", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var native struct {
		Kind, State, Binary, Error string
		ExitCode                   int
	}
	foundNative := false
	for _, path := range operations {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var op struct {
			Kind, State, Binary, Error string
			ExitCode                   int
		}
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
	fence := observer.SessionFence()
	var worker core.FFmpegObservationSnapshot
	cursor := anchor.Cursor
	failureEvent := false
	for !failureEvent {
		page, err := f.config.Core.RuntimeEvents(ctx, cursor, 64)
		if err != nil || page.Gap || page.Reset {
			t.Fatal("неполная страница событий", page, err)
		}
		for _, event := range page.Events {
			if event.Owner != core.RuntimeFFmpegOwner || event.Role != core.FFmpegFallbackPlayer || event.Reason != core.ReasonObservationFailed {
				continue
			}
			if event.Session != fence || event.GroupInstanceID != reference.Group().observer.Token() || event.Stamp.AttemptID != command.id {
				t.Fatal("ошибка приписана чужому владельцу", event)
			}
			snapshot, err := f.config.Core.RuntimeSnapshot(ctx, fence)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range snapshot.FFmpeg {
				if value.Stamp.Token == event.Stamp.Token {
					worker = value
				}
			}
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

func TestFFmpegAttemptResultCannotOverwriteOtherAttemptBinding(t *testing.T) {
	f := runtimeUnitFixture(t)
	ctx := context.Background()
	groupObserver, err := f.config.Observer.OpenGroup(ctx, f.snapshot.Group.ID, f.snapshot.Profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = groupObserver.Close(context.Background()) })
	workerObserver, err := groupObserver.OpenFFmpegRole(ctx, core.FFmpegLiveTranscode)
	if err != nil {
		t.Fatal(err)
	}
	g := &Group{config: f.config.Runtime}
	var binding atomic.Pointer[attemptBinding]
	callback := g.observeAttemptResult(&binding)
	failure := errors.New("синтетический отказ native-попытки")
	callback(ffmpeg.AttemptResult{ID: 1, Err: failure})
	first, err := workerObserver.BeginAttempt(ctx, 37)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Observe(ctx, core.FFmpegObservation{State: core.ObservationStarting}); err != nil {
		t.Fatal(err)
	}
	binding.Store(bindFFmpegAttempt(1, 37, first, nil))
	read := func() core.FFmpegObservationSnapshot {
		t.Helper()
		snapshot, err := f.config.Core.RuntimeSnapshot(ctx, f.config.Observer.SessionFence())
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range snapshot.FFmpeg {
			if item.Stamp.Token == workerObserver.Token() {
				return item
			}
		}
		t.Fatal("исчез текущий worker")
		return core.FFmpegObservationSnapshot{}
	}
	before := read()
	callback(ffmpeg.AttemptResult{ID: 2, Err: failure})
	if got := read(); got.Stamp != before.Stamp || got.Value == nil || *got.Value != *before.Value {
		t.Fatal("до построения новой команды перезаписана предыдущая попытка", got)
	}
	callback(ffmpeg.AttemptResult{ID: 1, Err: failure})
	if got := read(); got.Stamp.AttemptID != 37 || got.Value == nil || got.Value.State != core.ObservationFailed || got.Value.Error != failure.Error() {
		t.Fatal("потеряно сопоставление local/core attempt", got)
	}
	replacement, err := workerObserver.BeginAttempt(ctx, 38)
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Observe(ctx, core.FFmpegObservation{State: core.ObservationStarting}); err != nil {
		t.Fatal(err)
	}
	before = read()
	callback(ffmpeg.AttemptResult{ID: 1, Err: failure})
	if got := read(); got.Stamp != before.Stamp || got.Value == nil || *got.Value != *before.Value {
		t.Fatal("старый exact observer изменил новую core-попытку", got)
	}
	binding.Store(bindFFmpegAttempt(2, 38, replacement, nil))
	callback(ffmpeg.AttemptResult{ID: 1, Err: failure})
	if got := read(); got.Stamp != before.Stamp || got.Value == nil || *got.Value != *before.Value {
		t.Fatal("поздний local result изменил новое сопоставление", got)
	}
	callback(ffmpeg.AttemptResult{ID: 2, Err: context.Canceled, Stopped: true})
	if got := read(); got.Stamp.AttemptID != 38 || got.Value == nil || got.Value.State != core.ObservationStopped || got.Value.Error != "" {
		t.Fatal("отмена логического worker представлена как ошибка", got)
	}
}

func TestFFmpegAttemptResultKeepsStoppedCleanupFailureAndFullJSON(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "stopped_cleanup_failed"}[stopped], func(t *testing.T) {
			f := runtimeUnitFixture(t)
			ctx := context.Background()
			if err := f.config.Observer.Close(ctx); err != nil {
				t.Fatal(err)
			}
			limits := core.TelemetryLimits{MediaStaleAfter: 5 * time.Second, Events: core.EventLimits{MaxEvents: 16, MaxPageSize: 16, MaxBytes: 1 << 20, MaxEventBytes: 16384}, MaxGPUDevices: 1, MaxGPUUUIDBytes: 64, MaxGPUBytes: 4096}
			if err := f.config.Core.ConfigureTelemetry(ctx, limits); err != nil {
				t.Fatal(err)
			}
			observer, err := f.config.Core.OpenSessionObserver(ctx, f.config.Parent)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = observer.Close(context.Background()) })
			groupObserver, err := observer.OpenGroup(ctx, f.snapshot.Group.ID, f.snapshot.Profile.ID)
			if err != nil {
				t.Fatal(err)
			}
			worker, err := groupObserver.OpenFFmpegRole(ctx, core.FFmpegFallbackPlayer)
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := worker.BeginAttempt(ctx, 37)
			if err != nil {
				t.Fatal(err)
			}
			if err = attempt.Observe(ctx, core.FFmpegObservation{State: core.ObservationStarting}); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			sink, err := logging.New(logging.Config{MaxRecords: 16, MaxBytes: 1 << 20, MaxRecordBytes: 65536, ShutdownTimeout: time.Second, Level: slog.LevelInfo}, &output)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sink.Close(context.Background()) })
			g := &Group{config: f.config.Runtime, parent: f.config.Parent, id: f.snapshot.Group.ID, observer: groupObserver}
			g.config.Logger = sink.Logger().With("stream_id", "synthetic-stream")
			var binding atomic.Pointer[attemptBinding]
			binding.Store(bindFFmpegAttempt(3, 37, attempt, g.ffmpegLogger(worker, core.FFmpegFallbackPlayer)))
			anchor, err := f.config.Core.RuntimeAnchor(ctx)
			if err != nil {
				t.Fatal(err)
			}
			native := errors.New("native synthetic-native-token=full/+?\"\ncomplete")
			cleanup := errors.New("cleanup synthetic-cleanup-token=full/+?\"\ncomplete")
			g.observeAttemptResult(&binding)(ffmpeg.AttemptResult{ID: 3, Err: native, CleanupErr: cleanup, Stopped: stopped})
			snapshot, err := f.config.Core.RuntimeSnapshot(ctx, observer.SessionFence())
			if err != nil {
				t.Fatal(err)
			}
			var value *core.FFmpegObservation
			for _, item := range snapshot.FFmpeg {
				if item.Stamp.Token == worker.Token() && item.Stamp.AttemptID == 37 {
					value = item.Value
				}
			}
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
			page, err := f.config.Core.RuntimeEvents(ctx, anchor.Cursor, 16)
			if err != nil || page.Gap || page.Reset || len(page.Events) != 1 {
				t.Fatal("событие результата потеряно", page, err)
			}
			event := page.Events[0]
			if event.Kind != core.RuntimeStateEvent || event.Owner != core.RuntimeFFmpegOwner || event.Session != observer.SessionFence() || event.Stamp.Token != worker.Token() || event.Stamp.AttemptID != 37 || event.GroupInstanceID != groupObserver.Token() || event.Role != core.FFmpegFallbackPlayer || event.State != expectedState || event.Reason != core.ReasonObservationFailed {
				t.Fatal("событие имеет неверную принадлежность или причину", event)
			}
			encoded, err := json.Marshal(event)
			if err != nil || strings.Contains(string(encoded), "synthetic-native-token") || strings.Contains(string(encoded), "synthetic-cleanup-token") {
				t.Fatal("оперативное событие содержит произвольную ошибку", err)
			}
			if err = sink.Close(ctx); err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(&output)
			resultRecords := 0
			for {
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
