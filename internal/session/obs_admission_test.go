package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// obsAdmissionResult переносит результат допуска из проверяемой горутины к владельцу сценария.
type obsAdmissionResult struct {
	// session сохраняет точный запуск, выбранный допуском.
	session core.Session
	// router указывает медиавход выбранного запуска при успехе.
	router *media.Router
	// err содержит исход допуска, включая отмену ожидания.
	err error
}

// awaitOBSAdmission ограничивает ожидание результата ctx и возвращает фактический исход допуска.
func awaitOBSAdmission(t *testing.T, ctx context.Context, results <-chan obsAdmissionResult) obsAdmissionResult {
	t.Helper()
	select {
	// result содержит завершившийся допуск вместе с выбранным запуском.
	case result := <-results:
		return result
	case <-ctx.Done():
		t.Fatal("допуск не завершился", ctx.Err())
		return obsAdmissionResult{}
	}
}

// beginOBSAdmission запускает один допуск и передаёт его исход в ограниченный канал без ожидания читателя.
func beginOBSAdmission(ctx context.Context, f managerFixture) <-chan obsAdmissionResult {
	// results удерживает единственный исход до чтения владельцем теста.
	results := make(chan obsAdmissionResult, 1)
	go func() {
		// Выполняет допуск с заданным сроком и сохраняет все три результата до завершения горутины.
		// session фиксирует выбранный запуск; router содержит его вход; err сообщает отказ допуска.
		session, router, err := f.manager.AdmitOBS(ctx, f.stream.ID, f.stream.PublicationKey)
		results <- obsAdmissionResult{session, router, err}
	}()
	return results
}

// TestOBSColdStartAndReconnectPreserveIntent проверяет выбор всех Enabled, повторное использование
// RUNNING и сохранение ручной паузы, остановки и отсоединения выходов при новом допуске OBS.
func TestOBSColdStartAndReconnectPreserveIntent(t *testing.T) {
	// f содержит настоящую временную SQLite, подготовленный файл и управляемые медиапроцессы.
	f := newManagerFixture(t, true, nil)
	// ctx ограничивает полный жизненный цикл проверки; cancel освобождает срок.
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	// second добавляет ещё одно включённое назначение той же подготовленной группы; err сообщает отказ.
	second, err := f.service.CreateDestination(ctx, core.Destination{Name: "Второй", StreamID: f.stream.ID, GroupID: f.destination.GroupID, Platform: "custom", Endpoint: "rtmp://127.0.0.1:1/live", StreamKey: "second", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// disabled задаёт выключенное назначение, которое автозапуск обязан исключить.
	disabled, err := f.service.CreateDestination(ctx, core.Destination{Name: "Выключенный", StreamID: f.stream.ID, GroupID: f.destination.GroupID, Platform: "custom", Endpoint: "rtmp://127.0.0.1:1/live", StreamKey: "disabled"})
	if err != nil {
		t.Fatal(err)
	}
	// session содержит холодный запуск; router предоставляет готовый локальный вход.
	session, router, err := f.manager.AdmitOBS(ctx, f.stream.ID, f.stream.PublicationKey)
	if err != nil || session.State != core.SessionRunning || router == nil {
		t.Fatal("автозапуск не выдал готовый вход", session, router, err)
	}
	// outputs содержит реальные сохранённые выходы после холодного запуска.
	outputs, err := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: session.ID}})
	if err != nil || len(outputs.Items) != 2 {
		t.Fatal("выбраны не все Enabled", outputs, err)
	}
	// selected сопоставляет назначение с сохранённым выходом для проверки выбора и управления.
	selected := make(map[core.ID]core.SessionOutput)
	// output содержит очередной снимок выбранного выхода.
	for _, output := range outputs.Items {
		selected[output.Snapshot.Destination.ID] = output
	}
	if selected[f.destination.ID].ID == "" || selected[second.ID].ID == "" || selected[disabled.ID].ID != "" {
		t.Fatal("набор выходов не совпал с Enabled", selected)
	}
	// stopped содержит принятую явную остановку первого выхода.
	stopped, err := f.manager.StopOutput(ctx, core.OutputCommand{RequestID: "obs-stop-output", OutputID: selected[f.destination.ID].ID})
	if err != nil || waitReceipt(t, f, stopped).Status != core.CommandSucceeded {
		t.Fatal("не удалось остановить выход", err)
	}
	// detached содержит принятую явную команду отсоединения второго выхода.
	detached, err := f.manager.DetachOutput(ctx, core.OutputCommand{RequestID: "obs-detach-output", OutputID: selected[second.ID].ID})
	if err != nil || waitReceipt(t, f, detached).Status != core.CommandSucceeded {
		t.Fatal("не удалось отсоединить выход", err)
	}
	// pause содержит принятую ручную паузу Session.
	pause, err := f.manager.PauseSession(ctx, core.SessionCommand{RequestID: "obs-pause", SessionID: session.ID})
	if err != nil || waitReceipt(t, f, pause).Status != core.CommandSucceeded {
		t.Fatal("не удалось поставить паузу", err)
	}
	// before сохраняет намерения, поколения и снимки выходов до переподключения.
	before, err := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: session.ID}})
	if err != nil {
		t.Fatal(err)
	}
	// commandsBefore сохраняет полный набор команд до повторного допуска.
	commandsBefore, err := f.service.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: session.ID}})
	if err != nil {
		t.Fatal(err)
	}
	// again содержит тот же запуск; againRouter должен сохранить локальную идентичность входа.
	again, againRouter, err := f.manager.AdmitOBS(ctx, f.stream.ID, f.stream.PublicationKey)
	if err != nil || again.ID != session.ID || again.Lifecycle != session.Lifecycle || !again.DesiredPaused || againRouter != router {
		t.Fatal("переподключение изменило запуск или паузу", again, err)
	}
	// after содержит сохранённые выходы после переподключения для точного сравнения.
	after, err := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: session.ID}})
	if err != nil || !reflect.DeepEqual(before.Items, after.Items) {
		t.Fatal("переподключение изменило выходы", before, after, err)
	}
	// commandsAfter позволяет обнаружить скрытый Resume, StartOutput либо AddOutput.
	commandsAfter, err := f.service.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: session.ID}})
	if err != nil || !reflect.DeepEqual(commandsBefore.Items, commandsAfter.Items) {
		t.Fatal("переподключение создало управляющую команду", commandsAfter, err)
	}
	// stop содержит существующую API-команду завершения эфира.
	stop, err := f.manager.StopSession(ctx, core.SessionCommand{RequestID: "obs-stop-session", SessionID: session.ID})
	if err != nil || waitReceipt(t, f, stop).Status != core.CommandSucceeded {
		t.Fatal("не удалось завершить эфир", err)
	}
	// fresh содержит следующий эфир после явной остановки; freshRouter должен принадлежать ему.
	fresh, freshRouter, err := f.manager.AdmitOBS(ctx, f.stream.ID, f.stream.PublicationKey)
	if err != nil || fresh.ID == session.ID || freshRouter == router || fresh.DesiredPaused {
		t.Fatal("следующая публикация не запустила новый эфир", fresh, err)
	}
}

