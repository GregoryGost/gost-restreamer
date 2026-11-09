package rtmp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/handshake"
)

func acceptanceOutboundLimits() OutboundLimits {
	return OutboundLimits{MaxChunkStreams: 8, MaxMessageBytes: 65536, MaxChunkBytes: 4096, MaxFragments: 1024, MaxControlBytes: 256, MaxConfigBytes: 4096, MaxAMFBytes: 4096, MaxAMFDepth: 8, MaxAMFElements: 128, ChunkSize: 128, MaxControlPending: 16, MaxRetainedBytes: 4 << 20, DialTimeout: time.Second, TLSTimeout: time.Second, StartupTimeout: time.Second, MessageTimeout: time.Second, WriteTimeout: time.Second}
}

func acceptanceOutboundConfigs(t *testing.T, av1 bool, base media.TimeBase) (media.CodecConfig, media.CodecConfig) {
	t.Helper()
	h, err := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	if err != nil {
		t.Fatal(err)
	}
	vf, err := codec.H264Format(codec.VideoDescription{Headers: h, Width: 128, Height: 128})
	if err != nil {
		t.Fatal(err)
	}
	if av1 {
		h = []byte{0x81, 8, 0x0c, 0, 0x0a, 11, 0, 0, 0, 66, 167, 191, 228, 96, 13, 0, 64}
		vf, _, err = codec.InspectAV1TU(h, append(bytes.Clone(h[4:]), 0x1a, 1, 0x10), core.FrameRate{Numerator: 30, Denominator: 1})
		if err != nil {
			t.Fatal(err)
		}
	}
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: base, Video: vf}, h)
	if err != nil {
		t.Fatal(err)
	}
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: base, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	return v, a
}

func acceptanceOutboundPacket(t *testing.T, c media.CodecConfig, pts, dts int64, keyFlag bool, payload []byte) media.Packet {
	t.Helper()
	p, err := media.NewPacket(media.PacketInfo{PTS: pts, DTS: dts, DurationTicks: 1, Keyframe: keyFlag}, c, payload)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type acceptanceOutboundMemory struct {
	input                         io.Reader
	wire                          bytes.Buffer
	short, failAt                 int
	closed                        bool
	writeDeadlines, readDeadlines []time.Time
}

func (m *acceptanceOutboundMemory) Read(p []byte) (int, error) {
	if m.input == nil {
		return 0, io.EOF
	}
	return m.input.Read(p)
}
func (m *acceptanceOutboundMemory) Write(p []byte) (int, error) {
	n := len(p)
	if m.short > 0 {
		n = min(n, m.short)
	}
	if m.failAt > 0 {
		n = min(n, max(0, m.failAt-m.wire.Len()))
	}
	_, _ = m.wire.Write(p[:n])
	if m.failAt > 0 && m.wire.Len() == m.failAt {
		return n, io.ErrClosedPipe
	}
	return n, nil
}
func (m *acceptanceOutboundMemory) Close() error         { m.closed = true; return nil }
func (m *acceptanceOutboundMemory) LocalAddr() net.Addr  { return &net.TCPAddr{} }
func (m *acceptanceOutboundMemory) RemoteAddr() net.Addr { return &net.TCPAddr{} }
func (m *acceptanceOutboundMemory) SetDeadline(d time.Time) error {
	_ = m.SetReadDeadline(d)
	return m.SetWriteDeadline(d)
}
func (m *acceptanceOutboundMemory) SetReadDeadline(d time.Time) error {
	m.readDeadlines = append(m.readDeadlines, d)
	return nil
}
func (m *acceptanceOutboundMemory) SetWriteDeadline(d time.Time) error {
	m.writeDeadlines = append(m.writeDeadlines, d)
	return nil
}

func acceptanceOutboundLocal(t *testing.T, conn net.Conn, av1 bool) (*OutboundConn, media.CodecConfig, media.CodecConfig) {
	t.Helper()
	v, a := acceptanceOutboundConfigs(t, av1, media.TimeBase{Numerator: 1, Denominator: 2000})
	target, err := ResolveOutbound("rtmp://receiver.invalid/app", "synthetic-key", core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio})
	if err != nil {
		t.Fatal(err)
	}
	c := &OutboundConn{conn: conn, ctx: context.Background(), opts: OutboundOptions{Limits: acceptanceOutboundLimits(), Admit: func(context.Context, OutboundOperation) (func(), error) { return func() {}, nil }}, target: target, done: make(chan struct{}), outboundChunkSize: 128, controls: make(chan outboundControl, 16)}
	c.streamID.Store(0x765432)
	return c, v, a
}

