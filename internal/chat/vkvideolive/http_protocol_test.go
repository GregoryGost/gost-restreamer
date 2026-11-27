package vkvideolive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

const (
	// fixtureClientID обозначает вымышленное приложение локального DevAPI.
	fixtureClientID = "fixture-application"
	// fixtureClientSecret отличает секрет приложения от токенов во всех запросах.
	fixtureClientSecret = "fixture-basic-secret"
	// fixtureAppToken выдаётся локальным OAuth-сервером и принимается на DevAPI.
	fixtureAppToken = "fixture-app-bearer"
	// fixtureRenewedToken отличает обновлённый Bearer от отвергнутого.
	fixtureRenewedToken = "fixture-renewed-bearer"
	// fixtureSocketToken используется только в команде WebSocket connect.
	fixtureSocketToken = "fixture-short-lived-ws"
	// fixtureChannelID превышает границу точных целых чисел float64 на единицу.
	fixtureChannelID = "9007199254740993"
	// fixtureChannel отличает ожидаемый канал публикаций от постороннего.
	fixtureChannel = "channel-chat:" + fixtureChannelID
	// fixtureTimeout ограничивает ожидание выполнения локального сетевого сценария.
	fixtureTimeout = 5 * time.Second
)

// fixtureClock возвращает фиксированные часы проверки OAuth и HTTP Retry-After.
func fixtureClock() time.Time {
	return time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
}

// newProtocolFixture создаёт адаптер с limits и направляет все его запросы к
// HTTPS-серверу с handler. Сервер и локальный транспорт закрываются через t.
func newProtocolFixture(t *testing.T, limits core.ChatLimits, handler http.Handler) *Adapter {
	t.Helper()

	// server поднимает HTTP и WebSocket на одном закрытом от внешней сети адресе.
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	// adapter использует обычные правила проверки ответов, err сообщает об отказе настроить фикстуру.
	adapter, err := New(limits)
	if err != nil {
		t.Fatalf("Создание адаптера с проверенными пределами завершилось ошибкой: %v", err)
	}
	// client сохраняет правила перенаправления адаптера, меняя только доверенный сертификат.
	client := *adapter.httpClient
	// transport сохраняет ограничения исходного транспорта и принимает сертификат фикстуры.
	transport := client.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	t.Cleanup(transport.CloseIdleConnections)
	client.Transport = transport
	adapter.httpClient = &client
	adapter.oauthBase = server.URL
	adapter.apiBase = server.URL
	adapter.socketURL = "wss" + strings.TrimPrefix(server.URL, "https") + "/connection/websocket?cf_protocol_version=v2"
	adapter.now = fixtureClock
	return adapter
}

// fixtureCredentials выдаёт сохранённый набор с token для заданной физической попытки.
func fixtureCredentials(token string) core.ChatCredentials {
	return core.ChatCredentials{
		AccountID: "vk-account", Provider: core.ChatVKVideoLive, CredentialRevision: 1,
		CredentialWriteID: "vk-credential-write", CredentialUpdatedAt: fixtureClock(),
		Bundle: core.ChatCredentialBundle{VKVideoLive: &core.VKVideoLiveChatCredentials{
			ClientID: fixtureClientID, ClientSecret: fixtureClientSecret, AccessToken: token,
			ExpiresAt: fixtureClock().Add(time.Hour),
		}},
	}
}

