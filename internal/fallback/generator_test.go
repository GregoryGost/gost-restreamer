package fallback

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

func selfProfile(codec core.Codec) core.GroupProfile {
	p := core.GroupProfile{Mode: core.ModeTranscode, Format: core.MediaFormat{Video: core.VideoFormat{Codec: codec, Width: 128, Height: 128, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: "high", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 100000, MaxBitrateBPS: 200000, BufferBits: 200000, GOPFrames: 15, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 64000}}
	if codec == core.CodecAV1 {
		p.VideoEncoding.MaxBitrateBPS = p.VideoEncoding.TargetBitrateBPS
		p.Mode = core.ModeCopy
		p.Format.Video.CodecProfile = "main"
		p.Format.Video.Tier = "main"
	}
	return p
}
func selfProbeLimits() ProbeLimits {
	return ProbeLimits{MaxJSONBytes: 1 << 20, MaxWidth: 4096, MaxHeight: 4096, MaxPixels: 4096 * 4096, MaxDuration: time.Minute, ImageDuration: 1600 * time.Millisecond, MaxPackets: 10000, Container: fmp4.Limits{MaxBoxBytes: 4 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 1 << 20, MaxMdatBytes: 4 << 20, MaxSampleBytes: 1 << 20, MaxExtradataBytes: 1 << 16, MaxSamples: 10000}}
}
func selfRunner() Runner {
	return Runner{Limits: RunnerLimits{Timeout: 20 * time.Second, MaxArgs: 256, MaxCommandBytes: 1 << 16, MaxDiagnosticBytes: 1 << 20}}
}
func selfClose(t *testing.T, f *os.File) {
	t.Helper()
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
}

func TestProductionBuildersKeepHardwarePolicyAndFDInput(t *testing.T) {
	for _, c := range []core.Codec{core.CodecAV1, core.CodecH264} {
		p := selfProfile(c)
		cmd, err := ProductionBuilder("ffmpeg")(GenerateRequest{Source: Source{Kind: core.AssetImage, Duration: time.Second, Demuxer: "png_pipe"}, Profile: p, LoopDuration: 1600 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(cmd.Args, " ")
		for _, s := range []string{"-fd 3 -i fd:", "anullsrc=r=48000:cl=stereo", "-map 0:v:0 -map 1:a:0", "-c:a aac", "pipe:1"} {
			if !strings.Contains(joined, s) {
				t.Fatalf("нет %s: %s", s, joined)
			}
		}
		want := "h264_nvenc"
		if c == core.CodecAV1 {
			want = "libsvtav1"
		}
		if !strings.Contains(joined, "-c:v "+want) {
			t.Fatal(joined)
		}
		p.Format.Video.Color.Transfer = "smpte2084"
		if _, err = ProductionBuilder("ffmpeg")(GenerateRequest{Source: Source{Kind: core.AssetImage, Duration: time.Second, Demuxer: "png_pipe"}, Profile: p, LoopDuration: 1600 * time.Millisecond}); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
}

func TestRunnerChild(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-2] != "fallback-child" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "copy":
		f := os.NewFile(3, "input")
		_, _ = io.Copy(os.Stdout, f)
		_ = f.Close()
	case "overflow":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), 4096))
	case "diagnostic":
		_, _ = os.Stderr.Write(bytes.Repeat([]byte("d"), 4096))
	case "wait":
		<-time.After(time.Minute)
	}
	os.Exit(0)
}
func TestFiniteRunnerBoundsOutputDiagnosticsAndJoins(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := selfRunner()
	for _, mode := range []string{"copy", "overflow", "diagnostic", "wait"} {
		t.Run(mode, func(t *testing.T) {
			input, err := os.CreateTemp(t.TempDir(), "input")
			if err != nil {
				t.Fatal(err)
			}
			selfClose(t, input)
			if _, err = input.WriteString("owned descriptor"); err != nil {
				t.Fatal(err)
			}
			if _, err = input.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			local := r
			local.Limits.MaxDiagnosticBytes = 32
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "wait" {
				cancel()
			}
			var out bytes.Buffer
			_, err = local.Run(ctx, Command{Binary: binary, Args: []string{"-test.run=^TestRunnerChild$", "fallback-child", mode}}, input, &boundedWriter{w: &out, remaining: 32})
			if mode == "copy" {
				if err != nil || out.String() != "owned descriptor" {
					t.Fatal(out.String(), err)
				}
			} else if mode == "wait" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrLimit) {
				t.Fatal(err)
			}
		})
	}
	r.Limits.MaxCommandBytes = 1
	if _, err = r.Run(context.Background(), Command{Binary: binary}, nil, nil); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}

