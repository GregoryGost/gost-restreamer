package api_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/chat"
	"github.com/GregoryGost/gost-restreamer/internal/chat/twitch"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

const (
	// acceptancePrivatePrefix отмечает все синтетические полномочия, которые проверяются на отсутствие в HTTP.
	acceptancePrivatePrefix = "private-platform-fixture-"
	// acceptanceActivationURI содержит только публичный адрес подтверждения.
	acceptanceActivationURI = "https://www.twitch.tv/activate"
	// acceptanceUserCode представляет короткий код, который виден до подтверждения Twitch.
	acceptanceUserCode = "CHAT42"
)

// acceptancePlatform заменяет обмен с одной площадкой проверкой, которую отпускает тест.
type acceptancePlatform struct {
	// provider определяет площадку, которую представляет этот экземпляр.
	provider core.ChatProvider
	// calls сообщает о каждом допущенном обмене и позволяет закончить его точным результатом.
	calls chan acceptancePlatformCall
	// attempts учитывает сетевые вызовы, включая те, которые тест не ожидал.
	attempts atomic.Int32
}

// acceptancePlatformCall удерживает обмен до получения ошибки или подтверждения от теста.
type acceptancePlatformCall struct {
	// submitted содержит оба переданных реквизита в порядке вызова адаптера.
	submitted [2]string
	// finish передаёт результат обмена без повторного сетевого вызова.
	finish chan error
}

// Provider сообщает Manager, к какой платформе относятся проверки этого экземпляра.
func (a *acceptancePlatform) Provider() core.ChatProvider { return a.provider }

// Discover возвращает отказ для тестов полномочий, в которых эфира нет.
func (*acceptancePlatform) Discover(context.Context, core.ChatSource, core.ChatCredentials) (core.ChatDiscovery, error) {
	return core.ChatDiscovery{}, core.ErrNotReady
}

// Read возвращает отказ при неожиданном запуске чтения в сценарии без эфира.
func (*acceptancePlatform) Read(context.Context, core.ChatReadRequest, core.ChatAdapterSink) error {
	return core.ErrNotReady
}

// exchange сообщает два исходных реквизита в calls и ждёт решения теста либо отмены ctx.
func (a *acceptancePlatform) exchange(ctx context.Context, first, second string) error {
	a.attempts.Add(1)
	// call связывает переданные реквизиты и единственный ответ теста на этот вызов.
	call := acceptancePlatformCall{submitted: [2]string{first, second}, finish: make(chan error, 1)}
	select {
	case a.calls <- call:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	// err сообщает исход синтетической проверки.
	case err := <-call.finish:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ValidateAPIKey передаёт исходный Google API Key в контролируемый обмен.
func (a *acceptancePlatform) ValidateAPIKey(ctx context.Context, key string) (core.ChatReason, error) {
	return core.ChatReasonNone, a.exchange(ctx, key, "")
}

// VerifyCredentials ждёт проверки исходной пары DevAPI и выпускает синтетический токен.
func (a *acceptancePlatform) VerifyCredentials(ctx context.Context, clientID, secret string) (core.VKVideoLiveChatCredentials, error) {
	return core.VKVideoLiveChatCredentials{
		ClientID: clientID, ClientSecret: secret, AccessToken: acceptancePrivatePrefix + "vk-access",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond),
	}, a.exchange(ctx, clientID, secret)
}

// BeginDevice выдаёт Twitch-код со сроком и коротким интервалом для локального сценария.
func (*acceptancePlatform) BeginDevice(_ context.Context, _ string) (twitch.DeviceChallenge, error) {
	return twitch.DeviceChallenge{
		DeviceCode: acceptancePrivatePrefix + "device", UserCode: acceptanceUserCode,
		VerificationURI: acceptanceActivationURI, ExpiresIn: time.Minute, Interval: time.Millisecond,
	}, nil
}

// PollDevice позволяет тесту подтвердить клиент и закрытый код до выдачи пары токенов.
func (a *acceptancePlatform) PollDevice(ctx context.Context, clientID, code string) (twitch.DevicePoll, error) {
	// err представляет отказ подтверждения до выдачи синтетической пары.
	if err := a.exchange(ctx, clientID, code); err != nil {
		return twitch.DevicePoll{}, err
	}
	return twitch.DevicePoll{State: twitch.DeviceComplete, Bundle: &core.ChatCredentialBundle{
		Twitch: &core.TwitchChatCredentials{ClientID: clientID, UserID: "42",
			AccessToken: acceptancePrivatePrefix + "twitch-access", RefreshToken: acceptancePrivatePrefix + "twitch-refresh",
			Scopes: []string{"user:read:chat"}, ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)},
	}}, nil
}

