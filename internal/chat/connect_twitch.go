package chat

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/chat/twitch"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// twitchDeviceAuthorizer отправляет запросы Device Code и возвращает пару только после проверки в Twitch.
type twitchDeviceAuthorizer interface {
	// BeginDevice получает публичные параметры подтверждения и приватный код запроса для сервера.
	BeginDevice(context.Context, string) (twitch.DeviceChallenge, error)
	// PollDevice проверяет одну попытку и при подтверждении сверяет выданные полномочия.
	PollDevice(context.Context, string, string) (twitch.DevicePoll, error)
}

// twitchRefresher проверяет полномочия в Twitch и при необходимости получает следующую пару токенов.
type twitchRefresher interface {
	// Refresh сообщает, могла ли площадка израсходовать прежний refresh token, даже если ответ потерялся.
	Refresh(context.Context, core.ChatCredentials) (*core.ChatCredentialBundle, bool, error)
}

// twitchCredentialRefresh связывает сетевую проверку с сериализацией CredentialOwner и общим
// бюджетом конечных HTTP-запросов. Другие провайдеры получают текущий набор без обмена.
func twitchCredentialRefresh(adapter core.ChatAdapterPort, slots chan struct{}, timeout time.Duration) CredentialRefreshFunc {
	// refresher предоставляет сетевую проверку. ok означает, что адаптер поддерживает ротацию.
	refresher, ok := adapter.(twitchRefresher)
	if !ok {
		return nil
	}
	// Функция выполняет проверку в пределах допуска аккаунта. После возврата её результат
	// сохраняет CredentialOwner, поэтому SQLite не удерживает транзакцию во время запросов.
	return func(ctx context.Context, current core.ChatCredentials) (CredentialRefreshResult, error) {
		if current.Provider != core.ChatTwitch {
			return CredentialRefreshResult{}, nil
		}
		// operation ограничивает ожидание общего бюджета и конечные запросы. cancel освобождает таймер.
		operation, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		// release возвращает сетевой допуск. err передаёт безопасный отказ ожидания.
		release, err := acquireChatSlot(operation, slots)
		if err != nil {
			return CredentialRefreshResult{}, err
		}
		defer release()
		// bundle содержит новую проверенную пару, consumed отмечает возможный одноразовый обмен.
		// err относится к сетевому результату, до сохранения новой пары в SQLite.
		bundle, consumed, err := refresher.Refresh(operation, current)
		return CredentialRefreshResult{Bundle: bundle, PreviousMayBeConsumed: consumed}, err
	}
}

// ConnectTwitch возвращает проверенные параметры активации в том же вызове.
// После выдачи кода опрос и сохранение принадлежат Manager.
func (m *Manager) ConnectTwitch(ctx context.Context, request core.ChatAuthorizationRequest) (core.ChatAuthorization, error) {
	if request.Provider != core.ChatTwitch || !validAuthorizationTarget(request) || !m.validAuthorizationInput(request) {
		return core.ChatAuthorization{}, core.ErrInvalid
	}
	// authorizer выполняет выдачу и опрос кода Twitch, ok показывает поддержку Device Code.
	authorizer, ok := m.adapters[core.ChatTwitch].(twitchDeviceAuthorizer)
	if !ok {
		return core.ChatAuthorization{}, core.ErrNotReady
	}
	// owner сериализует ручное переподключение с обновлением полномочий.
	owner, _ := m.credentials.(*CredentialOwner)
	if request.AccountID != "" && owner == nil {
		return core.ChatAuthorization{}, core.ErrNotReady
	}
	// operation ограничивает синхронную выдачу кода, finish освобождает учёт Manager, err сообщает об отказе.
	operation, finish, err := m.beginOperation(ctx)
	if err != nil {
		return core.ChatAuthorization{}, err
	}
	defer finish()
	if err = m.checkAuthorizationTarget(operation, request); err != nil {
		return core.ChatAuthorization{}, err
	}
	// attempt резервирует общий допуск, err сообщает об исчерпании бюджета или остановке.
	attempt, err := m.reserveAuthorization(request.Provider, request.AccountID)
	if err != nil {
		return core.ChatAuthorization{}, err
	}
	// challenge содержит проверенные параметры и приватный код, err означает невозможность начать опрос.
	challenge, err := m.beginTwitchDevice(operation, authorizer, request.ClientID)
	if err != nil {
		m.releaseAuthorization(attempt)
		return core.ChatAuthorization{}, err
	}
	// expires ограничивает опрос меньшим сроком Twitch и Manager.
	expires := authorizationDeadline(min(challenge.ExpiresIn, m.limits.AuthorizationTTL))
	// Callback начинает опрос только после публикации положенного пользователю адреса и кода.
	return m.startAuthorization(operation, attempt, expires, challenge.UserCode, challenge.VerificationURI, func(polling context.Context, attempt *chatAuthorizationAttempt) {
		m.pollTwitchAuthorization(polling, attempt, request, owner, authorizer, challenge)
	})
}

