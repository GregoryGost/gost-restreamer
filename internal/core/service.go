package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Service принимает решения только в коротких транзакциях БД. Incarnation
// принадлежит владельцу хранилища и меняется при каждом открытии процесса.
type Service struct {
	uow         UnitOfWork
	incarnation string
}

func NewService(uow UnitOfWork, incarnation string) (*Service, error) {
	if uow == nil {
		return nil, fmt.Errorf("%w: отсутствует UnitOfWork", ErrInvalid)
	}
	if err := validText("incarnation", incarnation); err != nil {
		return nil, err
	}
	return &Service{uow: uow, incarnation: incarnation}, nil
}
func newID() (ID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("создание идентификатора: %w", err)
	}
	return ID(hex.EncodeToString(b[:])), nil
}
func newRecord() (Record, error) {
	id, err := newID()
	if err != nil {
		return Record{}, err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	return Record{ID: id, Revision: 1, CreatedAt: now, UpdatedAt: now}, nil
}
func revise(r Record) (Record, error) {
	next, err := NextRevision(r.Revision)
	if err != nil {
		return Record{}, err
	}
	r.Revision = next
	r.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
	return r, nil
}
func checkRevision(actual, expected uint64) error {
	if actual != expected {
		return ErrConflict
	}
	return nil
}

func (s *Service) CreateStream(ctx context.Context, value Stream) (Stream, error) {
	if err := value.Validate(); err != nil {
		return Stream{}, err
	}
	record, err := newRecord()
	if err != nil {
		return Stream{}, err
	}
	value.Record = record
	err = s.uow.Write(ctx, func(r Repositories) error { return r.Config.InsertStream(ctx, value) })
	return value, err
}
func (s *Service) UpdateStream(ctx context.Context, value Stream, expected uint64) (Stream, error) {
	if err := value.Validate(); err != nil {
		return Stream{}, err
	}
	err := s.uow.Write(ctx, func(r Repositories) error {
		current, err := r.Config.Stream(ctx, value.ID)
		if err != nil {
			return err
		}
		if err = checkRevision(current.Revision, expected); err != nil {
			return err
		}
		if value.PublicationKey != current.PublicationKey {
			return fmt.Errorf("%w: ключ публикации неизменяем", ErrConflict)
		}
		value.Record, err = revise(current.Record)
		if err != nil {
			return err
		}
		return r.Config.UpdateStream(ctx, value, expected)
	})
	return value, err
}
func (s *Service) DeleteStream(ctx context.Context, id ID, expected uint64) error {
	return s.uow.Write(ctx, func(r Repositories) error { return r.Config.DeleteStream(ctx, id, expected) })
}

func (s *Service) CreateProfile(ctx context.Context, value TranscodeProfile) (TranscodeProfile, error) {
	if err := value.Validate(); err != nil {
		return TranscodeProfile{}, err
	}
	record, err := newRecord()
	if err != nil {
		return TranscodeProfile{}, err
	}
	value.Record = record
	err = s.uow.Write(ctx, func(r Repositories) error { return r.Config.InsertProfile(ctx, value) })
	return value, err
}
func (s *Service) UpdateProfile(ctx context.Context, value TranscodeProfile, expected uint64) (TranscodeProfile, error) {
	if err := value.Validate(); err != nil {
		return TranscodeProfile{}, err
	}
	err := s.uow.Write(ctx, func(r Repositories) error {
		current, err := r.Config.Profile(ctx, value.ID)
		if err != nil {
			return err
		}
		if err = checkRevision(current.Revision, expected); err != nil {
			return err
		}
		value.Record, err = revise(current.Record)
		if err != nil {
			return err
		}
		return r.Config.UpdateProfile(ctx, value, expected)
	})
	return value, err
}
func (s *Service) DeleteProfile(ctx context.Context, id ID, expected uint64) error {
	return s.uow.Write(ctx, func(r Repositories) error { return r.Config.DeleteProfile(ctx, id, expected) })
}

func (s *Service) CreateGroup(ctx context.Context, value TranscodeGroup) (TranscodeGroup, error) {
	if err := value.Validate(); err != nil {
		return TranscodeGroup{}, err
	}
	record, err := newRecord()
	if err != nil {
		return TranscodeGroup{}, err
	}
	value.Record = record
	err = s.uow.Write(ctx, func(r Repositories) error { return r.Config.InsertGroup(ctx, value) })
	return value, err
}
func (s *Service) UpdateGroup(ctx context.Context, value TranscodeGroup, expected uint64) (TranscodeGroup, error) {
	if err := value.Validate(); err != nil {
		return TranscodeGroup{}, err
	}
	err := s.uow.Write(ctx, func(r Repositories) error {
		current, err := r.Config.Group(ctx, value.ID)
		if err != nil {
			return err
		}
		if err = checkRevision(current.Revision, expected); err != nil {
			return err
		}
		value.Record, err = revise(current.Record)
		if err != nil {
			return err
		}
		return r.Config.UpdateGroup(ctx, value, expected)
	})
	return value, err
}
func (s *Service) DeleteGroup(ctx context.Context, id ID, expected uint64) error {
	return s.uow.Write(ctx, func(r Repositories) error { return r.Config.DeleteGroup(ctx, id, expected) })
}

func (s *Service) CreateDestination(ctx context.Context, value Destination) (Destination, error) {
	if err := value.Validate(); err != nil {
		return Destination{}, err
	}
	record, err := newRecord()
	if err != nil {
		return Destination{}, err
	}
	value.Record = record
	err = s.uow.Write(ctx, func(r Repositories) error { return r.Config.InsertDestination(ctx, value) })
	return value, err
}
func (s *Service) UpdateDestination(ctx context.Context, value Destination, expected uint64) (Destination, error) {
	if err := value.Validate(); err != nil {
		return Destination{}, err
	}
	err := s.uow.Write(ctx, func(r Repositories) error {
		current, err := r.Config.Destination(ctx, value.ID)
		if err != nil {
			return err
		}
		if err = checkRevision(current.Revision, expected); err != nil {
			return err
		}
		value.Record, err = revise(current.Record)
		if err != nil {
			return err
		}
		return r.Config.UpdateDestination(ctx, value, expected)
	})
	return value, err
}
func (s *Service) DeleteDestination(ctx context.Context, id ID, expected uint64) error {
	return s.uow.Write(ctx, func(r Repositories) error { return r.Config.DeleteDestination(ctx, id, expected) })
}
