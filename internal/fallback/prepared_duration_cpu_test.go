package fallback

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

func TestCPUPreparedDurationProofUsesNativeSamplesInsteadOfProbeFill(t *testing.T) {
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("локальная CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	source, err := os.CreateTemp(t.TempDir(), "source.png")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, source)
	if err = png.Encode(source, image.NewRGBA(image.Rect(0, 0, 128, 128))); err != nil {
		t.Fatal(err)
	}
	profile := selfProfile(core.CodecH264)
	profile.Format.Video.FrameRate = core.FrameRate{Numerator: 25, Denominator: 2}
	profile.VideoEncoding.MaxBFrames = 2
	loop := 1600 * time.Millisecond
	command, err := buildCommand(binary, GenerateRequest{Source: Source{Kind: core.AssetImage, Duration: loop, Demuxer: "png_pipe"}, Profile: profile, LoopDuration: loop}, "libx264")
	if err != nil {
		t.Fatal(err)
	}
	// Намеренно грубая шкала создаёт отдельную контрольную фикстуру FFmpeg n7.1.5.
	command.Args[slices.Index(command.Args, "-video_track_timescale")+1] = "25"
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	raw, err := os.CreateTemp(t.TempDir(), "raw.mp4")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, raw)
	runner := selfRunner()
	if result, e := runner.Run(context.Background(), command, source, &boundedWriter{w: raw, remaining: 8 << 20}); e != nil {
		t.Fatal(e, result.Diagnostics)
	}
	if _, err = raw.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	demux, err := fmp4.NewDemuxer(raw, fmp4.DemuxOptions{Limits: selfProbeLimits().Container, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	for {
		packet, e := demux.Next()
		if e != nil {
			t.Fatal(e)
		}
		if packet.Config().Info().Track != media.TrackVideo {
			continue
		}
		if packet.Info().DurationTicks != 2 || packet.Config().Info().TimeBase != (media.TimeBase{Numerator: 1, Denominator: 25}) {
			t.Fatal(packet.Info(), packet.Config().Info())
		}
		break
	}
	if _, err = raw.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var filled bytes.Buffer
	probeBinary := filepath.Join(filepath.Dir(binary), "ffprobe")
	args := []string{"-v", "error", "-fd", "3", "-i", "fd:", "-select_streams", "v:0", "-read_intervals", "%+#1", "-show_entries", "packet=duration", "-of", "json"}
	if result, e := runner.Run(context.Background(), Command{Binary: probeBinary, Args: args}, raw, &filled); e != nil {
		t.Fatal(e, result.Diagnostics)
	}
	var doc struct {
		Packets []struct {
			Duration int64 `json:"duration"`
		} `json:"packets"`
	}
	if err = json.Unmarshal(filled.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Packets) != 1 || doc.Packets[0].Duration != 1 {
		t.Fatal("контроль синтетического duration ffprobe изменился", filled.String())
	}
	output, err := os.CreateTemp(t.TempDir(), "normalized.mp4")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, output)
	probe := Prober{Binary: probeBinary, Runner: runner, Limits: selfProbeLimits()}
	if err = probe.NormalizePrepared(context.Background(), raw, output); err != nil {
		t.Fatal(err)
	}
	selfComparePackets(t, probe, raw, output, true)
	if _, err = probe.Verify(context.Background(), output, profile); err != nil {
		t.Fatal(err)
	}
	if err = probe.VerifyLoopDuration(context.Background(), output, loop); err != nil {
		t.Fatal(err)
	}
}
