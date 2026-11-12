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

// Options задаёт явные конечные бюджеты соединений, повторов записи и RAM-наблюдений.
type Options struct {
	// OutputExecutionLimits задаёт общий бюджет физических резервов выходов.
	OutputExecutionLimits core.OutputExecutionLimits
	// PreparationLimits задаёт общий бюджет очереди подготовки и числа её профилей.
	PreparationLimits core.PreparationLimits
	// ObservationLimits задаёт общие бюджеты оперативных наблюдений.
	ObservationLimits core.ObservationLimits
	// MaxOpenConns ограничивает число физических соединений общего пула.
	MaxOpenConns int
	// MaxIdleConns ограничивает число простаивающих соединений пула.
	MaxIdleConns int
	// ConnMaxLifetime ограничивает полный срок жизни соединения пула.
	ConnMaxLifetime time.Duration
	// ConnMaxIdleTime ограничивает время простоя соединения пула.
	ConnMaxIdleTime time.Duration
	// BusyTimeout задаёт время ожидания блокировки SQLite на каждом соединении в целых
	// миллисекундах.
	BusyTimeout time.Duration
	// RetryAttempts ограничивает число попыток полностью откатившейся записи.
	RetryAttempts int
	// RetryTimeout ограничивает весь цикл записи с повторами и отдельную сверку или очистку.
	RetryTimeout time.Duration
	// RetryDelay задаёт задержку между разрешёнными повторными попытками записи.
	RetryDelay time.Duration
}

// Validate проверяет бюджеты без открытия хранилища.
func (o Options) Validate() error {
	// err сообщает об ошибке проверки бюджета физических исполнений.
	if err := o.OutputExecutionLimits.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки бюджетов подготовки.
	if err := o.PreparationLimits.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки бюджетов наблюдений.
	if err := o.ObservationLimits.Validate(); err != nil {
		return err
	}
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
	// db предоставляет GORM-отображение поверх общего пула SQLite.
	db *gorm.DB
	// sqlDB владеет общим физическим пулом GORM и Goose до закрытия Store.
	sqlDB *sql.DB
	// lock удерживает файловую блокировку единственного владельца БД до закрытия Store.
	lock *os.File
	// options сохраняет проверенные явные бюджеты данного открытия Store.
	options Options
	// incarnation идентифицирует текущее открытие хранилища и его владельца исполнения.
	incarnation string
	// authority координирует допуски и оперативные данные всех Service одного Store.
	authority *core.RuntimeAuthority
	// provider применяет встроенную историю схемы Goose через тот же sql.DB.
	provider *goose.Provider
	// closeOnce ограничивает освобождение ресурсов одним вызовом.
	closeOnce sync.Once
	// closeErr сохраняет объединённую ошибку закрытия для повторных Close.
	closeErr error
	// closed позволяет отклонять операции после начала закрытия Store.
	closed atomic.Bool
}

