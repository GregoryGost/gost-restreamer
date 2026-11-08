package core

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Постоянные веса учитывают все типизированные поля дополнительно к старому узлу.
// Это логический резерв реестра; он не описывает размер кучи Go либо RSS процесса.
const (
	inputTelemetryWeight  uint64 = 1024
	groupTelemetryWeight  uint64 = 3072
	outputTelemetryWeight uint64 = 1536
	ffmpegTelemetryWeight uint64 = 512
)

func telemetryNodeWeight(k observationKind) uint64 {
	switch k {
	case observationSession:
		return inputTelemetryWeight
	case observationGroup:
		return groupTelemetryWeight
	case observationOutput:
		return outputTelemetryWeight
	default:
		return ffmpegTelemetryWeight
	}
}

type FFmpegRole string

const (
	FFmpegLiveTranscode  FFmpegRole = "live_transcode"
	FFmpegFallbackPlayer FFmpegRole = "fallback_player"
)

func (o *GroupObserver) OpenFFmpegRole(ctx context.Context, role FFmpegRole) (*FFmpegObserver, error) {
	if o == nil || (role != FFmpegLiveTranscode && role != FFmpegFallbackPlayer) {
		return nil, ErrInvalid
	}
	n, err := o.child(ctx, observationFFmpeg, "", "", role)
	if err != nil {
		return nil, err
	}
	return &FFmpegObserver{observerHandle{n}}, nil
}
func cloneInput(p *InputSnapshot) *InputSnapshot {
	if p == nil {
		return nil
	}
	v := *p
	v.LeaseID = ID(strings.Clone(string(v.LeaseID)))
	if v.Metrics != nil {
		m := *v.Metrics
		v.Metrics = &m
	}
	return &v
}
func (a *RuntimeAuthority) connectInputNodeLocked(n *observationNode, l *RuntimeLease) {
	if n.input == nil {
		return
	}
	v := InputSnapshot{LeaseID: l.id, Connected: true, ChangedAt: time.Now().UTC()}
	v.Stamp = ObservationStamp{Token: n.token, InstanceID: n.token, ObservedAt: v.ChangedAt}
	n.metricsStamp = ObservationStamp{}
	n.input = &v
	n.sampleTimes = [3]time.Time{}
	e := a.nodeEvent(n, RuntimeInputEvent)
	e.Input = n.input
	e.Stamp = n.input.Stamp
	a.appendEventLocked(e)
}
func (a *RuntimeAuthority) inputConnectedLocked(l *RuntimeLease) {
	for _, n := range a.observations {
		if n.kind == observationSession && n.session == l.session {
			a.connectInputNodeLocked(n, l)
		}
	}
}
func (a *RuntimeAuthority) inputDisconnectedLocked(l *RuntimeLease) {
	for _, n := range a.observations {
		if n.kind == observationSession && n.session == l.session && n.input != nil && n.input.LeaseID == l.id {
			n.input = &InputSnapshot{LeaseID: l.id, ChangedAt: time.Now().UTC()}
			n.input.Stamp = ObservationStamp{Token: n.token, InstanceID: n.token, ObservedAt: n.input.ChangedAt}
			e := a.nodeEvent(n, RuntimeInputEvent)
			e.Input = n.input
			e.Reason = ReasonDisconnected
			a.appendEventLocked(e)
		}
	}
}
func (h observerHandle) metricsLocked(attempt uint64) error {
	if !h.Valid() || h.node.authority.observations[h.node.token] != h.node || (h.node.kind == observationFFmpeg && (h.node.stamp.AttemptID != attempt || h.node.attemptDone == nil)) {
		return ErrStale
	}
	if h.node.authority.telemetry == nil {
		return ErrNotReady
	}
	return nil
}
func (n *observationNode) nextMetricsStamp(sampled time.Time) (ObservationStamp, error) {
	revision, err := NextRevision(n.metricsStamp.Revision)
	if err != nil {
		return ObservationStamp{}, err
	}
	return ObservationStamp{Token: n.token, InstanceID: n.token, Revision: revision, AttemptID: n.stamp.AttemptID, ObservedAt: sampled.UTC()}, nil
}
func (o *SessionObserver) ObserveInput(ctx context.Context, leaseID ID, v InputMetrics) error {
	if o == nil || o.node == nil || leaseID == "" || !v.valid() {
		return ErrInvalid
	}
	a := o.node.authority
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if err := o.metricsLocked(0); err != nil {
		return err
	}
	n := o.node
	if n.input == nil || !n.input.Connected || n.input.LeaseID != leaseID {
		return ErrStale
	}
	current := false
	for _, l := range a.publications {
		if l.id == leaseID && l.session == n.session && l.Valid() {
			current = true
			break
		}
	}
	if !current {
		return ErrStale
	}
	if n.input.Metrics != nil && !v.SampledAt.After(n.sampleTimes[0]) {
		return ErrStale
	}
	stamp, err := n.nextMetricsStamp(v.SampledAt)
	if err != nil {
		return err
	}
	n.metricsStamp = stamp
	n.sampleTimes[0] = v.SampledAt
	v.MediaMetrics = utcMedia(v.MediaMetrics)
	p := *n.input
	p.Stamp = stamp
	p.Metrics = &v
	n.input = &p
	e := a.nodeEvent(n, RuntimeMetricsEvent)
	e.Input = n.input
	e.Stamp = n.input.Stamp
	a.appendEventLocked(e)
	return nil
}
func (o *GroupObserver) ObserveMetrics(ctx context.Context, v GroupMetrics) error {
	if o == nil || o.node == nil || !v.LiveInput.valid() || !v.FallbackInput.valid() || !v.Output.valid() {
		return ErrInvalid
	}
	a := o.node.authority
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if err := o.metricsLocked(0); err != nil {
		return err
	}
	n := o.node
	if n.groupMetrics != nil && (!v.LiveInput.SampledAt.After(n.sampleTimes[0]) || !v.FallbackInput.SampledAt.After(n.sampleTimes[1]) || !v.Output.SampledAt.After(n.sampleTimes[2])) {
		return ErrStale
	}
	stamp, err := n.nextMetricsStamp(v.Output.SampledAt)
	if err != nil {
		return err
	}
	n.sampleTimes = [3]time.Time{v.LiveInput.SampledAt, v.FallbackInput.SampledAt, v.Output.SampledAt}
	v.LiveInput = utcMedia(v.LiveInput)
	v.FallbackInput = utcMedia(v.FallbackInput)
	v.Output = utcMedia(v.Output)
	n.groupMetrics = &v
	n.metricsStamp = stamp
	e := a.nodeEvent(n, RuntimeMetricsEvent)
	e.Stamp = stamp
	e.Group = &v
	a.appendEventLocked(e)
	return nil
}
func (o *OutputObserver) ObserveMetrics(ctx context.Context, v OutputMetrics) error {
	if o == nil || o.node == nil || !v.valid() || v.QueuePackets > v.QueueCapacity || v.QueueBytes > v.QueueByteCapacity {
		return ErrInvalid
	}
	a := o.node.authority
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if err := o.metricsLocked(0); err != nil {
		return err
	}
	n := o.node
	if n.outputMetrics != nil && !v.SampledAt.After(n.sampleTimes[0]) {
		return ErrStale
	}
	stamp, err := n.nextMetricsStamp(v.SampledAt)
	if err != nil {
		return err
	}
	n.sampleTimes[0] = v.SampledAt
	v.MediaMetrics = utcMedia(v.MediaMetrics)
	v.LastWrite = v.LastWrite.UTC()
	v.LastMedia = v.LastMedia.UTC()
	v.AckLastAnomalyAt = v.AckLastAnomalyAt.UTC()
	n.outputMetrics = &v
	n.metricsStamp = stamp
	e := a.nodeEvent(n, RuntimeMetricsEvent)
	e.Stamp = stamp
	e.Output = &v
	a.appendEventLocked(e)
	return nil
}
func (o *FFmpegAttemptObserver) ObserveMetrics(ctx context.Context, v FFmpegMetrics) error {
	if o == nil || o.node == nil || !v.valid() {
		return ErrInvalid
	}
	a := o.node.authority
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if err := o.metricsLocked(o.attemptID); err != nil {
		return err
	}
	n := o.node
	if n.ffmpegMetrics != nil && !v.SampledAt.After(n.sampleTimes[0]) {
		return ErrStale
	}
	stamp, err := n.nextMetricsStamp(v.SampledAt)
	if err != nil {
		return err
	}
	n.sampleTimes[0] = v.SampledAt
	v.SampledAt = v.SampledAt.UTC()
	n.progressTime = v.LastProgressAt
	v.LastProgressAt = v.LastProgressAt.UTC()
	n.ffmpegMetrics = &v
	n.metricsStamp = stamp
	e := a.nodeEvent(n, RuntimeMetricsEvent)
	e.Stamp = stamp
	e.FFmpeg = &v
	a.appendEventLocked(e)
	return nil
}

