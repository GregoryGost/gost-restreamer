package rtmp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/bytecounter"
	"github.com/bluenviron/gortmplib/pkg/message"
)

func TestContractWireConfigBudgetsPrecedeBodyAllocation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		typ      byte
		prefix   []byte
		consumed int
	}{
		{"av1", 9, []byte{0x90, 'a', 'v', '0', '1'}, 13},
		{"legacy-aac", 8, []byte{0xaf, 0, 0x11, 0x90, 0}, 17},
		{"extended-aac", 8, []byte{0x90, 'm', 'p', '4', 'a'}, 17},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := acceptanceWireLimits()
			declared := limits.MaxConfigBytes + 1
			raw := chunk0(4, tc.typ, tc.prefix)
			raw[4], raw[5], raw[6] = byte(declared>>16), byte(declared>>8), byte(declared)
			w, conn := acceptanceWire(t, raw, limits)
			got, err := io.ReadAll(w)
			if !errors.Is(err, ErrLimit) || len(got) != 0 || conn.readBytes > tc.consumed || len(w.streams[4].capture) != 0 {
				t.Fatalf("бюджет конфигурации проверен после чтения или выделения тела: read=%d exposed=%d capture=%d err=%v", conn.readBytes, len(got), len(w.streams[4].capture), err)
			}
		})
	}
}

func TestContractWireReadAheadAndCaptureBudgetsPrecedePayload(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(map[bool]string{false: "read-ahead", true: "capture"}[capture], func(t *testing.T) {
			limits := acceptanceWireLimits()
			body := append([]byte{0x90, 'a', 'v', '0', '1'}, testAV1Config()...)
			wantRead := 12
			if capture {
				limits.MaxRetainedBytes = limits.baseRetained() + 512 + uint64(len(body)) - 1
				wantRead++
			} else {
				limits.MaxReadAheadBytes = 18
			}
			w, conn := acceptanceWire(t, chunk0(3, 9, body), limits)
			got, err := io.ReadAll(w)
			if !errors.Is(err, ErrLimit) || len(got) != 0 || conn.readBytes > wantRead || len(w.streams[3].capture) != 0 {
				t.Fatalf("тело выделено или прочитано сверх бюджета: read=%d exposed=%d capture=%d err=%v", conn.readBytes, len(got), len(w.streams[3].capture), err)
			}
		})
	}
}

func TestContractWireExtendedAACConfigSurvivesOneByteChunks(t *testing.T) {
	p, sub := newContractPublication(t)
	config := contractAV1Config(contractSDRSequence())
	raw := chunk0(2, 1, []byte{0, 0, 0, 1})
	raw = append(raw, fragmented(6, 9, append([]byte{0x90, 'a', 'v', '0', '1'}, config...), 1)...)
	raw = append(raw, fragmented(4, 8, []byte{0x90, 'm', 'p', '4', 'a', 0x11, 0x90}, 1)...)
	raw = append(raw, fragmented(6, 9, append([]byte{0x91, 'a', 'v', '0', '1'}, contractAV1TU(config)...), 1)...)
	raw = append(raw, fragmented(4, 8, []byte{0x91, 'm', 'p', '4', 'a', 0xab, 0xcd}, 1)...)
	if err := contractConsume(t, p, raw); err != nil {
		t.Fatal(err)
	}
	v, a := delivery(t, sub), delivery(t, sub)
	if !bytes.Equal(v.VideoConfig.Headers(), config) || !bytes.Equal(a.AudioConfig.Headers(), []byte{0x11, 0x90}) || !bytes.Equal(a.Packet.Payload(), []byte{0xab, 0xcd}) || !v.Packet.Info().Keyframe {
		t.Fatal("разбиение префикса уничтожило конфигурацию AAC/AV1 либо полезные данные")
	}
}

func TestContractWireRejectsUnsupportedCodecsBeforeRemainingPayload(t *testing.T) {
	for _, tc := range []struct {
		name     string
		typ      byte
		prefix   []byte
		consumed int
	}{
		{"legacy-video", 9, []byte{0x17}, 13},
		{"video-multitrack", 9, []byte{0x96}, 13},
		{"non-av1", 9, []byte{0x91, 'h', 'v', 'c', '1'}, 17},
		{"non-aac", 8, []byte{0x2f}, 13},
		{"audio-multitrack", 8, []byte{0x95}, 13},
		{"non-mp4a", 8, []byte{0x91, 'O', 'p', 'u', 's'}, 17},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := chunk0(3, tc.typ, append(bytes.Clone(tc.prefix), bytes.Repeat([]byte{0xee}, 64)...))
			w, conn := acceptanceWire(t, raw, acceptanceWireLimits())
			got, err := io.ReadAll(w)
			if !errors.Is(err, ErrCodec) || len(got) != 0 || conn.readBytes > tc.consumed {
				t.Fatalf("неподдерживаемый кодек пропущен до чтения полезных данных: read=%d exposed=%d err=%v", conn.readBytes, len(got), err)
			}
		})
	}
}

