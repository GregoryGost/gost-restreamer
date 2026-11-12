package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// metadataRow отделяет точные доменные метаданные от индексируемых SQL-значений.
type metadataRow struct {
	// ID идентифицирует SQL-строку и сверяется с ID доменной нагрузки.
	ID core.ID
	// Revision содержит ревизию двадцатизначной десятичной строкой без потери uint64.
	Revision string
	// CreatedAt содержит время создания строки в микросекундах Unix.
	CreatedAt int64
	// UpdatedAt содержит время изменения строки в микросекундах Unix.
	UpdatedAt int64
	// Payload содержит точные версионированные доменные метаданные строки.
	Payload []byte
}

// decimal кодирует uint64 двадцатью десятичными цифрами, сохраняя числовой порядок при строковом
// сравнении.
func decimal(v uint64) string { return fmt.Sprintf("%020d", v) }

// nullable представляет пустой доменный ID как SQL NULL, а непустой — строкой.
func nullable(v core.ID) any {
	if v == "" {
		return nil
	}
	return string(v)
}

// timestamp проверяет ненулевое представимое время и возвращает UTC с точностью до микросекунд.
func timestamp(t time.Time) (time.Time, error) {
	if t.IsZero() {
		return time.Time{}, core.ErrInvalid
	}
	// normalized содержит UTC-время с точностью хранения для проверки представимости исходной метки.
	normalized := time.UnixMicro(t.UnixMicro()).UTC()
	if !normalized.Equal(t.Truncate(time.Microsecond)) {
		return time.Time{}, core.ErrInvalid
	}
	return normalized, nil
}

// recordOf извлекает общую запись поддерживаемой сущности; для Event создаёт запись без ревизии с
// одинаковыми временами, а для неизвестного типа возвращает нулевое значение.
func recordOf(value any) core.Record {
	// v содержит конкретную доменную сущность для извлечения общей Record.
	switch v := value.(type) {
	case core.Stream:
		return v.Record
	case core.TranscodeProfile:
		return v.Record
	case core.TranscodeGroup:
		return v.Record
	case core.Destination:
		return v.Record
	case core.FallbackAsset:
		return v.Record
	case core.FallbackVariant:
		return v.Record
	case core.FallbackGeneration:
		return v.Record
	case core.OwnedFile:
		return v.Record
	case core.Session:
		return v.Record
	case core.SessionOutput:
		return v.Record
	case core.CommandReceipt:
		return v.Record
	case core.CleanupIntent:
		return v.Record
	case core.Event:
		return core.Record{ID: v.ID, CreatedAt: v.CreatedAt, UpdatedAt: v.CreatedAt}
	}
	return core.Record{}
}

// normalize проверяет запись, нормализует времена и выполняет применимую проверку доменного значения
// перед сохранением; неподдерживаемый тип отклоняется.
func normalize(value any) (any, error) {
	// record содержит общую запись сущности до проверки и нормализации времён.
	record := recordOf(value)
	// err сообщает об ошибке проверки идентификатора сохраняемой сущности.
	if err := record.ID.Validate(); err != nil {
		return nil, err
	}
	// err передаёт отказы нормализации времён и доменной проверки сохраняемого значения.
	var err error
	if record.CreatedAt, err = timestamp(record.CreatedAt); err != nil {
		return nil, err
	}
	if record.UpdatedAt, err = timestamp(record.UpdatedAt); err != nil {
		return nil, err
	}
	if record.UpdatedAt.Before(record.CreatedAt) {
		return nil, core.ErrInvalid
	}
	// event показывает, является ли значение событием, для которого ревизия не требуется.
	if _, event := value.(core.Event); !event && record.Revision == 0 {
		return nil, core.ErrInvalid
	}
	// v содержит копию конкретной сущности для подстановки нормализованной Record.
	switch v := value.(type) {
	case core.Stream:
		v.Record = record
		return v, v.Validate()
	case core.TranscodeProfile:
		v.Record = record
		return v, v.Validate()
	case core.TranscodeGroup:
		v.Record = record
		return v, v.Validate()
	case core.Destination:
		v.Record = record
		return v, v.Validate()
	case core.FallbackGeneration:
		v.Record = record
		return v, v.Validate()
	case core.OwnedFile:
		v.Record = record
		return v, v.Validate()
	case core.FallbackAsset:
		v.Record = record
		return v, v.Validate()
	case core.FallbackVariant:
		v.Record = record
		if err = v.Validate(); err != nil {
			return nil, err
		}
		// key содержит ключ профиля варианта, content — его канонические байты, а e — результат
		// вычисления идентичности.
		key, content, e := core.ProfileIdentity(v.Profile)
		if e != nil {
			return nil, e
		}
		if v.ProfileKey != key || !bytes.Equal(v.CanonicalProfile, content) {
			return nil, core.ErrInvalid
		}
		return v, nil
	case core.Session:
		v.Record = record
		if v.EndedAt != nil {
			// t содержит нормализованное время завершения сессии, а e — результат проверки
			// его представимости.
			t, e := timestamp(*v.EndedAt)
			if e != nil {
				return nil, e
			}
			v.EndedAt = &t
		}
		return v, v.Validate()
	case core.SessionOutput:
		v.Record = record
		return v, v.Validate()
	case core.CommandReceipt:
		v.Record = record
		return v, nil
	case core.CleanupIntent:
		v.Record = record
		if err = v.Validate(); err != nil {
			return nil, err
		}
		if v.Kind == core.CleanupFile {
			if err = core.ValidateManagedPath(v.ManagedPath); err != nil {
				return nil, err
			}
		}
		return v, nil
	case core.Event:
		v.CreatedAt = record.CreatedAt
		return v, v.Validate()
	}
	return nil, core.ErrInvalid
}

