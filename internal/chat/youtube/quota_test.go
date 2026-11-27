package youtube

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestDiscoverSharesSearchPagesAcrossKeysAndPacificDays проверяет, что три страницы одного
// прохода расходуют три единицы общего бюджета, который делят два источника с разными ключами.
func TestDiscoverSharesSearchPagesAcrossKeysAndPacificDays(t *testing.T) {
	// adapter представляет один Google Cloud Project с двумя независимыми источниками.
	adapter := newYouTubeFixture(t, core.DefaultChatLimits())
	// now задаёт полночь обычного 24-часового дня после перехода на зимнее время.
	now := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	adapter.now = func() time.Time {
		// Часы используются только в вызывающей горутине адаптера.
		return now
	}
	// first и firstCredentials адресуют первый публичный канал проекта.
	first, firstCredentials := fixtureYouTubeAccess(fixtureChannelID, fixtureAPIKey)
	// otherKey адресует другой ключ того же Google Cloud Project.
	const otherKey = "other-project-fixture-key"
	// second и secondCredentials адресуют независимый YouTube Channel ID с другим аккаунтом.
	second, secondCredentials := fixtureYouTubeAccess("UCanotherfixturechannel", otherKey)
	second.ID, second.AccountID, secondCredentials.AccountID = "source-two", "account-two", "account-two"
	// pages считает реальные обращения обоих аккаунтов к Google.
	var pages atomic.Int32
	useRESTFixture(t, adapter, func(w http.ResponseWriter, r *http.Request) {
		// Обработчик даёт три страницы первому проходу и по одной всем последующим.
		// page задаёт порядковый номер HTTP-запроса, key сверяет реквизиты своего канала.
		page := pages.Add(1)
		key := fixtureAPIKey
		if r.URL.Query().Get("channelId") == second.ChannelHint {
			key = otherKey
		}
		checkFixtureGET(t, r, "search", key)
		// next выдаёт уникальное продолжение только внутри первого поиска.
		var next string
		if page <= 2 {
			next = fmt.Sprintf("next-%d", page)
		}
		writeFixtureJSON(t, w, 200, fixtureSearchPage(t, next))
	})
	// found и err подтверждают первый полный проход без результата.
	found, err := adapter.Discover(t.Context(), first, firstCredentials)
	if err != nil || found.State != core.ChatDiscovering || found.Reason != core.ChatReasonNoBroadcast || pages.Load() != 3 {
		t.Fatalf("Первый источник не прошёл три пустые страницы: %+v, %v, запросов=%d", found, err, pages.Load())
	}
	found, err = adapter.Discover(t.Context(), second, secondCredentials)
	if err != nil || found.State != core.ChatRetrying || found.RetryAfter <= 0 || len(found.Broadcasts) != 0 || pages.Load() != 3 {
		t.Fatalf("Второй ключ обошёл общий темп поиска: %+v, %v, запросов=%d", found, err, pages.Load())
	}

	// now продвигается по фактически возвращённому сроку. Проверка ниже ограничивает его концом суток.
	now = now.Add(found.RetryAfter)
	// page адресует ожидаемый номер запроса одного из двух независимых источников.
	for page := int32(4); page <= 100; page++ {
		if page%2 == 0 {
			found, err = adapter.Discover(t.Context(), second, secondCredentials)
		} else {
			found, err = adapter.Discover(t.Context(), first, firstCredentials)
		}
		if err != nil || found.Reason != core.ChatReasonNoBroadcast || pages.Load() != page || found.RetryAfter <= 0 {
			t.Fatalf("В пределах суточного бюджета отказал запрос %d: %+v, %v, отправлено=%d", page, found, err, pages.Load())
		}
		if page != 100 {
			now = now.Add(found.RetryAfter)
		}
	}
	// midnight задаёт следующую фактическую полночь PT, независимо от реализации часов адаптера.
	midnight := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	if !now.Before(midnight) {
		t.Fatal("Первые 100 запросов не поместились в ожидаемые тихоокеанские сутки")
	}
	now = midnight.Add(-time.Nanosecond)
	found, err = adapter.Discover(t.Context(), second, secondCredentials)
	if err != nil || found.State != core.ChatRetrying || found.Reason != core.ChatReasonQuota ||
		found.RetryAfter != time.Nanosecond || pages.Load() != 100 {
		t.Fatalf("До полуночи допущена 101-я страница: %+v, %v, запросов=%d", found, err, pages.Load())
	}
	now = midnight
	found, err = adapter.Discover(t.Context(), second, secondCredentials)
	if err != nil || found.Reason != core.ChatReasonNoBroadcast || pages.Load() != 101 {
		t.Fatalf("Общий бюджет не возобновился в полночь PT: %+v, %v, запросов=%d", found, err, pages.Load())
	}
}

