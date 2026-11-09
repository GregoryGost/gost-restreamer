package integration

import (
	"context"
	"errors"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/message"
)

func acceptanceMonitoringIngest(t *testing.T, f *sessionAcceptanceFixture) net.Listener {
	t.Helper()
	server, err := rtmp.NewServer(rtmp.Options{Core: f.service, Limits: publisherRTMPLimits(), Route: rtmp.ConventionalRoute, Feed: func(s core.Session) (*media.Router, error) { return f.manager.Feed(s) }})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		ctx, stop := context.WithTimeout(context.Background(), 4*time.Second)
		defer stop()
		if err := server.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Error("локальный вход RTMP не присоединён")
		}
	})
	return listener
}

func acceptanceMonitoringClient(t *testing.T, f *sessionAcceptanceFixture, listener net.Listener, video media.CodecConfig) *gortmplib.Client {
	t.Helper()
	client := &gortmplib.Client{URL: &url.URL{Scheme: "rtmp", Host: listener.Addr().String(), Path: "/" + string(f.stream.ID), Fragment: f.stream.PublicationKey}, Publish: true}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	if err := client.Write(&message.SetChunkSize{Value: 128}); err != nil {
		t.Fatal(err)
	}
	if err := publisherWriteMessage(client.NetConn(), 9, 1, 0, append([]byte{0x90, 'a', 'v', '0', '1'}, video.Headers()...)); err != nil {
		t.Fatal(err)
	}
	return client
}

func acceptanceMonitoringBeforeReady(t *testing.T, client *gortmplib.Client, audioConfig media.CodecConfig, video, audio []byte, pairs int) {
	t.Helper()
	for i := range pairs {
		if err := publisherWriteMessage(client.NetConn(), 9, 1, uint32(20*i), append([]byte{0x91, 'a', 'v', '0', '1'}, video...)); err != nil {
			t.Fatal(err)
		}
	}
	if err := publisherWriteMessage(client.NetConn(), 8, 1, 0, append([]byte{0xaf, 0}, audioConfig.Headers()...)); err != nil {
		t.Fatal(err)
	}
	for i := range pairs {
		if err := publisherWriteMessage(client.NetConn(), 8, 1, uint32(20*i), append([]byte{0xaf, 1}, audio...)); err != nil {
			t.Fatal(err)
		}
	}
}

func acceptanceMonitoringWritePair(client *gortmplib.Client, stamp uint32, video, audio []byte) error {
	if err := publisherWriteMessage(client.NetConn(), 9, 1, stamp, append([]byte{0x91, 'a', 'v', '0', '1'}, video...)); err != nil {
		return err
	}
	return publisherWriteMessage(client.NetConn(), 8, 1, stamp, append([]byte{0xaf, 1}, audio...))
}

