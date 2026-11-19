package storage

import (
	"context"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"gorm.io/gorm"
)

// chatAccountColumns исключает credential-колонки из обычных SQL-чтений.
const chatAccountColumns = "id,revision,created_at,updated_at,payload,provider"

// ChatAccount читает только config-колонки аккаунта через действующий репозиторий.
func (r *repositories) ChatAccount(ctx context.Context, id core.ID) (core.ChatAccount, error) {
	// err отклоняет некорректный ID до чтения строки.
	if err := validateChatID(id); err != nil {
		return core.ChatAccount{}, err
	}
	// db предоставляет читающую сессию, err — отказ доступа.
	db, err := r.access(ctx, false)
	if err != nil {
		return core.ChatAccount{}, err
	}
	// row принимает конфигурацию без полномочий.
	var row chatAccountRow
	if err = db.Table("chat_accounts").Select(chatAccountColumns).Where("id = ?", id).Take(&row).Error; err != nil {
		return core.ChatAccount{}, translate(err)
	}
	return decodeChatAccount(row)
}

// ChatCredentials явно читает отдельную серверную область аккаунта с полной проверкой payload.
func (r *repositories) ChatCredentials(ctx context.Context, id core.ID) (core.ChatCredentials, error) {
	// err проверяет идентификатор целевого аккаунта явного серверного чтения.
	if err := validateChatID(id); err != nil {
		return core.ChatCredentials{}, err
	}
	// db предоставляет разрешённую сессию, err — отказ доступа.
	db, err := r.access(ctx, false)
	if err != nil {
		return core.ChatCredentials{}, err
	}
	// row принимает только колонки полного набора и его квитанции.
	var row chatCredentialRow
	if err = db.Table("chat_accounts").Select("id,provider,credential_revision,credential_updated_at,credential_write_id,credential_payload").Where("id = ?", id).Take(&row).Error; err != nil {
		return core.ChatCredentials{}, translate(err)
	}
	return decodeChatCredentials(row)
}

// InsertChatAccount проверяет полный новый набор и атомарно вставляет обе области с ревизиями 1.
func (r *repositories) InsertChatAccount(ctx context.Context, value core.ChatCredentialResult) error {
	// db предоставляет записывающую сессию текущей транзакции, err — отказ доступа.
	db, err := r.access(ctx, true)
	if err != nil {
		return err
	}
	value.Account.Record, err = normalizeChatRecord(value.Account.Record)
	if err != nil {
		return err
	}
	if value.Account.Validate() != nil || value.Account.Revision != 1 || value.Credentials.CredentialRevision != 1 || value.Account.ID != value.Credentials.AccountID || value.Account.Provider != value.Credentials.Provider || !value.Account.CreatedAt.Equal(value.Credentials.CredentialUpdatedAt) {
		return core.ErrInvalid
	}
	// config содержит SQL-поля конфигурации без полномочий.
	config, err := chatRecordFields(value.Account.Record, value.Account)
	if err != nil {
		return err
	}
	config["provider"] = value.Account.Provider
	// credentials содержит независимо проверенные колонки полного начального набора.
	credentials, err := chatCredentialFields(value.Credentials)
	if err != nil {
		return err
	}
	// key и field переносят каждую credential-колонку в единственную вставляемую строку.
	for key, field := range credentials {
		config[key] = field
	}
	return translate(db.Table("chat_accounts").Model(&metadataRow{}).Create(config).Error)
}

// UpdateChatAccount обновляет только config payload/Record и сохраняет весь credential-набор.
func (r *repositories) UpdateChatAccount(ctx context.Context, value core.ChatAccount, expected uint64) error {
	// db предоставляет записывающую сессию, err — отказ доступа либо нормализации.
	db, err := r.access(ctx, true)
	if err != nil {
		return err
	}
	value.Record, err = normalizeChatRecord(value.Record)
	if err != nil {
		return err
	}
	if value.Validate() != nil || expected == 0 {
		return core.ErrInvalid
	}
	// current содержит config без декодирования полномочий.
	current, err := r.ChatAccount(ctx, value.ID)
	if err != nil {
		return err
	}
	// next задаёт требуемую следующую config revision с защитой переполнения.
	next, err := core.NextRevision(expected)
	if err != nil {
		return err
	}
	if current.Revision != expected || value.Revision != next || value.Provider != current.Provider || !value.CreatedAt.Equal(current.CreatedAt) {
		return core.ErrConflict
	}
	// fields содержит только config-колонки; ID и created_at не перезаписываются.
	fields, err := chatRecordFields(value.Record, value)
	if err != nil {
		return err
	}
	delete(fields, "id")
	delete(fields, "created_at")
	return chatUpdated(db.Table("chat_accounts").Where("id = ? AND revision = ?", value.ID, decimal(expected)).Updates(fields), core.ErrConflict)
}

