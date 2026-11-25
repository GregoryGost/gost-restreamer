package chat

import (
	"context"
	"errors"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// beginOperation связывает выполняемую операцию с остановкой Manager и учитывает её до выхода.
func (m *Manager) beginOperation(ctx context.Context) (context.Context, func(), error) {
	if ctx == nil {
		return nil, nil, core.ErrInvalid
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return nil, nil, core.ErrClosed
	}
	m.work.Add(1)
	// operation и cancel связывают отмену внешнего запроса с временем жизни Manager.
	operation, cancel := context.WithCancel(ctx)
	// stop снимает зарегистрированное уведомление, когда работа завершилась сама.
	stop := context.AfterFunc(m.ctx, cancel)
	// Callback очищает обе стороны отмены и подтверждает фактическое окончание операции.
	return operation, func() { stop(); cancel(); m.work.Done() }, nil
}

// beginMutation закрывает допуск только изменяемого Source или Account на время CAS и сверки.
// Возвращённая функция открывает допуск сохранившимся поколениям и запускает повторную сверку.
func (m *Manager) beginMutation(ctx context.Context, sourceID, accountID core.ID) (context.Context, func(), error) {
	if ctx == nil {
		return nil, nil, core.ErrInvalid
	}
	select {
	case m.mutation <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-m.ctx.Done():
		return nil, nil, core.ErrClosed
	}
	// operation является связанным контекстом, done возвращает слот работы, err сообщает о закрытии.
	operation, done, err := m.beginOperation(ctx)
	if err != nil {
		<-m.mutation
		return nil, nil, err
	}
	m.mu.Lock()
	m.blockedSource, m.blockedAccount = sourceID, accountID
	m.changes++
	m.refreshMutationReadersLocked()
	m.mu.Unlock()
	// Callback открывает оставшиеся поколения только после принятия результата этого CAS.
	return operation, func() {
		m.mu.Lock()
		m.blockedSource, m.blockedAccount = "", ""
		m.changes++
		m.restoreMutationStatusesLocked(sourceID, accountID)
		m.refreshMutationReadersLocked()
		m.scheduleSourcesLocked()
		m.signalChangedLocked()
		m.mu.Unlock()
		<-m.mutation
		m.requestReconcile()
		done()
	}, nil
}

// restoreMutationStatusesLocked передаёт сохранившимся участникам состояние общего читателя после снятия барьера.
func (m *Manager) restoreMutationStatusesLocked(sourceID, accountID core.ID) {
	// reader хранит последнее состояние, в том числе после завершения физической подписки.
	for _, reader := range m.readers {
		// member мог пропустить обновление, если другой участник оставался доступен во время CAS.
		for member := range reader.members {
			if member.value.ID != sourceID && member.value.AccountID != accountID {
				continue
			}
			if m.sourceAttachedLocked(member) && m.checkOutputProofsLocked(m.ctx, member.session.owner, member.outputs) {
				m.setSourceStatusLocked(member, reader.state, reader.reason)
			}
		}
	}
}

// refreshMutationReadersLocked обновляет неизменяемые снимки при открытии и закрытии допуска.
func (m *Manager) refreshMutationReadersLocked() {
	// reader может разделяться затронутым участником и источником вне текущего изменения.
	for _, reader := range m.readers {
		m.refreshIngressLocked(reader)
	}
}

// ApplySource подтверждает точную сохранённую ревизию и синхронно отзывает затронутые поколения.
// До CAS существующего Source вызывающая сторона использует MutateSource.
func (m *Manager) ApplySource(ctx context.Context, id core.ID, revision uint64) (core.ChatApplyResult, error) {
	if !validDiscoveryID(string(id)) || revision == 0 {
		return core.ChatApplyResult{}, core.ErrInvalid
	}
	// operation задаёт срок чтения, finish освобождает конфигурационный допуск, err сообщает о закрытии.
	operation, finish, err := m.beginMutation(ctx, id, "")
	if err != nil {
		return core.ChatApplyResult{}, err
	}
	defer finish()
	// source содержит действующую конфигурацию, err сообщает об отказе её чтения.
	source, err := m.service.GetChatSource(operation, id)
	if err != nil {
		m.applySource(id, nil)
		if errors.Is(err, core.ErrNotFound) {
			return core.ChatApplyResult{}, core.ErrStale
		}
		return core.ChatApplyResult{}, err
	}
	// replaced показывает, пришлось ли отозвать работавшую привязку под текущей конфигурацией.
	replaced := m.applySource(id, &source)
	if source.Revision != revision {
		return core.ChatApplyResult{}, core.ErrStale
	}
	return core.ChatApplyResult{Revision: revision, Reconnected: replaced}, nil
}

// MutateSource закрывает допуск поколения перед переданной CAS-операцией и проверяет её результат.
// change выполняет только согласованное изменение этого Source через Service. Установленный
// откат сохраняет поколение. Неизвестный исход отзывает его до следующей согласованной сверки.
func (m *Manager) MutateSource(ctx context.Context, id core.ID, change func(context.Context) (core.ChatSource, error)) (core.ChatApplyResult, error) {
	if !validDiscoveryID(string(id)) || change == nil {
		return core.ChatApplyResult{}, core.ErrInvalid
	}
	// operation связывает срок SQL с запросом, finish возвращает допуск, err сообщает о закрытии.
	operation, finish, err := m.beginMutation(ctx, id, "")
	if err != nil {
		return core.ChatApplyResult{}, err
	}
	defer finish()
	// before содержит начальную ревизию, err сообщает об отказе чтения до CAS.
	before, err := m.service.GetChatSource(operation, id)
	if err != nil {
		return core.ChatApplyResult{}, err
	}
	// expected задаёт ожидаемый результат CAS, writeErr допускает неоднозначный COMMIT.
	expected, writeErr := change(operation)
	// check и cancel дают ограниченный срок на подтверждение даже после отмены исходного запроса.
	check, cancel := context.WithTimeout(m.ctx, m.limits.HTTPTimeout)
	defer cancel()
	// actual содержит прочитанную сохранённую конфигурацию, readErr — итог проверки COMMIT.
	actual, readErr := m.service.GetChatSource(check, id)
	if readErr != nil {
		m.applySource(id, nil)
		return core.ChatApplyResult{}, errors.Join(writeErr, readErr)
	}
	// replaced показывает отзыв старого поколения для обновлённой конфигурации.
	replaced := m.applySource(id, &actual)
	if expected.ID == id && expected.Revision > before.Revision && exactSourceConfig(expected, actual) {
		return core.ChatApplyResult{Revision: actual.Revision, Reconnected: replaced}, nil
	}
	if writeErr != nil {
		return core.ChatApplyResult{}, writeErr
	}
	return core.ChatApplyResult{}, core.ErrStale
}

// DeleteSource выполняет точный CAS DELETE с блокировкой допуска и подтверждением отсутствия.
func (m *Manager) DeleteSource(ctx context.Context, id core.ID, expected uint64) error {
	if !validDiscoveryID(string(id)) || expected == 0 {
		return core.ErrInvalid
	}
	// operation действует до отмены запроса, finish возвращает допуск, err сообщает о закрытии.
	operation, finish, err := m.beginMutation(ctx, id, "")
	if err != nil {
		return err
	}
	defer finish()
	// before подтверждает существование и требуемую ревизию источника до CAS DELETE.
	before, err := m.service.GetChatSource(operation, id)
	if err != nil {
		return err
	}
	if before.Revision != expected {
		return core.ErrConflict
	}
	// writeErr может сопровождать зафиксированное удаление и сверяется отдельным чтением.
	writeErr := m.service.DeleteChatSource(operation, id, expected)
	// check и cancel ограничивают чтение результата совершившегося CAS.
	check, cancel := context.WithTimeout(m.ctx, m.limits.HTTPTimeout)
	defer cancel()
	// actual содержит всё ещё существующий Source, readErr сообщает об отсутствии либо отказе чтения.
	actual, readErr := m.service.GetChatSource(check, id)
	if errors.Is(readErr, core.ErrNotFound) {
		m.applySource(id, nil)
		return nil
	}
	if readErr != nil {
		m.applySource(id, nil)
		return errors.Join(writeErr, readErr)
	}
	m.applySource(id, &actual)
	if writeErr != nil {
		return writeErr
	}
	return core.ErrStale
}

// ApplySourceDelete подтверждает сохранённое отсутствие и отзывает только соответствующий Source.
// До выполнения CAS DELETE вызывающая сторона использует DeleteSource.
func (m *Manager) ApplySourceDelete(ctx context.Context, id core.ID) error {
	if !validDiscoveryID(string(id)) {
		return core.ErrInvalid
	}
	// operation задаёт срок чтения, finish освобождает допуск, err сообщает о закрытии.
	operation, finish, err := m.beginMutation(ctx, id, "")
	if err != nil {
		return err
	}
	defer finish()
	// err возвращает подтверждение отсутствия либо причину, по которой его нет.
	if _, err := m.service.GetChatSource(operation, id); !errors.Is(err, core.ErrNotFound) {
		if err != nil {
			m.applySource(id, nil)
			return err
		}
		return core.ErrStale
	}
	m.applySource(id, nil)
	return nil
}

// exactSourceConfig сравнивает полный подтверждаемый результат CAS, включая обе временные метки.
func exactSourceConfig(a, b core.ChatSource) bool {
	return sameSourceReading(a, b) && a.Name == b.Name && a.Revision == b.Revision && a.CreatedAt.Equal(b.CreatedAt) && a.UpdatedAt.Equal(b.UpdatedAt)
}

// applySource сохраняет совместимых участников и отзывает старое чтение при существенном изменении.
func (m *Manager) applySource(id core.ID, source *core.ChatSource) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	// replaced отмечает отзыв хотя бы одного источника под прежней конфигурацией.
	replaced := false
	// session содержит только собственные действующие привязки этого источника.
	for _, session := range m.sessions {
		// member относится к данному Source, даже если его читатель разделяется с соседями.
		member := session.members[id]
		if member == nil {
			continue
		}
		if source != nil && sameSourceReading(member.value, *source) {
			member.value = readerSourceConfig(*source)
			continue
		}
		m.retireSourceLocked(member, false)
		replaced = true
	}
	return replaced
}

