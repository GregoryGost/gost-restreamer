package rtmp

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/handshake"
	"github.com/bluenviron/gortmplib/pkg/rawmessage"
)

func ackAcceptanceControl(c *OutboundConn, raw uint32) error {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], raw)
	return c.control(&outboundReader{c: c}, &rawmessage.Message{ChunkStreamID: 2, Type: 3, Body: b[:]})
}

func TestACKAcceptanceDistinctEventsAndSerialBoundaries(t *testing.T) {
	c, _, _ := acceptanceOutboundLocal(t, &acceptanceOutboundMemory{}, false)
	c.stats.WrittenBytes = 1000
	var callbacks []uint64
	c.opts.OnACKAnomaly = func(n uint64, _ time.Time) { callbacks = append(callbacks, n) }
	before := time.Now()
	for _, raw := range []uint32{900, 1000, 1004, 1004, 1003, 1008, 1009, 1009 + 1<<31, math.MaxUint32} {
		if err := ackAcceptanceControl(c, raw); err != nil {
			t.Fatalf("числовой ACK %d завершил транспорт: %v", raw, err)
		}
	}
	s := c.Snapshot()
	if s.AckAheadEventsTotal != 3 || s.AckAheadBytesLast != 9 || s.AckAheadBytesMax != 9 || !s.AckLastAnomalyAt.After(before) || s.AckLastAnomalyAt.After(time.Now()) || !reflect.DeepEqual(callbacks, []uint64{4, 8, 9}) {
		t.Fatalf("новые, повторные, устаревшие и неоднозначные ACK: %+v callbacks=%v", s, callbacks)
	}
	c.stats.WrittenBytes = 2000
	if err := ackAcceptanceControl(c, 1800); err != nil {
		t.Fatal(err)
	}
	if got := c.Snapshot(); got.AckAheadEventsTotal != 3 || got.AckAheadBytesLast != 9 || got.AckLastAnomalyAt != s.AckLastAnomalyAt {
		t.Fatalf("нормальный ACK изменил историю: %+v", got)
	}
}

func TestACKAcceptanceWrapUnknownEpochAndOverflow(t *testing.T) {
	for _, tc := range []struct {
		name            string
		written, anchor uint64
		raw             uint32
		events, last    uint64
		unknown         bool
	}{
		{"wrap_behind", 1<<32 + 100, 1<<32 - 20, 80, 0, 0, false},
		{"wrap_ahead", 1<<32 + 100, 1<<32 - 20, 104, 1, 4, false},
		{"half_range_lag", 1 << 31, 0, 1<<31 + 4, 0, 0, true},
		{"candidate_too_far", 0, 1, 1 << 31, 0, 0, false},
		{"checked_add", math.MaxUint64 - 5, math.MaxUint64 - 5, 4, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := acceptanceOutboundLocal(t, &acceptanceOutboundMemory{}, false)
			c.stats.WrittenBytes, c.peerAcknowledged = tc.written, tc.anchor
			if err := ackAcceptanceControl(c, tc.raw); err != nil {
				t.Fatal(err)
			}
			s := c.Snapshot()
			if s.AckAheadEventsTotal != tc.events || s.AckAheadBytesLast != tc.last || c.ackEpochUnknown != tc.unknown {
				t.Fatalf("неверная измеримость: %+v unknown=%v", s, c.ackEpochUnknown)
			}
			if tc.events == 0 && !tc.unknown && c.peerAcknowledged != tc.anchor && tc.name != "wrap_behind" {
				t.Fatal("непригодный кандидат продвинул anchor")
			}
			if tc.unknown {
				c.stats.WrittenBytes = 100
				if err := ackAcceptanceControl(c, 104); err != nil {
					t.Fatal(err)
				}
				if c.Snapshot().AckAheadEventsTotal != 0 {
					t.Fatal("потерянная эпоха восстановлена эвристикой")
				}
			}
		})
	}
	m := &acceptanceOutboundMemory{}
	c, _, _ := acceptanceOutboundLocal(t, m, false)
	c.stats.WrittenBytes, c.peerAcknowledged = math.MaxUint64-1, math.MaxUint64-1
	if err := c.writeMessage(context.Background(), 6, 9, 1, 0, []byte{7}); err != nil {
		t.Fatal("насыщение W остановило Write", err)
	}
	if s := c.Snapshot(); s.WrittenBytes != math.MaxUint64 || !s.Saturated || !c.ackEpochUnknown || m.wire.Len() != 13 {
		t.Fatalf("переполнение W: %+v unknown=%v bytes=%d", s, c.ackEpochUnknown, m.wire.Len())
	}
	if err := ackAcceptanceControl(c, 9); err != nil || c.Snapshot().AckAheadEventsTotal != 0 {
		t.Fatal("неизвестная эпоха создала измерение", err)
	}
}

