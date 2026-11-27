package chat

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// VKVideoLiveConnectRequest содержит реквизиты приложения и цель проверенного подключения.
type VKVideoLiveConnectRequest struct {
	// ClientID идентифицирует приложение DevAPI.
	ClientID string
	// ClientSecret передаётся адаптеру и сохраняется только вместе с проверенным токеном.
	ClientSecret string
	// Name задаёт необязательное имя при создании аккаунта.
	Name string
	// AccountID задаёт существующий аккаунт при замене полномочий.
	AccountID core.ID
	// ExpectedRevision защищает замену по конфигурационной ревизии аккаунта.
	ExpectedRevision uint64
}

// vkVideoLiveCredentialVerifier получает подтверждённый набор через зарегистрированный адаптер VK Видео Live.
type vkVideoLiveCredentialVerifier interface {
	// VerifyCredentials обменивает реквизиты приложения и проверяет тип и срок выданного токена.
	VerifyCredentials(ctx context.Context, clientID, clientSecret string) (core.VKVideoLiveChatCredentials, error)
}

// ConnectVKVideoLive проверяет реквизиты приложения перед созданием аккаунта или заменой его полномочий.
// Сетевая проверка не удерживает транзакцию или конфигурационный барьер. При успехе метод возвращает
// публичные метаданные подтверждённой записи. При замене прежние читатели отзываются до возврата.
func (m *Manager) ConnectVKVideoLive(ctx context.Context, request VKVideoLiveConnectRequest) (core.ChatAccount, core.ChatReason, error) {
	// authorization передаёт метаданные и реквизиты в общие стадии подключения.
	authorization := core.ChatAuthorizationRequest{
		Provider: core.ChatVKVideoLive, AccountID: request.AccountID, ExpectedRevision: request.ExpectedRevision,
		Name: request.Name, ClientID: request.ClientID, ClientSecret: request.ClientSecret,
	}
	if !validAuthorizationTarget(authorization) {
		return core.ChatAccount{}, core.ChatReasonConfiguration, core.ErrInvalid
	}
	if !validVKVideoLiveAuthorization(request.ClientID, request.ClientSecret) {
		return core.ChatAccount{}, core.ChatReasonCredentials, core.ErrInvalid
	}
	// verifier проверяет полномочия тем же адаптером, который затем читает чат. ok подтверждает поддержку обмена.
	verifier, ok := m.adapters[core.ChatVKVideoLive].(vkVideoLiveCredentialVerifier)
	if !ok {
		return core.ChatAccount{}, core.ChatReasonConfiguration, core.ErrNotReady
	}
	// owner применяет проверенную замену под тем же допуском, которым пользуются читатели аккаунта.
	owner, _ := m.credentials.(*CredentialOwner)
	if request.AccountID != "" && owner == nil {
		return core.ChatAccount{}, core.ChatReasonConfiguration, core.ErrNotReady
	}
	// operation учитывает срок запроса и остановку Manager. finish освобождает учёт, err сообщает об отказе или отмене.
	operation, finish, err := m.beginOperation(ctx)
	if err != nil {
		return core.ChatAccount{}, core.ChatReasonNone, err
	}
	defer finish()
	if err = m.checkAuthorizationTarget(operation, authorization); err != nil {
		if errors.Is(err, core.ErrConflict) {
			return core.ChatAccount{}, core.ChatReasonConfiguration, err
		}
		return core.ChatAccount{}, core.ChatReasonNone, err
	}
	// writeID связывает запись с квитанцией, release освобождает допуск замены, err сообщает об отказе.
	writeID, release, err := m.reserveSynchronousAuthorization(authorization)
	if err != nil {
		return core.ChatAccount{}, vkVideoLiveConnectReason(err), err
	}
	defer release()
	// bundle содержит проверенные реквизиты и app-токен. err исключает запись при неудачном обмене.
	bundle, err := m.verifyVKVideoLive(operation, verifier, request.ClientID, request.ClientSecret)
	if err != nil {
		return core.ChatAccount{}, vkVideoLiveConnectReason(err), err
	}
	if err = operation.Err(); err != nil {
		return core.ChatAccount{}, core.ChatReasonNone, err
	}
	// account содержит публичную конфигурацию подтверждённой записи, err передаёт её исход.
	account, err := m.persistAuthorization(operation, authorization, owner, *bundle, writeID)
	return account, core.ChatReasonNone, err
}

