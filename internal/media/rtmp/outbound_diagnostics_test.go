package rtmp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/handshake"
)

func diagnosticFrame(csid, typ byte, stream uint32, body []byte) []byte {
	h := []byte{csid, 0, 0, 0, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body)), typ, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(h[8:], stream)
	return append(h, body...)
}

func diagnosticAMF(t *testing.T, data amf0.Data) []byte {
	t.Helper()
	b, err := data.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// diagnosticReadLoop исполняет настоящий парсер и диспетчер без фоновых записей.
func diagnosticReadLoop(t *testing.T, wire []byte, setup func(*OutboundConn)) (*OutboundConn, error) {
	t.Helper()
	c := transportSelfConn(t, &transportSelfMemory{input: bytes.NewReader(wire)})
	c.readerDone = make(chan struct{})
	c.commands = make(chan outboundCommand, 4)
	if setup != nil {
		setup(c)
	}
	c.readLoop()
	select {
	case <-c.readerDone:
	default:
		t.Fatal("читатель не подтвердил завершение")
	}
	return c, c.Err()
}

func diagnosticProtocol(t *testing.T, err error, cause error, fields ...string) {
	t.Helper()
	var out *OutboundError
	if !errors.Is(err, cause) || !errors.As(err, &out) || out.Kind != OutboundProtocol || !IsOutboundTerminal(err) {
		t.Fatalf("потеряна причина или классификация protocol: %v", err)
	}
	for _, field := range fields {
		found := false
		for _, token := range strings.Fields(err.Error()) {
			if strings.TrimSuffix(token, ":") == field {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("в диагностике нет %q: %v", field, err)
		}
	}
}

func TestOutboundDiagnosticsChunkFailures(t *testing.T) {
	partial := diagnosticFrame(4, 20, 0x01020304, bytes.Repeat([]byte{5}, 129))
	partial = partial[:12+128]
	extended := bytes.Clone(partial)
	copy(extended[1:4], []byte{255, 255, 255})
	extended = append(extended[:12], append([]byte{1, 0, 0, 2}, extended[12:]...)...)
	for _, tc := range []struct {
		name   string
		wire   []byte
		fields []string
	}{
		{"unknown_compressed_header", []byte{0xc4}, []string{"phase=chunk_header", "csid=4", "fmt=3", "known=false", "active=false"}},
		{"new_header_during_message", append(bytes.Clone(partial), 4), []string{"phase=chunk_header", "csid=4", "fmt=0", "active=true", "received=128", "message_len=129"}},
		{"mismatched_extended_timestamp", append(bytes.Clone(extended), 0xc4, 1, 0, 0, 3), []string{"phase=extended_timestamp", "csid=4", "fmt=3", "extended_timestamp=16777219", "expected_extended_timestamp=16777218"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := diagnosticReadLoop(t, tc.wire, nil)
			diagnosticProtocol(t, err, ErrFraming, tc.fields...)
		})
	}
}

func TestOutboundDiagnosticsControlFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		csid, typ byte
		stream    uint32
		body      []byte
		fields    []string
	}{
		{"wrong_chunk_stream", 7, 3, 0, []byte{0, 0, 0, 0}, []string{"csid=7", "stream_id=0"}},
		{"wrong_message_stream", 2, 3, 23, []byte{0, 0, 0, 0}, []string{"csid=2", "stream_id=23"}},
		{"short_chunk_size", 2, 1, 0, []byte{0, 1, 0}, nil},
		{"short_abort", 2, 2, 0, []byte{0}, nil},
		{"invalid_abort_stream", 2, 2, 0, []byte{0, 0, 0, 1}, []string{"abort_csid=1"}},
		{"short_ack", 2, 3, 0, []byte{1}, nil},
		{"short_user_event", 2, 4, 0, []byte{0}, nil},
		{"wrong_event_length", 2, 4, 0, []byte{0, 3, 0, 0, 0, 1}, []string{"event=3", "expected_body_len=10"}},
		{"unknown_user_event", 2, 4, 0, []byte{0, 99, 0, 0, 0, 0}, []string{"event=99"}},
		{"zero_ack_window", 2, 5, 0, []byte{0, 0, 0, 0}, []string{"window=0"}},
		{"invalid_bandwidth_kind", 2, 6, 0, []byte{0, 0, 0, 1, 3}, []string{"window=1", "limit_type=3"}},
		{"zero_bandwidth_window", 2, 6, 0, []byte{0, 0, 0, 0, 0}, []string{"window=0", "limit_type=0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := diagnosticReadLoop(t, diagnosticFrame(tc.csid, tc.typ, tc.stream, tc.body), nil)
			fields := []string{"phase=control", fmt.Sprintf("type=%d", tc.typ), fmt.Sprintf("body_len=%d", len(tc.body))}
			diagnosticProtocol(t, err, ErrFraming, append(fields, tc.fields...)...)
		})
	}
}

