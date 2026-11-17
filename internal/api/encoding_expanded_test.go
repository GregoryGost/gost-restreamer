package api_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

// expandedRequest строит полный текущий запрос без запуска медиапроцессов.
// t проверяет фикстуру, encoder выбирает фиксированный серверный рецепт, rc — управление качеством.
func expandedRequest(t *testing.T, encoder, rc string) map[string]any {
	t.Helper()
	// mode выбирает существующий режим группы по семейству видеокодировщика.
	mode := "transcode"
	if encoder == "libaom-av1" || encoder == "libsvtav1" {
		mode = "copy"
	}
	// p содержит самостоятельный профиль, e — блок кодировщика, video — его целевой формат.
	p := encodingRequestProfile(t, mode, "vbr")
	e := p["encoding"].(map[string]any)
	video := p["format"].(map[string]any)["video"].(map[string]any)
	e["video_encoder"], e["rate_control"] = encoder, rc
	video["level"] = "auto"
	if rc == "crf" || rc == "cqp" {
		// ve содержит независимые поля битрейта, которые чистое качество обязано обнулить.
		ve := p["video_encoding"].(map[string]any)
		ve["target_bitrate_bps"], ve["max_bitrate_bps"], ve["buffer_bits"] = 0, 0, 0
		if rc == "crf" {
			e["crf"] = json.Number("23.51")
			if encoder != "libx264" {
				e["crf"] = json.Number("35")
			}
		} else {
			e["qp"] = json.Number("23")
		}
	}
	return p
}

// TestExpandedEncodingHTTPQualityRoundTrip проверяет точный CRF и явный ноль,
// отсутствие чужих блоков и POST→GET→POST через настоящее хранилище.
// t сообщает об ошибках проверяемого сценария.
func TestExpandedEncodingHTTPQualityRoundTrip(t *testing.T) {
	// f содержит настоящий API и временную SQLite для всех независимых ресурсов.
	f := newAPIAcceptanceFixture(t, nil)
	// tc задаёт кодировщик, исходный токен CRF, канонический ответ и нужный формат.
	for _, tc := range []struct {
		// encoder выбирает рецепт; token и want задают входной и выходной JSON number.
		encoder, token, want string
		// profile и pixel выбирают разрешённую точную пару профиля и размещения компонентов.
		profile, pixel string
		// depth задаёт глубину компонента в битах.
		depth int
	}{
		{"libx264", "2351e-2", "23.51", "high", "yuv420p", 8},
		{"libx264", "51.000", "51", "high10", "yuv420p10le", 10},
		{"libx264", "0e0", "0", "high444", "yuv444p10le", 10},
		{"libaom-av1", "0", "0", "main", "gray", 8},
		{"libaom-av1", "6300e-2", "63", "professional", "yuv422p10le", 10},
		{"libsvtav1", "35e0", "35", "main", "yuv420p10le", 10},
	} {
		// p содержит полный запрос; e задаёт точное качество; video содержит новую пару profile/pixel.
		p := expandedRequest(t, tc.encoder, "crf")
		e := p["encoding"].(map[string]any)
		video := p["format"].(map[string]any)["video"].(map[string]any)
		e["crf"] = json.Number(tc.token)
		video["codec_profile"], video["pixel_format"], video["bit_depth"] = tc.profile, tc.pixel, tc.depth
		// created содержит реальный ответ создания; profile и recipe адресуют сохранённый материализованный профиль.
		created := encodingCreate(t, f, p)
		profile := created["profile"].(map[string]any)
		recipe := profile["encoding"].(map[string]any)
		if recipe["crf"] != json.Number(tc.want) || recipe["qp"] != nil {
			t.Fatal("качество или присутствие изменено", recipe)
		}
		if tc.encoder != "libsvtav1" && (recipe["nvenc"] != nil || recipe["svt_av1"] != nil) {
			t.Fatal("опубликован неприменимый блок", recipe)
		}
		// read содержит независимо прочитанный ресурс, duplicate — копию профиля из ответа.
		read := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/transcode-profiles/"+created["id"].(string), "", 200))
		duplicate := encodingCreate(t, f, profile)
		if !reflect.DeepEqual(read, created) || !reflect.DeepEqual(duplicate["profile"], profile) {
			t.Fatal("публичный цикл изменил профиль", read, duplicate)
		}
	}
}

