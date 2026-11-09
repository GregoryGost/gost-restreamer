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

func TestLiveAudioHTTPCreateDefaultsIndependentlyOfEncoding(t *testing.T) {
	for _, mode := range []string{"copy", "transcode"} {
		for _, encoding := range []string{"absent", "null", "explicit"} {
			t.Run(mode+"/"+encoding, func(t *testing.T) {
				f := newAPIAcceptanceFixture(t, nil)
				p := encodingRequestProfile(t, mode, "cbr")
				if encoding == "absent" {
					delete(p, "encoding")
				}
				if encoding == "null" {
					p["encoding"] = nil
				}
				created := encodingCreate(t, f, p)
				profile := created["profile"].(map[string]any)
				if profile["live_audio_mode"] != "copy" {
					t.Fatalf("Новый профиль не получил copy: %+v", profile)
				}
				if (encoding == "explicit") != (profile["encoding"] != nil) {
					t.Fatalf("Изменился видеорецепт: %+v", profile)
				}
				got := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/transcode-profiles/"+created["id"].(string), "", 200))
				if !reflect.DeepEqual(got, created) {
					t.Fatalf("GET изменил профиль: %+v", got)
				}
			})
		}
	}
}

func TestLiveAudioHTTPRejectsInvalidSelectionsWithoutPersisting(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	for _, selector := range []any{nil, "", "automatic", false, float64(2)} {
		p := encodingRequestProfile(t, "transcode", "vbr")
		p["live_audio_mode"] = selector
		problem := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422))
		if selector == "" || selector == "automatic" {
			if problem["field"] != "profile.live_audio_mode" || problem["detail"] == "" {
				t.Fatalf("Потеряно объяснение селектора: %+v", problem)
			}
		}
	}
	p := encodingRequestProfile(t, "copy", "vbr")
	p["live_audio_mode"] = "transcode"
	apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Ошибка", "profile": p}), 422)
	profiles, err := f.service.ListProfiles(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 100}})
	if err != nil || len(profiles.Items) != 0 {
		t.Fatalf("Отказ сохранил профиль: %+v %v", profiles, err)
	}
	p = encodingRequestProfile(t, "transcode", "vbr")
	p["live_audio_mode"] = "transcode"
	created := encodingCreate(t, f, p)
	if created["profile"].(map[string]any)["live_audio_mode"] != "transcode" {
		t.Fatal("Явный transcode потерян")
	}
}

func insertLiveAudioLegacyHTTPProfile(t *testing.T, f *apiAcceptanceFixture) core.TranscodeProfile {
	t.Helper()
	data, err := os.ReadFile("../core/testdata/live_audio_legacy_v2/profiles.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name    string
		Payload []byte
	}
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		if fixture.Name != "h264" {
			continue
		}
		p, err := core.DecodeProfile(fixture.Payload)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		old := core.TranscodeProfile{ID: "legacy-profile", Revision: 1, CreatedAt: now, UpdatedAt: now, Name: "Прежний профиль", Profile: p}
		if err = f.store.Write(context.Background(), func(r core.Repositories) error { return r.Config.InsertProfile(context.Background(), old) }); err != nil {
			t.Fatal(err)
		}
		return old
	}
	t.Fatal("Нет прежнего H.264-профиля")
	return core.TranscodeProfile{}
}

func TestLiveAudioHTTPReplacementPreservesLegacyAndChecksCASBeforeMissingSelector(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	old := insertLiveAudioLegacyHTTPProfile(t, f)
	path := "/transcode-profiles/" + string(old.ID)
	before := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", path, "", 200))
	legacy := before["profile"].(map[string]any)
	if _, exists := legacy["live_audio_mode"]; exists {
		t.Fatal("GET добавил селектор прежнему профилю")
	}
	apiAcceptanceRequest(t, f.api, "PATCH", path, `{"expected_revision":"1","name":"Переименован"}`, 200)
	apiAcceptanceRequest(t, f.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": "2", "profile": legacy}), 200)
	stored, err := f.service.GetProfile(context.Background(), old.ID)
	if err != nil || stored.Profile != old.Profile || stored.Revision != 3 {
		t.Fatalf("Прежняя замена изменила обработку: %+v %v", stored, err)
	}
	legacy["live_audio_mode"] = "copy"
	selected := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": "3", "profile": legacy}), 200))
	apiAcceptanceRequest(t, f.api, "PATCH", path, `{"expected_revision":"4","name":"Явный профиль"}`, 200)
	delete(legacy, "live_audio_mode")
	apiAcceptanceRequest(t, f.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": "4", "profile": legacy}), 409)
	problem := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": "5", "profile": legacy}), 422))
	if problem["field"] != "profile.live_audio_mode" {
		t.Fatalf("Не объяснён отсутствующий селектор: %+v", problem)
	}
	got := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", path, "", 200))
	if got["revision"] != "5" || !reflect.DeepEqual(got["profile"], selected["profile"]) {
		t.Fatalf("Отклонённая замена/имя изменили профиль: %+v", got)
	}
}
