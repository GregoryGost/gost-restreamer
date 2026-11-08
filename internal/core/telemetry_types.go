package core

import (
	"math"
	"time"
)

type MetricStatus string

const (
	MetricAvailable    MetricStatus = "available"
	MetricWarmingUp    MetricStatus = "warming_up"
	MetricUnavailable  MetricStatus = "unavailable"
	MetricStale        MetricStatus = "stale"
	MetricSaturated    MetricStatus = "saturated"
	MetricCounterReset MetricStatus = "counter_reset"
	MetricUnsupported  MetricStatus = "unsupported"
)

func (s MetricStatus) valid() bool {
	switch s {
	case MetricAvailable, MetricWarmingUp, MetricUnavailable, MetricStale, MetricSaturated, MetricCounterReset, MetricUnsupported:
		return true
	}
	return false
}

type MetricReason string

const (
	ReasonNone                MetricReason = ""
	ReasonUnsupportedPlatform MetricReason = "unsupported_platform"
	ReasonLibraryUnavailable  MetricReason = "library_unavailable"
	ReasonDriverUnavailable   MetricReason = "driver_unavailable"
	ReasonNotSupported        MetricReason = "not_supported"
	ReasonNotFound            MetricReason = "not_found"
	ReasonCollectionFailed    MetricReason = "collection_failed"
	ReasonSourceAbsent        MetricReason = "source_absent"
	ReasonDisconnected        MetricReason = "disconnected"
	ReasonInvalidSample       MetricReason = "invalid_sample"
	ReasonCounterRegression   MetricReason = "counter_regression"
	ReasonCounterSaturated    MetricReason = "counter_saturated"
	ReasonObservationFailed   MetricReason = "observation_failed"
	ReasonCGODisabled         MetricReason = "cgo_disabled"
	ReasonPermissionDenied    MetricReason = "permission_denied"
	ReasonDeviceLost          MetricReason = "device_lost"
	ReasonProviderError       MetricReason = "provider_error"
	ReasonBudgetExceeded      MetricReason = "budget_exceeded"
	ReasonBusy                MetricReason = "busy"
	ReasonStale               MetricReason = "stale"
	ReasonWarmingUp           MetricReason = "warming_up"
)

func (r MetricReason) valid() bool {
	switch r {
	case ReasonNone, ReasonUnsupportedPlatform, ReasonLibraryUnavailable, ReasonDriverUnavailable, ReasonNotSupported, ReasonNotFound, ReasonCollectionFailed, ReasonSourceAbsent, ReasonDisconnected, ReasonInvalidSample, ReasonCounterRegression, ReasonCounterSaturated, ReasonObservationFailed, ReasonCGODisabled, ReasonPermissionDenied, ReasonDeviceLost, ReasonProviderError, ReasonBudgetExceeded, ReasonBusy, ReasonStale, ReasonWarmingUp:
		return true
	}
	return false
}

// Rate задаёт скорость за фактическое окно конкретного производителя.
// PayloadBitrate и WrittenBitrate измеряются в битах в секунду, FPS — в кадрах в секунду.
type Rate struct {
	Status      MetricStatus
	Reason      MetricReason
	Value       float64
	WindowStart time.Time
	WindowEnd   time.Time
	Duration    time.Duration
}

type MediaRates struct{ FPS, PayloadBitrate, WrittenBitrate Rate }

type ObservedFormat struct {
	VideoCodec    Codec
	AudioCodec    Codec
	Width, Height uint32
	SampleRateHz  uint32
	Channels      uint16
}

type MediaMetrics struct {
	SampledAt                               time.Time
	VideoFrames, PayloadBytes, WrittenBytes uint64
	AudioUnits                              uint64
	Saturated                               bool
	LastFrame                               time.Time
	Rates                                   MediaRates
	Format                                  ObservedFormat
}

type InputMetrics struct {
	MediaMetrics
	SourceEpoch uint64
}

type InputSnapshot struct {
	Stamp     ObservationStamp
	LeaseID   ID
	Connected bool
	ChangedAt time.Time
	Metrics   *InputMetrics
}

type GroupMetrics struct{ LiveInput, FallbackInput, Output MediaMetrics }

