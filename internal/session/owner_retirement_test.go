package session

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// retirementFaultStore отклоняет долговечную остановку, позволяя проверить сохранение обязанности
// завершить утраченное исполнение.
type retirementFaultStore struct {
	// Store выполняет настоящие транзакции, включая откат инъецированного отказа остановки.
	*storage.Store
	// unavailable включает отказ вставки команды остановки сессии.
	unavailable atomic.Bool
	// rejected сообщает тесту о попытках записи отклоняемой остановки через ограниченный канал.
	rejected chan struct{}
}

// Write подставляет репозиторий, способный отклонить запись команды остановки внутри настоящей
// транзакции.
func (s *retirementFaultStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	return s.Store.Write(ctx, func(r core.Repositories) error {
		// Подставляет репозиторий с управляемым отказом остановки и выполняет исходную операцию записи.

		r.Runtime = retirementFaultRepository{RuntimeRepository: r.Runtime, store: s}
		return callback(r)
	})
}

// retirementFaultRepository отклоняет вставку остановки при включённом отказе записи.
type retirementFaultRepository struct {
	// RuntimeRepository выполняет настоящие операции выполнения за исключением управляемого отказа
	// вставки остановки.
	core.RuntimeRepository
	// store предоставляет признак отказа и канал наблюдения попыток остановки.
	store *retirementFaultStore
}

// InsertReceipt при включённом отказе возвращает ErrBusy для команды остановки сессии и сообщает тесту
// о попытке записи.
func (r retirementFaultRepository) InsertReceipt(ctx context.Context, receipt core.CommandReceipt) error {
	if receipt.Kind == core.CommandStopSession && r.store.unavailable.Load() {
		select {
		case r.store.rejected <- struct{}{}:
		default:
		}
		// Ошибка внутри настоящей транзакции откатывает также жизненный цикл и обязательство очистки.
		return core.ErrBusy
	}
	return r.RuntimeRepository.InsertReceipt(ctx, receipt)
}

// waitRetirementCondition опрашивает заданное условие до его выполнения или завершает тест ошибкой по
// контексту.
func waitRetirementCondition(t *testing.T, ctx context.Context, condition func() bool) {
	t.Helper()
	// ticker задаёт миллисекундный период повторной проверки условия завершения.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("не завершилось изменение состояния исполнения", ctx.Err())
		}
	}
}

