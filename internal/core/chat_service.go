package core

import (
	"context"
	"time"
)

// CreateChatAccount сохраняет уже проверенный вызывающим backend набор без внешнего I/O в Write.
// writeID создаётся вызывающим до записи и сохраняется при повторе того же результата.
// Результат при ошибке COMMIT остаётся ожидаемым значением для последующей явной сверки.
func (s *Service) CreateChatAccount(ctx context.Context, value ChatAccount, bundle ChatCredentialBundle, writeID ID) (ChatCredentialResult, error) {
	if value.Validate() != nil || bundle.Validate(value.Provider) != nil || chatID(writeID) != nil {
		return ChatCredentialResult{}, ErrInvalid
	}
	// record задаёт новую идентичность, обе начальные метки и config revision; err сообщает отказ случайного ID.
	record, err := newRecord()
	if err != nil {
		return ChatCredentialResult{}, err
	}
	value.Record = record
	// result содержит полный неизменный ожидаемый результат всех разрешённых повторов DB-записи.
	result := ChatCredentialResult{Account: value, Credentials: ChatCredentials{AccountID: value.ID, Provider: value.Provider, CredentialRevision: 1, CredentialUpdatedAt: value.CreatedAt, CredentialWriteID: writeID, Bundle: bundle}}
	// Callback r вставляет только данные в одной транзакции, не выполняя авторизацию.
	err = s.uow.Write(ctx, func(r Repositories) error { return r.Chat.InsertChatAccount(ctx, result) })
	return result, err
}

// UpdateChatAccount меняет имя при ожидаемой config revision, сохраняя Provider и все полномочия.
func (s *Service) UpdateChatAccount(ctx context.Context, value ChatAccount, expected uint64) (ChatAccount, error) {
	if value.Validate() != nil || chatID(value.ID) != nil || expected == 0 {
		return ChatAccount{}, ErrInvalid
	}
	// err передаёт результат транзакции; callback r читает актуальную конфигурацию и меняет только её область.
	err := s.uow.Write(ctx, func(r Repositories) error {
		// current содержит действующие metadata, err — результат их чтения.
		current, err := r.Chat.ChatAccount(ctx, value.ID)
		if err != nil {
			return err
		}
		if current.Revision != expected || current.Provider != value.Provider {
			return ErrConflict
		}
		value.Record, err = revise(current.Record)
		if err != nil {
			return err
		}
		return r.Chat.UpdateChatAccount(ctx, value, expected)
	})
	return value, err
}

// DeleteChatAccount удаляет Account по config revision; используемый аккаунт защищён FK от Sources.
func (s *Service) DeleteChatAccount(ctx context.Context, id ID, expected uint64) error {
	if chatID(id) != nil || expected == 0 {
		return ErrInvalid
	}
	// Callback r передаёт условное удаление в репозиторий текущей Write.
	return s.uow.Write(ctx, func(r Repositories) error { return r.Chat.DeleteChatAccount(ctx, id, expected) })
}

// GetChatAccount возвращает только конфигурацию аккаунта в согласованном чтении.
func (s *Service) GetChatAccount(ctx context.Context, id ID) (ChatAccount, error) {
	if chatID(id) != nil {
		return ChatAccount{}, ErrInvalid
	}
	// value принимает конфигурацию, err — итог View; callback r не читает credential payload.
	var value ChatAccount
	err := s.uow.View(ctx, func(r Repositories) error {
		// readErr передаёт отказ явного чтения конфигурации в View.
		var readErr error
		value, readErr = r.Chat.ChatAccount(ctx, id)
		return readErr
	})
	return value, err
}

// ListChatAccounts возвращает ограниченную страницу метаданных без полномочий.
func (s *Service) ListChatAccounts(ctx context.Context, query ChatAccountQuery) ([]ChatAccount, error) {
	// err отклоняет неверный запрос до чтения.
	if err := query.Validate(); err != nil {
		return nil, err
	}
	// values принимает страницу, err — итог View; callback r материализует только config.
	var values []ChatAccount
	err := s.uow.View(ctx, func(r Repositories) error {
		// readErr сохраняет отказ чтения ограниченной страницы.
		var readErr error
		values, readErr = r.Chat.ChatAccounts(ctx, query)
		return readErr
	})
	return values, err
}

// GetChatCredentials явно читает серверный полный набор; метод не предназначен для публичных DTO.
func (s *Service) GetChatCredentials(ctx context.Context, id ID) (ChatCredentials, error) {
	if chatID(id) != nil {
		return ChatCredentials{}, ErrInvalid
	}
	// value принимает полный набор, err — итог View; callback r использует только явный credential-порт.
	var value ChatCredentials
	err := s.uow.View(ctx, func(r Repositories) error {
		// readErr передаёт отказ credential-чтения, включая повреждённый набор.
		var readErr error
		value, readErr = r.Chat.ChatCredentials(ctx, id)
		return readErr
	})
	return value, err
}

