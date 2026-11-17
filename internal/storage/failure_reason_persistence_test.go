package storage

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// failureReasonClaim захватывает и выдаёт принятую команду по requestID без создания внешних ресурсов.
// t сообщает отказ, fixture предоставляет настоящую SQLite, requestID адресует точную команду.
func failureReasonClaim(t *testing.T, fixture domainFixture, requestID string) core.ExecutionFence {
	t.Helper()
	// fence и err содержат результат единственного захвата команды.
	fence, err := fixture.service.Claim(context.Background(), requestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.service.MarkIssued(context.Background(), fence); err != nil {
		t.Fatal(err)
	}
	return fence
}

// TestFailureReasonSurvivesReopenReplayAndConfigurationChanges проверяет долговечность точной
// причины, самостоятельных снимков и прежней идентичности после редактирования и смены владельца.
func TestFailureReasonSurvivesReopenReplayAndConfigurationChanges(t *testing.T) {
	// fixture создаёт настоящую файловую SQLite с Goose и опубликованной заглушкой.
	fixture := newDomainFixture(t, 1)
	// diagnostics и diagnosticErr содержат фактические свойства того же движка, который выполнил Goose.
	diagnostics, diagnosticErr := fixture.store.Diagnostics(context.Background())
	if diagnosticErr != nil {
		t.Fatal(diagnosticErr)
	}
	t.Logf("SQLite %s compile_options=%v", diagnostics.SQLiteVersion, diagnostics.CompileOptions)
	// ctx ограничивает операции текущим тестом без внешних сервисов.
	ctx := context.Background()
	// accepted содержит исходную принятую квитанцию.
	accepted := startDomainSession(t, fixture)
	// snapshot сохраняет начальную конфигурацию независимо от будущего профиля.
	snapshot := readSession(t, fixture.store, accepted.SessionID).Snapshot
	// fence задаёт точную выданную попытку, которая завершается отказом.
	fence := failureReasonClaim(t, fixture, accepted.RequestID)
	// reason содержит исходные доказанные объекты шага запуска.
	reason := core.FailureReason{Code: core.FailureSessionStartFailed, Context: core.FailureContext{StreamID: fixture.stream.ID, SessionID: accepted.SessionID, CommandID: accepted.ID}}
	// wanted сохраняет неизменяемую ожидаемую причину до изменения входного указателя.
	wanted := reason
	// err содержит отказ атомарной записи результата выданного запуска.
	if err := fixture.service.Complete(ctx, fence, core.Completion{Failure: "synthetic-native-secret", FailureReason: &reason}); err != nil {
		t.Fatal(err)
	}
	reason.Code = core.FailureOutputStopFailed
	// completed и session содержат долговечные результаты обеих связанных записей.
	completed := readReceipt(t, fixture.store, accepted.RequestID)
	session := readSession(t, fixture.store, accepted.SessionID)
	if completed.Status != core.CommandFailed || session.State != core.SessionFailed || completed.Failure != "synthetic-native-secret" || session.Failure != completed.Failure || !reflect.DeepEqual(completed.FailureReason, &wanted) || !reflect.DeepEqual(session.FailureReason, &wanted) {
		t.Fatalf("причина не записана согласованно: receipt=%+v session=%+v", completed, session)
	}
	// changed заменяет текущую настройку, не имеющую права менять историческую причину или снимок.
	changed := fixture.profile
	changed.Name = "Будущая настройка"
	// err сообщает, удалось ли изменить будущую настройку без изменения истории.
	if _, err := fixture.service.UpdateProfile(ctx, changed, changed.Revision); err != nil {
		t.Fatal(err)
	}
	fixture = reopenFixture(t, fixture)
	// replayed возвращает прежний результат до новой проверки конфигурации; err сообщает об отказе повтора.
	replayed, err := fixture.service.StartSession(ctx, fixture.startRequest(accepted.RequestID))
	if err != nil || !reflect.DeepEqual(replayed, completed) {
		t.Fatalf("повтор реконструировал результат: %+v %v", replayed, err)
	}
	// got проверяет всю историческую Session после смены владельца.
	if got := readSession(t, fixture.store, accepted.SessionID); !reflect.DeepEqual(got, session) || !reflect.DeepEqual(got.Snapshot, snapshot) {
		t.Fatalf("перезапуск переписал терминальный результат: %+v", got)
	}
	if err = fixture.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	// rewriteErr содержит отказ попытки подменить уже терминальную причину через репозиторий.
	rewriteErr := fixture.store.Write(ctx, func(repositories core.Repositories) error {
		// Проверяет запрет изменения завершённого результата в обход повторного Complete.
		// rewritten сохраняет прежнюю идентичность, но подменяет только публичную причину.
		rewritten := completed
		rewritten.Revision++
		rewritten.FailureReason = &core.FailureReason{Code: core.FailureOutputStartFailed}
		return repositories.Runtime.UpdateReceipt(ctx, rewritten, completed.Revision)
	})
	if !errors.Is(rewriteErr, core.ErrConflict) {
		t.Fatal("терминальная причина переписана", rewriteErr)
	}
	// got содержит завершённый результат после повторного Recover и запрещённой подмены.
	if got := readReceipt(t, fixture.store, accepted.RequestID); !reflect.DeepEqual(got, completed) {
		t.Fatal("повтор Recover изменил завершённую причину", got)
	}
}

// TestFailureReasonCompletionRollbackAndValidationAreAtomic проверяет поздний отказ события
// после записи результата и недопустимую новую причину, включая прямой путь storage.normalize.
func TestFailureReasonCompletionRollbackAndValidationAreAtomic(t *testing.T) {
	// fixture предоставляет настоящую транзакционную границу без внешних процессов.
	fixture := newDomainFixture(t, 1)
	// ctx задаёт контекст SQLite-операций.
	ctx := context.Background()
	// accepted и fence адресуют одну точно выданную попытку запуска.
	accepted := startDomainSession(t, fixture)
	fence := failureReasonClaim(t, fixture, accepted.RequestID)
	// beforeSession, beforeReceipt, beforeOutputs и beforeCounts фиксируют весь затрагиваемый контроль.
	beforeSession := readSession(t, fixture.store, accepted.SessionID)
	beforeReceipt := readReceipt(t, fixture.store, accepted.RequestID)
	beforeOutputs := readOutputs(t, fixture.store, accepted.SessionID)
	beforeCounts := controlTableCounts(t, fixture.store)
	// invalid задаёт неизвестный код, который нельзя записать даже через прямой репозиторий.
	invalid := core.FailureReason{Code: core.FailureCode("synthetic-unknown-code")}
	// err проверяет отказ неизвестного кода до любых записей результата.
	if err := fixture.service.Complete(ctx, fence, core.Completion{Failure: "diagnostic", FailureReason: &invalid}); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("Complete принял неизвестную причину", err)
	}
	// err содержит отказ прямого обновления квитанции с недопустимой причиной.
	err := fixture.store.Write(ctx, func(repositories core.Repositories) error {
		// Пытается провести неизвестный код через обязательную проверку записи Receipt.
		// corrupted сохраняет все прежние поля кроме проверяемого недопустимого результата.
		corrupted := beforeReceipt
		corrupted.Status, corrupted.Failure, corrupted.FailureReason = core.CommandFailed, "diagnostic", &invalid
		corrupted.Revision++
		return repositories.Runtime.UpdateReceipt(ctx, corrupted, beforeReceipt.Revision)
	})
	if !errors.Is(err, core.ErrInvalid) {
		t.Fatal("storage.normalize пропустил неизвестную причину", err)
	}
	// reason задаёт допустимую причину, запись которой будет поздно отменена триггером.
	reason := core.FailureReason{Code: core.FailureSessionStartFailed, Context: core.FailureContext{SessionID: accepted.SessionID}}
	if err = fixture.service.Complete(ctx, fence, core.Completion{Success: true, FailureReason: &reason}); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("успех принят вместе с причиной отказа", err)
	}
	if _, err = fixture.store.sqlDB.ExecContext(ctx, `CREATE TRIGGER reject_reason_completion BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'synthetic completion rollback'); END`); err != nil {
		t.Fatal(err)
	}
	err = fixture.service.Complete(ctx, fence, core.Completion{Failure: "diagnostic", FailureReason: &reason})
	if err == nil {
		t.Fatal("поздняя инъекция не отменила Complete")
	}
	if !reflect.DeepEqual(beforeSession, readSession(t, fixture.store, accepted.SessionID)) || !reflect.DeepEqual(beforeReceipt, readReceipt(t, fixture.store, accepted.RequestID)) || !reflect.DeepEqual(beforeOutputs, readOutputs(t, fixture.store, accepted.SessionID)) || !reflect.DeepEqual(beforeCounts, controlTableCounts(t, fixture.store)) {
		t.Fatal("отказ оставил частичную причину, управление или очистку")
	}
	if _, err = fixture.store.sqlDB.ExecContext(ctx, "DROP TRIGGER reject_reason_completion"); err != nil {
		t.Fatal(err)
	}
	if err = fixture.service.Complete(ctx, fence, core.Completion{Failure: "diagnostic", FailureReason: &reason}); err != nil {
		t.Fatal("тот же результат нельзя сохранить после отката", err)
	}
}

