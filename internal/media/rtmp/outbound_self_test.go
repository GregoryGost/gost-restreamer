package rtmp

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/handshake"
	"github.com/bluenviron/gortmplib/pkg/rawmessage"
)

func transportSelfLimits() OutboundLimits {
	return OutboundLimits{MaxChunkStreams: 8, MaxMessageBytes: 1 << 20, MaxChunkBytes: 65536, MaxFragments: 20000, MaxControlBytes: 1024, MaxConfigBytes: 65536, MaxAMFBytes: 65536, MaxAMFDepth: 16, MaxAMFElements: 1024, ChunkSize: 4096, MaxControlPending: 16, MaxRetainedBytes: 32 << 20, DialTimeout: time.Second, TLSTimeout: time.Second, StartupTimeout: time.Second, MessageTimeout: time.Second, WriteTimeout: time.Second}
}

func transportSelfConfigs(t *testing.T, base media.TimeBase) (media.CodecConfig, media.CodecConfig) {
	t.Helper()
	h, _ := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	vf, err := codec.H264Format(codec.VideoDescription{Headers: h, Width: 128, Height: 128})
	if err != nil {
		t.Fatal(err)
	}
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: base, Video: vf}, h)
	if err != nil {
		t.Fatal(err)
	}
	af, err := codec.AudioFormatASC([]byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: base, Audio: af}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	return v, a
}

func transportSelfPacket(t *testing.T, c media.CodecConfig, pts, dts int64, key bool) media.Packet {
	t.Helper()
	payload := []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}
	if c.Info().Track == media.TrackAudio {
		payload = []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c}
	}
	if c.Info().Video.Codec == core.CodecAV1 {
		payload = testAV1TU()
	}
	p, err := media.NewPacket(media.PacketInfo{PTS: pts, DTS: dts, DurationTicks: 1, Keyframe: key}, c, payload)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type transportSelfMemory struct {
	input     *bytes.Reader
	output    bytes.Buffer
	short     int
	failure   error
	deadline  time.Time
	deadlines []time.Time
	closed    bool
}

func (c *transportSelfMemory) Read(p []byte) (int, error) {
	if c.input == nil {
		return 0, io.EOF
	}
	return c.input.Read(p)
}
func (c *transportSelfMemory) Write(p []byte) (int, error) {
	n := len(p)
	if c.short > 0 {
		n = min(n, c.short)
	}
	_, _ = c.output.Write(p[:n])
	return n, c.failure
}
func (c *transportSelfMemory) Close() error                  { c.closed = true; return nil }
func (c *transportSelfMemory) LocalAddr() net.Addr           { return &net.TCPAddr{} }
func (c *transportSelfMemory) RemoteAddr() net.Addr          { return &net.TCPAddr{} }
func (c *transportSelfMemory) SetDeadline(d time.Time) error { c.deadline = d; return nil }
func (c *transportSelfMemory) SetReadDeadline(d time.Time) error {
	c.deadlines = append(c.deadlines, d)
	return nil
}
func (c *transportSelfMemory) SetWriteDeadline(d time.Time) error {
	c.deadline = d
	c.deadlines = append(c.deadlines, d)
	return nil
}

func transportSelfConn(t *testing.T, conn net.Conn) *OutboundConn {
	t.Helper()
	v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
	target, err := ResolveOutbound("rtmp://example.test/app", "synthetic-key", core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio})
	if err != nil {
		t.Fatal(err)
	}
	c := &OutboundConn{conn: conn, ctx: context.Background(), opts: OutboundOptions{Limits: transportSelfLimits(), Admit: func(context.Context, OutboundOperation) (func(), error) { return func() {}, nil }}, target: target, done: make(chan struct{}), flowChanged: make(chan struct{}), controls: make(chan outboundControl, 16), outboundChunkSize: 128}
	c.streamID.Store(0x01020304)
	return c
}

