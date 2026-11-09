package core

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"
)

type observationAcceptanceRepository struct {
	RuntimeRepository
	session  Session
	receipts map[ID]CommandReceipt
	outputs  map[ID]SessionOutput
}

func (r *observationAcceptanceRepository) Session(context.Context, ID) (Session, error) {
	return r.session, nil
}
func (r *observationAcceptanceRepository) ReceiptByID(_ context.Context, id ID) (CommandReceipt, error) {
	c, ok := r.receipts[id]
	if !ok {
		return CommandReceipt{}, ErrNotFound
	}
	return c, nil
}
func (r *observationAcceptanceRepository) Output(_ context.Context, id ID) (SessionOutput, error) {
	o, ok := r.outputs[id]
	if !ok {
		return SessionOutput{}, ErrNotFound
	}
	return o, nil
}

type observationAcceptanceUOW struct {
	authority     *RuntimeAuthority
	repository    observationAcceptanceRepository
	views, writes int
	viewErr       error
}

func (u *observationAcceptanceUOW) RuntimeAuthority() *RuntimeAuthority { return u.authority }
func (u *observationAcceptanceUOW) View(ctx context.Context, fn func(Repositories) error) error {
	u.views++
	if u.viewErr != nil {
		return u.viewErr
	}
	return fn(Repositories{Runtime: &u.repository, Artifacts: emptyPreparationRepository{}})
}
func (u *observationAcceptanceUOW) Write(context.Context, func(Repositories) error) error {
	u.writes++
	return errors.New("неожиданная запись наблюдения")
}

func acceptanceObservationLimits() ObservationLimits {
	return ObservationLimits{MaxSessions: 2, MaxGroups: 4, MaxOutputs: 4, MaxFFmpegWorkers: 4, MaxIdentifierBytes: 128, MaxErrorBytes: 256, MaxBytes: 16384}
}

func newObservationAcceptanceService(t *testing.T, limits ObservationLimits) (*Service, *observationAcceptanceUOW, ExecutionFence, ExecutionFence) {
	t.Helper()
	a, err := NewRuntimeAuthority("owner", limits, OutputExecutionLimits{MaxReservations: 8, MaxIdentifierBytes: 1024, MaxBytes: 65536})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	parent := ExecutionFence{CommandID: "session-command", SessionID: "session", Incarnation: "owner", Lifecycle: 1, Attempt: 1}
	child := parent
	child.CommandID, child.OutputID, child.OutputGeneration = "output-command", "output", 1
	u := &observationAcceptanceUOW{authority: a, repository: observationAcceptanceRepository{
		session:  Session{ID: parent.SessionID, Incarnation: parent.Incarnation, Lifecycle: 1, State: SessionRunning, Issued: true},
		receipts: map[ID]CommandReceipt{}, outputs: map[ID]SessionOutput{},
	}}
	for _, f := range []ExecutionFence{parent, child} {
		kind := CommandStartSession
		if f.OutputID != "" {
			kind = CommandStartOutput
		}
		u.repository.receipts[f.CommandID] = CommandReceipt{ID: f.CommandID, Kind: kind, SessionID: f.SessionID, OutputID: f.OutputID, Incarnation: f.Incarnation, Lifecycle: f.Lifecycle, OutputGeneration: f.OutputGeneration, Attempt: f.Attempt, Status: CommandSucceeded}
	}
	u.repository.outputs[child.OutputID] = SessionOutput{ID: child.OutputID, SessionID: child.SessionID, DesiredRunning: true, Attached: true, Generation: 1, Attempt: 1, Issued: true}
	s, err := NewService(u, "owner")
	if err != nil {
		t.Fatal(err)
	}
	return s, u, parent, child
}

func observationSessionFence(f ExecutionFence) RuntimeSessionFence {
	return RuntimeSessionFence{SessionID: f.SessionID, Incarnation: f.Incarnation, Lifecycle: f.Lifecycle}
}

