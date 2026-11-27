package vkvideolive

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// oauthGrant содержит результат выдачи app-токена в обмен на реквизиты приложения.
type oauthGrant struct {
	// AccessToken передаётся в Bearer только сервису DevAPI.
	AccessToken string `json:"access_token"`
	// TokenType подтверждает обязательную схему Bearer для выданного токена.
	TokenType string `json:"token_type"`
	// ExpiresIn содержит необязательное точное число секунд с начала выдачи.
	ExpiresIn json.RawMessage `json:"expires_in"`
	// Error позволяет отклонить формально успешный ответ с оболочкой ошибки.
	Error json.RawMessage `json:"error"`
}

// tokenInfo содержит поля DevAPI, доказывающие тип и абсолютный срок выданного токена.
type tokenInfo struct {
	// Type должен подтверждать полномочия приложения.
	Type string `json:"type"`
	// ExpiredAt содержит абсолютный срок в целых Unix-секундах.
	ExpiredAt int64 `json:"expired_at"`
}

// VerifyCredentials получает app-токен по clientID и clientSecret и проверяет его
// через DevAPI. Только подтверждённый ответ с будущим сроком возвращается для
// сохранения. Подтверждённый отказ реквизитов даёт ErrInvalid, сбой выдачи или
// проверки даёт временный отказ. ctx может отменить всю операцию.
func (a *Adapter) VerifyCredentials(ctx context.Context, clientID, clientSecret string) (core.VKVideoLiveChatCredentials, error) {
	if ctx == nil || !validRequiredText(clientID, core.ChatIDBytes) || strings.Contains(clientID, ":") ||
		!validRequiredText(clientSecret, core.ChatCredentialBytes) || uint64(len(clientID)+len(clientSecret)) > a.limits.CredentialBytes {
		return core.VKVideoLiveChatCredentials{}, core.ErrInvalid
	}
	// operation ограничивает выдачу и проверку одним сроком, cancel освобождает таймер.
	operation, cancel := context.WithTimeout(ctx, a.limits.HTTPTimeout)
	defer cancel()
	// startedAt фиксирует время до выдачи для консервативного отсчёта expires_in.
	startedAt := a.now()
	// grant содержит выданный Bearer, err различает отказ реквизитов, сети и квоты.
	grant, err := a.issueToken(operation, clientID, clientSecret)
	if err != nil {
		return core.VKVideoLiveChatCredentials{}, err
	}
	// lifetime содержит допустимый относительный срок. Ноль соответствует отсутствующему expires_in.
	lifetime, err := grantLifetime(grant.ExpiresIn)
	if err != nil {
		return core.VKVideoLiveChatCredentials{}, err
	}

	// result содержит ответ проверки токена, err сообщает об отмене либо неполном ответе.
	result, err := a.bearerGET(operation, "/v1/token", "", grant.AccessToken)
	if operation.Err() != nil {
		return core.VKVideoLiveChatCredentials{}, operation.Err()
	}
	if err != nil || result.status != http.StatusOK {
		return core.VKVideoLiveChatCredentials{}, a.verificationFailure(result)
	}

	// envelope должен подтвердить, что выданный токен принадлежит приложению.
	var envelope devEnvelope[tokenInfo]
	if json.Unmarshal(result.body, &envelope) != nil || envelope.Data == nil || len(envelope.Error) != 0 || envelope.Data.Type != "application" {
		return core.VKVideoLiveChatCredentials{}, core.ErrNotReady
	}
	// expiresAt хранит проверенный срок в точности ChatCredentialBundle независимо от времени ответа DevAPI.
	expiresAt, err := verifiedExpiry(envelope.Data.ExpiredAt, lifetime, startedAt, a.now())
	if err != nil {
		return core.VKVideoLiveChatCredentials{}, err
	}
	// credentials связывает исходные реквизиты и проверенный результат выдачи для одного сохранения.
	credentials := core.VKVideoLiveChatCredentials{
		ClientID: clientID, ClientSecret: clientSecret, AccessToken: grant.AccessToken, ExpiresAt: expiresAt,
	}
	// bundle позволяет применить тот же закрытый контракт и бюджет, что у CredentialOwner.
	bundle := core.ChatCredentialBundle{VKVideoLive: &credentials}
	if bundle.Validate(core.ChatVKVideoLive) != nil {
		return core.VKVideoLiveChatCredentials{}, core.ErrNotReady
	}
	// encoded учитывает экранирование и служебные поля полного сохраняемого набора. err сообщает об ошибке кодирования.
	encoded, err := json.Marshal(bundle)
	if err != nil || uint64(len(encoded)) > a.limits.CredentialBytes {
		return core.VKVideoLiveChatCredentials{}, core.ErrNotReady
	}
	return credentials, nil
}

