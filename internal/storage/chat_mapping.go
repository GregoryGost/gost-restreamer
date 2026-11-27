package storage

import (
	"errors"
	"strconv"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// validateChatID проверяет обязательный ID репозиторного запроса до передачи значения в SQL.
func validateChatID(id core.ID) error {
	if len(id) > core.ChatIDBytes {
		return core.ErrInvalid
	}
	return id.Validate()
}

// chatAccountRow отделяет конфигурационную нагрузку от provider; credential-колонки сюда не входят.
type chatAccountRow struct {
	// Metadata содержит общую конфигурационную запись и payload; экспортируемое поле с embedded
	// позволяет GORM прочитать вложенные колонки, сохраняя сам тип metadataRow внутренним.
	Metadata metadataRow `gorm:"embedded"`
	// Provider сверяется с конфигурационной нагрузкой аккаунта.
	Provider core.ChatProvider
}

// chatSourceRow содержит индексируемые ссылки и конфигурационную нагрузку источника.
type chatSourceRow struct {
	// Metadata содержит общие конфигурационные поля; экспортируемое поле с embedded
	// включает их в отображение GORM без отдельной связи или префикса колонок.
	Metadata metadataRow `gorm:"embedded"`
	// Provider сверяется с платформой в payload.
	Provider core.ChatProvider
	// DestinationID сверяется с неизменным родителем в payload.
	DestinationID core.ID
	// AccountID сверяется с выбранным аккаунтом в payload.
	AccountID core.ID
	// Enabled сохраняет числовое 0/1 и позволяет обнаружить некорректную SQL-проекцию.
	Enabled int
}

// chatCredentialRow читает только серверную область аккаунта и её независимые метаданные.
type chatCredentialRow struct {
	// ID задаёт AccountID полного набора.
	ID core.ID
	// Provider сверяется с provider-specific набором.
	Provider core.ChatProvider
	// CredentialRevision содержит точный двадцатизначный uint64.
	CredentialRevision string
	// CredentialUpdatedAt содержит UTC-время записи в микросекундах Unix.
	CredentialUpdatedAt int64
	// CredentialWriteID содержит квитанцию последней принятой credential-операции.
	CredentialWriteID core.ID
	// CredentialPayload содержит отдельную Version1-оболочку полного набора.
	CredentialPayload []byte
}

// normalizeChatRecord нормализует только времена Record, проверяет ID и положительную точную ревизию.
func normalizeChatRecord(record core.Record) (core.Record, error) {
	// err передаёт отказ проверки времени либо итоговой Record.
	var err error
	if record.CreatedAt, err = timestamp(record.CreatedAt); err != nil {
		return core.Record{}, err
	}
	if record.UpdatedAt, err = timestamp(record.UpdatedAt); err != nil {
		return core.Record{}, err
	}
	if len(record.ID) > core.ChatIDBytes {
		return core.Record{}, core.ErrInvalid
	}
	return record, record.Validate()
}

// chatRecordFields кодирует config-оболочку и общие колонки уже проверенного значения.
func chatRecordFields(record core.Record, value any) (map[string]any, error) {
	// payload содержит Version1-оболочку конфигурации, err — отказ кодирования.
	payload, err := core.EncodeMetadata(value)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": record.ID, "revision": decimal(record.Revision), "created_at": record.CreatedAt.UnixMicro(), "updated_at": record.UpdatedAt.UnixMicro(), "payload": payload}, nil
}

// chatRecordMatches проверяет config-колонки против payload с точным uint64 и UTC microseconds.
func chatRecordMatches(row metadataRow, record core.Record) bool {
	return record.Validate() == nil && len(record.ID) <= core.ChatIDBytes && record.CreatedAt.Location() == time.UTC && record.UpdatedAt.Location() == time.UTC && row.ID == record.ID && row.Revision == decimal(record.Revision) && row.CreatedAt == record.CreatedAt.UnixMicro() && row.UpdatedAt == record.UpdatedAt.UnixMicro()
}

// decodeChatAccount проверяет config payload и колонки, не читая и не декодируя полномочия.
func decodeChatAccount(row chatAccountRow) (core.ChatAccount, error) {
	// value принимает конфигурационную нагрузку аккаунта.
	var value core.ChatAccount
	// err сообщает о нарушении Version1 либо доменной конфигурации.
	if err := core.DecodeMetadata(row.Metadata.Payload, &value); err != nil {
		return core.ChatAccount{}, errors.Join(core.ErrCorrupt, err)
	}
	if !chatRecordMatches(row.Metadata, value.Record) || value.Provider != row.Provider {
		return core.ChatAccount{}, core.ErrCorrupt
	}
	return value, nil
}

// decodeChatSource проверяет все индексируемые поля против полной конфигурационной нагрузки.
func decodeChatSource(row chatSourceRow) (core.ChatSource, error) {
	// value принимает типизированную нагрузку источника.
	var value core.ChatSource
	// err передаёт отказ строгого декодирования либо валидации нагрузки.
	if err := core.DecodeMetadata(row.Metadata.Payload, &value); err != nil {
		return core.ChatSource{}, errors.Join(core.ErrCorrupt, err)
	}
	if !chatRecordMatches(row.Metadata, value.Record) || value.Provider != row.Provider || value.DestinationID != row.DestinationID || value.AccountID != row.AccountID || row.Enabled < 0 || row.Enabled > 1 || value.Enabled != (row.Enabled == 1) {
		return core.ChatSource{}, core.ErrCorrupt
	}
	return value, nil
}

// chatCredentialFields формирует только credential-колонки без конфигурационной Record.
func chatCredentialFields(value core.ChatCredentials) (map[string]any, error) {
	// err отклоняет неверный полный набор до сериализации.
	if err := value.Validate(); err != nil {
		return nil, err
	}
	// payload содержит отдельную Version1-оболочку реквизитов, err — отказ кодирования.
	payload, err := core.EncodeMetadata(value)
	if err != nil {
		return nil, err
	}
	return map[string]any{"credential_revision": decimal(value.CredentialRevision), "credential_updated_at": value.CredentialUpdatedAt.UnixMicro(), "credential_write_id": value.CredentialWriteID, "credential_payload": payload}, nil
}

// decodeChatCredentials сверяет каждую внешнюю credential-колонку с отдельной полной нагрузкой.
func decodeChatCredentials(row chatCredentialRow) (core.ChatCredentials, error) {
	// value принимает серверный полный набор и квитанцию записи.
	var value core.ChatCredentials
	// err сообщает о повреждённой оболочке либо структурно неверном наборе.
	if err := core.DecodeMetadata(row.CredentialPayload, &value); err != nil {
		return core.ChatCredentials{}, errors.Join(core.ErrCorrupt, err)
	}
	// revision проверяет числовое представление колонки до сравнения канонических двадцати цифр.
	revision, err := strconv.ParseUint(row.CredentialRevision, 10, 64)
	if err != nil || revision != value.CredentialRevision || row.CredentialRevision != decimal(revision) || row.ID != value.AccountID || row.Provider != value.Provider || row.CredentialWriteID != value.CredentialWriteID || row.CredentialUpdatedAt != value.CredentialUpdatedAt.UnixMicro() {
		return core.ChatCredentials{}, core.ErrCorrupt
	}
	return value, nil
}
