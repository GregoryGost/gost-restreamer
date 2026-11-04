package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func reopenFixture(t *testing.T, f domainFixture) domainFixture {
	t.Helper()
	var sequence int
	var name, dbPath string
	if err := f.store.sqlDB.QueryRowContext(context.Background(), "PRAGMA database_list").Scan(&sequence, &name, &dbPath); err != nil {
		t.Fatal(err)
	}
	if dbPath == "" || name != "main" {
		t.Fatalf("не удалось определить временную файловую БД: %q, %q", name, dbPath)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), dbPath, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	svc, err := core.NewService(s, s.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	f.store, f.service = s, svc
	return f
}

func readReceipt(t *testing.T, s *Store, requestID string) core.CommandReceipt {
	t.Helper()
	var c core.CommandReceipt
	if err := s.View(context.Background(), func(r core.Repositories) error {
		var err error
		c, err = r.Runtime.Receipt(context.Background(), requestID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRestartInterruptsUnissuedWorkWithoutReplay(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	pending, err := f.service.ReserveAsset(ctx, core.FallbackAsset{Name: "Незавершённое изображение", Kind: core.AssetImage, OriginalPath: "originals/pending.png"})
	if err != nil {
		t.Fatal(err)
	}
	f = reopenFixture(t, f)
	s := readSession(t, f.store, start.SessionID)
	if s.State != core.SessionInterrupted || s.CleanupPending || s.EndedAt == nil {
		t.Fatalf("невыданный запуск после restart: %+v", s)
	}
	if c := readReceipt(t, f.store, start.RequestID); c.Status != core.CommandInterrupted {
		t.Fatalf("pending-команда не прервана: %+v", c)
	}
	for _, o := range readOutputs(t, f.store, s.ID) {
		if o.Issued || o.CleanupPending || o.DesiredRunning {
			t.Fatalf("restart повторил выход: %+v", o)
		}
	}
	if _, err := f.service.Claim(ctx, start.RequestID); !errors.Is(err, core.ErrStale) {
		t.Fatalf("прерванная команда доступна для replay: %v", err)
	}
	_, err = f.service.PublishAsset(ctx, pending.ID, pending.Revision, pending.AttemptToken, "late-hash", 10, core.SourceFormat{ImageFormat: "png"})
	requireDomainError(t, err, core.ErrInvalid)
	if err := f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if after := readSession(t, f.store, s.ID); !reflect.DeepEqual(s, after) {
		t.Fatal("повтор recovery переписал прерванную историю")
	}
	newStart, err := f.service.StartSession(ctx, f.startRequest("manual-after-restart"))
	if err != nil || newStart.SessionID == s.ID {
		t.Fatalf("новый ручной запуск после невыданной попытки: %+v, %v", newStart, err)
	}
	before := readSession(t, f.store, newStart.SessionID)
	if err := f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if after := readSession(t, f.store, newStart.SessionID); !reflect.DeepEqual(before, after) {
		t.Fatal("recovery текущего владельца прервал живой запуск")
	}
}

func TestIssuedWorkKeepsHoldsUntilCorrectCleanupAfterRestart(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	completed := readReceipt(t, f.store, start.RequestID)
	o := readOutputs(t, f.store, start.SessionID)[0]
	outputReceipt := outputStartReceipt(t, f, o.ID)
	oldFence, err := f.service.Claim(ctx, outputReceipt.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.MarkIssued(ctx, oldFence); err != nil {
		t.Fatal(err)
	}
	f = reopenFixture(t, f)
	session := readSession(t, f.store, start.SessionID)
	if session.State != core.SessionInterrupted || !session.CleanupPending {
		t.Fatalf("потеря владельца освободила выданные эффекты: %+v", session)
	}
	if got := readReceipt(t, f.store, start.RequestID); !reflect.DeepEqual(got, completed) {
		t.Fatal("recovery переписал завершённую квитанцию")
	}
	requireDomainError(t, f.service.Complete(ctx, oldFence, core.Completion{Success: true}), core.ErrStale)
	if _, err := f.service.StartSession(ctx, f.startRequest("blocked-before-cleanup")); !errors.Is(err, core.ErrNotReady) && !errors.Is(err, core.ErrInUse) {
		t.Fatalf("новый start до cleanup должен отклоняться: %v", err)
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, true)
	requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
	cleanups := pendingCleanups(t, f.store, session.ID)
	if len(cleanups) != 2 {
		t.Fatalf("нужны отдельные session/output cleanup: %+v", cleanups)
	}
	wrong := core.CleanupFenceFor(cleanups[0])
	wrong.Attempt++
	requireDomainError(t, f.service.AcknowledgeCleanup(ctx, wrong), core.ErrStale)
	if len(pendingCleanups(t, f.store, session.ID)) != 2 {
		t.Fatal("неверный ack освободил cleanup")
	}
	for _, cleanup := range cleanups {
		if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanup)); err != nil {
			t.Fatal(err)
		}
	}
	if len(pendingCleanups(t, f.store, session.ID)) != 0 {
		t.Fatal("подтверждённая очистка осталась pending")
	}
	newStart, err := f.service.StartSession(ctx, f.startRequest("manual-after-cleanup"))
	if err != nil || newStart.SessionID == session.ID {
		t.Fatalf("ручной start после cleanup: %+v, %v", newStart, err)
	}
	for _, cleanup := range cleanups {
		if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanup)); err != nil {
			t.Fatal(err)
		}
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, true)
	requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
	if current := readSession(t, f.store, newStart.SessionID); current.State != core.SessionStarting {
		t.Fatal("старый ack изменил новый запуск")
	}
}

func TestIssuedSessionStartupKeepsPinnedVariantUntilCleanup(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	fence, err := f.service.Claim(ctx, start.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	f = reopenFixture(t, f)
	if session := readSession(t, f.store, start.SessionID); session.State != core.SessionInterrupted || !session.CleanupPending {
		t.Fatalf("не сохранена очистка выданного запуска: %+v", session)
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, true)
	requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
	cleanups := pendingCleanups(t, f.store, start.SessionID)
	if len(cleanups) != 1 || cleanups[0].OutputID != "" {
		t.Fatalf("нужна очистка выданного запуска Session: %+v", cleanups)
	}
	if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
		t.Fatal(err)
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, false)
}

func TestStoppedAttachedOutputRestartsSavedSnapshotAndDetachReleasesOnlyOwnHold(t *testing.T) {
	f := newDomainFixture(t, 2)
	ctx := context.Background()
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	outputs := readOutputs(t, f.store, start.SessionID)
	first := outputs[0]
	if _, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "stop-unissued-output", OutputID: first.ID}); err != nil {
		t.Fatal(err)
	}
	requireDomainError(t, f.service.DeleteDestination(ctx, first.Snapshot.Destination.ID, first.Snapshot.Destination.Revision), core.ErrInUse)
	d := first.Snapshot.Destination
	d.StreamKey = "edited-synthetic-key"
	updated, err := f.service.UpdateDestination(ctx, d, d.Revision)
	if err != nil {
		t.Fatal(err)
	}
	restart, err := f.service.StartOutput(ctx, core.OutputCommand{RequestID: "restart-attached", OutputID: first.ID})
	if err != nil {
		t.Fatal(err)
	}
	executeCommand(t, f.service, restart.RequestID)
	current := readOutputs(t, f.store, start.SessionID)
	if !reflect.DeepEqual(first.Snapshot, current[0].Snapshot) || !current[0].Issued || !current[0].DesiredRunning {
		t.Fatalf("повторный start применил новую конфигурацию: %+v", current[0])
	}
	if _, err := f.service.DetachOutput(ctx, core.OutputCommand{RequestID: "detach-issued-output", OutputID: first.ID}); err != nil {
		t.Fatal(err)
	}
	requireDomainError(t, f.service.DeleteDestination(ctx, d.ID, updated.Revision), core.ErrInUse)
	cleanups := pendingCleanups(t, f.store, start.SessionID)
	if len(cleanups) != 1 || cleanups[0].OutputID != first.ID {
		t.Fatalf("detach затронул чужие ресурсы: %+v", cleanups)
	}
	if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
		t.Fatal(err)
	}
	if err := f.service.DeleteDestination(ctx, d.ID, updated.Revision); err != nil {
		t.Fatalf("очищенный detached output держит текущую конфигурацию: %v", err)
	}
	_, err = f.service.StartOutput(ctx, core.OutputCommand{RequestID: "restart-detached", OutputID: first.ID})
	requireDomainError(t, err, core.ErrConflict)
	current = readOutputs(t, f.store, start.SessionID)
	if current[0].Attached || current[0].CleanupPending || current[0].Issued || current[0].DesiredRunning {
		t.Fatalf("detached output не завершён: %+v", current[0])
	}
	if !reflect.DeepEqual(outputs[1], current[1]) {
		t.Fatal("detach изменил соседний выход")
	}
	if s := readSession(t, f.store, start.SessionID); s.State != core.SessionRunning {
		t.Fatal("detach сам остановил Session")
	}
}

