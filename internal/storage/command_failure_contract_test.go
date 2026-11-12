package storage

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Читает числа строк управляющих таблиц для проверки атомарности отклонённой команды.
func controlTableCounts(t *testing.T, store *Store) map[string]int {
	t.Helper()
	// counts сохраняет число строк каждой управляющей таблицы перед отклоняемой операцией.
	counts := make(map[string]int)
	// table называет управляющую таблицу, чьи строки проверяются на отсутствие частичной записи.
	for _, table := range []string{"sessions", "outputs", "command_receipts", "events", "session_holds", "output_holds", "cleanup_intents"} {
		// count принимает число строк проверяемой управляющей таблицы.
		var count int
		// err сообщает об ошибке чтения контрольных данных из SQLite.
		if err := store.sqlDB.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts[table] = count
	}
	return counts
}

// Проверяет необходимость Issued для успеха и причины для отказа без частичного изменения управления,
// квитанции и удержаний.
func TestCompletionRequiresIssuanceAndFailureReasonWithoutPartialWrites(t *testing.T) {
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
	// session содержит сохраняемый запуск для проверки состояния и владения.
	session := readSession(t, f.store, start.SessionID)
	// outputs содержит все сохраняемые выходы проверяемой сессии.
	outputs := readOutputs(t, f.store, start.SessionID)
	// receipt содержит сохраняемую квитанцию выбранной команды.
	receipt := readReceipt(t, f.store, start.RequestID)
	// counts сохраняет число строк каждой управляющей таблицы перед отклоняемой операцией.
	counts := controlTableCounts(t, f.store)
	// result содержит недопустимый результат завершения и ожидаемую доменную ошибку.
	for _, result := range []struct {
		// completion задаёт проверяемый результат исполнения команды.
		completion core.Completion
		// err задаёт ожидаемую ошибку недопустимого завершения.
		err error
	}{{core.Completion{Success: true}, core.ErrStale}, {core.Completion{}, core.ErrInvalid}} {
		requireDomainError(t, f.service.Complete(ctx, fence, result.completion), result.err)
		if !reflect.DeepEqual(session, readSession(t, f.store, start.SessionID)) || !reflect.DeepEqual(outputs, readOutputs(t, f.store, start.SessionID)) || !reflect.DeepEqual(receipt, readReceipt(t, f.store, start.RequestID)) || !reflect.DeepEqual(counts, controlTableCounts(t, f.store)) {
			t.Fatal("отклонённое завершение изменило принятую работу")
		}
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, true)
	requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
	// err сообщает об ошибке выдачи точной попытки команды.
	if err := f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	receipt = readReceipt(t, f.store, start.RequestID)
	session = readSession(t, f.store, start.SessionID)
	counts = controlTableCounts(t, f.store)
	requireDomainError(t, f.service.Complete(ctx, fence, core.Completion{}), core.ErrInvalid)
	if !reflect.DeepEqual(receipt, readReceipt(t, f.store, start.RequestID)) || !reflect.DeepEqual(session, readSession(t, f.store, start.SessionID)) || !reflect.DeepEqual(outputs, readOutputs(t, f.store, start.SessionID)) || !reflect.DeepEqual(counts, controlTableCounts(t, f.store)) {
		t.Fatal("отказ без причины завершил выданную команду")
	}
	// err сообщает об ошибке сохранения результата точной команды.
	if err := f.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
}

// Проверяет распространение общего отказа Stop на все ожидающие квитанции с сохранением точной очистки
// и удержаний до её подтверждения.
func TestFailedSessionStopSettlesEveryPendingStopAndRetainsCleanupHolds(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// output содержит сохраняемый выход с самостоятельным снимком и намерением.
	output := readOutputs(t, f.store, start.SessionID)[0]
	executeCommand(t, f.service, outputStartReceipt(t, f, output.ID).RequestID)
	// stops хранит три квитанции остановки одной сессии.
	stops := make([]core.CommandReceipt, 3)
	// i указывает позицию создаваемой квитанции остановки.
	for i := range stops {
		// err сообщает об ошибке принятия очередной остановки сессии.
		var err error
		stops[i], err = f.service.StopSession(ctx, core.SessionCommand{RequestID: fmt.Sprintf("stop-request-%d", i), SessionID: start.SessionID})
		if err != nil {
			t.Fatal(err)
		}
	}
	// cleanups содержит сохраняемые намерения физической очистки после проверяемой операции.
	cleanups := pendingCleanups(t, f.store, start.SessionID)
	// fence идентифицирует точную захваченную попытку проверяемой команды. err сообщает об ошибке
	// захвата точной команды исполнения.
	fence, err := f.service.Claim(ctx, stops[1].RequestID)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке выдачи точной попытки команды.
	if err := f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// reason задаёт сохраняемую причину отказа остановки.
	const reason = "процесс не подтвердил остановку"
	// err сообщает об ошибке сохранения результата точной команды.
	if err := f.service.Complete(ctx, fence, core.Completion{Failure: reason}); err != nil {
		t.Fatal(err)
	}
	// session содержит сохраняемый запуск для проверки состояния и владения.
	session := readSession(t, f.store, start.SessionID)
	if session.State != core.SessionFailed || session.Failure != reason || session.EndedAt == nil || !session.CleanupPending {
		t.Fatalf("отказ stop потерян: %+v", session)
	}
	// stop содержит очередную квитанцию остановки, которая должна получить общий отказ.
	for _, stop := range stops {
		// got содержит прочитанное сохраняемое значение для проверки результата операции.
		got := readReceipt(t, f.store, stop.RequestID)
		if got.Status != core.CommandFailed || got.Failure != reason {
			t.Fatalf("результат stop не распространился на %s: %+v", stop.RequestID, got)
		}
		// replayed содержит квитанцию тождественного повторного запроса. err сообщает об ошибке
		// принятия остановки сессии.
		replayed, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: stop.RequestID, SessionID: start.SessionID})
		if err != nil || !reflect.DeepEqual(got, replayed) {
			t.Fatalf("повтор stop потерял отказ: %+v, %v", replayed, err)
		}
	}
	if !reflect.DeepEqual(cleanups, pendingCleanups(t, f.store, start.SessionID)) || len(cleanups) != 2 {
		t.Fatal("отказ stop освободил или продублировал cleanup")
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, true)
	requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
	// intent содержит точное прежнее обязательство очистки, подтверждаемое после нового запуска.
	for _, intent := range cleanups {
		// err сообщает об ошибке подтверждения точного намерения очистки.
		if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(intent)); err != nil {
			t.Fatal(err)
		}
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, false)
}

