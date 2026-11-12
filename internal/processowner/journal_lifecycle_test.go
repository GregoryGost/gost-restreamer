package processowner

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

// Моделирует потерю блокировки основного процесса после запуска вспомогательного и проверяет, что
// Resolve перечитывает каталог и учитывает операции, отсутствовавшие в исходном снимке.
func TestResolveIncludesOperationsRegisteredAfterInitialSnapshot(t *testing.T) {
	// r содержит реестр с настоящим helper для проверки полного перечитывания каталога после потери
	// backend-блокировки.
	r := testRegistry(t, buildHelper(t))
	// ctx ограничивает операции регистрации, ожидания и сверки.
	ctx := testContext(t)
	// s содержит созданную область; err сохраняет результаты её создания, запуска цели и проверки
	// каталога.
	s, err := r.Create(ctx, "catalog-snapshot-owner")
	if s != nil {
		t.Cleanup(func() {
			// Закрывает созданную область, включая частичный результат Create, после проверки полноты каталога
			// операций.

			// err сообщает об отказе резервного закрытия созданной либо частичной области.
			if err := s.CloseContext(testContext(t)); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	// initial сохраняет ранний заголовок до регистрации native-процесса и его backend-операции.
	initial, err := s.header()
	if err != nil || initial.Count != 1 {
		t.Fatalf("начальный заголовок=%+v %v", initial, err)
	}
	// readyRead принимает готовность цели; readyWrite передаётся ей через FD 3.
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает оба дескриптора канала готовности проверочного дочернего процесса.
		_ = readyRead.Close()
		_ = readyWrite.Close()
	}()
	// controlRead передаётся цели через FD 4; controlWrite удерживает её до разрешения завершения.
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает оба дескриптора канала управления проверочным дочерним процессом.
		_ = controlRead.Close()
		_ = controlWrite.Close()
	}()
	// p содержит управляемую цель, зарегистрированную после раннего initial-снимка.
	p, err := s.Start(ctx, Command{Binary: os.Args[0], Args: []string{"-test.run=^TestControlledNativeChild$", "--", "--controlled-native-child"}, ExtraFiles: []*os.File{readyWrite, controlRead}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Отменяет процесс и ждёт его фактический результат при любом завершении проверки.

		p.Cancel()
		// err сообщает об отказе резервного ожидания отменённого целевого процесса.
		if _, err := p.Wait(testContext(t)); err != nil {
			t.Error(err)
		}
	})
	_ = readyWrite.Close()
	_ = controlRead.Close()
	// ready принимает однобайтовую готовность цели перед потерей backend-блокировки.
	var ready [1]byte
	if _, err = io.ReadFull(readyRead, ready[:]); err != nil {
		t.Fatal(err)
	}
	// current сохраняет поздний заголовок с полным каталогом трёх операций.
	current, err := s.header()
	if err != nil || current.Count != 3 {
		t.Fatalf("текущий заголовок=%+v %v", current, err)
	}
	// Моделируется освобождение блокировки backend.lock при аварии после регистрации и запуска дочернего процесса.
	s.mu.Lock()
	err = unlock(s.backend)
	s.backend = nil
	s.closed = true
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// Продолжается проверка, уже прочитавшая заголовок до появления двух новых операций.
	// proof содержит результат Resolve, которому передан устаревший initial и который должен
	// перечитать каталог.
	proof, err := s.resolve(ctx, initial)
	if err != nil || proof.Joined || proof.Unknown || !proof.Sealed || proof.Operations != 3 || proof.PendingOperations != 1 {
		t.Fatalf("живой helper пропущен в доказательстве=%+v %v", proof, err)
	}
	if _, err = os.Stat(s.Namespace()); err != nil {
		t.Fatalf("пространство имён удалено до завершения дочернего процесса: %v", err)
	}
	if err = r.AcknowledgeScope(ctx, s.ID()); !errors.Is(err, ErrPending) {
		t.Fatalf("преждевременное подтверждение очистки=%v", err)
	}
	p.Cancel()
	// result содержит фактически дождённый исход целевого процесса после отмены.
	result, err := p.Wait(ctx)
	if err != nil || !result.Joined {
		t.Fatalf("завершение дочернего процесса=%+v %v", result, err)
	}
	proof, err = s.Resolve(ctx)
	if err != nil || !proof.Joined || proof.Operations != 3 {
		t.Fatalf("окончательное доказательство=%+v %v", proof, err)
	}
}

// Проверяет, что отменённое создание не ожидает занятую блокировку очистки и возвращает частичную
// область, которую можно завершить после освобождения блокировки.
func TestCreateCancellationDoesNotWaitForCleanupGate(t *testing.T) {
	// r содержит реестр проверки отменённого Create с занятым gate очистки.
	r := testRegistry(t, buildHelper(t))
	// ctx задаёт отменяемое создание; cancel отменяет его внутри инициализатора при удерживаемом
	// gate.
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	// gate сохраняет намеренно занятую блокировку допуска частично созданной области.
	var gate *os.File
	// guard задаёт защитный таймер освобождения gate, если отменённое Create ошибочно продолжает
	// ждать.
	var guard *time.Timer
	// released передаёт результат освобождения gate защитным таймером.
	released := make(chan error, 1)
	// s содержит частично созданную область; err сохраняет результат отмены создания и
	// восстановления.
	s, err := r.create(ctx, "cancelled-runtime-owner", func(s *Scope, ctx context.Context) (*Ticket, error) {
		// Инициализирует область, удерживает её блокировку допуска и отменяет контекст создания для проверки
		// возврата частичного Scope.

		// ticket учитывает инициализацию runtime-корня; err сообщает об отказе инициализации
		// либо получения gate.
		ticket, err := s.initializeRuntime(ctx)
		if err != nil {
			return ticket, err
		}
		gate, err = lockExisting(ctx, r.root, scopePath(s.id)+"/gate.lock", true)
		if err != nil {
			return ticket, err
		}
		guard = time.AfterFunc(5*time.Second, func() {
			// Освобождает задержанную блокировку по защитному сроку и передаёт ошибку, если отменённое Create всё
			// ещё ожидало её.
			released <- unlock(gate)
		})
		cancel()
		return ticket, nil
	})
	if guard != nil {
		if guard.Stop() {
			// err сообщает об отказе явного освобождения gate после успешного возврата
			// отменённого Create.
			if err := unlock(gate); err != nil {
				t.Error(err)
			}
		} else {
			t.Error("отменённое создание ожидало освобождения блокировки очистки")
			// err содержит результат освобождения gate сработавшим защитным таймером.
			if err := <-released; err != nil {
				t.Error(err)
			}
		}
	}
	if s != nil {
		t.Cleanup(func() {
			// Повторяет закрытие частично созданной области с действующим контекстом после проверки отмены.

			// err сообщает об отказе резервного повторного закрытия частичной области.
			if err := s.CloseContext(testContext(t)); err != nil {
				t.Error(err)
			}
		})
	}
	if guard == nil || s == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("отменённое создание потеряло частичную область: scope=%v err=%v", s, err)
	}
	// err сообщает об отказе явного закрытия частичной области с действующим контекстом.
	if err := s.CloseContext(testContext(t)); err != nil {
		t.Fatal(err)
	}
	// proof содержит доказательство завершения восстановленного отменённого создания.
	proof, err := s.Resolve(testContext(t))
	if err != nil || !proof.Joined {
		t.Fatalf("восстановление отменённого создания=%+v %v", proof, err)
	}
}

