package ffmpeg_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

type acceptanceOwner struct {
	mu    sync.Mutex
	done  chan struct{}
	valid bool
}

func newAcceptanceOwner() *acceptanceOwner {
	return &acceptanceOwner{done: make(chan struct{}), valid: true}
}
func (o *acceptanceOwner) Do(ctx context.Context, fn func() error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !o.valid {
		return ffmpeg.ErrOwnerRevoked
	}
	return fn()
}
func (o *acceptanceOwner) Valid() bool           { o.mu.Lock(); defer o.mu.Unlock(); return o.valid }
func (o *acceptanceOwner) Done() <-chan struct{} { return o.done }
func (o *acceptanceOwner) revoke() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.valid {
		o.valid = false
		close(o.done)
	}
}

func acceptanceSupervisor(t *testing.T, change func(*ffmpeg.Config)) (*ffmpeg.Supervisor, string) {
	t.Helper()
	parent := os.TempDir()
	if runtime.GOOS == "darwin" {
		// Системный TMPDIR macOS слишком длинный для вложенных путей UDS.
		parent = "/tmp"
	}
	base, err := os.MkdirTemp(parent, "gs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	cfg := ffmpeg.Config{BaseDir: base, StartupTimeout: time.Second, VideoTimeout: 150 * time.Millisecond, AudioTimeout: 150 * time.Millisecond, WatchdogInterval: 10 * time.Millisecond, MaxRestarts: 0, RestartBackoff: 10 * time.Millisecond, GracePeriod: 30 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 2 * time.Second, DiagnosticEvents: 1024, DiagnosticBytes: 1 << 20, DiagnosticChunkBytes: 64, MaxCommandBytes: 4096, MaxCommandArgs: 32}
	if change != nil {
		change(&cfg)
	}
	s, err := ffmpeg.New(cfg)
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
	return s, base
}

func acceptanceChildCommand(t *testing.T, mode string, seen chan<- ffmpeg.Attempt) ffmpeg.CommandBuilder {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return func(_ context.Context, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		if seen != nil {
			seen <- a
		}
		return ffmpeg.Command{Binary: binary, Args: []string{"-test.run=^TestAcceptanceSupervisorChild$", "--", "media-helper", mode, a.InputPath, a.OutputPath}}, nil
	}
}

func acceptanceBlockInput(ctx context.Context, _ net.Conn, _ ffmpeg.Progress) error {
	<-ctx.Done()
	return ctx.Err()
}

func acceptanceOutput(ready chan<- struct{}, repeated chan<- bool) ffmpeg.IOHandler {
	return func(_ context.Context, c net.Conn, progress ffmpeg.Progress) error {
		var marker [2]byte
		if _, err := io.ReadFull(c, marker[:]); err != nil {
			return err
		}
		if marker != [2]byte{'V', 'A'} {
			return errors.New("неверный синтетический пакет")
		}
		if !progress(ffmpeg.Video, -10) || !progress(ffmpeg.Audio, -20) {
			return errors.New("первый валидный пакет отвергнут")
		}
		if ready != nil {
			close(ready)
		}
		for {
			var one [1]byte
			if _, err := c.Read(one[:]); err != nil {
				return err
			}
			if repeated != nil {
				repeated <- progress(ffmpeg.Video, -10) || progress(ffmpeg.Audio, -20)
			}
		}
	}
}

func acceptanceWait(t *testing.T, s *ffmpeg.Supervisor) ffmpeg.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	r, err := s.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Clean || r.CleanupErr != nil {
		t.Fatalf("нет доказательства очистки: %+v", r)
	}
	return r
}

func TestAcceptanceSupervisorChild(t *testing.T) {
	pos := -1
	for i, arg := range os.Args {
		if arg == "media-helper" {
			pos = i
			break
		}
	}
	if pos < 0 {
		return
	}
	if len(os.Args) < pos+4 {
		os.Exit(91)
	}
	mode, input, output := os.Args[pos+1], os.Args[pos+2], os.Args[pos+3]
	if mode == "early" {
		os.Exit(23)
	}
	if mode == "diagnostics" {
		payload := bytes.Repeat([]byte{0xff, 0, 0xc3, 0xa9, '\n'}, 8192)
		if _, err := os.Stderr.Write(payload); err != nil {
			os.Exit(92)
		}
	}
	i, err := net.DialTimeout("unix", input, time.Second)
	if err != nil {
		os.Exit(93)
	}
	defer func() { _ = i.Close() }()
	o, err := net.DialTimeout("unix", output, time.Second)
	if err != nil {
		os.Exit(94)
	}
	defer func() { _ = o.Close() }()
	if _, err = o.Write([]byte("VA")); err != nil {
		os.Exit(95)
	}
	if mode == "disconnect" {
		_ = o.Close()
		_, _ = io.Copy(io.Discard, i)
		return
	}
	if mode == "duplicates" {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if _, err := o.Write([]byte{'D'}); err != nil {
				return
			}
		}
	}
	_, _ = io.Copy(io.Discard, i)
}

