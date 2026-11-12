package integration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/session"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// recoveryDelayedIngress задерживает только возврат остановки прежнего приёмника после фактического
// завершения вложенного владельца.
type recoveryDelayedIngress struct {
	// IngressOwner содержит настоящего владельца допуска и остановки RTMP.
	session.IngressOwner
	// entered подтверждает возврат вложенного Shutdown; release разрешает обёртке вернуть его
	// результат.
	entered, release chan struct{}
	// once обеспечивает единственное закрытие entered при повторных ожиданиях старого владельца.
	once sync.Once
}

// CloseAdmission без ожидания физического завершения закрывает допуск у вложенного владельца приёма,
// если он поддерживает этот метод.
func (o *recoveryDelayedIngress) CloseAdmission() {
	// owner предоставляет необязательное закрытие допуска; ok отмечает поддержку этого метода
	// вложенным приёмником.
	if owner, ok := o.IngressOwner.(interface {
		// CloseAdmission закрывает допуск входящих публикаций у вложенного владельца приёма RTMP.
		CloseAdmission()
	}); ok {
		owner.CloseAdmission()
	}
}

// Shutdown сначала завершает вложенного владельца приёма и сообщает об этой границе через entered,
// затем задерживает возврат до release либо отмены контекста. Это позволяет проверить поздний ответ
// старого владельца после передачи управления.
func (o *recoveryDelayedIngress) Shutdown(ctx context.Context) error {
	// err сохраняет результат настоящего Shutdown до управляемой задержки возврата.
	err := o.IngressOwner.Shutdown(ctx)
	o.once.Do(func() {
		// Однократно сообщает о возврате Shutdown прежнего приёмника перед дополнительным ожиданием.
		close(o.entered)
	})
	select {
	case <-o.release:
		return err
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
}

// Ожидает синхронизирующее событие не более восьми секунд; при истечении срока завершает тест с
// описанием ожидаемой границы.
func recoveryWaitEvent(t *testing.T, event <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-event:
	case <-time.After(8 * time.Second):
		t.Fatal(description)
	}
}

