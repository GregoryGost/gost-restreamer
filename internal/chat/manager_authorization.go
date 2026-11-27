package chat

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// chatAuthorizationAttempt удерживает допуск до физического завершения проверки или записи.
// После освобождения допуска в реестре остаётся только публичная проекция с ограниченным сроком.
type chatAuthorizationAttempt struct {
	// view содержит публичное состояние под защитой Manager.mu.
	view core.ChatAuthorization
	// cancel прерывает проверку и ожидание подтверждения.
	cancel context.CancelFunc
	// running удерживает место, пока рабочий вызов ещё не завершился.
	running bool
	// uncertain отмечает отсутствие квитанции для текущей или предшествующей записи.
	uncertain bool
	// forgetAt задаёт срок удержания публичной проекции после завершения работы.
	forgetAt time.Time
}

// authorizationWorker выполняет проверку площадки и передаёт проверенный набор на общий допуск записи.
type authorizationWorker func(context.Context, *chatAuthorizationAttempt)

// Connect проверяет форму запроса и целевой аккаунт до допуска.
// После возврата за проверку площадки и сохранение отвечает Manager.
func (m *Manager) Connect(ctx context.Context, request core.ChatAuthorizationRequest) (core.ChatAuthorization, error) {
	if !validAuthorizationTarget(request) || !m.validAuthorizationInput(request) {
		return core.ChatAuthorization{}, core.ErrInvalid
	}
	// owner сериализует замену с другими операциями над полномочиями аккаунта.
	owner, _ := m.credentials.(*CredentialOwner)
	if request.AccountID != "" && owner == nil {
		return core.ChatAuthorization{}, core.ErrNotReady
	}
	// Отдельный набор позволяет вызывающему освободить исходный запрос сразу после возврата.
	request.Bundle = copyCredentialBundle(request.Bundle)
	// worker выполняет проверку выбранной площадки, err сообщает об отсутствии нужного адаптера.
	worker, err := m.authorizationWorker(request, owner)
	if err != nil {
		return core.ChatAuthorization{}, err
	}
	// operation ограничивает проверку цели, finish освобождает учёт Manager, err сообщает об отказе допуска.
	operation, finish, err := m.beginOperation(ctx)
	if err != nil {
		return core.ChatAuthorization{}, err
	}
	defer finish()
	if err = m.checkAuthorizationTarget(operation, request); err != nil {
		return core.ChatAuthorization{}, err
	}
	if err = operation.Err(); err != nil {
		return core.ChatAuthorization{}, err
	}
	// attempt резервирует место в общем реестре, err сообщает об исчерпании бюджета или остановке.
	attempt, err := m.reserveAuthorization(request.Provider, request.AccountID)
	if err != nil {
		return core.ChatAuthorization{}, err
	}
	return m.startAuthorization(operation, attempt, authorizationDeadline(m.limits.AuthorizationTTL), "", "", worker)
}

// reserveSynchronousAuthorization присваивает идентификатор синхронной Go-записи.
// Замена удерживает тот же допуск аккаунта, что и опубликованная попытка.
func (m *Manager) reserveSynchronousAuthorization(request core.ChatAuthorizationRequest) (core.ID, func(), error) {
	if request.AccountID == "" {
		// Callback не освобождает слот, поскольку создание его не резервирует.
		return core.ID(rand.Text()), func() {}, nil
	}
	// attempt резервирует замену, err сообщает об исчерпании мест или остановке Manager.
	attempt, err := m.reserveAuthorization(request.Provider, request.AccountID)
	if err != nil {
		return "", nil, err
	}
	// Callback освобождает неопубликованную запись после физического завершения замены.
	return attempt.view.AttemptID, func() { m.releaseAuthorization(attempt) }, nil
}