func TestRunnerRejectsUntrustedCommandBudgetsBeforeStart(t *testing.T) {
	r := selfRunner()
	for _, command := range []Command{{}, {Binary: "bad\x00binary"}, {Binary: "missing", Args: []string{"bad\x00arg"}}, {Binary: "missing", Args: make([]string, r.Limits.MaxArgs+1)}, {Binary: "missing", Args: []string{strings.Repeat("x", int(r.Limits.MaxCommandBytes))}}} {
		if _, err := r.Run(context.Background(), command, nil, nil); err == nil || (!errors.Is(err, ErrInvalid) && !errors.Is(err, ErrLimit)) {
			t.Fatal(command, err)
		}
	}
	for _, source := range []core.VideoFormat{{PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Transfer: "smpte2084"}}, {PixelFormat: "yuv420p10le", BitDepth: 10}} {
		if _, err := ProductionBuilder("ffmpeg")(GenerateRequest{Source: Source{Kind: core.AssetVideo, Format: core.SourceFormat{Video: &source}, Duration: time.Second, Demuxer: "mov"}, Profile: selfProfile(core.CodecH264), LoopDuration: 1600 * time.Millisecond}); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
}

func TestSourceImagesUseMeasuredDimensionsAndBudgets(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "image")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, file)
	if err = png.Encode(file, image.NewRGBA(image.Rect(0, 0, 128, 64))); err != nil {
		t.Fatal(err)
	}
	p := Prober{Limits: selfProbeLimits()}
	s, err := p.Source(context.Background(), file, core.AssetImage)
	if err != nil || s.Width != 128 || s.Height != 64 || s.Format.ImageFormat != "png" || s.Format.Audio != nil {
		t.Fatal(s, err)
	}
	p.Limits.MaxWidth = 127
	if _, err = p.Source(context.Background(), file, core.AssetImage); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if _, err = p.Source(context.Background(), file, "other"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

// Кодировщик CPU подставляется только этой явно включаемой локальной проверкой.
func TestCPUImageGenerationAndActualProbe(t *testing.T) {
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("локальная CPU-проверка не включена")
	}
	source, err := os.CreateTemp(t.TempDir(), "source.png")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, source)
	img := image.NewRGBA(image.Rect(0, 0, 128, 128))
	for y := range 128 {
		for x := range 128 {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}
	if err = png.Encode(source, img); err != nil {
		t.Fatal(err)
	}
	r := selfRunner()
	registry := fallbackProcessRegistry(t)
	scope, err := registry.Create(context.Background(), "cpu-image-preparation")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close() })
	r.Scope = scope
	p := Prober{Binary: filepath.Join(filepath.Dir(binary), "ffprobe"), Runner: r, Limits: selfProbeLimits()}
	p.Limits.ImageDuration = time.Second
	observed, err := p.Source(context.Background(), source, core.AssetImage)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []core.Codec{core.CodecH264, core.CodecAV1} {
		t.Run(string(c), func(t *testing.T) {
			profile := selfProfile(c)
			encoder := "libx264"
			if c == core.CodecAV1 {
				encoder = "libsvtav1"
			}
			loop, err := EffectiveLoopDuration(observed.Duration, profile, p.Limits.MaxDuration)
			if err != nil {
				t.Fatal(err)
			}
			cmd, err := buildCommand(binary, GenerateRequest{Source: observed, Profile: profile, LoopDuration: loop}, encoder)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = source.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			output, err := os.CreateTemp(t.TempDir(), "variant.mp4")
			if err != nil {
				t.Fatal(err)
			}
			selfClose(t, output)
			result, err := r.Run(context.Background(), cmd, source, &boundedWriter{w: output, remaining: 8 << 20})
			if err != nil {
				t.Fatalf("generator: %v: %s", err, result.Diagnostics)
			}
			{
				normalized, e := os.CreateTemp(t.TempDir(), "normalized.mp4")
				if e != nil {
					t.Fatal(e)
				}
				selfClose(t, normalized)
				if e = p.NormalizePrepared(context.Background(), output, &boundedWriter{w: normalized, remaining: 8 << 20}); e != nil {
					t.Fatalf("normalize: %v", e)
				}
				selfComparePackets(t, p, output, normalized, true)
				output = normalized
			}
			actual, err := p.Verify(context.Background(), output, profile)
			if err != nil {
				t.Fatalf("verify actual %+v: %v", actual, err)
			}
		})
	}
	if err = scope.Seal(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = scope.Close(); err != nil {
		t.Fatal(err)
	}
	proof, err := scope.Resolve(context.Background())
	if err != nil || !proof.Joined {
		t.Fatalf("CPU-подготовка не присоединила процессы: %+v %v", proof, err)
	}
}

