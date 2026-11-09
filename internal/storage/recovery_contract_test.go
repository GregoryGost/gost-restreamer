package storage

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestRecoveryTraversesAllOldQueuedGenerationsAndPreservesCurrentAndReady(t *testing.T) {
	f := newDomainFixture(t, 0)
	f.store.options.RetryTimeout = 10 * time.Second
	ctx := context.Background()
	currentAsset, err := f.service.CreateAsset(ctx, "Текущая подготовка")
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: currentAsset.ID, ExpectedRevision: currentAsset.Revision, Kind: core.AssetImage, GeneratorVersion: "generator"})
	if err != nil {
		t.Fatal(err)
	}
	old := make([]core.FallbackGeneration, core.MaxListLimit+1)
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err = f.store.Write(ctx, func(r core.Repositories) error {
		for i := range old {
			id := core.ID(fmt.Sprintf("retired-generation-%04d", i))
			a := core.FallbackAsset{ID: core.ID(fmt.Sprintf("retired-asset-%04d", i)), Revision: 1, CreatedAt: base, UpdatedAt: base, Name: "Прерванная подготовка", State: core.AssetPreparing, PendingGenerationID: id}
			if e := r.Artifacts.InsertAsset(ctx, a); e != nil {
				return e
			}
			g := current
			g.Record = core.Record{ID: id, Revision: 1, CreatedAt: base, UpdatedAt: base}
			g.AssetID = a.ID
			g.Incarnation = "retired-owner"
			g.OriginalPath = core.GenerationDirectory(id) + "/original"
			g.StagingPath = g.OriginalPath + ".stage"
			g.RequiredProfiles = nil
			old[i] = g
			if e := r.Artifacts.InsertGeneration(ctx, g); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	for _, before := range old {
		g, e := f.service.GetGeneration(ctx, before.ID)
		if e != nil || g.State != core.PreparationInterrupted || g.Revision != before.Revision+1 || g.Issued || g.CleanupPending {
			t.Fatalf("старое поколение не прервано: %+v,%v", g, e)
		}
		a, e := f.service.GetAsset(ctx, before.AssetID)
		if e != nil || a.PendingGenerationID != "" || a.State != core.AssetFailed {
			t.Fatalf("старый pending не освобождён: %+v,%v", a, e)
		}
	}
	got, err := f.service.GetGeneration(ctx, current.ID)
	if err != nil || !reflect.DeepEqual(got, current) {
		t.Fatalf("recovery отозвало текущую подготовку: %+v,%v", got, err)
	}
	ready, err := f.service.GetAsset(ctx, f.asset.ID)
	if err != nil || !reflect.DeepEqual(ready, f.asset) {
		t.Fatalf("recovery изменило опубликованный current: %+v,%v", ready, err)
	}
	if err = f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	for _, before := range old {
		g, e := f.service.GetGeneration(ctx, before.ID)
		if e != nil || g.Revision != 2 {
			t.Fatal("повтор recovery изменил историю", e)
		}
	}
}

func TestRecoveryFailureRollsBackGenerationAndPendingPointer(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	a, err := f.service.CreateAsset(ctx, "Старая подготовка")
	if err != nil {
		t.Fatal(err)
	}
	g, err := f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage, GeneratorVersion: "generator"})
	if err != nil {
		t.Fatal(err)
	}
	// Другая incarnation создаётся только в изолированной фикстуре аварийного восстановления.
	g.Incarnation = "retired-owner"
	data, err := core.EncodeMetadata(g)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.sqlDB.ExecContext(ctx, "UPDATE fallback_generations SET incarnation=?,payload=? WHERE id=?", g.Incarnation, data, g.ID); err != nil {
		t.Fatal(err)
	}
	a, err = f.service.GetAsset(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.sqlDB.ExecContext(ctx, `CREATE TRIGGER reject_pending_release BEFORE UPDATE ON fallback_assets WHEN NEW.pending_generation_id IS NULL BEGIN SELECT RAISE(ABORT, 'synthetic release failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = f.service.Recover(ctx); err == nil {
		t.Fatal("отказ снятия pending не отклонил recovery")
	}
	got, err := f.service.GetGeneration(ctx, g.ID)
	if err != nil || !reflect.DeepEqual(got, g) {
		t.Fatalf("неудачное recovery частично изменило generation: %+v,%v", got, err)
	}
	after, err := f.service.GetAsset(ctx, a.ID)
	if err != nil || !reflect.DeepEqual(after, a) {
		t.Fatal("неудачное recovery изменило asset", err)
	}
	if _, err = f.store.sqlDB.ExecContext(ctx, "DROP TRIGGER reject_pending_release"); err != nil {
		t.Fatal(err)
	}
	if err = f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = f.service.GetGeneration(ctx, g.ID)
	if err != nil || got.State != core.PreparationInterrupted {
		t.Fatal("recovery после отката не завершилось", err)
	}
}
