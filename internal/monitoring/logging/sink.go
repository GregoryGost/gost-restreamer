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
	// ErrInvalid сообщает о недопустимом получателе или бюджетах создания журнала.
	ErrInvalid = errors.New("неверная конфигурация журнала")
	// ErrPending сообщает, что единственный работник ещё не завершил попытки вывода принятых
	// сообщений.
	ErrPending = errors.New("завершение записи журнала ещё не подтверждено")
	// ErrBusy запрещает второго владельца stderr до подтверждённого завершения первого.
	ErrBusy = errors.New("stderr журнала уже имеет владельца")
)

// stderrOwner сохраняет единственное место владельца stderr на срок работы его писателя.
var stderrOwner struct {
	// Mutex защищает проверку, занятие и освобождение места владельца stderr.
	sync.Mutex
	// active отмечает, что stderr ещё закреплён за незавершённым Sink.
	active bool
}

// Config задаёт конечные бюджеты записей, срок ожидания закрытия и обычный порог slog.
type Config struct {
	// MaxRecords ограничивает принятые записи, включая уже переданную физическому получателю.
	MaxRecords int
	// MaxBytes ограничивает суммарные байты принятых, но ещё не завершённых записей.
	MaxBytes uint64
	// MaxRecordBytes ограничивает байты одной полной сериализованной записи.
	MaxRecordBytes uint64
	// ShutdownTimeout ограничивает попытку ожидания завершения работника записи.
	ShutdownTimeout time.Duration
	// Level задаёт порог обычного slog; обязательное предупреждение выбора файла использует
	// отдельный порог WARN.
	Level slog.Level
}

// Validate проверяет конечные бюджеты очереди, максимальной записи и срока завершения с учётом
// представимости размеров в памяти.
func (c Config) Validate() error {
	if c.MaxRecords <= 0 || uint64(c.MaxRecords) > uint64(math.MaxInt)/24 || c.MaxBytes == 0 || c.MaxRecordBytes == 0 || c.MaxRecordBytes > c.MaxBytes || c.MaxRecordBytes > uint64(math.MaxInt) || c.ShutdownTimeout <= 0 {
		return ErrInvalid
	}
	return nil
}

// Stats учитывает также запись, уже переданную физическому получателю.
// Stats возвращает согласованные счётчики допуска, удержания, записи и потерь журнала вместе с последней
// ошибкой получателя.
type Stats struct {
	// RetainedRecords содержит число принятых записей в очереди и выполняемой физической записи.
	RetainedRecords int
	// RetainedBytes содержит байты очереди и выполняемой физической записи до возврата Write.
	RetainedBytes uint64
	// Accepted насыщаемо считает записи, прошедшие допуск в очередь.
	Accepted uint64
	// Written насыщаемо считает полностью записанные сообщения без ошибки получателя.
	Written uint64
	// Overflow насыщаемо считает целые записи, отклонённые общим бюджетом числа или байтов.
	Overflow uint64
	// Oversized насыщаемо считает записи, превышающие отдельный MaxRecordBytes.
	Oversized uint64
	// Closed насыщаемо считает записи, поступившие после закрытия допуска.
	Closed uint64
	// WriterErrors насыщаемо считает ошибки и короткие физические записи.
	WriterErrors uint64
	// LastWriteError сохраняет последнюю ошибку физического получателя для Stats и Close.
	LastWriteError error
}

// Sink владеет ровно одним работником записи; физический io.Writer закрывает вызывающий код.
type Sink struct {
	// config сохраняет проверенные размеры очереди, порог уровня и срок ожидания остановки.
	config Config
	// writer получает целые сериализованные записи от единственного работника; его закрывает
	// вызывающий код.
	writer io.Writer
	// logger применяет настроенный обычный порог и передаёт JSON в общую очередь.
	logger *slog.Logger
	// priorityLogger передаёт обязательное предупреждение выбора файла в ту же очередь независимо от
	// порога ERROR.
	priorityLogger *slog.Logger
	// mu защищает кольцевую очередь, закрытие допуска и согласованные счётчики.
	mu sync.Mutex
	// queue хранит собственные неизменяемые копии ожидающих JSON-записей.
	queue [][]byte
	// head указывает первую ожидающую запись кольцевой очереди.
	head int
	// count содержит число ожидающих записей без уже переданной физическому получателю.
	count int
	// stats учитывает допуск, удержание, результаты физической записи и целые потери.
	stats Stats
	// closed запрещает новый допуск; работник после этого дочитывает принятую очередь.
	closed bool
	// wake неблокирующе будит работника при новой записи или закрытии допуска.
	wake chan struct{}
	// done подтверждает попытки вывода всей очереди и освобождение права на stderr.
	done chan struct{}
	// release освобождает внешнее место владельца stderr после выхода работника, если оно было
	// занято.
	release func()
}

