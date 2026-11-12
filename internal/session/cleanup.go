package session

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// cleanupProbe сохраняет состояние постраничного поиска единственной доказанно завершённой области
// прежнего выполнения.
type cleanupProbe struct {
	// intent содержит точное долговечное намерение очистки, для которого ищется область процессов.
	intent core.CleanupIntent
	// cursor хранит продолжение каталога областей; scopeID сохраняет найденную совпавшую область.
	cursor, scopeID string
	// found отмечает хотя бы одно совпадение; unknown запрещает подтверждение при неоднозначности
	// либо недостоверном завершении.
	found, unknown bool
}

// reconcileCleanup обрабатывает страницу текущих намерений очистки исполнения и после неё продвигает
// постраничную проверку прежних областей процессов.
func (m *Manager) reconcileCleanup(ctx context.Context) {
	// page содержит страницу незавершённых намерений очистки; err сохраняет результат чтения и
	// локальной очистки.
	page, err := m.config.Core.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: m.config.PageSize, PendingOnly: true}, Cursor: m.cleanupCursor})
	if err != nil {
		m.log(err, "cleanup_list")
		return
	}
	m.cleanupCursor = page.NextCursor
	defer m.advanceRecovery(ctx)
	// intent содержит очередное намерение текущей страницы для проверки runtime-ресурсов.
	for _, intent := range page.Items {
		if ctx.Err() != nil {
			return
		}
		if intent.Kind != core.CleanupRuntime {
			continue
		}
		// cleanup ограничивает обработку отдельного намерения OperationTimeout; stop отменяет
		// контекст cleanup после обработки намерения.
		cleanup, stop := context.WithTimeout(ctx, m.config.OperationTimeout)
		err = m.cleanupRuntime(cleanup, intent)
		stop()
		if err != nil && !errors.Is(err, processowner.ErrPending) && !errors.Is(err, core.ErrNotReady) && !errors.Is(err, context.DeadlineExceeded) {
			m.log(err, "runtime_cleanup")
		}
	}
}

// cleanupRuntime сверяет точное намерение, ждёт физического завершения совпадающего локального
// исполнения и подтверждает очистку в core. Чужая либо отсутствующая локальная попытка передаётся
// фоновой проверке каталога процессов.
func (m *Manager) cleanupRuntime(ctx context.Context, intent core.CleanupIntent) error {
	// currentIntent содержит повторно прочитанное намерение; err сообщает об отказе проверки его
	// точной идентичности.
	currentIntent, err := m.config.Core.GetCleanup(ctx, intent.ID)
	if err != nil {
		return err
	}
	if core.CleanupFenceFor(currentIntent) != core.CleanupFenceFor(intent) {
		return core.ErrStale
	}
	if currentIntent.Acknowledged {
		return nil
	}
	m.mu.Lock()
	// node содержит локальную Session с тем же SessionID, если она ещё учитывается.
	node := m.sessions[intent.SessionID]
	m.mu.Unlock()
	// current показывает совпадение incarnation и lifecycle локального узла с намерением очистки.
	current := false
	if node != nil {
		node.mu.Lock()
		// f сохраняет точную попытку локальной Session для проверки принадлежности ресурсов.
		f := node.fence
		node.mu.Unlock()
		current = f.Incarnation == intent.Incarnation && f.Lifecycle == intent.Lifecycle
	}
	if current {
		if intent.OutputID != "" {
			// err сообщает об отказе ожидания и очистки конкретного выхода текущей Session.
			if err := node.waitOutputCleanup(ctx, intent.OutputID, intent.Generation); err != nil {
				return err
			}
		} else {
			node.stop()
			select {
			case <-node.done:
			case <-ctx.Done():
				return ctx.Err()
			}
			node.mu.Lock()
			// joined сохраняет результат прежней физической очистки Session после возврата
			// её работника.
			joined := node.joined
			node.mu.Unlock()
			if !joined {
				// err сообщает об отказе повторной очистки ресурсов текущей Session.
				if err := node.cleanupWithContext(ctx); err != nil {
					return err
				}
			}
		}
		// err содержит результат точного долговечного подтверждения очистки после завершения
		// ресурсов.
		if err := m.acknowledge(ctx, intent); err != nil {
			return err
		}
		if intent.OutputID == "" {
			if node.scope != nil {
				// err сообщает об отказе освобождения служебных доказательств области
				// после подтверждения Session в core.
				if err := m.config.Processes.AcknowledgeScope(ctx, node.scope.ID()); err != nil {
					return err
				}
			}
			m.mu.Lock()
			if m.sessions[intent.SessionID] == node {
				delete(m.sessions, intent.SessionID)
			}
			m.mu.Unlock()
		}
		return nil
	}
	// Один ограниченный проход старого каталога продвигается между итерациями диспетчера.

	return processowner.ErrPending
}

