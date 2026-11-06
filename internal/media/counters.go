package media

import (
	"math"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// CounterSnapshot содержит накопительные измерения в границах одного владельца.
// SampledAt сохраняет монотонную составляющую времени для расчёта скоростей.
type CounterSnapshot struct {
	SampledAt                             time.Time
	PayloadBytes, VideoFrames, AudioUnits uint64
	LastFrame                             time.Time
	Video                                 core.VideoFormat
	Audio                                 core.AudioFormat
	Saturated                             bool
}

// Add учитывает подтверждённую единицу доступа; синхронизацией владеет вызывающий.
func (s *CounterSnapshot) Add(track Track, payloadBytes uint64, video core.VideoFormat, audio core.AudioFormat, now time.Time) {
	addCounter(&s.PayloadBytes, payloadBytes, &s.Saturated)
	if track == TrackVideo {
		addCounter(&s.VideoFrames, 1, &s.Saturated)
		s.LastFrame, s.Video = now, video
	} else {
		addCounter(&s.AudioUnits, 1, &s.Saturated)
		s.Audio = audio
	}
}

// AddPacket учитывает пакет без копирования его полезной нагрузки.
func (s *CounterSnapshot) AddPacket(p Packet, now time.Time) {
	s.Add(p.config.info.Track, uint64(len(p.payload)), p.config.info.Video, p.config.info.Audio, now)
}

func addCounter(dst *uint64, n uint64, saturated *bool) {
	if n > math.MaxUint64-*dst {
		*dst, *saturated = math.MaxUint64, true
		return
	}
	*dst += n
}

// IngressSnapshot привязывает счётчики к точному допуску публикации OBS.
type IngressSnapshot struct {
	PublicationID core.ID
	Connected     bool
	SourceEpoch   uint64
	Counters      CounterSnapshot
}

// BeginPublication сбрасывает измерения только при смене допуска, а не SourceEpoch.
// Вызывается из внутренней установки RuntimeLease до приёма сообщений.
func (r *Router) BeginPublication(id core.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ingress = IngressSnapshot{PublicationID: id, Connected: true}
}

// EndPublication отзывает только соответствующее соединение.
func (r *Router) EndPublication(id core.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ingress.PublicationID == id {
		r.ingress.Connected = false
	}
}

// ObserveIngress учитывает подтверждённую адаптером единицу до отсева готовности.
func (r *Router) ObserveIngress(id core.ID, track Track, bytes uint64, video core.VideoFormat, audio core.AudioFormat) {
	r.ObserveIngressAt(id, track, bytes, video, audio, time.Now())
}

// ObserveIngressAt сохраняет время получения подтверждённой единицы адаптером.
func (r *Router) ObserveIngressAt(id core.ID, track Track, bytes uint64, video core.VideoFormat, audio core.AudioFormat, receivedAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ingress.Connected && r.ingress.PublicationID == id {
		r.ingress.SourceEpoch = r.epoch
		r.ingress.Counters.Add(track, bytes, video, audio, receivedAt)
	}
}

// Metrics переводит подтверждённые поля без подстановки настроенного профиля.
func (s CounterSnapshot) Metrics(rates core.MediaRates) core.MediaMetrics {
	return core.MediaMetrics{SampledAt: s.SampledAt, VideoFrames: s.VideoFrames, AudioUnits: s.AudioUnits, PayloadBytes: s.PayloadBytes, Saturated: s.Saturated, LastFrame: s.LastFrame,
		Rates: rates, Format: core.ObservedFormat{VideoCodec: s.Video.Codec, AudioCodec: s.Audio.Codec, Width: s.Video.Width, Height: s.Video.Height, SampleRateHz: s.Audio.SampleRateHz, Channels: s.Audio.Channels}}
}

func (r *Router) IngressSnapshot() IngressSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.ingress
	s.Counters.SampledAt = time.Now()
	return s
}
