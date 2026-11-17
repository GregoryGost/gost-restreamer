package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"go.yaml.in/yaml/v3"
)

// numericProfile задаёт полный текущий AV1-профиль в нативных границах кодировщика.
const numericProfile = `{"mode":"copy","format":{"video":{"codec":"av1","width":640,"height":360,"frame_rate":{"numerator":60000,"denominator":2002},"codec_profile":"main","level":"2.0","tier":"","pixel_format":"yuv420p","bit_depth":8,"color":{"range":"limited","primaries":"bt709","transfer":"bt709","matrix":"bt709"}},"audio":{"codec":"aac","codec_profile":"LC","sample_rate_hz":48000,"channels":2,"channel_layout":"stereo"}},"video_encoding":{"target_bitrate_bps":4000000,"max_bitrate_bps":4000000,"buffer_bits":8000000,"gop_frames":30,"max_b_frames":0,"closed_gop":true},"audio_encoding":{"bitrate_bps":128000},"encoding":null}`

// numericJSON разбирает raw без промежуточного float64 и требует ровно один JSON-объект.
func numericJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	// decoder сохраняет исходные числовые лексемы; value принимает объект результата.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	// err сообщает некорректный JSON или данные после первого объекта.
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatalf("данные после JSON-объекта: %v", err)
	}
	return value
}

// numericAt извлекает вложенное поле path из объекта value; t сообщает отсутствие
// ожидаемой ветви вместо паники приведения типа.
func numericAt(t *testing.T, value map[string]any, path string) any {
	t.Helper()
	// current содержит текущую ветвь объекта при последовательном обходе path.
	var current any = value
	// part задаёт имя очередного обязательного свойства.
	for part := range strings.SplitSeq(path, ".") {
		// object содержит текущий JSON-объект; ok подтверждает возможность обхода.
		object, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("%s: промежуточное значение не объект: %#v", path, current)
		}
		// present различает отсутствие поля и допустимый null.
		var present bool
		current, present = object[part]
		if !present {
			t.Fatalf("%s: отсутствует %s", path, part)
		}
	}
	return current
}

// numericWant проверяет числовой тип JSON и точную десятичную запись поля path.
func numericWant(t *testing.T, value map[string]any, path, want string) {
	t.Helper()
	// got сохраняет число без округления; ok исключает строку с теми же цифрами.
	got, ok := numericAt(t, value, path).(json.Number)
	if !ok || got.String() != want {
		t.Fatalf("%s: got=%#v (%T), want number %s", path, numericAt(t, value, path), numericAt(t, value, path), want)
	}
}

// TestNumericRESTNativeProfileRoundTrip проверяет через t POST, PATCH и GET с точными
// допустимыми числами и несокращённой дробью FPS через настоящий роутер.
func TestNumericRESTNativeProfileRoundTrip(t *testing.T) {
	// f содержит собственную временную SQLite и зарегистрированный API.
	f := newAPIAcceptanceFixture(t, nil)
	// response содержит результат создания полного текущего профиля.
	response := apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", `{"name":"Числовой профиль","profile":`+numericProfile+`}`, 201)
	// created сохраняет точные числовые токены ответа и строковый идентификатор.
	created := numericJSON(t, response.Body.Bytes())
	// path адресует созданный профиль для полного цикла PATCH и GET.
	path := "/transcode-profiles/" + created["id"].(string)
	// body проверяет математически целые записи uint32 и Decimal с точкой и степенью.
	body := strings.Replace(numericProfile, `"width":640`, `"width":64000e-2`, 1)
	body = strings.Replace(body, "128000", "1.28e5", 1)
	body = strings.Replace(body, `"mode":"copy"`, `"mode":"copy","live_audio_mode":"copy"`, 1)
	apiAcceptanceRequest(t, f.api, "PATCH", path, `{"expected_revision":1.0,"profile":`+body+`}`, 200)
	// wire содержит фактически сохранённый и повторно прочитанный публичный профиль.
	wire := numericJSON(t, apiAcceptanceRequest(t, f.api, "GET", path, "", 200).Body.Bytes())
	numericWant(t, wire, "revision", "2")
	// field задаёт публичный числовой путь; want содержит независимую границу типа.
	for field, want := range map[string]string{
		"profile.format.video.width": "640", "profile.format.video.height": "360",
		"profile.format.video.frame_rate.numerator": "60000", "profile.format.video.frame_rate.denominator": "2002",
		"profile.format.video.bit_depth": "8", "profile.format.audio.channels": "2",
		"profile.format.audio.sample_rate_hz": "48000", "profile.video_encoding.gop_frames": "30",
		"profile.video_encoding.max_b_frames": "0", "profile.video_encoding.target_bitrate_bps": "4000000",
		"profile.video_encoding.max_bitrate_bps": "4000000", "profile.video_encoding.buffer_bits": "8000000",
		"profile.audio_encoding.bitrate_bps": "128000",
	} {
		numericWant(t, wire, field, want)
	}
	if numericAt(t, wire, "profile.format.video.level") != "2.0" || numericAt(t, wire, "profile.encoding") == nil || numericAt(t, wire, "profile.video_encoding.closed_gop") != true {
		t.Fatal("изменена строка, null или false", wire)
	}
}

