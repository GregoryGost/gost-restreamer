package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// receiptFaultStore моделирует потерянное подтверждение выбранной стадии команды и последующую временную
// недоступность БД.
type receiptFaultStore struct {
	// Store выполняет настоящие транзакции под обёрткой управляемого отказа.
	*storage.Store
	// mu защищает состояние инъекции, диагностические события и родительский допуск.
	mu sync.Mutex
	// kind выбирает вид команды, на котором включается отказ.
	kind core.CommandKind
	// stage выбирает стадию квитанции, при достижении которой включается отказ.
	stage core.CommandStatus
	// injected отмечает включённую недоступность после выбранного перехода команды.
	injected bool
	// healed разрешает последующим обращениям снова проходить к настоящему хранилищу.
	healed chan struct{}
	// blocked сообщает о запросах, задержанных искусственной недоступностью.
	blocked chan struct{}
	// expired сообщает о завершении задержанного запроса по его контексту.
	expired chan struct{}
	// parent сохраняет родительский допуск для контроля изоляции отказа назначения.
	parent *core.RuntimeLease
	// events хранит не более 512 строк диагностики запросов и действительности родителя.
	events []string
	// dropped считает диагностические события, не помещённые в ограниченную трассу.
	dropped int
}

// trace сохраняет до 512 диагностических событий с состоянием контекста и родительского допуска;
// последующие события учитывает счётчиком переполнения.
func (s *receiptFaultStore) trace(ctx context.Context, stage string, began time.Time, before bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// remaining содержит остаток срока диагностируемого запроса или ноль при отсутствии срока.
	remaining := time.Duration(0)
	// deadline задаёт крайний срок запроса; ok отмечает наличие этого срока в контексте.
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	if len(s.events) == 512 {
		s.dropped++
		return
	}
	// done отмечает закрытый сигнал завершения сохранённого родительского допуска.
	done := false
	if s.parent != nil {
		select {
		case <-s.parent.Done():
			done = true
		default:
		}
	}
	s.events = append(s.events, fmt.Sprintf("time=%s stage=%s elapsed=%s remaining=%s context=%v err=%v parent_before=%v parent_after=%v parent_done=%v injected=%v", time.Now().UTC().Format(time.RFC3339Nano), stage, time.Since(began), remaining, ctx.Err(), err, before, s.parent.Valid(), done, s.injected))
}

// parentValid читает действительность сохранённого родительского допуска под блокировкой проверочного
// хранилища.
func (s *receiptFaultStore) parentValid() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parent.Valid()
}

// wait после включения отказа задерживает текущий запрос до его собственного срока; восстановление
// открывает только последующие обращения.
func (s *receiptFaultStore) wait(ctx context.Context) error {
	s.mu.Lock()
	// injected содержит снимок признака искусственной недоступности перед ожиданием запроса.
	injected := s.injected
	s.mu.Unlock()
	if !injected {
		return nil
	}
	select {
	case <-s.healed:
		return nil
	default:
	}
	select {
	case s.blocked <- struct{}{}:
	default:
	}
	// Уже недоступный запрос завершается по своему сроку; восстановление открывает последующие запросы.
	<-ctx.Done()
	select {
	case s.expired <- struct{}{}:
	default:
	}
	return ctx.Err()
}

