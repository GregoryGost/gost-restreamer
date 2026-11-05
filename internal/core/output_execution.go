package core

import (
	"context"
	"errors"
	"math"
	"reflect"
)

var ErrOutputExecutionLimit = errors.New("исчерпан бюджет исполнений выходов")

// OutputExecutionLimits ограничивает число исполнений и логический вес их реестра.
// Вес записи равен 512 байтам плюс длины удерживаемых строк ExecutionFence; это не размер кучи Go.
type OutputExecutionLimits struct {
	MaxReservations    int
	MaxIdentifierBytes uint64
	MaxBytes           uint64
}

func (l OutputExecutionLimits) Validate() error {
	if l.MaxReservations <= 0 || l.MaxIdentifierBytes == 0 || l.MaxBytes < 512 || l.MaxIdentifierBytes > l.MaxBytes || l.MaxIdentifierBytes > (math.MaxUint64-512)/4 || uint64(l.MaxReservations) > math.MaxUint64/(512+4*l.MaxIdentifierBytes) {
		return ErrInvalid
	}
	return nil
}

type outputExecutionKey struct{ session, output ID }

// OutputOperationKind выбирает один из трёх независимых фиксированных допусков.
type OutputOperationKind uint8

const (
	OutputOperationEstablishment OutputOperationKind = iota
	OutputOperationRead
	OutputOperationWrite
)

// OutputExecutionReservation удерживает физическое исполнение до подтверждённого освобождения.
// Все поля защищены общей блокировкой; Done можно ожидать вне неё.
type OutputExecutionReservation struct {
	authority                                 *RuntimeAuthority
	token                                     uint64
	fence                                     ExecutionFence
	lease                                     *RuntimeLease
	observer                                  *observationNode
	weight                                    uint64
	active, sealed, releaseEligible, released bool
	slots                                     [3]*OutputOperation
	done                                      chan struct{}
}

// OutputOperation соответствует одному допущенному внешнему действию.
// Владелец вызывает Finish только после фактического завершения действия и его ресурсов.
type OutputOperation struct {
	reservation *OutputExecutionReservation
	kind        OutputOperationKind
}

func (r *OutputExecutionReservation) ExecutionFence() ExecutionFence {
	if r == nil {
		return ExecutionFence{}
	}
	return r.fence
}
func (r *OutputExecutionReservation) Done() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.done
}
func (r *OutputExecutionReservation) key() outputExecutionKey {
	return outputExecutionKey{r.fence.SessionID, r.fence.OutputID}
}
func (r *OutputExecutionReservation) currentLocked() bool {
	current := r.authority.outputExecutions[r.key()]
	return !r.released && current == r && current.token == r.token
}
func (r *OutputExecutionReservation) sealLocked() {
	if !r.sealed {
		r.sealed = true
		close(r.done)
	}
}

// Seal закрывает допуск без I/O, ожидания работников и освобождения удержания.
func (r *OutputExecutionReservation) Seal() {
	if r == nil || r.authority == nil {
		return
	}
	a := r.authority
	<-a.gate
	defer a.release()
	if r.currentLocked() {
		r.sealLocked()
	}
}

// Activate однократно связывает подготовку с точным текущим наблюдателем того же RuntimeLease.
func (r *OutputExecutionReservation) Activate(ctx context.Context, observer *OutputObserver) error {
	if r == nil || r.authority == nil || observer == nil || observer.node == nil {
		return ErrInvalid
	}
	a := r.authority
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	n := observer.node
	if !r.currentLocked() || r.sealed || !r.lease.valid.Load() || n.authority != a || n.kind != observationOutput || n.execution != r.fence || n.lease != r.lease || !n.valid.Load() || a.observations[n.token] != n {
		return ErrStale
	}
	if r.active {
		return ErrConflict
	}
	r.observer = n
	r.active = true
	return nil
}

// Admit резервирует фиксированный слот непосредственно перед внешней операцией.
func (r *OutputExecutionReservation) Admit(ctx context.Context, kind OutputOperationKind) (*OutputOperation, error) {
	if r == nil || r.authority == nil || kind > OutputOperationWrite {
		return nil, ErrInvalid
	}
	a := r.authority
	if err := a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	if !r.currentLocked() || r.sealed || !r.lease.valid.Load() {
		return nil, ErrStale
	}
	if !r.active {
		return nil, ErrNotReady
	}
	n := r.observer
	if n == nil || !n.valid.Load() || a.observations[n.token] != n || n.lease != r.lease {
		r.sealLocked()
		return nil, ErrStale
	}
	if r.slots[kind] != nil {
		return nil, ErrInUse
	}
	operation := &OutputOperation{reservation: r, kind: kind}
	r.slots[kind] = operation
	return operation, nil
}

// Finish идемпотентно завершает только собственный слот, включая после закрытия RuntimeAuthority.
func (o *OutputOperation) Finish() {
	if o == nil || o.reservation == nil {
		return
	}
	r := o.reservation
	a := r.authority
	<-a.gate
	defer a.release()
	if r.currentLocked() && r.slots[o.kind] == o {
		r.slots[o.kind] = nil
	}
}

