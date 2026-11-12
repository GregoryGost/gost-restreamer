package storage

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	sqlitemigrations "github.com/GregoryGost/gost-restreamer/migrations/sqlite"
	"github.com/pressly/goose/v3"
)

// Задаёт конечные явные бюджеты SQLite, подготовки, наблюдений и физических исполнений для
// изолированных тестов.
func testOptions() Options {
	return Options{PreparationLimits: core.PreparationLimits{MaxQueued: 16, MaxProfiles: 32}, OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 128, MaxIdentifierBytes: 1024, MaxBytes: 4 << 20}, ObservationLimits: core.ObservationLimits{MaxSessions: 16, MaxGroups: 64, MaxOutputs: 128, MaxFFmpegWorkers: 64, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond}
}

// Открывает временную файловую SQLite с рабочими миграциями и регистрирует закрытие Store в очистке
// теста.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	// s владеет временным Store проверяемого сценария. err сообщает об ошибке открытия изолированной
	// файловой SQLite.
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.sqlite"), testOptions())
	if err != nil {
		t.Fatalf("открыть изолированную SQLite: %v", err)
	}
	t.Cleanup(func() {
		// Закрывает временный Store после теста и сообщает ошибку закрытия SQLite.

		// err сообщает об ошибке закрытия проверяемого Store.
		if err := s.Close(); err != nil {
			t.Errorf("закрыть SQLite: %v", err)
		}
	})
	return s
}

// Проверяет повторный запуск рабочих миграций без изменения схемы, движка и параметров сборки SQLite и
// без закрытия общего пула.
func TestProductionMigrationsRepeatWithoutClosingPool(t *testing.T) {
	// s владеет временным Store проверяемого сценария.
	s := openTestStore(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// before сохраняет измеренные свойства движка и схемы до повторного запуска миграций. err
	// сообщает об ошибке чтения фактических свойств движка и схемы.
	before, err := s.Diagnostics(ctx)
	if err != nil || before.SchemaVersion != 1 || before.SQLiteVersion == "" || len(before.CompileOptions) == 0 {
		t.Fatalf("диагностика production-миграций: %+v, %v", before, err)
	}
	// err сообщает об ошибке применения встроенной истории схемы.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("повторный runner: %v", err)
	}
	// after содержит свойства движка и схемы после повторного запуска миграций.
	after, err := s.Diagnostics(ctx)
	if err != nil || before.SchemaVersion != after.SchemaVersion || before.SQLiteVersion != after.SQLiteVersion || !reflect.DeepEqual(before.CompileOptions, after.CompileOptions) {
		t.Fatalf("повтор runner изменил движок или схему: %+v, %v", after, err)
	}
	// err сообщает об ошибке проверки доступности общего пула.
	if err := s.sqlDB.PingContext(ctx); err != nil {
		t.Fatalf("runner закрыл общий пул: %v", err)
	}
	t.Logf("SQLite %s, schema %d, compile options %v", after.SQLiteVersion, after.SchemaVersion, after.CompileOptions)
}

