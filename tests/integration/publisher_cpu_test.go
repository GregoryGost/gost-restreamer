package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
)

// Оракул использует точные дроби: округление осуществляется после независимого PTS/DTS.
func publisherNearestMS(ticks int64, base media.TimeBase) int64 {
	value := new(big.Rat).Mul(new(big.Rat).SetInt64(ticks), new(big.Rat).SetFrac64(int64(base.Numerator)*1000, int64(base.Denominator)))
	sign := value.Sign()
	value.Abs(value)
	value.Add(value, big.NewRat(1, 2))
	rounded := new(big.Int).Quo(value.Num(), value.Denom())
	if sign < 0 {
		rounded.Neg(rounded)
	}
	return rounded.Int64()
}

func publisherRealFixture(t *testing.T, binaryPath string, p core.GroupProfile, duration string) ([]byte, []media.Packet, fmp4.Init) {
	t.Helper()
	args := []string{"-hide_banner", "-loglevel", "warning", "-xerror", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=60:duration=" + duration, "-f", "lavfi", "-i", "sine=frequency=750:sample_rate=48000:duration=" + duration, "-map", "0:v", "-map", "1:a", "-threads:v", "1", "-g:v", "6", "-bf:v", "0", "-fps_mode:v", "passthrough", "-vf", "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709", "-color_range:v", "tv", "-color_primaries:v", "bt709", "-color_trc:v", "bt709", "-colorspace:v", "bt709"}
	if p.Format.Video.Codec == core.CodecAV1 {
		args = append(args, "-c:v", "libaom-av1", "-cpu-used", "8", "-lag-in-frames", "0", "-b:v", "250000")
	} else {
		args = append(args, "-c:v", "libx264", "-profile:v", "high", "-sc_threshold", "0", "-b:v", "250000")
	}
	args = append(args, "-enc_time_base:v", "1:60", "-video_track_timescale", "60", "-c:a", "aac", "-b:a", "64000", "-ar:a", "48000", "-ac:a", "2", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-avoid_negative_ts", "disabled", "pipe:1")
	data := mediaCommand(t, binaryPath, nil, args...)
	probe := probeMedia(t, filepath.Join(filepath.Dir(binaryPath), "ffprobe"), data)
	d, err := fmp4.NewDemuxer(bytes.NewReader(data), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		if desc.Track == media.TrackAudio || desc.Codec == core.CodecH264 {
			return fmp4.ResolveCodecConfig(desc, 1, 1)
		}
		return media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase, Video: p.Format.Video}, desc.Headers)
	}})
	if err != nil {
		t.Fatal(err)
	}
	packets := readMediaPackets(t, d)
	assertProbePackets(t, probe, packets)
	return data, packets, d.Init()
}

func publisherFLV(messages []publisherWireMessage) []byte {
	var out bytes.Buffer
	out.Write([]byte{'F', 'L', 'V', 1, 5, 0, 0, 0, 9, 0, 0, 0, 0})
	for _, m := range messages {
		if m.typ != 8 && m.typ != 9 {
			continue
		}
		n := len(m.body)
		out.Write([]byte{m.typ, byte(n >> 16), byte(n >> 8), byte(n), byte(m.timestamp >> 16), byte(m.timestamp >> 8), byte(m.timestamp), byte(m.timestamp >> 24), 0, 0, 0})
		out.Write(m.body)
		var previous [4]byte
		binary.BigEndian.PutUint32(previous[:], uint32(n+11))
		out.Write(previous[:])
	}
	return out.Bytes()
}

