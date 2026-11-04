package media

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sync"
	"time"
)

var (
	// ErrRouterClosed обозначает остановленный маршрутизатор.
	ErrRouterClosed = errors.New("маршрутизатор закрыт")
	// ErrStaleSource обозначает отозванную публикацию либо устаревшую конфигурацию.
	ErrStaleSource = errors.New("устаревший источник или конфигурация")
	// ErrDTSRegression обозначает уменьшение времени декодирования дорожки.
	ErrDTSRegression = errors.New("время декодирования уменьшилось")
	// ErrRouterBudget обозначает превышение общего бюджета маршрутизатора.
	ErrRouterBudget = errors.New("превышен бюджет маршрутизатора")
	// ErrInvalidRouterLimits обозначает некорректные общие ограничения.
	ErrInvalidRouterLimits = errors.New("некорректные ограничения маршрутизатора")
)

// RouterLimits ограничивает число очередей, сумму их резервов и заголовки источника.
// MaxRetainedBytes включает MaxBytes каждой подписки и два MaxConfigBytes источника.
// Это логический бюджет сжатых данных; число объектов ограничено отдельно.
type RouterLimits struct {
	MaxConsumers     uint32
	MaxRetainedBytes uint64
	MaxConfigBytes   uint64
}

// Validate проверяет конечные положительные бюджеты без переполнения.
func (l RouterLimits) Validate() error {
	if l.MaxConsumers == 0 || l.MaxConfigBytes == 0 || l.MaxConfigBytes > math.MaxUint64/2 || l.MaxRetainedBytes <= 2*l.MaxConfigBytes {
		return ErrInvalidRouterLimits
	}
	return nil
}

// Router распределяет один текущий источник по независимым ограниченным очередям.
// Владелец выполняет проверку допуска снаружи; Router не вызывает внешние функции.
type Router struct {
	mu                 sync.Mutex
	limits             RouterLimits
	reserved           uint64
	epoch              uint64
	source             *Source
	video, audio       CodecConfig
	hasConfigs         bool
	videoDTS, audioDTS routerTimestamp
	subscriptions      map[*Subscription]struct{}
	closed             bool
}

type routerTimestamp struct {
	ticks int64
	base  TimeBase
	valid bool
}

// Source является непрозрачным допуском одной публикации, отдельным от SourceEpoch.
// Повторное использование epoch и восстановление отозванного Source запрещены.
type Source struct {
	router *Router
	epoch  uint64
}

// NewRouter создаёт маршрутизатор без фонового работника.
func NewRouter(limits RouterLimits) (*Router, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return &Router{limits: limits, reserved: 2 * limits.MaxConfigBytes, subscriptions: make(map[*Subscription]struct{})}, nil
}

// BeginSource заменяет публикацию и начинает новую непрерывную шкалу обеих дорожек.
func (r *Router) BeginSource() (*Source, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrRouterClosed
	}
	if r.epoch == math.MaxUint64 {
		return nil, fmt.Errorf("%w: исчерпаны SourceEpoch", ErrRouterBudget)
	}
	r.epoch++
	source := &Source{router: r, epoch: r.epoch}
	r.source = source
	r.hasConfigs = false
	r.video, r.audio = CodecConfig{}, CodecConfig{}
	r.videoDTS, r.audioDTS = routerTimestamp{}, routerTimestamp{}
	for s := range r.subscriptions {
		s.resetLocked(time.Now())
	}
	return source, nil
}

// ReplaceSource выполняет ту же атомарную замену, что и BeginSource.
func (r *Router) ReplaceSource() (*Source, error) { return r.BeginSource() }

// Epoch возвращает непереиспользуемую идентичность непрерывного сегмента потока.
func (s *Source) Epoch() uint64 {
	if s == nil {
		return 0
	}
	return s.epoch
}

