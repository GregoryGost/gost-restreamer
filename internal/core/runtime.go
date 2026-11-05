package core

import (
	"context"
	"crypto/subtle"
	"errors"
	"sync/atomic"
)

var ErrRuntimeCapability = errors.New("UnitOfWork не предоставляет общую RuntimeAuthority")

// RuntimeAuthorityProvider предоставляет общий допуск всех пользователей одного хранилища.
type RuntimeAuthorityProvider interface {
	RuntimeAuthority() *RuntimeAuthority
}

// RuntimeSessionFence не зависит от версии записи и ручной паузы.
type RuntimeSessionFence struct {
	SessionID   ID
	Incarnation string
	Lifecycle   uint64
}

// RuntimeAuthority упорядочивает записи БД, установку источников и допуск действий.
// Экземпляр принадлежит UnitOfWork, а не отдельному Service.
type RuntimeAuthority struct {
	gate                  chan struct{}
	incarnation           string
	closed                bool
	leases                map[ID]*RuntimeLease
	publications          map[ID]*RuntimeLease
	limits                ObservationLimits
	observations          map[uint64]*observationNode
	nextObservation       uint64
	preparations          map[ID]*PreparationOwner
	outputExecutionLimits OutputExecutionLimits
	outputExecutions      map[outputExecutionKey]*OutputExecutionReservation
	outputExecutionBytes  uint64
	nextOutputExecution   uint64
	dispatcher            *RuntimeDispatcher
}

func NewRuntimeAuthority(incarnation string, limits ObservationLimits, executionLimits OutputExecutionLimits) (*RuntimeAuthority, error) {
	if err := validText("incarnation", incarnation); err != nil {
		return nil, err
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if err := executionLimits.Validate(); err != nil {
		return nil, err
	}
	a := &RuntimeAuthority{outputExecutionLimits: executionLimits, outputExecutions: make(map[outputExecutionKey]*OutputExecutionReservation), preparations: make(map[ID]*PreparationOwner), limits: limits, observations: make(map[uint64]*observationNode), gate: make(chan struct{}, 1), incarnation: incarnation, leases: make(map[ID]*RuntimeLease), publications: make(map[ID]*RuntimeLease)}
	a.gate <- struct{}{}
	return a, nil
}

func (a *RuntimeAuthority) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.gate:
	}
	if err := ctx.Err(); err != nil {
		a.release()
		return err
	}
	if a.closed {
		a.release()
		return ErrClosed
	}
	return nil
}

func (a *RuntimeAuthority) release() { a.gate <- struct{}{} }

// GuardWrite вызывается только владельцем UnitOfWork. write завершает все
// транзакции до возврата; первый результат сообщает о возможном COMMIT.
// Сверка не заменяет результат записи и никогда не повторяет её callback.
func (a *RuntimeAuthority) GuardWrite(ctx context.Context, uow UnitOfWork, write func() (bool, error)) error {
	if uow == nil || write == nil {
		return ErrInvalid
	}
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	uncertain := true
	defer func() {
		if uncertain {
			a.reconcile(ctx, uow)
		}
	}()
	var err error
	uncertain, err = write()
	return err
}

