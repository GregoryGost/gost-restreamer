package core

import (
	"math"
	"time"
)

// ChatBudget одновременно ограничивает количество и логический объём одной RAM-структуры.
type ChatBudget struct {
	// Count ограничивает число удерживаемых элементов независимо от размера строк.
	Count int `yaml:"count"`
	// Bytes ограничивает сумму encoded либо логических байтов согласно контракту структуры.
	Bytes uint64 `yaml:"bytes"`
}

// ChatLimits содержит конечные бюджеты Manager, Hub, адаптеров, API и браузерных страниц.
// Значения DefaultChatLimits — исходные инженерные пределы, а не измеренные оптимумы или SLA.
type ChatLimits struct {
	// Sessions ограничивает число удерживаемых лент Session.
	Sessions int `yaml:"sessions"`
	// Sources ограничивает общее число активных источников.
	Sources int `yaml:"sources"`
	// SourcesPerSession ограничивает активные источники одного запуска.
	SourcesPerSession int `yaml:"sources_per_session"`
	// Readers ограничивает число физических reader workers.
	Readers int `yaml:"readers"`
	// Connections включает ещё закрывающиеся соединения и overlap при Twitch handover.
	Connections int `yaml:"connections"`
	// AccountOwners ограничивает сериализующих владельцев полномочий.
	AccountOwners int `yaml:"account_owners"`
	// HTTPRequests ограничивает параллельные конечные запросы адаптеров.
	HTTPRequests int `yaml:"http_requests"`
	// Authorizations ограничивает RAM-попытки авторизации.
	Authorizations int `yaml:"authorizations"`
	// ReplacementsPerAccount ограничивает одновременные replacement-попытки одного аккаунта.
	ReplacementsPerAccount int `yaml:"replacements_per_account"`
	// Registry ограничивает число memberships и их логические байты.
	Registry ChatBudget `yaml:"registry"`
	// History ограничивает сообщения одной Session по количеству и полным encoded bytes.
	History ChatBudget `yaml:"history"`
	// MessageBytes ограничивает окончательный публичный JSON одного сообщения.
	MessageBytes uint64 `yaml:"message_bytes"`
	// InputQueue ограничивает входную очередь одной Session по количеству и логическим байтам.
	InputQueue ChatBudget `yaml:"input_queue"`
	// Replay ограничивает журнал одной Session по числу событий и encoded bytes.
	Replay ChatBudget `yaml:"replay"`
	// Dedupe ограничивает число ключей и логический объём дедупликации одной Session.
	Dedupe ChatBudget `yaml:"dedupe"`
	// DedupeTTL ограничивает время удержания ключа дополнительно к count/bytes.
	DedupeTTL time.Duration `yaml:"dedupe_ttl"`
	// SSEClients ограничивает общее число браузерных соединений.
	SSEClients int `yaml:"sse_clients"`
	// SSEClientsPerStream ограничивает браузерные соединения одного Stream.
	SSEClientsPerStream int `yaml:"sse_clients_per_stream"`
	// SSEQueue ограничивает очередь одного клиента по числу событий и логическим байтам.
	SSEQueue ChatBudget `yaml:"sse_queue"`
	// SSEFrameBytes ограничивает целый frame одного события, включая служебные строки.
	SSEFrameBytes uint64 `yaml:"sse_frame_bytes"`
	// CursorBytes ограничивает непрозрачный cursor до декодирования.
	CursorBytes uint64 `yaml:"cursor_bytes"`
	// UpstreamHTTPBytes ограничивает тело одного конечного upstream HTTP-ответа.
	UpstreamHTTPBytes uint64 `yaml:"upstream_http_bytes"`
	// UpstreamMessageBytes ограничивает отдельное WS/gRPC-сообщение, а не весь streaming-сеанс.
	UpstreamMessageBytes uint64 `yaml:"upstream_message_bytes"`
	// RESTBodyBytes ограничивает входное тело управляющего REST.
	RESTBodyBytes uint64 `yaml:"rest_body_bytes"`
	// CredentialBytes ограничивает полный серверный набор полномочий.
	CredentialBytes uint64 `yaml:"credential_bytes"`
	// SnapshotBytes ограничивает полный encoded snapshot независимо от upstream body-limit.
	SnapshotBytes uint64 `yaml:"snapshot_bytes"`
	// SnapshotMetadataBytes ограничивает metadata/status-часть snapshot.
	SnapshotMetadataBytes uint64 `yaml:"snapshot_metadata_bytes"`
	// Snapshots ограничивает число параллельно формируемых снимков.
	Snapshots int `yaml:"snapshots"`
	// FlushInterval ограничивает ожидание внутренней пакетизации Hub.
	FlushInterval time.Duration `yaml:"flush_interval"`
	// FlushMessages задаёт порог числа сообщений для немедленного flush.
	FlushMessages int `yaml:"flush_messages"`
	// FlushBytes задаёт порог encoded bytes для внутреннего flush.
	FlushBytes uint64 `yaml:"flush_bytes"`
	// ConnectTimeout ограничивает установление сетевого соединения.
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
	// HTTPTimeout ограничивает конечный HTTP-запрос, но не streaming RPC/socket.
	HTTPTimeout time.Duration `yaml:"http_timeout"`
	// SSEHeartbeat задаёт период heartbeat браузерного соединения.
	SSEHeartbeat time.Duration `yaml:"sse_heartbeat"`
	// SSEWriteTimeout ограничивает запись одного frame клиенту.
	SSEWriteTimeout time.Duration `yaml:"sse_write_timeout"`
	// ShutdownTimeout ограничивает остановку подсистемы перед закрытием Store.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// AuthorizationTTL ограничивает попытку дополнительно к более строгому сроку провайдера.
	AuthorizationTTL time.Duration `yaml:"authorization_ttl"`
	// ReconnectConcurrent ограничивает число текущих попыток одного reader.
	ReconnectConcurrent int `yaml:"reconnect_concurrent"`
	// ReconnectAttempts ограничивает новые попытки в скользящем окне reader.
	ReconnectAttempts int `yaml:"reconnect_attempts"`
	// ReconnectWindow задаёт длительность скользящего окна попыток.
	ReconnectWindow time.Duration `yaml:"reconnect_window"`
	// ReconnectMinBackoff задаёт нижнюю исходную задержку перед jitter.
	ReconnectMinBackoff time.Duration `yaml:"reconnect_min_backoff"`
	// ReconnectMaxBackoff ограничивает исходную задержку перед jitter.
	ReconnectMaxBackoff time.Duration `yaml:"reconnect_max_backoff"`
	// OverlayMessages ограничивает DOM сообщения страницы Overlay.
	OverlayMessages int `yaml:"overlay_messages"`
	// DashboardMessages ограничивает DOM сообщения страницы Dashboard.
	DashboardMessages int `yaml:"dashboard_messages"`
}

