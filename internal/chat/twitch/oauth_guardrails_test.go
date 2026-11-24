package twitch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// oauthRoundTrip заменяет транспорт без сокетов и фоновых процессов.
type oauthRoundTrip func(*http.Request) (*http.Response, error)

// RoundTrip передаёт request синхронной фикстуре и закрывает тело запроса.
func (f oauthRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body != nil {
		// Функция закрывает входную форму даже при отказе смоделированного сервера.
		defer func() { _ = request.Body.Close() }()
	}
	return f(request)
}

// oauthResponse создаёт конечный ответ с заданным статусом и текстом.
func oauthResponse(status int, text string) *http.Response {
	// response имитирует конечный ответ, а для 429 задаёт срок до следующего запроса.
	response := &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(text))}
	if status == http.StatusTooManyRequests {
		response.Header.Set("Retry-After", "19")
	}
	return response
}

// TestRefreshReportsWhetherExchangeMayHaveConsumedToken различает ошибки,
// при которых владелец может снова использовать прежний набор, и неоднозначные
// исходы после начала обмена.
func TestRefreshReportsWhetherExchangeMayHaveConsumedToken(t *testing.T) {
	// current содержит исходный серверный набор с действительным локальным сроком.
	current := core.ChatCredentials{
		Provider: core.ChatTwitch,
		Bundle: core.ChatCredentialBundle{Twitch: &core.TwitchChatCredentials{
			ClientID: testClientID, UserID: testReaderID, AccessToken: testAccessToken, RefreshToken: testRefreshToken,
			Scopes: []string{"user:read:chat"}, ExpiresAt: testTime().Add(time.Hour),
		}},
	}
	// tokenBody представляет успешную выдачу пары до обязательной проверки.
	const tokenBody = `{"access_token":"fixture-next-access","refresh_token":"fixture-next-refresh","expires_in":3600,"token_type":"bearer"}`
	// scenario выбирает стадию наблюдаемого отказа и ожидаемый запрет повторного обмена.
	for _, scenario := range []struct {
		// name задаёт имя сценария в отчёте тестирования.
		name string
		// oldStatus показывает исход проверки исходного доступа.
		oldStatus int
		// exchangeStatus задаёт статус ответа на одноразовый POST при состоявшемся HTTP-ответе.
		exchangeStatus int
		// exchangeErr имитирует потерю ответа после передачи запроса транспорту.
		exchangeErr error
		// cancelDuringExchange останавливает вызывающего во время обмена.
		cancelDuringExchange bool
		// wantErr задаёт безопасную причину, передаваемую владельцу полномочий.
		wantErr error
		// wantTimeout отличает таймаут транспорта от других временных отказов.
		wantTimeout bool
		// wantConsumed показывает, следует ли владельцу запретить повтор прежнего обмена.
		wantConsumed bool
		// wantCalls определяет число фактических запросов до окончания операции.
		wantCalls int
	}{
		{name: "проверка исходного токена недоступна", oldStatus: http.StatusServiceUnavailable, wantErr: core.ErrNotReady, wantCalls: 1},
		{name: "проверка ограничена по темпу", oldStatus: http.StatusTooManyRequests, wantErr: core.ErrNotReady, wantCalls: 1},
		{name: "обмен отклонён по темпу", oldStatus: http.StatusUnauthorized, exchangeStatus: http.StatusTooManyRequests, wantErr: core.ErrNotReady, wantCalls: 2},
		{name: "ответ обмена потерян по таймауту транспорта", oldStatus: http.StatusUnauthorized, exchangeErr: context.DeadlineExceeded, wantErr: core.ErrNotReady, wantTimeout: true, wantConsumed: true, wantCalls: 2},
		{name: "вызывающий отменил обмен", oldStatus: http.StatusUnauthorized, cancelDuringExchange: true, wantErr: context.Canceled, wantConsumed: true, wantCalls: 2},
		{name: "новая пара выдана без доступной проверки", oldStatus: http.StatusUnauthorized, exchangeStatus: http.StatusOK, wantErr: core.ErrNotReady, wantConsumed: true, wantCalls: 3},
	} {
		// Подтест направляет вызовы OAuth в синхронный транспорт для выбранной последовательности.
		t.Run(scenario.name, func(t *testing.T) {
			// adapter использует проверяемую реализацию Refresh, err сообщает об ошибке начальной настройки.
			adapter, err := New(core.DefaultChatLimits())
			if err != nil {
				t.Fatal(err)
			}
			// ctx действует на весь обмен, cancel позволяет воспроизвести отмену внутри POST.
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			// calls считает обращения к транспорту для выявления любого ложного или повторного обмена.
			calls := 0
			// Callback различает текущий токен, запрос обмена и обязательную проверку выданной пары.
			adapter.httpClient.Transport = oauthRoundTrip(func(request *http.Request) (*http.Response, error) {
				calls++
				switch request.URL.Path {
				case "/oauth2/validate":
					if request.Header.Get("Authorization") == "OAuth "+testAccessToken {
						return oauthResponse(scenario.oldStatus, ""), nil
					}
					return oauthResponse(http.StatusServiceUnavailable, ""), nil
				case "/oauth2/token":
					if scenario.cancelDuringExchange {
						cancel()
						return nil, request.Context().Err()
					}
					if scenario.exchangeErr != nil {
						return nil, scenario.exchangeErr
					}
					return oauthResponse(scenario.exchangeStatus, tokenBody), nil
				default:
					t.Errorf("Неожиданный путь запроса %s", request.URL.Path)
					return oauthResponse(http.StatusNotFound, ""), nil
				}
			})
			// bundle должен оставаться пустым, consumed сохраняет риск обмена, err объясняет безопасный отказ.
			bundle, consumed, err := adapter.Refresh(ctx, current)
			if bundle != nil || !errors.Is(err, scenario.wantErr) || consumed != scenario.wantConsumed || calls != scenario.wantCalls {
				t.Fatalf("Пара получена=%t, возможный обмен=%t, отказ=%v, запросов=%d", bundle != nil, consumed, err, calls)
			}
			if errors.Is(err, context.DeadlineExceeded) != scenario.wantTimeout {
				t.Fatalf("Исход обмена неверно классифицирован как таймаут=%t, ошибка=%v", !scenario.wantTimeout, err)
			}
			// retry содержит срок, который общий владелец должен учесть при следующем запросе.
			var retry *core.ChatReadRetry
			if (scenario.oldStatus == http.StatusTooManyRequests || scenario.exchangeStatus == http.StatusTooManyRequests) &&
				(!errors.As(err, &retry) || retry.RetryAfter != 19*time.Second) {
				t.Fatalf("Потерян Retry-After после ограничения запросов: %v", err)
			}
		})
	}
}

