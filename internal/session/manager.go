// Package session координирует долговечные команды и принадлежащий им медиатракт.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

var ErrLimit = errors.New("исчерпан бюджет активной работы SessionManager")

// IngressOwner закрывает допуск OBS и присоединяет принадлежащие ему соединения.
type IngressOwner interface{ Shutdown(context.Context) error }

// PreparationOwner закрывает приём подготовки и присоединяет её пользователей.
type PreparationOwner interface {
	CloseAdmission()
	Reconcile(context.Context) error
	Close(context.Context) error
}

type Config struct {
	Core                                                                 *core.Service
	Files                                                                *fallback.FileStore
	Processes                                                            *processowner.Registry
	GeneratorVersion                                                     string
	Group                                                                group.RuntimeConfig
	Router                                                               media.RouterLimits
	Publisher                                                            publisher.Options
	Ingress                                                              IngressOwner
	Preparation                                                          PreparationOwner
	Logger                                                               *slog.Logger
	MaxSessions, MaxOutputs, MaxCommands, PageSize                       int
	MaxGroups, MaxReferences                                             uint32
	ReconcileInterval, StartupTimeout, OperationTimeout, ShutdownTimeout time.Duration
}

func (c Config) validate() error {
	if c.Core == nil || c.Files == nil || c.Processes == nil || c.GeneratorVersion == "" || c.Logger == nil || c.Ingress == nil || c.Preparation == nil || c.MaxSessions <= 0 || c.MaxOutputs <= 0 || c.MaxCommands <= 0 || c.PageSize <= 0 || c.PageSize > core.MaxPageLimit || c.MaxGroups == 0 || c.MaxReferences == 0 || c.ReconcileInterval <= 0 || c.StartupTimeout <= 0 || c.OperationTimeout <= 0 || c.ShutdownTimeout <= 0 || c.Router.Validate() != nil || c.Publisher.Validate() != nil {
		return core.ErrInvalid
	}
	c.Group.OpenFile = c.Files.OpenVerified
	return c.Group.Validate()
}

// Manager владеет диспетчером, Session и командами; БД и FileStore закрывает владелец приложения.
type Manager struct {
	config                       Config
	dispatcher                   *core.RuntimeDispatcher
	ctx                          context.Context
	cancel                       context.CancelFunc
	mu                           sync.Mutex
	closed                       bool
	commands                     map[core.ID]bool
	sessions                     map[core.ID]*sessionNode
	wake                         chan struct{}
	done                         chan struct{}
	workers                      sync.WaitGroup
	admissions                   sync.WaitGroup
	admissionWait                sync.Once
	admissionDone                chan struct{}
	workerWait                   sync.Once
	workerDone                   chan struct{}
	closeAdmission               sync.Once
	receiptCursor, cleanupCursor string
	recoveryIntentCursor         string
	recovery                     *cleanupProbe
}

func New(ctx context.Context, c Config) (*Manager, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	dispatcher, err := c.Core.ReserveDispatcher(ctx)
	if err != nil {
		return nil, err
	}
	// Срок исполнения задаёт Manager; отмена конструктора после возврата не отзывает Session.
	lifetime, cancel := context.WithCancel(context.Background())
	m := &Manager{config: c, dispatcher: dispatcher, ctx: lifetime, cancel: cancel, commands: map[core.ID]bool{}, sessions: map[core.ID]*sessionNode{}, wake: make(chan struct{}, 1), done: make(chan struct{}), admissionDone: make(chan struct{}), workerDone: make(chan struct{})}
	go m.run()
	return m, nil
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
func (m *Manager) admission() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || !m.dispatcher.Valid() {
		return core.ErrClosed
	}
	return nil
}

func (m *Manager) enter() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || !m.dispatcher.Valid() {
		return core.ErrClosed
	}
	m.admissions.Add(1)
	return nil
}

