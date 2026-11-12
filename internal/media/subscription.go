package media

import (
	"context"
	"errors"
	"math"
	"time"
)

var (
	// ErrSubscriptionGeneration обозначает терминальное исчерпание идентичностей сброса.
	ErrSubscriptionGeneration = errors.New("исчерпаны поколения подписки")
	// ErrSubscriptionClosed обозначает закрытую владельцем подписку.
	ErrSubscriptionClosed = errors.New("подписка закрыта")
	// ErrResyncTimeout обозначает истечение срока согласованного запуска A/V.
	ErrResyncTimeout = errors.New("истёк срок синхронизации подписки")
)

// Delivery передаёт владение неизменяемым пакетом вместе с атомарным снимком A/V.
// Перед сериализацией получатель повторно проверяет IsCurrent и идентичность попытки работника.
// Проверка не удерживает актуальность во время последующего внешнего ввода-вывода.
type Delivery struct {
	// Generation сохраняет поколение сброса подписки на момент выдачи пакета.
	Generation uint64
	// Packet содержит извлечённую из очереди неизменяемую единицу доступа.
	Packet Packet
	// VideoConfig сохраняет текущую видеоконфигурацию на момент выдачи пакета.
	VideoConfig CodecConfig
	// AudioConfig сохраняет текущую аудиоконфигурацию на момент выдачи пакета.
	AudioConfig CodecConfig
	// subscription связывает доставку с подпиской, которая её выдала.
	subscription *Subscription
	// source сохраняет допуск публикации, от которого была выдана доставка.
	source *Source
}

// IsCurrent проверяет источник, обе ревизии и поколение сброса после выдачи пакета.
func (d Delivery) IsCurrent() bool {
	if d.subscription == nil {
		return false
	}
	return d.subscription.IsCurrent(d)
}

// Subscription содержит отдельный бюджет, очередь и конечное ожидание синхронизации.
// Несколько Next безопасны, но каждый пакет передаётся только одному вызову.
type Subscription struct {
	// router указывает на маршрутизатор, чья блокировка защищает состояние подписки.
	router *Router
	// limits хранит проверенные ограничения отдельной очереди.
	limits QueueLimits
	// queue удерживает пакеты до передачи через Next.
	queue []Packet
	// pendingAudio удерживает аудио, поступившее до выбора видеопакета с заявленным Keyframe.
	pendingAudio []Packet
	// packetBytes считает логический вес пакетов queue и pendingAudio в байтах.
	packetBytes uint64
	// configBytes хранит логический вес пары заголовков, принятой в бюджет подписки; при отсутствии
	// подходящей пары или завершении подписки равен нулю.
	configBytes uint64
	// configFits показывает, помещается ли текущая пара конфигураций в ограничения подписки.
	configFits bool
	// resync блокирует выдачу пакетов до согласованного запуска видео и аудио.
	resync bool
	// hasVideo показывает, выбран ли видеопакет с заявленным Keyframe для текущей синхронизации.
	hasVideo bool
	// cut сохраняет PTS выбранного видеопакета с Keyframe; более раннее аудио отбрасывается.
	cut routerTimestamp
	// deadline хранит первоначальный срок текущего ожидания синхронизации и не продлевается
	// повторными сбросами.
	deadline time.Time
	// timer вызывает проверку истечения срока синхронизации под блокировкой маршрутизатора.
	timer *time.Timer
	// changed закрывается при изменении состояния, чтобы пробудить текущие вызовы Next, после чего
	// заменяется новым каналом.
	changed chan struct{}
	// done закрывается однократно при окончательном завершении подписки.
	done chan struct{}
	// terminal хранит окончательную причину завершения; nil обозначает работающую подписку.
	terminal error
	// generation увеличивается при каждом сбросе и позволяет отзывать ранее выданные доставки.
	generation uint64
	// drops накапливает число непереданных пакетов по причинам отбрасывания.
	drops SubscriptionDrops
	// saturated отмечает переполнение хотя бы одного счётчика потерь.
	saturated bool
}

