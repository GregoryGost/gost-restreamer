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

// acceptanceService подставляет управляемые сценарии ожидания и остановки приложения для проверок CLI.
type acceptanceService struct {
	// wait выполняет настроенный тестом сценарий обслуживания до отмены или отказа.
	wait func(context.Context) error
	// shutdown выполняет настроенный тестом сценарий освобождения сервиса, включая намеренное
	// зависание.
	shutdown func(context.Context) error
	// mu защищает список зарегистрированных ошибок при обращении из горутин CLI.
	mu sync.Mutex
	// logged сохраняет переданные CLI ошибки для проверки единственного пути журналирования.
	logged []error
}

// Wait вызывает настроенный тестом сценарий ожидания сервиса.
func (a *acceptanceService) Wait(ctx context.Context) error { return a.wait(ctx) }

// Shutdown передаёт контекст настроенному сценарию остановки, включая управляемое зависание.
func (a *acceptanceService) Shutdown(ctx context.Context) error { return a.shutdown(ctx) }

// LogError сохраняет ошибки под блокировкой для проверки поведения CLI.
func (a *acceptanceService) LogError(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logged = append(a.logged, err)
}

// Проверяет отклонение неизвестных, позиционных и незавершённых аргументов до загрузки конфигурации.
func TestAcceptanceCLIRejectsArgumentsBeforeLoading(t *testing.T) {
	// args задаёт очередной некорректный набор аргументов, который должен отклоняться до загрузки
	// конфигурации.
	for _, args := range [][]string{{"--unknown"}, {"unexpected"}, {"--config"}, {"--config="}} {
		// stderr сохраняет диагностику отклонённых аргументов.
		var stderr bytes.Buffer
		// loaded отмечает ошибочный вызов загрузчика при некорректных аргументах.
		loaded := false
		// status содержит код CLI после разбора текущего ошибочного набора аргументов.
		status := runWith(context.Background(), args, &stderr, commandDependencies{load: func(string) (config.Config, config.Selection, error) {
			// Отмечает запрещённое обращение к загрузчику при некорректных аргументах CLI.

			loaded = true
			return config.Config{}, config.Selection{}, errors.New("не должен вызываться")
		}})
		if status != 2 || loaded || stderr.Len() == 0 {
			t.Fatalf("некорректные аргументы %q: status=%d, loaded=%v, stderr=%q", args, status, loaded, stderr.String())
		}
	}
}

