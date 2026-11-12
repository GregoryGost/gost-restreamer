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

// PublishAsset отклоняет отдельную публикацию оригинала: сохраняется только полный комплект поколения.
func (s *Service) PublishAsset(context.Context, ID, uint64, string, string, uint64, SourceFormat) (FallbackAsset, error) {
	return FallbackAsset{}, fmt.Errorf("%w: требуется публикация полного поколения", ErrInvalid)
}

// FailAsset отклоняет отдельный отказ Asset: отменять следует подготовку поколения.
func (s *Service) FailAsset(context.Context, ID, uint64, string) error {
	return fmt.Errorf("%w: требуется отмена подготовки поколения", ErrInvalid)
}

// ReserveVariant отклоняет отдельное резервирование варианта вне подготовки полного поколения.
func (s *Service) ReserveVariant(context.Context, FallbackVariant) (FallbackVariant, error) {
	return FallbackVariant{}, fmt.Errorf("%w: требуется подготовка полного поколения", ErrInvalid)
}

// PublishVariant отклоняет отдельную публикацию варианта вне полного поколения.
func (s *Service) PublishVariant(context.Context, ID, uint64, string, string, uint64) (FallbackVariant, error) {
	return FallbackVariant{}, fmt.Errorf("%w: требуется публикация полного поколения", ErrInvalid)
}

// FailVariant отклоняет отдельный отказ варианта: отменять следует подготовку поколения.
func (s *Service) FailVariant(context.Context, ID, uint64, string) error {
	return fmt.Errorf("%w: требуется отмена подготовки поколения", ErrInvalid)
}

// RenameAsset меняет только имя заглушки при ожидаемой ревизии, проверяя значение и увеличивая
// ревизию.
func (s *Service) RenameAsset(ctx context.Context, id ID, expected uint64, name string) (FallbackAsset, error) {
	var result FallbackAsset
	// err сообщает об ошибке транзакционного переименования заглушки.
	err := s.uow.Write(ctx, func(r Repositories) error {
		// В транзакции сверяет Revision заглушки, проверяет новое имя и сохраняет следующую версию; result
		// заполняется после успешного UpdateAsset.

		// a содержит переименовываемую заглушку, а e — результат её чтения.
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

// DeleteVariant проверяет существование и ревизию варианта, затем отклоняет одиночное удаление:
// удалять требуется полный комплект поколения.
func (s *Service) DeleteVariant(ctx context.Context, id ID, expected uint64) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		// В транзакции сверяет вариант и его Asset, затем запрещает удаление отдельного варианта, поскольку
		// удалять можно только полный комплект поколения.

		// v содержит вариант для проверки ожидаемой ревизии, а e — результат его чтения.
		v, e := r.Artifacts.Variant(ctx, id)
		if e != nil {
			return e
		}
		if v.Revision != expected {
			return ErrConflict
		}
		// a содержит заглушку-владельца для проверки current/pending поколения варианта.
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

// DeleteAsset в одной транзакции снимает текущее поколение, ставит принадлежащие файлы на очистку и
// удаляет метаданные комплектов. Активная подготовка, незавершённая очистка или удержания откатывают
// удаление.
func (s *Service) DeleteAsset(ctx context.Context, id ID, expected uint64) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		// В одной транзакции очищает CurrentGenerationID, ставит очистку поколений и удаляет их метаданные
		// вместе с Asset; активная подготовка, удержание или ошибка прерывают операцию.

		// a содержит удаляемую заглушку, а e — результат её чтения.
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
		// Снятие текущего поколения и удаление всего комплекта откатываются при любом удержании.
		// old сохраняет ожидаемую ревизию перед снятием текущего поколения.
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
			// gs содержит страницу поколений удаляемой заглушки, а e — результат её чтения.
			gs, e := r.Artifacts.Generations(ctx, ListQuery{Limit: MaxListLimit, AssetID: a.ID})
			if e != nil {
				return e
			}
			// g содержит очередное поколение для проверки занятости и постановки файлов на
			// очистку.
			for _, g := range gs {
				if g.State.Active() || g.Issued || g.CleanupPending {
					return ErrInUse
				}
				if e = queueGenerationCleanup(ctx, r, g); e != nil {
					return e
				}
				for {
					// vs содержит страницу вариантов удаляемого поколения, а e —
					// результат её чтения.
					vs, e := r.Artifacts.Variants(ctx, ListQuery{Limit: MaxListLimit, GenerationID: g.ID})
					if e != nil {
						return e
					}
					// v содержит очередной вариант, метаданные которого удаляются с
					// полным комплектом.
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

// exactManifest принимает только готовый вариант текущего опубликованного поколения, точно совпадающий
// с привязкой, источником, генератором и полным профилем; канонические байты копируются.
func exactManifest(ctx context.Context, r Repositories, policy FallbackPolicy, profile GroupProfile, binding PreparedBinding) (ArtifactManifest, error) {
	// err сообщает об ошибке проверки политики выбранной заглушки.
	if err := policy.Validate(); err != nil {
		return ArtifactManifest{}, fmt.Errorf("%w: заглушка не настроена", ErrNotReady)
	}
	// a содержит заглушку и указатель её текущего поколения, а err — результат их чтения.
	a, err := r.Artifacts.Asset(ctx, policy.AssetID)
	if err != nil {
		return ArtifactManifest{}, err
	}
	if a.CurrentGenerationID == "" || binding.GenerationID != a.CurrentGenerationID {
		return ArtifactManifest{}, ErrNotReady
	}
	// g содержит поколение, точно указанное привязкой запуска.
	g, err := r.Artifacts.Generation(ctx, binding.GenerationID)
	if err != nil {
		return ArtifactManifest{}, err
	}
	if g.AssetID != a.ID || g.State != PreparationPublished {
		return ArtifactManifest{}, ErrNotReady
	}
	// v содержит конкретный вариант, который сверяется с поколением и полным профилем.
	v, err := r.Artifacts.Variant(ctx, binding.VariantID)
	if err != nil {
		return ArtifactManifest{}, err
	}
	// key содержит вычисленный ключ требуемого профиля, а b — его канонические байты для точного
	// сравнения.
	key, b, err := ProfileIdentity(profile)
	if err != nil {
		return ArtifactManifest{}, err
	}
	if v.State != VariantReady || v.AssetID != a.ID || v.GenerationID != g.ID || v.SourceHash != g.SourceHash || v.GeneratorVersion != binding.GeneratorVersion || v.GeneratorVersion != g.GeneratorVersion || v.ProfileKey != key || !bytes.Equal(v.CanonicalProfile, b) || !v.Profile.Equal(profile) {
		return ArtifactManifest{}, fmt.Errorf("%w: вариант не соответствует точному поколению и профилю", ErrNotReady)
	}
	return ArtifactManifest{GenerationID: g.ID, VerifiedFormat: v.VerifiedFormat, VariantID: v.ID, AssetID: a.ID, SourceHash: v.SourceHash, Profile: v.Profile, ProfileKey: v.ProfileKey, CanonicalProfile: bytes.Clone(v.CanonicalProfile), GeneratorVersion: v.GeneratorVersion, ManagedPath: v.ManagedPath, SizeBytes: v.SizeBytes, ContentHash: v.ContentHash}, nil
}
