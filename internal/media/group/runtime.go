package group

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

type FileOpener func(context.Context, core.ArtifactManifest) (*os.File, error)
type PlayerBuilder func(context.Context, string, *os.File, ffmpeg.Attempt) (ffmpeg.Command, error)
type TranscodeBuilder func(context.Context, ffmpeg.NVIDIAConfig, ffmpeg.Attempt) (ffmpeg.Command, error)
type RuntimeConfig struct {
	Switcher                                             SwitcherConfig
	FFmpeg                                               ffmpeg.Config
	Container                                            fmp4.Limits
	Binary                                               string
	OpenFile                                             FileOpener
	Player                                               PlayerBuilder
	Transcode                                            TranscodeBuilder
	ResubscribeBackoff, ControlInterval, ShutdownTimeout time.Duration
}

func (c RuntimeConfig) Validate() error {
	if c.Switcher.Validate() != nil || c.FFmpeg.Validate() != nil || c.Container.Validate() != nil || c.Binary == "" || c.OpenFile == nil || c.ResubscribeBackoff <= 0 || c.ControlInterval <= 0 || c.ShutdownTimeout <= 0 {
		return core.ErrInvalid
	}
	return nil
}

type RegistryConfig struct {
	Core                     *core.Service
	Parent                   core.ExecutionFence
	Observer                 *core.SessionObserver
	Runtime                  RuntimeConfig
	MaxGroups, MaxReferences uint32
}
type Registry struct {
	mu         sync.Mutex
	config     RegistryConfig
	owner      *core.RuntimeLease
	groups     map[string]*Group
	references uint32
	closed     bool
}

func NewRegistry(ctx context.Context, c RegistryConfig) (*Registry, error) {
	if c.Core == nil || c.Observer == nil || !c.Observer.Valid() || c.Observer.SessionFence() != (core.RuntimeSessionFence{SessionID: c.Parent.SessionID, Incarnation: c.Parent.Incarnation, Lifecycle: c.Parent.Lifecycle}) || c.Parent.Validate() != nil || c.Parent.OutputID != "" || c.MaxGroups == 0 || c.MaxReferences == 0 || c.Runtime.Validate() != nil {
		return nil, core.ErrInvalid
	}
	owner, err := c.Core.BindExecutionOwner(ctx, c.Parent)
	if err != nil {
		return nil, err
	}
	return &Registry{config: c, owner: owner, groups: make(map[string]*Group)}, nil
}

type Reference struct {
	registry *Registry
	group    *Group
	once     sync.Once
	last     bool
}

func (r *Reference) Router() *media.Router { return r.group.switcher.Router() }
func (r *Reference) Group() *Group         { return r.group }
func (r *Reference) Close() error          { return r.CloseContext(context.Background()) }
func (r *Reference) CloseContext(ctx context.Context) error {
	r.once.Do(func() {
		r.registry.mu.Lock()
		r.registry.references--
		r.group.refs--
		r.last = r.group.refs == 0
		if r.last {
			r.group.closing = true
			r.group.cancel()
		}
		r.registry.mu.Unlock()
	})
	if r.last {
		return r.group.Stop(ctx)
	}
	return nil
}

