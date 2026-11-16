package api_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет назначение режима copy для LIVE-аудио новым профилям обоих видеорежимов при отсутствующем,
// null и явном encoding и точное последующее чтение.
// t сообщает об ошибках проверяемого сценария.
func TestLiveAudioHTTPCreateDefaultsIndependentlyOfEncoding(t *testing.T) {
	// mode выбирает видео copy или transcode при проверке независимого LIVE-аудио.
	for _, mode := range []string{"copy", "transcode"} {
		// encoding задаёт отсутствие, null или явный видеорецепт нового профиля.
		for _, encoding := range []string{"absent", "null", "explicit"} {
			// t сообщает об ошибках проверяемого сценария.
			t.Run(mode+"/"+encoding, func(t *testing.T) {
				// Проверяет назначение LIVE copy при создании для выбранного видеорежима и наличия encoding без
				// изменения видеорецепта.

				// f содержит изолированный настоящий API и SQLite текущего сочетания.
				f := newAPIAcceptanceFixture(t, nil)
				// p содержит допустимый профиль с выбранным наличием encoding.
				p := encodingRequestProfile(t, mode, "cbr")
				if encoding == "absent" {
					delete(p, "encoding")
				}
				if encoding == "null" {
					p["encoding"] = nil
				}
				// created содержит результат создания для проверки самостоятельного LIVE
				// copy.
				created := encodingCreate(t, f, p)
				// profile содержит полный возвращённый профиль и выбранный видеорецепт.
				profile := created["profile"].(map[string]any)
				if profile["live_audio_mode"] != "copy" {
					t.Fatalf("Новый профиль не получил copy: %+v", profile)
				}
				if profile["encoding"] == nil {
					t.Fatalf("Изменился видеорецепт: %+v", profile)
				}
				// got содержит последующий GET для проверки точной сохранности
				// созданного ресурса.
				got := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/transcode-profiles/"+created["id"].(string), "", 200))
				if !reflect.DeepEqual(got, created) {
					t.Fatalf("GET изменил профиль: %+v", got)
				}
			})
		}
	}
}

// Проверяет отклонение неверных селекторов LIVE-аудио без сохранения профиля и запрет его
// перекодирования при видео copy.
func TestLiveAudioHTTPRejectsInvalidSelectionsWithoutPersisting(t *testing.T) {
	// f содержит API сценария недопустимых селекторов без сохранения профилей.
	f := newAPIAcceptanceFixture(t, nil)
	// selector задаёт очередное недопустимое значение live_audio_mode.
	for _, selector := range []any{nil, "", "automatic", false, float64(2)} {
		// p содержит допустимый transcode-профиль до замены селектора неверным значением.
		p := encodingRequestProfile(t, "transcode", "vbr")
		p["live_audio_mode"] = selector
		// problem содержит публичный ответ 422 для проверки пути и объяснения селектора.
		problem := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422))
		if selector == "" || selector == "automatic" {
			if problem["field"] != "profile.live_audio_mode" || problem["detail"] == "" {
				t.Fatalf("Потеряно объяснение селектора: %+v", problem)
			}
		}
	}
	// p содержит copy-профиль с запрещённым LIVE transcode и затем допустимый transcode-профиль.
	p := encodingRequestProfile(t, "copy", "vbr")
	p["live_audio_mode"] = "transcode"
	apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422)
	// profiles содержит фактически сохранённые профили; err содержит отказ чтения списка для
	// проверки отсутствия записей после отказов.
	profiles, err := f.service.ListProfiles(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 100}})
	if err != nil || len(profiles.Items) != 0 {
		t.Fatalf("Отказ сохранил профиль: %+v %v", profiles, err)
	}
	p = encodingRequestProfile(t, "transcode", "vbr")
	p["live_audio_mode"] = "transcode"
	// created содержит профиль с явным LIVE transcode для проверки успешного сохранения.
	created := encodingCreate(t, f, p)
	if created["profile"].(map[string]any)["live_audio_mode"] != "transcode" {
		t.Fatal("Явный transcode потерян")
	}
}
