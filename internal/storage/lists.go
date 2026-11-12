package storage

import (
	"context"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"gorm.io/gorm"
)

// listColumns задаёт закрытый каталог SQL-колонок, доступных фильтрам каждого вида списка.
var listColumns = map[string]map[string]bool{
	"streams":              {"asset_id": true},
	"profiles":             {},
	"groups":               {"stream_id": true, "profile_id": true},
	"destinations":         {"stream_id": true, "group_id": true},
	"fallback_generations": {"asset_id": true, "state": true, "incarnation": true},
	"owned_paths":          {"asset_id": true, "generation_id": true},
	"fallback_assets":      {"state": true, "incarnation": true},
	"fallback_variants":    {"generation_id": true, "asset_id": true, "state": true, "incarnation": true},
	"sessions":             {"stream_id": true, "state": true, "incarnation": true},
	"outputs":              {"session_id": true},
	"command_receipts":     {"session_id": true, "output_id": true, "state": true, "incarnation": true},
	"cleanup_intents":      {"artifact_generation_id": true, "stream_id": true, "session_id": true, "output_id": true, "incarnation": true},
	"events":               {"session_id": true, "output_id": true},
}

// listValues проверяет запрос, читает не более Limit строк по возрастанию created_at и id и декодирует
// полные доменные метаданные.
func listValues[T any](ctx context.Context, r *repositories, table string, q core.ListQuery) ([]T, error) {
	// err сообщает об ошибке проверки фильтров и границ страницы.
	if err := q.Validate(); err != nil {
		return nil, err
	}
	// db содержит разрешённую читающую GORM-сессию, а err — результат проверки доступа к
	// репозиторию.
	db, err := r.access(ctx, false)
	if err != nil {
		return nil, err
	}
	db, err = applyList(db.Table(table), table, q)
	if err != nil {
		return nil, err
	}
	// rows принимает ограниченную страницу SQL-строк с доменными метаданными.
	var rows []metadataRow
	if err = db.Order("created_at ASC").Order("id ASC").Limit(q.Limit).Find(&rows).Error; err != nil {
		return nil, translate(err)
	}
	// values накапливает проверенные доменные значения страницы в порядке SQL-запроса.
	values := make([]T, 0, len(rows))
	// row содержит очередную строку страницы для декодирования и сверки.
	for _, row := range rows {
		// value содержит декодированное доменное значение, а e — результат проверки строки и
		// метаданных.
		value, e := decodeRow[T](row)
		if e != nil {
			return nil, e
		}
		values = append(values, value)
	}
	return values, nil
}

// applyList добавляет только разрешённые для таблицы фильтры и строгую границу продолжения страницы;
// диапазон времени имеет вид From ≤ created_at < To.
func applyList(db *gorm.DB, table string, q core.ListQuery) (*gorm.DB, error) {
	// columns содержит разрешённые фильтруемые колонки выбранной таблицы.
	columns := listColumns[table]
	// generationColumn выбирает имя SQL-колонки поколения для обычных сущностей или cleanup_intents.
	generationColumn := "generation_id"
	if table == "cleanup_intents" {
		generationColumn = "artifact_generation_id"
	}
	// filter содержит очередную колонку и непустое значение доменного фильтра.
	for _, filter := range []struct {
		// column указывает внутреннее разрешённое имя фильтруемой SQL-колонки.
		column string
		// value содержит строковое значение фильтра; пустое значение не добавляет условие.
		value string
	}{
		{"stream_id", string(q.StreamID)}, {"session_id", string(q.SessionID)},
		{generationColumn, string(q.GenerationID)}, {"output_id", string(q.OutputID)}, {"asset_id", string(q.AssetID)},
		{"group_id", string(q.GroupID)}, {"profile_id", string(q.ProfileID)},
		{"state", q.State}, {"incarnation", q.Incarnation},
	} {
		if filter.value != "" {
			if !columns[filter.column] {
				return nil, core.ErrInvalid
			}
			db = db.Where(filter.column+" = ?", filter.value)
		}
	}
	if q.ExcludeIncarnation != "" {
		if !columns["incarnation"] {
			return nil, core.ErrInvalid
		}
		db = db.Where("incarnation <> ?", q.ExcludeIncarnation)
	}
	if q.PendingOnly {
		switch table {
		case "fallback_generations":
			db = db.Where("state IN ? OR issued = ? OR cleanup_pending = ?", []string{"QUEUED", "CLAIMED", "ISSUED"}, true, true)
		case "sessions", "outputs":
			db = db.Where("cleanup_pending = ?", true)
		case "command_receipts":
			db = db.Where("state IN ?", []string{"PENDING", "CLAIMED", "ISSUED"})
		case "cleanup_intents":
			db = db.Where("acknowledged = ?", false)
		default:
			return nil, core.ErrInvalid
		}
	}
	if q.From != nil {
		// n содержит округлённую вверх нижнюю границу в микросекундах Unix, а err — результат
		// проверки времени.
		n, err := queryTime(*q.From)
		if err != nil {
			return nil, err
		}
		db = db.Where("created_at >= ?", n)
	}
	if q.To != nil {
		// n содержит округлённую вверх верхнюю границу в микросекундах Unix, а err — результат
		// проверки времени.
		n, err := queryTime(*q.To)
		if err != nil {
			return nil, err
		}
		db = db.Where("created_at < ?", n)
	}
	if q.After != nil {
		// err сообщает об ошибке проверки ID границы продолжения страницы.
		if err := q.After.ID.Validate(); err != nil {
			return nil, err
		}
		// n содержит время границы продолжения с точностью хранения, а err — результат проверки
		// его представимости.
		n, err := timestamp(q.After.CreatedAt)
		if err != nil {
			return nil, err
		}
		db = db.Where("(created_at > ? OR (created_at = ? AND id > ?))", n.UnixMicro(), n.UnixMicro(), q.After.ID)
	}
	return db, nil
}

// queryTime переводит границу диапазона в микросекунды Unix с округлением вверх, чтобы фильтры
// сохраняли точность на микросекундных записях.
func queryTime(t time.Time) (int64, error) {
	// n содержит нормализованное время границы, а err — результат проверки его представимости.
	n, err := timestamp(t)
	if err != nil {
		return 0, err
	}
	// Хранимые времена кратны микросекунде; границы диапазона округляются вверх.
	if t.After(n) {
		n = n.Add(time.Microsecond)
		if n.UnixMicro() < t.UnixMicro() {
			return 0, core.ErrInvalid
		}
	}
	return n.UnixMicro(), nil
}
