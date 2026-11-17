package api_test

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestCurrentProfileShorthandMaterializesFullSettings проверяет через HTTP и SQLite,
// что отсутствующий/null encoding сразу получает тот же полный профиль, что явный запрос.
// t владеет временным приложением; расширенные CICP проверяют единый каталог и для shorthand.
func TestCurrentProfileShorthandMaterializesFullSettings(t *testing.T) {
	// fixture содержит настоящий API и отдельную временную SQLite.
	fixture := newAPIAcceptanceFixture(t, nil)
	// mode задаёт подтверждённые сочетания H264/transcode и AV1/copy.
	for _, mode := range []string{"transcode", "copy"} {
		// rc выбирает единственное значение по умолчанию соответствующего сочетания.
		rc := "vbr"
		if mode == "copy" {
			rc = "cbr"
		}
		// request задаёт корректный текущий профиль, включая расширенный SDR-цвет.
		request := encodingRequestProfile(t, mode, rc)
		request["format"].(map[string]any)["video"].(map[string]any)["color"] = map[string]any{"range": "full", "primaries": "film", "transfer": "gamma22", "matrix": "fcc"}
		// explicit сохраняет независимый положительный контроль материализованных полей.
		explicit := encodingCreate(t, fixture, request)["profile"]
		// presence выбирает отсутствие или null без изменения остальных полей.
		for _, presence := range []string{"absent", "null"} {
			if presence == "absent" {
				delete(request, "encoding")
			} else {
				request["encoding"] = nil
			}
			// created содержит фактический сохранённый результат сокращённого запроса.
			created := encodingCreate(t, fixture, request)
			if !reflect.DeepEqual(created["profile"], explicit) {
				t.Fatal(mode, presence, created["profile"], explicit)
			}
		}
	}
}

// TestCurrentProfileReplacementMaterializesShorthand проверяет полную замену без
// зависимости от прежнего видеорецепта. t проверяет rename, CAS и материализацию null.
func TestCurrentProfileReplacementMaterializesShorthand(t *testing.T) {
	// fixture содержит изолированное приложение; request задаёт явный текущий NVENC/VBR.
	fixture := newAPIAcceptanceFixture(t, nil)
	request := encodingRequestProfile(t, "transcode", "vbr")
	// created сохраняет полные значения перед переименованием.
	created := encodingCreate(t, fixture, request)
	// path адресует единственный изменяемый ресурс.
	path := "/transcode-profiles/" + created["id"].(string)
	// renamed содержит результат изменения имени, которое не должно менять профиль.
	renamed := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "PATCH", path, `{"expected_revision":1,"name":"Новое имя"}`, 200))
	if !reflect.DeepEqual(renamed["profile"], created["profile"]) {
		t.Fatal("Переименование изменило настройки", renamed)
	}
	request["live_audio_mode"], request["encoding"] = "copy", nil
	// replaced содержит полный результат текущей материализации, включая nonnull encoding.
	replaced := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": json.Number("2"), "profile": request}), 200))
	if !reflect.DeepEqual(replaced["profile"], created["profile"]) {
		t.Fatal("Замена shorthand изменила текущие defaults", replaced)
	}
}

// TestCurrentProfileRequiresExplicitAudioBinding проверяет, что новый desired-кодек
// не выбирает аудиокодировщик автоматически. t проверяет отсутствие и null encoding.
func TestCurrentProfileRequiresExplicitAudioBinding(t *testing.T) {
	// fixture содержит реальную регистрацию и временную SQLite для отрицательных запросов.
	fixture := newAPIAcceptanceFixture(t, nil)
	// codec задаёт каждую конечную привязку, для которой требуется явный audio_encoder.
	for _, codec := range []string{"opus", "flac", "mp3", "ac3"} {
		// presence отделяет отсутствие настроек от явного null.
		for _, presence := range []string{"absent", "null"} {
			// request содержит допустимую полную привязку до удаления явного кодировщика.
			request := audioBindingRequest(t, codec)
			if presence == "absent" {
				delete(request, "encoding")
			} else {
				request["encoding"] = nil
			}
			apiAcceptanceRequest(t, fixture.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Без выбора кодировщика", "profile": request}), 422)
		}
	}
}
