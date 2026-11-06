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

var ErrSamplerPending = errors.New("сборщик метрик ещё не завершён")

type SamplerConfig struct {
	MaxSources                  int
	StaleAfter, ShutdownTimeout time.Duration
}

func (c SamplerConfig) Validate() error {
	if c.MaxSources <= 0 || c.StaleAfter <= 0 || c.ShutdownTimeout <= 0 {
		return core.ErrInvalid
	}
	return nil
}

// SampleFunc получает разрешение публикации каждые две фактические секунды.
// Функция копирует данные производителя и отпускает его блокировки до вызова core.
// Отмена должна завершать ожидания; сборщик не создаёт отдельные горутины для функций чтения.
type SampleFunc func(context.Context, time.Time, bool) error

type sampleSource struct {
	id                      uint64
	sample                  SampleFunc
	started                 bool
	nextSample, nextPublish time.Time
}
type SamplerStats struct {
	Sources   int
	Failures  uint64
	Saturated bool
}
type SessionSampler struct {
	mu        sync.Mutex
	config    SamplerConfig
	sources   map[uint64]*sampleSource
	cancel    context.CancelFunc
	done      chan struct{}
	wake      chan struct{}
	stopping  bool
	failures  uint64
	saturated bool
}

func NewSessionSampler(parent context.Context, cfg SamplerConfig) (*SessionSampler, error) {
	if parent == nil {
		return nil, core.ErrInvalid
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	s := &SessionSampler{config: cfg, sources: map[uint64]*sampleSource{}, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	go s.run(ctx)
	return s, nil
}
func (s *SessionSampler) Register(id uint64, sample SampleFunc) error {
	if s == nil || id == 0 || sample == nil {
		return core.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return core.ErrClosed
	}
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
func (s *SessionSampler) Remove(id uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.sources, id)
	s.mu.Unlock()
}
func (s *SessionSampler) Stats() SamplerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SamplerStats{Sources: len(s.sources), Failures: s.failures, Saturated: s.saturated}
}
func (s *SessionSampler) Done() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.done
}
func (s *SessionSampler) Close(ctx context.Context) error {
	if s == nil || ctx == nil {
		return core.ErrInvalid
	}
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	s.cancel()
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
func (s *SessionSampler) run(ctx context.Context) {
	defer close(s.done)
	defer func() { s.mu.Lock(); s.stopping = true; clear(s.sources); s.mu.Unlock() }()
	timer := time.NewTimer(time.Second)
	timer.Stop()
	defer timer.Stop()
	var timerC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-timerC:
		case <-s.wake:
		}
		now := time.Now()
		s.mu.Lock()
		sources := make([]*sampleSource, 0, len(s.sources))
		for _, source := range s.sources {
			if !source.started || !now.Before(source.nextSample) {
				sources = append(sources, source)
			}
		}
		s.mu.Unlock()
		sort.Slice(sources, func(i, j int) bool { return sources[i].id < sources[j].id })
		for _, source := range sources {
			if ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			current := s.sources[source.id]
			s.mu.Unlock()
			if current != source {
				continue
			}
			publish := source.started && !time.Now().Before(source.nextPublish)
			err := source.sample(ctx, time.Now(), publish)
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
		next := time.Time{}
		now = time.Now()
		for _, source := range s.sources {
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
			delay := time.Until(next)
			if delay < 0 {
				delay = 0
			}
			timer.Reset(delay)
			timerC = timer.C
		}
	}
}
