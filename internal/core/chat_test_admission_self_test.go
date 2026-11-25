package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestChatTestAdmissionAmbiguousStartWrite проверяет сохранение допуска только при наличии точной
// прежней квитанции запуска и изоляцию соседней ленты при неподтверждённом откате. Тест подставляет
// результат чтения после ошибки записи и проверяет доступность публикаций к моменту возврата GuardWrite.
func TestChatTestAdmissionAmbiguousStartWrite(t *testing.T) {
	// commitErr обозначает неоднозначный результат COMMIT, который должен получить вызывающий.
	commitErr := errors.New("результат COMMIT неизвестен")
	// scenarios сопоставляет доступные после ошибки квитанции с ожидаемой судьбой предпросмотра.
	scenarios := []struct {
		// name называет состояние хранилища после неоднозначной записи.
		name string
		// status и revision задают прочитанную квитанцию. Пустой status означает отсутствие записи.
		status   CommandStatus
		revision uint64
		// readUnavailable моделирует отказ согласованного чтения квитанции.
		readUnavailable bool
		// revoked указывает, теряет ли допуск право публикации после проверки.
		revoked bool
	}{
		{name: "прежняя выданная квитанция", status: CommandIssued, revision: 3},
		{name: "успешная квитанция", status: CommandSucceeded, revision: 4, revoked: true},
		{name: "выданная квитанция другой ревизии", status: CommandIssued, revision: 4, revoked: true},
		{name: "нет квитанции", revoked: true},
		{name: "чтение недоступно", status: CommandIssued, revision: 3, readUnavailable: true, revoked: true},
	}
	// scenario передаёт репозиторию состояние квитанции и задаёт ожидаемый результат проверки.
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// t проверяет одну комбинацию ответа хранилища и судьбы двух допусков.
			// base предоставляет контролируемые квитанции, fence адресует попытку запуска.
			_, base, fence, _ := newObservationAcceptanceService(t, acceptanceObservationLimits())
			// authority хранит два выданных допуска, target относится к изменяемому потоку,
			// neighbor моделирует предпросмотр независимого потока.
			authority := base.authority
			target := &ChatTestAdmission{authority: authority, streamID: "target-stream", done: make(chan struct{})}
			neighbor := &ChatTestAdmission{authority: authority, streamID: "neighbor-stream", done: make(chan struct{})}
			authority.chatTestAdmissions[target.streamID] = target
			authority.chatTestAdmissions[neighbor.streamID] = neighbor
			if scenario.status == "" {
				delete(base.repository.receipts, fence.CommandID)
			} else {
				// receipt представляет результат чтения по ID точной команды запуска.
				receipt := base.repository.receipts[fence.CommandID]
				receipt.Status, receipt.Revision, receipt.TargetID = scenario.status, scenario.revision, target.streamID
				base.repository.receipts[fence.CommandID] = receipt
			}
			if scenario.readUnavailable {
				base.viewErr = ErrBusy
			}
			// uow проверяет отмену контекста перед передачей настроенного ответа репозитория.
			uow := &reconcileContextUOW{observationAcceptanceUOW: base}
			// writeTarget передаёт GuardWrite точную команду и известную до записи ревизию.
			writeTarget := &chatTestWriteTarget{streamID: target.streamID, fence: fence, receiptRevision: 3}
			// ctx имитирует запрос, отменённый во время записи. cancel освобождает его контекст.
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), chatTestWriteKey{}, writeTarget))
			defer cancel()
			// err должен сохранить ошибку COMMIT независимо от полученной при сверке квитанции.
			err := authority.GuardWrite(ctx, uow, time.Second, func() (bool, error) {
				// Имитирует отмену запроса после возможного COMMIT с потерянным подтверждением.
				cancel()
				return true, commitErr
			})
			if !errors.Is(err, commitErr) {
				t.Fatalf("Сверка изменила ошибку записи: %v", err)
			}
			// changed показывает, выполнил ли ещё действующий допуск переданную публикацию.
			changed := 0
			// publish отмечает запись в памяти за пределами отменённого запроса.
			publish := func() { changed++ }
			err = target.Publish(t.Context(), publish)
			if scenario.revoked {
				requireObservationDone(t, target.Done())
				if !errors.Is(err, ErrConflict) || changed != 0 {
					t.Fatalf("Неподтверждённый откат сохранил допуск: записей %d, ошибка %v", changed, err)
				}
			} else if err != nil || changed != 1 {
				t.Fatalf("Прежняя квитанция не сохранила допуск: записей %d, ошибка %v", changed, err)
			}
			if err = neighbor.Publish(t.Context(), publish); err != nil {
				t.Fatalf("Сверка повредила допуск соседнего потока: %v", err)
			}
		})
	}
}
