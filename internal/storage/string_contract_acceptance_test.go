package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	sqlitemigrations "github.com/GregoryGost/gost-restreamer/migrations/sqlite"
	"github.com/pressly/goose/v3"
)

// preservedSQLRows фиксирует все столбцы строк для проверки атомарности и ресурсов.
type preservedSQLRows map[string][][]any

// migrationBoundaryFixture копирует закреплённую заполненную БД в каталог t.
// Возвращаемые db и provider выполняют настоящую Goose-миграцию без Recover,
// чтобы изменения восстановления не маскировали нарушение миграции.
func migrationBoundaryFixture(t *testing.T) (*sql.DB, *goose.Provider) {
	t.Helper()
	// data содержит неизменённую прежнюю БД с историей, holds и командами; err сообщает об ошибке чтения.
	data, err := os.ReadFile("testdata/encoding_legacy_v1/state.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	// digest подтверждает происхождение независимой бинарной фикстуры.
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != "8bebed8276ae80703a90354ac64ffa7d8ed0e5acc87b6bb516240531df0277c0" {
		t.Fatal("Изменилось содержимое закреплённой прежней БД")
	}
	// path содержит путь собственной изменяемой копии, не рабочую БД.
	path := filepath.Join(t.TempDir(), "history.sqlite")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	// db использует тот же CGO-драйвер и включённые FK на каждом соединении.
	db, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on&_busy_timeout=1000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает только пул текущей временной копии; closeErr сохраняет отказ освобождения.
		if closeErr := db.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	// provider применяет точную production-миграцию через настоящий транзакционный runner Goose.
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, sqlitemigrations.Files,
		goose.WithDisableGlobalRegistry(true), goose.WithGoMigrations(desiredProfilesMigration()))
	if err != nil {
		t.Fatal(err)
	}
	return db, provider
}