// fixtureHTTPReply отправляет status и body через w без преобразований, чтобы
// проверять в том числе ответы с несколькими JSON или неполным телом.
func fixtureHTTPReply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// fixtureHoldHTTPBody отправляет через w заголовки ответа со status и Retry-After.
// Тело остаётся незавершённым, пока клиент не закроет запрос r.
func fixtureHoldHTTPBody(w http.ResponseWriter, r *http.Request, status int, retryAfter string) {
	w.Header().Set("Retry-After", retryAfter)
	w.Header().Set("Content-Length", "1")
	w.WriteHeader(status)
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

// assertFixtureBearer проверяет метод, Bearer token и отсутствие реквизитов в
// URL запроса r. Значения заголовков не включаются в вывод t.
func assertFixtureBearer(t *testing.T, r *http.Request, token string) {
	t.Helper()
	if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+token ||
		strings.Contains(r.URL.RawQuery, token) || strings.Contains(r.URL.String(), fixtureClientSecret) {
		t.Error("DevAPI должен получить GET с Bearer только в заголовке")
	}
}

// assertNoFixtureSecrets проверяет, что value пригодно для передачи в журнал
// или публичное событие и не содержит токены или секрет из фикстуры.
func assertNoFixtureSecrets(t *testing.T, value any) {
	t.Helper()
	// body содержит JSON представление результата, err показывает отказ сериализации.
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal("Не удалось сериализовать результат проверки")
	}
	// diagnostic покрывает строковое описание ошибок и JSON публичных данных.
	diagnostic := fmt.Sprint(value) + string(body)
	// secret содержит одно из закрытых значений фикстуры.
	for _, secret := range []string{fixtureClientSecret, fixtureAppToken, fixtureRenewedToken, fixtureSocketToken} {
		if strings.Contains(diagnostic, secret) {
			t.Fatal("Публичный результат содержит реквизиты фикстуры")
		}
	}
}

