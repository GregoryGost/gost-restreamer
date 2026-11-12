package session

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// sessionNode сохраняет локальное исполнение одной Session, его медиаресурсы и незавершённые продолжения
// очистки и компенсации.
type sessionNode struct {
	// manager предоставляет зависимости, диспетчеризацию и повторные операции данного узла.
	manager *Manager
	// id обозначает Session; streamID обозначает её Stream для проверки Feed и корреляции
	// диагностики.
	id, streamID core.ID
	// fence содержит точную выданную попытку запуска Session.
	fence core.ExecutionFence
	// ctx задаёт срок локального исполнения Session от Manager.
	ctx context.Context
	// cancel отзывает Session и зависимые работники медиатракта.
	cancel context.CancelFunc
	// done закрывается при возврате работника Session, отдельно от успешности физической очистки.
	done chan struct{}
	// mu защищает состояние выполнения, ресурсы и наборы членств и выходов Session.
	mu sync.Mutex
	// closing запрещает новое локальное исполнение; running допускает Feed и выходы; joined
	// подтверждает успешную очистку ресурсов.
	closing, running, joined bool
	// owner предоставляет допуск точного выполнения Session и сигнал его отзыва.
	owner *core.RuntimeLease
	// observer публикует входные наблюдения и создаёт наблюдателей групп.
	observer *core.SessionObserver
	// scope учитывает процессы и внешние backend-операции данного выполнения.
	scope *processowner.Scope
	// router содержит входной Router Session для публикации OBS и подписок групп.
	router *media.Router
	// registry разделяет обработку групп выбранных выходов Session.
	registry *group.Registry
	// sampler периодически собирает оперативные измерения входа, групп, выходов и FFmpeg.
	sampler *monitoring.SessionSampler
	// members хранит предварительные ссылки выходов на группы, включая барьер запуска Session.
	members map[core.ID]*membership
	// outputs хранит отдельные работники отправителей с их состоянием очистки.
	outputs map[core.ID]*outputNode
	// excluded сохраняет наибольшее исключённое поколение выхода во время первоначального барьера.
	excluded map[core.ID]uint64
	// cleanupMu исключает одновременную физическую очистку ресурсов узла.
	cleanupMu sync.Mutex
	// setupTicket сохраняет незавершённую регистрацию Router и Registry для повтора финальной
	// записи.
	setupTicket *processowner.Ticket
	// retirementNeeded отмечает необходимость долговечного завершения текущего исполнения Session.
	retirementNeeded bool
	// startReceipt сохраняет исходную идентичность команды запуска для точной компенсации после
	// неоднозначных записей.
	startReceipt core.CommandReceipt
	// retirementFailure сохраняет исходную причину отказа запуска до отдельного долговечного
	// завершения.
	retirementFailure error
}

// membership удерживает ссылку конкретного поколения выхода на общую группу и учитывает её позднее
// получение и очистку.
type membership struct {
	// parent содержит локальную Session, которой принадлежит членство.
	parent *sessionNode
	// output сохраняет поколение и закреплённый снимок выхода при создании членства.
	output core.SessionOutput
	// ctx ограничивает получение группы и ожидание её текущей готовности.
	ctx context.Context
	// cancel прекращает работу членства без потери поздно возвращённой ссылки.
	cancel context.CancelFunc
	// done закрывается после возврата работника членства; ready закрывается после сохранения исхода
	// получения и готовности группы.
	done, ready chan struct{}
	// mu защищает остановку, сохранённую ссылку и результат ожидания готовности.
	mu sync.Mutex
	// stopped запрещает дальнейшее ожидание и использование членства после отзыва.
	stopped bool
	// reference сохраняет полученную ссылку, включая поздний результат Acquire после остановки.
	reference *group.Reference
	// ticket учитывает внешние операции членства до освобождения ссылки и финальной записи.
	ticket *processowner.Ticket
	// err сохраняет отказ получения либо ожидания текущей готовности группы.
	err error
	// closeMu упорядочивает повторяемые отмену, ожидание и освобождение членства.
	closeMu sync.Mutex
}

