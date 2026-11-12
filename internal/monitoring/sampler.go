package monitoring

import (
	"context"
	"errors"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// ErrSamplerPending сообщает, что ожидание истекло до подтверждённого завершения того же работника.
var ErrSamplerPending = errors.New("сборщик метрик ещё не завершён")

// SamplerConfig задаёт предел зарегистрированных источников, свежесть измерений и срок ожидания
// остановки.
type SamplerConfig struct {
	// MaxSources ограничивает число зарегистрированных функций чтения в одном сборщике.
	MaxSources int
	// StaleAfter задаёт срок свежести измерений потребителям конфигурации; ShutdownTimeout
	// ограничивает ожидание завершения работника.
	StaleAfter, ShutdownTimeout time.Duration
}

// Validate требует положительные пределы числа источников, свежести и ожидания завершения.
func (c SamplerConfig) Validate() error {
	if c.MaxSources <= 0 || c.StaleAfter <= 0 || c.ShutdownTimeout <= 0 {
		return core.ErrInvalid
	}
	return nil
}

// SampleFunc читает метрики источника; параметр publish разрешает их публикацию при наступлении
// следующего срока. Целевой период публикации — две секунды после завершения предыдущего чтения,
// назначившего этот срок; задержка текущего или соседнего источника может увеличить интервал.
// Пропущенные периоды не вызывают дополнительных чтений.
// Функция копирует данные производителя и отпускает его блокировки до вызова core.
// Отмена должна завершать ожидания; сборщик не создаёт отдельные горутины для функций чтения.
type SampleFunc func(context.Context, time.Time, bool) error

// sampleSource хранит функцию и собственные сроки чтения и публикации зарегистрированного источника.
type sampleSource struct {
	// id содержит уникальный ненулевой идентификатор регистрации для проверки её актуальности.
	id uint64
	// sample последовательно читает данные производителя и публикует их только при разрешённом
	// проходе.
	sample SampleFunc
	// started отмечает выполненное первое чтение и включает дальнейшие сроки этого источника.
	started bool
	// nextSample задаёт ближайшее чтение источника; nextPublish задаёт ближайший проход с
	// разрешённой публикацией.
	nextSample, nextPublish time.Time
}

// SamplerStats содержит согласованный снимок числа регистраций и насыщаемого счётчика ошибок чтения.
type SamplerStats struct {
	// Sources содержит текущее число зарегистрированных источников.
	Sources int
	// Failures содержит число отказов чтения, возникших без отмены рабочего контекста.
	Failures uint64
	// Saturated отмечает отказ, который уже нельзя прибавить к максимальному Failures.
	Saturated bool
}

// SessionSampler обслуживает ограниченный набор источников одним последовательным отменяемым работником.
type SessionSampler struct {
	// mu защищает регистрации, допуск и общие счётчики отказов.
	mu sync.Mutex
	// config сохраняет проверенные бюджеты и срок ожидания остановки сборщика.
	config SamplerConfig
	// sources связывает уникальные идентификаторы с текущими регистрациями функций чтения.
	sources map[uint64]*sampleSource
	// cancel отменяет единственного работника при Close или отмене родительского контекста.
	cancel context.CancelFunc
	// done закрывается после выхода работника и очистки ссылок на зарегистрированные источники.
	done chan struct{}
	// wake будит работника после регистрации; буфер объединяет несколько ожидающих уведомлений.
	wake chan struct{}
	// stopping закрывает допуск новых регистраций при остановке или выходе работника.
	stopping bool
	// failures насыщаемо учитывает ошибки функций чтения, не вызванные отменой сборщика.
	failures uint64
	// saturated запоминает попытку увеличить уже максимальный счётчик failures.
	saturated bool
}

// NewSessionSampler создаёт одного последовательного работника под родительским контекстом; отменённый
// или отсутствующий контекст отклоняется до запуска.
func NewSessionSampler(parent context.Context, cfg SamplerConfig) (*SessionSampler, error) {
	if parent == nil {
		return nil, core.ErrInvalid
	}
	// err содержит отказ проверки числа источников и сроков до запуска работника.
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// err содержит причину уже отменённого родительского контекста, запрещающую создание сборщика.
	if err := parent.Err(); err != nil {
		return nil, err
	}
	// ctx управляет сроком жизни единственного работника; cancel запрашивает его остановку.
	ctx, cancel := context.WithCancel(parent)
	// s содержит создаваемый сборщик с собственными регистрациями и сигналами пробуждения и
	// завершения.
	s := &SessionSampler{config: cfg, sources: map[uint64]*sampleSource{}, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	go s.run(ctx)
	return s, nil
}

// Register добавляет функцию чтения под уникальным ненулевым ID в пределах MaxSources и будит сборщик.
// Закрытый допуск, повторный ID и переполнение возвращают отдельные ошибки.
func (s *SessionSampler) Register(id uint64, sample SampleFunc) error {
	if s == nil || id == 0 || sample == nil {
		return core.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return core.ErrClosed
	}
	// ok показывает наличие регистрации с таким ID для отклонения повторного источника.
	if _, ok := s.sources[id]; ok {
		return core.ErrConflict
	}
	if len(s.sources) >= s.config.MaxSources {
		return core.ErrObservationLimit
	}
	s.sources[id] = &sampleSource{id: id, sample: sample}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

// Remove удаляет регистрацию без ожидания уже начатого чтения. Снимок источников повторно проверяется
// перед следующим вызовом.
func (s *SessionSampler) Remove(id uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.sources, id)
	s.mu.Unlock()
}

// Stats возвращает согласованное число зарегистрированных источников и насыщаемый счётчик ошибок
// чтения.
func (s *SessionSampler) Stats() SamplerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SamplerStats{Sources: len(s.sources), Failures: s.failures, Saturated: s.saturated}
}

// Done закрывается после выхода единственного работника и очистки удерживаемых источников.
func (s *SessionSampler) Done() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.done
}