// TestOBSInvalidKeyPrecedesFilesAndWrites проверяет отказ ключа при удалённом обязательном файле:
// файловая ошибка не должна скрыть авторизацию, а команды и Session не должны появиться.
func TestOBSInvalidKeyPrecedesFilesAndWrites(t *testing.T) {
	// f содержит включённый выход, поэтому валидный холодный запуск потребует подготовленный файл.
	f := newManagerFixture(t, true, nil)
	// ctx ограничивает проверки ключа; cancel освобождает срок.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// variant определяет удаляемый файл; err сообщает отказ чтения манифеста.
	variant, err := f.service.GetVariant(ctx, f.binding.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.files.RemoveExact(ctx, variant.ManagedPath, variant.SizeBytes, variant.ContentHash); err != nil {
		t.Fatal(err)
	}
	// key задаёт непустой неверный либо пустой постоянный ключ.
	for _, key := range []string{"wrong-key", ""} {
		if _, _, err = f.manager.AdmitOBS(ctx, f.stream.ID, key); !errors.Is(err, core.ErrNotReady) {
			t.Fatal("ключ не проверен до файлов", key, err)
		}
	}
	// sessions содержит историю запусков после обоих отказов ключа.
	sessions, err := f.service.ListSessions(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, StreamID: f.stream.ID}})
	if err != nil || len(sessions.Items) != 0 {
		t.Fatal("неверный ключ создал Session", sessions, err)
	}
	// receipts содержит команды, которые не должны появиться при ошибке первоначальной авторизации.
	receipts, err := f.service.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8}})
	if err != nil || len(receipts.Items) != 0 {
		t.Fatal("неверный ключ создал квитанцию", receipts, err)
	}
}

