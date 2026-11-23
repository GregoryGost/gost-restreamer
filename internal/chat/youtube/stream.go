package youtube

import (
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Read читает один серверный поток YouTube для req.Key.ChannelID. До первого
// ответа действует срок подключения, затем чтением управляет ctx. Все вызовы
// sink используют req.Token. При сетевом разрыве или отзыве допуска к пакету
// ChatReadRetry возвращает позицию последнего обработанного пакета, если ctx
// ещё действует. Новая попытка принадлежит Manager.
func (a *Adapter) Read(ctx context.Context, req core.ChatReadRequest, sink core.ChatAdapterSink) error {
	if ctx == nil || sink == nil || req.Key.Provider != core.ChatYouTube ||
		!validLiveChatID(req.Key.ChannelID) || !validResumeToken(req.ResumeToken) {
		return core.ErrInvalid
	}
	// err останавливает отменённую попытку до открытия сетевого ресурса.
	if err := ctx.Err(); err != nil {
		return err
	}
	// credentials содержат серверный ключ точного аккаунта читателя.
	credentials := req.Credentials
	if credentials.Provider != core.ChatYouTube || credentials.AccountID != req.Key.AccountID ||
		credentials.Bundle.Validate(core.ChatYouTube) != nil || !validAPIKey(credentials.Bundle.YouTube.APIKey) {
		return sink.Status(ctx, req.Token, core.ChatAuthRequired, core.ChatReasonCredentials)
	}
	// method определяет форму запроса и ответов. err остаётся локальной ошибкой схемы.
	method, err := loadStreamSchema()
	if err != nil {
		return sink.Status(ctx, req.Token, core.ChatFailed, core.ChatReasonConfiguration)
	}
	// connection принадлежит только текущему Read, err сообщает об ошибке настройки клиента.
	connection, err := grpc.NewClient(a.grpcTarget,
		grpc.WithTransportCredentials(a.grpcCredentials), grpc.WithDisableRetry(), grpc.WithDisableServiceConfig(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(int(min(a.limits.UpstreamMessageBytes, uint64(math.MaxInt))))),
	)
	if err != nil {
		return sink.Status(ctx, req.Token, core.ChatFailed, core.ChatReasonConfiguration)
	}
	// Закрытие освобождает единственное соединение на любом пути возврата.
	// Ошибка уже закрытого ClientConn не меняет подтверждённую позицию потока.
	defer func() { _ = connection.Close() }()

	// rpcCtx обслуживает сетевой поток. cancel закрывает его при выходе,
	// stopConnectTimer завершает ожидание первого сообщения без остановки RPC.
	rpcCtx, cancel, stopConnectTimer := streamContext(ctx, a.limits.ConnectTimeout)
	defer cancel()
	defer stopConnectTimer()
	rpcCtx = metadata.NewOutgoingContext(rpcCtx, metadata.Pairs(apiKeyHeader, credentials.Bundle.YouTube.APIKey))
	// stream получает единственный ответный поток, err может содержать отказ до первого ответа.
	stream, err := startStream(rpcCtx, connection, method, req)
	if err != nil {
		return finishStreamFailure(ctx, req, sink, err, req.ResumeToken)
	}

	// resume хранит последнюю целиком обработанную позицию. confirmed показывает,
	// что сервер принял подписку и срок установления уже снят.
	resume := req.ResumeToken
	confirmed := false
	for {
		// response содержит только очередной пакет, ограниченный настройкой gRPC.
		response := dynamicpb.NewMessage(method.Output())
		// receiveErr сообщает о завершении RPC либо об ошибке чтения ответа.
		receiveErr := stream.RecvMsg(response)
		if !confirmed {
			stopConnectTimer()
			if rpcCtx.Err() != nil {
				receiveErr = rpcCtx.Err()
			}
		}
		if receiveErr != nil {
			return finishStreamFailure(ctx, req, sink, receiveErr, resume)
		}
		if !confirmed {
			// statusErr сообщает об отказе Manager принять подтверждение точной попытки.
			if statusErr := sink.Status(ctx, req.Token, core.ChatConnected, core.ChatReasonNone); statusErr != nil {
				return statusErr
			}
			confirmed = true
		}
		// ended опирается на данные площадки о завершении чата. emitErr означает, что пакет не обработан целиком.
		ended, emitErr := emitStreamBatch(ctx, req, sink, response.ProtoReflect())
		if emitErr != nil {
			// cancelErr показывает, что отменена сама попытка чтения.
			if cancelErr := ctx.Err(); cancelErr != nil {
				return cancelErr
			}
			if errors.Is(emitErr, core.ErrStale) {
				return &core.ChatReadRetry{ResumeToken: resume}
			}
			return emitErr
		}
		// next обновляет позицию после принятия сообщения Hub или учёта потери.
		// Отсутствующий либо чрезмерно большой токен оставляет прежнюю позицию.
		if next := protobufString(response, "next_page_token"); next != "" && validResumeToken(next) {
			resume = next
		}
		if ended {
			return sink.Status(ctx, req.Token, core.ChatFailed, core.ChatReasonEnded)
		}
	}
}

// streamContext создаёт дочерний контекст потока и таймер на timeout.
// Вторая функция отменяет RPC. Третья останавливает таймер, когда приходит
// первый ответ, либо дожидается уже начавшейся отмены. Обе вызываются при выходе.
func streamContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc, func()) {
	// ctx наследует отмену попытки, cancel прерывает в том числе блокирующий приём.
	ctx, cancel := context.WithCancel(parent)
	// timerDone закрывается сразу после отмены RPC по таймеру.
	timerDone := make(chan struct{})
	// timer ограничивает соединение и ожидание первого ответа одним интервалом.
	// Его функция отменяет RPC, если срок истекает до остановки таймера.
	timer := time.AfterFunc(timeout, func() {
		cancel()
		close(timerDone)
	})
	// stop ожидает только уже запущенную функцию таймера. Один вызов
	// sync.OnceFunc обслуживает подтверждение и отложенное освобождение.
	stop := sync.OnceFunc(func() {
		if !timer.Stop() {
			<-timerDone
		}
	})
	return ctx, cancel, stop
}

