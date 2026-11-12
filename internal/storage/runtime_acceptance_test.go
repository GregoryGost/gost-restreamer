package storage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

// Проверяет отзыв всех одновременно принятых публикаций при Stop через другой Service и сохранение
// долговечного обязательства очистки.
func TestAcceptanceConcurrentPublicationAndCrossServiceStop(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// other содержит второй Service того же Store с общей RuntimeAuthority. err сообщает об ошибке
	// создания доменного Service для текущего владельца Store.
	other, err := core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// initial сохраняет запуск, авторизованный до гонки публикаций и остановки.
	initial, err := f.service.AuthorizeOBS(ctx, f.stream.ID, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	// expected содержит точный допуск исходного запуска для конкурирующих публикаций.
	expected := core.RuntimeSessionFence{SessionID: initial.ID, Incarnation: initial.Incarnation, Lifecycle: initial.Lifecycle}
	// publishers задаёт число одновременных попыток публикации OBS.
	const publishers = 12
	// gate одновременно разрешает публикации и остановку через другой Service.
	gate := make(chan struct{})
	// leases передаёт успешно выданные допуски конкурирующих источников.
	leases := make(chan *core.RuntimeLease, publishers)
	// errorsCh передаёт неожиданные ошибки публикаций и остановки.
	errorsCh := make(chan error, publishers+1)
	// wg ожидает завершение всех публикаций и конкурирующей остановки.
	var wg sync.WaitGroup
	// current хранит идентификатор установленного источника OBS.
	var current core.ID
	// installed считает успешные вызовы установки источника OBS. revoked считает вызовы отзыва
	// установленных источников.
	installed, revoked := 0, 0
	// install учитывает установки и возвращает отзыв только своего источника.
	install := func(l *core.RuntimeLease) (func(), error) {
		// Учитывает установку OBS и запоминает ID текущей публикации под общей блокировкой.

		installed++
		current = l.ID()
		return func() {
			// Учитывает отзыв публикации и очищает current только при совпадении с собственным ID.

			revoked++
			if current == l.ID() {
				current = ""
			}
		}, nil
	}
	for range publishers {
		wg.Go(func() {
			// После общего сигнала пытается активировать OBS с исходным RuntimeSessionFence и передаёт допуск либо
			// неожиданную ошибку.

			<-gate
			// lease содержит допуск принятой публикации, который Stop должен отозвать. err
			// сообщает об ошибке конкурирующей установки OBS по точному допуску запуска.
			lease, err := f.service.ActivateOBSFor(ctx, expected, f.stream.ID, f.stream.PublicationKey, install)
			if err == nil {
				leases <- lease
			} else if !errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrNotReady) && !errors.Is(err, core.ErrStale) {
				errorsCh <- err
			}
		})
	}
	wg.Go(func() {
		// После общего сигнала принимает StopSession через другой Service того же Store.

		<-gate
		// err сообщает об ошибке принятия остановки сессии.
		_, err := other.StopSession(ctx, core.SessionCommand{RequestID: "concurrent-stop", SessionID: start.SessionID})
		if err != nil {
			errorsCh <- err
		}
	})
	close(gate)
	wg.Wait()
	close(errorsCh)
	close(leases)
	// err содержит очередную неожиданную ошибку конкурентной публикации или остановки.
	for err := range errorsCh {
		t.Error(err)
	}
	// lease содержит допуск принятой публикации, который Stop должен отозвать.
	for lease := range leases {
		if lease.Valid() {
			t.Error("stop вернулся с действующим допуском OBS")
		}
		select {
		case <-lease.Done():
		default:
			t.Error("отозванный источник не завершил Done")
		}
		// err сообщает об ошибке проверки запрета действия отозванного допуска.
		if err := lease.Do(ctx, func() error {
			// Возвращает контрольную ошибку, если действие отозванного RuntimeLease всё-таки будет вызвано.
			return errors.New("отозванное действие выполнено")
		}); !errors.Is(err, core.ErrStale) {
			t.Errorf("отозванный допуск разрешил действие: %v", err)
		}
	}
	if installed != revoked || current != "" {
		t.Fatalf("не все установки отозваны: установлено=%d отозвано=%d current=%q", installed, revoked, current)
	}
	// got содержит сохраняемую сессию после конкурирующей остановки.
	if got := readSession(t, f.store, start.SessionID); got.State != core.SessionStopping || !got.CleanupPending {
		t.Fatalf("stop потерял сохранённое обязательство очистки: %+v", got)
	}
}