// outputNode сохраняет точное исполнение независимого выхода, отправителя и продолжения компенсации и
// очистки.
type outputNode struct {
	// fence содержит точную выданную попытку запуска выхода.
	fence core.ExecutionFence
	// startReceipt сохраняет исходную квитанцию для проверки компенсации без изменения её
	// идентичности.
	startReceipt core.CommandReceipt
	// cancel отменяет контекст работника данного выхода.
	cancel context.CancelFunc
	// done закрывается после возврата работника, отдельно от успешного освобождения ресурсов.
	done chan struct{}
	// member удерживает группу конкретного поколения выхода до физической очистки.
	member *membership
	// mu защищает состояние отправителя, наблюдателя и результатов завершения выхода.
	mu sync.Mutex
	// publisher содержит подготовленный либо активированный независимый отправитель.
	publisher *publisher.Publisher
	// observer содержит точного наблюдателя выданной попытки выхода.
	observer *core.OutputObserver
	// joined отмечает успешное завершение очистки отправителя, членства и наблюдателя.
	joined bool
	// released отмечает уже освобождённую резервацию исполнения, исключая повторное освобождение.
	released bool
	// failure сохраняет исходную причину отказа запуска либо окончательного отказа отправителя.
	failure error
	// finalFailure отличает окончательный отказ работающего Publisher от отказа при запуске.
	finalFailure bool
	// cleanupMu исключает одновременные повторы компенсации и физической очистки выхода.
	cleanupMu sync.Mutex
}

// stop помечает сессию закрывающейся, отзывает Feed и отменяет контексты сессии, членств и выходов без
// ожидания их завершения.
func (n *sessionNode) stop() {
	n.mu.Lock()
	n.closing = true
	n.running = false
	n.cancel()
	// member содержит очередное членство, отменяемое при остановке всей Session.
	for _, member := range n.members {
		member.cancel()
	}
	// output содержит очередного отправителя, отменяемого при остановке Session.
	for _, output := range n.outputs {
		output.cancel()
	}
	n.mu.Unlock()
}

// stopOutput отменяет только членство и отправителя указанного выхода, сохраняя работу остальных
// выходов сессии.
func (n *sessionNode) stopOutput(id core.ID) {
	n.mu.Lock()
	// member содержит членство выбранного выхода; output содержит его независимого работника.
	member, output := n.members[id], n.outputs[id]
	n.mu.Unlock()
	if member != nil {
		member.stop()
	}
	if output != nil {
		output.cancel()
	}
}

// commandFinished снимает учёт команды до окончания длительного эфира, освобождая бюджет MaxCommands
// для следующих команд; сам работник остаётся учтённым до возврата.
func (m *Manager) commandFinished(id core.ID) {
	m.mu.Lock()
	delete(m.commands, id)
	m.mu.Unlock()
	m.signal()
}

