package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/message"
)

func publisherProfile(c core.Codec, width, height uint32) core.GroupProfile {
	p := fallbackPipelineProfile(c)
	p.Format.Video.Width, p.Format.Video.Height = width, height
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 60, Denominator: 1}
	p.VideoEncoding.GOPFrames = 60
	return p
}
func publisherQueue() media.QueueLimits {
	return media.QueueLimits{MaxPackets: 512, MaxBytes: 8 << 20, MaxPacketBytes: 1 << 20, MaxConfigBytes: 4096, ResyncTimeout: 2 * time.Second}
}
func publisherRouterLimits() media.RouterLimits {
	return media.RouterLimits{MaxConsumers: 12, MaxRetainedBytes: 128 << 20, MaxConfigBytes: 4096}
}
func publisherRTMPLimits() rtmp.Limits {
	return rtmp.Limits{MaxConnections: 8, MaxChunkStreams: 16, MaxMessageBytes: 1 << 20, MaxChunkBytes: 65536, MaxFragments: 8192, MaxRetainedBytes: 32 << 20, MaxControlBytes: 64, MaxConfigBytes: 4096, MaxAMFBytes: 4096, MaxAMFDepth: 8, MaxAMFElements: 128, MaxReadAheadBytes: 65554, HandshakeTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 2 * time.Second, ShutdownTimeout: 3 * time.Second}
}

func publisherTransportOptions(trust *tls.Config) rtmp.OutboundOptions {
	return rtmp.OutboundOptions{TLSConfig: trust, Limits: rtmp.OutboundLimits{MaxChunkStreams: 16, MaxMessageBytes: 1 << 20, MaxChunkBytes: 65536, MaxFragments: 8192, MaxControlBytes: 64, MaxConfigBytes: 4096, MaxAMFBytes: 4096, MaxAMFDepth: 8, MaxAMFElements: 128, ChunkSize: 128, MaxControlPending: 32, MaxRetainedBytes: 32 << 20, DialTimeout: time.Second, TLSTimeout: time.Second, StartupTimeout: 3 * time.Second, MessageTimeout: time.Second, WriteTimeout: time.Second}}
}

type publisherPipeline struct {
	service  *core.Service
	stream   core.Stream
	parent   core.ExecutionFence
	observer *core.SessionObserver
	outputs  []core.SessionOutput
	live     *media.Router
	config   group.RuntimeConfig
}

