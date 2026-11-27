package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/chat/twitch"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

const (
	// managerTwitchClientID совпадает с Public-приложением исходных полномочий фикстуры.
	managerTwitchClientID = "test-client"
	// managerDeviceCode принадлежит только backend и проходит от выдачи к запросу обмена.
	managerDeviceCode = "private-fixture-device-code"
	// managerUserCode отображается владельцу и доступен, пока тот может подтвердить вход.
	managerUserCode = "TWTEST"
	// managerActivationURL показывает публичную страницу подтверждения Twitch.
	managerActivationURL = "https://www.twitch.tv/activate?public=true&device-code=TWTEST"
)

// managerTwitchPoll представляет один опрос Device Code, исход которого определяет тест.
type managerTwitchPoll struct {
	// at фиксирует момент обращения Manager, чтобы сверить интервалы.
	at time.Time
	// clientID получает ожидаемую идентичность публичного приложения.
	clientID string
	// code передаёт непрозрачный код устройства, полученный при создании попытки.
	code string
	// done сигнализирует отмену сетевой операции, даже если ответ уже получен на стороне Twitch.
	done <-chan struct{}
	// reply разрешает этому вызову завершиться с заданным результатом.
	reply chan twitch.DevicePoll
}

// managerTwitchDeviceFixture заменяет платформу на порте адаптера. Настоящие Manager,
// CredentialOwner и SQLite обрабатывают все результаты, сроки и изменения аккаунта.
type managerTwitchDeviceFixture struct {
	// ctx ограничивает поздний ответ, даже если тест завершился до его отправки.
	ctx context.Context
	// interval задаёт минимальную паузу между опросами.
	interval time.Duration
	// ttl задаёт объявленное Twitch время жизни попытки.
	ttl time.Duration
	// polls принимает сетевой запрос, которому тест вручную выдаёт результат.
	polls chan managerTwitchPoll
	// starts считает выдачу кодов, в том числе при проверке переполнения реестра.
	starts atomic.Int32
	// allowLateReply разрешает только одному опросу вернуть полученную пару после отмены запроса.
	allowLateReply atomic.Bool
}

// Provider возвращает платформу, на которой Manager применяет сценарий авторизации.
func (*managerTwitchDeviceFixture) Provider() core.ChatProvider { return core.ChatTwitch }

// Discover завершает ошибкой ненужное этим сценариям сетевое обнаружение для Source.
func (*managerTwitchDeviceFixture) Discover(context.Context, core.ChatSource, core.ChatCredentials) (core.ChatDiscovery, error) {
	return core.ChatDiscovery{}, core.ErrNotReady
}

// Read завершает ошибкой непредусмотренную попытку чтения: эти сценарии работают без публикующего выхода.
func (*managerTwitchDeviceFixture) Read(context.Context, core.ChatReadRequest, core.ChatAdapterSink) error {
	return core.ErrNotReady
}

// BeginDevice возвращает разные публичный и приватный коды, если ctx не отменён.
// clientID сверяется тестом при последующем опросе.
func (f *managerTwitchDeviceFixture) BeginDevice(ctx context.Context, _ string) (twitch.DeviceChallenge, error) {
	f.starts.Add(1)
	return twitch.DeviceChallenge{
		DeviceCode: managerDeviceCode, UserCode: managerUserCode, VerificationURI: managerActivationURL,
		ExpiresIn: f.ttl, Interval: f.interval,
	}, ctx.Err()
}

// PollDevice объявляет начало обмена code для clientID и ждёт результат тестовой платформы.
// При allowLateReply возвращает даже уже полученный результат, который совпал с отменой ctx.
func (f *managerTwitchDeviceFixture) PollDevice(ctx context.Context, clientID, code string) (twitch.DevicePoll, error) {
	// poll связывает время, принадлежность запроса и отдельный результат этого опроса.
	poll := managerTwitchPoll{
		at: time.Now(), clientID: clientID, code: code,
		done: ctx.Done(), reply: make(chan twitch.DevicePoll, 1),
	}
	select {
	case f.polls <- poll:
	case <-ctx.Done():
		return twitch.DevicePoll{}, ctx.Err()
	}
	// cancel завершает обмен при отзыве контекста, если тест не моделирует поздний уже полученный ответ.
	cancel := ctx.Done()
	if f.allowLateReply.CompareAndSwap(true, false) {
		cancel = nil
	}
	select {
	// result содержится в единственном ответе, выданном тестом на конкретный опрос.
	case result := <-poll.reply:
		return result, nil
	case <-f.ctx.Done():
		return twitch.DevicePoll{}, f.ctx.Err()
	case <-cancel:
		return twitch.DevicePoll{}, ctx.Err()
	}
}

