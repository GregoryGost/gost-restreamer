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

// observationAcceptanceRepository предоставляет сохраняемое управление тестовой сессией, квитанциями и
// выходами в памяти.
type observationAcceptanceRepository struct {
	// RuntimeRepository сохраняет неиспользуемую часть контракта RuntimeRepository для узкого
	// тестового двойника.
	RuntimeRepository
	// session содержит заданную сохраняемую сессию для проверки допуска наблюдений.
	session Session
	// receipts хранит тестовые квитанции по внутренним ID.
	receipts map[ID]CommandReceipt
	// outputs хранит тестовые выходы по ID.
	outputs map[ID]SessionOutput
}

// Возвращает заданную сессию для проверки владельца оперативных наблюдений.
func (r *observationAcceptanceRepository) Session(context.Context, ID) (Session, error) {
	return r.session, nil
}

// Возвращает тестовую квитанцию по точному ID либо ErrNotFound.
func (r *observationAcceptanceRepository) ReceiptByID(_ context.Context, id ID) (CommandReceipt, error) {
	// c содержит найденную тестовую квитанцию, а ok показывает её наличие.
	c, ok := r.receipts[id]
	if !ok {
		return CommandReceipt{}, ErrNotFound
	}
	return c, nil
}

// Возвращает тестовый выход по точному ID либо ErrNotFound.
func (r *observationAcceptanceRepository) Output(_ context.Context, id ID) (SessionOutput, error) {
	// o содержит найденный тестовый выход, а ok показывает его наличие.
	o, ok := r.outputs[id]
	if !ok {
		return SessionOutput{}, ErrNotFound
	}
	return o, nil
}

// observationAcceptanceUOW предоставляет общую RuntimeAuthority и контролируемое чтение сохраняемого
// управления в тестах.
type observationAcceptanceUOW struct {
	// authority содержит общий реестр допусков и оперативных наблюдений.
	authority *RuntimeAuthority
	// repository содержит изменяемое управление сессией, квитанциями и выходами теста.
	repository observationAcceptanceRepository
	// views считает обращения к согласованному чтению, а writes — попытки записи постоянных данных.
	views, writes int
	// viewErr задаёт контролируемый отказ чтения сохраняемого управления.
	viewErr error
}

// Возвращает общий тестовый реестр допусков и наблюдений.
func (u *observationAcceptanceUOW) RuntimeAuthority() *RuntimeAuthority { return u.authority }

// Учитывает чтение и передаёт тестовые репозитории либо возвращает заданную ошибку недоступности
// хранения.
func (u *observationAcceptanceUOW) View(ctx context.Context, fn func(Repositories) error) error {
	u.views++
	if u.viewErr != nil {
		return u.viewErr
	}
	return fn(Repositories{Runtime: &u.repository, Artifacts: emptyPreparationRepository{}})
}

// Учитывает и отклоняет запись, чтобы тест обнаруживал обращение обычного наблюдения к постоянному
// хранению.
func (u *observationAcceptanceUOW) Write(context.Context, func(Repositories) error) error {
	u.writes++
	return errors.New("неожиданная запись наблюдения")
}

// Задаёт конечные тестовые бюджеты владельцев, строк и логических байтов наблюдений.
func acceptanceObservationLimits() ObservationLimits {
	return ObservationLimits{MaxSessions: 2, MaxGroups: 4, MaxOutputs: 4, MaxFFmpegWorkers: 4, MaxIdentifierBytes: 128, MaxErrorBytes: 256, MaxBytes: 16384}
}

