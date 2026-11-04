package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/message"
)

func TestAcceptanceCPUEncodedAV1ThroughRealIngestAndFMP4(t *testing.T) {
	binaryPath := os.Getenv("GOST_MEDIA_FFMPEG")
	if binaryPath == "" {
		t.Skip("локальная CPU-интеграция запускается только с явно заданным FFmpeg")
	}
	source := mediaCommand(t, binaryPath, nil, "-hide_banner", "-loglevel", "warning", "-xerror", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=30:duration=0.4", "-f", "lavfi", "-i", "sine=sample_rate=48000:duration=0.4", "-map", "0:v", "-map", "1:a", "-c:v", "libaom-av1", "-threads:v", "1", "-cpu-used", "8", "-lag-in-frames", "0", "-c:a", "aac", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-avoid_negative_ts", "disabled", "pipe:1")
	oracle := probeMedia(t, filepath.Join(filepath.Dir(binaryPath), "ffprobe"), source)
	d, err := fmp4.NewDemuxer(bytes.NewReader(source), fmp4.DemuxOptions{Limits: integrationLimits(), ResolveConfig: truthfulResolver(t, oracle)})
	if err != nil {
		t.Fatal(err)
	}
	packets := readMediaPackets(t, d)
	assertProbePackets(t, oracle, packets)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "ingest.sqlite"), storage.Options{ObservationLimits: core.ObservationLimits{MaxSessions: 16, MaxGroups: 64, MaxOutputs: 128, MaxFFmpegWorkers: 64, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	asset, err := svc.ReserveAsset(ctx, core.FallbackAsset{Name: "Оригинал", Kind: core.AssetImage, OriginalPath: "originals/test.png"})
	if err != nil {
		t.Fatal(err)
	}
	asset, err = svc.PublishAsset(ctx, asset.ID, asset.Revision, asset.AttemptToken, "synthetic-source", 10, core.SourceFormat{ImageFormat: "png"})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := svc.CreateStream(ctx, core.Stream{Name: "Вход", PublicationKey: "synthetic-key", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	start, err := svc.StartSession(ctx, core.StartSessionRequest{RequestID: "start-cpu-source", StreamID: stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	fence, err := svc.Claim(ctx, start.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	if err := svc.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	router, err := media.NewRouter(media.RouterLimits{MaxConsumers: 2, MaxRetainedBytes: 4 << 20, MaxConfigBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Close() })
	sub, err := router.Subscribe(media.QueueLimits{MaxPackets: 512, MaxBytes: 2 << 20, MaxPacketBytes: 1 << 18, MaxConfigBytes: 4096, ResyncTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	limits := rtmp.Limits{MaxConnections: 2, MaxChunkStreams: 8, MaxMessageBytes: 1 << 20, MaxChunkBytes: 65536, MaxFragments: 4096, MaxRetainedBytes: 32 << 20, MaxControlBytes: 64, MaxConfigBytes: 4096, MaxAMFBytes: 4096, MaxAMFDepth: 8, MaxAMFElements: 128, MaxReadAheadBytes: 65554, HandshakeTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 3 * time.Second, ShutdownTimeout: 2 * time.Second}
	server, err := rtmp.NewServer(rtmp.Options{Limits: limits, Core: svc, Route: rtmp.ConventionalRoute, Feed: func(session core.Session) (*media.Router, error) {
		if session.ID != start.SessionID {
			return nil, core.ErrStale
		}
		return router, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServe := context.WithCancel(ctx)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(serveCtx, listener) }()
	t.Cleanup(func() {
		stopServe()
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := server.Shutdown(cleanup); err != nil {
			t.Error(err)
		}
		select {
		case err := <-serveDone:
			if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("неожиданный результат Serve после отмены: %v", err)
			}
		case <-cleanup.Done():
			t.Error("ingest не завершился")
		}
	})
	client := &gortmplib.Client{URL: &url.URL{Scheme: "rtmp", Host: listener.Addr().String(), Path: "/" + string(stream.ID), Fragment: stream.PublicationKey}, Publish: true}
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	if err := client.NetConn().SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := client.Write(&message.SetChunkSize{Value: 128}); err != nil {
		t.Fatal(err)
	}
	if err := client.Write(&message.DataAMF0{ChunkStreamID: 5, MessageStreamID: 1, Payload: amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: float64(30)}}}}); err != nil {
		t.Fatal(err)
	}
	writeRaw := func(id, typ byte, at uint32, body []byte) {
		t.Helper()
		header := []byte{id, byte(at >> 16), byte(at >> 8), byte(at), byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body)), typ, 1, 0, 0, 0}
		var data []byte
		data = append(data, header...)
		for len(body) > 0 {
			n := min(len(body), 128)
			data = append(data, body[:n]...)
			body = body[n:]
			if len(body) > 0 {
				data = append(data, 0xc0|id)
			}
		}
		if _, err := io.Copy(client.NetConn(), bytes.NewReader(data)); err != nil {
			select {
			case result := <-server.Results():
				t.Fatalf("запись источника: %v; ingest: %v", err, result.Err)
			case <-ctx.Done():
				t.Fatal(err)
			}
		}
	}
	init := d.Init()
	writeRaw(6, 9, 0, append([]byte{0x90, 'a', 'v', '0', '1'}, init.Video.Headers()...))
	if init.Audio.Info().Audio.SampleRateHz != 48000 || init.Audio.Info().Audio.Channels != 1 {
		t.Fatal("генератор не дал ожидаемый AAC-LC 48 kHz mono")
	}
	writeRaw(4, 8, 0, append([]byte{0xaf, 0}, init.Audio.Headers()...))
	type expectedPacket struct {
		at      int64
		payload []byte
	}
	want := map[media.Track][]expectedPacket{}
	firstVideo := int64(-1)
	for _, p := range packets {
		if p.Config().Info().Track == media.TrackVideo {
			b := p.Config().Info().TimeBase
			value := new(big.Int).Mul(big.NewInt(p.Info().PTS), big.NewInt(int64(b.Numerator)*1000))
			value.Quo(value, big.NewInt(int64(b.Denominator)))
			firstVideo = value.Int64() + 200
			break
		}
	}
	for _, p := range packets {
		track := p.Config().Info().Track
		b := p.Config().Info().TimeBase
		value := new(big.Int).Mul(big.NewInt(p.Info().PTS), big.NewInt(int64(b.Numerator)*1000))
		value.Quo(value, big.NewInt(int64(b.Denominator)))
		at := value.Int64() + 200
		payload := p.Payload()
		if track == media.TrackVideo {
			flag := byte(0xa1)
			if p.Info().Keyframe {
				flag = 0x91
				payload = append(init.Video.Headers()[4:], payload...)
			}
			writeRaw(6, 9, uint32(at), append([]byte{flag, 'a', 'v', '0', '1'}, payload...))
		} else {
			writeRaw(4, 8, uint32(at), append([]byte{0xaf, 1}, payload...))
		}
		if track == media.TrackVideo || at >= firstVideo {
			want[track] = append(want[track], expectedPacket{at, payload})
		}
	}
	count := len(want[media.TrackVideo]) + len(want[media.TrackAudio])
	seen := map[media.Track]int{}
	var gotPackets []media.Packet
	var out bytes.Buffer
	var mux *fmp4.Muxer
	for range count {
		delivery, err := sub.Next(ctx)
		if err != nil {
			select {
			case result := <-server.Results():
				t.Fatalf("ingest: %v; Next: %v", result.Err, err)
			default:
				t.Fatal(err)
			}
		}
		p := delivery.Packet
		if !bytes.Equal(delivery.VideoConfig.Headers(), init.Video.Headers()) || !bytes.Equal(delivery.AudioConfig.Headers(), init.Audio.Headers()) {
			t.Fatal("ingest изменил исходные AV1/AAC headers реального кодировщика")
		}
		track := p.Config().Info().Track
		index := seen[track]
		seen[track]++
		if index >= len(want[track]) {
			t.Fatal("ingest добавил пакет")
		}
		expected := want[track][index]
		base := p.Config().Info().TimeBase
		pts := new(big.Rat).Mul(new(big.Rat).SetInt64(p.Info().PTS), new(big.Rat).SetFrac64(int64(base.Numerator), int64(base.Denominator)))
		if !delivery.IsCurrent() || pts.Cmp(new(big.Rat).SetFrac64(expected.at, 1000)) != 0 || p.Info().PTS != p.Info().DTS || !bytes.Equal(p.Payload(), expected.payload) {
			t.Fatalf("ingest изменил wire TU/AU %s[%d]", track, index)
		}
		if mux == nil {
			mux, err = fmp4.NewMuxer(&out, fmp4.Init{Video: delivery.VideoConfig, Audio: delivery.AudioConfig}, integrationLimits())
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := mux.WritePacket(p); err != nil {
			t.Fatal(err)
		}
		gotPackets = append(gotPackets, p)
	}
	if seen[media.TrackVideo] != 12 || seen[media.TrackAudio] == 0 {
		t.Fatalf("неполный encoded ingest: %+v", seen)
	}
	returned, err := fmp4.NewDemuxer(bytes.NewReader(out.Bytes()), fmp4.DemuxOptions{Limits: integrationLimits()})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range gotPackets {
		got, err := returned.Next()
		if err != nil || got.Info() != want.Info() || !bytes.Equal(got.Payload(), want.Payload()) {
			t.Fatalf("fMP4 изменил фактический ingest: %v", err)
		}
	}
	if _, err := returned.Next(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	decode := mediaCommand(t, binaryPath, out.Bytes(), "-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-i", "pipe:0", "-map", "0:v", "-map", "0:a", "-fps_mode", "passthrough", "-f", "framemd5", "pipe:1")
	if bytes.Count(decode, []byte("\n0,")) != 12 || bytes.Count(decode, []byte("\n1,")) == 0 {
		t.Fatal("пакеты настоящего ingest не декодируются в обе дорожки")
	}
}