// SystemObserver владеет единственным процессным снимком GPU независимо от Session.
type SystemObserver struct {
	authority *RuntimeAuthority
	token     uint64
	valid     atomic.Bool
	done      chan struct{}
	value     *GPUSnapshot
	revision  uint64
	sampledAt time.Time
}

func (a *RuntimeAuthority) OpenSystemObserver(ctx context.Context) (*SystemObserver, error) {
	if err := a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	if a.telemetry == nil {
		return nil, ErrNotReady
	}
	if a.system != nil {
		return nil, ErrConflict
	}
	if a.nextSystem == math.MaxUint64 {
		return nil, ErrConflict
	}
	a.nextSystem++
	o := &SystemObserver{authority: a, token: a.nextSystem, done: make(chan struct{})}
	o.valid.Store(true)
	a.system = o
	return o, nil
}
func (s *Service) OpenSystemObserver(ctx context.Context) (*SystemObserver, error) {
	a, err := s.runtimeAuthority()
	if err != nil {
		return nil, err
	}
	return a.OpenSystemObserver(ctx)
}
func (o *SystemObserver) Token() uint64 {
	if o == nil {
		return 0
	}
	return o.token
}
func (o *SystemObserver) Valid() bool { return o != nil && o.valid.Load() }
func (o *SystemObserver) Done() <-chan struct{} {
	if o == nil {
		return nil
	}
	return o.done
}
func (o *SystemObserver) Close(ctx context.Context) error {
	if o == nil {
		return ErrInvalid
	}
	a := o.authority
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	o.closeLocked()
	return nil
}
func (o *SystemObserver) closeLocked() {
	if !o.valid.Swap(false) {
		return
	}
	o.value = nil
	if o.authority.system == o {
		o.authority.system = nil
	}
	close(o.done)
	o.authority.appendEventLocked(RuntimeEvent{Kind: RuntimeRemovedEvent, Owner: RuntimeSystemOwner, Stamp: ObservationStamp{Token: o.token, InstanceID: o.token}})
}
func cloneGPU(p *GPUSnapshot) *GPUSnapshot {
	if p == nil {
		return nil
	}
	v := *p
	v.Devices = make([]GPUDevice, len(p.Devices))
	copy(v.Devices, p.Devices)
	for i := range v.Devices {
		v.Devices[i].UUID = strings.Clone(v.Devices[i].UUID)
	}
	return &v
}
func (o *SystemObserver) ObserveGPU(ctx context.Context, v GPUSnapshot) error {
	if o == nil || v.SampledAt.IsZero() || v.StaleAfter <= 0 || !v.Status.valid() || !v.Reason.valid() {
		return ErrInvalid
	}
	a := o.authority
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if !o.valid.Load() || a.system != o {
		return ErrStale
	}
	l := a.telemetry
	if len(v.Devices) > l.MaxGPUDevices {
		return ErrObservationLimit
	}
	weight := uint64(256)
	seen := map[string]bool{}
	for _, d := range v.Devices {
		if uint64(len(d.UUID)) > l.MaxGPUUUIDBytes {
			return ErrObservationLimit
		}
		if d.UUID == "" || !utf8.ValidString(d.UUID) || strings.ContainsRune(d.UUID, 0) || seen[d.UUID] {
			return ErrInvalid
		}
		seen[d.UUID] = true
		weight += 1024 + uint64(len(d.UUID))
		for i, f := range []GPUMetric{d.Utilization, d.MemoryUtilization, d.EncoderUtilization, d.DecoderUtilization, d.MemoryUsedBytes, d.MemoryTotalBytes, d.MemoryFreeBytes} {
			if !f.Status.valid() || !f.Reason.valid() || (i < 4 && f.Value > 100) || (f.Status == MetricAvailable && (f.LastSuccessAt.IsZero() || f.LastSuccessAt.After(v.SampledAt))) {
				return ErrInvalid
			}
		}
	}
	if weight > l.MaxGPUBytes {
		return ErrObservationLimit
	}
	if o.value != nil && !v.SampledAt.After(o.sampledAt) {
		return ErrStale
	}
	revision, err := NextRevision(o.revision)
	if err != nil {
		return err
	}
	o.revision = revision
	o.sampledAt = v.SampledAt
	v = *cloneGPU(&v)
	o.value = &v
	a.appendEventLocked(RuntimeEvent{Kind: RuntimeGPUEvent, Owner: RuntimeSystemOwner, Stamp: ObservationStamp{Token: o.token, InstanceID: o.token, Revision: revision, ObservedAt: v.SampledAt.UTC()}, GPU: o.value})
	return nil
}
func (s *Service) RuntimeAnchor(ctx context.Context) (RuntimeAnchor, error) {
	a, err := s.runtimeAuthority()
	if err != nil {
		return RuntimeAnchor{}, err
	}
	return a.RuntimeAnchor(ctx)
}
func (a *RuntimeAuthority) RuntimeAnchor(ctx context.Context) (RuntimeAnchor, error) {
	if err := a.acquire(ctx); err != nil {
		return RuntimeAnchor{}, err
	}
	defer a.release()
	if a.events == nil {
		return RuntimeAnchor{}, ErrNotReady
	}
	if a.events.exhausted {
		return RuntimeAnchor{}, ErrEventSequenceExhausted
	}
	result := RuntimeAnchor{Cursor: a.cursorLocked(), Changed: a.events.changed}
	fences := map[RuntimeSessionFence]bool{}
	for _, n := range a.observations {
		fences[n.session] = true
	}
	for f := range fences {
		snapshot, err := a.runtimeSnapshotLocked(f)
		if err != nil {
			return RuntimeAnchor{}, err
		}
		result.Sessions = append(result.Sessions, snapshot)
	}
	sort.Slice(result.Sessions, func(i, j int) bool {
		l, r := result.Sessions[i].Fence, result.Sessions[j].Fence
		if l.SessionID != r.SessionID {
			return l.SessionID < r.SessionID
		}
		if l.Incarnation != r.Incarnation {
			return l.Incarnation < r.Incarnation
		}
		return l.Lifecycle < r.Lifecycle
	})
	if a.system != nil {
		result.GPU = freshGPU(a.system.value, time.Now())
	}
	return result, nil
}

