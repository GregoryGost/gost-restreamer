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

func TestActiveSessionAndOutputResponsesKeepOriginalProfileVersions(t *testing.T) {
	f, preparation, _, release := newAPIAcceptanceUpload(t, time.Second)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	asset, err := f.service.CreateAsset(ctx, "Заглушка")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := f.service.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "synthetic-publication-secret", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	legacyProfile := apiAcceptanceProfile()
	legacyProfile.Format.Video.FrameRate = core.FrameRate{Numerator: 30, Denominator: 1}
	legacy, err := f.service.CreateProfile(ctx, core.TranscodeProfile{Name: "Прежний", Profile: legacyProfile})
	if err != nil {
		t.Fatal(err)
	}
	legacyWire := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/transcode-profiles/"+string(legacy.ID), "", 200))
	modernWire := encodingCreate(t, f, encodingRequestProfile(t, "transcode", "vbr"))
	ids := []core.ID{legacy.ID, core.ID(modernWire["id"].(string))}
	before := map[string]any{string(legacy.ID): legacyWire["profile"], string(ids[1]): modernWire["profile"]}
	for n, id := range ids {
		group, err := f.service.CreateGroup(ctx, core.TranscodeGroup{Name: fmt.Sprintf("Группа %d", n), StreamID: stream.ID, ProfileID: id})
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.service.CreateDestination(ctx, core.Destination{Name: fmt.Sprintf("Выход %d", n), StreamID: stream.ID, GroupID: group.ID, Platform: "custom", Endpoint: "rtmp://localhost/unused", StreamKey: "synthetic-stream-secret", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
	}
	body := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	apiAcceptanceUploadRequest(t, f, ctx, string(asset.ID), asset.Revision, body, 202)
	if _, err = (<-preparation.jobs).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	bindings, err := f.service.SelectStartBindings(ctx, stream.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := f.service.StartSession(ctx, core.StartSessionRequest{RequestID: "start-original-settings", StreamID: stream.ID, Bindings: bindings})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		p := encodingRequestProfile(t, "transcode", "cbr")
		apiAcceptanceRequest(t, f.api, "PATCH", "/transcode-profiles/"+string(id), encodingJSON(t, map[string]any{"expected_revision": "1", "profile": p}), 200)
	}
	path := "/sessions/" + string(receipt.SessionID)
	session := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", path, "", 200))
	if session["state"] != "STARTING" {
		t.Fatal("Сессия не активна:", session)
	}
	groups := session["groups"].([]any)
	if len(groups) != 2 {
		t.Fatal("Потеряны снимки групп:", groups)
	}
	for _, group := range groups {
		profile := group.(map[string]any)["profile"].(map[string]any)
		if !reflect.DeepEqual(profile["profile"], before[profile["id"].(string)]) || profile["revision"] != "1" {
			t.Fatal("GET Session заменил снимок текущей конфигурацией:", profile)
		}
	}
	page := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", path+"/outputs", "", 200))
	outputs := page["items"].([]any)
	if len(outputs) != 2 {
		t.Fatal("Потеряны снимки выходов:", outputs)
	}
	for _, item := range outputs {
		output := item.(map[string]any)
		for _, response := range []map[string]any{output, apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", path+"/outputs/"+output["id"].(string), "", 200))} {
			profile := response["profile"].(map[string]any)
			if !reflect.DeepEqual(profile["profile"], before[profile["id"].(string)]) || profile["revision"] != "1" {
				t.Fatal("GET Output заменил снимок текущей конфигурацией:", profile)
			}
		}
	}
}