func newPublisherPipeline(t *testing.T, profiles []core.GroupProfile, endpoints []string, keys []string, groupIndices []int) publisherPipeline {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "publisher.sqlite"), storage.Options{OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 16, MaxIdentifierBytes: 1024, MaxBytes: 1 << 20}, PreparationLimits: core.PreparationLimits{MaxQueued: 4, MaxProfiles: 16}, ObservationLimits: core.ObservationLimits{MaxSessions: 8, MaxGroups: 32, MaxOutputs: 64, MaxFFmpegWorkers: 32, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 20 * time.Millisecond, RetryAttempts: 4, RetryTimeout: time.Second, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := store.Close(); e != nil {
			t.Error(e)
		}
	})
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err := store.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SQLite=%s Goose=%d", diagnostics.SQLiteVersion, diagnostics.SchemaVersion)
	asset, err := svc.CreateAsset(ctx, "Заглушка")
	if err != nil {
		t.Fatal(err)
	}
	watchdog := core.WatchdogPolicy{VideoTimeout: 300 * time.Millisecond, AudioTimeout: 300 * time.Millisecond, RecoveryWindow: 70 * time.Millisecond, KeyframeTimeout: time.Second}
	stream, err := svc.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "synthetic-ingest-key", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: watchdog}})
	if err != nil {
		t.Fatal(err)
	}
	var groups []core.TranscodeGroup
	for _, p := range profiles {
		profile, e := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: p})
		if e != nil {
			t.Fatal(e)
		}
		g, e := svc.CreateGroup(ctx, core.TranscodeGroup{Name: "Группа", StreamID: stream.ID, ProfileID: profile.ID})
		if e != nil {
			t.Fatal(e)
		}
		groups = append(groups, g)
	}
	for i, endpoint := range endpoints {
		_, e := svc.CreateDestination(ctx, core.Destination{Name: "Назначение", StreamID: stream.ID, GroupID: groups[groupIndices[i]].ID, Platform: "custom", Endpoint: endpoint, StreamKey: keys[i], Enabled: true})
		if e != nil {
			t.Fatal(e)
		}
	}
	gen, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "controlled-generator"})
	if err != nil {
		t.Fatal(err)
	}
	pf, err := svc.ClaimPreparation(ctx, gen.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkPreparationIssued(ctx, pf); err != nil {
		t.Fatal(err)
	}
	publication := core.GenerationPublication{SourceHash: "controlled-source", SizeBytes: 1, SourceFormat: core.SourceFormat{ImageFormat: "png"}}
	for _, r := range gen.RequiredProfiles {
		publication.Variants = append(publication.Variants, core.FallbackVariant{Record: core.Record{ID: r.VariantID}, GenerationID: gen.ID, AssetID: asset.ID, SourceHash: publication.SourceHash, Profile: r.Profile, VerifiedFormat: r.Profile.Format, GeneratorVersion: gen.GeneratorVersion, ManagedPath: r.ManagedPath, SizeBytes: 1, ContentHash: "controlled-result"})
	}
	if _, err = svc.PublishPreparation(ctx, pf, publication); err != nil {
		t.Fatal(err)
	}
	if err = svc.AcknowledgePreparationCleanup(ctx, pf); err != nil {
		t.Fatal(err)
	}
	request := core.StartSessionRequest{RequestID: "start-publisher-pipeline", StreamID: stream.ID}
	for i, g := range groups {
		for _, r := range gen.RequiredProfiles {
			if r.Profile.Equal(profiles[i]) {
				request.Bindings = append(request.Bindings, core.PreparedBinding{GroupID: g.ID, GenerationID: gen.ID, VariantID: r.VariantID, GeneratorVersion: gen.GeneratorVersion})
			}
		}
	}
	receipt, err := svc.StartSession(ctx, request)
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
	outputs, err := svc.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 32, SessionID: parent.SessionID}})
	if err != nil {
		t.Fatal(err)
	}
	live, err := media.NewRouter(publisherRouterLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })
	base, err := os.MkdirTemp("/tmp", "publisher-group-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := os.RemoveAll(base); e != nil {
			t.Error(e)
		}
	})
	config := group.RuntimeConfig{Switcher: group.SwitcherConfig{Profile: profiles[0], Watchdog: watchdog, Queue: publisherQueue(), Router: publisherRouterLimits(), MaxAVSkew: 100 * time.Millisecond, MaxTransitionGap: 100 * time.Millisecond}, Container: integrationLimits(), Binary: "controlled", ResubscribeBackoff: 5 * time.Millisecond, ControlInterval: 5 * time.Millisecond, ShutdownTimeout: 3 * time.Second, FFmpeg: ffmpeg.Config{BaseDir: base, StartupTimeout: 3 * time.Second, VideoTimeout: 2 * time.Second, AudioTimeout: 2 * time.Second, WatchdogInterval: 10 * time.Millisecond, MaxRestarts: 0, RestartBackoff: time.Millisecond, GracePeriod: 50 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 3 * time.Second, DiagnosticEvents: 64, DiagnosticBytes: 1 << 16, DiagnosticChunkBytes: 1024, MaxCommandBytes: 8192, MaxCommandArgs: 128}}
	return publisherPipeline{svc, stream, parent, observer, outputs.Items, live, config}
}

func publisherWait(t *testing.T, condition func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-ctx.Done():
			t.Fatal("условие медиатракта не выполнено до срока")
		case <-ticker.C:
		}
	}
}

