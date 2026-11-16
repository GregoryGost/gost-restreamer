package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// strictWireProfile строит самостоятельный текущий JSON с точными числовыми полями.
// t сообщает о повреждении встроенного допустимого примера.
func strictWireProfile(t *testing.T) map[string]any {
	t.Helper()
	// profile принимает строки и точные числовые лексемы без преобразования в float64.
	var profile map[string]any
	// err сообщает об ошибке чтения полного примера без блока encoding.
	if err := apiAcceptanceDecodeJSON([]byte(`{"mode":"copy","format":{"video":{"codec":"av1","width":640,"height":360,"frame_rate":{"numerator":60000,"denominator":2002},"codec_profile":"main","level":"7.3","tier":"main","pixel_format":"yuv420p","bit_depth":8,"color":{"range":"pc","primaries":"bt709","transfer":"bt709","matrix":"bt709"}},"audio":{"codec":"aac","codec_profile":"LC","sample_rate_hz":48000,"channels":2,"channel_layout":"stereo"}},"video_encoding":{"target_bitrate_bps":4000000,"max_bitrate_bps":4000000,"buffer_bits":8000000,"gop_frames":17,"max_b_frames":0,"closed_gop":true},"audio_encoding":{"bitrate_bps":128000}}`), &profile); err != nil {
		t.Fatal(err)
	}
	return profile
}

// measuredVariantRepository предоставляет только синтетические сохранённые наблюдения HTTP-границе.
type measuredVariantRepository struct {
	// ArtifactRepository сохраняет неиспользуемые операции вне читаемого маршрута.
	core.ArtifactRepository
	// variant задаёт единственный результат текущего сценария измерений.
	variant core.FallbackVariant
}

// Asset возвращает родителя id, чтобы HTTP-маршрут проверил принадлежность варианта; ctx не используется.
func (r *measuredVariantRepository) Asset(ctx context.Context, id core.ID) (core.FallbackAsset, error) {
	return core.FallbackAsset{Record: core.Record{ID: id}, CurrentGenerationID: "generation"}, nil
}

// Generation возвращает поколение id с проверяемой принадлежностью к синтетическому Asset; ctx не используется.
func (r *measuredVariantRepository) Generation(ctx context.Context, id core.ID) (core.FallbackGeneration, error) {
	return core.FallbackGeneration{Record: core.Record{ID: id}, AssetID: "asset"}, nil
}

// Variants возвращает независимую копию единственного наблюдения без ввода-вывода; ctx и query ограничены тестом.
func (r *measuredVariantRepository) Variants(ctx context.Context, query core.ListQuery) ([]core.FallbackVariant, error) {
	return []core.FallbackVariant{r.variant}, nil
}

// measuredVariantWork допускает только чтение синтетического репозитория через настоящий Service.
type measuredVariantWork struct {
	// repository содержит результат измерений, меняемый только между завершёнными HTTP-запросами.
	repository *measuredVariantRepository
}

// View передаёт callback fn каталог измерений в контексте ctx без открытия SQLite.
func (w measuredVariantWork) View(ctx context.Context, fn func(core.Repositories) error) error {
	return fn(core.Repositories{Artifacts: w.repository})
}

// Write запрещает неожиданные записи callback fn; ctx не предоставляет дополнительных разрешений.
func (w measuredVariantWork) Write(ctx context.Context, fn func(core.Repositories) error) error {
	return core.ErrInvalid
}