// issueToken отправляет clientID и clientSecret только в Basic-заголовке фиксированного
// запроса client_credentials. ctx ограничивает работу временем операции владельца.
func (a *Adapter) issueToken(ctx context.Context, clientID, clientSecret string) (oauthGrant, error) {
	// request описывает POST выдачи. err сообщает об отказе создания запроса.
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.oauthBase+"/oauth/server/token", strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		return oauthGrant{}, core.ErrNotReady
	}
	request.SetBasicAuth(clientID, clientSecret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// result содержит исход HTTP. err сообщает о неполном ответе или отмене.
	result, err := a.readHTTP(request)
	if ctx.Err() != nil {
		return oauthGrant{}, ctx.Err()
	}
	if err != nil || result.status != http.StatusOK {
		// code позволяет отличить подтверждённый отказ OAuth от произвольного HTTP-ответа.
		code := result.errorCode()
		if result.status == http.StatusBadRequest && code == "invalid_client" ||
			result.status == http.StatusUnauthorized && (code == "invalid_client" || code == "unauthorized") {
			return oauthGrant{}, core.ErrInvalid
		}
		return oauthGrant{}, a.verificationFailure(result)
	}
	// grant содержит выданный результат до отдельной проверки типа приложения и срока.
	var grant oauthGrant
	if json.Unmarshal(result.body, &grant) != nil || len(grant.Error) != 0 || !validBearer(grant.AccessToken) ||
		!strings.EqualFold(grant.TokenType, "Bearer") {
		return oauthGrant{}, core.ErrNotReady
	}
	return grant, nil
}

// verificationFailure сохраняет задержку из result и причину квоты для владельца
// аккаунта. Остальные отказы выдачи либо проверки считаются временными.
func (a *Adapter) verificationFailure(result httpResult) error {
	// delay задаёт минимальный срок очередного запроса.
	delay := retryAfter(result.retryAfter, a.now())
	if result.status == http.StatusTooManyRequests {
		return &core.ChatReadRetry{Reason: core.ChatReasonQuota, RetryAfter: delay}
	}
	if delay > 0 {
		return &core.ChatReadRetry{Reason: core.ChatReasonNetwork, RetryAfter: delay}
	}
	return core.ErrNotReady
}

// grantLifetime проверяет точное число секунд в присутствующем expiresIn.
// Отсутствие поля даёт ноль без ошибки, чтобы срок мог определить DevAPI.
func grantLifetime(expiresIn json.RawMessage) (time.Duration, error) {
	if len(expiresIn) == 0 {
		return 0, nil
	}
	// seconds содержит целое число секунд, допустимое в time.Duration.
	var seconds int64
	if json.Unmarshal(expiresIn, &seconds) != nil || seconds <= 0 || seconds > math.MaxInt64/int64(time.Second) {
		return 0, core.ErrNotReady
	}
	return time.Duration(seconds) * time.Second, nil
}

// verifiedExpiry сверяет Unix-секунды absolute и необязательный lifetime.
// Относительный интервал считается от startedAt. Оба срока должны быть
// представимы, а их минимум должен быть в будущем относительно now.
func verifiedExpiry(absolute int64, lifetime time.Duration, startedAt, now time.Time) (time.Time, error) {
	if absolute <= 0 || absolute > math.MaxInt64/1_000_000 {
		return time.Time{}, core.ErrNotReady
	}
	// deadline содержит абсолютный срок, подтверждённый DevAPI.
	deadline := time.Unix(absolute, 0)
	if lifetime != 0 {
		// relativeDeadline ограничивает срок начиная ещё до отправки запроса выдачи.
		relativeDeadline := startedAt.Add(lifetime)
		if relativeDeadline.Before(deadline) {
			deadline = relativeDeadline
		}
	}
	// normalized содержится в UTC с точностью хранения. err сообщает о переполнении.
	normalized, err := core.NormalizeTimestamp(deadline)
	if err != nil || !normalized.After(now) || normalized.IsZero() {
		return time.Time{}, core.ErrNotReady
	}
	return normalized, nil
}
