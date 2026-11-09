package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

type apiAcceptanceFixture struct {
	service *core.Service
	store   *storage.Store
	api     *api.API
	limits  api.Limits
}

func apiAcceptanceLimits() api.Limits {
	return api.Limits{JSONBytes: 16 << 10, UploadBytes: 1024, UploadReadTimeout: time.Second, OperationTimeout: 2 * time.Second, ShutdownTimeout: time.Second, SSEClients: 1, SSEPageSize: 4, SSEFrameBytes: 32 << 10, SSEWriteTimeout: time.Second, SSEHeartbeat: 50 * time.Millisecond}
}

func newAPIAcceptanceFixture(t *testing.T, configure func(*api.Config)) *apiAcceptanceFixture {
	t.Helper()
	options := storage.Options{PreparationLimits: core.PreparationLimits{MaxQueued: 3, MaxProfiles: 8}, OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 16, MaxIdentifierBytes: 1024, MaxBytes: 1 << 20}, ObservationLimits: core.ObservationLimits{MaxSessions: 8, MaxGroups: 16, MaxOutputs: 16, MaxFFmpegWorkers: 16, MaxIdentifierBytes: 1024, MaxErrorBytes: 4096, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 20 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond}
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "api.sqlite"), options)
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
	if err = svc.ConfigureTelemetry(context.Background(), core.TelemetryLimits{MediaStaleAfter: time.Minute, Events: core.EventLimits{MaxEvents: 8, MaxBytes: 1 << 20, MaxEventBytes: 64 << 10, MaxPageSize: 4}, MaxGPUDevices: 4, MaxGPUUUIDBytes: 128, MaxGPUBytes: 1 << 16}); err != nil {
		t.Fatal(err)
	}
	config := api.Config{Core: svc, Commands: svc, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Limits: apiAcceptanceLimits()}
	if configure != nil {
		configure(&config)
	}
	handler, err := api.New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := handler.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return &apiAcceptanceFixture{svc, store, handler, config.Limits}
}

func apiAcceptanceRequest(t *testing.T, handler http.Handler, method, path, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, api.Prefix+path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: status=%d, want=%d, body=%s", method, path, w.Code, status, w.Body)
	}
	return w
}

func apiAcceptanceJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func apiAcceptanceProfile() core.GroupProfile {
	return core.GroupProfile{Mode: core.ModeCopy, Format: core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecAV1, Width: 640, Height: 360, FrameRate: core.FrameRate{Numerator: 60000, Denominator: 2002}, CodecProfile: "main", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: math.MaxUint64 - 1, MaxBitrateBPS: math.MaxUint64, BufferBits: math.MaxUint64, GOPFrames: 30, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 9007199254740993}}
}

