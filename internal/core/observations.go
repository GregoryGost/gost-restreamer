package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

var ErrObservationLimit = errors.New("исчерпан бюджет оперативных наблюдений")

// ObservationLimits ограничивает число владельцев и логический вес удерживаемых данных.
// Вес узла включает 256 байт и длины строк; при включении телеметрии
// дополнительно резервируются все её типизированные поля. Связь с родителем
// включена в постоянную часть. Это бюджет реестра, а не размер кучи Go.
type ObservationLimits struct {
	MaxSessions        int
	MaxGroups          int
	MaxOutputs         int
	MaxFFmpegWorkers   int
	MaxIdentifierBytes uint64
	MaxErrorBytes      uint64
	MaxBytes           uint64
}

func (l ObservationLimits) Validate() error {
	if l.MaxSessions <= 0 || l.MaxGroups <= 0 || l.MaxOutputs <= 0 || l.MaxFFmpegWorkers <= 0 || l.MaxIdentifierBytes == 0 || l.MaxErrorBytes == 0 || l.MaxBytes < 256 || l.MaxIdentifierBytes > l.MaxBytes || l.MaxErrorBytes > l.MaxBytes {
		return ErrInvalid
	}
	total := uint64(0)
	for _, n := range []int{l.MaxSessions, l.MaxGroups, l.MaxOutputs, l.MaxFFmpegWorkers} {
		if uint64(n) > (math.MaxUint64 - total) {
			return ErrInvalid
		}
		total += uint64(n)
	}
	if l.MaxIdentifierBytes > (math.MaxUint64-288)/8 || l.MaxErrorBytes > math.MaxUint64-288-8*l.MaxIdentifierBytes || total > math.MaxUint64/(288+8*l.MaxIdentifierBytes+l.MaxErrorBytes) {
		return ErrInvalid
	}
	return nil
}

type ObservationState string

const (
	ObservationUnknown  ObservationState = "UNKNOWN"
	ObservationStarting ObservationState = "STARTING"
	ObservationReady    ObservationState = "READY"
	ObservationRetrying ObservationState = "RETRYING"
	ObservationFailed   ObservationState = "FAILED"
	ObservationStopped  ObservationState = "STOPPED"
)

func (s ObservationState) valid() bool {
	switch s {
	case ObservationUnknown, ObservationStarting, ObservationReady, ObservationRetrying, ObservationFailed, ObservationStopped:
		return true
	}
	return false
}

type ObservationSource string

const (
	ObservationSourceNone     ObservationSource = "NONE"
	ObservationSourceLive     ObservationSource = "LIVE"
	ObservationSourceFallback ObservationSource = "FALLBACK"
)

type SessionObservation struct {
	State ObservationState
	Error string
}
type GroupObservation struct {
	SourceRevision uint64
	State          ObservationState
	Source         ObservationSource
	Error          string
}
type OutputObservation struct {
	State OutputState
	Error string
}
type FFmpegObservation struct {
	State ObservationState
	Error string
}
type ObservationStamp struct {
	Token      uint64
	InstanceID uint64
	Revision   uint64
	ObservedAt time.Time
	AttemptID  uint64
}
type SessionObservationSnapshot struct {
	Stamp ObservationStamp
	Value *SessionObservation
}
type GroupObservationSnapshot struct {
	Metrics         *GroupMetrics
	MetricsStamp    ObservationStamp
	Stamp           ObservationStamp
	ConfigGroupID   ID
	ConfigProfileID ID
	Value           *GroupObservation
}
type OutputObservationSnapshot struct {
	Metrics      *OutputMetrics
	MetricsStamp ObservationStamp
	Stamp        ObservationStamp
	Execution    ExecutionFence
	Value        *OutputObservation
}
type FFmpegObservationSnapshot struct {
	Metrics         *FFmpegMetrics
	MetricsStamp    ObservationStamp
	Role            FFmpegRole
	Stamp           ObservationStamp
	GroupInstanceID uint64
	Value           *FFmpegObservation
}
type SessionRuntimeSnapshot struct {
	Fence       RuntimeSessionFence
	Input       *InputSnapshot
	Cursor      RuntimeCursor
	CursorValid bool
	Session     *SessionObservationSnapshot
	Groups      []GroupObservationSnapshot
	Outputs     []OutputObservationSnapshot
	FFmpeg      []FFmpegObservationSnapshot
}

