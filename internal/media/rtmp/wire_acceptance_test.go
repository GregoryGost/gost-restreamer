package rtmp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func acceptanceWireLimits() Limits {
	return Limits{MaxConnections: 4, MaxChunkStreams: 8, MaxMessageBytes: 4096, MaxChunkBytes: 256, MaxFragments: 128, MaxRetainedBytes: 1 << 20, MaxControlBytes: 64, MaxConfigBytes: 1024, MaxAMFBytes: 1024, MaxAMFDepth: 4, MaxAMFElements: 32, MaxReadAheadBytes: 512, HandshakeTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, ShutdownTimeout: time.Second}
}

type acceptanceWireConn struct {
	*bytes.Reader
	readBytes int
}

func (c *acceptanceWireConn) Read(b []byte) (int, error) {
	n, err := c.Reader.Read(b)
	c.readBytes += n
	return n, err
}
func (*acceptanceWireConn) Write(b []byte) (int, error)      { return len(b), nil }
func (*acceptanceWireConn) Close() error                     { return nil }
func (*acceptanceWireConn) LocalAddr() net.Addr              { return nil }
func (*acceptanceWireConn) RemoteAddr() net.Addr             { return nil }
func (*acceptanceWireConn) SetDeadline(time.Time) error      { return nil }
func (*acceptanceWireConn) SetReadDeadline(time.Time) error  { return nil }
func (*acceptanceWireConn) SetWriteDeadline(time.Time) error { return nil }

func acceptanceChunk0(id, typ byte, body []byte) []byte {
	header := []byte{id, 0, 0, 0, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body)), typ, 1, 0, 0, 0}
	return append(header, body...)
}

func acceptanceWire(t *testing.T, data []byte, limits Limits) (*wireConn, *acceptanceWireConn) {
	t.Helper()
	conn := &acceptanceWireConn{Reader: bytes.NewReader(data)}
	w, err := newWireConn(conn, limits)
	if err != nil {
		t.Fatal(err)
	}
	w.handshake = 0
	return w, conn
}

func TestAcceptanceWireOversizeStopsBeforeReadingBody(t *testing.T) {
	limits := acceptanceWireLimits()
	for _, tc := range []struct {
		name   string
		typ    byte
		length uint32
	}{
		{"message", 9, limits.MaxMessageBytes + 1},
		{"command", 20, limits.MaxAMFBytes + 1},
		{"data", 18, limits.MaxAMFBytes + 1},
		{"control", 4, limits.MaxControlBytes + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := acceptanceChunk0(3, tc.typ, nil)
			wire[4], wire[5], wire[6] = byte(tc.length>>16), byte(tc.length>>8), byte(tc.length)
			w, c := acceptanceWire(t, wire, limits)
			got, err := io.ReadAll(w)
			if !errors.Is(err, ErrLimit) || len(got) != 0 || c.readBytes != 12 {
				t.Fatalf("отказ не предшествовал body: bytes=%d exposed=%d err=%v", c.readBytes, len(got), err)
			}
		})
	}
}

func TestAcceptanceWireAMFPreflightCoversEveryTypedAMFPath(t *testing.T) {
	// Пять вложенных массивов AMF0 Strict Array превышают MaxAMFDepth=4 до декодирования библиотекой.
	nested := []byte{5}
	for range 5 {
		nested = append([]byte{10, 0, 0, 0, 1}, nested...)
	}
	for _, tc := range []struct {
		name   string
		typ    byte
		prefix []byte
	}{
		{"command", 20, nil}, {"data", 18, nil}, {"video-metadata", 9, []byte{0x84, 'a', 'v', '0', '1'}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := append(bytes.Clone(tc.prefix), nested...)
			w, _ := acceptanceWire(t, acceptanceChunk0(3, tc.typ, body), acceptanceWireLimits())
			got, err := io.ReadAll(w)
			if !errors.Is(err, ErrLimit) || len(got) != 0 {
				t.Fatalf("опасный последний фрагмент передан декодеру: exposed=%d err=%v", len(got), err)
			}
		})
	}
}

func TestAcceptanceWireLegalTinyChunksAndRetainedSlots(t *testing.T) {
	limits := acceptanceWireLimits()
	limits.MaxFragments = 8
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], 1)
	data := acceptanceChunk0(2, 1, size[:])
	first := []byte{0x91, 'a', 'v', '0', '1', 0x32, 0}
	for range 300 {
		data = append(data, acceptanceChunk0(3, 9, first[:1])...)
		// Длина сообщения, в отличие от первого фрагмента, включает все семь байтов.
		data[len(data)-9], data[len(data)-8], data[len(data)-7] = 0, 0, byte(len(first))
		for _, b := range first[1:] {
			data = append(data, 0xc3, b)
		}
	}
	w, _ := acceptanceWire(t, data, limits)
	got, err := io.ReadAll(w)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("легальный размер фрагмента 1 или повторное использование слотов отвергнуты: %v", err)
	}
	if w.retained > 8192 {
		t.Fatalf("учёт удержанных слотов растёт с общим трафиком: %d", w.retained)
	}
}

func TestAcceptanceWireFragmentLimitPrecedesNextPayload(t *testing.T) {
	limits := acceptanceWireLimits()
	limits.MaxFragments = 2
	data := acceptanceChunk0(2, 1, []byte{0, 0, 0, 1})
	fragment := acceptanceChunk0(3, 9, []byte{0x91})
	fragment[6] = 4
	data = append(data, fragment...)
	data = append(data, 0xc3, 'a', 0xc3, 'v')
	w, c := acceptanceWire(t, data, limits)
	_, err := io.ReadAll(w)
	if !errors.Is(err, ErrLimit) || c.readBytes != len(data)-1 {
		t.Fatalf("третий фрагмент прочитан после исчерпания бюджета: read=%d size=%d err=%v", c.readBytes, len(data), err)
	}
}

func TestAcceptanceWireAV1FrameTypeIsIndependentOfPacketType(t *testing.T) {
	for _, flag := range []byte{0x81, 0xb1, 0x91, 0xa1} {
		w, _ := acceptanceWire(t, acceptanceChunk0(3, 9, []byte{flag, 'a', 'v', '0', '1', 0x32, 0}), acceptanceWireLimits())
		_, err := io.ReadAll(w)
		if flag == 0x91 || flag == 0xa1 {
			if err != nil {
				t.Errorf("допустимый FrameType %x отвергнут: %v", flag, err)
			}
		} else if !errors.Is(err, ErrCodec) {
			t.Errorf("недопустимый FrameType %x принят: %v", flag, err)
		}
	}
}
