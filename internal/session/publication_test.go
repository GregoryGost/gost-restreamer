package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// publicationBoundary задаёт сигналы входа и однократного продолжения этапа записи запуска.
type publicationBoundary struct {
	// entered сообщает о достижении управляемого этапа записи.
	entered chan struct{}
	// release разрешает продолжить задержанный этап записи.
	release chan struct{}
	// once обеспечивает однократное закрытие канала разрешения.
	once sync.Once
}

// newPublicationBoundary создаёт отдельные сигналы входа и разрешения для управляемой задержки записи.
func newPublicationBoundary() publicationBoundary {
	return publicationBoundary{entered: make(chan struct{}), release: make(chan struct{})}
}

// unblock однократно разрешает продолжение задержанного этапа записи.
func (b *publicationBoundary) unblock() {
	b.once.Do(func() {
		// Закрывает канал разрешения задержанного этапа записи ровно один раз.
		close(b.release)
	})
}

// hold сообщает о достижении этапа записи и ждёт разрешения проверки продолжить его.
func (b *publicationBoundary) hold() { close(b.entered); <-b.release }

// publicationStore управляет задержками выдачи и подтверждения запуска вокруг настоящего COMMIT.
type publicationStore struct {
	// Store выполняет настоящую запись и освобождает её ресурсы до задержки ответа после COMMIT.
	*storage.Store
	// issued задерживает возврат выдачи попытки; beforeCommit задерживает завершение до COMMIT;
	// afterCommit задерживает ответ уже сохранённого успеха.
	issued, beforeCommit, afterCommit publicationBoundary
	// reject включает откат завершения до COMMIT; ambiguous включает ошибку ответа после успешного
	// COMMIT.
	reject, ambiguous bool
}