// Проверяет полный откат Session, выходов, квитанций, событий и удержаний при поздней ошибке события
// принятия и успешный повтор после снятия отказа.
func TestStartSessionLateAcceptanceFailureRollsBackAllControlState(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 2)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// err сообщает об ошибке установки триггера отказа принятия сессии.
	_, err := f.store.sqlDB.ExecContext(ctx, `CREATE TRIGGER reject_session_acceptance BEFORE INSERT ON events WHEN NEW.output_id IS NULL BEGIN SELECT RAISE(ABORT, 'synthetic acceptance failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.StartSession(ctx, f.startRequest("atomic-start"))
	if err == nil {
		t.Fatal("инъекция отказа не отклонила принятие")
	}
	// table называет управляющую таблицу, чьи строки проверяются на отсутствие частичной записи.
	for _, table := range []string{"sessions", "outputs", "command_receipts", "events", "session_holds", "output_holds", "cleanup_intents"} {
		// count принимает число строк проверяемой управляющей таблицы.
		var count int
		// err сообщает об ошибке чтения контрольных данных из SQLite.
		if err := f.store.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("отказ принятия оставил %s: %d, %v", table, count, err)
		}
	}
	_, err = f.service.FindReceipt(ctx, "atomic-start")
	requireDomainError(t, err, core.ErrNotFound)
	// err сообщает об ошибке установки или снятия искусственного отказа SQLite.
	if _, err := f.store.sqlDB.ExecContext(ctx, "DROP TRIGGER reject_session_acceptance"); err != nil {
		t.Fatal(err)
	}
	// accepted содержит квитанцию успешно принятого запуска после проверки или снятия отказа.
	accepted, err := f.service.StartSession(ctx, f.startRequest("atomic-start"))
	if err != nil || len(readOutputs(t, f.store, accepted.SessionID)) != 2 {
		t.Fatalf("откат не позволил повторить исходный запрос: %+v, %v", accepted, err)
	}
}

// Проверяет отказ неизвестного назначения без частичных управляющих записей и запуск только явно
// выбранного допустимого подмножества.
func TestExplicitStartSelectionRejectsUnknownDestinationAtomically(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 2)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// request задаёт явный набор назначений запуска.
	request := f.startRequest("selected-start")
	request.DestinationIDs = []core.ID{f.destinations[0].ID, "missing-destination"}
	// err сообщает об ошибке принятия ручного запуска потока.
	_, err := f.service.StartSession(ctx, request)
	requireDomainError(t, err, core.ErrNotFound)
	// table называет управляющую таблицу, чьи строки проверяются на отсутствие частичной записи.
	for _, table := range []string{"sessions", "outputs", "command_receipts", "events", "session_holds", "output_holds"} {
		// count принимает число строк проверяемой управляющей таблицы.
		var count int
		// err сообщает об ошибке чтения контрольных данных из SQLite.
		if err := f.store.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("неполный выбор сохранил %s: %d, %v", table, count, err)
		}
	}
	request.DestinationIDs = []core.ID{f.destinations[1].ID}
	// accepted содержит квитанцию успешно принятого запуска после проверки или снятия отказа.
	accepted, err := f.service.StartSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// outputs содержит все сохраняемые выходы проверяемой сессии.
	outputs := readOutputs(t, f.store, accepted.SessionID)
	if len(outputs) != 1 || outputs[0].Snapshot.Destination.ID != f.destinations[1].ID {
		t.Fatalf("запущено другое подмножество назначений: %+v", outputs)
	}
}
