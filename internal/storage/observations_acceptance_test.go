package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Копирует строки всех проверяемых долговечных таблиц в порядке rowid, переводя байтовые нагрузки в
// строки для точного сравнения.
func observationRows(t *testing.T, db *sql.DB) map[string][][]any {
	t.Helper()
	// result собирает полные строки долговечных таблиц для точного сравнения.
	result := map[string][][]any{}
	// table называет очередную долговечную таблицу проверяемого управления.
	for _, table := range []string{"fallback_assets", "streams", "profiles", "groups", "destinations", "fallback_variants", "sessions", "outputs", "command_receipts", "cleanup_intents", "owned_paths", "session_holds", "output_holds", "events"} {
		// rows предоставляет все строки таблицы в порядке rowid. err сообщает об ошибке чтения
		// всех колонок проверяемой долговечной таблицы.
		rows, err := db.QueryContext(context.Background(), "SELECT * FROM "+table+" ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		// columns содержит имена колонок читаемой долговечной таблицы.
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			// values принимает полные значения колонок очередной строки. targets содержит
			// адреса значений колонок для SQL Scan.
			values, targets := make([]any, len(columns)), make([]any, len(columns))
			// i указывает позицию колонки либо номер очередного обновления наблюдений.
			for i := range values {
				targets[i] = &values[i]
			}
			// err сообщает об ошибке чтения очередной полной SQL-строки.
			if err := rows.Scan(targets...); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			// i указывает позицию колонки либо номер очередного обновления наблюдений. value
			// содержит очередное значение колонки для нормализации байтовой нагрузки.
			for i, value := range values {
				// b содержит байтовую SQL-нагрузку, преобразуемую в строку для точного
				// сравнения. ok показывает, что значение колонки является байтовой
				// нагрузкой.
				if b, ok := value.([]byte); ok {
					values[i] = string(b)
				}
			}
			result[table] = append(result[table], values)
		}
		// rowErr содержит итог обхода всех SQL-строк. closeErr содержит результат закрытия
		// SQL-строк.
		rowErr, closeErr := rows.Err(), rows.Close()
		if rowErr != nil || closeErr != nil {
			t.Fatal(rowErr, closeErr)
		}
	}
	return result
}

// Успешно завершает тестовый запуск Session и всех первоначальных выходов, возвращая их точные
// ExecutionFence.
func startObservationOutputs(t *testing.T, f domainFixture) (core.ExecutionFence, []core.ExecutionFence) {
	t.Helper()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// parent содержит точную выданную попытку родительской сессии.
	parent := executeCommand(t, f.service, start.RequestID)
	// outputs содержит точные выданные попытки первоначальных выходов.
	var outputs []core.ExecutionFence
	// o содержит очередной сохраняемый выход для исполнения его команды запуска.
	for _, o := range readOutputs(t, f.store, start.SessionID) {
		outputs = append(outputs, executeCommand(t, f.service, outputStartReceipt(t, f, o.ID).RequestID))
	}
	return parent, outputs
}

// Извлекает точный RuntimeSessionFence из исполнения Session либо её выхода.
func observationFence(f core.ExecutionFence) core.RuntimeSessionFence {
	return core.RuntimeSessionFence{SessionID: f.SessionID, Incarnation: f.Incarnation, Lifecycle: f.Lifecycle}
}