// Write задерживает выдачу попытки и завершение запуска по обе стороны COMMIT; может отклонить
// транзакцию либо вернуть ошибку после успешной записи.
func (s *publicationStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	// issued отмечает сохранение выдачи запуска; completed отмечает сохранение успешного завершения
	// запуска в текущей записи.
	var issued, completed bool
	// err сообщает результат настоящей записи с управляемыми задержкой и откатом завершения.
	err := s.Store.Write(ctx, func(r core.Repositories) error {
		// Отмечает изменения квитанции запуска и задерживает возврат из транзакции перед COMMIT, при
		// необходимости вызывая её откат.

		r.Runtime = publicationRepository{RuntimeRepository: r.Runtime, issued: &issued, completed: &completed}
		// err сообщает отказ исходной операции транзакции до задержки завершения.
		if err := callback(r); err != nil {
			return err
		}
		if completed {
			s.beforeCommit.hold()
			if s.reject {
				return core.ErrInvalid
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if issued {
		s.issued.hold()
	}
	if completed {
		// Store.Write уже завершил COMMIT и освободил RuntimeAuthority.
		s.afterCommit.hold()
		if s.ambiguous {
			return context.DeadlineExceeded
		}
	}
	return nil
}

// publicationRepository отмечает переходы квитанции запуска для задержек вокруг COMMIT.
type publicationRepository struct {
	// RuntimeRepository сохраняет настоящие изменения квитанций под проверочной обёрткой.
	core.RuntimeRepository
	// issued указывает флаг перехода запуска в ISSUED; completed указывает флаг перехода запуска в
	// SUCCEEDED.
	issued, completed *bool
}

// UpdateReceipt сохраняет квитанцию и отмечает переход запуска сессии в ISSUED или SUCCEEDED для
// управления задержками записи.
func (r publicationRepository) UpdateReceipt(ctx context.Context, receipt core.CommandReceipt, expected uint64) error {
	// err сообщает отказ настоящего обновления квитанции до установки признаков перехода.
	if err := r.RuntimeRepository.UpdateReceipt(ctx, receipt, expected); err != nil {
		return err
	}
	if receipt.Kind == core.CommandStartSession {
		*r.issued = receipt.Status == core.CommandIssued
		*r.completed = receipt.Status == core.CommandSucceeded
	}
	return nil
}

// publicationFeedResult передаёт медиамаршрутизатор и результат проверки допуска Feed между горутинами.
type publicationFeedResult struct {
	// router содержит допущенный медиамаршрутизатор либо nil при отказе Feed.
	router *media.Router
	// err сообщает причину отказа допуска Feed.
	err error
}

// awaitPublication ждёт контрольный сигнал этапа и завершает тест ошибкой при истечении контекста.
func awaitPublication(t *testing.T, ctx context.Context, event <-chan struct{}) {
	t.Helper()
	select {
	case <-event:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// awaitPublicationFeed ждёт результат параллельного Feed и завершает тест ошибкой при истечении
// контекста.
func awaitPublicationFeed(t *testing.T, ctx context.Context, result <-chan publicationFeedResult) publicationFeedResult {
	t.Helper()
	select {
	// value содержит результат параллельного запроса Feed после контрольного этапа записи.
	case value := <-result:
		return value
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return publicationFeedResult{}
	}
}

// TestManagerPublishesFeedWithDurableStartup проверяет допуск Feed после долговечного успеха запуска,
// включая потерянное подтверждение, отказ COMMIT и отзыв уже принятого исполнения.
func TestManagerPublishesFeedWithDurableStartup(t *testing.T) {
	// test задаёт исход записи запуска и возможный отзыв исполнения до возврата подтверждения.
	for _, test := range []struct {
		// name задаёт имя сценария публикации медиавхода относительно COMMIT.
		name string
		// reject выбирает откат завершения; ambiguous выбирает потерю подтверждения COMMIT; stop
		// выбирает отзыв сохранённого запуска.
		reject, ambiguous, stop bool
	}{
		{name: "committed"},
		{name: "ambiguous", ambiguous: true},
		{name: "rejected", reject: true},
		{name: "revoked", stop: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Проверяет выбранный исход публикации Feed относительно COMMIT и отзыва запуска.

			// f предоставляет сессию без назначений для проверки момента допуска Feed.
			f := newManagerFixture(t, false, nil)
			// err сообщает отказ закрытия исходного менеджера перед установкой задержек
			// записи.
			if err := f.manager.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			// wrapped задаёт задержки выдачи, COMMIT и ответа, а также выбранные отказ и
			// неоднозначность.
			wrapped := &publicationStore{Store: f.store, issued: newPublicationBoundary(), beforeCommit: newPublicationBoundary(), afterCommit: newPublicationBoundary(), reject: test.reject, ambiguous: test.ambiguous}
			// service предоставляет доменные операции через управляемые этапы записи; err
			// сообщает отказ его создания.
			service, err := core.NewService(wrapped, f.store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			// config содержит настройки менеджера с подменённым доменным сервисом.
			config := f.config
			config.Core = service
			// manager содержит диспетчер, чей допуск Feed сравнивается с долговечным
			// состоянием.
			manager, err := New(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает Manager проверки границ COMMIT до освобождения его общих зависимостей.

				// err сообщает отказ закрытия проверяемого менеджера при очистке
				// сценария.
				if err := manager.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			t.Cleanup(func() {
				// Снимает все задержки записи при любом завершении проверки, чтобы работники смогли вернуться.
				wrapped.issued.unblock()
				wrapped.beforeCommit.unblock()
				wrapped.afterCommit.unblock()
			})
			// ctx ограничивает сценарий публикации восемью секундами; cancel освобождает
			// ресурсы срока.
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			// receipt содержит принятую команду запуска для проверки границ публикации Feed.
			receipt, err := manager.StartSession(ctx, core.StartSessionRequest{RequestID: "publish-feed", StreamID: f.stream.ID})
			if err != nil {
				t.Fatal(err)
			}
			awaitPublication(t, ctx, wrapped.issued.entered)
			manager.mu.Lock()
			// node указывает локального владельца запуска и позволяет выбрать расписание
			// проверки Feed после COMMIT.
			node := manager.sessions[receipt.SessionID]
			manager.mu.Unlock()
			wrapped.issued.unblock()
			awaitPublication(t, ctx, wrapped.beforeCommit.entered)
			// starting содержит сохранённую сессию до COMMIT успешного завершения.
			starting, err := service.GetSession(ctx, receipt.SessionID)
			if err != nil || starting.State != core.SessionStarting {
				t.Fatal("до COMMIT нет состояния STARTING", starting, err)
			}
			// pending содержит квитанцию, которая до COMMIT должна оставаться в ISSUED.
			pending, err := service.GetReceipt(ctx, receipt.ID)
			if err != nil || pending.Status != core.CommandIssued {
				t.Fatal("до COMMIT изменена квитанция", pending, err)
			}
			// early передаёт результат параллельного Feed со снимком STARTING.
			early := make(chan publicationFeedResult, 1)
			go func() {
				// Запрашивает Feed со снимком STARTING параллельно с завершением запуска и передаёт результат тесту.
				// router содержит ответ Feed со снимком STARTING; e сообщает отказ
				// допуска этого снимка.
				router, e := manager.Feed(starting)
				early <- publicationFeedResult{router, e}
			}()
			wrapped.beforeCommit.unblock()
			if test.reject {
				// actual содержит результат раннего Feed при отклонённом завершении
				// запуска.
				actual := awaitPublicationFeed(t, ctx, early)
				if actual.router != nil || !errors.Is(actual.err, core.ErrStale) {
					t.Fatal("отклонённый startup допустил Feed", actual)
				}
				f.manager, f.service = manager, service
				// current содержит терминальный результат запуска после отката успешного
				// завершения.
				if current := waitReceipt(t, f, receipt); current.Status != core.CommandFailed {
					t.Fatal(current)
				}
				return
			}
			awaitPublication(t, ctx, wrapped.afterCommit.entered)
			// running содержит сохранённую RUNNING-сессию после успешного COMMIT.
			running, err := service.GetSession(ctx, receipt.SessionID)
			if err != nil || running.State != core.SessionRunning {
				t.Fatal("COMMIT не опубликовал RUNNING", running, err)
			}
			// succeeded содержит квитанцию, успешно завершённую в том же COMMIT, что и
			// запуск сессии.
			succeeded, err := service.GetReceipt(ctx, receipt.ID)
			if err != nil || succeeded.Status != core.CommandSucceeded {
				t.Fatal("COMMIT не завершил квитанцию", succeeded, err)
			}
			if test.stop {
				if _, err = service.StopSession(ctx, core.SessionCommand{RequestID: "revoke-published", SessionID: receipt.SessionID}); err != nil {
					t.Fatal(err)
				}
			}
			// result передаёт результат Feed со снимком RUNNING до возврата подтверждения
			// записи.
			result := make(chan publicationFeedResult, 1)
			// TryLock выбирает расписание, но не определяет ожидаемый результат Feed.
			// Свободный мьютекс позволяет проверить Feed до возврата Complete; занятый требует параллельного вызова.
			if node.mu.TryLock() {
				node.mu.Unlock()
				// router содержит результат Feed при свободной блокировке узла; e
				// сообщает отказ допуска снимка RUNNING.
				router, e := manager.Feed(running)
				result <- publicationFeedResult{router, e}
				wrapped.afterCommit.unblock()
			} else {
				go func() {
					// Запрашивает Feed со снимком RUNNING, пока возврат из записи ещё задержан, и передаёт результат после
					// снятия блокировки.
					// router содержит результат параллельного Feed при занятой
					// блокировке узла; e сообщает отказ допуска снимка RUNNING.
					router, e := manager.Feed(running)
					result <- publicationFeedResult{router, e}
				}()
				wrapped.afterCommit.unblock()
			}
			// actual содержит результат Feed сохранённого запуска после возможного отзыва
			// исполнения.
			actual := awaitPublicationFeed(t, ctx, result)
			if test.stop {
				if actual.router != nil || !errors.Is(actual.err, core.ErrStale) {
					t.Fatal("отозванный startup допустил Feed", actual)
				}
			} else if actual.err != nil || actual.router == nil {
				t.Fatal("успешный COMMIT ещё не допускает Feed", actual.router, actual.err)
			}
			// actual содержит результат прежнего Feed со снимком STARTING после завершения
			// записи.
			if actual := awaitPublicationFeed(t, ctx, early); actual.router != nil || !errors.Is(actual.err, core.ErrStale) {
				t.Fatal("STARTING допустил Feed", actual)
			}
		})
	}
}