func publisherActivate(t *testing.T, f publisherPipeline, o core.SessionOutput, router *media.Router, trust *tls.Config, configure ...func(*publisher.Options)) *publisher.Publisher {
	t.Helper()
	ctx := context.Background()
	receipts, err := f.service.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 64, SessionID: f.parent.SessionID}})
	if err != nil {
		t.Fatal(err)
	}
	var requestID string
	for _, r := range receipts.Items {
		if r.OutputID == o.ID && r.Kind == core.CommandStartOutput && r.Status == core.CommandPending {
			requestID = r.RequestID
		}
	}
	if requestID == "" {
		t.Fatal("нет собственной начальной квитанции выхода")
	}
	fence, err := f.service.Claim(ctx, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	owner, err := f.service.BindExecutionOwner(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	options := publisher.Options{Queue: publisherQueue(), RetryInitial: 10 * time.Millisecond, RetryMax: 80 * time.Millisecond, ResubscribeDelay: 5 * time.Millisecond, Transport: rtmp.OutboundOptions{}}
	options.Transport = publisherTransportOptions(trust)
	for _, configure := range configure {
		configure(&options)
	}
	p, err := publisher.Prepare(ctx, f.service, owner, o.Snapshot, router, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if err := p.Stop(cleanup); err != nil {
			t.Error(err)
		}
		if _, err := f.service.StopOutput(cleanup, core.OutputCommand{RequestID: "stop-" + string(o.ID), OutputID: o.ID}); err != nil {
			t.Error(err)
		}
		if err := f.service.ConfirmOutputExecutionResolution(cleanup, p.Reservation()); err != nil {
			t.Error(err)
		}
		if err := p.ReleaseJoined(); err != nil {
			t.Error(err)
		}
	})
	if err = f.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	observer, err := f.service.OpenOutputObserver(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(context.Background(), observer); err != nil {
		t.Fatal(err)
	}
	return p
}

func publisherPayload(c media.CodecConfig, marker byte) []byte {
	if c.Info().Track == media.TrackAudio {
		return []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c}
	}
	if c.Info().Video.Codec == core.CodecAV1 {
		return append(append(c.Headers()[4:], 0x1a, 1, 0x10), 0x2a, 1, marker)
	}
	return []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40, 0, 0, 0, 2, 0x0c, marker}
}

func publisherControlledCommand(profile core.GroupProfile, a ffmpeg.Attempt, marker byte, control string) (ffmpeg.Command, error) {
	binary, err := os.Executable()
	if err != nil {
		return ffmpeg.Command{}, err
	}
	return ffmpeg.Command{Binary: binary, Args: []string{"-test.run=^TestPublisherControlledMediaProcess$", "--", a.OutputPath, string(profile.Format.Video.Codec), fmt.Sprint(profile.Format.Video.Width), fmt.Sprint(profile.Format.Video.Height), a.InputPath, fmt.Sprint(marker), control}}, nil
}

// Процесс выдаёт структурные AU: этот сценарий не утверждает декодируемость изображения.
func TestPublisherControlledMediaProcess(t *testing.T) {
	position := -1
	for i, a := range os.Args {
		if a == "--" {
			position = i
			break
		}
	}
	if position < 0 {
		return
	}
	args := os.Args[position+1:]
	if len(args) != 7 {
		os.Exit(31)
	}
	w, e := strconv.ParseUint(args[2], 10, 32)
	if e != nil {
		os.Exit(32)
	}
	h, e := strconv.ParseUint(args[3], 10, 32)
	if e != nil {
		os.Exit(33)
	}
	marker, e := strconv.ParseUint(args[5], 10, 8)
	if e != nil {
		os.Exit(34)
	}
	output, e := net.Dial("unix", args[0])
	if e != nil {
		os.Exit(35)
	}
	defer func() { _ = output.Close() }()
	if args[4] != "" {
		input, e := net.Dial("unix", args[4])
		if e != nil {
			os.Exit(36)
		}
		defer func() { _ = input.Close() }()
		go func() { _, _ = io.Copy(io.Discard, input) }()
	}
	p := publisherProfile(core.Codec(args[1]), uint32(w), uint32(h))
	v, a := publisherConfigs(t, p, 1)
	mux, e := fmp4.NewMuxer(output, fmp4.Init{Video: v, Audio: a}, integrationLimits())
	if e != nil {
		os.Exit(37)
	}
	ticker := time.NewTicker(time.Second / 60)
	defer ticker.Stop()
	var audio int64
	for video := int64(0); ; video++ {
		if args[6] != "" {
			if _, e = os.Stat(args[6]); e == nil {
				return
			}
		}
		packet, e := media.NewPacket(media.PacketInfo{PTS: video, DTS: video, DurationTicks: 1, Keyframe: true}, v, publisherPayload(v, byte(marker)))
		if e != nil || mux.WritePacket(packet) != nil {
			return
		}
		for audio*60 < (video+1)*48000 {
			packet, e = media.NewPacket(media.PacketInfo{PTS: audio, DTS: audio, DurationTicks: 1024}, a, publisherPayload(a, 0))
			if e != nil || mux.WritePacket(packet) != nil {
				return
			}
			audio += 1024
		}
		<-ticker.C
	}
}