func selfComparePackets(t *testing.T, p Prober, raw, normalized *os.File, skipPriming bool) {
	t.Helper()
	read := func(f *os.File) []media.Packet {
		actual, err := p.observedFormat(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		d, err := fmp4.NewDemuxer(f, fmp4.DemuxOptions{Limits: p.Limits.Container, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
			info := media.CodecConfigInfo{Track: desc.Track, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase}
			if desc.Track == media.TrackVideo {
				info.Video = actual.Video
			} else {
				info.Audio = actual.Audio
			}
			return media.NewCodecConfig(info, desc.Headers)
		}})
		if err != nil {
			t.Fatal(err)
		}
		var packets []media.Packet
		for {
			packet, err := d.Next()
			if err == io.EOF {
				return packets
			}
			if err != nil {
				t.Fatal(err)
			}
			packets = append(packets, packet)
		}
	}
	a, b := read(raw), read(normalized)
	if skipPriming {
		for i, packet := range a {
			if packet.Config().Info().Track == media.TrackAudio {
				a = append(a[:i], a[i+1:]...)
				break
			}
		}
	}
	if len(a) != len(b) {
		t.Fatalf("число samples изменилось: %d/%d", len(a), len(b))
	}
	for i := range a {
		if a[i].Info() != b[i].Info() || a[i].Config().Info().TimeBase != b[i].Config().Info().TimeBase || a[i].Config().Info().Track != b[i].Config().Info().Track || !bytes.Equal(a[i].Payload(), b[i].Payload()) {
			t.Fatalf("sample %d изменён при нормализации", i)
		}
	}
}

func TestCPUVideoWithoutAudioReceivesVerifiedSilence(t *testing.T) {
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("локальная CPU-проверка не включена")
	}
	source, err := os.CreateTemp(t.TempDir(), "source.mp4")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, source)
	r := selfRunner()
	command := Command{Binary: binary, Args: []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-xerror", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=30:duration=1.13", "-an", "-c:v", "libx264", "-bf", "0", "-vf", "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "pipe:1"}}
	if result, err := r.Run(context.Background(), command, nil, &boundedWriter{w: source, remaining: 8 << 20}); err != nil {
		t.Fatalf("source: %v: %s", err, result.Diagnostics)
	}
	p := Prober{Binary: filepath.Join(filepath.Dir(binary), "ffprobe"), Runner: r, Limits: selfProbeLimits()}
	observed, err := p.Source(context.Background(), source, core.AssetVideo)
	if err != nil || observed.Format.Audio != nil {
		t.Fatal(observed, err)
	}
	profile := selfProfile(core.CodecH264)
	loop, err := EffectiveLoopDuration(observed.Duration, profile, p.Limits.MaxDuration)
	if err != nil {
		t.Fatal(err)
	}
	command, err = buildCommand(binary, GenerateRequest{Source: observed, Profile: profile, LoopDuration: loop}, "libx264")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(t.TempDir(), "output.mp4")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, output)
	if result, err := r.Run(context.Background(), command, source, &boundedWriter{w: output, remaining: 8 << 20}); err != nil {
		t.Fatalf("generate: %v: %s", err, result.Diagnostics)
	}
	normalized, err := os.CreateTemp(t.TempDir(), "normalized.mp4")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, normalized)
	if err = p.NormalizePrepared(context.Background(), output, &boundedWriter{w: normalized, remaining: 8 << 20}); err != nil {
		t.Fatal(err)
	}
	selfComparePackets(t, p, output, normalized, true)
	actual, err := p.Verify(context.Background(), normalized, profile)
	if err != nil || actual.Audio.Codec != core.CodecAAC || actual.Audio.Channels != 2 {
		t.Fatal(actual, err)
	}
}

