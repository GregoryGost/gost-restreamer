package publisher

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
)

type testExecution struct {
	mu                         sync.Mutex
	closed                     chan struct{}
	sealed, eligible, released bool
	sealBlocked                <-chan struct{}
	activateHook               func(context.Context, *core.OutputObserver) error
	activations, seals         int
}

func newTestExecution() *testExecution { return &testExecution{closed: make(chan struct{})} }
func (e *testExecution) activate(ctx context.Context, observer *core.OutputObserver) error {
	e.mu.Lock()
	e.activations++
	hook := e.activateHook
	e.mu.Unlock()
	if hook != nil {
		return hook(ctx, observer)
	}
	return nil
}
func (e *testExecution) admit(context.Context, rtmp.OutboundOperation) (func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sealed {
		return nil, core.ErrStale
	}
	return func() {}, nil
}
func (e *testExecution) seal() {
	if e.sealBlocked != nil {
		<-e.sealBlocked
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seals++
	if !e.sealed {
		e.sealed = true
		close(e.closed)
	}
}
func (e *testExecution) done() <-chan struct{} { return e.closed }
func (e *testExecution) release() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.eligible {
		return core.ErrNotReady
	}
	e.released = true
	return nil
}

type testObserver struct {
	mu     sync.Mutex
	values []core.OutputObservation
}

type testAttemptTimes struct {
	mu     sync.Mutex
	values []time.Time
}

func (a *testAttemptTimes) append() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.values = append(a.values, time.Now())
	return len(a.values)
}
func (a *testAttemptTimes) snapshot() []time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]time.Time(nil), a.values...)
}

func (o *testObserver) Observe(_ context.Context, v core.OutputObservation) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.values = append(o.values, v)
	return nil
}

type testConnection struct {
	mu             sync.Mutex
	done           chan struct{}
	err            error
	opts           rtmp.OutboundOptions
	headers        int
	packets        []media.Packet
	onHeaders      func() error
	beforeDelivery func()
	onPacket       func(context.Context, media.Packet) error
	join           <-chan struct{}
	waited         bool
}