func TestPublisherCPURealIngestGroupAndNativeOutputDecode(t *testing.T) {
	binaryPath := os.Getenv("GOST_MEDIA_FFMPEG")
	if binaryPath == "" {
		t.Skip("CPU-интеграция требует явно заданный FFmpeg")
	}
	for _, outputCodec := range []core.Codec{core.CodecAV1, core.CodecH264} {
		t.Run(string(outputCodec), func(t *testing.T) {
			p := publisherProfile(outputCodec, 128, 128)
			inputProfile := publisherProfile(core.CodecAV1, 128, 128)
			fallbackBytes, _, _ := publisherRealFixture(t, binaryPath, p, "1.6")
			_, source, inputInit := publisherRealFixture(t, binaryPath, inputProfile, "1.6")
			// В Enhanced RTMP независимая точка содержит заголовок последовательности вместе с OBU кадра.
			for i, packet := range source {
				if packet.Config().Info().Track == media.TrackVideo && packet.Info().Keyframe {
					payload := append(inputInit.Video.Headers()[4:], packet.Payload()...)
					rebuilt, e := media.NewPacket(packet.Info(), packet.Config(), payload)
					if e != nil {
						t.Fatal(e)
					}
					source[i] = rebuilt
				}
			}
			r, trust := newPublisherReceiver(t, outputCodec == core.CodecH264)
			scheme := "rtmp"
			if trust != nil {
				scheme = "rtmps"
			}
			f := newPublisherPipeline(t, []core.GroupProfile{p}, []string{scheme + "://" + r.listener.Addr().String() + "/cpu"}, []string{"synthetic-key"}, []int{0})
			filePath := filepath.Join(t.TempDir(), "fallback.mp4")
			inputPath := filepath.Join(t.TempDir(), "encoded.mp4")
			if err := os.WriteFile(inputPath, fallbackBytes, 0600); err != nil {
				t.Fatal(err)
			}
			encoded, err := os.Open(inputPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = encoded.Close() }()
			runner := fallback.Runner{Limits: fallback.RunnerLimits{Timeout: 10 * time.Second, MaxArgs: 128, MaxCommandBytes: 16384, MaxDiagnosticBytes: 1 << 20}}
			prober := fallback.Prober{Binary: filepath.Join(filepath.Dir(binaryPath), "ffprobe"), Runner: fallbackPipelineRunner{t: t, runner: runner}, Limits: fallback.ProbeLimits{MaxJSONBytes: 1 << 20, MaxWidth: 1024, MaxHeight: 1024, MaxPixels: 1 << 20, MaxDuration: 10 * time.Second, ImageDuration: 1600 * time.Millisecond, MaxPackets: 4096, Container: integrationLimits()}}
			var prepared bytes.Buffer
			if err = prober.NormalizePrepared(context.Background(), encoded, &prepared); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filePath, prepared.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			loop, err := os.Open(filePath)
			if err != nil {
				t.Fatal(err)
			}
			if err = prober.VerifyLoopDuration(context.Background(), loop, 1600*time.Millisecond); err != nil {
				_ = loop.Close()
				t.Fatal(err)
			}
			if err = loop.Close(); err != nil {
				t.Fatal(err)
			}
			f.config.Binary = binaryPath
			f.config.OpenFile = func(context.Context, core.ArtifactManifest) (*os.File, error) { return os.Open(filePath) }
			f.config.Transcode = func(_ context.Context, _ ffmpeg.NVIDIAConfig, a ffmpeg.Attempt) (ffmpeg.Command, error) {
				return ffmpeg.Command{Binary: binaryPath, Args: []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-xerror", "-copyts", "-f", "mp4", "-fflags", "+ignidx", "-i", "async:unix:" + a.InputPath, "-map", "0:v", "-map", "0:a", "-c:v", "libx264", "-threads:v", "1", "-profile:v", "high", "-g:v", "6", "-bf:v", "0", "-sc_threshold", "0", "-fps_mode:v", "passthrough", "-c:a", "copy", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-avoid_negative_ts", "disabled", "unix:" + a.OutputPath}}, nil
			}
			registry, err := group.NewRegistry(context.Background(), group.RegistryConfig{Core: f.service, Parent: f.parent, Observer: f.observer, Runtime: f.config, MaxGroups: 2, MaxReferences: 2})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if e := registry.Close(context.Background()); e != nil {
					t.Error(e)
				}
			})
			ref, err := registry.Acquire(context.Background(), f.outputs[0].Snapshot, f.live)
			if err != nil {
				t.Fatal(err)
			}
			tap, err := ref.Router().Subscribe(publisherQueue())
			if err != nil {
				t.Fatal(err)
			}
			tapContext, stopTap := context.WithCancel(context.Background())
			tapDone := make(chan struct{})
			var tapped []media.Packet
			var tapMutex sync.Mutex
			go func() {
				defer close(tapDone)
				for {
					delivery, e := tap.Next(tapContext)
					if e != nil {
						return
					}
					tapMutex.Lock()
					tapped = append(tapped, delivery.Packet)
					tapMutex.Unlock()
				}
			}()
			t.Cleanup(func() { stopTap(); _ = tap.Close(); <-tapDone })
			pub := publisherActivate(t, f, f.outputs[0], ref.Router(), trust)
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("Group status=%+v; Publisher=%+v; result=%+v", ref.Group().Status(), pub.Snapshot(), pub.Result())
				}
			})
			r.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= 12 })
			_, stop, done := publisherStartIngest(t, f, source, inputInit)
			defer stop()
			publisherWait(t, func() bool { return ref.Group().Status().Available && ref.Group().Status().Source == group.Live })
			wantLive := publisherMediaCount(r.snapshot()) + 30
			r.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= wantLive })
			stop()
			<-done
			publisherWait(t, func() bool { return ref.Group().Status().Available && ref.Group().Status().Source == group.Fallback })
			wantFallback := publisherMediaCount(r.snapshot()) + 12
			r.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= wantFallback })
			if err = pub.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			r.waitClosed(t)
			if r.accepted.Load() != 1 {
				t.Fatalf("реальные переходы закрыли здоровый socket: %d", r.accepted.Load())
			}
			captured := r.snapshot()
			tapMutex.Lock()
			groupPackets := append([]media.Packet(nil), tapped...)
			tapMutex.Unlock()
			publisherAssertMappedTimeline(t, captured, groupPackets)
			flv := publisherFLV(captured)
			probe := probeMedia(t, filepath.Join(filepath.Dir(binaryPath), "ffprobe"), flv)
			for _, s := range probe.Streams {
				if s.CodecType == "video" && (s.CodecName != string(outputCodec) || s.Width != 128 || s.Height != 128) {
					t.Fatalf("получатель увидел неверный формат: %+v", s)
				}
			}
			videoCount, audioCount := 0, 0
			var lastVideo, lastAudio *big.Rat
			for _, packet := range probe.Packets {
				var streamType, base string
				for _, s := range probe.Streams {
					if s.Index == packet.StreamIndex {
						streamType, base = s.CodecType, s.TimeBase
					}
				}
				dts := seconds(t, packet.DTS, base)
				pts := seconds(t, packet.PTS, base)
				if pts.Cmp(dts) != 0 {
					t.Fatalf("CPU-путь без B-кадров изменил PTS−DTS: %s/%s", pts, dts)
				}
				if streamType == "video" {
					if lastVideo != nil && dts.Cmp(lastVideo) < 0 {
						t.Fatal("video DTS сброшен")
					}
					lastVideo = dts
					videoCount++
				} else {
					if lastAudio != nil && dts.Cmp(lastAudio) < 0 {
						t.Fatal("AAC DTS сброшен")
					}
					lastAudio = dts
					audioCount++
				}
			}
			decoded := mediaCommand(t, binaryPath, flv, "-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-i", "pipe:0", "-map", "0:v", "-map", "0:a", "-fps_mode", "passthrough", "-f", "framemd5", "pipe:1")
			if got := strings.Count(string(decoded), "\n0,"); got != videoCount || videoCount < 12 {
				t.Fatalf("декодированы video=%d из %d", got, videoCount)
			}
			if got := strings.Count(string(decoded), "\n1,"); got != audioCount || audioCount < 12 {
				t.Fatalf("декодированы AAC=%d из %d", got, audioCount)
			}
			if outputCodec == core.CodecAV1 {
				known := map[string]bool{}
				for _, packet := range source {
					if packet.Config().Info().Track == media.TrackVideo {
						known[string(packet.Payload())] = true
					}
				}
				matched := 0
				for _, m := range captured {
					if m.typ == 9 && len(m.body) > 5 && m.body[0]&15 == 1 && known[string(m.body[5:])] {
						matched++
					}
				}
				if matched < 6 {
					t.Fatalf("не доказано побайтное сохранение actual AV1 TU: %d", matched)
				}
			}
			publisherAssertMetrics(t, pub.Snapshot().WrittenBytes, pub.Snapshot().PayloadBytes, pub.Snapshot().AccessUnits, pub.Snapshot().VideoFrames, r)
		})
	}
}

