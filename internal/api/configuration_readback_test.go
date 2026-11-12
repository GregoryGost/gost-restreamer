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

// readbackObject вызывает HTTP-операцию и проверяет отсутствие внутренних полей и прежних флагов
// *_configured в возвращённом объекте.
func readbackObject(t *testing.T, handler http.Handler, method, path, body string, status int) map[string]any {
	t.Helper()
	// w сохраняет реальный HTTP-ответ для проверки внутренних полей и конфигурационных значений.
	w := apiAcceptanceRequest(t, handler, method, path, body, status)
	apiAcceptanceNoServiceFields(t, w.Body.String())
	// value содержит разобранный объект ответа перед рекурсивной проверкой прежних флагов.
	value := apiAcceptanceJSON(t, w)
	readbackNoConfiguredFlags(t, value)
	return value
}

// readbackNoConfiguredFlags рекурсивно проверяет отсутствие трёх прежних флагов доступности
// сохранённых ключей и адреса.
func readbackNoConfiguredFlags(t *testing.T, value any) {
	t.Helper()
	// v содержит текущий JSON-объект или массив при рекурсивном обходе ответа.
	switch v := value.(type) {
	case map[string]any:
		// key содержит имя свойства объекта; child содержит его вложенное значение для проверки
		// старых флагов.
		for key, child := range v {
			if key == "publication_key_configured" || key == "endpoint_configured" || key == "stream_key_configured" {
				t.Fatalf("удалённый флаг присутствует: %s", key)
			}
			readbackNoConfiguredFlags(t, child)
		}
	case []any:
		// child содержит очередной элемент массива, проверяемый рекурсивно.
		for _, child := range v {
			readbackNoConfiguredFlags(t, child)
		}
	}
}

// readbackFields требует точного присутствующего строкового значения каждого указанного поля.
func readbackFields(t *testing.T, value map[string]any, fields map[string]string) {
	t.Helper()
	// field задаёт проверяемое строковое поле; want содержит его точное сохранённое значение.
	for field, want := range fields {
		// got содержит фактическую строку поля; present подтверждает присутствие и строковый
		// тип.
		got, present := value[field].(string)
		if !present || got != want {
			t.Fatalf("поле %s: got=%#v, want=%q", field, value[field], want)
		}
	}
}

// readbackItems требует массив заданной длины и возвращает его элементы как объекты для последующих
// проверок.
func readbackItems(t *testing.T, value map[string]any, count int) []map[string]any {
	t.Helper()
	// items содержит массив страницы; ok подтверждает тип перед проверкой ожидаемой длины.
	items, ok := value["items"].([]any)
	if !ok || len(items) != count {
		t.Fatalf("неверное число записей: %#v, want=%d", value, count)
	}
	// result собирает элементы страницы как объекты для последующих проверок полей.
	result := make([]map[string]any, 0, count)
	// item содержит очередной элемент массива страницы для преобразования в объект.
	for _, item := range items {
		result = append(result, item.(map[string]any))
	}
	return result
}