func newTestConnection() *testConnection { return &testConnection{done: make(chan struct{})} }
func (c *testConnection) WriteHeaders(ctx context.Context, _, _ media.CodecConfig) error {
	finish, err := c.opts.Admit(ctx, rtmp.OutboundWrite)
	if err != nil {
		return err
	}
	defer finish()
	c.mu.Lock()
	c.headers++
	hook := c.onHeaders
	c.mu.Unlock()
	c.opts.OnWrite(10)
	if hook != nil {
		return hook()
	}
	return nil
}
func (c *testConnection) WriteDelivery(ctx context.Context, delivery media.Delivery) (rtmp.OutboundWriteResult, error) {
	if c.beforeDelivery != nil {
		c.beforeDelivery()
	}
	if !delivery.IsCurrent() {
		return rtmp.OutboundWriteResult{}, media.ErrStaleSource
	}
	p := delivery.Packet
	finish, err := c.opts.Admit(ctx, rtmp.OutboundWrite)
	if err != nil {
		return rtmp.OutboundWriteResult{}, err
	}
	defer finish()
	if c.onPacket != nil {
		if err := c.onPacket(ctx, p); err != nil {
			return rtmp.OutboundWriteResult{}, err
		}
	}
	c.mu.Lock()
	c.packets = append(c.packets, p)
	c.mu.Unlock()
	c.opts.OnWrite(len(p.Payload()) + 12)
	return rtmp.OutboundWriteResult{PayloadBytes: uint64(len(p.Payload()))}, nil
}
func (c *testConnection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	return nil
}
func (c *testConnection) Wait(ctx context.Context) error {
	if c.join != nil {
		select {
		case <-c.join:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	c.waited = true
	c.mu.Unlock()
	return nil
}
func (c *testConnection) Done() <-chan struct{}           { return c.done }
func (c *testConnection) Err() error                      { c.mu.Lock(); defer c.mu.Unlock(); return c.err }
func (c *testConnection) Snapshot() rtmp.OutboundSnapshot { return rtmp.OutboundSnapshot{} }

func testRouter(t *testing.T) (*media.Router, *media.Source, media.CodecConfig, media.CodecConfig) {
	t.Helper()
	r, err := media.NewRouter(media.RouterLimits{MaxConsumers: 8, MaxRetainedBytes: 65536, MaxConfigBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	s, err := r.BeginSource()
	if err != nil {
		t.Fatal(err)
	}
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: s.Epoch(), Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1000}, Video: core.VideoFormat{Codec: core.CodecH264, Width: 1920, Height: 1080, FrameRate: core.FrameRate{Numerator: 60, Denominator: 1}, CodecProfile: "high", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}}}, []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: s.Epoch(), Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1000}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, []byte{2})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateConfigs(v, a); err != nil {
		t.Fatal(err)
	}
	return r, s, v, a
}
func testPublisher(t *testing.T, r *media.Router, dial func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error)) (*Publisher, *testExecution) {
	t.Helper()
	e := newTestExecution()
	p := &Publisher{router: r, execution: e, done: make(chan struct{}), sealDone: make(chan struct{}), state: core.OutputStopped, observer: &testObserver{}, dial: dial,
		options: Options{Queue: media.QueueLimits{MaxPackets: 16, MaxBytes: 4096, MaxPacketBytes: 128, MaxConfigBytes: 64, ResyncTimeout: time.Second}, RetryInitial: 5 * time.Millisecond, RetryMax: 20 * time.Millisecond, ResubscribeDelay: 10 * time.Millisecond}}
	t.Cleanup(func() {
		if err := p.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return p, e
}
func startTestPublisher(p *Publisher) {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.active = cancel, true
	go p.run(ctx)
}
func publishPair(t *testing.T, s *media.Source, v, a media.CodecConfig, tick int64) {
	t.Helper()
	for _, c := range []media.CodecConfig{v, a} {
		p, err := media.NewPacket(media.PacketInfo{PTS: tick, DTS: tick, Keyframe: c.Info().Track == media.TrackVideo}, c, []byte{3, 4, 5})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Publish(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetryBackoffHasNoAttemptLimitAndSaturatesDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, _, _, _ := testRouter(t)
		var attempts testAttemptTimes
		p, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			attempts.append()
			o.OnWrite(7)
			return nil, errors.New("сеть недоступна")
		})
		startTestPublisher(p)
		synctest.Wait()
		for _, delay := range []time.Duration{5, 10, 20, 20, 20, 20} {
			time.Sleep(delay * time.Millisecond)
			synctest.Wait()
		}
		times := attempts.snapshot()
		if len(times) != 7 {
			t.Fatalf("попыток: %d", len(times))
		}
		for i, want := range []time.Duration{5, 10, 20, 20, 20, 20} {
			if got := times[i+1].Sub(times[i]); got != want*time.Millisecond {
				t.Fatalf("интервал %d: %v", i, got)
			}
		}
		s := p.Snapshot()
		if s.Attempts != 7 || s.Connections != 0 || s.WrittenBytes != 49 || s.State != core.OutputRetrying {
			t.Fatalf("снимок: %+v", s)
		}
		if nextDelay(time.Duration(math.MaxInt64-1), time.Duration(math.MaxInt64)) != time.Duration(math.MaxInt64) {
			t.Fatal("переполнение задержки")
		}
	})
}

func TestAbortDoesNotDialAndJoinRequiresExplicitResolution(t *testing.T) {
	r, _, _, _ := testRouter(t)
	p, e := testPublisher(t, r, func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error) {
		t.Error("сеть до Activate")
		return nil, core.ErrInvalid
	})
	if err := p.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !p.Result().NetworkJoined || p.Result().Err != nil {
		t.Fatalf("result: %+v", p.Result())
	}
	if err := p.ReleaseJoined(); !errors.Is(err, core.ErrNotReady) {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.eligible = true
	e.mu.Unlock()
	if err := p.ReleaseJoined(); err != nil {
		t.Fatal(err)
	}
	if err := p.ReleaseJoined(); err != nil {
		t.Fatal(err)
	}
}

