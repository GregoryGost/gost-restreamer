package core

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

const MaxPageLimit = MaxListLimit - 1

type PageRequest struct {
	Query  ListQuery
	Cursor string
}
type Page[T any] struct {
	Items      []T
	NextCursor string
}
type pageCursor struct {
	Version  int
	Kind     string
	Filter   ListQuery
	Boundary ListBoundary
}

// NormalizeTimestamp проверяет диапазон микросекунд Unix и убирает локальную
// зону и доли микросекунды перед сохранением или сравнением.
func NormalizeTimestamp(value time.Time) (time.Time, error) {
	normalized := value.UTC().Truncate(time.Microsecond)
	decoded := time.UnixMicro(normalized.UnixMicro()).UTC()
	if !decoded.Equal(normalized) {
		return time.Time{}, fmt.Errorf("%w: время вне диапазона микросекунд", ErrInvalid)
	}
	return decoded, nil
}
func normalizeQuery(q ListQuery) (ListQuery, error) {
	var err error
	if q.From != nil {
		if _, e := NormalizeTimestamp(*q.From); e != nil {
			return ListQuery{}, e
		}
		value := q.From.UTC()
		q.From = &value
	}
	if q.To != nil {
		if _, e := NormalizeTimestamp(*q.To); e != nil {
			return ListQuery{}, e
		}
		value := q.To.UTC()
		q.To = &value
	}
	if q.After != nil {
		value, e := NormalizeTimestamp(q.After.CreatedAt)
		if e != nil {
			return ListQuery{}, e
		}
		if e = q.After.ID.Validate(); e != nil {
			return ListQuery{}, e
		}
		q.After = &ListBoundary{value, q.After.ID}
	}
	err = q.Validate()
	return q, err
}
func pageQuery(kind string, request PageRequest) (ListQuery, error) {
	if request.Query.After != nil || request.Query.Limit < 1 || request.Query.Limit > MaxPageLimit {
		return ListQuery{}, ErrInvalid
	}
	q, err := normalizeQuery(request.Query)
	if err != nil {
		return ListQuery{}, err
	}
	if request.Cursor != "" {
		if len(request.Cursor) > 32768 {
			return ListQuery{}, ErrInvalidCursor
		}
		data, err := base64.RawURLEncoding.DecodeString(request.Cursor)
		if err != nil {
			return ListQuery{}, ErrInvalidCursor
		}
		var c pageCursor
		if err = strictJSON(data, &c); err != nil || c.Version != MetadataVersion || c.Kind != kind {
			return ListQuery{}, ErrInvalidCursor
		}
		filter := q
		filter.Limit = 0
		expected, err := json.Marshal(filter)
		if err != nil {
			return ListQuery{}, err
		}
		actual, err := json.Marshal(c.Filter)
		if err != nil {
			return ListQuery{}, err
		}
		if !bytes.Equal(expected, actual) {
			return ListQuery{}, ErrInvalidCursor
		}
		if c.Boundary.ID.Validate() != nil {
			return ListQuery{}, ErrInvalidCursor
		}
		boundaryTime, err := NormalizeTimestamp(c.Boundary.CreatedAt)
		if err != nil || !boundaryTime.Equal(c.Boundary.CreatedAt) {
			return ListQuery{}, ErrInvalidCursor
		}
		q.After = &c.Boundary
	}
	q.Limit++
	return q, nil
}
func nextCursor(kind string, q ListQuery, boundary ListBoundary) (string, error) {
	q.Limit = 0
	q.After = nil
	data, err := json.Marshal(pageCursor{MetadataVersion, kind, q, boundary})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
func readPage[T any](ctx context.Context, s *Service, kind string, request PageRequest, read func(Repositories, ListQuery) ([]T, error), boundary func(T) ListBoundary) (Page[T], error) {
	q, err := pageQuery(kind, request)
	if err != nil {
		return Page[T]{}, err
	}
	var values []T
	err = s.uow.View(ctx, func(r Repositories) error { var err error; values, err = read(r, q); return err })
	if err != nil {
		return Page[T]{}, err
	}
	result := Page[T]{Items: values}
	if len(values) > request.Query.Limit {
		result.Items = values[:request.Query.Limit]
		result.NextCursor, err = nextCursor(kind, q, boundary(result.Items[len(result.Items)-1]))
		if err != nil {
			return Page[T]{}, err
		}
	}
	return result, nil
}

func (s *Service) AuthorizeOBS(ctx context.Context, streamID ID, key string) (Session, error) {
	var result Session
	err := s.uow.View(ctx, func(r Repositories) error {
		session, err := r.Runtime.ActiveSession(ctx, streamID)
		if err != nil {
			return err
		}
		if session.State != SessionRunning || subtle.ConstantTimeCompare([]byte(session.Snapshot.Stream.PublicationKey), []byte(key)) != 1 {
			return ErrNotReady
		}
		result = session
		return nil
	})
	return result, err
}

func (s *Service) GetStream(ctx context.Context, id ID) (Stream, error) {
	var result Stream
	err := s.uow.View(ctx, func(r Repositories) error { var err error; result, err = r.Config.Stream(ctx, id); return err })
	return result, err
}
func (s *Service) ListStreams(ctx context.Context, request PageRequest) (Page[Stream], error) {
	return readPage(ctx, s, "streams", request, func(r Repositories, q ListQuery) ([]Stream, error) { return r.Config.Streams(ctx, q) }, func(value Stream) ListBoundary { return ListBoundary{value.CreatedAt, value.ID} })
}

func (s *Service) GetProfile(ctx context.Context, id ID) (TranscodeProfile, error) {
	var result TranscodeProfile
	err := s.uow.View(ctx, func(r Repositories) error { var err error; result, err = r.Config.Profile(ctx, id); return err })
	return result, err
}
func (s *Service) ListProfiles(ctx context.Context, request PageRequest) (Page[TranscodeProfile], error) {
	return readPage(ctx, s, "profiles", request, func(r Repositories, q ListQuery) ([]TranscodeProfile, error) { return r.Config.Profiles(ctx, q) }, func(value TranscodeProfile) ListBoundary { return ListBoundary{value.CreatedAt, value.ID} })
}

func (s *Service) GetGroup(ctx context.Context, id ID) (TranscodeGroup, error) {
	var result TranscodeGroup
	err := s.uow.View(ctx, func(r Repositories) error { var err error; result, err = r.Config.Group(ctx, id); return err })
	return result, err
}
func (s *Service) ListGroups(ctx context.Context, request PageRequest) (Page[TranscodeGroup], error) {
	return readPage(ctx, s, "groups", request, func(r Repositories, q ListQuery) ([]TranscodeGroup, error) { return r.Config.Groups(ctx, q) }, func(value TranscodeGroup) ListBoundary { return ListBoundary{value.CreatedAt, value.ID} })
}

func (s *Service) GetDestination(ctx context.Context, id ID) (Destination, error) {
	var result Destination
	err := s.uow.View(ctx, func(r Repositories) error { var err error; result, err = r.Config.Destination(ctx, id); return err })
	return result, err
}
func (s *Service) ListDestinations(ctx context.Context, request PageRequest) (Page[Destination], error) {
	return readPage(ctx, s, "destinations", request, func(r Repositories, q ListQuery) ([]Destination, error) { return r.Config.Destinations(ctx, q) }, func(value Destination) ListBoundary { return ListBoundary{value.CreatedAt, value.ID} })
}

func (s *Service) GetAsset(ctx context.Context, id ID) (FallbackAsset, error) {
	var result FallbackAsset
	err := s.uow.View(ctx, func(r Repositories) error { var err error; result, err = r.Artifacts.Asset(ctx, id); return err })
	return result, err
}
func (s *Service) ListAssets(ctx context.Context, request PageRequest) (Page[FallbackAsset], error) {
	return readPage(ctx, s, "assets", request, func(r Repositories, q ListQuery) ([]FallbackAsset, error) { return r.Artifacts.Assets(ctx, q) }, func(value FallbackAsset) ListBoundary { return ListBoundary{value.CreatedAt, value.ID} })
}

func (s *Service) GetVariant(ctx context.Context, id ID) (FallbackVariant, error) {
	var result FallbackVariant
	err := s.uow.View(ctx, func(r Repositories) error { var err error; result, err = r.Artifacts.Variant(ctx, id); return err })
	return result, err
}
func (s *Service) ListVariants(ctx context.Context, request PageRequest) (Page[FallbackVariant], error) {
	return readPage(ctx, s, "variants", request, func(r Repositories, q ListQuery) ([]FallbackVariant, error) { return r.Artifacts.Variants(ctx, q) }, func(value FallbackVariant) ListBoundary { return ListBoundary{value.CreatedAt, value.ID} })
}

func (s *Service) GetSession(ctx context.Context, id ID) (Session, error) {
	var result Session
	err := s.uow.View(ctx, func(r Repositories) error { var err error; result, err = r.Runtime.Session(ctx, id); return err })
	return result, err
}
func (s *Service) ListSessions(ctx context.Context, request PageRequest) (Page[Session], error) {
	return readPage(ctx, s, "sessions", request, func(r Repositories, q ListQuery) ([]Session, error) { return r.Runtime.Sessions(ctx, q) }, func(value Session) ListBoundary { return ListBoundary{value.CreatedAt, value.ID} })
}

func (s *Service) GetOutput(ctx context.Context, id ID) (SessionOutput, error) {
	var result SessionOutput
	err := s.uow.View(ctx, func(r Repositories) error { var err error; result, err = r.Runtime.Output(ctx, id); return err })
	return result, err
}
func (s *Service) ListOutputs(ctx context.Context, request PageRequest) (Page[SessionOutput], error) {
	if request.Query.State != "" {
		return Page[SessionOutput]{}, ErrInvalid
	}
	return readPage(ctx, s, "outputs", request, func(r Repositories, q ListQuery) ([]SessionOutput, error) { return r.Runtime.Outputs(ctx, q) }, func(value SessionOutput) ListBoundary { return ListBoundary{value.CreatedAt, value.ID} })
}

func (s *Service) GetReceipt(ctx context.Context, id ID) (CommandReceipt, error) {
	var result CommandReceipt
	err := s.uow.View(ctx, func(r Repositories) error { var err error; result, err = r.Runtime.ReceiptByID(ctx, id); return err })
	return result, err
}
func (s *Service) ListReceipts(ctx context.Context, request PageRequest) (Page[CommandReceipt], error) {
	return readPage(ctx, s, "receipts", request, func(r Repositories, q ListQuery) ([]CommandReceipt, error) { return r.Runtime.Receipts(ctx, q) }, func(value CommandReceipt) ListBoundary { return ListBoundary{value.CreatedAt, value.ID} })
}

func (s *Service) GetCleanup(ctx context.Context, id ID) (CleanupIntent, error) {
	var result CleanupIntent
	err := s.uow.View(ctx, func(r Repositories) error { var err error; result, err = r.Artifacts.Cleanup(ctx, id); return err })
	return result, err
}
func (s *Service) ListCleanups(ctx context.Context, request PageRequest) (Page[CleanupIntent], error) {
	return readPage(ctx, s, "cleanups", request, func(r Repositories, q ListQuery) ([]CleanupIntent, error) { return r.Artifacts.Cleanups(ctx, q) }, func(value CleanupIntent) ListBoundary { return ListBoundary{value.CreatedAt, value.ID} })
}

func (s *Service) ListEvents(ctx context.Context, request PageRequest) (Page[Event], error) {
	return readPage(ctx, s, "events", request, func(r Repositories, q ListQuery) ([]Event, error) { return r.Runtime.Events(ctx, q) }, func(value Event) ListBoundary { return ListBoundary{value.CreatedAt, value.ID} })
}
func (s *Service) FindReceipt(ctx context.Context, requestID string) (CommandReceipt, error) {
	var result CommandReceipt
	err := s.uow.View(ctx, func(r Repositories) error { var err error; result, err = r.Runtime.Receipt(ctx, requestID); return err })
	return result, err
}
