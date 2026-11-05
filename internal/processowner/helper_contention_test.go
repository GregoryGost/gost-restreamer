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

type standaloneHelper struct {
	lifeline *os.File
	done     chan struct{}
	err      error
	stderr   bytes.Buffer
}

func startStandaloneHelper(t *testing.T, s *Scope, id string, files []*os.File) *standaloneHelper {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	h := &standaloneHelper{lifeline: writer, done: make(chan struct{})}
	cmd := exec.Command(s.registry.config.HelperPath, strconv.Itoa(ProtocolVersion), s.registry.config.Root, s.id, id)
	cmd.ExtraFiles = append([]*os.File{reader}, files...)
	cmd.Stderr = &h.stderr
	if err = cmd.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		t.Fatal(err)
	}
	_ = reader.Close()
	go func() { h.err = cmd.Wait(); close(h.done) }()
	t.Cleanup(func() {
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

func (h *standaloneHelper) requireWaiting(t *testing.T) {
	t.Helper()
	select {
	case <-h.done:
		t.Fatalf("helper завершился до освобождения блокировки: %v; stderr=%s", h.err, h.stderr.String())
	case <-time.After(150 * time.Millisecond):
	}
}

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

func holdNativeReservation(t *testing.T, s *Scope, command Command) (string, func()) {
	t.Helper()
	ticket, err := s.register(testContext(t), "native", &command, nil)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := lockExisting(testContext(t), s.registry.root, operationPath(s.id, ticket.id)+".lock", true)
	if err != nil {
		t.Fatal(err)
	}
	release := func() {
		if lock != nil {
			if err := unlock(lock); err != nil {
				t.Error(err)
			}
			lock = nil
		}
	}
	t.Cleanup(release)
	return ticket.id, release
}

func requireOperationState(t *testing.T, s *Scope, id, state string) {
	t.Helper()
	op, err := s.readOperation(id)
	if err != nil || op.State != state {
		t.Fatalf("состояние операции=%+v, ожидается %s: %v", op, state, err)
	}
}

func TestHelperOperationContention(t *testing.T) {
	helper := buildHelper(t)
	t.Run("reader-delays-start-without-losing-reservation", func(t *testing.T) {
		r := testRegistry(t, helper)
		s := testScope(t, r)
		readyRead, readyWrite, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = readyRead.Close(); _ = readyWrite.Close() }()
		if err = readyRead.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		controlRead, controlWrite, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = controlRead.Close(); _ = controlWrite.Close() }()
		marker := filepath.Join(t.TempDir(), "native-starts")
		files := []*os.File{readyWrite, controlRead}
		command := Command{Binary: "/bin/sh", Args: []string{"-c", `printf x >> "$1"; printf r >&3; read -r ignored <&4; exit 0`, "native", marker}, ExtraFiles: files}
		id, release := holdNativeReservation(t, s, command)
		first := startStandaloneHelper(t, s, id, files)
		first.requireWaiting(t)
		requireOperationState(t, s, id, reserved)
		if _, err = os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("native запущен при занятой блокировке: %v", err)
		}
		release()
		var ready [1]byte
		if _, err = io.ReadFull(readyRead, ready[:]); err != nil || ready[0] != 'r' {
			t.Fatalf("native не запущен после освобождения блокировки: %v", err)
		}
		requireOperationState(t, s, id, spawned)
		duplicate := startStandaloneHelper(t, s, id, files)
		duplicate.requireWaiting(t)
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
		late := startStandaloneHelper(t, s, id, files)
		if err = late.wait(t); err == nil {
			t.Fatal("повторно принята завершённая операция")
		}
		starts, err := os.ReadFile(marker)
		if err != nil || string(starts) != "x" {
			t.Fatalf("native должен запуститься ровно один раз: %q %v", starts, err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		proof, err := s.Resolve(testContext(t))
		if err != nil || !proof.Joined || proof.Unknown {
			t.Fatalf("физическое завершение=%+v %v", proof, err)
		}
	})
	t.Run("lifeline-cancels-waiting-helper", func(t *testing.T) {
		r := testRegistry(t, helper)
		s := testScope(t, r)
		marker := filepath.Join(t.TempDir(), "must-not-start")
		id, release := holdNativeReservation(t, s, Command{Binary: "/bin/touch", Args: []string{marker}})
		h := startStandaloneHelper(t, s, id, nil)
		h.requireWaiting(t)
		_ = h.lifeline.Close()
		if err := h.wait(t); err == nil {
			t.Fatal("отмена ожидания не отклонила helper")
		}
		requireOperationState(t, s, id, reserved)
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("отменённый helper запустил native: %v", err)
		}
		release()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		proof, err := s.Resolve(context.Background())
		if err != nil || !proof.Joined || proof.Unknown {
			t.Fatalf("закрытая reservation=%+v %v", proof, err)
		}
	})
	t.Run("lifeline-cancels-gate-wait-with-closed-reservation", func(t *testing.T) {
		r := testRegistry(t, helper)
		s := testScope(t, r)
		marker := filepath.Join(t.TempDir(), "must-not-start")
		id, release := holdNativeReservation(t, s, Command{Binary: "/bin/touch", Args: []string{marker}})
		release()
		gate, err := lockExisting(testContext(t), r.root, scopePath(s.id)+"/gate.lock", true)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := unlock(gate); err != nil {
				t.Error(err)
			}
		}()
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
	for _, state := range []string{closed, spawned, joined, "missing", "sealed"} {
		t.Run("fresh-state-after-wait/"+state, func(t *testing.T) {
			r := testRegistry(t, helper)
			s := testScope(t, r)
			marker := filepath.Join(t.TempDir(), "must-not-start")
			id, release := holdNativeReservation(t, s, Command{Binary: "/bin/touch", Args: []string{marker}})
			h := startStandaloneHelper(t, s, id, nil)
			h.requireWaiting(t)
			op, err := s.readOperation(id)
			if err != nil {
				t.Fatal(err)
			}
			name := operationPath(s.id, id) + ".json"
			switch state {
			case "missing":
				if err = r.root.Remove(name); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
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