func (m *Manager) StartSession(ctx context.Context, request core.StartSessionRequest) (core.CommandReceipt, error) {
	if err := m.enter(); err != nil {
		return core.CommandReceipt{}, err
	}
	defer m.admissions.Done()
	prior, found, err := m.config.Core.PriorStartSession(ctx, request)
	if err != nil || found {
		if found {
			m.signal()
		}
		return prior, err
	}
	bindings, err := m.config.Core.SelectStartBindings(ctx, request.StreamID, request.DestinationIDs)
	if err != nil {
		return m.startAfterFailure(request, err)
	}
	for _, binding := range bindings {
		if err = m.checkBinding(ctx, binding); err != nil {
			return m.startAfterFailure(request, err)
		}
	}
	request.Bindings = bindings
	if err = m.admission(); err != nil {
		return core.CommandReceipt{}, err
	}
	receipt, err := m.config.Core.StartSession(ctx, request)
	if err != nil {
		return m.startAfterFailure(request, err)
	}
	if m.admission() != nil {
		cleanup, cancel := m.operationContext()
		m.log(m.stopForClose(cleanup, receipt.SessionID), "late_start_close")
		cancel()
	}
	m.signal()
	return receipt, err
}

func (m *Manager) AddOutput(ctx context.Context, request core.AddOutputRequest) (core.CommandReceipt, error) {
	if err := m.enter(); err != nil {
		return core.CommandReceipt{}, err
	}
	defer m.admissions.Done()
	prior, found, err := m.config.Core.PriorAddOutput(ctx, request)
	if err != nil || found {
		if found {
			m.signal()
		}
		return prior, err
	}
	request.Binding, err = m.config.Core.SelectAddBinding(ctx, request.SessionID, request.DestinationID)
	if err != nil {
		return m.addAfterFailure(request, err)
	}
	if err = m.checkBinding(ctx, request.Binding); err != nil {
		return m.addAfterFailure(request, err)
	}
	if err = m.admission(); err != nil {
		return core.CommandReceipt{}, err
	}
	receipt, err := m.config.Core.AddOutput(ctx, request)
	if err != nil {
		return m.addAfterFailure(request, err)
	}
	m.signal()
	return receipt, err
}
func (m *Manager) startAfterFailure(request core.StartSessionRequest, cause error) (core.CommandReceipt, error) {
	ctx, cancel := m.operationContext()
	defer cancel()
	receipt, found, err := m.config.Core.PriorStartSession(ctx, request)
	if err != nil {
		return core.CommandReceipt{}, errors.Join(cause, err)
	}
	if found {
		if m.admission() != nil {
			cleanup, stop := m.operationContext()
			m.log(m.stopForClose(cleanup, receipt.SessionID), "late_start_close")
			stop()
		}
		m.signal()
		return receipt, nil
	}
	return core.CommandReceipt{}, cause
}
func (m *Manager) addAfterFailure(request core.AddOutputRequest, cause error) (core.CommandReceipt, error) {
	ctx, cancel := m.operationContext()
	defer cancel()
	receipt, found, err := m.config.Core.PriorAddOutput(ctx, request)
	if err != nil {
		return core.CommandReceipt{}, errors.Join(cause, err)
	}
	if found {
		m.signal()
		return receipt, nil
	}
	return core.CommandReceipt{}, cause
}