func TestCPUShortVideoRetainsToneAndPadsAFullLoop(t *testing.T) {
	binaryPath := os.Getenv("GOST_MEDIA_FFMPEG")
	if binaryPath == "" {
		t.Skip("локальная CPU-проверка не включена")
	}
	r := selfRunner()
	source, err := os.CreateTemp(t.TempDir(), "source-with-audio.mp4")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, source)
	command := Command{Binary: binaryPath, Args: []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-xerror", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=30:duration=1.13", "-f", "lavfi", "-i", "sine=frequency=750:sample_rate=48000:duration=1.13", "-map", "0:v:0", "-map", "1:a:0", "-c:v", "libx264", "-bf", "0", "-c:a", "aac", "-ar", "48000", "-ac", "2", "-vf", "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709", "-t", "1.13", "-f", "mp4", "-movflags", "+faststart", "-y", source.Name()}}
	if result, err := r.Run(context.Background(), command, nil, nil); err != nil {
		t.Fatalf("source: %v: %s", err, result.Diagnostics)
	}
	p := Prober{Binary: filepath.Join(filepath.Dir(binaryPath), "ffprobe"), Runner: r, Limits: selfProbeLimits()}
	observed, err := p.Source(context.Background(), source, core.AssetVideo)
	if err != nil || observed.Format.Audio == nil {
		t.Fatal(observed, err)
	}
	profile := selfProfile(core.CodecH264)
	loop, err := EffectiveLoopDuration(observed.Duration, profile, p.Limits.MaxDuration)
	if err != nil || loop != 1600*time.Millisecond {
		t.Fatal(observed.Duration, loop, err)
	}
	command, err = buildCommand(binaryPath, GenerateRequest{Source: observed, Profile: profile, LoopDuration: loop}, "libx264")
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(source.Name())
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, input)
	raw, err := os.CreateTemp(t.TempDir(), "raw.mp4")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, raw)
	if result, err := r.Run(context.Background(), command, input, &boundedWriter{w: raw, remaining: 8 << 20}); err != nil {
		t.Fatalf("generate: %v: %s", err, result.Diagnostics)
	}
	normalized, err := os.CreateTemp(t.TempDir(), "normalized.mp4")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, normalized)
	if err = p.NormalizePrepared(context.Background(), raw, &boundedWriter{w: normalized, remaining: 8 << 20}); err != nil {
		t.Fatal(err)
	}
	selfComparePackets(t, p, raw, normalized, true)
	if _, err = p.Verify(context.Background(), normalized, profile); err != nil {
		t.Fatal(err)
	}
	if err = p.VerifyLoopDuration(context.Background(), normalized, loop); err != nil {
		t.Fatal(err)
	}
	doc, err := p.inspect(context.Background(), normalized)
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range doc.Streams {
		start, err := strconv.ParseFloat(stream.StartTime, 64)
		if err != nil || start != 0 {
			t.Fatalf("начало %s=%s: %v", stream.Type, stream.StartTime, err)
		}
	}
	decode := func(args ...string) []byte {
		file, err := os.Open(normalized.Name())
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		}()
		var out bytes.Buffer
		argv := []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-xerror", "-protocol_whitelist", "fd,pipe", "-fd", "3", "-i", "fd:"}
		argv = append(argv, args...)
		argv = append(argv, "pipe:1")
		result, err := r.Run(context.Background(), Command{Binary: binaryPath, Args: argv}, file, &boundedWriter{w: &out, remaining: 2 << 20})
		if err != nil {
			t.Fatalf("decode: %v: %s", err, result.Diagnostics)
		}
		return out.Bytes()
	}
	video := decode("-map", "0:v:0", "-an", "-fps_mode:v", "passthrough", "-f", "framemd5")
	frames := 0
	for _, line := range strings.Split(string(video), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			frames++
		}
	}
	if frames != 48 {
		t.Fatalf("число декодированных видеокадров %d !=48", frames)
	}
	pcm := decode("-map", "0:a:0", "-vn", "-c:a", "pcm_s16le", "-f", "s16le")
	if len(pcm) != 76800*2*2 {
		t.Fatalf("число аудиосэмплов PCM на канал %d !=76800", len(pcm)/4)
	}
	window := func(from, to int, frequency float64) (power, spectral float64) {
		var sinSum, cosSum float64
		for n := from; n < to; n++ {
			sample := float64(int16(binary.LittleEndian.Uint16(pcm[n*4:])))
			power += sample * sample
			phase := 2 * math.Pi * frequency * float64(n) / 48000
			sinSum += sample * math.Sin(phase)
			cosSum += sample * math.Cos(phase)
		}
		count := float64(to - from)
		return power / count, (sinSum*sinSum + cosSum*cosSum) / (count * count)
	}
	tonePower, toneSpectrum := window(4800, 43200, 750)
	_, otherSpectrum := window(4800, 43200, 500)
	quietPower, _ := window(67200, 72000, 750)
	if tonePower < 10000 || toneSpectrum < 20*otherSpectrum || quietPower > tonePower/100 {
		t.Fatalf("тон или тихий хвост не сохранён: power=%g tone=%g other=%g quiet=%g", tonePower, toneSpectrum, otherSpectrum, quietPower)
	}
}
