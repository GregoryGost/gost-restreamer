package api_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// contractQualityRequest создаёт самостоятельный запрос x264 с точным качеством из token.
// t сообщает об ошибке исходного JSON; профиль high444 позволяет проверить явное качество ноль.
func contractQualityRequest(t *testing.T, token any) map[string]any {
	t.Helper()
	// profile содержит общие поля запроса; video, bitrate и encoding адресуют независимые части рецепта.
	profile := encodingRequestProfile(t, "transcode", "vbr")
	video := profile["format"].(map[string]any)["video"].(map[string]any)
	bitrate := profile["video_encoding"].(map[string]any)
	encoding := profile["encoding"].(map[string]any)
	video["codec_profile"], video["pixel_format"], video["bit_depth"], video["level"] = "high444", "yuv444p10le", 10, "auto"
	bitrate["target_bitrate_bps"], bitrate["max_bitrate_bps"], bitrate["buffer_bits"] = 0, 0, 0
	encoding["video_encoder"], encoding["rate_control"], encoding["crf"] = "libx264", "crf", token
	return profile
}

// TestProfileExactQualityHTTP проверяет исходные лексемы через настоящий HTTP и SQLite.
// Ожидаемые сотые заданы независимо от парсера; положительный ноль и -0 имеют один результат.
// t получает несовпадения публичного ответа и сохранённого доменного значения.
func TestProfileExactQualityHTTP(t *testing.T) {
	// fixture предоставляет изолированную SQLite и реальную регистрацию маршрутов.
	fixture := newAPIAcceptanceFixture(t, nil)
	// scenario задаёт исходную лексему, числовой ответ и независимо рассчитанное значение в сотых.
	for _, scenario := range []struct {
		// token передаётся без float64; wire задаёт каноническое число публичного ответа.
		token, wire string
		// hundredths задаёт точное ожидаемое значение в домене.
		hundredths int
	}{
		{"0", "0", 0}, {"-0", "0", 0}, {"-0.000e9", "0", 0},
		{"1e-2", "0.01", 1}, {"0.010000", "0.01", 1},
		{"2.345e1", "23.45", 2345}, {"234500e-4", "23.45", 2345},
		{"50.99", "50.99", 5099}, {"5100e-2", "51", 5100},
	} {
		// Подтест проверяет один числовой ввод и его сохранённый смысл; t получает точную причину отказа.
		t.Run(scenario.token, func(t *testing.T) {
			// created содержит ресурс; profile и encoding дают доступ к фактически опубликованным полям.
			created := encodingCreate(t, fixture, contractQualityRequest(t, json.Number(scenario.token)))
			profile := created["profile"].(map[string]any)
			encoding := profile["encoding"].(map[string]any)
			// qpPresent различает отсутствие неприменимого поля и его явный null.
			_, qpPresent := encoding["qp"]
			if encoding["crf"] != json.Number(scenario.wire) || qpPresent {
				t.Fatalf("Нарушен числовой ответ: %+v", encoding)
			}
			// stored и err проверяют реальные единицы хранения независимо от публичной обратной проекции.
			stored, err := fixture.service.GetProfile(context.Background(), core.ID(created["id"].(string)))
			if err != nil || stored.Profile.Encoding.CRF != (core.OptionalCRF{Set: true, ValueHundredths: scenario.hundredths}) {
				t.Fatalf("Качество изменилось при записи: %+v, %v", stored.Profile.Encoding.CRF, err)
			}
			// read сверяет отдельный запрос чтения с результатом создания, включая присутствие полей.
			read := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", "/transcode-profiles/"+created["id"].(string), "", 200))
			if !reflect.DeepEqual(read, created) {
				t.Fatal("Чтение изменило точный профиль", read)
			}
		})
	}
}

// TestProfileQualityRejectionIsAtomic защищает существующий ресурс от частичной записи PATCH.
// Неверные дроби выбраны по точному десятичному значению, включая округляющиеся к допустимому float64.
// t сообщает о неверном допуске или изменении существующего ресурса.
func TestProfileQualityRejectionIsAtomic(t *testing.T) {
	// fixture содержит отдельную БД; created и path задают единственный ресурс до всех отказов.
	fixture := newAPIAcceptanceFixture(t, nil)
	created := encodingCreate(t, fixture, contractQualityRequest(t, json.Number("23.45")))
	path := "/transcode-profiles/" + created["id"].(string)
	// token содержит математически недопустимую лексему, null либо строковый эквивалент числа.
	for _, token := range []any{
		nil, "23.45", json.Number("-0.01"), json.Number("-1e-1000"),
		json.Number("0.001"), json.Number("23.4500000000000001"), json.Number("51.000000000000001"),
		json.Number("1e" + strings.Repeat("9", 256)), json.Number("1e-" + strings.Repeat("9", 256)),
	} {
		// request сохраняет все корректные поля, меняя только спорное качество.
		request := contractQualityRequest(t, token)
		apiAcceptanceRequest(t, fixture.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": 1, "profile": request}), 422)
		// after сверяет не только профиль, но также имя, ревизию и временные поля ресурса.
		after := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", path, "", 200))
		if !reflect.DeepEqual(after, created) {
			t.Fatalf("Отклонённое качество изменило ресурс: %v", token)
		}
	}
	// missing проверяет отсутствие обязательного качества отдельно от явного нуля и null.
	missing := contractQualityRequest(t, json.Number("0"))
	delete(missing["encoding"].(map[string]any), "crf")
	apiAcceptanceRequest(t, fixture.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": 1, "profile": missing}), 422)
	if !reflect.DeepEqual(apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", path, "", 200)), created) {
		t.Fatal("Отсутствующее качество изменило ресурс")
	}
}