func TestACKAcceptanceSaturatedEventsContinueLastAndMaximum(t *testing.T) {
	c, _, _ := acceptanceOutboundLocal(t, &acceptanceOutboundMemory{}, false)
	c.stats.WrittenBytes = 100
	c.stats.AckAheadEventsTotal = math.MaxUint64
	var callbacks int
	c.opts.OnACKAnomaly = func(uint64, time.Time) { callbacks++ }
	if err := ackAcceptanceControl(c, 104); err != nil {
		t.Fatal(err)
	}
	first := c.Snapshot()
	if err := ackAcceptanceControl(c, 110); err != nil {
		t.Fatal(err)
	}
	c.stats.WrittenBytes = 110
	if err := ackAcceptanceControl(c, 112); err != nil {
		t.Fatal(err)
	}
	s := c.Snapshot()
	if s.AckAheadEventsTotal != math.MaxUint64 || !s.Saturated || s.AckAheadBytesLast != 2 || s.AckAheadBytesMax != 10 || s.AckLastAnomalyAt.Before(first.AckLastAnomalyAt) || callbacks != 3 {
		t.Fatalf("насыщение общего числа отключило наблюдения: %+v callbacks=%d", s, callbacks)
	}
}

type ackAcceptanceBlockedWrite struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *ackAcceptanceBlockedWrite) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return len(p), nil
}
func (b *ackAcceptanceBlockedWrite) SetWriteDeadline(time.Time) error { return nil }
func (b *ackAcceptanceBlockedWrite) Close() error                     { return nil }

func TestACKAcceptancePhysicalWriteSkipsWithoutReplayOrReaderWait(t *testing.T) {
	b := &ackAcceptanceBlockedWrite{entered: make(chan struct{}), release: make(chan struct{})}
	c, _, _ := acceptanceOutboundLocal(t, b, false)
	c.stats.WrittenBytes = 100
	done := make(chan error, 1)
	go func() { done <- c.writeMessage(context.Background(), 6, 9, 1, 0, []byte{7}) }()
	<-b.entered
	released := false
	defer func() {
		if !released {
			close(b.release)
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
	}()
	ackDone := make(chan error, 1)
	go func() { ackDone <- ackAcceptanceControl(c, 1000) }()
	select {
	case err := <-ackDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("читатель ждёт физической записи")
	}
	if s := c.Snapshot(); s.WrittenBytes != 100 || s.AckAheadEventsTotal != 0 {
		t.Fatalf("опубликованы неподтверждённые байты либо ложная аномалия: %+v", s)
	}
	close(b.release)
	released = true
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := ackAcceptanceControl(c, 1000); err != nil {
		t.Fatal(err)
	}
	if s := c.Snapshot(); s.WrittenBytes != 113 || s.AckAheadEventsTotal != 0 {
		t.Fatalf("ACK переигран после Write: %+v", s)
	}
	if err := ackAcceptanceControl(c, 1001); err != nil {
		t.Fatal(err)
	}
	if s := c.Snapshot(); s.AckAheadEventsTotal != 1 || s.AckAheadBytesLast != 888 {
		t.Fatalf("новый ACK вне записи не измерен: %+v", s)
	}
}

type ackAcceptanceCountConn struct {
	net.Conn
	bytes atomic.Uint64
}

func (c *ackAcceptanceCountConn) Read(p []byte) (int, error) {
	n, e := c.Conn.Read(p)
	if n > 0 {
		c.bytes.Add(uint64(n))
	}
	return n, e
}

type ackAcceptancePeer struct {
	conn   net.Conn
	count  *ackAcceptanceCountConn
	frames chan acceptanceWireMessage
	done   chan struct{}
	stop   chan struct{}
}

func (p *ackAcceptancePeer) send(typ byte, body []byte) error {
	if err := p.conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	h := []byte{2, 0, 0, 0, 0, 0, byte(len(body)), typ, 0, 0, 0, 0}
	_, err := p.conn.Write(append(h, body...))
	return err
}
func (p *ackAcceptancePeer) ack(raw uint32) error {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], raw)
	return p.send(3, b[:])
}

