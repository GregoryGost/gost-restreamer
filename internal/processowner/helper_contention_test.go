package processowner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// standaloneHelper учитывает отдельно запущенный helper с собственным lifeline, результатом Wait и
// диагностикой.
type standaloneHelper struct {
	// lifeline позволяет тесту отменить helper закрытием единственного конца записи.
	lifeline *os.File
	// done закрывается после сохранения результата Wait helper.
	done chan struct{}
	// err хранит окончательный результат отдельно запущенного helper.
	err error
	// stderr сохраняет диагностику helper для объяснения преждевременного выхода.
	stderr bytes.Buffer
}

// Запускает отдельный вспомогательный процесс для существующей операции с собственным каналом
// lifeline. При завершении теста закрывает канал и ограниченно ожидает процесс, принудительно завершая
// его при зависании.
func startStandaloneHelper(t *testing.T, s *Scope, id string, files []*os.File) *standaloneHelper {
	t.Helper()
	// reader передаётся helper как lifeline FD 3; writer остаётся у теста; err сохраняет результаты
	// создания канала и запуска.
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// h содержит учёт отдельно запущенного helper и его будущий результат.
	h := &standaloneHelper{lifeline: writer, done: make(chan struct{})}
	// cmd запускает helper для точной существующей области и native-операции.
	cmd := exec.Command(s.registry.config.HelperPath, strconv.Itoa(ProtocolVersion), s.registry.config.Root, s.id, id)
	cmd.ExtraFiles = append([]*os.File{reader}, files...)
	cmd.Stderr = &h.stderr
	if err = cmd.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		t.Fatal(err)
	}
	_ = reader.Close()
	go func() {
		// Ждёт отдельный gost-process-helper и публикует его ошибку перед закрытием канала завершения.
		h.err = cmd.Wait()
		close(h.done)
	}()
	t.Cleanup(func() {
		// Закрывает lifeline и ждёт отдельный gost-process-helper, принудительно завершая его при превышении
		// защитного срока проверки.

		_ = writer.Close()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-h.done
			t.Error("helper не завершился после закрытия lifeline")
		}
	})
	return h
}

// Проверяет, что вспомогательный процесс остаётся запущенным в течение контрольного интервала;
// преждевременный выход выводит ошибку и stderr.
func (h *standaloneHelper) requireWaiting(t *testing.T) {
	t.Helper()
	select {
	case <-h.done:
		t.Fatalf("helper завершился до освобождения блокировки: %v; stderr=%s", h.err, h.stderr.String())
	case <-time.After(150 * time.Millisecond):
	}
}

// Ожидает завершения вспомогательного процесса до срока тестового контекста и возвращает ошибку
// процесса.
func (h *standaloneHelper) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-h.done:
		return h.err
	case <-testContext(t).Done():
		t.Fatal("helper не завершился")
		return nil
	}
}

// Создаёт резервирующую запись целевого процесса и удерживает её блокировку. Возвращает идентификатор
// операции и повторно безопасную функцию освобождения, также вызываемую при завершении теста.
func holdNativeReservation(t *testing.T, s *Scope, command Command) (string, func()) {
	t.Helper()
	// ticket содержит предварительную native-резервацию; err сохраняет результаты её создания и
	// получения блокировки.
	ticket, err := s.register(testContext(t), "native", &command, nil)
	if err != nil {
		t.Fatal(err)
	}
	// lock намеренно удерживает блокировку native-операции до разрешения теста.
	lock, err := lockExisting(testContext(t), s.registry.root, operationPath(s.id, ticket.id)+".lock", true)
	if err != nil {
		t.Fatal(err)
	}
	// release снимает удержанную блокировку один раз и обнуляет локальный дескриптор.
	release := func() {
		// Освобождает удержанную блокировку нативного резервирования и обнуляет дескриптор, чтобы повтор не
		// освобождал её снова.

		if lock != nil {
			// err сообщает об отказе освобождения блокировки native-резервации.
			if err := unlock(lock); err != nil {
				t.Error(err)
			}
			lock = nil
		}
	}
	t.Cleanup(release)
	return ticket.id, release
}

// Сверяет сохранённое состояние конкретной операции с ожидаемым и завершает тест при ошибке чтения.
func requireOperationState(t *testing.T, s *Scope, id, state string) {
	t.Helper()
	// op содержит сохранённое состояние проверяемой операции; err сообщает об отказе его чтения.
	op, err := s.readOperation(id)
	if err != nil || op.State != state {
		t.Fatalf("состояние операции=%+v, ожидается %s: %v", op, state, err)
	}
}

