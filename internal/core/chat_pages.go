package core

import "context"

// PageChatAccounts читает только конфигурационные метаданные с продолжением в порядке (CreatedAt, ID).
func (s *Service) PageChatAccounts(ctx context.Context, limit int, cursor string) (Page[ChatAccount], error) {
	// request передаёт общий механизм границы страницы без дополнительных фильтров.
	request := PageRequest{Query: ListQuery{Limit: limit}, Cursor: cursor}
	return readPage(ctx, s, "chat-accounts", request, func(r Repositories, q ListQuery) ([]ChatAccount, error) {
		// Читает только метаданные аккаунтов, включая проверочную строку следующей страницы.
		return r.Chat.ChatAccounts(ctx, ChatAccountQuery{Limit: q.Limit, After: q.After})
	}, func(account ChatAccount) ListBoundary {
		// Возвращает неизменную пару, исключаемую из следующей страницы.
		return ListBoundary{CreatedAt: account.CreatedAt, ID: account.ID}
	})
}

// PageDestinationChatSources читает страницу чатов и проверяет существование назначения в том же View.
// Курсор одного назначения нельзя применить к списку другого назначения.
func (s *Service) PageDestinationChatSources(ctx context.Context, destinationID ID, limit int, cursor string) (Page[ChatSource], error) {
	// err обозначает недопустимый идентификатор родителя до чтения базы.
	if err := chatID(destinationID); err != nil {
		return Page[ChatSource]{}, err
	}
	// request задаёт размер страницы и продолжение, а вид списка включает точный ID назначения.
	request := PageRequest{Query: ListQuery{Limit: limit}, Cursor: cursor}
	return readPage(ctx, s, "destination-chat-sources:"+string(destinationID), request, func(r Repositories, q ListQuery) ([]ChatSource, error) {
		// Проверяет назначение и выбирает связанные источники в одном согласованном чтении.
		// err сохраняет отказ чтения родителя, в том числе отсутствие при пустом списке источников.
		if _, err := r.Config.Destination(ctx, destinationID); err != nil {
			return nil, err
		}
		return r.Chat.ChatSources(ctx, ChatSourceQuery{DestinationID: destinationID, Limit: q.Limit, After: q.After})
	}, func(source ChatSource) ListBoundary {
		// Возвращает неизменную пару, исключаемую из следующей страницы.
		return ListBoundary{CreatedAt: source.CreatedAt, ID: source.ID}
	})
}
