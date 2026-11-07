package rtmp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/rawmessage"
)

func transportSelfControl(typ byte, body []byte) *rawmessage.Message {
	return &rawmessage.Message{ChunkStreamID: 2, Type: typ, Body: body}
}

func TestOutboundTLSBlockedAlertPreservesCertificateFailure(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"example.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	defer func() { _ = left.Close(); _ = right.Close() }()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		peer := tls.Server(right, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
		_ = peer.Handshake()
	}()
	v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
	target, err := ResolveOutbound("rtmps://example.test/app", "key", core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio})
	if err != nil {
		t.Fatal(err)
	}
	l := transportSelfLimits()
	l.TLSTimeout = 30 * time.Millisecond
	_, err = DialOutbound(context.Background(), target, OutboundOptions{Limits: l, TLSConfig: &tls.Config{RootCAs: x509.NewCertPool()}, DialContext: func(context.Context, string, string) (net.Conn, error) { return left, nil }, Admit: func(context.Context, OutboundOperation) (func(), error) { return func() {}, nil }})
	var certificate *tls.CertificateVerificationError
	if !errors.As(err, &certificate) || !IsOutboundTerminal(err) {
		t.Fatalf("отказ сертификата подменён сетевым timeout: %v", err)
	}
	_ = right.Close()
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("TLS peer не завершён")
	}
}

func TestOutboundPeerWindowAndAcknowledgements(t *testing.T) {
	c := transportSelfConn(t, &transportSelfMemory{})
	r := outboundReader{c: c}
	bandwidth := func(window uint32, kind byte) error {
		var b [5]byte
		binary.BigEndian.PutUint32(b[:4], window)
		b[4] = kind
		return c.control(&r, transportSelfControl(6, b[:]))
	}
	if err := bandwidth(100, 2); err != nil || c.peerWindow != 0 {
		t.Fatalf("initial dynamic: %d %v", c.peerWindow, err)
	}
	for _, tc := range []struct {
		window uint32
		kind   byte
		want   uint32
	}{{100, 0, 100}, {200, 1, 100}, {50, 1, 50}, {80, 2, 50}, {120, 0, 120}, {140, 2, 140}, {160, 2, 160}} {
		if err := bandwidth(tc.window, tc.kind); err != nil || c.peerWindow != tc.want {
			t.Fatalf("peer bandwidth %v: %d %v", tc, c.peerWindow, err)
		}
	}
	if len(c.controls) != 5 {
		t.Fatalf("ответы WindowAckSize: %d", len(c.controls))
	}
	c.protocolBytes = (1 << 32) + 8
	c.peerAcknowledged = (1 << 32) - 8
	if err := c.control(&r, transportSelfControl(3, []byte{0, 0, 0, 8})); err != nil || c.peerAcknowledged != (1<<32)+8 {
		t.Fatalf("ACK wrap: %d %v", c.peerAcknowledged, err)
	}
	if err := c.control(&r, transportSelfControl(3, []byte{0, 0, 0, 9})); !errors.Is(err, ErrFraming) {
		t.Fatal("ACK будущих bytes принят")
	}
	if err := c.control(&r, transportSelfControl(3, []byte{0x80, 0, 0, 8})); !errors.Is(err, ErrFraming) {
		t.Fatal("неоднозначный ACK принят")
	}
	for _, m := range []*rawmessage.Message{transportSelfControl(0, nil), transportSelfControl(1, []byte{0, 0, 0, 0}), transportSelfControl(1, []byte{0xff, 0xff, 0xff, 0xff}), transportSelfControl(2, []byte{0, 0, 0, 1}), transportSelfControl(3, []byte{1}), transportSelfControl(4, []byte{0, 99, 0, 0, 0, 0}), transportSelfControl(5, []byte{0, 0, 0, 0}), transportSelfControl(6, []byte{0, 0, 0, 1, 3}), {ChunkStreamID: 4, Type: 3, Body: make([]byte, 4)}} {
		if err := c.control(&r, m); err == nil {
			t.Fatalf("некорректное control принято: %+v", m)
		}
	}
}

