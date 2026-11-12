package ffmpeg

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Проверяет единственный результат после очистки каждой попытки и до следующего запуска, включая отказ
// построителя, запуска процесса и ранний выход; обработчик выполняется вне допуска владельца.
func TestAttemptResultsPrecedeRetryAndFollowNamespaceCleanup(t *testing.T) {
	// mode выбирает отказ построителя, отказ native Start или ранний выход настоящего процесса.
	for _, mode := range []string{"builder_failure", "native_start_failure", "native_exit"} {
		t.Run(mode, func(t *testing.T) {
			// Проверяет три последовательные попытки выбранного отказа: результат приходит после очистки Namespace
			// и до следующего построения команды.

			// config разрешает две повторные попытки и увеличивает срок начальной
			// готовности.
			config := supervisorConfig(t)
			config.MaxRestarts = 2
			config.StartupTimeout = 3 * time.Second
			// s выполняет три последовательные физические попытки выбранного отказа.
			s := newTestSupervisor(t, config)
			// ctx ограничивает проверку десятью секундами; cancel освобождает таймер
			// сценария.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// owner предоставляет проверяемый допуск и позволяет обнаружить удержание его
			// блокировки в обработчике результата.
			owner := newOwnedPermit()
			// failure задаёт исходную ошибку отказа построителя команды.
			failure := errors.New("построение команды отклонено")
			// missing задаёт гарантированно отсутствующий исполняемый путь для отказа native
			// Start.
			missing := filepath.Join(t.TempDir(), "absent-ffmpeg")
			// err содержит результат проверки отсутствия выбранного исполняемого файла.
			if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			// witness объединяет результат попытки с наблюдениями очистки, допуска и номера
			// текущего снимка.
			type witness struct {
				// result сохраняет исход физической попытки, полученный обработчиком.
				result AttemptResult
				// cleanup сохраняет результат проверки удаления каталога попытки; permit
				// сохраняет результат повторного входа в Owner.Do из обработчика.
				cleanup, permit error
				// snapshot сохраняет номер попытки, наблюдаемый Supervisor во время
				// обработки результата.
				snapshot uint64
			}
			// results передаёт свидетельства завершившихся попыток основному тесту.
			results := make(chan witness, 4)
			// release разрешает обработчику результата вернуться и допустить следующую
			// попытку.
			release := make(chan struct{}, 4)
			// builds считает вызовы построителя для проверки последовательности попыток и
			// обработчиков результатов.
			var builds atomic.Uint64
			// path сохраняет каталог текущей попытки для проверки удаления до обработчика
			// результата.
			var path string
			// sequenceError отмечает несовпадение номера попытки и числа построений.
			var sequenceError atomic.Bool
			// w содержит работника с выбранным отказом и задерживаемым обработчиком
			// результата.
			w := childWorker("exit")
			w.Owner = owner
			// original сохраняет исходный построитель настоящего дочернего процесса для
			// сценария native_exit.
			original := w.Command
			w.Command = func(commandCtx context.Context, a Attempt) (Command, error) {
				// Считает построения, запоминает каталог попытки и вводит выбранный отказ построителя, запуска либо
				// преждевременный выход дочернего процесса.

				if builds.Add(1) != a.ID {
					sequenceError.Store(true)
				}
				path = filepath.Dir(a.OutputPath)
				switch mode {
				case "builder_failure":
					return Command{}, failure
				case "native_start_failure":
					return Command{Binary: missing}, nil
				default:
					return original(commandCtx, a)
				}
			}
			w.OnAttemptResult = func(result AttemptResult) {
				// Фиксирует исход и состояние удалённого каталога, проверяет доступность Owner.Do и задерживает
				// возврат до разрешения следующей попытки.

				// cleanup содержит результат проверки исчезновения каталога до отправки
				// результата попытки.
				_, cleanup := os.Stat(path)
				// Вызов Owner.Do из обработчика проверяет отсутствие удерживаемой блокировки.
				// permit содержит результат повторного входа в допуск владельца из
				// обработчика результата.
				permit := owner.Do(ctx, func() error {
					// Возвращает успех из повторного Owner.Do, позволяя обнаружить удержание блокировки во время
					// обработчика результата.
					return nil
				})
				results <- witness{result: result, cleanup: cleanup, permit: permit, snapshot: s.Snapshot().Attempt}
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			// err сообщает об отказе регистрации работника перед проверкой последовательных
			// попыток.
			if err := s.Start(ctx, w); err != nil {
				t.Fatal(err)
			}
			// id задаёт ожидаемый последовательный номер результата от 1 до 3.
			for id := uint64(1); id <= 3; id++ {
				// got содержит свидетельство очередной попытки для проверки исхода,
				// очистки и порядка повторов.
				var got witness
				select {
				case got = <-results:
				case <-ctx.Done():
					t.Fatal("нет результата физической попытки", id, ctx.Err())
				}
				if got.result.ID != id || got.snapshot != id || got.result.Err == nil || got.result.CleanupErr != nil || got.result.Stopped || got.permit != nil || !errors.Is(got.cleanup, os.ErrNotExist) {
					t.Fatalf("результат до очистки либо с неверной identity: %+v", got)
				}
				if mode == "builder_failure" && !errors.Is(got.result.Err, failure) {
					t.Fatal("утрачена исходная ошибка построителя", got.result.Err)
				}
				if mode == "native_start_failure" && !errors.Is(got.result.Err, os.ErrNotExist) {
					t.Fatal("утрачена исходная ошибка native Start", got.result.Err)
				}
				if mode == "native_exit" && !errors.Is(got.result.Err, ErrPrematureExit) {
					t.Fatal("преждевременный native exit объявлен успешным", got.result.Err)
				}
				if builds.Load() != id {
					t.Fatal("retry опередил результат предыдущей попытки", builds.Load(), id)
				}
				select {
				case <-s.Done():
					t.Fatal("Done закрыт до возврата callback")
				default:
				}
				release <- struct{}{}
			}
			// r содержит итог логического работника после трёх обработанных отказов.
			r := waitResult(t, s)
			if r.Attempts != 3 || !r.Clean || r.CleanupErr != nil || r.Err == nil || sequenceError.Load() {
				t.Fatalf("нарушено последовательное владение попытками: %+v", r)
			}
			select {
			// duplicate содержит недопустимое дополнительное свидетельство результата после
			// завершения работника.
			case duplicate := <-results:
				t.Fatal("повторный callback", duplicate)
			default:
			}
		})
	}
}

// Проверяет итог остановленной попытки после возврата отменённого построителя и закрытия ExtraFiles,
// включая отзыв владельца без повторного результата.
func TestAttemptResultCancellationClosesOwnedFileAndJoinsBuilder(t *testing.T) {
	// revoke выбирает отзыв владельца вместо непосредственной отмены контекста.
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "owner_revoked"}[revoke], func(t *testing.T) {
			// Проверяет завершение построителя и закрытие его ExtraFiles при отмене контекста либо отзыве
			// владельца без повторной попытки.

			// s выполняет файловую попытку с ожидающим отмены построителем.
			s := newTestSupervisor(t, supervisorConfig(t))
			// ctx задаёт отменяемый срок работы; cancel инициирует проверяемую отмену или
			// освобождает контекст после отзыва.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// file содержит дескриптор, возвращаемый построителем вместе с отменой; err
			// сообщает об отказе его создания.
			file, err := os.CreateTemp(t.TempDir(), "media")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает тестовый файл после сценария, игнорируя ошибку повторного закрытия.
				_ = file.Close()
			})
			// entered сообщает о входе в построитель до отмены или отзыва владельца.
			entered := make(chan struct{})
			// results передаёт результаты отменённой попытки для проверки единственности.
			results := make(chan AttemptResult, 2)
			// owner предоставляет отзываемый допуск файлового работника.
			owner := newOwnedPermit()
			// w связывает ожидающий отмены построитель, ExtraFiles и обработчик результата
			// попытки.
			w := Worker{SessionID: "session", GroupID: "group", Owner: owner, Topology: FileInput, Output: inertHandler,
				Command: func(commandCtx context.Context, _ Attempt) (Command, error) {
					// Сообщает о входе, ждёт отмены и возвращает принадлежащий попытке файл вместе с ошибкой контекста.

					close(entered)
					<-commandCtx.Done()
					return Command{ExtraFiles: []*os.File{file}}, commandCtx.Err()
				},
				OnAttemptResult: func(result AttemptResult) {
					// Передаёт результат отменённой попытки в канал для проверки до закрытия Done.
					results <- result
				},
			}
			// err сообщает об отказе запуска работника с задерживаемым построителем.
			if err := s.Start(ctx, w); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("построитель не начался")
			}
			if revoke {
				owner.revoke()
			} else {
				cancel()
			}
			// r содержит окончательный результат после возврата отменённого построителя и
			// закрытия файла.
			r := waitResult(t, s)
			if r.Attempts != 1 || !r.Clean || r.CleanupErr != nil || !errors.Is(r.Err, context.Canceled) {
				t.Fatalf("отмена потеряла join или причину: %+v", r)
			}
			if revoke && !errors.Is(r.Err, ErrOwnerRevoked) {
				t.Fatal("Wait потерял отзыв владельца", r.Err)
			}
			select {
			// got содержит единственный результат отменённой попытки, уже доступный после
			// Done.
			case got := <-results:
				if got.ID != 1 || !got.Stopped || got.CleanupErr != nil || !errors.Is(got.Err, context.Canceled) {
					t.Fatalf("отмена записана как отказ media: %+v", got)
				}
			default:
				t.Fatal("Done закрыт без результата попытки")
			}
			// err содержит ожидаемый os.ErrClosed при проверке файла после обработчика и
			// Wait.
			if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatal("callback/Wait опередили закрытие ExtraFiles", err)
			}
			select {
			// duplicate содержит недопустимый повтор результата отменённой попытки.
			case duplicate := <-results:
				t.Fatal("повторный результат отмены", duplicate)
			default:
			}
		})
	}
}

