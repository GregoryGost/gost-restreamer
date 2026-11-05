package core

import (
	"bytes"
	"context"
	"fmt"
	"sort"
)

func (s *Service) preparationLimits() (PreparationLimits, error) {
	p, ok := s.uow.(PreparationLimitsProvider)
	if !ok {
		return PreparationLimits{}, ErrRuntimeCapability
	}
	l := p.PreparationLimits()
	return l, l.Validate()
}
func (s *Service) CreateAsset(ctx context.Context, name string) (FallbackAsset, error) {
	record, err := newRecord()
	if err != nil {
		return FallbackAsset{}, err
	}
	a := FallbackAsset{Record: record, Name: name, State: AssetPreparing}
	if err = a.Validate(); err != nil {
		return FallbackAsset{}, err
	}
	err = s.uow.Write(ctx, func(r Repositories) error { return r.Artifacts.InsertAsset(ctx, a) })
	return a, err
}

func requiredProfiles(ctx context.Context, r Repositories, assetID ID, limit int) ([]RequiredProfile, error) {
	result := make([]RequiredProfile, 0)
	seen := map[string]bool{}
	q := ListQuery{Limit: MaxListLimit, AssetID: assetID}
	for {
		streams, err := r.Config.Streams(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, stream := range streams {
			groups, err := allGroups(ctx, r, stream.ID)
			if err != nil {
				return nil, err
			}
			for _, group := range groups {
				p, err := r.Config.Profile(ctx, group.ProfileID)
				if err != nil {
					return nil, err
				}
				key, b, err := ProfileIdentity(p.Profile)
				if err != nil {
					return nil, err
				}
				if !seen[string(b)] {
					if len(result) >= limit {
						return nil, fmt.Errorf("%w: превышен бюджет профилей", ErrBusy)
					}
					seen[string(b)] = true
					canonical, err := DecodeProfile(b)
					if err != nil {
						return nil, err
					}
					result = append(result, RequiredProfile{Profile: canonical, ProfileKey: key, CanonicalProfile: b})
				}
			}
		}
		if len(streams) < q.Limit {
			break
		}
		last := streams[len(streams)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
	sort.Slice(result, func(i, j int) bool { return bytes.Compare(result[i].CanonicalProfile, result[j].CanonicalProfile) < 0 })
	return result, nil
}

func runtimeIdleForPreparation(ctx context.Context, r Repositories) error {
	q := ListQuery{Limit: MaxListLimit}
	for {
		sessions, err := r.Runtime.Sessions(ctx, q)
		if err != nil {
			return err
		}
		for _, session := range sessions {
			if session.State.Active() || session.Issued || session.CleanupPending {
				return ErrInUse
			}
		}
		if len(sessions) < q.Limit {
			break
		}
		last := sessions[len(sessions)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
	q = ListQuery{Limit: MaxListLimit, PendingOnly: true}
	for {
		intents, err := r.Artifacts.Cleanups(ctx, q)
		if err != nil {
			return err
		}
		for _, i := range intents {
			if i.Kind == CleanupRuntime {
				return ErrNotReady
			}
		}
		if len(intents) < q.Limit {
			break
		}
		last := intents[len(intents)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
	q = ListQuery{Limit: MaxListLimit}
	for {
		outputs, err := r.Runtime.Outputs(ctx, q)
		if err != nil {
			return err
		}
		for _, output := range outputs {
			if output.Issued || output.CleanupPending {
				return ErrNotReady
			}
		}
		if len(outputs) < q.Limit {
			break
		}
		last := outputs[len(outputs)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}

	return nil
}
func preparationIdleForRuntime(ctx context.Context, r Repositories, incarnation string) error {
	if r.Artifacts == nil {
		return ErrRuntimeCapability
	}
	// Терминальные остатки сохраняют удержания, но не занимают допуск нового эфира.
	for _, state := range []PreparationState{PreparationQueued, PreparationClaimed, PreparationIssued} {
		gs, err := r.Artifacts.Generations(ctx, ListQuery{Limit: 1, State: string(state), Incarnation: incarnation})
		if err != nil {
			return err
		}
		if len(gs) > 0 {
			return ErrNotReady
		}
	}
	return nil
}

func (s *Service) ReservePreparation(ctx context.Context, request PreparationRequest) (FallbackGeneration, error) {
	limits, err := s.preparationLimits()
	if err != nil {
		return FallbackGeneration{}, err
	}
	if _, err = s.runtimeAuthority(); err != nil {
		return FallbackGeneration{}, err
	}
	if err = validText("generator", request.GeneratorVersion); err != nil {
		return FallbackGeneration{}, err
	}
	record, err := newRecord()
	if err != nil {
		return FallbackGeneration{}, err
	}
	token, err := newID()
	if err != nil {
		return FallbackGeneration{}, err
	}
	var result FallbackGeneration
	err = s.uow.Write(ctx, func(r Repositories) error {
		if e := runtimeIdleForPreparation(ctx, r); e != nil {
			return e
		}
		a, e := r.Artifacts.Asset(ctx, request.AssetID)
		if e != nil {
			return e
		}
		if a.Revision != request.ExpectedRevision {
			return ErrConflict
		}
		if a.PendingGenerationID != "" {
			return ErrConflict
		}
		queued, e := r.Artifacts.Generations(ctx, ListQuery{Limit: limits.MaxQueued, PendingOnly: true})
		if e != nil {
			return e
		}
		for _, pending := range queued {
			if pending.Incarnation != s.incarnation && (pending.Issued || pending.CleanupPending) {
				return ErrNotReady
			}
		}
		if len(queued) >= limits.MaxQueued {
			return ErrBusy
		}
		profiles, e := requiredProfiles(ctx, r, a.ID, limits.MaxProfiles)
		if e != nil {
			return e
		}
		g := FallbackGeneration{Record: record, AssetID: a.ID, Kind: request.Kind, GeneratorVersion: request.GeneratorVersion, Incarnation: s.incarnation, AttemptToken: string(token), State: PreparationQueued, RequiredProfiles: profiles, OriginalPath: GenerationDirectory(record.ID) + "/original", StagingPath: GenerationDirectory(record.ID) + "/original.stage"}
		if request.Rebuild {
			if a.CurrentGenerationID == "" {
				return ErrNotReady
			}
			current, e := r.Artifacts.Generation(ctx, a.CurrentGenerationID)
			if e != nil {
				return e
			}
			if current.State != PreparationPublished {
				return ErrNotReady
			}
			g.Kind = current.Kind
			g.RebuildFromGenerationID = current.ID
		}
		for i := range g.RequiredProfiles {
			id, e := newID()
			if e != nil {
				return e
			}
			g.RequiredProfiles[i].VariantID = id
			g.RequiredProfiles[i].ManagedPath = GenerationDirectory(g.ID) + "/" + string(id) + ".mp4"
			g.RequiredProfiles[i].StagingPath = g.RequiredProfiles[i].ManagedPath + ".stage"
		}
		if e = g.Validate(); e != nil {
			return e
		}
		if e = r.Artifacts.InsertGeneration(ctx, g); e != nil {
			return e
		}
		old := a.Revision
		a.PendingGenerationID = g.ID
		a.Record, e = revise(a.Record)
		if e != nil {
			return e
		}
		if e = r.Artifacts.UpdateAsset(ctx, a, old); e != nil {
			return e
		}
		files := []OwnedFile{{ManagedPath: GenerationDirectory(g.ID), Kind: OwnedDirectory}, {ManagedPath: g.OriginalPath, Kind: OwnedOriginal}, {ManagedPath: g.StagingPath, Kind: OwnedStaging}}
		for _, p := range g.RequiredProfiles {
			files = append(files, OwnedFile{ManagedPath: p.ManagedPath, Kind: OwnedVariant}, OwnedFile{ManagedPath: p.StagingPath, Kind: OwnedStaging})
		}
		for _, f := range files {
			f.Record, e = newRecord()
			if e != nil {
				return e
			}
			f.AssetID = a.ID
			f.GenerationID = g.ID
			if e = r.Artifacts.InsertOwnedFile(ctx, f); e != nil {
				return e
			}
		}
		result = g
		return nil
	})
	return result, err
}
func preparationFenced(ctx context.Context, r Repositories, f PreparationFence) (FallbackGeneration, error) {
	g, err := r.Artifacts.Generation(ctx, f.GenerationID)
	if err != nil {
		return g, err
	}
	if PreparationFenceFor(g) != f {
		return g, ErrStale
	}
	return g, nil
}
func (s *Service) ClaimPreparation(ctx context.Context, id ID) (PreparationFence, error) {
	var result PreparationFence
	err := s.uow.Write(ctx, func(r Repositories) error {
		if e := runtimeIdleForPreparation(ctx, r); e != nil {
			return e
		}
		g, e := r.Artifacts.Generation(ctx, id)
		if e != nil {
			return e
		}
		if g.State != PreparationQueued || g.Incarnation != s.incarnation {
			return ErrStale
		}
		queued, e := r.Artifacts.Generations(ctx, ListQuery{Limit: MaxListLimit, PendingOnly: true})
		if e != nil {
			return e
		}
		for _, other := range queued {
			if other.ID != g.ID && (other.State == PreparationClaimed || other.Issued || other.CleanupPending) {
				return ErrBusy
			}
			if other.State == PreparationQueued && (other.CreatedAt.Before(g.CreatedAt) || other.CreatedAt.Equal(g.CreatedAt) && other.ID < g.ID) {
				return ErrBusy
			}
		}
		old := g.Revision
		g.State = PreparationClaimed
		g.Attempt, e = NextRevision(g.Attempt)
		if e != nil {
			return e
		}
		g.Record, e = revise(g.Record)
		if e != nil {
			return e
		}
		if e = r.Artifacts.UpdateGeneration(ctx, g, old); e != nil {
			return e
		}
		result = PreparationFenceFor(g)
		return nil
	})
	return result, err
}
func (s *Service) MarkPreparationIssued(ctx context.Context, f PreparationFence) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		if e := runtimeIdleForPreparation(ctx, r); e != nil {
			return e
		}
		g, e := preparationFenced(ctx, r, f)
		if e != nil {
			return e
		}
		if f.Incarnation != s.incarnation || g.State != PreparationClaimed {
			return ErrStale
		}
		other, e := r.Artifacts.Generations(ctx, ListQuery{Limit: MaxListLimit, PendingOnly: true})
		if e != nil {
			return e
		}
		for _, v := range other {
			if v.ID != g.ID && (v.Issued || v.State == PreparationClaimed || v.CleanupPending) {
				return ErrBusy
			}
		}
		old := g.Revision
		g.State = PreparationIssued
		g.Issued = true
		g.CleanupPending = true
		g.Record, e = revise(g.Record)
		if e != nil {
			return e
		}
		return r.Artifacts.UpdateGeneration(ctx, g, old)
	})
}
func (s *Service) RegisterPreparationFile(ctx context.Context, f PreparationFence, value OwnedFile) (OwnedFile, error) {
	var result OwnedFile
	err := s.uow.Write(ctx, func(r Repositories) error {
		g, e := preparationFenced(ctx, r, f)
		if e != nil {
			return e
		}
		if f.Incarnation != s.incarnation || g.State != PreparationIssued || !g.Issued {
			return ErrStale
		}
		value.AssetID = g.AssetID
		value.GenerationID = g.ID
		value.Record, e = newRecord()
		if e != nil {
			return e
		}
		if e = value.Validate(); e != nil {
			return e
		}
		if e = r.Artifacts.InsertOwnedFile(ctx, value); e != nil {
			return e
		}
		result = value
		return nil
	})
	return result, err
}
func (s *Service) PublishPreparation(ctx context.Context, f PreparationFence, p GenerationPublication) (FallbackGeneration, error) {
	limits, err := s.preparationLimits()
	if err != nil {
		return FallbackGeneration{}, err
	}
	var result FallbackGeneration
	err = s.uow.Write(ctx, func(r Repositories) error {
		if e := runtimeIdleForPreparation(ctx, r); e != nil {
			return e
		}
		g, e := preparationFenced(ctx, r, f)
		if e != nil {
			return e
		}
		if f.Incarnation != s.incarnation || g.State != PreparationIssued || !g.Issued {
			return ErrStale
		}
		a, e := r.Artifacts.Asset(ctx, g.AssetID)
		if e != nil {
			return e
		}
		if a.PendingGenerationID != g.ID {
			return ErrStale
		}
		current, e := requiredProfiles(ctx, r, g.AssetID, limits.MaxProfiles)
		if e != nil {
			return e
		}
		if len(current) != len(g.RequiredProfiles) || len(p.Variants) != len(current) {
			return ErrConflict
		}
		for i := range current {
			if current[i].ProfileKey != g.RequiredProfiles[i].ProfileKey || !bytes.Equal(current[i].CanonicalProfile, g.RequiredProfiles[i].CanonicalProfile) {
				return ErrConflict
			}
		}
		g.SourceHash = p.SourceHash
		g.SizeBytes = p.SizeBytes
		g.SourceFormat = p.SourceFormat
		g.State = PreparationPublished
		if e = g.Validate(); e != nil {
			return e
		}
		generationRevision := g.Revision
		g.Record, e = revise(g.Record)
		if e != nil {
			return e
		}
		if e = r.Artifacts.UpdateGeneration(ctx, g, generationRevision); e != nil {
			return e
		}
		seen := map[ID]bool{}
		for _, v := range p.Variants {
			var required *RequiredProfile
			for i := range g.RequiredProfiles {
				if g.RequiredProfiles[i].VariantID == v.ID {
					required = &g.RequiredProfiles[i]
					break
				}
			}
			if required == nil || seen[v.ID] || !v.Profile.Equal(required.Profile) || v.ManagedPath != required.ManagedPath || v.GenerationID != g.ID || v.AssetID != g.AssetID || v.GeneratorVersion != g.GeneratorVersion || v.SourceHash != g.SourceHash {
				return ErrConflict
			}
			seen[v.ID] = true
			v.Record = Record{ID: v.ID, Revision: 1, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt}
			v.Incarnation = g.Incarnation
			v.AttemptToken = g.AttemptToken
			v.State = VariantReady
			v.ProfileKey = required.ProfileKey
			v.CanonicalProfile = bytes.Clone(required.CanonicalProfile)
			if e = v.VerifiedFormat.Validate(); e != nil {
				return e
			}
			if CompatibleFormat(v.VerifiedFormat, v.Profile.Format) != nil {
				return ErrInvalid
			}
			if e = v.Validate(); e != nil {
				return e
			}
			if e = r.Artifacts.InsertVariant(ctx, v); e != nil {
				return e
			}
		}
		if e = completeGenerationLedger(ctx, r, g, p.Variants); e != nil {
			return e
		}
		old := a.Revision
		a.CurrentGenerationID = g.ID
		a.PendingGenerationID = ""
		projectGeneration(&a, g)
		a.Record, e = revise(a.Record)
		if e != nil {
			return e
		}
		if e = r.Artifacts.UpdateAsset(ctx, a, old); e != nil {
			return e
		}
		result = g
		return nil
	})
	return result, err
}
func projectGeneration(a *FallbackAsset, g FallbackGeneration) {
	a.Kind = g.Kind
	a.OriginalPath = g.OriginalPath
	a.SourceHash = g.SourceHash
	a.SizeBytes = g.SizeBytes
	a.SourceFormat = g.SourceFormat
	a.State = AssetReady
	a.Incarnation = g.Incarnation
	a.AttemptToken = g.AttemptToken
}
func (s *Service) CancelPreparation(ctx context.Context, f PreparationFence, reason string) error {
	if err := validText("reason", reason); err != nil {
		return err
	}
	return s.uow.Write(ctx, func(r Repositories) error {
		g, e := preparationFenced(ctx, r, f)
		if e != nil {
			return e
		}
		if f.Incarnation != s.incarnation || !g.State.Active() {
			return ErrStale
		}
		return failPreparation(ctx, r, g, PreparationFailed, reason)
	})
}
func failPreparation(ctx context.Context, r Repositories, g FallbackGeneration, state PreparationState, reason string) error {
	old := g.Revision
	g.State = state
	g.Failure = reason
	var err error
	g.Record, err = revise(g.Record)
	if err != nil {
		return err
	}
	if err = r.Artifacts.UpdateGeneration(ctx, g, old); err != nil {
		return err
	}
	if !g.Issued {
		return releasePendingPreparation(ctx, r, g)
	}
	return nil
}
func releasePendingPreparation(ctx context.Context, r Repositories, g FallbackGeneration) error {
	a, e := r.Artifacts.Asset(ctx, g.AssetID)
	if e != nil {
		return e
	}
	if a.PendingGenerationID != g.ID {
		return nil
	}
	old := a.Revision
	a.PendingGenerationID = ""
	if a.CurrentGenerationID == "" {
		a.State = AssetFailed
	}
	a.Record, e = revise(a.Record)
	if e != nil {
		return e
	}
	return r.Artifacts.UpdateAsset(ctx, a, old)
}
func (s *Service) AcknowledgePreparationCleanup(ctx context.Context, f PreparationFence) error {
	a, err := s.runtimeAuthority()
	if err != nil {
		return err
	}
	// Write удерживает authority, поэтому проверка регистраций и запись неделимы.
	return s.uow.Write(ctx, func(r Repositories) error {
		g, e := preparationFenced(ctx, r, f)
		if e != nil {
			return e
		}
		if !g.State.Terminal() {
			return ErrConflict
		}
		if owner := a.preparations[g.ID]; owner != nil {
			if owner.valid.Load() || owner.operations != 0 {
				return ErrInUse
			}
		}
		if !g.CleanupPending && !g.Issued {
			return nil
		}
		old := g.Revision
		g.Issued = false
		g.CleanupPending = false
		g.Record, e = revise(g.Record)
		if e != nil {
			return e
		}
		if e = r.Artifacts.UpdateGeneration(ctx, g, old); e != nil {
			return e
		}
		if g.State != PreparationPublished {
			if e = releasePendingPreparation(ctx, r, g); e != nil {
				return e
			}
			if e = queueGenerationCleanup(ctx, r, g); e != nil {
				return e
			}
		}
		return nil
	})
}
func queueGenerationCleanup(ctx context.Context, r Repositories, g FallbackGeneration) error {
	existing := map[ID]bool{}
	q := ListQuery{Limit: MaxListLimit, GenerationID: g.ID}
	for {
		page, err := r.Artifacts.Cleanups(ctx, q)
		if err != nil {
			return err
		}
		for _, i := range page {
			if i.Kind == CleanupFile {
				existing[i.FileID] = true
			}
		}
		if len(page) < q.Limit {
			break
		}
		last := page[len(page)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
	q = ListQuery{Limit: MaxListLimit, GenerationID: g.ID}
	for {
		files, e := r.Artifacts.OwnedFiles(ctx, q)
		if e != nil {
			return e
		}
		for _, f := range files {
			if existing[f.ID] {
				continue
			}
			record, e := newRecord()
			if e != nil {
				return e
			}
			if e = r.Artifacts.InsertCleanup(ctx, CleanupIntent{Record: record, Kind: CleanupFile, ArtifactGenerationID: g.ID, FileID: f.ID, FileKind: f.Kind, ContentHash: f.ContentHash, SizeBytes: f.SizeBytes, ManagedPath: f.ManagedPath, Incarnation: g.Incarnation}); e != nil {
				return e
			}
		}
		if len(files) < q.Limit {
			return nil
		}
		last := files[len(files)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}

func (s *Service) DeleteGeneration(ctx context.Context, id ID, expected uint64) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		g, e := r.Artifacts.Generation(ctx, id)
		if e != nil {
			return e
		}
		if g.Revision != expected {
			return ErrConflict
		}
		a, e := r.Artifacts.Asset(ctx, g.AssetID)
		if e != nil {
			return e
		}
		if a.CurrentGenerationID == g.ID || a.PendingGenerationID == g.ID || g.State.Active() || g.Issued || g.CleanupPending {
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
		return r.Artifacts.DeleteGeneration(ctx, g.ID, g.Revision)
	})
}
func (s *Service) GetGeneration(ctx context.Context, id ID) (FallbackGeneration, error) {
	var result FallbackGeneration
	err := s.uow.View(ctx, func(r Repositories) error { var e error; result, e = r.Artifacts.Generation(ctx, id); return e })
	return result, err
}
func (s *Service) ListGenerations(ctx context.Context, q PageRequest) (Page[FallbackGeneration], error) {
	return readPage(ctx, s, "generations", q, func(r Repositories, q ListQuery) ([]FallbackGeneration, error) {
		return r.Artifacts.Generations(ctx, q)
	}, func(g FallbackGeneration) ListBoundary { return ListBoundary{g.CreatedAt, g.ID} })
}
func (s *Service) ListGenerationFiles(ctx context.Context, q PageRequest) (Page[OwnedFile], error) {
	return readPage(ctx, s, "owned_files", q, func(r Repositories, q ListQuery) ([]OwnedFile, error) { return r.Artifacts.OwnedFiles(ctx, q) }, func(f OwnedFile) ListBoundary { return ListBoundary{f.CreatedAt, f.ID} })
}
func recoverPreparations(ctx context.Context, r Repositories, incarnation string) error {
	q := ListQuery{Limit: MaxListLimit, ExcludeIncarnation: incarnation}
	for {
		gs, e := r.Artifacts.Generations(ctx, q)
		if e != nil {
			return e
		}
		for _, g := range gs {
			if g.State.Active() {
				if e = failPreparation(ctx, r, g, PreparationInterrupted, "Подготовка прервана сменой владельца"); e != nil {
					return e
				}
			}
		}
		if len(gs) < q.Limit {
			return nil
		}
		last := gs[len(gs)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}

func completeGenerationLedger(ctx context.Context, r Repositories, g FallbackGeneration, variants []FallbackVariant) error {
	complete := map[string]struct {
		hash string
		size uint64
	}{g.OriginalPath: {g.SourceHash, g.SizeBytes}}
	for _, v := range variants {
		complete[v.ManagedPath] = struct {
			hash string
			size uint64
		}{v.ContentHash, v.SizeBytes}
	}
	q := ListQuery{Limit: MaxListLimit, GenerationID: g.ID}
	for {
		files, err := r.Artifacts.OwnedFiles(ctx, q)
		if err != nil {
			return err
		}
		for _, file := range files {
			if value, ok := complete[file.ManagedPath]; ok {
				old := file.Revision
				file.ContentHash = value.hash
				file.SizeBytes = value.size
				file.Record, err = revise(file.Record)
				if err != nil {
					return err
				}
				if err = r.Artifacts.UpdateOwnedFile(ctx, file, old); err != nil {
					return err
				}
				delete(complete, file.ManagedPath)
			}
		}
		if len(files) < q.Limit {
			break
		}
		last := files[len(files)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
	if len(complete) != 0 {
		return ErrCorrupt
	}
	return nil
}
