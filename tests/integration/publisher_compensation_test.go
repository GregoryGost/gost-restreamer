package integration

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

type publisherCompensationFixture struct {
	service  *core.Service
	parent   core.ExecutionFence
	observer *core.SessionObserver
	outputs  []core.SessionOutput
	router   *media.Router
}

func newPublisherCompensationFixture(t *testing.T) publisherCompensationFixture {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "compensation.sqlite"), storage.Options{
		OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 2, MaxIdentifierBytes: 1024, MaxBytes: 1 << 20},
		PreparationLimits:     core.PreparationLimits{MaxQueued: 2, MaxProfiles: 2},
		ObservationLimits:     core.ObservationLimits{MaxSessions: 1, MaxGroups: 1, MaxOutputs: 1, MaxFFmpegWorkers: 1, MaxIdentifierBytes: 1024, MaxErrorBytes: 4096, MaxBytes: 1 << 20},
		MaxOpenConns:          4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute,
		BusyTimeout: 20 * time.Millisecond, RetryAttempts: 4, RetryTimeout: time.Second, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	diagnostics, err := store.Diagnostics(ctx)
	if err != nil || diagnostics.SchemaVersion != 1 {
		t.Fatalf("схема Goose: %+v, %v", diagnostics, err)
	}
	t.Logf("SQLite=%s Goose=%d", diagnostics.SQLiteVersion, diagnostics.SchemaVersion)
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	asset, err := svc.CreateAsset(ctx, "Заглушка")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := svc.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "synthetic-ingest", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Millisecond, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: publisherProfile(core.CodecAV1, 2560, 1440)})
	if err != nil {
		t.Fatal(err)
	}
	group, err := svc.CreateGroup(ctx, core.TranscodeGroup{Name: "Группа", StreamID: stream.ID, ProfileID: profile.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"target-key", "neighbor-key"} {
		if _, err = svc.CreateDestination(ctx, core.Destination{Name: "Назначение", StreamID: stream.ID, GroupID: group.ID, Platform: "custom", Endpoint: "rtmp://127.0.0.1:1/live", StreamKey: key, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	gen, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "controlled-generator"})
	if err != nil {
		t.Fatal(err)
	}
	pf, err := svc.ClaimPreparation(ctx, gen.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkPreparationIssued(ctx, pf); err != nil {
		t.Fatal(err)
	}
	if len(gen.RequiredProfiles) != 1 {
		t.Fatalf("набор профилей: %+v", gen.RequiredProfiles)
	}
	r := gen.RequiredProfiles[0]
	publication := core.GenerationPublication{SourceHash: "controlled-source", SizeBytes: 1, SourceFormat: core.SourceFormat{ImageFormat: "png"}, Variants: []core.FallbackVariant{{Record: core.Record{ID: r.VariantID}, GenerationID: gen.ID, AssetID: asset.ID, SourceHash: "controlled-source", Profile: r.Profile, VerifiedFormat: r.Profile.Format, GeneratorVersion: gen.GeneratorVersion, ManagedPath: r.ManagedPath, SizeBytes: 1, ContentHash: "controlled-result"}}}
	if _, err = svc.PublishPreparation(ctx, pf, publication); err != nil {
		t.Fatal(err)
	}
	if err = svc.AcknowledgePreparationCleanup(ctx, pf); err != nil {
		t.Fatal(err)
	}
	receipt, err := svc.StartSession(ctx, core.StartSessionRequest{RequestID: "start-compensation", StreamID: stream.ID, Bindings: []core.PreparedBinding{{GroupID: group.ID, GenerationID: gen.ID, VariantID: r.VariantID, GeneratorVersion: gen.GeneratorVersion}}})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := svc.Claim(ctx, receipt.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkIssued(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err = svc.Complete(ctx, parent, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	observer, err := svc.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := observer.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	outputs, err := svc.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: parent.SessionID}})
	if err != nil || len(outputs.Items) != 2 {
		t.Fatalf("выходы: %+v, %v", outputs, err)
	}
	router, err := media.NewRouter(publisherRouterLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := router.Close(); err != nil {
			t.Error(err)
		}
	})
	return publisherCompensationFixture{svc, parent, observer, outputs.Items, router}
}

func publisherCompensationIssue(t *testing.T, f publisherCompensationFixture, output core.SessionOutput) (core.ExecutionFence, *core.RuntimeLease) {
	t.Helper()
	ctx := context.Background()
	receipts, err := f.service.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 16, SessionID: f.parent.SessionID, OutputID: output.ID}})
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range receipts.Items {
		if receipt.Kind != core.CommandStartOutput || receipt.Status != core.CommandPending {
			continue
		}
		fence, err := f.service.Claim(ctx, receipt.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		if err = f.service.MarkIssued(ctx, fence); err != nil {
			t.Fatal(err)
		}
		owner, err := f.service.BindExecutionOwner(ctx, fence)
		if err != nil {
			t.Fatal(err)
		}
		return fence, owner
	}
	t.Fatal("нет собственной квитанции запуска выхода")
	return core.ExecutionFence{}, nil
}

