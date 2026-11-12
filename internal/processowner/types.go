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

// ProtocolVersion задаёт версию записей журнала и протокола запуска gost-process-helper.
const ProtocolVersion = 1

var (
	// ErrInvalid обозначает недопустимые параметры области, команды либо файловой идентичности.
	ErrInvalid = errors.New("некорректные параметры владельца процессов")
	// ErrLimit обозначает превышение явного бюджета журнала, команды либо регистрации операций.
	ErrLimit = errors.New("превышен бюджет журнала владельца процессов")
	// ErrClosed запрещает новые регистрации или завершение операций через уже закрытого владельца.
	ErrClosed = errors.New("регистрация владельца процессов закрыта")
	// ErrPending обозначает ещё выполняемую операцию либо неподтверждённую очистку ресурсов.
	ErrPending = errors.New("физическое завершение ещё не подтверждено")
	// ErrUnknown обозначает недостаточное или повреждённое доказательство физического завершения
	// либо владения объектом.
	ErrUnknown = errors.New("доказательство физического завершения неизвестно")
)

// Limits задаёт конечные пределы операций, записей, команд и страниц сверки.
type Limits struct {
	// MaxOperations ограничивает число зарегистрированных операций одной области.
	MaxOperations int
	// MaxRecordBytes ограничивает размер одной JSON-записи журнала в байтах.
	MaxRecordBytes int64
	// MaxCommandArgs ограничивает число отдельных аргументов целевой команды.
	MaxCommandArgs int
	// MaxCommandBytes ограничивает суммарный размер исполняемого пути и аргументов команды в байтах.
	MaxCommandBytes int
	// MaxExtraFiles ограничивает число файловых дескрипторов, передаваемых целевому процессу.
	MaxExtraFiles int
	// PageSize задаёт максимальное число записей в странице восстановления и подтверждений.
	PageSize int
}

// Validate проверяет допустимые границы всех бюджетов журнала и их согласованность; при нарушении
// возвращает ErrInvalid.
func (l Limits) Validate() error {
	if l.MaxOperations <= 0 || l.MaxOperations > 100000 || l.MaxRecordBytes < 1024 || l.MaxRecordBytes > 16<<20 || l.MaxCommandArgs <= 0 || l.MaxCommandArgs > 4096 || l.MaxCommandBytes <= 0 || l.MaxCommandBytes > int(l.MaxRecordBytes)/2 || l.MaxExtraFiles < 0 || l.MaxExtraFiles > 128 || l.PageSize <= 0 || l.PageSize > 1000 {
		return ErrInvalid
	}
	return nil
}

// Config задаёт корни доказательств и временных объектов, путь helper и сроки управляемых процессов.
type Config struct {
	// Root задаёт абсолютный корень долговечного журнала областей и операций.
	Root string
	// RuntimeRoot задаёт абсолютный корень рабочих файловых объектов; пустое значение выбирает
	// runtime внутри Root.
	RuntimeRoot string
	// HelperPath задаёт абсолютный путь исполняемого gost-process-helper.
	HelperPath string
	// Limits ограничивает память и объём учёта операций и команд данного реестра.
	Limits Limits
	// GracePeriod задаёт ожидание мягкой остановки целевого процесса после Interrupt до Kill.
	GracePeriod time.Duration
}

// Command описывает целевой процесс с отдельными аргументами, дополнительными дескрипторами и потоками
// ввода и вывода.
type Command struct {
	// Binary задаёт имя либо путь исполняемого файла целевого процесса.
	Binary string
	// Args содержит отдельные аргументы целевого процесса без обработки оболочкой.
	Args []string
	// ExtraFiles содержит передаваемые через helper дескрипторы, нумеруемые у целевого процесса
	// начиная с FD 3.
	ExtraFiles []*os.File
	// Stdin предоставляет стандартный вход вспомогательного и целевого процессов.
	Stdin io.Reader
	// Stdout принимает стандартный вывод, а Stderr принимает стандартный поток ошибок
	// вспомогательного и целевого процессов.
	Stdout, Stderr io.Writer
}

// Result разделяет подтверждение завершения целевого процесса, его код и ошибки ожидания и учёта.
type Result struct {
	// Joined подтверждает завершение целевого процесса либо доказанный отказ до его запуска по
	// сохранённому состоянию операции.
	Joined bool
	// ExitCode содержит сохранённый код целевого процесса; -1 используется при отсутствии обычного
	// кода или отказе запуска.
	ExitCode int
	// Err объединяет ошибки helper, целевого процесса, чтения результата и завершения учёта ввода и
	// вывода.
	Err error
}

// ScopeResult содержит текущую проверку завершения операций и очистки временных ресурсов области.
type ScopeResult struct {
	// ID обозначает область; Owner содержит её точного сохранённого владельца.
	ID, Owner string
	// Sealed запрещает новые регистрации; Joined подтверждает завершение операций и требуемое
	// удаление ресурсов; Unknown отмечает недостоверное доказательство; Acknowledged отмечает
	// сохранённое подтверждение владельца.
	Sealed, Joined, Unknown, Acknowledged bool
	// Operations содержит размер долговечного каталога операций; PendingOperations считает операции
	// или ресурсы, чьё завершение пока ожидается.
	Operations, PendingOperations int
}

