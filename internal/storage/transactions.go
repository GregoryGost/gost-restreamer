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

// repositories ограничивает доступ к одной GORM-транзакции временем исполнения callback.
type repositories struct {
	// db содержит GORM-сессию, связанную с текущим соединением или транзакцией.
	db *gorm.DB
	// active разрешает использование репозитория только пока callback не завершился.
	active atomic.Bool
	// writable разрешает изменения только репозиториям операции Write.
	writable bool
}

// access разрешает использование репозитория только во время вызова переданной функции и при живом
// контексте; изменение через читающий репозиторий отклоняется.
func (r *repositories) access(ctx context.Context, write bool) (*gorm.DB, error) {
	if !r.active.Load() {
		return nil, core.ErrClosed
	}
	if write && !r.writable {
		return nil, fmt.Errorf("%w: запись вне Write", core.ErrInvalid)
	}
	// err содержит отмену контекста перед выдачей GORM-сессии.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.db.WithContext(ctx), nil
}

// contracts возвращает доменные интерфейсы одного набора репозиториев, связанных с текущей
// транзакцией.
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
	// conn содержит выделенное соединение согласованного чтения, а err — результат получения его из
	// пула.
	conn, err := s.sqlDB.Conn(ctx)
	if err != nil {
		return translate(err)
	}
	defer func() {
		// Возвращает занятое View соединение в пул и добавляет ошибку закрытия, кроме уже закрытого
		// sql.ErrConnDone.

		// e сообщает об ошибке возврата соединения View в пул.
		if e := conn.Close(); e != nil && !errors.Is(e, sql.ErrConnDone) {
			retErr = errors.Join(retErr, e)
		}
	}()
	if _, err = conn.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
		return translate(err)
	}
	defer func() {
		// При любом выходе из View, включая панику, откатывает читающую транзакцию с отдельным конечным
		// сроком; неподтверждённый откат исключает соединение из пула.

		// Откат нужен и при panic; отдельный ограниченный контекст позволяет очистить отменённое чтение.
		// cleanup ограничивает откат чтения независимо от отмены операции, а cancel освобождает
		// этот контекст.
		cleanup, cancel := context.WithTimeout(context.Background(), s.options.RetryTimeout)
		defer cancel()
		// rollbackErr содержит результат отката читающей транзакции, определяющий пригодность
		// соединения для пула.
		_, rollbackErr := conn.ExecContext(cleanup, "ROLLBACK")
		if rollbackErr != nil {
			// Соединение с неподтверждённым откатом нельзя возвращать в пул.
			_ = conn.Raw(func(any) error {
				// Помечает физическое соединение как driver.ErrBadConn, чтобы пул не использовал транзакцию с
				// неподтверждённым откатом.
				return driver.ErrBadConn
			})
		}
		retErr = errors.Join(retErr, translate(rollbackErr))
	}()
	// db содержит отдельную GORM-сессию читающей операции на выделенном соединении.
	db := s.db.Session(&gorm.Session{NewDB: true, Context: ctx})
	db.Statement.ConnPool = conn
	// r предоставляет читающие доменные репозитории до возврата callback.
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
	// budget ограничивает весь цикл записи, а cancel освобождает его контекст.
	budget, cancel := context.WithTimeout(ctx, s.options.RetryTimeout)
	defer cancel()
	return s.authority.GuardWrite(budget, s, s.options.RetryTimeout, func() (bool, error) {
		// Под общим допуском выполняет ограниченные попытки записи и сообщает, мог ли произойти COMMIT, для
		// последующей сверки владельцев.

		return s.writeWithBudget(budget, callback)
	})
}

