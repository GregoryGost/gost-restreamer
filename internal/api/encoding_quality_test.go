package api_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// qualityRequestProfile создаёт через t полный AV1-запрос с обязательным качеством SVT для rc.
func qualityRequestProfile(t *testing.T, rc string) map[string]any {
	t.Helper()
	// p содержит допустимый AV1 copy-профиль до выбора quality-рецепта.
	p := encodingRequestProfile(t, "copy", "vbr")
	// encoding указывает на настройки для выбора RC и обязательного качества.
	encoding := p["encoding"].(map[string]any)
	encoding["rate_control"] = rc
	if rc == "cqp" {
		encoding["qp"] = 35
	} else {
		encoding["crf"] = 35
	}
	// video содержит независимые параметры битрейта и GOP.
	video := p["video_encoding"].(map[string]any)
	video["target_bitrate_bps"] = 0
	return p
}

// TestQualityEncodingHTTPRoundTrip проверяет материализацию CQP/CRF, числовое качество и отсутствие неприменимого поля.
// t сообщает об ошибках проверяемого сценария.
func TestQualityEncodingHTTPRoundTrip(t *testing.T) {
	// f содержит реальный API и отдельную временную SQLite.
	f := newAPIAcceptanceFixture(t, nil)
	// rc выбирает проверяемый режим качества.
	for _, rc := range []string{"cqp", "crf"} {
		// request содержит полный входной профиль выбранного режима.
		request := qualityRequestProfile(t, rc)
		// created содержит сохранённый ответ HTTP.
		created := encodingCreate(t, f, request)
		// profile содержит возвращённый материализованный профиль; encoding — его рецепт.
		profile := created["profile"].(map[string]any)
		encoding := profile["encoding"].(map[string]any)
		// field выбирает публичное имя качества, отличающееся от имени CQP режима.
		field := "crf"
		if rc == "cqp" {
			field = "qp"
		}
		if encoding[field] != json.Number("35") || profile["live_audio_mode"] != "copy" {
			t.Fatal(created)
		}
		// duplicate содержит новый профиль из полного публичного ответа.
		duplicate := encodingCreate(t, f, profile)
		if encodingJSON(t, duplicate["profile"]) != encodingJSON(t, profile) {
			t.Fatal("Потеряно качество", duplicate)
		}
	}
}

// TestQualityEncodingHTTPRejectsExactFraction проверяет исходную лексему CRF до округления float64 и запреты присутствия.
func TestQualityEncodingHTTPRejectsExactFraction(t *testing.T) {
	// f содержит изолированный настоящий API для проверки отказов.
	f := newAPIAcceptanceFixture(t, nil)
	// token задаёт числовую либо неверно типизированную лексему quality.
	for _, token := range []string{"1.0000000000000001", "35.000000000000001", "1.5", "0", "64", "null", `"35"`} {
		// request содержит допустимый CRF-запрос с заменяемой точной лексемой.
		request := qualityRequestProfile(t, "crf")
		// body содержит точный wire JSON, сохраняя дробные разряды.
		body := encodingJSON(t, map[string]any{"name": "Качество", "profile": request})
		body = strings.Replace(body, `"crf":35`, `"crf":`+token, 1)
		apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", body, 422)
	}
	// rc выбирает quality-рецепт, из которого удаляется обязательное поле.
	for _, rc := range []string{"cqp", "crf"} {
		// request содержит отдельную допустимую исходную форму.
		request := qualityRequestProfile(t, rc)
		// field выбирает имя обязательного quality-поля.
		field := "crf"
		if rc == "cqp" {
			field = "qp"
		}
		delete(request["encoding"].(map[string]any), field)
		apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Качество", "profile": request}), 422)
	}
}
