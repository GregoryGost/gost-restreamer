package processowner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// scopeHeader сохраняет идентичность, бюджеты и состояние допуска области отдельно от записей её
// операций.
type scopeHeader struct {
	// Version задаёт версию протокола сохранённого заголовка.
	Version int
	// ID содержит точную идентичность области; Owner содержит назначенного ей владельца выполнения.
	ID, Owner string
	// RuntimeRoot фиксирует корень временных объектов, с которым должна открываться область.
	RuntimeRoot string
	// Sealed запрещает новые регистрации; Acknowledged отмечает подтверждение очистки долговечным
	// владельцем.
	Sealed, Acknowledged bool
	// Count содержит число зарезервированных номеров каталога операций, включая возможные частичные
	// записи.
	Count int
	// Limits сохраняет бюджеты области для основного и вспомогательного процессов.
	Limits Limits
	// GracePeriod сохраняет срок ожидания Interrupt перед Kill целевой команды.
	GracePeriod time.Duration
}

// operation сохраняет точную зарегистрированную работу, её стадию и при необходимости native-команду
// либо файловую идентичность.
type operation struct {
	// Version задаёт версию протокола записи операции.
	Version int
	// ScopeID связывает операцию с областью; ID задаёт её локальный номер; Kind различает вид
	// работы; State хранит стадию её физического выполнения.
	ScopeID, ID, Kind, State string
	// Binary содержит исполняемый путь зарезервированной native-команды.
	Binary string
	// Args сохраняет независимую копию отдельных аргументов native-команды.
	Args []string
	// ExtraFiles содержит число наследуемых целевой командой дополнительных дескрипторов.
	ExtraFiles int
	// ExitCode сохраняет код завершения целевого процесса либо -1 при отказе запуска.
	ExitCode int
	// Error сохраняет полный текст ошибки целевого Start или Wait.
	Error string
	// File содержит точную идентичность временного объекта для очистки после завершения операций
	// области.
	File *FileIdentity `json:",omitempty"`
}

const (
	// reserved обозначает зарегистрированную операцию без отметки запуска либо завершения.
	reserved = "RESERVED"
	// spawned обозначает сохранённую отметку перед Start, которая без итогового результата оставляет
	// завершение неизвестным.
	spawned = "SPAWN_MARKER"
	// joined обозначает сохранённый окончательный результат физически завершённой операции.
	joined = "JOINED_RESULT"
	// closed обозначает закрытую резервацию, для которой native-запуск не был разрешён.
	closed = "CLOSED"
)

// Создаёт идентификатор из 16 криптографически случайных байтов в шестнадцатеричной записи; ошибку
// источника случайности возвращает вызывающему коду.
func randomID() (string, error) {
	// b содержит шестнадцать случайных байтов нового идентификатора области или временной записи.
	var b [16]byte
	// err сообщает об отказе получения случайных байтов идентификатора.
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Проверяет канонический идентификатор из 32 строчных шестнадцатеричных символов.
func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	// b содержит декодированные байты идентификатора; e сообщает о недопустимой шестнадцатеричной
	// записи.
	b, e := hex.DecodeString(id)
	return e == nil && len(b) == 16 && id == strings.ToLower(id)
}

// Возвращает относительный путь каталога области; вызывающий код предварительно проверяет
// идентификатор.
func scopePath(id string) string { return "scopes/" + id }

// Возвращает относительный путь операции внутри области без расширения записи или блокировки.
func operationPath(sid, id string) string { return scopePath(sid) + "/operations/" + id }

