package storage

import (
	"context"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет смену намерения паузы без изменения Lifecycle, снимка и здоровых выходов, отзыв прежней
// паузы и идемпотентный повтор Resume.
func TestResumePreservesSessionSnapshotAndHealthyOutputAttempt(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 2)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// before сохраняет долговечную сессию до проверяемого управления.
	before := readSession(t, f.store, start.SessionID)
	// outputs содержит все сохраняемые выходы проверяемой сессии.
	outputs := readOutputs(t, f.store, start.SessionID)
	// fences сохраняет точные выданные попытки выходов для проверки независимости управления.
	fences := make([]core.ExecutionFence, len(outputs))
	// i указывает выход в списке, а output содержит его сохраняемый снимок и намерение.
	for i, output := range outputs {
		// command содержит внутреннюю квитанцию запуска выбранного выхода.
		command := outputStartReceipt(t, f, output.ID)
		// err передаёт результат захвата очередной команды запуска выхода.
		var err error
		fences[i], err = f.service.Claim(ctx, command.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		// err сообщает об ошибке выдачи точной попытки команды.
		if err := f.service.MarkIssued(ctx, fences[i]); err != nil {
			t.Fatal(err)
		}
	}
	// issuedOutputs сохраняет выходы после выдачи попыток до управления паузой.
	issuedOutputs := readOutputs(t, f.store, start.SessionID)
	// pause содержит принятую квитанцию включения ручной паузы. err сообщает об ошибке принятия
	// ручной паузы.
	pause, err := f.service.PauseSession(ctx, core.SessionCommand{RequestID: "pause-fallback", SessionID: start.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	// pauseFence идентифицирует точную выданную попытку включения паузы.
	pauseFence, err := f.service.Claim(ctx, pause.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке выдачи точной попытки команды.
	if err := f.service.MarkIssued(ctx, pauseFence); err != nil {
		t.Fatal(err)
	}
	// session содержит сохраняемый запуск для проверки состояния и владения.
	if session := readSession(t, f.store, start.SessionID); !session.DesiredPaused {
		t.Fatal("принятая пауза не включила желаемый fallback")
	}
	// request задаёт повторяемый запрос снятия ручной паузы.
	request := core.SessionCommand{RequestID: "resume-live", SessionID: start.SessionID}
	// resume содержит принятую квитанцию снятия ручной паузы.
	resume, err := f.service.ResumeSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// after содержит долговечную сессию после проверяемого управления.
	after := readSession(t, f.store, start.SessionID)
	if after.DesiredPaused || after.State != core.SessionRunning || after.Lifecycle != before.Lifecycle || !reflect.DeepEqual(after.Snapshot, before.Snapshot) {
		t.Fatalf("возобновление изменило жизненный цикл или снимок: %+v", after)
	}
	if !reflect.DeepEqual(issuedOutputs, readOutputs(t, f.store, start.SessionID)) {
		t.Fatal("пауза или возобновление изменили выходы")
	}
	// err сообщает об ошибке проверки допуска OBS по текущему запуску и ключу.
	if _, err := f.service.AuthorizeOBS(ctx, f.stream.ID, f.stream.PublicationKey); err != nil {
		t.Fatalf("возобновление отозвало OBS: %v", err)
	}
	requireDomainError(t, f.service.Complete(ctx, pauseFence, core.Completion{Success: true}), core.ErrStale)
	// receipt содержит сохраняемую квитанцию выбранной команды.
	if receipt := readReceipt(t, f.store, pause.RequestID); receipt.Status != core.CommandInterrupted {
		t.Fatalf("отменённая пауза осталась исполнимой: %+v", receipt)
	}
	// fence содержит точную выданную попытку здорового выхода для проверки сохранности после Resume.
	for _, fence := range fences {
		// err сообщает об ошибке сохранения результата точной команды.
		if err := f.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
			t.Fatalf("возобновление аннулировало здоровый callback: %v", err)
		}
	}
	executeCommand(t, f.service, resume.RequestID)
	// completed сохраняет окончательную квитанцию до повторного запроса.
	completed := readReceipt(t, f.store, resume.RequestID)
	// stable сохраняет состояние сессии до тождественного повтора запроса.
	stable := readSession(t, f.store, start.SessionID)
	// replayed содержит квитанцию тождественного повторного запроса.
	replayed, err := f.service.ResumeSession(ctx, request)
	if err != nil || !reflect.DeepEqual(completed, replayed) || !reflect.DeepEqual(stable, readSession(t, f.store, start.SessionID)) {
		t.Fatalf("повтор возобновления изменил результат или сессию: %+v, %v", replayed, err)
	}
	_, err = f.service.PauseSession(ctx, request)
	requireDomainError(t, err, core.ErrConflict)
}

// Проверяет изменение только имени, ревизии и времени Asset без замены поколения и исторического
// снимка, включая отказы проверки и CAS.
func TestRenameAssetChangesOnlyNameAndRevisionWithoutReplacingLiveGeneration(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// output содержит сохраняемый выход с самостоятельным снимком и намерением.
	output := readOutputs(t, f.store, start.SessionID)[0]
	// name содержит очередное недопустимое имя стабильной заглушки.
	for _, name := range []string{"", " \t", "имя\x00", string([]byte{0xff})} {
		// err сообщает об ошибке условного переименования стабильной заглушки.
		_, err := f.service.RenameAsset(ctx, f.asset.ID, f.asset.Revision, name)
		requireDomainError(t, err, core.ErrInvalid)
		// got содержит прочитанное сохраняемое значение для проверки результата операции.
		got, err := f.service.GetAsset(ctx, f.asset.ID)
		if err != nil || !reflect.DeepEqual(got, f.asset) {
			t.Fatalf("ошибка имени изменила оригинал: %+v, %v", got, err)
		}
	}
	// renamed содержит заглушку после успешного изменения только имени и ревизии. err сообщает об
	// ошибке условного переименования стабильной заглушки.
	renamed, err := f.service.RenameAsset(ctx, f.asset.ID, f.asset.Revision, "Новое имя оригинала")
	if err != nil {
		t.Fatal(err)
	}
	// expected задаёт ожидаемое значение после проверяемого изменения.
	expected := f.asset
	expected.Name, expected.Revision, expected.UpdatedAt = "Новое имя оригинала", f.asset.Revision+1, renamed.UpdatedAt
	if renamed.UpdatedAt.Before(f.asset.UpdatedAt) || !reflect.DeepEqual(renamed, expected) {
		t.Fatalf("переименование изменило содержимое или владение: %+v", renamed)
	}
	_, err = f.service.RenameAsset(ctx, f.asset.ID, f.asset.Revision, "Устаревшая правка")
	requireDomainError(t, err, core.ErrConflict)
	_, err = f.service.RenameAsset(ctx, "missing-asset", 1, "Отсутствующий оригинал")
	requireDomainError(t, err, core.ErrNotFound)
	// stored содержит повторно прочитанные метаданные после проверяемого сохранения.
	stored, err := f.service.GetAsset(ctx, f.asset.ID)
	if err != nil || !reflect.DeepEqual(stored, renamed) {
		t.Fatalf("ошибка CAS изменила оригинал: %+v, %v", stored, err)
	}
	// variant содержит точный опубликованный вариант тестового поколения.
	variant, err := f.service.GetVariant(ctx, f.variant.ID)
	if err != nil || !reflect.DeepEqual(variant, f.variant) || !reflect.DeepEqual(output, readOutputs(t, f.store, start.SessionID)[0]) {
		t.Fatalf("переименование заменило поколение или снимок: %+v, %v", variant, err)
	}
	requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
}
