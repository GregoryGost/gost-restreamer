package vkvideolive

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// httpResult сохраняет ограниченный ответ и безопасные данные для классификации отказа.
type httpResult struct {
	// status содержит код HTTP, если сервер прислал заголовки.
	status int
	// retryAfter содержит исходное значение заголовка Retry-After.
	retryAfter string
	// body содержит полное тело в пределах бюджета либо nil при отказе чтения.
	body []byte
}

// devEnvelope содержит общие поля ответа DevAPI для операции с телом T.
type devEnvelope[T any] struct {
	// Data содержит результат конечной операции с OAuth Bearer.
	Data *T `json:"data"`
	// Error отражает присутствие кода ошибки, в том числе в формально успешном ответе.
	Error json.RawMessage `json:"error"`
}

// readHTTP выполняет конечный HTTP-запрос и закрывает его тело перед возвратом.
// Ответы квоты, сервера и отказа Bearer классифицируются по заголовкам сразу.
// Для остальных ответов срок включает чтение тела, а заголовки сохраняются
// при отказе чтения. Исходные сетевые ошибки остаются в адаптере.
func (a *Adapter) readHTTP(req *http.Request) (httpResult, error) {
	// ctx отвечает за отмену вызывающим. bounded ограничивает время HTTP, cancel снимает его таймер.
	ctx := req.Context()
	bounded, cancel := context.WithTimeout(ctx, a.limits.HTTPTimeout)
	defer cancel()
	// client сохраняет запрет перенаправления также для транспорта протокольного теста.
	client := *a.httpClient
	client.CheckRedirect = refuseRedirect

	// response предоставляет заголовки и тело. err сообщает о сбое запроса.
	response, err := client.Do(req.WithContext(bounded))
	if err != nil {
		if ctx.Err() != nil {
			return httpResult{}, ctx.Err()
		}
		return httpResult{}, core.ErrNotReady
	}
	// Отложенная функция освобождает тело даже при превышении лимита или отмене.
	defer func() { _ = response.Body.Close() }()

	// result сохраняет данные классификации до чтения полезной нагрузки.
	result := httpResult{status: response.StatusCode, retryAfter: response.Header.Get("Retry-After")}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if result.status == http.StatusTooManyRequests || result.status >= http.StatusInternalServerError && result.status < 600 ||
		result.status == http.StatusUnauthorized && req.Method == http.MethodGet {
		return result, nil
	}
	// maximum учитывает только полезную нагрузку. Дополнительный байт обнаруживает превышение.
	maximum := int64(a.limits.UpstreamHTTPBytes)
	// body содержит прочитанные байты, err сообщает об отказе чтения.
	body, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil || bounded.Err() != nil || int64(len(body)) > maximum || !utf8.Valid(body) {
		return result, core.ErrNotReady
	}
	result.body = body
	return result, nil
}

// bearerGET передаёт token только в заголовке запроса фиксированного маршрута path.
// query содержит уже экранированные параметры или пустую строку.
func (a *Adapter) bearerGET(ctx context.Context, path, query, token string) (httpResult, error) {
	// request содержит запрос DevAPI. err сообщает о невозможности построить фиксированный URL.
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.apiBase+path+query, nil)
	if err != nil {
		return httpResult{}, core.ErrNotReady
	}
	request.Header.Set("Authorization", "Bearer "+token)
	return a.readHTTP(request)
}

// errorCode читает только документированный машинный код. Текст и вложенные данные ошибки
// остаются внутри сетевой границы. Непригодный конверт даёт пустую строку.
func (r httpResult) errorCode() string {
	// envelope принимает стабильный код из тела конечного ответа.
	var envelope struct {
		// Error содержит машинный код, достаточный для классификации.
		Error string `json:"error"`
	}
	if json.Unmarshal(r.body, &envelope) != nil {
		return ""
	}
	return envelope.Error
}

// httpFailure переводит ответ result в состояние чтения или обнаружения.
// Параметр findingChannel разрешает ожидание для точного кода channel_not_found.
func (a *Adapter) httpFailure(result httpResult, findingChannel bool) readFailure {
	switch result.status {
	case http.StatusTooManyRequests:
		return readFailure{state: core.ChatRetrying, reason: core.ChatReasonQuota, delay: retryAfter(result.retryAfter, a.now())}
	case http.StatusForbidden:
		switch result.errorCode() {
		case "forbidden", "invalid_scope", "channel_banned":
			return readFailure{state: core.ChatFailed, reason: core.ChatReasonUnavailable}
		}
	case http.StatusMethodNotAllowed, http.StatusGone:
		return readFailure{state: core.ChatFailed, reason: core.ChatReasonConfiguration}
	case http.StatusBadRequest, http.StatusNotFound:
		switch result.errorCode() {
		case "channel_not_found":
			if findingChannel {
				return readFailure{state: core.ChatWaitingChat, reason: core.ChatReasonUnavailable, delay: retryAfter(result.retryAfter, a.now())}
			}
		case "not_found", "unknown_api_method":
			return readFailure{state: core.ChatFailed, reason: core.ChatReasonConfiguration}
		}
	}
	return readFailure{state: core.ChatRetrying, reason: core.ChatReasonNetwork, delay: retryAfter(result.retryAfter, a.now())}
}