// fields кодирует точные версионированные метаданные и индексируемые SQL-поля одной сущности, отдельно
// представляя пустые внешние ссылки через NULL.
func fields(value any) (map[string]any, error) {
	// payload содержит версионированные доменные метаданные, а err — результат их кодирования.
	payload, err := core.EncodeMetadata(value)
	if err != nil {
		return nil, err
	}
	// rec содержит общую запись для индексируемых SQL-колонок.
	rec := recordOf(value)
	// f накапливает общие и типоспецифичные SQL-поля сохраняемой сущности.
	f := map[string]any{"id": rec.ID, "revision": decimal(rec.Revision), "created_at": rec.CreatedAt.UnixMicro(), "updated_at": rec.UpdatedAt.UnixMicro(), "payload": payload}
	// v содержит конкретную доменную сущность для извлечения индексируемых полей.
	switch v := value.(type) {
	case core.Stream:
		f["publication_key"] = v.PublicationKey
		f["asset_id"] = nullable(v.Fallback.AssetID)
	case core.TranscodeGroup:
		f["stream_id"] = v.StreamID
		f["profile_id"] = v.ProfileID
	case core.Destination:
		f["stream_id"] = v.StreamID
		f["group_id"] = v.GroupID
	case core.FallbackAsset:
		f["current_generation_id"] = nullable(v.CurrentGenerationID)
		f["pending_generation_id"] = nullable(v.PendingGenerationID)
		f["incarnation"] = v.Incarnation
		f["state"] = v.State
	case core.FallbackGeneration:
		f["asset_id"] = v.AssetID
		f["incarnation"] = v.Incarnation
		f["state"] = v.State
		f["issued"] = v.Issued
		f["cleanup_pending"] = v.CleanupPending
		f["source_hash"] = v.SourceHash
	case core.OwnedFile:
		f["asset_id"] = v.AssetID
		f["generation_id"] = v.GenerationID
		f["managed_path"] = v.ManagedPath
		f["kind"] = v.Kind
	case core.FallbackVariant:
		f["generation_id"] = v.GenerationID
		f["asset_id"] = v.AssetID
		f["incarnation"] = v.Incarnation
		f["source_hash"] = v.SourceHash
		f["profile_key"] = v.ProfileKey
		f["canonical_profile"] = v.CanonicalProfile
		f["generator_version"] = v.GeneratorVersion
		f["managed_path"] = v.ManagedPath
		f["size_bytes"] = decimal(v.SizeBytes)
		f["state"] = v.State
	case core.Session:
		f["stream_id"] = v.StreamID
		f["incarnation"] = v.Incarnation
		f["state"] = v.State
		f["lifecycle"] = decimal(v.Lifecycle)
		f["issued"] = v.Issued
		f["cleanup_pending"] = v.CleanupPending
	case core.SessionOutput:
		f["session_id"] = v.SessionID
		f["generation"] = decimal(v.Generation)
		f["attempt"] = decimal(v.Attempt)
		f["issued"] = v.Issued
		f["cleanup_pending"] = v.CleanupPending
	case core.CommandReceipt:
		f["request_id"] = v.RequestID
		f["session_id"] = nullable(v.SessionID)
		f["output_id"] = nullable(v.OutputID)
		f["incarnation"] = v.Incarnation
		f["state"] = v.Status
		f["lifecycle"] = decimal(v.Lifecycle)
		f["generation"] = decimal(v.OutputGeneration)
		f["attempt"] = decimal(v.Attempt)
	case core.CleanupIntent:
		f["artifact_generation_id"] = nullable(v.ArtifactGenerationID)
		f["file_id"] = nullable(v.FileID)
		f["kind"] = v.Kind
		f["stream_id"] = nullable(v.StreamID)
		f["session_id"] = nullable(v.SessionID)
		f["output_id"] = nullable(v.OutputID)
		f["incarnation"] = v.Incarnation
		f["lifecycle"] = decimal(v.Lifecycle)
		f["generation"] = decimal(v.Generation)
		f["attempt"] = decimal(v.Attempt)
		f["acknowledged"] = v.Acknowledged
		if v.ManagedPath == "" {
			f["managed_path"] = nil
		} else {
			f["managed_path"] = v.ManagedPath
		}
	case core.Event:
		delete(f, "revision")
		delete(f, "updated_at")
		f["session_id"] = v.SessionID
		f["output_id"] = nullable(v.OutputID)
	}
	return f, nil
}

