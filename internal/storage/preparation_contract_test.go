package storage

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func issuedFixturePreparation(t *testing.T, f domainFixture) (core.FallbackGeneration, core.PreparationFence) {
	t.Helper()
	ctx := context.Background()
	g, err := f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: f.asset.ID, ExpectedRevision: f.asset.Revision, Kind: core.AssetImage, GeneratorVersion: "generator-next"})
	if err != nil {
		t.Fatal(err)
	}
	fence, err := f.service.ClaimPreparation(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.MarkPreparationIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	return g, fence
}

func TestPreparationOwnerRetainsLateOperationUntilJoinAndRegistersOnlyOwnedPaths(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	g, fence := issuedFixturePreparation(t, f)
	owner, err := f.service.BindPreparationOwner(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.service.BindPreparationOwner(ctx, fence)
	if err != nil || again != owner || owner.Fence() != fence {
		t.Fatal("повтор binding создал второго владельца", err)
	}
	finish, err := owner.Register(ctx)
	if err != nil {
		t.Fatal(err)
	}
	extra, err := f.service.RegisterPreparationFile(ctx, fence, core.OwnedFile{ManagedPath: core.GenerationDirectory(g.ID) + "/probe.stage", Kind: core.OwnedStaging})
	if err != nil {
		t.Fatal(err)
	}
	if extra.GenerationID != g.ID || extra.AssetID != g.AssetID {
		t.Fatal("ledger потерял владельца")
	}
	_, err = f.service.RegisterPreparationFile(ctx, fence, core.OwnedFile{ManagedPath: "foreign/file", Kind: core.OwnedStaging})
	requireDomainError(t, err, core.ErrInvalid)
	_, err = f.service.RegisterPreparationFile(ctx, fence, extra)
	requireDomainError(t, err, core.ErrConflict)
	requireDomainError(t, f.service.AcknowledgePreparationCleanup(ctx, fence), core.ErrConflict)
	if err = f.service.CancelPreparation(ctx, fence, "Отмена при незавершённой записи"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-owner.Done():
	default:
		t.Fatal("отмена не отозвала допуск")
	}
	if owner.Valid() {
		t.Fatal("отменённый owner действителен")
	}
	_, err = owner.Register(ctx)
	requireDomainError(t, err, core.ErrStale)
	requireDomainError(t, f.service.AcknowledgePreparationCleanup(ctx, fence), core.ErrInUse)
	if err = owner.Revoke(ctx); err != nil {
		t.Fatal(err)
	}
	finish()
	finish()
	if err = f.service.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
	if err = f.service.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
	a, err := f.service.GetAsset(ctx, f.asset.ID)
	if err != nil || a.PendingGenerationID != "" || a.CurrentGenerationID != f.asset.CurrentGenerationID {
		t.Fatal("очистка отменила рабочий current", err)
	}
	intents, err := f.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 20, PendingOnly: true}})
	if err != nil || len(intents.Items) != 6 {
		t.Fatalf("не все registered paths учтены: %+v,%v", intents, err)
	}
	wrong := fence
	wrong.Attempt++
	requireDomainError(t, f.service.AcknowledgePreparationCleanup(ctx, wrong), core.ErrStale)
}

