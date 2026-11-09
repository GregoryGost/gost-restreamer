package monitoring

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestAcceptanceSamplerCadenceLimitsRemovalAndJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := SamplerConfig{MaxSources: 1, StaleAfter: 5 * time.Second, ShutdownTimeout: time.Second}
		s, err := NewSessionSampler(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close(context.Background()) }()
		type observation struct {
			at      time.Time
			publish bool
		}
		calls := make(chan observation, 8)
		callback := func(_ context.Context, at time.Time, publish bool) error {
			calls <- observation{at, publish}
			return nil
		}
		if err = s.Register(1, callback); err != nil {
			t.Fatal(err)
		}
		if err = s.Register(1, callback); !errors.Is(err, core.ErrConflict) {
			t.Fatal("повторная регистрация принята", err)
		}
		if err = s.Register(2, callback); !errors.Is(err, core.ErrObservationLimit) {
			t.Fatal("превышение лимита источников принято", err)
		}
		baseline := <-calls
		if baseline.publish {
			t.Fatal("исходный снимок опубликован как готовая статистика")
		}
		for n := 1; n <= 4; n++ {
			time.Sleep(time.Second)
			got := <-calls
			if got.at.Sub(baseline.at) != time.Duration(n)*time.Second || got.publish != (n%2 == 0) {
				t.Fatalf("период сбора/публикации неверен: %d %+v", n, got)
			}
		}
		s.Remove(1)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if len(calls) != 0 || s.Stats().Sources != 0 {
			t.Fatal("удалённый источник продолжает наблюдения")
		}
		if err = s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-s.Done():
		default:
			t.Fatal("Close не подтвердил завершение работника")
		}
		if err = s.Register(3, callback); !errors.Is(err, core.ErrClosed) {
			t.Fatal("закрытый сборщик принял источник", err)
		}
	})
}

func TestAcceptanceSamplerPendingCallbackRetainsOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, err := NewSessionSampler(context.Background(), SamplerConfig{MaxSources: 1, StaleAfter: time.Second, ShutdownTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		entered, release := make(chan struct{}), make(chan struct{})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
			_ = s.Close(context.Background())
		}()
		if err = s.Register(1, func(context.Context, time.Time, bool) error { close(entered); <-release; return nil }); err != nil {
			t.Fatal(err)
		}
		<-entered
		if err = s.Close(context.Background()); !errors.Is(err, ErrSamplerPending) {
			t.Fatal("зависшая функция сбора не сохранила незавершённого владельца", err)
		}
		select {
		case <-s.Done():
			t.Fatal("завершение подтверждено до возврата функции сбора")
		default:
		}
		close(release)
		if err = s.Close(context.Background()); err != nil || s.Stats().Sources != 0 {
			t.Fatal("повторный Close не освободил источники", err)
		}
	})
}

func TestAcceptanceSamplerDoesNotReplayMissedTicksNearNextPeriod(t *testing.T) {
	for _, blocked := range []struct {
		name string
		call int
	}{{"initial_sample", 1}, {"periodic_sample", 3}} {
		t.Run(blocked.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, err := NewSessionSampler(context.Background(), SamplerConfig{MaxSources: 1, StaleAfter: 10 * time.Second, ShutdownTimeout: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				entered, release := make(chan struct{}), make(chan struct{})
				defer func() {
					select {
					case <-release:
					default:
						close(release)
					}
					_ = s.Close(context.Background())
				}()
				type sample struct {
					at      time.Time
					publish bool
				}
				var observations []sample
				var calls int
				if err = s.Register(1, func(_ context.Context, _ time.Time, publish bool) error {
					calls++
					if calls == blocked.call {
						close(entered)
						<-release
					}
					observations = append(observations, sample{at: time.Now(), publish: publish})
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				time.Sleep(time.Duration(blocked.call-1) * time.Second)
				<-entered
				time.Sleep((7999 - time.Duration(blocked.call-1)*1000) * time.Millisecond)
				close(release)
				synctest.Wait()
				time.Sleep(time.Millisecond)
				synctest.Wait()
				for i := 1; i < len(observations); i++ {
					if gap := observations[i].at.Sub(observations[i-1].at); gap < time.Second {
						t.Fatalf("пропущенный период повторён вместо удлинения окна: интервал=%v, измерения=%+v", gap, observations)
					}
				}
			})
		})
	}
}

func TestAcceptanceSamplerTransientSourcesCannotStarveExistingSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, err := NewSessionSampler(context.Background(), SamplerConfig{MaxSources: 2, StaleAfter: 5 * time.Second, ShutdownTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close(context.Background()) }()
		var samples, publications int
		if err = s.Register(1, func(_ context.Context, _ time.Time, publish bool) error {
			samples++
			if publish {
				publications++
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		for range 10 {
			time.Sleep(500 * time.Millisecond)
			if err = s.Register(2, func(context.Context, time.Time, bool) error { return nil }); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			s.Remove(2)
		}
		if samples != 6 || publications != 2 {
			t.Fatalf("быстрая смена соседнего источника остановила сбор: измерения=%d публикации=%d", samples, publications)
		}
	})
}