// Проверяет ожидание блокировок вспомогательным процессом, отмену через lifeline и повторную проверку
// состояния после ожидания. Дубликаты и закрытые, отсутствующие либо изменённые операции не должны
// запускать целевой процесс повторно.
func TestHelperOperationContention(t *testing.T) {
	// helper задаёт настоящий бинарник для всех сценариев конкуренции операций.
	helper := buildHelper(t)
	t.Run("reader-delays-start-without-losing-reservation", func(t *testing.T) {
		// Проверяет ожидание занятой операции, единственный запуск целевого процесса и отклонение ожидающих,
		// отменённых и поздних дубликатов gost-process-helper.

		// r содержит изолированный реестр проверки одного запуска при нескольких helper.
		r := testRegistry(t, helper)
		// s содержит область общей native-резервации.
		s := testScope(t, r)
		// readyRead принимает готовность целевого процесса; readyWrite передаётся ему как FD 3;
		// err сохраняет результаты каналов и проверок запуска.
		readyRead, readyWrite, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			// Закрывает оба дескриптора канала готовности целевого процесса при возврате из проверки.
			_ = readyRead.Close()
			_ = readyWrite.Close()
		}()
		if err = readyRead.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		// controlRead передаётся цели как FD 4; controlWrite разрешает её завершение после
		// проверки дубликатов.
		controlRead, controlWrite, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			// Закрывает оба дескриптора канала управления целевым процессом при возврате из проверки.
			_ = controlRead.Close()
			_ = controlWrite.Close()
		}()
		// marker задаёт файл подсчёта фактических запусков целевого процесса.
		marker := filepath.Join(t.TempDir(), "native-starts")
		// files содержит дескрипторы готовности и управления, передаваемые каждому претендующему
		// helper.
		files := []*os.File{readyWrite, controlRead}
		// command задаёт целевой процесс с записью одной метки запуска и ожиданием управляющего
		// EOF.
		command := Command{Binary: "/bin/sh", Args: []string{"-c", `printf x >> "$1"; printf r >&3; read -r ignored <&4; exit 0`, "native", marker}, ExtraFiles: files}
		// id содержит номер native-резервации; release разрешает первому helper получить её
		// блокировку.
		id, release := holdNativeReservation(t, s, command)
		// first содержит первоначальный helper, который должен единолично запустить целевой
		// процесс.
		first := startStandaloneHelper(t, s, id, files)
		first.requireWaiting(t)
		requireOperationState(t, s, id, reserved)
		if _, err = os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("native запущен при занятой блокировке: %v", err)
		}
		release()
		// ready принимает контрольный байт r после освобождения блокировки и фактического Start.
		var ready [1]byte
		if _, err = io.ReadFull(readyRead, ready[:]); err != nil || ready[0] != 'r' {
			t.Fatalf("native не запущен после освобождения блокировки: %v", err)
		}
		requireOperationState(t, s, id, spawned)
		// duplicate содержит helper-дубликат, ожидающий завершения первоначальной
		// native-операции.
		duplicate := startStandaloneHelper(t, s, id, files)
		duplicate.requireWaiting(t)
		// cancelledDuplicate содержит отдельный дубликат, отменяемый через собственный lifeline
		// при ожидании блокировки.
		cancelledDuplicate := startStandaloneHelper(t, s, id, files)
		cancelledDuplicate.requireWaiting(t)
		_ = cancelledDuplicate.lifeline.Close()
		if err = cancelledDuplicate.wait(t); err == nil {
			t.Fatal("отменённый дубликат helper принят")
		}
		first.requireWaiting(t)
		requireOperationState(t, s, id, spawned)
		if err = controlWrite.Close(); err != nil {
			t.Fatal(err)
		}
		if err = first.wait(t); err != nil {
			t.Fatalf("helper потерял результат native: %v; stderr=%s", err, first.stderr.String())
		}
		if err = duplicate.wait(t); err == nil {
			t.Fatal("ожидавший дубликат запустил native повторно")
		}
		requireOperationState(t, s, id, joined)
		// late содержит запоздалый helper для уже завершённой операции.
		late := startStandaloneHelper(t, s, id, files)
		if err = late.wait(t); err == nil {
			t.Fatal("повторно принята завершённая операция")
		}
		// starts содержит байты контрольного файла для проверки единственного фактического
		// запуска.
		starts, err := os.ReadFile(marker)
		if err != nil || string(starts) != "x" {
			t.Fatalf("native должен запуститься ровно один раз: %q %v", starts, err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		// proof содержит итог сверки завершённой операции и закрытой области.
		proof, err := s.Resolve(testContext(t))
		if err != nil || !proof.Joined || proof.Unknown {
			t.Fatalf("физическое завершение=%+v %v", proof, err)
		}
	})
	t.Run("lifeline-cancels-waiting-helper", func(t *testing.T) {
		// Проверяет отмену ожидающего блокировку gost-process-helper через lifeline без запуска целевого
		// процесса и с последующим закрытием резервирования.

		// r содержит реестр проверки отмены helper при ожидании блокировки native-операции.
		r := testRegistry(t, helper)
		// s содержит область ещё не запущенной native-резервации.
		s := testScope(t, r)
		// marker задаёт файл, который не должен создать отменённый helper.
		marker := filepath.Join(t.TempDir(), "must-not-start")
		// id содержит номер резервации; release снимает блокировку после проверки отмены helper.
		id, release := holdNativeReservation(t, s, Command{Binary: "/bin/touch", Args: []string{marker}})
		// h содержит ожидающий блокировку helper с собственным отменяемым lifeline.
		h := startStandaloneHelper(t, s, id, nil)
		h.requireWaiting(t)
		_ = h.lifeline.Close()
		// err содержит ожидаемый отказ helper после закрытия lifeline.
		if err := h.wait(t); err == nil {
			t.Fatal("отмена ожидания не отклонила helper")
		}
		requireOperationState(t, s, id, reserved)
		// err содержит результат проверки отсутствия целевого файла после отмены.
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("отменённый helper запустил native: %v", err)
		}
		release()
		// err сообщает об отказе закрытия области после освобождения блокировки.
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		// proof содержит доказательство безопасно закрытой резервации; err сообщает об отказе
		// сверки.
		proof, err := s.Resolve(context.Background())
		if err != nil || !proof.Joined || proof.Unknown {
			t.Fatalf("закрытая reservation=%+v %v", proof, err)
		}
	})
	t.Run("lifeline-cancels-gate-wait-with-closed-reservation", func(t *testing.T) {
		// Проверяет отмену gost-process-helper во время ожидания допуска области с записью CLOSED и
		// отсутствием целевого процесса.

		// r содержит реестр проверки отмены helper при ожидании gate области.
		r := testRegistry(t, helper)
		// s содержит область проверяемого допуска регистрации.
		s := testScope(t, r)
		// marker задаёт файл, который не должен создать ожидающий gate helper.
		marker := filepath.Join(t.TempDir(), "must-not-start")
		// id содержит номер native-резервации; release первоначально освобождает её собственную
		// блокировку.
		id, release := holdNativeReservation(t, s, Command{Binary: "/bin/touch", Args: []string{marker}})
		release()
		// gate удерживает общий допуск области; err сохраняет результаты его получения и
		// проверки отмены helper.
		gate, err := lockExisting(testContext(t), r.root, scopePath(s.id)+"/gate.lock", true)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			// Освобождает блокировку допуска области после проверки отмены ожидающего gost-process-helper.

			// err сообщает об отказе резервного освобождения общего gate области.
			if err := unlock(gate); err != nil {
				t.Error(err)
			}
		}()
		// h содержит helper, задержанный занятым gate и отменяемый через lifeline.
		h := startStandaloneHelper(t, s, id, nil)
		h.requireWaiting(t)
		_ = h.lifeline.Close()
		if err = h.wait(t); err == nil {
			t.Fatal("отмена ожидания gate не отклонила helper")
		}
		requireOperationState(t, s, id, closed)
		if _, err = os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("отменённый helper запустил native: %v", err)
		}
	})
	// state выбирает закрытое, отмеченное запуском, завершённое, отсутствующее либо запечатанное
	// состояние после ожидания.
	for _, state := range []string{closed, spawned, joined, "missing", "sealed"} {
		t.Run("fresh-state-after-wait/"+state, func(t *testing.T) {
			// Проверяет, что после ожидания блокировки gost-process-helper заново читает выбранное изменённое
			// состояние и не запускает целевой процесс.

			// r содержит реестр выбранной проверки повторного чтения состояния после
			// ожидания.
			r := testRegistry(t, helper)
			// s содержит область с изменяемой во время ожидания native-резервацией.
			s := testScope(t, r)
			// marker задаёт недопустимый побочный файл, отсутствие которого доказывает
			// запрет позднего Start.
			marker := filepath.Join(t.TempDir(), "must-not-start")
			// id содержит номер операции; release разрешает helper перечитать изменённое
			// состояние.
			id, release := holdNativeReservation(t, s, Command{Binary: "/bin/touch", Args: []string{marker}})
			// h содержит helper, первоначально задержанный блокировкой операции.
			h := startStandaloneHelper(t, s, id, nil)
			h.requireWaiting(t)
			// op содержит исходную резервацию; err сохраняет результаты моделирования
			// выбранного состояния и ожидания отказа.
			op, err := s.readOperation(id)
			if err != nil {
				t.Fatal(err)
			}
			// name задаёт путь JSON-записи операции для изменения либо временного удаления.
			name := operationPath(s.id, id) + ".json"
			switch state {
			case "missing":
				if err = r.root.Remove(name); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					// Восстанавливает удалённую проверкой запись операции перед завершением области.

					// err сообщает об отказе восстановления удалённой тестом записи
					// перед очисткой области.
					if err := writeRecord(r.root, name, op, r.config.Limits.MaxRecordBytes); err != nil {
						t.Error(err)
					}
				})
			case "sealed":
				if err = s.Seal(testContext(t)); err != nil {
					t.Fatal(err)
				}
			default:
				op.State = state
				if err = writeRecord(r.root, name, op, r.config.Limits.MaxRecordBytes); err != nil {
					t.Fatal(err)
				}
			}
			release()
			if err = h.wait(t); err == nil {
				t.Fatalf("недопустимое состояние %s принято после ожидания", state)
			}
			switch state {
			case "missing":
				if _, err = r.root.Stat(name); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("отсутствующая reservation восстановлена helper: %v", err)
				}
			case "sealed":
				requireOperationState(t, s, id, closed)
			default:
				requireOperationState(t, s, id, state)
			}
			if _, err = os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("недопустимое состояние %s запустило native: %v", state, err)
			}
		})
	}
}
