// Package group владеет общей обработкой и непрерывным выходом одной группы Session.
package group

import (
	"bytes"
	"errors"
	"math"
	"math/big"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

var (
	// ErrUnavailable обозначает отсутствие пригодного выбранного источника группы.
	ErrUnavailable = errors.New("оба источника группы недоступны")
	// ErrTiming обозначает непредставимое время, переполнение либо недопустимое расхождение при
	// переключении.
	ErrTiming = errors.New("временная шкала группы не представима")
	// ErrFeed отклоняет устаревшую попытку, непригодные пакеты или отсутствие согласованной точки
	// старта.
	ErrFeed = errors.New("устаревший или непригодный источник группы")
	// ErrFeedEnded обозначает обычное завершение входного потока как причину отзыва его пригодности.
	ErrFeedEnded = errors.New("источник группы завершился")
)

// Feed различает LIVE и заглушку при учёте попыток и выборе источника.
type Feed uint8

const (
	// Live обозначает источник текущей входной публикации.
	Live Feed = iota + 1
	// Fallback обозначает источник подготовленного файла заглушки.
	Fallback
)

// SwitcherConfig задаёт ожидаемый формат, контроль источников и пределы буферизации и перехода.
type SwitcherConfig struct {
	// Profile задаёт формат, которому должны соответствовать оба источника группы.
	Profile core.GroupProfile
	// Watchdog задаёт сроки продвижения дорожек и условия восстановления LIVE.
	Watchdog media.WatchdogPolicy
	// Queue ограничивает пакеты и конфигурации, удерживаемые отдельно для каждого источника.
	Queue media.QueueLimits
	// Router задаёт бюджеты постоянного выходного маршрутизатора группы.
	Router media.RouterLimits
	// MaxAVSkew ограничивает разницу PTS стартовых аудио и видео; MaxTransitionGap ограничивает
	// разрыв между ранней начальной меткой и первым представлением.
	MaxAVSkew, MaxTransitionGap time.Duration
}

// Validate проверяет выбранный профиль, политику контроля продвижения и статические ограничения
// очередей, маршрутизатора и времени перехода.
func (c SwitcherConfig) Validate() error {
	if c.Profile.Validate() != nil || c.Watchdog.Validate() != nil {
		return core.ErrInvalid
	}
	return c.ValidateTemplate()
}

// ValidateTemplate проверяет серверные бюджеты до выбора профиля и политики Stream.
func (c SwitcherConfig) ValidateTemplate() error {
	if c.Queue.MaxPackets < 4 || c.Queue.Validate() != nil || c.Router.Validate() != nil || c.MaxAVSkew <= 0 || c.MaxTransitionGap <= 0 {
		return core.ErrInvalid
	}
	return nil
}

// Status содержит текущий выбор источника, желание паузы, доступность и ревизии выхода.
type Status struct {
	// Source обозначает выбранный LIVE либо заглушку; ноль означает отсутствие выбранного источника.
	Source Feed
	// Paused сохраняет желание ручной паузы LIVE.
	Paused bool
	// Available показывает наличие пригодного текущего выхода.
	Available bool
	// Err содержит причину текущей недоступности выхода.
	Err error
	// Epoch содержит эпоху выходного Router; Revision содержит ревизию выходных конфигураций.
	Epoch, Revision uint64
	// StateRevision меняется при существенном изменении наблюдаемого состояния или причины отказа.
	StateRevision uint64
}

// queued связывает проверенный пакет с локальным временем получения и подтверждённым независимым
// стартом.
type queued struct {
	// packet содержит удерживаемую единицу доступа источника.
	packet media.Packet
	// at сохраняет локальное время получения пакета для проверки окна восстановления LIVE.
	at time.Time
	// key отмечает независимый видеостарт, подтверждённый инспектором кодека.
	key bool
}

// feedState хранит текущую попытку одного источника, проверенные конфигурации, буферы и признаки
// пригодности.
type feedState struct {
	// attempt задаёт текущий локальный номер попытки LIVE либо заглушки.
	attempt uint64
	// video содержит текущую конфигурацию видео источника; audio содержит его текущую конфигурацию
	// аудио.
	video, audio media.CodecConfig
	// inspector проверяет единицы доступа и независимость видеостарта в текущей конфигурации
	// источника.
	inspector codec.Inspector
	// queue хранит проверенные пакеты с известной длительностью до публикации либо выбора нового
	// старта.
	queue []queued
	// bytes содержит суммарный логический вес пакетов queue в байтах.
	bytes uint64
	// pending удерживает по одному пакету видео и аудио без длительности до следующего DTS той же
	// дорожки.
	pending [2]*queued
	// seen отмечает получение первого проверенного пакета каждой дорожки.
	seen [2]bool
	// dts хранит последний принятый DTS каждой дорожки для отклонения равных и убывающих меток.
	dts [2]int64
	// arrival хранит локальное время последнего продвижения видео и аудио.
	arrival [2]time.Time
	// stable задаёт начало текущего окна стабильного продвижения обеих дорожек.
	stable time.Time
	// rejected запрещает дальнейшие пакеты текущей попытки после подтверждённого отказа.
	rejected bool
	// failure сохраняет причину отзыва текущей попытки источника.
	failure error
	// ready отмечает проверенную согласованную точку старта или уже выбранный пригодный источник.
	ready bool
}

// Switcher буферизует LIVE и заглушку и публикует выбранный источник на общей непрерывной временной
// шкале.
type Switcher struct {
	// mu защищает состояния источников, выбор, временную шкалу и наблюдения выхода.
	mu sync.Mutex
	// config хранит проверенные ограничения и ожидаемый профиль группы.
	config SwitcherConfig
	// router содержит постоянный выход группы для независимых подписчиков.
	router *media.Router
	// source представляет текущую эпоху публикации выбранного источника в выходной Router.
	source *media.Source
	// feeds хранит отдельные состояния LIVE и заглушки.
	feeds [2]feedState
	// selected обозначает текущий выбранный источник; ноль означает отсутствие выбора.
	selected Feed
	// paused хранит желаемую ручную паузу, разрешающую только заглушку.
	paused bool
	// status сохраняет согласованное наблюдаемое состояние выхода.
	status Status
	// base задаёт общую точную шкалу видео, AAC и миллисекунд для выходных меток.
	base media.TimeBase
	// offset задаёт общий сдвиг выбранного входа в выходную шкалу; horizon хранит максимальный конец
	// уже опубликованных PTS и DTS.
	offset, horizon int64
	// hasHorizon отличает отсутствие опубликованных пакетов от нулевого выходного горизонта.
	hasHorizon bool
	// revision считает смены выходных конфигураций при начале нового выбранного источника.
	revision uint64
	// video содержит выходную видеоконфигурацию общей шкалы; audio содержит выходную
	// аудиоконфигурацию той же эпохи.
	video, audio media.CodecConfig
	// closed запрещает дальнейшие операции после закрытия выходного Router.
	closed bool
	// lastNow хранит последнее время проверки для исключения обратного хода локального времени.
	lastNow time.Time
	// stateRevision считает существенные изменения состояния с насыщением на MaxUint64.
	stateRevision uint64
	// failureRevision различает новые отказы источников при неизменной внешней недоступности.
	failureRevision uint64
	// changed закрывается при существенной смене состояния и заменяется сигналом следующего
	// изменения.
	changed chan struct{}
	// outputCounters накапливает только успешно опубликованные выходные пакеты группы.
	outputCounters media.CounterSnapshot
}

// NewSwitcher создаёт выходной Router и общую точную шкалу видео, AAC и миллисекунд; начальное
// состояние недоступно до пригодной пары дорожек.
func NewSwitcher(c SwitcherConfig) (*Switcher, error) {
	// err сообщает об отказе проверки настроек переключателя до выделения Router.
	if err := c.Validate(); err != nil {
		return nil, err
	}
	// frameRate содержит исходную частоту видео для построения общей выходной шкалы.
	frameRate := c.Profile.Format.Video.FrameRate
	// divisor содержит общий делитель числителя и знаменателя FPS.
	divisor := gcd32(frameRate.Numerator, frameRate.Denominator)
	// scale содержит точную общую шкалу; err сохраняет результаты её вычисления и создания Router.
	scale, err := fmp4.MovieTimeScale(media.TimeBase{Numerator: frameRate.Denominator / divisor, Denominator: frameRate.Numerator / divisor}, media.TimeBase{Numerator: 1, Denominator: c.Profile.Format.Audio.SampleRateHz}, media.TimeBase{Numerator: 1, Denominator: 1000})
	if err != nil {
		return nil, err
	}
	// r содержит новый постоянный выходной Router группы.
	r, err := media.NewRouter(c.Router)
	if err != nil {
		return nil, err
	}
	return &Switcher{config: c, router: r, base: media.TimeBase{Numerator: 1, Denominator: scale}, status: Status{Err: ErrUnavailable, StateRevision: 1}, stateRevision: 1, changed: make(chan struct{})}, nil
}

// Возвращает наибольший общий делитель для сокращения частоты кадров перед построением общей временной
// шкалы.
func gcd32(a, b uint32) uint32 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// Router возвращает общий выход, сохраняющийся при переключении LIVE и заглушки.
func (s *Switcher) Router() *media.Router { return s.router }

// Status возвращает согласованный снимок выбранного источника, доступности и ревизий под блокировкой.
func (s *Switcher) Status() Status { s.mu.Lock(); defer s.mu.Unlock(); return s.status }

// StateChange возвращает согласованные последнее состояние и сигнал его изменения.
func (s *Switcher) StateChange() (Status, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.changed
}

// Публикует новую ревизию и сигнал только при существенном изменении состояния или причины
// недоступности; вызывающий удерживает s.mu.
func (s *Switcher) recordStateLocked(previous Status, previousFailure uint64) {
	if previous.Source != s.status.Source || previous.Paused != s.status.Paused || previous.Available != s.status.Available || previous.Epoch != s.status.Epoch || (previous.Err == nil) != (s.status.Err == nil) || (!s.status.Available && previousFailure != s.failureRevision) {
		if s.stateRevision < math.MaxUint64 {
			s.stateRevision++
		}
		close(s.changed)
		s.changed = make(chan struct{})
	}
	s.status.StateRevision = s.stateRevision
}

// OutputSnapshot копирует накопления успешно опубликованных пакетов под блокировкой и задаёт время
// снимка.
func (s *Switcher) OutputSnapshot() media.CounterSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	// snapshot содержит независимую копию накоплений выходных пакетов с обновляемым временем чтения.
	snapshot := s.outputCounters
	snapshot.SampledAt = time.Now()
	return snapshot
}

