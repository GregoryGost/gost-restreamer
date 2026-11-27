package youtube

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestYouTubeRejectsUnusableKeysBeforeRequest проверяет, что ключ без допустимого
// представления в HTTP и gRPC отклоняется без обращения к Google. Отмена вызывающего
// контекста сохраняет свою причину и тоже не расходует запрос.
func TestYouTubeRejectsUnusableKeysBeforeRequest(t *testing.T) {
	// adapter направляет возможные запросы на локальный сервер, calls фиксирует их число.
	adapter := newYouTubeFixture(t, core.DefaultChatLimits())
	var calls atomic.Int32
	// Обработчик считает даже запросы с изменённым или удалённым заголовком ключа.
	useRESTFixture(t, adapter, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeFixtureJSON(t, w, http.StatusOK, `{"items":[]}`)
	})
	// keys содержит отдельные классы ключей, которые нельзя передать без изменения или сверх бюджета.
	keys := map[string]string{
		"empty":           "",
		"oversized":       strings.Repeat("x", core.ChatCredentialBytes+1),
		"leading space":   " " + fixtureAPIKey,
		"non ASCII":       fixtureAPIKey + "я",
		"control byte":    fixtureAPIKey + "\x7f",
		"line terminator": fixtureAPIKey + "\n",
	}
	// name различает класс отказа, key содержит соответствующее синтетическое значение.
	for name, key := range keys {
		// Подтест проверяет стабильную причину отказа без вывода содержимого ключа.
		t.Run(name, func(t *testing.T) {
			// reason и err отражают результат проверки входного ключа.
			reason, err := adapter.ValidateAPIKey(t.Context(), key)
			if reason != core.ChatReasonCredentials || err != nil {
				t.Fatalf("Причина проверки ключа %q, ошибка=%t", reason, err != nil)
			}
		})
	}
	// canceled и cancel создают отмену до передачи действительного ключа адаптеру.
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	// reason и err отражают отказ по запросу вызывающего, а не состояние Google.
	reason, err := adapter.ValidateAPIKey(canceled, fixtureAPIKey)
	if reason != core.ChatReasonNone || !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatalf("Отмена проверки дала причину %q, отмена=%t, запросов=%d", reason, errors.Is(err, context.Canceled), calls.Load())
	}
	adapter.limits.CredentialBytes = uint64(len(fixtureAPIKey))
	reason, err = adapter.ValidateAPIKey(t.Context(), fixtureAPIKey)
	if reason != core.ChatReasonCredentials || err != nil || calls.Load() != 0 {
		t.Fatalf("Полный набор не помещается в бюджет ключа: причина %q, ошибка=%t, запросов=%d", reason, err != nil, calls.Load())
	}
}