// TestMeasuredVariantHTTPPreservesKnownAndClosesUnknownStrings проверяет wire JSON настоящего
// маршрута списка вариантов. t отличает PREPARING без измерения от FAILED с известными и неизвестными строками.
func TestMeasuredVariantHTTPPreservesKnownAndClosesUnknownStrings(t *testing.T) {
	// repository хранит синтетический raw-формат и позволяет проверить отсутствие его изменения проекцией.
	repository := &measuredVariantRepository{}
	// service предоставляет настоящие доменные операции чтения; err сообщает отказ создания.
	service, err := core.NewService(measuredVariantWork{repository}, "observed-owner")
	if err != nil {
		t.Fatal(err)
	}
	// fixture регистрирует настоящий API с изолированным каталогом наблюдений вместо диска;
	// callback config заменяет только доменный источник GET-маршрута измерений.
	fixture := newAPIAcceptanceFixture(t, func(config *api.Config) {
		// Callback заменяет только доменный источник GET-маршрута измерений.
		config.Core = service
	})
	// scenario задаёт независимые ожидаемые строки, включая неизвестные байты производителя.
	for _, scenario := range []struct {
		// name обозначает класс raw-измерения; raw содержит фактический формат; expected задаёт точные публичные строки.
		name     string
		raw      core.MediaFormat
		expected map[string]any
	}{
		{"empty", core.MediaFormat{}, map[string]any{"video.codec": "", "video.level": "", "audio.codec": ""}},
		{"recognized", core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecH264, CodecProfile: "high422", Level: "25.5", PixelFormat: "yuv422p12le", Color: core.ColorInfo{Range: "unspecified", Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc"}}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", ChannelLayout: "mono"}}, map[string]any{"video.codec": "h264", "video.codec_profile": "high422", "video.level": "25.5", "video.tier": "", "video.pixel_format": "yuv422p12le", "video.color.range": "unspecified", "video.color.primaries": "bt2020", "video.color.transfer": "smpte2084", "video.color.matrix": "bt2020nc", "audio.codec_profile": "LC", "audio.channel_layout": "mono"}},
		{"multichannel", core.MediaFormat{Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", Channels: 6, ChannelLayout: "5.1"}}, map[string]any{"audio.channel_layout": "5.1"}},
		{"wide", core.MediaFormat{Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", Channels: 8, ChannelLayout: "7.1(wide-back)"}}, map[string]any{"audio.channel_layout": "7.1(wide-back)"}},
		{"unknown", core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecAV1, CodecProfile: "high", Level: "25.5", Tier: "other", PixelFormat: "ffprobe-other", Color: core.ColorInfo{Primaries: string([]byte{0xff}), Transfer: "cicp:255", Matrix: "iso-255"}}, Audio: core.AudioFormat{Codec: "unknown-audio-codec", CodecProfile: "HE", ChannelLayout: "9.1"}}, map[string]any{"video.codec": "av1", "video.codec_profile": "high", "video.level": "UNKNOWN", "video.tier": "UNKNOWN", "video.pixel_format": "UNKNOWN", "video.color.range": "", "video.color.primaries": "UNKNOWN", "video.color.transfer": "UNKNOWN", "video.color.matrix": "UNKNOWN", "audio.codec": "UNKNOWN", "audio.codec_profile": "UNKNOWN", "audio.channel_layout": "UNKNOWN"}},
	} {
		// t.Run проверяет один законченный HTTP-запрос без параллельной смены репозитория;
		// t сообщает об ошибках конкретной проекции.
		t.Run(scenario.name, func(t *testing.T) {
			repository.variant = core.FallbackVariant{Record: core.Record{ID: "variant"}, AssetID: "asset", GenerationID: "generation", State: core.VariantFailed, Profile: apiAcceptanceProfile(), VerifiedFormat: scenario.raw}
			if scenario.name == "empty" {
				repository.variant.State = core.VariantPreparing
			}
			// before фиксирует все исходные байты raw-формата до публичной проекции.
			before := repository.variant.VerifiedFormat
			// page и format содержат реально сериализованное представление измерения.
			page := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", "/fallback-assets/asset/variants", "", 200))
			format := page["items"].([]any)[0].(map[string]any)["format"].(map[string]any)
			// path и expected сопоставляют каждый проверяемый публичный путь с независимым ожиданием.
			for path, expected := range scenario.expected {
				// actual содержит значение публичного поля после настоящей HTTP-сериализации.
				if actual := numericAt(t, format, path); actual != expected {
					t.Fatalf("%s: получено %v, ожидается %v", path, actual, expected)
				}
			}
			if repository.variant.VerifiedFormat != before {
				t.Fatal("HTTP-проекция изменила сохранённое наблюдение")
			}
		})
	}
}

// TestSSEFFmpegStateUsesOwnerSpecificCatalog проверяет все шесть состояний наблюдения FFmpeg
// в настоящих SSE-кадрах без процесса кодировщика. t сверяет owner, role и неприменимый source.
func TestSSEFFmpegStateUsesOwnerSpecificCatalog(t *testing.T) {
	// fixture и attempt создают настоящий доменный запуск с синтетическим наблюдателем FFmpeg.
	fixture, _, attempt := apiAcceptanceFFmpegAttempt(t)
	// state перечисляет контрактные состояния наблюдения независимо от каталога реализации.
	for _, state := range []string{"UNKNOWN", "STARTING", "READY", "RETRYING", "FAILED", "STOPPED"} {
		// anchor фиксирует позицию до публикации очередного состояния.
		anchor := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", "/events/snapshot", "", 200))
		// err сообщает об отказе синтетического наблюдения без внешнего процесса.
		if err := attempt.Observe(context.Background(), core.FFmpegObservation{State: core.ObservationState(state)}); err != nil {
			t.Fatal(err)
		}
		// stream принимает реальные кадры; cancel и done ограничивают срок каждой подписки.
		stream := newAPIAcceptanceStreamWriter()
		cancel, done := apiAcceptanceStartStream(t, fixture, "/events?cursor="+url.QueryEscape(anchor["cursor"].(string)), stream)
		// event содержит wire data именно владельца ffmpeg и события state.
		event := numericAwaitEvent(t, stream, "state", "ffmpeg")
		cancel()
		apiAcceptanceAwait(t, done)
		if event["role"] != "live_transcode" || event["source"] != "" || event["observation"].(map[string]any)["state"] != state {
			t.Fatal("Неверный каталог строки SSE для владельца FFmpeg", state, event)
		}
	}
}

// strictSetField заменяет поле по публичному dotted-пути path в объекте value.
// replacement может быть любым JSON-значением для проверки типа, null или enum.
func strictSetField(value map[string]any, path string, replacement any) {
	// segments разделяет путь, а index и segment выбирают его очередную часть.
	segments := strings.Split(path, ".")
	for index, segment := range segments {
		if index == len(segments)-1 {
			value[segment] = replacement
			return
		}
		value = value[segment].(map[string]any)
	}
}

// TestProfileWireRejectsUnsafeSettingsOnCreateAndReplace проверяет реальные
// POST/PATCH: отсутствие encoding и encoding:null не обходят проверку строки,
// типа или Base64. t регистрирует статусы, утечки и изменение исходной записи.
func TestProfileWireRejectsUnsafeSettingsOnCreateAndReplace(t *testing.T) {
	// fixture связывает настоящий HTTP-маршрут с временной SQLite.
	fixture := newAPIAcceptanceFixture(t, nil)
	// created сохраняет допустимый исходный ресурс для всех отрицательных PATCH.
	created := encodingCreate(t, fixture, strictWireProfile(t))
	// path адресует ресурс, который обязан остаться неизменным после всех отказов.
	path := "/transcode-profiles/" + created["id"].(string)
	// field выбирает каждое строковое поле заданного медиаформата.
	for _, field := range []string{"mode", "live_audio_mode", "format.video.codec", "format.video.codec_profile", "format.video.level", "format.video.tier", "format.video.pixel_format", "format.video.color.range", "format.video.color.primaries", "format.video.color.transfer", "format.video.color.matrix", "format.audio.codec", "format.audio.codec_profile", "format.audio.channel_layout"} {
		// invalid задаёт разные классы недопустимого публичного значения.
		for _, invalid := range []any{"SECRET_UNSUPPORTED_ENUM_VALUE", nil, json.Number("17"), map[string]any{"encoding": "base64", "value": "U0VDUkVUX1VOU1VQUE9SVEVEX0VOVU1fVkFMVUU="}} {
			// explicitNull выбирает оба разрешённых способа отсутствия Encoding.
			for _, explicitNull := range []bool{false, true} {
				// profile содержит отдельную полную копию входного профиля.
				profile := strictWireProfile(t)
				strictSetField(profile, field, invalid)
				if explicitNull {
					profile["encoding"] = nil
				}
				// operation выбирает создание либо полную замену с той же ошибкой.
				for _, operation := range []string{"POST", "PATCH"} {
					// body содержит ошибочную настройку; target выбирает маршрут операции.
					body := map[string]any{"name": "Проверка", "profile": profile}
					target := "/transcode-profiles"
					if operation == "PATCH" {
						body["expected_revision"], target = json.Number("1"), path
					}
					// response содержит безопасный 422 настоящего обработчика.
					response := apiAcceptanceRequest(t, fixture.api, operation, target, encodingJSON(t, body), 422)
					if !strings.HasPrefix(response.Header().Get("Content-Type"), "application/problem+json") || strings.Contains(response.Body.String(), "SECRET_UNSUPPORTED_ENUM_VALUE") || strings.Contains(response.Body.String(), "U0VDUkVU") {
						t.Fatalf("Небезопасный отказ %s %s: %s", operation, field, response.Body)
					}
				}
			}
		}
	}
	// current проверяет отсутствие побочного повышения ревизии и изменения полей.
	current := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", path, "", 200))
	if !reflect.DeepEqual(current, created) {
		t.Fatal("Отклонённая настройка изменила сохранённый профиль", current, created)
	}
}

// TestProfileWireRejectsMalformedUTF8WithoutReplacement проверяет сырые байты
// HTTP-тела: json.Marshal не должен предварительно заменить повреждённый UTF-8.
// t сообщает о принятии произвольных байтов или раскрытии исходной настройки.
func TestProfileWireRejectsMalformedUTF8WithoutReplacement(t *testing.T) {
	// fixture содержит настоящий API на отдельной SQLite.
	fixture := newAPIAcceptanceFixture(t, nil)
	// invalid выбирает незаконный байт и непарный Unicode surrogate.
	for _, invalid := range []string{string([]byte{0xff}), `\ud800`} {
		// body содержит недопустимый фрагмент внутри JSON-строки level.
		body := encodingJSON(t, map[string]any{"name": "Проверка", "profile": strictWireProfile(t)})
		body = strings.Replace(body, `"level":"7.3"`, `"level":"`+invalid+`"`, 1)
		// request передаёт точные байты HTTP-тела, response сохраняет результат.
		request := httptest.NewRequest(http.MethodPost, api.Prefix+"/transcode-profiles", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		fixture.api.ServeHTTP(response, request)
		if response.Code != 422 || strings.Contains(response.Body.String(), `\ufffd`) || strings.Contains(response.Body.String(), `\ud800`) {
			t.Fatalf("Повреждённая строка получила status=%d: %s", response.Code, response.Body)
		}
	}
}

// TestProfileWirePreservesPrecisionAndStringAliases проверяет реальные
// POST→GET→PATCH без изменения несокращённой дроби и полного диапазона uint64.
// t сообщает о потере чисел, строковых значений или текущих настроек.
func TestProfileWirePreservesPrecisionAndStringAliases(t *testing.T) {
	// fixture содержит независимый API для допустимого полного цикла.
	fixture := newAPIAcceptanceFixture(t, nil)
	// input сохраняет точные исходные JSON-значения профиля.
	input := strictWireProfile(t)
	// created содержит фактическую форму сохранённого профиля.
	created := encodingCreate(t, fixture, input)
	input["encoding"], input["live_audio_mode"] = created["profile"].(map[string]any)["encoding"], "copy"
	if !reflect.DeepEqual(created["profile"], input) {
		t.Fatal("Создание изменило legacy-параметры", created["profile"], input)
	}
	// path адресует созданный ресурс для чтения и полной замены.
	path := "/transcode-profiles/" + created["id"].(string)
	// read содержит фактическое чтение исходного профиля.
	read := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", path, "", 200))
	if !reflect.DeepEqual(read, created) {
		t.Fatal("GET изменил точные параметры", read, created)
	}
	// changed содержит ответ полной замены с теми же точными числами и строками.
	changed := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "PATCH", path, encodingJSON(t, map[string]any{"expected_revision": 1, "profile": input}), 200))
	if !reflect.DeepEqual(changed["profile"], input) || changed["revision"] != json.Number("2") {
		t.Fatal("PATCH изменил точные параметры", changed)
	}
}