func publisherStartIngest(t *testing.T, f publisherPipeline, packets []media.Packet, init fmp4.Init) (*gortmplib.Client, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	server, err := rtmp.NewServer(rtmp.Options{Limits: publisherRTMPLimits(), Core: f.service, Route: rtmp.ConventionalRoute, Feed: func(core.Session) (*media.Router, error) { return f.live, nil }})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		cleanup, stop := context.WithTimeout(context.Background(), 4*time.Second)
		defer stop()
		if e := server.Shutdown(cleanup); e != nil {
			t.Error(e)
		}
		select {
		case serveErr := <-serveDone:
			if serveErr != nil && !errors.Is(serveErr, context.Canceled) && !errors.Is(serveErr, net.ErrClosed) {
				t.Errorf("ingest Serve: %v", serveErr)
			}
		case <-cleanup.Done():
			t.Error("ingest не присоединён")
		}
	})
	client := &gortmplib.Client{URL: &url.URL{Scheme: "rtmp", Host: l.Addr().String(), Path: "/" + string(f.stream.ID), Fragment: f.stream.PublicationKey}, Publish: true}
	if err = client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err = client.Write(&message.SetChunkSize{Value: 128}); err != nil {
		t.Fatal(err)
	}
	if err = client.Write(&message.DataAMF0{ChunkStreamID: 5, MessageStreamID: 1, Payload: amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: float64(init.Video.Info().Video.FrameRate.Numerator) / float64(init.Video.Info().Video.FrameRate.Denominator)}}}}); err != nil {
		t.Fatal(err)
	}
	if err = publisherWriteMessage(client.NetConn(), 9, 1, 0, append([]byte{0x90, 'a', 'v', '0', '1'}, init.Video.Headers()...)); err != nil {
		t.Fatal(err)
	}
	if err = publisherWriteMessage(client.NetConn(), 8, 1, 0, append([]byte{0xaf, 0}, init.Audio.Headers()...)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	producer, stop := context.WithCancel(ctx)
	go func() {
		defer close(done)
		defer client.Close()
		start := time.Now()
		for _, p := range packets {
			at := publisherNearestMS(p.Info().DTS, p.Config().Info().TimeBase) + 200
			delay := time.Until(start.Add(time.Duration(at-200) * time.Millisecond))
			timer := time.NewTimer(max(delay, 0))
			select {
			case <-producer.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			body := append([]byte{0xaf, 1}, p.Payload()...)
			typ := byte(8)
			if p.Config().Info().Track == media.TrackVideo {
				typ = 9
				flag := byte(0xa1)
				if p.Info().Keyframe {
					flag = 0x91
				}
				body = append([]byte{flag, 'a', 'v', '0', '1'}, p.Payload()...)
			}
			if e := publisherWriteMessage(client.NetConn(), typ, 1, uint32(at), body); e != nil {
				return
			}
		}
		<-producer.Done()
	}()
	t.Cleanup(func() {
		stop()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("источник не завершён")
		}
	})
	return client, stop, done
}

