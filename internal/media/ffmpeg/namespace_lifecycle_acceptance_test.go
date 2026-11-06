package ffmpeg_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

func namespaceLifecycleBase(t *testing.T) string {
	t.Helper()
	parent := os.TempDir()
	if runtime.GOOS == "darwin" {
		parent = "/tmp"
	}
	base, err := os.MkdirTemp(parent, "gnl-")
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

func namespaceLifecycleNew(t *testing.T, base string) *ffmpeg.Namespace {
	t.Helper()
	n, err := ffmpeg.NewNamespace(base, "session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })
	return n
}

func namespaceLifecycleEmpty(t *testing.T, base string) {
	t.Helper()
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatalf("после последнего Close остались ресурсы: %v, %v", entries, err)
	}
}

func namespaceLifecycleSockets(t *testing.T, n *ffmpeg.Namespace) {
	t.Helper()
	for _, path := range []string{n.InputPath, n.OutputPath} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			t.Fatalf("активный Namespace потерял endpoint %s: %v", path, err)
		}
	}
}

func TestNamespaceSharedParentLivesUntilLastClose(t *testing.T) {
	for _, creatorFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "creator_first", false: "creator_last"}[creatorFirst], func(t *testing.T) {
			base := namespaceLifecycleBase(t)
			a := namespaceLifecycleNew(t, base)
			b := namespaceLifecycleNew(t, base)
			parent := filepath.Dir(filepath.Dir(a.InputPath))
			original, err := os.Lstat(parent)
			if err != nil {
				t.Fatal(err)
			}
			first, last := a, b
			if !creatorFirst {
				first, last = b, a
			}
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			if err := first.Close(); err != nil {
				t.Fatal("повторный Close изменил исход", err)
			}
			current, err := os.Lstat(parent)
			if err != nil || !os.SameFile(original, current) {
				t.Fatal("первый Close удалил или заменил общий parent", err)
			}
			namespaceLifecycleSockets(t, last)
			c := namespaceLifecycleNew(t, base)
			if err := last.Close(); err != nil {
				t.Fatal(err)
			}
			namespaceLifecycleSockets(t, c)
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			namespaceLifecycleEmpty(t, base)
			if err := last.Close(); err != nil {
				t.Fatal(err)
			}
			d := namespaceLifecycleNew(t, base)
			if err := d.Close(); err != nil {
				t.Fatal("повторное создание после последнего Close", err)
			}
			namespaceLifecycleEmpty(t, base)
		})
	}
}

func TestNamespaceCanonicalBaseSharesParentLifetime(t *testing.T) {
	base, aliasRoot := namespaceLifecycleBase(t), namespaceLifecycleBase(t)
	alias := filepath.Join(aliasRoot, "alias")
	if err := os.Symlink(base, alias); err != nil {
		t.Fatal(err)
	}
	a := namespaceLifecycleNew(t, base)
	b := namespaceLifecycleNew(t, alias)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	namespaceLifecycleSockets(t, b)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	namespaceLifecycleEmpty(t, base)
}

func TestNamespaceCaseAliasesRespectPhysicalParentLifetime(t *testing.T) {
	for _, aliasKind := range []string{"session_case", "base_case"} {
		for _, creatorFirst := range []bool{true, false} {
			order := map[bool]string{true: "creator_first", false: "creator_last"}[creatorFirst]
			t.Run(aliasKind+"/"+order, func(t *testing.T) {
				root := namespaceLifecycleBase(t)
				baseA, baseB := root, root
				sessionA, sessionB := "Session", "session"
				if aliasKind == "base_case" {
					baseA, baseB = filepath.Join(root, "Base"), filepath.Join(root, "base")
					sessionA, sessionB = "session", "session"
				}
				create := func(base, session string) *ffmpeg.Namespace {
					t.Helper()
					n, err := ffmpeg.NewNamespace(base, session)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = n.Close() })
					return n
				}
				a, b := create(baseA, sessionA), create(baseB, sessionB)
				parentA, parentB := filepath.Dir(filepath.Dir(a.InputPath)), filepath.Dir(filepath.Dir(b.InputPath))
				infoA, err := os.Lstat(parentA)
				if err != nil {
					t.Fatal(err)
				}
				infoB, err := os.Lstat(parentB)
				if err != nil {
					t.Fatal(err)
				}
				shared := os.SameFile(infoA, infoB)
				if shared {
					t.Log("разные регистры пути обозначают один физический parent")
				} else {
					t.Log("файловая система различает регистр: проверяются два независимых parent")
				}
				first, last, firstParent, lastParent, lastInfo := a, b, parentA, parentB, infoB
				if !creatorFirst {
					first, last, firstParent, lastParent, lastInfo = b, a, parentB, parentA, infoA
				}
				if err := first.Close(); err != nil {
					t.Fatal(err)
				}
				current, err := os.Lstat(lastParent)
				if err != nil || !os.SameFile(lastInfo, current) {
					t.Fatal("Close первого пользователя изменил живой parent", err)
				}
				if !shared {
					if _, err := os.Lstat(firstParent); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("последний пользователь независимого parent не очистил его", err)
					}
				}
				namespaceLifecycleSockets(t, last)
				if err := last.Close(); err != nil {
					t.Fatal(err)
				}
				namespaceLifecycleEmpty(t, baseA)
				namespaceLifecycleEmpty(t, baseB)
			})
		}
	}
}