// validAuthorizationTarget проверяет метаданные создания либо обязательную пару ID и ревизии для замены.
func validAuthorizationTarget(request core.ChatAuthorizationRequest) bool {
	// account проверяет имя и допустимость платформы до проверки реквизитов.
	account := core.ChatAccount{Provider: request.Provider, Name: request.Name}
	if account.Validate() != nil {
		return false
	}
	if request.AccountID == "" {
		return request.ExpectedRevision == 0
	}
	return validDiscoveryID(string(request.AccountID)) && request.ExpectedRevision != 0 && request.Name == ""
}

// validAuthorizationInput проверяет закрытый набор исходных реквизитов выбранной платформы и его бюджет.
func (m *Manager) validAuthorizationInput(request core.ChatAuthorizationRequest) bool {
	switch request.Provider {
	case core.ChatYouTube:
		return request.ClientID == "" && request.ClientSecret == "" &&
			request.Bundle.ValidateLimit(core.ChatYouTube, m.limits.CredentialBytes) == nil &&
			validAuthorizationToken(request.Bundle.YouTube.APIKey, core.ChatCredentialBytes)
	case core.ChatTwitch:
		return request.ClientSecret == "" && request.Bundle == (core.ChatCredentialBundle{}) &&
			validAuthorizationToken(request.ClientID, core.ChatIDBytes)
	case core.ChatVKVideoLive:
		return request.Bundle == (core.ChatCredentialBundle{}) &&
			validVKVideoLiveAuthorization(request.ClientID, request.ClientSecret) &&
			uint64(len(request.ClientID)+len(request.ClientSecret)) <= m.limits.CredentialBytes
	default:
		return false
	}
}

// validAuthorizationToken проверяет, что исходное значение состоит из видимых ASCII-байтов.
// Такое значение передаётся без изменения через OAuth-форму Twitch или заголовки YouTube.
func validAuthorizationToken(value string, limit int) bool {
	if len(value) == 0 || len(value) > limit {
		return false
	}
	// index указывает на очередной байт проверяемого значения.
	for index := range len(value) {
		if value[index] <= ' ' || value[index] >= 0x7f {
			return false
		}
	}
	return true
}

// authorizationWorker выбирает доступный адаптер до выдачи попытки. Ошибки самой площадки
// переходят в типизированное состояние, которое не раскрывает исходный ответ или реквизиты.
func (m *Manager) authorizationWorker(request core.ChatAuthorizationRequest, owner *CredentialOwner) (authorizationWorker, error) {
	switch request.Provider {
	case core.ChatYouTube:
		// validator подтверждает ключ YouTube без активного эфира, ok показывает поддержку операции.
		validator, ok := m.adapters[core.ChatYouTube].(youtubeKeyValidator)
		if ok {
			// Callback проверяет ключ под сетевым бюджетом, затем допускает запись под отдельным контекстом.
			return func(ctx context.Context, attempt *chatAuthorizationAttempt) {
				// reason содержит безопасный результат проверки ключа, err прекращает попытку до записи.
				reason, err := m.verifyYouTubeAuthorization(ctx, validator, request.Bundle.YouTube.APIKey)
				if err != nil {
					m.failAuthorization(ctx, attempt, reason)
					return
				}
				m.commitAuthorization(ctx, attempt, request, owner, request.Bundle)
			}, nil
		}
	case core.ChatVKVideoLive:
		// verifier получает проверенный набор приложения VK Видео Live, ok показывает поддержку обмена.
		verifier, ok := m.adapters[core.ChatVKVideoLive].(vkVideoLiveCredentialVerifier)
		if ok {
			// Callback сохраняет только набор, подтверждённый проверкой реквизитов приложения.
			return func(ctx context.Context, attempt *chatAuthorizationAttempt) {
				// bundle содержит проверенный набор, err определяет безопасную причину отказа.
				bundle, err := m.verifyVKVideoLive(ctx, verifier, request.ClientID, request.ClientSecret)
				if err != nil {
					m.failAuthorization(ctx, attempt, vkVideoLiveConnectReason(err))
					return
				}
				m.commitAuthorization(ctx, attempt, request, owner, *bundle)
			}, nil
		}
	case core.ChatTwitch:
		// authorizer выполняет выдачу и опрос кода Twitch, ok показывает поддержку Device Code.
		authorizer, ok := m.adapters[core.ChatTwitch].(twitchDeviceAuthorizer)
		if ok {
			// Callback получает и публикует данные подтверждения, затем ждёт ответ Twitch.
			return func(ctx context.Context, attempt *chatAuthorizationAttempt) {
				m.runTwitchAuthorization(ctx, attempt, request, owner, authorizer)
			}, nil
		}
	}
	return nil, core.ErrNotReady
}

