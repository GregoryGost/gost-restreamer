package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
	"github.com/GregoryGost/gost-restreamer/internal/session"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

type sessionAcceptanceFixture struct {
	service      *core.Service
	store        *storage.Store
	files        *fallback.FileStore
	processes    *processowner.Registry
	stream       core.Stream
	asset        core.FallbackAsset
	groups       []core.TranscodeGroup
	destinations []core.Destination
	generation   core.FallbackGeneration
	config       session.Config
	manager      *session.Manager
	directory    string
	starts       atomic.Int32
	logs         sessionAcceptanceLog
	diagnosed    atomic.Bool
}

type sessionAcceptanceLog struct {
	mu      sync.Mutex
	data    bytes.Buffer
	dropped int
}

func (l *sessionAcceptanceLog) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(data) > (1<<20)-l.data.Len() {
		l.dropped += len(data)
		return len(data), nil
	}
	return l.data.Write(data)
}

// Сохраняются только полные диагностические записи собственной фикстуры, без медиа и бинарников.
func (f *sessionAcceptanceFixture) captureFailure(t *testing.T) {
	t.Helper()
	if !f.diagnosed.CompareAndSwap(false, true) {
		return
	}
	artifact, err := os.MkdirTemp("", "gost-session-failure-")
	if err != nil {
		t.Log("не удалось сохранить диагностику:", err)
		return
	}
	var report bytes.Buffer
	_, _ = fmt.Fprintf(&report, "test=%s\ntime=%s\nfixture=%s\nbuilders=%d\n", t.Name(), time.Now().UTC().Format(time.RFC3339Nano), f.directory, f.starts.Load())
	f.logs.mu.Lock()
	_, _ = fmt.Fprintf(&report, "manager_log_overflow=%v dropped_bytes=%d\n", f.logs.dropped != 0, f.logs.dropped)
	_, _ = report.Write(f.logs.data.Bytes())
	f.logs.mu.Unlock()
	_, _ = fmt.Fprintln(&report, "\nSupervisor.NextDiagnostic недоступен через публичные Manager/Group интерфейсы; stdout/stderr его частной очереди не получены.")
	// Resolve/RecoverPage изменяют журнал и поэтому не используются для снятия диагностики.
	journalRoot := filepath.Join(f.directory, "p", "scopes")
	remaining, entries := int64(8<<20), 0
	err = filepath.WalkDir(journalRoot, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			_, _ = fmt.Fprintf(&report, "journal_read_error path=%q err=%v\n", name, walkErr)
			return nil
		}
		entries++
		if entries > 4096 {
			_, _ = fmt.Fprintln(&report, "journal_overflow=true reason=entry_budget")
			return fs.SkipAll
		}
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			return nil
		}
		info, readErr := entry.Info()
		if readErr != nil || !info.Mode().IsRegular() {
			_, _ = fmt.Fprintf(&report, "journal_read_error path=%q err=%v regular=false\n", name, readErr)
			return nil
		}
		if info.Size() > remaining {
			_, _ = fmt.Fprintf(&report, "journal_overflow=true omitted_path=%q bytes=%d\n", name, info.Size())
			return fs.SkipAll
		}
		file, readErr := os.Open(name)
		if readErr != nil {
			_, _ = fmt.Fprintf(&report, "journal_read_error path=%q err=%v\n", name, readErr)
			return nil
		}
		data, readErr := io.ReadAll(io.LimitReader(file, remaining+1))
		readErr = errors.Join(readErr, file.Close())
		if readErr != nil {
			_, _ = fmt.Fprintf(&report, "journal_read_error path=%q err=%v\n", name, readErr)
			return nil
		}
		if int64(len(data)) > remaining {
			_, _ = fmt.Fprintf(&report, "journal_overflow=true omitted_path=%q\n", name)
			return fs.SkipAll
		}
		remaining -= int64(len(data))
		_, _ = fmt.Fprintf(&report, "journal_record=%q\n%s\n", name, data)
		return nil
	})
	if err != nil {
		_, _ = fmt.Fprintln(&report, "journal_walk_error:", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	query := core.PageRequest{Query: core.ListQuery{Limit: 64}}
	receipts, receiptErr := f.service.ListReceipts(ctx, query)
	_, _ = fmt.Fprintf(&report, "receipts=%+v err=%v receipt_page_overflow=%v\n", receipts, receiptErr, receipts.NextCursor != "")
	query.Query.StreamID = f.stream.ID
	sessions, sessionErr := f.service.ListSessions(ctx, query)
	_, _ = fmt.Fprintf(&report, "sessions=%+v err=%v session_page_overflow=%v\n", sessions, sessionErr, sessions.NextCursor != "")
	for _, current := range sessions.Items {
		outputs, outputErr := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 64, SessionID: current.ID}})
		snapshot, snapshotErr := f.service.RuntimeSnapshot(ctx, core.RuntimeSessionFence{SessionID: current.ID, Incarnation: current.Incarnation, Lifecycle: current.Lifecycle})
		encoded, encodeErr := json.Marshal(snapshot)
		_, _ = fmt.Fprintf(&report, "session_id=%s outputs=%+v err=%v output_page_overflow=%v\nruntime=%s err=%v encode_error=%v\n", current.ID, outputs, outputErr, outputs.NextCursor != "", encoded, snapshotErr, encodeErr)
	}
	name := filepath.Join(artifact, "diagnostics.log")
	if err := os.WriteFile(name, report.Bytes(), 0600); err != nil {
		t.Log("не удалось записать диагностику:", err)
		return
	}
	t.Log("диагностика до очистки фикстуры:", name)
}

