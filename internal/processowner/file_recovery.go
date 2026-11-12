package processowner

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
)

// RetryAcknowledgements повторяет только долговечные ACKING после подтверждения очистки владельцем.
func (j *FileJournal) RetryAcknowledgements(ctx context.Context, after string, limit int) (string, error) {
	// records содержит пригодные записи ACKING страницы; next задаёт следующий курсор; err сохраняет
	// ошибки чтения страницы и повторных подтверждений.
	records, next, err := j.acknowledgementPage(ctx, after, limit)
	// pending объединяет ошибки страницы и повторов Acknowledge, сохраняя обработку остальных
	// записей.
	pending := err
	// record содержит очередную запись, чьё подтверждение ACKING можно повторить.
	for _, record := range records {
		if record.Phase == "ACKING" {
			if err = j.Acknowledge(ctx, record.Identity); err != nil {
				pending = errors.Join(pending, err)
			}
		}
	}
	return next, pending
}

// Читает ограниченную страницу записей ACKING по смещению в каталоге. Повреждённые записи пропускает с
// ErrUnknown, сохраняя исправные записи и следующий курсор; снимок каталога между вызовами не
// закрепляется.
func (j *FileJournal) acknowledgementPage(ctx context.Context, after string, limit int) ([]fileRecord, string, error) {
	if limit <= 0 || limit > j.limits.PageSize {
		return nil, "", ErrInvalid
	}
	// offset задаёт число имён каталога, пропускаемых по числовому курсору.
	offset := 0
	// err сохраняет ошибки разбора курсора, открытия и чтения каталога и проверки записей.
	var err error
	if after != "" {
		offset, err = strconv.Atoi(after)
		if err != nil || offset < 0 {
			return nil, "", ErrInvalid
		}
	}
	// dir содержит открытый каталог JSON-доказательств для ограниченного чтения страницы.
	dir, err := j.root.Open(evidenceDirectory + "/records")
	if err != nil {
		return nil, "", err
	}
	defer func() {
		// Закрывает дескриптор каталога записей после чтения ограниченной страницы подтверждений.
		_ = dir.Close()
	}()
	// n считает уже пропущенные имена до запрошенного смещения каталога.
	for n := 0; n < offset; {
		// names содержит очередную порцию пропускаемых имён; e сообщает о конце либо отказе
		// чтения каталога.
		names, e := dir.Readdirnames(min(offset-n, j.limits.PageSize))
		n += len(names)
		if errors.Is(e, io.EOF) {
			return nil, "", nil
		}
		if e != nil {
			return nil, "", e
		}
	}
	// names содержит имена текущей ограниченной страницы файловых доказательств.
	names, err := dir.Readdirnames(limit)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, "", err
	}
	// out собирает пригодные записи ACKING текущей страницы.
	out := make([]fileRecord, 0, len(names))
	// pending накапливает недостоверные доказательства, позволяя вернуть остальные исправные записи.
	var pending error
	// name содержит очередное имя записи для проверки расширения и соответствия FileID.
	for _, name := range names {
		if err = ctx.Err(); err != nil {
			return out, "", errors.Join(pending, err)
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		// record содержит прочитанную запись доказательства для проверки версии, идентичности и
		// стадии ACKING.
		var record fileRecord
		if err = readRecord(j.root, evidenceDirectory+"/records/"+name, &record, j.limits.MaxRecordBytes); err != nil {
			pending = errors.Join(pending, ErrUnknown)
			continue
		}
		if validateIdentity(record.Identity) != nil || fileKey(record.Identity)+".json" != name || record.Version != ProtocolVersion {
			pending = errors.Join(pending, ErrUnknown)
			continue
		}
		if record.Phase == "ACKING" {
			out = append(out, record)
		}
	}
	// next содержит следующий числовой курсор либо пустую строку при неполной последней странице.
	next := ""
	if len(names) == limit {
		next = strconv.Itoa(offset + len(names))
	}
	return out, next, pending
}
