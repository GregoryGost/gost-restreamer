package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// compensationWaitingStore задерживает запись перед настоящим Store для воспроизводимой гонки
// компенсации.
type compensationWaitingStore struct {
	// Store предоставляет реальные транзакции и общую RuntimeAuthority изолированного хранилища.
	*Store
	// entered сигнализирует достижение границы перед записью компенсации.
	entered chan struct{}
	// resume разрешает задержанной компенсации перейти к настоящей транзакции.
	resume chan struct{}
}

// Сигнализирует достижение границы записи и ожидает разрешения либо отмены перед передачей операции
// настоящему Store.
func (s *compensationWaitingStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	close(s.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.resume:
		return s.Store.Write(ctx, callback)
	}
}

// Проверяет, что задержанная до записи компенсация уступает принятой пользовательской остановке без
// новой квитанции и изменения результата победителя.
func TestOutputStartCompensationRacingUserStopPreservesItsReceipt(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite. cancel отменяет или
	// освобождает ограниченный контекст проверяемого управления.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// output содержит сохраняемый выход с самостоятельным снимком и намерением.
	output := readOutputs(t, f.store, start.SessionID)[0]
	// fence идентифицирует точную захваченную попытку проверяемой команды.
	fence := executeCommand(t, f.service, outputStartReceipt(t, f, output.ID).RequestID)
	// waiting удерживает компенсацию перед настоящей транзакцией для управляемой гонки.
	waiting := &compensationWaitingStore{Store: f.store, entered: make(chan struct{}), resume: make(chan struct{})}
	// service предоставляет доменные операции текущего владельца временного Store. err сообщает об
	// ошибке создания доменного Service для текущего владельца Store.
	service, err := core.NewService(waiting, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// outcome передаёт квитанцию, факт применения и отказ конкурентной компенсации.
	type outcome struct {
		// receipt содержит квитанцию конкурентной компенсации.
		receipt core.CommandReceipt
		// applied показывает, применена ли конкурентная компенсация.
		applied bool
		// err содержит отказ конкурентной компенсации, передаваемый основной проверке.
		err error
	}
	// done передаёт итог конкурентной компенсации после разрешения её записи.
	done := make(chan outcome, 1)
	// joined закрывается после физического завершения горутины компенсации.
	joined := make(chan struct{})
	go func() {
		// В отдельной горутине запускает компенсацию выхода и сообщает квитанцию, факт принятия и ошибку после
		// управляемой границы Write.

		defer close(joined)
		// receipt содержит сохраняемую квитанцию выбранной команды. applied показывает, принята
		// ли компенсация или повтор её прежней квитанции. err сообщает об ошибке условной
		// компенсации точной попытки запуска выхода.
		receipt, applied, err := service.CompensateOutputStart(ctx, fence, "concurrent-compensation")
		done <- outcome{receipt, applied, err}
	}()
	defer func() {
		// При любом выходе отменяет ожидание компенсации и дожидается завершения её горутины.

		cancel()
		<-joined
	}()
	select {
	case <-waiting.entered:
	case <-ctx.Done():
		t.Fatal("компенсация не дошла до границы записи")
	}
	// stop содержит принятую квитанцию остановки точного исполнения.
	stop, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "winning-user-stop", OutputID: output.ID})
	if err != nil {
		t.Fatal(err)
	}
	close(waiting.resume)
	select {
	// result содержит полученный итог конкурентной компенсации после победы пользовательского stop.
	case result := <-done:
		if result.err != nil || result.applied || !reflect.DeepEqual(result.receipt, core.CommandReceipt{}) {
			t.Fatalf("компенсация не уступила выигравшей остановке: %+v", result)
		}
	case <-ctx.Done():
		t.Fatal("компенсация не завершилась после остановки")
	}
	// receipt содержит сохраняемую квитанцию выбранной команды.
	if receipt := readReceipt(t, f.store, stop.RequestID); !reflect.DeepEqual(stop, receipt) {
		t.Fatalf("компенсация прервала принятую остановку: %+v", receipt)
	}
}

