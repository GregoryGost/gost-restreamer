package monitoring

import (
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// SourceKind различает направления счётчиков, чтобы их смена начинала новое окно расчёта скоростей.
type SourceKind string

const (
	// SourceInput обозначает счётчики текущей входящей публикации OBS.
	SourceInput SourceKind = "input"
	// SourceGroupLive обозначает счётчики входа LIVE отдельной группы.
	SourceGroupLive SourceKind = "group_live"
	// SourceGroupFallback обозначает счётчики входа заглушки отдельной группы.
	SourceGroupFallback SourceKind = "group_fallback"
	// SourceGroupOutput обозначает счётчики общего выхода группы после переключения источников.
	SourceGroupOutput SourceKind = "group_output"
	// SourceOutput обозначает счётчики отдельного исходящего отправителя.
	SourceOutput SourceKind = "output"
	// SourceFFmpeg обозначает вид источника FFmpeg в идентичности расчётчика.
	SourceFFmpeg SourceKind = "ffmpeg"
)

// SourceIdentity отделяет окна счётчиков разных владельцев, публикаций, попыток и направлений.
type SourceIdentity struct {
	// Token связывает окно с токеном наблюдателя; Attempt позволяет отделять попытки: смена любого
	// значения сбрасывает накопленные выборки.
	Token, Attempt uint64
	// LeaseID отделяет входящие публикации, чтобы новые счётчики не сравнивались с прежней
	// публикацией.
	LeaseID core.ID
	// Kind отделяет направления источника при сравнении идентичности окна.
	Kind SourceKind
}

// RawCounters копируется под блокировкой производителя вместе с его SampledAt.
// RawCounters переносит согласованные накопленные счётчики кадров и байтов с временем производителя для
// расчёта оконных скоростей.
type RawCounters struct {
	// SampledAt содержит согласованное время производителя для вычисления фактического интервала
	// скоростей.
	SampledAt time.Time
	// VideoFrames считает видеокадры; PayloadBytes считает полезные байты; WrittenBytes считает
	// записанные транспортом байты для независимых FPS и битрейтов.
	VideoFrames, PayloadBytes, WrittenBytes uint64
	// Saturated сообщает о насыщении исходных счётчиков, при котором скорость нельзя достоверно
	// вычислить.
	Saturated bool
}

// RateCalculator последовательно накапливает три выборки одного источника для FPS и двухинтервальных
// битрейтов.
type RateCalculator struct {
	// staleAfter задаёт максимальный возраст исходной выборки перед расчётом скоростей.
	staleAfter time.Duration
	// identity сохраняет владельца и направление текущего окна; изменение сбрасывает накопленные
	// выборки.
	identity SourceIdentity
	// samples сохраняет до трёх последовательных согласованных выборок одного источника.
	samples [3]RawCounters
	// count содержит число заполненных выборок для различения прогрева и полного окна битрейтов.
	count int
}

// NewRateCalculator создаёт расчётчик оконных скоростей с положительным сроком свежести. Его состояние
// изменяется последовательными вызовами одного владельца.
func NewRateCalculator(staleAfter time.Duration) (*RateCalculator, error) {
	if staleAfter <= 0 {
		return nil, core.ErrInvalid
	}
	return &RateCalculator{staleAfter: staleAfter}, nil
}

// unavailable задаёт одинаковые статус и причину недоступности для FPS и двух битрейтов без измеренных
// значений.
func unavailable(status core.MetricStatus, reason core.MetricReason) core.MediaRates {
	// v задаёт общее состояние недоступности, копируемое в FPS и оба битрейта.
	v := core.Rate{Status: status, Reason: reason}
	return core.MediaRates{FPS: v, PayloadBitrate: v, WrittenBitrate: v}
}

// Absent сбрасывает идентичность и накопленное окно источника, возвращая source_absent для всех
// скоростей.
func (c *RateCalculator) Absent() core.MediaRates {
	c.count = 0
	c.samples = [3]RawCounters{}
	c.identity = SourceIdentity{}
	return unavailable(core.MetricUnavailable, core.ReasonSourceAbsent)
}

// Sample вычисляет скорости по последовательным согласованным счётчикам и времени производителя,
// отдельно обозначая неполное окно, регресс, насыщение и недостоверную выборку. Смена идентичности
// начинает новое окно; FPS использует последний фактический интервал, а битрейты — два
// последовательных. Нулевой прирост после полного окна остаётся измеренным нулём.
func (c *RateCalculator) Sample(identity SourceIdentity, raw RawCounters, now time.Time) core.MediaRates {
	if c.identity != identity {
		c.count = 0
		c.samples = [3]RawCounters{}
		c.identity = identity
	}
	if raw.SampledAt.IsZero() || now.Before(raw.SampledAt) {
		c.count = 0
		return unavailable(core.MetricUnavailable, core.ReasonInvalidSample)
	}
	if now.Sub(raw.SampledAt) > c.staleAfter {
		c.count = 0
		return unavailable(core.MetricStale, core.ReasonStale)
	}
	if raw.Saturated {
		c.count = 0
		return unavailable(core.MetricSaturated, core.ReasonCounterSaturated)
	}
	if c.count > 0 {
		// previous содержит последнюю выборку для проверки строгого роста времени и отсутствия
		// регрессии счётчиков.
		previous := c.samples[c.count-1]
		if !raw.SampledAt.After(previous.SampledAt) {
			return unavailable(core.MetricUnavailable, core.ReasonInvalidSample)
		}
		if raw.VideoFrames < previous.VideoFrames || raw.PayloadBytes < previous.PayloadBytes || raw.WrittenBytes < previous.WrittenBytes {
			c.samples = [3]RawCounters{raw}
			c.count = 1
			return unavailable(core.MetricCounterReset, core.ReasonCounterRegression)
		}
	}
	if c.count == 3 {
		c.samples[0], c.samples[1] = c.samples[1], c.samples[2]
		c.count = 2
	}
	c.samples[c.count] = raw
	c.count++
	// result начинает с прогрева и получает доступные скорости по мере заполнения необходимых
	// интервалов.
	result := unavailable(core.MetricWarmingUp, core.ReasonWarmingUp)
	if c.count >= 2 {
		// previous содержит начало последнего фактического интервала для расчёта FPS.
		previous := c.samples[c.count-2]
		result.FPS = rate(raw.VideoFrames-previous.VideoFrames, 1, previous.SampledAt, raw.SampledAt)
	}
	if c.count == 3 {
		// first содержит начало двух последовательных интервалов для расчёта обоих битрейтов.
		first := c.samples[0]
		result.PayloadBitrate = rate(raw.PayloadBytes-first.PayloadBytes, 8, first.SampledAt, raw.SampledAt)
		result.WrittenBitrate = rate(raw.WrittenBytes-first.WrittenBytes, 8, first.SampledAt, raw.SampledAt)
	}
	return result
}

// rate вычисляет скорость по положительному фактическому интервалу между выборками, возвращая его
// границы в UTC и точную длительность.
func rate(delta uint64, multiplier float64, start, end time.Time) core.Rate {
	// duration содержит фактическую длительность окна, по которой прирост переводится в значение за
	// секунду.
	duration := end.Sub(start)
	return core.Rate{Status: core.MetricAvailable, Value: float64(delta) * multiplier / duration.Seconds(), WindowStart: start.UTC(), WindowEnd: end.UTC(), Duration: duration}
}
