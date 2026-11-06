package rtmp

import (
	"bytes"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/media"
)

func TestIngressCountsConfirmedUnitsBeforeReadinessAndPreservesAcceptance(t *testing.T) {
	p, _ := newContractPublication(t)
	p.publicationID = "publication"
	p.router.BeginPublication(p.publicationID)
	config := contractAV1Config(contractSDRSequence())
	tu := contractAV1TU(config)
	au := []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c}
	if err := contractConsume(t, p, bytes.Join([][]byte{contractVideoConfig(config), contractVideo(0, tu), contractAudioConfig(), contractAudio(0, au)}, nil)); err != nil {
		t.Fatal(err)
	}
	s := p.router.IngressSnapshot()
	if p.ready || s.Counters.VideoFrames != 1 || s.Counters.AudioUnits != 1 || s.Counters.PayloadBytes != uint64(len(tu)+len(au)) || s.Counters.LastFrame.IsZero() || s.Counters.Video.Width != 640 || s.Counters.Audio.SampleRateHz != 48000 {
		t.Fatalf("измерение до готовности потеряно: ready=%v snapshot=%+v", p.ready, s)
	}
	// Измеритель не меняет прежнее решение адаптера пропустить неподтверждённый AAC.
	if err := contractConsume(t, p, contractAudio(1, []byte{0xbe, 0xef})); err != nil {
		t.Fatal(err)
	}
	if got := p.router.IngressSnapshot().Counters; got.PayloadBytes != s.Counters.PayloadBytes || got.AudioUnits != 1 {
		t.Fatalf("неподтверждённый AAC измерен: %+v", got)
	}
	if _, err := p.router.BeginSource(); err != nil {
		t.Fatal(err)
	}
	p.router.ObserveIngress(p.publicationID, media.TrackAudio, 6, s.Counters.Video, s.Counters.Audio)
	if got := p.router.IngressSnapshot(); got.Counters.VideoFrames != 1 || got.Counters.AudioUnits != 2 || got.SourceEpoch == s.SourceEpoch {
		t.Fatalf("SourceEpoch сбросил публикацию: %+v", got)
	}
	p.router.BeginPublication("replacement")
	p.router.EndPublication(p.publicationID)
	p.router.ObserveIngress(p.publicationID, media.TrackVideo, 100, s.Counters.Video, s.Counters.Audio)
	if got := p.router.IngressSnapshot(); !got.Connected || got.Counters.PayloadBytes != 0 {
		t.Fatalf("устаревший вызов изменил замену: %+v", got)
	}
}
