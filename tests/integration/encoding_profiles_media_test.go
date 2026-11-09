package integration

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

func encodingPipelineProfile(codec core.Codec, rate core.FrameRate) core.GroupProfile {
	p := fallbackPipelineProfile(codec)
	p.Format.Video.FrameRate = rate
	encoder := "h264_nvenc"
	if codec == core.CodecAV1 {
		encoder = "libsvtav1"
		p.VideoEncoding.MaxBitrateBPS = 0
		p.VideoEncoding.BufferBits = 0
	} else {
		p.VideoEncoding.MaxBFrames = 2
	}
	p.Encoding = core.DefaultEncoding(p.Mode, encoder, "vbr")
	if codec == core.CodecAV1 {
		p.Encoding.VideoCustom = "lp=1"
	}
	return p
}

// Только H.264 заменяется CPU-кодировщиком; AV1 использует рабочие параметры SVT-AV1 VBR.
func encodingPipelineCPUBuilder(binary string) fallback.CommandBuilder {
	return func(req fallback.GenerateRequest) (fallback.Command, error) {
		c, err := fallback.ProductionBuilder(binary)(req)
		if err != nil || req.Profile.Format.Video.Codec == core.CodecAV1 {
			return c, err
		}
		var args []string
		for i := 0; i < len(c.Args); i++ {
			switch c.Args[i] {
			case "-rc", "-preset", "-tune", "-multipass", "-rc-lookahead", "-spatial-aq", "-temporal-aq", "-aq-strength", "-no-scenecut", "-forced-idr":
				i++
				continue
			}
			arg := c.Args[i]
			if arg == "h264_nvenc" {
				arg = "libx264"
			}
			args = append(args, arg)
		}
		output := args[len(args)-1]
		args = args[:len(args)-1]
		c.Args = append(args, "-sc_threshold", "0", "-threads:v", "1", output)
		return c, nil
	}
}

func encodingPreparedPackets(data []byte, format core.MediaFormat) (map[media.Track][]media.Packet, error) {
	d, err := fmp4.NewDemuxer(bytes.NewReader(data), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		info := media.CodecConfigInfo{Track: desc.Track, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase}
		if desc.Track == media.TrackVideo {
			info.Video = format.Video
		} else {
			info.Audio = format.Audio
		}
		return media.NewCodecConfig(info, desc.Headers)
	}})
	if err != nil {
		return nil, err
	}
	result := map[media.Track][]media.Packet{}
	for {
		p, e := d.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return nil, e
		}
		track := p.Config().Info().Track
		result[track] = append(result[track], p)
	}
	return result, nil
}

