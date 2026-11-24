package twitch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

const (
	// testClientID идентифицирует синтетическое публичное приложение.
	testClientID = "fixture-public-client"
	// testReaderID отличает авторизованного читателя от владельца канала.
	testReaderID = "731"
	// testBroadcasterID указывает чат, который читает тестовый аккаунт.
	testBroadcasterID = "908"
	// testAccessToken заменяет полномочия Twitch во всех сетевых фикстурах.
	testAccessToken = "fixture-access"
	// testRefreshToken содержит символы, обязательные для корректного кодирования полей формы.
	testRefreshToken = "fixture+refresh/%"
	// testTimestamp фиксирует начало действия полномочий и время протокольных сообщений.
	testTimestamp = "2026-11-24T11:01:02.123456Z"
)

// oauthTestAdapter направляет OAuth-запросы к отдельному серверу с обработчиком handler.
// t закрывает сервер после теста, а limits ограничивает тестируемые ответы и операции.
func oauthTestAdapter(t *testing.T, limits core.ChatLimits, handler http.HandlerFunc) *Adapter {
	t.Helper()
	// server принимает только запросы этого теста на временном порту.
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	// adapter содержит настоящий OAuth-адаптер, а err сообщает об отказе его создания.
	adapter, err := New(limits)
	if err != nil {
		t.Fatal(err)
	}
	adapter.oauthBase = server.URL
	adapter.httpClient.Transport = server.Client().Transport
	// Функция возвращает фиксированное время, чтобы можно было точно проверить срок токена.
	adapter.now = testTime
	return adapter
}

// testTime возвращает фиксированное время начала действия тестовых полномочий.
func testTime() time.Time {
	return time.Date(2026, time.November, 24, 11, 1, 2, 123456000, time.UTC)
}

// writeTwitchResponse отправляет синтетический JSON body со статусом status.
// t фиксирует ошибку записи в w, если клиент завершил запрос раньше ответа.
func writeTwitchResponse(t *testing.T, w http.ResponseWriter, status int, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// err сообщает, если сервер не смог передать ожидаемый ответ.
	if _, err := io.WriteString(w, body); err != nil {
		t.Error(err)
	}
}

// checkOAuthForm проверяет POST r и сравнивает полученные параметры с want целиком.
// Отдельное сравнение формы защищает от отправки client_secret или потери символов токена.
func checkOAuthForm(t *testing.T, r *http.Request, want url.Values) {
	t.Helper()
	if r.Method != http.MethodPost {
		t.Errorf("Метод запроса %s, ожидается POST", r.Method)
	}
	// kind определяет формат тела, а err сообщает о синтаксической ошибке заголовка.
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "application/x-www-form-urlencoded" {
		t.Errorf("Тип тела %q, ошибка %v", r.Header.Get("Content-Type"), err)
	}
	// err сообщает о некорректном кодировании формы запроса.
	if err := r.ParseForm(); err != nil {
		t.Error(err)
	}
	if !reflect.DeepEqual(r.PostForm, want) || r.URL.RawQuery != "" {
		t.Errorf("Получены параметры %v, ожидаются %v в теле POST", r.PostForm, want)
	}
}

// checkOAuthValidation проверяет GET r и предъявление синтетического токена token
// в одном из двух форматов заголовка, которые принимает Twitch.
func checkOAuthValidation(t *testing.T, r *http.Request, token string) {
	t.Helper()
	// auth содержит тестовый заголовок OAuth или Bearer.
	auth := r.Header.Get("Authorization")
	if r.Method != http.MethodGet || auth != "OAuth "+token && auth != "Bearer "+token {
		t.Errorf("Проверка токена получила %s с авторизацией %q", r.Method, auth)
	}
}

