package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

const (
	// fixtureAPIKey задаёт синтетический ключ для проверки transport metadata.
	fixtureAPIKey = "fixture-google-key"
	// fixtureChannelID содержит постоянный публичный канал, по которому ищутся эфиры.
	fixtureChannelID = "UCabcdefghijklmnopqrstuv"
	// fixtureVideoID идентифицирует один синтетический эфир.
	fixtureVideoID = "video-00001"
	// fixtureChatID адресует чат этого эфира и отличается от идентификатора видео.
	fixtureChatID = "synthetic-chat-id"
	// fixtureStarted содержит фактическое время начала в формате YouTube Data API.
	fixtureStarted = "2026-11-12T12:14:16Z"
)

// newYouTubeFixture создаёт адаптер с реальными границами limits для изолированного теста t.
func newYouTubeFixture(t *testing.T, limits core.ChatLimits) *Adapter {
	t.Helper()
	// adapter обрабатывает протоколы самостоятельно, err сообщает об ошибке инициализации.
	adapter, err := New(limits)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

// useRESTFixture направляет a на HTTPS-сервер handler, который закрывается по завершении t.
func useRESTFixture(t *testing.T, a *Adapter, handler http.HandlerFunc) {
	t.Helper()
	// server обслуживает тестовые ответы на выделенном локальном порту.
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	a.restBase = server.URL + "/youtube/v3"
	// transport сохраняет настройки таймаутов, лимитов и повторов реального транспорта.
	transport := a.httpClient.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	a.httpClient.Transport = transport
	t.Cleanup(transport.CloseIdleConnections)
}

// fixtureYouTubeAccess возвращает допустимый Source с постоянным channel и credentials для key.
func fixtureYouTubeAccess(channel, key string) (core.ChatSource, core.ChatCredentials) {
	// record задаёт воспроизводимую начальную ревизию синтетических записей.
	record := core.Record{ID: "source-fixture", Revision: 1, CreatedAt: time.Unix(10, 0).UTC(), UpdatedAt: time.Unix(10, 0).UTC()}
	return core.ChatSource{
		Record: record, DestinationID: "fixture-destination", AccountID: "fixture-account", Provider: core.ChatYouTube,
		ChannelHint: channel, Enabled: true, Options: core.ChatOptions{Version: 1, YouTube: &core.YouTubeChatOptions{}},
	}, core.ChatCredentials{
		AccountID: "fixture-account", Provider: core.ChatYouTube, CredentialRevision: 1,
		CredentialUpdatedAt: record.CreatedAt, CredentialWriteID: "fixture-write",
		Bundle: core.ChatCredentialBundle{YouTube: &core.YouTubeChatCredentials{APIKey: key}},
	}
}

// writeFixtureJSON отвечает тесту t JSON с HTTP-кодом code либо отмечает отказ записи в w.
func writeFixtureJSON(t *testing.T, w http.ResponseWriter, code int, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// err сообщает о прерывании локального обмена при записи JSON.
	if _, err := fmt.Fprint(w, body); err != nil {
		t.Error(err)
	}
}

// checkFixtureGET сверяет форму запроса r и гарантирует передачу key только через заголовок.
func checkFixtureGET(t *testing.T, r *http.Request, path, key string) {
	t.Helper()
	if r.Method != http.MethodGet || r.URL.Path != "/youtube/v3/"+path || r.TLS == nil {
		t.Errorf("Запрос не соответствует HTTPS GET /youtube/v3/%s: %s %s", path, r.Method, r.URL.Path)
	}
	if !slices.Equal(r.Header.Values("x-goog-api-key"), []string{key}) ||
		r.Header.Get("Authorization") != "" || r.URL.Query().Has("key") || strings.Contains(r.URL.RawQuery, key) {
		t.Error("Неверный способ передачи ключа YouTube")
	}
}

// TestValidateAPIKeyWithoutLiveBroadcast проверяет изолированную проверку ключа, включая штатные
// ошибки Google и отказ HTTP-транспорта. Для валидного ключа достаточно пустой выдачи mostPopular.
func TestValidateAPIKeyWithoutLiveBroadcast(t *testing.T) {
	// cases связывает ответ Google либо отказ транспорта с безопасной причиной операции.
	cases := []struct {
		// name обозначает наблюдаемую категорию ответа.
		name string
		// code и body задают ответ HTTPS, code=0 имитирует отсутствие HTTP-ответа.
		code int
		body string
		// reason ожидается от валидатора без диагностического текста Google.
		reason core.ChatReason
	}{
		{"valid", 200, `{"items":[]}`, core.ChatReasonNone},
		{"invalid", 400, `{"error":{"message":"fixture-google-key","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"API_KEY_INVALID","domain":"googleapis.com"}]}}`, core.ChatReasonCredentials},
		{"forbidden", 403, `{"error":{"errors":[{"reason":"forbidden","message":"fixture-google-key"}]}}`, core.ChatReasonCredentials},
		{"quota", 403, `{"error":{"errors":[{"reason":"quotaExceeded","message":"fixture-google-key"}]}}`, core.ChatReasonQuota},
		{"temporarily unavailable", 503, `{"error":{"message":"fixture-google-key"}}`, core.ChatReasonNetwork},
		{"bad response", 200, `{"error":`, core.ChatReasonNetwork},
		{"transport", 0, "", core.ChatReasonNetwork},
	}
	// tc содержит один независимый исход проверки ключа.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Подтест начинает новый бюджет и не использует состояние других ответов.
			// adapter владеет проверкой конечного HTTP-запроса, calls считает обращения к API.
			adapter := newYouTubeFixture(t, core.DefaultChatLimits())
			var calls atomic.Int32
			useRESTFixture(t, adapter, func(w http.ResponseWriter, r *http.Request) {
				// Обработчик проверяет именно mostPopular и имитирует HTTP-ответ либо разрыв HTTP/1.1.
				calls.Add(1)
				checkFixtureGET(t, r, "videos", fixtureAPIKey)
				// query содержит фильтр проверки ключа; ему не нужны видео, канал или эфир.
				query := r.URL.Query()
				if query.Get("chart") != "mostPopular" || query.Has("id") || query.Has("channelId") ||
					query.Has("eventType") || query.Get("part") == "" {
					t.Error("Проверка ключа зависит от наличия эфира")
				}
				if tc.code == 0 {
					// conn и err позволяют закрыть TCP-фрагмент до отправки HTTP-заголовков.
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					if err := conn.Close(); err != nil {
						t.Error(err)
					}
					return
				}
				writeFixtureJSON(t, w, tc.code, tc.body)
			})
			// reason и err отражают состояние авторизации и безопасный отказ.
			reason, err := adapter.ValidateAPIKey(t.Context(), fixtureAPIKey)
			if reason != tc.reason || tc.reason == core.ChatReasonNone && err != nil {
				t.Fatalf("Результат проверки ключа: %q, ошибка=%t, ожидалась причина %q", reason, err != nil, tc.reason)
			}
			if err != nil && strings.Contains(err.Error(), fixtureAPIKey) {
				t.Error("Ошибка раскрыла ключ")
			}
			// Проверка числа запросов выполняется только после возврата клиента.
			if calls.Load() != 1 {
				t.Errorf("Число запросов проверки ключа = %d, ожидался один запрос", calls.Load())
			}
		})
	}
}