// Создаёт Service с общей RuntimeAuthority, успешными квитанциями Session/Output и выданным выходом;
// закрытие реестра регистрируется в очистке теста.
func newObservationAcceptanceService(t *testing.T, limits ObservationLimits) (*Service, *observationAcceptanceUOW, ExecutionFence, ExecutionFence) {
	t.Helper()
	// a содержит общий реестр с явными бюджетами наблюдений и физических исполнений. err сообщает об
	// ошибке создания общего реестра с явными бюджетами.
	a, err := NewRuntimeAuthority("owner", limits, OutputExecutionLimits{MaxReservations: 8, MaxIdentifierBytes: 1024, MaxBytes: 65536})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	// parent идентифицирует точное выполнение родительской сессии.
	parent := ExecutionFence{CommandID: "session-command", SessionID: "session", Incarnation: "owner", Lifecycle: 1, Attempt: 1}
	// child идентифицирует точную попытку дочернего выхода.
	child := parent
	child.CommandID, child.OutputID, child.OutputGeneration = "output-command", "output", 1
	// u предоставляет изменяемые тестовые репозитории и учитывает обращения к БД.
	u := &observationAcceptanceUOW{authority: a, repository: observationAcceptanceRepository{
		session:  Session{ID: parent.SessionID, Incarnation: parent.Incarnation, Lifecycle: 1, State: SessionRunning, Issued: true},
		receipts: map[ID]CommandReceipt{}, outputs: map[ID]SessionOutput{},
	}}
	// f содержит очередную точную попытку Session или Output для создания успешной квитанции.
	for _, f := range []ExecutionFence{parent, child} {
		// kind выбирает start_session или start_output по наличию OutputID.
		kind := CommandStartSession
		if f.OutputID != "" {
			kind = CommandStartOutput
		}
		u.repository.receipts[f.CommandID] = CommandReceipt{ID: f.CommandID, Kind: kind, SessionID: f.SessionID, OutputID: f.OutputID, Incarnation: f.Incarnation, Lifecycle: f.Lifecycle, OutputGeneration: f.OutputGeneration, Attempt: f.Attempt, Status: CommandSucceeded}
	}
	u.repository.outputs[child.OutputID] = SessionOutput{ID: child.OutputID, SessionID: child.SessionID, DesiredRunning: true, Attached: true, Generation: 1, Attempt: 1, Issued: true}
	// s предоставляет доменные операции с общим реестром наблюдений.
	s, err := NewService(u, "owner")
	if err != nil {
		t.Fatal(err)
	}
	return s, u, parent, child
}

// Извлекает точного владельца Session из тестового ExecutionFence.
func observationSessionFence(f ExecutionFence) RuntimeSessionFence {
	return RuntimeSessionFence{SessionID: f.SessionID, Incarnation: f.Incarnation, Lifecycle: f.Lifecycle}
}

