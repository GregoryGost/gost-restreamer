package core

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

// reconcileContextKey различает тестовое значение контекста для проверки его сохранения при сверке.
type reconcileContextKey struct{}

// reconcileContextUOW перехватывает контексты согласованных чтений и подставляет задержки для проверки
// срока сверки.
type reconcileContextUOW struct {
	// observationAcceptanceUOW предоставляет исходную общую RuntimeAuthority и тестовое сохраняемое
	// управление.
	*observationAcceptanceUOW
	// artifacts подставляет репозиторий поколений подготовки в согласованное чтение.
	artifacts ArtifactRepository
	// beforeView задаёт проверку, задержку или отказ перед очередным согласованным чтением.
	beforeView func(context.Context, int) error
	// contexts сохраняет полученные контексты чтений для проверки срока и освобождения.
	contexts []context.Context
}

// Запоминает контекст каждого чтения, проверяет отмену и вызывает заданную задержку или ошибку перед
// передачей тестовых репозиториев.
func (u *reconcileContextUOW) View(ctx context.Context, fn func(Repositories) error) error {
	u.contexts = append(u.contexts, ctx)
	// err содержит отмену контекста до перехвата согласованного чтения.
	if err := ctx.Err(); err != nil {
		return err
	}
	if u.beforeView != nil {
		// err сообщает об ошибке контролируемой задержки или проверки контекста перед чтением.
		if err := u.beforeView(ctx, len(u.contexts)); err != nil {
			return err
		}
	}
	// err содержит отмену контекста после заданной задержки чтения.
	if err := ctx.Err(); err != nil {
		return err
	}
	return u.observationAcceptanceUOW.View(ctx, func(r Repositories) error {
		// Подставляет назначенный репозиторий Artifacts в набор тестовых репозиториев и вызывает исходную
		// функцию чтения.

		if u.artifacts != nil {
			r.Artifacts = u.artifacts
		}
		return fn(r)
	})
}

// Выбирает обычную либо принадлежащую точному OutputObserver запись для одинаковых проверок
// послезаписной сверки.
func runReconcileGuard(owned bool, ctx context.Context, u *reconcileContextUOW, owner *OutputObserver, budget time.Duration, write func() (bool, error)) error {
	if owned {
		return u.authority.GuardOwnedWrite(ctx, u, owner, budget, write)
	}
	return u.authority.GuardWrite(ctx, u, budget, write)
}