// captureSQLRows читает полный состав фиксированных таблиц db в стабильном порядке.
// t завершает тест при любой ошибке чтения или закрытия курсора.
func captureSQLRows(t *testing.T, db *sql.DB) preservedSQLRows {
	t.Helper()
	// snapshot хранит полные строки, включая SQL-проекции и точные BLOB.
	snapshot := preservedSQLRows{}
	// table выбирается только из фиксированного перечня таблиц прежнего контракта.
	for _, table := range []string{"storage_format", "goose_db_version", "streams", "profiles", "groups", "destinations", "fallback_assets", "fallback_generations", "fallback_variants", "sessions", "outputs", "session_holds", "output_holds", "command_receipts", "events", "owned_paths", "cleanup_intents"} {
		// rows предоставляет все столбцы таблицы без потери двоичных данных; err сообщает об ошибке SQL.
		rows, err := db.Query("SELECT * FROM " + table + " ORDER BY 1")
		if err != nil {
			t.Fatal(err)
		}
		// columns задаёт размер списка Scan, не вводя предположений о числе столбцов.
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			// row хранит одну полную строку; targets предоставляет адреса ячеек для database/sql.
			row := make([]any, len(columns))
			targets := make([]any, len(columns))
			// index адресует каждый столбец текущей строки.
			for index := range row {
				targets[index] = &row[index]
			}
			if err = rows.Scan(targets...); err != nil {
				t.Fatal(err)
			}
			snapshot[table] = append(snapshot[table], row)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err = rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return snapshot
}

// mutateDesiredCopies заменяет только самостоятельные desired-копии oldProfile
// в JSON-дереве value; прежнее наблюдение файла согласуется с прежним desired,
// чтобы фикстура удовлетворяла ранее обязательному CompatibleFormat. replacement содержит
// согласованный прежний профиль, canonical — его байты, key — SHA-256 этих байтов.
// Результат сообщает число действительно изменённых desired-копий.
func mutateDesiredCopies(value any, oldProfile, replacement map[string]any, canonical []byte, key string) int {
	// changed считает самостоятельные профили в данном поддереве.
	changed := 0
	// object представляет объект JSON, а ok подтверждает, что value содержит именованные поля.
	if object, ok := value.(map[string]any); ok {
		if reflect.DeepEqual(object["Profile"], oldProfile) {
			object["Profile"] = replacement
			if reflect.DeepEqual(object["VerifiedFormat"], oldProfile["Format"]) {
				object["VerifiedFormat"] = replacement["Format"]
			}
			// present отличает контейнер с канонической идентичностью от простого именованного профиля.
			if _, present := object["ProfileKey"]; present {
				object["ProfileKey"], object["CanonicalProfile"] = key, base64.StdEncoding.EncodeToString(canonical)
			}
			changed++
		}
		// name и child выбирают каждое поле, кроме уже заменённой самостоятельной копии.
		for name, child := range object {
			if name != "Profile" || !reflect.DeepEqual(child, replacement) {
				changed += mutateDesiredCopies(child, oldProfile, replacement, canonical, key)
			}
		}
	}
	// list представляет список вложенных элементов JSON, а ok подтверждает этот вид value.
	if list, ok := value.([]any); ok {
		// child выбирает очередной исторический элемент списка.
		for _, child := range list {
			changed += mutateDesiredCopies(child, oldProfile, replacement, canonical, key)
		}
	}
	return changed
}

// migrationJSON разбирает data без float64; t сообщает об ошибке фикстуры.
func migrationJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()
	// value принимает точное JSON-дерево; decoder сохраняет все числовые лексемы.
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	// err сообщает об ошибке чтения единственного JSON-объекта фикстуры.
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

// poisonFrozenDesired вводит допустимый по прежнему контракту невалидный UTF-8
// range во все копии одного профиля db. Ключи и SQL-проекции обновляются точно,
// поэтому миграция должна исправить настройку, а не отвергнуть повреждение идентичности.
// Возвращаемые oldCanonical позволяют независимо сверить итоговую замену limited.
func poisonFrozenDesired(t *testing.T, db *sql.DB) []byte {
	t.Helper()
	// original содержит канонические байты из старой SQL-проекции.
	var original []byte
	// err сообщает об ошибке чтения исходного канонического профиля.
	if err := db.QueryRow("SELECT canonical_profile FROM fallback_variants LIMIT 1").Scan(&original); err != nil {
		t.Fatal(err)
	}
	// poisoned сохраняет прежний порядок полей canonical, меняя только байтовый range.
	poisoned := bytes.Replace(original, []byte(`"Range":"bGltaXRlZA=="`), []byte(`"Range":"/w=="`), 1)
	if bytes.Equal(poisoned, original) {
		t.Fatal("В независимой фикстуре нет исходного limited")
	}
	// digest и key задают прежнюю корректную идентичность изменённого профиля.
	digest := sha256.Sum256(poisoned)
	key := hex.EncodeToString(digest[:])
	// oldProfile и replacement представляют исходную и изменённую desired-структуру.
	oldProfile, replacement := migrationJSON(t, original), migrationJSON(t, poisoned)
	// before хранит строки до мутации; table выбирает таблицы с самостоятельными desired-копиями.
	before := captureSQLRows(t, db)
	for _, table := range []string{"profiles", "fallback_generations", "fallback_variants", "sessions", "outputs"} {
		// row содержит полную прежнюю строку; payload в каждой такой таблице занимает столбец 4.
		for _, row := range before[table] {
			// object содержит точный payload, count считает изменённые самостоятельные профили.
			object := migrationJSON(t, row[4].([]byte))
			count := mutateDesiredCopies(object, oldProfile, replacement, poisoned, key)
			if count == 0 {
				continue
			}
			// payload сериализует согласованную старую оболочку; err сообщает об ошибке JSON/SQL.
			payload, err := json.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("UPDATE "+table+" SET payload=? WHERE id=?", payload, row[0]); err != nil {
				t.Fatal(err)
			}
		}
	}
	// err сообщает об ошибке синхронизации SQL-ключей с их payload и canonical.
	if _, err := db.Exec("UPDATE fallback_variants SET canonical_profile=?, profile_key=?", poisoned, key); err != nil {
		t.Fatal(err)
	}
	return original
}

// TestDesiredMigrationPreservesGraphAndRollsBackLateFailure проверяет полный
// старый граф на реальной SQLite: поздний отказ откатывает данные, схему, marker
// и Goose; повтор после устранения отказа меняет только разрешённые поля.
// t сравнивает полные строки команд, ресурсов, holds и SQL-проекций.
func TestDesiredMigrationPreservesGraphAndRollsBackLateFailure(t *testing.T) {
	// db и provider работают только с собственной копией закреплённой старой БД.
	db, provider := migrationBoundaryFixture(t)
	// canonical задаёт независимый ожидаемый результат range→limited.
	canonical := poisonFrozenDesired(t, db)
	// err сообщает о невозможности установить детерминированный поздний отказ миграции.
	if _, err := db.Exec(`CREATE TRIGGER reject_transition BEFORE UPDATE OF version ON storage_format BEGIN SELECT RAISE(ABORT,'synthetic transition rejection'); END`); err != nil {
		t.Fatal(err)
	}
	// before содержит все байты до попытки, schemaBefore фиксирует DDL.
	before := captureSQLRows(t, db)
	var schemaBefore string
	// err сообщает о невозможности снять исходную схему до контролируемого отказа.
	if err := db.QueryRow("SELECT group_concat(sql,';') FROM (SELECT sql FROM sqlite_master ORDER BY name)").Scan(&schemaBefore); err != nil {
		t.Fatal(err)
	}
	// err сохраняет первый отказ миграции, вызванный установленным SQL-триггером.
	if _, err := provider.Up(context.Background()); err == nil {
		t.Fatal("Поздний отказ не остановил миграцию")
	}
	// after содержит полные строки сразу после неуспешного перехода.
	if after := captureSQLRows(t, db); !reflect.DeepEqual(after, before) {
		t.Fatal("После отказа изменились данные, marker или история Goose")
	}
	// schemaAfter проверяет транзакционный откат перестройки fallback_variants.
	var schemaAfter string
	// err сообщает об ошибке повторного чтения DDL после отката.
	if err := db.QueryRow("SELECT group_concat(sql,';') FROM (SELECT sql FROM sqlite_master ORDER BY name)").Scan(&schemaAfter); err != nil || schemaAfter != schemaBefore {
		t.Fatal("После отказа изменена схема", err)
	}
	// err сообщает о невозможности убрать только собственный синтетический триггер.
	if _, err := db.Exec("DROP TRIGGER reject_transition"); err != nil {
		t.Fatal(err)
	}
	// results подтверждает выполнение именно одного перехода версии 2; err сохраняет отказ повторной попытки.
	results, err := provider.Up(context.Background())
	if err != nil || len(results) != 1 {
		t.Fatalf("Повтор после полного отката: %d миграций, %v", len(results), err)
	}
	// after хранит все строки после успешной замены.
	after := captureSQLRows(t, db)
	// table выбирает данные, которые миграция обязана оставить побайтно неизменными.
	for _, table := range []string{"streams", "groups", "destinations", "fallback_assets", "session_holds", "output_holds", "command_receipts", "events", "owned_paths", "cleanup_intents"} {
		if !reflect.DeepEqual(before[table], after[table]) {
			t.Fatalf("Миграция изменила историю или ресурс %s", table)
		}
	}
	// table выбирает таблицы, где разрешено изменение desired-payload.
	for _, table := range []string{"profiles", "fallback_generations", "fallback_variants", "sessions", "outputs"} {
		if len(before[table]) != len(after[table]) {
			t.Fatalf("Потеряны строки %s", table)
		}
		// index сопоставляет одинаковые ID до и после миграции; oldRow и newRow содержат их столбцы.
		for index, oldRow := range before[table] {
			newRow := after[table][index]
			if !reflect.DeepEqual(oldRow[:4], newRow[:4]) {
				t.Fatalf("Изменены ID/revision/timestamps %s", table)
			}
		}
	}
	// variant содержит все поля готового ранее файла без потери измеренного формата и владения.
	variant := migrationJSON(t, after["fallback_variants"][0][4].([]byte))["Payload"].(map[string]any)
	if variant["State"] != "FAILED" || !bytes.Equal(after["fallback_variants"][0][10].([]byte), canonical) {
		t.Fatal("Заменённый вариант не аннулирован или canonical не пересчитан", variant)
	}
	// oldVariant содержит независимое исходное наблюдение файла для точного сравнения.
	oldVariant := migrationJSON(t, before["fallback_variants"][0][4].([]byte))["Payload"].(map[string]any)
	// field выбирает неизменяемые факты и ресурсы старого файла.
	for _, field := range []string{"VerifiedFormat", "ManagedPath", "ContentHash", "SizeBytes", "AttemptToken", "Incarnation", "GeneratorVersion", "SourceHash", "GenerationID"} {
		if !reflect.DeepEqual(variant[field], oldVariant[field]) {
			t.Fatalf("Переписан факт старого файла %s", field)
		}
	}
	// row содержит исторический output; artifact обязан сохранить явное аннулирование доказательства.
	for _, row := range after["outputs"] {
		// artifact содержит manifest самостоятельной исторической копии профиля.
		artifact := migrationJSON(t, row[4].([]byte))["Payload"].(map[string]any)["Snapshot"].(map[string]any)["Artifact"].(map[string]any)
		if artifact["RequiresRebuild"] != true {
			t.Fatal("Исторический manifest не отмечен для пересборки", artifact)
		}
	}
	// version и marker фиксируют две согласованные версии одного атомарного перехода.
	version, err := provider.GetDBVersion(context.Background())
	var marker int
	if err != nil || version != 2 {
		t.Fatal("Не сохранена версия Goose", version, err)
	}
	if err = db.QueryRow("SELECT version FROM storage_format").Scan(&marker); err != nil || marker != 3 {
		t.Fatal("Не сохранён marker 3", marker, err)
	}
	// foreignKeys и violations проверяют режим соединения и целостность после переноса holds.
	var foreignKeys, violations int
	if err = db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatal("Миграция отключила FK", foreignKeys, err)
	}
	if err = db.QueryRow("SELECT count(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil || violations != 0 {
		t.Fatal("Нарушены FK полного графа", violations, err)
	}
	results, err = provider.Up(context.Background())
	if err != nil || len(results) != 0 || !reflect.DeepEqual(captureSQLRows(t, db), after) {
		t.Fatal("Успешная миграция повторно преобразовала данные", err)
	}
}