// decodeRow декодирует и проверяет доменную нагрузку и её совпадение с общими колонками строки;
// повреждение возвращается как ErrCorrupt.
func decodeRow[T any](row metadataRow) (T, error) {
	// value принимает декодированное доменное значение проверяемой строки.
	var value T
	// err сообщает об ошибке чтения и доменной проверки нагрузки SQL-строки.
	if err := core.DecodeMetadata(row.Payload, &value); err != nil {
		return value, fmt.Errorf("%w: %w", core.ErrCorrupt, err)
	}
	// rec содержит общую запись нагрузки для сверки с индексируемыми колонками.
	rec := recordOf(value)
	if rec.ID != row.ID || rec.CreatedAt.UnixMicro() != row.CreatedAt {
		return value, core.ErrCorrupt
	}
	// event показывает, является ли строка событием без ревизии и времени изменения.
	if _, event := any(value).(core.Event); !event {
		// revision содержит числовую ревизию SQL-строки, а err — результат её разбора.
		revision, err := strconv.ParseUint(row.Revision, 10, 64)
		if err != nil || rec.Revision != revision || rec.UpdatedAt.UnixMicro() != row.UpdatedAt {
			return value, core.ErrCorrupt
		}
	}
	return value, nil
}

// getValue читает одну строку по заданной внутренней таблице и колонке и проверяет её метаданные;
// имена таблицы и колонки должны задаваться кодом репозитория.
func getValue[T any](ctx context.Context, r *repositories, table, column string, key any) (T, error) {
	// zero задаёт нулевой результат чтения при отказе до декодирования строки.
	var zero T
	// db содержит разрешённую читающую GORM-сессию, а err — результат проверки доступа.
	db, err := r.access(ctx, false)
	if err != nil {
		return zero, err
	}
	// row принимает единственную строку, выбранную по внутренней таблице и колонке.
	var row metadataRow
	if err = db.Table(table).Where(column+" = ?", key).Take(&row).Error; err != nil {
		return zero, translate(err)
	}
	return decodeRow[T](row)
}

// insertValue нормализует и сохраняет полное доменное значение через действующий записывающий
// репозиторий, переводя ошибки ограничений БД.
func insertValue(ctx context.Context, r *repositories, table string, value any) error {
	// db содержит разрешённую записывающую GORM-сессию, а err — результат проверки доступа.
	db, err := r.access(ctx, true)
	if err != nil {
		return err
	}
	value, err = normalize(value)
	if err != nil {
		return err
	}
	// f содержит полный набор SQL-полей вставляемой сущности.
	f, err := fields(value)
	if err != nil {
		return err
	}
	if err = db.Table(table).Model(&metadataRow{}).Create(f).Error; err != nil {
		return translate(err)
	}

	return nil
}