// Открывает наблюдателей Session и Output и публикует исходные READY/PUBLISHING-значения для сравнения
// после сверки.
func openReconcileObservers(t *testing.T, s *Service, parent, child ExecutionFence) (*SessionObserver, *OutputObserver) {
	t.Helper()
	// so содержит наблюдателя текущего выполнения сессии. err сообщает об ошибке регистрации
	// наблюдателя точной сессии.
	so, err := s.OpenSessionObserver(context.Background(), parent)
	if err != nil {
		t.Fatal(err)
	}
	// oo содержит наблюдателя точной попытки выхода.
	oo, err := s.OpenOutputObserver(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке записи оперативного состояния точного владельца.
	if err := so.Observe(context.Background(), SessionObservation{State: ObservationReady}); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке записи оперативного состояния точного владельца.
	if err := oo.Observe(context.Background(), OutputObservation{State: OutputPublishing}); err != nil {
		t.Fatal(err)
	}
	return so, oo
}

// Проверяет самостоятельный конечный контекст сверки после отмены вызывающего, сохранение только
// подтверждённых владельцев и отсутствие сверки после доказанного отката.
func TestReconcileCallerCancellationPreservesOnlyVerifiedOwners(t *testing.T) {
	// owned выбирает обычную запись либо запись под точным OutputObserver.
	for _, owned := range []bool{false, true} {
		// guardName задаёт имя проверяемого способа защиты записи.
		guardName := map[bool]string{false: "write", true: "owned"}[owned]
		// outcome выбирает подтверждённый откат, возможный COMMIT или недоступную сверку.
		for _, outcome := range []string{"committed", "ambiguous", "changed_fence", "read_failure", "rollback"} {
			t.Run(guardName+"/"+outcome, func(t *testing.T) {
				// Проверяет для обычной либо принадлежащей наблюдателю записи сохранность результата и только
				// подтверждённых владельцев после отмены вызывающего контекста.

				// s предоставляет доменные операции с общим реестром наблюдений. base
				// предоставляет исходный UnitOfWork с общей authority и изменяемым
				// управлением. parent идентифицирует точное выполнение родительской
				// сессии. child идентифицирует точную попытку дочернего выхода.
				s, base, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
				// so содержит наблюдателя текущего выполнения сессии. oo содержит
				// наблюдателя точной попытки выхода.
				so, oo := openReconcileObservers(t, s, parent, child)
				// before сохраняет согласованный снимок до проверяемого действия. err
				// сообщает об ошибке чтения текущих наблюдений сессии.
				before, err := s.RuntimeSnapshot(context.Background(), observationSessionFence(parent))
				if err != nil {
					t.Fatal(err)
				}
				// u добавляет перехват контекстов чтения к исходному тестовому
				// UnitOfWork.
				u := &reconcileContextUOW{observationAcceptanceUOW: base}
				// value идентифицирует значение вызывающего контекста, которое должно
				// сохраниться при сверке.
				value := new(int)
				// ctx задаёт контекст вызывающего кода для проверяемой записи. cancel
				// отменяет его внутри функции записи, перед послезаписной сверкой.
				ctx, cancel := context.WithCancel(context.WithValue(context.Background(), reconcileContextKey{}, value))
				defer cancel()
				u.beforeView = func(readCtx context.Context, _ int) error {
					// Проверяет сохранение значения контекста и выдачу нового конечного срока послезаписной сверке.

					if readCtx.Value(reconcileContextKey{}) != value {
						t.Error("сверка потеряла значение контекста")
					}
					// deadline содержит срок послезаписного чтения для проверки
					// свежего конечного бюджета. ok показывает наличие конечного
					// срока послезаписного контекста.
					if deadline, ok := readCtx.Deadline(); !ok || !deadline.After(time.Now()) {
						t.Error("сверка не получила свежий конечный бюджет")
					}
					return nil
				}
				// failure задаёт синтетический отказ записи для проверки сохранения
				// исходного результата.
				failure := errors.New("неоднозначная запись")
				// writeErr задаёт исходный результат записи, который сверка не должна
				// подменять.
				var writeErr error
				if outcome == "ambiguous" || outcome == "rollback" {
					writeErr = failure
				}
				// calls считает вызовы записи для обнаружения недопустимого повтора.
				calls := 0
				err = runReconcileGuard(owned, ctx, u, oo, time.Second, func() (bool, error) {
					// Имитирует выбранный исход записи, при необходимости меняет Lifecycle или ломает чтение, затем
					// отменяет вызывающий контекст.

					calls++
					if outcome == "changed_fence" {
						base.repository.session.Lifecycle++
					}
					if outcome == "read_failure" || outcome == "rollback" {
						base.viewErr = errors.New("чтение недоступно")
					}
					cancel()
					return outcome != "rollback", writeErr
				})
				if err != writeErr || calls != 1 || !errors.Is(ctx.Err(), context.Canceled) {
					t.Fatalf("результат или число записей изменились: err=%v calls=%d caller=%v", err, calls, ctx.Err())
				}
				// wantValid задаёт ожидаемую сохранность допусков после выбранного
				// исхода записи.
				wantValid := outcome != "changed_fence" && outcome != "read_failure"
				if so.Valid() != wantValid || oo.Valid() != wantValid {
					t.Fatalf("неверный допуск после %s: session=%t output=%t", outcome, so.Valid(), oo.Valid())
				}
				// after содержит согласованный снимок после проверяемого действия для
				// проверки сохранности. snapshotErr содержит результат чтения RAM после
				// проверяемого исхода записи.
				after, snapshotErr := s.RuntimeSnapshot(context.Background(), observationSessionFence(parent))
				if wantValid {
					if snapshotErr != nil || !reflect.DeepEqual(before, after) {
						t.Fatalf("неизменённое выполнение потеряло RAM: %v", snapshotErr)
					}
				} else if !errors.Is(snapshotErr, ErrNotReady) {
					t.Fatalf("неподтверждённый владелец сохранил снимок: %v", snapshotErr)
				}
				if outcome == "rollback" {
					if len(u.contexts) != 0 {
						t.Fatal("подтверждённый rollback запустил сверку")
					}
				} else if len(u.contexts) == 0 {
					t.Fatal("возможный COMMIT не проверен")
				}
				// readCtx содержит контекст завершённого согласованного чтения для
				// проверки освобождения.
				for _, readCtx := range u.contexts {
					select {
					case <-readCtx.Done():
					default:
						t.Error("контекст завершённой сверки не освобождён")
					}
				}
			})
		}
	}
}

// Проверяет начало отдельного бюджета сверки после завершения записи, даже если запись исчерпала срок
// исходного контекста.
func TestReconcileStartsItsBudgetAfterWrite(t *testing.T) {
	// owned выбирает обычную запись либо запись под точным OutputObserver.
	for _, owned := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "owned"}[owned], func(t *testing.T) {
			// Проверяет, что выбранный способ защиты записи начинает отдельный бюджет сверки после завершения
			// записи.

			synctest.Test(t, func(t *testing.T) {
				// В изолированном времени исчерпывает срок записи и проверяет сохранение подтверждённых наблюдателей
				// при свежем сроке сверки.

				// s предоставляет доменные операции с общим реестром наблюдений. base
				// предоставляет исходный UnitOfWork с общей authority и изменяемым
				// управлением. parent идентифицирует точное выполнение родительской
				// сессии. child идентифицирует точную попытку дочернего выхода.
				s, base, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
				// so содержит наблюдателя текущего выполнения сессии. oo содержит
				// наблюдателя точной попытки выхода.
				so, oo := openReconcileObservers(t, s, parent, child)
				// u добавляет перехват контекстов чтения к исходному тестовому
				// UnitOfWork.
				u := &reconcileContextUOW{observationAcceptanceUOW: base}
				// ctx ограничивает время исходной записи тремя секундами. cancel
				// освобождает таймер этого контекста после проверки.
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				// calls считает вызовы записи для обнаружения недопустимого повтора.
				calls := 0
				// err сообщает об ошибке проверки отмены согласованного чтения.
				err := runReconcileGuard(owned, ctx, u, oo, time.Second, func() (bool, error) {
					// Ждёт истечения срока вызывающего контекста и возвращает возможный COMMIT с его ошибкой.

					calls++
					<-ctx.Done()
					return true, ctx.Err()
				})
				if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || len(u.contexts) == 0 || !so.Valid() || !oo.Valid() {
					t.Fatalf("запись исчерпала бюджет последующей сверки: err=%v calls=%d reads=%d session=%t output=%t", err, calls, len(u.contexts), so.Valid(), oo.Valid())
				}
			})
		})
	}
}

