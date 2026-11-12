package core

import (
	"context"
	"errors"
	"math"
	"reflect"
)

// ErrOutputExecutionLimit обозначает превышение явного бюджета физических исполнений выходов.
var ErrOutputExecutionLimit = errors.New("исчерпан бюджет исполнений выходов")

// OutputExecutionLimits ограничивает число исполнений и логический вес их реестра.
// Вес записи равен 512 байтам плюс длины удерживаемых строк ExecutionFence; это не размер кучи Go.
type OutputExecutionLimits struct {
	// MaxReservations ограничивает число одновременно удерживаемых физических резервов.
	MaxReservations int
	// MaxIdentifierBytes ограничивает длину каждого идентификатора ExecutionFence в байтах.
	MaxIdentifierBytes uint64
	// MaxBytes ограничивает общий логический вес физических резервов без вытеснения.
	MaxBytes uint64
}

// Validate проверяет положительные бюджеты резерваций, идентификаторов и логических байтов с защитой
// расчётов веса от переполнения.
func (l OutputExecutionLimits) Validate() error {
	if l.MaxReservations <= 0 || l.MaxIdentifierBytes == 0 || l.MaxBytes < 512 || l.MaxIdentifierBytes > l.MaxBytes || l.MaxIdentifierBytes > (math.MaxUint64-512)/4 || uint64(l.MaxReservations) > math.MaxUint64/(512+4*l.MaxIdentifierBytes) {
		return ErrInvalid
	}
	return nil
}

// outputExecutionKey обеспечивает единственность физического резерва по сессии и выходу.
type outputExecutionKey struct { /* session идентифицирует сессию, а output — отдельный выход с единственным физическим резервом. */
	session, output ID
}

// OutputOperationKind выбирает один из трёх независимых фиксированных допусков.
type OutputOperationKind uint8

const (
	// OutputOperationEstablishment выбирает фиксированный слот установления соединения.
	OutputOperationEstablishment OutputOperationKind = iota
	// OutputOperationRead выбирает фиксированный слот чтения соединения.
	OutputOperationRead
	// OutputOperationWrite выбирает фиксированный слот записи медиаданных.
	OutputOperationWrite
)

// OutputExecutionReservation удерживает физическое исполнение до подтверждённого освобождения.
// Все поля защищены общей блокировкой; Done можно ожидать вне неё.
type OutputExecutionReservation struct {
	// authority связывает физический резерв с общей блокировкой и реестром Store.
	authority *RuntimeAuthority
	// token различает точное локальное владение резервом.
	token uint64
	// fence фиксирует точную долговечную попытку исполнения выхода.
	fence ExecutionFence
	// lease связывает резерв с логическим допуском исполнения.
	lease *RuntimeLease
	// observer ссылается на точного активированного наблюдателя выхода.
	observer *observationNode
	// weight содержит учтённый логический вес резерва.
	weight uint64
	// active отмечает привязку наблюдателя, sealed запрещает новые действия, releaseEligible
	// подтверждает долговечную непригодность прежней попытки, а released отмечает снятие физического
	// резерва.
	active, sealed, releaseEligible, released bool
	// slots хранит не более одной незавершённой операции каждого из трёх видов.
	slots [3]*OutputOperation
	// done закрывается при запрете новых действий, до возможного завершения занятых слотов.
	done chan struct{}
}

// OutputOperation соответствует одному допущенному внешнему действию.
// Владелец вызывает Finish только после фактического завершения действия и его ресурсов.
type OutputOperation struct {
	// reservation связывает внешнюю операцию с удерживаемым физическим исполнением.
	reservation *OutputExecutionReservation
	// kind выбирает собственный слот операции для точного завершения.
	kind OutputOperationKind
}

// ExecutionFence возвращает точную попытку физического исполнения; nil-резерв даёт нулевой
// ExecutionFence.
func (r *OutputExecutionReservation) ExecutionFence() ExecutionFence {
	if r == nil {
		return ExecutionFence{}
	}
	return r.fence
}

// Done сообщает о закрытии допуска. Закрытие канала может предшествовать
// завершению уже допущенных операций и освобождению резерва.
func (r *OutputExecutionReservation) Done() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.done
}

// key строит ключ единственности физического исполнения по SessionID и OutputID.
func (r *OutputExecutionReservation) key() outputExecutionKey {
	return outputExecutionKey{r.fence.SessionID, r.fence.OutputID}
}

// currentLocked проверяет совпадение записи и идентификатора с текущим реестром под общей блокировкой
// RuntimeAuthority.
func (r *OutputExecutionReservation) currentLocked() bool {
	// current содержит зарегистрированный сейчас резерв той же сессии и выхода.
	current := r.authority.outputExecutions[r.key()]
	return !r.released && current == r && current.token == r.token
}

// sealLocked однократно запрещает новые действия и закрывает Done под общей блокировкой
// RuntimeAuthority, сохраняя занятые слоты и резерв.
func (r *OutputExecutionReservation) sealLocked() {
	if !r.sealed {
		r.sealed = true
		close(r.done)
	}
}

