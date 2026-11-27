package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// captureFormatFiveSQLRows дополняет прежний снимок всеми колонками таблиц чатов формата5.
// db содержит только временную БД текущего формата; t останавливает тест при ошибке чтения
// или закрытия курсора. Фиксированный перечень таблиц не принимает внешние SQL-идентификаторы.
func captureFormatFiveSQLRows(t *testing.T, db *sql.DB) preservedSQLRows {
	t.Helper()
	// snapshot сохраняет прежние таблицы, чтобы проверка чатов не потеряла историю и удержания.
	snapshot := captureSQLRows(t, db)
	// table выбирает одну из двух таблиц текущего формата; первичный ID задаёт стабильный порядок.
	for _, table := range []string{"chat_accounts", "chat_sources"} {
		// rows читает все колонки, включая обе нагрузки аккаунта; err сообщает отказ SQL.
		rows, err := db.Query("SELECT * FROM " + table + " ORDER BY id COLLATE BINARY")
		if err != nil {
			t.Fatal(err)
		}
		// columns определяет число ячеек без пропуска новых сохраняемых полей.
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(errors.Join(err, rows.Close()))
		}
		for rows.Next() {
			// row хранит точные SQL-значения, targets передаёт адрес каждой ячейки в Scan.
			row := make([]any, len(columns))
			targets := make([]any, len(columns))
			// index адресует очередной столбец полной строки.
			for index := range row {
				targets[index] = &row[index]
			}
			if err = rows.Scan(targets...); err != nil {
				t.Fatal(errors.Join(err, rows.Close()))
			}
			snapshot[table] = append(snapshot[table], row)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(errors.Join(err, rows.Close()))
		}
		if err = rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return snapshot
}

// TestChatStorageFormatRejectsRealPopulatedPreviousBaseline проверяет сохранённый исходный формат4.
// t работает только с копией: схема, все строки и Goose должны сохраниться после отказа Open.
func TestChatStorageFormatRejectsRealPopulatedPreviousBaseline(t *testing.T) {
	// original и err содержат неизменяемый архив заполненного исходного формата4.
	original, err := os.ReadFile("testdata/storage_format4/state.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(original)) != "d629b54a8589fb968b3a96d0188223b69ad39cfa98b426e2b28428276986cd07" {
		t.Fatal("Не совпал зафиксированный заполненный baseline")
	}
	// initial сохраняет исходный SQL, независимо сверенный с прежним HEAD при подготовке фикстуры.
	initial, err := os.ReadFile("testdata/storage_format4/00001_initial.sql")
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(initial)) != "ed442e3b416a33ece10d0716bbcdf011e693762f1e8ec03446d65cab515b3853" {
		t.Fatal("Не совпала исходная схема baseline", err)
	}
	// path и db принадлежат только тесту; архив напрямую не открывается SQLite на запись.
	path := filepath.Join(t.TempDir(), "previous.sqlite")
	if err = os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	db := preflightTestDB(t, path)
	// marker и chatTables подтверждают физический прежний формат без новых таблиц.
	var marker, chatTables int
	if err = db.QueryRow("SELECT version FROM storage_format WHERE singleton=1").Scan(&marker); err != nil || marker != 4 {
		t.Fatal("Архив не содержит исходный marker4", marker, err)
	}
	if err = db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name IN ('chat_accounts','chat_sources')").Scan(&chatTables); err != nil || chatTables != 0 {
		t.Fatal("Архив уже содержит новые таблицы", chatTables, err)
	}
	// table перечисляет существенно заполненные строки, включая активную историю и удержания.
	for _, table := range []string{"streams", "profiles", "groups", "destinations", "sessions", "outputs", "command_receipts", "output_holds", "session_holds"} {
		// count подтверждает, что проверяется настоящая заполненная БД, а не подменённый marker.
		var count int
		if err = db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count == 0 {
			t.Fatalf("Не заполнена исходная таблица %s: %d, %v", table, count, err)
		}
	}
	// rows и schema фиксируют точные доменные данные, Goose и определения перед отказом.
	rows, schema := captureSQLRows(t, db), preflightSchema(t, db)
	// rejected должен остаться nil при отказе раньше пишущего Goose и Recover.
	rejected, err := Open(context.Background(), path, testOptions())
	if rejected != nil || !errors.Is(err, ErrIncompatibleFormat) {
		t.Fatalf("Прежний заполненный формат принят: %v, %v", rejected, err)
	}
	if !reflect.DeepEqual(rows, captureSQLRows(t, db)) || !reflect.DeepEqual(schema, preflightSchema(t, db)) {
		t.Fatal("Отказ изменил прежнюю схему, доменные строки или Goose")
	}
	// after подтверждает, что тест не изменил исходный артефакт.
	after, err := os.ReadFile("testdata/storage_format4/state.sqlite")
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("Изменён исходный архив", err)
	}
}