type observationKind uint8

const (
	observationSession observationKind = iota
	observationGroup
	observationOutput
	observationFFmpeg
)

type observationNode struct {
	authority      *RuntimeAuthority
	token          uint64
	parent         uint64
	kind           observationKind
	session        RuntimeSessionFence
	execution      ExecutionFence
	lease          *RuntimeLease
	groupID        ID
	profileID      ID
	stamp          ObservationStamp
	state          string
	source         ObservationSource
	failure        string
	present        bool
	weight         uint64
	valid          atomic.Bool
	done           chan struct{}
	attemptDone    chan struct{}
	sourceRevision uint64
	input          *InputSnapshot
	groupMetrics   *GroupMetrics
	outputMetrics  *OutputMetrics
	ffmpegMetrics  *FFmpegMetrics
	metricsStamp   ObservationStamp
	role           FFmpegRole
	sampleTimes    [3]time.Time
	progressTime   time.Time
}
type observerHandle struct{ node *observationNode }
type SessionObserver struct{ observerHandle }
type GroupObserver struct{ observerHandle }
type OutputObserver struct{ observerHandle }
type FFmpegObserver struct{ observerHandle }
type FFmpegAttemptObserver struct {
	observerHandle
	attemptID uint64
	done      <-chan struct{}
}

func (h observerHandle) Token() uint64 {
	if h.node == nil {
		return 0
	}
	return h.node.token
}
func (h observerHandle) Valid() bool {
	return h.node != nil && h.node.valid.Load() && h.node.lease.Valid()
}
func (h observerHandle) Done() <-chan struct{} {
	if h.node == nil {
		return nil
	}
	return h.node.done
}
func (h observerHandle) Close(ctx context.Context) error {
	if h.node == nil {
		return ErrInvalid
	}
	a := h.node.authority
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	a.removeObservation(h.node)
	return nil
}
func (o *OutputObserver) ExecutionFence() ExecutionFence {
	if o == nil || o.node == nil {
		return ExecutionFence{}
	}
	return o.node.execution
}
func (o *FFmpegAttemptObserver) Valid() bool {
	if o == nil || !o.observerHandle.Valid() {
		return false
	}
	select {
	case <-o.done:
		return false
	default:
		return true
	}
}
func (o *FFmpegAttemptObserver) Done() <-chan struct{} {
	if o == nil {
		return nil
	}
	return o.done
}
func (o *FFmpegAttemptObserver) Close(ctx context.Context) error {
	if o == nil || o.node == nil {
		return ErrInvalid
	}
	a := o.node.authority
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if err := o.current(); err != nil {
		return err
	}
	close(o.node.attemptDone)
	o.node.attemptDone = nil
	o.node.sampleTimes = [3]time.Time{}
	o.node.progressTime = time.Time{}
	o.node.ffmpegMetrics = nil
	o.node.metricsStamp = ObservationStamp{}
	o.node.present = false
	o.node.state = ""
	o.node.failure = ""
	o.node.stamp.ObservedAt = time.Time{}
	o.node.weight, _ = a.observationWeight(o.node)
	a.appendEventLocked(a.nodeEvent(o.node, RuntimeAttemptEvent))
	return nil
}
func (o *FFmpegAttemptObserver) current() error {
	if !o.observerHandle.Valid() || o.node.stamp.AttemptID != o.attemptID || o.node.attemptDone == nil {
		return ErrStale
	}
	return nil
}