// Проверяет отказ неверного бюджета и отменённого допуска до записи и сверки без изменения владельцев.
func TestReconcileRejectsInvalidBudgetAndCanceledAdmission(t *testing.T) {
	// owned выбирает обычную запись либо запись под точным OutputObserver.
	for _, owned := range []bool{false, true} {
		// budget задаёт проверяемый срок послезаписной сверки.
		for _, budget := range []time.Duration{-time.Second, 0, time.Second} {
			t.Run(map[bool]string{false: "write", true: "owned"}[owned]+"/"+budget.String(), func(t *testing.T) {
				// Проверяет отказ для неположительного бюджета либо отменённого допуска без вызова записи, чтений и
				// отзыва владельцев.

				// s предоставляет доменные операции с общим реестром наблюдений. base
				// предоставляет исходный UnitOfWork с общей authority и изменяемым
				// управлением. parent идентифицирует точное выполнение родительской
				// сессии. child идентифицирует точную попытку дочернего выхода.
				s, base, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
				// so содержит наблюдателя текущего выполнения сессии. oo содержит
				// наблюдателя точной попытки выхода.
				so, oo := openReconcileObservers(t, s, parent, child)
				// u добавляет перехват контекстов чтения к исходному тестовому
				// UnitOfWork.
				u := &reconcileContextUOW{observationAcceptanceUOW: base}
				// ctx задаёт контекст операций проверяемого сценария оперативных
				// наблюдений. cancel отменяет проверяемый контекст до обращения к
				// реестру.
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				// calls считает вызовы записи для обнаружения недопустимого повтора.
				calls := 0
				// err сообщает об ошибке проверяемой записи с последующей сверкой
				// владельцев.
				err := runReconcileGuard(owned, ctx, u, oo, budget, func() (bool, error) {
					// Учитывает ошибочный вызов записи, которая должна быть отклонена до исполнения.
					calls++
					return true, nil
				})
				// want задаёт ожидаемую категорию отказа проверяемого сценария.
				want := error(context.Canceled)
				if budget <= 0 {
					want = ErrInvalid
				}
				if !errors.Is(err, want) || calls != 0 || len(u.contexts) != 0 || !so.Valid() || !oo.Valid() {
					t.Fatalf("отклонённый запрос изменил выполнение: err=%v calls=%d reads=%d", err, calls, len(u.contexts))
				}
			})
		}
	}
}