func apiAcceptanceSeedConfiguration(t *testing.T, f *apiAcceptanceFixture) (core.Stream, core.TranscodeProfile, core.TranscodeGroup, core.Destination) {
	t.Helper()
	ctx := context.Background()
	s, err := f.service.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "synthetic-publication-secret"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.service.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: apiAcceptanceProfile()})
	if err != nil {
		t.Fatal(err)
	}
	g, err := f.service.CreateGroup(ctx, core.TranscodeGroup{Name: "Группа", StreamID: s.ID, ProfileID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.service.CreateDestination(ctx, core.Destination{Name: "Назначение", StreamID: s.ID, GroupID: g.ID, Platform: "custom", Endpoint: "rtmps://synthetic-user:synthetic-password@localhost/private-path?token=synthetic-url-secret", StreamKey: "synthetic-stream-secret", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return s, p, g, d
}

func apiAcceptanceInsertLegacyProfile(t *testing.T, f *apiAcceptanceFixture, profile core.GroupProfile) core.TranscodeProfile {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	p := core.TranscodeProfile{ID: "legacy-profile", Revision: 1, CreatedAt: now, UpdatedAt: now, Name: "Прежний", Profile: profile}
	if err := f.store.Write(context.Background(), func(r core.Repositories) error {
		return r.Config.InsertProfile(context.Background(), p)
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

func apiAcceptanceNoSecrets(t *testing.T, data string, additional ...string) {
	t.Helper()
	for _, secret := range append([]string{"synthetic-publication-secret", "synthetic-stream-secret", "synthetic-url-secret", "synthetic-password", "private-path", "publication_key\"", "stream_key\"", "endpoint\"", "incarnation\"", "attempt_token\"", "managed_path\"", "canonical_profile\"", "fingerprint\""}, additional...) {
		if strings.Contains(strings.ToLower(data), strings.ToLower(secret)) {
			t.Fatalf("публичный ответ раскрывает запрещённое поле или синтетический секрет %q", secret)
		}
	}
}

func TestAcceptanceConfigurationPresenceCASAndSecretProjection(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	s, _, _, d := apiAcceptanceSeedConfiguration(t, f)
	for _, path := range []string{"/streams", "/streams/" + string(s.ID), "/destinations", "/destinations/" + string(d.ID), "/streams/" + string(s.ID) + "/destinations"} {
		w := apiAcceptanceRequest(t, f.api, "GET", path, "", 200)
		apiAcceptanceNoSecrets(t, w.Body.String())
	}
	path := "/destinations/" + string(d.ID)
	apiAcceptanceRequest(t, f.api, "PATCH", path, `{"expected_revision":"1","enabled":false,"stream_key":""}`, 200)
	stored, err := f.service.GetDestination(context.Background(), d.ID)
	if err != nil || stored.Enabled || stored.StreamKey != "" || stored.Endpoint != d.Endpoint || stored.Name != d.Name || stored.Revision != 2 {
		t.Fatalf("PATCH потерял presence: %+v %v", stored, err)
	}
	for _, tc := range []struct {
		body   string
		status int
	}{{`{"enabled":true}`, 422}, {`{"expected_revision":"1","enabled":true}`, 409}, {`{"expected_revision":"2","enabled":null}`, 422}, {`{"expected_revision":2,"enabled":true}`, 422}, {`{"expected_revision":"18446744073709551616","enabled":true}`, 422}} {
		w := apiAcceptanceRequest(t, f.api, "PATCH", path, tc.body, tc.status)
		if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
			t.Fatal("ошибка не Problem Details")
		}
	}
	stored, err = f.service.GetDestination(context.Background(), d.ID)
	if err != nil || stored.Revision != 2 || stored.Enabled || stored.StreamKey != "" {
		t.Fatal("отклонённые PATCH изменили конфигурацию", stored, err)
	}
	apiAcceptanceRequest(t, f.api, "PATCH", "/streams/"+string(s.ID), `{"expected_revision":"1","publication_key":"changed"}`, 422)
	stream, err := f.service.GetStream(context.Background(), s.ID)
	if err != nil || stream.PublicationKey != s.PublicationKey || stream.Revision != 1 {
		t.Fatal("неизменяемый ключ изменён", stream, err)
	}
	apiAcceptanceRequest(t, f.api, "DELETE", path, "", 422)
	apiAcceptanceRequest(t, f.api, "DELETE", path+"?expected_revision=1", "", 409)
	apiAcceptanceRequest(t, f.api, "DELETE", path+"?expected_revision=2", "", 204)
	apiAcceptanceRequest(t, f.api, "GET", path, "", 404)
}

func TestAcceptanceConcurrentCASUpdatesHaveExactlyOneWinner(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	s, _, _, _ := apiAcceptanceSeedConfiguration(t, f)
	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	for _, name := range []string{"Первый", "Второй"} {
		wg.Go(func() {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("PATCH", api.Prefix+"/streams/"+string(s.ID), strings.NewReader(fmt.Sprintf(`{"expected_revision":"1","name":%q}`, name)))
			r.Header.Set("Content-Type", "application/json")
			f.api.ServeHTTP(w, r)
			statuses <- w.Code
		})
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for s := range statuses {
		counts[s]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatalf("CAS допустил потерю обновления: %v", counts)
	}
	stored, err := f.service.GetStream(context.Background(), s.ID)
	if err != nil || stored.Revision != 2 {
		t.Fatal(stored, err)
	}
}

func TestAcceptanceFallbackPolicyCASPreservesStreamAndSupportsExplicitClear(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	s, _, _, _ := apiAcceptanceSeedConfiguration(t, f)
	a, err := f.service.CreateAsset(context.Background(), "Политика")
	if err != nil {
		t.Fatal(err)
	}
	path := "/streams/" + string(s.ID) + "/fallback-policy"
	policy := fmt.Sprintf(`{"expected_revision":"1","fallback":{"asset_id":%q,"watchdog":{"video_timeout_ns":"9007199254740993","audio_timeout_ns":"1000000001","recovery_window_ns":"1000000002","keyframe_timeout_ns":"1000000003"}}}`, a.ID)
	w := apiAcceptanceRequest(t, f.api, "PUT", path, policy, 200)
	if !strings.Contains(w.Body.String(), `"video_timeout_ns":"9007199254740993"`) {
		t.Fatal("длительность потеряла точность", w.Body)
	}
	stored, err := f.service.GetStream(context.Background(), s.ID)
	if err != nil || stored.Revision != 2 || stored.Name != s.Name || stored.PublicationKey != s.PublicationKey || stored.Fallback.AssetID != a.ID || stored.Fallback.Watchdog.VideoTimeout != 9007199254740993 {
		t.Fatal("PUT policy изменил Stream или длительность", stored, err)
	}
	apiAcceptanceRequest(t, f.api, "PUT", path, `{"expected_revision":"2"}`, 422)
	apiAcceptanceRequest(t, f.api, "PUT", path, `{"expected_revision":"1","fallback":null}`, 409)
	apiAcceptanceRequest(t, f.api, "PUT", path, `{"expected_revision":"2","fallback":null}`, 200)
	stored, err = f.service.GetStream(context.Background(), s.ID)
	if err != nil || stored.Fallback != (core.FallbackPolicy{}) || stored.Revision != 3 || stored.PublicationKey != s.PublicationKey {
		t.Fatal("явная очистка policy не применена", stored, err)
	}
	apiAcceptanceRequest(t, f.api, "PATCH", "/streams/"+string(s.ID), strings.Replace(policy, `"expected_revision":"1"`, `"expected_revision":"3"`, 1), 200)
	apiAcceptanceRequest(t, f.api, "PATCH", "/streams/"+string(s.ID), `{"expected_revision":"4","fallback":null}`, 200)
}

func TestAcceptanceProfileRoundTripPreservesFullIntegersFPSAndBytes(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	p := apiAcceptanceProfile()
	p.Format.Video.Level = string([]byte{0xff, 'x', 0xfe})
	p.Format.Audio.ChannelLayout = string([]byte{'s', 0x80, 't'})
	stored, err := f.service.CreateProfile(context.Background(), core.TranscodeProfile{Name: "Точный профиль", Profile: p})
	if err != nil {
		t.Fatal(err)
	}
	p = stored.Profile
	w := apiAcceptanceRequest(t, f.api, "GET", "/transcode-profiles/"+string(stored.ID), "", 200)
	v := apiAcceptanceJSON(t, w)
	profile := v["profile"].(map[string]any)
	video := profile["video_encoding"].(map[string]any)
	if video["max_bitrate_bps"] != "18446744073709551615" || video["target_bitrate_bps"] != "18446744073709551614" || profile["audio_encoding"].(map[string]any)["bitrate_bps"] != "9007199254740993" {
		t.Fatalf("целые потеряли точность: %s", w.Body)
	}
	format := profile["format"].(map[string]any)
	vf := format["video"].(map[string]any)
	if !reflect.DeepEqual(vf["frame_rate"], map[string]any{"numerator": float64(60000), "denominator": float64(2002)}) {
		t.Fatal("исходная дробь FPS нормализована")
	}
	for _, tc := range []struct {
		value any
		bytes string
	}{{vf["level"], p.Format.Video.Level}, {format["audio"].(map[string]any)["channel_layout"], p.Format.Audio.ChannelLayout}} {
		encoded, ok := tc.value.(map[string]any)
		if !ok || encoded["encoding"] != "base64" || encoded["value"] != base64.StdEncoding.EncodeToString([]byte(tc.bytes)) {
			t.Fatalf("байтовое поле заменено: %#v", tc.value)
		}
	}
	body, err := json.Marshal(map[string]any{"name": "Копия", "profile": profile})
	if err != nil {
		t.Fatal(err)
	}
	created := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", string(body), 201))
	copy, err := f.service.GetProfile(context.Background(), core.ID(created["id"].(string)))
	if err != nil || !reflect.DeepEqual(copy.Profile, p) {
		t.Fatalf("HTTP round-trip изменил профиль: %+v %v", copy.Profile, err)
	}
	copyPath := "/transcode-profiles/" + created["id"].(string)
	apiAcceptanceRequest(t, f.api, "PATCH", copyPath, `{"expected_revision":"1","profile":{"mode":"copy"}}`, 422)
	apiAcceptanceRequest(t, f.api, "PATCH", copyPath, `{"expected_revision":"1","profile":null}`, 422)
	video["closed_gop"] = false
	body, err = json.Marshal(map[string]any{"expected_revision": "1", "profile": profile})
	if err != nil {
		t.Fatal(err)
	}
	apiAcceptanceRequest(t, f.api, "PATCH", copyPath, string(body), 200)
	copy, err = f.service.GetProfile(context.Background(), copy.ID)
	p.VideoEncoding.ClosedGOP = false
	if err != nil || !reflect.DeepEqual(copy.Profile, p) {
		t.Fatal("полная замена профиля потеряла поля или false", copy, err)
	}
	vf["level"] = map[string]any{"encoding": "base64", "value": "_invalid"}
	body, err = json.Marshal(map[string]any{"name": "Ошибка", "profile": profile})
	if err != nil {
		t.Fatal(err)
	}
	apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", string(body), 422)
}

func TestAcceptanceFrameworkErrorsNeverEchoRejectedValues(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	secret := "synthetic-rejected-secret"
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{{"GET", "/streams?limit=" + secret, "", 422}, {"GET", "/streams?" + secret + "=" + secret, "", 422}, {"POST", "/streams", `{"name":"` + secret + `","publication_key":false}`, 422}, {"POST", "/streams", `{"name":"` + secret + `","publication_key":"key","` + secret + `":true}`, 422}, {"POST", "/streams", `{"name":"` + secret + `",`, 400}, {"GET", "/streams?cursor=" + secret, "", 400}, {"GET", "/events?cursor=" + secret, "", 400}} {
		w := apiAcceptanceRequest(t, f.api, tc.method, tc.path, tc.body, tc.status)
		apiAcceptanceNoSecrets(t, w.Body.String(), secret)
		v := apiAcceptanceJSON(t, w)
		if v["status"] != float64(tc.status) || v["errors"] != nil {
			t.Fatalf("небезопасная структура Problem: %s", w.Body)
		}
	}
	apiAcceptanceRequest(t, f.api, "POST", "/streams", strings.Repeat(" ", int(f.limits.JSONBytes))+`{}`, 413)
}

func TestAcceptancePaginationIsBoundedStableAndFilterBound(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	var saved []core.Stream
	for i := range 3 {
		s, err := f.service.CreateStream(context.Background(), core.Stream{Name: "Поток " + strconv.Itoa(i), PublicationKey: "key-" + strconv.Itoa(i)})
		if err != nil {
			t.Fatal(err)
		}
		saved = append(saved, s)
	}
	v := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/streams?limit=1", "", 200))
	items := v["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != string(saved[0].ID) {
		t.Fatal("первый элемент нарушает порядок", v)
	}
	cursor, ok := v["next_cursor"].(string)
	if !ok || cursor == "" {
		t.Fatal("потеряна следующая страница")
	}
	page := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/streams?limit=2&cursor="+url.QueryEscape(cursor), "", 200))
	got := page["items"].([]any)
	if len(got) != 2 || got[0].(map[string]any)["id"] != string(saved[1].ID) || got[1].(map[string]any)["id"] != string(saved[2].ID) || page["next_cursor"] != "" {
		t.Fatal("страницы пересекаются или теряют строки", page)
	}
	apiAcceptanceRequest(t, f.api, "GET", "/streams?limit=1&from="+url.QueryEscape(saved[0].CreatedAt.Format(time.RFC3339Nano))+"&cursor="+url.QueryEscape(cursor), "", 400)
	for _, limit := range []string{"0", "500", "-1"} {
		apiAcceptanceRequest(t, f.api, "GET", "/streams?limit="+limit, "", 422)
	}
	apiAcceptanceRequest(t, f.api, "GET", "/streams/missing/destinations", "", 404)
	apiAcceptanceRequest(t, f.api, "GET", "/streams/missing/sessions", "", 404)
	apiAcceptanceRequest(t, f.api, "GET", "/sessions/missing/outputs", "", 404)
}

func TestAcceptanceImmutablePublicSpecConcurrentFirstGET(t *testing.T) {
	f := newAPIAcceptanceFixture(t, nil)
	results := make(chan *httptest.ResponseRecorder, 32)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			path := "/openapi.json"
			if i%2 == 1 {
				path = "/openapi.yaml"
			}
			w := httptest.NewRecorder()
			f.api.ServeHTTP(w, httptest.NewRequest("GET", api.Prefix+path, nil))
			results <- w
		})
	}
	wg.Wait()
	close(results)
	previous := map[string][]byte{}
	for w := range results {
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
		kind := w.Header().Get("Content-Type")
		if old := previous[kind]; old != nil && !bytes.Equal(old, w.Body.Bytes()) {
			t.Fatal("спецификация меняется при параллельном чтении")
		}
		previous[kind] = bytes.Clone(w.Body.Bytes())
	}
	w := apiAcceptanceRequest(t, f.api, "GET", "/openapi.json", "", 200)
	v := apiAcceptanceJSON(t, w)
	if v["security"] != nil {
		t.Fatal("OpenAPI требует credentials")
	}
	components := v["components"].(map[string]any)
	if schemes, ok := components["securitySchemes"].(map[string]any); ok && len(schemes) != 0 {
		t.Fatal("OpenAPI объявляет security scheme")
	}
	paths := v["paths"].(map[string]any)
	for _, path := range []string{"/streams", "/sessions/{id}/metrics", "/events", "/events/snapshot", "/system/capabilities", "/fallback-assets/{id}/content"} {
		if paths[api.Prefix+path] == nil {
			t.Fatalf("маршрут отсутствует в OpenAPI: %s", path)
		}
	}
	for path, operations := range paths {
		if !strings.HasPrefix(path, api.Prefix+"/") || strings.Contains(path, api.Prefix+api.Prefix) {
			t.Fatal("неверный префикс", path)
		}
		for _, op := range operations.(map[string]any) {
			if m, ok := op.(map[string]any); ok && m["security"] != nil {
				t.Fatal("операция требует credentials", path)
			}
		}
	}
	for _, path := range []string{"/openapi-3.0.json", "/openapi-3.0.yaml", "/sessions/missing/events"} {
		apiAcceptanceRequest(t, f.api, "GET", path, "", 404)
	}
	docs := apiAcceptanceRequest(t, f.api, "GET", "/docs", "", 200)
	if !strings.Contains(docs.Body.String(), api.Prefix+"/openapi.json") {
		t.Fatal("docs не ссылается на опубликованную спецификацию")
	}
	response := apiAcceptanceRequest(t, f.api, "GET", "/streams", "", 200)
	link := response.Header().Get("Link")
	start, end := strings.Index(link, "<"), strings.Index(link, ">")
	if start < 0 || end <= start {
		t.Fatal("ответ не содержит schema link", link)
	}
	schemaPath := link[start+1 : end]
	if !strings.HasPrefix(schemaPath, api.Prefix+"/schemas/") {
		t.Fatal("schema link вне API prefix", link)
	}
	apiAcceptanceRequest(t, f.api, "GET", strings.TrimPrefix(schemaPath, api.Prefix), "", 200)
}
