package rtmp

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/bluenviron/gortmplib/pkg/rawmessage"
)

type outboundChunkStream struct {
	known, active, extended               bool
	typ                                   byte
	length, received, fragments, streamID uint32
	timestamp, delta, extendedValue       uint32
	deadline                              time.Time
	body                                  []byte
}

// outboundReader принадлежит одной горутине. Завершённое тело передаётся владельцу
// сообщения; незавершённые тела учитываются до выделения памяти и освобождаются при Abort.
type outboundReader struct {
	c                       *OutboundConn
	streams                 map[uint32]*outboundChunkStream
	chunkSize               uint32
	retained                uint64
	readBytes, acknowledged uint64
	window                  uint32
	messageChunkStreamID    uint32
}

func (r *outboundReader) earliest(candidate time.Time) time.Time {
	d := candidate
	if n := r.c.startup.Load(); n != 0 {
		d = earlierDeadline(d, time.Unix(0, n))
	}
	for _, s := range r.streams {
		if s.active {
			d = earlierDeadline(d, s.deadline)
		}
	}
	return d
}

func earlierDeadline(a, b time.Time) time.Time {
	if a.IsZero() || !b.IsZero() && b.Before(a) {
		return b
	}
	return a
}

func (r *outboundReader) read(p []byte) error {
	for len(p) > 0 {
		n, err := r.c.conn.Read(p)
		if n < 0 || n > len(p) {
			return fmt.Errorf("некорректный результат чтения: phase=read received=%d requested=%d: %w", n, len(p), ErrFraming)
		}
		if uint64(n) > math.MaxUint64-r.readBytes {
			return fmt.Errorf("переполнение счётчика чтения: phase=read received=%d read_bytes=%d: %w", n, r.readBytes, ErrLimit)
		}
		r.readBytes += uint64(n)
		p = p[n:]
		if ackErr := r.ack(); ackErr != nil {
			return ackErr
		}
		if err != nil {
			if len(p) == 0 {
				return nil
			}
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func (r *outboundReader) next() (*rawmessage.Message, error) {
	for {
		m, err := r.chunk()
		if err != nil {
			return nil, err
		}
		if err = r.ack(); err != nil {
			return nil, err
		}
		if m != nil {
			return m, nil
		}
	}
}

func (r *outboundReader) ack() error {
	if r.window == 0 || r.readBytes-r.acknowledged < uint64(r.window) {
		return nil
	}
	var body [4]byte
	binary.BigEndian.PutUint32(body[:], uint32(r.readBytes%(uint64(1)<<32)))
	if err := r.c.queueControl(3, body[:]); err != nil {
		return err
	}
	r.acknowledged = r.readBytes
	return nil
}

func (r *outboundReader) chunk() (*rawmessage.Message, error) {
	l := r.c.opts.Limits
	// При отсутствии незавершённых сообщений тишина не ограничивается сроком чтения.
	if err := r.c.conn.SetReadDeadline(r.earliest(time.Time{})); err != nil {
		return nil, err
	}
	var header [18]byte
	if err := r.read(header[:1]); err != nil {
		return nil, err
	}
	started := time.Now().Add(l.MessageTimeout)
	if err := r.c.conn.SetReadDeadline(r.earliest(started)); err != nil {
		return nil, err
	}
	fmtID, id := header[0]>>6, uint32(header[0]&63)
	switch id {
	case 0:
		if err := r.read(header[1:2]); err != nil {
			return nil, err
		}
		id = 64 + uint32(header[1])
	case 1:
		if err := r.read(header[1:3]); err != nil {
			return nil, err
		}
		id = 64 + uint32(header[1]) + 256*uint32(header[2])
	}
	s := r.streams[id]
	if s == nil {
		if uint32(len(r.streams)) >= l.MaxChunkStreams {
			return nil, fmt.Errorf("превышено число потоков чанков: phase=chunk_header check=stream_limit fmt=%d csid=%d streams=%d max_chunk_streams=%d: %w", fmtID, id, len(r.streams), l.MaxChunkStreams, ErrLimit)
		}
		s = &outboundChunkStream{}
		r.streams[id] = s
	}
	if fmtID != 0 && !s.known || fmtID != 3 && s.active {
		return nil, fmt.Errorf("недопустимая история чанка: phase=chunk_header check=history fmt=%d csid=%d known=%t active=%t received=%d message_len=%d read_bytes=%d: %w", fmtID, id, s.known, s.active, s.received, s.length, r.readBytes, ErrFraming)
	}
	newMessage := !s.active
	n := [...]int{11, 7, 3, 0}[fmtID]
	if err := r.read(header[:n]); err != nil {
		return nil, err
	}
	if fmtID <= 1 {
		s.length = be24(header[3:6])
		s.typ = header[6]
	}
	if fmtID == 0 {
		s.streamID = binary.LittleEndian.Uint32(header[7:11])
	}
	var value uint32
	if fmtID <= 2 {
		value = be24(header[:3])
		s.extended = value == 0xffffff
	}
	if s.extended {
		if err := r.read(header[11:15]); err != nil {
			return nil, err
		}
		value = binary.BigEndian.Uint32(header[11:15])
		if fmtID == 3 && value != s.extendedValue {
			return nil, fmt.Errorf("не совпала расширенная временная метка: phase=extended_timestamp check=timestamp_match fmt=%d csid=%d active=%t extended_timestamp=%d expected_extended_timestamp=%d received=%d message_len=%d read_bytes=%d: %w", fmtID, id, s.active, value, s.extendedValue, s.received, s.length, r.readBytes, ErrFraming)
		}
	}
	if fmtID <= 2 {
		s.extendedValue = value
		s.delta = value
		if fmtID == 0 {
			s.timestamp = value
		} else {
			s.timestamp += value
		}
	} else if newMessage {
		s.timestamp += s.delta
	}
	s.known = true
	if newMessage {
		if s.length > l.MaxMessageBytes || s.typ <= 6 && s.length > l.MaxControlBytes || (s.typ == 18 || s.typ == 20 || s.typ == 17 || s.typ == 15) && s.length > l.MaxAMFBytes {
			return nil, fmt.Errorf("превышен размер сообщения: phase=chunk_message check=message_limit fmt=%d csid=%d type=%d stream_id=%d message_len=%d max_message_bytes=%d max_control_bytes=%d max_amf_bytes=%d: %w", fmtID, id, s.typ, s.streamID, s.length, l.MaxMessageBytes, l.MaxControlBytes, l.MaxAMFBytes, ErrLimit)
		}
		if uint64(s.length) > l.MaxRetainedBytes-l.baseBytes()-r.c.target.retainedBytes()-r.retained {
			return nil, fmt.Errorf("превышен резерв чтения: phase=chunk_message check=retained_limit csid=%d message_len=%d retained_bytes=%d base_bytes=%d target_bytes=%d max_retained_bytes=%d: %w", id, s.length, r.retained, l.baseBytes(), r.c.target.retainedBytes(), l.MaxRetainedBytes, ErrLimit)
		}
		r.retained += uint64(s.length)
		r.c.setReadBuffer(r.retained)
		s.body = make([]byte, int(s.length))
		s.active = true
		s.received = 0
		s.fragments = 0
		s.deadline = started
	}
	if s.fragments >= l.MaxFragments {
		return nil, fmt.Errorf("превышено число фрагментов: phase=chunk_body check=fragment_limit csid=%d fragments=%d max_fragments=%d received=%d message_len=%d: %w", id, s.fragments, l.MaxFragments, s.received, s.length, ErrLimit)
	}
	s.fragments++
	amount := min(s.length-s.received, r.chunkSize)
	if err := r.read(s.body[s.received : s.received+amount]); err != nil {
		return nil, err
	}
	s.received += amount
	if s.received != s.length {
		return nil, nil
	}
	m := &rawmessage.Message{Type: s.typ, MessageStreamID: s.streamID, Timestamp: time.Duration(s.timestamp) * time.Millisecond, Body: s.body}
	// В публичном rawmessage.Message поле ChunkStreamID имеет тип byte.
	// Проверка управляющих сообщений использует полный идентификатор; ограниченное представление наружу не передаётся.
	if id <= 255 {
		m.ChunkStreamID = byte(id)
	}
	r.retained -= uint64(len(s.body))
	s.body = nil
	s.active = false
	r.c.setReadBuffer(r.retained)
	r.messageChunkStreamID = id
	return m, nil
}

func putOutbound24(dst []byte, v uint32) {
	dst[0] = byte(v >> 16)
	dst[1] = byte(v >> 8)
	dst[2] = byte(v)
}

// writeFramed пишет fmt0 и fmt3 без SDK Chunk3.Marshal: расширенная временная метка
// повторяется в каждом продолжении, MessageStreamID всегда имеет порядок little-endian.
func (c *OutboundConn) writeFramed(csid, typ byte, streamID, timestamp uint32, body []byte) error {
	if csid < 2 || csid > 63 || uint64(len(body)) > uint64(c.opts.Limits.MaxMessageBytes) || len(body) > 0xffffff {
		return outboundError(OutboundProtocol, ErrLimit)
	}
	var h [16]byte
	h[0] = csid
	putOutbound24(h[1:4], min(timestamp, uint32(0xffffff)))
	putOutbound24(h[4:7], uint32(len(body)))
	h[7] = typ
	binary.LittleEndian.PutUint32(h[8:12], streamID)
	hn := 12
	if timestamp >= 0xffffff {
		binary.BigEndian.PutUint32(h[12:16], timestamp)
		hn = 16
	}
	if err := c.writeAll(h[:hn]); err != nil {
		return err
	}
	for {
		n := min(len(body), int(c.outboundChunkSize))
		if err := c.writeAll(body[:n]); err != nil {
			return err
		}
		body = body[n:]
		if len(body) == 0 {
			return nil
		}
		h[0] = 0xc0 | csid
		hn = 1
		if timestamp >= 0xffffff {
			binary.BigEndian.PutUint32(h[1:5], timestamp)
			hn = 5
		}
		if err := c.writeAll(h[:hn]); err != nil {
			return err
		}
	}
}
