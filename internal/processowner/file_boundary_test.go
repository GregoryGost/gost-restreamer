package processowner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Сверяет содержимое файла с ожидаемыми байтами и завершает тест при ошибке чтения или несовпадении.
func requireFileContent(t *testing.T, root, name, want string) {
	t.Helper()
	// got содержит прочитанные байты защищаемого файла; err сообщает об отказе чтения при проверке
	// неизменности.
	got, err := os.ReadFile(filepath.Join(root, name))
	if err != nil || string(got) != want {
		t.Fatalf("объект %q изменён: %q: %v", name, got, err)
	}
}

// Проверяет, что повтор FileID с другим владельцем, поколением, путём или видом объекта не меняет
// исходное доказательство и не разрешает преждевременное подтверждение удаления.
func TestFileIDCollisionPreservesOriginalEvidence(t *testing.T) {
	// root задаёт изолированный каталог проверки коллизий FileID.
	root := t.TempDir()
	// j содержит журнал исходного файлового доказательства.
	j := testJournal(t, root)
	// ctx ограничивает операции проверки коллизий тестовым сроком.
	ctx := testContext(t)
	// id содержит первоначальную полную идентичность защищаемого файла.
	id := testFileID("original", RegularFile)
	testFile(t, root, id.ManagedPath, "original payload")
	// err сообщает об отказе резервирования исходной идентичности.
	if err := j.Reserve(ctx, id); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе первоначальной регистрации исходного файла.
	if err := j.Register(ctx, id); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе безопасной повторной регистрации той же живой записи.
	if err := j.Register(ctx, id); err != nil {
		t.Fatal("повтор той же живой записи отклонён", err)
	}
	// change задаёт название и способ изменения одного поля при сохранённом FileID.
	for _, change := range []struct {
		// name задаёт название изменяемого поля идентичности.
		name string
		// edit меняет владельца, поколение, путь либо вид объекта для проверки коллизии.
		edit func(*FileIdentity)
	}{
		{"owner", func(id *FileIdentity) {
			// Меняет владельца при прежнем FileID для проверки отказа коллизии идентичности.
			id.Owner = "other-owner"
		}},
		{"generation", func(id *FileIdentity) {
			// Меняет поколение при прежнем FileID для проверки сохранности исходного доказательства.
			id.GenerationID = "other-generation"
		}},
		{"path", func(id *FileIdentity) {
			// Меняет управляемый путь при прежнем FileID для проверки отказа коллизии.
			id.ManagedPath = "other-path"
		}},
		{"kind", func(id *FileIdentity) {
			// Меняет вид объекта при прежнем FileID для проверки отказа коллизии.
			id.Kind = Directory
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			// Проверяет отклонение изменённой идентичности всеми операциями журнала и сохранность исходного файла
			// и доказательства.

			// collision содержит копию исходной идентичности с выбранным несовпадением.
			collision := id
			change.edit(&collision)
			// operation содержит очередной метод журнала, который должен отвергнуть
			// коллизию.
			for _, operation := range []func(context.Context, FileIdentity) error{j.Reserve, j.Register, j.Verify, j.Remove, j.Acknowledge} {
				// err содержит ожидаемый ErrUnknown метода с чужой полной идентичностью.
				if err := operation(ctx, collision); !errors.Is(err, ErrUnknown) {
					t.Fatalf("тот же FileID принят с другой identity: %+v: %v", collision, err)
				}
			}
			// err сообщает о повреждении исходного доказательства после отклонённой
			// коллизии.
			if err := j.Verify(ctx, id); err != nil {
				t.Fatal("коллизия повредила исходное доказательство", err)
			}
			requireFileContent(t, root, id.ManagedPath, "original payload")
		})
	}
	// err содержит ожидаемый ErrPending подтверждения до физического удаления.
	if err := j.Acknowledge(ctx, id); !errors.Is(err, ErrPending) {
		t.Fatalf("FILE ack до удаления принят: %v", err)
	}
	// err сообщает о потере anchor после отвергнутого преждевременного подтверждения.
	if err := j.Verify(ctx, id); err != nil {
		t.Fatal("преждевременный ack уничтожил anchor", err)
	}
}