// Registry открывает долговечный журнал областей и журнал принадлежащих им временных файловых объектов.
type Registry struct {
	// config хранит проверенные пути, бюджеты и срок мягкой остановки реестра.
	config Config
	// root ограничивает файловые операции корнем долговечного журнала процессов.
	root *os.Root
	// runtime хранит доказательства владения объектами RuntimeRoot.
	runtime *FileJournal
}

// PageLimit возвращает настроенный максимальный размер страницы восстановления и сверки.
func (r *Registry) PageLimit() int { return r.config.Limits.PageSize }

// Scope учитывает операции точного выполнения и удерживает блокировку основного процесса до допустимого
// закрытия.
type Scope struct {
	// registry предоставляет корни журналов и ограничения данной области.
	registry *Registry
	// id содержит неизменяемый случайный идентификатор области.
	id string
	// backend удерживает файловую блокировку живого основного владельца регистрации; у OpenScope
	// остаётся nil.
	backend *os.File
	// mu защищает закрытие области и наборы живых Ticket и Process.
	mu sync.Mutex
	// closed отмечает завершённое закрытие локального объекта области.
	closed bool
	// closing запрещает новые регистрации с момента начала CloseContext.
	closing bool
	// tickets хранит незавершённые локальные операции, включая запросы повторной финальной записи.
	tickets map[string]*Ticket
	// processes хранит процессы, для которых ещё работает ожидание helper.
	processes map[string]*Process
}

// Ticket подтверждает фактический возврат одной зарегистрированной операции основного процесса.
type Ticket struct {
	// scope содержит область, в журнале которой зарегистрирована операция.
	scope *Scope
	// id содержит номер операции внутри области.
	id string
	// kind содержит вид операции и запрещает завершать native через Ticket.Complete.
	kind string
	// completionRequested сохраняет факт физического возврата операции для повтора неудачной
	// финальной записи.
	completionRequested bool
	// completed отмечает успешно подтверждённое завершение и делает повтор Complete безопасным.
	completed bool
}

// Process сохраняет канал отмены и единственного ожидания helper для одной native-операции области.
type Process struct {
	// scope содержит область зарегистрированной native-операции.
	scope *Scope
	// id содержит точный номер native-операции в журнале области.
	id string
	// lifeline содержит принадлежащий основному процессу дескриптор записи; его закрытие запрашивает
	// остановку через helper.
	lifeline *os.File
	// helper содержит запущенный вспомогательный процесс, ожидающий целевую команду.
	helper *os.Process
	// once исключает повторное закрытие lifeline при отмене из разных путей.
	once sync.Once
	// done закрывается после ожидания helper, чтения сохранённого результата и завершения учёта
	// ввода и вывода.
	done chan struct{}
	// result хранит окончательный результат, опубликованный перед закрытием done.
	result Result
}

// ID возвращает неизменяемый идентификатор этой области.
func (s *Scope) ID() string { return s.id }

// Namespace возвращает путь рабочего каталога области. Наличие пути в результате не подтверждает, что
// каталог ещё существует или его очистка завершена.
func (s *Scope) Namespace() string { return s.registry.config.RuntimeRoot + "/" + s.id }

// Done сообщает о завершении ожидания вспомогательного процесса и чтения его результата.
// Физическое завершение целевого процесса подтверждает Result.Joined.
func (p *Process) Done() <-chan struct{} { return p.done }

// Cancel однократно закрывает lifeline, запрашивая через вспомогательный процесс остановку целевого.
// Для подтверждения завершения требуется Wait и проверка Result.Joined.
func (p *Process) Cancel() {
	p.once.Do(func() {
		// Однократно закрывает запись lifeline, сообщая gost-process-helper об отмене принадлежащего процесса.
		_ = p.lifeline.Close()
	})
}

// Wait ограничивает ожидание вызывающего и сохраняет владельца Wait целевого процесса.
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

// Run запускает управляемую команду и возвращает её результат. При отмене ожидания запрашивает
// остановку и продолжает ждать вспомогательный процесс без срока ctx, поэтому возврат может произойти
// после отмены контекста.
func (s *Scope) Run(ctx context.Context, command Command) (Result, error) {
	// p содержит запущенную native-операцию; err сохраняет результат запуска и ограниченного
	// ожидания.
	p, err := s.Start(ctx, command)
	if err != nil {
		return Result{}, err
	}
	// result содержит исход процесса после ожидания с переданным контекстом.
	result, err := p.Wait(ctx)
	if err != nil {
		p.Cancel()
		// result содержит фактически дождённый исход после отмены; joinErr сообщает об отказе
		// окончательного ожидания.
		result, joinErr := p.Wait(context.Background())
		return result, errors.Join(err, joinErr, result.Err)
	}
	return result, result.Err
}

// FileIdentity связывает файловый объект с точными идентификаторами, относительным путём, видом и
// владельцем для доказуемой очистки.
type FileIdentity struct { /* FileID задаёт ключ доказательства; GenerationID задаёт поколение ресурса; ManagedPath задаёт путь относительно корня журнала; Kind задаёт тип объекта; Owner задаёт точного разрешающего владельца. */
	FileID, GenerationID, ManagedPath, Kind, Owner string
}

const (
	// RegularFile обозначает обычный файл с жёсткой служебной ссылкой на исходный объект.
	RegularFile = "regular"
	// Directory обозначает каталог с собственным маркером владельца и служебной ссылкой на этот
	// маркер.
	Directory = "directory"
	// Socket обозначает Unix-сокет, идентичность которого подтверждается служебной ссылкой на
	// файловый объект.
	Socket = "socket"
)