// TestNumericRESTInvalidTokensNeverWrite проверяет каждый физический размер,
// строгие типы и дроби. Отказ POST не оставляет профиль в настоящей SQLite.
// t сообщает об ошибках проверяемого сценария.
func TestNumericRESTInvalidTokensNeverWrite(t *testing.T) {
	// f содержит изолированное хранилище для проверки отсутствия записей после отказов.
	f := newAPIAcceptanceFixture(t, nil)
	// tc задаёт существующий токен old и неверную замену invalid в полном запросе.
	for _, tc := range []struct {
		// old адресует точное свойство; invalid содержит неверный тип или границу.
		old, invalid string
	}{
		{`"width":640`, `"width":4294967296`},
		{`"bit_depth":8`, `"bit_depth":256`},
		{`"channels":2`, `"channels":65536`},
		{`"width":640`, `"width":"4294967295"`},
		{`"width":640`, `"width":1.5`},
		{`"width":640`, `"width":-0.0`},
		{`"width":640`, `"width":true`},
		{`"width":640`, `"width":null`},
		{`"bitrate_bps":128000`, `"bitrate_bps":"9007199254740993"`},
		{`"bitrate_bps":128000`, `"bitrate_bps":18446744073709551616`},
		{`"bitrate_bps":128000`, `"bitrate_bps":9007199254740993.1`},
		{`"bitrate_bps":128000`, `"bitrate_bps":1e-1000000`},
		{`"bitrate_bps":128000`, `"bitrate_bps":1e1000000`},
		{`"bitrate_bps":128000`, `"bitrate_bps":[]`},
		{`"bitrate_bps":128000`, `"bitrate_bps":{}`},
		{`"bitrate_bps":128000`, `"bitrate_bps":null`},
		{`"bitrate_bps":128000`, `"bitrate_bps":-0e0`},
		{`"codec_profile":"main"`, `"codec_profile":1`},
		{`"closed_gop":true`, `"closed_gop":0`},
		{`"mode":"copy"`, `"mode":"unknown"`},
		{`"mode":"copy"`, `"mode":"copy","unknown_numeric":1`},
	} {
		// body сохраняет все остальные поля корректными, изолируя причину отказа.
		body := strings.Replace(numericProfile, tc.old, tc.invalid, 1)
		apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", `{"name":"Недопустимый профиль","profile":`+body+`}`, 422)
		// page проверяет хранилище независимо от HTTP-ответа; err сообщает отказ чтения.
		page, err := f.service.ListProfiles(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 10}})
		if err != nil || len(page.Items) != 0 {
			t.Fatalf("ошибочный запрос сохранил профиль: %s; %+v %v", tc.invalid, page, err)
		}
	}
}

