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

// apiAcceptanceFixture связывает настоящий API, доменный сервис и временную SQLite с общими
// ограничениями.
type apiAcceptanceFixture struct {
	// service предоставляет доменные операции и общую RuntimeAuthority той же тестовой БД.
	service *core.Service
	// store владеет временной SQLite, миграциями и полномочиями текущего открытия.
	store *storage.Store
	// api содержит настоящий зарегистрированный HTTP-обработчик проверяемых маршрутов.
	api *api.API
	// limits сохраняет фактически применённые пределы JSON, загрузки и SSE для граничных запросов.
	limits api.Limits
	// databasePath сохраняет путь временной SQLite для сценариев повторного открытия.
	databasePath string
}

// apiAcceptanceLimits задаёт конечные пределы JSON, загрузки файлов и SSE для настоящего
// HTTP-обработчика в тестовой фикстуре.
func apiAcceptanceLimits() api.Limits {
	return api.Limits{JSONBytes: 16 << 10, UploadBytes: 1024, UploadReadTimeout: time.Second, OperationTimeout: 2 * time.Second, ShutdownTimeout: time.Second, SSEClients: 1, SSEPageSize: 4, SSEFrameBytes: 32 << 10, SSEWriteTimeout: time.Second, SSEHeartbeat: 50 * time.Millisecond}
}

// apiAcceptanceStorageOptions задаёт явные бюджеты временной SQLite, подготовки и оперативных
// владельцев без пользовательских данных.
func apiAcceptanceStorageOptions() storage.Options {
	return storage.Options{PreparationLimits: core.PreparationLimits{MaxQueued: 3, MaxProfiles: 8}, OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 16, MaxIdentifierBytes: 1024, MaxBytes: 1 << 20}, ObservationLimits: core.ObservationLimits{MaxSessions: 8, MaxGroups: 16, MaxOutputs: 16, MaxFFmpegWorkers: 16, MaxIdentifierBytes: 1024, MaxErrorBytes: 4096, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 20 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond}
}

// newAPIAcceptanceFixture открывает временную SQLite с настоящими миграциями, связывает Service и API
// и регистрирует их закрытие. Настройка позволяет заменить только необходимые зависимости конкретного
// сценария.
func newAPIAcceptanceFixture(t *testing.T, configure func(*api.Config)) *apiAcceptanceFixture {
	t.Helper()
	// databasePath указывает отдельный файл SQLite текущей фикстуры.
	databasePath := filepath.Join(t.TempDir(), "api.sqlite")
	// store содержит открытое настоящее хранилище; err содержит отказ миграций или открытия.
	store, err := storage.Open(context.Background(), databasePath, apiAcceptanceStorageOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает временное хранилище после завершения пользователей тестовой конфигурации.

		// err содержит отказ закрытия временного Store после завершения API.
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	// svc содержит доменный сервис того же Store и incarnation.
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.ConfigureTelemetry(context.Background(), core.TelemetryLimits{MediaStaleAfter: time.Minute, Events: core.EventLimits{MaxEvents: 8, MaxBytes: 1 << 20, MaxEventBytes: 64 << 10, MaxPageSize: 4}, MaxGPUDevices: 4, MaxGPUUUIDBytes: 128, MaxGPUBytes: 1 << 16}); err != nil {
		t.Fatal(err)
	}
	// config связывает настоящий Service, команды, журнал и конечные HTTP-бюджеты до настройки
	// сценария.
	config := api.Config{Core: svc, Commands: svc, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Limits: apiAcceptanceLimits()}
	if configure != nil {
		configure(&config)
	}
	// handler содержит зарегистрированный API с фактически выбранными зависимостями.
	handler, err := api.New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Останавливает API в отдельном ограниченном контексте перед освобождением хранилища.

		// ctx ограничивает очистку API двумя секундами; cancel освобождает её таймер.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		// err содержит результат закрытия API до освобождения общего Store.
		if err := handler.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return &apiAcceptanceFixture{svc, store, handler, config.Limits, databasePath}
}

// apiAcceptanceRequest вызывает настоящий HTTP-обработчик с JSON-телом при его наличии и требует
// ожидаемый статус.
func apiAcceptanceRequest(t *testing.T, handler http.Handler, method, path, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	// r содержит запрос к реальному префиксу API с текущим методом и телом.
	r := httptest.NewRequest(method, api.Prefix+path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	// w записывает фактические статус, заголовки и тело обработчика для проверки ожидаемого ответа.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: status=%d, want=%d, body=%s", method, path, w.Code, status, w.Body)
	}
	return w
}

// apiAcceptanceJSON декодирует тело тестового ответа в объект и завершает сценарий при некорректном
// JSON.
func apiAcceptanceJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	// v принимает JSON-объект настоящего ответа API.
	var v map[string]any
	// err содержит отказ разбора JSON-тела тестового ответа.
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// apiAcceptanceProfile создаёт прежний полный профиль с большими точными целыми и несокращённой дробью
// FPS для проверки сохранности представления.
func apiAcceptanceProfile() core.GroupProfile {
	return core.GroupProfile{Mode: core.ModeCopy, Format: core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecAV1, Width: 640, Height: 360, FrameRate: core.FrameRate{Numerator: 60000, Denominator: 2002}, CodecProfile: "main", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: math.MaxUint64 - 1, MaxBitrateBPS: math.MaxUint64, BufferBits: math.MaxUint64, GOPFrames: 30, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 9007199254740993}}
}