// Проверяет отказ записи от заменённого OutputObserver до вызова переданной функции и любых чтений без
// отзыва нового владельца.
func TestReconcileOwnedWriteRejectsReplacedObserver(t *testing.T) {
	// s предоставляет доменные операции с общим реестром наблюдений. base предоставляет исходный
	// UnitOfWork с общей authority и изменяемым управлением. parent идентифицирует точное выполнение
	// родительской сессии. child идентифицирует точную попытку дочернего выхода.
	s, base, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
	// so содержит наблюдателя текущего выполнения сессии. old содержит прежнего наблюдателя выхода
	// до замены token.
	so, old := openReconcileObservers(t, s, parent, child)
	// fresh содержит нового наблюдателя выхода после замены token. err сообщает об ошибке
	// регистрации наблюдателя точной попытки выхода.
	fresh, err := s.OpenOutputObserver(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	// u добавляет перехват контекстов чтения к исходному тестовому UnitOfWork.
	u := &reconcileContextUOW{observationAcceptanceUOW: base}
	// calls считает вызовы записи для обнаружения недопустимого повтора.
	calls := 0
	err = runReconcileGuard(true, context.Background(), u, old, time.Second, func() (bool, error) {
		// Учитывает ошибочный вызов записи под заменённым OutputObserver, которая должна быть отклонена как
		// ErrStale.
		calls++
		return true, nil
	})
	if !errors.Is(err, ErrStale) || calls != 0 || len(u.contexts) != 0 || old.Valid() || !fresh.Valid() || !so.Valid() {
		t.Fatalf("прежний наблюдатель затронул новое выполнение: err=%v calls=%d reads=%d", err, calls, len(u.contexts))
	}
}

// Проверяет сохранение исходного panic, выполнение свежей сверки и освобождение общей блокировки для
// следующей записи.
func TestReconcilePanicPreservesOutcomeAndReleasesAdmission(t *testing.T) {
	// owned выбирает обычную запись либо запись под точным OutputObserver.
	for _, owned := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "owned"}[owned], func(t *testing.T) {
			// Проверяет сохранение исходной паники и освобождение общей блокировки для выбранного способа защиты
			// записи.

			synctest.Test(t, func(t *testing.T) {
				// В изолированном времени проверяет свежую сверку после паники и возможность следующего входа под
				// общей блокировкой.

				// s предоставляет доменные операции с общим реестром наблюдений. base
				// предоставляет исходный UnitOfWork с общей authority и изменяемым
				// управлением. parent идентифицирует точное выполнение родительской
				// сессии. child идентифицирует точную попытку дочернего выхода.
				s, base, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
				// so содержит наблюдателя текущего выполнения сессии. oo содержит
				// наблюдателя точной попытки выхода.
				so, oo := openReconcileObservers(t, s, parent, child)
				// u добавляет перехват контекстов чтения к исходному тестовому
				// UnitOfWork.
				u := &reconcileContextUOW{observationAcceptanceUOW: base}
				// ctx задаёт контекст проверяемой записи. cancel отменяет его внутри
				// функции записи перед паникой.
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				// panicValue задаёт исходную панику записи для проверки сохранения её
				// значения.
				panicValue := errors.New("исходный panic записи")
				// recovered принимает перехваченное значение паники после отложенной
				// сверки.
				var recovered any
				// calls считает вызовы записи для обнаружения недопустимого повтора.
				calls := 0
				func() {
					// Перехватывает панику записи после выполнения отложенной сверки для сравнения с исходным значением.

					defer func() {
						// Сохраняет значение recover для проверки, что послезаписная сверка не подменила панику.
						recovered = recover()
					}()
					_ = runReconcileGuard(owned, ctx, u, oo, time.Second, func() (bool, error) {
						// Учитывает вызов записи, отменяет вызывающий контекст и возбуждает исходную панику.

						calls++
						cancel()
						panic(panicValue)
					})
				}()
				if recovered != panicValue || calls != 1 || len(u.contexts) == 0 || !so.Valid() || !oo.Valid() {
					t.Fatalf("panic изменён или нарушил свежую сверку: panic=%v calls=%d reads=%d session=%t output=%t", recovered, calls, len(u.contexts), so.Valid(), oo.Valid())
				}
				// next задаёт ограниченный контекст следующей записи после паники.
				// cancelNext освобождает контекст следующей записи после паники.
				next, cancelNext := context.WithTimeout(context.Background(), time.Second)
				defer cancelNext()
				// admitted отмечает фактический допуск следующей записи после паники.
				admitted := false
				// err сообщает об ошибке проверяемой записи с последующей сверкой
				// владельцев.
				err := runReconcileGuard(owned, next, u, oo, time.Second, func() (bool, error) {
					// Отмечает допуск следующей записи после паники и сообщает подтверждённое отсутствие COMMIT.
					admitted = true
					return false, nil
				})
				if err != nil || !admitted {
					t.Fatal("panic не освободил общий допуск", err)
				}
			})
		})
	}
}

