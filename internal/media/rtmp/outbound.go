package rtmp

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/handshake"
	"github.com/bluenviron/gortmplib/pkg/rawmessage"
)

// OutboundSnapshot считает принятые байты RTMP до шифрования, включая согласование
// соединения, управляющие сообщения и частичные записи с ошибкой. BufferBytes задаёт логический резерв, не RSS.
type OutboundSnapshot struct {
	WrittenBytes        uint64
	Saturated           bool
	BufferBytes         uint64
	LastWrite           time.Time
	AckAheadEventsTotal uint64
	AckAheadBytesLast   uint64
	AckAheadBytesMax    uint64
	AckLastAnomalyAt    time.Time
}

// OutboundConn владеет одним читателем, сериализатором записи и наблюдателем отмены.
// После Close вызывающий обязан дождаться Wait перед освобождением резерва исполнения.
type OutboundConn struct {
	conn                                  net.Conn
	ctx                                   context.Context
	opts                                  OutboundOptions
	target                                OutboundTarget
	startup                               atomic.Int64
	streamID                              atomic.Uint32
	mu                                    sync.Mutex
	closed                                bool
	err                                   error
	done, readerDone, watcherDone, joined chan struct{}
	writes                                sync.WaitGroup
	writeMu                               sync.Mutex
	stats                                 OutboundSnapshot
	readBuffer                            uint64
	commands                              chan outboundCommand
	inspector                             codec.Inspector
	video, audio                          media.CodecConfig
	headersPending, requireIndependent    bool
	clock                                 outboundClock
	outboundChunkSize                     uint32
	peerAcknowledged                      uint64
	writeActive                           bool
	writtenOverflow, ackEpochUnknown      bool
	peerWindow                            uint32
	peerLimitType                         byte
	peerLimitKnown                        bool
	handshakeRead                         uint64
	controls                              chan outboundControl
	controlDone                           chan struct{}
}

type outboundControl struct {
	typ  byte
	body []byte
}

type outboundCommand struct {
	name        string
	transaction float64
	streamID    uint32
	args        amf0.Data
}

