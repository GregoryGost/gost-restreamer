package storage

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestTerminalPreparationDoesNotBlockAnyRuntimeAdmissionStage(t *testing.T) {
	for _, previous := range []bool{false, true} {
		for _, published := range []bool{false, true} {
			t.Run(fmt.Sprintf("previous=%t/published=%t", previous, published), func(t *testing.T) {
				f := newDomainFixture(t, 1)
				ctx := context.Background()
				g, fence := issuedFixturePreparation(t, f)
				owner, err := f.service.BindPreparationOwner(ctx, fence)
				if err != nil {
					t.Fatal(err)
				}
				finish, err := owner.Register(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer finish()
				if published {
					p := fixturePublication(g, "fresh-source", "fresh-content")
					if _, err := f.service.PublishPreparation(ctx, fence, p); err != nil {
						t.Fatal(err)
					}
					f.variant, err = f.service.GetVariant(ctx, p.Variants[0].ID)
					if err != nil {
						t.Fatal(err)
					}
				} else if err := f.service.CancelPreparation(ctx, fence, "Отмена с незавершённой операцией"); err != nil {
					t.Fatal(err)
				}
				if owner.Valid() {
					t.Fatal("терминальная подготовка сохранила допуск новых действий")
				}
				if previous {
					f = reopenFixture(t, f)
				}
				before, err := f.service.GetGeneration(ctx, g.ID)
				if err != nil || !before.Issued || !before.CleanupPending || !before.State.Terminal() {
					t.Fatalf("утрачено терминальное обязательство: %+v, %v", before, err)
				}
				start := startDomainSession(t, f)
				parent := executeCommand(t, f.service, start.RequestID)
				lease, err := f.service.BindExecutionOwner(ctx, parent)
				if err != nil || !lease.Valid() {
					t.Fatal("остатки подготовки запретили родительский допуск", err)
				}
				observer, err := f.service.OpenSessionObserver(ctx, parent)
				if err != nil || !observer.Valid() {
					t.Fatal("остатки подготовки запретили наблюдателя", err)
				}
				output := readOutputs(t, f.store, start.SessionID)[0]
				child := executeCommand(t, f.service, outputStartReceipt(t, f, output.ID).RequestID)
				childLease, err := f.service.BindExecutionOwner(ctx, child)
				if err != nil || !childLease.Valid() {
					t.Fatal("остатки подготовки запретили выход", err)
				}
				obs, err := f.service.ActivateOBS(ctx, f.stream.ID, f.stream.PublicationKey, func(*core.RuntimeLease) (func(), error) { return nil, nil })
				if err != nil || !obs.Valid() {
					t.Fatal("остатки подготовки запретили OBS", err)
				}
				if after, err := f.service.GetGeneration(ctx, g.ID); err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("новый запуск изменил прежнюю подготовку: %+v, %v", after, err)
				}
				pending, err := f.service.ListGenerations(ctx, core.PageRequest{Query: core.ListQuery{Limit: 10, PendingOnly: true}})
				if err != nil || len(pending.Items) != 1 || pending.Items[0].ID != g.ID {
					t.Fatalf("PendingOnly потерял терминальные остатки: %+v, %v", pending, err)
				}
				// Сверка lease после последующей записи также не должна возвращать старый запрет.
				if _, err := f.service.RenameAsset(ctx, f.asset.ID, readAssetRevision(t, f, ctx), "Переименованная заглушка"); err != nil {
					t.Fatal(err)
				}
				if !lease.Valid() || !childLease.Valid() || !obs.Valid() {
					t.Fatal("сверка записи отозвала новый допуск из-за старой подготовки")
				}
			})
		}
	}
}

