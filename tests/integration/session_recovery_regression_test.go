package integration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/session"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

type recoveryDelayedIngress struct {
	session.IngressOwner
	entered, release chan struct{}
	once             sync.Once
}

func (o *recoveryDelayedIngress) CloseAdmission() {
	if owner, ok := o.IngressOwner.(interface{ CloseAdmission() }); ok {
		owner.CloseAdmission()
	}
}

func (o *recoveryDelayedIngress) Shutdown(ctx context.Context) error {
	err := o.IngressOwner.Shutdown(ctx)
	o.once.Do(func() { close(o.entered) })
	select {
	case <-o.release:
		return err
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
}

func recoveryWaitEvent(t *testing.T, event <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-event:
	case <-time.After(8 * time.Second):
		t.Fatal(description)
	}
}

func TestSessionManagerOldCloseCannotRevokeSuccessor(t *testing.T) {
	helper := sessionAcceptanceBuildHelper(t)
	receiver, _ := newPublisherReceiver(t, false)
	f := newSessionAcceptanceFixture(t, helper, []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	delayed := &recoveryDelayedIngress{IngressOwner: f.config.Ingress, entered: make(chan struct{}), release: make(chan struct{})}
	f.config.Ingress = delayed
	f.launch(t)
	old := f.manager
	var release sync.Once
	t.Cleanup(func() {
		release.Do(func() { close(delayed.release) })
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := old.Close(ctx); err != nil {
			t.Error("окончательное присоединение старого Manager:", err)
		}
	})
	previous := f.running(t, f.start(t, "start-before-manager-replacement"))
	receiver.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= 8 })
	closed := make(chan error, 1)
	go func() { closed <- old.Close(context.Background()) }()
	recoveryWaitEvent(t, delayed.entered, "старый ingress не начал физическое завершение")
	if _, err := old.Feed(previous); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("старый Manager сохранил допуск Feed: %v", err)
	}
	// Прежний ingress уже закрыл допуск и соединения; задержан только ответ физического завершения.
	f.configureOwners(t, helper)
	sessionAcceptanceWait(t, func() bool {
		manager, err := session.New(context.Background(), f.config)
		if errors.Is(err, core.ErrInUse) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		f.manager = manager
		return true
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := f.manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	select {
	case err := <-closed:
		t.Fatalf("старый Close завершился до разрешения физического ожидания: %v", err)
	default:
	}
	current := f.running(t, f.start(t, "start-under-successor-manager"))
	feed, err := f.manager.Feed(current)
	if err != nil {
		t.Fatal(err)
	}
	sessionAcceptanceAssertHolds(t, f, current.ID, true)
	before := publisherMediaCount(receiver.snapshot())
	receiver.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= before+8 })
	connections := receiver.accepted.Load()
	release.Do(func() { close(delayed.release) })
	select {
	case err = <-closed:
		if err != nil {
			t.Fatal("первое закрытие старого Manager:", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("старый Close не завершился после разрешения ожидания")
	}
	for range 2 {
		if err = old.Close(context.Background()); err != nil {
			t.Fatal("повторное закрытие старого Manager:", err)
		}
		after, err := f.service.GetSession(context.Background(), current.ID)
		if err != nil || after.State != core.SessionRunning || !after.Issued || after.Lifecycle != current.Lifecycle {
			t.Fatalf("старый Close отозвал новое исполнение: %+v %v", after, err)
		}
		if actual, err := f.manager.Feed(after); err != nil || actual != feed {
			t.Fatalf("старый Close заменил Feed: %v", err)
		}
		sessionAcceptanceAssertHolds(t, f, current.ID, true)
	}
	before = publisherMediaCount(receiver.snapshot())
	receiver.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= before+8 })
	if receiver.accepted.Load() != connections {
		t.Fatal("старый Close пересоздал соединение нового исполнения")
	}
}

// Ошибка следующего перехода возникает после настоящей фиксации выбранной стадии в БД.
// Откат транзакции подтверждается до восстановления записи вызывающим тестом.
type recoveryReceiptFaultStore struct {
	*storage.Store
	requestID       string
	stage           core.CommandStatus
	armed           atomic.Bool
	failed          chan struct{}
	restore         chan struct{}
	failedOnce      sync.Once
	transitionFault atomic.Bool
}