// TestOBSAdmissionAndStartEnterRespectHeldMutex проверяет отмену допуска и предзаписного StartSession
// при занятом mutex Manager, а также отмену допуска при занятом mutex уже работающего узла.
func TestOBSAdmissionAndStartEnterRespectHeldMutex(t *testing.T) {
	// f содержит работающий запуск, используемый после проверки входного mutex.
	f := newManagerFixture(t, false, nil)
	// ctx ограничивает проверку завершения отменённых вызовов; cancel освобождает срок.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	f.manager.mu.Lock()
	// unlock освобождает проверочный захват также при аварийном завершении утверждения.
	unlock := sync.OnceFunc(f.manager.mu.Unlock)
	defer unlock()
	// callCtx получает короткий срок, который должен истечь при всё ещё занятом mutex; stop освобождает срок.
	callCtx, stop := context.WithTimeout(ctx, 80*time.Millisecond)
	// admission передаёт исход OBS, ожидающего mutex Manager.
	admission := beginOBSAdmission(callCtx, f)
	// started передаёт исход обычного StartSession, ожидающего тот же mutex.
	started := make(chan error, 1)
	go func() {
		// Проверяет контекстный вход существующего предзаписного пути StartSession.
		// err содержит отказ отменённого входа до создания команды.
		_, err := f.manager.StartSession(callCtx, core.StartSessionRequest{RequestID: "mutex-start", StreamID: f.stream.ID})
		started <- err
	}()
	// result должен прийти до освобождения Manager.mu, иначе контекстное ожидание не работает.
	result := awaitOBSAdmission(t, ctx, admission)
	if !errors.Is(result.err, context.DeadlineExceeded) {
		unlock()
		t.Fatal(result.err)
	}
	select {
	// err содержит фактический исход отменённого StartSession.
	case err := <-started:
		if !errors.Is(err, context.DeadlineExceeded) {
			unlock()
			t.Fatal(err)
		}
	case <-ctx.Done():
		unlock()
		t.Fatal("StartSession.enter не прерван", ctx.Err())
	}
	unlock()
	stop()
	// session содержит успешный запуск после отменённых попыток.
	session, _, err := f.manager.AdmitOBS(ctx, f.stream.ID, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	f.manager.mu.Lock()
	// node указывает локальный узел готового запуска.
	node := f.manager.sessions[session.ID]
	f.manager.mu.Unlock()
	node.mu.Lock()
	// unlockNode гарантирует снятие искусственной задержки при любом завершении теста.
	unlockNode := sync.OnceFunc(node.mu.Unlock)
	defer unlockNode()
	// nodeCtx ограничивает ожидание занятого mutex узла; nodeCancel освобождает срок.
	nodeCtx, nodeCancel := context.WithTimeout(ctx, 80*time.Millisecond)
	// blocked передаёт исход допуска при занятом node.mu.
	blocked := beginOBSAdmission(nodeCtx, f)
	result = awaitOBSAdmission(t, ctx, blocked)
	unlockNode()
	nodeCancel()
	if !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatal("node.mu скрыл отмену", result.err)
	}
}

