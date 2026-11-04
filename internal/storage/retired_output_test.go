package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

type retiredRuntimeSnapshot struct {
	session  core.Session
	outputs  []core.SessionOutput
	receipts []core.CommandReceipt
	events   []core.Event
	cleanups []core.CleanupIntent
}

func readRetiredRuntime(t *testing.T, f domainFixture, sessionID core.ID) retiredRuntimeSnapshot {
	t.Helper()
	ctx := context.Background()
	var snapshot retiredRuntimeSnapshot
	if err := f.store.View(ctx, func(r core.Repositories) error {
		q := core.ListQuery{Limit: core.MaxListLimit, SessionID: sessionID}
		var err error
		if snapshot.session, err = r.Runtime.Session(ctx, sessionID); err != nil {
			return err
		}
		if snapshot.outputs, err = r.Runtime.Outputs(ctx, q); err != nil {
			return err
		}
		if snapshot.receipts, err = r.Runtime.Receipts(ctx, q); err != nil {
			return err
		}
		if snapshot.events, err = r.Runtime.Events(ctx, q); err != nil {
			return err
		}
		snapshot.cleanups, err = r.Artifacts.Cleanups(ctx, q)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestRetiredOutputControlsRejectNewRequestsAndPreserveReplay(t *testing.T) {
	for _, kind := range []core.CommandKind{core.CommandStopOutput, core.CommandDetachOutput} {
		t.Run(string(kind), func(t *testing.T) {
			f := newDomainFixture(t, 1)
			ctx := context.Background()
			control := func(request core.OutputCommand) (core.CommandReceipt, error) {
				if kind == core.CommandStopOutput {
					return f.service.StopOutput(ctx, request)
				}
				return f.service.DetachOutput(ctx, request)
			}
			start := startDomainSession(t, f)
			executeCommand(t, f.service, start.RequestID)
			completedStart := readReceipt(t, f.store, start.RequestID)
			output := readOutputs(t, f.store, start.SessionID)[0]
			executeCommand(t, f.service, outputStartReceipt(t, f, output.ID).RequestID)
			original := core.OutputCommand{RequestID: "original-output-control", OutputID: output.ID}
			accepted, err := control(original)
			if err != nil || accepted.Status != core.CommandPending {
				t.Fatalf("выданный выход должен ожидать очистки: статус %s, ошибка %v", accepted.Status, err)
			}
			oldIncarnation := f.store.Incarnation()
			f = reopenFixture(t, f)
			if f.store.Incarnation() == oldIncarnation {
				t.Fatal("повторное открытие не сменило владельца")
			}
			recovered := readSession(t, f.store, start.SessionID)
			if recovered.State != core.SessionInterrupted || !recovered.CleanupPending || recovered.Incarnation != oldIncarnation {
				t.Fatalf("не восстановлено прерывание старого владельца: %+v", recovered)
			}
			interrupted := readReceipt(t, f.store, original.RequestID)
			if interrupted.Status != core.CommandInterrupted {
				t.Fatalf("прежняя незавершённая команда не прервана: %s", interrupted.Status)
			}
			cleanups := pendingCleanups(t, f.store, start.SessionID)
			if len(cleanups) != 2 {
				t.Fatalf("нужны отдельные подтверждения родителя и выхода: %d", len(cleanups))
			}

			rejectNew := func(requestID string) {
				t.Helper()
				before := readRetiredRuntime(t, f, start.SessionID)
				receipt, err := control(core.OutputCommand{RequestID: requestID, OutputID: output.ID})
				if !errors.Is(err, core.ErrStale) && !errors.Is(err, core.ErrConflict) {
					t.Errorf("новая команда к выходу прежнего владельца должна отклоняться: статус %s, ошибка %v", receipt.Status, err)
				}
				if after := readRetiredRuntime(t, f, start.SessionID); !reflect.DeepEqual(before, after) {
					t.Error("отклонение изменило Session, Output, квитанции, события или очистку")
				}
				if err := f.store.View(ctx, func(r core.Repositories) error {
					_, err := r.Runtime.Receipt(ctx, requestID)
					if !errors.Is(err, core.ErrNotFound) {
						t.Errorf("отклонённый RequestID получил сохранённую квитанцию: %v", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			replayOriginal := func() {
				t.Helper()
				before := readRetiredRuntime(t, f, start.SessionID)
				replayed, err := control(original)
				if err != nil || !reflect.DeepEqual(replayed, interrupted) {
					t.Errorf("повтор прежнего управления потерял принятый результат: статус %s, ошибка %v", replayed.Status, err)
				}
				replayed, err = f.service.StartSession(ctx, f.startRequest(start.RequestID))
				if err != nil || !reflect.DeepEqual(replayed, completedStart) {
					t.Errorf("повтор завершённого запуска потерял принятый результат: статус %s, ошибка %v", replayed.Status, err)
				}
				if _, err := f.service.Claim(ctx, original.RequestID); !errors.Is(err, core.ErrStale) {
					t.Errorf("повторное исполнение прерванной команды разрешено: %v", err)
				}
				if _, err := f.service.Claim(ctx, start.RequestID); !errors.Is(err, core.ErrStale) {
					t.Errorf("повторное исполнение завершённого запуска разрешено: %v", err)
				}
				if after := readRetiredRuntime(t, f, start.SessionID); !reflect.DeepEqual(before, after) {
					t.Error("повтор прежней команды изменил сохранённое состояние")
				}
			}

			replayOriginal()
			rejectNew("new-retired-output-control")
			for _, cleanup := range cleanups {
				if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanup)); err != nil {
					t.Fatal(err)
				}
			}
			if pending := pendingCleanups(t, f.store, start.SessionID); len(pending) != 0 {
				t.Fatalf("подтверждённая очистка осталась незавершённой: %d", len(pending))
			}
			rejectNew("new-retired-output-control")
			rejectNew("new-control-after-cleanup")
			replayOriginal()
			if err := f.store.View(ctx, func(r core.Repositories) error {
				pending, err := r.Runtime.Receipts(ctx, core.ListQuery{Limit: core.MaxListLimit, SessionID: start.SessionID, PendingOnly: true})
				if err == nil && len(pending) != 0 {
					t.Errorf("очистка оставила невыполнимые намерения: %d, статус %s", len(pending), pending[0].Status)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			stop, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "stop-retired-session", SessionID: start.SessionID})
			if err != nil || stop.Status != core.CommandSucceeded {
				t.Fatalf("stop завершённой Session должен быть успешной пустой операцией: статус %s, ошибка %v", stop.Status, err)
			}
			fresh, err := f.service.StartSession(ctx, f.startRequest("manual-after-retired-cleanup"))
			if err != nil || fresh.SessionID == start.SessionID {
				t.Fatalf("ручной запуск после очистки не создал новую Session: %v", err)
			}
			executeCommand(t, f.service, fresh.RequestID)
			freshOutput := readOutputs(t, f.store, fresh.SessionID)[0]
			fence := executeCommand(t, f.service, outputStartReceipt(t, f, freshOutput.ID).RequestID)
			healthy := readRetiredRuntime(t, f, fresh.SessionID)
			if healthy.session.State != core.SessionRunning || healthy.outputs[0].State != core.OutputPublishing {
				t.Fatal("новый запуск не достиг RUNNING и PUBLISHING")
			}
			rejectNew("old-target-under-new-session")
			if after := readRetiredRuntime(t, f, fresh.SessionID); !reflect.DeepEqual(healthy, after) {
				t.Error("управление старым выходом изменило здоровый новый запуск")
			}
			if err := f.service.ObserveOutput(ctx, fence, core.OutputRetrying, "Временный отказ сети"); err != nil {
				t.Fatalf("старый выход отозвал допуск нового publisher: %v", err)
			}
			if err := f.service.ObserveOutput(ctx, fence, core.OutputPublishing, ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}
