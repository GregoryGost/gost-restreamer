package chat

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/chatwire"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// EventCursorBytes содержит длину выдаваемой позиции в байтах base64url.
const EventCursorBytes = (8*(chatCursorPositionBytes+chatCursorTagBytes) + 5) / 6

// chatJSONMessage кодирует ту же публичную запись, которую используют snapshot и SSE.
func chatJSONMessage(message core.ChatMessage) ([]byte, error) {
	return json.Marshal(chatwire.Message(message))
}

// chatEventSizes считает отдельно JSON события, полный кадр SSE и логический вес записи подписчика.
// messageJSON передаёт уже закодированное сообщение и используется только для chat.message.
func chatEventSizes(event core.ChatEvent, messageJSON []byte) (uint64, uint64, uint64, error) {
	// jsonEvent содержит полный JSON события, err сообщает об отказе кодирования.
	jsonEvent, err := chatwire.EventJSON(event, messageJSON)
	if err != nil {
		return 0, 0, 0, err
	}
	// frameBytes включает те же служебные строки, что и отправляемый HTTP-кадр.
	frameBytes := chatwire.FrameSize(event, jsonEvent)
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
	// test позволяет проверить допуск скопированного тестового снимка перед возвратом.
	test *core.ChatTestAdmission
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
	if result.Session != nil {
		// fence отделяет возвращаемый указатель от захваченного представления.
		fence := *result.Session
		result.Session = &fence
	}
	result.Sources = append(make([]core.ChatSource, 0, len(result.Sources)), result.Sources...)
	result.Statuses = append(make([]core.ChatSourceStatus, 0, len(result.Statuses)), result.Statuses...)
	// index адресует статус, status содержит временные метки, которые нужно отделить от Hub.
	for index, status := range result.Statuses {
		result.Statuses[index] = chatCopyStatus(status)
	}
	// metadata содержит тот же JSON метаданных, который добавляется в HTTP-ответ,
	// err сообщает об ошибке кодирования.
	metadata, err := json.Marshal(chatwire.Snapshot(result))
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