func TestEncodingProfilesPreparedManagerFDPlayerGroupLoops(t *testing.T) {
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("локальная CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	for _, scenario := range []struct {
		name   string
		codec  core.Codec
		rate   core.FrameRate
		reject bool
	}{
		{"av1-vbr", core.CodecAV1, core.FrameRate{Numerator: 30, Denominator: 1}, false},
		{"h264-b", core.CodecH264, core.FrameRate{Numerator: 30, Denominator: 1}, false},
		{"av1-vbr-rational", core.CodecAV1, core.FrameRate{Numerator: 25, Denominator: 2}, false},
		{"h264-b-rational", core.CodecH264, core.FrameRate{Numerator: 75, Denominator: 2}, false},
		{"h264-b-reorder-exceeds-budget", core.CodecH264, core.FrameRate{Numerator: 25, Denominator: 2}, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
			profile, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "CPU-профиль", Profile: encodingPipelineProfile(scenario.codec, scenario.rate)})
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
			probeLimits := fallback.ProbeLimits{MaxJSONBytes: 1 << 20, MaxWidth: 1024, MaxHeight: 1024, MaxPixels: 1024 * 1024, MaxDuration: 10 * time.Second, ImageDuration: 1600 * time.Millisecond, MaxPackets: 4096, Container: integrationLimits()}
			actualRunner := fallbackPipelineRunner{t: t, runner: runner}
			probe := fallback.Prober{Binary: filepath.Join(filepath.Dir(binary), "ffprobe"), Runner: actualRunner, Limits: probeLimits}
			config := fallback.Config{ReconcileLimit: 10, GeneratorVersion: "cpu-integration", FFmpeg: binary, FFprobe: probe.Binary, MaxUploadBytes: 1 << 20, MaxOutputBytes: 8 << 20, PrepareTimeout: 15 * time.Second, ShutdownTimeout: 3 * time.Second, QueueInterval: time.Millisecond, Runner: runner.Limits, Probe: probeLimits}
			builder := encodingPipelineCPUBuilder(binary)
			manager, err := fallback.NewManagerWithTools(svc, files, config, fallback.Tools{Builder: builder, Inspector: probe, Runner: actualRunner, Normalize: probe.NormalizePrepared, VerifyLoopDuration: probe.VerifyLoopDuration})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Close(context.Background()) })
			img := image.NewRGBA(image.Rect(0, 0, 128, 128))
			for y := range 128 {
				for x := range 128 {
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
			expected, err := encodingPreparedPackets(preparedBytes, variant.VerifiedFormat)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.reject {
				first := expected[media.TrackVideo][0]
				if first.Info().PTS != 0 || fallbackActualTime(first.Info().DTS, first.Config().Info().TimeBase).Cmp(big.NewRat(-4, 25)) != 0 {
					t.Fatal("фикстура не подтвердила reorder lead 160ms", first.Info())
				}
			}
			loop, err := fallback.EffectiveLoopDuration(probeLimits.ImageDuration, profile.Profile, probeLimits.MaxDuration)
			if err != nil {
				t.Fatal(err)
			}
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

			counts := map[media.Track]int{}
			var shift *big.Rat
			var epoch uint64
			for counts[media.TrackVideo] < 3*len(expected[media.TrackVideo]) || counts[media.TrackAudio] < 3*len(expected[media.TrackAudio]) {
				delivery, err := sub.Next(ctx)
				if err != nil {
					if scenario.reject && !ref.Group().Status().Available && errors.Is(ref.Group().Status().Err, group.ErrTiming) {
						t.Log("H.264 reorder lead 2×2/25 с превышает неизменный MaxTransitionGap=100ms; Group отказал")
						if closeErr := ref.Close(); closeErr != nil && !errors.Is(closeErr, context.Canceled) {
							t.Fatal(closeErr)
						}
						return
					}
					t.Fatalf("FD-player/UDS/Group: %v status=%+v", err, ref.Group().Status())
				}
				if !delivery.IsCurrent() {
					continue
				}
				packet := delivery.Packet
				track := packet.Config().Info().Track
				if epoch == 0 {
					epoch = packet.Config().Info().SourceEpoch
				}
				if packet.Config().Info().SourceEpoch != epoch {
					t.Fatal("петля сменила epoch")
				}
				pattern := expected[track]
				index := counts[track]
				original := pattern[index%len(pattern)]
				pi, oi := packet.Info(), original.Info()
				if pi.DurationTicks <= 0 || !bytes.Equal(packet.Payload(), original.Payload()) || pi.Keyframe != oi.Keyframe {
					t.Fatalf("изменена единица доступа %s[%d]", track, index)
				}
				base, originalBase := packet.Config().Info().TimeBase, original.Config().Info().TimeBase
				offset := new(big.Rat).Mul(new(big.Rat).SetInt64(int64(index/len(pattern))), big.NewRat(int64(loop), int64(time.Second)))
				currentShift := new(big.Rat).Sub(fallbackActualTime(pi.DTS, base), fallbackActualTime(oi.DTS, originalBase))
				currentShift.Sub(currentShift, offset)
				if shift == nil {
					shift = currentShift
				}
				if shift.Cmp(currentShift) != 0 {
					t.Fatalf("непостоянный общий A/V shift %s[%d]: %s != %s", track, index, currentShift, shift)
				}
				wantPTS := new(big.Rat).Add(fallbackActualTime(oi.PTS, originalBase), offset)
				wantPTS.Add(wantPTS, shift)
				if fallbackActualTime(pi.PTS, base).Cmp(wantPTS) != 0 || fallbackActualTime(pi.DurationTicks, base).Cmp(fallbackActualTime(oi.DurationTicks, originalBase)) != 0 {
					t.Fatalf("изменены PTS/CTS/duration %s[%d]: %+v != %+v", track, index, pi, oi)
				}
				counts[track]++
			}
			if scenario.reject {
				t.Fatal("Group принял reorder lead больше MaxTransitionGap")
			}
			if ref.Group().Status().Source != group.Fallback {
				t.Fatal(ref.Group().Status())
			}
			video := expected[media.TrackVideo]
			if scenario.codec == core.CodecH264 && (video[0].Info().DTS >= 0 || video[0].Info().PTS != 0) {
				t.Fatal("не проверены отрицательное начало и перестановка H.264")
			}
			t.Logf("точные границы петли: video=%d audio=%d period=%s common_shift=%s", counts[media.TrackVideo], counts[media.TrackAudio], loop, shift)
			if err = ref.Close(); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
