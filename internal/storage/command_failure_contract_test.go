package storage

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func controlTableCounts(t *testing.T, store *Store) map[string]int {
	t.Helper()
	counts := make(map[string]int)
	for _, table := range []string{"sessions", "outputs", "command_receipts", "events", "session_holds", "output_holds", "cleanup_intents"} {
		var count int
		if err := store.sqlDB.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts[table] = count
	}
	return counts
}

func TestCompletionRequiresIssuanceAndFailureReasonWithoutPartialWrites(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	fence, err := f.service.Claim(ctx, start.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	session := readSession(t, f.store, start.SessionID)
	outputs := readOutputs(t, f.store, start.SessionID)
	receipt := readReceipt(t, f.store, start.RequestID)
	counts := controlTableCounts(t, f.store)
	for _, result := range []struct {
		completion core.Completion
		err        error
	}{{core.Completion{Success: true}, core.ErrStale}, {core.Completion{}, core.ErrInvalid}} {
		requireDomainError(t, f.service.Complete(ctx, fence, result.completion), result.err)
		if !reflect.DeepEqual(session, readSession(t, f.store, start.SessionID)) || !reflect.DeepEqual(outputs, readOutputs(t, f.store, start.SessionID)) || !reflect.DeepEqual(receipt, readReceipt(t, f.store, start.RequestID)) || !reflect.DeepEqual(counts, controlTableCounts(t, f.store)) {
			t.Fatal("отклонённое завершение изменило принятую работу")
		}
	}
	requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
	if err := f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	receipt = readReceipt(t, f.store, start.RequestID)
	session = readSession(t, f.store, start.SessionID)
	counts = controlTableCounts(t, f.store)
	requireDomainError(t, f.service.Complete(ctx, fence, core.Completion{}), core.ErrInvalid)
	if !reflect.DeepEqual(receipt, readReceipt(t, f.store, start.RequestID)) || !reflect.DeepEqual(session, readSession(t, f.store, start.SessionID)) || !reflect.DeepEqual(outputs, readOutputs(t, f.store, start.SessionID)) || !reflect.DeepEqual(counts, controlTableCounts(t, f.store)) {
		t.Fatal("отказ без причины завершил выданную команду")
	}
	if err := f.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
}

func TestFailedSessionStopSettlesEveryPendingStopAndRetainsCleanupHolds(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	output := readOutputs(t, f.store, start.SessionID)[0]
	executeCommand(t, f.service, outputStartReceipt(t, f, output.ID).RequestID)
	stops := make([]core.CommandReceipt, 3)
	for i := range stops {
		var err error
		stops[i], err = f.service.StopSession(ctx, core.SessionCommand{RequestID: fmt.Sprintf("stop-request-%d", i), SessionID: start.SessionID})
		if err != nil {
			t.Fatal(err)
		}
	}
	cleanups := pendingCleanups(t, f.store, start.SessionID)
	fence, err := f.service.Claim(ctx, stops[1].RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	const reason = "процесс не подтвердил остановку"
	if err := f.service.Complete(ctx, fence, core.Completion{Failure: reason}); err != nil {
		t.Fatal(err)
	}
	session := readSession(t, f.store, start.SessionID)
	if session.State != core.SessionFailed || session.Failure != reason || session.EndedAt == nil || !session.CleanupPending {
		t.Fatalf("отказ stop потерян: %+v", session)
	}
	for _, stop := range stops {
		got := readReceipt(t, f.store, stop.RequestID)
		if got.Status != core.CommandFailed || got.Failure != reason {
			t.Fatalf("результат stop не распространился на %s: %+v", stop.RequestID, got)
		}
		replayed, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: stop.RequestID, SessionID: start.SessionID})
		if err != nil || !reflect.DeepEqual(got, replayed) {
			t.Fatalf("повтор stop потерял отказ: %+v, %v", replayed, err)
		}
	}
	if !reflect.DeepEqual(cleanups, pendingCleanups(t, f.store, start.SessionID)) || len(cleanups) != 2 {
		t.Fatal("отказ stop освободил или продублировал cleanup")
	}
	requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
	for _, intent := range cleanups {
		if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(intent)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision); err != nil {
		t.Fatalf("подтверждённая очистка не освободила поколение: %v", err)
	}
}

func TestStartSessionLateAcceptanceFailureRollsBackAllControlState(t *testing.T) {
	f := newDomainFixture(t, 2)
	ctx := context.Background()
	_, err := f.store.sqlDB.ExecContext(ctx, `CREATE TRIGGER reject_session_acceptance BEFORE INSERT ON events WHEN NEW.output_id IS NULL BEGIN SELECT RAISE(ABORT, 'synthetic acceptance failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.StartSession(ctx, f.startRequest("atomic-start"))
	if err == nil {
		t.Fatal("инъекция отказа не отклонила принятие")
	}
	for _, table := range []string{"sessions", "outputs", "command_receipts", "events", "session_holds", "output_holds", "cleanup_intents"} {
		var count int
		if err := f.store.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("отказ принятия оставил %s: %d, %v", table, count, err)
		}
	}
	_, err = f.service.FindReceipt(ctx, "atomic-start")
	requireDomainError(t, err, core.ErrNotFound)
	if _, err := f.store.sqlDB.ExecContext(ctx, "DROP TRIGGER reject_session_acceptance"); err != nil {
		t.Fatal(err)
	}
	accepted, err := f.service.StartSession(ctx, f.startRequest("atomic-start"))
	if err != nil || len(readOutputs(t, f.store, accepted.SessionID)) != 2 {
		t.Fatalf("откат не позволил повторить исходный запрос: %+v, %v", accepted, err)
	}
}

func TestExplicitStartSelectionRejectsUnknownDestinationAtomically(t *testing.T) {
	f := newDomainFixture(t, 2)
	ctx := context.Background()
	request := f.startRequest("selected-start")
	request.DestinationIDs = []core.ID{f.destinations[0].ID, "missing-destination"}
	_, err := f.service.StartSession(ctx, request)
	requireDomainError(t, err, core.ErrNotFound)
	for _, table := range []string{"sessions", "outputs", "command_receipts", "events", "session_holds", "output_holds"} {
		var count int
		if err := f.store.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("неполный выбор сохранил %s: %d, %v", table, count, err)
		}
	}
	request.DestinationIDs = []core.ID{f.destinations[1].ID}
	accepted, err := f.service.StartSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	outputs := readOutputs(t, f.store, accepted.SessionID)
	if len(outputs) != 1 || outputs[0].Snapshot.Destination.ID != f.destinations[1].ID {
		t.Fatalf("запущено другое подмножество назначений: %+v", outputs)
	}
}