// TestVerifyCredentialsChecksBothTokenLifetimes проверяет в t двухэтапную
// проверку пары реквизитов и выбирает более ранний подтверждённый срок.
func TestVerifyCredentialsChecksBothTokenLifetimes(t *testing.T) {
	// testCase задаёт два полученных срока и ожидаемый срок в секундах от часов фикстуры.
	for _, testCase := range []struct {
		name     string // name различает сценарии выбора срока.
		lifetime string // lifetime содержит необязательный expires_in в JSON ответа OAuth.
		want     int    // want задаёт ожидаемый срок относительно часов фикстуры в секундах.
	}{
		{"app_lifetime", `,"expires_in":90`, 90},
		{"verified_expiration", `,"expires_in":300`, 120},
		{"missing_relative_lifetime", "", 120},
	} {
		// Подтест использует отдельный сервер и наблюдает порядок двух запросов.
		t.Run(testCase.name, func(t *testing.T) {
			// calls считает запросы выдачи и проверки между горутиной HTTP и тестом.
			var calls atomic.Int32
			// elapsedSeconds продвигает часы после отправки запроса выдачи.
			var elapsedSeconds atomic.Int64
			// adapter использует локальный обработчик, который проверяет реквизиты по этапам.
			adapter := newProtocolFixture(t, core.DefaultChatLimits(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch calls.Add(1) {
				case 1:
					// id и secret содержат декодированную пару Basic, ok подтверждает саму схему.
					id, secret, ok := r.BasicAuth()
					if r.URL.Path != "/oauth/server/token" || r.Method != http.MethodPost || !ok ||
						id != fixtureClientID || secret != fixtureClientSecret ||
						r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" ||
						r.ParseForm() != nil || r.PostForm.Encode() != "grant_type=client_credentials" {
						t.Error("OAuth должен получить исходную пару реквизитов только в Basic и grant_type в форме")
					}
					fixtureHTTPReply(w, http.StatusOK, `{"access_token":"`+fixtureAppToken+`","token_type":"Bearer"`+testCase.lifetime+`}`)
				case 2:
					elapsedSeconds.Store(25)
					assertFixtureBearer(t, r, fixtureAppToken)
					if r.URL.Path != "/v1/token" || r.URL.RawQuery != "" {
						t.Error("Срок и тип токена должны проверяться через /v1/token")
					}
					fixtureHTTPReply(w, http.StatusOK, fmt.Sprintf(`{"data":{"type":"application","expired_at":%d}}`, fixtureClock().Unix()+120))
				default:
					t.Error("Выдача выполнила лишний сетевой запрос")
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			// Часы различают время выдачи и проверки, чтобы обнаружить неверную точку отсчёта expires_in.
			adapter.now = func() time.Time { return fixtureClock().Add(time.Duration(elapsedSeconds.Load()) * time.Second) }
			// got содержит проверенный полный набор, err свидетельствует об отказе выдачи или проверки.
			got, err := adapter.VerifyCredentials(t.Context(), fixtureClientID, fixtureClientSecret)
			if err != nil || got.ClientID != fixtureClientID || got.ClientSecret != fixtureClientSecret ||
				got.AccessToken != fixtureAppToken || !got.ExpiresAt.Equal(fixtureClock().Add(time.Duration(testCase.want)*time.Second)) ||
				got.ExpiresAt.Location() != time.UTC || calls.Load() != 2 {
				t.Fatalf("Проверка должна подтвердить пару и вернуть минимальный UTC-срок (%d с), получено запросов %d, ошибка %v", testCase.want, calls.Load(), err)
			}
		})
	}
}

// TestVerifyCredentialsRejectsUnverifiedResults проверяет в t, что отказ пары,
// временный HTTP-сбой и недостоверный успех различаются без выдачи набора.
func TestVerifyCredentialsRejectsUnverifiedResults(t *testing.T) {
	// validToken позволяет перейти ко второму этапу без изменения ответа выдачи.
	validToken := `{"access_token":"` + fixtureAppToken + `","token_type":"Bearer","expires_in":300}`
	// testCase определяет ответ выбранного этапа и ожидаемую категорию ошибки.
	for _, testCase := range []struct {
		name   string // name различает причины отказа.
		path   string // path указывает, какой этап возвращает ответ.
		status int    // status содержит HTTP-статус выбранного этапа.
		body   string // body задаёт тело ответа без нормализации.
		want   error  // want содержит ожидаемую ошибку, проверяемую через errors.Is.
	}{
		{"invalid_client", "/oauth/server/token", 401, `{"error":"invalid_client","error_description":"` + fixtureClientSecret + `"}`, core.ErrInvalid},
		{"unconfirmed_app_rejection", "/oauth/server/token", 401, `{}`, core.ErrNotReady},
		{"upstream_error", "/oauth/server/token", 503, `{"error":"invalid_client"}`, core.ErrNotReady},
		{"broken_json", "/oauth/server/token", 200, `{"access_token":`, core.ErrNotReady},
		{"second_value", "/oauth/server/token", 200, validToken + ` {}`, core.ErrNotReady},
		{"relative_lifetime_fraction", "/oauth/server/token", 200, `{"access_token":"token","token_type":"Bearer","expires_in":1.5}`, core.ErrNotReady},
		{"relative_lifetime_overflow", "/oauth/server/token", 200, `{"access_token":"token","token_type":"Bearer","expires_in":9223372036854775807}`, core.ErrNotReady},
		{"missing_bearer_type", "/oauth/server/token", 200, `{"access_token":"token","expires_in":42}`, core.ErrNotReady},
		{"empty_bearer_type", "/oauth/server/token", 200, `{"access_token":"token","token_type":"","expires_in":42}`, core.ErrNotReady},
		{"relative_lifetime_null", "/oauth/server/token", 200, `{"access_token":"token","token_type":"Bearer","expires_in":null}`, core.ErrNotReady},
		{"relative_lifetime_zero", "/oauth/server/token", 200, `{"access_token":"token","token_type":"Bearer","expires_in":0}`, core.ErrNotReady},
		{"user_token", "/v1/token", 200, fmt.Sprintf(`{"data":{"type":"user","expired_at":%d}}`, fixtureClock().Unix()+120), core.ErrNotReady},
		{"expired_token", "/v1/token", 200, fmt.Sprintf(`{"data":{"type":"application","expired_at":%d}}`, fixtureClock().Unix()), core.ErrNotReady},
		{"invalid_absolute_time", "/v1/token", 200, `{"data":{"type":"application","expired_at":9223372036854775807}}`, core.ErrNotReady},
		{"error_envelope_with_data", "/v1/token", 200, fmt.Sprintf(`{"error":"unexpected","data":{"type":"application","expired_at":%d}}`, fixtureClock().Unix()+120), core.ErrNotReady},
	} {
		// Подтест изолирует ответ каждого отказа и проверяет отсутствие частичного набора.
		t.Run(testCase.name, func(t *testing.T) {
			// calls отличает отказ выдачи от отказа проверки токена.
			var calls atomic.Int32
			// adapter получает заведомо правильную выдачу перед ошибкой проверки, если она нужна.
			adapter := newProtocolFixture(t, core.DefaultChatLimits(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == testCase.path {
					fixtureHTTPReply(w, testCase.status, testCase.body)
					return
				}
				if testCase.path == "/v1/token" && r.URL.Path == "/oauth/server/token" {
					fixtureHTTPReply(w, http.StatusOK, validToken)
					return
				}
				t.Error("Получен запрос после отказа текущего этапа")
				w.WriteHeader(http.StatusBadRequest)
			}))
			// got должен остаться пустым, err должен отличать отказ пары от временной ошибки.
			got, err := adapter.VerifyCredentials(t.Context(), fixtureClientID, fixtureClientSecret)
			assertNoFixtureSecrets(t, err)
			// wantCalls равен числу этапов, требуемых для указанного ответа.
			wantCalls := int32(1)
			if testCase.path == "/v1/token" {
				wantCalls = 2
			}
			if !errors.Is(err, testCase.want) || got != (core.VKVideoLiveChatCredentials{}) || calls.Load() != wantCalls {
				t.Fatalf("Ожидалась ошибка %v без набора после %d запросов, получено %v после %d запросов", testCase.want, wantCalls, err, calls.Load())
			}
		})
	}
}

// TestVerifyCredentialsDoesNotFollowRedirect проверяет в t, что OAuth не
// передаёт Basic на новый маршрут и отклоняет ответ перенаправления.
func TestVerifyCredentialsDoesNotFollowRedirect(t *testing.T) {
	// redirects считает попадания на указанный сервером чужой маршрут.
	var redirects atomic.Int32
	// adapter получает перенаправление 307, сохраняющее исходные метод и заголовок при переходе.
	adapter := newProtocolFixture(t, core.DefaultChatLimits(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/server/token" {
			http.Redirect(w, r, "/redirect-target", http.StatusTemporaryRedirect)
			return
		}
		redirects.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	// credentials должен остаться пустым, err содержит причину отказа.
	credentials, err := adapter.VerifyCredentials(t.Context(), fixtureClientID, fixtureClientSecret)
	assertNoFixtureSecrets(t, err)
	if err == nil || credentials != (core.VKVideoLiveChatCredentials{}) || redirects.Load() != 0 {
		t.Fatal("Перенаправление OAuth должно завершиться отказом без дальнейшего запроса")
	}
}

// TestDiscoverClassifiesNumericIDAndFailures проверяет в t точность int64,
// классификацию HTTP и тела DevAPI и безопасный результат обнаружения.
func TestDiscoverClassifiesNumericIDAndFailures(t *testing.T) {
	// testCase задаёт один ответ канала, ожидаемое состояние, причину и задержку.
	for _, testCase := range []struct {
		name       string               // name различает варианты ответа DevAPI.
		code       int                  // code содержит статус HTTP.
		body       string               // body содержит точный текст ответа площадки.
		retryAfter string               // retryAfter задаёт необязательный заголовок площадки.
		id         string               // id содержит точный ожидаемый ID либо пустую строку при отказе.
		state      core.ChatSourceState // state задаёт ожидаемое публичное состояние, если оно установлено контрактом.
		reason     core.ChatReason      // reason задаёт типизированное объяснение состояния.
		delay      time.Duration        // delay задаёт длительность из применимого Retry-After.
		want       error                // want содержит ожидаемую ошибку при отказе транспорта или реквизитов.
	}{
		{name: "precise_int64", code: 200, body: `{"data":{"channel":{"id":` + fixtureChannelID + `}}}`, id: fixtureChannelID},
		{name: "max_int64", code: 200, body: `{"data":{"channel":{"id":9223372036854775807}}}`, id: "9223372036854775807"},
		{name: "overflow", code: 200, body: `{"data":{"channel":{"id":9223372036854775808}}}`, state: core.ChatRetrying, reason: core.ChatReasonNetwork},
		{name: "quoted_id", code: 200, body: `{"data":{"channel":{"id":"42"}}}`, state: core.ChatRetrying, reason: core.ChatReasonNetwork},
		{name: "zero", code: 200, body: `{"data":{"channel":{"id":0}}}`, state: core.ChatRetrying, reason: core.ChatReasonNetwork},
		{name: "unauthorized", code: 401, body: `{"error":"unauthorized","error_description":"` + fixtureAppToken + `"}`, want: core.ErrChatAppTokenRejected},
		{name: "missing_channel", code: 404, body: `{"error":"channel_not_found","error_description":"` + fixtureAppToken + `"}`, retryAfter: "23", state: core.ChatWaitingChat, reason: core.ChatReasonUnavailable, delay: 23 * time.Second},
		{name: "unknown_route", code: 404, body: `{"error":"not_found"}`, state: core.ChatFailed, reason: core.ChatReasonConfiguration},
		{name: "forbidden", code: 403, body: `{"error":"forbidden"}`, state: core.ChatFailed, reason: core.ChatReasonUnavailable},
		{name: "unconfirmed_forbidden", code: 403, body: `{}`, state: core.ChatRetrying, reason: core.ChatReasonNetwork},
		{name: "upstream_failure", code: 502, body: `{"error":"channel_not_found"}`, state: core.ChatRetrying, reason: core.ChatReasonNetwork},
		{name: "quota", code: 429, body: `{}`, retryAfter: "37", state: core.ChatRetrying, reason: core.ChatReasonQuota, delay: 37 * time.Second},
		{name: "past_retry_date", code: 429, retryAfter: fixtureClock().Add(-time.Second).Format(http.TimeFormat), state: core.ChatRetrying, reason: core.ChatReasonQuota},
		{name: "delta_overflow", code: 429, retryAfter: "18446744073709551616", state: core.ChatRetrying, reason: core.ChatReasonQuota, delay: 9_223_372_036 * time.Second},
		{name: "malformed_delta_overflow", code: 429, retryAfter: "18446744073709551616s", state: core.ChatRetrying, reason: core.ChatReasonQuota},
	} {
		// Подтест сверяет запрос и публичный результат на собственной HTTPS-фикстуре.
		t.Run(testCase.name, func(t *testing.T) {
			// adapter адресует обработчик, который подставляет только один заранее заданный ответ.
			adapter := newProtocolFixture(t, core.DefaultChatLimits(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertFixtureBearer(t, r, fixtureAppToken)
				if r.URL.Path != "/v1/channel" || r.URL.Query().Encode() != "channel_url=streamer" {
					t.Error("Ожидался запрос /v1/channel с единственным постоянным slug")
				}
				w.Header().Set("Retry-After", testCase.retryAfter)
				fixtureHTTPReply(w, testCase.code, testCase.body)
			}))
			// source задаёт VK и постоянный slug, который нужно разрешить.
			source := core.ChatSource{Provider: core.ChatVKVideoLive, ChannelHint: "streamer", AccountID: "vk-account"}
			// got содержит публичный результат поиска, err отличает временный отказ и отвергнутый Bearer.
			got, err := adapter.Discover(t.Context(), source, fixtureCredentials(fixtureAppToken))
			assertNoFixtureSecrets(t, got)
			assertNoFixtureSecrets(t, err)
			if !errors.Is(err, testCase.want) || got.ChannelID != testCase.id ||
				(testCase.state != "" && got.State != testCase.state) || got.Reason != testCase.reason || got.RetryAfter != testCase.delay {
				t.Fatalf("Обнаружение вернуло ID %q, состояние %q, причину %q, задержку %s, ошибку %v", got.ChannelID, got.State, got.Reason, got.RetryAfter, err)
			}
		})
	}
}

// TestDiscoverClassifiesFailureWhileHTTPBodyIsPending проверяет отказы,
// которые определяются по заголовкам. Сервер держит тело открытым, пока клиент
// не освободит запрос. Предельный срок внешнего контекста выявляет ожидание тела.
func TestDiscoverClassifiesFailureWhileHTTPBodyIsPending(t *testing.T) {
	// testCase задаёт HTTP-статус, публичную причину и признак отзыва Bearer.
	for _, testCase := range []struct {
		code   int             // code определяет отказ, пока тело ответа ещё открыто.
		reason core.ChatReason // reason задаёт причину планового повтора.
	}{
		{http.StatusUnauthorized, core.ChatReasonNetwork},
		{http.StatusTooManyRequests, core.ChatReasonQuota},
		{http.StatusServiceUnavailable, core.ChatReasonNetwork},
	} {
		// Подтест проверяет один заголовок Retry-After и завершение HTTP-запроса.
		t.Run(http.StatusText(testCase.code), func(t *testing.T) {
			// delay задаёт интервал из абсолютного HTTP Retry-After.
			const delay = 19 * time.Second
			// closed закрывается, когда сервер получает уведомление о завершении запроса.
			closed := make(chan struct{})
			// adapter получает заголовки по TLS, пока обработчик удерживает тело.
			adapter := newProtocolFixture(t, core.DefaultChatLimits(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(closed)
				assertFixtureBearer(t, r, fixtureAppToken)
				fixtureHoldHTTPBody(w, r, testCase.code, fixtureClock().Add(delay).Format(http.TimeFormat))
			}))
			// ctx ограничивает проверку короче срока адаптера, cancel освобождает таймер.
			ctx, cancel := context.WithTimeout(t.Context(), fixtureTimeout)
			defer cancel()
			// source описывает канал, для которого DevAPI получает запрос обнаружения.
			source := core.ChatSource{Provider: core.ChatVKVideoLive, ChannelHint: "streamer", AccountID: "vk-account"}
			// got содержит состояние обнаружения, err позволяет восстановить отвергнутый Bearer.
			got, err := adapter.Discover(ctx, source, fixtureCredentials(fixtureAppToken))
			if testCase.code == http.StatusUnauthorized {
				// retry сохраняет сетевую причину и интервал из ответа 401.
				var retry *core.ChatReadRetry
				if !errors.Is(err, core.ErrChatAppTokenRejected) || !errors.As(err, &retry) ||
					retry.Reason != testCase.reason || retry.RetryAfter != delay || got.ChannelID != "" {
					t.Fatalf("Ответ 401 должен передать отказ Bearer и задержку %s уже по заголовкам, результат %v", delay, err)
				}
			} else if err != nil || got.ChannelID != "" || got.State != core.ChatRetrying ||
				got.Reason != testCase.reason || got.RetryAfter != delay {
				t.Fatalf("По заголовкам ожидались повтор с причиной %q и задержка %s, получено %+v и %v", testCase.reason, delay, got, err)
			}
			waitFixtureSignal(t, closed)
		})
	}
}

// TestVerifyCredentialsPreservesRetryAfter проверяет в t относительную и
// абсолютную задержку при квоте или временной ошибке сервиса выдачи.
func TestVerifyCredentialsPreservesRetryAfter(t *testing.T) {
	// testCase задаёт форму Retry-After и ожидаемую причину повторной выдачи.
	for _, testCase := range []struct {
		status int             // status содержит HTTP-статус ответа сервиса выдачи.
		header string          // header задаёт интервал числом секунд или датой HTTP.
		reason core.ChatReason // reason отличает квоту от временной ошибки сервиса.
	}{
		{http.StatusTooManyRequests, "41", core.ChatReasonQuota},
		{http.StatusTooManyRequests, fixtureClock().Add(41 * time.Second).Format(http.TimeFormat), core.ChatReasonQuota},
		{http.StatusServiceUnavailable, "41", core.ChatReasonNetwork},
	} {
		// adapter получает ответ с задержкой и без нового app-токена.
		adapter := newProtocolFixture(t, core.DefaultChatLimits(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", testCase.header)
			fixtureHTTPReply(w, testCase.status, `{"error":"`+fixtureClientSecret+`"}`)
		}))
		// credentials должен остаться пустым, err содержит безопасный повтор.
		credentials, err := adapter.VerifyCredentials(t.Context(), fixtureClientID, fixtureClientSecret)
		assertNoFixtureSecrets(t, err)
		// retry получает типизированную задержку и причину для Manager.
		var retry *core.ChatReadRetry
		if !errors.As(err, &retry) || retry.RetryAfter != 41*time.Second || retry.Reason != testCase.reason ||
			credentials != (core.VKVideoLiveChatCredentials{}) {
			t.Fatalf("Ответ %d с Retry-After %q должен отложить повтор на 41 с по причине %q, получено %v", testCase.status, testCase.header, testCase.reason, err)
		}
	}
}

