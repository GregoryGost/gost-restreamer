package fallback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

type preparedProbeRunner struct{ packets []byte }

func (r preparedProbeRunner) Run(_ context.Context, c Command, _ *os.File, w io.Writer) (RunResult, error) {
	data := []byte(av1ProbeJSON)
	if slices.Contains(c.Args, "-show_packets") {
		data = r.packets
	}
	_, err := w.Write(data)
	return RunResult{}, err
}
func selfPreparedMetadata() packetDocument {
	doc := packetDocument{}
	doc.Streams = append(doc.Streams, struct {
		Index int    `json:"index"`
		Type  string `json:"codec_type"`
		Base  string `json:"time_base"`
	}{0, "video", "1/30"}, struct {
		Index int    `json:"index"`
		Type  string `json:"codec_type"`
		Base  string `json:"time_base"`
	}{1, "audio", "1/48000"})
	stamp := func(kind string, index int, pts, duration int64) packetStamp {
		return packetStamp{Type: kind, Index: index, PTS: &pts, DTS: &pts, Duration: &duration}
	}
	for i := 0; i < 76; i++ {
		if i < 48 {
			doc.Packets = append(doc.Packets, stamp("video", 0, int64(i), 1))
		}
		p := stamp("audio", 1, int64(i-1)*1024, 1024)
		if i == 0 {
			p.Side = []packetSide{{Type: "Skip Samples", Skip: 1024}}
		}
		doc.Packets = append(doc.Packets, p)
	}
	return doc
}
func TestPreparedNormalizationDropsOnlyProvenWholePrimingAndChecksPeriod(t *testing.T) {
	for _, durationAbsent := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit-duration", true: "codec-and-fmp4-duration"}[durationAbsent], func(t *testing.T) {
			doc := selfPreparedMetadata()
			if durationAbsent {
				for i := range doc.Packets {
					if doc.Packets[i].Type == "audio" {
						doc.Packets[i].Duration = nil
					}
				}
			}
			encoded, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			p := Prober{Binary: "probe", Runner: preparedProbeRunner{packets: encoded}, Limits: selfProbeLimits()}
			raw := selfRawAV1Counts(t, false, 48, 76)
			if _, err = p.Verify(context.Background(), raw, selfProfile(core.CodecAV1)); err == nil {
				t.Fatal("устаревший av1C принят до нормализации")
			}
			output, err := os.CreateTemp(t.TempDir(), "normalized")
			if err != nil {
				t.Fatal(err)
			}
			selfClose(t, output)
			if err = p.NormalizePrepared(context.Background(), raw, &boundedWriter{w: output, remaining: 1 << 20}); err != nil {
				t.Fatal(err)
			}
			selfComparePackets(t, p, raw, output, true)
			if _, err = p.Verify(context.Background(), output, selfProfile(core.CodecAV1)); err != nil {
				t.Fatal(err)
			}
			if err = p.VerifyLoopDuration(context.Background(), output, 1600*time.Millisecond); err != nil {
				t.Fatal(err)
			}
			if err = p.VerifyLoopDuration(context.Background(), output, time.Second); !errors.Is(err, core.ErrNotReady) {
				t.Fatal(err)
			}
		})
	}
}
func TestPreparedNormalizationRejectsUnknownPartialPaddingAndUnprovenTime(t *testing.T) {
	for _, scenario := range []string{"missing-side", "partial-skip", "discard-tail", "unknown-side", "missing-dts", "unknown-index", "truncated-json", "unequal-loop", "b-frame", "changed-probe-time", "output-budget", "cancel", "missing-sequence", "initialization-limit", "packet-limit", "truncated-raw"} {
		t.Run(scenario, func(t *testing.T) {
			doc := selfPreparedMetadata()
			switch scenario {
			case "missing-side":
				doc.Packets[1].Side = nil
			case "partial-skip":
				doc.Packets[1].Side[0].Skip = 1000
			case "discard-tail":
				doc.Packets[1].Side[0].Discard = 1
			case "unknown-side":
				doc.Packets[1].Side[0].Type = "unknown"
			case "missing-dts":
				doc.Packets[0].DTS = nil
			case "unknown-index":
				doc.Packets[0].Index = 99
			case "unequal-loop":
				doc.Packets = doc.Packets[:len(doc.Packets)-1]
			case "b-frame":
				pts := int64(1)
				doc.Packets[0].PTS = &pts
			case "changed-probe-time":
				duration := int64(2)
				doc.Packets[0].Duration = &duration
			}
			encoded, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "truncated-json" {
				encoded = encoded[:len(encoded)/2]
			}
			p := Prober{Binary: "probe", Runner: preparedProbeRunner{packets: encoded}, Limits: selfProbeLimits()}
			if scenario == "initialization-limit" {
				p.Limits.Container.MaxInitBytes = 500
				p.Limits.Container.MaxExtradataBytes = 64
			}
			if scenario == "packet-limit" {
				p.Limits.MaxPackets = 1
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancel" {
				cancel()
			}
			var out bytes.Buffer
			limit := uint64(1 << 20)
			if scenario == "output-budget" {
				limit = 8
			}
			raw := selfRawAV1Counts(t, scenario == "missing-sequence", 48, 76)
			if scenario == "truncated-raw" {
				info, e := raw.Stat()
				if e != nil {
					t.Fatal(e)
				}
				if e = raw.Truncate(info.Size() - 10); e != nil {
					t.Fatal(e)
				}
			}
			err = p.NormalizePrepared(ctx, raw, &boundedWriter{w: &out, remaining: limit})
			if err == nil {
				t.Fatal("неподтверждённый padding или период принят")
			}
			if scenario == "output-budget" && !errors.Is(err, ErrLimit) {
				t.Fatal(err)
			}
		})
	}
}