// startManagerTwitchAuthorizationFixture создаёт Manager с adapter на мигрированной store.
// t закрывает реестр независимо от отмены ctx до освобождения файла SQLite.
func startManagerTwitchAuthorizationFixture(t *testing.T, ctx context.Context, service *core.Service, store *storage.Store, limits core.ChatLimits, adapter *managerTwitchDeviceFixture) *Manager {
	t.Helper()
	// manager управляет жизненным циклом реальных попыток, err сообщает об отказе настройки.
	manager, err := New(ctx, service, store, limits, []core.ChatAdapterPort{adapter})
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Функция завершает работы Manager, ограничивая ожидание внешних операций.
		// shutdown и cancel задают независимый срок этого ожидания.
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		chatTestOK(t, manager.Close(shutdown))
	})
	return manager
}

// waitManagerTwitchAuthorization опрашивает публичный статус id до ожидаемого состояния want.
// ctx служит пределом наблюдения и не влияет на время фонового опроса Manager.
func waitManagerTwitchAuthorization(t *testing.T, ctx context.Context, manager *Manager, id core.ID, want core.ChatAuthorizationState) core.ChatAuthorization {
	t.Helper()
	// ticker ограничивает частоту проверок, пока фоновая работа обрабатывает результат.
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		// status содержит публичный результат, err сообщает о недоступности попытки.
		status, err := manager.Authorization(ctx, id)
		chatTestOK(t, err)
		if status.State == want {
			return status
		}
		if status.State != core.ChatAuthorizationPending {
			t.Fatalf("Попытка завершилась состоянием %s вместо %s", status.State, want)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

// connectManagerTwitchAfterRelease повторяет запрос request, пока отменённая попытка
// удерживает допуск. Любой другой ответ Manager принимается сразу. ctx ограничивает ожидание.
func connectManagerTwitchAfterRelease(t *testing.T, ctx context.Context, manager *Manager, request core.ChatAuthorizationRequest) core.ChatAuthorization {
	t.Helper()
	// ticker ограничивает частоту обращений, пока фоновый опрос освобождает запись.
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		// attempt содержит следующее подтверждение, err до освобождения ресурса сообщает ErrBusy.
		attempt, err := manager.ConnectTwitch(ctx, request)
		if !errors.Is(err, core.ErrBusy) {
			chatTestOK(t, err)
			return attempt
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

// assertManagerTwitchPublic проверяет, что JSON value не содержит синтетических
// серверных полномочий. При отзыве кода проверяет также исчезновение страницы активации.
func assertManagerTwitchPublic(t *testing.T, value any, allowConfirmation bool) {
	t.Helper()
	// encoded представляет фактические поля возвращённого значения, err сообщает о сбое сериализации.
	encoded, err := json.Marshal(value)
	chatTestOK(t, err)
	// public используется для сравнения с синтетическими маркерами секретов.
	public := string(encoded)
	if strings.Contains(public, managerDeviceCode) ||
		strings.Contains(public, "test-access-") || strings.Contains(public, "test-refresh-") ||
		!allowConfirmation && (strings.Contains(public, managerUserCode) || strings.Contains(public, managerActivationURL)) {
		t.Fatal("Публичный ответ содержит полномочия или данные уже завершённого подтверждения")
	}
}

// TestManagerTwitchDevicePollingCreatesVerifiedAccount проверяет pending, минимальную задержку
// опроса, локальный TTL и единственную полную запись после подтверждения.
func TestManagerTwitchDevicePollingCreatesVerifiedAccount(t *testing.T) {
	// ctx и cancel ограничивают время создания изолированного аккаунта и работу Manager.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// store и service выполняют штатные миграции, чтения и записи аккаунта.
	store, service := openChatTestStore(t, ctx)
	// adapter задаёт минимальный интервал и больший срок, чем разрешено Manager.
	adapter := &managerTwitchDeviceFixture{
		ctx: ctx, interval: 30 * time.Millisecond, ttl: time.Minute, polls: make(chan managerTwitchPoll, 4),
	}
	// limits ограничивает срок попытки и число её сетевых запросов одновременно.
	limits := core.DefaultChatLimits()
	limits.AuthorizationTTL = 10 * time.Second
	limits.HTTPRequests = 1
	// manager обрабатывает фоновую попытку независимо от срока запроса ConnectTwitch.
	manager := startManagerTwitchAuthorizationFixture(t, ctx, service, store, limits, adapter)
	// requestCtx и endRequest имитируют закрытие первого HTTP-запроса после получения ответа.
	requestCtx, endRequest := context.WithCancel(ctx)
	defer endRequest()
	// began задаёт нижнюю границу времени выдачи кода для независимой проверки интервала.
	began := time.Now()
	// pending содержит публичное подтверждение. err сообщает о невозможности начать попытку.
	pending, err := manager.ConnectTwitch(requestCtx, core.ChatAuthorizationRequest{
		Provider: core.ChatTwitch, ClientID: managerTwitchClientID, Name: "Тестовый Twitch",
	})
	chatTestOK(t, err)
	endRequest()
	if pending.AttemptID == "" || pending.State != core.ChatAuthorizationPending || pending.AccountID != "" ||
		pending.UserCode != managerUserCode || pending.VerificationURI != managerActivationURL ||
		pending.ExpiresAt.Before(began.Add(10*time.Second-time.Microsecond)) || pending.ExpiresAt.After(time.Now().Add(10*time.Second)) {
		t.Fatalf("Получены неверные параметры ожидающей авторизации %+v", pending)
	}
	assertManagerTwitchPublic(t, pending, true)

	// first содержит первый наблюдаемый запрос опроса после выдачи кода.
	first := chatTestReceive(t, ctx, adapter.polls)
	if first.clientID != managerTwitchClientID || first.code != managerDeviceCode || first.at.Sub(began) < adapter.interval {
		t.Fatal("Первый опрос получил чужой код или начался до истечения интервала Twitch")
	}
	// firstReply задаёт нижнюю границу интервала до второго запроса.
	firstReply := time.Now()
	first.reply <- twitch.DevicePoll{State: twitch.DevicePending}
	// second содержит второе обращение при незавершённом подтверждении.
	second := chatTestReceive(t, ctx, adapter.polls)
	if second.at.Sub(firstReply) < adapter.interval {
		t.Fatal("authorization_pending сократил задержку следующего запроса")
	}
	// before проверяет, что незавершённая попытка не создала долговечную запись.
	before, err := service.ListChatAccounts(ctx, core.ChatAccountQuery{Provider: core.ChatTwitch, Limit: 10})
	chatTestOK(t, err)
	if len(before) != 0 {
		t.Fatal("Аккаунт появился до успешного подтверждения")
	}
	// bundle моделирует подтверждённую пару одного Twitch-аккаунта.
	bundle := credentialTestBundle("first-twitch-pair")
	second.reply <- twitch.DevicePoll{State: twitch.DeviceComplete, Bundle: &bundle}
	// succeeded отражает результат записи и больше не показывает код.
	succeeded := waitManagerTwitchAuthorization(t, ctx, manager, pending.AttemptID, core.ChatAuthorizationSucceeded)
	assertManagerTwitchPublic(t, succeeded, false)
	// saved читает полную квитанцию из SQLite через отдельный серверный порт.
	saved, err := service.GetChatCredentialResult(ctx, succeeded.AccountID)
	chatTestOK(t, err)
	if saved.Account.Name != "Тестовый Twitch" || saved.Account.Revision != 1 ||
		saved.Credentials.CredentialRevision != 1 || saved.Credentials.CredentialWriteID != pending.AttemptID ||
		saved.Credentials.Bundle.Twitch.AccessToken != bundle.Twitch.AccessToken ||
		saved.Credentials.Bundle.Twitch.RefreshToken != bundle.Twitch.RefreshToken {
		t.Fatal("SQLite не получила полный набор с обеими начальными ревизиями")
	}
	assertManagerTwitchPublic(t, saved.Account, false)
	// err отделяет запрет отмены совершённой операции от допустимой отмены незавершённой.
	if err := manager.CancelAuthorization(ctx, pending.AttemptID); !errors.Is(err, core.ErrConflict) {
		t.Errorf("Отмена успешного сохранения вернула %v", err)
	}
}

// TestManagerTwitchCancelsReplacementAndDiscardsRAMOnRestart проверяет единственную активную
// замену, отмену до записи и отсутствие восстановления попытки при создании нового Manager.
func TestManagerTwitchCancelsReplacementAndDiscardsRAMOnRestart(t *testing.T) {
	// ctx и cancel ограничивают обмен, отмену и запуск нового Manager в том же хранилище.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// store и service сохраняют исходный аккаунт независимо от срока жизни реестра.
	store, service := openChatTestStore(t, ctx)
	// original содержит квитанцию до начала замены.
	original := createCredentialTestAccount(t, ctx, service, "original")
	// adapter возвращает позднюю успешно проверенную пару даже после подтверждённой отмены запроса.
	adapter := &managerTwitchDeviceFixture{
		ctx: ctx, interval: time.Millisecond, ttl: time.Minute,
		polls: make(chan managerTwitchPoll, 2),
	}
	adapter.allowLateReply.Store(true)
	// limits оставляет всего один сетевой слот и одну запись авторизации.
	limits := core.DefaultChatLimits()
	limits.Authorizations = 1
	limits.HTTPRequests = 1
	// manager использует реальный CredentialOwner для подтверждённой ручной замены.
	manager := startManagerTwitchAuthorizationFixture(t, ctx, service, store, limits, adapter)
	// request указывает существующий аккаунт и его начальную конфигурационную ревизию.
	request := core.ChatAuthorizationRequest{
		Provider: core.ChatTwitch, ClientID: managerTwitchClientID, AccountID: original.Account.ID,
		ExpectedRevision: original.Account.Revision,
	}
	// attempt содержит открытое подтверждение, err сообщает об ошибках запуска или чтения.
	attempt, err := manager.ConnectTwitch(ctx, request)
	chatTestOK(t, err)
	// pastPending показывает, что прежний доступ остаётся в SQLite до принятия проверенного решения.
	pastPending, err := service.GetChatCredentialResult(ctx, original.Account.ID)
	chatTestOK(t, err)
	if attempt.State != core.ChatAuthorizationPending || !pastPending.Matches(original) {
		t.Fatal("Новая попытка успела изменить действующий аккаунт")
	}
	// poll удерживает запрос, чтобы проверить попытку параллельной замены и отмену.
	poll := chatTestReceive(t, ctx, adapter.polls)
	if _, err = manager.ConnectTwitch(ctx, request); !errors.Is(err, core.ErrBusy) || adapter.starts.Load() != 1 {
		t.Fatalf("Вторая замена дошла до выдачи нового кода или не получила ErrBusy, ошибка %v", err)
	}
	chatTestOK(t, manager.CancelAuthorization(ctx, attempt.AttemptID))
	chatTestOK(t, manager.CancelAuthorization(ctx, attempt.AttemptID))
	// canceled содержит подтверждённое состояние отмены до принятия результата платформы.
	canceled := waitManagerTwitchAuthorization(t, ctx, manager, attempt.AttemptID, core.ChatAuthorizationCancelled)
	assertManagerTwitchPublic(t, canceled, false)
	chatTestReceive(t, ctx, poll.done)
	// late выдаёт новую пару после подтверждения отмены, чтобы исключить гонку с записью.
	late := credentialTestBundle("late")
	poll.reply <- twitch.DevicePoll{State: twitch.DeviceComplete, Bundle: &late}
	// unfinished доказывает освобождение допуска без остановки Manager и остаётся незавершённой при рестарте.
	unfinished := connectManagerTwitchAfterRelease(t, ctx, manager, request)
	chatTestReceive(t, ctx, adapter.polls)
	// after читается после фактического завершения отменённого опроса, пока Manager продолжает работу.
	after, err := service.GetChatCredentialResult(ctx, original.Account.ID)
	chatTestOK(t, err)
	if !after.Matches(original) {
		t.Fatal("Пара от отменённой замены появилась в SQLite")
	}
	chatTestOK(t, manager.Close(ctx))

	// restart создаёт новый RAM-реестр для того же долговечного хранилища.
	restart := startManagerTwitchAuthorizationFixture(t, ctx, service, store, limits, adapter)
	if _, err = restart.Authorization(ctx, attempt.AttemptID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Новый Manager восстановил старую RAM-попытку, ошибка %v", err)
	}
	if _, err = restart.Authorization(ctx, unfinished.AttemptID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Новый Manager восстановил незавершённую попытку, ошибка %v", err)
	}
	// renewed показывает, что новую авторизацию можно начать после остановки старого реестра.
	renewed, err := restart.ConnectTwitch(ctx, request)
	chatTestOK(t, err)
	// nextPoll получает обмен для нового запуска той же ручной замены.
	nextPoll := chatTestReceive(t, ctx, adapter.polls)
	// replacement содержит принятый владельцем новый набор.
	replacement := credentialTestBundle("replacement")
	nextPoll.reply <- twitch.DevicePoll{State: twitch.DeviceComplete, Bundle: &replacement}
	// succeeded подтверждает запись пары в ту же строку после заново выполненного подтверждения.
	succeeded := waitManagerTwitchAuthorization(t, ctx, restart, renewed.AttemptID, core.ChatAuthorizationSucceeded)
	// current сверяет полный сохранённый результат с ожидаемым CAS.
	current, err := service.GetChatCredentialResult(ctx, original.Account.ID)
	chatTestOK(t, err)
	if succeeded.AccountID != original.Account.ID || current.Account.Revision != original.Account.Revision+1 ||
		current.Credentials.CredentialRevision != original.Credentials.CredentialRevision+1 ||
		current.Credentials.CredentialWriteID != renewed.AttemptID || current.Credentials.Bundle.Twitch.AccessToken != replacement.Twitch.AccessToken ||
		current.Credentials.Bundle.Twitch.RefreshToken != replacement.Twitch.RefreshToken {
		t.Fatal("Подтверждённая ручная замена не сохранила обе ревизии и полную пару в исходном аккаунте")
	}
	// accounts проверяет отсутствие второй долговечной записи после ручного подключения.
	accounts, err := service.ListChatAccounts(ctx, core.ChatAccountQuery{Provider: core.ChatTwitch, Limit: 10})
	chatTestOK(t, err)
	if len(accounts) != 1 {
		t.Fatal("Ручное переподключение создало вторую запись")
	}
	assertManagerTwitchPublic(t, succeeded, false)
}

// TestManagerTwitchExpiresBeforeFirstPoll проверяет меньший срок от платформы
// и отсутствие запроса обмена после истечения кода.
func TestManagerTwitchExpiresBeforeFirstPoll(t *testing.T) {
	// ctx и cancel ограничивают наблюдение за истечением короткого срока, заданного площадкой.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// store и service обеспечивают отдельную мигрированную SQLite.
	store, service := openChatTestStore(t, ctx)
	// adapter выдаёт код, срок которого меньше минимальной паузы перед первым запросом.
	adapter := &managerTwitchDeviceFixture{
		ctx: ctx, interval: time.Second, ttl: 120 * time.Millisecond, polls: make(chan managerTwitchPoll, 1),
	}
	// manager координирует истечение без внешнего обмена и без публикации.
	manager := startManagerTwitchAuthorizationFixture(t, ctx, service, store, core.DefaultChatLimits(), adapter)
	// pending содержит срок, полученный при запуске этого кода.
	pending, err := manager.ConnectTwitch(ctx, core.ChatAuthorizationRequest{Provider: core.ChatTwitch, ClientID: managerTwitchClientID})
	chatTestOK(t, err)
	// expired получает итог истёкшей попытки, когда сеть ещё не должна вызываться.
	expired := waitManagerTwitchAuthorization(t, ctx, manager, pending.AttemptID, core.ChatAuthorizationExpired)
	assertManagerTwitchPublic(t, expired, false)
	chatTestOK(t, manager.Close(ctx))
	if len(adapter.polls) != 0 {
		t.Fatal("Код был предъявлен после истечения срока действия")
	}
	// accounts подтверждает отсутствие учётной записи для истёкшего кода.
	accounts, err := service.ListChatAccounts(ctx, core.ChatAccountQuery{Provider: core.ChatTwitch, Limit: 10})
	chatTestOK(t, err)
	if len(accounts) != 0 {
		t.Fatal("Истёкшая авторизация создала аккаунт")
	}
}
