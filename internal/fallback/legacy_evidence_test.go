package fallback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// Проверяет, что совпадение размера и SHA256 не разрешает удалить файл с FileID без доказательства
// журнала: очистка возвращает ErrCleanupPending и ErrUnknown, содержимое сохраняется.
func TestRemoveOwnedKeepsSameContentLegacyReplacementWithoutEvidence(t *testing.T) {
	// ctx задаёт контекст проверки и очистки без дополнительного срока.
	ctx := context.Background()
	// directory содержит временный корень файлового хранилища.
	directory := t.TempDir()
	// files содержит открытое хранилище с журналом доказательств. err сообщает об отказе открытия.
	files, err := OpenFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает FileStore после проверки прежней записи без физического доказательства и сообщает ошибку
		// через t.Error.

		// err сообщает об отказе закрытия файлового хранилища.
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})

	// managedPath задаёт путь файла прежней записи без доказательства журнала.
	const managedPath = "fallback/generation/original"
	// err сообщает об отказе создания родительского каталога тестового файла.
	if err := os.MkdirAll(filepath.Join(directory, filepath.Dir(managedPath)), 0700); err != nil {
		t.Fatal(err)
	}
	// contents содержит байты, совпадающие с прежней записью, но не доказывающие владение объектом.
	contents := []byte("same bytes as the legacy ledger entry")
	// err сообщает об отказе записи заменяющего файла.
	if err := os.WriteFile(filepath.Join(directory, managedPath), contents, 0600); err != nil {
		t.Fatal(err)
	}
	// digest содержит SHA256 совпадающего тестового содержимого.
	digest := sha256.Sum256(contents)
	// owned содержит прежнюю запись FileID с верными размером и SHA256, для которой отсутствует
	// журнальное доказательство.
	owned := core.OwnedFile{
		ID:           "legacy-file-id",
		AssetID:      "asset",
		GenerationID: "generation",
		Kind:         core.OwnedOriginal,
		ManagedPath:  managedPath,
		SizeBytes:    uint64(len(contents)),
		ContentHash:  hex.EncodeToString(digest[:]),
	}

	// exists показывает наличие доказательства журнала. err сообщает об отказе проверки исходного
	// состояния.
	if exists, err := files.FileJournal().Exists(ctx, fileIdentity(owned)); exists || err != nil {
		t.Fatalf("test setup unexpectedly has journal evidence: exists=%t err=%v", exists, err)
	}
	// err содержит результат очистки по прежней записи без физического доказательства.
	if err := files.RemoveOwnedContext(ctx, owned); !errors.Is(err, ErrCleanupPending) || !errors.Is(err, processowner.ErrUnknown) {
		t.Fatalf("legacy cleanup without physical evidence must remain pending/unknown: %v", err)
	}
	// got содержит байты файла после отклонённой очистки для проверки их сохранности.
	got, err := os.ReadFile(filepath.Join(directory, managedPath))
	if err != nil || string(got) != string(contents) {
		t.Fatalf("replacement object was removed or changed: %q, %v", got, err)
	}
}
