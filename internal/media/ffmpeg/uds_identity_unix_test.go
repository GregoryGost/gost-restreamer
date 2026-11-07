//go:build unix

package ffmpeg

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestRemoveOwnedRejectsReplacedSocketWithReusedIdentity(t *testing.T) {
	parent := os.TempDir()
	if runtime.GOOS == "darwin" {
		parent = "/tmp"
	}
	dir, err := os.MkdirTemp(parent, "ff-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	path := filepath.Join(dir, "e.sock")
	listener, original, err := listenOwned(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("чужой файл")
	if err := os.WriteFile(path, foreign, 0600); err != nil {
		t.Fatal(err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	originalStat, ok := original.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("неожиданный тип метаданных сокета: %T", original.Sys())
	}
	currentStat, ok := current.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("неожиданный тип метаданных файла: %T", current.Sys())
	}
	originalStat.Dev = currentStat.Dev
	originalStat.Ino = currentStat.Ino
	if !os.SameFile(original, current) || original.Mode().Type() == current.Mode().Type() {
		t.Fatal("предусловия повторного inode не выполнены")
	}
	if err := removeOwned(path, original); !errors.Is(err, ErrResourceOwnership) {
		t.Fatal("заменённый сокет принят как собственный ресурс:", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(foreign) {
		t.Fatal("очистка удалила или изменила чужой файл:", err)
	}
}
