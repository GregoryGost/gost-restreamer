package rtmp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/bluenviron/gortmplib/pkg/bytecounter"
	"github.com/bluenviron/gortmplib/pkg/message"
)

func testLimits() Limits {
	return Limits{MaxConnections: 4, MaxChunkStreams: 8, MaxMessageBytes: 1 << 20, MaxChunkBytes: 65536, MaxFragments: 20000, MaxRetainedBytes: 32 << 20, MaxControlBytes: 64, MaxConfigBytes: 1 << 16, MaxAMFBytes: 1 << 16, MaxAMFDepth: 12, MaxAMFElements: 1024, MaxReadAheadBytes: 65554, HandshakeTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, ShutdownTimeout: 2 * time.Second}
}

type memoryConn struct{ *bytes.Reader }

func (c memoryConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c memoryConn) Close() error                     { return nil }
func (c memoryConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c memoryConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c memoryConn) SetDeadline(time.Time) error      { return nil }
func (c memoryConn) SetReadDeadline(time.Time) error  { return nil }
func (c memoryConn) SetWriteDeadline(time.Time) error { return nil }

func chunk0(id, typ byte, body []byte) []byte {
	h := make([]byte, 12)
	h[0] = id
	h[4] = byte(len(body) >> 16)
	h[5] = byte(len(body) >> 8)
	h[6] = byte(len(body))
	h[7] = typ
	h[8] = 1
	return append(h, body...)
}
func fragmented(id, typ byte, body []byte, size int) []byte {
	first := min(size, len(body))
	out := chunk0(id, typ, body[:first])
	out[4] = byte(len(body) >> 16)
	out[5] = byte(len(body) >> 8)
	out[6] = byte(len(body))
	for at := first; at < len(body); at += size {
		out = append(out, 0xc0|id)
		out = append(out, body[at:min(at+size, len(body))]...)
	}
	return out
}

func guarded(t *testing.T, data []byte, limits Limits) *wireConn {
	t.Helper()
	c, err := newWireConn(memoryConn{bytes.NewReader(data)}, limits)
	if err != nil {
		t.Fatal(err)
	}
	c.handshake = 0
	return c
}

func TestGuardRejectsDeclaredSizeBeforeBodyRead(t *testing.T) {
	l := testLimits()
	h := chunk0(3, 20, nil)
	n := l.MaxAMFBytes + 1
	h[4] = byte(n >> 16)
	h[5] = byte(n >> 8)
	h[6] = byte(n)
	c := guarded(t, h, l)
	_, err := io.ReadAll(c)
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("размер не отклонён до EOF: %v", err)
	}
	if c.retained > 512 {
		t.Fatal("тело выделено до проверки")
	}
	// Тип метаданных выясняется из первого байта до выделения полного блока.
	h = chunk0(6, 9, []byte{0x84})
	h[4] = byte(n >> 16)
	h[5] = byte(n >> 8)
	h[6] = byte(n)
	c = guarded(t, h, l)
	c.chunkSize = n
	_, err = io.ReadAll(c)
	if !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}

func TestGuardPreflightsEveryAMFDecoderBeforeFinalChunk(t *testing.T) {
	l := testLimits()
	l.MaxAMFDepth = 2
	deep := []byte{10, 0, 0, 0, 1, 10, 0, 0, 0, 1, 5}
	for _, typ := range []byte{18, 20, 9} {
		body := deep
		if typ == 9 {
			body = append([]byte{0x84, 'a', 'v', '0', '1'}, deep...)
		}
		data := append(chunk0(2, 1, []byte{0, 0, 0, 3}), fragmented(6, typ, body, 3)...)
		c := guarded(t, data, l)
		bcr := bytecounter.NewReader(c)
		reader := message.NewReader(bcr, bcr, func(uint32) error { return nil })
		if _, err := reader.Read(); err != nil {
			t.Fatal(err)
		}
		if _, err := reader.Read(); !errors.Is(err, ErrLimit) {
			t.Fatalf("тип %d не остановлен до typed AMF: %v", typ, err)
		}
	}
}

func TestGuardRetainsFragmentSlotsAndReusesBudget(t *testing.T) {
	l := testLimits()
	body := []byte{0xaf, 1, 1, 2, 3, 4, 5, 6}
	var data []byte
	for range 3000 {
		data = append(data, fragmented(4, 8, body, 1)...)
	}
	c := guarded(t, data, l)
	c.chunkSize = 1
	for range 8 {
		if err := c.readChunk(); err != nil {
			t.Fatal(err)
		}
	}
	retained := c.retained
	if retained < 8*129 {
		t.Fatal("слоты освобождены после completion")
	}
	for range 2999 * 8 {
		if err := c.readChunk(); err != nil {
			t.Fatal(err)
		}
	}
	if c.retained != retained {
		t.Fatalf("обычный поток навсегда расходует бюджет: %d != %d", c.retained, retained)
	}
}

