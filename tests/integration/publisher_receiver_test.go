package integration

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Приёмник независимо разбирает байты RTMP; код медиапарсера SDK здесь не используется.
type publisherWireMessage struct {
	connection        uint64
	typ               byte
	stream, timestamp uint32
	body              []byte
}

type publisherReceiver struct {
	listener    net.Listener
	mu          sync.Mutex
	connections map[net.Conn]struct{}
	messages    []publisherWireMessage
	apps, keys  []string
	changed     chan struct{}
	workers     sync.WaitGroup
	accepted    atomic.Uint64
	bytes       atomic.Uint64
	err         error
	hold        <-chan struct{}
	stop        chan struct{}
}

func newPublisherReceiver(t *testing.T, secure bool, hold ...<-chan struct{}) (*publisherReceiver, *tls.Config) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var trust *tls.Config
	if secure {
		key, e := rsa.GenerateKey(rand.Reader, 2048)
		if e != nil {
			t.Fatal(e)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, e := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if e != nil {
			t.Fatal(e)
		}
		cert, e := x509.ParseCertificate(der)
		if e != nil {
			t.Fatal(e)
		}
		pool := x509.NewCertPool()
		pool.AddCert(cert)
		trust = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		l = tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12})
	}
	r := &publisherReceiver{listener: l, connections: make(map[net.Conn]struct{}), changed: make(chan struct{}), stop: make(chan struct{})}
	if len(hold) > 0 {
		r.hold = hold[0]
	}
	r.workers.Add(1)
	go func() {
		defer r.workers.Done()
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			r.mu.Lock()
			r.connections[c] = struct{}{}
			r.mu.Unlock()
			id := r.accepted.Add(1)
			r.workers.Add(1)
			go func() {
				defer r.workers.Done()
				defer func() { _ = c.Close(); r.mu.Lock(); delete(r.connections, c); r.notifyLocked(); r.mu.Unlock() }()
				if e := r.serve(c, id); e != nil && !errors.Is(e, io.EOF) && !errors.Is(e, net.ErrClosed) {
					r.mu.Lock()
					if r.err == nil {
						r.err = e
					}
					r.notifyLocked()
					r.mu.Unlock()
				}
			}()
		}
	}()
	t.Cleanup(func() {
		close(r.stop)
		_ = l.Close()
		r.mu.Lock()
		for c := range r.connections {
			_ = c.Close()
		}
		r.mu.Unlock()
		done := make(chan struct{})
		go func() { r.workers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("локальный RTMP-приёмник не завершился")
		}
	})
	return r, trust
}

func (r *publisherReceiver) notifyLocked() { close(r.changed); r.changed = make(chan struct{}) }

func (r *publisherReceiver) waitClosed(t *testing.T) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		r.mu.Lock()
		remaining, changed := len(r.connections), r.changed
		r.mu.Unlock()
		if remaining == 0 {
			return
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatal("приёмник не дочитал соединение до EOF")
		}
	}
}
func (r *publisherReceiver) snapshot() []publisherWireMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]publisherWireMessage(nil), r.messages...)
}
func (r *publisherReceiver) disconnect() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for c := range r.connections {
		_ = c.Close()
	}
}
func (r *publisherReceiver) wait(t *testing.T, predicate func([]publisherWireMessage) bool) []publisherWireMessage {
	t.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		r.mu.Lock()
		messages := append([]publisherWireMessage(nil), r.messages...)
		changed, err := r.changed, r.err
		r.mu.Unlock()
		if predicate(messages) {
			return messages
		}
		if err != nil {
			t.Fatalf("независимый RTMP-приёмник: %v", err)
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatalf("RTMP-условие не выполнено: messages=%d connections=%d", len(messages), r.accepted.Load())
		}
	}
}

type publisherCountReader struct {
	io.Reader
	total *atomic.Uint64
}

func (r publisherCountReader) Read(p []byte) (int, error) {
	n, e := r.Reader.Read(p)
	r.total.Add(uint64(n))
	return n, e
}