func TestAcceptanceSessionMonitoringMeasuresIngressBeforeReadyAndAfterOverride(t *testing.T) {
	receiver, _ := newPublisherReceiver(t, false)
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	f.launch(t)
	current := f.running(t, f.start(t, "start-monitoring", f.destinations[0].ID))
	fence := core.RuntimeSessionFence{SessionID: current.ID, Incarnation: current.Incarnation, Lifecycle: current.Lifecycle}
	listener := acceptanceMonitoringIngest(t, f)
	v, a := publisherConfigs(t, publisherProfile(core.CodecAV1, 640, 360), 1)
	video, audio := publisherPayload(v, 20), publisherPayload(a, 0)
	wantPairBytes := uint64(len(video) + len(audio))
	first := acceptanceMonitoringClient(t, f, listener, v)
	acceptanceMonitoringBeforeReady(t, first, a, video, audio, 1)
	var before core.SessionRuntimeSnapshot
	sessionAcceptanceWait(t, func() bool {
		before, _ = f.service.RuntimeSnapshot(context.Background(), fence)
		return before.Input != nil && before.Input.Metrics != nil && len(before.Groups) == 1 && before.Groups[0].Metrics != nil && len(before.Outputs) == 1 && before.Outputs[0].Metrics != nil
	})
	input := before.Input
	if !input.Connected || input.Metrics.VideoFrames != 1 || input.Metrics.PayloadBytes != wantPairBytes || input.Metrics.Format.VideoCodec != core.CodecAV1 || input.Metrics.Format.Width != 640 || input.Metrics.Format.AudioCodec != core.CodecAAC || input.Metrics.LastFrame.IsZero() {
		t.Fatal("измерения до готовности потеряны", input)
	}
	g := before.Groups[0]
	if g.Metrics.LiveInput.VideoFrames != 0 || g.Metrics.LiveInput.PayloadBytes != 0 || g.Metrics.FallbackInput.VideoFrames == 0 || g.Metrics.Output.VideoFrames == 0 || g.Value == nil || g.Value.Source != core.ObservationSourceFallback {
		t.Fatal("вход до готовности смешан с реально потреблённым LIVE", g)
	}
	o := before.Outputs[0].Metrics
	if o.WrittenBytes <= o.PayloadBytes || o.PayloadBytes == 0 || o.VideoFrames == 0 || o.Connections != 1 || o.QueueCapacity <= 0 || o.LastWrite.IsZero() {
		t.Fatal("назначение не содержит фактическую передачу и ёмкость очереди", o)
	}
	second := acceptanceMonitoringClient(t, f, listener, v)
	acceptanceMonitoringBeforeReady(t, second, a, video, audio, 2)
	first.Close()
	var replacement core.SessionRuntimeSnapshot
	sessionAcceptanceWait(t, func() bool {
		replacement, _ = f.service.RuntimeSnapshot(context.Background(), fence)
		return replacement.Input != nil && replacement.Input.Connected && replacement.Input.LeaseID != input.LeaseID && replacement.Input.Metrics != nil && replacement.Input.Metrics.VideoFrames == 2
	})
	if replacement.Input.Metrics.PayloadBytes != 2*wantPairBytes || !replacement.Input.Metrics.LastFrame.After(input.Metrics.LastFrame) || replacement.Groups[0].Metrics.LiveInput.VideoFrames != 0 {
		t.Fatal("Override унаследовал счётчики или прежний отзыв повредил новое соединение", replacement)
	}
	if err := second.Write(&message.DataAMF0{ChunkStreamID: 5, MessageStreamID: 1, Payload: amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: float64(60)}}}}); err != nil {
		t.Fatal(err)
	}
	producerCtx, stopProducer := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopProducer()
	produced := make(chan error, 1)
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for i := range 120 {
			select {
			case <-producerCtx.Done():
				produced <- producerCtx.Err()
				return
			case <-ticker.C:
			}
			if err := acceptanceMonitoringWritePair(second, uint32(200+20*i), video, audio); err != nil {
				produced <- err
				return
			}
		}
		produced <- nil
	}()
	t.Cleanup(func() {
		stopProducer()
		second.Close()
		select {
		case <-producerDone:
		case <-time.After(time.Second):
			t.Error("производитель тестовых AU не присоединён")
		}
	})
	var live core.SessionRuntimeSnapshot
	sessionAcceptanceWait(t, func() bool {
		live, _ = f.service.RuntimeSnapshot(context.Background(), fence)
		return len(live.Groups) == 1 && live.Groups[0].Value != nil && live.Groups[0].Value.Source == core.ObservationSourceLive && live.Groups[0].Metrics != nil && live.Groups[0].Metrics.LiveInput.VideoFrames > 0
	})
	select {
	case err := <-produced:
		if err != nil {
			t.Fatal(err)
		}
	case <-producerCtx.Done():
		t.Fatal(producerCtx.Err())
	}
	var final core.SessionRuntimeSnapshot
	sessionAcceptanceWait(t, func() bool {
		final, _ = f.service.RuntimeSnapshot(context.Background(), fence)
		return final.Input != nil && final.Input.Metrics != nil && final.Input.Metrics.VideoFrames == 122 && final.Groups[0].Metrics.LiveInput.VideoFrames == 120
	})
	if final.Input.Metrics.PayloadBytes != 122*wantPairBytes || final.Groups[0].Metrics.LiveInput.PayloadBytes != 120*wantPairBytes || final.Groups[0].Metrics.Output.VideoFrames < live.Groups[0].Metrics.Output.VideoFrames {
		t.Fatal("известные AU/байты смешаны между входом, группой и назначением", final)
	}
	second.Close()
	sessionAcceptanceWait(t, func() bool {
		snapshot, err := f.service.RuntimeSnapshot(context.Background(), fence)
		return err == nil && snapshot.Input != nil && !snapshot.Input.Connected && snapshot.Input.Metrics == nil && snapshot.Groups[0].Value.Source == core.ObservationSourceFallback
	})
	stop, err := f.manager.StopSession(context.Background(), core.SessionCommand{RequestID: "stop-monitoring", SessionID: current.ID})
	if err != nil {
		t.Fatal(err)
	}
	sessionAcceptanceReceipt(t, f.service, stop.ID, core.CommandSucceeded)
	sessionAcceptanceWait(t, func() bool {
		_, err := f.service.RuntimeSnapshot(context.Background(), fence)
		return errors.Is(err, core.ErrNotReady)
	})
}
