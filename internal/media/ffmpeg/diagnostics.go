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
	// SessionID обозначает сессию, которой принадлежит диагностическое событие.
	SessionID string
	// GroupID обозначает группу, которой принадлежит диагностическое событие.
	GroupID string
	// Time содержит время создания диагностического события.
	Time time.Time
	// Attempt содержит номер физической попытки, породившей событие.
	Attempt uint64
	// Kind различает команду, stdout, stderr и сообщение о незавершённой очистке.
	Kind string
	// Sequence задаёт порядковый номер фрагмента в соответствующем потоке диагностики попытки.
	Sequence uint64
	Data     []byte
	// Args содержит полную команду запуска отдельными строками.
	Args []string
	// Message содержит текстовое пояснение диагностического события.
	Message string
}

// Вычисляет логический вес полного события, включая данные, идентичность владельца и отдельные
// аргументы команды.
func (d Diagnostic) weight() uint64 {
	// n накапливает логический вес события в байтах с учётом служебных полей и аргументов.
	n := uint64(128+len(d.Kind)) + uint64(len(d.Data)) + uint64(len(d.Message)) + uint64(len(d.SessionID)) + uint64(len(d.GroupID))
	// arg содержит очередной аргумент, чей вес добавляется к событию.
	for _, arg := range d.Args {
		n += uint64(16 + len(arg))
	}
	return n
}

// diagnosticQueue хранит полные события в пределах числа и веса и пробуждает ожидающих читателей.
type diagnosticQueue struct {
	// mu защищает содержимое, счётчики, владельца и сигналы очереди диагностики.
	mu sync.Mutex
	// sessionID содержит идентификатор сессии, приписываемый новым событиям.
	sessionID string
	// groupID содержит идентификатор группы, приписываемый новым событиям.
	groupID string
	// items хранит ещё не выданные события в порядке поступления.
	items []Diagnostic
	// bytes содержит суммарный логический вес ещё не выданных событий в байтах.
	bytes uint64
	// maxEvents задаёт предельное число событий, одновременно хранимых очередью.
	maxEvents int
	// maxBytes задаёт предельный логический вес содержимого очереди в байтах.
	maxBytes uint64
	// dropped считает события, отвергнутые из-за закрытия или заполнения очереди.
	dropped uint64
	// closed запрещает добавление новых событий, сохраняя накопленные для чтения.
	closed bool
	// changed закрывается при изменении очереди и заменяется новым сигналом для следующих ожиданий.
	changed chan struct{}
}

// Создаёт пустую очередь с явными пределами числа событий и логического веса.
func newDiagnosticQueue(events int, maxBytes uint64) *diagnosticQueue {
	return &diagnosticQueue{maxEvents: events, maxBytes: maxBytes, changed: make(chan struct{})}
}

// Пробуждает текущих ожидающих и создаёт новый сигнал изменения; вызывающий удерживает блокировку
// очереди.
func (q *diagnosticQueue) signal() {
	close(q.changed)
	q.changed = make(chan struct{})
}

// Приписывает событию владельца и копирует полное содержимое в пределах бюджета; переполнение или
// закрытие увеличивает потери, не блокируя процесс.
func (q *diagnosticQueue) push(d Diagnostic) {
	q.mu.Lock()
	defer q.mu.Unlock()
	d.SessionID, d.GroupID = q.sessionID, q.groupID
	// weight содержит логический вес добавляемого события для проверки и учёта бюджета.
	weight := d.weight()
	if q.closed || len(q.items) >= q.maxEvents || weight > q.maxBytes-q.bytes {
		q.dropped++
		return
	}
	d.Data = bytes.Clone(d.Data)
	d.Kind = strings.Clone(d.Kind)
	d.Message = strings.Clone(d.Message)
	// args содержит независимые копии аргументов сохраняемого события.
	args := make([]string, len(d.Args))
	// i обозначает позицию аргумента в копии команды; arg содержит исходную строку этого аргумента.
	for i, arg := range d.Args {
		args[i] = strings.Clone(arg)
	}
	d.Args = args
	q.items = append(q.items, d)
	q.bytes += weight
	q.signal()
}

// Ожидает событие с отменой, освобождает его вес при выдаче и возвращает EOF только после дочитывания
// закрытой очереди.
func (q *diagnosticQueue) next(ctx context.Context) (Diagnostic, error) {
	for {
		// err содержит причину отмены ожидания следующего диагностического события.
		if err := ctx.Err(); err != nil {
			return Diagnostic{}, err
		}
		q.mu.Lock()
		if len(q.items) != 0 {
			// d содержит первое накопленное событие, выдаваемое читателю.
			d := q.items[0]
			q.items[0] = Diagnostic{}
			q.items = q.items[1:]
			q.bytes -= d.weight()
			q.mu.Unlock()
			return d, nil
		}
		// closed сохраняет признак закрытия очереди; changed сохраняет сигнал следующего
		// изменения для ожидания вне блокировки.
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

// Единожды закрывает приём новых событий и пробуждает ожидающих, сохраняя уже накопленные события для
// чтения.
func (q *diagnosticQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		q.signal()
	}
}

// Возвращает число отвергнутых событий под блокировкой очереди.
func (q *diagnosticQueue) losses() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

// diagnosticWriter передаёт вывод процесса в диагностику фрагментами и при необходимости в разбор
// progress.
type diagnosticWriter struct {
	// mu упорядочивает разбор и нумерацию фрагментов при записи вывода процесса.
	mu sync.Mutex
	// queue принимает полные фрагменты вывода процесса.
	queue *diagnosticQueue
	// attempt связывает фрагменты с физической попыткой.
	attempt uint64
	// kind указывает, записывается stdout или stderr процесса.
	kind string
	// chunk ограничивает число байтов одного диагностического фрагмента.
	chunk int
	// sequence считает уже созданные фрагменты этого потока вывода.
	sequence uint64
	// progress разбирает отчёты FFmpeg из stdout; для stderr остаётся nil.
	progress *progressParser
}

// Write передаёт байты в разбор progress и разбивает полную диагностику на нумерованные фрагменты;
// переполнение очереди не возвращается как ошибка записи процесса.
func (w *diagnosticWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.progress.write(p)
	// n сохраняет исходный размер записи, возвращаемый после разбиения на фрагменты.
	n := len(p)
	for len(p) != 0 {
		// size содержит число байтов очередного фрагмента с учётом предела chunk.
		size := min(len(p), w.chunk)
		w.sequence++
		w.queue.push(Diagnostic{Time: time.Now(), Attempt: w.attempt, Kind: w.kind,
			Sequence: w.sequence, Data: p[:size]})
		p = p[size:]
	}
	return n, nil
}

// Проверяет непустой исполняемый путь, отсутствие нулевых байтов и общий бюджет байтов команды и
// аргументов до запуска.
func validCommand(binary string, args []string, budget uint64) bool {
	if strings.TrimSpace(binary) == "" || strings.ContainsRune(binary, 0) || uint64(len(binary)) > budget {
		return false
	}
	// used накапливает число байтов исполняемого пути и уже проверенных аргументов команды.
	used := uint64(len(binary))
	// arg содержит очередной аргумент для проверки нулевых байтов и оставшегося бюджета.
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) || uint64(len(arg)) > budget-used {
			return false
		}
		used += uint64(len(arg))
	}
	return true
}

// Сохраняет независимые копии SessionID и GroupID для последующих диагностических событий под
// блокировкой.
func (q *diagnosticQueue) setOwner(sessionID, groupID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sessionID, q.groupID = strings.Clone(sessionID), strings.Clone(groupID)
}