func (m *Manager) checkBinding(ctx context.Context, b core.PreparedBinding) error {
	if b.GeneratorVersion != m.config.GeneratorVersion {
		return core.ErrNotReady
	}
	variant, err := m.config.Core.GetVariant(ctx, b.VariantID)
	if err != nil {
		return err
	}
	if variant.GenerationID != b.GenerationID || variant.GeneratorVersion != b.GeneratorVersion {
		return core.ErrStale
	}
	file, err := m.config.Files.OpenExact(ctx, variant.ManagedPath, variant.SizeBytes, variant.ContentHash)
	if err != nil {
		return err
	}
	return file.Close()
}
func (m *Manager) sessionCommand(ctx context.Context, r core.SessionCommand, call func(context.Context, core.SessionCommand) (core.CommandReceipt, error)) (core.CommandReceipt, error) {
	if err := m.enter(); err != nil {
		return core.CommandReceipt{}, err
	}
	defer m.admissions.Done()
	receipt, err := call(ctx, r)
	m.signal()
	return receipt, err
}
func (m *Manager) PauseSession(ctx context.Context, r core.SessionCommand) (core.CommandReceipt, error) {
	return m.sessionCommand(ctx, r, m.config.Core.PauseSession)
}
func (m *Manager) ResumeSession(ctx context.Context, r core.SessionCommand) (core.CommandReceipt, error) {
	return m.sessionCommand(ctx, r, m.config.Core.ResumeSession)
}
func (m *Manager) StopSession(ctx context.Context, r core.SessionCommand) (core.CommandReceipt, error) {
	return m.sessionCommand(ctx, r, m.config.Core.StopSession)
}
func (m *Manager) outputCommand(ctx context.Context, r core.OutputCommand, call func(context.Context, core.OutputCommand) (core.CommandReceipt, error)) (core.CommandReceipt, error) {
	if err := m.enter(); err != nil {
		return core.CommandReceipt{}, err
	}
	defer m.admissions.Done()
	receipt, err := call(ctx, r)
	m.signal()
	return receipt, err
}
func (m *Manager) StartOutput(ctx context.Context, r core.OutputCommand) (core.CommandReceipt, error) {
	return m.outputCommand(ctx, r, m.config.Core.StartOutput)
}
func (m *Manager) StopOutput(ctx context.Context, r core.OutputCommand) (core.CommandReceipt, error) {
	return m.outputCommand(ctx, r, m.config.Core.StopOutput)
}
func (m *Manager) DetachOutput(ctx context.Context, r core.OutputCommand) (core.CommandReceipt, error) {
	return m.outputCommand(ctx, r, m.config.Core.DetachOutput)
}

// Feed возвращает Router только точного текущего RUNNING исполнения.
func (m *Manager) Feed(session core.Session) (*media.Router, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || !m.dispatcher.Valid() {
		return nil, core.ErrClosed
	}
	node := m.sessions[session.ID]
	if node == nil {
		return nil, core.ErrNotReady
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if node.closing || !node.running || node.owner == nil || !node.owner.Valid() || session.State != core.SessionRunning || session.Incarnation != node.fence.Incarnation || session.Lifecycle != node.fence.Lifecycle || session.StreamID != node.streamID {
		return nil, core.ErrStale
	}
	return node.router, nil
}
func (m *Manager) operationContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), m.config.OperationTimeout)
}
func (m *Manager) log(err error, operation string) {
	if err != nil {
		m.config.Logger.Error("session_operation", "component", "session", "operation", operation, "error", err)
	}
}

func (m *Manager) run() {
	defer m.finishDispatch()
	ticker := time.NewTicker(m.config.ReconcileInterval)
	defer ticker.Stop()
	for {
		m.reconcile()
		select {
		case <-m.ctx.Done():
			return
		case <-m.dispatcher.Done():
			m.cancel()
			return
		case <-ticker.C:
		case <-m.wake:
		}
	}
}

func (m *Manager) beginClose() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.closeAdmission.Do(func() {
		m.config.Preparation.CloseAdmission()
		if owner, ok := m.config.Ingress.(interface{ CloseAdmission() }); ok {
			owner.CloseAdmission()
		}
	})
	m.cancel()
}

func (m *Manager) finishDispatch() {
	m.beginClose()
	m.admissionWait.Do(func() { go func() { m.admissions.Wait(); close(m.admissionDone) }() })
	<-m.admissionDone
	// Последний глобальный отзыв выполняется до передачи диспетчера следующему Manager.
	for m.dispatcher.Valid() {
		ctx, cancel := m.operationContext()
		err := m.revokeSessions(ctx)
		cancel()
		if err == nil {
			break
		}
		m.log(err, "revoke_sessions")
		if !retryPauseUnlessDispatcher(m.dispatcher, m.config.ReconcileInterval) {
			break
		}
	}
	m.mu.Lock()
	for _, node := range m.sessions {
		node.stop()
	}
	m.mu.Unlock()
	for {
		ctx, cancel := m.operationContext()
		err := m.dispatcher.CloseContext(ctx)
		cancel()
		if err == nil {
			break
		}
		m.log(err, "dispatcher_release")
		time.Sleep(m.config.ReconcileInterval)
	}
	close(m.done)
}

