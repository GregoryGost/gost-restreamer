// Package ffmpeg управляет процессами медиатракта и принадлежащими им UDS.
package ffmpeg

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"time"
)

// Diagnostic содержит полное событие; Data хранит исходные байты без преобразования UTF-8.
type Diagnostic struct {
	SessionID string
	GroupID   string
	Time      time.Time
	Attempt   uint64
	Kind      string
	Sequence  uint64
	Data      []byte
	Args      []string
	Message   string
}

func (d Diagnostic) weight() uint64 {
	n := uint64(128+len(d.Kind)) + uint64(len(d.Data)) + uint64(len(d.Message)) + uint64(len(d.SessionID)) + uint64(len(d.GroupID))
	for _, arg := range d.Args {
		n += uint64(16 + len(arg))
	}
	return n
}

type diagnosticQueue struct {
	mu        sync.Mutex
	sessionID string
	groupID   string
	items     []Diagnostic
	bytes     uint64
	maxEvents int
	maxBytes  uint64
	dropped   uint64
	closed    bool
	changed   chan struct{}
}

func newDiagnosticQueue(events int, maxBytes uint64) *diagnosticQueue {
	return &diagnosticQueue{maxEvents: events, maxBytes: maxBytes, changed: make(chan struct{})}
}

func (q *diagnosticQueue) signal() {
	close(q.changed)
	q.changed = make(chan struct{})
}

func (q *diagnosticQueue) push(d Diagnostic) {
	q.mu.Lock()
	defer q.mu.Unlock()
	d.SessionID, d.GroupID = q.sessionID, q.groupID
	weight := d.weight()
	if q.closed || len(q.items) >= q.maxEvents || weight > q.maxBytes-q.bytes {
		q.dropped++
		return
	}
	d.Data = bytes.Clone(d.Data)
	d.Kind = strings.Clone(d.Kind)
	d.Message = strings.Clone(d.Message)
	args := make([]string, len(d.Args))
	for i, arg := range d.Args {
		args[i] = strings.Clone(arg)
	}
	d.Args = args
	q.items = append(q.items, d)
	q.bytes += weight
	q.signal()
}

func (q *diagnosticQueue) next(ctx context.Context) (Diagnostic, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Diagnostic{}, err
		}
		q.mu.Lock()
		if len(q.items) != 0 {
			d := q.items[0]
			q.items[0] = Diagnostic{}
			q.items = q.items[1:]
			q.bytes -= d.weight()
			q.mu.Unlock()
			return d, nil
		}
		closed, changed := q.closed, q.changed
		q.mu.Unlock()
		if closed {
			return Diagnostic{}, io.EOF
		}
		select {
		case <-ctx.Done():
			return Diagnostic{}, ctx.Err()
		case <-changed:
		}
	}
}

func (q *diagnosticQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		q.signal()
	}
}

func (q *diagnosticQueue) losses() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

type diagnosticWriter struct {
	mu       sync.Mutex
	queue    *diagnosticQueue
	attempt  uint64
	kind     string
	chunk    int
	sequence uint64
	progress *progressParser
}

func (w *diagnosticWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.progress.write(p)
	n := len(p)
	for len(p) != 0 {
		size := min(len(p), w.chunk)
		w.sequence++
		w.queue.push(Diagnostic{Time: time.Now(), Attempt: w.attempt, Kind: w.kind,
			Sequence: w.sequence, Data: p[:size]})
		p = p[size:]
	}
	return n, nil
}

func validCommand(binary string, args []string, budget uint64) bool {
	if strings.TrimSpace(binary) == "" || strings.ContainsRune(binary, 0) || uint64(len(binary)) > budget {
		return false
	}
	used := uint64(len(binary))
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) || uint64(len(arg)) > budget-used {
			return false
		}
		used += uint64(len(arg))
	}
	return true
}

func (q *diagnosticQueue) setOwner(sessionID, groupID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sessionID, q.groupID = strings.Clone(sessionID), strings.Clone(groupID)
}
