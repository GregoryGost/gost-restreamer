package monitoring

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет фактические сроки сбора и публикации, предел источников, освобождение регистрации и запрет
// новых источников после завершения работника.
func TestAcceptanceSamplerCadenceLimitsRemovalAndJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет периоды чтения и публикации, предел источников, удаление и подтверждённое завершение
		// сборщика в управляемом времени.

		// cfg задаёт сборщик одного источника с конечными сроками свежести и остановки.
		cfg := SamplerConfig{MaxSources: 1, StaleAfter: 5 * time.Second, ShutdownTimeout: time.Second}
		// s содержит проверяемый сборщик; err содержит отказ его создания.
		s, err := NewSessionSampler(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			// Закрывает сборщик после проверки его периодов и допуска источников.
			_ = s.Close(context.Background())
		}()
		// observation сохраняет фактическое время вызова и разрешение публикации для проверки
		// периодов.
		type observation struct {
			// at содержит переданное сборщиком время текущего чтения.
			at time.Time
			// publish показывает разрешение публикации именно в этом вызове.
			publish bool
		}
		// calls передаёт наблюдённые времена чтений и публикаций тесту.
		calls := make(chan observation, 8)
		// callback сохраняет время и publish каждого чтения без внешнего ввода-вывода.
		callback := func(_ context.Context, at time.Time, publish bool) error {
			// Сохраняет время вызова и разрешение публикации для проверки фактических периодов.

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
		// baseline содержит первый вызов без публикации и задаёт начало проверки последующих
		// периодов.
		baseline := <-calls
		if baseline.publish {
			t.Fatal("исходный снимок опубликован как готовая статистика")
		}
		// n задаёт очередную прошедшую секунду для проверки времени и двухсекундной публикации.
		for n := 1; n <= 4; n++ {
			time.Sleep(time.Second)
			// got содержит наблюдённый очередной вызов сборщика для сравнения с baseline.
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

// Проверяет сохранение незавершённого владельца при блокировке функции чтения и освобождение
// источников после повторного Close.
func TestAcceptanceSamplerPendingCallbackRetainsOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет сохранение незавершённого владельца до возврата заблокированной функции сбора.

		// s содержит сборщик сценария зависшей функции; err содержит отказ создания.
		s, err := NewSessionSampler(context.Background(), SamplerConfig{MaxSources: 1, StaleAfter: time.Second, ShutdownTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		// entered подтверждает начало функции чтения; release разрешает ей завершиться после
		// проверки pending.
		entered, release := make(chan struct{}), make(chan struct{})
		defer func() {
			// Разблокирует функцию сбора и завершает работника после проверки ErrSamplerPending.

			select {
			case <-release:
			default:
				close(release)
			}
			_ = s.Close(context.Background())
		}()
		if err = s.Register(1, func(context.Context, time.Time, bool) error {
			// Сообщает о начале сбора и удерживает функцию до разрешения теста.
			close(entered)
			<-release
			return nil
		}); err != nil {
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

// Проверяет отсутствие догоняющих чтений после долгой блокировки первоначальной или периодической
// выборки у границы следующего периода.
func TestAcceptanceSamplerDoesNotReplayMissedTicksNearNextPeriod(t *testing.T) {
	// blocked выбирает задержанное начальное либо периодическое чтение для проверки пропущенных
	// сроков.
	for _, blocked := range []struct {
		// name задаёт название выбранной точки задержки.
		name string
		// call задаёт порядковый номер вызова, который удерживается тестом.
		call int
	}{{"initial_sample", 1}, {"periodic_sample", 3}} {
		t.Run(blocked.name, func(t *testing.T) {
			// Выбирает задержку начального либо периодического измерения для проверки пропущенных периодов.

			synctest.Test(t, func(t *testing.T) {
				// Проверяет, что после выбранного задержанного чтения сборщик не выполняет пропущенные периоды подряд.

				// s содержит отдельный сборщик сценария пропущенных периодов; err
				// содержит отказ его создания.
				s, err := NewSessionSampler(context.Background(), SamplerConfig{MaxSources: 1, StaleAfter: 10 * time.Second, ShutdownTimeout: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				// entered подтверждает начало выбранного задержанного вызова; release
				// разрешает его позднее завершение.
				entered, release := make(chan struct{}), make(chan struct{})
				defer func() {
					// Освобождает задержанное чтение и закрывает сборщик после проверки интервалов.

					select {
					case <-release:
					default:
						close(release)
					}
					_ = s.Close(context.Background())
				}()
				// sample сохраняет фактическое завершение чтения и разрешение его
				// публикации.
				type sample struct {
					// at содержит время завершения функции чтения после возможной
					// блокировки.
					at time.Time
					// publish сохраняет решение сборщика о публикации для этого
					// чтения.
					publish bool
				}
				// observations собирает завершённые чтения для проверки отсутствия
				// догоняющих вызовов.
				var observations []sample
				// calls считает вызовы, чтобы задержать только выбранный blocked.call.
				var calls int
				if err = s.Register(1, func(_ context.Context, _ time.Time, publish bool) error {
					// Задерживает выбранный вызов и сохраняет фактическое время завершения каждого чтения.

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
				// i указывает очередное наблюдение, сравниваемое с предыдущим
				// завершённым чтением.
				for i := 1; i < len(observations); i++ {
					// gap содержит интервал между соседними завершёнными чтениями,
					// который не должен быть меньше секунды.
					if gap := observations[i].at.Sub(observations[i-1].at); gap < time.Second {
						t.Fatalf("пропущенный период повторён вместо удлинения окна: интервал=%v, измерения=%+v", gap, observations)
					}
				}
			})
		})
	}
}

// Проверяет, что частая регистрация и удаление соседнего источника не откладывают сбор и публикацию
// уже работающего.
func TestAcceptanceSamplerTransientSourcesCannotStarveExistingSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет, что частая смена соседнего источника не переносит сроки уже работающего источника.

		// s содержит сборщик постоянного и сменяемого соседнего источника; err содержит отказ
		// создания.
		s, err := NewSessionSampler(context.Background(), SamplerConfig{MaxSources: 2, StaleAfter: 5 * time.Second, ShutdownTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			// Закрывает сборщик после проверки частых регистраций соседнего источника.
			_ = s.Close(context.Background())
		}()
		// samples считает чтения постоянного источника; publications считает его разрешённые
		// публикации при частой смене соседа.
		var samples, publications int
		if err = s.Register(1, func(_ context.Context, _ time.Time, publish bool) error {
			// Считает чтения и публикации постоянного источника во время смены соседнего.

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
			if err = s.Register(2, func(context.Context, time.Time, bool) error {
				// Завершает чтение краткоживущего соседнего источника без изменения счётчиков основного.
				return nil
			}); err != nil {
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
