package session

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// startupOutputReadStore задерживает возврат прочитанного выхода и моделирует отказ повторной проверки
// исключения из начального барьера.
type startupOutputReadStore struct {
	// Store выполняет настоящее чтение до управляемой задержки его ответа.
	*storage.Store
	// armed включает однократный захват и задержку следующего прочитанного выхода.
	armed atomic.Bool
	// readFailure включает ErrBusy для повторных чтений захваченного выхода.
	readFailure atomic.Bool
	// rechecks считает чтения выхода, отклонённые искусственным отказом.
	rechecks atomic.Uint32
	// captured передаёт снимок выхода, прочитанный до конкурентной остановки.
	captured chan core.SessionOutput
	// release разрешает вернуть ранее прочитанный снимок после конкурентного изменения.
	release chan struct{}
	// once защищает однократное снятие задержки чтения.
	once sync.Once
	// captureList включает захват первого выхода при чтении списка до создания членства.
	captureList bool
}

// unblock однократно разрешает возврат задержанного чтения выхода.
func (s *startupOutputReadStore) unblock() {
	s.once.Do(func() {
		// Однократно закрывает канал разрешения задержанного чтения выхода.
		close(s.release)
	})
}

// View захватывает прочитанный снимок выхода и при включённой задержке удерживает возврат после
// освобождения транзакции. Отдельный режим возвращает ErrBusy при повторных чтениях для проверки
// недоказанного исключения из барьера.
func (s *startupOutputReadStore) View(ctx context.Context, callback func(core.Repositories) error) error {
	// output сохраняет адрес выхода, прочитанного внутри текущей транзакции наблюдения.
	var output *core.SessionOutput
	// err сообщает результат настоящего чтения до искусственной задержки или отказа.
	err := s.Store.View(ctx, func(r core.Repositories) error {
		// Подставляет репозиторий захвата снимка выхода и выполняет исходное чтение.

		r.Runtime = startupOutputReadRepository{RuntimeRepository: r.Runtime, output: &output, captureList: s.captureList}
		return callback(r)
	})
	if err != nil || output == nil {
		return err
	}
	if s.readFailure.Load() {
		s.rechecks.Add(1)
		return core.ErrBusy
	}
	if s.armed.CompareAndSwap(true, false) {
		// Снимок уже прочитан, а транзакция и RuntimeAuthority освобождены перед Stop.
		s.captured <- *output
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// startupOutputReadRepository сохраняет прочитанный выход для управляемой гонки начального барьера с
// остановкой.
type startupOutputReadRepository struct {
	// RuntimeRepository выполняет настоящие чтения выполнения под обёрткой захвата снимка.
	core.RuntimeRepository
	// output указывает место сохранения адреса успешно прочитанного выхода.
	output **core.SessionOutput
	// captureList разрешает захват первого элемента при чтении списка выходов.
	captureList bool
}

// Output сохраняет адрес успешно прочитанного снимка выхода для управляемой задержки возврата View.
func (r startupOutputReadRepository) Output(ctx context.Context, id core.ID) (core.SessionOutput, error) {
	// output содержит настоящий снимок выхода; err сообщает отказ его чтения до возможного захвата.
	output, err := r.RuntimeRepository.Output(ctx, id)
	if err == nil {
		*r.output = &output
	}
	return output, err
}

// Outputs сохраняет первый выход списка при включённом захвате списка, чтобы остановка могла опередить
// создание членства.
func (r startupOutputReadRepository) Outputs(ctx context.Context, query core.ListQuery) ([]core.SessionOutput, error) {
	// outputs содержит настоящий список выходов; err сообщает отказ чтения списка до возможного
	// захвата первого элемента.
	outputs, err := r.RuntimeRepository.Outputs(ctx, query)
	if err == nil && r.captureList && len(outputs) > 0 {
		*r.output = &outputs[0]
	}
	return outputs, err
}

// TestManagerStartupRevalidatesFailedMembershipAfterOutputRead проверяет повторное чтение после отмены
// членства: сохранённые StopOutput и DetachOutput исключают выход, а действующая ошибка или отказ
// повторного чтения завершают общий запуск.
func TestManagerStartupRevalidatesFailedMembershipAfterOutputRead(t *testing.T) {
	// test задаёт остановку либо отказ членства и ожидаемый состав продолжающих запуск выходов.
	for _, test := range []struct {
		// name задаёт имя сочетания остановки, соседнего выхода и отказа повторной проверки.
		name string
		// remaining задаёт число соседних выходов, продолжающих начальный запуск.
		remaining int
		// detach выбирает отсоединение вместо остановки; active оставляет выход долговечно
		// активным при отмене членства; failed включает отказ повторного чтения.
		detach, active, failed bool
	}{
		{name: "stop_last"},
		{name: "detach_last", detach: true},
		{name: "stop_shared", remaining: 1},
		{name: "detach_shared", remaining: 1, detach: true},
		{name: "active_failure", active: true},
		{name: "unconfirmed_exclusion", failed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Проверяет выбранное сочетание остановки, отсоединения, соседнего выхода и отказа повторного чтения
			// при запуске.

			// entered сообщает о входе построителя проигрывателя; releasePlayer разрешает
			// ему продолжить после проверки старого снимка.
			entered, releasePlayer := make(chan struct{}), make(chan struct{})
			// playerEntered ограничивает сигнал входа одним закрытием; playerReleased
			// ограничивает одним закрытием разрешение построителя.
			var playerEntered, playerReleased sync.Once
			// unblockPlayer однократно разрешает задержанному построителю проигрывателя
			// продолжить работу.
			unblockPlayer := func() {
				// Разрешает построителю проигрывателя продолжить работу с защитой от повторного закрытия канала.

				playerReleased.Do(func() {
					// Однократно закрывает канал разрешения задержанного проигрывателя.
					close(releasePlayer)
				})
			}
			// f предоставляет включённый выход с управляемо задержанным построителем
			// проигрывателя.
			f := newManagerFixture(t, true, func(c *Config) {
				// Подменяет построитель команды управляемым ожиданием до проверки устаревшего снимка выхода.

				// original сохраняет исходный построитель команды для продолжения после
				// задержки.
				original := c.Group.Player
				c.Group.Player = func(ctx context.Context, binary string, file *os.File, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
					// Сообщает о первом входе и ждёт разрешения теста либо отмены контекста перед построением исходной
					// команды.

					playerEntered.Do(func() {
						// Однократно уведомляет тест о входе в построитель команды проигрывателя.
						close(entered)
					})
					select {
					case <-releasePlayer:
						return original(ctx, binary, file, attempt)
					case <-ctx.Done():
						return ffmpeg.Command{}, ctx.Err()
					}
				}
			})
			// wrapped захватывает активный снимок выхода до долговечной остановки либо
			// отмены членства.
			wrapped := &startupOutputReadStore{Store: f.store, captured: make(chan core.SessionOutput, 1), release: make(chan struct{})}
			f = replaceAdmissionManager(t, f, wrapped)
			t.Cleanup(func() {
				// Снимает задержки чтения выхода и построения проигрывателя при любом исходе проверки.
				wrapped.unblock()
				unblockPlayer()
			})
			// ctx ограничивает гонку начального барьера восемью секундами; cancel
			// освобождает этот срок.
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if test.remaining > 0 {
				// neighbor содержит копию назначения для создания второго выхода той же
				// группы.
				neighbor := f.destination
				neighbor.Record = core.Record{}
				neighbor.Name = "Соседний выход"
				// err сообщает отказ создания соседнего назначения начального запуска.
				if _, err := f.service.CreateDestination(ctx, neighbor); err != nil {
					t.Fatal(err)
				}
			}
			// start содержит принятую команду запуска с управляемой гонкой чтения выхода;
			// err сообщает отказ её приёма.
			start, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "startup-output-read", StreamID: f.stream.ID})
			if err != nil {
				t.Fatal(err)
			}
			awaitPublication(t, ctx, entered)
			wrapped.armed.Store(true)
			// before сохраняет активный снимок выхода, задержанный перед конкурентной
			// отменой членства.
			var before core.SessionOutput
			select {
			case before = <-wrapped.captured:
			case <-ctx.Done():
				t.Fatal("барьер не прочитал Output", ctx.Err())
			}
			if !before.Attached || !before.DesiredRunning || before.CleanupPending {
				t.Fatal("задержан неактивный снимок", before)
			}
			// starting содержит исходную STARTING-сессию для проверки неизменности её
			// снимка.
			starting, err := f.service.GetSession(ctx, start.SessionID)
			if err != nil || starting.State != core.SessionStarting {
				t.Fatal("барьер не удерживает STARTING", starting, err)
			}
			f.manager.mu.Lock()
			// node указывает локальные ресурсы запускаемой сессии.
			node := f.manager.sessions[start.SessionID]
			f.manager.mu.Unlock()
			node.mu.Lock()
			// member указывает членство выхода, чей старый снимок задержан.
			member := node.members[before.ID]
			// members содержит число учтённых участников начального запуска перед отменой
			// одного из них.
			members := len(node.members)
			node.mu.Unlock()
			if member == nil || members != test.remaining+1 {
				t.Fatal("не все выбранные memberships учтены", members)
			}
			waitStartupCondition(t, ctx, "не выданы ссылки выбранных выходов", func() bool {
				// Проверяет наличие фактических ссылок всех выбранных членств перед остановкой во время ожидания
				// готовности.

				node.mu.Lock()
				defer node.mu.Unlock()
				// candidate указывает очередное выбранное членство, для которого
				// проверяется наличие ссылки группы.
				for _, candidate := range node.members {
					candidate.mu.Lock()
					// ref содержит ссылку проверяемого участника начального барьера.
					ref := candidate.reference
					candidate.mu.Unlock()
					if ref == nil {
						return false
					}
				}
				return true
			})
			// stopped сохраняет квитанцию остановки либо отсоединения выбранного выхода.
			var stopped core.CommandReceipt
			if test.active {
				// Ошибка активного объекта membership без долговечного Stop остаётся общим отказом.
				member.stop()
			} else {
				// command задаёт остановку или отсоединение выхода после чтения его
				// старого активного снимка.
				command := core.OutputCommand{RequestID: "stop-after-output-read", OutputID: before.ID}
				if test.detach {
					stopped, err = f.manager.DetachOutput(ctx, command)
				} else {
					stopped, err = f.manager.StopOutput(ctx, command)
				}
				if err != nil {
					t.Fatal(err)
				}
				// after содержит состояние выхода после долговечной остановки; err
				// сообщает отказ его чтения.
				after, err := f.service.GetOutput(ctx, before.ID)
				if err != nil || after.DesiredRunning || after.Attached == test.detach || after.Generation == before.Generation {
					t.Fatal("Stop/Detach не зафиксирован после старого чтения", after, err)
				}
				if !reflect.DeepEqual(before.Snapshot, after.Snapshot) {
					t.Fatal("Stop/Detach переписал снимок выхода")
				}
			}
			awaitPublication(t, ctx, member.ready)
			member.mu.Lock()
			// memberErr содержит результат отменённого членства; stoppedRef сохраняет его
			// ссылку для проверки общей группы соседей.
			memberErr, stoppedRef := member.err, member.reference
			member.mu.Unlock()
			if !errors.Is(memberErr, context.Canceled) {
				t.Fatal("старый снимок не пересёк отмену membership", memberErr)
			}
			wrapped.readFailure.Store(test.failed)
			wrapped.unblock()
			unblockPlayer()
			// current содержит терминальный результат родительского запуска после повторной
			// проверки выхода.
			current := waitReceipt(t, f, start)
			// want задаёт ожидаемый результат родителя с учётом действующей ошибки либо
			// неподтверждённого исключения.
			want := core.CommandSucceeded
			if test.active || test.failed {
				want = core.CommandFailed
			}
			if current.Status != want {
				t.Fatalf("неверный результат parent после отмены membership: %+v; ожидался %s", current, want)
			}
			if test.failed && (wrapped.rechecks.Load() == 0 || !strings.Contains(current.Failure, core.ErrBusy.Error())) {
				t.Fatal("ошибка повторного чтения не сохранилась", current.Failure)
			}
			// actual содержит родителя после завершения барьера для проверки состояния и
			// неизменности снимка.
			actual, err := f.service.GetSession(ctx, start.SessionID)
			if err != nil || !reflect.DeepEqual(actual.Snapshot, starting.Snapshot) {
				t.Fatal("изменён первоначальный снимок Session", err)
			}
			if want == core.CommandFailed {
				if actual.State != core.SessionFailed {
					t.Fatal("общий отказ не завершил Session", actual)
				}
				return
			}
			if actual.State != core.SessionRunning {
				t.Fatal("исключение Output остановило Session", actual)
			}
			waitStartupCondition(t, ctx, "RUNNING не допускает Feed", func() bool {
				// Проверяет доступность Feed сессии, успешно прошедшей барьер после исключения остановленного выхода.

				// router содержит медиавход успешно запущенного родителя; err сообщает
				// отказ его допуска после исключения выхода.
				router, err := f.manager.Feed(actual)
				return err == nil && router != nil
			})
			// current содержит результат очистки исключённого выхода.
			if current := waitReceipt(t, f, stopped); current.Status != core.CommandSucceeded {
				t.Fatal("исключённый Output не очищен", current)
			}
			// outputs содержит историю выбранных выходов после исключения одного участника
			// барьера.
			outputs, err := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: start.SessionID}})
			if err != nil || len(outputs.Items) != test.remaining+1 {
				t.Fatal("изменён состав истории Outputs", outputs, err)
			}
			// output содержит очередной исторический выход для проверки остановленного
			// участника и работающих соседей.
			for _, output := range outputs.Items {
				if output.ID == before.ID {
					if output.DesiredRunning || output.CleanupPending || output.Attached == test.detach {
						t.Fatal("исключённый Output не сохранил результат Stop/Detach", output)
					}
					continue
				}
				if !output.Attached || !output.DesiredRunning {
					t.Fatal("остановлен здоровый сосед", output)
				}
				node.mu.Lock()
				// neighbor указывает продолжающее работу членство соседнего выхода.
				neighbor := node.members[output.ID]
				node.mu.Unlock()
				if neighbor == nil {
					t.Fatal("нет membership здорового соседа")
				}
				neighbor.mu.Lock()
				// ref содержит ссылку соседнего выхода для проверки готовности и общей
				// локальной группы.
				ref := neighbor.reference
				neighbor.mu.Unlock()
				if ref == nil || ref.Group().Ready() != nil {
					t.Fatal("сосед не достиг фактической fallback-готовности")
				}
				if stoppedRef == nil || ref.Group() != stoppedRef.Group() {
					t.Fatal("выходы не разделяют одну локальную группу")
				}
			}
		})
	}
}