// Проверяет отклонение опасных путей без создания записей, служебных ссылок и блокировок и без
// изменения существующего файла.
func TestInvalidFileIdentityCannotCreateEvidence(t *testing.T) {
	// root задаёт изолированный каталог проверки опасных путей без создания доказательств.
	root := t.TempDir()
	// j содержит журнал, чьи служебные каталоги должны остаться пустыми.
	j := testJournal(t, root)
	testFile(t, root, "sentinel", "preserved")
	// name содержит очередной опасный либо неканонический управляемый путь.
	for _, name := range []string{"../escape", "/absolute", evidenceDirectory + "/records/escape", "parent/../sentinel", "bad\\name"} {
		// id содержит файловую идентичность с проверяемым опасным путём.
		id := testFileID(name, RegularFile)
		// operation содержит очередной метод журнала для проверки отказа до побочных эффектов.
		for _, operation := range []func(context.Context, FileIdentity) error{j.Reserve, j.Register, j.Verify, j.Remove, j.Acknowledge} {
			// err содержит ожидаемый ErrInvalid для опасного управляемого пути.
			if err := operation(testContext(t), id); !errors.Is(err, ErrInvalid) {
				t.Fatalf("опасный путь %q принят: %v", name, err)
			}
		}
		// exists содержит результат поиска доказательства опасного пути; err содержит ожидаемый
		// ErrInvalid.
		if exists, err := j.Exists(testContext(t), id); exists || !errors.Is(err, ErrInvalid) {
			t.Fatalf("опасный путь создал evidence: %v: %v", exists, err)
		}
	}
	// directory задаёт очередной служебный каталог records, anchors либо locks.
	for _, directory := range []string{"records", "anchors", "locks"} {
		// entries содержит его содержимое; err сообщает об отказе проверки отсутствия новых
		// объектов.
		entries, err := os.ReadDir(filepath.Join(root, evidenceDirectory, directory))
		if err != nil || len(entries) != 0 {
			t.Fatalf("отклонённая identity изменила %s: %v: %v", directory, entries, err)
		}
	}
	requireFileContent(t, root, "sentinel", "preserved")
}