// Open открывает файловую SQLite с единственным владельцем до Close. Он разрешает путь, создаёт
// необходимые каталог и файлы с ограниченными правами, получает неблокирующую эксклюзивную блокировку,
// настраивает пул и создаёт общую RuntimeAuthority. Повторное открытие занятой БД отклоняется.
// До возврата применяются встроенные миграции, проверяется формат хранилища и выполняется Recover,
// который изменяет долговечное управление незавершённых запусков прежнего владельца. При ошибке уже
// полученные ресурсы закрываются; после успеха вызывающий останавливает пользователей перед Close.
func Open(ctx context.Context, path string, options Options) (_ *Store, retErr error) {
	// err сообщает об ошибке проверки всех явных параметров открытия.
	if err := options.Validate(); err != nil {
		return nil, err
	}
	// err содержит отмену контекста до получения файловых ресурсов.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// canonical содержит разрешённый путь БД, lock удерживает единственного владельца, а err —
	// результат получения блокировки.
	canonical, lock, err := lockDatabase(path)
	if err != nil {
		return nil, err
	}
	// s содержит создаваемый Store, освобождающий полученные ресурсы при ошибке открытия.
	s := &Store{lock: lock, options: options}
	defer func() {
		// При ошибке Open закрывает уже полученные ресурсы Store и объединяет ошибку закрытия с исходной до
		// перевода в доменную категорию.

		if retErr != nil {
			retErr = translate(errors.Join(retErr, s.Close()))
		}
	}()
	// token принимает случайные байты идентичности нового владельца Store.
	var token [24]byte
	if _, err = rand.Read(token[:]); err != nil {
		return nil, fmt.Errorf("создать идентификатор владельца: %w", err)
	}
	s.incarnation = hex.EncodeToString(token[:])
	s.authority, err = core.NewRuntimeAuthority(s.incarnation, options.ObservationLimits, options.OutputExecutionLimits)
	if err != nil {
		return nil, err
	}
	// dsn задаёт файловый DSN канонического пути SQLite.
	dsn := url.URL{Scheme: "file", Path: canonical}
	// q задаёт параметры каждого соединения: private cache, FK, WAL, FULL, IMMEDIATE и конечное
	// ожидание блокировок.
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
	if err = s.checkStorageFormat(ctx); err != nil {
		return nil, err
	}
	// service предоставляет доменное восстановление прежних запусков перед возвратом открытого
	// Store.
	service, err := core.NewService(s, s.incarnation)
	if err != nil {
		return nil, err
	}
	if err = service.Recover(ctx); err != nil {
		return nil, fmt.Errorf("восстановить состояние SQLite: %w", err)
	}
	return s, nil
}

// lockDatabase разрешает канонический путь обычной файловой БД, запрещает дополнительные жёсткие
// ссылки и удерживает неблокирующую эксклюзивную блокировку отдельного файла .lock. Созданные каталог
// и файлы получают ограниченные права.
func lockDatabase(path string) (string, *os.File, error) {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return "", nil, errors.New("нужен путь к файловой SQLite")
	}
	// abs содержит абсолютный путь БД, а err — результат его разрешения.
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("разрешить путь SQLite: %w", err)
	}
	if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return "", nil, fmt.Errorf("создать каталог SQLite: %w", err)
	}
	// parent содержит путь родительского каталога после раскрытия символических ссылок.
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", nil, fmt.Errorf("разрешить каталог SQLite: %w", err)
	}
	// canonical содержит разрешённый путь файла БД для общего файла блокировки.
	canonical := filepath.Join(parent, filepath.Base(abs))
	// resolved содержит раскрытый путь существующей БД, а resolveErr — результат раскрытия ссылок.
	if resolved, resolveErr := filepath.EvalSymlinks(canonical); resolveErr == nil {
		canonical = resolved
	} else if !errors.Is(resolveErr, os.ErrNotExist) {
		return "", nil, fmt.Errorf("разрешить файл SQLite: %w", resolveErr)
	}
	// f содержит временно открытый файл БД для проверки типа, ссылок и прав.
	f, err := os.OpenFile(canonical, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", nil, fmt.Errorf("открыть файл SQLite: %w", err)
	}
	// fail закрывает предварительно открытый файл БД и сохраняет исходную ошибку проверки.
	fail := func(err error) (string, *os.File, error) {
		// Закрывает предварительно открытый файл SQLite при отказе проверки и возвращает обе возможные ошибки.
		return "", nil, errors.Join(err, f.Close())
	}
	// info содержит свойства открытого файла БД для проверки обычного файла.
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() {
		return fail(errors.New("SQLite должен быть обычным файлом"))
	}
	// stat содержит системные свойства файла БД, а ok показывает доступность числа жёстких ссылок.
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
	// lock содержит отдельный файл эксклюзивной блокировки без следования символической ссылке.
	lock, err := os.OpenFile(canonical+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return "", nil, fmt.Errorf("открыть блокировку SQLite: %w", err)
	}
	// lockInfo содержит свойства файла блокировки для проверки допустимого типа.
	lockInfo, err := lock.Stat()
	if err != nil {
		return "", nil, errors.Join(err, lock.Close())
	}
	if !lockInfo.Mode().IsRegular() {
		return "", nil, errors.Join(core.ErrInvalid, lock.Close())
	}
	// stat содержит системные свойства файла блокировки, а ok показывает доступность числа жёстких
	// ссылок.
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
	// err сообщает об ошибке применения ещё не выполненных встроенных миграций.
	if _, err := s.provider.Up(ctx); err != nil {
		return fmt.Errorf("применить миграции SQLite: %w", translate(err))
	}
	return nil
}

