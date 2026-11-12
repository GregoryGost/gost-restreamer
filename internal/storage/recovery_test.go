package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Закрывает тестовый Store и повторно открывает тот же временный файл с новой incarnation и
// автоматическим восстановлением протокола управления.
func reopenFixture(t *testing.T, f domainFixture) domainFixture {
	t.Helper()
	// sequence принимает номер основной базы в результате PRAGMA database_list.
	var sequence int
	// name принимает имя базы, чтобы подтвердить повторное открытие main. dbPath сохраняет путь
	// временного файла SQLite для нового открытия.
	var name, dbPath string
	// err сообщает об ошибке чтения контрольных данных из SQLite.
	if err := f.store.sqlDB.QueryRowContext(context.Background(), "PRAGMA database_list").Scan(&sequence, &name, &dbPath); err != nil {
		t.Fatal(err)
	}
	if dbPath == "" || name != "main" {
		t.Fatalf("не удалось определить временную файловую БД: %q, %q", name, dbPath)
	}
	// err сообщает об ошибке закрытия открытого временного хранилища.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	// s содержит повторно открытое хранилище с новой incarnation. err сообщает об ошибке повторного
	// открытия временной базы с новым владельцем.
	s, err := Open(context.Background(), dbPath, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает повторно открытый временный Store после проверки восстановления.

		// err сообщает об ошибке закрытия открытого временного хранилища.
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	// svc предоставляет доменные операции текущего владельца временного Store.
	svc, err := core.NewService(s, s.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	f.store, f.service = s, svc
	return f
}

// Читает тестовую квитанцию по RequestID в согласованной транзакции.
func readReceipt(t *testing.T, s *Store, requestID string) core.CommandReceipt {
	t.Helper()
	// c содержит запрошенную сохраняемую квитанцию.
	var c core.CommandReceipt
	// err сообщает об ошибке согласованного чтения квитанции по RequestID.
	if err := s.View(context.Background(), func(r core.Repositories) error {
		// Читает квитанцию по RequestID в захваченное c через согласованный View.

		// err сообщает об ошибке чтения запрошенной квитанции.
		var err error
		c, err = r.Runtime.Receipt(context.Background(), requestID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

// Проверяет прерывание невыданного запуска после открытия новым владельцем без повторного исполнения,
// идемпотентность Recover и допустимость нового ручного запуска.
func TestRestartInterruptsUnissuedWorkWithoutReplay(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// pending содержит незавершённую заглушку, созданную до повторного открытия. err сообщает об
	// ошибке создания стабильной заглушки.
	pending, err := f.service.ReserveAsset(ctx, core.FallbackAsset{Name: "Незавершённое изображение", Kind: core.AssetImage, OriginalPath: "originals/pending.png"})
	if err != nil {
		t.Fatal(err)
	}
	f = reopenFixture(t, f)
	// s содержит сохраняемое состояние проверяемой сессии.
	s := readSession(t, f.store, start.SessionID)
	if s.State != core.SessionInterrupted || s.CleanupPending || s.EndedAt == nil {
		t.Fatalf("невыданный запуск после restart: %+v", s)
	}
	// c содержит сохраняемую квитанцию запуска после восстановления.
	if c := readReceipt(t, f.store, start.RequestID); c.Status != core.CommandInterrupted {
		t.Fatalf("pending-команда не прервана: %+v", c)
	}
	// o содержит сохраняемый выход с самостоятельным снимком и намерением.
	for _, o := range readOutputs(t, f.store, s.ID) {
		if o.Issued || o.CleanupPending || o.DesiredRunning {
			t.Fatalf("restart повторил выход: %+v", o)
		}
	}
	// err сообщает об ошибке захвата точной команды исполнения.
	if _, err := f.service.Claim(ctx, start.RequestID); !errors.Is(err, core.ErrStale) {
		t.Fatalf("прерванная команда доступна для replay: %v", err)
	}
	_, err = f.service.PublishAsset(ctx, pending.ID, pending.Revision, pending.AttemptToken, "late-hash", 10, core.SourceFormat{ImageFormat: "png"})
	requireDomainError(t, err, core.ErrInvalid)
	// err сообщает об ошибке восстановления незавершённого управления прежних владельцев.
	if err := f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	// after содержит долговечную сессию после проверяемого управления.
	if after := readSession(t, f.store, s.ID); !reflect.DeepEqual(s, after) {
		t.Fatal("повтор recovery переписал прерванную историю")
	}
	// newStart содержит квитанцию нового ручного запуска после прерывания старого.
	newStart, err := f.service.StartSession(ctx, f.startRequest("manual-after-restart"))
	if err != nil || newStart.SessionID == s.ID {
		t.Fatalf("новый ручной запуск после невыданной попытки: %+v, %v", newStart, err)
	}
	// before сохраняет долговечную сессию до проверяемого управления.
	before := readSession(t, f.store, newStart.SessionID)
	// err сообщает об ошибке восстановления незавершённого управления прежних владельцев.
	if err := f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	// after содержит долговечную сессию после проверяемого управления.
	if after := readSession(t, f.store, newStart.SessionID); !reflect.DeepEqual(before, after) {
		t.Fatal("recovery текущего владельца прервал живой запуск")
	}
}

// Проверяет сохранение удержаний выданной работы и успешной квитанции после смены владельца, точное
// подтверждение старой очистки и изоляцию нового ручного запуска.
func TestIssuedWorkKeepsHoldsUntilCorrectCleanupAfterRestart(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// completed сохраняет окончательную квитанцию до повторного запроса.
	completed := readReceipt(t, f.store, start.RequestID)
	// o содержит сохраняемый выход с самостоятельным снимком и намерением.
	o := readOutputs(t, f.store, start.SessionID)[0]
	// outputReceipt содержит квитанцию запуска выхода прежнего владельца.
	outputReceipt := outputStartReceipt(t, f, o.ID)
	// oldFence идентифицирует прежнюю выданную попытку выхода до перезапуска. err сообщает об ошибке
	// захвата точной команды исполнения.
	oldFence, err := f.service.Claim(ctx, outputReceipt.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке выдачи точной попытки команды.
	if err := f.service.MarkIssued(ctx, oldFence); err != nil {
		t.Fatal(err)
	}
	f = reopenFixture(t, f)
	// session содержит сохраняемый запуск для проверки состояния и владения.
	session := readSession(t, f.store, start.SessionID)
	if session.State != core.SessionInterrupted || !session.CleanupPending {
		t.Fatalf("потеря владельца освободила выданные эффекты: %+v", session)
	}
	// got содержит прочитанное сохраняемое значение для проверки результата операции.
	if got := readReceipt(t, f.store, start.RequestID); !reflect.DeepEqual(got, completed) {
		t.Fatal("recovery переписал завершённую квитанцию")
	}
	requireDomainError(t, f.service.Complete(ctx, oldFence, core.Completion{Success: true}), core.ErrStale)
	// newStart содержит квитанцию нового ручного запуска после прерывания старого.
	newStart, err := f.service.StartSession(ctx, f.startRequest("manual-before-cleanup"))
	if err != nil || newStart.SessionID == session.ID {
		t.Fatalf("ручной start при старой очистке: %+v, %v", newStart, err)
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, true)
	requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
	// cleanups содержит сохраняемые намерения физической очистки после проверяемой операции.
	cleanups := pendingCleanups(t, f.store, session.ID)
	if len(cleanups) != 2 {
		t.Fatalf("нужны отдельные session/output cleanup: %+v", cleanups)
	}
	// wrong содержит копию точной идентичности с намеренно подменённым полем.
	wrong := core.CleanupFenceFor(cleanups[0])
	wrong.Attempt++
	requireDomainError(t, f.service.AcknowledgeCleanup(ctx, wrong), core.ErrStale)
	if len(pendingCleanups(t, f.store, session.ID)) != 2 {
		t.Fatal("неверный ack освободил cleanup")
	}
	// cleanup содержит очередное точное намерение прежней очистки для подтверждения.
	for _, cleanup := range cleanups {
		// err сообщает об ошибке подтверждения точного намерения очистки.
		if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanup)); err != nil {
			t.Fatal(err)
		}
	}
	if len(pendingCleanups(t, f.store, session.ID)) != 0 {
		t.Fatal("подтверждённая очистка осталась pending")
	}
	_, err = f.service.StartSession(ctx, f.startRequest("duplicate-active-session"))
	requireDomainError(t, err, core.ErrInUse)
	// cleanup содержит очередное точное намерение прежней очистки для подтверждения.
	for _, cleanup := range cleanups {
		// err сообщает об ошибке подтверждения точного намерения очистки.
		if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanup)); err != nil {
			t.Fatal(err)
		}
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, true)
	requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
	// current содержит новый запуск после повторного подтверждения старой очистки.
	if current := readSession(t, f.store, newStart.SessionID); current.State != core.SessionStarting {
		t.Fatal("старый ack изменил новый запуск")
	}
}

// Проверяет удержание варианта предварительными ресурсами выданного запуска Session до подтверждения
// родительской очистки.
func TestIssuedSessionStartupKeepsPinnedVariantUntilCleanup(t *testing.T) {
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
	// err сообщает об ошибке выдачи точной попытки команды.
	if err := f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	f = reopenFixture(t, f)
	// session содержит сохраняемый запуск для проверки состояния и владения.
	if session := readSession(t, f.store, start.SessionID); session.State != core.SessionInterrupted || !session.CleanupPending {
		t.Fatalf("не сохранена очистка выданного запуска: %+v", session)
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, true)
	requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
	// cleanups содержит сохраняемые намерения физической очистки после проверяемой операции.
	cleanups := pendingCleanups(t, f.store, start.SessionID)
	if len(cleanups) != 1 || cleanups[0].OutputID != "" {
		t.Fatalf("нужна очистка выданного запуска Session: %+v", cleanups)
	}
	// err сообщает об ошибке подтверждения точного намерения очистки.
	if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
		t.Fatal(err)
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, false)
}

// Проверяет повторный запуск по прежнему снимку и освобождение только собственных удержаний после
// Detach и очистки без изменения соседа.
func TestStoppedAttachedOutputRestartsSavedSnapshotAndDetachReleasesOnlyOwnHold(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 2)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// outputs содержит все сохраняемые выходы проверяемой сессии.
	outputs := readOutputs(t, f.store, start.SessionID)
	// first содержит первый выход, который останавливается, запускается снова и отсоединяется.
	first := outputs[0]
	// err сообщает об ошибке принятия остановки отдельного выхода.
	if _, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "stop-unissued-output", OutputID: first.ID}); err != nil {
		t.Fatal(err)
	}
	// provisional содержит предварительную очистку невыданного выхода при выданной сессии.
	provisional := pendingCleanups(t, f.store, start.SessionID)
	if len(provisional) != 1 || provisional[0].OutputID != first.ID || provisional[0].Attempt != 0 {
		t.Fatalf("остановка потеряла очистку предварительной группы: %+v", provisional)
	}
	// err сообщает об ошибке подтверждения точного намерения очистки.
	if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(provisional[0])); err != nil {
		t.Fatal(err)
	}
	requireDomainError(t, f.service.DeleteDestination(ctx, first.Snapshot.Destination.ID, first.Snapshot.Destination.Revision), core.ErrInUse)
	// d содержит настройки проверяемого назначения.
	d := first.Snapshot.Destination
	d.StreamKey = "edited-synthetic-key"
	// updated содержит новую конфигурацию назначения, которая не должна заменить старый снимок
	// выхода. err сообщает об ошибке условного изменения назначения.
	updated, err := f.service.UpdateDestination(ctx, d, d.Revision)
	if err != nil {
		t.Fatal(err)
	}
	// restart содержит принятую квитанцию нового запуска прикреплённого выхода.
	restart, err := f.service.StartOutput(ctx, core.OutputCommand{RequestID: "restart-attached", OutputID: first.ID})
	if err != nil {
		t.Fatal(err)
	}
	executeCommand(t, f.service, restart.RequestID)
	// current содержит состояние нового запуска либо выходов после операции восстановления.
	current := readOutputs(t, f.store, start.SessionID)
	if !reflect.DeepEqual(first.Snapshot, current[0].Snapshot) || !current[0].Issued || !current[0].DesiredRunning {
		t.Fatalf("повторный start применил новую конфигурацию: %+v", current[0])
	}
	// err сообщает об ошибке принятия отсоединения выхода.
	if _, err := f.service.DetachOutput(ctx, core.OutputCommand{RequestID: "detach-issued-output", OutputID: first.ID}); err != nil {
		t.Fatal(err)
	}
	requireDomainError(t, f.service.DeleteDestination(ctx, d.ID, updated.Revision), core.ErrInUse)
	// cleanups содержит сохраняемые намерения физической очистки после проверяемой операции.
	cleanups := pendingCleanups(t, f.store, start.SessionID)
	if len(cleanups) != 1 || cleanups[0].OutputID != first.ID {
		t.Fatalf("detach затронул чужие ресурсы: %+v", cleanups)
	}
	// err сообщает об ошибке подтверждения точного намерения очистки.
	if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке удаления конфигурации назначения.
	if err := f.service.DeleteDestination(ctx, d.ID, updated.Revision); err != nil {
		t.Fatalf("очищенный detached output держит текущую конфигурацию: %v", err)
	}
	_, err = f.service.StartOutput(ctx, core.OutputCommand{RequestID: "restart-detached", OutputID: first.ID})
	requireDomainError(t, err, core.ErrConflict)
	current = readOutputs(t, f.store, start.SessionID)
	if current[0].Attached || current[0].CleanupPending || current[0].Issued || current[0].DesiredRunning {
		t.Fatalf("detached output не завершён: %+v", current[0])
	}
	if !reflect.DeepEqual(outputs[1], current[1]) {
		t.Fatal("detach изменил соседний выход")
	}
	// s содержит сохраняемое состояние проверяемой сессии.
	if s := readSession(t, f.store, start.SessionID); s.State != core.SessionRunning {
		t.Fatal("detach сам остановил Session")
	}
}