// TestChatStorageFreshGooseAndReopenPreserveBothPayloadScopes проверяет новый формат5 рабочим Open.
// t сохраняет все три provider, Source и полный набор; reopen не меняет конфигурации или полномочия.
func TestChatStorageFreshGooseAndReopenPreserveBothPayloadScopes(t *testing.T) {
	// fixture содержит новую файловую SQLite с рабочим runner и действительным Destination.
	fixture := newDomainFixture(t, 1)
	// ctx адресует только временный граф; marker проверяет формат, versions — число записей Goose,
	// initial — число применённых записей начальной версии.
	ctx := context.Background()
	var marker, versions, initial int
	// err сообщает об отказе чтения единственного маркера совместимости.
	if err := fixture.store.sqlDB.QueryRow("SELECT version FROM storage_format WHERE singleton=1").Scan(&marker); err != nil || marker != 5 {
		t.Fatal("Новая установка не получила формат5", marker, err)
	}
	// err сообщает об отказе проверки истории рабочего механизма Goose.
	if err := fixture.store.sqlDB.QueryRow("SELECT count(*),sum(CASE WHEN version_id=1 AND is_applied=1 THEN 1 ELSE 0 END) FROM goose_db_version").Scan(&versions, &initial); err != nil || versions != 2 || initial != 1 {
		t.Fatal("Нет единственной начальной Goose1", versions, initial, err)
	}
	// results и sources сохраняют независимые эталоны всех поддерживаемых наборов после COMMIT.
	var results []core.ChatCredentialResult
	var sources []core.ChatSource
	// provider выбирает полный набор, доступный без сетевой проверки в структурном домене.
	for _, provider := range []core.ChatProvider{core.ChatYouTube, core.ChatTwitch, core.ChatVKVideoLive} {
		// result содержит сохраняемые metadata и полный credential-набор текущей платформы.
		result := chatAcceptanceAccount(t, fixture.service, provider)
		results = append(results, result)
		sources = append(sources, chatAcceptanceSource(t, fixture.service, fixture.destinations[0].ID, result.Account))
	}
	// rows сохраняет весь граф до повторного запуска рабочего runner.
	rows := captureFormatFiveSQLRows(t, fixture.store.sqlDB)
	// err сообщает об отказе повторного применения уже завершённой начальной схемы.
	if err := fixture.store.Migrate(ctx); err != nil || !reflect.DeepEqual(rows, captureFormatFiveSQLRows(t, fixture.store.sqlDB)) {
		t.Fatal("Повтор runner изменил заполненные данные", err)
	}
	// filename читает фактический путь основной SQLite без предположения о расположении t.TempDir.
	// sequence и name принимают обязательные служебные поля PRAGMA для той же основной БД.
	var sequence int
	var name, filename string
	// err сообщает об отказе получения пути основной базы для повторного открытия.
	if err := fixture.store.sqlDB.QueryRow("PRAGMA database_list").Scan(&sequence, &name, &filename); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе закрытия первого владельца файла перед повторным открытием.
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	// reopened и err содержат нового владельца того же файла, прошедшего штатный preflight и Recover.
	reopened, err := Open(ctx, filename, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	// Callback закрывает только принадлежащий этому тесту повторный пул.
	t.Cleanup(func() {
		// closeErr сообщает об отказе освобождения нового владельца файла.
		if closeErr := reopened.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if !reflect.DeepEqual(rows, captureFormatFiveSQLRows(t, reopened.sqlDB)) {
		t.Fatal("Reopen изменил строки заполненного формата5")
	}
	// svc читает обе области через публичные сервисные методы нового владельца.
	svc, err := core.NewService(reopened, reopened.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// index и result сопоставляют результаты с соответствующими Source после reopen.
	for index, result := range results {
		// stored и source читают независимые области без скрытых defaults или обновления expiry;
		// readErr передаёт отказ соответствующего сервисного чтения.
		stored, readErr := svc.GetChatCredentialResult(ctx, result.Account.ID)
		if readErr != nil || !stored.Matches(result) {
			t.Fatalf("Reopen изменил credentials/config: %v", readErr)
		}
		source, readErr := svc.GetChatSource(ctx, sources[index].ID)
		if readErr != nil || !reflect.DeepEqual(source, sources[index]) {
			t.Fatalf("Reopen изменил Source: %v", readErr)
		}
	}
}
