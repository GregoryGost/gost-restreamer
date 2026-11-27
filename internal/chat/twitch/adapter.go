// Package twitch получает пользовательские полномочия и читает чаты Twitch через EventSub.
package twitch

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

const (
	// twitchOAuthBase адресует выдачу и проверку полномочий Twitch.
	twitchOAuthBase = "https://id.twitch.tv"
	// twitchHelixBase адресует операции подписки EventSub.
	twitchHelixBase = "https://api.twitch.tv"
	// twitchSocketURL задаёт начальный адрес подключения EventSub.
	twitchSocketURL = "wss://eventsub.wss.twitch.tv/ws"
	// twitchHeaderBytes ограничивает служебные поля HTTP-ответа отдельно от тела.
	twitchHeaderBytes = 64 << 10
	// twitchValidationInterval задаёт обязательную частоту проверки действующих OAuth-сеансов.
	twitchValidationInterval = time.Hour
)

// Adapter выполняет запросы OAuth и Helix и открывает соединения EventSub.
// Допуск к сетевым операциям и сохранение полномочий координирует вызывающий код.
type Adapter struct {
	// limits задаёт верхние границы времени и размера ответа для сетевых операций.
	limits core.ChatLimits
	// httpClient выполняет конечные запросы и начальное HTTP-рукопожатие WebSocket.
	httpClient *http.Client
	// oauthBase адресует единственный сервис выдачи и проверки токенов.
	oauthBase string
	// helixBase адресует единственный сервис управления подписками.
	helixBase string
	// socketURL содержит фиксированный начальный URL чтения чата.
	socketURL string
	// now возвращает время для расчёта срока токена и задержки площадки.
	now func() time.Time
	// validationInterval определяет период проверки полномочий на открытом соединении.
	validationInterval time.Duration
}

// New проверяет limits и создаёт адаптер с фиксированными адресами Twitch.
// Создание не открывает соединений. Клиент ограничивает заголовки и завершает
// конечные запросы без перенаправлений или повторного использования соединения.
func New(limits core.ChatLimits) (*Adapter, error) {
	if limits.Validate() != nil || limits.UpstreamHTTPBytes >= math.MaxInt64 {
		return nil, core.ErrInvalid
	}
	// dialer ограничивает установление нового TCP-соединения.
	dialer := net.Dialer{Timeout: limits.ConnectTimeout}
	// protocols оставляет транспорт на HTTP/1.1 для EventSub Upgrade и конечных запросов.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	// transport ограничивает рукопожатие и заголовки. Новое соединение для каждого
	// запроса исключает транспортный повтор из-за устаревшего соединения в пуле.
	transport := &http.Transport{
		DialContext:            dialer.DialContext,
		TLSHandshakeTimeout:    limits.ConnectTimeout,
		ResponseHeaderTimeout:  limits.HTTPTimeout,
		MaxResponseHeaderBytes: twitchHeaderBytes,
		DisableKeepAlives:      true,
		Protocols:              protocols,
	}
	// client ограничивает конечные запросы и сохраняет их исходный адрес.
	client := &http.Client{
		Transport:     transport,
		Timeout:       limits.HTTPTimeout,
		CheckRedirect: refuseRedirect,
	}
	return &Adapter{
		limits: limits, httpClient: client, oauthBase: twitchOAuthBase, helixBase: twitchHelixBase,
		socketURL: twitchSocketURL, now: time.Now, validationInterval: twitchValidationInterval,
	}, nil
}

// Provider возвращает платформу, по которой Manager регистрирует адаптер.
func (a *Adapter) Provider() core.ChatProvider { return core.ChatTwitch }

