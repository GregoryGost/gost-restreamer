package vkvideolive

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// messageEnvelope принимает только поддерживаемую публикацию чата VK.
type messageEnvelope struct {
	// Type отличает message от других событий и дублирующего message_v8.
	Type string `json:"type"`
	// Data содержит автора, флаги видимости и части сообщения.
	Data *publicationMessage `json:"data"`
}

// publicationMessage описывает сообщение из push.pub.data, где канал уже проверен.
type publicationMessage struct {
	// ID сохраняет исходную форму идентификатора до проверки.
	ID json.RawMessage `json:"id"`
	// Author содержит публичные признаки отправителя и его бейджи.
	Author *publicationAuthor `json:"author"`
	// Parts содержит упорядоченные фрагменты в формате VK.
	Parts []publicationPart `json:"data"`
	// Private отмечает сообщение, которое нельзя передавать в публичную ленту.
	Private bool `json:"isPrivate"`
	// Deleted отмечает удалённое площадкой сообщение.
	Deleted bool `json:"isDeleted"`
}

// publicationAuthor содержит публичные поля автора в сетевом формате VK.
type publicationAuthor struct {
	// ID передаётся как строка либо число и разбирается без float64.
	ID json.RawMessage `json:"id"`
	// DisplayName содержит отображаемое имя VK.
	DisplayName string `json:"displayName"`
	// Nick заменяет отсутствующее DisplayName.
	Nick string `json:"nick"`
	// Name дополняет варианты имени, если первые два отсутствуют.
	Name string `json:"name"`
	// AvatarURL содержит необязательную ссылку на аватар.
	AvatarURL string `json:"avatarUrl"`
	// Badges содержит метки автора в данном чате.
	Badges []publicationBadge `json:"badges"`
}

// publicationImages содержит варианты размера одного изображения VK.
type publicationImages struct {
	// SmallURL содержит ссылку на малое изображение.
	SmallURL string `json:"smallUrl"`
	// MediumURL содержит ссылку на среднее изображение.
	MediumURL string `json:"mediumUrl"`
	// LargeURL содержит ссылку на большое изображение.
	LargeURL string `json:"largeUrl"`
}

// publicationPart содержит поля поддерживаемых типов текстового фрагмента VK.
type publicationPart struct {
	// Type выбирает text, smile, mention или link.
	Type string `json:"type"`
	// Content содержит видимый текст обычного или ссылочного фрагмента.
	Content string `json:"content"`
	// ID содержит исходный идентификатор эмодзи.
	ID json.RawMessage `json:"id"`
	// Name содержит текстовый эквивалент эмодзи.
	Name string `json:"name"`
	// DisplayName содержит отображаемое имя упомянутого автора.
	DisplayName string `json:"displayName"`
	// Nick служит текстом упоминания без DisplayName.
	Nick string `json:"nick"`
	// URL содержит назначение ссылочного фрагмента.
	URL string `json:"url"`
	// publicationImages содержит необязательные варианты изображения эмодзи.
	publicationImages
}

// publicationBadge описывает публичную метку автора.
type publicationBadge struct {
	// ID сохраняет идентификатор бейджа без числового округления.
	ID json.RawMessage `json:"id"`
	// Name содержит читаемое название метки.
	Name string `json:"name"`
	// publicationImages содержит необязательные варианты изображения бейджа.
	publicationImages
}