// Проверяет принятие компенсации точного успешно добавленного выхода с сохранением исходной успешной
// AddOutput-квитанции.
func TestAddedOutputStartCompensationUsesItsExactExecution(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// destination содержит добавленное назначение для проверки точной компенсации AddOutput. err
	// сообщает об ошибке сохранения назначения выбранной группы.
	destination, err := f.service.CreateDestination(ctx, core.Destination{StreamID: f.stream.ID, GroupID: f.group.ID, Name: "Добавленное назначение", Platform: "custom", Endpoint: "rtmp://example.invalid/live"})
	if err != nil {
		t.Fatal(err)
	}
	// command содержит внутреннюю квитанцию запуска выбранного выхода.
	command, err := f.service.AddOutput(ctx, core.AddOutputRequest{RequestID: "add-output", SessionID: start.SessionID, DestinationID: destination.ID, Binding: f.startRequest("unused").Bindings[0]})
	if err != nil {
		t.Fatal(err)
	}
	// fence идентифицирует точную захваченную попытку проверяемой команды.
	fence := executeCommand(t, f.service, command.RequestID)
	// original сохраняет принятую исходную квитанцию для проверки её неизменности.
	original := readReceipt(t, f.store, command.RequestID)
	// stop содержит принятую квитанцию остановки точного исполнения. applied показывает, принята ли
	// компенсация или повтор её прежней квитанции.
	stop, applied, err := f.service.CompensateOutputStart(ctx, fence, "added-output-compensation")
	if err != nil || !applied || stop.OutputID != command.OutputID || stop.Kind != core.CommandStopOutput {
		t.Fatalf("не принята компенсация добавленного выхода: %+v, %t, %v", stop, applied, err)
	}
	// after содержит повторно прочитанный долговечный снимок для проверки отсутствия изменений.
	if after := readReceipt(t, f.store, command.RequestID); !reflect.DeepEqual(original, after) {
		t.Fatal("компенсация изменила успешный AddOutput")
	}
}

