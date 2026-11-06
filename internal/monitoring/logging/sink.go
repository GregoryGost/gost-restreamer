// Package logging предоставляет ограниченный асинхронный JSON-журнал приложения.
package logging

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"sync"
	"time"
)

var (
	ErrInvalid = errors.New("неверная конфигурация журнала")
	ErrPending = errors.New("завершение записи журнала ещё не подтверждено")
	ErrBusy    = errors.New("stderr журнала уже имеет владельца")
)

var stderrOwner struct {
	sync.Mutex
	active bool
}

type Config struct {
	MaxRecords      int
	MaxBytes        uint64
	MaxRecordBytes  uint64
	ShutdownTimeout time.Duration
	Level           slog.Level
}

func (c Config) Validate() error {
	if c.MaxRecords <= 0 || uint64(c.MaxRecords) > uint64(math.MaxInt)/24 || c.MaxBytes == 0 || c.MaxRecordBytes == 0 || c.MaxRecordBytes > c.MaxBytes || c.MaxRecordBytes > uint64(math.MaxInt) || c.ShutdownTimeout <= 0 {
		return ErrInvalid
	}
	return nil
}

// Stats учитывает также запись, уже переданную физическому получателю.
type Stats struct {
	RetainedRecords int
	RetainedBytes   uint64
	Accepted        uint64
	Written         uint64
	Overflow        uint64
	Oversized       uint64
	Closed          uint64
	WriterErrors    uint64
	LastWriteError  error
}

// Sink владеет ровно одним работником записи; физический io.Writer закрывает вызывающий код.
type Sink struct {
	config         Config
	writer         io.Writer
	logger         *slog.Logger
	priorityLogger *slog.Logger
	mu             sync.Mutex
	queue          [][]byte
	head           int
	count          int
	stats          Stats
	closed         bool
	wake           chan struct{}
	done           chan struct{}
	release        func()
}

func New(c Config, writer io.Writer) (*Sink, error) {
	return newSink(c, writer, nil)
}

func newSink(c Config, writer io.Writer, release func()) (*Sink, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if writer == nil {
		return nil, ErrInvalid
	}
	s := &Sink{config: c, writer: writer, queue: make([][]byte, c.MaxRecords), wake: make(chan struct{}, 1), done: make(chan struct{}), release: release}
	s.logger = slog.New(slog.NewJSONHandler(queueWriter{s}, &slog.HandlerOptions{Level: c.Level}))
	s.priorityLogger = slog.New(slog.NewJSONHandler(queueWriter{s}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	go s.run()
	return s, nil
}

// NewStderr сохраняет единственного владельца stderr до окончания записи принятых сообщений.
func NewStderr(c Config) (*Sink, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	stderrOwner.Lock()
	defer stderrOwner.Unlock()
	if stderrOwner.active {
		return nil, ErrBusy
	}
	s, err := newSink(c, os.Stderr, func() {
		stderrOwner.Lock()
		stderrOwner.active = false
		stderrOwner.Unlock()
	})
	if err != nil {
		return nil, err
	}
	stderrOwner.active = true
	return s, nil
}

func (s *Sink) Logger() *slog.Logger  { return s.logger }
func (s *Sink) Done() <-chan struct{} { return s.done }

func (s *Sink) LogConfigFilePriorityWarning(yamlPath, ymlPath, selected string) {
	s.priorityLogger.Warn("config_file_priority", "yaml_path", yamlPath, "yml_path", ymlPath, "selected", selected)
}

func (s *Sink) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

type queueWriter struct{ sink *Sink }

// Write принимает полную неизменяемую JSON-запись только после проверки бюджета.
// Потери видны через Stats, а источник записей никогда не ждёт физического получателя.
func (w queueWriter) Write(p []byte) (int, error) {
	s := w.sink
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		increment(&s.stats.Closed)
	case uint64(len(p)) > s.config.MaxRecordBytes:
		increment(&s.stats.Oversized)
	case s.stats.RetainedRecords >= s.config.MaxRecords || uint64(len(p)) > s.config.MaxBytes-s.stats.RetainedBytes:
		increment(&s.stats.Overflow)
	default:
		b := append([]byte(nil), p...)
		s.queue[(s.head+s.count)%len(s.queue)] = b
		s.count++
		s.stats.RetainedRecords++
		s.stats.RetainedBytes += uint64(len(b))
		increment(&s.stats.Accepted)
		s.signal()
	}
	return len(p), nil
}

func (s *Sink) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Sink) run() {
	defer close(s.done)
	if s.release != nil {
		defer s.release()
	}
	for {
		s.mu.Lock()
		if s.count == 0 {
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			<-s.wake
			continue
		}
		b := s.queue[s.head]
		s.queue[s.head] = nil
		s.head = (s.head + 1) % len(s.queue)
		s.count--
		s.mu.Unlock()

		n, err := s.writer.Write(b)
		if err == nil && n != len(b) {
			err = io.ErrShortWrite
		}
		s.mu.Lock()
		s.stats.RetainedRecords--
		s.stats.RetainedBytes -= uint64(len(b))
		if err != nil {
			increment(&s.stats.WriterErrors)
			s.stats.LastWriteError = err
		} else {
			increment(&s.stats.Written)
		}
		s.mu.Unlock()
	}
}

// Close закрывает допуск и ограниченно ждёт записи принятых сообщений.
// ErrPending сохраняет владельца до Done; повторный вызов ждёт того же работника.
func (s *Sink) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.signal()
	s.mu.Unlock()
	select {
	case <-s.done:
		return s.Stats().LastWriteError
	default:
	}
	timer := time.NewTimer(s.config.ShutdownTimeout)
	defer timer.Stop()
	select {
	case <-s.done:
		return s.Stats().LastWriteError
	case <-ctx.Done():
		return errors.Join(ErrPending, ctx.Err())
	case <-timer.C:
		return errors.Join(ErrPending, context.DeadlineExceeded)
	}
}

// Binary сохраняет произвольные байты обратимо; JSONHandler кодирует []byte в base64.
func Binary(key string, value []byte) slog.Attr {
	return slog.Group(key, slog.String("encoding", "base64"), slog.Any("bytes", value))
}

func increment(value *uint64) {
	if *value < math.MaxUint64 {
		*value++
	}
}