func TestGuardChunkInheritanceAbortAndExtendedTimestamp(t *testing.T) {
	set := func(n uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, n); return chunk0(2, 1, b) }
	if _, err := io.ReadAll(guarded(t, set(0), testLimits())); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	for _, id := range []byte{0, 1} {
		if _, err := io.ReadAll(guarded(t, []byte{id}, testLimits())); !errors.Is(err, ErrFraming) {
			t.Fatal(err)
		}
	}
	if _, err := io.ReadAll(guarded(t, []byte{0xc4}, testLimits())); !errors.Is(err, ErrFraming) {
		t.Fatal(err)
	}
	data := chunk0(4, 8, []byte{0xaf, 1, 1})
	data[1] = 255
	data[2] = 255
	data[3] = 255
	data = append(append(append([]byte(nil), data[:12]...), 0, 255, 255, 255), data[12:]...)
	data = append(data, 0xc4, 0, 255, 255, 255, 0xaf, 1, 2)
	if _, err := io.ReadAll(guarded(t, data, testLimits())); err != nil {
		t.Fatal(err)
	}
	// Abort очищает текущее тело, но не ссылки в ячейках его внутреннего массива.
	partial := fragmented(4, 8, []byte{0xaf, 1, 1, 2}, 1)[:13]
	abort := fragmented(2, 2, []byte{0, 0, 0, 4}, 1)
	data = append(partial, abort...)
	data = append(data, fragmented(4, 8, []byte{0xaf, 1, 9}, 1)...)
	c := guarded(t, data, testLimits())
	c.chunkSize = 1
	if _, err := io.ReadAll(c); err != nil {
		t.Fatal(err)
	}
	if c.streams[4].length != 3 || len(c.streams[4].slots) < 3 {
		t.Fatal("Abort нарушил последующую сборку")
	}
}

func TestGuardLimitsFragmentsAndStreams(t *testing.T) {
	l := testLimits()
	l.MaxFragments = 2
	c := guarded(t, fragmented(4, 8, []byte{0xaf, 1, 1}, 1), l)
	c.chunkSize = 1
	if _, err := io.ReadAll(c); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	l = testLimits()
	l.MaxChunkStreams = 1
	data := append(chunk0(4, 8, []byte{0xaf, 1, 1}), chunk0(5, 8, []byte{0xaf, 1, 2})...)
	if _, err := io.ReadAll(guarded(t, data, l)); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}

func TestGuardMirrorsInheritedHeaderTimestamps(t *testing.T) {
	body := []byte{0xaf, 1, 7}
	data := chunk0(4, 8, body)
	data[3] = 10
	data = append(data, 0x44, 0, 0, 5, 0, 0, 3, 8)
	data = append(data, body...)
	data = append(data, 0x84, 0, 0, 2)
	data = append(data, body...)
	data = append(data, 0xc4)
	data = append(data, body...)
	c := guarded(t, data, testLimits())
	bcr := bytecounter.NewReader(c)
	r := message.NewReader(bcr, bcr, func(uint32) error { return nil })
	for _, want := range []time.Duration{10, 15, 17, 19} {
		m, err := r.Read()
		if err != nil {
			t.Fatal(err)
		}
		if got := m.(*message.Audio).DTS; got != want*time.Millisecond {
			t.Fatalf("наследование timestamp: %v", got)
		}
	}
}

func TestRetainedBudgetRejectsBeforeFragmentAllocation(t *testing.T) {
	l := testLimits()
	l.MaxRetainedBytes = l.baseRetained() + 512 + 128
	c := guarded(t, fragmented(4, 8, []byte{0xaf, 1, 1}, 1), l)
	c.chunkSize = 1
	if _, err := io.ReadAll(c); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if len(c.streams[4].slots) != 0 {
		t.Fatal("слот создан до проверки retained budget")
	}
}

func FuzzGuardAMF(f *testing.F) {
	f.Add([]byte{5})
	f.Add([]byte{10, 0, 0, 0, 1, 5})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 1<<16 {
			t.Skip()
		}
		_ = preflightAMF(b, testLimits())
	})
}

func FuzzGuardFraming(f *testing.F) {
	f.Add(chunk0(4, 8, []byte{0xaf, 1, 1}))
	f.Add([]byte{0xc4})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 1<<16 {
			t.Skip()
		}
		c, err := newWireConn(memoryConn{bytes.NewReader(b)}, testLimits())
		if err != nil {
			t.Fatal(err)
		}
		c.handshake = 0
		_, _ = io.Copy(io.Discard, c)
	})
}
