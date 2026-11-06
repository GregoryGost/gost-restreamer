package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/config"
)

type acceptanceService struct {
	wait     func(context.Context) error
	shutdown func(context.Context) error
	mu       sync.Mutex
	logged   []error
}

func (a *acceptanceService) Wait(ctx context.Context) error     { return a.wait(ctx) }
func (a *acceptanceService) Shutdown(ctx context.Context) error { return a.shutdown(ctx) }
func (a *acceptanceService) LogError(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logged = append(a.logged, err)
}

func TestAcceptanceCLIRejectsArgumentsBeforeLoading(t *testing.T) {
	for _, args := range [][]string{{"--unknown"}, {"unexpected"}, {"--config"}, {"--config="}} {
		var stderr bytes.Buffer
		loaded := false
		status := runWith(context.Background(), args, &stderr, commandDependencies{load: func(string) (config.Config, config.Selection, error) {
			loaded = true
			return config.Config{}, config.Selection{}, errors.New("не должен вызываться")
		}})
		if status != 2 || loaded || stderr.Len() == 0 {
			t.Fatalf("некорректные аргументы %q: status=%d, loaded=%v, stderr=%q", args, status, loaded, stderr.String())
		}
	}
}

func TestAcceptanceCLIInvalidFilePrecedesApplication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("http:\n  misspelled_address: ':8080'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	started := false
	var stderr bytes.Buffer
	status := runWith(context.Background(), []string{"--config", path}, &stderr, commandDependencies{start: func(context.Context, config.Config, config.Selection) (service, error) {
		started = true
		return nil, nil
	}})
	if status == 0 || started || !strings.Contains(stderr.String(), path) || !strings.Contains(stderr.String(), "misspelled_address") {
		t.Fatalf("ошибка YAML не остановила bootstrap: status=%d, started=%v, stderr=%q", status, started, stderr.String())
	}
}

func TestAcceptanceCLIPropagatesExactPathAndUsesIndependentShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan struct{})
	shutdownFresh := false
	current := &acceptanceService{
		wait: func(ctx context.Context) error { close(waiting); <-ctx.Done(); return ctx.Err() },
		shutdown: func(ctx context.Context) error {
			_, deadline := ctx.Deadline()
			shutdownFresh = deadline && ctx.Err() == nil
			return nil
		},
	}
	c := config.Default()
	wantPath := filepath.Join(t.TempDir(), "selected.yml")
	selected := config.Selection{Path: wantPath}
	var stderr bytes.Buffer
	deps := commandDependencies{
		load: func(path string) (config.Config, config.Selection, error) {
			if path != wantPath {
				t.Errorf("CLI заменил точный путь: %q", path)
			}
			return c, selected, nil
		},
		start: func(_ context.Context, received config.Config, source config.Selection) (service, error) {
			if source.Path != wantPath || received.HTTP.Address != c.HTTP.Address {
				t.Error("CLI потерял выбранный файл или конфигурацию")
			}
			return current, nil
		},
	}
	done := make(chan int, 1)
	go func() { done <- runWith(ctx, []string{"--config", wantPath}, &stderr, deps) }()
	select {
	case <-waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("CLI не перешёл к ожиданию сигнала")
	}
	cancel()
	select {
	case status := <-done:
		if status != 0 || !shutdownFresh || stderr.Len() != 0 {
			t.Fatalf("штатная отмена: status=%d, fresh=%v, stderr=%q", status, shutdownFresh, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CLI не завершился после отмены")
	}
}

func TestAcceptanceCLIOuterDeadlineBoundsUncooperativeStartAndClose(t *testing.T) {
	for _, stage := range []string{"startup", "shutdown"} {
		t.Run(stage, func(t *testing.T) {
			c := config.Default()
			c.Lifecycle.StartupTimeout, c.Lifecycle.ShutdownTimeout = 40*time.Millisecond, 40*time.Millisecond
			entered, released, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(released) }) }
			t.Cleanup(func() {
				release()
				select {
				case <-joined:
				case <-time.After(2 * time.Second):
					t.Error("тестовый владелец не завершён после снятия блокировки")
				}
			})
			failure := errors.New("контролируемый отказ Serve")
			current := &acceptanceService{
				wait: func(context.Context) error { return failure },
				shutdown: func(ctx context.Context) error {
					close(entered)
					<-released
					close(joined)
					return ctx.Err()
				},
			}
			deps := commandDependencies{
				load: func(string) (config.Config, config.Selection, error) {
					return c, config.Selection{Path: "selected.yaml"}, nil
				},
				start: func(context.Context, config.Config, config.Selection) (service, error) {
					if stage == "startup" {
						close(entered)
						<-released
						close(joined)
						return nil, failure
					}
					return current, nil
				},
			}
			var stderr bytes.Buffer
			done := make(chan int, 1)
			go func() { done <- runWith(context.Background(), nil, &stderr, deps) }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("контролируемая операция не началась")
			}
			select {
			case status := <-done:
				if status == 0 {
					t.Fatal("истечение внешнего срока объявлено успехом")
				}
				if stderr.Len() != 0 {
					t.Fatalf("при неподтверждённом завершении создан второй writer stderr: %q", stderr.String())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("внешний срок CLI не ограничил синхронно зависшую операцию")
			}
			release()
		})
	}
}

func TestAcceptanceCLICanceledBootstrapWaitsForPartialOwnerCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, noticedCancellation := make(chan struct{}), make(chan struct{})
	constructorJoined, cleaned, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var fresh bool
	current := &acceptanceService{
		wait: func(context.Context) error {
			t.Error("CLI ожидал Serve после отменённого startup")
			return errors.New("не должен вызываться")
		},
		shutdown: func(ctx context.Context) error {
			_, bounded := ctx.Deadline()
			fresh = bounded && ctx.Err() == nil
			close(cleaned)
			return nil
		},
	}
	c := config.Default()
	c.Lifecycle.ShutdownTimeout = time.Second
	deps := commandDependencies{
		load: func(string) (config.Config, config.Selection, error) { return c, config.Selection{}, nil },
		start: func(ctx context.Context, _ config.Config, _ config.Selection) (service, error) {
			defer close(constructorJoined)
			close(entered)
			<-ctx.Done()
			close(noticedCancellation)
			<-release
			return current, ctx.Err()
		},
	}
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runWith(ctx, nil, &stderr, deps) }()
	t.Cleanup(func() {
		cancel()
		unblock()
		select {
		case <-constructorJoined:
		case <-time.After(2 * time.Second):
			t.Error("тестовый конструктор не присоединён")
		}
	})
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("startup не начался")
	}
	cancel()
	select {
	case <-noticedCancellation:
	case <-time.After(2 * time.Second):
		t.Fatal("конструктор не получил отмену startup")
	}
	// Частичный владелец ещё не возвращён: CLI обязан сохранить ожидание до его выдачи.
	select {
	case <-done:
		t.Fatal("CLI завершился, пока конструктор ещё возвращает частичного владельца")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	select {
	case <-done:
		select {
		case <-cleaned:
			if !fresh {
				t.Fatal("partial handle получил уже отменённый shutdown context")
			}
		default:
			t.Fatal("CLI вышел до получения и очистки partial Application после отмены startup")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("отменённый startup не ограничен внешним сроком")
	}
}