// TestDesiredMigrationDoesNotRepairOldCorruption проверяет, что повреждённый
// прежний codec, ключ или число не превращаются в новую допустимую настройку.
// t фиксирует полный откат, включая неизменность Goose и marker.
func TestDesiredMigrationDoesNotRepairOldCorruption(t *testing.T) {
	// corruption задаёт независимые нарушения ранее действовавшего контракта.
	for _, corruption := range []string{"codec", "projection", "number"} {
		// callback проверяет один отказ на собственной временной SQLite.
		t.Run(corruption, func(t *testing.T) {
			// db и provider относятся только к текущему случаю старого повреждения.
			db, provider := migrationBoundaryFixture(t)
			poisonFrozenDesired(t, db)
			// query выбирает детерминированное повреждение SQL-проекции или старого payload.
			query := "UPDATE fallback_variants SET profile_key='invalid-key'"
			if corruption == "codec" {
				query = `UPDATE profiles SET payload=replace(CAST(payload AS TEXT),'"Codec":"av1"','"Codec":"hevc"')`
			}
			if corruption == "number" {
				query = `UPDATE profiles SET payload=replace(CAST(payload AS TEXT),'"GOPFrames":120','"GOPFrames":0')`
			}
			// err сообщает о невозможности установить проверяемое повреждение.
			if _, err := db.Exec(query); err != nil {
				t.Fatal(err)
			}
			// before фиксирует точные испорченные входные данные, которые нельзя частично исправить.
			before := captureSQLRows(t, db)
			// err должен сохранить отказ прежнего повреждения без частичного обновления.
			if _, err := provider.Up(context.Background()); err == nil {
				t.Fatal("Миграция приняла прежнее повреждение", corruption)
			}
			if !reflect.DeepEqual(captureSQLRows(t, db), before) {
				t.Fatal("Отказ оставил частичный переход", corruption)
			}
		})
	}
}

