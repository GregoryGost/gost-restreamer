package integration

import (
	"bufio"
	"context"
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

	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// Возвращает конечные бюджеты журнала процессов для интеграционных сценариев владения и
// восстановления.
func sessionAcceptanceProcessLimits() processowner.Limits {
	return processowner.Limits{MaxOperations: 128, MaxRecordBytes: 65536, MaxCommandArgs: 128, MaxCommandBytes: 16384, MaxExtraFiles: 4, PageSize: 16}
}

// Собирает настоящую вспомогательную программу во временном каталоге с ограничением времени и без
// загрузки модулей. При заданном каталоге покрытия включает инструментацию дочернего процесса.
func sessionAcceptanceBuildHelper(t *testing.T) string {
	t.Helper()
	// path указывает отдельный бинарник настоящего gost-process-helper во временном каталоге.
	path := filepath.Join(t.TempDir(), "gost-process-helper")
	// ctx ограничивает сборку helper 90 секундами; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// args содержит аргументы сборки helper и дополняется инструментацией при включённом покрытии
	// дочерних процессов.
	args := []string{"build", "-o", path}
	// directory содержит переданный каталог покрытия, включающий -cover и GOCOVERDIR для helper.
	if directory := os.Getenv("GOST_TEST_PROCESS_HELPER_COVERDIR"); directory != "" {
		t.Setenv("GOCOVERDIR", directory)
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=github.com/GregoryGost/gost-restreamer/...")
	}
	// build запускает локальную сборку настоящего helper без загрузки модулей.
	build := exec.CommandContext(ctx, "go", append(args, "../../cmd/gost-process-helper")...)
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-mod=readonly", "GOPROXY=off")
	// output сохраняет диагностику сборки; err содержит отказ выполнения go build.
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("сборка настоящего helper: %v\n%s", err, output)
	}
	return path
}

// Возвращает аргументы после первого точного маркера запуска дочернего сценария; при отсутствии
// маркера возвращает nil.
func sessionAcceptanceArgs(marker string) []string {
	// i указывает позицию аргумента процесса; arg содержит текущий аргумент при поиске точного
	// маркера дочернего режима.
	for i, arg := range os.Args {
		if arg == marker {
			return os.Args[i+1:]
		}
	}
	return nil
}

// Основной и целевой дочерний процессы запущены только этим тестом; SIGKILL направляется живому os.Process.
func TestProcessOwnerCrashBackendChild(t *testing.T) {
	// args содержит четыре пути дочернего сценария аварийного backend либо nil при обычном запуске
	// тестов.
	args := sessionAcceptanceArgs("--owned-crash-backend")
	if args == nil {
		return
	}
	if len(args) != 4 {
		os.Exit(71)
	}
	// registry содержит реестр дочернего основного процесса; err содержит отказ открытия его
	// журналов.
	registry, err := processowner.New(processowner.Config{Root: args[0], RuntimeRoot: args[1], HelperPath: args[2], Limits: sessionAcceptanceProcessLimits(), GracePeriod: 100 * time.Millisecond})
	if err != nil {
		os.Exit(72)
	}
	// scope содержит область abrupt-backend для физического владения целевым нативным процессом.
	scope, err := registry.Create(context.Background(), "abrupt-backend")
	if err != nil {
		os.Exit(73)
	}
	// readyRead принимает подтверждение готовности целевого процесса; readyWrite передаётся ему как
	// FD 3.
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		os.Exit(74)
	}
	_, err = scope.Start(context.Background(), processowner.Command{Binary: os.Args[0], Args: []string{"-test.run=^TestProcessOwnerNativeChild$", "--", "--owned-native", "graceful", args[3]}, ExtraFiles: []*os.File{readyWrite}})
	if err != nil {
		os.Exit(75)
	}
	if err = readyWrite.Close(); err != nil {
		os.Exit(76)
	}
	// ready принимает единственный байт подтверждения готовности целевого процесса.
	var ready [1]byte
	if _, err = io.ReadFull(readyRead, ready[:]); err != nil || ready[0] != 1 {
		os.Exit(77)
	}
	if err = readyRead.Close(); err != nil {
		os.Exit(78)
	}
	// status использует унаследованный FD 3 для передачи родительскому тесту точного ID области.
	status := os.NewFile(3, "backend-status")
	if status == nil {
		os.Exit(79)
	}
	if _, err = fmt.Fprintln(status, scope.ID()); err != nil {
		os.Exit(80)
	}
	if err = status.Close(); err != nil {
		os.Exit(81)
	}
	<-time.After(time.Hour)
}

