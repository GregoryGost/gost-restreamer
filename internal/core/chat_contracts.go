package core

import "context"

// ChatAccountQuery ограничивает страницу аккаунтов и фильтр неизменного провайдера.
type ChatAccountQuery struct {
	// Limit ограничивает материализуемую страницу диапазоном 1…500.
	Limit int
	// Provider выбирает платформу либо оставляет список без этого фильтра.
	Provider ChatProvider
	// After задаёт исключительную границу (CreatedAt, ID).
	After *ListBoundary
}

// Validate проверяет размер страницы, провайдера и точную границу без изменения запроса.
func (q ChatAccountQuery) Validate() error {
	if q.Provider != "" && q.Provider.Validate() != nil {
		return ErrInvalid
	}
	if q.After != nil && chatID(q.After.ID) != nil {
		return ErrInvalid
	}
	return (ListQuery{Limit: q.Limit, After: q.After}).Validate()
}

// ChatSourceQuery содержит только применимые фильтры источников; Stream выводится через Destination.
type ChatSourceQuery struct {
	// Limit ограничивает число строк страницы диапазоном 1…500.
	Limit int
	// Provider выбирает платформу либо не ограничивает её.
	Provider ChatProvider
	// DestinationID ограничивает список одним назначением.
	DestinationID ID
	// AccountID ограничивает список одним аккаунтом.
	AccountID ID
	// StreamID выбирает назначения потока без дублирования StreamID в Source.
	StreamID ID
	// After задаёт исключительную границу возрастающего порядка (CreatedAt, ID).
	After *ListBoundary
}

// Validate проверяет фильтры, размер и границу страницы источников.
func (q ChatSourceQuery) Validate() error {
	// id содержит очередной необязательный фильтр родителя, аккаунта либо потока.
	for _, id := range []ID{q.DestinationID, q.AccountID, q.StreamID} {
		if id != "" && chatID(id) != nil {
			return ErrInvalid
		}
	}
	return (ChatAccountQuery{Limit: q.Limit, Provider: q.Provider, After: q.After}).Validate()
}

// ChatRepository действует только внутри UoW; config reads никогда не возвращают полномочия.
type ChatRepository interface {
	// ChatAccount читает конфигурацию без декодирования credential payload.
	ChatAccount(context.Context, ID) (ChatAccount, error)
	// ChatAccounts возвращает ограниченную страницу конфигураций без полномочий.
	ChatAccounts(context.Context, ChatAccountQuery) ([]ChatAccount, error)
	// InsertChatAccount атомарно сохраняет новый Account и полный набор с двумя ревизиями 1.
	InsertChatAccount(context.Context, ChatCredentialResult) error
	// UpdateChatAccount меняет только metadata при expected config revision и неизменном Provider.
	UpdateChatAccount(context.Context, ChatAccount, uint64) error
	// DeleteChatAccount выполняет config CAS DELETE; связанные Sources запрещают удаление.
	DeleteChatAccount(context.Context, ID, uint64) error
	// ChatCredentials явно читает полный серверный набор с квитанцией последней записи.
	ChatCredentials(context.Context, ID) (ChatCredentials, error)
	// ReplaceChatCredentials проверяет config CAS и увеличивает обе актуальные ревизии одним UPDATE.
	ReplaceChatCredentials(context.Context, ChatCredentialResult, uint64) error
	// RefreshChatCredentials меняет только credential-область при expected credential revision.
	RefreshChatCredentials(context.Context, ChatCredentials, uint64) error
	// ChatSource читает конфигурацию источника без полномочий его аккаунта.
	ChatSource(context.Context, ID) (ChatSource, error)
	// ChatSources читает ограниченную страницу с типизированными фильтрами и стабильным порядком.
	ChatSources(context.Context, ChatSourceQuery) ([]ChatSource, error)
	// InsertChatSource сохраняет обязательные связи; SQLite независимо проверяет provider и FK.
	InsertChatSource(context.Context, ChatSource) error
	// UpdateChatSource атомарно заменяет настройки при config CAS, сохраняя Destination и CreatedAt.
	UpdateChatSource(context.Context, ChatSource, uint64) error
	// DeleteChatSource выполняет условное удаление по конфигурационной ревизии.
	DeleteChatSource(context.Context, ID, uint64) error
}