func TestOutboundDiagnosticsAcknowledgementValues(t *testing.T) {
	for _, tc := range []struct {
		name           string
		previous, sent uint64
		ack            uint32
		want           uint64
		events         uint64
	}{
		{"future", 100, 120, 124, 124, 1},
		{"backward", 100, 120, 99, 100, 0},
		{"half_range", 100, (1 << 31) + 100, (1 << 31) + 100, 100, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body [4]byte
			binary.BigEndian.PutUint32(body[:], tc.ack)
			c, err := diagnosticReadLoop(t, diagnosticFrame(2, 3, 0, body[:]), func(c *OutboundConn) {
				c.peerAcknowledged, c.stats.WrittenBytes = tc.previous, tc.sent
			})
			if !errors.Is(err, io.EOF) || IsOutboundTerminal(err) || c.peerAcknowledged != tc.want || c.Snapshot().AckAheadEventsTotal != tc.events {
				t.Fatalf("числовой ACK нарушил диагностику: ack=%d stats=%+v err=%v", c.peerAcknowledged, c.Snapshot(), err)
			}
		})
	}
}

func TestOutboundDiagnosticsAMFFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   []byte
		cause  error
		fields []string
	}{
		{"truncated_string", []byte{2, 0, 5, 'x'}, ErrFraming, []string{"phase=amf_preflight"}},
		{"element_limit", []byte{10, 0xff, 0xff, 0xff, 0xff}, ErrLimit, []string{"phase=amf_preflight"}},
		{"short_command", diagnosticAMF(t, amf0.Data{"_result", float64(1)}), ErrFraming, []string{"phase=command_shape", "values_len=2"}},
		{"numeric_command_name", diagnosticAMF(t, amf0.Data{float64(9), float64(1), nil}), ErrFraming, []string{"phase=command_name", "value_type=float64"}},
		{"string_transaction", diagnosticAMF(t, amf0.Data{"_result", "invalid", nil}), ErrFraming, []string{"phase=command_transaction", "value_type=string"}},
		{"nan_transaction", diagnosticAMF(t, amf0.Data{"_result", math.NaN(), nil}), ErrFraming, []string{"phase=command_transaction", "transaction=NaN"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := diagnosticReadLoop(t, diagnosticFrame(11, 20, 73, tc.body), nil)
			fields := append([]string{"type=20", "csid=11", "stream_id=73", fmt.Sprintf("body_len=%d", len(tc.body))}, tc.fields...)
			diagnosticProtocol(t, err, tc.cause, fields...)
		})
	}
}

func TestOutboundDiagnosticsUnexpectedMessages(t *testing.T) {
	t.Run("unexpected_type", func(t *testing.T) {
		_, err := diagnosticReadLoop(t, diagnosticFrame(9, 18, 55, []byte{5}), nil)
		diagnosticProtocol(t, err, ErrFraming, "phase=message_type", "type=18", "csid=9", "stream_id=55", "body_len=1")
	})
	t.Run("command_after_publish", func(t *testing.T) {
		start := diagnosticAMF(t, amf0.Data{"onStatus", float64(0), nil, amf0.Object{{Key: "code", Value: "NetStream.Publish.Start"}}})
		extra := diagnosticAMF(t, amf0.Data{"_result", float64(17), nil})
		wire := append(diagnosticFrame(3, 20, 0x01020304, start), diagnosticFrame(12, 20, 0x01020304, extra)...)
		c, err := diagnosticReadLoop(t, wire, nil)
		diagnosticProtocol(t, err, ErrFraming, "phase=post_publish", "command=\"_result\"", "transaction=17", "csid=12", "stream_id=16909060")
		if len(c.commands) != 1 {
			t.Fatal("лишняя команда передана как ответ установки соединения")
		}
	})
}