// Открывает каталог относительно корня и объединяет ошибки его синхронизации и закрытия.
func syncDir(root *os.Root, name string) error {
	// f содержит дескриптор синхронизируемого каталога; err сообщает об отказе его открытия.
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// Сохраняет ограниченный JSON через отдельный временный файл, его Sync, Rename и Sync каталога. Ошибка
// последней синхронизации возможна после замены записи и не означает отсутствия изменения.
func writeRecord(root *os.Root, name string, value any, limit int64) error {
	// b содержит JSON-представление сохраняемой записи; err сохраняет результаты кодирования и
	// файловой записи.
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if int64(len(b)) > limit {
		return ErrLimit
	}
	// id содержит случайный суффикс отдельного временного файла записи.
	id, err := randomID()
	if err != nil {
		return err
	}
	// tmp задаёт имя временного файла в том же каталоге, где заменяется итоговая JSON-запись.
	tmp := name + ".tmp-" + id
	// f содержит исключительно созданный временный файл до Sync и Rename.
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err = root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return syncDir(root, path.Dir(name))
}

// Читает ограниченный JSON из обычного файла. Отсутствующий, недопустимый или повреждённый объект не
// превращается в пустую успешную запись.
func readRecord(root *os.Root, name string, value any, limit int64) error {
	// info содержит метаданные читаемой записи; err сохраняет результаты проверки обычного файла и
	// чтения JSON.
	info, err := root.Lstat(name)
	if err != nil {
		return errors.Join(ErrUnknown, err)
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return ErrUnknown
	}
	// f содержит открытый файл ограниченной JSON-записи.
	f, err := root.Open(name)
	if err != nil {
		return errors.Join(ErrUnknown, err)
	}
	// b содержит прочитанные байты с запасным байтом для обнаружения превышения лимита.
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if int64(len(b)) > limit {
		return ErrLimit
	}
	if err = json.Unmarshal(b, value); err != nil {
		return errors.Join(ErrUnknown, err)
	}
	return nil
}

// Создаёт новый файл блокировки без замены существующего и синхронизирует файл и родительский каталог.
// Ошибка синхронизации может оставлять уже созданный файл.
func createLock(root *os.Root, name string) error {
	// f содержит новый файл блокировки; err сообщает об отказе его исключительного создания.
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close(), syncDir(root, path.Dir(name)))
}

// Открывает существующий обычный файл и проверяет его тождество объекту, найденному до открытия, затем
// получает исключительную flock. При wait=false занятая блокировка даёт ErrPending; ожидание
// прерывается через ctx.
func lockExisting(ctx context.Context, root *os.Root, name string, wait bool) (*os.File, error) {
	// info сохраняет исходную идентичность файла блокировки; err сохраняет результаты проверки и
	// получения flock.
	info, err := root.Lstat(name)
	if err != nil {
		return nil, errors.Join(ErrUnknown, err)
	}
	if !info.Mode().IsRegular() {
		return nil, ErrUnknown
	}
	// f содержит открытый дескриптор блокировки, чья идентичность сверяется перед flock.
	f, err := root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return nil, errors.Join(ErrUnknown, err)
	}
	// after содержит метаданные открытого дескриптора для сравнения с исходным объектом.
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		return nil, errors.Join(ErrUnknown, err, f.Close())
	}
	for {
		if err = ctx.Err(); err != nil {
			return nil, errors.Join(err, f.Close())
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, errors.Join(err, f.Close())
		}
		if !wait {
			return nil, errors.Join(ErrPending, f.Close())
		}
		// timer задаёт короткую паузу перед повторной попыткой получения занятой flock.
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errors.Join(ctx.Err(), f.Close())
		case <-timer.C:
		}
	}
}

// Снимает flock и закрывает её дескриптор, возвращая обе возможные ошибки.
func unlock(f *os.File) error {
	return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close())
}

// New проверяет ограничения, абсолютные пути и исполняемую вспомогательную программу, затем открывает
// или создаёт корни журнала и рабочих объектов. Пустой RuntimeRoot размещается внутри Root; реестр
// закрывают после всех его областей.
func New(config Config) (*Registry, error) {
	if config.Limits.Validate() != nil || config.GracePeriod <= 0 || !filepath.IsAbs(config.Root) || !filepath.IsAbs(config.HelperPath) {
		return nil, ErrInvalid
	}
	if config.RuntimeRoot == "" {
		config.RuntimeRoot = filepath.Join(config.Root, "runtime")
	}
	if !filepath.IsAbs(config.RuntimeRoot) {
		return nil, ErrInvalid
	}
	// info содержит метаданные выбранного helper; err сохраняет результаты проверки и открытия
	// корней реестра.
	info, err := os.Stat(config.HelperPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return nil, ErrInvalid
	}
	if err = os.MkdirAll(config.Root, 0700); err != nil {
		return nil, err
	}
	// root содержит ограниченный дескриптор корня долговечного журнала.
	root, err := os.OpenRoot(config.Root)
	if err != nil {
		return nil, err
	}
	if err = root.Mkdir("scopes", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, errors.Join(err, root.Close())
	}
	if err = syncDir(root, "."); err != nil {
		return nil, errors.Join(err, root.Close())
	}
	if err = os.MkdirAll(config.RuntimeRoot, 0700); err != nil {
		return nil, errors.Join(err, root.Close())
	}
	// runtime содержит файловый журнал доказательств временных ресурсов RuntimeRoot.
	runtime, err := NewFileJournal(config.RuntimeRoot, config.Limits)
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return &Registry{config: config, root: root, runtime: runtime}, nil
}