// appendLegacyCollision добавляет в опубликованное поколение db второй старый
// профиль с другим неподдержанным range и точной собственной идентичностью.
// t сообщает об ошибке фикстуры; возвращаются ID исходного и добавленного вариантов.
func appendLegacyCollision(t *testing.T, db *sql.DB) (string, string) {
	t.Helper()
	// oldPayload и oldCanonical содержат единственный исходный вариант до добавления соседа.
	var oldPayload, oldCanonical []byte
	// err сообщает о невозможности прочитать исходный вариант полного графа.
	if err := db.QueryRow("SELECT payload,canonical_profile FROM fallback_variants").Scan(&oldPayload, &oldCanonical); err != nil {
		t.Fatal(err)
	}
	// siblingCanonical задаёт вторую прежнюю идентичность с range, отличным от первого профиля.
	siblingCanonical := bytes.Replace(oldCanonical, []byte(`"Range":"bGltaXRlZA=="`), []byte(`"Range":"b2Jzb2xldGUtcmFuZ2U="`), 1)
	if bytes.Equal(siblingCanonical, oldCanonical) {
		siblingCanonical = bytes.Replace(oldCanonical, []byte(`"Range":"/w=="`), []byte(`"Range":"b2Jzb2xldGUtcmFuZ2U="`), 1)
	}
	if bytes.Equal(siblingCanonical, oldCanonical) {
		t.Fatal("Не найден range для второго прежнего профиля")
	}
	// oldProfile, replacement и digest фиксируют полную старую структуру и новую идентичность соседа.
	oldProfile, replacement := migrationJSON(t, oldCanonical), migrationJSON(t, siblingCanonical)
	digest := sha256.Sum256(siblingCanonical)
	// key задаёт SHA256 второго профиля без вызова нового строгого кодека.
	key := hex.EncodeToString(digest[:])
	// envelope и sibling содержат полную копию старого варианта с собственными ID и путём.
	envelope := migrationJSON(t, oldPayload)
	sibling := envelope["Payload"].(map[string]any)
	// originalID и siblingID сохраняют идентичности обоих исторических файлов.
	originalID, siblingID := sibling["ID"].(string), "collision-sibling"
	mutateDesiredCopies(sibling, oldProfile, replacement, siblingCanonical, key)
	sibling["ID"] = siblingID
	sibling["ManagedPath"] = core.GenerationDirectory(core.ID(sibling["GenerationID"].(string))) + "/" + siblingID + ".mp4"
	// ownedPayload сохраняет полную прежнюю запись зарегистрированного пути исходного файла.
	var ownedPayload []byte
	// err сообщает об отсутствии обязательной записи владения исходного варианта.
	if err := db.QueryRow("SELECT payload FROM owned_paths WHERE managed_path=?", migrationJSON(t, oldPayload)["Payload"].(map[string]any)["ManagedPath"]).Scan(&ownedPayload); err != nil {
		t.Fatal(err)
	}
	// ownedEnvelope и owned задают самостоятельную запись владения новым историческим путём.
	ownedEnvelope := migrationJSON(t, ownedPayload)
	owned := ownedEnvelope["Payload"].(map[string]any)
	// oldPath адресует исходную строку для копирования всех неизменяемых SQL-столбцов.
	oldPath := owned["ManagedPath"]
	owned["ID"], owned["ManagedPath"] = "collision-owned-file", sibling["ManagedPath"]
	// ownedBytes содержат согласованную нагрузку новой записи owned_paths; err сохраняет отказ JSON/SQL.
	ownedBytes, err := json.Marshal(ownedEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO owned_paths(id,revision,created_at,updated_at,payload,managed_path,asset_id,generation_id,kind)
SELECT ?,revision,created_at,updated_at,?,?,asset_id,generation_id,kind FROM owned_paths WHERE managed_path=?`, owned["ID"], ownedBytes, owned["ManagedPath"], oldPath); err != nil {
		t.Fatal(err)
	}
	// payload содержит новую согласованную строку старого формата.
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO fallback_variants(id,revision,created_at,updated_at,payload,asset_id,generation_id,incarnation,source_hash,profile_key,canonical_profile,generator_version,managed_path,size_bytes,state)
SELECT ?,revision,created_at,updated_at,?,asset_id,generation_id,incarnation,source_hash,?,?,generator_version,?,size_bytes,state FROM fallback_variants WHERE id=?`, siblingID, payload, key, siblingCanonical, sibling["ManagedPath"], originalID); err != nil {
		t.Fatal(err)
	}
	// generationPayload содержит опубликованное поколение обоих вариантов.
	var generationPayload []byte
	if err = db.QueryRow("SELECT payload FROM fallback_generations WHERE state='PUBLISHED'").Scan(&generationPayload); err != nil {
		t.Fatal(err)
	}
	// generationEnvelope, generation и requirements адресуют исходный резервируемый список.
	generationEnvelope := migrationJSON(t, generationPayload)
	generation := generationEnvelope["Payload"].(map[string]any)
	requirements := generation["RequiredProfiles"].([]any)
	// requiredPayload отделяет изменяемое второе требование от первого через точный JSON.
	requiredPayload, err := json.Marshal(requirements[0])
	if err != nil {
		t.Fatal(err)
	}
	// required содержит самостоятельное требование нового исторического варианта.
	required := migrationJSON(t, requiredPayload)
	mutateDesiredCopies(required, oldProfile, replacement, siblingCanonical, key)
	required["VariantID"], required["ManagedPath"] = siblingID, sibling["ManagedPath"]
	required["StagingPath"] = sibling["ManagedPath"].(string) + ".stage"
	// Изменённый сосед стоит первым: выбор обязан дойти до более позднего пригодного требования.
	generation["RequiredProfiles"] = []any{required, requirements[0]}
	payload, err = json.Marshal(generationEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE fallback_generations SET payload=? WHERE state='PUBLISHED'", payload); err != nil {
		t.Fatal(err)
	}
	// table выбирает все самостоятельные копии версии генератора для пригодного положительного контроля.
	for _, table := range []string{"fallback_generations", "fallback_variants", "outputs"} {
		if _, err = db.Exec("UPDATE "+table+" SET payload=CAST(replace(CAST(payload AS TEXT),?,?) AS BLOB)", `"GeneratorVersion":"generator-v1"`, `"GeneratorVersion":"`+core.CurrentFallbackGeneratorVersion+`"`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec("UPDATE fallback_variants SET generator_version=?", core.CurrentFallbackGeneratorVersion); err != nil {
		t.Fatal(err)
	}
	return originalID, siblingID
}

// TestMigrationCollisionsPreserveBothFilesAndRuntimeUniqueness проверяет обе
// коллизии: две изменённые копии и изменённая рядом с пригодной неизменённой.
// t проверяет сохранность ID/путей, исторические исключения и строгие обычные операции.
func TestMigrationCollisionsPreserveBothFilesAndRuntimeUniqueness(t *testing.T) {
	// bothChanged определяет, аннулируются оба требования или только добавленный сосед.
	for _, bothChanged := range []bool{false, true} {
		// callback выполняет отдельный переход полного графа для выбранного вида коллизии.
		t.Run(map[bool]string{false: "changed_unchanged", true: "changed_changed"}[bothChanged], func(t *testing.T) {
			// db и provider используют собственную копию прежней заполненной БД.
			db, provider := migrationBoundaryFixture(t)
			if bothChanged {
				poisonFrozenDesired(t, db)
			}
			// originalID и siblingID задают два сохраняемых исторических файла.
			originalID, siblingID := appendLegacyCollision(t, db)
			// before содержит исходные пути каждого файла и полный граф удержаний.
			before := captureSQLRows(t, db)
			// err сообщает об отказе миграции независимого сценария коллизии.
			if _, err := provider.Up(context.Background()); err != nil {
				t.Fatal("Коллизия не разрешена атомарной миграцией", err)
			}
			// after содержит две сохранившиеся строки после схождения canonical.
			after := captureSQLRows(t, db)
			if len(after["fallback_variants"]) != 2 || !reflect.DeepEqual(before["output_holds"], after["output_holds"]) {
				t.Fatal("Коллизия потеряла вариант или удержание")
			}
			// row выбирает каждый сохранённый файл; index сопоставляет его старый путь.
			for index, row := range after["fallback_variants"] {
				if row[0] != before["fallback_variants"][index][0] || row[12] != before["fallback_variants"][index][12] {
					t.Fatal("Коллизия переименовала ID или путь")
				}
				// expectedState отличает единственный пригодный старый файл от изменённых.
				expectedState := "FAILED"
				if row[0] == originalID && !bothChanged {
					expectedState = "READY"
				}
				if row[14] != expectedState {
					t.Fatal("Коллизия переписала пригодность файла", row[0], row[14], expectedState)
				}
			}
			if !reflect.DeepEqual(after["fallback_variants"][0][10], after["fallback_variants"][1][10]) {
				t.Fatal("Фикстура не образовала требуемую коллизию canonical")
			}
			// path извлекает путь собственной временной копии для настоящего повторного Open.
			var path string
			// err сообщает о невозможности определить путь текущей временной БД.
			if err := db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
				t.Fatal(err)
			}
			// err сохраняет отказ закрытия прежнего пула перед production Open.
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			// store выполняет production Open/Recover на уже мигрированной копии; err сохраняет отказ открытия.
			store, err := Open(context.Background(), path, testOptions())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает только Store текущего сценария; closeErr сообщает об ошибке освобождения.
				if closeErr := store.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			})
			// service читает мигрированную историю и выбирает привязки через реальные репозитории.
			service, err := core.NewService(store, store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			// generationID адресует опубликованное поколение, streamID — конфигурацию назначения.
			var generationID, streamID core.ID
			if err = store.sqlDB.QueryRow("SELECT id FROM fallback_generations WHERE state='PUBLISHED'").Scan(&generationID); err != nil {
				t.Fatal(err)
			}
			if err = store.sqlDB.QueryRow("SELECT stream_id FROM destinations LIMIT 1").Scan(&streamID); err != nil {
				t.Fatal(err)
			}
			// generation содержит оба исторических требования без потери ресурсов.
			generation, err := service.GetGeneration(context.Background(), generationID)
			if err != nil || len(generation.RequiredProfiles) != 2 {
				t.Fatal("Потерян резервируемый список", generation, err)
			}
			if generation.RequiredProfiles[0].VariantID != core.ID(siblingID) || !generation.RequiredProfiles[0].RequiresRebuild || generation.RequiredProfiles[1].VariantID != core.ID(originalID) || generation.RequiredProfiles[1].RequiresRebuild != bothChanged {
				t.Fatal("Неверные миграционные флаги требований", generation.RequiredProfiles)
			}
			// bindings должен выбрать пригодного соседа либо сообщить предметную неготовность.
			bindings, err := service.SelectStartBindings(context.Background(), streamID, nil)
			if bothChanged {
				if !errors.Is(err, core.ErrNotReady) {
					t.Fatal("Аннулированные файлы выданы как binding", bindings, err)
				}
			} else if err != nil || len(bindings) != 1 || bindings[0].VariantID != core.ID(originalID) {
				t.Fatal("Пригодный неизменённый сосед не выбран", bindings, err)
			}
			// groupID адресует единственную группу независимой заполненной фикстуры.
			var groupID core.ID
			if err = store.sqlDB.QueryRow("SELECT id FROM groups LIMIT 1").Scan(&groupID); err != nil {
				t.Fatal(err)
			}
			// explicit закрепляет изменённый файл, даже когда рядом есть пригодный canonical-дубль.
			explicit := core.StartSessionRequest{RequestID: "explicit-invalidated-binding", StreamID: streamID, Bindings: []core.PreparedBinding{{GroupID: groupID, GenerationID: generationID, VariantID: core.ID(siblingID), GeneratorVersion: generation.GeneratorVersion}}}
			// rejected и rejectErr должны подтвердить атомарный отказ без подмены VariantID.
			rejected, rejectErr := service.StartSession(context.Background(), explicit)
			if !errors.Is(rejectErr, core.ErrNotReady) || rejected.ID != "" {
				t.Fatal("Явный аннулированный binding подменён соседом", rejected, rejectErr)
			}
			// found исключает сохранение команды после отказа точной привязки; priorErr сообщает ошибку чтения.
			if _, found, priorErr := service.PriorStartSession(context.Background(), explicit); priorErr != nil || found {
				t.Fatal("Отклонённый binding оставил команду", found, priorErr)
			}
			// copyGeneration проверяет запрет нового обычного поколения с историческим флагом.
			copyGeneration := generation
			copyGeneration.ID = "ordinary-flagged-generation"
			err = store.Write(context.Background(), func(repositories core.Repositories) error {
				// Функция выполняет обычную вставку; repositories ограничен этой транзакцией.
				return repositories.Artifacts.InsertGeneration(context.Background(), copyGeneration)
			})
			if !errors.Is(err, core.ErrInvalid) {
				t.Fatal("Обычная вставка создала миграционное исключение", err)
			}
			// duplicate содержит новый ID и путь, но ту же canonical существующего FAILED варианта.
			var duplicate core.FallbackVariant
			if err = core.DecodeMetadata(after["fallback_variants"][1][4].([]byte), &duplicate); err != nil {
				t.Fatal(err)
			}
			duplicate.ID, duplicate.ManagedPath = "ordinary-duplicate", "fallback/ordinary-duplicate.mp4"
			err = store.Write(context.Background(), func(repositories core.Repositories) error {
				// Функция через repositories проверяет уникальность всех состояний, включая FAILED.
				return repositories.Artifacts.InsertVariant(context.Background(), duplicate)
			})
			if !errors.Is(err, core.ErrConflict) {
				t.Fatal("Обычная вставка допустила новый canonical-дубль", err)
			}
		})
	}
}