func readAssetRevision(t *testing.T, f domainFixture, ctx context.Context) uint64 {
	t.Helper()
	a, err := f.service.GetAsset(ctx, f.asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a.Revision
}

func insertActivePreparationForAdmission(t *testing.T, f domainFixture, state core.PreparationState) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	g := core.FallbackGeneration{ID: "competing-preparation", Revision: 1, CreatedAt: now, UpdatedAt: now, AssetID: f.asset.ID, Kind: core.AssetImage, GeneratorVersion: "generator", Incarnation: f.store.Incarnation(), AttemptToken: "competing-token", State: state, OriginalPath: "fallback/competing-preparation/original", StagingPath: "fallback/competing-preparation/original.stage"}
	if state != core.PreparationQueued {
		g.Attempt = 1
	}
	if state == core.PreparationIssued {
		g.Issued, g.CleanupPending = true, true
	}
	if err := f.store.Write(context.Background(), func(r core.Repositories) error { return r.Artifacts.InsertGeneration(context.Background(), g) }); err != nil {
		t.Fatal(err)
	}
}

func TestActivePreparationExcludesStartClaimIssuanceAndLease(t *testing.T) {
	for _, state := range []core.PreparationState{core.PreparationQueued, core.PreparationClaimed, core.PreparationIssued} {
		for _, stage := range []string{"start", "claim", "issue", "lease"} {
			t.Run(string(state)+"/"+stage, func(t *testing.T) {
				f := newDomainFixture(t, 1)
				ctx := context.Background()
				request := f.startRequest("admission")
				var fence core.ExecutionFence
				if stage != "start" {
					if _, err := f.service.StartSession(ctx, request); err != nil {
						t.Fatal(err)
					}
				}
				if stage == "issue" || stage == "lease" {
					var err error
					fence, err = f.service.Claim(ctx, request.RequestID)
					if err != nil {
						t.Fatal(err)
					}
				}
				if stage == "lease" {
					if err := f.service.MarkIssued(ctx, fence); err != nil {
						t.Fatal(err)
					}
				}
				// Прямая запись моделирует встреченную несовместимую подготовку между стадиями.
				insertActivePreparationForAdmission(t, f, state)
				var err error
				switch stage {
				case "start":
					_, err = f.service.StartSession(ctx, request)
				case "claim":
					_, err = f.service.Claim(ctx, request.RequestID)
				case "issue":
					err = f.service.MarkIssued(ctx, fence)
				case "lease":
					_, err = f.service.BindExecutionOwner(ctx, fence)
				}
				requireDomainError(t, err, core.ErrNotReady)
			})
		}
	}
}

func TestStopProvisionalOutputTracksLiabilityOnceBeforePublisherIssuance(t *testing.T) {
	f := newDomainFixture(t, 2)
	ctx := context.Background()
	start := startDomainSession(t, f)
	parent, err := f.service.Claim(ctx, start.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.MarkIssued(ctx, parent); err != nil {
		t.Fatal(err)
	}
	outputs := readOutputs(t, f.store, start.SessionID)
	target, neighbor := outputs[0], outputs[1]
	stop, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "stop-provisional", OutputID: target.ID})
	if err != nil || stop.Status != core.CommandPending {
		t.Fatal("остановка не сохранила обязательство", err)
	}
	stopped, err := f.service.GetOutput(ctx, target.ID)
	if err != nil || !stopped.CleanupPending || stopped.Issued || stopped.Attempt != 0 || stopped.DesiredRunning || stopped.Generation <= target.Generation || !stopped.Attached {
		t.Fatalf("остановка потеряла обязательство предварительного выхода или выдумала сеть: %+v, %v", stopped, err)
	}
	cleanups := pendingCleanups(t, f.store, start.SessionID)
	if len(cleanups) != 1 || cleanups[0].OutputID != target.ID || cleanups[0].Attempt != 0 || cleanups[0].Generation != target.Generation {
		t.Fatalf("неверное обязательство предварительного выхода: %+v", cleanups)
	}
	if err := f.service.Complete(ctx, parent, core.Completion{Success: true}); err != nil {
		t.Fatal("остановка одного выхода прервала запуск соседей", err)
	}
	if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
		t.Fatal(err)
	}
	cleared, err := f.service.GetOutput(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "stop-cleared-attached", OutputID: target.ID})
	if err != nil || repeated.Status != core.CommandSucceeded {
		t.Fatal("повторная остановка создала новую работу", err)
	}
	after, err := f.service.GetOutput(ctx, target.ID)
	if err != nil || after.CleanupPending || after.Issued || after.Attempt != 0 || after.Generation != cleared.Generation || len(pendingCleanups(t, f.store, start.SessionID)) != 0 {
		t.Fatalf("Attached повторно создал обязательство: %+v, %v", after, err)
	}
	if current, err := f.service.GetOutput(ctx, neighbor.ID); err != nil || !reflect.DeepEqual(neighbor, current) {
		t.Fatalf("очистка изменила соседа: %+v, %v", current, err)
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, true)
}

