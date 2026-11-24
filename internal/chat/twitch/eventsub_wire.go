package twitch

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/coder/websocket"
)

const (
	// chatSubscriptionType выбирает события сообщений обычного чата Twitch.
	chatSubscriptionType = "channel.chat.message"
	// chatSubscriptionVersion закрепляет версию контракта сообщений и подписки.
	chatSubscriptionVersion = "1"
	// eventSubTransport обозначает передачу уведомлений по WebSocket.
	eventSubTransport = "websocket"
	// maxKeepaliveSeconds ограничивает объявленный Twitch период тишины до 600 секунд.
	maxKeepaliveSeconds = 600
	// reconnectGrace ограничивает срок переноса подписки, указанный Twitch.
	reconnectGrace = 30 * time.Second
	// reconnectURLBytes ограничивает адрес переноса вместе с непрозрачным query.
	reconnectURLBytes = 4 << 10
)

// eventSubCondition связывает фактический чат и пользователя, от имени которого его читают.
type eventSubCondition struct {
	// BroadcasterID содержит постоянный идентификатор канала вещателя.
	BroadcasterID string `json:"broadcaster_user_id"`
	// ReaderID содержит идентификатор пользователя из подтверждённых полномочий.
	ReaderID string `json:"user_id"`
}

// eventSubSocketRef указывает размещение подписки на конкретном WebSocket.
type eventSubSocketRef struct {
	// Method отличает WebSocket от других способов доставки EventSub.
	Method string `json:"method"`
	// SessionID содержит идентификатор из session_welcome текущего соединения.
	SessionID string `json:"session_id"`
}

// eventSubSubscription содержит поля запроса подписки и доказательства её принятия.
type eventSubSubscription struct {
	// ID появляется в Helix и привязывает последующие уведомления к принятой подписке.
	ID string `json:"id,omitempty"`
	// Status содержит состояние из Helix либо причину отзыва.
	Status string `json:"status,omitempty"`
	// Type указывает каталог событий, выбранный для этой подписки.
	Type string `json:"type"`
	// Version закрепляет формат событий подписки.
	Version string `json:"version"`
	// Condition содержит оба точных пользовательских идентификатора.
	Condition eventSubCondition `json:"condition"`
	// Transport привязывает подписку к одному WebSocket.
	Transport eventSubSocketRef `json:"transport"`
}

// eventSubSession содержит данные session_welcome и session_reconnect.
type eventSubSession struct {
	// ID связывает сообщение с созданным или заменяемым сокетом.
	ID string `json:"id"`
	// Status различает готовое соединение и соединение, требующее переноса.
	Status string `json:"status"`
	// KeepaliveSeconds задаёт период тишины и срок первой подписки из session_welcome.
	KeepaliveSeconds int `json:"keepalive_timeout_seconds"`
	// ReconnectURL содержит адрес переноса, который Twitch требует использовать без изменений.
	ReconnectURL string `json:"reconnect_url"`
	// welcomeAt отмечает локальное подтверждение приветствия для контроля подписки и переноса.
	welcomeAt time.Time
}

// eventSubFrame описывает общую оболочку текстового сообщения Twitch.
type eventSubFrame struct {
	// Metadata содержит вид события, время его передачи и сведения о подписке.
	Metadata struct {
		// ID идентифицирует конверт EventSub независимо от идентификатора сообщения чата.
		ID string `json:"message_id"`
		// Kind выбирает уведомление или управляющее сообщение.
		Kind string `json:"message_type"`
		// Timestamp содержит платформенное время отправки конверта.
		Timestamp time.Time `json:"message_timestamp"`
		// Type указывает вид подписки для уведомления или отзыва.
		Type string `json:"subscription_type"`
		// Version содержит версию подписки для уведомления или отзыва.
		Version string `json:"subscription_version"`
	} `json:"metadata"`
	// Payload содержит только полезную нагрузку, соответствующую виду сообщения.
	Payload *struct {
		// Session присутствует в приветствии и запросе переноса подписки.
		Session *eventSubSession `json:"session"`
		// Subscription приходит в уведомлении или отзыве.
		Subscription *eventSubSubscription `json:"subscription"`
		// Event несёт данные одного сообщения чата в уведомлении.
		Event *eventSubChatMessage `json:"event"`
	} `json:"payload"`
}

// eventSubFailure содержит безопасный исход попытки и нижнюю границу задержки.
type eventSubFailure struct {
	// state сообщает рабочее или конечное состояние источника.
	state core.ChatSourceState
	// reason объясняет исход кодом из закрытого каталога чатов.
	reason core.ChatReason
	// retryAfter учитывает ограничение Twitch перед новой физической попыткой.
	retryAfter time.Duration
	// interrupted отмечает потерю уже подтверждённого транспорта без гарантии непрерывности.
	interrupted bool
}

