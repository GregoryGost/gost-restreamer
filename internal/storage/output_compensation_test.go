package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

type compensationWaitingStore struct {
	*Store
	entered chan struct{}
	resume  chan struct{}
}

func (s *compensationWaitingStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	close(s.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.resume:
		return s.Store.Write(ctx, callback)
	}
}

func TestOutputStartCompensationRacingUserStopPreservesItsReceipt(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	output := readOutputs(t, f.store, start.SessionID)[0]
	fence := executeCommand(t, f.service, outputStartReceipt(t, f, output.ID).RequestID)
	waiting := &compensationWaitingStore{Store: f.store, entered: make(chan struct{}), resume: make(chan struct{})}
	service, err := core.NewService(waiting, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		receipt core.CommandReceipt
		applied bool
		err     error
	}
	done := make(chan outcome, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		receipt, applied, err := service.CompensateOutputStart(ctx, fence, "concurrent-compensation")
		done <- outcome{receipt, applied, err}
	}()
	defer func() {
		cancel()
		<-joined
	}()
	select {
	case <-waiting.entered:
	case <-ctx.Done():
		t.Fatal("компенсация не дошла до границы записи")
	}
	stop, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "winning-user-stop", OutputID: output.ID})
	if err != nil {
		t.Fatal(err)
	}
	close(waiting.resume)
	select {
	case result := <-done:
		if result.err != nil || result.applied || !reflect.DeepEqual(result.receipt, core.CommandReceipt{}) {
			t.Fatalf("компенсация не уступила выигравшей остановке: %+v", result)
		}
	case <-ctx.Done():
		t.Fatal("компенсация не завершилась после остановки")
	}
	if receipt := readReceipt(t, f.store, stop.RequestID); !reflect.DeepEqual(stop, receipt) {
		t.Fatalf("компенсация прервала принятую остановку: %+v", receipt)
	}
}

func TestAddedOutputStartCompensationUsesItsExactExecution(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	destination, err := f.service.CreateDestination(ctx, core.Destination{StreamID: f.stream.ID, GroupID: f.group.ID, Name: "Добавленное назначение", Platform: "custom", Endpoint: "rtmp://example.invalid/live"})
	if err != nil {
		t.Fatal(err)
	}
	command, err := f.service.AddOutput(ctx, core.AddOutputRequest{RequestID: "add-output", SessionID: start.SessionID, DestinationID: destination.ID, Binding: f.startRequest("unused").Bindings[0]})
	if err != nil {
		t.Fatal(err)
	}
	fence := executeCommand(t, f.service, command.RequestID)
	original := readReceipt(t, f.store, command.RequestID)
	stop, applied, err := f.service.CompensateOutputStart(ctx, fence, "added-output-compensation")
	if err != nil || !applied || stop.OutputID != command.OutputID || stop.Kind != core.CommandStopOutput {
		t.Fatalf("не принята компенсация добавленного выхода: %+v, %t, %v", stop, applied, err)
	}
	if after := readReceipt(t, f.store, command.RequestID); !reflect.DeepEqual(original, after) {
		t.Fatal("компенсация изменила успешный AddOutput")
	}
}