func TestStopWaitsForLateDialAndLateTransportJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, _, _, _ := testRouter(t)
		late := make(chan struct{})
		join := make(chan struct{})
		c := newTestConnection()
		c.join = join
		p, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			<-late
			c.opts = o
			return c, nil
		})
		startTestPublisher(p)
		synctest.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		if err := p.Stop(ctx); !errors.Is(err, ErrCleanupPending) {
			t.Fatal(err)
		}
		if p.Result().NetworkJoined {
			t.Fatal("late dial ещё работает")
		}
		if err := p.ReleaseJoined(); !errors.Is(err, ErrCleanupPending) {
			t.Fatal(err)
		}
		close(late)
		synctest.Wait()
		select {
		case <-c.Done():
		default:
			t.Fatal("late connection не закрыто")
		}
		if p.Result().NetworkJoined {
			t.Fatal("Wait транспорта ещё работает")
		}
		close(join)
		synctest.Wait()
		result, err := p.Wait(context.Background())
		if err != nil || !result.NetworkJoined || result.Err != nil {
			t.Fatalf("%+v %v", result, err)
		}
	})
}

func TestPublishingRequiresActualVideoAndAudioAndNoPostSendDrop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, s, v, a := testRouter(t)
		audio := make(chan struct{})
		c := newTestConnection()
		c.onPacket = func(ctx context.Context, p media.Packet) error {
			if p.Config().Info().Track == media.TrackAudio {
				select {
				case <-audio:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}
		p, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			c.opts = o
			return c, nil
		})
		startTestPublisher(p)
		synctest.Wait()
		publishPair(t, s, v, a, 0)
		synctest.Wait()
		before := p.Snapshot()
		if before.AccessUnits != 1 || before.VideoFrames != 1 || before.State != core.OutputConnecting || before.InFlightPacketBytes == 0 {
			t.Fatalf("до AAC: %+v", before)
		}
		close(audio)
		synctest.Wait()
		after := p.Snapshot()
		if after.AccessUnits != 2 || after.PayloadBytes != 6 || after.State != core.OutputPublishing || after.LastMedia.IsZero() || after.LastWrite.IsZero() {
			t.Fatalf("после AAC: %+v", after)
		}
		if err := p.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := p.Snapshot().DeliveryDrops; got != (DeliveryDrops{}) {
			t.Fatalf("успешная AU потеряна позднее: %+v", got)
		}
	})
}

func TestRecoveryRepeatsHeadersWithoutReplacingHealthyConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, s, v, a := testRouter(t)
		c := newTestConnection()
		dials := 0
		p, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			dials++
			c.opts = o
			return c, nil
		})
		startTestPublisher(p)
		synctest.Wait()
		publishPair(t, s, v, a, 0)
		synctest.Wait()
		s2, err := r.ReplaceSource()
		if err != nil {
			t.Fatal(err)
		}
		vi, ai := v.Info(), a.Info()
		vi.SourceEpoch, ai.SourceEpoch = s2.Epoch(), s2.Epoch()
		v2, err := media.NewCodecConfig(vi, []byte{1})
		if err != nil {
			t.Fatal(err)
		}
		a2, err := media.NewCodecConfig(ai, []byte{2})
		if err != nil {
			t.Fatal(err)
		}
		if err := s2.UpdateConfigs(v2, a2); err != nil {
			t.Fatal(err)
		}
		publishPair(t, s2, v2, a2, 100)
		synctest.Wait()
		c.mu.Lock()
		headers := c.headers
		c.mu.Unlock()
		if dials != 1 || headers != 2 || p.Snapshot().AccessUnits != 4 {
			t.Fatalf("dials=%d headers=%d snapshot=%+v", dials, headers, p.Snapshot())
		}
		// Пустая подписка при восстановлении истекает, сохраняя соединение.
		if err := s2.Disconnect(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		if dials != 1 || p.Result().NetworkJoined {
			t.Fatal("resync закрыл исправное соединение")
		}
	})
}

