package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestReconcileSQLitePostCommitCancellationPreservesUnchangedExecution(t *testing.T) {
	for _, owned := range []bool{false, true} {
		for _, outcome := range []string{"unrelated_commit", "changed_fence", "rollback"} {
			t.Run(map[bool]string{false: "write", true: "owned"}[owned]+"/"+outcome, func(t *testing.T) {
				f := newDomainFixture(t, 1)
				parent, outputs := startObservationOutputs(t, f)
				owner := openOutputObserver(t, f.service, outputs[0])
				if err := owner.Observe(context.Background(), core.OutputObservation{State: core.OutputPublishing}); err != nil {
					t.Fatal(err)
				}
				publication := activateRuntimeSource(t, f)
				before := observationRows(t, f.store.sqlDB)
				beforeSession := readSession(t, f.store, parent.SessionID)
				beforeRAM, err := f.service.RuntimeSnapshot(context.Background(), observationFence(parent))
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				failure := errors.New("подтверждённый откат")
				writeCalls, callbackCalls := 0, 0
				committed := false
				write := func() (bool, error) {
					writeCalls++
					uncertain, writeErr := f.store.writeWithBudget(ctx, func(r core.Repositories) error {
						callbackCalls++
						if outcome == "changed_fence" {
							session, err := r.Runtime.Session(ctx, parent.SessionID)
							if err != nil {
								return err
							}
							old := session.Revision
							session.Lifecycle++
							session.Revision++
							return r.Runtime.UpdateSession(ctx, session, old)
						}
						profile, err := r.Config.Profile(ctx, f.profile.ID)
						if err != nil {
							return err
						}
						old := profile.Revision
						profile.Name = "Изменённая конфигурация следующего запуска"
						profile.Revision++
						profile.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
						if err := r.Config.UpdateProfile(ctx, profile, old); err != nil {
							return err
						}
						if outcome == "rollback" {
							return failure
						}
						return nil
					})
					committed = uncertain && writeErr == nil
					// Отмена наступает только после завершения настоящего COMMIT или rollback.
					cancel()
					return uncertain, writeErr
				}
				if owned {
					err = f.store.authority.GuardOwnedWrite(ctx, f.store, owner, f.store.options.RetryTimeout, write)
				} else {
					err = f.store.authority.GuardWrite(ctx, f.store, f.store.options.RetryTimeout, write)
				}
				if outcome == "rollback" {
					if !errors.Is(err, failure) || committed {
						t.Fatalf("rollback выдан за COMMIT: committed=%t err=%v", committed, err)
					}
				} else if err != nil || !committed {
					t.Fatalf("COMMIT не подтверждён до отмены: committed=%t err=%v", committed, err)
				}
				if writeCalls != 1 || callbackCalls != 1 || !errors.Is(ctx.Err(), context.Canceled) {
					t.Fatalf("сверка повторила запись либо потеряла отмену: write=%d callback=%d ctx=%v", writeCalls, callbackCalls, ctx.Err())
				}
				after := observationRows(t, f.store.sqlDB)
				for _, table := range []string{"command_receipts", "cleanup_intents", "session_holds", "output_holds", "outputs", "events"} {
					if !reflect.DeepEqual(before[table], after[table]) {
						t.Errorf("сверка изменила %s", table)
					}
				}
				if outcome == "changed_fence" {
					if got := readSession(t, f.store, parent.SessionID); got.Lifecycle != beforeSession.Lifecycle+1 {
						t.Fatal("смена fence не сохранена в SQLite")
					}
					requireRevokedLease(t, publication)
					if owner.Valid() {
						t.Fatal("смена долговечного fence сохранила наблюдателя")
					}
				} else {
					if got := readSession(t, f.store, parent.SessionID); !reflect.DeepEqual(beforeSession, got) {
						t.Fatal("посторонняя запись изменила действующую Session")
					}
					if !publication.Valid() || !owner.Valid() {
						t.Fatal("отмена завершённой записи отозвала неизменённое выполнение")
					}
					afterRAM, err := f.service.RuntimeSnapshot(context.Background(), observationFence(parent))
					if err != nil || !reflect.DeepEqual(beforeRAM, afterRAM) {
						t.Fatalf("неизменённое выполнение потеряло RAM: %v", err)
					}
					acted := false
					if err := publication.Do(context.Background(), func() error { acted = true; return nil }); err != nil || !acted {
						t.Fatal("проверенный источник потерял допуск действий", err)
					}
					if outcome == "rollback" {
						if !reflect.DeepEqual(before, after) {
							t.Fatal("подтверждённый rollback изменил долговечные строки")
						}
					} else {
						if reflect.DeepEqual(before["profiles"], after["profiles"]) {
							t.Fatal("посторонняя запись не сохранена")
						}
						if err := f.store.View(context.Background(), func(r core.Repositories) error {
							profile, err := r.Config.Profile(context.Background(), f.profile.ID)
							if err == nil && (profile.Name != "Изменённая конфигурация следующего запуска" || profile.Revision != f.profile.Revision+1) {
								t.Error("SQLite не сохранила точную конфигурацию и CAS")
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
				}
				if inUse := f.store.sqlDB.Stats().InUse; inUse != 0 {
					t.Fatalf("запись или сверка удерживает соединения: %d", inUse)
				}
				if err := f.store.Close(); err != nil {
					t.Fatal(err)
				}
				if publication.Valid() || owner.Valid() {
					t.Fatal("Close сохранил допуск после сверки")
				}
			})
		}
	}
}
