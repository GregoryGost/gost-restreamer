package integration

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

func sessionAcceptanceProcessLimits() processowner.Limits {
	return processowner.Limits{MaxOperations: 128, MaxRecordBytes: 65536, MaxCommandArgs: 128, MaxCommandBytes: 16384, MaxExtraFiles: 4, PageSize: 16}
}

func sessionAcceptanceBuildHelper(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gost-process-helper")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	args := []string{"build", "-o", path}
	if directory := os.Getenv("GOST_TEST_PROCESS_HELPER_COVERDIR"); directory != "" {
		t.Setenv("GOCOVERDIR", directory)
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=github.com/GregoryGost/gost-restreamer/...")
	}
	build := exec.CommandContext(ctx, "go", append(args, "../../cmd/gost-process-helper")...)
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-mod=readonly", "GOPROXY=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("сборка настоящего helper: %v\n%s", err, output)
	}
	return path
}

func sessionAcceptanceArgs(marker string) []string {
	for i, arg := range os.Args {
		if arg == marker {
			return os.Args[i+1:]
		}
	}
	return nil
}

// Backend и native запущены только этим тестом; SIGKILL направляется живому os.Process.
func TestProcessOwnerCrashBackendChild(t *testing.T) {
	args := sessionAcceptanceArgs("--owned-crash-backend")
	if args == nil {
		return
	}
	if len(args) != 4 {
		os.Exit(71)
	}
	registry, err := processowner.New(processowner.Config{Root: args[0], RuntimeRoot: args[1], HelperPath: args[2], Limits: sessionAcceptanceProcessLimits(), GracePeriod: 100 * time.Millisecond})
	if err != nil {
		os.Exit(72)
	}
	scope, err := registry.Create(context.Background(), "abrupt-backend")
	if err != nil {
		os.Exit(73)
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		os.Exit(74)
	}
	_, err = scope.Start(context.Background(), processowner.Command{Binary: os.Args[0], Args: []string{"-test.run=^TestProcessOwnerNativeChild$", "--", "--owned-native", "graceful", args[3]}, ExtraFiles: []*os.File{readyWrite}})
	if err != nil {
		os.Exit(75)
	}
	if err = readyWrite.Close(); err != nil {
		os.Exit(76)
	}
	var ready [1]byte
	if _, err = io.ReadFull(readyRead, ready[:]); err != nil || ready[0] != 1 {
		os.Exit(77)
	}
	if err = readyRead.Close(); err != nil {
		os.Exit(78)
	}
	status := os.NewFile(3, "backend-status")
	if status == nil {
		os.Exit(79)
	}
	if _, err = fmt.Fprintln(status, scope.ID()); err != nil {
		os.Exit(80)
	}
	if err = status.Close(); err != nil {
		os.Exit(81)
	}
	<-time.After(time.Hour)
}

func TestProcessOwnerNativeChild(t *testing.T) {
	args := sessionAcceptanceArgs("--owned-native")
	if args == nil {
		return
	}
	if len(args) != 2 {
		os.Exit(82)
	}
	interrupt := make(chan os.Signal, 1)
	if args[0] == "ignore" {
		signal.Ignore(os.Interrupt)
	} else {
		signal.Notify(interrupt, os.Interrupt)
	}
	ready := os.NewFile(3, "native-ready")
	if ready == nil {
		os.Exit(83)
	}
	if _, err := ready.Write([]byte{1}); err != nil {
		os.Exit(84)
	}
	if err := ready.Close(); err != nil {
		os.Exit(85)
	}
	if args[0] == "ignore" {
		<-time.After(15 * time.Second)
		return
	}
	select {
	case <-interrupt:
	case <-time.After(15 * time.Second):
		os.Exit(87)
	}
	if err := os.WriteFile(args[1], []byte("native interrupt observed"), 0600); err != nil {
		os.Exit(86)
	}
}

