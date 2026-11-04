package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func observationRows(t *testing.T, db *sql.DB) map[string][][]any {
	t.Helper()
	result := map[string][][]any{}
	for _, table := range []string{"fallback_assets", "streams", "profiles", "groups", "destinations", "fallback_variants", "sessions", "outputs", "command_receipts", "cleanup_intents", "owned_paths", "session_holds", "output_holds", "events"} {
		rows, err := db.QueryContext(context.Background(), "SELECT * FROM "+table+" ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values, targets := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			for i, value := range values {
				if b, ok := value.([]byte); ok {
					values[i] = string(b)
				}
			}
			result[table] = append(result[table], values)
		}
		rowErr, closeErr := rows.Err(), rows.Close()
		if rowErr != nil || closeErr != nil {
			t.Fatal(rowErr, closeErr)
		}
	}
	return result
}

func startObservationOutputs(t *testing.T, f domainFixture) (core.ExecutionFence, []core.ExecutionFence) {
	t.Helper()
	start := startDomainSession(t, f)
	parent := executeCommand(t, f.service, start.RequestID)
	var outputs []core.ExecutionFence
	for _, o := range readOutputs(t, f.store, start.SessionID) {
		outputs = append(outputs, executeCommand(t, f.service, outputStartReceipt(t, f, o.ID).RequestID))
	}
	return parent, outputs
}

func observationFence(f core.ExecutionFence) core.RuntimeSessionFence {
	return core.RuntimeSessionFence{SessionID: f.SessionID, Incarnation: f.Incarnation, Lifecycle: f.Lifecycle}
}

