package processowner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Owner читает точное сохранённое значение владельца области; недостоверный заголовок возвращает
// вместе с ошибкой.
func (s *Scope) Owner() (string, error) { /* h содержит заголовок области с сохранённым владельцем; err сообщает об отказе его чтения или проверки. */
	h, err := s.header()
	return h.Owner, err
}

// Проверяет имя программы, кодировку и нулевые байты, суммарный размер аргументов, бюджеты аргументов
// и дескрипторов и отсутствие nil среди ExtraFiles. Команду и файловую систему не изменяет.
func validateCommand(command Command, limits Limits) error {
	if len(command.Args) > limits.MaxCommandArgs || len(command.ExtraFiles) > limits.MaxExtraFiles {
		return ErrLimit
	}
	if !utf8.ValidString(command.Binary) {
		return ErrInvalid
	}
	// arg содержит очередной аргумент команды для проверки корректной UTF-8.
	for _, arg := range command.Args {
		if !utf8.ValidString(arg) {
			return ErrInvalid
		}
	}
	if command.Binary == "" || strings.ContainsRune(command.Binary, 0) || len(command.Args) > limits.MaxCommandArgs || len(command.ExtraFiles) > limits.MaxExtraFiles {
		return ErrInvalid
	}
	// n накапливает число байтов исполняемого пути и аргументов для проверки бюджета команды.
	n := len(command.Binary)
	// arg содержит очередной аргумент для проверки нулевых байтов и оставшегося байтового бюджета.
	for _, arg := range command.Args {
		if strings.ContainsRune(arg, 0) || len(arg) > limits.MaxCommandBytes-n {
			return ErrLimit
		}
		n += len(arg)
	}
	if n > limits.MaxCommandBytes {
		return ErrLimit
	}
	// f содержит очередной дополнительный дескриптор для проверки отсутствия nil.
	for _, f := range command.ExtraFiles {
		if f == nil {
			return ErrInvalid
		}
	}
	return nil
}

// Под блокировкой переводит ещё не запущенную операцию RESERVED в CLOSED. Иное состояние не считается
// доказательством безопасного отказа до запуска.
func (s *Scope) closeReserved(ctx context.Context, id string) error {
	// lock удерживает блокировку native-операции; err сохраняет результаты её получения и чтения
	// записи.
	lock, err := lockExisting(ctx, s.registry.root, operationPath(s.id, id)+".lock", true)
	if err != nil {
		return err
	}
	defer func() {
		// Освобождает блокировку нативной операции после закрытия резервирования либо отказа проверки.
		_ = unlock(lock)
	}()
	// op содержит ещё не запущенную операцию, переводимую из RESERVED в CLOSED.
	op, err := s.readOperation(id)
	if err != nil {
		return err
	}
	if op.State != reserved {
		return ErrUnknown
	}
	op.State = closed
	return writeRecord(s.registry.root, operationPath(s.id, id)+".json", op, s.registry.config.Limits.MaxRecordBytes)
}