// TestFailureReasonLegacyOmissionAndRecovery проверяет совместимость отсутствующего нового поля
// и собственную достоверную причину восстановления без угадывания из старой диагностики.
func TestFailureReasonLegacyOmissionAndRecovery(t *testing.T) {
	// fixture создаёт прежнюю сохраняемую форму без необязательной типизированной причины.
	fixture := newDomainFixture(t, 0)
	// accepted адресует незавершённый запуск старого открытия.
	accepted := startDomainSession(t, fixture)
	// payload содержит точные байты квитанции, ещё не имевшей публичной причины.
	var payload []byte
	// err содержит отказ чтения исходных байтов нагрузки прямо из временной SQLite.
	if err := fixture.store.sqlDB.QueryRowContext(context.Background(), "SELECT payload FROM command_receipts WHERE id = ?", accepted.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte("FailureReason")) {
		t.Fatalf("nil поле записано в legacy-нагрузку: %s", payload)
	}
	fixture = reopenFixture(t, fixture)
	// receipt и session содержат собственную доказанную причину потери прежнего владельца.
	receipt := readReceipt(t, fixture.store, accepted.RequestID)
	session := readSession(t, fixture.store, accepted.SessionID)
	if receipt.Status != core.CommandInterrupted || session.State != core.SessionInterrupted || receipt.FailureReason == nil || session.FailureReason == nil || receipt.FailureReason.Code != core.FailureExecutionInterrupted || session.FailureReason.Code != core.FailureExecutionInterrupted {
		t.Fatalf("восстановление не сохранило причину: %+v %+v", receipt, session)
	}
}

