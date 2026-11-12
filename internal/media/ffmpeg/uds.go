package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"

	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// ErrResourceOwnership означает, что путь больше не принадлежит данной попытке.
var ErrResourceOwnership = errors.New("владелец ресурса изменился")

// ErrAlreadyAccepted отклоняет повторный приём того же направления UDS одной попытки.
var ErrAlreadyAccepted = errors.New("направление UDS уже принято")

// Namespace владеет слушателями UDS, соединениями и каталогом одной попытки.
// Duplex использует два слушателя, FileInput — только выходной; InputPath при этом пуст.
type Namespace struct {
	// InputPath задаёт входной UDS попытки и остаётся пустым при FileInput.
	InputPath string
	// OutputPath задаёт выходной UDS попытки.
	OutputPath string

	// mu защищает приём направлений, список соединений и состояние закрытия Namespace.
	mu sync.Mutex
	// dir содержит путь отдельного каталога физической попытки.
	dir string
	// dirInfo сохраняет идентичность каталога неуправляемой попытки для безопасного удаления.
	dirInfo os.FileInfo
	// sessionDir содержит путь общего каталога Session у Namespace без Scope.
	sessionDir string
	// sessionInfo сохраняет идентичность общего каталога Session для проверки перед очисткой.
	sessionInfo os.FileInfo
	// parent удерживает общий каталог Session до освобождения последнего неуправляемого Namespace.
	parent *namespaceParent
	// input принимает соединение FFmpeg для передачи входных медиаданных; при FileInput остаётся
	// nil.
	input *net.UnixListener
	// output принимает соединение FFmpeg для чтения выходных медиаданных.
	output *net.UnixListener
	// inputInfo сохраняет идентичность входного сокета для безопасного удаления без Scope.
	inputInfo os.FileInfo
	// outputInfo сохраняет идентичность выходного сокета для безопасного удаления без Scope.
	outputInfo os.FileInfo
	// connections хранит принятые соединения, которые Shutdown должен закрыть.
	connections []net.Conn
	// inputAccepted отмечает уже зарезервированный приём входного направления.
	inputAccepted bool
	// outputAccepted отмечает уже зарезервированный приём выходного направления.
	outputAccepted bool
	// shutdown отмечает начало закрытия дескрипторов и запрещает принимать новые соединения.
	shutdown bool
	// shutdownErr сохраняет объединённую ошибку закрытия соединений и слушателей.
	shutdownErr error
	// closed отмечает уже выполненный Close и сохраняет его результат для повторного вызова.
	closed bool
	// closeErr хранит результат очистки путей и закрытия дескрипторов.
	closeErr error
	// scope задаёт журнал и пространство имён управляемых временных ресурсов попытки.
	scope *processowner.Scope
	// resources хранит идентичности зарегистрированных ресурсов в порядке создания для обратного
	// удаления.
	resources []processowner.FileIdentity
	// keepPaths запрещает удалять управляемые пути, пока завершение пользователей не подтверждено.
	keepPaths bool
}

// Допускает непустой сегмент пути до 64 ASCII-букв, цифр, дефисов и подчёркиваний, исключая точки и
// переход к родителю.
func safeSegment(s string) bool {
	if len(s) == 0 || len(s) > 64 || s == "." || s == ".." {
		return false
	}
	// c содержит очередной символ сегмента пути для проверки допустимого ASCII-набора.
	for _, c := range s {
		// valid показывает, разрешён ли текущий символ в отдельном сегменте пути.
		valid := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !valid {
			return false
		}
	}
	return true
}

// Проверяет существующий каталог без символической ссылки и прав записи группы или остальных
// пользователей, возвращая его файловую идентичность.
func verifyDirectory(path string) (os.FileInfo, error) {
	// info содержит метаданные пути без перехода по символической ссылке; err сообщает об отказе их
	// чтения.
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return nil, fmt.Errorf("%w: небезопасный каталог %s", ErrResourceOwnership, path)
	}
	return info, nil
}

// NewNamespace создаёт слушатели до запуска процесса; sessionID используется как проверенный сегмент пути.
func NewNamespace(base, sessionID string) (*Namespace, error) {
	return newNamespace(base, sessionID, false)
}

