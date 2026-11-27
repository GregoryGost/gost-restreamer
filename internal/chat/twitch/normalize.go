package twitch

import (
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// eventSubChatMessage содержит поля channel.chat.message, которые нужны ленте.
type eventSubChatMessage struct {
	// BroadcasterID определяет чат, в который Twitch доставил сообщение.
	BroadcasterID string `json:"broadcaster_user_id"`
	// AuthorID идентифицирует отправителя сообщения.
	AuthorID string `json:"chatter_user_id"`
	// AuthorName содержит отображаемое имя отправителя.
	AuthorName string `json:"chatter_user_name"`
	// Color содержит необязательный платформенный цвет отправителя.
	Color string `json:"color"`
	// ID идентифицирует сообщение чата независимо от конверта EventSub.
	ID string `json:"message_id"`
	// Message содержит исходный текст и его упорядоченные фрагменты.
	Message *struct {
		// Text содержит полное сообщение без оформления Twitch.
		Text string `json:"text"`
		// Fragments содержит исходные фрагменты в порядке отображения.
		Fragments []struct {
			// Type определяет представление фрагмента на стороне Twitch.
			Type string `json:"type"`
			// Text передаёт текстовый эквивалент фрагмента.
			Text string `json:"text"`
			// Emote содержит идентификатор только у эмодзи Twitch.
			Emote *struct {
				// ID идентифицирует эмодзи в каталоге Twitch.
				ID string `json:"id"`
			} `json:"emote"`
		} `json:"fragments"`
	} `json:"message"`
	// Badges содержит наборы и версии значков отправителя в чате назначения.
	Badges []struct {
		// SetID служит названием набора и частью идентификатора значка.
		SetID string `json:"set_id"`
		// ID содержит версию значка в его наборе.
		ID string `json:"id"`
	} `json:"badges"`
}

// validTwitchUserID проверяет ASCII-десятичный идентификатор value. Проверка
// формы оставляет исходный идентификатор без преобразования в машинное число.
func validTwitchUserID(value string) bool {
	if len(value) == 0 || len(value) > core.ChatIDBytes {
		return false
	}
	// index указывает позицию очередного байта Twitch ID.
	for index := range len(value) {
		// digit содержит байт для проверки формы.
		digit := value[index]
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

// normalizeChatMessage преобразует сообщение event для подтверждённого broadcasterID.
// timestamp берётся из оболочки EventSub. При нарушении идентичности, формы
// или лимитов функция возвращает false и не передаёт неполное сообщение в ленту.
func normalizeChatMessage(event *eventSubChatMessage, broadcasterID string, timestamp time.Time) (core.ChatIncomingMessage, bool) {
	if event == nil || event.BroadcasterID != broadcasterID || !validEventSubID(event.ID) ||
		!validTwitchUserID(event.AuthorID) || event.Message == nil || event.Message.Text == "" ||
		len(event.Message.Fragments) > core.ChatMaxParts || len(event.Badges) > core.ChatMaxBadges {
		return core.ChatIncomingMessage{}, false
	}
	// published содержит время отправки в точности домена, err сообщает о непредставимой метке.
	published, err := core.NormalizeTimestamp(timestamp)
	if err != nil || published.IsZero() {
		return core.ChatIncomingMessage{}, false
	}
	// msg сохраняет только публичные поля для дальнейшей проверки Hub.
	msg := core.ChatIncomingMessage{
		Provider: core.ChatTwitch, ChannelID: broadcasterID, ProviderMessageID: event.ID,
		ProviderTimestamp: &published,
		Author:            core.ChatAuthor{ID: event.AuthorID, DisplayName: event.AuthorName, Color: event.Color},
		Text:              event.Message.Text, Kind: core.ChatMessageText,
	}
	// fragment соответствует одному участку исходного сообщения.
	for _, fragment := range event.Message.Fragments {
		// part по умолчанию сохраняет текстовый эквивалент, в том числе для неизвестного вида фрагмента.
		part := core.ChatPart{Kind: core.ChatPartText, Text: fragment.Text}
		if fragment.Type == "emote" && fragment.Emote != nil && validEventSubID(fragment.Emote.ID) {
			part.Kind, part.EmoteID = core.ChatPartEmote, fragment.Emote.ID
		}
		msg.Parts = append(msg.Parts, part)
	}
	// badge содержит набор и версию значка. Версия без набора не идентифицирует значок Twitch.
	for _, badge := range event.Badges {
		if validEventSubID(badge.SetID) && validEventSubID(badge.ID) {
			msg.Badges = append(msg.Badges, core.ChatBadge{ID: badge.SetID + "/" + badge.ID, Name: badge.SetID})
		}
	}
	return msg, msg.Validate() == nil
}
