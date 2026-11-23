package chat

import (
	"context"
	"crypto/rand"
	"errors"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// youtubeKeyValidator проверяет ключ YouTube отдельно от поиска эфира.
type youtubeKeyValidator interface {
	// ValidateAPIKey сообщает безопасную причину отказа либо ошибку выполнения.
	// Сохранение разрешает только пустая причина вместе с nil.
	ValidateAPIKey(ctx context.Context, key string) (core.ChatReason, error)
}

// ConnectYouTube проверяет ключ и создаёт аккаунт либо заменяет его полномочия по ожидаемой ревизии.
// Неудачная или отменённая проверка сохраняет прежнее чтение. Подтверждённая замена отзывает его
// до возврата. Результат содержит только публичные метаданные сохранённого аккаунта.
func (m *Manager) ConnectYouTube(ctx context.Context, request core.ChatAuthorizationRequest) (core.ChatAccount, core.ChatReason, error) {
	// account содержит допустимые метаданные нового аккаунта, в том числе имя.
	account := core.ChatAccount{Provider: request.Provider, Name: request.Name}
	if request.Provider != core.ChatYouTube || account.Validate() != nil || request.Bundle.Validate(core.ChatYouTube) != nil ||
		request.AccountID == "" && request.ExpectedRevision != 0 ||
		request.AccountID != "" && (request.AccountID.Validate() != nil || len(request.AccountID) > core.ChatIDBytes || request.ExpectedRevision == 0 || request.Name != "") {
		return core.ChatAccount{}, core.ChatReasonConfiguration, core.ErrInvalid
	}
	// validator проверяет ключ тем же адаптером, который будет читать чат после подключения.
	validator, ok := m.adapters[core.ChatYouTube].(youtubeKeyValidator)
	if !ok {
		return core.ChatAccount{}, core.ChatReasonConfiguration, core.ErrNotReady
	}
	// owner сериализует сохранение новых полномочий относительно других операций аккаунта.
	owner, _ := m.credentials.(*CredentialOwner)
	if request.AccountID != "" && owner == nil {
		return core.ChatAccount{}, core.ChatReasonConfiguration, core.ErrNotReady
	}
	// operation связывает все шаги с запросом и временем работы Manager. finish освобождает учёт,
	// err сообщает, удалось ли принять операцию до отмены или закрытия Manager.
	operation, finish, err := m.beginOperation(ctx)
	if err != nil {
		return core.ChatAccount{}, core.ChatReasonNone, err
	}
	defer finish()
	// bundle принадлежит этой операции и используется для проверки и записи одного и того же ключа.
	bundle := copyCredentialBundle(request.Bundle)
	// reason и err сообщают безопасный результат проверки до входа в операцию записи.
	reason, err := m.verifyYouTubeKey(operation, validator, bundle.YouTube.APIKey)
	if reason.Validate() != nil {
		return core.ChatAccount{}, core.ChatReasonConfiguration, core.ErrInvalid
	}
	if err != nil {
		return core.ChatAccount{}, reason, err
	}
	if reason == core.ChatReasonCredentials || reason == core.ChatReasonConfiguration {
		return core.ChatAccount{}, reason, core.ErrInvalid
	}
	if reason != core.ChatReasonNone {
		return core.ChatAccount{}, reason, core.ErrNotReady
	}
	if err = operation.Err(); err != nil {
		return core.ChatAccount{}, core.ChatReasonNone, err
	}

	// writeID связывает ожидаемую запись с её полной квитанцией после неоднозначного COMMIT.
	writeID := core.ID(rand.Text())
	if request.AccountID != "" {
		_, account, err = owner.replace(operation, m, request.AccountID, core.ChatYouTube, bundle, writeID, request.ExpectedRevision)
	} else {
		account, err = m.createYouTubeAccount(operation, account, bundle, writeID)
	}
	return account, core.ChatReasonNone, err
}

// verifyYouTubeKey ограничивает сетевой запрос проверки ключа тем же бюджетом, что и обнаружение.
// При заполнении бюджета операция сразу возвращает ErrBusy, поэтому проверка не удерживает очередь.
func (m *Manager) verifyYouTubeKey(ctx context.Context, validator youtubeKeyValidator, key string) (core.ChatReason, error) {
	select {
	case <-ctx.Done():
		return core.ChatReasonNone, ctx.Err()
	case m.discoverSlots <- struct{}{}:
	default:
		return core.ChatReasonLimit, core.ErrBusy
	}
	// Callback возвращает сетевой слот после завершения проверки, до возможной записи.
	defer func() { <-m.discoverSlots }()
	// bounded ограничивает только сетевую проверку. cancel освобождает её таймер.
	bounded, cancel := context.WithTimeout(ctx, m.limits.HTTPTimeout)
	defer cancel()
	if bounded.Err() != nil {
		return core.ChatReasonNone, bounded.Err()
	}
	return validator.ValidateAPIKey(bounded, key)
}

// createYouTubeAccount создаёт аккаунт с проверенным ключом. Если исход записи неясен,
// отдельное ограниченное чтение сравнивает обе ревизии, квитанцию и полный набор полномочий.
func (m *Manager) createYouTubeAccount(ctx context.Context, account core.ChatAccount, bundle core.ChatCredentialBundle, writeID core.ID) (core.ChatAccount, error) {
	// expected хранит уже назначенный ID и ожидаемый результат. writeErr может сопровождать COMMIT.
	expected, writeErr := m.service.CreateChatAccount(ctx, account, bundle, writeID)
	if writeErr == nil {
		return expected.Account, nil
	}
	if expected.Validate() != nil {
		return core.ChatAccount{}, writeErr
	}
	// check доживает до сверки после отмены запроса, но отменяется при остановке Manager.
	// cancel освобождает таймер сверки.
	check, cancel := context.WithTimeout(m.ctx, m.limits.HTTPTimeout)
	defer cancel()
	// actual содержит сохранённый результат, readErr сообщает, удалось ли подтвердить состояние.
	actual, readErr := m.service.GetChatCredentialResult(check, expected.Account.ID)
	if errors.Is(readErr, core.ErrNotFound) {
		return core.ChatAccount{}, writeErr
	}
	if readErr != nil {
		return core.ChatAccount{}, errors.Join(ErrCredentialCommitUnknown, readErr)
	}
	if !actual.Matches(expected) {
		return core.ChatAccount{}, core.ErrStale
	}
	return actual.Account, nil
}
