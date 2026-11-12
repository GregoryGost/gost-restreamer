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

// Проверяет, что SQL-разрешение и совпадающие байты не позволяют удалить физически заменённый файл без
// журнала и служебной ссылки. Повторное открытие FileStore и повтор очистки сохраняют замену и
// неподтверждённое намерение очистки.
func TestFileCleanupWithoutJournalPreservesSameContentReplacement(t *testing.T) {
	// f предоставляет рабочую заглушку и отдельные реестры владения для проверки очистки прежнего
	// формата файлов.
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), nil, nil)
	// ctx используется для операций подготовки и повторной очистки тестового файла.
	ctx := context.Background()
	// asset содержит отдельную заглушку прежнего формата, а err сообщает об ошибке её создания и
	// последующих операций сценария.
	asset, err := f.service.CreateAsset(ctx, "Прежняя заглушка без журнала")
	if err != nil {
		t.Fatal(err)
	}
	// generation содержит резерв подготовки, для которого тест вручную создаёт исходный файл без
	// файловых доказательств.
	generation, err := f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "controlled-generator"})
	if err != nil {
		t.Fatal(err)
	}
	// fence разрешает публикацию и подтверждение завершения подготовки этого поколения.
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
	// content задаёт одинаковые байты исходного и физически заменённого файлов для исключения
	// доказательства только по содержимому.
	content := []byte("одинаковые байты не доказывают принадлежность файла")
	// digest содержит SHA-256 одинакового содержимого исходного файла и замены.
	digest := sha256.Sum256(content)
	// hash содержит шестнадцатеричный хеш содержимого, сохранённый в реестре очистки.
	hash := hex.EncodeToString(digest[:])
	// name задаёт полный путь исходного файла, по которому позднее создаётся физическая замена.
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
	// page содержит намерения очистки удалённой заглушки для поиска точной записи исходного файла.
	page, err := f.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 64, GenerationID: generation.ID}})
	if err != nil {
		t.Fatal(err)
	}
	// intent сохраняет намерение очистки исходного файла с ожидаемыми хешем, размером и
	// неподтверждённым состоянием.
	var intent core.CleanupIntent
	// candidate обозначает очередное намерение очистки поколения при поиске записи исходного файла.
	for _, candidate := range page.Items {
		if candidate.Kind == core.CleanupFile && candidate.ManagedPath == generation.OriginalPath {
			intent = candidate
		}
	}
	if intent.FileID == "" || intent.ContentHash != hash || intent.SizeBytes != uint64(len(content)) || intent.Acknowledged {
		t.Fatalf("не создано точное FILE намерение с хешем: %+v", intent)
	}
	// owned содержит разрешённую SQL-реестром запись файла без доказательства его физической
	// принадлежности.
	owned, err := f.service.AuthorizeFileCleanup(ctx, core.CleanupFenceFor(intent))
	if err != nil || owned.ID != intent.FileID {
		t.Fatalf("SQL-разрешение cleanup отсутствует: %+v %v", owned, err)
	}
	// owner содержит сериализованную принадлежность файла заглушке для построения проверяемой
	// файловой идентичности.
	owner, err := json.Marshal(struct {
		// AssetID указывает заглушку, которой SQL-реестр приписывает очищаемый файл.
		AssetID core.ID
		// Kind указывает доменную роль очищаемого файла в сериализованном владельце.
		Kind core.OwnedFileKind
	}{owned.AssetID, owned.Kind})
	if err != nil {
		t.Fatal(err)
	}
	// identity задаёт ожидаемую файловую идентичность, доказательства которой заведомо отсутствуют в
	// журнале фикстуры.
	identity := processowner.FileIdentity{FileID: string(owned.ID), GenerationID: string(owned.GenerationID), ManagedPath: owned.ManagedPath, Kind: processowner.RegularFile, Owner: string(owner)}
	// exists показывает наличие файлового доказательства до подмены, а err сообщает об ошибке этой
	// проверки.
	if exists, err := f.files.FileJournal().Exists(ctx, identity); err != nil || exists {
		t.Fatalf("публичная проверка обнаружила legacy evidence: exists=%v %v", exists, err)
	}
	// key содержит хеш идентификатора файла для проверки отсутствия записи, служебной ссылки и
	// блокировки на диске.
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(owned.ID)))
	// relative обозначает очередной путь доказательства, который не должен существовать в фикстуре
	// прежнего формата.
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
	// displaced содержит файловую идентичность прежнего объекта после его перемещения в сторону.
	displaced, err := os.Stat(name + ".displaced")
	if err != nil {
		t.Fatal(err)
	}
	// replacement содержит файловую идентичность нового объекта по прежнему пути для проверки его
	// сохранения очисткой.
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
	// manager выполняет повторную очистку через заново открытое файловое хранилище без доказательств
	// из прежней памяти.
	manager := f.config.Preparation.(*fallback.Manager)
	for range 2 {
		if err = manager.CleanupFile(ctx, intent); !errors.Is(err, processowner.ErrUnknown) {
			t.Fatalf("без journal/anchor требовался UNKNOWN: %v", err)
		}
		// stored содержит актуальное намерение очистки после отказа, а err сообщает об ошибке
		// его чтения.
		stored, err := f.service.GetCleanup(ctx, intent.ID)
		if err != nil || stored.Acknowledged || core.CleanupFenceFor(stored) != core.CleanupFenceFor(intent) {
			t.Fatalf("недоказанное удаление подтверждено либо изменило fence: %+v %v", stored, err)
		}
		// actual содержит байты физической замены после попытки очистки.
		actual, err := os.ReadFile(name)
		if err != nil || string(actual) != string(content) {
			t.Fatalf("заменённый файл потерян: %q %v", actual, err)
		}
		// current содержит файловую идентичность объекта по исходному пути после попытки
		// очистки.
		current, err := os.Stat(name)
		if err != nil || !os.SameFile(replacement, current) {
			t.Fatalf("cleanup изменил физический объект replacement: %v", err)
		}
		// exists показывает, не присвоила ли очистка чужой файл через новый журнал, а err
		// сообщает об ошибке проверки.
		if exists, err := f.files.FileJournal().Exists(ctx, identity); err != nil || exists {
			t.Fatalf("cleanup присвоил недоказанный replacement: exists=%v %v", exists, err)
		}
	}
	// active содержит рабочую заглушку фикстуры для проверки сохранения её текущего поколения.
	active, err := f.service.GetAsset(ctx, f.asset.ID)
	if err != nil || active.CurrentGenerationID != f.generation.ID {
		t.Fatalf("старый FILE cleanup затронул рабочее поколение: %+v %v", active, err)
	}
}
