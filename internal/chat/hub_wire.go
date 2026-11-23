package chat

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// chatJSONMessage формирует полную публичную запись с теми же именами полей, которые потребляет лента.
// json.Marshal учитывает экранирование строк и передаёт seq целым числом без float64.
func chatJSONMessage(message core.ChatMessage) ([]byte, error) {
	// author содержит обязательную часть публичного автора.
	author := map[string]any{"id": message.Author.ID, "display_name": message.Author.DisplayName}
	if message.Author.Color != "" {
		author["color"] = message.Author.Color
	}
	if message.Author.AvatarURL != "" {
		author["avatar_url"] = message.Author.AvatarURL
	}
	// parts содержит структурированные фрагменты в порядке адаптера.
	parts := make([]map[string]any, len(message.Parts))
	// index определяет позицию проекции, part содержит исходный фрагмент.
	for index, part := range message.Parts {
		// fields содержит обязательные поля фрагмента и пополняется доступными атрибутами.
		fields := map[string]any{"kind": part.Kind, "text": part.Text}
		if part.EmoteID != "" {
			fields["emote_id"] = part.EmoteID
		}
		if part.ImageURL != "" {
			fields["image_url"] = part.ImageURL
		}
		if part.URL != "" {
			fields["url"] = part.URL
		}
		parts[index] = fields
	}
	// badges содержит публичные проекции меток в исходном порядке.
	badges := make([]map[string]any, len(message.Badges))
	// index определяет позицию проекции, badge содержит исходную метку.
	for index, badge := range message.Badges {
		// fields содержит обязательные поля метки и её доступные ссылки.
		fields := map[string]any{"id": badge.ID, "name": badge.Name}
		if badge.ImageURL != "" {
			fields["image_url"] = badge.ImageURL
		}
		if badge.URL != "" {
			fields["url"] = badge.URL
		}
		badges[index] = fields
	}
	// fields содержит все обязательные данные сообщения, включая назначенные сервером атрибуты.
	fields := map[string]any{
		"session_id": message.SessionID, "chat_source_id": message.ChatSourceID, "seq": message.Seq,
		"provider": message.Provider, "channel_id": message.ChannelID, "provider_message_id": message.ProviderMessageID,
		"received_at": message.ReceivedAt, "author": author, "text": message.Text, "parts": parts,
		"badges": badges, "kind": message.Kind,
	}
	if message.ProviderTimestamp != nil {
		fields["provider_timestamp"] = message.ProviderTimestamp
	}
	return json.Marshal(fields)
}

// chatJSONStatus собирает публичное состояние без внутреннего токена и диагностики площадки.
func chatJSONStatus(status core.ChatSourceStatus) map[string]any {
	// fields содержит обязательные атрибуты и пополняется известными временами площадки.
	fields := map[string]any{
		"source_id": status.SourceID, "state": status.State, "reason": status.Reason,
		"changed_at": status.ChangedAt, "dropped": status.Dropped,
	}
	if status.LastEventAt != nil {
		fields["last_event_at"] = status.LastEventAt
	}
	if status.BroadcastAt != nil {
		fields["broadcast_at"] = status.BroadcastAt
	}
	return fields
}

// chatJSONSource собирает только открытые сохранённые настройки источника.
func chatJSONSource(source core.ChatSource) map[string]any {
	return map[string]any{
		"id": source.ID, "revision": source.Revision, "created_at": source.CreatedAt, "updated_at": source.UpdatedAt,
		"destination_id": source.DestinationID, "account_id": source.AccountID, "provider": source.Provider,
		"name": source.Name, "enabled": source.Enabled, "channel_hint": source.ChannelHint, "options": source.Options,
	}
}