// DialOutbound устанавливает TCP/TLS/RTMP и подтверждает точный Publish.Start.
// Неудачная попытка закрывает и присоединяет все начатые операции до возврата.
func DialOutbound(ctx context.Context, target OutboundTarget, opts OutboundOptions) (*OutboundConn, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if err := target.Validate(target.profile, opts.Limits); err != nil {
		return nil, err
	}
	if opts.Admit == nil {
		return nil, outboundError(OutboundInvalid, ErrFraming)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	finish, err := opts.Admit(ctx, OutboundEstablishment)
	if err != nil {
		return nil, err
	}
	if finish == nil {
		return nil, outboundError(OutboundInvalid, ErrFraming)
	}
	defer finish()
	dial := opts.DialContext
	if dial == nil {
		d := &net.Dialer{}
		dial = d.DialContext
	}
	dialCtx, cancel := context.WithTimeout(ctx, opts.Limits.DialTimeout)
	raw, err := dial(dialCtx, "tcp", target.address)
	dialErr := dialCtx.Err()
	cancel()
	if err != nil {
		if raw != nil {
			_ = raw.Close()
		}
		return nil, err
	}
	if raw == nil {
		return nil, outboundError(OutboundInvalid, ErrFraming)
	}
	if dialErr != nil {
		_ = raw.Close()
		return nil, dialErr
	}
	if err = ctx.Err(); err != nil {
		_ = raw.Close()
		return nil, err
	}
	c := &OutboundConn{conn: raw, ctx: ctx, opts: opts, target: target, done: make(chan struct{}), readerDone: make(chan struct{}), watcherDone: make(chan struct{}), joined: make(chan struct{}), commands: make(chan outboundCommand, 1), outboundChunkSize: 128, controls: make(chan outboundControl, opts.Limits.MaxControlPending), controlDone: make(chan struct{})}
	go func() {
		defer close(c.watcherDone)
		select {
		case <-ctx.Done():
			c.fail(ctx.Err())
		case <-c.done:
		}
	}()
	go func() { <-c.readerDone; <-c.controlDone; <-c.watcherDone; c.writes.Wait(); close(c.joined) }()
	go c.controlLoop()
	fail := func(e error) (*OutboundConn, error) { c.fail(e); _ = c.Wait(context.Background()); return nil, e }
	if target.tls {
		cfg := &tls.Config{}
		if opts.TLSConfig != nil {
			cfg = opts.TLSConfig.Clone()
			if cfg.RootCAs != nil {
				cfg.RootCAs = cfg.RootCAs.Clone()
			}
		}
		cfg.ServerName = target.hostname
		secured := tls.Client(raw, cfg)
		// conn заменяется под блокировкой до запуска чтения и записи RTMP; отмена закрывает текущий сокет.
		tlsDeadline := time.Now().Add(opts.Limits.TLSTimeout)
		if parentDeadline, ok := ctx.Deadline(); ok {
			tlsDeadline = earlierDeadline(tlsDeadline, parentDeadline)
		}
		if err = raw.SetDeadline(tlsDeadline); err != nil {
			close(c.readerDone)
			return fail(err)
		}
		// Срок сокета сохраняет причину проверки сертификата даже при зависшей
		// отправке предупреждения TLS; HandshakeContext не подменяет её истечением отдельного контекста.
		err = secured.HandshakeContext(ctx)
		if err != nil {
			close(c.readerDone)
			return fail(classifyOutboundProtocol(err))
		}
		if !time.Now().Before(tlsDeadline) {
			close(c.readerDone)
			return fail(context.DeadlineExceeded)
		}
		if err = raw.SetDeadline(time.Time{}); err != nil {
			close(c.readerDone)
			return fail(err)
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			close(c.readerDone)
			return fail(c.Err())
		}
		c.conn = secured
		c.mu.Unlock()
	}
	deadline := time.Now().Add(opts.Limits.StartupTimeout)
	c.startup.Store(deadline.UnixNano())
	if err = c.conn.SetReadDeadline(deadline); err != nil {
		close(c.readerDone)
		return fail(err)
	}
	readFinish, err := opts.Admit(ctx, OutboundRead)
	if err != nil {
		close(c.readerDone)
		return fail(err)
	}
	if readFinish == nil {
		close(c.readerDone)
		return fail(outboundError(OutboundInvalid, ErrFraming))
	}
	_, _, err = handshake.DoClient(outboundHandshake{c}, false, false)
	readFinish()
	if err != nil {
		close(c.readerDone)
		return fail(classifyOutboundProtocol(err))
	}
	go c.readLoop()
	startupCtx, stop := context.WithDeadline(ctx, deadline)
	defer stop()
	if err = c.command(startupCtx, 0, outboundConnectData(target)); err != nil {
		return fail(err)
	}
	m, err := c.receiveCommand(startupCtx)
	if err != nil {
		return fail(err)
	}
	if m.name != "_result" || m.transaction != 1 || m.streamID != 0 || !outboundStatus(m.args, "NetConnection.Connect.Success") {
		return fail(outboundStartupMismatch("connect", m, 0))
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], opts.Limits.ChunkSize)
	// SetChunkSize само передаётся с первоначальным размером 128.
	if err = c.writeInitialChunkSize(startupCtx, size[:]); err != nil {
		return fail(err)
	}
	if err = c.command(startupCtx, 0, amf0.Data{"createStream", float64(2), nil}); err != nil {
		return fail(err)
	}
	m, err = c.receiveCommand(startupCtx)
	if err != nil {
		return fail(err)
	}
	if m.name != "_result" || m.transaction != 2 || m.streamID != 0 || len(m.args) != 2 {
		return fail(outboundStartupMismatch("createStream", m, 0))
	}
	id, ok := m.args[1].(float64)
	if !ok || math.IsNaN(id) || math.IsInf(id, 0) || id < 1 || id > math.MaxUint32 || math.Trunc(id) != id {
		return fail(outboundError(OutboundProtocol, fmt.Errorf("некорректный идентификатор потока: phase=createStream check=stream_id value_type=%T stream_id_value=%g: %w", m.args[1], id, ErrFraming)))
	}
	c.streamID.Store(uint32(id))
	if err = c.command(startupCtx, uint32(id), amf0.Data{"publish", float64(0), nil, target.key, "live"}); err != nil {
		return fail(err)
	}
	m, err = c.receiveCommand(startupCtx)
	if err != nil {
		return fail(err)
	}
	if m.name != "onStatus" || m.transaction != 0 || m.streamID != uint32(id) || !outboundStatus(m.args, "NetStream.Publish.Start") {
		return fail(outboundStartupMismatch("publish", m, uint32(id)))
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	return c, nil
}

type outboundHandshake struct{ c *OutboundConn }

func (h outboundHandshake) Read(p []byte) (int, error) {
	n, err := h.c.conn.Read(p)
	if n > 0 {
		h.c.handshakeRead += uint64(n)
	}
	return n, err
}
func (h outboundHandshake) Write(p []byte) (int, error) {
	err := h.c.withWriter(h.c.ctx, func() error { return h.c.writeAll(p) })
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func classifyOutboundProtocol(err error) error {
	var network net.Error
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) || IsOutboundTerminal(err) {
		return err
	}
	return outboundError(OutboundProtocol, err)
}

func (c *OutboundConn) beginWrite() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.err
	}
	c.writes.Add(1)
	return nil
}