func (s *recoveryReceiptFaultStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	stageCommitted := false
	faulted := false
	err := s.Store.Write(ctx, func(r core.Repositories) error {
		r.Runtime = recoveryReceiptFaultRepository{RuntimeRepository: r.Runtime, store: s, stageCommitted: &stageCommitted, faulted: &faulted}
		return callback(r)
	})
	if err == nil && stageCommitted {
		s.armed.Store(true)
	}
	if faulted && err != nil {
		s.failedOnce.Do(func() { close(s.failed) })
	}
	return err
}

type recoveryReceiptFaultRepository struct {
	core.RuntimeRepository
	store          *recoveryReceiptFaultStore
	stageCommitted *bool
	faulted        *bool
}

func (r recoveryReceiptFaultRepository) UpdateReceipt(ctx context.Context, value core.CommandReceipt, revision uint64) error {
	s := r.store
	if value.RequestID == s.requestID {
		if value.Status == s.stage {
			*r.stageCommitted = true
		} else if s.armed.Load() {
			select {
			case <-s.restore:
			default:
				*r.faulted = true
				s.transitionFault.Store(true)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-s.restore:
				}
			}
		}
	}
	return r.RuntimeRepository.UpdateReceipt(ctx, value, revision)
}

func TestSessionManagerStopRecoversAfterPersistedReceiptWriteFailure(t *testing.T) {
	helper := sessionAcceptanceBuildHelper(t)
	for _, stage := range []core.CommandStatus{core.CommandClaimed, core.CommandIssued} {
		t.Run(string(stage), func(t *testing.T) {
			receiver, _ := newPublisherReceiver(t, false)
			f := newSessionAcceptanceFixture(t, helper, []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
			fault := &recoveryReceiptFaultStore{Store: f.store, requestID: "stop-with-temporary-write-failure", stage: stage, failed: make(chan struct{}), restore: make(chan struct{})}
			service, err := core.NewService(fault, f.store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			f.service = service
			f.configureOwners(t, helper)
			f.launch(t)
			var restore sync.Once
			t.Cleanup(func() { restore.Do(func() { close(fault.restore) }) })
			current := f.running(t, f.start(t, "start-before-recoverable-stop"))
			receiver.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= 8 })
			stop, err := f.manager.StopSession(context.Background(), core.SessionCommand{RequestID: fault.requestID, SessionID: current.ID})
			if err != nil {
				t.Fatal(err)
			}
			recoveryWaitEvent(t, fault.failed, "ошибка записи после сохранённой стадии не воспроизвелась")
			persisted, err := f.service.GetReceipt(context.Background(), stop.ID)
			if err != nil || persisted.Status != stage || !fault.transitionFault.Load() {
				t.Fatalf("не подтверждена граница rollback: %+v %v", persisted, err)
			}
			stopping, err := f.service.GetSession(context.Background(), current.ID)
			if err != nil || stopping.State != core.SessionStopping {
				t.Fatalf("неудачная запись сохранила терминальный переход: %+v %v", stopping, err)
			}
			restore.Do(func() { close(fault.restore) })
			completed := sessionAcceptanceReceipt(t, f.service, stop.ID, core.CommandSucceeded)
			if completed.Attempt != persisted.Attempt {
				t.Fatalf("восстановление создало новую попытку команды: %d -> %d", persisted.Attempt, completed.Attempt)
			}
			previous, err := f.service.GetSession(context.Background(), current.ID)
			if err != nil || previous.State != core.SessionStopped || previous.EndedAt == nil {
				t.Fatalf("восстановленная остановка не завершила Session: %+v %v", previous, err)
			}
			fresh := f.running(t, f.start(t, "manual-start-after-recovered-stop"))
			if fresh.ID == current.ID || fresh.Incarnation != current.Incarnation {
				t.Fatal("новый ручной запуск не получил независимую Session той же incarnation")
			}
			sessionAcceptanceAssertHolds(t, f, fresh.ID, true)
			before := publisherMediaCount(receiver.snapshot())
			receiver.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= before+8 })
		})
	}
}