// chatEventSizes считает отдельно JSON события, полный кадр SSE и логический вес записи подписчика.
// messageJSON передаёт уже закодированное сообщение и используется только для chat.message.
func chatEventSizes(event core.ChatEvent, messageJSON []byte) (uint64, uint64, uint64, error) {
	// fields содержит общие атрибуты события и пополняется данными его конкретного вида.
	fields := map[string]any{"kind": event.Kind, "cursor": event.Cursor, "reason": event.Reason}
	if event.SessionID != "" {
		fields["session_id"] = event.SessionID
	}
	if event.Message != nil {
		fields["message"] = json.RawMessage(messageJSON)
	}
	if event.Status != nil {
		fields["status"] = chatJSONStatus(*event.Status)
	}
	if event.Kind == core.ChatEventGap {
		fields["lost"] = event.Lost
	}
	// jsonEvent содержит полный JSON, err сообщает о невозможности закодировать значение.
	jsonEvent, err := json.Marshal(fields)
	if err != nil {
		return 0, 0, 0, err
	}
	// frameBytes содержит размер служебных строк SSE, cursor, kind и JSON.
	frameBytes := uint64(len("id: \nevent: \ndata: \n\n") + len(event.Cursor) + len(event.Kind) + len(jsonEvent))
	// logical учитывает строки и фиксированный вес собственно события.
	logical := uint64(256 + len(event.Kind) + len(event.Cursor) + len(event.SessionID) + len(event.Reason))
	if event.Message != nil {
		// messageWeight содержит вес вложенного сообщения, weightErr сообщает о переполнении при подсчёте.
		messageWeight, weightErr := event.Message.LogicalBytes()
		if weightErr != nil {
			return 0, 0, 0, weightErr
		}
		logical, err = core.CheckedChatBytes(logical, messageWeight)
	} else if event.Status != nil {
		logical, err = core.CheckedChatBytes(logical, uint64(256+len(event.Status.SourceID)+len(event.Status.State)+len(event.Status.Reason)))
	}
	if err != nil {
		return 0, 0, 0, err
	}
	return uint64(len(jsonEvent)), frameBytes, logical, nil
}

// chatCopyTime выделяет отдельную ячейку для необязательной временной метки.
func chatCopyTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	// copyOfValue можно передать вызывающему коду независимо от исходного указателя.
	copyOfValue := *value
	return &copyOfValue
}

// chatOwnIncoming отделяет срезы, метку времени и байты подстрок от буферов адаптера.
func chatOwnIncoming(value core.ChatIncomingMessage) core.ChatIncomingMessage {
	value.Provider = core.ChatProvider(strings.Clone(string(value.Provider)))
	value.ChannelID = strings.Clone(value.ChannelID)
	value.ProviderMessageID = strings.Clone(value.ProviderMessageID)
	value.ProviderTimestamp = chatCopyTime(value.ProviderTimestamp)
	value.Author.ID = strings.Clone(value.Author.ID)
	value.Author.DisplayName = strings.Clone(value.Author.DisplayName)
	value.Author.Color = strings.Clone(value.Author.Color)
	value.Author.AvatarURL = strings.Clone(value.Author.AvatarURL)
	value.Text = strings.Clone(value.Text)
	value.Kind = core.ChatMessageKind(strings.Clone(string(value.Kind)))
	value.Parts = slices.Clone(value.Parts)
	// index адресует принадлежащий Hub фрагмент.
	for index := range value.Parts {
		// part указывает на фрагмент новой копии, где заменяются ссылки на строки адаптера.
		part := &value.Parts[index]
		part.Kind = core.ChatPartKind(strings.Clone(string(part.Kind)))
		part.Text = strings.Clone(part.Text)
		part.EmoteID = strings.Clone(part.EmoteID)
		part.ImageURL = strings.Clone(part.ImageURL)
		part.URL = strings.Clone(part.URL)
	}
	value.Badges = slices.Clone(value.Badges)
	// index адресует принадлежащую Hub метку.
	for index := range value.Badges {
		// badge указывает на метку новой копии, где заменяются ссылки на строки адаптера.
		badge := &value.Badges[index]
		badge.ID = strings.Clone(badge.ID)
		badge.Name = strings.Clone(badge.Name)
		badge.ImageURL = strings.Clone(badge.ImageURL)
		badge.URL = strings.Clone(badge.URL)
	}
	return value
}

// chatCopyMessage отделяет изменяемые составные поля перед выдачей сообщения потребителю.
// Неизменяемые строки уже принадлежат Hub и могут использоваться обеими копиями.
func chatCopyMessage(message core.ChatMessage) core.ChatMessage {
	message.ProviderTimestamp = chatCopyTime(message.ProviderTimestamp)
	message.Parts = slices.Clone(message.Parts)
	message.Badges = slices.Clone(message.Badges)
	return message
}

// chatCopyStatus передаёт потребителю собственные копии обеих необязательных меток времени.
func chatCopyStatus(status core.ChatSourceStatus) core.ChatSourceStatus {
	status.LastEventAt = chatCopyTime(status.LastEventAt)
	status.BroadcastAt = chatCopyTime(status.BroadcastAt)
	return status
}