// updateValue требует следующую ревизию после expected, неизменное время создания и допустимые
// изменения метаданных, затем обновляет ровно одну строку с прежней ревизией.
func updateValue(ctx context.Context, r *repositories, table string, value any, expected uint64) error {
	// db содержит разрешённую записывающую GORM-сессию, а err — результат проверки доступа.
	db, err := r.access(ctx, true)
	if err != nil {
		return err
	}
	value, err = normalize(value)
	if err != nil {
		return err
	}
	// rec содержит новую Record для проверки ревизии и неизменности времени создания.
	rec := recordOf(value)
	// next содержит единственную допустимую следующую ревизию после expected.
	next, err := core.NextRevision(expected)
	if err != nil {
		return err
	}
	if rec.Revision != next {
		return core.ErrConflict
	}
	// old принимает прежнюю строку для проверки ревизии и неизменяемых метаданных.
	var old metadataRow
	if err = db.Table(table).Where("id = ?", rec.ID).Take(&old).Error; err != nil {
		return translate(err)
	}
	if old.Revision != decimal(expected) {
		return core.ErrConflict
	}
	if old.CreatedAt != rec.CreatedAt.UnixMicro() {
		return core.ErrConflict
	}
	if err = immutable(old.Payload, value); err != nil {
		return err
	}
	// f содержит SQL-поля условного обновления без ID и времени создания.
	f, err := fields(value)
	if err != nil {
		return err
	}
	delete(f, "id")
	delete(f, "created_at")
	// result содержит результат условного UPDATE для проверки ошибки и ровно одной изменённой
	// строки.
	result := db.Table(table).Model(&metadataRow{}).Where("id = ? AND revision = ?", rec.ID, decimal(expected)).Updates(f)
	if result.Error != nil {
		return translate(result.Error)
	}
	if result.RowsAffected != 1 {
		return core.ErrConflict
	}
	return nil
}

// deleteValue удаляет строку при ожидаемой ревизии; отсутствие ID возвращает ErrNotFound, а
// несовпадение ревизии — ErrConflict.
func deleteValue(ctx context.Context, r *repositories, table string, id core.ID, expected uint64) error {
	// db содержит разрешённую записывающую GORM-сессию, а err — результат проверки доступа.
	db, err := r.access(ctx, true)
	if err != nil {
		return err
	}
	// result содержит результат условного DELETE для различения успеха и конфликта.
	result := db.Table(table).Where("id = ? AND revision = ?", id, decimal(expected)).Delete(map[string]any{})
	if result.Error != nil {
		return translate(result.Error)
	}
	if result.RowsAffected != 1 {
		// row принимает оставшуюся строку для различения отсутствия ID и неверной ревизии.
		var row metadataRow
		if err = db.Table(table).Where("id = ?", id).Take(&row).Error; err != nil {
			return translate(err)
		}
		return core.ErrConflict
	}
	return nil
}

// same сравнивает точные версионированные представления значений; ошибка кодирования любого операнда
// означает неравенство.
func same(a, b any) bool {
	// x содержит точные метаданные левого значения, а e — результат их кодирования.
	x, e := core.EncodeMetadata(a)
	// y содержит точные метаданные правого значения, а f — результат их кодирования.
	y, f := core.EncodeMetadata(b)
	return e == nil && f == nil && bytes.Equal(x, y)
}