// TestRESTDoesNotFollowRedirect проверяет, что редирект запроса с ключом не достигает другого сервера.
func TestRESTDoesNotFollowRedirect(t *testing.T) {
	// redirected сообщает о любом сетевом обращении к целевому HTTPS-серверу.
	redirected := make(chan struct{}, 1)
	// target принимает HTTPS только если клиент ошибочно следует за переадресацией.
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Обработчик подаёт сигнал даже в случае, если заголовок ключа уже был удалён.
		redirected <- struct{}{}
		writeFixtureJSON(t, w, 200, `{"items":[]}`)
	}))
	t.Cleanup(target.Close)
	// adapter выполняет запрос с ключом к локальному адресу, который возвращает редирект.
	adapter := newYouTubeFixture(t, core.DefaultChatLimits())
	useRESTFixture(t, adapter, func(w http.ResponseWriter, r *http.Request) {
		// Обработчик отправляет корректное абсолютное назначение редиректа.
		checkFixtureGET(t, r, "videos", fixtureAPIKey)
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	// reason и err должны отличить переадресацию от подтверждённой авторизации.
	reason, err := adapter.ValidateAPIKey(t.Context(), fixtureAPIKey)
	if reason == core.ChatReasonNone && err == nil {
		t.Fatal("Переадресация выдана за успешную авторизацию")
	}
	select {
	case <-redirected:
		t.Fatal("Запрос с ключом последовал за переадресацией")
	default:
	}
}

// fixtureSearchPage возвращает JSON выдачи search.list с указанными ids и продолжением next.
func fixtureSearchPage(t *testing.T, next string, ids ...string) string {
	t.Helper()
	// items содержит видео в той последовательности, которую вернул поиск.
	items := make([]any, 0, len(ids))
	// id берётся из заданной страницы и используется как идентификатор searchResult.
	for _, id := range ids {
		items = append(items, map[string]any{"id": map[string]string{"kind": "youtube#video", "videoId": id},
			"snippet": map[string]string{"channelId": fixtureChannelID, "liveBroadcastContent": "live", "title": "Непроверенный заголовок"}})
	}
	return fixtureJSON(t, map[string]any{"items": items, "nextPageToken": next})
}

// fixtureVideo моделирует один ответ videos.list с публичностью privacy, состоянием live
// и временем завершения ended. Аргументы id, channel и chat задают возвращённые идентификаторы.
func fixtureVideo(id, channel, chat, privacy, live, ended string) map[string]any {
	// streaming содержит фактические даты только для уже начавшегося или завершённого эфира.
	streaming := map[string]string{}
	if live == "live" || ended != "" {
		streaming["actualStartTime"] = fixtureStarted
	}
	if ended != "" {
		streaming["actualEndTime"] = ended
	}
	if chat != "" {
		streaming["activeLiveChatId"] = chat
	}
	return map[string]any{
		"id": id, "snippet": map[string]string{"channelId": channel, "title": "Подтверждённый эфир", "liveBroadcastContent": live},
		"status":               map[string]string{"privacyStatus": privacy},
		"liveStreamingDetails": streaming,
	}
}

// fixtureJSON кодирует синтетический ответ value либо сообщает t об ошибке фикстуры.
func fixtureJSON(t *testing.T, value any) string {
	t.Helper()
	// encoded содержит ответ без сторонних библиотек, err сообщает об ошибке сериализации.
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Error(err)
	}
	return string(encoded)
}

