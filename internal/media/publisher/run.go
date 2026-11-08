package publisher

import (
	"context"
	"errors"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
)

func (p *Publisher) run(ctx context.Context) {
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
		case <-p.execution.done():
			p.mu.Lock()
			if !p.stopping && ctx.Err() == nil {
				p.revoked = true
			}
			p.mu.Unlock()
			p.cancel()
		}
	}()
	err := p.loop(ctx)
	p.requestSeal()
	p.cancel()
	<-watchDone
	p.finish(err)
}

func (p *Publisher) observe(ctx context.Context, state core.OutputState, reason FailureReason) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	value := core.OutputObservation{State: state, Error: string(reason)}
	var err error
	if observer, ok := p.observer.(terminalObservation); ok && (state == core.OutputFailed || state == core.OutputStopped) {
		err = observer.ObserveTerminal(ctx, value)
	} else {
		err = p.observer.Observe(ctx, value)
	}
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.state, p.failure = state, reason
	p.mu.Unlock()
	if p.options.Logger != nil {
		p.options.Logger.Info("publisher_state", "operation", "observe", "state", state, "reason", reason)
	}
	return nil
}

func (p *Publisher) loop(ctx context.Context) error {
	delay := p.options.RetryInitial
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := p.observe(ctx, core.OutputConnecting, FailureNone); err != nil {
			return stoppedError(ctx, err)
		}
		p.mu.Lock()
		saturate(&p.metrics.Attempts, 1, &p.metrics.Saturated)
		p.mu.Unlock()
		ready, err := p.attempt(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			return nil
		}
		if p.options.Logger != nil {
			p.options.Logger.Error("publisher_attempt", "operation", "publish", "ready", ready, "error", err)
		}
		if terminal(err) {
			_ = p.observe(ctx, core.OutputFailed, failureFor(err))
			return err
		}
		if err = p.observe(ctx, core.OutputRetrying, FailureNetwork); err != nil {
			return stoppedError(ctx, err)
		}
		if ready {
			delay = p.options.RetryInitial
		}
		if !pause(ctx, delay) {
			return nil
		}
		delay = nextDelay(delay, p.options.RetryMax)
	}
}

func stoppedError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func terminal(err error) bool {
	return rtmp.IsOutboundTerminal(err) || errors.Is(err, core.ErrStale) || errors.Is(err, core.ErrClosed) || errors.Is(err, core.ErrInvalid) || errors.Is(err, core.ErrConflict) || errors.Is(err, core.ErrInUse) || errors.Is(err, core.ErrNotReady) || errors.Is(err, core.ErrObservationLimit) || errors.Is(err, media.ErrRouterClosed) || errors.Is(err, media.ErrRouterBudget) || errors.Is(err, media.ErrSubscriptionGeneration)
}

func nextDelay(current, capDelay time.Duration) time.Duration {
	if current >= capDelay || current > capDelay-current {
		return capDelay
	}
	return current * 2
}