type acceptanceWireMessage struct {
	csid, typ     byte
	stream, stamp uint32
	body          []byte
}

// Эталон разбирает только явно выбранные fmt0/fmt3 по RTMP §5.3.1 и уточнениям §§4–5.
// Он не использует рабочий анализатор либо средства разбора сообщений SDK.
func acceptanceReadMessage(r io.Reader, chunk int) (acceptanceWireMessage, error) {
	var m acceptanceWireMessage
	var h [12]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return m, err
	}
	if h[0] < 2 || h[0] > 63 {
		return m, errors.New("ожидался fmt0")
	}
	m.csid, m.typ = h[0], h[7]
	m.stream = binary.LittleEndian.Uint32(h[8:])
	m.stamp = uint32(h[1])*65536 + uint32(h[2])*256 + uint32(h[3])
	ext := m.stamp == 0xffffff
	if ext {
		var b [4]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return m, err
		}
		m.stamp = binary.BigEndian.Uint32(b[:])
	}
	length := int(h[4])*65536 + int(h[5])*256 + int(h[6])
	if length > 65536 {
		return m, errors.New("слишком длинный oracle message")
	}
	m.body = make([]byte, length)
	for at := 0; at < length; {
		if at != 0 {
			var cont [5]byte
			n := 1
			if ext {
				n = 5
			}
			if _, err := io.ReadFull(r, cont[:n]); err != nil {
				return m, err
			}
			if cont[0] != 0xc0|m.csid || ext && binary.BigEndian.Uint32(cont[1:]) != m.stamp {
				return m, errors.New("неверный fmt3/extended timestamp")
			}
		}
		n := min(chunk, length-at)
		if _, err := io.ReadFull(r, m.body[at:at+n]); err != nil {
			return m, err
		}
		at += n
	}
	return m, nil
}

func TestOutboundAcceptanceFramingVectorsAndPartialAccounting(t *testing.T) {
	for _, tc := range []struct {
		name         string
		stamp        uint32
		header, cont []byte
	}{
		{"below_extended", 0xfffffe, []byte{6, 255, 255, 254, 0, 1, 1, 9, 0x32, 0x54, 0x76, 0}, []byte{0xc6}},
		{"first_extended", 0xffffff, []byte{6, 255, 255, 255, 0, 1, 1, 9, 0x32, 0x54, 0x76, 0, 0, 255, 255, 255}, []byte{0xc6, 0, 255, 255, 255}},
		{"above_extended", 0x1000000, []byte{6, 255, 255, 255, 0, 1, 1, 9, 0x32, 0x54, 0x76, 0, 1, 0, 0, 0}, []byte{0xc6, 1, 0, 0, 0}},
		{"before_wrap", 0xffffffff, []byte{6, 255, 255, 255, 0, 1, 1, 9, 0x32, 0x54, 0x76, 0, 255, 255, 255, 255}, []byte{0xc6, 255, 255, 255, 255}},
		{"after_wrap", 0, []byte{6, 0, 0, 0, 0, 1, 1, 9, 0x32, 0x54, 0x76, 0}, []byte{0xc6}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := make([]byte, 257)
			for i := range body {
				body[i] = byte(i)
			}
			want := append(bytes.Clone(tc.header), body[:128]...)
			want = append(want, tc.cont...)
			want = append(want, body[128:256]...)
			want = append(want, tc.cont...)
			want = append(want, body[256])
			for _, short := range []int{1, 7, 128} {
				m := &acceptanceOutboundMemory{short: short}
				c, _, _ := acceptanceOutboundLocal(t, m, false)
				observed := 0
				c.opts.OnWrite = func(n int) { observed += n }
				if err := c.writeMessage(context.Background(), 6, 9, 0x765432, tc.stamp, body); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(m.wire.Bytes(), want) || observed != len(want) || c.Snapshot().WrittenBytes != uint64(len(want)) {
					t.Fatalf("short=%d wire=%x want=%x observed=%d", short, m.wire.Bytes(), want, observed)
				}
				if len(m.writeDeadlines) != 1 || m.writeDeadlines[0].IsZero() {
					t.Fatal("дробные Write продлили общий срок")
				}
			}
		})
	}
	m := &acceptanceOutboundMemory{short: 7, failAt: 17}
	c, _, _ := acceptanceOutboundLocal(t, m, false)
	observed := 0
	c.opts.OnWrite = func(n int) { observed += n }
	err := c.writeMessage(context.Background(), 6, 9, 0x765432, 0, make([]byte, 257))
	if !errors.Is(err, io.ErrClosedPipe) || observed != 17 || c.Snapshot().WrittenBytes != 17 || !m.closed {
		t.Fatalf("потеряны положительные n частичных записей: n=%d stats=%+v closed=%v err=%v", observed, c.Snapshot(), m.closed, err)
	}
}

