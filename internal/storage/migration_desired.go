package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/pressly/goose/v3"
)

// desiredProfilesMigration регистрирует необратимый переход только в текущем Goose Provider.
// Оба обработчика получают транзакцию Goose; откат запрещён, поскольку исходный текст заменён.
func desiredProfilesMigration() *goose.Migration {
	return goose.NewGoMigration(2, &goose.GoFunc{RunTx: migrateDesiredProfiles}, &goose.GoFunc{RunTx: rejectDesiredProfilesDown})
}

// rejectDesiredProfilesDown отклоняет возврат заменённых настроек в tx; ctx передаётся Goose.
func rejectDesiredProfilesDown(ctx context.Context, tx *sql.Tx) error {
	return fmt.Errorf("%w: замена настроек необратима", ErrIncompatibleFormat)
}

// migrationChange сохраняет проверенную новую нагрузку и SQL-поля одной прежней строки.
type migrationChange struct {
	// table выбирает одну из внутренних таблиц графа профилей.
	table string
	// id сохраняет неизменяемый идентификатор строки.
	id core.ID
	// payload содержит исходную нагрузку либо точечное преобразование.
	payload []byte
	// updates содержит исключительно изменяемые проекции профиля и состояние варианта.
	updates map[string]any
}

// migrateDesiredProfiles проверяет старый граф, перестраивает уникальность вариантов при FK ON,
// заменяет только открытые прежние настройки и меняет маркер 2→3 в tx. Goose записывает свою
// версию в эту же транзакцию; любая ошибка, включая отмену ctx, откатывает весь переход.
func migrateDesiredProfiles(ctx context.Context, tx *sql.Tx) error {
	// count, singleton и version проверяют единственный исходный маркер.
	var count, singleton, version int
	// err содержит ошибку чтения исходного формата.
	if err := tx.QueryRowContext(ctx, "SELECT count(*),coalesce(min(singleton),0),coalesce(min(version),0) FROM storage_format").Scan(&count, &singleton, &version); err != nil {
		return errors.Join(ErrIncompatibleFormat, err)
	}
	if count != 1 || singleton != 1 || version != 2 {
		return ErrIncompatibleFormat
	}
	// foreignKeys подтверждает режим именно соединения миграции.
	var foreignKeys int
	// err сообщает об ошибке чтения проверяемого свойства SQLite.
	if err := tx.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return err
	}
	if foreignKeys != 1 {
		return fmt.Errorf("%w: внешние ключи отключены", core.ErrCorrupt)
	}
	// changes содержит только проверенные строки; до полной проверки данные не меняются.
	var changes []migrationChange
	// table содержит очередную таблицу самостоятельных профилей.
	for _, table := range []string{"profiles", "fallback_generations", "fallback_variants", "sessions", "outputs"} {
		// rows читает исходные идентичности и нагрузки, err сообщает отказ SQL.
		rows, err := tx.QueryContext(ctx, "SELECT id,payload FROM "+table+" ORDER BY id")
		if err != nil {
			return err
		}
		// pending освобождает SQL-курсор до дополнительных проверок той же транзакции.
		var pending []migrationChange
		for rows.Next() {
			// change сохраняет исходную строку до преобразования.
			change := migrationChange{table: table}
			if err = rows.Scan(&change.id, &change.payload); err != nil {
				return errors.Join(err, rows.Close())
			}
			pending = append(pending, change)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		// change получает проверенное преобразование очередной исходной строки.
		for _, change := range pending {
			// updated содержит точечную замену, changed сообщает её наличие, err — отказ прежней проверки.
			updated, changed, err := migrationTree(change.payload)
			if err != nil {
				return fmt.Errorf("%w: таблица %s: %w", core.ErrCorrupt, table, err)
			}
			// value принимает строго проверенный доменный результат, err сообщает отказ декодирования.
			value, err := migrationValue(table, updated)
			if err != nil {
				return fmt.Errorf("%w: таблица %s: %w", core.ErrCorrupt, table, err)
			}
			// expected содержит SQL-проекции результата для сверки исходной строки, err сообщает отказ кодирования.
			expected, err := fields(value)
			if err != nil {
				return err
			}
			change.updates = map[string]any{}
			if table == "fallback_variants" {
				// envelope читает исходные проекции до миграции.
				var envelope struct { /* Version содержит версию доменных метаданных, Payload — исходные поля варианта. */
					Version int
					Payload map[string]json.RawMessage
				}
				if err = json.Unmarshal(change.payload, &envelope); err != nil {
					return err
				}
				// column выбирает проекцию, которая могла измениться при замене собственного профиля.
				for _, column := range []struct { /* sqlName задаёт SQL-колонку, jsonName — поле исходной нагрузки. */
					sqlName, jsonName string
				}{{"profile_key", "ProfileKey"}, {"canonical_profile", "CanonicalProfile"}, {"state", "State"}} {
					change.updates[column.sqlName] = expected[column.sqlName]
					// original принимает исходную строковую либо байтовую проекцию.
					var original any
					if column.sqlName == "canonical_profile" {
						// canonical содержит исходную двоичную SQL-проекцию профиля.
						var canonical []byte
						if err = json.Unmarshal(envelope.Payload[column.jsonName], &canonical); err != nil {
							return err
						}
						original = canonical
					} else {
						// text содержит исходную строковую SQL-проекцию ключа или состояния.
						var text string
						if err = json.Unmarshal(envelope.Payload[column.jsonName], &text); err != nil {
							return err
						}
						original = text
					}
					expected[column.sqlName] = original
				}
			}
			delete(expected, "payload")
			// columns обеспечивает детерминированный запрос сверки всех SQL-проекций.
			columns := make([]string, 0, len(expected))
			// column содержит очередное доверенное имя SQL-проекции.
			for column := range expected {
				columns = append(columns, column)
			}
			sort.Strings(columns)
			// predicates и arguments связывают исходную строку с каждой проекцией без интерполяции данных.
			predicates := make([]string, 0, len(columns))
			arguments := make([]any, 0, len(columns))
			// column содержит очередную проверяемую проекцию.
			for _, column := range columns {
				predicates = append(predicates, column+" IS ?")
				arguments = append(arguments, expected[column])
			}
			if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE "+strings.Join(predicates, " AND "), arguments...).Scan(&count); err != nil {
				return err
			}
			if count != 1 || recordOf(value).ID != change.id {
				return fmt.Errorf("%w: SQL-проекции %s", core.ErrCorrupt, table)
			}
			if changed {
				change.payload = updated
				changes = append(changes, change)
			}
		}
	}
	// err сообщает об ошибке переноса таблицы и её ограничений.
	if err := rebuildFailedVariantUniqueness(ctx, tx); err != nil {
		return err
	}
	// change записывает проверенную нагрузку, сохраняя Record и остальные SQL-колонки.
	for _, change := range changes {
		// assignments и arguments описывают только разрешённые обновления одной строки.
		assignments := []string{"payload=?"}
		arguments := []any{change.payload}
		// column и value содержат проекцию нового собственного профиля варианта.
		for column, value := range change.updates {
			assignments = append(assignments, column+"=?")
			arguments = append(arguments, value)
		}
		arguments = append(arguments, change.id)
		// err сообщает об ошибке записи проверенной строки в той же транзакции.
		if _, err := tx.ExecContext(ctx, "UPDATE "+change.table+" SET "+strings.Join(assignments, ",")+" WHERE id=?", arguments...); err != nil {
			return err
		}
	}
	// violations читает нарушения всех FK после восстановления удержаний, err сообщает отказ PRAGMA.
	violations, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	if violations.Next() {
		return errors.Join(core.ErrCorrupt, violations.Close())
	}
	if err = errors.Join(violations.Err(), violations.Close()); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE storage_format SET version=3 WHERE singleton=1 AND version=2")
	return err
}

