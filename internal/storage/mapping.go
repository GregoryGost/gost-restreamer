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
	ID        core.ID
	Revision  string
	CreatedAt int64
	UpdatedAt int64
	Payload   []byte
}

func decimal(v uint64) string { return fmt.Sprintf("%020d", v) }
func nullable(v core.ID) any {
	if v == "" {
		return nil
	}
	return string(v)
}

func timestamp(t time.Time) (time.Time, error) {
	if t.IsZero() {
		return time.Time{}, core.ErrInvalid
	}
	normalized := time.UnixMicro(t.UnixMicro()).UTC()
	if !normalized.Equal(t.Truncate(time.Microsecond)) {
		return time.Time{}, core.ErrInvalid
	}
	return normalized, nil
}

func recordOf(value any) core.Record {
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

func normalize(value any) (any, error) {
	record := recordOf(value)
	if err := record.ID.Validate(); err != nil {
		return nil, err
	}
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
	if _, event := value.(core.Event); !event && record.Revision == 0 {
		return nil, core.ErrInvalid
	}
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

func fields(value any) (map[string]any, error) {
	payload, err := core.EncodeMetadata(value)
	if err != nil {
		return nil, err
	}
	rec := recordOf(value)
	f := map[string]any{"id": rec.ID, "revision": decimal(rec.Revision), "created_at": rec.CreatedAt.UnixMicro(), "updated_at": rec.UpdatedAt.UnixMicro(), "payload": payload}
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

func decodeRow[T any](row metadataRow) (T, error) {
	var value T
	if err := core.DecodeMetadata(row.Payload, &value); err != nil {
		return value, fmt.Errorf("%w: %w", core.ErrCorrupt, err)
	}
	rec := recordOf(value)
	if rec.ID != row.ID || rec.CreatedAt.UnixMicro() != row.CreatedAt {
		return value, core.ErrCorrupt
	}
	if _, event := any(value).(core.Event); !event {
		revision, err := strconv.ParseUint(row.Revision, 10, 64)
		if err != nil || rec.Revision != revision || rec.UpdatedAt.UnixMicro() != row.UpdatedAt {
			return value, core.ErrCorrupt
		}
	}
	return value, nil
}

func getValue[T any](ctx context.Context, r *repositories, table, column string, key any) (T, error) {
	var zero T
	db, err := r.access(ctx, false)
	if err != nil {
		return zero, err
	}
	var row metadataRow
	if err = db.Table(table).Where(column+" = ?", key).Take(&row).Error; err != nil {
		return zero, translate(err)
	}
	return decodeRow[T](row)
}

func insertValue(ctx context.Context, r *repositories, table string, value any) error {
	db, err := r.access(ctx, true)
	if err != nil {
		return err
	}
	value, err = normalize(value)
	if err != nil {
		return err
	}
	f, err := fields(value)
	if err != nil {
		return err
	}
	if err = db.Table(table).Model(&metadataRow{}).Create(f).Error; err != nil {
		return translate(err)
	}

	return nil
}

func updateValue(ctx context.Context, r *repositories, table string, value any, expected uint64) error {
	db, err := r.access(ctx, true)
	if err != nil {
		return err
	}
	value, err = normalize(value)
	if err != nil {
		return err
	}
	rec := recordOf(value)
	next, err := core.NextRevision(expected)
	if err != nil {
		return err
	}
	if rec.Revision != next {
		return core.ErrConflict
	}
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
	f, err := fields(value)
	if err != nil {
		return err
	}
	delete(f, "id")
	delete(f, "created_at")
	result := db.Table(table).Model(&metadataRow{}).Where("id = ? AND revision = ?", rec.ID, decimal(expected)).Updates(f)
	if result.Error != nil {
		return translate(result.Error)
	}
	if result.RowsAffected != 1 {
		return core.ErrConflict
	}
	return nil
}

func deleteValue(ctx context.Context, r *repositories, table string, id core.ID, expected uint64) error {
	db, err := r.access(ctx, true)
	if err != nil {
		return err
	}
	result := db.Table(table).Where("id = ? AND revision = ?", id, decimal(expected)).Delete(map[string]any{})
	if result.Error != nil {
		return translate(result.Error)
	}
	if result.RowsAffected != 1 {
		var row metadataRow
		if err = db.Table(table).Where("id = ?", id).Take(&row).Error; err != nil {
			return translate(err)
		}
		return core.ErrConflict
	}
	return nil
}

func same(a, b any) bool {
	x, e := core.EncodeMetadata(a)
	y, f := core.EncodeMetadata(b)
	return e == nil && f == nil && bytes.Equal(x, y)
}

func immutable(data []byte, value any) error {
	bad := false
	switch v := value.(type) {
	case core.Stream:
		var old core.Stream
		if err := core.DecodeMetadata(data, &old); err != nil {
			return errors.Join(core.ErrCorrupt, err)
		}
		bad = old.PublicationKey != v.PublicationKey
	case core.FallbackAsset:
		var old core.FallbackAsset
		if err := core.DecodeMetadata(data, &old); err != nil {
			return errors.Join(core.ErrCorrupt, err)
		}
	case core.OwnedFile:
		var old core.OwnedFile
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
		var old core.FallbackGeneration
		if err := core.DecodeMetadata(data, &old); err != nil {
			return errors.Join(core.ErrCorrupt, err)
		}
		bad = old.AssetID != v.AssetID || old.OriginalPath != v.OriginalPath || old.StagingPath != v.StagingPath || old.Kind != v.Kind || old.Incarnation != v.Incarnation || old.AttemptToken != v.AttemptToken || old.GeneratorVersion != v.GeneratorVersion || !same(old.RequiredProfiles, v.RequiredProfiles) || old.RebuildFromGenerationID != v.RebuildFromGenerationID
		if old.State.Terminal() {
			copy := old
			copy.Record = v.Record
			copy.Issued = v.Issued
			copy.CleanupPending = v.CleanupPending
			bad = bad || !same(copy, v)
		}
	case core.FallbackVariant:
		var old core.FallbackVariant
		if err := core.DecodeMetadata(data, &old); err != nil {
			return errors.Join(core.ErrCorrupt, err)
		}
		bad = old.GenerationID != v.GenerationID || old.AssetID != v.AssetID || old.SourceHash != v.SourceHash || old.ManagedPath != v.ManagedPath || old.AttemptToken != v.AttemptToken || old.GeneratorVersion != v.GeneratorVersion || old.ProfileKey != v.ProfileKey || !same(old.Profile, v.Profile) || !bytes.Equal(old.CanonicalProfile, v.CanonicalProfile) || old.Incarnation != v.Incarnation
		if old.State != core.VariantPreparing {
			old.Record = v.Record
			bad = bad || !same(old, v)
		}
	case core.Session:
		var old core.Session
		if err := core.DecodeMetadata(data, &old); err != nil {
			return errors.Join(core.ErrCorrupt, err)
		}
		bad = old.StreamID != v.StreamID || old.Incarnation != v.Incarnation || !same(old.Snapshot, v.Snapshot)
	case core.SessionOutput:
		var old core.SessionOutput
		if err := core.DecodeMetadata(data, &old); err != nil {
			return errors.Join(core.ErrCorrupt, err)
		}
		bad = old.SessionID != v.SessionID || !same(old.Snapshot, v.Snapshot)
	case core.CommandReceipt:
		var old core.CommandReceipt
		if err := core.DecodeMetadata(data, &old); err != nil {
			return errors.Join(core.ErrCorrupt, err)
		}
		bad = old.RequestID != v.RequestID || old.Kind != v.Kind || old.TargetID != v.TargetID || old.Fingerprint != v.Fingerprint || !bytes.Equal(old.Identity, v.Identity) || old.SessionID != v.SessionID || old.OutputID != v.OutputID || old.Incarnation != v.Incarnation
	case core.CleanupIntent:
		var old core.CleanupIntent
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
