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
	// MaxConsumers ограничивает число одновременно зарегистрированных подписок.
	MaxConsumers     uint32
	MaxRetainedBytes uint64
	// MaxConfigBytes ограничивает размер заголовков каждой из двух дорожек источника, в байтах.
	MaxConfigBytes uint64
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
	// mu защищает источник, конфигурации, бюджеты и состояние всех подписок.
	mu sync.Mutex
	// limits хранит проверенные общие ограничения маршрутизатора.
	limits RouterLimits
	// reserved учитывает два резерва заголовков источника и MaxBytes каждой зарегистрированной
	// подписки.
	reserved uint64
	// epoch хранит последний выданный номер непрерывного сегмента; при BeginSource увеличивается без
	// повторного использования.
	epoch uint64
	// source указывает на действующий допуск публикации либо равен nil после отзыва.
	source *Source
	// video хранит текущую конфигурацию видеодорожки. audio хранит текущую конфигурацию
	// аудиодорожки.
	video, audio CodecConfig
	// hasConfigs показывает, опубликована ли согласованная пара текущих конфигураций.
	hasConfigs bool
	// videoDTS хранит последнее принятое время декодирования видео. audioDTS хранит последнее
	// принятое время декодирования аудио.
	videoDTS, audioDTS routerTimestamp
	// subscriptions содержит подписки, для которых зарезервированы очереди.
	subscriptions map[*Subscription]struct{}
	// closed запрещает новые источники, подписки и публикацию после остановки маршрутизатора.
	closed bool
	// ingress хранит состояние допуска OBS и накопленные входные измерения.
	ingress IngressSnapshot
}

// routerTimestamp хранит временную метку вместе с её единицей времени и признаком наличия.
type routerTimestamp struct {
	// ticks хранит знаковое число отсчётов временной метки.
	ticks int64
	// base задаёт длительность одного отсчёта ticks в секундах.
	base TimeBase
	// valid отличает установленную временную метку от нулевого состояния.
	valid bool
}

// Source является непрозрачным допуском одной публикации, отдельным от SourceEpoch.
// Повторное использование SourceEpoch и восстановление отозванного Source запрещены.
type Source struct {
	// router связывает допуск с маршрутизатором, который его выдал.
	router *Router
	// epoch сохраняет номер непрерывного сегмента, назначенный при выдаче допуска.
	epoch uint64
}

