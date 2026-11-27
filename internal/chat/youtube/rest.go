package youtube

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

// restOperation определяет путь запроса и правила учета обнаружения.
type restOperation uint8

const (
	// restSearch начинает новый проход с проверкой общего темпа поиска.
	restSearch restOperation = iota
	// restSearchNext продолжает уже допущенный проход.
	restSearchNext
	// restVideos перепроверяет найденные видео с учётом общей квоты.
	restVideos
	// restKeyProbe проверяет полномочия через videos.list вне локальной квоты обнаружения.
	restKeyProbe
)

// search сообщает, что запрос относится к отдельной квоте search.list.
func (o restOperation) search() bool { return o == restSearch || o == restSearchNext }

// restFailure содержит только сведения, необходимые для безопасного статуса и повтора.
type restFailure struct {
	// reason содержит код причины либо пустое значение при успешном запросе.
	reason core.ChatReason
	// retryAfter задаёт нижнюю границу задержки следующей попытки.
	retryAfter time.Duration
	// daily отмечает подтверждённое исчерпание суточной квоты Google.
	daily bool
	// sharedRate отмечает подтверждённое ограничение темпа сервиса для этой категории.
	sharedRate bool
}

// discovery переводит подтверждённый отказ REST в состояние Manager.
func (f restFailure) discovery() core.ChatDiscovery {
	// state выбирает жизненный цикл источника в зависимости от причины.
	state := core.ChatRetrying
	switch f.reason {
	case core.ChatReasonCredentials:
		state = core.ChatAuthRequired
	case core.ChatReasonConfiguration:
		state = core.ChatFailed
	case core.ChatReasonNoBroadcast, core.ChatReasonUnavailable, core.ChatReasonEnded:
		state = core.ChatWaitingChat
	}
	return core.ChatDiscovery{State: state, Reason: f.reason, RetryAfter: f.retryAfter}
}

// restPage задаёт общую форму успешного списочного ответа YouTube.
type restPage[T any] struct {
	// Items содержит список ресурсов. Отсутствие массива отделяется от пустой выдачи.
	Items []T `json:"items"`
	// NextPageToken указывает на продолжение выдачи, если оно существует.
	NextPageToken string `json:"nextPageToken"`
}

// googleRESTError читает машинные поля ошибки Google. Сообщения и metadata
// не декодируются, поскольку могут содержать секреты или параметры запроса.
type googleRESTError struct {
	// Error содержит кодовые причины в старом и новом форматах Google.
	Error struct {
		// Errors содержит причины формата YouTube Data API.
		Errors []struct {
			// Reason содержит машинный код отказа YouTube Data API.
			Reason string `json:"reason"`
		} `json:"errors"`
		// Details содержит типизированные сведения формата Google RPC JSON.
		Details []struct {
			// Type определяет схему сведения до чтения его причины.
			Type string `json:"@type"`
			// Domain определяет область машинного кода причины.
			Domain string `json:"domain"`
			// Reason содержит код ErrorInfo без текста сообщения.
			Reason string `json:"reason"`
		} `json:"details"`
	} `json:"error"`
}

// getREST выполняет одну операцию Google с ограничением времени и объёма.
// params кодируются в URL, key передаётся отдельным заголовком. result заполняется
// только для успешного статуса и корректного JSON. Временные и постоянные отказы
// превращаются в restFailure, через error выходит только отмена контекста вызывающего.
func (a *Adapter) getREST(ctx context.Context, operation restOperation, key string, params url.Values, result any) (restFailure, error) {
	// limited ограничивает ожидание слота и сетевой запрос. cancel освобождает таймер.
	limited, cancel := context.WithTimeout(ctx, a.limits.HTTPTimeout)
	defer cancel()
	select {
	case a.restSlots <- struct{}{}:
		// Callback возвращает слот после завершения запроса и закрытия тела.
		defer func() { <-a.restSlots }()
	case <-limited.Done():
		return restFailure{reason: core.ChatReasonNetwork}, ctx.Err()
	}
	// path выбирается из фиксированных операций и не зависит от URL из ответа.
	path := "/videos"
	if operation.search() {
		path = "/search"
	}
	// request содержит конечный GET. err сообщает о некорректном формировании запроса.
	request, err := http.NewRequestWithContext(limited, http.MethodGet, a.restBase+path+"?"+params.Encode(), nil)
	if err != nil {
		return restFailure{reason: core.ChatReasonConfiguration}, nil
	}
	request.Header.Set(apiKeyHeader, key)
	if limited.Err() != nil {
		return restFailure{reason: core.ChatReasonNetwork}, ctx.Err()
	}
	// retry содержит срок отказа общего бюджета перед отправкой запроса.
	if retry := a.quota.take(a.now(), operation); retry > 0 {
		return restFailure{reason: core.ChatReasonQuota, retryAfter: retry}, nil
	}
	// response открывает тело, а err остаётся внутри адаптера и не выводится наружу.
	response, err := a.httpClient.Do(request)
	if err != nil {
		return restFailure{reason: core.ChatReasonNetwork}, ctx.Err()
	}
	// Callback освобождает соединение даже при повреждённом или чрезмерном теле.
	// Результат чтения определяется ReadAll и статусом, а тело открыто только для чтения.
	defer func() { _ = response.Body.Close() }()
	// maximum задаёт байтовую границу. Дополнительный байт выявляет превышение.
	maximum := int64(a.limits.UpstreamHTTPBytes)
	// body содержит ограниченный ответ. err сообщает о незавершённом чтении.
	body, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if ctx.Err() != nil {
		return restFailure{}, ctx.Err()
	}
	if err != nil || int64(len(body)) > maximum || !utf8.Valid(body) {
		body = nil
	}
	if response.StatusCode != http.StatusOK {
		// failure отражает только проверенные причины и безопасную задержку по HTTP.
		failure := classifyRESTFailure(operation, response.StatusCode, body)
		// now фиксирует момент получения отказа для абсолютных и относительных сроков.
		now := a.now()
		failure.retryAfter = parseRetryAfter(response.Header.Get("Retry-After"), now)
		if failure.daily || failure.sharedRate {
			failure.retryAfter = a.quota.pause(now, operation, max(failure.retryAfter, a.limits.ReconnectMinBackoff), failure.daily)
		}
		return failure, nil
	}
	if json.Unmarshal(body, result) != nil {
		return restFailure{reason: core.ChatReasonNetwork}, nil
	}
	return restFailure{}, nil
}

