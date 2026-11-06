// Package group владеет общей обработкой и непрерывным выходом одной группы Session.
package group

import (
	"bytes"
	"errors"
	"math"
	"math/big"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

var (
	ErrUnavailable = errors.New("оба источника группы недоступны")
	ErrTiming      = errors.New("временная шкала группы не представима")
	ErrFeed        = errors.New("устаревший или непригодный источник группы")
	ErrFeedEnded   = errors.New("источник группы завершился")
)

type Feed uint8

const (
	Live Feed = iota + 1
	Fallback
)

type SwitcherConfig struct {
	Profile                     core.GroupProfile
	Watchdog                    media.WatchdogPolicy
	Queue                       media.QueueLimits
	Router                      media.RouterLimits
	MaxAVSkew, MaxTransitionGap time.Duration
}

func (c SwitcherConfig) Validate() error {
	if c.Profile.Validate() != nil || c.Watchdog.Validate() != nil {
		return core.ErrInvalid
	}
	return c.ValidateTemplate()
}

// ValidateTemplate проверяет серверные бюджеты до выбора профиля и политики Stream.
func (c SwitcherConfig) ValidateTemplate() error {
	if c.Queue.MaxPackets < 4 || c.Queue.Validate() != nil || c.Router.Validate() != nil || c.MaxAVSkew <= 0 || c.MaxTransitionGap <= 0 {
		return core.ErrInvalid
	}
	return nil
}

type Status struct {
	Source          Feed
	Paused          bool
	Available       bool
	Err             error
	Epoch, Revision uint64
	StateRevision   uint64
}
type queued struct {
	packet media.Packet
	at     time.Time
	key    bool
}
type feedState struct {
	attempt      uint64
	video, audio media.CodecConfig
	inspector    codec.Inspector
	queue        []queued
	bytes        uint64
	pending      [2]*queued
	seen         [2]bool
	dts          [2]int64
	arrival      [2]time.Time
	stable       time.Time
	rejected     bool
	failure      error
	ready        bool
}
type Switcher struct {
	mu              sync.Mutex
	config          SwitcherConfig
	router          *media.Router
	source          *media.Source
	feeds           [2]feedState
	selected        Feed
	paused          bool
	status          Status
	base            media.TimeBase
	offset, horizon int64
	hasHorizon      bool
	revision        uint64
	video, audio    media.CodecConfig
	closed          bool
	lastNow         time.Time
	stateRevision   uint64
	failureRevision uint64
	changed         chan struct{}
	outputCounters  media.CounterSnapshot
}

func NewSwitcher(c SwitcherConfig) (*Switcher, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	frameRate := c.Profile.Format.Video.FrameRate
	divisor := gcd32(frameRate.Numerator, frameRate.Denominator)
	scale, err := fmp4.MovieTimeScale(media.TimeBase{Numerator: frameRate.Denominator / divisor, Denominator: frameRate.Numerator / divisor}, media.TimeBase{Numerator: 1, Denominator: c.Profile.Format.Audio.SampleRateHz}, media.TimeBase{Numerator: 1, Denominator: 1000})
	if err != nil {
		return nil, err
	}
	r, err := media.NewRouter(c.Router)
	if err != nil {
		return nil, err
	}
	return &Switcher{config: c, router: r, base: media.TimeBase{Numerator: 1, Denominator: scale}, status: Status{Err: ErrUnavailable, StateRevision: 1}, stateRevision: 1, changed: make(chan struct{})}, nil
}

func gcd32(a, b uint32) uint32 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func (s *Switcher) Router() *media.Router { return s.router }
func (s *Switcher) Status() Status        { s.mu.Lock(); defer s.mu.Unlock(); return s.status }

// StateChange возвращает согласованные последнее состояние и сигнал его изменения.
func (s *Switcher) StateChange() (Status, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.changed
}

func (s *Switcher) recordStateLocked(previous Status, previousFailure uint64) {
	if previous.Source != s.status.Source || previous.Paused != s.status.Paused || previous.Available != s.status.Available || previous.Epoch != s.status.Epoch || (previous.Err == nil) != (s.status.Err == nil) || (!s.status.Available && previousFailure != s.failureRevision) {
		if s.stateRevision < math.MaxUint64 {
			s.stateRevision++
		}
		close(s.changed)
		s.changed = make(chan struct{})
	}
	s.status.StateRevision = s.stateRevision
}

func (s *Switcher) OutputSnapshot() media.CounterSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := s.outputCounters
	snapshot.SampledAt = time.Now()
	return snapshot
}
func sameConfig(a, b media.CodecConfig) bool {
	return a.Info() == b.Info() && bytes.Equal(a.Headers(), b.Headers())
}
func index(f Feed) int { return int(f) - 1 }

