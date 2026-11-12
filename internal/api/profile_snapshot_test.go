package api_test

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет, что активные Session и Output сохраняют первоначальные снимки как прежних, так и новых
// профилей после изменения текущих настроек.
func TestActiveSessionAndOutputResponsesKeepOriginalProfileVersions(t *testing.T) {
	// f содержит API и SQLite; preparation принимает настоящие задания подготовки; release
	// освобождает удерживаемую тестовую подготовку при завершении.
	f, preparation, _, release := newAPIAcceptanceUpload(t, time.Second)
	defer release()
	// ctx ограничивает подготовку и проверку снимков пятью секундами; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// asset содержит заглушку для снимков обоих профилей; err содержит отказ создания.
	asset, err := f.service.CreateAsset(ctx, "Заглушка")
	if err != nil {
		t.Fatal(err)
	}
	// stream содержит поток, запускаемый с прежним и новым профилями одновременно.
	stream, err := f.service.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "synthetic-publication-secret", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	// legacyProfile задаёт прежний видеорецепт с согласованной частотой 30/1.
	legacyProfile := apiAcceptanceProfile()
	legacyProfile.Format.Video.FrameRate = core.FrameRate{Numerator: 30, Denominator: 1}
	// legacy содержит вставленный прежний профиль без назначения новых настроек.
	legacy := apiAcceptanceInsertLegacyProfile(t, f, legacyProfile)
	// legacyWire сохраняет публичное представление прежнего профиля до запуска.
	legacyWire := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/transcode-profiles/"+string(legacy.ID), "", 200))
	// modernWire содержит созданный профиль версии 2 до запуска.
	modernWire := encodingCreate(t, f, encodingRequestProfile(t, "transcode", "vbr"))
	// ids содержит идентификаторы прежнего и нового профилей для параллельных групп одного запуска.
	ids := []core.ID{legacy.ID, core.ID(modernWire["id"].(string))}
	// before сохраняет исходные полные профили по ID для сравнения после изменения текущих настроек.
	before := map[string]any{string(legacy.ID): legacyWire["profile"], string(ids[1]): modernWire["profile"]}
	// n задаёт номер создаваемой группы; id выбирает профиль этой группы.
	for n, id := range ids {
		// group содержит созданную группу выбранного профиля; err содержит отказ создания.
		group, err := f.service.CreateGroup(ctx, core.TranscodeGroup{Name: fmt.Sprintf("Группа %d", n), StreamID: stream.ID, ProfileID: id})
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.service.CreateDestination(ctx, core.Destination{Name: fmt.Sprintf("Выход %d", n), StreamID: stream.ID, GroupID: group.ID, Platform: "custom", Endpoint: "rtmp://localhost/unused", StreamKey: "synthetic-stream-secret", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
	}
	// body передаёт настоящий PNG для подготовки вариантов обоих профилей.
	body := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	apiAcceptanceUploadRequest(t, f, ctx, string(asset.ID), asset.Revision, body, 202)
	if _, err = (<-preparation.jobs).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	// bindings содержит выбранные доменом настройки запуска с подготовленными вариантами.
	bindings, err := f.service.SelectStartBindings(ctx, stream.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	// receipt содержит принятую команду запуска с самостоятельными снимками профилей.
	receipt, err := f.service.StartSession(ctx, core.StartSessionRequest{RequestID: "start-original-settings", StreamID: stream.ID, Bindings: bindings})
	if err != nil {
		t.Fatal(err)
	}
	// id содержит очередной текущий профиль, который заменяется после фиксации снимков Session.
	for _, id := range ids {
		// p содержит новый CBR-профиль с явным LIVE transcode для изменения текущей
		// конфигурации.
		p := encodingRequestProfile(t, "transcode", "cbr")
		p["live_audio_mode"] = "transcode"
		apiAcceptanceRequest(t, f.api, "PATCH", "/transcode-profiles/"+string(id), encodingJSON(t, map[string]any{"expected_revision": "1", "profile": p}), 200)
	}
	// path адресует созданную Session для чтения снимков запуска и выходов.
	path := "/sessions/" + string(receipt.SessionID)
	// session содержит сохранённую STARTING Session после изменения текущих профилей.
	session := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", path, "", 200))
	if session["state"] != "STARTING" {
		t.Fatal("Сессия не активна:", session)
	}
	// groups содержит публичные снимки двух групп из Session.
	groups := session["groups"].([]any)
	if len(groups) != 2 {
		t.Fatal("Потеряны снимки групп:", groups)
	}
	// group содержит очередной снимок группы запуска.
	for _, group := range groups {
		// profile содержит профиль из снимка группы для сравнения с исходным before.
		profile := group.(map[string]any)["profile"].(map[string]any)
		if !reflect.DeepEqual(profile["profile"], before[profile["id"].(string)]) || profile["revision"] != "1" {
			t.Fatal("GET Session заменил снимок текущей конфигурацией:", profile)
		}
	}
	// page содержит страницу сохранённых выходов запуска.
	page := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", path+"/outputs", "", 200))
	// outputs содержит оба выхода с самостоятельными профилями запуска.
	outputs := page["items"].([]any)
	if len(outputs) != 2 {
		t.Fatal("Потеряны снимки выходов:", outputs)
	}
	// item содержит очередной выход из страницы.
	for _, item := range outputs {
		// output содержит публичный объект текущего выхода для чтения его подробного маршрута.
		output := item.(map[string]any)
		// response содержит страничное либо отдельное представление того же выхода.
		for _, response := range []map[string]any{output, apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", path+"/outputs/"+output["id"].(string), "", 200))} {
			// profile содержит самостоятельный профиль выхода для проверки сохранности
			// исходного рецепта и ревизии.
			profile := response["profile"].(map[string]any)
			if !reflect.DeepEqual(profile["profile"], before[profile["id"].(string)]) || profile["revision"] != "1" {
				t.Fatal("GET Output заменил снимок текущей конфигурацией:", profile)
			}
		}
	}
}
