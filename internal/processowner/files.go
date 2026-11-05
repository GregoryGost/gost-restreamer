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

const evidenceDirectory = ".processowner-evidence"

type FileJournal struct {
	root   *os.Root
	limits Limits
}
type fileRecord struct {
	Version               int
	Identity              FileIdentity
	Anchor, Marker, Phase string
	Peer                  *FileIdentity `json:",omitempty"`
}

func NewFileJournal(directory string, limits Limits) (*FileJournal, error) {
	if limits.Validate() != nil {
		return nil, ErrInvalid
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{evidenceDirectory, evidenceDirectory + "/records", evidenceDirectory + "/anchors", evidenceDirectory + "/locks"} {
		if err = root.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, errors.Join(err, root.Close())
		}
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
func (j *FileJournal) Close() error { return j.root.Close() }

func fileKey(id FileIdentity) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(id.FileID))) }
func fileRecordPath(id FileIdentity) string {
	return evidenceDirectory + "/records/" + fileKey(id) + ".json"
}

func validateIdentity(id FileIdentity) error {
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
func validManagedPath(name string) bool {
	return name != "" && name != "." && !path.IsAbs(name) && path.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../") && !strings.ContainsRune(name, 0) && !strings.Contains(name, "\\")
}

func (j *FileJournal) lock(ctx context.Context, id FileIdentity, create bool) (*os.File, error) {
	if err := validateIdentity(id); err != nil {
		return nil, err
	}
	name := evidenceDirectory + "/locks/" + fileKey(id) + ".lock"
	if create {
		if err := createLock(j.root, name); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	return lockExisting(ctx, j.root, name, true)
}
func (j *FileJournal) read(id FileIdentity) (fileRecord, error) {
	var record fileRecord
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
func (j *FileJournal) write(record fileRecord) error {
	return writeRecord(j.root, fileRecordPath(record.Identity), record, j.limits.MaxRecordBytes)
}

func newFileRecord(id FileIdentity) fileRecord {
	record := fileRecord{Version: ProtocolVersion, Identity: id, Anchor: evidenceDirectory + "/anchors/" + fileKey(id), Phase: "RESERVED"}
	if id.Kind == Directory {
		record.Marker = id.ManagedPath + "/.owner-" + fileKey(id)
	}
	return record
}

// Reserve фиксирует точный FILE до mkdir/create и не разрешает повторное использование закрытой записи.
func (j *FileJournal) Reserve(ctx context.Context, id FileIdentity) error {
	lock, err := j.lock(ctx, id, true)
	if err != nil {
		return err
	}
	defer func() { _ = unlock(lock) }()
	if _, err = j.root.Lstat(fileRecordPath(id)); err == nil {
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

// Register вызывается после exclusive create, до передачи дескриптора writer.
func (j *FileJournal) Register(ctx context.Context, id FileIdentity) error {
	lock, err := j.lock(ctx, id, true)
	if err != nil {
		return err
	}
	defer func() { _ = unlock(lock) }()
	_, err = j.root.Lstat(fileRecordPath(id))
	record := newFileRecord(id)
	if err == nil {
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
	info, err := j.root.Lstat(id.ManagedPath)
	if err != nil {
		return err
	}
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
			marker, e1 := j.root.Lstat(record.Marker)
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

func (j *FileJournal) syncParents(names ...string) error {
	for _, name := range names {
		for dir := path.Dir(name); ; dir = path.Dir(dir) {
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

func (j *FileJournal) verify(record fileRecord) error {
	if record.Phase == "ACKED" || record.Phase == "ACKING" || record.Phase == "REMOVED" || record.Phase == "DIR_REMOVING" || record.Phase == "RENAMED" {
		return ErrClosed
	}
	name := record.Identity.ManagedPath
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
	current, err := j.root.Lstat(name)
	if err != nil {
		return errors.Join(ErrUnknown, err)
	}
	anchor, err := j.root.Lstat(record.Anchor)
	if err != nil {
		return errors.Join(ErrUnknown, err)
	}
	if current.Mode()&os.ModeSymlink != 0 || anchor.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, anchor) {
		return ErrUnknown
	}
	// Два текущих открытых объекта дополняют Lstat и удерживают identity во время проверки.
	if current.Mode().IsRegular() {
		a, e := j.root.Open(name)
		if e != nil {
			return e
		}
		b, e := j.root.Open(record.Anchor)
		if e != nil {
			return errors.Join(e, a.Close())
		}
		ai, e1 := a.Stat()
		bi, e2 := b.Stat()
		closeErr := errors.Join(a.Close(), b.Close())
		if e1 != nil || e2 != nil || !os.SameFile(ai, bi) || !os.SameFile(current, ai) {
			return errors.Join(ErrUnknown, e1, e2, closeErr)
		}
		return closeErr
	}
	return nil
}

func (j *FileJournal) Verify(ctx context.Context, id FileIdentity) error {
	lock, err := j.lock(ctx, id, false)
	if err != nil {
		return err
	}
	defer func() { _ = unlock(lock) }()
	record, err := j.read(id)
	if err != nil {
		return err
	}
	return j.verify(record)
}

// Exists отличает отсутствие legacy-записи от повреждённого или частичного evidence.
func (j *FileJournal) Exists(ctx context.Context, id FileIdentity) (bool, error) {
	if err := validateIdentity(id); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, err := j.root.Lstat(fileRecordPath(id))
	if errors.Is(err, os.ErrNotExist) {
		for _, name := range []string{evidenceDirectory + "/anchors/" + fileKey(id), evidenceDirectory + "/locks/" + fileKey(id) + ".lock"} {
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
	lock, err := j.lock(ctx, id, false)
	if err != nil {
		return true, err
	}
	defer func() { _ = unlock(lock) }()
	record, err := j.read(id)
	if err != nil {
		return true, err
	}
	if record.Phase == "ACKED" || record.Phase == "REMOVED" || record.Phase == "ACKING" || record.Phase == "RESERVED" {
		return true, nil
	}
	return true, j.verify(record)
}

// Rename связывает обе записи до изменения directory entries и не заменяет готовый объект.
func (j *FileJournal) Rename(ctx context.Context, source, target FileIdentity) error {
	if validateIdentity(source) != nil || validateIdentity(target) != nil || source.FileID == target.FileID || source.ManagedPath == target.ManagedPath || source.GenerationID != target.GenerationID || source.Kind != RegularFile || target.Kind != RegularFile {
		return ErrInvalid
	}
	first, second := source, target
	if fileKey(first) > fileKey(second) {
		first, second = second, first
	}
	l1, err := j.lock(ctx, first, true)
	if err != nil {
		return err
	}
	defer func() { _ = unlock(l1) }()
	l2, err := j.lock(ctx, second, true)
	if err != nil {
		return err
	}
	defer func() { _ = unlock(l2) }()
	a, err := j.read(source)
	if err != nil {
		return err
	}
	if a.Phase == "ACKED" || a.Phase == "ACKING" {
		return ErrClosed
	}
	var b fileRecord
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
	_, sourceErr := j.root.Lstat(source.ManagedPath)
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

// Remove требует уже проверенных SQL-разрешения и закрытия всех writers.
func (j *FileJournal) Remove(ctx context.Context, id FileIdentity) error {
	lock, err := j.lock(ctx, id, false)
	if err != nil {
		return err
	}
	defer func() { _ = unlock(lock) }()
	record, err := j.read(id)
	if err != nil {
		return err
	}
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
		// Проверяется полный ограниченный список: неизвестные children не удаляются.
		dir, e := j.root.Open(id.ManagedPath)
		if e != nil {
			return e
		}
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

// Acknowledge освобождает hard link после FILE ack, сохраняя небольшой tombstone.
func (j *FileJournal) Acknowledge(ctx context.Context, id FileIdentity) error {
	lock, err := j.lock(ctx, id, false)
	if err != nil {
		return err
	}
	defer func() { _ = unlock(lock) }()
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
