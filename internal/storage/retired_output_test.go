package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// retiredRuntimeSnapshot сохраняет согласованную историю и управление одного запуска для сравнения до и
// после команд.
type retiredRuntimeSnapshot struct {
	// session содержит сохраняемый запуск для проверки состояния и владения.
	session core.Session
	// outputs содержит все сохраняемые выходы проверяемой сессии.
	outputs []core.SessionOutput
	// receipts содержит сохраняемые квитанции команд выбранного запуска.
	receipts []core.CommandReceipt
	// events содержит служебные события выбранного запуска.
	events []core.Event
	// cleanups содержит сохраняемые намерения физической очистки после проверяемой операции.
	cleanups []core.CleanupIntent
}

// Читает согласованный снимок Session, её выходов, квитанций, событий и намерений очистки для проверки
// отсутствия изменений.
func readRetiredRuntime(t *testing.T, f domainFixture, sessionID core.ID) retiredRuntimeSnapshot {
	t.Helper()
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// snapshot принимает согласованные сессию, выходы, квитанции, события и очистку прежнего
	// запуска.
	var snapshot retiredRuntimeSnapshot
	// err сообщает об ошибке согласованного чтения сессии, выходов, квитанций, событий и очистки
	// прежнего запуска.
	if err := f.store.View(ctx, func(r core.Repositories) error {
		// В одном согласованном чтении собирает Session, выходы, квитанции, события и очистку старого запуска
		// для точного сравнения до и после команды.

		// q задаёт ограниченное чтение записей одной сессии.
		q := core.ListQuery{Limit: core.MaxListLimit, SessionID: sessionID}
		// err передаёт отказ чтения очередной части согласованного снимка прежнего запуска.
		var err error
		if snapshot.session, err = r.Runtime.Session(ctx, sessionID); err != nil {
			return err
		}
		if snapshot.outputs, err = r.Runtime.Outputs(ctx, q); err != nil {
			return err
		}
		if snapshot.receipts, err = r.Runtime.Receipts(ctx, q); err != nil {
			return err
		}
		if snapshot.events, err = r.Runtime.Events(ctx, q); err != nil {
			return err
		}
		snapshot.cleanups, err = r.Artifacts.Cleanups(ctx, q)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// Проверяет отказ новых Stop/Detach прежнего владельца при сохранении повторов квитанций,
// невозможность их исполнения и изоляцию нового ручного запуска.
func TestRetiredOutputControlsRejectNewRequestsAndPreserveReplay(t *testing.T) {
	// kind выбирает StopOutput или DetachOutput для одинаковой проверки прежнего владельца.
	for _, kind := range []core.CommandKind{core.CommandStopOutput, core.CommandDetachOutput} {
		t.Run(string(kind), func(t *testing.T) {
			// Проверяет отказ новых StopOutput либо DetachOutput для выхода прежнего владельца при сохранении
			// повторных квитанций и независимости нового запуска.

			// f объединяет временный Store, Service, поток и опубликованную заглушку
			// проверяемого сценария.
			f := newDomainFixture(t, 1)
			// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
			ctx := context.Background()
			// control вызывает выбранное управление через текущий Service, включая повторно
			// открытый Store.
			control := func(request core.OutputCommand) (core.CommandReceipt, error) {
				// Вызывает выбранную StopOutput либо DetachOutput через актуальное значение f.service, в том числе
				// после повторного открытия Store.

				if kind == core.CommandStopOutput {
					return f.service.StopOutput(ctx, request)
				}
				return f.service.DetachOutput(ctx, request)
			}
			// start содержит принятую квитанцию первоначального запуска сессии.
			start := startDomainSession(t, f)
			executeCommand(t, f.service, start.RequestID)
			// completedStart сохраняет успешную первоначальную квитанцию запуска до смены
			// владельца.
			completedStart := readReceipt(t, f.store, start.RequestID)
			// output содержит сохраняемый выход с самостоятельным снимком и намерением.
			output := readOutputs(t, f.store, start.SessionID)[0]
			executeCommand(t, f.service, outputStartReceipt(t, f, output.ID).RequestID)
			// original задаёт исходный повторяемый запрос StopOutput или DetachOutput.
			original := core.OutputCommand{RequestID: "original-output-control", OutputID: output.ID}
			// accepted содержит первоначально принятую квитанцию StopOutput либо
			// DetachOutput. err сообщает об ошибке принятия этой команды.
			accepted, err := control(original)
			if err != nil || accepted.Status != core.CommandPending {
				t.Fatalf("выданный выход должен ожидать очистки: статус %s, ошибка %v", accepted.Status, err)
			}
			// oldIncarnation сохраняет идентичность прежнего открытия Store.
			oldIncarnation := f.store.Incarnation()
			f = reopenFixture(t, f)
			if f.store.Incarnation() == oldIncarnation {
				t.Fatal("повторное открытие не сменило владельца")
			}
			// recovered содержит прежний запуск после восстановления при новом открытии
			// Store.
			recovered := readSession(t, f.store, start.SessionID)
			if recovered.State != core.SessionInterrupted || !recovered.CleanupPending || recovered.Incarnation != oldIncarnation {
				t.Fatalf("не восстановлено прерывание старого владельца: %+v", recovered)
			}
			// interrupted содержит прерванную квитанцию управления прежнего владельца.
			interrupted := readReceipt(t, f.store, original.RequestID)
			if interrupted.Status != core.CommandInterrupted {
				t.Fatalf("прежняя незавершённая команда не прервана: %s", interrupted.Status)
			}
			// cleanups содержит сохраняемые намерения физической очистки после проверяемой
			// операции.
			cleanups := pendingCleanups(t, f.store, start.SessionID)
			if len(cleanups) != 2 {
				t.Fatalf("нужны отдельные подтверждения родителя и выхода: %d", len(cleanups))
			}

			// rejectNew проверяет отказ нового управления прежним выходом без изменения
			// истории и очистки.
			rejectNew := func(requestID string) {
				// Проверяет, что новая команда старому выходу отклоняется без изменения истории, очистки и без
				// создания квитанции.

				t.Helper()
				// before сохраняет исходный долговечный снимок управления для проверки
				// отсутствия изменений.
				before := readRetiredRuntime(t, f, start.SessionID)
				// receipt содержит сохраняемую квитанцию выбранной команды. err сообщает
				// об ошибке выбранного управления выходом прежнего владельца.
				receipt, err := control(core.OutputCommand{RequestID: requestID, OutputID: output.ID})
				if !errors.Is(err, core.ErrStale) && !errors.Is(err, core.ErrConflict) {
					t.Errorf("новая команда к выходу прежнего владельца должна отклоняться: статус %s, ошибка %v", receipt.Status, err)
				}
				// after содержит повторно прочитанный долговечный снимок для проверки
				// отсутствия изменений.
				if after := readRetiredRuntime(t, f, start.SessionID); !reflect.DeepEqual(before, after) {
					t.Error("отклонение изменило Session, Output, квитанции, события или очистку")
				}
				// err сообщает об ошибке проверки отсутствия квитанции нового управления
				// прежним выходом.
				if err := f.store.View(ctx, func(r core.Repositories) error {
					// Проверяет отсутствие сохранённой квитанции отклонённого RequestID.

					// err сообщает об ошибке чтения сохраняемой квитанции.
					_, err := r.Runtime.Receipt(ctx, requestID)
					if !errors.Is(err, core.ErrNotFound) {
						t.Errorf("отклонённый RequestID получил сохранённую квитанцию: %v", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			// replayOriginal проверяет точный повтор прежних квитанций без нового
			// исполнения.
			replayOriginal := func() {
				// Проверяет точный повтор прежних квитанций управления и запуска без нового Claim или изменения
				// сохранённого состояния.

				t.Helper()
				// before сохраняет исходный долговечный снимок управления для проверки
				// отсутствия изменений.
				before := readRetiredRuntime(t, f, start.SessionID)
				// replayed содержит квитанцию тождественного повторного запроса. err
				// сообщает об ошибке выбранного управления выходом прежнего владельца.
				replayed, err := control(original)
				if err != nil || !reflect.DeepEqual(replayed, interrupted) {
					t.Errorf("повтор прежнего управления потерял принятый результат: статус %s, ошибка %v", replayed.Status, err)
				}
				replayed, err = f.service.StartSession(ctx, f.startRequest(start.RequestID))
				if err != nil || !reflect.DeepEqual(replayed, completedStart) {
					t.Errorf("повтор завершённого запуска потерял принятый результат: статус %s, ошибка %v", replayed.Status, err)
				}
				// err сообщает об ошибке захвата точной команды исполнения.
				if _, err := f.service.Claim(ctx, original.RequestID); !errors.Is(err, core.ErrStale) {
					t.Errorf("повторное исполнение прерванной команды разрешено: %v", err)
				}
				// err сообщает об ошибке захвата точной команды исполнения.
				if _, err := f.service.Claim(ctx, start.RequestID); !errors.Is(err, core.ErrStale) {
					t.Errorf("повторное исполнение завершённого запуска разрешено: %v", err)
				}
				// after содержит повторно прочитанный долговечный снимок для проверки
				// отсутствия изменений.
				if after := readRetiredRuntime(t, f, start.SessionID); !reflect.DeepEqual(before, after) {
					t.Error("повтор прежней команды изменил сохранённое состояние")
				}
			}

			replayOriginal()
			rejectNew("new-retired-output-control")
			// cleanup содержит очередное точное намерение прежней очистки для подтверждения.
			for _, cleanup := range cleanups {
				// err сообщает об ошибке подтверждения точного намерения очистки.
				if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanup)); err != nil {
					t.Fatal(err)
				}
			}
			// pending содержит оставшиеся неподтверждённые намерения очистки прежнего
			// запуска.
			if pending := pendingCleanups(t, f.store, start.SessionID); len(pending) != 0 {
				t.Fatalf("подтверждённая очистка осталась незавершённой: %d", len(pending))
			}
			rejectNew("new-retired-output-control")
			rejectNew("new-control-after-cleanup")
			replayOriginal()
			// err сообщает об ошибке проверки отсутствия незавершённых квитанций после
			// старой очистки.
			if err := f.store.View(ctx, func(r core.Repositories) error {
				// Проверяет отсутствие незавершённых квитанций после подтверждения обоих намерений очистки старого
				// запуска.

				// pending содержит незавершённые квитанции прежнего запуска после
				// подтверждения его очистки. err сообщает об ошибке чтения квитанций
				// прежнего запуска.
				pending, err := r.Runtime.Receipts(ctx, core.ListQuery{Limit: core.MaxListLimit, SessionID: start.SessionID, PendingOnly: true})
				if err == nil && len(pending) != 0 {
					t.Errorf("очистка оставила невыполнимые намерения: %d, статус %s", len(pending), pending[0].Status)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			// stop содержит принятую квитанцию остановки точного исполнения.
			stop, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "stop-retired-session", SessionID: start.SessionID})
			if err != nil || stop.Status != core.CommandSucceeded {
				t.Fatalf("stop завершённой Session должен быть успешной пустой операцией: статус %s, ошибка %v", stop.Status, err)
			}
			// fresh содержит принятую квитанцию нового ручного запуска после прежней
			// очистки.
			fresh, err := f.service.StartSession(ctx, f.startRequest("manual-after-retired-cleanup"))
			if err != nil || fresh.SessionID == start.SessionID {
				t.Fatalf("ручной запуск после очистки не создал новую Session: %v", err)
			}
			executeCommand(t, f.service, fresh.RequestID)
			// freshOutput содержит выход нового ручного запуска после очистки прежнего.
			freshOutput := readOutputs(t, f.store, fresh.SessionID)[0]
			// fence идентифицирует точную захваченную попытку проверяемой команды.
			fence := executeCommand(t, f.service, outputStartReceipt(t, f, freshOutput.ID).RequestID)
			// observer содержит наблюдателя точной успешно завершённой попытки выхода.
			observer := openOutputObserver(t, f.service, fence)
			// healthy сохраняет согласованные управление и историю нового здорового запуска.
			healthy := readRetiredRuntime(t, f, fresh.SessionID)
			if healthy.session.State != core.SessionRunning || !healthy.outputs[0].Issued || !healthy.outputs[0].DesiredRunning {
				t.Fatal("новый запуск не сохранил RUNNING и issuance")
			}
			rejectNew("old-target-under-new-session")
			// after содержит повторно прочитанный долговечный снимок для проверки отсутствия
			// изменений.
			if after := readRetiredRuntime(t, f, fresh.SessionID); !reflect.DeepEqual(healthy, after) {
				t.Error("управление старым выходом изменило здоровый новый запуск")
			}
			// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
			if err := observer.Observe(ctx, core.OutputObservation{State: core.OutputRetrying, Error: "Временный отказ сети"}); err != nil {
				t.Fatalf("старый выход отозвал допуск нового publisher: %v", err)
			}
			// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
			if err := observer.Observe(ctx, core.OutputObservation{State: core.OutputPublishing, Error: ""}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