func TestGenerationPublicationPersistsExactCleanupIntegrityAndRejectsPartialBundles(t *testing.T) {
	for _, kind := range []string{"missing", "duplicate", "asset", "generation", "path", "source", "generator", "actual", "empty_source", "empty_result"} {
		t.Run(kind, func(t *testing.T) {
			f := newDomainFixture(t, 1)
			ctx := context.Background()
			g, fence := issuedFixturePreparation(t, f)
			p := fixturePublication(g, "source-hash", "result-hash")
			switch kind {
			case "missing":
				p.Variants = nil
			case "duplicate":
				p.Variants = append(p.Variants, p.Variants[0])
			case "asset":
				p.Variants[0].AssetID = "foreign"
			case "generation":
				p.Variants[0].GenerationID = "foreign"
			case "path":
				p.Variants[0].ManagedPath = f.variant.ManagedPath
			case "source":
				p.Variants[0].SourceHash = "foreign"
			case "generator":
				p.Variants[0].GeneratorVersion = "foreign"
			case "actual":
				p.Variants[0].VerifiedFormat.Video.Width++
			case "empty_source":
				p.SourceHash = ""
			case "empty_result":
				p.Variants[0].ContentHash = ""
			}
			if _, err := f.service.PublishPreparation(ctx, fence, p); err == nil {
				t.Fatal("неполная публикация принята")
			}
			a, err := f.service.GetAsset(ctx, f.asset.ID)
			if err != nil || a.CurrentGenerationID != f.asset.CurrentGenerationID || a.PendingGenerationID != g.ID {
				t.Fatal("неудачный publish изменил current/pending", err)
			}
			page, err := f.service.ListVariants(ctx, core.PageRequest{Query: core.ListQuery{Limit: 10, GenerationID: g.ID}})
			if err != nil || len(page.Items) != 0 {
				t.Fatal("неудачный publish оставил частичные варианты", err)
			}
			p = fixturePublication(g, "source-hash", "result-hash")
			if _, err = f.service.PublishPreparation(ctx, fence, p); err != nil {
				t.Fatal(err)
			}
			if err = f.service.AcknowledgePreparationCleanup(ctx, fence); err != nil {
				t.Fatal(err)
			}
			files, err := f.service.ListGenerationFiles(ctx, core.PageRequest{Query: core.ListQuery{Limit: 10, GenerationID: g.ID}})
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files.Items {
				switch file.Kind {
				case core.OwnedOriginal:
					if file.SizeBytes != p.SizeBytes || file.ContentHash != p.SourceHash {
						t.Fatal("ledger потерял exact original integrity")
					}
				case core.OwnedVariant:
					if file.SizeBytes != p.Variants[0].SizeBytes || file.ContentHash != p.Variants[0].ContentHash {
						t.Fatal("ledger потерял exact variant integrity")
					}
				}
			}
		})
	}
}

func TestPublishedGenerationSurvivesRecoveryWhileIssuedCleanupRemainsQuarantined(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	g, fence := issuedFixturePreparation(t, f)
	published, err := f.service.PublishPreparation(ctx, fence, fixturePublication(g, "fresh-source", "fresh-result"))
	if err != nil {
		t.Fatal(err)
	}
	f = reopenFixture(t, f)
	got, err := f.service.GetGeneration(ctx, g.ID)
	if err != nil || !reflect.DeepEqual(got, published) || !got.Issued || !got.CleanupPending {
		t.Fatalf("recovery потеряло публикацию/cleanup: %+v,%v", got, err)
	}
	a, err := f.service.GetAsset(ctx, f.asset.ID)
	if err != nil || a.CurrentGenerationID != g.ID || a.PendingGenerationID != "" {
		t.Fatal("recovery откатило завершённый current", err)
	}
	_, err = f.service.StartSession(ctx, f.startRequest("before-trusted-cleanup"))
	requireDomainError(t, err, core.ErrNotReady)
	if err = f.service.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
	a, newVariant := publishFixtureGeneration(t, f.service, a, "next-source", "next-result")
	if a.CurrentGenerationID == g.ID || newVariant.GenerationID == g.ID {
		t.Fatal("следующая подготовка повторно использовала generation")
	}
	retired, err := f.service.GetGeneration(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.DeleteGeneration(ctx, g.ID, retired.Revision); err != nil {
		t.Fatal(err)
	}
	intents, err := f.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 20, PendingOnly: true}})
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, i := range intents.Items {
		if i.ArtifactGenerationID == g.ID && (i.FileKind == core.OwnedVariant || i.FileKind == core.OwnedOriginal) {
			found++
			if i.ContentHash == "" || i.SizeBytes == 0 {
				t.Fatal("удаление metadata потеряло cleanup integrity")
			}
		}
	}
	if found != 2 {
		t.Fatalf("не все завершённые файлы учтены: %d", found)
	}
	if _, err = f.service.GetGeneration(ctx, g.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestAssetMaterializedSourceFieldsCannotReplaceGenerationAuthority(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	projection := f.asset
	projection.Kind = core.AssetVideo
	projection.State = core.AssetFailed
	projection.SourceHash = "foreign-source"
	projection.OriginalPath = "foreign/original"
	projection.SizeBytes = 1
	projection.SourceFormat = core.SourceFormat{}
	projection.AttemptToken = "foreign-attempt"
	projection.Incarnation = "foreign-incarnation"
	projection.Revision++
	if err := f.store.Write(ctx, func(r core.Repositories) error { return r.Artifacts.UpdateAsset(ctx, projection, f.asset.Revision) }); err != nil {
		t.Fatal(err)
	}
	request := f.startRequest("generation-authority")
	receipt, err := f.service.StartSession(ctx, request)
	if err != nil {
		t.Fatal("материализованная проекция стала вторым authority", err)
	}
	snapshot := readOutputs(t, f.store, receipt.SessionID)[0].Snapshot.Artifact
	if snapshot.GenerationID != f.variant.GenerationID || snapshot.ManagedPath != f.variant.ManagedPath || snapshot.SourceHash != f.variant.SourceHash || snapshot.ContentHash != f.variant.ContentHash {
		t.Fatalf("source projection подменила точный manifest: %+v", snapshot)
	}
}

func TestResidualRuntimeCleanupBeyondFirstPageStillBlocksPreparation(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	f.store.options.RetryTimeout = 10 * time.Second
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := f.store.Write(ctx, func(r core.Repositories) error {
		for i := 0; i <= core.MaxListLimit; i++ {
			id := core.ID(fmt.Sprintf("file-%04d", i))
			record := core.Record{ID: id, Revision: 1, CreatedAt: base, UpdatedAt: base}
			file := core.OwnedFile{Record: record, AssetID: f.asset.ID, GenerationID: f.variant.GenerationID, ManagedPath: core.GenerationDirectory(f.variant.GenerationID) + "/" + string(id) + ".stage", Kind: core.OwnedStaging}
			if err := r.Artifacts.InsertOwnedFile(ctx, file); err != nil {
				return err
			}
			record.ID = core.ID("cleanup-" + string(id))
			if err := r.Artifacts.InsertCleanup(ctx, core.CleanupIntent{Record: record, Kind: core.CleanupFile, ArtifactGenerationID: file.GenerationID, FileID: file.ID, FileKind: file.Kind, ManagedPath: file.ManagedPath}); err != nil {
				return err
			}
		}
		record := core.Record{ID: "retired-session", Revision: 1, CreatedAt: base.Add(time.Second), UpdatedAt: base.Add(time.Second)}
		session := core.Session{Record: record, StreamID: f.stream.ID, Incarnation: f.store.Incarnation(), State: core.SessionStopped, Lifecycle: 2, Snapshot: core.SessionSnapshot{Stream: f.stream, Groups: []core.GroupSnapshot{{Group: f.group, Profile: f.profile}}}}
		if err := r.Runtime.InsertSession(ctx, session); err != nil {
			return err
		}
		record.ID = "residual-runtime-cleanup"
		return r.Artifacts.InsertCleanup(ctx, core.CleanupIntent{Record: record, Kind: core.CleanupRuntime, StreamID: f.stream.ID, SessionID: session.ID, Incarnation: session.Incarnation, Lifecycle: 1})
	}); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: f.asset.ID, ExpectedRevision: f.asset.Revision, Kind: core.AssetImage, GeneratorVersion: "generator"})
	requireDomainError(t, err, core.ErrNotReady)
	a, err := f.service.GetAsset(ctx, f.asset.ID)
	if err != nil || a.PendingGenerationID != "" {
		t.Fatal("residual cleanup оставило принятую подготовку", err)
	}
}

