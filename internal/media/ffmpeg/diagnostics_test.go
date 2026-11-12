package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// Проверяет восстановление исходных бинарных байтов из нумерованных ограниченных фрагментов,
// сохранение отдельных argv и учёт потерь по числу и весу событий.
func TestDiagnosticChunksPreserveBytesAndLossAccounting(t *testing.T) {
	// q хранит тестовую диагностику; после проверки фрагментов заменяется очередью на одно событие
	// для проверки потерь.
	q := newDiagnosticQueue(20, 8192)
	// w разбивает stderr попытки 42 на диагностические фрагменты не более трёх байтов.
	w := &diagnosticWriter{queue: q, attempt: 42, kind: "stderr", chunk: 3}
	// original содержит контрольные бинарные байты, включая ноль и неполные последовательности
	// UTF-8.
	original := []byte{0xff, '\n', 'a', 0xe2, 0x82, 0xac, 0, 0xfe}
	// n содержит число принятых байтов записи; err сообщает об отказе diagnosticWriter.Write.
	if n, err := w.Write(original); err != nil || n != len(original) {
		t.Fatalf("Write: %d %v", n, err)
	}
	q.close()
	// reconstructed накапливает данные прочитанных фрагментов для сравнения с исходными байтами.
	var reconstructed []byte
	// seq содержит ожидаемый следующий порядковый номер диагностического фрагмента.
	var seq uint64
	for {
		// d содержит очередной диагностический фрагмент; err сообщает об отказе чтения или EOF
		// после закрытия очереди.
		d, err := q.next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seq++
		if d.Attempt != 42 || d.Sequence != seq || len(d.Data) > 3 {
			t.Fatalf("событие: %+v", d)
		}
		reconstructed = append(reconstructed, d.Data...)
	}
	if !bytes.Equal(original, reconstructed) || q.bytes != 0 || q.losses() != 0 {
		t.Fatalf("байты: %x, удержано %d", reconstructed, q.bytes)
	}
	q = newDiagnosticQueue(1, 512)
	q.push(Diagnostic{Kind: "args", Args: []string{"a b", "c\nd"}})
	q.push(Diagnostic{Kind: "drop", Data: []byte{1, 2}})
	if q.losses() != 1 {
		t.Fatal(q.losses())
	}
	// d содержит сохранённое событие с отдельными argv; err сообщает об отказе его чтения.
	d, err := q.next(context.Background())
	if err != nil || len(d.Args) != 2 || d.Args[1] != "c\nd" {
		t.Fatalf("argv: %+v %v", d, err)
	}
	q.push(Diagnostic{Kind: "bytes", Data: bytes.Repeat([]byte{1}, 1024)})
	if q.losses() != 2 {
		t.Fatal(q.losses())
	}
}