// apiAcceptanceSeedConfiguration создаёт связанные Stream, Profile, Group и Destination с
// синтетическими ключами и полным URL для HTTP-проверок.
func apiAcceptanceSeedConfiguration(t *testing.T, f *apiAcceptanceFixture) (core.Stream, core.TranscodeProfile, core.TranscodeGroup, core.Destination) {
	t.Helper()
	// ctx задаёт общий контекст создания связанных конфигурационных объектов.
	ctx := context.Background()
	// s содержит созданный Stream с синтетическим постоянным ключом; err содержит отказ создания.
	s, err := f.service.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "synthetic-publication-secret"})
	if err != nil {
		t.Fatal(err)
	}
	// p содержит созданный полный профиль для связанных группы и назначения.
	p, err := f.service.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: apiAcceptanceProfile()})
	if err != nil {
		t.Fatal(err)
	}
	// g содержит созданную группу выбранного Stream и профиля.
	g, err := f.service.CreateGroup(ctx, core.TranscodeGroup{Name: "Группа", StreamID: s.ID, ProfileID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	// d содержит назначение с полным синтетическим URL и отдельным ключом для проверки чтения.
	d, err := f.service.CreateDestination(ctx, core.Destination{Name: "Назначение", StreamID: s.ID, GroupID: g.ID, Platform: "custom", Endpoint: "rtmps://synthetic-user:synthetic-password@localhost/private-path?token=synthetic-url-secret", StreamKey: "synthetic-stream-secret", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return s, p, g, d
}

// apiAcceptanceInsertLegacyProfile сохраняет прежний профиль напрямую в тестовое хранилище, обходя
// правила создания нового HTTP-профиля.
func apiAcceptanceInsertLegacyProfile(t *testing.T, f *apiAcceptanceFixture, profile core.GroupProfile) core.TranscodeProfile {
	t.Helper()
	// now задаёт UTC-времена прежнего профиля с микросекундной точностью SQLite.
	now := time.Now().UTC().Truncate(time.Microsecond)
	// p содержит прежний профиль ревизии 1, вставляемый без назначения значений по умолчанию для
	// нового HTTP-профиля.
	p := core.TranscodeProfile{ID: "legacy-profile", Revision: 1, CreatedAt: now, UpdatedAt: now, Name: "Прежний", Profile: profile}
	// err содержит отказ прямой вставки прежнего профиля в тестовую БД.
	if err := f.store.Write(context.Background(), func(r core.Repositories) error {
		// Вставляет прежний профиль напрямую, сохраняя его исходное представление без значений по умолчанию
		// сервиса.

		return r.Config.InsertProfile(context.Background(), p)
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

// apiAcceptanceNoSecrets требует отсутствия служебных полей, синтетических секретов и названий
// секретных полей в безопасных ответах.
func apiAcceptanceNoSecrets(t *testing.T, data string, additional ...string) {
	t.Helper()
	apiAcceptanceNoServiceFields(t, data)
	// secret содержит очередное запрещённое в безопасном ответе поле или синтетическое значение.
	for _, secret := range append([]string{"synthetic-publication-secret", "synthetic-stream-secret", "synthetic-url-secret", "synthetic-password", "private-path", "publication_key\"", "stream_key\"", "endpoint\""}, additional...) {
		if strings.Contains(strings.ToLower(data), strings.ToLower(secret)) {
			t.Fatalf("публичный ответ раскрывает запрещённое поле или синтетический секрет %q", secret)
		}
	}
}

// apiAcceptanceNoServiceFields проверяет отсутствие внутренних владельцев, токенов и файловых ключей в
// публичном ответе.
func apiAcceptanceNoServiceFields(t *testing.T, data string) {
	t.Helper()
	// field содержит очередное внутреннее поле полномочий или файлового хранения, запрещённое в
	// публичном ответе.
	for _, field := range []string{"incarnation\"", "attempt_token\"", "managed_path\"", "canonical_profile\"", "fingerprint\""} {
		if strings.Contains(strings.ToLower(data), field) {
			t.Fatalf("публичный ответ раскрывает служебное поле %q", field)
		}
	}
}

// Проверяет полное чтение сохранённых ключей, различие отсутствия/null/false/пустой строки и CAS для
// изменений и удаления без частичных побочных записей.
func TestAcceptanceConfigurationPresenceCASAndKeyReadback(t *testing.T) {
	// f содержит API сценария присутствия полей, CAS и полного чтения ключей.
	f := newAPIAcceptanceFixture(t, nil)
	// s содержит исходный Stream; d содержит исходное назначение для точного чтения и последующих
	// изменений.
	s, _, _, d := apiAcceptanceSeedConfiguration(t, f)
	// path задаёт очередной одиночный или списочный маршрут сохранённой конфигурации.
	for _, path := range []string{"/streams", "/streams/" + string(s.ID), "/destinations", "/destinations/" + string(d.ID), "/streams/" + string(s.ID) + "/destinations"} {
		// v содержит прочитанный объект либо страницу, из которой выбирается единственная
		// запись.
		v := readbackObject(t, f.api, "GET", path, "", 200)
		if v["items"] != nil {
			v = readbackItems(t, v, 1)[0]
		}
		if strings.Contains(path, "destinations") {
			readbackFields(t, v, map[string]string{"endpoint": d.Endpoint, "stream_key": d.StreamKey})
		} else {
			readbackFields(t, v, map[string]string{"publication_key": s.PublicationKey})
		}
	}
	// path адресует назначение, изменяемое и удаляемое по ожидаемой ревизии.
	path := "/destinations/" + string(d.ID)
	apiAcceptanceRequest(t, f.api, "PATCH", path, `{"expected_revision":"1","enabled":false,"stream_key":""}`, 200)
	// stored содержит назначение после PATCH; err содержит отказ чтения для проверки false, пустого
	// ключа и сохранённых полей.
	stored, err := f.service.GetDestination(context.Background(), d.ID)
	if err != nil || stored.Enabled || stored.StreamKey != "" || stored.Endpoint != d.Endpoint || stored.Name != d.Name || stored.Revision != 2 {
		t.Fatalf("PATCH потерял presence: %+v %v", stored, err)
	}
	// tc задаёт недопустимый PATCH и ожидаемый статус без побочного изменения назначения.
	for _, tc := range []struct {
		// body содержит ошибочный JSON частичного изменения назначения.
		body string
		// status задаёт ожидаемый отказ структуры, ревизии, null или Decimal.
		status int
	}{{`{"enabled":true}`, 422}, {`{"expected_revision":"1","enabled":true}`, 409}, {`{"expected_revision":"2","enabled":null}`, 422}, {`{"expected_revision":2,"enabled":true}`, 422}, {`{"expected_revision":"18446744073709551616","enabled":true}`, 422}} {
		// w содержит текущий отказ PATCH для проверки application/problem+json.
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
	// stream содержит Stream после запрещённого изменения publication_key для проверки неизменности
	// ключа.
	stream, err := f.service.GetStream(context.Background(), s.ID)
	if err != nil || stream.PublicationKey != s.PublicationKey || stream.Revision != 1 {
		t.Fatal("неизменяемый ключ изменён", stream, err)
	}
	apiAcceptanceRequest(t, f.api, "DELETE", path, "", 422)
	apiAcceptanceRequest(t, f.api, "DELETE", path+"?expected_revision=1", "", 409)
	apiAcceptanceRequest(t, f.api, "DELETE", path+"?expected_revision=2", "", 204)
	apiAcceptanceRequest(t, f.api, "GET", path, "", 404)
}

// Проверяет, что два конкурентных изменения одной ревизии дают ровно один успех и один конфликт без
// потери обновления.
func TestAcceptanceConcurrentCASUpdatesHaveExactlyOneWinner(t *testing.T) {
	// f содержит настоящий API конкурентных PATCH одной ревизии.
	f := newAPIAcceptanceFixture(t, nil)
	// s содержит исходный Stream ревизии 1 для обоих конкурирующих изменений.
	s, _, _, _ := apiAcceptanceSeedConfiguration(t, f)
	// statuses передаёт два независимых HTTP-статуса конкурентных PATCH.
	statuses := make(chan int, 2)
	// wg подтверждает возврат обоих HTTP-обработчиков до чтения итогов.
	var wg sync.WaitGroup
	// name задаёт новое имя каждого конкурирующего запроса одной ожидаемой ревизии.
	for _, name := range []string{"Первый", "Второй"} {
		wg.Go(func() {
			// Параллельно отправляет PATCH с одной ожидаемой ревизией, чтобы проверить единственного победителя
			// CAS.

			// w сохраняет ответ одного конкурентного PATCH.
			w := httptest.NewRecorder()
			// r содержит PATCH одного имени с общей ожидаемой ревизией 1.
			r := httptest.NewRequest("PATCH", api.Prefix+"/streams/"+string(s.ID), strings.NewReader(fmt.Sprintf(`{"expected_revision":"1","name":%q}`, name)))
			r.Header.Set("Content-Type", "application/json")
			f.api.ServeHTTP(w, r)
			statuses <- w.Code
		})
	}
	wg.Wait()
	close(statuses)
	// counts подсчитывает успешные и конфликтующие статусы двух изменений.
	counts := map[int]int{}
	// s содержит очередной полученный HTTP-статус для подсчёта результатов CAS.
	for s := range statuses {
		counts[s]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatalf("CAS допустил потерю обновления: %v", counts)
	}
	// stored содержит итоговый Stream; err содержит отказ чтения для проверки единственного
	// повышения ревизии.
	stored, err := f.service.GetStream(context.Background(), s.ID)
	if err != nil || stored.Revision != 2 {
		t.Fatal(stored, err)
	}
}

// Проверяет точные длительности и CAS политики заглушки, её явную очистку и сохранение остальных полей
// Stream.
func TestAcceptanceFallbackPolicyCASPreservesStreamAndSupportsExplicitClear(t *testing.T) {
	// f содержит API сценария точных сроков политики заглушки и её очистки.
	f := newAPIAcceptanceFixture(t, nil)
	// s содержит Stream, остальные поля которого должны пережить замену политики.
	s, _, _, _ := apiAcceptanceSeedConfiguration(t, f)
	// a содержит выбранную заглушку; err содержит отказ её создания.
	a, err := f.service.CreateAsset(context.Background(), "Политика")
	if err != nil {
		t.Fatal(err)
	}
	// path адресует самостоятельный ресурс политики заглушки Stream.
	path := "/streams/" + string(s.ID) + "/fallback-policy"
	// policy содержит JSON полной политики с точным сроком за пределами точности JavaScript.
	policy := fmt.Sprintf(`{"expected_revision":"1","fallback":{"asset_id":%q,"watchdog":{"video_timeout_ns":"9007199254740993","audio_timeout_ns":"1000000001","recovery_window_ns":"1000000002","keyframe_timeout_ns":"1000000003"}}}`, a.ID)
	// w содержит ответ замены политики для проверки точной наносекундной строки.
	w := apiAcceptanceRequest(t, f.api, "PUT", path, policy, 200)
	if !strings.Contains(w.Body.String(), `"video_timeout_ns":"9007199254740993"`) {
		t.Fatal("длительность потеряла точность", w.Body)
	}
	// stored содержит Stream после замены и последующей очистки политики.
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

// Проверяет сохранение полных uint64, исходной дроби FPS и произвольных байтов профиля через GET, POST
// и полную замену PATCH.
func TestAcceptanceProfileRoundTripPreservesFullIntegersFPSAndBytes(t *testing.T) {
	// f содержит API сценария точного полного цикла профиля.
	f := newAPIAcceptanceFixture(t, nil)
	// p содержит полный профиль с большими uint64, исходной дробью FPS и затем невалидными UTF-8
	// байтами.
	p := apiAcceptanceProfile()
	p.Format.Video.Level = string([]byte{0xff, 'x', 0xfe})
	p.Format.Audio.ChannelLayout = string([]byte{'s', 0x80, 't'})
	// stored содержит сохранённый точный профиль; err содержит отказ создания.
	stored, err := f.service.CreateProfile(context.Background(), core.TranscodeProfile{Name: "Точный профиль", Profile: p})
	if err != nil {
		t.Fatal(err)
	}
	p = stored.Profile
	// w сохраняет GET профиля для проверки JSON-строк точных целых.
	w := apiAcceptanceRequest(t, f.api, "GET", "/transcode-profiles/"+string(stored.ID), "", 200)
	// v содержит разобранный публичный ресурс профиля.
	v := apiAcceptanceJSON(t, w)
	// profile содержит полный возвращённый профиль для последующих POST и полной замены PATCH.
	profile := v["profile"].(map[string]any)
	// video содержит общие видеонастройки с проверяемыми точными uint64.
	video := profile["video_encoding"].(map[string]any)
	if video["max_bitrate_bps"] != "18446744073709551615" || video["target_bitrate_bps"] != "18446744073709551614" || profile["audio_encoding"].(map[string]any)["bitrate_bps"] != "9007199254740993" {
		t.Fatalf("целые потеряли точность: %s", w.Body)
	}
	// format содержит полное публичное описание обеих дорожек.
	format := profile["format"].(map[string]any)
	// vf содержит видеоформат с несокращённым FPS и байтовым level.
	vf := format["video"].(map[string]any)
	if !reflect.DeepEqual(vf["frame_rate"], map[string]any{"numerator": float64(60000), "denominator": float64(2002)}) {
		t.Fatal("исходная дробь FPS нормализована")
	}
	// tc связывает возвращённое байтовое поле с его точным исходным содержимым.
	for _, tc := range []struct {
		// value содержит публичное представление невалидных UTF-8 байтов.
		value any
		// bytes содержит исходные точные байты для проверки base64.
		bytes string
	}{{vf["level"], p.Format.Video.Level}, {format["audio"].(map[string]any)["channel_layout"], p.Format.Audio.ChannelLayout}} {
		// encoded содержит объектное представление поля; ok подтверждает объект вместо потери
		// исходных байтов.
		encoded, ok := tc.value.(map[string]any)
		if !ok || encoded["encoding"] != "base64" || encoded["value"] != base64.StdEncoding.EncodeToString([]byte(tc.bytes)) {
			t.Fatalf("байтовое поле заменено: %#v", tc.value)
		}
	}
	// body содержит JSON создания копии полного возвращённого профиля.
	body, err := json.Marshal(map[string]any{"name": "Копия", "profile": profile})
	if err != nil {
		t.Fatal(err)
	}
	// created содержит ответ POST копии профиля для независимого чтения из домена.
	created := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", string(body), 201))
	// copy содержит сохранённую копию профиля для точного сравнения после POST и PATCH.
	copy, err := f.service.GetProfile(context.Background(), core.ID(created["id"].(string)))
	if err != nil || !reflect.DeepEqual(copy.Profile, p) {
		t.Fatalf("HTTP round-trip изменил профиль: %+v %v", copy.Profile, err)
	}
	// copyPath адресует созданную копию для проверок неполной и полной замены.
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

// Проверяет безопасные Problem Details для ошибок структуры, параметров и Huma-валидации без
// отклонённых значений, а также предел JSON-тела.
func TestAcceptanceFrameworkErrorsNeverEchoRejectedValues(t *testing.T) {
	// f содержит API сценария безопасных ошибок структуры и ограничения JSON.
	f := newAPIAcceptanceFixture(t, nil)
	// secret задаёт синтетическое отклоняемое значение, которое не должно попадать в Problem
	// Details.
	secret := "synthetic-rejected-secret"
	// tc задаёт ошибочный запрос и ожидаемый HTTP-статус без раскрытия отклонённого значения.
	for _, tc := range []struct {
		// method задаёт метод ошибочного запроса; path задаёт его маршрут или query; body
		// содержит недопустимое JSON-тело.
		method, path, body string
		// status задаёт ожидаемую ошибку структуры, валидации или курсора.
		status int
	}{{"GET", "/streams?limit=" + secret, "", 422}, {"GET", "/streams?" + secret + "=" + secret, "", 422}, {"POST", "/streams", `{"name":"` + secret + `","publication_key":false}`, 422}, {"POST", "/streams", `{"name":"` + secret + `","publication_key":"key","` + secret + `":true}`, 422}, {"POST", "/streams", `{"name":"` + secret + `",`, 400}, {"GET", "/streams?cursor=" + secret, "", 400}, {"GET", "/events?cursor=" + secret, "", 400}} {
		// w содержит реальный ошибочный ответ для проверки отсутствия синтетического значения.
		w := apiAcceptanceRequest(t, f.api, tc.method, tc.path, tc.body, tc.status)
		apiAcceptanceNoSecrets(t, w.Body.String(), secret)
		// v содержит Problem Details для проверки статуса и отсутствия исходного массива errors.
		v := apiAcceptanceJSON(t, w)
		if v["status"] != float64(tc.status) || v["errors"] != nil {
			t.Fatalf("небезопасная структура Problem: %s", w.Body)
		}
	}
	apiAcceptanceRequest(t, f.api, "POST", "/streams", strings.Repeat(" ", int(f.limits.JSONBytes))+`{}`, 413)
}

// Проверяет порядок и непересечение страниц, изменение limit между страницами, привязку cursor к
// фильтрам и проверку родителя вложенных списков.
func TestAcceptancePaginationIsBoundedStableAndFilterBound(t *testing.T) {
	// f содержит API сценария ограниченных страниц и привязки cursor к фильтрам.
	f := newAPIAcceptanceFixture(t, nil)
	// saved сохраняет три созданных потока в ожидаемом порядке выдачи.
	var saved []core.Stream
	// i задаёт номер создаваемого потока и уникального ключа.
	for i := range 3 {
		// s содержит очередной сохранённый поток; err содержит отказ создания.
		s, err := f.service.CreateStream(context.Background(), core.Stream{Name: "Поток " + strconv.Itoa(i), PublicationKey: "key-" + strconv.Itoa(i)})
		if err != nil {
			t.Fatal(err)
		}
		saved = append(saved, s)
	}
	// v содержит первую страницу с пределом одной записи.
	v := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/streams?limit=1", "", 200))
	// items содержит единственный элемент первой страницы для проверки начального порядка.
	items := v["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != string(saved[0].ID) {
		t.Fatal("первый элемент нарушает порядок", v)
	}
	// cursor содержит позицию следующего чтения; ok подтверждает строковый тип и наличие
	// продолжения.
	cursor, ok := v["next_cursor"].(string)
	if !ok || cursor == "" {
		t.Fatal("потеряна следующая страница")
	}
	// page содержит вторую страницу с изменённым пределом двух записей.
	page := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/streams?limit=2&cursor="+url.QueryEscape(cursor), "", 200))
	// got содержит элементы продолжения для проверки отсутствия пересечений и потерь.
	got := page["items"].([]any)
	if len(got) != 2 || got[0].(map[string]any)["id"] != string(saved[1].ID) || got[1].(map[string]any)["id"] != string(saved[2].ID) || page["next_cursor"] != "" {
		t.Fatal("страницы пересекаются или теряют строки", page)
	}
	apiAcceptanceRequest(t, f.api, "GET", "/streams?limit=1&from="+url.QueryEscape(saved[0].CreatedAt.Format(time.RFC3339Nano))+"&cursor="+url.QueryEscape(cursor), "", 400)
	// limit задаёт очередной недопустимый размер страницы.
	for _, limit := range []string{"0", "500", "-1"} {
		apiAcceptanceRequest(t, f.api, "GET", "/streams?limit="+limit, "", 422)
	}
	apiAcceptanceRequest(t, f.api, "GET", "/streams/missing/destinations", "", 404)
	apiAcceptanceRequest(t, f.api, "GET", "/streams/missing/sessions", "", 404)
	apiAcceptanceRequest(t, f.api, "GET", "/sessions/missing/outputs", "", 404)
}

// Проверяет конкурентное первое чтение неизменяемых JSON/YAML OpenAPI, отсутствие авторизации, общий
// префикс и работоспособность ссылок на схемы.
func TestAcceptanceImmutablePublicSpecConcurrentFirstGET(t *testing.T) {
	// f содержит API для конкурентного первого чтения заранее подготовленных спецификаций.
	f := newAPIAcceptanceFixture(t, nil)
	// results передаёт 32 фактических HTTP-ответа JSON/YAML-спецификаций.
	results := make(chan *httptest.ResponseRecorder, 32)
	// wg подтверждает завершение всех конкурентных чтений до сравнения байтов.
	var wg sync.WaitGroup
	// i выбирает JSON или YAML для текущего конкурентного чтения.
	for i := range 32 {
		wg.Go(func() {
			// Параллельно читает JSON либо YAML спецификации для проверки неизменности подготовленных документов.

			// path содержит публичный JSON- либо YAML-маршрут текущего чтения.
			path := "/openapi.json"
			if i%2 == 1 {
				path = "/openapi.yaml"
			}
			// w сохраняет ответ одной конкурентной выдачи спецификации.
			w := httptest.NewRecorder()
			f.api.ServeHTTP(w, httptest.NewRequest("GET", api.Prefix+path, nil))
			results <- w
		})
	}
	wg.Wait()
	close(results)
	// previous сохраняет первую копию спецификации каждого Content-Type для побайтового сравнения
	// остальных.
	previous := map[string][]byte{}
	// w содержит очередной завершённый ответ спецификации из results.
	for w := range results {
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
		// kind содержит Content-Type ответа для выбора соответствующего эталона байтов.
		kind := w.Header().Get("Content-Type")
		// old содержит прежние байты той же спецификации для проверки неизменности при
		// конкурентном чтении.
		if old := previous[kind]; old != nil && !bytes.Equal(old, w.Body.Bytes()) {
			t.Fatal("спецификация меняется при параллельном чтении")
		}
		previous[kind] = bytes.Clone(w.Body.Bytes())
	}
	// w сохраняет OpenAPI JSON для структурной проверки публичного контракта.
	w := apiAcceptanceRequest(t, f.api, "GET", "/openapi.json", "", 200)
	// v содержит разобранную спецификацию без требований учётных данных.
	v := apiAcceptanceJSON(t, w)
	if v["security"] != nil {
		t.Fatal("OpenAPI требует credentials")
	}
	// components содержит общие компоненты OpenAPI для проверки отсутствия securitySchemes.
	components := v["components"].(map[string]any)
	// schemes содержит объявленные схемы авторизации; ok отмечает их объектный тип для требования
	// пустого набора.
	if schemes, ok := components["securitySchemes"].(map[string]any); ok && len(schemes) != 0 {
		t.Fatal("OpenAPI объявляет security scheme")
	}
	// paths содержит фактически опубликованные операции управляющего API.
	paths := v["paths"].(map[string]any)
	// path задаёт очередной обязательный маршрут, который должен присутствовать под единым
	// префиксом.
	for _, path := range []string{"/streams", "/sessions/{id}/metrics", "/events", "/events/snapshot", "/system/capabilities", "/fallback-assets/{id}/content"} {
		if paths[api.Prefix+path] == nil {
			t.Fatalf("маршрут отсутствует в OpenAPI: %s", path)
		}
	}
	// path содержит очередной ключ OpenAPI; operations содержит объявленные операции этого пути.
	for path, operations := range paths {
		if !strings.HasPrefix(path, api.Prefix+"/") || strings.Contains(path, api.Prefix+api.Prefix) {
			t.Fatal("неверный префикс", path)
		}
		// op содержит очередное значение операции для проверки отсутствия требований security.
		for _, op := range operations.(map[string]any) {
			// m содержит объект операции; ok отличает его от иных значений пути.
			if m, ok := op.(map[string]any); ok && m["security"] != nil {
				t.Fatal("операция требует credentials", path)
			}
		}
	}
	// path задаёт очередной отсутствующий дополнительный маршрут для проверки ответа 404.
	for _, path := range []string{"/openapi-3.0.json", "/openapi-3.0.yaml", "/sessions/missing/events"} {
		apiAcceptanceRequest(t, f.api, "GET", path, "", 404)
	}
	// docs содержит публичную HTML-документацию для проверки ссылки на действующую спецификацию.
	docs := apiAcceptanceRequest(t, f.api, "GET", "/docs", "", 200)
	if !strings.Contains(docs.Body.String(), api.Prefix+"/openapi.json") {
		t.Fatal("docs не ссылается на опубликованную спецификацию")
	}
	// response содержит обычный ответ списка потоков для извлечения публичной ссылки схемы.
	response := apiAcceptanceRequest(t, f.api, "GET", "/streams", "", 200)
	// link содержит заголовок describedBy с адресом схемы ответа.
	link := response.Header().Get("Link")
	// start указывает открывающую угловую скобку ссылки; end указывает закрывающую скобку для
	// извлечения адреса.
	start, end := strings.Index(link, "<"), strings.Index(link, ">")
	if start < 0 || end <= start {
		t.Fatal("ответ не содержит schema link", link)
	}
	// schemaPath содержит опубликованный адрес отдельной схемы, который затем проверяется реальным
	// GET.
	schemaPath := link[start+1 : end]
	if !strings.HasPrefix(schemaPath, api.Prefix+"/schemas/") {
		t.Fatal("schema link вне API prefix", link)
	}
	apiAcceptanceRequest(t, f.api, "GET", strings.TrimPrefix(schemaPath, api.Prefix), "", 200)
}