// startSession выдаёт попытку, создаёт медиатракт и ждёт готовности оставшихся групп до успешной
// квитанции и Feed. Затем сверяет управление и выходы; отказ запуска или потеря владельца требуют
// отдельного долговечного завершения и очистки.
func (m *Manager) startSession(c core.CommandReceipt) {
	m.mu.Lock()
	// node содержит предварительно зарегистрированный узел запускаемой Session.
	node := m.sessions[c.SessionID]
	m.mu.Unlock()
	if node == nil {
		return
	}
	defer close(node.done)
	// startup ограничивает выдачу и барьер одним сроком StartupTimeout; cancel освобождает таймер
	// запуска.
	startup, cancel := context.WithTimeout(node.ctx, m.config.StartupTimeout)
	defer cancel()
	// f содержит точную попытку запуска Session; err сохраняет результаты выдачи, инициализации,
	// барьера и успешного Complete.
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
		// cleanup ограничивает долговечное завершение отказавшей инициализации; stop отменяет
		// контекст cleanup после операции.
		cleanup, stop := m.operationContext()
		m.log(node.retireExecution(cleanup), "retire_startup")
		stop()
		m.log(err, "startup")
		node.stop()
		node.cleanup()
		return
	}
	if err = node.barrier(startup); err == nil {
		// Feed ждёт завершения COMMIT и сверки неоднозначного результата под тем же мьютексом.
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
		// cleanup ограничивает долговечное завершение отказавшего барьера; stop отменяет
		// контекст cleanup после операции.
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
	// ticker задаёт период сверки работающей Session и её выходов.
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
		// ctx ограничивает текущую сверку сроком Session и OperationTimeout; stop освобождает
		// таймер.
		ctx, stop := context.WithTimeout(node.ctx, m.config.OperationTimeout)
		// session содержит актуальное сохранённое состояние; e сохраняет результаты чтения и
		// применения ревизии паузы.
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

// initialize связывает точных владельцев сессии, создаёт область процессов, Router, сборщик метрик и
// реестр групп, затем регистрирует первоначальные членства. Частично созданные ресурсы сохраняются в
// узле для последующей очистки.
func (n *sessionNode) initialize(ctx context.Context) error {
	// m содержит управляющий Manager с общими зависимостями и пределами узла.
	m := n.manager
	// session содержит сохранённую Session для привязки Stream; err сохраняет результаты создания
	// локальных ресурсов.
	session, err := m.config.Core.GetSession(ctx, n.id)
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.streamID = session.StreamID
	n.mu.Unlock()
	// owner содержит полученный допуск точного выполнения Session.
	owner, err := m.config.Core.BindExecutionOwner(ctx, n.fence)
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.owner = owner
	n.mu.Unlock()
	// observer содержит наблюдателя входа и будущих групп точного выполнения.
	observer, err := m.config.Core.OpenSessionObserver(ctx, n.fence)
	if err != nil {
		return err
	}
	n.observer = observer
	// scope содержит созданную либо частичную область процессов, сохраняемую в узле для очистки.
	scope, err := m.config.Processes.Create(ctx, encodeOwner(n.fence))
	n.mu.Lock()
	n.scope = scope
	n.mu.Unlock()
	if err != nil {
		return err
	}
	// ticket учитывает создание Router и Registry до завершения их настройки.
	ticket, err := scope.Register(ctx, "router-registry")
	n.setupTicket = ticket
	if err != nil {
		return err
	}
	// router содержит новый ограниченный входной Router Session.
	router, err := media.NewRouter(m.config.Router)
	if err != nil {
		return errors.Join(err, ticket.Complete(ctx))
	}
	n.router = router
	// sampler содержит сборщик измерений со сроком локального выполнения Session.
	sampler, err := monitoring.NewSessionSampler(n.ctx, m.config.Monitoring)
	if err != nil {
		return err
	}
	n.sampler = sampler
	if err = n.registerInputMetrics(); err != nil {
		return err
	}
	// config содержит настройки групп с точными файловым портом, Scope, журналом и сборщиком данного
	// выполнения.
	config := m.config.Group
	config.OpenFile = m.openPreparedFile
	config.Scope = scope
	config.Logger, config.Sampler, config.MetricStaleAfter = m.config.Logger.With("stream_id", n.streamID), sampler, m.config.Monitoring.StaleAfter
	// registry содержит созданный реестр групп точного владельца Session.
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
	// request задаёт текущую страницу первоначально выбранных выходов Session.
	request := core.PageRequest{Query: core.ListQuery{Limit: m.config.PageSize, SessionID: n.id}}
	for {
		// page содержит очередную страницу выходов; e сохраняет результаты чтения и
		// предварительного acquire.
		page, e := m.config.Core.ListOutputs(ctx, request)
		if e != nil {
			return e
		}
		// output содержит очередной подключённый желаемый выход для начального членства.
		for _, output := range page.Items {
			if !output.Attached || !output.DesiredRunning || output.CleanupPending {
				continue
			}
			n.mu.Lock()
			// count сохраняет текущее число членств для проверки MaxOutputs.
			count := len(n.members)
			n.mu.Unlock()
			if count >= m.config.MaxOutputs {
				return ErrLimit
			}
			if _, e = n.acquire(ctx, output); e != nil {
				// required показывает, осталось ли прежнее поколение выхода нужным после
				// ошибки acquire; checkErr сообщает об отказе повторного чтения.
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

// acquire регистрирует операцию членства до запуска его работника и возвращает прежнее членство только
// для того же поколения выхода. Закрытие, исключённое поколение и исчерпание бюджета запрещают новую
// регистрацию.
func (n *sessionNode) acquire(ctx context.Context, output core.SessionOutput) (*membership, error) {
	if output.Snapshot.Artifact.GeneratorVersion != n.manager.config.GeneratorVersion {
		return nil, core.ErrNotReady
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closing || output.Generation <= n.excluded[output.ID] {
		return nil, core.ErrStale
	}
	// prior содержит ранее зарегистрированное членство того же OutputID для проверки поколения.
	if prior := n.members[output.ID]; prior != nil {
		if prior.output.Generation != output.Generation {
			return nil, core.ErrNotReady
		}
		return prior, nil
	}
	if len(n.members) >= n.manager.config.MaxOutputs {
		return nil, ErrLimit
	}
	// ticket учитывает операции нового членства; err сообщает об отказе регистрации в Scope.
	ticket, err := n.scope.Register(ctx, "membership:"+string(output.ID))
	if err != nil {
		return nil, err
	}
	// lifetime задаёт отдельный срок членства от Session; cancel отзывает только это членство.
	lifetime, cancel := context.WithCancel(n.ctx)
	// member содержит новый учтённый работник получения группы для конкретного поколения выхода.
	member := &membership{parent: n, output: output, ctx: lifetime, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}), ticket: ticket}
	n.members[output.ID] = member
	go member.run()
	return member, nil
}

// openPreparedFile отклоняет манифест другой версии генератора и открывает закреплённый файл через
// проверку FileStore.
func (m *Manager) openPreparedFile(ctx context.Context, manifest core.ArtifactManifest) (*os.File, error) {
	if manifest.GeneratorVersion != m.config.GeneratorVersion {
		return nil, core.ErrNotReady
	}
	return m.config.Files.OpenVerified(ctx, manifest)
}

// run получает ссылку на группу и ждёт готовности заглушки, сохраняя как ссылку, так и ошибку до
// закрытия ready. Поздняя выдача ссылки после остановки остаётся доступной для очистки.
func (r *membership) run() {
	defer close(r.done)
	// ref содержит результат Registry.Acquire; err сохраняет отказ получения либо ожидания
	// готовности группы.
	ref, err := r.parent.registry.Acquire(r.ctx, r.output.Snapshot, r.parent.router)
	r.mu.Lock()
	r.reference, r.err = ref, err
	// stopped сохраняет факт остановки или отмены членства после возврата Acquire.
	stopped := r.stopped || r.ctx.Err() != nil
	r.mu.Unlock()
	if err == nil && !stopped {
		// startup ограничивает ожидание готовности полученной группы; cancel отменяет контекст
		// startup после возврата из ожидания.
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

// stop запрещает дальнейшую работу членства и отменяет его контекст, оставляя ожидание и освобождение
// ссылки методу close.
func (r *membership) stop() { r.mu.Lock(); r.stopped = true; r.cancel(); r.mu.Unlock() }

// close последовательно отменяет членство, ждёт возврата Acquire и готовности, освобождает ссылку на
// группу и подтверждает зарегистрированную операцию. Истечение контекста сохраняет незавершённые
// ресурсы для повтора.
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
	// ref содержит сохранённую ссылку на группу после завершения работника членства.
	ref := r.reference
	r.mu.Unlock()
	if ref != nil {
		// err содержит результат закрытия ссылки и ожидания группы, различаемый с фактическим
		// Group.Done.
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
		// err сообщает об отказе финальной записи завершения операции членства.
		if err := r.ticket.Complete(ctx); err != nil {
			return err
		}
		r.ticket = nil
	}
	return nil
}

// membershipRequired повторно читает выход и проверяет, что именно его поколение ещё прикреплено,
// запущено по желанию и не ожидает очистки.
func (n *sessionNode) membershipRequired(ctx context.Context, output core.SessionOutput) (bool, error) {
	// current содержит актуальный сохранённый выход; err сообщает об отказе проверки нужности того
	// же поколения.
	current, err := n.manager.config.Core.GetOutput(ctx, output.ID)
	if err != nil {
		return false, err
	}
	return current.Attached && current.DesiredRunning && !current.CleanupPending && current.Generation == output.Generation, nil
}

// barrier ждёт текущей готовности каждой ещё нужной группы. При отказе членства повторно читает выход,
// чтобы принятая остановка исключила его из барьера, а действующая ошибка сохранилась.
func (n *sessionNode) barrier(ctx context.Context) error {
	// ticker задаёт период повторной проверки барьера оставшихся групп Session.
	ticker := time.NewTicker(n.manager.config.ReconcileInterval)
	defer ticker.Stop()
	for {
		n.mu.Lock()
		// members сохраняет текущий список предварительных членств для проверки вне блокировки
		// узла.
		members := make([]*membership, 0, len(n.members))
		// member содержит очередное членство при копировании списка начального барьера.
		for _, member := range n.members {
			members = append(members, member)
		}
		n.mu.Unlock()
		// ready показывает готовность всех ещё требуемых групп текущего прохода.
		ready := true
		// member содержит очередное членство для проверки нужности и текущей готовности.
		for _, member := range members {
			// required показывает, требуется ли прежнее поколение выхода; err сохраняет
			// результат чтения и проверки готовности.
			required, err := n.membershipRequired(ctx, member.output)
			if err != nil {
				return err
			}
			if !required {
				member.stop()
				continue
			}
			select {
			// err сохраняет исход ожидания членства; ref содержит его фактически полученную
			// ссылку на группу.
			case <-member.ready:
				member.mu.Lock()
				err, ref := member.err, member.reference
				member.mu.Unlock()
				if err == nil && ref == nil {
					err = core.ErrNotReady
				}
				if err != nil {
					// Stop мог зафиксироваться после первого чтения и отменить ожидание готовности.
					// required повторно проверяет нужность выхода после ошибки;
					// checkErr сообщает об отказе этой контрольной проверки.
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

// reconcileOutputs отзывает переставшие требоваться членства и повторяет очистку завершившихся
// работников выходов с неподтверждённым освобождением ресурсов.
func (n *sessionNode) reconcileOutputs(ctx context.Context) {
	n.mu.Lock()
	// ids сохраняет идентификаторы членств для сверки вне блокировки Session.
	ids := make([]core.ID, 0, len(n.members))
	// id содержит очередной OutputID при копировании набора членств.
	for id := range n.members {
		ids = append(ids, id)
	}
	n.mu.Unlock()
	// id содержит очередной OutputID, чьё желание и очистка сверяются.
	for _, id := range ids {
		// output содержит актуальный сохранённый выход; err сообщает об отказе его чтения.
		output, err := n.manager.config.Core.GetOutput(ctx, id)
		if err != nil {
			continue
		}
		if !output.Attached || !output.DesiredRunning || output.CleanupPending {
			n.stopOutput(id)
		}
		n.mu.Lock()
		// child содержит локального отправителя проверяемого выхода, если он уже создан.
		child := n.outputs[id]
		n.mu.Unlock()
		if child != nil {
			select {
			// pending показывает необходимость повторной очистки уже вернувшегося
			// отправителя.
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

// cleanup останавливает сессию и выполняет очистку с отдельным сроком завершения, после чего будит
// диспетчер для продолжения сверки.
func (n *sessionNode) cleanup() {
	n.stop()
	// ctx ограничивает отдельную очистку Session сроком ShutdownTimeout; stop освобождает таймер.
	ctx, stop := context.WithTimeout(context.Background(), n.manager.config.ShutdownTimeout)
	defer stop()
	n.manager.log(n.cleanupWithContext(ctx), "parent_cleanup")
	n.manager.signal()
}

// cleanupWithContext последовательно ждёт выходы, закрывает членства и общие ресурсы, завершает
// зарегистрированные операции и проверяет область процессов. Узел удаляется из Manager лишь после
// завершения пользователей, закрытия области и долговечного завершения исполнения.
func (n *sessionNode) cleanupWithContext(ctx context.Context) error {
	n.cleanupMu.Lock()
	defer n.cleanupMu.Unlock()
	n.stop()
	n.mu.Lock()
	// members сохраняет текущие членства для последовательного закрытия вне блокировки узла.
	members := make([]*membership, 0, len(n.members))
	// member содержит очередное членство при сборе ресурсов очистки.
	for _, member := range n.members {
		members = append(members, member)
	}
	// outputs сохраняет текущих отправителей для ожидания перед закрытием общих ресурсов.
	outputs := make([]*outputNode, 0, len(n.outputs))
	// output содержит очередного отправителя при сборе ресурсов очистки.
	for _, output := range n.outputs {
		outputs = append(outputs, output)
	}
	n.mu.Unlock()
	// output содержит отправителя, чей работник должен вернуться до освобождения общего медиатракта.
	for _, output := range outputs {
		select {
		case <-output.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		output.mu.Lock()
		// joined сохраняет результат предыдущей очистки отправителя для выбора необходимого
		// повтора.
		joined := output.joined
		output.mu.Unlock()
		if !joined {
			// err сообщает об отказе повторной очистки уже завершившегося работника выхода.
			if err := output.cleanup(ctx, n.manager); err != nil {
				return err
			}
		}
	}
	// result объединяет ошибки закрытия членств, общих ресурсов и доказательства процессов.
	var result error
	// member содержит очередное членство для освобождения группы и завершения операции Scope.
	for _, member := range members {
		result = errors.Join(result, member.close(ctx))
	}
	// usersJoined отмечает завершение пользователей, требуемое до удаления узла из RAM.
	usersJoined := result == nil
	if n.registry != nil {
		// err содержит результат закрытия реестра групп; фактическое присоединение проверяется
		// отдельно через Registry.Joined.
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
		// err сообщает об отказе ожидания завершения сборщика измерений.
		if err := n.sampler.Close(ctx); err != nil {
			result = errors.Join(result, err)
			usersJoined = false
		}
	}
	if n.observer != nil {
		result = errors.Join(result, n.observer.Close(ctx))
	}
	if n.setupTicket != nil {
		// err сообщает об отказе финальной записи операции настройки Router и Registry.
		if err := n.setupTicket.Complete(ctx); err != nil {
			result = errors.Join(result, err)
			usersJoined = false
		} else {
			n.setupTicket = nil
		}
	}
	// scopeClosed показывает успешное закрытие backend-владения Scope либо отсутствие созданной
	// области.
	scopeClosed := n.scope == nil
	if n.scope != nil {
		// retryErr содержит результат повторных финальных записей уже вернувшихся операций
		// Scope.
		retryErr := n.scope.RetryCompleted(ctx)
		// sealErr содержит результат долговечного запрета новых операций Scope.
		sealErr := n.scope.Seal(ctx)
		// closeErr содержит результат закрытия локального backend-владения областью.
		closeErr := n.scope.CloseContext(ctx)
		scopeClosed = retryErr == nil && sealErr == nil && closeErr == nil
		result = errors.Join(result, retryErr, sealErr, closeErr)
		// proof содержит текущее доказательство завершения и очистки области; err сообщает об
		// отказе сверки.
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
	// retirementPending сохраняет необходимость долговечного завершения, препятствующую удалению
	// узла из Manager.
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

// retireRevokedExecution сохраняет необходимость долговечного завершения отозванного исполнения,
// останавливает медиатракт и пытается записать результат до физической очистки.
func (n *sessionNode) retireRevokedExecution() {
	n.mu.Lock()
	n.retirementNeeded = true
	n.mu.Unlock()
	n.stop()
	// ctx ограничивает отдельную компенсацию отозванного исполнения; cancel отменяет контекст ctx
	// после попытки компенсации.
	ctx, cancel := n.manager.operationContext()
	n.manager.log(n.retireExecution(ctx), "retire_execution")
	cancel()
	n.cleanup()
}

// retireExecution восстанавливает точную попытку из квитанции при необходимости и запрашивает
// компенсацию сессии. Неудавшаяся запись сохраняет признак продолжения, а прежний успех запуска не
// заменяется новым результатом.
func (n *sessionNode) retireExecution(ctx context.Context) error {
	n.mu.Lock()
	// f содержит известную точную попытку; c сохраняет исходную квитанцию; cause сохраняет
	// первоначальный отказ запуска.
	f, c, cause := n.fence, n.startReceipt, n.retirementFailure
	n.mu.Unlock()
	if f.CommandID == "" {
		// current содержит повторно прочитанную квитанцию для восстановления попытки; err
		// сообщает об отказе чтения.
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
	// err содержит результат точной компенсации исполнения Session без изменения прежнего успеха
	// запуска.
	_, _, err := n.manager.config.Core.CompensateSessionExecution(ctx, f, "manager:execution-stop:"+string(c.ID))
	if err != nil {
		return err
	}
	n.finishRetirement()
	n.manager.signal()
	return nil
}

// finishRetirement снимает признак ожидаемого долговечного завершения и удаляет уже очищенный узел
// только при совпадении его адреса в Manager.
func (n *sessionNode) finishRetirement() {
	n.mu.Lock()
	n.retirementNeeded = false
	// joined сохраняет признак уже выполненной очистки после снятия необходимости компенсации.
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

// retryRetirements собирает ограниченное число незавершённых компенсаций сессий и очисток
// завершившихся выходов, затем повторяет их без удержания общей блокировки Manager.
func (m *Manager) retryRetirements(ctx context.Context) {
	m.mu.Lock()
	// nodes собирает ограниченное число Session с незавершённой долговечной компенсацией.
	nodes := make([]*sessionNode, 0, m.config.MaxCommands)
	// outputs собирает ограниченное число вернувшихся работников выходов для повторной очистки.
	var outputs []*outputNode
	// node содержит очередную собственную Session при отборе продолжений.
	for _, node := range m.sessions {
		node.mu.Lock()
		// pending сохраняет необходимость долговечного завершения данного узла.
		pending := node.retirementNeeded
		// output содержит очередного отправителя, чей работник уже завершился либо ещё работает.
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
	// node содержит очередной отобранный узел для повторной точной компенсации.
	for _, node := range nodes {
		if ctx.Err() != nil {
			return
		}
		m.log(node.retireExecution(ctx), "retry_execution_retirement")
	}
	// output содержит очередного отобранного отправителя для повторной очистки.
	for _, output := range outputs {
		if ctx.Err() != nil {
			return
		}
		output.mu.Lock()
		// pending показывает, не завершена ли ещё очистка данного вернувшегося отправителя.
		pending := !output.joined
		output.mu.Unlock()
		if pending {
			m.log(output.cleanup(ctx, m), "retry_output_retirement")
		}
	}
}
