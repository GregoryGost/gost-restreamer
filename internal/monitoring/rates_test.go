package monitoring

import (
	"math"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestRatesUseProducerIntervalsAndResetExactIdentity(t *testing.T) {
	c, err := NewRateCalculator(10 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("local", 3600))
	id := SourceIdentity{Token: 1, Kind: SourceGroupLive}
	first := c.Sample(id, RawCounters{SampledAt: base, VideoFrames: 10, PayloadBytes: 100, WrittenBytes: 120}, base)
	if first.FPS.Status != core.MetricWarmingUp || first.PayloadBitrate.Status != core.MetricWarmingUp {
		t.Fatalf("ранняя скорость: %+v", first)
	}
	second := c.Sample(id, RawCounters{SampledAt: base.Add(time.Second), VideoFrames: 40, PayloadBytes: 1100, WrittenBytes: 2120}, base.Add(time.Second))
	if second.FPS.Value != 30 || second.FPS.Duration != time.Second || second.PayloadBitrate.Status != core.MetricWarmingUp {
		t.Fatalf("первое окно: %+v", second)
	}
	third := c.Sample(id, RawCounters{SampledAt: base.Add(4 * time.Second), VideoFrames: 100, PayloadBytes: 4100, WrittenBytes: 8120}, base.Add(4*time.Second))
	if third.FPS.Value != 20 || third.FPS.Duration != 3*time.Second || third.PayloadBitrate.Value != 8000 || third.WrittenBitrate.Value != 16000 || third.PayloadBitrate.Duration != 4*time.Second || third.FPS.WindowEnd.Location() != time.UTC {
		t.Fatalf("фактические окна: %+v", third)
	}
	id.Kind = SourceGroupOutput
	if got := c.Sample(id, RawCounters{SampledAt: base.Add(5 * time.Second)}, base.Add(5*time.Second)); got.FPS.Status != core.MetricWarmingUp {
		t.Fatalf("чужое исходное измерение: %+v", got)
	}
	if got := c.Sample(id, RawCounters{SampledAt: base.Add(6 * time.Second)}, base.Add(6*time.Second)); got.FPS.Status != core.MetricAvailable || got.FPS.Value != 0 {
		t.Fatalf("пустое окно не измерено: %+v", got)
	}
	if got := c.Sample(id, RawCounters{SampledAt: base.Add(7 * time.Second)}, base.Add(7*time.Second)); got.PayloadBitrate.Status != core.MetricAvailable || got.PayloadBitrate.Value != 0 {
		t.Fatalf("пустой bitrate: %+v", got)
	}
	id.Attempt++
	if got := c.Sample(id, RawCounters{SampledAt: base.Add(8 * time.Second)}, base.Add(8*time.Second)); got.FPS.Status != core.MetricWarmingUp {
		t.Fatalf("новая попытка: %+v", got)
	}
}
func TestRatesDistinguishUnavailableStaleRegressionSaturation(t *testing.T) {
	if _, err := NewRateCalculator(0); err == nil {
		t.Fatal("пустая свежесть принята")
	}
	c, _ := NewRateCalculator(time.Second)
	base := time.Now()
	id := SourceIdentity{Token: 1}
	cases := []struct {
		name string
		raw  RawCounters
		now  time.Time
		want core.MetricStatus
	}{
		{"zero", RawCounters{}, base, core.MetricUnavailable},
		{"future", RawCounters{SampledAt: base.Add(time.Second)}, base, core.MetricUnavailable},
		{"stale", RawCounters{SampledAt: base}, base.Add(2 * time.Second), core.MetricStale},
		{"saturation", RawCounters{SampledAt: base, Saturated: true}, base, core.MetricSaturated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Sample(id, tc.raw, tc.now); got.FPS.Status != tc.want || got.FPS.Value != 0 {
				t.Fatalf("%+v", got)
			}
		})
	}
	c.Sample(id, RawCounters{SampledAt: base, VideoFrames: 100}, base)
	if got := c.Sample(id, RawCounters{SampledAt: base.Add(time.Second), VideoFrames: 99}, base.Add(time.Second)); got.FPS.Status != core.MetricCounterReset {
		t.Fatalf("регрессия: %+v", got)
	}
	if got := c.Sample(id, RawCounters{SampledAt: base.Add(2 * time.Second), VideoFrames: 100}, base.Add(2*time.Second)); got.FPS.Value != 1 || got.PayloadBitrate.Status != core.MetricWarmingUp {
		t.Fatalf("восстановление: %+v", got)
	}
	if got := c.Sample(id, RawCounters{SampledAt: base.Add(2 * time.Second), VideoFrames: 100}, base.Add(2*time.Second)); got.FPS.Status != core.MetricUnavailable {
		t.Fatalf("повтор времени: %+v", got)
	}
	if got := c.Absent(); got.FPS.Status != core.MetricUnavailable || got.FPS.Reason != core.ReasonSourceAbsent {
		t.Fatalf("отсутствие: %+v", got)
	}
	if got := c.Sample(id, RawCounters{SampledAt: base.Add(3 * time.Second), VideoFrames: 100}, base.Add(3*time.Second)); got.FPS.Status != core.MetricWarmingUp {
		t.Fatalf("устаревшее исходное измерение: %+v", got)
	}
}
func TestRatesAvoidUnsignedBitrateMultiplicationOverflow(t *testing.T) {
	c, _ := NewRateCalculator(time.Minute)
	base := time.Now()
	id := SourceIdentity{Token: 1}
	c.Sample(id, RawCounters{SampledAt: base}, base)
	c.Sample(id, RawCounters{SampledAt: base.Add(time.Second), PayloadBytes: math.MaxUint64 / 2}, base.Add(time.Second))
	got := c.Sample(id, RawCounters{SampledAt: base.Add(2 * time.Second), PayloadBytes: math.MaxUint64}, base.Add(2*time.Second))
	if got.PayloadBitrate.Status != core.MetricAvailable || got.PayloadBitrate.Value != float64(uint64(math.MaxUint64))*4 {
		t.Fatalf("переполнение: %+v", got)
	}
}