// checkAuthorizationTarget сверяет тип площадки и ревизию существующего аккаунта до внешней проверки.
func (m *Manager) checkAuthorizationTarget(ctx context.Context, request core.ChatAuthorizationRequest) error {
	if request.AccountID == "" {
		return nil
	}
	// account содержит только метаданные цели, err сообщает об отсутствии аккаунта или отказе чтения.
	account, err := m.service.GetChatAccount(ctx, request.AccountID)
	if err != nil {
		return err
	}
	if account.Provider != request.Provider || account.Revision != request.ExpectedRevision {
		return core.ErrConflict
	}
	return nil
}

// reserveAuthorization удерживает одну физическую попытку замены на аккаунт. Готовые
// проекции вытесняются после истечения retention TTL или при исчерпании общего бюджета.
func (m *Manager) reserveAuthorization(provider core.ChatProvider, accountID core.ID) (*chatAuthorizationAttempt, error) {
	// id служит адресом попытки и будущим идентификатором записи полномочий.
	id := core.ID(rand.Text())
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return nil, core.ErrClosed
	}
	// now задаёт общую границу истечения для обхода, oldest хранит кандидата на вытеснение.
	now := time.Now()
	var oldest *chatAuthorizationAttempt
	// previousID и previous адресуют одну хранимую попытку.
	for previousID, previous := range m.authorizations {
		if previous.running {
			if accountID != "" && previous.view.AccountID == accountID {
				return nil, core.ErrBusy
			}
			continue
		}
		if !now.Before(previous.forgetAt) {
			delete(m.authorizations, previousID)
			continue
		}
		if oldest == nil || previous.forgetAt.Before(oldest.forgetAt) {
			oldest = previous
		}
	}
	if m.authorizations[id] != nil {
		return nil, core.ErrBusy
	}
	if len(m.authorizations) >= m.limits.Authorizations {
		if oldest == nil {
			return nil, core.ErrBusy
		}
		delete(m.authorizations, oldest.view.AttemptID)
	}
	// attempt удерживает допуск до явного освобождения физической работы.
	attempt := &chatAuthorizationAttempt{
		view:    core.ChatAuthorization{AttemptID: id, Provider: provider, AccountID: accountID, State: core.ChatAuthorizationPending},
		running: true,
	}
	m.authorizations[id] = attempt
	return attempt, nil
}

// startAuthorization публикует попытку и передаёт работу Manager. Вызывающий удерживает
// активную операцию Manager до регистрации новой горутины в work.
func (m *Manager) startAuthorization(ctx context.Context, attempt *chatAuthorizationAttempt, expires time.Time, code, verificationURI string, worker authorizationWorker) (core.ChatAuthorization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		delete(m.authorizations, attempt.view.AttemptID)
		return core.ChatAuthorization{}, core.ErrClosed
	}
	// err отменяет передачу работы, если запрос завершился до публикации попытки.
	if err := ctx.Err(); err != nil {
		delete(m.authorizations, attempt.view.AttemptID)
		return core.ChatAuthorization{}, err
	}
	// verification ограничивает только внешнюю проверку, cancel прерывает её при отмене или завершении.
	verification, cancel := context.WithDeadline(m.ctx, expires)
	attempt.cancel = cancel
	attempt.view.ExpiresAt = expires
	attempt.view.Cancelable = true
	attempt.view.UserCode, attempt.view.VerificationURI = code, verificationURI
	// Callback удерживает допуск до физического возврата адаптера или сохранения.
	m.work.Go(func() {
		defer m.releaseAuthorization(attempt)
		worker(verification, attempt)
	})
	return attempt.view, nil
}

