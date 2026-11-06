package session

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

type sessionNode struct {
	manager                  *Manager
	id, streamID             core.ID
	fence                    core.ExecutionFence
	ctx                      context.Context
	cancel                   context.CancelFunc
	done                     chan struct{}
	mu                       sync.Mutex
	closing, running, joined bool
	owner                    *core.RuntimeLease
	observer                 *core.SessionObserver
	scope                    *processowner.Scope
	router                   *media.Router
	registry                 *group.Registry
	sampler                  *monitoring.SessionSampler
	members                  map[core.ID]*membership
	outputs                  map[core.ID]*outputNode
	excluded                 map[core.ID]uint64
	cleanupMu                sync.Mutex
	setupTicket              *processowner.Ticket
	retirementNeeded         bool
	startReceipt             core.CommandReceipt
	retirementFailure        error
}
type membership struct {
	parent      *sessionNode
	output      core.SessionOutput
	ctx         context.Context
	cancel      context.CancelFunc
	done, ready chan struct{}
	mu          sync.Mutex
	stopped     bool
	reference   *group.Reference
	ticket      *processowner.Ticket
	err         error
	closeMu     sync.Mutex
}
type outputNode struct {
	fence        core.ExecutionFence
	startReceipt core.CommandReceipt
	cancel       context.CancelFunc
	done         chan struct{}
	member       *membership
	mu           sync.Mutex
	publisher    *publisher.Publisher
	observer     *core.OutputObserver
	joined       bool
	released     bool
	failure      error
	finalFailure bool
	cleanupMu    sync.Mutex
}

func (n *sessionNode) stop() {
	n.mu.Lock()
	n.closing = true
	n.running = false
	n.cancel()
	for _, member := range n.members {
		member.cancel()
	}
	for _, output := range n.outputs {
		output.cancel()
	}
	n.mu.Unlock()
}
func (n *sessionNode) stopOutput(id core.ID) {
	n.mu.Lock()
	member, output := n.members[id], n.outputs[id]
	n.mu.Unlock()
	if member != nil {
		member.stop()
	}
	if output != nil {
		output.cancel()
	}
}
func (m *Manager) commandFinished(id core.ID) {
	m.mu.Lock()
	delete(m.commands, id)
	m.mu.Unlock()
	m.signal()
}