// Push принимает только локальную идентичность источника и его полную пару конфигураций.
// Внешние очереди продолжают вызывать Push для обоих источников при любом выборе.
func (s *Switcher) Push(feed Feed, attempt uint64, p media.Packet, video, audio media.CodecConfig, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recordStateLocked(s.status, s.failureRevision)
	if s.closed {
		return media.ErrRouterClosed
	}
	if feed != Live && feed != Fallback || attempt == 0 || now.IsZero() {
		return core.ErrInvalid
	}
	now = s.monotonic(now)
	f := &s.feeds[index(feed)]
	if attempt < f.attempt {
		return ErrFeed
	}
	if video.Validate() != nil || audio.Validate() != nil || video.Info().Track != media.TrackVideo || audio.Info().Track != media.TrackAudio || video.Info().SourceEpoch != audio.Info().SourceEpoch {
		return s.reject(feed, ErrFeed)
	}
	if attempt == f.attempt && f.attempt != 0 {
		if video.Info().SourceEpoch < f.video.Info().SourceEpoch {
			return ErrFeed
		}
		if video.Info().SourceEpoch == f.video.Info().SourceEpoch && (video.Info().Revision < f.video.Info().Revision || audio.Info().Revision < f.audio.Info().Revision) {
			return ErrFeed
		}
		if video.Info().SourceEpoch == f.video.Info().SourceEpoch && (video.Info().Revision == f.video.Info().Revision && !sameConfig(video, f.video) || audio.Info().Revision == f.audio.Info().Revision && !sameConfig(audio, f.audio)) {
			return s.reject(feed, ErrFeed)
		}
	}
	if attempt != f.attempt || !sameConfig(video, f.video) || !sameConfig(audio, f.audio) {
		*f = feedState{attempt: attempt, video: video, audio: audio}
		if s.selected == feed {
			s.selected = 0
		}
	}
	if f.rejected {
		return ErrFeed
	}
	if video.StoredBytes() > min(s.config.Queue.MaxConfigBytes, s.config.Router.MaxConfigBytes) || audio.StoredBytes() > min(s.config.Queue.MaxConfigBytes, s.config.Router.MaxConfigBytes) || p.StoredBytes() > s.config.Queue.MaxPacketBytes {
		return s.reject(feed, media.ErrRouterBudget)
	}
	expected := video
	if p.Config().Info().Track == media.TrackAudio {
		expected = audio
	}
	if !sameConfig(p.Config(), expected) {
		return s.reject(feed, ErrFeed)
	}
	if err := codec.Compatible(core.MediaFormat{Video: video.Info().Video, Audio: audio.Info().Audio}, s.config.Profile.Format); err != nil {
		return s.reject(feed, err)
	}
	key, err := f.inspector.Inspect(p)
	if err != nil {
		return s.reject(feed, err)
	}
	pi := p.Info()
	track := 0
	if p.Config().Info().Track == media.TrackAudio {
		track = 1
	}
	if f.seen[track] && pi.DTS <= f.dts[track] {
		if pi.DTS < f.dts[track] {
			return s.reject(feed, media.ErrDTSRegression)
		}
		return ErrFeed
	}
	if f.seen[track] && now.Sub(f.arrival[track]) >= s.timeout(track) {
		f.stable = time.Time{}
		f.ready = false
	}
	q := queued{packet: p, at: now, key: key}
	if previous := f.pending[track]; previous != nil {
		f.pending[track] = nil
		duration, err := difference(pi.DTS, previous.packet.Info().DTS)
		if err != nil || duration <= 0 {
			return s.reject(feed, ErrTiming)
		}
		info := previous.packet.Info()
		info.DurationTicks = duration
		previous.packet, err = media.NewPacket(info, previous.packet.Config(), previous.packet.Payload())
		if err != nil {
			return s.reject(feed, err)
		}
		if err = s.enqueue(feed, *previous); err != nil {
			return err
		}
	}
	f.seen[track] = true
	f.dts[track] = pi.DTS
	f.arrival[track] = now
	if f.seen[0] && f.seen[1] && f.stable.IsZero() {
		f.stable = now
	}
	if pi.DurationTicks == 0 {
		if !s.fits(f, q) {
			return s.reject(feed, media.ErrRouterBudget)
		}
		f.pending[track] = &q
	} else if err = s.enqueue(feed, q); err != nil {
		return err
	}
	return s.evaluate(now)
}
func (s *Switcher) monotonic(now time.Time) time.Time {
	if now.Before(s.lastNow) {
		return s.lastNow
	}
	s.lastNow = now
	return now
}