// migrationValue строго декодирует data в доменную сущность таблицы table и проверяет общую Record.
func migrationValue(table string, data []byte) (any, error) {
	// value выбирает конкретную доменную сущность без постоянного ослабленного декодера.
	var value any
	switch table {
	case "profiles":
		value = new(core.TranscodeProfile)
	case "fallback_generations":
		value = new(core.FallbackGeneration)
	case "fallback_variants":
		value = new(core.FallbackVariant)
	case "sessions":
		value = new(core.Session)
	case "outputs":
		value = new(core.SessionOutput)
	default:
		return nil, core.ErrCorrupt
	}
	// err сообщает об отказе строгой проверки преобразованной сущности.
	if err := core.DecodeMetadata(data, value); err != nil {
		return nil, err
	}
	// concrete извлекает значение для общего существующего отображения SQL.
	var concrete any
	// typed содержит конкретный указатель уже проверенной доменной сущности.
	switch typed := value.(type) {
	case *core.TranscodeProfile:
		concrete = *typed
	case *core.FallbackGeneration:
		concrete = *typed
	case *core.FallbackVariant:
		concrete = *typed
	case *core.Session:
		concrete = *typed
	case *core.SessionOutput:
		concrete = *typed
	}
	// err сообщает о нарушении исходных метаданных Record.
	if err := recordOf(concrete).Validate(); err != nil {
		return nil, err
	}
	return concrete, nil
}