// При специальном аргументе сообщает готовность через FD 3 и либо игнорирует Interrupt, либо ожидает
// его и записывает маркер наблюдения. Обычный запуск тестов этот дочерний сценарий не выполняет.
func TestProcessOwnerNativeChild(t *testing.T) {
	// args содержит режим реакции на Interrupt и путь маркера целевого дочернего процесса.
	args := sessionAcceptanceArgs("--owned-native")
	if args == nil {
		return
	}
	if len(args) != 2 {
		os.Exit(82)
	}
	// interrupt принимает уведомление os.Interrupt в управляемом режиме graceful.
	interrupt := make(chan os.Signal, 1)
	if args[0] == "ignore" {
		signal.Ignore(os.Interrupt)
	} else {
		signal.Notify(interrupt, os.Interrupt)
	}
	// ready содержит унаследованный FD 3 для подтверждения запуска целевого процесса.
	ready := os.NewFile(3, "native-ready")
	if ready == nil {
		os.Exit(83)
	}
	// err содержит отказ записи байта готовности целевого процесса.
	if _, err := ready.Write([]byte{1}); err != nil {
		os.Exit(84)
	}
	// err содержит отказ закрытия канала готовности целевого процесса.
	if err := ready.Close(); err != nil {
		os.Exit(85)
	}
	if args[0] == "ignore" {
		<-time.After(15 * time.Second)
		return
	}
	select {
	case <-interrupt:
	case <-time.After(15 * time.Second):
		os.Exit(87)
	}
	// err содержит отказ записи маркера реально полученного Interrupt.
	if err := os.WriteFile(args[1], []byte("native interrupt observed"), 0600); err != nil {
		os.Exit(86)
	}
}