func TestQueueDropTransferAndCounterSaturation(t *testing.T) {
	r, s, v, a := testRouter(t)
	p, _ := testPublisher(t, r, nil)
	sub, err := r.Subscribe(p.options.Queue)
	if err != nil {
		t.Fatal(err)
	}
	p.attachSubscription(sub)
	publishPair(t, s, v, a, 0)
	if p.Snapshot().QueuedPackets != 2 {
		t.Fatal("нет очереди")
	}
	p.closeSubscription()
	first := p.Snapshot()
	p.closeSubscription()
	second := p.Snapshot()
	if first.QueueDrops.Closed != 2 || second.QueueDrops.Closed != 2 || second.PacketBytes != 0 {
		t.Fatalf("%+v %+v", first, second)
	}
	p.writeMu.Lock()
	p.writtenBytes = math.MaxUint64 - 1
	p.writeMu.Unlock()
	p.noteWrite(3)
	if got := p.Snapshot(); got.WrittenBytes != math.MaxUint64 || !got.Saturated {
		t.Fatalf("%+v", got)
	}
	var n uint64 = math.MaxUint64
	sticky := false
	saturate(&n, 1, &sticky)
	saturate(&n, 0, &sticky)
	if !sticky || n != math.MaxUint64 {
		t.Fatal("насыщение не сохранилось")
	}
}

func TestAuthorityRevokeJoinsAndPreservesTerminalReason(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, _, _, _ := testRouter(t)
		c := newTestConnection()
		p, e := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			c.opts = o
			return c, nil
		})
		startTestPublisher(p)
		synctest.Wait()
		e.seal()
		synctest.Wait()
		result, err := p.Wait(context.Background())
		if err != nil || !result.NetworkJoined || !errors.Is(result.Err, core.ErrStale) {
			t.Fatalf("%+v %v", result, err)
		}
		c.mu.Lock()
		waited := c.waited
		c.mu.Unlock()
		if !waited {
			t.Fatal("transport join пропущен")
		}
	})
}

func TestPartialTransportFailureCountsBytesAndAbandonsOnlyOwnedAU(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, source, video, audio := testRouter(t)
		c := newTestConnection()
		c.onPacket = func(context.Context, media.Packet) error {
			c.opts.OnWrite(2)
			return errors.New("частичная запись")
		}
		p, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			c.opts = o
			return c, nil
		})
		startTestPublisher(p)
		synctest.Wait()
		publishPair(t, source, video, audio, 0)
		synctest.Wait()
		s := p.Snapshot()
		if s.WrittenBytes != 12 || s.PayloadBytes != 0 || s.AccessUnits != 0 || s.DeliveryDrops.Transport != 1 || s.QueueDrops.Closed != 1 || s.State != core.OutputRetrying {
			t.Fatalf("снимок после частичной записи: %+v", s)
		}
		if err := p.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := p.Snapshot().DeliveryDrops; got.Transport != 1 || got.Cancelled != 0 {
			t.Fatalf("повторный учёт: %+v", got)
		}
	})
}

func TestBackoffResetsOnlyAfterActualInitialAVWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, source, video, audio := testRouter(t)
		first, second := newTestConnection(), newTestConnection()
		var attempts testAttemptTimes
		p, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			switch attempts.append() {
			case 3:
				first.opts = o
				return first, nil
			case 4:
				second.opts = o
				return second, nil
			default:
				return nil, errors.New("сеть недоступна")
			}
		})
		startTestPublisher(p)
		synctest.Wait()
		time.Sleep(5 * time.Millisecond)
		synctest.Wait()
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		first.mu.Lock()
		first.err = errors.New("соединение разорвано до медиаданных")
		first.mu.Unlock()
		_ = first.Close()
		synctest.Wait()
		time.Sleep(19 * time.Millisecond)
		synctest.Wait()
		if len(attempts.snapshot()) != 3 {
			t.Fatal("publish handshake сбросил backoff")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		publishPair(t, source, video, audio, 0)
		synctest.Wait()
		if p.Snapshot().AccessUnits != 2 {
			t.Fatal("начальная пара не записана")
		}
		second.mu.Lock()
		second.err = errors.New("соединение разорвано после медиаданных")
		second.mu.Unlock()
		_ = second.Close()
		synctest.Wait()
		time.Sleep(5 * time.Millisecond)
		synctest.Wait()
		times := attempts.snapshot()
		if len(times) != 5 || times[4].Sub(times[3]) != 5*time.Millisecond {
			t.Fatalf("backoff не сброшен после A/V: %v", times)
		}
		if s := p.Snapshot(); s.Connections != 2 || s.Reconnects != 1 {
			t.Fatalf("publish-соединения: %+v", s)
		}
	})
}

