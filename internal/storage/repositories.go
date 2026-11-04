package storage

import (
	"context"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func (r *repositories) Stream(ctx context.Context, id core.ID) (core.Stream, error) {
	return getValue[core.Stream](ctx, r, "streams", "id", id)
}
func (r *repositories) Streams(ctx context.Context, q core.ListQuery) ([]core.Stream, error) {
	return listValues[core.Stream](ctx, r, "streams", q)
}
func (r *repositories) InsertStream(ctx context.Context, v core.Stream) error {
	return insertValue(ctx, r, "streams", v)
}
func (r *repositories) UpdateStream(ctx context.Context, v core.Stream, expected uint64) error {
	return updateValue(ctx, r, "streams", v, expected)
}

func (r *repositories) DeleteStream(ctx context.Context, id core.ID, expected uint64) error {
	return deleteValue(ctx, r, "streams", id, expected)
}

func (r *repositories) Profile(ctx context.Context, id core.ID) (core.TranscodeProfile, error) {
	return getValue[core.TranscodeProfile](ctx, r, "profiles", "id", id)
}
func (r *repositories) Profiles(ctx context.Context, q core.ListQuery) ([]core.TranscodeProfile, error) {
	return listValues[core.TranscodeProfile](ctx, r, "profiles", q)
}
func (r *repositories) InsertProfile(ctx context.Context, v core.TranscodeProfile) error {
	return insertValue(ctx, r, "profiles", v)
}
func (r *repositories) UpdateProfile(ctx context.Context, v core.TranscodeProfile, expected uint64) error {
	return updateValue(ctx, r, "profiles", v, expected)
}

func (r *repositories) DeleteProfile(ctx context.Context, id core.ID, expected uint64) error {
	return deleteValue(ctx, r, "profiles", id, expected)
}

func (r *repositories) Group(ctx context.Context, id core.ID) (core.TranscodeGroup, error) {
	return getValue[core.TranscodeGroup](ctx, r, "groups", "id", id)
}
func (r *repositories) Groups(ctx context.Context, q core.ListQuery) ([]core.TranscodeGroup, error) {
	return listValues[core.TranscodeGroup](ctx, r, "groups", q)
}
func (r *repositories) InsertGroup(ctx context.Context, v core.TranscodeGroup) error {
	return insertValue(ctx, r, "groups", v)
}
func (r *repositories) UpdateGroup(ctx context.Context, v core.TranscodeGroup, expected uint64) error {
	return updateValue(ctx, r, "groups", v, expected)
}

func (r *repositories) DeleteGroup(ctx context.Context, id core.ID, expected uint64) error {
	return deleteValue(ctx, r, "groups", id, expected)
}

func (r *repositories) Destination(ctx context.Context, id core.ID) (core.Destination, error) {
	return getValue[core.Destination](ctx, r, "destinations", "id", id)
}
func (r *repositories) Destinations(ctx context.Context, q core.ListQuery) ([]core.Destination, error) {
	return listValues[core.Destination](ctx, r, "destinations", q)
}
func (r *repositories) InsertDestination(ctx context.Context, v core.Destination) error {
	return insertValue(ctx, r, "destinations", v)
}
func (r *repositories) UpdateDestination(ctx context.Context, v core.Destination, expected uint64) error {
	return updateValue(ctx, r, "destinations", v, expected)
}

func (r *repositories) DeleteDestination(ctx context.Context, id core.ID, expected uint64) error {
	return deleteValue(ctx, r, "destinations", id, expected)
}

func (r *repositories) Asset(ctx context.Context, id core.ID) (core.FallbackAsset, error) {
	return getValue[core.FallbackAsset](ctx, r, "fallback_assets", "id", id)
}
func (r *repositories) Assets(ctx context.Context, q core.ListQuery) ([]core.FallbackAsset, error) {
	return listValues[core.FallbackAsset](ctx, r, "fallback_assets", q)
}
func (r *repositories) InsertAsset(ctx context.Context, v core.FallbackAsset) error {
	return insertValue(ctx, r, "fallback_assets", v)
}
func (r *repositories) UpdateAsset(ctx context.Context, v core.FallbackAsset, expected uint64) error {
	return updateValue(ctx, r, "fallback_assets", v, expected)
}

func (r *repositories) DeleteAsset(ctx context.Context, id core.ID, expected uint64) error {
	return deleteValue(ctx, r, "fallback_assets", id, expected)
}

func (r *repositories) Variant(ctx context.Context, id core.ID) (core.FallbackVariant, error) {
	return getValue[core.FallbackVariant](ctx, r, "fallback_variants", "id", id)
}
func (r *repositories) Variants(ctx context.Context, q core.ListQuery) ([]core.FallbackVariant, error) {
	return listValues[core.FallbackVariant](ctx, r, "fallback_variants", q)
}
func (r *repositories) InsertVariant(ctx context.Context, v core.FallbackVariant) error {
	return insertValue(ctx, r, "fallback_variants", v)
}
func (r *repositories) UpdateVariant(ctx context.Context, v core.FallbackVariant, expected uint64) error {
	return updateValue(ctx, r, "fallback_variants", v, expected)
}

func (r *repositories) DeleteVariant(ctx context.Context, id core.ID, expected uint64) error {
	return deleteValue(ctx, r, "fallback_variants", id, expected)
}

func (r *repositories) Session(ctx context.Context, id core.ID) (core.Session, error) {
	return getValue[core.Session](ctx, r, "sessions", "id", id)
}
func (r *repositories) Sessions(ctx context.Context, q core.ListQuery) ([]core.Session, error) {
	return listValues[core.Session](ctx, r, "sessions", q)
}
func (r *repositories) InsertSession(ctx context.Context, v core.Session) error {
	return insertValue(ctx, r, "sessions", v)
}
func (r *repositories) UpdateSession(ctx context.Context, v core.Session, expected uint64) error {
	return updateValue(ctx, r, "sessions", v, expected)
}

func (r *repositories) Output(ctx context.Context, id core.ID) (core.SessionOutput, error) {
	return getValue[core.SessionOutput](ctx, r, "outputs", "id", id)
}
func (r *repositories) Outputs(ctx context.Context, q core.ListQuery) ([]core.SessionOutput, error) {
	return listValues[core.SessionOutput](ctx, r, "outputs", q)
}
func (r *repositories) InsertOutput(ctx context.Context, v core.SessionOutput) error {
	return insertValue(ctx, r, "outputs", v)
}
func (r *repositories) UpdateOutput(ctx context.Context, v core.SessionOutput, expected uint64) error {
	return updateValue(ctx, r, "outputs", v, expected)
}

func (r *repositories) ReceiptByID(ctx context.Context, id core.ID) (core.CommandReceipt, error) {
	return getValue[core.CommandReceipt](ctx, r, "command_receipts", "id", id)
}
func (r *repositories) Receipts(ctx context.Context, q core.ListQuery) ([]core.CommandReceipt, error) {
	return listValues[core.CommandReceipt](ctx, r, "command_receipts", q)
}
func (r *repositories) InsertReceipt(ctx context.Context, v core.CommandReceipt) error {
	return insertValue(ctx, r, "command_receipts", v)
}
func (r *repositories) UpdateReceipt(ctx context.Context, v core.CommandReceipt, expected uint64) error {
	return updateValue(ctx, r, "command_receipts", v, expected)
}

func (r *repositories) Cleanup(ctx context.Context, id core.ID) (core.CleanupIntent, error) {
	return getValue[core.CleanupIntent](ctx, r, "cleanup_intents", "id", id)
}
func (r *repositories) Cleanups(ctx context.Context, q core.ListQuery) ([]core.CleanupIntent, error) {
	return listValues[core.CleanupIntent](ctx, r, "cleanup_intents", q)
}
func (r *repositories) InsertCleanup(ctx context.Context, v core.CleanupIntent) error {
	return insertValue(ctx, r, "cleanup_intents", v)
}
func (r *repositories) UpdateCleanup(ctx context.Context, v core.CleanupIntent, expected uint64) error {
	return updateValue(ctx, r, "cleanup_intents", v, expected)
}

func (r *repositories) Receipt(ctx context.Context, requestID string) (core.CommandReceipt, error) {
	return getValue[core.CommandReceipt](ctx, r, "command_receipts", "request_id", requestID)
}
func (r *repositories) ActiveSession(ctx context.Context, streamID core.ID) (core.Session, error) {
	db, err := r.access(ctx, false)
	if err != nil {
		return core.Session{}, err
	}
	var row metadataRow
	err = db.Table("sessions").Where("stream_id = ? AND state IN ?", streamID, []string{"STARTING", "RUNNING", "STOPPING"}).Take(&row).Error
	if err != nil {
		return core.Session{}, translate(err)
	}
	return decodeRow[core.Session](row)
}
func (r *repositories) InsertEvent(ctx context.Context, v core.Event) error {
	return insertValue(ctx, r, "events", v)
}
func (r *repositories) Events(ctx context.Context, q core.ListQuery) ([]core.Event, error) {
	return listValues[core.Event](ctx, r, "events", q)
}
func (r *repositories) InsertHold(ctx context.Context, v core.ReferenceHold) error {
	db, err := r.access(ctx, true)
	if err != nil {
		return err
	}
	fields := map[string]any{"session_id": v.SessionID, "stream_id": v.StreamID, "asset_id": v.AssetID}
	table := "session_holds"
	if v.OutputID != "" {
		table = "output_holds"
		fields["output_id"] = v.OutputID
		fields["group_id"] = v.GroupID
		fields["profile_id"] = v.ProfileID
		fields["destination_id"] = v.DestinationID
		fields["variant_id"] = v.VariantID
	}
	return translate(db.Table(table).Create(fields).Error)
}
func (r *repositories) ReleaseHolds(ctx context.Context, sessionID, outputID core.ID) error {
	if sessionID == "" {
		return core.ErrInvalid
	}
	db, err := r.access(ctx, true)
	if err != nil {
		return err
	}
	query := db.Table("output_holds").Where("session_id = ?", sessionID)
	if outputID != "" {
		return translate(query.Where("output_id = ?", outputID).Delete(map[string]any{}).Error)
	}
	if err = query.Delete(map[string]any{}).Error; err != nil {
		return translate(err)
	}
	return translate(db.Table("session_holds").Where("session_id = ?", sessionID).Delete(map[string]any{}).Error)
}
