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

// Открывает FileStore во временном каталоге и регистрирует его закрытие после теста.
func selfFileStore(t *testing.T) (*FileStore, string) {
	t.Helper()
	// dir содержит временный корень тестового файлового хранилища.
	dir := t.TempDir()
	// s содержит открытое тестовое хранилище. err сообщает об отказе его открытия.
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает тестовый FileStore после сценария и сообщает ошибку закрытия через t.Error.

		// err сообщает об отказе закрытия тестового хранилища.
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, dir
}

// Проверяет повторное удаление известного частичного файла и сохранение подменённого файла и непустого
// каталога без рекурсивной очистки.
func TestOwnedPartialCleanupRequiresActualIdentityAndKeepsUnknownObjects(t *testing.T) {
	// s содержит файловое хранилище. dir содержит его временный корень для создания подменяющего
	// объекта.
	s, dir := selfFileStore(t)
	// id обозначает поколение с проверяемым частичным файлом.
	id := core.ID("owned-generation")
	// err сообщает об отказе создания каталога тестового поколения.
	if err := s.CreateGeneration(id); err != nil {
		t.Fatal(err)
	}
	// base содержит канонический путь каталога этого поколения.
	base := core.GenerationDirectory(id)
	// owned содержит запись принадлежащего поколению промежуточного файла без подтверждённого
	// содержимого.
	owned := core.OwnedFile{AssetID: "selected", GenerationID: id, ManagedPath: base + "/original.stage", Kind: core.OwnedStaging}
	// f содержит созданный частичный файл. err сообщает об отказе его создания.
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
	// directory содержит запись каталога поколения для проверки отказа нерекурсивной очистки.
	directory := core.OwnedFile{AssetID: "selected", GenerationID: id, ManagedPath: base, Kind: core.OwnedDirectory}
	if err = s.RemoveOwned(directory); err == nil {
		t.Fatal("неизвестный вложенный файл удалён рекурсивно")
	}
	// data содержит байты подменённого объекта после отклонённой очистки.
	data, err := os.ReadFile(filepath.Join(dir, owned.ManagedPath))
	if err != nil || string(data) != "unknown" {
		t.Fatal(string(data), err)
	}
}

// Проверяет очистку файла по размеру и SHA256 после нового открытия FileStore и отказ при другом
// содержимом либо недоказанном владении каталогом.
func TestSealedCleanupVerifiesPersistedIntegrityAfterReopen(t *testing.T) {
	// dir содержит временное дерево для проверки повторного открытия хранилища.
	dir := t.TempDir()
	// s содержит хранилище, закрываемое и повторно открываемое в сценарии. err сообщает об отказе
	// открытия.
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// id обозначает поколение с завершённым исходным файлом.
	id := core.ID("sealed-generation")
	if err = s.CreateGeneration(id); err != nil {
		t.Fatal(err)
	}
	// name содержит точный управляемый путь завершённого исходника.
	name := core.GenerationDirectory(id) + "/original"
	// f содержит создаваемый файл для записи и измерения содержимого.
	f, err := s.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("completed-original"); err != nil {
		t.Fatal(err)
	}
	// size содержит измеренный размер исходника. hash содержит SHA256 его содержимого.
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
		// Закрывает повторно открытый FileStore после проверки сохранённой целостности и сообщает ошибку через
		// t.Error.

		// err сообщает об отказе закрытия повторно открытого хранилища.
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	// owned содержит запись исходника с сохранёнными размером и SHA256 для проверки после открытия.
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

// Проверяет отказ для некорректных путей и хешей, отсутствующих файлов, пустого или слишком большого
// содержимого и закрытого дескриптора.
func TestFileOperationsRejectInvalidMissingAndChangedDescriptors(t *testing.T) {
	// s содержит временное хранилище для отрицательных сценариев файловых операций.
	s, _ := selfFileStore(t)
	// name содержит очередной недопустимый путь файла.
	for _, name := range []string{"../escape", "/escape", "", "."} {
		// f содержит ошибочно созданный файл при неожиданном успехе. err содержит результат
		// попытки создания по недопустимому пути.
		if f, err := s.Create(name); err == nil {
			_ = f.Close()
			t.Fatal(name)
		}
		// f содержит ошибочно открытый файл при неожиданном успехе. err содержит результат
		// попытки открытия по недопустимому пути.
		if f, err := s.OpenRegular(name); err == nil {
			_ = f.Close()
			t.Fatal(name)
		}
	}
	// err содержит результат создания поколения с выходом за разрешённую форму пути.
	if err := s.CreateGeneration("../escape"); err == nil {
		t.Fatal("создан каталог вне поколения")
	}
	// err содержит результат переименования пути в него самого.
	if err := s.PublishRename("same", "same"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	// err содержит результат переименования отсутствующего исходного пути.
	if err := s.PublishRename("missing", "final"); err == nil {
		t.Fatal("отсутствующий staging принят")
	}
	// hash содержит очередную некорректную запись SHA256.
	for _, hash := range []string{"", string(make([]byte, 64)), "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF"} {
		// f содержит ошибочно открытый файл при неожиданном успехе. err содержит результат
		// проверки некорректного SHA256.
		if f, err := s.OpenExact(context.Background(), "missing", 1, hash); err == nil {
			_ = f.Close()
			t.Fatal("некорректный hash принят")
		}
	}
	// f содержит пустой файл для последующих проверок размера и закрытого дескриптора. err сообщает
	// об отказе его создания.
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