// beginTwitchDevice получает код под сетевым бюджетом и повторно проверяет публичные поля
// на границе Manager, где также могут использоваться инъецированные адаптеры.
func (m *Manager) beginTwitchDevice(ctx context.Context, authorizer twitchDeviceAuthorizer, clientID string) (twitch.DeviceChallenge, error) {
	// challenge принимает параметры активации, err сообщает отказ или отмену выдачи кода.
	// Callback передаёт запрос адаптеру только после получения сетевого допуска.
	challenge, err := twitchHTTP(ctx, m, func(call context.Context) (twitch.DeviceChallenge, error) {
		return authorizer.BeginDevice(call, clientID)
	})
	if err != nil {
		return twitch.DeviceChallenge{}, err
	}
	if err = challenge.Validate(); err != nil {
		return twitch.DeviceChallenge{}, err
	}
	return challenge, nil
}

// runTwitchAuthorization получает параметры асинхронно и сокращает оставшееся время
// проверки до срока кода Twitch. Отмена до публикации не раскрывает параметры активации.
func (m *Manager) runTwitchAuthorization(ctx context.Context, attempt *chatAuthorizationAttempt, request core.ChatAuthorizationRequest, owner *CredentialOwner, authorizer twitchDeviceAuthorizer) {
	// challenge содержит проверенные параметры подтверждения, err определяет безопасную причину отказа.
	challenge, err := m.beginTwitchDevice(ctx, authorizer, request.ClientID)
	if err != nil {
		m.failAuthorization(ctx, attempt, twitchAuthorizationReason(err))
		return
	}
	// providerDeadline ограничивает срок, не продлевая уже начатый локальный таймер.
	providerDeadline := authorizationDeadline(challenge.ExpiresIn)
	m.mu.Lock()
	if !m.checkAuthorizationOpenLocked(ctx, attempt) {
		m.mu.Unlock()
		return
	}
	if providerDeadline.Before(attempt.view.ExpiresAt) {
		attempt.view.ExpiresAt = providerDeadline
	}
	attempt.view.UserCode, attempt.view.VerificationURI = challenge.UserCode, challenge.VerificationURI
	// expires сохраняет согласованный публичный срок для создания контекста опроса.
	expires := attempt.view.ExpiresAt
	m.mu.Unlock()

	// polling прекращает запросы при истечении срока выданного кода либо отмене попытки.
	polling, cancel := context.WithDeadline(ctx, expires)
	defer cancel()
	m.pollTwitchAuthorization(polling, attempt, request, owner, authorizer, challenge)
}

