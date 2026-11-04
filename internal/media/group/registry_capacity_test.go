package group

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

func TestRegistryRetiringGroupCapacity(t *testing.T) {
	for _, mode := range []string{"clean", "pending-join", "cleanup-error"} {
		t.Run(mode, func(t *testing.T) {
			f := newRuntimeAcceptanceFixture(t)
			ctx := context.Background()
			first := f.outputs[0].Snapshot
			var second core.OutputSnapshot
			for _, output := range f.outputs {
				if output.Snapshot.Group.ID != first.Group.ID {
					second = output.Snapshot
					break
				}
			}
			if second.Validate() != nil {
				t.Fatal("нет второго закреплённого снимка другой группы")
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			c := f.config
			closedPath := filepath.Join(t.TempDir(), "closed.mp4")
			c.OpenFile = func(_ context.Context, manifest core.ArtifactManifest) (*os.File, error) {
				if manifest.VariantID != first.Artifact.VariantID {
					return nil, os.ErrNotExist
				}
				close(entered)
				if mode == "pending-join" {
					<-release
				}
				if mode == "cleanup-error" {
					file, err := os.Create(closedPath)
					if err != nil {
						return nil, err
					}
					if err = file.Close(); err != nil {
						return nil, err
					}
					return file, nil
				}
				return nil, os.ErrNotExist
			}
			r, err := NewRegistry(ctx, RegistryConfig{Core: f.service, Parent: f.parent, Observer: f.observer, Runtime: c, MaxGroups: 1, MaxReferences: 2})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				unblock()
				if err := r.Close(ctx); err != nil && (mode != "cleanup-error" || !errors.Is(err, os.ErrClosed)) {
					t.Error(err)
				}
			})
			live, err := media.NewRouter(c.Switcher.Router)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = live.Close() }()
			a, err := r.Acquire(ctx, first, live)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("проигрыватель файла не начал попытку")
			}
			if _, err = r.Acquire(ctx, second, live); !errors.Is(err, media.ErrRouterBudget) {
				t.Fatal("активная группа не удерживает лимит", err)
			}
			if mode == "pending-join" {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				if err = a.CloseContext(canceled); !errors.Is(err, ffmpeg.ErrCleanupPending) || !errors.Is(err, context.Canceled) {
					t.Fatal("незавершённое ожидание группы не видно вызывающему", err)
				}
				select {
				case <-a.Group().Done():
					t.Fatal("Done закрыт до возврата функции открытия файла")
				default:
				}
				if _, err = r.Acquire(ctx, second, live); !errors.Is(err, media.ErrRouterBudget) {
					t.Fatal("незавершённая группа освободила лимит", err)
				}
				if _, err = r.Acquire(ctx, first, live); !errors.Is(err, core.ErrNotReady) {
					t.Fatal("создана параллельная замена незавершённой группы", err)
				}
				unblock()
			}
			err = a.CloseContext(ctx)
			if mode == "cleanup-error" {
				if !errors.Is(err, os.ErrClosed) {
					t.Fatal("потеряна ошибка физической очистки FD", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			select {
			case <-a.Group().Done():
			default:
				t.Fatal("последняя ссылка не дождалась группы")
			}
			if !f.observer.Valid() {
				t.Fatal("закрытие группы отозвало владельца Session")
			}
			b, err := r.Acquire(ctx, second, live)
			if mode == "cleanup-error" {
				if !errors.Is(err, media.ErrRouterBudget) {
					t.Fatal("ошибка очистки освободила лимит", err)
				}
				if _, err = r.Acquire(ctx, first, live); !errors.Is(err, os.ErrClosed) {
					t.Fatal("повторное получение потеряло ошибку очистки", err)
				}
				return
			}
			if err != nil {
				t.Fatal("чисто завершённая группа не освободила лимит для другого ключа", err)
			}
			if b.Group() == a.Group() {
				t.Fatal("другой ключ получил прежнюю группу")
			}
			if err = b.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