// ApplyAccount подтверждает уже сохранённую ревизию ручного переподключения и отзывает старые поколения.
// Для выполнения самой CAS-замены CredentialOwner использует mutateAccount.
func (m *Manager) ApplyAccount(ctx context.Context, id core.ID, revision uint64) (core.ChatApplyResult, error) {
	if !validDiscoveryID(string(id)) || revision == 0 {
		return core.ChatApplyResult{}, core.ErrInvalid
	}
	// operation действует до закрытия, finish освобождает слот, err сообщает о закрытии Manager.
	operation, finish, err := m.beginMutation(ctx, "", id)
	if err != nil {
		return core.ChatApplyResult{}, err
	}
	defer finish()
	// account содержит актуальные метаданные, err сообщает об отсутствии подтверждения.
	account, err := m.service.GetChatAccount(operation, id)
	if err != nil {
		m.revokeAccount(id)
		if errors.Is(err, core.ErrNotFound) {
			return core.ChatApplyResult{}, core.ErrStale
		}
		return core.ChatApplyResult{}, err
	}
	if account.Revision != revision {
		return core.ChatApplyResult{}, core.ErrStale
	}
	return core.ChatApplyResult{Revision: revision, Reconnected: m.revokeAccount(id)}, nil
}

// DeleteAccount удаляет аккаунт по конфигурационной ревизии и отзывает место
// владельца полномочий только после подтверждённого отсутствия аккаунта.
func (m *Manager) DeleteAccount(ctx context.Context, id core.ID, expected uint64) error {
	if !validDiscoveryID(string(id)) || expected == 0 {
		return core.ErrInvalid
	}
	// operation блокирует допуск источников аккаунта, finish снимает барьер, err сообщает об отказе.
	operation, finish, err := m.beginMutation(ctx, "", id)
	if err != nil {
		return err
	}
	defer finish()
	// writeErr отличает подтверждённый DELETE от отказа или неустановленного исхода записи.
	writeErr := m.service.DeleteChatAccount(operation, id, expected)
	if writeErr == nil {
		m.retireDeletedAccount(id)
		return nil
	}
	if errors.Is(writeErr, core.ErrConflict) || errors.Is(writeErr, core.ErrInUse) {
		return writeErr
	}
	// check сверяет конфигурацию независимо от отмены запроса, cancel освобождает таймер чтения.
	check, cancel := context.WithTimeout(m.ctx, m.limits.HTTPTimeout)
	defer cancel()
	// readErr подтверждает отсутствие только через согласованное конфигурационное чтение.
	_, readErr := m.service.GetChatAccount(check, id)
	if errors.Is(readErr, core.ErrNotFound) {
		m.retireDeletedAccount(id)
		if errors.Is(writeErr, core.ErrNotFound) {
			return writeErr
		}
		return nil
	}
	return errors.Join(writeErr, readErr)
}