// TestOAuthRESTTransportRejectsRedirectAndForeignRequest проверяет границу
// транспорта даже при подмене клиента HTTP, разрешающей перенаправления.
func TestOAuthRESTTransportRejectsRedirectAndForeignRequest(t *testing.T) {
	// adapter и err содержат созданный с допустимыми пределами адаптер и итог создания.
	adapter, err := New(core.DefaultChatLimits())
	if err != nil {
		t.Fatal(err)
	}
	// calls считает любые запросы, полученные транспортом.
	calls := 0
	// Callback возвращает перенаправление на посторонний адрес. Запрос к нему
	// увеличил бы calls и нарушил проверку.
	adapter.httpClient = &http.Client{Transport: oauthRoundTrip(func(request *http.Request) (*http.Response, error) {
		calls++
		// response сохраняет перенаправление, которое должен увидеть do.
		response := oauthResponse(http.StatusTemporaryRedirect, "")
		response.Header.Set("Location", "https://fixture.invalid/collect")
		return response, nil
	})}

	// request и err относятся к запросу с OAuth-заголовком на разрешённый адрес.
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, adapter.oauthBase+"/oauth2/validate", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "OAuth "+testAccessToken)
	// status и err показывают, получил ли вызывающий ответ исходного сервера.
	status, _, _, err := adapter.do(request)
	if err != nil || status != http.StatusTemporaryRedirect || calls != 1 {
		t.Fatalf("Ответ перенаправления %d, ошибка=%v, запросов=%d", status, err, calls)
	}

	// foreign и err относятся к синтаксически корректному запросу на посторонний адрес.
	foreign, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://id.twitch.tv.fixture.invalid/oauth2/validate", nil)
	if err != nil {
		t.Fatal(err)
	}
	// err должен сообщить об отказе до передачи непризнанного URL транспорту.
	if _, _, _, err := adapter.do(foreign); !errors.Is(err, core.ErrInvalid) || calls != 1 {
		t.Fatalf("Посторонний адрес: ошибка=%v, запросов=%d", err, calls)
	}
}

