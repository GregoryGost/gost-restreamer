package core

import (
	"bytes"
	"context"
	"fmt"
)

func (s *Service) ReserveAsset(ctx context.Context, value FallbackAsset) (FallbackAsset, error) {
	value.Incarnation = s.incarnation
	value.State = AssetPreparing
	value.SourceHash = ""
	value.SizeBytes = 0
	value.SourceFormat = SourceFormat{}
	record, err := newRecord()
	if err != nil {
		return FallbackAsset{}, err
	}
	value.Record = record
	token, err := newID()
	if err != nil {
		return FallbackAsset{}, err
	}
	value.AttemptToken = string(token)
	if err = value.Validate(); err != nil {
		return FallbackAsset{}, err
	}
	err = s.uow.Write(ctx, func(r Repositories) error { return r.Artifacts.InsertAsset(ctx, value) })
	return value, err
}
func (s *Service) PublishAsset(ctx context.Context, id ID, expected uint64, attempt string, hash string, size uint64, format SourceFormat) (FallbackAsset, error) {
	var result FallbackAsset
	err := s.uow.Write(ctx, func(r Repositories) error {
		a, err := r.Artifacts.Asset(ctx, id)
		if err != nil {
			return err
		}
		if a.Revision != expected || a.AttemptToken != attempt || a.State != AssetPreparing || a.Incarnation != s.incarnation {
			return ErrStale
		}
		old := a.Revision
		a.State = AssetReady
		a.SourceHash = hash
		a.SizeBytes = size
		a.SourceFormat = format
		if err = a.Validate(); err != nil {
			return err
		}
		a.Record, err = revise(a.Record)
		if err != nil {
			return err
		}
		if err = r.Artifacts.UpdateAsset(ctx, a, old); err != nil {
			return err
		}
		result = a
		return nil
	})
	return result, err
}
func (s *Service) FailAsset(ctx context.Context, id ID, expected uint64, attempt string) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		a, err := r.Artifacts.Asset(ctx, id)
		if err != nil {
			return err
		}
		if a.Revision != expected || a.AttemptToken != attempt || a.State != AssetPreparing || a.Incarnation != s.incarnation {
			return ErrStale
		}
		a.State = AssetFailed
		a.Record, err = revise(a.Record)
		if err != nil {
			return err
		}
		if err = r.Artifacts.UpdateAsset(ctx, a, expected); err != nil {
			return err
		}
		return insertFileCleanup(ctx, r, a.OriginalPath)
	})
}
func (s *Service) RenameAsset(ctx context.Context, id ID, expected uint64, name string) (FallbackAsset, error) {
	var value FallbackAsset
	err := s.uow.Write(ctx, func(r Repositories) error {
		a, err := r.Artifacts.Asset(ctx, id)
		if err != nil {
			return err
		}
		if a.Revision != expected {
			return ErrConflict
		}
		a.Name = name
		if err = a.Validate(); err != nil {
			return err
		}
		a.Record, err = revise(a.Record)
		if err != nil {
			return err
		}
		value = a
		return r.Artifacts.UpdateAsset(ctx, a, expected)
	})
	return value, err
}
func (s *Service) ReserveVariant(ctx context.Context, value FallbackVariant) (FallbackVariant, error) {
	record, err := newRecord()
	if err != nil {
		return FallbackVariant{}, err
	}
	value.Record = record
	token, err := newID()
	if err != nil {
		return FallbackVariant{}, err
	}
	value.AttemptToken = string(token)
	value.Incarnation = s.incarnation
	value.State = VariantPreparing
	value.ContentHash = ""
	value.SizeBytes = 0
	value.ProfileKey, value.CanonicalProfile, err = ProfileIdentity(value.Profile)
	if err != nil {
		return FallbackVariant{}, err
	}
	if err = value.Validate(); err != nil {
		return FallbackVariant{}, err
	}
	err = s.uow.Write(ctx, func(r Repositories) error {
		a, err := r.Artifacts.Asset(ctx, value.AssetID)
		if err != nil {
			return err
		}
		if a.State != AssetReady || a.SourceHash != value.SourceHash {
			return ErrNotReady
		}
		return r.Artifacts.InsertVariant(ctx, value)
	})
	return value, err
}
func (s *Service) PublishVariant(ctx context.Context, id ID, expected uint64, attempt, hash string, size uint64) (FallbackVariant, error) {
	var result FallbackVariant
	err := s.uow.Write(ctx, func(r Repositories) error {
		v, err := r.Artifacts.Variant(ctx, id)
		if err != nil {
			return err
		}
		if v.Revision != expected || v.AttemptToken != attempt || v.State != VariantPreparing || v.Incarnation != s.incarnation {
			return ErrStale
		}
		v.State = VariantReady
		v.ContentHash = hash
		v.SizeBytes = size
		if err = v.Validate(); err != nil {
			return err
		}
		v.Record, err = revise(v.Record)
		if err != nil {
			return err
		}
		if err = r.Artifacts.UpdateVariant(ctx, v, expected); err != nil {
			return err
		}
		result = v
		return nil
	})
	return result, err
}
func (s *Service) FailVariant(ctx context.Context, id ID, expected uint64, attempt string) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		v, err := r.Artifacts.Variant(ctx, id)
		if err != nil {
			return err
		}
		if v.Revision != expected || v.AttemptToken != attempt || v.State != VariantPreparing || v.Incarnation != s.incarnation {
			return ErrStale
		}
		v.State = VariantFailed
		v.Record, err = revise(v.Record)
		if err != nil {
			return err
		}
		if err = r.Artifacts.UpdateVariant(ctx, v, expected); err != nil {
			return err
		}
		return insertFileCleanup(ctx, r, v.ManagedPath)
	})
}
func insertFileCleanup(ctx context.Context, r Repositories, managedPath string) error {
	record, err := newRecord()
	if err != nil {
		return err
	}
	return r.Artifacts.InsertCleanup(ctx, CleanupIntent{Record: record, Kind: CleanupFile, ManagedPath: managedPath})
}
func deleteVariant(ctx context.Context, r Repositories, v FallbackVariant) error {
	if err := r.Artifacts.DeleteVariant(ctx, v.ID, v.Revision); err != nil {
		return err
	}
	return insertFileCleanup(ctx, r, v.ManagedPath)
}
func (s *Service) DeleteVariant(ctx context.Context, id ID, expected uint64) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		v, err := r.Artifacts.Variant(ctx, id)
		if err != nil {
			return err
		}
		if v.Revision != expected {
			return ErrConflict
		}
		return deleteVariant(ctx, r, v)
	})
}
func (s *Service) DeleteAsset(ctx context.Context, id ID, expected uint64) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		a, err := r.Artifacts.Asset(ctx, id)
		if err != nil {
			return err
		}
		if a.Revision != expected {
			return ErrConflict
		}
		for {
			variants, err := r.Artifacts.Variants(ctx, ListQuery{Limit: MaxListLimit, AssetID: id})
			if err != nil {
				return err
			}
			for _, v := range variants {
				if err = deleteVariant(ctx, r, v); err != nil {
					return err
				}
			}
			if len(variants) < MaxListLimit {
				break
			}
		}
		if err = r.Artifacts.DeleteAsset(ctx, id, expected); err != nil {
			return err
		}
		return insertFileCleanup(ctx, r, a.OriginalPath)
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
	if a.State != AssetReady {
		return ArtifactManifest{}, ErrNotReady
	}
	v, err := r.Artifacts.Variant(ctx, binding.VariantID)
	if err != nil {
		return ArtifactManifest{}, err
	}
	key, content, err := ProfileIdentity(profile)
	if err != nil {
		return ArtifactManifest{}, err
	}
	if v.State != VariantReady || v.AssetID != a.ID || v.SourceHash != a.SourceHash || v.GeneratorVersion != binding.GeneratorVersion || v.ProfileKey != key || !bytes.Equal(v.CanonicalProfile, content) || !v.Profile.Equal(profile) {
		return ArtifactManifest{}, fmt.Errorf("%w: вариант не соответствует точному профилю", ErrNotReady)
	}
	return ArtifactManifest{VariantID: v.ID, AssetID: a.ID, SourceHash: v.SourceHash, Profile: v.Profile, ProfileKey: v.ProfileKey, CanonicalProfile: bytes.Clone(v.CanonicalProfile), GeneratorVersion: v.GeneratorVersion, ManagedPath: v.ManagedPath, SizeBytes: v.SizeBytes, ContentHash: v.ContentHash}, nil
}