// Next ждёт пакет или завершение. Успешное удаление из очереди передаёт владение.
func (s *Subscription) Next(ctx context.Context) (Delivery, error) {
	// r обозначает маршрутизатор, блокировка которого защищает извлечение пакета.
	r := s.router
	for {
		// err сообщает об отмене или истечении срока контекста ожидания пакета.
		if err := ctx.Err(); err != nil {
			return Delivery{}, err
		}
		r.mu.Lock()
		s.expireLocked(time.Now())
		if s.terminal != nil {
			// err сохраняет окончательную причину завершения подписки до снятия блокировки.
			err := s.terminal
			r.mu.Unlock()
			return Delivery{}, err
		}
		if !s.resync && len(s.queue) > 0 {
			// packet содержит первый пакет очереди, извлекаемый для передачи вызывающему.
			packet := s.queue[0]
			s.queue[0] = Packet{}
			s.queue = s.queue[1:]
			s.packetBytes -= packet.StoredBytes()
			if !r.currentPacketLocked(packet) {
				s.dropLocked(dropSourceReset, 1)
				r.mu.Unlock()
				continue
			}
			// delivery объединяет извлечённый пакет и согласованный снимок источника,
			// конфигураций и поколения.
			delivery := Delivery{Generation: s.generation, Packet: packet, VideoConfig: r.video, AudioConfig: r.audio, subscription: s, source: r.source}
			r.mu.Unlock()
			return delivery, nil
		}
		// changed сохраняет текущий канал уведомления до снятия блокировки и начала ожидания.
		changed := s.changed
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return Delivery{}, ctx.Err()
		case <-changed:
		}
	}
}

