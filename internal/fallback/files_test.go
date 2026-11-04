package fallback

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func selfFileStore(t *testing.T) (*FileStore, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, dir
}

func TestOwnedPartialCleanupRequiresActualIdentityAndKeepsUnknownObjects(t *testing.T) {
	s, dir := selfFileStore(t)
	id := core.ID("owned-generation")
	if err := s.CreateGeneration(id); err != nil {
		t.Fatal(err)
	}
	base := core.GenerationDirectory(id)
	owned := core.OwnedFile{AssetID: "selected", GenerationID: id, ManagedPath: base + "/original.stage", Kind: core.OwnedStaging}
	f, err := s.Create(owned.ManagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("partial"); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.RemoveOwnedContext(context.Background(), owned); err != nil {
		t.Fatal(err)
	}
	if err = s.RemoveOwnedContext(context.Background(), owned); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, owned.ManagedPath), []byte("unknown"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.RemoveOwnedContext(context.Background(), owned); !errors.Is(err, core.ErrCorrupt) {
		t.Fatal(err)
	}
	directory := core.OwnedFile{AssetID: "selected", GenerationID: id, ManagedPath: base, Kind: core.OwnedDirectory}
	if err = s.RemoveOwned(directory); err == nil {
		t.Fatal("неизвестный вложенный файл удалён рекурсивно")
	}
	data, err := os.ReadFile(filepath.Join(dir, owned.ManagedPath))
	if err != nil || string(data) != "unknown" {
		t.Fatal(string(data), err)
	}
}

func TestSealedCleanupVerifiesPersistedIntegrityAfterReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := core.ID("sealed-generation")
	if err = s.CreateGeneration(id); err != nil {
		t.Fatal(err)
	}
	name := core.GenerationDirectory(id) + "/original"
	f, err := s.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("completed-original"); err != nil {
		t.Fatal(err)
	}
	size, hash, err := InspectFile(context.Background(), f, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	owned := core.OwnedFile{AssetID: "selected", GenerationID: id, ManagedPath: name, Kind: core.OwnedOriginal, ContentHash: hash, SizeBytes: size}
	if err = s.RemoveOwnedContext(context.Background(), owned); err != nil {
		t.Fatal(err)
	}
	if err = s.RemoveOwnedContext(context.Background(), owned); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, name), []byte("mismatched-original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.RemoveOwnedContext(context.Background(), owned); err == nil {
		t.Fatal("изменённый completed original удалён")
	}
	owned.ManagedPath = core.GenerationDirectory(id)
	owned.Kind = core.OwnedDirectory
	owned.ContentHash = ""
	owned.SizeBytes = 0
	if err = s.RemoveOwned(owned); !errors.Is(err, core.ErrCorrupt) {
		t.Fatal(err)
	}
}

func TestFileOperationsRejectInvalidMissingAndChangedDescriptors(t *testing.T) {
	s, _ := selfFileStore(t)
	for _, name := range []string{"../escape", "/escape", "", "."} {
		if f, err := s.Create(name); err == nil {
			_ = f.Close()
			t.Fatal(name)
		}
		if f, err := s.OpenRegular(name); err == nil {
			_ = f.Close()
			t.Fatal(name)
		}
	}
	if err := s.CreateGeneration("../escape"); err == nil {
		t.Fatal("создан каталог вне поколения")
	}
	if err := s.PublishRename("same", "same"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := s.PublishRename("missing", "final"); err == nil {
		t.Fatal("отсутствующий staging принят")
	}
	for _, hash := range []string{"", string(make([]byte, 64)), "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF"} {
		if f, err := s.OpenExact(context.Background(), "missing", 1, hash); err == nil {
			_ = f.Close()
			t.Fatal("некорректный hash принят")
		}
	}
	f, err := s.Create("file")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = InspectFile(context.Background(), f, 8); !errors.Is(err, core.ErrCorrupt) {
		t.Fatal(err)
	}
	if _, err = f.WriteString("123456789"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = InspectFile(context.Background(), f, 8); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err = InspectFile(context.Background(), f, 16); !errors.Is(err, os.ErrClosed) {
		t.Fatal(err)
	}
	if _, err = CopyBounded(context.Background(), io.Discard, nil, 0); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