// TestDesiredMigrationDownCannotUndoReplacement проверяет необратимость настоящего Goose Down.
// t сравнивает полный граф, marker, историю версий и DDL до и после отклонённого возврата.
func TestDesiredMigrationDownCannotUndoReplacement(t *testing.T) {
	// db и provider принадлежат отдельной временной копии старой заполненной SQLite.
	db, provider := migrationBoundaryFixture(t)
	poisonFrozenDesired(t, db)
	// err сообщает о невозможности подготовить успешно заменённые данные.
	if _, err := provider.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	// before фиксирует все данные, включая версии; schema содержит полный состав DDL.
	before := captureSQLRows(t, db)
	var schema string
	// err сообщает об ошибке чтения исходной схемы перед отклоняемым Down.
	if err := db.QueryRow("SELECT group_concat(sql, char(10)) FROM (SELECT sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY type,name)").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	// err должен сохранить явный отказ необратимой миграции через runner Goose.
	if _, err := provider.Down(context.Background()); !errors.Is(err, ErrIncompatibleFormat) {
		t.Fatal("Необратимый переход допускает Down", err)
	}
	// afterSchema проверяет, что отказ не поменял индексы или таблицы.
	var afterSchema string
	// err сообщает об ошибке повторного чтения схемы после отказа Down.
	if err := db.QueryRow("SELECT group_concat(sql, char(10)) FROM (SELECT sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY type,name)").Scan(&afterSchema); err != nil {
		t.Fatal(err)
	}
	if afterSchema != schema || !reflect.DeepEqual(before, captureSQLRows(t, db)) {
		t.Fatal("Отказ Down изменил данные, версии или схему")
	}
}

