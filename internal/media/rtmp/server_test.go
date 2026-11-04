package rtmp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/message"
)

type ingestFixture struct {
	store   *storage.Store
	svc     *core.Service
	stream  core.Stream
	session core.CommandReceipt
	router  *media.Router
}

func newIngestFixture(t *testing.T) ingestFixture {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "ingest.sqlite"), storage.Options{PreparationLimits: core.PreparationLimits{MaxQueued: 8, MaxProfiles: 64}, ObservationLimits: core.ObservationLimits{MaxSessions: 16, MaxGroups: 64, MaxOutputs: 128, MaxFFmpegWorkers: 64, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond})
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
	asset, err := svc.CreateAsset(ctx, "Оригинал")
	if err != nil {
		t.Fatal(err)
	}
	generation, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	preparation, err := svc.ClaimPreparation(ctx, generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkPreparationIssued(ctx, preparation); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.PublishPreparation(ctx, preparation, core.GenerationPublication{SourceHash: "synthetic-content", SizeBytes: 10, SourceFormat: core.SourceFormat{ImageFormat: "png"}}); err != nil {
		t.Fatal(err)
	}
	if err = svc.AcknowledgePreparationCleanup(ctx, preparation); err != nil {
		t.Fatal(err)
	}
	stream, err := svc.CreateStream(ctx, core.Stream{Name: "Вход", PublicationKey: "synthetic-ingest-key", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	start, err := svc.StartSession(ctx, core.StartSessionRequest{RequestID: "start-ingest", StreamID: stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	complete(t, svc, start)
	router, err := media.NewRouter(media.RouterLimits{MaxConsumers: 8, MaxRetainedBytes: 16 << 20, MaxConfigBytes: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Close() })
	return ingestFixture{store, svc, stream, start, router}
}

func complete(t *testing.T, svc *core.Service, r core.CommandReceipt) {
	t.Helper()
	ctx := context.Background()
	f, err := svc.Claim(ctx, r.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkIssued(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err = svc.Complete(ctx, f, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
}

func newSubscription(t *testing.T, r *media.Router) *media.Subscription {
	t.Helper()
	s, err := r.Subscribe(media.QueueLimits{MaxPackets: 30, MaxBytes: 1 << 20, MaxPacketBytes: 1 << 18, MaxConfigBytes: 1 << 16, ResyncTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func runServer(t *testing.T, f ingestFixture, feed FeedResolver) (*Server, string) {
	t.Helper()
	if feed == nil {
		feed = func(session core.Session) (*media.Router, error) {
			if session.ID != f.session.SessionID {
				return nil, core.ErrStale
			}
			return f.router, nil
		}
	}
	s, err := NewServer(Options{Limits: testLimits(), Core: f.svc, Route: ConventionalRoute, Feed: feed})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		if err := s.Shutdown(stopCtx); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
				t.Error(err)
			}
		case <-stopCtx.Done():
			t.Error("Serve не завершился")
		}
	})
	return s, l.Addr().String()
}

func connectClient(t *testing.T, address string, stream core.Stream, key string) (*gortmplib.Client, error) {
	t.Helper()
	u := &url.URL{Scheme: "rtmp", Host: address, Path: "/" + string(stream.ID), Fragment: key}
	c := &gortmplib.Client{URL: u, Publish: true}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Initialize(ctx); err != nil {
		return nil, err
	}
	t.Cleanup(c.Close)
	if err := c.NetConn().SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return c, nil
}

func writeWire(t *testing.T, c *gortmplib.Client, b []byte) {
	t.Helper()
	for len(b) > 0 {
		n, err := c.NetConn().Write(b)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatal(io.ErrShortWrite)
		}
		b = b[n:]
	}
}

func sendReady(t *testing.T, c *gortmplib.Client, at uint32) {
	t.Helper()
	if err := c.Write(&message.DataAMF0{ChunkStreamID: 5, MessageStreamID: 1, Payload: amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: float64(30)}}}}); err != nil {
		t.Fatal(err)
	}
	writeWire(t, c, chunk0(6, 9, append([]byte{0x90, 'a', 'v', '0', '1'}, testAV1Config()...)))
	writeWire(t, c, chunk0(4, 8, []byte{0xaf, 0, 0x11, 0x90}))
	sendVideo(t, c, at, testAV1TU())
	sendAudio(t, c, at, []byte{1, 2, 3})
}

func sendVideo(t *testing.T, c *gortmplib.Client, at uint32, tu []byte) {
	t.Helper()
	b := chunk0(6, 9, append([]byte{0x91, 'a', 'v', '0', '1'}, tu...))
	b[1] = byte(at >> 16)
	b[2] = byte(at >> 8)
	b[3] = byte(at)
	writeWire(t, c, b)
}
func sendAudio(t *testing.T, c *gortmplib.Client, at uint32, au []byte) {
	t.Helper()
	b := chunk0(4, 8, append([]byte{0xaf, 1}, au...))
	b[1] = byte(at >> 16)
	b[2] = byte(at >> 8)
	b[3] = byte(at)
	writeWire(t, c, b)
}
func delivery(t *testing.T, s *media.Subscription) media.Delivery {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func serverResult(t *testing.T, s *Server) error {
	t.Helper()
	select {
	case r := <-s.Results():
		return r.Err
	case <-time.After(2 * time.Second):
		t.Fatal("нет результата соединения")
		return nil
	}
}

func TestLocalEnhancedRTMPUsesCoreAuthorizationAndExactCodecHeaders(t *testing.T) {
	f := newIngestFixture(t)
	s, address := runServer(t, f, nil)
	sub := newSubscription(t, f.router)
	c, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	sendReady(t, c, 100)
	v, a := delivery(t, sub), delivery(t, sub)
	if v.Packet.Config().Info().Track != media.TrackVideo || a.Packet.Config().Info().Track != media.TrackAudio || !v.Packet.Info().Keyframe || v.Packet.Info().PTS != 4800 || v.Packet.Info().DTS != 4800 || v.Packet.Info().DurationTicks != 1600 || a.Packet.Info().DurationTicks != 1024 || !v.IsCurrent() {
		t.Fatal("потеряны дорожки, независимость или метка TU")
	}
	if !bytes.Equal(v.VideoConfig.Headers(), testAV1Config()) || !bytes.Equal(v.AudioConfig.Headers(), []byte{0x11, 0x90}) {
		t.Fatal("потеряны исходные codec headers")
	}
	// Пакеты действующего входа принимаются fMP4 без подмены шкалы или длительностей.
	var encoded bytes.Buffer
	fl := fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 1 << 16, MaxMdatBytes: 1 << 20, MaxSampleBytes: 1 << 18, MaxExtradataBytes: 1 << 16, MaxSamples: 20}
	mux, err := fmp4.NewMuxer(&encoded, fmp4.Init{Video: v.VideoConfig, Audio: v.AudioConfig}, fl)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []media.Packet{v.Packet, a.Packet} {
		if err = mux.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	demux, err := fmp4.NewDemuxer(bytes.NewReader(encoded.Bytes()), fmp4.DemuxOptions{Limits: fl})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []media.Packet{v.Packet, a.Packet} {
		got, e := demux.Next()
		if e != nil || got.Info() != want.Info() || got.Config().Info() != want.Config().Info() || !bytes.Equal(got.Config().Headers(), want.Config().Headers()) {
			t.Fatalf("fMP4 изменил ingress: %+v, %v", got.Info(), e)
		}
	}
	if _, err = connectClient(t, address, f.stream, "wrong-synthetic-key"); err == nil {
		t.Fatal("неверный ключ принят")
	}
	if err = serverResult(t, s); err == nil {
		t.Fatal("нет отказа core")
	}
	if !v.IsCurrent() {
		t.Fatal("неавторизованное соединение заменило источник")
	}
	if _, err = f.svc.PauseSession(context.Background(), core.SessionCommand{RequestID: "pause-ingest", SessionID: f.session.SessionID}); err != nil {
		t.Fatal(err)
	}
	sendVideo(t, c, 133, []byte{0x1a, 2, 0x30, 0})
	sendAudio(t, c, 133, []byte{4, 5, 6})
	if got := delivery(t, sub); got.Packet.Info().DTS != 6384 || got.Packet.Info().Keyframe {
		t.Fatal("пауза повредила OBS либо inter выдан за key")
	}
	_ = delivery(t, sub)
	if _, err = f.svc.StopSession(context.Background(), core.SessionCommand{RequestID: "stop-ingest", SessionID: f.session.SessionID}); err != nil {
		t.Fatal(err)
	}
	if v.IsCurrent() {
		t.Fatal("StopSession не отозвал feed до возврата")
	}
	if _, err = c.Read(); err == nil {
		t.Fatal("отозванное соединение осталось открытым")
	}
	_ = serverResult(t, s)
}

func TestAuthorizedOverrideInvalidatesOldCallbacksAndConnection(t *testing.T) {
	f := newIngestFixture(t)
	s, address := runServer(t, f, nil)
	sub := newSubscription(t, f.router)
	old, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	sendReady(t, old, 0)
	before := delivery(t, sub)
	_ = delivery(t, sub)
	fresh, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	sendReady(t, fresh, 0)
	after := delivery(t, sub)
	_ = delivery(t, sub)
	if before.IsCurrent() || !after.IsCurrent() || before.VideoConfig.Info().SourceEpoch == after.VideoConfig.Info().SourceEpoch {
		t.Fatal("Override не заменил identity")
	}
	if _, err = old.Read(); err == nil {
		t.Fatal("старое соединение не закрыто")
	}
	_ = serverResult(t, s)
	if !after.IsCurrent() {
		t.Fatal("поздний disconnect прежней lease очистил новый Source")
	}
	// Смена заголовков разрывает непрерывность декодирования при прежнем допуске публикации.
	sendReady(t, fresh, 0)
	next := delivery(t, sub)
	_ = delivery(t, sub)
	if after.IsCurrent() || !next.IsCurrent() || after.VideoConfig.Info().SourceEpoch == next.VideoConfig.Info().SourceEpoch {
		t.Fatal("смена headers сохранила несовместимую очередь")
	}
	if _, err = f.svc.StopSession(context.Background(), core.SessionCommand{RequestID: "stop-rotated", SessionID: f.session.SessionID}); err != nil {
		t.Fatal(err)
	}
	if next.IsCurrent() {
		t.Fatal("revoke не отозвал последний Source lease")
	}
}

type failPublishConn struct {
	net.Conn
	fail atomic.Bool
}

func (c *failPublishConn) Write(b []byte) (int, error) {
	if c.fail.Load() && bytes.Contains(b, []byte("NetStream.Publish.Start")) {
		return 0, io.ErrClosedPipe
	}
	return c.Conn.Write(b)
}

func TestFailedNetworkAcceptDoesNotReplaceSource(t *testing.T) {
	f := newIngestFixture(t)
	_, address := runServer(t, f, nil)
	sub := newSubscription(t, f.router)
	old, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	sendReady(t, old, 0)
	current := delivery(t, sub)
	_ = delivery(t, sub)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	var failing *failPublishConn
	s, err := NewServer(Options{Limits: testLimits(), Core: f.svc, Route: ConventionalRoute, Feed: func(core.Session) (*media.Router, error) { failing.fail.Store(true); return f.router, nil }})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		failing = &failPublishConn{Conn: conn}
		done <- s.Handle(context.Background(), failing)
	}()
	if _, err = connectClient(t, l.Addr().String(), f.stream, f.stream.PublicationKey); err == nil {
		t.Fatal("неуспешный Accept завершился успехом")
	}
	select {
	case err = <-done:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Handle не завершился")
	}
	if !current.IsCurrent() {
		t.Fatal("неуспешный Accept отозвал прежнюю публикацию")
	}
}

func TestAuthorizationCannotAttachNextSessionAcrossAcceptWindow(t *testing.T) {
	f := newIngestFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var called atomic.Int32
	defer close(release)
	s, address := runServer(t, f, func(session core.Session) (*media.Router, error) {
		if called.Add(1) == 1 {
			close(entered)
			<-release
		}
		return f.router, nil
	})
	u := &url.URL{Scheme: "rtmp", Host: address, Path: "/" + string(f.stream.ID), Fragment: f.stream.PublicationKey}
	old := &gortmplib.Client{URL: u, Publish: true}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	initialized := make(chan error, 1)
	go func() { initialized <- old.Initialize(ctx) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("нет первоначальной авторизации")
	}
	stop, err := f.svc.StopSession(ctx, core.SessionCommand{RequestID: "stop-first", SessionID: f.session.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	complete(t, f.svc, stop)
	var cleanups []core.CleanupIntent
	if err = f.store.View(ctx, func(r core.Repositories) error {
		var e error
		cleanups, e = r.Artifacts.Cleanups(ctx, core.ListQuery{Limit: 100, SessionID: f.session.SessionID, PendingOnly: true})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range cleanups {
		if err = f.svc.AcknowledgeCleanup(ctx, core.CleanupFenceFor(c)); err != nil {
			t.Fatal(err)
		}
	}
	next, err := f.svc.StartSession(ctx, core.StartSessionRequest{RequestID: "start-next", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	complete(t, f.svc, next)
	fresh, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	sub := newSubscription(t, f.router)
	sendReady(t, fresh, 0)
	d := delivery(t, sub)
	_ = delivery(t, sub)
	release <- struct{}{}
	select {
	case err = <-initialized:
		if err == nil {
			old.Close()
		}
	case <-ctx.Done():
		t.Fatal("Accept первой Session не завершился")
	}
	if err = serverResult(t, s); !errors.Is(err, core.ErrStale) {
		t.Fatalf("ожидался stale fence первой Session: %v", err)
	}
	if !d.IsCurrent() {
		t.Fatal("авторизация первой Session заменила feed второй")
	}
}

type observedReadConn struct {
	net.Conn
	entered chan struct{}
	once    atomic.Bool
}

func (c *observedReadConn) Read(b []byte) (int, error) {
	if !c.once.Swap(true) {
		close(c.entered)
	}
	return c.Conn.Read(b)
}

func TestConnectionBudgetCancellationAndBoundedResultQueue(t *testing.T) {
	f := newIngestFixture(t)
	limits := testLimits()
	limits.MaxConnections = 1
	s, err := NewServer(Options{Limits: limits, Core: f.svc, Route: ConventionalRoute, Feed: func(core.Session) (*media.Router, error) { return f.router, nil }})
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	defer func() { _ = b.Close() }()
	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Handle(ctx, &observedReadConn{Conn: a, entered: entered}) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("нет начального чтения")
	}
	a2, b2 := net.Pipe()
	defer func() { _ = b2.Close() }()
	if err = s.Handle(context.Background(), a2); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("отменённый handshake завершился успехом")
		}
	case <-time.After(time.Second):
		t.Fatal("отмена не присоединила handshake")
	}
	for range 3 {
		a3, b3 := net.Pipe()
		done3 := make(chan error, 1)
		go func() { done3 <- s.Handle(context.Background(), a3) }()
		if _, err = b3.Write([]byte{0}); err != nil {
			t.Fatal(err)
		}
		_ = b3.Close()
		select {
		case err = <-done3:
			if !errors.Is(err, ErrFraming) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("ошибочный handshake не завершился")
		}
	}
	if s.DroppedResults() != 3 {
		t.Fatalf("не видны потери полных результатов: %d", s.DroppedResults())
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err = s.Shutdown(stopCtx); err != nil {
		t.Fatal(err)
	}
	a4, b4 := net.Pipe()
	defer func() { _ = b4.Close() }()
	if err = s.Handle(context.Background(), a4); !errors.Is(err, core.ErrClosed) {
		t.Fatal(err)
	}
}

func TestHandshakeDeadlineTerminatesIdlePeer(t *testing.T) {
	f := newIngestFixture(t)
	limits := testLimits()
	limits.HandshakeTimeout = 20 * time.Millisecond
	s, err := NewServer(Options{Limits: limits, Core: f.svc, Route: ConventionalRoute, Feed: func(core.Session) (*media.Router, error) { return f.router, nil }})
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	defer func() { _ = b.Close() }()
	if err = s.Handle(context.Background(), a); err == nil {
		t.Fatal("idle handshake не завершился")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
