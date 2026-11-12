package monitoring

import (
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет известные приросты кадров и байтов на неравных фактических интервалах, границы UTC и
// измеренный ноль после полного пустого окна.
func TestAcceptanceRatesUseProducerElapsedWindows(t *testing.T) {
	// c содержит расчётчик десятисекундной свежести; err содержит отказ его создания.
	c, err := NewRateCalculator(10 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// id задаёт источник исходящего выхода для известных кадровых и байтовых приростов.
	id := SourceIdentity{Token: 3, Kind: SourceOutput}
	// start задаёт исходное время производителя, от которого задаются неравные интервалы.
	start := time.Now()
	// first содержит начальную выборку без завершённого окна скоростей.
	first := c.Sample(id, RawCounters{SampledAt: start, VideoFrames: 100, PayloadBytes: 9000, WrittenBytes: 12000}, start)
	if first.FPS.Status != core.MetricWarmingUp || first.PayloadBitrate.Status != core.MetricWarmingUp {
		t.Fatal("первый снимок выдан за измеренное окно", first)
	}
	// second содержит FPS за 1.5 секунды и ещё прогревающийся битрейт.
	second := c.Sample(id, RawCounters{SampledAt: start.Add(1500 * time.Millisecond), VideoFrames: 145, PayloadBytes: 12000, WrittenBytes: 18000}, start.Add(1700*time.Millisecond))
	if second.FPS.Value != 30 || second.FPS.Duration != 1500*time.Millisecond || second.PayloadBitrate.Status != core.MetricWarmingUp {
		t.Fatal("окно FPS или прогрев битрейта неверны", second)
	}
	// third содержит FPS последнего интервала и оба битрейта полного окна; затем проверяется нулевой
	// прирост.
	third := c.Sample(id, RawCounters{SampledAt: start.Add(4 * time.Second), VideoFrames: 195, PayloadBytes: 17000, WrittenBytes: 32000}, start.Add(4500*time.Millisecond))
	if third.FPS.Value != 20 || third.FPS.Duration != 2500*time.Millisecond || third.PayloadBitrate.Value != 16000 || third.WrittenBitrate.Value != 40000 || third.PayloadBitrate.Duration != 4*time.Second {
		t.Fatal("неверные скорости по известным кадрам/байтам/времени", third)
	}
	if !third.FPS.WindowStart.Equal(start.Add(1500*time.Millisecond)) || !third.PayloadBitrate.WindowStart.Equal(start) || !third.WrittenBitrate.WindowEnd.Equal(start.Add(4*time.Second)) || third.FPS.WindowEnd.Location() != time.UTC {
		t.Fatal("окна не привязаны к производителю", third)
	}
	// offset задаёт очередное время выборки без прироста для заполнения полного нулевого окна.
	for _, offset := range []time.Duration{5 * time.Second, 6 * time.Second} {
		third = c.Sample(id, RawCounters{SampledAt: start.Add(offset), VideoFrames: 195, PayloadBytes: 17000, WrittenBytes: 32000}, start.Add(offset))
	}
	if third.FPS.Status != core.MetricAvailable || third.FPS.Value != 0 || third.PayloadBitrate.Status != core.MetricAvailable || third.PayloadBitrate.Value != 0 || third.WrittenBitrate.Value != 0 {
		t.Fatal("полное пустое окно не является измеренным нулём", third)
	}
}

// Проверяет отдельный сброс при изменении Token, Attempt, LeaseID и Kind и сохранение явных причин
// недоступности до прогрева восстановленного источника.
func TestAcceptanceRatesFenceEveryIdentityAndRecoverFromUnavailable(t *testing.T) {
	// start задаёт общую временную основу сценариев смены идентичности и недоступности.
	start := time.Now()
	// change задаёт одно изменение идентичности, которое должно сбросить расчётное окно.
	for _, change := range []struct {
		// name задаёт название проверяемого компонента идентичности.
		name string
		// edit изменяет только выбранный компонент SourceIdentity в подготовленном полном окне.
		edit func(*SourceIdentity)
	}{
		{"token", func(i *SourceIdentity) {
			// Меняет Token источника для проверки сброса накопленного окна скоростей.
			i.Token++
		}},
		{"attempt", func(i *SourceIdentity) {
			// Меняет Attempt источника для проверки независимости измерений новой попытки.
			i.Attempt++
		}},
		{"lease", func(i *SourceIdentity) {
			// Подставляет новый LeaseID для проверки сброса окна при смене публикации.
			i.LeaseID = "replacement"
		}},
		{"kind", func(i *SourceIdentity) {
			// Меняет Kind источника для проверки независимости измерений другого медиатракта.
			i.Kind = SourceGroupFallback
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			// Проверяет, что выбранное изменение идентичности источника начинает новые окна измерений.

			// c содержит отдельный расчётчик текущей смены идентичности; err содержит отказ
			// создания.
			c, err := NewRateCalculator(10 * time.Second)
			if err != nil {
				t.Fatal(err)
			}
			// id задаёт полную исходную идентичность, изменяемую текущим edit.
			id := SourceIdentity{Token: 1, Attempt: 1, LeaseID: "publication", Kind: SourceInput}
			// n указывает исходную выборку полного трёхотсчётного окна.
			for n := range 3 {
				// at задаёт время очередной исходной выборки производителя.
				at := start.Add(time.Duration(n) * time.Second)
				c.Sample(id, RawCounters{SampledAt: at, VideoFrames: uint64(30 * n), PayloadBytes: uint64(1000 * n)}, at)
			}
			change.edit(&id)
			// got содержит первую выборку после смены идентичности, которая должна начать
			// новый прогрев.
			got := c.Sample(id, RawCounters{SampledAt: start.Add(3 * time.Second), VideoFrames: 900, PayloadBytes: 90000}, start.Add(3*time.Second))
			if got.FPS.Status != core.MetricWarmingUp || got.PayloadBitrate.Status != core.MetricWarmingUp {
				t.Fatal("смена идентичности унаследовала исходные измерения", got)
			}
		})
	}
	// scenario задаёт причину недоступности и её точные ожидаемые status и reason.
	for _, scenario := range []struct {
		// name задаёт название отказа исходной выборки.
		name string
		// raw содержит счётчики, провоцирующие регрессию, насыщение или неверное время.
		raw RawCounters
		// now задаёт время приёма для проверки будущего или просроченного SampledAt.
		now time.Time
		// status задаёт ожидаемое состояние всех трёх скоростей.
		status core.MetricStatus
		// reason задаёт ожидаемую ограниченную причину недоступности.
		reason core.MetricReason
	}{
		{"regression", RawCounters{SampledAt: start.Add(time.Second), VideoFrames: 1}, start.Add(time.Second), core.MetricCounterReset, core.ReasonCounterRegression},
		{"saturation", RawCounters{SampledAt: start.Add(time.Second), Saturated: true}, start.Add(time.Second), core.MetricSaturated, core.ReasonCounterSaturated},
		{"stale", RawCounters{SampledAt: start.Add(time.Second)}, start.Add(20 * time.Second), core.MetricStale, core.ReasonStale},
		{"future", RawCounters{SampledAt: start.Add(2 * time.Second)}, start.Add(time.Second), core.MetricUnavailable, core.ReasonInvalidSample},
		{"zero_timestamp", RawCounters{}, start, core.MetricUnavailable, core.ReasonInvalidSample},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// Проверяет выбранную причину недоступности и повторный прогрев после возвращения источника.

			// c содержит отдельный расчётчик для текущего отказа и восстановления; err
			// содержит отказ создания.
			c, err := NewRateCalculator(5 * time.Second)
			if err != nil {
				t.Fatal(err)
			}
			// id задаёт неизменный источник внутри сценария недоступности и возврата.
			id := SourceIdentity{Token: 1}
			c.Sample(id, RawCounters{SampledAt: start, VideoFrames: 10, PayloadBytes: 100, WrittenBytes: 200}, start)
			// got содержит недоступные скорости, а затем первую выборку восстановленного
			// источника.
			got := c.Sample(id, scenario.raw, scenario.now)
			// r содержит очередной FPS или битрейт для одинаковой проверки статуса, причины
			// и отсутствия значения.
			for _, r := range []core.Rate{got.FPS, got.PayloadBitrate, got.WrittenBitrate} {
				if r.Status != scenario.status || r.Reason != scenario.reason || r.Value != 0 {
					t.Fatal("недоступное измерение выдано за свежий ноль", got)
				}
			}
			// absent содержит явный результат отсутствия источника, сбрасывающий старые
			// окна.
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