func (c *OutboundConn) withWriter(ctx context.Context, f func() error) error {
	return c.withCurrentWriter(ctx, nil, f)
}

func (c *OutboundConn) withCurrentWriter(ctx context.Context, current func() bool, f func() error) error {
	if err := c.beginWrite(); err != nil {
		return err
	}
	defer c.writes.Done()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.done:
		return c.Err()
	default:
	}
	if current != nil && !current() {
		return media.ErrStaleSource
	}
	finish, err := c.opts.Admit(ctx, OutboundWrite)
	if err != nil {
		return err
	}
	if finish == nil {
		return outboundError(OutboundInvalid, ErrFraming)
	}
	defer finish()
	if current != nil && !current() {
		return media.ErrStaleSource
	}
	d := time.Now().Add(c.opts.Limits.WriteTimeout)
	if n := c.startup.Load(); n != 0 {
		d = earlierDeadline(d, time.Unix(0, n))
	}
	if v, ok := ctx.Deadline(); ok {
		d = earlierDeadline(d, v)
	}
	if err = c.conn.SetWriteDeadline(d); err != nil {
		c.fail(err)
		return err
	}
	err = f()
	if err != nil {
		c.fail(err)
	}
	return err
}

func (c *OutboundConn) writeAll(p []byte) error {
	for len(p) > 0 {
		c.mu.Lock()
		if c.closed {
			err := c.err
			c.mu.Unlock()
			return err
		}
		c.writeActive = true
		c.mu.Unlock()
		n, err := c.conn.Write(p)
		c.mu.Lock()
		valid := n >= 0 && n <= len(p)
		if valid && n > 0 {
			if uint64(n) > math.MaxUint64-c.stats.WrittenBytes {
				c.stats.WrittenBytes = math.MaxUint64
				c.stats.Saturated = true
				c.writtenOverflow = true
			} else {
				c.stats.WrittenBytes += uint64(n)
			}
			c.stats.LastWrite = time.Now()
		}
		c.checkACKEpoch()
		c.writeActive = false
		c.mu.Unlock()
		if !valid {
			return outboundError(OutboundProtocol, io.ErrShortWrite)
		}
		if n > 0 {
			if c.opts.OnWrite != nil {
				c.opts.OnWrite(n)
			}
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

// checkACKEpoch вызывается только под c.mu; потерянная точность не восстанавливается.
func (c *OutboundConn) checkACKEpoch() {
	if c.writtenOverflow || c.stats.WrittenBytes > c.peerAcknowledged && c.stats.WrittenBytes-c.peerAcknowledged >= 1<<31 {
		c.ackEpochUnknown = true
	}
}

func (c *OutboundConn) acknowledge(raw uint32) {
	c.mu.Lock()
	c.checkACKEpoch()
	if c.ackEpochUnknown {
		c.mu.Unlock()
		return
	}
	delta := uint64(raw - uint32(c.peerAcknowledged))
	if delta == 0 || delta >= 1<<31 || delta > math.MaxUint64-c.peerAcknowledged {
		c.mu.Unlock()
		return
	}
	candidate := c.peerAcknowledged + delta
	written := c.stats.WrittenBytes
	if candidate > written && candidate-written >= 1<<31 || written > candidate && written-candidate >= 1<<31 {
		c.mu.Unlock()
		return
	}
	c.peerAcknowledged = candidate
	if c.writeActive || candidate <= written {
		c.mu.Unlock()
		return
	}
	ahead, at := candidate-written, time.Now()
	if c.stats.AckAheadEventsTotal == math.MaxUint64 {
		c.stats.Saturated = true
	} else {
		c.stats.AckAheadEventsTotal++
	}
	c.stats.AckAheadBytesLast = ahead
	c.stats.AckAheadBytesMax = max(c.stats.AckAheadBytesMax, ahead)
	c.stats.AckLastAnomalyAt = at
	c.mu.Unlock()
	if c.opts.OnACKAnomaly != nil {
		c.opts.OnACKAnomaly(ahead, at)
	}
}

func (c *OutboundConn) queueControl(typ byte, body []byte) error {
	select {
	case <-c.done:
		return c.Err()
	default:
	}
	select {
	case c.controls <- outboundControl{typ: typ, body: append([]byte(nil), body...)}:
		return nil
	default:
		return ErrLimit
	}
}

func (c *OutboundConn) controlLoop() {
	defer close(c.controlDone)
	for {
		select {
		case <-c.done:
			return
		case m := <-c.controls:
			if err := c.writeMessage(c.ctx, 2, m.typ, 0, 0, m.body); err != nil {
				c.fail(err)
				return
			}
		}
	}
}

func (c *OutboundConn) writeMessage(ctx context.Context, csid, typ byte, streamID, timestamp uint32, body []byte) error {
	return c.withWriter(ctx, func() error { return c.writeFramed(csid, typ, streamID, timestamp, body) })
}

func (c *OutboundConn) writeInitialChunkSize(ctx context.Context, body []byte) error {
	// Это четырёхбайтовое сообщение целиком помещается в начальные 128 байт.
	return c.withWriter(ctx, func() error {
		if err := c.writeFramed(2, 1, 0, 0, body); err != nil {
			return err
		}
		c.outboundChunkSize = c.opts.Limits.ChunkSize
		return nil
	})
}

func outboundConnectData(target OutboundTarget) amf0.Data {
	return amf0.Data{"connect", float64(1), amf0.Object{{Key: "app", Value: target.app}, {Key: "tcUrl", Value: target.endpoint}, {Key: "type", Value: "nonprivate"}, {Key: "flashVer", Value: "FMLE/3.0 (compatible; native)"}, {Key: "fpad", Value: false}, {Key: "capabilities", Value: float64(15)}, {Key: "audioCodecs", Value: float64(1024)}, {Key: "videoCodecs", Value: float64(128)}, {Key: "videoFunction", Value: float64(1)}, {Key: "objectEncoding", Value: float64(0)}, {Key: "fourCcList", Value: amf0.StrictArray{"av01"}}}}
}

func (c *OutboundConn) command(ctx context.Context, id uint32, data amf0.Data) error {
	body, err := c.marshalAMF(data)
	if err != nil {
		return err
	}
	return c.writeMessage(ctx, 3, 20, id, 0, body)
}

func (c *OutboundConn) marshalAMF(data amf0.Data) ([]byte, error) {
	n, err := data.MarshalSize()
	if err != nil {
		return nil, outboundError(OutboundProtocol, err)
	}
	if uint64(n) > uint64(c.opts.Limits.MaxAMFBytes) {
		return nil, outboundError(OutboundInvalid, ErrLimit)
	}
	b, err := data.Marshal()
	if err != nil {
		return nil, outboundError(OutboundProtocol, err)
	}
	if err = preflightAMF(b, c.opts.Limits.amfLimits()); err != nil {
		return nil, outboundError(OutboundProtocol, err)
	}
	return b, nil
}

func (c *OutboundConn) receiveCommand(ctx context.Context) (outboundCommand, error) {
	select {
	case <-ctx.Done():
		return outboundCommand{}, ctx.Err()
	case m := <-c.commands:
		return m, nil
	case <-c.done:
		return outboundCommand{}, c.Err()
	}
}

func (c *OutboundConn) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if err == nil {
		err = net.ErrClosed
	}
	c.closed = true
	c.err = err
	close(c.done)
	conn := c.conn
	c.mu.Unlock()
	_ = conn.Close()
}

// Close останавливает I/O; Wait отдельно подтверждает физическое завершение.
func (c *OutboundConn) Close() error          { c.fail(net.ErrClosed); return nil }
func (c *OutboundConn) Done() <-chan struct{} { return c.done }
func (c *OutboundConn) Err() error            { c.mu.Lock(); defer c.mu.Unlock(); return c.err }

// Wait не освобождает резерв исполнения и не считает истечение срока доказательством завершения.
// Для уже закрытого соединения повторный вызов может подтвердить позднее завершение всех операций.
func (c *OutboundConn) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.joined:
		return nil
	}
}

