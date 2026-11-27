package vkvideolive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// readFailure содержит безопасный исход сетевого шага до публикации статуса.
type readFailure struct {
	// state различает временное, ожидающее и конечное состояние.
	state core.ChatSourceState
	// reason содержит причину без токена или исходного текста площадки.
	reason core.ChatReason
	// delay сохраняет минимальную задержку для повтора.
	delay time.Duration
}

// Read проверяет req, получает свежий токен соединения и читает одну подписку.
// При раннем отказе Bearer один раз запрашивает восстановление у владельца
// полномочий. Перед итоговым событием в sink освобождает сокет и работу,
// публикующую CONNECTED. При отмене дополнительные события не передаёт.
func (a *Adapter) Read(ctx context.Context, req core.ChatReadRequest, sink core.ChatAdapterSink) error {
	if ctx == nil || sink == nil {
		return core.ErrInvalid
	}
	// err прекращает уже отменённую попытку до обращения к сети.
	if err := ctx.Err(); err != nil {
		return err
	}
	// id и err подтверждают каноническое десятичное представление результата Discover.
	id, err := strconv.ParseInt(req.Key.ChannelID, 10, 64)
	if req.Key.Provider != core.ChatVKVideoLive || req.Key.AccountID == "" ||
		err != nil || id <= 0 || strconv.FormatInt(id, 10) != req.Key.ChannelID {
		return finishRead(ctx, req.Token, sink, readFailure{state: core.ChatFailed, reason: core.ChatReasonConfiguration}, false)
	}
	if !validCredentials(req.Credentials, req.Key.AccountID) {
		return finishRead(ctx, req.Token, sink, readFailure{state: core.ChatAuthRequired, reason: core.ChatReasonCredentials}, false)
	}
	// token содержит новое полномочие соединения. failure и err различают исход площадки и отзыв попытки.
	token, failure, err := a.socketToken(ctx, req)
	if err != nil {
		return err
	}
	if failure.state != "" {
		return finishRead(ctx, req.Token, sink, failure, false)
	}
	// subscribed отмечает протокольный ACK независимо от времени публикации CONNECTED.
	failure, subscribed, err := a.readSocket(ctx, req, sink, token)
	if err != nil {
		return err
	}
	return finishRead(ctx, req.Token, sink, failure, subscribed)
}

// finishRead публикует итог failure точной попытки token. Подтверждённый
// разрыв subscribed сообщает Gap до нового статуса. Отказ управляющего вызова
// sink завершает попытку с его ошибкой.
func finishRead(ctx context.Context, token core.ChatWorkerToken, sink core.ChatAdapterSink, failure readFailure, subscribed bool) error {
	// err означает штатную отмену, при которой новых событий быть не должно.
	if err := ctx.Err(); err != nil {
		return err
	}
	if subscribed {
		// err показывает, что текущий читатель больше не может сообщить о пропуске.
		if err := sink.Gap(ctx, token, core.ChatReasonUpstreamGap); err != nil {
			return err
		}
	}
	// err подтверждает отказ от итогового статуса отозванной попытки.
	if err := sink.Status(ctx, token, failure.state, failure.reason); err != nil {
		return err
	}
	if failure.state == core.ChatRetrying {
		// retry передаёт срок и допустимое уточнение причины. Status уже опубликовал исходную причину.
		retry := &core.ChatReadRetry{RetryAfter: failure.delay}
		if failure.reason == core.ChatReasonNetwork || failure.reason == core.ChatReasonQuota {
			retry.Reason = failure.reason
		}
		return retry
	}
	return nil
}