func retryPauseUnlessDispatcher(d *core.RuntimeDispatcher, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-d.Done():
		return false
	case <-timer.C:
		return true
	}
}
func (m *Manager) reconcile() {
	ctx, cancel := context.WithTimeout(m.ctx, m.config.OperationTimeout)
	defer cancel()
	page, err := m.config.Core.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: m.config.PageSize, PendingOnly: true}, Cursor: m.receiptCursor})
	if err != nil {
		m.log(err, "receipts")
		return
	}
	m.receiptCursor = page.NextCursor
	for _, receipt := range page.Items {
		if ctx.Err() != nil {
			return
		}
		m.dispatch(receipt)
	}
	m.retryRetirements(ctx)
	m.reconcileCleanup(ctx)
	m.log(m.config.Preparation.Reconcile(ctx), "preparation_reconcile")
}
func (m *Manager) dispatch(receipt core.CommandReceipt) {
	if receipt.Status != core.CommandPending {
		return
	}
	m.mu.Lock()
	if m.closed || m.commands[receipt.ID] || len(m.commands) >= m.config.MaxCommands {
		m.mu.Unlock()
		return
	}
	switch receipt.Kind {
	case core.CommandStartSession:
		if m.sessions[receipt.SessionID] != nil {
			m.mu.Unlock()
			return
		}
		active := 0
		for _, node := range m.sessions {
			node.mu.Lock()
			if !node.closing {
				active++
			}
			node.mu.Unlock()
		}
		if active >= m.config.MaxSessions {
			m.mu.Unlock()
			return
		}
	case core.CommandStartOutput, core.CommandAddOutput:
		node := m.sessions[receipt.SessionID]
		if node == nil {
			m.mu.Unlock()
			return
		}
		node.mu.Lock()
		ready := node.running && !node.closing
		node.mu.Unlock()
		if !ready {
			m.mu.Unlock()
			return
		}
	}
	if receipt.Kind == core.CommandStartSession {
		lifetime, cancel := context.WithCancel(m.ctx)
		m.sessions[receipt.SessionID] = &sessionNode{manager: m, id: receipt.SessionID, ctx: lifetime, cancel: cancel, done: make(chan struct{}), members: map[core.ID]*membership{}, outputs: map[core.ID]*outputNode{}, excluded: map[core.ID]uint64{}}
	}
	m.commands[receipt.ID] = true
	m.workers.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.workers.Done()
		defer func() { m.mu.Lock(); delete(m.commands, receipt.ID); m.mu.Unlock(); m.signal() }()
		switch receipt.Kind {
		case core.CommandStartSession:
			m.startSession(receipt)
		case core.CommandStartOutput, core.CommandAddOutput:
			m.startOutput(receipt)
		default:
			m.control(receipt)
		}
	}()
}

