package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

type lifecycleOwner struct{ closed atomic.Bool }

func (o *lifecycleOwner) CloseAdmission()                 { o.closed.Store(true) }
func (o *lifecycleOwner) Reconcile(context.Context) error { return nil }
func (o *lifecycleOwner) Close(context.Context) error     { o.closed.Store(true); return nil }
func (o *lifecycleOwner) Shutdown(context.Context) error  { o.closed.Store(true); return nil }

type managerFixture struct {
	databasePath         string
	manager              *Manager
	service              *core.Service
	files                *fallback.FileStore
	config               Config
	stream               core.Stream
	destination          core.Destination
	binding              core.PreparedBinding
	ingress, preparation *lifecycleOwner
	store                *storage.Store
}

func fixtureProfile(t testing.TB) (core.GroupProfile, media.CodecConfig, media.CodecConfig) {
	t.Helper()
	headers := []byte{0x81, 8, 0x0c, 0, 0x0a, 11, 0, 0, 0, 66, 167, 191, 228, 96, 13, 0, 64}
	payload := append(append([]byte(nil), headers[4:]...), 0x1a, 1, 0x10)
	video, _, err := codec.InspectAV1TU(headers, payload, core.FrameRate{Numerator: 30, Denominator: 1})
	if err != nil {
		t.Fatal(err)
	}
	audio, err := codec.AudioFormatASC([]byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	base := media.TimeBase{Numerator: 1, Denominator: 48000}
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: base, Video: video}, headers)
	if err != nil {
		t.Fatal(err)
	}
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: base, Audio: audio}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	profile := core.GroupProfile{Mode: core.ModeCopy, Format: core.MediaFormat{Video: video, Audio: audio}, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 1000, MaxBitrateBPS: 1000, BufferBits: 1000, GOPFrames: 30, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 128000}}
	return profile, v, a
}
func newManagerFixture(t *testing.T, enabled bool, modify func(*Config)) managerFixture {
	t.Helper()
	ctx := context.Background()
	directory := t.TempDir()
	options := storage.Options{PreparationLimits: core.PreparationLimits{MaxQueued: 2, MaxProfiles: 8}, OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 32, MaxIdentifierBytes: 1024, MaxBytes: 4 << 20}, ObservationLimits: core.ObservationLimits{MaxSessions: 8, MaxGroups: 32, MaxOutputs: 32, MaxFFmpegWorkers: 32, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond}
	store, err := storage.Open(ctx, filepath.Join(directory, "session.sqlite"), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	service, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	files, err := fallback.OpenFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})
	profile, _, _ := fixtureProfile(t)
	asset, err := service.CreateAsset(ctx, "Заглушка")
	if err != nil {
		t.Fatal(err)
	}
	policy := core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: 20 * time.Millisecond, KeyframeTimeout: time.Second}}
	stream, err := service.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "session-key", Fallback: policy})
	if err != nil {
		t.Fatal(err)
	}
	p, err := service.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	profile = p.Profile
	g, err := service.CreateGroup(ctx, core.TranscodeGroup{Name: "Группа", StreamID: stream.ID, ProfileID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	destination, err := service.CreateDestination(ctx, core.Destination{Name: "Выход", StreamID: stream.ID, GroupID: g.ID, Platform: "custom", Endpoint: "rtmp://127.0.0.1:1/live", StreamKey: "test-key", Enabled: enabled})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := service.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "test-generator"})
	if err != nil {
		t.Fatal(err)
	}
	preparationFence, err := service.ClaimPreparation(ctx, generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.MarkPreparationIssued(ctx, preparationFence); err != nil {
		t.Fatal(err)
	}
	if err = files.CreateGeneration(generation.ID); err != nil {
		t.Fatal(err)
	}
	required := generation.RequiredProfiles[0]
	file, err := files.Create(required.ManagedPath)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("локальный проверяемый файл")
	if _, err = file.Write(content); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	hash := hex.EncodeToString(digest[:])
	_, err = service.PublishPreparation(ctx, preparationFence, core.GenerationPublication{SourceHash: "source", SizeBytes: 1, SourceFormat: core.SourceFormat{ImageFormat: "png"}, Variants: []core.FallbackVariant{{ID: required.VariantID, GenerationID: generation.ID, AssetID: asset.ID, SourceHash: "source", Profile: profile, VerifiedFormat: profile.Format, GeneratorVersion: generation.GeneratorVersion, ManagedPath: required.ManagedPath, SizeBytes: uint64(len(content)), ContentHash: hash}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.AcknowledgePreparationCleanup(ctx, preparationFence); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "gost-process-helper")
	args := []string{"build", "-o", helper}
	if directory := os.Getenv("GOST_TEST_PROCESS_HELPER_COVERDIR"); directory != "" {
		t.Setenv("GOCOVERDIR", directory)
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=github.com/GregoryGost/gost-restreamer/...")
	}
	command := exec.Command("go", append(args, "../../cmd/gost-process-helper")...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v %s", err, output)
	}
	runtimeParent := os.TempDir()
	if runtime.GOOS == "darwin" {
		// Системный TMPDIR macOS слишком длинный для вложенных путей UDS.
		runtimeParent = "/private/tmp"
	}
	runtimeDirectory, err := os.MkdirTemp(runtimeParent, "sm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(runtimeDirectory); err != nil {
			t.Error(err)
		}
	})
	processes, err := processowner.New(processowner.Config{Root: filepath.Join(directory, "processes"), RuntimeRoot: runtimeDirectory, HelperPath: helper, Limits: processowner.Limits{MaxOperations: 512, MaxRecordBytes: 1 << 20, MaxCommandArgs: 128, MaxCommandBytes: 16384, MaxExtraFiles: 8, PageSize: 32}, GracePeriod: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := processes.Close(); err != nil {
			t.Error(err)
		}
	})
	ingress, preparation := &lifecycleOwner{}, &lifecycleOwner{}
	queue := media.QueueLimits{MaxPackets: 64, MaxBytes: 65536, MaxPacketBytes: 4096, MaxConfigBytes: 512, ResyncTimeout: time.Second}
	router := media.RouterLimits{MaxConsumers: 16, MaxRetainedBytes: 2 << 20, MaxConfigBytes: 512}
	c := Config{Core: service, Files: files, Processes: processes, GeneratorVersion: generation.GeneratorVersion, Ingress: ingress, Preparation: preparation, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxSessions: 2, MaxOutputs: 8, MaxCommands: 8, PageSize: 8, MaxGroups: 8, MaxReferences: 8, Router: router, ReconcileInterval: 5 * time.Millisecond, StartupTimeout: 3 * time.Second, OperationTimeout: time.Second, ShutdownTimeout: 3 * time.Second, Publisher: publisher.Options{Queue: queue, RetryInitial: time.Millisecond, RetryMax: 20 * time.Millisecond, ResubscribeDelay: time.Millisecond, Transport: rtmp.OutboundOptions{Limits: rtmp.OutboundLimits{MaxChunkStreams: 8, MaxMessageBytes: 65536, MaxChunkBytes: 4096, MaxFragments: 1024, MaxControlBytes: 4096, MaxConfigBytes: 4096, MaxAMFBytes: 8192, MaxAMFDepth: 8, MaxAMFElements: 128, ChunkSize: 4096, MaxControlPending: 32, MaxRetainedBytes: 4 << 20, DialTimeout: time.Second, TLSTimeout: time.Second, StartupTimeout: time.Second, MessageTimeout: time.Second, WriteTimeout: time.Second}}}, Group: group.RuntimeConfig{Switcher: group.SwitcherConfig{Profile: profile, Watchdog: policy.Watchdog, Queue: queue, Router: router, MaxAVSkew: time.Second, MaxTransitionGap: time.Second}, Container: fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 65536, MaxMdatBytes: 1 << 20, MaxSampleBytes: 65536, MaxExtradataBytes: 4096, MaxSamples: 100}, Binary: "controlled", ResubscribeBackoff: time.Millisecond, ControlInterval: 5 * time.Millisecond, ShutdownTimeout: 2 * time.Second, FFmpeg: ffmpeg.Config{BaseDir: runtimeDirectory, StartupTimeout: time.Second, VideoTimeout: time.Second, AudioTimeout: time.Second, WatchdogInterval: 10 * time.Millisecond, RestartBackoff: time.Millisecond, GracePeriod: 20 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 2 * time.Second, DiagnosticEvents: 32, DiagnosticBytes: 65536, DiagnosticChunkBytes: 1024, MaxCommandBytes: 8192, MaxCommandArgs: 64}}}
	c.Monitoring = monitoring.SamplerConfig{MaxSources: 128, StaleAfter: 5 * time.Second, ShutdownTimeout: 3 * time.Second}
	c.Telemetry = core.TelemetryLimits{Events: core.EventLimits{MaxEvents: 128, MaxBytes: 1 << 20, MaxEventBytes: 65536, MaxPageSize: 64}, MediaStaleAfter: 5 * time.Second, MaxGPUDevices: 8, MaxGPUUUIDBytes: 256, MaxGPUBytes: 1 << 20}
	c.Group.FFmpeg.Progress = ffmpeg.ProgressConfig{MaxLineBytes: 4096, MaxReportBytes: 65536, MaxFields: 64}
	c.Group.Player = func(_ context.Context, _ string, _ *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		binary, err := os.Executable()
		return ffmpeg.Command{Binary: binary, Args: []string{"-test.run=^TestSessionMediaProcess$", "--", a.OutputPath}}, err
	}
	if modify != nil {
		modify(&c)
	}
	manager, err := New(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return managerFixture{filepath.Join(directory, "session.sqlite"), manager, service, files, c, stream, destination, core.PreparedBinding{GroupID: g.ID, GenerationID: generation.ID, VariantID: required.VariantID, GeneratorVersion: generation.GeneratorVersion}, ingress, preparation, store}
}

func TestSessionMediaProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	output, err := net.Dial("unix", os.Args[len(os.Args)-1])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = output.Close() }()
	_, v, a := fixtureProfile(t)
	mux, err := fmp4.NewMuxer(output, fmp4.Init{Video: v, Audio: a}, fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 65536, MaxMdatBytes: 1 << 20, MaxSampleBytes: 65536, MaxExtradataBytes: 4096, MaxSamples: 100})
	if err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for tick := int64(0); ; tick++ {
		video, _ := media.NewPacket(media.PacketInfo{PTS: tick * 1600, DTS: tick * 1600, DurationTicks: 1600, Keyframe: true}, v, append(v.Headers()[4:], 0x1a, 1, 0x10))
		audio, _ := media.NewPacket(media.PacketInfo{PTS: tick * 1600, DTS: tick * 1600, DurationTicks: 1024}, a, []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c})
		if mux.WritePacket(video) != nil || mux.WritePacket(audio) != nil {
			return
		}
		<-ticker.C
	}
}
func waitReceipt(t *testing.T, f managerFixture, receipt core.CommandReceipt) core.CommandReceipt {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := f.service.GetReceipt(ctx, receipt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status.Terminal() {
			return current
		}
		select {
		case <-ctx.Done():
			t.Fatalf("квитанция не завершилась: %+v", current)
		case <-ticker.C:
		}
	}
}
func TestManagerZeroOutputsOwnsLifecycleAndExactFeed(t *testing.T) {
	f := newManagerFixture(t, false, nil)
	ctx := context.Background()
	receipt, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "start-zero", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, receipt); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	session, err := f.service.GetSession(ctx, receipt.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	router, err := f.manager.Feed(session)
	if err != nil || router == nil {
		t.Fatal(err)
	}
	session.Lifecycle++
	if _, err = f.manager.Feed(session); !errors.Is(err, core.ErrStale) {
		t.Fatal("принят чужой lifecycle", err)
	}
	pause, err := f.manager.PauseSession(ctx, core.SessionCommand{RequestID: "pause-zero", SessionID: receipt.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, pause); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	if err = f.manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.ingress.closed.Load() || !f.preparation.closed.Load() {
		t.Fatal("владельцы не закрыты")
	}
	if _, err = f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "closed", StreamID: f.stream.ID}); !errors.Is(err, core.ErrClosed) {
		t.Fatal(err)
	}
}
func TestManagerFallbackBarrierAndPriorReceipt(t *testing.T) {
	f := newManagerFixture(t, true, nil)
	ctx := context.Background()
	receipt, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "start-media", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	current := waitReceipt(t, f, receipt)
	if current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	f.manager.mu.Lock()
	node := f.manager.sessions[receipt.SessionID]
	f.manager.mu.Unlock()
	if node == nil {
		t.Fatal("нет исполнения")
	}
	node.mu.Lock()
	for _, member := range node.members {
		member.mu.Lock()
		ref := member.reference
		member.mu.Unlock()
		if ref == nil || ref.Group().Ready() != nil {
			t.Fatal("RUNNING до текущего fallback")
		}
	}
	node.mu.Unlock()
	// Повтор не открывает файл заново и не выбирает новую конфигурацию.
	variant, err := f.service.GetVariant(ctx, f.binding.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.files.RemoveExact(ctx, variant.ManagedPath, variant.SizeBytes, variant.ContentHash); err != nil {
		t.Fatal(err)
	}
	duplicate, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "start-media", StreamID: f.stream.ID})
	if err != nil || duplicate.ID != receipt.ID {
		t.Fatal(duplicate, err)
	}
}

