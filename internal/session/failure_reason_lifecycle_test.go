package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// reasonRetirementStore моделирует отказ либо неподтверждённый исход записи поверх настоящей SQLite.
// Управление выполняется последовательно без диспетчера или медиапроцессов.
type reasonRetirementStore struct {
	// Store сохраняет реальный транзакционный механизм и исходные данные фикстуры.
	*storage.Store
	// rejectWrite отменяет запись до любых изменений.
	rejectWrite bool
	// rejectRead скрывает исход после ошибки записи.
	rejectRead bool
	// ambiguousCommit возвращает ошибку после настоящего успешного commit.
	ambiguousCommit bool
}

// Write выполняет callback в настоящей транзакции либо создаёт управляемый отказ её подтверждения.
func (s *reasonRetirementStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	if s.rejectWrite {
		return core.ErrBusy
	}
	// err сохраняет настоящий отказ SQLite; искусственная ошибка возможна только после успешного commit.
	if err := s.Store.Write(ctx, callback); err != nil {
		return err
	}
	if s.ambiguousCommit {
		s.ambiguousCommit = false
		return core.ErrBusy
	}
	return nil
}

// View позволяет скрыть результат Complete; callback при доступном чтении работает с реальной SQLite.
func (s *reasonRetirementStore) View(ctx context.Context, callback func(core.Repositories) error) error {
	if s.rejectRead {
		return core.ErrBusy
	}
	return s.Store.View(ctx, callback)
}

// TestOBSActiveReasonRequiresExactImmediateWrapper защищает существующий узкий допуск OBS:
// лишь bare sentinel и собственная непосредственная session_active разрешают одно перечитывание.
func TestOBSActiveReasonRequiresExactImmediateWrapper(t *testing.T) {
	// active содержит допустимую верхнеуровневую предметную оболочку.
	active := core.WithFailureReason(core.ErrInUse, core.FailureReason{Code: core.FailureSessionActive})
	// scenario описывает форму ошибки и независимый ожидаемый допуск.
	for _, scenario := range []struct {
		// name обозначает форму диагностического дерева; failure содержит проверяемую ошибку.
		name    string
		failure error
		// allowed разрешает только две явно установленные формы отказа.
		allowed bool
	}{
		{"bare", core.ErrInUse, true},
		{"exact reason", active, true},
		{"nil", nil, false},
		{"generic wrapper", fmt.Errorf("boundary: %w", core.ErrInUse), false},
		{"wrapper around reason", fmt.Errorf("boundary: %w", active), false},
		{"joined sentinel", errors.Join(core.ErrInUse), false},
		{"joined reason", errors.Join(active), false},
		{"reason around join", core.WithFailureReason(errors.Join(core.ErrInUse), core.FailureReason{Code: core.FailureSessionActive}), false},
		{"reason around wrapper", core.WithFailureReason(fmt.Errorf("boundary: %w", core.ErrInUse), core.FailureReason{Code: core.FailureSessionActive}), false},
		{"other reason", core.WithFailureReason(core.ErrInUse, core.FailureReason{Code: core.FailureResourceInUse}), false},
		{"wrong cause", core.WithFailureReason(core.ErrBusy, core.FailureReason{Code: core.FailureSessionActive}), false},
	} {
		// Подтест проверяет допустимость конкретной формы ошибки scenario для повторного выбора Session.
		t.Run(scenario.name, func(t *testing.T) {
			// got содержит решение узкого допуска для полной формы исходной ошибки.
			if got := obsSessionActive(scenario.failure); got != scenario.allowed {
				t.Fatalf("OBS допуск=%v вместо %v для %v", got, scenario.allowed, scenario.failure)
			}
		})
	}
}

