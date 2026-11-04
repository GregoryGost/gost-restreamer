package media

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrSubscriptionClosed обозначает закрытую владельцем подписку.
	ErrSubscriptionClosed = errors.New("подписка закрыта")
	// ErrResyncTimeout обозначает истечение срока согласованного запуска A/V.
	ErrResyncTimeout = errors.New("истёк срок синхронизации подписки")
)

// Delivery передаёт владение неизменяемым пакетом вместе с атомарным снимком A/V.
// Перед сериализацией получатель повторно проверяет IsCurrent и идентичность попытки работника.
// Проверка не удерживает актуальность во время последующего внешнего I/O.
type Delivery struct {
	Packet       Packet
	VideoConfig  CodecConfig
	AudioConfig  CodecConfig
	subscription *Subscription
	source       *Source
}

// IsCurrent проверяет актуальность источника и обеих ревизий после выдачи пакета.
func (d Delivery) IsCurrent() bool {
	if d.subscription == nil {
		return false
	}
	return d.subscription.IsCurrent(d)
}

// Subscription содержит отдельный бюджет, очередь и конечное ожидание синхронизации.
// Несколько Next безопасны, но каждый пакет передаётся только одному вызову.
type Subscription struct {
	router       *Router
	limits       QueueLimits
	queue        []Packet
	pendingAudio []Packet
	packetBytes  uint64
	configBytes  uint64
	configFits   bool
	resync       bool
	hasVideo     bool
	cut          routerTimestamp
	deadline     time.Time
	timer        *time.Timer
	changed      chan struct{}
	done         chan struct{}
	terminal     error
}

// Next ждёт пакет или завершение. Успешное удаление из очереди передаёт владение.
func (s *Subscription) Next(ctx context.Context) (Delivery, error) {
	r := s.router
	for {
		if err := ctx.Err(); err != nil {
			return Delivery{}, err
		}
		r.mu.Lock()
		s.expireLocked(time.Now())
		if s.terminal != nil {
			err := s.terminal
			r.mu.Unlock()
			return Delivery{}, err
		}
		if !s.resync && len(s.queue) > 0 {
			packet := s.queue[0]
			s.queue[0] = Packet{}
			s.queue = s.queue[1:]
			s.packetBytes -= packet.StoredBytes()
			if !r.currentPacketLocked(packet) {
				r.mu.Unlock()
				continue
			}
			delivery := Delivery{Packet: packet, VideoConfig: r.video, AudioConfig: r.audio, subscription: s, source: r.source}
			r.mu.Unlock()
			return delivery, nil
		}
		changed := s.changed
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return Delivery{}, ctx.Err()
		case <-changed:
		}
	}
}

// IsCurrent проверяет происхождение доставки, публикацию и обе текущие ревизии.
func (s *Subscription) IsCurrent(d Delivery) bool {
	r := s.router
	r.mu.Lock()
	defer r.mu.Unlock()
	s.expireLocked(time.Now())
	return s.terminal == nil && d.subscription == s && d.source != nil && r.source == d.source && r.hasConfigs && routerSameConfig(r.video, d.VideoConfig) && routerSameConfig(r.audio, d.AudioConfig) && r.currentPacketLocked(d.Packet)
}

// Done закрывается после Close, остановки Router или истечения ResyncTimeout.
func (s *Subscription) Done() <-chan struct{} { return s.done }

// Err возвращает окончательную причину завершения либо nil.
func (s *Subscription) Err() error {
	s.router.mu.Lock()
	defer s.router.mu.Unlock()
	s.expireLocked(time.Now())
	return s.terminal
}

// Close освобождает резерв и пробуждает все ожидающие Next.
func (s *Subscription) Close() error {
	s.router.mu.Lock()
	defer s.router.mu.Unlock()
	s.closeLocked(ErrSubscriptionClosed)
	return nil
}

func (s *Subscription) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Subscription) clearPacketsLocked() {
	s.queue, s.pendingAudio = nil, nil
	s.packetBytes = 0
	s.hasVideo = false
	s.cut = routerTimestamp{}
}

