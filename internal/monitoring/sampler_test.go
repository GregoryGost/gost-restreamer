package monitoring

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func samplerConfig() SamplerConfig {
	return SamplerConfig{MaxSources: 2, StaleAfter: 5 * time.Second, ShutdownTimeout: time.Second}
}
func TestSamplerCadenceRegistrationAndJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, err := NewSessionSampler(context.Background(), samplerConfig())
		if err != nil {
			t.Fatal(err)
		}
		var samples, published int
		callback := func(context.Context, time.Time, bool) error { return nil }
		if err = s.Register(1, func(_ context.Context, _ time.Time, publish bool) error {
			samples++
			if publish {
				published++
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if samples != 1 || published != 0 {
			t.Fatalf("начальное чтение: %d/%d", samples, published)
		}
		if err = s.Register(1, callback); !errors.Is(err, core.ErrConflict) {
			t.Fatal(err)
		}
		if err = s.Register(2, callback); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if samples != 1 {
			t.Fatal("регистрация изменила окно существующего производителя")
		}
		if err = s.Register(3, callback); !errors.Is(err, core.ErrObservationLimit) {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if samples != 3 || published != 1 {
			t.Fatalf("период: %d/%d", samples, published)
		}
		s.Remove(1)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if samples != 3 {
			t.Fatal("удалённая функция чтения вызвана")
		}
		if err = s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if s.Stats().Sources != 0 {
			t.Fatal("источники удерживаются после завершения")
		}
		if err = s.Register(1, callback); !errors.Is(err, core.ErrClosed) {
			t.Fatal(err)
		}
		if err = s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}
func TestSamplerPendingKeepsOneWorkerUntilPhysicalJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, err := NewSessionSampler(context.Background(), samplerConfig())
		if err != nil {
			t.Fatal(err)
		}
		release := make(chan struct{})
		started := make(chan struct{})
		if err = s.Register(1, func(context.Context, time.Time, bool) error { close(started); <-release; return nil }); err != nil {
			t.Fatal(err)
		}
		<-started
		if err = s.Close(context.Background()); !errors.Is(err, ErrSamplerPending) {
			t.Fatalf("нет pending: %v", err)
		}
		select {
		case <-s.Done():
			t.Fatal("ложное завершение")
		default:
		}
		close(release)
		synctest.Wait()
		if err = s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}
func TestSamplerFailuresAndValidation(t *testing.T) {
	//nolint:staticcheck // Проверяется отклонение отсутствующего контекста до запуска сборщика.
	if _, err := NewSessionSampler(nil, samplerConfig()); !errors.Is(err, core.ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := NewSessionSampler(context.Background(), SamplerConfig{}); !errors.Is(err, core.ErrInvalid) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSessionSampler(ctx, samplerConfig()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		s, _ := NewSessionSampler(ctx, samplerConfig())
		defer cancel()
		if err := s.Register(0, func(context.Context, time.Time, bool) error { return nil }); !errors.Is(err, core.ErrInvalid) {
			t.Fatal(err)
		}
		if err := s.Register(1, nil); !errors.Is(err, core.ErrInvalid) {
			t.Fatal(err)
		}
		if err := s.Register(1, func(context.Context, time.Time, bool) error { return core.ErrStale }); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if s.Stats().Failures != 1 {
			t.Fatal(s.Stats())
		}
		cancel()
		synctest.Wait()
		if err := s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}