func TestOutboundExactQuantizationAndClock(t *testing.T) {
	for _, tc := range []struct {
		ticks int64
		base  media.TimeBase
		want  int64
		bad   bool
	}{
		{1, media.TimeBase{Numerator: 1, Denominator: 2000}, 1, false}, {-1, media.TimeBase{Numerator: 1, Denominator: 2000}, -1, false}, {1, media.TimeBase{Numerator: 1, Denominator: 3000}, 0, false}, {-1, media.TimeBase{Numerator: 1, Denominator: 3000}, 0, false},
		{math.MaxInt64, media.TimeBase{Numerator: 1, Denominator: 1000}, math.MaxInt64, false}, {math.MinInt64, media.TimeBase{Numerator: 1, Denominator: 1000}, math.MinInt64, false}, {math.MaxInt64, media.TimeBase{Numerator: 1, Denominator: 1}, 0, true}, {1, media.TimeBase{}, 0, true},
	} {
		got, err := QuantizeOutboundMilliseconds(tc.ticks, tc.base)
		if (err != nil) != tc.bad || !tc.bad && got != tc.want {
			t.Fatalf("Q(%d,%+v)=%d %v", tc.ticks, tc.base, got, err)
		}
	}
	v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 2000})
	var clock outboundClock
	stamp, cts, err := clock.project(transportSelfPacket(t, v, 1, -1, false), true)
	if err != nil || stamp != 0 || cts != 2 || clock.origin != -1 {
		t.Fatalf("независимые Q: %d %d %v", stamp, cts, err)
	}
	stamp, cts, err = clock.project(transportSelfPacket(t, v, 1, -1, false), true)
	if err != nil || stamp != 0 || cts != 2 {
		t.Fatalf("равный DTS изменён: %d %d %v", stamp, cts, err)
	}
	stamp, _, err = clock.project(transportSelfPacket(t, a, 1, 1, false), false)
	if err != nil || stamp != 2 {
		t.Fatalf("общий A/V origin: %d %v", stamp, err)
	}
	if _, _, err = clock.project(transportSelfPacket(t, a, 2, 1, false), false); !IsOutboundTerminal(err) {
		t.Fatal("AAC reorder принят")
	}
	var wrap outboundClock
	v, a = transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
	_ = a
	for _, ticks := range []int64{-10, (1 << 31) - 11, (1 << 32) - 12, (1 << 32) - 9} {
		stamp, _, err = wrap.project(transportSelfPacket(t, v, ticks, ticks, false), true)
		if err != nil {
			t.Fatal(err)
		}
	}
	if stamp != 1 {
		t.Fatalf("modulo32: %d", stamp)
	}
	if _, _, err = wrap.project(transportSelfPacket(t, v, (1<<32)-10, (1<<32)-10, false), true); !IsOutboundTerminal(err) {
		t.Fatal("DTS regression принят")
	}
	if _, err = outboundSubtract(math.MaxInt64, -1); !IsOutboundTerminal(err) {
		t.Fatal("overflow принят")
	}
	if _, err = outboundSubtract(math.MinInt64, 1); !IsOutboundTerminal(err) {
		t.Fatal("underflow принят")
	}
}

