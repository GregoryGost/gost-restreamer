// Package chatwire задаёт публичный JSON чатов для Hub и HTTP API.
package chatwire

import (
	"encoding/json"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// OptionsDTO содержит версию и постоянные настройки одной площадки.
type OptionsDTO struct {
	// Version задаёт версию сохранённых настроек.
	Version int `json:"version" enum:"1"`
	// YouTube присутствует у источника YouTube.
	YouTube *struct{} `json:"youtube,omitempty"`
	// Twitch присутствует у источника Twitch.
	Twitch *struct{} `json:"twitch,omitempty"`
	// VKVideoLive присутствует у источника VK Видео Live.
	VKVideoLive *struct{} `json:"vk_video_live,omitempty"`
}

// SourceDTO представляет сохранённую конфигурацию источника чата.
type SourceDTO struct {
	// ID идентифицирует источник.
	ID core.ID `json:"id"`
	// Revision используется для условного изменения конфигурации.
	Revision uint64 `json:"revision" minimum:"1"`
	// CreatedAt содержит время создания записи.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt содержит время последнего изменения настроек.
	UpdatedAt time.Time `json:"updated_at"`
	// DestinationID связывает источник с родительским назначением.
	DestinationID core.ID `json:"destination_id"`
	// AccountID ссылается на конфигурационные метаданные аккаунта.
	AccountID core.ID `json:"account_id"`
	// Provider определяет площадку.
	Provider core.ChatProvider `json:"provider" enum:"youtube,twitch,vk_video_live"`
	// Name содержит отображаемое имя.
	Name string `json:"name"`
	// Enabled разрешает чтение с подходящего выхода.
	Enabled bool `json:"enabled"`
	// ChannelHint содержит постоянный идентификатор либо slug канала.
	ChannelHint string `json:"channel_hint"`
	// Options содержит единственный набор настроек соответствующей площадки.
	Options OptionsDTO `json:"options"`
}

// Source проецирует конфигурацию в открытые поля REST и снимка.
func Source(source core.ChatSource) SourceDTO {
	return SourceDTO{
		ID: source.ID, Revision: source.Revision, CreatedAt: source.CreatedAt, UpdatedAt: source.UpdatedAt,
		DestinationID: source.DestinationID, AccountID: source.AccountID, Provider: source.Provider,
		Name: source.Name, Enabled: source.Enabled, ChannelHint: source.ChannelHint,
		Options: OptionsDTO{
			Version: source.Options.Version, YouTube: (*struct{})(source.Options.YouTube),
			Twitch: (*struct{})(source.Options.Twitch), VKVideoLive: (*struct{})(source.Options.VKVideoLive),
		},
	}
}

// StatusDTO содержит текущее оперативное состояние источника чата.
type StatusDTO struct {
	// SourceID связывает статус с открытой конфигурацией.
	SourceID core.ID `json:"source_id"`
	// State задаёт текущее состояние чтения.
	State core.ChatSourceState `json:"state" enum:"DISABLED,WAITING_OUTPUT,DISCOVERING,WAITING_CHAT,CONNECTING,CONNECTED,RETRYING,AUTH_REQUIRED,FAILED,STOPPED"`
	// Reason содержит безопасную причину состояния или пустую строку.
	Reason core.ChatReason `json:"reason" enum:",no_broadcast,selection_required,chat_unavailable,chat_ended,chat_disabled,credentials_required,network,quota,configuration,limit,upstream_gap"`
	// ChangedAt содержит время последнего изменения состояния.
	ChangedAt time.Time `json:"changed_at"`
	// LastEventAt содержит время последнего известного события.
	LastEventAt *time.Time `json:"last_event_at,omitempty"`
	// BroadcastAt содержит время начала обнаруженного эфира, когда оно известно.
	BroadcastAt *time.Time `json:"broadcast_at,omitempty"`
	// Dropped учитывает локально отброшенные сообщения.
	Dropped uint64 `json:"dropped"`
}

// Status проецирует оперативное состояние в JSON снимка и SSE.
func Status(status core.ChatSourceStatus) StatusDTO {
	return StatusDTO{
		SourceID: status.SourceID, State: status.State, Reason: status.Reason, ChangedAt: status.ChangedAt,
		LastEventAt: status.LastEventAt, BroadcastAt: status.BroadcastAt, Dropped: status.Dropped,
	}
}

// AuthorDTO содержит открытые атрибуты отправителя.
type AuthorDTO struct {
	// ID идентифицирует автора на площадке, если она предоставила значение.
	ID string `json:"id"`
	// DisplayName содержит отображаемое имя.
	DisplayName string `json:"display_name"`
	// Color содержит необязательный цвет, переданный площадкой.
	Color string `json:"color,omitempty"`
	// AvatarURL содержит необязательный HTTPS-адрес аватара.
	AvatarURL string `json:"avatar_url,omitempty"`
}

// PartDTO хранит одну текстовую либо графическую часть сообщения.
type PartDTO struct {
	// Kind отличает текст от эмодзи площадки.
	Kind core.ChatPartKind `json:"kind" enum:"text,emote"`
	// Text содержит текст либо обозначение эмодзи.
	Text string `json:"text"`
	// EmoteID содержит необязательный идентификатор эмодзи.
	EmoteID string `json:"emote_id,omitempty"`
	// ImageURL содержит необязательный HTTPS-адрес изображения эмодзи.
	ImageURL string `json:"image_url,omitempty"`
	// URL содержит необязательную HTTPS-ссылку.
	URL string `json:"url,omitempty"`
}

// BadgeDTO передаёт знак автора и связанные с ним ссылки.
type BadgeDTO struct {
	// ID идентифицирует знак на площадке.
	ID string `json:"id"`
	// Name содержит читаемое название.
	Name string `json:"name"`
	// ImageURL содержит необязательный HTTPS-адрес изображения знака.
	ImageURL string `json:"image_url,omitempty"`
	// URL содержит необязательную HTTPS-ссылку.
	URL string `json:"url,omitempty"`
}

// MessageDTO содержит одну принятую запись в ленте.
type MessageDTO struct {
	// SessionID идентифицирует запуск и остаётся пустым у тестовой записи.
	SessionID core.ID `json:"session_id"`
	// ChatSourceID связывает запись с конфигурационным источником и остаётся пустым у тестовой записи.
	ChatSourceID core.ID `json:"chat_source_id"`
	// Seq задаёт точный порядок принятия записей в пределах запуска.
	Seq uint64 `json:"seq" minimum:"1"`
	// Provider указывает площадку сообщения или локальный генератор test.
	Provider core.ChatProvider `json:"provider" enum:"youtube,twitch,vk_video_live,test"`
	// TestProvider задаёт площадку, которую имитирует тестовая запись.
	TestProvider core.ChatProvider `json:"test_provider,omitempty" enum:"youtube,twitch,vk_video_live"`
	// ChannelID содержит публичный идентификатор платформенной беседы.
	ChannelID string `json:"channel_id"`
	// ProviderMessageID сохраняет исходный идентификатор, если он доступен.
	ProviderMessageID string `json:"provider_message_id"`
	// ProviderTimestamp содержит достоверное время площадки, если оно доступно.
	ProviderTimestamp *time.Time `json:"provider_timestamp,omitempty"`
	// ReceivedAt фиксирует серверное время приёма.
	ReceivedAt time.Time `json:"received_at"`
	// Author содержит открытые атрибуты автора.
	Author AuthorDTO `json:"author"`
	// Text сохраняет исходный текст без интерпретации HTML.
	Text string `json:"text"`
	// Parts содержит упорядоченные фрагменты текста и эмодзи.
	Parts []PartDTO `json:"parts" nullable:"false"`
	// Badges перечисляет знаки автора.
	Badges []BadgeDTO `json:"badges" nullable:"false"`
	// Kind различает сообщение и поддерживаемое событие площадки.
	Kind core.ChatMessageKind `json:"kind" enum:"message,supported_system_event"`
}

// Message формирует одинаковое представление записи в истории, снимке и кадре SSE.
func Message(message core.ChatMessage) MessageDTO {
	// parts и badges сохраняют порядок площадки и кодируют отсутствие элементов пустыми массивами.
	parts := make([]PartDTO, len(message.Parts))
	badges := make([]BadgeDTO, len(message.Badges))
	// index указывает позицию во фрагментах, part содержит открытые поля этой позиции.
	for index, part := range message.Parts {
		parts[index] = PartDTO(part)
	}
	// index указывает позицию знака, badge содержит его открытые поля.
	for index, badge := range message.Badges {
		badges[index] = BadgeDTO(badge)
	}
	return MessageDTO{
		SessionID: message.SessionID, ChatSourceID: message.ChatSourceID, Seq: message.Seq,
		Provider: message.Provider, TestProvider: message.TestProvider, ChannelID: message.ChannelID, ProviderMessageID: message.ProviderMessageID,
		ProviderTimestamp: message.ProviderTimestamp, ReceivedAt: message.ReceivedAt,
		Author: AuthorDTO(message.Author), Text: message.Text, Parts: parts, Badges: badges, Kind: message.Kind,
	}
}

// ChatSessionDTO содержит публичную идентичность запуска.
type ChatSessionDTO struct {
	// SessionID идентифицирует запуск без данных о внутреннем поколении.
	SessionID core.ID `json:"session_id"`
}

// SnapshotDTO содержит согласованную позицию и свежий хвост одного потока.
type SnapshotDTO struct {
	// StreamID идентифицирует поток, к которому относится снимок.
	StreamID core.ID `json:"stream_id"`
	// Session содержит текущий запуск либо null до его появления.
	Session *ChatSessionDTO `json:"session"`
	// Sources содержит безопасные конфигурации настроенных источников.
	Sources []SourceDTO `json:"sources" nullable:"false"`
	// Statuses содержит оперативные состояния источников отдельно от ленты.
	Statuses []StatusDTO `json:"statuses" nullable:"false"`
	// Messages содержит непрерывный хвост истории в порядке получения.
	Messages []MessageDTO `json:"messages" nullable:"false"`
	// Cursor определяет согласованную позицию продолжения через SSE.
	Cursor core.ChatCursor `json:"cursor"`
}

// Snapshot преобразует снимок в публичную проекцию. Списки без элементов передаются как [].
func Snapshot(snapshot core.ChatSnapshot) SnapshotDTO {
	// result хранит проекции метаданных и сообщений в порядке исходного снимка.
	result := SnapshotDTO{
		StreamID: snapshot.StreamID, Cursor: snapshot.Cursor,
		Sources: make([]SourceDTO, len(snapshot.Sources)), Statuses: make([]StatusDTO, len(snapshot.Statuses)),
		Messages: make([]MessageDTO, len(snapshot.Messages)),
	}
	if snapshot.Session != nil {
		result.Session = &ChatSessionDTO{SessionID: snapshot.Session.SessionID}
	}
	// index задаёт позицию, source содержит очередную конфигурацию снимка.
	for index, source := range snapshot.Sources {
		result.Sources[index] = Source(source)
	}
	// index задаёт позицию, status содержит состояние источника в момент снимка.
	for index, status := range snapshot.Statuses {
		result.Statuses[index] = Status(status)
	}
	// index задаёт позицию, message содержит следующую запись непрерывного хвоста.
	for index, message := range snapshot.Messages {
		result.Messages[index] = Message(message)
	}
	return result
}

// eventDTO описывает один публичный кадр и допускает повторное использование уже измеренного сообщения.
type eventDTO[M any] struct {
	// Kind задаёт имя события и вид его данных.
	Kind core.ChatEventKind `json:"kind" enum:"chat.message,chat.source_status,chat.gap,chat.cleared,resync_required,chat.session_closed"`
	// Cursor содержит ту же непрозрачную позицию, что и строка id кадра.
	Cursor core.ChatCursor `json:"cursor"`
	// Reason содержит безопасную причину или пустую строку.
	Reason core.ChatReason `json:"reason" enum:",no_broadcast,selection_required,chat_unavailable,chat_ended,chat_disabled,credentials_required,network,quota,configuration,limit,upstream_gap"`
	// SessionID связывает событие с запуском, если он уже известен.
	SessionID core.ID `json:"session_id,omitempty"`
	// Message передаёт одну запись для chat.message.
	Message M `json:"message,omitempty"`
	// Status передаёт оперативное состояние для chat.source_status.
	Status *StatusDTO `json:"status,omitempty"`
	// Lost содержит число известных потерь для chat.gap, включая значение 0.
	Lost *uint64 `json:"lost,omitempty"`
}

// EventDTO задаёт типизированную схему данных SSE независимо от оформления кадра.
type EventDTO eventDTO[*MessageDTO]

// eventFields собирает оболочку с применимой нагрузкой. Параметр message содержит
// типизированную проекцию либо уже закодированный JSON этой же проекции.
func eventFields[M any](event core.ChatEvent, message M) eventDTO[M] {
	// result содержит общие поля и выбранные для этого вида события данные.
	result := eventDTO[M]{Kind: event.Kind, Cursor: event.Cursor, Reason: event.Reason, SessionID: event.SessionID, Message: message}
	if event.Status != nil {
		// status содержит публичные атрибуты оперативного состояния.
		status := Status(*event.Status)
		result.Status = &status
	}
	if event.Kind == core.ChatEventGap {
		result.Lost = &event.Lost
	}
	return result
}

// EventJSON сериализует данные кадра. Hub может передать собственный JSON MessageDTO,
// чтобы повторно использовать байты, уже закодированные для учёта истории.
func EventJSON(event core.ChatEvent, messageJSON []byte) ([]byte, error) {
	if len(messageJSON) != 0 {
		return json.Marshal(eventFields(event, json.RawMessage(messageJSON)))
	}
	// message содержит типизированную запись только у события chat.message.
	var message *MessageDTO
	if event.Message != nil {
		// dto проецирует единственное сообщение кадра.
		dto := Message(*event.Message)
		message = &dto
	}
	return json.Marshal(EventDTO(eventFields(event, message)))
}