// Проверяет компенсацию CLAIMED/ISSUED/SUCCEEDED запуска выхода с точной очисткой и изоляцией соседа;
// успех исходной команды сохраняется, а повтор компенсации идемпотентен.
func TestOutputStartCompensationTracksExactCleanupAndPreservesSuccess(t *testing.T) {
	// status выбирает стадию принятой, захваченной, выданной или завершённой команды.
	for _, status := range []core.CommandStatus{core.CommandClaimed, core.CommandIssued, core.CommandSucceeded} {
		t.Run(string(status), func(t *testing.T) {
			// Проверяет компенсацию CLAIMED, ISSUED либо SUCCEEDED выхода с точным обязательством очистки,
			// неизменным соседом и сохранением успешной исходной квитанции.

			// f объединяет временный Store, Service, поток и опубликованную заглушку
			// проверяемого сценария.
			f := newDomainFixture(t, 2)
			// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
			ctx := context.Background()
			// start содержит принятую квитанцию первоначального запуска сессии.
			start := startDomainSession(t, f)
			executeCommand(t, f.service, start.RequestID)
			// outputs содержит все сохраняемые выходы проверяемой сессии.
			outputs := readOutputs(t, f.store, start.SessionID)
			// command содержит внутреннюю квитанцию запуска выбранного выхода.
			command := outputStartReceipt(t, f, outputs[0].ID)
			// fence идентифицирует точную захваченную попытку проверяемой команды. err
			// сообщает об ошибке захвата точной команды исполнения.
			fence, err := f.service.Claim(ctx, command.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if status != core.CommandClaimed {
				// err сообщает об ошибке выдачи точной попытки команды.
				if err := f.service.MarkIssued(ctx, fence); err != nil {
					t.Fatal(err)
				}
			}
			if status == core.CommandSucceeded {
				// err сообщает об ошибке сохранения результата точной команды.
				if err := f.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
					t.Fatal(err)
				}
			}
			// original сохраняет принятую исходную квитанцию для проверки её неизменности.
			original := readReceipt(t, f.store, command.RequestID)
			// stop содержит принятую квитанцию остановки точного исполнения. applied
			// показывает, принята ли компенсация или повтор её прежней квитанции.
			stop, applied, err := f.service.CompensateOutputStart(ctx, fence, "activation-compensation")
			if err != nil || !applied || stop.Kind != core.CommandStopOutput || stop.Status != core.CommandPending || stop.OutputGeneration != fence.OutputGeneration+1 {
				t.Fatalf("не принята точная компенсация: %+v, %t, %v", stop, applied, err)
			}
			// output содержит сохраняемый выход с самостоятельным снимком и намерением.
			output, err := f.service.GetOutput(ctx, fence.OutputID)
			if err != nil || output.DesiredRunning || !output.Attached || !output.CleanupPending || output.Attempt != fence.Attempt || output.Issued != (status != core.CommandClaimed) {
				t.Fatalf("утрачено обязательство или выдумана issuance: %+v, %v", output, err)
			}
			// cleanups содержит сохраняемые намерения физической очистки после проверяемой
			// операции.
			cleanups := pendingCleanups(t, f.store, start.SessionID)
			if len(cleanups) != 1 || cleanups[0].OutputID != fence.OutputID || cleanups[0].Generation != fence.OutputGeneration || cleanups[0].Attempt != fence.Attempt || cleanups[0].Lifecycle != fence.Lifecycle || cleanups[0].Incarnation != fence.Incarnation {
				t.Fatalf("компенсация потеряла точный cleanup fence: %+v", cleanups)
			}
			// session содержит сохраняемый запуск для проверки состояния и владения.
			if session := readSession(t, f.store, start.SessionID); session.State != core.SessionRunning || !session.CleanupPending {
				t.Fatalf("компенсация изменила Session: %+v", session)
			}
			// neighbor сохраняет здоровый соседний выход до добавления другого. err сообщает
			// об ошибке чтения сохраняемого снимка и намерения выхода.
			if neighbor, err := f.service.GetOutput(ctx, outputs[1].ID); err != nil || !reflect.DeepEqual(outputs[1], neighbor) {
				t.Fatalf("компенсация изменила соседа: %+v, %v", neighbor, err)
			}
			// after содержит повторно прочитанный долговечный снимок для проверки отсутствия
			// изменений.
			after := readReceipt(t, f.store, command.RequestID)
			if status == core.CommandSucceeded {
				if !reflect.DeepEqual(original, after) {
					t.Fatal("компенсация переписала успешную квитанцию запуска")
				}
			} else if after.Status != core.CommandInterrupted {
				t.Fatalf("компенсация оставила команду исполнимой: %+v", after)
			}
			requireOutputVariantHolds(t, f.store, f.variant.ID, true)
			// replayed содержит квитанцию тождественного повторного запроса.
			replayed, applied, err := f.service.CompensateOutputStart(ctx, fence, stop.RequestID)
			if err != nil || !applied || !reflect.DeepEqual(stop, replayed) || len(pendingCleanups(t, f.store, start.SessionID)) != 1 {
				t.Fatalf("повтор компенсации изменил результат: %+v, %t, %v", replayed, applied, err)
			}
			// err сообщает об ошибке подтверждения точного намерения очистки.
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
				t.Fatal(err)
			}
			// receipt содержит сохраняемую квитанцию выбранной команды.
			if receipt := readReceipt(t, f.store, stop.RequestID); receipt.Status != core.CommandSucceeded {
				t.Fatalf("cleanup не завершил компенсацию: %+v", receipt)
			}
		})
	}
}