// readEventSubFrame читает одно текстовое сообщение из conn. ctx ограничивает время
// чтения. При отказе функция возвращает только безопасную причину без данных кадра.
func readEventSubFrame(ctx context.Context, conn *websocket.Conn, limit uint64) (eventSubFrame, core.ChatReason) {
	// kind различает текстовый и двоичный кадры, data содержит ограниченный JSON, err сообщает об отказе чтения.
	kind, data, err := conn.Read(ctx)
	if errors.Is(err, websocket.ErrMessageTooBig) || uint64(len(data)) > limit {
		return eventSubFrame{}, core.ChatReasonLimit
	}
	// frame содержит разобранное сообщение с полями, необходимыми для определения события.
	var frame eventSubFrame
	if err != nil || kind != websocket.MessageText || json.Unmarshal(data, &frame) != nil ||
		!validEventSubID(frame.Metadata.ID) || frame.Metadata.Kind == "" || frame.Metadata.Timestamp.IsZero() || frame.Payload == nil {
		return eventSubFrame{}, core.ChatReasonNetwork
	}
	return frame, core.ChatReasonNone
}

// openEventSub открывает один сокет и сверяет первое приветствие Twitch.
// ctx ограничивает установление соединения и ожидание кадра. При ошибке сокет
// закрывается здесь, а при успехе его принимает вызывающий код.
func (a *Adapter) openEventSub(ctx context.Context, socketURL string) (*websocket.Conn, eventSubSession, eventSubFailure) {
	// conn принимает сокет, response даёт HTTP-статус отказа, err сообщает ошибку установки соединения.
	conn, response, err := websocket.Dial(ctx, socketURL, &websocket.DialOptions{HTTPClient: a.httpClient})
	if err != nil {
		if response != nil && (response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500) {
			return nil, eventSubSession{}, a.subscriptionFailure(response.StatusCode, response.Header)
		}
		return nil, eventSubSession{}, eventSubFailure{state: core.ChatRetrying, reason: core.ChatReasonNetwork}
	}
	// limit вмещает внутренний дополнительный байт счётчика websocket.SetReadLimit.
	limit := min(a.limits.UpstreamMessageBytes, uint64(math.MaxInt64-1))
	conn.SetReadLimit(int64(limit))
	// welcome содержит ожидаемое приветствие, reason сообщает об отказе чтения или разбора.
	welcome, reason := readEventSubFrame(ctx, conn, limit)
	if reason == core.ChatReasonNone && welcome.Metadata.Kind == "session_welcome" && welcome.Payload.Session != nil {
		// session содержит параметры, которым можно доверять после проверки приветствия.
		session := *welcome.Payload.Session
		if validEventSubID(session.ID) && session.Status == "connected" &&
			session.KeepaliveSeconds > 0 && session.KeepaliveSeconds <= maxKeepaliveSeconds && session.ReconnectURL == "" {
			session.welcomeAt = time.Now()
			return conn, session, eventSubFailure{}
		}
	}
	_ = conn.CloseNow()
	if reason == core.ChatReasonNone {
		reason = core.ChatReasonNetwork
	}
	return nil, eventSubSession{}, eventSubFailure{state: core.ChatRetrying, reason: reason}
}

// subscribeEventSub отправляет один запрос Helix для condition и sessionID.
// credentials содержит уже проверенную пару приложения и пользователя.
// Подтверждением считается единственный точный ответ 202 с включённой подпиской.
func (a *Adapter) subscribeEventSub(ctx context.Context, req core.ChatReadRequest, sessionID string, condition eventSubCondition, credentials *core.TwitchChatCredentials) (eventSubSubscription, eventSubFailure, int) {
	// expected задаёт единственный допустимый вид, условие и транспорт подписки.
	expected := eventSubSubscription{
		Type: chatSubscriptionType, Version: chatSubscriptionVersion, Condition: condition,
		Transport: eventSubSocketRef{Method: eventSubTransport, SessionID: sessionID},
	}
	// data содержит JSON без токена. err сообщает об отказе сериализации.
	data, err := json.Marshal(expected)
	if err != nil {
		return eventSubSubscription{}, eventSubFailure{state: core.ChatFailed, reason: core.ChatReasonConfiguration}, 0
	}
	// request отправляет JSON только на настроенный адрес Helix. err сообщает об ошибке запроса.
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.helixBase+"/helix/eventsub/subscriptions", bytes.NewReader(data))
	if err != nil {
		return eventSubSubscription{}, eventSubFailure{state: core.ChatFailed, reason: core.ChatReasonConfiguration}, 0
	}
	request.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
	request.Header.Set("Client-Id", credentials.ClientID)
	request.Header.Set("Content-Type", "application/json")
	if req.AcquireHTTP != nil {
		// release освобождает HTTP-слот, slotErr сообщает об отмене либо отказе допуска.
		release, slotErr := req.AcquireHTTP(ctx)
		if slotErr != nil || release == nil {
			return eventSubSubscription{}, eventSubFailure{state: core.ChatRetrying, reason: core.ChatReasonLimit}, 0
		}
		defer release()
	}
	// status и headers служат для классификации, body несёт результат, err сообщает об отказе транспорта.
	status, headers, body, err := a.do(request)
	if status != 0 && status != http.StatusAccepted {
		return eventSubSubscription{}, a.subscriptionFailure(status, headers), status
	}
	if err != nil {
		return eventSubSubscription{}, eventSubFailure{state: core.ChatRetrying, reason: core.ChatReasonNetwork}, 0
	}
	// response принимает ровно одну подписку, созданную запросом.
	var response struct {
		// Data содержит список подтверждённых записей Helix.
		Data []eventSubSubscription `json:"data"`
	}
	if json.Unmarshal(body, &response) != nil || len(response.Data) != 1 {
		return eventSubSubscription{}, eventSubFailure{state: core.ChatRetrying, reason: core.ChatReasonNetwork}, status
	}
	// confirmed содержит проверяемые поля возвращённой подписки.
	confirmed := response.Data[0]
	if !validEventSubID(confirmed.ID) || confirmed.Status != "enabled" || !confirmed.matches(expected) {
		return eventSubSubscription{}, eventSubFailure{state: core.ChatRetrying, reason: core.ChatReasonNetwork}, status
	}
	return confirmed, eventSubFailure{}, status
}

