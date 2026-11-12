package processowner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Возвращает конечные бюджеты журнала, команд, дескрипторов и страниц для тестовых реестров.
func testLimits() Limits {
	return Limits{MaxOperations: 64, MaxRecordBytes: 65536, MaxCommandArgs: 64, MaxCommandBytes: 16384, MaxExtraFiles: 4, PageSize: 16}
}

// Собирает настоящую вспомогательную программу в отдельном временном каталоге без загрузки модулей.
// При заданном каталоге покрытия включает инструментацию дочернего процесса.
func buildHelper(t *testing.T) string {
	t.Helper()
	// binary задаёт временный путь собираемого настоящего gost-process-helper.
	binary := filepath.Join(t.TempDir(), "gost-process-helper")
	// args собирает параметры сборки helper с необязательным покрытием.
	args := []string{"build", "-o", binary}
	// directory содержит явно заданный каталог покрытия вспомогательных процессов.
	if directory := os.Getenv("GOST_TEST_PROCESS_HELPER_COVERDIR"); directory != "" {
		t.Setenv("GOCOVERDIR", directory)
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=github.com/GregoryGost/gost-restreamer/...")
	}
	// cmd собирает helper с локальным набором инструментов Go и отключённой загрузкой модулей.
	cmd := exec.Command("go", append(args, "../../cmd/gost-process-helper")...)
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-mod=readonly", "GOPROXY=off")
	// output содержит диагностику сборки; err сообщает об отказе команды сборки.
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper build: %v\n%s", err, output)
	}
	return binary
}

