package api

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет одинаковую проекцию ACK-метрик в снимок и событие: точные uint64-строки, начальные нули,
// null и UTC без дублирования в counters.
func TestOutputACKProjectionPreservesDecimalNullAndUTCForSnapshotAndEvent(t *testing.T) {
	// at задаёт точное наносекундное время вне UTC для проверки нормализации времени ACK.
	at := time.Date(2026, time.November, 8, 12, 13, 14, 987654321, time.FixedZone("UTC+3", 3*60*60))
	// tc задаёт начальные либо измеренные ACK-метрики и точное ожидаемое JSON-представление.
	for _, tc := range []struct {
		// name задаёт название начального или измеренного случая ACK.
		name string
		// metrics содержит исходные счётчики ACK, включая MaxUint64 и значение за пределами
		// точности JavaScript.
		metrics core.OutputMetrics
		// want задаёт обязательные ACK-поля с точными строками, null или UTC-временем.
		want map[string]any
	}{
		{"initial", core.OutputMetrics{}, map[string]any{"ack_ahead_events_total": "0", "ack_ahead_bytes_last": "0", "ack_ahead_bytes_max": "0", "ack_last_anomaly_at": nil}},
		{"measured", core.OutputMetrics{AckAheadEventsTotal: math.MaxUint64, AckAheadBytesLast: 9007199254740993, AckAheadBytesMax: math.MaxUint64, AckLastAnomalyAt: at}, map[string]any{"ack_ahead_events_total": "18446744073709551615", "ack_ahead_bytes_last": "9007199254740993", "ack_ahead_bytes_max": "18446744073709551615", "ack_last_anomaly_at": "2026-11-08T09:13:14.987654321Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет точные десятичные значения ACK, null и время UTC в проекциях снимка и события для
			// выбранного набора метрик.

			tc.metrics.Attempts = 7
			// snapshot содержит проекцию измерений выхода в текущем снимке Session.
			snapshot := runtimeSessionDTO(core.SessionRuntimeSnapshot{Outputs: []core.OutputObservationSnapshot{{Metrics: &tc.metrics}}})
			// event содержит проекцию тех же измерений в оперативном событии.
			event := runtimeEventDTO(core.RuntimeEvent{Kind: core.RuntimeMetricsEvent, Output: &tc.metrics})
			// projected содержит очередное представление ACK из снимка или события для
			// одинаковой проверки.
			for _, projected := range []*OutputMetricsDTO{snapshot.Outputs[0].Metrics, event.Output} {
				// data содержит сериализованные метрики; err содержит отказ
				// JSON-кодирования проекции.
				data, err := json.Marshal(projected)
				if err != nil {
					t.Fatal(err)
				}
				// wire принимает публичный JSON-объект для проверки точных типов и
				// обязательных ACK-полей.
				var wire map[string]any
				if err = json.Unmarshal(data, &wire); err != nil {
					t.Fatal(err)
				}
				// counters содержит прежние счётчики отправителя, где ACK-поля не должны
				// дублироваться.
				counters := wire["counters"].(map[string]any)
				if counters["attempts"] != "7" {
					t.Fatalf("существующие счётчики изменены: %s", data)
				}
				// name задаёт очередное ACK-поле; want содержит его точное ожидаемое
				// JSON-значение.
				for name, want := range tc.want {
					// got содержит фактически выведенное ACK-значение; present
					// подтверждает обязательное присутствие поля.
					got, present := wire[name]
					if !present || got != want {
						t.Fatalf("поле %s: получено %#v, ожидается %#v; %s", name, got, want, data)
					}
					// duplicate отмечает запрещённое повторение самостоятельного
					// ACK-поля внутри counters.
					if _, duplicate := counters[name]; duplicate {
						t.Fatalf("поле ACK продублировано в counters: %s", data)
					}
				}
			}
		})
	}
	if outputMetricsDTO(nil) != nil {
		t.Fatal("отсутствующие метрики выданы как наблюдение")
	}
}