// Close закрывает допуск, отменяет работника и ограниченно ждёт завершения. ErrSamplerPending
// сохраняет того же работника для повторного ожидания.
func (s *SessionSampler) Close(ctx context.Context) error {
	if s == nil || ctx == nil {
		return core.ErrInvalid
	}
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	s.cancel()
	// timer ограничивает текущую попытку ожидания остановки, сохраняя работника для следующего
	// Close.
	timer := time.NewTimer(s.config.ShutdownTimeout)
	defer timer.Stop()
	select {
	case <-s.done:
		return nil
	default:
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return errors.Join(ErrSamplerPending, ctx.Err())
	case <-timer.C:
		return ErrSamplerPending
	}
}

// run последовательно читает готовые источники и назначает каждому следующий срок после фактического
// завершения чтения. Пропущенные периоды не переигрываются; новая регистрация не переносит сроки
// соседних источников.
func (s *SessionSampler) run(ctx context.Context) {
	defer close(s.done)
	defer func() {
		// Закрывает допуск и удаляет ссылки на источники под блокировкой перед подтверждением Done.
		s.mu.Lock()
		s.stopping = true
		clear(s.sources)
		s.mu.Unlock()
	}()
	// timer будит цикл в ближайший срок готового источника и останавливается при завершении
	// работника.
	timer := time.NewTimer(time.Second)
	timer.Stop()
	defer timer.Stop()
	// timerC подключает канал таймера к select только при наличии запланированного чтения.
	var timerC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-timerC:
		case <-s.wake:
		}
		// now содержит момент отбора готовых источников и затем обновляется при расчёте
		// ближайшего срока.
		now := time.Now()
		s.mu.Lock()
		// sources сохраняет снимок готовых регистраций для последовательного чтения вне
		// блокировки карты.
		sources := make([]*sampleSource, 0, len(s.sources))
		// source содержит очередную регистрацию при отборе источников, чей срок чтения уже
		// наступил.
		for _, source := range s.sources {
			if !source.started || !now.Before(source.nextSample) {
				sources = append(sources, source)
			}
		}
		s.mu.Unlock()
		sort.Slice(sources, func(i, j int) bool {
			// Упорядочивает готовые источники по идентификатору перед последовательным вызовом функций чтения.
			return sources[i].id < sources[j].id
		})
		// source содержит очередной готовый источник из упорядоченного снимка регистраций.
		for _, source := range sources {
			if ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			// current содержит регистрацию, остающуюся под этим ID, для отклонения
			// удалённого или заменённого источника.
			current := s.sources[source.id]
			s.mu.Unlock()
			if current != source {
				continue
			}
			// publish разрешает публикацию только после начального чтения и наступления
			// собственного срока источника.
			publish := source.started && !time.Now().Before(source.nextPublish)
			// err содержит результат последовательного чтения текущего источника для учёта
			// отказа без отмены.
			err := source.sample(ctx, time.Now(), publish)
			// completed содержит фактическое время завершения чтения, от которого
			// назначаются следующие сроки.
			completed := time.Now()
			if !source.started || publish {
				source.nextPublish = completed.Add(2 * time.Second)
			}
			source.started = true
			source.nextSample = completed.Add(time.Second)
			if err != nil && ctx.Err() == nil {
				s.mu.Lock()
				if s.failures < math.MaxUint64 {
					s.failures++
				} else {
					s.saturated = true
				}
				s.mu.Unlock()
			}
		}
		clear(sources)
		// Следующее окно начинается после фактически завершённого чтения этого источника.
		// Новая регистрация не переносит сроки уже работающих источников.
		s.mu.Lock()
		// next накапливает ближайший срок чтения среди оставшихся регистраций.
		next := time.Time{}
		now = time.Now()
		// source содержит очередную регистрацию при поиске ближайшего пробуждения работника.
		for _, source := range s.sources {
			// at содержит срок чтения текущего источника либо now для ещё не начатого
			// источника.
			at := source.nextSample
			if !source.started {
				at = now
			}
			if next.IsZero() || at.Before(next) {
				next = at
			}
		}
		s.mu.Unlock()
		timer.Stop()
		timerC = nil
		if !next.IsZero() {
			// delay содержит неотрицательное ожидание до ближайшего срока чтения.
			delay := max(time.Until(next), 0)
			timer.Reset(delay)
			timerC = timer.C
		}
	}
}
