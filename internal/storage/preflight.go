package storage

import (
	"context"
	"errors"
	"fmt"
)

// preflightStorage читает только метаданные схемы, маркер и историю Goose в пуле s.
// s предоставляет пул проверяемой БД; ctx ограничивает чтение. Пустая схема допускает первичное
// создание, текущая требует полного набора таблиц, marker4 и ровно применённых Goose0/1. Любой иной класс
// отклоняется до инициализации Goose и Recover; данные профилей здесь не читаются.
// Проверка не исправляет остаток прерванной первичной инициализации с одним Goose0.
func (s *Store) preflightStorage(ctx context.Context) error {
	// expected перечисляет таблицы единственной текущей схемы, включая служебную историю.
	expected := map[string]bool{
		"storage_format": true, "goose_db_version": true,
		"fallback_assets": true, "fallback_generations": true,
		"streams": true, "profiles": true, "groups": true, "destinations": true,
		"fallback_variants": true, "sessions": true, "outputs": true,
		"command_receipts": true, "cleanup_intents": true, "owned_paths": true,
		"session_holds": true, "output_holds": true, "events": true,
	}
	// rows читает пользовательские таблицы, представления и триггеры; err сообщает отказ SQLite.
	// Внутренние объекты SQLite не задают формат приложения, обычные индексы здесь не сканируются.
	rows, err := s.sqlDB.QueryContext(ctx, "SELECT type,name FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*' AND type IN ('table','view','trigger')")
	if err != nil {
		return fmt.Errorf("прочитать схему SQLite: %w", err)
	}
	// count считает найденные основные объекты; incompatible запоминает чужую или неполную схему.
	count, incompatible := 0, false
	for rows.Next() {
		// kind различает таблицу и чужой объект; name содержит точное имя объекта SQLite.
		var kind, name string
		if err = rows.Scan(&kind, &name); err != nil {
			return errors.Join(err, rows.Close())
		}
		count++
		if kind != "table" || !expected[name] {
			incompatible = true
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	if incompatible || count != len(expected) {
		return fmt.Errorf("%w: неизвестный или неполный набор таблиц", ErrIncompatibleFormat)
	}
	if err = s.checkStorageFormat(ctx); err != nil {
		return err
	}
	// total считает всю историю, zero — единственный применённый ноль, current — применённую начальную версию 1.
	var total, zero, current int64
	if err = s.sqlDB.QueryRowContext(ctx, `SELECT count(*),
		coalesce(sum(CASE WHEN version_id=0 AND is_applied=1 THEN 1 ELSE 0 END),0),
		coalesce(sum(CASE WHEN version_id=1 AND is_applied=1 THEN 1 ELSE 0 END),0)
		FROM goose_db_version`).Scan(&total, &zero, &current); err != nil {
		return fmt.Errorf("%w: нечитаемая история Goose: %w", ErrIncompatibleFormat, err)
	}
	if total != 2 || zero != 1 || current != 1 {
		return fmt.Errorf("%w: история Goose не соответствует текущей схеме", ErrIncompatibleFormat)
	}
	return nil
}