// Проверяет отдельную ошибку очистки подменённого сокета, запрет повторной попытки и сохранение обеих
// причин и чужого файла при повторном Stop.
func TestAttemptResultKeepsCleanupFailureSeparateAndPreventsRetry(t *testing.T) {
	// config разрешает повторы, которые должны быть запрещены после отказа очистки.
	config := supervisorConfig(t)
	config.MaxRestarts = 2
	// s содержит супервизор проверки подмены сокета; err сообщает об отказе его создания.
	s, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Останавливает Supervisor после сценария и сообщает ошибки, кроме ожидаемой ErrResourceOwnership.

		// err содержит результат резервного Stop с допустимой ожидаемой ошибкой подмены ресурса.
		err := s.Stop(context.Background())
		if err != nil && !errors.Is(err, ErrResourceOwnership) {
			t.Error(err)
		}
	})
	// failure задаёт исходный отказ команды после подмены выходного сокета.
	failure := errors.New("команда отклонена после замены сокета")
	// results передаёт отдельные результаты выполнения и очистки попытки.
	results := make(chan AttemptResult, 2)
	// builds считает построения для проверки отсутствия повторов после неполной очистки.
	var builds atomic.Uint64
	// foreignPath сохраняет путь подменённого выходного сокета для проверки сохранности чужого
	// файла.
	var foreignPath string
	// w содержит работника, чей построитель подменяет сокет перед возвратом ошибки.
	w := childWorker("exit")
	w.Command = func(_ context.Context, a Attempt) (Command, error) {
		// Подменяет выходной сокет чужим файлом и возвращает исходную ошибку команды, создавая отдельную
		// ошибку очистки.

		builds.Add(1)
		foreignPath = a.OutputPath
		// err сообщает об отказе удаления имени выходного сокета перед подменой.
		if err := os.Remove(a.OutputPath); err != nil {
			return Command{}, err
		}
		// err сообщает об отказе создания чужого файла вместо выходного сокета.
		if err := os.WriteFile(a.OutputPath, []byte("чужой ресурс"), 0600); err != nil {
			return Command{}, err
		}
		return Command{}, failure
	}
	w.OnAttemptResult = func(result AttemptResult) {
		// Передаёт результат попытки с отдельной ошибкой очистки в канал проверки.
		results <- result
	}
	// err сообщает об отказе запуска сценария подмены ресурса.
	if err := s.Start(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	// r содержит итог с раздельными исходной ошибкой и ошибкой физической очистки.
	r := waitResult(t, s)
	if builds.Load() != 1 || r.Attempts != 1 || r.Clean || !errors.Is(r.Err, failure) || !errors.Is(r.CleanupErr, ErrResourceOwnership) {
		t.Fatalf("неполная очистка разрешила retry либо утратила причины: %+v", r)
	}
	select {
	// got содержит результат единственной попытки с ожидаемым отказом очистки.
	case got := <-results:
		if got.ID != 1 || got.Stopped || !errors.Is(got.Err, failure) || !errors.Is(got.CleanupErr, ErrResourceOwnership) {
			t.Fatalf("смешаны исход попытки и ошибка очистки: %+v", got)
		}
	default:
		t.Fatal("нет результата попытки с ошибкой очистки")
	}
	// got содержит сохранённые байты чужого файла; err сообщает об отказе их чтения после очистки.
	if got, err := os.ReadFile(foreignPath); err != nil || string(got) != "чужой ресурс" {
		t.Fatal("cleanup изменил чужой файл", err)
	}
	// Повторный Stop обязан сохранять подтверждённую ошибку очистки.
	// err содержит результат повторного Stop для проверки сохранения ErrResourceOwnership.
	if err := s.Stop(context.Background()); !errors.Is(err, ErrResourceOwnership) {
		t.Fatal("Stop потерял ошибку очистки", err)
	}
	select {
	// duplicate содержит недопустимый повтор результата попытки с ошибкой очистки.
	case duplicate := <-results:
		t.Fatal("повторный результат неудачной очистки", duplicate)
	default:
	}
}

