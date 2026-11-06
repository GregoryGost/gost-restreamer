package gpu

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

func TestNativeClaimRetainedAcrossPendingPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		backend := &controlledProvider{initReason: Available, stopReason: Available, poll: func(Limits) Sample {
			close(entered)
			<-release
			return Sample{Reason: Available}
		}}
		p := newNativeProvider(backend)
		c, err := Start(testConfig(), p, func(Snapshot) {}, func() {})
		if err != nil {
			t.Fatal(err)
		}
		<-entered
		if _, err := Start(testConfig(), p, func(Snapshot) {}, func() {}); !errors.Is(err, ErrBusy) {
			t.Fatal("повторный Collector получил тот же нативный Provider")
		}
		if err := c.Close(context.Background()); !errors.Is(err, ErrPending) {
			t.Fatal(err)
		}
		replacement := newNativeProvider(&controlledProvider{initReason: Available, stopReason: Available})
		if replacement.Init() != Busy || replacement.Shutdown() != Available {
			t.Fatal("незавершённый владелец допускает замену")
		}
		close(release)
		if err := c.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if backend.starts.Load() != 1 || backend.stops.Load() != 1 || p.Init() != Busy || p.Poll(Limits{}).Reason != ProviderError || p.Shutdown() != Available {
			t.Fatal("нарушена однократность нативного жизненного цикла")
		}
		next := newNativeProvider(&controlledProvider{initReason: Available, stopReason: Available})
		if next.Init() != Available || next.Shutdown() != Available {
			t.Fatal("подтверждённое завершение не освобождает владельца")
		}
	})
}

func TestNativeInitializationFailureDoesNotRepeatInitOrGrowOwners(t *testing.T) {
	for _, reason := range []Reason{LibraryUnavailable, DriverUnavailable} {
		backend := &controlledProvider{initReason: reason, stopReason: ProviderError}
		p := newNativeProvider(backend)
		if p.Init() != reason || p.Init() != Busy || backend.starts.Load() != 1 || p.claimCollector() {
			t.Fatal("ошибка Init допускает повторную загрузку привязки")
		}
		got := p.Shutdown()
		if reason == LibraryUnavailable {
			if got != Available || backend.stops.Load() != 0 {
				t.Fatal("отсутствующая библиотека вызвала нативный Shutdown")
			}
			next := newNativeProvider(&controlledProvider{initReason: Available, stopReason: Available})
			if next.Init() != Available || next.Shutdown() != Available {
				t.Fatal("отсутствующая библиотека удержала владельца")
			}
		} else {
			if got != ProviderError || p.Shutdown() != ProviderError || backend.stops.Load() != 1 {
				t.Fatal("неопределённый Shutdown повторяет нативный вызов")
			}
			next := newNativeProvider(&controlledProvider{initReason: Available, stopReason: Available})
			if next.Init() != Busy || next.Shutdown() != Available {
				t.Fatal("неопределённый Shutdown допускает нового владельца")
			}
			// Синтетический Provider освобождается тестом; реальные ошибки сохраняют владельца.
			nativeOwner.Lock()
			if nativeOwner.provider != p {
				nativeOwner.Unlock()
				t.Fatal("потерян точный нативный владелец")
			}
			nativeOwner.provider = nil
			nativeOwner.Unlock()
		}
	}
}
