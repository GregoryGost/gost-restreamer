package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/danielgtaylor/huma/v2"
)

// TestDecimalNumericTokens проверяет точные беззнаковые числа, математически целые
// десятичные записи и отсутствие изменения получателя после ошибочного ввода.
func TestDecimalNumericTokens(t *testing.T) {
	// tc сопоставляет исходный JSON-токен с независимо заданным целым значением.
	for _, tc := range []struct {
		// token содержит числовой JSON-токен; want задаёт его точное значение uint64.
		token string
		want  uint64
	}{
		{"0", 0}, {"1.0", 1}, {"1e0", 1}, {"100e-2", 1},
		{"9007199254740991", 9007199254740991}, {"9007199254740992", 9007199254740992},
		{"9007199254740993", 9007199254740993}, {"9007199254740993.000", 9007199254740993},
		{"9.007199254740993e15", 9007199254740993},
		{"18446744073709551615", math.MaxUint64}, {"1844674407370955161500e-2", math.MaxUint64},
		{"0e" + strings.Repeat("9", 4096), 0},
		{"1e+" + strings.Repeat("0", 4096), 1},
		{"0." + strings.Repeat("0", 4096) + "1e4097", 1},
	} {
		// Функция t.Run изолирует токен и сверяет значение и числовой тип ответа.
		t.Run(tc.token[:min(len(tc.token), 60)], func(t *testing.T) {
			// got начинает с ненулевого значения, чтобы проверить присваивание нуля.
			got := Decimal(73)
			if err := json.Unmarshal([]byte(tc.token), &got); err != nil {
				// err сообщает отказ корректного точного целого.
				t.Fatal(err)
			}
			if uint64(got) != tc.want {
				t.Fatalf("значение: got=%d want=%d", got, tc.want)
			}
			// raw содержит фактическую сериализацию; err сообщает отказ кодирования.
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			// number декодирует только JSON-число, исключая строковые эквиваленты.
			var number json.Number
			if len(raw) == 0 || raw[0] == '"' || json.Unmarshal(raw, &number) != nil || number.String() != strings.TrimSpace(string(raw)) {
				t.Fatalf("выдан не числовой токен: %s", raw)
			}
		})
	}
	// token перебирает неверные типы, знак, дроби и значения вне физического диапазона.
	for _, token := range []string{
		`"1"`, `"18446744073709551615"`, "null", "true", "[]", "{}",
		"-1", "-0", "-0.0", "-0e100", "1.1", "1e-1", "9007199254740993.1",
		"18446744073709551616", "18446744073709551615.1", "18446744073709551616e0",
		"1e" + strings.Repeat("9", 4096), "1e-" + strings.Repeat("9", 4096),
	} {
		// got сохраняет контрольное значение при отказе UnmarshalJSON.
		got := Decimal(73)
		if err := json.Unmarshal([]byte(token), &got); err == nil || got != 73 {
			// err отличает обязательный отказ от успешного либо частичного присваивания.
			t.Fatalf("неверный токен %.80q: значение=%d ошибка=%v", token, got, err)
		}
	}
}

// TestExactIntegerNormalization проверяет независимые арифметические примеры,
// отрицательный ноль и ограничение обеих машинных границ без округления.
func TestExactIntegerNormalization(t *testing.T) {
	// tc задаёт исходный токен и его математически эквивалентное целое.
	for _, tc := range []struct {
		// input содержит JSON-число; want задаёт точную каноническую запись.
		input, want string
	}{
		{"1.000e3", "1000"}, {"1.2300e2", "123"}, {"1000e-3", "1"},
		{"-0.0", "-0"}, {"-0e-" + strings.Repeat("9", 1024), "-0"},
		{"-922337203685477580800e-2", "-9223372036854775808"},
		{"18446744073709551615.0", "18446744073709551615"},
		{"0." + strings.Repeat("0", 3000) + "1e3001", "1"},
		{"10e-" + strings.Repeat("0", 3000) + "1", "1"},
	} {
		// got содержит результат; ok подтверждает целочисленность и допустимый диапазон.
		got, ok := exactIntegerToken(tc.input)
		if !ok || got != tc.want {
			t.Fatalf("нормализация %.80q: got=%q ok=%v want=%q", tc.input, got, ok, tc.want)
		}
	}
	// input перечисляет дроби, переполнения и лексемы вне JSON-грамматики.
	for _, input := range []string{"1.2301e2", "-9223372036854775809", "18446744073709551616", "1e999999999999", "1e-999999999999", "01", "+1", "1.", ".1", "NaN", `"1"`, "1 2"} {
		// got сохраняет ошибочную проекцию для диагностики; ok должен быть false.
		got, ok := exactIntegerToken(input)
		if ok {
			t.Fatalf("нецелый либо неверный токен принят: %q => %q", input, got)
		}
	}
}