func publisherStructuralInput(t *testing.T, frames int) ([]media.Packet, fmp4.Init) {
	t.Helper()
	v, a := publisherConfigs(t, publisherProfile(core.CodecAV1, 2560, 1440), 1)
	var result []media.Packet
	var audio int64
	var inspector codec.Inspector
	for video := int64(0); video < int64(frames); video++ {
		p, e := media.NewPacket(media.PacketInfo{PTS: video, DTS: video, DurationTicks: 1, Keyframe: true}, v, publisherPayload(v, 20))
		if e != nil {
			t.Fatal(e)
		}
		if key, e := inspector.Inspect(p); e != nil || !key {
			t.Fatalf("неверная структурная video фикстура: %v", e)
		}
		result = append(result, p)
		for audio*60 < (video+1)*48000 {
			p, e = media.NewPacket(media.PacketInfo{PTS: audio, DTS: audio, DurationTicks: 1024}, a, publisherPayload(a, 0))
			if e != nil {
				t.Fatal(e)
			}
			result = append(result, p)
			audio += 1024
		}
	}
	return result, fmp4.Init{Video: v, Audio: a}
}

func publisherMediaCount(messages []publisherWireMessage) int {
	n := 0
	for _, m := range messages {
		if (m.typ == 8 && len(m.body) > 1 && m.body[1] == 1) || (m.typ == 9 && len(m.body) > 5 && m.body[1] == 1) || (m.typ == 9 && len(m.body) > 5 && m.body[0]&0x80 != 0 && m.body[0]&15 == 1) {
			n++
		}
	}
	return n
}

