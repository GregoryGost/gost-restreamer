package processowner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func boundaryHelperPath(t *testing.T) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(name, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestRegistryRejectsConfigurationBeforeCreatingRoots(t *testing.T) {
	helper := boundaryHelperPath(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative := func(name string) string {
		t.Helper()
		rel, err := filepath.Rel(cwd, name)
		if err != nil {
			t.Fatal(err)
		}
		return rel
	}
	for _, name := range []string{"operation-budget", "record-budget", "command-budget", "descriptor-budget", "page-budget", "grace-period", "relative-root", "relative-runtime", "relative-helper", "missing-helper", "directory-helper", "nonexecutable-helper"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			config := Config{Root: filepath.Join(base, "journal"), RuntimeRoot: filepath.Join(base, "runtime"), HelperPath: helper, Limits: testLimits(), GracePeriod: time.Millisecond}
			want := ErrInvalid
			switch name {
			case "operation-budget":
				config.Limits.MaxOperations = 100001
			case "record-budget":
				config.Limits.MaxRecordBytes = 16<<20 + 1
			case "command-budget":
				config.Limits.MaxCommandBytes = int(config.Limits.MaxRecordBytes)
			case "descriptor-budget":
				config.Limits.MaxExtraFiles = 129
			case "page-budget":
				config.Limits.PageSize = 1001
			case "grace-period":
				config.GracePeriod = 0
			case "relative-root":
				config.Root = relative(config.Root)
			case "relative-runtime":
				config.RuntimeRoot = relative(config.RuntimeRoot)
			case "relative-helper":
				config.HelperPath = relative(helper)
			case "missing-helper":
				config.HelperPath = filepath.Join(base, "missing")
				want = os.ErrNotExist
			case "directory-helper":
				config.HelperPath = base
			case "nonexecutable-helper":
				config.HelperPath = filepath.Join(base, "helper")
				if err := os.WriteFile(config.HelperPath, []byte("preserved"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			r, err := New(config)
			if r != nil {
				_ = r.Close()
				t.Fatal("некорректная конфигурация создала Registry")
			}
			if !errors.Is(err, want) {
				t.Fatalf("отказ конфигурации=%v, ожидается %v", err, want)
			}
			for _, root := range []string{filepath.Join(base, "journal"), filepath.Join(base, "runtime")} {
				if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("отклонённая конфигурация создала корень %q: %v", root, err)
				}
			}
			if name == "nonexecutable-helper" {
				requireFileContent(t, base, "helper", "preserved")
			}
		})
	}
}

func TestFileJournalRejectsUnsupportedEvidenceRoots(t *testing.T) {
	for _, name := range []string{"file", "symlink"} {
		t.Run(name, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			testFile(t, outside, "sentinel", "outside preserved")
			if name == "file" {
				testFile(t, root, evidenceDirectory, "occupied evidence path")
			} else if err := os.Symlink(outside, filepath.Join(root, evidenceDirectory)); err != nil {
				t.Fatal(err)
			}
			j, err := NewFileJournal(root, testLimits())
			if j != nil {
				_ = j.Close()
				t.Fatal("неподдержанный корень доказательств принят")
			}
			if !errors.Is(err, ErrUnknown) {
				t.Fatalf("неподдержанный корень=%v", err)
			}
			requireFileContent(t, outside, "sentinel", "outside preserved")
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 1 {
				t.Fatalf("конструктор записал данные через symlink: %v: %v", entries, err)
			}
			if name == "file" {
				requireFileContent(t, root, evidenceDirectory, "occupied evidence path")
			}
		})
	}
}

func boundaryClosedScope(t *testing.T) (*Registry, *Scope, FileIdentity, string) {
	t.Helper()
	r := testRegistry(t, boundaryHelperPath(t))
	s, id, opID := boundaryScopeWithPayload(t, r)
	return r, s, id, opID
}

func boundaryScopeWithPayload(t *testing.T, r *Registry) (*Scope, FileIdentity, string) {
	t.Helper()
	s := testScope(t, r)
	ctx := testContext(t)
	ticket, err := s.Register(ctx, "file-builder")
	if err != nil {
		t.Fatal(err)
	}
	testFile(t, s.Namespace(), "payload", "protected runtime payload")
	if err = s.RegisterRuntime(ctx, "payload", RegularFile); err != nil {
		t.Fatal(err)
	}
	id, err := s.RuntimeIdentity("payload", RegularFile)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.header()
	if err != nil {
		t.Fatal(err)
	}
	if err = ticket.Complete(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	return s, id, fmt.Sprintf("%08d", h.Count-1)
}

func boundarySavedRecord(t *testing.T, root *os.Root, name string) []byte {
	t.Helper()
	b, err := root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.WriteFile(name, b, 0600); err != nil {
			t.Error(err)
		}
	})
	return b
}

