package storage

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func (f domainFixture) startRequest(requestID string) core.StartSessionRequest {
	ids := make([]core.ID, 0, len(f.destinations))
	for _, d := range f.destinations {
		ids = append(ids, d.ID)
	}
	return core.StartSessionRequest{RequestID: requestID, StreamID: f.stream.ID, DestinationIDs: ids, Bindings: []core.PreparedBinding{{GenerationID: f.variant.GenerationID, GroupID: f.group.ID, VariantID: f.variant.ID, GeneratorVersion: f.variant.GeneratorVersion}}}
}

func startDomainSession(t *testing.T, f domainFixture) core.CommandReceipt {
	t.Helper()
	receipt, err := f.service.StartSession(context.Background(), f.startRequest("start-session"))
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func executeCommand(t *testing.T, svc *core.Service, requestID string) core.ExecutionFence {
	t.Helper()
	ctx := context.Background()
	fence, err := svc.Claim(ctx, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	if err := svc.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	return fence
}

func readSession(t *testing.T, s *Store, id core.ID) core.Session {
	t.Helper()
	var value core.Session
	if err := s.View(context.Background(), func(r core.Repositories) error {
		var err error
		value, err = r.Runtime.Session(context.Background(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return value
}

func readOutputs(t *testing.T, s *Store, sessionID core.ID) []core.SessionOutput {
	t.Helper()
	var values []core.SessionOutput
	if err := s.View(context.Background(), func(r core.Repositories) error {
		var err error
		values, err = r.Runtime.Outputs(context.Background(), core.ListQuery{Limit: core.MaxListLimit, SessionID: sessionID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return values
}

func outputStartReceipt(t *testing.T, f domainFixture, outputID core.ID) core.CommandReceipt {
	t.Helper()
	var result core.CommandReceipt
	if err := f.store.View(context.Background(), func(r core.Repositories) error {
		values, err := r.Runtime.Receipts(context.Background(), core.ListQuery{Limit: core.MaxListLimit, OutputID: outputID})
		if err != nil {
			return err
		}
		for _, c := range values {
			if c.Kind == core.CommandStartOutput && c.Status == core.CommandPending {
				result = c
				return nil
			}
		}
		return core.ErrNotFound
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func pendingCleanups(t *testing.T, s *Store, sessionID core.ID) []core.CleanupIntent {
	t.Helper()
	var values []core.CleanupIntent
	if err := s.View(context.Background(), func(r core.Repositories) error {
		var err error
		values, err = r.Artifacts.Cleanups(context.Background(), core.ListQuery{Limit: core.MaxListLimit, SessionID: sessionID, PendingOnly: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return values
}

func TestConcurrentStartsAcceptExactlyOneAtomicSession(t *testing.T) {
	f := newDomainFixture(t, 2)
	ctx := context.Background()
	const contenders = 6
	gate := make(chan struct{})
	results := make(chan error, contenders)
	var workers sync.WaitGroup
	for i := range contenders {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			<-gate
			_, err := f.service.StartSession(ctx, f.startRequest(fmt.Sprintf("start-%d", i)))
			results <- err
		}(i)
	}
	close(gate)
	workers.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
			continue
		}
		if !errors.Is(err, core.ErrInUse) && !errors.Is(err, core.ErrConflict) {
			t.Fatalf("неожиданный исход конкурентного start: %v", err)
		}
	}
	if accepted != 1 {
		t.Fatalf("принято запусков: %d, нужен один", accepted)
	}
	if err := f.store.View(ctx, func(r core.Repositories) error {
		sessions, err := r.Runtime.Sessions(ctx, core.ListQuery{Limit: core.MaxListLimit, StreamID: f.stream.ID})
		if err != nil {
			return err
		}
		if len(sessions) != 1 || sessions[0].State != core.SessionStarting {
			t.Fatalf("частично принятые или ложные RUNNING сессии: %+v", sessions)
		}
		outputs, err := r.Runtime.Outputs(ctx, core.ListQuery{Limit: core.MaxListLimit, SessionID: sessions[0].ID})
		if err != nil {
			return err
		}
		if len(outputs) != 2 {
			t.Fatalf("атомарный запуск сохранил %d выходов", len(outputs))
		}
		for _, o := range outputs {
			if o.Issued || o.Attempt != 0 || !o.DesiredRunning {
				t.Fatalf("принятие start выдало исполнение за результат: %+v", o)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptReplaySurvivesConfigAndDerivedBindingChanges(t *testing.T) {
	f := newDomainFixture(t, 2)
	ctx := context.Background()
	request := f.startRequest("durable-request")
	first, err := f.service.StartSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	p := f.profile
	p.Profile.VideoEncoding.GOPFrames++
	if _, err := f.service.UpdateProfile(ctx, p, p.Revision); err != nil {
		t.Fatal(err)
	}
	request.DestinationIDs[0], request.DestinationIDs[1] = request.DestinationIDs[1], request.DestinationIDs[0]
	request.Bindings = []core.PreparedBinding{{GroupID: "changed-derived-group", VariantID: "missing-derived-variant", GeneratorVersion: "changed-generator"}}
	replayed, err := f.service.StartSession(ctx, request)
	if err != nil || !reflect.DeepEqual(first, replayed) {
		t.Fatalf("повтор переоценил изменённую конфигурацию: %+v, %v", replayed, err)
	}
	request.DestinationIDs = request.DestinationIDs[:1]
	_, err = f.service.StartSession(ctx, request)
	requireDomainError(t, err, core.ErrConflict)
	_, err = f.service.StopSession(ctx, core.SessionCommand{RequestID: request.RequestID, SessionID: first.SessionID})
	requireDomainError(t, err, core.ErrConflict)
}

func TestCurrentConfigEditsLeaveLiveSnapshotsAndPinnedGeneration(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	receipt := startDomainSession(t, f)
	before := readSession(t, f.store, receipt.SessionID)
	output := readOutputs(t, f.store, receipt.SessionID)[0]
	p, err := f.service.CreateProfile(ctx, core.TranscodeProfile{Name: "Новый профиль", Profile: storageProfile()})
	if err != nil {
		t.Fatal(err)
	}
	g := f.group
	g.ProfileID = p.ID
	if _, err := f.service.UpdateGroup(ctx, g, g.Revision); err != nil {
		t.Fatalf("правка профиля текущей группы должна действовать со следующего запуска: %v", err)
	}
	newGroup, err := f.service.CreateGroup(ctx, core.TranscodeGroup{StreamID: f.stream.ID, ProfileID: p.ID, Name: "Другая группа"})
	if err != nil {
		t.Fatal(err)
	}
	d := f.destinations[0]
	d.GroupID, d.StreamKey, d.Enabled = newGroup.ID, "new-synthetic-key", false
	if _, err := f.service.UpdateDestination(ctx, d, d.Revision); err != nil {
		t.Fatalf("правка назначения должна сохранять live-снимок: %v", err)
	}
	_, err = f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: f.asset.ID, ExpectedRevision: f.asset.Revision, Kind: core.AssetImage, GeneratorVersion: f.variant.GeneratorVersion, Rebuild: true})
	requireDomainError(t, err, core.ErrInUse)
	after := readSession(t, f.store, receipt.SessionID)
	afterOutput := readOutputs(t, f.store, receipt.SessionID)[0]
	if !reflect.DeepEqual(before.Snapshot, after.Snapshot) || !reflect.DeepEqual(output.Snapshot, afterOutput.Snapshot) {
		t.Fatal("редактирование или rebuild переписали исторический снимок")
	}
	if afterOutput.Snapshot.Artifact.VariantID != f.variant.ID || afterOutput.Snapshot.Artifact.ManagedPath != f.variant.ManagedPath {
		t.Fatal("rebuild подменил удерживаемое поколение")
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, true)
	requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
}

func TestStopDuringStartupFencesLateCompletionAndTerminalStopIsNoop(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	old, err := f.service.Claim(ctx, start.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.PauseSession(ctx, core.SessionCommand{RequestID: "early-pause", SessionID: start.SessionID}); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("пауза до RUNNING: %v", err)
	}
	stop, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "startup-stop", SessionID: start.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	requireDomainError(t, f.service.MarkIssued(ctx, old), core.ErrStale)
	requireDomainError(t, f.service.Complete(ctx, old, core.Completion{Success: true}), core.ErrStale)
	s := readSession(t, f.store, start.SessionID)
	if s.State != core.SessionStopping || s.AllowsOBS(f.stream.PublicationKey) {
		t.Fatalf("принятый stop не отозвал запуск: %+v", s)
	}
	executeCommand(t, f.service, stop.RequestID)
	terminal := readSession(t, f.store, start.SessionID)
	if terminal.State != core.SessionStopped || terminal.EndedAt == nil {
		t.Fatalf("остановка не завершилась: %+v", terminal)
	}
	noop, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "terminal-stop", SessionID: start.SessionID})
	if err != nil || noop.Status != core.CommandSucceeded {
		t.Fatalf("stop завершённой сессии: %+v, %v", noop, err)
	}
	if after := readSession(t, f.store, start.SessionID); !reflect.DeepEqual(terminal, after) {
		t.Fatal("терминальный stop изменил сессию")
	}
}

func TestOutputCompletionSurvivesPauseAndNeighborStop(t *testing.T) {
	f := newDomainFixture(t, 2)
	ctx := context.Background()
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	outputs := readOutputs(t, f.store, start.SessionID)
	fences := make([]core.ExecutionFence, len(outputs))
	for i, o := range outputs {
		c := outputStartReceipt(t, f, o.ID)
		fence, err := f.service.Claim(ctx, c.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.MarkIssued(ctx, fence); err != nil {
			t.Fatal(err)
		}
		fences[i] = fence
	}
	if _, err := f.service.PauseSession(ctx, core.SessionCommand{RequestID: "pause-running", SessionID: start.SessionID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "stop-neighbor", OutputID: outputs[0].ID}); err != nil {
		t.Fatal(err)
	}
	requireDomainError(t, f.service.Complete(ctx, fences[0], core.Completion{Success: true}), core.ErrStale)
	if err := f.service.Complete(ctx, fences[1], core.Completion{Success: true}); err != nil {
		t.Fatalf("соседняя команда или пауза аннулировала здоровый callback: %v", err)
	}
	current := readOutputs(t, f.store, start.SessionID)
	if !current[0].CleanupPending || current[0].DesiredRunning || !current[0].Issued {
		t.Fatalf("принятие stop выдало неподтверждённую очистку за STOPPED: %+v", current[0])
	}
	if current[1].ID != outputs[1].ID || !current[1].Issued || !current[1].DesiredRunning || current[1].Generation != outputs[1].Generation {
		t.Fatalf("здоровый выход изменён: %+v", current[1])
	}
	session := readSession(t, f.store, start.SessionID)
	if session.State != core.SessionRunning || !session.DesiredPaused || !session.AllowsOBS(f.stream.PublicationKey) {
		t.Fatalf("пауза или отказ соседа нарушили Session: %+v", session)
	}
}

func TestRuntimeOutputFailureAndRetryLeaveHealthyNeighborRunning(t *testing.T) {
	f := newDomainFixture(t, 2)
	ctx := context.Background()
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	outputs := readOutputs(t, f.store, start.SessionID)
	fences := make([]core.ExecutionFence, len(outputs))
	for i, o := range outputs {
		c := outputStartReceipt(t, f, o.ID)
		fences[i] = executeCommand(t, f.service, c.RequestID)
	}
	observers := []*core.OutputObserver{openOutputObserver(t, f.service, fences[0]), openOutputObserver(t, f.service, fences[1])}
	if err := observers[0].Observe(ctx, core.OutputObservation{State: core.OutputRetrying, Error: "временная ошибка сети"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.service.RuntimeSnapshot(ctx, core.RuntimeSessionFence{SessionID: start.SessionID, Incarnation: fences[0].Incarnation, Lifecycle: fences[0].Lifecycle})
	if err != nil || len(snapshot.Outputs) != 2 || snapshot.Outputs[0].Value == nil || snapshot.Outputs[0].Value.State != core.OutputRetrying || snapshot.Outputs[1].Value != nil {
		t.Fatalf("retry изменил наблюдения соседа: %+v, %v", snapshot, err)
	}
	if err := observers[0].Observe(ctx, core.OutputObservation{State: core.OutputPublishing, Error: ""}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.FailOutput(ctx, observers[0], "окончательный отказ назначения"); err != nil {
		t.Fatal(err)
	}
	requireDomainError(t, observers[0].Observe(ctx, core.OutputObservation{State: core.OutputPublishing, Error: ""}), core.ErrStale)
	if err := observers[1].Observe(ctx, core.OutputObservation{State: core.OutputRetrying, Error: "временный отказ соседа"}); err != nil {
		t.Fatalf("отказ первого выхода аннулировал callback второго: %v", err)
	}
	if err := observers[1].Observe(ctx, core.OutputObservation{State: core.OutputPublishing, Error: ""}); err != nil {
		t.Fatal(err)
	}
	if session := readSession(t, f.store, start.SessionID); session.State != core.SessionRunning {
		t.Fatal("частичный отказ остановил Session")
	}
}

func TestOldCleanupAckCannotReleaseNewAttemptOnSameOutput(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	o := readOutputs(t, f.store, start.SessionID)[0]
	c := outputStartReceipt(t, f, o.ID)
	oldFence := executeCommand(t, f.service, c.RequestID)
	if _, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "first-stop", OutputID: o.ID}); err != nil {
		t.Fatal(err)
	}
	oldCleanup := pendingCleanups(t, f.store, start.SessionID)[0]
	if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(oldCleanup)); err != nil {
		t.Fatal(err)
	}
	restart, err := f.service.StartOutput(ctx, core.OutputCommand{RequestID: "new-output-attempt", OutputID: o.ID})
	if err != nil {
		t.Fatal(err)
	}
	newFence := executeCommand(t, f.service, restart.RequestID)
	if newFence.Attempt <= oldFence.Attempt || newFence.OutputGeneration <= oldFence.OutputGeneration {
		t.Fatal("повторный запуск переиспользовал старый fence")
	}
	if _, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "second-stop", OutputID: o.ID}); err != nil {
		t.Fatal(err)
	}
	newCleanup := pendingCleanups(t, f.store, start.SessionID)[0]
	if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(oldCleanup)); err != nil {
		t.Fatal(err)
	}
	remaining := pendingCleanups(t, f.store, start.SessionID)
	if len(remaining) != 1 || remaining[0].ID != newCleanup.ID {
		t.Fatal("старый ack освободил новое cleanup намерение")
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, true)
	requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
	if current := readOutputs(t, f.store, start.SessionID)[0]; !current.CleanupPending || !current.Issued {
		t.Fatal("старый ack снял владение новой попытки")
	}
}

func TestAddOutputUsesSessionPolicyAndPreservesNeighborAndReceipt(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	neighbor := readOutputs(t, f.store, start.SessionID)[0]
	neighborCommand := outputStartReceipt(t, f, neighbor.ID)
	neighborFence := executeCommand(t, f.service, neighborCommand.RequestID)
	neighborObserver := openOutputObserver(t, f.service, neighborFence)
	a, err := f.service.ReserveAsset(ctx, core.FallbackAsset{Name: "Следующий оригинал", Kind: core.AssetImage, OriginalPath: "originals/next.png"})
	if err != nil {
		t.Fatal(err)
	}
	stream := f.stream
	stream.Fallback.AssetID = a.ID
	if _, err := f.service.UpdateStream(ctx, stream, stream.Revision); err != nil {
		t.Fatal(err)
	}
	d, err := f.service.CreateDestination(ctx, core.Destination{StreamID: f.stream.ID, GroupID: f.group.ID, Name: "Новый выход", Platform: "custom", Endpoint: "rtmp://example.invalid/new", StreamKey: "new-synthetic-destination-key", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	request := core.AddOutputRequest{RequestID: "add-current-output", SessionID: start.SessionID, DestinationID: d.ID, Binding: core.PreparedBinding{GenerationID: f.variant.GenerationID, GroupID: f.group.ID, VariantID: f.variant.ID, GeneratorVersion: f.variant.GeneratorVersion}}
	accepted, err := f.service.AddOutput(ctx, request)
	if err != nil {
		t.Fatalf("AddOutput должен использовать сохранённую policy Session: %v", err)
	}
	d.StreamKey = "edited-after-acceptance"
	if _, err := f.service.UpdateDestination(ctx, d, d.Revision); err != nil {
		t.Fatal(err)
	}
	request.Binding = core.PreparedBinding{GroupID: "changed", VariantID: "missing", GeneratorVersion: "other"}
	replayed, err := f.service.AddOutput(ctx, request)
	if err != nil || !reflect.DeepEqual(replayed, accepted) {
		t.Fatalf("повтор AddOutput изменил принятый результат: %+v, %v", replayed, err)
	}
	request.DestinationID = neighbor.Snapshot.Destination.ID
	_, err = f.service.AddOutput(ctx, request)
	requireDomainError(t, err, core.ErrConflict)
	executeCommand(t, f.service, accepted.RequestID)
	added, err := f.service.GetOutput(ctx, accepted.OutputID)
	if err != nil {
		t.Fatal(err)
	}
	if added.Snapshot.Fallback.AssetID != f.asset.ID || added.Snapshot.Artifact.VariantID != f.variant.ID || added.Snapshot.Destination.StreamKey != "new-synthetic-destination-key" {
		t.Fatalf("новый выход потерял точный принятый снимок: %+v", added.Snapshot)
	}
	if err := neighborObserver.Observe(ctx, core.OutputObservation{State: core.OutputRetrying, Error: "временный отказ"}); err != nil {
		t.Fatalf("AddOutput аннулировал соседний fence: %v", err)
	}
	if err := neighborObserver.Observe(ctx, core.OutputObservation{State: core.OutputPublishing, Error: ""}); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentDeleteAndStartCannotLeavePartialHistory(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	gate := make(chan struct{})
	startResult := make(chan error, 1)
	deleteResult := make(chan error, 1)
	go func() {
		<-gate
		_, err := f.service.StartSession(ctx, f.startRequest("start-racing-delete"))
		startResult <- err
	}()
	go func() {
		<-gate
		deleteResult <- f.service.DeleteDestination(ctx, f.destinations[0].ID, f.destinations[0].Revision)
	}()
	close(gate)
	startErr, deleteErr := <-startResult, <-deleteResult
	expectedSessions := 0
	if startErr == nil {
		expectedSessions = 1
		requireDomainError(t, deleteErr, core.ErrInUse)
	} else {
		requireDomainError(t, startErr, core.ErrNotFound)
		if deleteErr != nil {
			t.Fatalf("ни start, ни delete не выполнились: %v, %v", startErr, deleteErr)
		}
	}
	if err := f.store.View(ctx, func(r core.Repositories) error {
		sessions, err := r.Runtime.Sessions(ctx, core.ListQuery{Limit: 10, StreamID: f.stream.ID})
		if err != nil {
			return err
		}
		if len(sessions) != expectedSessions {
			t.Fatalf("гонка оставила частичную историю: %+v", sessions)
		}
		_, err = r.Runtime.Receipt(ctx, "start-racing-delete")
		if expectedSessions == 0 {
			requireDomainError(t, err, core.ErrNotFound)
		} else if err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFailedUnissuedStartupReleasesHoldsAndCancelsIntents(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	fence, err := f.service.Claim(ctx, start.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.Complete(ctx, fence, core.Completion{Failure: "файл не прошёл проверку"}); err != nil {
		t.Fatal(err)
	}
	if session := readSession(t, f.store, start.SessionID); session.State != core.SessionFailed || session.EndedAt == nil || session.CleanupPending {
		t.Fatalf("невыданный отказ запуска: %+v", session)
	}
	if c := readReceipt(t, f.store, start.RequestID); c.Status != core.CommandFailed || c.Failure == "" {
		t.Fatalf("не сохранён отказ запуска: %+v", c)
	}
	if len(pendingCleanups(t, f.store, start.SessionID)) != 0 {
		t.Fatal("невыданный запуск требует внешнюю очистку")
	}
	if err := f.store.View(ctx, func(r core.Repositories) error {
		pending, err := r.Runtime.Receipts(ctx, core.ListQuery{Limit: 10, SessionID: start.SessionID, PendingOnly: true})
		if err != nil {
			return err
		}
		if len(pending) != 0 {
			t.Fatalf("после отказа остались исполнимые намерения: %+v", pending)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, false)
}

func openOutputObserver(t *testing.T, service *core.Service, fence core.ExecutionFence) *core.OutputObserver {
	t.Helper()
	observer, err := service.OpenOutputObserver(context.Background(), fence)
	if err != nil {
		t.Fatal(err)
	}
	return observer
}
