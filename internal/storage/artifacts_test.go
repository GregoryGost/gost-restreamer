package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestPreparationRequiresExactAttemptAndPreservesCurrentOnFailure(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	g, err := f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: f.asset.ID, ExpectedRevision: f.asset.Revision, Kind: core.AssetImage, GeneratorVersion: "generator-v2"})
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
	wrong := fence
	wrong.AttemptToken = "old-attempt"
	_, err = f.service.PublishPreparation(ctx, wrong, core.GenerationPublication{SourceHash: "hash", SizeBytes: 10, SourceFormat: core.SourceFormat{ImageFormat: "png"}})
	requireDomainError(t, err, core.ErrStale)
	p := fixturePublication(g, "hash", "result")
	p.SizeBytes = 0
	_, err = f.service.PublishPreparation(ctx, fence, p)
	requireDomainError(t, err, core.ErrInvalid)
	current, err := f.service.GetAsset(ctx, f.asset.ID)
	if err != nil || current.CurrentGenerationID != f.asset.CurrentGenerationID || current.SourceHash != f.asset.SourceHash {
		t.Fatalf("ошибка публикации изменила current: %+v, %v", current, err)
	}
	if err = f.service.CancelPreparation(ctx, fence, "Подготовка отменена"); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.PublishPreparation(ctx, fence, fixturePublication(g, "late", "late"))
	requireDomainError(t, err, core.ErrStale)
	if err = f.service.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.PublishAsset(ctx, f.asset.ID, f.asset.Revision, f.asset.AttemptToken, "bypass", 10, core.SourceFormat{ImageFormat: "png"})
	requireDomainError(t, err, core.ErrInvalid)
	_, err = f.service.ReserveVariant(ctx, f.variant)
	requireDomainError(t, err, core.ErrInvalid)
	_, err = f.service.PublishVariant(ctx, f.variant.ID, f.variant.Revision, f.variant.AttemptToken, "bypass", 10)
	requireDomainError(t, err, core.ErrInvalid)
	requireDomainError(t, f.service.FailAsset(ctx, f.asset.ID, f.asset.Revision, f.asset.AttemptToken), core.ErrInvalid)
	requireDomainError(t, f.service.FailVariant(ctx, f.variant.ID, f.variant.Revision, f.variant.AttemptToken), core.ErrInvalid)
}