// TestManagerStartupExcludesOutputStoppedBeforeMembershipAcquisition проверяет исключение точного
// поколения до создания членства и успех запуска без изменения первоначального снимка остановленного
// выхода.
func TestManagerStartupExcludesOutputStoppedBeforeMembershipAcquisition(t *testing.T) {
	// f предоставляет включённое назначение для остановки до получения членства.
	f := newManagerFixture(t, true, nil)
	// wrapped задерживает список выбранных выходов до создания их членств.
	wrapped := &startupOutputReadStore{Store: f.store, captured: make(chan core.SessionOutput, 1), release: make(chan struct{}), captureList: true}
	f = replaceAdmissionManager(t, f, wrapped)
	t.Cleanup(wrapped.unblock)
	// ctx ограничивает остановку до получения членства восемью секундами; cancel освобождает этот
	// срок.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	wrapped.armed.Store(true)
	// start содержит запуск, чьё чтение выбранных выходов задерживается; err сообщает отказ приёма
	// команды.
	start, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "stop-before-membership", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// selected сохраняет выбранный снимок выхода до создания его членства.
	var selected core.SessionOutput
	select {
	case selected = <-wrapped.captured:
	case <-ctx.Done():
		t.Fatal("initialize не прочитал выбранный Output", ctx.Err())
	}
	f.manager.mu.Lock()
	// node указывает локальные ресурсы запуска до остановки выбранного выхода.
	node := f.manager.sessions[start.SessionID]
	f.manager.mu.Unlock()
	node.mu.Lock()
	// members содержит число уже созданных членств, которое перед остановкой должно оставаться
	// нулевым.
	members := len(node.members)
	node.mu.Unlock()
	if members != 0 {
		t.Fatal("membership уже создан до остановки", members)
	}
	// stop содержит команду остановки выбранного выхода до получения членства.
	stop, err := f.manager.StopOutput(ctx, core.OutputCommand{RequestID: "stop-before-acquisition", OutputID: selected.ID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит успешный результат остановки до создания членства.
	if current := waitReceipt(t, f, stop); current.Status != core.CommandSucceeded {
		t.Fatal("остановка до Acquire не завершена", current)
	}
	node.mu.Lock()
	// excluded содержит точное поколение выбранного выхода, исключённое завершённой очисткой.
	excluded := node.excluded[selected.ID]
	node.mu.Unlock()
	if excluded != selected.Generation {
		t.Fatal("очистка не исключила точное первоначальное поколение", excluded, selected.Generation)
	}
	wrapped.unblock()
	// current содержит успешный результат родительского запуска после исключения остановленного
	// поколения.
	if current := waitReceipt(t, f, start); current.Status != core.CommandSucceeded {
		t.Fatal("ошибка Acquire исключённого поколения остановила parent", current)
	}
	// output содержит сохранённый остановленный выход для проверки состояния и неизменности
	// первоначального снимка.
	output, err := f.service.GetOutput(ctx, selected.ID)
	if err != nil || output.DesiredRunning || output.CleanupPending || !reflect.DeepEqual(output.Snapshot, selected.Snapshot) {
		t.Fatal("остановка до Acquire утратила состояние выхода", output, err)
	}
}