// Проверяет отсутствие выдуманных значений при регистрации и независимость RAM-наблюдений от
// недоступной БД, включая копирование возвращённого снимка.
func TestAcceptanceObservationsNeverReadOrWritePersistence(t *testing.T) {
	// ctx задаёт контекст операций проверяемого сценария оперативных наблюдений.
	ctx := context.Background()
	// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет изменяемые
	// тестовые репозитории и учитывает обращения к БД. parent идентифицирует точное выполнение
	// родительской сессии. child идентифицирует точную попытку дочернего выхода.
	s, u, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
	// so содержит наблюдателя текущего выполнения сессии. err сообщает об ошибке регистрации
	// наблюдателя точной сессии.
	so, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	// oo содержит наблюдателя точной попытки выхода.
	oo, err := s.OpenOutputObserver(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	// g содержит наблюдателя отдельной оперативной группы.
	g, err := so.OpenGroup(ctx, "dynamic-group", "dynamic-profile")
	if err != nil {
		t.Fatal(err)
	}
	// w содержит наблюдателя работника FFmpeg внутри группы.
	w, err := g.OpenFFmpeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// a содержит наблюдателя первой попытки FFmpeg.
	a, err := w.BeginAttempt(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	// f фиксирует точного владельца сессии для чтения RAM-снимка.
	f := observationSessionFence(parent)
	// empty содержит снимок сразу после регистрации до первого принятого наблюдения.
	empty, err := s.RuntimeSnapshot(ctx, f)
	if err != nil || empty.Session == nil || empty.Session.Value != nil || len(empty.Outputs) != 1 || empty.Outputs[0].Value != nil || empty.Groups[0].Value != nil || empty.FFmpeg[0].Value != nil {
		t.Fatalf("регистрация выдумала наблюдение: %+v %v", empty, err)
	}
	// views сохраняет число чтений БД перед проверяемой операцией RAM.
	views := u.views
	u.viewErr = errors.New("хранилище недоступно")
	// err сообщает об ошибке очередной записи исходного оперативного состояния.
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
	// got содержит снимок после записи всех оперативных состояний при недоступной БД.
	got, err := s.RuntimeSnapshot(ctx, f)
	if err != nil || got.Session.Value.State != ObservationReady || got.Groups[0].Value.Source != ObservationSourceFallback || got.Outputs[0].Value.State != OutputRetrying || got.FFmpeg[0].Stamp.AttemptID != 1 || got.FFmpeg[0].Value.State != ObservationStarting {
		t.Fatalf("наблюдения не опубликованы: %+v %v", got, err)
	}
	got.Session.Value.Error = "изменено вызывающим"
	got.Groups[0].ConfigGroupID = "изменено"
	got.Outputs[0].Value.State = OutputFailed
	got.FFmpeg[0].Value.Error = "изменено вызывающим"
	// again содержит повторный снимок после изменения возвращённой копии.
	again, err := s.RuntimeSnapshot(ctx, f)
	if err != nil || again.Session.Value.Error != "текущая ошибка сессии" || again.Groups[0].ConfigGroupID != "dynamic-group" || again.Outputs[0].Value.State != OutputRetrying || again.FFmpeg[0].Value.Error != "текущая ошибка FFmpeg" {
		t.Fatal("snapshot удерживает изменяемый alias", err)
	}
	// invalid задаёт состояние, запрещённое для обычного Observe выхода.
	for _, invalid := range []OutputState{OutputFailed, OutputStopped, "UNKNOWN"} {
		// err сообщает об ошибке записи оперативного состояния точного владельца.
		if err := oo.Observe(ctx, OutputObservation{State: invalid}); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if u.views != views || u.writes != 0 {
		t.Fatalf("обычное наблюдение вызвало SQL: views=%d/%d writes=%d", u.views, views, u.writes)
	}
}

// Проверяет отзыв прежних идентификаторов и попыток, самостоятельные идентичности групп и изоляцию
// соседей при закрытии и замене наблюдателей.
func TestAcceptanceObserverReplacementAndWorkerAttempts(t *testing.T) {
	// ctx задаёт контекст операций проверяемого сценария оперативных наблюдений.
	ctx := context.Background()
	// s предоставляет доменные операции с общим реестром наблюдений. parent идентифицирует точное
	// выполнение родительской сессии. child идентифицирует точную попытку дочернего выхода.
	s, _, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
	// so содержит наблюдателя текущего выполнения сессии. err сообщает об ошибке регистрации
	// наблюдателя точной сессии.
	so, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	// old содержит прежнего наблюдателя выхода до замены token.
	old, err := s.OpenOutputObserver(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	// fresh содержит нового наблюдателя выхода после замены token.
	fresh, err := s.OpenOutputObserver(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if old.Valid() || !fresh.Valid() {
		t.Fatal("replacement не отозвал точный старый token")
	}
	// err сообщает об ошибке записи оперативного состояния точного владельца.
	if err := old.Observe(ctx, OutputObservation{State: OutputPublishing}); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	// err сообщает об ошибке закрытия точного владельца наблюдений.
	if err := old.Close(ctx); err != nil || !fresh.Valid() {
		t.Fatal("старый Close удалил replacement", err)
	}
	// g1 содержит первую оперативную группу с общими конфигурационными ID.
	g1, err := so.OpenGroup(ctx, "same-config", "same-profile")
	if err != nil {
		t.Fatal(err)
	}
	// g2 содержит независимую вторую группу с теми же конфигурационными ID.
	g2, err := so.OpenGroup(ctx, "same-config", "same-profile")
	if err != nil {
		t.Fatal(err)
	}
	// w содержит наблюдателя работника FFmpeg внутри группы.
	w, err := g1.OpenFFmpeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// first содержит наблюдателя первой попытки FFmpeg.
	first, err := w.BeginAttempt(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке записи оперативного состояния точного владельца.
	if err := first.Observe(ctx, FFmpegObservation{State: ObservationReady}); err != nil {
		t.Fatal(err)
	}
	// second содержит наблюдателя следующей попытки FFmpeg, заменяющей первую.
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
	// err сообщает об ошибке записи оперативного состояния точного владельца.
	if err := first.Observe(ctx, FFmpegObservation{State: ObservationFailed}); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	// err сообщает об ошибке регистрации точной попытки FFmpeg.
	if _, err := w.BeginAttempt(ctx, 2); !errors.Is(err, ErrStale) {
		t.Fatal("повтор attemptID принят", err)
	}
	// err сообщает об ошибке регистрации точной попытки FFmpeg.
	if _, err := w.BeginAttempt(ctx, 0); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	// snapshot содержит согласованную копию текущих наблюдений сессии.
	snapshot, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || len(snapshot.Groups) != 2 || snapshot.Groups[0].Stamp.InstanceID == snapshot.Groups[1].Stamp.InstanceID || snapshot.FFmpeg[0].Value != nil || snapshot.FFmpeg[0].Stamp.AttemptID != 2 {
		t.Fatalf("потеря identity/absence: %+v %v", snapshot, err)
	}
	// err сообщает об ошибке закрытия точного владельца наблюдений.
	if err := g1.Close(ctx); err != nil || w.Valid() || second.Valid() || !g2.Valid() || !fresh.Valid() {
		t.Fatal("закрытие группы повредило соседа", err)
	}
	// w2 содержит работника FFmpeg второй независимой группы.
	w2, err := g2.OpenFFmpeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке регистрации точной попытки FFmpeg.
	if _, err := w2.BeginAttempt(ctx, 1); err != nil {
		t.Fatal("новому worker запрещена первая попытка", err)
	}
	// lease содержит логический допуск точного сохраняемого исполнения.
	lease, err := s.BindExecutionOwner(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	// replacement содержит нового наблюдателя после отзыва прежнего владельца.
	replacement, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if so.Valid() || fresh.Valid() || g2.Valid() || w2.Valid() || !replacement.Valid() || !lease.Valid() {
		t.Fatal("Session replacement нарушил subtree либо физический lease")
	}
	// err сообщает об ошибке закрытия точного владельца наблюдений.
	if err := so.Close(ctx); err != nil || !replacement.Valid() {
		t.Fatal("старый Session Close удалил новый token", err)
	}
}

// Проверяет сохранение снимка и владельца при исчерпании бюджета или ревизии, копирование строки
// ошибки и возможность закрыть поддерево после отказа.
func TestAcceptanceObservationBudgetsAreAtomicAndDoNotBlockRetirement(t *testing.T) {
	// ctx задаёт контекст операций проверяемого сценария оперативных наблюдений.
	ctx := context.Background()
	// limits задаёт явные бюджеты реестра для проверки их границ.
	limits := acceptanceObservationLimits()
	limits.MaxGroups, limits.MaxFFmpegWorkers, limits.MaxOutputs = 1, 1, 1
	// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет изменяемые
	// тестовые репозитории и учитывает обращения к БД. parent идентифицирует точное выполнение
	// родительской сессии. child идентифицирует точную попытку дочернего выхода.
	s, u, parent, child := newObservationAcceptanceService(t, limits)
	// so содержит наблюдателя текущего выполнения сессии. err сообщает об ошибке регистрации
	// наблюдателя точной сессии.
	so, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	// g содержит наблюдателя отдельной оперативной группы.
	g, err := so.OpenGroup(ctx, "group", "profile")
	if err != nil {
		t.Fatal(err)
	}
	// w содержит наблюдателя работника FFmpeg внутри группы.
	w, err := g.OpenFFmpeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке регистрации независимой оперативной группы.
	if _, err := so.OpenGroup(ctx, "second", "profile"); !errors.Is(err, ErrObservationLimit) {
		t.Fatal(err)
	}
	// err сообщает об ошибке регистрации работника FFmpeg.
	if _, err := g.OpenFFmpeg(ctx); !errors.Is(err, ErrObservationLimit) {
		t.Fatal(err)
	}
	// oo содержит наблюдателя точной попытки выхода.
	oo, err := s.OpenOutputObserver(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	// backing создаёт большой строковый буфер для проверки отсутствия его удержания короткой
	// ошибкой.
	backing := strings.Repeat("x", 1<<20)
	// value содержит короткую строку внутри большого чужого буфера для проверки её независимого
	// копирования.
	value := backing[512:544]
	// err сообщает об ошибке записи оперативного состояния точного владельца.
	if err := oo.Observe(ctx, OutputObservation{State: OutputRetrying, Error: value}); err != nil {
		t.Fatal(err)
	}
	if unsafe.StringData(oo.node.failure) == unsafe.StringData(value) {
		t.Fatal("короткая ошибка удерживает backing buffer вызывающего")
	}
	// before сохраняет согласованный снимок до проверяемого действия.
	before, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке записи оперативного состояния точного владельца.
	if err := oo.Observe(ctx, OutputObservation{State: OutputPublishing, Error: strings.Repeat("e", int(limits.MaxErrorBytes)+1)}); !errors.Is(err, ErrObservationLimit) {
		t.Fatal(err)
	}
	// after содержит согласованный снимок после проверяемого действия для проверки сохранности.
	after, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || !reflect.DeepEqual(before, after) || !oo.Valid() {
		t.Fatal("отказ бюджета частично изменил snapshot", err)
	}
	u.authority.nextObservation = math.MaxUint64
	// err сообщает об ошибке регистрации наблюдателя точной попытки выхода.
	if _, err := s.OpenOutputObserver(ctx, child); !errors.Is(err, ErrConflict) || !oo.Valid() {
		t.Fatal("переполнение token заменило владельца", err)
	}
	oo.node.stamp.Revision = math.MaxUint64
	// err сообщает об ошибке записи оперативного состояния точного владельца.
	if err := oo.Observe(ctx, OutputObservation{State: OutputPublishing}); !errors.Is(err, ErrConflict) || oo.node.failure != value {
		t.Fatal("переполнение revision изменило значение", err)
	}
	// err сообщает об ошибке закрытия точного владельца наблюдений.
	if err := so.Close(ctx); err != nil || oo.Valid() || w.Valid() {
		t.Fatal("лимиты помешали закрытию subtree", err)
	}
	// err сообщает об ошибке чтения текущих наблюдений сессии.
	if _, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent)); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
}

// Проверяет сохранение RAM после подтверждённого отката и отзыв неподтверждённых владельцев после
// возможного COMMIT без повтора операции и подмены её ошибки.
func TestAcceptanceOwnedWriteRollbackAndUncertainOutcome(t *testing.T) {
	// outcome выбирает подтверждённый откат, возможный COMMIT или недоступную сверку.
	for _, outcome := range []string{"rollback", "committed", "unreadable"} {
		t.Run(outcome, func(t *testing.T) {
			// Проверяет сохранность RAM после подтверждённого отката либо отзыв владельца после сохранённого
			// изменения и недоступной сверки, без повторения записи и подмены ошибки.

			// ctx задаёт контекст операций проверяемого сценария оперативных наблюдений.
			ctx := context.Background()
			// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет
			// изменяемые тестовые репозитории и учитывает обращения к БД. f идентифицирует
			// точную попытку выхода проверяемого исхода записи.
			s, u, _, f := newObservationAcceptanceService(t, acceptanceObservationLimits())
			// o содержит наблюдателя точной попытки выхода. err сообщает об ошибке
			// регистрации наблюдателя точной попытки выхода.
			o, err := s.OpenOutputObserver(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			// err сообщает об ошибке записи оперативного состояния точного владельца.
			if err := o.Observe(ctx, OutputObservation{State: OutputPublishing}); err != nil {
				t.Fatal(err)
			}
			// before сохраняет согласованный снимок до проверяемого действия.
			before, err := s.RuntimeSnapshot(ctx, observationSessionFence(f))
			if err != nil {
				t.Fatal(err)
			}
			// failure задаёт синтетическую диагностику, которая не должна попадать в
			// событие.
			failure := errors.New("неоднозначный COMMIT")
			// calls считает вызовы записи для обнаружения недопустимого повтора.
			calls := 0
			err = u.authority.GuardOwnedWrite(ctx, u, o, time.Second, func() (bool, error) {
				// Имитирует выбранный исход записи: меняет поколение выхода либо делает чтение недоступным и сообщает,
				// был ли возможен COMMIT.

				calls++
				if outcome == "committed" {
					// current содержит сохраняемый выход, поколение которого
					// меняется для имитации COMMIT.
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
			// after содержит согласованный снимок после проверяемого действия для проверки
			// сохранности. snapshotErr содержит результат чтения RAM после проверяемого
			// исхода записи.
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

// Проверяет отказ для нулевых бюджетов наблюдений и арифметического переполнения их расчёта.
func TestAcceptanceObservationLimitsRejectZeroAndArithmeticOverflow(t *testing.T) {
	// change вносит выбранное нарушение явного бюджета или идентичности события.
	for _, change := range []func(*ObservationLimits){
		func(l *ObservationLimits) {
			// Обнуляет бюджет числа Session.
			l.MaxSessions = 0
		}, func(l *ObservationLimits) {
			// Обнуляет бюджет числа групп.
			l.MaxGroups = 0
		},
		func(l *ObservationLimits) {
			// Обнуляет бюджет числа выходов.
			l.MaxOutputs = 0
		}, func(l *ObservationLimits) {
			// Обнуляет бюджет числа владельцев FFmpeg.
			l.MaxFFmpegWorkers = 0
		},
		func(l *ObservationLimits) {
			// Обнуляет предел длины идентификатора.
			l.MaxIdentifierBytes = 0
		}, func(l *ObservationLimits) {
			// Обнуляет предел длины ошибки.
			l.MaxErrorBytes = 0
		},
		func(l *ObservationLimits) {
			// Обнуляет общий бюджет байтов наблюдений.
			l.MaxBytes = 0
		}, func(l *ObservationLimits) {
			// Задаёт предельные uint64 для ошибки и общего бюджета, проверяя отклонение арифметического
			// переполнения.
			l.MaxErrorBytes = math.MaxUint64
			l.MaxBytes = math.MaxUint64
		},
		func(l *ObservationLimits) {
			// Задаёт math.MaxInt для всех счётчиков владельцев, проверяя переполнение суммарного бюджета.

			l.MaxSessions = math.MaxInt
			l.MaxGroups = math.MaxInt
			l.MaxOutputs = math.MaxInt
			l.MaxFFmpegWorkers = math.MaxInt
		},
	} {
		// limits задаёт явные бюджеты реестра для проверки их границ.
		limits := acceptanceObservationLimits()
		change(&limits)
		// err сообщает об ошибке создания общего реестра с явными бюджетами.
		if _, err := NewRuntimeAuthority("owner", limits, OutputExecutionLimits{MaxReservations: 8, MaxIdentifierBytes: 1024, MaxBytes: 65536}); !errors.Is(err, ErrInvalid) {
			t.Fatal("некорректные ограничения приняты", limits, err)
		}
	}
}

// Проверяет учёт Session при первой регистрации Output и атомарный общий бюджет байтов, включая замену
// без двойного учёта.
func TestAcceptanceOutputFirstCountsSessionAndByteBudget(t *testing.T) {
	// ctx задаёт контекст операций проверяемого сценария оперативных наблюдений.
	ctx := context.Background()
	// limits задаёт явные бюджеты реестра для проверки их границ.
	limits := acceptanceObservationLimits()
	limits.MaxSessions = 1
	// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет изменяемые
	// тестовые репозитории и учитывает обращения к БД. f идентифицирует точную попытку первого
	// наблюдаемого выхода.
	s, u, _, f := newObservationAcceptanceService(t, limits)
	// o содержит наблюдателя точной попытки выхода. err сообщает об ошибке регистрации наблюдателя
	// точной попытки выхода.
	o, err := s.OpenOutputObserver(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	// other содержит точное выполнение другой сессии для проверки общего бюджета.
	other := f
	other.SessionID, other.CommandID, other.OutputID = "other-session", "other-command", "other-output"
	u.repository.session.ID = other.SessionID
	// c содержит копию квитанции, подставляемую для другого исполнения.
	c := u.repository.receipts[f.CommandID]
	c.ID, c.SessionID, c.OutputID = other.CommandID, other.SessionID, other.OutputID
	u.repository.receipts[c.ID] = c
	// output содержит копию сохраняемого выхода для привязки к другой сессии.
	output := u.repository.outputs[f.OutputID]
	output.ID, output.SessionID = other.OutputID, other.SessionID
	u.repository.outputs[output.ID] = output
	// err сообщает об ошибке регистрации наблюдателя точной попытки выхода.
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
	// err сообщает об ошибке записи оперативного состояния точного владельца.
	if err := o.Observe(ctx, OutputObservation{State: OutputRetrying, Error: "сохранённая ошибка"}); err != nil {
		t.Fatal(err)
	}
	// before сохраняет согласованный снимок до проверяемого действия.
	before, err := s.RuntimeSnapshot(ctx, observationSessionFence(f))
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке записи оперативного состояния точного владельца.
	if err := o.Observe(ctx, OutputObservation{State: OutputPublishing, Error: strings.Repeat("e", 256)}); !errors.Is(err, ErrObservationLimit) {
		t.Fatal("MaxBytes не ограничил допустимую по MaxErrorBytes строку", err)
	}
	// after содержит согласованный снимок после проверяемого действия для проверки сохранности.
	after, err := s.RuntimeSnapshot(ctx, observationSessionFence(f))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("переполнение byte budget частично изменило снимок", err)
	}
	// replacement содержит нового наблюдателя после отзыва прежнего владельца.
	replacement, err := s.OpenOutputObserver(ctx, f)
	if err != nil || !replacement.Valid() || o.Valid() {
		t.Fatal("replacement ошибочно учтён дважды в byte budget", err)
	}
	// err сообщает об ошибке закрытия точного владельца наблюдений.
	if err := replacement.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// Проверяет обязательный полный совпадающий ExecutionFence события output_execution_failed и запрет
// такого поля у иных событий.
func TestAcceptanceExecutionFailureEventRequiresMatchingCompleteFence(t *testing.T) {
	// fence идентифицирует точную попытку проверяемого выхода.
	fence := ExecutionFence{CommandID: "command", SessionID: "session", OutputID: "output", Incarnation: "owner", Lifecycle: 1, OutputGeneration: 1, Attempt: 1}
	// event содержит исходное корректное служебное событие окончательного отказа выхода.
	event := Event{ID: "event", SessionID: fence.SessionID, OutputID: fence.OutputID, Kind: "output_execution_failed", Message: "Итоговый отказ", Execution: &fence, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	// err сообщает об ошибке проверки полноты идентичности служебного события.
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	// change вносит выбранное нарушение явного бюджета или идентичности события.
	for _, change := range []func(*Event){
		func(e *Event) {
			// Убирает обязательный ExecutionFence события окончательного отказа выхода.
			e.Execution = nil
		}, func(e *Event) {
			// Подменяет SessionID события без изменения ExecutionFence.
			e.SessionID = "other"
		},
		func(e *Event) {
			// Подменяет OutputID события без изменения ExecutionFence.
			e.OutputID = "other"
		}, func(e *Event) {
			// Меняет вид события на command_completed при сохранённом ExecutionFence отказа.
			e.Kind = "command_completed"
		},
		func(e *Event) {
			// Обнуляет Attempt ExecutionFence события.
			e.Execution.Attempt = 0
		}, func(e *Event) {
			// Обнуляет Lifecycle ExecutionFence события.
			e.Execution.Lifecycle = 0
		},
		func(e *Event) {
			// Обнуляет OutputGeneration ExecutionFence события.
			e.Execution.OutputGeneration = 0
		}, func(e *Event) {
			// Убирает CommandID ExecutionFence события.
			e.Execution.CommandID = ""
		},
		func(e *Event) {
			// Убирает Incarnation ExecutionFence события.
			e.Execution.Incarnation = ""
		},
	} {
		// candidate содержит независимую копию события для внесения выбранного нарушения.
		// copiedFence содержит отдельную копию ExecutionFence, чтобы повреждение не меняло
		// исходное событие.
		candidate, copiedFence := event, fence
		candidate.Execution = &copiedFence
		change(&candidate)
		// err сообщает об ошибке проверки полноты идентичности служебного события.
		if err := candidate.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatal("неполный или чужой fence принят в истории отказа", candidate, err)
		}
	}
}

// emptyPreparationRepository моделирует отсутствие подготовок при проверках допуска исполнения.
type emptyPreparationRepository struct { /* ArtifactRepository сохраняет остальные неиспользуемые методы контракта артефактов. */
	ArtifactRepository
}

// Возвращает пустой набор поколений, моделируя отсутствие подготовки в тестах допуска исполнения.
func (emptyPreparationRepository) Generations(context.Context, ListQuery) ([]FallbackGeneration, error) {
	return nil, nil
}
