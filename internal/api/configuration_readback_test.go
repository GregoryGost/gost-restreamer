package api_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

func readbackObject(t *testing.T, handler http.Handler, method, path, body string, status int) map[string]any {
	t.Helper()
	w := apiAcceptanceRequest(t, handler, method, path, body, status)
	apiAcceptanceNoServiceFields(t, w.Body.String())
	value := apiAcceptanceJSON(t, w)
	readbackNoConfiguredFlags(t, value)
	return value
}

func readbackNoConfiguredFlags(t *testing.T, value any) {
	t.Helper()
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if key == "publication_key_configured" || key == "endpoint_configured" || key == "stream_key_configured" {
				t.Fatalf("удалённый флаг присутствует: %s", key)
			}
			readbackNoConfiguredFlags(t, child)
		}
	case []any:
		for _, child := range v {
			readbackNoConfiguredFlags(t, child)
		}
	}
}

func readbackFields(t *testing.T, value map[string]any, fields map[string]string) {
	t.Helper()
	for field, want := range fields {
		got, present := value[field].(string)
		if !present || got != want {
			t.Fatalf("поле %s: got=%#v, want=%q", field, value[field], want)
		}
	}
}

func readbackItems(t *testing.T, value map[string]any, count int) []map[string]any {
	t.Helper()
	items, ok := value["items"].([]any)
	if !ok || len(items) != count {
		t.Fatalf("неверное число записей: %#v, want=%d", value, count)
	}
	result := make([]map[string]any, 0, count)
	for _, item := range items {
		result = append(result, item.(map[string]any))
	}
	return result
}

func TestAcceptanceConfigurationReturnsExactSavedKeysAndFullEndpoints(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	p, err := f.service.CreateProfile(context.Background(), core.TranscodeProfile{Name: "Профиль чтения", Profile: apiAcceptanceProfile()})
	if err != nil {
		t.Fatal(err)
	}
	streams := map[string]string{}
	destinations := map[string]map[string]string{}
	for i, key := range []string{"synthetic-stream-key/+?a=1&b=%2F", ""} {
		publicationKey := fmt.Sprintf("synthetic-publication-%d/+?token=exact", i)
		s := readbackObject(t, f.api, "POST", "/streams", fmt.Sprintf(`{"name":"Поток %d","publication_key":%q}`, i, publicationKey), 201)
		id := s["id"].(string)
		streams[id] = publicationKey
		readbackFields(t, s, map[string]string{"publication_key": publicationKey})
		s = readbackObject(t, f.api, "PATCH", "/streams/"+id, `{"expected_revision":"1","name":"Новое имя"}`, 200)
		readbackFields(t, s, map[string]string{"publication_key": publicationKey, "name": "Новое имя"})
		g, err := f.service.CreateGroup(context.Background(), core.TranscodeGroup{Name: "Группа чтения", StreamID: core.ID(id), ProfileID: p.ID})
		if err != nil {
			t.Fatal(err)
		}
		endpoint := fmt.Sprintf("rtmps://synthetic-user-%d:p%%40ss@localhost:1935/private-path/%d?token=synthetic-url-%d&next=%%2Flive", i, i, i)
		d := readbackObject(t, f.api, "POST", "/destinations", fmt.Sprintf(`{"stream_id":%q,"group_id":%q,"name":"Назначение","platform":"custom","endpoint":%q,"stream_key":%q,"enabled":false}`, id, g.ID, endpoint, key), 201)
		destinationID := d["id"].(string)
		fields := map[string]string{"endpoint": endpoint, "stream_key": key, "stream_id": id}
		readbackFields(t, d, fields)
		readbackFields(t, readbackObject(t, f.api, "GET", "/destinations/"+destinationID, "", 200), fields)
		readbackFields(t, readbackItems(t, readbackObject(t, f.api, "GET", "/streams/"+id+"/destinations", "", 200), 1)[0], fields)
		patchedKey := ""
		if key == "" {
			patchedKey = "synthetic-patched-key/+?exact=%2F"
		}
		endpoint += "&patched=true"
		d = readbackObject(t, f.api, "PATCH", "/destinations/"+destinationID, fmt.Sprintf(`{"expected_revision":"1","endpoint":%q,"stream_key":%q}`, endpoint, patchedKey), 200)
		fields = map[string]string{"endpoint": endpoint, "stream_key": patchedKey, "stream_id": id}
		readbackFields(t, d, fields)
		destinations[destinationID] = fields
		for _, path := range []string{"/destinations/" + destinationID, "/streams/" + id + "/destinations"} {
			v := readbackObject(t, f.api, "GET", path, "", 200)
			if strings.HasSuffix(path, "/destinations") {
				v = readbackItems(t, v, 1)[0]
			}
			readbackFields(t, v, fields)
		}
		readbackFields(t, readbackObject(t, f.api, "GET", "/streams/"+id, "", 200), map[string]string{"publication_key": publicationKey})
	}
	for _, s := range readbackItems(t, readbackObject(t, f.api, "GET", "/streams", "", 200), 2) {
		key, ok := streams[s["id"].(string)]
		if !ok {
			t.Fatal("список вернул неизвестный поток", s)
		}
		readbackFields(t, s, map[string]string{"publication_key": key})
		delete(streams, s["id"].(string))
	}
	for _, d := range readbackItems(t, readbackObject(t, f.api, "GET", "/destinations", "", 200), 2) {
		fields, ok := destinations[d["id"].(string)]
		if !ok {
			t.Fatal("список вернул неизвестное назначение", d)
		}
		readbackFields(t, d, fields)
		delete(destinations, d["id"].(string))
	}
	if len(streams) != 0 || len(destinations) != 0 {
		t.Fatal("списки потеряли сохранённые записи")
	}
}