func sessionAcceptanceOptions() storage.Options {
	return storage.Options{OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 16, MaxIdentifierBytes: 1024, MaxBytes: 1 << 20}, PreparationLimits: core.PreparationLimits{MaxQueued: 8, MaxProfiles: 16}, ObservationLimits: core.ObservationLimits{MaxSessions: 8, MaxGroups: 32, MaxOutputs: 64, MaxFFmpegWorkers: 32, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 20 * time.Millisecond, RetryAttempts: 4, RetryTimeout: time.Second, RetryDelay: time.Millisecond}
}

func sessionAcceptanceRejectingEndpoint(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_ = connection.Close()
		}
	}()
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("отказывающий RTMP-приёмник не завершился")
		}
	})
	return "rtmp://" + listener.Addr().String() + "/live"
}

func newSessionAcceptanceFixture(t *testing.T, helper string, endpoints []string, groupIndices []int) *sessionAcceptanceFixture {
	t.Helper()
	ctx := context.Background()
	base, err := os.MkdirTemp("/tmp", "session-acceptance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	f := &sessionAcceptanceFixture{directory: base}
	f.store, err = storage.Open(ctx, filepath.Join(base, "state.sqlite"), sessionAcceptanceOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.store.Close(); err != nil {
			t.Error(err)
		}
	})
	f.service, err = core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	f.files, err = fallback.OpenFileStore(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.files.Close(); err != nil {
			t.Error(err)
		}
	})
	f.processes, err = processowner.New(processowner.Config{Root: filepath.Join(base, "p"), RuntimeRoot: filepath.Join(base, "r"), HelperPath: helper, GracePeriod: 50 * time.Millisecond, Limits: sessionAcceptanceProcessLimits()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.processes.Close(); err != nil {
			t.Error(err)
		}
	})
	f.asset, err = f.service.CreateAsset(ctx, "Локальная заглушка")
	if err != nil {
		t.Fatal(err)
	}
	watchdog := core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: 100 * time.Millisecond, KeyframeTimeout: time.Second}
	f.stream, err = f.service.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "synthetic-session-key", Fallback: core.FallbackPolicy{AssetID: f.asset.ID, Watchdog: watchdog}})
	if err != nil {
		t.Fatal(err)
	}
	profile := publisherProfile(core.CodecAV1, 640, 360)
	saved, err := f.service.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		g, err := f.service.CreateGroup(ctx, core.TranscodeGroup{Name: fmt.Sprintf("Группа %d", i), StreamID: f.stream.ID, ProfileID: saved.ID})
		if err != nil {
			t.Fatal(err)
		}
		f.groups = append(f.groups, g)
	}
	for i, endpoint := range endpoints {
		d, err := f.service.CreateDestination(ctx, core.Destination{Name: fmt.Sprintf("Назначение %d", i), StreamID: f.stream.ID, GroupID: f.groups[groupIndices[i]].ID, Platform: "custom", Endpoint: endpoint, StreamKey: fmt.Sprintf("synthetic-key-%d", i), Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		f.destinations = append(f.destinations, d)
	}
	f.seedFallback(t)
	f.config = session.Config{Core: f.service, Files: f.files, Processes: f.processes, GeneratorVersion: "controlled-generator", Router: publisherRouterLimits(), Publisher: publisher.Options{Queue: publisherQueue(), RetryInitial: 10 * time.Millisecond, RetryMax: 50 * time.Millisecond, ResubscribeDelay: 5 * time.Millisecond, Transport: publisherTransportOptions(nil)}, Logger: slog.New(slog.NewJSONHandler(&f.logs, nil)), MaxSessions: 1, MaxOutputs: 8, MaxCommands: 8, PageSize: 8, MaxGroups: 4, MaxReferences: 8, ReconcileInterval: 5 * time.Millisecond, StartupTimeout: 4 * time.Second, OperationTimeout: 300 * time.Millisecond, ShutdownTimeout: 4 * time.Second,
		Group: group.RuntimeConfig{Switcher: group.SwitcherConfig{Profile: profile, Watchdog: watchdog, Queue: publisherQueue(), Router: publisherRouterLimits(), MaxAVSkew: 100 * time.Millisecond, MaxTransitionGap: 100 * time.Millisecond}, Container: integrationLimits(), Binary: helper, ResubscribeBackoff: 5 * time.Millisecond, ControlInterval: 5 * time.Millisecond, ShutdownTimeout: 3 * time.Second, FFmpeg: ffmpeg.Config{BaseDir: filepath.Join(base, "r"), StartupTimeout: 3 * time.Second, VideoTimeout: 2 * time.Second, AudioTimeout: 2 * time.Second, WatchdogInterval: 10 * time.Millisecond, MaxRestarts: 0, RestartBackoff: time.Millisecond, GracePeriod: 50 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 3 * time.Second, DiagnosticEvents: 64, DiagnosticBytes: 65536, DiagnosticChunkBytes: 1024, MaxCommandBytes: 8192, MaxCommandArgs: 128}}}
	f.config.Monitoring = monitoring.SamplerConfig{MaxSources: 128, StaleAfter: 5 * time.Second, ShutdownTimeout: 4 * time.Second}
	f.config.Telemetry = core.TelemetryLimits{Events: core.EventLimits{MaxEvents: 128, MaxBytes: 1 << 20, MaxEventBytes: 65536, MaxPageSize: 64}, MediaStaleAfter: 5 * time.Second, MaxGPUDevices: 8, MaxGPUUUIDBytes: 256, MaxGPUBytes: 1 << 20}
	f.config.Group.FFmpeg.Progress = ffmpeg.ProgressConfig{MaxLineBytes: 4096, MaxReportBytes: 65536, MaxFields: 64}
	f.configureOwners(t, helper)
	f.config.Group.Player = func(_ context.Context, _ string, _ *os.File, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
		f.starts.Add(1)
		return publisherControlledCommand(profile, attempt, 10, "")
	}
	t.Cleanup(func() {
		if t.Failed() {
			f.captureFailure(t)
		}
	})
	return f
}

func (f *sessionAcceptanceFixture) configureOwners(t *testing.T, helper string) {
	t.Helper()
	preparation, err := fallback.NewManager(f.service, f.files, fallback.Config{ReconcileLimit: 16, GeneratorVersion: "controlled-generator", FFmpeg: helper, FFprobe: helper, ProcessRegistry: f.processes, MaxUploadBytes: 1 << 20, MaxOutputBytes: 1 << 20, PrepareTimeout: 5 * time.Second, ShutdownTimeout: 3 * time.Second, QueueInterval: time.Millisecond, Runner: fallback.RunnerLimits{Timeout: 3 * time.Second, MaxArgs: 128, MaxCommandBytes: 8192, MaxDiagnosticBytes: 65536}, Probe: fallback.ProbeLimits{MaxJSONBytes: 65536, MaxWidth: 4096, MaxHeight: 4096, MaxPixels: 4096 * 4096, MaxDuration: time.Minute, ImageDuration: time.Second, MaxPackets: 1000, Container: integrationLimits()}})
	if err != nil {
		t.Fatal(err)
	}
	ingress, err := rtmp.NewServer(rtmp.Options{Core: f.service, Route: rtmp.ConventionalRoute, Limits: publisherRTMPLimits(), Feed: func(current core.Session) (*media.Router, error) { return f.manager.Feed(current) }})
	if err != nil {
		t.Fatal(err)
	}
	f.config.Core, f.config.Files, f.config.Preparation, f.config.Ingress = f.service, f.files, preparation, ingress
}

func (f *sessionAcceptanceFixture) seedFallback(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	g, err := f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: f.asset.ID, ExpectedRevision: f.asset.Revision, Kind: core.AssetImage, GeneratorVersion: "controlled-generator"})
	if err != nil {
		t.Fatal(err)
	}
	pf, err := f.service.ClaimPreparation(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.MarkPreparationIssued(ctx, pf); err != nil {
		t.Fatal(err)
	}
	ledger, err := f.service.ListGenerationFiles(ctx, core.PageRequest{Query: core.ListQuery{Limit: 32, GenerationID: g.ID}})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range ledger.Items {
		if file.Kind == core.OwnedDirectory {
			if err = f.files.CreateOwnedGeneration(file); err != nil {
				t.Fatal(err)
			}
		}
	}
	data := []byte("immutable local fallback fixture")
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	for _, file := range ledger.Items {
		if file.Kind != core.OwnedOriginal && file.Kind != core.OwnedVariant {
			continue
		}
		writer, err := f.files.CreateOwned(file)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := writer.Write(data)
		if err = errors.Join(writeErr, writer.Sync(), writer.Close()); err != nil {
			t.Fatal(err)
		}
	}
	publication := core.GenerationPublication{SourceHash: digest, SizeBytes: uint64(len(data)), SourceFormat: core.SourceFormat{ImageFormat: "png"}}
	for _, required := range g.RequiredProfiles {
		publication.Variants = append(publication.Variants, core.FallbackVariant{ID: required.VariantID, GenerationID: g.ID, AssetID: g.AssetID, SourceHash: digest, Profile: required.Profile, VerifiedFormat: required.Profile.Format, GeneratorVersion: g.GeneratorVersion, ManagedPath: required.ManagedPath, SizeBytes: uint64(len(data)), ContentHash: digest})
	}
	f.generation, err = f.service.PublishPreparation(ctx, pf, publication)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.AcknowledgePreparationCleanup(ctx, pf); err != nil {
		t.Fatal(err)
	}
}