func TestOutputStartCompensationTracksExactCleanupAndPreservesSuccess(t *testing.T) {
	for _, status := range []core.CommandStatus{core.CommandClaimed, core.CommandIssued, core.CommandSucceeded} {
		t.Run(string(status), func(t *testing.T) {
			f := newDomainFixture(t, 2)
			ctx := context.Background()
			start := startDomainSession(t, f)
			executeCommand(t, f.service, start.RequestID)
			outputs := readOutputs(t, f.store, start.SessionID)
			command := outputStartReceipt(t, f, outputs[0].ID)
			fence, err := f.service.Claim(ctx, command.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if status != core.CommandClaimed {
				if err := f.service.MarkIssued(ctx, fence); err != nil {
					t.Fatal(err)
				}
			}
			if status == core.CommandSucceeded {
				if err := f.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
					t.Fatal(err)
				}
			}
			original := readReceipt(t, f.store, command.RequestID)
			stop, applied, err := f.service.CompensateOutputStart(ctx, fence, "activation-compensation")
			if err != nil || !applied || stop.Kind != core.CommandStopOutput || stop.Status != core.CommandPending || stop.OutputGeneration != fence.OutputGeneration+1 {
				t.Fatalf("не принята точная компенсация: %+v, %t, %v", stop, applied, err)
			}
			output, err := f.service.GetOutput(ctx, fence.OutputID)
			if err != nil || output.DesiredRunning || !output.Attached || !output.CleanupPending || output.Attempt != fence.Attempt || output.Issued != (status != core.CommandClaimed) {
				t.Fatalf("утрачено обязательство или выдумана issuance: %+v, %v", output, err)
			}
			cleanups := pendingCleanups(t, f.store, start.SessionID)
			if len(cleanups) != 1 || cleanups[0].OutputID != fence.OutputID || cleanups[0].Generation != fence.OutputGeneration || cleanups[0].Attempt != fence.Attempt || cleanups[0].Lifecycle != fence.Lifecycle || cleanups[0].Incarnation != fence.Incarnation {
				t.Fatalf("компенсация потеряла точный cleanup fence: %+v", cleanups)
			}
			if session := readSession(t, f.store, start.SessionID); session.State != core.SessionRunning || !session.CleanupPending {
				t.Fatalf("компенсация изменила Session: %+v", session)
			}
			if neighbor, err := f.service.GetOutput(ctx, outputs[1].ID); err != nil || !reflect.DeepEqual(outputs[1], neighbor) {
				t.Fatalf("компенсация изменила соседа: %+v, %v", neighbor, err)
			}
			after := readReceipt(t, f.store, command.RequestID)
			if status == core.CommandSucceeded {
				if !reflect.DeepEqual(original, after) {
					t.Fatal("компенсация переписала успешную квитанцию запуска")
				}
			} else if after.Status != core.CommandInterrupted {
				t.Fatalf("компенсация оставила команду исполнимой: %+v", after)
			}
			requireOutputVariantHolds(t, f.store, f.variant.ID, true)
			replayed, applied, err := f.service.CompensateOutputStart(ctx, fence, stop.RequestID)
			if err != nil || !applied || !reflect.DeepEqual(stop, replayed) || len(pendingCleanups(t, f.store, start.SessionID)) != 1 {
				t.Fatalf("повтор компенсации изменил результат: %+v, %t, %v", replayed, applied, err)
			}
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
				t.Fatal(err)
			}
			if receipt := readReceipt(t, f.store, stop.RequestID); receipt.Status != core.CommandSucceeded {
				t.Fatalf("cleanup не завершил компенсацию: %+v", receipt)
			}
		})
	}
}

func TestOutputStartCompensationPreservesWinningUserStopAndDetach(t *testing.T) {
	for _, detach := range []bool{false, true} {
		name := "stop"
		if detach {
			name = "detach"
		}
		t.Run(name, func(t *testing.T) {
			f := newDomainFixture(t, 1)
			ctx := context.Background()
			start := startDomainSession(t, f)
			executeCommand(t, f.service, start.RequestID)
			output := readOutputs(t, f.store, start.SessionID)[0]
			command := outputStartReceipt(t, f, output.ID)
			fence := executeCommand(t, f.service, command.RequestID)
			request := core.OutputCommand{RequestID: "user-stop", OutputID: output.ID}
			var stop core.CommandReceipt
			var err error
			if detach {
				stop, err = f.service.DetachOutput(ctx, request)
			} else {
				stop, err = f.service.StopOutput(ctx, request)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := readOutputs(t, f.store, start.SessionID)
			cleanups := pendingCleanups(t, f.store, start.SessionID)
			assertNoCompensation := func() {
				t.Helper()
				result, applied, err := f.service.CompensateOutputStart(ctx, fence, "late-activation-compensation")
				if err != nil || applied || !reflect.DeepEqual(result, core.CommandReceipt{}) {
					t.Fatalf("отозванный запуск принял компенсацию: %+v, %t, %v", result, applied, err)
				}
				if receipt := readReceipt(t, f.store, stop.RequestID); !reflect.DeepEqual(stop, receipt) {
					t.Fatalf("компенсация прервала выигравшую команду: %+v", receipt)
				}
				if err := f.store.View(ctx, func(r core.Repositories) error {
					_, err := r.Runtime.Receipt(ctx, "late-activation-compensation")
					return err
				}); !errors.Is(err, core.ErrNotFound) {
					t.Fatalf("отозванный запуск создал квитанцию: %v", err)
				}
			}
			assertNoCompensation()
			if !reflect.DeepEqual(before, readOutputs(t, f.store, start.SessionID)) || !reflect.DeepEqual(cleanups, pendingCleanups(t, f.store, start.SessionID)) {
				t.Fatal("компенсация изменила остановленный выход или cleanup")
			}
			if len(cleanups) != 1 {
				t.Fatalf("ожидалось одно обязательство: %+v", cleanups)
			}
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
				t.Fatal(err)
			}
			stop = readReceipt(t, f.store, stop.RequestID)
			if stop.Status != core.CommandSucceeded {
				t.Fatal("выигравшая команда не завершилась")
			}
			assertNoCompensation()
			if receipt := readReceipt(t, f.store, command.RequestID); receipt.Status != core.CommandSucceeded {
				t.Fatal("успешный запуск изменён поздней компенсацией")
			}
		})
	}
}

