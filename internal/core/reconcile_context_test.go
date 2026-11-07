package core

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

type reconcileContextKey struct{}

type reconcileContextUOW struct {
	*observationAcceptanceUOW
	artifacts  ArtifactRepository
	beforeView func(context.Context, int) error
	contexts   []context.Context
}

func (u *reconcileContextUOW) View(ctx context.Context, fn func(Repositories) error) error {
	u.contexts = append(u.contexts, ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if u.beforeView != nil {
		if err := u.beforeView(ctx, len(u.contexts)); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return u.observationAcceptanceUOW.View(ctx, func(r Repositories) error {
		if u.artifacts != nil {
			r.Artifacts = u.artifacts
		}
		return fn(r)
	})
}

func runReconcileGuard(owned bool, ctx context.Context, u *reconcileContextUOW, owner *OutputObserver, budget time.Duration, write func() (bool, error)) error {
	if owned {
		return u.authority.GuardOwnedWrite(ctx, u, owner, budget, write)
	}
	return u.authority.GuardWrite(ctx, u, budget, write)
}

func openReconcileObservers(t *testing.T, s *Service, parent, child ExecutionFence) (*SessionObserver, *OutputObserver) {
	t.Helper()
	so, err := s.OpenSessionObserver(context.Background(), parent)
	if err != nil {
		t.Fatal(err)
	}
	oo, err := s.OpenOutputObserver(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	if err := so.Observe(context.Background(), SessionObservation{State: ObservationReady}); err != nil {
		t.Fatal(err)
	}
	if err := oo.Observe(context.Background(), OutputObservation{State: OutputPublishing}); err != nil {
		t.Fatal(err)
	}
	return so, oo
}

func TestReconcileCallerCancellationPreservesOnlyVerifiedOwners(t *testing.T) {
	for _, owned := range []bool{false, true} {
		guardName := map[bool]string{false: "write", true: "owned"}[owned]
		for _, outcome := range []string{"committed", "ambiguous", "changed_fence", "read_failure", "rollback"} {
			t.Run(guardName+"/"+outcome, func(t *testing.T) {
				s, base, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
				so, oo := openReconcileObservers(t, s, parent, child)
				before, err := s.RuntimeSnapshot(context.Background(), observationSessionFence(parent))
				if err != nil {
					t.Fatal(err)
				}
				u := &reconcileContextUOW{observationAcceptanceUOW: base}
				value := new(int)
				ctx, cancel := context.WithCancel(context.WithValue(context.Background(), reconcileContextKey{}, value))
				defer cancel()
				u.beforeView = func(readCtx context.Context, _ int) error {
					if readCtx.Value(reconcileContextKey{}) != value {
						t.Error("сверка потеряла значение контекста")
					}
					if deadline, ok := readCtx.Deadline(); !ok || !deadline.After(time.Now()) {
						t.Error("сверка не получила свежий конечный бюджет")
					}
					return nil
				}
				failure := errors.New("неоднозначная запись")
				var writeErr error
				if outcome == "ambiguous" || outcome == "rollback" {
					writeErr = failure
				}
				calls := 0
				err = runReconcileGuard(owned, ctx, u, oo, time.Second, func() (bool, error) {
					calls++
					if outcome == "changed_fence" {
						base.repository.session.Lifecycle++
					}
					if outcome == "read_failure" || outcome == "rollback" {
						base.viewErr = errors.New("чтение недоступно")
					}
					cancel()
					return outcome != "rollback", writeErr
				})
				if err != writeErr || calls != 1 || !errors.Is(ctx.Err(), context.Canceled) {
					t.Fatalf("результат или число записей изменились: err=%v calls=%d caller=%v", err, calls, ctx.Err())
				}
				wantValid := outcome != "changed_fence" && outcome != "read_failure"
				if so.Valid() != wantValid || oo.Valid() != wantValid {
					t.Fatalf("неверный допуск после %s: session=%t output=%t", outcome, so.Valid(), oo.Valid())
				}
				after, snapshotErr := s.RuntimeSnapshot(context.Background(), observationSessionFence(parent))
				if wantValid {
					if snapshotErr != nil || !reflect.DeepEqual(before, after) {
						t.Fatalf("неизменённое выполнение потеряло RAM: %v", snapshotErr)
					}
				} else if !errors.Is(snapshotErr, ErrNotReady) {
					t.Fatalf("неподтверждённый владелец сохранил снимок: %v", snapshotErr)
				}
				if outcome == "rollback" {
					if len(u.contexts) != 0 {
						t.Fatal("подтверждённый rollback запустил сверку")
					}
				} else if len(u.contexts) == 0 {
					t.Fatal("возможный COMMIT не проверен")
				}
				for _, readCtx := range u.contexts {
					select {
					case <-readCtx.Done():
					default:
						t.Error("контекст завершённой сверки не освобождён")
					}
				}
			})
		}
	}
}

func TestReconcileStartsItsBudgetAfterWrite(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "owned"}[owned], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, base, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
				so, oo := openReconcileObservers(t, s, parent, child)
				u := &reconcileContextUOW{observationAcceptanceUOW: base}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				calls := 0
				err := runReconcileGuard(owned, ctx, u, oo, time.Second, func() (bool, error) {
					calls++
					<-ctx.Done()
					return true, ctx.Err()
				})
				if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || len(u.contexts) == 0 || !so.Valid() || !oo.Valid() {
					t.Fatalf("запись исчерпала бюджет последующей сверки: err=%v calls=%d reads=%d session=%t output=%t", err, calls, len(u.contexts), so.Valid(), oo.Valid())
				}
			})
		})
	}
}

