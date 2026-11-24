package chat

import (
	"context"
	"crypto/rand"
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

// twitchAuthorization хранит попытку до освобождения сетевой работы и короткое время после неё.
// Все поля, в том числе публичный снимок, защищены Manager.mu.
type twitchAuthorization struct {
	// view содержит публичные поля подтверждения до начала сохранения или окончания попытки.
	view core.ChatAuthorization
	// cancel останавливает ожидание и запросы к Twitch после запуска опроса.
	cancel context.CancelFunc
	// running удерживает лимит до фактического завершения стартового запроса, опроса или записи.
	running bool
	// committing отмечает допуск к сохранению, после которого нельзя подтвердить отмену.
	committing bool
	// uncertain фиксирует результат записи, который пока нельзя подтвердить по полной квитанции.
	uncertain bool
	// forgetAt ограничивает срок доступа к итоговому публичному состоянию после остановки опроса.
	forgetAt time.Time
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

// ConnectTwitch начинает RAM-попытку входа через публичное приложение Twitch.
// Запрос возвращает параметры подтверждения. Затем Manager опрашивает Twitch независимо от
// срока запроса вызывающего, сохраняя существующий аккаунт до принятия проверенной замены.
func (m *Manager) ConnectTwitch(ctx context.Context, request core.ChatAuthorizationRequest) (core.ChatAuthorization, error) {
	// account содержит только метаданные нового аккаунта и проверяет допустимость имени.
	account := core.ChatAccount{Provider: request.Provider, Name: request.Name}
	if request.Provider != core.ChatTwitch || account.Validate() != nil || request.Bundle != (core.ChatCredentialBundle{}) ||
		!validDiscoveryID(request.ClientID) ||
		request.AccountID == "" && request.ExpectedRevision != 0 ||
		request.AccountID != "" && (!validDiscoveryID(string(request.AccountID)) || request.ExpectedRevision == 0 || request.Name != "") {
		return core.ChatAuthorization{}, core.ErrInvalid
	}
	// authorizer получает и проверяет коды через зарегистрированный адаптер, ok подтверждает доступность операции.
	authorizer, ok := m.adapters[core.ChatTwitch].(twitchDeviceAuthorizer)
	if !ok {
		return core.ChatAuthorization{}, core.ErrNotReady
	}
	// owner применяет ручную замену через ту же сериализацию, которой пользуется читатель.
	owner, _ := m.credentials.(*CredentialOwner)
	if request.AccountID != "" && owner == nil {
		return core.ChatAuthorization{}, core.ErrNotReady
	}
	// operation объединяет срок запроса и остановку Manager. finish подтверждает завершение стартовой работы.
	// err сообщает об отмене до регистрации либо об уже закрытом Manager.
	operation, finish, err := m.beginOperation(ctx)
	if err != nil {
		return core.ChatAuthorization{}, err
	}
	defer finish()

	if request.AccountID != "" {
		// before содержит конфигурацию аккаунта до сетевой операции. err сообщает результат чтения.
		before, err := m.service.GetChatAccount(operation, request.AccountID)
		if err != nil {
			return core.ChatAuthorization{}, err
		}
		if before.Provider != core.ChatTwitch || before.Revision != request.ExpectedRevision {
			return core.ChatAuthorization{}, core.ErrConflict
		}
	}

	// attempt резервирует RAM и допуск к замене до первого запроса Twitch. err сообщает об исчерпании бюджета.
	attempt, err := m.reserveTwitchAuthorization(request.AccountID)
	if err != nil {
		return core.ChatAuthorization{}, err
	}
	// challenge содержит ответ Twitch, err сообщает, была ли получена пригодная для опроса попытка.
	challenge, err := twitchHTTP(operation, m, func(call context.Context) (twitch.DeviceChallenge, error) {
		return authorizer.BeginDevice(call, request.ClientID)
	})
	if err == nil && (challenge.ExpiresIn <= 0 || challenge.Interval <= 0 || challenge.DeviceCode == "" ||
		challenge.UserCode == "" || challenge.VerificationURI == "") {
		err = core.ErrNotReady
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		err = operation.Err()
	}
	if err != nil {
		delete(m.twitchAuthorizations, attempt.view.AttemptID)
		return core.ChatAuthorization{}, err
	}

	attempt.view.UserCode = challenge.UserCode
	attempt.view.VerificationURI = challenge.VerificationURI
	attempt.view.ExpiresAt = time.Now().Add(min(challenge.ExpiresIn, m.limits.AuthorizationTTL)).UTC().Truncate(time.Microsecond)
	// polling наследует срок работы Manager, stop прекращает запросы по истечении срока кода.
	polling, stop := context.WithDeadline(m.ctx, attempt.view.ExpiresAt)
	attempt.cancel = stop
	// Функция опрашивает Twitch с приватным кодом и освобождает зарезервированную попытку при любом исходе.
	m.work.Go(func() {
		m.pollTwitchAuthorization(polling, attempt, request, authorizer, owner, challenge.DeviceCode, challenge.Interval)
	})
	return attempt.view, nil
}

// Authorization возвращает публичное состояние недавней попытки. Неопределённый результат записи
// возвращается как Pending и ErrCredentialCommitUnknown до удаления записи из RAM.
func (m *Manager) Authorization(ctx context.Context, id core.ID) (core.ChatAuthorization, error) {
	if ctx == nil || !validDiscoveryID(string(id)) {
		return core.ChatAuthorization{}, core.ErrInvalid
	}
	// err сообщает об отмене чтения до обращения к реестру.
	if err := ctx.Err(); err != nil {
		return core.ChatAuthorization{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// attempt доступна до истечения срока хранения итогового состояния.
	attempt := m.twitchAuthorizationLocked(id)
	if attempt == nil {
		return core.ChatAuthorization{}, core.ErrNotFound
	}
	if attempt.uncertain {
		return attempt.view, ErrCredentialCommitUnknown
	}
	return attempt.view, nil
}

// CancelAuthorization останавливает опрос Twitch до допуска к сохранению. При уже начатой
// записи вызывающий может только узнать подтверждённый итог через Authorization.
func (m *Manager) CancelAuthorization(ctx context.Context, id core.ID) error {
	if ctx == nil || !validDiscoveryID(string(id)) {
		return core.ErrInvalid
	}
	// err сообщает об отмене запроса вызывающим до изменения попытки.
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// attempt содержит текущее состояние и возможность остановить опрос.
	attempt := m.twitchAuthorizationLocked(id)
	if attempt == nil {
		return core.ErrNotFound
	}
	if attempt.uncertain {
		return ErrCredentialCommitUnknown
	}
	if attempt.view.State == core.ChatAuthorizationCancelled {
		return nil
	}
	if attempt.committing || attempt.view.State != core.ChatAuthorizationPending {
		return core.ErrConflict
	}
	m.finishTwitchAuthorizationLocked(attempt, core.ChatAuthorizationCancelled, core.ChatReasonNone)
	return nil
}

// reserveTwitchAuthorization выделяет одну активную замену для аккаунта и ограничивает общий
// размер реестра. При полном реестре удаляется самая старая завершённая попытка.
func (m *Manager) reserveTwitchAuthorization(accountID core.ID) (*twitchAuthorization, error) {
	// id адресует попытку и служит WriteID, чтобы запись и её сверка ссылались на один результат.
	id := core.ID(rand.Text())
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return nil, core.ErrClosed
	}
	// now определяет, какие завершённые попытки больше не нужно удерживать.
	now := time.Now()
	// oldest указывает на результат, которым можно освободить место под новую попытку.
	var oldest *twitchAuthorization
	// previousID адресует хранимую запись. previous сохраняет допуск до завершения её работы.
	for previousID, previous := range m.twitchAuthorizations {
		if previous.running {
			if accountID != "" && previous.view.AccountID == accountID {
				return nil, core.ErrBusy
			}
			continue
		}
		if !now.Before(previous.forgetAt) {
			delete(m.twitchAuthorizations, previousID)
			continue
		}
		if oldest == nil || previous.forgetAt.Before(oldest.forgetAt) {
			oldest = previous
		}
	}
	if m.twitchAuthorizations[id] != nil {
		return nil, core.ErrBusy
	}
	if len(m.twitchAuthorizations) >= m.limits.Authorizations {
		if oldest == nil {
			return nil, core.ErrBusy
		}
		delete(m.twitchAuthorizations, oldest.view.AttemptID)
	}
	// attempt удерживает место ещё до получения кода подтверждения от Twitch.
	attempt := &twitchAuthorization{
		view:    core.ChatAuthorization{AttemptID: id, AccountID: accountID, State: core.ChatAuthorizationPending},
		running: true,
	}
	m.twitchAuthorizations[id] = attempt
	return attempt, nil
}

// twitchAuthorizationLocked возвращает публично наблюдаемую попытку под Manager.mu.
// Истёкший код удаляется из публичного состояния даже при ещё работающем HTTP-запросе.
func (m *Manager) twitchAuthorizationLocked(id core.ID) *twitchAuthorization {
	// attempt принадлежит этому экземпляру Manager. До ответа Begin незачем открывать её параметры.
	attempt := m.twitchAuthorizations[id]
	if attempt == nil || attempt.view.ExpiresAt.IsZero() {
		return nil
	}
	// now сравнивается с более строгим сроком подтверждения кода либо со сроком хранения результата.
	now := time.Now()
	if !attempt.running && !now.Before(attempt.forgetAt) {
		delete(m.twitchAuthorizations, id)
		return nil
	}
	if attempt.running && !attempt.committing && !attempt.uncertain && attempt.view.State == core.ChatAuthorizationPending && !now.Before(attempt.view.ExpiresAt) {
		m.finishTwitchAuthorizationLocked(attempt, core.ChatAuthorizationExpired, core.ChatReasonNone)
	}
	return attempt
}

// pollTwitchAuthorization опрашивает Twitch до окончания подтверждения и сохраняет полученную
// пару один раз. Между запросами общий сетевой допуск свободен для других источников.
func (m *Manager) pollTwitchAuthorization(ctx context.Context, attempt *twitchAuthorization, request core.ChatAuthorizationRequest, authorizer twitchDeviceAuthorizer, owner *CredentialOwner, deviceCode string, interval time.Duration) {
	// Функция освобождает слот только после фактического выхода из опроса или записи.
	defer func() {
		m.mu.Lock()
		attempt.running = false
		attempt.cancel()
		m.mu.Unlock()
	}()

	// wait учитывает минимальный интервал и задержку из последнего ответа Twitch.
	wait := interval
	for waitTwitchPoll(ctx, wait) {
		// poll описывает исход обмена. err передаёт причину отказа или временное ограничение сети.
		poll, err := twitchHTTP(ctx, m, func(call context.Context) (twitch.DevicePoll, error) {
			return authorizer.PollDevice(call, request.ClientID, deviceCode)
		})
		if ctx.Err() != nil {
			break
		}
		switch poll.State {
		case twitch.DeviceComplete:
			if err == nil && poll.Bundle != nil && poll.Bundle.Validate(core.ChatTwitch) == nil &&
				poll.Bundle.Twitch.ClientID == request.ClientID {
				m.commitTwitchAuthorization(ctx, attempt, request, owner, *poll.Bundle)
			} else {
				m.finishTwitchAuthorization(attempt, core.ChatAuthorizationFailed, core.ChatReasonCredentials)
			}
			return
		case twitch.DeviceExpired:
			m.finishTwitchAuthorization(attempt, core.ChatAuthorizationExpired, core.ChatReasonNone)
			return
		case twitch.DeviceDenied:
			// reason отличает подтверждённый отказ от потери возможности проверить выданную пару.
			reason := core.ChatReasonCredentials
			if errors.Is(err, core.ErrNotReady) || errors.Is(err, context.DeadlineExceeded) {
				reason = core.ChatReasonNetwork
			}
			m.finishTwitchAuthorization(attempt, core.ChatAuthorizationFailed, reason)
			return
		case twitch.DeviceSlowDown:
			// Увеличение продолжается до конца этой попытки. Большой срок ограничивается её TTL.
			interval = min(interval, math.MaxInt64-5*time.Second) + 5*time.Second
		case twitch.DevicePending:
			// Twitch ожидает действий пользователя, поэтому до следующего запроса действует тот же интервал.
		default:
			if !errors.Is(err, core.ErrNotReady) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, core.ErrBusy) {
				m.finishTwitchAuthorization(attempt, core.ChatAuthorizationFailed, core.ChatReasonCredentials)
				return
			}
			if errors.Is(err, context.DeadlineExceeded) {
				// При таймауте RFC 8628 требует уменьшить частоту до следующего запроса.
				interval += min(interval, math.MaxInt64-interval)
			}
		}
		wait = max(interval, poll.RetryAfter)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		m.finishTwitchAuthorization(attempt, core.ChatAuthorizationExpired, core.ChatReasonNone)
	} else {
		m.finishTwitchAuthorization(attempt, core.ChatAuthorizationCancelled, core.ChatReasonNone)
	}
}

// commitTwitchAuthorization допускает запись только из текущего Pending до отмены или истечения.
// После допуска в итог попадает подтверждённое состояние SQLite либо неопределённость записи.
func (m *Manager) commitTwitchAuthorization(ctx context.Context, attempt *twitchAuthorization, request core.ChatAuthorizationRequest, owner *CredentialOwner, bundle core.ChatCredentialBundle) {
	m.mu.Lock()
	if attempt.view.State != core.ChatAuthorizationPending {
		m.mu.Unlock()
		return
	}
	if !time.Now().Before(attempt.view.ExpiresAt) {
		m.finishTwitchAuthorizationLocked(attempt, core.ChatAuthorizationExpired, core.ChatReasonNone)
		m.mu.Unlock()
		return
	}
	if ctx.Err() != nil {
		m.finishTwitchAuthorizationLocked(attempt, core.ChatAuthorizationCancelled, core.ChatReasonNone)
		m.mu.Unlock()
		return
	}
	attempt.committing = true
	attempt.view.UserCode, attempt.view.VerificationURI = "", ""
	// writeID связывает сохранение и имеющуюся сверку с этой же попыткой.
	writeID := attempt.view.AttemptID
	m.mu.Unlock()

	// operation ограничивает уже принятую запись независимо от времени, оставшегося на ввод кода.
	// cancel освобождает её таймер, Manager сохраняет владение операцией до выхода.
	operation, cancel := context.WithTimeout(m.ctx, m.limits.HTTPTimeout)
	defer cancel()
	// account содержит подтверждённую конфигурацию, err отделяет отказ от возможного COMMIT.
	var account core.ChatAccount
	var err error
	if request.AccountID == "" {
		account, err = m.createVerifiedChatAccount(operation, core.ChatAccount{Provider: core.ChatTwitch, Name: request.Name}, bundle, writeID)
	} else {
		_, account, err = owner.replace(operation, m, request.AccountID, core.ChatTwitch, bundle, writeID, request.ExpectedRevision)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	attempt.committing = false
	switch {
	case errors.Is(err, ErrCredentialCommitUnknown), errors.Is(err, core.ErrStale):
		// Действующий набор мог измениться после возможного COMMIT. Отмена и отказ не подтверждены.
		attempt.uncertain = true
		attempt.forgetAt = time.Now().Add(m.limits.AuthorizationTTL)
	case err != nil:
		m.finishTwitchAuthorizationLocked(attempt, core.ChatAuthorizationFailed, core.ChatReasonConfiguration)
	default:
		attempt.view.AccountID = account.ID
		m.finishTwitchAuthorizationLocked(attempt, core.ChatAuthorizationSucceeded, core.ChatReasonNone)
	}
}

// finishTwitchAuthorization публикует подтверждённый исход опроса, если его уже не отменили.
func (m *Manager) finishTwitchAuthorization(attempt *twitchAuthorization, state core.ChatAuthorizationState, reason core.ChatReason) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if attempt.view.State == core.ChatAuthorizationPending {
		m.finishTwitchAuthorizationLocked(attempt, state, reason)
	}
}

// finishTwitchAuthorizationLocked скрывает параметры входа и прекращает опрос под Manager.mu.
func (m *Manager) finishTwitchAuthorizationLocked(attempt *twitchAuthorization, state core.ChatAuthorizationState, reason core.ChatReason) {
	attempt.view.State = state
	attempt.view.Reason = reason
	attempt.view.UserCode, attempt.view.VerificationURI = "", ""
	attempt.forgetAt = time.Now().Add(m.limits.AuthorizationTTL)
	attempt.cancel()
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
	return call(bounded)
}