func TestOutputStartCompensationCannotStopNewerExecution(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	output := readOutputs(t, f.store, start.SessionID)[0]
	command := outputStartReceipt(t, f, output.ID)
	old := executeCommand(t, f.service, command.RequestID)
	compensation, applied, err := f.service.CompensateOutputStart(ctx, old, "old-compensation")
	if err != nil || !applied {
		t.Fatal("не принята исходная компенсация", err)
	}
	cleanups := pendingCleanups(t, f.store, start.SessionID)
	if len(cleanups) != 1 {
		t.Fatal("утрачено обязательство старого запуска")
	}
	if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
		t.Fatal(err)
	}
	compensation = readReceipt(t, f.store, compensation.RequestID)
	restart, err := f.service.StartOutput(ctx, core.OutputCommand{RequestID: "fresh-start", OutputID: output.ID})
	if err != nil {
		t.Fatal(err)
	}
	fresh := executeCommand(t, f.service, restart.RequestID)
	owner, err := f.service.BindExecutionOwner(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	before := readOutputs(t, f.store, start.SessionID)
	for _, requestID := range []string{"late-old-compensation", compensation.RequestID} {
		result, applied, err := f.service.CompensateOutputStart(ctx, old, requestID)
		if err != nil {
			t.Fatal(err)
		}
		if requestID == compensation.RequestID {
			if !applied || !reflect.DeepEqual(compensation, result) {
				t.Fatalf("повтор квитанции переоценил новый запуск: %+v, %t", result, applied)
			}
		} else if applied || !reflect.DeepEqual(result, core.CommandReceipt{}) {
			t.Fatal("старая компенсация принята для нового запуска")
		}
	}
	if !owner.Valid() || !reflect.DeepEqual(before, readOutputs(t, f.store, start.SessionID)) || len(pendingCleanups(t, f.store, start.SessionID)) != 0 {
		t.Fatal("старая компенсация изменила новое исполнение")
	}
	if receipt := readReceipt(t, f.store, restart.RequestID); receipt.Status != core.CommandSucceeded {
		t.Fatal("прервана новая команда запуска")
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, true)
}

func TestOutputStartCompensationRejectsMismatchedFenceWithoutMutation(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	output := readOutputs(t, f.store, start.SessionID)[0]
	command := outputStartReceipt(t, f, output.ID)
	fence := executeCommand(t, f.service, command.RequestID)
	before := readOutputs(t, f.store, start.SessionID)
	for _, change := range []func(*core.ExecutionFence){
		func(f *core.ExecutionFence) { f.CommandID = "missing-command" },
		func(f *core.ExecutionFence) { f.Incarnation = "different-owner" },
		func(f *core.ExecutionFence) { f.SessionID = "different-session" },
		func(f *core.ExecutionFence) { f.OutputID = "different-output" },
		func(f *core.ExecutionFence) { f.Lifecycle++ },
		func(f *core.ExecutionFence) { f.OutputGeneration++ },
		func(f *core.ExecutionFence) { f.Attempt++ },
	} {
		stale := fence
		change(&stale)
		result, applied, err := f.service.CompensateOutputStart(ctx, stale, "stale-compensation")
		if err != nil || applied || !reflect.DeepEqual(result, core.CommandReceipt{}) {
			t.Fatalf("принят несовпадающий fence: %+v, %t, %v", result, applied, err)
		}
	}
	if !reflect.DeepEqual(before, readOutputs(t, f.store, start.SessionID)) || len(pendingCleanups(t, f.store, start.SessionID)) != 0 {
		t.Fatal("несовпадающий fence изменил выход или cleanup")
	}
	_, _, err := f.service.CompensateOutputStart(ctx, fence, "")
	requireDomainError(t, err, core.ErrInvalid)
	_, err = f.service.PauseSession(ctx, core.SessionCommand{RequestID: "conflicting-command", SessionID: start.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.service.CompensateOutputStart(ctx, fence, "conflicting-command")
	requireDomainError(t, err, core.ErrConflict)
}