// socketToken получает токен WebSocket для req. Один конечный бюджет включает
// оба запроса и возможное реактивное восстановление. AcquireHTTP освобождается
// перед обращением к владельцу и захватывается заново для повторного запроса.
// Временный отказ сохраняет самый строгий интервал, полученный за эту попытку.
func (a *Adapter) socketToken(ctx context.Context, req core.ChatReadRequest) (string, readFailure, error) {
	// bounded ограничивает все шаги одного получения, cancel освобождает таймер.
	bounded, cancel := context.WithTimeout(ctx, a.limits.HTTPTimeout)
	defer cancel()
	// token содержит результат первого запроса, failure объясняет отказ площадки.
	// rejected означает именно HTTP 401 на запросе с Bearer.
	token, failure, rejected := a.requestSocketToken(bounded, req, req.Credentials.Bundle.VKVideoLive.AccessToken)
	if ctx.Err() != nil {
		return "", readFailure{}, ctx.Err()
	}
	if !rejected || req.RefreshRejectedCredentials == nil || bounded.Err() != nil {
		return token, failure, nil
	}

	// credentials содержит актуальный проверенный набор, err различает отказ полномочий,
	// отзыв попытки и сбой восстановления.
	credentials, err := req.RefreshRejectedCredentials(bounded)
	if ctx.Err() != nil {
		return "", readFailure{}, ctx.Err()
	}
	switch {
	case errors.Is(err, core.ErrStale):
		return "", readFailure{}, err
	case errors.Is(err, core.ErrInvalid), errors.Is(err, core.ErrNotFound):
		return "", readFailure{state: core.ChatAuthRequired, reason: core.ChatReasonCredentials}, nil
	case err != nil:
		// retry уточняет ограничение DevAPI, ok указывает на типизированную ошибку владельца.
		if retry, ok := errors.AsType[*core.ChatReadRetry](err); ok && retry != nil {
			if retry.Reason == core.ChatReasonQuota {
				failure.reason = core.ChatReasonQuota
			}
			failure.delay = max(failure.delay, retry.RetryAfter)
		}
		return "", failure, nil
	case !validCredentials(credentials, req.Key.AccountID):
		return "", readFailure{state: core.ChatAuthRequired, reason: core.ChatReasonCredentials}, nil
	}

	// replacement содержит исход повторного запроса с новым Bearer.
	token, replacement, _ := a.requestSocketToken(bounded, req, credentials.Bundle.VKVideoLive.AccessToken)
	if ctx.Err() != nil {
		return "", readFailure{}, ctx.Err()
	}
	if replacement.state == core.ChatRetrying {
		replacement.delay = max(replacement.delay, failure.delay)
	}
	return token, replacement, nil
}

// requestSocketToken выполняет один конечный запрос для appToken в HTTP-слоте req.
// Возвращает токен, безопасный отказ и признак HTTP 401 для условного восстановления.
func (a *Adapter) requestSocketToken(ctx context.Context, req core.ChatReadRequest, appToken string) (string, readFailure, bool) {
	if req.AcquireHTTP != nil {
		// release возвращает HTTP-слот, err сообщает об отказе допуска.
		release, err := req.AcquireHTTP(ctx)
		if err != nil || release == nil {
			return "", readFailure{state: core.ChatRetrying, reason: core.ChatReasonLimit}, false
		}
		defer release()
	}
	// result содержит ответ DevAPI, err отличает неполное тело или отмену.
	result, err := a.bearerGET(ctx, "/v1/websocket/token", "", appToken)
	if result.status != http.StatusOK || err != nil {
		return "", a.httpFailure(result, false), result.status == http.StatusUnauthorized
	}
	// envelope содержит только полномочие конкретного физического соединения.
	var envelope devEnvelope[struct {
		// Token передаётся в команду connect по защищённому WebSocket.
		Token string `json:"token"`
	}]
	if json.Unmarshal(result.body, &envelope) != nil || envelope.Data == nil || len(envelope.Error) != 0 ||
		!validRequiredText(envelope.Data.Token, core.ChatCredentialBytes) {
		return "", readFailure{state: core.ChatRetrying, reason: core.ChatReasonNetwork}, false
	}
	return envelope.Data.Token, readFailure{}, false
}