// Проверяет сохранение самостоятельных исторических снимков и событий после удаления текущей
// конфигурации и постановку всех принадлежащих файлов на очистку.
func TestHardDeletePreservesStandaloneSessionHistory(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// snapshot сохраняет снимок запуска перед удалением конфигураций.
	snapshot := readSession(t, f.store, start.SessionID).Snapshot
	// outputSnapshot сохраняет снимок выхода перед удалением конфигураций.
	outputSnapshot := readOutputs(t, f.store, start.SessionID)[0].Snapshot
	// stop содержит принятую квитанцию остановки сессии. err сообщает об ошибке принятия остановки
	// сессии.
	stop, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "stop-without-effects", SessionID: start.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	executeCommand(t, f.service, stop.RequestID)
	// err сообщает об ошибке удаления конфигурации назначения.
	if err := f.service.DeleteDestination(ctx, f.destinations[0].ID, f.destinations[0].Revision); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке удаления группы.
	if err := f.service.DeleteGroup(ctx, f.group.ID, f.group.Revision); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке удаления профиля при сохраняемых ссылках.
	if err := f.service.DeleteProfile(ctx, f.profile.ID, f.profile.Revision); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке удаления конфигурации потока.
	if err := f.service.DeleteStream(ctx, f.stream.ID, f.stream.Revision); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке удаления заглушки и её комплектов.
	if err := f.service.DeleteAsset(ctx, f.asset.ID, f.asset.Revision); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot, readSession(t, f.store, start.SessionID).Snapshot) || !reflect.DeepEqual(outputSnapshot, readOutputs(t, f.store, start.SessionID)[0].Snapshot) {
		t.Fatal("hard delete уничтожил самостоятельный снимок")
	}
	// err сообщает об ошибке проверки истории и очистки после удаления конфигураций.
	if err := f.store.View(ctx, func(r core.Repositories) error {
		// После удаления текущей конфигурации проверяет ErrNotFound Stream, сохранность служебных событий и
		// пяти уникальных путей файловой очистки.

		// err сообщает об ошибке чтения конфигурации потока.
		_, err := r.Config.Stream(ctx, f.stream.ID)
		requireDomainError(t, err, core.ErrNotFound)
		// events содержит служебные события выбранного запуска.
		events, err := r.Runtime.Events(ctx, core.ListQuery{Limit: core.MaxListLimit, SessionID: start.SessionID})
		if err != nil {
			return err
		}
		if len(events) < 2 {
			t.Fatal("hard delete уничтожил события")
		}
		// cleanups содержит сохраняемые намерения физической очистки после проверяемой операции.
		cleanups, err := r.Artifacts.Cleanups(ctx, core.ListQuery{Limit: core.MaxListLimit, PendingOnly: true})
		if err != nil {
			return err
		}
		// paths собирает пути файлов, для которых сохранились намерения очистки.
		paths := map[string]bool{}
		// cleanup содержит очередное точное намерение прежней очистки для подтверждения.
		for _, cleanup := range cleanups {
			if cleanup.Kind == core.CleanupFile {
				paths[cleanup.ManagedPath] = true
			}
		}
		if len(paths) != 5 || !paths[f.asset.OriginalPath] || !paths[f.variant.ManagedPath] {
			t.Fatalf("потеряны owned paths для cleanup: %+v", paths)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