// IsCurrent проверяет происхождение доставки, публикацию, ревизии и поколение сброса.
func (s *Subscription) IsCurrent(d Delivery) bool {
	// r обозначает маршрутизатор, под блокировкой которого проверяется актуальность доставки.
	r := s.router
	r.mu.Lock()
	defer r.mu.Unlock()
	s.expireLocked(time.Now())
	return s.terminal == nil && d.Generation == s.generation && d.subscription == s && d.source != nil && r.source == d.source && r.hasConfigs && routerSameConfig(r.video, d.VideoConfig) && routerSameConfig(r.audio, d.AudioConfig) && r.currentPacketLocked(d.Packet)
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

// Пробуждает текущие ожидания Next и создаёт новый канал уведомления; вызывающий удерживает блокировку
// Router.
func (s *Subscription) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// Освобождает очередь и ожидающее аудио, обнуляя их вес и точку среза; счётчики потерь обновляет
// вызывающий под той же блокировкой.
func (s *Subscription) clearPacketsLocked() {
	s.queue, s.pendingAudio = nil, nil
	s.packetBytes = 0
	s.hasVideo = false
	s.cut = routerTimestamp{}
}

// Начинает восстановление после переполнения с учётом прежних пакетов как Overflow; требует блокировку
// Router.
func (s *Subscription) resetLocked(now time.Time) { s.resetForLocked(now, dropOverflow) }

// Отзывает прежние доставки новым поколением, учитывает очищаемые AU и пересчитывает бюджет заголовков
// под блокировкой Router. Повторный сброс во время синхронизации сохраняет первоначальный срок;
// исчерпание поколений закрывает подписку.
func (s *Subscription) resetForLocked(now time.Time, reason subscriptionDropReason) {
	if s.terminal != nil {
		return
	}
	if s.generation == math.MaxUint64 {
		s.closeLocked(ErrSubscriptionGeneration)
		return
	}
	s.generation++
	s.dropLocked(reason, s.countLocked())
	s.clearPacketsLocked()
	s.configBytes, s.configFits = 0, false
	// r обозначает маршрутизатор с текущими конфигурациями для пересчёта бюджета подписки.
	r := s.router
	if r.hasConfigs && r.video.StoredBytes() <= s.limits.MaxConfigBytes && r.audio.StoredBytes() <= s.limits.MaxConfigBytes {
		// total содержит суммарный размер заголовков видео и аудио в байтах.
		total := r.video.StoredBytes() + r.audio.StoredBytes()
		if total <= s.limits.MaxBytes {
			s.configBytes, s.configFits = total, true
		}
	}
	if !s.resync {
		s.resync = true
		s.deadline = now.Add(s.limits.ResyncTimeout)
		s.timer = time.AfterFunc(s.limits.ResyncTimeout, func() {
			// Под блокировкой маршрутизатора проверяет истечение текущего срока синхронизации; готовую или уже
			// завершённую подписку повторно не завершает.

			r.mu.Lock()
			defer r.mu.Unlock()
			s.expireLocked(time.Now())
		})
	}
	s.expireLocked(now)
	s.notifyLocked()
}

// Закрывает подписку с ErrResyncTimeout, если первоначальный срок текущей синхронизации истёк; требует
// блокировку Router.
func (s *Subscription) expireLocked(now time.Time) {
	if s.terminal == nil && s.resync && !now.Before(s.deadline) {
		s.closeLocked(ErrResyncTimeout)
	}
}

// Однократно завершает подписку под блокировкой Router: останавливает таймер, учитывает оставшиеся AU
// как Closed, освобождает резерв и будит ожидания.
func (s *Subscription) closeLocked(err error) {
	if s.terminal != nil {
		return
	}
	s.terminal = err
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.dropLocked(dropClosed, s.countLocked())
	s.clearPacketsLocked()
	s.configBytes, s.configFits = 0, false
	delete(s.router.subscriptions, s)
	s.router.reserved -= s.limits.MaxBytes
	close(s.done)
	s.notifyLocked()
}

// Считает AU в очереди и ожидающем аудио при удерживаемой блокировке Router.
func (s *Subscription) countLocked() uint64 {
	return uint64(len(s.queue)) + uint64(len(s.pendingAudio))
}

// Проверяет вес пакета, число удерживаемых AU и общий бюджет с заголовками без переполнения
// арифметики; требует блокировку Router.
func (s *Subscription) fitsLocked(p Packet) bool {
	// weight содержит логический вес проверяемого пакета вместе с его конфигурацией, в байтах.
	weight := p.StoredBytes()
	return weight <= s.limits.MaxPacketBytes && s.countLocked() < uint64(s.limits.MaxPackets) && s.configBytes <= s.limits.MaxBytes && s.packetBytes <= s.limits.MaxBytes-s.configBytes && weight <= s.limits.MaxBytes-s.configBytes-s.packetBytes
}

// Принимает пакет под блокировкой Router с независимым учётом потерь подписки. После сброса ждёт
// видеопакет с объявленным PacketInfo.Keyframe и AAC не раньше его PTS, удерживая раннее аудио в том же
// бюджете. Здесь проверяются признаки и метки пакетов; содержимое видеопотока повторно не разбирается.
func (s *Subscription) enqueueLocked(p Packet, now time.Time) {
	s.expireLocked(now)
	if s.terminal != nil {
		s.dropLocked(dropClosed, 1)
		return
	}
	if !s.router.currentPacketLocked(p) {
		s.dropLocked(dropSourceReset, 1)
		return
	}
	if !s.configFits {
		s.dropLocked(dropConfigRejected, 1)
		return
	}
	if !s.fitsLocked(p) {
		s.resetLocked(now)
		if s.terminal != nil {
			s.dropLocked(dropClosed, 1)
			return
		}
		if !s.configFits {
			s.dropLocked(dropConfigRejected, 1)
			return
		}
		if !s.fitsLocked(p) {
			s.dropLocked(dropOversized, 1)
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
		// Полная аудиоединица начинается не раньше PTS выбранного стартового видеопакета.
		if routerCompareTimestamp(p.info.PTS, p.config.info.TimeBase, s.cut.ticks, s.cut.base) < 0 {
			s.dropLocked(dropAudioBeforeCut, 1)
			return
		}
		s.queue = append(s.queue, p)
		s.packetBytes += p.StoredBytes()
		s.readyLocked()
		return
	}
	if !s.hasVideo {
		if !p.info.Keyframe {
			s.dropLocked(dropAwaitingKeyframe, 1)
			return
		}
		s.hasVideo = true
		s.cut = routerTimestamp{ticks: p.info.PTS, base: p.config.info.TimeBase, valid: true}
		s.queue = append(s.queue, p)
		s.packetBytes += p.StoredBytes()
		// pending сохраняет накопленное до ключевого кадра аудио для отбора относительно точки
		// среза.
		pending := s.pendingAudio
		s.pendingAudio = nil
		// audio обозначает ожидающую аудиоединицу, начало которой сравнивается с PTS ключевого
		// видеокадра.
		for _, audio := range pending {
			if routerCompareTimestamp(audio.info.PTS, audio.config.info.TimeBase, s.cut.ticks, s.cut.base) >= 0 {
				s.queue = append(s.queue, audio)
			} else {
				s.packetBytes -= audio.StoredBytes()
				s.dropLocked(dropAudioBeforeCut, 1)
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

// Завершает синхронизацию после согласованного A/V: снимает срок, останавливает таймер и пробуждает
// Next под блокировкой Router.
func (s *Subscription) readyLocked() {
	s.resync = false
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.deadline = time.Time{}
	s.notifyLocked()
}
