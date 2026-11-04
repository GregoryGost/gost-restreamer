package storage

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/mattn/go-sqlite3"
)

func TestRepositoryScopeRejectsReadOnlyAndEscapedWrites(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	var escaped core.ConfigRepository
	err := f.store.View(ctx, func(r core.Repositories) error {
		escaped = r.Config
		return r.Config.DeleteProfile(ctx, f.profile.ID, f.profile.Revision)
	})
	requireDomainError(t, err, core.ErrInvalid)
	_, err = escaped.Profile(ctx, f.profile.ID)
	requireDomainError(t, err, core.ErrClosed)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	err = f.store.View(ctx, func(core.Repositories) error { return nil })
	requireDomainError(t, err, core.ErrClosed)
	err = f.store.Write(ctx, func(core.Repositories) error { return nil })
	requireDomainError(t, err, core.ErrClosed)
}

func TestReadSnapshotsStayIndependentAcrossConcurrentWrite(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	read := make(chan error, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.store.View(ctx, func(r core.Repositories) error {
			before, err := r.Config.Profile(ctx, f.profile.ID)
			read <- err
			if err != nil {
				return err
			}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			after, err := r.Config.Profile(ctx, f.profile.ID)
			if err != nil {
				return err
			}
			if before.Name != after.Name || before.Revision != after.Revision {
				return fmt.Errorf("читающий снимок изменился с %q/%d на %q/%d", before.Name, before.Revision, after.Name, after.Revision)
			}
			return nil
		})
	}()
	var observed error
	select {
	case observed = <-read:
	case <-ctx.Done():
		observed = ctx.Err()
	}
	if observed == nil {
		p := f.profile
		p.Name = "Изменённый профиль"
		_, observed = f.service.UpdateProfile(ctx, p, p.Revision)
		if observed == nil {
			observed = f.store.View(ctx, func(r core.Repositories) error {
				got, err := r.Config.Profile(ctx, p.ID)
				if err != nil {
					return err
				}
				if got.Name != p.Name || got.Revision != p.Revision+1 {
					return fmt.Errorf("новый читатель не видит committed изменение: %+v", got)
				}
				return nil
			})
		}
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("изолированное чтение: %v", err)
		}
	case <-ctx.Done():
		t.Errorf("чтение не завершено: %v", ctx.Err())
	}
	if observed != nil {
		t.Fatal(observed)
	}
	if inUse := f.store.sqlDB.Stats().InUse; inUse != 0 {
		t.Fatalf("после параллельных транзакций занято соединений: %d", inUse)
	}
}

func TestWriterContentionHonorsRetryAndCallerBudgets(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	conn, err := s.sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
			t.Error(err)
		}
	})
	var invoked atomic.Int32
	callback := func(core.Repositories) error { invoked.Add(1); return nil }
	start := time.Now()
	err = s.Write(ctx, callback)
	requireDomainError(t, err, core.ErrBusy)
	if elapsed := time.Since(start); elapsed > 2*s.options.RetryTimeout {
		t.Fatalf("занятая запись превысила общий бюджет: %v", elapsed)
	}
	if invoked.Load() != 0 {
		t.Fatal("callback выполнился без writer lock")
	}
	short, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	err = s.Write(short, callback)
	requireDomainError(t, err, context.DeadlineExceeded)
	if invoked.Load() != 0 {
		t.Fatal("отменённый callback выполнился")
	}
}

func TestTransientCallbackRetryRollsBackEveryFailedAttempt(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	attempts := 0
	err := f.store.Write(ctx, func(r core.Repositories) error {
		attempts++
		value := core.Stream{Record: core.Record{ID: "retry-stream", Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}, Name: "Повтор", PublicationKey: "retry-synthetic-key"}
		if err := r.Config.InsertStream(ctx, value); err != nil {
			return err
		}
		if attempts == 1 {
			return translate(sqlite3.Error{Code: sqlite3.ErrBusy, ExtendedCode: sqlite3.ErrBusy.Extend(0)})
		}
		return nil
	})
	if err != nil || attempts != 2 {
		t.Fatalf("повтор целого откатившегося callback: попыток %d, ошибка %v", attempts, err)
	}
	if err := f.store.View(ctx, func(r core.Repositories) error {
		got, err := r.Config.Stream(ctx, "retry-stream")
		if err != nil {
			return err
		}
		if got.PublicationKey != "retry-synthetic-key" {
			t.Fatal("успешная попытка не сохранилась")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.store.sqlDB.Stats().InUse; got != 0 {
		t.Fatalf("повтор оставил соединения: %d", got)
	}
}

func TestCanceledAndUnknownFailuresDoNotBecomeNotFound(t *testing.T) {
	s := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := s.Write(ctx, func(core.Repositories) error { called = true; return nil })
	requireDomainError(t, err, context.Canceled)
	if called {
		t.Fatal("отменённая операция выполнила callback")
	}
	unknown := errors.New("синтетическая неизвестная ошибка")
	if got := translate(unknown); got != unknown || errors.Is(got, core.ErrNotFound) {
		t.Fatalf("неизвестная ошибка заменена: %v", got)
	}
	requireDomainError(t, translate(sqlite3.Error{Code: sqlite3.ErrCorrupt}), core.ErrCorrupt)
}