// TestDeviceOAuthWire проверяет выдачу кода, ожидающее подтверждение и выдачу пары.
// Читатель и scope определяются отдельным запросом /validate.
func TestDeviceOAuthWire(t *testing.T) {
	// deviceCode требуется только запросу обмена, а verificationURI предназначен пользователю.
	const deviceCode = "device+only/%"
	const verificationURI = "https://www.twitch.tv/activate?public=true&device-code=ABCDEF"
	// polls различает первый незавершённый обмен и последующую выдачу пары.
	var polls atomic.Int32
	// validations считает вызовы /validate, чтобы исключить сохранение непроверенной пары.
	var validations atomic.Int32
	// adapter обращается к обработчику, который сверяет запросы и воспроизводит ход авторизации.
	adapter := oauthTestAdapter(t, core.DefaultChatLimits(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/device":
			checkOAuthForm(t, r, url.Values{"client_id": {testClientID}, "scopes": {"user:read:chat"}})
			writeTwitchResponse(t, w, http.StatusOK, fmt.Sprintf(
				`{"device_code":%q,"user_code":"ABCDEF","verification_uri":%q,"expires_in":1800,"interval":5}`, deviceCode, verificationURI))
		case "/oauth2/token":
			checkOAuthForm(t, r, url.Values{
				"client_id": {testClientID}, "scopes": {"user:read:chat"}, "device_code": {deviceCode},
				"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
			})
			if polls.Add(1) == 1 {
				writeTwitchResponse(t, w, http.StatusBadRequest, `{"status":400,"message":"authorization_pending"}`)
				return
			}
			writeTwitchResponse(t, w, http.StatusOK, fmt.Sprintf(
				`{"access_token":%q,"refresh_token":%q,"scope":["user:read:chat"],"expires_in":3600,"token_type":"bearer"}`, testAccessToken, testRefreshToken))
		case "/oauth2/validate":
			validations.Add(1)
			checkOAuthValidation(t, r, testAccessToken)
			writeTwitchResponse(t, w, http.StatusOK, fmt.Sprintf(
				`{"client_id":%q,"user_id":%q,"login":"reader","scopes":["user:read:chat"],"expires_in":3600}`, testClientID, testReaderID))
		default:
			t.Errorf("Неожиданный путь запроса %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// challenge содержит данные первого шага, а err сообщает об отказе этого запроса.
	challenge, err := adapter.BeginDevice(t.Context(), testClientID)
	if err != nil {
		t.Fatal(err)
	}
	if challenge.DeviceCode != deviceCode || challenge.UserCode != "ABCDEF" ||
		challenge.VerificationURI != verificationURI || challenge.ExpiresIn != 30*time.Minute || challenge.Interval != 5*time.Second {
		t.Fatalf("Данные подтверждения отличаются от ответа Twitch %+v", challenge)
	}
	// pending представляет опрос до подтверждения пользователем. err также используется для результата следующего запроса.
	pending, err := adapter.PollDevice(t.Context(), testClientID, deviceCode)
	if err != nil || pending.State != DevicePending || pending.Bundle != nil || validations.Load() != 0 {
		t.Fatalf("До подтверждения получено %+v, вызовов /validate %d, ошибка %v", pending, validations.Load(), err)
	}
	// complete содержит полный проверенный набор после подтверждения владельцем.
	complete, err := adapter.PollDevice(t.Context(), testClientID, deviceCode)
	if err != nil || complete.State != DeviceComplete || complete.Bundle == nil || complete.Bundle.Twitch == nil || validations.Load() != 1 || polls.Load() != 2 {
		t.Fatalf("После подтверждения получено %+v, обменов %d, проверок %d, ошибка %v", complete, polls.Load(), validations.Load(), err)
	}
	// credentials сверяет получателя, пару, разрешение и точный срок как единый набор.
	credentials := complete.Bundle.Twitch
	if credentials.ClientID != testClientID || credentials.UserID != testReaderID ||
		credentials.AccessToken != testAccessToken || credentials.RefreshToken != testRefreshToken ||
		!slices.Equal(credentials.Scopes, []string{"user:read:chat"}) || !credentials.ExpiresAt.Equal(testTime().Add(time.Hour)) {
		t.Fatalf("Получен неожиданный набор полномочий %+v", credentials)
	}
}

// TestDeviceOAuthPendingErrors проверяет типизацию ответов slow_down, expired_token и access_denied.
// В этих случаях в результат не должна попадать пара токенов.
func TestDeviceOAuthPendingErrors(t *testing.T) {
	// scenario задаёт один ответ несостоявшегося обмена и возможный Retry-After.
	for _, scenario := range []struct {
		// code содержит точное значение ошибки протокола.
		code string
		// state содержит результат опроса в терминах адаптера.
		state DeviceState
		// retryAfter задаёт синтетический предел в секундах, если сервер его прислал.
		retryAfter string
	}{
		{code: "slow_down", state: DeviceSlowDown, retryAfter: "14"},
		{code: "expired_token", state: DeviceExpired},
		{code: "access_denied", state: DeviceDenied},
	} {
		// Функция проверяет один ответ OAuth, не запуская запросы проверки токена.
		t.Run(scenario.code, func(t *testing.T) {
			// adapter обращается к обработчику, который возвращает только выбранный отказ при обмене.
			adapter := oauthTestAdapter(t, core.DefaultChatLimits(), func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/oauth2/token" {
					t.Errorf("Неожиданный запрос %s", r.URL.Path)
				}
				if scenario.retryAfter != "" {
					w.Header().Set("Retry-After", scenario.retryAfter)
				}
				writeTwitchResponse(t, w, http.StatusBadRequest, fmt.Sprintf(`{"error":%q}`, scenario.code))
			})
			// result содержит типизированный отказ, а err отделяет его от сетевой ошибки.
			result, err := adapter.PollDevice(t.Context(), testClientID, "device")
			if err != nil || result.State != scenario.state || result.Bundle != nil {
				t.Fatalf("Получено %+v, ошибка %v, ожидается %v без токена", result, err, scenario.state)
			}
			if scenario.retryAfter != "" && result.RetryAfter < 14*time.Second {
				t.Fatalf("Пауза %s короче Retry-After от Twitch", result.RetryAfter)
			}
		})
	}
}

// TestDeviceOAuthRejectsUnverifiedPair проверяет отказ принимать токен другого
// публичного приложения, токен приложения вместо пользовательского и отсутствие user:read:chat.
func TestDeviceOAuthRejectsUnverifiedPair(t *testing.T) {
	// validation содержит полное тело ответа Twitch для очередного несовпадения.
	for _, validation := range []string{
		`{"client_id":"other-client","user_id":"731","scopes":["user:read:chat"],"expires_in":3600}`,
		`{"client_id":"fixture-public-client","user_id":null,"scopes":["user:read:chat"],"expires_in":3600}`,
		`{"client_id":"fixture-public-client","user_id":"731","scopes":["user:write:chat"],"expires_in":3600}`,
	} {
		// adapter выдаёт корректную пару, но обработчик /validate объявляет неподходящий доступ.
		adapter := oauthTestAdapter(t, core.DefaultChatLimits(), func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/oauth2/token":
				writeTwitchResponse(t, w, http.StatusOK, `{"access_token":"fixture-access","refresh_token":"fixture-refresh","scope":["user:read:chat"],"expires_in":3600,"token_type":"bearer"}`)
			case "/oauth2/validate":
				checkOAuthValidation(t, r, testAccessToken)
				writeTwitchResponse(t, w, http.StatusOK, validation)
			default:
				t.Errorf("Неожиданный запрос %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		})
		// result не может содержать завершённую авторизацию, независимо от способа передачи отказа в err.
		result, err := adapter.PollDevice(t.Context(), testClientID, "device")
		if result.State == DeviceComplete || result.Bundle != nil {
			t.Errorf("Неподходящий доступ из %s принят как %+v, ошибка %v", validation, result, err)
		}
	}
}

// TestDeviceOAuthBoundsAndCancellation проверяет ограничение тела ответа и отмену
// уже выполняемого HTTP-обмена. Ожидания опираются на сигналы тестового сервера.
func TestDeviceOAuthBoundsAndCancellation(t *testing.T) {
	// limits уменьшает разрешённое тело, чтобы проверить лимит без большого ответа.
	limits := core.DefaultChatLimits()
	limits.UpstreamHTTPBytes = 256
	// entered сообщает, что обработчик обмена получил запрос. cancelled подтверждает отмену со стороны клиента.
	entered, cancelled := make(chan struct{}), make(chan struct{})
	// adapter обращается к обработчику, который выдаёт слишком длинный ответ /device и ждёт отмены /token.
	adapter := oauthTestAdapter(t, limits, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/device":
			writeTwitchResponse(t, w, http.StatusOK, `{"device_code":"device","user_code":"ABCDEF","verification_uri":"https://www.twitch.tv/activate","expires_in":1800,"interval":5,"padding":"`+strings.Repeat("a", 256)+`"}`)
		case "/oauth2/token":
			// err сообщает, если сервер не получил форму обмена целиком до отмены.
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Error(err)
			}
			close(entered)
			select {
			case <-r.Context().Done():
				close(cancelled)
			case <-t.Context().Done():
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	// err подтверждает отказ обработать синтаксически корректный ответ сверх лимита.
	if _, err := adapter.BeginDevice(t.Context(), testClientID); err == nil {
		t.Fatal("Принят OAuth-ответ, превышающий предел")
	}
	// ctx ограничивает время сценария, а stop освобождает его таймер.
	ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	// requestCtx позволяет отдельно отменить обмен, а cancel посылает отмену.
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// finished получает результат фонового OAuth-запроса.
	finished := make(chan error, 1)
	// Горутина выполняет запрос, пока основной тест ожидает его начала.
	go func() {
		// err сообщает, сохранилась ли причина отмены на границе адаптера.
		_, err := adapter.PollDevice(requestCtx, testClientID, "device")
		finished <- err
	}()
	awaitTwitchSignal(t, ctx, entered)
	cancel()
	// err содержит результат отменённого OAuth-запроса.
	if err := awaitTwitchSignal(t, ctx, finished); !errors.Is(err, context.Canceled) {
		t.Errorf("Обмен завершился с %v, ожидается context.Canceled", err)
	}
	awaitTwitchSignal(t, ctx, cancelled)
}

// awaitTwitchSignal читает значение T из signal до отмены ctx. t останавливает
// тест при истечении срока вместо бессрочного ожидания ответа фикстуры.
func awaitTwitchSignal[T any](t *testing.T, ctx context.Context, signal <-chan T) T {
	t.Helper()
	select {
	// value содержит результат проверяемого шага или сигнал закрытия.
	case value := <-signal:
		return value
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		// empty нужен для типизации недостижимого возврата после Fatal.
		var empty T
		return empty
	}
}

// TestTwitchRefreshWire проверяет предъявление прежнего токена, одноразовый обмен
// и независимую проверку личности с новой парой. Действующий доступ не требует обмена.
func TestTwitchRefreshWire(t *testing.T) {
	// newAccess и newRefresh различают обе части новой пары от прежней.
	const newAccess = "fixture-next-access"
	const newRefresh = "fixture-next-refresh"
	// scenario связывает решение Twitch с результатом refresh и правилом повторного использования старого токена.
	for _, scenario := range []struct {
		// name обозначает наблюдаемый исход обновления.
		name string
		// oldStatus определяет действительность исходного access token.
		oldStatus int
		// tokenStatus и tokenBody описывают исход единственного обмена на /token.
		tokenStatus int
		tokenBody   string
		// validationUser задаёт пользователя, подтверждённого для новой пары.
		validationUser string
		// wantNew сообщает, принимается ли новый набор.
		wantNew bool
		// mayConsume запрещает повтор старой пары при неопределённом исходе обмена.
		mayConsume bool
		// wantErr описывает безопасный результат по контрактным ошибкам core.
		wantErr error
	}{
		{name: "действующий access token", oldStatus: http.StatusOK},
		{name: "ротация пары", oldStatus: http.StatusUnauthorized, tokenStatus: http.StatusOK,
			tokenBody:      `{"access_token":"fixture-next-access","refresh_token":"fixture-next-refresh","expires_in":7200,"token_type":"bearer"}`,
			validationUser: testReaderID, wantNew: true, mayConsume: true},
		{name: "выпуск чужому пользователю", oldStatus: http.StatusUnauthorized, tokenStatus: http.StatusOK,
			tokenBody:      `{"access_token":"fixture-next-access","refresh_token":"fixture-next-refresh","expires_in":7200,"token_type":"bearer"}`,
			validationUser: "444", mayConsume: true, wantErr: core.ErrInvalid},
		{name: "отказ обмена", oldStatus: http.StatusUnauthorized, tokenStatus: http.StatusBadRequest,
			tokenBody: `{"status":400,"message":"Invalid refresh token"}`, wantErr: core.ErrInvalid},
		{name: "неопределённый исход обмена", oldStatus: http.StatusUnauthorized, tokenStatus: http.StatusServiceUnavailable,
			tokenBody: `{"status":503,"message":"diagnostic-fixture"}`, mayConsume: true, wantErr: core.ErrNotReady},
	} {
		// Подтест запускает отдельный сервер для каждого результата проверки и обмена.
		t.Run(scenario.name, func(t *testing.T) {
			// exchanges учитывает предъявления одноразового refresh token.
			var exchanges atomic.Int32
			// currentChecks и newChecks учитывают проверки исходной и новой пары отдельно.
			var currentChecks, newChecks atomic.Int32
			// adapter выполняет POST только после подтверждённого отказа старого access token.
			adapter := oauthTestAdapter(t, core.DefaultChatLimits(), func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/oauth2/token":
					exchanges.Add(1)
					checkOAuthForm(t, r, url.Values{
						"client_id": {testClientID}, "grant_type": {"refresh_token"}, "refresh_token": {testRefreshToken},
					})
					if scenario.tokenStatus == 0 {
						t.Error("Действующий access token не требует обмена")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					writeTwitchResponse(t, w, scenario.tokenStatus, scenario.tokenBody)
				case "/oauth2/validate":
					// subject определяет получателя, указанного сервером проверки предъявленного токена.
					subject := testReaderID
					if strings.HasSuffix(r.Header.Get("Authorization"), " "+newAccess) {
						newChecks.Add(1)
						checkOAuthValidation(t, r, newAccess)
						subject = scenario.validationUser
					} else {
						currentChecks.Add(1)
						checkOAuthValidation(t, r, testAccessToken)
						if scenario.oldStatus != http.StatusOK {
							writeTwitchResponse(t, w, scenario.oldStatus, `{"status":401,"message":"invalid access token"}`)
							return
						}
					}
					writeTwitchResponse(t, w, http.StatusOK, fmt.Sprintf(
						`{"client_id":%q,"user_id":%q,"scopes":["user:read:chat"],"expires_in":3600}`, testClientID, subject))
				default:
					t.Errorf("Неожиданный путь запроса %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			// current содержит старый набор, который действителен по локальному сроку до ответа Twitch.
			current := core.ChatCredentials{
				AccountID: "fixture-account", Provider: core.ChatTwitch, CredentialRevision: 1,
				CredentialUpdatedAt: testTime(), CredentialWriteID: "old-write",
				Bundle: core.ChatCredentialBundle{Twitch: &core.TwitchChatCredentials{
					ClientID: testClientID, UserID: testReaderID, AccessToken: testAccessToken, RefreshToken: testRefreshToken,
					Scopes: []string{"user:read:chat"}, ExpiresAt: testTime().Add(time.Hour),
				}},
			}
			// bundle содержит новую пару только после проверки её пригодности.
			// consumed показывает риск повторного предъявления старого токена, err сообщает безопасный отказ.
			bundle, consumed, err := adapter.Refresh(t.Context(), current)
			if !errors.Is(err, scenario.wantErr) || consumed != scenario.mayConsume || (bundle != nil) != scenario.wantNew ||
				currentChecks.Load() != 1 || (exchanges.Load() == 1) != (scenario.tokenStatus != 0) ||
				(newChecks.Load() == 1) != (scenario.validationUser != "") {
				t.Fatalf("Результат обновления %+v, использован=%t, ошибка=%v, проверок исходного/нового=%d/%d, обменов=%d",
					bundle, consumed, err, currentChecks.Load(), newChecks.Load(), exchanges.Load())
			}
			if bundle != nil && (bundle.Twitch == nil || bundle.Twitch.ClientID != testClientID || bundle.Twitch.UserID != testReaderID ||
				bundle.Twitch.AccessToken != newAccess || bundle.Twitch.RefreshToken != newRefresh ||
				!bundle.Twitch.ExpiresAt.Equal(testTime().Add(time.Hour))) {
				t.Fatalf("Ротация вернула неполную пару или изменила владельца %+v", bundle)
			}
		})
	}
}

// TestDeviceOAuthAcceptsOnlyPublicVerificationURL исключает передачу приватного
// кода в публичном URL и переход на сервер, адрес которого не принадлежит Twitch.
func TestDeviceOAuthAcceptsOnlyPublicVerificationURL(t *testing.T) {
	// target содержит очередную непригодную ссылку подтверждения из ответа сервера.
	for _, target := range []string{
		"https://www.twitch.tv/activate?device-code=private-device",
		"https://www.twitch.tv/activate?device-code=ABCDEF&token=private-device",
		"https://www.twitch.tv.fixture.invalid/activate?device-code=ABCDEF",
		"http://www.twitch.tv/activate?device-code=ABCDEF",
	} {
		// adapter получает формально корректный ответ, но с небезопасной ссылкой.
		adapter := oauthTestAdapter(t, core.DefaultChatLimits(), func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/oauth2/device" {
				t.Errorf("Неожиданный путь запроса %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeTwitchResponse(t, w, http.StatusOK, fmt.Sprintf(
				`{"device_code":"private-device","user_code":"ABCDEF","verification_uri":%q,"expires_in":30,"interval":5}`, target))
		})
		// result должен оставаться пустым, а err подтверждает отказ до выдачи ссылки.
		result, err := adapter.BeginDevice(t.Context(), testClientID)
		if err == nil || result.VerificationURI != "" || result.DeviceCode != "" {
			t.Errorf("Небезопасная ссылка %q привела к выдаче %+v, ошибка %v", target, result, err)
		}
	}
}
