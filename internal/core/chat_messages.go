package core

import (
	"math"
	"time"
)

// ChatMessageKind различает сообщение аудитории и поддерживаемое системное событие площадки.
type ChatMessageKind string

const (
	// ChatMessageText обозначает обычное сообщение чата.
	ChatMessageText ChatMessageKind = "message"
	// ChatMessageSystem обозначает распознанное системное событие, допустимое в ленте.
	ChatMessageSystem ChatMessageKind = "supported_system_event"
)

// ChatPartKind задаёт закрытый вид безопасного структурированного фрагмента.
type ChatPartKind string

const (
	// ChatPartText передаёт исходный текст без HTML.
	ChatPartText ChatPartKind = "text"
	// ChatPartEmote передаёт текстовое обозначение и необязательное изображение эмодзи.
	ChatPartEmote ChatPartKind = "emote"
)

// ChatAuthor содержит только публичные атрибуты автора, без платформенных полномочий.
type ChatAuthor struct {
	// ID сохраняет исходный идентификатор автора, если платформа его предоставила.
	ID string
	// DisplayName содержит исходное отображаемое имя без HTML-интерпретации.
	DisplayName string
	// Color сохраняет необязательное платформенное обозначение цвета; CSS-рендеринг принадлежит UI.
	Color string
	// AvatarURL содержит необязательный HTTPS-адрес изображения автора.
	AvatarURL string
}

// ChatPart хранит текст либо распознанное эмодзи; HTML в этой модели отсутствует.
type ChatPart struct {
	// Kind выбирает text либо emote.
	Kind ChatPartKind
	// Text сохраняет исходный текст или текстовый эквивалент эмодзи.
	Text string
	// EmoteID содержит необязательный платформенный ID для emote.
	EmoteID string
	// ImageURL содержит необязательное HTTPS-изображение только для emote.
	ImageURL string
	// URL содержит необязательную безопасную HTTPS-ссылку фрагмента.
	URL string
}

// ChatBadge содержит публичную метку автора и безопасные ссылки без HTML-разметки.
type ChatBadge struct {
	// ID сохраняет исходную идентичность метки площадки.
	ID string
	// Name содержит читаемое название метки.
	Name string
	// ImageURL содержит необязательное HTTPS-изображение метки.
	ImageURL string
	// URL содержит необязательную HTTPS-ссылку метки.
	URL string
}

// ChatIncomingMessage содержит нормализованные данные адаптера без доверенных полей Manager/Hub.
type ChatIncomingMessage struct {
	// Provider указывает источник платформенных данных.
	Provider ChatProvider
	// ChannelID содержит фактический liveChatID, broadcasterID либо channelID.
	ChannelID string
	// ProviderMessageID сохраняет исходный ID, если платформа его предоставляет.
	ProviderMessageID string
	// ProviderTimestamp сохраняет необязательное достоверное время площадки отдельно от серверного порядка.
	ProviderTimestamp *time.Time
	// Author содержит публичные атрибуты отправителя.
	Author ChatAuthor
	// Text содержит исходный безопасный текст без скрытой нормализации.
	Text string
	// Parts содержит ограниченные структурированные фрагменты.
	Parts []ChatPart
	// Badges содержит ограниченные публичные метки автора.
	Badges []ChatBadge
	// Kind выбирает сообщение либо поддерживаемое системное событие.
	Kind ChatMessageKind
}

// CheckedChatBytes складывает логические веса без переполнения uint64.
// Метод не измеряет heap/RSS: его используют владельцы ограниченных RAM-структур.
func CheckedChatBytes(current, additional uint64) (uint64, error) {
	if additional > math.MaxUint64-current {
		return 0, ErrInvalid
	}
	return current + additional, nil
}

// LogicalBytes считает 256 байт записи, её строки и по 64 байта каждого part/badge с их строками.
// Каждая удерживаемая запись учитывается владельцем один раз; наличие других копий не исключается.
func (m ChatIncomingMessage) LogicalBytes() (uint64, error) {
	// total накапливает вес записи, err сообщает переполнение любого слагаемого.
	total := uint64(256)
	var err error
	// text содержит очередную удерживаемую строку верхнего уровня.
	for _, text := range []string{string(m.Provider), m.ChannelID, m.ProviderMessageID, m.Author.ID, m.Author.DisplayName, m.Author.Color, m.Author.AvatarURL, m.Text, string(m.Kind)} {
		total, err = CheckedChatBytes(total, uint64(len(text)))
		if err != nil {
			return 0, err
		}
	}
	// part содержит очередной фрагмент, его фиксированный вес и строки учитываются отдельно.
	for _, part := range m.Parts {
		total, err = CheckedChatBytes(total, 64)
		if err != nil {
			return 0, err
		}
		// text содержит очередное строковое поле фрагмента.
		for _, text := range []string{string(part.Kind), part.Text, part.EmoteID, part.ImageURL, part.URL} {
			total, err = CheckedChatBytes(total, uint64(len(text)))
			if err != nil {
				return 0, err
			}
		}
	}
	// badge содержит очередную метку и её публичные строки.
	for _, badge := range m.Badges {
		total, err = CheckedChatBytes(total, 64)
		if err != nil {
			return 0, err
		}
		// text содержит очередное строковое поле метки.
		for _, text := range []string{badge.ID, badge.Name, badge.ImageURL, badge.URL} {
			total, err = CheckedChatBytes(total, uint64(len(text)))
			if err != nil {
				return 0, err
			}
		}
	}
	return total, nil
}

