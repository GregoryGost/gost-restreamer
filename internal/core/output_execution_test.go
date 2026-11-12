package core

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// Создаёт тестовый Service с выданным выходом, точным снимком и действующим RuntimeLease для проверки
// физического резерва.
func executionFixture(t *testing.T) (*Service, *observationAcceptanceUOW, *RuntimeLease, ExecutionFence, OutputSnapshot) {
	t.Helper()
	// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет изменяемые
	// тестовые репозитории и учитывает обращения к БД. f идентифицирует точную попытку проверяемого
	// выхода.
	s, u, _, f := newObservationAcceptanceService(t, acceptanceObservationLimits())
	// snapshot содержит неизменяемый снимок выхода для точного резервирования.
	snapshot := OutputSnapshot{Destination: Destination{Name: "Назначение", Endpoint: "rtmp://example.invalid/live", StreamKey: "exact-key"}, Artifact: ArtifactManifest{CanonicalProfile: []byte{1, 2, 3}}}
	// o содержит копию сохраняемого выхода для установки точного тестового снимка.
	o := u.repository.outputs[f.OutputID]
	o.Snapshot = snapshot
	u.repository.outputs[f.OutputID] = o
	// lease содержит логический допуск точного сохраняемого исполнения. err сообщает об ошибке
	// выдачи логического допуска точного исполнения.
	lease, err := s.BindExecutionOwner(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return s, u, lease, f, snapshot
}

// Резервирует физическое исполнение заданного снимка и завершает тест при отказе.
func reserveExecution(t *testing.T, s *Service, l *RuntimeLease, snapshot OutputSnapshot) *OutputExecutionReservation {
	t.Helper()
	// r удерживает физический резерв проверяемого выхода. err сообщает об ошибке резервирования
	// физического исполнения выхода.
	r, err := s.ReserveOutputExecution(context.Background(), l, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Открывает точный OutputObserver и однократно активирует подготовленный физический резерв.
func activateExecution(t *testing.T, s *Service, r *OutputExecutionReservation, f ExecutionFence) *OutputObserver {
	t.Helper()
	// o содержит точного наблюдателя активированного выхода. err сообщает об ошибке регистрации
	// наблюдателя точной попытки выхода.
	o, err := s.OpenOutputObserver(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Activate(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	return o
}

// Меняет тестовое сохраняемое намерение выхода, продвигая поколение и оставляя необходимость очистки.
func deauthorizeExecution(u *observationAcceptanceUOW, f ExecutionFence) {
	// o содержит копию выхода для отзыва сохраняемого намерения и продвижения поколения.
	o := u.repository.outputs[f.OutputID]
	o.Generation++
	o.DesiredRunning = false
	o.CleanupPending = true
	u.repository.outputs[f.OutputID] = o
}

// Проверяет точное совпадение снимка и единственность резерва между Service до подтверждения
// долговечной непригодности прежнего исполнения и его освобождения.
func TestOutputExecutionExactSnapshotAndSharedExclusivity(t *testing.T) {
	// ctx задаёт контекст проверяемых операций физического исполнения выхода.
	ctx := context.Background()
	// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет изменяемые
	// тестовые репозитории и учитывает обращения к БД. l содержит логический допуск точного
	// исполнения выхода. f идентифицирует точную попытку проверяемого выхода. snapshot содержит
	// неизменяемый снимок выхода для точного резервирования.
	s, u, l, f, snapshot := executionFixture(t)
	// change вносит выбранную подмену снимка перед проверкой точного резервирования.
	for _, change := range []func(*OutputSnapshot){
		func(s *OutputSnapshot) {
			// Меняет Endpoint сохранённого снимка для проверки отказа резервирования подменённого выхода.
			s.Destination.Endpoint += "?changed"
		},
		func(s *OutputSnapshot) {
			// Меняет канонические байты профиля манифеста для проверки точного сравнения снимка.
			s.Artifact.CanonicalProfile = []byte{1, 2, 4}
		},
		func(s *OutputSnapshot) {
			// Убирает канонические байты профиля манифеста для проверки различия присутствия и отсутствия.
			s.Artifact.CanonicalProfile = nil
		},
		func(s *OutputSnapshot) {
			// Меняет ключ назначения для проверки точного сравнения снимка.
			s.Destination.StreamKey = "other-key"
		},
	} {
		// forged содержит подменённый снимок, который должен быть отклонён при резервировании.
		forged := snapshot
		change(&forged)
		// err сообщает об ошибке резервирования физического исполнения выхода.
		if _, err := s.ReserveOutputExecution(ctx, l, forged); !errors.Is(err, ErrStale) {
			t.Fatal("подменённый снимок принят", err)
		}
	}
	// r удерживает физический резерв проверяемого выхода.
	r := reserveExecution(t, s, l, snapshot)
	if r.ExecutionFence() != f {
		t.Fatal("ExecutionFence изменён")
	}
	// other предоставляет второй Service с общей authority того же хранилища. err сообщает об ошибке
	// создания доменного сервиса.
	other, err := NewService(u, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.ReserveOutputExecution(ctx, l, snapshot); !errors.Is(err, ErrInUse) {
		t.Fatal("второй Service обошёл эксклюзивность", err)
	}
	if err = l.Revoke(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.Done():
	default:
		t.Fatal("отзыв не запечатал исполнение")
	}
	// fresh содержит повторно выданный логический допуск того же точного исполнения.
	fresh, err := other.BindExecutionOwner(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.ReserveOutputExecution(ctx, fresh, snapshot); !errors.Is(err, ErrInUse) {
		t.Fatal("повторная привязка обошла удержание", err)
	}
	if err = r.ReleaseJoined(); !errors.Is(err, ErrNotReady) {
		t.Fatal("отзыв разрешил повтор пригодного исполнения", err)
	}
	if err = other.ConfirmOutputExecutionResolution(ctx, r); !errors.Is(err, ErrNotReady) {
		t.Fatal("пригодный ExecutionFence ошибочно разрешён", err)
	}
	deauthorizeExecution(u, f)
	if err = other.ConfirmOutputExecutionResolution(ctx, r); err != nil {
		t.Fatal(err)
	}
	u.viewErr = errors.New("SQL недоступен")
	if err = r.ReleaseJoined(); err != nil {
		t.Fatal("ReleaseJoined обращается к SQL", err)
	}
	if err = r.ReleaseJoined(); err != nil {
		t.Fatal("ReleaseJoined не идемпотентен", err)
	}
	if len(u.authority.outputExecutions) != 0 || u.authority.outputExecutionBytes != 0 {
		t.Fatal("бюджет не освобождён")
	}
}

// Проверяет три независимых слота действий, однократную активацию и защиту нового слота от позднего
// Finish; закрытие реестра сохраняет незавершённые операции.
func TestOutputExecutionSlotsIdentityAndCloseCleanup(t *testing.T) {
	// ctx задаёт контекст проверяемых операций физического исполнения выхода.
	ctx := context.Background()
	// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет изменяемые
	// тестовые репозитории и учитывает обращения к БД. l содержит логический допуск точного
	// исполнения выхода. f идентифицирует точную попытку проверяемого выхода. snapshot содержит
	// неизменяемый снимок выхода для точного резервирования.
	s, u, l, f, snapshot := executionFixture(t)
	// r удерживает физический резерв проверяемого выхода.
	r := reserveExecution(t, s, l, snapshot)
	// err сообщает об ошибке допуска внешней операции в фиксированный слот.
	if _, err := r.Admit(ctx, OutputOperationRead); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	// err сообщает об ошибке активации физического резерва точным наблюдателем.
	if err := r.Activate(ctx, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	// observer содержит наблюдателя точной попытки выхода.
	observer := activateExecution(t, s, r, f)
	// err сообщает об ошибке активации физического резерва точным наблюдателем.
	if err := r.Activate(ctx, observer); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	// slots сохраняет допущенные операции установления, чтения и записи для проверки независимых
	// слотов.
	var slots [3]*OutputOperation
	// kind выбирает один из трёх фиксированных видов внешней операции.
	for _, kind := range []OutputOperationKind{OutputOperationEstablishment, OutputOperationRead, OutputOperationWrite} {
		// err принимает результат занятия очередного фиксированного слота.
		var err error
		slots[kind], err = r.Admit(ctx, kind)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.Admit(ctx, kind); !errors.Is(err, ErrInUse) {
			t.Fatal("слот занят повторно", err)
		}
	}
	slots[OutputOperationWrite].Finish()
	// fresh содержит новую операцию записи после завершения прежней. err сообщает об ошибке допуска
	// внешней операции в фиксированный слот.
	fresh, err := r.Admit(ctx, OutputOperationWrite)
	if err != nil {
		t.Fatal(err)
	}
	slots[OutputOperationWrite].Finish()
	if _, err = r.Admit(ctx, OutputOperationWrite); !errors.Is(err, ErrInUse) {
		t.Fatal("поздний Finish освободил чужой слот", err)
	}
	u.authority.Close()
	if _, err = r.Admit(ctx, OutputOperationRead); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err = r.ReleaseJoined(); !errors.Is(err, ErrNotReady) {
		t.Fatal("Close освободил незавершённые операции", err)
	}
	// slot содержит очередную допущенную внешнюю операцию для завершения.
	for _, slot := range slots {
		slot.Finish()
	}
	fresh.Finish()
	r.Seal()
	if err = r.ReleaseJoined(); err != nil {
		t.Fatal("освобождение после Close не завершилось", err)
	}
}

// Проверяет закрытие новых действий при замене наблюдателя или неопределённой сверке с сохранением
// резерва до доказанного отзыва сохраняемого исполнения.
func TestOutputExecutionObserverReplacementAndAmbiguityHold(t *testing.T) {
	// mode выбирает проверяемый вид отзыва или отказа бюджета физического резерва.
	for _, mode := range []string{"replacement", "close", "unreadable-reconcile"} {
		t.Run(mode, func(t *testing.T) {
			// Проверяет сохранение занятого резерва после замены или закрытия наблюдателя либо ошибки сверки до
			// долговечной непригодности ExecutionFence.

			// ctx задаёт контекст проверяемых операций физического исполнения выхода.
			ctx := context.Background()
			// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет
			// изменяемые тестовые репозитории и учитывает обращения к БД. l содержит
			// логический допуск точного исполнения выхода. f идентифицирует точную попытку
			// проверяемого выхода. snapshot содержит неизменяемый снимок выхода для точного
			// резервирования.
			s, u, l, f, snapshot := executionFixture(t)
			// r удерживает физический резерв проверяемого выхода.
			r := reserveExecution(t, s, l, snapshot)
			// o содержит точного наблюдателя активированного выхода.
			o := activateExecution(t, s, r, f)
			// views сохраняет число чтений БД перед проверяемой операцией RAM.
			views := u.views
			// operation содержит допущенную операцию чтения физического исполнения. err
			// сообщает об ошибке допуска внешней операции в фиксированный слот.
			operation, err := r.Admit(ctx, OutputOperationRead)
			if err != nil {
				t.Fatal(err)
			}
			operation.Finish()
			if u.views != views {
				t.Fatal("Admit вызвал View")
			}
			switch mode {
			case "replacement":
				if _, err = s.OpenOutputObserver(ctx, f); err != nil {
					t.Fatal(err)
				}
			case "close":
				if err = o.Close(ctx); err != nil {
					t.Fatal(err)
				}
			case "unreadable-reconcile":
				// ambiguous задаёт синтетическую ошибку возможного COMMIT для проверки
				// отзыва без освобождения резерва.
				ambiguous := errors.New("неоднозначный COMMIT")
				err = u.authority.GuardWrite(ctx, u, time.Second, func() (bool, error) {
					// Имитирует неоднозначный COMMIT и ломает последующее чтение, сохраняя исходную ошибку записи.
					u.viewErr = errors.New("read failure")
					return true, ambiguous
				})
				if !errors.Is(err, ambiguous) {
					t.Fatal(err)
				}
				if err = s.ConfirmOutputExecutionResolution(ctx, r); err == nil {
					t.Fatal("ошибка View сняла hold")
				}
				u.viewErr = nil
			}
			select {
			case <-r.Done():
			default:
				t.Fatal("исполнение сохранило допуск")
			}
			if _, err = r.Admit(ctx, OutputOperationWrite); !errors.Is(err, ErrStale) {
				t.Fatal(err)
			}
			if err = s.ConfirmOutputExecutionResolution(ctx, r); !errors.Is(err, ErrNotReady) {
				t.Fatal("отсутствие наблюдателя разрешило освобождение", err)
			}
			if err = r.ReleaseJoined(); !errors.Is(err, ErrNotReady) {
				t.Fatal(err)
			}
			// fresh содержит повторно выданный логический допуск того же точного исполнения.
			fresh, err := s.BindExecutionOwner(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ReserveOutputExecution(ctx, fresh, snapshot); !errors.Is(err, ErrInUse) {
				t.Fatal(err)
			}
			deauthorizeExecution(u, f)
			if err = s.ConfirmOutputExecutionResolution(ctx, r); err != nil {
				t.Fatal(err)
			}
			if err = r.ReleaseJoined(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Проверяет удержание подготовленного резерва до подтверждённого отказа команды и сохранение удержания
// при ошибке чтения результата.
func TestOutputExecutionPreparedFailureAndResolutionReadErrors(t *testing.T) {
	// ctx задаёт контекст проверяемых операций физического исполнения выхода.
	ctx := context.Background()
	// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет изменяемые
	// тестовые репозитории и учитывает обращения к БД. l содержит логический допуск точного
	// исполнения выхода. f идентифицирует точную попытку проверяемого выхода. snapshot содержит
	// неизменяемый снимок выхода для точного резервирования.
	s, u, l, f, snapshot := executionFixture(t)
	// c содержит копию сохраняемой квитанции для управляемого изменения допуска.
	c := u.repository.receipts[f.CommandID]
	c.Status = CommandIssued
	u.repository.receipts[f.CommandID] = c
	// r удерживает физический резерв проверяемого выхода.
	r := reserveExecution(t, s, l, snapshot)
	// err сообщает об ошибке регистрации наблюдателя точной попытки выхода.
	if _, err := s.OpenOutputObserver(ctx, f); !errors.Is(err, ErrStale) {
		t.Fatal("до Complete открыт OutputObserver", err)
	}
	r.Seal()
	r.Seal()
	c.Status = CommandFailed
	u.repository.receipts[f.CommandID] = c
	// readErr задаёт отказ чтения сохраняемого результата исполнения.
	readErr := errors.New("не удалось прочитать persisted outcome")
	u.viewErr = readErr
	// err сообщает об ошибке проверки долговечной непригодности прежней попытки.
	if err := s.ConfirmOutputExecutionResolution(ctx, r); !errors.Is(err, readErr) {
		t.Fatal(err)
	}
	// err сообщает об ошибке освобождения физического резерва после завершения действий.
	if err := r.ReleaseJoined(); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	u.viewErr = nil
	// err сообщает об ошибке проверки долговечной непригодности прежней попытки.
	if err := s.ConfirmOutputExecutionResolution(ctx, r); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке освобождения физического резерва после завершения действий.
	if err := r.ReleaseJoined(); err != nil {
		t.Fatal(err)
	}
}

// Проверяет явные бюджеты и переполнение идентичности без частичного резервирования, а также отказ
// отменённого или неизвестного вида действия.
func TestOutputExecutionBudgetsOverflowAndCanceledAdmission(t *testing.T) {
	// modify вносит выбранное нарушение явных бюджетов физических исполнений.
	for _, modify := range []func(*OutputExecutionLimits){
		func(l *OutputExecutionLimits) {
			// Обнуляет бюджет числа резервов исполнения.
			l.MaxReservations = 0
		}, func(l *OutputExecutionLimits) {
			// Обнуляет предел длины идентификатора исполнения.
			l.MaxIdentifierBytes = 0
		}, func(l *OutputExecutionLimits) {
			// Обнуляет общий бюджет байтов исполнения.
			l.MaxBytes = 0
		},
		func(l *OutputExecutionLimits) {
			// Задаёт предельные uint64 для идентификатора и общего бюджета, проверяя переполнение весов.
			l.MaxIdentifierBytes = math.MaxUint64
			l.MaxBytes = math.MaxUint64
		}, func(l *OutputExecutionLimits) {
			// Задаёт math.MaxInt для числа резервов, проверяя переполнение суммарного бюджета.
			l.MaxReservations = math.MaxInt
		},
	} {
		// limits задаёт исходные допустимые бюджеты физического реестра перед выбранным
		// нарушением.
		limits := OutputExecutionLimits{MaxReservations: 8, MaxIdentifierBytes: 1024, MaxBytes: 65536}
		modify(&limits)
		// err сообщает об ошибке проверки явных бюджетов физического реестра.
		if err := limits.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatal(limits, err)
		}
	}
	// mode выбирает проверяемый вид отзыва или отказа бюджета физического резерва.
	for _, mode := range []string{"bytes", "identifier", "identity"} {
		t.Run(mode, func(t *testing.T) {
			// Проверяет отказ резервирования по байтам, длине идентификатора либо исчерпанию счётчика
			// идентификаторов без занятия бюджета.

			// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет
			// изменяемые тестовые репозитории и учитывает обращения к БД. l содержит
			// логический допуск точного исполнения выхода. snapshot содержит неизменяемый
			// снимок выхода для точного резервирования.
			s, u, l, _, snapshot := executionFixture(t)
			switch mode {
			case "bytes":
				u.authority.outputExecutionLimits.MaxBytes = 512
			case "identifier":
				u.authority.outputExecutionLimits.MaxIdentifierBytes = 1
			case "identity":
				u.authority.nextOutputExecution = math.MaxUint64
			}
			// want задаёт ожидаемую категорию отказа проверяемого сценария.
			want := ErrOutputExecutionLimit
			if mode == "identity" {
				want = ErrConflict
			}
			// err сообщает об ошибке резервирования физического исполнения выхода.
			if _, err := s.ReserveOutputExecution(context.Background(), l, snapshot); !errors.Is(err, want) {
				t.Fatal(err)
			}
			if len(u.authority.outputExecutions) != 0 || u.authority.outputExecutionBytes != 0 {
				t.Fatal("неуспешное резервирование заняло бюджет")
			}
		})
	}
	// s предоставляет доменные операции с общим реестром наблюдений. l содержит логический допуск
	// точного исполнения выхода. f идентифицирует точную попытку проверяемого выхода. snapshot
	// содержит неизменяемый снимок выхода для точного резервирования.
	s, _, l, f, snapshot := executionFixture(t)
	// r удерживает физический резерв проверяемого выхода.
	r := reserveExecution(t, s, l, snapshot)
	activateExecution(t, s, r, f)
	// ctx задаёт контекст проверяемых операций физического исполнения выхода. cancel отменяет
	// контекст до проверяемой операции.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// err сообщает об ошибке допуска внешней операции в фиксированный слот.
	if _, err := r.Admit(ctx, OutputOperationWrite); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// err сообщает об ошибке допуска внешней операции в фиксированный слот.
	if _, err := r.Admit(context.Background(), OutputOperationKind(255)); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

// Проверяет совместное выполнение допуска, повторного Finish и закрытия резерва при конкурентных
// обращениях.
func TestOutputExecutionConcurrentAdmissionSealAndFinish(t *testing.T) {
	// s предоставляет доменные операции с общим реестром наблюдений. l содержит логический допуск
	// точного исполнения выхода. f идентифицирует точную попытку проверяемого выхода. snapshot
	// содержит неизменяемый снимок выхода для точного резервирования.
	s, _, l, f, snapshot := executionFixture(t)
	// r удерживает физический резерв проверяемого выхода.
	r := reserveExecution(t, s, l, snapshot)
	activateExecution(t, s, r, f)
	// wg ожидает завершения всех конкурентных операций допуска и Finish.
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			// Многократно конкурирует за слот записи и идемпотентно завершает полученную операцию; при занятом
			// слоте допускает ErrInUse, после закрытия допуска — ErrStale.

			for range 100 {
				// op содержит допущенную внешнюю операцию, завершаемую после её
				// фактического окончания. err сообщает об ошибке допуска внешней
				// операции в фиксированный слот.
				op, err := r.Admit(context.Background(), OutputOperationWrite)
				if err == nil {
					op.Finish()
					op.Finish()
				} else if !errors.Is(err, ErrStale) && !errors.Is(err, ErrInUse) {
					t.Error(err)
				}
			}
		})
	}
	wg.Go(r.Seal)
	wg.Wait()
	// err сообщает об ошибке допуска внешней операции в фиксированный слот.
	if _, err := r.Admit(context.Background(), OutputOperationWrite); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
}

// Проверяет копирование короткого OutputID, чтобы физический резерв не удерживал большой исходный
// строковый буфер.
func TestOutputExecutionFenceStringsAreDetached(t *testing.T) {
	// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет изменяемые
	// тестовые репозитории и учитывает обращения к БД. l содержит логический допуск точного
	// исполнения выхода. f идентифицирует точную попытку проверяемого выхода. snapshot содержит
	// неизменяемый снимок выхода для точного резервирования.
	s, u, l, f, snapshot := executionFixture(t)
	// huge создаёт большой чужой строковый буфер для проверки независимого хранения короткого ID.
	huge := strings.Repeat("x", 1<<20)
	f.OutputID = ID(huge[20:30])
	// c содержит копию сохраняемой квитанции для управляемого изменения допуска.
	c := u.repository.receipts[l.execution.CommandID]
	c.OutputID = f.OutputID
	u.repository.receipts[c.ID] = c
	// output содержит копию сохраняемого выхода для изменения его идентификатора или снимка.
	output := u.repository.outputs[l.execution.OutputID]
	output.ID = f.OutputID
	u.repository.outputs[f.OutputID] = output
	// fresh содержит повторно выданный логический допуск того же точного исполнения. err сообщает об
	// ошибке выдачи логического допуска точного исполнения.
	fresh, err := s.BindExecutionOwner(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	// r удерживает физический резерв проверяемого выхода.
	r := reserveExecution(t, s, fresh, snapshot)
	if unsafe.StringData(string(r.fence.OutputID)) == unsafe.StringData(string(f.OutputID)) {
		t.Fatal("короткий ID удерживает исходный буфер")
	}
	if r.fence.OutputID != f.OutputID {
		t.Fatal("клонирование изменило ID")
	}
}

// Проверяет занятость бюджета подготовленным, отозванным и ещё выполняющим действие резервом и защиту
// новой записи от позднего освобождения старой.
func TestOutputExecutionCountBudgetIncludesPreparedSealedAndJoinPending(t *testing.T) {
	// ctx задаёт контекст проверяемых операций физического исполнения выхода.
	ctx := context.Background()
	// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет изменяемые
	// тестовые репозитории и учитывает обращения к БД. l содержит логический допуск точного
	// исполнения выхода. f идентифицирует точную попытку проверяемого выхода. snapshot содержит
	// неизменяемый снимок выхода для точного резервирования.
	s, u, l, f, snapshot := executionFixture(t)
	u.authority.outputExecutionLimits.MaxReservations = 1
	// first содержит исходный физический резерв, сохраняющий бюджет до полного завершения.
	first := reserveExecution(t, s, l, snapshot)
	// otherFence идентифицирует другое исполнение выхода для проверки общего бюджета.
	otherFence := f
	otherFence.CommandID = "other-command"
	otherFence.OutputID = "other-output"
	// receipt содержит копию сохраняемой квитанции для другого выхода.
	receipt := u.repository.receipts[f.CommandID]
	receipt.ID = otherFence.CommandID
	receipt.OutputID = otherFence.OutputID
	u.repository.receipts[receipt.ID] = receipt
	// output содержит копию сохраняемого выхода для изменения его идентификатора или снимка.
	output := u.repository.outputs[f.OutputID]
	output.ID = otherFence.OutputID
	u.repository.outputs[output.ID] = output
	// otherLease содержит логический допуск другого выхода той же сессии. err сообщает об ошибке
	// выдачи логического допуска точного исполнения.
	otherLease, err := s.BindExecutionOwner(ctx, otherFence)
	if err != nil {
		t.Fatal(err)
	}
	// checkFull проверяет занятость общего бюджета исходным физическим резервом на разных этапах.
	checkFull := func() {
		// Проверяет, что подготовленный, закрытый либо ожидающий завершения резерв по-прежнему занимает
		// единственный слот бюджета.

		t.Helper()
		// err сообщает об ошибке резервирования физического исполнения выхода.
		if _, err := s.ReserveOutputExecution(ctx, otherLease, snapshot); !errors.Is(err, ErrOutputExecutionLimit) {
			t.Fatal("обойдён бюджет числа исполнений", err)
		}
	}
	checkFull()
	activateExecution(t, s, first, f)
	// op содержит допущенную внешнюю операцию, завершаемую после её фактического окончания.
	op, err := first.Admit(ctx, OutputOperationEstablishment)
	if err != nil {
		t.Fatal(err)
	}
	first.Seal()
	checkFull()
	deauthorizeExecution(u, f)
	if err = s.ConfirmOutputExecutionResolution(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err = first.ReleaseJoined(); !errors.Is(err, ErrNotReady) {
		t.Fatal("незавершённое подключение обошло удержание", err)
	}
	checkFull()
	op.Finish()
	if err = first.ReleaseJoined(); err != nil {
		t.Fatal(err)
	}
	// second содержит следующий физический резерв после освобождения предыдущего.
	second := reserveExecution(t, s, otherLease, snapshot)
	if second.token <= first.token {
		t.Fatal("идентичность переиспользована")
	}
	if err = first.ReleaseJoined(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReserveOutputExecution(ctx, otherLease, snapshot); !errors.Is(err, ErrInUse) {
		t.Fatal("старый ReleaseJoined удалил новую резервацию", err)
	}
}

// Проверяет, что временный отказ, пригодное исполнение, чужая RuntimeAuthority и копия резерва не
// разрешают освобождение; доказанное отсутствие выхода разрешает его.
func TestOutputExecutionResolutionRejectsTemporaryNotReadyAndForeignTokens(t *testing.T) {
	// ctx задаёт контекст проверяемых операций физического исполнения выхода.
	ctx := context.Background()
	// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет изменяемые
	// тестовые репозитории и учитывает обращения к БД. l содержит логический допуск точного
	// исполнения выхода. f идентифицирует точную попытку проверяемого выхода. snapshot содержит
	// неизменяемый снимок выхода для точного резервирования.
	s, u, l, f, snapshot := executionFixture(t)
	// r удерживает физический резерв проверяемого выхода.
	r := reserveExecution(t, s, l, snapshot)
	r.Seal()
	// state задаёт сохраняемый статус пригодной попытки, не разрешающий освобождение.
	for _, state := range []CommandStatus{CommandIssued, CommandSucceeded} {
		// c содержит копию сохраняемой квитанции для управляемого изменения допуска.
		c := u.repository.receipts[f.CommandID]
		c.Status = state
		u.repository.receipts[c.ID] = c
		// err сообщает об ошибке проверки долговечной непригодности прежней попытки.
		if err := s.ConfirmOutputExecutionResolution(ctx, r); !errors.Is(err, ErrNotReady) {
			t.Fatal("пригодное исполнение разрешено к освобождению", err)
		}
	}
	u.viewErr = ErrNotReady
	// err сообщает об ошибке проверки долговечной непригодности прежней попытки.
	if err := s.ConfirmOutputExecutionResolution(ctx, r); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	if r.releaseEligible {
		t.Fatal("временная ошибка допуска стала доказательством")
	}
	u.viewErr = nil
	// foreign предоставляет Service с другой authority для проверки чужого резерва.
	foreign, _, _, _, _ := executionFixture(t)
	// err сообщает об ошибке проверки долговечной непригодности прежней попытки.
	if err := foreign.ConfirmOutputExecutionResolution(ctx, r); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	// copied имитирует копию непрозрачного резерва для проверки точной идентичности.
	copied := *r
	// err сообщает об ошибке проверки долговечной непригодности прежней попытки.
	if err := s.ConfirmOutputExecutionResolution(ctx, &copied); !errors.Is(err, ErrStale) {
		t.Fatal("копия непрозрачного владельца принята", err)
	}
	delete(u.repository.outputs, f.OutputID)
	// err сообщает об ошибке проверки долговечной непригодности прежней попытки.
	if err := s.ConfirmOutputExecutionResolution(ctx, r); err != nil {
		t.Fatal("подтверждённое отсутствие выхода не разрешило освобождение", err)
	}
	// err сообщает об ошибке освобождения физического резерва после завершения действий.
	if err := r.ReleaseJoined(); err != nil {
		t.Fatal(err)
	}
}

// Проверяет отказ отменённого резервирования и безопасные результаты операций над nil и нулевыми
// дескрипторами.
func TestOutputExecutionInvalidHandlesAndCanceledReservation(t *testing.T) {
	// ctx задаёт контекст проверяемых операций физического исполнения выхода.
	ctx := context.Background()
	// s предоставляет доменные операции с общим реестром наблюдений. l содержит логический допуск
	// точного исполнения выхода. snapshot содержит неизменяемый снимок выхода для точного
	// резервирования.
	s, _, l, _, snapshot := executionFixture(t)
	// canceled задаёт контекст резервирования, отменяемый до вызова. cancel отменяет контекст до
	// проверяемой операции.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	// err сообщает об ошибке резервирования физического исполнения выхода.
	if _, err := s.ReserveOutputExecution(canceled, l, snapshot); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// err сообщает об ошибке резервирования физического исполнения выхода.
	if _, err := s.ReserveOutputExecution(ctx, nil, snapshot); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	// zero моделирует нулевой неподготовленный дескриптор физического резерва.
	var zero OutputExecutionReservation
	zero.Seal()
	// err сообщает об ошибке освобождения физического резерва после завершения действий.
	if err := zero.ReleaseJoined(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	// err сообщает об ошибке допуска внешней операции в фиксированный слот.
	if _, err := zero.Admit(ctx, OutputOperationWrite); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	// err сообщает об ошибке активации физического резерва точным наблюдателем.
	if err := zero.Activate(ctx, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if zero.Done() != nil {
		t.Fatal("нулевой владелец содержит Done")
	}
	// operation моделирует nil-дескриптор операции для проверки безопасного Finish.
	var operation *OutputOperation
	operation.Finish()
}