// UpdateConfigs атомарно публикует согласованную пару конфигураций обеих дорожек.
// Один номер ревизии всегда обозначает одни и те же байты и описание.
func (s *Source) UpdateConfigs(video, audio CodecConfig) error {
	if s == nil || s.router == nil {
		return ErrStaleSource
	}
	r := s.router
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkSourceLocked(s); err != nil {
		return err
	}
	for _, config := range []CodecConfig{video, audio} {
		if err := config.Validate(); err != nil {
			return err
		}
		if config.info.SourceEpoch != s.epoch {
			return ErrStaleSource
		}
		if config.StoredBytes() > r.limits.MaxConfigBytes {
			return ErrRouterBudget
		}
	}
	if video.info.Track != TrackVideo || audio.info.Track != TrackAudio {
		return ErrInvalidConfig
	}
	if r.hasConfigs {
		for _, pair := range [][2]CodecConfig{{r.video, video}, {r.audio, audio}} {
			if pair[1].info.Revision < pair[0].info.Revision || (pair[1].info.Revision == pair[0].info.Revision && !routerSameConfig(pair[0], pair[1])) {
				return ErrStaleSource
			}
		}
		if routerSameConfig(r.video, video) && routerSameConfig(r.audio, audio) {
			return nil
		}
	}
	r.video, r.audio, r.hasConfigs = video, audio, true
	for sub := range r.subscriptions {
		sub.resetLocked(time.Now())
	}
	return nil
}

// Publish принимает пакет только текущей публикации и текущей ревизии дорожки.
// Переполнение отдельной подписки не превращается в ошибку всего источника.
func (s *Source) Publish(packet Packet) error {
	if s == nil || s.router == nil {
		return ErrStaleSource
	}
	r := s.router
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkSourceLocked(s); err != nil {
		return err
	}
	if err := packet.Validate(); err != nil {
		return err
	}
	if !r.currentPacketLocked(packet) {
		return ErrStaleSource
	}
	last := &r.videoDTS
	if packet.config.info.Track == TrackAudio {
		last = &r.audioDTS
	}
	if last.valid && routerCompareTimestamp(packet.info.DTS, packet.config.info.TimeBase, last.ticks, last.base) < 0 {
		return ErrDTSRegression
	}
	*last = routerTimestamp{ticks: packet.info.DTS, base: packet.config.info.TimeBase, valid: true}
	now := time.Now()
	for sub := range r.subscriptions {
		sub.enqueueLocked(packet, now)
	}
	return nil
}

// Disconnect отзывает только соответствующий Source и очищает его очереди.
func (s *Source) Disconnect() error {
	if s == nil || s.router == nil {
		return ErrStaleSource
	}
	r := s.router
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkSourceLocked(s); err != nil {
		return err
	}
	r.source = nil
	r.hasConfigs = false
	r.video, r.audio = CodecConfig{}, CodecConfig{}
	for sub := range r.subscriptions {
		sub.resetLocked(time.Now())
	}
	return nil
}

// Subscribe резервирует ограниченную очередь в общем бюджете.
func (r *Router) Subscribe(limits QueueLimits) (*Subscription, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrRouterClosed
	}
	if uint64(len(r.subscriptions)) >= uint64(r.limits.MaxConsumers) || limits.MaxBytes > r.limits.MaxRetainedBytes-r.reserved {
		return nil, ErrRouterBudget
	}
	s := &Subscription{router: r, limits: limits, changed: make(chan struct{}), done: make(chan struct{})}
	r.reserved += limits.MaxBytes
	r.subscriptions[s] = struct{}{}
	s.resetLocked(time.Now())
	return s, nil
}

// Close отзывает источник и завершает все подписки.
func (r *Router) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	r.source = nil
	r.hasConfigs = false
	r.video, r.audio = CodecConfig{}, CodecConfig{}
	for s := range r.subscriptions {
		s.closeLocked(ErrRouterClosed)
	}
	return nil
}

func (r *Router) checkSourceLocked(s *Source) error {
	if r.closed {
		return ErrRouterClosed
	}
	if r.source != s {
		return ErrStaleSource
	}
	return nil
}

func (r *Router) currentPacketLocked(p Packet) bool {
	if !r.hasConfigs || r.source == nil {
		return false
	}
	current := r.video
	if p.config.info.Track == TrackAudio {
		current = r.audio
	}
	return routerSameConfig(current, p.config)
}

func routerSameConfig(a, b CodecConfig) bool {
	return a.info == b.info && bytes.Equal(a.headers, b.headers)
}

// Все множители расширяются до умножения; отрицательные ticks и крайние int64 точны.
func routerCompareTimestamp(a int64, ab TimeBase, b int64, bb TimeBase) int {
	var left, right, factor big.Int
	left.SetInt64(a)
	left.Mul(&left, factor.SetUint64(uint64(ab.Numerator)))
	left.Mul(&left, factor.SetUint64(uint64(bb.Denominator)))
	right.SetInt64(b)
	right.Mul(&right, factor.SetUint64(uint64(bb.Numerator)))
	right.Mul(&right, factor.SetUint64(uint64(ab.Denominator)))
	return left.Cmp(&right)
}