func TestOutboundDiagnosticsLargeChunkStreamIDs(t *testing.T) {
	start := diagnosticAMF(t, amf0.Data{"onStatus", float64(0), nil, amf0.Object{{Key: "code", Value: "NetStream.Publish.Start"}}})
	for _, tc := range []struct {
		name   string
		typ    byte
		stream uint32
		body   []byte
		prefix []byte
		phase  string
	}{
		{"control_address", 3, 0, []byte{0, 0, 0, 0}, nil, "control"},
		{"unexpected_type", 18, 55, []byte{5}, nil, "message_type"},
		{"malformed_amf", 20, 55, []byte{2, 0, 5, 'x'}, nil, "amf_preflight"},
		{"post_publish", 20, 0x01020304, diagnosticAMF(t, amf0.Data{"_result", float64(17), nil}), diagnosticFrame(3, 20, 0x01020304, start), "post_publish"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Трёхбайтовый базовый заголовок кодирует csid=64+0+256=320.
			wire := append([]byte{1, 0, 1}, diagnosticFrame(2, tc.typ, tc.stream, tc.body)[1:]...)
			_, err := diagnosticReadLoop(t, append(bytes.Clone(tc.prefix), wire...), nil)
			diagnosticProtocol(t, err, ErrFraming, "phase="+tc.phase, "csid=320", fmt.Sprintf("type=%d", tc.typ), fmt.Sprintf("stream_id=%d", tc.stream))
		})
	}
}

func TestOutboundDiagnosticsValidControls(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  byte
		body []byte
	}{
		{"minimum_chunk", 1, []byte{0, 0, 0, 1}},
		{"maximum_chunk", 1, []byte{0, 1, 0, 0}},
		{"abort_minimum_id", 2, []byte{0, 0, 0, 2}},
		{"abort_maximum_id", 2, []byte{0, 1, 0, 63}},
		{"stream_begin", 4, []byte{0, 0, 0, 0, 0, 37}},
		{"set_buffer_length", 4, []byte{0, 3, 0, 0, 0, 37, 0, 0, 1, 0}},
		{"ping_request", 4, []byte{0, 6, 1, 2, 3, 4}},
		{"minimum_ack_window", 5, []byte{0, 0, 0, 1}},
		{"maximum_peer_window", 6, []byte{0xff, 0xff, 0xff, 0xff, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := diagnosticReadLoop(t, diagnosticFrame(2, tc.typ, 0, tc.body), nil)
			if !errors.Is(err, io.EOF) || IsOutboundTerminal(err) {
				t.Fatalf("допустимое управление отклонено: %v", err)
			}
			if tc.name == "ping_request" {
				select {
				case response := <-c.controls:
					if response.typ != 4 || !bytes.Equal(response.body, []byte{0, 7, 1, 2, 3, 4}) {
						t.Fatalf("изменён ответ PingResponse: %+v", response)
					}
				default:
					t.Fatal("ответ PingResponse не поставлен в очередь")
				}
			}
		})
	}
}

type diagnosticFailedRead struct {
	*transportSelfMemory
	err error
}

func (c diagnosticFailedRead) Read([]byte) (int, error) { return 0, c.err }

func TestOutboundDiagnosticsNetworkFailuresPreserveClassification(t *testing.T) {
	for _, original := range []error{io.ErrUnexpectedEOF, context.Canceled, context.DeadlineExceeded, &net.DNSError{Err: "синтетический таймаут", IsTimeout: true}} {
		t.Run(fmt.Sprintf("%T_%v", original, original), func(t *testing.T) {
			c := transportSelfConn(t, diagnosticFailedRead{transportSelfMemory: &transportSelfMemory{}, err: original})
			c.readerDone = make(chan struct{})
			c.readLoop()
			if !errors.Is(c.Err(), original) || IsOutboundTerminal(c.Err()) || errors.Is(c.Err(), ErrFraming) {
				t.Fatalf("транспортная причина стала протокольным отказом: %v", c.Err())
			}
		})
	}
}

func TestOutboundDiagnosticsValidBoundariesAndNetworkEOF(t *testing.T) {
	t.Run("matching_extended_timestamp", func(t *testing.T) {
		wire := []byte{4, 255, 255, 255, 0, 0, 2, 20, 4, 3, 2, 1, 1, 0, 0, 2, 5, 0xc4, 1, 0, 0, 2, 6}
		c := transportSelfConn(t, &transportSelfMemory{input: bytes.NewReader(wire)})
		r := outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 1}
		m, err := r.next()
		if err != nil || m == nil || !bytes.Equal(m.Body, []byte{5, 6}) || m.Timestamp != 16777218*time.Millisecond || m.MessageStreamID != 0x01020304 {
			t.Fatalf("корректное продолжение изменено: %+v %v", m, err)
		}
	})
	for _, tc := range []struct {
		name     string
		previous uint64
		sent     uint64
		flight   uint64
		ack      uint32
		want     uint64
	}{
		{"duplicate_ack", 100, 120, 0, 100, 100},
		{"ack_in_flight_boundary", 100, 120, 3, 123, 123},
		{"ack_wrap", (1 << 32) - 8, (1 << 32) + 8, 0, 8, (1 << 32) + 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body [4]byte
			binary.BigEndian.PutUint32(body[:], tc.ack)
			c, err := diagnosticReadLoop(t, diagnosticFrame(2, 3, 0, body[:]), func(c *OutboundConn) {
				c.peerAcknowledged, c.stats.WrittenBytes = tc.previous, tc.sent
				c.writeActive = tc.flight > 0
			})
			if !errors.Is(err, io.EOF) || IsOutboundTerminal(err) || c.peerAcknowledged != tc.want {
				t.Fatalf("допустимый ACK отклонён: ack=%d err=%v", c.peerAcknowledged, err)
			}
		})
	}
	for _, wire := range [][]byte{nil, {4}, {4, 0, 0}, {4, 0, 0, 0, 0, 0, 4, 20, 0, 0, 0, 0, 5}} {
		t.Run(fmt.Sprintf("truncated_%d", len(wire)), func(t *testing.T) {
			_, err := diagnosticReadLoop(t, wire, nil)
			if !errors.Is(err, io.EOF) || errors.Is(err, ErrFraming) || IsOutboundTerminal(err) {
				t.Fatalf("EOF ошибочно стал протокольным отказом: %v", err)
			}
		})
	}
}