// newChatAcceptanceHandler создаёт изолированные Manager и REST API для заданных adapters и limits.
// Явный owner позволяет разместить отказ только на выбранной границе записи теста.
func newChatAcceptanceHandler(t *testing.T, fixture *apiAcceptanceFixture, limits core.ChatLimits, adapters []core.ChatAdapterPort, owner ...chat.CredentialReader) (*api.API, *chat.Manager) {
	t.Helper()
	// manager пользуется мигрированной базой fixture; err сообщает об отказе регистрации платформ.
	manager, err := chat.New(t.Context(), fixture.service, fixture.store, limits, adapters, owner...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// ctx ограничивает ожидание Manager, cancel освобождает таймер очистки.
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		// err сообщает, завершил ли Manager работы до закрытия SQLite.
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	// handler регистрирует реальные маршруты, промежуточные обработчики и схемы API.
	handler, err := api.New(api.Config{Core: fixture.service, Chat: manager, ChatLimits: limits,
		Limits: fixture.limits, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// ctx ограничивает завершение HTTP после закрытия клиентов.
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		// err сообщает о незавершённых HTTP-обработчиках.
		if err := handler.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return handler, manager
}

// acceptancePublicChatJSON запрещает полномочия в теле публичного ответа и во всех заголовках.
func acceptancePublicChatJSON(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	// value содержит тело либо заголовки публичного HTTP-ответа.
	for _, value := range []string{response.Body.String(), fmt.Sprint(response.Header())} {
		if strings.Contains(value, acceptancePrivatePrefix) {
			t.Fatal("Публичный HTTP-ответ содержит полномочия площадки")
		}
	}
	return apiAcceptanceJSON(t, response)
}

// acceptanceAuthorizationLocation проверяет Location и начальную проекцию одной принятой попытки.
func acceptanceAuthorizationLocation(t *testing.T, handler http.Handler, body string, provider core.ChatProvider) (string, string) {
	t.Helper()
	// response содержит принятое подключение до получения результата проверки платформы.
	response := apiAcceptanceRequest(t, handler, "POST", "/chat/accounts/connect", body, 202)
	// pending содержит публичные поля начального состояния.
	pending := acceptancePublicChatJSON(t, response)
	// id адресует эту попытку в Location.
	id, _ := pending["attempt_id"].(string)
	// path адресует GET/DELETE принятой попытки относительно общего префикса.
	path := "/chat/accounts/authorizations/" + id
	if id == "" || response.Header().Get("Location") != api.Prefix+path ||
		pending["state"] != "PENDING" || pending["cancelable"] != true || pending["provider"] != string(provider) {
		t.Fatalf("Неверный допуск авторизации: %v, Location=%q", pending, response.Header().Get("Location"))
	}
	return id, path
}

// acceptanceAwaitAuthorization читает публичный статус до выполнения ready, ограничивая ожидание ctx.
func acceptanceAwaitAuthorization(t *testing.T, ctx context.Context, handler http.Handler, path string, ready func(map[string]any) bool) map[string]any {
	t.Helper()
	// tick ограничивает частоту чтений RAM-попытки.
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		// view содержит последнее публичное состояние, включая безопасную причину.
		view := acceptancePublicChatJSON(t, apiAcceptanceRequest(t, handler, "GET", path, "", 200))
		if ready(view) {
			return view
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Не дождались результата авторизации: %v, последнее состояние %v", ctx.Err(), view)
		case <-tick.C:
		}
	}
}

// acceptanceWaitPlatform получает ровно один допущенный обмен и сверяет его исходные реквизиты.
func acceptanceWaitPlatform(t *testing.T, ctx context.Context, adapter *acceptancePlatform, first, second string) acceptancePlatformCall {
	t.Helper()
	select {
	// call содержит входы, полученные адаптером, и канал его результата.
	case call := <-adapter.calls:
		if call.submitted != [2]string{first, second} {
			t.Fatal("Внешняя проверка получила другую пару реквизитов")
		}
		return call
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return acceptancePlatformCall{}
	}
}

// TestChatConnectUsesSameVerifiedCreateAndReplaceFlow сверяет единый порядок через HTTP
// для трёх адаптеров. Проверка блокирует запись, успешная замена сохраняет идентичность,
// а CAS перед платформой отклоняет старую ревизию и не порождает нового обмена.
func TestChatConnectUsesSameVerifiedCreateAndReplaceFlow(t *testing.T) {
	// provider задаёт одну из трёх платформ с собственным входным набором.
	for _, provider := range []core.ChatProvider{core.ChatYouTube, core.ChatTwitch, core.ChatVKVideoLive} {
		t.Run(string(provider), func(t *testing.T) {
			// Каждый вариант создаёт новую базу и выпускает только синтетические токены.
			// ctx ограничивает ожидания сценария, cancel снимает срок при выходе.
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// fixture владеет чистой мигрированной SQLite.
			fixture := newAPIAcceptanceFixture(t, nil)
			// adapter ждёт разрешения теста на каждый обмен.
			adapter := &acceptancePlatform{provider: provider, calls: make(chan acceptancePlatformCall, 1)}
			// handler обслуживает настоящий REST со штатной валидацией и чтением из SQLite.
			handler, _ := newChatAcceptanceHandler(t, fixture, core.DefaultChatLimits(), []core.ChatAdapterPort{adapter})
			// accountID остаётся пустым до создания и затем выбирает единственную цель замены.
			var accountID string
			// revision различает создание первой ревизии и замену по текущей ревизии.
			for revision := 1; revision <= 2; revision++ {
				// first содержит первый закрытый реквизит, second — дополнительный аргумент фактического вызова адаптера.
				first, second := fmt.Sprintf("%s%d", acceptancePrivatePrefix, revision), ""
				// fields содержит передаваемые на HTTP входы выбранного провайдера.
				fields := fmt.Sprintf(`"api_key":%q`, first)
				switch provider {
				case core.ChatTwitch:
					fields = fmt.Sprintf(`"client_id":%q`, first)
					second = acceptancePrivatePrefix + "device"
				case core.ChatVKVideoLive:
					second = fmt.Sprintf("%sdevapi-secret-%d", acceptancePrivatePrefix, revision)
					fields = fmt.Sprintf(`"client_id":%q,"client_secret":%q`, first, second)
				}
				// target передаёт имя при создании либо те же ID и CAS при замене.
				target := `"name":"Площадка"`
				if revision == 2 {
					target = fmt.Sprintf(`"account_id":%q,"expected_revision":1`, accountID)
				}
				// body содержит фактический запрос общей операции подключения.
				body := fmt.Sprintf(`{"provider":%q,%s,%s}`, provider, fields, target)
				// attemptID служит квитанцией данной записи, path адресует её публичное состояние.
				attemptID, path := acceptanceAuthorizationLocation(t, handler, body, provider)
				// call подтверждает, что адаптер уже получил реквизиты, но ещё не разрешил запись.
				call := acceptanceWaitPlatform(t, ctx, adapter, first, second)
				// waiting — публичная проекция после входа в контролируемый сетевой обмен.
				waiting := acceptancePublicChatJSON(t, apiAcceptanceRequest(t, handler, "GET", path, "", 200))
				if waiting["cancelable"] != true || waiting["state"] != "PENDING" {
					t.Fatalf("Проверка площадки преждевременно закрыла попытку: %v", waiting)
				}
				if provider == core.ChatTwitch && (waiting["user_code"] != acceptanceUserCode || waiting["verification_uri"] != acceptanceActivationURI) {
					t.Fatalf("Во время ожидания Twitch нет публичного подтверждения: %v", waiting)
				}
				// before содержит число сохранённых аккаунтов до разрешения платформы.
				before := acceptancePublicChatJSON(t, apiAcceptanceRequest(t, handler, "GET", "/chat/accounts", "", 200))
				readbackItems(t, before, revision-1)
				if revision == 2 {
					// old хранит предыдущую пару во время замены, err сообщает об отказе чтения.
					old, err := fixture.service.GetChatCredentialResult(ctx, core.ID(accountID))
					if err != nil || old.Credentials.CredentialRevision != 1 || old.Credentials.CredentialWriteID == core.ID(attemptID) {
						t.Fatalf("Полномочия до проверки платформы изменились: %v", err)
					}
				}
				call.finish <- nil
				// view содержит первый окончательный результат, включая идентичность сохранённого аккаунта.
				view := acceptanceAwaitAuthorization(t, ctx, handler, path, func(value map[string]any) bool {
					// value содержит очередную публичную проекцию этого подключения.
					return value["state"] != "PENDING"
				})
				if revision == 1 {
					accountID, _ = view["account_id"].(string)
				}
				if accountID == "" || view["account_id"] != accountID || view["state"] != "SUCCEEDED" || view["cancelable"] != false ||
					view["user_code"] != nil || view["verification_uri"] != nil {
					t.Fatalf("Неверный результат записи: %v", view)
				}
				// saved содержит подтверждённый набор прямого серверного чтения, err сообщает отказ временной базы.
				saved, err := fixture.service.GetChatCredentialResult(ctx, core.ID(accountID))
				if err != nil || saved.Account.Revision != uint64(revision) || saved.Credentials.CredentialRevision != uint64(revision) ||
					saved.Credentials.CredentialWriteID != core.ID(attemptID) {
					t.Fatalf("SQLite не подтвердила обе ревизии или WriteID: %v", err)
				}
				assertAcceptanceVerifiedBundle(t, saved.Credentials.Bundle, provider, first, second)
				// public проверяет ту же сохранённую строку через независимую публичную проекцию.
				public := acceptancePublicChatJSON(t, apiAcceptanceRequest(t, handler, "GET", "/chat/accounts/"+accountID, "", 200))
				readbackFields(t, public, map[string]string{"id": accountID, "name": "Площадка", "authorization_state": "CONFIGURED"})
				readbackNumbers(t, public, map[string]uint64{"revision": uint64(revision)})
				if revision == 2 {
					acceptancePublicChatJSON(t, apiAcceptanceRequest(t, handler, "POST", "/chat/accounts/connect", body, 409))
				}
			}
			if adapter.attempts.Load() != 2 {
				t.Fatalf("Устаревший CAS допустил дополнительный обмен: %d", adapter.attempts.Load())
			}
			readbackItems(t, acceptancePublicChatJSON(t, apiAcceptanceRequest(t, handler, "GET", "/chat/accounts", "", 200)), 1)
		})
	}
}

// assertAcceptanceVerifiedBundle сверяет реквизиты формы first и second с подтверждённым
// серверным набором выбранного provider после сохранения. Тест не печатает их значения.
func assertAcceptanceVerifiedBundle(t *testing.T, bundle core.ChatCredentialBundle, provider core.ChatProvider, first, second string) {
	t.Helper()
	switch provider {
	case core.ChatYouTube:
		if bundle.YouTube == nil || bundle.YouTube.APIKey != first {
			t.Fatal("YouTube сохранил другой API Key")
		}
	case core.ChatTwitch:
		if bundle.Twitch == nil || bundle.Twitch.ClientID != first || bundle.Twitch.UserID != "42" ||
			bundle.Twitch.AccessToken != acceptancePrivatePrefix+"twitch-access" || bundle.Twitch.RefreshToken != acceptancePrivatePrefix+"twitch-refresh" ||
			!reflect.DeepEqual(bundle.Twitch.Scopes, []string{"user:read:chat"}) {
			t.Fatal("Twitch сохранил другую пару полномочий")
		}
	case core.ChatVKVideoLive:
		if bundle.VKVideoLive == nil || bundle.VKVideoLive.ClientID != first || bundle.VKVideoLive.ClientSecret != second ||
			bundle.VKVideoLive.AccessToken != acceptancePrivatePrefix+"vk-access" {
			t.Fatal("VK Видео Live сохранил другой результат DevAPI")
		}
	}
}

// acceptanceLostCredentialReceipt теряет ответ после настоящего SQLite CAS и временно скрывает
// квитанцию записи. Обычные публичные чтения метаданных используют исходный Service.
type acceptanceLostCredentialReceipt struct {
	// Service читает первоначальный набор и сохраняет замену в мигрированной SQLite.
	*core.Service
	// admitted показывает, что Manager пересёк границу допуска к записи.
	admitted chan struct{}
	// finish открывает проход к настоящему CAS, пока тест проверяет отмену допущенной записи.
	finish chan struct{}
	// writes считает обращение к записи, чтобы выявить автоматический повтор.
	writes atomic.Int32
	// unavailable ограничивает отказ чтением квитанции после настоящего COMMIT.
	unavailable atomic.Bool
}

// ReplaceChatCredentials завершает один CAS с id, provider, bundle, writeID и expected.
// В момент допуска он ждёт разрешения теста или отмену ctx, а после успешного SQLite COMMIT
// сообщает об утере ответа. Фоновое повторение гарантированно фиксируется отдельным счётчиком.
func (s *acceptanceLostCredentialReceipt) ReplaceChatCredentials(ctx context.Context, id core.ID, provider core.ChatProvider, bundle core.ChatCredentialBundle, writeID core.ID, expected uint64) (core.ChatCredentialResult, error) {
	if s.writes.Add(1) == 1 {
		close(s.admitted)
	}
	select {
	case <-s.finish:
	case <-ctx.Done():
		return core.ChatCredentialResult{}, ctx.Err()
	}
	// written содержит полную квитанцию реальной замены, err сообщает исход CAS.
	written, err := s.Service.ReplaceChatCredentials(ctx, id, provider, bundle, writeID, expected)
	if err != nil {
		return written, err
	}
	s.unavailable.Store(true)
	return written, context.DeadlineExceeded
}

// GetChatCredentialResult читает полномочия id в контексте ctx либо воспроизводит отказ сверки.
func (s *acceptanceLostCredentialReceipt) GetChatCredentialResult(ctx context.Context, id core.ID) (core.ChatCredentialResult, error) {
	if s.unavailable.Load() {
		return core.ChatCredentialResult{}, core.ErrBusy
	}
	return s.Service.GetChatCredentialResult(ctx, id)
}

// TestChatConnectLostCommitRemainsNonCancelableOverHTTP подтверждает, что после допуска
// нельзя отменить запись. Потеря подтверждения COMMIT остаётся отдельным публичным состоянием,
// хотя прямое чтение SQLite видит изменённый аккаунт, а адаптер и запись не повторяются.
func TestChatConnectLostCommitRemainsNonCancelableOverHTTP(t *testing.T) {
	// ctx ограничивает ожидание принятия, записи и результата, cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// fixture содержит изолированный Store и штатный сервис публичных метаданных.
	fixture := newAPIAcceptanceFixture(t, nil)
	// initial сохраняет действующий набор до попытки замены.
	initial, err := fixture.service.CreateChatAccount(ctx, core.ChatAccount{Provider: core.ChatYouTube},
		core.ChatCredentialBundle{YouTube: &core.YouTubeChatCredentials{APIKey: acceptancePrivatePrefix + "original-key"}}, "initial-chat-store")
	if err != nil {
		t.Fatal(err)
	}
	// fault ограничивает сбой портом записи и последующей сверки полномочий.
	fault := &acceptanceLostCredentialReceipt{Service: fixture.service, admitted: make(chan struct{}), finish: make(chan struct{})}
	// limits задаёт штатные сроки работы изолированного владельца.
	limits := core.DefaultChatLimits()
	// owner получает штатную квитанцию CAS через порт с управляемой потерей подтверждения.
	owner, err := chat.NewCredentialOwner(fault, limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает внешний для Manager владелец до освобождения временной SQLite.
		// cleanup ограничивает завершение, stop освобождает таймер.
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		// err сообщает, остались ли нетерминированные пользователи владельца.
		if err := owner.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	// adapter ждёт контрольный ответ синтетической площадки и считает попытки проверки.
	adapter := &acceptancePlatform{provider: core.ChatYouTube, calls: make(chan acceptancePlatformCall, 1)}
	// handler выполняет реальные HTTP-операции с настоящим Manager и проброшенным отказом квитанции.
	handler, _ := newChatAcceptanceHandler(t, fixture, limits, []core.ChatAdapterPort{adapter}, owner)
	// key представляет выбранную замену поля API Key.
	key := acceptancePrivatePrefix + "replacement-key"
	// body передаёт реквизит и исходную конфигурационную ревизию.
	body := fmt.Sprintf(`{"provider":"youtube","api_key":%q,"account_id":%q,"expected_revision":1}`, key, initial.Account.ID)
	// attemptID адресует будущую квитанцию SQLite, path выбирает публичное состояние и отмену.
	attemptID, path := acceptanceAuthorizationLocation(t, handler, body, core.ChatYouTube)
	// call отпускает внешнюю проверку, после которой наступает допуск к изменению.
	call := acceptanceWaitPlatform(t, ctx, adapter, key, "")
	call.finish <- nil
	select {
	case <-fault.admitted:
	case <-ctx.Done():
		t.Fatal("Проверенный набор не дошёл до сохранения", ctx.Err())
	}
	// blocked содержит PENDING после допуска ещё до физической записи.
	blocked := acceptancePublicChatJSON(t, apiAcceptanceRequest(t, handler, "GET", path, "", 200))
	if blocked["state"] != "PENDING" || blocked["cancelable"] != false || blocked["code"] != nil {
		t.Fatalf("Допущенная запись получила неверную проекцию до COMMIT: %v", blocked)
	}
	acceptancePublicChatJSON(t, apiAcceptanceRequest(t, handler, "DELETE", path, "", 409))
	close(fault.finish)

	// pending показывает, что публичный исход не установлен после зафиксированной записи.
	pending := acceptanceAwaitAuthorization(t, ctx, handler, path, func(value map[string]any) bool {
		// value содержит очередной ответ, включая возможную ошибочную смену состояния.
		return value["code"] == "credential_commit_unknown" || value["state"] != "PENDING"
	})
	if pending["state"] != "PENDING" || pending["cancelable"] != false || pending["code"] != "credential_commit_unknown" ||
		pending["account_id"] != string(initial.Account.ID) || pending["user_code"] != nil || pending["verification_uri"] != nil {
		t.Fatalf("Потеря COMMIT получила неверное публичное состояние: %v", pending)
	}
	// conflict представляет повторную отмену после потери подтверждения и сохраняет безопасную причину.
	conflict := acceptancePublicChatJSON(t, apiAcceptanceRequest(t, handler, "DELETE", path, "", 409))
	if conflict["code"] != "credential_commit_unknown" {
		t.Fatalf("Неустановленный COMMIT не получил свой код отмены: %v", conflict)
	}
	// saved читается напрямую из SQLite мимо потери подтверждения, err отражает настоящий исход чтения.
	saved, err := fixture.service.GetChatCredentialResult(ctx, initial.Account.ID)
	if err != nil || saved.Account.Revision != 2 || saved.Credentials.CredentialRevision != 2 ||
		saved.Credentials.CredentialWriteID != core.ID(attemptID) || saved.Credentials.Bundle.YouTube == nil ||
		saved.Credentials.Bundle.YouTube.APIKey != key {
		t.Fatalf("SQLite не подтвердила единственный CAS до потери ответа: %v", err)
	}
	// public подтверждает, что обычное чтение метаданных работает независимо от результата RAM-попытки.
	public := acceptancePublicChatJSON(t, apiAcceptanceRequest(t, handler, "GET", "/chat/accounts/"+string(initial.Account.ID), "", 200))
	readbackNumbers(t, public, map[string]uint64{"revision": 2})
	readbackFields(t, public, map[string]string{"authorization_state": "CONFIGURED"})
	if adapter.attempts.Load() != 1 || fault.writes.Load() != 1 {
		t.Fatalf("Неопределённость повторила обмен или сохранение: %d / %d", adapter.attempts.Load(), fault.writes.Load())
	}
}

// TestChatConnectRechecksRevisionAfterVerification проверяет второй CAS той же замены.
// Переименование между допуском площадки и записью сохраняет актуальное имя, а проверенные
// новые реквизиты не перезаписывают прежние полномочия по устаревшей конфигурационной ревизии.
func TestChatConnectRechecksRevisionAfterVerification(t *testing.T) {
	// ctx ограничивает сценарий и синтетическую проверку, cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// fixture использует штатные CRUD и CAS для временной SQLite.
	fixture := newAPIAcceptanceFixture(t, nil)
	// initial хранит первоначальное имя, ключ и обе ревизии аккаунта.
	initial, err := fixture.service.CreateChatAccount(ctx, core.ChatAccount{Provider: core.ChatYouTube, Name: "Прежнее имя"},
		core.ChatCredentialBundle{YouTube: &core.YouTubeChatCredentials{APIKey: acceptancePrivatePrefix + "cas-original"}}, "initial-chat-cas")
	if err != nil {
		t.Fatal(err)
	}
	// adapter позволяет переименовать аккаунт после первого допуска и до записи новой пары.
	adapter := &acceptancePlatform{provider: core.ChatYouTube, calls: make(chan acceptancePlatformCall, 1)}
	// handler предоставляет один настоящий CAS для переименования и другой для смены ключа.
	handler, _ := newChatAcceptanceHandler(t, fixture, core.DefaultChatLimits(), []core.ChatAdapterPort{adapter})
	// key поступит в SQLite только если второй CAS ошибочно пропустит смену ревизии.
	key := acceptancePrivatePrefix + "cas-replacement"
	// attemptPath содержит Location принятой до переименования попытки.
	_, attemptPath := acceptanceAuthorizationLocation(t, handler,
		fmt.Sprintf(`{"provider":"youtube","api_key":%q,"account_id":%q,"expected_revision":1}`, key, initial.Account.ID), core.ChatYouTube)
	// call доказывает приём исходной ревизии до конкурентного переименования.
	call := acceptanceWaitPlatform(t, ctx, adapter, key, "")
	// accountPath адресует неизменную сохранённую запись аккаунта.
	accountPath := "/chat/accounts/" + string(initial.Account.ID)
	// renamed содержит новую конфигурационную ревизию при прежних реквизитах.
	renamed := acceptancePublicChatJSON(t, apiAcceptanceRequest(t, handler, "PATCH", accountPath, `{"name":"Имя после проверки","expected_revision":1}`, 200))
	readbackNumbers(t, renamed, map[string]uint64{"revision": 2})
	call.finish <- nil
	// rejected содержит безопасный результат сохранения по ревизии, устаревшей за время проверки.
	rejected := acceptanceAwaitAuthorization(t, ctx, handler, attemptPath, func(value map[string]any) bool {
		// value содержит очередную проекцию результата, включая ошибочную неоднозначную запись.
		return value["state"] != "PENDING" || value["code"] != nil
	})
	if rejected["state"] != "FAILED" || rejected["cancelable"] != false {
		t.Fatalf("Устаревший CAS получил неверный результат: %v", rejected)
	}
	// saved содержит результат SQLite после попытки, err сообщает об отказе чтения.
	saved, err := fixture.service.GetChatCredentialResult(ctx, initial.Account.ID)
	if err != nil || saved.Account.Revision != 2 || saved.Account.Name != "Имя после проверки" ||
		!reflect.DeepEqual(saved.Credentials, initial.Credentials) || adapter.attempts.Load() != 1 {
		t.Fatalf("Конкурентная смена реквизитов нарушила сохранённые ревизии или имя: %v", err)
	}
}