func TestStartRejectsEveryNonmatchingPreparedBindingAtomically(t *testing.T) {
	for _, mismatch := range []string{"generator", "profile", "generation", "variant"} {
		t.Run(mismatch, func(t *testing.T) {
			f := newDomainFixture(t, 1)
			ctx := context.Background()
			request := f.startRequest("start-with-binding")
			switch mismatch {
			case "generator":
				request.Bindings[0].GeneratorVersion = "other-generator"
			case "generation":
				request.Bindings[0].GenerationID = "other-generation"
			case "variant":
				request.Bindings[0].VariantID = "missing-variant"
			case "profile":
				p := f.profile
				p.Profile.Format.Video.Width++
				if _, err := f.service.UpdateProfile(ctx, p, p.Revision); err != nil {
					t.Fatal(err)
				}
			}
			_, err := f.service.StartSession(ctx, request)
			if mismatch == "variant" {
				requireDomainError(t, err, core.ErrNotFound)
			} else {
				requireDomainError(t, err, core.ErrNotReady)
			}
			if err = f.store.View(ctx, func(r core.Repositories) error {
				ss, e := r.Runtime.Sessions(ctx, core.ListQuery{Limit: 10, StreamID: f.stream.ID})
				if e != nil {
					return e
				}
				if len(ss) != 0 {
					t.Fatal("отклонённый binding оставил частичную Session")
				}
				_, e = r.Runtime.Receipt(ctx, request.RequestID)
				requireDomainError(t, e, core.ErrNotFound)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestVariantSourceIdentityCannotDivergeFromGeneration(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	_, err := f.store.sqlDB.ExecContext(ctx, "UPDATE fallback_variants SET source_hash = ? WHERE id = ?", "wrong-source-hash", f.variant.ID)
	requireDomainError(t, translate(err), core.ErrInUse)
	got, err := f.service.GetVariant(ctx, f.variant.ID)
	if err != nil || !reflect.DeepEqual(got, f.variant) {
		t.Fatalf("смена источника изменила вариант: %+v, %v", got, err)
	}
}
func TestFreshStartRejectsRetiredGenerationWithoutSelectingNewest(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	old := f.startRequest("old-binding")
	a, v := publishFixtureGeneration(t, f.service, f.asset, f.asset.SourceHash, "new-content")
	_, err := f.service.StartSession(ctx, old)
	requireDomainError(t, err, core.ErrNotReady)
	f.asset, f.variant = a, v
	start := startDomainSession(t, f)
	output := readOutputs(t, f.store, start.SessionID)[0]
	if output.Snapshot.Artifact.GenerationID != v.GenerationID || output.Snapshot.Artifact.VariantID != v.ID {
		t.Fatal("start не сохранил точный запрошенный комплект")
	}
}
func TestDeletedGenerationPathCannotBeReusedAndCleanupIsImmutable(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	old, err := f.service.GetGeneration(ctx, f.variant.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := publishFixtureGeneration(t, f.service, f.asset, "new-source", "new-content")
	if a.CurrentGenerationID == old.ID {
		t.Fatal("замена переиспользовала поколение")
	}
	if err = f.service.DeleteGeneration(ctx, old.ID, old.Revision); err != nil {
		t.Fatal(err)
	}
	cleanups, err := f.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 20, PendingOnly: true}})
	if err != nil || len(cleanups.Items) != 5 {
		t.Fatalf("намерения файловой очистки: %+v, %v", cleanups, err)
	}
	intent := cleanups.Items[0]
	wrong := core.CleanupFenceFor(intent)
	wrong.ManagedPath = "variants/foreign.mp4"
	requireDomainError(t, f.service.AcknowledgeCleanup(ctx, wrong), core.ErrStale)
	if err = f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(intent)); err != nil {
		t.Fatal(err)
	}
	files, err := f.service.ListGenerationFiles(ctx, core.PageRequest{Query: core.ListQuery{Limit: 20, GenerationID: old.ID}})
	if err != nil {
		t.Fatal(err)
	}
	duplicate := files.Items[0]
	duplicate.ID = "new-file-id"
	err = f.store.Write(ctx, func(r core.Repositories) error { return r.Artifacts.InsertOwnedFile(ctx, duplicate) })
	requireDomainError(t, err, core.ErrConflict)
	if _, err = f.service.GetVariant(ctx, f.variant.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("удалённые метаданные сохранились: %v", err)
	}
}

func TestCorruptMetadataIsDistinctFromMissingAndDoesNotLeakRows(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	if _, err := f.store.sqlDB.ExecContext(ctx, "UPDATE profiles SET payload = ? WHERE id = ?", []byte(`{"Version":999,"Payload":{}}`), f.profile.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_, err := f.service.GetProfile(ctx, f.profile.ID)
		requireDomainError(t, err, core.ErrCorrupt)
		if errors.Is(err, core.ErrNotFound) {
			t.Fatal("повреждение подменено отсутствием")
		}
		if f.store.sqlDB.Stats().InUse != 0 {
			t.Fatal("ошибка декодирования удерживает соединение")
		}
	}
}

func TestStoredTimestampsNormalizeUTCAndRejectUnrepresentableRange(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	input := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.FixedZone("offset", 3*60*60))
	value := core.Stream{Record: core.Record{ID: "timestamp-stream", Revision: 1, CreatedAt: input, UpdatedAt: input}, Name: "Поток", PublicationKey: "synthetic-timestamp-key"}
	if err := s.Write(ctx, func(r core.Repositories) error { return r.Config.InsertStream(ctx, value) }); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, func(r core.Repositories) error {
		got, err := r.Config.Stream(ctx, value.ID)
		if err != nil {
			return err
		}
		if got.CreatedAt.Location() != time.UTC || !got.CreatedAt.Equal(input.Truncate(time.Microsecond)) {
			t.Fatalf("неверное UTC-время: %v", got.CreatedAt)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	value.ID = "overflow-time-stream"
	value.CreatedAt = time.Date(400000, 1, 1, 0, 0, 0, 0, time.UTC)
	value.UpdatedAt = value.CreatedAt
	requireDomainError(t, s.Write(ctx, func(r core.Repositories) error { return r.Config.InsertStream(ctx, value) }), core.ErrInvalid)
}