// DeleteChatAccount выполняет условное удаление; FK независимо запрещает удаление используемого Account.
func (r *repositories) DeleteChatAccount(ctx context.Context, id core.ID, expected uint64) error {
	if expected == 0 || validateChatID(id) != nil {
		return core.ErrInvalid
	}
	return deleteValue(ctx, r, "chat_accounts", id, expected)
}

// ReplaceChatCredentials проверяет обе следующие ревизии и сохраняет полный проверенный результат.
// Config CAS согласует replacement с rename/delete, credential CAS защищает актуальный набор внутри Write.
func (r *repositories) ReplaceChatCredentials(ctx context.Context, value core.ChatCredentialResult, expected uint64) error {
	// db предоставляет записывающую сессию, err — отказ доступа либо нормализации.
	db, err := r.access(ctx, true)
	if err != nil {
		return err
	}
	value.Account.Record, err = normalizeChatRecord(value.Account.Record)
	if err != nil {
		return err
	}
	if expected == 0 || value.Account.Validate() != nil || value.Credentials.Validate() != nil || value.Account.ID != value.Credentials.AccountID || value.Account.Provider != value.Credentials.Provider {
		return core.ErrInvalid
	}
	// account содержит актуальную config-область, credentials — актуальный независимый набор.
	account, err := r.ChatAccount(ctx, value.Account.ID)
	if err != nil {
		return err
	}
	credentials, err := r.ChatCredentials(ctx, value.Account.ID)
	if err != nil {
		return err
	}
	// configNext и credentialNext вычисляются до UPDATE, чтобы любое переполнение откатило всю замену.
	configNext, err := core.NextRevision(expected)
	if err != nil {
		return err
	}
	credentialNext, err := core.NextRevision(credentials.CredentialRevision)
	if err != nil {
		return err
	}
	if account.Revision != expected || value.Account.Revision != configNext || value.Credentials.CredentialRevision != credentialNext || value.Account.Provider != account.Provider || value.Account.Name != account.Name || !value.Account.CreatedAt.Equal(account.CreatedAt) {
		return core.ErrConflict
	}
	// fields содержит новые config Record/payload без изменения идентичности и времени создания.
	fields, err := chatRecordFields(value.Account.Record, value.Account)
	if err != nil {
		return err
	}
	delete(fields, "id")
	delete(fields, "created_at")
	// replacement содержит полный новый набор с независимыми метаданными.
	replacement, err := chatCredentialFields(value.Credentials)
	if err != nil {
		return err
	}
	// key и field объединяют обе области в одном атомарном UPDATE.
	for key, field := range replacement {
		fields[key] = field
	}
	return chatUpdated(db.Table("chat_accounts").Where("id = ? AND revision = ? AND credential_revision = ?", account.ID, decimal(expected), decimal(credentials.CredentialRevision)).Updates(fields), core.ErrConflict)
}

// RefreshChatCredentials обновляет только credential-колонки, оставляя config Record неизменной.
func (r *repositories) RefreshChatCredentials(ctx context.Context, value core.ChatCredentials, expected uint64) error {
	// db предоставляет записывающую сессию, err — отказ доступа.
	db, err := r.access(ctx, true)
	if err != nil {
		return err
	}
	if expected == 0 || value.Validate() != nil {
		return core.ErrInvalid
	}
	// current содержит полный прежний набор, а next — следующую credential revision.
	current, err := r.ChatCredentials(ctx, value.AccountID)
	if err != nil {
		return err
	}
	if current.CredentialRevision != expected {
		return core.ErrStale
	}
	if !chatRefreshIdentity(current, value) {
		return core.ErrConflict
	}
	next, err := core.NextRevision(expected)
	if err != nil {
		return err
	}
	if value.CredentialRevision != next || value.Provider != current.Provider {
		return core.ErrConflict
	}
	// fields содержит только полный набор, его время, квитанцию и независимую ревизию.
	fields, err := chatCredentialFields(value)
	if err != nil {
		return err
	}
	return chatUpdated(db.Table("chat_accounts").Where("id = ? AND credential_revision = ?", value.AccountID, decimal(expected)).Updates(fields), core.ErrStale)
}