// Close освобождает пул перед снятием блокировки; вызывающий сначала останавливает пользователей.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		// Однократно закрывает общий допуск, затем пул и файл блокировки, сохраняя объединённую ошибку для
		// последующих Close.

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
	// SQLiteVersion содержит фактическую версию используемого движка SQLite.
	SQLiteVersion string
	// CompileOptions содержит параметры компиляции используемого движка.
	CompileOptions []string
	// Pool содержит измеренную статистику общего пула database/sql.
	Pool sql.DBStats
	// SchemaVersion содержит текущую версию схемы по истории Goose.
	SchemaVersion int64
}

// Diagnostics читает свойства того же движка, который выполняет миграции.
func (s *Store) Diagnostics(ctx context.Context) (_ Diagnostics, retErr error) {
	if s.closed.Load() {
		return Diagnostics{}, core.ErrClosed
	}
	defer func() {
		// После освобождения строк переводит итоговую ошибку Diagnostics в доменную категорию.
		retErr = translate(retErr)
	}()
	// d накапливает измеренные свойства движка, пула и схемы.
	d := Diagnostics{Pool: s.sqlDB.Stats()}
	// err сообщает об ошибке чтения фактической версии SQLite.
	if err := s.sqlDB.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&d.SQLiteVersion); err != nil {
		return d, err
	}
	// rows предоставляет строки параметров компиляции SQLite, а err — результат запроса.
	rows, err := s.sqlDB.QueryContext(ctx, "PRAGMA compile_options")
	if err != nil {
		return d, err
	}
	defer func() {
		// Закрывает строки PRAGMA compile_options при любом выходе и добавляет ошибку закрытия к результату
		// Diagnostics.
		retErr = errors.Join(retErr, rows.Close())
	}()
	for rows.Next() {
		// option принимает очередной параметр компиляции движка.
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

// ErrIncompatibleFormat обозначает БД с неподдерживаемым форматом управления.
var ErrIncompatibleFormat = errors.New("несовместимый формат хранилища")

// checkStorageFormat требует единственную запись storage_format с singleton=1 и version=2; отсутствие
// таблицы и иной маркер дают ErrIncompatibleFormat.
func (s *Store) checkStorageFormat(ctx context.Context) error {
	// count содержит число строк маркера формата, singleton — минимальный ключ единственной строки,
	// а version — её версию для проверки совместимости.
	var count, singleton, version int64
	// err сообщает об ошибке чтения обязательного маркера формата хранилища.
	if err := s.sqlDB.QueryRowContext(ctx, "SELECT count(*), coalesce(min(singleton),0), coalesce(min(version),0) FROM storage_format").Scan(&count, &singleton, &version); err != nil {
		return fmt.Errorf("%w: требуется первичная инициализация поддерживаемого формата: %w", ErrIncompatibleFormat, err)
	}
	if count != 1 || singleton != 1 || version != 2 {
		return fmt.Errorf("%w: неизвестный маркер формата", ErrIncompatibleFormat)
	}
	return nil
}

// PreparationLimits возвращает общий неизменяемый бюджет подготовки.
func (s *Store) PreparationLimits() core.PreparationLimits { return s.options.PreparationLimits }