// TestNumericNormalizerPreservesOriginalBytes проверяет побайтовое сохранение
// строк, экранированных ключей, повторов и пробелов при изменении только числовых токенов.
func TestNumericNormalizerPreservesOriginalBytes(t *testing.T) {
	// before содержит сложные строки рядом с дробью, переполнением и целыми формами.
	before := []byte(" {\n\t\"a\\u0031\":1.0, \"a1\":1e0,\"text\":\"1e3 \\\" \\\\ 123\",\"raw\":\"" + string([]byte{0xff}) + "\",\"v\":[-0.0,-9223372036854775808.0,18446744073709551615e0,1.5,1e9999999999,null]} ")
	// want задаёт точные ожидаемые байты без повторного кодирования всего JSON-дерева.
	want := []byte(" {\n\t\"a\\u0031\":1, \"a1\":1,\"text\":\"1e3 \\\" \\\\ 123\",\"raw\":\"" + string([]byte{0xff}) + "\",\"v\":[-0,-9223372036854775808,18446744073709551615,1.5,1e9999999999,null]} ")
	// original доказывает, что нормализация не изменяет исходный буфер запроса.
	original := bytes.Clone(before)
	if got := normalizeIntegerTokens(before); !bytes.Equal(got, want) || !bytes.Equal(before, original) {
		// got содержит фактический документ после нормализации.
		t.Fatalf("изменены посторонние байты: got=%q want=%q original=%q", got, want, before)
	}
}

// TestRuntimeCursorRetainsPreviousBytes закрепляет прежнее строковое кодирование sequence,
// включая строгий разбор, независимо от числового JSON публичных DTO.
func TestRuntimeCursorRetainsPreviousBytes(t *testing.T) {
	// current задаёт внутреннего владельца для восстановления cursor без публикации incarnation.
	current := core.RuntimeCursor{Incarnation: "private", Epoch: "epoch", Sequence: math.MaxUint64}
	// raw задаёт прежние точные байты v1, которые не должны зависеть от Decimal.
	const raw = `{"v":1,"epoch":"epoch","sequence":"18446744073709551615"}`
	if got := encodeCursor(current); got != base64.RawURLEncoding.EncodeToString([]byte(raw)) {
		// got содержит фактическую непрозрачную позицию события.
		t.Fatalf("изменены байты cursor: %s", got)
	}
	// decoded содержит прежнюю позицию; err сообщает отказ совместимого v1.
	decoded, err := decodeCursor(base64.RawURLEncoding.EncodeToString([]byte(raw)), current)
	if err != nil || decoded != current {
		t.Fatal("прежний cursor не читается", decoded, err)
	}
	// invalid нарушает прежние правила строкового sequence, версии либо набора полей.
	for _, invalid := range []string{
		`{"v":1,"epoch":"epoch","sequence":18446744073709551615}`,
		`{"v":1,"epoch":"epoch","sequence":"01"}`,
		`{"v":1,"epoch":"epoch","sequence":"18446744073709551616"}`,
		`{"v":2,"epoch":"epoch","sequence":"1"}`,
		`{"v":1,"epoch":"epoch","sequence":"1","extra":1}`,
	} {
		// err сообщает обязательный отказ недопустимого v1 cursor.
		if _, err := decodeCursor(base64.RawURLEncoding.EncodeToString([]byte(invalid)), current); err == nil {
			t.Fatalf("некорректный cursor принят: %s", invalid)
		}
	}
}

