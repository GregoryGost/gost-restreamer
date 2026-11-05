package ffmpeg

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// Supervisor владеет одним логическим worker и всеми его последовательными попытками.
type Supervisor struct {
	config   Config
	queue    *diagnosticQueue
	mu       sync.Mutex
	started  bool
	stopping bool
	cancel   context.CancelFunc
	done     chan struct{}
	result   Result
}

func New(config Config) (*Supervisor, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Supervisor{config: config, queue: newDiagnosticQueue(config.DiagnosticEvents, config.DiagnosticBytes), done: make(chan struct{})}, nil
}

// Start регистрирует учитываемую работу под Owner.Do, затем запускает её вне authority.
// ctx задаёт lifetime worker, а не lifetime публикации OBS.
func (s *Supervisor) Start(ctx context.Context, w Worker) error {
	if w.Owner == nil || w.Command == nil || (w.Topology == Duplex && w.Input == nil) || w.Topology > FileInput || w.Output == nil || w.SessionID == "" || w.GroupID == "" || len(w.SessionID) > 256 || len(w.GroupID) > 256 {
		return fmt.Errorf("%w: неполный Worker", ErrInvalidSupervisor)
	}
	var runCtx context.Context
	registered := false
	err := w.Owner.Do(ctx, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.stopping {
			return ErrStopped
		}
		if s.started {
			return ErrAlreadyStarted
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		s.started = true
		s.queue.setOwner(w.SessionID, w.GroupID)
		runCtx, s.cancel = context.WithCancel(ctx)
		registered = true
		return nil
	})
	if registered {
		go s.run(runCtx, w)
	}
	return err
}

// Done закрывается только после join всех зарегистрированных операций и cleanup.
func (s *Supervisor) Done() <-chan struct{} { return s.done }

func (s *Supervisor) NextDiagnostic(ctx context.Context) (Diagnostic, error) {
	return s.queue.next(ctx)
}
func (s *Supervisor) DroppedDiagnostics() uint64 { return s.queue.losses() }

// Stop сначала запрещает новые попытки, затем ожидает физического завершения.
// По истечении срока Done остаётся наблюдаемым; повторный Stop может дождаться cleanup.
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	s.stopping = true
	if !s.started {
		s.started = true
		s.result.Clean = true
		s.queue.close()
		close(s.done)
	} else if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	waitCtx, cancel := context.WithTimeout(ctx, s.config.ShutdownTimeout)
	defer cancel()
	result, err := s.Wait(waitCtx)
	if err != nil {
		return errors.Join(ErrCleanupPending, err)
	}
	return result.CleanupErr
}

// Wait возвращает исход worker отдельно от ошибок ожидания и физической очистки.
func (s *Supervisor) Wait(ctx context.Context) (Result, error) {
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.result, nil
	default:
	}
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.result, nil
	}
}

func (s *Supervisor) run(ctx context.Context, w Worker) {
	// Наблюдатель отзывает worker даже во время внешнего CommandBuilder/Start.
	observerDone := make(chan struct{})
	observerJoined := make(chan struct{})
	go func() {
		defer close(observerJoined)
		select {
		case <-w.Owner.Done():
			s.mu.Lock()
			s.cancel()
			s.mu.Unlock()
		case <-observerDone:
		}
	}()
	result := Result{}
	for {
		if err := ctx.Err(); err != nil {
			result.Err = err
			break
		}
		// Ticket существует до полного возврата runAttempt, включая отложенный builder и join.
		err := w.Owner.Do(ctx, func() error {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.stopping {
				return ErrStopped
			}
			return ctx.Err()
		})
		if err != nil {
			result.Err = err
			break
		}
		result.Attempts++
		attemptErr, cleanupErr := s.runAttempt(ctx, w, result.Attempts)
		result.Err, result.CleanupErr = attemptErr, errors.Join(result.CleanupErr, cleanupErr)
		if cleanupErr != nil || attemptErr == nil || ctx.Err() != nil || !w.Owner.Valid() || result.Attempts > uint64(s.config.MaxRestarts) {
			break
		}
		timer := time.NewTimer(s.config.RestartBackoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			result.Err = ctx.Err()
		case <-timer.C:
			continue
		}
		break
	}
	close(observerDone)
	<-observerJoined
	if !w.Owner.Valid() {
		result.Err = errors.Join(result.Err, ErrOwnerRevoked)
	}
	result.Clean = result.CleanupErr == nil
	s.mu.Lock()
	s.stopping = true
	s.result = result
	s.cancel()
	s.queue.close()
	close(s.done)
	s.mu.Unlock()
}

type handlerResult struct {
	output bool
	err    error
}

type packetProgress struct {
	mu   sync.Mutex
	seen [2]bool
	dts  [2]int64
	at   [2]time.Time
}

