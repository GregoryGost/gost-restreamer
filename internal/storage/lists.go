package storage

import (
	"context"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"gorm.io/gorm"
)

var listColumns = map[string]map[string]bool{
	"streams":           {"asset_id": true},
	"profiles":          {},
	"groups":            {"stream_id": true, "profile_id": true},
	"destinations":      {"stream_id": true, "group_id": true},
	"fallback_assets":   {"state": true, "incarnation": true},
	"fallback_variants": {"asset_id": true, "state": true, "incarnation": true},
	"sessions":          {"stream_id": true, "state": true, "incarnation": true},
	"outputs":           {"session_id": true},
	"command_receipts":  {"session_id": true, "output_id": true, "state": true, "incarnation": true},
	"cleanup_intents":   {"stream_id": true, "session_id": true, "output_id": true, "incarnation": true},
	"events":            {"session_id": true, "output_id": true},
}

func listValues[T any](ctx context.Context, r *repositories, table string, q core.ListQuery) ([]T, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	db, err := r.access(ctx, false)
	if err != nil {
		return nil, err
	}
	db, err = applyList(db.Table(table), table, q)
	if err != nil {
		return nil, err
	}
	var rows []metadataRow
	if err = db.Order("created_at ASC").Order("id ASC").Limit(q.Limit).Find(&rows).Error; err != nil {
		return nil, translate(err)
	}
	values := make([]T, 0, len(rows))
	for _, row := range rows {
		value, e := decodeRow[T](row)
		if e != nil {
			return nil, e
		}
		values = append(values, value)
	}
	return values, nil
}

func applyList(db *gorm.DB, table string, q core.ListQuery) (*gorm.DB, error) {
	columns := listColumns[table]
	for _, filter := range []struct {
		column string
		value  string
	}{
		{"stream_id", string(q.StreamID)}, {"session_id", string(q.SessionID)},
		{"output_id", string(q.OutputID)}, {"asset_id", string(q.AssetID)},
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
		n, err := queryTime(*q.From)
		if err != nil {
			return nil, err
		}
		db = db.Where("created_at >= ?", n)
	}
	if q.To != nil {
		n, err := queryTime(*q.To)
		if err != nil {
			return nil, err
		}
		db = db.Where("created_at < ?", n)
	}
	if q.After != nil {
		if err := q.After.ID.Validate(); err != nil {
			return nil, err
		}
		n, err := timestamp(q.After.CreatedAt)
		if err != nil {
			return nil, err
		}
		db = db.Where("(created_at > ? OR (created_at = ? AND id > ?))", n.UnixMicro(), n.UnixMicro(), q.After.ID)
	}
	return db, nil
}

func queryTime(t time.Time) (int64, error) {
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
