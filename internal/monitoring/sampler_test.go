package monitoring

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// samplerConfig задаёт два источника и конечные сроки свежести и завершения для управляемых сценариев
// сборщика.
func samplerConfig() SamplerConfig {
	return SamplerConfig{MaxSources: 2, StaleAfter: 5 * time.Second, ShutdownTimeout: time.Second}
}

// Проверяет секундный сбор, двухсекундную публикацию, пределы регистрации, удаление источника и
// подтверждённую повторную остановку.
func TestSamplerCadenceRegistrationAndJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет периоды, независимость регистрации, удаление источника и повторное закрытие сборщика.

		// s содержит сборщик двух источников; err содержит отказ его создания.
		s, err := NewSessionSampler(context.Background(), samplerConfig())
		if err != nil {
			t.Fatal(err)
		}
		// samples считает чтения основного источника; published считает только вызовы с
		// разрешённой публикацией.
		var samples, published int
		// callback успешно читает дополнительный источник, не меняя счётчики основного.
		callback := func(context.Context, time.Time, bool) error {
			// Успешно завершает чтение дополнительного источника без изменения счётчиков основного.
			return nil
		}
		if err = s.Register(1, func(_ context.Context, _ time.Time, publish bool) error {
			// Считает чтения и публикации основного источника для проверки периодов сборщика.

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

// Проверяет, что зависшая функция чтения сохраняет единственного работника после ошибки
// незавершённости до явного разрешения завершения.
func TestSamplerPendingKeepsOneWorkerUntilPhysicalJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет сохранение одного работника до фактического возврата задержанного чтения.

		// s содержит сборщик с намеренно задержанным чтением; err содержит отказ его создания.
		s, err := NewSessionSampler(context.Background(), samplerConfig())
		if err != nil {
			t.Fatal(err)
		}
		// release удерживает функцию чтения до проверки незавершённого владельца.
		release := make(chan struct{})
		// started подтверждает вход работника в задержанную функцию чтения.
		started := make(chan struct{})
		if err = s.Register(1, func(context.Context, time.Time, bool) error {
			// Сообщает о начале чтения и удерживает его до снятия блокировки тестом.
			close(started)
			<-release
			return nil
		}); err != nil {
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

// Проверяет неверные и отменённые параметры запуска, отклонение пустых регистраций и учёт ошибки
// чтения до остановки.
func TestSamplerFailuresAndValidation(t *testing.T) {
	//nolint:staticcheck // Проверяется отклонение отсутствующего контекста до запуска сборщика.
	// err содержит отказ создания сборщика без родительского контекста.
	if _, err := NewSessionSampler(nil, samplerConfig()); !errors.Is(err, core.ErrInvalid) {
		t.Fatal(err)
	}
	// err содержит отказ создания сборщика с пустой конфигурацией.
	if _, err := NewSessionSampler(context.Background(), SamplerConfig{}); !errors.Is(err, core.ErrInvalid) {
		t.Fatal(err)
	}
	// ctx задаёт уже отменённого родителя; cancel отменяет его до конструктора.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// err содержит отказ запуска от уже отменённого родительского контекста.
	if _, err := NewSessionSampler(ctx, samplerConfig()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		// Проверяет ошибки регистрации и учёт отказа функции чтения с последующей отменой родительского
		// контекста.

		// ctx управляет работником сценария ошибок чтения; cancel завершает его после проверки
		// счётчика.
		ctx, cancel := context.WithCancel(context.Background())
		// s содержит сборщик, в котором проверяются недопустимые регистрации и рабочая ошибка
		// источника.
		s, _ := NewSessionSampler(ctx, samplerConfig())
		defer cancel()
		// err содержит отказ регистрации источника с нулевым идентификатором.
		if err := s.Register(0, func(context.Context, time.Time, bool) error {
			// Возвращает успех, но регистрация с нулевым идентификатором должна отклониться до вызова этой
			// функции.
			return nil
		}); !errors.Is(err, core.ErrInvalid) {
			t.Fatal(err)
		}
		// err содержит отказ регистрации отсутствующей функции чтения.
		if err := s.Register(1, nil); !errors.Is(err, core.ErrInvalid) {
			t.Fatal(err)
		}
		// err содержит результат регистрации функции, намеренно возвращающей core.ErrStale.
		if err := s.Register(1, func(context.Context, time.Time, bool) error {
			// Возвращает ErrStale для проверки счётчика ошибок чтения источника.
			return core.ErrStale
		}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if s.Stats().Failures != 1 {
			t.Fatal(s.Stats())
		}
		cancel()
		synctest.Wait()
		// err содержит итог закрытия после отмены родительского контекста.
		if err := s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}