func diagnosticStartupPeer(peer net.Conn, stage string) error {
	if _, _, err := handshake.DoServer(peer, true); err != nil {
		return err
	}
	if _, err := transportSelfReadFrame(peer, 128); err != nil {
		return err
	}
	transaction := float64(1)
	if stage == "connect" {
		transaction = 9
	}
	if err := transportSelfWriteAMF(peer, amf0.Data{"_result", transaction, nil, amf0.Object{{Key: "code", Value: "NetConnection.Connect.Success"}}}, 0); err != nil || stage == "connect" {
		return err
	}
	f, err := transportSelfReadFrame(peer, 128)
	if err != nil {
		return err
	}
	if f.typ != 1 || len(f.body) != 4 {
		return errors.New("не получен SetChunkSize")
	}
	chunkSize := int(binary.BigEndian.Uint32(f.body))
	if _, err := transportSelfReadFrame(peer, chunkSize); err != nil {
		return err
	}
	id := float64(37)
	if stage == "create_stream_id" {
		id = 1.5
	}
	transaction = 2
	if stage == "create_stream" {
		transaction = 19
	}
	if err := transportSelfWriteAMF(peer, amf0.Data{"_result", transaction, nil, id}, 0); err != nil || stage == "create_stream_id" || stage == "create_stream" {
		return err
	}
	if _, err := transportSelfReadFrame(peer, chunkSize); err != nil {
		return err
	}
	transaction = 0
	if stage == "publish" {
		transaction = 29
	}
	return transportSelfWriteAMF(peer, amf0.Data{"onStatus", transaction, nil, amf0.Object{{Key: "code", Value: "NetStream.Publish.Start"}}}, 37)
}

func TestOutboundDiagnosticsStartupFailures(t *testing.T) {
	for _, tc := range []struct {
		stage  string
		fields []string
	}{
		{"connect", []string{"phase=connect", "transaction=9", "command=\"_result\"", "status_code=\"NetConnection.Connect.Success\""}},
		{"create_stream", []string{"phase=createStream", "transaction=19", "command=\"_result\""}},
		{"create_stream_id", []string{"phase=createStream", "stream_id_value=1.5"}},
		{"publish", []string{"phase=publish", "transaction=29", "stream_id=37", "status_code=\"NetStream.Publish.Start\""}},
		{"valid", nil},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			client, peer := net.Pipe()
			peerDone := make(chan error, 1)
			joined := make(chan struct{})
			t.Cleanup(func() {
				_ = client.Close()
				_ = peer.Close()
				select {
				case <-joined:
				case <-time.After(3 * time.Second):
					t.Error("синтетический RTMP peer не завершился")
				}
			})
			if err := peer.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			go func() {
				defer close(joined)
				err := diagnosticStartupPeer(peer, tc.stage)
				peerDone <- err
				if err == nil {
					_, _ = io.Copy(io.Discard, peer)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			target := transportSelfConn(t, &transportSelfMemory{}).target
			opts := OutboundOptions{Limits: transportSelfLimits(), DialContext: func(context.Context, string, string) (net.Conn, error) { return client, nil }, Admit: func(context.Context, OutboundOperation) (func(), error) { return func() {}, nil }}
			c, err := DialOutbound(ctx, target, opts)
			if tc.stage == "valid" {
				if err != nil || c.streamID.Load() != 37 {
					t.Fatalf("допустимая публикация отклонена: %v", err)
				}
				_ = c.Close()
				if err := c.Wait(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				diagnosticProtocol(t, err, ErrFraming, tc.fields...)
			}
			if err := <-peerDone; err != nil {
				t.Fatalf("ошибка синтетического peer: %v", err)
			}
		})
	}
}