func TestOutboundAcceptanceRationalQuantization(t *testing.T) {
	for _, tc := range []struct {
		ticks    int64
		num, den uint32
		want     int64
	}{
		{1, 1, 2000, 1}, {-1, 1, 2000, -1}, {2, 1, 2000, 1}, {-2, 1, 2000, -1}, {3, 1, 2000, 2}, {-3, 1, 2000, -2},
		{1, 1, 2001, 0}, {-1, 1, 2001, 0}, {1, 1, 1999, 1}, {-1, 1, 1999, -1},
		{1, 1, 60000, 0}, {30, 1, 60000, 1}, {-30, 1, 60000, -1}, {1001, 1, 60000, 17}, {-1001, 1, 60000, -17},
		{1, 1001, 60000, 17}, {-1, 1001, 60000, -17}, {math.MaxInt64, 1, 1000, math.MaxInt64}, {math.MinInt64, 1, 1000, math.MinInt64},
		{123, math.MaxUint32, math.MaxUint32, 123000},
	} {
		got, err := QuantizeOutboundMilliseconds(tc.ticks, media.TimeBase{Numerator: tc.num, Denominator: tc.den})
		if err != nil || got != tc.want {
			t.Fatalf("Q(%d,%d/%d)=%d,%v; нужно %d", tc.ticks, tc.num, tc.den, got, err, tc.want)
		}
	}
	for _, base := range []media.TimeBase{{}, {Numerator: 1, Denominator: 1}, {Numerator: math.MaxUint32, Denominator: 1}} {
		_, err := QuantizeOutboundMilliseconds(math.MaxInt64, base)
		var out *OutboundError
		if !errors.As(err, &out) || out.Kind != OutboundTiming {
			t.Fatalf("нет типизированной ошибки времени: %v", err)
		}
	}
}

