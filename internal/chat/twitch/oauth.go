package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

const (
	// chatReadScope разрешает чтение чата от имени пользователя.
	chatReadScope = "user:read:chat"
	// deviceGrantType выбирает обмен кода устройства на пользовательскую пару.
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	// deviceEndpoint создаёт короткоживущие коды OAuth.
	deviceEndpoint = "/oauth2/device"
	// tokenEndpoint обменивает код устройства или refresh token.
	tokenEndpoint = "/oauth2/token"
	// validationEndpoint подтверждает приложение, пользователя, срок и полномочия access token.
	validationEndpoint = "/oauth2/validate"
)

// DeviceChallenge содержит данные новой попытки. DeviceCode остаётся на сервере.
type DeviceChallenge struct {
	// DeviceCode передаётся при опросе OAuth и остаётся в памяти владельца попытки.
	DeviceCode string `json:"-"`
	// UserCode показывает пользователю короткий код подтверждения.
	UserCode string
	// VerificationURI ведёт на страницу подтверждения Twitch.
	VerificationURI string
	// ExpiresIn содержит оставшийся срок кода на момент возврата.
	ExpiresIn time.Duration
	// Interval задаёт минимальную задержку перед следующим опросом.
	Interval time.Duration
}

// Validate подтверждает форму данных перед опросом и публикацией. Ссылка содержит только
// разрешённые параметры активации, а код устройства нельзя выдать за публичный код.
func (c DeviceChallenge) Validate() error {
	if c.ExpiresIn <= 0 || c.Interval <= 0 || !validOAuthValue(c.DeviceCode, core.ChatCredentialBytes) ||
		!validOAuthValue(c.UserCode, core.ChatIDBytes) || !validVerificationURI(c.VerificationURI, c.UserCode, c.DeviceCode) {
		return core.ErrNotReady
	}
	return nil
}

// DeviceState показывает, требуется ли ещё опрашивать Twitch для этой попытки.
type DeviceState uint8

const (
	// DevicePending оставляет попытку открытой до подтверждения пользователем.
	DevicePending DeviceState = iota + 1
	// DeviceSlowDown требует увеличить интервал текущей попытки.
	DeviceSlowDown
	// DeviceExpired завершает попытку после истечения либо недействительности кода.
	DeviceExpired
	// DeviceDenied завершает попытку после отказа или непригодности полученной пары.
	DeviceDenied
	// DeviceComplete сообщает о полностью проверенной новой паре.
	DeviceComplete
)

// DevicePoll содержит исход одного опроса и серверный набор только при успехе.
type DevicePoll struct {
	// State сообщает о продолжении или завершении попытки. Ноль означает отсутствие подтверждённого исхода.
	State DeviceState
	// Bundle содержит пару токенов и метаданные, подтверждённые через /validate.
	Bundle *core.ChatCredentialBundle `json:"-"`
	// RetryAfter содержит дополнительную нижнюю границу ожидания от Twitch.
	RetryAfter time.Duration
}

// deviceResponse читает поля выдачи короткоживущих кодов.
type deviceResponse struct {
	// DeviceCode содержит непрозрачный секрет для последующего обмена.
	DeviceCode string `json:"device_code"`
	// UserCode содержит короткий публичный код.
	UserCode string `json:"user_code"`
	// VerificationURI содержит предложенный Twitch адрес подтверждения.
	VerificationURI string `json:"verification_uri"`
	// ExpiresIn содержит время жизни кода в секундах.
	ExpiresIn int64 `json:"expires_in"`
	// Interval содержит нижнюю границу ожидания в секундах. nil допускает значение RFC 8628.
	Interval *int64 `json:"interval"`
}

// tokenResponse содержит новую пару и её собственный срок от OAuth-сервиса.
type tokenResponse struct {
	// AccessToken передаётся в заголовке на последующую проверку.
	AccessToken string `json:"access_token"`
	// RefreshToken нужен для следующего одноразового обмена.
	RefreshToken string `json:"refresh_token"`
	// ExpiresIn содержит время жизни токена в секундах после выдачи.
	ExpiresIn int64 `json:"expires_in"`
	// TokenType должен разрешать использование токена через Bearer.
	TokenType string `json:"token_type"`
}