// selectionFaultWork сохраняет настоящие транзакции и подменяет только отказ чтения варианта.
type selectionFaultWork struct {
	// UnitOfWork предоставляет исходное изолированное хранилище и неизменённую Write.
	core.UnitOfWork
	// failure задаёт точный возвращаемый отказ репозитория.
	failure error
}

// View оборачивает только Artifacts в согласованной транзакции ctx и вызывает fn без иных изменений.
func (w selectionFaultWork) View(ctx context.Context, fn func(core.Repositories) error) error {
	return w.UnitOfWork.View(ctx, func(repositories core.Repositories) error {
		// repositories сохраняет настоящие Config/Runtime; callback заменяет единственную границу отказа.
		repositories.Artifacts = selectionFaultArtifacts{repositories.Artifacts, w.failure}
		return fn(repositories)
	})
}

// selectionFaultArtifacts сохраняет настоящий каталог поколений, изменяя только чтение Variant.
type selectionFaultArtifacts struct {
	// ArtifactRepository обслуживает все операции, кроме выбранного чтения файла.
	core.ArtifactRepository
	// failure содержит ошибку, которую нельзя потерять при переборе canonical-совпадений.
	failure error
}

// Variant возвращает заданный отказ без файлового ввода-вывода; ctx и id определяют обычное чтение.
func (a selectionFaultArtifacts) Variant(ctx context.Context, id core.ID) (core.FallbackVariant, error) {
	return core.FallbackVariant{}, a.failure
}