// TestPrivateNumericSchemaOwnsMutableChildren проверяет отдельное дерево Huma,
// включая AdditionalProperties, составные схемы, enum и сохранность публичных метаданных.
func TestPrivateNumericSchemaOwnsMutableChildren(t *testing.T) {
	// referencedValue задаёт схему с настоящей ссылкой registry на числовое свойство.
	type referencedValue struct {
		// N содержит беззнаковое целое в ссылочном компоненте.
		N Decimal `json:"n"`
	}
	// registry разрешает ссылки тестовой схемы в том же формате, что публичный API.
	registry := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	// reference содержит $ref, который частная проверка обязана полностью разыменовать.
	reference := registry.Schema(reflect.TypeFor[referencedValue](), true, "")
	// leaf задаёт точный числовой enum и изменяемые метаданные публичного дочернего узла.
	leaf := &huma.Schema{Type: "integer", Enum: []any{json.Number("9007199254740993")}, Extensions: map[string]any{"maximum": json.Number("18446744073709551615")}}
	// public связывает все проверяемые способы вложения дочерних схем.
	public := &huma.Schema{Type: "object", Properties: map[string]*huma.Schema{"n": leaf, "reference": reference}, AdditionalProperties: &huma.Schema{Type: "object", Required: []string{"n"}, Properties: map[string]*huma.Schema{"n": leaf}}, AnyOf: []*huma.Schema{{Type: "object", Properties: map[string]*huma.Schema{"n": leaf}}}, Required: []string{"n"}}
	public.PrecomputeMessages()
	// before фиксирует публичное дерево до создания частной проверки.
	before, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	// private содержит независимую проекцию; err сообщает отказ клонирования.
	private, err := privateBodySchema(registry, public)
	if err != nil {
		t.Fatal(err)
	}
	if private.Properties["reference"].Ref != "" || private.Properties["reference"].AllOf[0].Properties["n"].Type != "string" {
		t.Fatal("ссылка частной схемы вернулась к публичному числовому узлу", private.Properties["reference"])
	}
	// valid подтверждает согласованную проверку enum в обычном и дополнительном свойстве.
	valid := &huma.ValidateResult{}
	huma.Validate(registry, private, &huma.PathBuffer{}, huma.ModeWriteToServer, map[string]any{"n": "9007199254740993", "extra": map[string]any{"n": "9007199254740993"}}, valid)
	if len(valid.Errors) != 0 {
		t.Fatal("точная строковая проекция отвергнута", valid.Errors)
	}
	// invalid доказывает фактическое действие Required внутри AdditionalProperties,
	// которое стандартный обход PrecomputeMessages родительской схемы не подготавливает.
	invalid := &huma.ValidateResult{}
	huma.Validate(registry, private, &huma.PathBuffer{}, huma.ModeWriteToServer, map[string]any{"n": "9007199254740993", "extra": map[string]any{}}, invalid)
	if len(invalid.Errors) == 0 {
		t.Fatal("AdditionalProperties пропустил обязательное числовое поле")
	}
	// branch выбирает обычное свойство, AdditionalProperties либо составную схему.
	for _, branch := range []*huma.Schema{private.Properties["n"], private.AdditionalProperties.(*huma.Schema).Properties["n"], private.AnyOf[0].Properties["n"]} {
		if branch == leaf || branch.Type != "string" || branch.Enum[0] != "9007199254740993" || branch.Extensions["maximum"] != nil {
			t.Fatalf("неверная частная проекция: %#v", branch)
		}
		branch.Enum[0] = "changed"
		branch.Extensions["test"] = "private"
	}
	private.Required[0] = "changed"
	// after проверяет полную неизменность сериализуемого публичного дерева.
	after, err := json.Marshal(public)
	if err != nil || !bytes.Equal(before, after) || public.Properties["n"].Type != "integer" {
		t.Fatal("частная проверка изменила публичную схему", string(before), string(after), err)
	}
}