// startStream открывает один StreamList на connection по схеме method.
// req определяет чат и позицию продолжения, ctx содержит метаданные и отмену.
func startStream(ctx context.Context, connection *grpc.ClientConn, method protoreflect.MethodDescriptor, req core.ChatReadRequest) (grpc.ClientStream, error) {
	// stream принимает ответы после однократной отправки запроса. err передаёт исход
	// операции вызывающему коду, который заменяет диагностику безопасной причиной.
	stream, err := connection.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, streamRPCPath)
	if err != nil {
		return nil, err
	}
	err = stream.SendMsg(buildStreamRequest(method, req.Key.ChannelID, req.ResumeToken))
	if errors.Is(err, io.EOF) {
		// Ошибка отправки от другой стороны приходит через первый RecvMsg.
		return stream, nil
	}
	if err != nil {
		return nil, err
	}
	if err = stream.CloseSend(); err != nil {
		return nil, err
	}
	return stream, nil
}

// emitStreamBatch обрабатывает response в порядке YouTube для точного req
// и его sink. Принятые Hub потери допускают продвижение позиции. Отзыв,
// отмена и прочие отказы, для которых потеря не учтена, прерывают пакет.
func emitStreamBatch(ctx context.Context, req core.ChatReadRequest, sink core.ChatAdapterSink, response protoreflect.Message) (bool, error) {
	// ended отмечает проверенную метку завершения эфира или событие конца чата.
	ended := streamTimestamp(protobufString(response, "offline_at")) != nil
	// items содержит сообщения текущего пакета от старых к новым.
	items := response.Get(protobufField(response, "items")).List()
	// index указывает позицию обрабатываемого события в данном пакете.
	for index := range items.Len() {
		// err сообщает об отмене даже между событиями, которые не требуют вызова sink.
		if err := ctx.Err(); err != nil {
			return false, err
		}
		// message содержит данные для ленты, chatEnded подтверждает управляющее событие.
		message, chatEnded := normalizeStreamItem(items.Get(index).Message(), req.Key.ChannelID)
		ended = ended || chatEnded
		if message.Kind == "" {
			continue
		}
		// err отделяет учтённое Hub переполнение от отзыва допуска и других отказов.
		err := sink.Message(ctx, req.Token, message)
		if errors.Is(err, core.ErrInvalid) {
			// Hub отклонил некорректное содержимое без учёта потери.
			err = sink.Gap(ctx, req.Token, core.ChatReasonUpstreamGap)
			if err != nil {
				return false, err
			}
			continue
		}
		if err != nil && !errors.Is(err, core.ErrNotReady) {
			return false, err
		}
	}
	return ended, ctx.Err()
}