func TestOutboundAcceptanceExactCodecBytesAndComposition(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		av1                              bool
		pts, dts                         int64
		stamp, audioStamp, recoveryStamp uint32
		cts                              []byte
	}{
		{"positive_ties", false, 1, -1, 0, 3, 1001, []byte{0, 0, 2}},
		{"negative_ties", false, -1, 1, 2, 3, 1001, []byte{255, 255, 254}},
		{"round_marks_before_difference", false, 2, 1, 0, 1, 999, []byte{0, 0, 0}},
		{"negative_half", false, -3, -1, 1, 4, 1002, []byte{255, 255, 255}},
		{"av1_exact", true, -1, -1, 0, 3, 1001, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &acceptanceOutboundMemory{}
			c, v, a := acceptanceOutboundLocal(t, m, tc.av1)
			videoAU := []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}
			if tc.av1 {
				videoAU = append(v.Headers()[4:], 0x1a, 1, 0x10)
			}
			audioAU := []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c}
			if err := c.WriteHeaders(context.Background(), v, a); err != nil {
				t.Fatal(err)
			}
			if m.wire.Len() != 0 {
				t.Fatal("заголовки записаны до проверки независимости")
			}
			result, err := c.WritePacket(context.Background(), acceptanceOutboundPacket(t, v, tc.pts, tc.dts, false, videoAU))
			if err != nil || !result.Independent || result.PayloadBytes != uint64(len(videoAU)) || result.Track != media.TrackVideo {
				t.Fatalf("%+v %v", result, err)
			}
			if _, err = c.WritePacket(context.Background(), acceptanceOutboundPacket(t, a, 3, 3, false, audioAU)); err != nil {
				t.Fatal(err)
			}
			r := bytes.NewReader(m.wire.Bytes())
			messages := make([]acceptanceWireMessage, 5)
			for i := range messages {
				messages[i], err = acceptanceReadMessage(r, 128)
				if err != nil {
					t.Fatal(err)
				}
			}
			metadata, err := amf0.Unmarshal(messages[0].body)
			if err != nil || len(metadata) != 3 || metadata[0] != "@setDataFrame" || metadata[1] != "onMetaData" {
				t.Fatalf("metadata: %v %v", metadata, err)
			}
			fields, ok := metadata[2].(amf0.ECMAArray)
			if !ok {
				t.Fatalf("тип metadata %T", metadata[2])
			}
			values := map[string]any{}
			for _, field := range fields {
				values[field.Key] = field.Value
			}
			wantCodec := float64(7)
			if tc.av1 {
				wantCodec = 1635135537
			}
			if values["videocodecid"] != wantCodec || values["audiocodecid"] != float64(10) || values["audiosamplerate"] != float64(48000) {
				t.Fatalf("кодек/частота metadata: %+v", values)
			}
			wantWidth, wantHeight := float64(128), float64(128)
			if tc.av1 {
				wantWidth, wantHeight = 1920, 804
			}
			if values["width"] != wantWidth || values["height"] != wantHeight || values["audiochannels"] != float64(2) || values["stereo"] != true {
				t.Fatalf("metadata размеров/каналов: %+v", values)
			}
			videoHeader := []byte{0x17, 0, 0, 0, 0}
			videoPrefix := append([]byte{0x17, 1}, tc.cts...)
			if tc.av1 {
				videoHeader = []byte{0x90, 'a', 'v', '0', '1'}
				videoPrefix = []byte{0x91, 'a', 'v', '0', '1'}
			}
			for i, want := range [][]byte{append(videoHeader, v.Headers()...), {0xaf, 0, 0x11, 0x90}, append(videoPrefix, videoAU...), append([]byte{0xaf, 1}, audioAU...)} {
				msg := messages[i+1]
				if !bytes.Equal(msg.body, want) || msg.stream != 0x765432 {
					t.Fatalf("message%d: body=%x stream=%x; want=%x", i+1, msg.body, msg.stream, want)
				}
			}
			if messages[3].stamp != tc.stamp || messages[4].stamp != tc.audioStamp {
				t.Fatalf("video time %d != %d", messages[3].stamp, tc.stamp)
			}
			before := m.wire.Len()
			vi, ai := v.Info(), a.Info()
			vi.Revision++
			ai.Revision++
			v2, err := media.NewCodecConfig(vi, v.Headers())
			if err != nil {
				t.Fatal(err)
			}
			a2, err := media.NewCodecConfig(ai, a.Headers())
			if err != nil {
				t.Fatal(err)
			}
			if err = c.WriteHeaders(context.Background(), v2, a2); err != nil {
				t.Fatal(err)
			}
			if _, err = c.WritePacket(context.Background(), acceptanceOutboundPacket(t, v2, 2000, 2000, false, videoAU)); err != nil {
				t.Fatal(err)
			}
			next := bytes.NewReader(m.wire.Bytes()[before:])
			for range 4 {
				msg, e := acceptanceReadMessage(next, 128)
				if e != nil || msg.stamp != tc.recoveryStamp {
					t.Fatalf("сброс конфигурации изменил начало отсчёта: %+v %v", msg, e)
				}
			}
		})
	}
}

func acceptanceSendAMF(w io.Writer, stream uint32, data amf0.Data) error {
	b, err := data.Marshal()
	if err != nil {
		return err
	}
	if len(b) > 128 {
		return errors.New("ответ oracle превышает один chunk")
	}
	h := []byte{3, 0, 0, 0, 0, 0, byte(len(b)), 20, byte(stream), byte(stream >> 8), byte(stream >> 16), byte(stream >> 24)}
	_, err = w.Write(append(h, b...))
	return err
}