// contractSchemaAt проходит path от именованной схемы в schemas, раскрывая
// внутренние ссылки и элементы массивов. t завершает тест при неверном пути.
func contractSchemaAt(t *testing.T, schemas map[string]any, path string) map[string]any {
	t.Helper()
	// parts содержит имя корневой схемы и последовательность свойств.
	parts := strings.Split(path, ".")
	// current хранит схему текущего узла пути.
	current := schemas[parts[0]].(map[string]any)
	// part выбирает дочернее свойство либо специальный переход items массива.
	for _, part := range parts[1:] {
		// reference адресует именованную схему OpenAPI; ok отличает ссылку от встроенного описания.
		if reference, ok := current["$ref"].(string); ok {
			current = schemas[strings.TrimPrefix(reference, "#/components/schemas/")].(map[string]any)
		}
		if part == "items" {
			current = current["items"].(map[string]any)
			continue
		}
		// properties содержит конечные свойства текущего объекта.
		properties := current["properties"].(map[string]any)
		current = properties[part].(map[string]any)
	}
	return current
}

// TestPublicSchemasCloseDesiredAndHistoricalStrings проверяет опубликованные
// enum независимо от доменного каталога, включая вложенные профили и наблюдения.
// t фиксирует лишнее значение, исчезновение пустоты или возврат Base64 union.
func TestPublicSchemasCloseDesiredAndHistoricalStrings(t *testing.T) {
	// fixture даёт реальный генерируемый OpenAPI зарегистрированного API.
	fixture := newAPIAcceptanceFixture(t, nil)
	// document и schemas содержат фактически выданную схему, а не вызов TransformSchema напрямую.
	document := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", "/openapi.json", "", 200))
	schemas := document["components"].(map[string]any)["schemas"].(map[string]any)
	// expectations задаёт независимые точные строковые множества из публичного контракта.
	expectations := map[string]string{
		"GroupProfileInput.mode":         "copy,transcode",
		"VideoFormatDTO.codec":           "av1,h264,hevc,vp9",
		"VideoFormatDTO.codec_profile":   "main,baseline,high,constrained-baseline,high10,high422,high444,professional,main10,profile0,profile2",
		"VideoFormatDTO.tier":            ",main,high",
		"VideoFormatDTO.pixel_format":    "yuv420p,yuv420p10le,yuv422p,yuv422p10le,yuv444p,yuv444p10le,gray",
		"ColorDTO.range":                 "limited,full,tv,pc",
		"ColorDTO.primaries":             "bt709,bt470m,bt470bg,smpte170m,smpte240m,film,bt2020",
		"ColorDTO.transfer":              "bt709,gamma22,gamma28,smpte170m,smpte240m,linear,iec61966-2-1,bt2020-10,bt2020-12,smpte2084,arib-std-b67",
		"ColorDTO.matrix":                "bt709,fcc,bt470bg,smpte170m,smpte240m,bt2020nc,bt2020c",
		"AudioFormatDTO.codec":           "aac,opus,flac,mp3,ac3",
		"AudioFormatDTO.codec_profile":   "LC,celt,s16,mpeg1-layer3,ac3",
		"AudioFormatDTO.channel_layout":  "mono,stereo,2.1,3.0,4.0,5.0,5.1,7.1",
		"SessionDTO.state":               "STARTING,RUNNING,STOPPING,STOPPED,FAILED,INTERRUPTED",
		"ReceiptDTO.kind":                "start_session,stop_session,pause_session,resume_session,add_output,start_output,stop_output,detach_output",
		"ReceiptDTO.status":              "PENDING,CLAIMED,ISSUED,SUCCEEDED,FAILED,INTERRUPTED",
		"AssetDTO.kind":                  ",image,video",
		"AssetDTO.state":                 "PREPARING,READY,FAILED",
		"GenerationDTO.kind":             "image,video",
		"GenerationDTO.status":           "queued,preparing,ready,error",
		"VariantDTO.state":               "PREPARING,READY,FAILED",
		"RuntimeGroupDTO.state":          "UNKNOWN,STARTING,READY,RETRYING,FAILED,STOPPED",
		"RuntimeOutputDTO.state":         "UNKNOWN,CONNECTING,PUBLISHING,RETRYING,FAILED,STOPPED",
		"RuntimeFFmpegDTO.role":          ",live_transcode,fallback_player",
		"RuntimeGroupDTO.source":         "NONE,LIVE,FALLBACK",
		"RuntimeEventDTO.kind":           "state,metrics,input,gpu,removed,attempt",
		"RuntimeEventDTO.owner":          "session,group,output,ffmpeg,system",
		"RuntimeEventDTO.source":         ",NONE,LIVE,FALLBACK",
		"EventObservationDTO.state":      ",UNKNOWN,STARTING,READY,RETRYING,FAILED,STOPPED,CONNECTING,PUBLISHING",
		"ResyncRequiredDTO.reason":       "gap,reset,frame_limit,source_unavailable",
		"CapabilityDTO.status":           "available,unavailable,unverified",
		"CapabilityDTO.reason":           "not_configured,executable_unavailable,executable_resolved,hardware_not_verified,unsupported_platform",
		"HealthDTO.reason":               "",
		"MeasuredVideoDTO.codec":         ",UNKNOWN,av1,h264,hevc,vp9",
		"MeasuredVideoDTO.codec_profile": ",UNKNOWN,baseline,constrained-baseline,main,extended,high,high10,high422,high444,professional,main10,profile0,profile1,profile2,profile3",
		"MeasuredAudioDTO.codec":         ",UNKNOWN,aac,opus,flac,mp3,ac3",
		"MeasuredAudioDTO.codec_profile": ",UNKNOWN,LC,celt,s16,mpeg1-layer3,ac3",
		"ObservedFormatDTO.audio_codec":  ",UNKNOWN,aac,opus,flac,mp3,ac3",
		"MeasuredColorDTO.range":         ",UNKNOWN,limited,full,tv,pc,unspecified",
		"SessionDTO.groups.items.profile.profile.format.audio.channel_layout": "mono,stereo,2.1,3.0,4.0,5.0,5.1,7.1",
		"OutputDTO.profile.profile.format.video.pixel_format":                 "yuv420p,yuv420p10le,yuv422p,yuv422p10le,yuv444p,yuv444p10le,gray",
		"VariantDTO.profile.format.video.color.matrix":                        "bt709,fcc,bt470bg,smpte170m,smpte240m,bt2020nc,bt2020c",
	}
	// path называет публичный узел; values содержит ожидаемые строки без зависимости от их порядка.
	for path, values := range expectations {
		// property содержит схему именно проверяемого поля.
		property := contractSchemaAt(t, schemas, path)
		if property["type"] != "string" || property["oneOf"] != nil || property["anyOf"] != nil {
			t.Fatalf("%s перестал быть обычной строкой: %#v", path, property)
		}
		// actual собирает выданный enum, expected задаёт независимый ожидаемый набор.
		actual := []string{}
		expected := strings.Split(values, ",")
		// value содержит один элемент фактического enum.
		for _, value := range property["enum"].([]any) {
			actual = append(actual, value.(string))
		}
		slices.Sort(actual)
		slices.Sort(expected)
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("%s: enum=%q, ожидается %q", path, actual, expected)
		}
	}
	// numericProperty проверяет, что строковая реорганизация не округлила maximum uint64.
	numericProperty := contractSchemaAt(t, schemas, "GroupProfileDTO.video_encoding.target_bitrate_bps")
	if numericProperty["maximum"] != json.Number("18446744073709551615") {
		t.Fatal("Схема потеряла полный uint64", numericProperty)
	}
}

