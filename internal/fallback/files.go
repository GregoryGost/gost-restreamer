// Package fallback подготавливает и хранит проверенные файлы заглушек.
package fallback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

var (
	ErrLimit          = errors.New("превышен бюджет подготовки заглушки")
	ErrInvalid        = errors.New("некорректные настройки подготовки заглушки")
	ErrCleanupPending = errors.New("физическое завершение подготовки не подтверждено")
)

// FileStore владеет ограниченным деревом; Close вызывается после завершения пользователей.
type FileStore struct {
	root       *os.Root
	mu         sync.Mutex
	identities map[string]*rememberedFile
	evidence   *processowner.FileJournal
}

type rememberedFile struct {
	info os.FileInfo
}

func OpenFileStore(directory string) (*FileStore, error) {
	return OpenFileStoreWithLimits(directory, processowner.Limits{MaxOperations: 10000, MaxRecordBytes: 16384, MaxCommandArgs: 256, MaxCommandBytes: 4096, MaxExtraFiles: 1, PageSize: 100})
}

// OpenFileStoreWithLimits открывает независимый долговечный журнал на файловой системе данных.
func OpenFileStoreWithLimits(directory string, limits processowner.Limits) (*FileStore, error) {
	r, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	s := &FileStore{root: r, identities: map[string]*rememberedFile{}}
	if err = r.Mkdir("fallback", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, errors.Join(err, r.Close())
	}
	info, err := r.Lstat("fallback")
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.Join(core.ErrCorrupt, err, r.Close())
	}
	if err = s.syncParents("fallback/entry"); err != nil {
		return nil, errors.Join(err, r.Close())
	}
	s.evidence, err = processowner.NewFileJournal(directory, limits)
	if err != nil {
		return nil, errors.Join(err, r.Close())
	}
	return s, nil
}
func (s *FileStore) Close() error { return errors.Join(s.evidence.Close(), s.root.Close()) }

func (s *FileStore) CreateGeneration(id core.ID) error {
	name := core.GenerationDirectory(id)
	if core.ValidateManagedPath(name) != nil || path.Dir(name) != "fallback" {
		return ErrInvalid
	}
	if err := s.root.Mkdir(name, 0700); err != nil {
		return err
	}
	info, err := s.root.Lstat(name)
	if err != nil {
		return err
	}
	s.remember(name, info)
	return s.syncParents(name + "/entry")
}

func (s *FileStore) remember(name string, info os.FileInfo) {
	s.mu.Lock()
	s.identities[name] = &rememberedFile{info: info}
	s.mu.Unlock()
}

func (s *FileStore) remembered(name string) *rememberedFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identities[name]
}

func (s *FileStore) forget(name string, expected *rememberedFile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Новая запись того же пути принадлежит другому наблюдению и сохраняется.
	if s.identities[name] == expected {
		delete(s.identities, name)
	}
}

// RemoveOwned принимает проверенную запись реестра файлов и не удаляет дерево рекурсивно.
func (s *FileStore) RemoveOwned(file core.OwnedFile) error {
	return s.RemoveOwnedContext(context.Background(), file)
}
func (s *FileStore) RemoveOwnedContext(ctx context.Context, file core.OwnedFile) error {
	if err := file.Validate(); err != nil {
		return err
	}
	if file.ID != "" {
		exists, evidenceErr := s.evidence.Exists(ctx, fileIdentity(file))
		if !exists {
			if evidenceErr != nil {
				return evidenceErr
			}
			return errors.Join(ErrCleanupPending, processowner.ErrUnknown)
		}
		if file.ContentHash != "" {
			f, err := s.OpenExact(ctx, file.ManagedPath, file.SizeBytes, file.ContentHash)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if f != nil {
				if err = f.Close(); err != nil {
					return err
				}
			}
		}
		known := s.remembered(file.ManagedPath)
		if err := s.evidence.Remove(ctx, fileIdentity(file)); err != nil {
			return err
		}
		s.forget(file.ManagedPath, known)
		return nil
	}
	if file.ContentHash != "" {
		return s.RemoveExact(ctx, file.ManagedPath, file.SizeBytes, file.ContentHash)
	}
	info, err := s.root.Lstat(file.ManagedPath)
	if errors.Is(err, os.ErrNotExist) {
		return s.syncExistingParents(file.ManagedPath)
	}
	if err != nil {
		return err
	}
	if file.Kind == core.OwnedDirectory {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return core.ErrCorrupt
		}
	} else if !info.Mode().IsRegular() {
		return core.ErrCorrupt
	}
	s.mu.Lock()
	known := s.identities[file.ManagedPath]
	s.mu.Unlock()
	if known == nil || !os.SameFile(known.info, info) {
		return core.ErrCorrupt
	}
	if err = s.root.Remove(file.ManagedPath); err != nil {
		return err
	}
	if err = s.syncExistingParents(file.ManagedPath); err != nil {
		return err
	}
	s.forget(file.ManagedPath, known)
	return nil
}
func (s *FileStore) syncExistingParents(name string) error {
	for dir := path.Dir(name); ; dir = path.Dir(dir) {
		_, err := s.root.Lstat(dir)
		if err == nil {
			return s.syncParents(dir + "/entry")
		}
		if !errors.Is(err, os.ErrNotExist) || dir == "." {
			return err
		}
	}
}