// Проверяет полные сохранённые ключи и URL, включая пустой stream_key, через создание, изменение,
// чтение и общие и вложенные списки.
func TestAcceptanceConfigurationReturnsExactSavedKeysAndFullEndpoints(t *testing.T) {
	// f содержит API сценария полного чтения ключей и адресов после создания и PATCH.
	f := newAPIAcceptanceFixture(t, nil)
	// p содержит профиль для групп создаваемых потоков; err содержит отказ его создания.
	p, err := f.service.CreateProfile(context.Background(), core.TranscodeProfile{Name: "Профиль чтения", Profile: apiAcceptanceProfile()})
	if err != nil {
		t.Fatal(err)
	}
	// streams сопоставляет ID созданных потоков их точным постоянным ключам до проверки общего
	// списка.
	streams := map[string]string{}
	// destinations сопоставляет ID назначений их ожидаемым полным полям после PATCH.
	destinations := map[string]map[string]string{}
	// i задаёт номер связанного набора; key выбирает непустой либо пустой ключ назначения.
	for i, key := range []string{"synthetic-stream-key/+?a=1&b=%2F", ""} {
		// publicationKey задаёт точный синтетический ключ OBS с символами URL для сохранения без
		// преобразований.
		publicationKey := fmt.Sprintf("synthetic-publication-%d/+?token=exact", i)
		// s содержит ответ создания, затем переименования Stream с сохранённым publication_key.
		s := readbackObject(t, f.api, "POST", "/streams", fmt.Sprintf(`{"name":"Поток %d","publication_key":%q}`, i, publicationKey), 201)
		// id содержит идентификатор созданного Stream для связанных запросов.
		id := s["id"].(string)
		streams[id] = publicationKey
		readbackFields(t, s, map[string]string{"publication_key": publicationKey})
		s = readbackObject(t, f.api, "PATCH", "/streams/"+id, `{"expected_revision":"1","name":"Новое имя"}`, 200)
		readbackFields(t, s, map[string]string{"publication_key": publicationKey, "name": "Новое имя"})
		// g содержит группу текущего потока и общего профиля; err содержит отказ создания.
		g, err := f.service.CreateGroup(context.Background(), core.TranscodeGroup{Name: "Группа чтения", StreamID: core.ID(id), ProfileID: p.ID})
		if err != nil {
			t.Fatal(err)
		}
		// endpoint задаёт полный RTMPS URL с синтетическими учётными данными и экранированными
		// query-параметрами.
		endpoint := fmt.Sprintf("rtmps://synthetic-user-%d:p%%40ss@localhost:1935/private-path/%d?token=synthetic-url-%d&next=%%2Flive", i, i, i)
		// d содержит ответ создания или изменения назначения для точного чтения полей.
		d := readbackObject(t, f.api, "POST", "/destinations", fmt.Sprintf(`{"stream_id":%q,"group_id":%q,"name":"Назначение","platform":"custom","endpoint":%q,"stream_key":%q,"enabled":false}`, id, g.ID, endpoint, key), 201)
		// destinationID содержит ID текущего назначения для одиночных и списочных запросов.
		destinationID := d["id"].(string)
		// fields содержит точные ожидаемые endpoint, stream_key и stream_id текущего состояния.
		fields := map[string]string{"endpoint": endpoint, "stream_key": key, "stream_id": id}
		readbackFields(t, d, fields)
		readbackFields(t, readbackObject(t, f.api, "GET", "/destinations/"+destinationID, "", 200), fields)
		readbackFields(t, readbackItems(t, readbackObject(t, f.api, "GET", "/streams/"+id+"/destinations", "", 200), 1)[0], fields)
		// patchedKey задаёт противоположный случай ключа: очистку непустого или заполнение
		// пустого.
		patchedKey := ""
		if key == "" {
			patchedKey = "synthetic-patched-key/+?exact=%2F"
		}
		endpoint += "&patched=true"
		d = readbackObject(t, f.api, "PATCH", "/destinations/"+destinationID, fmt.Sprintf(`{"expected_revision":"1","endpoint":%q,"stream_key":%q}`, endpoint, patchedKey), 200)
		fields = map[string]string{"endpoint": endpoint, "stream_key": patchedKey, "stream_id": id}
		readbackFields(t, d, fields)
		destinations[destinationID] = fields
		// path задаёт одиночный или вложенный маршрут назначения после PATCH.
		for _, path := range []string{"/destinations/" + destinationID, "/streams/" + id + "/destinations"} {
			// v содержит прочитанное назначение либо страницу для выбора её единственной
			// записи.
			v := readbackObject(t, f.api, "GET", path, "", 200)
			if strings.HasSuffix(path, "/destinations") {
				v = readbackItems(t, v, 1)[0]
			}
			readbackFields(t, v, fields)
		}
		readbackFields(t, readbackObject(t, f.api, "GET", "/streams/"+id, "", 200), map[string]string{"publication_key": publicationKey})
	}
	// s содержит очередной Stream общего списка для сверки полного постоянного ключа.
	for _, s := range readbackItems(t, readbackObject(t, f.api, "GET", "/streams", "", 200), 2) {
		// key содержит ожидаемый ключ потока; ok подтверждает, что список вернул известную
		// запись.
		key, ok := streams[s["id"].(string)]
		if !ok {
			t.Fatal("список вернул неизвестный поток", s)
		}
		readbackFields(t, s, map[string]string{"publication_key": key})
		delete(streams, s["id"].(string))
	}
	// d содержит очередное назначение общего списка для сверки полных полей.
	for _, d := range readbackItems(t, readbackObject(t, f.api, "GET", "/destinations", "", 200), 2) {
		// fields содержит ожидаемые поля назначения; ok подтверждает наличие записи в эталонном
		// наборе.
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

// readbackSeedHistory напрямую сохраняет завершённую Session и самостоятельный OutputSnapshot с
// заданной конфигурацией для проверки исторического чтения.
func readbackSeedHistory(t *testing.T, f *apiAcceptanceFixture, s core.Stream, p core.TranscodeProfile, g core.TranscodeGroup, d core.Destination) (core.Session, core.SessionOutput) {
	t.Helper()
	// now задаёт согласованные UTC-времена исторических записей с точностью SQLite.
	now := time.Now().UTC().Truncate(time.Microsecond)
	// session содержит завершённую Session с самостоятельным снимком исходной конфигурации.
	session := core.Session{Record: core.Record{ID: "saved-session", Revision: 1, CreatedAt: now, UpdatedAt: now}, StreamID: s.ID, Incarnation: f.store.Incarnation(), State: core.SessionStopped, Lifecycle: 1, Snapshot: core.SessionSnapshot{Stream: s, Groups: []core.GroupSnapshot{{Group: g, Profile: p}}}, EndedAt: &now}
	// key содержит идентичность профиля; canonical содержит точные канонические байты; err содержит
	// отказ их вычисления для снимка артефакта.
	key, canonical, err := core.ProfileIdentity(p.Profile)
	if err != nil {
		t.Fatal(err)
	}
	// output содержит самостоятельный исторический снимок назначения, группы, профиля и заглушки.
	output := core.SessionOutput{Record: core.Record{ID: "saved-output", Revision: 1, CreatedAt: now, UpdatedAt: now}, SessionID: session.ID, Generation: 1, Snapshot: core.OutputSnapshot{Destination: d, Group: g, Profile: p, Fallback: s.Fallback, Artifact: core.ArtifactManifest{GenerationID: "saved-generation", VariantID: "saved-variant", AssetID: s.Fallback.AssetID, SourceHash: "synthetic-source-hash", Profile: p.Profile, ProfileKey: key, CanonicalProfile: canonical, GeneratorVersion: "synthetic-generator", ManagedPath: "saved/variant.mp4", SizeBytes: 100, ContentHash: "synthetic-content-hash", VerifiedFormat: p.Profile.Format}}}
	// err содержит отказ атомарной вставки исторических Session и Output.
	if err := f.store.Write(context.Background(), func(r core.Repositories) error {
		// Сохраняет Session и Output одной транзакцией для проверки независимости исторических снимков от
		// конфигурации.

		// err содержит отказ вставки родительской исторической Session перед её выходом.
		if err := r.Runtime.InsertSession(context.Background(), session); err != nil {
			return err
		}
		return r.Runtime.InsertOutput(context.Background(), output)
	}); err != nil {
		t.Fatal(err)
	}
	return session, output
}

// readbackHistory сверяет сохранённые ключи, адреса, ревизии и политику в чтении Session/Output и
// применимых списках, отдельно учитывая удалённый Stream.
func readbackHistory(t *testing.T, handler http.Handler, s core.Session, o core.SessionOutput, streamExists bool) {
	t.Helper()
	// sessionPath адресует историческую Session во всех проверяемых маршрутах.
	sessionPath := "/sessions/" + string(s.ID)
	// sessions собирает одиночное и списочные представления той же исторической Session.
	sessions := []map[string]any{readbackObject(t, handler, "GET", sessionPath, "", 200)}
	sessions = append(sessions, readbackItems(t, readbackObject(t, handler, "GET", "/sessions", "", 200), 1)...)
	if streamExists {
		sessions = append(sessions, readbackItems(t, readbackObject(t, handler, "GET", "/streams/"+string(s.StreamID)+"/sessions", "", 200), 1)...)
	}
	// v содержит очередное представление Session для проверки снимка Stream.
	for _, v := range sessions {
		// stream содержит сохранённый в Session снимок исходных имени, ключа и ревизии Stream.
		stream := v["stream"].(map[string]any)
		readbackFields(t, stream, map[string]string{"id": string(s.StreamID), "publication_key": s.Snapshot.Stream.PublicationKey, "name": s.Snapshot.Stream.Name, "revision": fmt.Sprint(s.Snapshot.Stream.Revision)})
		// policy содержит сохранённую в снимке политику заглушки с исходным AssetID и сроками.
		policy := stream["fallback"].(map[string]any)
		readbackFields(t, policy, map[string]string{"asset_id": string(s.Snapshot.Stream.Fallback.AssetID)})
		readbackFields(t, policy["watchdog"].(map[string]any), map[string]string{"video_timeout_ns": fmt.Sprint(s.Snapshot.Stream.Fallback.Watchdog.VideoTimeout.Nanoseconds())})
	}
	// outputs собирает страничное и одиночное представления исторического выхода.
	outputs := readbackItems(t, readbackObject(t, handler, "GET", sessionPath+"/outputs", "", 200), 1)
	outputs = append(outputs, readbackObject(t, handler, "GET", sessionPath+"/outputs/"+string(o.ID), "", 200))
	// v содержит очередное представление Output для проверки самостоятельных назначения и политики.
	for _, v := range outputs {
		readbackFields(t, v["destination"].(map[string]any), map[string]string{"id": string(o.Snapshot.Destination.ID), "endpoint": o.Snapshot.Destination.Endpoint, "stream_key": o.Snapshot.Destination.StreamKey, "revision": "1"})
		readbackFields(t, v["fallback"].(map[string]any), map[string]string{"asset_id": string(o.Snapshot.Fallback.AssetID)})
	}
}

// Проверяет неизменность заполненных записей после повторного открытия SQLite и независимость
// исторических снимков от изменения и удаления текущих настроек.
func TestAcceptanceSavedConfigurationAndHistorySurviveReopenChangesAndDeletion(t *testing.T) {
	// f содержит исходный API и SQLite сценария повторного открытия и удаления конфигурации.
	f := newAPIAcceptanceFixture(t, nil)
	// s содержит исходный Stream; p содержит профиль; g содержит группу; d содержит полное
	// назначение для исторических снимков.
	s, p, g, d := apiAcceptanceSeedConfiguration(t, f)
	// asset содержит заглушку снимка; err содержит отказ её создания.
	asset, err := f.service.CreateAsset(context.Background(), "Заглушка снимка")
	if err != nil {
		t.Fatal(err)
	}
	s.Fallback = core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: 2 * time.Second, RecoveryWindow: 3 * time.Second, KeyframeTimeout: 4 * time.Second}}
	s, err = f.service.UpdateStream(context.Background(), s, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	// session содержит сохранённый завершённый запуск; output содержит его самостоятельный
	// исторический выход.
	session, output := readbackSeedHistory(t, f, s, p, g, d)
	// err содержит результат закрытия исходного API перед повторным открытием БД.
	if err := f.api.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// err содержит результат закрытия исходного Store перед новым владельцем.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	// store содержит повторно открытый файл той же временной SQLite.
	store, err := storage.Open(context.Background(), f.databasePath, apiAcceptanceStorageOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает повторно открытое временное хранилище после проверки истории.

		// err содержит результат закрытия повторно открытого Store при очистке теста.
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	// svc содержит новый доменный сервис текущего incarnation для чтения неизменённых записей.
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// handler содержит настоящий API поверх повторно открытого Store.
	handler, err := api.New(api.Config{Core: svc, Commands: svc, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Limits: apiAcceptanceLimits()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает API, работающий с повторно открытым хранилищем.

		// err содержит результат закрытия нового API при очистке.
		if err := handler.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	// unchanged повторно сравнивает все сохранённые конфигурационные и исторические записи с
	// исходными объектами.
	unchanged := func() {
		// Сравнивает конфигурацию и историю после чтения с исходными записями, выявляя побочные изменения.

		t.Helper()
		// checks связывает каждую исходную запись с отдельным доменным чтением после
		// HTTP-запросов.
		checks := []struct {
			// want содержит исходную запись для точного сравнения после повторного открытия.
			want any
			// get читает соответствующую конфигурационную или историческую запись из нового
			// сервиса.
			get func() (any, error)
		}{
			{s, func() (any, error) {
				// Читает сохранённый поток для сравнения с исходной записью после повторного открытия.
				return svc.GetStream(context.Background(), s.ID)
			}},
			{p, func() (any, error) {
				// Читает сохранённый профиль для проверки неизменности его представления.
				return svc.GetProfile(context.Background(), p.ID)
			}},
			{g, func() (any, error) {
				// Читает сохранённую группу для сравнения её связей и ревизии.
				return svc.GetGroup(context.Background(), g.ID)
			}},
			{d, func() (any, error) {
				// Читает сохранённое назначение для проверки точного сохранения настроек.
				return svc.GetDestination(context.Background(), d.ID)
			}},
			{session, func() (any, error) {
				// Читает историческую Session для проверки сохранности снимка запуска.
				return svc.GetSession(context.Background(), session.ID)
			}},
			{output, func() (any, error) {
				// Читает исторический Output для проверки сохранности снимка назначения.
				return svc.GetOutput(context.Background(), output.ID)
			}},
		}
		// check содержит очередную пару эталона и функции чтения.
		for _, check := range checks {
			// got содержит фактически прочитанную запись; err содержит отказ её чтения.
			got, err := check.get()
			if err != nil || !reflect.DeepEqual(got, check.want) {
				t.Fatalf("сохранённая запись изменилась: got=%+v want=%+v err=%v", got, check.want, err)
			}
		}
	}
	unchanged()
	readbackFields(t, readbackObject(t, handler, "GET", "/streams/"+string(s.ID), "", 200), map[string]string{"publication_key": s.PublicationKey})
	readbackFields(t, readbackItems(t, readbackObject(t, handler, "GET", "/streams", "", 200), 1)[0], map[string]string{"publication_key": s.PublicationKey})
	// path задаёт очередной одиночный или списочный маршрут назначения после повторного открытия.
	for _, path := range []string{"/destinations/" + string(d.ID), "/destinations", "/streams/" + string(s.ID) + "/destinations"} {
		// v содержит сохранённое назначение либо его единственную страницу для сверки точных
		// полей.
		v := readbackObject(t, handler, "GET", path, "", 200)
		if strings.HasSuffix(path, "/destinations") {
			v = readbackItems(t, v, 1)[0]
		}
		readbackFields(t, v, map[string]string{"endpoint": d.Endpoint, "stream_key": d.StreamKey})
	}
	readbackHistory(t, handler, session, output, true)
	unchanged()
	// changedDestination содержит ответ изменения текущего назначения, которое не должно менять
	// исторический Output.
	changedDestination := readbackObject(t, handler, "PATCH", "/destinations/"+string(d.ID), `{"expected_revision":"1","endpoint":"rtmp://localhost/changed?token=new","stream_key":"","name":"Следующее назначение"}`, 200)
	readbackFields(t, changedDestination, map[string]string{"endpoint": "rtmp://localhost/changed?token=new", "stream_key": "", "name": "Следующее назначение", "revision": "2"})
	// changedStream содержит ответ переименования и очистки текущей политики, которые не должны
	// менять историческую Session.
	changedStream := readbackObject(t, handler, "PATCH", "/streams/"+string(s.ID), `{"expected_revision":"2","name":"Следующий поток","fallback":null}`, 200)
	readbackFields(t, changedStream, map[string]string{"name": "Следующий поток", "publication_key": s.PublicationKey, "revision": "3"})
	if changedStream["fallback"] != nil {
		t.Fatal("текущая политика заглушки не очищена", changedStream)
	}
	readbackHistory(t, handler, session, output, true)
	// target задаёт очередной ресурс текущей конфигурации с правильной ревизией для удаления.
	for _, target := range []string{"/destinations/" + string(d.ID) + "?expected_revision=2", "/transcode-groups/" + string(g.ID) + "?expected_revision=1", "/streams/" + string(s.ID) + "?expected_revision=3"} {
		apiAcceptanceRequest(t, handler, "DELETE", target, "", 204)
	}
	apiAcceptanceRequest(t, handler, "GET", "/streams/"+string(s.ID)+"/sessions", "", 404)
	apiAcceptanceRequest(t, handler, "GET", "/destinations/"+string(d.ID), "", 404)
	readbackHistory(t, handler, session, output, false)
	// gotSession содержит историческую Session после удаления конфигурации.
	gotSession, err := svc.GetSession(context.Background(), session.ID)
	if err != nil || !reflect.DeepEqual(gotSession, session) {
		t.Fatal("изменена историческая Session", gotSession, err)
	}
	// gotOutput содержит исторический Output после удаления назначения и группы.
	gotOutput, err := svc.GetOutput(context.Background(), output.ID)
	if err != nil || !reflect.DeepEqual(gotOutput, output) {
		t.Fatal("изменён исторический Output", gotOutput, err)
	}
}

// readbackSchemaFields требует документированные обязательные строки в выходной схеме; stream_key
// должен допускать сохранённую пустую строку без лишних ограничений.
func readbackSchemaFields(t *testing.T, schema map[string]any, fields ...string) {
	t.Helper()
	// properties содержит свойства проверяемой выходной схемы.
	properties := schema["properties"].(map[string]any)
	// required содержит имена обязательных полей схемы; ok подтверждает их массив.
	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatal("выходная схема не объявляет обязательные поля", schema)
	}
	// field задаёт очередное поле полного ключа или адреса для проверки схемы.
	for _, field := range fields {
		// property содержит описание текущего поля; ok подтверждает наличие объектной схемы.
		property, ok := properties[field].(map[string]any)
		if !ok || property["type"] != "string" {
			t.Fatalf("выходная схема не содержит строку %s: %#v", field, property)
		}
		// description содержит документированное пояснение поля для проверки непустого текста.
		description, ok := property["description"].(string)
		if !ok || strings.TrimSpace(description) == "" {
			t.Fatal("поле не описано", field, property)
		}
		// found накапливает присутствие текущего поля в required.
		found := false
		// name содержит очередное обязательное имя, сравниваемое с проверяемым полем.
		for _, name := range required {
			found = found || name == field
		}
		if !found {
			t.Fatal("всегда присутствующее поле не обязательно в схеме", field)
		}
		if field == "stream_key" {
			// minimum содержит объявленную минимальную длину ключа; ok отмечает наличие
			// ограничения, которое не должно запрещать пустую строку.
			if minimum, ok := property["minLength"].(float64); ok && minimum > 0 {
				t.Fatal("схема запрещает пустой ключ", property)
			}
			if property["pattern"] != nil || property["enum"] != nil || property["const"] != nil {
				t.Fatal("схема необоснованно ограничивает сохранённый ключ", property)
			}
		}
	}
}

// Проверяет наличие документированных полных ключей и адресов во всех выходных OpenAPI-схемах,
// публичные ссылки вложенных схем и отсутствие флагов *_configured в JSON/YAML.
func TestAcceptanceOutputSchemasDescribeReadableConfigurationAndHistory(t *testing.T) {
	// f содержит настоящий API для проверки выходных схем конфигурации и истории.
	f := newAPIAcceptanceFixture(t, nil)
	// spec содержит опубликованный OpenAPI JSON всех зарегистрированных маршрутов.
	spec := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/openapi.json", "", 200))
	readbackNoConfiguredFlags(t, spec)
	// schemas индексирует компоненты схем для разрешения внутренних $ref.
	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
	// resolve возвращает целевую схему внутренней ссылки либо исходный объект и отклоняет
	// неизвестную ссылку.
	resolve := func(value map[string]any) map[string]any {
		// Разрешает внутреннюю ссылку $ref в проверяемой спецификации и отклоняет неизвестные ссылки.

		// ref содержит строковую ссылку компонента; ok подтверждает наличие $ref.
		if ref, ok := value["$ref"].(string); ok {
			// prefix задаёт единственный допустимый префикс внутренних ссылок OpenAPI.
			const prefix = "#/components/schemas/"
			if !strings.HasPrefix(ref, prefix) {
				t.Fatal("неизвестная ссылка схемы", ref)
			}
			// result содержит найденную схему компонента; ok подтверждает разрешимость
			// ссылки.
			result, ok := schemas[strings.TrimPrefix(ref, prefix)].(map[string]any)
			if !ok {
				t.Fatal("ссылка не разрешается", ref)
			}
			return result
		}
		return value
	}
	// tc задаёт ответ маршрута и место полного конфигурационного снимка внутри его схемы.
	for _, tc := range []struct {
		// method задаёт операцию OpenAPI; path задаёт маршрут; status выбирает проверяемый
		// успешный ответ.
		method, path, status string
		// list отмечает схему страницы, из которой требуется выбрать items.
		list bool
		// nested задаёт поле исторического снимка stream или destination внутри ответа.
		nested string
		// fields задаёт обязательные документированные строки полного ключа и адреса.
		fields []string
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
			// Проверяет поля конфигурации в схеме выбранного ответа, включая вложенные исторические снимки и
			// элементы списка.

			// operation содержит описание текущего метода зарегистрированного пути.
			operation := spec["paths"].(map[string]any)[api.Prefix+tc.path].(map[string]any)[tc.method].(map[string]any)
			// response содержит схему выбранного успешного HTTP-статуса.
			response := operation["responses"].(map[string]any)[tc.status].(map[string]any)
			// schema содержит разрешённую схему ответа и далее уточняется до элемента списка
			// или вложенного снимка.
			schema := resolve(response["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any))
			if tc.list {
				// items содержит описание массива элементов страницы.
				items := schema["properties"].(map[string]any)["items"].(map[string]any)
				// arrayType накапливает подтверждение типа array, включая объединённое
				// представление типов.
				arrayType := items["type"] == "array"
				// types содержит массив допустимых типов items; ok подтверждает такое
				// представление схемы.
				if types, ok := items["type"].([]any); ok {
					// kind содержит очередной тип items для поиска array.
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
	// name выбирает отдельную опубликованную схему конфигурации или исторического снимка.
	for _, name := range []string{"StreamDTO", "DestinationDTO", "SessionDTO", "OutputDTO", "OutputDetailDTO"} {
		// schema содержит JSON отдельной публичной схемы для проверки полей и внешних ссылок.
		schema := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/schemas/"+name+".json", "", 200))
		readbackNoConfiguredFlags(t, schema)
		switch name {
		case "StreamDTO":
			readbackSchemaFields(t, schema, "publication_key")
		case "DestinationDTO":
			readbackSchemaFields(t, schema, "endpoint", "stream_key")
		default:
			// field задаёт вложенный снимок; target задаёт ожидаемое имя его отдельной
			// публичной схемы.
			field, target := "destination", "DestinationDTO"
			if name == "SessionDTO" {
				field, target = "stream", "StreamDTO"
			}
			// ref содержит фактический публичный URL вложенной схемы.
			ref := schema["properties"].(map[string]any)[field].(map[string]any)["$ref"]
			if ref != api.Prefix+"/schemas/"+target+".json" {
				t.Fatal("вложенная публичная схема потеряла ссылку", ref)
			}
		}
	}
	// yaml содержит опубликованную OpenAPI YAML для проверки отсутствия прежних флагов.
	yaml := apiAcceptanceRequest(t, f.api, "GET", "/openapi.yaml", "", 200).Body.String()
	// old задаёт очередное прежнее поле *_configured, которое не должно встречаться в спецификации.
	for _, old := range []string{"publication_key_configured", "endpoint_configured", "stream_key_configured"} {
		if strings.Contains(yaml, old) {
			t.Fatal("удалённый флаг остался в YAML", old)
		}
	}
}
