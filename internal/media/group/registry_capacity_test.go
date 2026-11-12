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

// Проверяет удержание предела групп до физического завершения и при ошибке очистки; чистое завершение
// освобождает место для другого ключа без отзыва Session.
func TestRegistryRetiringGroupCapacity(t *testing.T) {
	// mode выбирает чистое завершение, незавершённое открытие файла либо ошибку очистки дескриптора.
	for _, mode := range []string{"clean", "pending-join", "cleanup-error"} {
		t.Run(mode, func(t *testing.T) {
			// Проверяет удержание лимита закрывающейся группой до завершения и при ошибке очистки, а после чистого
			// завершения допускает другой ключ.

			// f предоставляет сохранённые выходы разных групп и настоящий доменный допуск
			// Session.
			f := newRuntimeAcceptanceFixture(t)
			// ctx задаёт общий контекст операций реестра и ссылок.
			ctx := context.Background()
			// first содержит первый закреплённый снимок группы, занимающей единственное
			// место реестра.
			first := f.outputs[0].Snapshot
			// second сохраняет снимок другой группы для проверки освобождения общего лимита.
			var second core.OutputSnapshot
			// output содержит очередной сохранённый выход при поиске другой группы.
			for _, output := range f.outputs {
				if output.Snapshot.Group.ID != first.Group.ID {
					second = output.Snapshot
					break
				}
			}
			if second.Validate() != nil {
				t.Fatal("нет второго закреплённого снимка другой группы")
			}
			// entered сообщает о начале открытия файла первой группы.
			entered := make(chan struct{})
			// release разрешает задержанному открытию файла вернуться.
			release := make(chan struct{})
			// releaseOnce исключает повторное закрытие барьера открытия файла.
			var releaseOnce sync.Once
			// unblock единожды освобождает задержанное открытие для обычного и резервного
			// завершения.
			unblock := func() {
				// Освобождает задержанное открытие файла через sync.Once.

				releaseOnce.Do(func() {
					// Закрывает канал release при первом освобождении открытия.
					close(release)
				})
			}
			defer unblock()
			// c содержит настройки исполнения с управляемым файловым портом выбранного
			// сценария.
			c := f.config
			// closedPath задаёт временный файл, возвращаемый уже закрытым для ошибки
			// очистки.
			closedPath := filepath.Join(t.TempDir(), "closed.mp4")
			c.OpenFile = func(_ context.Context, manifest core.ArtifactManifest) (*os.File, error) {
				// Открывает только первый манифест; выбранный режим задерживает возврат либо отдаёт уже закрытый файл
				// для ошибки очистки.

				if manifest.VariantID != first.Artifact.VariantID {
					return nil, os.ErrNotExist
				}
				close(entered)
				if mode == "pending-join" {
					<-release
				}
				if mode == "cleanup-error" {
					// file содержит намеренно закрываемый дескриптор; err сохраняет
					// ошибки его создания и закрытия.
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
			// r содержит реестр с одним местом группы; err сохраняет результаты создания,
			// получения ссылок и их закрытия.
			r, err := NewRegistry(ctx, RegistryConfig{Core: f.service, Parent: f.parent, Observer: f.observer, Runtime: c, MaxGroups: 1, MaxReferences: 2})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Освобождает открытие и закрывает Registry, допуская только ожидаемую os.ErrClosed в сценарии
				// cleanup-error.

				unblock()
				// err содержит результат резервного Close реестра с допустимой ожидаемой
				// ошибкой os.ErrClosed.
				if err := r.Close(ctx); err != nil && (mode != "cleanup-error" || !errors.Is(err, os.ErrClosed)) {
					t.Error(err)
				}
			})
			// live содержит общий входной Router проверяемых групп.
			live, err := media.NewRouter(c.Switcher.Router)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				// Закрывает LIVE Router после проверки лимита групп, игнорируя ошибку.
				_ = live.Close()
			}()
			// a содержит ссылку на первую группу, удерживающую ограниченное место реестра.
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
				// canceled передаёт уже отменённый контекст в CloseContext; cancel
				// инициирует отмену перед проверкой незавершённой группы.
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
			// b содержит ссылку на другую группу после проверки завершения и освобождения
			// места.
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
