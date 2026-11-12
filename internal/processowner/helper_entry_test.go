package processowner

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"testing"
	"time"
)

// Проверяет результаты успешной, неуспешной и отсутствующей команды и принудительное завершение
// процесса, игнорирующего Interrupt.
func TestGuardianNativeResultsAndEscalation(t *testing.T) {
	// r содержит изолированный реестр с настоящим helper для проверки native-результатов и Kill.
	r := testRegistry(t, buildHelper(t))
	// s содержит область контрольных целевых процессов.
	s := testScope(t, r)
	// ctx ограничивает запуск и ожидание результатов тестовым сроком.
	ctx := testContext(t)
	// trueBinary содержит найденную успешную команду true; err сохраняет результаты поиска, запуска
	// и обмена готовностью.
	trueBinary, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	// falseBinary содержит найденную команду false для проверки ненулевого результата.
	falseBinary, err := exec.LookPath("false")
	if err != nil {
		t.Fatal(err)
	}
	// binary содержит очередной успешный, ошибочный либо отсутствующий исполняемый файл.
	for _, binary := range []string{trueBinary, falseBinary, "/nonexistent/processowner-binary"} {
		// result содержит исход синхронного Run; err сообщает о контрольном отказе команды либо
		// нарушении ожидания.
		result, err := s.Run(ctx, Command{Binary: binary})
		if !result.Joined || (binary == trueBinary && err != nil) || (binary != trueBinary && err == nil) {
			t.Fatalf("%s: %+v %v", binary, result, err)
		}
	}
	// reader принимает сигнал готовности зависшего процесса; writer передаётся ему через FD 3.
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает чтение канала готовности процесса, игнорирующего os.Interrupt, после проверки
		// принудительного завершения.
		_ = reader.Close()
	}()
	// p содержит управляемый процесс, который игнорирует Interrupt после сигнала готовности.
	p, err := s.Start(ctx, Command{Binary: os.Args[0], Args: []string{"-test.run=^TestInterruptIgnoringNative$", "--", "--interrupt-ignoring-native"}, ExtraFiles: []*os.File{writer}})
	if err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	// ready принимает однобайтовое подтверждение установленного игнорирования Interrupt.
	var ready [1]byte
	if _, err = io.ReadFull(reader, ready[:]); err != nil {
		t.Fatal(err)
	}
	p.Cancel()
	// result содержит итог принудительного завершения для проверки Joined и ExitCode=-1.
	result, err := p.Wait(ctx)
	if err != nil || !result.Joined || result.Err == nil || result.ExitCode != -1 {
		t.Fatalf("escalation=%+v %v", result, err)
	}
}

// При специальном аргументе служит дочерним процессом, игнорирующим Interrupt: сообщает готовность
// через FD 3 и ожидает принудительного завершения. В обычном запуске тестов сразу возвращается.
func TestInterruptIgnoringNative(t *testing.T) {
	// found отмечает наличие специального аргумента режима зависшего дочернего процесса.
	found := false
	// arg содержит очередной аргумент при поиске специального дочернего режима.
	for _, arg := range os.Args {
		if arg == "--interrupt-ignoring-native" {
			found = true
		}
	}
	if !found {
		return
	}
	signal.Ignore(os.Interrupt)
	// ready представляет FD 3 для сообщения о готовности к проверке принудительной остановки.
	ready := os.NewFile(3, "native-ready")
	if ready == nil {
		os.Exit(97)
	}
	_, _ = ready.Write([]byte{1})
	_ = ready.Close()
	// timer удерживает игнорирующий Interrupt процесс до внешнего Kill либо часового срока.
	timer := time.NewTimer(time.Hour)
	<-timer.C
}
