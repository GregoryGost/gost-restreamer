package core

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"
)

var ErrEventSequenceExhausted = errors.New("исчерпана последовательность оперативных событий")

type EventLimits struct {
	MaxEvents               int
	MaxBytes, MaxEventBytes uint64
	MaxPageSize             int
}

func (l EventLimits) Validate() error {
	if l.MaxEvents <= 0 || l.MaxPageSize <= 0 || l.MaxPageSize > l.MaxEvents || l.MaxEventBytes < 256 || l.MaxEventBytes > l.MaxBytes || uint64(l.MaxEvents) > math.MaxUint64/l.MaxEventBytes {
		return ErrInvalid
	}
	return nil
}

type TelemetryLimits struct {
	MediaStaleAfter              time.Duration
	Events                       EventLimits
	MaxGPUDevices                int
	MaxGPUUUIDBytes, MaxGPUBytes uint64
}

func (l TelemetryLimits) Validate() error {
	if err := l.Events.Validate(); err != nil {
		return err
	}
	if l.MediaStaleAfter <= 0 || l.MaxGPUDevices <= 0 || l.MaxGPUUUIDBytes == 0 || l.MaxGPUUUIDBytes > math.MaxUint64-1024 || uint64(l.MaxGPUDevices) > (math.MaxUint64-256)/(1024+l.MaxGPUUUIDBytes) || l.MaxGPUBytes < 256 {
		return ErrInvalid
	}
	return nil
}

type RuntimeCursor struct {
	Incarnation string
	Epoch       ID
	Sequence    uint64
}

type RuntimeEventKind string

const (
	RuntimeStateEvent   RuntimeEventKind = "state"
	RuntimeMetricsEvent RuntimeEventKind = "metrics"
	RuntimeInputEvent   RuntimeEventKind = "input"
	RuntimeGPUEvent     RuntimeEventKind = "gpu"
	RuntimeRemovedEvent RuntimeEventKind = "removed"
	RuntimeAttemptEvent RuntimeEventKind = "attempt"
)

type RuntimeOwnerKind string

const (
	RuntimeSessionOwner RuntimeOwnerKind = "session"
	RuntimeGroupOwner   RuntimeOwnerKind = "group"
	RuntimeOutputOwner  RuntimeOwnerKind = "output"
	RuntimeFFmpegOwner  RuntimeOwnerKind = "ffmpeg"
	RuntimeSystemOwner  RuntimeOwnerKind = "system"
)

// RuntimeEvent содержит только разрешённые типизированные данные; текст ошибки остаётся в наблюдении.
type RuntimeEvent struct {
	Cursor                         RuntimeCursor
	Kind                           RuntimeEventKind
	Owner                          RuntimeOwnerKind
	Session                        RuntimeSessionFence
	Stamp                          ObservationStamp
	Execution                      ExecutionFence
	GroupInstanceID                uint64
	ConfigGroupID, ConfigProfileID ID
	State                          ObservationState
	OutputState                    OutputState
	Source                         ObservationSource
	SourceRevision                 uint64
	Role                           FFmpegRole
	AttemptActive                  bool
	Reason                         MetricReason
	Input                          *InputSnapshot
	Group                          *GroupMetrics
	Output                         *OutputMetrics
	FFmpeg                         *FFmpegMetrics
	GPU                            *GPUSnapshot
	weight                         uint64
}

type RuntimeEventPage struct {
	Events     []RuntimeEvent
	Cursor     RuntimeCursor
	HighWater  RuntimeCursor
	Gap, Reset bool
	Changed    <-chan struct{}
}

type RuntimeAnchor struct {
	Sessions []SessionRuntimeSnapshot
	GPU      *GPUSnapshot
	Cursor   RuntimeCursor
	Changed  <-chan struct{}
}

type eventStream struct {
	limits         EventLimits
	epoch          ID
	sequence       uint64
	missingThrough uint64
	bytes          uint64
	events         []RuntimeEvent
	changed        chan struct{}
	exhausted      bool
}

