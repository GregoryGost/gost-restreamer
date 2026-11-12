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
	// ErrLimit обозначает превышение конечного бюджета файлов или операций подготовки.
	ErrLimit = errors.New("превышен бюджет подготовки заглушки")
	// ErrInvalid обозначает некорректные настройки либо параметры подготовки.
	ErrInvalid = errors.New("некорректные настройки подготовки заглушки")
	// ErrCleanupPending обозначает отсутствие подтверждения физического завершения подготовки.
	ErrCleanupPending = errors.New("физическое завершение подготовки не подтверждено")
)

// FileStore владеет ограниченным деревом; Close вызывается после завершения пользователей.
type FileStore struct {
	// root ограничивает файловые операции открытым деревом данных.
	root *os.Root
	// mu защищает локальные наблюдения идентичностей файлов.
	mu sync.Mutex
	// identities связывает управляемый путь с последним наблюдением файлового объекта.
	identities map[string]*rememberedFile
	// evidence хранит долговечные доказательства владения файлами для точной очистки.
	evidence *processowner.FileJournal
}

// rememberedFile сохраняет локальное наблюдение файловой идентичности для защиты от подмены объекта по
// пути.
type rememberedFile struct {
	// info содержит сведения, с которыми сравнивается текущий объект через os.SameFile.
	info os.FileInfo
}

// OpenFileStore открывает файловое дерево и журнал с заданными здесь конечными пределами операций и
// записей.
func OpenFileStore(directory string) (*FileStore, error) {
	return OpenFileStoreWithLimits(directory, processowner.Limits{MaxOperations: 10000, MaxRecordBytes: 16384, MaxCommandArgs: 256, MaxCommandBytes: 4096, MaxExtraFiles: 1, PageSize: 100})
}

// OpenFileStoreWithLimits открывает независимый долговечный журнал на файловой системе данных.
func OpenFileStoreWithLimits(directory string, limits processowner.Limits) (*FileStore, error) {
	// r содержит открытое корневое дерево данных. err сообщает об отказе его открытия.
	r, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	// s содержит создаваемое файловое хранилище с пустым набором локальных наблюдений.
	s := &FileStore{root: r, identities: map[string]*rememberedFile{}}
	if err = r.Mkdir("fallback", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, errors.Join(err, r.Close())
	}
	// info содержит сведения о каталоге fallback для проверки его вида и отсутствия символьной
	// ссылки.
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

// Close закрывает журнал доказательств и корневой дескриптор, объединяя ошибки обоих закрытий.
func (s *FileStore) Close() error { return errors.Join(s.evidence.Close(), s.root.Close()) }

// CreateGeneration создаёт новый каталог поколения, запоминает его файловую идентичность и
// синхронизирует родительские каталоги.
func (s *FileStore) CreateGeneration(id core.ID) error {
	// name содержит канонический управляемый путь каталога поколения.
	name := core.GenerationDirectory(id)
	if core.ValidateManagedPath(name) != nil || path.Dir(name) != "fallback" {
		return ErrInvalid
	}
	// err сообщает об отказе создания нового каталога поколения.
	if err := s.root.Mkdir(name, 0700); err != nil {
		return err
	}
	// info содержит наблюдение созданного каталога. err сообщает об отказе чтения его свойств.
	info, err := s.root.Lstat(name)
	if err != nil {
		return err
	}
	s.remember(name, info)
	return s.syncParents(name + "/entry")
}

// Сохраняет новое наблюдение файлового объекта под блокировкой, заменяя прежнее наблюдение пути.
func (s *FileStore) remember(name string, info os.FileInfo) {
	s.mu.Lock()
	s.identities[name] = &rememberedFile{info: info}
	s.mu.Unlock()
}

// Возвращает текущее наблюдение пути под блокировкой; отсутствие означает, что объект этим экземпляром
// не запомнен.
func (s *FileStore) remembered(name string) *rememberedFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identities[name]
}

// Удаляет наблюдение только при совпадении с expected, сохраняя более позднюю запись того же пути.
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