// pollTwitchAuthorization соблюдает интервал провайдера, Retry-After, slow_down и
// exponential backoff при сетевом timeout. Одноразовый успешный обмен не повторяется.
func (m *Manager) pollTwitchAuthorization(ctx context.Context, attempt *chatAuthorizationAttempt, request core.ChatAuthorizationRequest, owner *CredentialOwner, authorizer twitchDeviceAuthorizer, challenge twitch.DeviceChallenge) {
	// interval содержит текущую базовую задержку, wait включает одноразовый Retry-After.
	interval := challenge.Interval
	wait := interval
	for waitTwitchPoll(ctx, wait) {
		// poll содержит результат одного обмена, err различает временную ошибку и отказ.
		// Callback обменивает код устройства, остающийся внутри рабочего вызова.
		poll, err := twitchHTTP(ctx, m, func(call context.Context) (twitch.DevicePoll, error) {
			return authorizer.PollDevice(call, request.ClientID, challenge.DeviceCode)
		})
		if ctx.Err() != nil {
			break
		}
		switch poll.State {
		case twitch.DeviceComplete:
			if err == nil && poll.Bundle != nil && poll.Bundle.Validate(core.ChatTwitch) == nil &&
				poll.Bundle.Twitch.ClientID == request.ClientID {
				m.commitAuthorization(ctx, attempt, request, owner, *poll.Bundle)
			} else {
				m.failAuthorization(ctx, attempt, core.ChatReasonCredentials)
			}
			return
		case twitch.DeviceExpired:
			m.mu.Lock()
			if m.checkAuthorizationOpenLocked(ctx, attempt) {
				m.finishAuthorizationLocked(attempt, core.ChatAuthorizationExpired, core.ChatReasonNone)
			}
			m.mu.Unlock()
			return
		case twitch.DeviceDenied:
			m.failAuthorization(ctx, attempt, twitchAuthorizationReason(err))
			return
		case twitch.DeviceSlowDown:
			interval = min(interval, math.MaxInt64-5*time.Second) + 5*time.Second
		case twitch.DevicePending:
		default:
			if !errors.Is(err, core.ErrNotReady) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, core.ErrBusy) {
				m.failAuthorization(ctx, attempt, core.ChatReasonCredentials)
				return
			}
			if errors.Is(err, context.DeadlineExceeded) {
				interval += min(interval, math.MaxInt64-interval)
			}
		}
		wait = max(interval, poll.RetryAfter)
	}
	m.failAuthorization(ctx, attempt, core.ChatReasonNetwork)
}

// twitchAuthorizationReason преобразует отказ или временное ограничение в публичную причину.
func twitchAuthorizationReason(err error) core.ChatReason {
	switch {
	case errors.Is(err, core.ErrBusy):
		return core.ChatReasonLimit
	case errors.Is(err, core.ErrNotReady), errors.Is(err, context.DeadlineExceeded):
		return core.ChatReasonNetwork
	default:
		return core.ChatReasonCredentials
	}
}

// waitTwitchPoll ждёт срока следующего запроса, прекращая ожидание при отмене или истечении кода.
func waitTwitchPoll(ctx context.Context, delay time.Duration) bool {
	// timer позволяет остановить ожидание сразу после отмены попытки.
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

// twitchHTTP выполняет один шаг Device Code с общим сетевым бюджетом Manager. Выполнение
// операции, включая ожидание допуска, ограничено ctx и сроком конечного HTTP-запроса.
func twitchHTTP[T any](ctx context.Context, m *Manager, call func(context.Context) (T, error)) (T, error) {
	// bounded ограничивает конечный запрос и очередь. cancel освобождает его таймер.
	bounded, cancel := context.WithTimeout(ctx, m.limits.HTTPTimeout)
	defer cancel()
	// release возвращает место после завершения запроса. err сообщает, почему запрос не начался.
	release, err := acquireChatSlot(bounded, m.discoverSlots)
	if err != nil {
		// zero возвращается при отказе допуска, когда сетевого результата ещё нет.
		var zero T
		return zero, err
	}
	defer release()
	// result сохраняет состояние ответа, чтобы timeout после одноразового обмена не возобновил опрос.
	// err сообщает об ошибке самого запроса, если срок ещё действует.
	result, err := call(bounded)
	if bounded.Err() != nil {
		return result, bounded.Err()
	}
	return result, err
}