// TestOBSStartupOutcomeAndClientCancellation проверяет, что задержанный Complete не выдаёт ранний
// Feed, отказ запуска не становится успехом, а отмена публикации не останавливает принятую Session.
func TestOBSStartupOutcomeAndClientCancellation(t *testing.T) {
	// test выбирает успешный, неоднозначный либо откатившийся Complete.
	for _, test := range []struct {
		// name задаёт исход завершения запуска для диагностики подтеста.
		name string
		// reject откатывает Complete; ambiguous скрывает его успешное подтверждение.
		reject, ambiguous bool
	}{
		{name: "успех"},
		{name: "потерянное подтверждение", ambiguous: true},
		{name: "отказ", reject: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Проверяет готовность, отмену ожидания и повторный допуск того же запуска после исхода Complete.
			// f предоставляет подготовленную группу и включённое назначение для реального fallback-барьера.
			f := newManagerFixture(t, true, nil)
			// wrapped задерживает выдачу и Complete, сохраняя реальные транзакции и медиаготовность.
			wrapped := &publicationStore{Store: f.store, issued: newPublicationBoundary(), beforeCommit: newPublicationBoundary(), afterCommit: newPublicationBoundary(), reject: test.reject, ambiguous: test.ambiguous}
			f = replaceAdmissionManager(t, f, wrapped)
			t.Cleanup(func() {
				// Разрешает все задержанные этапы перед очисткой Manager даже после ошибки утверждения.
				wrapped.issued.unblock()
				wrapped.beforeCommit.unblock()
				wrapped.afterCommit.unblock()
			})
			// ctx ограничивает полный сценарий; cancel освобождает срок.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// client ограничивает только допуск публикации; disconnect моделирует уход клиента после принятия.
			client, disconnect := context.WithCancel(ctx)
			defer disconnect()
			// pending передаёт исход холодного допуска OBS.
			pending := beginOBSAdmission(client, f)
			awaitPublication(t, ctx, wrapped.issued.entered)
			wrapped.issued.unblock()
			awaitPublication(t, ctx, wrapped.beforeCommit.entered)
			// acceptedCommands сохраняет исходный start_session и законные первоначальные start_output
			// до отмены клиента; Complete в этот момент ещё не может изменить квитанции.
			acceptedCommands, err := f.service.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8}})
			if err != nil {
				t.Fatal(err)
			}
			select {
			// early обнаруживает недопустимую выдачу входа до долговечного Complete.
			case early := <-pending:
				t.Fatal("допуск завершился до Complete", early)
			default:
			}
			disconnect()
			// abandoned должен завершиться по отмене, хотя Complete всё ещё удерживает node.mu.
			abandoned := awaitOBSAdmission(t, ctx, pending)
			if !errors.Is(abandoned.err, context.Canceled) || abandoned.router != nil {
				t.Fatal("Complete скрыл отмену публикации", abandoned)
			}
			// sessions сохраняет единственный принятый запуск после ухода клиента.
			sessions, err := f.service.ListSessions(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, StreamID: f.stream.ID}})
			if err != nil || len(sessions.Items) != 1 || sessions.Items[0].State != core.SessionStarting {
				t.Fatal("отмена клиента компенсировала принятый запуск", sessions, err)
			}
			// canceledCommands обнаруживает новую компенсационную команду до снятия барьера Complete.
			canceledCommands, err := f.service.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8}})
			if err != nil || !reflect.DeepEqual(acceptedCommands.Items, canceledCommands.Items) {
				t.Fatal("отмена клиента изменила команды принятого запуска", acceptedCommands, canceledCommands, err)
			}
			wrapped.beforeCommit.unblock()
			if !test.reject {
				awaitPublication(t, ctx, wrapped.afterCommit.entered)
				// holdCtx ограничивает второй допуск во время задержанного возврата Complete; holdCancel освобождает срок.
				holdCtx, holdCancel := context.WithTimeout(ctx, 80*time.Millisecond)
				// waiting должен дождаться локального исхода, даже если RUNNING уже сохранён в SQLite.
				waiting := awaitOBSAdmission(t, ctx, beginOBSAdmission(holdCtx, f))
				holdCancel()
				if !errors.Is(waiting.err, context.DeadlineExceeded) || waiting.router != nil {
					t.Fatal("допуск опередил неизменяемый локальный исход", waiting)
				}
			}
			wrapped.afterCommit.unblock()
			// receipts содержит исходную команду запуска для ожидания её окончательного результата.
			receipts, err := f.service.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: sessions.Items[0].ID}})
			if err != nil {
				t.Fatal(err)
			}
			// start выделяет исходный start_session среди первоначальных команд выходов.
			var start core.CommandReceipt
			// receipt содержит очередную квитанцию для проверки единственности исходного запуска.
			for _, receipt := range receipts.Items {
				if receipt.Kind == core.CommandStartSession {
					if start.ID != "" {
						t.Fatal("созданы два запуска", receipts)
					}
					start = receipt
				}
			}
			if start.ID == "" {
				t.Fatal("нет команды принятого запуска", receipts)
			}
			// completed содержит окончательный результат исходного запуска.
			completed := waitReceipt(t, f, start)
			if test.reject {
				if completed.Status != core.CommandFailed {
					t.Fatal("отказ Complete стал успешным", completed)
				}
				return
			}
			if completed.Status != core.CommandSucceeded {
				t.Fatal("принятый запуск не пережил отмену клиента", completed)
			}
			// current содержит тот же готовый запуск; router подтверждает его локальную медиаготовность.
			current, router, err := f.manager.AdmitOBS(ctx, f.stream.ID, f.stream.PublicationKey)
			if err != nil || current.ID != sessions.Items[0].ID || router == nil {
				t.Fatal("потерян принятый запуск", current, err)
			}
		})
	}
}

// obsRaceKey маркирует единственный проверяемый запрос, не затрагивая фоновые чтения Manager.
type obsRaceKey struct{}

// obsRaceStore моделирует первоначально отсутствовавшую Session и последующий результат конкуренции.
type obsRaceStore struct {
	// Store сохраняет настоящие транзакции SQLite и общую RuntimeAuthority.
	*storage.Store
	// calls считает обращения ActiveSession только маркированного запроса.
	calls atomic.Int32
	// failure подменяет результат конкурирующего запуска во втором чтении.
	failure error
}

// View подставляет контролируемый ActiveSession только для маркированного запроса допуска.
func (s *obsRaceStore) View(ctx context.Context, callback func(core.Repositories) error) error {
	return s.Store.View(ctx, func(r core.Repositories) error {
		// Передаёт реальные репозитории с узкой подменой результата ActiveSession.
		r.Runtime = obsRaceRepository{r.Runtime, s}
		return callback(r)
	})
}

// Write сохраняет реальную транзакционную границу и подменяет проверку конкурирующей Session.
func (s *obsRaceStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	return s.Store.Write(ctx, func(r core.Repositories) error {
		// Передаёт реальный набор репозиториев, меняя только контролируемую проверку конкуренции.
		r.Runtime = obsRaceRepository{r.Runtime, s}
		return callback(r)
	})
}