// Проверяет неизменность всех долговечных строк при обычных наблюдениях, отсутствие оперативных полей
// Outputs в SQL и отзыв всех RAM-владельцев при Close.
func TestAcceptanceOrdinaryObservationsLeaveEveryDurableRowUnchanged(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 2)
	// parent содержит точную выданную попытку родительской сессии. outputs содержит точные выданные
	// попытки первоначальных выходов.
	parent, outputs := startObservationOutputs(t, f)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// err сообщает об ошибке чтения текущего согласованного снимка RAM.
	if _, err := f.service.RuntimeSnapshot(ctx, observationFence(parent)); !errors.Is(err, core.ErrNotReady) {
		t.Fatal("Complete выдумал RAM-наблюдение", err)
	}
	// so содержит наблюдателя текущей сессии. err сообщает об ошибке регистрации наблюдателя
	// выданной сессии.
	so, err := f.service.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	// g содержит наблюдателя группы текущего исполнения.
	g, err := so.OpenGroup(ctx, "dynamic-group-outside-start-snapshot", f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	// w содержит владельца наблюдений FFmpeg в текущей группе.
	w, err := g.OpenFFmpeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// a содержит наблюдателя точной попытки FFmpeg.
	a, err := w.BeginAttempt(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	// observers хранит независимых наблюдателей двух текущих выходов.
	var observers []*core.OutputObserver
	// fence идентифицирует точную захваченную попытку проверяемой команды.
	for _, fence := range outputs {
		// o содержит наблюдателя проверяемого выхода. err сообщает об ошибке регистрации
		// наблюдателя точной попытки выхода.
		o, err := f.service.OpenOutputObserver(ctx, fence)
		if err != nil {
			t.Fatal(err)
		}
		observers = append(observers, o)
	}
	// before сохраняет все долговечные строки до проверяемой операции.
	before := observationRows(t, f.store.sqlDB)
	// i указывает позицию колонки либо номер очередного обновления наблюдений.
	for i := range 3 {
		// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
		if err := so.Observe(ctx, core.SessionObservation{State: core.ObservationReady, Error: "временная ошибка Session"}); err != nil {
			t.Fatal(err)
		}
		// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
		if err := g.Observe(ctx, core.GroupObservation{State: core.ObservationReady, Source: core.ObservationSourceLive, Error: "временная ошибка Group"}); err != nil {
			t.Fatal(err)
		}
		// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
		if err := a.Observe(ctx, core.FFmpegObservation{State: core.ObservationRetrying, Error: "временная ошибка worker"}); err != nil {
			t.Fatal(err)
		}
		// o содержит наблюдателя проверяемого выхода.
		for _, o := range observers {
			// state задаёт наблюдаемое состояние выхода при очередном обновлении.
			state := core.OutputRetrying
			if i == 2 {
				state = core.OutputPublishing
			}
			// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
			if err := o.Observe(ctx, core.OutputObservation{State: state, Error: "временная ошибка соединения"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// after содержит долговечные строки после обычных наблюдений.
	after := observationRows(t, f.store.sqlDB)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("обычные наблюдения изменили долговечные строки, payload, revisions, events, receipts или holds")
	}
	// snapshot содержит согласованный RAM-снимок обычных наблюдений.
	snapshot, err := f.service.RuntimeSnapshot(ctx, observationFence(parent))
	if err != nil || snapshot.Session.Value.Error != "временная ошибка Session" || len(snapshot.Outputs) != 2 || snapshot.Outputs[0].Value.State != core.OutputPublishing || snapshot.FFmpeg[0].Stamp.Revision != 3 {
		t.Fatalf("нет текущего RAM-снимка: %+v %v", snapshot, err)
	}
	if readSession(t, f.store, parent.SessionID).Failure != "" {
		t.Fatal("текущая ошибка попала в итоговый Session.Failure")
	}
	// payload принимает сохраняемый JSON выхода для проверки отсутствия оперативных полей.
	var payload []byte
	// err сообщает об ошибке чтения очередной полной SQL-строки.
	if err := f.store.sqlDB.QueryRowContext(ctx, "SELECT payload FROM outputs WHERE id=?", outputs[0].OutputID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	// envelope принимает оболочку сохраняемой нагрузки выхода.
	var envelope struct { /* Payload содержит поля сохраняемой нагрузки выхода для проверки отсутствия State и Failure. */
		Payload map[string]json.RawMessage
	}
	// err сообщает об ошибке разбора сохраняемого payload выхода.
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	// field называет оперативное поле, которого не должно быть в сохраняемом payload.
	for _, field := range []string{"State", "Failure"} {
		// exists показывает присутствие оперативного поля в сохраняемой нагрузке.
		if _, exists := envelope.Payload[field]; exists {
			t.Errorf("оперативное поле %s осталось в durable payload", field)
		}
	}
	// err сообщает об ошибке проверки недопустимого SQL-фильтра по оперативному состоянию выхода.
	if err := f.store.View(ctx, func(r core.Repositories) error {
		// Пробует использовать наблюдаемое PUBLISHING как SQL-фильтр сохранённых выходов для проверки
		// ErrInvalid.

		// err сообщает об ошибке чтения сохраняемых выходов сессии.
		_, err := r.Runtime.Outputs(ctx, core.ListQuery{Limit: 10, SessionID: parent.SessionID, State: string(core.OutputPublishing)})
		return err
	}); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("наблюдаемое состояние принято как durable SQL-фильтр", err)
	}
	// err сообщает об ошибке закрытия временного хранилища или контрольного SQL-пула.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	if so.Valid() || g.Valid() || w.Valid() || a.Valid() || observers[0].Valid() || observers[1].Valid() {
		t.Fatal("Store.Close оставил оперативных владельцев")
	}
	// err сообщает об ошибке чтения текущего согласованного снимка RAM.
	if _, err := f.service.RuntimeSnapshot(ctx, observationFence(parent)); !errors.Is(err, core.ErrClosed) {
		t.Fatal("закрытое хранилище вернуло прежнюю RAM", err)
	}
}

// Проверяет полный откат FailOutput при ошибке записи события и точный последующий отказ с очисткой,
// сохраняя успешную квитанцию и независимых владельцев.
func TestAcceptanceOutputFailureRollsBackAtomicallyAndPreservesSuccessfulReceipt(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 2)
	// parent содержит точную выданную попытку родительской сессии. outputs содержит точные выданные
	// попытки первоначальных выходов.
	parent, outputs := startObservationOutputs(t, f)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// so содержит наблюдателя текущей сессии. err сообщает об ошибке регистрации наблюдателя
	// выданной сессии.
	so, err := f.service.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	// g содержит наблюдателя группы текущего исполнения.
	g, err := so.OpenGroup(ctx, f.group.ID, f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	// w содержит владельца наблюдений FFmpeg в текущей группе.
	w, err := g.OpenFFmpeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// o содержит наблюдателя проверяемого выхода.
	o, err := f.service.OpenOutputObserver(ctx, outputs[0])
	if err != nil {
		t.Fatal(err)
	}
	// neighbor содержит наблюдателя здорового соседнего выхода.
	neighbor, err := f.service.OpenOutputObserver(ctx, outputs[1])
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
	if err := o.Observe(ctx, core.OutputObservation{State: core.OutputPublishing}); err != nil {
		t.Fatal(err)
	}
	// publication содержит допуск заменяемого источника OBS.
	publication := activateRuntimeSource(t, f)
	// secondPublication содержит допуск нового источника OBS, независимый от наблюдателей.
	secondPublication := activateRuntimeSource(t, f)
	if publication.Valid() || !secondPublication.Valid() || !so.Valid() || !g.Valid() || !w.Valid() {
		t.Fatal("OBS Override отозвал независимые observations")
	}
	// before сохраняет все долговечные строки до проверяемой операции.
	before := observationRows(t, f.store.sqlDB)
	// beforeRAM сохраняет текущие наблюдения перед искусственным отказом события.
	beforeRAM, err := f.service.RuntimeSnapshot(ctx, observationFence(parent))
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке изменения изолированной SQLite для проверки отказа.
	if _, err := f.store.sqlDB.ExecContext(ctx, "CREATE TRIGGER reject_failure_event BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'synthetic failure event rollback'); END"); err != nil {
		t.Fatal(err)
	}
	// reason задаёт полную сохраняемую причину окончательного отказа выхода.
	reason := "Окончательный отказ назначения: " + strings.Repeat("подробности;", 64)
	// err сообщает об ошибке фиксации окончательного отказа точного выхода.
	if err := f.service.FailOutput(ctx, o, reason); err == nil {
		t.Fatal("отказ INSERT Event выдан за успешный FailOutput")
	}
	// got содержит прочитанное состояние для проверки неизменности либо сохранённого результата.
	if got := observationRows(t, f.store.sqlDB); !reflect.DeepEqual(before, got) {
		t.Fatal("rollback Event оставил частичное управление или cleanup")
	}
	// afterRAM содержит RAM-снимок после откатившегося отказа выхода.
	afterRAM, err := f.service.RuntimeSnapshot(ctx, observationFence(parent))
	if err != nil || !reflect.DeepEqual(beforeRAM, afterRAM) || !o.Valid() {
		t.Fatal("rollback уничтожил прежнюю RAM", err)
	}
	// err сообщает об ошибке изменения изолированной SQLite для проверки отказа.
	if _, err := f.store.sqlDB.ExecContext(ctx, "DROP TRIGGER reject_failure_event"); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке фиксации окончательного отказа точного выхода.
	if err := f.service.FailOutput(ctx, o, reason); err != nil {
		t.Fatal(err)
	}
	if o.Valid() || !neighbor.Valid() || !so.Valid() || !g.Valid() || !w.Valid() || !secondPublication.Valid() {
		t.Fatal("отказ выхода нарушил независимость владельцев")
	}
	// got содержит прочитанное состояние для проверки неизменности либо сохранённого результата.
	if got := observationRows(t, f.store.sqlDB)["command_receipts"]; !reflect.DeepEqual(before["command_receipts"], got) {
		t.Fatal("runtime failure переписал успешную start receipt")
	}
	// current содержит сохраняемое управление отказавшего выхода.
	current, err := f.service.GetOutput(ctx, outputs[0].OutputID)
	if err != nil || current.DesiredRunning || !current.CleanupPending || !current.Issued || current.Generation <= outputs[0].OutputGeneration {
		t.Fatalf("нет долговечного отзыва и cleanup: %+v %v", current, err)
	}
	// pending содержит намерение очистки точной отказавшей попытки.
	if pending := pendingCleanups(t, f.store, parent.SessionID); len(pending) != 1 || pending[0].OutputID != outputs[0].OutputID || pending[0].Attempt != outputs[0].Attempt {
		t.Fatalf("cleanup не совпадает с отказавшей попыткой: %+v", pending)
	}
	// failures считает события окончательного отказа проверяемого выхода.
	failures := 0
	// err сообщает об ошибке проверки единственного события отказа с точной причиной и
	// ExecutionFence.
	if err := f.store.View(ctx, func(r core.Repositories) error {
		// Проверяет единственное событие output_execution_failed с полным ExecutionFence и причиной и
		// отсутствие Execution у обычных служебных событий.

		// events содержит историю управления и окончательного отказа сессии. err сообщает об
		// ошибке чтения сохраняемой истории событий.
		events, err := r.Runtime.Events(ctx, core.ListQuery{Limit: 100, SessionID: parent.SessionID})
		if err != nil {
			return err
		}
		// event содержит очередное событие для проверки точной причины и ExecutionFence.
		for _, event := range events {
			if event.Kind == "output_execution_failed" {
				failures++
				if event.Message != reason || event.Execution == nil || *event.Execution != outputs[0] || event.OutputID != outputs[0].OutputID || event.SessionID != parent.SessionID {
					t.Fatalf("история потеряла полный fence/причину: %+v", event)
				}
			} else if event.Execution != nil {
				t.Fatal("обычное control event получило execution fence")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if failures != 1 {
		t.Fatalf("итоговых событий %d, требуется одно", failures)
	}
	// committed сохраняет долговечные строки после успешного окончательного отказа.
	committed := observationRows(t, f.store.sqlDB)
	// err сообщает об ошибке фиксации окончательного отказа точного выхода.
	if err := f.service.FailOutput(ctx, o, "поздний отказ"); !errors.Is(err, core.ErrStale) {
		t.Fatal(err)
	}
	// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
	if err := o.Observe(ctx, core.OutputObservation{State: core.OutputPublishing}); !errors.Is(err, core.ErrStale) {
		t.Fatal(err)
	}
	// got содержит прочитанное состояние для проверки неизменности либо сохранённого результата.
	if got := observationRows(t, f.store.sqlDB); !reflect.DeepEqual(committed, got) {
		t.Fatal("поздний callback изменил долговечное управление")
	}
}

// Проверяет открытие наблюдателя только выданного запуска и его отзыв Stop до позднего Complete без
// освобождения физических удержаний.
func TestAcceptanceIssuedStartupObserverRetiresBeforeLateCompletion(t *testing.T) {
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
	// err сообщает об ошибке регистрации наблюдателя выданной сессии.
	if _, err := f.service.OpenSessionObserver(ctx, fence); !errors.Is(err, core.ErrStale) {
		t.Fatal("невыданная попытка допущена", err)
	}
	// err сообщает об ошибке выдачи точной попытки команды.
	if err := f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// o содержит наблюдателя выданного запуска Session до его отзыва остановкой.
	o, err := f.service.OpenSessionObserver(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
	if err := o.Observe(ctx, core.SessionObservation{State: core.ObservationStarting}); err != nil {
		t.Fatal(err)
	}
	// stop содержит принятую квитанцию остановки сессии.
	stop, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "stop-observed-startup", SessionID: start.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if o.Valid() {
		t.Fatal("Stop не отозвал startup observer до возврата")
	}
	// err сообщает об ошибке чтения текущего согласованного снимка RAM.
	if _, err := f.service.RuntimeSnapshot(ctx, observationFence(fence)); !errors.Is(err, core.ErrNotReady) {
		t.Fatal(err)
	}
	// err сообщает об ошибке сохранения результата точной команды.
	if err := f.service.Complete(ctx, fence, core.Completion{Success: true}); !errors.Is(err, core.ErrStale) {
		t.Fatal(err)
	}
	executeCommand(t, f.service, stop.RequestID)
	if len(pendingCleanups(t, f.store, start.SessionID)) != 1 {
		t.Fatal("отсутствие RAM подменило cleanup ack")
	}
	// err сообщает об ошибке удаления варианта заглушки.
	if err := f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision); !errors.Is(err, core.ErrInUse) {
		t.Fatal("исчезнувший observer освободил физическое удержание", err)
	}
}

// Проверяет сохранение отказа команды выхода и очистки без выдуманного RAM-наблюдения и без
// терминальной ошибки родительской Session.
func TestAcceptanceFailedOutputCompletionRecordsOnlyDurableCommandResult(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// parent содержит точную выданную попытку родительской сессии.
	parent := executeCommand(t, f.service, start.RequestID)
	// output содержит сохраняемый выход с самостоятельным снимком и намерением.
	output := readOutputs(t, f.store, parent.SessionID)[0]
	// command содержит внутреннюю квитанцию запуска выбранного выхода.
	command := outputStartReceipt(t, f, output.ID)
	// fence идентифицирует точную захваченную попытку проверяемой команды. err сообщает об ошибке
	// захвата точной команды исполнения.
	fence, err := f.service.Claim(ctx, command.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке выдачи точной попытки команды.
	if err := f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке регистрации наблюдателя точной попытки выхода.
	if _, err := f.service.OpenOutputObserver(ctx, fence); !errors.Is(err, core.ErrStale) {
		t.Fatal("ISSUED без успешного Complete выдан за работающий выход", err)
	}
	// reason задаёт полную сохраняемую причину окончательного отказа выхода.
	reason := "Не удалось открыть назначение"
	// err сообщает об ошибке сохранения результата точной команды.
	if err := f.service.Complete(ctx, fence, core.Completion{Failure: reason}); err != nil {
		t.Fatal(err)
	}
	// got содержит прочитанное состояние для проверки неизменности либо сохранённого результата.
	if got := readReceipt(t, f.store, command.RequestID); got.Status != core.CommandFailed || got.Failure != reason {
		t.Fatalf("итог команды не сохранён: %+v", got)
	}
	// err сообщает об ошибке чтения текущего согласованного снимка RAM.
	if _, err := f.service.RuntimeSnapshot(ctx, observationFence(parent)); !errors.Is(err, core.ErrNotReady) {
		t.Fatal("неуспешный Complete выдумал наблюдение", err)
	}
	// got содержит сохраняемое управление выхода после неуспешного Complete. err сообщает об ошибке
	// чтения сохраняемого снимка и намерения выхода.
	if got, err := f.service.GetOutput(ctx, output.ID); err != nil || got.DesiredRunning || !got.CleanupPending || !got.Issued {
		t.Fatalf("отказ потерял durable cleanup: %+v %v", got, err)
	}
	// got содержит родительскую сессию после отказа отдельного выхода.
	if got := readSession(t, f.store, parent.SessionID); got.State != core.SessionRunning || got.Failure != "" {
		t.Fatal("ошибка выхода стала итоговой ошибкой Session", got)
	}
}

// Проверяет повторное открытие текущей схемы с пустой RAM, прерыванием прежнего запуска и сохранением
// снимков, квитанции и точной истории отказа после удаления конфигурации.
func TestAcceptanceFreshSchemaReopenKeepsHistoryAndStartsWithEmptyRAM(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// parent содержит точную выданную попытку родительской сессии. outputs содержит точные выданные
	// попытки первоначальных выходов.
	parent, outputs := startObservationOutputs(t, f)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// o содержит наблюдателя проверяемого выхода. err сообщает об ошибке регистрации наблюдателя
	// точной попытки выхода.
	o, err := f.service.OpenOutputObserver(ctx, outputs[0])
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
	if err := o.Observe(ctx, core.OutputObservation{State: core.OutputRetrying, Error: "ошибка прежнего процесса"}); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке фиксации окончательного отказа точного выхода.
	if err := f.service.FailOutput(ctx, o, "Итоговая причина для истории"); err != nil {
		t.Fatal(err)
	}
	// beforeOutput сохраняет выход после окончательного отказа до повторного открытия.
	beforeOutput, err := f.service.GetOutput(ctx, outputs[0].OutputID)
	if err != nil {
		t.Fatal(err)
	}
	// beforeReceipt сохраняет успешную исходную квитанцию запуска перед повторным открытием.
	beforeReceipt := readReceipt(t, f.store, startRequestID(t, f.store, parent.CommandID))
	// err сообщает об ошибке повторной проверки текущей схемы миграций.
	if err := f.store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// stateColumns принимает число колонок state в таблице outputs текущей схемы.
	var stateColumns int
	// err сообщает об ошибке чтения очередной полной SQL-строки.
	if err := f.store.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('outputs') WHERE name='state'").Scan(&stateColumns); err != nil || stateColumns != 0 {
		t.Fatal("primary schema сохранила outputs.state", stateColumns, err)
	}
	f = reopenFixture(t, f)
	// err сообщает об ошибке чтения текущего согласованного снимка RAM.
	if _, err := f.service.RuntimeSnapshot(ctx, observationFence(parent)); !errors.Is(err, core.ErrNotReady) {
		t.Fatal("reopen восстановил прежнюю телеметрию", err)
	}
	// after содержит выход после повторного открытия для проверки сохранности снимка.
	after, err := f.service.GetOutput(ctx, beforeOutput.ID)
	if err != nil || !reflect.DeepEqual(beforeOutput.Snapshot, after.Snapshot) {
		t.Fatal("reopen потерял output snapshot", err)
	}
	// got содержит прочитанное состояние для проверки неизменности либо сохранённого результата.
	if got := readReceipt(t, f.store, beforeReceipt.RequestID); !reflect.DeepEqual(beforeReceipt, got) {
		t.Fatal("recovery переписал terminal receipt")
	}
	// got содержит прочитанное состояние для проверки неизменности либо сохранённого результата.
	if got := readSession(t, f.store, parent.SessionID); got.State != core.SessionInterrupted || !got.CleanupPending {
		t.Fatalf("recovery потерял протокол управления: %+v", got)
	}
	if len(pendingCleanups(t, f.store, parent.SessionID)) != 2 {
		t.Fatal("reopen потерял parent/child cleanup")
	}
	// cleanup содержит очередное точное намерение прежней очистки для подтверждения.
	for _, cleanup := range pendingCleanups(t, f.store, parent.SessionID) {
		// err сообщает об ошибке подтверждения точного намерения очистки.
		if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanup)); err != nil {
			t.Fatal(err)
		}
	}
	// err сообщает об ошибке удаления больше не удерживаемого назначения.
	if err := f.service.DeleteDestination(ctx, f.destinations[0].ID, f.destinations[0].Revision); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке удаления больше не удерживаемой группы.
	if err := f.service.DeleteGroup(ctx, f.group.ID, f.group.Revision); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке удаления профиля при сохраняемых ссылках.
	if err := f.service.DeleteProfile(ctx, f.profile.ID, f.profile.Revision); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке поиска сохранённого отказа после повторного открытия и удаления
	// конфигурации.
	if err := f.store.View(ctx, func(r core.Repositories) error {
		// После повторного открытия и удаления текущих настроек ищет сохранённый окончательный отказ с точным
		// ExecutionFence и причиной.

		// events содержит историю управления и окончательного отказа сессии. err сообщает об
		// ошибке чтения сохраняемой истории событий.
		events, err := r.Runtime.Events(ctx, core.ListQuery{Limit: 100, SessionID: parent.SessionID})
		if err != nil {
			return err
		}
		// event содержит очередное событие для проверки точной причины и ExecutionFence.
		for _, event := range events {
			if event.Kind == "output_execution_failed" && event.Message == "Итоговая причина для истории" && event.Execution != nil && *event.Execution == outputs[0] {
				return nil
			}
		}
		return fmt.Errorf("после hard delete потеряна история отказа")
	}); err != nil {
		t.Fatal(err)
	}
}

// Читает исходный RequestID тестовой квитанции по её внутреннему ID непосредственно из SQLite.
func startRequestID(t *testing.T, s *Store, commandID core.ID) string {
	t.Helper()
	// request принимает исходный RequestID квитанции по её внутреннему ID.
	var request string
	// err сообщает об ошибке чтения очередной полной SQL-строки.
	if err := s.sqlDB.QueryRowContext(context.Background(), "SELECT request_id FROM command_receipts WHERE id=?", commandID).Scan(&request); err != nil {
		t.Fatal(err)
	}
	return request
}

// Проверяет отказ отсутствующего или неподдерживаемого storage_format до восстановления управления и
// сохранение всех прежних долговечных строк.
func TestAcceptanceUnsupportedFormatRefusesBeforeRecovery(t *testing.T) {
	// mutation задаёт способ повреждения маркера формата во временной базе.
	for _, mutation := range []string{
		"DROP TABLE storage_format",
		"DELETE FROM storage_format",
		"UPDATE storage_format SET version=1",
		"DROP TABLE storage_format; CREATE TABLE storage_format(singleton INTEGER,version INTEGER); INSERT INTO storage_format VALUES(1,1),(2,1)",
	} {
		t.Run(mutation, func(t *testing.T) {
			// Проверяет отказ Open с ErrIncompatibleFormat до восстановления при отсутствии, удалении, прежней
			// версии либо множественности маркера формата.

			// f объединяет временный Store, Service, поток и опубликованную заглушку
			// проверяемого сценария.
			f := newDomainFixture(t, 1)
			// parent содержит точную выданную попытку родительской сессии.
			parent, _ := startObservationOutputs(t, f)
			// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
			ctx := context.Background()
			// seq принимает номер основной базы из PRAGMA database_list.
			var seq int
			// name принимает имя основной базы из PRAGMA database_list. path сохраняет путь
			// несовместимой временной базы для повторных попыток открытия.
			var name, path string
			// err сообщает об ошибке чтения очередной полной SQL-строки.
			if err := f.store.sqlDB.QueryRowContext(ctx, "PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
				t.Fatal(err)
			}
			// err сообщает об ошибке изменения изолированной SQLite для проверки отказа.
			if _, err := f.store.sqlDB.ExecContext(ctx, mutation); err != nil {
				t.Fatal(err)
			}
			// before сохраняет все долговечные строки до проверяемой операции.
			before := observationRows(t, f.store.sqlDB)
			// err сообщает об ошибке закрытия временного хранилища или контрольного
			// SQL-пула.
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				// s принимает Store, который должен отсутствовать при несовместимом
				// формате. err сообщает об ошибке проверки отказа открытия
				// несовместимого формата.
				s, err := Open(ctx, path, testOptions())
				if s != nil {
					_ = s.Close()
					t.Fatal("неподдерживаемый формат открыт")
				}
				if !errors.Is(err, ErrIncompatibleFormat) {
					t.Fatal("нет стабильной категории несовместимости", err)
				}
			}
			// raw содержит отдельный SQL-пул для проверки неизменности базы после отказа
			// Store. err сообщает об ошибке проверки отказа открытия несовместимого формата.
			raw, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает отдельный SQL-пул, открытый после отказа Store для проверки неизменности несовместимой БД.

				// err сообщает об ошибке закрытия временного хранилища или контрольного
				// SQL-пула.
				if err := raw.Close(); err != nil {
					t.Error(err)
				}
			})
			// got содержит прочитанное состояние для проверки неизменности либо сохранённого
			// результата.
			if got := observationRows(t, raw); !reflect.DeepEqual(before, got) {
				t.Fatal("отказ формата успел выполнить recovery либо reset")
			}
			// state принимает сохраняемое состояние сессии после отказа открытия
			// несовместимой базы.
			var state string
			// err сообщает об ошибке чтения очередной полной SQL-строки.
			if err := raw.QueryRowContext(ctx, "SELECT state FROM sessions WHERE id=?", parent.SessionID).Scan(&state); err != nil || state != string(core.SessionRunning) {
				t.Fatal("несовместимый Open изменил Session", state, err)
			}
		})
	}
}
