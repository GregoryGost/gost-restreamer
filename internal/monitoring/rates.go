package monitoring

import (
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

type SourceKind string

const (
	SourceInput         SourceKind = "input"
	SourceGroupLive     SourceKind = "group_live"
	SourceGroupFallback SourceKind = "group_fallback"
	SourceGroupOutput   SourceKind = "group_output"
	SourceOutput        SourceKind = "output"
	SourceFFmpeg        SourceKind = "ffmpeg"
)

type SourceIdentity struct {
	Token, Attempt uint64
	LeaseID        core.ID
	Kind           SourceKind
}

// RawCounters копируется под блокировкой производителя вместе с его SampledAt.
type RawCounters struct {
	SampledAt                               time.Time
	VideoFrames, PayloadBytes, WrittenBytes uint64
	Saturated                               bool
}

type RateCalculator struct {
	staleAfter time.Duration
	identity   SourceIdentity
	samples    [3]RawCounters
	count      int
}

func NewRateCalculator(staleAfter time.Duration) (*RateCalculator, error) {
	if staleAfter <= 0 {
		return nil, core.ErrInvalid
	}
	return &RateCalculator{staleAfter: staleAfter}, nil
}
func unavailable(status core.MetricStatus, reason core.MetricReason) core.MediaRates {
	v := core.Rate{Status: status, Reason: reason}
	return core.MediaRates{FPS: v, PayloadBitrate: v, WrittenBitrate: v}
}
func (c *RateCalculator) Absent() core.MediaRates {
	c.count = 0
	c.samples = [3]RawCounters{}
	c.identity = SourceIdentity{}
	return unavailable(core.MetricUnavailable, core.ReasonSourceAbsent)
}
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
	result := unavailable(core.MetricWarmingUp, core.ReasonWarmingUp)
	if c.count >= 2 {
		previous := c.samples[c.count-2]
		result.FPS = rate(raw.VideoFrames-previous.VideoFrames, 1, previous.SampledAt, raw.SampledAt)
	}
	if c.count == 3 {
		first := c.samples[0]
		result.PayloadBitrate = rate(raw.PayloadBytes-first.PayloadBytes, 8, first.SampledAt, raw.SampledAt)
		result.WrittenBitrate = rate(raw.WrittenBytes-first.WrittenBytes, 8, first.SampledAt, raw.SampledAt)
	}
	return result
}
func rate(delta uint64, multiplier float64, start, end time.Time) core.Rate {
	duration := end.Sub(start)
	return core.Rate{Status: core.MetricAvailable, Value: float64(delta) * multiplier / duration.Seconds(), WindowStart: start.UTC(), WindowEnd: end.UTC(), Duration: duration}
}