// Сравнивает все поля описания конфигурации и точные байты её заголовков.
func sameConfig(a, b media.CodecConfig) bool {
	return a.Info() == b.Info() && bytes.Equal(a.Headers(), b.Headers())
}

// Преобразует допустимый Feed в индекс массива из двух источников; вызывающий предварительно проверяет
// значение.
func index(f Feed) int { return int(f) - 1 }

// Push принимает только локальную идентичность источника и его полную пару конфигураций.
// Внешние очереди продолжают вызывать Push для обоих источников при любом выборе.
func (s *Switcher) Push(feed Feed, attempt uint64, p media.Packet, video, audio media.CodecConfig, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recordStateLocked(s.status, s.failureRevision)
	if s.closed {
		return media.ErrRouterClosed
	}
	if feed != Live && feed != Fallback || attempt == 0 || now.IsZero() {
		return core.ErrInvalid
	}
	now = s.monotonic(now)
	// f указывает на состояние источника, которому принадлежит принимаемый пакет.
	f := &s.feeds[index(feed)]
	if attempt < f.attempt {
		return ErrFeed
	}
	if video.Validate() != nil || audio.Validate() != nil || video.Info().Track != media.TrackVideo || audio.Info().Track != media.TrackAudio || video.Info().SourceEpoch != audio.Info().SourceEpoch {
		return s.reject(feed, ErrFeed)
	}
	if attempt == f.attempt && f.attempt != 0 {
		if video.Info().SourceEpoch < f.video.Info().SourceEpoch {
			return ErrFeed
		}
		if video.Info().SourceEpoch == f.video.Info().SourceEpoch && (video.Info().Revision < f.video.Info().Revision || audio.Info().Revision < f.audio.Info().Revision) {
			return ErrFeed
		}
		if video.Info().SourceEpoch == f.video.Info().SourceEpoch && (video.Info().Revision == f.video.Info().Revision && !sameConfig(video, f.video) || audio.Info().Revision == f.audio.Info().Revision && !sameConfig(audio, f.audio)) {
			return s.reject(feed, ErrFeed)
		}
	}
	if attempt != f.attempt || !sameConfig(video, f.video) || !sameConfig(audio, f.audio) {
		*f = feedState{attempt: attempt, video: video, audio: audio}
		if s.selected == feed {
			s.selected = 0
		}
	}
	if f.rejected {
		return ErrFeed
	}
	if video.StoredBytes() > min(s.config.Queue.MaxConfigBytes, s.config.Router.MaxConfigBytes) || audio.StoredBytes() > min(s.config.Queue.MaxConfigBytes, s.config.Router.MaxConfigBytes) || p.StoredBytes() > s.config.Queue.MaxPacketBytes {
		return s.reject(feed, media.ErrRouterBudget)
	}
	// expected выбирает текущую видео- либо аудиоконфигурацию для проверки принадлежности пакета.
	expected := video
	if p.Config().Info().Track == media.TrackAudio {
		expected = audio
	}
	if !sameConfig(p.Config(), expected) {
		return s.reject(feed, ErrFeed)
	}
	// err сообщает о несовместимости фактического формата источника с профилем группы.
	if err := codec.Compatible(core.MediaFormat{Video: video.Info().Video, Audio: audio.Info().Audio}, s.config.Profile.Format); err != nil {
		return s.reject(feed, err)
	}
	// key содержит подтверждённый признак независимого видеостарта; err сохраняет ошибки инспекции и
	// последующего размещения пакета.
	key, err := f.inspector.Inspect(p)
	if err != nil {
		return s.reject(feed, err)
	}
	// pi содержит исходные временные метки и длительность принимаемого пакета.
	pi := p.Info()
	// track выбирает ячейку видео либо аудио для проверки DTS и контроля продвижения.
	track := 0
	if p.Config().Info().Track == media.TrackAudio {
		track = 1
	}
	if f.seen[track] && pi.DTS <= f.dts[track] {
		if pi.DTS < f.dts[track] {
			return s.reject(feed, media.ErrDTSRegression)
		}
		return ErrFeed
	}
	if f.seen[track] && now.Sub(f.arrival[track]) >= s.timeout(track) {
		f.stable = time.Time{}
		f.ready = false
	}
	// q связывает принимаемый пакет с временем получения и подтверждённым ключевым признаком.
	q := queued{packet: p, at: now, key: key}
	// previous содержит прежний пакет той же дорожки, чья неизвестная длительность определяется
	// новым DTS.
	if previous := f.pending[track]; previous != nil {
		f.pending[track] = nil
		// duration содержит разницу соседних DTS в шкале дорожки; err сообщает о переполнении
		// или отказе пересоздания прежнего пакета.
		duration, err := difference(pi.DTS, previous.packet.Info().DTS)
		if err != nil || duration <= 0 {
			return s.reject(feed, ErrTiming)
		}
		// info содержит метаданные прежнего пакета для назначения вычисленной длительности.
		info := previous.packet.Info()
		info.DurationTicks = duration
		previous.packet, err = media.NewPacket(info, previous.packet.Config(), previous.packet.Payload())
		if err != nil {
			return s.reject(feed, err)
		}
		if err = s.enqueue(feed, *previous); err != nil {
			return err
		}
	}
	f.seen[track] = true
	f.dts[track] = pi.DTS
	f.arrival[track] = now
	if f.seen[0] && f.seen[1] && f.stable.IsZero() {
		f.stable = now
	}
	if pi.DurationTicks == 0 {
		if !s.fits(f, q) {
			return s.reject(feed, media.ErrRouterBudget)
		}
		f.pending[track] = &q
	} else if err = s.enqueue(feed, q); err != nil {
		return err
	}
	return s.evaluate(now)
}