// chatRefreshIdentity запрещает менять получателя и приложение под видом обычного refresh.
// Оба набора уже структурно проверены; новая идентичность принимается только через replacement/config CAS.
func chatRefreshIdentity(current, replacement core.ChatCredentials) bool {
	if current.Provider != replacement.Provider {
		return false
	}
	switch current.Provider {
	case core.ChatYouTube:
		return current.Bundle.YouTube.APIKey == replacement.Bundle.YouTube.APIKey
	case core.ChatTwitch:
		return current.Bundle.Twitch.ClientID == replacement.Bundle.Twitch.ClientID && current.Bundle.Twitch.UserID == replacement.Bundle.Twitch.UserID
	case core.ChatVKVideoLive:
		return current.Bundle.VKVideoLive.ClientID == replacement.Bundle.VKVideoLive.ClientID && current.Bundle.VKVideoLive.ClientSecret == replacement.Bundle.VKVideoLive.ClientSecret
	default:
		return false
	}
}

// chatUpdated требует ровно одну изменённую строку и переводит ошибки реального SQLite.
func chatUpdated(result *gorm.DB, stale error) error {
	if result.Error != nil {
		return translate(result.Error)
	}
	if result.RowsAffected != 1 {
		return stale
	}
	return nil
}

// ChatSource читает полную конфигурацию одного источника с проверкой всех внешних колонок.
func (r *repositories) ChatSource(ctx context.Context, id core.ID) (core.ChatSource, error) {
	// err отклоняет некорректный ID до SQL-чтения источника.
	if err := validateChatID(id); err != nil {
		return core.ChatSource{}, err
	}
	// db предоставляет читающую сессию, err — отказ доступа.
	db, err := r.access(ctx, false)
	if err != nil {
		return core.ChatSource{}, err
	}
	// row принимает полную SQL-проекцию источника.
	var row chatSourceRow
	if err = db.Table("chat_sources").Where("id = ?", id).Take(&row).Error; err != nil {
		return core.ChatSource{}, translate(err)
	}
	return decodeChatSource(row)
}

// InsertChatSource сохраняет полную конфигурацию; обязательные родительские связи защищает SQLite.
func (r *repositories) InsertChatSource(ctx context.Context, value core.ChatSource) error {
	// db предоставляет записывающую сессию, err — отказ доступа либо нормализации.
	db, err := r.access(ctx, true)
	if err != nil {
		return err
	}
	value.Record, err = normalizeChatRecord(value.Record)
	if err != nil {
		return err
	}
	if value.Revision != 1 || value.Validate() != nil {
		return core.ErrInvalid
	}
	// fields содержит все индексируемые ссылки и полную нагрузку источника.
	fields, err := chatSourceFields(value)
	if err != nil {
		return err
	}
	return translate(db.Table("chat_sources").Model(&metadataRow{}).Create(fields).Error)
}

// chatSourceFields кодирует полную конфигурацию Source и её независимо проверяемые SQL-колонки.
func chatSourceFields(value core.ChatSource) (map[string]any, error) {
	// fields содержит общие конфигурационные колонки, err — отказ кодирования.
	fields, err := chatRecordFields(value.Record, value)
	if err != nil {
		return nil, err
	}
	fields["provider"], fields["destination_id"], fields["account_id"], fields["enabled"] = value.Provider, value.DestinationID, value.AccountID, value.Enabled
	return fields, nil
}

// UpdateChatSource сохраняет согласованные platform/account/channel/options одним config CAS.
func (r *repositories) UpdateChatSource(ctx context.Context, value core.ChatSource, expected uint64) error {
	// db предоставляет записывающую сессию, err — отказ доступа либо нормализации.
	db, err := r.access(ctx, true)
	if err != nil {
		return err
	}
	value.Record, err = normalizeChatRecord(value.Record)
	if err != nil {
		return err
	}
	if expected == 0 || value.Validate() != nil {
		return core.ErrInvalid
	}
	// current содержит прежнюю конфигурацию для проверки неизменного родителя.
	current, err := r.ChatSource(ctx, value.ID)
	if err != nil {
		return err
	}
	// next задаёт следующую config revision с явной защитой переполнения.
	next, err := core.NextRevision(expected)
	if err != nil {
		return err
	}
	if current.Revision != expected || value.Revision != next || value.DestinationID != current.DestinationID || !value.CreatedAt.Equal(current.CreatedAt) {
		return core.ErrConflict
	}
	// fields содержит атомарный набор новых колонок без ID и created_at.
	fields, err := chatSourceFields(value)
	if err != nil {
		return err
	}
	delete(fields, "id")
	delete(fields, "created_at")
	return chatUpdated(db.Table("chat_sources").Where("id = ? AND revision = ?", value.ID, decimal(expected)).Updates(fields), core.ErrConflict)
}

