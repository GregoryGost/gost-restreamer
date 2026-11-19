package core

import (
	"encoding/json"
	"reflect"
	"time"
)

// YouTubeChatCredentials хранит серверный ключ чтения публичного чата.
type YouTubeChatCredentials struct {
	// APIKey содержит полный проверенный ключ Google Cloud без нормализации.
	APIKey string
}

// TwitchChatCredentials хранит полную атомарно заменяемую пару и её получателя.
type TwitchChatCredentials struct {
	// ClientID идентифицирует Public-приложение Device Code.
	ClientID string
	// UserID закрепляет пользователя, которому выданы полномочия чтения.
	UserID string
	// AccessToken содержит действующий либо истёкший access token; срок не проверяется по часам.
	AccessToken string
	// RefreshToken сохраняется одновременно с AccessToken после обмена одноразового токена.
	RefreshToken string
	// Scopes содержит подтверждённые разрешения, включая user:read:chat.
	Scopes []string
	// ExpiresAt содержит срок access token в UTC с точностью до микросекунд.
	ExpiresAt time.Time
}

// VKVideoLiveChatCredentials хранит реквизиты DevAPI и полный результат выдачи app-токена.
type VKVideoLiveChatCredentials struct {
	// ClientID идентифицирует зарегистрированное приложение DevAPI.
	ClientID string
	// ClientSecret содержит полный секрет приложения без преобразований.
	ClientSecret string
	// AccessToken содержит app-токен, сохраняемый вместе со сроком.
	AccessToken string
	// ExpiresAt содержит срок app-токена; истечение не делает запись повреждённой.
	ExpiresAt time.Time
}

// ChatCredentialBundle содержит ровно один серверный набор; в ChatAccount этот тип не встраивается.
type ChatCredentialBundle struct {
	// YouTube содержит API Key только для YouTube.
	YouTube *YouTubeChatCredentials `json:",omitempty"`
	// Twitch содержит пользовательские полномочия только для Twitch.
	Twitch *TwitchChatCredentials `json:",omitempty"`
	// VKVideoLive содержит реквизиты приложения только для VK Видео Live.
	VKVideoLive *VKVideoLiveChatCredentials `json:",omitempty"`
}

// chatTime требует ненулевую представимую микросекундную метку UTC без сравнения с текущими часами.
func chatTime(value time.Time) error {
	// normalized содержит проверенное UTC-время, err — причину непредставимости.
	normalized, err := NormalizeTimestamp(value)
	if err != nil || value.IsZero() || !normalized.Equal(value) || value.Location() != time.UTC {
		return ErrInvalid
	}
	return nil
}

