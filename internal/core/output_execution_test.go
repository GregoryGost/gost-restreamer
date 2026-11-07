package core

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func executionFixture(t *testing.T) (*Service, *observationAcceptanceUOW, *RuntimeLease, ExecutionFence, OutputSnapshot) {
	t.Helper()
	s, u, _, f := newObservationAcceptanceService(t, acceptanceObservationLimits())
	snapshot := OutputSnapshot{Destination: Destination{Name: "Назначение", Endpoint: "rtmp://example.invalid/live", StreamKey: "exact-key"}, Artifact: ArtifactManifest{CanonicalProfile: []byte{1, 2, 3}}}
	o := u.repository.outputs[f.OutputID]
	o.Snapshot = snapshot
	u.repository.outputs[f.OutputID] = o
	lease, err := s.BindExecutionOwner(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return s, u, lease, f, snapshot
}
func reserveExecution(t *testing.T, s *Service, l *RuntimeLease, snapshot OutputSnapshot) *OutputExecutionReservation {
	t.Helper()
	r, err := s.ReserveOutputExecution(context.Background(), l, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func activateExecution(t *testing.T, s *Service, r *OutputExecutionReservation, f ExecutionFence) *OutputObserver {
	t.Helper()
	o, err := s.OpenOutputObserver(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Activate(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	return o
}
func deauthorizeExecution(u *observationAcceptanceUOW, f ExecutionFence) {
	o := u.repository.outputs[f.OutputID]
	o.Generation++
	o.DesiredRunning = false
	o.CleanupPending = true
	u.repository.outputs[f.OutputID] = o
}

func TestOutputExecutionExactSnapshotAndSharedExclusivity(t *testing.T) {
	ctx := context.Background()
	s, u, l, f, snapshot := executionFixture(t)
	for _, change := range []func(*OutputSnapshot){
		func(s *OutputSnapshot) { s.Destination.Endpoint += "?changed" },
		func(s *OutputSnapshot) { s.Artifact.CanonicalProfile = []byte{1, 2, 4} },
		func(s *OutputSnapshot) { s.Artifact.CanonicalProfile = nil },
		func(s *OutputSnapshot) { s.Destination.StreamKey = "other-key" },
	} {
		forged := snapshot
		change(&forged)
		if _, err := s.ReserveOutputExecution(ctx, l, forged); !errors.Is(err, ErrStale) {
			t.Fatal("подменённый снимок принят", err)
		}
	}
	r := reserveExecution(t, s, l, snapshot)
	if r.ExecutionFence() != f {
		t.Fatal("ExecutionFence изменён")
	}
	other, err := NewService(u, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.ReserveOutputExecution(ctx, l, snapshot); !errors.Is(err, ErrInUse) {
		t.Fatal("второй Service обошёл эксклюзивность", err)
	}
	if err = l.Revoke(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.Done():
	default:
		t.Fatal("отзыв не запечатал исполнение")
	}
	fresh, err := other.BindExecutionOwner(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.ReserveOutputExecution(ctx, fresh, snapshot); !errors.Is(err, ErrInUse) {
		t.Fatal("повторная привязка обошла удержание", err)
	}
	if err = r.ReleaseJoined(); !errors.Is(err, ErrNotReady) {
		t.Fatal("отзыв разрешил повтор пригодного исполнения", err)
	}
	if err = other.ConfirmOutputExecutionResolution(ctx, r); !errors.Is(err, ErrNotReady) {
		t.Fatal("пригодный ExecutionFence ошибочно разрешён", err)
	}
	deauthorizeExecution(u, f)
	if err = other.ConfirmOutputExecutionResolution(ctx, r); err != nil {
		t.Fatal(err)
	}
	u.viewErr = errors.New("SQL недоступен")
	if err = r.ReleaseJoined(); err != nil {
		t.Fatal("ReleaseJoined обращается к SQL", err)
	}
	if err = r.ReleaseJoined(); err != nil {
		t.Fatal("ReleaseJoined не идемпотентен", err)
	}
	if len(u.authority.outputExecutions) != 0 || u.authority.outputExecutionBytes != 0 {
		t.Fatal("бюджет не освобождён")
	}
}

func TestOutputExecutionSlotsIdentityAndCloseCleanup(t *testing.T) {
	ctx := context.Background()
	s, u, l, f, snapshot := executionFixture(t)
	r := reserveExecution(t, s, l, snapshot)
	if _, err := r.Admit(ctx, OutputOperationRead); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	if err := r.Activate(ctx, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	observer := activateExecution(t, s, r, f)
	if err := r.Activate(ctx, observer); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	var slots [3]*OutputOperation
	for _, kind := range []OutputOperationKind{OutputOperationEstablishment, OutputOperationRead, OutputOperationWrite} {
		var err error
		slots[kind], err = r.Admit(ctx, kind)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.Admit(ctx, kind); !errors.Is(err, ErrInUse) {
			t.Fatal("слот занят повторно", err)
		}
	}
	slots[OutputOperationWrite].Finish()
	fresh, err := r.Admit(ctx, OutputOperationWrite)
	if err != nil {
		t.Fatal(err)
	}
	slots[OutputOperationWrite].Finish()
	if _, err = r.Admit(ctx, OutputOperationWrite); !errors.Is(err, ErrInUse) {
		t.Fatal("поздний Finish освободил чужой слот", err)
	}
	u.authority.Close()
	if _, err = r.Admit(ctx, OutputOperationRead); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err = r.ReleaseJoined(); !errors.Is(err, ErrNotReady) {
		t.Fatal("Close освободил незавершённые операции", err)
	}
	for _, slot := range slots {
		slot.Finish()
	}
	fresh.Finish()
	r.Seal()
	if err = r.ReleaseJoined(); err != nil {
		t.Fatal("освобождение после Close не завершилось", err)
	}
}

func TestOutputExecutionObserverReplacementAndAmbiguityHold(t *testing.T) {
	for _, mode := range []string{"replacement", "close", "unreadable-reconcile"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s, u, l, f, snapshot := executionFixture(t)
			r := reserveExecution(t, s, l, snapshot)
			o := activateExecution(t, s, r, f)
			views := u.views
			operation, err := r.Admit(ctx, OutputOperationRead)
			if err != nil {
				t.Fatal(err)
			}
			operation.Finish()
			if u.views != views {
				t.Fatal("Admit вызвал View")
			}
			switch mode {
			case "replacement":
				if _, err = s.OpenOutputObserver(ctx, f); err != nil {
					t.Fatal(err)
				}
			case "close":
				if err = o.Close(ctx); err != nil {
					t.Fatal(err)
				}
			case "unreadable-reconcile":
				ambiguous := errors.New("неоднозначный COMMIT")
				err = u.authority.GuardWrite(ctx, u, time.Second, func() (bool, error) { u.viewErr = errors.New("read failure"); return true, ambiguous })
				if !errors.Is(err, ambiguous) {
					t.Fatal(err)
				}
				if err = s.ConfirmOutputExecutionResolution(ctx, r); err == nil {
					t.Fatal("ошибка View сняла hold")
				}
				u.viewErr = nil
			}
			select {
			case <-r.Done():
			default:
				t.Fatal("исполнение сохранило допуск")
			}
			if _, err = r.Admit(ctx, OutputOperationWrite); !errors.Is(err, ErrStale) {
				t.Fatal(err)
			}
			if err = s.ConfirmOutputExecutionResolution(ctx, r); !errors.Is(err, ErrNotReady) {
				t.Fatal("отсутствие наблюдателя разрешило освобождение", err)
			}
			if err = r.ReleaseJoined(); !errors.Is(err, ErrNotReady) {
				t.Fatal(err)
			}
			fresh, err := s.BindExecutionOwner(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ReserveOutputExecution(ctx, fresh, snapshot); !errors.Is(err, ErrInUse) {
				t.Fatal(err)
			}
			deauthorizeExecution(u, f)
			if err = s.ConfirmOutputExecutionResolution(ctx, r); err != nil {
				t.Fatal(err)
			}
			if err = r.ReleaseJoined(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputExecutionPreparedFailureAndResolutionReadErrors(t *testing.T) {
	ctx := context.Background()
	s, u, l, f, snapshot := executionFixture(t)
	c := u.repository.receipts[f.CommandID]
	c.Status = CommandIssued
	u.repository.receipts[f.CommandID] = c
	r := reserveExecution(t, s, l, snapshot)
	if _, err := s.OpenOutputObserver(ctx, f); !errors.Is(err, ErrStale) {
		t.Fatal("до Complete открыт OutputObserver", err)
	}
	r.Seal()
	r.Seal()
	c.Status = CommandFailed
	u.repository.receipts[f.CommandID] = c
	readErr := errors.New("не удалось прочитать persisted outcome")
	u.viewErr = readErr
	if err := s.ConfirmOutputExecutionResolution(ctx, r); !errors.Is(err, readErr) {
		t.Fatal(err)
	}
	if err := r.ReleaseJoined(); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	u.viewErr = nil
	if err := s.ConfirmOutputExecutionResolution(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := r.ReleaseJoined(); err != nil {
		t.Fatal(err)
	}
}

func TestOutputExecutionBudgetsOverflowAndCanceledAdmission(t *testing.T) {
	for _, modify := range []func(*OutputExecutionLimits){
		func(l *OutputExecutionLimits) { l.MaxReservations = 0 }, func(l *OutputExecutionLimits) { l.MaxIdentifierBytes = 0 }, func(l *OutputExecutionLimits) { l.MaxBytes = 0 },
		func(l *OutputExecutionLimits) { l.MaxIdentifierBytes = math.MaxUint64; l.MaxBytes = math.MaxUint64 }, func(l *OutputExecutionLimits) { l.MaxReservations = math.MaxInt },
	} {
		limits := OutputExecutionLimits{MaxReservations: 8, MaxIdentifierBytes: 1024, MaxBytes: 65536}
		modify(&limits)
		if err := limits.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatal(limits, err)
		}
	}
	for _, mode := range []string{"bytes", "identifier", "identity"} {
		t.Run(mode, func(t *testing.T) {
			s, u, l, _, snapshot := executionFixture(t)
			switch mode {
			case "bytes":
				u.authority.outputExecutionLimits.MaxBytes = 512
			case "identifier":
				u.authority.outputExecutionLimits.MaxIdentifierBytes = 1
			case "identity":
				u.authority.nextOutputExecution = math.MaxUint64
			}
			want := ErrOutputExecutionLimit
			if mode == "identity" {
				want = ErrConflict
			}
			if _, err := s.ReserveOutputExecution(context.Background(), l, snapshot); !errors.Is(err, want) {
				t.Fatal(err)
			}
			if len(u.authority.outputExecutions) != 0 || u.authority.outputExecutionBytes != 0 {
				t.Fatal("неуспешное резервирование заняло бюджет")
			}
		})
	}
	s, _, l, f, snapshot := executionFixture(t)
	r := reserveExecution(t, s, l, snapshot)
	activateExecution(t, s, r, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Admit(ctx, OutputOperationWrite); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := r.Admit(context.Background(), OutputOperationKind(255)); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestOutputExecutionConcurrentAdmissionSealAndFinish(t *testing.T) {
	s, _, l, f, snapshot := executionFixture(t)
	r := reserveExecution(t, s, l, snapshot)
	activateExecution(t, s, r, f)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 100 {
				op, err := r.Admit(context.Background(), OutputOperationWrite)
				if err == nil {
					op.Finish()
					op.Finish()
				} else if !errors.Is(err, ErrStale) && !errors.Is(err, ErrInUse) {
					t.Error(err)
				}
			}
		})
	}
	wg.Go(r.Seal)
	wg.Wait()
	if _, err := r.Admit(context.Background(), OutputOperationWrite); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
}

func TestOutputExecutionFenceStringsAreDetached(t *testing.T) {
	s, u, l, f, snapshot := executionFixture(t)
	huge := strings.Repeat("x", 1<<20)
	f.OutputID = ID(huge[20:30])
	c := u.repository.receipts[l.execution.CommandID]
	c.OutputID = f.OutputID
	u.repository.receipts[c.ID] = c
	output := u.repository.outputs[l.execution.OutputID]
	output.ID = f.OutputID
	u.repository.outputs[f.OutputID] = output
	fresh, err := s.BindExecutionOwner(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	r := reserveExecution(t, s, fresh, snapshot)
	if unsafe.StringData(string(r.fence.OutputID)) == unsafe.StringData(string(f.OutputID)) {
		t.Fatal("короткий ID удерживает исходный буфер")
	}
	if r.fence.OutputID != f.OutputID {
		t.Fatal("клонирование изменило ID")
	}
}

func TestOutputExecutionCountBudgetIncludesPreparedSealedAndJoinPending(t *testing.T) {
	ctx := context.Background()
	s, u, l, f, snapshot := executionFixture(t)
	u.authority.outputExecutionLimits.MaxReservations = 1
	first := reserveExecution(t, s, l, snapshot)
	otherFence := f
	otherFence.CommandID = "other-command"
	otherFence.OutputID = "other-output"
	receipt := u.repository.receipts[f.CommandID]
	receipt.ID = otherFence.CommandID
	receipt.OutputID = otherFence.OutputID
	u.repository.receipts[receipt.ID] = receipt
	output := u.repository.outputs[f.OutputID]
	output.ID = otherFence.OutputID
	u.repository.outputs[output.ID] = output
	otherLease, err := s.BindExecutionOwner(ctx, otherFence)
	if err != nil {
		t.Fatal(err)
	}
	checkFull := func() {
		t.Helper()
		if _, err := s.ReserveOutputExecution(ctx, otherLease, snapshot); !errors.Is(err, ErrOutputExecutionLimit) {
			t.Fatal("обойдён бюджет числа исполнений", err)
		}
	}
	checkFull()
	activateExecution(t, s, first, f)
	op, err := first.Admit(ctx, OutputOperationEstablishment)
	if err != nil {
		t.Fatal(err)
	}
	first.Seal()
	checkFull()
	deauthorizeExecution(u, f)
	if err = s.ConfirmOutputExecutionResolution(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err = first.ReleaseJoined(); !errors.Is(err, ErrNotReady) {
		t.Fatal("незавершённое подключение обошло удержание", err)
	}
	checkFull()
	op.Finish()
	if err = first.ReleaseJoined(); err != nil {
		t.Fatal(err)
	}
	second := reserveExecution(t, s, otherLease, snapshot)
	if second.token <= first.token {
		t.Fatal("идентичность переиспользована")
	}
	if err = first.ReleaseJoined(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReserveOutputExecution(ctx, otherLease, snapshot); !errors.Is(err, ErrInUse) {
		t.Fatal("старый ReleaseJoined удалил новую резервацию", err)
	}
}

func TestOutputExecutionResolutionRejectsTemporaryNotReadyAndForeignTokens(t *testing.T) {
	ctx := context.Background()
	s, u, l, f, snapshot := executionFixture(t)
	r := reserveExecution(t, s, l, snapshot)
	r.Seal()
	for _, state := range []CommandStatus{CommandIssued, CommandSucceeded} {
		c := u.repository.receipts[f.CommandID]
		c.Status = state
		u.repository.receipts[c.ID] = c
		if err := s.ConfirmOutputExecutionResolution(ctx, r); !errors.Is(err, ErrNotReady) {
			t.Fatal("пригодное исполнение разрешено к освобождению", err)
		}
	}
	u.viewErr = ErrNotReady
	if err := s.ConfirmOutputExecutionResolution(ctx, r); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	if r.releaseEligible {
		t.Fatal("временная ошибка допуска стала доказательством")
	}
	u.viewErr = nil
	foreign, _, _, _, _ := executionFixture(t)
	if err := foreign.ConfirmOutputExecutionResolution(ctx, r); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	copied := *r
	if err := s.ConfirmOutputExecutionResolution(ctx, &copied); !errors.Is(err, ErrStale) {
		t.Fatal("копия непрозрачного владельца принята", err)
	}
	delete(u.repository.outputs, f.OutputID)
	if err := s.ConfirmOutputExecutionResolution(ctx, r); err != nil {
		t.Fatal("подтверждённое отсутствие выхода не разрешило освобождение", err)
	}
	if err := r.ReleaseJoined(); err != nil {
		t.Fatal(err)
	}
}

func TestOutputExecutionInvalidHandlesAndCanceledReservation(t *testing.T) {
	ctx := context.Background()
	s, _, l, _, snapshot := executionFixture(t)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ReserveOutputExecution(canceled, l, snapshot); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.ReserveOutputExecution(ctx, nil, snapshot); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	var zero OutputExecutionReservation
	zero.Seal()
	if err := zero.ReleaseJoined(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := zero.Admit(ctx, OutputOperationWrite); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := zero.Activate(ctx, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if zero.Done() != nil {
		t.Fatal("нулевой владелец содержит Done")
	}
	var operation *OutputOperation
	operation.Finish()
}
