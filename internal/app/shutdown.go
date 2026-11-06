package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"net"
	"net/http"
)

var ErrPending = errors.New("завершение приложения ещё не подтверждено")

type stopResult struct {
	err    error
	joined bool
}
type operation struct {
	done   chan struct{}
	result stopResult
}

func startOperation(call func() stopResult) *operation {
	op := &operation{done: make(chan struct{})}
	go func() { op.result = call(); close(op.done) }()
	return op
}
func isClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}
func (op *operation) wait(ctx context.Context) bool {
	if isClosed(op.done) {
		return true
	}
	select {
	case <-op.done:
		return true
	case <-ctx.Done():
		return isClosed(op.done)
	}
}

type stopOwner struct {
	name     string
	close    func(context.Context) stopResult
	current  *operation
	joined   bool
	recorded bool
}

func newStopOwner(name string, close func(context.Context) error) *stopOwner {
	return &stopOwner{name: name, close: func(ctx context.Context) stopResult { err := close(ctx); return stopResult{err, err == nil} }}
}
func sharedOwner(name string, close func(context.Context) error) *stopOwner {
	return &stopOwner{name: name, close: func(ctx context.Context) stopResult { return stopResult{close(ctx), true} }}
}
func listenerOwner(name string, listener net.Listener) *stopOwner {
	return sharedOwner(name, func(context.Context) error {
		err := listener.Close()
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	})
}
func (o *stopOwner) begin(ctx context.Context) {
	if o != nil && !o.joined && o.current == nil {
		o.current = startOperation(func() stopResult { return o.close(ctx) })
		o.recorded = false
	}
}
func (a *Application) waitOwner(ctx context.Context, o *stopOwner) bool {
	if o == nil || o.joined {
		return true
	}
	for attempt := 0; attempt < 2; attempt++ {
		o.begin(ctx)
		if !o.current.wait(ctx) {
			return false
		}
		result := o.current.result
		if !o.recorded && result.err != nil {
			a.errors = append(a.errors, fmt.Errorf("%s: %w", o.name, result.err))
		}
		o.recorded = true
		if result.joined {
			o.joined = true
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		// Повтор ждёт того же владельца и сохраняет ошибку первой завершённой работы.
		o.current = nil
	}
	return false
}
func (a *Application) waitOwners(ctx context.Context, owners ...*stopOwner) bool {
	for _, owner := range owners {
		owner.begin(ctx)
	}
	joined := true
	for _, owner := range owners {
		if !a.waitOwner(ctx, owner) {
			joined = false
		}
	}
	return joined
}

// Shutdown ограничивает ожидание даже для синхронного Close, который не уважает ctx.
// Неоконченные операции остаются у Application и наблюдаются повторным вызовом.
func (a *Application) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return ErrPending
	}
	if a.config.Lifecycle.ShutdownTimeout > 0 {
		bounded, cancel := context.WithTimeout(ctx, a.config.Lifecycle.ShutdownTimeout)
		defer cancel()
		ctx = bounded
	}
	select {
	case <-a.gate:
		defer func() { a.gate <- struct{}{} }()
	case <-ctx.Done():
		return errors.Join(ErrPending, ctx.Err())
	}
	if isClosed(a.done) {
		return errors.Join(a.errors...)
	}
	// Все независимые владельцы получают сигнал остановки до ожидания первого результата.
	joined := a.waitOwners(ctx, a.owners.api, a.owners.transport, a.owners.manager, a.owners.ingest, a.owners.preparation, a.owners.gpu, a.owners.httpListener, a.owners.ingestListener)
	if a.serveCancel != nil {
		a.serveCancel()
	}
	for _, op := range []*operation{a.httpServe, a.ingestServe} {
		if op == nil {
			continue
		}
		if !op.wait(ctx) {
			joined = false
			continue
		}
		err := op.result.err
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, context.Canceled) && !errors.Is(err, core.ErrClosed) {
			a.errors = append(a.errors, err)
		}
	}
	if !joined {
		return errors.Join(errors.Join(a.errors...), ErrPending, ctx.Err())
	}
	if !a.waitOwners(ctx, a.owners.registry, a.owners.files) {
		return errors.Join(errors.Join(a.errors...), ErrPending, ctx.Err())
	}
	if !a.waitOwners(ctx, a.owners.store) {
		return errors.Join(errors.Join(a.errors...), ErrPending, ctx.Err())
	}
	if !a.waitOwners(ctx, a.owners.logger) {
		return errors.Join(errors.Join(a.errors...), ErrPending, ctx.Err())
	}
	a.doneOnce.Do(func() { close(a.done) })
	return errors.Join(a.errors...)
}