func TestPublisherIngestGroupsAndIndependentDestinations(t *testing.T) {
	profiles := []core.GroupProfile{publisherProfile(core.CodecAV1, 2560, 1440), publisherProfile(core.CodecH264, 2560, 1440), publisherProfile(core.CodecH264, 1920, 1080)}
	var receivers []*publisherReceiver
	var trusts []*tls.Config
	var endpoints []string
	for i := 0; i < 4; i++ {
		r, trust := newPublisherReceiver(t, i == 3)
		receivers = append(receivers, r)
		trusts = append(trusts, trust)
		scheme := "rtmp"
		if trust != nil {
			scheme = "rtmps"
		}
		endpoints = append(endpoints, scheme+"://"+r.listener.Addr().String()+"/live%2Fapp?token=synthetic")
	}
	f := newPublisherPipeline(t, profiles, endpoints, []string{"copy-key", "shared-one", "shared-two", "tls-key"}, []int{0, 1, 1, 2})
	var held sync.Map
	var players, transcodes atomic.Int32
	control := filepath.Join(t.TempDir(), "fail-worker")
	f.config.OpenFile = func(_ context.Context, m core.ArtifactManifest) (*os.File, error) {
		file, e := os.CreateTemp(t.TempDir(), "held")
		if e == nil {
			held.Store(file, m.Profile)
		}
		return file, e
	}
	f.config.Player = func(_ context.Context, _ string, file *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		p, ok := held.LoadAndDelete(file)
		if !ok {
			return ffmpeg.Command{}, core.ErrCorrupt
		}
		players.Add(1)
		return publisherControlledCommand(p.(core.GroupProfile), a, 10, "")
	}
	f.config.Transcode = func(_ context.Context, n ffmpeg.NVIDIAConfig, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		transcodes.Add(1)
		path := ""
		if n.Profile.Format.Video.Width == 1920 {
			path = control
		}
		return publisherControlledCommand(n.Profile, a, 20, path)
	}
	registry, err := group.NewRegistry(context.Background(), group.RegistryConfig{Core: f.service, Parent: f.parent, Observer: f.observer, Runtime: f.config, MaxGroups: 4, MaxReferences: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := registry.Close(context.Background()); e != nil {
			t.Error(e)
		}
	})
	refs := map[string]*group.Reference{}
	pubs := map[string]*publisher.Publisher{}
	for _, o := range f.outputs {
		ref, e := registry.Acquire(context.Background(), o.Snapshot, f.live)
		if e != nil {
			t.Fatal(e)
		}
		refs[o.Snapshot.Destination.Endpoint+o.Snapshot.Destination.StreamKey] = ref
		index := -1
		for i, endpoint := range endpoints {
			if endpoint == o.Snapshot.Destination.Endpoint {
				index = i
			}
		}
		if index < 0 {
			t.Fatal("неизвестный endpoint")
		}
		p := publisherActivate(t, f, o, ref.Router(), trusts[index])
		pubs[o.Snapshot.Destination.Endpoint] = p
	}
	groups := map[*group.Group]bool{}
	for _, ref := range refs {
		groups[ref.Group()] = true
	}
	if len(groups) != 3 {
		t.Fatalf("ожидались три shared группы: %d", len(groups))
	}
	if refs[endpoints[1]+"shared-one"].Group() != refs[endpoints[2]+"shared-two"].Group() {
		t.Fatal("два назначения H264-профиля не разделяют результат одного encoder")
	}
	waitSource := func(want group.Feed) {
		publisherWait(t, func() bool {
			for g := range groups {
				if !g.Status().Available || g.Status().Source != want {
					return false
				}
			}
			return true
		})
	}
	waitSource(group.Fallback)
	for _, r := range receivers {
		r.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= 8 })
	}
	if players.Load() != 3 || transcodes.Load() != 0 {
		t.Fatalf("до OBS players=%d transcodes=%d", players.Load(), transcodes.Load())
	}
	packets, init := publisherStructuralInput(t, 1200)
	_, stop, _ := publisherStartIngest(t, f, packets, init)
	defer stop()
	waitSource(group.Live)
	if transcodes.Load() != 2 {
		t.Fatalf("для двух H264-профилей запущено %d workers", transcodes.Load())
	}
	assertHealthy := func() {
		t.Helper()
		for i, r := range receivers {
			if r.accepted.Load() != 1 {
				t.Fatalf("здоровое назначение %d переподключено: %d", i, r.accepted.Load())
			}
		}
	}
	assertHealthy()
	if _, err = f.service.PauseSession(context.Background(), core.SessionCommand{RequestID: "pause-pipeline", SessionID: f.parent.SessionID}); err != nil {
		t.Fatal(err)
	}
	waitSource(group.Fallback)
	if _, err = f.service.ResumeSession(context.Background(), core.SessionCommand{RequestID: "resume-pipeline", SessionID: f.parent.SessionID}); err != nil {
		t.Fatal(err)
	}
	waitSource(group.Live)
	assertHealthy()
	stop()
	waitSource(group.Fallback)
	_, stop2, _ := publisherStartIngest(t, f, packets, init)
	defer stop2()
	waitSource(group.Live)
	assertHealthy()
	epochs := map[*group.Group]uint64{}
	for g := range groups {
		epochs[g] = g.Status().Epoch
	}
	_, stop3, _ := publisherStartIngest(t, f, packets, init)
	defer stop3()
	publisherWait(t, func() bool {
		for g := range groups {
			s := g.Status()
			if !s.Available || s.Source != group.Live || s.Epoch <= epochs[g] {
				return false
			}
		}
		return true
	})
	assertHealthy()
	if err = os.WriteFile(control, []byte("stop"), 0600); err != nil {
		t.Fatal(err)
	}
	failedGroup := refs[endpoints[3]+"tls-key"].Group()
	publisherWait(t, func() bool { s := failedGroup.Status(); return s.Source == group.Fallback && s.Available })
	for g := range groups {
		if g != failedGroup && (!g.Status().Available || g.Status().Source != group.Live) {
			t.Fatal("отказ 1080p worker изменил здоровую группу")
		}
	}
	assertHealthy()
	before := publisherMediaCount(receivers[0].snapshot())
	receivers[1].disconnect()
	receivers[1].wait(t, func(m []publisherWireMessage) bool {
		for _, v := range m {
			if v.connection >= 2 && v.typ == 8 && len(v.body) > 1 && v.body[1] == 1 {
				return true
			}
		}
		return false
	})
	receivers[0].wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) > before+4 })
	for _, i := range []int{0, 2, 3} {
		if receivers[i].accepted.Load() != 1 {
			t.Fatalf("reconnect изменил соседа %d", i)
		}
	}
	for i, r := range receivers {
		publisherAssertStructuralWire(t, r.snapshot(), profiles[[]int{0, 1, 1, 2}[i]])
		r.mu.Lock()
		for _, app := range r.apps {
			if app != "live%2Fapp?token=synthetic" {
				t.Errorf("escaped app изменён: %q", app)
			}
		}
		r.mu.Unlock()
	}
	for _, p := range pubs {
		if err = p.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		result, e := p.Wait(context.Background())
		if e != nil || !result.NetworkJoined {
			t.Fatalf("нет physical join: %+v %v", result, e)
		}
	}
}

