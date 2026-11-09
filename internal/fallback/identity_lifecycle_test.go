package fallback

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

func identityOwnedFile(generation, name string, kind core.OwnedFileKind) core.OwnedFile {
	managedPath := core.GenerationDirectory(core.ID(generation))
	if name != "" {
		managedPath += "/" + name
	}
	return core.OwnedFile{ID: core.ID(generation + "-" + name), AssetID: "asset", GenerationID: core.ID(generation), Kind: kind, ManagedPath: managedPath}
}

func identityWriteOwned(t *testing.T, files *FileStore, owned core.OwnedFile) (uint64, string) {
	t.Helper()
	f, err := files.CreateOwned(owned)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString("prepared media bytes")
	size, digest, inspectErr := InspectFile(context.Background(), f, 1024)
	if err = errors.Join(writeErr, inspectErr, f.Sync(), f.Close()); err != nil {
		t.Fatal(err)
	}
	return size, digest
}

func identityCount(files *FileStore) int {
	files.mu.Lock()
	defer files.mu.Unlock()
	return len(files.identities)
}

func identityAnchor(directory string, file core.OwnedFile) string {
	return filepath.Join(directory, ".processowner-evidence", "anchors", fmt.Sprintf("%x", sha256.Sum256([]byte(file.ID))))
}

func TestManagedGenerationCleanupReleasesRememberedIdentities(t *testing.T) {
	ctx := context.Background()
	files, directory := selfFileStore(t)
	keep, err := files.Create("preserved")
	if err != nil {
		t.Fatal(err)
	}
	if err = keep.Close(); err != nil {
		t.Fatal(err)
	}
	baseline := identityCount(files)
	preserved := files.remembered("preserved")
	for cycle := range 24 {
		t.Run(fmt.Sprintf("generation-%d", cycle), func(t *testing.T) {
			generation := fmt.Sprintf("generation-%d", cycle)
			dir := identityOwnedFile(generation, "", core.OwnedDirectory)
			if err := files.CreateOwnedGeneration(dir); err != nil {
				t.Fatal(err)
			}
			var retired []core.OwnedFile
			for _, name := range []string{"original", "variant"} {
				staging := identityOwnedFile(generation, name+".stage", core.OwnedStaging)
				kind := core.OwnedOriginal
				if name == "variant" {
					kind = core.OwnedVariant
				}
				final := identityOwnedFile(generation, name, kind)
				if err := files.ReserveOwnedContext(ctx, final); err != nil {
					t.Fatal(err)
				}
				final.SizeBytes, final.ContentHash = identityWriteOwned(t, files, staging)
				if err := files.PublishOwnedRename(staging, final); err != nil {
					t.Fatal(err)
				}
				if files.remembered(staging.ManagedPath) != nil {
					t.Fatal("успешный rename сохранил RAM-запись staging")
				}
				verified, err := files.OpenExact(ctx, final.ManagedPath, final.SizeBytes, final.ContentHash)
				if err != nil {
					t.Fatal(err)
				}
				if err = verified.Close(); err != nil {
					t.Fatal(err)
				}
				retired = append(retired, staging, final)
			}
			retired = append(retired, dir)
			for _, file := range retired {
				if err := files.RemoveOwnedContext(ctx, file); err != nil {
					t.Fatal(file.ManagedPath, err)
				}
				if files.remembered(file.ManagedPath) != nil {
					t.Fatal("успешное удаление сохранило RAM-запись", file.ManagedPath)
				}
				if _, err := os.Lstat(identityAnchor(directory, file)); err != nil {
					t.Fatal("служебная ссылка потеряна до FILE ack", err)
				}
				// Вызывающая сторона подтверждает FILE в SQL до освобождения ссылки.
				if err := files.AcknowledgeOwnedContext(ctx, file); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(identityAnchor(directory, file)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("служебная ссылка осталась после FILE ack", err)
				}
			}
			if got := identityCount(files); got != baseline || files.remembered("preserved") != preserved {
				t.Fatalf("жизненный цикл сохранил чужие или завершённые записи: %d, исходно %d", got, baseline)
			}
		})
	}
}