// Проверяет, что повреждённая или подменённая запись и служебная ссылка после повторного открытия дают
// ErrUnknown и сохраняют защищаемый файл.
func TestDamagedFileEvidenceNeverAuthorizesRemoval(t *testing.T) {
	// damage выбирает повреждаемую запись, ссылку либо поле файлового доказательства.
	for _, damage := range []string{"missing-record", "invalid-json", "oversized-record", "version", "identity", "anchor-path", "phase", "symlink-record", "replaced-anchor"} {
		t.Run(damage, func(t *testing.T) {
			// Повреждает выбранную часть доказательства и проверяет отказ Exists, Verify и Remove без изменения
			// защищаемого файла.

			// root задаёт изолированный каталог выбранного повреждения доказательства.
			root := t.TempDir()
			// j содержит исходный журнал защищаемого файла.
			j := testJournal(t, root)
			// id обозначает файл, который повреждённое доказательство не должно разрешать
			// удалить.
			id := testFileID("payload", RegularFile)
			testFile(t, root, id.ManagedPath, "protected payload")
			// ctx ограничивает регистрацию и проверки выбранного повреждения.
			ctx := testContext(t)
			// err сообщает об отказе первоначальной регистрации защищаемого файла.
			if err := j.Register(ctx, id); err != nil {
				t.Fatal(err)
			}
			// record содержит исходную запись доказательства; err сохраняет результаты
			// чтения, повреждения и последующих проверок.
			record, err := j.read(id)
			if err != nil {
				t.Fatal(err)
			}
			// name задаёт точный путь JSON-записи, изменяемой выбранным сценарием.
			name := fileRecordPath(id)
			switch damage {
			case "missing-record":
				err = j.root.Remove(name)
			case "invalid-json":
				err = os.WriteFile(filepath.Join(root, name), []byte("{"), 0600)
			case "oversized-record":
				err = os.WriteFile(filepath.Join(root, name), []byte(strings.Repeat("x", int(testLimits().MaxRecordBytes)+1)), 0600)
			case "version":
				record.Version++
				err = j.write(record)
			case "identity":
				record.Identity.Owner = "different-owner"
				err = j.write(record)
			case "anchor-path":
				record.Anchor = id.ManagedPath
				err = j.write(record)
			case "phase":
				record.Phase = "UNRECOGNIZED"
				err = j.write(record)
			case "symlink-record":
				if err = j.root.Rename(name, name+".saved"); err == nil {
					err = os.Symlink(filepath.Base(name)+".saved", filepath.Join(root, name))
				}
			case "replaced-anchor":
				if err = j.root.Remove(record.Anchor); err == nil {
					err = os.WriteFile(filepath.Join(root, record.Anchor), []byte("protected payload"), 0600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			// reopened содержит повторно открытый журнал после повреждения доказательства.
			reopened := testJournal(t, root)
			// exists отмечает найденное повреждённое доказательство; err содержит ожидаемый
			// ErrUnknown.
			if exists, err := reopened.Exists(ctx, id); !exists || !errors.Is(err, ErrUnknown) {
				t.Fatalf("повреждение %s принято за legacy: %v: %v", damage, exists, err)
			}
			if err = reopened.Verify(ctx, id); !errors.Is(err, ErrUnknown) {
				t.Fatalf("повреждение %s подтвердило объект: %v", damage, err)
			}
			if err = reopened.Remove(ctx, id); !errors.Is(err, ErrUnknown) {
				t.Fatalf("повреждение %s разрешило удаление: %v", damage, err)
			}
			requireFileContent(t, root, id.ManagedPath, "protected payload")
		})
	}
}

// Проверяет повтор переименования из четырёх долговечных промежуточных состояний. Подтверждение
// удаления исходника должно сохранить независимое доказательство конечного файла.
func TestRenameResumesFromDurableIntentWindows(t *testing.T) {
	// window выбирает долговечную промежуточную стадию восстанавливаемого переименования.
	for _, window := range []string{"target-intent", "linked-intents", "filesystem-renamed", "target-finalized"} {
		t.Run(window, func(t *testing.T) {
			// Воссоздаёт выбранное долговечное окно переименования и проверяет продолжение после повторного
			// открытия журнала с независимым доказательством целевого файла.

			// root задаёт изолированный каталог выбранного окна Rename.
			root := t.TempDir()
			// j содержит исходный журнал перед моделированием остановки процесса.
			j := testJournal(t, root)
			// ctx ограничивает создание намерений и повторное переименование.
			ctx := testContext(t)
			// source обозначает staging-файл; target обозначает final-файл с самостоятельной
			// идентичностью.
			source, target := testFileID("staging", RegularFile), testFileID("final", RegularFile)
			testFile(t, root, source.ManagedPath, "exact media payload")
			// err сообщает об отказе первоначальной регистрации staging-файла.
			if err := j.Register(ctx, source); err != nil {
				t.Fatal(err)
			}
			// a содержит исходную запись; err сохраняет результаты создания промежуточных
			// стадий и восстановления Rename.
			a, err := j.read(source)
			if err != nil {
				t.Fatal(err)
			}
			// b содержит целевую запись с намерением RENAME_TARGET и ссылкой на source.
			b := newFileRecord(target)
			b.Phase, b.Peer = "RENAME_TARGET", &source
			if err = createLock(j.root, evidenceDirectory+"/locks/"+fileKey(target)+".lock"); err != nil {
				t.Fatal(err)
			}
			if err = j.write(b); err != nil {
				t.Fatal(err)
			}
			// Моделируются долговечные шаги протокола, завершённые до остановки основного процесса.
			if window != "target-intent" {
				if err = j.root.Link(a.Anchor, b.Anchor); err != nil {
					t.Fatal(err)
				}
				a.Phase, a.Peer = "RENAME_SOURCE", &target
				if err = j.write(a); err != nil {
					t.Fatal(err)
				}
			}
			if window == "filesystem-renamed" || window == "target-finalized" {
				if err = j.root.Rename(source.ManagedPath, target.ManagedPath); err != nil {
					t.Fatal(err)
				}
			}
			if window == "target-finalized" {
				b.Phase = "LIVE"
				if err = j.write(b); err != nil {
					t.Fatal(err)
				}
			}
			if err = j.syncParents(source.ManagedPath, target.ManagedPath, b.Anchor); err != nil {
				t.Fatal(err)
			}
			// reopened содержит журнал после моделируемого окна для продолжения того же
			// Rename.
			reopened := testJournal(t, root)
			if err = reopened.Rename(ctx, source, target); err != nil {
				t.Fatalf("rename не восстановился из окна %s: %v", window, err)
			}
			if _, err = os.Stat(filepath.Join(root, source.ManagedPath)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("исходный путь сохранился: %v", err)
			}
			requireFileContent(t, root, target.ManagedPath, "exact media payload")
			if err = reopened.Verify(ctx, target); err != nil {
				t.Fatal("rename потерял доказательство final", err)
			}
			if err = reopened.Remove(ctx, source); err != nil {
				t.Fatal(err)
			}
			if err = reopened.Acknowledge(ctx, source); err != nil {
				t.Fatal(err)
			}
			if err = reopened.Verify(ctx, target); err != nil {
				t.Fatal("FILE ack source уничтожил независимый final anchor", err)
			}
		})
	}
}

// Проверяет, что существующий конечный файл не заменяется при переименовании и не удаляется на
// основании одной резервирующей записи.
func TestRenameRejectsExistingTargetWithoutChangingObjects(t *testing.T) {
	// root задаёт изолированный каталог проверки уже существующего final.
	root := t.TempDir()
	// j содержит журнал, который должен сохранить оба исходных объекта.
	j := testJournal(t, root)
	// source обозначает новый staging-файл; target обозначает ранее существующий final без
	// доказательства владения.
	source, target := testFileID("staging", RegularFile), testFileID("final", RegularFile)
	testFile(t, root, source.ManagedPath, "new upload")
	testFile(t, root, target.ManagedPath, "existing final")
	// ctx ограничивает операции проверки отказа замены конечного файла.
	ctx := testContext(t)
	// err сообщает об отказе регистрации нового staging-файла.
	if err := j.Register(ctx, source); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе резервации целевой идентичности без признания существующего объекта
	// своим.
	if err := j.Reserve(ctx, target); err != nil {
		t.Fatal(err)
	}
	// err содержит ожидаемый ErrUnknown попытки заменить существующий final.
	if err := j.Rename(ctx, source, target); !errors.Is(err, ErrUnknown) {
		t.Fatalf("существующий final заменён: %v", err)
	}
	requireFileContent(t, root, source.ManagedPath, "new upload")
	requireFileContent(t, root, target.ManagedPath, "existing final")
	// err сообщает о повреждении доказательства staging после отказа Rename.
	if err := j.Verify(ctx, source); err != nil {
		t.Fatal("отказ rename повредил staging", err)
	}
	// err содержит ожидаемый ErrUnknown удаления существующего final по одной RESERVED-записи.
	if err := j.Remove(ctx, target); !errors.Is(err, ErrUnknown) {
		t.Fatalf("reservation разрешила удалить неизвестный final: %v", err)
	}
	requireFileContent(t, root, target.ManagedPath, "existing final")
}

// Проверяет продолжение ограниченной сверки после повреждённой записи и завершение ранее начатого
// ACKING. Факт удаления соседнего файла не заменяет подтверждение его владельца.
func TestAcknowledgementPagingContinuesPastCorruption(t *testing.T) {
	// root задаёт изолированный каталог проверки пагинации подтверждений при повреждённой записи.
	root := t.TempDir()
	// j содержит журнал трёх контрольных файлов.
	j := testJournal(t, root)
	// ctx ограничивает подготовку, повторные подтверждения и чтение результатов.
	ctx := testContext(t)
	// ids задаёт файл ACKING, файл без разрешённого подтверждения и файл с повреждаемой записью.
	ids := []FileIdentity{testFileID("retry", RegularFile), testFileID("unacknowledged", RegularFile), testFileID("corrupt", RegularFile)}
	// id содержит очередную идентичность для создания, регистрации и физического удаления файла.
	for _, id := range ids {
		testFile(t, root, id.ManagedPath, id.FileID)
		// err сообщает об отказе регистрации очередного файла перед проверкой подтверждений.
		if err := j.Register(ctx, id); err != nil {
			t.Fatal(err)
		}
		// err сообщает об отказе физического удаления очередного контрольного файла.
		if err := j.Remove(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	// record содержит запись файла retry; err сохраняет результаты моделирования ACKING и
	// последующего чтения.
	record, err := j.read(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	record.Phase = "ACKING"
	if err = j.write(record); err != nil {
		t.Fatal(err)
	}
	// После удаления служебной ссылки, но до записи ACKED повтор должен завершить то же подтверждение FILE.
	if err = j.root.Remove(record.Anchor); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, fileRecordPath(ids[2])), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	// reopened содержит повторно открытый журнал для ограниченного продолжения ACKING.
	reopened := testJournal(t, root)
	// observedUnknown отмечает, что пагинация явно сообщила о повреждённой записи.
	var observedUnknown bool
	// after содержит текущий курсор повторного подтверждения каталога.
	after := ""
	// pages считает прочитанные страницы для проверки конечного продвижения курсора.
	for pages := 0; ; pages++ {
		if pages > len(ids) {
			t.Fatal("ограниченная пагинация не завершилась")
		}
		// next содержит следующий курсор; err сообщает о повреждении записи либо отказе прохода
		// страницы.
		next, err := reopened.RetryAcknowledgements(ctx, after, 1)
		if err != nil && !errors.Is(err, ErrUnknown) {
			t.Fatal(err)
		}
		observedUnknown = observedUnknown || errors.Is(err, ErrUnknown)
		if next == "" {
			break
		}
		if next == after {
			t.Fatal("курсор повторного подтверждения не продвинулся")
		}
		after = next
	}
	if !observedUnknown {
		t.Fatal("повреждённая запись скрыта при сверке")
	}
	record, err = reopened.read(ids[0])
	if err != nil || record.Phase != "ACKED" {
		t.Fatalf("неповреждённый FILE ack не завершён: %+v: %v", record, err)
	}
	if err = reopened.Acknowledge(ctx, ids[0]); err != nil {
		t.Fatal("повтор уже завершённого ack не идемпотентен", err)
	}
	if _, err = os.Stat(filepath.Join(root, evidenceDirectory, "anchors", fileKey(ids[1]))); err != nil {
		t.Fatal("повтор сверки вывел SQL ack из факта удаления", err)
	}
	// tc содержит очередной неверный курсор или размер страницы подтверждений.
	for _, tc := range []struct {
		// after задаёт проверяемый некорректный курсор страницы.
		after string
		// limit задаёт проверяемый некорректный размер страницы.
		limit int
	}{{"-1", 1}, {"invalid", 1}, {"", 0}, {"", testLimits().PageSize + 1}} {
		if _, err = reopened.RetryAcknowledgements(ctx, tc.after, tc.limit); !errors.Is(err, ErrInvalid) {
			t.Fatalf("некорректная страница %+v принята: %v", tc, err)
		}
	}
	// next содержит курсор после запроса за концом каталога; err сообщает о неожиданном отказе
	// такого чтения.
	if next, err := reopened.RetryAcknowledgements(ctx, "100", 1); err != nil || next != "" {
		t.Fatalf("курсор за концом каталога: next=%q: %v", next, err)
	}
}
