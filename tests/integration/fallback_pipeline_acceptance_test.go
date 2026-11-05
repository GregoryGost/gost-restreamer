package integration

import (
	"bytes"
	"context"
	encodingbinary "encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

func fallbackPipelineProfile(codec core.Codec) core.GroupProfile {
	mode, profile := core.ModeCopy, "main"
	if codec == core.CodecH264 {
		mode, profile = core.ModeTranscode, "high"
	}
	return core.GroupProfile{Mode: mode, Format: core.MediaFormat{Video: core.VideoFormat{Codec: codec, Width: 128, Height: 128, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: profile, PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 200000, MaxBitrateBPS: 200000, BufferBits: 400000, GOPFrames: 15, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 64000}}
}

// Подстановка кодировщика принадлежит только CPU-тесту; остальные аргументы формирует ProductionBuilder.
func fallbackPipelineCPUBuilder(binary string) fallback.CommandBuilder {
	return func(request fallback.GenerateRequest) (fallback.Command, error) {
		c, err := fallback.ProductionBuilder(binary)(request)
		if err != nil {
			return c, err
		}
		args := make([]string, 0, len(c.Args)+8)
		for i := 0; i < len(c.Args); i++ {
			arg := c.Args[i]
			if arg == "-svtav1-params" || arg == "-preset" || arg == "-no-scenecut" || arg == "-forced-idr" {
				i++
				continue
			}
			if arg == "h264_nvenc" {
				arg = "libx264"
			}
			if arg == "libsvtav1" {
				arg = "libaom-av1"
			}
			args = append(args, arg)
		}
		output := args[len(args)-1]
		args = args[:len(args)-1]
		if request.Profile.Format.Video.Codec == core.CodecAV1 {
			args = append(args, "-threads:v", "1", "-cpu-used", "8", "-lag-in-frames", "0")
		} else {
			args = append(args, "-threads:v", "1", "-sc_threshold", "0")
		}
		args = append(args, output)
		c.Args = args
		return c, nil
	}
}

type fallbackPipelineRunner struct {
	t      *testing.T
	runner fallback.Runner
}

type fallbackActualSource struct {
	init    fmp4.Init
	packets [2][]media.Packet
	next    [2]int
}

func fallbackActualPackets(t *testing.T, probe fallback.Prober, data []byte) fallbackActualSource {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "actual-source")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err = file.Write(data); err != nil {
		t.Fatal(err)
	}
	observed, err := probe.Source(context.Background(), file, core.AssetVideo)
	if err != nil {
		t.Fatal(err)
	}
	d, err := fmp4.NewDemuxer(bytes.NewReader(data), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		if desc.Track == media.TrackAudio {
			return fmp4.ResolveCodecConfig(desc, 1, 1)
		}
		if observed.Format.Video == nil {
			return media.CodecConfig{}, core.ErrCorrupt
		}
		return media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase, Video: *observed.Format.Video}, desc.Headers)
	}})
	if err != nil {
		t.Fatal(err)
	}
	result := fallbackActualSource{init: d.Init()}
	for {
		p, e := d.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		index := 0
		if p.Config().Info().Track == media.TrackAudio {
			index = 1
		}
		result.packets[index] = append(result.packets[index], p)
	}
	if len(result.packets[0]) != 48 || len(result.packets[1]) == 0 {
		t.Fatalf("независимый 1,6-секундный источник: %d video, %d audio", len(result.packets[0]), len(result.packets[1]))
	}
	return result
}

func fallbackActualTime(ticks int64, base media.TimeBase) *big.Rat {
	return new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(ticks), new(big.Int).SetUint64(uint64(base.Numerator))), new(big.Int).SetUint64(uint64(base.Denominator)))
}