func readbackSeedHistory(t *testing.T, f *apiAcceptanceFixture, s core.Stream, p core.TranscodeProfile, g core.TranscodeGroup, d core.Destination) (core.Session, core.SessionOutput) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	session := core.Session{Record: core.Record{ID: "saved-session", Revision: 1, CreatedAt: now, UpdatedAt: now}, StreamID: s.ID, Incarnation: f.store.Incarnation(), State: core.SessionStopped, Lifecycle: 1, Snapshot: core.SessionSnapshot{Stream: s, Groups: []core.GroupSnapshot{{Group: g, Profile: p}}}, EndedAt: &now}
	key, canonical, err := core.ProfileIdentity(p.Profile)
	if err != nil {
		t.Fatal(err)
	}
	output := core.SessionOutput{Record: core.Record{ID: "saved-output", Revision: 1, CreatedAt: now, UpdatedAt: now}, SessionID: session.ID, Generation: 1, Snapshot: core.OutputSnapshot{Destination: d, Group: g, Profile: p, Fallback: s.Fallback, Artifact: core.ArtifactManifest{GenerationID: "saved-generation", VariantID: "saved-variant", AssetID: s.Fallback.AssetID, SourceHash: "synthetic-source-hash", Profile: p.Profile, ProfileKey: key, CanonicalProfile: canonical, GeneratorVersion: "synthetic-generator", ManagedPath: "saved/variant.mp4", SizeBytes: 100, ContentHash: "synthetic-content-hash", VerifiedFormat: p.Profile.Format}}}
	if err := f.store.Write(context.Background(), func(r core.Repositories) error {
		if err := r.Runtime.InsertSession(context.Background(), session); err != nil {
			return err
		}
		return r.Runtime.InsertOutput(context.Background(), output)
	}); err != nil {
		t.Fatal(err)
	}
	return session, output
}