// reconcilePreparationArtifacts подставляет точное поколение подготовки для проверки общей сверки
// владельцев.
type reconcilePreparationArtifacts struct {
	// ArtifactRepository сохраняет неиспользуемую часть контракта артефактов.
	ArtifactRepository
	// generation хранит точное выданное или терминальное поколение подготовки.
	generation FallbackGeneration
}

// Возвращает заданное тестовое поколение для проверки владельца подготовки.
func (r *reconcilePreparationArtifacts) Generation(context.Context, ID) (FallbackGeneration, error) {
	return r.generation, nil
}

// Возвращает тестовое поколение только при совпадении фильтров состояния и incarnation.
func (r *reconcilePreparationArtifacts) Generations(_ context.Context, q ListQuery) ([]FallbackGeneration, error) {
	if q.State == string(r.generation.State) && q.Incarnation == r.generation.Incarnation {
		return []FallbackGeneration{r.generation}, nil
	}
	return nil, nil
}

// Моделирует отсутствие сохраняемых намерений очистки в проверках общего бюджета сверки.
func (*reconcilePreparationArtifacts) Cleanups(context.Context, ListQuery) ([]CleanupIntent, error) {
	return nil, nil
}

// reconcilePreparationRuntime предоставляет текущую сессию и выходы для проверки допуска подготовки.
type reconcilePreparationRuntime struct {
	// RuntimeRepository сохраняет неиспользуемую часть контракта runtime-репозитория.
	RuntimeRepository
	// repository ссылается на изменяемое управление тестовой сессией и выходами.
	repository *observationAcceptanceRepository
}

// Возвращает единственную тестовую сессию для проверки запрета подготовки.
func (r reconcilePreparationRuntime) Sessions(context.Context, ListQuery) ([]Session, error) {
	return []Session{r.repository.session}, nil
}

// Материализует все тестовые выходы из текущего набора репозитория.
func (r reconcilePreparationRuntime) Outputs(context.Context, ListQuery) ([]SessionOutput, error) {
	// outputs материализует копии всех тестовых выходов для проверки отсутствия занятых ресурсов.
	outputs := make([]SessionOutput, 0, len(r.repository.outputs))
	// output содержит очередной сохраняемый выход при материализации списка.
	for _, output := range r.repository.outputs {
		outputs = append(outputs, output)
	}
	return outputs, nil
}