func TestSlowAndFailedPublishersDoNotBlockHealthyOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, source, video, audio := testRouter(t)
		slowConn, healthyConn := newTestConnection(), newTestConnection()
		slowConn.onPacket = func(ctx context.Context, _ media.Packet) error { <-ctx.Done(); return ctx.Err() }
		slow, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			slowConn.opts = o
			return slowConn, nil
		})
		healthy, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			healthyConn.opts = o
			return healthyConn, nil
		})
		startTestPublisher(slow)
		startTestPublisher(healthy)
		synctest.Wait()
		publishPair(t, source, video, audio, 0)
		synctest.Wait()
		if got := healthy.Snapshot(); got.AccessUnits != 2 || got.State != core.OutputPublishing {
			t.Fatalf("здоровый выход заблокирован: %+v", got)
		}
		if err := slow.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		publishPair(t, source, video, audio, 1)
		synctest.Wait()
		if got := healthy.Snapshot(); got.AccessUnits != 4 || got.Connections != 1 {
			t.Fatalf("остановка соседа: %+v", got)
		}
		if got := slow.Snapshot(); got.DeliveryDrops.Cancelled != 1 || got.QueueDrops.Closed != 1 {
			t.Fatalf("потери остановленного выхода: %+v", got)
		}
	})
}

func TestStaleDeliveryAfterWriterWaitRecoversOnSameSocket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, source, video, audio := testRouter(t)
		c := newTestConnection()
		once := false
		c.beforeDelivery = func() {
			if once {
				return
			}
			once = true
			vi := video.Info()
			vi.Revision++
			freshVideo, err := media.NewCodecConfig(vi, []byte{9})
			if err != nil {
				t.Error(err)
				return
			}
			if err := source.UpdateConfigs(freshVideo, audio); err != nil {
				t.Error(err)
				return
			}
			publishPair(t, source, freshVideo, audio, 1)
		}
		p, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			c.opts = o
			return c, nil
		})
		startTestPublisher(p)
		synctest.Wait()
		publishPair(t, source, video, audio, 0)
		synctest.Wait()
		s := p.Snapshot()
		if s.AccessUnits != 2 || s.Connections != 1 || s.DeliveryDrops.Stale != 1 || s.DeliveryDrops.Transport != 0 || s.QueueDrops.ConfigReset != 1 || s.State != core.OutputPublishing {
			t.Fatalf("recovery: %+v", s)
		}
		c.mu.Lock()
		headers := c.headers
		c.mu.Unlock()
		if headers != 2 {
			t.Fatalf("headers: %d", headers)
		}
	})
}

