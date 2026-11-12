package session

import (
	"context"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring"
)

// registerInputMetrics регистрирует измерение входа по токену наблюдателя сессии; пропавшая публикация
// сбрасывает расчёт скоростей, а запись выполняется только в разрешённом проходе сборщика.
func (n *sessionNode) registerInputMetrics() error {
	// calculator вычисляет скорости только текущей входной публикации; err сообщает об отказе
	// создания расчёта.
	calculator, err := monitoring.NewRateCalculator(n.manager.config.Monitoring.StaleAfter)
	if err != nil {
		return err
	}
	// token содержит точную идентичность наблюдателя Session для регистрации входных измерений.
	token := n.observer.Token()
	return n.sampler.Register(token, func(ctx context.Context, _ time.Time, publish bool) error {
		// Читает снимок публикации Router, сбрасывает скорости при её отсутствии и при разрешённой записи
		// передаёт метрики точному наблюдателю входа.

		// snapshot содержит текущее подключение входной публикации и её накопления Router.
		snapshot := n.router.IngressSnapshot()
		if !snapshot.Connected || snapshot.PublicationID == "" {
			calculator.Absent()
			return nil
		}
		// s содержит счётчики текущей входной публикации.
		s := snapshot.Counters
		// rates содержит скорости текущего PublicationID, различаемого вместе с токеном
		// наблюдателя.
		rates := calculator.Sample(monitoring.SourceIdentity{Token: token, LeaseID: snapshot.PublicationID, Kind: monitoring.SourceInput}, monitoring.RawCounters{SampledAt: s.SampledAt, PayloadBytes: s.PayloadBytes, VideoFrames: s.VideoFrames, Saturated: s.Saturated}, time.Now())
		if !publish {
			return nil
		}
		return n.observer.ObserveInput(ctx, snapshot.PublicationID, core.InputMetrics{MediaMetrics: s.Metrics(rates), SourceEpoch: snapshot.SourceEpoch})
	})
}

// registerOutputMetrics регистрирует снимки отправителя по токену наблюдателя выхода, вычисляя
// скорости и передавая счётчики доставки, очереди и памяти при разрешённой публикации.
func (n *sessionNode) registerOutputMetrics(p *publisher.Publisher, observer *core.OutputObserver) error {
	// calculator вычисляет скорости конкретного исполнения выхода; err сообщает об отказе создания
	// расчёта.
	calculator, err := monitoring.NewRateCalculator(n.manager.config.Monitoring.StaleAfter)
	if err != nil {
		return err
	}
	// token содержит точную идентичность наблюдателя выхода для регистрации его измерений.
	token := observer.Token()
	return n.sampler.Register(token, func(ctx context.Context, _ time.Time, publish bool) error {
		// Снимает счётчики отправителя, обновляет расчёт скоростей и при разрешённой записи передаёт
		// наблюдателю выхода метрики доставки, очереди и памяти.

		// s содержит согласованный снимок отправителя с накоплениями, очередью и логическими
		// резервами.
		s := p.Snapshot()
		// rates содержит скорости полезных байтов, записанных RTMP-байтов и видеокадров этого
		// выхода.
		rates := calculator.Sample(monitoring.SourceIdentity{Token: token, Kind: monitoring.SourceOutput}, monitoring.RawCounters{SampledAt: s.SampledAt, PayloadBytes: s.PayloadBytes, WrittenBytes: s.WrittenBytes, VideoFrames: s.VideoFrames, Saturated: s.Saturated}, time.Now())
		if !publish {
			return nil
		}
		// q содержит потери очереди подписки; d содержит потери уже выданных отправителю единиц
		// доступа.
		q, d := s.QueueDrops, s.DeliveryDrops
		return observer.ObserveMetrics(ctx, core.OutputMetrics{SampledAt: s.SampledAt, PayloadBytes: s.PayloadBytes, WrittenBytes: s.WrittenBytes, VideoFrames: s.VideoFrames, AudioUnits: s.AccessUnits - s.VideoFrames, Saturated: s.Saturated, LastFrame: s.LastFrame, Format: s.Format, Rates: rates,
			Attempts: s.Attempts, Connections: s.Connections, Reconnects: s.Reconnects,
			AckAheadEventsTotal: s.AckAheadEventsTotal, AckAheadBytesLast: s.AckAheadBytesLast, AckAheadBytesMax: s.AckAheadBytesMax, AckLastAnomalyAt: s.AckLastAnomalyAt,
			QueueDrops:    core.QueueDropMetrics{Overflow: q.Overflow, Oversized: q.Oversized, ConfigRejected: q.ConfigRejected, AwaitingKeyframe: q.AwaitingKeyframe, AudioBeforeCut: q.AudioBeforeCut, SourceReset: q.SourceReset, ConfigReset: q.ConfigReset, Closed: q.Closed},
			DeliveryDrops: core.DeliveryDropMetrics{Stale: d.Stale, Transport: d.Transport, Cancelled: d.Cancelled, Terminal: d.Terminal},
			AccessUnits:   s.AccessUnits, Generation: s.Generation, QueuedPackets: s.QueuedPackets, PendingPackets: s.PendingPackets, Resync: s.Resync,
			PacketBytes: s.PacketBytes, ConfigBytes: s.ConfigBytes, ConfigByteCapacity: s.ConfigCapacityBytes,
			InFlightPacketBytes: s.InFlightPacketBytes, InFlightConfigBytes: s.InFlightConfigBytes, WireBufferBytes: s.WireBufferBytes,
			QueuePackets: s.QueuedPackets + s.PendingPackets, QueueCapacity: s.QueueCapacityPackets, QueueBytes: s.PacketBytes + s.ConfigBytes, QueueByteCapacity: s.QueueCapacityBytes, LastWrite: s.LastWrite, LastMedia: s.LastMedia})
	})
}