func readbackHistory(t *testing.T, handler http.Handler, s core.Session, o core.SessionOutput, streamExists bool) {
	t.Helper()
	sessionPath := "/sessions/" + string(s.ID)
	sessions := []map[string]any{readbackObject(t, handler, "GET", sessionPath, "", 200)}
	sessions = append(sessions, readbackItems(t, readbackObject(t, handler, "GET", "/sessions", "", 200), 1)...)
	if streamExists {
		sessions = append(sessions, readbackItems(t, readbackObject(t, handler, "GET", "/streams/"+string(s.StreamID)+"/sessions", "", 200), 1)...)
	}
	for _, v := range sessions {
		stream := v["stream"].(map[string]any)
		readbackFields(t, stream, map[string]string{"id": string(s.StreamID), "publication_key": s.Snapshot.Stream.PublicationKey, "name": s.Snapshot.Stream.Name, "revision": fmt.Sprint(s.Snapshot.Stream.Revision)})
		policy := stream["fallback"].(map[string]any)
		readbackFields(t, policy, map[string]string{"asset_id": string(s.Snapshot.Stream.Fallback.AssetID)})
		readbackFields(t, policy["watchdog"].(map[string]any), map[string]string{"video_timeout_ns": fmt.Sprint(s.Snapshot.Stream.Fallback.Watchdog.VideoTimeout.Nanoseconds())})
	}
	outputs := readbackItems(t, readbackObject(t, handler, "GET", sessionPath+"/outputs", "", 200), 1)
	outputs = append(outputs, readbackObject(t, handler, "GET", sessionPath+"/outputs/"+string(o.ID), "", 200))
	for _, v := range outputs {
		readbackFields(t, v["destination"].(map[string]any), map[string]string{"id": string(o.Snapshot.Destination.ID), "endpoint": o.Snapshot.Destination.Endpoint, "stream_key": o.Snapshot.Destination.StreamKey, "revision": "1"})
		readbackFields(t, v["fallback"].(map[string]any), map[string]string{"asset_id": string(o.Snapshot.Fallback.AssetID)})
	}
}