// TestDiscoverPacificMidnightObservesDST сверяет фактическую продолжительность общего отклонения
// Google на днях смены смещения. Квитанция дневной квоты не закрывает соседнюю операцию проверки ключа.
func TestDiscoverPacificMidnightObservesDST(t *testing.T) {
	// days задаёт независимо переведённые в UTC границы тихоокеанских суток в 2026 году.
	days := []struct {
		// name поясняет, почему эти сутки отличаются от обычных.
		name string
		// start и midnight задают последовательные местные полуночи, пересчитанные в UTC.
		start, midnight time.Time
	}{
		{"spring 23h", time.Date(2026, 3, 8, 8, 0, 0, 0, time.UTC), time.Date(2026, 3, 9, 7, 0, 0, 0, time.UTC)},
		{"autumn 25h", time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)},
	}
	// day содержит очередную календарную границу дневной квоты.
	for _, day := range days {
		t.Run(day.name, func(t *testing.T) {
			// Подтест проверяет обе полночные границы на своём локальном сервере.
			// limits позволяет обнаружить ошибку, при которой ожидание квоты занимает единственный HTTP-слот.
			limits := core.DefaultChatLimits()
			limits.HTTPRequests = 1
			// adapter представляет отдельный проект с предварительно исчерпанной на стороне Google квотой поиска.
			adapter := newYouTubeFixture(t, limits)
			// now управляет временем без ожидания целого тихоокеанского дня.
			now := day.start
			adapter.now = func() time.Time {
				// Часы синхронны с вызовами теста.
				return now
			}
			// calls считает подтверждённые обращения к Google search.list.
			var calls atomic.Int32
			useRESTFixture(t, adapter, func(w http.ResponseWriter, r *http.Request) {
				// Первый поиск получает отказ дневной квоты. Проверка ключа проходит независимо.
				if strings.HasSuffix(r.URL.Path, "/search") && calls.Add(1) == 1 {
					writeFixtureJSON(t, w, 403, `{"error":{"errors":[{"reason":"dailyLimitExceeded"}]}}`)
					return
				}
				writeFixtureJSON(t, w, 200, `{"items":[]}`)
			})
			// source и credentials принадлежат первому источнику с исчерпанной квотой поиска.
			source, credentials := fixtureYouTubeAccess(fixtureChannelID, fixtureAPIKey)
			// found и err содержат безопасный отказ с календарным сроком до сброса.
			found, err := adapter.Discover(t.Context(), source, credentials)
			if err != nil || found.Reason != core.ChatReasonQuota || found.RetryAfter != day.midnight.Sub(day.start) {
				t.Fatalf("Пауза квоты не учитывает переход часового пояса: %+v, %v", found, err)
			}
			// reason подтверждает, что ожидание квоты оставило сетевой слот доступным для другого ключа.
			reason, err := adapter.ValidateAPIKey(t.Context(), "unrelated-fixture-key")
			if err != nil || reason != core.ChatReasonNone || calls.Load() != 1 {
				t.Fatalf("Срок поиска занял слот или бюджет проверки ключа: %q, %v", reason, err)
			}
			now = day.midnight
			found, err = adapter.Discover(t.Context(), source, credentials)
			if err != nil || found.Reason != core.ChatReasonNoBroadcast || calls.Load() != 2 {
				t.Fatalf("Сброс не наступил в пределах тихоокеанских суток: %+v, %v, запросов=%d", found, err, calls.Load())
			}
		})
	}
}

