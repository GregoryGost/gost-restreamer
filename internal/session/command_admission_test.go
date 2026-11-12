package session

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// replaceAdmissionManager закрывает прежний Manager и создаёт новый с заданным UnitOfWork и новыми
// проверочными владельцами приёма, сохраняя общие данные проверки.
func replaceAdmissionManager(t *testing.T, f managerFixture, uow core.UnitOfWork) managerFixture {
	t.Helper()
	// err сообщает отказ закрытия прежнего менеджера перед заменой транзакционного слоя.
	if err := f.manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// service предоставляет доменные операции через заданный UnitOfWork; err сообщает отказ создания
	// сервиса.
	service, err := core.NewService(uow, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// config сохраняет настройки фикстуры для замены сервиса и проверочных владельцев.
	config := f.config
	config.Core = service
	f.ingress, f.preparation = &lifecycleOwner{}, &lifecycleOwner{}
	config.Ingress, config.Preparation = f.ingress, f.preparation
	// manager содержит новый диспетчер над подменённым транзакционным слоем.
	manager, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает заменивший Manager до освобождения общих зависимостей проверки.

		// err сообщает отказ закрытия заменившего менеджера при очистке теста.
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	f.manager = manager
	return f
}

// startAdmissionParent запускает сессию и возвращает её квитанцию только после проверенного успеха
// запуска.
func startAdmissionParent(t *testing.T, f managerFixture) core.CommandReceipt {
	t.Helper()
	// start содержит принятую команду родительского запуска; err сообщает отказ её приёма.
	start, err := f.manager.StartSession(context.Background(), core.StartSessionRequest{RequestID: "admission-parent", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит терминальную квитанцию родительского запуска, требуемую для допуска
	// добавления.
	if current := waitReceipt(t, f, start); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	return start
}

// assertOnlyAdmissionOutput проверяет, что повтор запроса сохранил единственные выход и квитанцию с
// исходными идентификаторами.
func assertOnlyAdmissionOutput(t *testing.T, f managerFixture, receipt core.CommandReceipt) {
	t.Helper()
	// outputs содержит назначения проверяемой сессии; err сообщает отказ чтения для проверки
	// единственности выхода.
	outputs, err := f.service.ListOutputs(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: receipt.SessionID}})
	if err != nil || len(outputs.Items) != 1 || outputs.Items[0].ID != receipt.OutputID {
		t.Fatal("повтор команды создал другой выход", outputs, receipt, err)
	}
	// receipts содержит команды выбранного назначения для проверки единственности квитанции.
	receipts, err := f.service.ListReceipts(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: receipt.SessionID, OutputID: receipt.OutputID}})
	if err != nil || len(receipts.Items) != 1 || receipts.Items[0].ID != receipt.ID {
		t.Fatal("повтор команды создал другую квитанцию", receipts, receipt, err)
	}
}