// retireDeletedAccount передаёт CredentialOwner доказанное удаление и снимает оставшееся чтение.
// Метод не меняет сохранённые публичные проекции попыток авторизации.
func (m *Manager) retireDeletedAccount(id core.ID) {
	// owner указывает на владельца слотов, ok подтверждает, что Manager читает через него.
	if owner, ok := m.credentials.(*CredentialOwner); ok {
		owner.retire(id)
	}
	m.revokeAccount(id)
}

// mutateAccount закрывает допуск аккаунта на время ручной замены через CredentialOwner.
// change подтверждает полную квитанцию полномочий. При доказанном откате чтение использует прежнее
// поколение. При неизвестном исходе старые обратные вызовы отзываются до сверки владельца.
func (m *Manager) mutateAccount(ctx context.Context, id core.ID, change func(context.Context) (credentialReplacement, error)) (core.ChatApplyResult, error) {
	if !validDiscoveryID(string(id)) || change == nil {
		return core.ChatApplyResult{}, core.ErrInvalid
	}
	// operation связывает срок замены с запросом, finish открывает допуск, err сообщает о закрытии.
	operation, finish, err := m.beginMutation(ctx, "", id)
	if err != nil {
		return core.ChatApplyResult{}, err
	}
	defer finish()
	// before подтверждает исходную config revision без чтения токенов.
	before, err := m.service.GetChatAccount(operation, id)
	if err != nil {
		return core.ChatApplyResult{}, err
	}
	// replacement содержит квитанцию владельца, writeErr сообщает об отказе или неизвестном исходе.
	replacement, writeErr := change(operation)
	// account содержит метаданные квитанции, applied подтверждает переход или восстановление той же записи.
	account := replacement.account
	applied := account.Revision > before.Revision || replacement.recovered && account.Revision == before.Revision
	if writeErr == nil && account.ID == id && account.Provider == before.Provider && applied {
		return core.ChatApplyResult{Revision: account.Revision, Reconnected: m.revokeAccount(id)}, nil
	}
	// check и cancel ограничивают чтение исхода после отмены запроса.
	check, cancel := context.WithTimeout(m.ctx, m.limits.HTTPTimeout)
	defer cancel()
	// actual и readErr позволяют отличить доказанное прежнее состояние от неизвестного результата.
	actual, readErr := m.service.GetChatAccount(check, id)
	// unknown означает, что владелец не подтвердил полный результат, а unchanged подтверждает прежнюю ревизию.
	unknown := errors.Is(writeErr, ErrCredentialCommitUnknown)
	unchanged := readErr == nil && actual.Revision == before.Revision && actual.Provider == before.Provider
	// knownConflict охватывает определённый отказ CAS, включая гонку с переименованием без смены полномочий.
	knownConflict := errors.Is(writeErr, core.ErrConflict) && !errors.Is(writeErr, core.ErrStale)
	if unknown || !unchanged && !knownConflict {
		m.revokeAccount(id)
	}
	if writeErr != nil || readErr != nil {
		return core.ChatApplyResult{}, errors.Join(writeErr, readErr)
	}
	return core.ChatApplyResult{}, core.ErrStale
}

// revokeAccount отзывает только участников данного аккаунта и позже запускает их на новом поколении.
func (m *Manager) revokeAccount(id core.ID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	// replaced показывает, был ли остановлен хотя бы один активный участник.
	replaced := false
	// session удерживает независимые распространения одного аккаунта по активным эпохам.
	for _, session := range m.sessions {
		// member может разделять обработчик только с участниками того же аккаунта.
		for _, member := range session.members {
			if member.value.AccountID == id {
				m.retireSourceLocked(member, false)
				replaced = true
			}
		}
	}
	return replaced
}
