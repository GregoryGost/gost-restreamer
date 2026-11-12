//go:build unix

package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

// TestOutputReadinessMediaProcess записывает PID дочернего проигрывателя в маркер и запускает передачу
// проверочного fMP4 для управления процессом сигналами.
func TestOutputReadinessMediaProcess(t *testing.T) {
	if len(os.Args) < 4 || os.Args[len(os.Args)-3] != "--" {
		return
	}
	// err сообщает отказ записи PID управляемого проигрывателя в маркерный файл.
	if err := os.WriteFile(os.Args[len(os.Args)-1], []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	os.Args = os.Args[:len(os.Args)-1]
	TestSessionMediaProcess(t)
}

// TestManagerOutputRechecksCurrentFallbackWithinStartupBudget проверяет повторную текущую готовность
// заглушки после начальной готовности членства: восстановление, исходный срок, отмену и терминальную
// группу с сохранением Feed и соседнего отправителя.
func TestManagerOutputRechecksCurrentFallbackWithinStartupBudget(t *testing.T) {
	// scenario выбирает восстановление, истечение срока, отмену либо терминальную остановку группы.
	for _, scenario := range []string{"recovers", "deadline", "canceled", "terminal"} {
		t.Run(scenario, func(t *testing.T) {
			// Проверяет выбранный исход утраты текущей готовности заглушки при добавлении отдельного выхода.

			// marker указывает файл PID проигрывателя, используемый для управления
			// сигналами.
			marker := filepath.Join(t.TempDir(), "player.pid")
			// f предоставляет работающую заглушку с увеличенными сроками контроля процесса и
			// доступным PID.
			f := newManagerFixture(t, true, func(c *Config) {
				// Увеличивает сроки контроля FFmpeg и задаёт проигрыватель с маркером PID, чтобы SIGSTOP отзывал
				// пригодность пакетов без немедленного завершения работника.

				// Истекает пригодность пакетов при продолжающейся работе настоящего работника.
				c.Group.FFmpeg.VideoTimeout = 15 * time.Second
				c.Group.FFmpeg.AudioTimeout = 15 * time.Second
				c.Group.Player = func(_ context.Context, _ string, _ *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
					// Строит команду дочернего проигрывателя, записывающего PID и отправляющего fMP4 в UDS конкретной
					// попытки.

					// binary указывает текущий тестовый исполняемый файл; err
					// сообщает отказ определения пути проигрывателя.
					binary, err := os.Executable()
					return ffmpeg.Command{Binary: binary, Args: []string{"-test.run=^TestOutputReadinessMediaProcess$", "--", a.OutputPath, marker}}, err
				}
			})
			// ctx ограничивает сценарий повторной готовности пятнадцатью секундами; cancel
			// освобождает этот срок.
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			// parent содержит квитанцию запуска; session содержит сохранённую сессию; feed
			// содержит её медиавход; node содержит локального владельца её ресурсов.
			parent, session, feed, node := startupParent(t, ctx, f)
			// neighbor сохраняет узел первоначального назначения для проверки изоляции
			// нового выхода.
			var neighbor *outputNode
			waitStartupCondition(t, ctx, "соседний Publisher не активирован", func() bool {
				// Проверяет наличие отправителя и наблюдателя первоначального выхода и сохраняет его узел для проверки
				// изоляции.

				node.mu.Lock()
				defer node.mu.Unlock()
				// candidate указывает очередной выход родителя при поиске уже
				// активированного отправителя.
				for _, candidate := range node.outputs {
					candidate.mu.Lock()
					// active отмечает наличие отправителя и наблюдателя у
					// проверяемого соседнего выхода.
					active := candidate.publisher != nil && candidate.observer != nil
					candidate.mu.Unlock()
					if active {
						neighbor = candidate
						return true
					}
				}
				return false
			})
			neighbor.mu.Lock()
			// neighborPublisher сохраняет исходного отправителя соседа для проверки
			// отсутствия его замены.
			neighborPublisher := neighbor.publisher
			neighbor.mu.Unlock()
			// destination содержит новое назначение той же группы; err сообщает отказ его
			// создания.
			destination, err := f.service.CreateDestination(ctx, core.Destination{Name: "Дополнительный выход", StreamID: f.stream.ID, GroupID: f.binding.GroupID, Platform: "custom", Endpoint: "rtmp://127.0.0.1:1/live", StreamKey: "additional-key"})
			if err != nil {
				t.Fatal(err)
			}
			// Диспетчер ждёт, пока настоящий объект membership пройдёт начальный барьер.
			f.manager.mu.Lock()
			// added содержит принятую в сервисе команду нового выхода, временно скрытую от
			// диспетчера.
			added, err := f.service.AddOutput(ctx, core.AddOutputRequest{RequestID: "current-fallback-add", SessionID: parent.SessionID, DestinationID: destination.ID, Binding: f.binding})
			if err == nil {
				f.manager.commands[added.ID] = true
			}
			f.manager.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			// output содержит снимок добавленного выхода для предварительного получения
			// членства.
			output, err := f.service.GetOutput(ctx, added.OutputID)
			if err != nil {
				t.Fatal(err)
			}
			// member содержит участника группы, чья начальная готовность ожидается до
			// остановки пакетов.
			member, err := node.acquire(ctx, output)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-member.ready:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			member.mu.Lock()
			// ref содержит полученную ссылку группы; readyErr сообщает результат
			// первоначальной подготовки членства.
			ref, readyErr := member.reference, member.err
			member.mu.Unlock()
			if readyErr != nil || ref == nil || ref.Group().Ready() != nil {
				t.Fatal("membership не получил фактическую готовность", readyErr)
			}
			// data содержит байты PID из маркера проигрывателя.
			data, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			// pid задаёт процесс проигрывателя для SIGSTOP и SIGCONT.
			pid, err := strconv.Atoi(string(data))
			if err != nil {
				t.Fatal(err)
			}
			if err = syscall.Kill(pid, syscall.SIGSTOP); err != nil {
				t.Fatal(err)
			}
			// paused отмечает необходимость возобновить остановленный сигналом процесс при
			// очистке сценария.
			paused := true
			defer func() {
				// Возобновляет остановленный сигналом проигрыватель при возврате из проверки, если он ещё
				// приостановлен.

				if paused {
					_ = syscall.Kill(pid, syscall.SIGCONT)
				}
			}()
			waitStartupCondition(t, ctx, "готовность не отозвана после остановки пакетов", func() bool {
				// Проверяет отзыв готовности группы после прекращения поступления пакетов от приостановленного
				// процесса.

				return ref.Group().Ready() != nil
			})
			select {
			case <-ref.Group().Done():
				t.Fatal("вместо временной неготовности завершилась группа")
			default:
			}
			if scenario == "terminal" {
				if err = syscall.Kill(pid, syscall.SIGCONT); err != nil {
					t.Fatal(err)
				}
				paused = false
				if err = ref.Group().Stop(ctx); err != nil {
					t.Fatal(err)
				}
			}
			f.manager.mu.Lock()
			delete(f.manager.commands, added.ID)
			f.manager.mu.Unlock()
			f.manager.dispatch(added)
			if scenario == "recovers" || scenario == "canceled" {
				waitStartupCondition(t, ctx, "выход не начал проверку готовности", func() bool {
					// Проверяет достижение ISSUED новой командой и отклоняет преждевременный терминальный результат до
					// восстановления либо отмены.

					// current содержит состояние команды во время повторной проверки
					// готовности; err сообщает отказ чтения квитанции.
					current, err := f.service.GetReceipt(ctx, added.ID)
					if err != nil {
						t.Fatal(err)
					}
					if current.Status.Terminal() {
						t.Fatalf("временная неготовность завершила команду до восстановления: %+v", current)
					}
					return current.Status == core.CommandIssued
				})
				if scenario == "recovers" {
					// Источник восстанавливается в пределах того же срока запуска.
					// timer задаёт задержку восстановления пакетов внутри исходного
					// срока запуска.
					timer := time.NewTimer(200 * time.Millisecond)
					defer timer.Stop()
					select {
					case <-timer.C:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					// current содержит квитанцию перед восстановлением пакетов; err
					// сообщает отказ её контрольного чтения.
					if current, err := f.service.GetReceipt(ctx, added.ID); err != nil || current.Status.Terminal() {
						t.Fatal("команда не дождалась восстановления", current, err)
					}
					if err = syscall.Kill(pid, syscall.SIGCONT); err != nil {
						t.Fatal(err)
					}
					paused = false
				} else {
					node.stopOutput(added.OutputID)
				}
			}
			// current содержит терминальный результат нового выхода после выбранного исхода
			// готовности.
			current := startupReceipt(t, ctx, f, added)
			if scenario == "recovers" {
				if current.Status != core.CommandSucceeded {
					t.Fatal("восстановившийся fallback не запустил выход", current)
				}
			} else if current.Status != core.CommandFailed {
				t.Fatal("непригодный fallback дал успешную квитанцию", current)
			}
			if scenario == "deadline" && current.Failure != context.DeadlineExceeded.Error() {
				t.Fatal("не сохранено истечение исходного срока", current)
			}
			if scenario == "canceled" && current.Failure != context.Canceled.Error() {
				t.Fatal("не сохранена отмена ожидания", current)
			}
			if scenario == "terminal" && !errors.Is(ref.Group().Ready(), core.ErrStale) {
				t.Fatal("терминальная группа сохранила готовность")
			}
			// currentFeed содержит актуальный медиавход родительской сессии; err сообщает
			// отказ проверки его сохранности.
			if currentFeed, err := f.manager.Feed(session); err != nil || currentFeed != feed {
				t.Fatal("проверка выхода отозвала Feed родителя", err)
			}
			neighbor.mu.Lock()
			// preserved отмечает сохранение исходного соседнего отправителя и, вне
			// терминального сценария, отсутствие его отказа.
			preserved := neighbor.publisher == neighborPublisher
			if scenario != "terminal" {
				preserved = preserved && neighbor.failure == nil
			}
			neighbor.mu.Unlock()
			if !preserved {
				t.Fatal("проверка нового выхода заменила соседний Publisher")
			}
		})
	}
}