// TestFailureReasonFailedStopSharesOneResultAndPreservesCleanup проверяет общую причину нескольких
// stop-квитанций, исходную успешную квитанцию и обязательства точной физической очистки.
func TestFailureReasonFailedStopSharesOneResultAndPreservesCleanup(t *testing.T) {
	// fixture предоставляет запуск с выданными эффектами и одной группой.
	fixture := newDomainFixture(t, 1)
	// ctx ограничивает доменные операции текущим тестом.
	ctx := context.Background()
	// start содержит исходный запуск, а succeeded — его терминальную успешную квитанцию.
	start := startDomainSession(t, fixture)
	executeCommand(t, fixture.service, start.RequestID)
	succeeded := readReceipt(t, fixture.store, start.RequestID)
	// first и second представляют разные принятые намерения остановки одного запуска;
	// err сообщает об отказе их принятия.
	first, err := fixture.service.StopSession(ctx, core.SessionCommand{RequestID: "first-stop", SessionID: start.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.service.StopSession(ctx, core.SessionCommand{RequestID: "second-stop", SessionID: start.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	// cleanups сохраняет точные обязательства, которые отказ результата не освобождает.
	cleanups := pendingCleanups(t, fixture.store, start.SessionID)
	// fence задаёт единственное исполнение общей остановки.
	fence := failureReasonClaim(t, fixture, second.RequestID)
	// reason содержит доказанный шаг остановки, без предположений о нативной причине.
	reason := core.FailureReason{Code: core.FailureSessionStopFailed, Context: core.FailureContext{SessionID: start.SessionID}}
	if err = fixture.service.Complete(ctx, fence, core.Completion{Failure: "synthetic-stop-diagnostic", FailureReason: &reason}); err != nil {
		t.Fatal(err)
	}
	// stop содержит очередную квитанцию, которая должна получить тот же общий результат.
	for _, stop := range []core.CommandReceipt{first, second} {
		// got содержит долговечный результат каждого намерения остановки.
		got := readReceipt(t, fixture.store, stop.RequestID)
		if got.Status != core.CommandFailed || got.Failure != "synthetic-stop-diagnostic" || !reflect.DeepEqual(got.FailureReason, &reason) {
			t.Fatal("общая остановка потеряла причину", got)
		}
	}
	// got содержит итоговую Session после общей неуспешной остановки.
	if got := readSession(t, fixture.store, start.SessionID); got.State != core.SessionFailed || !reflect.DeepEqual(got.FailureReason, &reason) {
		t.Fatal("сессия потеряла общую причину остановки", got)
	}
	if !reflect.DeepEqual(cleanups, pendingCleanups(t, fixture.store, start.SessionID)) || !reflect.DeepEqual(succeeded, readReceipt(t, fixture.store, start.RequestID)) {
		t.Fatal("результат остановки изменил прежний успех или очистку")
	}
}