func (c *OutboundConn) Snapshot() OutboundSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.BufferBytes = c.opts.Limits.baseBytes() + c.target.retainedBytes() + c.readBuffer
	return s
}
func (c *OutboundConn) setReadBuffer(n uint64) { c.mu.Lock(); c.readBuffer = n; c.mu.Unlock() }

func (c *OutboundConn) readLoop() {
	defer close(c.readerDone)
	finish, err := c.opts.Admit(c.ctx, OutboundRead)
	if err != nil {
		c.fail(err)
		return
	}
	if finish == nil {
		c.fail(outboundError(OutboundInvalid, ErrFraming))
		return
	}
	defer finish()
	r := outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 128, readBytes: c.handshakeRead}
	startup := true
	for {
		m, err := r.next()
		if err != nil {
			c.fail(classifyOutboundProtocol(err))
			return
		}
		if m.Type <= 6 {
			if err = c.control(&r, m); err != nil {
				c.fail(classifyOutboundProtocol(err))
				return
			}
			continue
		}
		if m.Type != 20 {
			c.fail(outboundError(OutboundProtocol, outboundMessageError("message_type", m, r.messageChunkStreamID, fmt.Errorf("неподдерживаемый тип сообщения: expected_type=20: %w", ErrFraming))))
			return
		}
		cmd, err := c.decodeCommand(m, r.messageChunkStreamID)
		if err != nil {
			c.fail(err)
			return
		}
		if cmd.name == "onBWDone" {
			continue
		}
		if cmd.name == "_error" {
			c.fail(outboundDenial(cmd))
			return
		}
		if cmd.name == "onStatus" && !outboundStatus(cmd.args, "NetStream.Publish.Start") {
			c.fail(outboundDenial(cmd))
			return
		}
		if !startup {
			c.fail(outboundError(OutboundProtocol, outboundMessageError("post_publish", m, r.messageChunkStreamID, fmt.Errorf("неожиданная команда после публикации: command=%q transaction=%g args_len=%d: %w", cmd.name, cmd.transaction, len(cmd.args), ErrFraming))))
			return
		}
		if cmd.name == "onStatus" && cmd.transaction == 0 && cmd.streamID == c.streamID.Load() && cmd.streamID != 0 && outboundStatus(cmd.args, "NetStream.Publish.Start") {
			startup = false
			c.startup.Store(0)
		}
		select {
		case c.commands <- cmd:
		case <-c.done:
			return
		}
	}
}