func TestFileCleanupAuthorizationRejectsFabricatedIntentAndProtectedCurrent(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	files, err := f.service.ListGenerationFiles(ctx, core.PageRequest{Query: core.ListQuery{Limit: 20, GenerationID: f.variant.GenerationID}})
	if err != nil {
		t.Fatal(err)
	}
	var owned core.OwnedFile
	for _, file := range files.Items {
		if file.Kind == core.OwnedVariant {
			owned = file
		}
	}
	record := owned.Record
	record.ID = "current-file-cleanup"
	record.Revision = 1
	intent := core.CleanupIntent{Record: record, Kind: core.CleanupFile, ArtifactGenerationID: owned.GenerationID, FileID: owned.ID, FileKind: owned.Kind, ManagedPath: owned.ManagedPath, ContentHash: owned.ContentHash, SizeBytes: owned.SizeBytes}
	fence := core.CleanupFenceFor(intent)
	_, err = f.service.AuthorizeFileCleanup(ctx, fence)
	requireDomainError(t, err, core.ErrNotFound)
	if err = f.store.Write(ctx, func(r core.Repositories) error { return r.Artifacts.InsertCleanup(ctx, intent) }); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.AuthorizeFileCleanup(ctx, fence)
	requireDomainError(t, err, core.ErrInUse)
	requireDomainError(t, f.service.AcknowledgeCleanup(ctx, fence), core.ErrInUse)
	got, err := f.service.GetCleanup(ctx, intent.ID)
	if err != nil || got.Acknowledged {
		t.Fatal("отклонённый ack подтвердил физическую очистку current", err)
	}
	wrong := fence
	wrong.ContentHash = "foreign"
	_, err = f.service.AuthorizeFileCleanup(ctx, wrong)
	requireDomainError(t, err, core.ErrStale)
}