// obsRaceRepository изменяет только ActiveSession внутри настоящего набора репозиториев.
type obsRaceRepository struct {
	// RuntimeRepository выполняет остальные операции над настоящей SQLite.
	core.RuntimeRepository
	// owner определяет исходы первых двух проверок маркированного запроса.
	owner *obsRaceStore
}

// ActiveSession сначала возвращает отсутствие, затем выбранный отказ запуска, далее реальную Session.
func (r obsRaceRepository) ActiveSession(ctx context.Context, stream core.ID) (core.Session, error) {
	if ctx.Value(obsRaceKey{}) != nil {
		// call различает первоначальный допуск, транзакционный StartSession и разрешённое перечитывание.
		call := r.owner.calls.Add(1)
		if call == 1 {
			return core.Session{}, core.ErrNotFound
		}
		if call == 2 {
			return core.Session{}, r.owner.failure
		}
	}
	return r.RuntimeRepository.ActiveSession(ctx, stream)
}

// TestOBSConcurrentStartJoinsOnlyExactInUse отличает точный конфликт конкуренции от обёрнутой,
// составной и прочих ошибок. Только точный ErrInUse разрешает дополнительное чтение и присоединение.
func TestOBSConcurrentStartJoinsOnlyExactInUse(t *testing.T) {
	// test определяет ошибку транзакции конкурирующего запуска и возможность присоединения.
	for _, test := range []struct {
		// name задаёт понятное имя источника отказа.
		name string
		// failure возвращается вместо ответа проверки активной Session в StartSession.
		failure error
		// join разрешён только для точного контрактного ErrInUse.
		join bool
	}{
		{"exact", core.ErrInUse, true},
		{"wrapped", fmt.Errorf("конфликт: %w", core.ErrInUse), false},
		{"joined", errors.Join(core.ErrInUse, core.ErrConflict), false},
		{"conflict", core.ErrConflict, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Проверяет один источник ошибки на реальной уже работающей конкурирующей Session.
			// f предоставляет базу и ресурсы для запуска победителя конкуренции.
			f := newManagerFixture(t, false, nil)
			// wrapped управляет ответами только маркированного допуска.
			wrapped := &obsRaceStore{Store: f.store, failure: test.failure}
			f = replaceAdmissionManager(t, f, wrapped)
			// winner содержит настоящий работающий запуск конкурента.
			winner := startAdmissionParent(t, f)
			// ctx маркирует текущий запрос, сохраняя независимость фоновой сверки.
			ctx := context.WithValue(context.Background(), obsRaceKey{}, true)
			// selected содержит выбранный запуск; router содержит его вход; err сообщает результат гонки.
			selected, router, err := f.manager.AdmitOBS(ctx, f.stream.ID, f.stream.PublicationKey)
			if test.join {
				if err != nil || selected.ID != winner.SessionID || router == nil || wrapped.calls.Load() != 3 {
					t.Fatal("точный ErrInUse не присоединил победителя", selected, err, wrapped.calls.Load())
				}
			} else if err != test.failure || router != nil || wrapped.calls.Load() != 2 {
				t.Fatal("посторонняя ошибка скрыта перечитыванием", selected, err, wrapped.calls.Load())
			}
		})
	}
}

// obsSelectionStore задерживает возврат первого маркированного согласованного чтения после
// освобождения настоящей транзакции. Это позволяет воспроизвести смену запуска без удержания SQLite.
type obsSelectionStore struct {
	// Store сохраняет настоящие чтения, записи и общую RuntimeAuthority.
	*storage.Store
	// boundary управляет возвратом первоначального авторизованного выбора.
	boundary publicationBoundary
	// delayed ограничивает задержку единственным маркированным чтением.
	delayed atomic.Bool
}

// View сохраняет точный результат первого чтения, затем разрешает конкуренту изменить БД до его возврата.
func (s *obsSelectionStore) View(ctx context.Context, callback func(core.Repositories) error) error {
	// delay отмечает единственное маркированное чтение, которое нужно задержать после транзакции.
	delay := ctx.Value(obsRaceKey{}) != nil && s.delayed.CompareAndSwap(false, true)
	// err сохраняет настоящий исход согласованного чтения до управляемой задержки ответа.
	err := s.Store.View(ctx, callback)
	if delay {
		s.boundary.hold()
	}
	return err
}

