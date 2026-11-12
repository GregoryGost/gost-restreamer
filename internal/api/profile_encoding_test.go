package api_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/api"
)

// encodingJSON кодирует значение запроса в JSON и завершает сценарий при ошибке сериализации.
func encodingJSON(t *testing.T, value any) string {
	t.Helper()
	// data содержит JSON-байты тестового запроса; err содержит отказ сериализации значения.
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// encodingRequestProfile создаёт полный допустимый запрос для H264 с transcode либо AV1 с copy и
// согласованными настройками CBR/VBR.
func encodingRequestProfile(t *testing.T, mode, rc string) map[string]any {
	t.Helper()
	// p принимает полный исходный профиль, который затем настраивается под видеорежим и управление
	// битрейтом.
	var p map[string]any
	// err содержит отказ разбора встроенного допустимого JSON-профиля.
	if err := json.Unmarshal([]byte(`{"mode":"transcode","format":{"video":{"codec":"h264","width":640,"height":360,"frame_rate":{"numerator":30,"denominator":1},"codec_profile":"high","level":"4.1","tier":"","pixel_format":"yuv420p","bit_depth":8,"color":{"range":"limited","primaries":"bt709","transfer":"bt709","matrix":"bt709"}},"audio":{"codec":"aac","codec_profile":"LC","sample_rate_hz":48000,"channels":2,"channel_layout":"stereo"}},"video_encoding":{"target_bitrate_bps":"4000000","max_bitrate_bps":"6000000","buffer_bits":"8000000","gop_frames":60,"max_b_frames":2,"closed_gop":true},"audio_encoding":{"bitrate_bps":"128000"}}`), &p); err != nil {
		t.Fatal(err)
	}
	// encoder выбирает NVENC для H.264 transcode либо SVT-AV1 для подготовки AV1 copy.
	encoder := "h264_nvenc"
	// ve указывает общие параметры видео, изменяемые для согласованного CBR/VBR и кодека.
	ve := p["video_encoding"].(map[string]any)
	if rc == "cbr" {
		ve["max_bitrate_bps"] = "4000000"
	}
	if mode == "copy" {
		encoder = "libsvtav1"
		p["mode"] = mode
		// vf указывает формат видео для переключения тестового запроса на AV1 copy.
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

// encodingCreate создаёт профиль через настоящий HTTP-маршрут и возвращает объект ответа 201.
func encodingCreate(t *testing.T, f *apiAcceptanceFixture, p map[string]any) map[string]any {
	t.Helper()
	// body содержит JSON создания именованного профиля через настоящий API.
	body := encodingJSON(t, map[string]any{"name": "Профиль", "profile": p})
	return apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", body, 201))
}

// Проверяет материализацию общих и применимых настроек NVENC/SVT-AV1 для обоих режимов битрейта и
// сохранность GET→POST.
func TestProfileEncodingMaterializesDefaultsAndRoundTripsBothRateControls(t *testing.T) {
	// mode выбирает transcode/H.264 или copy/AV1 для проверки материализации.
	for _, mode := range []string{"transcode", "copy"} {
		// rc выбирает явный cbr или vbr в каждом видеорежиме.
		for _, rc := range []string{"cbr", "vbr"} {
			t.Run(mode+"/"+rc, func(t *testing.T) {
				// Проверяет материализацию настроек выбранного кодировщика и режима битрейта при создании, чтении и
				// копировании профиля.

				// f содержит изолированный настоящий API и SQLite текущего сочетания
				// режима и битрейта.
				f := newAPIAcceptanceFixture(t, nil)
				// p содержит полный допустимый входной профиль выбранного сочетания.
				p := encodingRequestProfile(t, mode, rc)
				// created содержит ответ создания с материализованными настройками
				// кодировщика.
				created := encodingCreate(t, f, p)
				// profile содержит полный публичный профиль из ответа создания.
				profile := created["profile"].(map[string]any)
				// e содержит материализованный encoding для проверки общих значений по
				// умолчанию и применимого блока.
				e := profile["encoding"].(map[string]any)
				if e["audio_encoder"] != "aac" || e["rate_control"] != rc || e["video_custom"] != "" || e["audio_custom"] != "" || e["rescale_output"] != (mode == "transcode") {
					t.Fatal("Неверные общие настройки:", e)
				}
				if mode == "transcode" {
					// want задаёт полный ожидаемый набор значений NVENC по
					// умолчанию.
					want := map[string]any{"preset": "p4", "tuning": "hq", "multipass": "disabled", "lookahead_frames": float64(0), "enable_spatial_aq": false, "enable_temporal_aq": false, "aq_strength": float64(8)}
					if e["svt_av1"] != nil || !reflect.DeepEqual(e["nvenc"], want) {
						t.Fatal("Неполные настройки NVENC:", e)
					}
				} else {
					// lookahead задаёт ожидаемое значение SVT-AV1 по умолчанию: 0
					// для CBR или -1 для VBR.
					lookahead := float64(0)
					if rc == "vbr" {
						lookahead = -1
					}
					// want задаёт полный ожидаемый набор значений SVT-AV1 по
					// умолчанию для текущего режима битрейта.
					want := map[string]any{"preset": float64(8), "tuning": "psnr", "aq_mode": float64(2), "lookahead_frames": lookahead}
					if e["nvenc"] != nil || !reflect.DeepEqual(e["svt_av1"], want) {
						t.Fatal("Неполные настройки SVT-AV1:", e)
					}
				}
				// got содержит прочитанный через GET ресурс для точного сравнения с
				// созданным.
				got := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/transcode-profiles/"+created["id"].(string), "", 200))
				if !reflect.DeepEqual(got, created) {
					t.Fatal("GET изменил созданный профиль:", got, created)
				}
				// copy содержит созданный из возвращённого профиля второй ресурс для
				// проверки GET→POST.
				copy := encodingCreate(t, f, profile)
				if !reflect.DeepEqual(copy["profile"], profile) {
					t.Fatal("GET → POST изменил материализованные настройки:", copy)
				}
			})
		}
	}
}

// Проверяет применение явных false, 0 и -1, нормализацию дополнительных параметров и отклонение
// неподдерживаемого SVT VBR lookahead_frames.
func TestProfileEncodingPreservesFalseZeroAndNormalizesCustom(t *testing.T) {
	// f содержит настоящий API сценария явных нулевых полей и дополнительных параметров.
	f := newAPIAcceptanceFixture(t, nil)
	// p содержит изменяемый допустимый запрос NVENC, затем переиспользуемый для SVT-AV1.
	p := encodingRequestProfile(t, "transcode", "vbr")
	// e указывает входной encoding для задания false, нулей и дополнительных параметров.
	e := p["encoding"].(map[string]any)
	e["rescale_output"] = false
	e["nvenc"] = map[string]any{"lookahead_frames": 0, "enable_spatial_aq": false, "enable_temporal_aq": false}
	e["video_custom"], e["audio_custom"] = "strict_gop=01 nonref_p=+0", "aac_coder=fast"
	// created содержит созданный профиль текущего варианта явных настроек.
	created := encodingCreate(t, f, p)
	// got содержит возвращённый encoding для проверки сохранённых нулей и нормализованных
	// параметров.
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

// Проверяет обязательные поля encoding, запрет null внутри настроек и отклонение даже пустого блока
// другого кодировщика.
func TestProfileEncodingRejectsMissingRequiredNullAndWrongBlocks(t *testing.T) {
	// f содержит изолированный API всех отказов обязательных и неприменимых полей.
	f := newAPIAcceptanceFixture(t, nil)
	// name указывает обязательное поле encoding, удаляемое из очередного запроса.
	for _, name := range []string{"version", "video_encoder", "rate_control"} {
		t.Run("missing/"+name, func(t *testing.T) {
			// Проверяет отказ создания профиля без выбранного обязательного поля encoding.

			// p содержит допустимый профиль до удаления текущего обязательного поля.
			p := encodingRequestProfile(t, "transcode", "vbr")
			delete(p["encoding"].(map[string]any), name)
			apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422)
		})
	}
	// field указывает очередное поле encoding, которому намеренно передаётся null.
	for _, field := range []string{"version", "video_encoder", "audio_encoder", "rate_control", "rescale_output", "video_custom", "audio_custom", "nvenc", "svt_av1"} {
		t.Run("null/"+field, func(t *testing.T) {
			// Проверяет отказ создания профиля с null в выбранном поле encoding.

			// p содержит профиль с null в текущем общем поле encoding.
			p := encodingRequestProfile(t, "transcode", "vbr")
			p["encoding"].(map[string]any)[field] = nil
			apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422)
		})
	}
	// field указывает очередное поле NVENC для проверки запрета null.
	for _, field := range []string{"preset", "tuning", "multipass", "lookahead_frames", "enable_spatial_aq", "enable_temporal_aq", "aq_strength"} {
		// p содержит NVENC-профиль с единственным ошибочным null внутри вложенного блока.
		p := encodingRequestProfile(t, "transcode", "vbr")
		p["encoding"].(map[string]any)["nvenc"] = map[string]any{field: nil}
		apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422)
	}
	// field указывает очередное поле SVT-AV1 для проверки запрета null.
	for _, field := range []string{"preset", "tuning", "aq_mode", "lookahead_frames"} {
		// p содержит SVT-AV1-профиль с единственным ошибочным null внутри вложенного блока.
		p := encodingRequestProfile(t, "copy", "vbr")
		p["encoding"].(map[string]any)["svt_av1"] = map[string]any{field: nil}
		apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422)
	}
	// mode выбирает видеорежим, которому передаётся пустой блок другого кодировщика.
	for _, mode := range []string{"transcode", "copy"} {
		// p содержит допустимый профиль до добавления неприменимого блока.
		p := encodingRequestProfile(t, mode, "vbr")
		// block задаёт имя чужого блока кодировщика, который должен отклоняться даже пустым.
		block := "svt_av1"
		if mode == "copy" {
			block = "nvenc"
		}
		p["encoding"].(map[string]any)[block] = map[string]any{}
		// w содержит ответ 422 на неприменимый блок для проверки безопасных деталей.
		w := apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422)
		// problem содержит Problem Details с точным публичным field чужого блока.
		problem := apiAcceptanceJSON(t, w)
		if problem["field"] != "profile.encoding."+block || problem["code"] != "invalid_profile" {
			t.Fatal("Потеряна понятная ошибка неприменимого блока:", problem)
		}
	}
}