type OutputMetrics struct {
	QueuedPackets, PacketBytes                                uint64
	AccessUnits, PendingPackets, Generation                   uint64
	Resync                                                    bool
	InFlightPacketBytes, InFlightConfigBytes, WireBufferBytes uint64
	ConfigBytes, ConfigByteCapacity                           uint64
	MediaMetrics
	Attempts, Connections, Reconnects   uint64
	QueueDrops                          QueueDropMetrics
	DeliveryDrops                       DeliveryDropMetrics
	QueuePackets, QueueCapacity         uint64
	QueueBytes, QueueByteCapacity       uint64
	LastWrite, LastMedia                time.Time
	AckAheadEventsTotal                 uint64
	AckAheadBytesLast, AckAheadBytesMax uint64
	AckLastAnomalyAt                    time.Time
}

type QueueDropMetrics struct{ Overflow, Oversized, ConfigRejected, AwaitingKeyframe, AudioBeforeCut, SourceReset, ConfigReset, Closed uint64 }
type DeliveryDropMetrics struct{ Stale, Transport, Cancelled, Terminal uint64 }

type OptionalUint64 struct {
	Value   uint64
	Present bool
}
type OptionalInt64 struct {
	Value   int64
	Present bool
}
type OptionalFloat64 struct {
	Value   float64
	Present bool
}

type FFmpegProgress struct {
	Frame, TotalSizeBytes, DupFrames, DropFrames OptionalUint64
	FPS, BitrateBitsPerSecond, Speed             OptionalFloat64
	OutTimeUS                                    OptionalInt64
	End                                          bool
}

type FFmpegMetrics struct {
	Status           MetricStatus
	Reason           MetricReason
	HasProgress      bool
	LastProgressAt   time.Time
	ProgressLosses   uint64
	SampledAt        time.Time
	Progress         FFmpegProgress
	DiagnosticLosses uint64
}

// GPUMetric сохраняет точное целое значение в процентах либо байтах, согласно полю.
type GPUMetric struct {
	Status                   MetricStatus
	Reason                   MetricReason
	Value                    uint64
	SampledAt, LastSuccessAt time.Time
	SamplingPeriodUS         uint64
}

type GPUDevice struct {
	UUID                                                                   string
	Utilization, MemoryUtilization, EncoderUtilization, DecoderUtilization GPUMetric
	MemoryUsedBytes, MemoryTotalBytes, MemoryFreeBytes                     GPUMetric
}

type GPUSnapshot struct {
	StaleAfter time.Duration
	SampledAt  time.Time
	Status     MetricStatus
	Reason     MetricReason
	Devices    []GPUDevice
}

func validFloat(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
func (r Rate) valid() bool {
	if r.Status == "" {
		return r == (Rate{})
	}
	if !r.Status.valid() || !r.Reason.valid() || !validFloat(r.Value) || r.Duration < 0 {
		return false
	}
	if r.Status == MetricAvailable {
		// Duration сохраняет монотонное измерение; UTC-метки могут сдвинуться при коррекции часов.
		return r.Duration > 0 && !r.WindowStart.IsZero() && !r.WindowEnd.IsZero()
	}
	return r.Value == 0
}
func (m MediaMetrics) valid() bool {
	f := m.Format
	return !m.SampledAt.IsZero() && (f.VideoCodec == "" || f.VideoCodec == CodecAV1 || f.VideoCodec == CodecH264) && (f.AudioCodec == "" || f.AudioCodec == CodecAAC) && m.Rates.FPS.valid() && m.Rates.PayloadBitrate.valid() && m.Rates.WrittenBitrate.valid()
}
func (m FFmpegMetrics) valid() bool {
	p := m.Progress
	return !m.SampledAt.IsZero() && (m.Status == "" || m.Status.valid()) && m.Reason.valid() && validFloat(p.FPS.Value) && validFloat(p.BitrateBitsPerSecond.Value) && validFloat(p.Speed.Value)
}
func utcRate(r Rate) Rate {
	r.WindowStart = r.WindowStart.UTC()
	r.WindowEnd = r.WindowEnd.UTC()
	return r
}
func utcMedia(m MediaMetrics) MediaMetrics {
	m.SampledAt = m.SampledAt.UTC()
	m.LastFrame = m.LastFrame.UTC()
	m.Rates.FPS = utcRate(m.Rates.FPS)
	m.Rates.PayloadBitrate = utcRate(m.Rates.PayloadBitrate)
	m.Rates.WrittenBitrate = utcRate(m.Rates.WrittenBitrate)
	return m
}