// Write выполняет запись через репозиторий с управляемым отказом, сохраняет диагностику и может
// вернуть ErrBusy после достижения заданной стадии квитанции.
func (s *receiptFaultStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	// began отмечает начало диагностируемой записи; before сохраняет действительность родительского
	// допуска до операции.
	began, before := time.Now(), s.parentValid()
	// err сообщает завершение искусственно задержанной записи по её контексту.
	if err := s.wait(ctx); err != nil {
		s.trace(ctx, "wrapper_write_wait", began, before, err)
		return err
	}
	began, before = time.Now(), s.parentValid()
	// reached отмечает достижение выбранной стадии команды внутри настоящей записи.
	var reached bool
	// err содержит результат настоящей записи перед возможной потерей её подтверждения.
	err := s.Store.Write(ctx, func(r core.Repositories) error {
		// Подставляет репозиторий, отмечающий достижение выбранной стадии квитанции, и выполняет исходную
		// запись.

		r.Runtime = receiptFaultRepository{RuntimeRepository: r.Runtime, store: s, reached: &reached}
		return callback(r)
	})
	s.trace(ctx, "underlying_write", began, before, err)
	if err != nil || !reached {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.injected {
		s.injected = true
		return core.ErrBusy
	}
	return nil
}

// View моделирует недоступное чтение после включения отказа и фиксирует ошибки или изменение
// родительского допуска при настоящем чтении.
func (s *receiptFaultStore) View(ctx context.Context, callback func(core.Repositories) error) error {
	// began отмечает начало диагностируемого чтения; before сохраняет действительность родительского
	// допуска до операции.
	began, before := time.Now(), s.parentValid()
	// err сообщает завершение искусственно задержанного чтения по его контексту.
	if err := s.wait(ctx); err != nil {
		s.trace(ctx, "wrapper_view_wait", began, before, err)
		return err
	}
	began, before = time.Now(), s.parentValid()
	// err содержит результат настоящего чтения для диагностики отказа либо изменения родительского
	// допуска.
	err := s.Store.View(ctx, callback)
	if err != nil || before != s.parentValid() {
		s.trace(ctx, "underlying_view", began, before, err)
	}
	return err
}

// receiptFaultRepository отмечает выбранный переход квитанции либо отклоняет запись успеха до COMMIT.
type receiptFaultRepository struct {
	// RuntimeRepository выполняет настоящие операции выполнения под проверочной обёрткой.
	core.RuntimeRepository
	// store предоставляет условия инъекции и её синхронизированное состояние.
	store *receiptFaultStore
	// reached указывает флаг достижения выбранной стадии в текущей транзакции.
	reached *bool
}

// UpdateReceipt отклоняет первую запись успеха выбранной команды либо отмечает достижение заданной
// стадии после настоящего обновления квитанции.
func (r receiptFaultRepository) UpdateReceipt(ctx context.Context, c core.CommandReceipt, revision uint64) error {
	if c.Kind == r.store.kind && c.Status == core.CommandSucceeded && r.store.stage == core.CommandSucceeded {
		r.store.mu.Lock()
		if !r.store.injected {
			r.store.injected = true
			r.store.mu.Unlock()
			return core.ErrBusy
		}
		r.store.mu.Unlock()
	}
	// err сообщает отказ настоящего обновления квитанции до отметки выбранной стадии.
	if err := r.RuntimeRepository.UpdateReceipt(ctx, c, revision); err != nil {
		return err
	}
	if c.Kind == r.store.kind && c.Status == r.store.stage {
		*r.reached = true
	}
	return nil
}

// TestManagerRetainsControlAcrossUnavailableDatabase проверяет продолжение остановки после временной
// недоступности БД на стадиях CLAIMED, ISSUED и завершения и освобождение активного слота для
// следующего запуска.
func TestManagerRetainsControlAcrossUnavailableDatabase(t *testing.T) {
	// stage выбирает CLAIMED, ISSUED либо завершение команды остановки для отказа БД.
	for _, stage := range []core.CommandStatus{core.CommandClaimed, core.CommandIssued, core.CommandSucceeded} {
		t.Run(string(stage), func(t *testing.T) {
			// Проверяет сохранение управляющей команды при недоступности БД на выбранной стадии выполнения.

			// f предоставляет сессию без назначений для проверки продолжения управляющей
			// команды.
			f := newManagerFixture(t, false, nil)
			// err сообщает отказ закрытия исходного менеджера перед подменой хранилища.
			if err := f.manager.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			// fault включает недоступность на выбранной стадии остановки и сообщает об
			// истечении запросов.
			fault := &receiptFaultStore{Store: f.store, kind: core.CommandStopSession, stage: stage, healed: make(chan struct{}), blocked: make(chan struct{}, 32), expired: make(chan struct{}, 32)}
			// healed защищает однократное восстановление обращений к БД.
			var healed sync.Once
			// heal разрешает последующим запросам снова обращаться к настоящей БД.
			heal := func() {
				// Восстанавливает доступ последующих запросов к БД с защитой от повторного закрытия канала.

				healed.Do(func() {
					// Однократно закрывает канал восстановления проверочного хранилища.
					close(fault.healed)
				})
			}
			t.Cleanup(heal)
			// service предоставляет доменные операции через обёртку отказа; err сообщает
			// отказ его создания.
			service, err := core.NewService(fault, f.store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			// c содержит настройки с коротким сроком отдельной операции БД.
			c := f.config
			c.Core = service
			c.OperationTimeout = 40 * time.Millisecond
			// manager содержит диспетчер, продолжающий управляющую команду после временной
			// недоступности.
			manager, err := New(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Восстанавливает доступ к БД и закрывает Manager после проверки продолжения команды.

				heal()
				// err сообщает отказ закрытия диспетчера после восстановления БД при
				// очистке теста.
				if err := manager.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			// start содержит принятую команду запуска перед проверяемой остановкой.
			start, err := manager.StartSession(context.Background(), core.StartSessionRequest{RequestID: "fault-start", StreamID: f.stream.ID})
			if err != nil {
				t.Fatal(err)
			}
			// current содержит успешный результат исходного запуска.
			if current := waitReceipt(t, f, start); current.Status != core.CommandSucceeded {
				t.Fatal(current)
			}
			// stopped содержит принятую остановку, выполнение которой пересекает отказ БД.
			stopped, err := manager.StopSession(context.Background(), core.SessionCommand{RequestID: "fault-stop", SessionID: start.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			// ctx ограничивает ожидание отказов и нового запуска тремя секундами; cancel
			// освобождает этот срок.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			for range 4 {
				select {
				case <-fault.expired:
				case <-ctx.Done():
					t.Fatal("не достигнут срок недоступной операции", ctx.Err())
				}
			}
			heal()
			// current содержит завершение остановки после восстановления БД.
			if current := waitReceipt(t, f, stopped); current.Status != core.CommandSucceeded {
				t.Fatal(current)
			}
			// next содержит следующий запуск для проверки освобождения активного слота.
			next, err := manager.StartSession(ctx, core.StartSessionRequest{RequestID: "fault-next", StreamID: f.stream.ID})
			if err != nil {
				t.Fatal("предыдущая команда сохранила активный слот", err)
			}
			// current содержит результат следующего запуска после восстановленной остановки.
			if current := waitReceipt(t, f, next); current.Status != core.CommandSucceeded {
				t.Fatal(current)
			}
		})
	}
}

// delayedIngress моделирует входной приём, завершение которого требует отдельного разрешения теста.
type delayedIngress struct {
	// entered сообщает о входе в ожидание завершения приёма.
	entered chan struct{}
	// release разрешает завершить ожидание входного приёма.
	release chan struct{}
	// once ограничивает сигнал входа в Shutdown одним закрытием.
	once sync.Once
}

// CloseAdmission оставляет проверочный приём публикаций OBS без дополнительных действий при закрытии
// допуска; задерживается только ожидание Shutdown.
func (o *delayedIngress) CloseAdmission() {}

// Shutdown сообщает о начале ожидания приёма публикаций OBS и завершается после разрешения теста либо
// отмены контекста.
func (o *delayedIngress) Shutdown(ctx context.Context) error {
	o.once.Do(func() {
		// Однократно сообщает тесту о начале ожидания физического завершения приёма публикаций OBS.
		close(o.entered)
	})
	select {
	case <-o.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestManagerHandsOffBeforePhysicalJoinAndRepeatedCloseIsExact проверяет передачу диспетчера до
// завершения приёма публикаций OBS и сохранение новой сессии при позднем или повторном Close прежнего
// Manager.
func TestManagerHandsOffBeforePhysicalJoinAndRepeatedCloseIsExact(t *testing.T) {
	// ingress предоставляет управляемо задержанное завершение входного приёма.
	ingress := &delayedIngress{entered: make(chan struct{}), release: make(chan struct{})}
	// f предоставляет прежний менеджер с задержанным завершением приёма OBS.
	f := newManagerFixture(t, false, func(c *Config) {
		// Устанавливает проверочный приём публикаций OBS с управляемой задержкой физического завершения.
		c.Ingress = ingress
	})
	// released защищает однократное разрешение завершения входного приёма.
	var released sync.Once
	// release разрешает прежнему входному приёму закончить Shutdown.
	release := func() {
		// Разрешает отложенному приёму публикаций OBS завершиться с защитой от повторного закрытия канала.

		released.Do(func() {
			// Однократно закрывает канал разрешения физического завершения приёма публикаций OBS.
			close(ingress.release)
		})
	}
	t.Cleanup(release)
	// ctx ограничивает передачу диспетчера пятью секундами; cancel освобождает ресурсы этого срока.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// old содержит исходный запуск прежнего менеджера; err сообщает отказ его приёма.
	old, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "handoff-old", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит успешное завершение запуска прежнего менеджера.
	if current := waitReceipt(t, f, old); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	// closing передаёт результат закрытия прежнего менеджера после разрешения завершения приёма.
	closing := make(chan error, 1)
	go func() {
		// Закрывает прежний Manager параллельно с ожиданием передачи диспетчера.
		closing <- f.manager.Close(ctx)
	}()
	select {
	case <-ingress.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// c содержит настройки нового диспетчера с отдельными владельцами входа и подготовки.
	c := f.config
	c.Ingress = &lifecycleOwner{}
	c.Preparation = &lifecycleOwner{}
	// successor содержит новый менеджер, получивший диспетчер до завершения прежнего приёма.
	successor, err := New(ctx, c)
	if err != nil {
		t.Fatal("диспетчер не передан до физического присоединения", err)
	}
	t.Cleanup(func() {
		// Закрывает Manager, принявший диспетчер и новую сессию, до освобождения общих зависимостей.

		// err сообщает отказ закрытия нового менеджера при очистке теста.
		if err := successor.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	// next содержит новый запуск менеджера-преемника.
	next, err := successor.StartSession(ctx, core.StartSessionRequest{RequestID: "handoff-new", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит успешный результат запуска нового менеджера.
	if current := waitReceipt(t, f, next); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	release()
	select {
	case err = <-closing:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for range 2 {
		if err = f.manager.Close(ctx); err != nil {
			t.Fatal(err)
		}
		// session содержит новый запуск для проверки сохранности при позднем и повторном Close
		// прежнего менеджера; err сообщает отказ его чтения.
		session, err := f.service.GetSession(ctx, next.SessionID)
		if err != nil || session.State != core.SessionRunning {
			t.Fatal("старый Close отозвал новую Session", session, err)
		}
		if _, err = successor.Feed(session); err != nil {
			t.Fatal("старый Close отозвал новый Feed", err)
		}
	}
}

// TestManagerCloseDeadlineKeepsRetryablePhysicalJoin проверяет возврат DeadlineExceeded при
// незавершённом приёме публикаций OBS; зарегистрированное завершение проверки освобождает его перед
// повторным Close.
func TestManagerCloseDeadlineKeepsRetryablePhysicalJoin(t *testing.T) {
	// ingress задерживает завершение приёма до отложенного разрешения теста.
	ingress := &delayedIngress{entered: make(chan struct{}), release: make(chan struct{})}
	// f предоставляет менеджер для проверки ограниченного ожидания закрытия.
	f := newManagerFixture(t, false, func(c *Config) {
		// Устанавливает задерживаемый приём публикаций OBS для проверки истечения срока Close.
		c.Ingress = ingress
	})
	defer close(ingress.release)
	// ctx ограничивает первое закрытие пятьюдесятью миллисекундами; cancel освобождает ресурсы этого
	// срока.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// err содержит результат закрытия при ещё не завершённом входном приёме.
	if err := f.manager.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("просроченное присоединение принято за успешное", err)
	}
}

// TestManagerRetiresUnobservedStartupClaimWithoutReplay проверяет, что неоднозначный CLAIMED
// переживает срок исходного обработчика и после восстановления БД завершается отказом без повторного
// запуска; ошибка выхода сохраняет Feed родителя.
func TestManagerRetiresUnobservedStartupClaimWithoutReplay(t *testing.T) {
	// kind выбирает неизвестный захват родительского запуска либо добавления назначения.
	for _, kind := range []core.CommandKind{core.CommandStartSession, core.CommandAddOutput} {
		t.Run(string(kind), func(t *testing.T) {
			// Проверяет завершение без повторного запуска для выбранной команды с неоднозначным CLAIMED.

			// f предоставляет зависимости для проверки завершения неизвестного CLAIMED без
			// повторного запуска.
			f := newManagerFixture(t, false, nil)
			// err сообщает отказ закрытия исходного менеджера перед установкой отказа
			// захвата.
			if err := f.manager.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			// fault теряет подтверждение CLAIMED выбранной команды и задерживает последующие
			// обращения к БД.
			fault := &receiptFaultStore{Store: f.store, kind: kind, stage: core.CommandClaimed, healed: make(chan struct{}), blocked: make(chan struct{}, 32), expired: make(chan struct{}, 32)}
			// healed защищает однократное восстановление обращений после неизвестного
			// захвата.
			var healed sync.Once
			// heal разрешает последующим операциям снова обращаться к БД.
			heal := func() {
				// Разрешает последующие запросы к проверочной БД после временного отказа.

				healed.Do(func() {
					// Однократно закрывает канал восстановления запросов к БД.
					close(fault.healed)
				})
			}
			// service предоставляет доменные операции через обёртку неизвестного CLAIMED;
			// err сообщает отказ создания сервиса.
			service, err := core.NewService(fault, f.store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			// c задаёт сроки запуска и отдельных операций для проверки пережившего
			// обработчик захвата.
			c := f.config
			c.Core = service
			c.StartupTimeout = 3 * time.Second
			c.OperationTimeout = 40 * time.Millisecond
			// manager содержит диспетчер, который должен завершить неизвестный захват без
			// повторной выдачи запуска.
			manager, err := New(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Восстанавливает доступ к БД и закрывает Manager независимо от исхода проверки.

				heal()
				// err сообщает отказ закрытия диспетчера после восстановления БД.
				if err := manager.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			// start содержит принятый родительский запуск перед выбором проверяемой команды.
			start, err := manager.StartSession(context.Background(), core.StartSessionRequest{RequestID: "claim-parent", StreamID: f.stream.ID})
			if err != nil {
				t.Fatal(err)
			}
			// receipt выбирает квитанцию родителя либо добавленного назначения для ожидания
			// неизвестного захвата.
			receipt := start
			defer func() {
				// При неуспехе теста выводит ограниченную трассу и текущие состояния БД, наблюдений и узла для разбора
				// задержанного CLAIMED.

				if !t.Failed() {
					return
				}
				fault.mu.Lock()
				// event содержит очередную сохранённую строку трассы для диагностики
				// неуспеха теста.
				for _, event := range fault.events {
					t.Log(event)
				}
				t.Logf("diagnostic_overflow=%d parent_valid=%v", fault.dropped, fault.parent.Valid())
				fault.mu.Unlock()
				// diagnostic ограничивает диагностические чтения одной секундой; stop
				// освобождает этот срок.
				diagnostic, stop := context.WithTimeout(context.Background(), time.Second)
				defer stop()
				// session содержит диагностическое состояние родителя; sessionErr
				// сообщает отказ чтения сессии.
				session, sessionErr := f.service.GetSession(diagnostic, start.SessionID)
				// current содержит диагностическую квитанцию проверяемой команды;
				// receiptErr сообщает отказ её чтения.
				current, receiptErr := f.service.GetReceipt(diagnostic, receipt.ID)
				// snapshot содержит диагностические наблюдения точного исполнения;
				// snapshotErr сообщает отказ чтения оперативного снимка.
				snapshot, snapshotErr := f.service.RuntimeSnapshot(diagnostic, core.RuntimeSessionFence{SessionID: session.ID, Incarnation: session.Incarnation, Lifecycle: session.Lifecycle})
				t.Logf("SQL parent=%+v err=%v receipt=%+v err=%v RAM=%+v err=%v", session, sessionErr, current, receiptErr, snapshot, snapshotErr)
				manager.mu.Lock()
				// node указывает локальный узел родителя для диагностики его завершения
				// и допуска.
				node := manager.sessions[start.SessionID]
				manager.mu.Unlock()
				if node != nil {
					node.mu.Lock()
					t.Logf("node running=%v closing=%v joined=%v ctx=%v owner_valid=%v retirement=%v cause=%v", node.running, node.closing, node.joined, node.ctx.Err(), node.owner.Valid(), node.retirementNeeded, node.retirementFailure)
					node.mu.Unlock()
				} else {
					t.Log("node отсутствует до cleanup фикстуры")
				}
			}()
			if kind == core.CommandAddOutput {
				// current содержит успешную квитанцию родителя перед отказом захвата
				// добавляемого выхода.
				current := waitReceipt(t, f, start)
				if current.Status != core.CommandSucceeded {
					t.Fatal(current)
				}
				// parent содержит родительский допуск для контроля изоляции отказа
				// выхода; bindErr сообщает отказ привязки допуска.
				parent, bindErr := f.service.BindExecutionOwner(context.Background(), receiptFence(current))
				if bindErr != nil {
					t.Fatal(bindErr)
				}
				fault.mu.Lock()
				fault.parent = parent
				fault.mu.Unlock()
				receipt, err = manager.AddOutput(context.Background(), core.AddOutputRequest{RequestID: "claim-child", SessionID: start.SessionID, DestinationID: f.destination.ID})
				if err != nil {
					t.Fatal(err)
				}
			}
			// ctx ограничивает ожидание исходного обработчика и восстановления десятью
			// секундами; cancel освобождает этот срок.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			select {
			case <-fault.blocked:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			// Ошибка записи переживает исходного обработчика запуска и его ограниченную очистку.
			// ticker задаёт период проверки завершения обработчика неизвестного запуска.
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				manager.mu.Lock()
				// working отмечает сохранение проверяемой команды среди работающих
				// обработчиков менеджера.
				working := manager.commands[receipt.ID]
				manager.mu.Unlock()
				if !working {
					break
				}
				select {
				case <-ticker.C:
				case <-ctx.Done():
					t.Fatal("обработчик запуска не завершился", ctx.Err())
				}
			}
			heal()
			// current содержит терминальный результат неизвестного захвата после
			// восстановления БД.
			if current := waitReceipt(t, f, receipt); current.Status == core.CommandSucceeded {
				t.Fatal("просроченный запуск повторён после восстановления БД", current)
			}
			if kind == core.CommandStartSession {
				// session содержит родителя после отказа неизвестного запуска; err
				// сообщает отказ чтения его терминального состояния.
				session, err := f.service.GetSession(ctx, start.SessionID)
				if err != nil || !session.State.Terminal() {
					t.Fatal("неизвестный Claim оставил STARTING", session, err)
				}
				// next содержит следующий запуск после завершения неизвестного родителя.
				next, err := manager.StartSession(ctx, core.StartSessionRequest{RequestID: "claim-next", StreamID: f.stream.ID})
				if err != nil {
					t.Fatal(err)
				}
				// current содержит результат нового запуска после освобождения прежнего
				// активного слота.
				if current := waitReceipt(t, f, next); current.Status != core.CommandSucceeded {
					t.Fatal(current)
				}
			} else {
				// output содержит состояние назначения после отказа неизвестного
				// захвата; err сообщает отказ его чтения.
				output, err := f.service.GetOutput(ctx, receipt.OutputID)
				if err != nil || output.DesiredRunning || output.Issued {
					t.Fatal("неизвестный Claim оставил выход активным", output, err)
				}
				// session содержит родителя для проверки сохранности Feed после отказа
				// назначения.
				session, err := f.service.GetSession(ctx, start.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = manager.Feed(session); err != nil {
					t.Fatal("ошибка выхода отозвала здоровый parent", err)
				}
			}
		})
	}
}

// TestOutputCleanupRejectsDifferentGenerationBeforeCancellation проверяет отклонение старого поколения
// до отмены нового членства или отправителя и до изменения исключённых поколений.
func TestOutputCleanupRejectsDifferentGenerationBeforeCancellation(t *testing.T) {
	// withOutput выбирает проверку устаревшей очистки для членства либо отправителя нового
	// поколения.
	for _, withOutput := range []bool{false, true} {
		t.Run(map[bool]string{false: "membership", true: "publisher"}[withOutput], func(t *testing.T) {
			// Проверяет сохранность нового поколения отдельно для предварительного членства и работающего
			// отправителя.

			// lifetime задаёт наблюдаемый контекст нового поколения; cancel позволяет
			// обнаружить его ошибочную отмену старой очисткой.
			lifetime, cancel := context.WithCancel(context.Background())
			defer cancel()
			// id задаёт идентификатор назначения нового поколения.
			id := core.ID("new-output")
			// node содержит членство либо отправителя поколения 3 и пустую карту исключённых
			// поколений.
			node := &sessionNode{members: map[core.ID]*membership{}, outputs: map[core.ID]*outputNode{}, excluded: map[core.ID]uint64{}}
			if withOutput {
				node.outputs[id] = &outputNode{startReceipt: core.CommandReceipt{OutputGeneration: 3}, cancel: cancel, done: make(chan struct{})}
			} else {
				node.members[id] = &membership{output: core.SessionOutput{Generation: 3}, ctx: lifetime, cancel: cancel, done: make(chan struct{})}
			}
			// ctx ограничивает проверку старой очистки одной секундой; stop освобождает этот
			// срок.
			ctx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			// err содержит отказ очистки поколения 1 при наличии поколения 3.
			if err := node.waitOutputCleanup(ctx, id, 1); !errors.Is(err, core.ErrStale) {
				t.Fatal("очистка приняла другое поколение", err)
			}
			if lifetime.Err() != nil || len(node.excluded) != 0 {
				t.Fatal("старый fence изменил текущее исполнение", lifetime.Err(), node.excluded)
			}
		})
	}
}

// TestAcknowledgedOutputCleanupPagePreservesRestartedExecution проверяет, что устаревшая страница уже
// подтверждённой очистки не удаляет и не останавливает повторно запущенный выход.
func TestAcknowledgedOutputCleanupPagePreservesRestartedExecution(t *testing.T) {
	// f предоставляет работающую сессию для проверки устаревшей страницы очистки назначения.
	f := newManagerFixture(t, false, nil)
	// ctx ограничивает сценарий остановки и повторного запуска восемью секундами; cancel освобождает
	// этот срок.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// start содержит запуск родительской сессии; err сообщает отказ его приёма.
	start, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "stale-page-parent", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит успешный результат родительского запуска.
	if current := waitReceipt(t, f, start); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	// added содержит команду добавления проверяемого назначения.
	added, err := f.manager.AddOutput(ctx, core.AddOutputRequest{RequestID: "stale-page-add", SessionID: start.SessionID, DestinationID: f.destination.ID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит успешное завершение добавления назначения.
	if current := waitReceipt(t, f, added); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	// stopped содержит команду остановки назначения перед повторным запуском.
	stopped, err := f.manager.StopOutput(ctx, core.OutputCommand{RequestID: "stale-page-stop", OutputID: added.OutputID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит успешный результат остановки назначения.
	if current := waitReceipt(t, f, stopped); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	// page содержит подтверждённую очистку остановленного назначения.
	page, err := f.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: start.SessionID, OutputID: added.OutputID}})
	if err != nil || len(page.Items) != 1 || !page.Items[0].Acknowledged {
		t.Fatal(page, err)
	}
	// oldPage изображает устаревшую неподтверждённую страницу уже завершённой очистки.
	oldPage := page.Items[0]
	// Законное подтверждение уже разрешило новый запуск; старая страница ещё сообщает о неподтверждённой очистке.
	oldPage.Acknowledged = false
	// restarted содержит команду нового поколения прежнего назначения.
	restarted, err := f.manager.StartOutput(ctx, core.OutputCommand{RequestID: "stale-page-restart", OutputID: added.OutputID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит успешный результат повторного запуска.
	if current := waitReceipt(t, f, restarted); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	if err = f.manager.cleanupRuntime(ctx, oldPage); err != nil {
		t.Fatal(err)
	}
	f.manager.mu.Lock()
	// parent указывает локального владельца сессии после подачи устаревшей очистки.
	parent := f.manager.sessions[start.SessionID]
	f.manager.mu.Unlock()
	parent.mu.Lock()
	// output указывает текущий узел назначения для проверки сохранности нового поколения.
	output := parent.outputs[added.OutputID]
	parent.mu.Unlock()
	if output == nil || output.startReceipt.OutputGeneration != restarted.OutputGeneration {
		t.Fatal("старая страница удалила текущее исполнение")
	}
	output.mu.Lock()
	// prepared содержит текущего отправителя для проверки, что старая очистка его не остановила.
	prepared := output.publisher
	output.mu.Unlock()
	if prepared == nil {
		t.Fatal("новый Publisher отсутствует")
	}
	select {
	case <-prepared.Done():
		t.Fatal("старая страница остановила новый Publisher")
	default:
	}
}
