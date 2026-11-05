package processowner

import (
	"bytes"
	"context"
	"encoding/json"
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
)

func testLimits() Limits {
	return Limits{MaxOperations: 64, MaxRecordBytes: 65536, MaxCommandArgs: 64, MaxCommandBytes: 16384, MaxExtraFiles: 4, PageSize: 16}
}

func buildHelper(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "gost-process-helper")
	args := []string{"build", "-o", binary}
	if directory := os.Getenv("GOST_TEST_PROCESS_HELPER_COVERDIR"); directory != "" {
		t.Setenv("GOCOVERDIR", directory)
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=github.com/GregoryGost/gost-restreamer/...")
	}
	cmd := exec.Command("go", append(args, "../../cmd/gost-process-helper")...)
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-mod=readonly", "GOPROXY=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper build: %v\n%s", err, output)
	}
	return binary
}

func testRegistry(t *testing.T, helper string) *Registry {
	t.Helper()
	r, err := New(Config{Root: filepath.Join(t.TempDir(), "journal"), HelperPath: helper, Limits: testLimits(), GracePeriod: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}
func testScope(t *testing.T, r *Registry) *Scope {
	t.Helper()
	s, err := r.Create(context.Background(), `{"version":1,"kind":"session","fence":{"id":"synthetic"}}`)
	if s != nil {
		t.Cleanup(func() {
			if err := s.CloseContext(testContext(t)); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestManagedProcesses(t *testing.T) {
	helper := buildHelper(t)
	t.Run("output-and-native-error", func(t *testing.T) {
		r := testRegistry(t, helper)
		s := testScope(t, r)
		var stdout, stderr bytes.Buffer
		result, err := s.Run(testContext(t), Command{Binary: "/bin/sh", Args: []string{"-c", "printf native-output; printf native-error >&2; exit 7"}, Stdout: &stdout, Stderr: &stderr})
		if err == nil || !result.Joined || result.ExitCode != 7 || stdout.String() != "native-output" || stderr.String() != "native-error" {
			t.Fatalf("result=%+v err=%v stdout=%q stderr=%q", result, err, stdout.String(), stderr.String())
		}
		if err = s.Seal(testContext(t)); err != nil {
			t.Fatal(err)
		}
		proof, err := s.Resolve(testContext(t))
		if err != nil || !proof.Joined || proof.Owner == "" {
			t.Fatalf("proof=%+v %v", proof, err)
		}
		if err = r.AcknowledgeScope(testContext(t), s.ID()); err != nil {
			t.Fatal(err)
		}
		if _, err = os.Stat(s.Namespace()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("namespace still present: %v", err)
		}
		proof, err = s.Resolve(testContext(t))
		if err != nil || !proof.Joined || !proof.Acknowledged {
			t.Fatalf("ack proof=%+v %v", proof, err)
		}
	})
	t.Run("wait-deadline-does-not-kill-owner", func(t *testing.T) {
		r := testRegistry(t, helper)
		s := testScope(t, r)
		ctx := testContext(t)
		p, err := s.Start(ctx, Command{Binary: "/bin/sleep", Args: []string{"100"}})
		if err != nil {
			t.Fatal(err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err = p.Wait(cancelled); !errors.Is(err, context.Canceled) {
			t.Fatalf("wait: %v", err)
		}
		select {
		case <-p.Done():
			t.Fatal("wait deadline terminated child")
		default:
		}
		p.Cancel()
		result, err := p.Wait(ctx)
		if err != nil || !result.Joined {
			t.Fatalf("join=%+v %v", result, err)
		}
	})
	t.Run("late-helper-denied-without-resurrection", func(t *testing.T) {
		r := testRegistry(t, helper)
		s := testScope(t, r)
		marker := filepath.Join(t.TempDir(), "must-not-exist")
		ticket, err := s.register(testContext(t), "native", &Command{Binary: "/bin/touch", Args: []string{marker}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		proof, err := s.Resolve(testContext(t))
		if err != nil || !proof.Joined {
			t.Fatalf("proof=%+v %v", proof, err)
		}
		cmd := exec.Command(helper, "1", r.config.Root, s.id, ticket.id)
		if err = cmd.Run(); err == nil {
			t.Fatal("late helper accepted CLOSED")
		}
		if _, err = os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("late child ran: %v", err)
		}
		if err = r.root.Remove(operationPath(s.id, ticket.id) + ".json"); err != nil {
			t.Fatal(err)
		}
		cmd = exec.Command(helper, "1", r.config.Root, s.id, ticket.id)
		if err = cmd.Run(); err == nil {
			t.Fatal("late helper accepted missing reservation")
		}
		if _, err = r.root.Stat(operationPath(s.id, ticket.id) + ".json"); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("missing reservation recreated")
		}
	})
	t.Run("spawn-marker-without-owner-remains-unknown", func(t *testing.T) {
		r := testRegistry(t, helper)
		s := testScope(t, r)
		ticket, err := s.register(testContext(t), "native", &Command{Binary: "/bin/true"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		op, err := s.readOperation(ticket.id)
		if err != nil {
			t.Fatal(err)
		}
		op.State = spawned
		if err = writeRecord(r.root, operationPath(s.id, ticket.id)+".json", op, r.config.Limits.MaxRecordBytes); err != nil {
			t.Fatal(err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		proof, err := s.Resolve(testContext(t))
		if err != nil || proof.Joined || !proof.Unknown {
			t.Fatalf("unknown=%+v %v", proof, err)
		}
		fresh := testScope(t, r)
		if fresh.ID() == s.ID() {
			t.Fatal("namespace reused")
		}
	})
	t.Run("catalog-and-backend-ticket", func(t *testing.T) {
		r := testRegistry(t, helper)
		s := testScope(t, r)
		ticket, err := s.Register(testContext(t), "builder")
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Close(); !errors.Is(err, ErrPending) {
			t.Fatalf("close=%v", err)
		}
		proof, err := s.Resolve(testContext(t))
		if err != nil || proof.Joined || proof.PendingOperations == 0 {
			t.Fatalf("proof=%+v %v", proof, err)
		}
		if err = ticket.Complete(testContext(t)); err != nil {
			t.Fatal(err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		proof, err = s.Resolve(testContext(t))
		if err != nil || !proof.Joined || proof.Operations != 2 {
			t.Fatalf("proof=%+v %v", proof, err)
		}
		page, next, err := r.RecoverPage(testContext(t), "", 1)
		if err != nil || len(page) != 1 || next == "" {
			t.Fatalf("page=%+v next=%s %v", page, next, err)
		}
		page, _, err = r.RecoverPage(testContext(t), next, 1)
		if err != nil || len(page) != 0 {
			t.Fatalf("last page=%+v %v", page, err)
		}
	})
	t.Run("helper-death-never-fabricates-native-join", func(t *testing.T) {
		r := testRegistry(t, helper)
		s := testScope(t, r)
		ctx := testContext(t)
		readyRead, readyWrite, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = readyRead.Close() }()
		controlRead, controlWrite, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = controlWrite.Close() }()
		p, err := s.Start(ctx, Command{Binary: os.Args[0], Args: []string{"-test.run=^TestControlledNativeChild$", "--", "--controlled-native-child"}, ExtraFiles: []*os.File{readyWrite, controlRead}})
		if err != nil {
			t.Fatal(err)
		}
		_ = readyWrite.Close()
		_ = controlRead.Close()
		var b [1]byte
		if _, err = io.ReadFull(readyRead, b[:]); err != nil {
			t.Fatal(err)
		}
		if err = p.helper.Kill(); err != nil {
			t.Fatal(err)
		}
		result, err := p.Wait(ctx)
		if err != nil || result.Joined || !errors.Is(result.Err, ErrUnknown) {
			t.Fatalf("helper death=%+v %v", result, err)
		}
		if err = controlWrite.Close(); err != nil {
			t.Fatal(err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		proof, err := s.Resolve(ctx)
		if err != nil || proof.Joined || !proof.Unknown {
			t.Fatalf("helper death proof=%+v %v", proof, err)
		}
	})
}

func TestControlledNativeChild(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "--controlled-native-child") {
		return
	}
	ready := os.NewFile(3, "native-ready")
	control := os.NewFile(4, "native-control")
	if ready == nil || control == nil {
		os.Exit(99)
	}
	_, _ = ready.Write([]byte{1})
	_ = ready.Close()
	var b [1]byte
	_, _ = control.Read(b[:])
	os.Exit(0)
}

// TestCrashBackendChild используется только как изолированный backend subprocess.
func TestCrashBackendChild(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "--crash-backend" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	args := os.Args[index+1:]
	if len(args) != 3 {
		os.Exit(91)
	}
	r, err := New(Config{Root: args[0], RuntimeRoot: args[1], HelperPath: args[2], Limits: testLimits(), GracePeriod: 20 * time.Millisecond})
	if err != nil {
		os.Exit(92)
	}
	s, err := r.Create(context.Background(), "crashed-backend")
	if err != nil {
		os.Exit(93)
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		os.Exit(94)
	}
	_, err = s.Start(context.Background(), Command{Binary: os.Args[0], Args: []string{"-test.run=^TestNativeCrashChild$", "--", "--native-crash-child"}, ExtraFiles: []*os.File{readyWrite}})
	if err != nil {
		os.Exit(95)
	}
	_ = readyWrite.Close()
	var ready [1]byte
	if _, err = io.ReadFull(readyRead, ready[:]); err != nil {
		os.Exit(96)
	}
	_, _ = fmt.Fprintln(os.Stdout, s.ID())
	os.Exit(0)
}

func TestNativeCrashChild(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "--native-crash-child") {
		return
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	ready := os.NewFile(3, "native-ready")
	if ready == nil {
		os.Exit(98)
	}
	_, _ = ready.Write([]byte{1})
	_ = ready.Close()
	<-interrupt
	os.Exit(0)
}

func TestBackendCrashGuardianRecovery(t *testing.T) {
	helper := buildHelper(t)
	root := filepath.Join(t.TempDir(), "journal")
	runtime := filepath.Join(t.TempDir(), "runtime")
	cmd := exec.CommandContext(testContext(t), os.Args[0], "-test.run=^TestCrashBackendChild$", "--", "--crash-backend", root, runtime, helper)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("backend=%v %s", err, output)
	}
	id := strings.TrimSpace(string(output))
	if !validID(id) {
		t.Fatalf("invalid scope output %q", output)
	}
	r, err := New(Config{Root: root, RuntimeRoot: runtime, HelperPath: helper, Limits: testLimits(), GracePeriod: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	s, err := r.OpenScope(testContext(t), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		proof, e := s.Resolve(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if proof.Joined {
			if proof.Owner != "crashed-backend" {
				t.Fatal(proof.Owner)
			}
			break
		}
		if proof.Unknown {
			b, _ := json.Marshal(proof)
			t.Fatalf("guardian lost proof: %s", b)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("guardian did not join: %+v", proof)
		case <-ticker.C:
		}
	}
}