func TestReconcileRejectsInvalidBudgetAndCanceledAdmission(t *testing.T) {
	for _, owned := range []bool{false, true} {
		for _, budget := range []time.Duration{-time.Second, 0, time.Second} {
			t.Run(map[bool]string{false: "write", true: "owned"}[owned]+"/"+budget.String(), func(t *testing.T) {
				s, base, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
				so, oo := openReconcileObservers(t, s, parent, child)
				u := &reconcileContextUOW{observationAcceptanceUOW: base}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				calls := 0
				err := runReconcileGuard(owned, ctx, u, oo, budget, func() (bool, error) { calls++; return true, nil })
				want := error(context.Canceled)
				if budget <= 0 {
					want = ErrInvalid
				}
				if !errors.Is(err, want) || calls != 0 || len(u.contexts) != 0 || !so.Valid() || !oo.Valid() {
					t.Fatalf("отклонённый запрос изменил выполнение: err=%v calls=%d reads=%d", err, calls, len(u.contexts))
				}
			})
		}
	}
}

func TestReconcileOwnedWriteRejectsReplacedObserver(t *testing.T) {
	s, base, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
	so, old := openReconcileObservers(t, s, parent, child)
	fresh, err := s.OpenOutputObserver(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	u := &reconcileContextUOW{observationAcceptanceUOW: base}
	calls := 0
	err = runReconcileGuard(true, context.Background(), u, old, time.Second, func() (bool, error) { calls++; return true, nil })
	if !errors.Is(err, ErrStale) || calls != 0 || len(u.contexts) != 0 || old.Valid() || !fresh.Valid() || !so.Valid() {
		t.Fatalf("прежний наблюдатель затронул новое выполнение: err=%v calls=%d reads=%d", err, calls, len(u.contexts))
	}
}

func TestReconcilePanicPreservesOutcomeAndReleasesAdmission(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "owned"}[owned], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, base, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
				so, oo := openReconcileObservers(t, s, parent, child)
				u := &reconcileContextUOW{observationAcceptanceUOW: base}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				panicValue := errors.New("исходный panic записи")
				var recovered any
				calls := 0
				func() {
					defer func() { recovered = recover() }()
					_ = runReconcileGuard(owned, ctx, u, oo, time.Second, func() (bool, error) {
						calls++
						cancel()
						panic(panicValue)
					})
				}()
				if recovered != panicValue || calls != 1 || len(u.contexts) == 0 || !so.Valid() || !oo.Valid() {
					t.Fatalf("panic изменён или нарушил свежую сверку: panic=%v calls=%d reads=%d session=%t output=%t", recovered, calls, len(u.contexts), so.Valid(), oo.Valid())
				}
				next, cancelNext := context.WithTimeout(context.Background(), time.Second)
				defer cancelNext()
				admitted := false
				err := runReconcileGuard(owned, next, u, oo, time.Second, func() (bool, error) { admitted = true; return false, nil })
				if err != nil || !admitted {
					t.Fatal("panic не освободил общий допуск", err)
				}
			})
		})
	}
}

