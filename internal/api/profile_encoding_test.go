package api_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/api"
)

func encodingJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func encodingRequestProfile(t *testing.T, mode, rc string) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal([]byte(`{"mode":"transcode","format":{"video":{"codec":"h264","width":640,"height":360,"frame_rate":{"numerator":30,"denominator":1},"codec_profile":"high","level":"4.1","tier":"","pixel_format":"yuv420p","bit_depth":8,"color":{"range":"limited","primaries":"bt709","transfer":"bt709","matrix":"bt709"}},"audio":{"codec":"aac","codec_profile":"LC","sample_rate_hz":48000,"channels":2,"channel_layout":"stereo"}},"video_encoding":{"target_bitrate_bps":"4000000","max_bitrate_bps":"6000000","buffer_bits":"8000000","gop_frames":60,"max_b_frames":2,"closed_gop":true},"audio_encoding":{"bitrate_bps":"128000"}}`), &p); err != nil {
		t.Fatal(err)
	}
	encoder := "h264_nvenc"
	ve := p["video_encoding"].(map[string]any)
	if rc == "cbr" {
		ve["max_bitrate_bps"] = "4000000"
	}
	if mode == "copy" {
		encoder = "libsvtav1"
		p["mode"] = mode
		vf := p["format"].(map[string]any)["video"].(map[string]any)
		vf["codec"], vf["codec_profile"] = "av1", "main"
		ve["max_b_frames"] = 0
		if rc == "vbr" {
			ve["max_bitrate_bps"], ve["buffer_bits"] = "0", "0"
		}
	}
	p["encoding"] = map[string]any{"version": 2, "video_encoder": encoder, "rate_control": rc}
	return p
}

func encodingCreate(t *testing.T, f *apiAcceptanceFixture, p map[string]any) map[string]any {
	t.Helper()
	body := encodingJSON(t, map[string]any{"name": "Профиль", "profile": p})
	return apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", body, 201))
}

func TestProfileEncodingMaterializesDefaultsAndRoundTripsBothRateControls(t *testing.T) {
	for _, mode := range []string{"transcode", "copy"} {
		for _, rc := range []string{"cbr", "vbr"} {
			t.Run(mode+"/"+rc, func(t *testing.T) {
				f := newAPIAcceptanceFixture(t, nil)
				p := encodingRequestProfile(t, mode, rc)
				created := encodingCreate(t, f, p)
				profile := created["profile"].(map[string]any)
				e := profile["encoding"].(map[string]any)
				if e["audio_encoder"] != "aac" || e["rate_control"] != rc || e["video_custom"] != "" || e["audio_custom"] != "" || e["rescale_output"] != (mode == "transcode") {
					t.Fatal("Неверные общие настройки:", e)
				}
				if mode == "transcode" {
					want := map[string]any{"preset": "p4", "tuning": "hq", "multipass": "disabled", "lookahead_frames": float64(0), "enable_spatial_aq": false, "enable_temporal_aq": false, "aq_strength": float64(8)}
					if e["svt_av1"] != nil || !reflect.DeepEqual(e["nvenc"], want) {
						t.Fatal("Неполные настройки NVENC:", e)
					}
				} else {
					lookahead := float64(0)
					if rc == "vbr" {
						lookahead = -1
					}
					want := map[string]any{"preset": float64(8), "tuning": "psnr", "aq_mode": float64(2), "lookahead_frames": lookahead}
					if e["nvenc"] != nil || !reflect.DeepEqual(e["svt_av1"], want) {
						t.Fatal("Неполные настройки SVT-AV1:", e)
					}
				}
				got := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/transcode-profiles/"+created["id"].(string), "", 200))
				if !reflect.DeepEqual(got, created) {
					t.Fatal("GET изменил созданный профиль:", got, created)
				}
				copy := encodingCreate(t, f, profile)
				if !reflect.DeepEqual(copy["profile"], profile) {
					t.Fatal("GET → POST изменил материализованные настройки:", copy)
				}
			})
		}
	}
}

