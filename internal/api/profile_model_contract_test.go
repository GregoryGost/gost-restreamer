package api_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// modelContractRequest задаёт независимый полный запрос x265/FLAC с явным нулём CRF.
// t сообщает ошибку встроенной фикстуры; возвращаемое дерево каждый раз создаётся заново.
func modelContractRequest(t *testing.T) map[string]any {
	t.Helper()
	// request содержит явные значения запроса без использования преобразователя запроса реализации.
	var request map[string]any
	// err сообщает об ошибке точного разбора чисел независимой JSON-фикстуры.
	if err := apiAcceptanceDecodeJSON([]byte(`{"mode":"transcode","live_audio_mode":"transcode","format":{"video":{"codec":"hevc","width":640,"height":360,"frame_rate":{"numerator":30,"denominator":1},"codec_profile":"main","level":"","tier":"","pixel_format":"yuv420p","bit_depth":8,"color":{"range":"limited","primaries":"bt709","transfer":"bt709","matrix":"bt709"}},"audio":{"codec":"flac","codec_profile":"s16","sample_rate_hz":48000,"channels":2,"channel_layout":"stereo"}},"video_encoding":{"target_bitrate_bps":0,"max_bitrate_bps":0,"buffer_bits":0,"gop_frames":30,"max_b_frames":0,"closed_gop":true},"audio_encoding":{"bitrate_bps":0},"encoding":{"video_encoder":"libx265","audio_encoder":"flac","rate_control":"crf","crf":0,"rescale_output":false}}`), &request); err != nil {
		t.Fatal(err)
	}
	return request
}

// TestProfileModelHTTPPreservesExplicitZeroThroughRename проверяет HTTP, домен и реальную SQLite.
// t отличает явный CRF0/false/FLAC0 от значений по умолчанию и проверяет сохранность этих значений при переименовании.
func TestProfileModelHTTPPreservesExplicitZeroThroughRename(t *testing.T) {
	// fixture содержит настоящий маршрутизатор, Service и собственную файловую SQLite.
	fixture := newAPIAcceptanceFixture(t, nil)
	// created сохраняет результат независимого полного запроса; profile и encoding адресуют его публичные поля.
	created := encodingCreate(t, fixture, modelContractRequest(t))
	profile := created["profile"].(map[string]any)
	encoding := profile["encoding"].(map[string]any)
	if encoding["video_encoder"] != "libx265" || encoding["audio_encoder"] != "flac" || encoding["crf"] != json.Number("0") || encoding["rescale_output"] != false || encoding["qp"] != nil || encoding["version"] != nil || profile["version"] != nil || profile["live_audio_mode"] != "transcode" || profile["audio_encoding"].(map[string]any)["bitrate_bps"] != json.Number("0") {
		t.Fatal("Явные нули или текущая форма изменены", created)
	}
	// path адресует созданный ресурс для проверки чтения и переименования.
	path := "/transcode-profiles/" + created["id"].(string)
	// renamed меняет только имя, не передавая весь профиль повторно.
	renamed := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "PATCH", path, `{"expected_revision":1,"name":"Сохранённый ноль"}`, 200))
	if renamed["revision"] != json.Number("2") || !reflect.DeepEqual(renamed["profile"], profile) {
		t.Fatal("Переименование материализовало другие настройки", renamed)
	}
	// stored и err проверяют сохранённые доменные значения, независимо от публичной проекции.
	stored, err := fixture.service.GetProfile(context.Background(), core.ID(created["id"].(string)))
	if err != nil || stored.Profile.Encoding.CRF != (core.OptionalCRF{Set: true}) || stored.Profile.Encoding.RescaleOutput || stored.Profile.AudioEncoding.BitrateBPS != 0 || stored.Profile.Encoding.AudioEncoder != "flac" {
		t.Fatal("SQLite потеряла присутствие нулевого качества", stored, err)
	}
	// read подтверждает полный результат HTTP отдельного GET после записи.
	read := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", path, "", 200))
	if !reflect.DeepEqual(read, renamed) {
		t.Fatal("GET изменил полный сохранённый ресурс", read)
	}
}

// TestProfileModelHTTPRejectsCapabilitySelectionAtomically проверяет запрет выбора прежней семантики.
// t отправляет каждый старый селектор в корне и Encoding; callback требует неизменность ресурса после 422.
func TestProfileModelHTTPRejectsCapabilitySelectionAtomically(t *testing.T) {
	// fixture и created фиксируют независимый положительный контроль до отрицательных замен.
	fixture := newAPIAcceptanceFixture(t, nil)
	created := encodingCreate(t, fixture, modelContractRequest(t))
	// path адресует единственный ресурс; location выбирает прежний слой селектора версии.
	path := "/transcode-profiles/" + created["id"].(string)
	for _, location := range []string{"profile", "encoding"} {
		// version задаёт каждое ранее исполнявшееся поведение, которое больше не выбирается входом.
		for version := 1; version <= 7; version++ {
			// t сообщает об ошибках проверяемого сценария.
			t.Run(location+"/"+strconv.Itoa(version), func(t *testing.T) {
				// request создаёт полную допустимую замену до добавления одного чужого селектора.
				request := modelContractRequest(t)
				if location == "profile" {
					request["version"] = version
				} else {
					request["encoding"].(map[string]any)["version"] = version
				}
				apiAcceptanceRequest(t, fixture.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": 1, "profile": request}), 422)
				// after сверяет весь ресурс, включая ревизию и времена, после отвергнутой замены.
				after := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", path, "", 200))
				if !reflect.DeepEqual(after, created) {
					t.Fatal("Отказ выбора версии изменил ресурс", after)
				}
			})
		}
	}
}

// TestProfileModelHTTPShorthandCannotInferNewVideoEncoder проверяет отсутствие новых неявных значений по умолчанию.
// t требует отказ HEVC и VP9 без Encoding, сохраняя исходный x265/FLAC-профиль и его ревизию.
func TestProfileModelHTTPShorthandCannotInferNewVideoEncoder(t *testing.T) {
	// fixture, created и path задают существующий профиль для отрицательных полных замен.
	fixture := newAPIAcceptanceFixture(t, nil)
	created := encodingCreate(t, fixture, modelContractRequest(t))
	path := "/transcode-profiles/" + created["id"].(string)
	// codec и presence выбирают каждый новый видеокодек и обе формы сокращённого запроса.
	for _, codec := range []string{"hevc", "vp9"} {
		for _, presence := range []string{"missing", "null"} {
			// request сохраняет полный исходный формат до удаления обязательного явного выбора encoder.
			request := modelContractRequest(t)
			if codec == "vp9" {
				request["format"].(map[string]any)["video"].(map[string]any)["codec"] = "vp9"
				request["format"].(map[string]any)["video"].(map[string]any)["codec_profile"] = "profile0"
			}
			if presence == "missing" {
				delete(request, "encoding")
			} else {
				request["encoding"] = nil
			}
			// problem должен указывать отсутствующий выбор Encoding, а не последующий отказ другой дорожки.
			problem := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": 1, "profile": request}), 422))
			if problem["field"] != "profile.encoding" || problem["code"] != "invalid_profile" {
				t.Fatalf("Не подтверждён отказ преобразователя запроса для %s/%s: %v", codec, presence, problem)
			}
			// after проверяет отсутствие записи при отказе преобразователя запроса до доменной операции.
			after := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", path, "", 200))
			if !reflect.DeepEqual(after, created) {
				t.Fatalf("Отказ %s/%s изменил ресурс: %v", codec, presence, after)
			}
		}
	}
}
