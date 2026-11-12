package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// Сбой откатывает только переход прежней Session; подтверждение очистки имеет отдельный барьер.
type ownerLossFaultStore struct {
	// Store выполняет настоящие операции хранения, кроме явно подменённых записей Session и
	// CleanupIntent.
	*storage.Store
	// mu защищает выбор прежней сессии и признак управляемого отказа от параллельных операций
	// менеджера.
	mu sync.Mutex
	// old задаёт идентификатор прежней сессии, записи которой задерживаются тестовыми барьерами.
	old core.ID
	// retirement включает управляемый ErrBusy при выходе прежней сессии из RUNNING.
	retirement bool
	// retirementFailed сообщает о попытке заблокированного терминального перехода, а healed
	// разрешает его последующие записи после закрытия канала.
	retirementFailed, healed chan struct{}
	// acknowledgement разрешает подтверждение очистки прежней сессии после закрытия канала, а
	// ackTried сообщает о заблокированной попытке такого подтверждения.
	acknowledgement, ackTried chan struct{}
}

// Выполняет настоящую транзакцию с подстановками репозиториев, отдельно управляющими отказом
// завершения сессии и подтверждения её очистки.
func (s *ownerLossFaultStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	return s.Store.Write(ctx, func(r core.Repositories) error {
		// Подставляет контролируемые репозитории перехода Session и подтверждения очистки внутри настоящей
		// транзакции.

		r.Runtime = ownerLossRuntimeRepository{RuntimeRepository: r.Runtime, store: s}
		r.Artifacts = ownerLossArtifactRepository{ArtifactRepository: r.Artifacts, store: s}
		return callback(r)
	})
}

// ownerLossRuntimeRepository задерживает только терминальную запись прежней сессии, передавая остальные
// операции настоящему репозиторию.
type ownerLossRuntimeRepository struct {
	// RuntimeRepository выполняет штатные операции состояния выполнения вне управляемого отказа.
	core.RuntimeRepository
	// store предоставляет общий выбор прежней сессии, блокировку и каналы управляемого отказа
	// записи.
	store *ownerLossFaultStore
}

// Возвращает ErrBusy при переходе прежней сессии из RUNNING, пока не снят управляемый барьер отказа;
// остальные записи передаёт настоящему репозиторию.
func (r ownerLossRuntimeRepository) UpdateSession(ctx context.Context, value core.Session, revision uint64) error {
	r.store.mu.Lock()
	// blocked показывает, относится ли запись к выходу прежней сессии из RUNNING при включённом
	// управляемом отказе.
	blocked := value.ID == r.store.old && r.store.retirement && value.State != core.SessionRunning
	r.store.mu.Unlock()
	if blocked {
		select {
		case <-r.store.healed:
		default:
			select {
			case r.store.retirementFailed <- struct{}{}:
			default:
			}
			return core.ErrBusy
		}
	}
	return r.RuntimeRepository.UpdateSession(ctx, value, revision)
}

// ownerLossArtifactRepository задерживает подтверждение очистки прежнего выполнения до снятия отдельного
// тестового барьера.
type ownerLossArtifactRepository struct {
	// ArtifactRepository выполняет штатные операции артефактов и незадержанных намерений очистки.
	core.ArtifactRepository
	// store предоставляет идентификатор прежней сессии и барьеры подтверждения её очистки.
	store *ownerLossFaultStore
}

// Блокирует подтверждение очистки исполнения прежней сессии до освобождения отдельного барьера, не
// затрагивая остальные намерения очистки.
func (r ownerLossArtifactRepository) UpdateCleanup(ctx context.Context, value core.CleanupIntent, revision uint64) error {
	r.store.mu.Lock()
	// old фиксирует под блокировкой идентификатор сессии, подтверждение очистки которой
	// задерживается.
	old := r.store.old
	r.store.mu.Unlock()
	if value.SessionID == old && value.Kind == core.CleanupRuntime && value.Acknowledged {
		select {
		case <-r.store.acknowledgement:
		default:
			select {
			case r.store.ackTried <- struct{}{}:
			default:
			}
			return core.ErrBusy
		}
	}
	return r.ArtifactRepository.UpdateCleanup(ctx, value, revision)
}

// Собирает точный ExecutionFence из идентификаторов и номера попытки квитанции без чтения текущего
// исполнения.
func ownerLossFence(receipt core.CommandReceipt) core.ExecutionFence {
	return core.ExecutionFence{CommandID: receipt.ID, Incarnation: receipt.Incarnation, SessionID: receipt.SessionID, Lifecycle: receipt.Lifecycle, OutputID: receipt.OutputID, OutputGeneration: receipt.OutputGeneration, Attempt: receipt.Attempt}
}

