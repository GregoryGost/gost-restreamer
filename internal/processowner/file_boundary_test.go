package processowner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func requireFileContent(t *testing.T, root, name, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, name))
	if err != nil || string(got) != want {
		t.Fatalf("объект %q изменён: %q: %v", name, got, err)
	}
}

func TestFileIDCollisionPreservesOriginalEvidence(t *testing.T) {
	root := t.TempDir()
	j := testJournal(t, root)
	ctx := testContext(t)
	id := testFileID("original", RegularFile)
	testFile(t, root, id.ManagedPath, "original payload")
	if err := j.Reserve(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := j.Register(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := j.Register(ctx, id); err != nil {
		t.Fatal("повтор той же живой записи отклонён", err)
	}
	for _, change := range []struct {
		name string
		edit func(*FileIdentity)
	}{
		{"owner", func(id *FileIdentity) { id.Owner = "other-owner" }},
		{"generation", func(id *FileIdentity) { id.GenerationID = "other-generation" }},
		{"path", func(id *FileIdentity) { id.ManagedPath = "other-path" }},
		{"kind", func(id *FileIdentity) { id.Kind = Directory }},
	} {
		t.Run(change.name, func(t *testing.T) {
			collision := id
			change.edit(&collision)
			for _, operation := range []func(context.Context, FileIdentity) error{j.Reserve, j.Register, j.Verify, j.Remove, j.Acknowledge} {
				if err := operation(ctx, collision); !errors.Is(err, ErrUnknown) {
					t.Fatalf("тот же FileID принят с другой identity: %+v: %v", collision, err)
				}
			}
			if err := j.Verify(ctx, id); err != nil {
				t.Fatal("коллизия повредила исходное доказательство", err)
			}
			requireFileContent(t, root, id.ManagedPath, "original payload")
		})
	}
	if err := j.Acknowledge(ctx, id); !errors.Is(err, ErrPending) {
		t.Fatalf("FILE ack до удаления принят: %v", err)
	}
	if err := j.Verify(ctx, id); err != nil {
		t.Fatal("преждевременный ack уничтожил anchor", err)
	}
}

func TestInvalidFileIdentityCannotCreateEvidence(t *testing.T) {
	root := t.TempDir()
	j := testJournal(t, root)
	testFile(t, root, "sentinel", "preserved")
	for _, name := range []string{"../escape", "/absolute", evidenceDirectory + "/records/escape", "parent/../sentinel", "bad\\name"} {
		id := testFileID(name, RegularFile)
		for _, operation := range []func(context.Context, FileIdentity) error{j.Reserve, j.Register, j.Verify, j.Remove, j.Acknowledge} {
			if err := operation(testContext(t), id); !errors.Is(err, ErrInvalid) {
				t.Fatalf("опасный путь %q принят: %v", name, err)
			}
		}
		if exists, err := j.Exists(testContext(t), id); exists || !errors.Is(err, ErrInvalid) {
			t.Fatalf("опасный путь создал evidence: %v: %v", exists, err)
		}
	}
	for _, directory := range []string{"records", "anchors", "locks"} {
		entries, err := os.ReadDir(filepath.Join(root, evidenceDirectory, directory))
		if err != nil || len(entries) != 0 {
			t.Fatalf("отклонённая identity изменила %s: %v: %v", directory, entries, err)
		}
	}
	requireFileContent(t, root, "sentinel", "preserved")
}

func TestDamagedFileEvidenceNeverAuthorizesRemoval(t *testing.T) {
	for _, damage := range []string{"missing-record", "invalid-json", "oversized-record", "version", "identity", "anchor-path", "phase", "symlink-record", "replaced-anchor"} {
		t.Run(damage, func(t *testing.T) {
			root := t.TempDir()
			j := testJournal(t, root)
			id := testFileID("payload", RegularFile)
			testFile(t, root, id.ManagedPath, "protected payload")
			ctx := testContext(t)
			if err := j.Register(ctx, id); err != nil {
				t.Fatal(err)
			}
			record, err := j.read(id)
			if err != nil {
				t.Fatal(err)
			}
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
			reopened := testJournal(t, root)
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

func TestRenameResumesFromDurableIntentWindows(t *testing.T) {
	for _, window := range []string{"target-intent", "linked-intents", "filesystem-renamed", "target-finalized"} {
		t.Run(window, func(t *testing.T) {
			root := t.TempDir()
			j := testJournal(t, root)
			ctx := testContext(t)
			source, target := testFileID("staging", RegularFile), testFileID("final", RegularFile)
			testFile(t, root, source.ManagedPath, "exact media payload")
			if err := j.Register(ctx, source); err != nil {
				t.Fatal(err)
			}
			a, err := j.read(source)
			if err != nil {
				t.Fatal(err)
			}
			b := newFileRecord(target)
			b.Phase, b.Peer = "RENAME_TARGET", &source
			if err = createLock(j.root, evidenceDirectory+"/locks/"+fileKey(target)+".lock"); err != nil {
				t.Fatal(err)
			}
			if err = j.write(b); err != nil {
				t.Fatal(err)
			}
			// Моделируются долговечные шаги протокола, завершённые до остановки backend.
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

func TestRenameRejectsExistingTargetWithoutChangingObjects(t *testing.T) {
	root := t.TempDir()
	j := testJournal(t, root)
	source, target := testFileID("staging", RegularFile), testFileID("final", RegularFile)
	testFile(t, root, source.ManagedPath, "new upload")
	testFile(t, root, target.ManagedPath, "existing final")
	ctx := testContext(t)
	if err := j.Register(ctx, source); err != nil {
		t.Fatal(err)
	}
	if err := j.Reserve(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := j.Rename(ctx, source, target); !errors.Is(err, ErrUnknown) {
		t.Fatalf("существующий final заменён: %v", err)
	}
	requireFileContent(t, root, source.ManagedPath, "new upload")
	requireFileContent(t, root, target.ManagedPath, "existing final")
	if err := j.Verify(ctx, source); err != nil {
		t.Fatal("отказ rename повредил staging", err)
	}
	if err := j.Remove(ctx, target); !errors.Is(err, ErrUnknown) {
		t.Fatalf("reservation разрешила удалить неизвестный final: %v", err)
	}
	requireFileContent(t, root, target.ManagedPath, "existing final")
}

func TestAcknowledgementPagingContinuesPastCorruption(t *testing.T) {
	root := t.TempDir()
	j := testJournal(t, root)
	ctx := testContext(t)
	ids := []FileIdentity{testFileID("retry", RegularFile), testFileID("unacknowledged", RegularFile), testFileID("corrupt", RegularFile)}
	for _, id := range ids {
		testFile(t, root, id.ManagedPath, id.FileID)
		if err := j.Register(ctx, id); err != nil {
			t.Fatal(err)
		}
		if err := j.Remove(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	record, err := j.read(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	record.Phase = "ACKING"
	if err = j.write(record); err != nil {
		t.Fatal(err)
	}
	// После удаления служебной ссылки, но до записи ACKED повтор должен завершить тот же FILE ack.
	if err = j.root.Remove(record.Anchor); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, fileRecordPath(ids[2])), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	reopened := testJournal(t, root)
	var observedUnknown bool
	after := ""
	for pages := 0; ; pages++ {
		if pages > len(ids) {
			t.Fatal("ограниченная пагинация не завершилась")
		}
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
	for _, tc := range []struct {
		after string
		limit int
	}{{"-1", 1}, {"invalid", 1}, {"", 0}, {"", testLimits().PageSize + 1}} {
		if _, err = reopened.RetryAcknowledgements(ctx, tc.after, tc.limit); !errors.Is(err, ErrInvalid) {
			t.Fatalf("некорректная страница %+v принята: %v", tc, err)
		}
	}
	if next, err := reopened.RetryAcknowledgements(ctx, "100", 1); err != nil || next != "" {
		t.Fatalf("курсор за концом каталога: next=%q: %v", next, err)
	}
}
