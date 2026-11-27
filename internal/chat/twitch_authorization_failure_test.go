package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/chat/twitch"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestManagerTwitchRejectsUnconfirmedDeviceAuthorization проверяет, что каждый
// терминальный отказ виден пользователю, скрывает данные подтверждения и оставляет SQLite без аккаунтов.
// Подтверждённый повторный вход в том же Manager проверяет освобождение допуска.
func TestManagerTwitchRejectsUnconfirmedDeviceAuthorization(t *testing.T) {
	// foreignBundle содержит структурно допустимые токены другого публичного приложения.
	foreignBundle := credentialTestBundle("foreign-device-pair")
	foreignBundle.Twitch.ClientID = "other-fixture-client"
	// scenarios различает отказ пользователя, истечение кода у Twitch и чужую пару.
	scenarios := []struct {
		// name объясняет, почему попытку нельзя завершить сохранением.
		name string
		// reply передаёт решение Twitch или результат проверки выданной пары.
		reply twitch.DevicePoll
		// state и reason задают публичный итог проверки.
		state  core.ChatAuthorizationState
		reason core.ChatReason
	}{
		{"отказ пользователя", twitch.DevicePoll{State: twitch.DeviceDenied}, core.ChatAuthorizationFailed, core.ChatReasonCredentials},
		{"истечение кода у Twitch", twitch.DevicePoll{State: twitch.DeviceExpired}, core.ChatAuthorizationExpired, core.ChatReasonNone},
		{"пара другого приложения", twitch.DevicePoll{State: twitch.DeviceComplete, Bundle: &foreignBundle}, core.ChatAuthorizationFailed, core.ChatReasonCredentials},
	}
	// scenario задаёт один ответ платформы, после которого пользователь начинает новый вход.
	for _, scenario := range scenarios {
		// Подтест проверяет весь переход от ожидания до отказа и подтверждённого повторного входа.
		t.Run(scenario.name, func(t *testing.T) {
			// ctx ограничивает ожидание ответов, cancel прерывает работу при выходе из подтеста.
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// store и service обслуживают временную SQLite без ранее сохранённых аккаунтов.
			store, service := openChatTestStore(t, ctx)
			// adapter публикует запрос опроса, прежде чем подтест передаст решение Twitch.
			adapter := &managerTwitchDeviceFixture{
				ctx: ctx, interval: time.Millisecond, ttl: time.Minute, polls: make(chan managerTwitchPoll, 1),
			}
			// limits оставляет по одному месту для авторизации и HTTP-запроса, чтобы проверить их повторное использование.
			limits := core.DefaultChatLimits()
			limits.Authorizations = 1
			limits.HTTPRequests = 1
			// manager ведёт попытки для обоих входов и сохраняет только подтверждённый результат.
			manager := startManagerTwitchAuthorizationFixture(t, ctx, service, store, limits, adapter)
			// request начинает подключение нового аккаунта через публичное приложение Twitch.
			request := core.ChatAuthorizationRequest{Provider: core.ChatTwitch, ClientID: managerTwitchClientID}
			// attempt содержит идентификатор фоновой попытки, err сообщает об отказе запуска или чтения.
			attempt, err := manager.Connect(ctx, request)
			chatTestOK(t, err)
			// poll ждёт решения платформы, пока сервер показывает пользователю код подтверждения.
			poll := chatTestReceive(t, ctx, adapter.polls)
			// pending подтверждает, что код уже был опубликован до проверяемого отказа.
			pending, err := manager.Authorization(ctx, attempt.AttemptID)
			chatTestOK(t, err)
			if pending.State != core.ChatAuthorizationPending || !pending.Cancelable ||
				pending.UserCode != managerUserCode || pending.VerificationURI != managerActivationURL {
				t.Fatalf("Ожидающая попытка не содержит данных подтверждения: %+v", pending)
			}
			poll.reply <- scenario.reply
			// terminal показывает состояние и причину после обработки ответа Twitch.
			terminal := waitManagerTwitchAuthorization(t, ctx, manager, attempt.AttemptID, scenario.state)
			if terminal.Reason != scenario.reason || terminal.Cancelable || terminal.AccountID != "" ||
				terminal.UserCode != "" || terminal.VerificationURI != "" {
				t.Fatalf("Получен неверный итог авторизации %+v, ожидаемая причина %q", terminal, scenario.reason)
			}
			assertManagerTwitchPublic(t, terminal, false)
			// retry получает отдельную попытку после освобождения единственного места в реестре.
			retry := connectManagerTwitchAfterRelease(t, ctx, manager, request)
			if retry.AttemptID == attempt.AttemptID || retry.State != core.ChatAuthorizationPending {
				t.Fatalf("Повторный вход не открыл новую попытку: %+v", retry)
			}
			// confirmation ждёт ответа на опрос, уже допущенный после завершения первой попытки.
			confirmation := chatTestReceive(t, ctx, adapter.polls)
			// accounts показывает состояние SQLite после отказа и до подтверждения повторного входа.
			accounts, err := service.ListChatAccounts(ctx, core.ChatAccountQuery{Limit: 10})
			chatTestOK(t, err)
			if len(accounts) != 0 {
				t.Fatal("Отказ Twitch оставил сохранённый аккаунт")
			}
			// bundle содержит проверенную пару токенов для повторного входа.
			bundle := credentialTestBundle("confirmed-device-pair")
			confirmation.reply <- twitch.DevicePoll{State: twitch.DeviceComplete, Bundle: &bundle}
			// completed содержит ID созданного аккаунта, saved сверяет токены и квитанцию в SQLite.
			completed := waitManagerTwitchAuthorization(t, ctx, manager, retry.AttemptID, core.ChatAuthorizationSucceeded)
			saved, err := service.GetChatCredentialResult(ctx, completed.AccountID)
			chatTestOK(t, err)
			if saved.Credentials.CredentialWriteID != retry.AttemptID ||
				saved.Credentials.Bundle.Twitch.AccessToken != bundle.Twitch.AccessToken ||
				saved.Credentials.Bundle.Twitch.RefreshToken != bundle.Twitch.RefreshToken {
				t.Fatal("Повторный вход не сохранил выданную ему пару токенов")
			}
		})
	}
}