func TestManagedIdentityCleanupAfterReopen(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	files, err := OpenFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			if err := files.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	dir := identityOwnedFile("reopened", "", core.OwnedDirectory)
	owned := identityOwnedFile("reopened", "original", core.OwnedOriginal)
	if err = files.CreateOwnedGeneration(dir); err != nil {
		t.Fatal(err)
	}
	owned.SizeBytes, owned.ContentHash = identityWriteOwned(t, files, owned)
	if err = files.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	files, err = OpenFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	closed = false
	verified, err := files.OpenExact(ctx, owned.ManagedPath, owned.SizeBytes, owned.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	if err = verified.Close(); err != nil {
		t.Fatal(err)
	}
	if identityCount(files) != 1 {
		t.Fatal("проверенное чтение не учтено")
	}
	for _, file := range []core.OwnedFile{owned, dir} {
		if err = files.RemoveOwned(file); err != nil {
			t.Fatal(err)
		}
		if err = files.AcknowledgeOwned(file); err != nil {
			t.Fatal(err)
		}
	}
	if identityCount(files) != 0 {
		t.Fatal("очистка после повторного открытия сохранила RAM-запись")
	}
}

func TestManagedIdentityFailuresPreserveExplicitCleanup(t *testing.T) {
	for _, failure := range []string{"remove-canceled", "remove-replacement", "rename-conflict", "ack-pending"} {
		t.Run(failure, func(t *testing.T) {
			files, directory := selfFileStore(t)
			dir := identityOwnedFile("pending", "", core.OwnedDirectory)
			staging := identityOwnedFile("pending", "source.stage", core.OwnedStaging)
			final := identityOwnedFile("pending", "original", core.OwnedOriginal)
			if err := files.CreateOwnedGeneration(dir); err != nil {
				t.Fatal(err)
			}
			identityWriteOwned(t, files, staging)
			known := files.remembered(staging.ManagedPath)
			var err error
			switch failure {
			case "remove-canceled":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				err = files.RemoveOwnedContext(ctx, staging)
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case "remove-replacement":
				name := filepath.Join(directory, staging.ManagedPath)
				if err = os.Rename(name, name+".old"); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(name, []byte("replacement"), 0600); err != nil {
					t.Fatal(err)
				}
				if err = files.RemoveOwned(staging); !errors.Is(err, processowner.ErrUnknown) {
					t.Fatal(err)
				}
				if err = os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(name+".old", name); err != nil {
					t.Fatal(err)
				}
			case "rename-conflict":
				if err = os.WriteFile(filepath.Join(directory, final.ManagedPath), []byte("replacement"), 0600); err != nil {
					t.Fatal(err)
				}
				err = files.PublishOwnedRename(staging, final)
				if !errors.Is(err, processowner.ErrUnknown) {
					t.Fatal(err)
				}
			case "ack-pending":
				err = files.AcknowledgeOwned(staging)
				if !errors.Is(err, processowner.ErrPending) {
					t.Fatal(err)
				}
			}
			if files.remembered(staging.ManagedPath) != known {
				t.Fatal("неуспешное действие потеряло RAM-доказательство")
			}
			if _, err = os.Lstat(identityAnchor(directory, staging)); err != nil {
				t.Fatal("неуспешное действие потеряло служебную ссылку", err)
			}
			// Явный прежний порт без FileID по-прежнему использует точную RAM-запись.
			staging.ID = ""
			if err = files.RemoveOwned(staging); err != nil {
				t.Fatal("утрачена возможность явной очистки", err)
			}
		})
	}
}

func TestManagedAcknowledgementReleasesRememberedIdentity(t *testing.T) {
	ctx := context.Background()
	files, directory := selfFileStore(t)
	dir := identityOwnedFile("acknowledged", "", core.OwnedDirectory)
	owned := identityOwnedFile("acknowledged", "source.stage", core.OwnedStaging)
	if err := files.CreateOwnedGeneration(dir); err != nil {
		t.Fatal(err)
	}
	identityWriteOwned(t, files, owned)
	// Общий журнал завершил физическую очистку; FileStore ещё хранит прежнее наблюдение.
	if err := files.FileJournal().Remove(ctx, fileIdentity(owned)); err != nil {
		t.Fatal(err)
	}
	if files.remembered(owned.ManagedPath) == nil {
		t.Fatal("исходное RAM-наблюдение отсутствует")
	}
	if _, err := os.Lstat(identityAnchor(directory, owned)); err != nil {
		t.Fatal("служебная ссылка потеряна до FILE ack", err)
	}
	if err := files.AcknowledgeOwned(owned); err != nil {
		t.Fatal(err)
	}
	if files.remembered(owned.ManagedPath) != nil {
		t.Fatal("FILE ack сохранил завершённое RAM-наблюдение")
	}
	replacement, err := files.Create(owned.ManagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = replacement.Close(); err != nil {
		t.Fatal(err)
	}
	current := files.remembered(owned.ManagedPath)
	if err = files.AcknowledgeOwned(owned); err != nil {
		t.Fatal(err)
	}
	if files.remembered(owned.ManagedPath) != current {
		t.Fatal("повтор FILE ack удалил наблюдение нового объекта")
	}
	owned.ID = ""
	if err = files.RemoveOwned(owned); err != nil {
		t.Fatal("новый объект потерял доказательство явного владения", err)
	}
	if err := files.RemoveOwned(dir); err != nil {
		t.Fatal(err)
	}
	if err := files.AcknowledgeOwned(dir); err != nil {
		t.Fatal(err)
	}
}

func TestRememberedIdentityCleanupKeepsReplacementObservation(t *testing.T) {
	files, directory := selfFileStore(t)
	if err := files.CreateGeneration("replacement"); err != nil {
		t.Fatal(err)
	}
	owned := identityOwnedFile("replacement", "source", core.OwnedStaging)
	owned.ID = ""
	original, err := files.Create(owned.ManagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = original.Close(); err != nil {
		t.Fatal(err)
	}
	previous := files.remembered(owned.ManagedPath)
	if err = os.Rename(filepath.Join(directory, owned.ManagedPath), filepath.Join(directory, owned.ManagedPath+".old")); err != nil {
		t.Fatal(err)
	}
	replacement, err := files.Create(owned.ManagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = replacement.Close(); err != nil {
		t.Fatal(err)
	}
	current := files.remembered(owned.ManagedPath)
	// Завершение прежнего действия не удаляет запись более нового объекта по тому же пути.
	files.forget(owned.ManagedPath, previous)
	if current == previous || files.remembered(owned.ManagedPath) != current {
		t.Fatal("завершение прежнего действия удалило новое наблюдение")
	}
	if err = files.RemoveOwned(owned); err != nil {
		t.Fatal("новый объект потерял доказательство явного владения", err)
	}
}
