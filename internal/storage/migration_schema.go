package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// rebuildFailedVariantUniqueness переносит все столбцы fallback_variants при включённых FK в tx.
// ctx ограничивает весь перенос. Удержания сначала сохраняются в транзакционную таблицу и удаляются
// из child; parent удаляется без ссылающихся строк. Схема child остаётся прежней. Явные индексы и
// триггеры восстанавливаются, а UNIQUE допускает только исторические FAILED-коллизии.
func rebuildFailedVariantUniqueness(ctx context.Context, tx *sql.Tx) error {
	// schema содержит действующую полную схему, включая все CHECK/PK/FK.
	var schema string
	// err сообщает об ошибке чтения проверяемого свойства SQLite.
	if err := tx.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name='fallback_variants'").Scan(&schema); err != nil {
		return err
	}
	if strings.Count(schema, "UNIQUE(generation_id,canonical_profile)") != 1 || !strings.HasPrefix(schema, "CREATE TABLE fallback_variants (") {
		return fmt.Errorf("%w: неизвестная схема вариантов", core.ErrCorrupt)
	}
	schema = strings.Replace(schema, "CREATE TABLE fallback_variants (", "CREATE TABLE fallback_variants_replacement (", 1)
	schema = strings.Replace(schema, "UNIQUE(generation_id,canonical_profile),", "", 1)
	// rows читает определения всех явных индексов и триггеров до удаления parent.
	rows, err := tx.QueryContext(ctx, "SELECT sql FROM sqlite_master WHERE tbl_name='fallback_variants' AND type IN ('index','trigger') AND sql IS NOT NULL ORDER BY type,name")
	if err != nil {
		return err
	}
	// definitions содержит точные определения объектов схемы для восстановления.
	var definitions []string
	for rows.Next() {
		// definition принимает исходный DDL одного индекса или триггера.
		var definition string
		if err = rows.Scan(&definition); err != nil {
			return errors.Join(err, rows.Close())
		}
		definitions = append(definitions, definition)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	// statement выполняет очередной этап переноса: staging остаётся частью той же транзакции.
	for _, statement := range []string{
		"CREATE TEMP TABLE migration_output_holds AS SELECT * FROM output_holds",
		"DELETE FROM output_holds", schema,
		"INSERT INTO fallback_variants_replacement SELECT * FROM fallback_variants",
	} {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if err = verifyMigrationCopy(ctx, tx, "fallback_variants", "fallback_variants_replacement"); err != nil {
		return err
	}
	// statement восстанавливает прежнее имя после удаления освобождённого parent.
	for _, statement := range []string{"DROP TABLE fallback_variants", "ALTER TABLE fallback_variants_replacement RENAME TO fallback_variants"} {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	// definition восстанавливает каждый прежний явный объект схемы.
	for _, definition := range definitions {
		if _, err = tx.ExecContext(ctx, definition); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "CREATE UNIQUE INDEX fallback_variants_active_canonical ON fallback_variants(generation_id,canonical_profile) WHERE state!='FAILED'"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO output_holds SELECT * FROM migration_output_holds"); err != nil {
		return err
	}
	if err = verifyMigrationCopy(ctx, tx, "output_holds", "migration_output_holds"); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "DROP TABLE migration_output_holds")
	return err
}

// verifyMigrationCopy проверяет число и все значения строк original и copied в tx до удаления
// исходной таблицы. Имена принадлежат коду миграции, ctx переносит отмену открытия хранилища.
func verifyMigrationCopy(ctx context.Context, tx *sql.Tx, original, copied string) error {
	// equal проверяет обе разности множеств и число строк, включая пустые таблицы.
	var equal bool
	// query использует только доверенные внутренние имена таблиц.
	query := "SELECT (SELECT count(*) FROM " + original + ")=(SELECT count(*) FROM " + copied + ") AND NOT EXISTS(SELECT * FROM " + original + " EXCEPT SELECT * FROM " + copied + ") AND NOT EXISTS(SELECT * FROM " + copied + " EXCEPT SELECT * FROM " + original + ")"
	// err сообщает об ошибке чтения проверяемого свойства SQLite.
	if err := tx.QueryRowContext(ctx, query).Scan(&equal); err != nil {
		return err
	}
	if !equal {
		return fmt.Errorf("%w: неполный перенос таблицы", core.ErrCorrupt)
	}
	return nil
}
