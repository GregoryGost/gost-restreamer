package rtmp

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

type wireStream struct {
	known                       bool
	typ                         byte
	length, received, fragments uint32
	extended                    bool
	extendedValue               uint32
	slots                       []uint32
	capture                     []byte
	prefix                      [5]byte
	prefixN                     int
	collect                     bool
	amfOffset                   int
}

// wireConn передаёт библиотеке не более одного проверенного блока RTMP за чтение.
// Последний блок AMF не выдаётся до предварительной проверки всего ограниченного тела.
type wireConn struct {
	net.Conn
	limits          Limits
	handshake       int
	streams         [64]*wireStream
	count           uint32
	chunkSize       uint32
	pending         []byte
	retained        uint64
	base            uint64
	lastConfig      []byte
	startupDeadline time.Time
}

func newWireConn(conn net.Conn, limits Limits) (*wireConn, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	// Резерв включает четыре последних блока библиотеки, временную сборку, Packet
	// и копии конфигураций. Отдельно учитываются удерживаемые фрагменты и capture.
	base := limits.baseRetained()
	if base > limits.MaxRetainedBytes {
		return nil, ErrLimit
	}
	return &wireConn{Conn: conn, limits: limits, handshake: 3073, chunkSize: 128, base: base}, nil
}

func (c *wireConn) Write(p []byte) (int, error) {
	if err := c.SetWriteDeadline(c.deadline(c.limits.WriteTimeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

func (c *wireConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.handshake > 0 {
		n := min(len(p), c.handshake)
		n, err := c.Conn.Read(p[:n])
		if c.handshake == 3073 && n > 0 && p[0] != 3 {
			return 0, ErrFraming
		}
		c.handshake -= n
		return n, err
	}
	if len(c.pending) == 0 {
		if err := c.SetReadDeadline(c.deadline(c.limits.ReadTimeout)); err != nil {
			return 0, err
		}
		if err := c.readChunk(); err != nil {
			return 0, err
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	if len(c.pending) == 0 {
		c.pending = nil
	}
	return n, nil
}

func (c *wireConn) deadline(timeout time.Duration) time.Time {
	d := time.Now().Add(timeout)
	if !c.startupDeadline.IsZero() && c.startupDeadline.Before(d) {
		return c.startupDeadline
	}
	return d
}

func be24(p []byte) uint32 { return uint32(p[0])<<16 | uint32(p[1])<<8 | uint32(p[2]) }

func (c *wireConn) reserve(n uint64) error {
	if n > c.limits.MaxRetainedBytes-c.base-c.retained {
		return ErrLimit
	}
	c.retained += n
	return nil
}

func (c *wireConn) readChunk() error {
	var header [18]byte
	if _, err := io.ReadFull(c.Conn, header[:1]); err != nil {
		return err
	}
	fmtID, id := header[0]>>6, header[0]&63
	if id < 2 {
		return ErrFraming
	}
	s := c.streams[id]
	if s == nil {
		if c.count >= c.limits.MaxChunkStreams {
			return ErrLimit
		}
		if err := c.reserve(512); err != nil {
			return err
		}
		s = &wireStream{}
		c.streams[id] = s
		c.count++
	}
	if (fmtID != 0 && !s.known) || (fmtID != 3 && s.received != 0) {
		return ErrFraming
	}
	hn := 1
	start := s.received == 0
	switch fmtID {
	case 0:
		hn = 12
	case 1:
		hn = 8
	case 2:
		hn = 4
	}
	if _, err := io.ReadFull(c.Conn, header[1:hn]); err != nil {
		return err
	}
	if fmtID <= 1 {
		s.length = be24(header[4:7])
		s.typ = header[7]
	}
	if fmtID <= 2 {
		s.extended = be24(header[1:4]) == 0xffffff
	}
	if s.extended {
		if _, err := io.ReadFull(c.Conn, header[hn:hn+4]); err != nil {
			return err
		}
		v := binary.BigEndian.Uint32(header[hn : hn+4])
		hn += 4
		if fmtID == 3 && v != s.extendedValue {
			return ErrFraming
		}
		s.extendedValue = v
	}
	s.known = true
	if s.length > c.limits.MaxMessageBytes {
		return ErrLimit
	}
	if s.typ <= 6 && s.length > c.limits.MaxControlBytes {
		return ErrLimit
	}
	if (s.typ == 18 || s.typ == 20) && s.length > c.limits.MaxAMFBytes {
		return ErrLimit
	}
	if start {
		s.fragments = 0
		s.prefixN = 0
		s.collect = false
		s.amfOffset = -1
		if s.capture != nil {
			c.retained -= uint64(cap(s.capture))
			s.capture = nil
		}
		if s.typ <= 6 || s.typ == 18 || s.typ == 20 {
			if err := c.beginCapture(s); err != nil {
				return err
			}
			if s.typ == 18 || s.typ == 20 {
				s.amfOffset = 0
			}
		}
	}
	amount := min(s.length-s.received, c.chunkSize)
	if uint64(amount)+uint64(hn) > uint64(c.limits.MaxReadAheadBytes) {
		return ErrLimit
	}
	if s.fragments >= c.limits.MaxFragments {
		return ErrLimit
	}
	// gortmplib сохраняет ячейки массива после завершения и Abort. Перезапись
	// освобождает лишь соответствующую старую ссылку; хвост остаётся учтённым.
	fragmented := s.length > c.chunkSize || s.received != 0
	if fragmented {
		i := int(s.fragments)
		if i < len(s.slots) {
			old := s.slots[i]
			if amount > old {
				if err := c.reserve(uint64(amount - old)); err != nil {
					return err
				}
			} else {
				c.retained -= uint64(old - amount)
			}
			s.slots[i] = amount
		} else {
			// 128 байт покрывают рост массива срезов библиотеки и []uint32 ограничителя.
			if err := c.reserve(uint64(amount) + 128); err != nil {
				return err
			}
			s.slots = append(s.slots, amount)
		}
	}
	var initial [5]byte
	initialN := min(int(amount), 1)
	if s.prefixN >= 5 {
		initialN = 0
	}
	if _, err := io.ReadFull(c.Conn, initial[:initialN]); err != nil {
		return err
	}
	oldPrefix := s.prefixN
	s.prefixN += copy(s.prefix[s.prefixN:], initial[:initialN])
	if err := c.inspectPrefix(s, oldPrefix); err != nil {
		return err
	}
	remainingPrefix := min(int(amount)-initialN, 5-s.prefixN)
	if _, err := io.ReadFull(c.Conn, initial[initialN:initialN+remainingPrefix]); err != nil {
		return err
	}
	oldPrefix = s.prefixN
	s.prefixN += copy(s.prefix[s.prefixN:], initial[initialN:initialN+remainingPrefix])
	initialN += remainingPrefix
	if err := c.inspectPrefix(s, oldPrefix); err != nil {
		return err
	}
	chunk := make([]byte, hn+int(amount))
	copy(chunk, header[:hn])
	copy(chunk[hn:], initial[:initialN])
	if _, err := io.ReadFull(c.Conn, chunk[hn+initialN:]); err != nil {
		return err
	}
	body := chunk[hn:]
	if s.collect {
		copy(s.capture[s.received:], body)
	}
	s.received += amount
	s.fragments++
	if s.received == s.length {
		if s.amfOffset >= 0 {
			if len(s.capture) <= s.amfOffset {
				return ErrFraming
			}
			if err := preflightAMF(s.capture[s.amfOffset:], c.limits); err != nil {
				return err
			}
		}
		if s.typ == 1 {
			if len(s.capture) != 4 {
				return ErrFraming
			}
			size := binary.BigEndian.Uint32(s.capture)
			if size == 0 || size > c.limits.MaxChunkBytes {
				return ErrLimit
			}
			c.chunkSize = size
		}
		if s.typ == 2 {
			if len(s.capture) != 4 {
				return ErrFraming
			}
			target := binary.BigEndian.Uint32(s.capture)
			if target < 2 || target > 63 {
				return ErrFraming
			}
			if t := c.streams[target]; t != nil && t.received > 0 {
				t.received = 0
				t.length = 0
				t.fragments = 0
				if t.capture != nil {
					c.retained -= uint64(cap(t.capture))
					t.capture = nil
				}
			}
		}
		c.lastConfig = nil
		if s.collect && (s.typ == 8 || s.typ == 9) && s.amfOffset < 0 {
			c.lastConfig = s.capture
		}
		s.received = 0
		s.fragments = 0
	}
	c.pending = chunk
	return nil
}

func (c *wireConn) beginCapture(s *wireStream) error {
	if err := c.reserve(uint64(s.length)); err != nil {
		return err
	}
	s.capture = make([]byte, int(s.length))
	s.collect = true
	return nil
}

func (c *wireConn) inspectPrefix(s *wireStream, oldPrefix int) error {
	if s.typ == 9 && s.prefixN >= 1 {
		if s.prefix[0]&0x80 == 0 {
			return fmt.Errorf("%w: требуется Enhanced RTMP AV1", ErrCodec)
		}
		kind := s.prefix[0] & 15
		if kind == 6 {
			return fmt.Errorf("%w: multitrack", ErrCodec)
		}
		if kind == 1 && (s.prefix[0]>>4&7 < 1 || s.prefix[0]>>4&7 > 2) {
			return fmt.Errorf("%w: AV1 FrameType", ErrCodec)
		}
		if s.prefixN == 5 && string(s.prefix[1:5]) != "av01" {
			return ErrCodec
		}
		if !s.collect && (kind == 0 || kind == 4) {
			limit := c.limits.MaxConfigBytes
			if kind == 4 {
				limit = c.limits.MaxAMFBytes
			}
			if s.length > limit {
				return ErrLimit
			}
			if err := c.beginCapture(s); err != nil {
				return err
			}
			if kind == 4 {
				s.amfOffset = 5
			}
		}
	}
	if s.typ == 8 && s.prefixN >= 1 {
		codec := s.prefix[0] >> 4
		if codec != 10 && codec != 9 {
			return ErrCodec
		}
		if codec == 9 {
			kind := s.prefix[0] & 15
			if kind != 0 && kind != 1 && kind != 2 {
				return ErrCodec
			}
			if s.prefixN == 5 && string(s.prefix[1:5]) != "mp4a" {
				return ErrCodec
			}
		}
		if !s.collect && s.prefixN >= 2 {
			config := (codec == 10 && s.prefix[1] == 0) || (codec == 9 && s.prefix[0]&15 == 0)
			if config {
				if s.length > c.limits.MaxConfigBytes {
					return ErrLimit
				}
				if err := c.beginCapture(s); err != nil {
					return err
				}
			}
		}
	}
	if s.collect && s.received > 0 && oldPrefix > 0 {
		copy(s.capture, s.prefix[:oldPrefix])
	}
	return nil
}
