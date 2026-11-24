package core

import (
	"context"
	"time"
)

// ChatSourceState задаёт публичное оперативное состояние без диагностического текста платформы.
type ChatSourceState string

const (
	// ChatDisabled означает явное отключение конфигурацией.
	ChatDisabled ChatSourceState = "DISABLED"
	// ChatWaitingOutput означает отсутствие подходящего фактически публиковавшего выхода.
	ChatWaitingOutput ChatSourceState = "WAITING_OUTPUT"
	// ChatDiscovering означает поиск канала либо текущего эфира.
	ChatDiscovering ChatSourceState = "DISCOVERING"
	// ChatWaitingChat означает ожидание доступности чата площадки.
	ChatWaitingChat ChatSourceState = "WAITING_CHAT"
	// ChatConnecting означает установление и подтверждение upstream-подписки.
	ChatConnecting ChatSourceState = "CONNECTING"
	// ChatConnected означает подтверждённую площадкой подписку независимо от активности аудитории.
	ChatConnected ChatSourceState = "CONNECTED"
	// ChatRetrying означает отменяемое ожидание после временного отказа либо исчерпания окна попыток.
	ChatRetrying ChatSourceState = "RETRYING"
	// ChatAuthRequired требует новой успешной авторизации до чтения.
	ChatAuthRequired ChatSourceState = "AUTH_REQUIRED"
	// ChatFailed означает подтверждённую невосстановимую ошибку либо неверную настройку.
	ChatFailed ChatSourceState = "FAILED"
	// ChatStopped означает остановку соответствующей Session/Output-привязки.
	ChatStopped ChatSourceState = "STOPPED"
)

// Validate разрешает закрытый каталог наблюдаемых состояний чата.
func (s ChatSourceState) Validate() error {
	switch s {
	case ChatDisabled, ChatWaitingOutput, ChatDiscovering, ChatWaitingChat, ChatConnecting, ChatConnected, ChatRetrying, ChatAuthRequired, ChatFailed, ChatStopped:
		return nil
	default:
		return ErrInvalid
	}
}

// ChatReason передаёт безопасную причину состояния без ключей, токенов и текста upstream-ошибки.
type ChatReason string

const (
	// ChatReasonNone означает отсутствие дополнительной причины.
	ChatReasonNone ChatReason = ""
	// ChatReasonNoBroadcast сообщает об отсутствии текущего эфира.
	ChatReasonNoBroadcast ChatReason = "no_broadcast"
	// ChatReasonSelectionRequired требует явного выбора из нескольких обнаруженных эфиров.
	ChatReasonSelectionRequired ChatReason = "selection_required"
	// ChatReasonUnavailable не различает disabled/ended без подтверждающих метаданных площадки.
	ChatReasonUnavailable ChatReason = "chat_unavailable"
	// ChatReasonEnded означает подтверждённое завершение чата.
	ChatReasonEnded ChatReason = "chat_ended"
	// ChatReasonDisabled означает подтверждённое отключение чата на площадке.
	ChatReasonDisabled ChatReason = "chat_disabled"
	// ChatReasonCredentials сообщает о необходимости повторной авторизации.
	ChatReasonCredentials ChatReason = "credentials_required"
	// ChatReasonNetwork сообщает о временной недоступности транспорта.
	ChatReasonNetwork ChatReason = "network"
	// ChatReasonQuota сообщает об ограничении площадки.
	ChatReasonQuota ChatReason = "quota"
	// ChatReasonConfiguration сообщает о подтверждённой ошибке настройки.
	ChatReasonConfiguration ChatReason = "configuration"
	// ChatReasonLimit сообщает об исчерпании локального конечного бюджета.
	ChatReasonLimit ChatReason = "limit"
	// ChatReasonUpstreamGap сообщает о возможной потере сообщений площадки.
	ChatReasonUpstreamGap ChatReason = "upstream_gap"
)

// Validate отклоняет произвольный текст вместо безопасной типизированной причины.
func (r ChatReason) Validate() error {
	switch r {
	case ChatReasonNone, ChatReasonNoBroadcast, ChatReasonSelectionRequired, ChatReasonUnavailable, ChatReasonEnded, ChatReasonDisabled, ChatReasonCredentials, ChatReasonNetwork, ChatReasonQuota, ChatReasonConfiguration, ChatReasonLimit, ChatReasonUpstreamGap:
		return nil
	default:
		return ErrInvalid
	}
}