// checkDiscoveryParts требует от videos.list метаданные, которые подтверждают все свойства эфира.
func checkDiscoveryParts(t *testing.T, r *http.Request) {
	t.Helper()
	// parts содержит фактически запрошенные разделы публичных метаданных видео.
	parts := strings.Split(r.URL.Query().Get("part"), ",")
	for _, required := range []string{"snippet", "status", "liveStreamingDetails"} {
		// required задаёт один необходимый раздел данных.
		if !slices.Contains(parts, required) {
			t.Errorf("В videos.list нет раздела %s", required)
		}
	}
}

// TestDiscoverConfirmsEveryPageAndVideo проверяет полный просмотр search.list, дедупликацию
// совпавших ID и независимую проверку публичности, канала, ID и факта текущего эфира.
func TestDiscoverConfirmsEveryPageAndVideo(t *testing.T) {
	// adapter обрабатывает протоколы, source и credentials задают рабочие реквизиты канала.
	adapter := newYouTubeFixture(t, core.DefaultChatLimits())
	source, credentials := fixtureYouTubeAccess(fixtureChannelID, fixtureAPIKey)
	// searchTokens и videoQueries фиксируют отправленные параметры для проверки полного прохода.
	searchTokens := make(chan string, 8)
	videoQueries := make(chan string, 8)
	useRESTFixture(t, adapter, func(w http.ResponseWriter, r *http.Request) {
		// Обработчик возвращает два продолжения, а videos.list проверяет ID пакетно.
		// query хранит параметры именно этого HTTPS-запроса.
		query := r.URL.Query()
		if strings.HasSuffix(r.URL.Path, "/search") {
			checkFixtureGET(t, r, "search", fixtureAPIKey)
			if query.Get("part") != "snippet" || query.Get("channelId") != fixtureChannelID ||
				query.Get("eventType") != "live" || query.Get("type") != "video" || query.Get("maxResults") != "50" {
				t.Errorf("Неверные фильтры поиска: %v", query)
			}
			// next указывает, какую страницу запрашивает клиент.
			next := query.Get("pageToken")
			searchTokens <- next
			switch next {
			case "":
				writeFixtureJSON(t, w, 200, fixtureSearchPage(t, "opaque+second/=", fixtureVideoID, "video-00002", "video-00003"))
			case "opaque+second/=":
				writeFixtureJSON(t, w, 200, fixtureSearchPage(t, "", fixtureVideoID, "video-00004", "video-00005", "video-00006"))
			default:
				t.Errorf("Искажён токен следующей страницы: %q", next)
				writeFixtureJSON(t, w, 400, `{}`)
			}
			return
		}
		checkFixtureGET(t, r, "videos", fixtureAPIKey)
		checkDiscoveryParts(t, r)
		videoQueries <- query.Get("id")
		// videos содержит подходящий эфир и находки, которые нельзя публиковать как подтверждённые.
		videos := []map[string]any{
			fixtureVideo("video-99999", fixtureChannelID, "foreign-video-chat", "public", "live", ""),
			fixtureVideo("video-00002", "UCdifferent", "foreign-channel-chat", "public", "live", ""),
			fixtureVideo("video-00003", fixtureChannelID, "hidden-chat", "unlisted", "live", ""),
			fixtureVideo("video-00004", fixtureChannelID, "ended-chat", "public", "live", fixtureStarted),
			fixtureVideo("video-00005", fixtureChannelID, "scheduled-chat", "public", "upcoming", ""),
			fixtureVideo("video-00006", fixtureChannelID, "non-live-chat", "public", "none", ""),
			fixtureVideo(fixtureVideoID, fixtureChannelID, fixtureChatID, "public", "live", ""),
		}
		// items содержит подтверждения запрошенных видео и одну неожиданную запись Google.
		items := make([]any, 0, len(videos))
		// video содержит очередной предложенный результат, часть из них снята с поиска.
		for _, video := range videos {
			if slices.Contains(strings.Split(query.Get("id"), ","), video["id"].(string)) || video["id"] == "video-99999" {
				items = append(items, video)
			}
		}
		writeFixtureJSON(t, w, 200, fixtureJSON(t, map[string]any{"items": items}))
	})
	// found содержит только подтверждённого кандидата. err сообщает об отказе протокола.
	found, err := adapter.Discover(t.Context(), source, credentials)
	if err != nil || len(found.Broadcasts) != 1 {
		t.Fatalf("Получено эфиров: %d, ошибка=%v, состояние=%s/%s", len(found.Broadcasts), err, found.State, found.Reason)
	}
	// only сохраняет видео и фактический чат того же публичного эфира.
	only := found.Broadcasts[0]
	if only.VideoID != fixtureVideoID || only.ChannelID != fixtureChatID || only.Title != "Подтверждённый эфир" ||
		only.StartedAt == nil || only.StartedAt.Format(time.RFC3339) != fixtureStarted {
		t.Fatalf("Поля подтверждённого эфира расходятся с videos.list: %+v", only)
	}
	// tokens содержит реально использованные страницы, ids собирает все запрошенные идентификаторы видео.
	var tokens []string
	for len(searchTokens) > 0 {
		tokens = append(tokens, <-searchTokens)
	}
	var ids []string
	for len(videoQueries) > 0 {
		ids = append(ids, strings.Split(<-videoQueries, ",")...)
	}
	slices.Sort(ids)
	if !slices.Equal(tokens, []string{"", "opaque+second/="}) || !slices.Equal(ids, []string{
		fixtureVideoID, "video-00002", "video-00003", "video-00004", "video-00005", "video-00006",
	}) {
		t.Fatalf("Поиск покрыл не все страницы или проверял дубликаты: %v, %v", tokens, ids)
	}
}

