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

func testOptions() Options {
	return Options{ObservationLimits: core.ObservationLimits{MaxSessions: 16, MaxGroups: 64, MaxOutputs: 128, MaxFFmpegWorkers: 64, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.sqlite"), testOptions())
	if err != nil {
		t.Fatalf("открыть изолированную SQLite: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("закрыть SQLite: %v", err)
		}
	})
	return s
}

func TestProductionMigrationsRepeatWithoutClosingPool(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	before, err := s.Diagnostics(ctx)
	if err != nil || before.SchemaVersion != 1 || before.SQLiteVersion == "" || len(before.CompileOptions) == 0 {
		t.Fatalf("диагностика production-миграций: %+v, %v", before, err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("повторный runner: %v", err)
	}
	after, err := s.Diagnostics(ctx)
	if err != nil || before.SchemaVersion != after.SchemaVersion || before.SQLiteVersion != after.SQLiteVersion || !reflect.DeepEqual(before.CompileOptions, after.CompileOptions) {
		t.Fatalf("повтор runner изменил движок или схему: %+v, %v", after, err)
	}
	if err := s.sqlDB.PingContext(ctx); err != nil {
		t.Fatalf("runner закрыл общий пул: %v", err)
	}
	t.Logf("SQLite %s, schema %d, compile options %v", after.SQLiteVersion, after.SchemaVersion, after.CompileOptions)
}

func TestEveryPhysicalConnectionEnforcesPragmas(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	diagnostics, err := s.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var connections []*sql.Conn
	t.Cleanup(func() {
		for _, conn := range connections {
			if err := conn.Close(); err != nil {
				t.Errorf("вернуть физическое соединение: %v", err)
			}
		}
	})
	for i := 0; i < 3; i++ {
		conn, err := s.sqlDB.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
		for pragma, expected := range map[string]int{"foreign_keys": 1, "busy_timeout": 10, "synchronous": 2} {
			var actual int
			if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&actual); err != nil || actual != expected {
				t.Fatalf("соединение %d, %s = %d, нужно %d, ошибка %v", i, pragma, actual, expected, err)
			}
		}
		var mode, version string
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
			t.Fatalf("соединение %d: journal_mode = %q, %v", i, mode, err)
		}
		if err := conn.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&version); err != nil || version != diagnostics.SQLiteVersion {
			t.Fatalf("соединение %d: версия SQLite = %q, %v", i, version, err)
		}
		rows, err := conn.QueryContext(ctx, "PRAGMA compile_options")
		if err != nil {
			t.Fatal(err)
		}
		var options []string
		for rows.Next() {
			var option string
			if err := rows.Scan(&option); err != nil {
				if closeErr := rows.Close(); closeErr != nil {
					t.Error(closeErr)
				}
				t.Fatal(err)
			}
			options = append(options, option)
		}
		rowErr := rows.Err()
		closeErr := rows.Close()
		if rowErr != nil || closeErr != nil || !reflect.DeepEqual(options, diagnostics.CompileOptions) {
			t.Fatalf("соединение %d использует другие compile options: %v, %v", i, rowErr, closeErr)
		}
	}
	if got := s.sqlDB.Stats().InUse; got != len(connections) {
		t.Fatalf("нужны одновременно занятые разные соединения: %d", got)
	}
}

func TestFailedMigrationRollsBackSchemaAndVersionThroughProductionRunner(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	initial, err := fs.ReadFile(sqlitemigrations.Files, "00001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{
		"00001_initial.sql": &fstest.MapFile{Data: initial},
		"00002_failure.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE migration_partial (id INTEGER PRIMARY KEY);\nINSERT INTO missing_migration_target(id) VALUES (1);\n")},
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, s.sqlDB, files, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	original := s.provider
	s.provider = provider
	err = s.Migrate(ctx)
	s.provider = original
	if err == nil {
		t.Fatal("неисправная миграция успешно применена")
	}
	var count int
	if err := s.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE name = ?", "migration_partial").Scan(&count); err != nil || count != 0 {
		t.Fatalf("неисправная миграция оставила частичную схему: %d, %v", count, err)
	}
	diagnostics, err := s.Diagnostics(ctx)
	if err != nil || diagnostics.SchemaVersion != 1 {
		t.Fatalf("неудачная миграция изменила version history: %+v, %v", diagnostics, err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("ошибка миграции повредила исходный runner/pool: %v", err)
	}
	if s.sqlDB.Stats().InUse != 0 {
		t.Fatal("ошибка миграции оставила соединения")
	}
}

func TestCorruptDatabaseOpenMapsErrorAndReleasesLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "corrupt.sqlite")
	if err := os.WriteFile(dbPath, []byte("синтетическое повреждённое содержимое базы"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s, err := Open(context.Background(), dbPath, testOptions())
		if err == nil {
			if closeErr := s.Close(); closeErr != nil {
				t.Error(closeErr)
			}
			t.Fatal("повреждённая база открылась")
		}
		requireDomainError(t, err, core.ErrCorrupt)
	}
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), dbPath, testOptions())
	if err != nil {
		t.Fatalf("ошибка открытия оставила lifetime lock: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreLockCanonicalPathCloseAndReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.sqlite")
	s, err := Open(ctx, dbPath, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("закрыть первоначальный пул: %v", err)
		}
	})
	alias := filepath.Join(dir, "alias.sqlite")
	if err := os.Symlink(dbPath, alias); err != nil {
		t.Fatal(err)
	}
	other, err := Open(ctx, alias, testOptions())
	if err == nil {
		if closeErr := other.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("два владельца получили один канонический файл")
	}
	incarnation := s.Incarnation()
	if incarnation == "" {
		t.Fatal("отсутствует incarnation владельца")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close не идемпотентен: %v", err)
	}
	if err := s.sqlDB.PingContext(ctx); err == nil {
		t.Fatal("закрытый пул принимает запросы")
	}
	reopened, err := Open(ctx, alias, testOptions())
	if err != nil {
		t.Fatalf("ошибка открытия по alias оставила блокировку: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("закрыть повторный пул: %v", err)
		}
	})
	if reopened.Incarnation() == incarnation || reopened.Incarnation() == "" {
		t.Fatal("новое открытие не сменило владельца")
	}
}

func TestInvalidOptionsAndCanceledOpenLeaveNoDatabase(t *testing.T) {
	for _, change := range []func(*Options){
		func(o *Options) { o.MaxOpenConns = 0 }, func(o *Options) { o.MaxIdleConns = 5 },
		func(o *Options) { o.ConnMaxLifetime = 0 }, func(o *Options) { o.ConnMaxIdleTime = 0 },
		func(o *Options) { o.BusyTimeout = time.Nanosecond }, func(o *Options) { o.RetryAttempts = 0 },
		func(o *Options) { o.RetryTimeout = 0 }, func(o *Options) { o.RetryDelay = 0 },
	} {
		options := testOptions()
		change(&options)
		p := filepath.Join(t.TempDir(), "invalid.sqlite")
		if s, err := Open(context.Background(), p, options); err == nil {
			if closeErr := s.Close(); closeErr != nil {
				t.Error(closeErr)
			}
			t.Fatal("некорректный бюджет принят")
		}
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("невалидное открытие создало файл: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := filepath.Join(t.TempDir(), "canceled.sqlite")
	if _, err := Open(ctx, p, testOptions()); !errors.Is(err, context.Canceled) {
		t.Fatalf("отмена открытия потеряна: %v", err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("отменённое открытие создало файл: %v", err)
	}
}