func (s *Subscription) resetLocked(now time.Time) {
	if s.terminal != nil {
		return
	}
	s.clearPacketsLocked()
	s.configBytes, s.configFits = 0, false
	r := s.router
	if r.hasConfigs && r.video.StoredBytes() <= s.limits.MaxConfigBytes && r.audio.StoredBytes() <= s.limits.MaxConfigBytes {
		total := r.video.StoredBytes() + r.audio.StoredBytes()
		if total <= s.limits.MaxBytes {
			s.configBytes, s.configFits = total, true
		}
	}
	if !s.resync {
		s.resync = true
		s.deadline = now.Add(s.limits.ResyncTimeout)
		s.timer = time.AfterFunc(s.limits.ResyncTimeout, func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			s.expireLocked(time.Now())
		})
	}
	s.expireLocked(now)
	s.notifyLocked()
}

func (s *Subscription) expireLocked(now time.Time) {
	if s.terminal == nil && s.resync && !now.Before(s.deadline) {
		s.closeLocked(ErrResyncTimeout)
	}
}

func (s *Subscription) closeLocked(err error) {
	if s.terminal != nil {
		return
	}
	s.terminal = err
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.clearPacketsLocked()
	s.configBytes, s.configFits = 0, false
	delete(s.router.subscriptions, s)
	s.router.reserved -= s.limits.MaxBytes
	close(s.done)
	s.notifyLocked()
}

func (s *Subscription) countLocked() uint64 {
	return uint64(len(s.queue)) + uint64(len(s.pendingAudio))
}

func (s *Subscription) fitsLocked(p Packet) bool {
	weight := p.StoredBytes()
	return weight <= s.limits.MaxPacketBytes && s.countLocked() < uint64(s.limits.MaxPackets) && s.configBytes <= s.limits.MaxBytes && s.packetBytes <= s.limits.MaxBytes-s.configBytes && weight <= s.limits.MaxBytes-s.configBytes-s.packetBytes
}

func (s *Subscription) enqueueLocked(p Packet, now time.Time) {
	s.expireLocked(now)
	if s.terminal != nil || !s.router.currentPacketLocked(p) {
		return
	}
	if !s.configFits {
		return
	}
	if !s.fitsLocked(p) {
		s.resetLocked(now)
		if s.terminal != nil || !s.configFits || !s.fitsLocked(p) {
			return
		}
	}
	if !s.resync {
		s.queue = append(s.queue, p)
		s.packetBytes += p.StoredBytes()
		s.notifyLocked()
		return
	}
	if p.config.info.Track == TrackAudio {
		if !s.hasVideo {
			s.pendingAudio = append(s.pendingAudio, p)
			s.packetBytes += p.StoredBytes()
			return
		}
		// Полная аудиоединица начинается не раньше показанного независимого кадра.
		if routerCompareTimestamp(p.info.PTS, p.config.info.TimeBase, s.cut.ticks, s.cut.base) < 0 {
			return
		}
		s.queue = append(s.queue, p)
		s.packetBytes += p.StoredBytes()
		s.readyLocked()
		return
	}
	if !s.hasVideo {
		if !p.info.Keyframe {
			return
		}
		s.hasVideo = true
		s.cut = routerTimestamp{ticks: p.info.PTS, base: p.config.info.TimeBase, valid: true}
		s.queue = append(s.queue, p)
		s.packetBytes += p.StoredBytes()
		pending := s.pendingAudio
		s.pendingAudio = nil
		for _, audio := range pending {
			if routerCompareTimestamp(audio.info.PTS, audio.config.info.TimeBase, s.cut.ticks, s.cut.base) >= 0 {
				s.queue = append(s.queue, audio)
			} else {
				s.packetBytes -= audio.StoredBytes()
			}
		}
		if len(s.queue) > 1 {
			s.readyLocked()
		}
		return
	}
	s.queue = append(s.queue, p)
	s.packetBytes += p.StoredBytes()
}

func (s *Subscription) readyLocked() {
	s.resync = false
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.deadline = time.Time{}
	s.notifyLocked()
}