// issue сверяет неоднозначные результаты записи в БД до любого внешнего действия.
func (m *Manager) issue(ctx context.Context, c core.CommandReceipt) (core.ExecutionFence, error) {
	var f core.ExecutionFence
	if c.Status == core.CommandClaimed || c.Status == core.CommandIssued {
		f = receiptFence(c)
		if c.Status == core.CommandIssued {
			return f, nil
		}
	}
	for f.CommandID == "" {
		claimed, err := m.config.Core.Claim(ctx, c.RequestID)
		if err == nil {
			f = claimed
			break
		}
		check, stop := m.operationContext()
		current, e := m.config.Core.GetReceipt(check, c.ID)
		stop()
		if e == nil && current.SessionID == c.SessionID && current.Incarnation == c.Incarnation && current.Lifecycle == c.Lifecycle && current.OutputGeneration == c.OutputGeneration && current.Status == core.CommandClaimed && current.Attempt > 0 {
			f = receiptFence(current)
			break
		}
		if e == nil && current.Status != core.CommandPending {
			return claimed, core.ErrStale
		}
		if ctx.Err() != nil {
			return claimed, errors.Join(err, e, ctx.Err())
		}
		if e == nil && !errors.Is(err, core.ErrBusy) && !errors.Is(err, core.ErrNotReady) {
			return claimed, err
		}
		if !retryPause(ctx, m.config.ReconcileInterval) {
			return claimed, ctx.Err()
		}
	}
	for {
		err := m.config.Core.MarkIssued(ctx, f)
		if err == nil {
			return f, nil
		}
		check, stop := m.operationContext()
		current, e := m.config.Core.GetReceipt(check, c.ID)
		stop()
		if e == nil && receiptFence(current) == f && current.Status == core.CommandIssued {
			return f, nil
		}
		if e == nil && (receiptFence(current) != f || current.Status != core.CommandClaimed) {
			return f, core.ErrStale
		}
		if ctx.Err() != nil {
			return f, errors.Join(err, e, ctx.Err())
		}
		if e == nil && !errors.Is(err, core.ErrBusy) && !errors.Is(err, core.ErrNotReady) {
			return f, err
		}
		if !retryPause(ctx, m.config.ReconcileInterval) {
			return f, ctx.Err()
		}
	}
}

func receiptFence(c core.CommandReceipt) core.ExecutionFence {
	return core.ExecutionFence{CommandID: c.ID, Incarnation: c.Incarnation, SessionID: c.SessionID, Lifecycle: c.Lifecycle, OutputID: c.OutputID, OutputGeneration: c.OutputGeneration, Attempt: c.Attempt}
}
func (m *Manager) complete(ctx context.Context, f core.ExecutionFence, success bool, failure error) error {
	completion := core.Completion{Success: success}
	if !success {
		if failure == nil {
			failure = core.ErrNotReady
		}
		completion.Failure = failure.Error()
	}
	err := m.config.Core.Complete(ctx, f, completion)
	if err == nil {
		return nil
	}
	check, cancel := m.operationContext()
	defer cancel()
	current, readErr := m.config.Core.GetReceipt(check, f.CommandID)
	if readErr == nil && receiptFence(current) == f && (success && current.Status == core.CommandSucceeded || !success && current.Status == core.CommandFailed) {
		return nil
	}
	return errors.Join(err, readErr)
}
func sameCommand(a, b core.CommandReceipt) bool {
	return a.ID == b.ID && a.RequestID == b.RequestID && a.Kind == b.Kind && a.SessionID == b.SessionID && a.Incarnation == b.Incarnation && a.Lifecycle == b.Lifecycle && a.OutputID == b.OutputID && a.OutputGeneration == b.OutputGeneration
}

func (m *Manager) control(c core.CommandReceipt) {
	var fence core.ExecutionFence
	var applied bool
	var failure error
	for m.ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(m.ctx, m.config.OperationTimeout)
		current, err := m.config.Core.GetReceipt(ctx, c.ID)
		if err == nil && (!sameCommand(current, c) || current.Status.Terminal() || fence.CommandID != "" && receiptFence(current) != fence) {
			cancel()
			return
		}
		if err == nil && !applied {
			fence, err = m.issue(ctx, current)
			if err == nil {
				failure, applied = m.applyControl(ctx, c)
				if !applied {
					err = failure
				}
			}
		}
		if err == nil && applied {
			// Очистка сама завершает StopOutput/DetachOutput после физического присоединения.
			if c.Kind == core.CommandStopOutput || c.Kind == core.CommandDetachOutput {
				cancel()
				return
			}
			err = m.complete(ctx, fence, failure == nil, failure)
			if err == nil {
				cancel()
				return
			}
		}
		cancel()
		m.log(err, "continue_control")
		// Один ограниченный работник сохраняет точный результат; внешний шаг не повторяется.
		if !retryPause(m.ctx, m.config.ReconcileInterval) {
			return
		}
	}
}