// Не допускает обратного хода времени проверок, возвращая последнее наблюдавшееся время для
// запоздалого вызова.
func (s *Switcher) monotonic(now time.Time) time.Time {
	if now.Before(s.lastNow) {
		return s.lastNow
	}
	s.lastNow = now
	return now
}

// Выбирает срок отсутствия прогресса по индексу видео либо аудио.
func (s *Switcher) timeout(track int) time.Duration {
	if track == 0 {
		return s.config.Watchdog.VideoTimeout
	}
	return s.config.Watchdog.AudioTimeout
}

// Добавляет полный пакет в ограниченную очередь; новый ключевой кадр невыбранного источника заменяет
// прежнее начало. Переполнение сбрасывает очередь и готовность; текущий пакет сохраняется только при
// независимом старте и соблюдении бюджета после сброса.
func (s *Switcher) enqueue(feed Feed, q queued) error {
	// f указывает на очередь источника, принимающую пакет с известной длительностью.
	f := &s.feeds[index(feed)]
	// weight содержит логический вес добавляемого пакета для проверки и учёта бюджета.
	weight := q.packet.StoredBytes()
	if weight > s.config.Queue.MaxPacketBytes {
		return s.reject(feed, media.ErrRouterBudget)
	}
	if q.key && s.selected != feed {
		f.ready = false
		f.queue = nil
		f.bytes = 0
	}
	if !s.fits(f, q) {
		f.queue = nil
		f.bytes = 0
		f.pending = [2]*queued{}
		f.stable = time.Time{}
		f.ready = false
		if s.selected == feed {
			s.selected = 0
		}
		if !q.key {
			return nil
		}
		if !s.fits(f, q) {
			return s.reject(feed, media.ErrRouterBudget)
		}
	}
	f.queue = append(f.queue, q)
	f.bytes += weight
	return nil
}

