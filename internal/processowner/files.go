package processowner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"unicode/utf8"
)

// evidenceDirectory задаёт служебный каталог записей, жёстких ссылок и блокировок файловых
// доказательств.
const evidenceDirectory = ".processowner-evidence"

// FileJournal сохраняет доказательства точного файлового объекта и стадии его переименования, удаления и
// подтверждения.
type FileJournal struct {
	// root ограничивает файловые операции выбранным корнем управляемых объектов и доказательств.
	root *os.Root
	// limits задаёт бюджет одной записи и ограничение страниц восстановления подтверждений.
	limits Limits
}

// fileRecord сохраняет точную идентичность объекта, служебные доказательства и стадию файлового
// протокола.
type fileRecord struct {
	// Version задаёт версию протокола файловой записи.
	Version int
	// Identity содержит полную идентичность объекта, сверяемую при каждом чтении доказательства.
	Identity FileIdentity
	// Anchor задаёт служебную жёсткую ссылку на исходный объект; Marker задаёт маркер каталога;
	// Phase хранит стадию файлового протокола.
	Anchor, Marker, Phase string
	// Peer связывает исходную и целевую идентичности при незавершённом переименовании.
	Peer *FileIdentity `json:",omitempty"`
}

// NewFileJournal открывает существующий корень и создаёт в нём каталоги файловых доказательств.
// Проверяет ограничения и запрещает подмену этих каталогов файлами или символическими ссылками;
// возвращённый журнал закрывает вызывающий код.
func NewFileJournal(directory string, limits Limits) (*FileJournal, error) {
	if limits.Validate() != nil {
		return nil, ErrInvalid
	}
	// root содержит открытый корень файлового журнала; err сохраняет результаты открытия и создания
	// служебных каталогов.
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	// name содержит очередной обязательный служебный каталог доказательств.
	for _, name := range []string{evidenceDirectory, evidenceDirectory + "/records", evidenceDirectory + "/anchors", evidenceDirectory + "/locks"} {
		if err = root.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, errors.Join(err, root.Close())
		}
		// info содержит метаданные служебного каталога; e сообщает об отказе проверки отсутствия
		// подмены файлом или ссылкой.
		info, e := root.Lstat(name)
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.Join(ErrUnknown, e, root.Close())
		}
		if err = syncDir(root, path.Dir(name)); err != nil {
			return nil, errors.Join(err, root.Close())
		}
	}
	return &FileJournal{root: root, limits: limits}, nil
}

// Close освобождает дескриптор корня файлового журнала; сохранённые записи и служебные ссылки остаются
// на диске.
func (j *FileJournal) Close() error { return j.root.Close() }

// Вычисляет имя служебной записи из SHA256 точного FileID; остальные поля идентичности отдельно
// сверяются при чтении записи.
func fileKey(id FileIdentity) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(id.FileID))) }

// Возвращает относительный путь JSON-записи файлового доказательства внутри служебного каталога.
func fileRecordPath(id FileIdentity) string {
	return evidenceDirectory + "/records/" + fileKey(id) + ".json"
}

// Проверяет кодировку, обязательные поля, их размеры, вид объекта и допустимость управляемого пути.
// Путь не может указывать в каталог самих доказательств.
func validateIdentity(id FileIdentity) error {
	// value содержит очередное строковое поле идентичности для проверки корректной UTF-8.
	for _, value := range []string{id.FileID, id.GenerationID, id.ManagedPath, id.Kind, id.Owner} {
		if !utf8.ValidString(value) {
			return ErrInvalid
		}
	}
	if id.FileID == "" || id.GenerationID == "" || id.Owner == "" || len(id.FileID) > 512 || len(id.GenerationID) > 512 || len(id.Owner) > 4096 || len(id.ManagedPath) > 4096 {
		return ErrInvalid
	}
	if id.Kind != RegularFile && id.Kind != Directory && id.Kind != Socket {
		return ErrInvalid
	}
	if !validManagedPath(id.ManagedPath) || id.ManagedPath == evidenceDirectory || strings.HasPrefix(id.ManagedPath, evidenceDirectory+"/") {
		return ErrInvalid
	}
	return nil
}

// Проверяет непустой канонический относительный путь без выхода через родителя, нулевого байта и
// обратной косой черты.
func validManagedPath(name string) bool {
	return name != "" && name != "." && !path.IsAbs(name) && path.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../") && !strings.ContainsRune(name, 0) && !strings.Contains(name, "\\")
}

