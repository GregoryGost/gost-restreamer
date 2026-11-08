package api_test

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/api"
)

func TestACKOpenAPIAcceptanceExplicitDecimalAndNullableTime(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	first := apiAcceptanceRequest(t, f.api, "GET", "/openapi.json", "", 200)
	spec := apiAcceptanceJSON(t, first)
	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
	schema, ok := schemas["OutputMetricsDTO"].(map[string]any)
	if !ok {
		t.Fatal("нет схемы OutputMetricsDTO")
	}
	props := schema["properties"].(map[string]any)
	required := map[string]bool{}
	for _, v := range schema["required"].([]any) {
		required[v.(string)] = true
	}
	for _, name := range []string{"ack_ahead_events_total", "ack_ahead_bytes_last", "ack_ahead_bytes_max"} {
		v, ok := props[name].(map[string]any)
		if !ok || v["type"] != "string" || v["pattern"] != "^(0|[1-9][0-9]{0,19})$" || !required[name] {
			t.Fatalf("неверный контракт Decimal %s: %#v required=%v", name, v, required)
		}
	}
	clock, ok := props["ack_last_anomaly_at"].(map[string]any)
	if !ok || !reflect.DeepEqual(clock["type"], []any{"string", "null"}) || clock["format"] != "date-time" || !required["ack_last_anomaly_at"] {
		t.Fatalf("время должно быть явным полем даты с допустимым null: %#v", clock)
	}
	dto := api.OutputMetricsDTO{AckAheadEventsTotal: api.Decimal(math.MaxUint64), AckAheadBytesLast: api.Decimal(9007199254740993), AckAheadBytesMax: api.Decimal(math.MaxUint64)}
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["ack_ahead_events_total"] != "18446744073709551615" || wire["ack_ahead_bytes_last"] != "9007199254740993" || wire["ack_ahead_bytes_max"] != "18446744073709551615" || wire["ack_last_anomaly_at"] != nil {
		t.Fatalf("потеря точности либо неверное значение null: %s", raw)
	}
	second := apiAcceptanceRequest(t, f.api, "GET", "/openapi.json", "", 200)
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatal("сериализация метрик изменила опубликованный OpenAPI")
	}
}