// Close вызывается после закрытия всех областей текущего основного процесса.
func (r *Registry) Close() error { return errors.Join(r.root.Close(), r.runtime.Close()) }

// Create возвращает частичную область вместе с ошибкой; вызывающий сохраняет её до успешного CloseContext.
func (r *Registry) Create(ctx context.Context, owner string) (*Scope, error) {
	return r.create(ctx, owner, (*Scope).initializeRuntime)
}

// Создаёт долговечную область и удерживает блокировку основного процесса, пока инициализатор
// регистрирует рабочий корень. Ошибка инициализации возвращает частичную область вместе с результатом
// попытки её закрытия.
func (r *Registry) create(ctx context.Context, owner string, initialize func(*Scope, context.Context) (*Ticket, error)) (*Scope, error) {
	// err содержит причину отмены контекста до создания области.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if owner == "" || len(owner) > 4096 || !utf8.ValidString(owner) {
		return nil, ErrInvalid
	}
	// id содержит новый случайный идентификатор области; err сохраняет результаты создания её
	// каталогов и записей.
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	// base задаёт относительный путь каталога создаваемой области.
	base := scopePath(id)
	if err = r.root.Mkdir(base, 0700); err != nil {
		return nil, err
	}
	// dir содержит имя обязательного подкаталога операций новой области.
	for _, dir := range []string{"operations"} {
		if err = r.root.Mkdir(base+"/"+dir, 0700); err != nil {
			return nil, err
		}
	}
	// name содержит очередное имя создаваемой блокировки gate либо backend.
	for _, name := range []string{"gate.lock", "backend.lock"} {
		if err = createLock(r.root, base+"/"+name); err != nil {
			return nil, err
		}
	}
	// backend удерживает блокировку основного владельца создаваемой области.
	backend, err := lockExisting(ctx, r.root, base+"/backend.lock", false)
	if err != nil {
		return nil, err
	}
	// h содержит начальный заголовок с точным владельцем, корнем ресурсов и бюджетами.
	h := scopeHeader{Version: ProtocolVersion, ID: id, Owner: owner, RuntimeRoot: r.config.RuntimeRoot, Limits: r.config.Limits, GracePeriod: r.config.GracePeriod}
	if err = writeRecord(r.root, base+"/header.json", h, r.config.Limits.MaxRecordBytes); err != nil {
		return nil, errors.Join(err, unlock(backend))
	}
	if err = syncDir(r.root, "scopes"); err != nil {
		return nil, errors.Join(err, unlock(backend))
	}
	// s содержит частично либо полностью созданную область, которую требуется сохранить до успешного
	// закрытия.
	s := &Scope{registry: r, id: id, backend: backend, tickets: map[string]*Ticket{}, processes: map[string]*Process{}}
	// ticket учитывает операции инициализатора временного корня до их фактического завершения.
	ticket, err := initialize(s, ctx)
	if ticket != nil {
		err = errors.Join(err, ticket.Complete(ctx))
	}
	if err != nil {
		return s, errors.Join(err, s.CloseContext(ctx))
	}
	return s, nil
}

