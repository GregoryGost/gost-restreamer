package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

type receiptFaultStore struct {
	*storage.Store
	mu       sync.Mutex
	kind     core.CommandKind
	stage    core.CommandStatus
	injected bool
	healed   chan struct{}
	blocked  chan struct{}
	expired  chan struct{}
	parent   *core.RuntimeLease
	events   []string
	dropped  int
}

func (s *receiptFaultStore) trace(ctx context.Context, stage string, began time.Time, before bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	remaining := time.Duration(0)
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	if len(s.events) == 512 {
		s.dropped++
		return
	}
	done := false
	if s.parent != nil {
		select {
		case <-s.parent.Done():
			done = true
		default:
		}
	}
	s.events = append(s.events, fmt.Sprintf("time=%s stage=%s elapsed=%s remaining=%s context=%v err=%v parent_before=%v parent_after=%v parent_done=%v injected=%v", time.Now().UTC().Format(time.RFC3339Nano), stage, time.Since(began), remaining, ctx.Err(), err, before, s.parent.Valid(), done, s.injected))
}

func (s *receiptFaultStore) parentValid() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parent.Valid()
}

func (s *receiptFaultStore) wait(ctx context.Context) error {
	s.mu.Lock()
	injected := s.injected
	s.mu.Unlock()
	if !injected {
		return nil
	}
	select {
	case <-s.healed:
		return nil
	default:
	}
	select {
	case s.blocked <- struct{}{}:
	default:
	}
	// Уже недоступный запрос завершается по своему сроку; восстановление открывает последующие запросы.
	<-ctx.Done()
	select {
	case s.expired <- struct{}{}:
	default:
	}
	return ctx.Err()
}