// writeWithBudget повторяет только подтверждённо откатившиеся конфликты блокировки в пределах попыток
// и контекста; признак возможного COMMIT передаётся для сверки RuntimeAuthority.
func (s *Store) writeWithBudget(budget context.Context, callback func(core.Repositories) error) (bool, error) {
	if s.closed.Load() {
		return false, core.ErrClosed
	}
	// attempt содержит номер текущей попытки транзакции записи, начиная с нуля.
	for attempt := 0; attempt < s.options.RetryAttempts; attempt++ {
		// retry разрешает повтор после подтверждённого отката, uncertain отмечает возможный
		// COMMIT, а err сообщает об ошибке попытки записи.
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
		// timer ограничивает задержку перед следующей разрешённой попыткой.
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

// writeAttempt выполняет переданную функцию в одной транзакции и закрывает доступ к репозиториям после
// возврата. Ошибка COMMIT либо неподтверждённый ROLLBACK запрещают повтор и требуют сверки допусков.
func (s *Store) writeAttempt(ctx context.Context, callback func(core.Repositories) error) (retry bool, uncertain bool, retErr error) {
	// tx содержит новую SQL-транзакцию записи, а err — результат её начала.
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return isLock(err), false, err
	}
	// committed показывает наличие подтверждённого COMMIT и необходимость отложенного отката.
	committed := false
	defer func() {
		// При отсутствии подтверждённого COMMIT откатывает транзакцию; ошибка отката запрещает повтор и
		// отмечает результат как неоднозначный.

		if !committed {
			// err сообщает об ошибке отката незавершённой записи; неожиданный отказ
			// запрещает повтор.
			err := tx.Rollback()
			if err != nil && !errors.Is(err, sql.ErrTxDone) {
				retErr = errors.Join(retErr, err)
				retry = false
				uncertain = true
			}
		}
	}()
	// db содержит отдельную GORM-сессию текущей транзакции записи.
	db := s.db.Session(&gorm.Session{NewDB: true, Context: ctx})
	db.Statement.ConnPool = tx
	// r предоставляет записывающие доменные репозитории до возврата callback.
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

// isLock распознаёт только SQLite BUSY и LOCKED, в том числе внутри обёрнутой ошибки.
func isLock(err error) bool {
	// sqliteErr принимает ошибку драйвера SQLite для проверки BUSY или LOCKED.
	var sqliteErr sqlite3.Error
	return errors.As(err, &sqliteErr) && (sqliteErr.Code == sqlite3.ErrBusy || sqliteErr.Code == sqlite3.ErrLocked)
}

// translate сопоставляет ошибки чтения, закрытия, блокировок и ограничений SQLite доменным причинам,
// сохраняя контекст отмены и исходную диагностическую ошибку движка.
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
	// kind содержит очередную уже переведённую доменную категорию ошибки.
	for _, kind := range []error{core.ErrNotFound, core.ErrConflict, core.ErrInUse, core.ErrInvalid, core.ErrBusy, core.ErrCorrupt, core.ErrClosed} {
		if errors.Is(err, kind) {
			return err
		}
	}
	// sqliteErr содержит извлечённую ошибку движка, а ok показывает наличие ошибки SQLite в цепочке.
	if sqliteErr, ok := errors.AsType[sqlite3.Error](err); ok {
		// kind принимает доменную категорию, соответствующую коду SQLite.
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

// WriteOwned проверяет точный токен наблюдателя под тем же допуском, что и запись БД.
func (s *Store) WriteOwned(ctx context.Context, owner *core.OutputObserver, callback func(core.Repositories) error) error {
	if s.closed.Load() {
		return core.ErrClosed
	}
	if callback == nil {
		return core.ErrInvalid
	}
	// budget ограничивает запись точного владельца с повторами, а cancel освобождает его контекст.
	budget, cancel := context.WithTimeout(ctx, s.options.RetryTimeout)
	defer cancel()
	return s.authority.GuardOwnedWrite(budget, s, owner, s.options.RetryTimeout, func() (bool, error) {
		// Под точным допуском OutputObserver выполняет ограниченные попытки записи и сообщает возможность
		// COMMIT для послезаписной сверки.
		return s.writeWithBudget(budget, callback)
	})
}

var _ core.RuntimeOwnedUnitOfWork = (*Store)(nil)