func (s *FileStore) Create(name string) (*os.File, error) {
	if err := core.ValidateManagedPath(name); err != nil {
		return nil, err
	}
	f, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	s.remember(name, info)
	return f, nil
}

// OpenRegular открывает новый независимый дескриптор и отвергает ссылки/специальные файлы.
func (s *FileStore) OpenRegular(name string) (*os.File, error) {
	if err := core.ValidateManagedPath(name); err != nil {
		return nil, err
	}
	before, err := s.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, core.ErrCorrupt
	}
	f, err := s.root.Open(name)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, errors.Join(core.ErrCorrupt, err, f.Close())
	}
	return f, nil
}

// OpenVerified возвращает точный проверенный файл с offset=0, принадлежащий вызывающему.
func (s *FileStore) OpenVerified(ctx context.Context, manifest core.ArtifactManifest) (*os.File, error) {
	return s.OpenExact(ctx, manifest.ManagedPath, manifest.SizeBytes, manifest.ContentHash)
}
func (s *FileStore) OpenExact(ctx context.Context, name string, size uint64, digest string) (*os.File, error) {
	if size == 0 || size > math.MaxInt64 || len(digest) != sha256.Size*2 || digest != strings.ToLower(digest) {
		return nil, core.ErrCorrupt
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return nil, core.ErrCorrupt
	}
	f, err := s.OpenRegular(name)
	if err != nil {
		return nil, err
	}
	actualSize, actualHash, err := InspectFile(ctx, f, size)
	if err == nil && (actualSize != size || actualHash != digest) {
		err = core.ErrCorrupt
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	s.remember(name, info)
	return f, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// CopyBounded проверяет переполнение до записи лишнего байта.
func CopyBounded(ctx context.Context, dst io.Writer, src io.Reader, limit uint64) (uint64, error) {
	if limit == 0 || limit >= math.MaxInt64 {
		return 0, ErrInvalid
	}
	w := &boundedWriter{w: dst, remaining: limit}
	n, err := io.Copy(w, io.LimitReader(contextReader{ctx, src}, int64(limit)+1))
	return uint64(n), err
}

type boundedWriter struct {
	w         io.Writer
	remaining uint64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if uint64(len(p)) > w.remaining {
		return 0, ErrLimit
	}
	n, err := w.w.Write(p)
	w.remaining -= uint64(n)
	return n, err
}

// InspectFile проверяет размер и считает SHA256 прочитанного дескриптора.
func InspectFile(ctx context.Context, f *os.File, limit uint64) (uint64, string, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, "", err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return 0, "", core.ErrCorrupt
	}
	if uint64(info.Size()) > limit {
		return 0, "", ErrLimit
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return 0, "", err
	}
	h := sha256.New()
	n, err := CopyBounded(ctx, h, f, limit)
	if err != nil {
		return 0, "", err
	}
	after, err := f.Stat()
	if err != nil {
		return 0, "", err
	}
	if !os.SameFile(info, after) || after.Size() != info.Size() || uint64(after.Size()) != n || !after.ModTime().Equal(info.ModTime()) {
		return 0, "", core.ErrCorrupt
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// PublishRename требует уникальный конечный путь и синхронизирует изменённые каталоги.
func (s *FileStore) PublishRename(staging, final string) error {
	if core.ValidateManagedPath(staging) != nil || core.ValidateManagedPath(final) != nil || staging == final {
		return ErrInvalid
	}
	if _, err := s.root.Lstat(final); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(core.ErrConflict, err)
	}
	if err := s.root.Rename(staging, final); err != nil {
		return err
	}
	s.mu.Lock()
	if info := s.identities[staging]; info != nil {
		s.identities[final] = info
		delete(s.identities, staging)
	}
	s.mu.Unlock()
	return s.syncParents(staging, final)
}
func (s *FileStore) syncParents(names ...string) error {
	seen := map[string]bool{}
	for _, name := range names {
		for dir := path.Dir(name); ; dir = path.Dir(dir) {
			if !seen[dir] {
				seen[dir] = true
				f, err := s.root.Open(dir)
				if err != nil {
					return err
				}
				err = errors.Join(f.Sync(), f.Close())
				if err != nil {
					return fmt.Errorf("синхронизация каталога %s: %w", dir, err)
				}
			}
			if dir == "." {
				break
			}
		}
	}
	return nil
}

// RemoveExact удаляет только принадлежащий записи обычный файл с точным содержимым.
// Отсутствующий путь тоже проходит барьер долговечности перед подтверждением очистки.
func (s *FileStore) RemoveExact(ctx context.Context, name string, size uint64, digest string) error {
	f, err := s.OpenExact(ctx, name, size, digest)
	if errors.Is(err, os.ErrNotExist) {
		return s.syncExistingParents(name)
	}
	if err != nil {
		return err
	}
	known := s.remembered(name)
	info, err := f.Stat()
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	current, err := s.root.Lstat(name)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return errors.Join(core.ErrCorrupt, err)
	}
	if err = s.root.Remove(name); err != nil {
		return err
	}
	if err = s.syncExistingParents(name); err != nil {
		return err
	}
	s.forget(name, known)
	return nil
}
