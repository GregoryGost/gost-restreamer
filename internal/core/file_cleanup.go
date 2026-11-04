package core

import (
	"context"
	"errors"
)

// AuthorizeFileCleanup сверяет долговечное намерение до любого удаления файла.
// Удалённое поколение не может снова стать current или получить новые удержания.
func (s *Service) AuthorizeFileCleanup(ctx context.Context, f CleanupFence) (OwnedFile, error) {
	var result OwnedFile
	err := s.uow.View(ctx, func(r Repositories) error {
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
func authorizeFileCleanup(ctx context.Context, r Repositories, i CleanupIntent) (OwnedFile, error) {
	held, e := r.Runtime.GenerationHeld(ctx, i.ArtifactGenerationID)
	if e != nil {
		return OwnedFile{}, e
	}
	if held {
		return OwnedFile{}, ErrInUse
	}
	g, e := r.Artifacts.Generation(ctx, i.ArtifactGenerationID)
	if e != nil && !errors.Is(e, ErrNotFound) {
		return OwnedFile{}, e
	}
	if e == nil {
		if g.State.Active() || g.Issued || g.CleanupPending {
			return OwnedFile{}, ErrInUse
		}
		a, e := r.Artifacts.Asset(ctx, g.AssetID)
		if e != nil {
			return OwnedFile{}, e
		}
		if a.CurrentGenerationID == g.ID || a.PendingGenerationID == g.ID {
			return OwnedFile{}, ErrInUse
		}
	}
	q := ListQuery{Limit: MaxListLimit, GenerationID: i.ArtifactGenerationID}
	for {
		files, e := r.Artifacts.OwnedFiles(ctx, q)
		if e != nil {
			return OwnedFile{}, e
		}
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
		last := files[len(files)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}