// Проверяет, что поздняя компенсация уступает пользовательским Stop/Detach до и после очистки и не
// создаёт новую квитанцию.
func TestOutputStartCompensationPreservesWinningUserStopAndDetach(t *testing.T) {
	// detach выбирает пользовательскую остановку либо отсоединение выхода.
	for _, detach := range []bool{false, true} {
		// name задаёт имя сценария пользовательской остановки либо отсоединения.
		name := "stop"
		if detach {
			name = "detach"
		}
		t.Run(name, func(t *testing.T) {
			// Проверяет, что поздняя компенсация уступает уже принятой остановке либо отсоединению выхода до и
			// после подтверждения очистки.

			// f объединяет временный Store, Service, поток и опубликованную заглушку
			// проверяемого сценария.
			f := newDomainFixture(t, 1)
			// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
			ctx := context.Background()
			// start содержит принятую квитанцию первоначального запуска сессии.
			start := startDomainSession(t, f)
			executeCommand(t, f.service, start.RequestID)
			// output содержит сохраняемый выход с самостоятельным снимком и намерением.
			output := readOutputs(t, f.store, start.SessionID)[0]
			// command содержит внутреннюю квитанцию запуска выбранного выхода.
			command := outputStartReceipt(t, f, output.ID)
			// fence идентифицирует точную захваченную попытку проверяемой команды.
			fence := executeCommand(t, f.service, command.RequestID)
			// request задаёт пользовательскую остановку либо отсоединение проверяемого
			// выхода.
			request := core.OutputCommand{RequestID: "user-stop", OutputID: output.ID}
			// stop содержит принятую квитанцию остановки точного исполнения.
			var stop core.CommandReceipt
			// err передаёт результат выбранной пользовательской остановки или отсоединения.
			var err error
			if detach {
				stop, err = f.service.DetachOutput(ctx, request)
			} else {
				stop, err = f.service.StopOutput(ctx, request)
			}
			if err != nil {
				t.Fatal(err)
			}
			// before сохраняет исходный долговечный снимок управления для проверки
			// отсутствия изменений.
			before := readOutputs(t, f.store, start.SessionID)
			// cleanups содержит сохраняемые намерения физической очистки после проверяемой
			// операции.
			cleanups := pendingCleanups(t, f.store, start.SessionID)
			// assertNoCompensation проверяет отказ поздней компенсации без квитанции и без
			// изменения выигравшего управления.
			assertNoCompensation := func() {
				// Проверяет отсутствие новой компенсации и её квитанции и неизменность выигравшей пользовательской
				// команды.

				t.Helper()
				// result содержит квитанцию и факт применения проверяемой компенсации.
				// applied показывает, принята ли компенсация или повтор её прежней
				// квитанции. err сообщает об ошибке условной компенсации точной попытки
				// запуска выхода.
				result, applied, err := f.service.CompensateOutputStart(ctx, fence, "late-activation-compensation")
				if err != nil || applied || !reflect.DeepEqual(result, core.CommandReceipt{}) {
					t.Fatalf("отозванный запуск принял компенсацию: %+v, %t, %v", result, applied, err)
				}
				// receipt содержит сохраняемую квитанцию выбранной команды.
				if receipt := readReceipt(t, f.store, stop.RequestID); !reflect.DeepEqual(stop, receipt) {
					t.Fatalf("компенсация прервала выигравшую команду: %+v", receipt)
				}
				// err сообщает об ошибке проверки отсутствия квитанции отклонённой
				// поздней компенсации.
				if err := f.store.View(ctx, func(r core.Repositories) error {
					// Ищет квитанцию отклонённой поздней компенсации для проверки ErrNotFound.

					// err сообщает об ошибке чтения сохраняемой квитанции.
					_, err := r.Runtime.Receipt(ctx, "late-activation-compensation")
					return err
				}); !errors.Is(err, core.ErrNotFound) {
					t.Fatalf("отозванный запуск создал квитанцию: %v", err)
				}
			}
			assertNoCompensation()
			if !reflect.DeepEqual(before, readOutputs(t, f.store, start.SessionID)) || !reflect.DeepEqual(cleanups, pendingCleanups(t, f.store, start.SessionID)) {
				t.Fatal("компенсация изменила остановленный выход или cleanup")
			}
			if len(cleanups) != 1 {
				t.Fatalf("ожидалось одно обязательство: %+v", cleanups)
			}
			// err сообщает об ошибке подтверждения точного намерения очистки.
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
				t.Fatal(err)
			}
			stop = readReceipt(t, f.store, stop.RequestID)
			if stop.Status != core.CommandSucceeded {
				t.Fatal("выигравшая команда не завершилась")
			}
			assertNoCompensation()
			// receipt содержит сохраняемую квитанцию выбранной команды.
			if receipt := readReceipt(t, f.store, command.RequestID); receipt.Status != core.CommandSucceeded {
				t.Fatal("успешный запуск изменён поздней компенсацией")
			}
		})
	}
}