func boundaryRequireProtectedRuntime(t *testing.T, r *Registry, s *Scope, id FileIdentity) {
	t.Helper()
	requireFileContent(t, s.Namespace(), "payload", "protected runtime payload")
	if err := r.runtime.Verify(testContext(t), id); err != nil {
		t.Fatal("повреждение scope уничтожило независимое файловое доказательство", err)
	}
	h, err := s.header()
	if err == nil && h.Acknowledged {
		t.Fatal("повреждённая область получила подтверждение очистки")
	}
}

func boundaryRequireRestoredScope(t *testing.T, r *Registry, s *Scope) {
	t.Helper()
	ctx := testContext(t)
	proof, err := s.Resolve(ctx)
	if err != nil || !proof.Joined || proof.Unknown || proof.Acknowledged {
		t.Fatalf("восстановленное доказательство=%+v: %v", proof, err)
	}
	if _, err = os.Stat(s.Namespace()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("подтверждённое пространство имён не удалено: %v", err)
	}
	if err = r.AcknowledgeScope(ctx, s.ID()); err != nil {
		t.Fatal(err)
	}
	proof, err = s.Resolve(ctx)
	if err != nil || !proof.Joined || !proof.Acknowledged {
		t.Fatalf("окончательное подтверждение=%+v: %v", proof, err)
	}
}

func TestDamagedScopeHeaderPreservesRuntimeAndAcknowledgement(t *testing.T) {
	for _, damage := range []string{"protocol", "scope-id", "runtime-root", "negative-catalog", "unbounded-catalog", "json", "oversized"} {
		t.Run(damage, func(t *testing.T) {
			r, s, identity, _ := boundaryClosedScope(t)
			name := scopePath(s.ID()) + "/header.json"
			original := boundarySavedRecord(t, r.root, name)
			h, err := s.header()
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "protocol":
				h.Version++
			case "scope-id":
				h.ID = strings.Repeat("a", 32)
			case "runtime-root":
				h.RuntimeRoot = t.TempDir()
			case "negative-catalog":
				h.Count = -1
			case "unbounded-catalog":
				h.Count = h.Limits.MaxOperations + 1
			}
			switch damage {
			case "json":
				err = r.root.WriteFile(name, []byte("{"), 0600)
			case "oversized":
				err = r.root.WriteFile(name, bytes.Repeat([]byte("x"), int(r.config.Limits.MaxRecordBytes)+1), 0600)
			default:
				err = writeRecord(r.root, name, h, r.config.Limits.MaxRecordBytes)
			}
			if err != nil {
				t.Fatal(err)
			}
			proof, err := s.Resolve(testContext(t))
			if !errors.Is(err, ErrUnknown) || !proof.Unknown || proof.Joined || proof.Acknowledged {
				t.Fatalf("повреждённый заголовок дал доказательство=%+v: %v", proof, err)
			}
			if opened, err := r.OpenScope(testContext(t), s.ID()); opened != nil || !errors.Is(err, ErrUnknown) {
				t.Fatalf("повреждённый заголовок открыт: scope=%v: %v", opened, err)
			}
			if err = r.AcknowledgeScope(testContext(t), s.ID()); !errors.Is(err, ErrUnknown) {
				t.Fatalf("повреждённый заголовок подтверждён: %v", err)
			}
			boundaryRequireProtectedRuntime(t, r, s, identity)
			if err = r.root.WriteFile(name, original, 0600); err != nil {
				t.Fatal(err)
			}
			boundaryRequireRestoredScope(t, r, s)
		})
	}
}

