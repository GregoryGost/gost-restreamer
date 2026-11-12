package gpu

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

// Проверяет сохранение единственного нативного владельца при зависшем Poll и запрет повторного
// Collector до физического завершения.
func TestNativeClaimRetainedAcrossPendingPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет сохранение единственного нативного владельца при незавершённом Poll и его освобождение
		// после возврата.

		// entered подтверждает начало удерживаемого Poll; release разрешает ему вернуть
		// результат.
		entered, release := make(chan struct{}), make(chan struct{})
		// backend моделирует успешные Init и Shutdown с намеренно заблокированным опросом.
		backend := &controlledProvider{initReason: Available, stopReason: Available, poll: func(Limits) Sample {
			// Задерживает опрос нативного поставщика до разрешения теста.

			close(entered)
			<-release
			return Sample{Reason: Available}
		}}
		// p оборачивает управляемый источник настоящим протоколом процессного владения.
		p := newNativeProvider(backend)
		// c содержит единственный Collector нативного источника; err содержит отказ его запуска.
		c, err := Start(testConfig(), p, func(Snapshot) {
			// Не обрабатывает измерения при проверке владения нативным поставщиком.
		}, func() {
			// Не меняет внешние наблюдения при завершении тестового сборщика.
		})
		if err != nil {
			t.Fatal(err)
		}
		<-entered
		// err содержит результат повторного Start с тем же Provider, ожидается ErrBusy.
		if _, err := Start(testConfig(), p, func(Snapshot) {
			// Оставляет публикацию пустой для повторного Start, который должен быть отклонён как ErrBusy.
		}, func() {
			// Оставляет удаление пустым для повторного Start, который не должен получить владельца.
		}); !errors.Is(err, ErrBusy) {
			t.Fatal("повторный Collector получил тот же нативный Provider")
		}
		// err содержит результат Close при удерживаемом Poll, ожидается ErrPending.
		if err := c.Close(context.Background()); !errors.Is(err, ErrPending) {
			t.Fatal(err)
		}
		// replacement пытается получить процессное владение до завершения исходного Poll и
		// должен получить Busy.
		replacement := newNativeProvider(&controlledProvider{initReason: Available, stopReason: Available})
		if replacement.Init() != Busy || replacement.Shutdown() != Available {
			t.Fatal("незавершённый владелец допускает замену")
		}
		close(release)
		// err содержит результат повторного Close после освобождения Poll.
		if err := c.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if backend.starts.Load() != 1 || backend.stops.Load() != 1 || p.Init() != Busy || p.Poll(Limits{}).Reason != ProviderError || p.Shutdown() != Available {
			t.Fatal("нарушена однократность нативного жизненного цикла")
		}
		// next содержит новый Provider, который должен получить владение после подтверждённого
		// завершения прежнего.
		next := newNativeProvider(&controlledProvider{initReason: Available, stopReason: Available})
		if next.Init() != Available || next.Shutdown() != Available {
			t.Fatal("подтверждённое завершение не освобождает владельца")
		}
	})
}

// Проверяет однократный Init после ошибки и раздельное освобождение владельца при отсутствующей
// библиотеке и неподтверждённом Shutdown.
func TestNativeInitializationFailureDoesNotRepeatInitOrGrowOwners(t *testing.T) {
	// reason выбирает отсутствие библиотеки или драйвера для проверки различий освобождения после
	// Init.
	for _, reason := range []Reason{LibraryUnavailable, DriverUnavailable} {
		// backend моделирует выбранный отказ Init и неподтверждённый нативный Shutdown.
		backend := &controlledProvider{initReason: reason, stopReason: ProviderError}
		// p содержит нативного владельца, которому запрещён повторный Init после отказа.
		p := newNativeProvider(backend)
		if p.Init() != reason || p.Init() != Busy || backend.starts.Load() != 1 || p.claimCollector() {
			t.Fatal("ошибка Init допускает повторную загрузку привязки")
		}
		// got содержит сохранённый результат первой попытки Shutdown после ошибки Init.
		got := p.Shutdown()
		if reason == LibraryUnavailable {
			if got != Available || backend.stops.Load() != 0 {
				t.Fatal("отсутствующая библиотека вызвала нативный Shutdown")
			}
			// next проверяет новое процессное владение после отсутствующей библиотеки без
			// вызова нативного Shutdown.
			next := newNativeProvider(&controlledProvider{initReason: Available, stopReason: Available})
			if next.Init() != Available || next.Shutdown() != Available {
				t.Fatal("отсутствующая библиотека удержала владельца")
			}
		} else {
			if got != ProviderError || p.Shutdown() != ProviderError || backend.stops.Load() != 1 {
				t.Fatal("неопределённый Shutdown повторяет нативный вызов")
			}
			// next проверяет запрет замены после неопределённого результата нативного
			// Shutdown.
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
