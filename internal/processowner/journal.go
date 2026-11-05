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

type scopeHeader struct {
	Version              int
	ID, Owner            string
	RuntimeRoot          string
	Sealed, Acknowledged bool
	Count                int
	Limits               Limits
	GracePeriod          time.Duration
}

type operation struct {
	Version                  int
	ScopeID, ID, Kind, State string
	Binary                   string
	Args                     []string
	ExtraFiles               int
	ExitCode                 int
	Error                    string
	File                     *FileIdentity `json:",omitempty"`
}

const (
	reserved = "RESERVED"
	spawned  = "SPAWN_MARKER"
	joined   = "JOINED_RESULT"
	closed   = "CLOSED"
)

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	b, e := hex.DecodeString(id)
	return e == nil && len(b) == 16 && id == strings.ToLower(id)
}
func scopePath(id string) string          { return "scopes/" + id }
func operationPath(sid, id string) string { return scopePath(sid) + "/operations/" + id }

func syncDir(root *os.Root, name string) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func writeRecord(root *os.Root, name string, value any, limit int64) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if int64(len(b)) > limit {
		return ErrLimit
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	tmp := name + ".tmp-" + id
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

func readRecord(root *os.Root, name string, value any, limit int64) error {
	info, err := root.Lstat(name)
	if err != nil {
		return errors.Join(ErrUnknown, err)
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return ErrUnknown
	}
	f, err := root.Open(name)
	if err != nil {
		return errors.Join(ErrUnknown, err)
	}
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

func createLock(root *os.Root, name string) error {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close(), syncDir(root, path.Dir(name)))
}

func lockExisting(ctx context.Context, root *os.Root, name string, wait bool) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, errors.Join(ErrUnknown, err)
	}
	if !info.Mode().IsRegular() {
		return nil, ErrUnknown
	}
	f, err := root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return nil, errors.Join(ErrUnknown, err)
	}
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
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errors.Join(ctx.Err(), f.Close())
		case <-timer.C:
		}
	}
}

func unlock(f *os.File) error {
	return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close())
}

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
	runtime, err := NewFileJournal(config.RuntimeRoot, config.Limits)
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return &Registry{config: config, root: root, runtime: runtime}, nil
}

// Close вызывается после закрытия всех принадлежащих backend областей.
func (r *Registry) Close() error { return errors.Join(r.root.Close(), r.runtime.Close()) }

// Create возвращает частичную область вместе с ошибкой; вызывающий сохраняет её до успешного CloseContext.
func (r *Registry) Create(ctx context.Context, owner string) (*Scope, error) {
	return r.create(ctx, owner, (*Scope).initializeRuntime)
}