// acknowledge подтверждает точное намерение очистки и после ошибки чтением проверяет, не было ли
// подтверждение уже сохранено.
func (m *Manager) acknowledge(ctx context.Context, intent core.CleanupIntent) error {
	// f содержит точную идентичность разрешённого подтверждения CleanupIntent.
	f := core.CleanupFenceFor(intent)
	// err содержит исход записи подтверждения, который при ошибке перепроверяется чтением.
	err := m.config.Core.AcknowledgeCleanup(ctx, f)
	if err == nil {
		return nil
	}
	// current содержит повторно прочитанное намерение; e сообщает об отказе проверки уже
	// сохранённого подтверждения.
	current, e := m.config.Core.GetCleanup(ctx, intent.ID)
	if e == nil && core.CleanupFenceFor(current) == f && current.Acknowledged {
		return nil
	}
	return errors.Join(err, e)
}

// advanceRecovery продолжает один постраничный поиск области прежнего исполнения. Подтверждение
// очистки допускается лишь после полного прохода, единственной совпавшей области и доказанного
// завершения без UNKNOWN; область сессии освобождается после подтверждения в core.
func (m *Manager) advanceRecovery(ctx context.Context) {
	// probe содержит продолжаемый постраничный поиск либо создаваемую новую проверку прежнего
	// выполнения.
	probe := m.recovery
	if probe == nil {
		// page содержит очередное намерение для фонового восстановления; err сообщает об отказе
		// его чтения.
		page, err := m.config.Core.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 1}, Cursor: m.recoveryIntentCursor})
		if err != nil {
			m.log(err, "recovery_intent")
			return
		}
		m.recoveryIntentCursor = page.NextCursor
		if len(page.Items) == 0 || page.Items[0].Kind != core.CleanupRuntime || page.Items[0].Acknowledged && page.Items[0].OutputID != "" {
			return
		}
		// intent содержит выбранное runtime-намерение, включая уже подтверждённую очистку всей
		// Session.
		intent := page.Items[0]
		m.mu.Lock()
		// node содержит возможный локальный узел выбранной Session перед запуском восстановления
		// прежней области.
		node := m.sessions[intent.SessionID]
		m.mu.Unlock()
		if node != nil {
			node.mu.Lock()
			// f сохраняет локальную попытку для исключения ещё текущего неподтверждённого
			// исполнения из фонового поиска.
			f := node.fence
			node.mu.Unlock()
			if f.Incarnation == intent.Incarnation && f.Lifecycle == intent.Lifecycle && !intent.Acknowledged {
				return
			}
		}
		probe = &cleanupProbe{intent: intent}
		m.recovery = probe
	}
	// results содержит страницу доказательств областей; next задаёт продолжение; err сохраняет
	// результаты чтения и последующего подтверждения.
	results, next, err := m.config.Processes.RecoverPage(ctx, probe.cursor, min(m.config.PageSize, m.config.Processes.PageLimit()))
	if err != nil {
		m.log(err, "recover_scope_page")
		return
	}
	// result содержит очередное доказательство области из каталога процессов.
	for _, result := range results {
		// owner принимает версию, вид и точную идентичность сохранённого владельца области.
		var owner scopeOwner
		if json.Unmarshal([]byte(result.Owner), &owner) != nil || owner.Version != 1 {
			if result.Unknown {
				probe.unknown = true
			}
			continue
		}
		if owner.Kind != "session" {
			continue
		}
		// f содержит ExecutionFence владельца прочитанной области.
		f := owner.Fence
		// intent содержит исходное намерение текущего полного поиска для проверки совпадения
		// Session и lifecycle.
		intent := probe.intent
		if f.SessionID != intent.SessionID || f.Incarnation != intent.Incarnation || f.Lifecycle != intent.Lifecycle {
			continue
		}
		if probe.found && probe.scopeID != result.ID {
			probe.unknown = true
		}
		probe.found = true
		probe.scopeID = result.ID
		if !result.Joined || result.Unknown {
			probe.unknown = true
		}
	}
	probe.cursor = next
	if next != "" {
		return
	}
	m.recovery = nil
	if !probe.found || probe.unknown {
		return
	}
	if err = m.acknowledge(ctx, probe.intent); err != nil {
		m.log(err, "recovered_ack")
		return
	}
	if probe.intent.OutputID == "" {
		err = m.config.Processes.AcknowledgeScope(ctx, probe.scopeID)
		m.log(err, "scope_ack")
		if err == nil {
			m.mu.Lock()
			// node содержит оставшийся локальный узел после подтверждения восстановленной
			// области.
			node := m.sessions[probe.intent.SessionID]
			if node != nil {
				node.mu.Lock()
				// joined сохраняет признак уже завершённой очистки узла для допустимого
				// удаления его из Manager.
				joined := node.joined
				node.mu.Unlock()
				if joined {
					delete(m.sessions, probe.intent.SessionID)
				}
			}
			m.mu.Unlock()
		}
	}
}