// Задерживает ответ Shutdown старого владельца приёма после закрытия допуска и запускает следующий
// Manager с той же RuntimeAuthority. Позднее и повторное закрытие старого Manager не должно отзывать
// новую Session, Feed, удержания или исходящее соединение.
func TestSessionManagerOldCloseCannotRevokeSuccessor(t *testing.T) {
	// helper содержит настоящий helper для старого менеджера и его преемника.
	helper := sessionAcceptanceBuildHelper(t)
	// receiver содержит RTMP-приёмник передачи до и после смены менеджера.
	receiver, _ := newPublisherReceiver(t, false)
	// f содержит общие SQLite, файлы и RuntimeAuthority обоих последовательных менеджеров.
	f := newSessionAcceptanceFixture(t, helper, []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	// delayed оборачивает прежнего владельца приёма и удерживает только ответ его физического
	// завершения.
	delayed := &recoveryDelayedIngress{IngressOwner: f.config.Ingress, entered: make(chan struct{}), release: make(chan struct{})}
	f.config.Ingress = delayed
	f.launch(t)
	// old сохраняет прежний Manager для поздних и повторных Close после появления преемника.
	old := f.manager
	// release обеспечивает однократное освобождение задержанного Shutdown в основной проверке и
	// очистке.
	var release sync.Once
	t.Cleanup(func() {
		// Освобождает задержанный Shutdown и окончательно завершает прежний SessionManager после проверки
		// преемника.

		release.Do(func() {
			// Однократно разрешает возврат Shutdown прежнего приёмника при освобождении теста.
			close(delayed.release)
		})
		// ctx ограничивает окончательное присоединение старого менеджера восемью секундами;
		// cancel освобождает таймер.
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		// err содержит результат позднего закрытия старого менеджера при очистке.
		if err := old.Close(ctx); err != nil {
			t.Error("окончательное присоединение старого Manager:", err)
		}
	})
	// previous содержит RUNNING Session прежнего менеджера для проверки отзыва его Feed.
	previous := f.running(t, f.start(t, "start-before-manager-replacement"))
	receiver.wait(t, func(m []publisherWireMessage) bool {
		// Ожидает начальную передачу прежнего SessionManager.
		return publisherMediaCount(m) >= 8
	})
	// closed передаёт результат отдельного Close старого менеджера после разрешения возврата
	// Shutdown прежнего приёмника.
	closed := make(chan error, 1)
	go func() {
		// Вызывает Close прежнего менеджера отдельно, чтобы проверить запуск преемника до возврата физического
		// ожидания.
		closed <- old.Close(context.Background())
	}()
	recoveryWaitEvent(t, delayed.entered, "старый ingress не начал физическое завершение")
	// err содержит результат Feed прежней Session после закрытия допуска старого менеджера.
	if _, err := old.Feed(previous); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("старый Manager сохранил допуск Feed: %v", err)
	}
	// Прежний ingress уже закрыл допуск и соединения; задержан только ответ физического завершения.
	f.configureOwners(t, helper)
	sessionAcceptanceWait(t, func() bool {
		// Повторяет создание нового SessionManager до освобождения исключительного места управления прежним.

		// manager содержит создаваемого преемника; err содержит возможный core.ErrInUse до
		// освобождения места управления.
		manager, err := session.New(context.Background(), f.config)
		if errors.Is(err, core.ErrInUse) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		f.manager = manager
		return true
	})
	t.Cleanup(func() {
		// Закрывает SessionManager преемника после проверки изоляции позднего Close.

		// ctx ограничивает остановку преемника при очистке восемью секундами; cancel освобождает
		// таймер.
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		// err содержит результат закрытия менеджера-преемника при очистке.
		if err := f.manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	select {
	// err содержит преждевременный результат старого Close, которого до release быть не должно.
	case err := <-closed:
		t.Fatalf("старый Close завершился до разрешения физического ожидания: %v", err)
	default:
	}
	// current содержит RUNNING Session, уже принадлежащую новому менеджеру.
	current := f.running(t, f.start(t, "start-under-successor-manager"))
	// feed содержит Router новой Session; err содержит отказ получения для сравнения после старых
	// Close.
	feed, err := f.manager.Feed(current)
	if err != nil {
		t.Fatal(err)
	}
	sessionAcceptanceAssertHolds(t, f, current.ID, true)
	// before сохраняет число медиасообщений перед проверкой продолжения передачи преемника.
	before := publisherMediaCount(receiver.snapshot())
	receiver.wait(t, func(m []publisherWireMessage) bool {
		// Проверяет передачу новой Session до освобождения задержанного прежнего Close.
		return publisherMediaCount(m) >= before+8
	})
	// connections сохраняет число принятых соединений новой Session до позднего Close прежнего
	// менеджера.
	connections := receiver.accepted.Load()
	release.Do(func() {
		// Однократно освобождает возврат Shutdown прежнего приёмника в основной проверке.
		close(delayed.release)
	})
	select {
	case err = <-closed:
		if err != nil {
			t.Fatal("первое закрытие старого Manager:", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("старый Close не завершился после разрешения ожидания")
	}
	for range 2 {
		if err = old.Close(context.Background()); err != nil {
			t.Fatal("повторное закрытие старого Manager:", err)
		}
		// after содержит новую Session после очередного старого Close; err содержит отказ её
		// чтения.
		after, err := f.service.GetSession(context.Background(), current.ID)
		if err != nil || after.State != core.SessionRunning || !after.Issued || after.Lifecycle != current.Lifecycle {
			t.Fatalf("старый Close отозвал новое исполнение: %+v %v", after, err)
		}
		// actual содержит Feed преемника после старого Close; err содержит отказ его получения.
		if actual, err := f.manager.Feed(after); err != nil || actual != feed {
			t.Fatalf("старый Close заменил Feed: %v", err)
		}
		sessionAcceptanceAssertHolds(t, f, current.ID, true)
	}
	before = publisherMediaCount(receiver.snapshot())
	receiver.wait(t, func(m []publisherWireMessage) bool {
		// Проверяет продолжение медиа преемника после повторных Close прежнего менеджера.
		return publisherMediaCount(m) >= before+8
	})
	if receiver.accepted.Load() != connections {
		t.Fatal("старый Close пересоздал соединение нового исполнения")
	}
}

// Ошибка следующего перехода возникает после настоящей фиксации выбранной стадии в БД.
// Откат транзакции подтверждается до восстановления записи вызывающим тестом.
// recoveryReceiptFaultStore вводит отказ следующей записи квитанции только после успешной фиксации
// выбранной стадии.
type recoveryReceiptFaultStore struct {
	// Store выполняет настоящие транзакции SQLite и предоставляет остальные операции хранилища.
	*storage.Store
	// requestID выбирает единственную квитанцию остановки для управляемого отказа.
	requestID string
	// stage задаёт CLAIMED либо ISSUED, которые должны успешно сохраниться перед отказом следующего
	// перехода.
	stage core.CommandStatus
	// armed атомарно включает отказ только после успешного COMMIT выбранной стадии.
	armed atomic.Bool
	// failed закрывается после возврата ошибки настоящей откатившейся транзакции.
	failed chan struct{}
	// restore разрешает последующую запись квитанции после проверки сохранённой границы.
	restore chan struct{}
	// failedOnce обеспечивает единственное уведомление о фактически полученном отказе транзакции.
	failedOnce sync.Once
	// transitionFault атомарно подтверждает достижение управляемого отказа следующего перехода.
	transitionFault atomic.Bool
}

// Write оборачивает настоящую транзакцию репозиторием с управляемым отказом записи квитанции.
// Сохранённая стадия включает отказ только после успешного COMMIT, а failed закрывается лишь после
// возврата ошибки транзакции.
func (s *recoveryReceiptFaultStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	// stageCommitted отмечает запись выбранной стадии внутри текущей транзакции для включения armed
	// только после её успеха.
	stageCommitted := false
	// faulted отмечает управляемый отказ внутри текущей транзакции для сигнала failed после возврата
	// ошибки.
	faulted := false
	// err содержит результат настоящей транзакции, определяющий фиксацию стадии или подтверждение
	// отката.
	err := s.Store.Write(ctx, func(r core.Repositories) error {
		// Подставляет репозиторий квитанций внутри настоящей транзакции и отслеживает фиксацию выбранной
		// стадии.

		r.Runtime = recoveryReceiptFaultRepository{RuntimeRepository: r.Runtime, store: s, stageCommitted: &stageCommitted, faulted: &faulted}
		return callback(r)
	})
	if err == nil && stageCommitted {
		s.armed.Store(true)
	}
	if faulted && err != nil {
		s.failedOnce.Do(func() {
			// Однократно сообщает тесту о произошедшем отказе после возврата откатившейся транзакции.
			close(s.failed)
		})
	}
	return err
}

// recoveryReceiptFaultRepository оборачивает запись квитанций в одной настоящей транзакции, сохраняя
// остальные операции репозитория.
type recoveryReceiptFaultRepository struct {
	// RuntimeRepository выполняет обычные чтения и записи состояния выполнения, не затронутые
	// управляемым отказом.
	core.RuntimeRepository
	// store указывает настройки отказа и синхронизацию оборачивающего хранилища.
	store *recoveryReceiptFaultStore
	// stageCommitted передаёт текущей транзакции факт записи выбранной стадии.
	stageCommitted *bool
	// faulted передаёт текущей транзакции факт внедрённого отказа перехода.
	faulted *bool
}

// UpdateReceipt пропускает выбранную стадию квитанции, а после её фиксации задерживает следующий
// переход до отмены контекста либо restore. До восстановления отмечает внедрённый отказ; прочие
// квитанции передаёт настоящему репозиторию.
func (r recoveryReceiptFaultRepository) UpdateReceipt(ctx context.Context, value core.CommandReceipt, revision uint64) error {
	// s содержит обёртку хранилища для проверки RequestID, стадии и барьера восстановления.
	s := r.store
	if value.RequestID == s.requestID {
		if value.Status == s.stage {
			*r.stageCommitted = true
		} else if s.armed.Load() {
			select {
			case <-s.restore:
			default:
				*r.faulted = true
				s.transitionFault.Store(true)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-s.restore:
				}
			}
		}
	}
	return r.RuntimeRepository.UpdateReceipt(ctx, value, revision)
}

// После фиксации CLAIMED либо ISSUED вызывает откатившийся отказ следующей записи остановки и
// проверяет сохранённую границу. Восстановление завершает ту же попытку команды и позволяет новый
// ручной запуск с независимыми удержаниями и передачей медиа.
func TestSessionManagerStopRecoversAfterPersistedReceiptWriteFailure(t *testing.T) {
	// helper содержит настоящий helper сценариев восстановления записи остановки.
	helper := sessionAcceptanceBuildHelper(t)
	// stage выбирает сохранённую границу CLAIMED или ISSUED перед откатившимся переходом.
	for _, stage := range []core.CommandStatus{core.CommandClaimed, core.CommandIssued} {
		t.Run(string(stage), func(t *testing.T) {
			// Проверяет восстановление остановки после отказа записи следующего перехода за выбранной сохранённой
			// стадией команды.

			// receiver содержит RTMP-приёмник старого и нового ручного запусков.
			receiver, _ := newPublisherReceiver(t, false)
			// f содержит изолированный менеджер с заменяемым транзакционным хранилищем.
			f := newSessionAcceptanceFixture(t, helper, []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
			// fault задаёт точный RequestID, выбранную стадию и барьер временного отказа
			// записи.
			fault := &recoveryReceiptFaultStore{Store: f.store, requestID: "stop-with-temporary-write-failure", stage: stage, failed: make(chan struct{}), restore: make(chan struct{})}
			// service содержит доменный сервис поверх управляемого Store; err содержит отказ
			// создания.
			service, err := core.NewService(fault, f.store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			f.service = service
			f.configureOwners(t, helper)
			f.launch(t)
			// restore обеспечивает единственное снятие барьера записи при проверке и
			// очистке.
			var restore sync.Once
			t.Cleanup(func() {
				// Снимает барьер восстановления записи при освобождении теста.

				restore.Do(func() {
					// Однократно разрешает дальнейшую запись квитанции после контролируемого отказа.
					close(fault.restore)
				})
			})
			// current содержит RUNNING Session перед восстановимой остановкой.
			current := f.running(t, f.start(t, "start-before-recoverable-stop"))
			receiver.wait(t, func(m []publisherWireMessage) bool {
				// Ожидает начальное медиа перед отказом записи команды остановки.
				return publisherMediaCount(m) >= 8
			})
			// stop содержит принятую команду остановки, чья следующая запись должна временно
			// откатиться.
			stop, err := f.manager.StopSession(context.Background(), core.SessionCommand{RequestID: fault.requestID, SessionID: current.ID})
			if err != nil {
				t.Fatal(err)
			}
			recoveryWaitEvent(t, fault.failed, "ошибка записи после сохранённой стадии не воспроизвелась")
			// persisted содержит квитанцию после отката для проверки сохранённой выбранной
			// стадии и той же попытки.
			persisted, err := f.service.GetReceipt(context.Background(), stop.ID)
			if err != nil || persisted.Status != stage || !fault.transitionFault.Load() {
				t.Fatalf("не подтверждена граница rollback: %+v %v", persisted, err)
			}
			// stopping содержит Session после откатившегося перехода, которая должна
			// оставаться STOPPING.
			stopping, err := f.service.GetSession(context.Background(), current.ID)
			if err != nil || stopping.State != core.SessionStopping {
				t.Fatalf("неудачная запись сохранила терминальный переход: %+v %v", stopping, err)
			}
			restore.Do(func() {
				// Разрешает повтор записи после проверки сохранённой стадии и отката неудачного перехода.
				close(fault.restore)
			})
			// completed содержит успешно завершённую после восстановления квитанцию той же
			// попытки.
			completed := sessionAcceptanceReceipt(t, f.service, stop.ID, core.CommandSucceeded)
			if completed.Attempt != persisted.Attempt {
				t.Fatalf("восстановление создало новую попытку команды: %d -> %d", persisted.Attempt, completed.Attempt)
			}
			// previous содержит STOPPED Session с сохранённым временем завершения после
			// восстановления.
			previous, err := f.service.GetSession(context.Background(), current.ID)
			if err != nil || previous.State != core.SessionStopped || previous.EndedAt == nil {
				t.Fatalf("восстановленная остановка не завершила Session: %+v %v", previous, err)
			}
			// fresh содержит новый независимый ручной запуск в том же экземпляре хранилища
			// после успешной остановки.
			fresh := f.running(t, f.start(t, "manual-start-after-recovered-stop"))
			if fresh.ID == current.ID || fresh.Incarnation != current.Incarnation {
				t.Fatal("новый ручной запуск не получил независимую Session той же incarnation")
			}
			sessionAcceptanceAssertHolds(t, f, fresh.ID, true)
			// before сохраняет число медиасообщений перед проверкой передачи нового ручного
			// запуска.
			before := publisherMediaCount(receiver.snapshot())
			receiver.wait(t, func(m []publisherWireMessage) bool {
				// Проверяет передачу новой ручной Session после успешного восстановления остановки.
				return publisherMediaCount(m) >= before+8
			})
		})
	}
}