func acceptanceStartup(peer net.Conn, reject string) error {
	if _, _, err := handshake.DoServer(peer, true); err != nil {
		return err
	}
	m, err := acceptanceReadMessage(peer, 128)
	if err != nil {
		return err
	}
	data, err := amf0.Unmarshal(m.body)
	if err != nil || len(data) != 3 || data[0] != "connect" || data[1] != float64(1) {
		return fmt.Errorf("connect: %v %v", data, err)
	}
	obj, ok := data[2].(amf0.Object)
	if !ok {
		return errors.New("connect object отсутствует")
	}
	app, _ := obj.GetString("app")
	tcURL, _ := obj.GetString("tcUrl")
	if app != "a%2Fb/../live?x=%2F&x=2" || (tcURL != "rtmp://receiver.test/a%2Fb/../live?x=%2F&x=2" && tcURL != "rtmps://receiver.test/a%2Fb/../live?x=%2F&x=2" && tcURL != "rtmps://wrong.test/a%2Fb/../live?x=%2F&x=2") {
		return fmt.Errorf("URL изменён: app=%s tcUrl=%s", app, tcURL)
	}
	code, transaction := "NetConnection.Connect.Success", float64(1)
	if reject == "connect_code" {
		code = "NetConnection.Connect.Rejected"
	}
	if reject == "connect_transaction" {
		transaction = 9
	}
	name := "_result"
	if reject == "connect_denied" {
		name, code = "_error", "NetConnection.Connect.Rejected"
	}
	if err = acceptanceSendAMF(peer, 0, amf0.Data{name, transaction, nil, amf0.Object{{Key: "level", Value: "status"}, {Key: "code", Value: code}}}); err != nil {
		return err
	}
	if reject == "connect_code" || reject == "connect_transaction" || reject == "connect_denied" {
		return nil
	}
	m, err = acceptanceReadMessage(peer, 128)
	if err != nil {
		return err
	}
	if m.typ != 1 || !bytes.Equal(m.body, []byte{0, 0, 0, 128}) {
		return errors.New("неверный SetChunkSize")
	}
	m, err = acceptanceReadMessage(peer, 128)
	if err != nil {
		return err
	}
	data, err = amf0.Unmarshal(m.body)
	if err != nil || len(data) != 3 || data[0] != "createStream" || data[1] != float64(2) {
		return errors.New("неверный createStream")
	}
	if err = acceptanceSendAMF(peer, 0, amf0.Data{"_result", float64(2), nil, float64(0x765432)}); err != nil {
		return err
	}
	m, err = acceptanceReadMessage(peer, 128)
	if err != nil {
		return err
	}
	data, err = amf0.Unmarshal(m.body)
	if err != nil || m.stream != 0x765432 || len(data) != 5 || data[0] != "publish" || data[1] != float64(0) || data[3] != "exact/key?#%2F" || data[4] != "live" {
		return fmt.Errorf("publish изменён: %v, stream=%x, %v", data, m.stream, err)
	}
	code, transaction = "NetStream.Publish.Start", 0
	stream := uint32(0x765432)
	if reject == "publish_code" {
		code = "NetStream.Publish.BadName"
	}
	if reject == "publish_transaction" {
		transaction = 1
	}
	if reject == "publish_stream" {
		stream = 0x345676
	}
	return acceptanceSendAMF(peer, stream, amf0.Data{"onStatus", transaction, nil, amf0.Object{{Key: "level", Value: "status"}, {Key: "code", Value: code}}})
}

func acceptancePipeOptions(conn net.Conn, l OutboundLimits, active *[4]atomic.Int32) OutboundOptions {
	return OutboundOptions{Limits: l, DialContext: func(context.Context, string, string) (net.Conn, error) { return conn, nil }, Admit: func(_ context.Context, op OutboundOperation) (func(), error) {
		if active[op].Add(1) != 1 {
			return nil, errors.New("одновременное занятие одного operation slot")
		}
		return func() { active[op].Add(-1) }, nil
	}}
}

func acceptanceTarget(t *testing.T, endpoint string) OutboundTarget {
	t.Helper()
	v, a := acceptanceOutboundConfigs(t, false, media.TimeBase{Numerator: 1, Denominator: 1000})
	target, err := ResolveOutbound(endpoint, "exact/key?#%2F", core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio})
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func TestOutboundAcceptanceExactBindingAndStrictSuccess(t *testing.T) {
	for _, reject := range []string{"", "connect_code", "connect_denied", "connect_transaction", "publish_code", "publish_transaction", "publish_stream"} {
		t.Run("status_"+reject, func(t *testing.T) {
			client, peer := net.Pipe()
			defer func() { _ = peer.Close() }()
			defer func() { _ = client.Close() }()
			serverResult := make(chan error, 1)
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				err := acceptanceStartup(peer, reject)
				serverResult <- err
				if err == nil {
					_, _ = io.Copy(io.Discard, peer)
				}
			}()
			var active [4]atomic.Int32
			opts := acceptancePipeOptions(client, acceptanceOutboundLimits(), &active)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, err := DialOutbound(ctx, acceptanceTarget(t, "rtmp://receiver.test/a%2Fb/../live?x=%2F&x=2"), opts)
			if reject == "" {
				if err != nil {
					t.Fatal(err)
				}
				if err = c.Close(); err != nil {
					t.Fatal(err)
				}
				if err = c.Wait(ctx); err != nil {
					t.Fatal(err)
				}
			} else if !IsOutboundTerminal(err) {
				t.Fatalf("неверный статус/транзакция принят либо отказ не окончательный: %v", err)
			}
			if reject == "connect_denied" || reject == "publish_code" {
				var out *OutboundError
				if !errors.As(err, &out) || out.Kind != OutboundDenied || out.Code == "" {
					t.Fatalf("потерян код окончательного отказа авторизации: %v", err)
				}
			}
			if err = <-serverResult; err != nil {
				t.Fatal(err)
			}
			_ = client.Close()
			select {
			case <-serverDone:
			case <-ctx.Done():
				t.Fatal("принимающий узел не завершился")
			}
			for op := 1; op <= 3; op++ {
				if active[op].Load() != 0 {
					t.Fatalf("операция %d не завершена", op)
				}
			}
		})
	}
}