// Проверяет сохранение частичной области, запроса завершения и блокировки основного процесса при
// отказе файловой системы. После восстановления записей повторное закрытие и восстановление должны
// завершить очистку.
func TestCreatePreservesScopeAfterCompletionAndCloseFailure(t *testing.T) {
	// r содержит реестр проверки ошибки финальной записи при создании области.
	r := testRegistry(t, buildHelper(t))
	// ctx ограничивает создание, повторные финальные записи и восстановление.
	ctx := testContext(t)
	// restore возвращает временно перемещённый каталог операций после моделируемого файлового
	// отказа.
	var restore func() error
	// initialTicket сохраняет начальную физически вернувшуюся операцию, чья финальная запись должна
	// остаться повторяемой.
	var initialTicket *Ticket
	// s содержит частичную область; err сохраняет результаты создания, восстановления и повторного
	// закрытия.
	s, err := r.create(ctx, "partial-runtime-owner", func(s *Scope, ctx context.Context) (*Ticket, error) {
		// После инициализации временно убирает каталог операций, вызывая ошибки завершения начальной операции
		// и закрытия частичной области.

		// ticket содержит операцию инициализации runtime-корня; err сообщает о её создании либо
		// перемещении каталога операций.
		ticket, err := s.initializeRuntime(ctx)
		if err != nil {
			return ticket, err
		}
		initialTicket = ticket
		// operations задаёт исходный путь каталога операций создаваемой области.
		operations := scopePath(s.id) + "/operations"
		// saved задаёт временное имя, под которым тест скрывает каталог операций.
		saved := scopePath(s.id) + "/saved-operations"
		if err = r.root.Rename(operations, saved); err != nil {
			return ticket, err
		}
		restore = func() error {
			// Возвращает временно перемещённый каталог операций на исходное место для повторной записи завершения.
			return r.root.Rename(saved, operations)
		}
		return ticket, nil
	})
	if restore == nil {
		t.Fatalf("отказ файловой системы не смоделирован: %v", err)
	}
	t.Cleanup(func() {
		// Восстанавливает каталог операций при необходимости и закрывает сохранённую частичную область.

		if restore != nil {
			// err сообщает об отказе резервного восстановления каталога операций.
			if err := restore(); err != nil {
				t.Error(err)
			}
		}
		if s != nil {
			// err сообщает об отказе резервного закрытия сохранённой частичной области.
			if err := s.CloseContext(testContext(t)); err != nil {
				t.Error(err)
			}
		}
	})
	if err == nil || s == nil || !errors.Is(err, ErrUnknown) {
		t.Fatalf("частичная область потеряна: scope=%v err=%v", s, err)
	}
	if initialTicket == nil || !initialTicket.completionRequested || initialTicket.completed {
		t.Fatal("начальное завершение не сохранено для повтора")
	}
	// backend сохраняет исходный дескриптор владения для проверки его удержания после ошибки Close.
	backend := s.backend
	if backend == nil || !s.closing || s.closed {
		t.Fatal("неудачное закрытие освободило владение backend")
	}
	if err = s.CloseContext(ctx); !errors.Is(err, ErrUnknown) {
		t.Fatalf("отказ файловой системы не отклонил повторное закрытие: %v", err)
	}
	if err = restore(); err != nil {
		t.Fatal(err)
	}
	restore = nil
	// lock содержит неожиданно доступную backend-блокировку; lockErr должен подтвердить её удержание
	// через ErrPending.
	lock, lockErr := lockExisting(ctx, r.root, scopePath(s.id)+"/backend.lock", false)
	if lock != nil {
		_ = unlock(lock)
	}
	if !errors.Is(lockErr, ErrPending) || s.backend != backend {
		t.Fatalf("блокировка частичной области потеряна: %v", lockErr)
	}
	if err = s.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	if !initialTicket.completed || s.backend != nil {
		t.Fatal("восстановленная файловая система не позволила завершить операцию и освободить блокировку")
	}
	// page содержит результаты восстановления тем же реестром после возврата каталога операций.
	page, _, err := r.RecoverPage(ctx, "", r.PageLimit())
	if err != nil || len(page) != 1 || page[0].ID != s.ID() || !page[0].Joined || page[0].Unknown {
		t.Fatalf("восстановление тем же Registry=%+v %v", page, err)
	}
	if _, err = os.Stat(s.Namespace()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("пространство имён осталось после восстановления: %v", err)
	}
}