// Проверяет, что ошибка выбранного YAML с указанием пути и поля препятствует созданию приложения.
func TestAcceptanceCLIInvalidFilePrecedesApplication(t *testing.T) {
	// path указывает временный YAML с неизвестным полем HTTP для проверки отказа до запуска
	// приложения.
	path := filepath.Join(t.TempDir(), "config.yml")
	// err содержит отказ записи ошибочного YAML в изолированный файл теста.
	if err := os.WriteFile(path, []byte("http:\n  misspelled_address: ':8080'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// started отмечает запрещённое создание приложения после ошибки YAML.
	started := false
	// stderr сохраняет диагностику пути и неизвестного поля конфигурации.
	var stderr bytes.Buffer
	// status содержит код CLI после чтения выбранного некорректного YAML.
	status := runWith(context.Background(), []string{"--config", path}, &stderr, commandDependencies{start: func(context.Context, config.Config, config.Selection) (service, error) {
		// Отмечает ошибочный запуск приложения после отказа разбора выбранного YAML.

		started = true
		return nil, nil
	}})
	if status == 0 || started || !strings.Contains(stderr.String(), path) || !strings.Contains(stderr.String(), "misspelled_address") {
		t.Fatalf("ошибка YAML не остановила bootstrap: status=%d, started=%v, stderr=%q", status, started, stderr.String())
	}
}

// Проверяет передачу точного пути конфигурации и остановку по свежему ограниченному контексту после
// отмены рабочего.
func TestAcceptanceCLIPropagatesExactPathAndUsesIndependentShutdown(t *testing.T) {
	// ctx управляет рабочим ожиданием CLI; cancel посылает отмену после подтверждения начала
	// ожидания сервиса.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// waiting закрывается при входе сервиса в Wait и позволяет отменить CLI в нужной стадии.
	waiting := make(chan struct{})
	// shutdownFresh отмечает получение действующего контекста остановки с отдельным сроком.
	shutdownFresh := false
	// current моделирует сервис, который ждёт отмены рабочего контекста и проверяет независимый
	// контекст очистки.
	current := &acceptanceService{
		wait: func(ctx context.Context) error {
			// Сообщает о начале ожидания и удерживает сервис до отмены контекста CLI.
			close(waiting)
			<-ctx.Done()
			return ctx.Err()
		},
		shutdown: func(ctx context.Context) error {
			// Проверяет, что остановка получает ещё действующий контекст с отдельным сроком.

			// deadline показывает наличие конечного срока у контекста, переданного в
			// Shutdown.
			_, deadline := ctx.Deadline()
			shutdownFresh = deadline && ctx.Err() == nil
			return nil
		},
	}
	// c содержит штатные настройки, передаваемые от подставленного загрузчика конструктору сервиса.
	c := config.Default()
	// wantPath задаёт точный путь --config, сохранность которого проверяется на обеих границах CLI.
	wantPath := filepath.Join(t.TempDir(), "selected.yml")
	// selected описывает выбранный YAML, передаваемый загрузчиком вместе с настройками.
	selected := config.Selection{Path: wantPath}
	// stderr перехватывает нежелательную диагностику штатной остановки по отмене.
	var stderr bytes.Buffer
	// deps проверяет точную передачу пути, настроек и сведений об источнике до создания тестового
	// сервиса.
	deps := commandDependencies{
		load: func(path string) (config.Config, config.Selection, error) {
			// Проверяет точное сохранение пути --config и возвращает выбранную конфигурацию.

			if path != wantPath {
				t.Errorf("CLI заменил точный путь: %q", path)
			}
			return c, selected, nil
		},
		start: func(_ context.Context, received config.Config, source config.Selection) (service, error) {
			// Проверяет передачу выбранного файла и конфигурации из CLI в конструктор сервиса.

			if source.Path != wantPath || received.HTTP.Address != c.HTTP.Address {
				t.Error("CLI потерял выбранный файл или конфигурацию")
			}
			return current, nil
		},
	}
	// done передаёт код CLI после обработки отмены и остановки сервиса.
	done := make(chan int, 1)
	go func() {
		// Запускает CLI параллельно, чтобы тест отменил контекст после перехода сервиса к ожиданию.
		done <- runWith(ctx, []string{"--config", wantPath}, &stderr, deps)
	}()
	select {
	case <-waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("CLI не перешёл к ожиданию сигнала")
	}
	cancel()
	select {
	// status содержит полученный код штатного завершения CLI после отмены.
	case status := <-done:
		if status != 0 || !shutdownFresh || stderr.Len() != 0 {
			t.Fatalf("штатная отмена: status=%d, fresh=%v, stderr=%q", status, shutdownFresh, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CLI не завершился после отмены")
	}
}

// Проверяет, что внешний срок CLI завершает ожидание даже при синхронно зависших запуске или остановке
// без второго вывода в stderr.
func TestAcceptanceCLIOuterDeadlineBoundsUncooperativeStartAndClose(t *testing.T) {
	// stage выбирает намеренно зависшую операцию: создание приложения или его остановку.
	for _, stage := range []string{"startup", "shutdown"} {
		t.Run(stage, func(t *testing.T) {
			// Проверяет ограничение внешним сроком для выбранной зависшей стадии запуска или остановки.

			// c задаёт короткие независимые сроки запуска и остановки для проверки внешнего
			// ограничения CLI.
			c := config.Default()
			c.Lifecycle.StartupTimeout, c.Lifecycle.ShutdownTimeout = 40*time.Millisecond, 40*time.Millisecond
			// entered подтверждает начало зависшей операции; released разрешает ей
			// продолжиться; joined подтверждает выход тестового владельца.
			entered, released, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
			// releaseOnce обеспечивает единственное закрытие канала released при очистке и
			// явном освобождении.
			var releaseOnce sync.Once
			// release снимает тестовую блокировку через releaseOnce, позволяя безопасные
			// повторные вызовы.
			release := func() {
				// Освобождает заблокированную тестовую операцию только один раз.

				releaseOnce.Do(func() {
					// Закрывает канал освобождения при первом вызове release.
					close(released)
				})
			}
			t.Cleanup(func() {
				// Снимает блокировку тестового владельца и ограниченно ожидает его завершения после проверки CLI.

				release()
				select {
				case <-joined:
				case <-time.After(2 * time.Second):
					t.Error("тестовый владелец не завершён после снятия блокировки")
				}
			})
			// failure задаёт узнаваемый отказ обслуживания или запуска, переводящий CLI к
			// ошибочному завершению.
			failure := errors.New("контролируемый отказ Serve")
			// current моделирует отказ Wait и остановку, намеренно игнорирующую отмену до
			// освобождения тестом.
			current := &acceptanceService{
				wait: func(context.Context) error {
					// Имитирует ошибку Serve, переводящую CLI к остановке сервиса.
					return failure
				},
				shutdown: func(ctx context.Context) error {
					// Блокирует остановку до явного освобождения, игнорируя отмену её контекста.

					close(entered)
					<-released
					close(joined)
					return ctx.Err()
				},
			}
			// deps возвращает короткие сроки и удерживает выбранную стадию, чтобы проверить
			// внешний срок CLI.
			deps := commandDependencies{
				load: func(string) (config.Config, config.Selection, error) {
					// Возвращает короткие сроки жизненного цикла для проверки внешнего ограничения CLI.

					return c, config.Selection{Path: "selected.yaml"}, nil
				},
				start: func(context.Context, config.Config, config.Selection) (service, error) {
					// Для стадии startup задерживает возврат конструктора до снятия блокировки тестом.

					if stage == "startup" {
						close(entered)
						<-released
						close(joined)
						return nil, failure
					}
					return current, nil
				},
			}
			// stderr сохраняет вывод CLI для обнаружения второго получателя диагностики при
			// неподтверждённом завершении.
			var stderr bytes.Buffer
			// done передаёт код CLI, позволяя проверить его возврат при ещё заблокированной
			// операции.
			done := make(chan int, 1)
			go func() {
				// Запускает CLI отдельно, позволяя проверить его возврат при зависшей операции.
				done <- runWith(context.Background(), nil, &stderr, deps)
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("контролируемая операция не началась")
			}
			select {
			// status содержит код возврата CLI при истечении внешнего срока зависшей
			// операции.
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

// Проверяет, что отмена запуска сохраняет ограниченное ожидание частичного сервиса и вызывает его
// очистку до выхода CLI.
func TestAcceptanceCLICanceledBootstrapWaitsForPartialOwnerCleanup(t *testing.T) {
	// ctx управляет отменяемым запуском CLI; cancel инициирует отмену и повторно вызывается при
	// очистке теста.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// entered подтверждает вход в конструктор; noticedCancellation подтверждает получение им отмены
	// запуска.
	entered, noticedCancellation := make(chan struct{}), make(chan struct{})
	// constructorJoined подтверждает завершение конструктора; cleaned подтверждает очистку
	// частичного сервиса; release разрешает конструктору вернуть владельца.
	constructorJoined, cleaned, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	// releaseOnce защищает однократное закрытие release при явном освобождении и очистке теста.
	var releaseOnce sync.Once
	// unblock разрешает конструктору вернуть частично созданный сервис, безопасно допуская повторные
	// вызовы.
	unblock := func() {
		// Разблокирует возврат частично созданного сервиса только один раз.

		releaseOnce.Do(func() {
			// Закрывает канал, разрешающий конструктору вернуть частичного владельца.
			close(release)
		})
	}
	// fresh отмечает действующий ограниченный контекст, полученный частичным сервисом при очистке.
	var fresh bool
	// current представляет частичный сервис: Wait запрещён, а Shutdown подтверждает очистку и свежий
	// контекст.
	current := &acceptanceService{
		wait: func(context.Context) error {
			// Фиксирует ошибочный переход к ожиданию Serve после отмены запуска.

			t.Error("CLI ожидал Serve после отменённого startup")
			return errors.New("не должен вызываться")
		},
		shutdown: func(ctx context.Context) error {
			// Подтверждает получение свежего ограниченного контекста и завершение очистки частичного владельца.

			// bounded показывает наличие срока у контекста очистки частичного сервиса.
			_, bounded := ctx.Deadline()
			fresh = bounded && ctx.Err() == nil
			close(cleaned)
			return nil
		},
	}
	// c задаёт настройки запуска с отдельной секундой для возврата и очистки частичного владельца.
	c := config.Default()
	c.Lifecycle.ShutdownTimeout = time.Second
	// deps удерживает частичного владельца после отмены запуска до явного разрешения теста.
	deps := commandDependencies{
		load: func(string) (config.Config, config.Selection, error) {
			// Возвращает конфигурацию с отдельным сроком остановки для отменяемого запуска.
			return c, config.Selection{}, nil
		},
		start: func(ctx context.Context, _ config.Config, _ config.Selection) (service, error) {
			// После отмены запуска удерживает частичного владельца до разрешения теста и возвращает его вместе с
			// ошибкой контекста.

			defer close(constructorJoined)
			close(entered)
			<-ctx.Done()
			close(noticedCancellation)
			<-release
			return current, ctx.Err()
		},
	}
	// stderr перехватывает диагностику CLI во время отменённого частичного запуска.
	var stderr bytes.Buffer
	// done уведомляет о возврате CLI для проверки, что тот дождался выдачи владельца и его очистки.
	done := make(chan int, 1)
	go func() {
		// Запускает CLI параллельно, чтобы проверить ожидание возврата и очистки частичного владельца.
		done <- runWith(ctx, nil, &stderr, deps)
	}()
	t.Cleanup(func() {
		// Отменяет запуск, освобождает конструктор и ограниченно ожидает завершения его горутины.

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