// Регистрирует операцию создания корня области, резервирует его файловую идентичность, создаёт каталог
// и закрепляет доказательство. Возвращённую операцию завершает вызывающий код, в том числе после
// частичного отказа.
func (s *Scope) initializeRuntime(ctx context.Context) (*Ticket, error) {
	// owner содержит сохранённого владельца области; err сохраняет результаты чтения, регистрации и
	// закрепления runtime-корня.
	owner, err := s.Owner()
	if err != nil {
		return nil, err
	}
	// r предоставляет файловый журнал и корень временных объектов данной области.
	r := s.registry
	// identity связывает каталог runtime-корня с точным идентификатором и владельцем области.
	identity := FileIdentity{FileID: s.id + ":.", GenerationID: s.id, ManagedPath: s.id, Kind: Directory, Owner: owner}
	// ticket учитывает внешние действия создания и закрепления доказательства корня области.
	ticket, err := s.register(ctx, "runtime-resource", nil, &identity)
	if err != nil {
		return nil, err
	}
	if err = r.runtime.Reserve(ctx, identity); err == nil {
		err = r.runtime.root.Mkdir(s.id, 0700)
	}
	if err == nil {
		err = r.runtime.Register(ctx, identity)
	}
	return ticket, err
}

// OpenScope открывает сохранённую область для проверки и восстановления без приобретения владения
// основного процесса. Проверяет ctx, идентификатор и заголовок; новые операции через такой объект не
// регистрируются.
func (r *Registry) OpenScope(ctx context.Context, id string) (*Scope, error) {
	// err содержит причину отмены контекста до открытия сохранённой области.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(id) {
		return nil, ErrInvalid
	}
	// s представляет сохранённую область для восстановления без backend-владения регистрацией.
	s := &Scope{registry: r, id: id}
	// err сообщает об отказе проверки заголовка открываемой области.
	if _, err := s.header(); err != nil {
		return nil, err
	}
	return s, nil
}

// Читает заголовок и сверяет версию, идентификатор, владельца, ограничения, размер каталога и
// настроенный рабочий корень. Недостоверные сведения возвращает с ErrUnknown.
func (s *Scope) header() (scopeHeader, error) {
	// h принимает сохранённые поля заголовка для проверки принадлежности и бюджетов.
	var h scopeHeader
	// err содержит результат чтения и проверки заголовка области.
	err := readRecord(s.registry.root, scopePath(s.id)+"/header.json", &h, s.registry.config.Limits.MaxRecordBytes)
	if err == nil && (h.Version != ProtocolVersion || h.ID != s.id || h.Owner == "" || h.Limits.Validate() != nil || h.Count < 0 || h.Count > h.Limits.MaxOperations) {
		err = ErrUnknown
	}
	if err == nil && s.registry.config.RuntimeRoot != "" && h.RuntimeRoot != s.registry.config.RuntimeRoot {
		err = ErrUnknown
	}
	return h, err
}

// Под блокировками области и регистрации добавляет ограниченную операцию RESERVED. Сначала увеличивает
// долговечный каталог, чтобы частичная запись после аварии оставалась обнаружимой; закрытая область
// отклоняет регистрацию.
func (s *Scope) register(ctx context.Context, kind string, command *Command, file *FileIdentity) (*Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing || s.backend == nil {
		return nil, ErrClosed
	}
	// gate удерживает общий допуск регистрации; err сохраняет результаты блокировки и долговечного
	// добавления операции.
	gate, err := lockExisting(ctx, s.registry.root, scopePath(s.id)+"/gate.lock", true)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Освобождает блокировку допуска области после резервирования операции либо отказа регистрации.
		_ = unlock(gate)
	}()
	// h содержит текущий заголовок, чей Count увеличивается до записи новой операции.
	h, err := s.header()
	if err != nil {
		return nil, err
	}
	if h.Sealed {
		return nil, ErrClosed
	}
	if h.Count >= h.Limits.MaxOperations {
		return nil, ErrLimit
	}
	if kind == "" || len(kind) > 128 {
		return nil, ErrInvalid
	}
	// id задаёт восьмизначный номер новой операции по текущему размеру каталога.
	id := fmt.Sprintf("%08d", h.Count)
	// base задаёт относительное основание JSON-записи и блокировки новой операции.
	base := operationPath(s.id, id)
	if err = createLock(s.registry.root, base+".lock"); err != nil {
		return nil, err
	}
	// op содержит начальную запись RESERVED с командой либо файловой идентичностью при их наличии.
	op := operation{Version: ProtocolVersion, ScopeID: s.id, ID: id, Kind: kind, State: reserved}
	op.File = file
	if command != nil {
		op.Binary = command.Binary
		op.Args = append([]string(nil), command.Args...)
		op.ExtraFiles = len(command.ExtraFiles)
	}
	// Каталог фиксируется первым: авария между записями даёт UNKNOWN, а не пропущенную операцию.
	h.Count++
	if err = writeRecord(s.registry.root, scopePath(s.id)+"/header.json", h, h.Limits.MaxRecordBytes); err != nil {
		return nil, err
	}
	if err = writeRecord(s.registry.root, base+".json", op, h.Limits.MaxRecordBytes); err != nil {
		return nil, err
	}
	// t содержит локальный Ticket, сохраняемый до физического возврата и финальной записи.
	t := &Ticket{scope: s, id: id, kind: kind}
	s.tickets[id] = t
	return t, nil
}