func TestOutboundCustomTargetAndPreflight(t *testing.T) {
	v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
	profile := core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio}
	target, err := ResolveOutbound("rtmps://[::1]:444/a%2Fb/../c?x=%2F&x=2", "exact/key?#", profile)
	if err != nil || target.app != "a%2Fb/../c?x=%2F&x=2" || target.key != "exact/key?#" || target.address != "[::1]:444" {
		t.Fatalf("точный target: %+v %v", target, err)
	}
	if err = target.Validate(profile, transportSelfLimits()); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"rtmp://host/app#", "rtmp://host/app#key", "rtmp://user:pass@host/app", "http://host/app", "rtmp://host:/app", "rtmp://host:0/app", "rtmp://host:65536/app", "rtmp://host/", "rtmp://[invalid]/app", "rtmp:///app"} {
		if _, err = ResolveOutbound(endpoint, "key", profile); !IsOutboundTerminal(err) {
			t.Fatalf("некорректный endpoint принят: %s", endpoint)
		}
	}
	for _, endpoint := range []string{"rtmp://host/app", "rtmps://host/app"} {
		got, e := ResolveOutbound(endpoint, "key", profile)
		if e != nil || got.address != "host:"+map[bool]string{true: "443", false: "1935"}[got.tls] {
			t.Fatalf("protocol port: %v %v", got, e)
		}
	}
	if _, err = ResolveOutbound("rtmp://host/app", strings.Repeat("x", 65536), profile); !IsOutboundTerminal(err) {
		t.Fatal("AMF string overflow принят")
	}
	bad := profile
	bad.Video.BitDepth = 10
	if _, err = ResolveOutbound("rtmp://host/app", "key", bad); !IsOutboundTerminal(err) {
		t.Fatal("неподдержанный профиль принят")
	}
	bad = profile
	bad.Video.Width++
	if err = target.Validate(bad, transportSelfLimits()); !IsOutboundTerminal(err) {
		t.Fatal("resolver изменил профиль")
	}
	if err = (OutboundTarget{}).Validate(profile, transportSelfLimits()); !IsOutboundTerminal(err) {
		t.Fatal("zero target принят")
	}
	l := transportSelfLimits()
	l.MaxAMFBytes = 16
	if err = target.Validate(profile, l); !IsOutboundTerminal(err) {
		t.Fatal("AMF budget пропущен до dial")
	}
	if IsOutboundTerminal(io.EOF) || IsOutboundTerminal(context.DeadlineExceeded) || !IsOutboundTerminal(outboundError(OutboundTiming, codec.ErrTiming)) {
		t.Fatal("неверная классификация")
	}
}

func TestOutboundFramingIndependentByteExpectationAndPartialWrites(t *testing.T) {
	m := &transportSelfMemory{short: 2}
	c := transportSelfConn(t, m)
	c.outboundChunkSize = 3
	var observed int
	c.opts.OnWrite = func(n int) { observed += n }
	if err := c.writeMessage(context.Background(), 6, 9, 0x01020304, 0x01000002, []byte{1, 2, 3, 4, 5, 6, 7}); err != nil {
		t.Fatal(err)
	}
	want := []byte{6, 255, 255, 255, 0, 0, 7, 9, 4, 3, 2, 1, 1, 0, 0, 2, 1, 2, 3, 0xc6, 1, 0, 0, 2, 4, 5, 6, 0xc6, 1, 0, 0, 2, 7}
	if !bytes.Equal(m.output.Bytes(), want) || observed != len(want) || c.Snapshot().WrittenBytes != uint64(len(want)) {
		t.Fatalf("wire=%x want=%x bytes=%d", m.output.Bytes(), want, observed)
	}
	if len(m.deadlines) != 1 || m.deadline.IsZero() {
		t.Fatal("срок полной записи обновлялся при short Write")
	}
	m = &transportSelfMemory{short: 2, failure: io.ErrClosedPipe}
	c = transportSelfConn(t, m)
	observed = 0
	c.opts.OnWrite = func(n int) { observed += n }
	if err := c.writeMessage(context.Background(), 3, 20, 0, 0, []byte{5}); !errors.Is(err, io.ErrClosedPipe) || observed != 2 || c.Snapshot().WrittenBytes != 2 || !m.closed {
		t.Fatalf("partial error: %d %v", observed, err)
	}
	c = transportSelfConn(t, &transportSelfMemory{})
	c.stats.WrittenBytes = math.MaxUint64 - 1
	if err := c.writeMessage(context.Background(), 3, 20, 0, 0, []byte{5}); err != nil || !c.Snapshot().Saturated || c.Snapshot().WrittenBytes != math.MaxUint64 {
		t.Fatalf("saturation: %+v %v", c.Snapshot(), err)
	}
}

