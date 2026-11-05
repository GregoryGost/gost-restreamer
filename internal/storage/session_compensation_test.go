package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestSessionExecutionCompensationPreservesExactLiabilitiesAndSuccess(t *testing.T) {
	for _, status := range []core.CommandStatus{core.CommandPending, core.CommandClaimed, core.CommandIssued, core.CommandSucceeded} {
		t.Run(string(status), func(t *testing.T) {
			f := newDomainFixture(t, 1)
			ctx := context.Background()
			start := startDomainSession(t, f)
			fence := core.ExecutionFence{CommandID: start.ID, SessionID: start.SessionID, Incarnation: start.Incarnation, Lifecycle: start.Lifecycle}
			if status != core.CommandPending {
				var err error
				fence, err = f.service.Claim(ctx, start.RequestID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if status == core.CommandIssued || status == core.CommandSucceeded {
				if err := f.service.MarkIssued(ctx, fence); err != nil {
					t.Fatal(err)
				}
			}
			if status == core.CommandSucceeded {
				if err := f.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
					t.Fatal(err)
				}
			}
			original := readReceipt(t, f.store, start.RequestID)
			if status == core.CommandPending {
				if err := fence.Validate(); !errors.Is(err, core.ErrInvalid) {
					t.Fatal("нулевой Attempt допущен общим ExecutionFence", err)
				}
				if owner, err := f.service.BindExecutionOwner(ctx, fence); !errors.Is(err, core.ErrStale) || owner != nil {
					t.Fatal("незахваченная команда получила владельца исполнения", owner, err)
				}
			}
			stop, applied, err := f.service.CompensateSessionExecution(ctx, fence, "retire-owner")
			if err != nil || !applied || stop.Kind != core.CommandStopSession || stop.Lifecycle != fence.Lifecycle+1 {
				t.Fatalf("не принята точная компенсация: %+v %t %v", stop, applied, err)
			}
			session := readSession(t, f.store, start.SessionID)
			issued := status == core.CommandIssued || status == core.CommandSucceeded
			if session.State != core.SessionStopping || session.CleanupPending != issued || session.Issued != issued {
				t.Fatalf("компенсация потеряла состояние или выдумала issuance: %+v", session)
			}
			cleanups := pendingCleanups(t, f.store, start.SessionID)
			if issued {
				if len(cleanups) != 1 || cleanups[0].OutputID != "" || cleanups[0].SessionID != fence.SessionID || cleanups[0].Lifecycle != fence.Lifecycle || cleanups[0].Incarnation != fence.Incarnation {
					t.Fatalf("утрачен точный parent cleanup: %+v", cleanups)
				}
				requireOutputVariantHolds(t, f.store, f.variant.ID, true)
			} else if len(cleanups) != 0 {
				t.Fatalf("создана фиктивная очистка: %+v", cleanups)
			}
			if status == core.CommandSucceeded && !reflect.DeepEqual(original, readReceipt(t, f.store, start.RequestID)) {
				t.Fatal("переписана успешная квитанция запуска")
			}
			replay, applied, err := f.service.CompensateSessionExecution(ctx, fence, stop.RequestID)
			if err != nil || !applied || !reflect.DeepEqual(stop, replay) {
				t.Fatalf("повтор изменил компенсацию: %+v %t %v", replay, applied, err)
			}
			executeCommand(t, f.service, stop.RequestID)
			settled := readReceipt(t, f.store, stop.RequestID)
			// Неподтверждённая очистка старого исполнения не мешает новому ручному запуску.
			next, err := f.service.StartSession(ctx, f.startRequest("next-session"))
			if err != nil {
				t.Fatal(err)
			}
			fresh := executeCommand(t, f.service, next.RequestID)
			owner, err := f.service.BindExecutionOwner(ctx, fresh)
			if err != nil {
				t.Fatal(err)
			}
			before := readSession(t, f.store, next.SessionID)
			for range 3 {
				_, applied, err = f.service.CompensateSessionExecution(ctx, fence, "late-retirement")
				if err != nil || applied {
					t.Fatal("старое исполнение повторно отозвано", applied, err)
				}
			}
			replay, applied, err = f.service.CompensateSessionExecution(ctx, fence, stop.RequestID)
			if err != nil || !applied || !reflect.DeepEqual(settled, replay) {
				t.Fatal("повтор квитанции переоценил новое исполнение", replay, applied, err)
			}
			for _, intent := range cleanups {
				if err = f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(intent)); err != nil {
					t.Fatal(err)
				}
			}
			if !owner.Valid() || !reflect.DeepEqual(before, readSession(t, f.store, next.SessionID)) {
				t.Fatal("старая очистка изменила новый запуск")
			}
			requireOutputVariantHolds(t, f.store, f.variant.ID, true)
		})
	}
}