// ConfigureTelemetry включает наблюдения до запуска пользователей authority.
// Повтор с теми же пределами допустим; менять их после настройки запрещено.
func (a *RuntimeAuthority) ConfigureTelemetry(ctx context.Context, limits TelemetryLimits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	epoch, err := newID()
	if err != nil {
		return err
	}
	if err = a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if a.telemetry != nil {
		if *a.telemetry == limits {
			return nil
		}
		return ErrConflict
	}
	if len(a.observations) != 0 {
		return ErrConflict
	}
	a.telemetry = &limits
	a.events = &eventStream{limits: limits.Events, epoch: epoch, changed: make(chan struct{})}
	return nil
}
func (s *Service) ConfigureTelemetry(ctx context.Context, limits TelemetryLimits) error {
	a, err := s.runtimeAuthority()
	if err != nil {
		return err
	}
	return a.ConfigureTelemetry(ctx, limits)
}
func (a *RuntimeAuthority) cursorLocked() RuntimeCursor {
	if a.events == nil {
		return RuntimeCursor{}
	}
	return RuntimeCursor{Incarnation: a.incarnation, Epoch: a.events.epoch, Sequence: a.events.sequence}
}
func (a *RuntimeAuthority) notifyEventsLocked() {
	close(a.events.changed)
	a.events.changed = make(chan struct{})
}
func ownerKind(k observationKind) RuntimeOwnerKind {
	switch k {
	case observationSession:
		return RuntimeSessionOwner
	case observationGroup:
		return RuntimeGroupOwner
	case observationOutput:
		return RuntimeOutputOwner
	default:
		return RuntimeFFmpegOwner
	}
}
func (a *RuntimeAuthority) nodeEvent(n *observationNode, kind RuntimeEventKind) RuntimeEvent {
	return RuntimeEvent{Kind: kind, Owner: ownerKind(n.kind), Session: n.session, Stamp: n.stamp, Execution: n.execution, GroupInstanceID: n.parent, ConfigGroupID: n.groupID, ConfigProfileID: n.profileID, Role: n.role}
}
func (a *RuntimeAuthority) appendEventLocked(e RuntimeEvent) {
	if a.events == nil {
		return
	}
	s := a.events
	if s.sequence == math.MaxUint64 {
		if !s.exhausted {
			s.exhausted = true
			a.notifyEventsLocked()
		}
		return
	}
	s.sequence++
	e.Cursor = a.cursorLocked()
	weight := uint64(256)
	for _, v := range []string{e.Cursor.Incarnation, string(e.Cursor.Epoch), string(e.Session.SessionID), e.Session.Incarnation, string(e.Execution.CommandID), string(e.Execution.SessionID), string(e.Execution.OutputID), e.Execution.Incarnation, string(e.ConfigGroupID), string(e.ConfigProfileID)} {
		if uint64(len(v)) > math.MaxUint64-weight {
			weight = math.MaxUint64
			break
		}
		weight += uint64(len(v))
	}
	add := func(n uint64) {
		if n > math.MaxUint64-weight {
			weight = math.MaxUint64
		} else {
			weight += n
		}
	}
	if e.Input != nil {
		add(inputTelemetryWeight)
	}
	if e.Group != nil {
		add(groupTelemetryWeight)
	}
	if e.Output != nil {
		add(outputTelemetryWeight)
	}
	if e.FFmpeg != nil {
		add(ffmpegTelemetryWeight)
	}
	if e.GPU != nil {
		for _, d := range e.GPU.Devices {
			add(1024 + uint64(len(d.UUID)))
		}
		add(256)
	}
	if weight > s.limits.MaxEventBytes || weight > s.limits.MaxBytes {
		s.missingThrough = s.sequence
		a.notifyEventsLocked()
		return
	}
	for len(s.events) > 0 && (len(s.events) >= s.limits.MaxEvents || s.bytes > s.limits.MaxBytes-weight) {
		old := s.events[0]
		s.bytes -= old.weight
		if old.Cursor.Sequence > s.missingThrough {
			s.missingThrough = old.Cursor.Sequence
		}
		copy(s.events, s.events[1:])
		s.events[len(s.events)-1] = RuntimeEvent{}
		s.events = s.events[:len(s.events)-1]
	}
	e.weight = weight
	s.events = append(s.events, cloneRuntimeEvent(e))
	s.bytes += weight
	a.notifyEventsLocked()
}
func cloneRuntimeEvent(e RuntimeEvent) RuntimeEvent {
	e.Cursor.Incarnation = strings.Clone(e.Cursor.Incarnation)
	e.Cursor.Epoch = ID(strings.Clone(string(e.Cursor.Epoch)))
	e.Session = cloneRuntimeFence(e.Session)
	e.Execution = cloneExecutionFence(e.Execution)
	e.ConfigGroupID = ID(strings.Clone(string(e.ConfigGroupID)))
	e.ConfigProfileID = ID(strings.Clone(string(e.ConfigProfileID)))
	if e.Input != nil {
		e.Input = cloneInput(e.Input)
	}
	if e.Group != nil {
		v := *e.Group
		e.Group = &v
	}
	if e.Output != nil {
		v := *e.Output
		e.Output = &v
	}
	if e.FFmpeg != nil {
		v := *e.FFmpeg
		e.FFmpeg = &v
	}
	if e.GPU != nil {
		e.GPU = freshGPU(e.GPU, e.GPU.SampledAt)
	}
	return e
}
func (a *RuntimeAuthority) purgeSessionEventsLocked(f RuntimeSessionFence) {
	if a.events == nil {
		return
	}
	s := a.events
	kept := s.events[:0]
	for i := range s.events {
		e := s.events[i]
		if e.Session == f {
			s.bytes -= e.weight
			if e.Cursor.Sequence > s.missingThrough {
				s.missingThrough = e.Cursor.Sequence
			}
		} else {
			kept = append(kept, e)
		}
	}
	clear(s.events[len(kept):])
	s.events = kept
}
func (a *RuntimeAuthority) runtimeEventsLocked(cursor RuntimeCursor, limit int) (RuntimeEventPage, error) {
	s := a.events
	if s == nil {
		return RuntimeEventPage{}, ErrNotReady
	}
	if s.exhausted {
		return RuntimeEventPage{}, ErrEventSequenceExhausted
	}
	if limit <= 0 || limit > s.limits.MaxPageSize {
		return RuntimeEventPage{}, ErrInvalid
	}
	current := a.cursorLocked()
	p := RuntimeEventPage{Cursor: cursor, HighWater: current, Changed: s.changed}
	zero := cursor == (RuntimeCursor{})
	if !zero && (cursor.Incarnation != current.Incarnation || cursor.Epoch != current.Epoch) {
		p.Gap = true
		p.Reset = true
		cursor = RuntimeCursor{}
	}
	if !zero && !p.Reset && cursor.Sequence > current.Sequence {
		return RuntimeEventPage{}, ErrInvalid
	}
	if cursor.Sequence < s.missingThrough {
		p.Gap = true
	}
	p.Cursor = RuntimeCursor{Incarnation: current.Incarnation, Epoch: current.Epoch, Sequence: cursor.Sequence}
	for _, e := range s.events {
		if e.Cursor.Sequence > cursor.Sequence {
			p.Events = append(p.Events, cloneRuntimeEvent(e))
			p.Cursor = e.Cursor
			if len(p.Events) == limit {
				return p, nil
			}
		}
	}
	p.Cursor = current
	return p, nil
}
func (s *Service) RuntimeEvents(ctx context.Context, cursor RuntimeCursor, limit int) (RuntimeEventPage, error) {
	a, err := s.runtimeAuthority()
	if err != nil {
		return RuntimeEventPage{}, err
	}
	return a.RuntimeEvents(ctx, cursor, limit)
}
func (a *RuntimeAuthority) RuntimeEvents(ctx context.Context, cursor RuntimeCursor, limit int) (RuntimeEventPage, error) {
	if err := a.acquire(ctx); err != nil {
		return RuntimeEventPage{}, err
	}
	defer a.release()
	return a.runtimeEventsLocked(cursor, limit)
}
