package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestAssetAndVariantPreparationRequireCurrentAttempt(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	a, err := f.service.ReserveAsset(ctx, core.FallbackAsset{Name: "Изображение", Kind: core.AssetImage, OriginalPath: "originals/second.png", SourceHash: "unverified", SizeBytes: 100, State: core.AssetReady})
	if err != nil {
		t.Fatal(err)
	}
	if a.State != core.AssetPreparing || a.SizeBytes != 0 || a.SourceHash != "" || a.AttemptToken == "" {
		t.Fatalf("резервирование опубликовало непроверенный файл: %+v", a)
	}
	_, err = f.service.PublishAsset(ctx, a.ID, a.Revision, "old-attempt", "hash", 10, core.SourceFormat{ImageFormat: "png"})
	requireDomainError(t, err, core.ErrStale)
	_, err = f.service.PublishAsset(ctx, a.ID, a.Revision, a.AttemptToken, "hash", 0, core.SourceFormat{ImageFormat: "png"})
	requireDomainError(t, err, core.ErrInvalid)
	if got, err := f.service.GetAsset(ctx, a.ID); err != nil || !reflect.DeepEqual(got, a) {
		t.Fatalf("ошибка публикации изменила reservation: %+v, %v", got, err)
	}
	if err := f.service.FailAsset(ctx, a.ID, a.Revision, a.AttemptToken); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.PublishAsset(ctx, a.ID, a.Revision, a.AttemptToken, "late-hash", 10, core.SourceFormat{ImageFormat: "png"})
	requireDomainError(t, err, core.ErrStale)
	video, err := f.service.ReserveAsset(ctx, core.FallbackAsset{Name: "Видео без аудио", Kind: core.AssetVideo, OriginalPath: "originals/no-audio.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	inputVideo := storageProfile().Format.Video
	video, err = f.service.PublishAsset(ctx, video.ID, video.Revision, video.AttemptToken, "video-source-hash", 100, core.SourceFormat{Video: &inputVideo})
	if err != nil || video.SourceFormat.Audio != nil {
		t.Fatalf("оригинал без AAC ошибочно отклонён: %+v, %v", video, err)
	}
	v, err := f.service.ReserveVariant(ctx, core.FallbackVariant{AssetID: f.asset.ID, SourceHash: f.asset.SourceHash, Profile: f.profile.Profile, GeneratorVersion: "generator-v1", ManagedPath: "variants/preparing.nut"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.PublishVariant(ctx, v.ID, v.Revision, "old-attempt", "hash", 100)
	requireDomainError(t, err, core.ErrStale)
	if err := f.service.FailVariant(ctx, v.ID, v.Revision, v.AttemptToken); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.PublishVariant(ctx, v.ID, v.Revision, v.AttemptToken, "hash", 100)
	requireDomainError(t, err, core.ErrStale)
}

func TestStartRejectsEveryNonmatchingPreparedBindingAtomically(t *testing.T) {
	for _, mismatch := range []string{"generator", "profile", "preparing", "failed"} {
		t.Run(mismatch, func(t *testing.T) {
			f := newDomainFixture(t, 1)
			ctx := context.Background()
			request := f.startRequest("start-with-binding")
			if mismatch == "generator" {
				request.Bindings[0].GeneratorVersion = "other-generator"
			} else {
				v := core.FallbackVariant{AssetID: f.asset.ID, SourceHash: f.asset.SourceHash, Profile: f.profile.Profile, GeneratorVersion: f.variant.GeneratorVersion, ManagedPath: "variants/binding.nut"}
				if mismatch == "profile" {
					v.Profile.Format.Video.Width++
				}
				var err error
				v, err = f.service.ReserveVariant(ctx, v)
				if err != nil {
					t.Fatal(err)
				}
				if mismatch == "failed" {
					if err := f.service.FailVariant(ctx, v.ID, v.Revision, v.AttemptToken); err != nil {
						t.Fatal(err)
					}
				} else if mismatch != "preparing" {
					v, err = f.service.PublishVariant(ctx, v.ID, v.Revision, v.AttemptToken, "other-content", 100)
					if err != nil {
						t.Fatal(err)
					}
				}
				request.Bindings[0].VariantID = v.ID
			}
			_, err := f.service.StartSession(ctx, request)
			requireDomainError(t, err, core.ErrNotReady)
			if err := f.store.View(ctx, func(r core.Repositories) error {
				sessions, err := r.Runtime.Sessions(ctx, core.ListQuery{Limit: core.MaxListLimit, StreamID: f.stream.ID})
				if err != nil {
					return err
				}
				if len(sessions) != 0 {
					t.Fatal("отклонённый binding оставил частичную Session")
				}
				_, err = r.Runtime.Receipt(ctx, request.RequestID)
				requireDomainError(t, err, core.ErrNotFound)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestVariantSourceIdentityCannotDivergeFromAsset(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	_, err := f.service.ReserveVariant(ctx, core.FallbackVariant{AssetID: f.asset.ID, SourceHash: "wrong-source-hash", Profile: f.profile.Profile, GeneratorVersion: f.variant.GeneratorVersion, ManagedPath: "variants/wrong-source.nut"})
	requireDomainError(t, err, core.ErrNotReady)
	_, err = f.store.sqlDB.ExecContext(ctx, "UPDATE fallback_variants SET source_hash = ? WHERE id = ?", "wrong-source-hash", f.variant.ID)
	requireDomainError(t, translate(err), core.ErrInUse)
	got, err := f.service.GetVariant(ctx, f.variant.ID)
	if err != nil || !reflect.DeepEqual(got, f.variant) {
		t.Fatalf("неудачная смена источника изменила вариант: %+v, %v", got, err)
	}
}

func TestExactBindingPinsRequestedGenerationRatherThanNewest(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	p := f.profile.Profile
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 120, Denominator: 2}
	v, err := f.service.ReserveVariant(ctx, core.FallbackVariant{AssetID: f.asset.ID, SourceHash: f.asset.SourceHash, Profile: p, GeneratorVersion: f.variant.GeneratorVersion, ManagedPath: "variants/newest.nut"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.PublishVariant(ctx, v.ID, v.Revision, v.AttemptToken, "newest-hash", 500); err != nil {
		t.Fatal(err)
	}
	start := startDomainSession(t, f)
	o := readOutputs(t, f.store, start.SessionID)[0]
	if o.Snapshot.Artifact.VariantID != f.variant.ID || o.Snapshot.Artifact.ContentHash != f.variant.ContentHash {
		t.Fatalf("start сам выбрал latest READY: %+v", o.Snapshot.Artifact)
	}
}

func TestDeletedGenerationPathCannotBeReusedAndCleanupIsImmutable(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	if err := f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision); err != nil {
		t.Fatal(err)
	}
	cleanups, err := f.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 10, PendingOnly: true}})
	if err != nil || len(cleanups.Items) != 1 {
		t.Fatalf("намерение файловой очистки: %+v, %v", cleanups, err)
	}
	intent := cleanups.Items[0]
	if intent.Kind != core.CleanupFile || intent.ManagedPath != f.variant.ManagedPath {
		t.Fatalf("очистка потеряла неизменяемый путь: %+v", intent)
	}
	fence := core.CleanupFenceFor(intent)
	fence.ManagedPath = "variants/foreign.nut"
	requireDomainError(t, f.service.AcknowledgeCleanup(ctx, fence), core.ErrStale)
	if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(intent)); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.ReserveVariant(ctx, core.FallbackVariant{AssetID: f.asset.ID, SourceHash: f.asset.SourceHash, Profile: f.profile.Profile, GeneratorVersion: "generator-v2", ManagedPath: f.variant.ManagedPath})
	requireDomainError(t, err, core.ErrConflict)
	if _, err := f.service.GetVariant(ctx, f.variant.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("удалённый metadata row сохранился: %v", err)
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