// Register учитывает ещё не завершённую операцию основного процесса до её побочных эффектов.
// Вызывающий код вызывает Complete после фактического возврата операции; целевые процессы
// регистрируются только через Start.
func (s *Scope) Register(ctx context.Context, kind string) (*Ticket, error) {
	if kind == "native" {
		return nil, ErrInvalid
	}
	return s.register(ctx, kind, nil, nil)
}

// Complete записывает завершение уже вернувшейся операции основного процесса. При ошибке сохраняет
// запрос для RetryCompleted; повтор успешного завершения безопасен, а операции native завершаются
// только владельцем процесса.
func (t *Ticket) Complete(ctx context.Context) error {
	// s содержит область завершённой операции, чьи локальные состояние и журнал согласуются.
	s := t.scope
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.kind == "native" {
		return ErrInvalid
	}
	if t.completed {
		return nil
	}
	t.completionRequested = true
	if s.closed || s.backend == nil {
		return ErrClosed
	}
	// lock удерживает блокировку завершаемой операции; err сохраняет результаты её чтения и
	// финальной записи.
	lock, err := lockExisting(ctx, s.registry.root, operationPath(s.id, t.id)+".lock", true)
	if err != nil {
		return err
	}
	defer func() {
		// Освобождает блокировку операции после сохранения её физического завершения либо ошибки записи.
		_ = unlock(lock)
	}()
	// op содержит сохранённую операцию, переводимую в JOINED_RESULT после физического возврата.
	op, err := s.readOperation(t.id)
	if err != nil {
		return err
	}
	if op.Kind == "native" || op.State == closed {
		return ErrClosed
	}
	if op.State == joined {
		t.completed = true
		delete(s.tickets, t.id)
		return nil
	}
	op.State = joined
	if err = writeRecord(s.registry.root, operationPath(s.id, t.id)+".json", op, s.registry.config.Limits.MaxRecordBytes); err != nil {
		return err
	}
	delete(s.tickets, t.id)
	t.completed = true
	return nil
}