func (r *Registry) Acquire(ctx context.Context, snapshot core.OutputSnapshot, live *media.Router) (*Reference, error) {
	if live == nil || snapshot.Validate() != nil {
		return nil, core.ErrInvalid
	}
	session, err := r.config.Core.GetSession(ctx, r.config.Parent.SessionID)
	if err != nil {
		return nil, err
	}
	if session.Incarnation != r.config.Parent.Incarnation || session.Lifecycle != r.config.Parent.Lifecycle || session.Snapshot.Stream.Fallback != snapshot.Fallback {
		return nil, core.ErrStale
	}
	if err = r.checkSnapshot(ctx, session.ID, snapshot); err != nil {
		return nil, err
	}
	canonical, err := core.CanonicalProfile(snapshot.Profile.Profile)
	if err != nil {
		return nil, err
	}
	manifest := snapshot.Artifact
	manifest.CanonicalProfile = append([]byte(nil), manifest.CanonicalProfile...)
	encoded, err := json.Marshal(struct {
		Parent   core.ExecutionFence
		Group    core.ID
		Profile  []byte
		Manifest core.ArtifactManifest
		Policy   core.FallbackPolicy
	}{r.config.Parent, snapshot.Group.ID, canonical, manifest, snapshot.Fallback})
	if err != nil {
		return nil, err
	}
	key := string(encoded)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.owner.Valid() {
		return nil, core.ErrStale
	}
	if r.references >= r.config.MaxReferences {
		return nil, media.ErrRouterBudget
	}
	g := r.groups[key]
	if g != nil && g.closing {
		select {
		case <-g.done:
			g.mu.Lock()
			err := g.result
			g.mu.Unlock()
			if err != nil {
				return nil, err
			}
			delete(r.groups, key)
			g = nil
		default:
			return nil, core.ErrNotReady
		}
	}
	if g == nil {
		for retiredKey, retired := range r.groups {
			if !retired.closing {
				continue
			}
			select {
			case <-retired.done:
				retired.mu.Lock()
				clean := retired.result == nil
				retired.mu.Unlock()
				if clean {
					delete(r.groups, retiredKey)
				}
			default:
			}
		}
		if uint64(len(r.groups)) >= uint64(r.config.MaxGroups) {
			return nil, media.ErrRouterBudget
		}
		observer, err := r.config.Observer.OpenGroup(ctx, snapshot.Group.ID, snapshot.Profile.ID)
		if err != nil {
			return nil, err
		}
		c := r.config.Runtime
		c.Switcher.Profile = snapshot.Profile.Profile
		c.Switcher.Watchdog = snapshot.Fallback.Watchdog
		g, err = newGroup(c, r.config.Core, r.config.Parent, r.owner, observer, snapshot.Group.ID, manifest, live, session.DesiredPaused)
		if err != nil {
			_ = observer.Close(ctx)
			return nil, err
		}
		r.groups[key] = g
	} else if g.live != live {
		return nil, core.ErrConflict
	}
	r.references++
	g.refs++
	return &Reference{registry: r, group: g}, nil
}
func (r *Registry) checkSnapshot(ctx context.Context, session core.ID, snapshot core.OutputSnapshot) error {
	wanted, err := core.EncodeOutputSnapshot(snapshot)
	if err != nil {
		return err
	}
	request := core.PageRequest{Query: core.ListQuery{Limit: core.MaxPageLimit, SessionID: session}}
	for {
		page, err := r.config.Core.ListOutputs(ctx, request)
		if err != nil {
			return err
		}
		for _, o := range page.Items {
			if !o.Attached || !o.DesiredRunning || o.CleanupPending {
				continue
			}
			actual, err := core.EncodeOutputSnapshot(o.Snapshot)
			if err != nil {
				return err
			}
			if bytes.Equal(wanted, actual) {
				return nil
			}
		}
		if page.NextCursor == "" {
			return core.ErrStale
		}
		request.Cursor = page.NextCursor
	}
}

