package fallback

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

type preparationScopeOwner struct {
	Version int                   `json:"version"`
	Kind    string                `json:"kind"`
	Fence   core.PreparationFence `json:"fence"`
}

type retiredPreparationScope struct {
	scope        *processowner.Scope
	ticket       *processowner.Ticket
	generationID core.ID
}

func (m *Manager) retryRetiredScopes(ctx context.Context) error {
	m.mu.Lock()
	scopes := make([]retiredPreparationScope, 0, min(len(m.retiredScopes), m.config.ReconcileLimit))
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
	var result error
	for _, retired := range scopes {
		if retired.ticket != nil {
			if err := retired.ticket.Complete(ctx); err != nil {
				result = errors.Join(result, err)
				continue
			}
			retired.ticket = nil
		}
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

// Reconcile проверяет ограниченные страницы областей и FILE, включая освобождение ссылок после SQL ack.
// Неизвестное доказательство сохраняет pending и не запрещает новый ручной запуск.
func (m *Manager) Reconcile(ctx context.Context) error {
	select {
	case <-m.reconcileGate:
		defer func() { m.reconcileGate <- struct{}{} }()
	case <-ctx.Done():
		return ctx.Err()
	}
	m.mu.Lock()
	closed := m.reconciliationClosed
	m.mu.Unlock()
	if closed {
		return core.ErrClosed
	}
	result := m.retryRetiredScopes(ctx)
	if m.config.ProcessRegistry != nil {
		scopes, next, err := m.config.ProcessRegistry.RecoverPage(ctx, m.scopeCursor, min(m.config.ReconcileLimit, m.config.ProcessRegistry.PageLimit()))
		if err != nil {
			result = errors.Join(result, err)
		} else {
			m.scopeCursor = next
			for _, scope := range scopes {
				result = errors.Join(result, m.reconcilePreparation(ctx, scope))
			}
		}
	}
	page, err := m.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: m.config.ReconcileLimit}, Cursor: m.fileCursor})
	if err != nil {
		return errors.Join(result, err)
	}
	m.fileCursor = page.NextCursor
	// Каталоги обрабатываются после файлов текущей страницы; следующий проход повторяет недоступные.
	for _, directory := range []bool{false, true} {
		for _, intent := range page.Items {
			if intent.Kind != core.CleanupFile || (intent.FileKind == core.OwnedDirectory) != directory {
				continue
			}
			if err := ctx.Err(); err != nil {
				return errors.Join(result, err)
			}
			result = errors.Join(result, m.CleanupFile(ctx, intent))
		}
	}
	return result
}

func (m *Manager) reconcilePreparation(ctx context.Context, proof processowner.ScopeResult) error {
	if proof.Acknowledged {
		return nil
	}
	var owner preparationScopeOwner
	if json.Unmarshal([]byte(proof.Owner), &owner) != nil || owner.Kind != "preparation" {
		return nil
	}
	if owner.Version != 1 || owner.Fence.GenerationID == "" || owner.Fence.AssetID == "" || owner.Fence.Attempt == 0 || owner.Fence.Incarnation == "" || owner.Fence.AttemptToken == "" {
		return processowner.ErrUnknown
	}
	m.mu.Lock()
	job := m.jobs[owner.Fence.GenerationID]
	m.mu.Unlock()
	if job != nil {
		return nil
	}
	if !proof.Joined {
		return processowner.ErrPending
	}
	generation, err := m.service.GetGeneration(ctx, owner.Fence.GenerationID)
	if err != nil {
		return err
	}
	if core.PreparationFenceFor(generation) != owner.Fence || !generation.State.Terminal() {
		return core.ErrStale
	}
	if err = m.service.AcknowledgePreparationCleanup(ctx, owner.Fence); err != nil {
		// Неоднозначная запись перечитывается по тому же fence без внешнего повтора.
		current, checkErr := m.service.GetGeneration(ctx, owner.Fence.GenerationID)
		if checkErr != nil || core.PreparationFenceFor(current) != owner.Fence || current.Issued || current.CleanupPending {
			return errors.Join(err, checkErr)
		}
	}
	return m.config.ProcessRegistry.AcknowledgeScope(ctx, proof.ID)
}
