package session

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

type cleanupProbe struct {
	intent          core.CleanupIntent
	cursor, scopeID string
	found, unknown  bool
}

func (m *Manager) reconcileCleanup(ctx context.Context) {
	page, err := m.config.Core.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: m.config.PageSize, PendingOnly: true}, Cursor: m.cleanupCursor})
	if err != nil {
		m.log(err, "cleanup_list")
		return
	}
	m.cleanupCursor = page.NextCursor
	defer m.advanceRecovery(ctx)
	for _, intent := range page.Items {
		if ctx.Err() != nil {
			return
		}
		if intent.Kind != core.CleanupRuntime {
			continue
		}
		cleanup, stop := context.WithTimeout(ctx, m.config.OperationTimeout)
		err = m.cleanupRuntime(cleanup, intent)
		stop()
		if err != nil && !errors.Is(err, processowner.ErrPending) && !errors.Is(err, core.ErrNotReady) && !errors.Is(err, context.DeadlineExceeded) {
			m.log(err, "runtime_cleanup")
		}
	}
}
func (m *Manager) cleanupRuntime(ctx context.Context, intent core.CleanupIntent) error {
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
	node := m.sessions[intent.SessionID]
	m.mu.Unlock()
	current := false
	if node != nil {
		node.mu.Lock()
		f := node.fence
		node.mu.Unlock()
		current = f.Incarnation == intent.Incarnation && f.Lifecycle == intent.Lifecycle
	}
	if current {
		if intent.OutputID != "" {
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
			joined := node.joined
			node.mu.Unlock()
			if !joined {
				if err := node.cleanupWithContext(ctx); err != nil {
					return err
				}
			}
		}
		if err := m.acknowledge(ctx, intent); err != nil {
			return err
		}
		if intent.OutputID == "" {
			if node.scope != nil {
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

func (m *Manager) acknowledge(ctx context.Context, intent core.CleanupIntent) error {
	f := core.CleanupFenceFor(intent)
	err := m.config.Core.AcknowledgeCleanup(ctx, f)
	if err == nil {
		return nil
	}
	current, e := m.config.Core.GetCleanup(ctx, intent.ID)
	if e == nil && core.CleanupFenceFor(current) == f && current.Acknowledged {
		return nil
	}
	return errors.Join(err, e)
}

func (m *Manager) advanceRecovery(ctx context.Context) {
	probe := m.recovery
	if probe == nil {
		page, err := m.config.Core.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 1}, Cursor: m.recoveryIntentCursor})
		if err != nil {
			m.log(err, "recovery_intent")
			return
		}
		m.recoveryIntentCursor = page.NextCursor
		if len(page.Items) == 0 || page.Items[0].Kind != core.CleanupRuntime || page.Items[0].Acknowledged && page.Items[0].OutputID != "" {
			return
		}
		intent := page.Items[0]
		m.mu.Lock()
		node := m.sessions[intent.SessionID]
		m.mu.Unlock()
		if node != nil {
			node.mu.Lock()
			f := node.fence
			node.mu.Unlock()
			if f.Incarnation == intent.Incarnation && f.Lifecycle == intent.Lifecycle && !intent.Acknowledged {
				return
			}
		}
		probe = &cleanupProbe{intent: intent}
		m.recovery = probe
	}
	results, next, err := m.config.Processes.RecoverPage(ctx, probe.cursor, min(m.config.PageSize, m.config.Processes.PageLimit()))
	if err != nil {
		m.log(err, "recover_scope_page")
		return
	}
	for _, result := range results {
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
		f := owner.Fence
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
			node := m.sessions[probe.intent.SessionID]
			if node != nil {
				node.mu.Lock()
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
