package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет компенсацию запуска на всех стадиях, точную очистку только выданной работы, сохранение
// успешной квитанции и изоляцию нового ручного запуска от старых событий.
func TestSessionExecutionCompensationPreservesExactLiabilitiesAndSuccess(t *testing.T) {
	// status выбирает стадию принятой, захваченной, выданной или завершённой команды.
	for _, status := range []core.CommandStatus{core.CommandPending, core.CommandClaimed, core.CommandIssued, core.CommandSucceeded} {
		t.Run(string(status), func(t *testing.T) {
			// Проверяет точную компенсацию PENDING, CLAIMED, ISSUED либо SUCCEEDED Session, сохранность
			// обязательств и успешной квитанции и независимость следующего ручного запуска от прежней очистки.

			// f объединяет временный Store, Service, поток и опубликованную заглушку
			// проверяемого сценария.
			f := newDomainFixture(t, 1)
			// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
			ctx := context.Background()
			// start содержит принятую квитанцию первоначального запуска сессии.
			start := startDomainSession(t, f)
			// fence идентифицирует точную захваченную попытку проверяемой команды.
			fence := core.ExecutionFence{CommandID: start.ID, SessionID: start.SessionID, Incarnation: start.Incarnation, Lifecycle: start.Lifecycle}
			if status != core.CommandPending {
				// err передаёт результат захвата первоначального запуска на выбранной
				// стадии.
				var err error
				fence, err = f.service.Claim(ctx, start.RequestID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if status == core.CommandIssued || status == core.CommandSucceeded {
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
			original := readReceipt(t, f.store, start.RequestID)
			if status == core.CommandPending {
				// err сообщает об ошибке структурной проверки ExecutionFence
				// компенсации.
				if err := fence.Validate(); !errors.Is(err, core.ErrInvalid) {
					t.Fatal("нулевой Attempt допущен общим ExecutionFence", err)
				}
				// owner содержит возможный допуск незахваченной команды, который должен
				// отсутствовать. err сообщает об ошибке проверки и выдачи логического
				// допуска точного исполнения.
				if owner, err := f.service.BindExecutionOwner(ctx, fence); !errors.Is(err, core.ErrStale) || owner != nil {
					t.Fatal("незахваченная команда получила владельца исполнения", owner, err)
				}
			}
			// stop содержит принятую квитанцию остановки точного исполнения. applied
			// показывает, принята ли компенсация или повтор её прежней квитанции. err
			// сообщает об ошибке условной компенсации точного запуска сессии.
			stop, applied, err := f.service.CompensateSessionExecution(ctx, fence, "retire-owner")
			if err != nil || !applied || stop.Kind != core.CommandStopSession || stop.Lifecycle != fence.Lifecycle+1 {
				t.Fatalf("не принята точная компенсация: %+v %t %v", stop, applied, err)
			}
			// session содержит сохраняемый запуск для проверки состояния и владения.
			session := readSession(t, f.store, start.SessionID)
			// issued задаёт ожидаемое наличие выданных ресурсов на выбранной стадии команды.
			issued := status == core.CommandIssued || status == core.CommandSucceeded
			if session.State != core.SessionStopping || session.CleanupPending != issued || session.Issued != issued {
				t.Fatalf("компенсация потеряла состояние или выдумала issuance: %+v", session)
			}
			// cleanups содержит сохраняемые намерения физической очистки после проверяемой
			// операции.
			cleanups := pendingCleanups(t, f.store, start.SessionID)
			if issued {
				if len(cleanups) != 1 || cleanups[0].OutputID != "" || cleanups[0].SessionID != fence.SessionID || cleanups[0].Lifecycle != fence.Lifecycle || cleanups[0].Incarnation != fence.Incarnation {
					t.Fatalf("утрачен точный parent cleanup: %+v", cleanups)
				}
				requireOutputVariantHolds(t, f.store, f.variant.ID, true)
			} else if len(cleanups) != 0 {
				t.Fatalf("создана фиктивная очистка: %+v", cleanups)
			}
			if status == core.CommandSucceeded && !reflect.DeepEqual(original, readReceipt(t, f.store, start.RequestID)) {
				t.Fatal("переписана успешная квитанция запуска")
			}
			// replay содержит повторно прочитанную квитанцию прежней компенсации.
			replay, applied, err := f.service.CompensateSessionExecution(ctx, fence, stop.RequestID)
			if err != nil || !applied || !reflect.DeepEqual(stop, replay) {
				t.Fatalf("повтор изменил компенсацию: %+v %t %v", replay, applied, err)
			}
			executeCommand(t, f.service, stop.RequestID)
			// settled сохраняет окончательную квитанцию компенсации до её повторного
			// запроса.
			settled := readReceipt(t, f.store, stop.RequestID)
			// Неподтверждённая очистка старого исполнения не мешает новому ручному запуску.
			// next содержит принятую квитанцию следующего ручного запуска.
			next, err := f.service.StartSession(ctx, f.startRequest("next-session"))
			if err != nil {
				t.Fatal(err)
			}
			// fresh идентифицирует точную выданную попытку нового ручного исполнения.
			fresh := executeCommand(t, f.service, next.RequestID)
			// owner содержит логический допуск нового точного исполнения.
			owner, err := f.service.BindExecutionOwner(ctx, fresh)
			if err != nil {
				t.Fatal(err)
			}
			// before сохраняет исходный долговечный снимок управления для проверки
			// отсутствия изменений.
			before := readSession(t, f.store, next.SessionID)
			for range 3 {
				_, applied, err = f.service.CompensateSessionExecution(ctx, fence, "late-retirement")
				if err != nil || applied {
					t.Fatal("старое исполнение повторно отозвано", applied, err)
				}
			}
			replay, applied, err = f.service.CompensateSessionExecution(ctx, fence, stop.RequestID)
			if err != nil || !applied || !reflect.DeepEqual(settled, replay) {
				t.Fatal("повтор квитанции переоценил новое исполнение", replay, applied, err)
			}
			// intent содержит точное прежнее обязательство очистки, подтверждаемое после
			// нового запуска.
			for _, intent := range cleanups {
				if err = f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(intent)); err != nil {
					t.Fatal(err)
				}
			}
			if !owner.Valid() || !reflect.DeepEqual(before, readSession(t, f.store, next.SessionID)) {
				t.Fatal("старая очистка изменила новый запуск")
			}
			requireOutputVariantHolds(t, f.store, f.variant.ID, true)
		})
	}
}

// Проверяет уступку задержанной компенсации принятому пользовательскому Stop без изменения его
// квитанции и намерений очистки.
func TestSessionExecutionCompensationLosesAtomicallyToUserStop(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite. cancel отменяет или
	// освобождает ограниченный контекст проверяемого управления.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// fence идентифицирует точную захваченную попытку проверяемой команды.
	fence := executeCommand(t, f.service, start.RequestID)
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
		// applied показывает, применилась ли конкурентная компенсация сессии.
		applied bool
		// err сообщает об ошибке конкурентной компенсации сессии.
		err error
	}
	// done передаёт итог конкурентной компенсации после разрешения её записи.
	done := make(chan outcome, 1)
	// joined закрывается после физического завершения горутины компенсации.
	joined := make(chan struct{})
	go func() {
		// В отдельной горутине начинает компенсацию Session и передаёт результат после управляемой границы
		// Write.

		defer close(joined)
		// applied показывает, принята ли компенсация или повтор её прежней квитанции. err
		// сообщает об ошибке условной компенсации точного запуска сессии.
		_, applied, err := service.CompensateSessionExecution(ctx, fence, "losing-retirement")
		done <- outcome{applied, err}
	}()
	defer func() {
		// Отменяет контекст компенсации и дожидается её горутины при любом выходе из теста.
		cancel()
		<-joined
	}()
	select {
	case <-waiting.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// stop содержит принятую квитанцию остановки точного исполнения.
	stop, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "winning-stop", SessionID: start.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	// cleanups содержит сохраняемые намерения физической очистки после проверяемой операции.
	cleanups := pendingCleanups(t, f.store, start.SessionID)
	close(waiting.resume)
	select {
	// result содержит квитанцию и факт применения проверяемой компенсации.
	case result := <-done:
		if result.err != nil || result.applied {
			t.Fatal("компенсация не уступила остановке", result)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !reflect.DeepEqual(stop, readReceipt(t, f.store, stop.RequestID)) || !reflect.DeepEqual(cleanups, pendingCleanups(t, f.store, start.SessionID)) {
		t.Fatal("компенсация изменила выигравшую остановку или её обязательства")
	}
	executeCommand(t, f.service, stop.RequestID)
}

// Проверяет различие некорректных и устаревших ExecutionFence, запрет чужой incarnation и конфликт
// RequestID без изменения Session и очистки.
func TestSessionExecutionCompensationRejectsMalformedAndStaleFences(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// fence идентифицирует точную захваченную попытку проверяемой команды.
	fence := executeCommand(t, f.service, start.RequestID)
	// before сохраняет исходный долговечный снимок управления для проверки отсутствия изменений.
	before := readSession(t, f.store, start.SessionID)
	// foreignService предоставляет Service с заведомо чужой incarnation для проверки отказа
	// компенсации. err сообщает об ошибке создания этого тестового Service.
	foreignService, err := core.NewService(f.store, "other-incarnation")
	if err != nil {
		t.Fatal(err)
	}
	// applied показывает, принята ли компенсация или повтор её прежней квитанции. err сообщает об
	// ошибке условной компенсации точного запуска сессии.
	if _, applied, err := foreignService.CompensateSessionExecution(ctx, fence, "foreign-retirement"); err != nil || applied {
		t.Fatal("чужая инкарнация отозвала исполнение", applied, err)
	}
	// change подменяет выбранное поле точной идентичности компенсации.
	for _, change := range []func(*core.ExecutionFence){
		func(f *core.ExecutionFence) {
			// Подменяет CommandID компенсации Session отсутствующей командой.
			f.CommandID = "missing-command"
		},
		func(f *core.ExecutionFence) {
			// Подменяет SessionID компенсации отсутствующей сессией.
			f.SessionID = "missing-session"
		},
		func(f *core.ExecutionFence) {
			// Подменяет Incarnation компенсации чужим владельцем.
			f.Incarnation = "other-incarnation"
		},
		func(f *core.ExecutionFence) {
			// Увеличивает Lifecycle компенсации Session.
			f.Lifecycle++
		},
		func(f *core.ExecutionFence) {
			// Увеличивает Attempt компенсации Session.
			f.Attempt++
		},
		func(f *core.ExecutionFence) {
			// Обнуляет Attempt уже исполненной команды, делая компенсацию неактуальной.
			f.Attempt = 0
		},
	} {
		// stale содержит копию ExecutionFence с несовпадающим полем.
		stale := fence
		change(&stale)
		// applied показывает, принята ли компенсация или повтор её прежней квитанции. err
		// сообщает об ошибке условной компенсации точного запуска сессии.
		_, applied, err := f.service.CompensateSessionExecution(ctx, stale, "stale-retirement")
		if err != nil || applied {
			t.Fatal("принят чужой fence", stale, applied, err)
		}
	}
	// change подменяет выбранное поле точной идентичности компенсации.
	for _, change := range []func(*core.ExecutionFence){
		func(f *core.ExecutionFence) {
			// Убирает обязательный CommandID ExecutionFence компенсации.
			f.CommandID = ""
		},
		func(f *core.ExecutionFence) {
			// Обнуляет обязательный Lifecycle ExecutionFence компенсации.
			f.Lifecycle = 0
		},
		func(f *core.ExecutionFence) {
			// Убирает обязательный Incarnation ExecutionFence компенсации.
			f.Incarnation = ""
		},
		func(f *core.ExecutionFence) {
			// Задаёт OutputGeneration без OutputID для проверки некорректного ExecutionFence компенсации Session.
			f.OutputGeneration = 1
		},
	} {
		// malformed содержит структурно недопустимую копию ExecutionFence компенсации.
		malformed := fence
		change(&malformed)
		// err сообщает об ошибке условной компенсации точного запуска сессии.
		_, _, err := f.service.CompensateSessionExecution(ctx, malformed, "malformed-retirement")
		if !errors.Is(err, core.ErrInvalid) {
			t.Fatal("принят некорректный fence", malformed, err)
		}
	}
	_, _, err = f.service.CompensateSessionExecution(ctx, fence, "")
	requireDomainError(t, err, core.ErrInvalid)
	_, _, err = f.service.CompensateSessionExecution(ctx, fence, start.RequestID)
	requireDomainError(t, err, core.ErrConflict)
	if !reflect.DeepEqual(before, readSession(t, f.store, start.SessionID)) || len(pendingCleanups(t, f.store, start.SessionID)) != 0 {
		t.Fatal("неверный fence изменил Session")
	}
}