func (s *Switcher) timeout(track int) time.Duration {
	if track == 0 {
		return s.config.Watchdog.VideoTimeout
	}
	return s.config.Watchdog.AudioTimeout
}
func (s *Switcher) enqueue(feed Feed, q queued) error {
	f := &s.feeds[index(feed)]
	weight := q.packet.StoredBytes()
	if weight > s.config.Queue.MaxPacketBytes {
		return s.reject(feed, media.ErrRouterBudget)
	}
	if q.key && s.selected != feed {
		f.ready = false
		f.queue = nil
		f.bytes = 0
	}
	if !s.fits(f, q) {
		f.queue = nil
		f.bytes = 0
		f.pending = [2]*queued{}
		f.stable = time.Time{}
		f.ready = false
		if s.selected == feed {
			s.selected = 0
		}
		if !q.key {
			return nil
		}
		if !s.fits(f, q) {
			return s.reject(feed, media.ErrRouterBudget)
		}
	}
	f.queue = append(f.queue, q)
	f.bytes += weight
	return nil
}
func (s *Switcher) fits(f *feedState, q queued) bool {
	total, count := f.bytes, uint64(len(f.queue))
	for _, c := range []media.CodecConfig{f.video, f.audio} {
		n := c.StoredBytes()
		if n > s.config.Queue.MaxBytes-total {
			return false
		}
		total += n
	}
	for _, p := range f.pending {
		if p != nil {
			n := p.packet.StoredBytes()
			if n > s.config.Queue.MaxBytes-total {
				return false
			}
			total += n
			count++
		}
	}
	return count < uint64(s.config.Queue.MaxPackets) && q.packet.StoredBytes() <= s.config.Queue.MaxBytes-total
}

func (s *Switcher) reject(feed Feed, err error) error {
	if s.failureRevision < math.MaxUint64 {
		s.failureRevision++
	}
	f := &s.feeds[index(feed)]
	f.rejected = true
	f.ready = false
	f.failure = err
	f.queue = nil
	f.pending = [2]*queued{}
	f.bytes = 0
	f.stable = time.Time{}
	if s.selected == feed {
		s.selected = 0
	}
	if s.selected == 0 {
		s.status = Status{Paused: s.paused, Err: errors.Join(ErrUnavailable, err), Epoch: s.status.Epoch, Revision: s.revision}
	}
	return err
}
func (s *Switcher) healthy(f *feedState, now time.Time) bool {
	if f.rejected || !f.seen[0] || !f.seen[1] {
		return false
	}
	for i := range 2 {
		if now.Before(f.arrival[i]) || now.Sub(f.arrival[i]) >= s.timeout(i) {
			f.stable = time.Time{}
			f.ready = false
			return false
		}
	}
	return true
}

// BeginAttempt отзывает прежнюю готовность до запуска новой внешней попытки.
func (s *Switcher) BeginAttempt(feed Feed, attempt uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recordStateLocked(s.status, s.failureRevision)
	if s.closed {
		return media.ErrRouterClosed
	}
	if (feed != Live && feed != Fallback) || attempt == 0 {
		return core.ErrInvalid
	}
	f := &s.feeds[index(feed)]
	if attempt <= f.attempt {
		return ErrFeed
	}
	*f = feedState{attempt: attempt}
	if s.selected == feed {
		s.selected = 0
		s.status.Available = false
		s.status.Err = ErrUnavailable
	}
	return nil
}

