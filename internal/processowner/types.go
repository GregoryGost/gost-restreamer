// Package processowner учитывает физическое владение процессами и файловыми объектами.
package processowner

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"
)

const ProtocolVersion = 1

var (
	ErrInvalid = errors.New("некорректные параметры владельца процессов")
	ErrLimit   = errors.New("превышен бюджет журнала владельца процессов")
	ErrClosed  = errors.New("регистрация владельца процессов закрыта")
	ErrPending = errors.New("физическое завершение ещё не подтверждено")
	ErrUnknown = errors.New("доказательство физического завершения неизвестно")
)

type Limits struct {
	MaxOperations   int
	MaxRecordBytes  int64
	MaxCommandArgs  int
	MaxCommandBytes int
	MaxExtraFiles   int
	PageSize        int
}

func (l Limits) Validate() error {
	if l.MaxOperations <= 0 || l.MaxOperations > 100000 || l.MaxRecordBytes < 1024 || l.MaxRecordBytes > 16<<20 || l.MaxCommandArgs <= 0 || l.MaxCommandArgs > 4096 || l.MaxCommandBytes <= 0 || l.MaxCommandBytes > int(l.MaxRecordBytes)/2 || l.MaxExtraFiles < 0 || l.MaxExtraFiles > 128 || l.PageSize <= 0 || l.PageSize > 1000 {
		return ErrInvalid
	}
	return nil
}

type Config struct {
	Root        string
	RuntimeRoot string
	HelperPath  string
	Limits      Limits
	GracePeriod time.Duration
}

type Command struct {
	Binary         string
	Args           []string
	ExtraFiles     []*os.File
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

type Result struct {
	Joined   bool
	ExitCode int
	Err      error
}

type ScopeResult struct {
	ID, Owner                             string
	Sealed, Joined, Unknown, Acknowledged bool
	Operations, PendingOperations         int
}

type Registry struct {
	config  Config
	root    *os.Root
	runtime *FileJournal
}

func (r *Registry) PageLimit() int { return r.config.Limits.PageSize }

type Scope struct {
	registry  *Registry
	id        string
	backend   *os.File
	mu        sync.Mutex
	closed    bool
	closing   bool
	tickets   map[string]*Ticket
	processes map[string]*Process
}

type Ticket struct {
	scope               *Scope
	id                  string
	kind                string
	completionRequested bool
	completed           bool
}

type Process struct {
	scope    *Scope
	id       string
	lifeline *os.File
	helper   *os.Process
	once     sync.Once
	done     chan struct{}
	result   Result
}

func (s *Scope) ID() string              { return s.id }
func (s *Scope) Namespace() string       { return s.registry.config.RuntimeRoot + "/" + s.id }
func (p *Process) Done() <-chan struct{} { return p.done }
func (p *Process) Cancel()               { p.once.Do(func() { _ = p.lifeline.Close() }) }

// Wait ограничивает ожидание вызывающего, но не уничтожает владельца native Wait.
func (p *Process) Wait(ctx context.Context) (Result, error) {
	select {
	case <-p.done:
		return p.result, nil
	default:
	}
	select {
	case <-p.done:
		return p.result, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

func (s *Scope) Run(ctx context.Context, command Command) (Result, error) {
	p, err := s.Start(ctx, command)
	if err != nil {
		return Result{}, err
	}
	result, err := p.Wait(ctx)
	if err != nil {
		p.Cancel()
		result, joinErr := p.Wait(context.Background())
		return result, errors.Join(err, joinErr, result.Err)
	}
	return result, result.Err
}

type FileIdentity struct{ FileID, GenerationID, ManagedPath, Kind, Owner string }

const (
	RegularFile = "regular"
	Directory   = "directory"
	Socket      = "socket"
)
