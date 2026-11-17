package api_test

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// hdrRequest строит полный публичный запрос нового фиксированного кодировщика encoder.
// mode определяет LIVE-видео, rc — битрейт либо качество; hdr включает известные 10-битные PQ-пиксели.
// t сообщает об ошибках проверяемого сценария.
func hdrRequest(t *testing.T, encoder, mode, rc string, hdr bool) map[string]any {
	t.Helper()
	// p использует независимые битрейтные поля H264-фикстуры; e задаёт выбранный кодировщик.
	p := encodingRequestProfile(t, "transcode", rc)
	e := p["encoding"].(map[string]any)
	// video адресует полную целевую дорожку; ve содержит битрейты и ограничения GOP.
	video := p["format"].(map[string]any)["video"].(map[string]any)
	ve := p["video_encoding"].(map[string]any)
	p["mode"] = mode
	e["video_encoder"] = encoder
	video["codec"], video["codec_profile"], video["level"] = "hevc", "main", "auto"
	ve["max_b_frames"] = 0
	if encoder == "libvpx-vp9" {
		video["codec"], video["codec_profile"] = "vp9", "profile0"
		if rc == "vbr" {
			ve["max_bitrate_bps"], ve["buffer_bits"] = 0, 0
		}
	}
	if hdr {
		video["codec_profile"], video["pixel_format"], video["bit_depth"] = "main10", "yuv420p10le", 10
		if encoder == "libvpx-vp9" {
			video["codec_profile"] = "profile2"
		}
		video["color"] = map[string]any{"range": "limited", "primaries": "bt2020", "transfer": "smpte2084", "matrix": "bt2020nc"}
	}
	if rc == "crf" || rc == "cqp" {
		ve["target_bitrate_bps"], ve["max_bitrate_bps"], ve["buffer_bits"] = 0, 0, 0
		if rc == "crf" {
			e["crf"] = json.Number("23")
		} else {
			e["qp"] = json.Number("0")
		}
	}
	return p
}

// TestHDRFixedEncoderHTTPReadback проверяет POST/GET/POST реальных маршрутов,
// точное качество CRF с нулём и сотыми долями, отсутствие неприменимых блоков и сохранность SDR/PQ/HLG-параметров.
// t сообщает об ошибках проверяемого сценария.
func TestHDRFixedEncoderHTTPReadback(t *testing.T) {
	// f содержит настоящий API с временной файловой SQLite, без медиапроцессов.
	f := newAPIAcceptanceFixture(t, nil)
	// scenario задаёт одну применимую пару кодировщика, RC и точной числовой лексемы качества.
	for _, scenario := range []struct {
		// encoder выбирает фиксированный рецепт; rc различает QP и CRF; token сохраняет исходное число.
		encoder, rc, token string
	}{
		{"libx265", "crf", "2351e-2"}, {"libx265", "crf", "0"}, {"libx265", "crf", "5100e-2"},
		{"libx265", "cqp", "0"}, {"hevc_nvenc", "cqp", "51"},
		{"libvpx-vp9", "crf", "6300e-2"}, {"libvpx-vp9", "crf", "0"},
	} {
		// mode проверяет обе разрешённые обработки LIVE без неявной смены encoder.
		for _, mode := range []string{"copy", "transcode"} {
			// p содержит независимый полный HDR-запрос; e адресует точное качество.
			p := hdrRequest(t, scenario.encoder, mode, scenario.rc, true)
			e := p["encoding"].(map[string]any)
			// qualityField выбирает реальное публичное имя поля, поскольку cqp называется qp.
			qualityField := "qp"
			if scenario.rc == "crf" {
				qualityField = "crf"
			}
			e[qualityField] = json.Number(scenario.token)
			// created содержит сохранённый ресурс; profile и recipe выделяют материализованный ответ.
			created := encodingCreate(t, f, p)
			profile := created["profile"].(map[string]any)
			recipe := profile["encoding"].(map[string]any)
			if recipe["nvenc"] != nil || recipe["svt_av1"] != nil || recipe["rescale_output"] != (mode == "transcode") {
				t.Fatal("Изменён фиксированный рецепт", recipe)
			}
			// stored проверяет точную доменную запись после реальной HTTP/SQLite-границы.
			// err сообщает об отказе чтения сохранённого профиля после HTTP-запроса.
			stored, err := f.service.GetProfile(context.Background(), core.ID(created["id"].(string)))
			if err != nil || stored.Profile.Encoding.VideoEncoder != scenario.encoder {
				t.Fatal("Потеряна явная версия", stored, err)
			}
			if scenario.token == "2351e-2" && (recipe["crf"] != json.Number("23.51") || stored.Profile.Encoding.CRF.ValueHundredths != 2351) {
				t.Fatal("Потеряна сотая CRF", recipe)
			}
			// read содержит отдельный GET, duplicate — новый ресурс из полного возвращённого профиля.
			read := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/transcode-profiles/"+created["id"].(string), "", 200))
			duplicate := encodingCreate(t, f, profile)
			if !reflect.DeepEqual(read, created) || !reflect.DeepEqual(duplicate["profile"], profile) {
				t.Fatal("Публичный цикл изменил профиль", read, duplicate)
			}
		}
	}
	// encoder выбирает каждый новый кодировщик с SDR и HLG для проверки CBR/VBR.
	for _, encoder := range []string{"libx265", "hevc_nvenc", "libvpx-vp9"} {
		// rc перечисляет битрейтные рецепты без полей качества.
		for _, rc := range []string{"cbr", "vbr"} {
			// hdr отделяет SDR от HLG; p содержит отдельный запрос для выбранного цвета.
			for _, hdr := range []bool{false, true} {
				p := hdrRequest(t, encoder, "transcode", rc, hdr)
				if hdr {
					p["format"].(map[string]any)["video"].(map[string]any)["color"].(map[string]any)["transfer"] = "arib-std-b67"
				}
				encodingCreate(t, f, p)
			}
		}
	}
}