func TestContractWireChunkSizeUpperBoundAndLegalIncrease(t *testing.T) {
	limits := acceptanceWireLimits()
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], limits.MaxChunkBytes+1)
	w, conn := acceptanceWire(t, chunk0(2, 1, size[:]), limits)
	got, err := io.ReadAll(w)
	if !errors.Is(err, ErrLimit) || len(got) != 0 || conn.readBytes > 16 {
		t.Fatalf("превышающий бюджет ChunkSize передан библиотеке: read=%d exposed=%d err=%v", conn.readBytes, len(got), err)
	}
	binary.BigEndian.PutUint32(size[:], limits.MaxChunkBytes)
	payload := bytes.Repeat([]byte{0xab}, 200)
	raw := append(chunk0(2, 1, size[:]), chunk0(4, 8, append([]byte{0x91, 'm', 'p', '4', 'a'}, payload...))...)
	w, _ = acceptanceWire(t, raw, limits)
	counter := bytecounter.NewReader(w)
	reader := message.NewReader(counter, counter, func(uint32) error { return nil })
	if _, err := reader.Read(); err != nil {
		t.Fatal(err)
	}
	m, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	audio, ok := m.(*message.AudioExCodedFrames)
	if !ok || !bytes.Equal(audio.Payload, payload) {
		t.Fatalf("легальное увеличение ChunkSize исказило AAC: %T", m)
	}
}

func TestContractWireAMFSupportedGraphsAndMalformedBoundaries(t *testing.T) {
	data := amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: float64(30)}, {Key: "extra", Value: amf0.ECMAArray{{Key: "items", Value: amf0.StrictArray{nil, amf0.Undefined{}, true, "value"}}}}}}
	body, err := data.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	limits := acceptanceWireLimits()
	limits.MaxAMFDepth = 5
	w, _ := acceptanceWire(t, fragmented(3, 18, body, 128), limits)
	counter := bytecounter.NewReader(w)
	reader := message.NewReader(counter, counter, func(uint32) error { return nil })
	msg, err := reader.Read()
	if err != nil {
		t.Fatalf("допустимый ограниченный граф отвергнут: %v", err)
	}
	metadata, ok := msg.(*message.DataAMF0)
	if !ok || !reflect.DeepEqual(metadata.Payload, data) {
		t.Fatalf("потерян граф метаданных: %T", msg)
	}
	object, ok := metadata.Payload[1].(amf0.Object)
	if !ok {
		t.Fatalf("потерян Object: %T", metadata.Payload[1])
	}
	if rate, ok := object.GetFloat64("framerate"); !ok || rate != 30 {
		t.Fatal("потерян проверенный framerate")
	}
	for _, tc := range []struct {
		name string
		body []byte
		want error
	}{
		{"unknown-marker", []byte{0xff}, ErrFraming},
		{"truncated-number", []byte{0, 0}, ErrFraming},
		{"truncated-boolean", []byte{1}, ErrFraming},
		{"truncated-string-length", []byte{2, 0}, ErrFraming},
		{"truncated-string", []byte{2, 0, 2, 'a'}, ErrFraming},
		{"truncated-ecma-count", []byte{8, 0}, ErrFraming},
		{"truncated-property-length", []byte{3, 0}, ErrFraming},
		{"truncated-property-name", []byte{3, 0, 2, 'a'}, ErrFraming},
		{"missing-object-end", []byte{3, 0, 0}, ErrFraming},
		{"wrong-object-end", []byte{3, 0, 0, 8}, ErrFraming},
		{"truncated-array-count", []byte{10, 0}, ErrFraming},
		{"array-count-budget", []byte{10, 0, 0, 0, 32}, ErrLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := acceptanceWire(t, chunk0(3, 18, tc.body), limits)
			got, err := io.ReadAll(w)
			if !errors.Is(err, tc.want) || len(got) != 0 {
				t.Fatalf("опасный граф AMF передан декодеру сообщений: exposed=%d err=%v", len(got), err)
			}
		})
	}
}

func TestContractWireAMFElementLimitHasInclusiveBoundary(t *testing.T) {
	limits := acceptanceWireLimits()
	limits.MaxAMFElements = 2
	for _, tc := range []struct {
		body []byte
		want error
	}{
		{[]byte{5, 5}, nil}, {[]byte{5, 5, 5}, ErrLimit},
	} {
		w, _ := acceptanceWire(t, chunk0(3, 18, tc.body), limits)
		got, err := io.ReadAll(w)
		if !errors.Is(err, tc.want) || (tc.want != nil && len(got) != 0) || (tc.want == nil && !bytes.Equal(got, chunk0(3, 18, tc.body))) {
			t.Fatalf("граница количества AMF-значений: body=%x exposed=%x err=%v", tc.body, got, err)
		}
	}
}

func TestContractWireRejectsConflictingContinuationBeforePayload(t *testing.T) {
	for _, extended := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-header", true: "extended-timestamp"}[extended], func(t *testing.T) {
			first := chunk0(4, 8, []byte{0xaf, 1})[:13]
			next := chunk0(4, 8, []byte{0xaf, 1})
			wantRead := 14
			if extended {
				first = contractMessage(4, 8, 0xffffffff, []byte{0xaf, 1})[:17]
				next = []byte{0xc4, 0, 0, 0, 0, 1}
				wantRead = 22
			}
			w, conn := acceptanceWire(t, append(first, next...), acceptanceWireLimits())
			w.chunkSize = 1
			got, err := io.ReadAll(w)
			if !errors.Is(err, ErrFraming) || len(got) != len(first) || conn.readBytes > wantRead {
				t.Fatalf("несогласованное продолжение передано декодеру сообщений: read=%d exposed=%d err=%v", conn.readBytes, len(got), err)
			}
		})
	}
}