// Создаёт отдельный каталог попытки и слушатели до запуска процесса с учётом длины UDS; при FileInput
// создаёт только выход, а частичный отказ пытается очистить.
func newNamespace(base, sessionID string, fileInput bool) (_ *Namespace, retErr error) {
	if !filepath.IsAbs(base) || !safeSegment(sessionID) {
		return nil, fmt.Errorf("%w: абсолютный base и безопасный SessionID обязательны", ErrInvalidSupervisor)
	}
	// Родительский каталог base выбирает вызывающий; после канонизации дочерние символические ссылки не принимаются.
	// err содержит результат создания корня неуправляемых пространств имён.
	if err := os.MkdirAll(base, 0700); err != nil {
		return nil, err
	}
	// canonical содержит корень после раскрытия символических ссылок; err сообщает об отказе
	// канонизации.
	canonical, err := filepath.EvalSymlinks(base)
	if err != nil {
		return nil, err
	}
	if _, err = verifyDirectory(canonical); err != nil {
		return nil, err
	}
	// n хранит создаваемое пространство имён, включая ресурсы для очистки при частичном отказе.
	n := &Namespace{sessionDir: filepath.Join(canonical, sessionID)}
	defer func() {
		// При ошибке создания закрывает частичный Namespace и сохраняет ошибку очистки отдельно от исходной
		// причины.

		if retErr != nil {
			// cleanupErr содержит ошибку очистки частично созданного неуправляемого
			// Namespace.
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
	// maxPath задаёт допустимую длину пути UDS в байтах для текущей ОС.
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

// Создаёт каталог и слушатели в пространстве Scope и регистрирует доказательства каждого ресурса;
// частичный отказ сохраняет отдельно ошибку очистки.
func newManagedNamespace(ctx context.Context, scope *processowner.Scope, fileInput bool) (_ *Namespace, retErr error) {
	// base содержит каталог временных ресурсов переданного Scope.
	base := scope.Namespace()
	// err сообщает об отказе проверки безопасности каталога Scope.
	if _, err := verifyDirectory(base); err != nil {
		return nil, err
	}
	// n хранит создаваемое управляемое пространство имён и уже созданные ресурсы.
	n := &Namespace{scope: scope}
	defer func() {
		// При ошибке создания управляемого Namespace пытается очистить уже созданные ресурсы и сохраняет оба
		// результата.

		if retErr != nil {
			// cleanup содержит ошибку очистки частично созданного управляемого Namespace.
			if cleanup := n.Close(); cleanup != nil {
				retErr = &namespaceCreationError{cause: retErr, cleanup: cleanup}
			}
		}
	}()
	// err переносит ошибки создания каталога, слушателей и регистрации временных ресурсов.
	var err error
	if n.dir, err = os.MkdirTemp(base, "w-"); err != nil {
		return nil, err
	}
	// register связывает созданный объект с относительным путём Scope и регистрирует доказательство
	// для последующей очистки.
	register := func(name, kind string) error {
		// Вычисляет относительную идентичность объекта области, сохраняет её для обратной очистки и
		// регистрирует объект в журнале временных ресурсов.

		// relative содержит путь созданного ресурса относительно каталога Scope; err сообщает об
		// отказе вычисления этого пути.
		relative, err := filepath.Rel(base, name)
		if err != nil {
			return err
		}
		// identity содержит идентичность временного ресурса для регистрации и последующего
		// удаления.
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
	// maxPath задаёт допустимую длину пути управляемого UDS в байтах для текущей ОС.
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

// Открывает Unix-слушатель без автоматического удаления пути, фиксирует его идентичность и права 0600;
// при ошибке закрывает и удаляет только известный объект.
func listenOwned(path string) (*net.UnixListener, os.FileInfo, error) {
	// l содержит созданный Unix-слушатель; err сообщает об отказе открытия, чтения идентичности или
	// назначения прав сокета.
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, nil, err
	}
	l.SetUnlinkOnClose(false)
	// info содержит исходную файловую идентичность сокета для последующей проверки удаления.
	info, err := os.Lstat(path)
	if err == nil {
		err = os.Chmod(path, 0600)
	}
	if err != nil {
		return nil, nil, errors.Join(err, l.Close(), removeOwned(path, info))
	}
	return l, info, nil
}

// Однократно резервирует приём направления, закрывает слушатель после соединения и сохраняет
// соединение под владением Namespace; Shutdown отвергает и закрывает поздний результат.
func (n *Namespace) accept(input bool) (net.Conn, error) {
	n.mu.Lock()
	if n.closed || n.shutdown {
		n.mu.Unlock()
		return nil, net.ErrClosed
	}
	// l содержит слушатель выбранного входного или выходного направления.
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
	// c содержит принятое соединение; err сообщает об отказе приёма или закрытия слушателя.
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

// Удаляет путь только при совпадении файловой идентичности и типа с исходным объектом; отсутствие
// допустимо, подмена возвращает ErrResourceOwnership.
func removeOwned(path string, original os.FileInfo) error {
	if path == "" || original == nil {
		return nil
	}
	// current содержит текущую идентичность удаляемого пути; err сообщает об отказе её чтения или
	// отсутствии объекта.
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(original, current) || original.Mode().Type() != current.Mode().Type() {
		return fmt.Errorf("%w: %s", ErrResourceOwnership, path)
	}
	return os.Remove(path)
}

// Shutdown закрывает дескрипторы и разблокирует Accept и операции ввода-вывода,
// сохраняя пути до завершения пользователей.
func (n *Namespace) Shutdown() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.shutdownLocked()
}

// Единожды закрывает принятые соединения и слушатели, сохраняя объединённую ошибку; вызывающий
// удерживает n.mu.
func (n *Namespace) shutdownLocked() error {
	if n.shutdown {
		return n.shutdownErr
	}
	n.shutdown = true
	// errs накапливает ошибки закрытия соединений и слушателей Namespace.
	var errs []error
	// c содержит очередное принятое соединение, закрываемое при Shutdown.
	for _, c := range n.connections {
		// err содержит отказ закрытия принятого соединения, кроме допустимого net.ErrClosed.
		if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	n.connections = nil
	// l содержит очередной входной или выходной слушатель, закрываемый при Shutdown.
	for _, l := range []*net.UnixListener{n.input, n.output} {
		if l != nil {
			// err содержит отказ закрытия слушателя, кроме допустимого net.ErrClosed.
			if err := l.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		}
	}
	n.shutdownErr = errors.Join(errs...)
	return n.shutdownErr
}

// Close вызывается после завершения пользователей и удаляет только собственные пути.
func (n *Namespace) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return n.closeErr
	}
	n.closed = true
	// errs накапливает ошибки закрытия дескрипторов и удаления путей Namespace.
	errs := []error{n.shutdownLocked()}
	if n.scope != nil {
		if n.keepPaths {
			n.closeErr = errors.Join(n.shutdownErr, ErrCleanupPending)
			return n.closeErr
		}
		// resource содержит очередную идентичность временного объекта в обратном порядке
		// создания.
		for _, resource := range slices.Backward(n.resources) {
			errs = append(errs, n.scope.RuntimeJournal().Remove(context.Background(), resource))
		}
		n.closeErr = errors.Join(errs...)
		return n.closeErr
	}
	n.closeErr = errors.Join(append(errs, n.closeUnmanaged())...)
	return n.closeErr
}

// Проверяет идентичность родителей до удаления вложенных путей без Scope и всегда освобождает
// удержание родителя, включая отказ проверки.
func (n *Namespace) closeUnmanaged() (retErr error) {
	// Удержание родителя возвращается и при отказе проверки вложенных ресурсов.
	defer func() {
		// Возвращает удержание родительского каталога при любом исходе удаления вложенных путей и присоединяет
		// ошибку освобождения.
		retErr = errors.Join(retErr, releaseNamespaceParent(n.parent))
	}()
	// Проверка родителей предшествует доступу к вложенным путям.
	// err сообщает о подмене или недоступности каталога Session перед удалением вложенных ресурсов.
	if err := sameDirectory(n.sessionDir, n.sessionInfo); err != nil {
		return err
	}
	if n.dirInfo != nil {
		// err сообщает о подмене или недоступности каталога попытки перед удалением её сокетов.
		if err := sameDirectory(n.dir, n.dirInfo); err != nil {
			return err
		}
	}
	return errors.Join(removeOwned(n.InputPath, n.inputInfo), removeOwned(n.OutputPath, n.outputInfo), removeOwned(n.dir, n.dirInfo))
}

// Сверяет текущий каталог с исходной файловой идентичностью, отвергая исчезновение или замену объекта.
func sameDirectory(path string, original os.FileInfo) error {
	if original == nil {
		return nil
	}
	// current содержит текущую идентичность проверяемого каталога; err сообщает об отказе чтения
	// метаданных.
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !current.IsDir() || !os.SameFile(original, current) {
		return fmt.Errorf("%w: %s", ErrResourceOwnership, path)
	}
	return nil
}

// namespaceCreationError сохраняет отдельный результат очистки частично созданных ресурсов.
type namespaceCreationError struct { /* cause сохраняет исходный отказ создания Namespace; cleanup сохраняет отдельный отказ освобождения частичных ресурсов. */
	cause, cleanup error
}

// Error возвращает объединённый текст причины создания и ошибки очистки частичных ресурсов.
func (e *namespaceCreationError) Error() string { return errors.Join(e.cause, e.cleanup).Error() }

// Unwrap раскрывает обе ошибки, сохраняя проверку исходной причины и неполной очистки.
func (e *namespaceCreationError) Unwrap() []error { return []error{e.cause, e.cleanup} }