// TestVideoDiscoveryBudgetExcludesKeyProbe проверяет последний доступный запрос из 10 000,
// учёт неуспешного запроса и независимость проверки ключа от бюджета обнаружения.
func TestVideoDiscoveryBudgetExcludesKeyProbe(t *testing.T) {
	// adapter держит один воспроизводимый RAM-бюджет, не связанный с квотой реальных аккаунтов.
	adapter := newYouTubeFixture(t, core.DefaultChatLimits())
	// now задаёт полдень PT в обычный зимний день.
	now := time.Date(2026, 11, 3, 20, 0, 0, 0, time.UTC)
	adapter.now = func() time.Time {
		// Возвращает управляемое сценарием время.
		return now
	}
	// source и credentials адресуют ранее найденный публичный канал.
	source, credentials := fixtureYouTubeAccess(fixtureChannelID, fixtureAPIKey)
	// videos и probes независимо считают обращения к одному HTTP-методу с разными фильтрами.
	var videos, probes atomic.Int32
	useRESTFixture(t, adapter, func(w http.ResponseWriter, r *http.Request) {
		// Обработчик тратит последний запрос videos.list на ошибку сети, а проверки ключа принимает.
		if r.URL.Query().Get("chart") == "mostPopular" {
			probes.Add(1)
			writeFixtureJSON(t, w, 200, `{"items":[]}`)
			return
		}
		if r.URL.Query().Get("id") != fixtureVideoID {
			t.Error("Вместо проверки известного видео запрошено другое видео")
		}
		if videos.Add(1) == 1 {
			writeFixtureJSON(t, w, 503, `{"error":{"message":"temporary outage"}}`)
			return
		}
		writeFixtureJSON(t, w, 200, `{"items":[]}`)
	})
	// До первого запроса подготавливаются уже использованные 9999 позиций этого дня.
	adapter.quota.resetAt = time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	adapter.quota.videoUsed = 9999
	// probeReason и err проверяют, что проверка ключа не тратит оставшееся обращение к видео.
	probeReason, err := adapter.ValidateAPIKey(t.Context(), fixtureAPIKey)
	if err != nil || probeReason != core.ChatReasonNone {
		t.Fatalf("Проверка ключа не прошла перед границей расхода: %q, %v", probeReason, err)
	}
	// found и err показывают, что последний HTTP-запрос прошёл допуск, хотя Google вернул ошибку.
	found, err := adapter.DiscoverBroadcast(t.Context(), source, credentials, fixtureVideoID)
	if err != nil || found.Reason != core.ChatReasonNetwork || videos.Load() != 1 {
		t.Fatalf("Последняя допустимая попытка не дошла до Google: %+v, %v, запросов=%d", found, err, videos.Load())
	}
	found, err = adapter.DiscoverBroadcast(t.Context(), source, credentials, fixtureVideoID)
	if err != nil || found.Reason != core.ChatReasonQuota || found.RetryAfter < 12*time.Hour || videos.Load() != 1 {
		t.Fatalf("Неудачная попытка не учтена в квоте: %+v, %v, запросов=%d", found, err, videos.Load())
	}
	probeReason, err = adapter.ValidateAPIKey(t.Context(), fixtureAPIKey)
	if err != nil || probeReason != core.ChatReasonNone || probes.Load() != 2 || videos.Load() != 1 {
		t.Fatalf("Исчерпанный бюджет обнаружения изменил проверку ключа: %q, %v", probeReason, err)
	}
}