func TestTerminalMediaFailureLeavesHealthyPublisherRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, source, video, audio := testRouter(t)
		badConn, healthyConn := newTestConnection(), newTestConnection()
		badConn.onPacket = func(context.Context, media.Packet) error { return &rtmp.OutboundError{Kind: rtmp.OutboundTiming} }
		bad, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			badConn.opts = o
			return badConn, nil
		})
		healthy, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			healthyConn.opts = o
			return healthyConn, nil
		})
		startTestPublisher(bad)
		startTestPublisher(healthy)
		synctest.Wait()
		publishPair(t, source, video, audio, 0)
		synctest.Wait()
		result, err := bad.Wait(context.Background())
		if err != nil || !rtmp.IsOutboundTerminal(result.Err) || !result.NetworkJoined {
			t.Fatalf("terminal: %+v %v", result, err)
		}
		publishPair(t, source, video, audio, 1)
		synctest.Wait()
		if s := healthy.Snapshot(); s.AccessUnits != 4 || s.Connections != 1 || s.NetworkJoined {
			t.Fatalf("здоровый сосед: %+v", s)
		}
		if s := bad.Snapshot(); s.DeliveryDrops.Terminal != 1 || s.QueueDrops.Closed != 1 || s.Attempts != 1 || s.State != core.OutputFailed {
			t.Fatalf("окончательный отказ: %+v", s)
		}
	})
}

func TestStopDeadlineDoesNotWaitForBlockedAuthoritySeal(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepared", true: "active"}[active], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r, _, _, _ := testRouter(t)
				c := newTestConnection()
				p, e := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
					c.opts = o
					return c, nil
				})
				blocked := make(chan struct{})
				e.sealBlocked = blocked
				if active {
					startTestPublisher(p)
					synctest.Wait()
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				defer cancel()
				if err := p.Stop(ctx); !errors.Is(err, ErrCleanupPending) {
					t.Fatal(err)
				}
				if p.Result().NetworkJoined {
					t.Fatal("seal ещё работает")
				}
				if err := p.Stop(ctx); !errors.Is(err, ErrCleanupPending) {
					t.Fatal(err)
				}
				if active {
					select {
					case <-c.Done():
					default:
						t.Fatal("authority gate задержал отмену сети")
					}
				}
				close(blocked)
				synctest.Wait()
				if result, err := p.Wait(context.Background()); err != nil || !result.NetworkJoined || result.Err != nil {
					t.Fatalf("%+v %v", result, err)
				}
			})
		})
	}
}

func TestStopCancelsPendingActivationAndTracksItsCompletion(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled_gate", true: "late_success"}[late], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r, _, _, _ := testRouter(t)
				p, e := testPublisher(t, r, func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error) {
					t.Error("сеть после остановки незавершённой активации")
					return nil, core.ErrInvalid
				})
				entered, cancelled := make(chan struct{}), make(chan struct{})
				activationGate, sealGate := make(chan struct{}), make(chan struct{})
				var activationOnce, sealOnce sync.Once
				releaseActivation := func() { activationOnce.Do(func() { close(activationGate) }) }
				releaseSeal := func() { sealOnce.Do(func() { close(sealGate) }) }
				t.Cleanup(func() { releaseActivation(); releaseSeal() })
				e.sealBlocked = sealGate
				e.activateHook = func(ctx context.Context, _ *core.OutputObserver) error {
					close(entered)
					<-ctx.Done()
					close(cancelled)
					if late {
						<-activationGate
						return nil
					}
					return ctx.Err()
				}
				activationResult := make(chan error, 1)
				go func() { activationResult <- p.Activate(context.Background(), &core.OutputObserver{}) }()
				<-entered
				if err := p.Activate(context.Background(), &core.OutputObserver{}); !errors.Is(err, core.ErrConflict) {
					t.Fatalf("повторная активация: %v", err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				defer cancel()
				if err := p.Stop(ctx); !errors.Is(err, ErrCleanupPending) || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("остановка: %v", err)
				}
				<-cancelled
				if p.Result().NetworkJoined {
					t.Fatal("активация или Seal ещё не завершены")
				}
				if err := p.ReleaseJoined(); !errors.Is(err, ErrCleanupPending) {
					t.Fatal(err)
				}
				if err := p.Stop(ctx); !errors.Is(err, ErrCleanupPending) {
					t.Fatal(err)
				}
				if late {
					// Завершение Seal отдельно не доказывает возврат поздней Activate.
					releaseSeal()
					synctest.Wait()
					if p.Result().NetworkJoined {
						t.Fatal("поздняя Activate ещё работает")
					}
					releaseActivation()
				}
				if err := <-activationResult; !errors.Is(err, context.Canceled) {
					t.Fatalf("активация после Stop: %v", err)
				}
				if !late {
					if p.Result().NetworkJoined {
						t.Fatal("Seal ещё работает")
					}
					releaseSeal()
				}
				synctest.Wait()
				for range 2 {
					if result, err := p.Wait(context.Background()); err != nil || !result.NetworkJoined || result.Err != nil {
						t.Fatalf("завершение: %+v %v", result, err)
					}
					if err := p.Stop(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				if err := p.Activate(context.Background(), &core.OutputObserver{}); !errors.Is(err, core.ErrConflict) {
					t.Fatal(err)
				}
				e.mu.Lock()
				defer e.mu.Unlock()
				if e.activations != 1 || e.seals != 1 || e.released {
					t.Fatalf("активации=%d Seal=%d release=%v", e.activations, e.seals, e.released)
				}
			})
		})
	}
}