// classifyRESTFailure проверяет машинные причины Google и сохраняет различие
// между исчерпанием суточной квоты, ограничением темпа и отсутствием полномочий.
// Код причины используется только для допустимых сочетаний с HTTP-статусом.
func classifyRESTFailure(operation restOperation, status int, body []byte) restFailure {
	// response содержит только диагностические коды, необходимые для классификации.
	var response googleRESTError
	if json.Unmarshal(body, &response) != nil {
		response = googleRESTError{}
	}
	// reason хранит наиболее точную распознанную причину и признак дневного отказа.
	var typed restFailure
	// detail содержит очередной код в формате YouTube Data API.
	for _, detail := range response.Error.Errors {
		typed = preferRESTReason(typed, googleReason(operation, status, detail.Reason))
	}
	// detail содержит очередное типизированное сведение Google.
	for _, detail := range response.Error.Details {
		if detail.Type == "type.googleapis.com/google.rpc.ErrorInfo" && (detail.Domain == "googleapis.com" || detail.Domain == "youtube.googleapis.com") {
			typed = preferRESTReason(typed, googleReason(operation, status, detail.Reason))
		}
	}
	if typed.reason != core.ChatReasonNone {
		return typed
	}
	switch status {
	case http.StatusTooManyRequests:
		return restFailure{reason: core.ChatReasonQuota}
	case http.StatusUnauthorized, http.StatusForbidden:
		return restFailure{reason: core.ChatReasonCredentials}
	case http.StatusBadRequest:
		return restFailure{reason: core.ChatReasonConfiguration}
	default:
		return restFailure{reason: core.ChatReasonNetwork}
	}
}

// googleReason распознаёт закрытый набор кодов YouTube и Google API.
// Подтверждение отсутствия видео относится только к videos.list по идентификаторам.
func googleReason(operation restOperation, status int, reason string) restFailure {
	switch status {
	case http.StatusForbidden, http.StatusTooManyRequests:
		switch reason {
		case "quotaExceeded", "dailyLimitExceeded", "QUOTA_EXCEEDED", "DAILY_LIMIT_EXCEEDED":
			return restFailure{reason: core.ChatReasonQuota, daily: true}
		case "servingLimitExceeded":
			return restFailure{reason: core.ChatReasonQuota, sharedRate: true}
		case "rateLimitExceeded", "userRateLimitExceeded", "concurrentLimitExceeded",
			"RATE_LIMIT_EXCEEDED", "USER_RATE_LIMIT_EXCEEDED":
			return restFailure{reason: core.ChatReasonQuota}
		case "channelClosed", "channelNotFound", "channelSuspended":
			if status == http.StatusForbidden && operation.search() {
				return restFailure{reason: core.ChatReasonConfiguration}
			}
		}
	case http.StatusBadRequest:
		switch reason {
		case "keyInvalid", "keyExpired", "API_KEY_INVALID", "API_KEY_EXPIRED":
			return restFailure{reason: core.ChatReasonCredentials}
		case "invalidPageToken":
			if operation == restSearchNext {
				return restFailure{reason: core.ChatReasonNetwork}
			}
		}
	case http.StatusNotFound:
		if operation == restVideos && reason == "videoNotFound" {
			return restFailure{reason: core.ChatReasonNoBroadcast}
		}
	}
	return restFailure{}
}

// preferRESTReason отдаёт приоритет суточному отказу и общему ограничению темпа.
// При одинаковом приоритете остаётся первая подходящая причина в ответе.
func preferRESTReason(current, next restFailure) restFailure {
	if current.reason == core.ChatReasonNone || next.daily && !current.daily ||
		next.sharedRate && !current.daily && !current.sharedRate {
		return next
	}
	return current
}

// parseRetryAfter разбирает обе формы HTTP Retry-After.
// Слишком большая длительность ограничивается представимым значением Go.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	// seconds содержит задержку Google в секундах, err отличает её от HTTP-даты.
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		return time.Duration(min(seconds, uint64(math.MaxInt64/int64(time.Second)))) * time.Second
	}
	// until содержит абсолютный срок Google, err отмечает неподходящий формат.
	if until, err := http.ParseTime(value); err == nil {
		return max(until.Sub(now), 0)
	}
	return 0
}