// validResumeToken проверяет конечный размер и строковый тип токена value.
// Пустое значение обозначает начальное чтение. Остальные токены передаются без изменений.
func validResumeToken(value string) bool {
	return len(value) <= core.ChatResumeTokenBytes && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// finishStreamFailure переводит err одной физической попытки в безопасное
// состояние для sink. Сброс затрагивает только явно отвергнутую позицию из
// req. Если уже обработан пакет с новой позицией resume, она сохраняется.
func finishStreamFailure(ctx context.Context, req core.ChatReadRequest, sink core.ChatAdapterSink, err error, resume string) error {
	// cancelErr определяет отмену вызывающим кодом прежде, чем публикуется исход RPC.
	if cancelErr := ctx.Err(); cancelErr != nil {
		return cancelErr
	}
	// failure содержит только безопасный статус, срок ожидания и признак отказа токена.
	failure := classifyStreamFailure(status.Convert(err))
	if failure.invalidResume && req.ResumeToken != "" {
		failure.state, failure.reason = core.ChatRetrying, core.ChatReasonNetwork
		if resume == req.ResumeToken {
			// gapErr сообщает, был ли пробел записан для ещё действующей попытки.
			if gapErr := sink.Gap(ctx, req.Token, core.ChatReasonUpstreamGap); gapErr != nil {
				return gapErr
			}
			resume = ""
			failure.reason = core.ChatReasonUpstreamGap
		}
	}
	// statusErr сообщает, принял ли Manager итог попытки. При отзыве статуса
	// в продолжающемся чтении сохраняется уже обработанная позиция.
	if statusErr := sink.Status(ctx, req.Token, failure.state, failure.reason); statusErr != nil {
		if errors.Is(statusErr, core.ErrStale) && ctx.Err() == nil {
			return &core.ChatReadRetry{ResumeToken: resume, RetryAfter: failure.retryAfter}
		}
		return statusErr
	}
	if failure.state == core.ChatRetrying {
		// cancelErr запрещает возвращать позицию, если попытку отозвали во время callback.
		if cancelErr := ctx.Err(); cancelErr != nil {
			return cancelErr
		}
		return &core.ChatReadRetry{ResumeToken: resume, RetryAfter: failure.retryAfter}
	}
	return nil
}

// streamFailure содержит локальную классификацию ответа gRPC без текста и метаданных Google.
type streamFailure struct {
	// state и reason задают состояние источника и его публичное объяснение.
	state  core.ChatSourceState
	reason core.ChatReason
	// retryAfter содержит наибольшую допустимую задержку из RetryInfo.
	retryAfter time.Duration
	// invalidResume означает, что площадка указала на недействительный токен.
	invalidResume bool
}

// classifyStreamFailure выбирает безопасную причину по коду rpc и
// типизированным деталям. EOF без признаков завершения допускает возобновление.
func classifyStreamFailure(rpc *status.Status) streamFailure {
	// failure по умолчанию разрешает восстановление после транспортного отказа.
	failure := streamFailure{state: core.ChatRetrying, reason: core.ChatReasonNetwork}
	// code содержит стандартный код gRPC.
	code := rpc.Code()
	switch code {
	case codes.Unauthenticated:
		failure.state, failure.reason = core.ChatAuthRequired, core.ChatReasonCredentials
	case codes.PermissionDenied, codes.FailedPrecondition, codes.NotFound:
		failure.state, failure.reason = core.ChatFailed, core.ChatReasonUnavailable
	case codes.InvalidArgument, codes.Unimplemented:
		failure.state, failure.reason = core.ChatFailed, core.ChatReasonConfiguration
	case codes.ResourceExhausted:
		failure.reason = core.ChatReasonQuota
	}
	// detail содержит очередное типизированное расширение gRPC, value — его разобранное значение.
	for _, detail := range rpc.Details() {
		switch value := detail.(type) {
		case *errdetails.RetryInfo:
			// delay содержит рекомендованный Google интервал между попытками.
			if delay := value.GetRetryDelay(); delay != nil && delay.CheckValid() == nil {
				failure.retryAfter = max(failure.retryAfter, delay.AsDuration())
			}
		case *errdetails.ErrorInfo:
			failure.applyErrorInfo(code, value)
		}
	}
	return failure
}

// applyErrorInfo уточняет причину только по значению ErrorInfo,
// относящемуся к сервису YouTube и совместимому с исходным code.
func (f *streamFailure) applyErrorInfo(code codes.Code, info *errdetails.ErrorInfo) {
	if info.GetDomain() != "youtube.googleapis.com" && info.GetDomain() != "googleapis.com" {
		return
	}
	// service позволяет исключить детали, помеченные именем другой службы.
	if service := info.GetMetadata()["service"]; service != "" && service != "youtube.googleapis.com" {
		return
	}
	switch info.GetReason() {
	case "API_KEY_INVALID", "API_KEY_SERVICE_BLOCKED", "API_KEY_IP_ADDRESS_BLOCKED",
		"API_KEY_HTTP_REFERRER_BLOCKED", "API_KEY_ANDROID_APP_BLOCKED", "API_KEY_IOS_APP_BLOCKED":
		if code == codes.PermissionDenied || code == codes.InvalidArgument || code == codes.Unauthenticated {
			f.state, f.reason, f.invalidResume = core.ChatAuthRequired, core.ChatReasonCredentials, false
		}
	case "SERVICE_DISABLED":
		if code == codes.PermissionDenied {
			f.state, f.reason = core.ChatFailed, core.ChatReasonConfiguration
		}
	case "LIVE_CHAT_DISABLED", "liveChatDisabled":
		if code == codes.FailedPrecondition || code == codes.PermissionDenied {
			f.state, f.reason = core.ChatFailed, core.ChatReasonDisabled
		}
	case "LIVE_CHAT_ENDED", "liveChatEnded":
		if code == codes.FailedPrecondition || code == codes.PermissionDenied {
			f.state, f.reason = core.ChatFailed, core.ChatReasonEnded
		}
	case "PAGE_TOKEN_INVALID", "pageTokenInvalid", "invalidPageToken":
		if code == codes.InvalidArgument && f.state == core.ChatFailed {
			f.invalidResume = true
		}
	case "RATE_LIMIT_EXCEEDED", "RESOURCE_QUOTA_EXCEEDED", "rateLimitExceeded", "quotaExceeded":
		if code == codes.ResourceExhausted || code == codes.PermissionDenied {
			f.state, f.reason = core.ChatRetrying, core.ChatReasonQuota
		}
	}
}