func (m *Manager) startSession(c core.CommandReceipt) {
	m.mu.Lock()
	node := m.sessions[c.SessionID]
	m.mu.Unlock()
	if node == nil {
		return
	}
	defer close(node.done)
	startup, cancel := context.WithTimeout(node.ctx, m.config.StartupTimeout)
	defer cancel()
	f, err := m.issue(startup, c)
	node.mu.Lock()
	node.fence = f
	node.startReceipt = c
	node.mu.Unlock()
	if err == nil {
		err = node.initialize(startup)
	}
	if err != nil {
		node.mu.Lock()
		node.retirementNeeded = true
		node.retirementFailure = err
		node.mu.Unlock()
		cleanup, stop := m.operationContext()
		m.log(node.retireExecution(cleanup), "retire_startup")
		stop()
		m.log(err, "startup")
		node.stop()
		node.cleanup()
		return
	}
	if err = node.barrier(startup); err == nil {
		// Feed ждёт завершения COMMIT и сверки неоднозначного результата под тем же mutex.
		node.mu.Lock()
		if node.closing || node.ctx.Err() != nil {
			err = core.ErrStale
		} else {
			err = m.complete(startup, f, true, nil)
		}
		if err == nil && (node.ctx.Err() != nil || !node.owner.Valid()) {
			err = core.ErrStale
		}
		if err == nil {
			node.running = true
			node.excluded = map[core.ID]uint64{}
		}
		node.mu.Unlock()
	}
	if err != nil {
		node.mu.Lock()
		node.retirementNeeded = true
		node.retirementFailure = err
		node.mu.Unlock()
		cleanup, stop := m.operationContext()
		m.log(node.retireExecution(cleanup), "retire_startup")
		stop()
		m.log(err, "startup_barrier")
		node.stop()
		node.cleanup()
		return
	}
	m.commandFinished(c.ID)
	m.signal()
	ticker := time.NewTicker(m.config.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-node.ctx.Done():
			node.retireRevokedExecution()
			return
		case <-node.owner.Done():
			node.retireRevokedExecution()
			return
		case <-ticker.C:
		}
		ctx, stop := context.WithTimeout(node.ctx, m.config.OperationTimeout)
		session, e := m.config.Core.GetSession(ctx, node.id)
		if e == nil && (session.Lifecycle != f.Lifecycle || session.Incarnation != f.Incarnation || session.State != core.SessionRunning) {
			stop()
			node.stop()
			node.cleanup()
			return
		}
		if e == nil {
			e = node.registry.ApplyControl(session.Revision, session.DesiredPaused)
		}
		if e != nil && !errors.Is(e, context.Canceled) {
			m.log(e, "control_poll")
		}
		node.reconcileOutputs(ctx)
		m.log(node.scope.RetryCompleted(ctx), "retry_completed_operations")
		stop()
	}
}
func (n *sessionNode) initialize(ctx context.Context) error {
	m := n.manager
	session, err := m.config.Core.GetSession(ctx, n.id)
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.streamID = session.StreamID
	n.mu.Unlock()
	owner, err := m.config.Core.BindExecutionOwner(ctx, n.fence)
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.owner = owner
	n.mu.Unlock()
	observer, err := m.config.Core.OpenSessionObserver(ctx, n.fence)
	if err != nil {
		return err
	}
	n.observer = observer
	scope, err := m.config.Processes.Create(ctx, encodeOwner(n.fence))
	n.mu.Lock()
	n.scope = scope
	n.mu.Unlock()
	if err != nil {
		return err
	}
	ticket, err := scope.Register(ctx, "router-registry")
	n.setupTicket = ticket
	if err != nil {
		return err
	}
	router, err := media.NewRouter(m.config.Router)
	if err != nil {
		return errors.Join(err, ticket.Complete(ctx))
	}
	n.router = router
	sampler, err := monitoring.NewSessionSampler(n.ctx, m.config.Monitoring)
	if err != nil {
		return err
	}
	n.sampler = sampler
	if err = n.registerInputMetrics(); err != nil {
		return err
	}
	config := m.config.Group
	config.OpenFile = m.config.Files.OpenVerified
	config.Scope = scope
	config.Logger, config.Sampler, config.MetricStaleAfter = m.config.Logger.With("stream_id", n.streamID), sampler, m.config.Monitoring.StaleAfter
	registry, err := group.NewRegistry(ctx, group.RegistryConfig{Core: m.config.Core, Parent: n.fence, Observer: observer, Runtime: config, MaxGroups: m.config.MaxGroups, MaxReferences: m.config.MaxReferences})
	if err != nil {
		return errors.Join(err, ticket.Complete(ctx))
	}
	n.mu.Lock()
	n.registry = registry
	n.mu.Unlock()
	if err = ticket.Complete(ctx); err != nil {
		return err
	}
	n.setupTicket = nil
	request := core.PageRequest{Query: core.ListQuery{Limit: m.config.PageSize, SessionID: n.id}}
	for {
		page, e := m.config.Core.ListOutputs(ctx, request)
		if e != nil {
			return e
		}
		for _, output := range page.Items {
			if !output.Attached || !output.DesiredRunning || output.CleanupPending {
				continue
			}
			n.mu.Lock()
			count := len(n.members)
			n.mu.Unlock()
			if count >= m.config.MaxOutputs {
				return ErrLimit
			}
			if _, e = n.acquire(ctx, output); e != nil {
				required, checkErr := n.membershipRequired(ctx, output)
				if checkErr != nil {
					return errors.Join(e, checkErr)
				}
				if !required {
					continue
				}
				return e
			}
		}
		if page.NextCursor == "" {
			break
		}
		request.Cursor = page.NextCursor
	}
	return nil
}
func (n *sessionNode) acquire(ctx context.Context, output core.SessionOutput) (*membership, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closing || output.Generation <= n.excluded[output.ID] {
		return nil, core.ErrStale
	}
	if prior := n.members[output.ID]; prior != nil {
		if prior.output.Generation != output.Generation {
			return nil, core.ErrNotReady
		}
		return prior, nil
	}
	if len(n.members) >= n.manager.config.MaxOutputs {
		return nil, ErrLimit
	}
	ticket, err := n.scope.Register(ctx, "membership:"+string(output.ID))
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(n.ctx)
	member := &membership{parent: n, output: output, ctx: lifetime, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}), ticket: ticket}
	n.members[output.ID] = member
	go member.run()
	return member, nil
}
func (r *membership) run() {
	defer close(r.done)
	ref, err := r.parent.registry.Acquire(r.ctx, r.output.Snapshot, r.parent.router)
	r.mu.Lock()
	r.reference, r.err = ref, err
	stopped := r.stopped || r.ctx.Err() != nil
	r.mu.Unlock()
	if err == nil && !stopped {
		startup, cancel := context.WithTimeout(r.ctx, r.parent.manager.config.StartupTimeout)
		err = ref.Group().WaitReady(startup)
		cancel()
		r.mu.Lock()
		r.err = err
		r.mu.Unlock()
	}
	close(r.ready)
	if stopped || err != nil {
		r.stop()
	}
}
func (r *membership) stop() { r.mu.Lock(); r.stopped = true; r.cancel(); r.mu.Unlock() }
func (r *membership) close(ctx context.Context) error {
	r.closeMu.Lock()
	defer r.closeMu.Unlock()
	r.stop()
	select {
	case <-r.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	r.mu.Lock()
	ref := r.reference
	r.mu.Unlock()
	if ref != nil {
		if err := ref.CloseContext(ctx); err != nil {
			select {
			case <-ref.Group().Done():
				r.parent.manager.log(err, "joined_group_cleanup")
			default:
				return err
			}
		}
	}
	if r.ticket != nil {
		if err := r.ticket.Complete(ctx); err != nil {
			return err
		}
		r.ticket = nil
	}
	return nil
}

func (n *sessionNode) membershipRequired(ctx context.Context, output core.SessionOutput) (bool, error) {
	current, err := n.manager.config.Core.GetOutput(ctx, output.ID)
	if err != nil {
		return false, err
	}
	return current.Attached && current.DesiredRunning && !current.CleanupPending && current.Generation == output.Generation, nil
}

func (n *sessionNode) barrier(ctx context.Context) error {
	ticker := time.NewTicker(n.manager.config.ReconcileInterval)
	defer ticker.Stop()
	for {
		n.mu.Lock()
		members := make([]*membership, 0, len(n.members))
		for _, member := range n.members {
			members = append(members, member)
		}
		n.mu.Unlock()
		ready := true
		for _, member := range members {
			required, err := n.membershipRequired(ctx, member.output)
			if err != nil {
				return err
			}
			if !required {
				member.stop()
				continue
			}
			select {
			case <-member.ready:
				member.mu.Lock()
				err, ref := member.err, member.reference
				member.mu.Unlock()
				if err == nil && ref == nil {
					err = core.ErrNotReady
				}
				if err != nil {
					// Stop мог зафиксироваться после первого чтения и отменить ожидание готовности.
					required, checkErr := n.membershipRequired(ctx, member.output)
					if checkErr != nil {
						return errors.Join(err, checkErr)
					}
					if !required {
						member.stop()
						continue
					}
					return err
				}
				if err = ref.Group().Ready(); err != nil {
					ready = false
				}
			default:
				ready = false
			}
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-n.owner.Done():
			return core.ErrStale
		case <-ticker.C:
		}
	}
}
func (n *sessionNode) reconcileOutputs(ctx context.Context) {
	n.mu.Lock()
	ids := make([]core.ID, 0, len(n.members))
	for id := range n.members {
		ids = append(ids, id)
	}
	n.mu.Unlock()
	for _, id := range ids {
		output, err := n.manager.config.Core.GetOutput(ctx, id)
		if err != nil {
			continue
		}
		if !output.Attached || !output.DesiredRunning || output.CleanupPending {
			n.stopOutput(id)
		}
		n.mu.Lock()
		child := n.outputs[id]
		n.mu.Unlock()
		if child != nil {
			select {
			case <-child.done:
				child.mu.Lock()
				pending := !child.joined
				child.mu.Unlock()
				if pending {
					n.manager.log(child.cleanup(ctx, n.manager), "retry_output_resolution")
				}
			default:
			}
		}

	}
}
func (n *sessionNode) cleanup() {
	n.stop()
	ctx, stop := context.WithTimeout(context.Background(), n.manager.config.ShutdownTimeout)
	defer stop()
	n.manager.log(n.cleanupWithContext(ctx), "parent_cleanup")
	n.manager.signal()
}

func (n *sessionNode) cleanupWithContext(ctx context.Context) error {
	n.cleanupMu.Lock()
	defer n.cleanupMu.Unlock()
	n.stop()
	n.mu.Lock()
	members := make([]*membership, 0, len(n.members))
	for _, member := range n.members {
		members = append(members, member)
	}
	outputs := make([]*outputNode, 0, len(n.outputs))
	for _, output := range n.outputs {
		outputs = append(outputs, output)
	}
	n.mu.Unlock()
	for _, output := range outputs {
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
	}
	var result error
	for _, member := range members {
		result = errors.Join(result, member.close(ctx))
	}
	usersJoined := result == nil
	if n.registry != nil {
		if err := n.registry.Close(ctx); err != nil {
			n.manager.log(err, "registry_cleanup")
			if !n.registry.Joined() {
				result = errors.Join(result, err)
				usersJoined = false
			}
		}
	}
	if n.router != nil {
		result = errors.Join(result, n.router.Close())
	}
	if n.sampler != nil {
		if err := n.sampler.Close(ctx); err != nil {
			result = errors.Join(result, err)
			usersJoined = false
		}
	}
	if n.observer != nil {
		result = errors.Join(result, n.observer.Close(ctx))
	}
	if n.setupTicket != nil {
		if err := n.setupTicket.Complete(ctx); err != nil {
			result = errors.Join(result, err)
			usersJoined = false
		} else {
			n.setupTicket = nil
		}
	}
	scopeClosed := n.scope == nil
	if n.scope != nil {
		retryErr := n.scope.RetryCompleted(ctx)
		sealErr := n.scope.Seal(ctx)
		closeErr := n.scope.CloseContext(ctx)
		scopeClosed = retryErr == nil && sealErr == nil && closeErr == nil
		result = errors.Join(result, retryErr, sealErr, closeErr)
		proof, err := n.scope.Resolve(ctx)
		result = errors.Join(result, err)
		if !proof.Joined {
			result = errors.Join(result, processowner.ErrPending)
		}
	}
	n.mu.Lock()
	n.joined = result == nil
	n.mu.Unlock()
	// Каталог области исполнения сохраняет доказательства и удержания после освобождения завершённых объектов в памяти.
	n.mu.Lock()
	retirementPending := n.retirementNeeded
	n.mu.Unlock()
	if usersJoined && scopeClosed && !retirementPending {
		n.manager.mu.Lock()
		if n.manager.sessions[n.id] == n {
			delete(n.manager.sessions, n.id)
		}
		n.manager.mu.Unlock()
	}
	return result
}

func (n *sessionNode) retireRevokedExecution() {
	n.mu.Lock()
	n.retirementNeeded = true
	n.mu.Unlock()
	n.stop()
	ctx, cancel := n.manager.operationContext()
	n.manager.log(n.retireExecution(ctx), "retire_execution")
	cancel()
	n.cleanup()
}

func (n *sessionNode) retireExecution(ctx context.Context) error {
	n.mu.Lock()
	f, c, cause := n.fence, n.startReceipt, n.retirementFailure
	n.mu.Unlock()
	if f.CommandID == "" {
		current, err := n.manager.config.Core.GetReceipt(ctx, c.ID)
		if err != nil {
			return err
		}
		if !sameCommand(current, c) {
			return core.ErrStale
		}
		f = receiptFence(current)
		n.mu.Lock()
		n.fence = f
		n.mu.Unlock()
	}
	if f.CommandID == "" {
		f = receiptFence(c)
	}
	if cause != nil && f.Attempt > 0 {
		_ = n.manager.complete(ctx, f, false, cause)
	}
	_, _, err := n.manager.config.Core.CompensateSessionExecution(ctx, f, "manager:execution-stop:"+string(c.ID))
	if err != nil {
		return err
	}
	n.finishRetirement()
	n.manager.signal()
	return nil
}

func (n *sessionNode) finishRetirement() {
	n.mu.Lock()
	n.retirementNeeded = false
	joined := n.joined
	n.mu.Unlock()
	if joined {
		n.manager.mu.Lock()
		if n.manager.sessions[n.id] == n {
			delete(n.manager.sessions, n.id)
		}
		n.manager.mu.Unlock()
	}
}

func (m *Manager) retryRetirements(ctx context.Context) {
	m.mu.Lock()
	nodes := make([]*sessionNode, 0, m.config.MaxCommands)
	var outputs []*outputNode
	for _, node := range m.sessions {
		node.mu.Lock()
		pending := node.retirementNeeded
		for _, output := range node.outputs {
			select {
			case <-output.done:
				if len(outputs) < m.config.MaxCommands {
					outputs = append(outputs, output)
				}
			default:
			}
		}
		node.mu.Unlock()
		if pending && len(nodes) < m.config.MaxCommands {
			nodes = append(nodes, node)
		}
	}
	m.mu.Unlock()
	for _, node := range nodes {
		if ctx.Err() != nil {
			return
		}
		m.log(node.retireExecution(ctx), "retry_execution_retirement")
	}
	for _, output := range outputs {
		if ctx.Err() != nil {
			return
		}
		output.mu.Lock()
		pending := !output.joined
		output.mu.Unlock()
		if pending {
			m.log(output.cleanup(ctx, m), "retry_output_retirement")
		}
	}
}
