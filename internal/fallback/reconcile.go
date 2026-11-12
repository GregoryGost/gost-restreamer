package fallback

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// preparationScopeOwner связывает область процессов с точным допуском попытки подготовки.
type preparationScopeOwner struct {
	// Version задаёт версию JSON идентичности владельца области.
	Version int `json:"version"`
	// Kind отличает владельца подготовки от других владельцев областей.
	Kind string `json:"kind"`
	// Fence сохраняет точную идентичность поколения и попытки подготовки.
	Fence core.PreparationFence `json:"fence"`
}

// retiredPreparationScope сохраняет область и незавершённый билет для повторного подтверждения очистки
// после окончания Job.
type retiredPreparationScope struct {
	// scope содержит область процессов, закрытие которой ещё не подтверждено.
	scope *processowner.Scope
	// ticket при наличии требует завершения файловой операции до закрытия области.
	ticket *processowner.Ticket
	// generationID связывает отложенную область с поколением для исключения действующего задания.
	generationID core.ID
}

// Повторяет завершение операций и закрытие ограниченной порции отложенных областей без активного Job;
// неуспех сохраняет область и ErrCleanupPending.
func (m *Manager) retryRetiredScopes(ctx context.Context) error {
	m.mu.Lock()
	// scopes содержит ограниченную порцию отложенных областей без активного задания.
	scopes := make([]retiredPreparationScope, 0, min(len(m.retiredScopes), m.config.ReconcileLimit))
	// scope обозначает очередную отложенную область при выборе порции.
	for _, scope := range m.retiredScopes {
		if m.jobs[scope.generationID] != nil {
			continue
		}
		scopes = append(scopes, scope)
		if len(scopes) >= m.config.ReconcileLimit {
			break
		}
	}
	m.mu.Unlock()
	// result объединяет ошибки повторного завершения операций и закрытия областей.
	var result error
	// retired содержит очередную выбранную область с возможным незавершённым билетом.
	for _, retired := range scopes {
		if retired.ticket != nil {
			// err сообщает об отказе завершения отложенной операции.
			if err := retired.ticket.Complete(ctx); err != nil {
				result = errors.Join(result, err)
				continue
			}
			retired.ticket = nil
		}
		// err содержит результат повторного закрытия области, определяющий сохранение записи для
		// следующей сверки.
		err := retired.scope.CloseContext(ctx)
		m.mu.Lock()
		if err == nil {
			delete(m.retiredScopes, retired.scope.ID())
		} else {
			m.retiredScopes[retired.scope.ID()] = retired
		}
		m.mu.Unlock()
		result = errors.Join(result, err)
	}
	if result != nil {
		return errors.Join(ErrCleanupPending, result)
	}
	return nil
}

// Reconcile проверяет ограниченные страницы областей и FILE, включая освобождение ссылок после подтверждения в SQL.
// Неизвестное доказательство оставляет очистку неподтверждённой и не запрещает новый ручной запуск.
func (m *Manager) Reconcile(ctx context.Context) error {
	select {
	case <-m.reconcileGate:
		defer func() {
			// Возвращает маркер reconcileGate при любом выходе из сверки, освобождая доступ следующей сверке или
			// Close.
			m.reconcileGate <- struct{}{}
		}()
	case <-ctx.Done():
		return ctx.Err()
	}
	m.mu.Lock()
	// closed показывает, запрещена ли дальнейшая сверка после завершения менеджера.
	closed := m.reconciliationClosed
	m.mu.Unlock()
	if closed {
		return core.ErrClosed
	}
	// result накапливает ошибки отложенных областей и последующей сверки.
	result := m.retryRetiredScopes(ctx)
	if m.config.ProcessRegistry != nil {
		// scopes содержит страницу восстановленных областей процессов. next задаёт курсор
		// следующей страницы. err сообщает об отказе восстановления.
		scopes, next, err := m.config.ProcessRegistry.RecoverPage(ctx, m.scopeCursor, min(m.config.ReconcileLimit, m.config.ProcessRegistry.PageLimit()))
		if err != nil {
			result = errors.Join(result, err)
		} else {
			m.scopeCursor = next
			// scope содержит очередное доказательство области подготовки для сверки с
			// сохранённым поколением.
			for _, scope := range scopes {
				result = errors.Join(result, m.reconcilePreparation(ctx, scope))
			}
		}
	}
	// page содержит страницу намерений очистки файлов. err сообщает об отказе чтения страницы.
	page, err := m.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: m.config.ReconcileLimit}, Cursor: m.fileCursor})
	if err != nil {
		return errors.Join(result, err)
	}
	m.fileCursor = page.NextCursor
	// Каталоги обрабатываются после файлов текущей страницы; следующий проход повторяет недоступные.
	// directory выбирает сначала файлы, затем каталоги, чтобы дочерние объекты удалялись раньше
	// родителей.
	for _, directory := range []bool{false, true} {
		// intent содержит очередное намерение очистки текущей страницы.
		for _, intent := range page.Items {
			if intent.Kind != core.CleanupFile || (intent.FileKind == core.OwnedDirectory) != directory {
				continue
			}
			// err сообщает об отмене или истечении срока сверки перед очисткой файла.
			if err := ctx.Err(); err != nil {
				return errors.Join(result, err)
			}
			result = errors.Join(result, m.CleanupFile(ctx, intent))
		}
	}
	return result
}

// Подтверждает очистку только терминальной подготовки с точной идентичностью допуска и завершённой
// областью, затем освобождает запись области; активное локальное задание пропускается.
func (m *Manager) reconcilePreparation(ctx context.Context, proof processowner.ScopeResult) error {
	if proof.Acknowledged {
		return nil
	}
	// owner принимает идентичность подготовки из сохранённого владельца области.
	var owner preparationScopeOwner
	if json.Unmarshal([]byte(proof.Owner), &owner) != nil || owner.Kind != "preparation" {
		return nil
	}
	if owner.Version != 1 || owner.Fence.GenerationID == "" || owner.Fence.AssetID == "" || owner.Fence.Attempt == 0 || owner.Fence.Incarnation == "" || owner.Fence.AttemptToken == "" {
		return processowner.ErrUnknown
	}
	m.mu.Lock()
	// job обозначает активное локальное задание того же поколения, которое сверка должна пропустить.
	job := m.jobs[owner.Fence.GenerationID]
	m.mu.Unlock()
	if job != nil {
		return nil
	}
	if !proof.Joined {
		return processowner.ErrPending
	}
	// generation содержит сохранённое поколение для сопоставления с допуском области. err сообщает
	// об отказе чтения.
	generation, err := m.service.GetGeneration(ctx, owner.Fence.GenerationID)
	if err != nil {
		return err
	}
	if core.PreparationFenceFor(generation) != owner.Fence || !generation.State.Terminal() {
		return core.ErrStale
	}
	if err = m.service.AcknowledgePreparationCleanup(ctx, owner.Fence); err != nil {
		// Неоднозначная запись перечитывается по тому же fence без внешнего повтора.
		// current содержит повторно прочитанное поколение после неоднозначного подтверждения
		// очистки. checkErr сообщает об отказе этого чтения.
		current, checkErr := m.service.GetGeneration(ctx, owner.Fence.GenerationID)
		if checkErr != nil || core.PreparationFenceFor(current) != owner.Fence || current.Issued || current.CleanupPending {
			return errors.Join(err, checkErr)
		}
	}
	return m.config.ProcessRegistry.AcknowledgeScope(ctx, proof.ID)
}