// GetChatCredentialResult читает обе области одним snapshot для сверки неоднозначного COMMIT.
// Owner сравнивает весь результат через Matches до следующей credential-операции.
func (s *Service) GetChatCredentialResult(ctx context.Context, id ID) (ChatCredentialResult, error) {
	if chatID(id) != nil {
		return ChatCredentialResult{}, ErrInvalid
	}
	// result принимает согласованные области; err — итог View, callback r читает их в одной транзакции.
	var result ChatCredentialResult
	err := s.uow.View(ctx, func(r Repositories) error {
		// readErr передаёт отказ любой части серверного чтения.
		var readErr error
		result.Account, readErr = r.Chat.ChatAccount(ctx, id)
		if readErr != nil {
			return readErr
		}
		result.Credentials, readErr = r.Chat.ChatCredentials(ctx, id)
		return readErr
	})
	return result, err
}

// ReplaceChatCredentials принимает вручную перепроверенный полный набор при ожидаемой config revision.
// Актуальная credential revision читается внутри Write, поэтому законный refresh во время внешней
// проверки не отменяет замену. Обе ревизии увеличиваются атомарно; старый refresh затем получает ErrStale.
func (s *Service) ReplaceChatCredentials(ctx context.Context, id ID, provider ChatProvider, bundle ChatCredentialBundle, writeID ID, expected uint64) (ChatCredentialResult, error) {
	if chatID(id) != nil || chatID(writeID) != nil || expected == 0 || bundle.Validate(provider) != nil {
		return ChatCredentialResult{}, ErrInvalid
	}
	// now задаёт стабильное время принятия для всех допустимых повторов DB-транзакции.
	now := time.Now().UTC().Truncate(time.Microsecond)
	// result содержит ожидаемые обе области даже при неоднозначной ошибке COMMIT.
	var result ChatCredentialResult
	// err передаёт итог Write; callback r читает обе текущие ревизии и сохраняет замену одним UPDATE.
	err := s.uow.Write(ctx, func(r Repositories) error {
		// current содержит актуальный Account, readErr — отказ чтения или увеличения счётчика.
		current, readErr := r.Chat.ChatAccount(ctx, id)
		if readErr != nil {
			return readErr
		}
		if current.Revision != expected || current.Provider != provider {
			return ErrConflict
		}
		// credentials содержит текущий полный набор для следующей credential revision.
		credentials, readErr := r.Chat.ChatCredentials(ctx, id)
		if readErr != nil {
			return readErr
		}
		current.Revision, readErr = NextRevision(current.Revision)
		if readErr != nil {
			return readErr
		}
		credentials.CredentialRevision, readErr = NextRevision(credentials.CredentialRevision)
		if readErr != nil {
			return readErr
		}
		current.UpdatedAt = now
		credentials.Bundle, credentials.CredentialWriteID, credentials.CredentialUpdatedAt = bundle, writeID, now
		result = ChatCredentialResult{Account: current, Credentials: credentials}
		return r.Chat.ReplaceChatCredentials(ctx, result, expected)
	})
	return result, err
}

// RefreshChatCredentials сохраняет результат уже выполненного внешнего обмена при credential CAS.
// Config Record и имя остаются прежними; повтор одноразового внешнего refresh внутри Write запрещён.
func (s *Service) RefreshChatCredentials(ctx context.Context, id ID, provider ChatProvider, bundle ChatCredentialBundle, writeID ID, expected uint64) (ChatCredentialResult, error) {
	if chatID(id) != nil || chatID(writeID) != nil || expected == 0 || bundle.Validate(provider) != nil {
		return ChatCredentialResult{}, ErrInvalid
	}
	// now сохраняет одно время результата при разрешённых повторах полностью откатившейся записи.
	now := time.Now().UTC().Truncate(time.Microsecond)
	// result принимает полный ожидаемый snapshot для возможной сверки COMMIT.
	var result ChatCredentialResult
	// err передаёт итог Write; callback r изменяет только credential-область текущего аккаунта.
	err := s.uow.Write(ctx, func(r Repositories) error {
		// account содержит нетронутую config-область, readErr — отказ чтения.
		account, readErr := r.Chat.ChatAccount(ctx, id)
		if readErr != nil {
			return readErr
		}
		if account.Provider != provider {
			return ErrConflict
		}
		// credentials содержит набор перед обменом для проверки точного expected.
		credentials, readErr := r.Chat.ChatCredentials(ctx, id)
		if readErr != nil {
			return readErr
		}
		if credentials.CredentialRevision != expected {
			return ErrStale
		}
		credentials.CredentialRevision, readErr = NextRevision(expected)
		if readErr != nil {
			return readErr
		}
		credentials.Bundle, credentials.CredentialWriteID, credentials.CredentialUpdatedAt = bundle, writeID, now
		result = ChatCredentialResult{Account: account, Credentials: credentials}
		return r.Chat.RefreshChatCredentials(ctx, credentials, expected)
	})
	return result, err
}