// TestDiscoverWaitsForKnownVideoWithoutAnotherSearch проверяет полное обнаружение эфира без
// activeLiveChatId и повторный опрос одного видео до появления чата через videos.list.
func TestDiscoverWaitsForKnownVideoWithoutAnotherSearch(t *testing.T) {
	// adapter использует тот же бюджет, source и credentials адресуют тот же постоянный канал.
	adapter := newYouTubeFixture(t, core.DefaultChatLimits())
	source, credentials := fixtureYouTubeAccess(fixtureChannelID, fixtureAPIKey)
	// calls хранит путь и id запроса, а answers задаёт последовательность ответов видео.
	calls := make(chan string, 8)
	answers := make(chan string, 2)
	answers <- ""
	answers <- fixtureChatID
	close(answers)
	useRESTFixture(t, adapter, func(w http.ResponseWriter, r *http.Request) {
		// Поиск даёт один эфир, у которого чат появляется только во второй выдаче videos.list.
		if strings.HasSuffix(r.URL.Path, "/search") {
			calls <- "search"
			writeFixtureJSON(t, w, 200, fixtureSearchPage(t, "", fixtureVideoID))
			return
		}
		// id содержит видео, которое адаптер повторно проверяет.
		id := r.URL.Query().Get("id")
		calls <- id
		writeFixtureJSON(t, w, 200, fixtureJSON(t, map[string]any{"items": []any{
			fixtureVideo(fixtureVideoID, fixtureChannelID, <-answers, "public", "live", ""),
		}}))
	})
	// found подтверждает наличие эфира при отсутствии чата, err сообщает отказ транспорта.
	found, err := adapter.Discover(t.Context(), source, credentials)
	if err != nil || found.State != core.ChatWaitingChat || found.Reason != core.ChatReasonUnavailable ||
		len(found.Broadcasts) != 1 || found.Broadcasts[0].VideoID != fixtureVideoID || found.Broadcasts[0].ChannelID != "" {
		t.Fatalf("Эфир без чата не сохранён для уточняющего опроса: %+v, %v", found, err)
	}
	// confirmed отражает повторную проверку того же VideoID.
	confirmed, err := adapter.DiscoverBroadcast(t.Context(), source, credentials, fixtureVideoID)
	if err != nil || confirmed.ChannelID != fixtureChatID &&
		(len(confirmed.Broadcasts) != 1 || confirmed.Broadcasts[0].VideoID != fixtureVideoID || confirmed.Broadcasts[0].ChannelID != fixtureChatID) {
		t.Fatalf("Повторная проверка не подтвердила чат: %+v, %v", confirmed, err)
	}
	// requests собирает завершённые запросы в порядке отправки.
	var requests []string
	for len(calls) > 0 {
		requests = append(requests, <-calls)
	}
	if !slices.Equal(requests, []string{"search", fixtureVideoID, fixtureVideoID}) {
		t.Fatalf("Ожидание чата выполнило посторонний поиск: %v", requests)
	}
}