// Start регистрирует операции ввода-вывода и целевого процесса до запуска отдельного вспомогательного
// процесса. Отмена ctx закрывает lifeline; вспомогательный процесс сохраняет владение Wait, а
// результат и физическое завершение читаются через возвращённый Process.
func (s *Scope) Start(ctx context.Context, command Command) (*Process, error) {
	// err содержит причину отмены контекста до регистрации native-процесса.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// err содержит отказ проверки аргументов и дескрипторов целевой команды.
	if err := validateCommand(command, s.registry.config.Limits); err != nil {
		return nil, err
	}
	// backendTicket учитывает весь backend-ввод и вывод native-процесса; err сохраняет результаты
	// регистрации и запуска helper.
	backendTicket, err := s.Register(ctx, "native-io")
	if err != nil {
		return nil, err
	}
	// finishBackend подтверждает физическое завершение учтённого backend-ввода и вывода независимым
	// контекстом.
	finishBackend := func() error {
		// Подтверждает завершение зарегистрированного ввода и вывода основного процесса с независимым
		// контекстом.
		return backendTicket.Complete(context.Background())
	}
	// nativeTicket содержит предварительную долговечную резервацию целевой команды до запуска
	// helper.
	nativeTicket, err := s.register(ctx, "native", &command, nil)
	if err != nil {
		return nil, errors.Join(err, finishBackend())
	}
	// reader содержит читающий конец lifeline для helper; writer содержит единственный конец записи
	// основного процесса.
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, s.closeReserved(context.Background(), nativeTicket.id), finishBackend())
	}
	// p содержит создаваемый Process с lifeline и отдельным сигналом окончательного результата.
	p := &Process{scope: s, id: nativeTicket.id, lifeline: writer, done: make(chan struct{})}
	// cmd запускает настоящий helper с версией протокола, корнем журнала и точными номерами области
	// и операции.
	cmd := exec.Command(s.registry.config.HelperPath, strconv.Itoa(ProtocolVersion), s.registry.config.Root, s.id, p.id)
	cmd.ExtraFiles = append([]*os.File{reader}, command.ExtraFiles...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = command.Stdin, command.Stdout, command.Stderr
	// CommandContext здесь уничтожил бы единственного владельца Wait целевого процесса при отмене основного процесса.
	if err = cmd.Start(); err != nil {
		p.Cancel()
		return nil, errors.Join(err, reader.Close(), s.closeReserved(context.Background(), p.id), finishBackend())
	}
	// readerErr сохраняет результат закрытия backend-копии читающего конца lifeline после запуска
	// helper.
	readerErr := reader.Close()
	p.helper = cmd.Process
	s.mu.Lock()
	s.processes[p.id] = p
	// alreadyClosed отмечает закрытие области, успевшее начаться до помещения процесса в локальный
	// учёт.
	alreadyClosed := s.closed || s.closing
	s.mu.Unlock()
	// stopCancel снимает обработчик отмены контекста, закрывающий lifeline процесса.
	stopCancel := context.AfterFunc(ctx, p.Cancel)
	if alreadyClosed {
		p.Cancel()
	}
	go func() {
		// Ждёт gost-process-helper, читает сохранённый результат целевого процесса и завершает учёт ввода и
		// вывода до публикации результата и закрытия done.

		// helperErr содержит результат единственного Wait вспомогательного процесса.
		helperErr := cmd.Wait()
		stopCancel()
		p.Cancel()
		// op содержит сохранённый результат native-операции; readErr сообщает об отказе его
		// чтения.
		op, readErr := s.readOperation(p.id)
		// result накапливает код, подтверждение Joined и ошибки helper, журнала и завершения
		// backend-операции.
		result := Result{ExitCode: -1, Err: errors.Join(helperErr, readerErr, readErr)}
		if readErr == nil {
			result.ExitCode = op.ExitCode
			switch op.State {
			case joined:
				result.Joined = true
				if op.Error != "" {
					result.Err = errors.Join(result.Err, fmt.Errorf("процесс: %s", op.Error))
				}
			case closed:
				result.Joined = true
				result.Err = errors.Join(result.Err, ErrClosed)
			default:
				result.Err = errors.Join(result.Err, ErrUnknown)
			}
		}
		result.Err = errors.Join(result.Err, finishBackend())
		s.mu.Lock()
		delete(s.processes, p.id)
		delete(s.tickets, p.id)
		s.mu.Unlock()
		p.result = result
		close(p.done)
	}()
	return p, nil
}