// validVKVideoLiveAuthorization проверяет исходные реквизиты приложения до обращения к DevAPI.
// Двоеточие в Client ID неоднозначно в имени пользователя HTTP Basic.
func validVKVideoLiveAuthorization(clientID, clientSecret string) bool {
	return validDiscoveryID(clientID) && !strings.ContainsRune(clientID, ':') &&
		len(clientSecret) <= core.ChatCredentialBytes && utf8.ValidString(clientSecret) &&
		!strings.ContainsRune(clientSecret, 0) && strings.TrimSpace(clientSecret) != ""
}

// verifyVKVideoLive ограничивает ручную проверку общим HTTP-бюджетом и сроком конечного запроса.
// При заполнении бюджета сразу возвращает ErrBusy. Сетевой слот освобождается до вызова записи.
func (m *Manager) verifyVKVideoLive(ctx context.Context, verifier vkVideoLiveCredentialVerifier, clientID, clientSecret string) (*core.ChatCredentialBundle, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case m.discoverSlots <- struct{}{}:
	default:
		return nil, core.ErrBusy
	}
	// Функция возвращает место после завершения проверки также при отмене или ошибке.
	defer func() { <-m.discoverSlots }()
	// bounded ограничивает сетевую проверку, cancel освобождает её таймер.
	bounded, cancel := context.WithTimeout(ctx, m.limits.HTTPTimeout)
	defer cancel()
	return verifiedVKVideoLiveBundle(bounded, verifier, clientID, clientSecret)
}

// vkVideoLiveConnectReason отображает безопасные отказы проверки в причину результата подключения.
func vkVideoLiveConnectReason(err error) core.ChatReason {
	switch err {
	case context.Canceled, core.ErrClosed:
		return core.ChatReasonNone
	case core.ErrBusy:
		return core.ChatReasonLimit
	}
	// reason сохраняет подтверждённый отказ реквизитов, временное ограничение площадки или отказ сети.
	_, reason := credentialStatus(err)
	return reason
}

// vkVideoLiveCredentialRefresh возвращает обмен для CredentialOwner. Владелец решает, когда
// он нужен, а функция ждёт общий сетевой бюджет под конечным сроком и возвращает результат для CAS.
func vkVideoLiveCredentialRefresh(adapter core.ChatAdapterPort, slots chan struct{}, timeout time.Duration) CredentialRefreshFunc {
	// verifier предоставляет обмен и проверку app-токена. ok отмечает доступность этого порта.
	verifier, ok := adapter.(vkVideoLiveCredentialVerifier)
	if !ok {
		return nil
	}
	// Функция выполняет запросы после допуска владельца аккаунта и освобождает слот перед сохранением.
	return func(ctx context.Context, current core.ChatCredentials) (CredentialRefreshResult, error) {
		// bounded ограничивает ожидание места и обмен, cancel освобождает таймер.
		bounded, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		// release возвращает место, err сообщает об отмене при ожидании общего бюджета.
		release, err := acquireChatSlot(bounded, slots)
		if err != nil {
			return CredentialRefreshResult{}, err
		}
		defer release()
		// app содержит реквизиты ранее подтверждённого приложения.
		app := current.Bundle.VKVideoLive
		// bundle содержит новый полный набор, err сообщает о неподтверждённом обмене.
		bundle, err := verifiedVKVideoLiveBundle(bounded, verifier, app.ClientID, app.ClientSecret)
		return CredentialRefreshResult{Bundle: bundle}, err
	}
}

// verifiedVKVideoLiveBundle получает полный подтверждённый набор для исходной пары реквизитов.
// Неполный ответ, другая пара или истёкший токен дают временный отказ без записи.
func verifiedVKVideoLiveBundle(ctx context.Context, verifier vkVideoLiveCredentialVerifier, clientID, clientSecret string) (*core.ChatCredentialBundle, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// app содержит выданный приложению токен, err сообщает результат сетевой проверки.
	app, err := verifier.VerifyCredentials(ctx, clientID, clientSecret)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, safeCredentialExchangeError(err)
	}
	// bundle связывает результат DevAPI с сохраняемым провайдером и проходит общий доменный бюджет.
	bundle := &core.ChatCredentialBundle{VKVideoLive: &app}
	if app.ClientID != clientID || app.ClientSecret != clientSecret || bundle.Validate(core.ChatVKVideoLive) != nil || !app.ExpiresAt.After(time.Now()) {
		return nil, core.ErrNotReady
	}
	return bundle, nil
}