func TestOutboundAcceptanceIdleAndFixedStartedMessageDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, peer := net.Pipe()
		defer func() { _ = peer.Close() }()
		defer func() { _ = client.Close() }()
		ready := make(chan error, 1)
		send := make(chan []byte)
		serverDone := make(chan struct{})
		go func() {
			defer close(serverDone)
			if err := acceptanceStartup(peer, ""); err != nil {
				ready <- err
				return
			}
			ready <- nil
			for b := range send {
				if _, err := peer.Write(b); err != nil {
					return
				}
			}
		}()
		defer close(send)
		var active [4]atomic.Int32
		l := acceptanceOutboundLimits()
		l.MessageTimeout = 100 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c, err := DialOutbound(ctx, acceptanceTarget(t, "rtmp://receiver.test/a%2Fb/../live?x=%2F&x=2"), acceptancePipeOptions(client, l, &active))
		if err != nil {
			t.Fatal(err)
		}
		if err = <-ready; err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		synctest.Wait()
		select {
		case <-c.Done():
			t.Fatalf("тишина завершила исправное соединение: %v", c.Err())
		default:
		}
		// В первом незавершённом сообщении принят один из двух байтов тела.
		send <- []byte{2, 0, 0, 0, 0, 0, 4, 1, 0, 0, 0, 0, 0, 0, 0, 1}
		synctest.Wait()
		send <- []byte{3, 0, 0, 0, 0, 0, 2, 20, 0, 0, 0, 0, 5}
		synctest.Wait()
		time.Sleep(60 * time.Millisecond)
		// Второй поток фрагментов не может продлить общий срок первого сообщения.
		send <- []byte{4}
		synctest.Wait()
		time.Sleep(40 * time.Millisecond)
		synctest.Wait()
		select {
		case <-c.Done():
		default:
			t.Fatal("фрагмент другого потока продлил срок первого сообщения")
		}
		var timeout net.Error
		if !errors.As(c.Err(), &timeout) || !timeout.Timeout() {
			t.Fatalf("ожидалось истечение сетевого срока: %v", c.Err())
		}
		join, cancelJoin := context.WithTimeout(context.Background(), time.Second)
		defer cancelJoin()
		if err = c.Wait(join); err != nil {
			t.Fatal(err)
		}
	})
}

func acceptanceCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(41), Subject: pkix.Name{CommonName: "synthetic receiver"}, DNSNames: []string{"receiver.test"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func TestOutboundAcceptanceTLSTrustAndHostname(t *testing.T) {
	cert, pool := acceptanceCertificate(t)
	for _, mode := range []string{"trusted", "untrusted", "wrong_hostname"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			serverDone := make(chan struct{})
			serverResult := make(chan error, 1)
			go func() {
				defer close(serverDone)
				rawPeer, err := listener.Accept()
				if err != nil {
					serverResult <- err
					return
				}
				defer func() { _ = rawPeer.Close() }()
				peer := tls.Server(rawPeer, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
				err = peer.Handshake()
				if err == nil && peer.ConnectionState().ServerName != "receiver.test" {
					err = errors.New("SNI не совпадает с target hostname")
				}
				if err == nil {
					err = acceptanceStartup(peer, "")
				}
				serverResult <- err
				if err == nil {
					_, _ = io.Copy(io.Discard, peer)
				}
			}()
			var active [4]atomic.Int32
			opts := acceptancePipeOptions(nil, acceptanceOutboundLimits(), &active)
			opts.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
			}
			cfg := &tls.Config{RootCAs: pool, ServerName: "unchanged.test", MinVersion: tls.VersionTLS12}
			if mode == "untrusted" {
				cfg.RootCAs = x509.NewCertPool()
			}
			opts.TLSConfig = cfg
			endpoint := "rtmps://receiver.test/a%2Fb/../live?x=%2F&x=2"
			if mode == "wrong_hostname" {
				endpoint = "rtmps://wrong.test/a%2Fb/../live?x=%2F&x=2"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, err := DialOutbound(ctx, acceptanceTarget(t, endpoint), opts)
			if mode == "trusted" {
				if err != nil {
					t.Fatal(err)
				}
				if err = c.Close(); err != nil {
					t.Fatal(err)
				}
				if err = c.Wait(ctx); err != nil {
					t.Fatal(err)
				}
			} else if !IsOutboundTerminal(err) {
				t.Fatalf("ошибка сертификата должна быть окончательной: %v", err)
			}
			if cfg.ServerName != "unchanged.test" || cfg.InsecureSkipVerify {
				t.Fatal("изменена конфигурация TLS вызывающего кода")
			}
			select {
			case <-serverDone:
			case <-ctx.Done():
				t.Fatal("принимающий узел TLS не завершился")
			}
			peerErr := <-serverResult
			if mode == "trusted" && peerErr != nil {
				t.Fatal(peerErr)
			}
			for op := 1; op <= 3; op++ {
				if active[op].Load() != 0 {
					t.Fatalf("операция TLS %d не завершена", op)
				}
			}
		})
	}
}

