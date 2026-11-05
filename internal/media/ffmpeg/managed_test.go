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

func managedRegistry(t *testing.T) *processowner.Registry {
	t.Helper()
	helper := filepath.Join(t.TempDir(), "gost-process-helper")
	args := []string{"build", "-o", helper}
	if directory := os.Getenv("GOST_TEST_PROCESS_HELPER_COVERDIR"); directory != "" {
		t.Setenv("GOCOVERDIR", directory)
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=github.com/GregoryGost/gost-restreamer/...")
	}
	build := exec.Command("go", append(args, "../../../cmd/gost-process-helper")...)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("сборка helper: %v: %s", err, output)
	}
	registry, err := processowner.New(processowner.Config{
		Root: t.TempDir(), RuntimeRoot: shortBase(t), HelperPath: helper,
		GracePeriod: 30 * time.Millisecond,
		Limits:      processowner.Limits{MaxOperations: 100, MaxRecordBytes: 16384, MaxCommandArgs: 128, MaxCommandBytes: 4096, MaxExtraFiles: 1, PageSize: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := registry.Close(); err != nil {
			t.Error(err)
		}
	})
	return registry
}

func TestManagedSupervisorFileDescriptorAndNamespaceLifecycle(t *testing.T) {
	registry := managedRegistry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	scope, err := registry.Create(ctx, "exact-session-execution")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close() })
	f, err := os.CreateTemp(t.TempDir(), "media")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("seekable-native-fd3"); err != nil {
		t.Fatal(err)
	}
	config := supervisorConfig(t)
	config.StartupTimeout, config.VideoTimeout, config.AudioTimeout = 3*time.Second, 3*time.Second, 3*time.Second
	supervisor := newTestSupervisor(t, config)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	worker := Worker{SessionID: "session", GroupID: "file", Owner: newOwnedPermit(), ProcessScope: scope, Topology: FileInput,
		Command: func(_ context.Context, a Attempt) (Command, error) {
			return Command{Binary: binary, Args: []string{"-test.run=^TestManagedSupervisorNativeChild$", "--", "managed-native", a.OutputPath}, ExtraFiles: []*os.File{f}}, nil
		},
		Output: func(_ context.Context, conn net.Conn, progress Progress) error {
			var markers [2]byte
			if _, err := io.ReadFull(conn, markers[:]); err != nil {
				return err
			}
			if string(markers[:]) != "VA" {
				return errors.New("неверные данные native child")
			}
			progress(Video, 1)
			progress(Audio, 1)
			_, err := io.Copy(io.Discard, conn)
			return err
		},
	}
	if err = supervisor.Start(ctx, worker); err != nil {
		t.Fatal(err)
	}
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
	for i, arg := range os.Args {
		if arg != "managed-native" {
			continue
		}
		f := os.NewFile(3, "native-input")
		got, err := io.ReadAll(f)
		if err != nil || string(got) != "seekable-native-fd3" {
			os.Exit(20)
		}
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

func TestManagedSupervisorSealedScopeRejectsBuilder(t *testing.T) {
	registry := managedRegistry(t)
	ctx := context.Background()
	scope, err := registry.Create(ctx, "sealed-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close() })
	if err = scope.Seal(ctx); err != nil {
		t.Fatal(err)
	}
	supervisor := newTestSupervisor(t, supervisorConfig(t))
	worker := childWorker("exit")
	worker.ProcessScope = scope
	called := false
	worker.Command = func(context.Context, Attempt) (Command, error) { called = true; return Command{}, nil }
	if err = supervisor.Start(ctx, worker); err != nil {
		t.Fatal(err)
	}
	result := waitResult(t, supervisor)
	if called || !errors.Is(result.Err, processowner.ErrClosed) {
		t.Fatalf("seal допустил builder: %v %+v", called, result)
	}
}