func (p *packetProgress) report(track Track, dts int64) bool {
	if track != Video && track != Audio {
		return false
	}
	i := int(track - 1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen[i] && dts <= p.dts[i] {
		return false
	}
	p.seen[i], p.dts[i], p.at[i] = true, dts, time.Now()
	return true
}
func (p *packetProgress) health(now, start time.Time, c Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.seen[0] || !p.seen[1] {
		if now.Sub(start) >= c.StartupTimeout {
			return ErrStartupTimeout
		}
		return nil
	}
	if now.Sub(p.at[0]) >= c.VideoTimeout || now.Sub(p.at[1]) >= c.AudioTimeout {
		return ErrProgressTimeout
	}
	return nil
}

func (s *Supervisor) runAttempt(parent context.Context, w Worker, id uint64) (retErr, cleanupErr error) {
	defer func() {
		if errors.Is(retErr, ErrCleanupPending) || errors.Is(retErr, processowner.ErrPending) || errors.Is(retErr, processowner.ErrUnknown) {
			cleanupErr = errors.Join(cleanupErr, ErrCleanupPending)
		}
	}()
	ctx, cancelCause := context.WithCancelCause(parent)
	cancel := func() { cancelCause(context.Canceled) }
	defer cancel()
	if w.ProcessScope != nil {
		ticket, err := w.ProcessScope.Register(ctx, "ffmpeg-attempt")
		if err != nil {
			return err, nil
		}
		defer func() {
			complete, stop := context.WithTimeout(context.Background(), s.config.ShutdownTimeout)
			defer stop()
			cleanupErr = errors.Join(cleanupErr, ticket.Complete(complete))
		}()
	}
	sessionHash := fmt.Sprintf("%x", sha256.Sum256([]byte(w.SessionID)))[:16]
	var n *Namespace
	var err error
	if w.ProcessScope != nil {
		n, err = newManagedNamespace(ctx, w.ProcessScope, w.Topology == FileInput)
	} else {
		n, err = newNamespace(s.config.BaseDir, sessionHash, w.Topology == FileInput)
	}
	if err != nil {
		var partial *namespaceCreationError
		if errors.As(err, &partial) {
			return err, partial.cleanup
		}
		return err, nil
	}
	shutdownDone := make(chan struct{})
	var shutdownErr error
	stopShutdown := context.AfterFunc(ctx, func() { shutdownErr = n.Shutdown(); close(shutdownDone) })
	var ioWG sync.WaitGroup
	ioResults := make(chan handlerResult, 2)
	var handlerErrors [2]error
	progress := &packetProgress{}
	startupJoined := make(chan struct{})
	startup := time.AfterFunc(s.config.StartupTimeout, func() {
		defer close(startupJoined)
		progress.mu.Lock()
		ready := progress.seen[0] && progress.seen[1]
		progress.mu.Unlock()
		if !ready {
			cancelCause(ErrStartupTimeout)
		}
	})
	defer func() {
		if !startup.Stop() {
			<-startupJoined
		}
	}()
	handler := func(accept func() (net.Conn, error), fn IOHandler, report Progress, output bool) {
		defer ioWG.Done()
		c, acceptErr := accept()
		if acceptErr == nil {
			acceptErr = fn(ctx, c, report)
			closeErr := c.Close()
			if closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				acceptErr = errors.Join(acceptErr, closeErr)
			}
		}
		index := 0
		if output {
			index = 1
		}
		handlerErrors[index] = acceptErr
		ioResults <- handlerResult{output: output, err: acceptErr}
	}
	if w.Topology == Duplex {
		ioWG.Add(1)
		go handler(n.AcceptInput, w.Input, func(Track, int64) bool { return false }, false)
	}
	ioWG.Add(1)
	go handler(n.AcceptOutput, w.Output, func(t Track, dts int64) bool {
		if ctx.Err() != nil {
			return false
		}
		return progress.report(t, dts)
	}, true)
	defer func() {
		cancel()
		if stopShutdown() {
			cleanupErr = errors.Join(cleanupErr, n.Shutdown())
		} else {
			<-shutdownDone
			cleanupErr = errors.Join(cleanupErr, shutdownErr)
		}
		ioWG.Wait()
		for _, handlerErr := range handlerErrors {
			if handlerErr != nil && !errors.Is(handlerErr, io.EOF) && !errors.Is(handlerErr, context.Canceled) && !errors.Is(handlerErr, net.ErrClosed) {
				retErr = errors.Join(retErr, handlerErr)
			}
		}
		if errors.Is(retErr, ErrCleanupPending) || errors.Is(retErr, processowner.ErrPending) || errors.Is(retErr, processowner.ErrUnknown) {
			n.keepPaths = true
		}
		cleanupErr = errors.Join(cleanupErr, n.Close())
	}()
	if ctx.Err() != nil {
		return context.Cause(ctx), nil
	}
	command, err := w.Command(ctx, Attempt{ID: id, InputPath: n.InputPath, OutputPath: n.OutputPath})
	defer func() {
		for _, f := range command.ExtraFiles {
			if f != nil {
				cleanupErr = errors.Join(cleanupErr, f.Close())
			}
		}
	}()
	if err != nil {
		return errors.Join(err, context.Cause(ctx)), nil
	}
	if ctx.Err() != nil {
		return context.Cause(ctx), nil
	}
	if (w.Topology == FileInput && len(command.ExtraFiles) != 1) || (w.Topology == Duplex && len(command.ExtraFiles) != 0) {
		return fmt.Errorf("%w: несогласованное владение ExtraFiles", ErrInvalidSupervisor), nil
	}
	if w.Topology == FileInput {
		file := command.ExtraFiles[0]
		if file == nil {
			return fmt.Errorf("%w: отсутствует дескриптор файла", ErrInvalidSupervisor), nil
		}
		info, err := file.Stat()
		if err != nil {
			return err, nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: требуется обычный файл", ErrInvalidSupervisor), nil
		}
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return err, nil
		}
	}
	if len(command.Args) > s.config.MaxCommandArgs || !validCommand(command.Binary, command.Args, s.config.MaxCommandBytes) {
		return fmt.Errorf("%w: команда превышает бюджет", ErrInvalidSupervisor), nil
	}
	// argv копируются до передачи процессу и диагностики.
	command.Args = append([]string(nil), command.Args...)
	s.queue.push(Diagnostic{Time: time.Now(), Attempt: id, Kind: "command", Args: append([]string{command.Binary}, command.Args...)})
	processDone, err := s.startProcess(ctx, w.ProcessScope, command, id)
	if err != nil {
		return err, nil
	}
	ticker := time.NewTicker(s.config.WatchdogInterval)
	defer ticker.Stop()
	start := time.Now()
	var reason error
	outputFinished := false
