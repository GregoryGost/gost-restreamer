package api_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

// TestMediaExpansionTuningCompatibility проверяет доступность новых настроек tuning в CRF
// и отсутствие молчаливой подстановки при CBR/VBR/CQP либо неизвестном имени.
// t сообщает об ошибках проверяемого сценария.
func TestMediaExpansionTuningCompatibility(t *testing.T) {
	// f предоставляет настоящий HTTP и отдельную SQLite сценария.
	f := newAPIAcceptanceFixture(t, nil)
	// tuning выбирает каждую разрешённую новую настройку SVT.
	for _, tuning := range []string{"ssim", "ms_ssim", "vmaf"} {
		// p содержит полноценный CRF35; e адресует новый блок encoding.
		p := qualityRequest(t, "crf", json.Number("35"))
		e := p["encoding"].(map[string]any)
		e["svt_av1"] = map[string]any{"tuning": tuning}
		// created содержит материализованный ответ для проверки сохранения имени.
		created := encodingCreate(t, f, p)
		if created["profile"].(map[string]any)["encoding"].(map[string]any)["svt_av1"].(map[string]any)["tuning"] != tuning {
			t.Fatal("Tuning молча подменён", created)
		}
		// rc задаёт три режима, для которых новые tuning явно запрещены.
		for _, rc := range []string{"cqp", "cbr", "vbr"} {
			p = encodingRequestProfile(t, "copy", rc)
			if rc == "cqp" {
				p = qualityRequest(t, rc, json.Number("35"))
			}
			e = p["encoding"].(map[string]any)
			e["svt_av1"] = map[string]any{"tuning": tuning}
			apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Несовместимое качество", "profile": p}), 422)
		}
	}
	// tuning проверяет отложенный iq и неизвестное значение без разрешённой подстановки.
	for _, tuning := range []string{"iq", "future"} {
		// p содержит CRF35 с единственным неподдержанным tuning.
		p := qualityRequest(t, "crf", 35)
		p["encoding"].(map[string]any)["svt_av1"] = map[string]any{"tuning": tuning}
		apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Неизвестное качество", "profile": p}), 422)
	}
}

// TestMediaExpansionSchemaPreservesQualityTypes проверяет через t реальные публичные схемы:
// QP/CRF остаются необязательными числами; селектора версии возможностей нет.
func TestMediaExpansionSchemaPreservesQualityTypes(t *testing.T) {
	// f предоставляет рабочую регистрацию Huma; spec содержит OpenAPI, schemas — его компоненты.
	f := newAPIAcceptanceFixture(t, nil)
	spec := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/openapi.json", "", 200))
	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
	// name выбирает request и response, которым требуется согласованное числовое представление.
	for _, name := range []string{"EncodingInput", "EncodingDTO"} {
		// schema содержит проверяемую модель; properties — её публичные свойства.
		schema := schemas[name].(map[string]any)
		properties := schema["properties"].(map[string]any)
		if properties["qp"].(map[string]any)["type"] != "integer" || properties["crf"].(map[string]any)["type"] != "number" {
			t.Fatal("Изменён числовой контракт", name, properties)
		}
		// required содержит обязательные свойства, которым нельзя требовать quality в CBR/VBR.
		required := schema["required"].([]any)
		if slices.Contains(required, any("qp")) || slices.Contains(required, any("crf")) {
			t.Fatal("Quality навязано прежним рецептам", name)
		}
		if properties["version"] != nil {
			t.Fatal("Схема публикует удалённый селектор возможностей", properties["version"])
		}
	}
}

// qualityRequest создаёт полный запрос AV1 с чистым управлением качеством.
// t получает ошибки фикстуры, rc выбирает cqp/crf, quality сохраняет точный JSON-токен.
func qualityRequest(t *testing.T, rc string, quality any) map[string]any {
	t.Helper()
	// p содержит самостоятельный профиль; e — его запрос кодировщика; ve — общие параметры видео.
	p := encodingRequestProfile(t, "copy", "vbr")
	e := p["encoding"].(map[string]any)
	ve := p["video_encoding"].(map[string]any)
	e["rate_control"] = rc
	ve["target_bitrate_bps"], ve["max_bitrate_bps"], ve["buffer_bits"] = 0, 0, 0
	if rc == "cqp" {
		e["qp"] = quality
	} else {
		e["crf"] = quality
	}
	return p
}