// TestNumericRESTPolicyAndCAS сохраняет точные наносекунды через PUT, различает
// отсутствие/null и доказывает сохранность записи после неверной ревизии или длительности.
func TestNumericRESTPolicyAndCAS(t *testing.T) {
	// f содержит API и временное хранилище политики потока.
	f := newAPIAcceptanceFixture(t, nil)
	// stream содержит исходную запись; err сообщает отказ создания.
	stream, err := f.service.CreateStream(context.Background(), core.Stream{Name: "Поток", PublicationKey: "123456"})
	if err != nil {
		t.Fatal(err)
	}
	// asset содержит допустимую ссылку политики без загрузки файла.
	asset, err := f.service.CreateAsset(context.Background(), "Заглушка")
	if err != nil {
		t.Fatal(err)
	}
	// path адресует политику потока; policy содержит максимальную Duration и точность выше 2^53.
	path := "/streams/" + string(stream.ID) + "/fallback-policy"
	policy := fmt.Sprintf(`{"expected_revision":1e0,"fallback":{"asset_id":%q,"watchdog":{"video_timeout_ns":9223372036854775807,"audio_timeout_ns":9007199254740993,"recovery_window_ns":1.0,"keyframe_timeout_ns":1e+000000000000000000000000000}}}`, asset.ID)
	// wire содержит реальный ответ PUT policy с числовыми наносекундами и новой ревизией.
	wire := numericJSON(t, apiAcceptanceRequest(t, f.api, "PUT", path, policy, 200).Body.Bytes())
	numericWant(t, wire, "revision", "2")
	numericWant(t, wire, "fallback.watchdog.video_timeout_ns", "9223372036854775807")
	numericWant(t, wire, "fallback.watchdog.audio_timeout_ns", "9007199254740993")
	// before фиксирует сохранённый Stream до серии неверных PUT.
	before, err := f.service.GetStream(context.Background(), stream.ID)
	if err != nil {
		t.Fatal(err)
	}
	// token перебирает типы, неверные ревизии и границы uint64; нулевая ревизия запрещена доменом.
	for _, token := range []string{`"2"`, "0", "null", "true", "2.1", "18446744073709551616", "-0.0"} {
		apiAcceptanceRequest(t, f.api, "PUT", path, `{"expected_revision":`+token+`,"fallback":null}`, 422)
	}
	apiAcceptanceRequest(t, f.api, "PUT", path, `{"expected_revision":1,"fallback":null}`, 409)
	apiAcceptanceRequest(t, f.api, "PUT", path, `{"expected_revision":2}`, 422)
	// token задаёт неверную длительность при корректной текущей ревизии.
	for _, token := range []string{"0", "9223372036854775808", `"9223372036854775807"`, "1.5"} {
		apiAcceptanceRequest(t, f.api, "PUT", path, strings.Replace(strings.Replace(policy, `"expected_revision":1e0`, `"expected_revision":2`, 1), "9223372036854775807", token, 1), 422)
	}
	// after подтверждает отсутствие записи после всех отказов.
	after, err := f.service.GetStream(context.Background(), stream.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("отклонённые запросы изменили Stream", before, after, err)
	}
	apiAcceptanceRequest(t, f.api, "PUT", path, `{"expected_revision":2,"fallback":null}`, 200)
}

// TestNumericRESTEscapedProfileStrings проверяет экранированные ключи и повторные
// строковые поля рядом с точными числовыми токенами; Base64 и неверный UTF-8 отклоняются.
// t сообщает об ошибках проверяемого сценария.
func TestNumericRESTEscapedProfileStrings(t *testing.T) {
	// f содержит настоящий API для проверки полного пути декодирования запроса.
	f := newAPIAcceptanceFixture(t, nil)
	// body задаёт повторный экранированный ключ с последним допустимым значением уровня.
	body := strings.Replace(numericProfile, `"level":"2.0"`, `"level":"3.0","le\u0076el":"2.0"`, 1)
	// created содержит ответ профиля после обработки исходных числовых лексем.
	created := numericJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", `{"name":"Точный профиль","profile":`+body+`}`, 201).Body.Bytes())
	// saved содержит независимо прочитанный профиль; err сообщает отказ чтения.
	saved, err := f.service.GetProfile(context.Background(), core.ID(created["id"].(string)))
	if err != nil || saved.Profile.Format.Video.Level != "2.0" {
		t.Fatal("Изменён порядок повторных ключей", saved, err)
	}
	// invalid задаёт запрещённый тип, неизвестный текст или неверные UTF-8 байты уровня.
	for _, invalid := range []string{`{"encoding":"base64","value":"/3j+"}`, `"1e0"`, "\"" + string([]byte{0xff}) + "\""} {
		apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", `{"name":"Ошибка","profile":`+strings.Replace(numericProfile, `"level":"2.0"`, `"level":`+invalid, 1)+`}`, 422)
	}
}