// TestDiscoverBroadcastRequiresEvidenceForMissingVideo проверяет, что пустой список
// подтверждает отсутствие запрошенного видео, а список с чужим ID требует повторной проверки.
func TestDiscoverBroadcastRequiresEvidenceForMissingVideo(t *testing.T) {
	// cases сопоставляет три независимых ответа videos.list с безопасными результатами обнаружения.
	cases := []struct {
		// name обозначает связь выданного видео с точным запросом.
		name string
		// id задаёт возвращаемое видео. Пустое значение означает корректный пустой массив.
		id string
		// reason задаёт причину, по которой Manager отличает отсутствие видео от временного отказа.
		reason core.ChatReason
	}{
		{"foreign video", "video-00002", core.ChatReasonNetwork},
		{"empty result", "", core.ChatReasonNoBroadcast},
		{"exact video", fixtureVideoID, core.ChatReasonNone},
	}
	// tc определяет ответ HTTPS-сервера для одного изолированного запроса.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Подтест проверяет только повторный запрос конкретного видео.
			// adapter выполняет запрос, source и credentials выбирают ранее обнаруженный канал.
			adapter := newYouTubeFixture(t, core.DefaultChatLimits())
			source, credentials := fixtureYouTubeAccess(fixtureChannelID, fixtureAPIKey)
			// items представляет выдачу Google, в которой чужое видео тоже публично и находится в эфире.
			items := []any{}
			if tc.id != "" {
				items = append(items, fixtureVideo(tc.id, fixtureChannelID, fixtureChatID, "public", "live", ""))
			}
			// calls считает запросы к проверке известного видео.
			var calls atomic.Int32
			useRESTFixture(t, adapter, func(w http.ResponseWriter, r *http.Request) {
				// Обработчик принимает только точную проверку сохранённого видео.
				calls.Add(1)
				checkFixtureGET(t, r, "videos", fixtureAPIKey)
				if r.URL.Query().Get("id") != fixtureVideoID {
					t.Error("В запросе изменился ID проверяемого видео")
				}
				writeFixtureJSON(t, w, http.StatusOK, fixtureJSON(t, map[string]any{"items": items}))
			})
			// found и err содержат классификацию ответа, которую получит Manager.
			found, err := adapter.DiscoverBroadcast(t.Context(), source, credentials, fixtureVideoID)
			if err != nil || found.Reason != tc.reason || tc.reason == core.ChatReasonNetwork && found.State != core.ChatRetrying || calls.Load() != 1 {
				t.Fatalf("Неверный результат проверки видео: %+v, %v, запросов %d", found, err, calls.Load())
			}
			if tc.id == fixtureVideoID {
				if len(found.Broadcasts) != 1 || found.Broadcasts[0].VideoID != fixtureVideoID || found.Broadcasts[0].ChannelID != fixtureChatID {
					t.Fatalf("Точное видео не подтвердило свой чат: %+v", found)
				}
			} else if found.ChannelID != "" || len(found.Broadcasts) != 0 {
				t.Fatalf("Ответ без запрошенного видео выдал кандидата: %+v", found)
			}
		})
	}
}