// normalizePublication преобразует raw в сообщение подтверждённого channelID.
// Неизвестные события и скрытые сообщения пропускаются. Фрагменты сохраняют
// порядок, а текст объединяет их видимое содержимое. Hub проверяет размеры
// нормализованного сообщения и учитывает недопущенные записи.
func normalizePublication(raw json.RawMessage, channelID string) (core.ChatIncomingMessage, bool) {
	// event принимает известную форму сообщения только после проверки типа.
	var event messageEnvelope
	if json.Unmarshal(raw, &event) != nil || event.Type != "message" || event.Data == nil {
		return core.ChatIncomingMessage{}, false
	}
	// data содержит видимость и исходные части публичной публикации.
	data := event.Data
	if data.Private || data.Deleted || data.Author == nil {
		return core.ChatIncomingMessage{}, false
	}
	// messageID сохраняет идентификатор для дедупликации общим Hub, когда VK его передал.
	messageID := identifier(data.ID)
	if messageID == "" && len(data.ID) != 0 && string(data.ID) != "null" && string(data.ID) != `""` {
		return core.ChatIncomingMessage{}, false
	}
	// author содержит публичную идентичность отправителя до проекции.
	author := data.Author
	// message собирает видимые поля для точного канала и общей проверки домена.
	message := core.ChatIncomingMessage{
		Provider: core.ChatVKVideoLive, ChannelID: channelID, ProviderMessageID: messageID, Kind: core.ChatMessageText,
		Author: core.ChatAuthor{
			ID: identifier(author.ID), DisplayName: displayName(author.DisplayName, author.Nick, author.Name), AvatarURL: httpsURL(author.AvatarURL),
		},
	}
	// text объединяет видимые фрагменты в исходном порядке.
	var text strings.Builder
	// fragment содержит очередной фрагмент VK.
	for _, fragment := range data.Parts {
		// part содержит поддерживаемую проекцию. ok отличает её от неизвестного типа или пустого фрагмента.
		part, ok := fragment.normalize()
		if !ok {
			continue
		}
		text.WriteString(part.Text)
		message.Parts = append(message.Parts, part)
		if len(message.Parts) > core.ChatMaxParts {
			// Одного фрагмента сверх предела достаточно, чтобы Hub учёл потерю.
			break
		}
	}
	// badge соответствует очередной метке исходного автора.
	for _, badge := range author.Badges {
		// normalized сохраняет исходную идентичность, текст и пригодное изображение.
		normalized := core.ChatBadge{ID: identifier(badge.ID), Name: badge.Name, ImageURL: badge.httpsImage()}
		if normalized.ID != "" || normalized.Name != "" || normalized.ImageURL != "" {
			message.Badges = append(message.Badges, normalized)
			if len(message.Badges) > core.ChatMaxBadges {
				// Hub учитывает такой же отказ независимо от числа остальных меток.
				break
			}
		}
	}
	message.Text = text.String()
	return message, len(message.Parts) != 0
}

// normalize преобразует поддерживаемый f в текст либо эмодзи. Некорректный
// URL убирается, видимый текст ссылки остаётся доступным.
func (f publicationPart) normalize() (core.ChatPart, bool) {
	// result содержит текстовый фрагмент, пока вид smile не назначит эмодзи.
	result := core.ChatPart{Kind: core.ChatPartText}
	switch f.Type {
	case "text":
		result.Text = f.Content
	case "link":
		result.Text, result.URL = f.Content, httpsURL(f.URL)
	case "mention":
		result.Text = displayName(f.DisplayName, f.Nick)
	case "smile":
		result.Kind = core.ChatPartEmote
		result.EmoteID = identifier(f.ID)
		result.Text = displayName(f.Name, result.EmoteID)
		result.ImageURL = f.httpsImage()
	default:
		return core.ChatPart{}, false
	}
	return result, result.Text != ""
}

// identifier читает исходный opaque ID из raw. Число принимается в форме
// положительной последовательности десятичных цифр без ограничения точности float64.
func identifier(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		// id сохраняет декодированное строковое значение со всеми исходными символами.
		var id string
		if json.Unmarshal(raw, &id) == nil && validRequiredText(id, core.ChatIDBytes) {
			return id
		}
		return ""
	}
	if len(raw) > core.ChatIDBytes || raw[0] < '1' || raw[0] > '9' {
		return ""
	}
	// digit проверяет очередной байт числовой формы без преобразования значения.
	for _, digit := range raw {
		if digit < '0' || digit > '9' {
			return ""
		}
	}
	return string(raw)
}

// displayName возвращает первое непустое имя из values, сохраняя исходный текст.
func displayName(values ...string) string {
	// value является очередным вариантом имени в порядке платформенного приоритета.
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// httpsURL проверяет исходную строку value. Возвращает пригодный адрес HTTPS
// с именем хоста либо пустую строку. Адаптер не запрашивает URL из публикации.
func httpsURL(value string) string {
	if !validRequiredText(value, core.ChatURLBytes) {
		return ""
	}
	// parsed содержит проверенные части адреса. err сообщает о синтаксической ошибке.
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return ""
	}
	return value
}

// httpsImage выбирает первый пригодный HTTPS-адрес изображения из набора i.
func (i publicationImages) httpsImage() string {
	// value содержит очередной размер от малого к большому.
	for _, value := range [...]string{i.SmallURL, i.MediumURL, i.LargeURL} {
		// address содержит пригодный адрес без изменения исходной записи.
		if address := httpsURL(value); address != "" {
			return address
		}
	}
	return ""
}