func TestPublisherStartupCompensationAfterSuccessfulReceipt(t *testing.T) {
	for _, phase := range []string{"observer-budget", "replaced-observer"} {
		t.Run(phase, func(t *testing.T) {
			f := newPublisherCompensationFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			target, neighbor := f.outputs[0], f.outputs[1]
			if phase == "observer-budget" {
				fence, _ := publisherCompensationIssue(t, f, neighbor)
				if err := f.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
					t.Fatal(err)
				}
				observer, err := f.service.OpenOutputObserver(ctx, fence)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := observer.Close(context.Background()); err != nil {
						t.Error(err)
					}
				})
			}
			neighborBefore, err := f.service.GetOutput(ctx, neighbor.ID)
			if err != nil {
				t.Fatal(err)
			}
			fence, owner := publisherCompensationIssue(t, f, target)
			var dials atomic.Uint64
			options := publisher.Options{Queue: publisherQueue(), RetryInitial: time.Millisecond, RetryMax: 8 * time.Millisecond, ResubscribeDelay: time.Millisecond, Transport: publisherTransportOptions(nil)}
			options.Transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
				dials.Add(1)
				return nil, errors.New("сеть до активации запрещена")
			}
			prepare := func(lease *core.RuntimeLease) (*publisher.Publisher, error) {
				return publisher.Prepare(ctx, f.service, lease, target.Snapshot, f.router, options)
			}
			p, err := prepare(owner)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), time.Second)
				defer stop()
				if err := p.Abort(cleanup); err != nil {
					t.Error(err)
				}
			})
			if err = f.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
				t.Fatal(err)
			}
			startReceipt, err := f.service.GetReceipt(ctx, fence.CommandID)
			if err != nil || startReceipt.Status != core.CommandSucceeded {
				t.Fatalf("результат подготовки: %+v, %v", startReceipt, err)
			}
			if phase == "observer-budget" {
				if observer, err := f.service.OpenOutputObserver(ctx, fence); !errors.Is(err, core.ErrObservationLimit) || observer != nil {
					t.Fatalf("бюджет наблюдателя: %v, %v", observer, err)
				}
			} else {
				old, err := f.service.OpenOutputObserver(ctx, fence)
				if err != nil {
					t.Fatal(err)
				}
				current, err := f.service.OpenOutputObserver(ctx, fence)
				if err != nil || old.Valid() || !current.Valid() {
					t.Fatalf("замена наблюдателя: %v", err)
				}
				if err = p.Activate(ctx, old); !errors.Is(err, core.ErrStale) {
					t.Fatalf("устаревший токен принят: %v", err)
				}
			}
			if duplicate, err := prepare(owner); !errors.Is(err, core.ErrInUse) || duplicate != nil {
				t.Fatalf("повторная подготовка: %v, %v", duplicate, err)
			}
			if err = f.service.ConfirmOutputExecutionResolution(ctx, p.Reservation()); !errors.Is(err, core.ErrNotReady) {
				t.Fatalf("пригодный ExecutionFence со статусом SUCCEEDED освобождён: %v", err)
			}
			publisherCompensationNoNetwork(t, p, &dials)
			runtime, err := f.service.RuntimeSnapshot(ctx, f.observer.SessionFence())
			if err != nil {
				t.Fatal(err)
			}
			for _, observed := range runtime.Outputs {
				if observed.Execution.OutputID == target.ID && observed.Value != nil {
					t.Fatalf("неактивный выход получил наблюдаемое состояние: %+v", observed)
				}
			}
			stopReceipt, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "compensate-output", OutputID: target.ID})
			if err != nil || stopReceipt.Status != core.CommandPending {
				t.Fatalf("компенсация: %+v, %v", stopReceipt, err)
			}
			if err = p.Abort(ctx); err != nil {
				t.Fatal(err)
			}
			result, err := p.Wait(ctx)
			if err != nil || !result.NetworkJoined {
				t.Fatalf("не завершён Publisher: %+v, %v", result, err)
			}
			if err = p.ReleaseJoined(); !errors.Is(err, core.ErrNotReady) {
				t.Fatalf("завершение без подтверждения управления освободило резерв: %v", err)
			}
			publisherCompensationNoNetwork(t, p, &dials)
			output, err := f.service.GetOutput(ctx, target.ID)
			if err != nil || output.DesiredRunning || !output.Attached || !output.Issued || !output.CleanupPending || output.Generation != fence.OutputGeneration+1 || output.Attempt != fence.Attempt {
				t.Fatalf("состояние компенсации: %+v, %v", output, err)
			}
			cleanups, err := f.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: f.parent.SessionID, PendingOnly: true}})
			if err != nil || len(cleanups.Items) != 1 {
				t.Fatalf("точная дочерняя очистка: %+v, %v", cleanups, err)
			}
			intent := cleanups.Items[0]
			if intent.Kind != core.CleanupRuntime || intent.OutputID != target.ID || intent.Incarnation != fence.Incarnation || intent.Lifecycle != fence.Lifecycle || intent.Generation != fence.OutputGeneration || intent.Attempt != fence.Attempt {
				t.Fatalf("неверный владелец очистки: %+v", intent)
			}
			wrong := core.CleanupFenceFor(intent)
			wrong.Attempt++
			if err = f.service.AcknowledgeCleanup(ctx, wrong); !errors.Is(err, core.ErrStale) {
				t.Fatalf("принято чужое подтверждение: %v", err)
			}
			unchanged, err := f.service.GetCleanup(ctx, intent.ID)
			if err != nil || !reflect.DeepEqual(unchanged, intent) {
				t.Fatalf("отклонённое подтверждение изменило очистку: %+v, %v", unchanged, err)
			}
			// Группа и её ссылка не создавались; физически завершён единственный ресурс этого выхода.
			if err = f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(intent)); err != nil {
				t.Fatal(err)
			}
			output, err = f.service.GetOutput(ctx, target.ID)
			if err != nil || output.CleanupPending || output.Issued || output.DesiredRunning {
				t.Fatalf("очистка выхода не завершена: %+v, %v", output, err)
			}
			cleanups, err = f.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: f.parent.SessionID, PendingOnly: true}})
			if err != nil || len(cleanups.Items) != 0 {
				t.Fatalf("осталась дочерняя очистка: %+v, %v", cleanups, err)
			}
			session, err := f.service.GetSession(ctx, f.parent.SessionID)
			if err != nil || session.State != core.SessionRunning || !session.Issued || !f.observer.Valid() {
				t.Fatalf("завершение дочернего ресурса изменило родителя: %+v, %v", session, err)
			}
			neighborAfter, err := f.service.GetOutput(ctx, neighbor.ID)
			if err != nil || !reflect.DeepEqual(neighborBefore, neighborAfter) {
				t.Fatalf("компенсация изменила соседний выход: %+v, %v", neighborAfter, err)
			}
			receipt, err := f.service.GetReceipt(ctx, stopReceipt.ID)
			if err != nil || receipt.Status != core.CommandSucceeded {
				t.Fatalf("stop не завершён очисткой: %+v, %v", receipt, err)
			}
			receipt, err = f.service.GetReceipt(ctx, fence.CommandID)
			if err != nil || !reflect.DeepEqual(receipt, startReceipt) {
				t.Fatalf("успешная квитанция переписана: %+v, %v", receipt, err)
			}
			if _, err = f.service.StartOutput(ctx, core.OutputCommand{RequestID: "restart-compensated-output", OutputID: target.ID}); err != nil {
				t.Fatal(err)
			}
			nextFence, nextOwner := publisherCompensationIssue(t, f, target)
			if duplicate, err := prepare(nextOwner); !errors.Is(err, core.ErrInUse) || duplicate != nil {
				t.Fatalf("очистка обошла удержание резерва исполнения: %v, %v", duplicate, err)
			}
			if err = f.service.ConfirmOutputExecutionResolution(ctx, p.Reservation()); err != nil {
				t.Fatal(err)
			}
			if err = p.ReleaseJoined(); err != nil {
				t.Fatal(err)
			}
			if err = p.ReleaseJoined(); err != nil {
				t.Fatalf("повторное освобождение: %v", err)
			}
			next, err := prepare(nextOwner)
			if err != nil {
				t.Fatalf("подтверждённое завершение не освободило слот: %v", err)
			}
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), time.Second)
				defer stop()
				if err := next.Abort(cleanup); err != nil {
					t.Error(err)
				}
			})
			if err = f.service.Complete(ctx, nextFence, core.Completion{Success: false, Failure: "Локальная подготовка отменена"}); err != nil {
				t.Fatal(err)
			}
			if err = next.Abort(ctx); err != nil {
				t.Fatal(err)
			}
			if err = f.service.ConfirmOutputExecutionResolution(ctx, next.Reservation()); err != nil {
				t.Fatal(err)
			}
			if err = next.ReleaseJoined(); err != nil {
				t.Fatal(err)
			}
			publisherCompensationNoNetwork(t, next, &dials)
			publisherCompensationNoNetwork(t, p, &dials)
		})
	}
}

func publisherCompensationNoNetwork(t *testing.T, p *publisher.Publisher, dials *atomic.Uint64) {
	t.Helper()
	s := p.Snapshot()
	if dials.Load() != 0 || s.State == core.OutputPublishing || s.Attempts != 0 || s.Connections != 0 || s.WrittenBytes != 0 || s.AccessUnits != 0 || s.QueuedPackets != 0 || s.PendingPackets != 0 {
		t.Fatalf("неактивный Publisher начал передачу: dials=%d, %+v", dials.Load(), s)
	}
}