func ackAcceptanceDial(t *testing.T, secure bool, writeTimeout time.Duration, hold ...<-chan struct{}) (*OutboundConn, *ackAcceptancePeer) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	opts := OutboundOptions{Limits: acceptanceOutboundLimits(), Admit: func(context.Context, OutboundOperation) (func(), error) { return func() {}, nil }}
	opts.Limits.WriteTimeout = writeTimeout
	endpoint := "rtmp://receiver.test/a%2Fb/../live?x=%2F&x=2"
	var serverConfig *tls.Config
	if secure {
		cert, pool := acceptanceCertificate(t)
		serverConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		opts.TLSConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		endpoint = "rtmps://receiver.test/a%2Fb/../live?x=%2F&x=2"
	}
	opts.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", l.Addr().String())
		if err == nil {
			if err = conn.(*net.TCPConn).SetWriteBuffer(1024); err != nil {
				_ = conn.Close()
				return nil, err
			}
		}
		return conn, err
	}
	ready := make(chan *ackAcceptancePeer, 1)
	failure := make(chan error, 1)
	go func() {
		raw, e := l.Accept()
		if e != nil {
			failure <- e
			return
		}
		conn := raw
		if secure {
			conn = tls.Server(raw, serverConfig)
		}
		count := &ackAcceptanceCountConn{Conn: conn}
		p := &ackAcceptancePeer{conn: conn, count: count, frames: make(chan acceptanceWireMessage, 128), done: make(chan struct{}), stop: make(chan struct{})}
		defer close(p.done)
		defer func() { _ = conn.Close() }()
		if e = acceptanceStartup(count, ""); e != nil {
			failure <- e
			return
		}
		ready <- p
		if len(hold) > 0 {
			select {
			case <-hold[0]:
			case <-p.stop:
				return
			}
		}
		for {
			m, e := acceptanceReadMessage(count, 128)
			if e != nil {
				return
			}
			p.frames <- m
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c, err := DialOutbound(ctx, acceptanceTarget(t, endpoint), opts)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var peer *ackAcceptancePeer
	select {
	case peer = <-ready:
	case err = <-failure:
		cancel()
		t.Fatal(err)
	case <-ctx.Done():
		cancel()
		t.Fatal(ctx.Err())
	}
	t.Cleanup(func() {
		_ = c.Close()
		join, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		if e := c.Wait(join); e != nil {
			t.Error(e)
		}
		cancel()
		close(peer.stop)
		_ = peer.conn.Close()
		select {
		case <-peer.done:
		case <-join.Done():
			t.Error("приёмник не присоединён")
		}
	})
	return c, peer
}

func TestACKAcceptanceRealSlowTCPAndTLSDeadlineAndJoin(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls_%v", secure), func(t *testing.T) {
			hold := make(chan struct{})
			c, _ := ackAcceptanceDial(t, secure, 100*time.Millisecond, hold)
			body := bytes.Repeat([]byte{9}, 65536)
			var err error
			for range 256 {
				if err = c.writeMessage(context.Background(), 6, 9, 1, 0, body); err != nil {
					break
				}
			}
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() || IsOutboundTerminal(err) {
				t.Fatalf("зависший получатель не ограничен сетевым сроком: %v", err)
			}
			join, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err = c.Wait(join); err != nil {
				t.Fatal("истечение срока не завершило операции", err)
			}
		})
	}
}

func TestACKAcceptanceCloseJoinsActiveTCPAndTLSWrite(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls_%v", secure), func(t *testing.T) {
			c, _ := ackAcceptanceDial(t, secure, 5*time.Second, make(chan struct{}))
			initial := c.Snapshot().WrittenBytes
			result := make(chan error, 1)
			go func() {
				body := bytes.Repeat([]byte{9}, 65536)
				for range 256 {
					if err := c.writeMessage(context.Background(), 6, 9, 1, 0, body); err != nil {
						result <- err
						return
					}
				}
				result <- nil
			}()
			guard := time.NewTimer(time.Second)
			defer guard.Stop()
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for {
				c.mu.Lock()
				active, written := c.writeActive, c.stats.WrittenBytes
				c.mu.Unlock()
				if active && written > initial {
					break
				}
				select {
				case err := <-result:
					t.Fatalf("запись завершилась до отмены при зависшем получателе: %v", err)
				case <-guard.C:
					t.Fatal("не достигнута активная сетевая запись")
				case <-tick.C:
				}
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("отмена принята как успешная передача")
				}
			case <-time.After(time.Second):
				t.Fatal("Close ждёт будущего сетевого срока")
			}
			join, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := c.Wait(join); err != nil {
				t.Fatal("Close не присоединил физическую запись", err)
			}
		})
	}
}