func TestFailedActivationCanRetryWithoutReleasingCallerOwnership(t *testing.T) {
	r, _, _, _ := testRouter(t)
	p, e := testPublisher(t, r, func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error) {
		t.Error("сеть после ошибки активации")
		return nil, core.ErrInvalid
	})
	e.activateHook = func(context.Context, *core.OutputObserver) error { return core.ErrNotReady }
	for range 2 {
		if err := p.Activate(context.Background(), &core.OutputObserver{}); !errors.Is(err, core.ErrNotReady) {
			t.Fatal(err)
		}
		if p.Result().NetworkJoined {
			t.Fatal("ошибка активации не заменяет Abort вызывающего кода")
		}
	}
	e.mu.Lock()
	if e.activations != 2 || e.sealed || e.released {
		t.Errorf("активации=%d sealed=%v released=%v", e.activations, e.sealed, e.released)
	}
	e.mu.Unlock()
	if err := p.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.ReleaseJoined(); !errors.Is(err, core.ErrNotReady) {
		t.Fatal(err)
	}
}

func TestCancelledSuccessfulActivationNeverStartsNetwork(t *testing.T) {
	r, _, _, _ := testRouter(t)
	p, e := testPublisher(t, r, func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error) {
		t.Error("сеть после отмены активации")
		return nil, core.ErrInvalid
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.activateHook = func(context.Context, *core.OutputObserver) error {
		cancel()
		return nil
	}
	if err := p.Activate(ctx, &core.OutputObserver{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if result, err := p.Wait(context.Background()); err != nil || !result.NetworkJoined {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestConcurrentActivationHasOneAdmissionAfterFailedAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, _, _, _ := testRouter(t)
		p, e := testPublisher(t, r, func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error) {
			t.Error("недействительный тестовый observer не допускает сеть")
			return nil, core.ErrInvalid
		})
		e.activateHook = func(context.Context, *core.OutputObserver) error { return core.ErrNotReady }
		if err := p.Activate(context.Background(), &core.OutputObserver{}); !errors.Is(err, core.ErrNotReady) {
			t.Fatal(err)
		}
		entered, gate := make(chan struct{}), make(chan struct{})
		var once sync.Once
		release := func() { once.Do(func() { close(gate) }) }
		t.Cleanup(release)
		observer := &core.OutputObserver{}
		e.activateHook = func(_ context.Context, got *core.OutputObserver) error {
			if got != observer {
				t.Error("observer был подменён")
			}
			close(entered)
			<-gate
			return nil
		}
		result := make(chan error, 1)
		go func() { result <- p.Activate(context.Background(), observer) }()
		<-entered
		for range 2 {
			if err := p.Activate(context.Background(), observer); !errors.Is(err, core.ErrConflict) {
				t.Fatal(err)
			}
		}
		release()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if err := p.Activate(context.Background(), observer); !errors.Is(err, core.ErrConflict) {
			t.Fatal(err)
		}
		if err := p.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.activations != 2 || e.seals != 1 || e.released {
			t.Fatalf("активации=%d Seal=%d release=%v", e.activations, e.seals, e.released)
		}
	})
}
