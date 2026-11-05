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

type retirementFaultStore struct {
	*storage.Store
	unavailable atomic.Bool
	rejected    chan struct{}
}

func (s *retirementFaultStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	return s.Store.Write(ctx, func(r core.Repositories) error {
		r.Runtime = retirementFaultRepository{RuntimeRepository: r.Runtime, store: s}
		return callback(r)
	})
}

type retirementFaultRepository struct {
	core.RuntimeRepository
	store *retirementFaultStore
}

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

func waitRetirementCondition(t *testing.T, ctx context.Context, condition func() bool) {
	t.Helper()
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

func TestManagerRetainsRetirementAfterOwnerLossAndDatabaseRollback(t *testing.T) {
	f := newManagerFixture(t, false, nil)
	if err := f.manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	fault := &retirementFaultStore{Store: f.store, rejected: make(chan struct{}, 32)}
	service, err := core.NewService(fault, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	config := f.config
	config.Core = service
	manager, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		fault.unavailable.Store(false)
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	f.manager = manager
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start, err := manager.StartSession(ctx, core.StartSessionRequest{RequestID: "owner-loss", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	original := waitReceipt(t, f, start)
	if original.Status != core.CommandSucceeded {
		t.Fatal(original)
	}
	session, err := service.GetSession(ctx, start.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	waitRetirementCondition(t, ctx, func() bool { _, e := manager.Feed(session); return e == nil })
	manager.mu.Lock()
	node := manager.sessions[start.SessionID]
	manager.mu.Unlock()
	node.mu.Lock()
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
	joined, pending := node.joined, node.retirementNeeded
	node.mu.Unlock()
	manager.mu.Lock()
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
		current, e := service.GetSession(ctx, start.SessionID)
		return e == nil && current.State.Terminal()
	})
	preserved, err := service.GetReceipt(ctx, start.ID)
	if err != nil || !reflect.DeepEqual(original, preserved) {
		t.Fatal("отзыв исполнения изменил успешную квитанцию запуска", preserved, err)
	}
	cleanups, err := service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: start.SessionID}})
	if err != nil || len(cleanups.Items) != 1 || cleanups.Items[0].OutputID != "" || cleanups.Items[0].Lifecycle != session.Lifecycle || cleanups.Items[0].Incarnation != session.Incarnation {
		t.Fatal("отзыв исполнения не сохранил точное обязательство очистки", cleanups, err)
	}
	next, err := manager.StartSession(ctx, core.StartSessionRequest{RequestID: "after-owner-loss", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal("завершённое исполнение сохранило активный слот", err)
	}
	if current := waitReceipt(t, f, next); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	if err = node.retireExecution(ctx); err != nil {
		t.Fatal(err)
	}
	fresh, err := service.GetSession(ctx, next.SessionID)
	if err != nil || fresh.State != core.SessionRunning {
		t.Fatal("поздний отзыв исполнения изменил новый запуск", fresh, err)
	}
	waitRetirementCondition(t, ctx, func() bool { _, e := manager.Feed(fresh); return e == nil })
	_, applied, err := service.CompensateSessionExecution(ctx, node.fence, "late-owner-retirement")
	if err != nil || applied {
		t.Fatal("старое исполнение повторно отозвано", applied, err)
	}
}
