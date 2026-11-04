package storage

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func activateRuntimeSource(t *testing.T, f domainFixture) *core.RuntimeLease {
	t.Helper()
	lease, err := f.service.ActivateOBS(context.Background(), f.stream.ID, f.stream.PublicationKey, func(*core.RuntimeLease) (func(), error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func requireRevokedLease(t *testing.T, lease *core.RuntimeLease) {
	t.Helper()
	if lease.Valid() {
		t.Fatal("допуск остался действительным")
	}
	select {
	case <-lease.Done():
	default:
		t.Fatal("отзыв не закрыл Done")
	}
	called := false
	err := lease.Do(context.Background(), func() error { called = true; return nil })
	if called || (!errors.Is(err, core.ErrStale) && !errors.Is(err, core.ErrClosed)) {
		t.Fatalf("отозванный допуск выполнил действие: %v, %v", called, err)
	}
}

func TestRuntimeAuthoritySharedAcrossServicesAndDirectWrites(t *testing.T) {
	f := newDomainFixture(t, 0)
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	ctx := context.Background()
	other, err := core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	var installed core.ID
	var revoked atomic.Int32
	lease, err := f.service.ActivateOBS(ctx, f.stream.ID, f.stream.PublicationKey, func(lease *core.RuntimeLease) (func(), error) {
		if f.store.sqlDB.Stats().InUse != 0 {
			t.Fatal("установка выполняется внутри транзакции")
		}
		installed = lease.ID()
		return func() {
			if f.store.sqlDB.Stats().InUse != 0 {
				t.Error("отзыв выполняется внутри транзакции")
			}
			installed = ""
			revoked.Add(1)
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.PauseSession(ctx, core.SessionCommand{RequestID: "pause-authority", SessionID: start.SessionID}); err != nil {
		t.Fatal(err)
	}
	if !lease.Valid() || installed != lease.ID() {
		t.Fatal("пауза отозвала источник")
	}
	if _, err := other.StopSession(ctx, core.SessionCommand{RequestID: "stop-authority", SessionID: start.SessionID}); err != nil {
		t.Fatal(err)
	}
	requireRevokedLease(t, lease)
	if installed != "" || revoked.Load() != 1 {
		t.Fatal("запись другого Service не отозвала источник до возврата")
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	if revoked.Load() != 1 {
		t.Fatal("Close повторно вызвал callback отзыва")
	}
}

func TestRuntimeDirectWriteRollbackKeepsSourceAndCommitRevokes(t *testing.T) {
	f := newDomainFixture(t, 0)
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	lease := activateRuntimeSource(t, f)
	ctx := context.Background()
	mutate := func(r core.Repositories) error {
		session, err := r.Runtime.Session(ctx, start.SessionID)
		if err != nil {
			return err
		}
		old := session.Revision
		session.Lifecycle++
		session.Revision++
		return r.Runtime.UpdateSession(ctx, session, old)
	}
	err := f.store.Write(ctx, func(r core.Repositories) error {
		if err := mutate(r); err != nil {
			return err
		}
		return core.ErrInvalid
	})
	requireDomainError(t, err, core.ErrInvalid)
	if !lease.Valid() || readSession(t, f.store, start.SessionID).Lifecycle != lease.SessionFence().Lifecycle {
		t.Fatal("подтверждённый откат отозвал неизменённый источник")
	}
	if err = f.store.Write(ctx, mutate); err != nil {
		t.Fatal(err)
	}
	requireRevokedLease(t, lease)
}

func TestRuntimeActivationReplacementAndFailedInstall(t *testing.T) {
	f := newDomainFixture(t, 0)
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	ctx := context.Background()
	var current core.ID
	install := func(lease *core.RuntimeLease) (func(), error) {
		current = lease.ID()
		return func() {
			if current == lease.ID() {
				current = ""
			}
		}, nil
	}
	old, err := f.service.ActivateOBS(ctx, f.stream.ID, f.stream.PublicationKey, install)
	if err != nil {
		t.Fatal(err)
	}
	failed := errors.New("синтетический отказ установки")
	_, err = f.service.ActivateOBS(ctx, f.stream.ID, f.stream.PublicationKey, func(*core.RuntimeLease) (func(), error) { return nil, failed })
	if !errors.Is(err, failed) || !old.Valid() || current != old.ID() {
		t.Fatal("неудачная установка изменила прежний источник")
	}
	stale := old.SessionFence()
	stale.Lifecycle++
	_, err = f.service.ActivateOBSFor(ctx, stale, f.stream.ID, f.stream.PublicationKey, install)
	requireDomainError(t, err, core.ErrStale)
	if current != old.ID() || !old.Valid() {
		t.Fatal("устаревшая авторизация заменила источник")
	}
	newLease, err := f.service.ActivateOBSFor(ctx, old.SessionFence(), f.stream.ID, f.stream.PublicationKey, install)
	if err != nil {
		t.Fatal(err)
	}
	requireRevokedLease(t, old)
	if current != newLease.ID() || newLease.ID() == old.ID() {
		t.Fatal("Override потерял новый источник или переиспользовал идентификатор")
	}
	if err = old.Revoke(ctx); err != nil {
		t.Fatal(err)
	}
	if !newLease.Valid() || current != newLease.ID() {
		t.Fatal("поздний отзыв прежнего источника повредил новый")
	}
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	requireRevokedLease(t, newLease)
	if current != "" {
		t.Fatal("Close оставил установленный источник")
	}
}

func TestRuntimeOwnerRequiresIssuanceAndSurvivesCompletionAndOBSOverride(t *testing.T) {
	f := newDomainFixture(t, 2)
	start := startDomainSession(t, f)
	ctx := context.Background()
	fence, err := f.service.Claim(ctx, start.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.BindExecutionOwner(ctx, fence)
	requireDomainError(t, err, core.ErrStale)
	if err = f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	owner, err := f.service.BindExecutionOwner(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	if !owner.Valid() {
		t.Fatal("успешное Complete отозвало физического владельца")
	}
	duplicate, err := f.service.BindExecution(ctx, fence)
	if err != nil || duplicate != owner {
		t.Fatalf("один ExecutionFence получил несколько независимых допусков: %v", err)
	}
	oldOBS := activateRuntimeSource(t, f)
	newOBS := activateRuntimeSource(t, f)
	requireRevokedLease(t, oldOBS)
	if !owner.Valid() || !newOBS.Valid() {
		t.Fatal("Override отозвал независимого владельца Session")
	}
	outputs := readOutputs(t, f.store, start.SessionID)
	leases := make([]*core.RuntimeLease, len(outputs))
	for i, output := range outputs {
		command := outputStartReceipt(t, f, output.ID)
		outputFence := executeCommand(t, f.service, command.RequestID)
		leases[i], err = f.service.BindExecutionOwner(ctx, outputFence)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = f.service.PauseSession(ctx, core.SessionCommand{RequestID: "owner-pause", SessionID: start.SessionID}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.StopOutput(ctx, core.OutputCommand{RequestID: "owner-stop-output", OutputID: outputs[0].ID}); err != nil {
		t.Fatal(err)
	}
	requireRevokedLease(t, leases[0])
	if !leases[1].Valid() || !owner.Valid() || !newOBS.Valid() {
		t.Fatal("команда соседа или пауза отозвала независимый допуск")
	}
	if _, err = f.service.StopSession(ctx, core.SessionCommand{RequestID: "owner-stop-session", SessionID: start.SessionID}); err != nil {
		t.Fatal(err)
	}
	for _, lease := range []*core.RuntimeLease{leases[1], owner, newOBS} {
		requireRevokedLease(t, lease)
	}
	if len(pendingCleanups(t, f.store, start.SessionID)) != 3 {
		t.Fatal("привязка исполнения подтвердила очистку без физического доказательства")
	}
}

func TestRuntimeAuthorityWaitHonorsWriteBudget(t *testing.T) {
	for _, callerBudget := range []bool{true, false} {
		t.Run(map[bool]string{true: "caller", false: "store"}[callerBudget], func(t *testing.T) {
			testRuntimeAuthorityWaitBudget(t, callerBudget)
		})
	}
}

func testRuntimeAuthorityWaitBudget(t *testing.T, callerBudget bool) {
	t.Helper()
	f := newDomainFixture(t, 0)
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	lease := activateRuntimeSource(t, f)
	// Детерминированно удерживаем authority, чтобы проверить отмену ожидающей записи.
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	holder, cancelHolder := context.WithTimeout(context.Background(), 2*time.Second)
	done := make(chan struct{})
	t.Cleanup(func() { cancelHolder(); <-done })
	go func() {
		defer close(done)
		finished <- lease.Do(holder, func() error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-holder.Done():
				return holder.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-holder.Done():
		t.Fatal("проверка не смогла удержать authority")
	}
	budget := 20 * time.Millisecond
	if callerBudget {
		f.store.options.RetryTimeout = time.Second
	} else {
		f.store.options.RetryTimeout = budget
		budget = time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	called := false
	err := f.store.Write(ctx, func(core.Repositories) error { called = true; return nil })
	close(release)
	if heldErr := <-finished; heldErr != nil {
		t.Fatal(heldErr)
	}
	if called || !errors.Is(err, context.DeadlineExceeded) || !lease.Valid() {
		t.Fatalf("ожидание authority нарушило бюджет или отозвало источник: %v, %v", called, err)
	}
}

func TestRuntimeFailedIssuedStartupRevokesOwnerWithoutCleanupAcknowledgement(t *testing.T) {
	f := newDomainFixture(t, 0)
	start := startDomainSession(t, f)
	ctx := context.Background()
	fence, err := f.service.Claim(ctx, start.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	owner, err := f.service.BindExecutionOwner(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.Complete(ctx, fence, core.Completion{Failure: "синтетический отказ запуска"}); err != nil {
		t.Fatal(err)
	}
	requireRevokedLease(t, owner)
	if session := readSession(t, f.store, start.SessionID); session.State != core.SessionFailed || !session.CleanupPending || !session.Issued {
		t.Fatal("отказ запуска выдумал физическую очистку")
	}
	if len(pendingCleanups(t, f.store, start.SessionID)) != 1 {
		t.Fatal("отказ запуска потерял ожидающую очистку")
	}
}

func TestRuntimeServiceCannotUseAnotherIncarnation(t *testing.T) {
	f := newDomainFixture(t, 0)
	start := startDomainSession(t, f)
	fence := executeCommand(t, f.service, start.RequestID)
	foreign, err := core.NewService(f.store, "another-incarnation")
	if err != nil {
		t.Fatal(err)
	}
	_, err = foreign.BindExecutionOwner(context.Background(), fence)
	requireDomainError(t, err, core.ErrStale)
	_, err = foreign.ActivateOBS(context.Background(), f.stream.ID, f.stream.PublicationKey, func(*core.RuntimeLease) (func(), error) { return nil, nil })
	requireDomainError(t, err, core.ErrStale)
	if _, err := foreign.AuthorizeOBS(context.Background(), f.stream.ID, f.stream.PublicationKey); err != nil {
		t.Fatalf("старый API снимка изменил смысл чтения: %v", err)
	}
}

func TestRuntimeActivationCannotCrossSessionDuringNetworkAccept(t *testing.T) {
	f := newDomainFixture(t, 0)
	first := startDomainSession(t, f)
	executeCommand(t, f.service, first.RequestID)
	ctx := context.Background()
	initial, err := f.service.AuthorizeOBS(ctx, f.stream.ID, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	expected := core.RuntimeSessionFence{SessionID: initial.ID, Incarnation: initial.Incarnation, Lifecycle: initial.Lifecycle}
	stop, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "first-session-stop", SessionID: first.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	executeCommand(t, f.service, stop.RequestID)
	for _, cleanup := range pendingCleanups(t, f.store, first.SessionID) {
		if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanup)); err != nil {
			t.Fatal(err)
		}
	}
	second, err := f.service.StartSession(ctx, f.startRequest("second-session-start"))
	if err != nil {
		t.Fatal(err)
	}
	executeCommand(t, f.service, second.RequestID)
	current := activateRuntimeSource(t, f)
	called := false
	_, err = f.service.ActivateOBSFor(ctx, expected, f.stream.ID, f.stream.PublicationKey, func(*core.RuntimeLease) (func(), error) { called = true; return nil, nil })
	if !errors.Is(err, core.ErrStale) || called || !current.Valid() || current.SessionFence().SessionID != second.SessionID {
		t.Fatalf("авторизация прежней Session пересекла сетевое окно: %v, %v", called, err)
	}
}
