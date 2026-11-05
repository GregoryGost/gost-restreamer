package session

import (
	"context"
	"errors"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
)

func (m *Manager) startOutput(c core.CommandReceipt) {
	m.mu.Lock()
	parent := m.sessions[c.SessionID]
	m.mu.Unlock()
	if parent == nil {
		return
	}
	parent.mu.Lock()
	if parent.closing || !parent.running {
		parent.mu.Unlock()
		return
	}
	if old := parent.outputs[c.OutputID]; old != nil {
		select {
		case <-old.done:
			old.mu.Lock()
			joined := old.joined
			old.mu.Unlock()
			if !joined {
				parent.mu.Unlock()
				return
			}
		default:
			parent.mu.Unlock()
			return
		}
	}
	ctx, cancel := context.WithCancel(parent.ctx)
	node := &outputNode{cancel: cancel, done: make(chan struct{}), startReceipt: c}
	parent.outputs[c.OutputID] = node
	parent.mu.Unlock()
	defer func() {
		close(node.done)
		node.mu.Lock()
		joined := node.joined
		node.mu.Unlock()
		if joined {
			parent.mu.Lock()
			if parent.outputs[c.OutputID] == node {
				delete(parent.outputs, c.OutputID)
			}
			if parent.members[c.OutputID] == node.member {
				delete(parent.members, c.OutputID)
			}
			parent.mu.Unlock()
		}
	}()
	defer cancel()
	startup, stop := context.WithTimeout(ctx, m.config.StartupTimeout)
	f, err := m.issue(startup, c)
	node.fence = f
	var member *membership
	if err == nil {
		output, e := m.config.Core.GetOutput(startup, c.OutputID)
		err = e
		if err == nil {
			member, err = parent.acquire(startup, output)
			node.member = member
		}
	}
	if err == nil {
		select {
		case <-member.ready:
			member.mu.Lock()
			err = member.err
			member.mu.Unlock()
		case <-startup.Done():
			err = startup.Err()
		}
	}
	var prepared *publisher.Publisher
	var observer *core.OutputObserver
	succeeded := false
	if err == nil {
		owner, e := m.config.Core.BindExecutionOwner(startup, f)
		err = e
		if err == nil {
			member.mu.Lock()
			ref := member.reference
			member.mu.Unlock()
			if ref == nil {
				err = core.ErrNotReady
			} else if err = ref.Group().Ready(); err == nil {
				prepared, err = publisher.Prepare(startup, m.config.Core, owner, member.output.Snapshot, ref.Router(), m.config.Publisher)
			}
		}
	}
	node.mu.Lock()
	node.publisher = prepared
	node.mu.Unlock()
	if err == nil {
		err = m.complete(startup, f, true, nil)
		succeeded = err == nil
	}
	if err == nil {
		observer, err = m.config.Core.OpenOutputObserver(startup, f)
		node.mu.Lock()
		node.observer = observer
		node.mu.Unlock()
	}
	if err == nil {
		err = prepared.Activate(ctx, observer)
	}
	stop()
	if err != nil {
		node.mu.Lock()
		node.failure = err
		node.mu.Unlock()
		cleanup, finish := m.operationContext()
		if !succeeded {
			m.log(m.complete(cleanup, f, false, err), "fail_output_start")
		}
		m.log(node.resolveFailure(cleanup, m), "compensate_output")
		finish()
		m.log(err, "output_start")
	} else {
		m.commandFinished(c.ID)
		select {
		case <-ctx.Done():
		case <-prepared.Done():
			result := prepared.Result()
			if result.Err != nil {
				node.mu.Lock()
				node.failure = result.Err
				node.finalFailure = true
				node.mu.Unlock()
				cleanup, finish := m.operationContext()
				m.log(m.config.Core.FailOutput(cleanup, observer, result.Err.Error()), "fail_output")
				finish()
			}
		}
	}
	cancel()
	cleanup, finish := context.WithTimeout(context.Background(), m.config.ShutdownTimeout)
	defer finish()
	m.log(node.cleanup(cleanup, m), "output_cleanup")
	m.signal()
}