// Проверяет foreign_keys, busy_timeout, FULL и WAL на трёх одновременно занятых физических соединениях
// того же движка.
func TestEveryPhysicalConnectionEnforcesPragmas(t *testing.T) {
	// s владеет временным Store проверяемого сценария.
	s := openTestStore(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// diagnostics содержит свойства того же SQLite-движка и текущую версию схемы. err сообщает об
	// ошибке чтения фактических свойств движка и схемы.
	diagnostics, err := s.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// connections удерживает одновременно занятые физические соединения для независимой проверки
	// PRAGMA.
	var connections []*sql.Conn
	t.Cleanup(func() {
		// Возвращает все одновременно занятые физические соединения в пул после проверки их PRAGMA и движка.

		// conn содержит отдельное физическое соединение для проверки его настроек и движка.
		for _, conn := range connections {
			// err сообщает об ошибке возврата отдельного соединения в пул.
			if err := conn.Close(); err != nil {
				t.Errorf("вернуть физическое соединение: %v", err)
			}
		}
	})
	// i указывает очередное из трёх одновременно занятых физических соединений.
	for i := range 3 {
		// conn содержит отдельное физическое соединение для проверки его настроек и движка. err
		// сообщает об ошибке получения отдельного физического соединения.
		conn, err := s.sqlDB.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
		// pragma выбирает проверяемую настройку физического соединения. expected задаёт
		// ожидаемое численное значение выбранной PRAGMA.
		for pragma, expected := range map[string]int{"foreign_keys": 1, "busy_timeout": 10, "synchronous": 2} {
			// actual принимает фактическое численное значение выбранной PRAGMA.
			var actual int
			// err сообщает об ошибке чтения проверяемого свойства или ограничения SQLite.
			if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&actual); err != nil || actual != expected {
				t.Fatalf("соединение %d, %s = %d, нужно %d, ошибка %v", i, pragma, actual, expected, err)
			}
		}
		// mode принимает фактический режим журнала SQLite. version принимает фактическую версию
		// SQLite данного соединения.
		var mode, version string
		// err сообщает об ошибке чтения проверяемого свойства или ограничения SQLite.
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
			t.Fatalf("соединение %d: journal_mode = %q, %v", i, mode, err)
		}
		// err сообщает об ошибке чтения проверяемого свойства или ограничения SQLite.
		if err := conn.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&version); err != nil || version != diagnostics.SQLiteVersion {
			t.Fatalf("соединение %d: версия SQLite = %q, %v", i, version, err)
		}
		// rows предоставляет строки параметров компиляции SQLite.
		rows, err := conn.QueryContext(ctx, "PRAGMA compile_options")
		if err != nil {
			t.Fatal(err)
		}
		// options накапливает параметры компиляции движка текущего физического соединения.
		var options []string
		for rows.Next() {
			// option принимает очередной параметр компиляции SQLite.
			var option string
			// err сообщает об ошибке чтения очередного параметра компиляции.
			if err := rows.Scan(&option); err != nil {
				// closeErr содержит результат освобождения проверяемого ресурса SQLite.
				if closeErr := rows.Close(); closeErr != nil {
					t.Error(closeErr)
				}
				t.Fatal(err)
			}
			options = append(options, option)
		}
		// rowErr содержит результат завершения обхода параметров компиляции.
		rowErr := rows.Err()
		// closeErr содержит результат освобождения проверяемого ресурса SQLite.
		closeErr := rows.Close()
		if rowErr != nil || closeErr != nil || !reflect.DeepEqual(options, diagnostics.CompileOptions) {
			t.Fatalf("соединение %d использует другие compile options: %v, %v", i, rowErr, closeErr)
		}
	}
	// got содержит число одновременно занятых физических соединений.
	if got := s.sqlDB.Stats().InUse; got != len(connections) {
		t.Fatalf("нужны одновременно занятые разные соединения: %d", got)
	}
}

// Проверяет откат частичной схемы и истории версии при ошибке добавочной миграции через рабочий
// исполнитель Goose без потери соединений.
func TestFailedMigrationRollsBackSchemaAndVersionThroughProductionRunner(t *testing.T) {
	// s владеет временным Store проверяемого сценария.
	s := openTestStore(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// initial содержит точные байты рабочей первичной SQL-миграции. err сообщает об ошибке чтения
	// встроенной первичной миграции.
	initial, err := fs.ReadFile(sqlitemigrations.Files, "00001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	// files объединяет рабочую первичную миграцию с намеренно ошибочной добавочной миграцией.
	files := fstest.MapFS{
		"00001_initial.sql": &fstest.MapFile{Data: initial},
		"00002_failure.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE migration_partial (id INTEGER PRIMARY KEY);\nINSERT INTO missing_migration_target(id) VALUES (1);\n")},
	}
	// provider содержит тестовый исполнитель миграций Goose с ошибочной добавочной миграцией.
	provider, err := goose.NewProvider(goose.DialectSQLite3, s.sqlDB, files, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	// original сохраняет рабочий исполнитель миграций для восстановления после отрицательного
	// сценария.
	original := s.provider
	s.provider = provider
	err = s.Migrate(ctx)
	s.provider = original
	if err == nil {
		t.Fatal("неисправная миграция успешно применена")
	}
	// count принимает число оставшихся объектов частичной неуспешной миграции.
	var count int
	// err сообщает об ошибке чтения проверяемого свойства или ограничения SQLite.
	if err := s.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE name = ?", "migration_partial").Scan(&count); err != nil || count != 0 {
		t.Fatalf("неисправная миграция оставила частичную схему: %d, %v", count, err)
	}
	// diagnostics содержит свойства того же SQLite-движка и текущую версию схемы.
	diagnostics, err := s.Diagnostics(ctx)
	if err != nil || diagnostics.SchemaVersion != 1 {
		t.Fatalf("неудачная миграция изменила version history: %+v, %v", diagnostics, err)
	}
	// err сообщает об ошибке применения встроенной истории схемы.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("ошибка миграции повредила исходный runner/pool: %v", err)
	}
	if s.sqlDB.Stats().InUse != 0 {
		t.Fatal("ошибка миграции оставила соединения")
	}
}