func (r *Registry) create(ctx context.Context, owner string, initialize func(*Scope, context.Context) (*Ticket, error)) (*Scope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if owner == "" || len(owner) > 4096 || !utf8.ValidString(owner) {
		return nil, ErrInvalid
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	base := scopePath(id)
	if err = r.root.Mkdir(base, 0700); err != nil {
		return nil, err
	}
	for _, dir := range []string{"operations"} {
		if err = r.root.Mkdir(base+"/"+dir, 0700); err != nil {
			return nil, err
		}
	}
	for _, name := range []string{"gate.lock", "backend.lock"} {
		if err = createLock(r.root, base+"/"+name); err != nil {
			return nil, err
		}
	}
	backend, err := lockExisting(ctx, r.root, base+"/backend.lock", false)
	if err != nil {
		return nil, err
	}
	h := scopeHeader{Version: ProtocolVersion, ID: id, Owner: owner, RuntimeRoot: r.config.RuntimeRoot, Limits: r.config.Limits, GracePeriod: r.config.GracePeriod}
	if err = writeRecord(r.root, base+"/header.json", h, r.config.Limits.MaxRecordBytes); err != nil {
		return nil, errors.Join(err, unlock(backend))
	}
	if err = syncDir(r.root, "scopes"); err != nil {
		return nil, errors.Join(err, unlock(backend))
	}
	s := &Scope{registry: r, id: id, backend: backend, tickets: map[string]*Ticket{}, processes: map[string]*Process{}}
	ticket, err := initialize(s, ctx)
	if ticket != nil {
		err = errors.Join(err, ticket.Complete(ctx))
	}
	if err != nil {
		return s, errors.Join(err, s.CloseContext(ctx))
	}
	return s, nil
}

func (s *Scope) initializeRuntime(ctx context.Context) (*Ticket, error) {
	owner, err := s.Owner()
	if err != nil {
		return nil, err
	}
	r := s.registry
	identity := FileIdentity{FileID: s.id + ":.", GenerationID: s.id, ManagedPath: s.id, Kind: Directory, Owner: owner}
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

func (r *Registry) OpenScope(ctx context.Context, id string) (*Scope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(id) {
		return nil, ErrInvalid
	}
	s := &Scope{registry: r, id: id}
	if _, err := s.header(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Scope) header() (scopeHeader, error) {
	var h scopeHeader
	err := readRecord(s.registry.root, scopePath(s.id)+"/header.json", &h, s.registry.config.Limits.MaxRecordBytes)
	if err == nil && (h.Version != ProtocolVersion || h.ID != s.id || h.Owner == "" || h.Limits.Validate() != nil || h.Count < 0 || h.Count > h.Limits.MaxOperations) {
		err = ErrUnknown
	}
	if err == nil && s.registry.config.RuntimeRoot != "" && h.RuntimeRoot != s.registry.config.RuntimeRoot {
		err = ErrUnknown
	}
	return h, err
}

func (s *Scope) register(ctx context.Context, kind string, command *Command, file *FileIdentity) (*Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing || s.backend == nil {
		return nil, ErrClosed
	}
	gate, err := lockExisting(ctx, s.registry.root, scopePath(s.id)+"/gate.lock", true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unlock(gate) }()
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
	id := fmt.Sprintf("%08d", h.Count)
	base := operationPath(s.id, id)
	if err = createLock(s.registry.root, base+".lock"); err != nil {
		return nil, err
	}
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
	t := &Ticket{scope: s, id: id, kind: kind}
	s.tickets[id] = t
	return t, nil
}

func (s *Scope) Register(ctx context.Context, kind string) (*Ticket, error) {
	if kind == "native" {
		return nil, ErrInvalid
	}
	return s.register(ctx, kind, nil, nil)
}

func (t *Ticket) Complete(ctx context.Context) error {
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
	lock, err := lockExisting(ctx, s.registry.root, operationPath(s.id, t.id)+".lock", true)
	if err != nil {
		return err
	}
	defer func() { _ = unlock(lock) }()
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
	tickets := make([]*Ticket, 0, len(s.tickets))
	for _, ticket := range s.tickets {
		if ticket.completionRequested && ticket.kind != "native" {
			tickets = append(tickets, ticket)
		}
	}
	s.mu.Unlock()
	var first error
	for _, ticket := range tickets {
		if err := ctx.Err(); err != nil {
			return errors.Join(first, err)
		}
		if err := ticket.Complete(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Scope) readOperation(id string) (operation, error) {
	var op operation
	err := readRecord(s.registry.root, operationPath(s.id, id)+".json", &op, s.registry.config.Limits.MaxRecordBytes)
	if err == nil && (op.Version != ProtocolVersion || op.ScopeID != s.id || op.ID != id || op.Kind == "" || (op.State != reserved && op.State != spawned && op.State != joined && op.State != closed)) {
		err = ErrUnknown
	}
	return op, err
}

func (s *Scope) Seal(ctx context.Context) error {
	_, err := s.seal(ctx)
	return err
}

func (s *Scope) seal(ctx context.Context) (scopeHeader, error) {
	gate, err := lockExisting(ctx, s.registry.root, scopePath(s.id)+"/gate.lock", true)
	if err != nil {
		return scopeHeader{}, err
	}
	defer func() { _ = unlock(gate) }()
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

// Close отзывает lifeline, но не закрывает backend lock до завершения его операций.
func (s *Scope) Close() error { return s.CloseContext(context.Background()) }

// CloseContext сохраняет backend lock и completion handles при ошибке финальной записи.
func (s *Scope) CloseContext(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	for _, p := range s.processes {
		p.Cancel()
	}
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if err := s.Seal(ctx); err != nil {
		return err
	}
	if err := s.RetryCompleted(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backend == nil {
		s.closed = true
		return nil
	}
	for id := range s.tickets {
		op, err := s.readOperation(id)
		if err != nil {
			return err
		}
		if op.Kind != "native" {
			return ErrPending
		}
	}
	err := unlock(s.backend)
	if err == nil {
		s.backend = nil
		s.closed = true
	}
	return err
}

func (s *Scope) Resolve(ctx context.Context) (ScopeResult, error) {
	h, err := s.header()
	if err != nil {
		return ScopeResult{ID: s.id, Unknown: true}, err
	}
	return s.resolve(ctx, h)
}

func (s *Scope) resolve(ctx context.Context, h scopeHeader) (ScopeResult, error) {
	result := ScopeResult{ID: s.id, Owner: h.Owner, Sealed: h.Sealed, Acknowledged: h.Acknowledged, Operations: h.Count}
	backend, backendErr := lockExisting(ctx, s.registry.root, scopePath(s.id)+"/backend.lock", false)
	backendGone := backendErr == nil
	if backendGone {
		defer func() { _ = unlock(backend) }()
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
	resources := make([]FileIdentity, 0, h.Count)
	var err error
	for i := 0; i < h.Count; i++ {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		id := fmt.Sprintf("%08d", i)
		lock, lockErr := lockExisting(ctx, s.registry.root, operationPath(s.id, id)+".lock", false)
		if errors.Is(lockErr, ErrPending) {
			result.PendingOperations++
			continue
		}
		if lockErr != nil {
			result.Unknown = true
			continue
		}
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
			return strings.Count(resources[i].ManagedPath, "/") > strings.Count(resources[j].ManagedPath, "/")
		})
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

// AcknowledgeScope сохраняет каталог даже после SQL ack: запоздалый helper всегда видит закрытую область.
func (r *Registry) AcknowledgeScope(ctx context.Context, id string) error {
	s, err := r.OpenScope(ctx, id)
	if err != nil {
		return err
	}
	result, err := s.Resolve(ctx)
	if err != nil {
		return err
	}
	if !result.Joined {
		return ErrPending
	}
	gate, err := lockExisting(ctx, r.root, scopePath(id)+"/gate.lock", true)
	if err != nil {
		return err
	}
	defer func() { _ = unlock(gate) }()
	h, err := s.header()
	if err != nil {
		return err
	}
	if h.Acknowledged {
		return nil
	}
	for i := 0; i < h.Count; i++ {
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
func (r *Registry) RecoverPage(ctx context.Context, after string, limit int) ([]ScopeResult, string, error) {
	if limit <= 0 || limit > r.config.Limits.PageSize {
		return nil, "", ErrInvalid
	}
	offset := 0
	var err error
	if after != "" {
		offset, err = strconv.Atoi(after)
		if err != nil || offset < 0 {
			return nil, "", ErrInvalid
		}
	}
	dir, err := r.root.Open("scopes")
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = dir.Close() }()
	for n := 0; n < offset; {
		batch := min(offset-n, r.config.Limits.PageSize)
		names, e := dir.Readdirnames(batch)
		n += len(names)
		if e != nil {
			if errors.Is(e, io.EOF) {
				return nil, "", nil
			}
			return nil, "", e
		}
	}
	names, err := dir.Readdirnames(limit)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, "", err
	}
	out := make([]ScopeResult, 0, len(names))
	for _, id := range names {
		if err = ctx.Err(); err != nil {
			return out, "", err
		}
		if !validID(id) {
			out = append(out, ScopeResult{ID: id, Unknown: true})
			continue
		}
		s, e := r.OpenScope(ctx, id)
		if e != nil {
			out = append(out, ScopeResult{ID: id, Unknown: true})
			continue
		}
		result, e := s.Resolve(ctx)
		if e != nil && !errors.Is(e, ErrUnknown) {
			return out, "", e
		}
		out = append(out, result)
	}
	next := ""
	if len(names) == limit {
		next = strconv.Itoa(offset + len(names))
	}
	return out, next, nil
}