// TestOBSConcurrentAPIStartUsesAtomicWinner проверяет реальную конкуренцию: OBS видит отсутствие,
// API успевает принять запуск, а OBS присоединяется к нему без второго Session или start_session.
func TestOBSConcurrentAPIStartUsesAtomicWinner(t *testing.T) {
	// f содержит пустую историю запусков и настоящую временную SQLite.
	f := newManagerFixture(t, false, nil)
	// wrapped задерживает ответ первоначального чтения OBS после закрытия read-транзакции.
	wrapped := &obsSelectionStore{Store: f.store, boundary: newPublicationBoundary()}
	f = replaceAdmissionManager(t, f, wrapped)
	defer wrapped.boundary.unblock()
	// ctx ограничивает сценарий; cancel освобождает срок.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// marked выделяет первоначальное чтение OBS среди фоновой сверки.
	marked := context.WithValue(ctx, obsRaceKey{}, true)
	// waiting переносит исход OBS, уже увидевшего отсутствие активной Session.
	waiting := beginOBSAdmission(marked, f)
	awaitPublication(t, ctx, wrapped.boundary.entered)
	// winner содержит настоящий запуск, принятый существующим управляющим API.
	winner := startAdmissionParent(t, f)
	wrapped.boundary.unblock()
	// joined содержит результат OBS после выигрыша API в транзакционной границе.
	joined := awaitOBSAdmission(t, ctx, waiting)
	if joined.err != nil || joined.session.ID != winner.SessionID || joined.router == nil {
		t.Fatal("OBS не присоединился к победителю API", joined)
	}
	// sessions подтверждает единственный долговечный запуск после гонки.
	sessions, err := f.service.ListSessions(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, StreamID: f.stream.ID}})
	if err != nil || len(sessions.Items) != 1 || sessions.Items[0].ID != winner.SessionID {
		t.Fatal("гонка создала второй запуск", sessions, err)
	}
	// receipts подтверждает отсутствие второй команды запуска при пустом наборе назначений.
	receipts, err := f.service.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: winner.SessionID}})
	if err != nil || len(receipts.Items) != 1 || receipts.Items[0].ID != winner.ID {
		t.Fatal("гонка создала вторую команду запуска", receipts, err)
	}
}

// TestOBSSelectedSessionNeverHopsAcrossStopStart проверяет остановку и новый запуск между
// первоначальным выбором OBS и ожиданием готовности: старый допуск не должен получить новый Router.
func TestOBSSelectedSessionNeverHopsAcrossStopStart(t *testing.T) {
	// f предоставляет управляемый локальный Manager и пустую историю.
	f := newManagerFixture(t, false, nil)
	// wrapped задерживает возврат точного первоначального выбора OBS.
	wrapped := &obsSelectionStore{Store: f.store, boundary: newPublicationBoundary()}
	f = replaceAdmissionManager(t, f, wrapped)
	defer wrapped.boundary.unblock()
	// old содержит исходный работающий эфир.
	old := startAdmissionParent(t, f)
	// ctx ограничивает сценарий; cancel освобождает срок.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// marked выделяет задерживаемый допуск OBS.
	marked := context.WithValue(ctx, obsRaceKey{}, true)
	// waiting уже выбрал старую Session до задержки возврата InspectOBS.
	waiting := beginOBSAdmission(marked, f)
	awaitPublication(t, ctx, wrapped.boundary.entered)
	// stop содержит явное завершение старого эфира через существующий API; err сообщает отказ.
	stop, err := f.manager.StopSession(ctx, core.SessionCommand{RequestID: "selected-stop", SessionID: old.SessionID})
	if err != nil || waitReceipt(t, f, stop).Status != core.CommandSucceeded {
		t.Fatal("не удалось завершить старый эфир", err)
	}
	// fresh содержит новый эфир, созданный отдельной публикацией; router принадлежит новому запуску.
	fresh, router, err := f.manager.AdmitOBS(ctx, f.stream.ID, f.stream.PublicationKey)
	if err != nil || fresh.ID == old.SessionID || router == nil {
		t.Fatal("нет нового эфира", fresh, err)
	}
	wrapped.boundary.unblock()
	// rejected содержит результат старого запроса после смены запуска.
	rejected := awaitOBSAdmission(t, ctx, waiting)
	if !errors.Is(rejected.err, core.ErrStale) || rejected.router != nil {
		t.Fatal("старый допуск перескочил на новый запуск", rejected)
	}
	// current подтверждает сохранность нового эфира после отказа старого допуска.
	current, err := f.service.GetSession(ctx, fresh.ID)
	if err != nil || current.State != core.SessionRunning {
		t.Fatal("старый допуск повредил новый эфир", current, err)
	}
}