// Проверяет категорию ErrCorrupt повторного открытия повреждённого файла и освобождение блокировки
// после ошибки.
func TestCorruptDatabaseOpenMapsErrorAndReleasesLock(t *testing.T) {
	// dbPath задаёт путь изолированного файла БД текущего сценария.
	dbPath := filepath.Join(t.TempDir(), "corrupt.sqlite")
	// err сообщает об ошибке создания заведомо повреждённого файла SQLite.
	if err := os.WriteFile(dbPath, []byte("синтетическое повреждённое содержимое базы"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		// s владеет временным Store проверяемого сценария. err сообщает об ошибке открытия
		// изолированной файловой SQLite.
		s, err := Open(context.Background(), dbPath, testOptions())
		if err == nil {
			// closeErr содержит результат освобождения проверяемого ресурса SQLite.
			if closeErr := s.Close(); closeErr != nil {
				t.Error(closeErr)
			}
			t.Fatal("повреждённая база открылась")
		}
		requireDomainError(t, err, core.ErrCorrupt)
	}
	// err сообщает об ошибке удаления принадлежащего тесту повреждённого файла.
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	// s владеет временным Store проверяемого сценария. err сообщает об ошибке открытия изолированной
	// файловой SQLite.
	s, err := Open(context.Background(), dbPath, testOptions())
	if err != nil {
		t.Fatalf("ошибка открытия оставила lifetime lock: %v", err)
	}
	// err сообщает об ошибке закрытия проверяемого Store.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// Проверяет единственного владельца канонического пути через символическую ссылку, идемпотентное
// закрытие и новую incarnation при повторном открытии.
func TestStoreLockCanonicalPathCloseAndReopen(t *testing.T) {
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// dir содержит изолированный каталог БД и её символического псевдонима.
	dir := t.TempDir()
	// dbPath задаёт путь изолированного файла БД текущего сценария.
	dbPath := filepath.Join(dir, "state.sqlite")
	// s владеет временным Store проверяемого сценария. err сообщает об ошибке открытия изолированной
	// файловой SQLite.
	s, err := Open(ctx, dbPath, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает первоначальный пул теста канонического пути, включая ранний отказ проверки.

		// err сообщает об ошибке закрытия проверяемого Store.
		if err := s.Close(); err != nil {
			t.Errorf("закрыть первоначальный пул: %v", err)
		}
	})
	// alias задаёт символический псевдоним канонического файла БД.
	alias := filepath.Join(dir, "alias.sqlite")
	// err сообщает об ошибке создания тестового символического псевдонима БД.
	if err := os.Symlink(dbPath, alias); err != nil {
		t.Fatal(err)
	}
	// other содержит результат попытки второго открытия уже занятой БД.
	other, err := Open(ctx, alias, testOptions())
	if err == nil {
		// closeErr содержит результат освобождения проверяемого ресурса SQLite.
		if closeErr := other.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("два владельца получили один канонический файл")
	}
	// incarnation сохраняет идентичность первого открытия для проверки смены владельца.
	incarnation := s.Incarnation()
	if incarnation == "" {
		t.Fatal("отсутствует incarnation владельца")
	}
	// err сообщает об ошибке закрытия проверяемого Store.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке закрытия проверяемого Store.
	if err := s.Close(); err != nil {
		t.Fatalf("Close не идемпотентен: %v", err)
	}
	// err сообщает об ошибке проверки доступности общего пула.
	if err := s.sqlDB.PingContext(ctx); err == nil {
		t.Fatal("закрытый пул принимает запросы")
	}
	// reopened содержит заново открытый Store после освобождения прежней блокировки.
	reopened, err := Open(ctx, alias, testOptions())
	if err != nil {
		t.Fatalf("ошибка открытия по alias оставила блокировку: %v", err)
	}
	t.Cleanup(func() {
		// Закрывает повторно открытый пул после проверки смены Incarnation.

		// err сообщает об ошибке закрытия проверяемого Store.
		if err := reopened.Close(); err != nil {
			t.Errorf("закрыть повторный пул: %v", err)
		}
	})
	if reopened.Incarnation() == incarnation || reopened.Incarnation() == "" {
		t.Fatal("новое открытие не сменило владельца")
	}
}

// Проверяет отказ неверных бюджетов и заранее отменённого открытия до создания файла БД.
func TestInvalidOptionsAndCanceledOpenLeaveNoDatabase(t *testing.T) {
	// change вносит выбранное нарушение явных бюджетов открытия.
	for _, change := range []func(*Options){
		func(o *Options) {
			// Обнуляет MaxOpenConns для проверки отказа до создания файла БД.
			o.MaxOpenConns = 0
		}, func(o *Options) {
			// Задаёт MaxIdleConns выше MaxOpenConns для проверки отказа до создания файла БД.
			o.MaxIdleConns = 5
		},
		func(o *Options) {
			// Обнуляет срок жизни соединения для проверки явного бюджета.
			o.ConnMaxLifetime = 0
		}, func(o *Options) {
			// Обнуляет срок простоя соединения для проверки явного бюджета.
			o.ConnMaxIdleTime = 0
		},
		func(o *Options) {
			// Задаёт BusyTimeout меньше миллисекунды для проверки допустимой точности и минимума.
			o.BusyTimeout = time.Nanosecond
		}, func(o *Options) {
			// Обнуляет число попыток записи.
			o.RetryAttempts = 0
		},
		func(o *Options) {
			// Обнуляет общий срок повторов записи.
			o.RetryTimeout = 0
		}, func(o *Options) {
			// Обнуляет задержку между повторами записи.
			o.RetryDelay = 0
		},
	} {
		// options содержит исходные допустимые параметры открытия перед внесением выбранного
		// нарушения.
		options := testOptions()
		change(&options)
		// p задаёт путь БД, который не должен появиться после отказа проверки или отмены.
		p := filepath.Join(t.TempDir(), "invalid.sqlite")
		// s владеет временным Store проверяемого сценария. err сообщает об ошибке открытия
		// изолированной файловой SQLite.
		if s, err := Open(context.Background(), p, options); err == nil {
			// closeErr содержит результат освобождения проверяемого ресурса SQLite.
			if closeErr := s.Close(); closeErr != nil {
				t.Error(closeErr)
			}
			t.Fatal("некорректный бюджет принят")
		}
		// err сообщает об ошибке проверки отсутствия файла после отклонённого открытия.
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("невалидное открытие создало файл: %v", err)
		}
	}
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite. cancel освобождает или
	// отменяет контекст проверяемого ожидания.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// p задаёт путь БД, который не должен появиться после отказа проверки или отмены.
	p := filepath.Join(t.TempDir(), "canceled.sqlite")
	// err сообщает об ошибке открытия изолированной файловой SQLite.
	if _, err := Open(ctx, p, testOptions()); !errors.Is(err, context.Canceled) {
		t.Fatalf("отмена открытия потеряна: %v", err)
	}
	// err сообщает об ошибке проверки отсутствия файла после отклонённого открытия.
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("отменённое открытие создало файл: %v", err)
	}
}
