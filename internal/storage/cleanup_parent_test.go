package storage

import (
	"context"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestFailedStartupReleasesPinsOnlyAfterIssuedParentCleanup(t *testing.T) {
	for _, issued := range []bool{false, true} {
		name := "unissued"
		if issued {
			name = "issued"
		}
		t.Run(name, func(t *testing.T) {
			f := newDomainFixture(t, 1)
			ctx := context.Background()
			start := startDomainSession(t, f)
			before := readOutputs(t, f.store, start.SessionID)[0]
			fence, err := f.service.Claim(ctx, start.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if issued {
				if err := f.service.MarkIssued(ctx, fence); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.Complete(ctx, fence, core.Completion{Failure: "Не удалось подготовить группу"}); err != nil {
				t.Fatal(err)
			}
			session := readSession(t, f.store, start.SessionID)
			if session.State != core.SessionFailed || session.EndedAt == nil || session.CleanupPending != issued {
				t.Fatalf("неверное завершение запуска: %+v", session)
			}
			after := readOutputs(t, f.store, start.SessionID)[0]
			if after.Issued || after.Attempt != 0 || !reflect.DeepEqual(before.Snapshot, after.Snapshot) {
				t.Fatalf("отказ родителя изменил снимок или выдумал publisher: %+v", after)
			}
			cleanups := pendingCleanups(t, f.store, start.SessionID)
			if issued {
				if len(cleanups) != 1 || cleanups[0].OutputID != "" {
					t.Fatalf("нужна отдельная очистка родительской попытки: %+v", cleanups)
				}
				requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
				requireDomainError(t, f.service.DeleteDestination(ctx, f.destinations[0].ID, f.destinations[0].Revision), core.ErrInUse)
				wrong := core.CleanupFenceFor(cleanups[0])
				wrong.Lifecycle++
				requireDomainError(t, f.service.AcknowledgeCleanup(ctx, wrong), core.ErrStale)
				requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
				if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
					t.Fatal(err)
				}
			} else if len(cleanups) != 0 {
				t.Fatalf("невыданная попытка требует очистки: %+v", cleanups)
			}
			if err := f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision); err != nil {
				t.Fatalf("освобождённый запуск удерживает поколение: %v", err)
			}
			if err := f.service.DeleteDestination(ctx, f.destinations[0].ID, f.destinations[0].Revision); err != nil {
				t.Fatal(err)
			}
			if err := f.service.DeleteGroup(ctx, f.group.ID, f.group.Revision); err != nil {
				t.Fatal(err)
			}
			if err := f.service.DeleteProfile(ctx, f.profile.ID, f.profile.Revision); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParentAndPublisherCleanupRetainPinsInEitherAcknowledgementOrder(t *testing.T) {
	for _, parentFirst := range []bool{false, true} {
		name := "publisher_first"
		if parentFirst {
			name = "parent_first"
		}
		t.Run(name, func(t *testing.T) {
			f := newDomainFixture(t, 1)
			ctx := context.Background()
			start := startDomainSession(t, f)
			executeCommand(t, f.service, start.RequestID)
			output := readOutputs(t, f.store, start.SessionID)[0]
			executeCommand(t, f.service, outputStartReceipt(t, f, output.ID).RequestID)
			f = reopenFixture(t, f)
			cleanups := pendingCleanups(t, f.store, start.SessionID)
			if len(cleanups) != 2 {
				t.Fatalf("утрачено независимое владение родителя или publisher: %+v", cleanups)
			}
			if (cleanups[0].OutputID == "") != parentFirst {
				cleanups[0], cleanups[1] = cleanups[1], cleanups[0]
			}
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
				t.Fatal(err)
			}
			remaining := pendingCleanups(t, f.store, start.SessionID)
			if len(remaining) != 1 || remaining[0].ID != cleanups[1].ID {
				t.Fatalf("первый ack поглотил чужую очистку: %+v", remaining)
			}
			requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
				t.Fatal(err)
			}
			requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[1])); err != nil {
				t.Fatal(err)
			}
			if session := readSession(t, f.store, start.SessionID); session.CleanupPending || session.State != core.SessionInterrupted {
				t.Fatalf("оба ack не завершили очистку прерванного запуска: %+v", session)
			}
			if err := f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision); err != nil {
				t.Fatalf("после обоих ack остался pin: %v", err)
			}
		})
	}
}

