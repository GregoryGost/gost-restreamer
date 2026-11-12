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

// Создаёт запрос запуска со всеми тестовыми назначениями и точной привязкой опубликованного варианта
// группы.
func (f domainFixture) startRequest(requestID string) core.StartSessionRequest {
	// ids собирает идентификаторы всех назначений первоначального запроса запуска.
	ids := make([]core.ID, 0, len(f.destinations))
	// d содержит настройки проверяемого назначения.
	for _, d := range f.destinations {
		ids = append(ids, d.ID)
	}
	return core.StartSessionRequest{RequestID: requestID, StreamID: f.stream.ID, DestinationIDs: ids, Bindings: []core.PreparedBinding{{GenerationID: f.variant.GenerationID, GroupID: f.group.ID, VariantID: f.variant.ID, GeneratorVersion: f.variant.GeneratorVersion}}}
}

// Принимает первоначальный тестовый запуск с RequestID start-session, не выдавая его за выполненный.
func startDomainSession(t *testing.T, f domainFixture) core.CommandReceipt {
	t.Helper()
	// receipt содержит сохраняемую квитанцию выбранной команды. err сообщает об ошибке принятия
	// ручного запуска потока.
	receipt, err := f.service.StartSession(context.Background(), f.startRequest("start-session"))
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

// Проводит принятую тестовую команду через Claim, MarkIssued и успешный Complete без создания внешних
// ресурсов.
func executeCommand(t *testing.T, svc *core.Service, requestID string) core.ExecutionFence {
	t.Helper()
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// fence идентифицирует точную захваченную попытку проверяемой команды. err сообщает об ошибке
	// захвата точной команды исполнения.
	fence, err := svc.Claim(ctx, requestID)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке выдачи точной попытки команды.
	if err := svc.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке сохранения результата точной команды.
	if err := svc.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	return fence
}

// Читает сохраняемую Session по ID в согласованной транзакции и завершает тест при ошибке.
func readSession(t *testing.T, s *Store, id core.ID) core.Session {
	t.Helper()
	var value core.Session
	// err сообщает об ошибке согласованного чтения сохраняемой сессии по ID.
	if err := s.View(context.Background(), func(r core.Repositories) error {
		// Читает Session по ID в захваченное value через согласованный View.

		// err передаёт результат чтения сохраняемой Session по ID.
		var err error
		value, err = r.Runtime.Session(context.Background(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return value
}

// Читает до MaxListLimit выходов заданной тестовой сессии в порядке хранения.
func readOutputs(t *testing.T, s *Store, sessionID core.ID) []core.SessionOutput {
	t.Helper()
	var values []core.SessionOutput
	// err сообщает об ошибке согласованного чтения сохраняемых выходов сессии.
	if err := s.View(context.Background(), func(r core.Repositories) error {
		// Читает сохранённые выходы выбранной Session в захваченный values с конечным MaxListLimit.

		// err передаёт результат чтения сохраняемых выходов выбранной Session.
		var err error
		values, err = r.Runtime.Outputs(context.Background(), core.ListQuery{Limit: core.MaxListLimit, SessionID: sessionID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return values
}

// Находит незахваченную внутреннюю команду первоначального запуска указанного выхода.
func outputStartReceipt(t *testing.T, f domainFixture, outputID core.ID) core.CommandReceipt {
	t.Helper()
	// result принимает PENDING-квитанцию первоначального запуска выбранного выхода.
	var result core.CommandReceipt
	// err сообщает об ошибке поиска PENDING-квитанции первоначального запуска выхода.
	if err := f.store.View(context.Background(), func(r core.Repositories) error {
		// Ищет PENDING-квитанцию первоначального CommandStartOutput выбранного выхода, иначе возвращает
		// ErrNotFound.

		// values принимает ограниченный сохраняемый список выбранного владельца. err сообщает об
		// ошибке чтения сохраняемых квитанций управления.
		values, err := r.Runtime.Receipts(context.Background(), core.ListQuery{Limit: core.MaxListLimit, OutputID: outputID})
		if err != nil {
			return err
		}
		// c содержит сохраняемую квитанцию выбранной команды.
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

// Читает до MaxListLimit неподтверждённых намерений очистки тестовой сессии.
func pendingCleanups(t *testing.T, s *Store, sessionID core.ID) []core.CleanupIntent {
	t.Helper()
	var values []core.CleanupIntent
	// err сообщает об ошибке чтения сохраняемых намерений очистки.
	if err := s.View(context.Background(), func(r core.Repositories) error {
		// Читает неподтверждённые намерения очистки выбранной Session в захваченный values.

		// err передаёт результат чтения неподтверждённых намерений очистки выбранной Session.
		var err error
		values, err = r.Artifacts.Cleanups(context.Background(), core.ListQuery{Limit: core.MaxListLimit, SessionID: sessionID, PendingOnly: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return values
}

// Проверяет единственного победителя конкурентных запусков и атомарное сохранение STARTING-сессии с
// выходами без ложной выдачи исполнения.
func TestConcurrentStartsAcceptExactlyOneAtomicSession(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 2)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// contenders задаёт число конкурентных запросов запуска одного потока.
	const contenders = 6
	// gate одновременно выпускает конкурирующие операции проверяемой гонки.
	gate := make(chan struct{})
	// results собирает результаты конкурентных запросов запуска.
	results := make(chan error, contenders)
	// workers ожидает завершения всех конкурирующих запросов.
	var workers sync.WaitGroup
	// i указывает конкурирующий запрос и используется для его уникального RequestID.
	for i := range contenders {
		workers.Add(1)
		go func(i int) {
			// После общего сигнала конкурирует за StartSession с собственным RequestID и сообщает результат.

			defer workers.Done()
			<-gate
			// err сообщает об ошибке принятия ручного запуска потока.
			_, err := f.service.StartSession(ctx, f.startRequest(fmt.Sprintf("start-%d", i)))
			results <- err
		}(i)
	}
	close(gate)
	workers.Wait()
	close(results)
	// accepted считает успешно принятые конкурентные запросы запуска.
	accepted := 0
	// err сообщает об ошибке очередного конкурентного запуска.
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
	// err сообщает об ошибке проверки атомарного сохранения единственной сессии и обоих выходов
	// после гонки запусков.
	if err := f.store.View(ctx, func(r core.Repositories) error {
		// Проверяет единственную Session в состоянии STARTING и два атомарно сохранённых выхода без выдуманных
		// Issued и Attempt.

		// sessions содержит страницу сохраняемых запусков проверяемого потока. err сообщает об
		// ошибке чтения сохраняемых запусков потока.
		sessions, err := r.Runtime.Sessions(ctx, core.ListQuery{Limit: core.MaxListLimit, StreamID: f.stream.ID})
		if err != nil {
			return err
		}
		if len(sessions) != 1 || sessions[0].State != core.SessionStarting {
			t.Fatalf("частично принятые или ложные RUNNING сессии: %+v", sessions)
		}
		// outputs содержит все сохраняемые выходы проверяемой сессии.
		outputs, err := r.Runtime.Outputs(ctx, core.ListQuery{Limit: core.MaxListLimit, SessionID: sessions[0].ID})
		if err != nil {
			return err
		}
		if len(outputs) != 2 {
			t.Fatalf("атомарный запуск сохранил %d выходов", len(outputs))
		}
		// o содержит сохраняемый выход с самостоятельным снимком и намерением.
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

// Проверяет повтор исходной квитанции после изменения конфигурации и производных привязок, а также
// конфликт другого содержания того же RequestID.
func TestReceiptReplaySurvivesConfigAndDerivedBindingChanges(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 2)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// request сохраняет исходный запрос запуска для проверки повторяемой квитанции.
	request := f.startRequest("durable-request")
	// first сохраняет первоначальную принятую квитанцию до изменения конфигурации и повтора. err
	// сообщает об ошибке принятия ручного запуска потока.
	first, err := f.service.StartSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// p содержит полный именованный профиль для проверяемого изменения.
	p := f.profile
	p.Profile.VideoEncoding.GOPFrames++
	// err сообщает об ошибке условного изменения полного профиля.
	if _, err := f.service.UpdateProfile(ctx, p, p.Revision); err != nil {
		t.Fatal(err)
	}
	request.DestinationIDs[0], request.DestinationIDs[1] = request.DestinationIDs[1], request.DestinationIDs[0]
	request.Bindings = []core.PreparedBinding{{GroupID: "changed-derived-group", VariantID: "missing-derived-variant", GeneratorVersion: "changed-generator"}}
	// replayed содержит квитанцию тождественного повторного запроса.
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

// Проверяет неизменность исторических снимков и удерживаемого поколения при редактировании
// конфигурации и отклонённой подготовке во время запуска.
func TestCurrentConfigEditsLeaveLiveSnapshotsAndPinnedGeneration(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// receipt содержит сохраняемую квитанцию выбранной команды.
	receipt := startDomainSession(t, f)
	// before сохраняет долговечную сессию до проверяемого управления.
	before := readSession(t, f.store, receipt.SessionID)
	// output содержит сохраняемый выход с самостоятельным снимком и намерением.
	output := readOutputs(t, f.store, receipt.SessionID)[0]
	// p содержит полный именованный профиль для проверяемого изменения. err сообщает об ошибке
	// сохранения полного именованного профиля.
	p, err := f.service.CreateProfile(ctx, core.TranscodeProfile{Name: "Новый профиль", Profile: storageProfile()})
	if err != nil {
		t.Fatal(err)
	}
	// g содержит конфигурацию группы тестового потока.
	g := f.group
	g.ProfileID = p.ID
	// err сообщает об ошибке условного изменения группы.
	if _, err := f.service.UpdateGroup(ctx, g, g.Revision); err != nil {
		t.Fatalf("правка профиля текущей группы должна действовать со следующего запуска: %v", err)
	}
	// newGroup содержит новую текущую группу, которая не должна менять сохранённый снимок запуска.
	newGroup, err := f.service.CreateGroup(ctx, core.TranscodeGroup{StreamID: f.stream.ID, ProfileID: p.ID, Name: "Другая группа"})
	if err != nil {
		t.Fatal(err)
	}
	// d содержит настройки проверяемого назначения.
	d := f.destinations[0]
	d.GroupID, d.StreamKey, d.Enabled = newGroup.ID, "new-synthetic-key", false
	// err сообщает об ошибке условного изменения назначения.
	if _, err := f.service.UpdateDestination(ctx, d, d.Revision); err != nil {
		t.Fatalf("правка назначения должна сохранять live-снимок: %v", err)
	}
	_, err = f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: f.asset.ID, ExpectedRevision: f.asset.Revision, Kind: core.AssetImage, GeneratorVersion: f.variant.GeneratorVersion, Rebuild: true})
	requireDomainError(t, err, core.ErrInUse)
	// after содержит долговечную сессию после проверяемого управления.
	after := readSession(t, f.store, receipt.SessionID)
	// afterOutput содержит повторно прочитанный выход после изменения текущей конфигурации.
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

// Проверяет отзыв поздних Issued/Complete после остановки STARTING и успешный Stop терминальной
// Session без её изменения.
func TestStopDuringStartupFencesLateCompletionAndTerminalStopIsNoop(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// old содержит точную прежнюю попытку запуска, отзываемую последующей остановкой. err сообщает
	// об ошибке захвата точной команды исполнения.
	old, err := f.service.Claim(ctx, start.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке принятия ручной паузы.
	if _, err := f.service.PauseSession(ctx, core.SessionCommand{RequestID: "early-pause", SessionID: start.SessionID}); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("пауза до RUNNING: %v", err)
	}
	// stop содержит принятую квитанцию остановки сессии.
	stop, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "startup-stop", SessionID: start.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	requireDomainError(t, f.service.MarkIssued(ctx, old), core.ErrStale)
	requireDomainError(t, f.service.Complete(ctx, old, core.Completion{Success: true}), core.ErrStale)
	// s содержит сессию после принятого stop для проверки отзыва допуска OBS.
	s := readSession(t, f.store, start.SessionID)
	if s.State != core.SessionStopping || s.AllowsOBS(f.stream.PublicationKey) {
		t.Fatalf("принятый stop не отозвал запуск: %+v", s)
	}
	executeCommand(t, f.service, stop.RequestID)
	// terminal содержит окончательно остановленную сессию до повторной команды stop.
	terminal := readSession(t, f.store, start.SessionID)
	if terminal.State != core.SessionStopped || terminal.EndedAt == nil {
		t.Fatalf("остановка не завершилась: %+v", terminal)
	}
	// noop содержит успешную квитанцию остановки уже завершённой сессии.
	noop, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "terminal-stop", SessionID: start.SessionID})
	if err != nil || noop.Status != core.CommandSucceeded {
		t.Fatalf("stop завершённой сессии: %+v, %v", noop, err)
	}
	// after содержит долговечную сессию после проверяемого управления.
	if after := readSession(t, f.store, start.SessionID); !reflect.DeepEqual(terminal, after) {
		t.Fatal("терминальный stop изменил сессию")
	}
}

// Проверяет сохранение допустимости завершения здорового выхода при паузе и остановке соседа и
// отдельный учёт его очистки.
func TestOutputCompletionSurvivesPauseAndNeighborStop(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 2)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// outputs содержит все сохраняемые выходы проверяемой сессии.
	outputs := readOutputs(t, f.store, start.SessionID)
	// fences сохраняет точные выданные попытки выходов для проверки независимости управления.
	fences := make([]core.ExecutionFence, len(outputs))
	// i указывает позицию выхода, а o содержит выход для выдачи его точной попытки.
	for i, o := range outputs {
		// c содержит сохраняемую квитанцию выбранной команды.
		c := outputStartReceipt(t, f, o.ID)
		// fence идентифицирует точную захваченную попытку проверяемой команды. err сообщает об
		// ошибке захвата точной команды исполнения.
		fence, err := f.service.Claim(ctx, c.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		// err сообщает об ошибке выдачи точной попытки команды.
		if err := f.service.MarkIssued(ctx, fence); err != nil {
			t.Fatal(err)
		}
		fences[i] = fence
	}
	// err сообщает об ошибке принятия ручной паузы.
	if _, err := f.service.PauseSession(ctx, core.SessionCommand{RequestID: "pause-running", SessionID: start.SessionID}); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке принятия остановки отдельного выхода.
	if _, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "stop-neighbor", OutputID: outputs[0].ID}); err != nil {
		t.Fatal(err)
	}
	requireDomainError(t, f.service.Complete(ctx, fences[0], core.Completion{Success: true}), core.ErrStale)
	// err сообщает об ошибке сохранения результата точной команды.
	if err := f.service.Complete(ctx, fences[1], core.Completion{Success: true}); err != nil {
		t.Fatalf("соседняя команда или пауза аннулировала здоровый callback: %v", err)
	}
	// current содержит повторно прочитанные выходы после независимого управления соседом.
	current := readOutputs(t, f.store, start.SessionID)
	if !current[0].CleanupPending || current[0].DesiredRunning || !current[0].Issued {
		t.Fatalf("принятие stop выдало неподтверждённую очистку за STOPPED: %+v", current[0])
	}
	if current[1].ID != outputs[1].ID || !current[1].Issued || !current[1].DesiredRunning || current[1].Generation != outputs[1].Generation {
		t.Fatalf("здоровый выход изменён: %+v", current[1])
	}
	// session содержит сохраняемый запуск для проверки состояния и владения.
	session := readSession(t, f.store, start.SessionID)
	if session.State != core.SessionRunning || !session.DesiredPaused || !session.AllowsOBS(f.stream.PublicationKey) {
		t.Fatalf("пауза или отказ соседа нарушили Session: %+v", session)
	}
}

// Проверяет независимые RAM-повторы соединения и изоляцию долговечного отказа выхода от соседа и
// Session.
func TestRuntimeOutputFailureAndRetryLeaveHealthyNeighborRunning(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 2)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// outputs содержит все сохраняемые выходы проверяемой сессии.
	outputs := readOutputs(t, f.store, start.SessionID)
	// fences сохраняет точные выданные попытки выходов для проверки независимости управления.
	fences := make([]core.ExecutionFence, len(outputs))
	// i указывает позицию выхода, а o содержит выход для запуска независимого наблюдателя.
	for i, o := range outputs {
		// c содержит сохраняемую квитанцию выбранной команды.
		c := outputStartReceipt(t, f, o.ID)
		fences[i] = executeCommand(t, f.service, c.RequestID)
	}
	// observers сохраняет независимых наблюдателей точных попыток двух выходов.
	observers := []*core.OutputObserver{openOutputObserver(t, f.service, fences[0]), openOutputObserver(t, f.service, fences[1])}
	// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
	if err := observers[0].Observe(ctx, core.OutputObservation{State: core.OutputRetrying, Error: "временная ошибка сети"}); err != nil {
		t.Fatal(err)
	}
	// snapshot содержит текущие наблюдения обоих выходов для проверки изоляции отказов. err сообщает
	// об ошибке чтения текущего согласованного снимка RAM.
	snapshot, err := f.service.RuntimeSnapshot(ctx, core.RuntimeSessionFence{SessionID: start.SessionID, Incarnation: fences[0].Incarnation, Lifecycle: fences[0].Lifecycle})
	if err != nil || len(snapshot.Outputs) != 2 || snapshot.Outputs[0].Value == nil || snapshot.Outputs[0].Value.State != core.OutputRetrying || snapshot.Outputs[1].Value != nil {
		t.Fatalf("retry изменил наблюдения соседа: %+v, %v", snapshot, err)
	}
	// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
	if err := observers[0].Observe(ctx, core.OutputObservation{State: core.OutputPublishing, Error: ""}); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке фиксации окончательного отказа точного выхода.
	if err := f.service.FailOutput(ctx, observers[0], "окончательный отказ назначения"); err != nil {
		t.Fatal(err)
	}
	requireDomainError(t, observers[0].Observe(ctx, core.OutputObservation{State: core.OutputPublishing, Error: ""}), core.ErrStale)
	// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
	if err := observers[1].Observe(ctx, core.OutputObservation{State: core.OutputRetrying, Error: "временный отказ соседа"}); err != nil {
		t.Fatalf("отказ первого выхода аннулировал callback второго: %v", err)
	}
	// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
	if err := observers[1].Observe(ctx, core.OutputObservation{State: core.OutputPublishing, Error: ""}); err != nil {
		t.Fatal(err)
	}
	// session содержит сохраняемый запуск для проверки состояния и владения.
	if session := readSession(t, f.store, start.SessionID); session.State != core.SessionRunning {
		t.Fatal("частичный отказ остановил Session")
	}
}

// Проверяет, что повтор подтверждения старой очистки не снимает Issued, удержания и намерение очистки
// следующей попытки того же выхода.
func TestOldCleanupAckCannotReleaseNewAttemptOnSameOutput(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// o содержит сохраняемый выход с самостоятельным снимком и намерением.
	o := readOutputs(t, f.store, start.SessionID)[0]
	// c содержит сохраняемую квитанцию выбранной команды.
	c := outputStartReceipt(t, f, o.ID)
	// oldFence идентифицирует прежнюю выданную попытку выхода до перезапуска.
	oldFence := executeCommand(t, f.service, c.RequestID)
	// err сообщает об ошибке принятия остановки отдельного выхода.
	if _, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "first-stop", OutputID: o.ID}); err != nil {
		t.Fatal(err)
	}
	// oldCleanup сохраняет намерение очистки прежней попытки выхода.
	oldCleanup := pendingCleanups(t, f.store, start.SessionID)[0]
	// err сообщает об ошибке подтверждения точного намерения очистки.
	if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(oldCleanup)); err != nil {
		t.Fatal(err)
	}
	// restart содержит принятую квитанцию нового запуска прикреплённого выхода. err сообщает об
	// ошибке принятия повторного запуска прикреплённого выхода.
	restart, err := f.service.StartOutput(ctx, core.OutputCommand{RequestID: "new-output-attempt", OutputID: o.ID})
	if err != nil {
		t.Fatal(err)
	}
	// newFence идентифицирует новую выданную попытку того же выхода.
	newFence := executeCommand(t, f.service, restart.RequestID)
	if newFence.Attempt <= oldFence.Attempt || newFence.OutputGeneration <= oldFence.OutputGeneration {
		t.Fatal("повторный запуск переиспользовал старый fence")
	}
	// err сообщает об ошибке принятия остановки отдельного выхода.
	if _, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "second-stop", OutputID: o.ID}); err != nil {
		t.Fatal(err)
	}
	// newCleanup содержит намерение очистки новой попытки выхода.
	newCleanup := pendingCleanups(t, f.store, start.SessionID)[0]
	// err сообщает об ошибке подтверждения точного намерения очистки.
	if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(oldCleanup)); err != nil {
		t.Fatal(err)
	}
	// remaining содержит оставшиеся неподтверждённые намерения после повторного старого
	// подтверждения.
	remaining := pendingCleanups(t, f.store, start.SessionID)
	if len(remaining) != 1 || remaining[0].ID != newCleanup.ID {
		t.Fatal("старый ack освободил новое cleanup намерение")
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, true)
	requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
	// current содержит новый выход после повтора старого подтверждения очистки.
	if current := readOutputs(t, f.store, start.SessionID)[0]; !current.CleanupPending || !current.Issued {
		t.Fatal("старый ack снял владение новой попытки")
	}
}

// Проверяет использование политики из снимка Session при AddOutput, неизменность принятой квитанции и
// изоляцию соседнего исполнения.
func TestAddOutputUsesSessionPolicyAndPreservesNeighborAndReceipt(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// neighbor сохраняет здоровый соседний выход до добавления другого.
	neighbor := readOutputs(t, f.store, start.SessionID)[0]
	// neighborCommand содержит квитанцию запуска здорового соседнего выхода.
	neighborCommand := outputStartReceipt(t, f, neighbor.ID)
	// neighborFence идентифицирует точную выданную попытку здорового соседа.
	neighborFence := executeCommand(t, f.service, neighborCommand.RequestID)
	// neighborObserver содержит наблюдателя здорового соседа для проверки сохранности его допуска.
	neighborObserver := openOutputObserver(t, f.service, neighborFence)
	// a содержит стабильную заглушку проверяемого поколения. err сообщает об ошибке создания
	// стабильной заглушки.
	a, err := f.service.ReserveAsset(ctx, core.FallbackAsset{Name: "Следующий оригинал", Kind: core.AssetImage, OriginalPath: "originals/next.png"})
	if err != nil {
		t.Fatal(err)
	}
	// stream содержит конфигурацию потока с постоянным ключом и выбранной заглушкой.
	stream := f.stream
	stream.Fallback.AssetID = a.ID
	// err сообщает об ошибке условного изменения конфигурации потока.
	if _, err := f.service.UpdateStream(ctx, stream, stream.Revision); err != nil {
		t.Fatal(err)
	}
	// d содержит настройки проверяемого назначения.
	d, err := f.service.CreateDestination(ctx, core.Destination{StreamID: f.stream.ID, GroupID: f.group.ID, Name: "Новый выход", Platform: "custom", Endpoint: "rtmp://example.invalid/new", StreamKey: "new-synthetic-destination-key", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// request задаёт независимое добавление текущего назначения в работающую сессию.
	request := core.AddOutputRequest{RequestID: "add-current-output", SessionID: start.SessionID, DestinationID: d.ID, Binding: core.PreparedBinding{GenerationID: f.variant.GenerationID, GroupID: f.group.ID, VariantID: f.variant.ID, GeneratorVersion: f.variant.GeneratorVersion}}
	// accepted содержит принятую квитанцию добавления независимого выхода.
	accepted, err := f.service.AddOutput(ctx, request)
	if err != nil {
		t.Fatalf("AddOutput должен использовать сохранённую policy Session: %v", err)
	}
	d.StreamKey = "edited-after-acceptance"
	// err сообщает об ошибке условного изменения назначения.
	if _, err := f.service.UpdateDestination(ctx, d, d.Revision); err != nil {
		t.Fatal(err)
	}
	request.Binding = core.PreparedBinding{GroupID: "changed", VariantID: "missing", GeneratorVersion: "other"}
	// replayed содержит квитанцию тождественного повторного запроса.
	replayed, err := f.service.AddOutput(ctx, request)
	if err != nil || !reflect.DeepEqual(replayed, accepted) {
		t.Fatalf("повтор AddOutput изменил принятый результат: %+v, %v", replayed, err)
	}
	request.DestinationID = neighbor.Snapshot.Destination.ID
	_, err = f.service.AddOutput(ctx, request)
	requireDomainError(t, err, core.ErrConflict)
	executeCommand(t, f.service, accepted.RequestID)
	// added содержит сохранённый новый выход с принятой политикой и снимком.
	added, err := f.service.GetOutput(ctx, accepted.OutputID)
	if err != nil {
		t.Fatal(err)
	}
	if added.Snapshot.Fallback.AssetID != f.asset.ID || added.Snapshot.Artifact.VariantID != f.variant.ID || added.Snapshot.Destination.StreamKey != "new-synthetic-destination-key" {
		t.Fatalf("новый выход потерял точный принятый снимок: %+v", added.Snapshot)
	}
	// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
	if err := neighborObserver.Observe(ctx, core.OutputObservation{State: core.OutputRetrying, Error: "временный отказ"}); err != nil {
		t.Fatalf("AddOutput аннулировал соседний fence: %v", err)
	}
	// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
	if err := neighborObserver.Observe(ctx, core.OutputObservation{State: core.OutputPublishing, Error: ""}); err != nil {
		t.Fatal(err)
	}
}

// Проверяет допустимые исходы гонки удаления назначения и запуска без частичной истории Session или
// квитанции.
func TestConcurrentDeleteAndStartCannotLeavePartialHistory(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// gate одновременно выпускает конкурирующие операции проверяемой гонки.
	gate := make(chan struct{})
	// startResult передаёт итог конкурентного запуска сессии.
	startResult := make(chan error, 1)
	// deleteResult передаёт итог конкурентного удаления назначения.
	deleteResult := make(chan error, 1)
	go func() {
		// После общего сигнала запускает Session, конкурируя с удалением назначения.

		<-gate
		// err сообщает об ошибке принятия ручного запуска потока.
		_, err := f.service.StartSession(ctx, f.startRequest("start-racing-delete"))
		startResult <- err
	}()
	go func() {
		// После общего сигнала удаляет назначение, конкурируя с запуском Session.

		<-gate
		deleteResult <- f.service.DeleteDestination(ctx, f.destinations[0].ID, f.destinations[0].Revision)
	}()
	close(gate)
	// startErr содержит результат запуска в гонке с удалением назначения. deleteErr содержит
	// результат удаления назначения в гонке с запуском.
	startErr, deleteErr := <-startResult, <-deleteResult
	// expectedSessions задаёт допустимое число сохранённых сессий по результату гонки.
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
	// err сообщает об ошибке проверки целостности истории и квитанции после гонки запуска с
	// удалением назначения.
	if err := f.store.View(ctx, func(r core.Repositories) error {
		// Проверяет целостность истории и квитанции после гонки запуска с удалением назначения.

		// sessions содержит страницу сохраняемых запусков проверяемого потока. err сообщает об
		// ошибке чтения сохраняемых запусков потока.
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

// Проверяет отказ невыданного запуска без внешней очистки с освобождением удержаний и прерыванием
// дочерних намерений.
func TestFailedUnissuedStartupReleasesHoldsAndCancelsIntents(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// fence идентифицирует точную захваченную попытку проверяемой команды. err сообщает об ошибке
	// захвата точной команды исполнения.
	fence, err := f.service.Claim(ctx, start.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке сохранения результата точной команды.
	if err := f.service.Complete(ctx, fence, core.Completion{Failure: "файл не прошёл проверку"}); err != nil {
		t.Fatal(err)
	}
	// session содержит сохраняемый запуск для проверки состояния и владения.
	if session := readSession(t, f.store, start.SessionID); session.State != core.SessionFailed || session.EndedAt == nil || session.CleanupPending {
		t.Fatalf("невыданный отказ запуска: %+v", session)
	}
	// c содержит сохраняемую квитанцию выбранной команды.
	if c := readReceipt(t, f.store, start.RequestID); c.Status != core.CommandFailed || c.Failure == "" {
		t.Fatalf("не сохранён отказ запуска: %+v", c)
	}
	if len(pendingCleanups(t, f.store, start.SessionID)) != 0 {
		t.Fatal("невыданный запуск требует внешнюю очистку")
	}
	// err сообщает об ошибке проверки отсутствия исполнимых квитанций после отказа невыданного
	// запуска.
	if err := f.store.View(ctx, func(r core.Repositories) error {
		// Проверяет отсутствие исполнимых квитанций после отказа невыданного запуска.

		// pending содержит незавершённые квитанции после отказа первоначального запуска. err
		// сообщает об ошибке чтения сохраняемых квитанций управления.
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

// Открывает наблюдателя точной успешно выполненной попытки выхода и завершает тест при ошибке.
func openOutputObserver(t *testing.T, service *core.Service, fence core.ExecutionFence) *core.OutputObserver {
	t.Helper()
	// observer содержит наблюдателя точной успешно завершённой попытки выхода. err сообщает об
	// ошибке регистрации наблюдателя точной попытки выхода.
	observer, err := service.OpenOutputObserver(context.Background(), fence)
	if err != nil {
		t.Fatal(err)
	}
	return observer
}