// ChatSourceStatus хранится только в RAM и публикуется отдельно от сообщений аудитории.
type ChatSourceStatus struct {
	// SourceID адресует конфигурационный источник.
	SourceID ID
	// State задаёт текущее оперативное состояние.
	State ChatSourceState
	// Reason содержит безопасный код без платформенных полномочий.
	Reason ChatReason
	// ChangedAt содержит время последнего изменения состояния в UTC.
	ChangedAt time.Time
	// LastEventAt содержит необязательное время последнего успешного события площадки.
	LastEventAt *time.Time
	// BroadcastAt содержит необязательную дату обнаруженного эфира.
	BroadcastAt *time.Time
	// Dropped считает локально отброшенные сообщения с точностью uint64.
	Dropped uint64
}

// Validate проверяет публичный статус и UTC-метки, не читая сохранённые полномочия.
func (s ChatSourceStatus) Validate() error {
	if chatID(s.SourceID) != nil || s.State.Validate() != nil || s.Reason.Validate() != nil || chatTime(s.ChangedAt) != nil {
		return ErrInvalid
	}
	// timestamp содержит очередную необязательную метку подтверждённого события либо эфира.
	for _, timestamp := range []*time.Time{s.LastEventAt, s.BroadcastAt} {
		if timestamp != nil && chatTime(*timestamp) != nil {
			return ErrInvalid
		}
	}
	return nil
}

// ChatEventKind задаёт отдельный от медиамониторинга поток событий чатов.
type ChatEventKind string

const (
	// ChatEventMessage переносит одно принятое сообщение, а не внутренний пакет flush.
	ChatEventMessage ChatEventKind = "chat.message"
	// ChatEventSourceStatus переносит оперативное состояние отдельно от ленты аудитории.
	ChatEventSourceStatus ChatEventKind = "chat.source_status"
	// ChatEventGap сообщает об утрате входа, включая возможный upstream-пробел.
	ChatEventGap ChatEventKind = "chat.gap"
	// ChatEventResync требует нового согласованного snapshot.
	ChatEventResync ChatEventKind = "resync_required"
	// ChatEventSessionClosed закрывает ленту точной Session.
	ChatEventSessionClosed ChatEventKind = "chat.session_closed"
)

// ChatCursor остаётся непрозрачным для потребителей, включая браузер.
type ChatCursor string

// ChatEvent задаёт типизированные варианты RAM-событий для будущей SSE-проекции.
type ChatEvent struct {
	// Kind определяет единственный применимый вариант события.
	Kind ChatEventKind
	// Cursor содержит позицию события в эпохе точной Session либо Stream cursor до неё.
	Cursor ChatCursor
	// Message содержит одно сообщение для chat.message.
	Message *ChatMessage
	// Status содержит публичное состояние для chat.source_status.
	Status *ChatSourceStatus
	// SessionID адресует точный запуск события, если он уже существует.
	SessionID ID
	// Lost содержит число известных локальных потерь для chat.gap; upstream-пробел может быть неизвестен.
	Lost uint64
	// Reason содержит безопасную причину gap/resync/закрытия.
	Reason ChatReason
}