// chatCopyEvent копирует содержимое события для внешнего читателя и сохраняет его точную позицию.
func chatCopyEvent(event core.ChatEvent) core.ChatEvent {
	if event.Message != nil {
		// message содержит отделённую копию полезной нагрузки.
		message := chatCopyMessage(*event.Message)
		event.Message = &message
	}
	if event.Status != nil {
		// status содержит отделённую копию состояния.
		status := chatCopyStatus(*event.Status)
		event.Status = &status
	}
	return event
}

// chatSnapshotView хранит данные, снятые под блокировкой одной позиции.
// Сообщения оставлены неизменяемыми до формирования собственной копии ответа.
type chatSnapshotView struct {
	// snapshot содержит метаданные и согласованный cursor.
	snapshot core.ChatSnapshot
	// history содержит соответствующее курсору окно сообщений с их полными размерами JSON.
	history []chatBufferItem[core.ChatMessage]
}

// build проверяет полный бюджет метаданных и оставляет самый свежий непрерывный хвост сообщений.
// Удерживаемые Hub записи остаются неизменными при обрезке и передаче копий.
func (v chatSnapshotView) build(ctx context.Context, limits core.ChatLimits) (core.ChatSnapshot, error) {
	if ctx == nil {
		return core.ChatSnapshot{}, core.ErrInvalid
	}
	// err сообщает об отмене формирования снимка до кодирования метаданных.
	if err := ctx.Err(); err != nil {
		return core.ChatSnapshot{}, err
	}
	// result содержит независимые верхнеуровневые поля возвращаемого снимка.
	result := v.snapshot
	// session содержит публичную форму точного выполнения, если сессия уже существует.
	var session map[string]any
	if result.Session != nil {
		session = map[string]any{"session_id": result.Session.SessionID, "incarnation": result.Session.Incarnation, "lifecycle": result.Session.Lifecycle}
		// fence отделяет возвращаемый указатель от захваченного представления.
		fence := *result.Session
		result.Session = &fence
	}
	result.Sources = append(make([]core.ChatSource, 0, len(result.Sources)), result.Sources...)
	result.Statuses = append(make([]core.ChatSourceStatus, 0, len(result.Statuses)), result.Statuses...)
	// sources и statuses содержат публичные JSON-проекции соответствующих полей снимка.
	sources := make([]map[string]any, len(result.Sources))
	statuses := make([]map[string]any, len(result.Statuses))
	// index задаёт позицию конфигурации, source содержит очередную открытую запись.
	for index, source := range result.Sources {
		sources[index] = chatJSONSource(source)
	}
	// index задаёт позицию статуса, status содержит соответствующую оперативную запись.
	for index, status := range result.Statuses {
		statuses[index] = chatJSONStatus(status)
		result.Statuses[index] = chatCopyStatus(status)
	}
	// metadata содержит JSON снимка с пустым массивом сообщений, err сообщает об ошибке кодирования.
	metadata, err := json.Marshal(map[string]any{
		"stream_id": result.StreamID, "session": session, "sources": sources, "statuses": statuses,
		"messages": []json.RawMessage{}, "cursor": result.Cursor,
	})
	if err != nil {
		return core.ChatSnapshot{}, core.ErrInvalid
	}
	// used занимает фактический размер всей оболочки снимка до добавления сообщений.
	used := uint64(len(metadata))
	if used > limits.SnapshotMetadataBytes || used > limits.SnapshotBytes {
		return core.ChatSnapshot{}, core.ErrNotReady
	}
	// first отмечает начало самого свежего хвоста, помещающегося в общий бюджет.
	first := len(v.history)
	for first > 0 {
		// additional включает JSON очередной записи и запятую перед уже выбранным хвостом.
		additional := v.history[first-1].size
		if first != len(v.history) {
			additional, err = core.CheckedChatBytes(additional, 1)
		}
		if err != nil || additional > limits.SnapshotBytes-used {
			break
		}
		used += additional
		first--
	}
	result.Messages = make([]core.ChatMessage, len(v.history)-first)
	// index задаёт позицию в ответе, item содержит неизменяемую запись выбранного хвоста.
	for index, item := range v.history[first:] {
		// err сообщает об отмене во время копирования ограниченного хвоста.
		if err := ctx.Err(); err != nil {
			return core.ChatSnapshot{}, err
		}
		result.Messages[index] = chatCopyMessage(item.value)
	}
	return result, nil
}