// TestVideoDiscoveryRetryAfterScope проверяет общий предел videos.list для двух ключей
// и отличает его от ограничений, чья область в ответе Google не подтверждена как общая.
func TestVideoDiscoveryRetryAfterScope(t *testing.T) {
	// cases различает подтверждённую причину сервиса, ограничение пользователя и HTTP 429 без уточнения.
	cases := []struct {
		// name обозначает машинный код отказа или отсутствие такого кода.
		name string
		// code и body воспроизводят статус и JSON ответа Google.
		code int
		body string
		// shared определяет, касается ли срок отказа следующего запроса с другим ключом.
		shared bool
	}{
		{"servingLimitExceeded", http.StatusForbidden, `{"error":{"errors":[{"reason":"servingLimitExceeded"}]}}`, true},
		{"userRateLimitExceeded", http.StatusForbidden, `{"error":{"errors":[{"reason":"userRateLimitExceeded"}]}}`, false},
		{"rateLimitExceeded", http.StatusForbidden, `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`, false},
		{"generic 429", http.StatusTooManyRequests, `{"error":{"message":"Too many requests"}}`, false},
	}
	// tc задаёт отказ первого источника в текущем изолированном подтесте.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Подтест сравнивает два источника одного проекта при фиксированном времени.
			// adapter обслуживает оба ключа и выполняет HTTPS-запросы к одной фикстуре.
			adapter := newYouTubeFixture(t, core.DefaultChatLimits())
			// now задаёт время вдали от полуночи PT, когда суточный бюджет мог бы сбросить ограничение.
			now := time.Date(2026, 11, 3, 20, 0, 0, 0, time.UTC)
			adapter.now = func() time.Time {
				// Часы читаются только из последовательных вызовов адаптера.
				return now
			}
			// cooldown и retryHeader задают короткий срок и его независимое представление HTTP-датой.
			const cooldown = 37 * time.Second
			retryHeader := now.Add(cooldown).Format(http.TimeFormat)
			// first и firstCredentials представляют источник, которому провайдер возвращает отказ.
			first, firstCredentials := fixtureYouTubeAccess(fixtureChannelID, fixtureAPIKey)
			// otherVideo, otherChat и otherKey идентифицируют эфир, чат и ключ второго источника.
			const otherVideo, otherChat, otherKey = "video-00002", "other-chat-id", "other-project-fixture-key"
			// second и secondCredentials задают отдельный канал и аккаунт того же проекта.
			second, secondCredentials := fixtureYouTubeAccess("UCanotherfixturechannel", otherKey)
			second.ID, second.AccountID, secondCredentials.AccountID = "source-two", "account-two", "account-two"
			// firstCalls и secondCalls считают дошедшие до сервера запросы об известных видео.
			var firstCalls, secondCalls atomic.Int32
			useRESTFixture(t, adapter, func(w http.ResponseWriter, r *http.Request) {
				// Обработчик отклоняет запрос первого видео и подтверждает активный чат второго.
				switch r.URL.Query().Get("id") {
				case fixtureVideoID:
					firstCalls.Add(1)
					checkFixtureGET(t, r, "videos", fixtureAPIKey)
					w.Header().Set("Retry-After", retryHeader)
					writeFixtureJSON(t, w, tc.code, tc.body)
				case otherVideo:
					secondCalls.Add(1)
					checkFixtureGET(t, r, "videos", otherKey)
					writeFixtureJSON(t, w, http.StatusOK, fixtureJSON(t, map[string]any{"items": []any{
						fixtureVideo(otherVideo, second.ChannelHint, otherChat, "public", "live", ""),
					}}))
				default:
					t.Error("Получен запрос вне двух заданных видео")
					writeFixtureJSON(t, w, http.StatusBadRequest, `{}`)
				}
			})
			// found и err фиксируют квотную ошибку первого видео и последующий допуск второго.
			found, err := adapter.DiscoverBroadcast(t.Context(), first, firstCredentials, fixtureVideoID)
			if err != nil || found.State != core.ChatRetrying || found.Reason != core.ChatReasonQuota ||
				found.RetryAfter != cooldown || firstCalls.Load() != 1 || secondCalls.Load() != 0 {
				t.Fatalf("Неверный ответ первому источнику: %+v, %v, обращений %d/%d", found, err, firstCalls.Load(), secondCalls.Load())
			}
			if tc.shared {
				now = now.Add(cooldown - time.Nanosecond)
				found, err = adapter.DiscoverBroadcast(t.Context(), second, secondCredentials, otherVideo)
				if err != nil || found.State != core.ChatRetrying || found.Reason != core.ChatReasonQuota ||
					found.RetryAfter != time.Nanosecond || len(found.Broadcasts) != 0 || secondCalls.Load() != 0 {
					t.Fatalf("Второй ключ обошёл общий срок: %+v, %v, обращений %d", found, err, secondCalls.Load())
				}
				now = now.Add(time.Nanosecond)
			}
			found, err = adapter.DiscoverBroadcast(t.Context(), second, secondCredentials, otherVideo)
			if err != nil || found.Reason != core.ChatReasonNone || len(found.Broadcasts) != 1 ||
				found.Broadcasts[0].VideoID != otherVideo || found.Broadcasts[0].ChannelID != otherChat ||
				firstCalls.Load() != 1 || secondCalls.Load() != 1 {
				t.Fatalf("Независимое видео не дошло до Google при разрешённом запросе: %+v, %v, обращений %d/%d",
					found, err, firstCalls.Load(), secondCalls.Load())
			}
		})
	}
}