// TestDiscoverCancellationReturnsTransportSlot проверяет, что отмена запроса освобождает
// единственный сетевой слот для независимой проверки ключа.
func TestDiscoverCancellationReturnsTransportSlot(t *testing.T) {
	// limits ограничивает HTTP-адаптер одним текущим запросом.
	limits := core.DefaultChatLimits()
	limits.HTTPRequests = 1
	// adapter, source и credentials образуют первый отменяемый поиск.
	adapter := newYouTubeFixture(t, limits)
	source, credentials := fixtureYouTubeAccess(fixtureChannelID, fixtureAPIKey)
	// started и stopped подтверждают фактическое начало и прекращение HTTP-запроса.
	started, stopped := make(chan struct{}), make(chan struct{})
	useRESTFixture(t, adapter, func(w http.ResponseWriter, r *http.Request) {
		// Обработчик поиска ждёт закрытия контекста запроса, проверка ключа сразу отвечает.
		if strings.HasSuffix(r.URL.Path, "/search") {
			close(started)
			<-r.Context().Done()
			close(stopped)
			return
		}
		writeFixtureJSON(t, w, 200, `{"items":[]}`)
	})
	// deadline и finish ограничивают всю проверку остановки.
	deadline, finish := context.WithTimeout(t.Context(), 3*time.Second)
	defer finish()
	// search и cancel позволяют оборвать только зависший запрос первого источника.
	search, cancel := context.WithCancel(deadline)
	defer cancel()
	// result принимает результат отменённого HTTP-обмена.
	result := make(chan error, 1)
	go func() {
		// Горутина выполняет один запрос поиска и сообщает, когда он вернулся.
		// err содержит наблюдаемую причину прерывания поиска.
		_, err := adapter.Discover(search, source, credentials)
		result <- err
	}()
	awaitYouTubeFixture(t, deadline, started)
	cancel()
	if !errors.Is(awaitYouTubeFixture(t, deadline, result), context.Canceled) {
		t.Fatal("Поиск не вернул ошибку отмены")
	}
	awaitYouTubeFixture(t, deadline, stopped)
	// reason и err подтверждают, что отдельная операция заняла освобождённый HTTP-слот.
	reason, err := adapter.ValidateAPIKey(deadline, "other-fixture-key")
	if reason != core.ChatReasonNone || err != nil {
		t.Fatalf("Соседняя проверка после отмены не прошла: %q, %v", reason, err)
	}
}