// validationResponse читает идентичность и полномочия, подтверждённые Twitch.
type validationResponse struct {
	// ClientID указывает приложение, которому выдан токен.
	ClientID string `json:"client_id"`
	// UserID содержит десятичный идентификатор пользователя. App token оставляет его пустым.
	UserID string `json:"user_id"`
	// Scopes содержит действующие разрешения access token.
	Scopes []string `json:"scopes"`
	// ExpiresIn содержит оставшиеся секунды; nil означает отсутствие доказанного срока.
	ExpiresIn *int64 `json:"expires_in"`
}

// tokenValidation содержит проверенный результат /validate с разобранным временем.
type tokenValidation struct {
	// userID закрепляет пользователя, подтверждённого Twitch для предъявленного токена.
	userID string
	// scopes содержит разрешения, проверенные через /validate.
	scopes []string
	// expiresIn содержит остаток срока, включая ноль для исчерпанного срока.
	expiresIn time.Duration
}

// oauthError содержит два поля ответа, в которых Twitch передаёт признаки OAuth-отказа.
type oauthError struct {
	// Error содержит машинную причину либо общее название HTTP-ошибки.
	Error string `json:"error"`
	// Message содержит документированную причину Twitch. Исходный текст не выходит из адаптера.
	Message string `json:"message"`
}

// BeginDevice запрашивает коды для публичного приложения clientID и проверяет
// ссылку для браузера. Время действия уменьшается на длительность запроса,
// чтобы вызывающий код мог установить локальный срок без продления кода.
func (a *Adapter) BeginDevice(ctx context.Context, clientID string) (DeviceChallenge, error) {
	if !validOAuthValue(clientID, core.ChatIDBytes) {
		return DeviceChallenge{}, core.ErrInvalid
	}
	// request передаёт Client ID и согласованный scope. err означает ошибку построения запроса.
	request, err := a.formRequest(ctx, deviceEndpoint, url.Values{
		"client_id": {clientID}, "scopes": {chatReadScope},
	})
	if err != nil {
		return DeviceChallenge{}, err
	}
	// startedAt отмечает нижнюю границу времени выдачи кода.
	startedAt := a.now()
	// status и body содержат ответ о выдаче, err отличает отказ транспорта или отмену.
	status, _, body, err := a.do(request)
	if err != nil {
		return DeviceChallenge{}, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return DeviceChallenge{}, core.ErrInvalid
	default:
		return DeviceChallenge{}, core.ErrNotReady
	}

	// response содержит непроверенные поля выдачи.
	var response deviceResponse
	if json.Unmarshal(body, &response) != nil {
		return DeviceChallenge{}, core.ErrNotReady
	}
	// interval использует пять секунд из RFC 8628 при отсутствии поля.
	interval := 5 * time.Second
	if response.Interval != nil {
		interval = oauthSeconds(*response.Interval)
	}
	// challenge отделяет код устройства для опроса от полей, доступных браузеру после проверки.
	challenge := DeviceChallenge{
		DeviceCode: response.DeviceCode, UserCode: response.UserCode, VerificationURI: response.VerificationURI,
		ExpiresIn: oauthSeconds(response.ExpiresIn) - max(a.now().Sub(startedAt), 0), Interval: interval,
	}
	// err сообщает, почему параметры нельзя вернуть для публичного подтверждения или опроса.
	if err := challenge.Validate(); err != nil {
		return DeviceChallenge{}, err
	}
	return challenge, nil
}

// PollDevice опрашивает текущий код устройства один раз. Только DeviceComplete
// содержит пару, подтверждённую Twitch для clientID. После успешного обмена
// даже временная ошибка проверки завершает попытку, поскольку код одноразовый.
// Интервалы, истечение и отмену всей попытки координирует её владелец.
func (a *Adapter) PollDevice(ctx context.Context, clientID, deviceCode string) (DevicePoll, error) {
	if !validOAuthValue(clientID, core.ChatIDBytes) || !validOAuthValue(deviceCode, core.ChatCredentialBytes) {
		return DevicePoll{}, core.ErrInvalid
	}
	// request содержит предъявляемые Twitch параметры обмена, err сообщает об ошибке построения.
	request, err := a.formRequest(ctx, tokenEndpoint, url.Values{
		"client_id": {clientID}, "device_code": {deviceCode}, "grant_type": {deviceGrantType}, "scopes": {chatReadScope},
	})
	if err != nil {
		return DevicePoll{}, err
	}
	// startedAt служит нижней границей времени выдачи токенов.
	startedAt := a.now()
	// status, headers и body содержат ответ обмена, err сообщает об отмене или сбое чтения.
	status, headers, body, err := a.do(request)
	// result сохраняет запрошенный Twitch минимум до следующей сетевой попытки.
	result := DevicePoll{RetryAfter: parseRetryAfter(headers.Get("Retry-After"), a.now())}
	if status == http.StatusOK {
		result.State = DeviceDenied
		if err != nil {
			return result, err
		}
		// bundle содержит пару после сверки с /validate, err сообщает о непригодной паре или сбое проверки.
		bundle, err := a.validatedBundle(ctx, clientID, "", "", startedAt, body)
		if err != nil {
			return result, err
		}
		result.State, result.Bundle = DeviceComplete, bundle
		return result, nil
	}
	if err != nil {
		return result, err
	}
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		result.State = classifyDeviceError(body)
		return result, nil
	default:
		return result, core.ErrNotReady
	}
}

