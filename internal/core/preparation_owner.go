package core

import (
	"context"
	"sync"
	"sync/atomic"
)

// PreparationOwner запрещает новые действия после отзыва и учитывает незавершённые операции.
// Register вызывается до первого возможного внешнего действия, finish — после полного завершения операции.
type PreparationOwner struct {
	authority  *RuntimeAuthority
	fence      PreparationFence
	valid      atomic.Bool
	done       chan struct{}
	operations uint64
}

func (o *PreparationOwner) Valid() bool             { return o != nil && o.valid.Load() }
func (o *PreparationOwner) Done() <-chan struct{}   { return o.done }
func (o *PreparationOwner) Fence() PreparationFence { return o.fence }
func (o *PreparationOwner) Register(ctx context.Context) (func(), error) {
	if o == nil {
		return nil, ErrInvalid
	}
	a := o.authority
	if err := a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	if !o.valid.Load() {
		return nil, ErrStale
	}
	if o.operations == ^uint64(0) {
		return nil, ErrBusy
	}
	o.operations++
	var once sync.Once
	return func() { once.Do(func() { <-a.gate; o.operations--; a.release() }) }, nil
}
func (o *PreparationOwner) Revoke(ctx context.Context) error {
	if o == nil {
		return ErrInvalid
	}
	a := o.authority
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	a.revokePreparation(o)
	return nil
}
func (a *RuntimeAuthority) revokePreparation(o *PreparationOwner) {
	if o.valid.Swap(false) {
		close(o.done)
	}
}
func (s *Service) BindPreparationOwner(ctx context.Context, f PreparationFence) (*PreparationOwner, error) {
	a, err := s.runtimeAuthority()
	if err != nil {
		return nil, err
	}
	if err = a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	err = s.uow.View(ctx, func(r Repositories) error {
		if e := runtimeIdleForPreparation(ctx, r); e != nil {
			return e
		}
		g, e := preparationFenced(ctx, r, f)
		if e != nil {
			return e
		}
		if f.Incarnation != s.incarnation || g.State != PreparationIssued || !g.Issued {
			return ErrStale
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if o := a.preparations[f.GenerationID]; o != nil {
		if o.fence != f || !o.valid.Load() {
			return nil, ErrStale
		}
		return o, nil
	}
	o := &PreparationOwner{authority: a, fence: f, done: make(chan struct{})}
	o.valid.Store(true)
	a.preparations[f.GenerationID] = o
	return o, nil
}
func (a *RuntimeAuthority) reconcilePreparations(ctx context.Context, uow UnitOfWork) {
	if len(a.preparations) == 0 {
		return
	}
	err := uow.View(ctx, func(r Repositories) error {
		for id, o := range a.preparations {
			g, e := preparationFenced(ctx, r, o.fence)
			if e != nil || g.State != PreparationIssued || !g.Issued {
				a.revokePreparation(o)
			}
			if e == nil && !g.Issued && !g.CleanupPending && g.State.Terminal() && o.operations == 0 {
				delete(a.preparations, id)
			}
		}
		return nil
	})
	if err != nil {
		for _, o := range a.preparations {
			a.revokePreparation(o)
		}
	}
}