// TestSSESystemEventPreservesInapplicableStrings проверяет реальный SSE-кадр
// системного наблюдения: неприменимые state/source/role остаются пустыми,
// GPU metric status остаётся непустым; REST и SSE дают одинаковые значения.
// t сообщает о нарушении enum или потере согласованности транспортных форм.
func TestSSESystemEventPreservesInapplicableStrings(t *testing.T) {
	// fixture и observer создают настоящую RuntimeAuthority без GPU или NVML.
	fixture := newAPIAcceptanceFixture(t, nil)
	observer := apiAcceptanceSystem(t, fixture)
	// anchor содержит позицию до публикации измерения.
	anchor := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", "/events/snapshot", "", 200))
	// sample задаёт синтетические доступные и unsupported поля с допустимыми причинами.
	sample := apiAcceptanceGPU(9007199254740993, time.Now().UTC())
	// err содержит отказ публикации синтетического наблюдения в настоящую RuntimeAuthority.
	if err := observer.ObserveGPU(context.Background(), sample); err != nil {
		t.Fatal(err)
	}
	// rest содержит тот же системный снимок по настоящему HTTP-маршруту.
	rest := apiAcceptanceJSON(t, apiAcceptanceRequest(t, fixture.api, "GET", "/events/snapshot", "", 200))
	// stream принимает настоящие SSE-кадры; cancel и done обеспечивают завершение подписки.
	stream := newAPIAcceptanceStreamWriter()
	cancel, done := apiAcceptanceStartStream(t, fixture, "/events?cursor="+url.QueryEscape(anchor["cursor"].(string)), stream)
	// event содержит data завершённого кадра GPU, а не проекцию DTO напрямую.
	event := numericAwaitEvent(t, stream, "gpu", "system")
	cancel()
	apiAcceptanceAwait(t, done)
	if event["kind"] != "gpu" || event["owner"] != "system" || event["source"] != "" || event["role"] != "" || event["observation"].(map[string]any)["state"] != "" {
		t.Fatal("Неприменимые строки события изменены", event)
	}
	if !reflect.DeepEqual(event["gpu"], rest["gpu"]) {
		t.Fatal("GPU enum или числа расходятся между REST и SSE")
	}
	// gpu и device адресуют конкретные статусы и причины без предположения о наличии всех измерений.
	gpu := event["gpu"].(map[string]any)
	device := gpu["devices"].([]any)[0].(map[string]any)
	if gpu["status"] != "available" || device["utilization"].(map[string]any)["status"] != "available" || device["encoder_utilization"].(map[string]any)["status"] != "unsupported" || device["encoder_utilization"].(map[string]any)["reason"] != "not_supported" {
		t.Fatal("Потеряны конечные GPU статусы или причины", gpu)
	}
	sample.Status = ""
	// err должен сообщить о недопустимом пустом статусе системного наблюдения.
	if err := observer.ObserveGPU(context.Background(), sample); err == nil {
		t.Fatal("Пустой GPU MetricStatus принят")
	}
}
