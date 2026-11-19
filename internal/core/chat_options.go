package core

import (
	"bytes"
	"encoding/json"
	"io"
)

// YouTubeChatOptions не сохраняет выбранное видео: выбор принадлежит текущей Session.
type YouTubeChatOptions struct{}

// TwitchChatOptions резервирует закрытую форму без дополнительных постоянных переключателей.
type TwitchChatOptions struct{}

// VKVideoLiveChatOptions задаёт закрытую начальную форму настроек DevAPI-чата.
type VKVideoLiveChatOptions struct{}

// ChatOptions хранит версию и ровно один вариант платформы; nil отличается от пустого объекта.
type ChatOptions struct {
	// Version задаёт единственную поддерживаемую версию постоянных настроек.
	Version int `json:"version"`
	// YouTube присутствует только у YouTube и не содержит постоянного video ID.
	YouTube *YouTubeChatOptions `json:"youtube,omitempty"`
	// Twitch присутствует только у Twitch.
	Twitch *TwitchChatOptions `json:"twitch,omitempty"`
	// VKVideoLive присутствует только у VK Видео Live.
	VKVideoLive *VKVideoLiveChatOptions `json:"vk_video_live,omitempty"`
}

// NewChatOptions создаёт явный пустой вариант версии 1; неизвестный provider отклоняется.
func NewChatOptions(provider ChatProvider) (ChatOptions, error) {
	// result содержит единственный выбранный вариант до проверки провайдера.
	result := ChatOptions{Version: 1}
	switch provider {
	case ChatYouTube:
		result.YouTube = &YouTubeChatOptions{}
	case ChatTwitch:
		result.Twitch = &TwitchChatOptions{}
	case ChatVKVideoLive:
		result.VKVideoLive = &VKVideoLiveChatOptions{}
	default:
		return ChatOptions{}, ErrInvalid
	}
	return result, nil
}

// Validate требует version1 и точное соответствие единственного варианта переданному provider.
func (o ChatOptions) Validate(provider ChatProvider) error {
	if o.Version != 1 {
		return ErrInvalid
	}
	switch provider {
	case ChatYouTube:
		if o.YouTube != nil && o.Twitch == nil && o.VKVideoLive == nil {
			return nil
		}
	case ChatTwitch:
		if o.Twitch != nil && o.YouTube == nil && o.VKVideoLive == nil {
			return nil
		}
	case ChatVKVideoLive:
		if o.VKVideoLive != nil && o.YouTube == nil && o.Twitch == nil {
			return nil
		}
	}
	return ErrInvalid
}

// UnmarshalJSON читает закрытую форму настроек без defaults, неизвестных полей и лишних вариантов.
// Получатель меняется лишь после полной проверки; даже явно переданный лишний null-вариант запрещён.
func (o *ChatOptions) UnmarshalJSON(data []byte) error {
	// raw сохраняет точный набор полей без повторяющихся ключей, err сообщает о нарушении JSON-объекта.
	raw, err := chatJSONObject(data)
	if err != nil {
		return err
	}
	if len(raw) != 2 || raw["version"] == nil {
		return ErrInvalid
	}
	// key содержит точное имя поля; регистр и каталог не расширяются правилами encoding/json.
	for key := range raw {
		if key != "version" && key != "youtube" && key != "twitch" && key != "vk_video_live" {
			return ErrInvalid
		}
	}
	// plain исключает повторный вызов UnmarshalJSON для вложенных типизированных вариантов.
	type plain ChatOptions
	// result принимает строгую форму, не меняя прежний получатель при отказе.
	var result plain
	// err сообщает о неизвестных полях, в том числе во вложенном пустом варианте.
	if err := strictJSON(data, &result); err != nil {
		return err
	}
	if result.Version != 1 {
		return ErrInvalid
	}
	if result.YouTube != nil && result.Twitch == nil && result.VKVideoLive == nil || result.Twitch != nil && result.YouTube == nil && result.VKVideoLive == nil || result.VKVideoLive != nil && result.YouTube == nil && result.Twitch == nil {
		*o = ChatOptions(result)
		return nil
	}
	return ErrInvalid
}

// chatJSONObject читает ровно один объект, отклоняя повторные ключи, null и последующие значения.
// Значения остаются RawMessage до типизированной проверки вызывающим кодом.
func chatJSONObject(data []byte) (map[string]json.RawMessage, error) {
	// decoder читает токены объекта без преобразования чисел через float64.
	decoder := json.NewDecoder(bytes.NewReader(data))
	// opening содержит начальный токен, err — отказ чтения.
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, ErrInvalid
	}
	// fields накапливает уникальные точные ключи и их необработанные значения.
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		// token содержит очередной ключ, tokenErr — причину отказа токенизации.
		token, tokenErr := decoder.Token()
		if tokenErr != nil {
			return nil, tokenErr
		}
		// key содержит строковое имя поля, ok подтверждает допустимый ключ объекта.
		key, ok := token.(string)
		if !ok || fields[key] != nil {
			return nil, ErrInvalid
		}
		// value принимает точные байты одного значения поля.
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err = decoder.Token(); err != nil {
		return nil, err
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return nil, ErrInvalid
	}
	return fields, nil
}
