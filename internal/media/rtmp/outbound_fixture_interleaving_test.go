package rtmp

import (
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/handshake"
)

func TestOutboundStartupPeerAcceptsControlInterleaving(t *testing.T) {
	for _, stage := range []string{"before_chunk_size", "before_create_stream", "before_publish"} {
		t.Run(stage, func(t *testing.T) {
			peerErr, clientErr := transportSelfScriptStartup(t, stage, 5, []byte{0, 0, 1, 0}, 0, 2)
			if peerErr != nil || clientErr != nil {
				t.Fatalf("допустимое управление между командами: peer=%v client=%v", peerErr, clientErr)
			}
		})
	}
}

func TestOutboundStartupPeerRejectsInvalidInterleaving(t *testing.T) {
	for _, tc := range []struct {
		name        string
		typ         byte
		body        []byte
		stream      uint32
		chunkStream byte
	}{
		{"unknown_type", 9, []byte{0, 0, 1, 0}, 0, 2},
		{"short_window", 5, []byte{1}, 0, 2},
		{"short_ack", 3, []byte{1}, 0, 2},
		{"zero_window", 5, []byte{0, 0, 0, 0}, 0, 2},
		{"nonzero_window_stream", 5, []byte{0, 0, 1, 0}, 1, 2},
		{"nonzero_ack_stream", 3, []byte{0, 0, 12, 1}, 1, 2},
		{"wrong_chunk_stream", 5, []byte{0, 0, 1, 0}, 0, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peerErr, _ := transportSelfScriptStartup(t, "before_create_stream", tc.typ, tc.body, tc.stream, tc.chunkStream)
			if !errors.Is(peerErr, ErrFraming) {
				t.Fatalf("некорректное управление принято: %v", peerErr)
			}
		})
	}
}

// transportSelfScriptStartup задаёт порядок сообщений без зависимости от планировщика.
func transportSelfScriptStartup(t *testing.T, stage string, typ byte, body []byte, stream uint32, chunkStream byte) (error, error) {
	t.Helper()
	client, peer := net.Pipe()
	defer func() { _ = client.Close(); _ = peer.Close() }()
	deadline := time.Now().Add(time.Second)
	if err := client.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := peer.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	peerDone := make(chan error, 1)
	go func() {
		err := transportSelfStartupWindow(peer, "key", 256)
		_ = peer.Close()
		peerDone <- err
	}()
	run := func() error {
		if _, _, err := handshake.DoClient(client, false, false); err != nil {
			return err
		}
		if err := transportSelfWriteAMF(client, amf0.Data{"connect", float64(1), nil}, 0); err != nil {
			return err
		}
		for _, want := range []byte{6, 3, 20} {
			f, err := transportSelfReadFrame(client, 128)
			if err != nil {
				return err
			}
			if f.typ != want {
				return ErrFraming
			}
		}
		send := func(at string) error {
			if at != stage {
				return nil
			}
			h := []byte{chunkStream, 0, 0, 0, 0, 0, byte(len(body)), typ, 0, 0, 0, 0}
			binary.LittleEndian.PutUint32(h[8:], stream)
			if _, err := client.Write(h); err != nil {
				return err
			}
			if _, err := client.Write(body); err != nil {
				return err
			}
			// ACK допустим в том же месте, что и WindowAckSize.
			return transportSelfWriteControl(client, 3, []byte{0, 0, 12, 1})
		}
		if err := send("before_chunk_size"); err != nil {
			return err
		}
		if err := transportSelfWriteControl(client, 1, []byte{0, 0, 16, 0}); err != nil {
			return err
		}
		if err := send("before_create_stream"); err != nil {
			return err
		}
		if err := transportSelfWriteAMF(client, amf0.Data{"createStream", float64(2), nil}, 0); err != nil {
			return err
		}
		if _, err := transportSelfReadFrame(client, 128); err != nil {
			return err
		}
		if err := send("before_publish"); err != nil {
			return err
		}
		if err := transportSelfWriteAMF(client, amf0.Data{"publish", float64(0), nil, "key", "live"}, 0x01020304); err != nil {
			return err
		}
		f, err := transportSelfReadFrame(client, 128)
		if err != nil {
			return err
		}
		data, err := amf0.Unmarshal(f.body)
		if err != nil || f.typ != 20 || f.stream != 0x01020304 || len(data) < 1 || data[0] != "onStatus" {
			return ErrFraming
		}
		return nil
	}
	clientErr := run()
	_ = client.Close()
	return <-peerDone, clientErr
}