// Validate проверяет закрытый вариант события и непустую ограниченную позицию его выдачи.
func (e ChatEvent) Validate() error {
	if chatText(string(e.Cursor), 4<<10, true) != nil || e.Reason.Validate() != nil {
		return ErrInvalid
	}
	switch e.Kind {
	case ChatEventMessage:
		if e.Message == nil || e.Status != nil || e.Message.Validate() != nil || e.SessionID != e.Message.SessionID || e.Lost != 0 {
			return ErrInvalid
		}
	case ChatEventSourceStatus:
		if e.Status == nil || e.Message != nil || e.Status.Validate() != nil || e.Lost != 0 {
			return ErrInvalid
		}
	case ChatEventGap:
		if e.Message != nil || e.Status != nil || chatID(e.SessionID) != nil {
			return ErrInvalid
		}
	case ChatEventSessionClosed:
		if e.Message != nil || e.Status != nil || e.Lost != 0 || chatID(e.SessionID) != nil {
			return ErrInvalid
		}
	case ChatEventResync:
		if e.Message != nil || e.Status != nil || e.Lost != 0 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	if e.SessionID != "" && chatID(e.SessionID) != nil {
		return ErrInvalid
	}
	return nil
}

// ChatSnapshot представляет согласованный ответ даже до первого запуска существующего Stream.
type ChatSnapshot struct {
	// StreamID адресует существующий поток; неизвестный Stream возвращает ErrNotFound.
	StreamID ID
	// Session равно nil до активной Session, при этом Messages пуст, а настройки доступны.
	Session *RuntimeSessionFence
	// Sources содержит текущие конфигурации без полномочий.
	Sources []ChatSource
	// Statuses содержит отдельные оперативные состояния источников.
	Statuses []ChatSourceStatus
	// Messages содержит согласованный ограниченный хвост ленты.
	Messages []ChatMessage
	// Cursor соответствует снимку и используется без числового преобразования.
	Cursor ChatCursor
}

// ChatEventPage содержит ограниченный replay и сигнал для ожидания следующего изменения.
type ChatEventPage struct {
	// Events содержит события после исходного cursor в серверном порядке.
	Events []ChatEvent
	// Cursor задаёт позицию продолжения после страницы.
	Cursor ChatCursor
	// ResyncRequired сообщает, что для продолжения нужен новый согласованный снимок.
	ResyncRequired bool
	// Changed закрывается при следующем изменении и не требует отдельного pre-session Hub.
	Changed <-chan struct{}
}

// ChatWorkerToken идентифицирует точного RAM-worker; он не равен durable config/credential revisions.
type ChatWorkerToken [32]byte

// ChatWorkerKey задаёт совместное чтение только внутри одной точной Session и одного аккаунта.
type ChatWorkerKey struct {
	// Session различает SessionID/Incarnation/Lifecycle владельца чтения.
	Session RuntimeSessionFence
	// Provider задаёт платформу общего upstream-reader.
	Provider ChatProvider
	// AccountID закрепляет серверного владельца полномочий.
	AccountID ID
	// ChannelID содержит фактический платформенный chat ID, а не постоянный hint.
	ChannelID string
}

// ChatBroadcastSelection защищает RAM-выбор эфира от устаревшей страницы и нового discovery.
type ChatBroadcastSelection struct {
	// Session задаёт текущий запуск; новая Session выполняет новое обнаружение.
	Session RuntimeSessionFence
	// SourceID адресует источник YouTube.
	SourceID ID
	// SourceRevision сверяется с актуальной конфигурацией.
	SourceRevision uint64
	// DiscoveryGeneration сверяется с текущим RAM-набором найденных эфиров.
	DiscoveryGeneration uint64
	// VideoID должен принадлежать текущему набору и не сохраняется в Options.
	VideoID string
}

// ChatApplyResult подтверждает применение сохранённой конфигурации и исключение старых callbacks.
type ChatApplyResult struct {
	// Revision содержит применённую config revision, отдельно от credential revision.
	Revision uint64
	// Reconnected показывает замену чтения, в отличие от rename либо обычного refresh.
	Reconnected bool
}

// ChatManagerPort определяет будущего владельца привязок Sources/Executions и точных workers.
// Он начинает чтение после PUBLISHING, сверяет RuntimeAnchor/RuntimeEvents после Gap/Reset,
// сохраняет его при последующих RETRYING/fallback/pause и завершает только отозванные привязки.
type ChatManagerPort interface {
	// ApplySource применяет уже подтверждённую config revision; ErrStale означает её замену.
	// Успех исключает дальнейшую публикацию callbacks прежней конфигурации этого Source.
	ApplySource(context.Context, ID, uint64) (ChatApplyResult, error)
	// ApplyAccount применяет подтверждённый reconnect аккаунта; metadata/refresh не требуют перезапуска.
	ApplyAccount(context.Context, ID, uint64) (ChatApplyResult, error)
	// SelectBroadcast принимает только текущие Session/Source/discovery/video, иначе ErrStale.
	SelectBroadcast(context.Context, ChatBroadcastSelection) error
	// Close отменяет workers и ограниченно ожидает их завершения до закрытия Store.
	Close(context.Context) error
	// Done закрывается после физического завершения всех принадлежащих Manager работ.
	Done() <-chan struct{}
}

// ChatHubPort задаёт будущие согласованные snapshot/replay без SQL-сохранения сообщений.
type ChatHubPort interface {
	// Snapshot возвращает пустую ленту при отсутствии активной Session существующего Stream.
	Snapshot(context.Context, ID) (ChatSnapshot, error)
	// Events читает ограниченный replay либо resync и допускает ожидание запуска Session.
	Events(context.Context, ID, ChatCursor, int) (ChatEventPage, error)
}

// ChatAdapterSink принимает данные лишь после проверки точного непрозрачного токена Manager/Hub.
// Адаптер не назначает SessionID, ChatSourceID, seq и received_at.
type ChatAdapterSink interface {
	// Message передаёт нормализованное сообщение; отозванный token возвращает ErrStale.
	Message(context.Context, ChatWorkerToken, ChatIncomingMessage) error
	// Status передаёт безопасное состояние точного worker без доверенных Session/Source-полей.
	Status(context.Context, ChatWorkerToken, ChatSourceState, ChatReason) error
	// Gap сообщает о возможной upstream-потере отдельно от локального replay.
	Gap(context.Context, ChatWorkerToken, ChatReason) error
}

// ChatReadRequest задаёт полномочия и найденный канал одного отменяемого upstream-reader.
type ChatReadRequest struct {
	// Token принадлежит Manager и используется без изменения для каждого callback.
	Token ChatWorkerToken
	// Key задаёт точную Session/provider/account/chat идентичность общего reader.
	Key ChatWorkerKey
	// Credentials содержит серверный полный набор и никогда не публикуется в событиях.
	Credentials ChatCredentials
	// ResumeToken содержит позицию последнего обработанного пакета того же логического читателя.
	ResumeToken string
	// RefreshCredentials повторно проверяет полномочия того же аккаунта в пределах текущего чтения.
	// ErrInvalid означает, что продолжить работу можно после повторной авторизации.
	RefreshCredentials func(context.Context) (ChatCredentials, error)
	// AcquireConnection резервирует дополнительное физическое соединение до его закрытия.
	// Основное соединение уже учтено Manager на всё время вызова Read.
	AcquireConnection func(context.Context) (func(), error)
	// AcquireHTTP резервирует место для конечного запроса адаптера. Возвращённая функция освобождает место.
	AcquireHTTP func(context.Context) (func(), error)
}

// ChatResumeTokenBytes ограничивает позицию площадки, удерживаемую между попытками чтения.
const ChatResumeTokenBytes = 4 << 10

// ChatReadRetry передаёт Manager позицию и минимальную задержку следующей физической попытки.
// Адаптер сообщает причину через ChatAdapterSink, а Manager сохраняет свои ограничения повторов.
type ChatReadRetry struct {
	// ResumeToken содержит полную позицию продолжения, пустое значение начинает чтение с начала.
	ResumeToken string
	// RetryAfter задаёт минимальную задержку площадки, ноль оставляет задержку Manager.
	RetryAfter time.Duration
}

// Error возвращает безопасное описание повтора без позиции и диагностических данных площадки.
func (r *ChatReadRetry) Error() string {
	return "требуется повторная попытка чтения чата"
}

// ChatAdapterPort определяет единственный выбранный протокол чтения конкретного провайдера.
type ChatAdapterPort interface {
	// Provider возвращает одну платформу этого адаптера.
	Provider() ChatProvider
	// Discover разрешает постоянный ChannelHint и возвращает найденный чат либо текущие эфиры.
	// Сеть выполняется вне Write; Provider и Credentials должны соответствовать Source.
	Discover(context.Context, ChatSource, ChatCredentials) (ChatDiscovery, error)
	// Read работает до отмены ctx, подтверждает подписку через sink и освобождает соединения до возврата.
	Read(context.Context, ChatReadRequest, ChatAdapterSink) error
}

// ChatBroadcastResolver повторно проверяет выбранное видео без поиска других эфиров.
// Необязательный порт позволяет сохранять точный выбор, пока чат видео ещё недоступен.
type ChatBroadcastResolver interface {
	// DiscoverBroadcast сверяет videoID с текущим каналом. Активное видео возвращается в Broadcasts
	// даже без chat ID. Отсутствующее или завершённое видео возвращает безопасную причину без кандидата.
	DiscoverBroadcast(ctx context.Context, source ChatSource, credentials ChatCredentials, videoID string) (ChatDiscovery, error)
}

// ChatBroadcast содержит публичного кандидата текущего YouTube discovery без постоянного выбора.
type ChatBroadcast struct {
	// VideoID идентифицирует видео, разрешённое для выбора из текущего набора.
	VideoID string
	// ChannelID содержит фактический liveChatID этого эфира, если чат уже доступен.
	ChannelID string
	// Title содержит публичное название обнаруженного эфира.
	Title string
	// StartedAt содержит необязательную подтверждённую дату начала.
	StartedAt *time.Time
}

// ChatDiscovery возвращает результат одного ограниченного обнаружения; RAM generation назначает Manager.
type ChatDiscovery struct {
	// ChannelID содержит единственный найденный фактический чат Twitch/VK либо выбранного эфира.
	ChannelID string
	// Broadcasts содержит ограниченный адаптером набор YouTube-кандидатов; multiple требует выбора.
	Broadcasts []ChatBroadcast
	// State задаёт безопасный результат поиска либо ожидания.
	State ChatSourceState
	// Reason объясняет ожидание либо отказ без текста платформенной ошибки.
	Reason ChatReason
	// RetryAfter сохраняет более строгий интервал площадки, применяемый до нового обнаружения.
	RetryAfter time.Duration
}

// ChatAuthorizationRequest начинает новую RAM-попытку; набор передаётся только серверному owner.
type ChatAuthorizationRequest struct {
	// Provider выбирает один фиксированный способ авторизации платформы.
	Provider ChatProvider
	// AccountID пуст для нового аккаунта, иначе задаёт цель проверенной замены того же provider.
	AccountID ID
	// ExpectedRevision требуется для замены и относится к config revision.
	ExpectedRevision uint64
	// Name содержит необязательное имя нового аккаунта.
	Name string
	// ClientID задаёт публичное приложение Twitch до получения первого набора токенов.
	ClientID string
	// Bundle передаёт первоначальные серверные реквизиты без публикации в состоянии попытки.
	Bundle ChatCredentialBundle
}

// ChatAuthorizationState задаёт публичные состояния ограниченной RAM-попытки авторизации.
type ChatAuthorizationState string

const (
	// ChatAuthorizationPending означает незавершённое подтверждение либо проверку.
	ChatAuthorizationPending ChatAuthorizationState = "PENDING"
	// ChatAuthorizationSucceeded означает принятую и сохранённую конфигурацию.
	ChatAuthorizationSucceeded ChatAuthorizationState = "SUCCEEDED"
	// ChatAuthorizationFailed означает отказ проверки без изменения прежнего аккаунта.
	ChatAuthorizationFailed ChatAuthorizationState = "FAILED"
	// ChatAuthorizationCancelled означает отмену до принятия сохранения.
	ChatAuthorizationCancelled ChatAuthorizationState = "CANCELLED"
	// ChatAuthorizationExpired означает истечение меньшего из provider TTL и локального TTL.
	ChatAuthorizationExpired ChatAuthorizationState = "EXPIRED"
)

// ChatAuthorization содержит только публичное состояние попытки без device_code и токенов.
type ChatAuthorization struct {
	// AttemptID адресует ограниченную RAM-попытку, не восстанавливаемую после рестарта.
	AttemptID ID
	// AccountID заполняется для замены либо после принятого создания нового аккаунта.
	AccountID ID
	// State содержит текущее безопасное состояние попытки.
	State ChatAuthorizationState
	// UserCode содержит публичный Device Code user_code, если он нужен платформе.
	UserCode string
	// VerificationURI содержит публичный адрес подтверждения, без платформенных секретов.
	VerificationURI string
	// ExpiresAt ограничивает RAM-попытку более строгим сроком провайдера либо локальной политики.
	ExpiresAt time.Time
	// Reason содержит безопасную причину отказа без диагностической ошибки.
	Reason ChatReason
}

// ChatCredentialOwnerPort определяет будущего единственного сериализующего owner аккаунта.
// Внешняя проверка выполняется вне Write. После неоднозначного COMMIT owner сверяет обе версии,
// WriteID и полный результат до следующей credential-операции; внешний одноразовый обмен не повторяется.
type ChatCredentialOwnerPort interface {
	// Connect начинает ограниченную RAM-попытку, не меняя прежние настройки до успешного принятия.
	Connect(context.Context, ChatAuthorizationRequest) (ChatAuthorization, error)
	// Authorization читает только публичное состояние попытки; неизвестный ID возвращает ErrNotFound.
	Authorization(context.Context, ID) (ChatAuthorization, error)
	// CancelAuthorization отменяет только целевую незавершённую попытку до принятия сохранения.
	// Уже выполненный COMMIT не откатывается; неподтверждённый исход остаётся неопределённым.
	CancelAuthorization(context.Context, ID) error
	// Credentials сериализует получение либо обновление набора для reader данного аккаунта.
	Credentials(context.Context, ID) (ChatCredentials, error)
	// Close отменяет попытки и ожидает их ограниченное завершение до закрытия Store.
	Close(context.Context) error
	// Done закрывается после окончания принадлежащих owner операций.
	Done() <-chan struct{}
}