// FallbackReady проверяет текущую попытку независимо от выбранного источника.
// Проверка использует те же A/V и временные условия, что и переключение.
func (s *Switcher) FallbackReady(attempt uint64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recordStateLocked(s.status, s.failureRevision)
	if s.closed {
		return media.ErrRouterClosed
	}
	f := &s.feeds[index(Fallback)]
	if attempt == 0 || f.attempt != attempt {
		return core.ErrNotReady
	}
	if f.rejected {
		return errors.Join(ErrUnavailable, f.failure)
	}
	if !s.healthy(f, s.monotonic(now)) {
		return core.ErrNotReady
	}
	if f.ready && s.selected == Fallback {
		return nil
	}
	if _, _, _, err := s.transition(Fallback, now); err != nil {
		return err
	}
	f.ready = true
	return nil
}

func (s *Switcher) Tick(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recordStateLocked(s.status, s.failureRevision)
	if s.closed {
		return media.ErrRouterClosed
	}
	return s.evaluate(s.monotonic(now))
}
func (s *Switcher) SetPaused(paused bool, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recordStateLocked(s.status, s.failureRevision)
	if s.closed {
		return media.ErrRouterClosed
	}
	s.paused = paused
	return s.evaluate(s.monotonic(now))
}
func (s *Switcher) Fail(feed Feed, attempt uint64, err error, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recordStateLocked(s.status, s.failureRevision)
	if s.closed {
		return media.ErrRouterClosed
	}
	if feed != Live && feed != Fallback || attempt == 0 {
		return core.ErrInvalid
	}
	if err == nil {
		err = ErrFeedEnded
	}
	f := &s.feeds[index(feed)]
	if attempt < f.attempt {
		return ErrFeed
	}
	if attempt > f.attempt {
		*f = feedState{attempt: attempt}
	}
	_ = s.reject(feed, err)
	return s.evaluate(s.monotonic(now))
}
func (s *Switcher) evaluate(now time.Time) error {
	live, fallback := s.healthy(&s.feeds[0], now), s.healthy(&s.feeds[1], now)
	desired := s.selected
	if s.paused || !live {
		desired = Fallback
	} else if s.selected == Live {
		desired = Live
	} else {
		f := &s.feeds[0]
		if !f.stable.IsZero() && now.Sub(f.stable) >= s.config.Watchdog.RecoveryWindow {
			desired = Live
		}
	}
	if desired == Fallback && !fallback {
		if live && !s.paused {
			desired = Live
		} else {
			desired = 0
		}
	}
	if desired == 0 {
		s.selected = 0
		s.status = Status{Paused: s.paused, Err: errors.Join(ErrUnavailable, s.feeds[0].failure, s.feeds[1].failure), Epoch: s.status.Epoch, Revision: s.revision}
		return ErrUnavailable
	}
	if s.selected != desired {
		if err := s.begin(desired, now); err != nil {
			if s.selected != 0 && s.healthy(&s.feeds[index(s.selected)], now) && (!s.paused || s.selected == Fallback) {
				return s.drain(s.selected)
			}
			s.status.Available = false
			s.status.Err = errors.Join(ErrUnavailable, err)
			return s.status.Err
		}
	}
	return s.drain(desired)
}
func (s *Switcher) transition(feed Feed, now time.Time) (int, int, int64, error) {
	f := &s.feeds[index(feed)]
	vi, ai := -1, -1
	for i, q := range f.queue {
		if q.packet.Config().Info().Track != media.TrackVideo || !q.key {
			continue
		}
		if feed == Live && s.selected != Live && !s.paused {
			ready := f.stable.Add(s.config.Watchdog.RecoveryWindow)
			if q.at.Before(ready) {
				continue
			}
			if q.at.After(ready.Add(s.config.Watchdog.KeyframeTimeout)) {
				f.stable = now
				return 0, 0, 0, ErrFeed
			}
		}
		vi = i
		break
	}
	if vi < 0 {
		return 0, 0, 0, ErrFeed
	}
	vp := f.queue[vi].packet
	vpts, err := rescale(vp.Info().PTS, vp.Config().Info().TimeBase, s.base)
	if err != nil {
		return 0, 0, 0, s.reject(feed, err)
	}
	for i, q := range f.queue {
		if q.packet.Config().Info().Track != media.TrackAudio {
			continue
		}
		a, err := rescale(q.packet.Info().PTS, q.packet.Config().Info().TimeBase, s.base)
		if err != nil {
			return 0, 0, 0, s.reject(feed, err)
		}
		if a >= vpts {
			skew := new(big.Int).Sub(big.NewInt(a), big.NewInt(vpts))
			if skew.Cmp(s.duration(s.config.MaxAVSkew)) > 0 {
				return 0, 0, 0, s.reject(feed, ErrTiming)
			}
			ai = i
			break
		}
	}
	if ai < 0 {
		return 0, 0, 0, ErrFeed
	}
	earliest := int64(math.MaxInt64)
	for _, i := range []int{vi, ai} {
		p := f.queue[i].packet
		for _, t := range []int64{p.Info().PTS, p.Info().DTS} {
			v, e := rescale(t, p.Config().Info().TimeBase, s.base)
			if e != nil {
				return 0, 0, 0, s.reject(feed, e)
			}
			earliest = min(earliest, v)
		}
	}
	target := int64(0)
	if s.hasHorizon {
		target = s.horizon
	}
	firstPresentation := vpts
	ap := f.queue[ai].packet
	apts, e := rescale(ap.Info().PTS, ap.Config().Info().TimeBase, s.base)
	if e != nil {
		return 0, 0, 0, s.reject(feed, e)
	}
	firstPresentation = min(firstPresentation, apts)
	gap := new(big.Int).Sub(big.NewInt(firstPresentation), big.NewInt(earliest))
	if gap.Cmp(s.duration(s.config.MaxTransitionGap)) > 0 {
		return 0, 0, 0, s.reject(feed, ErrTiming)
	}
	offset, err := difference(target, earliest)
	if err != nil {
		return 0, 0, 0, s.reject(feed, err)
	}
	for i, q := range f.queue {
		track := q.packet.Config().Info().Track
		if track == media.TrackVideo && i < vi || track == media.TrackAudio && i < ai {
			continue
		}
		if _, _, err := s.mapPacketInfo(q.packet, offset); err != nil {
			return 0, 0, 0, s.reject(feed, err)
		}
	}
	return vi, ai, offset, nil
}