func (c *OutboundConn) control(r *outboundReader, m *rawmessage.Message) (err error) {
	defer func() {
		if err != nil {
			csid := uint32(m.ChunkStreamID)
			if csid == 0 && r.messageChunkStreamID > 255 {
				csid = r.messageChunkStreamID
			}
			err = fmt.Errorf("ошибка управляющего сообщения: phase=control type=%d csid=%d stream_id=%d body_len=%d: %w", m.Type, csid, m.MessageStreamID, len(m.Body), err)
		}
	}()
	b := m.Body
	if m.MessageStreamID != 0 || m.ChunkStreamID != 2 {
		return fmt.Errorf("неверная адресация: check=control_address expected_stream_id=0 expected_csid=2: %w", ErrFraming)
	}
	switch m.Type {
	case 1:
		if len(b) != 4 {
			return fmt.Errorf("неверная длина SetChunkSize: check=body_length expected_body_len=4: %w", ErrFraming)
		}
		size := binary.BigEndian.Uint32(b)
		if size == 0 || size > c.opts.Limits.MaxChunkBytes {
			return fmt.Errorf("недопустимый размер чанка: check=chunk_size chunk_size=%d max_chunk_bytes=%d: %w", size, c.opts.Limits.MaxChunkBytes, ErrLimit)
		}
		r.chunkSize = size
	case 2:
		if len(b) != 4 {
			return fmt.Errorf("неверная длина Abort: check=body_length expected_body_len=4: %w", ErrFraming)
		}
		id := binary.BigEndian.Uint32(b)
		if id < 2 || id > 65599 {
			return fmt.Errorf("недопустимый поток Abort: check=abort_csid abort_csid=%d: %w", id, ErrFraming)
		}
		if s := r.streams[id]; s != nil && s.active {
			r.retained -= uint64(len(s.body))
			s.body = nil
			s.active = false
			s.received = 0
			c.setReadBuffer(r.retained)
		}
	case 3:
		if len(b) != 4 {
			return fmt.Errorf("неверная длина ACK: check=body_length expected_body_len=4: %w", ErrFraming)
		}
		c.acknowledge(binary.BigEndian.Uint32(b))
	case 4:
		if len(b) < 2 {
			return fmt.Errorf("отсутствует тип UserControl: check=event_header min_body_len=2: %w", ErrFraming)
		}
		event := binary.BigEndian.Uint16(b)
		want := 6
		if event == 3 {
			want = 10
		}
		if len(b) != want {
			return fmt.Errorf("неверная длина UserControl: check=body_length event=%d expected_body_len=%d: %w", event, want, ErrFraming)
		}
		switch event {
		case 0, 1, 2, 3, 4, 7:
		case 6:
			response := append([]byte(nil), b...)
			binary.BigEndian.PutUint16(response, 7)
			return c.queueControl(4, response)
		default:
			return fmt.Errorf("неподдерживаемое событие UserControl: check=event_type event=%d: %w", event, ErrFraming)
		}
	case 5:
		if len(b) != 4 {
			return fmt.Errorf("неверная длина WindowAcknowledgementSize: check=body_length expected_body_len=4: %w", ErrFraming)
		}
		r.window = binary.BigEndian.Uint32(b)
		if r.window == 0 {
			return fmt.Errorf("нулевое окно подтверждения: check=ack_window window=%d: %w", r.window, ErrFraming)
		}
		return r.ack()
	case 6:
		if len(b) != 5 {
			return fmt.Errorf("неверная длина SetPeerBandwidth: check=body_length expected_body_len=5: %w", ErrFraming)
		}
		if b[4] > 2 || binary.BigEndian.Uint32(b) == 0 {
			return fmt.Errorf("недопустимое окно или тип ограничения: check=peer_bandwidth window=%d limit_type=%d: %w", binary.BigEndian.Uint32(b), b[4], ErrFraming)
		}
		window, kind := binary.BigEndian.Uint32(b), b[4]
		c.mu.Lock()
		previous := c.peerWindow
		switch kind {
		case 0:
			c.peerWindow = window
		case 1:
			if c.peerWindow == 0 || window < c.peerWindow {
				c.peerWindow = window
			}
		case 2:
			if c.peerLimitKnown && c.peerLimitType == 0 {
				c.peerWindow = window
				kind = 0
			} else {
				c.mu.Unlock()
				return nil
			}
		}
		c.peerLimitType = kind
		c.peerLimitKnown = true
		effective := c.peerWindow
		c.mu.Unlock()
		if previous != effective {
			var value [4]byte
			binary.BigEndian.PutUint32(value[:], effective)
			return c.queueControl(5, value[:])
		}
	default:
		return fmt.Errorf("неподдерживаемое управляющее сообщение: check=control_type: %w", ErrFraming)
	}
	return nil
}

