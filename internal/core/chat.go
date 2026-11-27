package core

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// ChatProvider задаёт закрытый каталог платформ чтения чата независимо от Destination.Platform.
type ChatProvider string

const (
	// ChatYouTube выбирает публичный чат YouTube с серверным API Key.
	ChatYouTube ChatProvider = "youtube"
	// ChatTwitch выбирает Twitch EventSub с пользовательскими полномочиями чтения.
	ChatTwitch ChatProvider = "twitch"
	// ChatVKVideoLive выбирает чат VK Видео Live с реквизитами DevAPI.
	ChatVKVideoLive ChatProvider = "vk_video_live"
	// ChatIDBytes ограничивает идентификаторы и ссылки чатов в байтах UTF-8.
	ChatIDBytes = 256
	// ChatNameBytes ограничивает отображаемые имена в байтах UTF-8.
	ChatNameBytes = 512
	// ChatURLBytes ограничивает адрес ссылки или изображения в байтах UTF-8.
	ChatURLBytes = 4096
	// ChatMessageBytes ограничивает полное конечное JSON-представление сообщения.
	ChatMessageBytes = 64 << 10
	// ChatCredentialBytes ограничивает сохраняемый полный набор реквизитов.
	ChatCredentialBytes = 64 << 10
	// ChatMaxParts ограничивает число структурированных фрагментов сообщения.
	ChatMaxParts = 256
	// ChatMaxBadges ограничивает число меток автора сообщения.
	ChatMaxBadges = 64
)

// Validate разрешает только три согласованных провайдера; наличие runtime-адаптера не требуется.
func (p ChatProvider) Validate() error {
	switch p {
	case ChatYouTube, ChatTwitch, ChatVKVideoLive:
		return nil
	default:
		return fmt.Errorf("%w: провайдер чата", ErrInvalid)
	}
}

// chatText проверяет конечную UTF-8 строку без NUL и не меняет её байты.
// required запрещает пустой или пробельный текст; limit задаёт максимальное число байтов.
func chatText(value string, limit int, required bool) error {
	if len(value) > limit || !utf8.ValidString(value) || strings.ContainsRune(value, 0) || (required && strings.TrimSpace(value) == "") {
		return fmt.Errorf("%w: текстовое поле чата", ErrInvalid)
	}
	return nil
}

// chatID проверяет обязательную ссылку чата с отдельным конечным бюджетом.
func chatID(id ID) error { return chatText(string(id), ChatIDBytes, true) }

// chatURL проверяет необязательный публичный HTTPS-адрес с непустым hostname.
func chatURL(value string) error {
	if value == "" {
		return nil
	}
	// err сообщает о нарушении текстового бюджета URL до разбора адреса.
	if err := chatText(value, ChatURLBytes, true); err != nil {
		return err
	}
	// parsed содержит разобранный адрес, err — причину ошибки синтаксиса.
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return ErrInvalid
	}
	return nil
}

// chatDecimal проверяет форму непустого платформенного десятичного ID без ограничения uint64.
func chatDecimal(value string) bool {
	if value == "" {
		return false
	}
	// digit содержит очередную ASCII-цифру проверяемого идентификатора.
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

// ChatAccount содержит только конфигурационные метаданные; полномочия читаются отдельным портом.
type ChatAccount struct {
	// Record хранит конфигурационную ревизию, идентичность и времена записи.
	Record
	// Provider неизменно задаёт способ авторизации аккаунта.
	Provider ChatProvider
	// Name содержит необязательное имя для настройки и диагностики.
	Name string
}

// Validate проверяет конфигурацию аккаунта без требования уже назначенной Record.
func (a ChatAccount) Validate() error {
	// err сообщает о неизвестной платформе аккаунта.
	if err := a.Provider.Validate(); err != nil {
		return err
	}
	return chatText(a.Name, ChatNameBytes, false)
}

// ChatSource хранит текущую настройку чтения одного Destination без оперативного состояния.
type ChatSource struct {
	// Record содержит конфигурационную ревизию и неизменное время создания.
	Record
	// DestinationID закрепляет единственного неизменного родителя источника.
	DestinationID ID
	// AccountID ссылается на аккаунт того же Provider.
	AccountID ID
	// Provider задаёт платформу независимо от свободного Destination.Platform.
	Provider ChatProvider
	// Name содержит необязательное отображаемое имя источника.
	Name string
	// Enabled явно разрешает чтение источника при подходящем выходе.
	Enabled bool
	// ChannelHint хранит постоянный канал, но не выбранный эфир YouTube.
	ChannelHint string
	// Options содержит ровно один соответствующий Provider вариант версии 1.
	Options ChatOptions
}

// Validate проверяет ссылки, варианты и форму канала без назначения defaults или сетевой проверки.
func (s ChatSource) Validate() error {
	// id содержит очередную обязательную ссылку на Destination либо Account.
	for _, id := range []ID{s.DestinationID, s.AccountID} {
		// err сообщает об ошибке ссылки источника.
		if err := chatID(id); err != nil {
			return err
		}
	}
	// err сообщает о недопустимом необязательном имени.
	if err := chatText(s.Name, ChatNameBytes, false); err != nil {
		return err
	}
	// err сообщает о несогласованных провайдере и варианте настроек.
	if err := s.Options.Validate(s.Provider); err != nil {
		return err
	}
	// err ограничивает постоянный канал до платформенной проверки формы.
	if err := chatText(s.ChannelHint, ChatIDBytes, s.Provider != ChatTwitch); err != nil {
		return err
	}
	switch s.Provider {
	case ChatYouTube:
		if !strings.HasPrefix(s.ChannelHint, "UC") || len(s.ChannelHint) <= 2 {
			return ErrInvalid
		}
	case ChatTwitch:
		if s.ChannelHint != "" && !chatDecimal(s.ChannelHint) {
			return ErrInvalid
		}
	case ChatVKVideoLive:
		if strings.ContainsAny(s.ChannelHint, "/\\?#") || strings.Contains(s.ChannelHint, "://") {
			return ErrInvalid
		}
	}
	return nil
}
