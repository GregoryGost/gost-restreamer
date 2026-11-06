package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// ErrResourceOwnership означает, что путь больше не принадлежит данной попытке.
var ErrResourceOwnership = errors.New("владелец ресурса изменился")

var ErrAlreadyAccepted = errors.New("направление UDS уже принято")

// Namespace владеет двумя listeners, соединениями и каталогом одной попытки.
type Namespace struct {
	InputPath  string
	OutputPath string

	mu             sync.Mutex
	dir            string
	dirInfo        os.FileInfo
	sessionDir     string
	sessionInfo    os.FileInfo
	parent         *namespaceParent
	input          *net.UnixListener
	output         *net.UnixListener
	inputInfo      os.FileInfo
	outputInfo     os.FileInfo
	connections    []net.Conn
	inputAccepted  bool
	outputAccepted bool
	shutdown       bool
	shutdownErr    error
	closed         bool
	closeErr       error
	scope          *processowner.Scope
	resources      []processowner.FileIdentity
	keepPaths      bool
}

func safeSegment(s string) bool {
	if len(s) == 0 || len(s) > 64 || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		valid := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !valid {
			return false
		}
	}
	return true
}

func verifyDirectory(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return nil, fmt.Errorf("%w: небезопасный каталог %s", ErrResourceOwnership, path)
	}
	return info, nil
}

// NewNamespace создаёт listeners до запуска процесса; SessionID используется как проверенный сегмент пути.
func NewNamespace(base, sessionID string) (*Namespace, error) {
	return newNamespace(base, sessionID, false)
}

func newNamespace(base, sessionID string, fileInput bool) (_ *Namespace, retErr error) {
	if !filepath.IsAbs(base) || !safeSegment(sessionID) {
		return nil, fmt.Errorf("%w: абсолютный base и безопасный SessionID обязательны", ErrInvalidSupervisor)
	}
	// Родитель base выбирает вызывающий; после канонизации дочерние symlink не принимаются.
	if err := os.MkdirAll(base, 0700); err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(base)
	if err != nil {
		return nil, err
	}
	if _, err = verifyDirectory(canonical); err != nil {
		return nil, err
	}
	n := &Namespace{sessionDir: filepath.Join(canonical, sessionID)}
	defer func() {
		if retErr != nil {
			if cleanupErr := n.Close(); cleanupErr != nil {
				retErr = &namespaceCreationError{cause: retErr, cleanup: cleanupErr}
			}
		}
	}()
	if n.parent, err = acquireNamespaceParent(n.sessionDir); err != nil {
		return nil, err
	}
	n.sessionInfo = n.parent.info
	if n.dir, err = os.MkdirTemp(n.sessionDir, "w-"); err != nil {
		return nil, err
	}
	if n.dirInfo, err = verifyDirectory(n.dir); err != nil {
		return nil, err
	}
	n.InputPath, n.OutputPath = filepath.Join(n.dir, "i.sock"), filepath.Join(n.dir, "o.sock")
	maxPath := 107
	if runtime.GOOS == "darwin" {
		maxPath = 103
	}
	if len(n.InputPath) > maxPath || len(n.OutputPath) > maxPath {
		return nil, fmt.Errorf("%w: слишком длинный путь UDS", ErrInvalidSupervisor)
	}
	if !fileInput {
		if n.input, n.inputInfo, err = listenOwned(n.InputPath); err != nil {
			return nil, err
		}
	} else {
		n.InputPath = ""
	}
	if n.output, n.outputInfo, err = listenOwned(n.OutputPath); err != nil {
		return nil, err
	}
	return n, nil
}

func newManagedNamespace(ctx context.Context, scope *processowner.Scope, fileInput bool) (_ *Namespace, retErr error) {
	base := scope.Namespace()
	if _, err := verifyDirectory(base); err != nil {
		return nil, err
	}
	n := &Namespace{scope: scope}
	defer func() {
		if retErr != nil {
			if cleanup := n.Close(); cleanup != nil {
				retErr = &namespaceCreationError{cause: retErr, cleanup: cleanup}
			}
		}
	}()
	var err error
	if n.dir, err = os.MkdirTemp(base, "w-"); err != nil {
		return nil, err
	}
	register := func(name, kind string) error {
		relative, err := filepath.Rel(base, name)
		if err != nil {
			return err
		}
		identity, err := scope.RuntimeIdentity(relative, kind)
		if err != nil {
			return err
		}
		n.resources = append(n.resources, identity)
		return scope.RegisterRuntime(ctx, relative, kind)
	}
	if err = register(n.dir, processowner.Directory); err != nil {
		return nil, err
	}
	n.InputPath, n.OutputPath = filepath.Join(n.dir, "i.sock"), filepath.Join(n.dir, "o.sock")
	maxPath := 107
	if runtime.GOOS == "darwin" {
		maxPath = 103
	}
	if len(n.InputPath) > maxPath || len(n.OutputPath) > maxPath {
		return nil, fmt.Errorf("%w: слишком длинный путь UDS", ErrInvalidSupervisor)
	}
	if !fileInput {
		if n.input, n.inputInfo, err = listenOwned(n.InputPath); err != nil {
			return nil, err
		}
		if err = register(n.InputPath, processowner.Socket); err != nil {
			return nil, err
		}
	} else {
		n.InputPath = ""
	}
	if n.output, n.outputInfo, err = listenOwned(n.OutputPath); err != nil {
		return nil, err
	}
	if err = register(n.OutputPath, processowner.Socket); err != nil {
		return nil, err
	}
	return n, nil
}