func (c *OutboundConn) decodeCommand(m *rawmessage.Message, csid uint32) (outboundCommand, error) {
	if err := preflightAMF(m.Body, c.opts.Limits.amfLimits()); err != nil {
		return outboundCommand{}, outboundError(OutboundProtocol, outboundMessageError("amf_preflight", m, csid, err))
	}
	a, err := amf0.Unmarshal(m.Body)
	if err != nil {
		return outboundCommand{}, outboundError(OutboundProtocol, outboundMessageError("amf_decode", m, csid, fmt.Errorf("ошибка декодирования AMF: decoder_error=%v: %w", err, ErrFraming)))
	}
	if len(a) < 3 {
		return outboundCommand{}, outboundError(OutboundProtocol, outboundMessageError("command_shape", m, csid, fmt.Errorf("недостаточно элементов команды: values_len=%d min_values_len=3: %w", len(a), ErrFraming)))
	}
	name, ok := a[0].(string)
	if !ok {
		return outboundCommand{}, outboundError(OutboundProtocol, outboundMessageError("command_name", m, csid, fmt.Errorf("неверный тип имени команды: value_type=%T: %w", a[0], ErrFraming)))
	}
	txn, ok := a[1].(float64)
	if !ok || math.IsNaN(txn) || math.IsInf(txn, 0) {
		return outboundCommand{}, outboundError(OutboundProtocol, outboundMessageError("command_transaction", m, csid, fmt.Errorf("некорректная транзакция команды: command=%q value_type=%T transaction=%g: %w", name, a[1], txn, ErrFraming)))
	}
	return outboundCommand{name: name, transaction: txn, streamID: m.MessageStreamID, args: a[2:]}, nil
}

