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

// Создаёт исполняемый файл подставной вспомогательной программы, сразу возвращающей успех; целевая
// команда этой подстановкой не запускается.
func boundaryHelperPath(t *testing.T) string {
	t.Helper()
	// name задаёт временный путь helper-подстановки, которая сразу возвращает успех без целевого
	// запуска.
	name := filepath.Join(t.TempDir(), "helper")
	// err сообщает об отказе записи исполняемой helper-подстановки.
	if err := os.WriteFile(name, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return name
}

// Проверяет отклонение недопустимых бюджетов, путей и файла вспомогательной программы до создания
// корней журнала и рабочих файлов.
func TestRegistryRejectsConfigurationBeforeCreatingRoots(t *testing.T) {
	// helper задаёт допустимый исходный путь подставной вспомогательной программы.
	helper := boundaryHelperPath(t)
	// cwd содержит рабочий каталог для проверки относительных путей; err сообщает об отказе его
	// определения.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// relative преобразует абсолютный путь в относительный для намеренно недопустимой конфигурации.
	relative := func(name string) string {
		// Получает путь относительно текущего каталога для проверки отказа конфигурации с относительным путём.

		t.Helper()
		// rel содержит вычисленный относительный путь; err сообщает об отказе его построения.
		rel, err := filepath.Rel(cwd, name)
		if err != nil {
			t.Fatal(err)
		}
		return rel
	}
	// name выбирает проверяемое нарушение бюджета, срока, пути либо helper-файла.
	for _, name := range []string{"operation-budget", "record-budget", "command-budget", "descriptor-budget", "page-budget", "grace-period", "relative-root", "relative-runtime", "relative-helper", "missing-helper", "directory-helper", "nonexecutable-helper"} {
		t.Run(name, func(t *testing.T) {
			// Проверяет выбранную недопустимую конфигурацию Registry до создания корней журнала и временных
			// ресурсов.

			// base задаёт отдельный каталог конкретной конфигурационной проверки.
			base := t.TempDir()
			// config содержит исходно допустимые настройки, изменяемые выбранным нарушением.
			config := Config{Root: filepath.Join(base, "journal"), RuntimeRoot: filepath.Join(base, "runtime"), HelperPath: helper, Limits: testLimits(), GracePeriod: time.Millisecond}
			// want задаёт ожидаемую причину отказа конструктора для выбранной конфигурации.
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
				// err сообщает об отказе создания неисполняемого helper-файла
				// контрольного сценария.
				if err := os.WriteFile(config.HelperPath, []byte("preserved"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// r содержит неожиданный реестр при недопустимой конфигурации; err содержит
			// проверяемый отказ New.
			r, err := New(config)
			if r != nil {
				_ = r.Close()
				t.Fatal("некорректная конфигурация создала Registry")
			}
			if !errors.Is(err, want) {
				t.Fatalf("отказ конфигурации=%v, ожидается %v", err, want)
			}
			// root содержит очередной предполагаемый корень, который не должен быть создан
			// отвергнутой конфигурацией.
			for _, root := range []string{filepath.Join(base, "journal"), filepath.Join(base, "runtime")} {
				// err содержит результат проверки отсутствия корня после
				// конфигурационного отказа.
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

// Проверяет, что файл или символическая ссылка вместо каталога доказательств отклоняются без записи
// через ссылку и изменения исходных объектов.
func TestFileJournalRejectsUnsupportedEvidenceRoots(t *testing.T) {
	// name выбирает обычный файл либо символическую ссылку вместо каталога доказательств.
	for _, name := range []string{"file", "symlink"} {
		t.Run(name, func(t *testing.T) {
			// Проверяет отказ файлового журнала при обычном файле либо символической ссылке на месте каталога
			// доказательств без записи во внешний каталог.

			// root задаёт корень журнала; outside задаёт защищаемый внешний каталог ссылки.
			root, outside := t.TempDir(), t.TempDir()
			testFile(t, outside, "sentinel", "outside preserved")
			if name == "file" {
				testFile(t, root, evidenceDirectory, "occupied evidence path")
			} else /* err сообщает об отказе создания ссылки на внешний каталог вместо evidence-корня. */ if err := os.Symlink(outside, filepath.Join(root, evidenceDirectory)); err != nil {
				t.Fatal(err)
			}
			// j содержит неожиданный журнал при неподдержанном evidence-корне; err сохраняет
			// результат конструктора и проверки внешнего каталога.
			j, err := NewFileJournal(root, testLimits())
			if j != nil {
				_ = j.Close()
				t.Fatal("неподдержанный корень доказательств принят")
			}
			if !errors.Is(err, ErrUnknown) {
				t.Fatalf("неподдержанный корень=%v", err)
			}
			requireFileContent(t, outside, "sentinel", "outside preserved")
			// entries содержит содержимое внешнего каталога для проверки отсутствия записей
			// через ссылку.
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

// Создаёт реестр с подставной вспомогательной программой и закрытую область с зарегистрированным
// файлом для проверки восстановления.
func boundaryClosedScope(t *testing.T) (*Registry, *Scope, FileIdentity, string) {
	t.Helper()
	// r содержит изолированный реестр с helper-подстановкой для проверки повреждений без
	// native-запуска.
	r := testRegistry(t, boundaryHelperPath(t))
	// s содержит закрытую область; id содержит её рабочий файл; opID содержит номер последней
	// операции для управляемого повреждения.
	s, id, opID := boundaryScopeWithPayload(t, r)
	return r, s, id, opID
}

// Создаёт область и зарегистрированный рабочий файл, завершает операцию записи и закрывает область.
// Возвращает идентичность файла и номер последней операции для управляемого повреждения журнала.
func boundaryScopeWithPayload(t *testing.T, r *Registry) (*Scope, FileIdentity, string) {
	t.Helper()
	// s содержит область, в которой создаётся защищаемый рабочий файл.
	s := testScope(t, r)
	// ctx ограничивает регистрацию, завершение построителя и закрытие области.
	ctx := testContext(t)
	// ticket учитывает создание рабочего файла; err сохраняет результаты его регистрации и закрытия.
	ticket, err := s.Register(ctx, "file-builder")
	if err != nil {
		t.Fatal(err)
	}
	testFile(t, s.Namespace(), "payload", "protected runtime payload")
	if err = s.RegisterRuntime(ctx, "payload", RegularFile); err != nil {
		t.Fatal(err)
	}
	// id содержит точную runtime-идентичность защищаемого payload.
	id, err := s.RuntimeIdentity("payload", RegularFile)
	if err != nil {
		t.Fatal(err)
	}
	// h содержит полный заголовок для вычисления номера последней зарегистрированной операции.
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

// Читает исходные байты записи журнала и регистрирует их восстановление при завершении теста.
func boundarySavedRecord(t *testing.T, root *os.Root, name string) []byte {
	t.Helper()
	// b сохраняет исходные байты проверяемой записи; err сообщает об отказе их чтения.
	b, err := root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Восстанавливает исходные байты изменяемой записи журнала после проверки повреждения.

		// err сообщает об отказе восстановления исходных байтов записи при очистке теста.
		if err := root.WriteFile(name, b, 0600); err != nil {
			t.Error(err)
		}
	})
	return b
}

// Проверяет сохранность рабочего файла и независимого файлового доказательства и запрещает
// подтверждение повреждённой области.
func boundaryRequireProtectedRuntime(t *testing.T, r *Registry, s *Scope, id FileIdentity) {
	t.Helper()
	requireFileContent(t, s.Namespace(), "payload", "protected runtime payload")
	// err сообщает о повреждении независимого файлового доказательства рабочего payload.
	if err := r.runtime.Verify(testContext(t), id); err != nil {
		t.Fatal("повреждение scope уничтожило независимое файловое доказательство", err)
	}
	// h содержит текущий заголовок области; err сообщает о его недостоверности при проверке запрета
	// Acknowledged.
	h, err := s.header()
	if err == nil && h.Acknowledged {
		t.Fatal("повреждённая область получила подтверждение очистки")
	}
}

// Проверяет восстановленное Joined, удаление рабочего пространства и последующее долговечное
// подтверждение области.
func boundaryRequireRestoredScope(t *testing.T, r *Registry, s *Scope) {
	t.Helper()
	// ctx ограничивает восстановление и окончательное подтверждение области.
	ctx := testContext(t)
	// proof содержит результат сверки до и после AcknowledgeScope; err сохраняет результаты
	// восстановления и подтверждения.
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

// Проверяет, что повреждённый заголовок области не доказывает завершение и не разрешает очистку.
// Восстановление исходного заголовка вновь позволяет завершить тот же протокол.
func TestDamagedScopeHeaderPreservesRuntimeAndAcknowledgement(t *testing.T) {
	// damage выбирает повреждение версии, идентичности, runtime-корня, каталога либо JSON заголовка.
	for _, damage := range []string{"protocol", "scope-id", "runtime-root", "negative-catalog", "unbounded-catalog", "json", "oversized"} {
		t.Run(damage, func(t *testing.T) {
			// Повреждает выбранное поле заголовка области и проверяет сохранность временного файла и отказ
			// подтверждения до восстановления записи.

			// r содержит реестр; s содержит закрытую область; identity содержит защищаемый
			// runtime-файл выбранного сценария.
			r, s, identity, _ := boundaryClosedScope(t)
			// name задаёт путь повреждаемого заголовка области.
			name := scopePath(s.ID()) + "/header.json"
			// original сохраняет исходный заголовок для восстановления того же протокола.
			original := boundarySavedRecord(t, r.root, name)
			// h содержит изменяемый заголовок; err сохраняет результаты моделирования
			// повреждения и последующих проверок.
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
			// proof содержит результат отказа сверки повреждённого заголовка.
			proof, err := s.Resolve(testContext(t))
			if !errors.Is(err, ErrUnknown) || !proof.Unknown || proof.Joined || proof.Acknowledged {
				t.Fatalf("повреждённый заголовок дал доказательство=%+v: %v", proof, err)
			}
			// opened содержит недопустимо открытую область; err содержит ожидаемый
			// ErrUnknown повреждённого заголовка.
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

// Проверяет сохранение рабочих файлов при неполном или недостоверном каталоге операций, включая
// подменённого владельца и недоступную блокировку. После восстановления записей очистка должна
// завершиться.
func TestIncompleteScopeCatalogCannotJoinOrRemoveRuntime(t *testing.T) {
	// damage выбирает отсутствующую или чужую операцию, неизвестную стадию, блокировку либо
	// владельца runtime-файла.
	for _, damage := range []string{"missing-record", "record-json", "foreign-scope", "foreign-operation", "unsupported-state", "spawn-without-wait", "missing-lock", "symlink-lock", "foreign-runtime-owner"} {
		t.Run(damage, func(t *testing.T) {
			// Моделирует выбранную неполноту каталога операций и проверяет UNKNOWN без удаления временного файла и
			// подтверждения области.

			// r содержит реестр; s содержит область; identity содержит защищаемый файл; opID
			// задаёт повреждаемую операцию.
			r, s, identity, opID := boundaryClosedScope(t)
			// name задаёт точный путь записи повреждаемой операции.
			name := operationPath(s.ID(), opID) + ".json"
			// original сохраняет байты исходной операции для последующего восстановления.
			original := boundarySavedRecord(t, r.root, name)
			// op содержит изменяемую операцию; err сохраняет результаты повреждения и сверки
			// полного каталога.
			op, err := s.readOperation(opID)
			if err != nil {
				t.Fatal(err)
			}
			// lockName задаёт путь блокировки операции, которая может быть временно удалена
			// либо подменена ссылкой.
			lockName := operationPath(s.ID(), opID) + ".lock"
			// restoreLock отмечает необходимость вернуть исходную блокировку при резервной
			// очистке.
			restoreLock := false
			if damage == "missing-lock" || damage == "symlink-lock" {
				if err = r.root.Rename(lockName, lockName+".saved"); err != nil {
					t.Fatal(err)
				}
				restoreLock = true
				t.Cleanup(func() {
					// Восстанавливает исходную блокировку операции, если проверка ещё не вернула её явно после удаления
					// либо подмены ссылкой.

					if !restoreLock {
						return
					}
					_ = r.root.Remove(lockName)
					// err сообщает об отказе резервного возврата исходного файла
					// блокировки.
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
			// proof содержит результат сверки неполного каталога для проверки Unknown без
			// удаления runtime-файла.
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

// Проверяет ограниченный обход здоровой и повреждённых областей без потери здорового результата,
// повторов и удаления неизвестных каталогов; также проверяет неверные курсоры и отмену.
func TestRecoveryPagingPreservesUnknownScopesAndFindsHealthyScope(t *testing.T) {
	// r содержит реестр; s содержит здоровую закрытую область среди неизвестных соседей.
	r, s, _, _ := boundaryClosedScope(t)
	// ctx ограничивает постраничное восстановление неизменного каталога областей.
	ctx := testContext(t)
	// corruptID задаёт синтаксически допустимую область с повреждённым заголовком.
	corruptID := strings.Repeat("c", 32)
	// id содержит имя добавляемого неизвестного каталога области.
	for _, id := range []string{"unrecognized-entry", corruptID} {
		// err сообщает об отказе создания контрольного неизвестного каталога.
		if err := r.root.Mkdir(scopePath(id), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// err сообщает об отказе записи повреждённого JSON-заголовка контрольной области.
	if err := r.root.WriteFile(scopePath(corruptID)+"/header.json", []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	// seen сохраняет единственный результат каждой встреченной области по её имени.
	seen := map[string]ScopeResult{}
	// after содержит текущий курсор ограниченного обхода каталога.
	after := ""
	// calls считает запросы страниц для проверки конечного завершения обхода.
	for calls := 0; ; calls++ {
		if calls > 3 {
			t.Fatal("ограниченная пагинация не завершилась")
		}
		// page содержит очередной результат страницы; next задаёт продолжение; err сообщает об
		// отказе восстановления.
		page, next, err := r.RecoverPage(ctx, after, 1)
		if err != nil || len(page) > 1 {
			t.Fatalf("страница=%+v: %v", page, err)
		}
		// item содержит очередную область ограниченной страницы.
		for _, item := range page {
			// duplicate показывает, встречалась ли эта область ранее в неизменном каталоге.
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
	// id содержит очередной неизвестный каталог для проверки его сохранности и признака Unknown.
	for _, id := range []string{"unrecognized-entry", corruptID} {
		if !seen[id].Unknown || seen[id].Joined || seen[id].Acknowledged {
			t.Fatalf("неизвестная область подтверждена: %+v", seen[id])
		}
		// err сообщает об исчезновении неизвестного каталога после восстановления.
		if _, err := r.root.Stat(scopePath(id)); err != nil {
			t.Fatal("неизвестная область удалена", err)
		}
	}
	// after содержит очередной недопустимый курсор каталога для проверки отказа.
	for _, after := range []string{"-1", "invalid"} {
		// page содержит неожиданный результат неверного курсора; err содержит ожидаемый
		// ErrInvalid.
		if page, _, err := r.RecoverPage(ctx, after, 1); len(page) != 0 || !errors.Is(err, ErrInvalid) {
			t.Fatalf("некорректный курсор %q дал результат: %+v: %v", after, page, err)
		}
	}
	// page содержит результат запроса за концом каталога; next должен быть пустым; err сообщает о
	// неожиданном отказе.
	if page, next, err := r.RecoverPage(ctx, "100", 1); err != nil || len(page) != 0 || next != "" {
		t.Fatalf("курсор за концом каталога: %+v %q: %v", page, next, err)
	}
	// cancelled передаёт уже отменённое восстановление; cancel инициирует отмену до чтения страницы.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	// page содержит неожиданные доказательства после отмены; err содержит ожидаемый
	// context.Canceled.
	if page, _, err := r.RecoverPage(cancelled, "", 1); len(page) != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("отменённое восстановление вернуло доказательство: %+v: %v", page, err)
	}
}

// Проверяет, что отменённые регистрация, завершение и подтверждение не переписывают каталог и не
// выдают Joined для занятой операции. Разрешённое Seal и последующий повтор завершают протокол.
func TestScopeLockCancellationPreservesCatalogAndPendingWork(t *testing.T) {
	// r содержит реестр проверки отменённых операций при занятых блокировках.
	r := testRegistry(t, boundaryHelperPath(t))
	// s содержит область с незавершённым backend-построителем.
	s := testScope(t, r)
	// ctx ограничивает подготовку блокировок и окончательные повторы завершения.
	ctx := testContext(t)
	// ticket содержит незавершённый построитель; err сохраняет результаты регистрации, отменённых
	// операций и восстановления.
	ticket, err := s.Register(ctx, "pending-builder")
	if err != nil {
		t.Fatal(err)
	}
	// headerName задаёт путь заголовка для проверки отсутствия лишних изменений.
	headerName := scopePath(s.ID()) + "/header.json"
	// opName задаёт путь записи построителя для сравнения исходных байтов после отмены.
	opName := operationPath(s.ID(), ticket.id) + ".json"
	// header сохраняет исходные байты заголовка до отменённых операций и разрешённого Seal.
	header := boundarySavedRecord(t, r.root, headerName)
	// op сохраняет исходные байты результата операции до отменённого Complete.
	op := boundarySavedRecord(t, r.root, opName)
	// cancelled передаёт уже отменённые регистрацию, Seal, Complete и подтверждение; cancel
	// инициирует отмену до вызовов.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	// gate намеренно удерживает общий допуск для проверки отменённой регистрации и Seal.
	gate, err := lockExisting(ctx, r.root, scopePath(s.ID())+"/gate.lock", true)
	if err != nil {
		t.Fatal(err)
	}
	// late содержит неожиданно созданный Ticket; err содержит ожидаемую отмену регистрации.
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
	// lock намеренно удерживает блокировку построителя во время отменённого Complete и Resolve.
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
	// proof содержит состояние занятой операции до и после разрешённых повторов завершения.
	proof, err := s.Resolve(ctx)
	if err != nil || proof.Joined || proof.Unknown || proof.PendingOperations != 1 || !proof.Sealed {
		_ = unlock(lock)
		t.Fatalf("занятая операция подтверждена: %+v: %v", proof, err)
	}
	if err = unlock(lock); err != nil {
		t.Fatal(err)
	}
	// currentOp содержит байты операции после отменённого Complete для проверки их неизменности.
	currentOp, err := r.root.ReadFile(opName)
	if err != nil || !bytes.Equal(currentOp, op) {
		t.Fatalf("отмена переписала результат операции: %s: %v", currentOp, err)
	}
	if err = r.AcknowledgeScope(cancelled, s.ID()); !errors.Is(err, context.Canceled) {
		t.Fatalf("отменённый ack=%v", err)
	}
	// h содержит текущий заголовок после единственного разрешённого изменения Sealed.
	h, err := s.header()
	if err != nil || h.Count != 2 || h.Acknowledged {
		t.Fatalf("отмена изменила каталог/ack: %+v: %v", h, err)
	}
	// Единственным разрешённым изменением заголовка был явный успешный Seal.
	// prior восстанавливает исходный заголовок для сравнения после ожидаемой установки Sealed.
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

// Проверяет, что закрытые дескрипторы журнала не позволяют создать область, подтвердить завершение или
// удалить рабочие файлы, остающиеся доступными для независимой проверки.
func TestClosedRegistryCannotInventProofOrRemoveRuntime(t *testing.T) {
	// r содержит реестр проверки закрытых дескрипторов; err сохраняет результаты создания и
	// последующих отказов чтения.
	r, err := New(Config{Root: filepath.Join(t.TempDir(), "journal"), HelperPath: boundaryHelperPath(t), Limits: testLimits(), GracePeriod: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// rootClosed отмечает явное закрытие root, исключая повтор при резервной очистке.
	rootClosed := false
	t.Cleanup(func() {
		// Закрывает корневой дескриптор, если он ещё открыт, и файловый журнал после проверки отказов
		// закрытого Registry.

		if !rootClosed {
			// err сообщает об отказе резервного закрытия ещё открытого корня журнала.
			if err := r.root.Close(); err != nil {
				t.Error(err)
			}
		}
		// err сообщает об отказе закрытия независимого файлового журнала при очистке.
		if err := r.runtime.Close(); err != nil {
			t.Error(err)
		}
	})
	// s содержит закрытую область; identity содержит её защищаемый runtime-файл.
	s, identity, _ := boundaryScopeWithPayload(t, r)
	// Закрываются дескрипторы, а сами объекты остаются доступными для независимой проверки через ОС.
	// err сообщает об отказе явного закрытия дескриптора журнала перед проверкой отказов.
	if err := r.root.Close(); err != nil {
		t.Fatal(err)
	}
	rootClosed = true
	// ctx ограничивает операции через уже закрытый корень журнала.
	ctx := testContext(t)
	// created содержит неожиданную новую область; err должен сообщить отказ Create через закрытый
	// корень.
	if created, err := r.Create(ctx, "new-owner"); created != nil || err == nil {
		t.Fatalf("закрытый Registry создал область: %v: %v", created, err)
	}
	// opened содержит неожиданно открытую область; err содержит ожидаемый ErrUnknown закрытого
	// корня.
	if opened, err := r.OpenScope(ctx, s.ID()); opened != nil || !errors.Is(err, ErrUnknown) {
		t.Fatalf("закрытый Registry открыл область: %v: %v", opened, err)
	}
	// proof содержит неизвестное доказательство, полученное через закрытый журнал.
	proof, err := s.Resolve(ctx)
	if !errors.Is(err, ErrUnknown) || !proof.Unknown || proof.Joined || proof.Acknowledged {
		t.Fatalf("закрытый корень подтвердил завершение: %+v: %v", proof, err)
	}
	// page содержит неожиданные результаты восстановления; err должен сообщить отказ чтения
	// закрытого корня.
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

// Проверяет точного владельца корня области и отказ для некорректных или запоздавших рабочих
// идентичностей без расширения каталога и создания файлов.
func TestRuntimeIdentityLimitsRejectRegistrationWithoutArtifacts(t *testing.T) {
	// r содержит реестр проверки границ runtime-идентичности без внешних действий.
	r := testRegistry(t, boundaryHelperPath(t))
	// s содержит область с точным владельцем и исходным каталогом операций.
	s := testScope(t, r)
	// ctx ограничивает построение идентичностей и попытки поздней регистрации.
	ctx := testContext(t)
	// owner содержит сохранённого владельца области; err сохраняет результаты построения и проверки
	// runtime-идентичностей.
	owner, err := s.Owner()
	if err != nil {
		t.Fatal(err)
	}
	// id содержит точную идентичность корневого runtime-каталога области.
	id, err := s.RuntimeIdentity(".", Directory)
	if err != nil || id.FileID != s.ID()+":." || id.GenerationID != s.ID() || id.ManagedPath != s.ID() || id.Owner != owner || id.Kind != Directory {
		t.Fatalf("корень runtime потерял точного владельца: %+v: %v", id, err)
	}
	// h сохраняет исходное число операций до отклонённых runtime-идентичностей.
	h, err := s.header()
	if err != nil {
		t.Fatal(err)
	}
	// item содержит очередные недопустимые имя либо вид runtime-объекта.
	for _, item := range []struct { /* name задаёт проверяемый относительный путь; kind задаёт проверяемый тип runtime-объекта. */
		name, kind string
	}{{"payload", "unsupported"}, {strings.Repeat("x", 4097), RegularFile}, {"bad\xff", RegularFile}} {
		if err = s.RegisterRuntime(ctx, item.name, item.kind); !errors.Is(err, ErrInvalid) {
			t.Fatalf("некорректная runtime identity принята: %v", err)
		}
	}
	// after содержит заголовок после отклонённых регистраций для проверки неизменности Count.
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

// Проверяет ограничения полей FileIdentity без создания доказательств для отклонённых значений и
// успешную регистрацию предельных допустимых значений с сохранением файла.
func TestFileIdentityBoundsPreserveUnregisteredObject(t *testing.T) {
	// root задаёт изолированный каталог проверки границ полей FileIdentity.
	root := t.TempDir()
	// j содержит журнал, который не должен создавать доказательства отклонённых идентичностей.
	j := testJournal(t, root)
	testFile(t, root, "payload", "preserved")
	// field выбирает превышение длины, неверную UTF-8 либо неподдержанный вид объекта.
	for _, field := range []string{"file-id", "generation", "owner", "utf8", "kind"} {
		// id содержит идентичность с выбранным недопустимым полем.
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
		// action содержит очередной метод журнала, проверяемый до создания доказательств.
		for _, action := range []func(context.Context, FileIdentity) error{j.Reserve, j.Register, j.Remove, j.Acknowledge} {
			// err содержит ожидаемый ErrInvalid метода для недопустимого поля.
			if err := action(testContext(t), id); !errors.Is(err, ErrInvalid) {
				t.Fatalf("некорректное поле %s принято: %v", field, err)
			}
		}
	}
	// name содержит очередной служебный каталог для проверки отсутствия побочных записей.
	for _, name := range []string{"records", "anchors", "locks"} {
		// entries содержит его содержимое; err сообщает об отказе проверки пустого каталога.
		entries, err := os.ReadDir(filepath.Join(root, evidenceDirectory, name))
		if err != nil || len(entries) != 0 {
			t.Fatalf("отклонённая identity создала %s: %v: %v", name, entries, err)
		}
	}
	// id содержит предельно допустимые FileID, GenerationID и Owner для регистрации существующего
	// файла.
	id := testFileID("payload", RegularFile)
	id.FileID, id.GenerationID, id.Owner = strings.Repeat("f", 512), strings.Repeat("g", 512), strings.Repeat("o", 4096)
	// err сообщает об отказе регистрации допустимых граничных значений идентичности.
	if err := j.Register(testContext(t), id); err != nil {
		t.Fatal("допустимая граничная identity отклонена", err)
	}
	requireFileContent(t, root, "payload", "preserved")
	// err сообщает об отказе проверки живого anchor при допустимых граничных значениях.
	if err := j.Verify(testContext(t), id); err != nil {
		t.Fatal("граничная identity не сохранила живой anchor", err)
	}
}