// do выполняет один конечный запрос req к OAuth или Helix и закрывает тело ответа.
// Статус и заголовки доступны после получения ответа, даже если тело прочитано
// не полностью. Отмена вызывающего возвращается как ошибка контекста. Сбой
// транспорта и непригодное тело возвращают ErrNotReady. Таймаут транспорта
// дополнительно различим как DeadlineExceeded для увеличения интервала опроса.
func (a *Adapter) do(req *http.Request) (int, http.Header, []byte, error) {
	if req == nil || !a.allowedRESTRequest(req) {
		return 0, nil, nil, core.ErrInvalid
	}
	// ctx сохраняет срок вызывающего для отличия от локального таймаута HTTP.
	ctx := req.Context()
	// err показывает, что запрос отменён до отправки.
	if err := ctx.Err(); err != nil {
		return 0, nil, nil, err
	}
	// requestCtx включает чтение тела в единый срок, cancel освобождает таймер.
	requestCtx, cancel := context.WithTimeout(ctx, a.limits.HTTPTimeout)
	defer cancel()
	// client сохраняет проверку перенаправлений и с транспортом, подменённым в тесте.
	client := *a.httpClient
	client.CheckRedirect = refuseRedirect
	// response содержит статус и открытое тело Twitch, err остаётся внутри
	// адаптера, поскольку транспортная ошибка может содержать данные запроса.
	response, err := client.Do(req.WithContext(requestCtx))
	if err != nil {
		return 0, nil, nil, restTransportError(ctx, requestCtx, err)
	}
	// Callback закрывает тело даже при отмене и превышении байтового предела.
	defer func() { _ = response.Body.Close() }()

	// maximum ограничивает полезную нагрузку. Дополнительный байт отличает
	// точное попадание в предел от слишком большого ответа.
	maximum := int64(a.limits.UpstreamHTTPBytes)
	// body содержит прочитанные байты, err сообщает, удалось ли дочитать ответ.
	body, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil || requestCtx.Err() != nil {
		return response.StatusCode, response.Header, nil, restTransportError(ctx, requestCtx, err)
	}
	if int64(len(body)) > maximum || !utf8.Valid(body) {
		return response.StatusCode, response.Header, nil, core.ErrNotReady
	}
	return response.StatusCode, response.Header, body, nil
}

// refuseRedirect оставляет исходный HTTP-ответ и открытое тело вызывающему.
// Клиент не использует следующий запрос и историю перенаправления.
func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// restTransportError оставляет отмену вызывающего ctx и безопасные причины
// отказа. requestCtx и transportErr позволяют распознать отдельный таймаут
// соединения или чтения, не раскрывая исходную ошибку транспорта.
func restTransportError(ctx, requestCtx context.Context, transportErr error) error {
	// contextErr отражает отмену или истечение срока вызывающего.
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	// networkErr позволяет различить сетевой таймаут, в том числе на TLS-рукопожатии.
	var networkErr net.Error
	if requestCtx.Err() == context.DeadlineExceeded || errors.Is(transportErr, context.DeadlineExceeded) ||
		errors.As(transportErr, &networkErr) && networkErr.Timeout() {
		return errors.Join(core.ErrNotReady, context.DeadlineExceeded)
	}
	return core.ErrNotReady
}

// allowedRESTRequest проверяет адрес req перед передачей ему полномочий.
// Адрес должен принадлежать одному из двух HTTP-сервисов этого адаптера.
func (a *Adapter) allowedRESTRequest(req *http.Request) bool {
	// target содержит проверяемые сетевой адрес и путь исходящего запроса.
	target := req.URL
	if target == nil || target.Host == "" || target.User != nil || target.Opaque != "" ||
		target.Fragment != "" || req.Host != "" && req.Host != target.Host {
		return false
	}
	// origin содержит настроенный адрес очередного Twitch-сервиса.
	for _, origin := range [...]string{a.oauthBase, a.helixBase} {
		// base содержит схему и адрес сервиса, err сообщает о непригодном URL.
		base, err := url.Parse(origin)
		if err == nil && target.Scheme == base.Scheme && target.Host == base.Host &&
			(target.Scheme == "https" || target.Scheme == "http" && loopbackHTTPHost(target.Hostname())) {
			return true
		}
	}
	return false
}

// loopbackHTTPHost разрешает незашифрованный HTTP на локальном адресе
// из подменённого внутри пакета транспорта для изолированных тестов.
func loopbackHTTPHost(host string) bool {
	// ip содержит числовой адрес и позволяет проверить принадлежность loopback.
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

// parseRetryAfter разбирает интервал в секундах или HTTP-дату относительно now.
// Прошедшая дата и неподдерживаемая форма дают ноль. Чрезмерный интервал
// ограничивается максимальной длительностью, представимой в секундах.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	// seconds содержит ожидание в секундах, err отличает число от даты.
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		return time.Duration(min(seconds, uint64(math.MaxInt64/int64(time.Second)))) * time.Second
	}
	// until задаёт абсолютное время повторной попытки, err сообщает о неверной дате.
	if until, err := http.ParseTime(value); err == nil {
		return max(until.Sub(now), 0)
	}
	return 0
}