// migrationTree рекурсивно обходит известные JSON-объекты data, сохраняя байты незатронутых ветвей.
// Контейнер с собственным Profile проверяет старую идентичность до замены; RequiredProfile и
// ArtifactManifest получают внутренний признак, а собственный изменённый Variant становится FAILED.
func migrationTree(data []byte) ([]byte, bool, error) {
	// trimmed определяет вид JSON без потери исходного представления чисел.
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, false, core.ErrCorrupt
	}
	if trimmed[0] == '[' {
		// items содержит исходные самостоятельные элементы массива.
		var items []json.RawMessage
		// err сообщает об ошибке чтения исходного JSON и его типов.
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, false, err
		}
		// changed объединяет изменения отдельных элементов.
		changed := false
		// index выбирает преобразуемый элемент массива.
		for index := range items {
			// replacement содержит новую ветвь, updated отмечает замену, err сообщает отказ преобразования.
			replacement, updated, err := migrationTree(items[index])
			if err != nil {
				return nil, false, err
			}
			if updated {
				items[index] = replacement
				changed = true
			}
		}
		if !changed {
			return data, false, nil
		}
		// replacement сохраняет массив с точными неизменёнными ветвями, err сообщает отказ JSON.
		replacement, err := json.Marshal(items)
		return replacement, true, err
	}
	if trimmed[0] != '{' {
		return data, false, nil
	}
	// object хранит исходные JSON-поля без декодирования чисел во float64.
	var object map[string]json.RawMessage
	// err сообщает об ошибке чтения исходного JSON и его типов.
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, false, err
	}
	// exists обнаруживает поле, которого прежний контракт ещё не допускал.
	if _, exists := object["RequiresRebuild"]; exists {
		return nil, false, core.ErrCorrupt
	}
	// err проверяет прежние связи до того, как разные профили могут получить одну идентичность.
	if err := migrationOriginalRelations(object); err != nil {
		return nil, false, err
	}
	// changed объединяет собственное изменение и изменения вложенных объектов.
	changed := false
	// profileObject определяет контейнер именно доменного профиля, а не именованную обёртку.
	var profileObject map[string]json.RawMessage
	// raw содержит самостоятельный профиль, exists отличает его отсутствие от повреждения.
	if raw, exists := object["Profile"]; exists && json.Unmarshal(raw, &profileObject) == nil && profileObject["Mode"] != nil {
		// replacement содержит новый профиль, canonical — исходную идентичность, updated — факт замены, err — отказ проверки.
		replacement, canonical, updated, err := migrationProfile(object["Profile"])
		if err != nil {
			return nil, false, err
		}
		if object["ProfileKey"] != nil || object["CanonicalProfile"] != nil {
			if err = migrationIdentity(object, canonical); err != nil {
				return nil, false, err
			}
			if object["VerifiedFormat"] != nil {
				// oldProfile читает прежний desired только структурно для сохранения старого доказательства.
				var oldProfile struct { /* Format содержит исходное требование обеих дорожек. */
					Format core.MediaFormat
				}
				// verified содержит исходное измерение, которое никогда не переписывается миграцией.
				var verified core.MediaFormat
				if err = json.Unmarshal(object["Profile"], &oldProfile); err != nil {
					return nil, false, err
				}
				if err = json.Unmarshal(object["VerifiedFormat"], &verified); err != nil {
					return nil, false, err
				}
				// state различает вариант и manifest; совместимость требовалась у manifest и READY-варианта.
				var state core.VariantState
				if object["State"] != nil {
					if err = json.Unmarshal(object["State"], &state); err != nil {
						return nil, false, err
					}
				}
				if state == "" || state == core.VariantReady {
					if err = core.CompatibleFormat(verified, oldProfile.Format); err != nil {
						return nil, false, err
					}
				}
			}
			if updated {
				// profile содержит строго допустимую копию после ремонта, decodeErr сообщает отказ её чтения.
				profile, decodeErr := core.DecodeProfile(replacement)
				if decodeErr != nil {
					return nil, false, decodeErr
				}
				// key и bits задают новую идентичность копии, identityErr сообщает отказ её вычисления.
				key, bits, identityErr := core.ProfileIdentity(profile)
				if identityErr != nil {
					return nil, false, identityErr
				}
				object["ProfileKey"], err = json.Marshal(key)
				if err != nil {
					return nil, false, err
				}
				object["CanonicalProfile"], err = json.Marshal(bits)
				if err != nil {
					return nil, false, err
				}
				if object["State"] != nil {
					object["State"] = json.RawMessage(`"FAILED"`)
				} else {
					object["RequiresRebuild"] = json.RawMessage("true")
				}
			}
		}
		if updated {
			object["Profile"] = replacement
			changed = true
		}
	}
	// key и raw перебирают остальные самостоятельные объекты и массивы.
	for key, raw := range object {
		if key == "Profile" && profileObject["Mode"] != nil || key == "RequiresRebuild" {
			continue
		}
		// replacement и updated описывают преобразование вложенной ветви, err сообщает отказ её проверки.
		replacement, updated, err := migrationTree(raw)
		if err != nil {
			return nil, false, err
		}
		if updated {
			object[key] = replacement
			changed = true
		}
	}
	if !changed {
		return data, false, nil
	}
	// replacement сохраняет исходные RawMessage неизменённых полей, err сообщает отказ кодирования.
	replacement, err := json.Marshal(object)
	return replacement, true, err
}

