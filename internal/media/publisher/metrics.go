package publisher

import (
	"errors"
	"math"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// FailureReason задаёт конечный набор оперативных причин без произвольных меток.
type FailureReason string

const (
	FailureNone      FailureReason = ""
	FailureNetwork   FailureReason = "network"
	FailureTerminal  FailureReason = "terminal"
	FailureSource    FailureReason = "source"
	FailureAuthority FailureReason = "authority"
)

// DeliveryDrops учитывает только единицы доступа, уже переданные через Next.
type DeliveryDrops struct {
	Stale, Transport, Cancelled, Terminal uint64
}

// Snapshot содержит логический вес данных и накопленные измерения одного выхода.
// WrittenBytes — незашифрованные байты RTMP, принятые транспортом,
// включая частичные записи с ошибками.
// PayloadBytes и AccessUnits учитывают только полные сообщения с медиаданными.
// Connections и Reconnects учитывают подтверждённое начало публикации.
// Attempts учитывает отдельные попытки установления соединения.
// WireBufferBytes до получения соединения показывает зарезервированный предел
// транспортной попытки, затем — логический резерв самого транспорта; это не RSS.
type Snapshot struct {
	SampledAt                                                 time.Time
	State                                                     core.OutputState
	Failure                                                   FailureReason
	Attempts, Connections, Reconnects                         uint64
	WrittenBytes, PayloadBytes, AccessUnits, VideoFrames      uint64
	QueuedPackets, PendingPackets, PacketBytes, ConfigBytes   uint64
	Generation                                                uint64
	Resync                                                    bool
	InFlightPacketBytes, InFlightConfigBytes, WireBufferBytes uint64
	QueueDrops                                                media.SubscriptionDrops
	DeliveryDrops                                             DeliveryDrops
	LastWrite, LastMedia                                      time.Time
	LastFrame                                                 time.Time
	Format                                                    core.ObservedFormat
	Saturated                                                 bool
	NetworkJoined                                             bool
	QueueCapacityPackets, QueueCapacityBytes                  uint64
	ConfigCapacityBytes                                       uint64
	AckAheadEventsTotal, AckAheadBytesLast, AckAheadBytesMax  uint64
	AckLastAnomalyAt                                          time.Time
}

// Snapshot согласует активную подписку с переносом её окончательных счётчиков.
func (p *Publisher) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.metrics
	s.State, s.Failure, s.NetworkJoined = p.state, p.failure, p.joined
	s.QueueDrops = p.queueDrops
	if p.sub != nil {
		q := p.sub.Snapshot()
		s.QueuedPackets, s.PendingPackets = q.QueuedPackets, q.PendingPackets
		s.PacketBytes, s.ConfigBytes, s.Generation, s.Resync = q.PacketBytes, q.ConfigBytes, q.Generation, q.Resync
		addQueueDrops(&s.QueueDrops, q.Drops, &s.Saturated)
		s.Saturated = s.Saturated || q.Saturated
	}
	// Обработчик записи использует отдельную блокировку и не захватывает p.mu.
	if p.conn != nil {
		wire := p.conn.Snapshot()
		s.WireBufferBytes = wire.BufferBytes
		s.Saturated = s.Saturated || wire.Saturated
	}
	p.writeMu.Lock()
	s.WrittenBytes, s.LastWrite = p.writtenBytes, p.lastWrite
	s.AckAheadEventsTotal, s.AckAheadBytesLast, s.AckAheadBytesMax = p.ackAheadEventsTotal, p.ackAheadBytesLast, p.ackAheadBytesMax
	s.AckLastAnomalyAt = p.ackLastAnomalyAt
	s.Saturated = s.Saturated || p.writeSaturated
	s.SampledAt = time.Now()
	p.writeMu.Unlock()
	p.metrics.Saturated = s.Saturated
	s.QueueCapacityPackets, s.QueueCapacityBytes = uint64(p.options.Queue.MaxPackets), p.options.Queue.MaxBytes
	s.ConfigCapacityBytes = p.options.Queue.MaxConfigBytes
	if s.ConfigCapacityBytes > p.options.Queue.MaxBytes-s.ConfigCapacityBytes {
		s.ConfigCapacityBytes = p.options.Queue.MaxBytes
	} else {
		s.ConfigCapacityBytes *= 2
	}
	return s
}

func saturate(dst *uint64, n uint64, saturated *bool) {
	if n > math.MaxUint64-*dst {
		*dst = math.MaxUint64
		*saturated = true
		return
	}
	*dst += n
}

