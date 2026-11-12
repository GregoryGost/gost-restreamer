package api_test

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет назначение режима copy для LIVE-аудио новым профилям обоих видеорежимов при отсутствующем,
// null и явном encoding и точное последующее чтение.
func TestLiveAudioHTTPCreateDefaultsIndependentlyOfEncoding(t *testing.T) {
	// mode выбирает видео copy или transcode при проверке независимого LIVE-аудио.
	for _, mode := range []string{"copy", "transcode"} {
		// encoding задаёт отсутствие, null или явный видеорецепт нового профиля.
		for _, encoding := range []string{"absent", "null", "explicit"} {
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
				if (encoding == "explicit") != (profile["encoding"] != nil) {
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

// insertLiveAudioLegacyHTTPProfile читает сохранённую фикстуру прежнего профиля и вставляет её в
// SQLite без назначения нового селектора LIVE-аудио.
func insertLiveAudioLegacyHTTPProfile(t *testing.T, f *apiAcceptanceFixture) core.TranscodeProfile {
	t.Helper()
	// data содержит сохранённые байты прежних v2-профилей; err содержит отказ чтения фикстуры.
	data, err := os.ReadFile("../core/testdata/live_audio_legacy_v2/profiles.json")
	if err != nil {
		t.Fatal(err)
	}
	// fixtures принимает прежние именованные профили без назначения нового селектора.
	var fixtures []struct {
		// Name выбирает вид прежнего профиля, здесь h264.
		Name string
		// Payload содержит точные сериализованные байты прежнего профиля.
		Payload []byte
	}
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	// fixture содержит очередной прежний профиль при поиске h264.
	for _, fixture := range fixtures {
		if fixture.Name != "h264" {
			continue
		}
		// p содержит декодированный прежний профиль; err содержит отказ чтения его контрактных
		// байтов.
		p, err := core.DecodeProfile(fixture.Payload)
		if err != nil {
			t.Fatal(err)
		}
		// now задаёт согласованные UTC-времена фикстуры с микросекундной точностью SQLite.
		now := time.Now().UTC().Truncate(time.Microsecond)
		// old содержит прежний профиль ревизии 1, вставляемый непосредственно в SQLite.
		old := core.TranscodeProfile{ID: "legacy-profile", Revision: 1, CreatedAt: now, UpdatedAt: now, Name: "Прежний профиль", Profile: p}
		if err = f.store.Write(context.Background(), func(r core.Repositories) error {
			// Вставляет прежний H.264-профиль напрямую для проверки сохранения отсутствующего селектора
			// LIVE-аудио.
			return r.Config.InsertProfile(context.Background(), old)
		}); err != nil {
			t.Fatal(err)
		}
		return old
	}
	t.Fatal("Нет прежнего H.264-профиля")
	return core.TranscodeProfile{}
}

// Проверяет сохранение прежней обработки при чтении и полной замене, обязательность селектора после
// явного выбора и приоритет CAS над отсутствующим полем.
func TestLiveAudioHTTPReplacementPreservesLegacyAndChecksCASBeforeMissingSelector(t *testing.T) {
	// f содержит настоящий API и хранилище сценария совместимости и CAS.
	f := newAPIAcceptanceFixture(t, nil)
	// old содержит вставленный прежний H.264-профиль без live_audio_mode.
	old := insertLiveAudioLegacyHTTPProfile(t, f)
	// path адресует прежний профиль для чтения, переименования и полных замен.
	path := "/transcode-profiles/" + string(old.ID)
	// before содержит исходный публичный профиль без добавленного селектора.
	before := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", path, "", 200))
	// legacy содержит прежний полный профиль для замены с сохранением прежней аудиообработки.
	legacy := before["profile"].(map[string]any)
	// exists показывает нежелательное появление live_audio_mode при GET прежнего профиля.
	if _, exists := legacy["live_audio_mode"]; exists {
		t.Fatal("GET добавил селектор прежнему профилю")
	}
	apiAcceptanceRequest(t, f.api, "PATCH", path, `{"expected_revision":"1","name":"Переименован"}`, 200)
	apiAcceptanceRequest(t, f.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": "2", "profile": legacy}), 200)
	// stored содержит профиль после переименования и прежней полной замены; err содержит отказ
	// чтения.
	stored, err := f.service.GetProfile(context.Background(), old.ID)
	if err != nil || stored.Profile != old.Profile || stored.Revision != 3 {
		t.Fatalf("Прежняя замена изменила обработку: %+v %v", stored, err)
	}
	legacy["live_audio_mode"] = "copy"
	// selected содержит успешный явный выбор LIVE copy для сравнения после последующих отказов.
	selected := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": "3", "profile": legacy}), 200))
	apiAcceptanceRequest(t, f.api, "PATCH", path, `{"expected_revision":"4","name":"Явный профиль"}`, 200)
	delete(legacy, "live_audio_mode")
	apiAcceptanceRequest(t, f.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": "4", "profile": legacy}), 409)
	// problem содержит отказ полной замены без селектора при правильной текущей ревизии.
	problem := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": "5", "profile": legacy}), 422))
	if problem["field"] != "profile.live_audio_mode" {
		t.Fatalf("Не объяснён отсутствующий селектор: %+v", problem)
	}
	// got содержит окончательный GET для проверки сохранённых профиля и ревизии после CAS и отказа
	// селектора.
	got := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", path, "", 200))
	if got["revision"] != "5" || !reflect.DeepEqual(got["profile"], selected["profile"]) {
		t.Fatalf("Отклонённая замена/имя изменили профиль: %+v", got)
	}
}