func TestOutboundAcceptanceLateDialFinishesAfterSocketClose(t *testing.T) {
	client, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	var active [4]atomic.Int32
	l := acceptanceOutboundLimits()
	l.DialTimeout = time.Millisecond
	opts := acceptancePipeOptions(client, l, &active)
	opts.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return client, nil }
	_, err := DialOutbound(context.Background(), acceptanceTarget(t, "rtmp://receiver.test/a%2Fb/../live?x=%2F&x=2"), opts)
	if !errors.Is(err, context.DeadlineExceeded) || active[OutboundEstablishment].Load() != 0 {
		t.Fatalf("позднее завершение подключения: %v", err)
	}
	if _, err = peer.Write([]byte{1}); err == nil {
		t.Fatal("позднее соединение осталось открытым")
	}
}

func TestOutboundAcceptancePreflightRejectsBeforeNetwork(t *testing.T) {
	target := acceptanceTarget(t, "rtmp://receiver.test/a%2Fb/../live?x=%2F&x=2")
	for _, mode := range []string{"missing_admit", "invalid_limits", "insecure_tls", "invalid_target", "denied_admission"} {
		t.Run(mode, func(t *testing.T) {
			called := false
			opts := OutboundOptions{Limits: acceptanceOutboundLimits(), DialContext: func(context.Context, string, string) (net.Conn, error) {
				called = true
				return nil, errors.New("неожиданная сеть")
			}, Admit: func(context.Context, OutboundOperation) (func(), error) { return func() {}, nil }}
			current := target
			switch mode {
			case "missing_admit":
				opts.Admit = nil
				if err := opts.Validate(); err != nil {
					t.Fatal("Prepare должен позволять отсутствие исполняемого Admit", err)
				}
			case "invalid_limits":
				opts.Limits.MessageTimeout = 0
			case "insecure_tls":
				opts.TLSConfig = &tls.Config{InsecureSkipVerify: true}
			case "invalid_target":
				current = OutboundTarget{}
			case "denied_admission":
				opts.Admit = func(context.Context, OutboundOperation) (func(), error) { return nil, core.ErrStale }
			}
			_, err := DialOutbound(context.Background(), current, opts)
			if called || err == nil {
				t.Fatalf("сеть до проверки и допуска: called=%v err=%v", called, err)
			}
			if mode == "denied_admission" {
				if !errors.Is(err, core.ErrStale) {
					t.Fatal(err)
				}
			} else {
				var out *OutboundError
				if !errors.As(err, &out) || out.Kind != OutboundInvalid {
					t.Fatalf("нет типизированного OutboundInvalid: %v", err)
				}
			}
		})
	}
	for _, endpoint := range []string{"rtmp://user:pass@receiver.test/app", "rtmp://receiver.test/app#", "rtmp://receiver.test/app#key", "rtmp://receiver.test:0/app", "rtmps://receiver.test:/app"} {
		if _, err := ResolveOutbound(endpoint, "key", target.profile); !IsOutboundTerminal(err) {
			t.Fatalf("недопустимая привязка принята: %q %v", endpoint, err)
		}
	}
}