func (s *Switcher) begin(feed Feed, now time.Time) error {
	vi, ai, offset, err := s.transition(feed, now)
	if err != nil {
		return err
	}
	f := &s.feeds[index(feed)]
	if s.revision == math.MaxUint64 {
		return ErrTiming
	}
	revision := s.revision + 1
	source, err := s.router.BeginSource()
	if err != nil {
		return err
	}
	configs := [2]media.CodecConfig{}
	for i, c := range []media.CodecConfig{f.video, f.audio} {
		info := c.Info()
		info.SourceEpoch = source.Epoch()
		info.Revision = revision
		info.TimeBase = s.base
		configs[i], err = media.NewCodecConfig(info, c.Headers())
		if err != nil {
			return err
		}
	}
	if err = source.UpdateConfigs(configs[0], configs[1]); err != nil {
		return err
	}
	f.ready = true
	s.source = source
	s.video, s.audio = configs[0], configs[1]
	s.revision = revision
	s.offset = offset
	s.selected = feed
	keep := make([]queued, 0, len(f.queue))
	for i, q := range f.queue {
		track := q.packet.Config().Info().Track
		if track == media.TrackVideo && i >= vi || track == media.TrackAudio && i >= ai {
			keep = append(keep, q)
		}
	}
	f.queue = keep
	f.bytes = 0
	for _, q := range keep {
		f.bytes += q.packet.StoredBytes()
	}
	s.status = Status{Source: feed, Paused: s.paused, Available: true, Epoch: source.Epoch(), Revision: revision}
	return nil
}
func (s *Switcher) duration(d time.Duration) *big.Int {
	n := new(big.Int).Mul(big.NewInt(int64(d)), new(big.Int).SetUint64(uint64(s.base.Denominator)))
	return n.Quo(n, big.NewInt(int64(time.Second)*int64(s.base.Numerator)))
}
func (s *Switcher) drain(feed Feed) error {
	f := &s.feeds[index(feed)]
	for len(f.queue) > 0 {
		q := f.queue[0]
		f.queue[0] = queued{}
		f.queue = f.queue[1:]
		f.bytes -= q.packet.StoredBytes()
		p := q.packet
		info, end, err := s.mapPacketInfo(p, s.offset)
		if err != nil {
			return s.reject(feed, err)
		}
		c := s.video
		info.Keyframe = q.key
		if p.Config().Info().Track == media.TrackAudio {
			c = s.audio
			info.Keyframe = false
		}
		out, err := media.NewPacket(info, c, p.Payload())
		if err != nil {
			return s.reject(feed, err)
		}
		if err = s.source.Publish(out); err != nil {
			return s.reject(feed, err)
		}
		s.outputCounters.AddPacket(out, time.Now())
		if !s.hasHorizon {
			s.horizon = end
			s.hasHorizon = true
		} else {
			s.horizon = max(s.horizon, end)
		}
	}
	s.status = Status{Source: feed, Paused: s.paused, Available: true, Epoch: s.source.Epoch(), Revision: s.revision}
	return nil
}
func (s *Switcher) mapPacketInfo(p media.Packet, offset int64) (media.PacketInfo, int64, error) {
	info, base := p.Info(), p.Config().Info().TimeBase
	var err error
	info.PTS, err = mapTime(info.PTS, base, s.base, offset)
	if err != nil {
		return info, 0, err
	}
	info.DTS, err = mapTime(info.DTS, base, s.base, offset)
	if err != nil {
		return info, 0, err
	}
	info.DurationTicks, err = rescale(info.DurationTicks, base, s.base)
	if err != nil || info.DurationTicks <= 0 {
		return info, 0, ErrTiming
	}
	endPTS, err := sum(info.PTS, info.DurationTicks)
	if err != nil {
		return info, 0, err
	}
	endDTS, err := sum(info.DTS, info.DurationTicks)
	if err != nil {
		return info, 0, err
	}
	return info, max(endPTS, endDTS), nil
}