// Validate проверяет закрытый provider-specific набор и его конечный JSON-бюджет.
// Сетевая действительность и фактическая принадлежность каналу проверяются вызывающим адаптером.
func (b ChatCredentialBundle) Validate(provider ChatProvider) error {
	// texts содержит обязательные поля выбранного набора; expires указывает срок токена при его наличии.
	var texts []string
	var expires *time.Time
	switch provider {
	case ChatYouTube:
		if b.YouTube == nil || b.Twitch != nil || b.VKVideoLive != nil {
			return ErrInvalid
		}
		texts = []string{b.YouTube.APIKey}
	case ChatTwitch:
		if b.Twitch == nil || b.YouTube != nil || b.VKVideoLive != nil {
			return ErrInvalid
		}
		if !chatDecimal(b.Twitch.UserID) || chatText(b.Twitch.UserID, ChatIDBytes, true) != nil || chatText(b.Twitch.ClientID, ChatIDBytes, true) != nil {
			return ErrInvalid
		}
		texts = []string{b.Twitch.ClientID, b.Twitch.UserID, b.Twitch.AccessToken, b.Twitch.RefreshToken}
		expires = &b.Twitch.ExpiresAt
		// scopeBytes ограничивает исходную сумму до выделения encoded-представления большого набора.
		scopeBytes := uint64(0)
		// found отмечает обязательное разрешение чтения; scope содержит очередное сохранённое разрешение.
		found := false
		for _, scope := range b.Twitch.Scopes {
			if chatText(scope, ChatCredentialBytes, true) != nil {
				return ErrInvalid
			}
			// total содержит новый проверенный вес строк и JSON-разделителей, err — переполнение.
			total, err := CheckedChatBytes(scopeBytes, uint64(len(scope))+3)
			if err != nil || total > ChatCredentialBytes {
				return ErrInvalid
			}
			scopeBytes = total
			found = found || scope == "user:read:chat"
		}
		if !found {
			return ErrInvalid
		}
	case ChatVKVideoLive:
		if b.VKVideoLive == nil || b.YouTube != nil || b.Twitch != nil {
			return ErrInvalid
		}
		if chatText(b.VKVideoLive.ClientID, ChatIDBytes, true) != nil {
			return ErrInvalid
		}
		texts = []string{b.VKVideoLive.ClientID, b.VKVideoLive.ClientSecret, b.VKVideoLive.AccessToken}
		expires = &b.VKVideoLive.ExpiresAt
	default:
		return ErrInvalid
	}
	// text содержит очередное обязательное поле без скрытого trim или перекодирования.
	for _, text := range texts {
		if chatText(text, ChatCredentialBytes, true) != nil {
			return ErrInvalid
		}
	}
	if expires != nil && chatTime(*expires) != nil {
		return ErrInvalid
	}
	// encoded задаёт размер полного набора с escaping; err сообщает отказ кодирования.
	encoded, err := json.Marshal(b)
	if err != nil || len(encoded) > ChatCredentialBytes {
		return ErrInvalid
	}
	return nil
}

// ChatCredentials представляет явное серверное чтение полного набора и квитанции последней записи.
type ChatCredentials struct {
	// AccountID идентифицирует аккаунт, которому принадлежат полномочия.
	AccountID ID
	// Provider закрепляет неизменный тип аккаунта.
	Provider ChatProvider
	// CredentialRevision различает сохранённые наборы независимо от конфигурационной ревизии.
	CredentialRevision uint64
	// CredentialUpdatedAt содержит время последней принятой записи полного набора.
	CredentialUpdatedAt time.Time
	// CredentialWriteID позволяет сверить результат неоднозначного COMMIT вместе с версиями и Bundle.
	CredentialWriteID ID
	// Bundle содержит полный provider-specific результат, недоступный обычному чтению Account.
	Bundle ChatCredentialBundle
}

// Validate проверяет метаданные явного серверного чтения и форму полного набора.
func (c ChatCredentials) Validate() error {
	if chatID(c.AccountID) != nil || chatID(c.CredentialWriteID) != nil || c.CredentialRevision == 0 || chatTime(c.CredentialUpdatedAt) != nil {
		return ErrInvalid
	}
	return c.Bundle.Validate(c.Provider)
}

// ChatCredentialResult объединяет конфигурационную и credential-ревизии только для серверного owner.
type ChatCredentialResult struct {
	// Account содержит конфигурационный результат без встроенных полномочий.
	Account ChatAccount
	// Credentials содержит полный результат и квитанцию записи для сверки после ошибки COMMIT.
	Credentials ChatCredentials
}

// Matches подтверждает полное совпадение сохранённого результата с ожидаемым после неоднозначного COMMIT.
// Одного совпадения WriteID недостаточно: проверяются обе ревизии, времена, конфигурация и весь набор.
func (r ChatCredentialResult) Matches(expected ChatCredentialResult) bool {
	return r.Validate() == nil && expected.Validate() == nil && reflect.DeepEqual(r, expected)
}

// Validate проверяет согласованность конфигурации и серверного набора до использования квитанции.
func (r ChatCredentialResult) Validate() error {
	if r.Account.Validate() != nil || r.Account.Record.Validate() != nil || chatID(r.Account.ID) != nil || r.Credentials.Validate() != nil || r.Account.ID != r.Credentials.AccountID || r.Account.Provider != r.Credentials.Provider {
		return ErrInvalid
	}
	return nil
}