type transportSelfFrame struct {
	typ           byte
	stream, stamp uint32
	body          []byte
}

// transportSelfReadFrame независимо сверяет байтовое представление fmt0/fmt3.
func transportSelfReadFrame(r io.Reader, chunk int) (transportSelfFrame, error) {
	var h [16]byte
	if _, err := io.ReadFull(r, h[:12]); err != nil {
		return transportSelfFrame{}, err
	}
	if h[0]>>6 != 0 {
		return transportSelfFrame{}, ErrFraming
	}
	stamp := uint32(h[1])<<16 | uint32(h[2])<<8 | uint32(h[3])
	extended := stamp == 0xffffff
	if extended {
		if _, err := io.ReadFull(r, h[12:16]); err != nil {
			return transportSelfFrame{}, err
		}
		stamp = binary.BigEndian.Uint32(h[12:16])
	}
	n := int(h[4])<<16 | int(h[5])<<8 | int(h[6])
	if n > 1<<20 {
		return transportSelfFrame{}, ErrLimit
	}
	f := transportSelfFrame{typ: h[7], stream: binary.LittleEndian.Uint32(h[8:12]), stamp: stamp, body: make([]byte, n)}
	for at := 0; at < n; {
		amount := min(chunk, n-at)
		if _, err := io.ReadFull(r, f.body[at:at+amount]); err != nil {
			return f, err
		}
		at += amount
		if at < n {
			var continuation [5]byte
			count := 1
			if extended {
				count = 5
			}
			if _, err := io.ReadFull(r, continuation[:count]); err != nil {
				return f, err
			}
			if continuation[0] != 0xc0|h[0] || extended && binary.BigEndian.Uint32(continuation[1:]) != stamp {
				return f, ErrFraming
			}
		}
	}
	return f, nil
}

func TestOutboundExactMediaHeadersAndRecoveryOrigin(t *testing.T) {
	for _, av1 := range []bool{false, true} {
		t.Run(map[bool]string{false: "h264", true: "av1"}[av1], func(t *testing.T) {
			m := &transportSelfMemory{}
			c := transportSelfConn(t, m)
			v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 2000})
			if av1 {
				vf, key, err := codec.InspectAV1TU(testAV1Config(), testAV1TU(), core.FrameRate{Numerator: 30, Denominator: 1})
				if err != nil || !key {
					t.Fatal(err)
				}
				vi := v.Info()
				vi.Video = vf
				v, err = media.NewCodecConfig(vi, testAV1Config())
				if err != nil {
					t.Fatal(err)
				}
				c.target.profile.Video = vf
			}
			if err := c.WriteHeaders(context.Background(), v, a); err != nil {
				t.Fatal(err)
			}
			if m.output.Len() != 0 {
				t.Fatal("headers выданы до проверки независимого AU")
			}
			pts, dts := int64(1), int64(-1)
			if av1 {
				pts = -1
			}
			result, err := c.WritePacket(context.Background(), transportSelfPacket(t, v, pts, dts, false))
			if err != nil || !result.Independent {
				t.Fatalf("флаг заменил проверку: %+v %v", result, err)
			}
			if _, err = c.WritePacket(context.Background(), transportSelfPacket(t, a, 1, 1, false)); err != nil {
				t.Fatal(err)
			}
			r := bytes.NewReader(m.output.Bytes())
			frames := make([]transportSelfFrame, 5)
			for i := range frames {
				frames[i], err = transportSelfReadFrame(r, 128)
				if err != nil {
					t.Fatal(err)
				}
			}
			if frames[0].typ != 18 || frames[1].typ != 9 || frames[2].typ != 8 || frames[3].stream != 0x01020304 || frames[4].stamp != 2 {
				t.Fatalf("track/header/time: %+v", frames)
			}
			videoPrefix := []byte{0x17, 0, 0, 0, 0}
			packetPrefix := []byte{0x17, 1, 0, 0, 2}
			if av1 {
				videoPrefix = []byte{0x90, 'a', 'v', '0', '1'}
				packetPrefix = []byte{0x91, 'a', 'v', '0', '1'}
			}
			if !bytes.Equal(frames[1].body, append(videoPrefix, v.Headers()...)) || !bytes.Equal(frames[2].body, append([]byte{0xaf, 0}, a.Headers()...)) || !bytes.Equal(frames[3].body, append(packetPrefix, transportSelfPacket(t, v, pts, dts, false).Payload()...)) || !bytes.Equal(frames[4].body, append([]byte{0xaf, 1}, transportSelfPacket(t, a, 1, 1, false).Payload()...)) {
				t.Fatal("исходные headers/AU изменены")
			}
			before := m.output.Len()
			if err = c.WriteHeaders(context.Background(), v, a); err != nil {
				t.Fatal(err)
			}
			if _, err = c.WritePacket(context.Background(), transportSelfPacket(t, v, 3, 3, false)); err != nil {
				t.Fatal(err)
			}
			next := bytes.NewReader(m.output.Bytes()[before:])
			for range 4 {
				f, e := transportSelfReadFrame(next, 128)
				if e != nil || f.stamp != 3 {
					t.Fatalf("recovery изменил origin: %+v %v", f, e)
				}
			}
		})
	}
}