func TestIncompleteScopeCatalogCannotJoinOrRemoveRuntime(t *testing.T) {
	for _, damage := range []string{"missing-record", "record-json", "foreign-scope", "foreign-operation", "unsupported-state", "spawn-without-wait", "missing-lock", "symlink-lock", "foreign-runtime-owner"} {
		t.Run(damage, func(t *testing.T) {
			r, s, identity, opID := boundaryClosedScope(t)
			name := operationPath(s.ID(), opID) + ".json"
			original := boundarySavedRecord(t, r.root, name)
			op, err := s.readOperation(opID)
			if err != nil {
				t.Fatal(err)
			}
			lockName := operationPath(s.ID(), opID) + ".lock"
			restoreLock := false
			if damage == "missing-lock" || damage == "symlink-lock" {
				if err = r.root.Rename(lockName, lockName+".saved"); err != nil {
					t.Fatal(err)
				}
				restoreLock = true
				t.Cleanup(func() {
					if !restoreLock {
						return
					}
					_ = r.root.Remove(lockName)
					if err := r.root.Rename(lockName+".saved", lockName); err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Error(err)
					}
				})
				if damage == "symlink-lock" {
					err = os.Symlink(filepath.Base(lockName)+".saved", filepath.Join(r.config.Root, lockName))
				}
			} else {
				switch damage {
				case "missing-record":
					err = r.root.Remove(name)
				case "record-json":
					err = r.root.WriteFile(name, []byte("{"), 0600)
				case "foreign-scope":
					op.ScopeID = strings.Repeat("b", 32)
				case "foreign-operation":
					op.ID = "99999999"
				case "unsupported-state":
					op.State = "UNKNOWN_RESULT"
				case "spawn-without-wait":
					op.Kind, op.State = "native", spawned
				case "foreign-runtime-owner":
					op.File.Owner = "another-execution"
				}
				if damage != "missing-record" && damage != "record-json" {
					err = writeRecord(r.root, name, op, r.config.Limits.MaxRecordBytes)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			proof, err := s.Resolve(testContext(t))
			if err != nil || proof.Joined || !proof.Unknown || !proof.Sealed || proof.Acknowledged {
				t.Fatalf("неполный каталог дал доказательство=%+v: %v", proof, err)
			}
			if err = r.AcknowledgeScope(testContext(t), s.ID()); !errors.Is(err, ErrPending) {
				t.Fatalf("неполный каталог подтверждён: %v", err)
			}
			boundaryRequireProtectedRuntime(t, r, s, identity)
			if damage == "missing-lock" || damage == "symlink-lock" {
				_ = r.root.Remove(lockName)
				if err = r.root.Rename(lockName+".saved", lockName); err != nil {
					t.Fatal(err)
				}
				restoreLock = false
			}
			if err = r.root.WriteFile(name, original, 0600); err != nil {
				t.Fatal(err)
			}
			boundaryRequireRestoredScope(t, r, s)
		})
	}
}

func TestRecoveryPagingPreservesUnknownScopesAndFindsHealthyScope(t *testing.T) {
	r, s, _, _ := boundaryClosedScope(t)
	ctx := testContext(t)
	corruptID := strings.Repeat("c", 32)
	for _, id := range []string{"unrecognized-entry", corruptID} {
		if err := r.root.Mkdir(scopePath(id), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.root.WriteFile(scopePath(corruptID)+"/header.json", []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	seen := map[string]ScopeResult{}
	after := ""
	for calls := 0; ; calls++ {
		if calls > 3 {
			t.Fatal("ограниченная пагинация не завершилась")
		}
		page, next, err := r.RecoverPage(ctx, after, 1)
		if err != nil || len(page) > 1 {
			t.Fatalf("страница=%+v: %v", page, err)
		}
		for _, item := range page {
			if _, duplicate := seen[item.ID]; duplicate {
				t.Fatalf("область повторилась в неизменном каталоге: %s", item.ID)
			}
			seen[item.ID] = item
		}
		if next == "" {
			break
		}
		if next == after {
			t.Fatal("курсор восстановления не продвинулся")
		}
		after = next
	}
	if len(seen) != 3 || !seen[s.ID()].Joined || seen[s.ID()].Unknown {
		t.Fatalf("здоровая область потеряна из-за повреждённого соседа: %+v", seen)
	}
	for _, id := range []string{"unrecognized-entry", corruptID} {
		if !seen[id].Unknown || seen[id].Joined || seen[id].Acknowledged {
			t.Fatalf("неизвестная область подтверждена: %+v", seen[id])
		}
		if _, err := r.root.Stat(scopePath(id)); err != nil {
			t.Fatal("неизвестная область удалена", err)
		}
	}
	for _, after := range []string{"-1", "invalid"} {
		if page, _, err := r.RecoverPage(ctx, after, 1); len(page) != 0 || !errors.Is(err, ErrInvalid) {
			t.Fatalf("некорректный курсор %q дал результат: %+v: %v", after, page, err)
		}
	}
	if page, next, err := r.RecoverPage(ctx, "100", 1); err != nil || len(page) != 0 || next != "" {
		t.Fatalf("курсор за концом каталога: %+v %q: %v", page, next, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if page, _, err := r.RecoverPage(cancelled, "", 1); len(page) != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("отменённое восстановление вернуло доказательство: %+v: %v", page, err)
	}
}

func TestScopeLockCancellationPreservesCatalogAndPendingWork(t *testing.T) {
	r := testRegistry(t, boundaryHelperPath(t))
	s := testScope(t, r)
	ctx := testContext(t)
	ticket, err := s.Register(ctx, "pending-builder")
	if err != nil {
		t.Fatal(err)
	}
	headerName := scopePath(s.ID()) + "/header.json"
	opName := operationPath(s.ID(), ticket.id) + ".json"
	header := boundarySavedRecord(t, r.root, headerName)
	op := boundarySavedRecord(t, r.root, opName)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	gate, err := lockExisting(ctx, r.root, scopePath(s.ID())+"/gate.lock", true)
	if err != nil {
		t.Fatal(err)
	}
	if late, err := s.Register(cancelled, "late-builder"); late != nil || !errors.Is(err, context.Canceled) {
		_ = unlock(gate)
		t.Fatalf("отменённая регистрация принята: %v: %v", late, err)
	}
	if err = s.Seal(cancelled); !errors.Is(err, context.Canceled) {
		_ = unlock(gate)
		t.Fatalf("отменённый seal=%v", err)
	}
	if err = unlock(gate); err != nil {
		t.Fatal(err)
	}
	lock, err := lockExisting(ctx, r.root, operationPath(s.ID(), ticket.id)+".lock", true)
	if err != nil {
		t.Fatal(err)
	}
	if err = ticket.Complete(cancelled); !errors.Is(err, context.Canceled) {
		_ = unlock(lock)
		t.Fatalf("отменённое завершение=%v", err)
	}
	if err = s.Seal(ctx); err != nil {
		_ = unlock(lock)
		t.Fatal(err)
	}
	proof, err := s.Resolve(ctx)
	if err != nil || proof.Joined || proof.Unknown || proof.PendingOperations != 1 || !proof.Sealed {
		_ = unlock(lock)
		t.Fatalf("занятая операция подтверждена: %+v: %v", proof, err)
	}
	if err = unlock(lock); err != nil {
		t.Fatal(err)
	}
	currentOp, err := r.root.ReadFile(opName)
	if err != nil || !bytes.Equal(currentOp, op) {
		t.Fatalf("отмена переписала результат операции: %s: %v", currentOp, err)
	}
	if err = r.AcknowledgeScope(cancelled, s.ID()); !errors.Is(err, context.Canceled) {
		t.Fatalf("отменённый ack=%v", err)
	}
	h, err := s.header()
	if err != nil || h.Count != 2 || h.Acknowledged {
		t.Fatalf("отмена изменила каталог/ack: %+v: %v", h, err)
	}
	// Единственным разрешённым изменением заголовка был явный успешный Seal.
	var prior scopeHeader
	if err = json.Unmarshal(header, &prior); err != nil {
		t.Fatal(err)
	}
	prior.Sealed = true
	if h != prior {
		t.Fatalf("отмена изменила заголовок помимо успешного Seal: before=%+v after=%+v", prior, h)
	}
	if err = s.RetryCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	proof, err = s.Resolve(ctx)
	if err != nil || !proof.Joined || proof.Unknown || proof.PendingOperations != 0 {
		t.Fatalf("повтор физически завершённой работы не восстановил доказательство: %+v: %v", proof, err)
	}
}

func TestClosedRegistryCannotInventProofOrRemoveRuntime(t *testing.T) {
	r, err := New(Config{Root: filepath.Join(t.TempDir(), "journal"), HelperPath: boundaryHelperPath(t), Limits: testLimits(), GracePeriod: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	rootClosed := false
	t.Cleanup(func() {
		if !rootClosed {
			if err := r.root.Close(); err != nil {
				t.Error(err)
			}
		}
		if err := r.runtime.Close(); err != nil {
			t.Error(err)
		}
	})
	s, identity, _ := boundaryScopeWithPayload(t, r)
	// Закрываются дескрипторы, а сами объекты остаются доступными для независимой проверки через ОС.
	if err := r.root.Close(); err != nil {
		t.Fatal(err)
	}
	rootClosed = true
	ctx := testContext(t)
	if created, err := r.Create(ctx, "new-owner"); created != nil || err == nil {
		t.Fatalf("закрытый Registry создал область: %v: %v", created, err)
	}
	if opened, err := r.OpenScope(ctx, s.ID()); opened != nil || !errors.Is(err, ErrUnknown) {
		t.Fatalf("закрытый Registry открыл область: %v: %v", opened, err)
	}
	proof, err := s.Resolve(ctx)
	if !errors.Is(err, ErrUnknown) || !proof.Unknown || proof.Joined || proof.Acknowledged {
		t.Fatalf("закрытый корень подтвердил завершение: %+v: %v", proof, err)
	}
	if page, _, err := r.RecoverPage(ctx, "", 1); len(page) != 0 || err == nil {
		t.Fatalf("закрытый корень дал recovery proof: %+v: %v", page, err)
	}
	if err = r.AcknowledgeScope(ctx, s.ID()); !errors.Is(err, ErrUnknown) {
		t.Fatalf("закрытый корень подтвердил очистку: %v", err)
	}
	requireFileContent(t, s.Namespace(), "payload", "protected runtime payload")
	if err = r.runtime.Verify(ctx, identity); err != nil {
		t.Fatal("закрытие журнала процессов уничтожило файловый anchor", err)
	}
}

func TestRuntimeIdentityLimitsRejectRegistrationWithoutArtifacts(t *testing.T) {
	r := testRegistry(t, boundaryHelperPath(t))
	s := testScope(t, r)
	ctx := testContext(t)
	owner, err := s.Owner()
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.RuntimeIdentity(".", Directory)
	if err != nil || id.FileID != s.ID()+":." || id.GenerationID != s.ID() || id.ManagedPath != s.ID() || id.Owner != owner || id.Kind != Directory {
		t.Fatalf("корень runtime потерял точного владельца: %+v: %v", id, err)
	}
	h, err := s.header()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ name, kind string }{{"payload", "unsupported"}, {strings.Repeat("x", 4097), RegularFile}, {"bad\xff", RegularFile}} {
		if err = s.RegisterRuntime(ctx, item.name, item.kind); !errors.Is(err, ErrInvalid) {
			t.Fatalf("некорректная runtime identity принята: %v", err)
		}
	}
	after, err := s.header()
	if err != nil || after.Count != h.Count {
		t.Fatalf("отклонённая identity расширила каталог: %+v: %v", after, err)
	}
	if err = s.Seal(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterRuntime(ctx, "late", RegularFile); !errors.Is(err, ErrClosed) {
		t.Fatalf("sealed scope принял новый объект: %v", err)
	}
	if _, err = os.Stat(filepath.Join(s.Namespace(), "late")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("поздний объект создан: %v", err)
	}
}

func TestFileIdentityBoundsPreserveUnregisteredObject(t *testing.T) {
	root := t.TempDir()
	j := testJournal(t, root)
	testFile(t, root, "payload", "preserved")
	for _, field := range []string{"file-id", "generation", "owner", "utf8", "kind"} {
		id := testFileID("payload", RegularFile)
		switch field {
		case "file-id":
			id.FileID = strings.Repeat("f", 513)
		case "generation":
			id.GenerationID = strings.Repeat("g", 513)
		case "owner":
			id.Owner = strings.Repeat("o", 4097)
		case "utf8":
			id.GenerationID = "bad\xff"
		case "kind":
			id.Kind = "unsupported"
		}
		for _, action := range []func(context.Context, FileIdentity) error{j.Reserve, j.Register, j.Remove, j.Acknowledge} {
			if err := action(testContext(t), id); !errors.Is(err, ErrInvalid) {
				t.Fatalf("некорректное поле %s принято: %v", field, err)
			}
		}
	}
	for _, name := range []string{"records", "anchors", "locks"} {
		entries, err := os.ReadDir(filepath.Join(root, evidenceDirectory, name))
		if err != nil || len(entries) != 0 {
			t.Fatalf("отклонённая identity создала %s: %v: %v", name, entries, err)
		}
	}
	id := testFileID("payload", RegularFile)
	id.FileID, id.GenerationID, id.Owner = strings.Repeat("f", 512), strings.Repeat("g", 512), strings.Repeat("o", 4096)
	if err := j.Register(testContext(t), id); err != nil {
		t.Fatal("допустимая граничная identity отклонена", err)
	}
	requireFileContent(t, root, "payload", "preserved")
	if err := j.Verify(testContext(t), id); err != nil {
		t.Fatal("граничная identity не сохранила живой anchor", err)
	}
}
