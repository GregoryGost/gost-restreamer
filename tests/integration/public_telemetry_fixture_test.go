package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

func publicTelemetryAPI(t *testing.T, service *core.Service) *api.API {
	t.Helper()
	a, err := api.New(api.Config{Core: service, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Limits: api.Limits{JSONBytes: 16384, UploadBytes: 65536, UploadReadTimeout: time.Second, OperationTimeout: 2 * time.Second, ShutdownTimeout: 2 * time.Second, SSEClients: 2, SSEPageSize: 8, SSEFrameBytes: 128 << 10, SSEWriteTimeout: time.Second, SSEHeartbeat: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := a.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return a
}

type publicTelemetryFrame struct {
	kind, id string
	data     map[string]any
	err      error
}

type publicTelemetryStream struct {
	frames <-chan publicTelemetryFrame
	done   <-chan struct{}
	cancel context.CancelFunc
	body   io.ReadCloser
}

func publicTelemetrySubscribe(t *testing.T, server *httptest.Server, path, lastID string) *publicTelemetryStream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	request, err := http.NewRequestWithContext(ctx, "GET", server.URL+api.Prefix+path, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if lastID != "" {
		request.Header.Set("Last-Event-ID", lastID)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		_ = response.Body.Close()
		cancel()
		t.Fatalf("SSE не открыт: %s %v", response.Status, response.Header)
	}
	frames, done := make(chan publicTelemetryFrame, 256), make(chan struct{})
	s := &publicTelemetryStream{frames: frames, done: done, cancel: cancel, body: response.Body}
	go func() {
		defer close(done)
		defer close(frames)
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 4096), 128<<10)
		frame := publicTelemetryFrame{}
		send := func(v publicTelemetryFrame) bool {
			select {
			case frames <- v:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if frame.kind != "" && !send(frame) {
					return
				}
				frame = publicTelemetryFrame{}
				continue
			}
			switch {
			case strings.HasPrefix(line, "event: "):
				frame.kind = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				frame.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				frame.err = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame.data)
			}
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			_ = send(publicTelemetryFrame{err: err})
		}
	}()
	t.Cleanup(func() { s.close(t) })
	return s
}

func (s *publicTelemetryStream) close(t *testing.T) {
	t.Helper()
	s.cancel()
	_ = s.body.Close()
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		t.Error("чтение SSE не присоединено")
	}
}

func (s *publicTelemetryStream) next(t *testing.T) (publicTelemetryFrame, bool) {
	t.Helper()
	select {
	case frame, ok := <-s.frames:
		if frame.err != nil {
			t.Fatal("разбор SSE:", frame.err)
		}
		if ok && frame.kind != "resync_required" && (frame.id == "" || frame.data["cursor"] != frame.id || frame.data["kind"] != frame.kind) {
			t.Fatalf("SSE потерял связь cursor и payload: %+v", frame)
		}
		return frame, ok
	case <-time.After(10 * time.Second):
		t.Fatal("следующий кадр SSE не получен до срока")
		return publicTelemetryFrame{}, false
	}
}

// Конечный источник оставляет UDS открытым: счётчики успевают попасть в RAM без нового медиа.
func TestPublicTelemetryFiniteMediaChild(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "--public-telemetry" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	args := os.Args[index+1:]
	if len(args) != 2 {
		t.Fatal("неверные аргументы конечного источника")
	}
	connection, err := net.Dial("unix", args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	video, audio := publisherConfigs(t, publisherProfile(core.CodecAV1, 640, 360), 1)
	mux, err := fmp4.NewMuxer(connection, fmp4.Init{Video: video, Audio: audio}, integrationLimits())
	if err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(time.Second / 60)
	defer ticker.Stop()
	var audioTicks int64
	for frame := range int64(60) {
		packet, err := media.NewPacket(media.PacketInfo{PTS: frame, DTS: frame, DurationTicks: 1, Keyframe: true}, video, publisherPayload(video, 10))
		if err != nil || mux.WritePacket(packet) != nil {
			t.Fatal("передача конечного видео:", err)
		}
		for audioTicks*60 < (frame+1)*48000 {
			packet, err = media.NewPacket(media.PacketInfo{PTS: audioTicks, DTS: audioTicks, DurationTicks: 1024}, audio, publisherPayload(audio, 0))
			if err != nil || mux.WritePacket(packet) != nil {
				t.Fatal("передача конечного аудио:", err)
			}
			audioTicks += 1024
		}
		<-ticker.C
	}
	if err := os.WriteFile(args[1], []byte("complete"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, connection)
}