func TestOutboundWindowWaitResumesWithoutDiscardingConnection(t *testing.T) {
	left, right := net.Pipe()
	defer func() { _ = right.Close(); _ = left.Close() }()
	c := transportSelfConn(t, left)
	c.countProtocol = true
	c.peerWindow = 20
	c.peerLimitKnown = true
	c.opts.Limits.WriteTimeout = time.Second
	peerDone := make(chan error, 1)
	go func() {
		r := outboundReader{c: c}
		var total uint32
		for _, n := range []int{20, 20, 20, 20, 13} {
			b := make([]byte, n)
			if _, err := io.ReadFull(right, b); err != nil {
				peerDone <- err
				return
			}
			total += uint32(n)
			var ack [4]byte
			binary.BigEndian.PutUint32(ack[:], total)
			if err := c.control(&r, transportSelfControl(3, ack[:])); err != nil {
				peerDone <- err
				return
			}
		}
		peerDone <- nil
	}()
	if err := c.writeMessage(context.Background(), 6, 9, 1, 0, bytes.Repeat([]byte{7}, 81)); err != nil {
		t.Fatal(err)
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
	if c.Err() != nil || c.Snapshot().WrittenBytes != 93 {
		t.Fatalf("ACK in flight вызвал отказ: %+v %v", c.Snapshot(), c.Err())
	}
}

func TestOutboundWindowWaitHasFixedDeadline(t *testing.T) {
	m := &transportSelfMemory{}
	c := transportSelfConn(t, m)
	c.countProtocol = true
	c.peerWindow = 12
	c.opts.Limits.WriteTimeout = 20 * time.Millisecond
	if err := c.writeMessage(context.Background(), 6, 9, 1, 0, []byte{1}); !errors.Is(err, context.DeadlineExceeded) || IsOutboundTerminal(err) || c.Snapshot().WrittenBytes != 12 {
		t.Fatalf("window timeout: %v %+v", err, c.Snapshot())
	}
	if !m.closed {
		t.Fatal("незавершённое сообщение не закрыло соединение")
	}
}

func TestOutboundStartedDeadlineSurvivesInterleaving(t *testing.T) {
	data := []byte{4, 0, 0, 0, 0, 0, 3, 20, 0, 0, 0, 0, 5, 5, 0, 0, 0, 0, 0, 1, 20, 0, 0, 0, 0, 5, 0xc4, 5}
	m := &transportSelfMemory{input: bytes.NewReader(data)}
	c := transportSelfConn(t, m)
	r := outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 1}
	if msg, err := r.chunk(); msg != nil || err != nil {
		t.Fatal(msg, err)
	}
	first := r.streams[4].deadline
	if msg, err := r.chunk(); msg == nil || err != nil {
		t.Fatal(msg, err)
	}
	if msg, err := r.chunk(); msg != nil || err != nil {
		t.Fatal(msg, err)
	}
	for _, d := range m.deadlines[2:] {
		if d != first {
			t.Fatalf("срок был продлён: %v != %v", d, first)
		}
	}
	left, right := net.Pipe()
	defer func() { _ = right.Close(); _ = left.Close() }()
	c = transportSelfConn(t, left)
	c.opts.Limits.MessageTimeout = 20 * time.Millisecond
	r = outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 128}
	done := make(chan error, 1)
	go func() { _, err := r.next(); done <- err }()
	if _, err := right.Write([]byte{4}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var n net.Error
		if !errors.As(err, &n) || !n.Timeout() {
			t.Fatalf("started timeout: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("частичный заголовок не ограничен сроком")
	}
}

func TestOutboundExtendedTimestampAndLargeChunkStreamIDs(t *testing.T) {
	data := []byte{4, 255, 255, 255, 0, 0, 2, 20, 4, 3, 2, 1, 1, 0, 0, 2, 5, 0xc4, 1, 0, 0, 2, 5}
	for _, bad := range []bool{false, true} {
		raw := bytes.Clone(data)
		if bad {
			raw[len(raw)-2]++
		}
		c := transportSelfConn(t, &transportSelfMemory{input: bytes.NewReader(raw)})
		r := outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 1}
		m, err := r.next()
		if bad {
			if !errors.Is(err, ErrFraming) {
				t.Fatal(err)
			}
		} else if err != nil || m.Timestamp != 0x1000002*time.Millisecond || !bytes.Equal(m.Body, []byte{5, 5}) {
			t.Fatalf("extended: %+v %v", m, err)
		}
	}
	for _, basic := range [][]byte{{0, 0}, {1, 0, 1}} {
		raw := append(bytes.Clone(basic), 0, 0, 0, 0, 0, 1, 20, 4, 3, 2, 1, 5)
		c := transportSelfConn(t, &transportSelfMemory{input: bytes.NewReader(raw)})
		r := outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 128}
		m, err := r.next()
		if err != nil || m.MessageStreamID != 0x01020304 || len(r.streams) != 1 {
			t.Fatalf("large csid: %+v %v", m, err)
		}
	}
	for _, limit := range []string{"fragments", "streams", "retained"} {
		t.Run(limit, func(t *testing.T) {
			m := &transportSelfMemory{input: bytes.NewReader(data)}
			c := transportSelfConn(t, m)
			r := outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 1}
			switch limit {
			case "fragments":
				c.opts.Limits.MaxFragments = 1
			case "streams":
				c.opts.Limits.MaxChunkStreams = 1
				raw := append(bytes.Clone(data), 5, 0, 0, 0, 0, 0, 1, 20, 0, 0, 0, 0, 5)
				m.input = bytes.NewReader(raw)
			case "retained":
				c.opts.Limits.MaxRetainedBytes = c.opts.Limits.baseBytes() + c.target.retainedBytes() + 1
			}
			if _, err := r.next(); limit == "streams" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err = r.next(); !errors.Is(err, ErrLimit) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrLimit) {
				t.Fatalf("%s: %v", limit, err)
			}
		})
	}
}