// TestDiscoverCancellationReleasesHTTP проверяет в t, что отмена discovery
// закрывает HTTP-запрос, пока сервер ещё удерживает тело ответа.
func TestDiscoverCancellationReleasesHTTP(t *testing.T) {
	// started сигнализирует, что сервер начал ответ с незавершённым JSON.
	started := make(chan struct{})
	// closed закрывается после отмены контекста запроса на сервере.
	closed := make(chan struct{})
	// adapter читает сетевой ответ локального обработчика вплоть до отмены.
	adapter := newProtocolFixture(t, core.DefaultChatLimits(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixtureHTTPReply(w, http.StatusOK, `{"data":`)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(closed)
	}))
	// ctx ограничивает чтение ответа, cancel моделирует остановку discovery вызывающим кодом.
	ctx, cancel := context.WithTimeout(t.Context(), fixtureTimeout)
	defer cancel()
	// done отдаёт фактическую ошибку discovery в горутину теста.
	done := make(chan error, 1)
	// Фоновый вызов обнаруживает канал, пока тест синхронизирует момент отмены.
	go func() {
		// err содержит результат обращения с отменённым контекстом.
		_, err := adapter.Discover(ctx, core.ChatSource{Provider: core.ChatVKVideoLive, ChannelHint: "streamer", AccountID: "vk-account"}, fixtureCredentials(fixtureAppToken))
		done <- err
	}()
	waitFixtureSignal(t, started)
	cancel()
	if !errors.Is(awaitFixture(t, done), context.Canceled) {
		t.Error("Отмена обнаружения должна остаться ошибкой контекста")
	}
	waitFixtureSignal(t, closed)
}