// DefaultChatLimits возвращает полный явный набор начальных пределов без запуска runtime-компонентов.
func DefaultChatLimits() ChatLimits {
	return ChatLimits{
		Sessions: 4, Sources: 64, SourcesPerSession: 32, Readers: 32, Connections: 64,
		AccountOwners: 32, HTTPRequests: 16, Authorizations: 8, ReplacementsPerAccount: 1,
		Registry: ChatBudget{Count: 256, Bytes: 8 << 20}, History: ChatBudget{Count: 1000, Bytes: 16 << 20},
		MessageBytes: ChatMessageBytes, InputQueue: ChatBudget{Count: 256, Bytes: 1 << 20},
		Replay: ChatBudget{Count: 2048, Bytes: 16 << 20}, Dedupe: ChatBudget{Count: 8192, Bytes: 2 << 20}, DedupeTTL: 15 * time.Minute,
		SSEClients: 32, SSEClientsPerStream: 8, SSEQueue: ChatBudget{Count: 256, Bytes: 1 << 20}, SSEFrameBytes: 128 << 10, CursorBytes: 4 << 10,
		UpstreamHTTPBytes: 1 << 20, UpstreamMessageBytes: 1 << 20, RESTBodyBytes: 1 << 20, CredentialBytes: ChatCredentialBytes,
		SnapshotBytes: 20 << 20, SnapshotMetadataBytes: 1 << 20, Snapshots: 4,
		FlushInterval: 50 * time.Millisecond, FlushMessages: 100, FlushBytes: 128 << 10,
		ConnectTimeout: 10 * time.Second, HTTPTimeout: 30 * time.Second, SSEHeartbeat: 15 * time.Second, SSEWriteTimeout: 5 * time.Second, ShutdownTimeout: 10 * time.Second,
		AuthorizationTTL: 10 * time.Minute, ReconnectConcurrent: 1, ReconnectAttempts: 8, ReconnectWindow: 5 * time.Minute, ReconnectMinBackoff: time.Second, ReconnectMaxBackoff: time.Minute,
		OverlayMessages: 50, DashboardMessages: 200,
	}
}