func (s *Switcher) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recordStateLocked(s.status, s.failureRevision)
	if !s.closed && s.failureRevision < math.MaxUint64 {
		s.failureRevision++
	}
	s.closed = true
	s.status = Status{Paused: s.paused, Err: media.ErrRouterClosed, Epoch: s.status.Epoch, Revision: s.revision}
	for i := range s.feeds {
		s.feeds[i] = feedState{}
	}
	return s.router.Close()
}
func rescale(v int64, from, to media.TimeBase) (int64, error) {
	if from.Validate() != nil || to.Validate() != nil {
		return 0, ErrTiming
	}
	n := new(big.Int).Mul(big.NewInt(v), new(big.Int).SetUint64(uint64(from.Numerator)*uint64(to.Denominator)))
	d := new(big.Int).SetUint64(uint64(from.Denominator) * uint64(to.Numerator))
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(n, d, r)
	if r.Sign() != 0 || !q.IsInt64() {
		return 0, ErrTiming
	}
	return q.Int64(), nil
}
func sum(a, b int64) (int64, error) {
	n := new(big.Int).Add(big.NewInt(a), big.NewInt(b))
	if !n.IsInt64() {
		return 0, ErrTiming
	}
	return n.Int64(), nil
}
func difference(a, b int64) (int64, error) {
	n := new(big.Int).Sub(big.NewInt(a), big.NewInt(b))
	if !n.IsInt64() {
		return 0, ErrTiming
	}
	return n.Int64(), nil
}
func mapTime(v int64, from, to media.TimeBase, offset int64) (int64, error) {
	n, e := rescale(v, from, to)
	if e != nil {
		return 0, e
	}
	return sum(n, offset)
}