// checkChatParents проверяет обе обязательные ссылки и совпадение provider внутри текущей Write.
// Отсутствие родителя сохраняет ErrNotFound; неизвестная ошибка чтения не подменяется отсутствием.
func checkChatParents(ctx context.Context, r Repositories, source ChatSource) error {
	// err сообщает об отказе чтения Destination до вставки либо замены Source.
	if _, err := r.Config.Destination(ctx, source.DestinationID); err != nil {
		return err
	}
	// account содержит конфигурацию родительского аккаунта, err — отказ её чтения.
	account, err := r.Chat.ChatAccount(ctx, source.AccountID)
	if err != nil {
		return err
	}
	if account.Provider != source.Provider {
		return ErrInvalid
	}
	return nil
}

// CreateChatSource проверяет конфигурацию и родителей, назначает новую Record и сохраняет Source.
func (s *Service) CreateChatSource(ctx context.Context, value ChatSource) (ChatSource, error) {
	// err отклоняет неверную конфигурацию до назначения идентичности.
	if err := value.Validate(); err != nil {
		return ChatSource{}, err
	}
	// record содержит новую идентичность источника, err — отказ её создания.
	record, err := newRecord()
	if err != nil {
		return ChatSource{}, err
	}
	value.Record = record
	// Callback r проверяет ссылки и вставляет Source в той же короткой Write.
	err = s.uow.Write(ctx, func(r Repositories) error {
		// parentErr передаёт отсутствие либо несогласованность родителя.
		if parentErr := checkChatParents(ctx, r, value); parentErr != nil {
			return parentErr
		}
		return r.Chat.InsertChatSource(ctx, value)
	})
	return value, err
}

// UpdateChatSource атомарно заменяет provider/account/channel/options при config CAS.
// DestinationID переносом не меняется: для другого назначения создаётся новый Source.
func (s *Service) UpdateChatSource(ctx context.Context, value ChatSource, expected uint64) (ChatSource, error) {
	if value.Validate() != nil || chatID(value.ID) != nil || expected == 0 {
		return ChatSource{}, ErrInvalid
	}
	// err сообщает итог Write; callback r проверяет config CAS, неизменного родителя и новые ссылки.
	err := s.uow.Write(ctx, func(r Repositories) error {
		// current содержит прежнюю конфигурацию, readErr — отказ чтения либо увеличения ревизии.
		current, readErr := r.Chat.ChatSource(ctx, value.ID)
		if readErr != nil {
			return readErr
		}
		if current.Revision != expected || current.DestinationID != value.DestinationID {
			return ErrConflict
		}
		if readErr = checkChatParents(ctx, r, value); readErr != nil {
			return readErr
		}
		value.Record, readErr = revise(current.Record)
		if readErr != nil {
			return readErr
		}
		return r.Chat.UpdateChatSource(ctx, value, expected)
	})
	return value, err
}

// DeleteChatSource удаляет одну конфигурацию по ожидаемой ревизии.
func (s *Service) DeleteChatSource(ctx context.Context, id ID, expected uint64) error {
	if chatID(id) != nil || expected == 0 {
		return ErrInvalid
	}
	// Callback r выполняет только условное удаление внутри Write.
	return s.uow.Write(ctx, func(r Repositories) error { return r.Chat.DeleteChatSource(ctx, id, expected) })
}

// GetChatSource читает конфигурацию без полномочий связанного аккаунта.
func (s *Service) GetChatSource(ctx context.Context, id ID) (ChatSource, error) {
	if chatID(id) != nil {
		return ChatSource{}, ErrInvalid
	}
	// value принимает Source, err — итог View; callback r использует конфигурационное чтение.
	var value ChatSource
	err := s.uow.View(ctx, func(r Repositories) error {
		// readErr сохраняет отказ чтения Source в View.
		var readErr error
		value, readErr = r.Chat.ChatSource(ctx, id)
		return readErr
	})
	return value, err
}

// ListChatSources читает ограниченную страницу через типизированные фильтры Sources.
func (s *Service) ListChatSources(ctx context.Context, query ChatSourceQuery) ([]ChatSource, error) {
	// err отклоняет неверный запрос до открытия View.
	if err := query.Validate(); err != nil {
		return nil, err
	}
	// values принимает страницу, err — итог View; callback r материализует не более query.Limit строк.
	var values []ChatSource
	err := s.uow.View(ctx, func(r Repositories) error {
		// readErr передаёт отказ чтения страницы.
		var readErr error
		values, readErr = r.Chat.ChatSources(ctx, query)
		return readErr
	})
	return values, err
}