func TestNamespacePreservesPreexistingParent(t *testing.T) {
	for _, foreignFile := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "foreign_child"}[foreignFile], func(t *testing.T) {
			base := namespaceLifecycleBase(t)
			parent := filepath.Join(base, "session")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			original, err := os.Lstat(parent)
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(parent, "foreign")
			if foreignFile {
				if err := os.WriteFile(marker, []byte("чужое содержимое"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			a, b := namespaceLifecycleNew(t, base), namespaceLifecycleNew(t, base)
			for _, n := range []*ffmpeg.Namespace{a, b} {
				if err := n.Close(); err != nil {
					t.Fatal(err)
				}
			}
			current, err := os.Lstat(parent)
			if err != nil || !os.SameFile(original, current) {
				t.Fatal("чужой parent удалён или заменён", err)
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != map[bool]int{false: 0, true: 1}[foreignFile] {
				t.Fatal("в чужом parent изменён набор ресурсов", entries, err)
			}
			if foreignFile {
				data, err := os.ReadFile(marker)
				if err != nil || string(data) != "чужое содержимое" {
					t.Fatal("изменён чужой файл", err)
				}
			}
		})
	}
}

func TestNamespaceRejectsReplacedParentAndReleasesFailedOwners(t *testing.T) {
	base := namespaceLifecycleBase(t)
	a, b := namespaceLifecycleNew(t, base), namespaceLifecycleNew(t, base)
	parent := filepath.Join(base, "session")
	if err := os.Rename(parent, filepath.Join(base, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(parent, "foreign")
	if err := os.WriteFile(marker, []byte("новый владелец"), 0600); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.Lstat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if unexpected, err := ffmpeg.NewNamespace(base, "session"); !errors.Is(err, ffmpeg.ErrResourceOwnership) {
		if unexpected != nil {
			_ = unexpected.Close()
		}
		t.Fatal("подменённый живой parent принят", err)
	}
	for _, n := range []*ffmpeg.Namespace{a, b} {
		first := n.Close()
		if !errors.Is(first, ffmpeg.ErrResourceOwnership) {
			t.Fatal("Close не обнаружил подмену parent", first)
		}
		if again := n.Close(); again == nil || again.Error() != first.Error() || !errors.Is(again, ffmpeg.ErrResourceOwnership) {
			t.Fatal("повторный Close изменил сохранённый исход", again, first)
		}
	}
	// После завершения прежних владельцев замена является обычным чужим parent.
	c := namespaceLifecycleNew(t, base)
	if err := c.Close(); err != nil {
		t.Fatal("ошибочный Close сохранил прежнее удержание", err)
	}
	current, err := os.Lstat(parent)
	if err != nil || !os.SameFile(replacement, current) {
		t.Fatal("Close изменил чужой parent", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "новый владелец" {
		t.Fatal("Close изменил чужое содержимое", err)
	}
}

func TestNamespaceForeignEndpointDoesNotReleaseSibling(t *testing.T) {
	base := namespaceLifecycleBase(t)
	a, b := namespaceLifecycleNew(t, base), namespaceLifecycleNew(t, base)
	if err := a.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(a.InputPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.InputPath, []byte("чужой endpoint"), 0600); err != nil {
		t.Fatal(err)
	}
	first := a.Close()
	if !errors.Is(first, ffmpeg.ErrResourceOwnership) {
		t.Fatal("подмена endpoint не обнаружена", first)
	}
	if again := a.Close(); again == nil || again.Error() != first.Error() || !errors.Is(again, ffmpeg.ErrResourceOwnership) {
		t.Fatal("повторный Close изменил сохранённый исход", again, first)
	}
	namespaceLifecycleSockets(t, b)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	c := namespaceLifecycleNew(t, base)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(a.InputPath)
	if err != nil || string(data) != "чужой endpoint" {
		t.Fatal("cleanup удалил или изменил чужой endpoint", err)
	}
}

func TestNamespacePartialStartupReleasesParent(t *testing.T) {
	base := filepath.Join(namespaceLifecycleBase(t), strings.Repeat("long", 32))
	if n, err := ffmpeg.NewNamespace(base, "session"); !errors.Is(err, ffmpeg.ErrInvalidSupervisor) {
		if n != nil {
			_ = n.Close()
		}
		t.Fatal("ожидался отказ длины UDS после создания каталогов", err)
	}
	namespaceLifecycleEmpty(t, base)
	parent := filepath.Join(base, "session")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	original, err := os.Lstat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := ffmpeg.NewNamespace(base, "session"); !errors.Is(err, ffmpeg.ErrInvalidSupervisor) || errors.Is(err, ffmpeg.ErrResourceOwnership) {
		if n != nil {
			_ = n.Close()
		}
		t.Fatal("неудачный startup удержал прежнюю identity", err)
	}
	current, err := os.Lstat(parent)
	if err != nil || !os.SameFile(original, current) {
		t.Fatal("неудачный startup удалил чужой parent", err)
	}
	namespaceLifecycleEmpty(t, parent)
}

func TestNamespaceConcurrentCreateAndLastClose(t *testing.T) {
	base := namespaceLifecycleBase(t)
	a := namespaceLifecycleNew(t, base)
	start := make(chan struct{})
	closed := make(chan error, 1)
	type result struct {
		n   *ffmpeg.Namespace
		err error
	}
	created := make(chan result, 1)
	completed := make(chan struct{}, 2)
	go func() { defer func() { completed <- struct{}{} }(); <-start; closed <- a.Close() }()
	go func() {
		defer func() { completed <- struct{}{} }()
		<-start
		n, err := ffmpeg.NewNamespace(base, "session")
		created <- result{n, err}
	}()
	close(start)
	joined := false
	join := func() bool {
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		for range 2 {
			select {
			case <-completed:
			case <-timer.C:
				t.Error("конкурентные операции не присоединились")
				return false
			}
		}
		return true
	}
	t.Cleanup(func() {
		if !joined && !join() {
			return
		}
		select {
		case r := <-created:
			if r.n != nil {
				_ = r.n.Close()
			}
		default:
		}
	})
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("последний Close не завершился")
	}
	var r result
	select {
	case r = <-created:
	case <-time.After(3 * time.Second):
		t.Fatal("конкурентное создание не завершилось")
	}
	if r.err != nil {
		t.Fatal("создание потеряло parent при последнем Close", r.err)
	}
	t.Cleanup(func() { _ = r.n.Close() })
	namespaceLifecycleSockets(t, r.n)
	if err := r.n.Close(); err != nil {
		t.Fatal(err)
	}
	joined = join()
	if joined {
		namespaceLifecycleEmpty(t, base)
	}
}

func TestSupervisorsShareSessionParentAcrossRealAttempts(t *testing.T) {
	a, base := acceptanceSupervisor(t, func(c *ffmpeg.Config) {
		c.MaxRestarts = 1
		c.StartupTimeout = 5 * time.Second
	})
	b, _ := acceptanceSupervisor(t, func(c *ffmpeg.Config) {
		c.BaseDir = base
		c.MaxRestarts = 1
		c.StartupTimeout = 5 * time.Second
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	marker := filepath.Join(t.TempDir(), "executed")
	started := make(chan string, 2)
	releases := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	var releaseOnce [2]sync.Once
	release := func(i int) { releaseOnce[i].Do(func() { close(releases[i]) }) }
	t.Cleanup(func() {
		release(0)
		release(1)
		cancel()
		for _, supervisor := range []*ffmpeg.Supervisor{a, b} {
			stopCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
			if err := supervisor.Stop(stopCtx); err != nil {
				t.Error("Supervisor не завершился при очистке", err)
			}
			stop()
		}
	})
	start := func(s *ffmpeg.Supervisor, index int, label string) {
		t.Helper()
		worker := ffmpeg.Worker{SessionID: "shared-session", GroupID: label, Owner: newAcceptanceOwner(), Input: acceptanceBlockInput,
			Output: func(context.Context, net.Conn, ffmpeg.Progress) error { return nil },
			Command: func(commandCtx context.Context, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
				if attempt.ID == 1 {
					started <- label
					select {
					case <-releases[index]:
					case <-commandCtx.Done():
						return ffmpeg.Command{}, commandCtx.Err()
					}
				}
				return ffmpeg.Command{Binary: "/bin/sh", Args: []string{"-c", "printf '%s\\n' \"$1\" >> \"$2\"; exit 23", "namespace-child", fmt.Sprintf("%s/%d", label, attempt.ID), marker}}, nil
			}}
		if err := s.Start(ctx, worker); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-started:
			if got != label {
				t.Fatal("неверный владелец барьера", got)
			}
		case <-ctx.Done():
			t.Fatal("Supervisor не достиг первой команды", ctx.Err())
		}
	}
	start(a, 0, "creator")
	start(b, 1, "sibling")
	release(0)
	if result := acceptanceWait(t, a); result.Attempts != 2 || !errors.Is(result.Err, ffmpeg.ErrPrematureExit) {
		t.Fatal("создатель не завершил обе реальные попытки", result)
	}
	release(1)
	if result := acceptanceWait(t, b); result.Attempts != 2 || !errors.Is(result.Err, ffmpeg.ErrPrematureExit) {
		t.Fatal("второй Supervisor не завершил обе реальные попытки", result)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"creator/1", "creator/2", "sibling/1", "sibling/2"}
	if got := strings.Fields(string(data)); !reflect.DeepEqual(got, want) {
		t.Fatal("реальные команды пропущены или повторены", got, want)
	}
	namespaceLifecycleEmpty(t, base)
}
