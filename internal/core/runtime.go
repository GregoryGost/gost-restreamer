package core

import (
	"context"
	"crypto/subtle"
	"errors"
	"sync/atomic"
	"time"
)

// ErrRuntimeCapability обозначает отсутствие общей RuntimeAuthority у UnitOfWork.
var ErrRuntimeCapability = errors.New("UnitOfWork не предоставляет общую RuntimeAuthority")

// RuntimeAuthorityProvider предоставляет общий допуск всех пользователей одного хранилища.
type RuntimeAuthorityProvider interface {

	// RuntimeAuthority возвращает единый реестр допусков всех Service данного владельца хранилища.
	RuntimeAuthority() *RuntimeAuthority
}

// RuntimeSessionFence не зависит от версии записи и ручной паузы.
// RuntimeSessionFence идентифицирует точное выполнение сессии без зависимости от ревизии записи и ручной
// паузы.
type RuntimeSessionFence struct {
	// SessionID идентифицирует родительскую сессию.
	SessionID ID
	// Incarnation идентифицирует открытие хранилища, которому принадлежит выполнение сессии.
	Incarnation string
	// Lifecycle различает поколения долговечного жизненного цикла сессии.
	Lifecycle uint64
}

// RuntimeAuthority упорядочивает записи БД, установку источников и допуск действий.
// Экземпляр принадлежит UnitOfWork, а не отдельному Service.
type RuntimeAuthority struct {
	// gate сериализует допуски, записи и сверку владельцев через единственный токен.
	gate chan struct{}
	// incarnation идентифицирует открытие хранилища, которому принадлежит общий реестр.
	incarnation string
	// closed запрещает выдачу новых допусков после закрытия.
	closed bool
	// leases хранит действующие допуски исполнения и публикации по локальному ID.
	leases map[ID]*RuntimeLease
	// publications хранит единственную текущую публикацию OBS каждого потока.
	publications map[ID]*RuntimeLease
	// limits хранит неизменяемые бюджеты оперативных наблюдений.
	limits ObservationLimits
	// observations хранит точные узлы наблюдений по локальным token.
	observations map[uint64]*observationNode
	// nextObservation содержит последний выданный идентификатор узла наблюдений.
	nextObservation uint64
	// preparations хранит владельцев подготовки по идентификатору поколения.
	preparations map[ID]*PreparationOwner
	// outputExecutionLimits хранит явные бюджеты физических резервов выходов.
	outputExecutionLimits OutputExecutionLimits
	// outputExecutions хранит занятые физические резервы по сессии и выходу.
	outputExecutions map[outputExecutionKey]*OutputExecutionReservation
	// outputExecutionBytes считает суммарный логический вес физических резервов.
	outputExecutionBytes uint64
	// nextOutputExecution содержит последний выданный локальный token физического резерва.
	nextOutputExecution uint64
	// dispatcher хранит единственный занятый резерв диспетчера.
	dispatcher *RuntimeDispatcher
	// telemetry хранит неизменяемые пределы включённой телеметрии либо nil до настройки.
	telemetry *TelemetryLimits
	// events хранит ограниченный журнал оперативных событий текущей эпохи.
	events *eventStream
	// system хранит единственного системного владельца GPU-снимка.
	system *SystemObserver
	// nextSystem содержит последний выданный token системного владельца GPU.
	nextSystem uint64
}

// NewRuntimeAuthority создаёт общий реестр допусков для одной incarnation с явно проверенными
// бюджетами наблюдений и физических исполнений выходов.
func NewRuntimeAuthority(incarnation string, limits ObservationLimits, executionLimits OutputExecutionLimits) (*RuntimeAuthority, error) {
	// err сообщает об ошибке проверки идентификатора владельца открытия хранилища.
	if err := validText("incarnation", incarnation); err != nil {
		return nil, err
	}
	// err сообщает об ошибке проверки явных бюджетов наблюдений.
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	// err сообщает об ошибке проверки явных бюджетов физических резервов.
	if err := executionLimits.Validate(); err != nil {
		return nil, err
	}
	// a содержит новый общий реестр с пустыми владельцами и одним токеном блокировки.
	a := &RuntimeAuthority{outputExecutionLimits: executionLimits, outputExecutions: make(map[outputExecutionKey]*OutputExecutionReservation), preparations: make(map[ID]*PreparationOwner), limits: limits, observations: make(map[uint64]*observationNode), gate: make(chan struct{}, 1), incarnation: incarnation, leases: make(map[ID]*RuntimeLease), publications: make(map[ID]*RuntimeLease)}
	a.gate <- struct{}{}
	return a, nil
}