func listenOwned(path string) (*net.UnixListener, os.FileInfo, error) {
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, nil, err
	}
	l.SetUnlinkOnClose(false)
	info, err := os.Lstat(path)
	if err == nil {
		err = os.Chmod(path, 0600)
	}
	if err != nil {
		return nil, nil, errors.Join(err, l.Close(), removeOwned(path, info))
	}
	return l, info, nil
}

func (n *Namespace) accept(input bool) (net.Conn, error) {
	n.mu.Lock()
	if n.closed || n.shutdown {
		n.mu.Unlock()
		return nil, net.ErrClosed
	}
	l := n.output
	if input {
		if n.inputAccepted {
			n.mu.Unlock()
			return nil, ErrAlreadyAccepted
		}
		n.inputAccepted = true
		l = n.input
	} else {
		if n.outputAccepted {
			n.mu.Unlock()
			return nil, ErrAlreadyAccepted
		}
		n.outputAccepted = true
	}
	n.mu.Unlock()
	if l == nil {
		return nil, net.ErrClosed
	}
	c, err := l.Accept()
	if err != nil {
		return nil, err
	}
	if err = l.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return nil, errors.Join(err, c.Close())
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed || n.shutdown {
		return nil, errors.Join(net.ErrClosed, c.Close())
	}
	n.connections = append(n.connections, c)
	return c, nil
}

// AcceptInput ожидает соединение FFmpeg для чтения входа; Close прерывает ожидание.
func (n *Namespace) AcceptInput() (net.Conn, error) { return n.accept(true) }

// AcceptOutput ожидает соединение FFmpeg для записи результата; Close прерывает ожидание.
func (n *Namespace) AcceptOutput() (net.Conn, error) { return n.accept(false) }

func removeOwned(path string, original os.FileInfo) error {
	if path == "" || original == nil {
		return nil
	}
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(original, current) {
		return fmt.Errorf("%w: %s", ErrResourceOwnership, path)
	}
	return os.Remove(path)
}

// Shutdown закрывает дескрипторы и разблокирует Accept/I/O, сохраняя пути до join.
func (n *Namespace) Shutdown() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.shutdownLocked()
}

func (n *Namespace) shutdownLocked() error {
	if n.shutdown {
		return n.shutdownErr
	}
	n.shutdown = true
	var errs []error
	for _, c := range n.connections {
		if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	n.connections = nil
	for _, l := range []*net.UnixListener{n.input, n.output} {
		if l != nil {
			if err := l.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		}
	}
	n.shutdownErr = errors.Join(errs...)
	return n.shutdownErr
}

// Close удаляет только собственные пути после завершения их пользователей.
func (n *Namespace) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return n.closeErr
	}
	n.closed = true
	errs := []error{n.shutdownLocked()}
	if n.scope != nil {
		if n.keepPaths {
			n.closeErr = errors.Join(n.shutdownErr, ErrCleanupPending)
			return n.closeErr
		}
		for i := len(n.resources) - 1; i >= 0; i-- {
			errs = append(errs, n.scope.RuntimeJournal().Remove(context.Background(), n.resources[i]))
		}
		n.closeErr = errors.Join(errs...)
		return n.closeErr
	}
	n.closeErr = errors.Join(append(errs, n.closeUnmanaged())...)
	return n.closeErr
}

func (n *Namespace) closeUnmanaged() (retErr error) {
	// Удержание родителя возвращается и при отказе проверки вложенных ресурсов.
	defer func() { retErr = errors.Join(retErr, releaseNamespaceParent(n.parent)) }()
	// Проверка родителей предшествует доступу к вложенным путям.
	if err := sameDirectory(n.sessionDir, n.sessionInfo); err != nil {
		return err
	}
	if n.dirInfo != nil {
		if err := sameDirectory(n.dir, n.dirInfo); err != nil {
			return err
		}
	}
	return errors.Join(removeOwned(n.InputPath, n.inputInfo), removeOwned(n.OutputPath, n.outputInfo), removeOwned(n.dir, n.dirInfo))
}

func sameDirectory(path string, original os.FileInfo) error {
	if original == nil {
		return nil
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !current.IsDir() || !os.SameFile(original, current) {
		return fmt.Errorf("%w: %s", ErrResourceOwnership, path)
	}
	return nil
}

// namespaceCreationError сохраняет отдельный результат частичной очистки startup.
type namespaceCreationError struct{ cause, cleanup error }

func (e *namespaceCreationError) Error() string   { return errors.Join(e.cause, e.cleanup).Error() }
func (e *namespaceCreationError) Unwrap() []error { return []error{e.cause, e.cleanup} }