func (r *publisherReceiver) serve(conn net.Conn, id uint64) error {
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	reader := publisherCountReader{conn, &r.bytes}
	c1 := make([]byte, 1537)
	if _, err := io.ReadFull(reader, c1); err != nil {
		return err
	}
	if c1[0] != 3 {
		return errors.New("неверная версия RTMP")
	}
	server := make([]byte, 3073)
	server[0] = 3
	copy(server[1537:], c1[1:])
	if _, err := io.Copy(conn, bytes.NewReader(server)); err != nil {
		return err
	}
	if _, err := io.ReadFull(reader, make([]byte, 1536)); err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return err
	}
	parser := publisherChunkReader{reader: reader, chunk: 128, streams: make(map[uint32]*publisherChunkState)}
	for {
		m, err := parser.next()
		if err != nil {
			return err
		}
		m.connection = id
		if m.typ == 1 {
			if len(m.body) != 4 {
				return errors.New("неверный SetChunkSize")
			}
			parser.chunk = binary.BigEndian.Uint32(m.body)
			if parser.chunk == 0 || parser.chunk > 1<<20 {
				return errors.New("неверный размер chunk")
			}
			continue
		}
		if m.typ == 20 {
			values, e := publisherAMFValues(m.body)
			if e != nil {
				return e
			}
			if len(values) < 2 {
				return errors.New("неполная команда AMF")
			}
			name, _ := values[0].(string)
			tx, _ := values[1].(float64)
			var reply []any
			switch name {
			case "connect":
				if len(values) < 3 {
					return errors.New("нет connect object")
				}
				object, ok := values[2].(map[string]any)
				if !ok {
					return errors.New("неверный connect object")
				}
				app, _ := object["app"].(string)
				r.mu.Lock()
				r.apps = append(r.apps, app)
				r.mu.Unlock()
				reply = []any{"_result", tx, map[string]any{"fmsVer": "FMS/3,0,1,123", "capabilities": float64(31)}, map[string]any{"level": "status", "code": "NetConnection.Connect.Success", "objectEncoding": float64(0)}}
			case "createStream":
				reply = []any{"_result", tx, nil, float64(1)}
			case "releaseStream", "FCPublish":
				reply = []any{"_result", tx, nil, nil}
			case "publish":
				if len(values) < 4 {
					return errors.New("нет publish-name")
				}
				key, _ := values[3].(string)
				r.mu.Lock()
				r.keys = append(r.keys, key)
				r.notifyLocked()
				r.mu.Unlock()
				reply = []any{"onStatus", float64(0), nil, map[string]any{"level": "status", "code": "NetStream.Publish.Start", "description": "accepted"}}
			}
			if reply != nil {
				stream := uint32(0)
				if name == "publish" {
					stream = 1
				}
				if err = publisherWriteMessage(conn, 20, stream, 0, publisherAMFEncode(reply...)); err != nil {
					return err
				}
				if name == "publish" && r.hold != nil {
					select {
					case <-r.hold:
					case <-r.stop:
						return net.ErrClosed
					}
				}
			}
			continue
		}
		if m.typ == 8 || m.typ == 9 || m.typ == 18 {
			r.mu.Lock()
			if len(r.messages) >= 20000 {
				r.mu.Unlock()
				return errors.New("превышен тестовый бюджет сообщений")
			}
			r.messages = append(r.messages, m)
			r.notifyLocked()
			r.mu.Unlock()
		}
	}
}

type publisherChunkState struct {
	timestamp, delta, length, stream uint32
	typ                              byte
	extended                         bool
	body                             []byte
}
type publisherChunkReader struct {
	reader  io.Reader
	chunk   uint32
	streams map[uint32]*publisherChunkState
}

func publisherU24(b []byte) uint32 { return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]) }
func (p *publisherChunkReader) next() (publisherWireMessage, error) {
	read := func(n int) ([]byte, error) { b := make([]byte, n); _, e := io.ReadFull(p.reader, b); return b, e }
	for {
		b, e := read(1)
		if e != nil {
			return publisherWireMessage{}, e
		}
		format, cs := b[0]>>6, uint32(b[0]&63)
		switch cs {
		case 0:
			b, e = read(1)
			if e != nil {
				return publisherWireMessage{}, e
			}
			cs = 64 + uint32(b[0])
		case 1:
			b, e = read(2)
			if e != nil {
				return publisherWireMessage{}, e
			}
			cs = 64 + uint32(b[0]) + 256*uint32(b[1])
		}
		s := p.streams[cs]
		if s == nil {
			if len(p.streams) >= 32 {
				return publisherWireMessage{}, errors.New("слишком много chunk streams")
			}
			s = &publisherChunkState{}
			p.streams[cs] = s
		}
		started := len(s.body) > 0 && uint32(len(s.body)) < s.length
		if format != 3 && started {
			return publisherWireMessage{}, errors.New("новый header прервал сообщение")
		}
		if format < 3 {
			n := []int{11, 7, 3}[format]
			h, e := read(n)
			if e != nil {
				return publisherWireMessage{}, e
			}
			stamp := publisherU24(h)
			s.extended = stamp == 0xffffff
			if format < 2 {
				s.length = publisherU24(h[3:])
				s.typ = h[6]
				if s.length > 4<<20 {
					return publisherWireMessage{}, errors.New("слишком большое сообщение")
				}
				if format == 0 {
					s.stream = binary.LittleEndian.Uint32(h[7:])
				}
			}
			if s.extended {
				h, e = read(4)
				if e != nil {
					return publisherWireMessage{}, e
				}
				stamp = binary.BigEndian.Uint32(h)
			}
			if format == 0 {
				s.timestamp = stamp
				s.delta = 0
			} else {
				s.delta = stamp
				s.timestamp += stamp
			}
			s.body = nil
		} else {
			if s.extended {
				h, e := read(4)
				if e != nil {
					return publisherWireMessage{}, e
				}
				value := binary.BigEndian.Uint32(h)
				if value != s.timestamp && value != s.delta {
					return publisherWireMessage{}, errors.New("неповторённый extended timestamp")
				}
			}
			if !started {
				s.timestamp += s.delta
				s.body = nil
			}
		}
		remaining := s.length - uint32(len(s.body))
		h, e := read(int(min(remaining, p.chunk)))
		if e != nil {
			return publisherWireMessage{}, e
		}
		s.body = append(s.body, h...)
		if uint32(len(s.body)) == s.length {
			m := publisherWireMessage{typ: s.typ, stream: s.stream, timestamp: s.timestamp, body: bytes.Clone(s.body)}
			s.body = nil
			return m, nil
		}
	}
}