// Проверяет вместимость нового пакета вместе с очередью, отложенными пакетами и обеими конфигурациями
// без переполнения счётчиков.
func (s *Switcher) fits(f *feedState, q queued) bool {
	// total накапливает логический вес очереди, конфигураций и отложенных пакетов; count накапливает
	// число пакетов перед проверкой нового.
	total, count := f.bytes, uint64(len(f.queue))
	// c содержит очередную текущую конфигурацию источника при проверке общего бюджета.
	for _, c := range []media.CodecConfig{f.video, f.audio} {
		// n содержит логический вес проверяемой конфигурации в байтах.
		n := c.StoredBytes()
		if n > s.config.Queue.MaxBytes-total {
			return false
		}
		total += n
	}
	// p содержит очередной отложенный пакет без известной длительности, если он имеется.
	for _, p := range f.pending {
		if p != nil {
			// n содержит логический вес отложенного пакета в байтах.
			n := p.packet.StoredBytes()
			if n > s.config.Queue.MaxBytes-total {
				return false
			}
			total += n
			count++
		}
	}
	return count < uint64(s.config.Queue.MaxPackets) && q.packet.StoredBytes() <= s.config.Queue.MaxBytes-total
}

// Отзывает пригодность источника, очищает его буферы и сохраняет причину; отказ выбранного источника
// снимает выбор, а отсутствие выбора делает выход недоступным.
func (s *Switcher) reject(feed Feed, err error) error {
	if s.failureRevision < math.MaxUint64 {
		s.failureRevision++
	}
	// f указывает на состояние отзываемого источника для очистки буферов и сохранения причины.
	f := &s.feeds[index(feed)]
	f.rejected = true
	f.ready = false
	f.failure = err
	f.queue = nil
	f.pending = [2]*queued{}
	f.bytes = 0
	f.stable = time.Time{}
	if s.selected == feed {
		s.selected = 0
	}
	if s.selected == 0 {
		s.status = Status{Paused: s.paused, Err: errors.Join(ErrUnavailable, err), Epoch: s.status.Epoch, Revision: s.revision}
	}
	return err
}