func (f *sessionAcceptanceFixture) launch(t *testing.T) {
	t.Helper()
	var err error
	f.manager, err = session.New(context.Background(), f.config)
	if err != nil {
		f.captureFailure(t)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			f.captureFailure(t)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := f.manager.Close(ctx); err != nil {
			t.Error("закрытие Manager:", err)
			f.captureFailure(t)
		}
	})
}

func (f *sessionAcceptanceFixture) start(t *testing.T, requestID string, selected ...core.ID) core.CommandReceipt {
	t.Helper()
	r, err := f.manager.StartSession(context.Background(), core.StartSessionRequest{RequestID: requestID, StreamID: f.stream.ID, DestinationIDs: selected})
	f.config.Logger.Info("fixture_start_result", "request_id", requestID, "receipt", r, "error", err)
	if err != nil {
		f.captureFailure(t)
		t.Fatalf("принятие start RequestID=%q receipt=%+v: %v", requestID, r, err)
	}
	return r
}

func sessionAcceptanceReceipt(t *testing.T, svc *core.Service, id core.ID, status core.CommandStatus, failureDiagnostics ...func()) core.CommandReceipt {
	t.Helper()
	var result core.CommandReceipt
	diagnose := func() {
		for _, capture := range failureDiagnostics {
			capture()
		}
		t.Logf("ожидание квитанции id=%s RequestID=%q status=%s: %+v", id, result.RequestID, status, result)
	}
	sessionAcceptanceWait(t, func() bool {
		current, err := svc.GetReceipt(context.Background(), id)
		if err != nil {
			diagnose()
			t.Fatalf("чтение квитанции id=%s RequestID=%q: %v", id, result.RequestID, err)
		}
		result = current
		if result.Status.Terminal() && result.Status != status {
			diagnose()
			t.Fatalf("квитанция %s RequestID=%q: ожидалось %s, получено %+v", id, result.RequestID, status, result)
		}
		return result.Status == status
	}, diagnose)
	return result
}

