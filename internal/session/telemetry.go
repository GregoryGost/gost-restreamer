package session

import (
	"context"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring"
)

func (n *sessionNode) registerInputMetrics() error {
	calculator, err := monitoring.NewRateCalculator(n.manager.config.Monitoring.StaleAfter)
	if err != nil {
		return err
	}
	token := n.observer.Token()
	return n.sampler.Register(token, func(ctx context.Context, _ time.Time, publish bool) error {
		snapshot := n.router.IngressSnapshot()
		if !snapshot.Connected || snapshot.PublicationID == "" {
			calculator.Absent()
			return nil
		}
		s := snapshot.Counters
		rates := calculator.Sample(monitoring.SourceIdentity{Token: token, LeaseID: snapshot.PublicationID, Kind: monitoring.SourceInput}, monitoring.RawCounters{SampledAt: s.SampledAt, PayloadBytes: s.PayloadBytes, VideoFrames: s.VideoFrames, Saturated: s.Saturated}, time.Now())
		if !publish {
			return nil
		}
		return n.observer.ObserveInput(ctx, snapshot.PublicationID, core.InputMetrics{MediaMetrics: s.Metrics(rates), SourceEpoch: snapshot.SourceEpoch})
	})
}

func (n *sessionNode) registerOutputMetrics(p *publisher.Publisher, observer *core.OutputObserver) error {
	calculator, err := monitoring.NewRateCalculator(n.manager.config.Monitoring.StaleAfter)
	if err != nil {
		return err
	}
	token := observer.Token()
	return n.sampler.Register(token, func(ctx context.Context, _ time.Time, publish bool) error {
		s := p.Snapshot()
		rates := calculator.Sample(monitoring.SourceIdentity{Token: token, Kind: monitoring.SourceOutput}, monitoring.RawCounters{SampledAt: s.SampledAt, PayloadBytes: s.PayloadBytes, WrittenBytes: s.WrittenBytes, VideoFrames: s.VideoFrames, Saturated: s.Saturated}, time.Now())
		if !publish {
			return nil
		}
		q, d := s.QueueDrops, s.DeliveryDrops
		return observer.ObserveMetrics(ctx, core.OutputMetrics{MediaMetrics: core.MediaMetrics{SampledAt: s.SampledAt, PayloadBytes: s.PayloadBytes, WrittenBytes: s.WrittenBytes, VideoFrames: s.VideoFrames, AudioUnits: s.AccessUnits - s.VideoFrames, Saturated: s.Saturated, LastFrame: s.LastFrame, Format: s.Format, Rates: rates},
			Attempts: s.Attempts, Connections: s.Connections, Reconnects: s.Reconnects,
			QueueDrops:    core.QueueDropMetrics{Overflow: q.Overflow, Oversized: q.Oversized, ConfigRejected: q.ConfigRejected, AwaitingKeyframe: q.AwaitingKeyframe, AudioBeforeCut: q.AudioBeforeCut, SourceReset: q.SourceReset, ConfigReset: q.ConfigReset, Closed: q.Closed},
			DeliveryDrops: core.DeliveryDropMetrics{Stale: d.Stale, Transport: d.Transport, Cancelled: d.Cancelled, Terminal: d.Terminal},
			AccessUnits:   s.AccessUnits, Generation: s.Generation, QueuedPackets: s.QueuedPackets, PendingPackets: s.PendingPackets, Resync: s.Resync,
			PacketBytes: s.PacketBytes, ConfigBytes: s.ConfigBytes, ConfigByteCapacity: s.ConfigCapacityBytes,
			InFlightPacketBytes: s.InFlightPacketBytes, InFlightConfigBytes: s.InFlightConfigBytes, WireBufferBytes: s.WireBufferBytes,
			QueuePackets: s.QueuedPackets + s.PendingPackets, QueueCapacity: s.QueueCapacityPackets, QueueBytes: s.PacketBytes + s.ConfigBytes, QueueByteCapacity: s.QueueCapacityBytes, LastWrite: s.LastWrite, LastMedia: s.LastMedia})
	})
}
