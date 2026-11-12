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
	// FailureNone обозначает отсутствие оперативной причины отказа.
	FailureNone FailureReason = ""
	// FailureNetwork обозначает повторяемую сетевую ошибку передачи.
	FailureNetwork FailureReason = "network"
	// FailureTerminal обозначает окончательный отказ, не устраняемый повтором сети.
	FailureTerminal FailureReason = "terminal"
	// FailureSource обозначает окончательный отказ маршрутизатора или поколения подписки.
	FailureSource FailureReason = "source"
	// FailureAuthority обозначает отзыв либо закрытие полномочий точного исполнения.
	FailureAuthority FailureReason = "authority"
)

// DeliveryDrops учитывает только единицы доступа, уже переданные через Next.
type DeliveryDrops struct {
	// Stale считает устаревшие выдачи; Transport считает сетевые потери; Cancelled считает потери
	// при отмене; Terminal считает потери при окончательном отказе.
	Stale, Transport, Cancelled, Terminal uint64
}

// Snapshot содержит логический вес данных и накопленные измерения одного выхода.
// WrittenBytes — незашифрованные байты RTMP, принятые транспортом,
// включая частичные записи с ошибками.
// PayloadBytes и AccessUnits учитывают только полные сообщения с медиаданными.
// Connections и Reconnects учитывают подтверждённое начало публикации.
// Attempts учитывает отдельные попытки установления соединения.
// WireBufferBytes до получения соединения показывает зарезервированный предел
// транспортной попытки, затем — логический резерв самого транспорта; это не объём резидентной памяти.
type Snapshot struct {
	// SampledAt содержит время согласованного чтения снимка измерений.
	SampledAt time.Time
	// State содержит текущее оперативное состояние выхода.
	State core.OutputState
	// Failure содержит ограниченную причину состояния отказа или восстановления.
	Failure                           FailureReason
	Attempts, Connections, Reconnects uint64
	// WrittenBytes считает принятые транспортом RTMP-байты; PayloadBytes считает полезные байты
	// полных медиасообщений; AccessUnits считает эти сообщения; VideoFrames считает отправленные
	// видеопакеты.
	WrittenBytes, PayloadBytes, AccessUnits, VideoFrames uint64
	// QueuedPackets считает пакеты очереди; PendingPackets считает AAC, ожидающие выбора первого
	// ключевого кадра; PacketBytes содержит логический вес очереди и ожидающих AAC вместе с
	// конфигурациями пакетов; ConfigBytes отдельно учитывает пару текущих заголовков подписки при
	// допустимом бюджете.
	QueuedPackets, PendingPackets, PacketBytes, ConfigBytes uint64
	// Generation содержит текущее поколение активной подписки.
	Generation uint64
	// Resync показывает ожидание подпиской синхронизации текущего источника.
	Resync bool
	// InFlightPacketBytes содержит вес полученного, но ещё не обработанного пакета;
	// InFlightConfigBytes содержит вес его двух конфигураций; WireBufferBytes содержит логический
	// резерв транспортной попытки.
	InFlightPacketBytes, InFlightConfigBytes, WireBufferBytes uint64
	// QueueDrops объединяет потери активной и уже закрытых подписок.
	QueueDrops media.SubscriptionDrops
	// DeliveryDrops считает потери после получения пакетов из подписки по ограниченным причинам.
	DeliveryDrops DeliveryDrops
	// LastWrite содержит время последней положительной записи транспортом; LastMedia содержит время
	// последнего полностью отправленного медиасообщения.
	LastWrite, LastMedia time.Time
	// LastFrame содержит время последнего полностью отправленного видеопакета.
	LastFrame time.Time
	// Format содержит наблюдённый формат фактически отправленных видео и аудио.
	Format core.ObservedFormat
	// Saturated отмечает насыщение любого учитываемого счётчика вместо переполнения.
	Saturated bool
	// NetworkJoined подтверждает завершение собственных ресурсов Publisher.
	NetworkJoined bool
	// QueueCapacityPackets задаёт предельное число пакетов подписки; QueueCapacityBytes задаёт её
	// общий бюджет байтов.
	QueueCapacityPackets, QueueCapacityBytes uint64
	// ConfigCapacityBytes содержит ограниченный бюджет пары конфигураций с учётом общего бюджета
	// очереди.
	ConfigCapacityBytes uint64
	// AckAheadEventsTotal считает аномалии опережения ACK; AckAheadBytesLast содержит последнее
	// опережение в байтах; AckAheadBytesMax содержит максимальное опережение.
	AckAheadEventsTotal, AckAheadBytesLast, AckAheadBytesMax uint64
	// AckLastAnomalyAt содержит время последней аномалии ACK.
	AckLastAnomalyAt time.Time
}

