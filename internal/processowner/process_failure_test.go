package processowner

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStartFailureClosesReservationAndRecoversProof(t *testing.T) {
	for _, failure := range []string{"missing-helper", "nonexecutable-helper"} {
		t.Run(failure, func(t *testing.T) {
			helper := filepath.Join(t.TempDir(), "helper")
			if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			r := testRegistry(t, helper)
			s := testScope(t, r)
			expected := os.ErrNotExist
			if failure == "missing-helper" {
				if err := os.Remove(helper); err != nil {
					t.Fatal(err)
				}
			} else {
				expected = syscall.EACCES
				if err := os.Chmod(helper, 0600); err != nil {
					t.Fatal(err)
				}
			}
			marker := filepath.Join(t.TempDir(), "native-effect")
			p, err := s.Start(testContext(t), Command{Binary: "/bin/sh", Args: []string{"-c", `printf x > "$1"`, "native", marker}})
			if p != nil || !errors.Is(err, expected) {
				t.Fatalf("отказ запуска helper: process=%v err=%v", p, err)
			}
			if _, err = os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("native выполнился без helper: %v", err)
			}
			liveProof, err := s.Resolve(testContext(t))
			if err != nil || liveProof.Sealed || liveProof.Joined || liveProof.Unknown || liveProof.PendingOperations != 0 || liveProof.Operations != 3 {
				t.Fatalf("отказ запуска не закрыл reservation до seal: %+v: %v", liveProof, err)
			}
			if err = s.CloseContext(testContext(t)); err != nil {
				t.Fatal("отказ запуска оставил незавершённую backend-операцию", err)
			}
			reopened, err := r.OpenScope(testContext(t), s.ID())
			if err != nil {
				t.Fatal(err)
			}
			proof, err := reopened.Resolve(testContext(t))
			if err != nil || !proof.Sealed || !proof.Joined || proof.Unknown || proof.PendingOperations != 0 || proof.Operations != 3 {
				t.Fatalf("долговечное доказательство после отказа=%+v: %v", proof, err)
			}
			if err = r.AcknowledgeScope(testContext(t), s.ID()); err != nil {
				t.Fatal(err)
			}
			proof, err = reopened.Resolve(testContext(t))
			if err != nil || !proof.Joined || !proof.Acknowledged {
				t.Fatalf("подтверждение восстановленной области=%+v: %v", proof, err)
			}
		})
	}
}

func TestRejectedStartHasNoCatalogOrNativeEffect(t *testing.T) {
	helper, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	r := testRegistry(t, helper)
	s := testScope(t, r)
	marker := filepath.Join(t.TempDir(), "native-effect")
	base := Command{Binary: "/bin/sh", Args: []string{"-c", `printf x > "$1"`, "native", marker}}
	cancelled, cancel := context.WithCancel(testContext(t))
	cancel()
	cases := []struct {
		name    string
		command Command
		ctx     context.Context
		want    error
	}{
		{"cancelled", base, cancelled, context.Canceled},
		{"binary-nul", Command{Binary: "bad\x00"}, testContext(t), ErrInvalid},
		{"argument-utf8", Command{Binary: base.Binary, Args: []string{"bad\xff"}}, testContext(t), ErrInvalid},
		{"too-many-arguments", Command{Binary: base.Binary, Args: make([]string, testLimits().MaxCommandArgs+1)}, testContext(t), ErrLimit},
		{"command-bytes", Command{Binary: strings.Repeat("x", testLimits().MaxCommandBytes+1)}, testContext(t), ErrLimit},
		{"too-many-descriptors", Command{Binary: base.Binary, ExtraFiles: make([]*os.File, testLimits().MaxExtraFiles+1)}, testContext(t), ErrLimit},
		{"nil-descriptor", Command{Binary: base.Binary, ExtraFiles: []*os.File{nil}}, testContext(t), ErrInvalid},
	}
	before, err := s.header()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := s.Start(tc.ctx, tc.command)
			if p != nil || !errors.Is(err, tc.want) {
				t.Fatalf("некорректный запуск: process=%v err=%v", p, err)
			}
			after, err := s.header()
			if err != nil || after.Count != before.Count || after.Sealed {
				t.Fatalf("отклонённый ввод изменил каталог: %+v: %v", after, err)
			}
		})
	}
	if _, err = os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("отклонённая команда создала объект: %v", err)
	}
	if err = s.CloseContext(testContext(t)); err != nil {
		t.Fatal(err)
	}
	proof, err := s.Resolve(testContext(t))
	if err != nil || !proof.Joined || proof.Operations != before.Count {
		t.Fatalf("отклонённый ввод оставил незавершённую операцию: %+v: %v", proof, err)
	}
}

func TestNativeBudgetFailureCompletesBackendTicket(t *testing.T) {
	helper, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	limits := testLimits()
	limits.MaxOperations = 2
	r, err := New(Config{Root: filepath.Join(t.TempDir(), "journal"), HelperPath: helper, Limits: limits, GracePeriod: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	s := testScope(t, r)
	p, err := s.Start(testContext(t), Command{Binary: helper})
	if p != nil || !errors.Is(err, ErrLimit) {
		t.Fatalf("недостаточный бюджет native принят: process=%v err=%v", p, err)
	}
	liveProof, err := s.Resolve(testContext(t))
	if err != nil || liveProof.Sealed || liveProof.Joined || liveProof.Unknown || liveProof.PendingOperations != 0 || liveProof.Operations != 2 {
		t.Fatalf("частичный запуск сохранил незавершённый backend ticket: %+v: %v", liveProof, err)
	}
	if err = s.CloseContext(testContext(t)); err != nil {
		t.Fatal("частичный запуск удерживает backend ticket", err)
	}
	proof, err := s.Resolve(testContext(t))
	if err != nil || !proof.Sealed || !proof.Joined || proof.Unknown || proof.Operations != 2 {
		t.Fatalf("неполная регистрация не завершена: %+v: %v", proof, err)
	}
}

func TestRunCancellationWaitsForPhysicalJoin(t *testing.T) {
	r := testRegistry(t, buildHelper(t))
	s := testScope(t, r)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	if err = reader.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := s.Run(ctx, Command{Binary: os.Args[0], Args: []string{"-test.run=^TestInterruptIgnoringNative$", "--", "--interrupt-ignoring-native"}, ExtraFiles: []*os.File{writer}})
		done <- outcome{result, err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-testContext(t).Done():
			t.Error("Run не завершился после отмены")
		}
	})
	var ready [1]byte
	if _, err = io.ReadFull(reader, ready[:]); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case got := <-done:
		// Результат сохраняется для общей проверки завершения и t.Cleanup.
		done <- got
		if !errors.Is(got.err, context.Canceled) || !got.result.Joined || got.result.ExitCode != -1 {
			t.Fatalf("Run вернулся без фактического Wait: %+v: %v", got.result, got.err)
		}
	case <-testContext(t).Done():
		t.Fatal("Run не дождался завершения native")
	}
	if err = s.CloseContext(testContext(t)); err != nil {
		t.Fatal(err)
	}
	proof, err := s.Resolve(testContext(t))
	if err != nil || !proof.Joined || proof.Unknown || proof.PendingOperations != 0 {
		t.Fatalf("после Run осталась незавершённая операция: %+v: %v", proof, err)
	}
}