// Validate проверяет структурные поля, ссылки и предварительную сумму байтов сообщения.
// Окончательный размер JSON после escaping и добавления полей Hub проверяет будущая граница выдачи.
func (m ChatIncomingMessage) Validate() error {
	if m.Provider.Validate() != nil || chatText(m.ChannelID, ChatIDBytes, true) != nil || chatText(m.ProviderMessageID, ChatIDBytes, false) != nil || len(m.Parts) > ChatMaxParts || len(m.Badges) > ChatMaxBadges {
		return ErrInvalid
	}
	if m.Kind != ChatMessageText && m.Kind != ChatMessageSystem {
		return ErrInvalid
	}
	if m.ProviderTimestamp != nil && chatTime(*m.ProviderTimestamp) != nil {
		return ErrInvalid
	}
	if chatText(m.Author.ID, ChatIDBytes, false) != nil || chatText(m.Author.DisplayName, ChatNameBytes, false) != nil || chatText(m.Author.Color, ChatNameBytes, false) != nil || chatURL(m.Author.AvatarURL) != nil || chatText(m.Text, ChatMessageBytes, false) != nil {
		return ErrInvalid
	}
	// part содержит очередной текстовый фрагмент либо эмодзи для проверки закрытого вида и URL.
	for _, part := range m.Parts {
		if part.Kind != ChatPartText && part.Kind != ChatPartEmote {
			return ErrInvalid
		}
		if part.Kind == ChatPartText && (part.EmoteID != "" || part.ImageURL != "") {
			return ErrInvalid
		}
		if chatText(part.Text, ChatMessageBytes, false) != nil || chatText(part.EmoteID, ChatIDBytes, false) != nil || chatURL(part.ImageURL) != nil || chatURL(part.URL) != nil {
			return ErrInvalid
		}
	}
	// badge содержит очередную публичную метку для проверки её бюджета и HTTPS-адресов.
	for _, badge := range m.Badges {
		if chatText(badge.ID, ChatIDBytes, false) != nil || chatText(badge.Name, ChatNameBytes, false) != nil || chatURL(badge.ImageURL) != nil || chatURL(badge.URL) != nil {
			return ErrInvalid
		}
	}
	// weight проверяет конечность совокупного предварительного размера, err — переполнение.
	weight, err := m.LogicalBytes()
	if err != nil || weight > ChatMessageBytes {
		return ErrInvalid
	}
	return nil
}

// ChatMessage добавляет поля, которые назначает Hub после проверки точного worker token.
type ChatMessage struct {
	// ChatIncomingMessage содержит только проверенные публичные данные площадки.
	ChatIncomingMessage
	// SessionID задаёт точный запуск, в котором принято сообщение.
	SessionID ID
	// ChatSourceID содержит минимальный текущий допущенный SourceID общего reader при первом приёме.
	ChatSourceID ID
	// Seq задаёт серверный порядок получения в пределах эпохи, без float64-преобразования.
	Seq uint64
	// ReceivedAt содержит UTC-время принятия сервером с точностью до микросекунд.
	ReceivedAt time.Time
}

// Validate проверяет структурную форму сообщения; финальный encoded-limit остаётся обязанностью Hub/API.
func (m ChatMessage) Validate() error {
	if chatID(m.SessionID) != nil || chatID(m.ChatSourceID) != nil || m.Seq == 0 || chatTime(m.ReceivedAt) != nil {
		return ErrInvalid
	}
	return m.ChatIncomingMessage.Validate()
}

// LogicalBytes добавляет серверные идентификаторы к логическому весу нормализованного сообщения.
func (m ChatMessage) LogicalBytes() (uint64, error) {
	// weight содержит уже проверенный вес платформенных полей, err — переполнение.
	weight, err := m.ChatIncomingMessage.LogicalBytes()
	if err != nil {
		return 0, err
	}
	weight, err = CheckedChatBytes(weight, uint64(len(m.SessionID)))
	if err != nil {
		return 0, err
	}
	return CheckedChatBytes(weight, uint64(len(m.ChatSourceID)))
}