// TestManagerRetainsRetirementAfterOwnerLossAndDatabaseRollback проверяет сохранение и повтор
// долговечного завершения после потери владельца при откатах БД, неизменность квитанции запуска и
// изоляцию следующей сессии.
func TestManagerRetainsRetirementAfterOwnerLossAndDatabaseRollback(t *testing.T) {
	// f предоставляет сессию без назначений для проверки отзыва потерянного владельца.
	f := newManagerFixture(t, false, nil)
	// err сообщает отказ закрытия исходного менеджера перед установкой отказа записи.
	if err := f.manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// fault управляет доступностью записи остановки и сообщает о повторных попытках.
	fault := &retirementFaultStore{Store: f.store, rejected: make(chan struct{}, 32)}
	// service предоставляет доменные операции через обёртку отказа; err сообщает отказ создания
	// сервиса.
	service, err := core.NewService(fault, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// config сохраняет настройки фикстуры с сервисом управляемого отказа записи.
	config := f.config
	config.Core = service
	// manager содержит диспетчер, у которого проверяется сохранение обязанности долговечного
	// завершения.
	manager, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Снимает искусственную недоступность записи и закрывает проверяемый Manager до освобождения
		// хранилища.

		fault.unavailable.Store(false)
		// err сообщает отказ закрытия менеджера после снятия искусственного отказа записи.
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	f.manager = manager
	// ctx ограничивает сценарий потери владельца восемью секундами; cancel освобождает этот срок.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// start содержит принятую команду запуска перед отзывом владельца.
	start, err := manager.StartSession(ctx, core.StartSessionRequest{RequestID: "owner-loss", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// original содержит исходную успешную квитанцию для проверки её неизменности после отзыва.
	original := waitReceipt(t, f, start)
	if original.Status != core.CommandSucceeded {
		t.Fatal(original)
	}
	// session содержит сохранённое работающее исполнение до потери владельца.
	session, err := service.GetSession(ctx, start.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	waitRetirementCondition(t, ctx, func() bool {
		// Проверяет доступность Feed принятой сессии перед отзывом её владельца.
		// e сообщает результат проверки доступности Feed до отзыва владельца.
		_, e := manager.Feed(session)
		return e == nil
	})
	manager.mu.Lock()
	// node указывает локальные ресурсы исполнения, владелец которого будет отозван.
	node := manager.sessions[start.SessionID]
	manager.mu.Unlock()
	node.mu.Lock()
	// owner содержит точного владельца наблюдений и ресурсов проверяемой сессии.
	owner := node.owner
	node.mu.Unlock()
	fault.unavailable.Store(true)
	if err = owner.Revoke(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-node.done:
	case <-ctx.Done():
		t.Fatal("обработчик отозванного владельца не завершился", ctx.Err())
	}
	// Даже Session без выходов уже физически завершена, но её долговечный отзыв ещё нужен.
	node.mu.Lock()
	// joined отмечает завершение локальной очистки; pending отмечает ещё необходимое долговечное
	// завершение исполнения.
	joined, pending := node.joined, node.retirementNeeded
	node.mu.Unlock()
	manager.mu.Lock()
	// retained отмечает сохранение прежнего узла менеджером до долговечного завершения.
	retained := manager.sessions[start.SessionID] == node
	manager.mu.Unlock()
	if !joined || !pending || !retained {
		t.Fatal("утрачено обязательство долговечного завершения после физической очистки", joined, pending, retained)
	}
	for range 3 {
		select {
		case <-fault.rejected:
		case <-ctx.Done():
			t.Fatal("долговечное завершение не повторяется после отката БД", ctx.Err())
		}
	}
	// before содержит состояние сессии после откатов остановки для проверки сохранения исходного
	// жизненного цикла.
	before, err := service.GetSession(ctx, start.SessionID)
	if err != nil || before.State != core.SessionRunning || before.Lifecycle != session.Lifecycle || before.CleanupPending {
		t.Fatal("откат не сохранил исходное долговечное состояние", before, err)
	}
	if _, err = manager.Feed(session); err == nil {
		t.Fatal("отозванный владелец сохранил Feed")
	}
	fault.unavailable.Store(false)
	manager.signal()
	waitRetirementCondition(t, ctx, func() bool {
		// Проверяет терминальное сохранённое состояние после восстановления возможности записать остановку.

		// current содержит повторно прочитанную сессию после восстановления записи; e сообщает
		// отказ её чтения.
		current, e := service.GetSession(ctx, start.SessionID)
		return e == nil && current.State.Terminal()
	})
	// preserved содержит исходную квитанцию запуска после долговечного отзыва исполнения.
	preserved, err := service.GetReceipt(ctx, start.ID)
	if err != nil || !reflect.DeepEqual(original, preserved) {
		t.Fatal("отзыв исполнения изменил успешную квитанцию запуска", preserved, err)
	}
	// cleanups содержит сохранённые обязательства очистки для проверки точной сессии, жизненного
	// цикла и инкарнации.
	cleanups, err := service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: start.SessionID}})
	if err != nil || len(cleanups.Items) != 1 || cleanups.Items[0].OutputID != "" || cleanups.Items[0].Lifecycle != session.Lifecycle || cleanups.Items[0].Incarnation != session.Incarnation {
		t.Fatal("отзыв исполнения не сохранил точное обязательство очистки", cleanups, err)
	}
	// next содержит новый запуск после освобождения активного слота прежнего исполнения.
	next, err := manager.StartSession(ctx, core.StartSessionRequest{RequestID: "after-owner-loss", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal("завершённое исполнение сохранило активный слот", err)
	}
	// current содержит терминальный результат нового запуска.
	if current := waitReceipt(t, f, next); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	if err = node.retireExecution(ctx); err != nil {
		t.Fatal(err)
	}
	// fresh содержит новую работающую сессию после повторного позднего отзыва прежнего исполнения.
	fresh, err := service.GetSession(ctx, next.SessionID)
	if err != nil || fresh.State != core.SessionRunning {
		t.Fatal("поздний отзыв исполнения изменил новый запуск", fresh, err)
	}
	waitRetirementCondition(t, ctx, func() bool {
		// Проверяет доступность Feed новой сессии после позднего завершения прежнего исполнения.
		// e сообщает результат проверки Feed нового исполнения после позднего отзыва прежнего.
		_, e := manager.Feed(fresh)
		return e == nil
	})
	// applied отмечает применение повторной компенсации прежнего исполнения, которая должна быть
	// отклонена как уже выполненная.
	_, applied, err := service.CompensateSessionExecution(ctx, node.fence, "late-owner-retirement")
	if err != nil || applied {
		t.Fatal("старое исполнение повторно отозвано", applied, err)
	}
}
