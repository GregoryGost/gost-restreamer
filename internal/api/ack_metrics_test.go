package api

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestOutputACKProjectionPreservesDecimalNullAndUTCForSnapshotAndEvent(t *testing.T) {
	at := time.Date(2026, time.November, 8, 12, 13, 14, 987654321, time.FixedZone("UTC+3", 3*60*60))
	for _, tc := range []struct {
		name    string
		metrics core.OutputMetrics
		want    map[string]any
	}{
		{"initial", core.OutputMetrics{}, map[string]any{"ack_ahead_events_total": "0", "ack_ahead_bytes_last": "0", "ack_ahead_bytes_max": "0", "ack_last_anomaly_at": nil}},
		{"measured", core.OutputMetrics{AckAheadEventsTotal: math.MaxUint64, AckAheadBytesLast: 9007199254740993, AckAheadBytesMax: math.MaxUint64, AckLastAnomalyAt: at}, map[string]any{"ack_ahead_events_total": "18446744073709551615", "ack_ahead_bytes_last": "9007199254740993", "ack_ahead_bytes_max": "18446744073709551615", "ack_last_anomaly_at": "2026-11-08T09:13:14.987654321Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.metrics.Attempts = 7
			snapshot := runtimeSessionDTO(core.SessionRuntimeSnapshot{Outputs: []core.OutputObservationSnapshot{{Metrics: &tc.metrics}}})
			event := runtimeEventDTO(core.RuntimeEvent{Kind: core.RuntimeMetricsEvent, Output: &tc.metrics})
			for _, projected := range []*OutputMetricsDTO{snapshot.Outputs[0].Metrics, event.Output} {
				data, err := json.Marshal(projected)
				if err != nil {
					t.Fatal(err)
				}
				var wire map[string]any
				if err = json.Unmarshal(data, &wire); err != nil {
					t.Fatal(err)
				}
				counters := wire["counters"].(map[string]any)
				if counters["attempts"] != "7" {
					t.Fatalf("существующие счётчики изменены: %s", data)
				}
				for name, want := range tc.want {
					got, present := wire[name]
					if !present || got != want {
						t.Fatalf("поле %s: получено %#v, ожидается %#v; %s", name, got, want, data)
					}
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
