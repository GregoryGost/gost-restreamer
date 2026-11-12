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

// Проверяет отказ запуска отсутствующей или неисполняемой вспомогательной программы без запуска
// целевой команды. Частичные регистрации должны закрыться и после повторного открытия давать
// долговечное доказательство завершения.
func TestStartFailureClosesReservationAndRecoversProof(t *testing.T) {
	// failure выбирает исчезнувший либо лишённый права исполнения helper после создания реестра.
	for _, failure := range []string{"missing-helper", "nonexecutable-helper"} {
		t.Run(failure, func(t *testing.T) {
			// Проверяет отказ выбранного способа запуска gost-process-helper без выполнения целевой команды и с
			// восстановимым завершённым резервированием.

			// helper задаёт временный исполняемый путь, повреждаемый выбранным сценарием.
			helper := filepath.Join(t.TempDir(), "helper")
			// err сообщает об отказе первоначальной записи допустимого helper-скрипта.
			if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			// r содержит реестр, проверивший helper до его удаления или смены прав.
			r := testRegistry(t, helper)
			// s содержит область проверяемого неуспешного запуска.
			s := testScope(t, r)
			// expected задаёт ожидаемую системную ошибку отсутствия файла либо EACCES.
			expected := os.ErrNotExist
			if failure == "missing-helper" {
				// err сообщает об отказе удаления helper для выбранного отсутствующего
				// пути.
				if err := os.Remove(helper); err != nil {
					t.Fatal(err)
				}
			} else {
				expected = syscall.EACCES
				// err сообщает об отказе снятия исполняемых прав helper.
				if err := os.Chmod(helper, 0600); err != nil {
					t.Fatal(err)
				}
			}
			// marker задаёт файл, который целевая команда не должна создать без запущенного
			// helper.
			marker := filepath.Join(t.TempDir(), "native-effect")
			// p содержит результат неуспешного Start и должен быть nil; err сохраняет
			// ожидаемый отказ и результаты восстановления области.
			p, err := s.Start(testContext(t), Command{Binary: "/bin/sh", Args: []string{"-c", `printf x > "$1"`, "native", marker}})
			if p != nil || !errors.Is(err, expected) {
				t.Fatalf("отказ запуска helper: process=%v err=%v", p, err)
			}
			if _, err = os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("native выполнился без helper: %v", err)
			}
			// liveProof содержит состояние закрытых частичных регистраций до Seal области.
			liveProof, err := s.Resolve(testContext(t))
			if err != nil || liveProof.Sealed || liveProof.Joined || liveProof.Unknown || liveProof.PendingOperations != 0 || liveProof.Operations != 3 {
				t.Fatalf("отказ запуска не закрыл reservation до seal: %+v: %v", liveProof, err)
			}
			if err = s.CloseContext(testContext(t)); err != nil {
				t.Fatal("отказ запуска оставил незавершённую backend-операцию", err)
			}
			// reopened содержит заново открытую область для проверки долговечного
			// доказательства отказа запуска.
			reopened, err := r.OpenScope(testContext(t), s.ID())
			if err != nil {
				t.Fatal(err)
			}
			// proof содержит завершённое доказательство области до и после подтверждения
			// владельцем.
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

// Проверяет отклонение отменённой команды, недопустимых строк и превышенных бюджетов до изменения
// каталога и запуска целевого процесса.
func TestRejectedStartHasNoCatalogOrNativeEffect(t *testing.T) {
	// helper содержит найденный true для допустимой конфигурации реестра; err сохраняет результаты
	// поиска и последующих проверок.
	helper, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	// r содержит реестр проверки отказа Start до изменения каталога операций.
	r := testRegistry(t, helper)
	// s содержит область с исходным каталогом перед недопустимыми командами.
	s := testScope(t, r)
	// marker задаёт файл, который не должен появиться при отклонённом Start.
	marker := filepath.Join(t.TempDir(), "native-effect")
	// base содержит допустимую целевую команду для контрольного сценария уже отменённого контекста.
	base := Command{Binary: "/bin/sh", Args: []string{"-c", `printf x > "$1"`, "native", marker}}
	// cancelled задаёт уже отменённый контекст команды; cancel инициирует отмену до Start.
	cancelled, cancel := context.WithCancel(testContext(t))
	cancel()
	// cases содержит недопустимые команды, контексты и ожидаемые причины отказа.
	cases := []struct {
		// name задаёт название конкретного отказа входных параметров.
		name string
		// command содержит проверяемые путь, аргументы и ExtraFiles.
		command Command
		// ctx задаёт контекст запуска выбранного сценария.
		ctx context.Context
		// want содержит ожидаемую проверяемую причину отказа Start.
		want error
	}{
		{"cancelled", base, cancelled, context.Canceled},
		{"binary-nul", Command{Binary: "bad\x00"}, testContext(t), ErrInvalid},
		{"argument-utf8", Command{Binary: base.Binary, Args: []string{"bad\xff"}}, testContext(t), ErrInvalid},
		{"too-many-arguments", Command{Binary: base.Binary, Args: make([]string, testLimits().MaxCommandArgs+1)}, testContext(t), ErrLimit},
		{"command-bytes", Command{Binary: strings.Repeat("x", testLimits().MaxCommandBytes+1)}, testContext(t), ErrLimit},
		{"too-many-descriptors", Command{Binary: base.Binary, ExtraFiles: make([]*os.File, testLimits().MaxExtraFiles+1)}, testContext(t), ErrLimit},
		{"nil-descriptor", Command{Binary: base.Binary, ExtraFiles: []*os.File{nil}}, testContext(t), ErrInvalid},
	}
	// before сохраняет исходное число операций и состояние допуска области.
	before, err := s.header()
	if err != nil {
		t.Fatal(err)
	}
	// tc содержит очередной сценарий недопустимого Start.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет отказ выбранного недопустимого ввода до изменения каталога операций.

			// p содержит неожиданный Process при отклоняемом вводе; err содержит ожидаемый
			// отказ.
			p, err := s.Start(tc.ctx, tc.command)
			if p != nil || !errors.Is(err, tc.want) {
				t.Fatalf("некорректный запуск: process=%v err=%v", p, err)
			}
			// after содержит заголовок после отказа для проверки неизменности Count и
			// Sealed.
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
	// proof содержит окончательное доказательство отсутствия незавершённых операций после всех
	// отказов ввода.
	proof, err := s.Resolve(testContext(t))
	if err != nil || !proof.Joined || proof.Operations != before.Count {
		t.Fatalf("отклонённый ввод оставил незавершённую операцию: %+v: %v", proof, err)
	}
}

// Проверяет завершение уже созданной операции основного процесса, если бюджета не хватает на
// регистрацию целевой команды.
func TestNativeBudgetFailureCompletesBackendTicket(t *testing.T) {
	// helper содержит допустимый true; err сохраняет результаты поиска и проверки частичного
	// резервирования.
	helper, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	// limits ограничивает каталог двумя операциями, оставляя место backend-учёту без места
	// native-команде.
	limits := testLimits()
	limits.MaxOperations = 2
	// r содержит реестр намеренно малого бюджета операций.
	r, err := New(Config{Root: filepath.Join(t.TempDir(), "journal"), HelperPath: helper, Limits: limits, GracePeriod: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает реестр после проверки исчерпания бюджета при частичном резервировании запуска.
		_ = r.Close()
	})
	// s содержит область с уже учтённым runtime-корнем.
	s := testScope(t, r)
	// p содержит результат Start при исчерпанном native-бюджете и должен быть nil.
	p, err := s.Start(testContext(t), Command{Binary: helper})
	if p != nil || !errors.Is(err, ErrLimit) {
		t.Fatalf("недостаточный бюджет native принят: process=%v err=%v", p, err)
	}
	// liveProof содержит доказательство завершённого backend-Ticket до Seal области.
	liveProof, err := s.Resolve(testContext(t))
	if err != nil || liveProof.Sealed || liveProof.Joined || liveProof.Unknown || liveProof.PendingOperations != 0 || liveProof.Operations != 2 {
		t.Fatalf("частичный запуск сохранил незавершённый backend ticket: %+v: %v", liveProof, err)
	}
	if err = s.CloseContext(testContext(t)); err != nil {
		t.Fatal("частичный запуск удерживает backend ticket", err)
	}
	// proof содержит итог после Close для проверки завершения частичной регистрации.
	proof, err := s.Resolve(testContext(t))
	if err != nil || !proof.Sealed || !proof.Joined || proof.Unknown || proof.Operations != 2 {
		t.Fatalf("неполная регистрация не завершена: %+v: %v", proof, err)
	}
}

// Проверяет, что Run после отмены возвращает context.Canceled вместе с доказательством фактического
// Wait целевого процесса, игнорирующего Interrupt.
func TestRunCancellationWaitsForPhysicalJoin(t *testing.T) {
	// r содержит реестр с настоящим helper для проверки отмены синхронного Run.
	r := testRegistry(t, buildHelper(t))
	// s содержит область процесса, игнорирующего Interrupt до принудительной остановки.
	s := testScope(t, r)
	// reader принимает готовность цели; writer передаётся ей как FD 3; err сохраняет результаты
	// канала и последующих проверок.
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает оба дескриптора канала готовности целевого процесса после проверки отмены Run.
		_ = reader.Close()
		_ = writer.Close()
	}()
	if err = reader.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// ctx задаёт отменяемый срок Run; cancel инициирует проверяемую отмену и резервную остановку.
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	// outcome объединяет доказательство завершения Run и ошибку отмены его ожидания.
	type outcome struct {
		// result содержит сохранённый native-результат после фактического Wait.
		result Result
		// err содержит результат Run, включая отмену и ошибки целевого процесса.
		err error
	}
	// done передаёт единый итог синхронного Run после завершения native-процесса.
	done := make(chan outcome, 1)
	go func() {
		// Выполняет синхронный Run процесса, игнорирующего os.Interrupt, и передаёт результат после его
		// физического завершения.

		// result содержит фактически дождённый native-результат; err содержит итог отменённого
		// синхронного Run.
		result, err := s.Run(ctx, Command{Binary: os.Args[0], Args: []string{"-test.run=^TestInterruptIgnoringNative$", "--", "--interrupt-ignoring-native"}, ExtraFiles: []*os.File{writer}})
		done <- outcome{result, err}
	}()
	t.Cleanup(func() {
		// Отменяет Run и дожидается его возврата при любом исходе проверки.

		cancel()
		select {
		case <-done:
		case <-testContext(t).Done():
			t.Error("Run не завершился после отмены")
		}
	})
	// ready принимает сигнал готовности к проверке отмены процесса, игнорирующего Interrupt.
	var ready [1]byte
	if _, err = io.ReadFull(reader, ready[:]); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	// got содержит окончательный исход Run, возвращаемый в канал для резервной очистки.
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
	// proof содержит итог области после возврата Run для проверки отсутствия незавершённых операций.
	proof, err := s.Resolve(testContext(t))
	if err != nil || !proof.Joined || proof.Unknown || proof.PendingOperations != 0 {
		t.Fatalf("после Run осталась незавершённая операция: %+v: %v", proof, err)
	}
}