func (m *Manager) applyControl(ctx context.Context, c core.CommandReceipt) (error, bool) {
	m.mu.Lock()
	node := m.sessions[c.SessionID]
	m.mu.Unlock()
	switch c.Kind {
	case core.CommandPauseSession, core.CommandResumeSession:
		if node == nil {
			return core.ErrNotReady, true
		}
		session, err := m.config.Core.GetSession(ctx, c.SessionID)
		if err != nil {
			return err, false
		}
		node.mu.Lock()
		registry := node.registry
		node.mu.Unlock()
		if registry == nil {
			return core.ErrNotReady, true
		}
		return registry.ApplyControl(session.Revision, session.DesiredPaused), true
	case core.CommandStopSession:
		if node != nil {
			node.stop()
		}
	case core.CommandStopOutput, core.CommandDetachOutput:
		if node != nil {
			node.stopOutput(c.OutputID)
		}
	}
	return nil, true
}

// Close запрещает новый допуск, отзывает команды и независимо ограничивает shutdown.
// Повторный вызов может дождаться операций, не завершившихся в предыдущий срок.
func (m *Manager) Close(ctx context.Context) error {
	m.beginClose()
	shutdown, cancel := context.WithTimeout(context.Background(), m.config.ShutdownTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	select {
	case <-m.done:
	case <-shutdown.Done():
		return errors.Join(shutdown.Err(), ctx.Err())
	}
	// После передачи диспетчера остаются только ресурсы этого Manager.
	var result error
	result = errors.Join(result, m.config.Ingress.Shutdown(shutdown), m.config.Preparation.Close(shutdown))
	m.workerWait.Do(func() { go func() { m.workers.Wait(); close(m.workerDone) }() })
	select {
	case <-m.workerDone:
	case <-shutdown.Done():
		return errors.Join(result, shutdown.Err(), ctx.Err())
	}
	m.mu.Lock()
	nodes := make([]*sessionNode, 0, len(m.sessions))
	for _, node := range m.sessions {
		nodes = append(nodes, node)
	}
	m.mu.Unlock()
	for _, node := range nodes {
		select {
		case <-node.done:
		case <-shutdown.Done():
			return errors.Join(result, shutdown.Err(), ctx.Err())
		}
		node.mu.Lock()
		joined := node.joined
		node.mu.Unlock()
		if !joined {
			result = errors.Join(result, node.cleanupWithContext(shutdown))
		}
	}
	return result
}

func (m *Manager) stopForClose(ctx context.Context, id core.ID) error {
	receipt, err := m.config.Core.StopSession(ctx, core.SessionCommand{RequestID: "manager:close:" + string(id), SessionID: id})
	if err != nil {
		return err
	}
	if !receipt.Status.Terminal() {
		f, e := m.issue(ctx, receipt)
		if e == nil {
			e = m.complete(ctx, f, true, nil)
		}
		return e
	}
	return nil
}
func (m *Manager) revokeSessions(ctx context.Context) error {
	var result error
	for _, state := range []core.SessionState{core.SessionStarting, core.SessionRunning, core.SessionStopping} {
		request := core.PageRequest{Query: core.ListQuery{Limit: m.config.PageSize, State: string(state)}}
		for {
			page, err := m.config.Core.ListSessions(ctx, request)
			if err != nil {
				return errors.Join(result, err)
			}
			for _, session := range page.Items {
				result = errors.Join(result, m.stopForClose(ctx, session.ID))
			}
			if page.NextCursor == "" {
				break
			}
			request.Cursor = page.NextCursor
		}
	}
	return result
}

type scopeOwner struct {
	Version int                 `json:"version"`
	Kind    string              `json:"kind"`
	Fence   core.ExecutionFence `json:"fence"`
}

func encodeOwner(f core.ExecutionFence) string {
	b, _ := json.Marshal(scopeOwner{Version: 1, Kind: "session", Fence: f})
	return string(b)
}