// TestRetirementKeepsOriginalReasonUntilCompletionIsConfirmed проверяет неизвестный исход,
// известную ISSUED-квитанцию и успешный commit с ошибкой подтверждения. Ни одна ветвь не выполняет
// внешний запуск: узел создан после остановки реального диспетчера и вызывает лишь протокол БД.
func TestRetirementKeepsOriginalReasonUntilCompletionIsConfirmed(t *testing.T) {
	// scenario задаёт независимые классы неоднозначности записи результата.
	for _, scenario := range []struct {
		// name называет границу отказа; unreadable запрещает подтверждение чтением.
		name       string
		unreadable bool
		// committed обозначает запись результата до возврата ошибки хранилища.
		committed bool
	}{
		{"known issued rollback", false, false},
		{"unknown outcome", true, false},
		{"committed outcome", false, true},
	} {
		// Подтест проверяет сохранение исходной причины при выбранной неоднозначности записи scenario.
		t.Run(scenario.name, func(t *testing.T) {
			// fixture создаёт изолированную файловую SQLite и проверенные настройки.
			fixture := newManagerFixture(t, false, nil)
			// err подтверждает завершение рабочих горутин до ручного протокола retirement.
			if err := fixture.manager.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			// ctx задаёт контекст последовательных операций после присоединения диспетчера.
			ctx := context.Background()
			// accepted содержит новую команду без физического выполнения; err сообщает об отказе принятия.
			accepted, err := fixture.service.StartSession(ctx, core.StartSessionRequest{RequestID: "startup-retirement", StreamID: fixture.stream.ID})
			if err != nil {
				t.Fatal(err)
			}
			// fence содержит точную текущую попытку запуска.
			fence, err := fixture.service.Claim(ctx, accepted.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if err = fixture.service.MarkIssued(ctx, fence); err != nil {
				t.Fatal(err)
			}
			// fault сохраняет настоящие данные и управляет лишь доступностью подтверждения.
			fault := &reasonRetirementStore{Store: fixture.store, rejectWrite: !scenario.committed, rejectRead: scenario.unreadable, ambiguousCommit: scenario.committed}
			// service использует тот же incarnation, но получает управляемый отказ записи.
			service, err := core.NewService(fault, fixture.store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			// manager не запускает workers и используется только для контролируемого retirement.
			manager := &Manager{config: fixture.config, sessions: make(map[core.ID]*sessionNode), wake: make(chan struct{}, 1)}
			manager.config.Core = service
			// reason и cause сохраняют доказанную границу запуска и полную закрытую диагностику.
			reason := core.FailureReason{Code: core.FailureSessionStartFailed, Context: core.FailureContext{SessionID: accepted.SessionID, CommandID: accepted.ID}}
			cause := core.WithFailureReason(errors.New("synthetic-native-start-failure"), reason)
			// node уже физически очищен, но обязан остаться в Manager до подтверждённого результата.
			node := &sessionNode{manager: manager, id: accepted.SessionID, streamID: fixture.stream.ID, fence: fence, startReceipt: accepted, retirementNeeded: true, retirementFailure: cause, joined: true}
			manager.sessions[node.id] = node
			err = node.retireExecution(ctx)
			if !scenario.committed {
				if err == nil || !node.retirementNeeded || node.retirementFailure != cause || manager.sessions[node.id] != node {
					t.Fatal("неподтверждённый результат потерял продолжение", err, node.retirementNeeded)
				}
				// pending содержит реальный результат после отмены записи, читаемый в обход fault.
				pending, readErr := fixture.service.GetReceipt(ctx, accepted.ID)
				if readErr != nil || pending.Status != core.CommandIssued || pending.FailureReason != nil {
					t.Fatal("компенсация прервала неподтверждённую попытку", pending, readErr)
				}
				fault.rejectWrite, fault.rejectRead = false, false
				if err = node.retireExecution(ctx); err != nil {
					t.Fatal("повтор не сохранил первоначальный отказ", err)
				}
			} else if err != nil {
				t.Fatal("подтверждённый commit не был распознан", err)
			}
			// completed подтверждает первоначальную причину, а не синтетическую причину компенсации.
			completed, err := fixture.service.GetReceipt(ctx, accepted.ID)
			if err != nil || completed.Status != core.CommandFailed || completed.Failure != cause.Error() || !reflect.DeepEqual(completed.FailureReason, &reason) || node.retirementNeeded || manager.sessions[node.id] != nil {
				t.Fatal("retirement потерял исходную причину", completed, err)
			}
		})
	}
}

// TestRetirementDoesNotReplaceSuccessStopOrAnotherFence проверяет поздний отказ запуска после
// принятого успеха, пользовательской остановки и смены точной попытки. Нулевая попытка не захватывается.
func TestRetirementDoesNotReplaceSuccessStopOrAnotherFence(t *testing.T) {
	// scenario выбирает независимый приоритет уже принятого управления.
	for _, scenario := range []string{"success", "user stop", "another fence", "attempt zero"} {
		// Подтест проверяет приоритет уже принятого управления scenario перед поздним отказом.
		t.Run(scenario, func(t *testing.T) {
			// fixture предоставляет настоящую БД; её диспетчер закрывается до ручного управления.
			fixture := newManagerFixture(t, false, nil)
			// err подтверждает завершение рабочих горутин до ручного протокола retirement.
			if err := fixture.manager.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			// ctx задаёт контекст последовательного проверочного протокола без медиапроцессов.
			ctx := context.Background()
			// accepted и err содержат принятие команды, ещё не исполнявшей внешних действий.
			accepted, err := fixture.service.StartSession(ctx, core.StartSessionRequest{RequestID: "late-start-result", StreamID: fixture.stream.ID})
			if err != nil {
				t.Fatal(err)
			}
			// fence остаётся нулевой попыткой либо назначается единственным настоящим Claim.
			fence := receiptFence(accepted)
			if scenario != "attempt zero" {
				fence, err = fixture.service.Claim(ctx, accepted.RequestID)
				if err != nil {
					t.Fatal(err)
				}
				if err = fixture.service.MarkIssued(ctx, fence); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "success" {
				if err = fixture.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "user stop" {
				if _, err = fixture.service.StopSession(ctx, core.SessionCommand{RequestID: "user-stop", SessionID: accepted.SessionID}); err != nil {
					t.Fatal(err)
				}
			}
			// before сохраняет подтверждённый результат до позднего callback.
			before, err := fixture.service.GetReceipt(ctx, accepted.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "another fence" {
				fence.Attempt++
			}
			// manager не имеет рабочих горутин; node представляет уже очищенное старое исполнение.
			manager := &Manager{config: fixture.config, sessions: make(map[core.ID]*sessionNode), wake: make(chan struct{}, 1)}
			// cause содержит поздний типизированный отказ, который не может заменить принятый результат.
			cause := core.WithFailureReason(errors.New("late-startup-diagnostic"), core.FailureReason{Code: core.FailureSessionStartFailed})
			// node сохраняет точную локальную попытку и исходную квитанцию без нового Claim.
			node := &sessionNode{manager: manager, id: accepted.SessionID, streamID: fixture.stream.ID, fence: fence, startReceipt: accepted, retirementNeeded: true, retirementFailure: cause, joined: true}
			manager.sessions[node.id] = node
			if err = node.retireExecution(ctx); err != nil {
				t.Fatal(err)
			}
			// after содержит исходную квитанцию после точной компенсации либо no-op.
			after, err := fixture.service.GetReceipt(ctx, accepted.ID)
			if err != nil || node.retirementNeeded || manager.sessions[node.id] != nil {
				t.Fatal("подтверждённое управление не завершило retirement", after, err)
			}
			if scenario == "attempt zero" {
				if after.Attempt != 0 || after.FailureReason != nil && after.FailureReason.Code == core.FailureSessionStartFailed {
					t.Fatal("нулевая попытка стала новым исполнением запуска", after)
				}
			} else if !reflect.DeepEqual(before, after) {
				t.Fatalf("поздний отказ переписал %s: before=%+v after=%+v", scenario, before, after)
			}
		})
	}
}
