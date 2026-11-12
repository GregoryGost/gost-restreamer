package core

import (
	"context"
	"errors"
)

// AuthorizeFileCleanup сверяет долговечное намерение до любого удаления файла.
// Удалённое поколение не может снова стать current или получить новые удержания.
func (s *Service) AuthorizeFileCleanup(ctx context.Context, f CleanupFence) (OwnedFile, error) {
	// result принимает запись владения файлом после разрешения точной очистки.
	var result OwnedFile
	// err содержит причину отказа согласованной проверки разрешения удаления.
	err := s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении сверяет точный CleanupFence и отсутствие подтверждения, затем проверяет
		// журнал файлов, удержания и принадлежность файла перед разрешением удаления.

		// i содержит сохранённое намерение очистки, а e — причину отказа его чтения.
		i, e := r.Artifacts.Cleanup(ctx, f.IntentID)
		if e != nil {
			return e
		}
		if i.Kind != CleanupFile || CleanupFenceFor(i) != f {
			return ErrStale
		}
		if i.Acknowledged {
			return ErrStale
		}
		result, e = authorizeFileCleanup(ctx, r, i)
		return e
	})
	return result, err
}

// authorizeFileCleanup запрещает очистку удерживаемого, активного или выбранного поколения и находит
// точную запись происхождения файла; несовпадение пути, вида, размера или хеша считается повреждением.
func authorizeFileCleanup(ctx context.Context, r Repositories, i CleanupIntent) (OwnedFile, error) {
	// held показывает наличие удержаний поколения, а e — причину отказа их проверки.
	held, e := r.Runtime.GenerationHeld(ctx, i.ArtifactGenerationID)
	if e != nil {
		return OwnedFile{}, e
	}
	if held {
		return OwnedFile{}, ErrInUse
	}
	// g содержит поколение удаляемого файла, если его метаданные ещё существуют.
	g, e := r.Artifacts.Generation(ctx, i.ArtifactGenerationID)
	if e != nil && !errors.Is(e, ErrNotFound) {
		return OwnedFile{}, e
	}
	if e == nil {
		if g.State.Active() || g.Issued || g.CleanupPending {
			return OwnedFile{}, ErrInUse
		}
		// a содержит заглушку для проверки current/pending, а e — причину отказа её чтения.
		a, e := r.Artifacts.Asset(ctx, g.AssetID)
		if e != nil {
			return OwnedFile{}, e
		}
		if a.CurrentGenerationID == g.ID || a.PendingGenerationID == g.ID {
			return OwnedFile{}, ErrInUse
		}
	}
	// q задаёт постраничный обход журнала файлов очищаемого поколения.
	q := ListQuery{Limit: MaxListLimit, GenerationID: i.ArtifactGenerationID}
	for {
		// files содержит страницу принадлежащих поколению путей, а e — причину отказа её чтения.
		files, e := r.Artifacts.OwnedFiles(ctx, q)
		if e != nil {
			return OwnedFile{}, e
		}
		// f содержит очередную запись владения путём для точного сравнения с намерением очистки.
		for _, f := range files {
			if f.ID == i.FileID {
				if f.ManagedPath != i.ManagedPath || f.Kind != i.FileKind || f.ContentHash != i.ContentHash || f.SizeBytes != i.SizeBytes {
					return OwnedFile{}, ErrCorrupt
				}
				return f, nil
			}
		}
		if len(files) < q.Limit {
			return OwnedFile{}, ErrNotFound
		}
		// last содержит последнюю запись страницы для продолжения обхода журнала файлов.
		last := files[len(files)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}
