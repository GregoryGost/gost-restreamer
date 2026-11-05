package integration

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
)

func TestPublisherSlowQueueOverflowKeepsHealthyOutputAndResyncs(t *testing.T) {
	hold := make(chan struct{})
	var release sync.Once
	unlock := func() { release.Do(func() { close(hold) }) }
	defer unlock()
	slow, _ := newPublisherReceiver(t, false, hold)
	healthy, _ := newPublisherReceiver(t, false)
	endpoints := []string{"rtmp://" + slow.listener.Addr().String() + "/slow", "rtmp://" + healthy.listener.Addr().String() + "/healthy"}
	f := newPublisherPipeline(t, []core.GroupProfile{publisherProfile(core.CodecAV1, 2560, 1440)}, endpoints, []string{"slow-key", "healthy-key"}, []int{0, 0})
	var slowPublisher, healthyPublisher *publisher.Publisher
	for _, o := range f.outputs {
		configure := func(options *publisher.Options) {}
		if o.Snapshot.Destination.Endpoint == endpoints[0] {
			configure = func(options *publisher.Options) {
				options.Queue.MaxPackets = 8
				options.Queue.MaxBytes = 1 << 20
				options.Transport.Limits.WriteTimeout = 5 * time.Second
				options.Transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
					connection, e := (&net.Dialer{}).DialContext(ctx, network, address)
					if e == nil {
						if tcp, ok := connection.(*net.TCPConn); ok {
							if e = tcp.SetWriteBuffer(1024); e != nil {
								_ = tcp.Close()
								return nil, e
							}
						}
					}
					return connection, e
				}
			}
		}
		p := publisherActivate(t, f, o, f.live, nil, configure)
		if o.Snapshot.Destination.Endpoint == endpoints[0] {
			slowPublisher = p
		} else {
			healthyPublisher = p
		}
	}
	publisherWait(t, func() bool {
		return slowPublisher.Snapshot().Connections == 1 && healthyPublisher.Snapshot().Connections == 1
	})
	source, err := f.live.BeginSource()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Disconnect() }()
	v, a := publisherConfigs(t, publisherProfile(core.CodecAV1, 2560, 1440), source.Epoch())
	if err = source.UpdateConfigs(v, a); err != nil {
		t.Fatal(err)
	}
	// Дополняющий OBU проверяет большие сообщения RTMP без заявления о декодируемости.
	payload := append(publisherPayload(v, 20), 0x7a, 0x80, 0x80, 0x10)
	payload = append(payload, make([]byte, 256<<10)...)
	probe, err := media.NewPacket(media.PacketInfo{Keyframe: true, DurationTicks: 1}, v, payload)
	if err != nil {
		t.Fatal(err)
	}
	var inspector codec.Inspector
	if key, e := inspector.Inspect(probe); e != nil || !key {
		t.Fatalf("неверный независимый padding AU: %v", e)
	}
	var audio int64
	video := int64(0)
	publish := func(large bool) {
		t.Helper()
		body := publisherPayload(v, 20)
		if large {
			body = payload
		}
		p, e := media.NewPacket(media.PacketInfo{PTS: video, DTS: video, DurationTicks: 1, Keyframe: true}, v, body)
		if e != nil {
			t.Fatal(e)
		}
		if e = source.Publish(p); e != nil {
			t.Fatal(e)
		}
		for audio*60 < (video+1)*48000 {
			p, e = media.NewPacket(media.PacketInfo{PTS: audio, DTS: audio, DurationTicks: 1024}, a, publisherPayload(a, 0))
			if e != nil {
				t.Fatal(e)
			}
			if e = source.Publish(p); e != nil {
				t.Fatal(e)
			}
			audio += 1024
		}
		stamp := uint32(publisherNearestMS(video, v.Info().TimeBase))
		healthy.wait(t, func(messages []publisherWireMessage) bool {
			for _, m := range messages {
				if m.typ == 9 && len(m.body) > 5 && m.body[0]&15 == 1 && m.timestamp == stamp {
					return true
				}
			}
			return false
		})
		video++
	}
	for range 40 {
		publish(true)
		if slowPublisher.Snapshot().QueueDrops.Overflow > 0 {
			break
		}
	}
	if slowPublisher.Snapshot().QueueDrops.Overflow == 0 {
		t.Fatal("реально заблокированный выход не достиг ограниченного overflow")
	}
	if healthyPublisher.Snapshot().QueueDrops.Overflow != 0 {
		t.Fatal("медленный socket переполнил здоровую очередь при подтверждённой доставке каждого AU")
	}
	before := publisherMediaCount(healthy.snapshot())
	recoveryStamp := uint32(publisherNearestMS(video, v.Info().TimeBase))
	unlock()
	for range 8 {
		publish(false)
	}
	slow.wait(t, func(messages []publisherWireMessage) bool {
		headers, recovered := 0, false
		for _, m := range messages {
			if m.typ == 9 && len(m.body) > 5 {
				if m.body[0]&15 == 0 {
					headers++
				}
				if m.body[0]&15 == 1 && m.timestamp >= recoveryStamp {
					recovered = true
				}
			}
		}
		return headers >= 2 && recovered
	})
	healthy.wait(t, func(messages []publisherWireMessage) bool { return publisherMediaCount(messages) > before+4 })
	if healthy.accepted.Load() != 1 || slow.accepted.Load() != 1 {
		t.Fatalf("queue recovery закрыла исправный socket: healthy=%d slow=%d", healthy.accepted.Load(), slow.accepted.Load())
	}
	if err = slowPublisher.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = healthyPublisher.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slowPublisher.Result().NetworkJoined || !healthyPublisher.Result().NetworkJoined {
		t.Fatal("остановка не подтвердила физическое завершение")
	}
}