func cloneRuntimeFence(f RuntimeSessionFence) RuntimeSessionFence {
	f.SessionID = ID(strings.Clone(string(f.SessionID)))
	f.Incarnation = strings.Clone(f.Incarnation)
	return f
}
func cloneExecutionFence(f ExecutionFence) ExecutionFence {
	f.CommandID = ID(strings.Clone(string(f.CommandID)))
	f.SessionID = ID(strings.Clone(string(f.SessionID)))
	f.OutputID = ID(strings.Clone(string(f.OutputID)))
	f.Incarnation = strings.Clone(f.Incarnation)
	return f
}
func (n *observationNode) strings() []string {
	return []string{string(n.session.SessionID), n.session.Incarnation, string(n.execution.CommandID), string(n.execution.SessionID), string(n.execution.OutputID), n.execution.Incarnation, string(n.groupID), string(n.profileID), n.state, string(n.source), n.failure}
}
func (a *RuntimeAuthority) observationWeight(n *observationNode) (uint64, error) {
	weight := uint64(256)
	if a.telemetry != nil {
		weight += telemetryNodeWeight(n.kind)
	}
	for i, s := range n.strings() {
		if i < 8 && uint64(len(s)) > a.limits.MaxIdentifierBytes {
			return 0, ErrObservationLimit
		}
		if uint64(len(s)) > math.MaxUint64-weight {
			return 0, ErrObservationLimit
		}
		weight += uint64(len(s))
	}
	if uint64(len(n.failure)) > a.limits.MaxErrorBytes {
		return 0, ErrObservationLimit
	}
	return weight, nil
}
func (a *RuntimeAuthority) budgetObservation(candidate *observationNode, removed map[uint64]bool) error {
	weight, err := a.observationWeight(candidate)
	if err != nil {
		return err
	}
	counts := [4]int{}
	sessions := map[RuntimeSessionFence]bool{}
	counts[candidate.kind]++
	sessions[candidate.session] = true
	for token, n := range a.observations {
		if removed[token] {
			continue
		}
		if n.weight > math.MaxUint64-weight {
			return ErrObservationLimit
		}
		weight += n.weight
		counts[n.kind]++
		sessions[n.session] = true
	}
	if len(sessions) > a.limits.MaxSessions || counts[observationSession] > a.limits.MaxSessions || counts[observationGroup] > a.limits.MaxGroups || counts[observationOutput] > a.limits.MaxOutputs || counts[observationFFmpeg] > a.limits.MaxFFmpegWorkers || weight > a.limits.MaxBytes {
		return ErrObservationLimit
	}
	return nil
}
func (a *RuntimeAuthority) observationSubtree(n *observationNode) map[uint64]bool {
	removed := map[uint64]bool{n.token: true}
	if n.kind == observationSession {
		for token, other := range a.observations {
			if other.session == n.session {
				removed[token] = true
			}
		}
		return removed
	}
	for changed := true; changed; {
		changed = false
		for token, other := range a.observations {
			if !removed[token] && removed[other.parent] {
				removed[token] = true
				changed = true
			}
		}
	}
	return removed
}
func (a *RuntimeAuthority) removeObservation(n *observationNode) {
	if n == nil || a.observations[n.token] != n {
		return
	}
	if n.kind == observationSession {
		a.purgeSessionEventsLocked(n.session)
	}
	for token := range a.observationSubtree(n) {
		other := a.observations[token]
		if other == nil {
			continue
		}
		for _, reservation := range a.outputExecutions {
			if other.kind == observationOutput && reservation.fence == other.execution {
				reservation.sealLocked()
			}
		}
		delete(a.observations, token)
		other.valid.Store(false)
		close(other.done)
		if other.attemptDone != nil {
			close(other.attemptDone)
			other.attemptDone = nil
		}
		other.failure = ""
		other.state = ""
		other.present = false
		other.input = nil
		other.groupMetrics = nil
		other.outputMetrics = nil
		other.ffmpegMetrics = nil
		other.metricsStamp = ObservationStamp{}
		other.sampleTimes = [3]time.Time{}
		other.progressTime = time.Time{}
	}
	e := a.nodeEvent(n, RuntimeRemovedEvent)
	e.Stamp.ObservedAt = time.Now().UTC()
	a.appendEventLocked(e)
}
func (a *RuntimeAuthority) registerObservation(n *observationNode, replace *observationNode) (*observationNode, error) {
	removed := map[uint64]bool{}
	if replace != nil {
		removed = a.observationSubtree(replace)
	}
	if err := a.budgetObservation(n, removed); err != nil {
		return nil, err
	}
	if a.nextObservation == math.MaxUint64 {
		return nil, fmt.Errorf("%w: исчерпаны идентификаторы наблюдений", ErrConflict)
	}
	a.nextObservation++
	n.token = a.nextObservation
	n.authority = a
	n.stamp = ObservationStamp{Token: n.token, InstanceID: n.token}
	n.done = make(chan struct{})
	n.session = cloneRuntimeFence(n.session)
	n.execution = cloneExecutionFence(n.execution)
	n.groupID = ID(strings.Clone(string(n.groupID)))
	n.profileID = ID(strings.Clone(string(n.profileID)))
	n.weight, _ = a.observationWeight(n)
	if replace != nil {
		a.removeObservation(replace)
	}
	n.valid.Store(true)
	a.observations[n.token] = n
	if a.telemetry != nil && n.kind == observationSession {
		n.input = &InputSnapshot{}
		for _, publication := range a.publications {
			if publication.session == n.session && publication.Valid() {
				a.connectInputNodeLocked(n, publication)
			}
		}
	}
	return n, nil
}
func (s *Service) openObserver(ctx context.Context, f ExecutionFence, kind observationKind) (*observationNode, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if (kind == observationSession) != (f.OutputID == "") {
		return nil, ErrInvalid
	}
	a, err := s.runtimeAuthority()
	if err != nil {
		return nil, err
	}
	if err = a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	// Проверка длины предшествует копированию идентификаторов и созданию допуска.
	candidate := &observationNode{kind: kind, session: RuntimeSessionFence{f.SessionID, f.Incarnation, f.Lifecycle}, execution: f}
	if _, err = a.observationWeight(candidate); err != nil {
		return nil, err
	}
	lease := &RuntimeLease{authority: a, session: candidate.session, execution: f, done: make(chan struct{})}
	err = s.uow.View(ctx, func(r Repositories) error {
		if err := lease.authorized(ctx, r); err != nil {
			return err
		}
		if kind == observationOutput {
			c, err := r.Runtime.ReceiptByID(ctx, f.CommandID)
			if err != nil {
				return err
			}
			if c.Status != CommandSucceeded {
				return ErrStale
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var replace *observationNode
	for _, n := range a.observations {
		if n.kind == kind && n.session == candidate.session && (kind == observationSession || n.execution.OutputID == f.OutputID) {
			replace = n
		}
	}
	for _, existing := range a.leases {
		if existing.execution == f {
			lease = existing
			break
		}
	}
	if !lease.valid.Load() {
		id, e := newID()
		if e != nil {
			return nil, e
		}
		lease.id = id
	}
	candidate.lease = lease
	node, err := a.registerObservation(candidate, replace)
	if err != nil {
		return nil, err
	}
	if !lease.valid.Load() {
		lease.session = node.session
		lease.execution = node.execution
		lease.valid.Store(true)
		a.leases[lease.id] = lease
	}
	return node, nil
}
func (s *Service) OpenSessionObserver(ctx context.Context, f ExecutionFence) (*SessionObserver, error) {
	n, err := s.openObserver(ctx, f, observationSession)
	if err != nil {
		return nil, err
	}
	return &SessionObserver{observerHandle{n}}, nil
}
func (s *Service) OpenOutputObserver(ctx context.Context, f ExecutionFence) (*OutputObserver, error) {
	n, err := s.openObserver(ctx, f, observationOutput)
	if err != nil {
		return nil, err
	}
	return &OutputObserver{observerHandle{n}}, nil
}
func (h observerHandle) child(ctx context.Context, kind observationKind, group, profile ID, role FFmpegRole) (*observationNode, error) {
	if h.node == nil {
		return nil, ErrInvalid
	}
	a := h.node.authority
	if err := a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	if !h.Valid() {
		return nil, ErrStale
	}
	return a.registerObservation(&observationNode{kind: kind, parent: h.node.token, session: h.node.session, lease: h.node.lease, groupID: group, profileID: profile, role: role}, nil)
}
func (o *SessionObserver) OpenGroup(ctx context.Context, group, profile ID) (*GroupObserver, error) {
	if o == nil {
		return nil, ErrInvalid
	}
	if err := group.Validate(); err != nil {
		return nil, err
	}
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	n, err := o.child(ctx, observationGroup, group, profile, "")
	if err != nil {
		return nil, err
	}
	return &GroupObserver{observerHandle{n}}, nil
}
func (o *GroupObserver) OpenFFmpeg(ctx context.Context) (*FFmpegObserver, error) {
	if o == nil {
		return nil, ErrInvalid
	}
	n, err := o.child(ctx, observationFFmpeg, "", "", "")
	if err != nil {
		return nil, err
	}
	return &FFmpegObserver{observerHandle{n}}, nil
}
func (o *FFmpegObserver) BeginAttempt(ctx context.Context, attemptID uint64) (*FFmpegAttemptObserver, error) {
	if o == nil || o.node == nil || attemptID == 0 {
		return nil, ErrInvalid
	}
	a := o.node.authority
	if err := a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	if !o.Valid() || attemptID <= o.node.stamp.AttemptID {
		return nil, ErrStale
	}
	if o.node.attemptDone != nil {
		close(o.node.attemptDone)
	}
	o.node.attemptDone = make(chan struct{})
	o.node.stamp.AttemptID = attemptID
	o.node.stamp.ObservedAt = time.Time{}
	o.node.sampleTimes = [3]time.Time{}
	o.node.progressTime = time.Time{}
	o.node.ffmpegMetrics = nil
	o.node.metricsStamp = ObservationStamp{}
	o.node.present = false
	o.node.failure = ""
	o.node.state = ""
	o.node.weight, _ = a.observationWeight(o.node)
	e := a.nodeEvent(o.node, RuntimeAttemptEvent)
	e.AttemptActive = true
	a.appendEventLocked(e)
	return &FFmpegAttemptObserver{observerHandle: o.observerHandle, attemptID: attemptID, done: o.node.attemptDone}, nil
}
func (h observerHandle) observe(ctx context.Context, state string, source ObservationSource, failure string, attempt uint64, sourceRevision uint64) error {
	if h.node == nil {
		return ErrInvalid
	}
	a := h.node.authority
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if !h.Valid() || (h.node.kind == observationFFmpeg && (h.node.stamp.AttemptID != attempt || h.node.attemptDone == nil)) {
		return ErrStale
	}
	if (sourceRevision > 0 && sourceRevision <= h.node.sourceRevision) || (h.node.kind == observationGroup && h.node.sourceRevision > 0 && sourceRevision == 0) {
		return ErrStale
	}
	if !utf8.ValidString(failure) || strings.ContainsRune(failure, 0) {
		return ErrInvalid
	}
	n := h.node
	changed := !n.present || n.state != state || n.source != source || n.failure != failure
	candidate := &observationNode{kind: n.kind, session: n.session, execution: n.execution, groupID: n.groupID, profileID: n.profileID, state: state, source: source, failure: failure}
	if err := a.budgetObservation(candidate, map[uint64]bool{n.token: true}); err != nil {
		return err
	}
	revision, err := NextRevision(n.stamp.Revision)
	if err != nil {
		return err
	}
	n.state = strings.Clone(state)
	n.source = ObservationSource(strings.Clone(string(source)))
	n.failure = strings.Clone(failure)
	n.present = true
	n.stamp.Revision = revision
	n.stamp.ObservedAt = time.Now().UTC()
	if sourceRevision > 0 {
		n.sourceRevision = sourceRevision
	}
	if changed {
		e := a.nodeEvent(n, RuntimeStateEvent)
		e.Source = n.source
		e.SourceRevision = n.sourceRevision
		if n.kind == observationOutput {
			e.OutputState = OutputState(n.state)
		} else {
			e.State = ObservationState(n.state)
		}
		if failure != "" {
			e.Reason = ReasonObservationFailed
		}
		a.appendEventLocked(e)
	}
	n.weight, _ = a.observationWeight(n)
	return nil
}
func (o *SessionObserver) Observe(ctx context.Context, v SessionObservation) error {
	if o == nil || !v.State.valid() {
		return ErrInvalid
	}
	return o.observe(ctx, string(v.State), "", v.Error, 0, 0)
}
func (o *GroupObserver) Observe(ctx context.Context, v GroupObservation) error {
	if o == nil || !v.State.valid() || (v.Source != ObservationSourceNone && v.Source != ObservationSourceLive && v.Source != ObservationSourceFallback) {
		return ErrInvalid
	}
	return o.observe(ctx, string(v.State), v.Source, v.Error, 0, v.SourceRevision)
}
func (o *OutputObserver) Observe(ctx context.Context, v OutputObservation) error {
	if o == nil || (v.State != OutputConnecting && v.State != OutputPublishing && v.State != OutputRetrying) {
		return ErrInvalid
	}
	return o.observe(ctx, string(v.State), "", v.Error, 0, 0)
}

// ObserveTerminal публикует терминальное RAM-состояние, не освобождая физическое выполнение.
// Долговечный отказ и очистка остаются отдельными операциями владельца.
func (o *OutputObserver) ObserveTerminal(ctx context.Context, v OutputObservation) error {
	if o == nil || (v.State != OutputFailed && v.State != OutputStopped) {
		return ErrInvalid
	}
	return o.observe(ctx, string(v.State), "", v.Error, 0, 0)
}
func (o *FFmpegAttemptObserver) Observe(ctx context.Context, v FFmpegObservation) error {
	if o == nil || !v.State.valid() {
		return ErrInvalid
	}
	return o.observe(ctx, string(v.State), "", v.Error, o.attemptID, 0)
}

func (s *Service) RuntimeSnapshot(ctx context.Context, f RuntimeSessionFence) (SessionRuntimeSnapshot, error) {
	a, err := s.runtimeAuthority()
	if err != nil {
		return SessionRuntimeSnapshot{}, err
	}
	if err = a.acquire(ctx); err != nil {
		return SessionRuntimeSnapshot{}, err
	}
	defer a.release()
	return a.runtimeSnapshotLocked(f)
}
func (a *RuntimeAuthority) runtimeSnapshotLocked(f RuntimeSessionFence) (SessionRuntimeSnapshot, error) {
	result := SessionRuntimeSnapshot{Cursor: a.cursorLocked(), CursorValid: a.events != nil && !a.events.exhausted}
	tokens := make([]uint64, 0)
	for token, n := range a.observations {
		if n.session == f {
			tokens = append(tokens, token)
		}
	}
	if len(tokens) == 0 {
		return SessionRuntimeSnapshot{}, ErrNotReady
	}
	result.Fence = cloneRuntimeFence(f)
	sort.Slice(tokens, func(i, j int) bool { return tokens[i] < tokens[j] })
	for _, token := range tokens {
		n := a.observations[token]
		switch n.kind {
		case observationSession:
			v := SessionObservationSnapshot{Stamp: n.stamp}
			if n.present {
				v.Value = &SessionObservation{State: ObservationState(n.state), Error: strings.Clone(n.failure)}
			}
			result.Input = cloneInput(n.input)
			if result.Input != nil && result.Input.Metrics != nil {
				result.Input.Metrics.MediaMetrics = a.freshMediaLocked(result.Input.Metrics.MediaMetrics, n.sampleTimes[0])
			}
			result.Session = &v
		case observationGroup:
			v := GroupObservationSnapshot{Stamp: n.stamp, ConfigGroupID: ID(strings.Clone(string(n.groupID))), ConfigProfileID: ID(strings.Clone(string(n.profileID)))}
			if n.present {
				v.Value = &GroupObservation{State: ObservationState(n.state), Source: n.source, SourceRevision: n.sourceRevision, Error: strings.Clone(n.failure)}
			}
			if n.groupMetrics != nil {
				m := *n.groupMetrics
				m.LiveInput = a.freshMediaLocked(m.LiveInput, n.sampleTimes[0])
				m.FallbackInput = a.freshMediaLocked(m.FallbackInput, n.sampleTimes[1])
				m.Output = a.freshMediaLocked(m.Output, n.sampleTimes[2])
				v.Metrics = &m
			}
			v.MetricsStamp = n.metricsStamp
			result.Groups = append(result.Groups, v)
		case observationOutput:
			v := OutputObservationSnapshot{Stamp: n.stamp, Execution: cloneExecutionFence(n.execution)}
			if n.present {
				v.Value = &OutputObservation{State: OutputState(n.state), Error: strings.Clone(n.failure)}
			}
			if n.outputMetrics != nil {
				m := *n.outputMetrics
				m.MediaMetrics = a.freshMediaLocked(m.MediaMetrics, n.sampleTimes[0])
				v.Metrics = &m
			}
			v.MetricsStamp = n.metricsStamp
			result.Outputs = append(result.Outputs, v)
		case observationFFmpeg:
			v := FFmpegObservationSnapshot{Stamp: n.stamp, GroupInstanceID: n.parent}
			if n.present {
				v.Value = &FFmpegObservation{State: ObservationState(n.state), Error: strings.Clone(n.failure)}
			}
			if n.ffmpegMetrics != nil {
				m := *n.ffmpegMetrics
				if a.telemetry != nil && m.HasProgress && time.Since(n.progressTime) >= a.telemetry.MediaStaleAfter {
					m.Status = MetricStale
					m.Reason = ReasonStale
				}
				v.Metrics = &m
			}
			v.MetricsStamp = n.metricsStamp
			v.Role = n.role
			result.FFmpeg = append(result.FFmpeg, v)
		}
	}
	return result, nil
}

// RuntimeOwnedUnitOfWork удерживает точный допуск наблюдателя до завершения записи и сверки.
type RuntimeOwnedUnitOfWork interface {
	WriteOwned(context.Context, *OutputObserver, func(Repositories) error) error
}

func (a *RuntimeAuthority) GuardOwnedWrite(ctx context.Context, uow UnitOfWork, owner *OutputObserver, write func() (bool, error)) error {
	if owner == nil || owner.node == nil || owner.node.authority != a || write == nil || uow == nil {
		return ErrInvalid
	}
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if !owner.Valid() || a.observations[owner.node.token] != owner.node {
		return ErrStale
	}
	uncertain := true
	defer func() {
		if uncertain {
			a.reconcile(ctx, uow)
		}
	}()
	var err error
	uncertain, err = write()
	return err
}
func (s *Service) FailOutput(ctx context.Context, owner *OutputObserver, reason string) error {
	if owner == nil || owner.node == nil {
		return ErrInvalid
	}
	if err := validText("reason", reason); err != nil {
		return err
	}
	a, err := s.runtimeAuthority()
	if err != nil {
		return err
	}
	if owner.node.authority != a {
		return ErrStale
	}
	uow, ok := s.uow.(RuntimeOwnedUnitOfWork)
	if !ok {
		return ErrRuntimeCapability
	}
	f := owner.ExecutionFence()
	return uow.WriteOwned(ctx, owner, func(r Repositories) error {
		c, session, o, err := s.fenced(ctx, r, f)
		if err != nil {
			return err
		}
		if o == nil || c.Status != CommandSucceeded || (c.Kind != CommandStartOutput && c.Kind != CommandAddOutput) || session.State != SessionRunning || !o.Attached || !o.DesiredRunning || !o.Issued || o.CleanupPending || o.Attempt != f.Attempt {
			return ErrStale
		}
		old := o.Revision
		if err = stopOutput(ctx, r, session, o, false); err != nil {
			return err
		}
		if err = cancelCommands(ctx, r, session.ID, o.ID, c.ID, false); err != nil {
			return err
		}
		o.Record, err = revise(o.Record)
		if err != nil {
			return err
		}
		if err = r.Runtime.UpdateOutput(ctx, *o, old); err != nil {
			return err
		}
		if o.CleanupPending && !session.CleanupPending {
			old = session.Revision
			session.CleanupPending = true
			session.Record, err = revise(session.Record)
			if err != nil {
				return err
			}
			if err = r.Runtime.UpdateSession(ctx, session, old); err != nil {
				return err
			}
		}
		id, err := newID()
		if err != nil {
			return err
		}
		execution := f
		return r.Runtime.InsertEvent(ctx, Event{ID: id, SessionID: session.ID, OutputID: o.ID, Kind: "output_execution_failed", Message: reason, Execution: &execution, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)})
	})
}

// SessionFence возвращает неизменяемого владельца родительского запуска.
func (o *SessionObserver) SessionFence() RuntimeSessionFence {
	if o == nil || o.node == nil {
		return RuntimeSessionFence{}
	}
	return o.node.session
}
