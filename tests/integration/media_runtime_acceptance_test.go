package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

type mediaProbe struct {
	Streams []struct {
		Index         int
		CodecType     string `json:"codec_type"`
		CodecName     string `json:"codec_name"`
		TimeBase      string `json:"time_base"`
		Width, Height uint32
		Profile       string
		PixelFormat   string `json:"pix_fmt"`
		FrameRate     string `json:"r_frame_rate"`
		SampleRate    string `json:"sample_rate"`
		Channels      uint16
		ChannelLayout string `json:"channel_layout"`
	}
	Packets []struct {
		StreamIndex int `json:"stream_index"`
		PTS, DTS    int64
	}
}

type boundedMediaBuffer struct {
	bytes.Buffer
	max int
}

func (b *boundedMediaBuffer) Write(p []byte) (int, error) {
	if len(p) > b.max-b.Len() {
		return 0, errors.New("превышен бюджет синтетической фикстуры")
	}
	return b.Buffer.Write(p)
}

func mediaCommand(t *testing.T, binary string, input []byte, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = bytes.NewReader(input)
	out, stderr := &boundedMediaBuffer{max: 16 << 20}, &boundedMediaBuffer{max: 1 << 20}
	cmd.Stdout, cmd.Stderr = out, stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s: %v: %s", filepath.Base(binary), err, stderr.String())
	}
	if strings.Contains(stderr.String(), "Invalid DTS") || strings.Contains(stderr.String(), "Non-monotonic") {
		t.Fatalf("FFmpeg изменил время: %s", stderr.String())
	}
	return bytes.Clone(out.Bytes())
}

func probeMedia(t *testing.T, binary string, data []byte) mediaProbe {
	t.Helper()
	var p mediaProbe
	if err := json.Unmarshal(mediaCommand(t, binary, data, "-v", "error", "-show_packets", "-show_streams", "-of", "json", "pipe:0"), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func integrationLimits() fmp4.Limits {
	return fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 1 << 18, MaxMdatBytes: 1 << 20, MaxSampleBytes: 1 << 18, MaxExtradataBytes: 4096, MaxSamples: 256}
}

func truthfulResolver(t *testing.T, probe mediaProbe) func(fmp4.TrackDescription) (media.CodecConfig, error) {
	t.Helper()
	return func(d fmp4.TrackDescription) (media.CodecConfig, error) {
		if d.Codec != core.CodecAV1 {
			return fmp4.ResolveCodecConfig(d, 11, 1)
		}
		for _, s := range probe.Streams {
			if s.CodecType == "video" {
				var n, den uint32
				if _, err := fmt.Sscanf(s.FrameRate, "%d/%d", &n, &den); err != nil {
					return media.CodecConfig{}, err
				}
				if d.Width != s.Width || d.Height != s.Height || s.CodecName != "av1" || s.PixelFormat != "yuv420p" {
					return media.CodecConfig{}, errors.New("init не совпадает с независимым ffprobe")
				}
				return media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 11, Revision: 1, TimeBase: d.TimeBase, Video: core.VideoFormat{Codec: core.CodecAV1, Width: s.Width, Height: s.Height, FrameRate: core.FrameRate{Numerator: n, Denominator: den}, CodecProfile: s.Profile, PixelFormat: s.PixelFormat, BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}}}, d.Headers)
			}
		}
		return media.CodecConfig{}, errors.New("ffprobe не дал видеодорожку")
	}
}

func readMediaPackets(t *testing.T, d *fmp4.Demuxer) []media.Packet {
	t.Helper()
	var packets []media.Packet
	for {
		p, err := d.Next()
		if errors.Is(err, io.EOF) {
			return packets
		}
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, p)
		if len(packets) > 512 {
			t.Fatal("слишком большая синтетическая фикстура")
		}
	}
}

func seconds(t *testing.T, ticks int64, base string) *big.Rat {
	t.Helper()
	b, ok := new(big.Rat).SetString(base)
	if !ok {
		t.Fatal(base)
	}
	return new(big.Rat).Mul(new(big.Rat).SetInt64(ticks), b)
}