func TestProfileEncodingPreservesFalseZeroAndNormalizesCustom(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	p := encodingRequestProfile(t, "transcode", "vbr")
	e := p["encoding"].(map[string]any)
	e["rescale_output"] = false
	e["nvenc"] = map[string]any{"lookahead_frames": 0, "enable_spatial_aq": false, "enable_temporal_aq": false}
	e["video_custom"], e["audio_custom"] = "strict_gop=01 nonref_p=+0", "aac_coder=fast"
	created := encodingCreate(t, f, p)
	got := created["profile"].(map[string]any)["encoding"].(map[string]any)
	if got["rescale_output"] != false || got["video_custom"] != "nonref_p=0 strict_gop=1" || got["audio_custom"] != "aac_coder=fast" {
		t.Fatal("Явные настройки изменены:", got)
	}
	p = encodingRequestProfile(t, "copy", "vbr")
	e = p["encoding"].(map[string]any)
	e["svt_av1"] = map[string]any{"preset": 0, "lookahead_frames": -1}
	e["video_custom"] = "lp=+0"
	created = encodingCreate(t, f, p)
	got = created["profile"].(map[string]any)["encoding"].(map[string]any)
	if got["svt_av1"].(map[string]any)["preset"] != float64(0) || got["svt_av1"].(map[string]any)["lookahead_frames"] != float64(-1) || got["video_custom"] != "lp=0" {
		t.Fatal("Явный ноль или auto заменён значением по умолчанию:", got)
	}
	p = encodingRequestProfile(t, "copy", "vbr")
	p["encoding"].(map[string]any)["svt_av1"] = map[string]any{"lookahead_frames": 0}
	apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422)
}

func TestProfileEncodingRejectsMissingRequiredNullAndWrongBlocks(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	for _, name := range []string{"version", "video_encoder", "rate_control"} {
		t.Run("missing/"+name, func(t *testing.T) {
			p := encodingRequestProfile(t, "transcode", "vbr")
			delete(p["encoding"].(map[string]any), name)
			apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422)
		})
	}
	for _, field := range []string{"version", "video_encoder", "audio_encoder", "rate_control", "rescale_output", "video_custom", "audio_custom", "nvenc", "svt_av1"} {
		t.Run("null/"+field, func(t *testing.T) {
			p := encodingRequestProfile(t, "transcode", "vbr")
			p["encoding"].(map[string]any)[field] = nil
			apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422)
		})
	}
	for _, field := range []string{"preset", "tuning", "multipass", "lookahead_frames", "enable_spatial_aq", "enable_temporal_aq", "aq_strength"} {
		p := encodingRequestProfile(t, "transcode", "vbr")
		p["encoding"].(map[string]any)["nvenc"] = map[string]any{field: nil}
		apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422)
	}
	for _, field := range []string{"preset", "tuning", "aq_mode", "lookahead_frames"} {
		p := encodingRequestProfile(t, "copy", "vbr")
		p["encoding"].(map[string]any)["svt_av1"] = map[string]any{field: nil}
		apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422)
	}
	for _, mode := range []string{"transcode", "copy"} {
		p := encodingRequestProfile(t, mode, "vbr")
		block := "svt_av1"
		if mode == "copy" {
			block = "nvenc"
		}
		p["encoding"].(map[string]any)[block] = map[string]any{}
		w := apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422)
		problem := apiAcceptanceJSON(t, w)
		if problem["field"] != "profile.encoding."+block || problem["code"] != "invalid_profile" {
			t.Fatal("Потеряна понятная ошибка неприменимого блока:", problem)
		}
	}
}

func TestProfileEncodingCustomErrorsKeepTrustedDetailsWithoutRejectedValues(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	for _, tc := range []struct{ custom, code string }{
		{"preset=synthetic-rejected-secret", "custom_conflict"},
		{"rate_control=synthetic-rejected-secret", "custom_conflict"},
		{"nonref_p=synthetic-rejected-secret", "invalid_custom"},
		{"secret_option=synthetic-rejected-secret", "unknown_custom"},
		{"nonref_p=1 nonref_p=0", "duplicate_custom"},
	} {
		p := encodingRequestProfile(t, "transcode", "vbr")
		p["encoding"].(map[string]any)["video_custom"] = tc.custom
		w := apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422)
		problem := apiAcceptanceJSON(t, w)
		if problem["code"] != tc.code || problem["field"] != "profile.encoding.video_custom" || problem["detail"] == "Параметры запроса недопустимы." || problem["errors"] != nil {
			t.Fatal("Проверенная ошибка потеряна при очистке:", problem)
		}
		apiAcceptanceNoSecrets(t, w.Body.String(), "synthetic-rejected-secret", "secret_option")
	}
	p := encodingRequestProfile(t, "copy", "vbr")
	p["video_encoding"].(map[string]any)["buffer_bits"] = "1"
	problem := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422))
	if problem["code"] != "invalid_profile" || problem["field"] != "profile.video_encoding" || !strings.Contains(problem["detail"].(string), "VBR") {
		t.Fatal("Ошибка VBR стала общей:", problem)
	}
}