func sessionAcceptanceWait(t *testing.T, predicate func() bool, timeoutDiagnostics ...func()) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !predicate() {
		select {
		case <-timer.C:
			for _, capture := range timeoutDiagnostics {
				capture()
			}
			t.Fatal("условие lifecycle не выполнено до срока")
		case <-ticker.C:
		}
	}
}

func (f *sessionAcceptanceFixture) running(t *testing.T, receipt core.CommandReceipt) core.Session {
	t.Helper()
	diagnose := func() { f.captureFailure(t) }
	sessionAcceptanceReceipt(t, f.service, receipt.ID, core.CommandSucceeded, diagnose)
	current, err := f.service.GetSession(context.Background(), receipt.SessionID)
	if err != nil || current.State != core.SessionRunning {
		diagnose()
		t.Fatalf("RequestID=%q receipt=%+v Session=%+v: %v", receipt.RequestID, receipt, current, err)
	}
	var feedErr error
	sessionAcceptanceWait(t, func() bool { _, feedErr = f.manager.Feed(current); return feedErr == nil }, func() {
		t.Logf("Feed RequestID=%q receipt=%+v Session=%+v: %v", receipt.RequestID, receipt, current, feedErr)
		diagnose()
	})
	return current
}

func (f *sessionAcceptanceFixture) outputs(t *testing.T, sessionID core.ID) []core.SessionOutput {
	t.Helper()
	page, err := f.service.ListOutputs(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 32, SessionID: sessionID}})
	if err != nil {
		t.Fatal(err)
	}
	return page.Items
}