// TestHDRRequestRejectionIsAtomic проверяет согласованность нативных настроек, обязательность полей и запрет null,
// запрет чужих блоков и сохранность полного ресурса после ошибочного PATCH.
// t сообщает об ошибках проверяемого сценария.
func TestHDRRequestRejectionIsAtomic(t *testing.T) {
	// f содержит настоящий API; created и path фиксируют ресурс до каждой отвергнутой замены.
	f := newAPIAcceptanceFixture(t, nil)
	created := encodingCreate(t, f, hdrRequest(t, "libx265", "transcode", "crf", true))
	path := "/transcode-profiles/" + created["id"].(string)
	// field выбирает конкретное нарушение новой границы запроса.
	for _, field := range []string{"null", "absent", "string", "fraction", "range", "nvenc", "svt_av1", "version", "profile", "color", "depth", "vbv", "custom"} {
		// p содержит новый независимый допустимый запрос; e и video адресуют изменяемые поля.
		p := hdrRequest(t, "libx265", "transcode", "crf", true)
		e := p["encoding"].(map[string]any)
		video := p["format"].(map[string]any)["video"].(map[string]any)
		switch field {
		case "null":
			e["crf"] = nil
		case "absent":
			delete(e, "crf")
		case "string":
			e["crf"] = "23"
		case "fraction":
			e["crf"] = json.Number("23.000000000000001")
		case "range":
			e["crf"] = json.Number("51.01")
		case "nvenc", "svt_av1":
			e[field] = map[string]any{}
		case "version":
			e["version"] = 5
		case "profile":
			video["codec_profile"] = "main"
		case "color":
			video["color"].(map[string]any)["matrix"] = "bt709"
		case "depth":
			video["bit_depth"] = 8
		case "vbv":
			p["video_encoding"].(map[string]any)["buffer_bits"] = 1
		case "custom":
			e["video_custom"] = "preset=medium"
		}
		apiAcceptanceRequest(t, f.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": 1, "profile": p}), 422)
		// after сверяет полный ресурс, включая ревизию и времена, после отвергнутого PATCH.
		after := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", path, "", 200))
		if !reflect.DeepEqual(created, after) {
			t.Fatal("Отказ изменил ресурс", field, after)
		}
	}
	// encoder выбирает фиксированные рецепты, где даже пустой nvenc запрещён и имеет точную безопасную ошибку.
	for _, encoder := range []string{"libx265", "hevc_nvenc", "libvpx-vp9"} {
		// p содержит иначе допустимый запрос с единственным неприменимым блоком.
		p := hdrRequest(t, encoder, "transcode", "cbr", false)
		p["encoding"].(map[string]any)["nvenc"] = map[string]any{}
		// problem сохраняет опубликованный доверенный путь ошибки.
		problem := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка блока", "profile": p}), 422))
		if problem["field"] != "profile.encoding.nvenc" || problem["code"] != "invalid_profile" {
			t.Fatal("Утрачена причина отказа", problem)
		}
	}
}

// TestHDRSchemasKeepDesiredAndObservedDistinct проверяет реальную OpenAPI-регистрацию
// текущих строк, точного CRF и независимого наблюдаемого каталога VP9.
// t сообщает об ошибках проверяемого сценария.
func TestHDRSchemasKeepDesiredAndObservedDistinct(t *testing.T) {
	// f содержит настоящее приложение; schemas содержит опубликованные компоненты OpenAPI.
	f := newAPIAcceptanceFixture(t, nil)
	schemas := numericAt(t, apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/openapi.json", "", 200)), "components.schemas").(map[string]any)
	// name выбирает запрос и ответ нового рецепта для симметричного публичного контракта.
	for _, name := range []string{"EncodingInput", "EncodingDTO"} {
		// encoders и crf содержат конкретные схемы без обхода ссылок клиентом.
		encoders := contractSchemaAt(t, schemas, name+".video_encoder")["enum"].([]any)
		crf := contractSchemaAt(t, schemas, name+".crf")
		if !slices.Contains(encoders, any("libx265")) || !slices.Contains(encoders, any("hevc_nvenc")) || !slices.Contains(encoders, any("libvpx-vp9")) || crf["multipleOf"] != json.Number("0.01") {
			t.Fatal(name, encoders, crf)
		}
	}
	// desired и observed содержат разные каталоги профилей: известные VP9 1/3 не являются настройками.
	desired := contractSchemaAt(t, schemas, "VideoFormatDTO.codec_profile")["enum"].([]any)
	observed := contractSchemaAt(t, schemas, "MeasuredVideoDTO.codec_profile")["enum"].([]any)
	if !slices.Contains(desired, any("main10")) || !slices.Contains(desired, any("profile2")) || slices.Contains(desired, any("profile1")) || slices.Contains(desired, any("0")) || !slices.Contains(observed, any("profile1")) || !slices.Contains(observed, any("profile3")) {
		t.Fatal(desired, observed)
	}
	// transfers проверяет HDR-строки как желаемые настройки, независимо от наблюдаемых значений.
	transfers := contractSchemaAt(t, schemas, "ColorDTO.transfer")["enum"].([]any)
	if !slices.Contains(transfers, any("smpte2084")) || !slices.Contains(transfers, any("arib-std-b67")) || slices.Contains(transfers, any("UNKNOWN")) {
		t.Fatal(transfers)
	}
}
