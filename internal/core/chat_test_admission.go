package core

import (
	"context"
	"errors"
	"time"
)

// ChatTestAdmission разрешает публикации одной тестовой ленты до перехода потока в RUNNING.
// RuntimeAuthority хранит один действующий допуск на поток и отзывает его при запуске эфира,
// удалении потока или закрытии хранилища.
type ChatTestAdmission struct {
	// authority упорядочивает публикации тестовой ленты и записи состояния её потока.
	authority *RuntimeAuthority
	// streamID связывает допуск с конфигурацией потока.
	streamID ID
	// done закрывается под общей блокировкой при отзыве точного допуска.
	done chan struct{}
}

// AdmitChatTest под общей блокировкой читает конфигурацию streamID и его активную сессию.
// Существующий RUNNING даёт ErrConflict, а другие состояния позволяют получить один текущий
// допуск на поток. Отменённое или неуспешное чтение не выдаёт допуск.
func (s *Service) AdmitChatTest(ctx context.Context, streamID ID) (*ChatTestAdmission, error) {
	// err сообщает о недопустимом идентификаторе до чтения конфигурации.
	if err := streamID.Validate(); err != nil {
		return nil, err
	}
	// a содержит реестр текущего открытия хранилища, а err сообщает об ошибке получения реестра.
	a, err := s.runtimeAuthority()
	if err != nil {
		return nil, err
	}
	if err = a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	// running показывает, что согласованное чтение нашло активную сессию в RUNNING.
	var running bool
	err = s.uow.View(ctx, func(r Repositories) error {
		// Проверяет существование потока и различает отсутствие активной сессии и ошибку чтения.
		// readErr сообщает об ошибке чтения существующего потока.
		if _, readErr := r.Config.Stream(ctx, streamID); readErr != nil {
			return readErr
		}
		// session содержит активную сессию потока, а readErr сообщает результат её поиска.
		session, readErr := r.Runtime.ActiveSession(ctx, streamID)
		if errors.Is(readErr, ErrNotFound) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		running = session.State == SessionRunning
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	// admission содержит текущий допуск, который можно вернуть повторному запросу.
	admission := a.chatTestAdmissions[streamID]
	if running {
		if admission != nil {
			a.revokeChatTestLocked(admission)
			a.notifyChatLocked()
		}
		return nil, ErrConflict
	}
	if admission == nil {
		admission = &ChatTestAdmission{authority: a, streamID: streamID, done: make(chan struct{})}
		a.chatTestAdmissions[streamID] = admission
	}
	return admission, nil
}

// Publish под общей блокировкой выполняет commit, только пока p остаётся текущим допуском
// своего потока. Отозванный допуск возвращает ErrConflict. Функция commit синхронно меняет
// подготовленное состояние в памяти и не выполняет ввод-вывод или операции RuntimeAuthority.
func (p *ChatTestAdmission) Publish(ctx context.Context, commit func()) error {
	if p == nil || p.authority == nil || commit == nil {
		return ErrInvalid
	}
	// a сериализует публикацию с возможным COMMIT запуска эфира.
	a := p.authority
	// err сообщает об отмене ожидания или закрытии реестра.
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if a.chatTestAdmissions[p.streamID] != p {
		return ErrConflict
	}
	commit()
	return nil
}

// Done возвращает канал, по закрытию которого владелец ленты прекращает публикации.
func (p *ChatTestAdmission) Done() <-chan struct{} { return p.done }

// Close синхронно отзывает только этот допуск и будит его владельца через Done.
// Повторный вызов безопасен, в том числе после регистрации нового допуска для того же потока.
func (p *ChatTestAdmission) Close() {
	if p == nil || p.authority == nil {
		return
	}
	// a синхронизирует отзыв с публикациями и заменой допуска.
	a := p.authority
	<-a.gate
	defer a.release()
	a.revokeChatTestLocked(p)
}

// revokeChatTestLocked удаляет точный допуск под общей блокировкой и закрывает его Done.
// Подтверждённые изменения Session и Stream отдельно будят сверку топологии чатов.
func (a *RuntimeAuthority) revokeChatTestLocked(p *ChatTestAdmission) {
	if a.chatTestAdmissions[p.streamID] != p {
		return
	}
	delete(a.chatTestAdmissions, p.streamID)
	close(p.done)
}

// chatTestWriteKey передаёт из Service в GuardWrite сведения только о записи, которая может
// прекратить тестовую ленту. UnitOfWork сохраняет значение при создании контекста записи.
type chatTestWriteKey struct{}

// chatTestWriteTarget описывает изменение одного потока, подготовленное к COMMIT.
// Транзакционная функция заполняет его под общей блокировкой, GuardWrite читает после записи.
type chatTestWriteTarget struct {
	// streamID выбирает точный поток. Пустое значение означает, что запись не отзывает допуск.
	streamID ID
	// fence отличает точную квитанцию запуска. Нулевое значение соответствует удалению потока.
	fence ExecutionFence
	// receiptRevision хранит ревизию квитанции ISSUED до перехода в SUCCEEDED для проверки отката.
	receiptRevision uint64
}

// reconcileChatTestWriteLocked отзывает допуск после подтверждённого COMMIT, а при ошибке
// сохраняет его только после подтверждения отката. Читает лишь затронутый поток или его точную
// квитанцию, пока GuardWrite удерживает общую блокировку. Сверка не меняет результат записи.
func (a *RuntimeAuthority) reconcileChatTestWriteLocked(ctx context.Context, uow UnitOfWork, timeout time.Duration, committed bool) {
	// target содержит сведения из последней попытки записи, ok подтверждает их наличие в контексте.
	target, ok := ctx.Value(chatTestWriteKey{}).(*chatTestWriteTarget)
	if !ok || target.streamID == "" {
		return
	}
	// admission выбирает допуск, которого касается эта запись.
	admission := a.chatTestAdmissions[target.streamID]
	if admission == nil {
		return
	}
	if !committed && target.rolledBack(ctx, uow, timeout) {
		return
	}
	a.revokeChatTestLocked(admission)
	a.notifyChatLocked()
}

// rolledBack подтверждает, что после неоднозначного исхода записи прежний поток или точная
// квитанция остались в исходном состоянии. Конечный срок чтения не зависит от отмены записи.
func (t *chatTestWriteTarget) rolledBack(ctx context.Context, uow UnitOfWork, timeout time.Duration) bool {
	// verification ограничивает независимое чтение результата, а cancel освобождает его таймер.
	verification, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	// unchanged показывает, что чтение обнаружило прежнее состояние. При ошибке View этот вывод
	// использовать нельзя.
	var unchanged bool
	// err сообщает об ошибке согласованного чтения точного результата записи.
	err := uow.View(verification, func(r Repositories) error {
		// Для удаления проверяет наличие потока. Для запуска сверяет прежнюю ревизию квитанции,
		// поскольку последующее состояние сессии само по себе не подтверждает откат запуска.
		if t.fence.CommandID == "" {
			// readErr сообщает, сохранилась ли целевая конфигурация после попытки удаления.
			_, readErr := r.Config.Stream(verification, t.streamID)
			unchanged = readErr == nil
			return readErr
		}
		// receipt содержит квитанцию точной команды, а readErr сообщает результат её чтения.
		receipt, readErr := r.Runtime.ReceiptByID(verification, t.fence.CommandID)
		if readErr != nil {
			return readErr
		}
		unchanged = receipt.Kind == CommandStartSession && executionFence(receipt) == t.fence &&
			receipt.Revision == t.receiptRevision && receipt.Status == CommandIssued
		return nil
	})
	return err == nil && verification.Err() == nil && unchanged
}