// Проверяет единый срок сверки подготовки и допусков исполнения: время первого чтения уменьшает бюджет
// второго, а истечение отзывает непроверенных владельцев.
func TestReconcileSharesOneBudgetAcrossPreparationsAndLeases(t *testing.T) {
	// owned выбирает обычную запись либо запись под точным OutputObserver.
	for _, owned := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "owned"}[owned], func(t *testing.T) {
			// Проверяет единый бюджет сверки подготовки и допусков для выбранного способа защиты записи.

			synctest.Test(t, func(t *testing.T) {
				// В изолированном времени расходует общий срок на два последовательных чтения и проверяет отзыв
				// владельцев после истечения срока.

				// base предоставляет исходный UnitOfWork с общей authority и изменяемым
				// управлением. parent идентифицирует точное выполнение родительской
				// сессии. child идентифицирует точную попытку дочернего выхода.
				_, base, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
				base.repository.RuntimeRepository = reconcilePreparationRuntime{repository: &base.repository}
				// artifacts предоставляет выданное поколение подготовки для проверки
				// единого срока сверки.
				artifacts := &reconcilePreparationArtifacts{generation: FallbackGeneration{Record: Record{ID: "old-generation"}, AssetID: "asset", Incarnation: "owner", AttemptToken: "attempt", Attempt: 1, State: PreparationIssued, Issued: true}}
				// u добавляет перехват контекстов чтения к исходному тестовому
				// UnitOfWork.
				u := &reconcileContextUOW{observationAcceptanceUOW: base, artifacts: artifacts}
				// s предоставляет доменные операции с общим реестром наблюдений. err
				// сообщает об ошибке создания доменного сервиса.
				s, err := NewService(u, "owner")
				if err != nil {
					t.Fatal(err)
				}
				// currentSession сохраняет исходную сессию перед временным освобождением
				// допуска подготовки. currentOutputs сохраняет исходные выходы перед
				// временным освобождением допуска подготовки.
				currentSession, currentOutputs := base.repository.session, base.repository.outputs
				base.repository.session.State, base.repository.session.Issued = SessionStopped, false
				base.repository.outputs = nil
				// preparation содержит владельца точной выданной подготовки перед
				// переводом в терминальное состояние.
				preparation, err := s.BindPreparationOwner(context.Background(), PreparationFenceFor(artifacts.generation))
				if err != nil {
					t.Fatal(err)
				}
				artifacts.generation.State, artifacts.generation.CleanupPending = PreparationFailed, true
				// err сообщает об ошибке сверки терминальной подготовки после возможного
				// COMMIT.
				if err := base.authority.GuardWrite(context.Background(), u, time.Second, func() (bool, error) {
					// Сообщает возможный COMMIT для сверки и отзыва терминального владельца подготовки.
					return true, nil
				}); err != nil || preparation.Valid() {
					t.Fatal("терминальная подготовка не отозвана", err)
				}
				base.repository.session, base.repository.outputs = currentSession, currentOutputs
				// so содержит наблюдателя текущего выполнения сессии. oo содержит
				// наблюдателя точной попытки выхода.
				so, oo := openReconcileObservers(t, s, parent, child)
				u.contexts = nil
				// timedOut отмечает истечение единого срока на втором чтении сверки.
				var timedOut bool
				u.beforeView = func(ctx context.Context, n int) error {
					// Задерживает первое и второе чтения на разные доли общего срока и фиксирует DeadlineExceeded без
					// продления бюджета.

					// cost задаёт длительность задержки текущего чтения относительно
					// общего срока.
					cost := 3 * time.Second / 4
					if n > 1 {
						cost = time.Second / 2
					}
					select {
					case <-ctx.Done():
						timedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
						return ctx.Err()
					case <-time.After(cost):
						return nil
					}
				}
				// ctx задаёт контекст исходной записи. cancel отменяет его внутри
				// функции записи перед общей сверкой подготовки и допусков.
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				// calls считает вызовы записи для обнаружения недопустимого повтора.
				calls := 0
				err = runReconcileGuard(owned, ctx, u, oo, time.Second, func() (bool, error) {
					// Учитывает вызов записи, отменяет вызывающий контекст и сообщает возможный COMMIT без ошибки.
					calls++
					cancel()
					return true, nil
				})
				if err != nil || calls != 1 || !timedOut || len(u.contexts) != 2 || so.Valid() || oo.Valid() {
					t.Fatalf("бюджет продлён или истечение срока сохранило допуск: err=%v calls=%d reads=%d timeout=%t session=%t output=%t", err, calls, len(u.contexts), timedOut, so.Valid(), oo.Valid())
				}
				// first содержит срок первого чтения сверки подготовки. firstOK
				// показывает наличие срока у первого чтения.
				first, firstOK := u.contexts[0].Deadline()
				// second содержит срок второго чтения сверки runtime-допусков. secondOK
				// показывает наличие срока у второго чтения.
				second, secondOK := u.contexts[1].Deadline()
				if !firstOK || !secondOK || !first.Equal(second) {
					t.Fatal("подготовка и допуски получили разные сроки одной сверки")
				}
			})
		})
	}
}
