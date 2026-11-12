package storage

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет постраничное прерывание более MaxListLimit прежних подготовок, сохраняя текущую
// подготовку, опубликованную заглушку и историю при повторном Recover.
func TestRecoveryTraversesAllOldQueuedGenerationsAndPreservesCurrentAndReady(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	f.store.options.RetryTimeout = 10 * time.Second
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// currentAsset содержит заглушку с подготовкой текущего открытия хранилища. err сообщает об
	// ошибке создания стабильной заглушки.
	currentAsset, err := f.service.CreateAsset(ctx, "Текущая подготовка")
	if err != nil {
		t.Fatal(err)
	}
	// current сохраняет подготовку текущего владельца, которую Recover должен оставить неизменной.
	current, err := f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: currentAsset.ID, ExpectedRevision: currentAsset.Revision, Kind: core.AssetImage, GeneratorVersion: "generator"})
	if err != nil {
		t.Fatal(err)
	}
	// old содержит больше страницы подготовок прежнего владельца для проверки полного обхода.
	old := make([]core.FallbackGeneration, core.MaxListLimit+1)
	// base задаёт одинаковое исходное время сохраняемых старых записей.
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err = f.store.Write(ctx, func(r core.Repositories) error {
		// Создаёт больше страницы QUEUED-поколений прежнего владельца с PendingGenerationID для проверки
		// полного обхода восстановления.

		// i указывает позицию создаваемой подготовки прежнего владельца.
		for i := range old {
			// id задаёт уникальный идентификатор очередного старого поколения.
			id := core.ID(fmt.Sprintf("retired-generation-%04d", i))
			// a содержит заглушку, чей PendingGenerationID должен быть снят при прерывании
			// подготовки.
			a := core.FallbackAsset{ID: core.ID(fmt.Sprintf("retired-asset-%04d", i)), Revision: 1, CreatedAt: base, UpdatedAt: base, Name: "Прерванная подготовка", State: core.AssetPreparing, PendingGenerationID: id}
			// e сообщает об ошибке вставки заглушки прежнего владельца.
			if e := r.Artifacts.InsertAsset(ctx, a); e != nil {
				return e
			}
			// g содержит поколение подготовки прежнего владельца.
			g := current
			g.Record = core.Record{ID: id, Revision: 1, CreatedAt: base, UpdatedAt: base}
			g.AssetID = a.ID
			g.Incarnation = "retired-owner"
			g.OriginalPath = core.GenerationDirectory(id) + "/original"
			g.StagingPath = g.OriginalPath + ".stage"
			g.RequiredProfiles = nil
			old[i] = g
			// e сообщает об ошибке вставки поколения прежнего владельца.
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
	// before сохраняет старое поколение до восстановления для сравнения ревизии.
	for _, before := range old {
		// g содержит поколение подготовки прежнего владельца. e сообщает об ошибке чтения
		// полного поколения подготовки.
		g, e := f.service.GetGeneration(ctx, before.ID)
		if e != nil || g.State != core.PreparationInterrupted || g.Revision != before.Revision+1 || g.Issued || g.CleanupPending {
			t.Fatalf("старое поколение не прервано: %+v,%v", g, e)
		}
		// a содержит заглушку, чей PendingGenerationID должен быть снят при прерывании
		// подготовки.
		a, e := f.service.GetAsset(ctx, before.AssetID)
		if e != nil || a.PendingGenerationID != "" || a.State != core.AssetFailed {
			t.Fatalf("старый pending не освобождён: %+v,%v", a, e)
		}
	}
	// got содержит прочитанное сохраняемое значение для проверки результата операции.
	got, err := f.service.GetGeneration(ctx, current.ID)
	if err != nil || !reflect.DeepEqual(got, current) {
		t.Fatalf("recovery отозвало текущую подготовку: %+v,%v", got, err)
	}
	// ready содержит ранее опубликованную заглушку, которую восстановление должно сохранить.
	ready, err := f.service.GetAsset(ctx, f.asset.ID)
	if err != nil || !reflect.DeepEqual(ready, f.asset) {
		t.Fatalf("recovery изменило опубликованный current: %+v,%v", ready, err)
	}
	if err = f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	// before сохраняет старое поколение до восстановления для сравнения ревизии.
	for _, before := range old {
		// g содержит поколение подготовки прежнего владельца. e сообщает об ошибке чтения
		// полного поколения подготовки.
		g, e := f.service.GetGeneration(ctx, before.ID)
		if e != nil || g.Revision != 2 {
			t.Fatal("повтор recovery изменил историю", e)
		}
	}
}

// Проверяет атомарный откат прерывания поколения и снятия указателя PendingGenerationID при ошибке
// триггера SQLite и успешный повтор после устранения отказа.
func TestRecoveryFailureRollsBackGenerationAndPendingPointer(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// a содержит заглушку, чей PendingGenerationID должен быть снят при прерывании подготовки. err
	// сообщает об ошибке создания стабильной заглушки.
	a, err := f.service.CreateAsset(ctx, "Старая подготовка")
	if err != nil {
		t.Fatal(err)
	}
	// g содержит поколение подготовки прежнего владельца.
	g, err := f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage, GeneratorVersion: "generator"})
	if err != nil {
		t.Fatal(err)
	}
	// Другая incarnation создаётся только в изолированной фикстуре аварийного восстановления.
	g.Incarnation = "retired-owner"
	// data содержит сериализованное поколение с incarnation прежнего владельца.
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
	// got содержит прочитанное сохраняемое значение для проверки результата операции.
	got, err := f.service.GetGeneration(ctx, g.ID)
	if err != nil || !reflect.DeepEqual(got, g) {
		t.Fatalf("неудачное recovery частично изменило generation: %+v,%v", got, err)
	}
	// after содержит заглушку после неудачного восстановления для проверки отката.
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