type reconcilePreparationArtifacts struct {
	ArtifactRepository
	generation FallbackGeneration
}

func (r *reconcilePreparationArtifacts) Generation(context.Context, ID) (FallbackGeneration, error) {
	return r.generation, nil
}
func (r *reconcilePreparationArtifacts) Generations(_ context.Context, q ListQuery) ([]FallbackGeneration, error) {
	if q.State == string(r.generation.State) && q.Incarnation == r.generation.Incarnation {
		return []FallbackGeneration{r.generation}, nil
	}
	return nil, nil
}
func (*reconcilePreparationArtifacts) Cleanups(context.Context, ListQuery) ([]CleanupIntent, error) {
	return nil, nil
}

type reconcilePreparationRuntime struct {
	RuntimeRepository
	repository *observationAcceptanceRepository
}

func (r reconcilePreparationRuntime) Sessions(context.Context, ListQuery) ([]Session, error) {
	return []Session{r.repository.session}, nil
}
func (r reconcilePreparationRuntime) Outputs(context.Context, ListQuery) ([]SessionOutput, error) {
	outputs := make([]SessionOutput, 0, len(r.repository.outputs))
	for _, output := range r.repository.outputs {
		outputs = append(outputs, output)
	}
	return outputs, nil
}

func TestReconcileSharesOneBudgetAcrossPreparationsAndLeases(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "owned"}[owned], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, base, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
				base.repository.RuntimeRepository = reconcilePreparationRuntime{repository: &base.repository}
				artifacts := &reconcilePreparationArtifacts{generation: FallbackGeneration{Record: Record{ID: "old-generation"}, AssetID: "asset", Incarnation: "owner", AttemptToken: "attempt", Attempt: 1, State: PreparationIssued, Issued: true}}
				u := &reconcileContextUOW{observationAcceptanceUOW: base, artifacts: artifacts}
				s, err := NewService(u, "owner")
				if err != nil {
					t.Fatal(err)
				}
				currentSession, currentOutputs := base.repository.session, base.repository.outputs
				base.repository.session.State, base.repository.session.Issued = SessionStopped, false
				base.repository.outputs = nil
				preparation, err := s.BindPreparationOwner(context.Background(), PreparationFenceFor(artifacts.generation))
				if err != nil {
					t.Fatal(err)
				}
				artifacts.generation.State, artifacts.generation.CleanupPending = PreparationFailed, true
				if err := base.authority.GuardWrite(context.Background(), u, time.Second, func() (bool, error) { return true, nil }); err != nil || preparation.Valid() {
					t.Fatal("терминальная подготовка не отозвана", err)
				}
				base.repository.session, base.repository.outputs = currentSession, currentOutputs
				so, oo := openReconcileObservers(t, s, parent, child)
				u.contexts = nil
				var timedOut bool
				u.beforeView = func(ctx context.Context, n int) error {
					cost := 3 * time.Second / 4
					if n > 1 {
						cost = time.Second / 2
					}
					select {
					case <-ctx.Done():
						timedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
						return ctx.Err()
					case <-time.After(cost):
						return nil
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				err = runReconcileGuard(owned, ctx, u, oo, time.Second, func() (bool, error) { calls++; cancel(); return true, nil })
				if err != nil || calls != 1 || !timedOut || len(u.contexts) != 2 || so.Valid() || oo.Valid() {
					t.Fatalf("бюджет продлён или истечение срока сохранило допуск: err=%v calls=%d reads=%d timeout=%t session=%t output=%t", err, calls, len(u.contexts), timedOut, so.Valid(), oo.Valid())
				}
				first, firstOK := u.contexts[0].Deadline()
				second, secondOK := u.contexts[1].Deadline()
				if !firstOK || !secondOK || !first.Equal(second) {
					t.Fatal("подготовка и допуски получили разные сроки одной сверки")
				}
			})
		})
	}
}