func TestProcessOwnerAbruptBackendDeathJoinsOnlyOldExecution(t *testing.T) {
	helper := sessionAcceptanceBuildHelper(t)
	base, err := os.MkdirTemp("/tmp", "owner-crash-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	root, runtime := filepath.Join(base, "journal"), filepath.Join(base, "runtime")
	interrupted := filepath.Join(base, "old-native-interrupted")
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = statusRead.Close() })
	backend := exec.Command(os.Args[0], "-test.run=^TestProcessOwnerCrashBackendChild$", "--", "--owned-crash-backend", root, runtime, helper, interrupted)
	backend.ExtraFiles = []*os.File{statusWrite}
	if err = backend.Start(); err != nil {
		_ = statusWrite.Close()
		t.Fatal(err)
	}
	backendJoined := false
	t.Cleanup(func() {
		if !backendJoined {
			_ = backend.Process.Kill()
			_ = backend.Wait()
		}
	})
	if err = statusWrite.Close(); err != nil {
		t.Fatal(err)
	}
	if err = statusRead.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	scopeID, err := bufio.NewReader(statusRead).ReadString('\n')
	if err != nil {
		t.Fatal("backend/native handshake:", err)
	}
	scopeID = strings.TrimSpace(scopeID)
	registry, err := processowner.New(processowner.Config{Root: root, RuntimeRoot: runtime, HelperPath: helper, Limits: sessionAcceptanceProcessLimits(), GracePeriod: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := registry.Close(); err != nil {
			t.Error(err)
		}
	})
	old, err := registry.OpenScope(context.Background(), scopeID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !backendJoined {
			_ = backend.Process.Kill()
			_ = backend.Wait()
			backendJoined = true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			proof, err := old.Resolve(ctx)
			if err == nil && proof.Joined {
				return
			}
			select {
			case <-ctx.Done():
				t.Errorf("старый native не присоединён при cleanup: %+v %v", proof, err)
				return
			case <-ticker.C:
			}
		}
	})
	proof, err := old.Resolve(context.Background())
	if err != nil || proof.Joined || proof.Sealed {
		t.Fatalf("живой backend ошибочно присоединён: %+v %v", proof, err)
	}
	fresh, err := registry.Create(context.Background(), "new-active-execution")
	if err != nil {
		t.Fatal(err)
	}
	newNative := sessionAcceptanceStartNative(t, fresh, "ignore", filepath.Join(base, "unused"))
	t.Cleanup(func() {
		newNative.Cancel()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if result, err := newNative.Wait(ctx); err != nil || !result.Joined {
			t.Errorf("новый native не присоединён: %+v %v", result, err)
		}
		if err := fresh.Close(); err != nil {
			t.Error(err)
		}
	})
	if err = backend.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = backend.Wait(); err == nil {
		t.Fatal("SIGKILL backend не наблюдался")
	}
	backendJoined = true
	sessionAcceptanceWait(t, func() bool {
		proof, err = old.Resolve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return proof.Joined
	})
	if !proof.Sealed || proof.Unknown || proof.PendingOperations != 0 || proof.Owner != "abrupt-backend" {
		t.Fatalf("ложное доказательство: %+v", proof)
	}
	data, err := os.ReadFile(interrupted)
	if err != nil || string(data) != "native interrupt observed" {
		t.Fatalf("guardian не сообщил native EOF/interrupt: %q %v", data, err)
	}
	if err = registry.AcknowledgeScope(context.Background(), old.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(old.Namespace()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("старый namespace остался: %v", err)
	}
	if _, err = os.Stat(fresh.Namespace()); err != nil {
		t.Fatal("старый cleanup затронул новый namespace:", err)
	}
	select {
	case <-newNative.Done():
		t.Fatal("старый cleanup завершил новый native")
	default:
	}
	ticket, err := fresh.Register(context.Background(), "work-after-old-ack")
	if err != nil {
		t.Fatal("старый ack закрыл новый scope:", err)
	}
	if err = ticket.Complete(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func sessionAcceptanceStartNative(t *testing.T, scope *processowner.Scope, mode, marker string) *processowner.Process {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := scope.Start(context.Background(), processowner.Command{Binary: os.Args[0], Args: []string{"-test.run=^TestProcessOwnerNativeChild$", "--", "--owned-native", mode, marker}, ExtraFiles: []*os.File{w}})
	if err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		p.Cancel()
		t.Fatal(err)
	}
	if err = r.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		p.Cancel()
		t.Fatal(err)
	}
	var b [1]byte
	if _, err = io.ReadFull(r, b[:]); err != nil || b[0] != 1 {
		p.Cancel()
		_, _ = p.Wait(ctx)
		t.Fatalf("native readiness: %v", err)
	}
	return p
}

func TestProcessOwnerCanceledWaitRetainsJoinAndSealedScopeRejectsLaunch(t *testing.T) {
	helper := sessionAcceptanceBuildHelper(t)
	registry, err := processowner.New(processowner.Config{Root: t.TempDir(), HelperPath: helper, Limits: sessionAcceptanceProcessLimits(), GracePeriod: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := registry.Close(); err != nil {
			t.Error(err)
		}
	})
	scope, err := registry.Create(context.Background(), "cancelled-wait")
	if err != nil {
		t.Fatal(err)
	}
	native := sessionAcceptanceStartNative(t, scope, "ignore", "unused")
	t.Cleanup(func() {
		native.Cancel()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = native.Wait(ctx)
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = native.Wait(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait: %v", err)
	}
	if err = scope.Seal(context.Background()); err != nil {
		t.Fatal(err)
	}
	proof, err := scope.Resolve(context.Background())
	if err != nil || proof.Joined || proof.PendingOperations == 0 {
		t.Fatalf("Wait timeout выдан за join: %+v %v", proof, err)
	}
	marker := filepath.Join(t.TempDir(), "must-not-launch")
	if _, err = scope.Start(context.Background(), processowner.Command{Binary: "/usr/bin/touch", Args: []string{marker}}); !errors.Is(err, processowner.ErrClosed) {
		t.Fatalf("sealed scope: %v", err)
	}
	if _, err = os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("sealed scope выполнил внешнее действие")
	}
	native.Cancel()
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	result, err := native.Wait(ctx)
	if err != nil || !result.Joined || result.ExitCode == 0 {
		t.Fatalf("эскалация без точного Wait-result: %+v %v", result, err)
	}
	if err = scope.Close(); err != nil {
		t.Fatal(err)
	}
	proof, err = scope.Resolve(ctx)
	if err != nil || !proof.Joined || proof.Unknown {
		t.Fatalf("sealed proof: %+v %v", proof, err)
	}
}