// Независимо сверяет SQL-удержания заданной сессии с её точными ресурсами. Для сессии без выходов
// дополнительно проверяет отсутствие удержаний выходов.
func ownerLossAssertHolds(t *testing.T, f *sessionAcceptanceFixture, id core.ID, withOutput, retained bool) {
	t.Helper()
	if withOutput {
		sessionAcceptanceAssertHolds(t, f, id, retained)
		return
	}
	// dsn задаёт независимое подключение только для чтения к SQLite фикстуры с ожиданием блокировки
	// до 1000 мс.
	dsn := url.URL{Scheme: "file", Path: filepath.Join(f.directory, "state.sqlite"), RawQuery: "mode=ro&_busy_timeout=1000"}
	// db предоставляет независимое SQL-чтение удержаний сессии без выходов, а err сообщает об ошибке
	// его открытия.
	db, err := sql.Open("sqlite3", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает соединение чтения SQLite после проверки точных удержаний Session.

		// err сообщает об ошибке закрытия независимого SQL-подключения проверки удержаний.
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	// ctx ограничивает SQL-проверки удержаний двумя секундами, а cancel освобождает их таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// parent содержит число точных родительских удержаний сессии, а outputs — число всех её
	// удержаний выходов.
	var parent, outputs int
	err = db.QueryRowContext(ctx, "SELECT count(*) FROM session_holds WHERE session_id = ? AND stream_id = ? AND asset_id = ? AND generation_id = ?", id, f.stream.ID, f.asset.ID, f.generation.ID).Scan(&parent)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM output_holds WHERE session_id = ?", id).Scan(&outputs); err != nil {
		t.Fatal(err)
	}
	// want задаёт ожидаемое число родительских удержаний: одно до подтверждения очистки либо ноль
	// после него.
	want := 0
	if retained {
		want = 1
	}
	if parent != want || outputs != 0 {
		t.Fatalf("точные удержания Session без выходов: parent=%d outputs=%d, ожидалось parent=%d outputs=0", parent, outputs, want)
	}
}