// TestManagerAddPriorPrecedesFreshConfigurationAndFiles проверяет возврат прежней квитанции до нового
// выбора профиля и файла, а также конфликт при смене идентичности RequestID.
func TestManagerAddPriorPrecedesFreshConfigurationAndFiles(t *testing.T) {
	// f предоставляет сессию без начальных назначений для проверки повторного добавления.
	f := newManagerFixture(t, false, nil)
	// parent содержит успешную квитанцию родительской сессии.
	parent := startAdmissionParent(t, f)
	// request задаёт идентичность повторяемого добавления назначения.
	request := core.AddOutputRequest{RequestID: "prior-add", SessionID: parent.SessionID, DestinationID: f.destination.ID}
	// accepted содержит принятую команду добавления; err сообщает отказ её приёма.
	accepted, err := f.manager.AddOutput(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	// original содержит завершённую исходную квитанцию для сравнения повторного ответа.
	original := waitReceipt(t, f, accepted)
	if original.Status != core.CommandSucceeded {
		t.Fatal(original)
	}
	// output содержит снимок созданного назначения до изменения конфигурации и удаления файла.
	output, err := f.service.GetOutput(context.Background(), accepted.OutputID)
	if err != nil {
		t.Fatal(err)
	}
	// profile содержит сохранённый профиль, ревизия которого меняется для отказа нового выбора.
	profile := output.Snapshot.Profile
	profile.Profile.VideoEncoding.GOPFrames++
	if _, err = f.service.UpdateProfile(context.Background(), profile, profile.Revision); err != nil {
		t.Fatal(err)
	}
	// manifest задаёт точные путь, размер и хеш удаляемого файла заглушки.
	manifest := output.Snapshot.Artifact
	if err = f.files.RemoveExact(context.Background(), manifest.ManagedPath, manifest.SizeBytes, manifest.ContentHash); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.SelectAddBinding(context.Background(), request.SessionID, request.DestinationID); !errors.Is(err, core.ErrNotReady) {
		t.Fatal("новый выбор неожиданно готов", err)
	}
	request.Binding = core.PreparedBinding{GroupID: "untrusted-binding"}
	// duplicate содержит ответ повторного добавления после утраты пригодности новой конфигурации.
	duplicate, err := f.manager.AddOutput(context.Background(), request)
	if err != nil || !reflect.DeepEqual(duplicate, original) {
		t.Fatal("повтор изменил квитанцию из-за новой конфигурации", duplicate, original, err)
	}
	// conflicting содержит тот же RequestID с изменённым назначением для проверки конфликта
	// идентичности.
	conflicting := request
	conflicting.DestinationID = "different-destination"
	if _, err = f.manager.AddOutput(context.Background(), conflicting); !errors.Is(err, core.ErrConflict) {
		t.Fatal("RequestID принял другую идентичность", err)
	}
	assertOnlyAdmissionOutput(t, f, accepted)
}

// ambiguousAdmissionStore возвращает ошибку срока после настоящей вставки выбранной квитанции.
type ambiguousAdmissionStore struct {
	// Store выполняет настоящую транзакцию до инъекции потерянного подтверждения.
	*storage.Store
	// requestID выбирает команду, успешная вставка которой вызывает инъекцию.
	requestID string
	// injected ограничивает потерю подтверждения единственным случаем.
	injected atomic.Bool
}

// Write помечает вставку нужной квитанции в настоящей транзакции и однократно возвращает
// DeadlineExceeded после успешной записи.
func (s *ambiguousAdmissionStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	// inserted отмечает вставку выбранной квитанции внутри текущей транзакции.
	var inserted bool
	// err содержит результат настоящей транзакции до возможной замены на DeadlineExceeded.
	err := s.Store.Write(ctx, func(r core.Repositories) error {
		// Подставляет репозиторий, отмечающий вставку выбранной квитанции внутри настоящей записи.

		r.Runtime = ambiguousAdmissionRepository{RuntimeRepository: r.Runtime, requestID: s.requestID, inserted: &inserted}
		return callback(r)
	})
	if err == nil && inserted && s.injected.CompareAndSwap(false, true) {
		return context.DeadlineExceeded
	}
	return err
}

// ambiguousAdmissionRepository отмечает вставку выбранного запроса, сохраняя настоящее выполнение
// репозитория.
type ambiguousAdmissionRepository struct {
	// RuntimeRepository выполняет операции, включая настоящую вставку перехватываемой квитанции.
	core.RuntimeRepository
	// requestID выбирает квитанцию, вставка которой отмечается для инъекции.
	requestID string
	// inserted указывает флаг вставки выбранной квитанции в текущей транзакции.
	inserted *bool
}

// InsertReceipt сохраняет квитанцию и отмечает вставку выбранного RequestID для имитации потерянного
// подтверждения.
func (r ambiguousAdmissionRepository) InsertReceipt(ctx context.Context, receipt core.CommandReceipt) error {
	// err сообщает отказ настоящей вставки квитанции до установки признака инъекции.
	if err := r.RuntimeRepository.InsertReceipt(ctx, receipt); err != nil {
		return err
	}
	if receipt.RequestID == r.requestID {
		*r.inserted = true
	}
	return nil
}

// TestManagerAddResolvesAmbiguousAdmissionWithoutDuplicate проверяет восстановление квитанции
// добавления после ошибки подтверждения COMMIT без второго выхода или второй команды.
func TestManagerAddResolvesAmbiguousAdmissionWithoutDuplicate(t *testing.T) {
	// f предоставляет работающую сессию для проверки потерянного подтверждения добавления.
	f := newManagerFixture(t, false, nil)
	// wrapped скрывает подтверждение вставки команды ambiguous-add после успешной записи.
	wrapped := &ambiguousAdmissionStore{Store: f.store, requestID: "ambiguous-add"}
	f = replaceAdmissionManager(t, f, wrapped)
	// parent содержит успешно запущенную родительскую сессию.
	parent := startAdmissionParent(t, f)
	// request задаёт добавление, чьё подтверждение записи будет потеряно.
	request := core.AddOutputRequest{RequestID: wrapped.requestID, SessionID: parent.SessionID, DestinationID: f.destination.ID}
	// receipt содержит восстановленную квитанцию добавления; err сообщает отказ приёма или
	// восстановления команды.
	receipt, err := f.manager.AddOutput(context.Background(), request)
	if err != nil || !wrapped.injected.Load() {
		t.Fatal("неоднозначный COMMIT не сверен", receipt, err)
	}
	// current содержит результат исполнения добавления после восстановления его квитанции.
	if current := waitReceipt(t, f, receipt); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	// prior содержит сохранённую квитанцию выбранного добавления; found отмечает её наличие.
	prior, found, err := f.service.PriorAddOutput(context.Background(), request)
	if err != nil || !found || !sameCommand(prior, receipt) {
		t.Fatal("возвращена чужая квитанция", prior, receipt, found, err)
	}
	assertOnlyAdmissionOutput(t, f, receipt)
}

// TestManagerConcurrentAddPriorSurvivesFreshAdmissionFailure проверяет возврат параллельно принятой
// команды после устаревания профиля либо исчезновения файла у задержанного запроса.
func TestManagerConcurrentAddPriorSurvivesFreshAdmissionFailure(t *testing.T) {
	// failure выбирает утрату пригодности профиля либо исчезновение файла после конкурентного
	// приёма.
	for _, failure := range []string{"selection", "file"} {
		t.Run(failure, func(t *testing.T) {
			// Проверяет выбранную причину отказа нового выбора после того, как параллельный запрос уже принят.

			// f предоставляет родительскую сессию с согласованными сроками локального
			// запуска.
			f := newManagerFixture(t, false, func(c *Config) {
				// Согласует сроки запуска FFmpeg и сессии для проверки конкурирующего добавления выхода.

				c.Group.FFmpeg.StartupTimeout = c.StartupTimeout
			})
			// wrapped задерживает первое отсутствие квитанции concurrent-add для
			// конкурентного приёма.
			wrapped := &delayedPriorStore{Store: f.store, requestID: "concurrent-add", entered: make(chan struct{}), release: make(chan struct{})}
			f = replaceAdmissionManager(t, f, wrapped)
			// released обеспечивает однократное разрешение задержанного чтения.
			var released sync.Once
			// release разрешает задержанному чтению продолжиться с защитой от повторного
			// закрытия канала.
			release := func() {
				// Разрешает задержанному чтению продолжиться, не закрывая канал повторно.

				released.Do(func() {
					// Закрывает канал разрешения первого чтения ровно один раз.
					close(wrapped.release)
				})
			}
			t.Cleanup(release)
			// parent содержит квитанцию работающей родительской сессии.
			parent := startAdmissionParent(t, f)
			// request задаёт одинаковое добавление для задержанного и победившего обращений.
			request := core.AddOutputRequest{RequestID: wrapped.requestID, SessionID: parent.SessionID, DestinationID: f.destination.ID}
			// commandResult переносит результат задержанного добавления из горутины в тест.
			type commandResult struct {
				// receipt содержит квитанцию задержанного добавления.
				receipt core.CommandReceipt
				// err сообщает отказ задержанного добавления.
				err error
			}
			// result передаёт завершение задержанного обращения через буфер одного
			// результата.
			result := make(chan commandResult, 1)
			// ctx ограничивает управляемую гонку пятью секундами; cancel освобождает ресурсы
			// этого срока.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			go func() {
				// Выполняет задерживаемое добавление выхода и передаёт результат тесту через канал.
				// receipt содержит результат задержанного приёма; err сообщает отказ
				// задержанного добавления.
				receipt, err := f.manager.AddOutput(ctx, request)
				result <- commandResult{receipt, err}
			}()
			awaitPublication(t, ctx, wrapped.entered)
			// winner содержит квитанцию конкурентного приёма; err сообщает отказ победившего
			// обращения.
			winner, err := f.manager.AddOutput(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			// original содержит завершённую квитанцию победившего добавления для сравнения
			// ответов.
			original := waitReceipt(t, f, winner)
			if original.Status != core.CommandSucceeded {
				t.Fatal(original)
			}
			// output содержит снимок победившего назначения для изменения профиля либо
			// удаления файла.
			output, err := f.service.GetOutput(ctx, winner.OutputID)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "selection" {
				// profile содержит профиль созданного назначения, который изменяется
				// перед продолжением задержанного обращения.
				profile := output.Snapshot.Profile
				profile.Profile.VideoEncoding.GOPFrames++
				if _, err = f.service.UpdateProfile(ctx, profile, profile.Revision); err != nil {
					t.Fatal(err)
				}
			} else {
				// manifest содержит точные реквизиты файла, удаляемого перед
				// продолжением задержанного обращения.
				manifest := output.Snapshot.Artifact
				if err = f.files.RemoveExact(ctx, manifest.ManagedPath, manifest.SizeBytes, manifest.ContentHash); err != nil {
					t.Fatal(err)
				}
			}
			release()
			select {
			// actual содержит поздний ответ первого обращения для сравнения с сохранённым
			// успехом.
			case actual := <-result:
				if actual.err != nil || !reflect.DeepEqual(actual.receipt, original) {
					t.Fatal("ошибка нового выбора скрыла параллельно принятую команду", actual.receipt, original, actual.err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			assertOnlyAdmissionOutput(t, f, winner)
		})
	}
}

// TestManagerRejectsFreshOutputWithoutAdmissionSideEffects проверяет ошибки допуска добавления выхода
// без квитанции и выхода и с сохранением Feed родительской сессии.
func TestManagerRejectsFreshOutputWithoutAdmissionSideEffects(t *testing.T) {
	// test задаёт причину отказа нового добавления и ожидаемую ошибку допуска.
	for _, test := range []struct {
		// name выбирает моделируемый отказ и задаёт имя подтеста.
		name string
		// want задаёт ожидаемую ошибку отклонения нового добавления.
		want error
	}{
		{name: "missing_destination", want: core.ErrNotFound},
		{name: "missing_file", want: os.ErrNotExist},
		{name: "wrong_generator", want: core.ErrNotReady},
		{name: "canceled", want: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Проверяет выбранный отказ нового выхода и отсутствие долговечных последствий для работающей сессии.

			// f предоставляет родительскую сессию с выбранной причиной отказа допуска.
			f := newManagerFixture(t, false, func(c *Config) {
				// При проверке несовпадения генератора задаёт Manager версию, отличную от подготовленного варианта.

				if test.name == "wrong_generator" {
					c.GeneratorVersion = "other-generator"
				}
			})
			// parent содержит успешную команду запуска сессии, которая должна пережить отказ
			// добавления.
			parent := startAdmissionParent(t, f)
			// request задаёт новое добавление, для которого моделируется отказ допуска.
			request := core.AddOutputRequest{RequestID: "rejected-add", SessionID: parent.SessionID, DestinationID: f.destination.ID}
			// ctx задаёт отменяемый контекст приёма; cancel освобождает его и моделирует
			// отмену в выбранном сценарии.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch test.name {
			case "missing_destination":
				request.DestinationID = "missing-destination"
			case "missing_file":
				// variant содержит реквизиты удаляемой заглушки; err сообщает отказ
				// чтения варианта.
				variant, err := f.service.GetVariant(ctx, f.binding.VariantID)
				if err != nil {
					t.Fatal(err)
				}
				if err = f.files.RemoveExact(ctx, variant.ManagedPath, variant.SizeBytes, variant.ContentHash); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			}
			// err содержит отказ добавления, сравниваемый с ожидаемой причиной допуска.
			if _, err := f.manager.AddOutput(ctx, request); !errors.Is(err, test.want) {
				t.Fatal("неверный отказ допуска", err, test.want)
			}
			// found отмечает наличие квитанции отклонённого добавления; err сообщает отказ
			// проверки прежнего запроса.
			if _, found, err := f.service.PriorAddOutput(context.Background(), request); err != nil || found {
				t.Fatal("отклонённая команда оставила квитанцию", found, err)
			}
			// outputs содержит выходы родительской сессии; err сообщает отказ проверки
			// отсутствия созданного назначения.
			outputs, err := f.service.ListOutputs(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: parent.SessionID}})
			if err != nil || len(outputs.Items) != 0 {
				t.Fatal("отклонённая команда оставила выход", outputs, err)
			}
			// parentState содержит актуальную сессию для проверки сохранённого доступа к
			// Feed.
			parentState, err := f.service.GetSession(context.Background(), parent.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.manager.Feed(parentState); err != nil {
				t.Fatal("отказ допуска отозвал здоровую Session", err)
			}
		})
	}
}

// TestManagerCloseWaitsForEnteredAdmissionAndRejectsItsCommit проверяет ожидание вошедшего обращения
// при Close и отсутствие поздних квитанций и выходов после закрытия допуска.
func TestManagerCloseWaitsForEnteredAdmissionAndRejectsItsCommit(t *testing.T) {
	// kind выбирает запуск сессии либо добавление назначения во время закрытия менеджера.
	for _, kind := range []core.CommandKind{core.CommandStartSession, core.CommandAddOutput} {
		t.Run(string(kind), func(t *testing.T) {
			// Проверяет закрытие приёма для выбранного вида команды: запуска сессии или добавления выхода.

			// f предоставляет зависимости для проверки закрытия во время приёма команды.
			f := newManagerFixture(t, false, nil)
			// wrapped задерживает первое чтение квитанции вошедшего обращения до закрытия
			// допуска.
			wrapped := &delayedPriorStore{Store: f.store, requestID: "closing-admission", entered: make(chan struct{}), release: make(chan struct{})}
			f = replaceAdmissionManager(t, f, wrapped)
			// released обеспечивает однократное продолжение задержанного обращения.
			var released sync.Once
			// release открывает задержанному чтению путь к проверке уже закрытого допуска.
			release := func() {
				// Разрешает задержанному обращению продолжиться с защитой от повторного закрытия канала.

				released.Do(func() {
					// Однократно закрывает канал разрешения задержанного чтения квитанции.
					close(wrapped.release)
				})
			}
			t.Cleanup(release)
			// parent содержит успешный родительский запуск только для сценария добавления
			// назначения.
			var parent core.CommandReceipt
			if kind == core.CommandAddOutput {
				parent = startAdmissionParent(t, f)
			}
			// ctx ограничивает ожидание приёма и закрытия пятью секундами; cancel
			// освобождает этот срок.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// admitted передаёт результат ранее вошедшего обращения после закрытия допуска.
			admitted := make(chan error, 1)
			go func() {
				// Запускает выбранную команду до закрытия допуска и передаёт тесту ошибку её позднего завершения.

				// err содержит отказ выбранной команды, начатой до закрытия менеджера.
				var err error
				if kind == core.CommandStartSession {
					_, err = f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: wrapped.requestID, StreamID: f.stream.ID})
				} else {
					_, err = f.manager.AddOutput(ctx, core.AddOutputRequest{RequestID: wrapped.requestID, SessionID: parent.SessionID, DestinationID: f.destination.ID})
				}
				admitted <- err
			}()
			awaitPublication(t, ctx, wrapped.entered)
			// closed передаёт результат закрытия, которое должно дождаться вошедшего
			// обращения.
			closed := make(chan error, 1)
			go func() {
				// Закрывает Manager параллельно с ранее вошедшим задержанным обращением и передаёт результат ожидания.
				closed <- f.manager.Close(ctx)
			}()
			waitRetirementCondition(t, ctx, f.preparation.closed.Load)
			select {
			// err содержит преждевременный результат Close, появление которого нарушает
			// ожидание вошедшего обращения.
			case err := <-closed:
				t.Fatal("Close завершился до ранее вошедшего обращения", err)
			default:
			}
			release()
			select {
			// err содержит поздний отказ приёма после закрытия допуска.
			case err := <-admitted:
				if !errors.Is(err, core.ErrClosed) && (kind != core.CommandAddOutput || !errors.Is(err, core.ErrConflict)) {
					t.Fatal("закрывшийся Manager разрешил позднее принятие", err)
				}
			case <-ctx.Done():
				t.Fatal("обращение не завершилось после закрытия допуска", ctx.Err())
			}
			select {
			// err сообщает результат Close после завершения ранее вошедшего обращения.
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("Close не дождался завершившегося допуска", ctx.Err())
			}
			// err сообщает отказ транзакции проверки отсутствия поздней квитанции.
			if err := f.store.View(ctx, func(r core.Repositories) error {
				// Проверяет в настоящем репозитории отсутствие квитанции позднего обращения после закрытия допуска.

				// err содержит результат поиска квитанции отклонённого после закрытия
				// запроса.
				_, err := r.Runtime.Receipt(ctx, wrapped.requestID)
				if !errors.Is(err, core.ErrNotFound) {
					t.Fatal("позднее обращение оставило квитанцию", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// outputs содержит назначения после закрытия допуска; err сообщает отказ
			// проверки отсутствия позднего выхода.
			outputs, err := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: parent.SessionID}})
			if err != nil || len(outputs.Items) != 0 {
				t.Fatal("позднее обращение оставило выход", outputs, err)
			}
		})
	}
}