func TestOutboundAcceptanceTimestampWrapAndSignedCompositionLimits(t *testing.T) {
	m := &acceptanceOutboundMemory{}
	c, _, _ := acceptanceOutboundLocal(t, m, false)
	v, a := acceptanceOutboundConfigs(t, false, media.TimeBase{Numerator: 1, Denominator: 1000})
	if err := c.WriteHeaders(context.Background(), v, a); err != nil {
		t.Fatal(err)
	}
	var err error
	for _, ticks := range []int64{0, 2147483647, 4294967294, 4294967300} {
		if _, err = c.WritePacket(context.Background(), acceptanceOutboundPacket(t, v, ticks, ticks, false, []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40})); err != nil {
			t.Fatal(err)
		}
	}
	r := bytes.NewReader(m.wire.Bytes())
	for range 3 {
		if _, err = acceptanceReadMessage(r, 128); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []uint32{0, 0x7fffffff, 0xfffffffe, 4} {
		msg, e := acceptanceReadMessage(r, 128)
		if e != nil || msg.stamp != want {
			t.Fatalf("wire timestamp=%x want=%x %v", msg.stamp, want, e)
		}
	}
	_, err = c.WritePacket(context.Background(), acceptanceOutboundPacket(t, v, 6442450948, 6442450948, false, []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}))
	var out *OutboundError
	if !errors.As(err, &out) || out.Kind != OutboundTiming {
		t.Fatalf("неоднозначный скачок последовательного номера принят: %v", err)
	}
	for _, tc := range []struct {
		cts    int64
		prefix []byte
		valid  bool
	}{
		{-8388608, []byte{0x17, 1, 0x80, 0, 0}, true}, {8388607, []byte{0x17, 1, 0x7f, 255, 255}, true}, {-8388609, nil, false}, {8388608, nil, false},
	} {
		t.Run(fmt.Sprintf("cts_%d", tc.cts), func(t *testing.T) {
			m := &acceptanceOutboundMemory{}
			c, _, _ := acceptanceOutboundLocal(t, m, false)
			if err := c.WriteHeaders(context.Background(), v, a); err != nil {
				t.Fatal(err)
			}
			_, err := c.WritePacket(context.Background(), acceptanceOutboundPacket(t, v, tc.cts, 0, false, []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}))
			if !tc.valid {
				if !errors.As(err, &out) || out.Kind != OutboundTiming || m.wire.Len() != 0 {
					t.Fatalf("непредставимый CTS записан: %x %v", m.wire.Bytes(), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r := bytes.NewReader(m.wire.Bytes())
			for range 3 {
				if _, err = acceptanceReadMessage(r, 128); err != nil {
					t.Fatal(err)
				}
			}
			msg, e := acceptanceReadMessage(r, 128)
			if e != nil || !bytes.HasPrefix(msg.body, tc.prefix) {
				t.Fatalf("signed24=%x want=%x %v", msg.body, tc.prefix, e)
			}
		})
	}
}

func TestOutboundAcceptanceInvalidAccessUnitsNeverBecomeSent(t *testing.T) {
	for _, mode := range []string{"forged_keyframe", "aac_reorder", "av1_reorder"} {
		t.Run(mode, func(t *testing.T) {
			m := &acceptanceOutboundMemory{}
			c, v, a := acceptanceOutboundLocal(t, m, mode == "av1_reorder")
			if err := c.WriteHeaders(context.Background(), v, a); err != nil {
				t.Fatal(err)
			}
			cfg, pts, dts, payload, key := v, int64(0), int64(0), []byte{0, 0, 0, 3, 0x41, 0xb8, 0x40}, true
			if mode == "aac_reorder" {
				if _, err := c.WritePacket(context.Background(), acceptanceOutboundPacket(t, v, 0, 0, false, []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40})); err != nil {
					t.Fatal(err)
				}
				cfg, pts, payload, key = a, 1, []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c}, false
			}
			if mode == "av1_reorder" {
				pts = 1
				payload = append(v.Headers()[4:], 0x1a, 1, 0x10)
			}
			before := m.wire.Len()
			result, err := c.WritePacket(context.Background(), acceptanceOutboundPacket(t, cfg, pts, dts, key, payload))
			if !IsOutboundTerminal(err) || result.PayloadBytes != 0 || m.wire.Len() != before {
				t.Fatalf("неверный AU принят: %+v err=%v wire delta=%d", result, err, m.wire.Len()-before)
			}
		})
	}
}

func TestOutboundAcceptancePartialWritesCannotRenewWholeMessageDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, peer := net.Pipe()
		defer func() { _ = peer.Close() }()
		c, _, _ := acceptanceOutboundLocal(t, client, false)
		c.opts.Limits.WriteTimeout = 100 * time.Millisecond
		serverDone := make(chan struct{})
		go func() {
			defer close(serverDone)
			var b [3]byte
			for {
				if _, err := io.ReadFull(peer, b[:]); err != nil {
					return
				}
				time.Sleep(60 * time.Millisecond)
			}
		}()
		start := time.Now()
		err := c.writeMessage(context.Background(), 6, 9, 0x765432, 0, bytes.Repeat([]byte{7}, 257))
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() || time.Since(start) != 100*time.Millisecond || c.Snapshot().WrittenBytes != 6 {
			t.Fatalf("срок полной записи сообщения: elapsed=%s stats=%+v err=%v", time.Since(start), c.Snapshot(), err)
		}
		<-serverDone
	})
}