func (s *receiptFaultStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	began, before := time.Now(), s.parentValid()
	if err := s.wait(ctx); err != nil {
		s.trace(ctx, "wrapper_write_wait", began, before, err)
		return err
	}
	began, before = time.Now(), s.parentValid()
	var reached bool
	err := s.Store.Write(ctx, func(r core.Repositories) error {
		r.Runtime = receiptFaultRepository{RuntimeRepository: r.Runtime, store: s, reached: &reached}
		return callback(r)
	})
	s.trace(ctx, "underlying_write", began, before, err)
	if err != nil || !reached {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.injected {
		s.injected = true
		return core.ErrBusy
	}
	return nil
}

func (s *receiptFaultStore) View(ctx context.Context, callback func(core.Repositories) error) error {
	began, before := time.Now(), s.parentValid()
	if err := s.wait(ctx); err != nil {
		s.trace(ctx, "wrapper_view_wait", began, before, err)
		return err
	}
	began, before = time.Now(), s.parentValid()
	err := s.Store.View(ctx, callback)
	if err != nil || before != s.parentValid() {
		s.trace(ctx, "underlying_view", began, before, err)
	}
	return err
}

type receiptFaultRepository struct {
	core.RuntimeRepository
	store   *receiptFaultStore
	reached *bool
}

func (r receiptFaultRepository) UpdateReceipt(ctx context.Context, c core.CommandReceipt, revision uint64) error {
	if c.Kind == r.store.kind && c.Status == core.CommandSucceeded && r.store.stage == core.CommandSucceeded {
		r.store.mu.Lock()
		if !r.store.injected {
			r.store.injected = true
			r.store.mu.Unlock()
			return core.ErrBusy
		}
		r.store.mu.Unlock()
	}
	if err := r.RuntimeRepository.UpdateReceipt(ctx, c, revision); err != nil {
		return err
	}
	if c.Kind == r.store.kind && c.Status == r.store.stage {
		*r.reached = true
	}
	return nil
}

func TestManagerRetainsControlAcrossUnavailableDatabase(t *testing.T) {
	for _, stage := range []core.CommandStatus{core.CommandClaimed, core.CommandIssued, core.CommandSucceeded} {
		t.Run(string(stage), func(t *testing.T) {
			f := newManagerFixture(t, false, nil)
			if err := f.manager.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			fault := &receiptFaultStore{Store: f.store, kind: core.CommandStopSession, stage: stage, healed: make(chan struct{}), blocked: make(chan struct{}, 32), expired: make(chan struct{}, 32)}
			var healed sync.Once
			heal := func() { healed.Do(func() { close(fault.healed) }) }
			t.Cleanup(heal)
			service, err := core.NewService(fault, f.store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			c := f.config
			c.Core = service
			c.OperationTimeout = 40 * time.Millisecond
			manager, err := New(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				heal()
				if err := manager.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			start, err := manager.StartSession(context.Background(), core.StartSessionRequest{RequestID: "fault-start", StreamID: f.stream.ID})
			if err != nil {
				t.Fatal(err)
			}
			if current := waitReceipt(t, f, start); current.Status != core.CommandSucceeded {
				t.Fatal(current)
			}
			stopped, err := manager.StopSession(context.Background(), core.SessionCommand{RequestID: "fault-stop", SessionID: start.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			for range 4 {
				select {
				case <-fault.expired:
				case <-ctx.Done():
					t.Fatal("не достигнут срок недоступной операции", ctx.Err())
				}
			}
			heal()
			if current := waitReceipt(t, f, stopped); current.Status != core.CommandSucceeded {
				t.Fatal(current)
			}
			next, err := manager.StartSession(ctx, core.StartSessionRequest{RequestID: "fault-next", StreamID: f.stream.ID})
			if err != nil {
				t.Fatal("предыдущая команда сохранила активный слот", err)
			}
			if current := waitReceipt(t, f, next); current.Status != core.CommandSucceeded {
				t.Fatal(current)
			}
		})
	}
}

type delayedIngress struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (o *delayedIngress) CloseAdmission() {}
func (o *delayedIngress) Shutdown(ctx context.Context) error {
	o.once.Do(func() { close(o.entered) })
	select {
	case <-o.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestManagerHandsOffBeforePhysicalJoinAndRepeatedCloseIsExact(t *testing.T) {
	ingress := &delayedIngress{entered: make(chan struct{}), release: make(chan struct{})}
	f := newManagerFixture(t, false, func(c *Config) { c.Ingress = ingress })
	var released sync.Once
	release := func() { released.Do(func() { close(ingress.release) }) }
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	old, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "handoff-old", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, old); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	closing := make(chan error, 1)
	go func() { closing <- f.manager.Close(ctx) }()
	select {
	case <-ingress.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	c := f.config
	c.Ingress = &lifecycleOwner{}
	c.Preparation = &lifecycleOwner{}
	successor, err := New(ctx, c)
	if err != nil {
		t.Fatal("диспетчер не передан до физического присоединения", err)
	}
	t.Cleanup(func() {
		if err := successor.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	next, err := successor.StartSession(ctx, core.StartSessionRequest{RequestID: "handoff-new", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, next); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	release()
	select {
	case err = <-closing:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for range 2 {
		if err = f.manager.Close(ctx); err != nil {
			t.Fatal(err)
		}
		session, err := f.service.GetSession(ctx, next.SessionID)
		if err != nil || session.State != core.SessionRunning {
			t.Fatal("старый Close отозвал новую Session", session, err)
		}
		if _, err = successor.Feed(session); err != nil {
			t.Fatal("старый Close отозвал новый Feed", err)
		}
	}
}

func TestManagerCloseDeadlineKeepsRetryablePhysicalJoin(t *testing.T) {
	ingress := &delayedIngress{entered: make(chan struct{}), release: make(chan struct{})}
	f := newManagerFixture(t, false, func(c *Config) { c.Ingress = ingress })
	defer close(ingress.release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := f.manager.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("просроченное присоединение принято за успешное", err)
	}
}

func TestManagerRetiresUnobservedStartupClaimWithoutReplay(t *testing.T) {
	for _, kind := range []core.CommandKind{core.CommandStartSession, core.CommandAddOutput} {
		t.Run(string(kind), func(t *testing.T) {
			f := newManagerFixture(t, false, nil)
			if err := f.manager.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			fault := &receiptFaultStore{Store: f.store, kind: kind, stage: core.CommandClaimed, healed: make(chan struct{}), blocked: make(chan struct{}, 32), expired: make(chan struct{}, 32)}
			var healed sync.Once
			heal := func() { healed.Do(func() { close(fault.healed) }) }
			service, err := core.NewService(fault, f.store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			c := f.config
			c.Core = service
			c.StartupTimeout = 3 * time.Second
			c.OperationTimeout = 40 * time.Millisecond
			manager, err := New(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				heal()
				if err := manager.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			start, err := manager.StartSession(context.Background(), core.StartSessionRequest{RequestID: "claim-parent", StreamID: f.stream.ID})
			if err != nil {
				t.Fatal(err)
			}
			receipt := start
			defer func() {
				if !t.Failed() {
					return
				}
				fault.mu.Lock()
				for _, event := range fault.events {
					t.Log(event)
				}
				t.Logf("diagnostic_overflow=%d parent_valid=%v", fault.dropped, fault.parent.Valid())
				fault.mu.Unlock()
				diagnostic, stop := context.WithTimeout(context.Background(), time.Second)
				defer stop()
				session, sessionErr := f.service.GetSession(diagnostic, start.SessionID)
				current, receiptErr := f.service.GetReceipt(diagnostic, receipt.ID)
				snapshot, snapshotErr := f.service.RuntimeSnapshot(diagnostic, core.RuntimeSessionFence{SessionID: session.ID, Incarnation: session.Incarnation, Lifecycle: session.Lifecycle})
				t.Logf("SQL parent=%+v err=%v receipt=%+v err=%v RAM=%+v err=%v", session, sessionErr, current, receiptErr, snapshot, snapshotErr)
				manager.mu.Lock()
				node := manager.sessions[start.SessionID]
				manager.mu.Unlock()
				if node != nil {
					node.mu.Lock()
					t.Logf("node running=%v closing=%v joined=%v ctx=%v owner_valid=%v retirement=%v cause=%v", node.running, node.closing, node.joined, node.ctx.Err(), node.owner.Valid(), node.retirementNeeded, node.retirementFailure)
					node.mu.Unlock()
				} else {
					t.Log("node отсутствует до cleanup фикстуры")
				}
			}()
			if kind == core.CommandAddOutput {
				current := waitReceipt(t, f, start)
				if current.Status != core.CommandSucceeded {
					t.Fatal(current)
				}
				parent, bindErr := f.service.BindExecutionOwner(context.Background(), receiptFence(current))
				if bindErr != nil {
					t.Fatal(bindErr)
				}
				fault.mu.Lock()
				fault.parent = parent
				fault.mu.Unlock()
				receipt, err = manager.AddOutput(context.Background(), core.AddOutputRequest{RequestID: "claim-child", SessionID: start.SessionID, DestinationID: f.destination.ID})
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			select {
			case <-fault.blocked:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			// Ошибка записи переживает исходного обработчика запуска и его ограниченную очистку.
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				manager.mu.Lock()
				working := manager.commands[receipt.ID]
				manager.mu.Unlock()
				if !working {
					break
				}
				select {
				case <-ticker.C:
				case <-ctx.Done():
					t.Fatal("обработчик запуска не завершился", ctx.Err())
				}
			}
			heal()
			if current := waitReceipt(t, f, receipt); current.Status == core.CommandSucceeded {
				t.Fatal("просроченный запуск повторён после восстановления БД", current)
			}
			if kind == core.CommandStartSession {
				session, err := f.service.GetSession(ctx, start.SessionID)
				if err != nil || !session.State.Terminal() {
					t.Fatal("неизвестный Claim оставил STARTING", session, err)
				}
				next, err := manager.StartSession(ctx, core.StartSessionRequest{RequestID: "claim-next", StreamID: f.stream.ID})
				if err != nil {
					t.Fatal(err)
				}
				if current := waitReceipt(t, f, next); current.Status != core.CommandSucceeded {
					t.Fatal(current)
				}
			} else {
				output, err := f.service.GetOutput(ctx, receipt.OutputID)
				if err != nil || output.DesiredRunning || output.Issued {
					t.Fatal("неизвестный Claim оставил выход активным", output, err)
				}
				session, err := f.service.GetSession(ctx, start.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = manager.Feed(session); err != nil {
					t.Fatal("ошибка выхода отозвала здоровый parent", err)
				}
			}
		})
	}
}

func TestOutputCleanupRejectsDifferentGenerationBeforeCancellation(t *testing.T) {
	for _, withOutput := range []bool{false, true} {
		t.Run(map[bool]string{false: "membership", true: "publisher"}[withOutput], func(t *testing.T) {
			lifetime, cancel := context.WithCancel(context.Background())
			defer cancel()
			id := core.ID("new-output")
			node := &sessionNode{members: map[core.ID]*membership{}, outputs: map[core.ID]*outputNode{}, excluded: map[core.ID]uint64{}}
			if withOutput {
				node.outputs[id] = &outputNode{startReceipt: core.CommandReceipt{OutputGeneration: 3}, cancel: cancel, done: make(chan struct{})}
			} else {
				node.members[id] = &membership{output: core.SessionOutput{Generation: 3}, ctx: lifetime, cancel: cancel, done: make(chan struct{})}
			}
			ctx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err := node.waitOutputCleanup(ctx, id, 1); !errors.Is(err, core.ErrStale) {
				t.Fatal("очистка приняла другое поколение", err)
			}
			if lifetime.Err() != nil || len(node.excluded) != 0 {
				t.Fatal("старый fence изменил текущее исполнение", lifetime.Err(), node.excluded)
			}
		})
	}
}

func TestAcknowledgedOutputCleanupPagePreservesRestartedExecution(t *testing.T) {
	f := newManagerFixture(t, false, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "stale-page-parent", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, start); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	added, err := f.manager.AddOutput(ctx, core.AddOutputRequest{RequestID: "stale-page-add", SessionID: start.SessionID, DestinationID: f.destination.ID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, added); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	stopped, err := f.manager.StopOutput(ctx, core.OutputCommand{RequestID: "stale-page-stop", OutputID: added.OutputID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, stopped); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	page, err := f.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: start.SessionID, OutputID: added.OutputID}})
	if err != nil || len(page.Items) != 1 || !page.Items[0].Acknowledged {
		t.Fatal(page, err)
	}
	oldPage := page.Items[0]
	// Законное подтверждение уже разрешило новый запуск; старая страница ещё сообщает pending.
	oldPage.Acknowledged = false
	restarted, err := f.manager.StartOutput(ctx, core.OutputCommand{RequestID: "stale-page-restart", OutputID: added.OutputID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, restarted); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	if err = f.manager.cleanupRuntime(ctx, oldPage); err != nil {
		t.Fatal(err)
	}
	f.manager.mu.Lock()
	parent := f.manager.sessions[start.SessionID]
	f.manager.mu.Unlock()
	parent.mu.Lock()
	output := parent.outputs[added.OutputID]
	parent.mu.Unlock()
	if output == nil || output.startReceipt.OutputGeneration != restarted.OutputGeneration {
		t.Fatal("старая страница удалила текущее исполнение")
	}
	output.mu.Lock()
	prepared := output.publisher
	output.mu.Unlock()
	if prepared == nil {
		t.Fatal("новый Publisher отсутствует")
	}
	select {
	case <-prepared.Done():
		t.Fatal("старая страница остановила новый Publisher")
	default:
	}
}
