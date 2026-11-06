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

func TestAttemptResultsPrecedeRetryAndFollowNamespaceCleanup(t *testing.T) {
	for _, mode := range []string{"builder_failure", "native_start_failure", "native_exit"} {
		t.Run(mode, func(t *testing.T) {
			config := supervisorConfig(t)
			config.MaxRestarts = 2
			config.StartupTimeout = 3 * time.Second
			s := newTestSupervisor(t, config)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			owner := newOwnedPermit()
			failure := errors.New("построение команды отклонено")
			missing := filepath.Join(t.TempDir(), "absent-ffmpeg")
			if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			type witness struct {
				result          AttemptResult
				cleanup, permit error
				snapshot        uint64
			}
			results := make(chan witness, 4)
			release := make(chan struct{}, 4)
			var builds atomic.Uint64
			var path string
			var sequenceError atomic.Bool
			w := childWorker("exit")
			w.Owner = owner
			original := w.Command
			w.Command = func(commandCtx context.Context, a Attempt) (Command, error) {
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
				_, cleanup := os.Stat(path)
				// Вызов Owner.Do из обработчика проверяет отсутствие удерживаемой блокировки.
				permit := owner.Do(ctx, func() error { return nil })
				results <- witness{result: result, cleanup: cleanup, permit: permit, snapshot: s.Snapshot().Attempt}
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			if err := s.Start(ctx, w); err != nil {
				t.Fatal(err)
			}
			for id := uint64(1); id <= 3; id++ {
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
			r := waitResult(t, s)
			if r.Attempts != 3 || !r.Clean || r.CleanupErr != nil || r.Err == nil || sequenceError.Load() {
				t.Fatalf("нарушено последовательное владение попытками: %+v", r)
			}
			select {
			case duplicate := <-results:
				t.Fatal("повторный callback", duplicate)
			default:
			}
		})
	}
}

func TestAttemptResultCancellationClosesOwnedFileAndJoinsBuilder(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "owner_revoked"}[revoke], func(t *testing.T) {
			s := newTestSupervisor(t, supervisorConfig(t))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			file, err := os.CreateTemp(t.TempDir(), "media")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = file.Close() })
			entered := make(chan struct{})
			results := make(chan AttemptResult, 2)
			owner := newOwnedPermit()
			w := Worker{SessionID: "session", GroupID: "group", Owner: owner, Topology: FileInput, Output: inertHandler,
				Command: func(commandCtx context.Context, _ Attempt) (Command, error) {
					close(entered)
					<-commandCtx.Done()
					return Command{ExtraFiles: []*os.File{file}}, commandCtx.Err()
				},
				OnAttemptResult: func(result AttemptResult) { results <- result },
			}
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
			r := waitResult(t, s)
			if r.Attempts != 1 || !r.Clean || r.CleanupErr != nil || !errors.Is(r.Err, context.Canceled) {
				t.Fatalf("отмена потеряла join или причину: %+v", r)
			}
			if revoke && !errors.Is(r.Err, ErrOwnerRevoked) {
				t.Fatal("Wait потерял отзыв владельца", r.Err)
			}
			select {
			case got := <-results:
				if got.ID != 1 || !got.Stopped || got.CleanupErr != nil || !errors.Is(got.Err, context.Canceled) {
					t.Fatalf("отмена записана как отказ media: %+v", got)
				}
			default:
				t.Fatal("Done закрыт без результата попытки")
			}
			if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatal("callback/Wait опередили закрытие ExtraFiles", err)
			}
			select {
			case duplicate := <-results:
				t.Fatal("повторный результат отмены", duplicate)
			default:
			}
		})
	}
}

func TestAttemptResultKeepsCleanupFailureSeparateAndPreventsRetry(t *testing.T) {
	config := supervisorConfig(t)
	config.MaxRestarts = 2
	s, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		err := s.Stop(context.Background())
		if err != nil && !errors.Is(err, ErrResourceOwnership) {
			t.Error(err)
		}
	})
	failure := errors.New("команда отклонена после замены сокета")
	results := make(chan AttemptResult, 2)
	var builds atomic.Uint64
	var foreignPath string
	w := childWorker("exit")
	w.Command = func(_ context.Context, a Attempt) (Command, error) {
		builds.Add(1)
		foreignPath = a.OutputPath
		if err := os.Remove(a.OutputPath); err != nil {
			return Command{}, err
		}
		if err := os.WriteFile(a.OutputPath, []byte("чужой ресурс"), 0600); err != nil {
			return Command{}, err
		}
		return Command{}, failure
	}
	w.OnAttemptResult = func(result AttemptResult) { results <- result }
	if err := s.Start(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	r := waitResult(t, s)
	if builds.Load() != 1 || r.Attempts != 1 || r.Clean || !errors.Is(r.Err, failure) || !errors.Is(r.CleanupErr, ErrResourceOwnership) {
		t.Fatalf("неполная очистка разрешила retry либо утратила причины: %+v", r)
	}
	select {
	case got := <-results:
		if got.ID != 1 || got.Stopped || !errors.Is(got.Err, failure) || !errors.Is(got.CleanupErr, ErrResourceOwnership) {
			t.Fatalf("смешаны исход попытки и ошибка очистки: %+v", got)
		}
	default:
		t.Fatal("нет результата попытки с ошибкой очистки")
	}
	if got, err := os.ReadFile(foreignPath); err != nil || string(got) != "чужой ресурс" {
		t.Fatal("cleanup изменил чужой файл", err)
	}
	// Повторный Stop обязан сохранять подтверждённую ошибку очистки.
	if err := s.Stop(context.Background()); !errors.Is(err, ErrResourceOwnership) {
		t.Fatal("Stop потерял ошибку очистки", err)
	}
	select {
	case duplicate := <-results:
		t.Fatal("повторный результат неудачной очистки", duplicate)
	default:
	}
}

func TestAttemptResultSuccessfulNativeCompletionIsNotStopped(t *testing.T) {
	config := supervisorConfig(t)
	// Дочерний тест с -race перед выходом ждёт 1 с по умолчанию (GORACE atexit_sleep_ms).
	config.VideoTimeout, config.AudioTimeout = 3*time.Second, 3*time.Second
	s := newTestSupervisor(t, config)
	results := make(chan AttemptResult, 2)
	w := childWorker("duplex")
	w.Input = func(_ context.Context, conn net.Conn, _ Progress) error {
		_, err := conn.Write([]byte("input"))
		return err
	}
	w.Output = func(_ context.Context, conn net.Conn, progress Progress) error {
		var data [2]byte
		if _, err := io.ReadFull(conn, data[:]); err != nil {
			return err
		}
		if string(data[:]) != "va" || !progress(Video, 1) || !progress(Audio, 1) {
			return errors.New("неверные проверенные дорожки")
		}
		_, err := io.Copy(io.Discard, conn)
		return err
	}
	w.OnAttemptResult = func(result AttemptResult) { results <- result }
	if err := s.Start(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	r := waitResult(t, s)
	if r.Attempts != 1 || r.Err != nil || r.CleanupErr != nil || !r.Clean {
		t.Fatalf("успешное завершение потеряло join: %+v", r)
	}
	select {
	case got := <-results:
		if got.ID != 1 || got.Err != nil || got.CleanupErr != nil || got.Stopped {
			t.Fatalf("внутренняя отмена cleanup подменила успех: %+v", got)
		}
	default:
		t.Fatal("нет результата успешной физической попытки")
	}
	select {
	case duplicate := <-results:
		t.Fatal("повторный результат успешной попытки", duplicate)
	default:
	}
}