// Refresh проверяет текущую пару из сериализованного CredentialOwner.
// Новая пара нужна при подтверждённом 401 или исчерпанном сроке. Обмен
// выполняется один раз, а результат проверяется для тех же ClientID/UserID
// перед возвратом. Второе значение запрещает повторно предъявлять прежний
// refresh token, если он мог быть использован, даже при отмене или потере ответа.
func (a *Adapter) Refresh(ctx context.Context, current core.ChatCredentials) (*core.ChatCredentialBundle, bool, error) {
	if current.Provider != core.ChatTwitch || current.Bundle.Validate(core.ChatTwitch) != nil {
		return nil, false, core.ErrInvalid
	}
	// credentials содержит текущую пару и закреплённую идентичность аккаунта.
	credentials := current.Bundle.Twitch
	// checked содержит проверенный срок, status отвечает за подтверждение 401,
	// err отличает невалидные полномочия от недоступности проверки.
	checked, status, err := a.validate(ctx, credentials.ClientID, credentials.UserID, credentials.AccessToken)
	if status != http.StatusUnauthorized {
		if err != nil {
			return nil, false, err
		}
		if checked.expiresIn > 0 {
			return nil, false, nil
		}
	}

	// request передаёт одноразовый refresh token в теле. err сообщает об ошибке до обмена.
	request, err := a.formRequest(ctx, tokenEndpoint, url.Values{
		"client_id": {credentials.ClientID}, "grant_type": {"refresh_token"}, "refresh_token": {credentials.RefreshToken},
	})
	if err != nil {
		return nil, false, err
	}
	// err подтверждает, что обмен ещё можно остановить до передачи транспорту.
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	// startedAt ограничивает начало срока пригодности новой пары.
	startedAt := a.now()
	// status и headers описывают ответ обмена, body содержит новую пару, err сообщает о сбое чтения.
	status, headers, body, err := a.do(request)
	// authRejected подтверждает отказ в обмене из-за полномочий, rejected включает отказ из-за частоты запросов.
	authRejected := refreshAuthRejected(status, body)
	rejected := authRejected || status == http.StatusTooManyRequests
	if status == http.StatusTooManyRequests && ctx.Err() == nil {
		return nil, false, a.oauthRateLimit(headers)
	}
	if authRejected && ctx.Err() == nil {
		return nil, false, core.ErrInvalid
	}
	if err != nil {
		return nil, !rejected && err != core.ErrInvalid, err
	}
	if status != http.StatusOK {
		return nil, !rejected, core.ErrNotReady
	}
	// bundle содержит проверенный следующий набор, err сообщает об отказе проверки после обмена.
	bundle, err := a.validatedBundle(ctx, credentials.ClientID, credentials.UserID, credentials.RefreshToken, startedAt, body)
	return bundle, true, err
}