func outboundMessageError(phase string, m *rawmessage.Message, csid uint32, err error) error {
	return fmt.Errorf("ошибка входящего сообщения: phase=%s type=%d csid=%d stream_id=%d body_len=%d: %w", phase, m.Type, csid, m.MessageStreamID, len(m.Body), err)
}

func outboundStartupMismatch(phase string, m outboundCommand, expectedStreamID uint32) error {
	var code string
	if len(m.args) > 1 {
		if status, ok := m.args[1].(amf0.Object); ok {
			code, _ = status.GetString("code")
		}
	}
	return outboundError(OutboundProtocol, fmt.Errorf("неожиданный ответ при установке публикации: phase=%s command=%q transaction=%g stream_id=%d expected_stream_id=%d args_len=%d status_code=%q: %w", phase, m.name, m.transaction, m.streamID, expectedStreamID, len(m.args), code, ErrFraming))
}

func outboundStatus(args amf0.Data, code string) bool {
	if len(args) != 2 {
		return false
	}
	o, ok := args[1].(amf0.Object)
	if !ok {
		return false
	}
	v, ok := o.GetString("code")
	return ok && v == code
}

func outboundDenial(m outboundCommand) error {
	e := &OutboundError{Kind: OutboundDenied}
	if len(m.args) > 1 {
		if o, ok := m.args[1].(amf0.Object); ok {
			e.Code, _ = o.GetString("code")
			if description, ok := o.GetString("description"); ok {
				e.Cause = fmt.Errorf("%s", description)
			}
		}
	}
	return e
}
