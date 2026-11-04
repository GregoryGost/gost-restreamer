package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type ownedPermit struct {
	mu    sync.Mutex
	valid atomic.Bool
	done  chan struct{}
}

func newOwnedPermit() *ownedPermit {
	p := &ownedPermit{done: make(chan struct{})}
	p.valid.Store(true)
	return p
}
func (p *ownedPermit) Do(ctx context.Context, fn func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !p.valid.Load() {
		return ErrOwnerRevoked
	}
	return fn()
}
func (p *ownedPermit) Valid() bool           { return p.valid.Load() }
func (p *ownedPermit) Done() <-chan struct{} { return p.done }
func (p *ownedPermit) revoke() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.valid.Swap(false) {
		close(p.done)
	}
}

func shortBase(t *testing.T) string {
	t.Helper()
	parent := os.TempDir()
	if runtime.GOOS == "darwin" {
		// Системный TMPDIR macOS слишком длинный для вложенных путей UDS.
		parent = "/tmp"
	}
	base, err := os.MkdirTemp(parent, "gfr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	return base
}
func supervisorConfig(t *testing.T) Config {
	t.Helper()
	return Config{BaseDir: shortBase(t), StartupTimeout: time.Second, VideoTimeout: time.Second, AudioTimeout: time.Second, WatchdogInterval: 10 * time.Millisecond, MaxRestarts: 0, RestartBackoff: 10 * time.Millisecond, GracePeriod: 50 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 2 * time.Second, DiagnosticEvents: 8, DiagnosticBytes: 16384, DiagnosticChunkBytes: 256, MaxCommandBytes: 4096, MaxCommandArgs: 32}
}
func newTestSupervisor(t *testing.T, c Config) *Supervisor {
	t.Helper()
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}
func childCommand(mode string) CommandBuilder {
	return func(_ context.Context, a Attempt) (Command, error) {
		binary, err := os.Executable()
		return Command{Binary: binary, Args: []string{"-test.run=^TestSupervisorChild$", "--", "owned-child", mode, a.InputPath, a.OutputPath}}, err
	}
}
func inertHandler(ctx context.Context, _ net.Conn, _ Progress) error { <-ctx.Done(); return ctx.Err() }
func childWorker(mode string) Worker {
	return Worker{SessionID: "session/unsafe", GroupID: "group", Owner: newOwnedPermit(), Command: childCommand(mode), Input: inertHandler, Output: inertHandler}
}
func waitResult(t *testing.T, s *Supervisor) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := s.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestSupervisorChild выполняется только отдельным управляемым дочерним процессом.
func TestSupervisorChild(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "owned-child" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	mode := os.Args[index+1]
	switch mode {
	case "exit":
		os.Exit(3)
	case "hang":
		signal.Ignore(os.Interrupt)
		for {
			time.Sleep(time.Hour)
		}
	case "flood":
		data := bytes.Repeat([]byte{0xff, '\n'}, 1<<18)
		if _, err := os.Stdout.Write(data); err != nil {
			os.Exit(2)
		}
		if _, err := os.Stderr.Write(data); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	// Сначала output: последовательный Accept двух направлений создал бы deadlock.
	out, err := net.Dial("unix", os.Args[index+3])
	if err != nil {
		os.Exit(2)
	}
	if _, err = out.Write([]byte("v")); err != nil {
		os.Exit(2)
	}
	in, err := net.Dial("unix", os.Args[index+2])
	if err != nil {
		os.Exit(2)
	}
	if _, err = io.Copy(io.Discard, in); err != nil {
		os.Exit(2)
	}
	if _, err = out.Write([]byte("a")); err != nil {
		os.Exit(2)
	}
	if err = in.Close(); err != nil {
		os.Exit(2)
	}
	if mode == "connected-hang" {
		signal.Ignore(os.Interrupt)
		for {
			time.Sleep(time.Hour)
		}
	}
	if err = out.Close(); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestSupervisorDuplexCompletion(t *testing.T) {
	c := supervisorConfig(t)
	c.VideoTimeout, c.AudioTimeout = 3*time.Second, 3*time.Second
	s := newTestSupervisor(t, c)
	w := childWorker("duplex")
	w.Input = func(_ context.Context, c net.Conn, _ Progress) error { _, err := c.Write([]byte("input")); return err }
	w.Output = func(_ context.Context, c net.Conn, report Progress) error {
		// Задержка проверки пакетов позволяет процессу завершиться раньше demux.
		time.Sleep(30 * time.Millisecond)
		data := make([]byte, 1)
		for {
			_, err := io.ReadFull(c, data)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			track := Video
			if data[0] == 'a' {
				track = Audio
			}
			if !report(track, 1) {
				return errors.New("не принят первый пакет")
			}
		}
	}
	if err := s.Start(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	r := waitResult(t, s)
	if r.Err != nil || !r.Clean || r.Attempts != 1 {
		t.Fatalf("исход: %+v", r)
	}
	entries, err := os.ReadDir(s.config.BaseDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("остались ресурсы: %v", entries)
	}
}
func TestSupervisorEarlyExitHasFiniteRestartBudget(t *testing.T) {
	c := supervisorConfig(t)
	c.MaxRestarts = 2
	s := newTestSupervisor(t, c)
	if err := s.Start(context.Background(), childWorker("exit")); err != nil {
		t.Fatal(err)
	}
	r := waitResult(t, s)
	if r.Attempts != 3 || !r.Clean || !errors.Is(r.Err, ErrPrematureExit) {
		t.Fatalf("исход: %+v", r)
	}
	if err := s.Start(context.Background(), childWorker("exit")); !errors.Is(err, ErrStopped) {
		t.Fatalf("повторный Start: %v", err)
	}
}
func TestSupervisorStopsHangingProcessAndAccepts(t *testing.T) {
	c := supervisorConfig(t)
	c.StartupTimeout = 150 * time.Millisecond
	s := newTestSupervisor(t, c)
	if err := s.Start(context.Background(), childWorker("hang")); err != nil {
		t.Fatal(err)
	}
	r := waitResult(t, s)
	if !r.Clean || !errors.Is(r.Err, ErrStartupTimeout) {
		t.Fatalf("исход: %+v", r)
	}
}
func TestSupervisorDelayedBuilderCannotEscapeStop(t *testing.T) {
	c := supervisorConfig(t)
	c.ShutdownTimeout = 30 * time.Millisecond
	s := newTestSupervisor(t, c)
	entered, release := make(chan struct{}), make(chan struct{})
	w := childWorker("exit")
	w.Command = func(context.Context, Attempt) (Command, error) {
		close(entered)
		<-release
		return Command{Binary: "/usr/bin/false"}, nil
	}
	if err := s.Start(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-s.Done():
		t.Fatalf("startup завершился: %+v", waitResult(t, s))
	case <-time.After(3 * time.Second):
		t.Fatal("builder не вызван")
	}
	if err := s.Stop(context.Background()); !errors.Is(err, ErrCleanupPending) {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-s.Done():
		t.Fatal("ложное подтверждение cleanup")
	default:
	}
	close(release)
	r := waitResult(t, s)
	if !r.Clean || !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("исход: %+v", r)
	}
	for {
		d, err := s.NextDiagnostic(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if d.Kind == "command" {
			t.Fatal("процесс запущен после Stop")
		}
	}
}
func TestSupervisorUnjoinedHandlerKeepsCleanupPending(t *testing.T) {
	c := supervisorConfig(t)
	c.ShutdownTimeout = 30 * time.Millisecond
	s := newTestSupervisor(t, c)
	entered, release := make(chan struct{}), make(chan struct{})
	w := childWorker("connected-hang")
	w.Input = func(context.Context, net.Conn, Progress) error { return nil }
	w.Output = func(context.Context, net.Conn, Progress) error { close(entered); <-release; return nil }
	if err := s.Start(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler не вызван")
	}
	if err := s.Stop(context.Background()); !errors.Is(err, ErrCleanupPending) {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-s.Done():
		t.Fatal("callback не присоединён")
	default:
	}
	close(release)
	if r := waitResult(t, s); !r.Clean {
		t.Fatalf("исход: %+v", r)
	}
}
func TestSupervisorRevokedOwnerCancelsBuilder(t *testing.T) {
	s := newTestSupervisor(t, supervisorConfig(t))
	w := childWorker("exit")
	p := w.Owner.(*ownedPermit)
	entered := make(chan struct{})
	w.Command = func(ctx context.Context, _ Attempt) (Command, error) {
		close(entered)
		<-ctx.Done()
		return Command{}, ctx.Err()
	}
	if err := s.Start(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-s.Done():
		t.Fatalf("startup завершился: %+v", waitResult(t, s))
	case <-time.After(3 * time.Second):
		t.Fatal("builder не вызван")
	}
	p.revoke()
	r := waitResult(t, s)
	if !r.Clean || !errors.Is(r.Err, ErrOwnerRevoked) {
		t.Fatalf("исход: %+v", r)
	}
}
func TestSupervisorBlockedDiagnosticConsumerDoesNotBlockChild(t *testing.T) {
	s := newTestSupervisor(t, supervisorConfig(t))
	if err := s.Start(context.Background(), childWorker("flood")); err != nil {
		t.Fatal(err)
	}
	r := waitResult(t, s)
	if !r.Clean || s.DroppedDiagnostics() == 0 {
		t.Fatalf("исход: %+v, dropped=%d", r, s.DroppedDiagnostics())
	}
}
func TestProgressDuplicatesCannotRestoreHealth(t *testing.T) {
	p := &packetProgress{}
	if !p.report(Video, -10) || !p.report(Audio, -20) {
		t.Fatal("первые пакеты отклонены")
	}
	p.mu.Lock()
	p.at[0] = time.Now().Add(-time.Hour)
	p.mu.Unlock()
	if p.report(Video, -10) || p.report(Video, -11) {
		t.Fatal("дубликат обновил прогресс")
	}
	if err := p.health(time.Now(), time.Now(), Config{VideoTimeout: time.Second, AudioTimeout: time.Second}); !errors.Is(err, ErrProgressTimeout) {
		t.Fatal(err)
	}
}
func TestNamespaceOwnershipAndSingleAccept(t *testing.T) {
	base := shortBase(t)
	n, err := NewNamespace(base, "session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })
	accepted := make(chan error, 1)
	go func() { _, err := n.AcceptInput(); accepted <- err }()
	c, err := net.Dial("unix", n.InputPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err = <-accepted; err != nil {
		t.Fatal(err)
	}
	if _, err = n.AcceptInput(); !errors.Is(err, ErrAlreadyAccepted) {
		t.Fatal(err)
	}
	for _, path := range []string{n.InputPath, n.OutputPath} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatal(info.Mode())
		}
	}
	if err = os.Remove(n.OutputPath); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(n.OutputPath, []byte("чужой файл"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = n.Close(); !errors.Is(err, ErrResourceOwnership) {
		t.Fatal(err)
	}
	data, err := os.ReadFile(n.OutputPath)
	if err != nil || string(data) != "чужой файл" {
		t.Fatalf("чужой путь изменён: %q %v", data, err)
	}
}
func TestNamespaceRejectsSymlinkSession(t *testing.T) {
	base, foreign := shortBase(t), shortBase(t)
	if err := os.Symlink(foreign, filepath.Join(base, "session")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewNamespace(base, "session"); !errors.Is(err, ErrResourceOwnership) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(foreign)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink escape: %v %v", entries, err)
	}
}
func TestNamespaceShutdownUnblocksAccept(t *testing.T) {
	n, err := NewNamespace(shortBase(t), "session")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := n.AcceptOutput(); done <- err }()
	if err = n.Shutdown(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Accept не завершился")
	}
	if err = n.Close(); err != nil {
		t.Fatal(err)
	}
}