// Проверяет, что незавершённое построение команды Supervisor сохраняет SQL-очистку до фактического
// окончания работы и отдельного явного подтверждения.
func TestAcceptancePersistedCleanupWaitsForSupervisorJoinProof(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// fence идентифицирует точную захваченную попытку проверяемой команды.
	fence := executeCommand(t, f.service, start.RequestID)
	// owner содержит логический допуск нового точного исполнения. err сообщает об ошибке проверки и
	// выдачи логического допуска точного исполнения.
	owner, err := f.service.BindExecutionOwner(context.Background(), fence)
	if err != nil {
		t.Fatal(err)
	}
	// parent задаёт родительский каталог короткого пути Supervisor.
	parent := os.TempDir()
	if runtime.GOOS == "darwin" {
		// Системный TMPDIR macOS слишком длинный для вложенных путей UDS.
		parent = "/tmp"
	}
	// base содержит созданный временный каталог Supervisor и его UDS.
	base, err := os.MkdirTemp(parent, "gc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Удаляет временный каталог данного теста при выполнении зарегистрированного завершения.

		// err сообщает об ошибке удаления временного каталога Supervisor.
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	// s содержит Supervisor с задержанным построением команды.
	s, err := ffmpeg.New(ffmpeg.Config{BaseDir: base, StartupTimeout: 5 * time.Second, VideoTimeout: time.Second, AudioTimeout: time.Second, WatchdogInterval: 10 * time.Millisecond, MaxRestarts: 1, RestartBackoff: 10 * time.Millisecond, GracePeriod: 20 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: time.Second, DiagnosticEvents: 16, DiagnosticBytes: 8192, DiagnosticChunkBytes: 64, MaxCommandBytes: 4096, MaxCommandArgs: 16})
	if err != nil {
		t.Fatal(err)
	}
	// entered сигнализирует начало построения команды Supervisor. release разрешает завершить
	// задержанное построение команды.
	entered, release := make(chan struct{}), make(chan struct{})
	// releaseOnce гарантирует однократное закрытие сигнала освобождения построителя.
	var releaseOnce sync.Once
	t.Cleanup(func() {
		// Разрешает построителю команды завершиться и вызывает Stop с отдельным конечным сроком, сообщая
		// ошибку остановки Supervisor.

		releaseOnce.Do(func() {
			// Однократно закрывает release, чтобы заблокированный построитель команды мог завершиться.
			close(release)
		})
		// ctx задаёт контекст доменных операций проверяемого сценария SQLite. cancel отменяет
		// контекст проверяемой остановки или допуска.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		// err сообщает об ошибке остановки и ожидания очистки Supervisor.
		if err := s.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	// handler удерживает обработчик соединения до отмены контекста работника.
	handler := func(ctx context.Context, _ net.Conn, _ ffmpeg.Progress) error {
		// Ждёт отмены контекста обработчика соединения и возвращает её причину.
		<-ctx.Done()
		return ctx.Err()
	}
	// err сообщает об ошибке регистрации работника с заблокированным построением команды.
	if err := s.Start(context.Background(), ffmpeg.Worker{SessionID: string(start.SessionID), GroupID: "group", Owner: owner, Command: func(context.Context, ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Сигнализирует вход построителя команды, ждёт release и возвращает несуществующий бинарник для
		// проверки незавершённого запуска.

		close(entered)
		<-release
		return ffmpeg.Command{Binary: "/does-not-exist"}, nil
	}, Input: handler, Output: handler}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	// result содержит итог Supervisor с доказательством завершения и состоянием очистки. err
	// сообщает об ошибке ожидания доказанного завершения Supervisor.
	case <-s.Done():
		result, err := s.Wait(context.Background())
		t.Fatalf("worker завершился до builder: %+v, %v", result, err)
	case <-time.After(time.Second):
		t.Fatal("зарегистрированный builder не начался")
	}
	// other содержит второй Service того же Store с общей RuntimeAuthority.
	other, err := core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// stop содержит принятую квитанцию остановки сессии.
	stop, err := other.StopSession(context.Background(), core.SessionCommand{RequestID: "stop-registered-start", SessionID: start.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	executeCommand(t, other, stop.RequestID)
	// cancelled содержит уже отменённый контекст для проверки незавершённой остановки. cancel
	// отменяет контекст проверяемой остановки или допуска.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	// err сообщает об ошибке остановки и ожидания очистки Supervisor.
	if err := s.Stop(cancelled); !errors.Is(err, ffmpeg.ErrCleanupPending) {
		t.Fatal("незавершённый start выдан за join:", err)
	}
	// pending содержит сохраняемое обязательство очистки до явного подтверждения.
	if pending := pendingCleanups(t, f.store, start.SessionID); len(pending) != 1 {
		t.Fatalf("до join потеряно обязательство очистки: %+v", pending)
	}
	releaseOnce.Do(func() {
		// Однократно освобождает построитель команды перед ожиданием доказанного завершения Supervisor.
		close(release)
	})
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite. waitCancel освобождает
	// таймер ожидания доказанного завершения Supervisor.
	ctx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer waitCancel()
	// result содержит итог Supervisor с доказательством завершения и состоянием очистки.
	result, err := s.Wait(ctx)
	if err != nil || !result.Clean || result.CleanupErr != nil {
		t.Fatalf("join не доказан: %+v %v", result, err)
	}
	// pending содержит сохраняемое обязательство очистки до явного подтверждения.
	pending := pendingCleanups(t, f.store, start.SessionID)
	if len(pending) != 1 {
		t.Fatal("Supervisor автоматически подтвердил сохранённую очистку")
	}
	// err сообщает об ошибке подтверждения точного намерения очистки.
	if err := other.AcknowledgeCleanup(ctx, core.CleanupFenceFor(pending[0])); err != nil {
		t.Fatal(err)
	}
	if len(pendingCleanups(t, f.store, start.SessionID)) != 0 {
		t.Fatal("явное подтверждение после join не завершило очистку")
	}
}

// Проверяет сохранение текущей публикации при неверном ключе, отменённом допуске и полностью
// откатившейся записи.
func TestAcceptanceInvalidAdmissionPreservesCurrentPublication(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// current содержит допуск текущего источника OBS до ошибочных попыток замены.
	current := activateRuntimeSource(t, f)
	// key задаёт пустой либо чужой ключ допуска OBS.
	for _, key := range []string{"", "wrong-synthetic-key"} {
		// called отмечает ошибочный вызов установки при отклонённом допуске.
		called := false
		// err сообщает об ошибке конкурирующей установки OBS по точному допуску запуска.
		if _, err := f.service.ActivateOBSFor(ctx, current.SessionFence(), f.stream.ID, key, func(*core.RuntimeLease) (func(), error) {
			// Отмечает ошибочный вызов установки OBS при пустом либо чужом ключе.
			called = true
			return nil, nil
		}); err == nil || called || !current.Valid() {
			t.Fatalf("ошибочный ключ изменил публикацию: called=%v valid=%v err=%v", called, current.Valid(), err)
		}
	}
	// ctxCancelled содержит отменённый контекст попытки установки OBS. cancel отменяет контекст
	// проверяемой остановки или допуска.
	ctxCancelled, cancel := context.WithCancel(ctx)
	cancel()
	// called отмечает ошибочный вызов установки при отклонённом допуске.
	called := false
	// err сообщает об ошибке конкурирующей установки OBS по точному допуску запуска.
	if _, err := f.service.ActivateOBSFor(ctxCancelled, current.SessionFence(), f.stream.ID, f.stream.PublicationKey, func(*core.RuntimeLease) (func(), error) {
		// Отмечает ошибочный вызов установки OBS после отмены контекста допуска.
		called = true
		return nil, nil
	}); !errors.Is(err, context.Canceled) || called || !current.Valid() {
		t.Fatalf("отменённый Accept изменил публикацию: %v", err)
	}
	// err сообщает об ошибке искусственно откатываемой записи для проверки сохранности публикации
	// OBS.
	if err := f.store.Write(ctx, func(core.Repositories) error {
		// Возвращает синтетический ErrInvalid для проверки сохранения действующей публикации после
		// подтверждённого отката.

		return fmt.Errorf("синтетический rollback: %w", core.ErrInvalid)
	}); !errors.Is(err, core.ErrInvalid) || !current.Valid() {
		t.Fatalf("откат CRUD отозвал действующий источник: %v", err)
	}
}

// Проверяет отзыв только отказавшего выхода через другой Service с сохранением соседа, родительского
// исполнения и OBS.
func TestAcceptanceOutputFailureRevokesOnlyItsOwner(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 2)
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// parentFence содержит точную выданную попытку родительской сессии.
	parentFence := executeCommand(t, f.service, start.RequestID)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// parent содержит логический допуск владельца родительской сессии. err сообщает об ошибке
	// проверки и выдачи логического допуска точного исполнения.
	parent, err := f.service.BindExecutionOwner(ctx, parentFence)
	if err != nil {
		t.Fatal(err)
	}
	// publication содержит допуск OBS, независимый от владельцев выходов.
	publication := activateRuntimeSource(t, f)
	// outputs содержит все сохраняемые выходы проверяемой сессии.
	outputs := readOutputs(t, f.store, start.SessionID)
	// fences сохраняет точные выданные попытки выходов для проверки независимости управления.
	fences := make([]core.ExecutionFence, 2)
	// owners хранит независимые допуски двух выданных выходов.
	owners := make([]*core.RuntimeLease, 2)
	// i указывает позицию выхода и его допуска в контрольных массивах. output содержит сохраняемый
	// выход с самостоятельным снимком и намерением.
	for i, output := range outputs {
		// command содержит внутреннюю квитанцию запуска выбранного выхода.
		command := outputStartReceipt(t, f, output.ID)
		fences[i] = executeCommand(t, f.service, command.RequestID)
		owners[i], err = f.service.BindExecutionOwner(ctx, fences[i])
		if err != nil {
			t.Fatal(err)
		}
	}
	// other содержит второй Service того же Store с общей RuntimeAuthority.
	other, err := core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// observers сохраняет независимых наблюдателей точных попыток двух выходов.
	observers := []*core.OutputObserver{openOutputObserver(t, other, fences[0]), openOutputObserver(t, other, fences[1])}
	// err сообщает об ошибке фиксации окончательного отказа точного выхода.
	if err := other.FailOutput(ctx, observers[0], "синтетический отказ"); err != nil {
		t.Fatal(err)
	}
	if owners[0].Valid() || !owners[1].Valid() || !parent.Valid() || !publication.Valid() {
		t.Fatal("частичный отказ нарушил независимость runtime-владельцев")
	}
	if len(pendingCleanups(t, f.store, start.SessionID)) != 1 {
		t.Fatal("после отказа выданной попытки требуется одно сохранённое обязательство очистки")
	}
	// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
	if err := observers[1].Observe(ctx, core.OutputObservation{State: core.OutputRetrying, Error: "повтор соединения"}); err != nil {
		t.Fatal(err)
	}
	if !owners[1].Valid() || !parent.Valid() || !publication.Valid() {
		t.Fatal("наблюдение состояния соседа отозвало действующие допуски")
	}
}