func pause(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

func (p *Publisher) attempt(ctx context.Context) (ready bool, result error) {
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	options := p.options.Transport
	options.Admit, options.OnWrite = p.execution.admit, p.noteWrite
	options.OnACKAnomaly = p.noteACKAnomaly
	p.mu.Lock()
	p.metrics.WireBufferBytes = options.Limits.MaxRetainedBytes
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.metrics.WireBufferBytes = 0
		p.mu.Unlock()
	}()
	conn, err := p.dial(attemptCtx, p.target, options)
	if err != nil {
		return false, err
	}
	if conn == nil {
		return false, core.ErrInvalid
	}
	// Даже поздно вернувшееся соединение проходит Close и Wait до NetworkJoined.
	defer func() {
		if terminal(result) {
			if ctx.Err() == nil {
				if p.options.Logger != nil {
					p.options.Logger.Error("publisher_failure", "operation", "publish", "error", result)
				}
				_ = p.observe(ctx, core.OutputFailed, failureFor(result))
			}
			p.requestSeal()
		}
		cancel()
		p.closeSubscription()
		_ = conn.Close()
		joinErr := conn.Wait(context.Background())
		p.mu.Lock()
		p.conn = nil
		p.mu.Unlock()
		if result == nil && ctx.Err() == nil && joinErr != nil {
			result = joinErr
		}
	}()
	p.mu.Lock()
	if p.hadConnection {
		saturate(&p.metrics.Reconnects, 1, &p.metrics.Saturated)
	}
	p.hadConnection = true
	saturate(&p.metrics.Connections, 1, &p.metrics.Saturated)
	p.conn = conn
	p.mu.Unlock()
	if attemptCtx.Err() != nil {
		return false, attemptCtx.Err()
	}
	readCtx, stopRead := context.WithCancel(attemptCtx)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-conn.Done():
			stopRead()
		case <-readCtx.Done():
		}
	}()
	defer func() { stopRead(); <-watchDone }()
	for {
		if readCtx.Err() != nil {
			return ready, connectionError(ctx, conn)
		}
		sub, err := p.router.Subscribe(p.options.Queue)
		if err != nil {
			return ready, err
		}
		p.attachSubscription(sub)
		generationReady, err := p.forward(readCtx, conn, sub)
		ready = ready || generationReady
		p.closeSubscription()
		if readCtx.Err() != nil {
			return ready, connectionError(ctx, conn)
		}
		if !errors.Is(err, media.ErrResyncTimeout) {
			return ready, err
		}
		// Истечение срока синхронизации обновляет очередь, сохраняя соединение и начало отсчёта.
		if !pause(readCtx, p.options.ResubscribeDelay) {
			return ready, connectionError(ctx, conn)
		}
	}
}

func connectionError(ctx context.Context, conn connection) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := conn.Err(); err != nil {
		return err
	}
	return context.Canceled
}

func (p *Publisher) forward(ctx context.Context, conn connection, sub *media.Subscription) (ready bool, err error) {
	var generation uint64
	var video, audio, publishing bool
	for {
		delivery, err := sub.Next(ctx)
		if err != nil {
			return ready, err
		}
		p.hold(delivery)
		if !delivery.IsCurrent() {
			p.abandoned(dropStale)
			continue
		}
		if err = ctx.Err(); err != nil {
			p.abandoned(dropCancelled)
			return ready, err
		}
		if delivery.Generation != generation {
			video, audio, publishing = false, false, false
			if err = p.observe(ctx, core.OutputConnecting, FailureNone); err != nil {
				p.abandoned(p.dropFor(ctx, err))
				return ready, err
			}
			// Транспорт проверяет допуск до сериализации и внешних операций.
			if !delivery.IsCurrent() {
				p.abandoned(dropStale)
				continue
			}
			if err = conn.WriteHeaders(ctx, delivery.VideoConfig, delivery.AudioConfig); err != nil {
				p.abandoned(p.dropFor(ctx, err))
				return ready, err
			}
			generation = delivery.Generation
		}
		if !delivery.IsCurrent() {
			p.abandoned(dropStale)
			generation = 0
			continue
		}
		written, err := conn.WriteDelivery(ctx, delivery)
		if err != nil {
			if errors.Is(err, media.ErrStaleSource) {
				p.abandoned(dropStale)
				generation = 0
				continue
			}
			p.abandoned(p.dropFor(ctx, err))
			return ready, err
		}
		p.delivered(delivery, written.PayloadBytes)
		if delivery.Packet.Config().Info().Track == media.TrackVideo {
			video = true
		} else {
			audio = true
		}
		if !delivery.IsCurrent() {
			generation, video, audio, publishing = 0, false, false, false
			continue
		}
		if video && audio && !publishing {
			if err = p.observe(ctx, core.OutputPublishing, FailureNone); err != nil {
				return ready, err
			}
			ready = true
			publishing = true
		}
	}
}

func (p *Publisher) dropFor(ctx context.Context, err error) dropReason {
	if ctx.Err() != nil {
		return dropCancelled
	}
	if terminal(err) {
		return dropTerminal
	}
	return dropTransport
}
