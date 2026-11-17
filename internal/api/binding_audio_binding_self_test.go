package api_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// audioBindingRequest создаёт публичный H264-запрос с конечной привязкой codec.
// t проверяет исходную фикстуру; encoder и profile задаются независимо от рабочих каталогов.
func audioBindingRequest(t *testing.T, codec string) map[string]any {
	t.Helper()
	// p содержит полный H264/VBR-запрос; audio и encoding адресуют его независимые аудиопараметры.
	p := encodingRequestProfile(t, "transcode", "vbr")
	audio := p["format"].(map[string]any)["audio"].(map[string]any)
	encoding := p["encoding"].(map[string]any)
	p["live_audio_mode"] = "transcode"
	audio["codec"], audio["sample_rate_hz"], audio["channels"], audio["channel_layout"] = codec, 48000, 2, "stereo"
	switch codec {
	case "opus":
		audio["codec_profile"], encoding["audio_encoder"] = "celt", "libopus"
		p["audio_encoding"] = map[string]any{"bitrate_bps": 128000}
	case "flac":
		audio["codec_profile"], encoding["audio_encoder"] = "s16", "flac"
		p["audio_encoding"] = map[string]any{"bitrate_bps": 0}
	case "mp3":
		audio["codec_profile"], encoding["audio_encoder"] = "mpeg1-layer3", "libmp3lame"
		p["audio_encoding"] = map[string]any{"bitrate_bps": 128000}
	case "ac3":
		audio["codec_profile"], encoding["audio_encoder"] = "ac3", "ac3"
		p["audio_encoding"] = map[string]any{"bitrate_bps": 192000}
	default:
		t.Fatal("Неизвестная тестовая привязка", codec)
	}
	return p
}

// TestAudioBindingHTTPReadback проверяет настоящие POST/GET/SQLite для каждой аудиопривязки.
// t сравнивает полный публичный профиль с повторным созданием, включая FLAC target0.
func TestAudioBindingHTTPReadback(t *testing.T) {
	// f содержит настоящий API и изолированную временную SQLite без медиапроцессов.
	f := newAPIAcceptanceFixture(t, nil)
	// codec выбирает каждую конечную привязку из независимого тестового списка.
	for _, codec := range []string{"opus", "flac", "mp3", "ac3"} {
		// created сохраняет новый ресурс; profile содержит материализованный публичный профиль.
		created := encodingCreate(t, f, audioBindingRequest(t, codec))
		profile := created["profile"].(map[string]any)
		// stored и err сверяют аудиопривязку после реальной записи.
		stored, err := f.service.GetProfile(context.Background(), core.ID(created["id"].(string)))
		if err != nil || string(stored.Profile.Format.Audio.Codec) != codec {
			t.Fatal("Потерян новый аудиопрофиль", stored, err)
		}
		// duplicate проверяет пригодность ответа для создания самостоятельного эквивалентного профиля.
		duplicate := encodingCreate(t, f, profile)
		if !reflect.DeepEqual(profile, duplicate["profile"]) {
			t.Fatal("HTTP round-trip изменил рецепт", codec)
		}

	}
}

// TestAudioBindingHTTPRejectionIsAtomic проверяет неверный binding, FLAC copy и строковый ноль target.
// t требует неизменности всего сохранённого ресурса после отказов PATCH.
func TestAudioBindingHTTPRejectionIsAtomic(t *testing.T) {
	// f предоставляет отдельное приложение; created и path фиксируют ресурс до ошибочных замен.
	f := newAPIAcceptanceFixture(t, nil)
	created := encodingCreate(t, f, audioBindingRequest(t, "flac"))
	path := "/transcode-profiles/" + created["id"].(string)
	// field выбирает единственное нарушение в самостоятельном запросе FLAC.
	for _, field := range []string{"copy", "selector", "rate", "channels", "profile", "bitrate", "string", "encoder", "custom"} {
		// request содержит исходную допустимую форму; audio и encoding адресуют изменяемые части.
		request := audioBindingRequest(t, "flac")
		audio := request["format"].(map[string]any)["audio"].(map[string]any)
		encoding := request["encoding"].(map[string]any)
		switch field {
		case "copy":
			request["live_audio_mode"] = "copy"
		case "selector":
			delete(request, "live_audio_mode")
		case "rate":
			audio["sample_rate_hz"] = 44100
		case "channels":
			audio["channels"], audio["channel_layout"] = 1, "mono"
		case "profile":
			audio["codec_profile"] = "celt"
		case "bitrate":
			request["audio_encoding"].(map[string]any)["bitrate_bps"] = 128000
		case "string":
			request["audio_encoding"].(map[string]any)["bitrate_bps"] = "0"
		case "encoder":
			encoding["audio_encoder"] = "aac"
		case "custom":
			encoding["audio_custom"] = "aac_coder=fast"
		}
		apiAcceptanceRequest(t, f.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": 1, "profile": request}), 422)
		// after получает ресурс через отдельное чтение после отклонённой мутации.
		after := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", path, "", 200))
		if !reflect.DeepEqual(after, created) {
			t.Fatal("Отказ изменил сохранённый профиль", field)
		}
	}
	// request проверяет создание без явного селектора: default copy не разрешает FLAC.
	request := audioBindingRequest(t, "flac")
	delete(request, "live_audio_mode")
	apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", encodingJSON(t, map[string]any{"name": "Без селектора", "profile": request}), 422)
}

// TestAudioBindingSchemaCatalogAndDependencies проверяет фактически опубликованные OpenAPI enum и описания.
// t требует полный аудиокаталог в обоих направлениях и описание применимости настроек.
func TestAudioBindingSchemaCatalogAndDependencies(t *testing.T) {
	// f содержит настоящее приложение; schemas адресует его опубликованный граф компонентов.
	f := newAPIAcceptanceFixture(t, nil)
	schemas := numericAt(t, apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/openapi.json", "", 200)), "components.schemas").(map[string]any)
	// name выбирает публичные DTO запроса и ответа без смешивания с формой хранения.
	for _, name := range []string{"EncodingInput", "EncodingDTO"} {
		// encoders содержит реальное строковое перечисление аудиокодировщиков.
		encoders := contractSchemaAt(t, schemas, name+".audio_encoder")["enum"].([]any)
		if !slices.Contains(encoders, any("libopus")) || !slices.Contains(encoders, any("flac")) || !slices.Contains(encoders, any("libmp3lame")) || !slices.Contains(encoders, any("ac3")) {
			t.Fatal("Схема не описывает аудиорецепты", name, encoders)
		}
	}
	// description содержит требования нового desired-каталога; token задаёт обязательную связь полей.
	description := contractSchemaAt(t, schemas, "GroupProfileInput.format")["description"].(string)
	for _, token := range []string{"Применимость", "кодировщикам"} {
		if !strings.Contains(description, token) {
			t.Fatal("В схеме отсутствует зависимость", token, description)
		}
	}
	// name выбирает desired и observed аудиокаталоги; codec перечисляет новые известные значения.
	for _, name := range []string{"AudioFormatDTO", "MeasuredAudioDTO"} {
		// codecs содержит опубликованный набор значений конкретного DTO.
		codecs := contractSchemaAt(t, schemas, name+".codec")["enum"].([]any)
		for _, codec := range []string{"opus", "flac", "mp3", "ac3"} {
			if !slices.Contains(codecs, any(codec)) {
				t.Fatal("Кодек отсутствует в схеме", name, codec)
			}
		}
	}
}