func TestHardDeletePreservesStandaloneSessionHistory(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	snapshot := readSession(t, f.store, start.SessionID).Snapshot
	outputSnapshot := readOutputs(t, f.store, start.SessionID)[0].Snapshot
	stop, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "stop-without-effects", SessionID: start.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	executeCommand(t, f.service, stop.RequestID)
	if err := f.service.DeleteDestination(ctx, f.destinations[0].ID, f.destinations[0].Revision); err != nil {
		t.Fatal(err)
	}
	if err := f.service.DeleteGroup(ctx, f.group.ID, f.group.Revision); err != nil {
		t.Fatal(err)
	}
	if err := f.service.DeleteProfile(ctx, f.profile.ID, f.profile.Revision); err != nil {
		t.Fatal(err)
	}
	if err := f.service.DeleteStream(ctx, f.stream.ID, f.stream.Revision); err != nil {
		t.Fatal(err)
	}
	if err := f.service.DeleteAsset(ctx, f.asset.ID, f.asset.Revision); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot, readSession(t, f.store, start.SessionID).Snapshot) || !reflect.DeepEqual(outputSnapshot, readOutputs(t, f.store, start.SessionID)[0].Snapshot) {
		t.Fatal("hard delete уничтожил самостоятельный снимок")
	}
	if err := f.store.View(ctx, func(r core.Repositories) error {
		_, err := r.Config.Stream(ctx, f.stream.ID)
		requireDomainError(t, err, core.ErrNotFound)
		events, err := r.Runtime.Events(ctx, core.ListQuery{Limit: core.MaxListLimit, SessionID: start.SessionID})
		if err != nil {
			return err
		}
		if len(events) < 2 {
			t.Fatal("hard delete уничтожил события")
		}
		cleanups, err := r.Artifacts.Cleanups(ctx, core.ListQuery{Limit: core.MaxListLimit, PendingOnly: true})
		if err != nil {
			return err
		}
		paths := map[string]bool{}
		for _, cleanup := range cleanups {
			if cleanup.Kind == core.CleanupFile {
				paths[cleanup.ManagedPath] = true
			}
		}
		if len(paths) != 5 || !paths[f.asset.OriginalPath] || !paths[f.variant.ManagedPath] {
			t.Fatalf("потеряны owned paths для cleanup: %+v", paths)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