// Проверяет продвижение обеих дорожек в пределах их сроков; устаревшая дорожка сбрасывает окно
// стабильности и готовность.
func (s *Switcher) healthy(f *feedState, now time.Time) bool {
	if f.rejected || !f.seen[0] || !f.seen[1] {
		return false
	}
	// i обозначает видео либо аудио при проверке срока последнего продвижения.
	for i := range 2 {
		if now.Before(f.arrival[i]) || now.Sub(f.arrival[i]) >= s.timeout(i) {
			f.stable = time.Time{}
			f.ready = false
			return false
		}
	}
	return true
}

// BeginAttempt отзывает прежнюю готовность до запуска новой внешней попытки.
func (s *Switcher) BeginAttempt(feed Feed, attempt uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recordStateLocked(s.status, s.failureRevision)
	if s.closed {
		return media.ErrRouterClosed
	}
	if (feed != Live && feed != Fallback) || attempt == 0 {
		return core.ErrInvalid
	}
	// f указывает на состояние источника, чья новая попытка отзывает прежнюю готовность.
	f := &s.feeds[index(feed)]
	if attempt <= f.attempt {
		return ErrFeed
	}
	*f = feedState{attempt: attempt}
	if s.selected == feed {
		s.selected = 0
		s.status.Available = false
		s.status.Err = ErrUnavailable
	}
	return nil
}

// FallbackReady проверяет текущую попытку независимо от выбранного источника.
// Проверка использует те же A/V и временные условия, что и переключение.
func (s *Switcher) FallbackReady(attempt uint64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recordStateLocked(s.status, s.failureRevision)
	if s.closed {
		return media.ErrRouterClosed
	}
	// f указывает на текущую fallback-попытку, проверяемую для барьера готовности.
	f := &s.feeds[index(Fallback)]
	if attempt == 0 || f.attempt != attempt {
		return core.ErrNotReady
	}
	if f.rejected {
		return errors.Join(ErrUnavailable, f.failure)
	}
	if !s.healthy(f, s.monotonic(now)) {
		return core.ErrNotReady
	}
	if f.ready && s.selected == Fallback {
		return nil
	}
	// err содержит результат проверки пригодной A/V-точки перехода текущей заглушки.
	if _, _, _, err := s.transition(Fallback, now); err != nil {
		return err
	}
	f.ready = true
	return nil
}

// Tick повторно оценивает доступность и выбор источника на неубывающем времени, включая истечение
// сроков без новых пакетов.
func (s *Switcher) Tick(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recordStateLocked(s.status, s.failureRevision)
	if s.closed {
		return media.ErrRouterClosed
	}
	return s.evaluate(s.monotonic(now))
}

// SetPaused сохраняет желание ручной паузы и немедленно переоценивает выбор заглушки; при её
// отсутствии желание сохраняется вместе с ошибкой доступности.
func (s *Switcher) SetPaused(paused bool, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recordStateLocked(s.status, s.failureRevision)
	if s.closed {
		return media.ErrRouterClosed
	}
	s.paused = paused
	return s.evaluate(s.monotonic(now))
}

// Fail отзывает указанную неустаревшую попытку и переоценивает альтернативный источник; nil означает
// завершение потока ErrFeedEnded.
func (s *Switcher) Fail(feed Feed, attempt uint64, err error, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recordStateLocked(s.status, s.failureRevision)
	if s.closed {
		return media.ErrRouterClosed
	}
	if feed != Live && feed != Fallback || attempt == 0 {
		return core.ErrInvalid
	}
	if err == nil {
		err = ErrFeedEnded
	}
	// f указывает на источник, к которому относится сообщение о завершении попытки.
	f := &s.feeds[index(feed)]
	if attempt < f.attempt {
		return ErrFeed
	}
	if attempt > f.attempt {
		*f = feedState{attempt: attempt}
	}
	_ = s.reject(feed, err)
	return s.evaluate(s.monotonic(now))
}

// Выбирает источник по паузе, здоровью и окну восстановления; при невозможном переходе сохраняет ещё
// пригодный прежний источник, затем выпускает очередь выбранного.
func (s *Switcher) evaluate(now time.Time) error {
	// live показывает текущее здоровье LIVE; fallback показывает текущее здоровье заглушки.
	live, fallback := s.healthy(&s.feeds[0], now), s.healthy(&s.feeds[1], now)
	// desired содержит предполагаемый источник после учёта паузы, здоровья и окна восстановления.
	desired := s.selected
	if s.paused || !live {
		desired = Fallback
	} else if s.selected == Live {
		desired = Live
	} else {
		// f указывает на LIVE для проверки длительности его стабильного восстановления.
		f := &s.feeds[0]
		if !f.stable.IsZero() && now.Sub(f.stable) >= s.config.Watchdog.RecoveryWindow {
			desired = Live
		}
	}
	if desired == Fallback && !fallback {
		if live && !s.paused {
			desired = Live
		} else {
			desired = 0
		}
	}
	if desired == 0 {
		s.selected = 0
		s.status = Status{Paused: s.paused, Err: errors.Join(ErrUnavailable, s.feeds[0].failure, s.feeds[1].failure), Epoch: s.status.Epoch, Revision: s.revision}
		return ErrUnavailable
	}
	if s.selected != desired {
		// err содержит отказ начала перехода, при котором ещё пригодный прежний источник может
		// продолжить работу.
		if err := s.begin(desired, now); err != nil {
			if s.selected != 0 && s.healthy(&s.feeds[index(s.selected)], now) && (!s.paused || s.selected == Fallback) {
				return s.drain(s.selected)
			}
			s.status.Available = false
			s.status.Err = errors.Join(ErrUnavailable, err)
			return s.status.Err
		}
	}
	return s.drain(desired)
}

