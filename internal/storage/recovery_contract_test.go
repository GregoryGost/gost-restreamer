package storage

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestRecoveryTraversesAllOldPreparingVariantsAndPreservesCurrentAndReady(t *testing.T) {
	f := newDomainFixture(t, 0)
	// Проверка пересекает границу страницы и не задаёт секундный SLO для recovery.
	// Отдельный конечный бюджет учитывает обработку 501 строки под race detector.
	f.store.options.RetryTimeout = 10 * time.Second
	ctx := context.Background()
	current, err := f.service.ReserveVariant(ctx, core.FallbackVariant{AssetID: f.asset.ID, SourceHash: f.asset.SourceHash, Profile: f.profile.Profile, GeneratorVersion: "generator-v2", ManagedPath: "variants/current-preparation.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	currentAsset, err := f.service.ReserveAsset(ctx, core.FallbackAsset{Name: "Текущий оригинал", Kind: core.AssetImage, OriginalPath: "originals/current-preparation.png"})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	oldAsset := currentAsset
	oldAsset.Record = core.Record{ID: "retired-preparing-asset", Revision: 1, CreatedAt: base, UpdatedAt: base}
	oldAsset.Incarnation, oldAsset.OriginalPath = "retired-owner", "originals/retired-preparation.png"
	wantPaths := map[string]bool{oldAsset.OriginalPath: true}
	oldVariants := make([]core.FallbackVariant, core.MaxListLimit+1)
	if err := f.store.Write(ctx, func(r core.Repositories) error {
		if err := r.Artifacts.InsertAsset(ctx, oldAsset); err != nil {
			return err
		}
		for i := range oldVariants {
			v := current
			v.Record = core.Record{ID: core.ID(fmt.Sprintf("retired-variant-%04d", i)), Revision: 1, CreatedAt: base, UpdatedAt: base}
			v.Incarnation, v.ManagedPath = "retired-owner", fmt.Sprintf("variants/retired-%04d.mp4", i)
			oldVariants[i] = v
			wantPaths[v.ManagedPath] = true
			if err := r.Artifacts.InsertVariant(ctx, v); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := f.service.Recover(ctx); err != nil {
		t.Logf("Recover: %s, бюджет записи %s, ошибка %v", time.Since(started), f.store.options.RetryTimeout, err)
		for _, original := range oldVariants {
			got, readErr := f.service.GetVariant(ctx, original.ID)
			if readErr != nil || !reflect.DeepEqual(got, original) {
				t.Fatalf("ошибка recovery оставила частичное изменение варианта: %+v, %v", got, readErr)
			}
		}
		got, readErr := f.service.GetAsset(ctx, oldAsset.ID)
		if readErr != nil || !reflect.DeepEqual(got, oldAsset) {
			t.Fatalf("ошибка recovery оставила частичное изменение оригинала: %+v, %v", got, readErr)
		}
		page, readErr := f.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 1}})
		if readErr != nil || len(page.Items) != 0 {
			t.Fatalf("ошибка recovery оставила cleanup: %+v, %v", page, readErr)
		}
		t.Fatal(err)
	}
	t.Logf("Recover: %s, бюджет записи %s", time.Since(started), f.store.options.RetryTimeout)
	for _, original := range oldVariants {
		got, err := f.service.GetVariant(ctx, original.ID)
		expected := original
		expected.State, expected.Revision, expected.UpdatedAt = core.VariantFailed, original.Revision+1, got.UpdatedAt
		if err != nil || !reflect.DeepEqual(got, expected) {
			t.Fatalf("старое поколение не прервано без замены: %+v, %v", got, err)
		}
		requireDomainError(t, f.service.FailVariant(ctx, got.ID, got.Revision, got.AttemptToken), core.ErrStale)
	}
	failedAsset, err := f.service.GetAsset(ctx, oldAsset.ID)
	if err != nil || failedAsset.State != core.AssetFailed || failedAsset.OriginalPath != oldAsset.OriginalPath || failedAsset.AttemptToken != oldAsset.AttemptToken {
		t.Fatalf("старый оригинал не прерван: %+v, %v", failedAsset, err)
	}
	for _, original := range []core.FallbackVariant{current, f.variant} {
		got, err := f.service.GetVariant(ctx, original.ID)
		if err != nil || !reflect.DeepEqual(got, original) {
			t.Fatalf("recovery затронул текущее или READY-поколение: %+v, %v", got, err)
		}
	}
	for _, original := range []core.FallbackAsset{currentAsset, f.asset} {
		got, err := f.service.GetAsset(ctx, original.ID)
		if err != nil || !reflect.DeepEqual(got, original) {
			t.Fatalf("recovery затронул текущий или READY-оригинал: %+v, %v", got, err)
		}
	}
	readIntents := func() []core.CleanupIntent {
		t.Helper()
		var result []core.CleanupIntent
		request := core.PageRequest{Query: core.ListQuery{Limit: 173, PendingOnly: true}}
		for {
			page, err := f.service.ListCleanups(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			result = append(result, page.Items...)
			if page.NextCursor == "" {
				return result
			}
			request.Cursor = page.NextCursor
		}
	}
	intents := readIntents()
	if len(intents) != len(wantPaths) {
		t.Fatalf("cleanup потерял или продублировал пути: %d, нужно %d", len(intents), len(wantPaths))
	}
	seen := map[string]bool{}
	for _, intent := range intents {
		if intent.Kind != core.CleanupFile || !wantPaths[intent.ManagedPath] || seen[intent.ManagedPath] || intent.Acknowledged {
			t.Fatalf("cleanup не соответствует точному старому пути: %+v", intent)
		}
		seen[intent.ManagedPath] = true
	}
	if err := f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(intents, readIntents()) {
		t.Fatal("повторное recovery переписало или продублировало cleanup")
	}
	if _, err := f.service.PublishVariant(ctx, current.ID, current.Revision, current.AttemptToken, "current-content", 4096); err != nil {
		t.Fatalf("recovery отозвало текущего владельца подготовки: %v", err)
	}
}

func TestRecoveryCleanupFailureRollsBackArtifactState(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	v := f.variant
	v.ID, v.Revision, v.Incarnation, v.State, v.ManagedPath, v.SizeBytes, v.ContentHash = "retired-preparation", 1, "retired-owner", core.VariantPreparing, "variants/retired-failure.mp4", 0, ""
	if err := f.store.Write(ctx, func(r core.Repositories) error { return r.Artifacts.InsertVariant(ctx, v) }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.sqlDB.ExecContext(ctx, `CREATE TRIGGER reject_file_cleanup BEFORE INSERT ON cleanup_intents WHEN NEW.kind = 'file' BEGIN SELECT RAISE(ABORT, 'synthetic cleanup failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Recover(ctx); err == nil {
		t.Fatal("отказ записи cleanup не отклонил recovery")
	}
	got, err := f.service.GetVariant(ctx, v.ID)
	if err != nil || !reflect.DeepEqual(got, v) {
		t.Fatalf("неудачное recovery изменило подготовку: %+v, %v", got, err)
	}
	intents, err := f.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 10}})
	if err != nil || len(intents.Items) != 0 {
		t.Fatalf("неудачное recovery оставило cleanup: %+v, %v", intents, err)
	}
	if _, err := f.store.sqlDB.ExecContext(ctx, "DROP TRIGGER reject_file_cleanup"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = f.service.GetVariant(ctx, v.ID)
	if err != nil || got.State != core.VariantFailed || got.Revision != v.Revision+1 {
		t.Fatalf("recovery после отката не завершилось: %+v, %v", got, err)
	}
}