func freshGPU(p *GPUSnapshot, now time.Time) *GPUSnapshot {
	v := cloneGPU(p)
	if v == nil {
		return nil
	}
	v.SampledAt = v.SampledAt.UTC()
	for i := range v.Devices {
		d := &v.Devices[i]
		for _, f := range []*GPUMetric{&d.Utilization, &d.MemoryUtilization, &d.EncoderUtilization, &d.DecoderUtilization, &d.MemoryUsedBytes, &d.MemoryTotalBytes, &d.MemoryFreeBytes} {
			if f.Status == MetricAvailable && (f.LastSuccessAt.IsZero() || now.Sub(f.LastSuccessAt) >= v.StaleAfter) {
				f.Status = MetricStale
				f.Reason = ReasonStale
			}
			f.SampledAt = f.SampledAt.UTC()
			f.LastSuccessAt = f.LastSuccessAt.UTC()
		}
	}
	return v
}

func (a *RuntimeAuthority) freshMediaLocked(m MediaMetrics, at time.Time) MediaMetrics {
	if a.telemetry == nil || time.Since(at) < a.telemetry.MediaStaleAfter {
		return m
	}
	for _, r := range []*Rate{&m.Rates.FPS, &m.Rates.PayloadBitrate, &m.Rates.WrittenBitrate} {
		if r.Status == MetricAvailable || r.Status == MetricWarmingUp {
			*r = Rate{Status: MetricStale, Reason: ReasonStale, WindowStart: r.WindowStart, WindowEnd: r.WindowEnd, Duration: r.Duration}
		}
	}
	return m
}