func TestAcceptanceOrdinaryObservationsLeaveEveryDurableRowUnchanged(t *testing.T) {
	f := newDomainFixture(t, 2)
	parent, outputs := startObservationOutputs(t, f)
	ctx := context.Background()
	if _, err := f.service.RuntimeSnapshot(ctx, observationFence(parent)); !errors.Is(err, core.ErrNotReady) {
		t.Fatal("Complete выдумал RAM-наблюдение", err)
	}
	so, err := f.service.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	g, err := so.OpenGroup(ctx, "dynamic-group-outside-start-snapshot", f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	w, err := g.OpenFFmpeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a, err := w.BeginAttempt(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	var observers []*core.OutputObserver
	for _, fence := range outputs {
		o, err := f.service.OpenOutputObserver(ctx, fence)
		if err != nil {
			t.Fatal(err)
		}
		observers = append(observers, o)
	}
	before := observationRows(t, f.store.sqlDB)
	for i := range 3 {
		if err := so.Observe(ctx, core.SessionObservation{State: core.ObservationReady, Error: "временная ошибка Session"}); err != nil {
			t.Fatal(err)
		}
		if err := g.Observe(ctx, core.GroupObservation{State: core.ObservationReady, Source: core.ObservationSourceLive, Error: "временная ошибка Group"}); err != nil {
			t.Fatal(err)
		}
		if err := a.Observe(ctx, core.FFmpegObservation{State: core.ObservationRetrying, Error: "временная ошибка worker"}); err != nil {
			t.Fatal(err)
		}
		for _, o := range observers {
			state := core.OutputRetrying
			if i == 2 {
				state = core.OutputPublishing
			}
			if err := o.Observe(ctx, core.OutputObservation{State: state, Error: "временная ошибка соединения"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	after := observationRows(t, f.store.sqlDB)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("обычные наблюдения изменили долговечные строки, payload, revisions, events, receipts или holds")
	}
	snapshot, err := f.service.RuntimeSnapshot(ctx, observationFence(parent))
	if err != nil || snapshot.Session.Value.Error != "временная ошибка Session" || len(snapshot.Outputs) != 2 || snapshot.Outputs[0].Value.State != core.OutputPublishing || snapshot.FFmpeg[0].Stamp.Revision != 3 {
		t.Fatalf("нет текущего RAM-снимка: %+v %v", snapshot, err)
	}
	if readSession(t, f.store, parent.SessionID).Failure != "" {
		t.Fatal("текущая ошибка попала в итоговый Session.Failure")
	}
	var payload []byte
	if err := f.store.sqlDB.QueryRowContext(ctx, "SELECT payload FROM outputs WHERE id=?", outputs[0].OutputID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var envelope struct{ Payload map[string]json.RawMessage }
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"State", "Failure"} {
		if _, exists := envelope.Payload[field]; exists {
			t.Errorf("оперативное поле %s осталось в durable payload", field)
		}
	}
	if err := f.store.View(ctx, func(r core.Repositories) error {
		_, err := r.Runtime.Outputs(ctx, core.ListQuery{Limit: 10, SessionID: parent.SessionID, State: string(core.OutputPublishing)})
		return err
	}); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("наблюдаемое состояние принято как durable SQL-фильтр", err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	if so.Valid() || g.Valid() || w.Valid() || a.Valid() || observers[0].Valid() || observers[1].Valid() {
		t.Fatal("Store.Close оставил оперативных владельцев")
	}
	if _, err := f.service.RuntimeSnapshot(ctx, observationFence(parent)); !errors.Is(err, core.ErrClosed) {
		t.Fatal("закрытое хранилище вернуло прежнюю RAM", err)
	}
}

func TestAcceptanceOutputFailureRollsBackAtomicallyAndPreservesSuccessfulReceipt(t *testing.T) {
	f := newDomainFixture(t, 2)
	parent, outputs := startObservationOutputs(t, f)
	ctx := context.Background()
	so, err := f.service.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	g, err := so.OpenGroup(ctx, f.group.ID, f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	w, err := g.OpenFFmpeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	o, err := f.service.OpenOutputObserver(ctx, outputs[0])
	if err != nil {
		t.Fatal(err)
	}
	neighbor, err := f.service.OpenOutputObserver(ctx, outputs[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Observe(ctx, core.OutputObservation{State: core.OutputPublishing}); err != nil {
		t.Fatal(err)
	}
	publication := activateRuntimeSource(t, f)
	secondPublication := activateRuntimeSource(t, f)
	if publication.Valid() || !secondPublication.Valid() || !so.Valid() || !g.Valid() || !w.Valid() {
		t.Fatal("OBS Override отозвал независимые observations")
	}
	before := observationRows(t, f.store.sqlDB)
	beforeRAM, err := f.service.RuntimeSnapshot(ctx, observationFence(parent))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.sqlDB.ExecContext(ctx, "CREATE TRIGGER reject_failure_event BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'synthetic failure event rollback'); END"); err != nil {
		t.Fatal(err)
	}
	reason := "Окончательный отказ назначения: " + strings.Repeat("подробности;", 64)
	if err := f.service.FailOutput(ctx, o, reason); err == nil {
		t.Fatal("отказ INSERT Event выдан за успешный FailOutput")
	}
	if got := observationRows(t, f.store.sqlDB); !reflect.DeepEqual(before, got) {
		t.Fatal("rollback Event оставил частичное управление или cleanup")
	}
	afterRAM, err := f.service.RuntimeSnapshot(ctx, observationFence(parent))
	if err != nil || !reflect.DeepEqual(beforeRAM, afterRAM) || !o.Valid() {
		t.Fatal("rollback уничтожил прежнюю RAM", err)
	}
	if _, err := f.store.sqlDB.ExecContext(ctx, "DROP TRIGGER reject_failure_event"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.FailOutput(ctx, o, reason); err != nil {
		t.Fatal(err)
	}
	if o.Valid() || !neighbor.Valid() || !so.Valid() || !g.Valid() || !w.Valid() || !secondPublication.Valid() {
		t.Fatal("отказ выхода нарушил независимость владельцев")
	}
	if got := observationRows(t, f.store.sqlDB)["command_receipts"]; !reflect.DeepEqual(before["command_receipts"], got) {
		t.Fatal("runtime failure переписал успешную start receipt")
	}
	current, err := f.service.GetOutput(ctx, outputs[0].OutputID)
	if err != nil || current.DesiredRunning || !current.CleanupPending || !current.Issued || current.Generation <= outputs[0].OutputGeneration {
		t.Fatalf("нет долговечного отзыва и cleanup: %+v %v", current, err)
	}
	if pending := pendingCleanups(t, f.store, parent.SessionID); len(pending) != 1 || pending[0].OutputID != outputs[0].OutputID || pending[0].Attempt != outputs[0].Attempt {
		t.Fatalf("cleanup не совпадает с отказавшей попыткой: %+v", pending)
	}
	failures := 0
	if err := f.store.View(ctx, func(r core.Repositories) error {
		events, err := r.Runtime.Events(ctx, core.ListQuery{Limit: 100, SessionID: parent.SessionID})
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.Kind == "output_execution_failed" {
				failures++
				if event.Message != reason || event.Execution == nil || *event.Execution != outputs[0] || event.OutputID != outputs[0].OutputID || event.SessionID != parent.SessionID {
					t.Fatalf("история потеряла полный fence/причину: %+v", event)
				}
			} else if event.Execution != nil {
				t.Fatal("обычное control event получило execution fence")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if failures != 1 {
		t.Fatalf("итоговых событий %d, требуется одно", failures)
	}
	committed := observationRows(t, f.store.sqlDB)
	if err := f.service.FailOutput(ctx, o, "поздний отказ"); !errors.Is(err, core.ErrStale) {
		t.Fatal(err)
	}
	if err := o.Observe(ctx, core.OutputObservation{State: core.OutputPublishing}); !errors.Is(err, core.ErrStale) {
		t.Fatal(err)
	}
	if got := observationRows(t, f.store.sqlDB); !reflect.DeepEqual(committed, got) {
		t.Fatal("поздний callback изменил долговечное управление")
	}
}

func TestAcceptanceIssuedStartupObserverRetiresBeforeLateCompletion(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	fence, err := f.service.Claim(ctx, start.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.OpenSessionObserver(ctx, fence); !errors.Is(err, core.ErrStale) {
		t.Fatal("невыданная попытка допущена", err)
	}
	if err := f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	o, err := f.service.OpenSessionObserver(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Observe(ctx, core.SessionObservation{State: core.ObservationStarting}); err != nil {
		t.Fatal(err)
	}
	stop, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "stop-observed-startup", SessionID: start.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if o.Valid() {
		t.Fatal("Stop не отозвал startup observer до возврата")
	}
	if _, err := f.service.RuntimeSnapshot(ctx, observationFence(fence)); !errors.Is(err, core.ErrNotReady) {
		t.Fatal(err)
	}
	if err := f.service.Complete(ctx, fence, core.Completion{Success: true}); !errors.Is(err, core.ErrStale) {
		t.Fatal(err)
	}
	executeCommand(t, f.service, stop.RequestID)
	if len(pendingCleanups(t, f.store, start.SessionID)) != 1 {
		t.Fatal("отсутствие RAM подменило cleanup ack")
	}
	if err := f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision); !errors.Is(err, core.ErrInUse) {
		t.Fatal("исчезнувший observer освободил физическое удержание", err)
	}
}

func TestAcceptanceFailedOutputCompletionRecordsOnlyDurableCommandResult(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	parent := executeCommand(t, f.service, start.RequestID)
	output := readOutputs(t, f.store, parent.SessionID)[0]
	command := outputStartReceipt(t, f, output.ID)
	fence, err := f.service.Claim(ctx, command.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.OpenOutputObserver(ctx, fence); !errors.Is(err, core.ErrStale) {
		t.Fatal("ISSUED без успешного Complete выдан за работающий выход", err)
	}
	reason := "Не удалось открыть назначение"
	if err := f.service.Complete(ctx, fence, core.Completion{Failure: reason}); err != nil {
		t.Fatal(err)
	}
	if got := readReceipt(t, f.store, command.RequestID); got.Status != core.CommandFailed || got.Failure != reason {
		t.Fatalf("итог команды не сохранён: %+v", got)
	}
	if _, err := f.service.RuntimeSnapshot(ctx, observationFence(parent)); !errors.Is(err, core.ErrNotReady) {
		t.Fatal("неуспешный Complete выдумал наблюдение", err)
	}
	if got, err := f.service.GetOutput(ctx, output.ID); err != nil || got.DesiredRunning || !got.CleanupPending || !got.Issued {
		t.Fatalf("отказ потерял durable cleanup: %+v %v", got, err)
	}
	if got := readSession(t, f.store, parent.SessionID); got.State != core.SessionRunning || got.Failure != "" {
		t.Fatal("ошибка выхода стала итоговой ошибкой Session", got)
	}
}

func TestAcceptanceFreshSchemaReopenKeepsHistoryAndStartsWithEmptyRAM(t *testing.T) {
	f := newDomainFixture(t, 1)
	parent, outputs := startObservationOutputs(t, f)
	ctx := context.Background()
	o, err := f.service.OpenOutputObserver(ctx, outputs[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Observe(ctx, core.OutputObservation{State: core.OutputRetrying, Error: "ошибка прежнего процесса"}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.FailOutput(ctx, o, "Итоговая причина для истории"); err != nil {
		t.Fatal(err)
	}
	beforeOutput, err := f.service.GetOutput(ctx, outputs[0].OutputID)
	if err != nil {
		t.Fatal(err)
	}
	beforeReceipt := readReceipt(t, f.store, startRequestID(t, f.store, parent.CommandID))
	if err := f.store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var stateColumns int
	if err := f.store.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('outputs') WHERE name='state'").Scan(&stateColumns); err != nil || stateColumns != 0 {
		t.Fatal("primary schema сохранила outputs.state", stateColumns, err)
	}
	f = reopenFixture(t, f)
	if _, err := f.service.RuntimeSnapshot(ctx, observationFence(parent)); !errors.Is(err, core.ErrNotReady) {
		t.Fatal("reopen восстановил прежнюю телеметрию", err)
	}
	after, err := f.service.GetOutput(ctx, beforeOutput.ID)
	if err != nil || !reflect.DeepEqual(beforeOutput.Snapshot, after.Snapshot) {
		t.Fatal("reopen потерял output snapshot", err)
	}
	if got := readReceipt(t, f.store, beforeReceipt.RequestID); !reflect.DeepEqual(beforeReceipt, got) {
		t.Fatal("recovery переписал terminal receipt")
	}
	if got := readSession(t, f.store, parent.SessionID); got.State != core.SessionInterrupted || !got.CleanupPending {
		t.Fatalf("recovery потерял протокол управления: %+v", got)
	}
	if len(pendingCleanups(t, f.store, parent.SessionID)) != 2 {
		t.Fatal("reopen потерял parent/child cleanup")
	}
	for _, cleanup := range pendingCleanups(t, f.store, parent.SessionID) {
		if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanup)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.service.DeleteDestination(ctx, f.destinations[0].ID, f.destinations[0].Revision); err != nil {
		t.Fatal(err)
	}
	if err := f.service.DeleteGroup(ctx, f.group.ID, f.group.Revision); err != nil {
		t.Fatal(err)
	}
	if err := f.service.DeleteProfile(ctx, f.profile.ID, f.profile.Revision); err != nil {
		t.Fatal(err)
	}
	if err := f.store.View(ctx, func(r core.Repositories) error {
		events, err := r.Runtime.Events(ctx, core.ListQuery{Limit: 100, SessionID: parent.SessionID})
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.Kind == "output_execution_failed" && event.Message == "Итоговая причина для истории" && event.Execution != nil && *event.Execution == outputs[0] {
				return nil
			}
		}
		return fmt.Errorf("после hard delete потеряна история отказа")
	}); err != nil {
		t.Fatal(err)
	}
}

func startRequestID(t *testing.T, s *Store, commandID core.ID) string {
	t.Helper()
	var request string
	if err := s.sqlDB.QueryRowContext(context.Background(), "SELECT request_id FROM command_receipts WHERE id=?", commandID).Scan(&request); err != nil {
		t.Fatal(err)
	}
	return request
}

func TestAcceptanceUnsupportedFormatRefusesBeforeRecovery(t *testing.T) {
	for _, mutation := range []string{
		"DROP TABLE storage_format",
		"DELETE FROM storage_format",
		"UPDATE storage_format SET version=1",
		"DROP TABLE storage_format; CREATE TABLE storage_format(singleton INTEGER,version INTEGER); INSERT INTO storage_format VALUES(1,1),(2,1)",
	} {
		t.Run(mutation, func(t *testing.T) {
			f := newDomainFixture(t, 1)
			parent, _ := startObservationOutputs(t, f)
			ctx := context.Background()
			var seq int
			var name, path string
			if err := f.store.sqlDB.QueryRowContext(ctx, "PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.sqlDB.ExecContext(ctx, mutation); err != nil {
				t.Fatal(err)
			}
			before := observationRows(t, f.store.sqlDB)
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				s, err := Open(ctx, path, testOptions())
				if s != nil {
					_ = s.Close()
					t.Fatal("неподдерживаемый формат открыт")
				}
				if !errors.Is(err, ErrIncompatibleFormat) {
					t.Fatal("нет стабильной категории несовместимости", err)
				}
			}
			raw, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := raw.Close(); err != nil {
					t.Error(err)
				}
			})
			if got := observationRows(t, raw); !reflect.DeepEqual(before, got) {
				t.Fatal("отказ формата успел выполнить recovery либо reset")
			}
			var state string
			if err := raw.QueryRowContext(ctx, "SELECT state FROM sessions WHERE id=?", parent.SessionID).Scan(&state); err != nil || state != string(core.SessionRunning) {
				t.Fatal("несовместимый Open изменил Session", state, err)
			}
		})
	}
}