func fallbackPipelineActualTransitions(t *testing.T, binary string, probe fallback.Prober, profile core.GroupProfile, prepared []byte) {
	t.Helper()
	args := []string{"-hide_banner", "-loglevel", "warning", "-xerror", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=30:duration=1.6", "-f", "lavfi", "-i", "sine=frequency=750:sample_rate=48000:duration=1.6", "-map", "0:v:0", "-map", "1:a:0", "-threads:v", "1", "-g:v", "3", "-bf:v", "0", "-fps_mode:v", "passthrough", "-vf", "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709", "-color_range:v", "tv", "-color_primaries:v", "bt709", "-color_trc:v", "bt709", "-colorspace:v", "bt709"}
	if profile.Format.Video.Codec == core.CodecAV1 {
		args = append(args, "-c:v", "libaom-av1", "-cpu-used", "8", "-lag-in-frames", "0", "-enable-cdef", "0", "-b:v", "250000")
	} else {
		args = append(args, "-c:v", "libx264", "-profile:v", "high", "-refs:v", "1", "-sc_threshold", "0", "-b:v", "250000")
	}
	args = append(args, "-c:a", "aac", "-b:a", "64000", "-ar:a", "48000", "-ac:a", "2", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-avoid_negative_ts", "disabled", "pipe:1")
	independent := mediaCommand(t, binary, nil, args...)
	fallbackSource := fallbackActualPackets(t, probe, prepared)
	liveSource := fallbackActualPackets(t, probe, independent)
	if bytes.Equal(fallbackSource.init.Video.Headers(), liveSource.init.Video.Headers()) {
		t.Fatal("независимые кодирования должны проверять переход между различными actual headers")
	}
	queue := media.QueueLimits{MaxPackets: 512, MaxBytes: 8 << 20, MaxPacketBytes: 1 << 20, MaxConfigBytes: 1 << 12, ResyncTimeout: 5 * time.Second}
	watchdog := core.WatchdogPolicy{VideoTimeout: 100 * time.Millisecond, AudioTimeout: 100 * time.Millisecond, RecoveryWindow: 100 * time.Millisecond, KeyframeTimeout: time.Second}
	sw, err := group.NewSwitcher(group.SwitcherConfig{Profile: profile, Watchdog: watchdog, Queue: queue, Router: media.RouterLimits{MaxConsumers: 2, MaxRetainedBytes: 20 << 20, MaxConfigBytes: 1 << 12}, MaxAVSkew: 100 * time.Millisecond, MaxTransitionGap: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sw.Close() }()
	sub, err := sw.Router().Subscribe(queue)
	if err != nil {
		t.Fatal(err)
	}
	sources := map[group.Feed]*fallbackActualSource{group.Fallback: &fallbackSource, group.Live: &liveSource}
	var captured []media.Packet
	shifts := map[uint64][]*big.Rat{}
	epochs := map[uint64]group.Feed{}
	last := map[media.Track]*big.Rat{}
	horizon := new(big.Rat)
	seenHorizon := false
	lastEpoch := uint64(0)
	modes := []group.Feed{}
	appendOutput := func(p media.Packet, feed group.Feed) {
		source := sources[feed]
		index := 0
		if p.Config().Info().Track == media.TrackAudio {
			index = 1
		}
		stamp := p.Info()
		base := p.Config().Info().TimeBase
		dts, pts := fallbackActualTime(stamp.DTS, base), fallbackActualTime(stamp.PTS, base)
		duration := fallbackActualTime(stamp.DurationTicks, base)
		expected := big.NewRat(1, 30)
		if index == 1 {
			expected = big.NewRat(1024, 48000)
		}
		if duration.Cmp(expected) != 0 || stamp.PTS != stamp.DTS {
			t.Fatalf("actual Switcher изменил полный AU/CTS: %+v", stamp)
		}
		if previous := last[p.Config().Info().Track]; previous != nil && dts.Cmp(previous) <= 0 {
			t.Fatal("actual переход сбросил DTS")
		}
		last[p.Config().Info().Track] = dts
		epoch := p.Config().Info().SourceEpoch
		if epoch != lastEpoch {
			if index != 0 || !stamp.Keyframe {
				t.Fatal("новый actual сегмент начался без независимого video AU")
			}
			if seenHorizon {
				gap := new(big.Rat).Sub(dts, horizon)
				if gap.Sign() < 0 || gap.Cmp(big.NewRat(1, 10)) > 0 {
					t.Fatalf("actual переход нарушил horizon/gap: %s", gap)
				}
			}
			epochs[epoch] = feed
			lastEpoch = epoch
		}
		end := new(big.Rat).Add(pts, duration)
		if !seenHorizon || end.Cmp(horizon) > 0 {
			horizon = end
			seenHorizon = true
		}
		var candidates []*big.Rat
		for _, input := range source.packets[index] {
			if !bytes.Equal(input.Payload(), p.Payload()) {
				continue
			}
			inputStamp := input.Info()
			inputBase := input.Config().Info().TimeBase
			offset := new(big.Rat).Sub(dts, fallbackActualTime(inputStamp.DTS, inputBase))
			if new(big.Rat).Sub(pts, fallbackActualTime(inputStamp.PTS, inputBase)).Cmp(offset) != 0 {
				continue
			}
			if prior, ok := shifts[epoch]; ok {
				for _, value := range prior {
					if value.Cmp(offset) == 0 {
						candidates = append(candidates, offset)
						break
					}
				}
			} else {
				candidates = append(candidates, offset)
			}
		}
		if len(candidates) == 0 {
			t.Fatal("нет единого точного A/V offset, сохраняющего actual payload/PTS/DTS")
		}
		shifts[epoch] = candidates
		captured = append(captured, p)
	}
	now := time.Unix(2000, 0)
	pushSource := func(feed group.Feed, frame int, at time.Time) {
		source := sources[feed]
		end := big.NewRat(int64(frame+1), 30)
		for track := 0; track < 2; track++ {
			for source.next[track] < len(source.packets[track]) {
				p := source.packets[track][source.next[track]]
				if fallbackActualTime(p.Info().DTS, p.Config().Info().TimeBase).Cmp(end) >= 0 {
					break
				}
				source.next[track]++
				if e := sw.Push(feed, 1, p, source.init.Video, source.init.Audio, at); e != nil && !errors.Is(e, group.ErrUnavailable) {
					t.Fatalf("actual %v: %v", feed, e)
				}
			}
		}
	}
	for frame := 0; frame < 48; frame++ {
		at := now.Add(time.Duration(frame) * time.Second / 30)
		if frame == 24 {
			_ = sw.SetPaused(true, at)
		}
		if frame == 32 {
			_ = sw.SetPaused(false, at)
		}
		pushSource(group.Fallback, frame, at)
		if frame >= 10 && frame < 39 {
			pushSource(group.Live, frame-10, at)
		}
		_ = sw.Tick(at)
		status := sw.Status()
		if !status.Available {
			continue
		}
		if len(modes) == 0 || modes[len(modes)-1] != status.Source {
			modes = append(modes, status.Source)
		}
		// Push публикует синхронно; ограниченное ожидание Next завершает опустошение очереди.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		for {
			d, e := sub.Next(ctx)
			if errors.Is(e, context.DeadlineExceeded) {
				break
			}
			if e != nil {
				cancel()
				t.Fatalf("actual output: %v", e)
			}
			p := d.Packet
			appendOutput(p, epochsOrCurrent(epochs, p.Config().Info().SourceEpoch, status.Source))
		}
		cancel()
	}
	if len(modes) < 5 || modes[0] != group.Fallback || modes[1] != group.Live || modes[2] != group.Fallback || modes[3] != group.Live || modes[len(modes)-1] != group.Fallback {
		t.Fatalf("не выполнены actual recovery/pause/resume/watchdog переходы: %v", modes)
	}
	for epoch := range epochs {
		if len(shifts[epoch]) == 0 {
			t.Fatal("не проверен общий A/V offset")
		}
	}
	fallbackDecodeMixedElementary(t, binary, profile, captured)
}

func epochsOrCurrent(epochs map[uint64]group.Feed, epoch uint64, current group.Feed) group.Feed {
	if feed, ok := epochs[epoch]; ok {
		return feed
	}
	return current
}

func fallbackDecodeMixedElementary(t *testing.T, binary string, profile core.GroupProfile, packets []media.Packet) {
	t.Helper()
	var video, audio bytes.Buffer
	videos, audios := 0, 0
	epoch := uint64(0)
	for _, p := range packets {
		c := p.Config()
		if c.Info().Track == media.TrackVideo {
			videos++
			if profile.Format.Video.Codec == core.CodecAV1 {
				// MP4 уже задал границу полного TU; демультиплексор OBU получает её как пустой Temporal Delimiter.
				payload := p.Payload()
				if payload[0]>>3&15 != 2 {
					video.Write([]byte{0x12, 0})
				}
				video.Write(payload)
			} else {
				if c.Info().SourceEpoch != epoch {
					headers := c.Headers()
					if len(headers) < 8 {
						t.Fatal("обрезан avcC")
					}
					position := 6
					count := int(headers[5] & 31)
					for pass := 0; pass < 2; pass++ {
						for i := 0; i < count; i++ {
							if len(headers)-position < 2 {
								t.Fatal("обрезана длина avcC")
							}
							n := int(encodingbinary.BigEndian.Uint16(headers[position:]))
							position += 2
							if len(headers)-position < n {
								t.Fatal("обрезан SPS/PPS")
							}
							video.Write([]byte{0, 0, 0, 1})
							video.Write(headers[position : position+n])
							position += n
						}
						if pass == 0 {
							if position >= len(headers) {
								t.Fatal("нет PPS")
							}
							count = int(headers[position])
							position++
						}
					}
				}
				data := p.Payload()
				for len(data) > 0 {
					if len(data) < 4 {
						t.Fatal("обрезан AVCC NAL")
					}
					n := int(encodingbinary.BigEndian.Uint32(data))
					data = data[4:]
					if n <= 0 || n > len(data) {
						t.Fatal("неверная длина NAL")
					}
					video.Write([]byte{0, 0, 0, 1})
					video.Write(data[:n])
					data = data[n:]
				}
			}
			epoch = c.Info().SourceEpoch
			continue
		}
		actual, err := codec.AudioFormatASC(c.Headers())
		if err != nil || actual != profile.Format.Audio {
			t.Fatalf("actual ASC: %+v %v", actual, err)
		}
		headers := c.Headers()
		objectType := headers[0] >> 3
		rateIndex := (headers[0]&7)<<1 | headers[1]>>7
		channels := headers[1] >> 3 & 15
		if objectType != 2 || rateIndex > 12 || channels == 0 || channels > 7 {
			t.Fatal("ASC не представим в ADTS")
		}
		payload := p.Payload()
		length := len(payload) + 7
		if length > 8191 {
			t.Fatal("AU превышает ADTS length")
		}
		audio.Write([]byte{0xff, 0xf1, (objectType-1)<<6 | rateIndex<<2 | channels>>2, (channels&3)<<6 | byte(length>>11), byte(length >> 3), byte(length&7)<<5 | 31, 0xfc})
		audio.Write(payload)
		audios++
	}
	format := "h264"
	if profile.Format.Video.Codec == core.CodecAV1 {
		format = "obu"
	}
	decodedVideo := mediaCommand(t, binary, video.Bytes(), "-hide_banner", "-loglevel", "error", "-xerror", "-f", format, "-i", "pipe:0", "-map", "0:v:0", "-fps_mode", "passthrough", "-pix_fmt", "gray", "-f", "rawvideo", "pipe:1")
	if len(decodedVideo) != videos*128*128 {
		t.Fatalf("mixed actual video decode count: bytes=%d frames=%d", len(decodedVideo), videos)
	}
	pcm := mediaCommand(t, binary, audio.Bytes(), "-hide_banner", "-loglevel", "error", "-xerror", "-flags2:a", "+skip_manual", "-f", "aac", "-i", "pipe:0", "-map", "0:a:0", "-f", "s16le", "pipe:1")
	if len(pcm) != audios*1024*2*2 {
		t.Fatalf("mixed actual AAC sample count: bytes=%d AU=%d", len(pcm), audios)
	}
	var energy750, energy500, energy1000 float64
	for block := 0; block < audios; block++ {
		var s750, c750, s500, c500, s1000, c1000 float64
		for n := 0; n < 1024; n++ {
			sample := float64(int16(encodingbinary.LittleEndian.Uint16(pcm[(block*1024+n)*4:])))
			x := 2 * math.Pi * float64(n) / 48000
			s750 += sample * math.Sin(750*x)
			c750 += sample * math.Cos(750*x)
			s500 += sample * math.Sin(500*x)
			c500 += sample * math.Cos(500*x)
			s1000 += sample * math.Sin(1000*x)
			c1000 += sample * math.Cos(1000*x)
		}
		energy750 += s750*s750 + c750*c750
		energy500 += s500*s500 + c500*c500
		energy1000 += s1000*s1000 + c1000*c1000
	}
	if energy750 == 0 || energy750 <= energy500+energy1000 {
		t.Fatalf("actual mixed AAC потерял независимый тон 750 Hz: energy=%g/%g/%g", energy750, energy500, energy1000)
	}
}

func (r fallbackPipelineRunner) Run(ctx context.Context, c fallback.Command, f *os.File, w io.Writer) (fallback.RunResult, error) {
	result, err := r.runner.Run(ctx, c, f, w)
	if err != nil {
		r.t.Logf("finite process binary=%s args=%q error=%v diagnostics=%s", c.Binary, c.Args, err, result.Diagnostics)
	}
	return result, err
}

func TestFallbackPipelineCPUGenerationFDLoopsAndDecode(t *testing.T) {
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("короткая CPU-проверка требует явно заданного GOST_MEDIA_FFMPEG")
	}
	for _, scenario := range []struct {
		name          string
		codec         core.Codec
		production    bool
		imageDuration time.Duration
	}{{"av1-cpu", core.CodecAV1, false, 1600 * time.Millisecond}, {"h264-cpu", core.CodecH264, false, 1600 * time.Millisecond}, {"av1-production-svt", core.CodecAV1, true, 1600 * time.Millisecond}, {"h264-short-image", core.CodecH264, false, time.Second}} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			options := storage.Options{PreparationLimits: core.PreparationLimits{MaxQueued: 3, MaxProfiles: 8}, OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 128, MaxIdentifierBytes: 1024, MaxBytes: 4 << 20}, ObservationLimits: core.ObservationLimits{MaxSessions: 8, MaxGroups: 32, MaxOutputs: 64, MaxFFmpegWorkers: 32, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond}
			store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "pipeline.sqlite"), options)
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
			asset, err := svc.CreateAsset(ctx, "CPU-заглушка")
			if err != nil {
				t.Fatal(err)
			}
			watchdog := core.WatchdogPolicy{VideoTimeout: 2 * time.Second, AudioTimeout: 2 * time.Second, RecoveryWindow: 100 * time.Millisecond, KeyframeTimeout: time.Second}
			stream, err := svc.CreateStream(ctx, core.Stream{Name: "CPU-поток", PublicationKey: "cpu-fallback-key", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: watchdog}})
			if err != nil {
				t.Fatal(err)
			}
			profile, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "CPU-профиль", Profile: fallbackPipelineProfile(scenario.codec)})
			if err != nil {
				t.Fatal(err)
			}
			configured, err := svc.CreateGroup(ctx, core.TranscodeGroup{Name: "CPU-группа", StreamID: stream.ID, ProfileID: profile.ID})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = svc.CreateDestination(ctx, core.Destination{Name: "CPU-выход", StreamID: stream.ID, GroupID: configured.ID, Platform: "custom", Endpoint: "rtmp://example.invalid/live", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			files, err := fallback.OpenFileStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := files.Close(); err != nil {
					t.Error(err)
				}
			})
			runner := fallback.Runner{Limits: fallback.RunnerLimits{Timeout: 10 * time.Second, MaxArgs: 256, MaxCommandBytes: 1 << 16, MaxDiagnosticBytes: 1 << 20}}
			// 48 кадров при 30 FPS и 75 AAC AU по 1024 сэмпла при 48 kHz занимают ровно 1,6 секунды.
			probeLimits := fallback.ProbeLimits{MaxJSONBytes: 1 << 20, MaxWidth: 1024, MaxHeight: 1024, MaxPixels: 1024 * 1024, MaxDuration: 10 * time.Second, ImageDuration: scenario.imageDuration, MaxPackets: 4096, Container: integrationLimits()}
			actualRunner := fallbackPipelineRunner{t: t, runner: runner}
			probe := fallback.Prober{Binary: filepath.Join(filepath.Dir(binary), "ffprobe"), Runner: actualRunner, Limits: probeLimits}
			config := fallback.Config{ReconcileLimit: 10, GeneratorVersion: "cpu-integration", FFmpeg: binary, FFprobe: probe.Binary, MaxUploadBytes: 1 << 20, MaxOutputBytes: 8 << 20, PrepareTimeout: 15 * time.Second, ShutdownTimeout: 3 * time.Second, QueueInterval: time.Millisecond, Runner: runner.Limits, Probe: probeLimits}
			builder := fallbackPipelineCPUBuilder(binary)
			if scenario.production {
				builder = fallback.ProductionBuilder(binary)
			}
			manager, err := fallback.NewManagerWithTools(svc, files, config, fallback.Tools{Builder: builder, Inspector: probe, Runner: actualRunner, Normalize: probe.NormalizePrepared, VerifyLoopDuration: probe.VerifyLoopDuration})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Close(context.Background()) })
			img := image.NewRGBA(image.Rect(0, 0, 128, 128))
			for y := 0; y < 128; y++ {
				for x := 0; x < 128; x++ {
					img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 40, A: 255})
				}
			}
			var pngData bytes.Buffer
			if err = png.Encode(&pngData, img); err != nil {
				t.Fatal(err)
			}
			job, err := manager.Upload(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, io.NopCloser(bytes.NewReader(pngData.Bytes())))
			if err != nil {
				t.Fatal(err)
			}
			result, err := job.Wait(ctx)
			if err != nil || !result.Clean {
				t.Fatalf("рабочие upload/generator/probe/publication: generation=%s clean=%v error=%v", result.Generation.ID, result.Clean, err)
			}
			if len(result.Generation.RequiredProfiles) != 1 {
				t.Fatal("не подготовлен точный профиль")
			}
			prepared := result.Generation.RequiredProfiles[0]
			variant, err := svc.GetVariant(ctx, prepared.VariantID)
			if err != nil {
				t.Fatal(err)
			}
			preparedFile, err := files.OpenExact(ctx, variant.ManagedPath, variant.SizeBytes, variant.ContentHash)
			if err != nil {
				t.Fatal(err)
			}
			preparedBytes, readErr := io.ReadAll(preparedFile)
			closeErr := preparedFile.Close()
			if readErr != nil || closeErr != nil {
				t.Fatal(errors.Join(readErr, closeErr))
			}
			t.Logf("prepared packet oracle: %s", mediaCommand(t, probe.Binary, preparedBytes, "-v", "error", "-select_streams", "v:0", "-read_intervals", "%+#3", "-show_entries", "packet=pts,dts,duration,time_base", "-of", "json", "pipe:0"))
			var audioOracle struct {
				Packets []json.RawMessage `json:"packets"`
				Streams json.RawMessage   `json:"streams"`
				Format  json.RawMessage   `json:"format"`
			}
			if err = json.Unmarshal(mediaCommand(t, probe.Binary, preparedBytes, "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=time_base,start_pts,duration_ts:format=duration:packet=pts,dts,duration,side_data_list:packet_side_data=side_data_type,skip_samples,discard_padding,skip_reason,discard_reason", "-of", "json", "pipe:0"), &audioOracle); err != nil {
				t.Fatal(err)
			}
			if len(audioOracle.Packets) > 0 {
				t.Logf("prepared AAC: count=%d first=%s last=%s streams=%s format=%s", len(audioOracle.Packets), audioOracle.Packets[0], audioOracle.Packets[len(audioOracle.Packets)-1], audioOracle.Streams, audioOracle.Format)
			}
			fallbackPipelineActualTransitions(t, binary, probe, profile.Profile, preparedBytes)
			request := core.StartSessionRequest{RequestID: "cpu-pipeline-start", StreamID: stream.ID, Bindings: []core.PreparedBinding{{GroupID: configured.ID, GenerationID: result.Generation.ID, VariantID: prepared.VariantID, GeneratorVersion: config.GeneratorVersion}}}
			start, err := svc.StartSession(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			parent, err := svc.Claim(ctx, start.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if err = svc.MarkIssued(ctx, parent); err != nil {
				t.Fatal(err)
			}
			if err = svc.Complete(ctx, parent, core.Completion{Success: true}); err != nil {
				t.Fatal(err)
			}
			observer, err := svc.OpenSessionObserver(ctx, parent)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = observer.Close(context.Background()) })
			outputs, err := svc.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 10, SessionID: parent.SessionID}})
			if err != nil || len(outputs.Items) != 1 {
				t.Fatalf("снимок выхода: %+v, %v", outputs, err)
			}

			parentDirectory := os.TempDir()
			if runtime.GOOS == "darwin" {
				parentDirectory = "/tmp"
			}
			base, err := os.MkdirTemp(parentDirectory, "fallback-cpu-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(base); err != nil {
					t.Error(err)
				}
			})
			queue := media.QueueLimits{MaxPackets: 512, MaxBytes: 8 << 20, MaxPacketBytes: 1 << 20, MaxConfigBytes: 1 << 12, ResyncTimeout: 5 * time.Second}
			routerLimits := media.RouterLimits{MaxConsumers: 4, MaxRetainedBytes: 40 << 20, MaxConfigBytes: 1 << 12}
			live, err := media.NewRouter(routerLimits)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = live.Close() })
			runtimeConfig := group.RuntimeConfig{Switcher: group.SwitcherConfig{Profile: profile.Profile, Watchdog: watchdog, Queue: queue, Router: routerLimits, MaxAVSkew: 100 * time.Millisecond, MaxTransitionGap: 100 * time.Millisecond}, Binary: binary, OpenFile: files.OpenVerified, Container: integrationLimits(), ResubscribeBackoff: time.Millisecond, ControlInterval: 10 * time.Millisecond, ShutdownTimeout: 3 * time.Second, FFmpeg: ffmpeg.Config{BaseDir: base, StartupTimeout: 5 * time.Second, VideoTimeout: 2 * time.Second, AudioTimeout: 2 * time.Second, WatchdogInterval: 20 * time.Millisecond, RestartBackoff: 10 * time.Millisecond, GracePeriod: 100 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 3 * time.Second, DiagnosticEvents: 128, DiagnosticBytes: 1 << 20, DiagnosticChunkBytes: 1024, MaxCommandBytes: 8192, MaxCommandArgs: 128}}
			registry, err := group.NewRegistry(ctx, group.RegistryConfig{Core: svc, Parent: parent, Observer: observer, Runtime: runtimeConfig, MaxGroups: 4, MaxReferences: 4})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := registry.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			ref, err := registry.Acquire(ctx, outputs.Items[0].Snapshot, live)
			if err != nil {
				t.Fatal(err)
			}
			sub, err := ref.Router().Subscribe(queue)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sub.Close() }()
			var captured bytes.Buffer
			var mux *fmp4.Muxer
			counts := map[media.Track]int{}
			last := map[media.Track]*big.Rat{}
			firstPTS := map[media.Track]*big.Rat{}
			var epoch uint64
			for counts[media.TrackVideo] < 150 {
				delivery, err := sub.Next(ctx)
				if err != nil {
					t.Fatalf("FD-player/UDS/Group: %v status=%+v", err, ref.Group().Status())
				}
				p := delivery.Packet
				track := p.Config().Info().Track
				info := p.Info()
				tb := p.Config().Info().TimeBase
				if !delivery.IsCurrent() {
					continue
				}
				if mux == nil {
					mux, err = fmp4.NewMuxer(&captured, fmp4.Init{Video: delivery.VideoConfig, Audio: delivery.AudioConfig}, integrationLimits())
					if err != nil {
						t.Fatal(err)
					}
					epoch = delivery.VideoConfig.Info().SourceEpoch
				}
				if delivery.VideoConfig.Info().SourceEpoch != epoch {
					t.Fatal("непрерывная петля изменила выходную epoch")
				}
				stamp := new(big.Rat).SetFrac64(info.DTS*int64(tb.Numerator), int64(tb.Denominator))
				duration := new(big.Rat).SetFrac64(info.DurationTicks*int64(tb.Numerator), int64(tb.Denominator))
				expected := big.NewRat(1, 30)
				if track == media.TrackAudio {
					expected = big.NewRat(1024, 48000)
				}
				if duration.Cmp(expected) != 0 {
					t.Fatalf("%s[%d] duration=%s expected=%s packet=%+v config=%+v", track, counts[track], duration, expected, info, p.Config().Info())
				}
				if previous := last[track]; previous != nil {
					if step := new(big.Rat).Sub(stamp, previous); step.Cmp(expected) != 0 {
						t.Fatalf("%s шаг/граница петли %d: %s expected=%s", track, counts[track], step, expected)
					}
				}
				last[track] = stamp
				if firstPTS[track] == nil {
					firstPTS[track] = new(big.Rat).SetFrac64(info.PTS*int64(tb.Numerator), int64(tb.Denominator))
				}
				if info.PTS != info.DTS {
					t.Fatal("copy player изменил PTS−DTS источника без B-кадров")
				}
				counts[track]++
				if err = mux.WritePacket(p); err != nil {
					t.Fatal(err)
				}
			}
			if counts[media.TrackAudio] < 100 || ref.Group().Status().Source != group.Fallback {
				t.Fatalf("нет циклического A/V fallback до OBS: counts=%v status=%+v", counts, ref.Group().Status())
			}
			skew := new(big.Rat).Sub(firstPTS[media.TrackAudio], firstPTS[media.TrackVideo])
			if skew.Sign() < 0 || skew.Cmp(big.NewRat(1024, 48000)) > 0 {
				t.Fatalf("первые полные A/V AU не согласованы: %s", skew)
			}
			mediaCommand(t, binary, captured.Bytes(), "-hide_banner", "-loglevel", "error", "-xerror", "-flags2:a", "+skip_manual", "-i", "pipe:0", "-map", "0:v:0", "-map", "0:a:0", "-f", "null", "-")
			pcm := mediaCommand(t, binary, captured.Bytes(), "-hide_banner", "-loglevel", "error", "-xerror", "-flags2:a", "+skip_manual", "-i", "pipe:0", "-map", "0:a:0", "-f", "s16le", "pipe:1")
			if len(pcm) != counts[media.TrackAudio]*1024*2*2 {
				t.Fatalf("PCM тишины потерял полные AAC AU: bytes=%d AU=%d", len(pcm), counts[media.TrackAudio])
			}
			for i := 0; i < len(pcm); i += 2 {
				if sample := int16(encodingbinary.LittleEndian.Uint16(pcm[i:])); sample != 0 {
					t.Fatalf("подготовленная AAC-тишина содержит ненулевой PCM sample[%d]=%d", i/2, sample)
				}
			}
			if err = ref.Close(); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