selectLoop:
	for {
		select {
		case err = <-processDone:
			if ctx.Err() != nil {
				return errors.Join(context.Cause(ctx), err), nil
			}
			if err != nil {
				progress.mu.Lock()
				ready := progress.seen[0] && progress.seen[1]
				progress.mu.Unlock()
				if !ready {
					return errors.Join(ErrPrematureExit, err), nil
				}
				return err, nil
			}
			// Wait не доказывает, что downstream уже прочитал переданный сокету хвост.
			drain := time.NewTimer(s.config.ShutdownTimeout)
			for !outputFinished {
				select {
				case result := <-ioResults:
					if result.output {
						outputFinished = true
					}
					if result.err != nil && !errors.Is(result.err, io.EOF) && result.output {
						drain.Stop()
						return result.err, nil
					}
				case <-ctx.Done():
					drain.Stop()
					return context.Cause(ctx), nil
				case <-drain.C:
					return ErrOutputDrainTimeout, nil
				}
			}
			drain.Stop()
			progress.mu.Lock()
			ready := progress.seen[0] && progress.seen[1]
			progress.mu.Unlock()
			if !ready {
				return ErrPrematureExit, nil
			}
			return nil, nil
		case result := <-ioResults:
			if result.output {
				outputFinished = true
			}
			err = result.err
			if err == nil || errors.Is(err, io.EOF) {
				continue
			}
			reason = err
			break selectLoop
		case <-ctx.Done():
			reason = context.Cause(ctx)
			break selectLoop
		case now := <-ticker.C:
			if err = progress.health(now, start, s.config); err != nil {
				reason = err
				break selectLoop
			}
		}
	}
	cancel()
	// CommandContext отправляет Interrupt, затем WaitDelay обеспечивает Kill.
	deadline := time.NewTimer(s.config.GracePeriod + s.config.KillTimeout)
	defer deadline.Stop()
	select {
	case err = <-processDone:
		return errors.Join(reason, err), nil
	case <-deadline.C:
		s.queue.push(Diagnostic{Time: time.Now(), Attempt: id, Kind: "cleanup_pending", Message: ErrCleanupPending.Error()})
		// Timeout не превращается в ложный cleanup: Wait остаётся единственным владельцем.
		err = <-processDone
		return errors.Join(reason, err), nil
	}
}

func (s *Supervisor) startProcess(ctx context.Context, scope *processowner.Scope, command Command, id uint64) (<-chan error, error) {
	stdout := &diagnosticWriter{queue: s.queue, attempt: id, kind: "stdout", chunk: s.config.DiagnosticChunkBytes}
	stderr := &diagnosticWriter{queue: s.queue, attempt: id, kind: "stderr", chunk: s.config.DiagnosticChunkBytes}
	done := make(chan error, 1)
	if scope != nil {
		process, err := scope.Start(ctx, processowner.Command{Binary: command.Binary, Args: command.Args, ExtraFiles: command.ExtraFiles, Stdout: stdout, Stderr: stderr})
		if err != nil {
			return nil, err
		}
		go func() {
			result, waitErr := process.Wait(context.Background())
			if !result.Joined {
				waitErr = errors.Join(waitErr, ErrCleanupPending)
			}
			done <- errors.Join(result.Err, waitErr)
		}()
		return done, nil
	}
	// Прямой запуск оставлен отдельным портом низкоуровневых тестов компонента.
	cmd := exec.CommandContext(ctx, command.Binary, command.Args...)
	cmd.ExtraFiles = append([]*os.File(nil), command.ExtraFiles...)
	cmd.Cancel = func() error {
		err := cmd.Process.Signal(os.Interrupt)
		if errors.Is(err, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = s.config.GracePeriod
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { done <- cmd.Wait() }()
	return done, nil
}