// Ищет независимое видео и аудио не раньше его PTS, проверяет окно восстановления, A/V-расхождение и
// представимость времени и вычисляет общий сдвиг к выходному горизонту до смены источника.
func (s *Switcher) transition(feed Feed, now time.Time) (int, int, int64, error) {
	// f указывает на буфер источника, для которого вычисляется точка перехода.
	f := &s.feeds[index(feed)]
	// vi хранит позицию выбранного независимого видео; ai хранит позицию первого подходящего AAC; -1
	// означает отсутствие точки старта.
	vi, ai := -1, -1
	// i обозначает позицию кандидата в очереди; q содержит проверяемый пакет при поиске независимого
	// видео.
	for i, q := range f.queue {
		if q.packet.Config().Info().Track != media.TrackVideo || !q.key {
			continue
		}
		if feed == Live && s.selected != Live && !s.paused {
			// ready задаёт момент завершения окна стабильного восстановления LIVE.
			ready := f.stable.Add(s.config.Watchdog.RecoveryWindow)
			if q.at.Before(ready) {
				continue
			}
			if q.at.After(ready.Add(s.config.Watchdog.KeyframeTimeout)) {
				f.stable = now
				return 0, 0, 0, ErrFeed
			}
		}
		vi = i
		break
	}
	if vi < 0 {
		return 0, 0, 0, ErrFeed
	}
	// vp содержит выбранный независимый видеопакет начала перехода.
	vp := f.queue[vi].packet
	// vpts содержит PTS стартового видео в общей выходной шкале; err сохраняет ошибки пересчёта и
	// вычисления сдвига.
	vpts, err := rescale(vp.Info().PTS, vp.Config().Info().TimeBase, s.base)
	if err != nil {
		return 0, 0, 0, s.reject(feed, err)
	}
	// i обозначает позицию аудиокандидата; q содержит проверяемый пакет при поиске AAC не раньше PTS
	// видеостарта.
	for i, q := range f.queue {
		if q.packet.Config().Info().Track != media.TrackAudio {
			continue
		}
		// a содержит PTS аудиокандидата в общей шкале; err сообщает об отказе точного пересчёта.
		a, err := rescale(q.packet.Info().PTS, q.packet.Config().Info().TimeBase, s.base)
		if err != nil {
			return 0, 0, 0, s.reject(feed, err)
		}
		if a >= vpts {
			// skew содержит разницу аудио- и видеостарта в широкой целой арифметике для
			// проверки MaxAVSkew.
			skew := new(big.Int).Sub(big.NewInt(a), big.NewInt(vpts))
			if skew.Cmp(s.duration(s.config.MaxAVSkew)) > 0 {
				return 0, 0, 0, s.reject(feed, ErrTiming)
			}
			ai = i
			break
		}
	}
	if ai < 0 {
		return 0, 0, 0, ErrFeed
	}
	// earliest накапливает минимальную из PTS и DTS стартовых пакетов обеих дорожек.
	earliest := int64(math.MaxInt64)
	// i содержит позицию выбранного стартового видео либо аудио в очереди.
	for _, i := range []int{vi, ai} {
		// p содержит выбранный стартовый пакет для поиска самой ранней временной метки.
		p := f.queue[i].packet
		// t содержит очередную PTS либо DTS стартового пакета в его исходной шкале.
		for _, t := range []int64{p.Info().PTS, p.Info().DTS} {
			// v содержит стартовую метку в общей шкале; e сообщает об отказе точного
			// пересчёта.
			v, e := rescale(t, p.Config().Info().TimeBase, s.base)
			if e != nil {
				return 0, 0, 0, s.reject(feed, e)
			}
			earliest = min(earliest, v)
		}
	}
	// target задаёт выходной горизонт прежних пакетов либо ноль до первой публикации.
	target := int64(0)
	if s.hasHorizon {
		target = s.horizon
	}
	// firstPresentation содержит наиболее раннюю PTS из выбранных стартовых видео и аудио.
	firstPresentation := vpts
	// ap содержит выбранный стартовый AAC-пакет.
	ap := f.queue[ai].packet
	// apts содержит его PTS в общей шкале; e сообщает об отказе точного пересчёта.
	apts, e := rescale(ap.Info().PTS, ap.Config().Info().TimeBase, s.base)
	if e != nil {
		return 0, 0, 0, s.reject(feed, e)
	}
	firstPresentation = min(firstPresentation, apts)
	// gap содержит разрыв между ранней начальной PTS/DTS и первым представлением для проверки
	// MaxTransitionGap.
	gap := new(big.Int).Sub(big.NewInt(firstPresentation), big.NewInt(earliest))
	if gap.Cmp(s.duration(s.config.MaxTransitionGap)) > 0 {
		return 0, 0, 0, s.reject(feed, ErrTiming)
	}
	// offset содержит единый сдвиг нового источника к текущему выходному горизонту.
	offset, err := difference(target, earliest)
	if err != nil {
		return 0, 0, 0, s.reject(feed, err)
	}
	// i обозначает позицию удерживаемого пакета; q содержит пакет для предварительной проверки всех
	// меток после выбранного старта.
	for i, q := range f.queue {
		// track задаёт дорожку проверяемого пакета для исключения данных до её точки старта.
		track := q.packet.Config().Info().Track
		if track == media.TrackVideo && i < vi || track == media.TrackAudio && i < ai {
			continue
		}
		// err содержит отказ предварительного преобразования временных меток пакета выбранного
		// перехода.
		if _, _, err := s.mapPacketInfo(q.packet, offset); err != nil {
			return 0, 0, 0, s.reject(feed, err)
		}
	}
	return vi, ai, offset, nil
}