// releaseAuthorization освобождает допуск после фактического завершения работы.
// Неопубликованные записи удаляются сразу, опубликованные живут до forgetAt или вытеснения.
func (m *Manager) releaseAuthorization(attempt *chatAuthorizationAttempt) {
	m.mu.Lock()
	defer m.mu.Unlock()
	attempt.running = false
	if attempt.cancel == nil {
		delete(m.authorizations, attempt.view.AttemptID)
		return
	}
	attempt.cancel()
	attempt.cancel = nil
}

// Authorization возвращает публичную проекцию, включая PENDING при неустановленном результате записи.
func (m *Manager) Authorization(ctx context.Context, id core.ID) (core.ChatAuthorization, error) {
	if ctx == nil || !validDiscoveryID(string(id)) {
		return core.ChatAuthorization{}, core.ErrInvalid
	}
	// err передаёт отмену самого запроса чтения.
	if err := ctx.Err(); err != nil {
		return core.ChatAuthorization{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// attempt указывает на опубликованную и ещё удерживаемую запись.
	attempt := m.authorizationLocked(id)
	if attempt == nil {
		return core.ChatAuthorization{}, core.ErrNotFound
	}
	if attempt.uncertain {
		return attempt.view, ErrCredentialCommitUnknown
	}
	return attempt.view, nil
}

// CancelAuthorization идемпотентно отменяет открытую попытку и отзывает её параметры подтверждения.
// После допуска записи отмена не меняет результат даже при истечении исходного срока проверки.
func (m *Manager) CancelAuthorization(ctx context.Context, id core.ID) error {
	if ctx == nil || !validDiscoveryID(string(id)) {
		return core.ErrInvalid
	}
	// err передаёт отмену запроса до изменения состояния попытки.
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// attempt удерживает состояние и флаг допуска к записи под той же блокировкой.
	attempt := m.authorizationLocked(id)
	if attempt == nil {
		return core.ErrNotFound
	}
	if attempt.uncertain {
		return ErrCredentialCommitUnknown
	}
	if attempt.view.State == core.ChatAuthorizationCancelled {
		return nil
	}
	if !attempt.view.Cancelable {
		return core.ErrConflict
	}
	m.finishAuthorizationLocked(attempt, core.ChatAuthorizationCancelled, core.ChatReasonNone)
	return nil
}

// authorizationLocked исключает непубличные и вытесненные записи и применяет срок
// проверки даже тогда, когда адаптер ещё не вышел из отменённой операции.
func (m *Manager) authorizationLocked(id core.ID) *chatAuthorizationAttempt {
	// attempt содержит запись, которая может ещё не быть опубликована.
	attempt := m.authorizations[id]
	if attempt == nil || attempt.view.ExpiresAt.IsZero() {
		return nil
	}
	if !attempt.running && !time.Now().Before(attempt.forgetAt) {
		delete(m.authorizations, id)
		return nil
	}
	m.checkAuthorizationOpenLocked(m.ctx, attempt)
	return attempt
}

// checkAuthorizationOpenLocked закрывает попытку при истечении срока или отмене её владельца.
// После допуска сохранения проверка больше не влияет на ожидающий результат.
func (m *Manager) checkAuthorizationOpenLocked(ctx context.Context, attempt *chatAuthorizationAttempt) bool {
	if !attempt.view.Cancelable {
		return false
	}
	switch {
	case !time.Now().Before(attempt.view.ExpiresAt):
		m.finishAuthorizationLocked(attempt, core.ChatAuthorizationExpired, core.ChatReasonNone)
	case ctx.Err() != nil || m.ctx.Err() != nil:
		m.finishAuthorizationLocked(attempt, core.ChatAuthorizationCancelled, core.ChatReasonNone)
	default:
		return true
	}
	return false
}

// failAuthorization публикует безопасный отказ, если отмена или истечение ещё не закрыли попытку.
func (m *Manager) failAuthorization(ctx context.Context, attempt *chatAuthorizationAttempt, reason core.ChatReason) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.checkAuthorizationOpenLocked(ctx, attempt) {
		if reason == core.ChatReasonNone {
			reason = core.ChatReasonNetwork
		}
		m.finishAuthorizationLocked(attempt, core.ChatAuthorizationFailed, reason)
	}
}

// commitAuthorization атомарно закрывает окно отмены и отбрасывает параметры подтверждения.
// Допущенная запись использует свой ограниченный контекст Manager и полную сверку квитанции.
func (m *Manager) commitAuthorization(ctx context.Context, attempt *chatAuthorizationAttempt, request core.ChatAuthorizationRequest, owner *CredentialOwner, bundle core.ChatCredentialBundle) {
	m.mu.Lock()
	if !m.checkAuthorizationOpenLocked(ctx, attempt) {
		m.mu.Unlock()
		return
	}
	attempt.view.Cancelable = false
	attempt.view.UserCode, attempt.view.VerificationURI = "", ""
	// writeID связывает публичную попытку с её точной квитанцией.
	writeID := attempt.view.AttemptID
	m.mu.Unlock()

	// writeCtx ограничивает уже допущенное сохранение независимо от ожидания подтверждения.
	writeCtx, cancel := context.WithTimeout(m.ctx, m.limits.HTTPTimeout)
	defer cancel()
	// account содержит подтверждённую конфигурацию, err сохраняет различие между отказом и неизвестным исходом.
	account, err := m.persistAuthorization(writeCtx, request, owner, bundle, writeID)

	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case errors.Is(err, ErrCredentialCommitUnknown), errors.Is(err, core.ErrStale):
		attempt.uncertain = true
		attempt.forgetAt = time.Now().Add(m.limits.AuthorizationTTL)
	case err != nil:
		m.finishAuthorizationLocked(attempt, core.ChatAuthorizationFailed, core.ChatReasonConfiguration)
	default:
		attempt.view.AccountID = account.ID
		m.finishAuthorizationLocked(attempt, core.ChatAuthorizationSucceeded, core.ChatReasonNone)
	}
}