func TestSessionManagerRequiresCurrentAudioVideoFallback(t *testing.T) {
	helper := sessionAcceptanceBuildHelper(t)
	receiver, _ := newPublisherReceiver(t, false)
	f := newSessionAcceptanceFixture(t, helper, []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	gate, started := filepath.Join(f.directory, "audio-enabled"), filepath.Join(f.directory, "video-started")
	f.config.Group.Player = func(_ context.Context, _ string, _ *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		f.starts.Add(1)
		binary, err := os.Executable()
		return ffmpeg.Command{Binary: binary, Args: []string{"-test.run=^TestSessionAcceptanceMediaChild$", "--", a.OutputPath, gate, started}}, err
	}
	f.launch(t)
	r := f.start(t, "start-with-track-barrier", f.destinations[0].ID)
	sessionAcceptanceWait(t, func() bool { _, err := os.Stat(started); return err == nil })
	current, err := f.service.GetSession(context.Background(), r.SessionID)
	if err != nil || current.State != core.SessionStarting || current.AllowsOBS(f.stream.PublicationKey) {
		t.Fatalf("видео без аудио прошло barrier: %+v %v", current, err)
	}
	if _, err = f.manager.Feed(current); err == nil {
		t.Fatal("Feed доступен до обеих дорожек")
	}
	if receiver.accepted.Load() != 0 {
		t.Fatal("сеть началась до parent readiness")
	}
	if err = os.WriteFile(gate, []byte{1}, 0600); err != nil {
		t.Fatal(err)
	}
	current = f.running(t, r)
	receiver.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= 8 })
	if len(current.Snapshot.Groups) != 2 || f.starts.Load() != 1 || len(f.outputs(t, current.ID)) != 1 {
		t.Fatal("невыбранная группа запущена либо потеряна в снимке")
	}
	if _, err = session.New(context.Background(), f.config); !errors.Is(err, core.ErrInUse) {
		t.Fatalf("второй диспетчер: %v", err)
	}
	path := filepath.Join(f.directory, f.generation.RequiredProfiles[0].ManagedPath)
	if err = os.Rename(path, path+".temporarily-unavailable"); err != nil {
		t.Fatal(err)
	}
	duplicate := f.start(t, "start-with-track-barrier", f.destinations[0].ID)
	if duplicate.ID != r.ID || f.starts.Load() != 1 {
		t.Fatal("повтор RequestID повторил физический startup")
	}
	if err = os.Rename(path+".temporarily-unavailable", path); err != nil {
		t.Fatal(err)
	}
}