// twitchDeviceBeginFailureFixture моделирует отказ выдачи кода для отдельного
// Client ID. Остальные вызовы обслуживает штатная фикстура опроса.
type twitchDeviceBeginFailureFixture struct {
	// managerTwitchDeviceFixture выдаёт корректный код для контрольного подключения.
	*managerTwitchDeviceFixture
	// beginErr передаёт причину отказа либо nil для сценария неполного ответа.
	beginErr error
}

// BeginDevice возвращает результат обычной фикстуры для managerTwitchClientID.
// Для другого clientID возвращает ошибку выдачи либо ответ без кода. ctx
// ограничивает штатную выдачу кода в контрольном сценарии.
func (f *twitchDeviceBeginFailureFixture) BeginDevice(ctx context.Context, clientID string) (twitch.DeviceChallenge, error) {
	if clientID == managerTwitchClientID {
		return f.managerTwitchDeviceFixture.BeginDevice(ctx, clientID)
	}
	return twitch.DeviceChallenge{}, f.beginErr
}

// TestManagerTwitchRejectsMissingDeviceChallenge проверяет отказ до опроса
// при недоступности Twitch и ответе без подтверждения. Синхронный вход
// возвращает ошибку, асинхронный публикует безопасную сетевую причину.
func TestManagerTwitchRejectsMissingDeviceChallenge(t *testing.T) {
	// scenarios охватывает две причины, по которым нельзя опубликовать код.
	scenarios := []struct {
		// name описывает ответ на запрос выдачи кода.
		name string
		// beginErr задаёт отказ адаптера, nil оставляет проверку неполного ответа Manager.
		beginErr error
	}{
		{"выдача кода недоступна", core.ErrNotReady},
		{"ответ без кода", nil},
	}
	// scenario определяет один отказ выдачи кода для обоих методов подключения.
	for _, scenario := range scenarios {
		// Подтест проверяет отсутствие записи и возможность занять тот же слот после отказа.
		t.Run(scenario.name, func(t *testing.T) {
			// ctx ограничивает начало и наблюдение попытки, cancel отзывает фоновые запросы при выходе.
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// store и service обслуживают временную SQLite для обоих методов входа.
			store, service := openChatTestStore(t, ctx)
			// adapter отклоняет первый Client ID и передаёт опрос контрольной попытки подтесту.
			adapter := &twitchDeviceBeginFailureFixture{
				managerTwitchDeviceFixture: &managerTwitchDeviceFixture{
					ctx: ctx, interval: time.Millisecond, ttl: time.Minute, polls: make(chan managerTwitchPoll, 1),
				},
				beginErr: scenario.beginErr,
			}
			// limits выделяет единственное место в реестре и один сетевой слот.
			limits := core.DefaultChatLimits()
			limits.Authorizations = 1
			limits.HTTPRequests = 1
			// manager обрабатывает оба способа запуска, err сообщает об ошибке создания.
			manager, err := New(ctx, service, store, limits, []core.ChatAdapterPort{adapter})
			chatTestOK(t, err)
			// Callback завершает Manager до закрытия SQLite, даже если подтест прервался.
			t.Cleanup(func() {
				// shutdown ограничивает остановку, stop освобождает её таймер.
				shutdown, stop := context.WithTimeout(context.Background(), 3*time.Second)
				defer stop()
				chatTestOK(t, manager.Close(shutdown))
			})
			// request использует синтетический Client ID, для которого фикстура не выдаёт код.
			request := core.ChatAuthorizationRequest{Provider: core.ChatTwitch, ClientID: "unavailable-fixture-client"}
			// synchronous и err проверяют прямой отказ без опубликованной попытки.
			synchronous, err := manager.ConnectTwitch(ctx, request)
			if !errors.Is(err, core.ErrNotReady) || synchronous != (core.ChatAuthorization{}) {
				t.Fatalf("Прямая выдача кода вернула %+v и ошибку %v", synchronous, err)
			}
			// attempt подтверждает, что синхронный отказ освободил место для фоновой проверки.
			attempt, err := manager.Connect(ctx, request)
			chatTestOK(t, err)
			// failed содержит публичный итог, полученный по идентификатору фоновой проверки.
			failed := waitManagerTwitchAuthorization(t, ctx, manager, attempt.AttemptID, core.ChatAuthorizationFailed)
			if failed.Reason != core.ChatReasonNetwork || failed.Cancelable || failed.AccountID != "" ||
				failed.UserCode != "" || failed.VerificationURI != "" {
				t.Fatalf("Отказ выдачи кода вернул неверное публичное состояние: %+v", failed)
			}
			request.ClientID = managerTwitchClientID
			// retry занимает освобождённый слот при исправном ответе платформы.
			retry := connectManagerTwitchAfterRelease(t, ctx, manager, request)
			// poll проверяет, что до этапа опроса дошёл только исправный запрос.
			poll := chatTestReceive(t, ctx, adapter.polls)
			if poll.clientID != managerTwitchClientID || poll.code != managerDeviceCode {
				t.Fatal("Запрос без выданного кода дошёл до опроса Twitch")
			}
			chatTestOK(t, manager.CancelAuthorization(ctx, retry.AttemptID))
			chatTestOK(t, manager.Close(ctx))
			// accounts проверяет долговечное состояние после полного завершения фоновой работы.
			accounts, err := service.ListChatAccounts(ctx, core.ChatAccountQuery{Limit: 10})
			chatTestOK(t, err)
			if len(accounts) != 0 {
				t.Fatal("Отказ выдачи кода оставил сохранённый аккаунт")
			}
		})
	}
}
