package api_test

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/api"
)

// Проверяет обязательные строковые поля ACK и время с допустимым null в опубликованном OpenAPI,
// точность JSON за пределами JavaScript и неизменность спецификации после сериализации DTO.
func TestACKOpenAPIAcceptanceExplicitDecimalAndNullableTime(t *testing.T) {
	// f содержит настоящий API, чья опубликованная спецификация проверяется до и после сериализации
	// DTO.
	f := newAPIAcceptanceFixture(t, nil)
	// first сохраняет исходные неизменяемые байты OpenAPI JSON.
	first := apiAcceptanceRequest(t, f.api, "GET", "/openapi.json", "", 200)
	// spec содержит разобранную фактическую спецификацию API.
	spec := apiAcceptanceJSON(t, first)
	// schemas содержит компоненты опубликованных JSON-схем.
	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
	// schema содержит описание OutputMetricsDTO; ok подтверждает наличие объектной схемы.
	schema, ok := schemas["OutputMetricsDTO"].(map[string]any)
	if !ok {
		t.Fatal("нет схемы OutputMetricsDTO")
	}
	// props содержит свойства метрик выхода для проверки ACK-типов и времени.
	props := schema["properties"].(map[string]any)
	// required индексирует обязательные поля схемы OutputMetricsDTO.
	required := map[string]bool{}
	// v содержит очередное имя обязательного поля, добавляемое в индекс required.
	for _, v := range schema["required"].([]any) {
		required[v.(string)] = true
	}
	// name задаёт очередное ACK-поле, которое должно быть обязательной Decimal-строкой.
	for _, name := range []string{"ack_ahead_events_total", "ack_ahead_bytes_last", "ack_ahead_bytes_max"} {
		// v содержит схему текущего ACK-поля; ok подтверждает наличие описания свойства.
		v, ok := props[name].(map[string]any)
		if !ok || v["type"] != "string" || v["pattern"] != "^(0|[1-9][0-9]{0,19})$" || !required[name] {
			t.Fatalf("неверный контракт Decimal %s: %#v required=%v", name, v, required)
		}
	}
	// clock содержит схему ack_last_anomaly_at с обязательным присутствием и допустимостью null.
	clock, ok := props["ack_last_anomaly_at"].(map[string]any)
	if !ok || !reflect.DeepEqual(clock["type"], []any{"string", "null"}) || clock["format"] != "date-time" || !required["ack_last_anomaly_at"] {
		t.Fatalf("время должно быть явным полем даты с допустимым null: %#v", clock)
	}
	// dto задаёт максимальные и выходящие за точность JavaScript ACK-счётчики без времени события.
	dto := api.OutputMetricsDTO{AckAheadEventsTotal: api.Decimal(math.MaxUint64), AckAheadBytesLast: api.Decimal(9007199254740993), AckAheadBytesMax: api.Decimal(math.MaxUint64)}
	// raw содержит JSON метрик; err содержит отказ его сериализации.
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	// wire принимает JSON-метрики для проверки точных десятичных строк и начального null времени.
	var wire map[string]any
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["ack_ahead_events_total"] != "18446744073709551615" || wire["ack_ahead_bytes_last"] != "9007199254740993" || wire["ack_ahead_bytes_max"] != "18446744073709551615" || wire["ack_last_anomaly_at"] != nil {
		t.Fatalf("потеря точности либо неверное значение null: %s", raw)
	}
	// second сохраняет OpenAPI после сериализации DTO для побайтовой проверки неизменности
	// спецификации.
	second := apiAcceptanceRequest(t, f.api, "GET", "/openapi.json", "", 200)
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatal("сериализация метрик изменила опубликованный OpenAPI")
	}
}