func publisherAssertStructuralWire(t *testing.T, messages []publisherWireMessage, p core.GroupProfile) {
	t.Helper()
	v, a := publisherConfigs(t, p, 1)
	seenVideo, seenAudio, seenLive, seenFallback, seenMetadata := false, false, false, false, false
	last := map[string]uint32{}
	for _, m := range messages {
		if m.typ == 18 {
			values, e := publisherAMFValues(m.body)
			if e != nil || len(values) != 3 {
				t.Fatalf("неверная metadata: %v", e)
			}
			object, ok := values[2].(map[string]any)
			if !ok || object["width"] != float64(p.Format.Video.Width) || object["height"] != float64(p.Format.Video.Height) || object["framerate"] != float64(60) || object["audiosamplerate"] != float64(48000) {
				t.Fatalf("metadata не отражает проверенный формат: %+v", object)
			}
			seenMetadata = true
		}
		if m.typ != 8 && m.typ != 9 {
			continue
		}
		if m.stream != 1 {
			t.Fatalf("MessageStreamID не little-endian: %d", m.stream)
		}
		if m.typ == 8 {
			if len(m.body) < 2 || m.body[0] != 0xaf {
				t.Fatal("неверный AAC binding")
			}
			switch m.body[1] {
			case 0:
				if !bytes.Equal(m.body[2:], a.Headers()) {
					t.Fatal("ASC изменён")
				}
				seenAudio = true
			case 1:
				if !seenAudio || !bytes.Equal(m.body[2:], publisherPayload(a, 0)) {
					t.Fatal("AAC AU изменён или пришёл без ASC")
				}
			}
			continue
		}
		if len(m.body) < 5 {
			t.Fatal("неполное video body")
		}
		header := m.body[1] == 0
		payload := m.body[5:]
		if p.Format.Video.Codec == core.CodecAV1 {
			if !bytes.Equal(m.body[1:5], []byte("av01")) {
				t.Fatal("нет Enhanced av01")
			}
			header = m.body[0]&15 == 0
			if m.body[0]&15 != 0 && m.body[0]&15 != 1 {
				t.Fatal("неверный AV1 packet type")
			}
		} else if m.body[0] != 0x17 {
			t.Fatal("структурный независимый H264 AU не помечен keyframe")
		}
		if header {
			if !bytes.Equal(payload, v.Headers()) {
				t.Fatal("actual video config изменён")
			}
			seenVideo = true
			continue
		}
		if !seenVideo {
			t.Fatal("AU пришёл до sequence header")
		}
		if bytes.Equal(payload, publisherPayload(v, 10)) {
			seenFallback = true
		} else if bytes.Equal(payload, publisherPayload(v, 20)) {
			seenLive = true
		} else {
			t.Fatalf("video payload изменён: %x", payload)
		}
		key := fmt.Sprintf("%d/video", m.connection)
		if prev, ok := last[key]; ok && m.timestamp < prev {
			t.Fatalf("здоровый socket сбросил timeline: %d -> %d", prev, m.timestamp)
		}
		last[key] = m.timestamp
	}
	if !seenMetadata || !seenVideo || !seenAudio || !seenLive || !seenFallback {
		t.Fatalf("неполный выход: video=%v audio=%v live=%v fallback=%v", seenVideo, seenAudio, seenLive, seenFallback)
	}
}