// acquire ожидает общую блокировку RuntimeAuthority с учётом контекста и отклоняет закрытый реестр;
// при ошибке захваченный допуск освобождается.
func (a *RuntimeAuthority) acquire(ctx context.Context) error {
	// err содержит отмену контекста до ожидания общей блокировки.
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.gate:
	}
	// err содержит отмену контекста, наступившую до получения общей блокировки.
	if err := ctx.Err(); err != nil {
		a.release()
		return err
	}
	if a.closed {
		a.release()
		return ErrClosed
	}
	return nil
}

// release освобождает ранее захваченную общую блокировку RuntimeAuthority; повторное освобождение без
// захвата недопустимо.
func (a *RuntimeAuthority) release() { a.gate <- struct{}{} }

// GuardWrite вызывается только владельцем UnitOfWork. write завершает все
// транзакции до возврата; первый результат сообщает о возможном COMMIT.
// Сверка не заменяет результат записи и никогда не повторяет переданную функцию.
// reconcileTimeout ограничивает общую сверку после записи; её контекст сохраняет
// значения ctx, но не наследует его отмену и срок завершения.
func (a *RuntimeAuthority) GuardWrite(ctx context.Context, uow UnitOfWork, reconcileTimeout time.Duration, write func() (bool, error)) error {
	if uow == nil || reconcileTimeout <= 0 || write == nil {
		return ErrInvalid
	}
	// err сообщает об ошибке ожидания общей блокировки перед записью.
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	// uncertain показывает, мог ли COMMIT состояться и нужна ли последующая сверка владельцев.
	uncertain := true
	defer func() {
		// После записи запускает сверку только при возможном COMMIT, включая панику до получения результата;
		// ошибка сверки не подменяет результат write.

		if uncertain {
			a.reconcile(ctx, uow, reconcileTimeout)
		}
	}()
	// err сохраняет исходный результат записи независимо от последующей сверки.
	var err error
	uncertain, err = write()
	return err
}

// reconcile под удерживаемой общей блокировкой RuntimeAuthority сверяет владельцев с БД в отдельном
// ограниченном контексте после возможного COMMIT. Ошибка чтения отзывает допуски, свежесть которых не
// доказана.
func (a *RuntimeAuthority) reconcile(ctx context.Context, uow UnitOfWork, reconcileTimeout time.Duration) {
	// cancel освобождает отдельный ограниченный контекст послезаписной сверки.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reconcileTimeout)
	defer cancel()
	a.reconcilePreparations(ctx, uow)
	if len(a.leases) == 0 {
		return
	}
	// invalid собирает допуски, потерявшие долговечное разрешение.
	invalid := make([]*RuntimeLease, 0)
	// invalidObservations собирает наблюдателей выходов без успешной актуальной квитанции.
	invalidObservations := make([]*observationNode, 0)
	// err сообщает об ошибке согласованной послезаписной проверки владельцев.
	err := uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении собирает допуски и наблюдателей, утративших актуальность; неожиданная ошибка
		// чтения затем отзывает все допуски.

		// lease содержит очередной допуск, сверяемый с сохраняемым управлением.
		for _, lease := range a.leases {
			// err сообщает об ошибке проверки долговечного разрешения текущего допуска.
			if err := lease.authorized(ctx, r); err != nil {
				if !errors.Is(err, ErrStale) && !errors.Is(err, ErrNotFound) {
					return err
				}
				invalid = append(invalid, lease)
			}
		}
		// node содержит очередного наблюдателя выхода для проверки его квитанции.
		for _, node := range a.observations {
			if node.kind != observationOutput {
				continue
			}
			// c содержит квитанцию исполнения выхода, а err — результат её чтения.
			c, err := r.Runtime.ReceiptByID(ctx, node.execution.CommandID)
			if errors.Is(err, ErrNotFound) || err == nil && c.Status != CommandSucceeded {
				invalidObservations = append(invalidObservations, node)
			} else if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// Неподтверждённая свежесть закрывает допуск до новой успешной привязки.
		// lease содержит очередной допуск, отзываемый при недоказанной свежести БД.
		for _, lease := range a.leases {
			a.revoke(lease)
		}
		return
	}
	// node содержит наблюдателя выхода, который требуется удалить после сверки.
	for _, node := range invalidObservations {
		a.removeObservation(node)
	}
	// lease содержит допуск, который требуется отозвать после сверки.
	for _, lease := range invalid {
		a.revoke(lease)
	}
}