func assertProbePackets(t *testing.T, probe mediaProbe, packets []media.Packet) {
	t.Helper()
	indices := map[int]media.Track{}
	bases := map[media.Track]string{}
	for _, s := range probe.Streams {
		track := media.Track(s.CodecType)
		indices[s.Index] = track
		bases[track] = s.TimeBase
	}
	type stamp struct{ pts, dts *big.Rat }
	want := map[media.Track][]stamp{}
	for _, p := range probe.Packets {
		track := indices[p.StreamIndex]
		want[track] = append(want[track], stamp{seconds(t, p.PTS, bases[track]), seconds(t, p.DTS, bases[track])})
	}
	seen := map[media.Track]int{}
	for _, p := range packets {
		track := p.Config().Info().Track
		i := seen[track]
		seen[track]++
		if i >= len(want[track]) {
			t.Fatal("Go добавил пакет")
		}
		base := p.Config().Info().TimeBase
		baseText := fmt.Sprintf("%d/%d", base.Numerator, base.Denominator)
		if seconds(t, p.Info().PTS, baseText).Cmp(want[track][i].pts) != 0 || seconds(t, p.Info().DTS, baseText).Cmp(want[track][i].dts) != 0 {
			t.Fatalf("%s[%d] потерял рациональные PTS/DTS: %+v, ожидалось %s/%s", track, i, p.Info(), want[track][i].pts, want[track][i].dts)
		}
	}
	for track, list := range want {
		if seen[track] != len(list) {
			t.Fatalf("число пакетов %s: %d != %d", track, seen[track], len(list))
		}
	}
}

type integrationOwner struct{ done chan struct{} }

func (o integrationOwner) Do(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}
func (o integrationOwner) Valid() bool           { return true }
func (o integrationOwner) Done() <-chan struct{} { return o.done }

type splitWriter struct{ w io.Writer }

func (w splitWriter) Write(p []byte) (int, error) {
	if len(p) > 7 {
		p = p[:7]
	}
	return w.w.Write(p)
}

type splitReader struct{ r io.Reader }

func (r splitReader) Read(p []byte) (int, error) {
	if len(p) > 5 {
		p = p[:5]
	}
	return r.r.Read(p)
}