// TestNumericOutputMapsPreserveEveryCounter проверяет весь именованный набор
// счётчиков, потерь и общих медиаметрик на верхней границе uint64.
func TestNumericOutputMapsPreserveEveryCounter(t *testing.T) {
	// source задаёт синтетические измерения каждого поля без зависимостей от Publisher.
	source := core.OutputMetrics{
		QueuedPackets: math.MaxUint64, PacketBytes: math.MaxUint64, AccessUnits: math.MaxUint64, PendingPackets: math.MaxUint64, Generation: math.MaxUint64,
		InFlightPacketBytes: math.MaxUint64, InFlightConfigBytes: math.MaxUint64, WireBufferBytes: math.MaxUint64,
		ConfigBytes: math.MaxUint64, ConfigByteCapacity: math.MaxUint64, Attempts: math.MaxUint64, Connections: math.MaxUint64, Reconnects: math.MaxUint64,
		QueuePackets: math.MaxUint64, QueueCapacity: math.MaxUint64, QueueBytes: math.MaxUint64, QueueByteCapacity: math.MaxUint64,
		QueueDrops:    core.QueueDropMetrics{Overflow: math.MaxUint64, Oversized: math.MaxUint64, ConfigRejected: math.MaxUint64, AwaitingKeyframe: math.MaxUint64, AudioBeforeCut: math.MaxUint64, SourceReset: math.MaxUint64, ConfigReset: math.MaxUint64, Closed: math.MaxUint64},
		DeliveryDrops: core.DeliveryDropMetrics{Stale: math.MaxUint64, Transport: math.MaxUint64, Cancelled: math.MaxUint64, Terminal: math.MaxUint64},
		MediaMetrics:  core.MediaMetrics{VideoFrames: math.MaxUint64, AudioUnits: math.MaxUint64, PayloadBytes: math.MaxUint64, WrittenBytes: math.MaxUint64},
	}
	// raw содержит публичное JSON-представление всех трёх карт; err сообщает отказ сериализации.
	raw, err := json.Marshal(outputMetricsDTO(&source))
	if err != nil {
		t.Fatal(err)
	}
	// wire сохраняет RawMessage, чтобы строковое представление нельзя было спутать с числом.
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		// err сообщает некорректный JSON-пакет измерений.
		t.Fatal(err)
	}
	// family задаёт имя карты, keys содержит независимо перечисленные обязательные счётчики.
	for family, keys := range map[string][]string{
		"counters":       {"queued_packets", "packet_bytes", "access_units", "pending_packets", "generation", "in_flight_packet_bytes", "in_flight_config_bytes", "wire_buffer_bytes", "config_bytes", "config_byte_capacity", "attempts", "connections", "reconnects", "queue_packets", "queue_capacity", "queue_bytes", "queue_byte_capacity"},
		"queue_drops":    {"overflow", "oversized", "config_rejected", "awaiting_keyframe", "audio_before_cut", "source_reset", "config_reset", "closed"},
		"delivery_drops": {"stale", "transport", "cancelled", "terminal"},
		"media":          {"video_frames", "audio_units", "payload_bytes", "written_bytes"},
	} {
		// values содержит исходные числовые токены выбранной семьи.
		var values map[string]json.RawMessage
		if err := json.Unmarshal(wire[family], &values); err != nil {
			// err сообщает отсутствие или неверный тип обязательной карты.
			t.Fatal(family, err)
		}
		if family != "media" && len(values) != len(keys) {
			t.Fatalf("изменился набор %s: %s", family, wire[family])
		}
		// key задаёт проверяемый счётчик, который обязан сохранять все 64 бита.
		for _, key := range keys {
			if string(values[key]) != "18446744073709551615" {
				t.Fatalf("%s.%s: got=%s", family, key, values[key])
			}
		}
	}
	// duration проверяет максимальное окно time.Duration независимо от uint64-счётчиков.
	duration := rateDTO(core.Rate{Status: core.MetricAvailable, Duration: time.Duration(math.MaxInt64)})
	if uint64(duration.DurationNS) != uint64(math.MaxInt64) || duration.Value == nil || *duration.Value != 0 {
		t.Fatal("потеряны окно или измеренный ноль", duration)
	}
	// variant содержит ещё не готовый вариант, у которого VerifiedFormat нулевой.
	variant := variantDTO(core.FallbackVariant{})
	if variant.Format.Video.Width != 0 || variant.Format.Audio.SampleRateHz != 0 {
		t.Fatal("нулевой непроверенный формат изменён", variant)
	}
	// value задаёт отрицательный вещественный ноль; нормализация входа не должна менять его на выходе.
	value := math.Copysign(0, -1)
	// floatJSON содержит стандартный вывод float64, сохраняющий знак измеренного нуля.
	floatJSON, err := json.Marshal(rateDTO(core.Rate{Status: core.MetricAvailable, Value: value, Duration: time.Nanosecond}))
	if err != nil || !bytes.Contains(floatJSON, []byte(`"value":`+strconv.FormatFloat(value, 'g', -1, 64))) {
		t.Fatal("изменено вещественное представление", string(floatJSON), err)
	}
}
