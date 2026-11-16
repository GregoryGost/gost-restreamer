package storage

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	sqlitemigrations "github.com/GregoryGost/gost-restreamer/migrations/sqlite"
	"github.com/pressly/goose/v3"
)

// TestCurrentMigrationFSContainsOnlyBaseline проверяет корень FS, который реально получает Goose.
// t требует одну текущую SQL-миграцию и отсутствие других объектов в активном наборе.
func TestCurrentMigrationFSContainsOnlyBaseline(t *testing.T) {
	// entries содержит непосредственные элементы корня миграций; err сообщает об отказе чтения.
	entries, err := fs.ReadDir(sqlitemigrations.Files, ".")
	if err != nil || len(entries) != 1 || entries[0].Name() != "00001_initial.sql" || entries[0].IsDir() {
		t.Fatalf("Активные миграции: %v, %v", entries, err)
	}
}

// preflightTestDB открывает только файл path, созданный текущим тестом t.
// Возвращаемый CGO-пул нужен для контролируемого изменения метаданных вне Store и закрывается при очистке.
func preflightTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	// db и err содержат пул собственной временной SQLite и результат его создания.
	db, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on&_busy_timeout=1000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// err сообщает об отказе закрытия пула собственной временной базы.
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

// preflightSchema читает точные определения пользовательских объектов db для сравнения отказов.
// t получает ошибки чтения; служебные WAL/SHM и lock-файлы этим снимком не охватываются.
func preflightSchema(t *testing.T, db *sql.DB) []string {
	t.Helper()
	// rows и err адресуют только определения схемы, не содержимое профилей.
	rows, err := db.Query("SELECT type||':'||name||':'||coalesce(sql,'') FROM sqlite_schema ORDER BY type,name")
	if err != nil {
		t.Fatal(err)
	}
	// result сохраняет все определения в стабильном порядке.
	var result []string
	for rows.Next() {
		// definition содержит точное определение одного объекта.
		var definition string
		if err = rows.Scan(&definition); err != nil {
			t.Fatal(errors.Join(err, rows.Close()))
		}
		result = append(result, definition)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return result
}

// TestCurrentPreflightRejectsMetadataDisagreementWithoutRepair проверяет отказ до Recover и записи Goose.
// t создаёт новый полный граф, затем меняет только один класс метаданных; схема, история миграций и содержимое записей
// должны остаться точными после каждого отказа, включая незавершённую активную Session.
func TestCurrentPreflightRejectsMetadataDisagreementWithoutRepair(t *testing.T) {
	// name называет нарушенный контракт; statement меняет только собственную временную БД.
	for name, statement := range map[string]string{
		"old_marker":                 "UPDATE storage_format SET version=3",
		"unknown_marker":             "UPDATE storage_format SET version=99",
		"missing_marker":             "DELETE FROM storage_format",
		"extra_history":              "INSERT INTO goose_db_version(version_id,is_applied) VALUES(2,1)",
		"unapplied_baseline":         "UPDATE goose_db_version SET is_applied=0 WHERE version_id=1",
		"unexpected_initial_version": "UPDATE goose_db_version SET version_id=3 WHERE version_id=1",
		"missing_zero":               "DELETE FROM goose_db_version WHERE version_id=0",
		"duplicate_baseline":         "INSERT INTO goose_db_version(version_id,is_applied) VALUES(1,1)",
		"foreign_table":              "CREATE TABLE foreign_data(id INTEGER PRIMARY KEY, payload BLOB)",
		"foreign_view":               "CREATE VIEW foreign_view AS SELECT id FROM streams",
		"foreign_trigger":            "CREATE TRIGGER foreign_trigger AFTER INSERT ON streams BEGIN SELECT 1; END",
	} {
		// Подтест создаёт отдельный текущий граф и сверяет снимки до и после отказа Open; t сообщает нарушения.
		t.Run(name, func(t *testing.T) {
			// store содержит текущий полный граф с активным запуском; path адресует только его.
			store, _, _, path := openCurrentEncodingState(t)
			// err сообщает об отказе закрытия исходного владельца до контролируемой подмены схемы.
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			// db обслуживает контролируемое повреждение закрытого текущего графа.
			db := preflightTestDB(t, path)
			// err сообщает об отказе изменения метаданных отрицательного сценария.
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			// before и schema сохраняют все прикладные строки, историю и определения после подмены.
			before, schema := captureSQLRows(t, db), preflightSchema(t, db)
			// rejected и err подтверждают отказ до восстановления активного запуска.
			rejected, err := Open(context.Background(), path, testOptions())
			if rejected != nil || !errors.Is(err, ErrIncompatibleFormat) {
				t.Fatalf("Несовместимые метаданные приняты: %v, %v", rejected, err)
			}
			if !reflect.DeepEqual(before, captureSQLRows(t, db)) || !reflect.DeepEqual(schema, preflightSchema(t, db)) {
				t.Fatal("Отказ переписал схему, историю либо данные")
			}
		})
	}
}

// TestPreflightRejectsForeignPartialAndGooseZero проверяет, что отсутствие marker не считается пустой схемой.
// t требует отказ и сохранность всех определений без автоматического создания текущих таблиц.
func TestPreflightRejectsForeignPartialAndGooseZero(t *testing.T) {
	// name обозначает класс схемы, statement создаёт только его метаданные в собственной пустой БД.
	for name, statement := range map[string]string{
		"foreign":          "CREATE TABLE unrelated(id INTEGER PRIMARY KEY, payload BLOB); INSERT INTO unrelated VALUES(1,x'0102')",
		"sqlite_like_name": "CREATE TABLE sqlitex_foreign(id INTEGER PRIMARY KEY)",
		"view":             "CREATE VIEW unrelated AS SELECT 1",
		"marker_only":      "CREATE TABLE storage_format(singleton INTEGER PRIMARY KEY,version INTEGER); INSERT INTO storage_format VALUES(1,4)",
		"goose_zero":       "CREATE TABLE goose_db_version(id INTEGER PRIMARY KEY,version_id INTEGER,is_applied INTEGER); INSERT INTO goose_db_version VALUES(1,0,1)",
	} {
		// Подтест изолирует частичную или постороннюю схему; t получает отказ или нежелательную запись.
		t.Run(name, func(t *testing.T) {
			// path задаёт новый файл собственной схемы, db создаёт её без Store.
			path := filepath.Join(t.TempDir(), "partial.sqlite")
			db := preflightTestDB(t, path)
			// err сообщает о неудаче подготовки отрицательного сценария.
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			// before сохраняет определения до попытки первичной инициализации.
			before := preflightSchema(t, db)
			// rejected и err фиксируют отказ, не превращающий частичную схему в новую установку.
			rejected, err := Open(context.Background(), path, testOptions())
			if rejected != nil || !errors.Is(err, ErrIncompatibleFormat) || !reflect.DeepEqual(before, preflightSchema(t, db)) {
				t.Fatalf("Частичная схема принята или исправлена: %v, %v", rejected, err)
			}
		})
	}
}

// TestPopulatedOldDatabaseIsRejectedWithoutMigration проверяет настоящую прежнюю БД только на копии.
// t требует неизменности payload, истории и схемы копии после отказа; исходная фикстура только читается.
func TestPopulatedOldDatabaseIsRejectedWithoutMigration(t *testing.T) {
	// original и err содержат байты архивной фикстуры, не открываемой SQLite на запись.
	original, err := os.ReadFile("testdata/encoding_legacy_v1/state.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	// path адресует единственную изменяемую копию в каталоге текущего теста.
	path := filepath.Join(t.TempDir(), "old.sqlite")
	if err = os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	// db позволяет снять логические данные копии до и после отказа Open.
	db := preflightTestDB(t, path)
	// before и schema фиксируют весь старый граф и определения без запуска migration2.
	before, schema := captureSQLRows(t, db), preflightSchema(t, db)
	// rejected содержит результат намеренно отклоняемого открытия.
	rejected, err := Open(context.Background(), path, testOptions())
	if rejected != nil || !errors.Is(err, ErrIncompatibleFormat) {
		t.Fatalf("Прежняя БД принята: %v, %v", rejected, err)
	}
	if !reflect.DeepEqual(before, captureSQLRows(t, db)) || !reflect.DeepEqual(schema, preflightSchema(t, db)) {
		t.Fatal("Отказ изменил прежнюю схему или данные")
	}
	// after повторно читает исходный архив, исключая неявное изменение фикстуры самим тестом.
	after, err := os.ReadFile("testdata/encoding_legacy_v1/state.sqlite")
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("Изменилась исходная фикстура", err)
	}
}

// TestFailedInitialBaselineLeavesOnlyGooseZeroAndCannotResume проверяет реальную границу Goose.
// t запускает ошибочный начальный SQL версии 1 в его обычной транзакции: маркер, прикладная схема и запись Goose 1
// должны откатиться вместе; отдельно созданный Goose0 остаётся причиной явного отказа следующего Open.
func TestFailedInitialBaselineLeavesOnlyGooseZeroAndCannotResume(t *testing.T) {
	// path и db принадлежат только неудачному первичному запуску текущего теста.
	path := filepath.Join(t.TempDir(), "failed.sqlite")
	db := preflightTestDB(t, path)
	// baseline содержит действующую SQL-схему до добавления контролируемого отказа; err сообщает об отказе чтения.
	baseline, err := fs.ReadFile(sqlitemigrations.Files, "00001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	// files сохраняет транзакционный начальный SQL и добавляет ошибку после его последнего объекта.
	files := fstest.MapFS{"00001_initial.sql": &fstest.MapFile{Data: append(baseline, []byte("\nINSERT INTO nonexistent_table VALUES(1);\n")...)}}
	// provider использует тот же закреплённый исполнитель Goose и CGO-пул, что и рабочий Store.
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, files, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Up(context.Background()); err == nil {
		t.Fatal("Ошибочный baseline успешно применён")
	}
	// count проверяет, что ни marker, ни часть прикладной схемы не вышли из откатившейся транзакции.
	var count int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='table' AND name NOT GLOB 'sqlite_*' AND name!='goose_db_version'").Scan(&count); err != nil || count != 0 {
		t.Fatal("Часть baseline не откатилась", count, err)
	}
	// version и applied подтверждают только отдельно созданную запись нулевой версии.
	var version, applied int
	if err = db.QueryRow("SELECT count(*),min(version_id),min(is_applied) FROM goose_db_version").Scan(&count, &version, &applied); err != nil || count != 1 || version != 0 || applied != 1 {
		t.Fatal("Неожиданный остаток истории Goose", version, applied, err)
	}
	// before фиксирует определения прерванной инициализации до явного отказа повторного Open.
	before := preflightSchema(t, db)
	// rejected подтверждает отказ от автоматического продолжения начальной схемы версии 1.
	rejected, err := Open(context.Background(), path, testOptions())
	if rejected != nil || !errors.Is(err, ErrIncompatibleFormat) || !reflect.DeepEqual(before, preflightSchema(t, db)) {
		t.Fatal("Остаток Goose0 автоматически продолжен или изменён", rejected, err)
	}
}

// TestCanonicalUniqueIncludesFailedVariants проверяет сильное SQL-ограничение, а не read-before-write.
// t переводит собственный вариант в FAILED и пытается вставить дубль с иным ID и путём напрямую.
func TestCanonicalUniqueIncludesFailedVariants(t *testing.T) {
	// fixture содержит сохранённый пригодный вариант с полной канонической идентичностью.
	fixture := newDomainFixture(t, 1)
	// db использует пул Store; err сообщает о контролируемой смене состояния SQL-проекции.
	db := fixture.store.sqlDB
	if _, err := db.Exec("UPDATE fallback_variants SET state='FAILED' WHERE id=?", fixture.variant.ID); err != nil {
		t.Fatal(err)
	}
	// err содержит результат прямой вставки дублированной полной идентичности, без проверки repository.
	_, err := db.Exec(`INSERT INTO fallback_variants
		(id,revision,created_at,updated_at,payload,asset_id,generation_id,incarnation,source_hash,profile_key,canonical_profile,generator_version,managed_path,size_bytes,state)
		SELECT 'duplicate-variant',revision,created_at,updated_at,payload,asset_id,generation_id,incarnation,source_hash,profile_key,canonical_profile,generator_version,managed_path||'.duplicate',size_bytes,'FAILED'
		FROM fallback_variants WHERE id=?`, fixture.variant.ID)
	if !errors.Is(translate(err), core.ErrConflict) {
		t.Fatal("FAILED-дубль не остановлен UNIQUE", err)
	}
}
