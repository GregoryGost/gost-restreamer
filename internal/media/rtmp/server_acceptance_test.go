package rtmp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

func acceptanceWireTime(p media.Packet, milliseconds int64) bool {
	b := p.Config().Info().TimeBase
	unit := new(big.Rat).SetFrac64(int64(b.Numerator), int64(b.Denominator))
	want := new(big.Rat).SetFrac64(milliseconds, 1000)
	return new(big.Rat).Mul(new(big.Rat).SetInt64(p.Info().PTS), unit).Cmp(want) == 0 && new(big.Rat).Mul(new(big.Rat).SetInt64(p.Info().DTS), unit).Cmp(want) == 0
}

func TestAcceptanceIngestHeaderRotationDropsOldAudioAndRevokesLatestSource(t *testing.T) {
	f := newIngestFixture(t)
	s, address := runServer(t, f, nil)
	sub := newSubscription(t, f.router)
	c, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	sendReady(t, c, 100)
	before := delivery(t, sub)
	_ = delivery(t, sub)
	// Новый заголовок уже разорвал непрерывность, но ещё не доказал новый независимый кадр.
	writeWire(t, c, chunk0(6, 9, append([]byte{0x90, 'a', 'v', '0', '1'}, testAV1Config()...)))
	sendAudio(t, c, 200, []byte{0xde, 0xad})
	sendVideo(t, c, 300, testAV1TU())
	sendAudio(t, c, 300, []byte{0xbe, 0xef})
	after := delivery(t, sub)
	audio := delivery(t, sub)
	if !acceptanceWireTime(after.Packet, 300) || !acceptanceWireTime(audio.Packet, 300) || audio.Packet.Config().Info().Track != media.TrackAudio {
		t.Fatalf("после смены header вышла старая очередь: video=%+v audio=%+v", after.Packet.Info(), audio.Packet.Info())
	}
	if before.IsCurrent() || !after.IsCurrent() || before.VideoConfig.Info().SourceEpoch == after.VideoConfig.Info().SourceEpoch {
		t.Fatal("смена init не исключила старую decoder identity")
	}
	other, err := core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.StopSession(context.Background(), core.SessionCommand{RequestID: "stop-current-header", SessionID: f.session.SessionID}); err != nil {
		t.Fatal(err)
	}
	if after.IsCurrent() {
		t.Fatal("stop другого Service не отозвал последний Source после ротации")
	}
	if _, err := c.Read(); err == nil {
		t.Fatal("остановленный источник сохранил соединение")
	}
	if err := serverResult(t, s); err == nil {
		t.Fatal("отзыв не завершил ingest")
	}
}

func TestAcceptanceIngestPacketsEnterFMP4WithoutClockSubstitution(t *testing.T) {
	f := newIngestFixture(t)
	_, address := runServer(t, f, nil)
	sub := newSubscription(t, f.router)
	c, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	sendReady(t, c, 123)
	v, a := delivery(t, sub), delivery(t, sub)
	sendVideo(t, c, 156, testAV1TU())
	sendAudio(t, c, 145, []byte{4, 5, 6})
	packets := []media.Packet{v.Packet, a.Packet, delivery(t, sub).Packet, delivery(t, sub).Packet}
	var wire bytes.Buffer
	limits := fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 4096, MaxMdatBytes: 65536, MaxSampleBytes: 32768, MaxExtradataBytes: 4096, MaxSamples: 32}
	m, err := fmp4.NewMuxer(&wire, fmp4.Init{Video: v.VideoConfig, Audio: v.AudioConfig}, limits)
	if err != nil {
		t.Fatal("фактический init ingest не принят fMP4:", err)
	}
	for _, p := range packets {
		if err := m.WritePacket(p); err != nil {
			t.Fatal("фактический пакет ingest не принят fMP4:", err)
		}
	}
	d, err := fmp4.NewDemuxer(bytes.NewReader(wire.Bytes()), fmp4.DemuxOptions{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range packets {
		got, err := d.Next()
		if err != nil {
			t.Fatal(err)
		}
		if got.Info() != want.Info() || got.Config().Info() != want.Config().Info() || !bytes.Equal(got.Config().Headers(), want.Config().Headers()) || !bytes.Equal(got.Payload(), want.Payload()) {
			t.Fatalf("пакет %d изменился между фактическим ingest и fMP4: %+v != %+v", i, got.Info(), want.Info())
		}
		wantMilliseconds := []int64{123, 123, 156, 145}[i]
		if !acceptanceWireTime(got, wantMilliseconds) {
			t.Fatal("исходные wire-миллисекунды изменены")
		}
	}
	if _, err := d.Next(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

func TestAcceptanceIngestOversizeConsumerDoesNotSetInputLimit(t *testing.T) {
	f := newIngestFixture(t)
	_, address := runServer(t, f, nil)
	fast := newSubscription(t, f.router)
	slow, err := f.router.Subscribe(media.QueueLimits{MaxPackets: 2, MaxBytes: 128, MaxPacketBytes: 32, MaxConfigBytes: 32, ResyncTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slow.Close() })
	c, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	sendReady(t, c, 0)
	_ = delivery(t, fast)
	_ = delivery(t, fast)
	// Валидный для адаптера TU превышает частный бюджет медленной подписки.
	tu := append([]byte(nil), testAV1TU()...)
	tu = append(tu, 0x7a, 64)
	tu = append(tu, make([]byte, 64)...)
	sendVideo(t, c, 33, tu)
	if got := delivery(t, fast); !acceptanceWireTime(got.Packet, 33) || len(got.Packet.Payload()) != len(tu) {
		t.Fatal("частный MaxPacketBytes стал глобальным")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := slow.Next(ctx); !errors.Is(err, media.ErrResyncTimeout) {
		t.Fatal("медленная очередь не завершила resync явной причиной:", err)
	}
}