// Проверяет идентичность и получает исключительную блокировку её служебной записи с ожиданием по ctx.
// При create сначала создаёт отсутствующий файл блокировки.
func (j *FileJournal) lock(ctx context.Context, id FileIdentity, create bool) (*os.File, error) {
	// err содержит результат проверки полной файловой идентичности перед получением блокировки.
	if err := validateIdentity(id); err != nil {
		return nil, err
	}
	// name задаёт путь блокировки, вычисленный из точного FileID.
	name := evidenceDirectory + "/locks/" + fileKey(id) + ".lock"
	if create {
		// err содержит результат создания отсутствующей блокировки файлового доказательства.
		if err := createLock(j.root, name); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	return lockExisting(ctx, j.root, name, true)
}

// Читает запись и сверяет версию, полную идентичность, путь служебной ссылки и допустимое состояние.
// Несовпадение даёт ErrUnknown вместо разрешения работать с другим объектом.
func (j *FileJournal) read(id FileIdentity) (fileRecord, error) {
	// record принимает сохранённую запись для проверки полной идентичности, anchor и стадии.
	var record fileRecord
	// err содержит результат чтения и проверки файлового доказательства.
	err := readRecord(j.root, fileRecordPath(id), &record, j.limits.MaxRecordBytes)
	if err == nil && (record.Version != ProtocolVersion || record.Identity != id || record.Anchor != evidenceDirectory+"/anchors/"+fileKey(id)) {
		err = ErrUnknown
	}
	if err == nil {
		switch record.Phase {
		case "RESERVED", "LIVE", "RENAME_SOURCE", "RENAME_TARGET", "RENAMED", "REMOVING", "DIR_REMOVING", "REMOVED", "ACKING", "ACKED":
		default:
			err = ErrUnknown
		}
	}
	return record, err
}

// Сохраняет запись по пути, вычисленному из её FileID, с ограничением размера и синхронизацией
// каталога.
func (j *FileJournal) write(record fileRecord) error {
	return writeRecord(j.root, fileRecordPath(record.Identity), record, j.limits.MaxRecordBytes)
}

// Создаёт несохранённую запись RESERVED с путём служебной ссылки; для каталога также задаёт путь
// маркера владельца.
func newFileRecord(id FileIdentity) fileRecord {
	// record содержит начальную несохранённую запись RESERVED с детерминированными путями anchor и
	// маркера каталога.
	record := fileRecord{Version: ProtocolVersion, Identity: id, Anchor: evidenceDirectory + "/anchors/" + fileKey(id), Phase: "RESERVED"}
	if id.Kind == Directory {
		record.Marker = id.ManagedPath + "/.owner-" + fileKey(id)
	}
	return record
}

// Reserve фиксирует точный FILE до mkdir/create и не разрешает повторное использование закрытой записи.
func (j *FileJournal) Reserve(ctx context.Context, id FileIdentity) error {
	// lock удерживает блокировку резервируемой идентичности; err сохраняет результаты поиска и
	// записи RESERVED.
	lock, err := j.lock(ctx, id, true)
	if err != nil {
		return err
	}
	defer func() {
		// Освобождает блокировку идентичности файла после проверки либо сохранения его резервирования.
		_ = unlock(lock)
	}()
	if _, err = j.root.Lstat(fileRecordPath(id)); err == nil {
		// record содержит уже существующее доказательство; e сообщает об отказе его точной
		// проверки.
		record, e := j.read(id)
		if e != nil {
			return e
		}
		if record.Phase == "RESERVED" || record.Phase == "LIVE" {
			return nil
		}
		return ErrClosed
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return j.write(newFileRecord(id))
}

// Register вызывается после создания нового объекта без замены существующего, до передачи дескриптора для записи.
func (j *FileJournal) Register(ctx context.Context, id FileIdentity) error {
	// lock удерживает блокировку регистрируемого объекта; err сохраняет результаты создания и
	// закрепления доказательства.
	lock, err := j.lock(ctx, id, true)
	if err != nil {
		return err
	}
	defer func() {
		// Освобождает блокировку идентичности после регистрации и проверки созданного файлового объекта.
		_ = unlock(lock)
	}()
	_, err = j.root.Lstat(fileRecordPath(id))
	// record содержит начальное либо прочитанное доказательство регистрируемого объекта.
	record := newFileRecord(id)
	if err == nil {
		// current содержит прежнюю запись; e сообщает об отказе её проверки перед повторной
		// регистрацией.
		current, e := j.read(id)
		if e != nil {
			return e
		}
		if current.Phase == "LIVE" {
			return j.verify(current)
		}
		if current.Phase != "RESERVED" {
			return ErrClosed
		}
		record = current
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else {
		if err = j.write(record); err != nil {
			return err
		}
	}
	// info содержит метаданные созданного объекта для проверки ожидаемого типа.
	info, err := j.root.Lstat(id.ManagedPath)
	if err != nil {
		return err
	}
	// source задаёт объект жёсткой ссылки: сам файл или сокет либо маркер каталога.
	source := id.ManagedPath
	switch id.Kind {
	case RegularFile:
		if !info.Mode().IsRegular() {
			return ErrUnknown
		}
	case Socket:
		if info.Mode()&os.ModeSocket == 0 {
			return ErrUnknown
		}
	case Directory:
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnknown
		}
		// f содержит исключительно создаваемый маркер каталога; e сохраняет ошибки создания,
		// записи и синхронизации маркера.
		f, e := j.root.OpenFile(record.Marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil && !errors.Is(e, os.ErrExist) {
			return e
		}
		if e == nil {
			_, e = f.WriteString(id.Owner)
			e = errors.Join(e, f.Sync(), f.Close())
			if e != nil {
				return e
			}
		} else {
			// marker содержит метаданные существующего маркера; e1 сообщает об отказе их
			// чтения.
			marker, e1 := j.root.Lstat(record.Marker)
			// anchor содержит метаданные служебной ссылки маркера; e2 сообщает об отказе их
			// чтения.
			anchor, e2 := j.root.Lstat(record.Anchor)
			if e1 != nil || e2 != nil || !marker.Mode().IsRegular() || !os.SameFile(marker, anchor) {
				return errors.Join(ErrUnknown, e1, e2)
			}
		}
		source = record.Marker
	}
	if err = j.root.Link(source, record.Anchor); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err = j.syncParents(source, record.Anchor); err != nil {
		return err
	}
	if err = j.verify(record); err != nil {
		return err
	}
	record.Phase = "LIVE"
	return j.write(record)
}

// Синхронизирует все родительские каталоги каждого изменённого пути до корня журнала, чтобы закрепить
// изменения записей каталогов.
func (j *FileJournal) syncParents(names ...string) error {
	// name содержит очередной изменённый путь, чьи родители требуется синхронизировать.
	for _, name := range names {
		// dir задаёт очередной родительский каталог при подъёме до корня журнала.
		for dir := path.Dir(name); ; dir = path.Dir(dir) {
			// err содержит результат синхронизации текущего родительского каталога.
			if err := syncDir(j.root, dir); err != nil {
				return err
			}
			if dir == "." {
				break
			}
		}
	}
	return nil
}

// Сверяет вид объекта и его тождество служебной ссылке, а для каталога — маркер владельца. Закрытые
// состояния отклоняет; для обычных файлов дополнительно сопоставляет одновременно открытые
// дескрипторы.
func (j *FileJournal) verify(record fileRecord) error {
	if record.Phase == "ACKED" || record.Phase == "ACKING" || record.Phase == "REMOVED" || record.Phase == "DIR_REMOVING" || record.Phase == "RENAMED" {
		return ErrClosed
	}
	// name задаёт текущий путь объекта либо его маркера для проверки по anchor.
	name := record.Identity.ManagedPath
	// info содержит метаданные управляемого пути; err сохраняет результаты сверки типа и файловой
	// идентичности.
	info, err := j.root.Lstat(name)
	if err != nil {
		return errors.Join(ErrUnknown, err)
	}
	if record.Identity.Kind == Directory {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || record.Marker != name+"/.owner-"+fileKey(record.Identity) {
			return ErrUnknown
		}
		name = record.Marker
	} else if record.Identity.Kind == RegularFile {
		if !info.Mode().IsRegular() {
			return ErrUnknown
		}
	} else if info.Mode()&os.ModeSocket == 0 {
		return ErrUnknown
	}
	// current содержит метаданные непосредственно сравниваемого объекта либо маркера каталога.
	current, err := j.root.Lstat(name)
	if err != nil {
		return errors.Join(ErrUnknown, err)
	}
	// anchor содержит текущую идентичность служебной жёсткой ссылки.
	anchor, err := j.root.Lstat(record.Anchor)
	if err != nil {
		return errors.Join(ErrUnknown, err)
	}
	if current.Mode()&os.ModeSymlink != 0 || anchor.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, anchor) {
		return ErrUnknown
	}
	// Два текущих открытых объекта дополняют Lstat и сохраняют идентичность объектов во время проверки.
	if current.Mode().IsRegular() {
		// a содержит открытый проверяемый файл; e сохраняет ошибки открытия сравниваемых
		// объектов.
		a, e := j.root.Open(name)
		if e != nil {
			return e
		}
		// b содержит одновременно открытый anchor для устойчивого сравнения текущих
		// дескрипторов.
		b, e := j.root.Open(record.Anchor)
		if e != nil {
			return errors.Join(e, a.Close())
		}
		// ai содержит метаданные открытого файла; e1 сообщает об отказе их чтения.
		ai, e1 := a.Stat()
		// bi содержит метаданные открытого anchor; e2 сообщает об отказе их чтения.
		bi, e2 := b.Stat()
		// closeErr объединяет результаты закрытия обоих сравниваемых дескрипторов.
		closeErr := errors.Join(a.Close(), b.Close())
		if e1 != nil || e2 != nil || !os.SameFile(ai, bi) || !os.SameFile(current, ai) {
			return errors.Join(ErrUnknown, e1, e2, closeErr)
		}
		return closeErr
	}
	return nil
}

// Verify под блокировкой сверяет сохранённую идентичность с текущим объектом и служебной ссылкой.
// Ошибка не разрешает удаление; проверка не заменяет подтверждение завершения пользователей объекта.
func (j *FileJournal) Verify(ctx context.Context, id FileIdentity) error {
	// lock удерживает блокировку проверяемой идентичности; err сохраняет результаты получения
	// блокировки и чтения записи.
	lock, err := j.lock(ctx, id, false)
	if err != nil {
		return err
	}
	defer func() {
		// Освобождает блокировку идентичности после проверки текущего объекта по служебной ссылке.
		_ = unlock(lock)
	}()
	// record содержит точное доказательство для сверки текущего файлового объекта.
	record, err := j.read(id)
	if err != nil {
		return err
	}
	return j.verify(record)
}

// Exists отличает отсутствие прежней записи от повреждённых или частичных доказательств владения.
// true вместе с ошибкой означает найденные, но непроверенные сведения.
// false вместе с nil означает отсутствие записи, служебной ссылки и блокировки.
func (j *FileJournal) Exists(ctx context.Context, id FileIdentity) (bool, error) {
	// err содержит результат проверки допустимости идентичности перед поиском доказательства.
	if err := validateIdentity(id); err != nil {
		return false, err
	}
	// err содержит причину отмены контекста до проверки существования записи.
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// err содержит результат поиска JSON-записи для различения отсутствия и частичного
	// доказательства.
	_, err := j.root.Lstat(fileRecordPath(id))
	if errors.Is(err, os.ErrNotExist) {
		// name содержит путь возможного оставшегося anchor либо блокировки без JSON-записи.
		for _, name := range []string{evidenceDirectory + "/anchors/" + fileKey(id), evidenceDirectory + "/locks/" + fileKey(id) + ".lock"} {
			// e содержит результат проверки существования части файлового доказательства.
			if _, e := j.root.Lstat(name); e == nil {
				return true, ErrUnknown
			} else if !errors.Is(e, os.ErrNotExist) {
				return true, e
			}
		}
		return false, nil
	}
	if err != nil {
		return true, err
	}
	// lock удерживает блокировку найденного доказательства во время проверки его стадии.
	lock, err := j.lock(ctx, id, false)
	if err != nil {
		return true, err
	}
	defer func() {
		// Освобождает блокировку идентичности после проверки существующей записи файлового журнала.
		_ = unlock(lock)
	}()
	// record содержит найденное доказательство, чьи закрытые либо незавершённые стадии проверяются
	// отдельно.
	record, err := j.read(id)
	if err != nil {
		return true, err
	}
	if record.Phase == "ACKED" || record.Phase == "REMOVED" || record.Phase == "ACKING" || record.Phase == "RESERVED" {
		return true, nil
	}
	return true, j.verify(record)
}

// Rename связывает обе записи до изменения записей каталогов и не заменяет готовый объект.
func (j *FileJournal) Rename(ctx context.Context, source, target FileIdentity) error {
	if validateIdentity(source) != nil || validateIdentity(target) != nil || source.FileID == target.FileID || source.ManagedPath == target.ManagedPath || source.GenerationID != target.GenerationID || source.Kind != RegularFile || target.Kind != RegularFile {
		return ErrInvalid
	}
	// first задаёт идентичность с первым по порядку ключом; second задаёт вторую для одинакового
	// порядка блокировок переименования.
	first, second := source, target
	if fileKey(first) > fileKey(second) {
		first, second = second, first
	}
	// l1 удерживает первую упорядоченную блокировку; err сохраняет результаты чтения и выполнения
	// переименования.
	l1, err := j.lock(ctx, first, true)
	if err != nil {
		return err
	}
	defer func() {
		// Освобождает первую из упорядоченных блокировок двух идентичностей после переименования или отказа.
		_ = unlock(l1)
	}()
	// l2 удерживает вторую упорядоченную блокировку исходной и целевой идентичностей.
	l2, err := j.lock(ctx, second, true)
	if err != nil {
		return err
	}
	defer func() {
		// Освобождает вторую из упорядоченных блокировок двух идентичностей после переименования или отказа.
		_ = unlock(l2)
	}()
	// a содержит доказательство исходного объекта переименования.
	a, err := j.read(source)
	if err != nil {
		return err
	}
	if a.Phase == "ACKED" || a.Phase == "ACKING" {
		return ErrClosed
	}
	// b содержит существующую либо новую запись целевого объекта переименования.
	var b fileRecord
	// statErr показывает наличие прежней записи целевой идентичности.
	_, statErr := j.root.Lstat(fileRecordPath(target))
	if statErr == nil {
		b, err = j.read(target)
		if err != nil {
			return err
		}
		if b.Phase != "RESERVED" && (b.Phase == "ACKED" || b.Phase == "ACKING" || b.Peer == nil || *b.Peer != source) {
			return ErrClosed
		}
	} else {
		if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		b = newFileRecord(target)
	}
	if b.Phase == "RESERVED" {
		if a.Phase != "LIVE" {
			return ErrUnknown
		}
		if err = j.verify(a); err != nil {
			return err
		}
		// e содержит результат проверки отсутствия целевого пути до начала переименования.
		if _, e := j.root.Lstat(target.ManagedPath); !errors.Is(e, os.ErrNotExist) {
			return errors.Join(ErrUnknown, e)
		}
		b.Phase = "RENAME_TARGET"
		b.Peer = &source
		if err = j.write(b); err != nil {
			return err
		}
	}
	if b.Phase == "RENAME_TARGET" {
		if err = j.root.Link(a.Anchor, b.Anchor); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err = j.syncParents(b.Anchor); err != nil {
			return err
		}
	}
	if a.Phase == "LIVE" {
		a.Phase = "RENAME_SOURCE"
		a.Peer = &target
		if err = j.write(a); err != nil {
			return err
		}
	}
	if a.Peer == nil || *a.Peer != target {
		return ErrUnknown
	}
	// sourceErr показывает наличие исходного пути при восстановлении стадии переименования.
	_, sourceErr := j.root.Lstat(source.ManagedPath)
	// targetErr показывает наличие целевого пути для различения ещё не выполненного и уже
	// выполненного Rename.
	_, targetErr := j.root.Lstat(target.ManagedPath)
	if sourceErr == nil && errors.Is(targetErr, os.ErrNotExist) {
		if err = j.verify(a); err != nil {
			return err
		}
		if err = j.root.Rename(source.ManagedPath, target.ManagedPath); err != nil {
			return err
		}
	} else if !errors.Is(sourceErr, os.ErrNotExist) || targetErr != nil {
		return errors.Join(ErrUnknown, sourceErr, targetErr)
	}
	if err = j.syncParents(source.ManagedPath, target.ManagedPath); err != nil {
		return err
	}
	if err = j.verify(b); err != nil {
		return err
	}
	b.Phase = "LIVE"
	if err = j.write(b); err != nil {
		return err
	}
	a.Phase = "RENAMED"
	return j.write(a)
}

// Remove требует разрешения владельца на удаление и завершения всех операций с объектом.
// SQL-разрешение для файлов заглушек и завершение пользователей временных ресурсов
// проверяет вызывающий код; журнал подтверждает принадлежность самого объекта.
func (j *FileJournal) Remove(ctx context.Context, id FileIdentity) error {
	// lock удерживает блокировку удаляемой идентичности; err сохраняет результаты чтения
	// доказательства и удаления.
	lock, err := j.lock(ctx, id, false)
	if err != nil {
		return err
	}
	defer func() {
		// Освобождает блокировку файловой записи после удаления либо отказа проверки владения.
		_ = unlock(lock)
	}()
	// record содержит точное доказательство и стадию удаляемого объекта.
	record, err := j.read(id)
	if err != nil {
		return err
	}
	// statErr показывает текущее наличие управляемого пути для восстановления стадий удаления.
	_, statErr := j.root.Lstat(id.ManagedPath)
	if record.Phase == "RESERVED" && errors.Is(statErr, os.ErrNotExist) {
		record.Phase = "REMOVED"
		return j.write(record)
	}
	if record.Phase == "ACKED" {
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		return ErrUnknown
	}
	if record.Phase == "REMOVED" {
		if !errors.Is(statErr, os.ErrNotExist) {
			return ErrUnknown
		}
		return nil
	}
	if record.Phase == "DIR_REMOVING" {
		return ErrUnknown
	}
	if record.Phase == "RENAMED" || record.Phase == "REMOVING" {
		if errors.Is(statErr, os.ErrNotExist) {
			if err = j.syncParents(id.ManagedPath); err != nil {
				return err
			}
			record.Phase = "REMOVED"
			return j.write(record)
		}
	}
	if record.Phase != "LIVE" && record.Phase != "REMOVING" {
		return ErrUnknown
	}
	if err = j.verify(record); err != nil {
		return err
	}
	if id.Kind == Directory {
		// Допускается только служебный маркер; наличие любого другого дочернего объекта запрещает удаление.
		// dir содержит открытый удаляемый каталог; e сохраняет ошибки проверки его содержимого и
		// закрытия.
		dir, e := j.root.Open(id.ManagedPath)
		if e != nil {
			return e
		}
		// names содержит не более двух имён для проверки, что в каталоге остался только
		// служебный маркер.
		names, e := dir.Readdirnames(2)
		e = errors.Join(e, dir.Close())
		if e != nil || len(names) != 1 || names[0] != path.Base(record.Marker) {
			return errors.Join(ErrPending, e)
		}
		record.Phase = "DIR_REMOVING"
		if err = j.write(record); err != nil {
			return err
		}
		if err = j.root.Remove(record.Marker); err != nil {
			return err
		}
		if err = j.syncParents(record.Marker); err != nil {
			return err
		}
	} else {
		record.Phase = "REMOVING"
		if err = j.write(record); err != nil {
			return err
		}
	}
	if err = j.root.Remove(id.ManagedPath); err != nil {
		return err
	}
	if err = j.syncParents(id.ManagedPath); err != nil {
		return err
	}
	record.Phase = "REMOVED"
	return j.write(record)
}

// Acknowledge освобождает служебную жёсткую ссылку после подтверждения очистки владельцем,
// сохраняя запись о завершённом удалении.
func (j *FileJournal) Acknowledge(ctx context.Context, id FileIdentity) error {
	// lock удерживает блокировку подтверждаемого удаления; err сохраняет результаты записи ACKING,
	// удаления anchor и записи ACKED.
	lock, err := j.lock(ctx, id, false)
	if err != nil {
		return err
	}
	defer func() {
		// Освобождает блокировку файловой записи после подтверждения и освобождения служебной ссылки.
		_ = unlock(lock)
	}()
	// record содержит точную запись удалённого объекта, чей anchor освобождается после разрешения
	// владельца.
	record, err := j.read(id)
	if err != nil {
		return err
	}
	if record.Phase == "ACKED" {
		return nil
	}
	if record.Phase != "REMOVED" && record.Phase != "ACKING" {
		return ErrPending
	}
	if _, err = j.root.Lstat(id.ManagedPath); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrUnknown, err)
	}
	if record.Phase != "ACKING" {
		record.Phase = "ACKING"
		if err = j.write(record); err != nil {
			return err
		}
	}
	if err = j.root.Remove(record.Anchor); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = j.syncParents(record.Anchor); err != nil {
		return err
	}
	record.Phase = "ACKED"
	return j.write(record)
}
