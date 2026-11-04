package core

import (
	"context"
	"time"
)

// UnitOfWork ограничивает время жизни репозиториев одним вызовом. Write может
// повторить целиком откатившийся callback; внутри разрешены только операции БД.
type UnitOfWork interface {
	View(context.Context, func(Repositories) error) error
	Write(context.Context, func(Repositories) error) error
}

type Repositories struct {
	Config    ConfigRepository
	Runtime   RuntimeRepository
	Artifacts ArtifactRepository
}

type ConfigRepository interface {
	Stream(context.Context, ID) (Stream, error)
	Streams(context.Context, ListQuery) ([]Stream, error)
	InsertStream(context.Context, Stream) error
	UpdateStream(context.Context, Stream, uint64) error
	DeleteStream(context.Context, ID, uint64) error
	Profile(context.Context, ID) (TranscodeProfile, error)
	Profiles(context.Context, ListQuery) ([]TranscodeProfile, error)
	InsertProfile(context.Context, TranscodeProfile) error
	UpdateProfile(context.Context, TranscodeProfile, uint64) error
	DeleteProfile(context.Context, ID, uint64) error
	Group(context.Context, ID) (TranscodeGroup, error)
	Groups(context.Context, ListQuery) ([]TranscodeGroup, error)
	InsertGroup(context.Context, TranscodeGroup) error
	UpdateGroup(context.Context, TranscodeGroup, uint64) error
	DeleteGroup(context.Context, ID, uint64) error
	Destination(context.Context, ID) (Destination, error)
	Destinations(context.Context, ListQuery) ([]Destination, error)
	InsertDestination(context.Context, Destination) error
	UpdateDestination(context.Context, Destination, uint64) error
	DeleteDestination(context.Context, ID, uint64) error
}

type ArtifactRepository interface {
	Asset(context.Context, ID) (FallbackAsset, error)
	Assets(context.Context, ListQuery) ([]FallbackAsset, error)
	InsertAsset(context.Context, FallbackAsset) error
	UpdateAsset(context.Context, FallbackAsset, uint64) error
	DeleteAsset(context.Context, ID, uint64) error
	Variant(context.Context, ID) (FallbackVariant, error)
	Variants(context.Context, ListQuery) ([]FallbackVariant, error)
	InsertVariant(context.Context, FallbackVariant) error
	UpdateVariant(context.Context, FallbackVariant, uint64) error
	DeleteVariant(context.Context, ID, uint64) error
	InsertCleanup(context.Context, CleanupIntent) error
	Cleanup(context.Context, ID) (CleanupIntent, error)
	Cleanups(context.Context, ListQuery) ([]CleanupIntent, error)
	UpdateCleanup(context.Context, CleanupIntent, uint64) error
}

type RuntimeRepository interface {
	Session(context.Context, ID) (Session, error)
	Sessions(context.Context, ListQuery) ([]Session, error)
	ActiveSession(context.Context, ID) (Session, error)
	InsertSession(context.Context, Session) error
	UpdateSession(context.Context, Session, uint64) error
	Output(context.Context, ID) (SessionOutput, error)
	Outputs(context.Context, ListQuery) ([]SessionOutput, error)
	InsertOutput(context.Context, SessionOutput) error
	UpdateOutput(context.Context, SessionOutput, uint64) error
	Receipt(context.Context, string) (CommandReceipt, error)
	ReceiptByID(context.Context, ID) (CommandReceipt, error)
	Receipts(context.Context, ListQuery) ([]CommandReceipt, error)
	InsertReceipt(context.Context, CommandReceipt) error
	UpdateReceipt(context.Context, CommandReceipt, uint64) error
	InsertEvent(context.Context, Event) error
	Events(context.Context, ListQuery) ([]Event, error)
	InsertHold(context.Context, ReferenceHold) error
	ReleaseHolds(context.Context, ID, ID) error
}

// ListQuery задаёт только известные фильтры и возрастающую границу (CreatedAt, ID).
// Репозиторий материализует не более Limit строк. Пустой OutputID в ReleaseHolds
// освобождает все удержания Session; непустой — только конкретный выход.
type ListQuery struct {
	Limit              int
	StreamID           ID
	SessionID          ID
	OutputID           ID
	AssetID            ID
	GroupID            ID
	ProfileID          ID
	State              string
	Incarnation        string
	ExcludeIncarnation string
	PendingOnly        bool
	From               *time.Time
	To                 *time.Time
	After              *ListBoundary
}
type ListBoundary struct {
	CreatedAt time.Time
	ID        ID
}

const MaxListLimit = 500

func (q ListQuery) Validate() error {
	for _, id := range []ID{q.StreamID, q.SessionID, q.OutputID, q.AssetID, q.GroupID, q.ProfileID} {
		if id != "" {
			if err := id.Validate(); err != nil {
				return err
			}
		}
	}
	for _, value := range []string{q.State, q.Incarnation, q.ExcludeIncarnation} {
		if value != "" {
			if err := validText("ListQuery", value); err != nil {
				return err
			}
		}
	}
	for _, value := range []*time.Time{q.From, q.To} {
		if value != nil {
			if _, err := NormalizeTimestamp(*value); err != nil {
				return err
			}
		}
	}
	if q.After != nil {
		if err := q.After.ID.Validate(); err != nil {
			return err
		}
		if _, err := NormalizeTimestamp(q.After.CreatedAt); err != nil {
			return err
		}
	}
	if q.Limit < 1 || q.Limit > MaxListLimit {
		return ErrInvalid
	}
	if q.From != nil && q.To != nil && !q.From.Before(*q.To) {
		return ErrInvalid
	}
	return nil
}
