package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

func TestFileCleanupWithoutJournalPreservesSameContentReplacement(t *testing.T) {
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), nil, nil)
	ctx := context.Background()
	asset, err := f.service.CreateAsset(ctx, "Прежняя заглушка без журнала")
	if err != nil {
		t.Fatal(err)
	}
	generation, err := f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "controlled-generator"})
	if err != nil {
		t.Fatal(err)
	}
	fence, err := f.service.ClaimPreparation(ctx, generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.MarkPreparationIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// Прежний формат имеет SQL-реестр и хеш, но не создавал журнал, служебную ссылку или блокировку.
	if err = os.Mkdir(filepath.Join(f.directory, core.GenerationDirectory(generation.ID)), 0700); err != nil {
		t.Fatal(err)
	}
	content := []byte("одинаковые байты не доказывают принадлежность файла")
	digest := sha256.Sum256(content)
	hash := hex.EncodeToString(digest[:])
	name := filepath.Join(f.directory, generation.OriginalPath)
	if err = os.WriteFile(name, content, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.PublishPreparation(ctx, fence, core.GenerationPublication{SourceHash: hash, SizeBytes: uint64(len(content)), SourceFormat: core.SourceFormat{ImageFormat: "png"}}); err != nil {
		t.Fatal(err)
	}
	if err = f.service.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
	asset, err = f.service.GetAsset(ctx, asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.DeleteAsset(ctx, asset.ID, asset.Revision); err != nil {
		t.Fatal(err)
	}
	page, err := f.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 64, GenerationID: generation.ID}})
	if err != nil {
		t.Fatal(err)
	}
	var intent core.CleanupIntent
	for _, candidate := range page.Items {
		if candidate.Kind == core.CleanupFile && candidate.ManagedPath == generation.OriginalPath {
			intent = candidate
		}
	}
	if intent.FileID == "" || intent.ContentHash != hash || intent.SizeBytes != uint64(len(content)) || intent.Acknowledged {
		t.Fatalf("не создано точное FILE намерение с хешем: %+v", intent)
	}
	owned, err := f.service.AuthorizeFileCleanup(ctx, core.CleanupFenceFor(intent))
	if err != nil || owned.ID != intent.FileID {
		t.Fatalf("SQL-разрешение cleanup отсутствует: %+v %v", owned, err)
	}
	owner, err := json.Marshal(struct {
		AssetID core.ID
		Kind    core.OwnedFileKind
	}{owned.AssetID, owned.Kind})
	if err != nil {
		t.Fatal(err)
	}
	identity := processowner.FileIdentity{FileID: string(owned.ID), GenerationID: string(owned.GenerationID), ManagedPath: owned.ManagedPath, Kind: processowner.RegularFile, Owner: string(owner)}
	if exists, err := f.files.FileJournal().Exists(ctx, identity); err != nil || exists {
		t.Fatalf("публичная проверка обнаружила legacy evidence: exists=%v %v", exists, err)
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(owned.ID)))
	for _, relative := range []string{"records/" + key + ".json", "anchors/" + key, "locks/" + key + ".lock"} {
		if _, err = os.Lstat(filepath.Join(f.directory, ".processowner-evidence", relative)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy-фикстура уже содержит evidence %s: %v", relative, err)
		}
	}
	if err = os.Rename(name, name+".displaced"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(name, content, 0600); err != nil {
		t.Fatal(err)
	}
	displaced, err := os.Stat(name + ".displaced")
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := os.Stat(name)
	if err != nil || os.SameFile(displaced, replacement) {
		t.Fatal("фикстура не создала физически другой файл:", err)
	}
	// Повторное открытие исключает доказательство только из памяти прежнего FileStore.
	if err = f.files.Close(); err != nil {
		t.Fatal(err)
	}
	f.files, err = fallback.OpenFileStore(f.directory)
	if err != nil {
		t.Fatal(err)
	}
	f.configureOwners(t, f.config.Group.Binary)
	manager := f.config.Preparation.(*fallback.Manager)
	for range 2 {
		if err = manager.CleanupFile(ctx, intent); !errors.Is(err, processowner.ErrUnknown) {
			t.Fatalf("без journal/anchor требовался UNKNOWN: %v", err)
		}
		stored, err := f.service.GetCleanup(ctx, intent.ID)
		if err != nil || stored.Acknowledged || core.CleanupFenceFor(stored) != core.CleanupFenceFor(intent) {
			t.Fatalf("недоказанное удаление подтверждено либо изменило fence: %+v %v", stored, err)
		}
		actual, err := os.ReadFile(name)
		if err != nil || string(actual) != string(content) {
			t.Fatalf("заменённый файл потерян: %q %v", actual, err)
		}
		current, err := os.Stat(name)
		if err != nil || !os.SameFile(replacement, current) {
			t.Fatalf("cleanup изменил физический объект replacement: %v", err)
		}
		if exists, err := f.files.FileJournal().Exists(ctx, identity); err != nil || exists {
			t.Fatalf("cleanup присвоил недоказанный replacement: exists=%v %v", exists, err)
		}
	}
	active, err := f.service.GetAsset(ctx, f.asset.ID)
	if err != nil || active.CurrentGenerationID != f.generation.ID {
		t.Fatalf("старый FILE cleanup затронул рабочее поколение: %+v %v", active, err)
	}
}