func TestOutboundInboundInheritanceBoundsAndAbort(t *testing.T) {
	data := []byte{4, 0, 0, 10, 0, 0, 1, 20, 4, 3, 2, 1, 5, 0x44, 0, 0, 5, 0, 0, 1, 20, 5, 0x84, 0, 0, 2, 5, 0xc4, 5}
	c := transportSelfConn(t, &transportSelfMemory{input: bytes.NewReader(data)})
	r := outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 128}
	for _, want := range []uint32{10, 15, 17, 19} {
		m, err := r.next()
		if err != nil || m.MessageStreamID != 0x01020304 || m.Timestamp != time.Duration(want)*time.Millisecond || !bytes.Equal(m.Body, []byte{5}) {
			t.Fatalf("inheritance: %+v %v", m, err)
		}
	}
	for _, data := range [][]byte{{0xc4}, {4, 0, 0, 0, 0x10, 0, 1, 20, 0, 0, 0, 0}} {
		c = transportSelfConn(t, &transportSelfMemory{input: bytes.NewReader(data)})
		r = outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 128}
		if _, err := r.next(); err == nil {
			t.Fatal("неизвестное состояние/oversize принято")
		}
	}
	partial := []byte{4, 0, 0, 0, 0, 0, 2, 20, 0, 0, 0, 0, 5}
	c = transportSelfConn(t, &transportSelfMemory{input: bytes.NewReader(partial)})
	r = outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 1}
	if m, err := r.chunk(); err != nil || m != nil || r.retained != 2 {
		t.Fatal(m, err, r.retained)
	}
	if err := c.control(&r, &rawmessage.Message{ChunkStreamID: 2, Type: 2, Body: []byte{0, 0, 0, 4}}); err != nil || r.retained != 0 || r.streams[4].active {
		t.Fatalf("Abort: %v", err)
	}
}

func transportSelfWriteAMF(conn net.Conn, data amf0.Data, stream uint32) error {
	b, err := data.Marshal()
	if err != nil {
		return err
	}
	h := []byte{3, 0, 0, 0, byte(len(b) >> 16), byte(len(b) >> 8), byte(len(b)), 20, byte(stream), byte(stream >> 8), byte(stream >> 16), byte(stream >> 24)}
	for at := 0; at < len(b); {
		part := min(128, len(b)-at)
		header := h
		if at > 0 {
			header = []byte{0xc3}
		}
		if _, err = conn.Write(header); err != nil {
			return err
		}
		if _, err = conn.Write(b[at : at+part]); err != nil {
			return err
		}
		at += part
	}
	return nil
}

func transportSelfStartup(conn net.Conn, key string) error {
	return transportSelfStartupWindow(conn, key, 0)
}

