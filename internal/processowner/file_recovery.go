package processowner

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
)

// RetryAcknowledgements повторяет только долговечные ACKING после ранее подтверждённого FILE ack.
func (j *FileJournal) RetryAcknowledgements(ctx context.Context, after string, limit int) (string, error) {
	records, next, err := j.acknowledgementPage(ctx, after, limit)
	pending := err
	for _, record := range records {
		if record.Phase == "ACKING" {
			if err = j.Acknowledge(ctx, record.Identity); err != nil {
				pending = errors.Join(pending, err)
			}
		}
	}
	return next, pending
}

func (j *FileJournal) acknowledgementPage(ctx context.Context, after string, limit int) ([]fileRecord, string, error) {
	if limit <= 0 || limit > j.limits.PageSize {
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
	dir, err := j.root.Open(evidenceDirectory + "/records")
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = dir.Close() }()
	for n := 0; n < offset; {
		names, e := dir.Readdirnames(min(offset-n, j.limits.PageSize))
		n += len(names)
		if errors.Is(e, io.EOF) {
			return nil, "", nil
		}
		if e != nil {
			return nil, "", e
		}
	}
	names, err := dir.Readdirnames(limit)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, "", err
	}
	out := make([]fileRecord, 0, len(names))
	var pending error
	for _, name := range names {
		if err = ctx.Err(); err != nil {
			return out, "", errors.Join(pending, err)
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
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
	next := ""
	if len(names) == limit {
		next = strconv.Itoa(offset + len(names))
	}
	return out, next, pending
}
