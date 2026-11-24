// Package vkvideolive получает полномочия DevAPI и читает публичные чаты VK Видео Live.
package vkvideolive

import (
	"math"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

const (
	// oauthBase задаёт сервис выдачи токена приложения.
	oauthBase = "https://api.live.vkvideo.ru"
	// apiBase задаёт сервис проверки токена, обнаружения канала и получения токена соединения.
	apiBase = "https://apidev.live.vkvideo.ru"
	// socketURL задаёт точку входа Centrifugo с протоколом JSON версии 2.
	socketURL = "wss://pubsub.live.vkvideo.ru/connection/websocket?cf_protocol_version=v2"
	// socketOrigin содержит Origin, который VK ожидает при открытии сокета.
	socketOrigin = "https://vkvideo.ru"
	// responseHeaderBytes ограничивает заголовки независимо от тела ответа.
	responseHeaderBytes = 64 << 10
)

// Adapter проверяет полномочия приложения и открывает одну физическую подписку
// за вызов Read. Manager сохраняет полномочия, выдаёт сетевые слоты и планирует повторы.
type Adapter struct {
	// limits ограничивает длительность и размер запросов и сообщений.
	limits core.ChatLimits
	// oauthBase содержит адрес выдачи токена приложения.
	oauthBase string
	// apiBase содержит адрес сервиса DevAPI.
	apiBase string
	// socketURL содержит адрес единственного WebSocket текущей попытки.
	socketURL string
	// httpClient выполняет конечные запросы и HTTP Upgrade.
	httpClient *http.Client
	// now возвращает время для проверки срока полномочий и заголовка Retry-After.
	now func() time.Time
}

// New проверяет limits и создаёт адаптер с фиксированными адресами VK.
// Сетевые соединения открываются только при последующих вызовах.
func New(limits core.ChatLimits) (*Adapter, error) {
	if limits.Validate() != nil || limits.UpstreamHTTPBytes >= math.MaxInt64 || limits.UpstreamMessageBytes >= math.MaxInt64 {
		return nil, core.ErrInvalid
	}
	// dialer ограничивает установление TCP-соединения.
	dialer := net.Dialer{Timeout: limits.ConnectTimeout}
	// protocols разрешает HTTP/1.1 для Upgrade и конечных запросов.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	// transport ограничивает заголовки и TLS. Отключение повторного использования
	// соединений исключает повтор запроса при отказе устаревшего соединения.
	transport := &http.Transport{
		DialContext:            dialer.DialContext,
		TLSHandshakeTimeout:    limits.ConnectTimeout,
		ResponseHeaderTimeout:  limits.HTTPTimeout,
		MaxResponseHeaderBytes: responseHeaderBytes,
		DisableKeepAlives:      true,
		Protocols:              protocols,
	}
	return &Adapter{
		limits: limits, oauthBase: oauthBase, apiBase: apiBase, socketURL: socketURL, now: time.Now,
		httpClient: &http.Client{Transport: transport, CheckRedirect: refuseRedirect},
	}, nil
}

// Provider возвращает каталог платформы, под которым Manager регистрирует адаптер.
func (a *Adapter) Provider() core.ChatProvider { return core.ChatVKVideoLive }

// refuseRedirect оставляет исходный ответ клиенту. Аргументы нового запроса
// и истории перенаправлений не нужны, поскольку переход запрещён для всех адресов.
func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// validRequiredText проверяет предел в байтах и форму непустого value, сохраняя исходную строку.
func validRequiredText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, 0) && strings.TrimSpace(value) != ""
}