// TestDiscoverBoundsHTTPResponse проверяет точную границу байтов всего HTTP-тела.
// Дополнительный байт допустимого JSON-пробела должен отклонять ответ независимо
// от корректного ID внутри.
func TestDiscoverBoundsHTTPResponse(t *testing.T) {
	// limit задаёт общий предел тела ответа в байтах.
	const limit = 1024
	// extra отличает точное попадание в предел от превышения на один байт.
	for _, extra := range []int{0, 1} {
		// Подтест изменяет лишь размер пробельного заполнения после JSON.
		t.Run(fmt.Sprintf("bytes_%d", limit+extra), func(t *testing.T) {
			// limits задаёт бюджеты адаптера с уменьшенным пределом HTTP-тела.
			limits := core.DefaultChatLimits()
			limits.UpstreamHTTPBytes = limit
			// data содержит один успешный JSON с ID канала.
			data := `{"data":{"channel":{"id":` + fixtureChannelID + `}}}`
			// body сохраняет тот же результат поиска при обоих размерах тела.
			body := data + strings.Repeat(" ", limit+extra-len(data))
			// adapter выполняет обычный запрос discovery и получает выбранный размер.
			adapter := newProtocolFixture(t, limits, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertFixtureBearer(t, r, fixtureAppToken)
				fixtureHTTPReply(w, http.StatusOK, body)
			}))
			// source задаёт постоянный slug для запроса обнаружения канала.
			source := core.ChatSource{Provider: core.ChatVKVideoLive, ChannelHint: "streamer", AccountID: "vk-account"}
			// got и err показывают, принял ли адаптер тело ответа целиком.
			got, err := adapter.Discover(t.Context(), source, fixtureCredentials(fixtureAppToken))
			if extra == 0 {
				if err != nil || got.ChannelID != fixtureChannelID {
					t.Fatalf("На границе HTTP-бюджета ожидался точный ID, получено %q и %v", got.ChannelID, err)
				}
			} else if err != nil || got.ChannelID != "" || got.State != core.ChatRetrying {
				t.Fatalf("Ответ за пределом HTTP-бюджета должен дать временный отказ без ID, получено %q, %q и %v", got.ChannelID, got.State, err)
			}
		})
	}
}