// TestOBSManagerCloseCancelsAdmissionBeforeMutexBarrier проверяет, что действительное закрытие
// Manager прерывает ожидающие допуски до освобождения занятого m.mu, без отмены контекста публикации.
func TestOBSManagerCloseCancelsAdmissionBeforeMutexBarrier(t *testing.T) {
	// f содержит Manager без запущенных медиаресурсов.
	f := newManagerFixture(t, false, nil)
	// guard ограничивает только ожидания теста; cancel освобождает срок, не отменяя контекст допуска.
	guard, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f.manager.mu.Lock()
	// unlock гарантирует снятие искусственного барьера также после ошибки утверждения.
	unlock := sync.OnceFunc(f.manager.mu.Unlock)
	defer unlock()
	// waiting использует контекст без срока, чтобы единственной причиной отмены был Manager.Close.
	waiting := beginOBSAdmission(context.Background(), f)
	// closed передаёт результат настоящего Close, ожидающего тот же mutex.
	closed := make(chan error, 1)
	go func() {
		// Начинает закрытие и сохраняет исход до присоединения владельцем проверки.
		closed <- f.manager.Close(guard)
	}()
	// rejected должен вернуться до unlock: фактический Close уже обязан быть виден admission.
	rejected := awaitOBSAdmission(t, guard, waiting)
	if !errors.Is(rejected.err, core.ErrClosed) && !errors.Is(rejected.err, context.Canceled) || rejected.router != nil {
		t.Fatal("закрытие Manager не прервало допуск", rejected)
	}
	unlock()
	select {
	// err сообщает исход закрытия после снятия барьера регистрации.
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-guard.Done():
		t.Fatal("Manager.Close не присоединён", guard.Err())
	}
}

// obsReceiptDelayStore задерживает ответ после настоящего COMMIT выбранной стадии квитанции.
type obsReceiptDelayStore struct {
	// Store выполняет настоящие транзакции, освобождая SQLite и RuntimeAuthority до задержки.
	*storage.Store
	// kind и status выбирают стадию команды для воспроизведения конкуренции.
	kind   core.CommandKind
	status core.CommandStatus
	// insertion выбирает вставку квитанции вместо обновления существующей.
	insertion bool
	// boundary сообщает о состоявшемся COMMIT и разрешает возврат ответа.
	boundary publicationBoundary
	// delayed ограничивает инъекцию одним ответом успешно сохранённой операции.
	delayed atomic.Bool
	// returnErr имитирует утрату ответа после успешного COMMIT, когда задана.
	returnErr error
}

// Write сохраняет настоящий результат и один раз задерживает возврат выбранной квитанции после COMMIT.
func (s *obsReceiptDelayStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	// matched отмечает выбранную стадию внутри текущей транзакции.
	var matched bool
	// err содержит настоящий исход транзакции до возможной потери подтверждения.
	err := s.Store.Write(ctx, func(r core.Repositories) error {
		// Подставляет наблюдение квитанции, не меняя реальные операции SQLite.
		matched = false
		r.Runtime = obsReceiptDelayRepository{r.Runtime, s, &matched}
		return callback(r)
	})
	if err == nil && matched && s.delayed.CompareAndSwap(false, true) {
		s.boundary.hold()
		return s.returnErr
	}
	return err
}

// obsReceiptDelayRepository отмечает выбранные вставку или обновление реальной квитанции.
type obsReceiptDelayRepository struct {
	// RuntimeRepository сохраняет все настоящие операции исполнения.
	core.RuntimeRepository
	// owner задаёт выбранную стадию и тип операции.
	owner *obsReceiptDelayStore
	// matched связывает успешную запись с задержкой ответа текущей транзакции.
	matched *bool
}

// InsertReceipt сохраняет квитанцию и отмечает выбранную первоначальную запись после её успеха.
func (r obsReceiptDelayRepository) InsertReceipt(ctx context.Context, receipt core.CommandReceipt) error {
	// err сообщает настоящий отказ вставки до отметки инъекции.
	err := r.RuntimeRepository.InsertReceipt(ctx, receipt)
	if err == nil && r.owner.insertion && receipt.Kind == r.owner.kind && receipt.Status == r.owner.status {
		*r.matched = true
	}
	return err
}

// UpdateReceipt сохраняет обновление и отмечает выбранный переход стадии после его успеха.
func (r obsReceiptDelayRepository) UpdateReceipt(ctx context.Context, receipt core.CommandReceipt, expected uint64) error {
	// err сообщает настоящий отказ изменения квитанции до отметки инъекции.
	err := r.RuntimeRepository.UpdateReceipt(ctx, receipt, expected)
	if err == nil && !r.owner.insertion && receipt.Kind == r.owner.kind && receipt.Status == r.owner.status {
		*r.matched = true
	}
	return err
}

