package group

import (
	"bytes"
	"context"
	"errors"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type runtimeFixture struct {
	config   RegistryConfig
	snapshot core.OutputSnapshot
	v, a     media.CodecConfig
}

func runtimeUnitFixture(t *testing.T) runtimeFixture {
	t.Helper()
	sw, v, a, _ := switcherFixture(t)
	ctx := context.Background()
	options := storage.Options{PreparationLimits: core.PreparationLimits{MaxQueued: 2, MaxProfiles: 8}, OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 128, MaxIdentifierBytes: 1024, MaxBytes: 4 << 20}, ObservationLimits: core.ObservationLimits{MaxSessions: 8, MaxGroups: 16, MaxOutputs: 16, MaxFFmpegWorkers: 16, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond}
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "group.sqlite"), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	asset, err := svc.CreateAsset(ctx, "Заглушка")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := svc.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "unit-key", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: sw.config.Watchdog}})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: sw.config.Profile})
	if err != nil {
		t.Fatal(err)
	}
	group, err := svc.CreateGroup(ctx, core.TranscodeGroup{Name: "Группа", StreamID: stream.ID, ProfileID: profile.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.CreateDestination(ctx, core.Destination{Name: "Назначение", StreamID: stream.ID, GroupID: group.ID, Platform: "custom", Endpoint: "rtmp://example.invalid/live", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	gen, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "unit-generator"})
	if err != nil {
		t.Fatal(err)
	}
	prep, err := svc.ClaimPreparation(ctx, gen.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkPreparationIssued(ctx, prep); err != nil {
		t.Fatal(err)
	}
	required := gen.RequiredProfiles[0]
	publication := core.GenerationPublication{SourceHash: "unit-source", SizeBytes: 1, SourceFormat: core.SourceFormat{ImageFormat: "png"}, Variants: []core.FallbackVariant{{ID: required.VariantID, GenerationID: gen.ID, AssetID: asset.ID, SourceHash: "unit-source", Profile: required.Profile, VerifiedFormat: required.Profile.Format, GeneratorVersion: gen.GeneratorVersion, ManagedPath: required.ManagedPath, SizeBytes: 1, ContentHash: "unit-result"}}}
	if _, err = svc.PublishPreparation(ctx, prep, publication); err != nil {
		t.Fatal(err)
	}
	if err = svc.AcknowledgePreparationCleanup(ctx, prep); err != nil {
		t.Fatal(err)
	}
	receipt, err := svc.StartSession(ctx, core.StartSessionRequest{RequestID: "unit-start", StreamID: stream.ID, Bindings: []core.PreparedBinding{{GroupID: group.ID, GenerationID: gen.ID, VariantID: required.VariantID, GeneratorVersion: gen.GeneratorVersion}}})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := svc.Claim(ctx, receipt.RequestID)
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
	outputs, err := svc.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: parent.SessionID}})
	if err != nil || len(outputs.Items) != 1 {
		t.Fatal(outputs, err)
	}
	c := RegistryConfig{Core: svc, Parent: parent, Observer: observer, MaxGroups: 2, MaxReferences: 2, Runtime: RuntimeConfig{Switcher: sw.config, Container: fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 65536, MaxMdatBytes: 1 << 20, MaxSampleBytes: 65536, MaxExtradataBytes: 4096, MaxSamples: 100}, Binary: "unavailable-unit-ffmpeg", OpenFile: func(context.Context, core.ArtifactManifest) (*os.File, error) { return nil, os.ErrNotExist }, ResubscribeBackoff: time.Millisecond, ControlInterval: time.Millisecond, ShutdownTimeout: time.Second, FFmpeg: ffmpeg.Config{BaseDir: filepath.Join(t.TempDir(), "sockets"), StartupTimeout: time.Second, VideoTimeout: time.Second, AudioTimeout: time.Second, WatchdogInterval: time.Millisecond, RestartBackoff: time.Millisecond, GracePeriod: 10 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: time.Second, DiagnosticEvents: 32, DiagnosticBytes: 65536, DiagnosticChunkBytes: 1024, MaxCommandBytes: 8192, MaxCommandArgs: 64}}}
	return runtimeFixture{c, outputs.Items[0].Snapshot, v, a}
}
func TestRegistryRejectsUnheldSnapshotAndBoundsReferences(t *testing.T) {
	f := runtimeUnitFixture(t)
	ctx := context.Background()
	if _, err := NewRegistry(ctx, RegistryConfig{}); !errors.Is(err, core.ErrInvalid) {
		t.Fatal(err)
	}
	registry, err := NewRegistry(ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close(context.Background()) })
	live, _ := media.NewRouter(f.config.Runtime.Switcher.Router)
	defer func() { _ = live.Close() }()
	if _, err = registry.Acquire(ctx, f.snapshot, nil); !errors.Is(err, core.ErrInvalid) {
		t.Fatal(err)
	}
	forged := f.snapshot
	forged.Artifact.GenerationID = "unheld-generation"
	if _, err = registry.Acquire(ctx, forged, live); !errors.Is(err, core.ErrStale) {
		t.Fatal("принят чужой manifest", err)
	}
	first, err := registry.Acquire(ctx, f.snapshot, live)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := media.NewRouter(f.config.Runtime.Switcher.Router)
	defer func() { _ = other.Close() }()
	if _, err = registry.Acquire(ctx, f.snapshot, other); !errors.Is(err, core.ErrConflict) {
		t.Fatal(err)
	}
	second, err := registry.Acquire(ctx, f.snapshot, live)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registry.Acquire(ctx, f.snapshot, live); !errors.Is(err, media.ErrRouterBudget) {
		t.Fatal("превышен общий бюджет ссылок", err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	if err = second.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := registry.Acquire(ctx, f.snapshot, live)
	if err != nil {
		t.Fatal(err)
	}
	if third.Group() == first.Group() {
		t.Fatal("отозванная группа ожила")
	}
	if err = third.Close(); err != nil {
		t.Fatal(err)
	}
	if err = registry.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = registry.Acquire(ctx, f.snapshot, live); !errors.Is(err, core.ErrStale) {
		t.Fatal(err)
	}
}
func TestGroupReadOutputResolvesActualAV1AndReportsOnlyValidProgress(t *testing.T) {
	f := runtimeUnitFixture(t)
	ctx := context.Background()
	owner, err := f.config.Core.BindExecutionOwner(ctx, f.config.Parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "unknown-metadata", "invalid-au", "rejected-progress"} {
		t.Run(mode, func(t *testing.T) {
			sw, _, _, _ := switcherFixture(t)
			g := &Group{config: f.config.Runtime, owner: owner, switcher: sw}
			var wire bytes.Buffer
			m, err := fmp4.NewMuxer(&wire, fmp4.Init{Video: f.v, Audio: f.a}, g.config.Container)
			if err != nil {
				t.Fatal(err)
			}
			payload := av1TU(true)
			if mode == "invalid-au" {
				payload = []byte{0x1a, 0}
			}
			video, _ := media.NewPacket(media.PacketInfo{PTS: 0, DTS: 0, DurationTicks: 1600, Keyframe: true}, f.v, payload)
			audio, _ := media.NewPacket(media.PacketInfo{PTS: 480, DTS: 480, DurationTicks: 1024}, f.a, []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c})
			if err = m.WritePacket(video); err != nil {
				t.Fatal(err)
			}
			if err = m.WritePacket(audio); err != nil {
				t.Fatal(err)
			}
			data := bytes.Clone(wire.Bytes())
			position := bytes.Index(data, []byte("uuid"))
			if position < 0 {
				t.Fatal("нет metadata")
			}
			copy(data[position:], "free")
			reader, writer := net.Pipe()
			done := make(chan error, 1)
			go func() { _, err := writer.Write(data); err = errors.Join(err, writer.Close()); done <- err }()
			format := f.snapshot.Artifact.VerifiedFormat
			if mode == "unknown-metadata" {
				format = core.MediaFormat{}
			}
			count := 0
			err = g.readOutput(ctx, reader, Fallback, 1, format, func(ffmpeg.Track, int64) bool { count++; return mode != "rejected-progress" })
			_ = reader.Close()
			if writeErr := <-done; writeErr != nil && !errors.Is(writeErr, io.ErrClosedPipe) {
				t.Fatal(writeErr)
			}
			switch mode {
			case "valid":
				if !errors.Is(err, io.EOF) || count != 2 || sw.Status().Available {
					t.Fatalf("valid: %v %d %+v", err, count, sw.Status())
				}
			case "unknown-metadata", "invalid-au":
				if err == nil || count != 0 || sw.Status().Available {
					t.Fatalf("невалидный поток сообщил прогресс: %v %d", err, count)
				}
			case "rejected-progress":
				if !errors.Is(err, ErrFeed) || count != 1 || sw.Status().Available {
					t.Fatal(err, count)
				}
			}
		})
	}
}
func TestGroupStopKeepsPhysicalJoinAndCleanupErrorsVisible(t *testing.T) {
	g := &Group{config: RuntimeConfig{ShutdownTimeout: time.Millisecond}, cancel: func() {}, done: make(chan struct{})}
	if err := g.Stop(context.Background()); !errors.Is(err, ffmpeg.ErrCleanupPending) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	failure := errors.New("не удалён собственный сокет")
	g.addCleanup(nil)
	g.addCleanup(failure)
	close(g.done)
	if err := g.Stop(context.Background()); !errors.Is(err, failure) {
		t.Fatal("потеряно доказательство неполной очистки", err)
	}
}
func TestTransportOriginPreservesCommonSignedClockAndRejectsOverflow(t *testing.T) {
	_, v, a, _ := switcherFixture(t)
	for _, dt := range []int64{100, -100, math.MinInt64} {
		p, _ := media.NewPacket(media.PacketInfo{PTS: dt, DTS: dt, Keyframe: true}, v, av1TU(true))
		origin, err := transportOrigin(media.Delivery{Packet: p, VideoConfig: v, AudioConfig: a})
		if dt == math.MinInt64 {
			if err == nil {
				t.Fatal("переполнение origin принято")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if dt >= 0 && origin.OffsetTicks != 0 || dt < 0 && origin.OffsetTicks != 100 {
			t.Fatal(origin)
		}
	}
}

func TestGroupAttemptIdentityCannotWrapOrAffectOtherFeed(t *testing.T) {
	g := &Group{}
	g.attempts[0].Store(math.MaxUint64)
	if id, err := g.nextAttempt(Live); id != 0 || !errors.Is(err, ErrFeed) {
		t.Fatal(id, err)
	}
	if id, err := g.nextAttempt(Fallback); id != 1 || err != nil {
		t.Fatal(id, err)
	}
	if g.attempts[0].Load() != math.MaxUint64 {
		t.Fatal("исчерпанная identity переиспользована")
	}
}

func TestLiveAudioCopyRejectsASCBeforeSelectedBuilderAndReportsReason(t *testing.T) {
	f := runtimeUnitFixture(t)
	for _, scenario := range []string{"unsupported-asc", "metadata-mismatch", "profile-mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			live, err := media.NewRouter(f.config.Runtime.Switcher.Router)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = live.Close() }()
			source, err := live.BeginSource()
			if err != nil {
				t.Fatal(err)
			}
			info := f.a.Info()
			info.SourceEpoch = source.Epoch()
			headers := f.a.Headers()
			switch scenario {
			case "unsupported-asc":
				headers = []byte{0}
			case "metadata-mismatch":
				info.Audio.SampleRateHz = 44100
			}
			audio, err := media.NewCodecConfig(info, headers)
			if err != nil {
				t.Fatal(err)
			}
			videoInfo := f.v.Info()
			videoInfo.SourceEpoch = source.Epoch()
			video, err := media.NewCodecConfig(videoInfo, f.v.Headers())
			if err != nil {
				t.Fatal(err)
			}
			if err = source.UpdateConfigs(video, audio); err != nil {
				t.Fatal(err)
			}
			sub, err := live.Subscribe(f.config.Runtime.Switcher.Queue)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sub.Close() }()
			for _, config := range []media.CodecConfig{video, audio} {
				packet, err := media.NewPacket(media.PacketInfo{Keyframe: config.Info().Track == media.TrackVideo, DurationTicks: 1024}, config, []byte{1})
				if err != nil {
					t.Fatal(err)
				}
				if err = source.Publish(packet); err != nil {
					t.Fatal(err)
				}
			}
			observer, err := f.config.Observer.OpenGroup(context.Background(), f.snapshot.Group.ID, f.snapshot.Profile.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = observer.Close(context.Background()) }()
			var output bytes.Buffer
			g := &Group{config: f.config.Runtime, parent: f.config.Parent, id: f.snapshot.Group.ID, observer: observer}
			g.config.Switcher.Profile.LiveAudioMode = core.LiveAudioCopy
			if scenario == "profile-mismatch" {
				g.config.Switcher.Profile.Format.Audio.SampleRateHz = 44100
			}
			g.config.Logger = slog.New(slog.NewJSONHandler(&output, nil))
			called := false
			g.config.Transcode = func(context.Context, ffmpeg.NVIDIAConfig, ffmpeg.Attempt) (ffmpeg.Command, error) {
				called = true
				return ffmpeg.Command{}, errors.New("построитель не должен вызываться")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err = g.transcode(ctx, sub, 1)
			if !errors.Is(err, codec.ErrCodec) || called || !strings.Contains(output.String(), "LIVE AAC copy") || !strings.Contains(output.String(), "validate_audio") {
				t.Fatal("несовместимое аудио дошло до построителя или потеряна причина", err, called, output.String())
			}
		})
	}
}