type transportSelfCountingConn struct {
	net.Conn
	received uint32
}

func (c *transportSelfCountingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.received += uint32(n)
	return n, err
}

func transportSelfStartupWindow(raw net.Conn, key string, window uint32) error {
	conn := &transportSelfCountingConn{Conn: raw}
	if _, _, err := handshake.DoServer(conn, true); err != nil {
		return err
	}
	f, err := transportSelfReadFrame(conn, 128)
	if err != nil {
		return err
	}
	data, err := amf0.Unmarshal(f.body)
	if err != nil || data[0] != "connect" {
		return ErrFraming
	}
	if window != 0 {
		var body [5]byte
		binary.BigEndian.PutUint32(body[:4], window)
		if err := transportSelfWriteControl(conn, 6, body[:]); err != nil {
			return err
		}
		binary.BigEndian.PutUint32(body[:4], conn.received)
		if err := transportSelfWriteControl(conn, 3, body[:4]); err != nil {
			return err
		}
	}
	if err = transportSelfWriteAMF(conn, amf0.Data{"_result", float64(1), nil, amf0.Object{{Key: "code", Value: "NetConnection.Connect.Success"}}}, 0); err != nil {
		return err
	}
	for {
		f, err = transportSelfReadFrame(conn, 128)
		if err != nil {
			return err
		}
		if f.typ == 1 {
			break
		}
		if f.typ != 5 && f.typ != 3 {
			return ErrFraming
		}
	}
	size := int(binary.BigEndian.Uint32(f.body))
	f, err = transportSelfReadFrame(conn, size)
	if err != nil {
		return err
	}
	data, err = amf0.Unmarshal(f.body)
	if err != nil || data[0] != "createStream" {
		return ErrFraming
	}
	if err = transportSelfWriteAMF(conn, amf0.Data{"_result", float64(2), nil, float64(0x01020304)}, 0); err != nil {
		return err
	}
	f, err = transportSelfReadFrame(conn, size)
	if err != nil {
		return err
	}
	data, err = amf0.Unmarshal(f.body)
	if err != nil || f.stream != 0x01020304 || len(data) != 5 || data[0] != "publish" || data[3] != key {
		return ErrFraming
	}
	return transportSelfWriteAMF(conn, amf0.Data{"onStatus", float64(0), nil, amf0.Object{{Key: "code", Value: "NetStream.Publish.Start"}}}, 0x01020304)
}

func transportSelfWriteControl(conn net.Conn, typ byte, body []byte) error {
	h := []byte{2, 0, 0, 0, 0, 0, byte(len(body)), typ, 0, 0, 0, 0}
	if _, err := conn.Write(h); err != nil {
		return err
	}
	_, err := conn.Write(body)
	return err
}

