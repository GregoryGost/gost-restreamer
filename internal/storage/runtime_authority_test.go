package storage

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Активирует тестовый источник OBS с действующим RuntimeLease без внешних действий установки.
func activateRuntimeSource(t *testing.T, f domainFixture) *core.RuntimeLease {
	t.Helper()
	// lease содержит проверяемый логический допуск источника или владельца исполнения. err сообщает
	// об ошибке установки источника OBS с проверенным допуском.
	lease, err := f.service.ActivateOBS(context.Background(), f.stream.ID, f.stream.PublicationKey, func(*core.RuntimeLease) (func(), error) {
		// Возвращает nil вместо функции отзыва для успешной установки синтетического источника OBS.
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

// Проверяет недействительный допуск, закрытый Done и отказ выполнения Do после отзыва или закрытия
// реестра.
func requireRevokedLease(t *testing.T, lease *core.RuntimeLease) {
	t.Helper()
	if lease.Valid() {
		t.Fatal("допуск остался действительным")
	}
	select {
	case <-lease.Done():
	default:
		t.Fatal("отзыв не закрыл Done")
	}
	// called отмечает ошибочный вызов действия, которое проверка должна отклонить.
	called := false
	// err сообщает об ошибке выполнения действия через проверяемый RuntimeLease.
	err := lease.Do(context.Background(), func() error {
		// Отмечает ошибочный вызов действия отозванного RuntimeLease.
		called = true
		return nil
	})
	if called || (!errors.Is(err, core.ErrStale) && !errors.Is(err, core.ErrClosed)) {
		t.Fatalf("отозванный допуск выполнил действие: %v, %v", called, err)
	}
}

// Проверяет общий допуск разных Service: пауза сохраняет OBS, Stop отзывает его до возврата, а функции
// установки и отзыва выполняются вне SQL-транзакции.
func TestRuntimeAuthoritySharedAcrossServicesAndDirectWrites(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// other содержит второй Service с общей RuntimeAuthority того же Store. err сообщает об ошибке
	// создания доменного Service для текущего владельца Store.
	other, err := core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// installed хранит идентификатор установленного тестового источника OBS.
	var installed core.ID
	// revoked считает вызовы отзыва установленного источника OBS.
	var revoked atomic.Int32
	// lease содержит проверяемый логический допуск источника или владельца исполнения.
	lease, err := f.service.ActivateOBS(ctx, f.stream.ID, f.stream.PublicationKey, func(lease *core.RuntimeLease) (func(), error) {
		// Запоминает установленный ID и проверяет отсутствие занятой SQL-транзакции во время установки OBS.

		if f.store.sqlDB.Stats().InUse != 0 {
			t.Fatal("установка выполняется внутри транзакции")
		}
		installed = lease.ID()
		return func() {
			// Очищает установленный ID, считает отзыв и проверяет отсутствие занятой SQL-транзакции во время
			// отзыва.

			if f.store.sqlDB.Stats().InUse != 0 {
				t.Error("отзыв выполняется внутри транзакции")
			}
			installed = ""
			revoked.Add(1)
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке принятия ручной паузы.
	if _, err := other.PauseSession(ctx, core.SessionCommand{RequestID: "pause-authority", SessionID: start.SessionID}); err != nil {
		t.Fatal(err)
	}
	if !lease.Valid() || installed != lease.ID() {
		t.Fatal("пауза отозвала источник")
	}
	// err сообщает об ошибке принятия остановки сессии.
	if _, err := other.StopSession(ctx, core.SessionCommand{RequestID: "stop-authority", SessionID: start.SessionID}); err != nil {
		t.Fatal(err)
	}
	requireRevokedLease(t, lease)
	if installed != "" || revoked.Load() != 1 {
		t.Fatal("запись другого Service не отозвала источник до возврата")
	}
	// err сообщает об ошибке закрытия временного хранилища.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	if revoked.Load() != 1 {
		t.Fatal("Close повторно вызвал callback отзыва")
	}
}

// Проверяет сохранение источника после доказанного отката прямой записи и отзыв после COMMIT нового
// Lifecycle.
func TestRuntimeDirectWriteRollbackKeepsSourceAndCommitRevokes(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// lease содержит проверяемый логический допуск источника или владельца исполнения.
	lease := activateRuntimeSource(t, f)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// mutate увеличивает Lifecycle и Revision сессии для сравнения отката и COMMIT.
	mutate := func(r core.Repositories) error {
		// Увеличивает Lifecycle и Revision Session через репозиторий для сравнения откатившейся и сохранённой
		// записи.

		// session содержит сохраняемый запуск для проверки состояния и владения. err сообщает об
		// ошибке чтения сохраняемого запуска для изменения Lifecycle.
		session, err := r.Runtime.Session(ctx, start.SessionID)
		if err != nil {
			return err
		}
		// old сохраняет исходную ревизию сессии для условной прямой записи.
		old := session.Revision
		session.Lifecycle++
		session.Revision++
		return r.Runtime.UpdateSession(ctx, session, old)
	}
	// err сообщает об ошибке откатываемого изменения Lifecycle и Revision сессии.
	err := f.store.Write(ctx, func(r core.Repositories) error {
		// Меняет Session и возвращает ErrInvalid, чтобы проверить сохранение прежнего допуска после отката.

		// err сообщает об ошибке прямого увеличения Lifecycle и Revision сессии.
		if err := mutate(r); err != nil {
			return err
		}
		return core.ErrInvalid
	})
	requireDomainError(t, err, core.ErrInvalid)
	if !lease.Valid() || readSession(t, f.store, start.SessionID).Lifecycle != lease.SessionFence().Lifecycle {
		t.Fatal("подтверждённый откат отозвал неизменённый источник")
	}
	if err = f.store.Write(ctx, mutate); err != nil {
		t.Fatal(err)
	}
	requireRevokedLease(t, lease)
}

// Проверяет сохранение старой публикации при ошибке установки или устаревшем RuntimeSessionFence,
// точную замену и изоляцию нового источника от позднего отзыва старого.
func TestRuntimeActivationReplacementAndFailedInstall(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// current хранит идентификатор установленного источника OBS.
	var current core.ID
	// install устанавливает идентификатор источника и возвращает отзыв только своей публикации.
	install := func(lease *core.RuntimeLease) (func(), error) {
		// Запоминает ID новой публикации и возвращает отзыв только для установленного им источника.

		current = lease.ID()
		return func() {
			// Очищает current только при совпадении с собственным ID, сохраняя более новую публикацию.

			if current == lease.ID() {
				current = ""
			}
		}, nil
	}
	// old сохраняет допуск прежнего источника OBS. err сообщает об ошибке установки источника OBS с
	// проверенным допуском.
	old, err := f.service.ActivateOBS(ctx, f.stream.ID, f.stream.PublicationKey, install)
	if err != nil {
		t.Fatal(err)
	}
	// failed задаёт искусственный отказ установки нового источника.
	failed := errors.New("синтетический отказ установки")
	_, err = f.service.ActivateOBS(ctx, f.stream.ID, f.stream.PublicationKey, func(*core.RuntimeLease) (func(), error) {
		// Отклоняет установку новой публикации синтетической ошибкой для проверки сохранности прежней.
		return nil, failed
	})
	if !errors.Is(err, failed) || !old.Valid() || current != old.ID() {
		t.Fatal("неудачная установка изменила прежний источник")
	}
	// stale содержит ожидаемый запуск с намеренно изменённым Lifecycle.
	stale := old.SessionFence()
	stale.Lifecycle++
	_, err = f.service.ActivateOBSFor(ctx, stale, f.stream.ID, f.stream.PublicationKey, install)
	requireDomainError(t, err, core.ErrStale)
	if current != old.ID() || !old.Valid() {
		t.Fatal("устаревшая авторизация заменила источник")
	}
	// newLease содержит допуск успешно заменившего источника OBS.
	newLease, err := f.service.ActivateOBSFor(ctx, old.SessionFence(), f.stream.ID, f.stream.PublicationKey, install)
	if err != nil {
		t.Fatal(err)
	}
	requireRevokedLease(t, old)
	if current != newLease.ID() || newLease.ID() == old.ID() {
		t.Fatal("Override потерял новый источник или переиспользовал идентификатор")
	}
	if err = old.Revoke(ctx); err != nil {
		t.Fatal(err)
	}
	if !newLease.Valid() || current != newLease.ID() {
		t.Fatal("поздний отзыв прежнего источника повредил новый")
	}
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	requireRevokedLease(t, newLease)
	if current != "" {
		t.Fatal("Close оставил установленный источник")
	}
}

// Проверяет необходимость Issued для владельца исполнения, повторную привязку того же допуска и
// независимость Session, OBS и соседних выходов до их точного отзыва.
func TestRuntimeOwnerRequiresIssuanceAndSurvivesCompletionAndOBSOverride(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 2)
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// fence идентифицирует точную захваченную попытку проверяемой команды. err сообщает об ошибке
	// захвата точной команды исполнения.
	fence, err := f.service.Claim(ctx, start.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.BindExecutionOwner(ctx, fence)
	requireDomainError(t, err, core.ErrStale)
	if err = f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// owner содержит допуск физического владельца выданной сессии.
	owner, err := f.service.BindExecutionOwner(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	if !owner.Valid() {
		t.Fatal("успешное Complete отозвало физического владельца")
	}
	// duplicate содержит повторно найденный допуск того же ExecutionFence.
	duplicate, err := f.service.BindExecution(ctx, fence)
	if err != nil || duplicate != owner {
		t.Fatalf("один ExecutionFence получил несколько независимых допусков: %v", err)
	}
	// oldOBS содержит допуск заменяемого источника OBS.
	oldOBS := activateRuntimeSource(t, f)
	// newOBS содержит допуск заменившего источника OBS.
	newOBS := activateRuntimeSource(t, f)
	requireRevokedLease(t, oldOBS)
	if !owner.Valid() || !newOBS.Valid() {
		t.Fatal("Override отозвал независимого владельца Session")
	}
	// outputs содержит все сохраняемые выходы проверяемой сессии.
	outputs := readOutputs(t, f.store, start.SessionID)
	// leases хранит независимые допуски владельцев выходов.
	leases := make([]*core.RuntimeLease, len(outputs))
	// i указывает позицию выхода и соответствующего ему допуска. output содержит сохраняемый выход с
	// самостоятельным снимком и намерением.
	for i, output := range outputs {
		// command содержит внутреннюю квитанцию запуска выбранного выхода.
		command := outputStartReceipt(t, f, output.ID)
		// outputFence содержит точную выданную попытку запуска очередного выхода.
		outputFence := executeCommand(t, f.service, command.RequestID)
		leases[i], err = f.service.BindExecutionOwner(ctx, outputFence)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = f.service.PauseSession(ctx, core.SessionCommand{RequestID: "owner-pause", SessionID: start.SessionID}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.StopOutput(ctx, core.OutputCommand{RequestID: "owner-stop-output", OutputID: outputs[0].ID}); err != nil {
		t.Fatal(err)
	}
	requireRevokedLease(t, leases[0])
	if !leases[1].Valid() || !owner.Valid() || !newOBS.Valid() {
		t.Fatal("команда соседа или пауза отозвала независимый допуск")
	}
	if _, err = f.service.StopSession(ctx, core.SessionCommand{RequestID: "owner-stop-session", SessionID: start.SessionID}); err != nil {
		t.Fatal(err)
	}
	// lease содержит проверяемый логический допуск источника или владельца исполнения.
	for _, lease := range []*core.RuntimeLease{leases[1], owner, newOBS} {
		requireRevokedLease(t, lease)
	}
	if len(pendingCleanups(t, f.store, start.SessionID)) != 3 {
		t.Fatal("привязка исполнения подтвердила очистку без физического доказательства")
	}
}

// Проверяет ограничение ожидания общей блокировки сроком вызывающего и отдельным бюджетом Store.
func TestRuntimeAuthorityWaitHonorsWriteBudget(t *testing.T) {
	// callerBudget выбирает ограничение ожидания контекстом вызывающего либо бюджетом Store.
	for _, callerBudget := range []bool{true, false} {
		t.Run(map[bool]string{true: "caller", false: "store"}[callerBudget], func(t *testing.T) {
			// Запускает проверку срока ожидания общей блокировки с бюджетом вызывающего контекста либо Store.

			testRuntimeAuthorityWaitBudget(t, callerBudget)
		})
	}
}

// Удерживает общую блокировку через RuntimeLease и проверяет отказ ожидающей записи до вызова
// переданной функции без отзыва источника.
func testRuntimeAuthorityWaitBudget(t *testing.T, callerBudget bool) {
	t.Helper()
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// lease содержит проверяемый логический допуск источника или владельца исполнения.
	lease := activateRuntimeSource(t, f)
	// Детерминированно удерживаем RuntimeAuthority, чтобы проверить отмену ожидающей записи.
	// entered сигнализирует, что действие RuntimeLease уже заняло общую блокировку. release
	// разрешает держателю освободить общую блокировку. finished передаёт результат действия
	// держателя блокировки.
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	// holder ограничивает срок действия держателя общей блокировки. cancelHolder отменяет действие
	// держателя общей блокировки при завершении теста.
	holder, cancelHolder := context.WithTimeout(context.Background(), 2*time.Second)
	// done сигнализирует завершение горутины держателя блокировки.
	done := make(chan struct{})
	t.Cleanup(func() {
		// Отменяет контекст держателя общей блокировки и дожидается его горутины при завершении теста.
		cancelHolder()
		<-done
	})
	go func() {
		// В отдельной горутине занимает RuntimeLease.Do и передаёт результат ожидания освобождения общей
		// блокировки.

		defer close(done)
		finished <- lease.Do(holder, func() error {
			// Удерживает общую блокировку до release либо отмены контекста держателя.

			close(entered)
			select {
			case <-release:
				return nil
			case <-holder.Done():
				return holder.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-holder.Done():
		t.Fatal("проверка не смогла удержать authority")
	}
	// budget задаёт срок ожидающей записи для выбранного источника ограничения.
	budget := 20 * time.Millisecond
	if callerBudget {
		f.store.options.RetryTimeout = time.Second
	} else {
		f.store.options.RetryTimeout = budget
		budget = time.Second
	}
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite. cancel освобождает таймер
	// контекста ожидающей записи.
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	// called отмечает ошибочный вызов действия, которое проверка должна отклонить.
	called := false
	// err сообщает об ошибке ожидающей записи, чей срок должен истечь до вызова тела транзакции.
	err := f.store.Write(ctx, func(core.Repositories) error {
		// Отмечает ошибочный вызов Write, срок которого должен истечь при ожидании общей блокировки.
		called = true
		return nil
	})
	close(release)
	// heldErr содержит результат действия, удерживавшего общую блокировку.
	if heldErr := <-finished; heldErr != nil {
		t.Fatal(heldErr)
	}
	if called || !errors.Is(err, context.DeadlineExceeded) || !lease.Valid() {
		t.Fatalf("ожидание authority нарушило бюджет или отозвало источник: %v, %v", called, err)
	}
}

// Проверяет логический отзыв неуспешного выданного запуска при сохранении Issued и обязательства
// физической очистки.
func TestRuntimeFailedIssuedStartupRevokesOwnerWithoutCleanupAcknowledgement(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// fence идентифицирует точную захваченную попытку проверяемой команды. err сообщает об ошибке
	// захвата точной команды исполнения.
	fence, err := f.service.Claim(ctx, start.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// owner содержит допуск физического владельца выданной сессии.
	owner, err := f.service.BindExecutionOwner(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.Complete(ctx, fence, core.Completion{Failure: "синтетический отказ запуска"}); err != nil {
		t.Fatal(err)
	}
	requireRevokedLease(t, owner)
	// session содержит сохраняемый запуск для проверки состояния и владения.
	if session := readSession(t, f.store, start.SessionID); session.State != core.SessionFailed || !session.CleanupPending || !session.Issued {
		t.Fatal("отказ запуска выдумал физическую очистку")
	}
	if len(pendingCleanups(t, f.store, start.SessionID)) != 1 {
		t.Fatal("отказ запуска потерял ожидающую очистку")
	}
}

// Проверяет запрет выдачи допуска исполнения Service с чужой incarnation при сохранении читающей
// семантики AuthorizeOBS.
func TestRuntimeServiceCannotUseAnotherIncarnation(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// fence идентифицирует точную захваченную попытку проверяемой команды.
	fence := executeCommand(t, f.service, start.RequestID)
	// foreign содержит Service с incarnation, отличной от текущего владельца Store. err сообщает об
	// ошибке создания этого тестового Service.
	foreign, err := core.NewService(f.store, "another-incarnation")
	if err != nil {
		t.Fatal(err)
	}
	_, err = foreign.BindExecutionOwner(context.Background(), fence)
	requireDomainError(t, err, core.ErrStale)
	_, err = foreign.ActivateOBS(context.Background(), f.stream.ID, f.stream.PublicationKey, func(*core.RuntimeLease) (func(), error) {
		// Возвращает пустой результат установки, которая должна быть отклонена для чужой Incarnation до
		// вызова.
		return nil, nil
	})
	requireDomainError(t, err, core.ErrStale)
	// err сообщает об ошибке проверки допуска OBS по текущему запуску и ключу.
	if _, err := foreign.AuthorizeOBS(context.Background(), f.stream.ID, f.stream.PublicationKey); err != nil {
		t.Fatalf("старый API снимка изменил смысл чтения: %v", err)
	}
}

// Проверяет повторную проверку Session после сетевого принятия: авторизация прежнего запуска не
// заменяет публикацию нового.
func TestRuntimeActivationCannotCrossSessionDuringNetworkAccept(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// first содержит квитанцию первоначального запуска перед сетевым ожиданием OBS.
	first := startDomainSession(t, f)
	executeCommand(t, f.service, first.RequestID)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// initial сохраняет авторизованный запуск перед сменой сессии. err сообщает об ошибке проверки
	// допуска OBS по текущему запуску и ключу.
	initial, err := f.service.AuthorizeOBS(ctx, f.stream.ID, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	// expected содержит точный допуск старого запуска для повторной проверки после ожидания.
	expected := core.RuntimeSessionFence{SessionID: initial.ID, Incarnation: initial.Incarnation, Lifecycle: initial.Lifecycle}
	// stop содержит принятую квитанцию остановки сессии.
	stop, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "first-session-stop", SessionID: first.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	executeCommand(t, f.service, stop.RequestID)
	// cleanup содержит очередное точное намерение прежней очистки для подтверждения.
	for _, cleanup := range pendingCleanups(t, f.store, first.SessionID) {
		// err сообщает об ошибке подтверждения точного намерения очистки.
		if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanup)); err != nil {
			t.Fatal(err)
		}
	}
	// second содержит квитанцию нового запуска, который старая авторизация не должна заменить.
	second, err := f.service.StartSession(ctx, f.startRequest("second-session-start"))
	if err != nil {
		t.Fatal(err)
	}
	executeCommand(t, f.service, second.RequestID)
	// current содержит допуск источника OBS нового запуска.
	current := activateRuntimeSource(t, f)
	// called отмечает ошибочный вызов действия, которое проверка должна отклонить.
	called := false
	_, err = f.service.ActivateOBSFor(ctx, expected, f.stream.ID, f.stream.PublicationKey, func(*core.RuntimeLease) (func(), error) {
		// Отмечает ошибочный вызов установки OBS с RuntimeSessionFence прежней Session после сетевого
		// ожидания.
		called = true
		return nil, nil
	})
	if !errors.Is(err, core.ErrStale) || called || !current.Valid() || current.SessionFence().SessionID != second.SessionID {
		t.Fatalf("авторизация прежней Session пересекла сетевое окно: %v, %v", called, err)
	}
}