func TestAcceptanceSavedConfigurationAndHistorySurviveReopenChangesAndDeletion(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	s, p, g, d := apiAcceptanceSeedConfiguration(t, f)
	asset, err := f.service.CreateAsset(context.Background(), "Заглушка снимка")
	if err != nil {
		t.Fatal(err)
	}
	s.Fallback = core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: 2 * time.Second, RecoveryWindow: 3 * time.Second, KeyframeTimeout: 4 * time.Second}}
	s, err = f.service.UpdateStream(context.Background(), s, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	session, output := readbackSeedHistory(t, f, s, p, g, d)
	if err := f.api.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), f.databasePath, apiAcceptanceStorageOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.New(api.Config{Core: svc, Commands: svc, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Limits: apiAcceptanceLimits()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := handler.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	unchanged := func() {
		t.Helper()
		checks := []struct {
			want any
			get  func() (any, error)
		}{
			{s, func() (any, error) { return svc.GetStream(context.Background(), s.ID) }},
			{p, func() (any, error) { return svc.GetProfile(context.Background(), p.ID) }},
			{g, func() (any, error) { return svc.GetGroup(context.Background(), g.ID) }},
			{d, func() (any, error) { return svc.GetDestination(context.Background(), d.ID) }},
			{session, func() (any, error) { return svc.GetSession(context.Background(), session.ID) }},
			{output, func() (any, error) { return svc.GetOutput(context.Background(), output.ID) }},
		}
		for _, check := range checks {
			got, err := check.get()
			if err != nil || !reflect.DeepEqual(got, check.want) {
				t.Fatalf("сохранённая запись изменилась: got=%+v want=%+v err=%v", got, check.want, err)
			}
		}
	}
	unchanged()
	readbackFields(t, readbackObject(t, handler, "GET", "/streams/"+string(s.ID), "", 200), map[string]string{"publication_key": s.PublicationKey})
	readbackFields(t, readbackItems(t, readbackObject(t, handler, "GET", "/streams", "", 200), 1)[0], map[string]string{"publication_key": s.PublicationKey})
	for _, path := range []string{"/destinations/" + string(d.ID), "/destinations", "/streams/" + string(s.ID) + "/destinations"} {
		v := readbackObject(t, handler, "GET", path, "", 200)
		if strings.HasSuffix(path, "/destinations") {
			v = readbackItems(t, v, 1)[0]
		}
		readbackFields(t, v, map[string]string{"endpoint": d.Endpoint, "stream_key": d.StreamKey})
	}
	readbackHistory(t, handler, session, output, true)
	unchanged()
	changedDestination := readbackObject(t, handler, "PATCH", "/destinations/"+string(d.ID), `{"expected_revision":"1","endpoint":"rtmp://localhost/changed?token=new","stream_key":"","name":"Следующее назначение"}`, 200)
	readbackFields(t, changedDestination, map[string]string{"endpoint": "rtmp://localhost/changed?token=new", "stream_key": "", "name": "Следующее назначение", "revision": "2"})
	changedStream := readbackObject(t, handler, "PATCH", "/streams/"+string(s.ID), `{"expected_revision":"2","name":"Следующий поток","fallback":null}`, 200)
	readbackFields(t, changedStream, map[string]string{"name": "Следующий поток", "publication_key": s.PublicationKey, "revision": "3"})
	if changedStream["fallback"] != nil {
		t.Fatal("текущая политика заглушки не очищена", changedStream)
	}
	readbackHistory(t, handler, session, output, true)
	for _, target := range []string{"/destinations/" + string(d.ID) + "?expected_revision=2", "/transcode-groups/" + string(g.ID) + "?expected_revision=1", "/streams/" + string(s.ID) + "?expected_revision=3"} {
		apiAcceptanceRequest(t, handler, "DELETE", target, "", 204)
	}
	apiAcceptanceRequest(t, handler, "GET", "/streams/"+string(s.ID)+"/sessions", "", 404)
	apiAcceptanceRequest(t, handler, "GET", "/destinations/"+string(d.ID), "", 404)
	readbackHistory(t, handler, session, output, false)
	gotSession, err := svc.GetSession(context.Background(), session.ID)
	if err != nil || !reflect.DeepEqual(gotSession, session) {
		t.Fatal("изменена историческая Session", gotSession, err)
	}
	gotOutput, err := svc.GetOutput(context.Background(), output.ID)
	if err != nil || !reflect.DeepEqual(gotOutput, output) {
		t.Fatal("изменён исторический Output", gotOutput, err)
	}
}

func readbackSchemaFields(t *testing.T, schema map[string]any, fields ...string) {
	t.Helper()
	properties := schema["properties"].(map[string]any)
	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatal("выходная схема не объявляет обязательные поля", schema)
	}
	for _, field := range fields {
		property, ok := properties[field].(map[string]any)
		if !ok || property["type"] != "string" {
			t.Fatalf("выходная схема не содержит строку %s: %#v", field, property)
		}
		description, ok := property["description"].(string)
		if !ok || strings.TrimSpace(description) == "" {
			t.Fatal("поле не описано", field, property)
		}
		found := false
		for _, name := range required {
			found = found || name == field
		}
		if !found {
			t.Fatal("всегда присутствующее поле не обязательно в схеме", field)
		}
		if field == "stream_key" {
			if minimum, ok := property["minLength"].(float64); ok && minimum > 0 {
				t.Fatal("схема запрещает пустой ключ", property)
			}
			if property["pattern"] != nil || property["enum"] != nil || property["const"] != nil {
				t.Fatal("схема необоснованно ограничивает сохранённый ключ", property)
			}
		}
	}
}

