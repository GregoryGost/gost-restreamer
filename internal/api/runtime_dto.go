package api

import (
	"strconv"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

type RateDTO struct {
	Status      string     `json:"status"`
	Reason      string     `json:"reason"`
	Value       *float64   `json:"value"`
	WindowStart *time.Time `json:"window_start"`
	WindowEnd   *time.Time `json:"window_end"`
	DurationNS  Decimal    `json:"duration_ns"`
}

func rateDTO(r core.Rate) RateDTO {
	status := string(r.Status)
	reason := string(r.Reason)
	if status == "" {
		status = "unavailable"
		reason = "source_absent"
	}
	var value *float64
	if r.Status == core.MetricAvailable {
		v := r.Value
		value = &v
	}
	return RateDTO{status, reason, value, timestamp(r.WindowStart), timestamp(r.WindowEnd), Decimal(r.Duration)}
}

type RatesDTO struct {
	FPS            RateDTO `json:"fps"`
	PayloadBitrate RateDTO `json:"payload_bitrate"`
	WrittenBitrate RateDTO `json:"written_bitrate"`
}
type ObservedFormatDTO struct {
	VideoCodec   string `json:"video_codec"`
	AudioCodec   string `json:"audio_codec"`
	Width        uint32 `json:"width"`
	Height       uint32 `json:"height"`
	SampleRateHz uint32 `json:"sample_rate_hz"`
	Channels     uint16 `json:"channels"`
}
type MediaMetricsDTO struct {
	SampledAt    *time.Time        `json:"sampled_at"`
	VideoFrames  Decimal           `json:"video_frames"`
	PayloadBytes Decimal           `json:"payload_bytes"`
	WrittenBytes Decimal           `json:"written_bytes"`
	AudioUnits   Decimal           `json:"audio_units"`
	Saturated    bool              `json:"saturated"`
	LastFrame    *time.Time        `json:"last_frame"`
	Rates        RatesDTO          `json:"rates"`
	Format       ObservedFormatDTO `json:"format"`
}

func mediaDTO(m core.MediaMetrics) MediaMetricsDTO {
	f := m.Format
	return MediaMetricsDTO{timestamp(m.SampledAt), Decimal(m.VideoFrames), Decimal(m.PayloadBytes), Decimal(m.WrittenBytes), Decimal(m.AudioUnits), m.Saturated, timestamp(m.LastFrame), RatesDTO{rateDTO(m.Rates.FPS), rateDTO(m.Rates.PayloadBitrate), rateDTO(m.Rates.WrittenBitrate)}, ObservedFormatDTO{string(f.VideoCodec), string(f.AudioCodec), f.Width, f.Height, f.SampleRateHz, f.Channels}}
}

type InputMetricsDTO struct {
	Media       MediaMetricsDTO `json:"media"`
	SourceEpoch Decimal         `json:"source_epoch"`
}
type InputDTO struct {
	Connected bool             `json:"connected"`
	ChangedAt *time.Time       `json:"changed_at"`
	Metrics   *InputMetricsDTO `json:"metrics"`
}

func inputDTO(i *core.InputSnapshot) *InputDTO {
	if i == nil {
		return nil
	}
	v := &InputDTO{Connected: i.Connected, ChangedAt: timestamp(i.ChangedAt)}
	if i.Metrics != nil {
		v.Metrics = &InputMetricsDTO{mediaDTO(i.Metrics.MediaMetrics), Decimal(i.Metrics.SourceEpoch)}
	}
	return v
}

type GroupMetricsDTO struct {
	LiveInput     MediaMetricsDTO `json:"live_input"`
	FallbackInput MediaMetricsDTO `json:"fallback_input"`
	Output        MediaMetricsDTO `json:"output"`
}

func groupMetricsDTO(m *core.GroupMetrics) *GroupMetricsDTO {
	if m == nil {
		return nil
	}
	return &GroupMetricsDTO{mediaDTO(m.LiveInput), mediaDTO(m.FallbackInput), mediaDTO(m.Output)}
}

type OutputMetricsDTO struct {
	Media         MediaMetricsDTO    `json:"media"`
	Counters      map[string]Decimal `json:"counters"`
	QueueDrops    map[string]Decimal `json:"queue_drops"`
	DeliveryDrops map[string]Decimal `json:"delivery_drops"`
	Resync        bool               `json:"resync"`
	LastWrite     *time.Time         `json:"last_write"`
	LastMedia     *time.Time         `json:"last_media"`
}

func outputMetricsDTO(m *core.OutputMetrics) *OutputMetricsDTO {
	if m == nil {
		return nil
	}
	q, d := m.QueueDrops, m.DeliveryDrops
	return &OutputMetricsDTO{mediaDTO(m.MediaMetrics), map[string]Decimal{"queued_packets": Decimal(m.QueuedPackets), "packet_bytes": Decimal(m.PacketBytes), "access_units": Decimal(m.AccessUnits), "pending_packets": Decimal(m.PendingPackets), "generation": Decimal(m.Generation), "in_flight_packet_bytes": Decimal(m.InFlightPacketBytes), "in_flight_config_bytes": Decimal(m.InFlightConfigBytes), "wire_buffer_bytes": Decimal(m.WireBufferBytes), "config_bytes": Decimal(m.ConfigBytes), "config_byte_capacity": Decimal(m.ConfigByteCapacity), "attempts": Decimal(m.Attempts), "connections": Decimal(m.Connections), "reconnects": Decimal(m.Reconnects), "queue_packets": Decimal(m.QueuePackets), "queue_capacity": Decimal(m.QueueCapacity), "queue_bytes": Decimal(m.QueueBytes), "queue_byte_capacity": Decimal(m.QueueByteCapacity)}, map[string]Decimal{"overflow": Decimal(q.Overflow), "oversized": Decimal(q.Oversized), "config_rejected": Decimal(q.ConfigRejected), "awaiting_keyframe": Decimal(q.AwaitingKeyframe), "audio_before_cut": Decimal(q.AudioBeforeCut), "source_reset": Decimal(q.SourceReset), "config_reset": Decimal(q.ConfigReset), "closed": Decimal(q.Closed)}, map[string]Decimal{"stale": Decimal(d.Stale), "transport": Decimal(d.Transport), "cancelled": Decimal(d.Cancelled), "terminal": Decimal(d.Terminal)}, m.Resync, timestamp(m.LastWrite), timestamp(m.LastMedia)}
}

type FFmpegProgressDTO struct {
	Frame          *Decimal `json:"frame"`
	TotalSizeBytes *Decimal `json:"total_size_bytes"`
	DupFrames      *Decimal `json:"dup_frames"`
	DropFrames     *Decimal `json:"drop_frames"`
	FPS            *float64 `json:"fps"`
	BitrateBPS     *float64 `json:"bitrate_bps"`
	Speed          *float64 `json:"speed"`
	OutTimeUS      *string  `json:"out_time_us"`
	End            bool     `json:"end"`
}

func optionalUint(v core.OptionalUint64) *Decimal {
	if !v.Present {
		return nil
	}
	d := Decimal(v.Value)
	return &d
}
func optionalFloat(v core.OptionalFloat64) *float64 {
	if !v.Present {
		return nil
	}
	x := v.Value
	return &x
}

type FFmpegMetricsDTO struct {
	Status           string             `json:"status"`
	Reason           string             `json:"reason"`
	SampledAt        *time.Time         `json:"sampled_at"`
	LastProgressAt   *time.Time         `json:"last_progress_at"`
	ProgressLosses   Decimal            `json:"progress_losses"`
	DiagnosticLosses Decimal            `json:"diagnostic_losses"`
	Progress         *FFmpegProgressDTO `json:"progress"`
}

func ffmpegMetricsDTO(m *core.FFmpegMetrics) *FFmpegMetricsDTO {
	if m == nil {
		return nil
	}
	status, reason := string(m.Status), string(m.Reason)
	if status == "" {
		status = "unavailable"
		reason = "source_absent"
	}
	v := &FFmpegMetricsDTO{Status: status, Reason: reason, SampledAt: timestamp(m.SampledAt), LastProgressAt: timestamp(m.LastProgressAt), ProgressLosses: Decimal(m.ProgressLosses), DiagnosticLosses: Decimal(m.DiagnosticLosses)}
	if m.Status == core.MetricAvailable && m.HasProgress {
		p := m.Progress
		var out *string
		if p.OutTimeUS.Present {
			x := strconv.FormatInt(p.OutTimeUS.Value, 10)
			out = &x
		}
		v.Progress = &FFmpegProgressDTO{optionalUint(p.Frame), optionalUint(p.TotalSizeBytes), optionalUint(p.DupFrames), optionalUint(p.DropFrames), optionalFloat(p.FPS), optionalFloat(p.BitrateBitsPerSecond), optionalFloat(p.Speed), out, p.End}
	}
	return v
}

type GPUMetricDTO struct {
	Status           string     `json:"status"`
	Reason           string     `json:"reason"`
	Value            *Decimal   `json:"value"`
	SampledAt        *time.Time `json:"sampled_at"`
	LastSuccessAt    *time.Time `json:"last_success_at"`
	SamplingPeriodUS Decimal    `json:"sampling_period_us"`
}

func gpuMetricDTO(m core.GPUMetric) GPUMetricDTO {
	var value *Decimal
	if m.Status == core.MetricAvailable {
		v := Decimal(m.Value)
		value = &v
	}
	return GPUMetricDTO{string(m.Status), string(m.Reason), value, timestamp(m.SampledAt), timestamp(m.LastSuccessAt), Decimal(m.SamplingPeriodUS)}
}

type GPUDeviceDTO struct {
	UUID               string       `json:"uuid"`
	Utilization        GPUMetricDTO `json:"utilization"`
	MemoryUtilization  GPUMetricDTO `json:"memory_utilization"`
	EncoderUtilization GPUMetricDTO `json:"encoder_utilization"`
	DecoderUtilization GPUMetricDTO `json:"decoder_utilization"`
	MemoryUsedBytes    GPUMetricDTO `json:"memory_used_bytes"`
	MemoryTotalBytes   GPUMetricDTO `json:"memory_total_bytes"`
	MemoryFreeBytes    GPUMetricDTO `json:"memory_free_bytes"`
}
type GPUDTO struct {
	Status       string         `json:"status"`
	Reason       string         `json:"reason"`
	SampledAt    *time.Time     `json:"sampled_at"`
	StaleAfterNS Decimal        `json:"stale_after_ns"`
	Devices      []GPUDeviceDTO `json:"devices"`
}

func gpuDTO(g *core.GPUSnapshot) *GPUDTO {
	if g == nil {
		return nil
	}
	v := &GPUDTO{string(g.Status), string(g.Reason), timestamp(g.SampledAt), Decimal(g.StaleAfter), make([]GPUDeviceDTO, 0, len(g.Devices))}
	for _, d := range g.Devices {
		v.Devices = append(v.Devices, GPUDeviceDTO{d.UUID, gpuMetricDTO(d.Utilization), gpuMetricDTO(d.MemoryUtilization), gpuMetricDTO(d.EncoderUtilization), gpuMetricDTO(d.DecoderUtilization), gpuMetricDTO(d.MemoryUsedBytes), gpuMetricDTO(d.MemoryTotalBytes), gpuMetricDTO(d.MemoryFreeBytes)})
	}
	return v
}

type ObservationDTO struct {
	InstanceID string      `json:"instance_id"`
	Revision   Decimal     `json:"revision"`
	ObservedAt *time.Time  `json:"observed_at"`
	State      string      `json:"state"`
	Failure    *FailureDTO `json:"failure"`
}

func observationDTO(epoch core.ID, s core.ObservationStamp, state string, failed bool) ObservationDTO {
	var f *FailureDTO
	if failed {
		f = failure("observation_failed")
	}
	return ObservationDTO{publicInstance(epoch, s.InstanceID), Decimal(s.Revision), timestamp(s.ObservedAt), state, f}
}
func publicInstance(epoch core.ID, id uint64) string {
	if id == 0 {
		return ""
	}
	return string(epoch) + ":" + strconv.FormatUint(id, 10)
}

type RuntimeGroupDTO struct {
	ObservationDTO
	GroupID        string           `json:"group_id"`
	ProfileID      string           `json:"profile_id"`
	Source         string           `json:"source"`
	SourceRevision Decimal          `json:"source_revision"`
	Metrics        *GroupMetricsDTO `json:"metrics"`
}
type RuntimeOutputDTO struct {
	ObservationDTO
	OutputID string            `json:"output_id"`
	Metrics  *OutputMetricsDTO `json:"metrics"`
}
type RuntimeFFmpegDTO struct {
	ObservationDTO
	GroupInstanceID string            `json:"group_instance_id"`
	Role            string            `json:"role"`
	Metrics         *FFmpegMetricsDTO `json:"metrics"`
}
type RuntimeSessionDTO struct {
	SessionID   string             `json:"session_id"`
	Observation *ObservationDTO    `json:"observation"`
	Input       *InputDTO          `json:"input"`
	Groups      []RuntimeGroupDTO  `json:"groups"`
	Outputs     []RuntimeOutputDTO `json:"outputs"`
	FFmpeg      []RuntimeFFmpegDTO `json:"ffmpeg"`
}

func runtimeSessionDTO(s core.SessionRuntimeSnapshot) RuntimeSessionDTO {
	epoch := s.Cursor.Epoch
	r := RuntimeSessionDTO{SessionID: string(s.Fence.SessionID), Input: inputDTO(s.Input), Groups: []RuntimeGroupDTO{}, Outputs: []RuntimeOutputDTO{}, FFmpeg: []RuntimeFFmpegDTO{}}
	if s.Session != nil {
		state := "UNKNOWN"
		failed := false
		if s.Session.Value != nil {
			state = string(s.Session.Value.State)
			failed = s.Session.Value.Error != ""
		}
		o := observationDTO(epoch, s.Session.Stamp, state, failed)
		r.Observation = &o
	}
	for _, g := range s.Groups {
		state, source := "UNKNOWN", "NONE"
		failed := false
		var revision uint64
		if g.Value != nil {
			state = string(g.Value.State)
			source = string(g.Value.Source)
			failed = g.Value.Error != ""
			revision = g.Value.SourceRevision
		}
		r.Groups = append(r.Groups, RuntimeGroupDTO{observationDTO(epoch, g.Stamp, state, failed), string(g.ConfigGroupID), string(g.ConfigProfileID), source, Decimal(revision), groupMetricsDTO(g.Metrics)})
	}
	for _, o := range s.Outputs {
		state := "UNKNOWN"
		failed := false
		if o.Value != nil {
			state = string(o.Value.State)
			failed = o.Value.Error != ""
		}
		r.Outputs = append(r.Outputs, RuntimeOutputDTO{observationDTO(epoch, o.Stamp, state, failed), string(o.Execution.OutputID), outputMetricsDTO(o.Metrics)})
	}
	for _, f := range s.FFmpeg {
		state := "UNKNOWN"
		failed := false
		if f.Value != nil {
			state = string(f.Value.State)
			failed = f.Value.Error != ""
		}
		r.FFmpeg = append(r.FFmpeg, RuntimeFFmpegDTO{observationDTO(epoch, f.Stamp, state, failed), publicInstance(epoch, f.GroupInstanceID), string(f.Role), ffmpegMetricsDTO(f.Metrics)})
	}
	return r
}

type AnchorDTO struct {
	Cursor   string              `json:"cursor"`
	Sessions []RuntimeSessionDTO `json:"sessions"`
	GPU      *GPUDTO             `json:"gpu"`
}
type SessionMetricsDTO struct {
	Availability string             `json:"availability"`
	Reason       string             `json:"reason"`
	Snapshot     *RuntimeSessionDTO `json:"snapshot"`
}
type RuntimeEventDTO struct {
	Cursor          string            `json:"cursor"`
	Kind            string            `json:"kind"`
	Owner           string            `json:"owner"`
	SessionID       string            `json:"session_id"`
	OutputID        string            `json:"output_id"`
	GroupID         string            `json:"group_id"`
	ProfileID       string            `json:"profile_id"`
	Observation     ObservationDTO    `json:"observation"`
	GroupInstanceID string            `json:"group_instance_id"`
	Source          string            `json:"source"`
	SourceRevision  Decimal           `json:"source_revision"`
	Role            string            `json:"role"`
	AttemptActive   bool              `json:"attempt_active"`
	Reason          string            `json:"reason"`
	Input           *InputDTO         `json:"input"`
	Group           *GroupMetricsDTO  `json:"group"`
	Output          *OutputMetricsDTO `json:"output"`
	FFmpeg          *FFmpegMetricsDTO `json:"ffmpeg"`
	GPU             *GPUDTO           `json:"gpu"`
}

func runtimeEventDTO(e core.RuntimeEvent) RuntimeEventDTO {
	state := string(e.State)
	if e.OutputState != "" {
		state = string(e.OutputState)
	}
	return RuntimeEventDTO{encodeCursor(e.Cursor), string(e.Kind), string(e.Owner), string(e.Session.SessionID), string(e.Execution.OutputID), string(e.ConfigGroupID), string(e.ConfigProfileID), observationDTO(e.Cursor.Epoch, e.Stamp, state, e.Reason == core.ReasonObservationFailed), publicInstance(e.Cursor.Epoch, e.GroupInstanceID), string(e.Source), Decimal(e.SourceRevision), string(e.Role), e.AttemptActive, string(e.Reason), inputDTO(e.Input), groupMetricsDTO(e.Group), outputMetricsDTO(e.Output), ffmpegMetricsDTO(e.FFmpeg), gpuDTO(e.GPU)}
}