func TestACKAcceptanceMalformedWireStillTerminates(t *testing.T) {
	for _, secure := range []bool{false, true} {
		for _, typ := range []byte{3, 5, 6} {
			t.Run(fmt.Sprintf("tls_%v_type_%d", secure, typ), func(t *testing.T) {
				c, peer := ackAcceptanceDial(t, secure, 5*time.Second)
				if err := peer.send(typ, []byte{1}); err != nil {
					t.Fatal(err)
				}
				select {
				case <-c.Done():
				case <-time.After(time.Second):
					t.Fatal("повреждённое управляющее сообщение принято")
				}
				if !errors.Is(c.Err(), ErrFraming) || !IsOutboundTerminal(c.Err()) {
					t.Fatal("потерян отказ проверки формата", c.Err())
				}
			})
		}
	}
}

func TestACKAcceptanceFailedDialRetainsMeasuredStartupCallback(t *testing.T) {
	client, peer := net.Pipe()
	defer func() { _ = client.Close(); _ = peer.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := peer.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var committed atomic.Uint64
	changed := make(chan struct{}, 16)
	callbacks := make(chan uint64, 4)
	serverDone := make(chan error, 1)
	go func() {
		count := &ackAcceptanceCountConn{Conn: peer}
		if _, _, err := handshake.DoServer(count, true); err != nil {
			serverDone <- err
			return
		}
		if _, err := acceptanceReadMessage(count, 128); err != nil {
			serverDone <- err
			return
		}
		for committed.Load() != count.bytes.Load() {
			select {
			case <-changed:
			case <-ctx.Done():
				serverDone <- ctx.Err()
				return
			}
		}
		p := &ackAcceptancePeer{conn: peer}
		if err := p.ack(uint32(count.bytes.Load() + 4)); err != nil {
			serverDone <- err
			return
		}
		serverDone <- acceptanceSendAMF(peer, 0, amf0.Data{"_error", float64(1), nil, amf0.Object{{Key: "code", Value: "NetConnection.Connect.Rejected"}}})
	}()
	opts := OutboundOptions{Limits: acceptanceOutboundLimits(), DialContext: func(context.Context, string, string) (net.Conn, error) { return client, nil }, Admit: func(context.Context, OutboundOperation) (func(), error) { return func() {}, nil }, OnWrite: func(n int) {
		committed.Add(uint64(n))
		select {
		case changed <- struct{}{}:
		default:
		}
	}, OnACKAnomaly: func(n uint64, _ time.Time) { callbacks <- n }}
	conn, err := DialOutbound(ctx, acceptanceTarget(t, "rtmp://receiver.test/a%2Fb/../live?x=%2F&x=2"), opts)
	if conn != nil || !IsOutboundTerminal(err) {
		t.Fatalf("неверный результат установления соединения: %v %v", conn, err)
	}
	if err = <-serverDone; err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-callbacks:
		if n != 4 {
			t.Fatal("неверный размер ACK при установлении соединения", n)
		}
	default:
		t.Fatal("DialOutbound потерял OnACKAnomaly неудачного установления соединения")
	}
	if len(callbacks) != 0 {
		t.Fatal("ACK установления соединения повторно учтён")
	}
}

func ackAcceptanceFrame(t *testing.T, p *ackAcceptancePeer, typ byte) acceptanceWireMessage {
	t.Helper()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case m := <-p.frames:
			if m.typ == typ {
				return m
			}
		case <-timeout.C:
			t.Fatalf("не получен тип %d до срока записи", typ)
			return acceptanceWireMessage{}
		}
	}
}