// matches связывает s с типом, версией, обоими пользователями и сокетом expected.
// Вызывающий отдельно сверяет идентификатор и состояние записи.
func (s eventSubSubscription) matches(expected eventSubSubscription) bool {
	return s.Type == expected.Type && s.Version == expected.Version && s.Condition == expected.Condition && s.Transport == expected.Transport
}

// subscriptionFailure переводит ответ HTTP в безопасный исход. При 401 и 403
// Read может уточнить действительность полномочий через владельца аккаунта.
func (a *Adapter) subscriptionFailure(status int, headers http.Header) eventSubFailure {
	switch status {
	case http.StatusUnauthorized:
		return eventSubFailure{state: core.ChatAuthRequired, reason: core.ChatReasonCredentials}
	case http.StatusForbidden, http.StatusNotFound:
		return eventSubFailure{state: core.ChatFailed, reason: core.ChatReasonUnavailable}
	case http.StatusGone:
		return eventSubFailure{state: core.ChatFailed, reason: core.ChatReasonConfiguration}
	case http.StatusTooManyRequests:
		// now задаёт одну точку отсчёта для обеих форм срока ожидания Twitch.
		now := a.now()
		// delay хранит больший допустимый срок из заголовков ответа.
		delay := parseRetryAfter(headers.Get("Retry-After"), now)
		// reset содержит время открытия лимита в Unix-секундах. err сообщает о непригодном заголовке.
		if reset, err := strconv.ParseInt(headers.Get("Ratelimit-Reset"), 10, 64); err == nil &&
			reset > now.Unix() && reset <= 253402300799 {
			delay = max(delay, time.Unix(reset, 0).Sub(now))
		}
		return eventSubFailure{state: core.ChatRetrying, reason: core.ChatReasonQuota, retryAfter: delay}
	default:
		if status >= 500 {
			return eventSubFailure{state: core.ChatRetrying, reason: core.ChatReasonNetwork,
				retryAfter: parseRetryAfter(headers.Get("Retry-After"), a.now())}
		}
		return eventSubFailure{state: core.ChatRetrying, reason: core.ChatReasonNetwork}
	}
}

// validEventSubID проверяет непустой непрозрачный идентификатор value до его
// сравнения или публикации. Формат UUID нужен Twitch, но здесь сравниваются байты.
func validEventSubID(value string) bool {
	return len(value) <= core.ChatIDBytes && strings.TrimSpace(value) != "" &&
		utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// trustedReconnectURL разрешает перенос на тот же WSS-сервер, что у исходного
// аутентичного сокета initial. Значение next передаётся в Dial без нормализации.
func trustedReconnectURL(initial, next string) bool {
	if len(next) == 0 || len(next) > reconnectURLBytes || strings.Contains(next, "#") {
		return false
	}
	// origin содержит фиксированную точку входа, err сообщает об ошибке её настройки.
	origin, err := url.Parse(initial)
	if err != nil {
		return false
	}
	// target содержит адрес из Twitch. err сообщает о синтаксической ошибке до любого запроса.
	target, err := url.Parse(next)
	if err != nil || target.Opaque != "" || target.User != nil || target.Hostname() == "" ||
		target.Scheme != origin.Scheme || !strings.EqualFold(target.Hostname(), origin.Hostname()) {
		return false
	}
	// defaultPort соответствует WSS либо частному WS-серверу протокольных тестов.
	defaultPort := "443"
	switch origin.Scheme {
	case "wss":
	case "ws":
		if !loopbackHTTPHost(origin.Hostname()) {
			return false
		}
		defaultPort = "80"
	default:
		return false
	}
	// originPort и targetPort различают два сетевых адреса на одном имени.
	originPort, targetPort := origin.Port(), target.Port()
	if originPort == "" {
		originPort = defaultPort
	}
	if targetPort == "" {
		targetPort = defaultPort
	}
	return targetPort == originPort
}
