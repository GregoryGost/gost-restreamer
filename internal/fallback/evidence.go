package fallback

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// Переносит точную запись OwnedFile в идентичность файлового журнала, включая поколение и владельца
// Asset.
func fileIdentity(file core.OwnedFile) processowner.FileIdentity {
	// kind содержит вид файлового объекта в журнале processowner.
	kind := processowner.RegularFile
	if file.Kind == core.OwnedDirectory {
		kind = processowner.Directory
	}
	// owner содержит JSON точного владельца Asset и вида файла для сопоставления журнальных записей.
	owner, _ := json.Marshal(struct {
		// AssetID обозначает Asset, которому принадлежит файловый объект.
		AssetID core.ID
		// Kind сохраняет доменный вид файлового объекта в идентичности владельца.
		Kind core.OwnedFileKind
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

// CreateOwnedContext резервирует идентичность, создаёт новый файл и регистрирует служебную ссылку до
// возврата дескриптора вызывающему. При ошибке регистрации дескриптор закрывается, а следы остаются
// для очистки.
func (s *FileStore) CreateOwnedContext(ctx context.Context, file core.OwnedFile) (*os.File, error) {
	if file.Validate() != nil || file.ID == "" || file.Kind == core.OwnedDirectory {
		return nil, ErrInvalid
	}
	// err сообщает об отказе резервирования идентичности файла до его создания.
	if err := s.ReserveOwnedContext(ctx, file); err != nil {
		return nil, err
	}
	// f содержит новый файловый дескриптор, передаваемый вызывающему после регистрации
	// доказательства. err сообщает об отказе создания.
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

// CreateOwnedGenerationContext создаёт каталог только по точному пути поколения после резервирования
// его идентичности и регистрирует доказательство владения.
func (s *FileStore) CreateOwnedGenerationContext(ctx context.Context, file core.OwnedFile) error {
	if file.Validate() != nil || file.ID == "" || file.Kind != core.OwnedDirectory || file.ManagedPath != core.GenerationDirectory(file.GenerationID) {
		return ErrInvalid
	}
	// err сообщает об отказе резервирования идентичности каталога поколения.
	if err := s.ReserveOwnedContext(ctx, file); err != nil {
		return err
	}
	// err сообщает об отказе создания каталога поколения.
	if err := s.CreateGeneration(file.GenerationID); err != nil {
		return err
	}
	return s.evidence.Register(ctx, fileIdentity(file))
}

// PublishOwnedRename фиксирует связь обоих FileID до изменения записей каталогов.
func (s *FileStore) PublishOwnedRename(staging, final core.OwnedFile) error {
	return s.PublishOwnedRenameContext(context.Background(), staging, final)
}

// PublishOwnedRenameContext передаёт журналу обе идентичности для переименования; после успеха
// забывает прежнее наблюдение исходного пути.
func (s *FileStore) PublishOwnedRenameContext(ctx context.Context, staging, final core.OwnedFile) error {
	if staging.Validate() != nil || final.Validate() != nil || staging.ID == "" || final.ID == "" {
		return ErrInvalid
	}
	// known сохраняет прежнее локальное наблюдение исходного пути для удаления только этой записи
	// после переименования.
	known := s.remembered(staging.ManagedPath)
	// err сообщает об отказе журналируемого переименования исходного файла в конечный.
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

// AcknowledgeOwnedContext освобождает доказательство после подтверждения FILE; отсутствие записи
// допустимо. Запомненное наблюдение удаляется лишь при отсутствии самого пути.
func (s *FileStore) AcknowledgeOwnedContext(ctx context.Context, file core.OwnedFile) error {
	if file.ID == "" {
		return nil
	}
	// exists показывает наличие доказательства этой идентичности в журнале. err сообщает об отказе
	// проверки доказательства.
	exists, err := s.evidence.Exists(ctx, fileIdentity(file))
	if !exists {
		return err
	}
	// known сохраняет наблюдение пути до подтверждения освобождения доказательства.
	known := s.remembered(file.ManagedPath)
	// err сообщает об отказе освобождения журнального доказательства файла.
	if err := s.evidence.Acknowledge(ctx, fileIdentity(file)); err != nil {
		return err
	}
	// Повторный ACKED может встретить новый объект по прежнему пути.
	// err содержит результат проверки существования пути после подтверждения, чтобы не забыть новый
	// объект на прежнем пути.
	if _, err := s.root.Lstat(file.ManagedPath); errors.Is(err, os.ErrNotExist) {
		s.forget(file.ManagedPath, known)
	}
	return nil
}