// TestBindingSelectionPreservesJoinedStorageFailures проверяет, что предметная неготовность
// не скрывает одновременно возвращённую отмену или повреждение. t использует настоящие остальные репозитории.
func TestBindingSelectionPreservesJoinedStorageFailures(t *testing.T) {
	// fixture содержит один пригодный вариант, позволяющий дойти до проверяемого чтения.
	fixture := newDomainFixture(t, 1)
	// serious перечисляет независимые причины, которые обязаны дойти до вызывающего кода.
	for _, serious := range []error{context.Canceled, core.ErrCorrupt} {
		// callback проверяет одну составную ошибку на неизменённой временной базе.
		t.Run(serious.Error(), func(t *testing.T) {
			// failure сохраняет оба признака отказа, не меняя доменные значения фикстуры.
			failure := errors.Join(core.ErrNotReady, serious)
			// service использует настоящую конфигурацию и контролируемое чтение Variant; err сообщает отказ создания.
			service, err := core.NewService(selectionFaultWork{fixture.store, failure}, fixture.store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			// bindings и err фиксируют результат публичной операции выбора привязок.
			bindings, err := service.SelectStartBindings(context.Background(), fixture.stream.ID, nil)
			if !errors.Is(err, serious) || len(bindings) != 0 {
				t.Fatalf("Перебор потерял серьёзную причину %v: bindings=%v, error=%v", serious, bindings, err)
			}
		})
	}
}

// TestOutputRestartRejectsInvalidatedSnapshotAtomically проверяет первый и последующий запуск
// прикреплённого выхода после остановки. t сверяет полный откат отказа и пригодность неизменённого файла.
func TestOutputRestartRejectsInvalidatedSnapshotAtomically(t *testing.T) {
	// repeated выбирает снимок до первого внешнего запуска либо после принятого повторного запуска.
	for _, repeated := range []bool{false, true} {
		// callback выполняет сценарий на собственной временной SQLite без физических работников.
		t.Run(map[bool]string{false: "first", true: "repeated"}[repeated], func(t *testing.T) {
			// fixture предоставляет прикреплённый выход и действующую родительскую Session.
			fixture := newDomainFixture(t, 1)
			// ctx ограничивает доменные операции текущим сценарием.
			ctx := context.Background()
			// start содержит принятую первоначальную команду Session.
			start := startDomainSession(t, fixture)
			executeCommand(t, fixture.service, start.RequestID)
			// output содержит ещё не выданный внешнему работнику снимок назначения.
			output := readOutputs(t, fixture.store, start.SessionID)[0]
			// err сообщает об ошибке остановки первоначального намерения без внешнего процесса.
			if _, err := fixture.service.StopOutput(ctx, core.OutputCommand{RequestID: "stop-initial-intent", OutputID: output.ID}); err != nil {
				t.Fatal(err)
			}
			// cleanup подтверждает предварительные ресурсы выданной Session до нового запуска выхода.
			for _, cleanup := range pendingCleanups(t, fixture.store, start.SessionID) {
				// err сообщает об отказе подтверждения завершённой предварительной очистки.
				if err := fixture.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanup)); err != nil {
					t.Fatal(err)
				}
			}
			if repeated {
				// err сообщает об ошибке допуска пригодного выхода до моделирования прежнего снимка.
				if _, err := fixture.service.StartOutput(ctx, core.OutputCommand{RequestID: "healthy-prior-restart", OutputID: output.ID}); err != nil {
					t.Fatal(err)
				}
				// err сообщает об ошибке остановки этого предварительного повторного запуска.
				if _, err := fixture.service.StopOutput(ctx, core.OutputCommand{RequestID: "stop-prior-restart", OutputID: output.ID}); err != nil {
					t.Fatal(err)
				}
				// cleanup закрывает обязательство предварительной группы повторного намерения.
				for _, cleanup := range pendingCleanups(t, fixture.store, start.SessionID) {
					// err сообщает об отказе подтверждения очистки повторного намерения.
					if err := fixture.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanup)); err != nil {
						t.Fatal(err)
					}
				}
			}
			output = readOutputs(t, fixture.store, start.SessionID)[0]
			// healthyPayload позволяет вернуть неизменённый снимок для положительного контроля;
			// err сообщает об ошибке кодирования его метаданных.
			healthyPayload, err := core.EncodeMetadata(output)
			if err != nil {
				t.Fatal(err)
			}
			output.Snapshot.Artifact.RequiresRebuild = true
			// invalidPayload моделирует самостоятельный исторический снимок после миграции.
			invalidPayload, err := core.EncodeMetadata(output)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = fixture.store.sqlDB.Exec("UPDATE outputs SET payload=? WHERE id=?", invalidPayload, output.ID); err != nil {
				t.Fatal(err)
			}
			// before фиксирует все таблицы перед отклоняемой командой.
			before := captureSQLRows(t, fixture.store.sqlDB)
			// rejected и err должны подтвердить отказ до квитанции и изменения Generation.
			rejected, err := fixture.service.StartOutput(ctx, core.OutputCommand{RequestID: "invalidated-restart", OutputID: output.ID})
			if !errors.Is(err, core.ErrNotReady) || rejected.ID != "" || !reflect.DeepEqual(before, captureSQLRows(t, fixture.store.sqlDB)) {
				t.Fatal("Непригодный снимок запущен либо отказ оставил изменения", rejected, err)
			}
			if _, err = fixture.store.sqlDB.Exec("UPDATE outputs SET payload=? WHERE id=?", healthyPayload, output.ID); err != nil {
				t.Fatal(err)
			}
			// accepted подтверждает новый допуск только прежнего неизменённого снимка.
			accepted, err := fixture.service.StartOutput(ctx, core.OutputCommand{RequestID: "unchanged-restart", OutputID: output.ID})
			if err != nil || accepted.OutputID != output.ID {
				t.Fatal("Неизменённый снимок потерял пригодность", accepted, err)
			}
		})
	}
}