// TestYouTubeRejectsMismatchedDiscoveryInput проверяет принадлежность полномочий
// источнику и допустимость одного Video ID до передачи запроса videos.list.
func TestYouTubeRejectsMismatchedDiscoveryInput(t *testing.T) {
	// adapter обслуживает проверку одного видео, calls фиксирует неожиданные обращения.
	adapter := newYouTubeFixture(t, core.DefaultChatLimits())
	var calls atomic.Int32
	// Обработчик считает любой запрос и отдаёт корректный пустой список.
	useRESTFixture(t, adapter, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeFixtureJSON(t, w, http.StatusOK, `{"items":[]}`)
	})
	// source привязан к каналу YouTube, credentials содержит исходные полномочия этого аккаунта.
	source, credentials := fixtureYouTubeAccess(fixtureChannelID, fixtureAPIKey)
	// anotherAccount и anotherProvider моделируют ошибочное связывание сохранённых записей.
	anotherAccount, anotherProvider := credentials, credentials
	anotherAccount.AccountID = "another-account"
	anotherProvider.Provider = core.ChatTwitch
	// unusableKey из отдельной фикстуры содержит не-ASCII символ, допустимый в JSON.
	_, unusableKey := fixtureYouTubeAccess(fixtureChannelID, fixtureAPIKey+"я")
	// cases задаёт одиночное отличие от валидной привязки и ожидаемую безопасную классификацию.
	cases := []struct {
		// name различает нарушенные ограничения входа.
		name string
		// access задаёт полный набор полномочий, передаваемый с неизменным источником.
		access core.ChatCredentials
		// video передаётся как идентификатор единственного выбранного видео.
		video string
		// state и reason определяют наблюдаемый отказ для Manager.
		state  core.ChatSourceState
		reason core.ChatReason
	}{
		{"account mismatch", anotherAccount, fixtureVideoID, core.ChatFailed, core.ChatReasonConfiguration},
		{"provider mismatch", anotherProvider, fixtureVideoID, core.ChatFailed, core.ChatReasonConfiguration},
		{"unusable key", unusableKey, fixtureVideoID, core.ChatAuthRequired, core.ChatReasonCredentials},
		{"multiple IDs", credentials, fixtureVideoID + ",another-video", core.ChatFailed, core.ChatReasonConfiguration},
	}
	// tc представляет один отказ до сети.
	for _, tc := range cases {
		// Подтест проверяет причину отказа и отсутствие данных для подключения к чату.
		t.Run(tc.name, func(t *testing.T) {
			// found содержит результат перепроверки, err передаёт ошибку вызывающему.
			found, err := adapter.DiscoverBroadcast(t.Context(), source, tc.access, tc.video)
			if err != nil || found.State != tc.state || found.Reason != tc.reason || len(found.Broadcasts) != 0 || found.ChannelID != "" {
				t.Fatalf("Неверный отказ перепроверки: %s/%s, кандидатов=%d, ошибка=%t", found.State, found.Reason, len(found.Broadcasts), err != nil)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("Входные отказы привели к %d запросам", calls.Load())
	}
}

// TestYouTubeBoundsVideoResponse проверяет границу тела HTTPS и отклоняет
// повреждённый ответ о видео с временной причиной. Даже корректный JSON
// превышает контракт, если после объекта идёт лишний байт пробела.
func TestYouTubeBoundsVideoResponse(t *testing.T) {
	// limits задаёт малый байтовый бюджет ответа, достаточный для одного публичного видео.
	limits := core.DefaultChatLimits()
	limits.UpstreamHTTPBytes = 512
	// valid содержит полный JSON, пригодный для подтверждения конкретного видео.
	valid := fixtureJSON(t, map[string]any{"items": []any{
		fixtureVideo(fixtureVideoID, fixtureChannelID, fixtureChatID, "public", "live", ""),
	}})
	// atLimit доводит тело до точной границы допустимым JSON-пробелом.
	atLimit := valid + strings.Repeat(" ", int(limits.UpstreamHTTPBytes)-len(valid))
	// cases изменяет только тело ответа при одинаковом HTTP-статусе и запросе.
	cases := []struct {
		// name различает пограничные и повреждённые ответы.
		name string
		// body передаётся как исходная последовательность байтов HTTPS.
		body string
		// accepted отмечает ответ, достаточный для подтверждения выбранного публичного эфира.
		accepted bool
	}{
		{"exact byte limit", atLimit, true},
		{"one byte over limit", atLimit + " ", false},
		{"malformed JSON", valid[:len(valid)-1], false},
		{"invalid UTF-8", strings.TrimSuffix(valid, "}") + `,"ignored":"` + "\xff" + `"}`, false},
	}
	// tc содержит исходное тело для одного изолированного запроса.
	for _, tc := range cases {
		// Подтест использует настоящий HTTPS с ограничением тела, заданным для адаптера.
		t.Run(tc.name, func(t *testing.T) {
			// adapter читает ограниченный ответ, source и credentials задают канал и полномочия.
			adapter := newYouTubeFixture(t, limits)
			source, credentials := fixtureYouTubeAccess(fixtureChannelID, fixtureAPIKey)
			// Обработчик подтверждает, что адаптер получил именно ответ на запрос выбранного видео.
			useRESTFixture(t, adapter, func(w http.ResponseWriter, r *http.Request) {
				checkFixtureGET(t, r, "videos", fixtureAPIKey)
				if r.URL.Query().Get("id") != fixtureVideoID {
					t.Error("Запрос потерял ID выбранного видео")
				}
				writeFixtureJSON(t, w, http.StatusOK, tc.body)
			})
			// found и err показывают, подтвердил ли ответ точное видео и его чат.
			found, err := adapter.DiscoverBroadcast(t.Context(), source, credentials, fixtureVideoID)
			if tc.accepted {
				if err != nil || found.Reason != core.ChatReasonNone || len(found.Broadcasts) != 1 ||
					found.Broadcasts[0].VideoID != fixtureVideoID || found.Broadcasts[0].ChannelID != fixtureChatID {
					t.Fatalf("Ответ на границе потерял подтверждённый эфир: %+v, ошибка=%t", found, err != nil)
				}
			} else if err != nil || found.State != core.ChatRetrying || found.Reason != core.ChatReasonNetwork || len(found.Broadcasts) != 0 || found.ChannelID != "" {
				t.Fatalf("Повреждённый ответ дал неверный результат: %s/%s, кандидатов=%d, ошибка=%t", found.State, found.Reason, len(found.Broadcasts), err != nil)
			}
		})
	}
}

// TestYouTubeMalformedLiveEvidencePreventsPartialDiscovery проверяет полный проход
// с двумя видео. При неясных метаданных одного из них адаптер возвращает временный
// отказ и не предлагает другое видео как единственный подтверждённый вариант.
func TestYouTubeMalformedLiveEvidencePreventsPartialDiscovery(t *testing.T) {
	// questionableVideo обозначает второе видео с непригодным подтверждением эфира.
	const questionableVideo = "video-00002"
	// cases задаёт отдельные пробелы и ошибки в метаданных успешного HTTP-ответа.
	cases := []struct {
		// name обозначает повреждённое поле или отсутствующее подтверждение.
		name string
		// channel, privacy, start, end и chat задают исходные поля videos.list для второго видео.
		channel string
		privacy string
		start   string
		end     string
		chat    string
	}{
		{"missing channel", "", "public", fixtureStarted, "", fixtureChatID},
		{"missing privacy", fixtureChannelID, "", fixtureStarted, "", fixtureChatID},
		{"malformed start", fixtureChannelID, "public", "not a timestamp", "", fixtureChatID},
		{"malformed end", fixtureChannelID, "public", fixtureStarted, "not a timestamp", fixtureChatID},
		{"invalid chat ID", fixtureChannelID, "public", fixtureStarted, "", fixtureChatID + "\x00"},
	}
	// tc описывает ответ API в текущем полном проходе.
	for _, tc := range cases {
		// Подтест проверяет, что готовое к чтению первое видео не становится частичным результатом.
		t.Run(tc.name, func(t *testing.T) {
			// adapter выполняет поиск, source и credentials задают проверяемую привязку.
			adapter := newYouTubeFixture(t, core.DefaultChatLimits())
			source, credentials := fixtureYouTubeAccess(fixtureChannelID, fixtureAPIKey)
			// Часы держат обе части поиска внутри одних суток квоты.
			adapter.now = func() time.Time { return time.Date(2026, 11, 12, 20, 0, 0, 0, time.UTC) }
			// videoCalls подтверждает, что проверка действительно дошла до ответа о двух видео.
			var videoCalls atomic.Int32
			// Обработчик возвращает обе поисковые ссылки и две записи videos.list в одном ответе.
			useRESTFixture(t, adapter, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/search") {
					writeFixtureJSON(t, w, http.StatusOK, fixtureSearchPage(t, "", fixtureVideoID, questionableVideo))
					return
				}
				checkFixtureGET(t, r, "videos", fixtureAPIKey)
				// requestedIDs содержит обе платформенные ссылки в порядке, выбранном адаптером.
				requestedIDs := r.URL.Query().Get("id")
				if requestedIDs != fixtureVideoID+","+questionableVideo && requestedIDs != questionableVideo+","+fixtureVideoID {
					t.Error("Перепроверка не запросила оба результата поиска")
				}
				videoCalls.Add(1)
				writeFixtureJSON(t, w, http.StatusOK, fixtureJSON(t, map[string]any{"items": []any{
					fixtureVideo(fixtureVideoID, fixtureChannelID, fixtureChatID, "public", "live", ""),
					map[string]any{
						"id": questionableVideo, "snippet": map[string]string{"channelId": tc.channel},
						"status": map[string]string{"privacyStatus": tc.privacy},
						"liveStreamingDetails": map[string]string{
							"actualStartTime": tc.start, "actualEndTime": tc.end, "activeLiveChatId": tc.chat,
						},
					},
				}}))
			})
			// found и err показывают, что весь проход требуется повторить до выбора чата.
			found, err := adapter.Discover(t.Context(), source, credentials)
			if videoCalls.Load() != 1 {
				t.Fatal("Обнаружение не выполнило ровно один запрос о двух видео")
			}
			if err != nil || found.State != core.ChatRetrying || found.Reason != core.ChatReasonNetwork ||
				found.RetryAfter <= 0 || len(found.Broadcasts) != 0 || found.ChannelID != "" {
				t.Fatalf("Неполное подтверждение дало неверный результат: %s/%s, кандидатов=%d, ошибка=%t", found.State, found.Reason, len(found.Broadcasts), err != nil)
			}
		})
	}
}

