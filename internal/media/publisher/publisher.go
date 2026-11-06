// Package publisher передаёт независимые выходы группы через RTMP и RTMPS.
package publisher

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
)

var (
	ErrOptions        = errors.New("некорректные настройки Publisher")
	ErrCleanupPending = errors.New("завершение Publisher ещё не подтверждено")
)

// Options задаёт явные бюджеты и положительные интервалы восстановления.
// Admit и OnWrite транспорта принадлежат Publisher и должны быть nil на входе.
type Options struct {
	Queue                                    media.QueueLimits
	RetryInitial, RetryMax, ResubscribeDelay time.Duration
	Transport                                rtmp.OutboundOptions
	Resolver                                 rtmp.OutboundResolver
	Logger                                   *slog.Logger
}

func (o Options) Validate() error {
	if o.Queue.Validate() != nil || o.Transport.Validate() != nil || o.RetryInitial <= 0 || o.RetryMax < o.RetryInitial || o.ResubscribeDelay <= 0 || o.Transport.Admit != nil || o.Transport.OnWrite != nil {
		return ErrOptions
	}
	return nil
}

// Result описывает завершение ресурсов данного Publisher. Ресурсы группы и
// родительского запуска вызывающий код завершает отдельно.
type Result struct {
	Err           error
	NetworkJoined bool
}

type connection interface {
	WriteHeaders(context.Context, media.CodecConfig, media.CodecConfig) error
	WriteDelivery(context.Context, media.Delivery) (rtmp.OutboundWriteResult, error)
	Close() error
	Wait(context.Context) error
	Done() <-chan struct{}
	Err() error
	Snapshot() rtmp.OutboundSnapshot
}

type execution interface {
	activate(context.Context, *core.OutputObserver) error
	admit(context.Context, rtmp.OutboundOperation) (func(), error)
	seal()
	done() <-chan struct{}
	release() error
}

type observation interface {
	Observe(context.Context, core.OutputObservation) error
}

type terminalObservation interface {
	ObserveTerminal(context.Context, core.OutputObservation) error
}

type coreExecution struct {
	reservation *core.OutputExecutionReservation
}

func (e coreExecution) activate(ctx context.Context, o *core.OutputObserver) error {
	return e.reservation.Activate(ctx, o)
}
func (e coreExecution) seal()                 { e.reservation.Seal() }
func (e coreExecution) done() <-chan struct{} { return e.reservation.Done() }
func (e coreExecution) release() error        { return e.reservation.ReleaseJoined() }
func (e coreExecution) admit(ctx context.Context, kind rtmp.OutboundOperation) (func(), error) {
	var k core.OutputOperationKind
	switch kind {
	case rtmp.OutboundEstablishment:
		k = core.OutputOperationEstablishment
	case rtmp.OutboundRead:
		k = core.OutputOperationRead
	case rtmp.OutboundWrite:
		k = core.OutputOperationWrite
	default:
		return nil, core.ErrInvalid
	}
	op, err := e.reservation.Admit(ctx, k)
	if err != nil {
		return nil, err
	}
	return op.Finish, nil
}

// Publisher владеет одной подпиской и последовательными сетевыми попытками.
// Вызывающий код сохраняет владение наблюдателем, ссылкой на группу и долговечными командами.
type Publisher struct {
	mu                                sync.Mutex
	options                           Options
	router                            *media.Router
	target                            rtmp.OutboundTarget
	execution                         execution
	reservation                       *core.OutputExecutionReservation
	dial                              func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error)
	observer                          observation
	cancel                            context.CancelFunc
	active, stopping, revoked, joined bool
	activating                        bool
	abortStarted                      bool
	sealOnce                          sync.Once
	sealDone                          chan struct{}
	done                              chan struct{}
	result                            Result
	state                             core.OutputState
	failure                           FailureReason
	sub                               *media.Subscription
	conn                              connection
	queueDrops                        media.SubscriptionDrops
	metrics                           Snapshot
	hadConnection                     bool
	writeMu                           sync.Mutex
	writtenBytes                      uint64
	writeSaturated                    bool
	lastWrite                         time.Time
}