// retryAfter читает число секунд либо HTTP-дату value относительно now.
// Недопустимое значение и прошедшая дата оставляют штатное ожидание Manager.
func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value != "" && strings.Trim(value, "0123456789") == "" {
		// seconds содержит проверенное целое число секунд. При переполнении ParseUint возвращает максимум.
		seconds, _ := strconv.ParseUint(value, 10, 64)
		return time.Duration(min(seconds, uint64(math.MaxInt64/int64(time.Second)))) * time.Second
	}
	// until задаёт абсолютный срок, err сообщает о неверной HTTP-дате.
	if until, err := http.ParseTime(value); err == nil {
		return max(until.Sub(now), 0)
	}
	return 0
}

// Discover определяет точный ID чата по постоянному ChannelHint source.
// При HTTP 401 возвращает ErrChatAppTokenRejected для условного обновления
// отвергнутого набора credentials владельцем аккаунта. Задержка сохраняется
// в ChatReadRetry для следующей плановой попытки при временном отказе восстановления.
func (a *Adapter) Discover(ctx context.Context, source core.ChatSource, credentials core.ChatCredentials) (core.ChatDiscovery, error) {
	if ctx == nil {
		return core.ChatDiscovery{}, core.ErrInvalid
	}
	// err прерывает отменённый поиск до запроса.
	if err := ctx.Err(); err != nil {
		return core.ChatDiscovery{}, err
	}
	if source.Provider != core.ChatVKVideoLive || source.AccountID == "" ||
		!validRequiredText(source.ChannelHint, core.ChatIDBytes) || strings.ContainsAny(source.ChannelHint, "/\\?#") {
		return core.ChatDiscovery{State: core.ChatFailed, Reason: core.ChatReasonConfiguration}, nil
	}
	if !validCredentials(credentials, source.AccountID) {
		return core.ChatDiscovery{State: core.ChatAuthRequired, Reason: core.ChatReasonCredentials}, nil
	}
	// query передаёт slug как единственное значение параметра.
	query := url.Values{"channel_url": {source.ChannelHint}}.Encode()
	// result содержит ограниченный ответ канала. err отличает отмену и неполное тело.
	result, err := a.bearerGET(ctx, "/v1/channel", "?"+query, credentials.Bundle.VKVideoLive.AccessToken)
	if ctx.Err() != nil {
		return core.ChatDiscovery{}, ctx.Err()
	}
	if result.status == http.StatusUnauthorized {
		// delay ограничивает следующую плановую попытку после реактивного перевыпуска.
		if delay := retryAfter(result.retryAfter, a.now()); delay > 0 {
			return core.ChatDiscovery{}, errors.Join(core.ErrChatAppTokenRejected, &core.ChatReadRetry{Reason: core.ChatReasonNetwork, RetryAfter: delay})
		}
		return core.ChatDiscovery{}, core.ErrChatAppTokenRejected
	}
	if result.status != http.StatusOK || err != nil {
		// failure описывает безопасное состояние и время следующей попытки.
		failure := a.httpFailure(result, true)
		return core.ChatDiscovery{State: failure.state, Reason: failure.reason, RetryAfter: failure.delay}, nil
	}
	// envelope извлекает числовой ID без преобразования в float64.
	var envelope devEnvelope[struct {
		// Channel содержит точную идентичность чата, найденного по slug.
		Channel struct {
			// ID является положительным int64, который VK передаёт числом JSON.
			ID int64 `json:"id"`
		} `json:"channel"`
	}]
	if json.Unmarshal(result.body, &envelope) != nil || envelope.Data == nil || len(envelope.Error) != 0 || envelope.Data.Channel.ID <= 0 {
		return core.ChatDiscovery{State: core.ChatRetrying, Reason: core.ChatReasonNetwork}, nil
	}
	return core.ChatDiscovery{ChannelID: strconv.FormatInt(envelope.Data.Channel.ID, 10)}, nil
}

// validCredentials проверяет принадлежность набора credentials аккаунту accountID
// и пригодность токена для заголовка Bearer. Срок проверяет владелец аккаунта.
func validCredentials(credentials core.ChatCredentials, accountID core.ID) bool {
	return credentials.AccountID == accountID && credentials.Provider == core.ChatVKVideoLive &&
		credentials.Bundle.Validate(core.ChatVKVideoLive) == nil && validBearer(credentials.Bundle.VKVideoLive.AccessToken)
}

// validBearer проверяет форму токена value для заголовка Authorization.
func validBearer(value string) bool {
	if !validRequiredText(value, core.ChatCredentialBytes) {
		return false
	}
	// character содержит очередной символ токена до передачи в HTTP-заголовок.
	for _, character := range value {
		if character <= ' ' || character >= 0x7f {
			return false
		}
	}
	return true
}