// Проверяет долговечное завершение после отзыва точного владельца, включая откат БД и задержку
// подтверждения очистки. Старые удержания сохраняются до подтверждения, а поздние действия старого
// владельца не меняют новый ручной запуск.
func TestSessionManagerOwnerLossRetiresDurablyAndIsolatesNewManualStart(t *testing.T) {
	// helper содержит путь настоящего вспомогательного процесса владения для вариантов сценария
	// отзыва владельца.
	helper := sessionAcceptanceBuildHelper(t)
	// withOutput выбирает сессию без выходов либо с одним публикующим выходом.
	for _, withOutput := range []bool{false, true} {
		// unavailable включает управляемый отказ долговечного терминального перехода прежней
		// сессии.
		for _, unavailable := range []bool{false, true} {
			t.Run(fmt.Sprintf("output-%v-retirement-unavailable-%v", withOutput, unavailable), func(t *testing.T) {
				// Проверяет отзыв владельца с выбранными наличием выхода и отказом долговечного завершения прежней
				// Session.

				// endpoints содержит адрес назначения только для сценария с публикующим
				// выходом.
				var endpoints []string
				// indices связывает единственное назначение с первой группой при
				// включённом выходе.
				var indices []int
				// receiver принимает публикацию прежней и новой сессий для проверки
				// изоляции их соединений.
				receiver, _ := newPublisherReceiver(t, false)
				if withOutput {
					endpoints, indices = []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0}
				}
				// f предоставляет рабочие ресурсы и менеджер для выбранного сценария
				// потери владельца.
				f := newSessionAcceptanceFixture(t, helper, endpoints, indices)
				// fault подменяет только запись прежней сессии и подтверждение её
				// очистки управляемыми барьерами.
				fault := &ownerLossFaultStore{Store: f.store, retirement: unavailable, retirementFailed: make(chan struct{}, 1), healed: make(chan struct{}), acknowledgement: make(chan struct{}), ackTried: make(chan struct{}, 1)}
				// healOnce защищает снятие барьера терминального перехода от повторного
				// закрытия, а acknowledgeOnce — снятие барьера очистки.
				var healOnce, acknowledgeOnce sync.Once
				// heal однократно разрешает долговечный терминальный переход прежней
				// сессии после воспроизведённого отказа.
				heal := func() {
					// Однократно разрешает долговечный переход прежней Session после контролируемого отказа.

					healOnce.Do(func() {
						// Закрывает канал восстановления записи терминального состояния прежней Session.
						close(fault.healed)
					})
				}
				// acknowledge однократно разрешает подтверждение очистки прежней сессии
				// после проверки удержаний нового запуска.
				acknowledge := func() {
					// Однократно разрешает подтверждение очистки прежней Session.

					acknowledgeOnce.Do(func() {
						// Закрывает канал допуска подтверждения очистки прежней Session.
						close(fault.acknowledgement)
					})
				}
				// service использует реальные транзакции с управляемыми отказами двух
				// репозиториев, а err сообщает об ошибке его создания.
				service, err := core.NewService(fault, f.store.Incarnation())
				if err != nil {
					t.Fatal(err)
				}
				f.service = service
				f.configureOwners(t, helper)
				f.launch(t)
				t.Cleanup(func() {
					// Снимает оба барьера записи перед окончательным освобождением фикстуры.
					heal()
					acknowledge()
				})
				// start содержит квитанцию запуска прежней сессии до отзыва владельца.
				start := f.start(t, "start-before-owner-loss")
				// old содержит работающее прежнее выполнение сессии, к которому
				// применяется отзыв точного владельца.
				old := f.running(t, start)
				// original фиксирует успешную квитанцию прежнего запуска для проверки её
				// неизменности при последующем завершении.
				original := sessionAcceptanceReceipt(t, service, start.ID, core.CommandSucceeded)
				if withOutput {
					receiver.wait(t, func(messages []publisherWireMessage) bool {
						// Ожидает начальные медиапакеты выхода прежней Session.
						return publisherMediaCount(messages) >= 8
					})
				}
				ownerLossAssertHolds(t, f, old.ID, withOutput, true)
				// lease предоставляет точное право действий успешного родительского
				// выполнения, которое тест отзывает.
				lease, err := service.BindExecutionOwner(context.Background(), ownerLossFence(original))
				if err != nil || !lease.Valid() {
					t.Fatalf("нет действующего точного владельца успешного parent: %v", err)
				}
				fault.mu.Lock()
				fault.old = old.ID
				fault.mu.Unlock()
				if err = lease.Revoke(context.Background()); err != nil {
					t.Fatal(err)
				}
				recoveryWaitEvent(t, lease.Done(), "точный owner не отозван")
				if _, err = f.manager.Feed(old); err == nil {
					t.Fatal("Feed сохранил допуск после отзыва owner")
				}
				if unavailable {
					recoveryWaitEvent(t, fault.retirementFailed, "не воспроизведена ошибка долговечного завершения")
					// persisted содержит SQL-состояние прежней сессии после
					// управляемого отказа записи, а readErr сообщает об ошибке его
					// чтения.
					persisted, readErr := service.GetSession(context.Background(), old.ID)
					if readErr != nil || persisted.State != core.SessionRunning || !persisted.Issued {
						t.Fatalf("сбой не сохранил прежнюю SQL-границу: %+v %v", persisted, readErr)
					}
					ownerLossAssertHolds(t, f, old.ID, withOutput, true)
					// rejected содержит результат запрещённого нового запуска при
					// ещё активной SQL-сессии, а startErr должен подтверждать
					// занятость потока.
					if rejected, startErr := f.manager.StartSession(context.Background(), core.StartSessionRequest{RequestID: "reject-while-old-sql-slot-active", StreamID: f.stream.ID}); !errors.Is(startErr, core.ErrInUse) || rejected.ID != "" {
						t.Fatalf("неподтверждённая retirement освободила активный SQL slot: %+v %v", rejected, startErr)
					}
					heal()
				}
				// terminal сохраняет долговечное терминальное состояние прежней сессии с
				// ещё неподтверждённой очисткой.
				var terminal core.Session
				sessionAcceptanceWait(t, func() bool {
					// Ожидает долговечное терминальное состояние прежней Session после отзыва владельца.

					terminal, err = service.GetSession(context.Background(), old.ID)
					return err == nil && terminal.State.Terminal()
				}, func() {
					// Сохраняет диагностику при неудаче ожидания терминального перехода.
					f.captureFailure(t)
				})
				if terminal.EndedAt == nil || !terminal.Issued || !terminal.CleanupPending {
					t.Fatalf("retirement скрыла незавершённую очистку: %+v", terminal)
				}
				ownerLossAssertHolds(t, f, old.ID, withOutput, true)
				// receipt содержит перечитанную успешную квитанцию прежнего запуска, а
				// readErr сообщает об ошибке её чтения после завершения владельца.
				if receipt, readErr := service.GetReceipt(context.Background(), original.ID); readErr != nil || !reflect.DeepEqual(receipt, original) {
					t.Fatalf("retirement переписала успешный start: %+v %v", receipt, readErr)
				}
				// fresh содержит новый ручной запуск после долговечного завершения
				// прежней сессии.
				fresh := f.running(t, f.start(t, "manual-start-after-owner-loss"))
				if fresh.ID == old.ID || fresh.Incarnation != old.Incarnation {
					t.Fatal("новый ручной start не создал независимую Session той же incarnation")
				}
				// feed сохраняет медиавход нового выполнения для проверки его
				// независимости от поздних действий прежнего владельца.
				feed, err := f.manager.Feed(fresh)
				if err != nil {
					t.Fatal(err)
				}
				ownerLossAssertHolds(t, f, old.ID, withOutput, true)
				ownerLossAssertHolds(t, f, fresh.ID, withOutput, true)
				// before фиксирует число принятых медиасообщений перед ожиданием
				// продолжения нового выполнения.
				before := publisherMediaCount(receiver.snapshot())
				if withOutput {
					receiver.wait(t, func(messages []publisherWireMessage) bool {
						// Проверяет передачу новой Session при ещё удерживаемых ресурсах прежней.
						return publisherMediaCount(messages) >= before+8
					})
				}
				// connections фиксирует число соединений нового выполнения до снятия
				// барьера прежней очистки.
				connections := receiver.accepted.Load()
				// Повторный отзыв старого токена и запоздавшее действие не получают права на новое исполнение.
				// called отмечает выполнение функции обратного вызова, которую
				// отозванный прежний lease обязан отклонить.
				called := false
				if err = lease.Do(context.Background(), func() error {
					// Отмечает вызов действия, которое отозванный токен не должен допустить.
					called = true
					return nil
				}); !errors.Is(err, core.ErrStale) || called {
					t.Fatalf("старый callback получил допуск: called=%v err=%v", called, err)
				}
				if err = lease.Revoke(context.Background()); err != nil {
					t.Fatal(err)
				}
				acknowledge()
				sessionAcceptanceWait(t, func() bool {
					// Ожидает подтверждение очистки точной прежней Session после снятия отдельного барьера.

					// previous содержит прежнюю сессию после снятия барьера очистки,
					// а readErr сообщает об ошибке чтения при ожидании
					// подтверждения.
					previous, readErr := service.GetSession(context.Background(), old.ID)
					return readErr == nil && !previous.CleanupPending && !previous.Issued
				}, func() {
					// Сохраняет диагностику при задержке подтверждения очистки прежней Session.
					f.captureFailure(t)
				})
				ownerLossAssertHolds(t, f, old.ID, withOutput, false)
				ownerLossAssertHolds(t, f, fresh.ID, withOutput, true)
				// after содержит состояние нового выполнения после позднего
				// подтверждения прежней очистки.
				after, err := service.GetSession(context.Background(), fresh.ID)
				if err != nil || after.State != core.SessionRunning || !after.Issued || after.Lifecycle != fresh.Lifecycle {
					t.Fatalf("поздняя retirement изменила новую Session: %+v %v", after, err)
				}
				// current содержит актуальный медиавход новой сессии, а feedErr сообщает
				// об ошибке его получения при сравнении с feed.
				if current, feedErr := f.manager.Feed(after); feedErr != nil || current != feed {
					t.Fatalf("поздняя retirement изменила новый Feed: %v", feedErr)
				}
				before = publisherMediaCount(receiver.snapshot())
				if withOutput {
					receiver.wait(t, func(messages []publisherWireMessage) bool {
						// Проверяет продолжение новой Session после запоздалой очистки прежнего исполнения.
						return publisherMediaCount(messages) >= before+8
					})
				}
				if receiver.accepted.Load() != connections {
					t.Fatal("поздняя retirement пересоздала соединение нового выполнения")
				}
				// snapshotErr показывает недоступность оперативного снимка прежнего
				// выполнения после его очистки.
				if _, snapshotErr := service.RuntimeSnapshot(context.Background(), core.RuntimeSessionFence{SessionID: old.ID, Incarnation: old.Incarnation, Lifecycle: old.Lifecycle}); !errors.Is(snapshotErr, core.ErrNotReady) {
					t.Fatalf("retirement сохранила старую телеметрию: %v", snapshotErr)
				}
			})
		}
	}
}
