package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Проверяет независимую инициацию остановки GPU при зависшем менеджере и сохранение Store до позднего
// завершения менеджера.
func TestShutdownInitiatesIndependentOwnersAndRetainsShared(t *testing.T) {
	// a моделирует приложение с отдельными владельцами менеджера, GPU и общего хранилища.
	a := &Application{gate: make(chan struct{}, 1), done: make(chan struct{})}
	a.gate <- struct{}{}
	// blocked удерживает остановку менеджера до проверки сохранности общих ресурсов.
	blocked := make(chan struct{})
	// started подтверждает начало остановки заблокированного менеджера.
	started := make(chan struct{})
	a.owners.manager = &stopOwner{name: "manager", close: func(context.Context) stopResult {
		// Удерживает остановку менеджера до разрешения теста, сообщая о её начале.
		close(started)
		<-blocked
		return stopResult{nil, true}
	}}
	// gpuStopped подтверждает независимую остановку GPU при ещё заблокированном менеджере.
	gpuStopped := make(chan struct{})
	a.owners.gpu = &stopOwner{name: "gpu", close: func(context.Context) stopResult {
		// Подтверждает независимый запуск остановки GPU при заблокированном менеджере.
		close(gpuStopped)
		return stopResult{nil, true}
	}}
	// sharedClosed сообщает о закрытии общего Store для проверки порядка после завершения менеджера.
	sharedClosed := make(chan struct{})
	a.owners.store = sharedOwner("store", func(context.Context) error {
		// Сообщает о закрытии общего хранилища для проверки порядка освобождения.
		close(sharedClosed)
		return nil
	})
	// ctx ограничивает первую попытку остановки 50 миллисекундами; cancel освобождает её таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// err содержит результат первой остановки с ожидаемым ErrPending.
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
	// ctx2 задаёт отдельную секунду для позднего завершения; stop освобождает таймер повторного
	// ожидания.
	ctx2, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	// err содержит результат повторной остановки после снятия блокировки менеджера.
	if err := a.Shutdown(ctx2); err != nil {
		t.Fatal(err)
	}
	if !isClosed(a.Done()) || !isClosed(sharedClosed) {
		t.Fatal("поздний join не завершил приложение")
	}
}

// Проверяет повторное закрытие после ошибки завершённой работы с сохранением первой ошибки и
// подтверждением Done.
func TestShutdownPreservesHistoricalErrorAfterRepeatedClose(t *testing.T) {
	// a моделирует приложение с исторической ошибкой завершённого задания подготовки.
	a := &Application{gate: make(chan struct{}, 1), done: make(chan struct{})}
	a.gate <- struct{}{}
	// failure задаёт узнаваемую первую ошибку закрытия, которая должна сохраниться после успешного
	// повтора.
	failure := errors.New("ошибка завершённого задания")
	// attempts считает вызовы закрытия владельца для проверки одного допустимого повтора.
	attempts := 0
	a.owners.preparation = newStopOwner("preparation", func(context.Context) error {
		// Возвращает ошибку первой остановки и успех второй, проверяя сохранение исходной ошибки.

		attempts++
		if attempts == 1 {
			return failure
		}
		return nil
	})
	// ctx ограничивает сценарий повторной остановки секундой; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// err содержит итог остановки, который сохраняет failure без ErrPending после подтверждённого
	// завершения.
	if err := a.Shutdown(ctx); !errors.Is(err, failure) || errors.Is(err, ErrPending) {
		t.Fatalf("историческая ошибка: %v", err)
	}
	if attempts != 2 || !isClosed(a.Done()) {
		t.Fatal("повтор Close не подтвердил join")
	}
}