// awaitYouTubeFixture принимает сигнал или значение из ch, завершая тест t по истечении ctx.
func awaitYouTubeFixture[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	// value отдаёт наблюдаемое событие локальной фикстуры.
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		// zero нужен для типизации ветки после Fatal.
		var zero T
		return zero
	}
}

// TestDiscoverNeverReturnsPartialSearch проверяет предел четырёх страниц, повтор токена
// и ошибки продолжения. Первое валидное видео не должно превращать незавершённый поиск в выбор.
func TestDiscoverNeverReturnsPartialSearch(t *testing.T) {
	// cases описывает наблюдаемые границы и известный тип отказа до проверки видео.
	cases := []struct {
		// name различает источник неполности поиска.
		name string
		// pages задаёт фактическое число HTTP-обращений search.list.
		pages int32
		// reason содержит ожидаемый безопасный код отказа.
		reason core.ChatReason
	}{
		{"continuation after four pages", 4, core.ChatReasonLimit},
		{"repeated continuation", 2, core.ChatReasonNetwork},
		{"invalid continuation", 2, core.ChatReasonNetwork},
	}
	// tc задаёт один наблюдаемый отказ посреди прохода.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Подтест использует независимый бюджет для каждого сценария обрыва.
			// adapter, source и credentials запускают обнаружение для одного канала.
			adapter := newYouTubeFixture(t, core.DefaultChatLimits())
			source, credentials := fixtureYouTubeAccess(fixtureChannelID, fixtureAPIKey)
			// pages и videos считают отдельные запросы поиска и конкретных видео.
			var pages, videos atomic.Int32
			useRESTFixture(t, adapter, func(w http.ResponseWriter, r *http.Request) {
				// Обработчик имитирует ранний валидный кандидат и невозможность подтвердить полный список.
				if strings.HasSuffix(r.URL.Path, "/videos") {
					videos.Add(1)
					writeFixtureJSON(t, w, 200, `{"items":[]}`)
					return
				}
				// page задаёт номер запроса, значение next — токен для недоступной либо очередной страницы.
				page := pages.Add(1)
				next := fmt.Sprintf("continuation-%d", page)
				if tc.name == "repeated continuation" {
					next = "repeated"
				}
				if tc.name == "invalid continuation" && page > 1 {
					writeFixtureJSON(t, w, 400, `{"error":{"errors":[{"reason":"invalidPageToken"}]}}`)
					return
				}
				writeFixtureJSON(t, w, 200, fixtureSearchPage(t, next, fixtureVideoID))
			})
			// found и err позволяют отличить корректный безопасный отказ от частичного автоподбора.
			found, err := adapter.Discover(t.Context(), source, credentials)
			if err != nil || found.State != core.ChatRetrying || found.Reason != tc.reason ||
				found.RetryAfter <= 0 || len(found.Broadcasts) != 0 || found.ChannelID != "" ||
				pages.Load() != tc.pages || videos.Load() != 0 {
				t.Fatalf("Неполный проход выдал кандидата или неверную причину: %+v, %v, запросов=%d/%d",
					found, err, pages.Load(), videos.Load())
			}
		})
	}
}

