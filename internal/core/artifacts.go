package core

import (
	"bytes"
	"context"
	"fmt"
)

// ReserveAsset создаёт только стабильный объект; содержимое публикуется полным поколением.
func (s *Service) ReserveAsset(ctx context.Context, v FallbackAsset) (FallbackAsset, error) {
	return s.CreateAsset(ctx, v.Name)
}
func (s *Service) PublishAsset(context.Context, ID, uint64, string, string, uint64, SourceFormat) (FallbackAsset, error) {
	return FallbackAsset{}, fmt.Errorf("%w: требуется публикация полного поколения", ErrInvalid)
}
func (s *Service) FailAsset(context.Context, ID, uint64, string) error {
	return fmt.Errorf("%w: требуется отмена подготовки поколения", ErrInvalid)
}
func (s *Service) ReserveVariant(context.Context, FallbackVariant) (FallbackVariant, error) {
	return FallbackVariant{}, fmt.Errorf("%w: требуется подготовка полного поколения", ErrInvalid)
}
func (s *Service) PublishVariant(context.Context, ID, uint64, string, string, uint64) (FallbackVariant, error) {
	return FallbackVariant{}, fmt.Errorf("%w: требуется публикация полного поколения", ErrInvalid)
}
func (s *Service) FailVariant(context.Context, ID, uint64, string) error {
	return fmt.Errorf("%w: требуется отмена подготовки поколения", ErrInvalid)
}
func (s *Service) RenameAsset(ctx context.Context, id ID, expected uint64, name string) (FallbackAsset, error) {
	var result FallbackAsset
	err := s.uow.Write(ctx, func(r Repositories) error {
		a, e := r.Artifacts.Asset(ctx, id)
		if e != nil {
			return e
		}
		if a.Revision != expected {
			return ErrConflict
		}
		a.Name = name
		if e = a.Validate(); e != nil {
			return e
		}
		a.Record, e = revise(a.Record)
		if e != nil {
			return e
		}
		if e = r.Artifacts.UpdateAsset(ctx, a, expected); e != nil {
			return e
		}
		result = a
		return nil
	})
	return result, err
}
func (s *Service) DeleteVariant(ctx context.Context, id ID, expected uint64) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		v, e := r.Artifacts.Variant(ctx, id)
		if e != nil {
			return e
		}
		if v.Revision != expected {
			return ErrConflict
		}
		a, e := r.Artifacts.Asset(ctx, v.AssetID)
		if e != nil {
			return e
		}
		if a.CurrentGenerationID == v.GenerationID || a.PendingGenerationID == v.GenerationID {
			return ErrInUse
		}
		return fmt.Errorf("%w: удаляется только полный комплект поколения", ErrInUse)
	})
}
func (s *Service) DeleteAsset(ctx context.Context, id ID, expected uint64) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		a, e := r.Artifacts.Asset(ctx, id)
		if e != nil {
			return e
		}
		if a.Revision != expected {
			return ErrConflict
		}
		if a.PendingGenerationID != "" {
			return ErrInUse
		}
		// Снятие current и удаление всего комплекта откатываются при любом удержании.
		old := a.Revision
		a.CurrentGenerationID = ""
		a.State = AssetFailed
		a.Record, e = revise(a.Record)
		if e != nil {
			return e
		}
		if e = r.Artifacts.UpdateAsset(ctx, a, old); e != nil {
			return e
		}
		for {
			gs, e := r.Artifacts.Generations(ctx, ListQuery{Limit: MaxListLimit, AssetID: a.ID})
			if e != nil {
				return e
			}
			for _, g := range gs {
				if g.State.Active() || g.Issued || g.CleanupPending {
					return ErrInUse
				}
				if e = queueGenerationCleanup(ctx, r, g); e != nil {
					return e
				}
				for {
					vs, e := r.Artifacts.Variants(ctx, ListQuery{Limit: MaxListLimit, GenerationID: g.ID})
					if e != nil {
						return e
					}
					for _, v := range vs {
						if e = r.Artifacts.DeleteVariant(ctx, v.ID, v.Revision); e != nil {
							return e
						}
					}
					if len(vs) < MaxListLimit {
						break
					}
				}
				if e = r.Artifacts.DeleteGeneration(ctx, g.ID, g.Revision); e != nil {
					return e
				}
			}
			if len(gs) < MaxListLimit {
				break
			}
		}
		return r.Artifacts.DeleteAsset(ctx, a.ID, a.Revision)
	})
}
func exactManifest(ctx context.Context, r Repositories, policy FallbackPolicy, profile GroupProfile, binding PreparedBinding) (ArtifactManifest, error) {
	if err := policy.Validate(); err != nil {
		return ArtifactManifest{}, fmt.Errorf("%w: заглушка не настроена", ErrNotReady)
	}
	a, err := r.Artifacts.Asset(ctx, policy.AssetID)
	if err != nil {
		return ArtifactManifest{}, err
	}
	if a.CurrentGenerationID == "" || binding.GenerationID != a.CurrentGenerationID {
		return ArtifactManifest{}, ErrNotReady
	}
	g, err := r.Artifacts.Generation(ctx, binding.GenerationID)
	if err != nil {
		return ArtifactManifest{}, err
	}
	if g.AssetID != a.ID || g.State != PreparationPublished {
		return ArtifactManifest{}, ErrNotReady
	}
	v, err := r.Artifacts.Variant(ctx, binding.VariantID)
	if err != nil {
		return ArtifactManifest{}, err
	}
	key, b, err := ProfileIdentity(profile)
	if err != nil {
		return ArtifactManifest{}, err
	}
	if v.State != VariantReady || v.AssetID != a.ID || v.GenerationID != g.ID || v.SourceHash != g.SourceHash || v.GeneratorVersion != binding.GeneratorVersion || v.GeneratorVersion != g.GeneratorVersion || v.ProfileKey != key || !bytes.Equal(v.CanonicalProfile, b) || !v.Profile.Equal(profile) {
		return ArtifactManifest{}, fmt.Errorf("%w: вариант не соответствует точному поколению и профилю", ErrNotReady)
	}
	return ArtifactManifest{GenerationID: g.ID, VerifiedFormat: v.VerifiedFormat, VariantID: v.ID, AssetID: a.ID, SourceHash: v.SourceHash, Profile: v.Profile, ProfileKey: v.ProfileKey, CanonicalProfile: bytes.Clone(v.CanonicalProfile), GeneratorVersion: v.GeneratorVersion, ManagedPath: v.ManagedPath, SizeBytes: v.SizeBytes, ContentHash: v.ContentHash}, nil
}