func TestOutboundHandshakeBytesArePartOfAcknowledgementSequence(t *testing.T) {
	left, right := net.Pipe()
	defer func() { _ = left.Close(); _ = right.Close() }()
	peerDone := make(chan error, 1)
	go func() {
		err := transportSelfStartupWindow(right, "key", 256)
		peerDone <- err
		if err == nil {
			_, _ = io.Copy(io.Discard, right)
		}
	}()
	v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
	target, err := ResolveOutbound("rtmp://example.test/app", "key", core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio})
	if err != nil {
		t.Fatal(err)
	}
	c, err := DialOutbound(context.Background(), target, OutboundOptions{Limits: transportSelfLimits(), DialContext: func(context.Context, string, string) (net.Conn, error) { return left, nil }, Admit: func(context.Context, OutboundOperation) (func(), error) { return func() {}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = <-peerDone; err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = c.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if c.peerAcknowledged < 3073 || c.protocolBytes < 3073 || c.handshakeRead != 3073 || c.peerWindow != 256 {
		t.Fatalf("неверное начало ACK: sent=%d ack=%d read=%d window=%d", c.protocolBytes, c.peerAcknowledged, c.handshakeRead, c.peerWindow)
	}
}

func TestOutboundStartupQuietControlAndJoinedOperations(t *testing.T) {
	left, right := net.Pipe()
	defer func() { _ = right.Close() }()
	ready, release := make(chan error, 1), make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		ready <- transportSelfStartup(right, "exact/key?#")
		<-release
		_, _ = right.Write([]byte{2, 0, 0, 0, 0, 0, 6, 4, 0, 0, 0, 0, 0, 6, 1, 2, 3, 4})
		for {
			f, err := transportSelfReadFrame(right, 4096)
			if err != nil {
				return
			}
			if f.typ == 4 {
				if !bytes.Equal(f.body, []byte{0, 7, 1, 2, 3, 4}) {
					return
				}
				break
			}
		}
		_, _ = io.Copy(io.Discard, right)
	}()
	v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
	target, err := ResolveOutbound("rtmp://example.test/a%2Fb?x=1", "exact/key?#", core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := transportSelfLimits()
	l.MessageTimeout = 20 * time.Millisecond
	var active [4]atomic.Int32
	var written atomic.Int64
	options := OutboundOptions{Limits: l, DialContext: func(context.Context, string, string) (net.Conn, error) { return left, nil }, Admit: func(_ context.Context, k OutboundOperation) (func(), error) {
		if active[k].Add(1) != 1 {
			t.Errorf("конкурентный admission %d", k)
		}
		return func() { active[k].Add(-1) }, nil
	}, OnWrite: func(n int) { written.Add(int64(n)) }}
	c, err := DialOutbound(ctx, target, options)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	if err = <-ready; err != nil {
		t.Fatal(err)
	}
	// Тишина длиннее MessageTimeout допустима; таймер здесь задаёт проверяемую границу.
	select {
	case <-c.Done():
		t.Fatalf("idle control silence: %v", c.Err())
	case <-time.After(3 * l.MessageTimeout):
	}
	close(release)
	if err = c.WriteHeaders(ctx, v, a); err != nil {
		t.Fatal(err)
	}
	if _, err = c.WritePacket(ctx, transportSelfPacket(t, v, 0, 0, false)); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	joinCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err = c.Wait(joinCtx); err != nil {
		t.Fatal(err)
	}
	if active[OutboundEstablishment].Load() != 0 || active[OutboundRead].Load() != 0 || active[OutboundWrite].Load() != 0 || written.Load() == 0 {
		t.Fatalf("операции не завершены: %d %d %d", active[OutboundEstablishment].Load(), active[OutboundRead].Load(), active[OutboundWrite].Load())
	}
	_ = right.Close()
	<-serverDone
}

func TestOutboundLateDialAndDeniedAdmissionAreNetworkFree(t *testing.T) {
	v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
	target, _ := ResolveOutbound("rtmp://example.test/app", "key", core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio})
	left, right := net.Pipe()
	defer func() { _ = right.Close() }()
	l := transportSelfLimits()
	l.DialTimeout = time.Millisecond
	var finished bool
	_, err := DialOutbound(context.Background(), target, OutboundOptions{Limits: l, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return left, nil }, Admit: func(context.Context, OutboundOperation) (func(), error) { return func() { finished = true }, nil }})
	if !errors.Is(err, context.DeadlineExceeded) || !finished {
		t.Fatalf("late dial: %v finish=%v", err, finished)
	}
	if _, err = right.Write([]byte{1}); err == nil {
		t.Fatal("поздний socket не закрыт")
	}
	called := false
	_, err = DialOutbound(context.Background(), target, OutboundOptions{Limits: l, DialContext: func(context.Context, string, string) (net.Conn, error) { called = true; return nil, nil }, Admit: func(context.Context, OutboundOperation) (func(), error) { return nil, core.ErrStale }})
	if !errors.Is(err, core.ErrStale) || called {
		t.Fatalf("сеть до admission: %v %v", called, err)
	}
}