func TestAcceptanceOutputSchemasDescribeReadableConfigurationAndHistory(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	spec := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/openapi.json", "", 200))
	readbackNoConfiguredFlags(t, spec)
	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
	resolve := func(value map[string]any) map[string]any {
		if ref, ok := value["$ref"].(string); ok {
			const prefix = "#/components/schemas/"
			if !strings.HasPrefix(ref, prefix) {
				t.Fatal("неизвестная ссылка схемы", ref)
			}
			result, ok := schemas[strings.TrimPrefix(ref, prefix)].(map[string]any)
			if !ok {
				t.Fatal("ссылка не разрешается", ref)
			}
			return result
		}
		return value
	}
	for _, tc := range []struct {
		method, path, status string
		list                 bool
		nested               string
		fields               []string
	}{
		{"post", "/streams", "201", false, "", []string{"publication_key"}},
		{"get", "/streams/{id}", "200", false, "", []string{"publication_key"}},
		{"patch", "/streams/{id}", "200", false, "", []string{"publication_key"}},
		{"get", "/streams", "200", true, "", []string{"publication_key"}},
		{"post", "/destinations", "201", false, "", []string{"endpoint", "stream_key"}},
		{"get", "/destinations/{id}", "200", false, "", []string{"endpoint", "stream_key"}},
		{"patch", "/destinations/{id}", "200", false, "", []string{"endpoint", "stream_key"}},
		{"get", "/destinations", "200", true, "", []string{"endpoint", "stream_key"}},
		{"get", "/streams/{id}/destinations", "200", true, "", []string{"endpoint", "stream_key"}},
		{"get", "/sessions/{id}", "200", false, "stream", []string{"publication_key"}},
		{"get", "/sessions", "200", true, "stream", []string{"publication_key"}},
		{"get", "/streams/{id}/sessions", "200", true, "stream", []string{"publication_key"}},
		{"get", "/sessions/{id}/outputs", "200", true, "destination", []string{"endpoint", "stream_key"}},
		{"get", "/sessions/{id}/outputs/{outputId}", "200", false, "destination", []string{"endpoint", "stream_key"}},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			operation := spec["paths"].(map[string]any)[api.Prefix+tc.path].(map[string]any)[tc.method].(map[string]any)
			response := operation["responses"].(map[string]any)[tc.status].(map[string]any)
			schema := resolve(response["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any))
			if tc.list {
				items := schema["properties"].(map[string]any)["items"].(map[string]any)
				arrayType := items["type"] == "array"
				if types, ok := items["type"].([]any); ok {
					for _, kind := range types {
						arrayType = arrayType || kind == "array"
					}
				}
				if !arrayType {
					t.Fatal("список не массив", items)
				}
				schema = resolve(items["items"].(map[string]any))
			}
			if tc.nested != "" {
				schema = resolve(schema["properties"].(map[string]any)[tc.nested].(map[string]any))
			}
			readbackSchemaFields(t, schema, tc.fields...)
		})
	}
	for _, name := range []string{"StreamDTO", "DestinationDTO", "SessionDTO", "OutputDTO", "OutputDetailDTO"} {
		schema := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/schemas/"+name+".json", "", 200))
		readbackNoConfiguredFlags(t, schema)
		switch name {
		case "StreamDTO":
			readbackSchemaFields(t, schema, "publication_key")
		case "DestinationDTO":
			readbackSchemaFields(t, schema, "endpoint", "stream_key")
		default:
			field, target := "destination", "DestinationDTO"
			if name == "SessionDTO" {
				field, target = "stream", "StreamDTO"
			}
			ref := schema["properties"].(map[string]any)[field].(map[string]any)["$ref"]
			if ref != api.Prefix+"/schemas/"+target+".json" {
				t.Fatal("вложенная публичная схема потеряла ссылку", ref)
			}
		}
	}
	yaml := apiAcceptanceRequest(t, f.api, "GET", "/openapi.yaml", "", 200).Body.String()
	for _, old := range []string{"publication_key_configured", "endpoint_configured", "stream_key_configured"} {
		if strings.Contains(yaml, old) {
			t.Fatal("удалённый флаг остался в YAML", old)
		}
	}
}
