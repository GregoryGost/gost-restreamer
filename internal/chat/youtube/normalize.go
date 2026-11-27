package youtube

import (
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// normalizeStreamItem преобразует item из чата chatID в сообщение ленты.
// Второй результат подтверждает завершение чата. Пустой Kind означает, что
// элемент не предназначен для ленты либо относится к другому чату.
func normalizeStreamItem(item protoreflect.Message, chatID string) (core.ChatIncomingMessage, bool) {
	// snippet содержит вид, текст и дополнительные поля события.
	snippet := item.Get(protobufField(item, "snippet")).Message()
	if !snippet.IsValid() {
		return core.ChatIncomingMessage{}, false
	}
	// actualID при наличии должен совпасть с фактическим ID запроса.
	if actualID := protobufString(snippet, "live_chat_id"); actualID != "" && actualID != chatID {
		return core.ChatIncomingMessage{}, false
	}
	// typeField указывает перечисление из официальной схемы.
	typeField := protobufField(snippet, "type")
	// eventType связывает присланный номер с известным именем события.
	eventType := typeField.Enum().Values().ByNumber(snippet.Get(typeField).Enum())
	if eventType == nil {
		return core.ChatIncomingMessage{}, false
	}
	if eventType.Name() == "CHAT_ENDED_EVENT" {
		return core.ChatIncomingMessage{}, true
	}
	// displayContent явно запрещает показ, если поле присутствует и равно false.
	displayContent := protobufField(snippet, "has_display_content")
	if snippet.Has(displayContent) && !snippet.Get(displayContent).Bool() {
		return core.ChatIncomingMessage{}, false
	}
	// text предназначен для показа в ленте, kind различает сообщения и объявления.
	text, kind := streamMessageText(snippet, eventType.Name())
	if text == "" {
		return core.ChatIncomingMessage{}, false
	}
	// author содержит сведения об отправителе, badges — подтверждённые площадкой роли.
	author, badges := streamAuthor(item, snippet)
	return core.ChatIncomingMessage{
		Provider: core.ChatYouTube, ChannelID: chatID, ProviderMessageID: protobufString(item, "id"),
		ProviderTimestamp: streamTimestamp(protobufString(snippet, "published_at")),
		Author:            author, Badges: badges, Text: text, Kind: kind,
	}, false
}

// streamMessageText возвращает текст для поддерживаемого eventType из snippet.
// Обычное сообщение сохраняет слова автора. Для объявлений используется
// готовый текст YouTube. Управляющие события остаются за пределами ленты.
func streamMessageText(snippet protoreflect.Message, eventType protoreflect.Name) (string, core.ChatMessageKind) {
	switch eventType {
	case "TEXT_MESSAGE_EVENT":
		// details содержит исходный пользовательский текст, если площадка его прислала.
		details := snippet.Get(protobufField(snippet, "text_message_details")).Message()
		if details.IsValid() {
			// sourceText хранит сообщение без оформления площадкой.
			if sourceText := protobufString(details, "message_text"); sourceText != "" {
				return sourceText, core.ChatMessageText
			}
		}
		return protobufString(snippet, "display_message"), core.ChatMessageText
	case "FAN_FUNDING_EVENT", "NEW_SPONSOR_EVENT", "MEMBER_MILESTONE_CHAT_EVENT",
		"MEMBERSHIP_GIFTING_EVENT", "GIFT_MEMBERSHIP_RECEIVED_EVENT", "SUPER_CHAT_EVENT",
		"SUPER_STICKER_EVENT", "GIFT_EVENT":
		return protobufString(snippet, "display_message"), core.ChatMessageSystem
	default:
		return "", ""
	}
}

// streamAuthor извлекает из item публичные данные автора. Когда author_details
// не содержит ID, функция использует author_channel_id из snippet.
func streamAuthor(item, snippet protoreflect.Message) (core.ChatAuthor, []core.ChatBadge) {
	// author получает ID из snippet до чтения отдельного блока author_details.
	author := core.ChatAuthor{ID: protobufString(snippet, "author_channel_id")}
	// details содержит отображаемое имя, аватар и метки YouTube.
	details := item.Get(protobufField(item, "author_details")).Message()
	if !details.IsValid() {
		return author, nil
	}
	// authorID имеет приоритет при наличии ID автора в author_details.
	if authorID := protobufString(details, "channel_id"); authorID != "" {
		author.ID = authorID
	}
	author.DisplayName = protobufString(details, "display_name")
	author.AvatarURL = streamAvatar(protobufString(details, "profile_image_url"))
	// badges содержит только роли, для которых YouTube прислал значение true.
	var badges []core.ChatBadge
	if details.Get(protobufField(details, "is_chat_owner")).Bool() {
		badges = append(badges, core.ChatBadge{ID: "owner", Name: "Владелец"})
	}
	if details.Get(protobufField(details, "is_chat_moderator")).Bool() {
		badges = append(badges, core.ChatBadge{ID: "moderator", Name: "Модератор"})
	}
	if details.Get(protobufField(details, "is_chat_sponsor")).Bool() {
		badges = append(badges, core.ChatBadge{ID: "member", Name: "Участник канала"})
	}
	if details.Get(protobufField(details, "is_verified")).Bool() {
		badges = append(badges, core.ChatBadge{ID: "verified", Name: "Подтверждённый аккаунт"})
	}
	return author, badges
}

// streamAvatar пропускает адрес value только как абсолютный HTTPS URL без
// учётных данных. Непригодная ссылка не мешает показать сообщение.
func streamAvatar(value string) string {
	if value == "" || len(value) > core.ChatURLBytes || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return ""
	}
	// parsed содержит разобранный URL, err сообщает об ошибке его синтаксиса.
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return ""
	}
	return value
}

// streamTimestamp переводит RFC 3339 из value в платформенную метку UTC,
// которую принимает Hub. Отсутствующее или непригодное время остаётся пустым.
func streamTimestamp(value string) *time.Time {
	// timestamp содержит время площадки, err сообщает об ошибке разбора.
	timestamp, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	timestamp, err = core.NormalizeTimestamp(timestamp)
	if err != nil || timestamp.IsZero() {
		return nil
	}
	return &timestamp
}