func TestAcceptanceSupervisorEarlyExitRestartsAndCleansEveryNamespace(t *testing.T) {
	s, base := acceptanceSupervisor(t, func(c *ffmpeg.Config) { c.MaxRestarts = 2 })
	seen := make(chan ffmpeg.Attempt, 3)
	if err := s.Start(context.Background(), ffmpeg.Worker{SessionID: "session", GroupID: "group", Owner: newAcceptanceOwner(), Command: acceptanceChildCommand(t, "early", seen), Input: acceptanceBlockInput, Output: acceptanceOutput(nil, nil)}); err != nil {
		t.Fatal(err)
	}
	r := acceptanceWait(t, s)
	if r.Attempts != 3 || r.Err == nil {
		t.Fatalf("нарушен предел рестартов: %+v", r)
	}
	close(seen)
	paths := make(map[string]bool)
	for a := range seen {
		if paths[a.InputPath] {
			t.Fatal("рестарт переиспользовал namespace")
		}
		paths[a.InputPath] = true
		if _, err := os.Stat(filepath.Dir(a.InputPath)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("namespace попытки %d остался: %v", a.ID, err)
		}
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatalf("остатки после join: %v, %v", entries, err)
	}
}

func TestAcceptanceSupervisorStopWaitsForRegisteredBuilder(t *testing.T) {
	s, base := acceptanceSupervisor(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	owner := newAcceptanceOwner()
	if err := s.Start(context.Background(), ffmpeg.Worker{SessionID: "session", GroupID: "group", Owner: owner, Command: func(context.Context, ffmpeg.Attempt) (ffmpeg.Command, error) {
		close(entered)
		<-release
		return ffmpeg.Command{Binary: "/does-not-exist"}, nil
	}, Input: acceptanceBlockInput, Output: acceptanceOutput(nil, nil)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("builder не начался")
	}
	owner.revoke()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Stop(ctx); !errors.Is(err, ffmpeg.ErrCleanupPending) {
		t.Fatal("заблокированный зарегистрированный запуск объявлен очищенным:", err)
	}
	select {
	case <-s.Done():
		t.Fatal("Done закрылся до завершения builder")
	default:
	}
	once.Do(func() { close(release) })
	r := acceptanceWait(t, s)
	if !errors.Is(r.Err, ffmpeg.ErrOwnerRevoked) {
		t.Fatal("потеряна причина отзыва:", r.Err)
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatalf("UDS не очищены: %v %v", entries, err)
	}
}

func TestAcceptanceSupervisorDuplicatePacketsCannotFeedWatchdog(t *testing.T) {
	s, _ := acceptanceSupervisor(t, nil)
	repeated := make(chan bool, 64)
	if err := s.Start(context.Background(), ffmpeg.Worker{SessionID: "session", GroupID: "group", Owner: newAcceptanceOwner(), Command: acceptanceChildCommand(t, "duplicates", nil), Input: acceptanceBlockInput, Output: acceptanceOutput(nil, repeated)}); err != nil {
		t.Fatal(err)
	}
	r := acceptanceWait(t, s)
	if !errors.Is(r.Err, ffmpeg.ErrProgressTimeout) {
		t.Fatalf("дубликаты скрыли отсутствие продвижения: %+v", r)
	}
	close(repeated)
	if len(repeated) == 0 {
		t.Fatal("проверка не получила дубликаты")
	}
	for accepted := range repeated {
		if accepted {
			t.Fatal("дубликат принят как продвижение")
		}
	}
}

func TestAcceptanceSupervisorUnreadDiagnosticsCannotBlockMedia(t *testing.T) {
	s, _ := acceptanceSupervisor(t, func(c *ffmpeg.Config) { c.DiagnosticEvents = 2; c.DiagnosticBytes = 8192 })
	ready := make(chan struct{})
	if err := s.Start(context.Background(), ffmpeg.Worker{SessionID: "session", GroupID: "group", Owner: newAcceptanceOwner(), Command: acceptanceChildCommand(t, "diagnostics", nil), Input: acceptanceBlockInput, Output: acceptanceOutput(ready, nil)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("нечитанная диагностика заблокировала медиа")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	acceptanceWait(t, s)
	if s.DroppedDiagnostics() == 0 {
		t.Fatal("переполнение очереди не отражено счётчиком")
	}
}

func TestAcceptanceSupervisorDisconnectIsFailureWithCleanJoin(t *testing.T) {
	s, _ := acceptanceSupervisor(t, nil)
	if err := s.Start(context.Background(), ffmpeg.Worker{SessionID: "session", GroupID: "group", Owner: newAcceptanceOwner(), Command: acceptanceChildCommand(t, "disconnect", nil), Input: acceptanceBlockInput, Output: acceptanceOutput(nil, nil)}); err != nil {
		t.Fatal(err)
	}
	r := acceptanceWait(t, s)
	if r.Err == nil {
		t.Fatal("исчезновение UDS объявлено успешным worker")
	}
}

func TestAcceptanceSupervisorDiagnosticsPreserveBytesAndLiteralArgs(t *testing.T) {
	s, _ := acceptanceSupervisor(t, nil)
	ready := make(chan struct{})
	build := acceptanceChildCommand(t, "diagnostics", nil)
	var wantArgs []string
	command := func(ctx context.Context, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		cmd, err := build(ctx, a)
		cmd.Args = append(cmd.Args, "synthetic-token=секрет; $(literal) `literal`\nnext")
		wantArgs = append([]string{cmd.Binary}, cmd.Args...)
		return cmd, err
	}
	if err := s.Start(context.Background(), ffmpeg.Worker{SessionID: "session", GroupID: "group", Owner: newAcceptanceOwner(), Command: command, Input: acceptanceBlockInput, Output: acceptanceOutput(ready, nil)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("процесс не передал проверенный пакет")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	acceptanceWait(t, s)
	var stderr []byte
	var sequence uint64
	commandSeen := false
	for {
		d, err := s.NextDiagnostic(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if d.Attempt != 1 {
			t.Fatal("диагностика потеряла идентичность попытки")
		}
		switch d.Kind {
		case "command":
			commandSeen = true
			if !reflect.DeepEqual(d.Args, wantArgs) {
				t.Fatalf("аргументы изменены: %q", d.Args)
			}
		case "stderr":
			sequence++
			if d.Sequence != sequence || len(d.Data) > 64 {
				t.Fatalf("нарушена последовательность ограниченных фрагментов: %+v", d)
			}
			stderr = append(stderr, d.Data...)
		}
	}
	want := bytes.Repeat([]byte{0xff, 0, 0xc3, 0xa9, '\n'}, 8192)
	if !commandSeen || !bytes.Equal(stderr, want) || s.DroppedDiagnostics() != 0 {
		t.Fatalf("полные значения потеряны: command=%v bytes=%d want=%d dropped=%d", commandSeen, len(stderr), len(want), s.DroppedDiagnostics())
	}
}

func TestAcceptanceNamespaceRejectsEscapeAndPreservesReplacedEndpoint(t *testing.T) {
	parent := os.TempDir()
	if runtime.GOOS == "darwin" {
		parent = "/tmp"
	}
	base, err := os.MkdirTemp(parent, "gn-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	for _, id := range []string{"../escape", "a/b", ".", "..", ""} {
		if n, err := ffmpeg.NewNamespace(base, id); err == nil {
			_ = n.Close()
			t.Errorf("небезопасный сегмент принят: %q", id)
		}
	}
	n, err := ffmpeg.NewNamespace(base, "session")
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(n.InputPath); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("чужой ресурс")
	if err := os.WriteFile(n.InputPath, foreign, 0600); err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); !errors.Is(err, ffmpeg.ErrResourceOwnership) {
		t.Fatal("подмена endpoint не обнаружена:", err)
	}
	got, err := os.ReadFile(n.InputPath)
	if err != nil || !bytes.Equal(got, foreign) {
		t.Fatal("cleanup удалил или изменил чужой ресурс:", err)
	}
}
