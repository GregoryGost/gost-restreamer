package core

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func dispatcherTestAuthority(t *testing.T) *RuntimeAuthority {
	t.Helper()
	a, err := NewRuntimeAuthority("owner", acceptanceObservationLimits(), OutputExecutionLimits{MaxReservations: 2, MaxIdentifierBytes: 1024, MaxBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestDispatcherReservationIsSharedAndClosesOnlyItsOwnLifetime(t *testing.T) {
	a := dispatcherTestAuthority(t)
	uow := &runtimeAuthorityUOW{authority: a}
	s, err := NewService(uow, "owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewService(uow, "owner")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	d, err := s.ReserveDispatcher(ctx)
	if err != nil || !d.Valid() {
		t.Fatal("диспетчер не получил допуск", err)
	}
	if _, err := other.ReserveDispatcher(ctx); !errors.Is(err, ErrInUse) {
		t.Fatal("второй Service получил параллельного диспетчера", err)
	}
	copied := &RuntimeDispatcher{authority: a, self: d, done: d.done}
	copied.valid.Store(true)
	if copied.Valid() || !errors.Is(copied.CloseContext(ctx), ErrStale) || !d.Valid() {
		t.Fatal("копия непрозрачной резервации изменила владельца")
	}
	d.Close()
	if d.Valid() {
		t.Fatal("закрытый диспетчер сохранил допуск")
	}
	select {
	case <-d.Done():
	default:
		t.Fatal("закрытие не завершило Done")
	}
	fresh, err := other.ReserveDispatcher(ctx)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	if !fresh.Valid() {
		t.Fatal("старое закрытие отозвало нового диспетчера")
	}
	a.Close()
	if fresh.Valid() {
		t.Fatal("закрытие authority не отозвало диспетчера")
	}
	select {
	case <-fresh.Done():
	default:
		t.Fatal("закрытие authority не завершило Done")
	}
	if err := fresh.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveDispatcher(ctx); !errors.Is(err, ErrClosed) {
		t.Fatal("закрытая authority выдала новый допуск", err)
	}
}

func TestDispatcherConcurrentAdmissionHasOneWinnerAndCancelledReleaseRetainsSlot(t *testing.T) {
	a := dispatcherTestAuthority(t)
	s, err := NewService(&runtimeAuthorityUOW{authority: a}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	const n = 12
	start := make(chan struct{})
	results := make(chan *RuntimeDispatcher, n)
	failures := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			<-start
			d, err := s.ReserveDispatcher(context.Background())
			if err == nil {
				results <- d
			} else {
				failures <- err
			}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	close(failures)
	if len(results) != 1 || len(failures) != n-1 {
		t.Fatalf("число победителей/отказов: %d/%d", len(results), len(failures))
	}
	for err := range failures {
		if !errors.Is(err, ErrInUse) {
			t.Fatal(err)
		}
	}
	d := <-results
	if err := a.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = d.CloseContext(ctx)
	a.release()
	if !errors.Is(err, context.Canceled) || !d.Valid() {
		t.Fatal("отменённое закрытие потеряло допуск", err)
	}
	if _, err := s.ReserveDispatcher(context.Background()); !errors.Is(err, ErrInUse) {
		t.Fatal("отменённое закрытие освободило слот", err)
	}
	d.Close()
}