// RunHelper выполняется только отдельным исполняемым файлом; FD 3 читает канал lifeline основного процесса.
func RunHelper(args []string) (resultErr error) {
	if len(args) != 4 || args[0] != strconv.Itoa(ProtocolVersion) || !validID(args[2]) || len(args[3]) != 8 {
		return ErrInvalid
	}
	// err сообщает об отказе разбора восьмизначного номера native-операции в аргументах helper.
	if _, err := strconv.ParseUint(args[3], 10, 32); err != nil {
		return ErrInvalid
	}
	// root содержит открытый корень журнала; err сохраняет ошибки чтения, блокировок и исполнения
	// протокола helper.
	root, err := os.OpenRoot(args[1])
	if err != nil {
		return err
	}
	defer func() {
		// Закрывает корневой дескриптор журнала при возврате gost-process-helper.
		_ = root.Close()
	}()
	// h содержит проверенный заголовок области с бюджетами и сроком мягкой остановки.
	var h scopeHeader
	if err = readRecord(root, scopePath(args[2])+"/header.json", &h, 16<<20); err != nil {
		return err
	}
	if h.Version != ProtocolVersion || h.ID != args[2] || h.Limits.Validate() != nil || h.GracePeriod <= 0 {
		return ErrUnknown
	}
	syscall.CloseOnExec(3)
	if err = syscall.SetNonblock(3, true); err != nil {
		return err
	}
	// lifeline представляет входной FD 3 helper, который не наследуется целевым процессом.
	lifeline := os.NewFile(3, "backend-lifeline")
	if lifeline == nil {
		return ErrInvalid
	}
	// ctx отменяется при данных, EOF либо ошибке чтения lifeline; cancel прекращает ожидания и
	// запускает остановку целевого процесса.
	ctx, cancel := context.WithCancel(context.Background())
	// readerDone подтверждает возврат горутины чтения lifeline.
	readerDone := make(chan struct{})
	go func() {
		// Читает один байт из lifeline и отменяет контекст gost-process-helper после данных, EOF либо ошибки
		// чтения.

		defer close(readerDone)
		// b принимает один байт lifeline; любое завершение чтения приводит к отмене.
		var b [1]byte
		_, _ = lifeline.Read(b[:])
		cancel()
	}()
	// lifelineOpen исключает повторное закрытие дескриптора чтения lifeline.
	lifelineOpen := true
	// closeLifeline единожды отменяет контекст, закрывает lifeline и ждёт возврата его читающей
	// горутины.
	closeLifeline := func() error {
		// Однократно отменяет контекст, закрывает чтение lifeline и ждёт возврата читающей горутины.

		if !lifelineOpen {
			return nil
		}
		lifelineOpen = false
		cancel()
		// closeErr содержит результат закрытия дескриптора чтения lifeline.
		closeErr := lifeline.Close()
		<-readerDone
		return closeErr
	}
	defer func() {
		// Добавляет ошибку закрытия lifeline к итоговой ошибке gost-process-helper.
		resultErr = errors.Join(resultErr, closeLifeline())
	}()
	// r предоставляет helper доступ к точному корню журнала и сохранённым бюджетам области.
	r := &Registry{root: root, config: Config{Root: args[1], Limits: h.Limits}}
	// s представляет область целевой native-операции без владения backend-регистрацией.
	s := &Scope{registry: r, id: args[2]}
	// Краткое чтение журнала не отзывает регистрацию; EOF прекращает ожидание дубликата.
	// lock удерживает исключительную блокировку native-операции до сохранения её окончательного
	// результата.
	lock, err := lockExisting(ctx, root, operationPath(s.id, args[3])+".lock", true)
	if errors.Is(err, context.Canceled) {
		// Свободная блокировка позволяет подтвердить отказ до запуска даже при раннем EOF.
		lock, err = lockExisting(context.Background(), root, operationPath(s.id, args[3])+".lock", false)
	}
	if err != nil {
		return err
	}
	defer func() {
		// Освобождает блокировку нативной операции при любом возврате gost-process-helper.
		_ = unlock(lock)
	}()
	defer func() {
		// После ошибки при отменённом контексте закрывает ещё не запущенное резервирование и сохраняет ошибку
		// этой записи в результате.

		if resultErr == nil || ctx.Err() == nil {
			return
		}
		// op содержит запись ещё не запущенной операции; readErr сохраняет результат чтения и
		// возможной финальной записи CLOSED при ранней отмене.
		op, readErr := s.readOperation(args[3])
		if readErr == nil && op.State == reserved {
			op.State = closed
			readErr = writeRecord(root, operationPath(s.id, op.ID)+".json", op, h.Limits.MaxRecordBytes)
		}
		resultErr = errors.Join(resultErr, readErr)
	}()
	// gate исключает конкурентный Seal либо регистрацию во время окончательной проверки и начала
	// запуска.
	gate, err := lockExisting(ctx, root, scopePath(s.id)+"/gate.lock", true)
	if err != nil {
		return err
	}
	h, err = s.header()
	if err != nil {
		return errors.Join(err, unlock(gate))
	}
	// op содержит проверяемую native-операцию с сохранённой командой и состоянием RESERVED.
	op, err := s.readOperation(args[3])
	if err != nil {
		return errors.Join(err, unlock(gate))
	}
	if h.Sealed || h.Acknowledged || op.State != reserved || op.Kind != "native" || ctx.Err() != nil {
		if op.State == reserved {
			op.State = closed
			err = writeRecord(root, operationPath(s.id, op.ID)+".json", op, h.Limits.MaxRecordBytes)
		}
		return errors.Join(ErrClosed, ctx.Err(), err, unlock(gate))
	}
	if op.ExtraFiles < 0 || op.ExtraFiles > h.Limits.MaxExtraFiles {
		return errors.Join(ErrInvalid, unlock(gate))
	}
	// command восстанавливает целевую команду и ожидаемое число дополнительных дескрипторов из
	// журнала.
	command := Command{Binary: op.Binary, Args: op.Args, ExtraFiles: make([]*os.File, op.ExtraFiles)}
	// i обозначает позицию наследованного ExtraFiles, полученного helper начиная с FD 4.
	for i := range command.ExtraFiles {
		command.ExtraFiles[i] = os.NewFile(uintptr(4+i), "native-input")
		if command.ExtraFiles[i] == nil {
			return errors.Join(ErrInvalid, unlock(gate))
		}
	}
	if err = validateCommand(command, h.Limits); err != nil {
		return errors.Join(err, unlock(gate))
	}
	// Дескриптор записи lifeline остаётся только у основного процесса.
	// lifeline не наследуется целевым процессом; ExtraFiles нумеруются в нём с FD 3.
	// cmd представляет прямой целевой процесс, чьим единственным Wait владеет helper.
	cmd := exec.Command(command.Binary, command.Args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = command.ExtraFiles
	op.State = spawned
	if err = writeRecord(root, operationPath(s.id, op.ID)+".json", op, h.Limits.MaxRecordBytes); err != nil {
		return errors.Join(err, unlock(gate))
	}
	// startErr содержит исход запуска целевой команды после сохранения SPAWN_MARKER.
	startErr := cmd.Start()
	// gateErr объединяет освобождение допуска и ошибки закрытия копий ExtraFiles либо остановки
	// процесса.
	gateErr := unlock(gate)
	// f содержит очередной наследованный дополнительный дескриптор, закрываемый в helper после
	// Start.
	for _, f := range command.ExtraFiles {
		if err = f.Close(); err != nil {
			gateErr = errors.Join(gateErr, err)
		}
	}
	if startErr != nil {
		op.State = joined
		op.ExitCode = -1
		op.Error = errorText(startErr)
		return errors.Join(gateErr, closeLifeline(), writeRecord(root, operationPath(s.id, op.ID)+".json", op, h.Limits.MaxRecordBytes))
	}
	// waitDone передаёт единственный исход Wait прямого целевого процесса.
	waitDone := make(chan error, 1)
	go func() {
		// Выполняет единственный Wait прямого дочернего процесса и передаёт его результат владельцу остановки.
		waitDone <- cmd.Wait()
	}()
	// waitErr сохраняет результат завершения целевого процесса для долговечной записи.
	var waitErr error
	select {
	case waitErr = <-waitDone:
	// signalErr содержит результат отправки Interrupt после отзыва lifeline.
	// timer ограничивает мягкую остановку GracePeriod перед принудительным Kill.
	case <-ctx.Done():
		signalErr := cmd.Process.Signal(os.Interrupt)
		if signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
			gateErr = errors.Join(gateErr, signalErr)
		}
		timer := time.NewTimer(h.GracePeriod)
		select {
		case waitErr = <-waitDone:
			timer.Stop()
		// killErr содержит результат принудительной остановки после истечения GracePeriod.
		case <-timer.C:
			killErr := cmd.Process.Kill()
			if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				gateErr = errors.Join(gateErr, killErr)
			}
			waitErr = <-waitDone
		}
	}
	// Закрытие дескриптора чтения lifeline прерывает только чтение вспомогательного процесса;
	// владелец Wait продолжает ждать целевой процесс.
	// closeErr сохраняет результат завершения lifeline перед записью JOINED_RESULT.
	closeErr := closeLifeline()
	op.State = joined
	op.ExitCode = cmd.ProcessState.ExitCode()
	op.Error = errorText(waitErr)
	return errors.Join(gateErr, closeErr, writeRecord(root, operationPath(s.id, op.ID)+".json", op, h.Limits.MaxRecordBytes))
}

// Возвращает текст ненулевой ошибки для сохранённого результата процесса; nil представляет пустой
// строкой.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