// Открывает изолированный реестр с заданной вспомогательной программой и регистрирует его закрытие при
// завершении теста.
func testRegistry(t *testing.T, helper string) *Registry {
	t.Helper()
	// r содержит новый изолированный реестр; err сообщает об отказе его создания.
	r, err := New(Config{Root: filepath.Join(t.TempDir(), "journal"), HelperPath: helper, Limits: testLimits(), GracePeriod: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает реестр процессов при завершении проверки и сообщает об ошибке.

		// err сообщает об отказе резервного закрытия реестра процессов.
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}

// Создаёт область с тестовым владельцем сессии и регистрирует ограниченное закрытие, включая частично
// возвращённую область при ошибке создания.
func testScope(t *testing.T, r *Registry) *Scope {
	t.Helper()
	// s содержит созданную либо частичную область тестового владельца; err сообщает об отказе её
	// создания.
	s, err := r.Create(context.Background(), `{"version":1,"kind":"session","fence":{"id":"synthetic"}}`)
	if s != nil {
		t.Cleanup(func() {
			// Закрывает созданную область, включая частичный результат Create, с ограниченным контекстом проверки.

			// err сообщает об отказе ограниченного резервного закрытия области.
			if err := s.CloseContext(testContext(t)); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Создаёт независимый контекст с десятисекундным сроком и отменяет его при завершении теста.
func testContext(t *testing.T) context.Context {
	t.Helper()
	// ctx задаёт десятисекундный срок тестовых операций; cancel освобождает его при очистке теста.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// Проверяет передачу вывода и результата команды, независимость срока Wait, запрет запоздалого
// вспомогательного процесса и учёт незавершённых операций. Гибель вспомогательного процесса и один
// маркер запуска не должны подменять доказательство завершения целевого процесса.
func TestManagedProcesses(t *testing.T) {
	// helper задаёт общий собранный бинарник для независимых сценариев управляемых процессов.
	helper := buildHelper(t)
	t.Run("output-and-native-error", func(t *testing.T) {
		// Проверяет сохранение вывода и кода ошибки целевого процесса, доказательство завершения и
		// подтверждение удаления временного каталога.

		// r содержит реестр сценария передачи вывода и ненулевого native-результата.
		r := testRegistry(t, helper)
		// s содержит область процесса, записывающего оба потока и завершающегося с кодом 7.
		s := testScope(t, r)
		// stdout сохраняет контрольный стандартный вывод; stderr сохраняет контрольный
		// стандартный поток ошибок.
		var stdout, stderr bytes.Buffer
		// result содержит исход native-команды; err сохраняет результаты Run и дальнейшего
		// подтверждения области.
		result, err := s.Run(testContext(t), Command{Binary: "/bin/sh", Args: []string{"-c", "printf native-output; printf native-error >&2; exit 7"}, Stdout: &stdout, Stderr: &stderr})
		if err == nil || !result.Joined || result.ExitCode != 7 || stdout.String() != "native-output" || stderr.String() != "native-error" {
			t.Fatalf("result=%+v err=%v stdout=%q stderr=%q", result, err, stdout.String(), stderr.String())
		}
		if err = s.Seal(testContext(t)); err != nil {
			t.Fatal(err)
		}
		// proof содержит результат сверки завершения области и её последующего подтверждения.
		proof, err := s.Resolve(testContext(t))
		if err != nil || !proof.Joined || proof.Owner == "" {
			t.Fatalf("proof=%+v %v", proof, err)
		}
		if err = r.AcknowledgeScope(testContext(t), s.ID()); err != nil {
			t.Fatal(err)
		}
		if _, err = os.Stat(s.Namespace()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("namespace still present: %v", err)
		}
		proof, err = s.Resolve(testContext(t))
		if err != nil || !proof.Joined || !proof.Acknowledged {
			t.Fatalf("ack proof=%+v %v", proof, err)
		}
	})
	t.Run("wait-deadline-does-not-kill-owner", func(t *testing.T) {
		// Проверяет, что отмена отдельного Wait сохраняет владельца и целевой процесс до явной отмены Process.

		// r содержит реестр проверки независимости срока отдельного Wait.
		r := testRegistry(t, helper)
		// s содержит область длительно работающей команды sleep.
		s := testScope(t, r)
		// ctx ограничивает жизнь процесса и окончательное ожидание после явной Cancel.
		ctx := testContext(t)
		// p содержит запущенный sleep; err сохраняет результаты запуска и отдельных ожиданий.
		p, err := s.Start(ctx, Command{Binary: "/bin/sleep", Args: []string{"100"}})
		if err != nil {
			t.Fatal(err)
		}
		// cancelled прерывает только проверяемый Wait; cancel инициирует отмену до вызова.
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err = p.Wait(cancelled); !errors.Is(err, context.Canceled) {
			t.Fatalf("wait: %v", err)
		}
		select {
		case <-p.Done():
			t.Fatal("wait deadline terminated child")
		default:
		}
		p.Cancel()
		// result содержит фактически дождённый результат после явного Process.Cancel.
		result, err := p.Wait(ctx)
		if err != nil || !result.Joined {
			t.Fatalf("join=%+v %v", result, err)
		}
	})
	t.Run("late-helper-denied-without-resurrection", func(t *testing.T) {
		// Проверяет отказ позднего gost-process-helper для закрытого либо отсутствующего резервирования без
		// повторного запуска целевого процесса.

		// r содержит реестр проверки отказа запоздалого helper.
		r := testRegistry(t, helper)
		// s содержит область с незапущенной native-резервацией перед Close и Resolve.
		s := testScope(t, r)
		// marker задаёт файл, который не должен создать поздний целевой touch.
		marker := filepath.Join(t.TempDir(), "must-not-exist")
		// ticket содержит native-резервацию контрольного touch; err сохраняет результаты
		// регистрации и проверок позднего запуска.
		ticket, err := s.register(testContext(t), "native", &Command{Binary: "/bin/touch", Args: []string{marker}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		// proof содержит доказательство безопасного закрытия незапущенной резервации.
		proof, err := s.Resolve(testContext(t))
		if err != nil || !proof.Joined {
			t.Fatalf("proof=%+v %v", proof, err)
		}
		// cmd запускает helper с прежними точными номерами после закрытия либо удаления
		// резервации.
		cmd := exec.Command(helper, "1", r.config.Root, s.id, ticket.id)
		if err = cmd.Run(); err == nil {
			t.Fatal("late helper accepted CLOSED")
		}
		if _, err = os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("late child ran: %v", err)
		}
		if err = r.root.Remove(operationPath(s.id, ticket.id) + ".json"); err != nil {
			t.Fatal(err)
		}
		cmd = exec.Command(helper, "1", r.config.Root, s.id, ticket.id)
		if err = cmd.Run(); err == nil {
			t.Fatal("late helper accepted missing reservation")
		}
		if _, err = r.root.Stat(operationPath(s.id, ticket.id) + ".json"); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("missing reservation recreated")
		}
	})
	t.Run("spawn-marker-without-owner-remains-unknown", func(t *testing.T) {
		// Проверяет UNKNOWN для отметки запуска без сохранённого результата Wait и отдельную идентичность
		// новой области.

		// r содержит реестр проверки неизвестного исхода отметки запуска без Wait.
		r := testRegistry(t, helper)
		// s содержит область с моделируемой незавершённой native-операцией.
		s := testScope(t, r)
		// ticket содержит предварительную native-резервацию; err сохраняет результаты изменения
		// и проверки её записи.
		ticket, err := s.register(testContext(t), "native", &Command{Binary: "/bin/true"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		// op содержит запись, намеренно переводимую в SPAWN_MARKER без итогового результата.
		op, err := s.readOperation(ticket.id)
		if err != nil {
			t.Fatal(err)
		}
		op.State = spawned
		if err = writeRecord(r.root, operationPath(s.id, ticket.id)+".json", op, r.config.Limits.MaxRecordBytes); err != nil {
			t.Fatal(err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		// proof содержит результат сверки для проверки Unknown вместо ложного Joined.
		proof, err := s.Resolve(testContext(t))
		if err != nil || proof.Joined || !proof.Unknown {
			t.Fatalf("unknown=%+v %v", proof, err)
		}
		// fresh содержит новую независимую область после неизвестного исхода прежней.
		fresh := testScope(t, r)
		if fresh.ID() == s.ID() {
			t.Fatal("namespace reused")
		}
	})
	t.Run("catalog-and-backend-ticket", func(t *testing.T) {
		// Проверяет сохранение незавершённой операции основного процесса, её позднее завершение и ограниченную
		// пагинацию каталога.

		// r содержит реестр проверки backend-Ticket и пагинации каталога.
		r := testRegistry(t, helper)
		// s содержит область с задержанной backend-операцией builder.
		s := testScope(t, r)
		// ticket содержит незавершённую операцию builder; err сохраняет результаты регистрации,
		// завершения и сверки.
		ticket, err := s.Register(testContext(t), "builder")
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Close(); !errors.Is(err, ErrPending) {
			t.Fatalf("close=%v", err)
		}
		// proof содержит состояние до и после Complete для проверки полного каталога двух
		// операций.
		proof, err := s.Resolve(testContext(t))
		if err != nil || proof.Joined || proof.PendingOperations == 0 {
			t.Fatalf("proof=%+v %v", proof, err)
		}
		if err = ticket.Complete(testContext(t)); err != nil {
			t.Fatal(err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		proof, err = s.Resolve(testContext(t))
		if err != nil || !proof.Joined || proof.Operations != 2 {
			t.Fatalf("proof=%+v %v", proof, err)
		}
		// page содержит ограниченную страницу результатов областей; next содержит курсор
		// продолжения каталога.
		page, next, err := r.RecoverPage(testContext(t), "", 1)
		if err != nil || len(page) != 1 || next == "" {
			t.Fatalf("page=%+v next=%s %v", page, next, err)
		}
		page, _, err = r.RecoverPage(testContext(t), next, 1)
		if err != nil || len(page) != 0 {
			t.Fatalf("last page=%+v %v", page, err)
		}
	})
	t.Run("helper-death-never-fabricates-native-join", func(t *testing.T) {
		// Проверяет, что смерть gost-process-helper оставляет UNKNOWN вместо ложного подтверждения завершения
		// целевого процесса.

		// r содержит реестр проверки смерти helper при продолжающемся целевом процессе.
		r := testRegistry(t, helper)
		// s содержит область контролируемого целевого процесса и его helper.
		s := testScope(t, r)
		// ctx ограничивает ожидание готовности, смерти helper и последующей сверки.
		ctx := testContext(t)
		// readyRead принимает готовность целевого процесса; readyWrite передаётся ему как FD 3;
		// err сохраняет результаты запуска и проверки сценария.
		readyRead, readyWrite, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			// Закрывает чтение канала готовности целевого процесса после проверки смерти gost-process-helper.
			_ = readyRead.Close()
		}()
		// controlRead передаётся целевому процессу как FD 4; controlWrite разрешает ему выйти
		// после смерти helper.
		controlRead, controlWrite, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			// Закрывает запись канала управления, позволяя оставшемуся целевому процессу выйти после смерти
			// gost-process-helper.
			_ = controlWrite.Close()
		}()
		// p содержит управляемый процесс, чей helper тест принудительно завершает.
		p, err := s.Start(ctx, Command{Binary: os.Args[0], Args: []string{"-test.run=^TestControlledNativeChild$", "--", "--controlled-native-child"}, ExtraFiles: []*os.File{readyWrite, controlRead}})
		if err != nil {
			t.Fatal(err)
		}
		_ = readyWrite.Close()
		_ = controlRead.Close()
		// b принимает однобайтовую готовность целевого процесса до смерти helper.
		var b [1]byte
		if _, err = io.ReadFull(readyRead, b[:]); err != nil {
			t.Fatal(err)
		}
		if err = p.helper.Kill(); err != nil {
			t.Fatal(err)
		}
		// result содержит итог ожидания погибшего helper для проверки отсутствия native-Joined.
		result, err := p.Wait(ctx)
		if err != nil || result.Joined || !errors.Is(result.Err, ErrUnknown) {
			t.Fatalf("helper death=%+v %v", result, err)
		}
		if err = controlWrite.Close(); err != nil {
			t.Fatal(err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		// proof содержит результат сверки после смерти helper для проверки Unknown.
		proof, err := s.Resolve(ctx)
		if err != nil || proof.Joined || !proof.Unknown {
			t.Fatalf("helper death proof=%+v %v", proof, err)
		}
	})
}

// При специальном аргументе сообщает готовность через FD 3 и удерживает дочерний процесс до чтения или
// закрытия FD 4. В обычном запуске тестов побочных эффектов не создаёт.
func TestControlledNativeChild(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "--controlled-native-child") {
		return
	}
	// ready представляет FD 3 контролируемого дочернего процесса для сигнала готовности.
	ready := os.NewFile(3, "native-ready")
	// control представляет FD 4, чтение которого удерживает целевой процесс до разрешения родителя.
	control := os.NewFile(4, "native-control")
	if ready == nil || control == nil {
		os.Exit(99)
	}
	_, _ = ready.Write([]byte{1})
	_ = ready.Close()
	// b принимает один байт управления либо EOF для завершения контролируемого процесса.
	var b [1]byte
	_, _ = control.Read(b[:])
	os.Exit(0)
}

// TestCrashBackendChild используется только как изолированный основной процесс тестового приложения.
func TestCrashBackendChild(t *testing.T) {
	// index хранит позицию --crash-backend; -1 означает обычный запуск теста.
	index := -1
	// i обозначает позицию аргумента; arg содержит проверяемую строку при поиске изолированного
	// backend-режима.
	for i, arg := range os.Args {
		if arg == "--crash-backend" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	// args содержит корень журнала, runtime-каталог и helper после маркера backend-режима.
	args := os.Args[index+1:]
	if len(args) != 3 {
		os.Exit(91)
	}
	// r содержит реестр изолированного основного процесса; err сохраняет результаты создания,
	// запуска и ожидания готовности.
	r, err := New(Config{Root: args[0], RuntimeRoot: args[1], HelperPath: args[2], Limits: testLimits(), GracePeriod: 20 * time.Millisecond})
	if err != nil {
		os.Exit(92)
	}
	// s содержит область владельца crashed-backend, сохраняемую перед выходом основного процесса.
	s, err := r.Create(context.Background(), "crashed-backend")
	if err != nil {
		os.Exit(93)
	}
	// readyRead принимает готовность целевой команды; readyWrite передаётся ей через FD 3.
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		os.Exit(94)
	}
	_, err = s.Start(context.Background(), Command{Binary: os.Args[0], Args: []string{"-test.run=^TestNativeCrashChild$", "--", "--native-crash-child"}, ExtraFiles: []*os.File{readyWrite}})
	if err != nil {
		os.Exit(95)
	}
	_ = readyWrite.Close()
	// ready принимает сигнал готовности перед печатью идентификатора области и завершением backend.
	var ready [1]byte
	if _, err = io.ReadFull(readyRead, ready[:]); err != nil {
		os.Exit(96)
	}
	_, _ = fmt.Fprintln(os.Stdout, s.ID())
	os.Exit(0)
}

// При специальном аргументе сообщает готовность через FD 3 и ожидает Interrupt от вспомогательного
// процесса после исчезновения основного.
func TestNativeCrashChild(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "--native-crash-child") {
		return
	}
	// interrupt принимает сигнал мягкой остановки от helper после закрытия backend-lifeline.
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	// ready представляет FD 3 для подтверждения готовности целевого процесса к проверке исчезновения
	// backend.
	ready := os.NewFile(3, "native-ready")
	if ready == nil {
		os.Exit(98)
	}
	_, _ = ready.Write([]byte{1})
	_ = ready.Close()
	<-interrupt
	os.Exit(0)
}

// Проверяет, что после выхода изолированного основного процесса вспомогательный процесс завершает
// целевой процесс, а вновь открытый реестр получает Joined с прежним владельцем.
func TestBackendCrashGuardianRecovery(t *testing.T) {
	// helper задаёт настоящий собранный бинарник восстановления после выхода backend.
	helper := buildHelper(t)
	// root задаёт общий для изолированного backend и восстановителя корень журнала.
	root := filepath.Join(t.TempDir(), "journal")
	// runtime задаёт общий корень рабочих объектов восстанавливаемой области.
	runtime := filepath.Join(t.TempDir(), "runtime")
	// cmd запускает изолированный тестовый backend, создающий управляемого целевого ребёнка.
	cmd := exec.CommandContext(testContext(t), os.Args[0], "-test.run=^TestCrashBackendChild$", "--", "--crash-backend", root, runtime, helper)
	// output содержит выведенный backend идентификатор области; err сохраняет результаты запуска и
	// открытия её журнала.
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("backend=%v %s", err, output)
	}
	// id содержит очищенный от пробелов идентификатор созданной изолированным backend области.
	id := strings.TrimSpace(string(output))
	if !validID(id) {
		t.Fatalf("invalid scope output %q", output)
	}
	// r содержит повторно открытый реестр после выхода основного процесса.
	r, err := New(Config{Root: root, RuntimeRoot: runtime, HelperPath: helper, Limits: testLimits(), GracePeriod: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает повторно открытый реестр после проверки завершения процессов вслед за выходом основного
		// процесса.
		_ = r.Close()
	}()
	// s содержит область прежнего владельца, открытую для проверки завершения helper.
	s, err := r.OpenScope(testContext(t), id)
	if err != nil {
		t.Fatal(err)
	}
	// ctx ограничивает повторную сверку сохранённых операций.
	ctx := testContext(t)
	// ticker задаёт период повторной проверки Joined после исчезновения backend.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		// proof содержит текущий результат сверки; e сообщает об отказе чтения либо
		// восстановления.
		proof, e := s.Resolve(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if proof.Joined {
			if proof.Owner != "crashed-backend" {
				t.Fatal(proof.Owner)
			}
			break
		}
		if proof.Unknown {
			// b содержит JSON неизвестного доказательства для диагностического сообщения
			// теста.
			b, _ := json.Marshal(proof)
			t.Fatalf("guardian lost proof: %s", b)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("guardian did not join: %+v", proof)
		case <-ticker.C:
		}
	}
}