func TestSessionExecutionCompensationLosesAtomicallyToUserStop(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := startDomainSession(t, f)
	fence := executeCommand(t, f.service, start.RequestID)
	waiting := &compensationWaitingStore{Store: f.store, entered: make(chan struct{}), resume: make(chan struct{})}
	service, err := core.NewService(waiting, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		applied bool
		err     error
	}
	done := make(chan outcome, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		_, applied, err := service.CompensateSessionExecution(ctx, fence, "losing-retirement")
		done <- outcome{applied, err}
	}()
	defer func() { cancel(); <-joined }()
	select {
	case <-waiting.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stop, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "winning-stop", SessionID: start.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	cleanups := pendingCleanups(t, f.store, start.SessionID)
	close(waiting.resume)
	select {
	case result := <-done:
		if result.err != nil || result.applied {
			t.Fatal("компенсация не уступила остановке", result)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !reflect.DeepEqual(stop, readReceipt(t, f.store, stop.RequestID)) || !reflect.DeepEqual(cleanups, pendingCleanups(t, f.store, start.SessionID)) {
		t.Fatal("компенсация изменила выигравшую остановку или её обязательства")
	}
	executeCommand(t, f.service, stop.RequestID)
}

func TestSessionExecutionCompensationRejectsMalformedAndStaleFences(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	start := startDomainSession(t, f)
	fence := executeCommand(t, f.service, start.RequestID)
	before := readSession(t, f.store, start.SessionID)
	foreignService, err := core.NewService(f.store, "other-incarnation")
	if err != nil {
		t.Fatal(err)
	}
	if _, applied, err := foreignService.CompensateSessionExecution(ctx, fence, "foreign-retirement"); err != nil || applied {
		t.Fatal("чужая инкарнация отозвала исполнение", applied, err)
	}
	for _, change := range []func(*core.ExecutionFence){
		func(f *core.ExecutionFence) { f.CommandID = "missing-command" },
		func(f *core.ExecutionFence) { f.SessionID = "missing-session" },
		func(f *core.ExecutionFence) { f.Incarnation = "other-incarnation" },
		func(f *core.ExecutionFence) { f.Lifecycle++ },
		func(f *core.ExecutionFence) { f.Attempt++ },
		func(f *core.ExecutionFence) { f.Attempt = 0 },
	} {
		stale := fence
		change(&stale)
		_, applied, err := f.service.CompensateSessionExecution(ctx, stale, "stale-retirement")
		if err != nil || applied {
			t.Fatal("принят чужой fence", stale, applied, err)
		}
	}
	for _, change := range []func(*core.ExecutionFence){
		func(f *core.ExecutionFence) { f.CommandID = "" },
		func(f *core.ExecutionFence) { f.Lifecycle = 0 },
		func(f *core.ExecutionFence) { f.Incarnation = "" },
		func(f *core.ExecutionFence) { f.OutputGeneration = 1 },
	} {
		malformed := fence
		change(&malformed)
		_, _, err := f.service.CompensateSessionExecution(ctx, malformed, "malformed-retirement")
		if !errors.Is(err, core.ErrInvalid) {
			t.Fatal("принят некорректный fence", malformed, err)
		}
	}
	_, _, err = f.service.CompensateSessionExecution(ctx, fence, "")
	requireDomainError(t, err, core.ErrInvalid)
	_, _, err = f.service.CompensateSessionExecution(ctx, fence, start.RequestID)
	requireDomainError(t, err, core.ErrConflict)
	if !reflect.DeepEqual(before, readSession(t, f.store, start.SessionID)) || len(pendingCleanups(t, f.store, start.SessionID)) != 0 {
		t.Fatal("неверный fence изменил Session")
	}
}