func TestAcceptanceObservationsNeverReadOrWritePersistence(t *testing.T) {
	ctx := context.Background()
	s, u, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
	so, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	oo, err := s.OpenOutputObserver(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	g, err := so.OpenGroup(ctx, "dynamic-group", "dynamic-profile")
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
	f := observationSessionFence(parent)
	empty, err := s.RuntimeSnapshot(ctx, f)
	if err != nil || empty.Session == nil || empty.Session.Value != nil || len(empty.Outputs) != 1 || empty.Outputs[0].Value != nil || empty.Groups[0].Value != nil || empty.FFmpeg[0].Value != nil {
		t.Fatalf("регистрация выдумала наблюдение: %+v %v", empty, err)
	}
	views := u.views
	u.viewErr = errors.New("хранилище недоступно")
	for _, err := range []error{
		so.Observe(ctx, SessionObservation{State: ObservationReady, Error: "текущая ошибка сессии"}),
		g.Observe(ctx, GroupObservation{State: ObservationRetrying, Source: ObservationSourceFallback, Error: "текущая ошибка группы"}),
		oo.Observe(ctx, OutputObservation{State: OutputRetrying, Error: "текущая ошибка выхода"}),
		a.Observe(ctx, FFmpegObservation{State: ObservationStarting, Error: "текущая ошибка FFmpeg"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.RuntimeSnapshot(ctx, f)
	if err != nil || got.Session.Value.State != ObservationReady || got.Groups[0].Value.Source != ObservationSourceFallback || got.Outputs[0].Value.State != OutputRetrying || got.FFmpeg[0].Stamp.AttemptID != 1 || got.FFmpeg[0].Value.State != ObservationStarting {
		t.Fatalf("наблюдения не опубликованы: %+v %v", got, err)
	}
	got.Session.Value.Error = "изменено вызывающим"
	got.Groups[0].ConfigGroupID = "изменено"
	got.Outputs[0].Value.State = OutputFailed
	got.FFmpeg[0].Value.Error = "изменено вызывающим"
	again, err := s.RuntimeSnapshot(ctx, f)
	if err != nil || again.Session.Value.Error != "текущая ошибка сессии" || again.Groups[0].ConfigGroupID != "dynamic-group" || again.Outputs[0].Value.State != OutputRetrying || again.FFmpeg[0].Value.Error != "текущая ошибка FFmpeg" {
		t.Fatal("snapshot удерживает изменяемый alias", err)
	}
	for _, invalid := range []OutputState{OutputFailed, OutputStopped, "UNKNOWN"} {
		if err := oo.Observe(ctx, OutputObservation{State: invalid}); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if u.views != views || u.writes != 0 {
		t.Fatalf("обычное наблюдение вызвало SQL: views=%d/%d writes=%d", u.views, views, u.writes)
	}
}

func TestAcceptanceObserverReplacementAndWorkerAttempts(t *testing.T) {
	ctx := context.Background()
	s, _, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
	so, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.OpenOutputObserver(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := s.OpenOutputObserver(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if old.Valid() || !fresh.Valid() {
		t.Fatal("replacement не отозвал точный старый token")
	}
	if err := old.Observe(ctx, OutputObservation{State: OutputPublishing}); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	if err := old.Close(ctx); err != nil || !fresh.Valid() {
		t.Fatal("старый Close удалил replacement", err)
	}
	g1, err := so.OpenGroup(ctx, "same-config", "same-profile")
	if err != nil {
		t.Fatal(err)
	}
	g2, err := so.OpenGroup(ctx, "same-config", "same-profile")
	if err != nil {
		t.Fatal(err)
	}
	w, err := g1.OpenFFmpeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := w.BeginAttempt(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Observe(ctx, FFmpegObservation{State: ObservationReady}); err != nil {
		t.Fatal(err)
	}
	second, err := w.BeginAttempt(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if first.Valid() || !second.Valid() {
		t.Fatal("первая попытка сохранила допуск")
	}
	select {
	case <-first.Done():
	default:
		t.Fatal("Done старой попытки не закрыт")
	}
	if err := first.Observe(ctx, FFmpegObservation{State: ObservationFailed}); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	if _, err := w.BeginAttempt(ctx, 2); !errors.Is(err, ErrStale) {
		t.Fatal("повтор attemptID принят", err)
	}
	if _, err := w.BeginAttempt(ctx, 0); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	snapshot, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || len(snapshot.Groups) != 2 || snapshot.Groups[0].Stamp.InstanceID == snapshot.Groups[1].Stamp.InstanceID || snapshot.FFmpeg[0].Value != nil || snapshot.FFmpeg[0].Stamp.AttemptID != 2 {
		t.Fatalf("потеря identity/absence: %+v %v", snapshot, err)
	}
	if err := g1.Close(ctx); err != nil || w.Valid() || second.Valid() || !g2.Valid() || !fresh.Valid() {
		t.Fatal("закрытие группы повредило соседа", err)
	}
	w2, err := g2.OpenFFmpeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w2.BeginAttempt(ctx, 1); err != nil {
		t.Fatal("новому worker запрещена первая попытка", err)
	}
	lease, err := s.BindExecutionOwner(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if so.Valid() || fresh.Valid() || g2.Valid() || w2.Valid() || !replacement.Valid() || !lease.Valid() {
		t.Fatal("Session replacement нарушил subtree либо физический lease")
	}
	if err := so.Close(ctx); err != nil || !replacement.Valid() {
		t.Fatal("старый Session Close удалил новый token", err)
	}
}

func TestAcceptanceObservationBudgetsAreAtomicAndDoNotBlockRetirement(t *testing.T) {
	ctx := context.Background()
	limits := acceptanceObservationLimits()
	limits.MaxGroups, limits.MaxFFmpegWorkers, limits.MaxOutputs = 1, 1, 1
	s, u, parent, child := newObservationAcceptanceService(t, limits)
	so, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	g, err := so.OpenGroup(ctx, "group", "profile")
	if err != nil {
		t.Fatal(err)
	}
	w, err := g.OpenFFmpeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := so.OpenGroup(ctx, "second", "profile"); !errors.Is(err, ErrObservationLimit) {
		t.Fatal(err)
	}
	if _, err := g.OpenFFmpeg(ctx); !errors.Is(err, ErrObservationLimit) {
		t.Fatal(err)
	}
	oo, err := s.OpenOutputObserver(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	backing := strings.Repeat("x", 1<<20)
	value := backing[512:544]
	if err := oo.Observe(ctx, OutputObservation{State: OutputRetrying, Error: value}); err != nil {
		t.Fatal(err)
	}
	if unsafe.StringData(oo.node.failure) == unsafe.StringData(value) {
		t.Fatal("короткая ошибка удерживает backing buffer вызывающего")
	}
	before, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil {
		t.Fatal(err)
	}
	if err := oo.Observe(ctx, OutputObservation{State: OutputPublishing, Error: strings.Repeat("e", int(limits.MaxErrorBytes)+1)}); !errors.Is(err, ErrObservationLimit) {
		t.Fatal(err)
	}
	after, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || !reflect.DeepEqual(before, after) || !oo.Valid() {
		t.Fatal("отказ бюджета частично изменил snapshot", err)
	}
	u.authority.nextObservation = math.MaxUint64
	if _, err := s.OpenOutputObserver(ctx, child); !errors.Is(err, ErrConflict) || !oo.Valid() {
		t.Fatal("переполнение token заменило владельца", err)
	}
	oo.node.stamp.Revision = math.MaxUint64
	if err := oo.Observe(ctx, OutputObservation{State: OutputPublishing}); !errors.Is(err, ErrConflict) || oo.node.failure != value {
		t.Fatal("переполнение revision изменило значение", err)
	}
	if err := so.Close(ctx); err != nil || oo.Valid() || w.Valid() {
		t.Fatal("лимиты помешали закрытию subtree", err)
	}
	if _, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent)); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
}

func TestAcceptanceOwnedWriteRollbackAndUncertainOutcome(t *testing.T) {
	for _, outcome := range []string{"rollback", "committed", "unreadable"} {
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			s, u, _, f := newObservationAcceptanceService(t, acceptanceObservationLimits())
			o, err := s.OpenOutputObserver(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			if err := o.Observe(ctx, OutputObservation{State: OutputPublishing}); err != nil {
				t.Fatal(err)
			}
			before, err := s.RuntimeSnapshot(ctx, observationSessionFence(f))
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("неоднозначный COMMIT")
			calls := 0
			err = u.authority.GuardOwnedWrite(ctx, u, o, time.Second, func() (bool, error) {
				calls++
				if outcome == "committed" {
					current := u.repository.outputs[f.OutputID]
					current.Generation++
					u.repository.outputs[f.OutputID] = current
				}
				if outcome == "unreadable" {
					u.viewErr = errors.New("не удалось проверить COMMIT")
				}
				return outcome != "rollback", failure
			})
			if !errors.Is(err, failure) || calls != 1 {
				t.Fatalf("результат подменён либо callback повторён: %v %d", err, calls)
			}
			after, snapshotErr := s.RuntimeSnapshot(ctx, observationSessionFence(f))
			if outcome == "rollback" {
				if snapshotErr != nil || !o.Valid() || !reflect.DeepEqual(before, after) {
					t.Fatal("подтверждённый rollback уничтожил RAM", snapshotErr)
				}
			} else if o.Valid() || !errors.Is(snapshotErr, ErrNotReady) {
				t.Fatal("неподтверждённый владелец сохранил RAM", snapshotErr)
			}
		})
	}
}

func TestAcceptanceObservationLimitsRejectZeroAndArithmeticOverflow(t *testing.T) {
	for _, change := range []func(*ObservationLimits){
		func(l *ObservationLimits) { l.MaxSessions = 0 }, func(l *ObservationLimits) { l.MaxGroups = 0 },
		func(l *ObservationLimits) { l.MaxOutputs = 0 }, func(l *ObservationLimits) { l.MaxFFmpegWorkers = 0 },
		func(l *ObservationLimits) { l.MaxIdentifierBytes = 0 }, func(l *ObservationLimits) { l.MaxErrorBytes = 0 },
		func(l *ObservationLimits) { l.MaxBytes = 0 }, func(l *ObservationLimits) { l.MaxErrorBytes = math.MaxUint64; l.MaxBytes = math.MaxUint64 },
		func(l *ObservationLimits) {
			l.MaxSessions = math.MaxInt
			l.MaxGroups = math.MaxInt
			l.MaxOutputs = math.MaxInt
			l.MaxFFmpegWorkers = math.MaxInt
		},
	} {
		limits := acceptanceObservationLimits()
		change(&limits)
		if _, err := NewRuntimeAuthority("owner", limits, OutputExecutionLimits{MaxReservations: 8, MaxIdentifierBytes: 1024, MaxBytes: 65536}); !errors.Is(err, ErrInvalid) {
			t.Fatal("некорректные ограничения приняты", limits, err)
		}
	}
}

func TestAcceptanceOutputFirstCountsSessionAndByteBudget(t *testing.T) {
	ctx := context.Background()
	limits := acceptanceObservationLimits()
	limits.MaxSessions = 1
	s, u, _, f := newObservationAcceptanceService(t, limits)
	o, err := s.OpenOutputObserver(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	other := f
	other.SessionID, other.CommandID, other.OutputID = "other-session", "other-command", "other-output"
	u.repository.session.ID = other.SessionID
	c := u.repository.receipts[f.CommandID]
	c.ID, c.SessionID, c.OutputID = other.CommandID, other.SessionID, other.OutputID
	u.repository.receipts[c.ID] = c
	output := u.repository.outputs[f.OutputID]
	output.ID, output.SessionID = other.OutputID, other.SessionID
	u.repository.outputs[output.ID] = output
	if _, err := s.OpenOutputObserver(ctx, other); !errors.Is(err, ErrObservationLimit) || !o.Valid() {
		t.Fatal("Output-first обошёл MaxSessions или повредил владельца", err)
	}

	limits = acceptanceObservationLimits()
	limits.MaxBytes = 500
	s, _, _, f = newObservationAcceptanceService(t, limits)
	o, err = s.OpenOutputObserver(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Observe(ctx, OutputObservation{State: OutputRetrying, Error: "сохранённая ошибка"}); err != nil {
		t.Fatal(err)
	}
	before, err := s.RuntimeSnapshot(ctx, observationSessionFence(f))
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Observe(ctx, OutputObservation{State: OutputPublishing, Error: strings.Repeat("e", 256)}); !errors.Is(err, ErrObservationLimit) {
		t.Fatal("MaxBytes не ограничил допустимую по MaxErrorBytes строку", err)
	}
	after, err := s.RuntimeSnapshot(ctx, observationSessionFence(f))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("переполнение byte budget частично изменило снимок", err)
	}
	replacement, err := s.OpenOutputObserver(ctx, f)
	if err != nil || !replacement.Valid() || o.Valid() {
		t.Fatal("replacement ошибочно учтён дважды в byte budget", err)
	}
	if err := replacement.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptanceExecutionFailureEventRequiresMatchingCompleteFence(t *testing.T) {
	fence := ExecutionFence{CommandID: "command", SessionID: "session", OutputID: "output", Incarnation: "owner", Lifecycle: 1, OutputGeneration: 1, Attempt: 1}
	event := Event{ID: "event", SessionID: fence.SessionID, OutputID: fence.OutputID, Kind: "output_execution_failed", Message: "Итоговый отказ", Execution: &fence, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Event){
		func(e *Event) { e.Execution = nil }, func(e *Event) { e.SessionID = "other" },
		func(e *Event) { e.OutputID = "other" }, func(e *Event) { e.Kind = "command_completed" },
		func(e *Event) { e.Execution.Attempt = 0 }, func(e *Event) { e.Execution.Lifecycle = 0 },
		func(e *Event) { e.Execution.OutputGeneration = 0 }, func(e *Event) { e.Execution.CommandID = "" },
		func(e *Event) { e.Execution.Incarnation = "" },
	} {
		candidate, copiedFence := event, fence
		candidate.Execution = &copiedFence
		change(&candidate)
		if err := candidate.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatal("неполный или чужой fence принят в истории отказа", candidate, err)
		}
	}
}

type emptyPreparationRepository struct{ ArtifactRepository }

func (emptyPreparationRepository) Generations(context.Context, ListQuery) ([]FallbackGeneration, error) {
	return nil, nil
}