// TestExpandedEncodingRejectsInvalidCoupling проверяет границы шага и кодировщика,
// обязательность качества и запрет чужих блоков даже при пустом объекте.
// t сообщает об ошибках проверяемого сценария.
func TestExpandedEncodingRejectsInvalidCoupling(t *testing.T) {
	// f содержит изолированное настоящее приложение без медиапроцессов.
	f := newAPIAcceptanceFixture(t, nil)
	// tc задаёт единственное ошибочное качество для выбранного кодировщика.
	for _, tc := range []struct {
		// encoder выбирает рецепт; token содержит точную неверную числовую лексему.
		encoder, token string
	}{
		{"libx264", "23.510000000000001"}, {"libx264", "51.01"}, {"libx264", "0.001"},
		{"libx264", "0"}, {"libaom-av1", "23.51"}, {"libaom-av1", "63.01"},
		{"libsvtav1", "1.01"}, {"libsvtav1", "0"},
	} {
		// p содержит полноценный запрос с ошибочным токеном; e адресует поле качества.
		p := expandedRequest(t, tc.encoder, "crf")
		e := p["encoding"].(map[string]any)
		e["crf"] = json.Number(tc.token)
		apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422)
	}
	// encoder проверяет новые фиксированные рецепты независимо друг от друга.
	for _, encoder := range []string{"libx264", "libaom-av1"} {
		// field выбирает несовместимый блок, null, отсутствие качества и запрещённую строку параметров.
		for _, field := range []string{"nvenc", "svt_av1", "null", "missing", "qp", "custom", "version"} {
			// p и e содержат отдельный запрос, чтобы предыдущая ошибка не влияла на следующий сценарий.
			p := expandedRequest(t, encoder, "crf")
			e := p["encoding"].(map[string]any)
			switch field {
			case "nvenc", "svt_av1":
				e[field] = map[string]any{}
			case "null":
				e["crf"] = nil
			case "missing":
				delete(e, "crf")
			case "qp":
				e["qp"] = 23
			case "custom":
				e["video_custom"] = "lp=2"
			case "version":
				e["version"] = 4
			}
			apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422)
		}
	}
}