// Prepare проверяет неизменяемый снимок и резервирует исполнение без сетевых операций.
// Контекст запроса не сохраняется; срок активного выхода задаёт Activate.
func Prepare(ctx context.Context, service *core.Service, owner *core.RuntimeLease, snapshot core.OutputSnapshot, router *media.Router, options Options) (*Publisher, error) {
	if service == nil || owner == nil || router == nil {
		return nil, core.ErrInvalid
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	resolver := options.Resolver
	if resolver == nil {
		resolver = rtmp.ResolveOutbound
	}
	target, err := resolver(snapshot.Destination.Endpoint, snapshot.Destination.StreamKey, snapshot.Profile.Profile.Format)
	if err != nil {
		return nil, err
	}
	if err = target.Validate(snapshot.Profile.Profile.Format, options.Transport.Limits); err != nil {
		return nil, err
	}
	if options.Transport.TLSConfig != nil {
		options.Transport.TLSConfig = options.Transport.TLSConfig.Clone()
		if options.Transport.TLSConfig.RootCAs != nil {
			options.Transport.TLSConfig.RootCAs = options.Transport.TLSConfig.RootCAs.Clone()
		}
	}
	reservation, err := service.ReserveOutputExecution(ctx, owner, snapshot)
	if err != nil {
		return nil, err
	}
	// Полный снимок артефакта не удерживается после проверки точного исполнения.
	options.Resolver = nil
	if options.Logger != nil {
		options.Logger = options.Logger.With("component", "publisher", "session_id", owner.SessionFence().SessionID, "execution", owner.ExecutionFence(), "destination_id", snapshot.Destination.ID, "endpoint", snapshot.Destination.Endpoint, "stream_key", snapshot.Destination.StreamKey)
	}
	p := &Publisher{options: options, router: router, target: target, reservation: reservation, execution: coreExecution{reservation}, done: make(chan struct{}), sealDone: make(chan struct{}), state: core.OutputStopped}
	p.dial = func(ctx context.Context, t rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
		return rtmp.DialOutbound(ctx, t, o)
	}
	return p, nil
}

// Activate однократно открывает сеть после успешного Complete с точным OutputObserver.
func (p *Publisher) Activate(ctx context.Context, observer *core.OutputObserver) error {
	if ctx == nil || observer == nil {
		return core.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	if p.activating || p.active || p.stopping || p.joined {
		p.mu.Unlock()
		return core.ErrConflict
	}
	runCtx, cancel := context.WithCancel(ctx)
	p.activating, p.cancel = true, cancel
	p.mu.Unlock()

	// Общая блокировка может ждать управляющую операцию; Stop должен отменять
	// этот контекст, не ожидая окончания её захвата.
	err := p.execution.activate(runCtx, observer)
	p.mu.Lock()
	p.activating = false
	if err != nil {
		cancel()
		p.cancel = nil
		startAbort := p.startAbortLocked()
		p.mu.Unlock()
		if startAbort {
			go p.finish(nil)
		}
		return err
	}
	// Успешный допуск не возобновляет выход, если Stop уже победил.
	if p.stopping || runCtx.Err() != nil {
		p.stopping = true
		cancel()
		err = runCtx.Err()
		startAbort := p.startAbortLocked()
		p.mu.Unlock()
		if startAbort {
			go p.finish(nil)
		}
		return err
	}
	p.active, p.observer = true, observer
	go p.run(runCtx)
	p.mu.Unlock()
	return nil
}

// Reservation позволяет вызывающему коду отдельно подтвердить долговечное разрешение ExecutionFence.
func (p *Publisher) Reservation() *core.OutputExecutionReservation { return p.reservation }
func (p *Publisher) Done() <-chan struct{}                         { return p.done }

// Stop запрещает новые операции, отменяет сеть и ждёт фактическое завершение.
func (p *Publisher) Stop(ctx context.Context) error {
	p.mu.Lock()
	p.stopping = true
	cancel := p.cancel
	startAbort := p.startAbortLocked()
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	p.requestSeal()
	if startAbort {
		go p.finish(nil)
	}
	_, err := p.Wait(ctx)
	return err
}

// startAbortLocked передаёт завершение единственному работнику только после
// возврата незавершённой активации. Вызывающий код удерживает p.mu.
func (p *Publisher) startAbortLocked() bool {
	if !p.stopping || p.active || p.activating || p.abortStarted || p.joined {
		return false
	}
	p.abortStarted = true
	return true
}

// Abort завершает подготовленный выход; до Activate сеть не открывается.
func (p *Publisher) Abort(ctx context.Context) error { return p.Stop(ctx) }

// Wait не отменяет исполнение и допускает повторное ожидание после истечения срока.
func (p *Publisher) Wait(ctx context.Context) (Result, error) {
	select {
	case <-p.done:
		return p.Result(), nil
	default:
	}
	select {
	case <-p.done:
		return p.Result(), nil
	case <-ctx.Done():
		return p.Result(), errors.Join(ErrCleanupPending, ctx.Err())
	}
}

func (p *Publisher) Result() Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.result
}

// ReleaseJoined освобождает только доказанно завершённое и разрешённое исполнение.
// Итог окончательного отказа не выдаёт разрешение автоматически.
func (p *Publisher) ReleaseJoined() error {
	p.mu.Lock()
	joined := p.joined
	p.mu.Unlock()
	if !joined {
		return ErrCleanupPending
	}
	return p.execution.release()
}

func (p *Publisher) finish(err error) {
	p.requestSeal()
	<-p.sealDone
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.joined {
		return
	}
	if p.revoked && err == nil {
		err = core.ErrStale
	}
	p.joined = true
	p.result = Result{Err: err, NetworkJoined: true}
	if err == nil {
		p.state, p.failure = core.OutputStopped, FailureNone
	} else {
		p.state = core.OutputFailed
		p.failure = failureFor(err)
	}
	close(p.done)
}

func (p *Publisher) requestSeal() {
	p.sealOnce.Do(func() {
		// Захват общей блокировки может ждать чужую управляющую операцию.
		// Отдельный учтённый работник позволяет немедленно отменить сеть.
		go func() { p.execution.seal(); close(p.sealDone) }()
	})
}