// Проверяет, что SIGKILL прежнего основного процесса приводит через lifeline к остановке и
// подтверждённому завершению его целевого процесса. Очистка старой области сохраняет новый процесс,
// его рабочий каталог и возможность регистрации.
func TestProcessOwnerAbruptBackendDeathJoinsOnlyOldExecution(t *testing.T) {
	// helper содержит настоящий бинарник владельца нативных процессов.
	helper := sessionAcceptanceBuildHelper(t)
	// base выделяет частный короткий каталог журнала и runtime; err содержит отказ создания.
	base, err := os.MkdirTemp("/tmp", "owner-crash-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Удаляет частный каталог журнала и временных файлов проверки аварийного завершения.

		// err содержит отказ удаления собственных временных данных сценария.
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	// root задаёт каталог долговечного журнала процессов; runtime задаёт отдельный корень областей
	// выполнения.
	root, runtime := filepath.Join(base, "journal"), filepath.Join(base, "runtime")
	// interrupted указывает маркер получения Interrupt прежним целевым процессом.
	interrupted := filepath.Join(base, "old-native-interrupted")
	// statusRead читает ID области из дочернего backend; statusWrite передаётся ему как FD 3.
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает читающий конец канала состояния тестового основного процесса.
		_ = statusRead.Close()
	})
	// backend запускает отдельный тестовый основной процесс, чей SIGKILL должен закрыть lifeline его
	// целевого процесса.
	backend := exec.Command(os.Args[0], "-test.run=^TestProcessOwnerCrashBackendChild$", "--", "--owned-crash-backend", root, runtime, helper, interrupted)
	backend.ExtraFiles = []*os.File{statusWrite}
	if err = backend.Start(); err != nil {
		_ = statusWrite.Close()
		t.Fatal(err)
	}
	// backendJoined отмечает фактический Wait основного процесса, предотвращая повторное ожидание
	// при очистке.
	backendJoined := false
	t.Cleanup(func() {
		// Убивает и дожидается тестового основного процесса, если основной сценарий ещё не подтвердил его
		// завершение.

		if !backendJoined {
			_ = backend.Process.Kill()
			_ = backend.Wait()
		}
	})
	if err = statusWrite.Close(); err != nil {
		t.Fatal(err)
	}
	if err = statusRead.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// scopeID содержит точный идентификатор области, полученный после готовности backend и нативного
	// процесса.
	scopeID, err := bufio.NewReader(statusRead).ReadString('\n')
	if err != nil {
		t.Fatal("backend/native handshake:", err)
	}
	scopeID = strings.TrimSpace(scopeID)
	// registry открывает те же журналы для проверки доказательства завершения старой и независимости
	// новой области.
	registry, err := processowner.New(processowner.Config{Root: root, RuntimeRoot: runtime, HelperPath: helper, Limits: sessionAcceptanceProcessLimits(), GracePeriod: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает реестр владения процессами после освобождения областей выполнения.

		// err содержит результат закрытия наблюдающего реестра при очистке.
		if err := registry.Close(); err != nil {
			t.Error(err)
		}
	})
	// old содержит открытую область прежнего backend для Resolve и AcknowledgeScope.
	old, err := registry.OpenScope(context.Background(), scopeID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// При необходимости завершает основной процесс и ограниченно ожидает доказательство завершения его
		// прежнего дочернего исполнения.

		if !backendJoined {
			_ = backend.Process.Kill()
			_ = backend.Wait()
			backendJoined = true
		}
		// ctx ограничивает очистку прежнего исполнения пятью секундами; cancel освобождает
		// таймер.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// ticker задаёт период повторного чтения доказательства завершения прежнего нативного
		// процесса.
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			// proof содержит очередное доказательство старой области; err содержит отказ его
			// чтения при очистке.
			proof, err := old.Resolve(ctx)
			if err == nil && proof.Joined {
				return
			}
			select {
			case <-ctx.Done():
				t.Errorf("старый native не присоединён при cleanup: %+v %v", proof, err)
				return
			case <-ticker.C:
			}
		}
	})
	// proof сохраняет состояние старой области до SIGKILL и затем после фактического завершения.
	proof, err := old.Resolve(context.Background())
	if err != nil || proof.Joined || proof.Sealed {
		t.Fatalf("живой backend ошибочно присоединён: %+v %v", proof, err)
	}
	// fresh содержит новую независимую область с живым целевым процессом.
	fresh, err := registry.Create(context.Background(), "new-active-execution")
	if err != nil {
		t.Fatal(err)
	}
	// newNative содержит новый управляемый процесс, которого очистка старой области не должна
	// затронуть.
	newNative := sessionAcceptanceStartNative(t, fresh, "ignore", filepath.Join(base, "unused"))
	t.Cleanup(func() {
		// Отменяет новое дочернее исполнение, ожидает его завершения и закрывает принадлежащую ему область.

		newNative.Cancel()
		// ctx ограничивает завершение нового процесса при очистке; cancel освобождает таймер.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// result содержит фактический Wait-результат нового процесса; err содержит отказ
		// ожидания.
		if result, err := newNative.Wait(ctx); err != nil || !result.Joined {
			t.Errorf("новый native не присоединён: %+v %v", result, err)
		}
		// err содержит результат закрытия новой области после подтверждённого завершения её
		// процесса.
		if err := fresh.Close(); err != nil {
			t.Error(err)
		}
	})
	if err = backend.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = backend.Wait(); err == nil {
		t.Fatal("SIGKILL backend не наблюдался")
	}
	backendJoined = true
	sessionAcceptanceWait(t, func() bool {
		// Ожидает подтверждение завершения прежнего исполнения после аварийной смерти его основного процесса.

		proof, err = old.Resolve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return proof.Joined
	})
	if !proof.Sealed || proof.Unknown || proof.PendingOperations != 0 || proof.Owner != "abrupt-backend" {
		t.Fatalf("ложное доказательство: %+v", proof)
	}
	// data содержит маркер получения Interrupt прежним процессом для проверки действия lifeline.
	data, err := os.ReadFile(interrupted)
	if err != nil || string(data) != "native interrupt observed" {
		t.Fatalf("guardian не сообщил native EOF/interrupt: %q %v", data, err)
	}
	if err = registry.AcknowledgeScope(context.Background(), old.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(old.Namespace()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("старый namespace остался: %v", err)
	}
	if _, err = os.Stat(fresh.Namespace()); err != nil {
		t.Fatal("старый cleanup затронул новый namespace:", err)
	}
	select {
	case <-newNative.Done():
		t.Fatal("старый cleanup завершил новый native")
	default:
	}
	// ticket содержит новую операцию fresh после ACK старой области для проверки сохранённого
	// допуска.
	ticket, err := fresh.Register(context.Background(), "work-after-old-ack")
	if err != nil {
		t.Fatal("старый ack закрыл новый scope:", err)
	}
	if err = ticket.Complete(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Запускает управляемый тестовый процесс в заданном режиме и ожидает его подтверждение готовности
// через отдельный канал. Возвращённый процесс останавливает и ожидает вызывающий код.
func sessionAcceptanceStartNative(t *testing.T, scope *processowner.Scope, mode, marker string) *processowner.Process {
	t.Helper()
	// r читает байт готовности запускаемого процесса; w передаётся ему как FD 3; err содержит отказ
	// создания канала.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает читающий конец канала готовности запущенного дочернего процесса.
		_ = r.Close()
	}()
	// ctx ограничивает ожидание при ошибке готовности пятью секундами; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// p содержит физически управляемый дочерний процесс заданного режима.
	p, err := scope.Start(context.Background(), processowner.Command{Binary: os.Args[0], Args: []string{"-test.run=^TestProcessOwnerNativeChild$", "--", "--owned-native", mode, marker}, ExtraFiles: []*os.File{w}})
	if err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		p.Cancel()
		t.Fatal(err)
	}
	if err = r.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		p.Cancel()
		t.Fatal(err)
	}
	// b принимает единственный байт готовности запущенного процесса.
	var b [1]byte
	if _, err = io.ReadFull(r, b[:]); err != nil || b[0] != 1 {
		p.Cancel()
		_, _ = p.Wait(ctx)
		t.Fatalf("native readiness: %v", err)
	}
	return p
}