// New создаёт ограниченный асинхронный JSON-журнал с одним работником записи. Физический получатель
// остаётся собственностью вызывающего кода.
func New(c Config, writer io.Writer) (*Sink, error) {
	return newSink(c, writer, nil)
}

// newSink проверяет пределы, создаёт средства обычного журналирования и обязательного WARN и запускает
// единственного работника. Обработчик release вызывается после физического завершения работника.
func newSink(c Config, writer io.Writer, release func()) (*Sink, error) {
	// err содержит отказ проверки бюджетов до выделения очереди и запуска работника.
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if writer == nil {
		return nil, ErrInvalid
	}
	// s содержит новый журнал с фиксированной очередью и собственными сигналами работника.
	s := &Sink{config: c, writer: writer, queue: make([][]byte, c.MaxRecords), wake: make(chan struct{}, 1), done: make(chan struct{}), release: release}
	s.logger = slog.New(slog.NewJSONHandler(queueWriter{s}, &slog.HandlerOptions{Level: c.Level}))
	s.priorityLogger = slog.New(slog.NewJSONHandler(queueWriter{s}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	go s.run()
	return s, nil
}

// NewStderr сохраняет единственного владельца stderr до завершения попыток вывода принятых сообщений.
func NewStderr(c Config) (*Sink, error) {
	// err содержит отказ конфигурации до попытки занять общий stderr.
	if err := c.Validate(); err != nil {
		return nil, err
	}
	stderrOwner.Lock()
	defer stderrOwner.Unlock()
	if stderrOwner.active {
		return nil, ErrBusy
	}
	// s содержит журнал stderr; err содержит отказ его создания до фиксации занятого места.
	s, err := newSink(c, os.Stderr, func() {
		// Освобождает единственное место владельца stderr после завершения работника записи.

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

// Logger возвращает средство журналирования с настроенным порогом уровня; принятые записи проходят
// через общую ограниченную очередь.
func (s *Sink) Logger() *slog.Logger { return s.logger }

// Done закрывается после завершения попыток записи всей принятой очереди и освобождения права на
// stderr, если Sink им владел. Ошибки вывода доступны через Stats и Close.
func (s *Sink) Done() <-chan struct{} { return s.done }

// LogConfigFilePriorityWarning записывает обязательный WARN о приоритете файлов через ту же очередь
// даже при обычном пороге ERROR.
func (s *Sink) LogConfigFilePriorityWarning(yamlPath, ymlPath, selected string) {
	s.priorityLogger.Warn("config_file_priority", "yaml_path", yamlPath, "yml_path", ymlPath, "selected", selected)
}

// Stats возвращает согласованную копию счётчиков допуска, удержания, записи и потерь вместе с
// последней ошибкой получателя.
func (s *Sink) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// queueWriter передаёт сериализованные slog-записи в ограниченный допуск Sink.
type queueWriter struct { /* sink указывает владельца очереди для записи JSONHandler. */
	sink *Sink
}

// Write принимает полную неизменяемую JSON-запись только после проверки бюджета.
// Потери видны через Stats, а источник записей никогда не ждёт физического получателя.
func (w queueWriter) Write(p []byte) (int, error) {
	// s содержит Sink текущего queueWriter для проверки бюджета под общей блокировкой.
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
		// b содержит собственную копию полной принятой записи, независимую от исходного буфера.
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

// signal неблокирующе уведомляет единственного работника о новых записях или закрытии допуска.
func (s *Sink) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// run последовательно передаёт целые принятые записи физическому получателю и освобождает их бюджет
// после возврата Write. При закрытом допуске работник выходит только после опустошения очереди.
func (s *Sink) run() {
	defer close(s.done)
	if s.release != nil {
		defer s.release()
	}
	for {
		s.mu.Lock()
		if s.count == 0 {
			// closed сохраняет состояние допуска перед снятием блокировки пустой очереди.
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			<-s.wake
			continue
		}
		// b содержит извлечённую целую запись, байтовый бюджет которой удерживается до возврата
		// физического Write.
		b := s.queue[s.head]
		s.queue[s.head] = nil
		s.head = (s.head + 1) % len(s.queue)
		s.count--
		s.mu.Unlock()

		// n содержит число записанных байтов; err содержит отказ получателя либо выявленную
		// короткую запись.
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

// Close закрывает допуск и ограниченно ждёт завершения попыток вывода принятых сообщений.
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
	// timer ограничивает текущий Close, сохраняя того же работника для последующего ожидания.
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

// increment увеличивает счётчик до MaxUint64 без сворачивания при переполнении.
func increment(value *uint64) {
	if *value < math.MaxUint64 {
		*value++
	}
}