func publisherWriteMessage(w io.Writer, typ byte, stream, stamp uint32, body []byte) error {
	h := []byte{3, byte(stamp >> 16), byte(stamp >> 8), byte(stamp), byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body)), typ, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(h[8:], stream)
	var wire bytes.Buffer
	wire.Write(h)
	for len(body) > 0 {
		n := min(len(body), 128)
		wire.Write(body[:n])
		body = body[n:]
		if len(body) > 0 {
			wire.WriteByte(0xc3)
		}
	}
	_, err := io.Copy(w, &wire)
	return err
}

func publisherAMFEncode(values ...any) []byte {
	var out bytes.Buffer
	var encode func(any)
	encode = func(value any) {
		switch v := value.(type) {
		case nil:
			out.WriteByte(5)
		case string:
			out.WriteByte(2)
			_ = binary.Write(&out, binary.BigEndian, uint16(len(v)))
			out.WriteString(v)
		case float64:
			out.WriteByte(0)
			_ = binary.Write(&out, binary.BigEndian, math.Float64bits(v))
		case bool:
			out.WriteByte(1)
			if v {
				out.WriteByte(1)
			} else {
				out.WriteByte(0)
			}
		case map[string]any:
			out.WriteByte(3)
			for k, x := range v {
				_ = binary.Write(&out, binary.BigEndian, uint16(len(k)))
				out.WriteString(k)
				encode(x)
			}
			out.Write([]byte{0, 0, 9})
		default:
			panic(fmt.Sprintf("неподдержанный тестовый AMF тип %T", v))
		}
	}
	for _, v := range values {
		encode(v)
	}
	return out.Bytes()
}

func publisherAMFValues(data []byte) ([]any, error) {
	r := bytes.NewReader(data)
	elements := 0
	var value func(int) (any, error)
	str := func() (string, error) {
		var n uint16
		if e := binary.Read(r, binary.BigEndian, &n); e != nil {
			return "", e
		}
		b := make([]byte, int(n))
		_, e := io.ReadFull(r, b)
		return string(b), e
	}
	value = func(depth int) (any, error) {
		elements++
		if depth > 8 || elements > 256 {
			return nil, errors.New("превышен тестовый AMF бюджет")
		}
		typ, e := r.ReadByte()
		if e != nil {
			return nil, e
		}
		switch typ {
		case 0:
			var n uint64
			e = binary.Read(r, binary.BigEndian, &n)
			return math.Float64frombits(n), e
		case 1:
			b, e := r.ReadByte()
			return b != 0, e
		case 2:
			return str()
		case 5, 6:
			return nil, nil
		case 10:
			var n uint32
			if e = binary.Read(r, binary.BigEndian, &n); e != nil {
				return nil, e
			}
			if n > 256 {
				return nil, errors.New("слишком длинный тестовый AMF array")
			}
			list := make([]any, 0, n)
			for range n {
				item, e := value(depth + 1)
				if e != nil {
					return nil, e
				}
				list = append(list, item)
			}
			return list, nil
		case 3, 8:
			if typ == 8 {
				var n uint32
				if e = binary.Read(r, binary.BigEndian, &n); e != nil {
					return nil, e
				}
			}
			o := map[string]any{}
			for {
				k, e := str()
				if e != nil {
					return nil, e
				}
				if k == "" {
					b, e := r.ReadByte()
					if e != nil || b != 9 {
						return nil, errors.New("неверное завершение AMF object")
					}
					return o, nil
				}
				v, e := value(depth + 1)
				if e != nil {
					return nil, e
				}
				o[k] = v
			}
		default:
			return nil, fmt.Errorf("неподдержанный тестовый AMF marker %d", typ)
		}
	}
	var result []any
	for r.Len() > 0 {
		v, e := value(0)
		if e != nil {
			return nil, e
		}
		result = append(result, v)
	}
	return result, nil
}