// TestDeviceOAuthBoundsProviderTiming проверяет ошибочные сроки, стандартный интервал
// при отсутствии поля и сокращение TTL на время, уже проведённое в запросе.
func TestDeviceOAuthBoundsProviderTiming(t *testing.T) {
	// noInterval содержит пригодный код без отдельного поля interval.
	const noInterval = `{"device_code":"device","user_code":"ABCDEF","verification_uri":"https://www.twitch.tv/activate","expires_in":4}`
	// cases задаёт ответы конечного запроса выдачи и надёжно отличает отказ от пригодной попытки.
	cases := []struct {
		// name описывает входную ошибку или правило времени.
		name string
		// status возвращается от конечного OAuth-ресурса.
		status int
		// body содержит синтетический протокольный ответ.
		body string
		// elapsed моделирует длительность запроса выдачи.
		elapsed time.Duration
		// reason содержит ожидаемый отказ, если начать попытку нельзя.
		reason error
	}{
		{"неподходящее приложение", http.StatusBadRequest, "wrong", 0, core.ErrInvalid},
		{"сервис недоступен", http.StatusServiceUnavailable, "temporary", 0, core.ErrNotReady},
		{"повреждённый JSON", http.StatusOK, "{", 0, core.ErrNotReady},
		{"переполнение времени", http.StatusOK, strings.Replace(noInterval, `"expires_in":4`, `"expires_in":9223372036854775807`, 1), 0, core.ErrNotReady},
		{"код истёк до ответа", http.StatusOK, noInterval, 4 * time.Second, core.ErrNotReady},
		{"стандартный интервал", http.StatusOK, noInterval, time.Second, nil},
	}
	// scenario содержит один внешний ответ с собственным отсчётом.
	for _, scenario := range cases {
		// Подтест выполняет запрос выдачи без живых сетевых ресурсов.
		t.Run(scenario.name, func(t *testing.T) {
			// adapter содержит проверяемую валидацию и транспорт, err сообщает об отказе настройки.
			adapter, err := New(core.DefaultChatLimits())
			if err != nil {
				t.Fatal(err)
			}
			// now начинает отсчёт с фиксированного времени, затем транспорт моделирует затраченное время.
			now := testTime()
			// Функция возвращает единые виртуальные часы запроса.
			adapter.now = func() time.Time { return now }
			// Функция завершает один HTTP-запрос, сообщая новый момент чтения ответа.
			adapter.httpClient.Transport = oauthRoundTrip(func(*http.Request) (*http.Response, error) {
				now = now.Add(scenario.elapsed)
				return oauthResponse(scenario.status, scenario.body), nil
			})
			// challenge появляется только для пригодной попытки, err классифицирует все остальные ответы.
			challenge, err := adapter.BeginDevice(t.Context(), testClientID)
			if !errors.Is(err, scenario.reason) {
				t.Fatalf("Выдача вернула %v вместо %v", err, scenario.reason)
			}
			if scenario.reason == nil {
				if challenge.Interval != 5*time.Second || challenge.ExpiresIn != 3*time.Second {
					t.Fatalf("Интервал или оставшийся срок не соответствуют контракту: %+v", challenge)
				}
			} else if challenge != (DeviceChallenge{}) {
				t.Fatal("Неудачная выдача оставила публичный код подтверждения")
			}
		})
	}
}