// Проверяет сохранение доверенных code, field и detail для ошибок дополнительных параметров без
// отклонённых значений и понятное объяснение недопустимого AV1 VBR.
func TestProfileEncodingCustomErrorsKeepTrustedDetailsWithoutRejectedValues(t *testing.T) {
	// f содержит настоящий API сценариев проверенных ошибок дополнительных параметров.
	f := newAPIAcceptanceFixture(t, nil)
	// tc задаёт ошибочную строку дополнительных параметров и ожидаемый стабильный код.
	for _, tc := range []struct { /* custom содержит недопустимые дополнительные параметры; code задаёт ожидаемый вид их ошибки. */
		custom, code string
	}{
		{"preset=synthetic-rejected-secret", "custom_conflict"},
		{"rate_control=synthetic-rejected-secret", "custom_conflict"},
		{"nonref_p=synthetic-rejected-secret", "invalid_custom"},
		{"secret_option=synthetic-rejected-secret", "unknown_custom"},
		{"nonref_p=1 nonref_p=0", "duplicate_custom"},
	} {
		// p содержит допустимый профиль с подставляемой ошибочной строкой video_custom.
		p := encodingRequestProfile(t, "transcode", "vbr")
		p["encoding"].(map[string]any)["video_custom"] = tc.custom
		// w содержит ответ 422 для проверки доверенных деталей и отсутствия отклонённых
		// значений.
		w := apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422)
		// problem содержит безопасные code, field и detail ошибки дополнительных параметров.
		problem := apiAcceptanceJSON(t, w)
		if problem["code"] != tc.code || problem["field"] != "profile.encoding.video_custom" || problem["detail"] == "Параметры запроса недопустимы." || problem["errors"] != nil {
			t.Fatal("Проверенная ошибка потеряна при очистке:", problem)
		}
		apiAcceptanceNoSecrets(t, w.Body.String(), "synthetic-rejected-secret", "secret_option")
	}
	// p содержит AV1 VBR-профиль с недопустимым ненулевым buffer_bits.
	p := encodingRequestProfile(t, "copy", "vbr")
	p["video_encoding"].(map[string]any)["buffer_bits"] = "1"
	// problem содержит проверенное объяснение отказа общего video_encoding для AV1 VBR.
	problem := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422))
	if problem["code"] != "invalid_profile" || problem["field"] != "profile.video_encoding" || !strings.Contains(problem["detail"].(string), "VBR") {
		t.Fatal("Ошибка VBR стала общей:", problem)
	}
}

