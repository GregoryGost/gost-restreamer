package fallback

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

func fileIdentity(file core.OwnedFile) processowner.FileIdentity {
	kind := processowner.RegularFile
	if file.Kind == core.OwnedDirectory {
		kind = processowner.Directory
	}
	owner, _ := json.Marshal(struct {
		AssetID core.ID
		Kind    core.OwnedFileKind
	}{file.AssetID, file.Kind})
	return processowner.FileIdentity{FileID: string(file.ID), GenerationID: string(file.GenerationID), ManagedPath: file.ManagedPath, Kind: kind, Owner: string(owner)}
}

// FileJournal возвращает общий журнал файлов, который закрывается вместе с FileStore.
func (s *FileStore) FileJournal() *processowner.FileJournal { return s.evidence }

// ReserveOwnedContext сохраняет точную запись реестра до возможного файлового действия.
func (s *FileStore) ReserveOwnedContext(ctx context.Context, file core.OwnedFile) error {
	if file.Validate() != nil || file.ID == "" {
		return ErrInvalid
	}
	return s.evidence.Reserve(ctx, fileIdentity(file))
}

// CreateOwned устанавливает долговечную служебную ссылку до передачи файла для записи.
func (s *FileStore) CreateOwned(file core.OwnedFile) (*os.File, error) {
	return s.CreateOwnedContext(context.Background(), file)
}

func (s *FileStore) CreateOwnedContext(ctx context.Context, file core.OwnedFile) (*os.File, error) {
	if file.Validate() != nil || file.ID == "" || file.Kind == core.OwnedDirectory {
		return nil, ErrInvalid
	}
	if err := s.ReserveOwnedContext(ctx, file); err != nil {
		return nil, err
	}
	f, err := s.Create(file.ManagedPath)
	if err != nil {
		return nil, err
	}
	if err = s.evidence.Register(ctx, fileIdentity(file)); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// CreateOwnedGeneration допускает дочерние объекты после установки долговечного маркера со ссылкой.
func (s *FileStore) CreateOwnedGeneration(file core.OwnedFile) error {
	return s.CreateOwnedGenerationContext(context.Background(), file)
}

func (s *FileStore) CreateOwnedGenerationContext(ctx context.Context, file core.OwnedFile) error {
	if file.Validate() != nil || file.ID == "" || file.Kind != core.OwnedDirectory || file.ManagedPath != core.GenerationDirectory(file.GenerationID) {
		return ErrInvalid
	}
	if err := s.ReserveOwnedContext(ctx, file); err != nil {
		return err
	}
	if err := s.CreateGeneration(file.GenerationID); err != nil {
		return err
	}
	return s.evidence.Register(ctx, fileIdentity(file))
}

// PublishOwnedRename фиксирует связь обоих FileID до изменения записей каталогов.
func (s *FileStore) PublishOwnedRename(staging, final core.OwnedFile) error {
	return s.PublishOwnedRenameContext(context.Background(), staging, final)
}

func (s *FileStore) PublishOwnedRenameContext(ctx context.Context, staging, final core.OwnedFile) error {
	if staging.Validate() != nil || final.Validate() != nil || staging.ID == "" || final.ID == "" {
		return ErrInvalid
	}
	known := s.remembered(staging.ManagedPath)
	if err := s.evidence.Rename(ctx, fileIdentity(staging), fileIdentity(final)); err != nil {
		return err
	}
	s.forget(staging.ManagedPath, known)
	return nil
}

// AcknowledgeOwned освобождает служебную ссылку после долговечного подтверждения FILE.
func (s *FileStore) AcknowledgeOwned(file core.OwnedFile) error {
	return s.AcknowledgeOwnedContext(context.Background(), file)
}

func (s *FileStore) AcknowledgeOwnedContext(ctx context.Context, file core.OwnedFile) error {
	if file.ID == "" {
		return nil
	}
	exists, err := s.evidence.Exists(ctx, fileIdentity(file))
	if !exists {
		return err
	}
	known := s.remembered(file.ManagedPath)
	if err := s.evidence.Acknowledge(ctx, fileIdentity(file)); err != nil {
		return err
	}
	// Повторный ACKED может встретить новый объект по прежнему пути.
	if _, err := s.root.Lstat(file.ManagedPath); errors.Is(err, os.ErrNotExist) {
		s.forget(file.ManagedPath, known)
	}
	return nil
}