func TestOutboundStaleAfterAdmissionPreservesSocket(t *testing.T) {
	m := &transportSelfMemory{}
	c := transportSelfConn(t, m)
	checks := 0
	finished := false
	c.opts.Admit = func(context.Context, OutboundOperation) (func(), error) { return func() { finished = true }, nil }
	err := c.withCurrentWriter(context.Background(), func() bool { checks++; return checks == 1 }, func() error {
		t.Fatal("устаревшее сообщение сериализовано")
		return nil
	})
	if !errors.Is(err, media.ErrStaleSource) || !finished || c.Err() != nil || m.output.Len() != 0 {
		t.Fatalf("stale: %v %v %v", err, finished, c.Err())
	}
	if _, err = c.WriteDelivery(context.Background(), media.Delivery{}); !errors.Is(err, media.ErrStaleSource) || c.Err() != nil {
		t.Fatalf("нулевая доставка: %v", err)
	}
}

func TestOutboundCommandPreflightAndDenialDetails(t *testing.T) {
	c := transportSelfConn(t, &transportSelfMemory{})
	for _, raw := range [][]byte{{10, 255, 255, 255, 255}, {2, 0, 10, 'x'}, {5}, {0, 0, 0, 0, 0, 0, 0, 0, 0}} {
		if _, err := c.decodeCommand(&rawmessage.Message{Body: raw}, 0); !IsOutboundTerminal(err) {
			t.Fatalf("AMF command принят: %x %v", raw, err)
		}
	}
	description := "Синтетический отказ авторизации"
	denied := outboundDenial(outboundCommand{args: amf0.Data{nil, amf0.Object{{Key: "code", Value: "NetStream.Publish.BadName"}, {Key: "description", Value: description}}}})
	var e *OutboundError
	if !errors.As(denied, &e) || e.Code != "NetStream.Publish.BadName" || e.Cause.Error() != description || !IsOutboundTerminal(denied) {
		t.Fatalf("причина отказа потеряна: %v", denied)
	}
}

func TestOutboundDeclaredCapabilitiesAreCheckedBeforeNetwork(t *testing.T) {
	v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
	profile := core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio}
	for _, change := range []func(*core.MediaFormat){
		func(p *core.MediaFormat) { p.Video.CodecProfile = "unrepresentable-profile" }, func(p *core.MediaFormat) { p.Video.Tier = "high" }, func(p *core.MediaFormat) { p.Video.Width = 127 }, func(p *core.MediaFormat) { p.Video.Level = "30.0" },
		func(p *core.MediaFormat) { p.Video.Color.Range = "unknown" }, func(p *core.MediaFormat) { p.Video.Color.Primaries = "iso-1" }, func(p *core.MediaFormat) { p.Video.Color.Transfer = "iso-16" }, func(p *core.MediaFormat) { p.Video.Color.Matrix = "not-a-matrix" },
		func(p *core.MediaFormat) { p.Audio.Channels = 1 }, func(p *core.MediaFormat) { p.Audio.SampleRateHz = 1 << 24 }, func(p *core.MediaFormat) { p.Audio.CodecProfile = "HE" },
	} {
		candidate := profile
		change(&candidate)
		if _, err := ResolveOutbound("rtmp://example.test/app", "key", candidate); !IsOutboundTerminal(err) {
			t.Fatalf("неподдержанный полный профиль принят: %+v %v", candidate, err)
		}
	}
	for _, level := range []string{"", "auto", "1.1"} {
		candidate := profile
		candidate.Video.Level = level
		if _, err := ResolveOutbound("rtmp://example.test/app", "key", candidate); err != nil {
			t.Fatalf("допустимый level: %v", err)
		}
	}
	if !outboundColorValue("iso-0", "iso-", map[uint8]string{1: "bt709"}) || outboundColorValue("iso-256", "iso-", nil) || outboundColorValue("iso-01", "iso-", nil) {
		t.Fatal("каноническое кодовое значение")
	}
	vf, _, err := codec.InspectAV1TU(testAV1Config(), testAV1TU(), core.FrameRate{Numerator: 30, Denominator: 1})
	if err != nil {
		t.Fatal(err)
	}
	profile.Video = vf
	for _, change := range []func(*core.MediaFormat){func(p *core.MediaFormat) { p.Video.CodecProfile = "high" }, func(p *core.MediaFormat) { p.Video.Width = 65537 }, func(p *core.MediaFormat) { p.Video.Level = "9.0" }, func(p *core.MediaFormat) { p.Video.Level = "2.0"; p.Video.Tier = "high" }, func(p *core.MediaFormat) { p.Video.Color.Transfer = "cicp:16" }, func(p *core.MediaFormat) { p.Video.Color.Matrix = "iso-0" }} {
		candidate := profile
		change(&candidate)
		if _, err := ResolveOutbound("rtmp://example.test/app", "key", candidate); !IsOutboundTerminal(err) {
			t.Fatalf("неподдержанный AV1 принят: %+v %v", candidate, err)
		}
	}
	for _, level := range []string{"", "auto", "unspecified"} {
		candidate := profile
		candidate.Video.Level = level
		candidate.Video.Tier = ""
		if _, err := ResolveOutbound("rtmp://example.test/app", "key", candidate); err != nil {
			t.Fatalf("неопределённый optional AV1 level/tier: %v", err)
		}
	}
}