// Проверяет единственный успешный результат после полного завершения процесса и очистки, где
// внутренняя отмена ресурсов не выставляет Stopped.
func TestAttemptResultSuccessfulNativeCompletionIsNotStopped(t *testing.T) {
	// config увеличивает сроки продвижения для успешного дочернего процесса под детектором гонок.
	config := supervisorConfig(t)
	// Дочерний тест с -race перед выходом ждёт 1 с по умолчанию (GORACE atexit_sleep_ms).
	config.VideoTimeout, config.AudioTimeout = 3*time.Second, 3*time.Second
	// s выполняет успешную дуплексную попытку с проверяемым обработчиком результата.
	s := newTestSupervisor(t, config)
	// results передаёт результаты успешной попытки для проверки Stopped и единственности.
	results := make(chan AttemptResult, 2)
	// w содержит дуплексного работника с контрольными входными и выходными байтами.
	w := childWorker("duplex")
	w.Input = func(_ context.Context, conn net.Conn, _ Progress) error {
		// Передаёт условные входные байты дочернему процессу и возвращает ошибку записи.

		// err содержит результат записи контрольного входа дочернему процессу.
		_, err := conn.Write([]byte("input"))
		return err
	}
	w.Output = func(_ context.Context, conn net.Conn, progress Progress) error {
		// Проверяет выходные маркеры va и продвижение обеих дорожек, затем дочитывает соединение до
		// завершения.

		// data содержит двухбайтовые выходные маркеры проверяемых дорожек.
		var data [2]byte
		// err сообщает об отказе чтения контрольных выходных маркеров.
		if _, err := io.ReadFull(conn, data[:]); err != nil {
			return err
		}
		if string(data[:]) != "va" || !progress(Video, 1) || !progress(Audio, 1) {
			return errors.New("неверные проверенные дорожки")
		}
		// err содержит результат дочитывания выходного соединения после подтверждения дорожек.
		_, err := io.Copy(io.Discard, conn)
		return err
	}
	w.OnAttemptResult = func(result AttemptResult) {
		// Передаёт результат успешной попытки для проверки отсутствия признака Stopped.
		results <- result
	}
	// err сообщает об отказе запуска успешного дуплексного сценария.
	if err := s.Start(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	// r содержит итог успешного выполнения и очистки одной попытки.
	r := waitResult(t, s)
	if r.Attempts != 1 || r.Err != nil || r.CleanupErr != nil || !r.Clean {
		t.Fatalf("успешное завершение потеряло join: %+v", r)
	}
	select {
	// got содержит единственный успешный результат для проверки отсутствия Stopped.
	case got := <-results:
		if got.ID != 1 || got.Err != nil || got.CleanupErr != nil || got.Stopped {
			t.Fatalf("внутренняя отмена cleanup подменила успех: %+v", got)
		}
	default:
		t.Fatal("нет результата успешной физической попытки")
	}
	select {
	// duplicate содержит недопустимый повтор результата успешной попытки.
	case duplicate := <-results:
		t.Fatal("повторный результат успешной попытки", duplicate)
	default:
	}
}