// ReleaseJoined вызывается владельцем только после фактического завершения всех работников,
// подписки и поздних подключений. Метод не ждёт их и не выполняет SQL; ошибка сохраняет удержание.
func (r *OutputExecutionReservation) ReleaseJoined() error {
	if r == nil || r.authority == nil {
		return ErrInvalid
	}
	a := r.authority
	<-a.gate
	defer a.release()
	if r.released {
		return nil
	}
	if !r.currentLocked() {
		return ErrStale
	}
	if !r.sealed || (!r.releaseEligible && !a.closed) {
		return ErrNotReady
	}
	for _, slot := range r.slots {
		if slot != nil {
			return ErrNotReady
		}
	}
	delete(a.outputExecutions, r.key())
	a.outputExecutionBytes -= r.weight
	r.released = true
	r.observer = nil
	r.lease = nil
	return nil
}

// ReserveOutputExecution сверяет весь неизменяемый снимок под общим допуском без сети.
func (s *Service) ReserveOutputExecution(ctx context.Context, lease *RuntimeLease, snapshot OutputSnapshot) (*OutputExecutionReservation, error) {
	a, err := s.runtimeAuthority()
	if err != nil {
		return nil, err
	}
	if lease == nil || lease.authority != a || lease.execution.OutputID == "" {
		return nil, ErrInvalid
	}
	if err = lease.execution.Validate(); err != nil {
		return nil, err
	}
	if err = a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	if !lease.valid.Load() || a.leases[lease.id] != lease {
		return nil, ErrStale
	}
	f := lease.execution
	key := outputExecutionKey{f.SessionID, f.OutputID}
	if a.outputExecutions[key] != nil {
		return nil, ErrInUse
	}
	weight := uint64(512)
	for _, value := range []string{string(f.CommandID), string(f.SessionID), string(f.OutputID), f.Incarnation} {
		n := uint64(len(value))
		if n > a.outputExecutionLimits.MaxIdentifierBytes || n > math.MaxUint64-weight {
			return nil, ErrOutputExecutionLimit
		}
		weight += n
	}
	if len(a.outputExecutions) >= a.outputExecutionLimits.MaxReservations || weight > a.outputExecutionLimits.MaxBytes-a.outputExecutionBytes {
		return nil, ErrOutputExecutionLimit
	}
	if a.nextOutputExecution == math.MaxUint64 {
		return nil, ErrConflict
	}
	err = s.uow.View(ctx, func(repo Repositories) error {
		if e := lease.authorized(ctx, repo); e != nil {
			return e
		}
		output, e := repo.Runtime.Output(ctx, f.OutputID)
		if e != nil {
			return e
		}
		if output.ID != f.OutputID || output.SessionID != f.SessionID || !reflect.DeepEqual(output.Snapshot, snapshot) {
			return ErrStale
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	a.nextOutputExecution++
	r := &OutputExecutionReservation{authority: a, token: a.nextOutputExecution, fence: f, lease: lease, weight: weight, done: make(chan struct{})}
	a.outputExecutions[key] = r
	a.outputExecutionBytes += weight
	return r, nil
}

// ConfirmOutputExecutionResolution разрешает освобождение лишь после успешного
// согласованного чтения, доказавшего долговечную непригодность прежнего ExecutionFence.
func (s *Service) ConfirmOutputExecutionResolution(ctx context.Context, reservation *OutputExecutionReservation) error {
	a, err := s.runtimeAuthority()
	if err != nil {
		return err
	}
	if reservation == nil || reservation.authority != a {
		return ErrInvalid
	}
	if err = a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if !reservation.currentLocked() {
		return ErrStale
	}
	f := reservation.fence
	eligible := false
	err = s.uow.View(ctx, func(repo Repositories) error {
		session, se := repo.Runtime.Session(ctx, f.SessionID)
		receipt, ce := repo.Runtime.ReceiptByID(ctx, f.CommandID)
		output, oe := repo.Runtime.Output(ctx, f.OutputID)
		for _, e := range []error{se, ce, oe} {
			if e != nil && !errors.Is(e, ErrNotFound) {
				return e
			}
		}
		eligible = se == nil && ce == nil && oe == nil && session.ID == f.SessionID && session.Incarnation == f.Incarnation && session.Lifecycle == f.Lifecycle && session.State == SessionRunning && executionFence(receipt) == f && (receipt.Kind == CommandStartOutput || receipt.Kind == CommandAddOutput) && (receipt.Status == CommandIssued || receipt.Status == CommandSucceeded) && receipt.Attempt != 0 && output.ID == f.OutputID && output.SessionID == f.SessionID && output.Generation == f.OutputGeneration && output.Attempt == f.Attempt && output.Attached && output.DesiredRunning && output.Issued && !output.CleanupPending
		return nil
	})
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if eligible {
		return ErrNotReady
	}
	reservation.releaseEligible = true
	reservation.sealLocked()
	return nil
}