func TestManagerStopDuringStartingExcludesOutputAndWaitsLateBuilder(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce atomic.Bool
	f := newManagerFixture(t, true, func(c *Config) {
		original := c.Group.Player
		c.Group.Player = func(ctx context.Context, binary string, file *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
			if enteredOnce.CompareAndSwap(false, true) {
				close(entered)
			}
			<-release
			return original(ctx, binary, file, a)
		}
	})
	var released atomic.Bool
	defer func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}()
	ctx := context.Background()
	receipt, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "start-held", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("builder не начал работу")
	}
	if _, err = f.service.AuthorizeOBS(ctx, f.stream.ID, f.stream.PublicationKey); !errors.Is(err, core.ErrNotReady) {
		t.Fatal("OBS допущен до fallback", err)
	}
	outputs, err := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: receipt.SessionID}})
	if err != nil || len(outputs.Items) != 1 {
		t.Fatal(outputs, err)
	}
	stop, err := f.manager.StopOutput(ctx, core.OutputCommand{RequestID: "stop-held", OutputID: outputs.Items[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, receipt); current.Status != core.CommandSucceeded {
		t.Fatal("ноль оставшихся выходов должен допускать RUNNING", current)
	}
	output, err := f.service.GetOutput(ctx, outputs.Items[0].ID)
	if err != nil || !output.CleanupPending {
		t.Fatal("поздний builder уже объявлен очищенным", output, err)
	}
	released.Store(true)
	close(release)
	if current := waitReceipt(t, f, stop); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	output, err = f.service.GetOutput(ctx, outputs.Items[0].ID)
	if err != nil || output.CleanupPending {
		t.Fatal(output, err)
	}
}

func TestManagerCommonStartupFailureEndsSession(t *testing.T) {
	f := newManagerFixture(t, true, func(c *Config) {
		c.Group.Player = func(context.Context, string, *os.File, ffmpeg.Attempt) (ffmpeg.Command, error) {
			return ffmpeg.Command{}, os.ErrPermission
		}
	})
	receipt, err := f.manager.StartSession(context.Background(), core.StartSessionRequest{RequestID: "start-failed", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, receipt); current.Status != core.CommandFailed {
		t.Fatal(current)
	}
	session, err := f.service.GetSession(context.Background(), receipt.SessionID)
	if err != nil || session.State != core.SessionFailed {
		t.Fatal(session, err)
	}
	if _, err = f.manager.Feed(session); err == nil {
		t.Fatal("неуспешный startup сохранил Feed")
	}
}

func TestManagerSecondDispatcherRejectedAndRequestContextDoesNotOwnRuntime(t *testing.T) {
	f := newManagerFixture(t, false, nil)
	if duplicate, err := New(context.Background(), f.config); !errors.Is(err, core.ErrInUse) || duplicate != nil {
		t.Fatal(duplicate, err)
	}
	request, cancel := context.WithCancel(context.Background())
	receipt, err := f.manager.StartSession(request, core.StartSessionRequest{RequestID: "start-context", StreamID: f.stream.ID})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, receipt); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	session, err := f.service.GetSession(context.Background(), receipt.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.Feed(session); err != nil {
		t.Fatal("контекст запроса отозвал runtime", err)
	}
}

func TestManagerOldUnknownCleanupDoesNotConsumeSessionQuota(t *testing.T) {
	var oldID core.ID
	f := newManagerFixture(t, false, func(c *Config) {
		c.MaxSessions = 1
		receipt, err := c.Core.StartSession(context.Background(), core.StartSessionRequest{RequestID: "old-start", StreamID: func() core.ID {
			streams, e := c.Core.ListStreams(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 1}})
			if e != nil {
				t.Fatal(e)
			}
			return streams.Items[0].ID
		}()})
		if err != nil {
			t.Fatal(err)
		}
		fence, err := c.Core.Claim(context.Background(), receipt.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		if err = c.Core.MarkIssued(context.Background(), fence); err != nil {
			t.Fatal(err)
		}
		if err = c.Core.Complete(context.Background(), fence, core.Completion{Failure: "старое неизвестное исполнение"}); err != nil {
			t.Fatal(err)
		}
		oldID = receipt.SessionID
	})
	receipt, err := f.manager.StartSession(context.Background(), core.StartSessionRequest{RequestID: "new-start", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, receipt); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	old, err := f.service.GetSession(context.Background(), oldID)
	if err != nil || !old.CleanupPending || !old.Issued {
		t.Fatal("неизвестное старое исполнение потеряло удержания", old, err)
	}
}

func TestManagerDynamicOutputRestartKeepsSnapshot(t *testing.T) {
	f := newManagerFixture(t, false, func(c *Config) {
		c.Group.FFmpeg.StartupTimeout = c.StartupTimeout
	})
	ctx := context.Background()
	parent, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "dynamic-parent", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, parent); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	added, err := f.manager.AddOutput(ctx, core.AddOutputRequest{RequestID: "dynamic-add", SessionID: parent.SessionID, DestinationID: f.destination.ID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, added); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	before, err := f.service.GetOutput(ctx, added.OutputID)
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := f.manager.StopOutput(ctx, core.OutputCommand{RequestID: "dynamic-stop", OutputID: added.OutputID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, stopped); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	restarted, err := f.manager.StartOutput(ctx, core.OutputCommand{RequestID: "dynamic-restart", OutputID: added.OutputID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, restarted); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	after, err := f.service.GetOutput(ctx, added.OutputID)
	if err != nil {
		t.Fatal(err)
	}
	left, _ := core.EncodeOutputSnapshot(before.Snapshot)
	right, _ := core.EncodeOutputSnapshot(after.Snapshot)
	if string(left) != string(right) || after.Generation <= before.Generation {
		t.Fatal("перезапуск изменил снимок", before, after)
	}
}

type ambiguousCompletionStore struct {
	*storage.Store
	mu             sync.Mutex
	poisoned       context.Context
	injected       bool
	failRecovery   bool
	recoveryFailed bool
}

func (s *ambiguousCompletionStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	err := s.Store.Write(ctx, callback)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.injected {
		return nil
	}
	var succeeded bool
	check, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	readErr := s.Store.View(check, func(r core.Repositories) error {
		receipts, e := r.Runtime.Receipts(check, core.ListQuery{Limit: 8, State: string(core.CommandSucceeded)})
		if e != nil {
			return e
		}
		for _, receipt := range receipts {
			if receipt.Kind == core.CommandStartSession {
				succeeded = true
			}
		}
		return nil
	})
	if readErr != nil {
		return readErr
	}
	if succeeded {
		s.injected = true
		s.poisoned = ctx
		return context.Canceled
	}
	return nil
}
func (s *ambiguousCompletionStore) View(ctx context.Context, callback func(core.Repositories) error) error {
	s.mu.Lock()
	poisoned := s.poisoned != nil && s.poisoned == ctx
	s.mu.Unlock()
	if poisoned {
		return context.Canceled
	}
	return s.Store.View(ctx, func(r core.Repositories) error {
		r.Runtime = ambiguousReceiptRepository{RuntimeRepository: r.Runtime, store: s}
		return callback(r)
	})
}

type ambiguousReceiptRepository struct {
	core.RuntimeRepository
	store *ambiguousCompletionStore
}

func (r ambiguousReceiptRepository) ReceiptByID(ctx context.Context, id core.ID) (core.CommandReceipt, error) {
	receipt, err := r.RuntimeRepository.ReceiptByID(ctx, id)
	if err != nil {
		return receipt, err
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if r.store.injected && r.store.failRecovery && !r.store.recoveryFailed && receipt.Kind == core.CommandStartSession && receipt.Status == core.CommandSucceeded {
		r.store.recoveryFailed = true
		return core.CommandReceipt{}, core.ErrBusy
	}
	return receipt, nil
}
func TestManagerAmbiguousParentCompletionPreservesSucceededReceipt(t *testing.T) {
	for _, failRecovery := range []bool{false, true} {
		t.Run(fmt.Sprint(failRecovery), func(t *testing.T) {
			f := newManagerFixture(t, false, nil)
			if err := f.manager.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			// Повторный Service использует ту же общую RuntimeAuthority и временную БД.
			store := f.store
			wrapped := &ambiguousCompletionStore{Store: store, failRecovery: failRecovery}
			service, err := core.NewService(wrapped, store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			c := f.config
			c.Core = service
			manager, err := New(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := manager.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			f.manager = manager
			receipt, err := manager.StartSession(context.Background(), core.StartSessionRequest{RequestID: "ambiguous-parent", StreamID: f.stream.ID})
			if err != nil {
				t.Fatal(err)
			}
			current := waitReceipt(t, f, receipt)
			if current.Status != core.CommandSucceeded {
				t.Fatal(current)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				session, e := f.service.GetSession(ctx, receipt.SessionID)
				if e != nil {
					t.Fatal(e)
				}
				if !failRecovery {
					if _, e = manager.Feed(session); e == nil {
						break
					}
				} else if session.State.Terminal() {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("неоднозначный результат оставил Session без завершения", session)
				case <-ticker.C:
				}
			}
			preserved, err := f.service.GetReceipt(context.Background(), receipt.ID)
			if err != nil || preserved.Status != core.CommandSucceeded {
				t.Fatal("исходная успешная квитанция переписана", preserved, err)
			}
		})
	}
}

type delayedPriorStore struct {
	*storage.Store
	requestID        string
	entered, release chan struct{}
	delayed          atomic.Bool
}

func (s *delayedPriorStore) View(ctx context.Context, callback func(core.Repositories) error) error {
	return s.Store.View(ctx, func(r core.Repositories) error {
		r.Runtime = delayedPriorRepository{RuntimeRepository: r.Runtime, store: s}
		return callback(r)
	})
}

type delayedPriorRepository struct {
	core.RuntimeRepository
	store *delayedPriorStore
}

func (r delayedPriorRepository) Receipt(ctx context.Context, requestID string) (core.CommandReceipt, error) {
	receipt, err := r.RuntimeRepository.Receipt(ctx, requestID)
	if requestID == r.store.requestID && errors.Is(err, core.ErrNotFound) && r.store.delayed.CompareAndSwap(false, true) {
		close(r.store.entered)
		select {
		case <-r.store.release:
		case <-ctx.Done():
			return core.CommandReceipt{}, ctx.Err()
		}
	}
	return receipt, err
}
func TestManagerConcurrentPriorWinsOverFreshFileFailure(t *testing.T) {
	f := newManagerFixture(t, true, nil)
	if err := f.manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	wrapped := &delayedPriorStore{Store: f.store, requestID: "concurrent-start", entered: make(chan struct{}), release: make(chan struct{})}
	service, err := core.NewService(wrapped, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	c := f.config
	c.Core = service
	manager, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	request := core.StartSessionRequest{RequestID: wrapped.requestID, StreamID: f.stream.ID}
	type commandResult struct {
		receipt core.CommandReceipt
		err     error
	}
	result := make(chan commandResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { receipt, e := manager.StartSession(ctx, request); result <- commandResult{receipt, e} }()
	select {
	case <-wrapped.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	winner, err := manager.StartSession(ctx, request)
	if err != nil {
		close(wrapped.release)
		t.Fatal(err)
	}
	variant, err := f.service.GetVariant(ctx, f.binding.VariantID)
	if err != nil {
		close(wrapped.release)
		t.Fatal(err)
	}
	if err = f.files.RemoveExact(ctx, variant.ManagedPath, variant.SizeBytes, variant.ContentHash); err != nil {
		close(wrapped.release)
		t.Fatal(err)
	}
	close(wrapped.release)
	select {
	case actual := <-result:
		if actual.err != nil || actual.receipt.ID != winner.ID {
			t.Fatal("параллельная квитанция потеряна после ошибки файла", actual.receipt, actual.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