// migrationOriginalRelations проверяет прежние связи object до замены. Иначе два различных
// повреждённых профиля могли бы случайно совпасть после ремонта и скрыть исходную ошибку.
func migrationOriginalRelations(object map[string]json.RawMessage) error {
	// raw содержит прежний список требований, exists сообщает о его наличии.
	if raw, exists := object["RequiredProfiles"]; exists {
		// requirements сохраняет исходные элементы без применения новой валидации.
		var requirements []map[string]json.RawMessage
		// err сообщает об ошибке чтения исходного JSON и его типов.
		if err := json.Unmarshal(raw, &requirements); err != nil {
			return err
		}
		// seen обнаруживает исходные дубли до разрешённых миграционных коллизий.
		seen := map[string]bool{}
		// required содержит исходную идентичность одного требования.
		for _, required := range requirements {
			// canonical содержит исходные точные байты профиля.
			var canonical []byte
			// err сообщает об ошибке чтения исходного JSON и его типов.
			if err := json.Unmarshal(required["CanonicalProfile"], &canonical); err != nil {
				return err
			}
			if seen[string(canonical)] {
				return core.ErrCorrupt
			}
			seen[string(canonical)] = true
		}
	}
	// artifactRaw содержит исходный manifest, exists сообщает о наличии снимка выхода.
	if artifactRaw, exists := object["Artifact"]; exists {
		// artifact и named читают две самостоятельные обёртки профиля снимка выхода.
		var artifact, named map[string]json.RawMessage
		// err сообщает об ошибке чтения исходного JSON и его типов.
		if err := json.Unmarshal(artifactRaw, &artifact); err != nil {
			return err
		}
		// err сообщает об ошибке чтения исходного JSON и его типов.
		if err := json.Unmarshal(object["Profile"], &named); err != nil {
			return err
		}
		// left сохраняет прежний canonical именованного профиля, leftErr сообщает отказ его чтения.
		_, left, _, leftErr := migrationProfile(named["Profile"])
		// right сохраняет прежний canonical артефакта, rightErr сообщает отказ его чтения.
		_, right, _, rightErr := migrationProfile(artifact["Profile"])
		// err объединяет отказы чтения обеих прежних самостоятельных копий.
		if err := errors.Join(leftErr, rightErr); err != nil {
			return err
		}
		if !bytes.Equal(left, right) {
			return core.ErrCorrupt
		}
	}
	if object["CanonicalProfile"] != nil && object["State"] != nil {
		// state сохраняет исходный этап, чтобы его ошибка не скрывалась заменой на FAILED.
		var state core.VariantState
		// err сообщает об ошибке чтения исходного JSON и его типов.
		if err := json.Unmarshal(object["State"], &state); err != nil {
			return err
		}
		if state != core.VariantPreparing && state != core.VariantReady && state != core.VariantFailed {
			return core.ErrCorrupt
		}
		if state == core.VariantReady {
			// size сохраняет исходный размер готового файла.
			var size uint64
			// err сообщает об ошибке чтения исходного JSON и его типов.
			if err := json.Unmarshal(object["SizeBytes"], &size); err != nil {
				return err
			}
			if size == 0 {
				return core.ErrCorrupt
			}
			// field выбирает обязательный текст готового варианта.
			for _, field := range []string{"SourceHash", "ContentHash"} {
				// text проверяется теми же требованиями к непустому UTF-8, что и доменные идентификаторы.
				var text string
				// err сообщает об ошибке чтения исходного JSON и его типов.
				if err := json.Unmarshal(object[field], &text); err != nil {
					return err
				}
				// err сообщает о недопустимом исходном тексте готового файла.
				if err := core.ID(text).Validate(); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
