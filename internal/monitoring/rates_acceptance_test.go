package monitoring

import (
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestAcceptanceRatesUseProducerElapsedWindows(t *testing.T) {
	c, err := NewRateCalculator(10 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	id := SourceIdentity{Token: 3, Kind: SourceOutput}
	start := time.Now()
	first := c.Sample(id, RawCounters{SampledAt: start, VideoFrames: 100, PayloadBytes: 9000, WrittenBytes: 12000}, start)
	if first.FPS.Status != core.MetricWarmingUp || first.PayloadBitrate.Status != core.MetricWarmingUp {
		t.Fatal("первый снимок выдан за измеренное окно", first)
	}
	second := c.Sample(id, RawCounters{SampledAt: start.Add(1500 * time.Millisecond), VideoFrames: 145, PayloadBytes: 12000, WrittenBytes: 18000}, start.Add(1700*time.Millisecond))
	if second.FPS.Value != 30 || second.FPS.Duration != 1500*time.Millisecond || second.PayloadBitrate.Status != core.MetricWarmingUp {
		t.Fatal("окно FPS или прогрев битрейта неверны", second)
	}
	third := c.Sample(id, RawCounters{SampledAt: start.Add(4 * time.Second), VideoFrames: 195, PayloadBytes: 17000, WrittenBytes: 32000}, start.Add(4500*time.Millisecond))
	if third.FPS.Value != 20 || third.FPS.Duration != 2500*time.Millisecond || third.PayloadBitrate.Value != 16000 || third.WrittenBitrate.Value != 40000 || third.PayloadBitrate.Duration != 4*time.Second {
		t.Fatal("неверные скорости по известным кадрам/байтам/времени", third)
	}
	if !third.FPS.WindowStart.Equal(start.Add(1500*time.Millisecond)) || !third.PayloadBitrate.WindowStart.Equal(start) || !third.WrittenBitrate.WindowEnd.Equal(start.Add(4*time.Second)) || third.FPS.WindowEnd.Location() != time.UTC {
		t.Fatal("окна не привязаны к производителю", third)
	}
	for _, offset := range []time.Duration{5 * time.Second, 6 * time.Second} {
		third = c.Sample(id, RawCounters{SampledAt: start.Add(offset), VideoFrames: 195, PayloadBytes: 17000, WrittenBytes: 32000}, start.Add(offset))
	}
	if third.FPS.Status != core.MetricAvailable || third.FPS.Value != 0 || third.PayloadBitrate.Status != core.MetricAvailable || third.PayloadBitrate.Value != 0 || third.WrittenBitrate.Value != 0 {
		t.Fatal("полное пустое окно не является измеренным нулём", third)
	}
}

func TestAcceptanceRatesFenceEveryIdentityAndRecoverFromUnavailable(t *testing.T) {
	start := time.Now()
	for _, change := range []struct {
		name string
		edit func(*SourceIdentity)
	}{
		{"token", func(i *SourceIdentity) { i.Token++ }},
		{"attempt", func(i *SourceIdentity) { i.Attempt++ }},
		{"lease", func(i *SourceIdentity) { i.LeaseID = "replacement" }},
		{"kind", func(i *SourceIdentity) { i.Kind = SourceGroupFallback }},
	} {
		t.Run(change.name, func(t *testing.T) {
			c, err := NewRateCalculator(10 * time.Second)
			if err != nil {
				t.Fatal(err)
			}
			id := SourceIdentity{Token: 1, Attempt: 1, LeaseID: "publication", Kind: SourceInput}
			for n := 0; n < 3; n++ {
				at := start.Add(time.Duration(n) * time.Second)
				c.Sample(id, RawCounters{SampledAt: at, VideoFrames: uint64(30 * n), PayloadBytes: uint64(1000 * n)}, at)
			}
			change.edit(&id)
			got := c.Sample(id, RawCounters{SampledAt: start.Add(3 * time.Second), VideoFrames: 900, PayloadBytes: 90000}, start.Add(3*time.Second))
			if got.FPS.Status != core.MetricWarmingUp || got.PayloadBitrate.Status != core.MetricWarmingUp {
				t.Fatal("смена идентичности унаследовала исходные измерения", got)
			}
		})
	}
	for _, scenario := range []struct {
		name   string
		raw    RawCounters
		now    time.Time
		status core.MetricStatus
		reason core.MetricReason
	}{
		{"regression", RawCounters{SampledAt: start.Add(time.Second), VideoFrames: 1}, start.Add(time.Second), core.MetricCounterReset, core.ReasonCounterRegression},
		{"saturation", RawCounters{SampledAt: start.Add(time.Second), Saturated: true}, start.Add(time.Second), core.MetricSaturated, core.ReasonCounterSaturated},
		{"stale", RawCounters{SampledAt: start.Add(time.Second)}, start.Add(20 * time.Second), core.MetricStale, core.ReasonStale},
		{"future", RawCounters{SampledAt: start.Add(2 * time.Second)}, start.Add(time.Second), core.MetricUnavailable, core.ReasonInvalidSample},
		{"zero_timestamp", RawCounters{}, start, core.MetricUnavailable, core.ReasonInvalidSample},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			c, err := NewRateCalculator(5 * time.Second)
			if err != nil {
				t.Fatal(err)
			}
			id := SourceIdentity{Token: 1}
			c.Sample(id, RawCounters{SampledAt: start, VideoFrames: 10, PayloadBytes: 100, WrittenBytes: 200}, start)
			got := c.Sample(id, scenario.raw, scenario.now)
			for _, r := range []core.Rate{got.FPS, got.PayloadBitrate, got.WrittenBitrate} {
				if r.Status != scenario.status || r.Reason != scenario.reason || r.Value != 0 {
					t.Fatal("недоступное измерение выдано за свежий ноль", got)
				}
			}
			if absent := c.Absent(); absent.FPS.Reason != core.ReasonSourceAbsent {
				t.Fatal(absent)
			}
			got = c.Sample(id, RawCounters{SampledAt: start.Add(30 * time.Second)}, start.Add(30*time.Second))
			if got.FPS.Status != core.MetricWarmingUp || got.WrittenBitrate.Status != core.MetricWarmingUp {
				t.Fatal("возвращённый источник унаследовал старое окно", got)
			}
		})
	}
}