// TestMediaExpansionQualityHTTPRoundTrip проверяет сохранение чисел, применимость полей,
// независимые исходные значения AQ и lookahead и точный цикл GET→POST.
// t сообщает об ошибках проверяемого сценария.
func TestMediaExpansionQualityHTTPRoundTrip(t *testing.T) {
	// rc выбирает один из двух новых режимов качества.
	for _, rc := range []string{"cqp", "crf"} {
		// value выбирает нижнюю и верхнюю допустимые границы SVT.
		for _, value := range []json.Number{"1", "63"} {
			// t сообщает об ошибках проверяемого сценария.
			t.Run(rc+"/"+string(value), func(t *testing.T) {
				// Проверяет границу rc/value через настоящий HTTP и временную SQLite.
				// f изолирует хранилище; p содержит исходный запрос.
				f := newAPIAcceptanceFixture(t, nil)
				p := qualityRequest(t, rc, value)
				// created содержит сохранённый ресурс; profile — возвращённые параметры; e — encoding.
				created := encodingCreate(t, f, p)
				profile := created["profile"].(map[string]any)
				e := profile["encoding"].(map[string]any)
				// field выбирает применимое качество; other обозначает запрещённое соседнее поле.
				field, other := "crf", "qp"
				// aq задаёт контрактный режим адаптивного квантования.
				aq := json.Number("2")
				if rc == "cqp" {
					field, other, aq = "qp", "crf", json.Number("0")
				}
				// otherPresent различает отсутствие неприменимого качества и его явный null.
				_, otherPresent := e[other]
				if e[field] != value || otherPresent || profile["live_audio_mode"] != "copy" {
					t.Fatalf("Искажены версия/качество/присутствие/режим аудио: %+v", profile)
				}
				// svt содержит материализованные настройки SVT, проверяемые независимо от построителя команды.
				svt := e["svt_av1"].(map[string]any)
				if svt["aq_mode"] != aq || svt["lookahead_frames"] != json.Number("-1") || svt["tuning"] != "psnr" {
					t.Fatalf("Неверные defaults режима качества: %+v", svt)
				}
				// path адресует созданный ресурс; got содержит его независимое чтение.
				path := "/transcode-profiles/" + created["id"].(string)
				got := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", path, "", 200))
				if !reflect.DeepEqual(got, created) || !reflect.DeepEqual(encodingCreate(t, f, profile)["profile"], profile) {
					t.Fatal("Изменён GET→POST")
				}

			})
		}
	}
}

// TestMediaExpansionQualityRejectsInvalidPresenceAndExactFractions различает отсутствующее
// качество, null, ноль и точные дроби, включая значение, округляющееся float64 до единицы.
func TestMediaExpansionQualityRejectsInvalidPresenceAndExactFractions(t *testing.T) {
	// f использует одну изолированную БД для запросов, которые не должны создавать ресурсов.
	f := newAPIAcceptanceFixture(t, nil)
	// rc выбирает качество с отдельной обязательностью и типом.
	for _, rc := range []string{"cqp", "crf"} {
		// bad задаёт недопустимый JSON-токен, не проходящий контракт SVT 1..63.
		for _, bad := range []any{nil, "35", json.Number("0"), json.Number("64"), json.Number("-1"), json.Number("1.5"), json.Number("1.0000000000000001")} {
			// p содержит запрос с единственным ошибочным качеством.
			p := qualityRequest(t, rc, bad)
			apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка качества", "profile": p}), 422)
		}
		// p содержит корректный запрос до удаления или добавления неприменимого поля; e — его encoding.
		p := qualityRequest(t, rc, json.Number("35"))
		e := p["encoding"].(map[string]any)
		delete(e, "qp")
		delete(e, "crf")
		apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Без качества", "profile": p}), 422)
		e["qp"], e["crf"] = 35, 35
		apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Два качества", "profile": p}), 422)
	}
}

// TestMediaExpansionAACLayouts проверяет через t шесть канонических пар AAC в обоих
// видеорежимах с одним текущим набором настроек.
func TestMediaExpansionAACLayouts(t *testing.T) {
	// f предоставляет настоящий API с временной БД.
	f := newAPIAcceptanceFixture(t, nil)
	// layout и channels задают независимые контрактные пары раскладки и числа каналов.
	for layout, channels := range map[string]int{"2.1": 3, "3.0": 3, "4.0": 4, "5.0": 5, "5.1": 6, "7.1": 8} {
		// mode выбирает copy/AV1 либо transcode/H264.
		for _, mode := range []string{"copy", "transcode"} {
			// p содержит новый профиль; audio — его аудиоформат; e — encoding.
			p := encodingRequestProfile(t, mode, "vbr")
			audio := p["format"].(map[string]any)["audio"].(map[string]any)
			audio["channels"], audio["channel_layout"] = channels, layout
			// created содержит прочитанный ответ на создание проверяемой раскладки.
			created := encodingCreate(t, f, p)
			if !reflect.DeepEqual(created["profile"].(map[string]any)["format"].(map[string]any)["audio"], map[string]any{"codec": "aac", "codec_profile": "LC", "sample_rate_hz": json.Number("48000"), "channels": json.Number(encodingJSON(t, channels)), "channel_layout": layout}) {
				t.Fatal("Раскладка изменилась", created)
			}

		}
	}
}