// incompleteExplicitProfile удаляет обязательное поле field из корректного v3 с Encoding:null
// и добавляет старое открытое значение range. t сообщает о повреждении подготовки JSON.
func incompleteExplicitProfile(t *testing.T, field string) []byte {
	t.Helper()
	// profile использует прежний рецепт с явным режимом LIVE-аудио и нулевым Encoding.
	profile := storageProfile()
	profile.LiveAudioMode = core.LiveAudioCopy
	// encoded содержит корректную полную оболочку v3; err сообщает ошибку её создания.
	encoded, err := core.EncodeProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	// object и video адресуют обязательное поле дорожки, не меняя остальные данные.
	object := migrationJSON(t, encoded)
	video := object["Format"].(map[string]any)["Video"].(map[string]any)
	delete(video, field)
	// missingOnly доказывает, что прежний декодер отвергает отсутствие, ещё до неподдержанного range.
	missingOnly, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = core.DecodeProfile(missingOnly); !errors.Is(err, core.ErrInvalidMetadata) || !strings.Contains(err.Error(), "отсутствует материализованное поле "+field) {
		t.Fatal("Контрпример не нарушает прежний контракт полноты", field, err)
	}
	video["Range"] = base64.StdEncoding.EncodeToString([]byte("obsolete-range"))
	// incomplete содержит старое повреждение структуры рядом с разрешённой заменой настройки.
	incomplete, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return incomplete
}

// TestDesiredMigrationRejectsIncompleteExplicitLegacyProfile проверяет, что разрешённая замена
// range не материализует отсутствующие поля старого v3. t проверяет декодер и настоящий Goose-переход отдельно.
func TestDesiredMigrationRejectsIncompleteExplicitLegacyProfile(t *testing.T) {
	// field перечисляет обязательные поля, чья пустота допустима только при явном присутствии.
	for _, field := range []string{"Level", "Tier"} {
		// callback проверяет один неполный профиль без изменения других случаев.
		t.Run(field, func(t *testing.T) {
			// incomplete сохраняет точную структуру повреждённого старого v3.
			incomplete := incompleteExplicitProfile(t, field)
			// callback проверяет чистую границу миграционного декодера.
			t.Run("decoder", func(t *testing.T) {
				// err должен отвергнуть старое повреждение вместо ремонта недостающего поля.
				if _, _, _, err := migrationProfile(incomplete); err == nil {
					t.Fatal("Миграция скрыла отсутствие обязательного поля", field)
				}
			})
			// callback проверяет полный откат на временной заполненной SQLite.
			t.Run("goose", func(t *testing.T) {
				// db и provider принадлежат отдельной копии, остальные исторические профили остаются корректными.
				db, provider := migrationBoundaryFixture(t)
				// payload содержит самостоятельный именованный профиль без canonical-проекции.
				var payload []byte
				// profileID ограничивает изменение единственной строкой с её собственной Record.
				var profileID core.ID
				// err сообщает об ошибке чтения выбранной исходной записи профиля.
				if err := db.QueryRow("SELECT id,payload FROM profiles LIMIT 1").Scan(&profileID, &payload); err != nil {
					t.Fatal(err)
				}
				// envelope содержит запись профиля; raw заменяет только её собственный desired.
				envelope := migrationJSON(t, payload)
				envelope["Payload"].(map[string]any)["Profile"] = migrationJSON(t, incomplete)
				// raw содержит согласованную внешнюю оболочку с единственным прежним повреждением;
				// err сообщает об ошибке её JSON-кодирования.
				raw, err := json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec("UPDATE profiles SET payload=? WHERE id=?", raw, profileID); err != nil {
					t.Fatal(err)
				}
				// before фиксирует все данные и обе версии до попытки миграции.
				before := captureSQLRows(t, db)
				if _, err = provider.Up(context.Background()); err == nil || !reflect.DeepEqual(before, captureSQLRows(t, db)) {
					t.Fatal("Неполный прежний профиль изменён вместо полного отката", field, err)
				}
				t.Log("Причина отклонения неполного прежнего профиля:", err)
			})
		})
	}
}
