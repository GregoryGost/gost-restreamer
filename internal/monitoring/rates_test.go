package monitoring

import (
	"math"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет фактические интервалы производителя, разные окна FPS и битрейта и сброс базы при смене
// источника или попытки.
func TestRatesUseProducerIntervalsAndResetExactIdentity(t *testing.T) {
	// c содержит расчётчик с десятисекундной свежестью; err содержит отказ его создания.
	c, err := NewRateCalculator(10 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// base задаёт фиксированное исходное время вне UTC для проверки фактических окон и нормализации
	// границ.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("local", 3600))
	// id задаёт точную идентичность источника и далее меняется для проверки сброса окна.
	id := SourceIdentity{Token: 1, Kind: SourceGroupLive}
	// first содержит результат единственной исходной выборки, который должен оставаться прогревом.
	first := c.Sample(id, RawCounters{SampledAt: base, VideoFrames: 10, PayloadBytes: 100, WrittenBytes: 120}, base)
	if first.FPS.Status != core.MetricWarmingUp || first.PayloadBitrate.Status != core.MetricWarmingUp {
		t.Fatalf("ранняя скорость: %+v", first)
	}
	// second содержит FPS первого секундного интервала и ещё неготовый двухинтервальный битрейт.
	second := c.Sample(id, RawCounters{SampledAt: base.Add(time.Second), VideoFrames: 40, PayloadBytes: 1100, WrittenBytes: 2120}, base.Add(time.Second))
	if second.FPS.Value != 30 || second.FPS.Duration != time.Second || second.PayloadBitrate.Status != core.MetricWarmingUp {
		t.Fatalf("первое окно: %+v", second)
	}
	// third содержит FPS трёхсекундного интервала и оба битрейта полного четырёхсекундного окна.
	third := c.Sample(id, RawCounters{SampledAt: base.Add(4 * time.Second), VideoFrames: 100, PayloadBytes: 4100, WrittenBytes: 8120}, base.Add(4*time.Second))
	if third.FPS.Value != 20 || third.FPS.Duration != 3*time.Second || third.PayloadBitrate.Value != 8000 || third.WrittenBitrate.Value != 16000 || third.PayloadBitrate.Duration != 4*time.Second || third.FPS.WindowEnd.Location() != time.UTC {
		t.Fatalf("фактические окна: %+v", third)
	}
	id.Kind = SourceGroupOutput
	// got содержит первую выборку другого направления после изменения Kind, ожидается прогрев.
	if got := c.Sample(id, RawCounters{SampledAt: base.Add(5 * time.Second)}, base.Add(5*time.Second)); got.FPS.Status != core.MetricWarmingUp {
		t.Fatalf("чужое исходное измерение: %+v", got)
	}
	// got содержит второй отсчёт без прироста, который уже даёт измеренный нулевой FPS.
	if got := c.Sample(id, RawCounters{SampledAt: base.Add(6 * time.Second)}, base.Add(6*time.Second)); got.FPS.Status != core.MetricAvailable || got.FPS.Value != 0 {
		t.Fatalf("пустое окно не измерено: %+v", got)
	}
	// got содержит полное окно без прироста, которое даёт измеренный нулевой битрейт.
	if got := c.Sample(id, RawCounters{SampledAt: base.Add(7 * time.Second)}, base.Add(7*time.Second)); got.PayloadBitrate.Status != core.MetricAvailable || got.PayloadBitrate.Value != 0 {
		t.Fatalf("пустой bitrate: %+v", got)
	}
	id.Attempt++
	// got содержит первую выборку новой Attempt для проверки отдельного сброса окна.
	if got := c.Sample(id, RawCounters{SampledAt: base.Add(8 * time.Second)}, base.Add(8*time.Second)); got.FPS.Status != core.MetricWarmingUp {
		t.Fatalf("новая попытка: %+v", got)
	}
}

// Проверяет различие отсутствия, устаревания, будущего времени, насыщения и регресса счётчиков с
// последующим прогревом нового окна.
func TestRatesDistinguishUnavailableStaleRegressionSaturation(t *testing.T) {
	// err содержит отказ создания расчётчика с нулевым сроком свежести.
	if _, err := NewRateCalculator(0); err == nil {
		t.Fatal("пустая свежесть принята")
	}
	// c содержит расчётчик секундной свежести для различения причин недоступности.
	c, _ := NewRateCalculator(time.Second)
	// base задаёт общее время сценариев неверных, просроченных и регрессирующих выборок.
	base := time.Now()
	// id задаёт постоянный источник внутри сценариев недоступности.
	id := SourceIdentity{Token: 1}
	// cases задаёт исходные счётчики, момент чтения и ожидаемую доступность.
	cases := []struct {
		// name задаёт название причины недоступности.
		name string
		// raw содержит согласованные счётчики либо намеренно неверное время или насыщение.
		raw RawCounters
		// now задаёт время проверки относительно времени исходной выборки.
		now time.Time
		// want задаёт ожидаемый MetricStatus текущего случая.
		want core.MetricStatus
	}{
		{"zero", RawCounters{}, base, core.MetricUnavailable},
		{"future", RawCounters{SampledAt: base.Add(time.Second)}, base, core.MetricUnavailable},
		{"stale", RawCounters{SampledAt: base}, base.Add(2 * time.Second), core.MetricStale},
		{"saturation", RawCounters{SampledAt: base, Saturated: true}, base, core.MetricSaturated},
	}
	// tc содержит очередной сценарий недоступности для расчёта по одному источнику.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет выбранный недопустимый либо устаревший набор счётчиков, различая отсутствие измерения и
			// измеренный ноль.

			// got содержит результат текущего недопустимого набора для проверки статуса и
			// отсутствия измеренного значения.
			if got := c.Sample(id, tc.raw, tc.now); got.FPS.Status != tc.want || got.FPS.Value != 0 {
				t.Fatalf("%+v", got)
			}
		})
	}
	c.Sample(id, RawCounters{SampledAt: base, VideoFrames: 100}, base)
	// got содержит результат регрессии числа кадров, ожидается отдельный counter_reset.
	if got := c.Sample(id, RawCounters{SampledAt: base.Add(time.Second), VideoFrames: 99}, base.Add(time.Second)); got.FPS.Status != core.MetricCounterReset {
		t.Fatalf("регрессия: %+v", got)
	}
	// got содержит первое доступное FPS-окно после регрессии при ещё прогревающемся битрейте.
	if got := c.Sample(id, RawCounters{SampledAt: base.Add(2 * time.Second), VideoFrames: 100}, base.Add(2*time.Second)); got.FPS.Value != 1 || got.PayloadBitrate.Status != core.MetricWarmingUp {
		t.Fatalf("восстановление: %+v", got)
	}
	// got содержит отказ повторной временной метки производителя.
	if got := c.Sample(id, RawCounters{SampledAt: base.Add(2 * time.Second), VideoFrames: 100}, base.Add(2*time.Second)); got.FPS.Status != core.MetricUnavailable {
		t.Fatalf("повтор времени: %+v", got)
	}
	// got содержит явное отсутствие источника после очистки накопленного окна.
	if got := c.Absent(); got.FPS.Status != core.MetricUnavailable || got.FPS.Reason != core.ReasonSourceAbsent {
		t.Fatalf("отсутствие: %+v", got)
	}
	// got содержит первую выборку вернувшегося источника, которая должна снова прогреваться.
	if got := c.Sample(id, RawCounters{SampledAt: base.Add(3 * time.Second), VideoFrames: 100}, base.Add(3*time.Second)); got.FPS.Status != core.MetricWarmingUp {
		t.Fatalf("устаревшее исходное измерение: %+v", got)
	}
}

// Проверяет расчёт битрейта до MaxUint64 без промежуточного беззнакового умножения байтов на восемь.
func TestRatesAvoidUnsignedBitrateMultiplicationOverflow(t *testing.T) {
	// c содержит расчётчик минутной свежести для проверки больших байтовых приростов.
	c, _ := NewRateCalculator(time.Minute)
	// base задаёт начало двухсекундного окна с максимальным приростом байтов.
	base := time.Now()
	// id задаёт постоянного владельца больших счётчиков.
	id := SourceIdentity{Token: 1}
	c.Sample(id, RawCounters{SampledAt: base}, base)
	c.Sample(id, RawCounters{SampledAt: base.Add(time.Second), PayloadBytes: math.MaxUint64 / 2}, base.Add(time.Second))
	// got содержит битрейт MaxUint64 байтов за две секунды без промежуточного целочисленного
	// переполнения.
	got := c.Sample(id, RawCounters{SampledAt: base.Add(2 * time.Second), PayloadBytes: math.MaxUint64}, base.Add(2*time.Second))
	if got.PayloadBitrate.Status != core.MetricAvailable || got.PayloadBitrate.Value != float64(uint64(math.MaxUint64))*4 {
		t.Fatalf("переполнение: %+v", got)
	}
}