// TestDiscoverBroadcastChoiceCountBoundary проверяет полный набор из 64 видео и отказ
// при 65-м кандидате. Сокращение до 64 дало бы неполный пользовательский выбор.
func TestDiscoverBroadcastChoiceCountBoundary(t *testing.T) {
	// count задаёт соседние значения вокруг точной границы Manager.
	for _, count := range []int{64, 65} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			// Подтест выдаёт ровно count разных подтверждённых публичных эфиров.
			// adapter, source и credentials представляют независимый полный проход.
			adapter := newYouTubeFixture(t, core.DefaultChatLimits())
			source, credentials := fixtureYouTubeAccess(fixtureChannelID, fixtureAPIKey)
			// ids хранит все видео в порядке поиска по двум страницам.
			ids := make([]string, count)
			// i задаёт уникальный порядковый идентификатор прямого эфира.
			for i := range ids {
				ids[i] = fmt.Sprintf("video-%05d", i)
			}
			useRESTFixture(t, adapter, func(w http.ResponseWriter, r *http.Request) {
				// Обработчик возвращает 50 ссылок первой страницы, остаток второй и метаданные по ID.
				// query хранит id или pageToken фактически запрошенной операции.
				query := r.URL.Query()
				if strings.HasSuffix(r.URL.Path, "/search") {
					if query.Get("pageToken") == "" {
						writeFixtureJSON(t, w, 200, fixtureSearchPage(t, "second-page", ids[:50]...))
						return
					}
					writeFixtureJSON(t, w, 200, fixtureSearchPage(t, "", ids[50:]...))
					return
				}
				// requested разбирает группу ID, response вмещает её публичные метаданные.
				requested := strings.Split(query.Get("id"), ",")
				response := make([]any, 0, len(requested))
				// id содержит очередной запрошенный эфир.
				for _, id := range requested {
					response = append(response, fixtureVideo(id, fixtureChannelID, "chat-"+id, "public", "live", ""))
				}
				writeFixtureJSON(t, w, 200, fixtureJSON(t, map[string]any{"items": response}))
			})
			// found возвращает количество вариантов и признак допустимого выбора, err сообщает об отмене.
			found, err := adapter.Discover(t.Context(), source, credentials)
			if err != nil || found.ChannelID != "" {
				t.Fatalf("Неоднозначный поиск вернул готовый чат либо ошибку: %q, %v", found.ChannelID, err)
			}
			if count == 64 {
				if found.State != core.ChatWaitingChat || found.Reason != core.ChatReasonSelectionRequired ||
					len(found.Broadcasts) != count || found.Broadcasts[count-1].VideoID != ids[count-1] {
					t.Fatalf("Полный пограничный набор потерян: %s/%s, найдено=%d", found.State, found.Reason, len(found.Broadcasts))
				}
			} else if found.State != core.ChatRetrying || found.Reason != core.ChatReasonLimit || len(found.Broadcasts) != 0 {
				t.Fatalf("Превышенный набор выдан частично: %s/%s, найдено=%d", found.State, found.Reason, len(found.Broadcasts))
			}
		})
	}
}
