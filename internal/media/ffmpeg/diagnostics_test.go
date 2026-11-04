package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestDiagnosticChunksPreserveBytesAndLossAccounting(t *testing.T) {
	q := newDiagnosticQueue(20, 8192)
	w := &diagnosticWriter{queue: q, attempt: 42, kind: "stderr", chunk: 3}
	original := []byte{0xff, '\n', 'a', 0xe2, 0x82, 0xac, 0, 0xfe}
	if n, err := w.Write(original); err != nil || n != len(original) {
		t.Fatalf("Write: %d %v", n, err)
	}
	q.close()
	var reconstructed []byte
	var seq uint64
	for {
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
	d, err := q.next(context.Background())
	if err != nil || len(d.Args) != 2 || d.Args[1] != "c\nd" {
		t.Fatalf("argv: %+v %v", d, err)
	}
	q.push(Diagnostic{Kind: "bytes", Data: bytes.Repeat([]byte{1}, 1024)})
	if q.losses() != 2 {
		t.Fatal(q.losses())
	}
}