func TestProfileEncodingPatchPreservesLegacyAndRequiresExplicitReplacement(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	_, old, _, _ := apiAcceptanceSeedConfiguration(t, f)
	path := "/transcode-profiles/" + string(old.ID)
	before := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", path, "", 200))
	if before["profile"].(map[string]any)["encoding"] != nil {
		t.Fatal("Прежнему профилю выданы вымышленные значения по умолчанию")
	}
	apiAcceptanceRequest(t, f.api, "PATCH", path, `{"expected_revision":"1","name":"Прежний"}`, 200)
	stored, err := f.service.GetProfile(context.Background(), old.ID)
	if err != nil || stored.Profile != old.Profile {
		t.Fatal("Имя изменило прежний профиль:", stored, err)
	}
	for _, include := range []bool{false, true} {
		p := before["profile"].(map[string]any)
		if !include {
			delete(p, "encoding")
		} else {
			p["encoding"] = nil
		}
		created := encodingCreate(t, f, p)
		if created["profile"].(map[string]any)["encoding"] != nil {
			t.Fatal("Прежний POST автоматически обновлён:", created)
		}
	}
	p := encodingRequestProfile(t, "transcode", "vbr")
	changed := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": "2", "profile": p}), 200))
	apiAcceptanceRequest(t, f.api, "PATCH", path, `{"expected_revision":"3","name":"Новое имя"}`, 200)
	for _, encoding := range []any{nil, "absent"} {
		legacy := before["profile"].(map[string]any)
		if encoding == nil {
			legacy["encoding"] = nil
		} else {
			delete(legacy, "encoding")
		}
		apiAcceptanceRequest(t, f.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": "4", "profile": legacy}), 422)
	}
	got := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", path, "", 200))
	if !reflect.DeepEqual(got["profile"], changed["profile"]) || got["revision"] != "4" {
		t.Fatal("Отклонённая замена изменила профиль:", got)
	}
}

func TestProfileEncodingOpenAPIDocumentsPresenceNullabilityAndDefaults(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	spec := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/openapi.json", "", 200))
	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
	input := schemas["EncodingInput"].(map[string]any)
	if !reflect.DeepEqual(input["required"], []any{"version", "video_encoder", "rate_control"}) {
		t.Fatal("Необязательные поля стали обязательными:", input)
	}
	properties := input["properties"].(map[string]any)
	if properties["audio_encoder"].(map[string]any)["default"] != "aac" || properties["video_custom"].(map[string]any)["default"] != "" || properties["rescale_output"].(map[string]any)["type"] != "boolean" {
		t.Fatal("Потеряны значения по умолчанию или тип необязательного поля:", properties)
	}
	profile := schemas["GroupProfileDTO"].(map[string]any)
	encoding := profile["properties"].(map[string]any)["encoding"].(map[string]any)
	if !strings.Contains(encodingJSON(t, encoding), `"type":"null"`) {
		t.Fatal("Ответ не описывает прежний encoding:null:", encoding)
	}
	request := schemas["GroupProfileInput"].(map[string]any)
	if !strings.Contains(encodingJSON(t, request["properties"].(map[string]any)["encoding"]), `"type":"null"`) {
		t.Fatal("Запрос не описывает допустимый encoding:null:", request)
	}
	paths := spec["paths"].(map[string]any)
	for path, method := range map[string]string{api.Prefix + "/transcode-profiles": "post", api.Prefix + "/transcode-profiles/{id}": "patch"} {
		response := paths[path].(map[string]any)[method].(map[string]any)["responses"].(map[string]any)["422"]
		if !strings.Contains(encodingJSON(t, response), "ProfileProblemDTO") {
			t.Fatal("422 не описывает проверенные code/field:", response)
		}
	}
}