// immutable сверяет сохраняемые неизменяемые поля конкретного типа с прежней нагрузкой; для
// завершённых поколений и вариантов дополнительно запрещает изменение опубликованного содержимого.
func immutable(data []byte, value any) error {
	// bad отмечает изменение сохраняемого поля, которое должно оставаться неизменным.
	bad := false
	// v содержит конкретное новое значение для сравнения с прежней нагрузкой.
	switch v := value.(type) {
	case core.Stream:
		// old принимает прежнее значение потока для сравнения сохраняемых полей.
		var old core.Stream
		// err сообщает об ошибке чтения прежних метаданных потока.
		if err := core.DecodeMetadata(data, &old); err != nil {
			return errors.Join(core.ErrCorrupt, err)
		}
		bad = old.PublicationKey != v.PublicationKey
	case core.FallbackAsset:
		// old принимает прежнее значение заглушки для сравнения сохраняемых полей.
		var old core.FallbackAsset
		// err сообщает об ошибке чтения прежних метаданных заглушки.
		if err := core.DecodeMetadata(data, &old); err != nil {
			return errors.Join(core.ErrCorrupt, err)
		}
	case core.OwnedFile:
		// old принимает прежнее значение записи владения файлом для сравнения сохраняемых полей.
		var old core.OwnedFile
		// err сообщает об ошибке чтения прежних метаданных записи владения файлом.
		if err := core.DecodeMetadata(data, &old); err != nil {
			return errors.Join(core.ErrCorrupt, err)
		}
		old.Record = v.Record
		if old.ContentHash == "" && old.SizeBytes == 0 {
			old.ContentHash = v.ContentHash
			old.SizeBytes = v.SizeBytes
		}
		bad = !same(old, v)
	case core.FallbackGeneration:
		// old принимает прежнее значение поколения для сравнения сохраняемых полей.
		var old core.FallbackGeneration
		// err сообщает об ошибке чтения прежних метаданных поколения.
		if err := core.DecodeMetadata(data, &old); err != nil {
			return errors.Join(core.ErrCorrupt, err)
		}
		bad = old.AssetID != v.AssetID || old.OriginalPath != v.OriginalPath || old.StagingPath != v.StagingPath || old.Kind != v.Kind || old.Incarnation != v.Incarnation || old.AttemptToken != v.AttemptToken || old.GeneratorVersion != v.GeneratorVersion || !same(old.RequiredProfiles, v.RequiredProfiles) || old.RebuildFromGenerationID != v.RebuildFromGenerationID
		if old.State.Terminal() {
			// copy содержит прежнее завершённое поколение с допустимыми новыми служебными
			// признаками для проверки неизменности содержимого.
			copy := old
			copy.Record = v.Record
			copy.Issued = v.Issued
			copy.CleanupPending = v.CleanupPending
			bad = bad || !same(copy, v)
		}
	case core.FallbackVariant:
		// old принимает прежнее значение варианта для сравнения сохраняемых полей.
		var old core.FallbackVariant
		// err сообщает об ошибке чтения прежних метаданных варианта.
		if err := core.DecodeMetadata(data, &old); err != nil {
			return errors.Join(core.ErrCorrupt, err)
		}
		bad = old.GenerationID != v.GenerationID || old.AssetID != v.AssetID || old.SourceHash != v.SourceHash || old.ManagedPath != v.ManagedPath || old.AttemptToken != v.AttemptToken || old.GeneratorVersion != v.GeneratorVersion || old.ProfileKey != v.ProfileKey || !same(old.Profile, v.Profile) || !bytes.Equal(old.CanonicalProfile, v.CanonicalProfile) || old.Incarnation != v.Incarnation
		if old.State != core.VariantPreparing {
			old.Record = v.Record
			bad = bad || !same(old, v)
		}
	case core.Session:
		// old принимает прежнее значение сессии для сравнения сохраняемых полей.
		var old core.Session
		// err сообщает об ошибке чтения прежних метаданных сессии.
		if err := core.DecodeMetadata(data, &old); err != nil {
			return errors.Join(core.ErrCorrupt, err)
		}
		bad = old.StreamID != v.StreamID || old.Incarnation != v.Incarnation || !same(old.Snapshot, v.Snapshot)
	case core.SessionOutput:
		// old принимает прежнее значение выхода для сравнения сохраняемых полей.
		var old core.SessionOutput
		// err сообщает об ошибке чтения прежних метаданных выхода.
		if err := core.DecodeMetadata(data, &old); err != nil {
			return errors.Join(core.ErrCorrupt, err)
		}
		bad = old.SessionID != v.SessionID || !same(old.Snapshot, v.Snapshot)
	case core.CommandReceipt:
		// old принимает прежнее значение квитанции для сравнения сохраняемых полей.
		var old core.CommandReceipt
		// err сообщает об ошибке чтения прежних метаданных квитанции.
		if err := core.DecodeMetadata(data, &old); err != nil {
			return errors.Join(core.ErrCorrupt, err)
		}
		bad = old.RequestID != v.RequestID || old.Kind != v.Kind || old.TargetID != v.TargetID || old.Fingerprint != v.Fingerprint || !bytes.Equal(old.Identity, v.Identity) || old.SessionID != v.SessionID || old.OutputID != v.OutputID || old.Incarnation != v.Incarnation
	case core.CleanupIntent:
		// old принимает прежнее значение намерения очистки для сравнения сохраняемых полей.
		var old core.CleanupIntent
		// err сообщает об ошибке чтения прежних метаданных намерения очистки.
		if err := core.DecodeMetadata(data, &old); err != nil {
			return errors.Join(core.ErrCorrupt, err)
		}
		old.Record = v.Record
		old.Acknowledged = v.Acknowledged
		bad = !same(old, v)
	}
	if bad {
		return fmt.Errorf("%w: изменение неизменяемых данных", core.ErrConflict)
	}
	return nil
}