// TestYouTubeTrustsOnlyRecognizedErrorInfo проверяет, что безопасная причина
// зависит от подтверждённой схемы, домена и статуса Google. Суточная причина
// сохраняет срок до полуночи даже при наличии более короткого Retry-After.
func TestYouTubeTrustsOnlyRecognizedErrorInfo(t *testing.T) {
	// cases сопоставляет HTTP-ответы с ожидаемыми причинами и нижней границей повтора.
	cases := []struct {
		// name обозначает источник классификации ошибки.
		name string
		// status и body передают исходный отказ HTTPS.
		status int
		body   string
		// reason и retry задают безопасный код и итоговую задержку обнаружения.
		reason core.ChatReason
		retry  time.Duration
	}{
		{"confirmed expired key", http.StatusBadRequest, `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"youtube.googleapis.com","reason":"API_KEY_EXPIRED"}]}}`, core.ChatReasonCredentials, 7 * time.Second},
		{"unknown domain", http.StatusBadRequest, `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"example.invalid","reason":"API_KEY_EXPIRED"}]}}`, core.ChatReasonConfiguration, 7 * time.Second},
		{"unknown type", http.StatusBadRequest, `{"error":{"details":[{"@type":"type.googleapis.com/example.Failure","domain":"googleapis.com","reason":"API_KEY_EXPIRED"}]}}`, core.ChatReasonConfiguration, 7 * time.Second},
		{"mismatched HTTP status", http.StatusServiceUnavailable, `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"googleapis.com","reason":"API_KEY_EXPIRED"}]}}`, core.ChatReasonNetwork, 7 * time.Second},
		{"daily quota takes precedence", http.StatusForbidden, `{"error":{"errors":[{"reason":"userRateLimitExceeded"}],"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"googleapis.com","reason":"DAILY_LIMIT_EXCEEDED"}]}}`, core.ChatReasonQuota, 47 * time.Minute},
		{"malformed error body", http.StatusServiceUnavailable, `{"error":{"details":`, core.ChatReasonNetwork, 7 * time.Second},
	}
	// tc задаёт реальный статус HTTPS и контролируемое тело ошибки в текущем подтесте.
	for _, tc := range cases {
		// Подтест разделяет суточный бюджет и транспорт с остальными проверками.
		t.Run(tc.name, func(t *testing.T) {
			// adapter обрабатывает классификацию, source и credentials задают выбранный канал и аккаунт.
			adapter := newYouTubeFixture(t, core.DefaultChatLimits())
			source, credentials := fixtureYouTubeAccess(fixtureChannelID, fixtureAPIKey)
			// Часы возвращают 23:13 по времени Лос-Анджелеса в ноябре.
			adapter.now = func() time.Time { return time.Date(2026, 11, 12, 7, 13, 0, 0, time.UTC) }
			// Обработчик отправляет Retry-After во всех случаях независимо от формы ошибки.
			useRESTFixture(t, adapter, func(w http.ResponseWriter, r *http.Request) {
				checkFixtureGET(t, r, "videos", fixtureAPIKey)
				w.Header().Set("Retry-After", "7")
				writeFixtureJSON(t, w, tc.status, tc.body)
			})
			// found и err дают публичную классификацию и требуемый интервал повтора.
			found, err := adapter.DiscoverBroadcast(t.Context(), source, credentials, fixtureVideoID)
			if err != nil || found.Reason != tc.reason || found.RetryAfter != tc.retry || len(found.Broadcasts) != 0 || found.ChannelID != "" {
				t.Fatalf("Ответ дал причину %q и задержку %s, ожидались %q и %s, ошибка=%t", found.Reason, found.RetryAfter, tc.reason, tc.retry, err != nil)
			}
		})
	}
}