func publisherAssertMappedTimeline(t *testing.T, wire []publisherWireMessage, groupPackets []media.Packet) {
	t.Helper()
	type point struct{ pts, dts int64 }
	wanted := map[string][]point{}
	for _, p := range groupPackets {
		wanted[string(p.Config().Info().Track)+string(p.Payload())] = append(wanted[string(p.Config().Info().Track)+string(p.Payload())], point{publisherNearestMS(p.Info().PTS, p.Config().Info().TimeBase), publisherNearestMS(p.Info().DTS, p.Config().Info().TimeBase)})
	}
	var origins []int64
	checked := 0
	for _, m := range wire {
		track, payload, cts := media.TrackAudio, []byte(nil), int64(0)
		if m.typ == 8 && len(m.body) > 2 && m.body[1] == 1 {
			payload = m.body[2:]
		}
		if m.typ == 9 && len(m.body) > 5 && (m.body[0]&0x80 != 0 && m.body[0]&15 == 1 || m.body[0]&0x80 == 0 && m.body[1] == 1) {
			track, payload = media.TrackVideo, m.body[5:]
			if m.body[0]&0x80 == 0 {
				raw := publisherU24(m.body[2:5])
				if raw&0x800000 != 0 {
					cts = int64(raw) - 1<<24
				} else {
					cts = int64(raw)
				}
			}
		}
		if payload == nil {
			continue
		}
		candidates := wanted[string(track)+string(payload)]
		if checked == 0 {
			if track != media.TrackVideo {
				t.Fatal("первый wire AU не video")
			}
			for _, p := range candidates {
				if p.pts-p.dts == cts && uint32(p.dts-min(p.pts, p.dts)) == m.timestamp {
					origins = append(origins, min(p.pts, p.dts))
				}
			}
		}
		var remaining []int64
		for _, origin := range origins {
			for _, p := range candidates {
				if p.dts-origin >= 0 && uint32(p.dts-origin) == m.timestamp && p.pts-p.dts == cts {
					remaining = append(remaining, origin)
					break
				}
			}
		}
		origins = remaining
		checked++
		if len(origins) == 0 {
			t.Fatalf("AU %d %s timestamp=%d CTS=%d не соответствует exact Group Q(t)−origin", checked, track, m.timestamp, cts)
		}
	}
	if checked < 12 {
		t.Fatalf("недостаточно AU для независимой проверки timeline: %d", checked)
	}
}

func publisherAssertMetrics(t *testing.T, written, payload, units, videos uint64, r *publisherReceiver) {
	t.Helper()
	var wantPayload, wantUnits, wantVideo uint64
	for _, m := range r.snapshot() {
		if m.typ == 8 && len(m.body) >= 2 && m.body[1] == 1 {
			wantUnits++
			wantPayload += uint64(len(m.body) - 2)
		}
		if m.typ == 9 && len(m.body) >= 5 {
			media := m.body[0]&0x80 != 0 && m.body[0]&15 == 1 || m.body[0]&0x80 == 0 && m.body[1] == 1
			if media {
				wantUnits++
				wantVideo++
				wantPayload += uint64(len(m.body) - 5)
			}
		}
	}
	if payload != wantPayload || units != wantUnits || videos != wantVideo {
		t.Fatalf("метрики считают не полные AU: got=%d/%d/%d wire=%d/%d/%d", payload, units, videos, wantPayload, wantUnits, wantVideo)
	}
	if written != r.bytes.Load() {
		t.Fatalf("plaintext Write bytes=%d, независимый receiver=%d", written, r.bytes.Load())
	}
}
