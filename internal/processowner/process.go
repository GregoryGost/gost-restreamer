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

func (s *Scope) Owner() (string, error) { h, err := s.header(); return h.Owner, err }

func validateCommand(command Command, limits Limits) error {
	if len(command.Args) > limits.MaxCommandArgs || len(command.ExtraFiles) > limits.MaxExtraFiles {
		return ErrLimit
	}
	if !utf8.ValidString(command.Binary) {
		return ErrInvalid
	}
	for _, arg := range command.Args {
		if !utf8.ValidString(arg) {
			return ErrInvalid
		}
	}
	if command.Binary == "" || strings.ContainsRune(command.Binary, 0) || len(command.Args) > limits.MaxCommandArgs || len(command.ExtraFiles) > limits.MaxExtraFiles {
		return ErrInvalid
	}
	n := len(command.Binary)
	for _, arg := range command.Args {
		if strings.ContainsRune(arg, 0) || len(arg) > limits.MaxCommandBytes-n {
			return ErrLimit
		}
		n += len(arg)
	}
	if n > limits.MaxCommandBytes {
		return ErrLimit
	}
	for _, f := range command.ExtraFiles {
		if f == nil {
			return ErrInvalid
		}
	}
	return nil
}

func (s *Scope) closeReserved(ctx context.Context, id string) error {
	lock, err := lockExisting(ctx, s.registry.root, operationPath(s.id, id)+".lock", true)
	if err != nil {
		return err
	}
	defer func() { _ = unlock(lock) }()
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

func (s *Scope) Start(ctx context.Context, command Command) (*Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateCommand(command, s.registry.config.Limits); err != nil {
		return nil, err
	}
	backendTicket, err := s.Register(ctx, "native-io")
	if err != nil {
		return nil, err
	}
	finishBackend := func() error { return backendTicket.Complete(context.Background()) }
	nativeTicket, err := s.register(ctx, "native", &command, nil)
	if err != nil {
		return nil, errors.Join(err, finishBackend())
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, s.closeReserved(context.Background(), nativeTicket.id), finishBackend())
	}
	p := &Process{scope: s, id: nativeTicket.id, lifeline: writer, done: make(chan struct{})}
	cmd := exec.Command(s.registry.config.HelperPath, strconv.Itoa(ProtocolVersion), s.registry.config.Root, s.id, p.id)
	cmd.ExtraFiles = append([]*os.File{reader}, command.ExtraFiles...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = command.Stdin, command.Stdout, command.Stderr
	// CommandContext здесь уничтожил бы единственного владельца native Wait при отмене backend.
	if err = cmd.Start(); err != nil {
		p.Cancel()
		return nil, errors.Join(err, reader.Close(), s.closeReserved(context.Background(), p.id), finishBackend())
	}
	readerErr := reader.Close()
	p.helper = cmd.Process
	s.mu.Lock()
	s.processes[p.id] = p
	alreadyClosed := s.closed || s.closing
	s.mu.Unlock()
	stopCancel := context.AfterFunc(ctx, p.Cancel)
	if alreadyClosed {
		p.Cancel()
	}
	go func() {
		helperErr := cmd.Wait()
		stopCancel()
		p.Cancel()
		op, readErr := s.readOperation(p.id)
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

// RunHelper выполняется только отдельным executable; FD 3 принадлежит lifeline backend.
func RunHelper(args []string) (resultErr error) {
	if len(args) != 4 || args[0] != strconv.Itoa(ProtocolVersion) || !validID(args[2]) || len(args[3]) != 8 {
		return ErrInvalid
	}
	if _, err := strconv.ParseUint(args[3], 10, 32); err != nil {
		return ErrInvalid
	}
	root, err := os.OpenRoot(args[1])
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
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
	lifeline := os.NewFile(3, "backend-lifeline")
	if lifeline == nil {
		return ErrInvalid
	}
	ctx, cancel := context.WithCancel(context.Background())
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		var b [1]byte
		_, _ = lifeline.Read(b[:])
		cancel()
	}()
	lifelineOpen := true
	closeLifeline := func() error {
		if !lifelineOpen {
			return nil
		}
		lifelineOpen = false
		cancel()
		closeErr := lifeline.Close()
		<-readerDone
		return closeErr
	}
	defer func() { resultErr = errors.Join(resultErr, closeLifeline()) }()
	r := &Registry{root: root, config: Config{Root: args[1], Limits: h.Limits}}
	s := &Scope{registry: r, id: args[2]}
	// Краткое чтение журнала не отзывает регистрацию; EOF прекращает ожидание дубликата.
	lock, err := lockExisting(ctx, root, operationPath(s.id, args[3])+".lock", true)
	if errors.Is(err, context.Canceled) {
		// Свободная блокировка позволяет подтвердить отказ до запуска даже при раннем EOF.
		lock, err = lockExisting(context.Background(), root, operationPath(s.id, args[3])+".lock", false)
	}
	if err != nil {
		return err
	}
	defer func() { _ = unlock(lock) }()
	defer func() {
		if resultErr == nil || ctx.Err() == nil {
			return
		}
		op, readErr := s.readOperation(args[3])
		if readErr == nil && op.State == reserved {
			op.State = closed
			readErr = writeRecord(root, operationPath(s.id, op.ID)+".json", op, h.Limits.MaxRecordBytes)
		}
		resultErr = errors.Join(resultErr, readErr)
	}()
	gate, err := lockExisting(ctx, root, scopePath(s.id)+"/gate.lock", true)
	if err != nil {
		return err
	}
	h, err = s.header()
	if err != nil {
		return errors.Join(err, unlock(gate))
	}
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
	command := Command{Binary: op.Binary, Args: op.Args, ExtraFiles: make([]*os.File, op.ExtraFiles)}
	for i := range command.ExtraFiles {
		command.ExtraFiles[i] = os.NewFile(uintptr(4+i), "native-input")
		if command.ExtraFiles[i] == nil {
			return errors.Join(ErrInvalid, unlock(gate))
		}
	}
	if err = validateCommand(command, h.Limits); err != nil {
		return errors.Join(err, unlock(gate))
	}
	// Writer lifeline отсутствует у helper и native child; FD 3 не передаётся native.
	cmd := exec.Command(command.Binary, command.Args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = command.ExtraFiles
	op.State = spawned
	if err = writeRecord(root, operationPath(s.id, op.ID)+".json", op, h.Limits.MaxRecordBytes); err != nil {
		return errors.Join(err, unlock(gate))
	}
	startErr := cmd.Start()
	gateErr := unlock(gate)
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
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-ctx.Done():
		signalErr := cmd.Process.Signal(os.Interrupt)
		if signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
			gateErr = errors.Join(gateErr, signalErr)
		}
		timer := time.NewTimer(h.GracePeriod)
		select {
		case waitErr = <-waitDone:
			timer.Stop()
		case <-timer.C:
			killErr := cmd.Process.Kill()
			if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				gateErr = errors.Join(gateErr, killErr)
			}
			waitErr = <-waitDone
		}
	}
	// Закрытие read-end прерывает только собственное чтение helper, не Wait-owner.
	closeErr := closeLifeline()
	op.State = joined
	op.ExitCode = cmd.ProcessState.ExitCode()
	op.Error = errorText(waitErr)
	return errors.Join(gateErr, closeErr, writeRecord(root, operationPath(s.id, op.ID)+".json", op, h.Limits.MaxRecordBytes))
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
