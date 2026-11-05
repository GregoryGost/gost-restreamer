package core

import (
	"context"
	"sync/atomic"
)

// RuntimeDispatcher резервирует единственного диспетчера общей RuntimeAuthority.
// Закрытие завершает логический допуск; физическую очистку выполняет владелец ресурсов.
type RuntimeDispatcher struct {
	authority *RuntimeAuthority
	self      *RuntimeDispatcher
	valid     atomic.Bool
	done      chan struct{}
}

func (d *RuntimeDispatcher) Valid() bool { return d != nil && d.self == d && d.valid.Load() }
func (d *RuntimeDispatcher) Done() <-chan struct{} {
	if d == nil {
		return nil
	}
	return d.done
}

func (d *RuntimeDispatcher) closeLocked() {
	if d.valid.Swap(false) {
		close(d.done)
	}
	if d.authority.dispatcher == d {
		d.authority.dispatcher = nil
	}
}

// Close идемпотентно освобождает только собственную резервацию, включая после Close authority.
func (d *RuntimeDispatcher) Close() {
	_ = d.CloseContext(context.Background())
}

// CloseContext ограничивает ожидание authority; отмена сохраняет действующую резервацию.
func (d *RuntimeDispatcher) CloseContext(ctx context.Context) error {
	if d == nil || d.authority == nil {
		return nil
	}
	if d.self != d {
		return ErrStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a := d.authority
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.gate:
	}
	defer a.release()
	if err := ctx.Err(); err != nil {
		return err
	}
	d.closeLocked()
	return nil
}

// ReserveDispatcher не создаёт процессы и не меняет долговечное состояние.
func (s *Service) ReserveDispatcher(ctx context.Context) (*RuntimeDispatcher, error) {
	a, err := s.runtimeAuthority()
	if err != nil {
		return nil, err
	}
	if err = a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	if a.dispatcher != nil {
		return nil, ErrInUse
	}
	d := &RuntimeDispatcher{authority: a, done: make(chan struct{})}
	d.self = d
	d.valid.Store(true)
	a.dispatcher = d
	return d, nil
}