// RemoveOwnedContext удаляет объект по доказательству журнала либо по проверенному содержимому и
// локальной идентичности для прежних записей без FileID. Неизвестный или подменённый объект
// сохраняется; каталоги удаляются только пустыми.
func (s *FileStore) RemoveOwnedContext(ctx context.Context, file core.OwnedFile) error {
	// err сообщает о некорректной записи владельца удаляемого файла.
	if err := file.Validate(); err != nil {
		return err
	}
	if file.ID != "" {
		// exists показывает наличие точного доказательства файла. evidenceErr сообщает об отказе
		// проверки журнала.
		exists, evidenceErr := s.evidence.Exists(ctx, fileIdentity(file))
		if !exists {
			if evidenceErr != nil {
				return evidenceErr
			}
			return errors.Join(ErrCleanupPending, processowner.ErrUnknown)
		}
		if file.ContentHash != "" {
			// f содержит дескриптор файла с проверенным содержимым перед удалением. err
			// сообщает о несовпадении или отказе открытия.
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
		// known сохраняет локальное наблюдение пути до журналируемого удаления.
		known := s.remembered(file.ManagedPath)
		// err сообщает об отказе удаления по долговечному доказательству.
		if err := s.evidence.Remove(ctx, fileIdentity(file)); err != nil {
			return err
		}
		s.forget(file.ManagedPath, known)
		return nil
	}
	if file.ContentHash != "" {
		return s.RemoveExact(ctx, file.ManagedPath, file.SizeBytes, file.ContentHash)
	}
	// info содержит свойства прежнего файлового объекта без FileID. err сообщает об отказе чтения
	// пути.
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
	// known содержит локальное наблюдение прежнего объекта для проверки его идентичности перед
	// удалением.
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

// Синхронизирует ближайший существующий родительский каталог и его предков после удаления, в том числе
// когда часть пути уже отсутствует.
func (s *FileStore) syncExistingParents(name string) error {
	// dir обозначает очередной родительский каталог в поиске ближайшего существующего.
	for dir := path.Dir(name); ; dir = path.Dir(dir) {
		// err содержит результат проверки существования очередного родительского каталога.
		_, err := s.root.Lstat(dir)
		if err == nil {
			return s.syncParents(dir + "/entry")
		}
		if !errors.Is(err, os.ErrNotExist) || dir == "." {
			return err
		}
	}
}

// Create создаёт обычный файл исключительно по новому управляемому пути с правами 0600 и запоминает
// его идентичность; дескриптор закрывает вызывающий.
func (s *FileStore) Create(name string) (*os.File, error) {
	// err сообщает о выходе нового имени за разрешённую форму управляемого пути.
	if err := core.ValidateManagedPath(name); err != nil {
		return nil, err
	}
	// f содержит исключительно созданный новый файл. err сообщает об отказе его создания.
	f, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	// info содержит свойства нового файла для сохранения его локальной идентичности.
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	s.remember(name, info)
	return f, nil
}

// OpenRegular открывает новый независимый дескриптор и отвергает ссылки/специальные файлы.
func (s *FileStore) OpenRegular(name string) (*os.File, error) {
	// err сообщает о некорректном управляемом пути открываемого файла.
	if err := core.ValidateManagedPath(name); err != nil {
		return nil, err
	}
	// before содержит свойства пути до открытия. err сообщает об отказе их чтения.
	before, err := s.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, core.ErrCorrupt
	}
	// f содержит независимо открытый дескриптор проверяемого файла.
	f, err := s.root.Open(name)
	if err != nil {
		return nil, err
	}
	// after содержит свойства открытого дескриптора для обнаружения подмены относительно before.
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

// OpenExact сверяет обычный файл с размером и SHA256, возвращает отдельный дескриптор в позиции 0 и
// запоминает идентичность; при ошибке дескриптор закрывается.
func (s *FileStore) OpenExact(ctx context.Context, name string, size uint64, digest string) (*os.File, error) {
	if size == 0 || size > math.MaxInt64 || len(digest) != sha256.Size*2 || digest != strings.ToLower(digest) {
		return nil, core.ErrCorrupt
	}
	// err сообщает о некорректной шестнадцатеричной записи ожидаемого SHA256.
	if _, err := hex.DecodeString(digest); err != nil {
		return nil, core.ErrCorrupt
	}
	// f содержит обычный файл для проверки размера и содержимого. err сообщает об отказе безопасного
	// открытия.
	f, err := s.OpenRegular(name)
	if err != nil {
		return nil, err
	}
	// actualSize содержит фактическое число прочитанных байтов. actualHash содержит SHA256
	// проверенного содержимого.
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
	// info содержит свойства проверенного файла для сохранения локальной идентичности.
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	s.remember(name, info)
	return f, nil
}

// contextReader проверяет отмену перед каждым чтением вложенного источника.
type contextReader struct {
	// ctx задаёт отмену и срок чтения.
	ctx context.Context
	// r содержит вложенный источник байтов, чьё уже выполняющееся чтение отмена не прерывает.
	r io.Reader
}

// Read проверяет отмену перед очередным чтением; уже выполняющееся чтение вложенного источника этим не
// прерывается.
func (r contextReader) Read(p []byte) (int, error) {
	// err сообщает об отмене либо истечении срока перед следующим чтением.
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
	// w ограничивает объём передаваемых получателю байтов до записи.
	w := &boundedWriter{w: dst, remaining: limit}
	// n содержит число скопированных байтов. err сообщает об отказе чтения, записи, отмене или
	// превышении лимита.
	n, err := io.Copy(w, io.LimitReader(contextReader{ctx, src}, int64(limit)+1))
	return uint64(n), err
}

// boundedWriter ограничивает суммарное число байтов, передаваемых вложенному получателю.
type boundedWriter struct {
	// w содержит получателя, которому передаются только помещающиеся в бюджет блоки.
	w io.Writer
	// remaining содержит оставшийся разрешённый объём записи в байтах.
	remaining uint64
}

// Write отвергает целый блок, превышающий оставшийся бюджет, до передачи данных писателю; бюджет
// уменьшается на фактически записанное число байтов.
func (w *boundedWriter) Write(p []byte) (int, error) {
	if uint64(len(p)) > w.remaining {
		return 0, ErrLimit
	}
	// n содержит фактически записанное число байтов. err сообщает об отказе вложенного получателя.
	n, err := w.w.Write(p)
	w.remaining -= uint64(n)
	return n, err
}

// InspectFile проверяет размер и считает SHA256 прочитанного дескриптора.
func InspectFile(ctx context.Context, f *os.File, limit uint64) (uint64, string, error) {
	// info содержит исходные свойства файлового дескриптора. err сообщает об отказе их чтения.
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
	// h накапливает SHA256 прочитанного содержимого файла.
	h := sha256.New()
	// n содержит фактическое число байтов, прочитанных при вычислении SHA256.
	n, err := CopyBounded(ctx, h, f, limit)
	if err != nil {
		return 0, "", err
	}
	// after содержит свойства файла после чтения для проверки идентичности, размера и времени
	// изменения.
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
	// err содержит результат проверки, что конечный путь ещё отсутствует.
	if _, err := s.root.Lstat(final); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(core.ErrConflict, err)
	}
	// err сообщает об отказе переименования подготовленного файла.
	if err := s.root.Rename(staging, final); err != nil {
		return err
	}
	s.mu.Lock()
	// info содержит запомненную идентичность исходного пути, переносимую на конечный путь.
	if info := s.identities[staging]; info != nil {
		s.identities[final] = info
		delete(s.identities, staging)
	}
	s.mu.Unlock()
	return s.syncParents(staging, final)
}

// Синхронизирует каждый родительский каталог переданных путей до корня, открывая каждый общий каталог
// один раз.
func (s *FileStore) syncParents(names ...string) error {
	// seen отмечает каталоги, уже синхронизированные в рамках текущего вызова.
	seen := map[string]bool{}
	// name содержит очередной изменённый путь, чьи родители должны быть синхронизированы.
	for _, name := range names {
		// dir обозначает текущий родительский каталог при подъёме к корню дерева.
		for dir := path.Dir(name); ; dir = path.Dir(dir) {
			if !seen[dir] {
				seen[dir] = true
				// f содержит открытый каталог для синхронизации. err сообщает об отказе
				// открытия.
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
	// f содержит файл с точным проверенным размером и SHA256. err сообщает об отсутствии,
	// несовпадении или отказе открытия.
	f, err := s.OpenExact(ctx, name, size, digest)
	if errors.Is(err, os.ErrNotExist) {
		return s.syncExistingParents(name)
	}
	if err != nil {
		return err
	}
	// known сохраняет локальное наблюдение удаляемого файла, чтобы не удалить более позднюю запись
	// наблюдения.
	known := s.remembered(name)
	// info содержит свойства проверенного дескриптора до его закрытия.
	info, err := f.Stat()
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	// current содержит текущие свойства пути для обнаружения подмены перед удалением.
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