// Seal закрывает допуск без ввода-вывода, ожидания работников и освобождения удержания.
func (r *OutputExecutionReservation) Seal() {
	if r == nil || r.authority == nil {
		return
	}
	// a предоставляет общую блокировку закрытия новых действий.
	a := r.authority
	<-a.gate
	defer a.release()
	if r.currentLocked() {
		r.sealLocked()
	}
}

// Activate однократно связывает подготовку с точным текущим наблюдателем того же RuntimeLease.
func (r *OutputExecutionReservation) Activate(ctx context.Context, observer *OutputObserver) error {
	if r == nil || r.authority == nil || observer == nil || observer.node == nil {
		return ErrInvalid
	}
	// a предоставляет общую блокировку активации физического резерва.
	a := r.authority
	// err сообщает об ошибке ожидания общей блокировки перед активацией.
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	// n содержит точный узел переданного наблюдателя выхода.
	n := observer.node
	if !r.currentLocked() || r.sealed || !r.lease.valid.Load() || n.authority != a || n.kind != observationOutput || n.execution != r.fence || n.lease != r.lease || !n.valid.Load() || a.observations[n.token] != n {
		return ErrStale
	}
	if r.active {
		return ErrConflict
	}
	r.observer = n
	r.active = true
	return nil
}

// Admit резервирует фиксированный слот непосредственно перед внешней операцией.
func (r *OutputExecutionReservation) Admit(ctx context.Context, kind OutputOperationKind) (*OutputOperation, error) {
	if r == nil || r.authority == nil || kind > OutputOperationWrite {
		return nil, ErrInvalid
	}
	// a предоставляет общую блокировку допуска внешней операции.
	a := r.authority
	// err сообщает об ошибке ожидания общей блокировки перед занятием слота.
	if err := a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	if !r.currentLocked() || r.sealed || !r.lease.valid.Load() {
		return nil, ErrStale
	}
	if !r.active {
		return nil, ErrNotReady
	}
	// n содержит привязанного наблюдателя для проверки его текущего допуска.
	n := r.observer
	if n == nil || !n.valid.Load() || a.observations[n.token] != n || n.lease != r.lease {
		r.sealLocked()
		return nil, ErrStale
	}
	if r.slots[kind] != nil {
		return nil, ErrInUse
	}
	// operation содержит новый допуск внешнего действия, занимающий выбранный слот.
	operation := &OutputOperation{reservation: r, kind: kind}
	r.slots[kind] = operation
	return operation, nil
}

// Finish идемпотентно завершает только собственный слот, включая после закрытия RuntimeAuthority.
func (o *OutputOperation) Finish() {
	if o == nil || o.reservation == nil {
		return
	}
	// r содержит физический резерв завершаемой операции.
	r := o.reservation
	// a предоставляет общую блокировку освобождения собственного слота.
	a := r.authority
	<-a.gate
	defer a.release()
	if r.currentLocked() && r.slots[o.kind] == o {
		r.slots[o.kind] = nil
	}
}

// ReleaseJoined вызывается владельцем только после фактического завершения всех работников,
// подписки и поздних подключений. Метод не ждёт их и не выполняет SQL; ошибка сохраняет удержание.
func (r *OutputExecutionReservation) ReleaseJoined() error {
	if r == nil || r.authority == nil {
		return ErrInvalid
	}
	// a предоставляет общую блокировку проверки и освобождения физического резерва.
	a := r.authority
	<-a.gate
	defer a.release()
	if r.released {
		return nil
	}
	if !r.currentLocked() {
		return ErrStale
	}
	if !r.sealed || (!r.releaseEligible && !a.closed) {
		return ErrNotReady
	}
	// slot содержит очередную незавершённую внешнюю операцию, препятствующую освобождению.
	for _, slot := range r.slots {
		if slot != nil {
			return ErrNotReady
		}
	}
	delete(a.outputExecutions, r.key())
	a.outputExecutionBytes -= r.weight
	r.released = true
	r.observer = nil
	r.lease = nil
	return nil
}

