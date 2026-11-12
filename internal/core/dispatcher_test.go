package core

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// Создаёт тестовую RuntimeAuthority с конечными бюджетами наблюдений и двух физических исполнений.
func dispatcherTestAuthority(t *testing.T) *RuntimeAuthority {
	t.Helper()
	// a содержит общую тестовую RuntimeAuthority, а err — результат проверки её явных бюджетов.
	a, err := NewRuntimeAuthority("owner", acceptanceObservationLimits(), OutputExecutionLimits{MaxReservations: 2, MaxIdentifierBytes: 1024, MaxBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Проверяет единственность диспетчера между Service, недействительность копии резервации и сохранение
// нового владельца при позднем закрытии старого.
func TestDispatcherReservationIsSharedAndClosesOnlyItsOwnLifetime(t *testing.T) {
	// a предоставляет общую координацию единственного слота диспетчера.
	a := dispatcherTestAuthority(t)
	// uow связывает тестовый UnitOfWork с общей authority двух сервисов.
	uow := &runtimeAuthorityUOW{authority: a}
	// s предоставляет первый доменный сервис, а err — результат его создания.
	s, err := NewService(uow, "owner")
	if err != nil {
		t.Fatal(err)
	}
	// other предоставляет второй сервис с тем же общим слотом диспетчера.
	other, err := NewService(uow, "owner")
	if err != nil {
		t.Fatal(err)
	}
	// ctx задаёт контекст резервирования и освобождения диспетчера.
	ctx := context.Background()
	// d содержит первоначально занятый допуск диспетчера.
	d, err := s.ReserveDispatcher(ctx)
	if err != nil || !d.Valid() {
		t.Fatal("диспетчер не получил допуск", err)
	}
	// err сообщает об ошибке конкурирующего резервирования занятого слота.
	if _, err := other.ReserveDispatcher(ctx); !errors.Is(err, ErrInUse) {
		t.Fatal("второй Service получил параллельного диспетчера", err)
	}
	// copied имитирует копию непрозрачного резерва, которая не должна менять владельца.
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
	// fresh содержит новый допуск после закрытия первого диспетчера.
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
	// err сообщает об ошибке повторного закрытия уже отозванного диспетчера.
	if err := fresh.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке запроса резерва после закрытия authority.
	if _, err := s.ReserveDispatcher(ctx); !errors.Is(err, ErrClosed) {
		t.Fatal("закрытая authority выдала новый допуск", err)
	}
}

// Проверяет единственного победителя одновременного резервирования и сохранение занятого слота при
// отменённом закрытии.
func TestDispatcherConcurrentAdmissionHasOneWinnerAndCancelledReleaseRetainsSlot(t *testing.T) {
	// a предоставляет общий слот диспетчера для конкурентных запросов.
	a := dispatcherTestAuthority(t)
	// s предоставляет сервис конкурентного резервирования, а err — результат его создания.
	s, err := NewService(&runtimeAuthorityUOW{authority: a}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// n задаёт число одновременно конкурирующих запросов резерва.
	const n = 12
	// start одновременно выпускает все конкурирующие запросы резервирования.
	start := make(chan struct{})
	// results собирает выданные допуски для проверки единственного победителя.
	results := make(chan *RuntimeDispatcher, n)
	// failures собирает отказы остальных конкурирующих запросов.
	failures := make(chan error, n)
	// wg ожидает завершения всех конкурирующих запросов.
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			// После общего сигнала пытается занять слот RuntimeDispatcher и передаёт допуск либо ошибку в
			// отдельный ограниченный канал.

			<-start
			// d содержит выданный допуск конкурирующей попытки, а err — результат
			// резервирования.
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
	// err содержит очередной отказ конкурента, который должен быть ErrInUse.
	for err := range failures {
		if !errors.Is(err, ErrInUse) {
			t.Fatal(err)
		}
	}
	// d содержит единственный успешно выданный допуск диспетчера.
	d := <-results
	// err сообщает об ошибке захвата authority для проверки отменённого освобождения.
	if err := a.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	// ctx задаёт отменяемый контекст закрытия, а cancel отменяет его до освобождения резерва.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = d.CloseContext(ctx)
	a.release()
	if !errors.Is(err, context.Canceled) || !d.Valid() {
		t.Fatal("отменённое закрытие потеряло допуск", err)
	}
	// err сообщает об ошибке проверки, что отменённое закрытие сохранило занятый слот.
	if _, err := s.ReserveDispatcher(context.Background()); !errors.Is(err, ErrInUse) {
		t.Fatal("отменённое закрытие освободило слот", err)
	}
	d.Close()
}
