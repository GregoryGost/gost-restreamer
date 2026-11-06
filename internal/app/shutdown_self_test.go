package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestShutdownInitiatesIndependentOwnersAndRetainsShared(t *testing.T) {
	a := &Application{gate: make(chan struct{}, 1), done: make(chan struct{})}
	a.gate <- struct{}{}
	blocked := make(chan struct{})
	started := make(chan struct{})
	a.owners.manager = &stopOwner{name: "manager", close: func(context.Context) stopResult { close(started); <-blocked; return stopResult{nil, true} }}
	gpuStopped := make(chan struct{})
	a.owners.gpu = &stopOwner{name: "gpu", close: func(context.Context) stopResult { close(gpuStopped); return stopResult{nil, true} }}
	sharedClosed := make(chan struct{})
	a.owners.store = sharedOwner("store", func(context.Context) error { close(sharedClosed); return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := a.Shutdown(ctx); !errors.Is(err, ErrPending) {
		t.Fatalf("незавершённый Manager: %v", err)
	}
	<-started
	select {
	case <-gpuStopped:
	default:
		t.Fatal("GPU не получил независимую остановку")
	}
	select {
	case <-sharedClosed:
		t.Fatal("shared закрыт до join")
	default:
	}
	close(blocked)
	ctx2, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := a.Shutdown(ctx2); err != nil {
		t.Fatal(err)
	}
	if !isClosed(a.Done()) || !isClosed(sharedClosed) {
		t.Fatal("поздний join не завершил приложение")
	}
}
func TestShutdownPreservesHistoricalErrorAfterRepeatedClose(t *testing.T) {
	a := &Application{gate: make(chan struct{}, 1), done: make(chan struct{})}
	a.gate <- struct{}{}
	failure := errors.New("ошибка завершённого задания")
	attempts := 0
	a.owners.preparation = newStopOwner("preparation", func(context.Context) error {
		attempts++
		if attempts == 1 {
			return failure
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Shutdown(ctx); !errors.Is(err, failure) || errors.Is(err, ErrPending) {
		t.Fatalf("историческая ошибка: %v", err)
	}
	if attempts != 2 || !isClosed(a.Done()) {
		t.Fatal("повтор Close не подтвердил join")
	}
}