// Дочерний процесс выдаёт структурные AU; декодируемость изображения проверяется отдельно.
func TestSessionAcceptanceMediaChild(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "--" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	args := os.Args[index+1:]
	if len(args) != 3 {
		os.Exit(61)
	}
	conn, err := net.Dial("unix", args[0])
	if err != nil {
		os.Exit(62)
	}
	defer func() { _ = conn.Close() }()
	v, a := publisherConfigs(t, publisherProfile(core.CodecAV1, 640, 360), 1)
	mux, err := fmp4.NewMuxer(conn, fmp4.Init{Video: v, Audio: a}, integrationLimits())
	if err != nil {
		os.Exit(63)
	}
	ticker := time.NewTicker(time.Second / 60)
	defer ticker.Stop()
	var audio int64
	for video := int64(0); ; video++ {
		packet, err := media.NewPacket(media.PacketInfo{PTS: video, DTS: video, DurationTicks: 1, Keyframe: true}, v, publisherPayload(v, 10))
		if err != nil || mux.WritePacket(packet) != nil {
			return
		}
		if video == 0 {
			if err = os.WriteFile(args[2], []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
				os.Exit(64)
			}
		}
		if _, err = os.Stat(args[1]); err == nil {
			if audio == 0 {
				audio = video * 800
			}
			for audio*60 < (video+1)*48000 {
				packet, err = media.NewPacket(media.PacketInfo{PTS: audio, DTS: audio, DurationTicks: 1024}, a, publisherPayload(a, 0))
				if err != nil || mux.WritePacket(packet) != nil {
					return
				}
				audio += 1024
			}
		}
		<-ticker.C
	}
}

func TestSessionManagerCommonFailureEndsSessionAndJoinsResources(t *testing.T) {
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), []string{sessionAcceptanceRejectingEndpoint(t)}, []int{0})
	f.config.Group.Player = func(context.Context, string, *os.File, ffmpeg.Attempt) (ffmpeg.Command, error) {
		return ffmpeg.Command{Binary: "/usr/bin/false"}, nil
	}
	f.launch(t)
	r := f.start(t, "start-with-local-failure")
	sessionAcceptanceReceipt(t, f.service, r.ID, core.CommandFailed)
	current, err := f.service.GetSession(context.Background(), r.SessionID)
	if err != nil || current.State != core.SessionFailed || current.Failure == "" {
		t.Fatalf("общий startup failure: %+v %v", current, err)
	}
	if _, err = f.manager.Feed(current); err == nil {
		t.Fatal("Feed сохранён после startup failure")
	}
	sessionAcceptanceWait(t, func() bool {
		current, err := f.service.GetSession(context.Background(), r.SessionID)
		return err == nil && !current.CleanupPending && !current.Issued
	})
}