// NewRouter создаёт маршрутизатор без фонового работника.
func NewRouter(limits RouterLimits) (*Router, error) {
	// err сообщает о некорректных общих бюджетах маршрутизатора.
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
	// source содержит новый допуск публикации с только что выделенным сегментом источника.
	source := &Source{router: r, epoch: r.epoch}
	r.source = source
	r.hasConfigs = false
	r.video, r.audio = CodecConfig{}, CodecConfig{}
	r.videoDTS, r.audioDTS = routerTimestamp{}, routerTimestamp{}
	// s обозначает подписку, очередь которой сбрасывается при замене источника.
	for s := range r.subscriptions {
		s.resetForLocked(time.Now(), dropSourceReset)
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
	// r обозначает маршрутизатор, выдавший обновляемый допуск.
	r := s.router
	r.mu.Lock()
	defer r.mu.Unlock()
	// err сообщает, что маршрутизатор закрыт либо допуск уже отозван.
	if err := r.checkSourceLocked(s); err != nil {
		return err
	}
	// config обозначает проверяемую конфигурацию одной из двух дорожек.
	for _, config := range []CodecConfig{video, audio} {
		// err сообщает о некорректном описании или заголовках проверяемой дорожки.
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
		// pair содержит прежнюю и предлагаемую конфигурации одной дорожки для проверки
		// монотонности ревизии.
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
	// sub обозначает подписку, доставки которой отзываются после смены конфигураций.
	for sub := range r.subscriptions {
		sub.resetForLocked(time.Now(), dropConfigReset)
	}
	return nil
}

// Publish принимает пакет только текущей публикации и текущей ревизии дорожки.
// Переполнение отдельной подписки не превращается в ошибку всего источника.
func (s *Source) Publish(packet Packet) error {
	if s == nil || s.router == nil {
		return ErrStaleSource
	}
	// r обозначает маршрутизатор допуска, от имени которого публикуется пакет.
	r := s.router
	r.mu.Lock()
	defer r.mu.Unlock()
	// err сообщает, что маршрутизатор закрыт либо публикация использует отозванный допуск.
	if err := r.checkSourceLocked(s); err != nil {
		return err
	}
	// err сообщает о некорректной структуре публикуемого пакета.
	if err := packet.Validate(); err != nil {
		return err
	}
	if !r.currentPacketLocked(packet) {
		return ErrStaleSource
	}
	// last указывает на последнюю принятую метку DTS той дорожки, которой принадлежит пакет.
	last := &r.videoDTS
	if packet.config.info.Track == TrackAudio {
		last = &r.audioDTS
	}
	if last.valid && routerCompareTimestamp(packet.info.DTS, packet.config.info.TimeBase, last.ticks, last.base) < 0 {
		return ErrDTSRegression
	}
	*last = routerTimestamp{ticks: packet.info.DTS, base: packet.config.info.TimeBase, valid: true}
	// now задаёт общее время поступления пакета для всех очередей подписчиков.
	now := time.Now()
	// sub обозначает подписку, в независимую очередь которой передаётся пакет.
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
	// r обозначает маршрутизатор отзываемого допуска.
	r := s.router
	r.mu.Lock()
	defer r.mu.Unlock()
	// err сообщает, что маршрутизатор закрыт либо этот допуск уже заменён.
	if err := r.checkSourceLocked(s); err != nil {
		return err
	}
	r.source = nil
	r.hasConfigs = false
	r.video, r.audio = CodecConfig{}, CodecConfig{}
	// sub обозначает подписку, очередь которой очищается после отключения источника.
	for sub := range r.subscriptions {
		sub.resetForLocked(time.Now(), dropSourceReset)
	}
	return nil
}

// Subscribe резервирует ограниченную очередь в общем бюджете.
func (r *Router) Subscribe(limits QueueLimits) (*Subscription, error) {
	// err сообщает о некорректных ограничениях создаваемой очереди.
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
	// s содержит новую подписку с каналами уведомления и завершения до регистрации в маршрутизаторе.
	s := &Subscription{router: r, limits: limits, changed: make(chan struct{}), done: make(chan struct{})}
	r.reserved += limits.MaxBytes
	r.subscriptions[s] = struct{}{}
	s.resetForLocked(time.Now(), dropSourceReset)
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
	r.ingress.Connected = false
	r.source = nil
	r.hasConfigs = false
	r.video, r.audio = CodecConfig{}, CodecConfig{}
	// s обозначает подписку, завершаемую при закрытии маршрутизатора.
	for s := range r.subscriptions {
		s.closeLocked(ErrRouterClosed)
	}
	return nil
}

// Проверяет при удерживаемой r.mu, что маршрутизатор открыт и Source всё ещё является текущим
// допуском.
func (r *Router) checkSourceLocked(s *Source) error {
	if r.closed {
		return ErrRouterClosed
	}
	if r.source != s {
		return ErrStaleSource
	}
	return nil
}

// Проверяет при удерживаемой r.mu точное совпадение конфигурации пакета с текущей конфигурацией его
// дорожки.
func (r *Router) currentPacketLocked(p Packet) bool {
	if !r.hasConfigs || r.source == nil {
		return false
	}
	// current содержит текущую конфигурацию той дорожки, которой принадлежит проверяемый пакет.
	current := r.video
	if p.config.info.Track == TrackAudio {
		current = r.audio
	}
	return routerSameConfig(current, p.config)
}

// Сравнивает все поля описания конфигурации и точные байты заголовков; математически равные, но иначе
// записанные шкалы здесь различаются.
func routerSameConfig(a, b CodecConfig) bool {
	return a.info == b.info && bytes.Equal(a.headers, b.headers)
}

// Все множители расширяются до умножения; отрицательные отсчёты и крайние значения int64 точны.
func routerCompareTimestamp(a int64, ab TimeBase, b int64, bb TimeBase) int {
	// left хранит первую метку после приведения к общему знаменателю. right хранит вторую
	// приведённую метку. factor служит временным множителем произвольной точности при приведении
	// шкал.
	var left, right, factor big.Int
	left.SetInt64(a)
	left.Mul(&left, factor.SetUint64(uint64(ab.Numerator)))
	left.Mul(&left, factor.SetUint64(uint64(bb.Denominator)))
	right.SetInt64(b)
	right.Mul(&right, factor.SetUint64(uint64(bb.Numerator)))
	right.Mul(&right, factor.SetUint64(uint64(ab.Denominator)))
	return left.Cmp(&right)
}
