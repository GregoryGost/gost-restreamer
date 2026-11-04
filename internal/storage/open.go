// Package storage реализует хранение доменных данных в SQLite.
package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pressly/goose/v3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	sqlitemigrations "github.com/GregoryGost/gost-restreamer/migrations/sqlite"
)

// Options задаёт явные конечные бюджеты соединений и повторов записи.
type Options struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	BusyTimeout     time.Duration
	RetryAttempts   int
	RetryTimeout    time.Duration
	RetryDelay      time.Duration
}

func (o Options) validate() error {
	if o.MaxOpenConns <= 0 || o.MaxIdleConns < 0 || o.MaxIdleConns > o.MaxOpenConns ||
		o.ConnMaxLifetime <= 0 || o.ConnMaxIdleTime <= 0 || o.BusyTimeout < time.Millisecond ||
		o.BusyTimeout%time.Millisecond != 0 || o.BusyTimeout/time.Millisecond > math.MaxInt32 ||
		o.RetryAttempts <= 0 || o.RetryTimeout <= 0 || o.RetryDelay <= 0 {
		return fmt.Errorf("%w: некорректные бюджеты SQLite", core.ErrInvalid)
	}
	return nil
}

// Store владеет общим пулом, миграциями и блокировкой файла до Close.
type Store struct {
	db          *gorm.DB
	sqlDB       *sql.DB
	lock        *os.File
	options     Options
	incarnation string
	authority   *core.RuntimeAuthority
	provider    *goose.Provider
	closeOnce   sync.Once
	closeErr    error
	closed      atomic.Bool
}

// Open открывает файловую БД и применяет встроенные миграции под блокировкой.
func Open(ctx context.Context, path string, options Options) (_ *Store, retErr error) {
	if err := options.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	canonical, lock, err := lockDatabase(path)
	if err != nil {
		return nil, err
	}
	s := &Store{lock: lock, options: options}
	defer func() {
		if retErr != nil {
			retErr = translate(errors.Join(retErr, s.Close()))
		}
	}()
	var token [24]byte
	if _, err = rand.Read(token[:]); err != nil {
		return nil, fmt.Errorf("создать идентификатор владельца: %w", err)
	}
	s.incarnation = hex.EncodeToString(token[:])
	s.authority, err = core.NewRuntimeAuthority(s.incarnation)
	if err != nil {
		return nil, err
	}
	dsn := url.URL{Scheme: "file", Path: canonical}
	q := url.Values{"mode": {"rw"}, "cache": {"private"}, "_foreign_keys": {"on"}, "_journal_mode": {"WAL"}, "_synchronous": {"FULL"}, "_txlock": {"immediate"}, "_busy_timeout": {strconv.FormatInt(int64(options.BusyTimeout/time.Millisecond), 10)}}
	dsn.RawQuery = q.Encode()
	s.sqlDB, err = sql.Open("sqlite3", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("открыть SQLite: %w", err)
	}
	s.sqlDB.SetMaxOpenConns(options.MaxOpenConns)
	s.sqlDB.SetMaxIdleConns(options.MaxIdleConns)
	s.sqlDB.SetConnMaxLifetime(options.ConnMaxLifetime)
	s.sqlDB.SetConnMaxIdleTime(options.ConnMaxIdleTime)
	if err = s.sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("проверить SQLite: %w", err)
	}
	s.db, err = gorm.Open(sqlite.New(sqlite.Config{Conn: s.sqlDB}), &gorm.Config{SkipDefaultTransaction: true, DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("создать отображение SQLite: %w", err)
	}
	s.provider, err = goose.NewProvider(goose.DialectSQLite3, s.sqlDB, sqlitemigrations.Files, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return nil, fmt.Errorf("создать исполнитель миграций: %w", err)
	}
	if err = s.Migrate(ctx); err != nil {
		return nil, err
	}
	service, err := core.NewService(s, s.incarnation)
	if err != nil {
		return nil, err
	}
	if err = service.Recover(ctx); err != nil {
		return nil, fmt.Errorf("восстановить состояние SQLite: %w", err)
	}
	return s, nil
}