func (n *outputNode) cleanup(ctx context.Context, m *Manager) error {
	n.cleanupMu.Lock()
	defer n.cleanupMu.Unlock()
	n.cancel()
	n.mu.Lock()
	prepared, observer := n.publisher, n.observer
	n.mu.Unlock()
	var result error
	if err := n.resolveFailure(ctx, m); err != nil {
		return err
	}
	if prepared != nil {
		result = errors.Join(result, prepared.Stop(ctx))
		if !prepared.Result().NetworkJoined {
			result = errors.Join(result, publisher.ErrCleanupPending)
		}
	}
	if n.member != nil {
		result = errors.Join(result, n.member.close(ctx))
	}
	if result != nil {
		return result
	}
	// Завершение физических ресурсов и освобождение RAM предшествуют подтверждению очистки в БД.
	n.mu.Lock()
	released := n.released
	n.mu.Unlock()
	if prepared != nil && !released {
		if err := m.config.Core.ConfirmOutputExecutionResolution(ctx, prepared.Reservation()); err != nil {
			return err
		}
		if err := prepared.ReleaseJoined(); err != nil {
			return err
		}
		n.mu.Lock()
		n.released = true
		n.mu.Unlock()
	}
	if observer != nil {
		result = errors.Join(result, observer.Close(ctx))
	}
	n.mu.Lock()
	n.joined = result == nil
	n.mu.Unlock()
	return result
}

func (n *outputNode) resolveFailure(ctx context.Context, m *Manager) error {
	n.mu.Lock()
	failure, final, observer := n.failure, n.finalFailure, n.observer
	n.mu.Unlock()
	if failure == nil {
		return nil
	}
	if n.startReceipt.ID != "" && !final {
		current, err := m.config.Core.GetReceipt(ctx, n.startReceipt.ID)
		if err != nil {
			return err
		}
		if !sameCommand(current, n.startReceipt) || current.Status.Terminal() && current.Status != core.CommandSucceeded {
			return nil
		}
		if current.Status == core.CommandPending {
			// Внешнее исполнение ещё не начиналось; Claim нужен только для точного отказа.
			f, err := m.config.Core.Claim(ctx, current.RequestID)
			if err != nil {
				return err
			}
			n.fence = f
		} else {
			f := receiptFence(current)
			if n.fence.CommandID != "" && n.fence != f {
				return core.ErrStale
			}
			n.fence = f
		}
		if current.Status != core.CommandSucceeded {
			if err := m.complete(ctx, n.fence, false, failure); err == nil {
				return nil
			}
		}
	}
	if n.fence.OutputID == "" {
		return core.ErrNotReady
	}
	if final && observer != nil && observer.Valid() {
		err := m.config.Core.FailOutput(ctx, observer, failure.Error())
		if err == nil {
			return nil
		}
		output, readErr := m.config.Core.GetOutput(ctx, n.fence.OutputID)
		if readErr == nil && (!output.DesiredRunning || output.Generation != n.fence.OutputGeneration || output.Attempt != n.fence.Attempt) {
			return nil
		}
		return errors.Join(err, readErr)
	}
	_, _, err := m.config.Core.CompensateOutputStart(ctx, n.fence, "manager:resolve:"+string(n.fence.CommandID))
	return err
}

// waitOutputCleanup не принимает закрытый Done за доказательство успешной очистки.
func (n *sessionNode) waitOutputCleanup(ctx context.Context, id core.ID, generation uint64) error {
	n.mu.Lock()
	output, member := n.outputs[id], n.members[id]
	if output != nil && output.startReceipt.OutputGeneration != generation || member != nil && member.output.Generation != generation {
		n.mu.Unlock()
		return core.ErrStale
	}
	if n.excluded == nil {
		n.excluded = map[core.ID]uint64{}
	}
	if !n.running && !n.closing {
		n.excluded[id] = max(n.excluded[id], generation)
	}
	n.mu.Unlock()
	if output != nil {
		output.cancel()
		select {
		case <-output.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		output.mu.Lock()
		joined := output.joined
		output.mu.Unlock()
		if !joined {
			if err := output.cleanup(ctx, n.manager); err != nil {
				return err
			}
		}
	} else if member != nil {
		if err := member.close(ctx); err != nil {
			return err
		}
	}
	n.mu.Lock()
	if n.outputs[id] == output {
		delete(n.outputs, id)
	}
	if n.members[id] == member {
		delete(n.members, id)
	}
	n.mu.Unlock()
	return nil
}

func retryPause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
