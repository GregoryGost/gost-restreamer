package core

import "time"

// ChatBudget одновременно ограничивает количество и логический объём одной RAM-структуры.
type ChatBudget struct {
	// Count ограничивает число удерживаемых элементов независимо от размера строк.
	Count int
	// Bytes ограничивает сумму encoded либо логических байтов согласно контракту структуры.
	Bytes uint64
}

// ChatLimits содержит явные конечные бюджеты будущих Manager/Hub/адаптеров/API.
// Значения DefaultChatLimits — исходные инженерные пределы, а не измеренные оптимумы или SLA.
type ChatLimits struct {
	// Sessions ограничивает число удерживаемых лент Session.
	Sessions int
	// Sources ограничивает общее число активных источников.
	Sources int
	// SourcesPerSession ограничивает активные источники одного запуска.
	SourcesPerSession int
	// Readers ограничивает число физических reader workers.
	Readers int
	// Connections включает ещё закрывающиеся соединения и overlap при Twitch handover.
	Connections int
	// AccountOwners ограничивает сериализующих владельцев полномочий.
	AccountOwners int
	// HTTPRequests ограничивает параллельные конечные запросы адаптеров.
	HTTPRequests int
	// Authorizations ограничивает RAM-попытки авторизации.
	Authorizations int
	// ReplacementsPerAccount ограничивает одновременные replacement-попытки одного аккаунта.
	ReplacementsPerAccount int
	// Registry ограничивает число memberships и их логические байты.
	Registry ChatBudget
	// History ограничивает сообщения одной Session по количеству и полным encoded bytes.
	History ChatBudget
	// MessageBytes ограничивает окончательный публичный JSON одного сообщения.
	MessageBytes uint64
	// InputQueue ограничивает входную очередь одной Session по количеству и логическим байтам.
	InputQueue ChatBudget
	// Replay ограничивает журнал одной Session по числу событий и encoded bytes.
	Replay ChatBudget
	// Dedupe ограничивает число ключей и логический объём дедупликации одной Session.
	Dedupe ChatBudget
	// DedupeTTL ограничивает время удержания ключа дополнительно к count/bytes.
	DedupeTTL time.Duration
	// SSEClients ограничивает общее число браузерных соединений.
	SSEClients int
	// SSEClientsPerStream ограничивает браузерные соединения одного Stream.
	SSEClientsPerStream int
	// SSEQueue ограничивает очередь одного клиента по числу событий и логическим байтам.
	SSEQueue ChatBudget
	// SSEFrameBytes ограничивает целый frame одного события, включая служебные строки.
	SSEFrameBytes uint64
	// CursorBytes ограничивает непрозрачный cursor до декодирования.
	CursorBytes uint64
	// UpstreamHTTPBytes ограничивает тело одного конечного upstream HTTP-ответа.
	UpstreamHTTPBytes uint64
	// UpstreamMessageBytes ограничивает отдельное WS/gRPC-сообщение, а не весь streaming-сеанс.
	UpstreamMessageBytes uint64
	// RESTBodyBytes ограничивает входное тело управляющего REST.
	RESTBodyBytes uint64
	// CredentialBytes ограничивает полный серверный набор полномочий.
	CredentialBytes uint64
	// SnapshotBytes ограничивает полный encoded snapshot независимо от upstream body-limit.
	SnapshotBytes uint64
	// SnapshotMetadataBytes ограничивает metadata/status-часть snapshot.
	SnapshotMetadataBytes uint64
	// Snapshots ограничивает число параллельно формируемых снимков.
	Snapshots int
	// FlushInterval ограничивает ожидание внутренней пакетизации Hub.
	FlushInterval time.Duration
	// FlushMessages задаёт порог числа сообщений для немедленного flush.
	FlushMessages int
	// FlushBytes задаёт порог encoded bytes для внутреннего flush.
	FlushBytes uint64
	// ConnectTimeout ограничивает установление сетевого соединения.
	ConnectTimeout time.Duration
	// HTTPTimeout ограничивает конечный HTTP-запрос, но не streaming RPC/socket.
	HTTPTimeout time.Duration
	// SSEHeartbeat задаёт период heartbeat браузерного соединения.
	SSEHeartbeat time.Duration
	// SSEWriteTimeout ограничивает запись одного frame клиенту.
	SSEWriteTimeout time.Duration
	// ShutdownTimeout ограничивает остановку подсистемы перед закрытием Store.
	ShutdownTimeout time.Duration
	// AuthorizationTTL ограничивает попытку дополнительно к более строгому сроку провайдера.
	AuthorizationTTL time.Duration
	// ReconnectConcurrent ограничивает число текущих попыток одного reader.
	ReconnectConcurrent int
	// ReconnectAttempts ограничивает новые попытки в скользящем окне reader.
	ReconnectAttempts int
	// ReconnectWindow задаёт длительность скользящего окна попыток.
	ReconnectWindow time.Duration
	// ReconnectMinBackoff задаёт нижнюю исходную задержку перед jitter.
	ReconnectMinBackoff time.Duration
	// ReconnectMaxBackoff ограничивает исходную задержку перед jitter.
	ReconnectMaxBackoff time.Duration
	// OverlayMessages ограничивает DOM сообщения страницы Overlay.
	OverlayMessages int
	// DashboardMessages ограничивает DOM сообщения страницы Dashboard.
	DashboardMessages int
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

// Validate отклоняет нулевые/отрицательные бюджеты и противоречивые вложенные пределы без defaults.
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
	return nil
}
