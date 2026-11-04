package storage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

func TestAcceptanceConcurrentPublicationAndCrossServiceStop(t *testing.T) {
	f := newDomainFixture(t, 0)
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	other, err := core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	initial, err := f.service.AuthorizeOBS(ctx, f.stream.ID, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	expected := core.RuntimeSessionFence{SessionID: initial.ID, Incarnation: initial.Incarnation, Lifecycle: initial.Lifecycle}
	const publishers = 12
	gate := make(chan struct{})
	leases := make(chan *core.RuntimeLease, publishers)
	errorsCh := make(chan error, publishers+1)
	var wg sync.WaitGroup
	var current core.ID
	installed, revoked := 0, 0
	install := func(l *core.RuntimeLease) (func(), error) {
		installed++
		current = l.ID()
		return func() {
			revoked++
			if current == l.ID() {
				current = ""
			}
		}, nil
	}
	for range publishers {
		wg.Go(func() {
			<-gate
			lease, err := f.service.ActivateOBSFor(ctx, expected, f.stream.ID, f.stream.PublicationKey, install)
			if err == nil {
				leases <- lease
			} else if !errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrNotReady) && !errors.Is(err, core.ErrStale) {
				errorsCh <- err
			}
		})
	}
	wg.Go(func() {
		<-gate
		_, err := other.StopSession(ctx, core.SessionCommand{RequestID: "concurrent-stop", SessionID: start.SessionID})
		if err != nil {
			errorsCh <- err
		}
	})
	close(gate)
	wg.Wait()
	close(errorsCh)
	close(leases)
	for err := range errorsCh {
		t.Error(err)
	}
	for lease := range leases {
		if lease.Valid() {
			t.Error("stop вернулся с действующим допуском OBS")
		}
		select {
		case <-lease.Done():
		default:
			t.Error("отозванный источник не завершил Done")
		}
		if err := lease.Do(ctx, func() error { return errors.New("отозванное действие выполнено") }); !errors.Is(err, core.ErrStale) {
			t.Errorf("отозванный допуск разрешил действие: %v", err)
		}
	}
	if installed != revoked || current != "" {
		t.Fatalf("не все установки отозваны: установлено=%d отозвано=%d current=%q", installed, revoked, current)
	}
	if got := readSession(t, f.store, start.SessionID); got.State != core.SessionStopping || !got.CleanupPending {
		t.Fatalf("stop потерял сохранённое обязательство очистки: %+v", got)
	}
}