// Проверяет, что отменённый Wait не доказывает завершение, а Seal запрещает новую команду до внешнего
// эффекта. Последующая остановка должна дать точный результат Wait и Joined.
func TestProcessOwnerCanceledWaitRetainsJoinAndSealedScopeRejectsLaunch(t *testing.T) {
	// helper содержит настоящий helper сценария отменённого Wait и закрытого допуска.
	helper := sessionAcceptanceBuildHelper(t)
	// registry содержит отдельный реестр сценария; err содержит отказ его создания.
	registry, err := processowner.New(processowner.Config{Root: t.TempDir(), HelperPath: helper, Limits: sessionAcceptanceProcessLimits(), GracePeriod: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает реестр после проверки отменённого ожидания и запрета запуска в закрытой области.

		// err содержит результат закрытия реестра после всех его областей.
		if err := registry.Close(); err != nil {
			t.Error(err)
		}
	})
	// scope содержит область canceled-wait для проверки незавершённого процесса после отмены
	// ожидания.
	scope, err := registry.Create(context.Background(), "cancelled-wait")
	if err != nil {
		t.Fatal(err)
	}
	// native содержит целевой процесс, игнорирующий Interrupt до принудительной остановки.
	native := sessionAcceptanceStartNative(t, scope, "ignore", "unused")
	t.Cleanup(func() {
		// Отменяет дочерний процесс, ограниченно ожидает его завершения и закрывает область перед
		// освобождением реестра.

		native.Cancel()
		// ctx ограничивает очистку целевого процесса пятью секундами; cancel освобождает таймер.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = native.Wait(ctx)
		// err содержит результат закрытия области после очистки её процесса.
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	})
	// canceled задаёт уже отменённое ожидание Wait; cancel отменяет его до вызова.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = native.Wait(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait: %v", err)
	}
	if err = scope.Seal(context.Background()); err != nil {
		t.Fatal(err)
	}
	// proof содержит доказательство области после Seal без физического завершения процесса.
	proof, err := scope.Resolve(context.Background())
	if err != nil || proof.Joined || proof.PendingOperations == 0 {
		t.Fatalf("Wait timeout выдан за join: %+v %v", proof, err)
	}
	// marker указывает файл, который запрещённый запуск touch в sealed-области не должен создать.
	marker := filepath.Join(t.TempDir(), "must-not-launch")
	if _, err = scope.Start(context.Background(), processowner.Command{Binary: "/usr/bin/touch", Args: []string{marker}}); !errors.Is(err, processowner.ErrClosed) {
		t.Fatalf("sealed scope: %v", err)
	}
	if _, err = os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("sealed scope выполнил внешнее действие")
	}
	native.Cancel()
	// ctx ограничивает ожидание после Cancel пятью секундами; stop освобождает таймер.
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	// result содержит точный исход физической остановки нативного процесса, включая Joined и
	// ненулевой код.
	result, err := native.Wait(ctx)
	if err != nil || !result.Joined || result.ExitCode == 0 {
		t.Fatalf("эскалация без точного Wait-result: %+v %v", result, err)
	}
	if err = scope.Close(); err != nil {
		t.Fatal(err)
	}
	proof, err = scope.Resolve(ctx)
	if err != nil || !proof.Joined || proof.Unknown {
		t.Fatalf("sealed proof: %+v %v", proof, err)
	}
}
