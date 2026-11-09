package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/mattn/go-sqlite3"
	"gorm.io/gorm"
)

type repositories struct {
	db       *gorm.DB
	active   atomic.Bool
	writable bool
}

func (r *repositories) access(ctx context.Context, write bool) (*gorm.DB, error) {
	if !r.active.Load() {
		return nil, core.ErrClosed
	}
	if write && !r.writable {
		return nil, fmt.Errorf("%w: запись вне Write", core.ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.db.WithContext(ctx), nil
}

func (r *repositories) contracts() core.Repositories {
	return core.Repositories{Config: r, Runtime: r, Artifacts: r}
}

// View выполняет согласованное чтение на одном соединении без захвата блокировки записи.
func (s *Store) View(ctx context.Context, callback func(core.Repositories) error) (retErr error) {
	if s.closed.Load() {
		return core.ErrClosed
	}
	if callback == nil {
		return core.ErrInvalid
	}
	conn, err := s.sqlDB.Conn(ctx)
	if err != nil {
		return translate(err)
	}
	defer func() {
		if e := conn.Close(); e != nil && !errors.Is(e, sql.ErrConnDone) {
			retErr = errors.Join(retErr, e)
		}
	}()
	if _, err = conn.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
		return translate(err)
	}
	defer func() {
		// Откат нужен и при panic; отдельный ограниченный контекст позволяет очистить отменённое чтение.
		cleanup, cancel := context.WithTimeout(context.Background(), s.options.RetryTimeout)
		defer cancel()
		_, rollbackErr := conn.ExecContext(cleanup, "ROLLBACK")
		if rollbackErr != nil {
			// Соединение с неподтверждённым откатом нельзя возвращать в пул.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		retErr = errors.Join(retErr, translate(rollbackErr))
	}()
	db := s.db.Session(&gorm.Session{NewDB: true, Context: ctx})
	db.Statement.ConnPool = conn
	r := &repositories{db: db}
	r.active.Store(true)
	defer r.active.Store(false)
	return callback(r.contracts())
}

// Write повторяет только полностью откатившиеся конфликты блокировки.
func (s *Store) Write(ctx context.Context, callback func(core.Repositories) error) error {
	if s.closed.Load() {
		return core.ErrClosed
	}
	if callback == nil {
		return core.ErrInvalid
	}
	budget, cancel := context.WithTimeout(ctx, s.options.RetryTimeout)
	defer cancel()
	return s.authority.GuardWrite(budget, s, s.options.RetryTimeout, func() (bool, error) {
		return s.writeWithBudget(budget, callback)
	})
}

func (s *Store) writeWithBudget(budget context.Context, callback func(core.Repositories) error) (bool, error) {
	if s.closed.Load() {
		return false, core.ErrClosed
	}
	for attempt := 0; attempt < s.options.RetryAttempts; attempt++ {
		retry, uncertain, err := s.writeAttempt(budget, callback)
		if err == nil {
			return uncertain, nil
		}
		if budget.Err() != nil {
			return uncertain, budget.Err()
		}
		if !retry || attempt+1 == s.options.RetryAttempts {
			return uncertain, translate(err)
		}
		timer := time.NewTimer(s.options.RetryDelay)
		select {
		case <-budget.Done():
			timer.Stop()
			return false, budget.Err()
		case <-timer.C:
		}
	}
	return false, core.ErrBusy
}

func (s *Store) writeAttempt(ctx context.Context, callback func(core.Repositories) error) (retry bool, uncertain bool, retErr error) {
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return isLock(err), false, err
	}
	committed := false
	defer func() {
		if !committed {
			err := tx.Rollback()
			if err != nil && !errors.Is(err, sql.ErrTxDone) {
				retErr = errors.Join(retErr, err)
				retry = false
				uncertain = true
			}
		}
	}()
	db := s.db.Session(&gorm.Session{NewDB: true, Context: ctx})
	db.Statement.ConnPool = tx
	r := &repositories{db: db, writable: true}
	r.active.Store(true)
	defer r.active.Store(false)
	if err = callback(r.contracts()); err != nil {
		return isLock(err), false, err
	}
	// Неоднозначный результат COMMIT не повторяется: сервис сверяет сохранённую квитанцию.
	err = tx.Commit()
	committed = err == nil
	return false, true, err
}

func isLock(err error) bool {
	var sqliteErr sqlite3.Error
	return errors.As(err, &sqliteErr) && (sqliteErr.Code == sqlite3.ErrBusy || sqliteErr.Code == sqlite3.ErrLocked)
}

func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, sql.ErrNoRows) {
		return core.ErrNotFound
	}
	if errors.Is(err, sql.ErrConnDone) {
		return core.ErrClosed
	}
	for _, kind := range []error{core.ErrNotFound, core.ErrConflict, core.ErrInUse, core.ErrInvalid, core.ErrBusy, core.ErrCorrupt, core.ErrClosed} {
		if errors.Is(err, kind) {
			return err
		}
	}
	if sqliteErr, ok := errors.AsType[sqlite3.Error](err); ok {
		var kind error
		switch sqliteErr.Code {
		case sqlite3.ErrBusy, sqlite3.ErrLocked:
			kind = core.ErrBusy
		case sqlite3.ErrCorrupt, sqlite3.ErrNotADB:
			kind = core.ErrCorrupt
		case sqlite3.ErrInterrupt:
			kind = context.Canceled
		case sqlite3.ErrConstraint:
			switch sqliteErr.ExtendedCode {
			case sqlite3.ErrConstraintForeignKey:
				kind = core.ErrInUse
			case sqlite3.ErrConstraintUnique, sqlite3.ErrConstraintPrimaryKey:
				kind = core.ErrConflict
			case sqlite3.ErrConstraintTrigger:
				// SQLite исполняет ON DELETE RESTRICT внутренним FK-триггером.
				if sqliteErr.Error() == "FOREIGN KEY constraint failed" {
					kind = core.ErrInUse
				} else {
					kind = core.ErrInvalid
				}
			default:
				kind = core.ErrInvalid
			}
		}
		if kind != nil {
			return fmt.Errorf("%w: SQLite %d/%d: %w", kind, sqliteErr.Code, sqliteErr.ExtendedCode, err)
		}
	}
	return err
}

var _ core.UnitOfWork = (*Store)(nil)
var _ core.RuntimeAuthorityProvider = (*Store)(nil)
var _ core.ConfigRepository = (*repositories)(nil)
var _ core.RuntimeRepository = (*repositories)(nil)
var _ core.ArtifactRepository = (*repositories)(nil)

// WriteOwned проверяет точный token под тем же допуском, что и запись БД.
func (s *Store) WriteOwned(ctx context.Context, owner *core.OutputObserver, callback func(core.Repositories) error) error {
	if s.closed.Load() {
		return core.ErrClosed
	}
	if callback == nil {
		return core.ErrInvalid
	}
	budget, cancel := context.WithTimeout(ctx, s.options.RetryTimeout)
	defer cancel()
	return s.authority.GuardOwnedWrite(budget, s, owner, s.options.RetryTimeout, func() (bool, error) { return s.writeWithBudget(budget, callback) })
}

var _ core.RuntimeOwnedUnitOfWork = (*Store)(nil)