func (a *RuntimeAuthority) reconcile(ctx context.Context, uow UnitOfWork) {
	a.reconcilePreparations(ctx, uow)
	if len(a.leases) == 0 {
		return
	}
	invalid := make([]*RuntimeLease, 0)
	invalidObservations := make([]*observationNode, 0)
	err := uow.View(ctx, func(r Repositories) error {
		for _, lease := range a.leases {
			if err := lease.authorized(ctx, r); err != nil {
				if !errors.Is(err, ErrStale) && !errors.Is(err, ErrNotFound) {
					return err
				}
				invalid = append(invalid, lease)
			}
		}
		for _, node := range a.observations {
			if node.kind != observationOutput {
				continue
			}
			c, err := r.Runtime.ReceiptByID(ctx, node.execution.CommandID)
			if errors.Is(err, ErrNotFound) || err == nil && c.Status != CommandSucceeded {
				invalidObservations = append(invalidObservations, node)
			} else if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// Неподтверждённая свежесть закрывает допуск до новой успешной привязки.
		for _, lease := range a.leases {
			a.revoke(lease)
		}
		return
	}
	for _, node := range invalidObservations {
		a.removeObservation(node)
	}
	for _, lease := range invalid {
		a.revoke(lease)
	}
}

// Close синхронно отзывает допуск. Физические ресурсы закрывает их владелец.
// Функция отзыва должна менять только память и не захватывать authority повторно.
func (a *RuntimeAuthority) Close() {
	<-a.gate
	defer a.release()
	a.closed = true
	if a.dispatcher != nil {
		a.dispatcher.closeLocked()
	}
	for _, reservation := range a.outputExecutions {
		reservation.sealLocked()
	}
	for _, o := range a.preparations {
		a.revokePreparation(o)
	}
	for _, lease := range a.leases {
		a.revoke(lease)
	}
}

// RuntimeLease разрешает ограниченное изменение памяти под общим допуском.
// В Do и функциях установки/отзыва запрещены I/O, ожидания и повторный захват authority.
type RuntimeLease struct {
	authority *RuntimeAuthority
	id        ID
	streamID  ID
	session   RuntimeSessionFence
	execution ExecutionFence
	valid     atomic.Bool
	done      chan struct{}
	revoke    func()
}

func (l *RuntimeLease) ID() ID                            { return l.id }
func (l *RuntimeLease) SessionFence() RuntimeSessionFence { return l.session }
func (l *RuntimeLease) ExecutionFence() ExecutionFence    { return l.execution }
func (l *RuntimeLease) Valid() bool                       { return l != nil && l.valid.Load() }
func (l *RuntimeLease) Done() <-chan struct{}             { return l.done }

func (l *RuntimeLease) Do(ctx context.Context, fn func() error) error {
	if l == nil || fn == nil {
		return ErrInvalid
	}
	if err := l.authority.acquire(ctx); err != nil {
		return err
	}
	defer l.authority.release()
	if !l.valid.Load() {
		return ErrStale
	}
	return fn()
}

func (l *RuntimeLease) Revoke(ctx context.Context) error {
	if l == nil {
		return ErrInvalid
	}
	if err := l.authority.acquire(ctx); err != nil {
		return err
	}
	defer l.authority.release()
	l.authority.revoke(l)
	return nil
}

func (a *RuntimeAuthority) revoke(l *RuntimeLease) {
	for _, reservation := range a.outputExecutions {
		if reservation.lease == l {
			reservation.sealLocked()
		}
	}
	if !l.valid.Swap(false) {
		return
	}
	for _, n := range a.observations {
		if n.lease == l {
			a.removeObservation(n)
		}
	}
	delete(a.leases, l.id)
	if a.publications[l.streamID] == l {
		delete(a.publications, l.streamID)
	}
	close(l.done)
	if l.revoke != nil {
		l.revoke()
		l.revoke = nil
	}
}

func (s *Service) runtimeAuthority() (*RuntimeAuthority, error) {
	provider, ok := s.uow.(RuntimeAuthorityProvider)
	if !ok || provider.RuntimeAuthority() == nil {
		return nil, ErrRuntimeCapability
	}
	a := provider.RuntimeAuthority()
	if a.incarnation != s.incarnation {
		return nil, ErrStale
	}
	return a, nil
}

// ActivateOBS атомарно допускает источник по свежему состоянию Session. Для
// первоначальной проверки, разделённой сетевым Accept, нужен ActivateOBSFor.
// install атомарно меняет память; при ошибке прежний источник сохраняется.
// Возвращённый revoke отзывает только установленный им источник.
func (s *Service) ActivateOBS(ctx context.Context, streamID ID, key string, install func(*RuntimeLease) (revoke func(), err error)) (*RuntimeLease, error) {
	return s.activateOBS(ctx, nil, streamID, key, install)
}

// ActivateOBSFor проверяет RuntimeSessionFence первоначальной проверки перед сетевым Accept.
func (s *Service) ActivateOBSFor(ctx context.Context, expected RuntimeSessionFence, streamID ID, key string, install func(*RuntimeLease) (revoke func(), err error)) (*RuntimeLease, error) {
	return s.activateOBS(ctx, &expected, streamID, key, install)
}

func (s *Service) activateOBS(ctx context.Context, expected *RuntimeSessionFence, streamID ID, key string, install func(*RuntimeLease) (revoke func(), err error)) (*RuntimeLease, error) {
	if install == nil {
		return nil, ErrInvalid
	}
	a, err := s.runtimeAuthority()
	if err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	if err = a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	var session Session
	err = s.uow.View(ctx, func(r Repositories) error {
		var err error
		session, err = r.Runtime.ActiveSession(ctx, streamID)
		if err != nil {
			return err
		}
		if session.Incarnation != a.incarnation || session.State != SessionRunning || key == "" || subtle.ConstantTimeCompare([]byte(session.Snapshot.Stream.PublicationKey), []byte(key)) != 1 {
			return ErrNotReady
		}
		if expected != nil && *expected != (RuntimeSessionFence{session.ID, session.Incarnation, session.Lifecycle}) {
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
	lease := &RuntimeLease{authority: a, id: id, streamID: streamID, session: RuntimeSessionFence{session.ID, session.Incarnation, session.Lifecycle}, done: make(chan struct{})}
	lease.valid.Store(true)
	revoke, err := install(lease)
	if err != nil {
		lease.valid.Store(false)
		close(lease.done)
		if revoke != nil {
			revoke()
		}
		return nil, err
	}
	lease.revoke = revoke
	old := a.publications[streamID]
	a.leases[id] = lease
	a.publications[streamID] = lease
	if old != nil {
		a.revoke(old)
	}
	return lease, nil
}

// BindExecutionOwner проверяет долговечную issuance без изменения команд и БД.
// Допуск Session независим от OBS, а допуск выхода — от соседних выходов.
func (s *Service) BindExecutionOwner(ctx context.Context, fence ExecutionFence) (*RuntimeLease, error) {
	a, err := s.runtimeAuthority()
	if err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	if err = a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	fence = cloneExecutionFence(fence)
	lease := &RuntimeLease{authority: a, id: id, session: RuntimeSessionFence{fence.SessionID, fence.Incarnation, fence.Lifecycle}, execution: fence, done: make(chan struct{})}
	if err = s.uow.View(ctx, func(r Repositories) error { return lease.authorized(ctx, r) }); err != nil {
		return nil, err
	}
	for _, existing := range a.leases {
		if existing.execution == fence {
			return existing, nil
		}
	}
	lease.valid.Store(true)
	a.leases[id] = lease
	return lease, nil
}

func (s *Service) BindExecution(ctx context.Context, fence ExecutionFence) (*RuntimeLease, error) {
	return s.BindExecutionOwner(ctx, fence)
}

func (l *RuntimeLease) authorized(ctx context.Context, r Repositories) error {
	if err := preparationIdleForRuntime(ctx, r, l.authority.incarnation); err != nil {
		return err
	}
	session, err := r.Runtime.Session(ctx, l.session.SessionID)
	if err != nil {
		return err
	}
	if session.Incarnation != l.authority.incarnation || (RuntimeSessionFence{session.ID, session.Incarnation, session.Lifecycle}) != l.session {
		return ErrStale
	}
	if l.execution.CommandID == "" {
		if session.State != SessionRunning || session.StreamID != l.streamID {
			return ErrStale
		}
		return nil
	}
	c, err := r.Runtime.ReceiptByID(ctx, l.execution.CommandID)
	if err != nil {
		return err
	}
	if executionFence(c) != l.execution || (c.Status != CommandIssued && c.Status != CommandSucceeded) || c.Attempt == 0 {
		return ErrStale
	}
	if c.OutputID == "" {
		if c.Kind != CommandStartSession || !session.Issued || (session.State != SessionStarting && session.State != SessionRunning) {
			return ErrStale
		}
		return nil
	}
	if (c.Kind != CommandStartOutput && c.Kind != CommandAddOutput) || session.State != SessionRunning {
		return ErrStale
	}
	o, err := r.Runtime.Output(ctx, c.OutputID)
	if err != nil {
		return err
	}
	if o.SessionID != session.ID || o.Generation != c.OutputGeneration || o.Attempt != c.Attempt || !o.Attached || !o.DesiredRunning || !o.Issued || o.CleanupPending {
		return ErrStale
	}
	return nil
}