// Проверяет отказ новой компенсации старого ExecutionFence и повтор прежней квитанции без отзыва
// следующего исполнения и его удержаний.
func TestOutputStartCompensationCannotStopNewerExecution(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// output содержит сохраняемый выход с самостоятельным снимком и намерением.
	output := readOutputs(t, f.store, start.SessionID)[0]
	// command содержит внутреннюю квитанцию запуска выбранного выхода.
	command := outputStartReceipt(t, f, output.ID)
	// old содержит точную прежнюю попытку запуска, отзываемую последующей остановкой.
	old := executeCommand(t, f.service, command.RequestID)
	// compensation содержит принятую квитанцию остановки точного прежнего исполнения. applied
	// показывает, принята ли компенсация или повтор её прежней квитанции. err сообщает об ошибке
	// условной компенсации точной попытки запуска выхода.
	compensation, applied, err := f.service.CompensateOutputStart(ctx, old, "old-compensation")
	if err != nil || !applied {
		t.Fatal("не принята исходная компенсация", err)
	}
	// cleanups содержит сохраняемые намерения физической очистки после проверяемой операции.
	cleanups := pendingCleanups(t, f.store, start.SessionID)
	if len(cleanups) != 1 {
		t.Fatal("утрачено обязательство старого запуска")
	}
	// err сообщает об ошибке подтверждения точного намерения очистки.
	if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
		t.Fatal(err)
	}
	compensation = readReceipt(t, f.store, compensation.RequestID)
	// restart содержит принятую квитанцию нового запуска прикреплённого выхода.
	restart, err := f.service.StartOutput(ctx, core.OutputCommand{RequestID: "fresh-start", OutputID: output.ID})
	if err != nil {
		t.Fatal(err)
	}
	// fresh идентифицирует точную выданную попытку нового ручного исполнения.
	fresh := executeCommand(t, f.service, restart.RequestID)
	// owner содержит логический допуск нового точного исполнения.
	owner, err := f.service.BindExecutionOwner(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	// before сохраняет исходный долговечный снимок управления для проверки отсутствия изменений.
	before := readOutputs(t, f.store, start.SessionID)
	// requestID выбирает новый идентификатор поздней компенсации либо повтор прежнего.
	for _, requestID := range []string{"late-old-compensation", compensation.RequestID} {
		// result содержит квитанцию и факт применения проверяемой компенсации. applied
		// показывает, принята ли компенсация или повтор её прежней квитанции. err сообщает об
		// ошибке условной компенсации точной попытки запуска выхода.
		result, applied, err := f.service.CompensateOutputStart(ctx, old, requestID)
		if err != nil {
			t.Fatal(err)
		}
		if requestID == compensation.RequestID {
			if !applied || !reflect.DeepEqual(compensation, result) {
				t.Fatalf("повтор квитанции переоценил новый запуск: %+v, %t", result, applied)
			}
		} else if applied || !reflect.DeepEqual(result, core.CommandReceipt{}) {
			t.Fatal("старая компенсация принята для нового запуска")
		}
	}
	if !owner.Valid() || !reflect.DeepEqual(before, readOutputs(t, f.store, start.SessionID)) || len(pendingCleanups(t, f.store, start.SessionID)) != 0 {
		t.Fatal("старая компенсация изменила новое исполнение")
	}
	// receipt содержит сохраняемую квитанцию выбранной команды.
	if receipt := readReceipt(t, f.store, restart.RequestID); receipt.Status != core.CommandSucceeded {
		t.Fatal("прервана новая команда запуска")
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, true)
}

