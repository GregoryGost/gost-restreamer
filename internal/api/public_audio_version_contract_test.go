package api_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestFLACWithX264FractionalCRF проверяет полный публичный запрос
// FLAC/H.264/x264 с CRF 23.5, нулевым VBV и полным профилем после записи.
// t использует настоящие HTTP/SQLite и проверяет отказ при неполной аудиопривязке.
func TestFLACWithX264FractionalCRF(t *testing.T) {
	// fixture предоставляет реальный API и собственную временную SQLite без кодировщика.
	fixture := newAPIAcceptanceFixture(t, nil)
	// request дополняет обязательный H.264-формат явными параметрами FLAC и x264 CRF.
	request := encodingRequestProfile(t, "transcode", "vbr")
	request["live_audio_mode"] = "transcode"
	request["format"].(map[string]any)["audio"] = map[string]any{"codec": "flac", "codec_profile": "s16", "sample_rate_hz": 48000, "channels": 2, "channel_layout": "stereo"}
	request["format"].(map[string]any)["video"].(map[string]any)["level"] = "auto"
	request["audio_encoding"] = map[string]any{"bitrate_bps": 0}
	request["encoding"] = map[string]any{"video_encoder": "libx264", "audio_encoder": "flac", "rate_control": "crf", "crf": json.Number("23.5")}
	// video содержит общие видеопараметры; чистый CRF не получает целевой битрейт или VBV.
	video := request["video_encoding"].(map[string]any)
	video["target_bitrate_bps"], video["max_bitrate_bps"], video["buffer_bits"] = 0, 0, 0
	// created и profile содержат фактический ответ, encoding выделяет материализованный рецепт.
	created := encodingCreate(t, fixture, request)
	profile := created["profile"].(map[string]any)
	encoding := profile["encoding"].(map[string]any)
	if encoding["video_encoder"] != "libx264" || encoding["audio_encoder"] != "flac" || encoding["crf"] != json.Number("23.5") || profile["audio_encoding"].(map[string]any)["bitrate_bps"] != json.Number("0") {
		t.Fatal("Ответ изменил явный рецепт или точное качество", profile)
	}
	// stored и err сверяют запись через другой доменный путь без публичного округления CRF.
	stored, err := fixture.service.GetProfile(context.Background(), core.ID(created["id"].(string)))
	if err != nil || stored.Profile.Encoding.CRF != (core.OptionalCRF{Set: true, ValueHundredths: 2350}) || stored.Profile.Format.Audio.Codec != core.CodecFLAC || stored.Profile.AudioEncoding.BitrateBPS != 0 {
		t.Fatal("Сохранённая форма потеряла точный рецепт", stored, err)
	}
	// path адресует созданный профиль; read проверяет независимый GET всей материализованной формы.
	path := "/transcode-profiles/" + created["id"].(string)
	read := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", path, "", 200))
	if !reflect.DeepEqual(read, created) || !reflect.DeepEqual(encodingCreate(t, fixture, profile)["profile"], profile) {
		t.Fatal("GET или повторный POST изменил полный рецепт")
	}
	// missing выбирает отсутствие encoder либо audio-профиля: оба обязательны для конечной привязки.
	for _, missing := range []string{"audio_encoder", "codec_profile"} {
		// invalid содержит независимую копию полного корректного ответа до единственного удаления.
		invalid := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", path, "", 200))["profile"].(map[string]any)
		if missing == "audio_encoder" {
			delete(invalid["encoding"].(map[string]any), missing)
		} else {
			delete(invalid["format"].(map[string]any)["audio"].(map[string]any), missing)
		}
		apiAcceptanceRequest(t, fixture.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": 1, "profile": invalid}), 422)
		if !reflect.DeepEqual(apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", path, "", 200)), created) {
			t.Fatal("Отклонённая неполная привязка изменила ресурс", missing)
		}
	}
}

// TestMeasuredAudioHTTPClosesProfilesPerCodec проверяет измерения через
// настоящий GET, включая чужой профиль из известного объединённого enum.
// t требует UNKNOWN для каждой чужой пары, сохраняя исходную запись и пустоту.
func TestMeasuredAudioHTTPClosesProfilesPerCodec(t *testing.T) {
	// repository предоставляет только синтетические наблюдения без внешнего ввода-вывода.
	repository := &measuredVariantRepository{}
	// service и err связывают проверяемую HTTP-проекцию с настоящим доменным чтением.
	service, err := core.NewService(measuredVariantWork{repository}, "audio-measured-owner")
	if err != nil {
		t.Fatal(err)
	}
	// fixture регистрирует реальный маршрут; callback config заменяет только источник доменного GET.
	fixture := newAPIAcceptanceFixture(t, func(config *api.Config) { config.Core = service })
	// binding задаёт независимые конечные пары codec/profile из публичного контракта.
	for _, binding := range []struct {
		// codec обозначает наблюдаемый кодек; profile содержит его единственный известный профиль.
		codec, profile string
	}{{"aac", "LC"}, {"opus", "celt"}, {"flac", "s16"}, {"mp3", "mpeg1-layer3"}, {"ac3", "ac3"}} {
		// measured выбирает каждый профиль объединённого enum, пустоту и неизвестное непустое имя.
		for _, measured := range []string{"LC", "celt", "s16", "mpeg1-layer3", "ac3", "", "future-profile"} {
			// raw содержит измерение, которое HTTP не вправе изменять или дополнять.
			raw := core.MediaFormat{Audio: core.AudioFormat{Codec: core.Codec(binding.codec), CodecProfile: measured, SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}
			repository.variant = core.FallbackVariant{Record: core.Record{ID: "variant"}, AssetID: "asset", GenerationID: "generation", State: core.VariantFailed, Profile: apiAcceptanceProfile(), VerifiedFormat: raw}
			// page и audio содержат реальные JSON-поля ответа списка вариантов.
			page := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", "/fallback-assets/asset/variants", "", 200))
			audio := page["items"].([]any)[0].(map[string]any)["format"].(map[string]any)["audio"].(map[string]any)
			// expected отделяет отсутствие измерения от неизвестного и чужого профиля.
			expected := "UNKNOWN"
			if measured == "" || measured == binding.profile {
				expected = measured
			}
			if audio["codec"] != binding.codec || audio["codec_profile"] != expected || audio["sample_rate_hz"] != json.Number("48000") || audio["channels"] != json.Number("2") || audio["channel_layout"] != "stereo" || repository.variant.VerifiedFormat != raw {
				t.Fatal("Нарушена проекция отдельного аудиокаталога", binding.codec, measured, expected, audio)
			}
		}
	}
}