// Validate проверяет положительность бюджетов, согласованность вложенных пределов и размеры,
// используемые адаптерами для чтения. Пределы очереди SSE и числа сообщений в браузере жёсткие.
func (l ChatLimits) Validate() error {
	// count содержит очередной обязательный положительный счётчик ресурса.
	for _, count := range []int{l.Sessions, l.Sources, l.SourcesPerSession, l.Readers, l.Connections, l.AccountOwners, l.HTTPRequests, l.Authorizations, l.ReplacementsPerAccount, l.SSEClients, l.SSEClientsPerStream, l.Snapshots, l.FlushMessages, l.ReconnectConcurrent, l.ReconnectAttempts, l.OverlayMessages, l.DashboardMessages} {
		if count <= 0 {
			return ErrInvalid
		}
	}
	// budget содержит очередную пару count/bytes независимой ограниченной структуры.
	for _, budget := range []ChatBudget{l.Registry, l.History, l.InputQueue, l.Replay, l.Dedupe, l.SSEQueue} {
		if budget.Count <= 0 || budget.Bytes == 0 {
			return ErrInvalid
		}
	}
	// size содержит очередной обязательный байтовый предел.
	for _, size := range []uint64{l.MessageBytes, l.SSEFrameBytes, l.CursorBytes, l.UpstreamHTTPBytes, l.UpstreamMessageBytes, l.RESTBodyBytes, l.CredentialBytes, l.SnapshotBytes, l.SnapshotMetadataBytes, l.FlushBytes} {
		if size == 0 {
			return ErrInvalid
		}
	}
	// duration содержит очередной конечный положительный срок.
	for _, duration := range []time.Duration{l.DedupeTTL, l.FlushInterval, l.ConnectTimeout, l.HTTPTimeout, l.SSEHeartbeat, l.SSEWriteTimeout, l.ShutdownTimeout, l.AuthorizationTTL, l.ReconnectWindow, l.ReconnectMinBackoff, l.ReconnectMaxBackoff} {
		if duration <= 0 {
			return ErrInvalid
		}
	}
	if l.SourcesPerSession > l.Sources || l.Readers > l.Connections || l.SSEClientsPerStream > l.SSEClients || l.SnapshotMetadataBytes > l.SnapshotBytes || l.ReconnectMinBackoff > l.ReconnectMaxBackoff || l.ReplacementsPerAccount != 1 || l.ReconnectConcurrent != 1 {
		return ErrInvalid
	}
	if l.SSEQueue.Count > 256 || l.OverlayMessages > 50 || l.DashboardMessages > 200 ||
		l.UpstreamHTTPBytes >= math.MaxInt64 || l.UpstreamMessageBytes >= math.MaxInt {
		return ErrInvalid
	}
	return nil
}