// validate сверяет accessToken с clientID и ожидаемым userID. Пустой userID
// допускает впервые авторизуемого пользователя. 401 выделяется отдельно,
// поскольку только подтверждённый отказ токена допускает следующий обмен.
func (a *Adapter) validate(ctx context.Context, clientID, userID, accessToken string) (tokenValidation, int, error) {
	if !validOAuthValue(clientID, core.ChatIDBytes) || !validOAuthValue(accessToken, core.ChatCredentialBytes) {
		return tokenValidation{}, 0, core.ErrInvalid
	}
	// request содержит только фиксированный адрес проверки, err сообщает об ошибке построения.
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.oauthBase+validationEndpoint, nil)
	if err != nil {
		return tokenValidation{}, 0, core.ErrInvalid
	}
	request.Header.Set("Authorization", "OAuth "+accessToken)
	// status и body содержат ответ проверки, err сообщает об отмене или отказе транспорта.
	status, headers, body, err := a.do(request)
	if status == http.StatusTooManyRequests && ctx.Err() == nil {
		return tokenValidation{}, status, a.oauthRateLimit(headers)
	}
	if status == http.StatusUnauthorized {
		if ctx.Err() != nil {
			return tokenValidation{}, 0, ctx.Err()
		}
		return tokenValidation{}, status, core.ErrInvalid
	}
	if err != nil {
		return tokenValidation{}, status, err
	}
	if status == http.StatusForbidden || status == http.StatusBadRequest {
		return tokenValidation{}, status, core.ErrInvalid
	}
	if status != http.StatusOK {
		return tokenValidation{}, status, core.ErrNotReady
	}
	// response содержит непроверенные метаданные предъявленного токена.
	var response validationResponse
	if json.Unmarshal(body, &response) != nil {
		return tokenValidation{}, status, core.ErrNotReady
	}
	if response.ClientID != clientID || !validTwitchUserID(response.UserID) ||
		userID != "" && response.UserID != userID || !slices.Contains(response.Scopes, chatReadScope) {
		return tokenValidation{}, status, core.ErrInvalid
	}
	if response.ExpiresIn == nil {
		return tokenValidation{}, status, core.ErrNotReady
	}
	// expiresIn выражает подтверждённый остаток срока в единицах локального таймера.
	expiresIn := oauthSeconds(*response.ExpiresIn)
	if expiresIn < 0 {
		return tokenValidation{}, status, core.ErrNotReady
	}
	return tokenValidation{userID: response.UserID, scopes: response.Scopes, expiresIn: expiresIn}, status, nil
}

// validatedBundle принимает только полный новый bearer-набор. Он проверяет
// access token и выбирает меньший срок из выдачи и /validate. previousRefresh
// передаётся при ротации, когда возвращать прежний одноразовый токен уже нельзя.
func (a *Adapter) validatedBundle(ctx context.Context, clientID, userID, previousRefresh string, issuedAfter time.Time, body []byte) (*core.ChatCredentialBundle, error) {
	// response содержит полученную, но ещё не принятую локально пару.
	var response tokenResponse
	if json.Unmarshal(body, &response) != nil {
		return nil, core.ErrNotReady
	}
	// issuedTTL содержит верхнюю границу срока от момента выдачи.
	issuedTTL := oauthSeconds(response.ExpiresIn)
	if issuedTTL <= 0 || !strings.EqualFold(response.TokenType, "bearer") ||
		!validOAuthValue(response.RefreshToken, core.ChatCredentialBytes) || response.RefreshToken == previousRefresh {
		return nil, core.ErrInvalid
	}
	// validatedAfter служит нижней границей момента, на который Twitch подтвердил остаток срока.
	validatedAfter := a.now()
	// checked содержит подтверждённые данные, err сообщает об отказе или недоступности проверки.
	checked, _, err := a.validate(ctx, clientID, userID, response.AccessToken)
	if err != nil {
		return nil, err
	}
	// expiry выбирает более раннюю границу срока из выдачи и проверки.
	expiry := issuedAfter.Add(issuedTTL)
	// checkedExpiry переводит оставшиеся секунды /validate в абсолютный срок.
	checkedExpiry := validatedAfter.Add(checked.expiresIn)
	if checkedExpiry.Before(expiry) {
		expiry = checkedExpiry
	}
	// normalizedExpiry соответствует микросекундному формату хранилища, err сообщает о непредставимом сроке.
	normalizedExpiry, err := core.NormalizeTimestamp(expiry)
	if err != nil || !normalizedExpiry.After(a.now()) {
		return nil, core.ErrInvalid
	}
	// bundle объединяет проверенную идентичность, разрешения и новую пару для одного CAS вызывающего.
	bundle := &core.ChatCredentialBundle{Twitch: &core.TwitchChatCredentials{
		ClientID: clientID, UserID: checked.userID, Scopes: checked.scopes,
		AccessToken: response.AccessToken, RefreshToken: response.RefreshToken, ExpiresAt: normalizedExpiry,
	}}
	if bundle.Validate(core.ChatTwitch) != nil {
		return nil, core.ErrInvalid
	}
	if a.limits.CredentialBytes < core.ChatCredentialBytes {
		// encoded позволяет проверить локальный бюджет полного набора, err сообщает об отказе кодирования.
		encoded, err := json.Marshal(bundle)
		if err != nil || uint64(len(encoded)) > a.limits.CredentialBytes {
			return nil, core.ErrInvalid
		}
	}
	return bundle, nil
}