func isolatedOutputDependency(t *testing.T, f domainFixture) (core.TranscodeProfile, core.TranscodeGroup, core.FallbackVariant, core.Destination) {
	t.Helper()
	ctx := context.Background()
	profile := storageProfile()
	profile.VideoEncoding.GOPFrames++
	p, err := f.service.CreateProfile(ctx, core.TranscodeProfile{Name: "Отдельный профиль", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	g, err := f.service.CreateGroup(ctx, core.TranscodeGroup{StreamID: f.stream.ID, ProfileID: p.ID, Name: "Отдельная группа"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := f.service.ReserveVariant(ctx, core.FallbackVariant{AssetID: f.asset.ID, SourceHash: f.asset.SourceHash, Profile: profile, GeneratorVersion: f.variant.GeneratorVersion, ManagedPath: "variants/isolated-group.nut"})
	if err != nil {
		t.Fatal(err)
	}
	v, err = f.service.PublishVariant(ctx, v.ID, v.Revision, v.AttemptToken, "isolated-content-hash", 4096)
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.service.CreateDestination(ctx, core.Destination{StreamID: f.stream.ID, GroupID: g.ID, Name: "Отдельный выход", Platform: "custom", Endpoint: "rtmps://example.invalid/isolated", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return p, g, v, d
}

func TestDetachUnissuedPublisherCleansOnlyItsGroupUnderLiveIssuedParent(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		name := "connecting"
		if stopped {
			name = "already_stopped"
		}
		t.Run(name, func(t *testing.T) {
			f := newDomainFixture(t, 1)
			ctx := context.Background()
			p, g, variant, destination := isolatedOutputDependency(t, f)
			request := f.startRequest("start-with-independent-groups")
			request.DestinationIDs = append(request.DestinationIDs, destination.ID)
			request.Bindings = append(request.Bindings, core.PreparedBinding{GroupID: g.ID, VariantID: variant.ID, GeneratorVersion: variant.GeneratorVersion})
			start, err := f.service.StartSession(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			executeCommand(t, f.service, start.RequestID)
			var target, neighbor core.SessionOutput
			for _, output := range readOutputs(t, f.store, start.SessionID) {
				if output.Snapshot.Destination.ID == destination.ID {
					target = output
				} else {
					neighbor = output
				}
			}
			neighborFence := executeCommand(t, f.service, outputStartReceipt(t, f, neighbor.ID).RequestID)
			neighborObserver := openOutputObserver(t, f.service, neighborFence)
			neighbor, err = f.service.GetOutput(ctx, neighbor.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stopped {
				if _, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "stop-before-detach", OutputID: target.ID}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.service.DetachOutput(ctx, core.OutputCommand{RequestID: "detach-without-publisher", OutputID: target.ID}); err != nil {
				t.Fatal(err)
			}
			detached, err := f.service.GetOutput(ctx, target.ID)
			if err != nil {
				t.Fatal(err)
			}
			if detached.Attached || detached.DesiredRunning || !detached.CleanupPending || detached.Issued || detached.Attempt != 0 {
				t.Fatalf("очистка группы выдумала publisher или потеряла владение: %+v", detached)
			}
			cleanups := pendingCleanups(t, f.store, start.SessionID)
			if len(cleanups) != 1 || cleanups[0].OutputID != target.ID {
				t.Fatalf("detach не создал точную очистку своей группы: %+v", cleanups)
			}
			wrong := core.CleanupFenceFor(cleanups[0])
			wrong.OutputID = neighbor.ID
			requireDomainError(t, f.service.AcknowledgeCleanup(ctx, wrong), core.ErrStale)
			requireDomainError(t, f.service.DeleteVariant(ctx, variant.ID, variant.Revision), core.ErrInUse)
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
				t.Fatal(err)
			}
			current, err := f.service.GetOutput(ctx, neighbor.ID)
			if err != nil || !reflect.DeepEqual(current, neighbor) {
				t.Fatalf("detach изменил здорового соседа: %+v, %v", current, err)
			}
			if session := readSession(t, f.store, start.SessionID); session.State != core.SessionRunning || !session.Issued || session.CleanupPending {
				t.Fatalf("очистка группы затронула живого родителя: %+v", session)
			}
			replacement, err := f.service.AddOutput(ctx, core.AddOutputRequest{RequestID: "attach-new-output", SessionID: start.SessionID, DestinationID: destination.ID, Binding: core.PreparedBinding{GroupID: g.ID, VariantID: variant.ID, GeneratorVersion: variant.GeneratorVersion}})
			if err != nil {
				t.Fatal(err)
			}
			executeCommand(t, f.service, replacement.RequestID)
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
				t.Fatal(err)
			}
			requireDomainError(t, f.service.DeleteVariant(ctx, variant.ID, variant.Revision), core.ErrInUse)
			if _, err := f.service.DetachOutput(ctx, core.OutputCommand{RequestID: "detach-replacement", OutputID: replacement.OutputID}); err != nil {
				t.Fatal(err)
			}
			pending := pendingCleanups(t, f.store, start.SessionID)
			if len(pending) != 1 || pending[0].OutputID != replacement.OutputID {
				t.Fatalf("потеряно владение новым выходом: %+v", pending)
			}
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
				t.Fatal(err)
			}
			requireDomainError(t, f.service.DeleteVariant(ctx, variant.ID, variant.Revision), core.ErrInUse)
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(pending[0])); err != nil {
				t.Fatal(err)
			}
			if err := f.service.DeleteVariant(ctx, variant.ID, variant.Revision); err != nil {
				t.Fatalf("подтверждённый detach удерживает собственный artifact: %v", err)
			}
			if err := f.service.DeleteDestination(ctx, destination.ID, destination.Revision); err != nil {
				t.Fatal(err)
			}
			if err := f.service.DeleteGroup(ctx, g.ID, g.Revision); err != nil {
				t.Fatal(err)
			}
			if err := f.service.DeleteProfile(ctx, p.ID, p.Revision); err != nil {
				t.Fatal(err)
			}
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
				t.Fatal(err)
			}
			requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
			if err := neighborObserver.Observe(ctx, core.OutputObservation{State: core.OutputRetrying, Error: "Временный отказ сети"}); err != nil {
				t.Fatalf("очистка группы отозвала допуск здорового соседа: %v", err)
			}
		})
	}
}