// Проверяет чтение и переименование прежнего рецепта без материализации и запрет удаления уже
// сохранённого encoding при полной замене.
func TestProfileEncodingPatchPreservesLegacyAndRequiresExplicitReplacement(t *testing.T) {
	// f содержит API сценария сохранения прежнего рецепта и обязательной полной замены.
	f := newAPIAcceptanceFixture(t, nil)
	// old содержит вставленный прежний профиль без материализованного encoding.
	old := apiAcceptanceInsertLegacyProfile(t, f, apiAcceptanceProfile())
	// path адресует сохранённый прежний профиль при чтении и изменениях.
	path := "/transcode-profiles/" + string(old.ID)
	// before содержит исходный ответ GET прежнего профиля без назначения новых значений по
	// умолчанию.
	before := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", path, "", 200))
	if before["profile"].(map[string]any)["encoding"] != nil {
		t.Fatal("Прежнему профилю выданы вымышленные значения по умолчанию")
	}
	apiAcceptanceRequest(t, f.api, "PATCH", path, `{"expected_revision":"1","name":"Прежний"}`, 200)
	// stored содержит профиль после переименования; err содержит отказ чтения для проверки
	// неизменности содержимого.
	stored, err := f.service.GetProfile(context.Background(), old.ID)
	if err != nil || stored.Profile != old.Profile {
		t.Fatal("Имя изменило прежний профиль:", stored, err)
	}
	// include выбирает отсутствие encoding либо явный null при создании нового профиля из прежнего
	// рецепта.
	for _, include := range []bool{false, true} {
		// p содержит прежний публичный профиль с выбранным представлением отсутствующего
		// encoding.
		p := before["profile"].(map[string]any)
		if !include {
			delete(p, "encoding")
		} else {
			p["encoding"] = nil
		}
		// created содержит новый ресурс из прежнего видеорецепта с самостоятельным LIVE copy.
		created := encodingCreate(t, f, p)
		if created["profile"].(map[string]any)["encoding"] != nil || created["profile"].(map[string]any)["live_audio_mode"] != "copy" {
			t.Fatal("Новый POST не сохранил видеонастройки или не назначил LIVE copy:", created)
		}
	}
	// p содержит новый явный рецепт для полной замены прежнего профиля.
	p := encodingRequestProfile(t, "transcode", "vbr")
	// changed содержит ответ успешной полной замены для сравнения после отклонённых изменений.
	changed := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": "2", "profile": p}), 200))
	apiAcceptanceRequest(t, f.api, "PATCH", path, `{"expected_revision":"3","name":"Новое имя"}`, 200)
	// encoding выбирает null либо отсутствие блока при запрещённом удалении уже сохранённой версии
	// 2.
	for _, encoding := range []any{nil, "absent"} {
		// legacy содержит прежний видеорецепт, подставляемый как недопустимая полная замена
		// нового encoding.
		legacy := before["profile"].(map[string]any)
		if encoding == nil {
			legacy["encoding"] = nil
		} else {
			delete(legacy, "encoding")
		}
		apiAcceptanceRequest(t, f.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": "4", "profile": legacy}), 422)
	}
	// got содержит итоговый ресурс после отказов для проверки неизменной версии профиля и ревизии.
	got := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", path, "", 200))
	if !reflect.DeepEqual(got["profile"], changed["profile"]) || got["revision"] != "4" {
		t.Fatal("Отклонённая замена изменила профиль:", got)
	}
}

// Проверяет обязательность и типы полей OpenAPI, документированные значения по умолчанию, допустимость
// encoding:null, строгий селектор LIVE-аудио и схему ошибок профиля.
func TestProfileEncodingOpenAPIDocumentsPresenceNullabilityAndDefaults(t *testing.T) {
	// f содержит настоящий API для проверки генерируемой OpenAPI-схемы.
	f := newAPIAcceptanceFixture(t, nil)
	// spec содержит фактический OpenAPI JSON зарегистрированных маршрутов.
	spec := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/openapi.json", "", 200))
	// schemas содержит компоненты схем OpenAPI для проверки типов и обязательности.
	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
	// input содержит схему EncodingInput с проверяемыми обязательными полями.
	input := schemas["EncodingInput"].(map[string]any)
	if !reflect.DeepEqual(input["required"], []any{"version", "video_encoder", "rate_control"}) {
		t.Fatal("Необязательные поля стали обязательными:", input)
	}
	// properties содержит свойства EncodingInput для проверки значений по умолчанию и типа boolean.
	properties := input["properties"].(map[string]any)
	if properties["audio_encoder"].(map[string]any)["default"] != "aac" || properties["video_custom"].(map[string]any)["default"] != "" || properties["rescale_output"].(map[string]any)["type"] != "boolean" {
		t.Fatal("Потеряны значения по умолчанию или тип необязательного поля:", properties)
	}
	// profile содержит схему ответа GroupProfileDTO.
	profile := schemas["GroupProfileDTO"].(map[string]any)
	// encoding содержит свойство encoding ответа для проверки явной допустимости null.
	encoding := profile["properties"].(map[string]any)["encoding"].(map[string]any)
	if !strings.Contains(encodingJSON(t, encoding), `"type":"null"`) {
		t.Fatal("Ответ не описывает прежний encoding:null:", encoding)
	}
	// request содержит схему входного GroupProfileInput.
	request := schemas["GroupProfileInput"].(map[string]any)
	// selector содержит схему live_audio_mode для проверки строгого enum без общего default и
	// nullable.
	selector := request["properties"].(map[string]any)["live_audio_mode"].(map[string]any)
	if selector["type"] != "string" || !reflect.DeepEqual(selector["enum"], []any{"copy", "transcode"}) || selector["default"] != nil || selector["nullable"] == true {
		t.Fatal("Схема LIVE-аудио изменила строгий выбор или назначает общее значение по умолчанию:", selector)
	}
	if !strings.Contains(encodingJSON(t, request["properties"].(map[string]any)["encoding"]), `"type":"null"`) {
		t.Fatal("Запрос не описывает допустимый encoding:null:", request)
	}
	// paths содержит реальные пути OpenAPI для проверки специальных ответов профиля 422.
	paths := spec["paths"].(map[string]any)
	// path задаёт маршрут создания или изменения профиля; method задаёт его HTTP-операцию в OpenAPI.
	for path, method := range map[string]string{api.Prefix + "/transcode-profiles": "post", api.Prefix + "/transcode-profiles/{id}": "patch"} {
		// response содержит объявленный ответ 422 текущей операции для проверки ссылки
		// ProfileProblemDTO.
		response := paths[path].(map[string]any)[method].(map[string]any)["responses"].(map[string]any)["422"]
		if !strings.Contains(encodingJSON(t, response), "ProfileProblemDTO") {
			t.Fatal("422 не описывает проверенные code/field:", response)
		}
	}
}

// Проверяет независимое назначение режима copy для LIVE-аудио при создании, обязательность селектора
// при последующей полной замене, CAS и безопасные ошибки неверного выбора.
func TestProfileLiveAudioCreationReplacementAndTrustedErrors(t *testing.T) {
	// f содержит API сценария независимого назначения LIVE-аудио и последующих полных замен.
	f := newAPIAcceptanceFixture(t, nil)
	// encoding выбирает отсутствие, null или явную версию 2 видеорецепта при создании.
	for _, encoding := range []string{"absent", "null", "v2"} {
		// p содержит полный входной профиль с текущим представлением encoding.
		p := encodingRequestProfile(t, "transcode", "vbr")
		switch encoding {
		case "absent":
			delete(p, "encoding")
		case "null":
			p["encoding"] = nil
		}
		// created содержит новый ресурс для проверки самостоятельного LIVE copy.
		created := encodingCreate(t, f, p)
		// profile содержит возвращённый полный профиль, далее используемый при замене.
		profile := created["profile"].(map[string]any)
		if profile["live_audio_mode"] != "copy" {
			t.Fatal("Новый профиль не получил LIVE copy:", created)
		}
		// path адресует созданный ресурс для проверок полной замены и CAS.
		path := "/transcode-profiles/" + created["id"].(string)
		delete(profile, "live_audio_mode")
		// body содержит JSON полной замены, включая отсутствующий либо явный селектор
		// LIVE-аудио.
		body := encodingJSON(t, map[string]any{"expected_revision": "1", "profile": profile})
		// problem содержит отказ отсутствующего селектора после явного сохранённого выбора.
		problem := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "PATCH", path, body, 422))
		if problem["field"] != "profile.live_audio_mode" || problem["code"] != "invalid_profile" {
			t.Fatal("Полная замена потеряла путь селектора:", problem)
		}
		body = encodingJSON(t, map[string]any{"expected_revision": "9", "profile": profile})
		apiAcceptanceRequest(t, f.api, "PATCH", path, body, 409)
		// named содержит успешное переименование и его новую ревизию.
		named := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "PATCH", path, `{"expected_revision":"1","name":"Другое имя"}`, 200))
		profile["live_audio_mode"] = "transcode"
		body = encodingJSON(t, map[string]any{"expected_revision": named["revision"], "profile": profile})
		// updated содержит успешную полную замену с явным LIVE transcode.
		updated := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "PATCH", path, body, 200))
		if updated["profile"].(map[string]any)["live_audio_mode"] != "transcode" {
			t.Fatal("Явный transcode не сохранён:", updated)
		}
	}
	// value задаёт очередной недопустимый селектор LIVE-аудио.
	for _, value := range []any{nil, "", "synthetic-rejected-secret", true} {
		// p содержит полный профиль с текущим неверным значением live_audio_mode.
		p := encodingRequestProfile(t, "transcode", "vbr")
		p["live_audio_mode"] = value
		// w содержит ответ 422 для проверки безопасной доверенной ошибки селектора.
		w := apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Профиль", "profile": p}), 422)
		// problem содержит публичные поля ошибки селектора без массива исходных нарушений Huma.
		problem := apiAcceptanceJSON(t, w)
		if problem["field"] != "profile.live_audio_mode" || problem["code"] != "invalid_profile" || problem["errors"] != nil {
			t.Fatal("Некорректный selector потерял доверенную ошибку:", problem)
		}
		apiAcceptanceNoSecrets(t, w.Body.String(), "synthetic-rejected-secret")
	}
	// p содержит AV1 copy-профиль с запрещённым перекодированием LIVE-аудио.
	p := encodingRequestProfile(t, "copy", "vbr")
	p["live_audio_mode"] = "transcode"
	// problem содержит отказ несовместимого live_audio_mode для видео copy.
	problem := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Профиль", "profile": p}), 422))
	if problem["field"] != "profile.live_audio_mode" {
		t.Fatal("copy разрешил transcode аудио:", problem)
	}
}