// formRequest собирает POST к OAuth-сервису. form кодируется один раз в теле,
// поэтому URL не содержит полномочий, а транспорт не может переотправить тело.
func (a *Adapter) formRequest(ctx context.Context, path string, form url.Values) (*http.Request, error) {
	// request содержит фиксированный URL и форму, err сообщает о локальном отказе до отправки.
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.oauthBase+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, core.ErrInvalid
	}
	request.GetBody = nil
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return request, nil
}

// oauthSeconds переводит секунды Twitch в длительность без переполнения.
// Некорректное значение возвращает минус единицу, чтобы отличать его от нуля.
func oauthSeconds(seconds int64) time.Duration {
	if seconds < 0 || seconds > math.MaxInt64/int64(time.Second) {
		return -1
	}
	return time.Duration(seconds) * time.Second
}

// validOAuthValue проверяет размер и возможность передать непрозрачные
// OAuth-реквизиты без пробельных и управляющих символов.
func validOAuthValue(value string, limit int) bool {
	if len(value) == 0 || len(value) > limit {
		return false
	}
	// index указывает на очередной байт непрозрачного значения.
	for index := range len(value) {
		if value[index] <= ' ' || value[index] >= 0x7f {
			return false
		}
	}
	return true
}

// oauthRateLimit сохраняет допустимую паузу Twitch вместе с безопасным временным отказом.
func (a *Adapter) oauthRateLimit(headers http.Header) error {
	return errors.Join(core.ErrNotReady, &core.ChatReadRetry{RetryAfter: parseRetryAfter(headers.Get("Retry-After"), a.now())})
}

// validVerificationURI принимает только страницу Twitch для активации.
// Единственный допускаемый код в ссылке совпадает с публичным userCode.
// deviceCode нельзя показать ни в ссылке, ни вместо userCode.
func validVerificationURI(value, userCode, deviceCode string) bool {
	if len(value) > core.ChatURLBytes || userCode == deviceCode {
		return false
	}
	// address содержит разобранную ссылку, err сообщает о неверном URL.
	address, err := url.Parse(value)
	if err != nil || address.Scheme != "https" || address.User != nil || address.Opaque != "" ||
		address.Fragment != "" || address.EscapedPath() != "/activate" ||
		address.Host != "www.twitch.tv" && address.Host != "twitch.tv" {
		return false
	}
	// query содержит публичные параметры активации, err сообщает о неверном кодировании.
	query, err := url.ParseQuery(address.RawQuery)
	if err != nil {
		return false
	}
	// name выбирает разрешённый параметр, values позволяет исключить неоднозначные повторы.
	for name, values := range query {
		if len(values) != 1 {
			return false
		}
		switch name {
		case "public":
			if values[0] != "true" {
				return false
			}
		case "device-code":
			if values[0] != userCode {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// classifyDeviceError распознаёт OAuth-коды RFC 8628 и документированные
// сообщения Twitch. Другой отказ клиента завершает попытку без исходного текста.
func classifyDeviceError(body []byte) DeviceState {
	// response принимает только поля причины отказа.
	var response oauthError
	if json.Unmarshal(body, &response) != nil {
		return DeviceDenied
	}
	// code содержит очередное поле причины в стандартной или Twitch-форме.
	for _, code := range [...]string{response.Error, response.Message} {
		switch strings.ToLower(strings.TrimSpace(code)) {
		case "authorization_pending":
			return DevicePending
		case "slow_down":
			return DeviceSlowDown
		case "expired_token", "invalid device code", "expired device code":
			return DeviceExpired
		case "access_denied", "authorization_declined":
			return DeviceDenied
		}
	}
	return DeviceDenied
}

// refreshAuthRejected распознаёт прямой отказ Twitch принять прежний токен.
// Неизвестный HTTP 400 не подтверждает результат одноразового обмена.
func refreshAuthRejected(status int, body []byte) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	if status != http.StatusBadRequest {
		return false
	}
	// response принимает поля отказа без передачи текста вызывающему коду.
	var response oauthError
	if json.Unmarshal(body, &response) != nil {
		return false
	}
	// code содержит очередное поле причины в стандартной или Twitch-форме.
	for _, code := range [...]string{response.Error, response.Message} {
		switch strings.ToLower(strings.TrimSpace(code)) {
		case "invalid_grant", "invalid_client", "unauthorized_client", "invalid refresh token":
			return true
		}
	}
	return false
}