func addQueueDrops(dst *media.SubscriptionDrops, src media.SubscriptionDrops, saturated *bool) {
	for _, pair := range []struct {
		dst *uint64
		n   uint64
	}{
		{&dst.Overflow, src.Overflow}, {&dst.Oversized, src.Oversized}, {&dst.ConfigRejected, src.ConfigRejected}, {&dst.AwaitingKeyframe, src.AwaitingKeyframe},
		{&dst.AudioBeforeCut, src.AudioBeforeCut}, {&dst.SourceReset, src.SourceReset}, {&dst.ConfigReset, src.ConfigReset}, {&dst.Closed, src.Closed},
	} {
		saturate(pair.dst, pair.n, saturated)
	}
}

func (p *Publisher) noteWrite(n int) {
	if n <= 0 {
		return
	}
	p.writeMu.Lock()
	saturate(&p.writtenBytes, uint64(n), &p.writeSaturated)
	p.lastWrite = time.Now().UTC()
	p.writeMu.Unlock()
}

func (p *Publisher) noteACKAnomaly(bytes uint64, at time.Time) {
	p.writeMu.Lock()
	saturate(&p.ackAheadEventsTotal, 1, &p.writeSaturated)
	p.ackAheadBytesLast = bytes
	p.ackAheadBytesMax = max(p.ackAheadBytesMax, bytes)
	p.ackLastAnomalyAt = at
	p.writeMu.Unlock()
}

func (p *Publisher) attachSubscription(s *media.Subscription) {
	p.mu.Lock()
	p.sub = s
	p.mu.Unlock()
}

func (p *Publisher) closeSubscription() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sub == nil {
		return
	}
	_ = p.sub.Close()
	final := p.sub.Snapshot()
	addQueueDrops(&p.queueDrops, final.Drops, &p.metrics.Saturated)
	p.metrics.Saturated = p.metrics.Saturated || final.Saturated
	p.sub = nil
}

func (p *Publisher) hold(d media.Delivery) {
	p.mu.Lock()
	p.metrics.InFlightPacketBytes = d.Packet.StoredBytes()
	p.metrics.InFlightConfigBytes = d.VideoConfig.StoredBytes() + d.AudioConfig.StoredBytes()
	p.mu.Unlock()
}

func (p *Publisher) delivered(d media.Delivery, bytes uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	saturate(&p.metrics.PayloadBytes, bytes, &p.metrics.Saturated)
	saturate(&p.metrics.AccessUnits, 1, &p.metrics.Saturated)
	if d.Packet.Config().Info().Track == media.TrackVideo {
		saturate(&p.metrics.VideoFrames, 1, &p.metrics.Saturated)
		v := d.Packet.Config().Info().Video
		p.metrics.Format.VideoCodec, p.metrics.Format.Width, p.metrics.Format.Height = v.Codec, v.Width, v.Height
		p.metrics.LastFrame = time.Now()
	} else {
		a := d.Packet.Config().Info().Audio
		p.metrics.Format.AudioCodec, p.metrics.Format.SampleRateHz, p.metrics.Format.Channels = a.Codec, a.SampleRateHz, a.Channels
	}
	p.metrics.LastMedia = time.Now().UTC()
	p.metrics.InFlightPacketBytes, p.metrics.InFlightConfigBytes = 0, 0
}

type dropReason uint8

const (
	dropStale dropReason = iota
	dropTransport
	dropCancelled
	dropTerminal
)

func (p *Publisher) abandoned(reason dropReason) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var count *uint64
	switch reason {
	case dropStale:
		count = &p.metrics.DeliveryDrops.Stale
	case dropTransport:
		count = &p.metrics.DeliveryDrops.Transport
	case dropCancelled:
		count = &p.metrics.DeliveryDrops.Cancelled
	case dropTerminal:
		count = &p.metrics.DeliveryDrops.Terminal
	}
	saturate(count, 1, &p.metrics.Saturated)
	p.metrics.InFlightPacketBytes, p.metrics.InFlightConfigBytes = 0, 0
}

func failureFor(err error) FailureReason {
	if errors.Is(err, core.ErrStale) || errors.Is(err, core.ErrClosed) {
		return FailureAuthority
	}
	if errors.Is(err, media.ErrRouterClosed) || errors.Is(err, media.ErrRouterBudget) || errors.Is(err, media.ErrSubscriptionGeneration) {
		return FailureSource
	}
	if terminal(err) {
		return FailureTerminal
	}
	return FailureNetwork
}