// Проверяет неприменимость каждого несовпадающего поля ExecutionFence без изменения выхода и очистки,
// а также отказ пустого и конфликтующего RequestID.
func TestOutputStartCompensationRejectsMismatchedFenceWithoutMutation(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// output содержит сохраняемый выход с самостоятельным снимком и намерением.
	output := readOutputs(t, f.store, start.SessionID)[0]
	// command содержит внутреннюю квитанцию запуска выбранного выхода.
	command := outputStartReceipt(t, f, output.ID)
	// fence идентифицирует точную захваченную попытку проверяемой команды.
	fence := executeCommand(t, f.service, command.RequestID)
	// before сохраняет исходный долговечный снимок управления для проверки отсутствия изменений.
	before := readOutputs(t, f.store, start.SessionID)
	// change подменяет выбранное поле точной идентичности компенсации.
	for _, change := range []func(*core.ExecutionFence){
		func(f *core.ExecutionFence) {
			// Подменяет CommandID попытки компенсации отсутствующей командой.
			f.CommandID = "missing-command"
		},
		func(f *core.ExecutionFence) {
			// Подменяет Incarnation попытки компенсации другим владельцем.
			f.Incarnation = "different-owner"
		},
		func(f *core.ExecutionFence) {
			// Подменяет SessionID попытки компенсации другой сессией.
			f.SessionID = "different-session"
		},
		func(f *core.ExecutionFence) {
			// Подменяет OutputID попытки компенсации другим выходом.
			f.OutputID = "different-output"
		},
		func(f *core.ExecutionFence) {
			// Увеличивает Lifecycle попытки компенсации, делая ExecutionFence неактуальным.
			f.Lifecycle++
		},
		func(f *core.ExecutionFence) {
			// Увеличивает OutputGeneration попытки компенсации, делая ExecutionFence неактуальным.
			f.OutputGeneration++
		},
		func(f *core.ExecutionFence) {
			// Увеличивает Attempt попытки компенсации, делая ExecutionFence неактуальным.
			f.Attempt++
		},
	} {
		// stale содержит копию ExecutionFence с несовпадающим полем.
		stale := fence
		change(&stale)
		// result содержит квитанцию и факт применения проверяемой компенсации. applied
		// показывает, принята ли компенсация или повтор её прежней квитанции. err сообщает об
		// ошибке условной компенсации точной попытки запуска выхода.
		result, applied, err := f.service.CompensateOutputStart(ctx, stale, "stale-compensation")
		if err != nil || applied || !reflect.DeepEqual(result, core.CommandReceipt{}) {
			t.Fatalf("принят несовпадающий fence: %+v, %t, %v", result, applied, err)
		}
	}
	if !reflect.DeepEqual(before, readOutputs(t, f.store, start.SessionID)) || len(pendingCleanups(t, f.store, start.SessionID)) != 0 {
		t.Fatal("несовпадающий fence изменил выход или cleanup")
	}
	// err сообщает об ошибке условной компенсации точной попытки запуска выхода.
	_, _, err := f.service.CompensateOutputStart(ctx, fence, "")
	requireDomainError(t, err, core.ErrInvalid)
	_, err = f.service.PauseSession(ctx, core.SessionCommand{RequestID: "conflicting-command", SessionID: start.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.service.CompensateOutputStart(ctx, fence, "conflicting-command")
	requireDomainError(t, err, core.ErrConflict)
}