// TestExpandedSchemasPublishNumericStepAndSeparateCatalogs проверяет реальные OpenAPI
// и отдельные схемы: CRF остаётся числом, UNKNOWN и unspecified не становятся настройками.
func TestExpandedSchemasPublishNumericStepAndSeparateCatalogs(t *testing.T) {
	// f предоставляет настоящую неизменяемую регистрацию Huma; schemas содержит её компоненты.
	f := newAPIAcceptanceFixture(t, nil)
	schemas := numericAt(t, apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/openapi.json", "", 200)), "components.schemas").(map[string]any)
	// name выбирает одинаковое числовое представление CRF в запросе и ответе.
	for _, name := range []string{"EncodingInput", "EncodingDTO"} {
		// crf содержит конкретную числовую схему без округления metadata.
		crf := contractSchemaAt(t, schemas, name+".crf")
		if crf["type"] != "number" || crf["minimum"] != json.Number("0") || crf["maximum"] != json.Number("63") || crf["multipleOf"] != json.Number("0.01") {
			t.Fatal(name, crf)
		}
	}
	// desired и measured содержат независимые каталоги допустимых и наблюдаемых первичных координат.
	desired := contractSchemaAt(t, schemas, "ColorDTO.primaries")["enum"].([]any)
	measured := contractSchemaAt(t, schemas, "MeasuredColorDTO.primaries")["enum"].([]any)
	if slices.Contains(desired, any("UNKNOWN")) || slices.Contains(desired, any("unspecified")) || !slices.Contains(measured, any("UNKNOWN")) || !slices.Contains(measured, any("unspecified")) || !slices.Contains(desired, any("film")) {
		t.Fatal(desired, measured)
	}
	// individual проверяет фактические metadata после публикации отдельной JSON Schema.
	individual := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/schemas/EncodingInput.json", "", 200))
	if numericAt(t, individual, "properties.crf.multipleOf") != json.Number("0.01") {
		t.Fatal(individual)
	}
}

// TestExpandedEncodingTuplesAndAACLayouts проверяет конечные пары profile/pixel/depth
// через POST и сохраняет шесть многоканальных схем AAC.
// t сообщает об ошибках проверяемого сценария.
func TestExpandedEncodingTuplesAndAACLayouts(t *testing.T) {
	// f предоставляет настоящий API с одной временной БД и отдельными ресурсами каждого сочетания.
	f := newAPIAcceptanceFixture(t, nil)
	// tc задаёт независимо перечисленную разрешённую пару кодировщика и видеопараметров.
	for _, tc := range []struct {
		// encoder выбирает рецепт; profile и pixel задают точные профиль и размещение компонентов.
		encoder, profile, pixel string
		// depth содержит глубину компонента в битах.
		depth int
	}{
		{"libx264", "constrained-baseline", "yuv420p", 8},
		{"libx264", "main", "yuv420p", 8}, {"libx264", "high", "yuv420p", 8},
		{"libx264", "high10", "yuv420p10le", 10},
		{"libx264", "high422", "yuv422p", 8}, {"libx264", "high422", "yuv422p10le", 10},
		{"libx264", "high444", "yuv444p", 8}, {"libx264", "high444", "yuv444p10le", 10},
		{"h264_nvenc", "baseline", "yuv420p", 8}, {"h264_nvenc", "main", "yuv420p", 8},
		{"h264_nvenc", "high", "yuv420p", 8}, {"h264_nvenc", "high444", "yuv444p", 8},
		{"libsvtav1", "main", "yuv420p", 8}, {"libsvtav1", "main", "yuv420p10le", 10},
		{"libaom-av1", "main", "yuv420p", 8}, {"libaom-av1", "main", "yuv420p10le", 10},
		{"libaom-av1", "main", "gray", 8}, {"libaom-av1", "high", "yuv444p", 8},
		{"libaom-av1", "high", "yuv444p10le", 10},
		{"libaom-av1", "professional", "yuv422p", 8}, {"libaom-av1", "professional", "yuv422p10le", 10},
	} {
		// p содержит допустимые базовые битрейты, video — новую пару, ve — число B-кадров.
		p := expandedRequest(t, tc.encoder, "vbr")
		video := p["format"].(map[string]any)["video"].(map[string]any)
		ve := p["video_encoding"].(map[string]any)
		video["codec_profile"], video["pixel_format"], video["bit_depth"] = tc.profile, tc.pixel, tc.depth
		if tc.profile == "baseline" || tc.profile == "constrained-baseline" {
			ve["max_b_frames"] = 0
		}
		// created содержит сохранённый формат; gotVideo проверяет отсутствие скрытого повышения профиля.
		created := encodingCreate(t, f, p)
		gotVideo := created["profile"].(map[string]any)["format"].(map[string]any)["video"].(map[string]any)
		if gotVideo["codec_profile"] != tc.profile || gotVideo["pixel_format"] != tc.pixel || gotVideo["bit_depth"] != json.Number(encodingJSON(t, tc.depth)) {
			t.Fatal(tc, gotVideo)
		}
	}
	// layout и channels перечисляют все дополнительные схемы AAC и их точное число каналов.
	for layout, channels := range map[string]int{"2.1": 3, "3.0": 3, "4.0": 4, "5.0": 5, "5.1": 6, "7.1": 8} {
		// p содержит полный текущий профиль; audio адресует проверяемую аудиодорожку.
		p := expandedRequest(t, "libaom-av1", "crf")
		audio := p["format"].(map[string]any)["audio"].(map[string]any)
		audio["channel_layout"], audio["channels"] = layout, channels
		encodingCreate(t, f, p)
	}
}

// TestExpandedEncodingRejectsQualityInBitrateModes проверяет присутствие, null и
// неверные типы качества независимо от допустимости его числовой величины.
func TestExpandedEncodingRejectsQualityInBitrateModes(t *testing.T) {
	// f изолирует настоящие маршруты от пользовательских данных.
	f := newAPIAcceptanceFixture(t, nil)
	// encoder выбирает каждый поддержанный видеорецепт.
	for _, encoder := range []string{"h264_nvenc", "libx264", "libsvtav1", "libaom-av1"} {
		// field задаёт одно неприменимое поле качества в VBR.
		for _, field := range []string{"qp", "crf"} {
			// value проверяет явный ноль, null и число в кавычках.
			for _, value := range []any{json.Number("0"), nil, "23"} {
				// p и e содержат полноценный VBR с единственным запрещённым полем.
				p := expandedRequest(t, encoder, "vbr")
				e := p["encoding"].(map[string]any)
				e[field] = value
				apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Лишнее качество", "profile": p}), 422)
			}
		}
	}
}

// TestExpandedCQPZeroAndLosslessRequest проверяет явный QP=0 без его замены значением по умолчанию,
// узкую строку NVENC lossless и отсутствие неприменимого CRF в числовом ответе.
func TestExpandedCQPZeroAndLosslessRequest(t *testing.T) {
	// f содержит изолированную временную БД настоящего API.
	f := newAPIAcceptanceFixture(t, nil)
	// tc задаёт разрешённые крайние QP и точный формат, включая запрос lossless без GPU-проверки.
	for _, tc := range []struct {
		// encoder выбирает рецепт; profile и pixel задают нужную строку матрицы.
		encoder, profile, pixel string
		// depth задаёт глубину; qp содержит явный целый квантователь.
		depth, qp int
		// lossless требует специальный tuning NVENC.
		lossless bool
	}{
		{"h264_nvenc", "high", "yuv420p", 8, 0, false},
		{"h264_nvenc", "high", "yuv420p", 8, 51, false},
		{"h264_nvenc", "high444", "yuv444p", 8, 0, true},
		{"libx264", "high444", "yuv444p", 8, 0, false},
		{"libx264", "high444", "yuv444p10le", 10, 0, false},
		{"libx264", "high", "yuv420p", 8, 51, false},
	} {
		// p содержит профиль CQP; e, video и ve адресуют качество, формат и B-кадры.
		p := expandedRequest(t, tc.encoder, "cqp")
		e := p["encoding"].(map[string]any)
		video := p["format"].(map[string]any)["video"].(map[string]any)
		ve := p["video_encoding"].(map[string]any)
		e["qp"] = tc.qp
		video["codec_profile"], video["pixel_format"], video["bit_depth"] = tc.profile, tc.pixel, tc.depth
		if tc.lossless {
			e["nvenc"] = map[string]any{"tuning": "lossless"}
			ve["max_b_frames"] = 0
		}
		// recipe содержит материализованный публичный ответ с точным квантователем.
		recipe := encodingCreate(t, f, p)["profile"].(map[string]any)["encoding"].(map[string]any)
		if recipe["qp"] != json.Number(encodingJSON(t, tc.qp)) || recipe["crf"] != nil {
			t.Fatal(tc, recipe)
		}
		if tc.lossless && recipe["nvenc"].(map[string]any)["tuning"] != "lossless" {
			t.Fatal("запрос lossless подменён", recipe)
		}
	}
}

// TestExpandedLevelAndTierRequests проверяет новые H264 6.x и SVT high tier в
// текущей матрице; успех HTTP не объявляет аппаратную или полную битстримную совместимость.
// t сообщает об ошибках проверяемого сценария.
func TestExpandedLevelAndTierRequests(t *testing.T) {
	// f содержит настоящее приложение с отдельным временным хранилищем.
	f := newAPIAcceptanceFixture(t, nil)
	// encoder выбирает оба кодировщика H264, поддерживающие новую ограниченную строку уровня.
	for _, encoder := range []string{"h264_nvenc", "libx264"} {
		// level перечисляет три новых явных уровня H264.
		for _, level := range []string{"6.0", "6.1", "6.2"} {
			// p содержит битрейтный рецепт; video и ve адресуют уровень и обязательное B=0.
			p := expandedRequest(t, encoder, "vbr")
			video := p["format"].(map[string]any)["video"].(map[string]any)
			ve := p["video_encoding"].(map[string]any)
			video["level"], ve["max_b_frames"] = level, 0
			encodingCreate(t, f, p)
			ve["max_b_frames"] = 1
			apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка B-кадра", "profile": p}), 422)
		}
	}
	// p содержит SVT CBR; video выбирает явные level/high, ve задаёт ровно 1000 мс буфера.
	p := expandedRequest(t, "libsvtav1", "vbr")
	video := p["format"].(map[string]any)["video"].(map[string]any)
	ve := p["video_encoding"].(map[string]any)
	p["encoding"].(map[string]any)["rate_control"] = "cbr"
	video["level"], video["tier"] = "4.0", "high"
	ve["max_bitrate_bps"], ve["buffer_bits"] = 4000000, 4000000
	encodingCreate(t, f, p)
	ve["buffer_bits"] = 4004000
	apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Буфер high tier", "profile": p}), 422)
}
