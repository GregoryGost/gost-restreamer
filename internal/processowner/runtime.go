package processowner

import (
	"context"
	"errors"
	"path"
)

// RuntimeJournal принадлежит Registry и использует RuntimeRoot как корень путей.
func (s *Scope) RuntimeJournal() *FileJournal { return s.registry.runtime }

func (s *Scope) RuntimeIdentity(relativePath, kind string) (FileIdentity, error) {
	if relativePath != "." && !validManagedPath(relativePath) {
		return FileIdentity{}, ErrInvalid
	}
	owner, err := s.Owner()
	if err != nil {
		return FileIdentity{}, err
	}
	id := FileIdentity{FileID: s.id + ":" + relativePath, GenerationID: s.id, ManagedPath: path.Join(s.id, relativePath), Kind: kind, Owner: owner}
	if err = validateIdentity(id); err != nil {
		return FileIdentity{}, err
	}
	return id, nil
}

// RegisterRuntime учитывает созданный объект до передачи его writer.
// Создание самого объекта должно находиться внутри ранее зарегистрированного backend ticket.
func (s *Scope) RegisterRuntime(ctx context.Context, relativePath, kind string) error {
	id, err := s.RuntimeIdentity(relativePath, kind)
	if err != nil {
		return err
	}
	ticket, err := s.register(ctx, "runtime-resource", nil, &id)
	if err != nil {
		return err
	}
	return errors.Join(s.registry.runtime.Register(ctx, id), ticket.Complete(context.Background()))
}