// TestNumericRESTHighRevisionCAS использует полную точность CAS и проверяет отказ
// следующей ревизии при MaxUint64 без изменения долговечной записи.
func TestNumericRESTHighRevisionCAS(t *testing.T) {
	// tc задаёт начальную ревизию, запрос и ожидаемый статус изменения.
	for _, tc := range []struct {
		// revision содержит точную сохраняемую ревизию; token задаёт JSON expected_revision.
		revision uint64
		token    string
		// status задаёт успех либо запрет переполнения следующей ревизии.
		status int
	}{{9007199254740993, "9007199254740993", 200}, {18446744073709551615, "18446744073709551615", 409}} {
		// f содержит отдельное хранилище для каждой границы ревизии.
		f := newAPIAcceptanceFixture(t, nil)
		// now задаёт корректные UTC-метки прямой вставки прежней записи.
		now := time.Now().UTC().Truncate(time.Microsecond)
		// stream содержит запись с высокой ревизией, недостижимой короткой серией PATCH.
		stream := core.Stream{Record: core.Record{ID: "numeric-stream", Revision: tc.revision, CreatedAt: now, UpdatedAt: now}, Name: "Прежнее имя", PublicationKey: "12345"}
		// err сообщает отказ прямой вставки в изолированную SQLite; функция вставки не меняет другие ресурсы.
		if err := f.store.Write(context.Background(), func(r core.Repositories) error {
			return r.Config.InsertStream(context.Background(), stream)
		}); err != nil {
			t.Fatal(err)
		}
		apiAcceptanceRequest(t, f.api, "PATCH", "/streams/numeric-stream", `{"expected_revision":`+tc.token+`,"name":"Новое имя"}`, tc.status)
		// saved проверяет точную следующую ревизию либо полную неизменность при переполнении.
		saved, err := f.service.GetStream(context.Background(), stream.ID)
		if err != nil {
			t.Fatal(err)
		}
		if tc.status == 200 && (saved.Revision != 9007199254740994 || saved.Name != "Новое имя") || tc.status != 200 && !reflect.DeepEqual(saved, stream) {
			t.Fatal("нарушен точный CAS", saved, stream)
		}
	}
}

// TestNumericRESTEncodingIntegerForms проверяет int и числовое перечисление
// в реальном POST: целые записи со степенью, отрицательное особое значение, значения по умолчанию и явный ноль.
// t сообщает об ошибках проверяемого сценария.
func TestNumericRESTEncodingIntegerForms(t *testing.T) {
	// f содержит API с изолированными текущими профилями.
	f := newAPIAcceptanceFixture(t, nil)
	// profile предоставляет существующий допустимый AV1/VBR формат без медиапроцессов.
	profile := encodingRequestProfile(t, "copy", "vbr")
	// encoding задаёт точные записи числового перечисления и знаковых полей.
	encoding := profile["encoding"].(map[string]any)
	encoding["svt_av1"] = map[string]any{"preset": json.Number("0e0"), "lookahead_frames": json.Number("-1.0"), "aq_mode": json.Number("2e0")}
	// body сохраняет числовые лексемы при сериализации контрольного запроса.
	body := encodingJSON(t, map[string]any{"name": "Целые настройки", "profile": profile})
	// created содержит ответ POST с материализованными значениями по умолчанию и точными целыми.
	created := numericJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", body, 201).Body.Bytes())
	numericWant(t, created, "profile.encoding.svt_av1.preset", "0")
	numericWant(t, created, "profile.encoding.svt_av1.lookahead_frames", "-1")
	numericWant(t, created, "profile.encoding.svt_av1.aq_mode", "2")
	numericWant(t, created, "profile.video_encoding.max_bitrate_bps", "0")
	numericWant(t, created, "profile.video_encoding.buffer_bits", "0")
	// replacement изолирует неверный тип, дробь, null и границы int.
	for _, replacement := range []struct {
		// old выбирает корректное поле; invalid задаёт единственное нарушение запроса.
		old, invalid string
	}{
		{`"preset":0e0`, `"preset":"0"`}, {`"preset":0e0`, `"preset":null`},
		{`"preset":0e0`, `"preset":0.1`}, {`"preset":0e0`, `"preset":14`},
		{`"lookahead_frames":-1.0`, `"lookahead_frames":-9223372036854775809`},
		{`"lookahead_frames":-1.0`, `"lookahead_frames":9223372036854775808`},
		{`"aq_mode":2e0`, `"aq_mode":3e0`},
	} {
		apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", strings.Replace(body, replacement.old, replacement.invalid, 1), 422)
	}
}