func lockDatabase(path string) (string, *os.File, error) {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return "", nil, errors.New("нужен путь к файловой SQLite")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("разрешить путь SQLite: %w", err)
	}
	if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return "", nil, fmt.Errorf("создать каталог SQLite: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", nil, fmt.Errorf("разрешить каталог SQLite: %w", err)
	}
	canonical := filepath.Join(parent, filepath.Base(abs))
	if resolved, resolveErr := filepath.EvalSymlinks(canonical); resolveErr == nil {
		canonical = resolved
	} else if !errors.Is(resolveErr, os.ErrNotExist) {
		return "", nil, fmt.Errorf("разрешить файл SQLite: %w", resolveErr)
	}
	f, err := os.OpenFile(canonical, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", nil, fmt.Errorf("открыть файл SQLite: %w", err)
	}
	fail := func(err error) (string, *os.File, error) { return "", nil, errors.Join(err, f.Close()) }
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() {
		return fail(errors.New("SQLite должен быть обычным файлом"))
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
		return fail(errors.New("SQLite не поддерживает дополнительные жёсткие ссылки"))
	}
	if err = f.Chmod(0600); err != nil {
		return fail(fmt.Errorf("защитить файл SQLite: %w", err))
	}
	if err = f.Close(); err != nil {
		return "", nil, err
	}
	// На Darwin flock файла БД конфликтует с блокировками самого SQLite.
	lock, err := os.OpenFile(canonical+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return "", nil, fmt.Errorf("открыть блокировку SQLite: %w", err)
	}
	lockInfo, err := lock.Stat()
	if err != nil {
		return "", nil, errors.Join(err, lock.Close())
	}
	if !lockInfo.Mode().IsRegular() {
		return "", nil, errors.Join(core.ErrInvalid, lock.Close())
	}
	if stat, ok := lockInfo.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
		return "", nil, errors.Join(core.ErrInvalid, lock.Close())
	}
	if err = lock.Chmod(0600); err != nil {
		return "", nil, errors.Join(err, lock.Close())
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return "", nil, errors.Join(fmt.Errorf("%w: БД уже принадлежит другому владельцу", core.ErrInUse), lock.Close())
	}
	return canonical, lock, nil
}

// Incarnation возвращает идентификатор текущего владельца исполнения.
func (s *Store) Incarnation() string { return s.incarnation }

// RuntimeAuthority возвращает общий допуск всех экземпляров Service и записей этого Store.
func (s *Store) RuntimeAuthority() *core.RuntimeAuthority { return s.authority }

// Migrate повторно применяет только ещё не выполненные встроенные миграции.
func (s *Store) Migrate(ctx context.Context) error {
	if s.closed.Load() {
		return core.ErrClosed
	}
	if _, err := s.provider.Up(ctx); err != nil {
		return fmt.Errorf("применить миграции SQLite: %w", translate(err))
	}
	return nil
}

// Close освобождает пул перед снятием блокировки; вызывающий сначала останавливает пользователей.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if s.authority != nil {
			s.authority.Close()
		}
		if s.sqlDB != nil {
			s.closeErr = s.sqlDB.Close()
		}
		if s.lock != nil {
			s.closeErr = errors.Join(s.closeErr, s.lock.Close())
		}
	})
	return s.closeErr
}

// Diagnostics содержит измеренные свойства движка и общего пула.
type Diagnostics struct {
	SQLiteVersion  string
	CompileOptions []string
	Pool           sql.DBStats
	SchemaVersion  int64
}

// Diagnostics читает свойства того же движка, который выполняет миграции.
func (s *Store) Diagnostics(ctx context.Context) (_ Diagnostics, retErr error) {
	if s.closed.Load() {
		return Diagnostics{}, core.ErrClosed
	}
	defer func() { retErr = translate(retErr) }()
	d := Diagnostics{Pool: s.sqlDB.Stats()}
	if err := s.sqlDB.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&d.SQLiteVersion); err != nil {
		return d, err
	}
	rows, err := s.sqlDB.QueryContext(ctx, "PRAGMA compile_options")
	if err != nil {
		return d, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	for rows.Next() {
		var option string
		if err = rows.Scan(&option); err != nil {
			return d, err
		}
		d.CompileOptions = append(d.CompileOptions, option)
	}
	if err = rows.Err(); err != nil {
		return d, err
	}
	if err = rows.Close(); err != nil {
		return d, err
	}
	d.SchemaVersion, err = s.provider.GetDBVersion(ctx)
	return d, err
}