func (r *Registry) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	groups := make([]*Group, 0, len(r.groups))
	for _, g := range r.groups {
		groups = append(groups, g)
		g.cancel()
	}
	r.mu.Unlock()
	var errs []error
	for _, g := range groups {
		if err := g.Stop(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type Group struct {
	refs     uint32
	closing  bool
	config   RuntimeConfig
	service  *core.Service
	parent   core.ExecutionFence
	owner    *core.RuntimeLease
	observer *core.GroupObserver
	id       core.ID
	manifest core.ArtifactManifest
	live     *media.Router
	switcher *Switcher
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	result   error
	attempts [2]atomic.Uint64
}

func newGroup(c RuntimeConfig, service *core.Service, parent core.ExecutionFence, owner *core.RuntimeLease, observer *core.GroupObserver, id core.ID, manifest core.ArtifactManifest, live *media.Router, paused bool) (*Group, error) {
	if c.Switcher.Profile.Mode == core.ModeCopy && c.Switcher.Profile.Format.Video.Codec != core.CodecAV1 || c.Switcher.Profile.Mode == core.ModeTranscode && c.Switcher.Profile.Format.Video.Codec != core.CodecH264 {
		return nil, core.ErrInvalid
	}
	sw, err := NewSwitcher(c.Switcher)
	if err != nil {
		return nil, err
	}
	// Срок работы принадлежит выданному владельцу Session; ctx запроса Acquire не удерживается.
	ctx, cancel := context.WithCancel(context.Background())
	g := &Group{config: c, service: service, parent: parent, owner: owner, observer: observer, id: id, manifest: manifest, live: live, switcher: sw, cancel: cancel, done: make(chan struct{})}
	_ = sw.SetPaused(paused, time.Now())
	go g.run(ctx)
	return g, nil
}
func (g *Group) Router() *media.Router { return g.switcher.Router() }
func (g *Group) Status() Status        { return g.switcher.Status() }
func (g *Group) Done() <-chan struct{} { return g.done }
func (g *Group) Stop(ctx context.Context) error {
	g.cancel()
	if g.switcher != nil {
		_ = g.switcher.Close()
	}
	select {
	case <-g.done:
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.result
	default:
	}
	timer := time.NewTimer(g.config.ShutdownTimeout)
	defer timer.Stop()
	select {
	case <-g.done:
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.result
	case <-ctx.Done():
		return errors.Join(ffmpeg.ErrCleanupPending, ctx.Err())
	case <-timer.C:
		return ffmpeg.ErrCleanupPending
	}
}
func (g *Group) run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); g.play(ctx) }()
	go func() { defer wg.Done(); g.consumeLive(ctx) }()
	ticker := time.NewTicker(g.config.ControlInterval)
	running := true
	for running {
		select {
		case <-ctx.Done():
			running = false
		case <-g.owner.Done():
			g.cancel()
			running = false
		case <-ticker.C:
			session, err := g.service.GetSession(ctx, g.parent.SessionID)
			if err != nil {
				g.cancel()
				running = false
				break
			}
			if session.Incarnation != g.parent.Incarnation || session.Lifecycle != g.parent.Lifecycle {
				g.cancel()
				running = false
				break
			}
			now := time.Now()
			_ = g.switcher.SetPaused(session.DesiredPaused, now)
			_ = g.switcher.Tick(now)
			g.observe(ctx)
		}
	}
	ticker.Stop()
	g.cancel()
	wg.Wait()
	err := g.switcher.Close()
	cleanupCtx, cancel := context.WithTimeout(context.Background(), g.config.ShutdownTimeout)
	defer cancel()
	err = errors.Join(err, g.observer.Close(cleanupCtx))
	g.mu.Lock()
	g.result = errors.Join(g.result, err)
	close(g.done)
	g.mu.Unlock()
}
func (g *Group) observe(ctx context.Context) {
	status := g.switcher.Status()
	state, source := core.ObservationReady, core.ObservationSourceNone
	failure := ""
	if status.Source == Live {
		source = core.ObservationSourceLive
	}
	if status.Source == Fallback {
		source = core.ObservationSourceFallback
	}
	if !status.Available {
		state = core.ObservationFailed
		source = core.ObservationSourceNone
		if status.Err != nil {
			failure = status.Err.Error()
		}
	}
	_ = g.observer.Observe(ctx, core.GroupObservation{State: state, Source: source, Error: failure})
}
func (g *Group) push(ctx context.Context, feed Feed, attempt uint64, p media.Packet, v, a media.CodecConfig) error {
	return g.owner.Do(ctx, func() error { return g.switcher.Push(feed, attempt, p, v, a, time.Now()) })
}
func (g *Group) consumeLive(ctx context.Context) {
	for ctx.Err() == nil && g.owner.Valid() {
		sub, err := g.live.Subscribe(g.config.Switcher.Queue)
		if err != nil {
			if errors.Is(err, media.ErrRouterClosed) {
				return
			}
			if !delay(ctx, g.config.ResubscribeBackoff) {
				return
			}
			continue
		}
		attempt, allocationErr := g.nextAttempt(Live)
		if allocationErr != nil {
			_ = sub.Close()
			g.fail(Live, allocationErr)
			return
		}
		if g.config.Switcher.Profile.Mode == core.ModeCopy {
			for ctx.Err() == nil {
				d, e := sub.Next(ctx)
				if e != nil {
					err = e
					break
				}
				if !d.IsCurrent() {
					continue
				}
				e = g.push(ctx, Live, attempt, d.Packet, d.VideoConfig, d.AudioConfig)
				if e != nil && !errors.Is(e, ErrUnavailable) && !errors.Is(e, ErrFeed) {
					err = e
					break
				}
			}
		} else {
			err = g.transcode(ctx, sub, attempt)
		}
		_ = sub.Close()
		_ = g.switcher.Fail(Live, attempt, err, time.Now())
		if !delay(ctx, g.config.ResubscribeBackoff) {
			return
		}
	}
}
func delay(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func (g *Group) play(ctx context.Context) {
	observer, err := g.observer.OpenFFmpeg(ctx)
	if err != nil {
		g.fail(Fallback, err)
		return
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), g.config.ShutdownTimeout)
		defer cancel()
		_ = observer.Close(c)
	}()
	supervisor, err := ffmpeg.New(g.config.FFmpeg)
	if err != nil {
		g.fail(Fallback, err)
		return
	}
	var attempt atomic.Uint64
	var observation atomic.Pointer[core.FFmpegAttemptObserver]
	worker := ffmpeg.Worker{SessionID: string(g.parent.SessionID), GroupID: string(g.id), Owner: g.owner, Topology: ffmpeg.FileInput}
	worker.Command = func(ctx context.Context, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		current, err := g.nextAttempt(Fallback)
		if err != nil {
			return ffmpeg.Command{}, err
		}
		attempt.Store(current)
		ao, err := observer.BeginAttempt(ctx, current)
		if err != nil {
			return ffmpeg.Command{}, err
		}
		_ = ao.Observe(ctx, core.FFmpegObservation{State: core.ObservationStarting})
		observation.Store(ao)
		file, err := g.config.OpenFile(ctx, g.manifest)
		if err != nil {
			return ffmpeg.Command{}, err
		}
		var command ffmpeg.Command
		if g.config.Player != nil {
			command, err = g.config.Player(ctx, g.config.Binary, file, a)
		} else {
			command, err = ffmpeg.BuildFileCommand(g.config.Binary, file, a)
		}
		command.ExtraFiles = []*os.File{file}
		if err != nil {
			return command, err
		}
		return command, nil
	}
	worker.Output = func(ctx context.Context, c net.Conn, progress ffmpeg.Progress) error {
		return g.readOutputObserved(ctx, c, Fallback, attempt.Load(), g.manifest.VerifiedFormat, progress, observation.Load())
	}
	if err = supervisor.Start(ctx, worker); err != nil {
		g.fail(Fallback, err)
		return
	}
	result, _ := supervisor.Wait(context.Background())
	g.fail(Fallback, result.Err)
	g.addCleanup(result.CleanupErr)
}
func (g *Group) nextAttempt(feed Feed) (uint64, error) {
	counter := &g.attempts[index(feed)]
	for {
		previous := counter.Load()
		if previous == math.MaxUint64 {
			return 0, ErrFeed
		}
		if counter.CompareAndSwap(previous, previous+1) {
			return previous + 1, nil
		}
	}
}