// finishAuthorizationLocked публикует завершение попытки и удаляет данные подтверждения.
func (m *Manager) finishAuthorizationLocked(attempt *chatAuthorizationAttempt, state core.ChatAuthorizationState, reason core.ChatReason) {
	attempt.view.State = state
	attempt.view.Reason = reason
	attempt.view.Cancelable = false
	attempt.view.UserCode, attempt.view.VerificationURI = "", ""
	attempt.forgetAt = time.Now().Add(m.limits.AuthorizationTTL)
	attempt.cancel()
}

// persistAuthorization создаёт проверенный аккаунт либо заменяет полномочия через общий
// CredentialOwner. Подтверждённая замена отзывает прежнее чтение до возврата.
func (m *Manager) persistAuthorization(ctx context.Context, request core.ChatAuthorizationRequest, owner *CredentialOwner, bundle core.ChatCredentialBundle, writeID core.ID) (core.ChatAccount, error) {
	if request.AccountID == "" {
		return m.createVerifiedChatAccount(ctx, core.ChatAccount{Provider: request.Provider, Name: request.Name}, bundle, writeID)
	}
	// account содержит подтверждённую конфигурацию, err передаёт итог проверки и CAS.
	_, account, err := owner.replace(ctx, m, request.AccountID, request.Provider, bundle, writeID, request.ExpectedRevision)
	return account, err
}

// authorizationDeadline возвращает срок проверки в точности публичного UTC-представления.
func authorizationDeadline(ttl time.Duration) time.Time {
	return time.Now().Add(ttl).UTC().Truncate(time.Microsecond)
}
