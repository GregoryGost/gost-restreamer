package ffmpeg

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// Собирает настоящий gost-process-helper во временный каталог и открывает изолированный реестр с
// короткими путями UDS и закрытием после теста.
func managedRegistry(t *testing.T) *processowner.Registry {
	t.Helper()
	// helper задаёт временный путь собираемого gost-process-helper.
	helper := filepath.Join(t.TempDir(), "gost-process-helper")
	// args накапливает аргументы сборки helper с необязательной инструментацией покрытия.
	args := []string{"build", "-o", helper}
	// directory содержит явно переданный каталог покрытия настоящих вспомогательных процессов.
	if directory := os.Getenv("GOST_TEST_PROCESS_HELPER_COVERDIR"); directory != "" {
		t.Setenv("GOCOVERDIR", directory)
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=github.com/GregoryGost/gost-restreamer/...")
	}
	// build собирает helper из исходников проекта во временный файл теста.
	build := exec.Command("go", append(args, "../../../cmd/gost-process-helper")...)
	// output содержит диагностику сборки helper; err сообщает об отказе сборки.
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("сборка helper: %v: %s", err, output)
	}
	// registry содержит изолированный реестр процессов; err сообщает об отказе его создания.
	registry, err := processowner.New(processowner.Config{
		Root: t.TempDir(), RuntimeRoot: shortBase(t), HelperPath: helper,
		GracePeriod: 30 * time.Millisecond,
		Limits:      processowner.Limits{MaxOperations: 100, MaxRecordBytes: 16384, MaxCommandArgs: 128, MaxCommandBytes: 4096, MaxExtraFiles: 1, PageSize: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает тестовый реестр процессов и сообщает ошибку через t.Error.

		// err сообщает об отказе закрытия тестового реестра процессов при очистке.
		if err := registry.Close(); err != nil {
			t.Error(err)
		}
	})
	return registry
}

// Проверяет FD 3 и обе контрольные дорожки через настоящий управляемый процесс, закрытие файла и
// удаление пространства с подтверждением Joined.
func TestManagedSupervisorFileDescriptorAndNamespaceLifecycle(t *testing.T) {
	// registry учитывает настоящий helper и ресурсы контрольного файлового работника.
	registry := managedRegistry(t)
	// ctx ограничивает проверку жизненного цикла десятью секундами; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// scope учитывает операции конкретного выполнения; err сохраняет результаты создания, запуска и
	// подтверждения очистки.
	scope, err := registry.Create(ctx, "exact-session-execution")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает область выполнения после сценария, игнорируя ошибку резервного закрытия.
		_ = scope.Close()
	})
	// f содержит контрольный файл, передаваемый настоящему дочернему процессу как FD 3.
	f, err := os.CreateTemp(t.TempDir(), "media")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("seekable-native-fd3"); err != nil {
		t.Fatal(err)
	}
	// config увеличивает сроки ожидания дорожек для сценария с настоящим helper.
	config := supervisorConfig(t)
	config.StartupTimeout, config.VideoTimeout, config.AudioTimeout = 3*time.Second, 3*time.Second, 3*time.Second
	// supervisor выполняет одну управляемую файловую попытку.
	supervisor := newTestSupervisor(t, config)
	// binary задаёт текущий тестовый бинарник, запускаемый helper как дочерний процесс.
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// worker связывает файловый вход, ProcessScope и проверку обеих выходных дорожек.
	worker := Worker{SessionID: "session", GroupID: "file", Owner: newOwnedPermit(), ProcessScope: scope, Topology: FileInput,
		Command: func(_ context.Context, a Attempt) (Command, error) {
			// Строит команду тестового дочернего процесса с выходным UDS и передаёт открытый файл как FD 3.

			return Command{Binary: binary, Args: []string{"-test.run=^TestManagedSupervisorNativeChild$", "--", "managed-native", a.OutputPath}, ExtraFiles: []*os.File{f}}, nil
		},
		Output: func(_ context.Context, conn net.Conn, progress Progress) error {
			// Проверяет маркеры VA от настоящего дочернего процесса, сообщает прогресс обеих дорожек и дочитывает
			// соединение до завершения.

			// markers содержит контрольные байты VA от дочернего процесса.
			var markers [2]byte
			// err сообщает об отказе чтения контрольных маркеров из выходного UDS.
			if _, err := io.ReadFull(conn, markers[:]); err != nil {
				return err
			}
			if string(markers[:]) != "VA" {
				return errors.New("неверные данные native child")
			}
			progress(Video, 1)
			progress(Audio, 1)
			// err содержит результат дочитывания выходного UDS после сообщения прогресса.
			_, err := io.Copy(io.Discard, conn)
			return err
		},
	}
	if err = supervisor.Start(ctx, worker); err != nil {
		t.Fatal(err)
	}
	// result содержит окончательный исход управляемого файлового работника и очистки.
	result := waitResult(t, supervisor)
	if result.Err != nil || !result.Clean {
		t.Fatalf("управляемый Worker: %+v", result)
	}
	if _, err = f.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("входной FD не закрыт", err)
	}
	if err = scope.Seal(ctx); err != nil {
		t.Fatal(err)
	}
	if err = scope.Close(); err != nil {
		t.Fatal(err)
	}
	// proof содержит доказательство завершения Scope после Seal и Close.
	proof, err := scope.Resolve(ctx)
	if err != nil || !proof.Joined {
		t.Fatalf("scope: %+v %v", proof, err)
	}
	if _, err = os.Lstat(scope.Namespace()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("namespace не удалён", err)
	}
	if err = registry.AcknowledgeScope(ctx, scope.ID()); err != nil {
		t.Fatal(err)
	}
}