func TestAcceptanceCPUUDSSupervisorRoundTrip(t *testing.T) {
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("локальная CPU-интеграция запускается только с явно заданным FFmpeg")
	}
	probeBinary := filepath.Join(filepath.Dir(binary), "ffprobe")
	for _, scenario := range []string{"h264-copy", "av1-transcode", "h264-copy-negative", "av1-transcode-negative"} {
		t.Run(scenario, func(t *testing.T) {
			encoder := []string{"-c:v", "libx264", "-threads:v", "1", "-g", "15", "-bf", "2"}
			if strings.HasPrefix(scenario, "av1-transcode") {
				encoder = []string{"-c:v", "libaom-av1", "-threads:v", "1", "-cpu-used", "8", "-lag-in-frames", "0"}
			}
			sign := "+"
			if strings.HasSuffix(scenario, "-negative") {
				sign = "-"
			}
			args := []string{"-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=30:duration=0.4", "-f", "lavfi", "-i", "sine=sample_rate=48000:frequency=750:duration=0.4", "-map", "0:v", "-map", "1:a", "-vf", "setpts=PTS" + sign + "15", "-af", "asetpts=PTS" + sign + "6000", "-fps_mode", "passthrough"}
			args = append(args, encoder...)
			args = append(args, "-c:a", "aac", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-avoid_negative_ts", "disabled", "pipe:1")
			source := mediaCommand(t, binary, nil, args...)
			oracle := probeMedia(t, probeBinary, source)
			d, err := fmp4.NewDemuxer(bytes.NewReader(source), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 11, VideoRevision: 1, AudioRevision: 1, ResolveConfig: truthfulResolver(t, oracle)})
			if err != nil {
				t.Fatal(err)
			}
			packets := readMediaPackets(t, d)
			assertProbePackets(t, oracle, packets)
			init := d.Init()
			init.Origin = fmp4.OriginMapping{OffsetTicks: 3, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1}}
			scale, err := fmp4.MovieTimeScale(init.Video.Info().TimeBase, init.Audio.Info().TimeBase)
			if err != nil {
				t.Fatal(err)
			}
			parent := os.TempDir()
			if runtime.GOOS == "darwin" {
				// Системный TMPDIR macOS слишком длинный для вложенных путей UDS.
				parent = "/tmp"
			}
			base, err := os.MkdirTemp(parent, "gm-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(base); err != nil {
					t.Error(err)
				}
			})
			s, err := ffmpeg.New(ffmpeg.Config{BaseDir: base, StartupTimeout: 10 * time.Second, VideoTimeout: 5 * time.Second, AudioTimeout: 5 * time.Second, WatchdogInterval: 50 * time.Millisecond, MaxRestarts: 0, RestartBackoff: 10 * time.Millisecond, GracePeriod: 100 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 3 * time.Second, DiagnosticEvents: 1024, DiagnosticBytes: 1 << 20, DiagnosticChunkBytes: 256, MaxCommandBytes: 8192, MaxCommandArgs: 128})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				if err := s.Stop(ctx); err != nil {
					t.Error(err)
				}
			})
			var out boundedMediaBuffer
			out.max = 16 << 20
			var inputWire boundedMediaBuffer
			inputWire.max = 16 << 20
			var returned []media.Packet
			var returnedInit fmp4.Init
			input := func(ctx context.Context, c net.Conn, _ ffmpeg.Progress) error {
				m, err := fmp4.NewMuxer(splitWriter{io.MultiWriter(c, &inputWire)}, init, integrationLimits())
				if err != nil {
					return err
				}
				validator := fmp4.NewFFmpegTimingValidatorForOrigin(init.Origin)
				for _, p := range packets {
					if err := validator.Validate(p); err != nil {
						return err
					}
					if err := m.WritePacket(p); err != nil {
						return err
					}
				}
				if u, ok := c.(*net.UnixConn); ok {
					if err := u.CloseWrite(); err != nil {
						return err
					}
				} else {
					return errors.New("ожидался настоящий UnixConn")
				}
				<-ctx.Done()
				return ctx.Err()
			}
			output := func(ctx context.Context, c net.Conn, progress ffmpeg.Progress) error {
				d, err := fmp4.NewDemuxer(splitReader{io.TeeReader(c, &out)}, fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 21, VideoRevision: 1, AudioRevision: 1})
				if err != nil {
					return err
				}
				returnedInit = d.Init()
				for {
					p, err := d.Next()
					if errors.Is(err, io.EOF) {
						return nil
					}
					if err != nil {
						return err
					}
					returned = append(returned, p)
					track := ffmpeg.Video
					if p.Config().Info().Track == media.TrackAudio {
						track = ffmpeg.Audio
					}
					progress(track, p.Info().DTS)
				}
			}
			command := func(_ context.Context, a ffmpeg.Attempt) (ffmpeg.Command, error) {
				args := []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-xerror", "-copyts", "-f", "mp4", "-fflags", "+ignidx", "-i", "async:unix:" + a.InputPath, "-map", "0:v", "-map", "0:a"}
				if strings.HasPrefix(scenario, "h264-copy") {
					args = append(args, "-c", "copy")
				} else {
					args = append(args, "-c:v", "libx264", "-threads:v", "1", "-g", "15", "-bf", "2", "-fps_mode", "passthrough", "-c:a", "copy")
				}
				args = append(args, "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", strconv.FormatUint(uint64(scale), 10), "-avoid_negative_ts", "disabled", "unix:"+a.OutputPath)
				return ffmpeg.Command{Binary: binary, Args: args}, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := s.Start(ctx, ffmpeg.Worker{SessionID: "cpu-session", GroupID: "cpu-group", Owner: integrationOwner{make(chan struct{})}, Command: command, Input: input, Output: output}); err != nil {
				t.Fatal(err)
			}
			result, err := s.Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var diagnostics []byte
			for {
				event, err := s.NextDiagnostic(ctx)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if event.Kind == "stderr" {
					diagnostics = append(diagnostics, event.Data...)
				}
			}
			if result.Err != nil || result.CleanupErr != nil || !result.Clean || result.Attempts != 1 {
				mediaCommand(t, binary, inputWire.Bytes(), "-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-f", "mp4", "-i", "pipe:0", "-map", "0:v", "-map", "0:a", "-c", "copy", "-f", "null", "-")
				t.Log("те же входные байты успешно прочитаны FFmpeg через pipe")
				mediaCommand(t, binary, inputWire.Bytes(), "-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-f", "mp4", "-i", "pipe:0", "-map", "0:v", "-map", "0:a", "-f", "null", "-")
				t.Log("те же входные байты успешно декодированы FFmpeg через pipe")
				t.Fatalf("UDS/Supervisor: %+v: %s", result, diagnostics)
			}
			if bytes.Contains(diagnostics, []byte("Invalid DTS")) || bytes.Contains(diagnostics, []byte("Non-monotonic")) {
				t.Fatalf("DTS переписан: %s", diagnostics)
			}
			returnedOracle := probeMedia(t, probeBinary, out.Bytes())
			assertProbePackets(t, returnedOracle, returned)
			if strings.HasPrefix(scenario, "h264-copy") {
				assertProbePackets(t, oracle, returned)
			}
			if returnedInit.Video.Info().Video.Codec != core.CodecH264 || !bytes.Equal(returnedInit.Audio.Headers(), init.Audio.Headers()) {
				t.Fatal("выход потерял codec/extradata")
			}
			bframes := false
			videoFrames := 0
			presentation := make(map[string]int)
			var wantAudio, gotAudio []media.Packet
			for _, p := range packets {
				if p.Config().Info().Track == media.TrackAudio {
					wantAudio = append(wantAudio, p)
				} else {
					b := p.Config().Info().TimeBase
					presentation[seconds(t, p.Info().PTS, fmt.Sprintf("%d/%d", b.Numerator, b.Denominator)).RatString()]++
				}
			}
			for _, p := range returned {
				if p.Config().Info().Track == media.TrackVideo {
					videoFrames++
					bframes = bframes || p.Info().PTS != p.Info().DTS
					b := p.Config().Info().TimeBase
					presentation[seconds(t, p.Info().PTS, fmt.Sprintf("%d/%d", b.Numerator, b.Denominator)).RatString()]--
				} else {
					gotAudio = append(gotAudio, p)
				}
			}
			if videoFrames != 12 || !bframes || len(wantAudio) != len(gotAudio) {
				t.Fatalf("число кадров или B-кадры потеряны: video=%d B=%v audio=%d/%d", videoFrames, bframes, len(wantAudio), len(gotAudio))
			}
			for pts, count := range presentation {
				if count != 0 {
					t.Fatalf("транскодирование изменило исходный video PTS=%s: delta=%d", pts, count)
				}
			}
			for i, want := range wantAudio {
				got := gotAudio[i]
				wb, gb := want.Config().Info().TimeBase, got.Config().Info().TimeBase
				if !bytes.Equal(want.Payload(), got.Payload()) || seconds(t, want.Info().DTS, fmt.Sprintf("%d/%d", wb.Numerator, wb.Denominator)).Cmp(seconds(t, got.Info().DTS, fmt.Sprintf("%d/%d", gb.Numerator, gb.Denominator))) != 0 {
					t.Fatalf("AAC пакет %d изменился", i)
				}
			}
			decoded := mediaCommand(t, binary, out.Bytes(), "-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-i", "pipe:0", "-map", "0:v", "-map", "0:a", "-fps_mode", "passthrough", "-f", "framemd5", "pipe:1")
			videoDecoded, audioDecoded := 0, 0
			for _, line := range strings.Split(string(decoded), "\n") {
				if strings.HasPrefix(line, "0,") {
					videoDecoded++
				}
				if strings.HasPrefix(line, "1,") {
					audioDecoded++
				}
			}
			if videoDecoded != 12 || audioDecoded == 0 {
				t.Fatalf("не все кадры декодированы: video=%d audio=%d", videoDecoded, audioDecoded)
			}
			entries, err := os.ReadDir(base)
			if err != nil || len(entries) != 0 {
				t.Fatalf("UDS остались после join: %v %v", entries, err)
			}
		})
	}
}
