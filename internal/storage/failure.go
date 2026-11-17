package storage

import (
	"context"
	"errors"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/mattn/go-sqlite3"
)

// storedFailureContext связывает ID прочитанной строки с известным видом сущности.
// Неподдерживаемая внутренняя таблица даёт пустой контекст, а не произвольное имя ресурса.
func storedFailureContext(table string, id core.ID) core.FailureContext {
	switch table {
	case "streams":
		return core.FailureContext{StreamID: id}
	case "profiles":
		return core.FailureContext{ProfileID: id}
	case "groups":
		return core.FailureContext{GroupID: id}
	case "destinations":
		return core.FailureContext{DestinationID: id}
	case "fallback_assets":
		return core.FailureContext{AssetID: id}
	case "fallback_generations":
		return core.FailureContext{GenerationID: id}
	case "fallback_variants":
		return core.FailureContext{VariantID: id}
	case "sessions":
		return core.FailureContext{SessionID: id}
	case "outputs":
		return core.FailureContext{OutputID: id}
	case "command_receipts":
		return core.FailureContext{CommandID: id}
	default:
		return core.FailureContext{}
	}
}

// deletionFailure дополняет отказ DELETE только при доказанной сохраняемой ссылке на его строку.
// Текст драйвера и один ErrInUse не являются предметным доказательством. Дополнительное чтение
// выполняется лишь после отказа в той же транзакции и не меняет исходную категорию ошибки.
func deletionFailure(ctx context.Context, r *repositories, table string, id core.ID, cause error) error {
	// translated сохраняет прежний выбор доменной категории и диагностику движка.
	translated := translate(cause)
	// engine подтверждает ограничение SQLite; ok исключает произвольную ошибку с тем же sentinel.
	engine, ok := errors.AsType[sqlite3.Error](cause)
	if !ok || engine.Code != sqlite3.ErrConstraint || !errors.Is(translated, core.ErrInUse) {
		return translated
	}
	// query проверяет только известные FK-ссылки и удержания соответствующей таблицы.
	var query string
	switch table {
	case "streams":
		query = "SELECT EXISTS(SELECT 1 FROM groups WHERE stream_id = ?1 UNION ALL SELECT 1 FROM destinations WHERE stream_id = ?1 UNION ALL SELECT 1 FROM session_holds WHERE stream_id = ?1 UNION ALL SELECT 1 FROM output_holds WHERE stream_id = ?1)"
	case "profiles":
		query = "SELECT EXISTS(SELECT 1 FROM groups WHERE profile_id = ?1 UNION ALL SELECT 1 FROM output_holds WHERE profile_id = ?1)"
	case "groups":
		query = "SELECT EXISTS(SELECT 1 FROM destinations WHERE group_id = ?1 UNION ALL SELECT 1 FROM output_holds WHERE group_id = ?1)"
	case "destinations":
		query = "SELECT EXISTS(SELECT 1 FROM output_holds WHERE destination_id = ?1)"
	case "fallback_assets":
		query = "SELECT EXISTS(SELECT 1 FROM streams WHERE asset_id = ?1 UNION ALL SELECT 1 FROM fallback_generations WHERE asset_id = ?1 UNION ALL SELECT 1 FROM session_holds WHERE asset_id = ?1 UNION ALL SELECT 1 FROM output_holds WHERE asset_id = ?1)"
	case "fallback_generations":
		query = "SELECT EXISTS(SELECT 1 FROM fallback_assets WHERE current_generation_id = ?1 OR pending_generation_id = ?1 UNION ALL SELECT 1 FROM fallback_variants WHERE generation_id = ?1 UNION ALL SELECT 1 FROM owned_paths WHERE generation_id = ?1 UNION ALL SELECT 1 FROM session_holds WHERE generation_id = ?1 UNION ALL SELECT 1 FROM output_holds WHERE generation_id = ?1)"
	case "fallback_variants":
		query = "SELECT EXISTS(SELECT 1 FROM output_holds WHERE variant_id = ?1)"
	default:
		return translated
	}
	// db использует то же записывающее соединение; err сообщает отказ диагностического чтения.
	db, err := r.access(ctx, false)
	if err != nil {
		return translated
	}
	// row подтверждает существование цели; входной id сам по себе не становится публичным.
	var row metadataRow
	if err = db.Table(table).Where("id = ?", id).Take(&row).Error; err != nil {
		return translated
	}
	// used подтверждает реальную сохраняемую ссылку, из-за которой удаление запрещено.
	var used bool
	if err = db.Raw(query, row.ID).Scan(&used).Error; err != nil || !used {
		return translated
	}
	return core.WithFailureReason(translated, core.FailureReason{Code: core.FailureResourceInUse, Context: storedFailureContext(table, row.ID)})
}