func TestACKAcceptanceRealTCPAndTLSProgressBeyondWindow(t *testing.T) {
	for _, secure := range []bool{false, true} {
		for _, mode := range []string{"absent", "constant_ahead", "duplicate_stale_wrap"} {
			t.Run(fmt.Sprintf("tls_%v_%s", secure, mode), func(t *testing.T) {
				c, p := ackAcceptanceDial(t, secure, 5*time.Second)
				if got := c.Snapshot().WrittenBytes; got != p.count.bytes.Load() || got < 3073 {
					t.Fatalf("не учтены согласование соединения и управляющие сообщения: written=%d receiver=%d", got, p.count.bytes.Load())
				}
				if err := p.send(6, []byte{0, 0, 0, 32, 0}); err != nil {
					t.Fatal(err)
				}
				if m := ackAcceptanceFrame(t, p, 5); !bytes.Equal(m.body, []byte{0, 0, 0, 32}) {
					t.Fatalf("ответ окна: %x", m.body)
				}
				offset := uint64(0)
				if mode == "duplicate_stale_wrap" {
					// Большое состояние задаётся явно; переход через ноль приходит настоящим сетевым ACK.
					c.writeMu.Lock()
					c.mu.Lock()
					offset = 1 << 32
					c.stats.WrittenBytes += offset
					c.peerAcknowledged = 1<<32 - 20
					c.mu.Unlock()
					c.writeMu.Unlock()
				}
				for i := 0; i < 4; i++ {
					if mode == "constant_ahead" {
						if err := p.ack(uint32(p.count.bytes.Load() + 4)); err != nil {
							t.Fatal(err)
						}
					}
					if mode == "duplicate_stale_wrap" {
						for _, v := range []uint32{0xfffffff0, 4, 4, 3, 0x80000004} {
							if err := p.ack(v); err != nil {
								t.Fatal(err)
							}
						}
					}
					body := bytes.Repeat([]byte{byte(i + 1)}, 4096)
					written := make(chan error, 1)
					go func() { written <- c.writeMessage(context.Background(), 6, 9, 0x765432, uint32(i), body) }()
					m := ackAcceptanceFrame(t, p, 9)
					if !bytes.Equal(m.body, body) {
						t.Fatal("получатель потерял полезную нагрузку")
					}
					select {
					case err := <-written:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(time.Second):
						t.Fatal("Write ждёт ACK до будущего сетевого срока")
					}
				}
				video, audio := acceptanceOutboundConfigs(t, false, media.TimeBase{Numerator: 1, Denominator: 1000})
				if err := c.WriteHeaders(context.Background(), video, audio); err != nil {
					t.Fatal(err)
				}
				payload := []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}
				packet := acceptanceOutboundPacket(t, video, 0, 0, true, payload)
				mediaResult := make(chan error, 1)
				go func() {
					result, err := c.WritePacket(context.Background(), packet)
					if err == nil && result.PayloadBytes != 7 {
						err = errors.New("неверное число медиабайтов")
					}
					mediaResult <- err
				}()
				for {
					frame := ackAcceptanceFrame(t, p, 9)
					if len(frame.body) > 1 && frame.body[1] == 0 {
						continue
					}
					if !bytes.Equal(frame.body, append([]byte{0x17, 1, 0, 0, 0}, payload...)) {
						t.Fatalf("медиатракт не передал реальный H.264 AU: %x", frame.body)
					}
					break
				}
				select {
				case err := <-mediaResult:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("медиатракт ждёт ACK после превышения окна")
				}
				if c.Err() != nil || c.Snapshot().WrittenBytes != p.count.bytes.Load()+offset {
					t.Fatalf("ACK прервал либо исказил счётчик: %+v bytes=%d err=%v", c.Snapshot(), p.count.bytes.Load(), c.Err())
				}
				if err := p.send(5, []byte{0, 0, 0, 1}); err != nil {
					t.Fatal(err)
				}
				if m := ackAcceptanceFrame(t, p, 3); len(m.body) != 4 || binary.BigEndian.Uint32(m.body) < 3073 {
					t.Fatalf("собственный ACK потерян: %x", m.body)
				}
				if err := p.send(4, []byte{0, 6, 1, 2, 3, 4}); err != nil {
					t.Fatal(err)
				}
				if m := ackAcceptanceFrame(t, p, 4); !bytes.Equal(m.body, []byte{0, 7, 1, 2, 3, 4}) {
					t.Fatalf("ping: %x", m.body)
				}
			})
		}
	}
}

func TestACKAcceptancePartialPositiveWriteUsesOnlyReceivedBytes(t *testing.T) {
	m := &acceptanceOutboundMemory{short: 7, failAt: 17}
	c, _, _ := acceptanceOutboundLocal(t, m, false)
	if err := c.writeMessage(context.Background(), 6, 9, 1, 0, bytes.Repeat([]byte{1}, 128)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if err := ackAcceptanceControl(c, 21); err != nil {
		t.Fatal(err)
	}
	if s := c.Snapshot(); s.WrittenBytes != 17 || s.AckAheadEventsTotal != 1 || s.AckAheadBytesLast != 4 || m.wire.Len() != 17 {
		t.Fatalf("положительный результат частичной записи: %+v received=%d", s, m.wire.Len())
	}
}