// RetryCompleted повторяет только запись завершения уже физически вернувшихся операций.
func (s *Scope) RetryCompleted(ctx context.Context) error {
	s.mu.Lock()
	// tickets собирает только операции с ранее запрошенным завершением для повтора финальной записи
	// вне блокировки области.
	tickets := make([]*Ticket, 0, len(s.tickets))
	// ticket содержит очередную локальную операцию при отборе допустимых повторов завершения.
	for _, ticket := range s.tickets {
		if ticket.completionRequested && ticket.kind != "native" {
			tickets = append(tickets, ticket)
		}
	}
	s.mu.Unlock()
	// first сохраняет первую ошибку повтора финальной записи.
	var first error
	// ticket содержит очередную уже вернувшуюся операцию для повторного Complete.
	for _, ticket := range tickets {
		// err содержит причину отмены контекста до очередного повтора финальной записи.
		if err := ctx.Err(); err != nil {
			return errors.Join(first, err)
		}
		// err содержит результат повторного Complete физически завершившейся операции.
		if err := ticket.Complete(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Читает запись операции и проверяет версию, принадлежность области, номер, вид и допустимое
// состояние; несовпадение даёт ErrUnknown.
func (s *Scope) readOperation(id string) (operation, error) {
	// op принимает сохранённую запись операции для проверки версии, принадлежности и стадии.
	var op operation
	// err содержит результат чтения и проверки точной записи операции.
	err := readRecord(s.registry.root, operationPath(s.id, id)+".json", &op, s.registry.config.Limits.MaxRecordBytes)
	if err == nil && (op.Version != ProtocolVersion || op.ScopeID != s.id || op.ID != id || op.Kind == "" || (op.State != reserved && op.State != spawned && op.State != joined && op.State != closed)) {
		err = ErrUnknown
	}
	return op, err
}

// Seal долговечно запрещает новые регистрации области. Существующие операции продолжают учитываться и
// требуют отдельного подтверждения завершения.
func (s *Scope) Seal(ctx context.Context) error {
	// err содержит результат долговечного запрета новых регистраций области.
	_, err := s.seal(ctx)
	return err
}

// Под блокировкой регистрации сохраняет Sealed и возвращает текущий заголовок. Повторное запечатывание
// не создаёт новый каталог операций.
func (s *Scope) seal(ctx context.Context) (scopeHeader, error) {
	// gate удерживает общий допуск при сохранении Sealed; err сохраняет результаты блокировки и
	// чтения заголовка.
	gate, err := lockExisting(ctx, s.registry.root, scopePath(s.id)+"/gate.lock", true)
	if err != nil {
		return scopeHeader{}, err
	}
	defer func() {
		// Освобождает блокировку допуска области после сохранения запрета новых регистраций.
		_ = unlock(gate)
	}()
	// h содержит текущий заголовок, которому устанавливается долговечный запрет регистрации.
	h, err := s.header()
	if err != nil {
		return scopeHeader{}, err
	}
	if h.Sealed {
		return h, nil
	}
	h.Sealed = true
	return h, writeRecord(s.registry.root, scopePath(s.id)+"/header.json", h, h.Limits.MaxRecordBytes)
}

// Close отзывает lifeline и сохраняет блокировку основного процесса до завершения его операций.
func (s *Scope) Close() error { return s.CloseContext(context.Background()) }

// CloseContext сохраняет блокировку основного процесса и объекты Ticket при ошибке финальной записи.
func (s *Scope) CloseContext(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	// p содержит очередной живой Process для запроса отмены через lifeline.
	for _, p := range s.processes {
		p.Cancel()
	}
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	// err сообщает об отказе сохранения Seal перед освобождением backend-владения.
	if err := s.Seal(ctx); err != nil {
		return err
	}
	// err сообщает об отказе повтора запрошенных финальных записей перед закрытием.
	if err := s.RetryCompleted(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backend == nil {
		s.closed = true
		return nil
	}
	// id содержит номер ещё учитываемого Ticket для проверки отсутствия незавершённой
	// backend-работы.
	for id := range s.tickets {
		// op содержит состояние учитываемой операции; err сообщает об отказе его чтения.
		op, err := s.readOperation(id)
		if err != nil {
			return err
		}
		if op.Kind != "native" {
			return ErrPending
		}
	}
	// err содержит результат освобождения backend-блокировки, после которого можно отметить область
	// закрытой.
	err := unlock(s.backend)
	if err == nil {
		s.backend = nil
		s.closed = true
	}
	return err
}

// Resolve сверяет сохранённые операции и может изменять журнал и файловую систему.
// При свободной блокировке основного процесса запрещает новые регистрации и закрывает
// зарезервированные операции. После подтверждения завершения удаляет временные ресурсы.
func (s *Scope) Resolve(ctx context.Context) (ScopeResult, error) {
	// h содержит проверенный заголовок сверяемой области; err сообщает об отказе его чтения.
	h, err := s.header()
	if err != nil {
		return ScopeResult{ID: s.id, Unknown: true}, err
	}
	return s.resolve(ctx, h)
}

// Сверяет операции области и, если основной процесс больше не удерживает блокировку, перечитывает
// каталог после Seal и закрывает незапущенные резервации. Удаляет принадлежащие области рабочие
// ресурсы только после подтверждения завершения всех операций.
func (s *Scope) resolve(ctx context.Context, h scopeHeader) (ScopeResult, error) {
	// result собирает текущее доказательство закрытого допуска, завершения операций и очистки
	// ресурсов.
	result := ScopeResult{ID: s.id, Owner: h.Owner, Sealed: h.Sealed, Acknowledged: h.Acknowledged, Operations: h.Count}
	// backend содержит временно полученную блокировку основного владельца; backendErr показывает,
	// свободна ли она или ещё удерживается.
	backend, backendErr := lockExisting(ctx, s.registry.root, scopePath(s.id)+"/backend.lock", false)
	// backendGone показывает возможность запретить новые регистрации и перечитать полный каталог при
	// свободной backend-блокировке.
	backendGone := backendErr == nil
	if backendGone {
		defer func() {
			// Освобождает временно занятую блокировку основного процесса после полного прохода Resolve.
			_ = unlock(backend)
		}()
		// sealed содержит заголовок после долговечного Seal; err сообщает об отказе этой
		// операции.
		sealed, err := s.seal(ctx)
		if err != nil {
			return result, err
		}
		h = sealed
		result = ScopeResult{ID: s.id, Owner: h.Owner, Sealed: h.Sealed, Acknowledged: h.Acknowledged, Operations: h.Count}
	}
	if backendErr != nil && !errors.Is(backendErr, ErrPending) {
		return result, backendErr
	}
	// resources собирает точные файловые идентичности из прочитанных операций для удаления после
	// доказанного завершения.
	resources := make([]FileIdentity, 0, h.Count)
	// err сохраняет ошибки отмены и удаления временных ресурсов во время сверки.
	var err error
	// i задаёт очередной номер записи полного долговечного каталога операций.
	for i := 0; i < h.Count; i++ {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		// id содержит восьмизначное имя сверяемой операции.
		id := fmt.Sprintf("%08d", i)
		// lock удерживает доступную блокировку операции; lockErr различает выполняемую работу и
		// недостоверную блокировку.
		lock, lockErr := lockExisting(ctx, s.registry.root, operationPath(s.id, id)+".lock", false)
		if errors.Is(lockErr, ErrPending) {
			result.PendingOperations++
			continue
		}
		if lockErr != nil {
			result.Unknown = true
			continue
		}
		// op содержит прочитанную операцию; opErr сохраняет результат чтения и возможного
		// закрытия незапущенной резервации.
		op, opErr := s.readOperation(id)
		if opErr == nil && op.File != nil {
			resources = append(resources, *op.File)
		}
		if opErr != nil {
			result.Unknown = true
		} else {
			switch op.State {
			case reserved:
				if backendGone {
					op.State = closed
					opErr = writeRecord(s.registry.root, operationPath(s.id, id)+".json", op, h.Limits.MaxRecordBytes)
				} else {
					result.PendingOperations++
				}
			case spawned:
				result.Unknown = true
			}
		}
		// unlockErr содержит результат освобождения блокировки сверенной операции.
		if unlockErr := unlock(lock); unlockErr != nil {
			return result, unlockErr
		}
		if opErr != nil {
			result.Unknown = true
		}
	}
	result.Joined = result.Sealed && result.PendingOperations == 0 && !result.Unknown
	if result.Joined && !h.Acknowledged {
		sort.Slice(resources, func(i, j int) bool {
			// Ставит более глубокие пути раньше родительских, чтобы удаление содержимого предшествовало удалению
			// принадлежащих области каталогов.

			return strings.Count(resources[i].ManagedPath, "/") > strings.Count(resources[j].ManagedPath, "/")
		})
		// identity содержит очередной временный ресурс в порядке от вложенных путей к
		// родительским.
		for _, identity := range resources {
			if identity.GenerationID != s.id || identity.Owner != h.Owner || (identity.ManagedPath != s.id && !strings.HasPrefix(identity.ManagedPath, s.id+"/")) {
				result.Unknown = true
				result.Joined = false
				break
			}
			if err = s.registry.runtime.Remove(ctx, identity); err != nil {
				result.Joined = false
				if errors.Is(err, ErrPending) {
					result.PendingOperations++
				} else {
					result.Unknown = true
				}
				break
			}
		}
	}
	return result, nil
}

// AcknowledgeScope сохраняет каталог даже после подтверждения в SQL: запоздалый gost-process-helper видит закрытую область.
func (r *Registry) AcknowledgeScope(ctx context.Context, id string) error {
	// s содержит открытую для подтверждения область; err сохраняет результаты её проверки и записи
	// подтверждения.
	s, err := r.OpenScope(ctx, id)
	if err != nil {
		return err
	}
	// result содержит доказательство присоединения области до разрешения AcknowledgeScope.
	result, err := s.Resolve(ctx)
	if err != nil {
		return err
	}
	if !result.Joined {
		return ErrPending
	}
	// gate удерживает общий допуск области во время подтверждения очистки.
	gate, err := lockExisting(ctx, r.root, scopePath(id)+"/gate.lock", true)
	if err != nil {
		return err
	}
	defer func() {
		// Освобождает блокировку допуска области после подтверждения её очистки либо ошибки финальной записи.
		_ = unlock(gate)
	}()
	// h содержит заголовок с текущим Acknowledged и полным числом операций.
	h, err := s.header()
	if err != nil {
		return err
	}
	if h.Acknowledged {
		return nil
	}
	// i задаёт номер операции, чьё файловое доказательство подтверждается.
	for i := 0; i < h.Count; i++ {
		// op содержит очередную операцию; e сохраняет результат её чтения и освобождения
		// файлового anchor.
		op, e := s.readOperation(fmt.Sprintf("%08d", i))
		if e != nil {
			return e
		}
		if op.File != nil {
			if e = r.runtime.Acknowledge(ctx, *op.File); e != nil {
				return e
			}
		}
	}
	h.Acknowledged = true
	return writeRecord(r.root, scopePath(id)+"/header.json", h, h.Limits.MaxRecordBytes)
}

// RecoverPage использует ограниченный курсор каталога; следующий полный проход начинает с пустого курсора.
// Каждая область проходит Resolve с его изменениями журнала и очисткой временных ресурсов.
func (r *Registry) RecoverPage(ctx context.Context, after string, limit int) ([]ScopeResult, string, error) {
	if limit <= 0 || limit > r.config.Limits.PageSize {
		return nil, "", ErrInvalid
	}
	// offset задаёт число каталогов областей, пропускаемых по числовому курсору.
	offset := 0
	// err сохраняет ошибки курсора и чтения страницы каталогов областей.
	var err error
	if after != "" {
		offset, err = strconv.Atoi(after)
		if err != nil || offset < 0 {
			return nil, "", ErrInvalid
		}
	}
	// dir содержит открытый каталог областей для ограниченного перечисления.
	dir, err := r.root.Open("scopes")
	if err != nil {
		return nil, "", err
	}
	defer func() {
		// Закрывает дескриптор каталога областей после чтения страницы восстановления.
		_ = dir.Close()
	}()
	// n считает число уже пропущенных имён до запрошенного смещения.
	for n := 0; n < offset; {
		// batch ограничивает размер очередного чтения при пропуске до курсора.
		batch := min(offset-n, r.config.Limits.PageSize)
		// names содержит очередную порцию пропускаемых имён; e сообщает о конце либо отказе
		// чтения.
		names, e := dir.Readdirnames(batch)
		n += len(names)
		if e != nil {
			if errors.Is(e, io.EOF) {
				return nil, "", nil
			}
			return nil, "", e
		}
	}
	// names содержит имена областей текущей ограниченной страницы.
	names, err := dir.Readdirnames(limit)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, "", err
	}
	// out собирает результаты сверки и неизвестные области текущей страницы.
	out := make([]ScopeResult, 0, len(names))
	// id содержит очередное имя каталога для проверки формата идентификатора области.
	for _, id := range names {
		if err = ctx.Err(); err != nil {
			return out, "", err
		}
		if !validID(id) {
			out = append(out, ScopeResult{ID: id, Unknown: true})
			continue
		}
		// s содержит открытую область; e сообщает об отказе открытия либо последующего Resolve.
		s, e := r.OpenScope(ctx, id)
		if e != nil {
			out = append(out, ScopeResult{ID: id, Unknown: true})
			continue
		}
		// result содержит результат сверки операций и очистки временных ресурсов области.
		result, e := s.Resolve(ctx)
		if e != nil && !errors.Is(e, ErrUnknown) {
			return out, "", e
		}
		out = append(out, result)
	}
	// next содержит следующий числовой курсор либо пустую строку при неполной странице.
	next := ""
	if len(names) == limit {
		next = strconv.Itoa(offset + len(names))
	}
	return out, next, nil
}