// После проверки перехода создаёт новую эпоху выхода и пару конфигураций общей шкалы, отбрасывая
// пакеты до найденных точек старта каждой дорожки.
func (s *Switcher) begin(feed Feed, now time.Time) error {
	// vi задаёт стартовую позицию видео; ai задаёт стартовую позицию аудио; offset задаёт общий
	// выходной сдвиг; err сообщает об отказе проверки перехода.
	vi, ai, offset, err := s.transition(feed, now)
	if err != nil {
		return err
	}
	// f указывает на состояние источника, выбранного после успешной проверки перехода.
	f := &s.feeds[index(feed)]
	if s.revision == math.MaxUint64 {
		return ErrTiming
	}
	// revision содержит следующий номер пары выходных конфигураций.
	revision := s.revision + 1
	// source содержит новую эпоху выходного Router для выбранного источника.
	source, err := s.router.BeginSource()
	if err != nil {
		return err
	}
	// configs собирает новую пару выходных конфигураций с общей шкалой и ревизией.
	configs := [2]media.CodecConfig{}
	// i обозначает позицию видео либо аудио; c содержит соответствующую входную конфигурацию.
	for i, c := range []media.CodecConfig{f.video, f.audio} {
		// info содержит копию описания конфигурации для замены эпохи, ревизии и временной базы.
		info := c.Info()
		info.SourceEpoch = source.Epoch()
		info.Revision = revision
		info.TimeBase = s.base
		configs[i], err = media.NewCodecConfig(info, c.Headers())
		if err != nil {
			return err
		}
	}
	if err = source.UpdateConfigs(configs[0], configs[1]); err != nil {
		return err
	}
	f.ready = true
	s.source = source
	s.video, s.audio = configs[0], configs[1]
	s.revision = revision
	s.offset = offset
	s.selected = feed
	// keep собирает пакеты не раньше выбранных точек старта каждой дорожки.
	keep := make([]queued, 0, len(f.queue))
	// i обозначает позицию исходного пакета; q содержит пакет для отбора в новую выходную очередь.
	for i, q := range f.queue {
		// track задаёт дорожку пакета при сравнении с её стартовой позицией.
		track := q.packet.Config().Info().Track
		if track == media.TrackVideo && i >= vi || track == media.TrackAudio && i >= ai {
			keep = append(keep, q)
		}
	}
	f.queue = keep
	f.bytes = 0
	// q содержит очередной сохранённый пакет при пересчёте логического веса новой очереди.
	for _, q := range keep {
		f.bytes += q.packet.StoredBytes()
	}
	s.status = Status{Source: feed, Paused: s.paused, Available: true, Epoch: source.Epoch(), Revision: revision}
	return nil
}

// Переводит положительный временной предел в целое число единиц выходной шкалы с округлением вниз.
func (s *Switcher) duration(d time.Duration) *big.Int {
	// n содержит числитель перевода временного предела в единицы выходной шкалы без переполнения
	// промежуточного результата.
	n := new(big.Int).Mul(big.NewInt(int64(d)), new(big.Int).SetUint64(uint64(s.base.Denominator)))
	return n.Quo(n, big.NewInt(int64(time.Second)*int64(s.base.Numerator)))
}