// Close синхронно отзывает допуск. Физические ресурсы закрывает их владелец.
// Функция отзыва должна менять только память и не захватывать RuntimeAuthority повторно.
func (a *RuntimeAuthority) Close() {
	<-a.gate
	defer a.release()
	if a.closed {
		return
	}
	a.closed = true
	if a.system != nil {
		a.system.closeLocked()
	}
	if a.dispatcher != nil {
		a.dispatcher.closeLocked()
	}
	// reservation содержит физический резерв, закрываемый для новых действий при остановке
	// authority.
	for _, reservation := range a.outputExecutions {
		reservation.sealLocked()
	}
	// o содержит владельца подготовки, отзываемого при остановке authority.
	for _, o := range a.preparations {
		a.revokePreparation(o)
	}
	// lease содержит допуск, отзываемый при остановке authority.
	for _, lease := range a.leases {
		a.revoke(lease)
	}
	if a.events != nil {
		close(a.events.changed)
	}
}

// RuntimeLease разрешает ограниченное изменение памяти под общим допуском.
// В Do и функциях установки/отзыва запрещены ввод-вывод, ожидания и повторный захват RuntimeAuthority.
type RuntimeLease struct {
	// authority связывает допуск с общей блокировкой и реестрами владельца хранилища.
	authority *RuntimeAuthority
	// id идентифицирует конкретный локальный допуск.
	id ID
	// streamID указывает поток публикации OBS для точного отзыва и замены.
	streamID ID
	// session фиксирует родительскую сессию, владельца открытия хранилища и Lifecycle допуска.
	session RuntimeSessionFence
	// execution фиксирует точную попытку команды; для публикации OBS остаётся нулевым.
	execution ExecutionFence
	// valid показывает, разрешены ли действия через этот точный допуск.
	valid atomic.Bool
	// done закрывается при логическом отзыве допуска.
	done chan struct{}
	// revoke содержит функцию отзыва установленного в памяти ресурса; вызывается не более одного
	// раза.
	revoke func()
}

// ID возвращает локальную идентичность выданного допуска.
func (l *RuntimeLease) ID() ID { return l.id }

// SessionFence возвращает владельца и Lifecycle сессии, к которой привязан допуск.
func (l *RuntimeLease) SessionFence() RuntimeSessionFence { return l.session }

// ExecutionFence возвращает точную попытку команды; у допуска OBS значение нулевое.
func (l *RuntimeLease) ExecutionFence() ExecutionFence { return l.execution }

// Valid сообщает о логическом допуске, не подтверждая существование или завершение физических
// ресурсов.
func (l *RuntimeLease) Valid() bool { return l != nil && l.valid.Load() }

// Done возвращает канал, закрываемый при отзыве допуска.
func (l *RuntimeLease) Done() <-chan struct{} { return l.done }

// Do выполняет изменение памяти под общей блокировкой RuntimeAuthority только пока допуск
// действителен; fn не должен выполнять ввод-вывод, ждать или повторно захватывать блокировку.
func (l *RuntimeLease) Do(ctx context.Context, fn func() error) error {
	if l == nil || fn == nil {
		return ErrInvalid
	}
	// err сообщает об ошибке ожидания общей блокировки для изменения памяти.
	if err := l.authority.acquire(ctx); err != nil {
		return err
	}
	defer l.authority.release()
	if !l.valid.Load() {
		return ErrStale
	}
	return fn()
}

// Revoke под общей блокировкой RuntimeAuthority отзывает допуск и связанные наблюдения; физические
// ресурсы остаются у их владельца.
func (l *RuntimeLease) Revoke(ctx context.Context) error {
	if l == nil {
		return ErrInvalid
	}
	// err сообщает об ошибке ожидания общей блокировки перед отзывом допуска.
	if err := l.authority.acquire(ctx); err != nil {
		return err
	}
	defer l.authority.release()
	l.authority.revoke(l)
	return nil
}