func TestSessionManagerStopDuringStartupExcludesOnlyItsOutput(t *testing.T) {
	helper := sessionAcceptanceBuildHelper(t)
	for _, scenario := range []struct {
		remaining int
		detach    bool
	}{{0, false}, {1, false}, {0, true}, {1, true}} {
		t.Run(fmt.Sprintf("remaining-%d-detach-%v", scenario.remaining, scenario.detach), func(t *testing.T) {
			receiver, _ := newPublisherReceiver(t, false)
			endpoints := []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}
			if scenario.remaining == 1 {
				endpoints = append(endpoints, endpoints[0])
			}
			f := newSessionAcceptanceFixture(t, helper, endpoints, make([]int, len(endpoints)))
			entered, release := make(chan struct{}, 1), make(chan struct{})
			f.config.Group.Player = func(ctx context.Context, _ string, _ *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-ctx.Done():
					return ffmpeg.Command{}, ctx.Err()
				case <-release:
				}
				return publisherControlledCommand(publisherProfile(core.CodecAV1, 640, 360), a, 10, "")
			}
			f.launch(t)
			r := f.start(t, "start-before-output-exclusion")
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("builder не начался")
			}
			outputs := f.outputs(t, r.SessionID)
			var stopped core.SessionOutput
			for _, o := range outputs {
				if o.Snapshot.Destination.ID == f.destinations[0].ID {
					stopped = o
				}
			}
			command := core.OutputCommand{RequestID: "stop-during-startup", OutputID: stopped.ID}
			var stop core.CommandReceipt
			var err error
			if scenario.detach {
				stop, err = f.manager.DetachOutput(context.Background(), command)
			} else {
				stop, err = f.manager.StopOutput(context.Background(), command)
			}
			if err != nil {
				t.Fatal(err)
			}
			close(release)
			current := f.running(t, r)
			sessionAcceptanceReceipt(t, f.service, stop.ID, core.CommandSucceeded)
			output, err := f.service.GetOutput(context.Background(), stopped.ID)
			if err != nil || output.DesiredRunning || output.CleanupPending || output.Issued || output.Attempt != 0 {
				t.Fatalf("фиктивная выдача/неочищенный provisional Output: %+v %v", output, err)
			}
			if output.Attached == scenario.detach {
				t.Fatal("stop/detach потерял контракт прикрепления")
			}
			if len(current.Snapshot.Groups) != 2 || len(f.outputs(t, current.ID)) != len(endpoints) {
				t.Fatal("startup переписал историю")
			}
			if scenario.remaining == 0 {
				if receiver.accepted.Load() != 0 {
					t.Fatal("остановленный выход открыл сеть")
				}
			} else {
				receiver.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= 8 })
				if receiver.accepted.Load() != 1 {
					t.Fatal("соседний выход пересоздан либо остановленный запущен")
				}
			}
		})
	}
}

func TestSessionManagerDynamicOutputsPreserveHealthyConnection(t *testing.T) {
	receiver, _ := newPublisherReceiver(t, false)
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), []string{"rtmp://" + receiver.listener.Addr().String() + "/live", sessionAcceptanceRejectingEndpoint(t)}, []int{0, 1})
	f.launch(t)
	r := f.start(t, "start-healthy", f.destinations[0].ID)
	current := f.running(t, r)
	receiver.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= 8 })
	add, err := f.manager.AddOutput(context.Background(), core.AddOutputRequest{RequestID: "add-unavailable-destination", SessionID: current.ID, DestinationID: f.destinations[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	sessionAcceptanceReceipt(t, f.service, add.ID, core.CommandSucceeded)
	second, err := f.service.GetOutput(context.Background(), add.OutputID)
	if err != nil {
		t.Fatal(err)
	}
	sessionAcceptanceWait(t, func() bool {
		snapshot, err := f.service.RuntimeSnapshot(context.Background(), core.RuntimeSessionFence{SessionID: current.ID, Incarnation: current.Incarnation, Lifecycle: current.Lifecycle})
		if err != nil {
			return false
		}
		for _, output := range snapshot.Outputs {
			if output.Execution.OutputID == second.ID && output.Value != nil && output.Value.State == core.OutputRetrying {
				return true
			}
		}
		return false
	})
	before := publisherMediaCount(receiver.snapshot())
	pause, err := f.manager.PauseSession(context.Background(), core.SessionCommand{RequestID: "pause-with-retrying-destination", SessionID: current.ID})
	if err != nil {
		t.Fatal(err)
	}
	sessionAcceptanceReceipt(t, f.service, pause.ID, core.CommandSucceeded)
	paused, err := f.service.GetSession(context.Background(), current.ID)
	if err != nil || !paused.DesiredPaused || !paused.AllowsOBS(f.stream.PublicationKey) {
		t.Fatalf("pause: %+v %v", paused, err)
	}
	detach, err := f.manager.DetachOutput(context.Background(), core.OutputCommand{RequestID: "detach-retrying-destination", OutputID: second.ID})
	if err != nil {
		t.Fatal(err)
	}
	sessionAcceptanceReceipt(t, f.service, detach.ID, core.CommandSucceeded)
	receiver.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= before+8 })
	if receiver.accepted.Load() != 1 {
		t.Fatal("изменения соседа пересоздали здоровый socket")
	}
	if _, err = f.manager.Feed(paused); err != nil {
		t.Fatal("изолированный отказ потерял Feed:", err)
	}
	if len(current.Snapshot.Groups) != 2 || len(f.outputs(t, current.ID)) != 2 {
		t.Fatal("Add/Detach переписали историю запуска")
	}
}
