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

// Проверяет дополнительную сверку типа объекта при совпавших Dev/Ino: обычный файл на месте сокета
// сохраняется с ErrResourceOwnership.
func TestRemoveOwnedRejectsReplacedSocketWithReusedIdentity(t *testing.T) {
	// parent задаёт короткий временный корень, учитывая ограничение длины UDS на Darwin.
	parent := os.TempDir()
	if runtime.GOOS == "darwin" {
		parent = "/tmp"
	}
	// dir содержит изолированный каталог проверки подмены сокета; err сохраняет результаты создания
	// и проверки файлов.
	dir, err := os.MkdirTemp(parent, "ff-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Удаляет принадлежащий тесту временный каталог сокета и сообщает ошибку через t.Error.

		// err сообщает об отказе удаления временного каталога при очистке теста.
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	// path задаёт путь сокета, который тест заменяет обычным файлом.
	path := filepath.Join(dir, "e.sock")
	// listener содержит исходный Unix-слушатель; original сохраняет файловую идентичность исходного
	// сокета.
	listener, original, err := listenOwned(path)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе закрытия исходного слушателя перед подменой.
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе удаления исходного имени сокета перед созданием чужого файла.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// foreign содержит контрольные байты чужого файла, которые очистка должна сохранить.
	foreign := []byte("чужой файл")
	// err сообщает об отказе создания чужого обычного файла на месте сокета.
	if err := os.WriteFile(path, foreign, 0600); err != nil {
		t.Fatal(err)
	}
	// current содержит метаданные обычного файла, заменившего сокет.
	current, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	// originalStat содержит системные метаданные исходного сокета; ok показывает доступность
	// ожидаемого syscall.Stat_t.
	originalStat, ok := original.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("неожиданный тип метаданных сокета: %T", original.Sys())
	}
	// currentStat содержит системные метаданные заменившего файла для моделирования повторного
	// Dev/Ino.
	currentStat, ok := current.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("неожиданный тип метаданных файла: %T", current.Sys())
	}
	originalStat.Dev = currentStat.Dev
	originalStat.Ino = currentStat.Ino
	if !os.SameFile(original, current) || original.Mode().Type() == current.Mode().Type() {
		t.Fatal("предусловия повторного inode не выполнены")
	}
	// err содержит ожидаемый отказ удалить чужой файл при совпавших Dev/Ino и различающемся типе.
	if err := removeOwned(path, original); !errors.Is(err, ErrResourceOwnership) {
		t.Fatal("заменённый сокет принят как собственный ресурс:", err)
	}
	// got содержит байты сохранившегося чужого файла для проверки отсутствия удаления или изменений.
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(foreign) {
		t.Fatal("очистка удалила или изменила чужой файл:", err)
	}
}