// revoke закрывает допуск связанных резервов и наблюдений, удаляет RuntimeLease из реестров и
// однократно вызывает его функцию отзыва под общей блокировкой RuntimeAuthority.
func (a *RuntimeAuthority) revoke(l *RuntimeLease) {
	// reservation содержит физический резерв, связанный с отзываемым допуском.
	for _, reservation := range a.outputExecutions {
		if reservation.lease == l {
			reservation.sealLocked()
		}
	}
	if !l.valid.Swap(false) {
		return
	}
	a.inputDisconnectedLocked(l)
	// n содержит узел наблюдений, связанный с отзываемым допуском.
	for _, n := range a.observations {
		if n.lease == l {
			a.removeObservation(n)
		}
	}
	delete(a.leases, l.id)
	if a.publications[l.streamID] == l {
		delete(a.publications, l.streamID)
	}
	close(l.done)
	if l.revoke != nil {
		l.revoke()
		l.revoke = nil
	}
}

// runtimeAuthority получает общую RuntimeAuthority от UnitOfWork и отклоняет отсутствующую возможность
// либо другую incarnation.
func (s *Service) runtimeAuthority() (*RuntimeAuthority, error) {
	// provider предоставляет общую RuntimeAuthority хранилища, а ok показывает наличие этой
	// возможности.
	provider, ok := s.uow.(RuntimeAuthorityProvider)
	if !ok || provider.RuntimeAuthority() == nil {
		return nil, ErrRuntimeCapability
	}
	// a содержит общий реестр хранилища для проверки совпадения incarnation.
	a := provider.RuntimeAuthority()
	if a.incarnation != s.incarnation {
		return nil, ErrStale
	}
	return a, nil
}

// ActivateOBS атомарно допускает источник по свежему состоянию Session. Для
// первоначальной проверки, разделённой сетевым Accept, нужен ActivateOBSFor.
// install атомарно меняет память; при ошибке прежний источник сохраняется.
// Возвращённый revoke отзывает только установленный им источник.
func (s *Service) ActivateOBS(ctx context.Context, streamID ID, key string, install func(*RuntimeLease) (revoke func(), err error)) (*RuntimeLease, error) {
	return s.activateOBS(ctx, nil, streamID, key, install)
}

// ActivateOBSFor повторно сверяет текущую Session с RuntimeSessionFence первоначальной проверки.
// При вызове после сетевого принятия публикации это не даёт источнику перейти в другой запуск.
func (s *Service) ActivateOBSFor(ctx context.Context, expected RuntimeSessionFence, streamID ID, key string, install func(*RuntimeLease) (revoke func(), err error)) (*RuntimeLease, error) {
	return s.activateOBS(ctx, &expected, streamID, key, install)
}