// TestManagedSupervisorNativeChild проверяет FD 3 в дочернем процессе реального helper.
func TestManagedSupervisorNativeChild(t *testing.T) {
	// i обозначает позицию режима дочернего процесса; arg содержит очередной аргумент запуска.
	for i, arg := range os.Args {
		if arg != "managed-native" {
			continue
		}
		// f представляет входной FD 3, переданный через настоящий helper.
		f := os.NewFile(3, "native-input")
		// got содержит прочитанные байты FD 3; err сохраняет результаты чтения и обмена через
		// выходной UDS.
		got, err := io.ReadAll(f)
		if err != nil || string(got) != "seekable-native-fd3" {
			os.Exit(20)
		}
		// out содержит UDS-соединение для передачи контрольных маркеров VA.
		out, err := net.Dial("unix", os.Args[i+1])
		if err != nil {
			os.Exit(21)
		}
		if _, err = out.Write([]byte("VA")); err != nil {
			os.Exit(22)
		}
		if err = out.Close(); err != nil {
			os.Exit(23)
		}
		os.Exit(0)
	}
}

// Проверяет отказ регистрации в закрытом Scope до вызова построителя команды.
func TestManagedSupervisorSealedScopeRejectsBuilder(t *testing.T) {
	// registry содержит изолированный реестр для проверки запрета работы после Seal.
	registry := managedRegistry(t)
	// ctx используется для последовательных операций создания и закрытия тестового Scope.
	ctx := context.Background()
	// scope содержит область, запечатываемую до запуска работника; err сохраняет результаты операций
	// её жизненного цикла.
	scope, err := registry.Create(ctx, "sealed-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает заранее запечатанную область после сценария, игнорируя ошибку.
		_ = scope.Close()
	})
	if err = scope.Seal(ctx); err != nil {
		t.Fatal(err)
	}
	// supervisor проверяет отказ попытки до вызова построителя в запечатанной области.
	supervisor := newTestSupervisor(t, supervisorConfig(t))
	// worker содержит контрольную команду с уже запечатанным ProcessScope.
	worker := childWorker("exit")
	worker.ProcessScope = scope
	// called отмечает недопустимый вызов построителя команды после Seal.
	called := false
	worker.Command = func(context.Context, Attempt) (Command, error) {
		// Отмечает вызов построителя команды, который тест запрещает после Seal.
		called = true
		return Command{}, nil
	}
	if err = supervisor.Start(ctx, worker); err != nil {
		t.Fatal(err)
	}
	// result содержит отказ попытки для проверки processowner.ErrClosed и отсутствия вызова
	// построителя.
	result := waitResult(t, supervisor)
	if called || !errors.Is(result.Err, processowner.ErrClosed) {
		t.Fatalf("seal допустил builder: %v %+v", called, result)
	}
}