// Публикует очередь выбранного источника с проверенными метками и ключевыми признаками, учитывая
// только успешные пакеты и общий горизонт PTS/DTS.
func (s *Switcher) drain(feed Feed) error {
	// f указывает на очередь выбранного источника для последовательной публикации.
	f := &s.feeds[index(feed)]
	for len(f.queue) > 0 {
		// q содержит первый пакет, извлечённый из очереди с освобождением её логического веса.
		q := f.queue[0]
		f.queue[0] = queued{}
		f.queue = f.queue[1:]
		f.bytes -= q.packet.StoredBytes()
		// p содержит исходную единицу доступа для преобразования меток и выходной конфигурации.
		p := q.packet
		// info содержит преобразованные метаданные; end содержит максимальный конец PTS/DTS
		// пакета; err сохраняет ошибки преобразования, создания и публикации выхода.
		info, end, err := s.mapPacketInfo(p, s.offset)
		if err != nil {
			return s.reject(feed, err)
		}
		// c выбирает выходную конфигурацию видео либо аудио текущей эпохи.
		c := s.video
		info.Keyframe = q.key
		if p.Config().Info().Track == media.TrackAudio {
			c = s.audio
			info.Keyframe = false
		}
		// out содержит пересозданный пакет с общей шкалой и подтверждённым ключевым признаком.
		out, err := media.NewPacket(info, c, p.Payload())
		if err != nil {
			return s.reject(feed, err)
		}
		if err = s.source.Publish(out); err != nil {
			return s.reject(feed, err)
		}
		s.outputCounters.AddPacket(out, time.Now())
		if !s.hasHorizon {
			s.horizon = end
			s.hasHorizon = true
		} else {
			s.horizon = max(s.horizon, end)
		}
	}
	s.status = Status{Source: feed, Paused: s.paused, Available: true, Epoch: s.source.Epoch(), Revision: s.revision}
	return nil
}

// Точно пересчитывает PTS, DTS и положительную длительность пакета, применяет общий сдвиг и возвращает
// максимальный конец представления или декодирования.
func (s *Switcher) mapPacketInfo(p media.Packet, offset int64) (media.PacketInfo, int64, error) {
	// info содержит изменяемую копию временных меток пакета; base задаёт их исходную временную
	// шкалу.
	info, base := p.Info(), p.Config().Info().TimeBase
	// err переносит ошибки точного пересчёта и сложения выходных меток пакета.
	var err error
	info.PTS, err = mapTime(info.PTS, base, s.base, offset)
	if err != nil {
		return info, 0, err
	}
	info.DTS, err = mapTime(info.DTS, base, s.base, offset)
	if err != nil {
		return info, 0, err
	}
	info.DurationTicks, err = rescale(info.DurationTicks, base, s.base)
	if err != nil || info.DurationTicks <= 0 {
		return info, 0, ErrTiming
	}
	// endPTS содержит конец представления пакета в общей выходной шкале.
	endPTS, err := sum(info.PTS, info.DurationTicks)
	if err != nil {
		return info, 0, err
	}
	// endDTS содержит конец декодирования пакета в общей выходной шкале.
	endDTS, err := sum(info.DTS, info.DurationTicks)
	if err != nil {
		return info, 0, err
	}
	return info, max(endPTS, endDTS), nil
}

// Close отзывает доступность, освобождает состояния обоих источников и закрывает выходной Router;
// последующие операции переключения отвергаются.
func (s *Switcher) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recordStateLocked(s.status, s.failureRevision)
	if !s.closed && s.failureRevision < math.MaxUint64 {
		s.failureRevision++
	}
	s.closed = true
	s.status = Status{Paused: s.paused, Err: media.ErrRouterClosed, Epoch: s.status.Epoch, Revision: s.revision}
	// i обозначает состояние источника, освобождаемое при закрытии переключателя.
	for i := range s.feeds {
		s.feeds[i] = feedState{}
	}
	return s.router.Close()
}

// Пересчитывает знаковую метку между временными базами только без остатка и в пределах int64;
// округление не допускается.
func rescale(v int64, from, to media.TimeBase) (int64, error) {
	if from.Validate() != nil || to.Validate() != nil {
		return 0, ErrTiming
	}
	// n содержит широкий числитель точного пересчёта знаковой метки между временными базами.
	n := new(big.Int).Mul(big.NewInt(v), new(big.Int).SetUint64(uint64(from.Numerator)*uint64(to.Denominator)))
	// d содержит широкий знаменатель пересчёта временной метки.
	d := new(big.Int).SetUint64(uint64(from.Denominator) * uint64(to.Numerator))
	// q содержит целую часть пересчёта; r содержит остаток, который должен быть нулевым для точного
	// результата.
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(n, d, r)
	if r.Sign() != 0 || !q.IsInt64() {
		return 0, ErrTiming
	}
	return q.Int64(), nil
}

// Складывает две знаковые метки через проверяемый широкий результат, возвращая ErrTiming при
// переполнении int64.
func sum(a, b int64) (int64, error) {
	// n содержит широкую сумму меток для проверки диапазона int64.
	n := new(big.Int).Add(big.NewInt(a), big.NewInt(b))
	if !n.IsInt64() {
		return 0, ErrTiming
	}
	return n.Int64(), nil
}

// Вычисляет разность знаковых меток без переполнения int64 либо возвращает ErrTiming.
func difference(a, b int64) (int64, error) {
	// n содержит широкую разность меток для проверки диапазона int64.
	n := new(big.Int).Sub(big.NewInt(a), big.NewInt(b))
	if !n.IsInt64() {
		return 0, ErrTiming
	}
	return n.Int64(), nil
}

// Точно пересчитывает метку и прибавляет проверяемый общий сдвиг выходной шкалы.
func mapTime(v int64, from, to media.TimeBase, offset int64) (int64, error) {
	// n содержит точно пересчитанную метку; e сообщает об отказе пересчёта до прибавления общего
	// сдвига.
	n, e := rescale(v, from, to)
	if e != nil {
		return 0, e
	}
	return sum(n, offset)
}
