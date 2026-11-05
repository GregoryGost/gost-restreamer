package processowner

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func TestResolveIncludesOperationsRegisteredAfterInitialSnapshot(t *testing.T) {
	r := testRegistry(t, buildHelper(t))
	ctx := testContext(t)
	s, err := r.Create(ctx, "catalog-snapshot-owner")
	if s != nil {
		t.Cleanup(func() {
			if err := s.CloseContext(testContext(t)); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	initial, err := s.header()
	if err != nil || initial.Count != 1 {
		t.Fatalf("начальный заголовок=%+v %v", initial, err)
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = readyRead.Close(); _ = readyWrite.Close() }()
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = controlRead.Close(); _ = controlWrite.Close() }()
	p, err := s.Start(ctx, Command{Binary: os.Args[0], Args: []string{"-test.run=^TestControlledNativeChild$", "--", "--controlled-native-child"}, ExtraFiles: []*os.File{readyWrite, controlRead}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.Cancel()
		if _, err := p.Wait(testContext(t)); err != nil {
			t.Error(err)
		}
	})
	_ = readyWrite.Close()
	_ = controlRead.Close()
	var ready [1]byte
	if _, err = io.ReadFull(readyRead, ready[:]); err != nil {
		t.Fatal(err)
	}
	current, err := s.header()
	if err != nil || current.Count != 3 {
		t.Fatalf("текущий заголовок=%+v %v", current, err)
	}
	// Моделируется освобождение блокировки backend.lock при аварии после регистрации и запуска дочернего процесса.
	s.mu.Lock()
	err = unlock(s.backend)
	s.backend = nil
	s.closed = true
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// Продолжается проверка, уже прочитавшая заголовок до появления двух новых операций.
	proof, err := s.resolve(ctx, initial)
	if err != nil || proof.Joined || proof.Unknown || !proof.Sealed || proof.Operations != 3 || proof.PendingOperations != 1 {
		t.Fatalf("живой helper пропущен в доказательстве=%+v %v", proof, err)
	}
	if _, err = os.Stat(s.Namespace()); err != nil {
		t.Fatalf("пространство имён удалено до завершения дочернего процесса: %v", err)
	}
	if err = r.AcknowledgeScope(ctx, s.ID()); !errors.Is(err, ErrPending) {
		t.Fatalf("преждевременное подтверждение очистки=%v", err)
	}
	p.Cancel()
	result, err := p.Wait(ctx)
	if err != nil || !result.Joined {
		t.Fatalf("завершение дочернего процесса=%+v %v", result, err)
	}
	proof, err = s.Resolve(ctx)
	if err != nil || !proof.Joined || proof.Operations != 3 {
		t.Fatalf("окончательное доказательство=%+v %v", proof, err)
	}
}

func TestCreateCancellationDoesNotWaitForCleanupGate(t *testing.T) {
	r := testRegistry(t, buildHelper(t))
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	var gate *os.File
	var guard *time.Timer
	released := make(chan error, 1)
	s, err := r.create(ctx, "cancelled-runtime-owner", func(s *Scope, ctx context.Context) (*Ticket, error) {
		ticket, err := s.initializeRuntime(ctx)
		if err != nil {
			return ticket, err
		}
		gate, err = lockExisting(ctx, r.root, scopePath(s.id)+"/gate.lock", true)
		if err != nil {
			return ticket, err
		}
		guard = time.AfterFunc(5*time.Second, func() { released <- unlock(gate) })
		cancel()
		return ticket, nil
	})
	if guard != nil {
		if guard.Stop() {
			if err := unlock(gate); err != nil {
				t.Error(err)
			}
		} else {
			t.Error("отменённое создание ожидало освобождения блокировки очистки")
			if err := <-released; err != nil {
				t.Error(err)
			}
		}
	}
	if s != nil {
		t.Cleanup(func() {
			if err := s.CloseContext(testContext(t)); err != nil {
				t.Error(err)
			}
		})
	}
	if guard == nil || s == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("отменённое создание потеряло частичную область: scope=%v err=%v", s, err)
	}
	if err := s.CloseContext(testContext(t)); err != nil {
		t.Fatal(err)
	}
	proof, err := s.Resolve(testContext(t))
	if err != nil || !proof.Joined {
		t.Fatalf("восстановление отменённого создания=%+v %v", proof, err)
	}
}

func TestCreatePreservesScopeAfterCompletionAndCloseFailure(t *testing.T) {
	r := testRegistry(t, buildHelper(t))
	ctx := testContext(t)
	var restore func() error
	var initialTicket *Ticket
	s, err := r.create(ctx, "partial-runtime-owner", func(s *Scope, ctx context.Context) (*Ticket, error) {
		ticket, err := s.initializeRuntime(ctx)
		if err != nil {
			return ticket, err
		}
		initialTicket = ticket
		operations := scopePath(s.id) + "/operations"
		saved := scopePath(s.id) + "/saved-operations"
		if err = r.root.Rename(operations, saved); err != nil {
			return ticket, err
		}
		restore = func() error { return r.root.Rename(saved, operations) }
		return ticket, nil
	})
	if restore == nil {
		t.Fatalf("отказ файловой системы не смоделирован: %v", err)
	}
	t.Cleanup(func() {
		if restore != nil {
			if err := restore(); err != nil {
				t.Error(err)
			}
		}
		if s != nil {
			if err := s.CloseContext(testContext(t)); err != nil {
				t.Error(err)
			}
		}
	})
	if err == nil || s == nil || !errors.Is(err, ErrUnknown) {
		t.Fatalf("частичная область потеряна: scope=%v err=%v", s, err)
	}
	if initialTicket == nil || !initialTicket.completionRequested || initialTicket.completed {
		t.Fatal("начальное завершение не сохранено для повтора")
	}
	backend := s.backend
	if backend == nil || !s.closing || s.closed {
		t.Fatal("неудачное закрытие освободило владение backend")
	}
	if err = s.CloseContext(ctx); !errors.Is(err, ErrUnknown) {
		t.Fatalf("отказ файловой системы не отклонил повторное закрытие: %v", err)
	}
	if err = restore(); err != nil {
		t.Fatal(err)
	}
	restore = nil
	lock, lockErr := lockExisting(ctx, r.root, scopePath(s.id)+"/backend.lock", false)
	if lock != nil {
		_ = unlock(lock)
	}
	if !errors.Is(lockErr, ErrPending) || s.backend != backend {
		t.Fatalf("блокировка частичной области потеряна: %v", lockErr)
	}
	if err = s.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	if !initialTicket.completed || s.backend != nil {
		t.Fatal("восстановленная файловая система не позволила завершить операцию и освободить блокировку")
	}
	page, _, err := r.RecoverPage(ctx, "", r.PageLimit())
	if err != nil || len(page) != 1 || page[0].ID != s.ID() || !page[0].Joined || page[0].Unknown {
		t.Fatalf("восстановление тем же Registry=%+v %v", page, err)
	}
	if _, err = os.Stat(s.Namespace()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("пространство имён осталось после восстановления: %v", err)
	}
}