// ReserveOutputExecution под общей блокировкой проверяет действующий RuntimeLease и полный
// сохранённый OutputSnapshot, затем резервирует единственное исполнение пары SessionID/OutputID.
// Уже зарезервированная пара даёт ErrInUse, исчерпание бюджета реестра — ErrOutputExecutionLimit. Полученный резерв
// сначала связывают с наблюдателем через Activate; он удерживает пару до ReleaseJoined, включая
// после закрытия допуска. Резервирование не запускает внешние действия и не выполняет сетевой обмен.
func (s *Service) ReserveOutputExecution(ctx context.Context, lease *RuntimeLease, snapshot OutputSnapshot) (*OutputExecutionReservation, error) {
	// a содержит общую RuntimeAuthority, а err — причину невозможности её получить.
	a, err := s.runtimeAuthority()
	if err != nil {
		return nil, err
	}
	if lease == nil || lease.authority != a || lease.execution.OutputID == "" {
		return nil, ErrInvalid
	}
	if err = lease.execution.Validate(); err != nil {
		return nil, err
	}
	if err = a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	if !lease.valid.Load() || a.leases[lease.id] != lease {
		return nil, ErrStale
	}
	// f фиксирует точную попытку исполнения из логического допуска.
	f := lease.execution
	// key задаёт единственный слот физического исполнения этой пары Session/Output.
	key := outputExecutionKey{f.SessionID, f.OutputID}
	if a.outputExecutions[key] != nil {
		return nil, ErrInUse
	}
	// weight накапливает логический вес резерва вместе с длинами удерживаемых идентификаторов.
	weight := uint64(512)
	// value содержит очередную строку идентичности исполнения для проверки длины и бюджета.
	for _, value := range []string{string(f.CommandID), string(f.SessionID), string(f.OutputID), f.Incarnation} {
		// n содержит длину текущего удерживаемого идентификатора в байтах.
		n := uint64(len(value))
		if n > a.outputExecutionLimits.MaxIdentifierBytes || n > math.MaxUint64-weight {
			return nil, ErrOutputExecutionLimit
		}
		weight += n
	}
	if len(a.outputExecutions) >= a.outputExecutionLimits.MaxReservations || weight > a.outputExecutionLimits.MaxBytes-a.outputExecutionBytes {
		return nil, ErrOutputExecutionLimit
	}
	if a.nextOutputExecution == math.MaxUint64 {
		return nil, ErrConflict
	}
	err = s.uow.View(ctx, func(repo Repositories) error {
		// Под общей блокировкой сверяет сохранённый допуск RuntimeLease и полное совпадение OutputSnapshot
		// перед резервированием физического исполнения.

		// e сообщает об ошибке повторной проверки долговечного разрешения исполнения.
		if e := lease.authorized(ctx, repo); e != nil {
			return e
		}
		// output содержит сохраняемый снимок выхода, а e — результат его чтения.
		output, e := repo.Runtime.Output(ctx, f.OutputID)
		if e != nil {
			return e
		}
		if output.ID != f.OutputID || output.SessionID != f.SessionID || !reflect.DeepEqual(output.Snapshot, snapshot) {
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
	a.nextOutputExecution++
	// r содержит новый физический резерв с точным token и ExecutionFence.
	r := &OutputExecutionReservation{authority: a, token: a.nextOutputExecution, fence: f, lease: lease, weight: weight, done: make(chan struct{})}
	a.outputExecutions[key] = r
	a.outputExecutionBytes += weight
	return r, nil
}

// ConfirmOutputExecutionResolution разрешает освобождение лишь после успешного
// согласованного чтения, доказавшего долговечную непригодность прежнего ExecutionFence.
func (s *Service) ConfirmOutputExecutionResolution(ctx context.Context, reservation *OutputExecutionReservation) error {
	// a содержит общую RuntimeAuthority, а err — причину невозможности её получить.
	a, err := s.runtimeAuthority()
	if err != nil {
		return err
	}
	if reservation == nil || reservation.authority != a {
		return ErrInvalid
	}
	if err = a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if !reservation.currentLocked() {
		return ErrStale
	}
	// f фиксирует точную прежнюю попытку, пригодность которой проверяется в БД.
	f := reservation.fence
	// eligible показывает, сохраняется ли долговечное разрешение прежней попытки; true запрещает
	// освобождение.
	eligible := false
	err = s.uow.View(ctx, func(repo Repositories) error {
		// В согласованном чтении проверяет Session, квитанцию и выход старого ExecutionFence; любая
		// неожиданная ошибка запрещает разрешить освобождение резерва.

		// session содержит родительский запуск, а se — результат его чтения.
		session, se := repo.Runtime.Session(ctx, f.SessionID)
		// receipt содержит квитанцию прежней команды, а ce — результат её чтения.
		receipt, ce := repo.Runtime.ReceiptByID(ctx, f.CommandID)
		// output содержит сохраняемый выход, а oe — результат его чтения.
		output, oe := repo.Runtime.Output(ctx, f.OutputID)
		// e содержит очередной результат чтения владельца, квитанции или выхода для запрета
		// освобождения при неожиданном отказе.
		for _, e := range []error{se, ce, oe} {
			if e != nil && !errors.Is(e, ErrNotFound) {
				return e
			}
		}
		eligible = se == nil && ce == nil && oe == nil && session.ID == f.SessionID && session.Incarnation == f.Incarnation && session.Lifecycle == f.Lifecycle && session.State == SessionRunning && executionFence(receipt) == f && (receipt.Kind == CommandStartOutput || receipt.Kind == CommandAddOutput) && (receipt.Status == CommandIssued || receipt.Status == CommandSucceeded) && receipt.Attempt != 0 && output.ID == f.OutputID && output.SessionID == f.SessionID && output.Generation == f.OutputGeneration && output.Attempt == f.Attempt && output.Attached && output.DesiredRunning && output.Issued && !output.CleanupPending
		return nil
	})
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if eligible {
		return ErrNotReady
	}
	reservation.releaseEligible = true
	reservation.sealLocked()
	return nil
}