// TestOBSStoppingRejectsCurrentPublishThenAllowsNext проверяет реальную STOPPING под задержанной
// выдачей команды остановки: текущий publish отклоняется без нового запуска, следующий запускает эфир.
func TestOBSStoppingRejectsCurrentPublishThenAllowsNext(t *testing.T) {
	// f предоставляет сессию без выходов для точного контроля остановки.
	f := newManagerFixture(t, false, nil)
	// wrapped задерживает ответ захвата stop после COMMIT, пока Session ещё STOPPING.
	wrapped := &obsReceiptDelayStore{Store: f.store, kind: core.CommandStopSession, status: core.CommandClaimed, boundary: newPublicationBoundary()}
	f = replaceAdmissionManager(t, f, wrapped)
	defer wrapped.boundary.unblock()
	// old содержит исходный работающий эфир.
	old := startAdmissionParent(t, f)
	// ctx ограничивает сценарий; cancel освобождает срок.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// stop содержит принятую явную команду завершения; err сообщает отказ API.
	stop, err := f.manager.StopSession(ctx, core.SessionCommand{RequestID: "stopping-publish", SessionID: old.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	awaitPublication(t, ctx, wrapped.boundary.entered)
	// stopping доказывает актуальную долговечную STOPPING до проверки допуска OBS.
	stopping, err := f.service.GetSession(ctx, old.SessionID)
	if err != nil || stopping.State != core.SessionStopping {
		t.Fatal("не достигнута STOPPING", stopping, err)
	}
	// rejected содержит результат публикации во время явной остановки.
	rejected := awaitOBSAdmission(t, ctx, beginOBSAdmission(ctx, f))
	if rejected.err == nil || rejected.router != nil {
		t.Fatal("STOPPING принял публикацию", rejected)
	}
	// sessions обнаруживает недопустимый новый запуск внутри того же publish.
	sessions, err := f.service.ListSessions(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, StreamID: f.stream.ID}})
	if err != nil || len(sessions.Items) != 1 {
		t.Fatal("STOPPING создал новый запуск", sessions, err)
	}
	wrapped.boundary.unblock()
	if waitReceipt(t, f, stop).Status != core.CommandSucceeded {
		t.Fatal("явная остановка не завершилась")
	}
	// fresh содержит эфир следующей отдельной публикации после завершения остановки.
	fresh, router, err := f.manager.AdmitOBS(ctx, f.stream.ID, f.stream.PublicationKey)
	if err != nil || fresh.ID == old.SessionID || router == nil {
		t.Fatal("следующая публикация не начала эфир", fresh, err)
	}
}

// TestOBSCancellationAfterStartCommitKeepsAcceptedSession проверяет отмену контекста вызова и потерю ответа
// уже сохранённого StartSession. Независимая сверка обязана найти тот же запуск без Stop-компенсации.
func TestOBSCancellationAfterStartCommitKeepsAcceptedSession(t *testing.T) {
	// f содержит Manager и настоящий атомарный путь принятия запуска.
	f := newManagerFixture(t, false, nil)
	// wrapped теряет подтверждение первоначального start_session после успешного COMMIT.
	wrapped := &obsReceiptDelayStore{Store: f.store, kind: core.CommandStartSession, status: core.CommandPending, insertion: true, boundary: newPublicationBoundary(), returnErr: context.Canceled}
	f = replaceAdmissionManager(t, f, wrapped)
	defer wrapped.boundary.unblock()
	// ctx ограничивает сценарий; cancel освобождает срок.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// client ограничивает только ожидающий ответ OBS; disconnect моделирует уход после COMMIT.
	client, disconnect := context.WithCancel(ctx)
	defer disconnect()
	// pending передаёт результат допуска, ещё не получившего подтверждение принятия запуска.
	pending := beginOBSAdmission(client, f)
	awaitPublication(t, ctx, wrapped.boundary.entered)
	// accepted подтверждает реальную сохранённую Session до отмены контекста вызова.
	accepted, err := f.service.ListSessions(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, StreamID: f.stream.ID}})
	if err != nil || len(accepted.Items) != 1 {
		t.Fatal("COMMIT не сохранил единственный запуск", accepted, err)
	}
	disconnect()
	wrapped.boundary.unblock()
	// abandoned содержит ожидаемый отказ публикации, который не владеет принятой Session.
	abandoned := awaitOBSAdmission(t, ctx, pending)
	if !errors.Is(abandoned.err, context.Canceled) || abandoned.router != nil {
		t.Fatal("отменённая публикация получила вход", abandoned)
	}
	// current содержит тот же запуск после независимой сверки потерянного ответа.
	current, router, err := f.manager.AdmitOBS(ctx, f.stream.ID, f.stream.PublicationKey)
	if err != nil || current.ID != accepted.Items[0].ID || current.State != core.SessionRunning || router == nil {
		t.Fatal("отмена после COMMIT остановила принятый эфир", current, err)
	}
	// receipts содержит единственную команду исходного запуска без компенсационного Stop.
	receipts, err := f.service.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: current.ID}})
	if err != nil || len(receipts.Items) != 1 || receipts.Items[0].Kind != core.CommandStartSession || receipts.Items[0].Status != core.CommandSucceeded {
		t.Fatal("отмена создала Stop или повторный Start", receipts, err)
	}
}