// Snapshot согласует активную подписку с переносом её окончательных счётчиков.
func (p *Publisher) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	// s собирает согласованный снимок накоплений, подписки, транспорта и обработчиков записи.
	s := p.metrics
	s.State, s.Failure, s.NetworkJoined = p.state, p.failure, p.joined
	s.QueueDrops = p.queueDrops
	if p.sub != nil {
		// q содержит снимок активной подписки до объединения её текущих потерь.
		q := p.sub.Snapshot()
		s.QueuedPackets, s.PendingPackets = q.QueuedPackets, q.PendingPackets
		s.PacketBytes, s.ConfigBytes, s.Generation, s.Resync = q.PacketBytes, q.ConfigBytes, q.Generation, q.Resync
		addQueueDrops(&s.QueueDrops, q.Drops, &s.Saturated)
		s.Saturated = s.Saturated || q.Saturated
	}
	// Обработчик записи использует отдельную блокировку и не захватывает p.mu.
	if p.conn != nil {
		// wire содержит текущий логический резерв транспорта и признак насыщения его измерений.
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

// Прибавляет счётчик без переполнения, закрепляя MaxUint64 и признак насыщения.
func saturate(dst *uint64, n uint64, saturated *bool) {
	if n > math.MaxUint64-*dst {
		*dst = math.MaxUint64
		*saturated = true
		return
	}
	*dst += n
}

// Переносит все причины потерь подписки в итоговые счётчики с насыщением каждого поля.
func addQueueDrops(dst *media.SubscriptionDrops, src media.SubscriptionDrops, saturated *bool) {
	// pair связывает конкретный итоговый счётчик потерь с добавляемым накоплением закрытой либо
	// активной подписки.
	for _, pair := range []struct {
		// dst указывает на итоговый счётчик выбранной причины потери.
		dst *uint64
		// n содержит число потерь выбранной причины, добавляемое с насыщением.
		n uint64
	}{
		{&dst.Overflow, src.Overflow}, {&dst.Oversized, src.Oversized}, {&dst.ConfigRejected, src.ConfigRejected}, {&dst.AwaitingKeyframe, src.AwaitingKeyframe},
		{&dst.AudioBeforeCut, src.AudioBeforeCut}, {&dst.SourceReset, src.SourceReset}, {&dst.ConfigReset, src.ConfigReset}, {&dst.Closed, src.Closed},
	} {
		saturate(pair.dst, pair.n, saturated)
	}
}

// Учитывает фактически принятые транспортом положительные байты и время записи под отдельной
// блокировкой, не захватывая p.mu.
func (p *Publisher) noteWrite(n int) {
	if n <= 0 {
		return
	}
	p.writeMu.Lock()
	saturate(&p.writtenBytes, uint64(n), &p.writeSaturated)
	p.lastWrite = time.Now().UTC()
	p.writeMu.Unlock()
}

// Атомарно для снимка учитывает очередное опережение ACK, его последнее и максимальное значение и
// время; насыщение счётчика не останавливает остальные поля.
func (p *Publisher) noteACKAnomaly(bytes uint64, at time.Time) {
	p.writeMu.Lock()
	saturate(&p.ackAheadEventsTotal, 1, &p.writeSaturated)
	p.ackAheadBytesLast = bytes
	p.ackAheadBytesMax = max(p.ackAheadBytesMax, bytes)
	p.ackLastAnomalyAt = at
	p.writeMu.Unlock()
}

// Устанавливает активную подписку под блокировкой, согласующей её со снимком и переносом итоговых
// потерь.
func (p *Publisher) attachSubscription(s *media.Subscription) {
	p.mu.Lock()
	p.sub = s
	p.mu.Unlock()
}

// Закрывает активную подписку и единожды переносит окончательные потери в накопления Publisher перед
// освобождением ссылки.
func (p *Publisher) closeSubscription() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sub == nil {
		return
	}
	_ = p.sub.Close()
	// final содержит окончательные потери закрытой подписки перед освобождением ссылки.
	final := p.sub.Snapshot()
	addQueueDrops(&p.queueDrops, final.Drops, &p.metrics.Saturated)
	p.metrics.Saturated = p.metrics.Saturated || final.Saturated
	p.sub = nil
}

// Учитывает логический вес полученной единицы доступа и двух конфигураций до её отправки или
// отбрасывания.
func (p *Publisher) hold(d media.Delivery) {
	p.mu.Lock()
	p.metrics.InFlightPacketBytes = d.Packet.StoredBytes()
	p.metrics.InFlightConfigBytes = d.VideoConfig.StoredBytes() + d.AudioConfig.StoredBytes()
	p.mu.Unlock()
}

// Учитывает полностью отправленное медиасообщение, фактический формат и время дорожки и освобождает
// вес удержанной выдачи подписки.
func (p *Publisher) delivered(d media.Delivery, bytes uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	saturate(&p.metrics.PayloadBytes, bytes, &p.metrics.Saturated)
	saturate(&p.metrics.AccessUnits, 1, &p.metrics.Saturated)
	if d.Packet.Config().Info().Track == media.TrackVideo {
		saturate(&p.metrics.VideoFrames, 1, &p.metrics.Saturated)
		// v содержит фактический формат полностью отправленного видеопакета.
		v := d.Packet.Config().Info().Video
		p.metrics.Format.VideoCodec, p.metrics.Format.Width, p.metrics.Format.Height = v.Codec, v.Width, v.Height
		p.metrics.LastFrame = time.Now()
	} else {
		// a содержит фактический формат полностью отправленного аудиопакета.
		a := d.Packet.Config().Info().Audio
		p.metrics.Format.AudioCodec, p.metrics.Format.SampleRateHz, p.metrics.Format.Channels = a.Codec, a.SampleRateHz, a.Channels
	}
	p.metrics.LastMedia = time.Now().UTC()
	p.metrics.InFlightPacketBytes, p.metrics.InFlightConfigBytes = 0, 0
}

// dropReason различает причины потери выдачи после её получения из подписки.
type dropReason uint8

const (
	// dropStale обозначает выдачу уже отозванного поколения источника.
	dropStale dropReason = iota
	// dropTransport обозначает потерю при сетевой ошибке передачи.
	dropTransport
	// dropCancelled обозначает потерю после отмены контекста передачи.
	dropCancelled
	// dropTerminal обозначает потерю при окончательном отказе выхода.
	dropTerminal
)

// Учитывает одну потерянную выдачу подписки по заданной причине и освобождает её логический вес.
func (p *Publisher) abandoned(reason dropReason) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// count выбирает счётчик конкретной причины отбрасывания полученной выдачи.
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

// Преобразует ошибку в ограниченную оперативную причину, отдельно выделяя отзыв полномочий и отказ
// источника.
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