// DeleteChatSource удаляет Source при ожидаемой config revision.
func (r *repositories) DeleteChatSource(ctx context.Context, id core.ID, expected uint64) error {
	if expected == 0 || validateChatID(id) != nil {
		return core.ErrInvalid
	}
	return deleteValue(ctx, r, "chat_sources", id, expected)
}

// chatPage ограничивает внутренний SQL-запрос стабильной границей и возрастающим порядком.
// table задаётся только кодом репозитория, after уже проверена доменным типом запроса.
func chatPage(db *gorm.DB, table string, limit int, after *core.ListBoundary) *gorm.DB {
	if after != nil {
		db = db.Where("("+table+".created_at > ? OR ("+table+".created_at = ? AND "+table+".id > ?))", after.CreatedAt.UnixMicro(), after.CreatedAt.UnixMicro(), after.ID)
	}
	return db.Order(table + ".created_at ASC").Order(table + ".id ASC").Limit(limit)
}

// ChatAccounts читает ограниченную страницу config-колонок с необязательным provider-фильтром.
func (r *repositories) ChatAccounts(ctx context.Context, query core.ChatAccountQuery) ([]core.ChatAccount, error) {
	// err отклоняет неверную границу и размер до выполнения SQL.
	if err := query.Validate(); err != nil {
		return nil, err
	}
	// db предоставляет читающую сессию, err — отказ доступа.
	db, err := r.access(ctx, false)
	if err != nil {
		return nil, err
	}
	db = db.Table("chat_accounts").Select(chatAccountColumns)
	if query.Provider != "" {
		db = db.Where("provider = ?", query.Provider)
	}
	// rows принимает не более query.Limit конфигурационных SQL-строк без credential payload.
	var rows []chatAccountRow
	if err = chatPage(db, "chat_accounts", query.Limit, query.After).Find(&rows).Error; err != nil {
		return nil, translate(err)
	}
	// values содержит проверенные конфигурации в точном порядке SQL-страницы.
	values := make([]core.ChatAccount, 0, len(rows))
	// row содержит очередную строку для сверки нагрузки и колонок.
	for _, row := range rows {
		// value содержит прочитанную конфигурацию, decodeErr — нарушение её представления.
		value, decodeErr := decodeChatAccount(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		values = append(values, value)
	}
	return values, nil
}

// ChatSources читает ограниченную страницу с фильтрами Account/Destination/Provider и производным Stream.
func (r *repositories) ChatSources(ctx context.Context, query core.ChatSourceQuery) ([]core.ChatSource, error) {
	// err отклоняет некорректный типизированный запрос до SQL.
	if err := query.Validate(); err != nil {
		return nil, err
	}
	// db предоставляет читающую сессию, err — отказ доступа.
	db, err := r.access(ctx, false)
	if err != nil {
		return nil, err
	}
	db = db.Table("chat_sources").Select("chat_sources.*")
	if query.Provider != "" {
		db = db.Where("chat_sources.provider = ?", query.Provider)
	}
	if query.DestinationID != "" {
		db = db.Where("chat_sources.destination_id = ?", query.DestinationID)
	}
	if query.AccountID != "" {
		db = db.Where("chat_sources.account_id = ?", query.AccountID)
	}
	if query.StreamID != "" {
		db = db.Joins("JOIN destinations ON destinations.id = chat_sources.destination_id").Where("destinations.stream_id = ?", query.StreamID)
	}
	// rows принимает ограниченную страницу полного конфигурационного представления.
	var rows []chatSourceRow
	if err = chatPage(db, "chat_sources", query.Limit, query.After).Find(&rows).Error; err != nil {
		return nil, translate(err)
	}
	// values содержит проверенные Sources в стабильном порядке.
	values := make([]core.ChatSource, 0, len(rows))
	// row содержит очередную конфигурационную строку для полной сверки.
	for _, row := range rows {
		// value содержит Source, decodeErr сообщает о повреждении payload либо SQL-проекции.
		value, decodeErr := decodeChatSource(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		values = append(values, value)
	}
	return values, nil
}