func (g *Group) fail(feed Feed, err error) {
	attempt := g.attempts[index(feed)].Load()
	if attempt == 0 {
		attempt, _ = g.nextAttempt(feed)
	}
	_ = g.switcher.Fail(feed, attempt, err, time.Now())
}

func (g *Group) addCleanup(err error) {
	if err != nil {
		g.mu.Lock()
		g.result = errors.Join(g.result, err)
		g.mu.Unlock()
	}
}
func (g *Group) readOutput(ctx context.Context, c net.Conn, feed Feed, attempt uint64, verified core.MediaFormat, progress ffmpeg.Progress) error {
	return g.readOutputObserved(ctx, c, feed, attempt, verified, progress, nil)
}
func (g *Group) readOutputObserved(ctx context.Context, c net.Conn, feed Feed, attempt uint64, verified core.MediaFormat, progress ffmpeg.Progress, observation *core.FFmpegAttemptObserver) (result error) {
	defer func() {
		if observation == nil {
			return
		}
		state := core.ObservationFailed
		failure := ""
		if result != nil {
			failure = result.Error()
		}
		if ctx.Err() != nil {
			state = core.ObservationStopped
			failure = ""
		}
		cleanup, cancel := context.WithTimeout(context.Background(), g.config.ShutdownTimeout)
		defer cancel()
		_ = observation.Observe(cleanup, core.FFmpegObservation{State: state, Error: failure})
	}()
	seen := [2]bool{}
	ready := false
	d, err := fmp4.NewDemuxer(c, fmp4.DemuxOptions{Limits: g.config.Container, SourceEpoch: attempt, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		if desc.Codec != core.CodecAV1 {
			return fmp4.ResolveCodecConfig(desc, attempt, 1)
		}
		if verified.Validate() != nil || desc.Width != verified.Video.Width || desc.Height != verified.Video.Height {
			return media.CodecConfig{}, codec.ErrUnready
		}
		return media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: attempt, Revision: 1, TimeBase: desc.TimeBase, Video: verified.Video}, desc.Headers)
	}})
	if err != nil {
		return err
	}
	init := d.Init()
	var inspector codec.Inspector
	for {
		p, err := d.Next()
		if err != nil {
			return err
		}
		if _, err = inspector.Inspect(p); err != nil {
			return err
		}
		track := ffmpeg.Video
		if p.Config().Info().Track == media.TrackAudio {
			track = ffmpeg.Audio
		}
		if !progress(track, p.Info().DTS) {
			return ErrFeed
		}
		seen[int(track)-1] = true
		if observation != nil && !ready && seen[0] && seen[1] {
			_ = observation.Observe(ctx, core.FFmpegObservation{State: core.ObservationReady})
			ready = true
		}
		err = g.push(ctx, feed, attempt, p, init.Video, init.Audio)
		if err != nil && !errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrFeed) {
			return err
		}
	}
}
func (g *Group) transcode(ctx context.Context, sub *media.Subscription, attempt uint64) error {
	first, err := sub.Next(ctx)
	if err != nil {
		return err
	}
	if !first.IsCurrent() {
		return ErrFeed
	}
	observer, err := g.observer.OpenFFmpeg(ctx)
	if err != nil {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), g.config.ShutdownTimeout)
		defer cancel()
		_ = observer.Close(c)
	}()
	// Каждая новая входная конфигурация получает отдельный работник; исходный Router остаётся.
	workerConfig := g.config.FFmpeg
	workerConfig.MaxRestarts = 0
	supervisor, err := ffmpeg.New(workerConfig)
	if err != nil {
		return err
	}
	input := core.MediaFormat{Video: first.VideoConfig.Info().Video, Audio: first.AudioConfig.Info().Audio}
	var observation atomic.Pointer[core.FFmpegAttemptObserver]
	worker := ffmpeg.Worker{SessionID: string(g.parent.SessionID), GroupID: string(g.id), Owner: g.owner}
	worker.Command = func(ctx context.Context, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		ao, err := observer.BeginAttempt(ctx, attempt)
		if err != nil {
			return ffmpeg.Command{}, err
		}
		_ = ao.Observe(ctx, core.FFmpegObservation{State: core.ObservationStarting})
		observation.Store(ao)
		config := ffmpeg.NVIDIAConfig{Binary: g.config.Binary, Input: input, Profile: g.config.Switcher.Profile, VideoTimeBase: first.VideoConfig.Info().TimeBase, AudioTimeBase: first.AudioConfig.Info().TimeBase}
		if g.config.Transcode != nil {
			return g.config.Transcode(ctx, config, a)
		}
		return ffmpeg.BuildNVIDIACommand(config, a)
	}
	worker.Input = func(ctx context.Context, c net.Conn, _ ffmpeg.Progress) error {
		origin, err := transportOrigin(first)
		if err != nil {
			return err
		}
		mux, err := fmp4.NewMuxer(c, fmp4.Init{Video: first.VideoConfig, Audio: first.AudioConfig, Origin: origin}, g.config.Container)
		if err != nil {
			return err
		}
		validator := fmp4.NewFFmpegTimingValidatorForOrigin(origin)
		delivery := first
		for {
			if !delivery.IsCurrent() {
				return media.ErrStaleSource
			}
			if err = validator.Validate(delivery.Packet); err != nil {
				return err
			}
			if err = mux.WritePacket(delivery.Packet); err != nil {
				return err
			}
			delivery, err = sub.Next(ctx)
			if err != nil {
				return err
			}
			if !sameConfig(delivery.VideoConfig, first.VideoConfig) || !sameConfig(delivery.AudioConfig, first.AudioConfig) {
				return media.ErrStaleSource
			}
		}
	}
	worker.Output = func(ctx context.Context, c net.Conn, progress ffmpeg.Progress) error {
		return g.readOutputObserved(ctx, c, Live, attempt, core.MediaFormat{}, progress, observation.Load())
	}
	if err = supervisor.Start(ctx, worker); err != nil {
		return err
	}
	result, err := supervisor.Wait(context.Background())
	g.addCleanup(result.CleanupErr)
	return errors.Join(err, result.Err)
}
func transportOrigin(d media.Delivery) (fmp4.OriginMapping, error) {
	scale, err := fmp4.MovieTimeScale(d.VideoConfig.Info().TimeBase, d.AudioConfig.Info().TimeBase)
	if err != nil {
		return fmp4.OriginMapping{}, err
	}
	base := media.TimeBase{Numerator: 1, Denominator: scale}
	start, err := rescale(d.Packet.Info().DTS, d.Packet.Config().Info().TimeBase, base)
	if err != nil {
		return fmp4.OriginMapping{}, err
	}
	if start >= 0 {
		return fmp4.OriginMapping{}, nil
	}
	offset, err := difference(0, start)
	return fmp4.OriginMapping{OffsetTicks: offset, TimeBase: base}, err
}