func TestTerminalRuntimeCleanupDoesNotBlockNewExecutionOrReleaseItsHolds(t *testing.T) {
	for _, previous := range []bool{false, true} {
		for _, parentFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("previous=%t/parentFirst=%t", previous, parentFirst), func(t *testing.T) {
				f := newDomainFixture(t, 1)
				ctx := context.Background()
				old := startDomainSession(t, f)
				executeCommand(t, f.service, old.RequestID)
				oldOutput := readOutputs(t, f.store, old.SessionID)[0]
				executeCommand(t, f.service, outputStartReceipt(t, f, oldOutput.ID).RequestID)
				stop, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "stop-old-execution", SessionID: old.SessionID})
				if err != nil {
					t.Fatal(err)
				}
				executeCommand(t, f.service, stop.RequestID)
				if previous {
					f = reopenFixture(t, f)
				}
				cleanups := pendingCleanups(t, f.store, old.SessionID)
				if len(cleanups) != 2 {
					t.Fatalf("потеряны обязательства старого выполнения: %+v", cleanups)
				}
				if (cleanups[0].OutputID == "") != parentFirst {
					cleanups[0], cleanups[1] = cleanups[1], cleanups[0]
				}
				fresh, err := f.service.StartSession(ctx, f.startRequest("new-manual-execution"))
				if err != nil || fresh.SessionID == old.SessionID {
					t.Fatal("старые остатки запретили новый ручной start", err)
				}
				parent := executeCommand(t, f.service, fresh.RequestID)
				parentLease, err := f.service.BindExecutionOwner(ctx, parent)
				if err != nil {
					t.Fatal(err)
				}
				output := readOutputs(t, f.store, fresh.SessionID)[0]
				child := executeCommand(t, f.service, outputStartReceipt(t, f, output.ID).RequestID)
				observer := openOutputObserver(t, f.service, child)
				before := readSession(t, f.store, fresh.SessionID)
				beforeOutput, err := f.service.GetOutput(ctx, output.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, cleanup := range cleanups {
					if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanup)); err != nil {
						t.Fatal(err)
					}
					if !parentLease.Valid() || !observer.Valid() {
						t.Fatal("старый ack отозвал допуск нового выполнения")
					}
					requireOutputVariantHolds(t, f.store, f.variant.ID, true)
				}
				if after := readSession(t, f.store, fresh.SessionID); !reflect.DeepEqual(before, after) {
					t.Fatal("старый ack изменил новую Session")
				}
				if after, err := f.service.GetOutput(ctx, output.ID); err != nil || !reflect.DeepEqual(beforeOutput, after) {
					t.Fatal("старый ack изменил новый Output", err)
				}
				_, err = f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: f.asset.ID, ExpectedRevision: readAssetRevision(t, f, ctx), Kind: core.AssetImage, GeneratorVersion: "generator"})
				requireDomainError(t, err, core.ErrInUse)
			})
		}
	}
}