// activateOBS под общей блокировкой RuntimeAuthority повторно проверяет RUNNING-сессию, ключ и
// необязательный ожидаемый RuntimeSessionFence, затем устанавливает источник и отзывает прежний.
// Ошибка install закрывает новый допуск и вызывает полученную функцию отзыва.
func (s *Service) activateOBS(ctx context.Context, expected *RuntimeSessionFence, streamID ID, key string, install func(*RuntimeLease) (revoke func(), err error)) (*RuntimeLease, error) {
	if install == nil {
		return nil, ErrInvalid
	}
	// a содержит общую RuntimeAuthority, а err — причину невозможности её получить.
	a, err := s.runtimeAuthority()
	if err != nil {
		return nil, err
	}
	// id содержит новый локальный идентификатор допуска публикации OBS.
	id, err := newID()
	if err != nil {
		return nil, err
	}
	if err = a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	// session принимает текущую сессию для проверки состояния, ключа и ожидаемого владельца.
	var session Session
	err = s.uow.View(ctx, func(r Repositories) error {
		// Под общей блокировкой проверяет Session в состоянии RUNNING, непустой совпадающий ключ и
		// необязательный ожидаемый RuntimeSessionFence перед установкой OBS.

		// err передаёт результат чтения текущей сессии при проверке допуска OBS.
		var err error
		session, err = r.Runtime.ActiveSession(ctx, streamID)
		if err != nil {
			return err
		}
		if session.Incarnation != a.incarnation || session.State != SessionRunning || key == "" || subtle.ConstantTimeCompare([]byte(session.Snapshot.Stream.PublicationKey), []byte(key)) != 1 {
			return ErrNotReady
		}
		if expected != nil && *expected != (RuntimeSessionFence{session.ID, session.Incarnation, session.Lifecycle}) {
			return ErrStale
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	// lease содержит новый ещё не опубликованный допуск устанавливаемого OBS-источника.
	lease := &RuntimeLease{authority: a, id: id, streamID: streamID, session: RuntimeSessionFence{session.ID, session.Incarnation, session.Lifecycle}, done: make(chan struct{})}
	lease.valid.Store(true)
	// revoke отзывает только ресурс, установленный текущим вызовом install.
	revoke, err := install(lease)
	if err != nil {
		lease.valid.Store(false)
		close(lease.done)
		if revoke != nil {
			revoke()
		}
		return nil, err
	}
	lease.revoke = revoke
	// old содержит прежнюю публикацию потока, отзываемую после успешной установки новой.
	old := a.publications[streamID]
	a.leases[id] = lease
	a.publications[streamID] = lease
	a.inputConnectedLocked(lease)
	if old != nil {
		a.revoke(old)
	}
	return lease, nil
}

// BindExecutionOwner проверяет сохранённое разрешение на исполнение без изменения команд и БД.
// Допуск Session независим от OBS, а допуск выхода — от соседних выходов.
func (s *Service) BindExecutionOwner(ctx context.Context, fence ExecutionFence) (*RuntimeLease, error) {
	// a содержит общую RuntimeAuthority, а err — причину невозможности её получить.
	a, err := s.runtimeAuthority()
	if err != nil {
		return nil, err
	}
	// id содержит новый локальный идентификатор допуска исполнения.
	id, err := newID()
	if err != nil {
		return nil, err
	}
	if err = a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	fence = cloneExecutionFence(fence)
	// lease содержит новый допуск точной попытки до сверки долговечного разрешения.
	lease := &RuntimeLease{authority: a, id: id, session: RuntimeSessionFence{fence.SessionID, fence.Incarnation, fence.Lifecycle}, execution: fence, done: make(chan struct{})}
	if err = s.uow.View(ctx, func(r Repositories) error {
		// Под общей блокировкой сверяет сохранённое разрешение нового RuntimeLease без изменения команд и БД.
		return lease.authorized(ctx, r)
	}); err != nil {
		return nil, err
	}
	// existing содержит существующий допуск для возврата того же точного исполнения.
	for _, existing := range a.leases {
		if existing.execution == fence {
			return existing, nil
		}
	}
	lease.valid.Store(true)
	a.leases[id] = lease
	return lease, nil
}

// BindExecution возвращает допуск точного сохраняемого исполнения через BindExecutionOwner.
func (s *Service) BindExecution(ctx context.Context, fence ExecutionFence) (*RuntimeLease, error) {
	return s.BindExecutionOwner(ctx, fence)
}

// authorized сверяет допуск с сохраняемой сессией, квитанцией и при необходимости выходом; OBS требует
// RUNNING, а исполнитель — точную выданную или успешную попытку без отзыва управления.
func (l *RuntimeLease) authorized(ctx context.Context, r Repositories) error {
	// err сообщает об ошибке проверки отсутствия активной подготовки, несовместимой с исполнением.
	if err := preparationIdleForRuntime(ctx, r, l.authority.incarnation); err != nil {
		return err
	}
	// session содержит родительский запуск допуска, а err — результат его чтения.
	session, err := r.Runtime.Session(ctx, l.session.SessionID)
	if err != nil {
		return err
	}
	if session.Incarnation != l.authority.incarnation || (RuntimeSessionFence{session.ID, session.Incarnation, session.Lifecycle}) != l.session {
		return ErrStale
	}
	if l.execution.CommandID == "" {
		if session.State != SessionRunning || session.StreamID != l.streamID {
			return ErrStale
		}
		return nil
	}
	// c содержит квитанцию точной команды для проверки статуса и попытки.
	c, err := r.Runtime.ReceiptByID(ctx, l.execution.CommandID)
	if err != nil {
		return err
	}
	if executionFence(c) != l.execution || (c.Status != CommandIssued && c.Status != CommandSucceeded) || c.Attempt == 0 {
		return ErrStale
	}
	if c.OutputID == "" {
		if c.Kind != CommandStartSession || !session.Issued || (session.State != SessionStarting && session.State != SessionRunning) {
			return ErrStale
		}
		return nil
	}
	if (c.Kind != CommandStartOutput && c.Kind != CommandAddOutput) || session.State != SessionRunning {
		return ErrStale
	}
	// o содержит выход для проверки прикрепления, намерения, поколения и очистки.
	o, err := r.Runtime.Output(ctx, c.OutputID)
	if err != nil {
		return err
	}
	if o.SessionID != session.ID || o.Generation != c.OutputGeneration || o.Attempt != c.Attempt || !o.Attached || !o.DesiredRunning || !o.Issued || o.CleanupPending {
		return ErrStale
	}
	return nil
}