// TestNumericOpenAPIExactBounds сверяет OpenAPI, отдельные JSON Schema и YAML
// с физическими типами, сохраняя нулевой формат до готовности варианта.
func TestNumericOpenAPIExactBounds(t *testing.T) {
	// f содержит API, замораживающий спецификацию при регистрации.
	f := newAPIAcceptanceFixture(t, nil)
	// raw содержит первую неизменяемую JSON-спецификацию; spec разбирается без float64.
	raw := apiAcceptanceRequest(t, f.api, "GET", "/openapi.json", "", 200).Body.Bytes()
	spec := numericJSON(t, raw)
	// schemas содержит опубликованные компоненты схем.
	schemas := numericAt(t, spec, "components.schemas").(map[string]any)
	// tc перечисляет независимые физические границы публичных типов.
	for _, tc := range []struct {
		// schema выбирает компонент; field выбирает его числовое свойство; maximum задаёт точный предел.
		schema, field, maximum string
	}{
		{"StreamDTO", "revision", "18446744073709551615"},
		{"VideoFormatDTO", "width", "4294967295"}, {"VideoFormatDTO", "height", "4294967295"},
		{"VideoFormatDTO", "bit_depth", "255"}, {"AudioFormatDTO", "channels", "65535"},
		{"AudioFormatDTO", "sample_rate_hz", "4294967295"},
		{"FrameRateDTO", "numerator", "4294967295"}, {"FrameRateDTO", "denominator", "4294967295"},
		{"VideoEncodingDTO", "max_b_frames", "4294967295"}, {"VideoEncodingDTO", "buffer_bits", "18446744073709551615"},
		{"AssetDTO", "size_bytes", "18446744073709551615"}, {"VariantDTO", "size_bytes", "18446744073709551615"},
	} {
		// property содержит публичную числовую схему без частной строковой проекции.
		property := numericAt(t, schemas, tc.schema+".properties."+tc.field).(map[string]any)
		if property["type"] != "integer" || property["format"] == "int32" && tc.maximum == "4294967295" || property["format"] == "int64" && tc.maximum == "18446744073709551615" {
			t.Fatalf("неверный тип %s.%s: %#v", tc.schema, tc.field, property)
		}
		numericWant(t, property, "maximum", tc.maximum)
		// individual проверяет тот же точный предел после переписывания ссылок отдельной схемы.
		individual := numericJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/schemas/"+tc.schema+".json", "", 200).Body.Bytes())
		numericWant(t, individual, "properties."+tc.field+".maximum", tc.maximum)
	}
	// yamlSpec содержит исходные скалярные токены YAML без преобразования больших чисел в float64.
	var yamlSpec yaml.Node
	// err сообщает отказ разбора опубликованного YAML.
	if err := yaml.Unmarshal(apiAcceptanceRequest(t, f.api, "GET", "/openapi.yaml", "", 200).Body.Bytes(), &yamlSpec); err != nil {
		t.Fatal(err)
	}
	// node проходит путь YAML к максимальной ревизии, сохраняя её текст и тег целочисленного типа.
	node := yamlSpec.Content[0]
	// key задаёт очередное имя отображения YAML.
	for _, key := range []string{"components", "schemas", "StreamDTO", "properties", "revision", "maximum"} {
		// next содержит найденное значение key либо nil при пропущенном свойстве.
		var next *yaml.Node
		// index перебирает пары ключ/значение отображения YAML.
		for index := 0; index+1 < len(node.Content); index += 2 {
			if node.Content[index].Value == key {
				next = node.Content[index+1]
				break
			}
		}
		if next == nil {
			t.Fatalf("YAML не содержит %s", key)
		}
		node = next
	}
	if node.Value != "18446744073709551615" || node.Tag != "!!int" {
		t.Fatalf("YAML округляет или цитирует uint64: %#v", node)
	}
	if schemas["RuntimeEventDTO"] == nil || schemas["ResyncRequiredDTO"] == nil {
		t.Fatal("схемы JSON data событий удалены из OpenAPI", schemas["RuntimeEventDTO"], schemas["ResyncRequiredDTO"])
	}
	// events связывает каждое публичное имя SSE с разрешимой схемой его JSON-данных.
	events := numericAt(t, spec, "paths."+api.Prefix+"/events.get.x-sse-events").(map[string]any)
	// name задаёт обычное событие либо управляющий resync_required.
	for _, name := range []string{"state", "metrics", "input", "gpu", "removed", "attempt", "resync_required"} {
		// ref содержит публичную ссылку, сохраняющую компонент данных при удалении неиспользуемых схем.
		ref, ok := numericAt(t, events, name+".$ref").(string)
		if !ok || !strings.HasPrefix(ref, "#/components/schemas/") || schemas[strings.TrimPrefix(ref, "#/components/schemas/")] == nil {
			t.Fatalf("событие %s потеряло схему данных: %#v", name, events[name])
		}
	}
	if !bytes.Equal(raw, apiAcceptanceRequest(t, f.api, "GET", "/openapi.json", "", 200).Body.Bytes()) {
		t.Fatal("чтение схем изменило замороженную спецификацию")
	}
}

