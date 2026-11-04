package fmp4

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

var be = binary.BigEndian

func u32(v uint32) []byte { b := make([]byte, 4); be.PutUint32(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); be.PutUint64(b, v); return b }
func join(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	b := make([]byte, 0, n)
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}
func box(kind string, parts ...[]byte) []byte {
	data := join(parts...)
	return join(u32(uint32(len(data)+8)), []byte(kind), data)
}
func full(kind string, version byte, flags uint32, parts ...[]byte) []byte {
	return box(kind, append([]byte{version}, u32(flags)[1:]...), join(parts...))
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if n < 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

type rawBox struct {
	kind         string
	data         []byte
	size, header uint64
}

func readHeader(r io.Reader) (rawBox, error) {
	var h [8]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return rawBox{}, err
	}
	b := rawBox{kind: string(h[4:]), size: uint64(be.Uint32(h[:4])), header: 8}
	if b.size == 1 {
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return rawBox{}, err
		}
		b.size = be.Uint64(ext[:])
		b.header = 16
	}
	if b.size < b.header {
		return rawBox{}, fmt.Errorf("%w: размер box %s", ErrInvalid, b.kind)
	}
	return b, nil
}
func readBody(r io.Reader, b rawBox, limit uint64) (rawBox, error) {
	if b.size > limit || b.size > math.MaxInt32 {
		return rawBox{}, fmt.Errorf("%w: box %s (%d)", ErrLimit, b.kind, b.size)
	}
	b.data = make([]byte, int(b.size-b.header))
	_, err := io.ReadFull(r, b.data)
	return b, err
}
func children(data []byte) ([]rawBox, error) {
	var result []rawBox
	for len(data) > 0 {
		if len(data) < 8 {
			return nil, ErrInvalid
		}
		size := uint64(be.Uint32(data[:4]))
		header := uint64(8)
		if size == 1 {
			if len(data) < 16 {
				return nil, ErrInvalid
			}
			size = be.Uint64(data[8:16])
			header = 16
		}
		if size < header || size > uint64(len(data)) {
			return nil, ErrInvalid
		}
		result = append(result, rawBox{kind: string(data[4:8]), data: data[header:size], size: size, header: header})
		data = data[size:]
	}
	return result, nil
}
func find(data []byte, kind string) (rawBox, error) {
	boxes, err := children(data)
	if err != nil {
		return rawBox{}, err
	}
	var result rawBox
	found := false
	for _, b := range boxes {
		if b.kind == kind {
			if found {
				return rawBox{}, fmt.Errorf("%w: повторный %s", ErrInvalid, kind)
			}
			result, found = b, true
		}
	}
	if found {
		return result, nil
	}
	return rawBox{}, fmt.Errorf("%w: отсутствует %s", ErrInvalid, kind)
}
func flags(data []byte) (uint32, error) {
	if len(data) < 4 {
		return 0, ErrInvalid
	}
	return be.Uint32(data[:4]) & 0xffffff, nil
}

type cursor struct {
	b   []byte
	err error
}

func (c *cursor) take(n int) []byte {
	if c.err != nil || n < 0 || n > len(c.b) {
		c.err = ErrInvalid
		return make([]byte, n)
	}
	v := c.b[:n]
	c.b = c.b[n:]
	return v
}
func (c *cursor) u32() uint32 { return be.Uint32(c.take(4)) }
func (c *cursor) u64() uint64 { return be.Uint64(c.take(8)) }