func TestAcceptancePersistedCleanupWaitsForSupervisorJoinProof(t *testing.T) {
	f := newDomainFixture(t, 0)
	start := startDomainSession(t, f)
	fence := executeCommand(t, f.service, start.RequestID)
	owner, err := f.service.BindExecutionOwner(context.Background(), fence)
	if err != nil {
		t.Fatal(err)
	}
	parent := os.TempDir()
	if runtime.GOOS == "darwin" {
		// Системный TMPDIR macOS слишком длинный для вложенных путей UDS.
		parent = "/tmp"
	}
	base, err := os.MkdirTemp(parent, "gc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	s, err := ffmpeg.New(ffmpeg.Config{BaseDir: base, StartupTimeout: 5 * time.Second, VideoTimeout: time.Second, AudioTimeout: time.Second, WatchdogInterval: 10 * time.Millisecond, MaxRestarts: 1, RestartBackoff: 10 * time.Millisecond, GracePeriod: 20 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: time.Second, DiagnosticEvents: 16, DiagnosticBytes: 8192, DiagnosticChunkBytes: 64, MaxCommandBytes: 4096, MaxCommandArgs: 16})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	handler := func(ctx context.Context, _ net.Conn, _ ffmpeg.Progress) error { <-ctx.Done(); return ctx.Err() }
	if err := s.Start(context.Background(), ffmpeg.Worker{SessionID: string(start.SessionID), GroupID: "group", Owner: owner, Command: func(context.Context, ffmpeg.Attempt) (ffmpeg.Command, error) {
		close(entered)
		<-release
		return ffmpeg.Command{Binary: "/does-not-exist"}, nil
	}, Input: handler, Output: handler}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-s.Done():
		result, err := s.Wait(context.Background())
		t.Fatalf("worker завершился до builder: %+v, %v", result, err)
	case <-time.After(time.Second):
		t.Fatal("зарегистрированный builder не начался")
	}
	other, err := core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	stop, err := other.StopSession(context.Background(), core.SessionCommand{RequestID: "stop-registered-start", SessionID: start.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	executeCommand(t, other, stop.RequestID)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Stop(cancelled); !errors.Is(err, ffmpeg.ErrCleanupPending) {
		t.Fatal("незавершённый start выдан за join:", err)
	}
	if pending := pendingCleanups(t, f.store, start.SessionID); len(pending) != 1 {
		t.Fatalf("до join потеряно обязательство очистки: %+v", pending)
	}
	releaseOnce.Do(func() { close(release) })
	ctx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer waitCancel()
	result, err := s.Wait(ctx)
	if err != nil || !result.Clean || result.CleanupErr != nil {
		t.Fatalf("join не доказан: %+v %v", result, err)
	}
	pending := pendingCleanups(t, f.store, start.SessionID)
	if len(pending) != 1 {
		t.Fatal("Supervisor автоматически подтвердил сохранённую очистку")
	}
	if err := other.AcknowledgeCleanup(ctx, core.CleanupFenceFor(pending[0])); err != nil {
		t.Fatal(err)
	}
	if len(pendingCleanups(t, f.store, start.SessionID)) != 0 {
		t.Fatal("явное подтверждение после join не завершило очистку")
	}
}

func TestAcceptanceInvalidAdmissionPreservesCurrentPublication(t *testing.T) {
	f := newDomainFixture(t, 0)
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	ctx := context.Background()
	current := activateRuntimeSource(t, f)
	for _, key := range []string{"", "wrong-synthetic-key"} {
		called := false
		if _, err := f.service.ActivateOBSFor(ctx, current.SessionFence(), f.stream.ID, key, func(*core.RuntimeLease) (func(), error) { called = true; return nil, nil }); err == nil || called || !current.Valid() {
			t.Fatalf("ошибочный ключ изменил публикацию: called=%v valid=%v err=%v", called, current.Valid(), err)
		}
	}
	ctxCancelled, cancel := context.WithCancel(ctx)
	cancel()
	called := false
	if _, err := f.service.ActivateOBSFor(ctxCancelled, current.SessionFence(), f.stream.ID, f.stream.PublicationKey, func(*core.RuntimeLease) (func(), error) { called = true; return nil, nil }); !errors.Is(err, context.Canceled) || called || !current.Valid() {
		t.Fatalf("отменённый Accept изменил публикацию: %v", err)
	}
	if err := f.store.Write(ctx, func(core.Repositories) error {
		return fmt.Errorf("синтетический rollback: %w", core.ErrInvalid)
	}); !errors.Is(err, core.ErrInvalid) || !current.Valid() {
		t.Fatalf("откат CRUD отозвал действующий источник: %v", err)
	}
}

func TestAcceptanceOutputFailureRevokesOnlyItsOwner(t *testing.T) {
	f := newDomainFixture(t, 2)
	start := startDomainSession(t, f)
	parentFence := executeCommand(t, f.service, start.RequestID)
	ctx := context.Background()
	parent, err := f.service.BindExecutionOwner(ctx, parentFence)
	if err != nil {
		t.Fatal(err)
	}
	publication := activateRuntimeSource(t, f)
	outputs := readOutputs(t, f.store, start.SessionID)
	fences := make([]core.ExecutionFence, 2)
	owners := make([]*core.RuntimeLease, 2)
	for i, output := range outputs {
		command := outputStartReceipt(t, f, output.ID)
		fences[i] = executeCommand(t, f.service, command.RequestID)
		owners[i], err = f.service.BindExecutionOwner(ctx, fences[i])
		if err != nil {
			t.Fatal(err)
		}
	}
	other, err := core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	observers := []*core.OutputObserver{openOutputObserver(t, other, fences[0]), openOutputObserver(t, other, fences[1])}
	if err := other.FailOutput(ctx, observers[0], "синтетический отказ"); err != nil {
		t.Fatal(err)
	}
	if owners[0].Valid() || !owners[1].Valid() || !parent.Valid() || !publication.Valid() {
		t.Fatal("частичный отказ нарушил независимость runtime-владельцев")
	}
	if len(pendingCleanups(t, f.store, start.SessionID)) != 1 {
		t.Fatal("после отказа выданной попытки требуется одно сохранённое обязательство очистки")
	}
	if err := observers[1].Observe(ctx, core.OutputObservation{State: core.OutputRetrying, Error: "повтор соединения"}); err != nil {
		t.Fatal(err)
	}
	if !owners[1].Valid() || !parent.Valid() || !publication.Valid() {
		t.Fatal("наблюдение состояния соседа отозвало действующие допуски")
	}
}