// TestNumericOpenAPIFamilies проверяет типы всех числовых семейств, nullable,
// беззнаковые значения словарей и более узкие ограничения настроек без изменения физических границ.
func TestNumericOpenAPIFamilies(t *testing.T) {
	// f предоставляет настоящий документ OpenAPI после полной регистрации.
	f := newAPIAcceptanceFixture(t, nil)
	// schemas содержит компоненты, разобранные без округления numeric metadata.
	schemas := numericAt(t, numericJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/openapi.json", "", 200).Body.Bytes()), "components.schemas").(map[string]any)
	// tc задаёт независимо ожидаемые integer/number и применимые точные ограничения.
	for _, tc := range []struct {
		// component и path адресуют схему свойства либо additionalProperties словаря.
		component, path string
		// kind задаёт integer либо number; nullable требует явную альтернативу null.
		kind     string
		nullable bool
		// minimum и maximum задают проверяемые границы, пустота означает отсутствие отдельного ожидания.
		minimum, maximum string
	}{
		{"StreamPatchBody", "properties.expected_revision", "integer", false, "0", "18446744073709551615"},
		{"ProfilePatchBody", "properties.expected_revision", "integer", false, "0", "18446744073709551615"},
		{"GroupPatchBody", "properties.expected_revision", "integer", false, "0", "18446744073709551615"},
		{"DestinationPatchBody", "properties.expected_revision", "integer", false, "0", "18446744073709551615"},
		{"PolicyResult", "properties.revision", "integer", false, "0", "18446744073709551615"},
		{"GenerationDTO", "properties.size_bytes", "integer", false, "0", "18446744073709551615"},
		{"ObservationDTO", "properties.revision", "integer", false, "0", "18446744073709551615"},
		{"RuntimeGroupDTO", "properties.source_revision", "integer", false, "0", "18446744073709551615"},
		{"RuntimeEventDTO", "properties.source_revision", "integer", false, "0", "18446744073709551615"},
		{"InputMetricsDTO", "properties.source_epoch", "integer", false, "0", "18446744073709551615"},
		{"MediaMetricsDTO", "properties.video_frames", "integer", false, "0", "18446744073709551615"},
		{"MediaMetricsDTO", "properties.audio_units", "integer", false, "0", "18446744073709551615"},
		{"MediaMetricsDTO", "properties.payload_bytes", "integer", false, "0", "18446744073709551615"},
		{"MediaMetricsDTO", "properties.written_bytes", "integer", false, "0", "18446744073709551615"},
		{"OutputMetricsDTO", "properties.counters.additionalProperties", "integer", false, "0", "18446744073709551615"},
		{"OutputMetricsDTO", "properties.queue_drops.additionalProperties", "integer", false, "0", "18446744073709551615"},
		{"OutputMetricsDTO", "properties.delivery_drops.additionalProperties", "integer", false, "0", "18446744073709551615"},
		{"OutputMetricsDTO", "properties.ack_ahead_events_total", "integer", false, "0", "18446744073709551615"},
		{"OutputMetricsDTO", "properties.ack_ahead_bytes_last", "integer", false, "0", "18446744073709551615"},
		{"OutputMetricsDTO", "properties.ack_ahead_bytes_max", "integer", false, "0", "18446744073709551615"},
		{"FFmpegMetricsDTO", "properties.progress_losses", "integer", false, "0", "18446744073709551615"},
		{"FFmpegMetricsDTO", "properties.diagnostic_losses", "integer", false, "0", "18446744073709551615"},
		{"FFmpegProgressDTO", "properties.frame", "integer", true, "0", "18446744073709551615"},
		{"FFmpegProgressDTO", "properties.total_size_bytes", "integer", true, "0", "18446744073709551615"},
		{"FFmpegProgressDTO", "properties.dup_frames", "integer", true, "0", "18446744073709551615"},
		{"FFmpegProgressDTO", "properties.drop_frames", "integer", true, "0", "18446744073709551615"},
		{"FFmpegProgressDTO", "properties.out_time_us", "integer", true, "-9223372036854775808", "9223372036854775807"},
		{"FFmpegProgressDTO", "properties.fps", "number", true, "0", ""},
		{"FFmpegProgressDTO", "properties.bitrate_bps", "number", true, "0", ""},
		{"FFmpegProgressDTO", "properties.speed", "number", true, "0", ""},
		{"RateDTO", "properties.value", "number", true, "0", ""},
		{"RateDTO", "properties.duration_ns", "integer", false, "0", "9223372036854775807"},
		{"GPUMetricDTO", "properties.value", "integer", true, "0", "18446744073709551615"},
		{"GPUMetricDTO", "properties.sampling_period_us", "integer", false, "0", "18446744073709551615"},
		{"GPUDTO", "properties.stale_after_ns", "integer", false, "1", "9223372036854775807"},
		{"WatchdogDTO", "properties.video_timeout_ns", "integer", false, "1", "9223372036854775807"},
		{"WatchdogDTO", "properties.audio_timeout_ns", "integer", false, "1", "9223372036854775807"},
		{"WatchdogDTO", "properties.recovery_window_ns", "integer", false, "1", "9223372036854775807"},
		{"WatchdogDTO", "properties.keyframe_timeout_ns", "integer", false, "1", "9223372036854775807"},
		{"EncodingInput", "properties.nvenc.properties.lookahead_frames", "integer", false, "0", "31"},
		{"EncodingInput", "properties.nvenc.properties.aq_strength", "integer", false, "1", "15"},
		{"EncodingInput", "properties.svt_av1.properties.preset", "integer", false, "0", "13"},
		{"EncodingInput", "properties.svt_av1.properties.lookahead_frames", "integer", false, "-1", "120"},
	} {
		// property содержит проверяемый числовой узел публичной схемы.
		property := numericAt(t, schemas, tc.component+"."+tc.path).(map[string]any)
		if tc.nullable {
			if !reflect.DeepEqual(property["type"], []any{tc.kind, "null"}) {
				t.Fatalf("%s.%s: потерян nullable %s: %#v", tc.component, tc.path, tc.kind, property)
			}
		} else if property["type"] != tc.kind {
			t.Fatalf("%s.%s: неверный тип %s: %#v", tc.component, tc.path, tc.kind, property)
		}
		if tc.minimum != "" {
			numericWant(t, property, "minimum", tc.minimum)
		}
		if tc.maximum != "" {
			numericWant(t, property, "maximum", tc.maximum)
		}
	}
}
