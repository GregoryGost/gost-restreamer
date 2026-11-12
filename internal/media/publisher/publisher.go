// Package publisher передаёт независимые выходы группы через RTMP и RTMPS.
package publisher

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
)

var (
	// ErrOptions обозначает некорректные бюджеты, сроки или запрещённые входные обработчики
	// транспорта Publisher.
	ErrOptions = errors.New("некорректные настройки Publisher")
	// ErrCleanupPending обозначает истечение ожидания до фактического завершения Publisher.
	ErrCleanupPending = errors.New("завершение Publisher ещё не подтверждено")
)

// Options задаёт явные бюджеты и положительные интервалы восстановления.
// Admit, OnWrite и OnACKAnomaly транспорта принадлежат Publisher и должны быть nil на входе.
type Options struct {
	// Queue ограничивает очередь подписки данного независимого выхода.
	Queue media.QueueLimits
	// RetryInitial задаёт начальную паузу сетевого повтора; RetryMax ограничивает её удвоение;
	// ResubscribeDelay задаёт паузу перед новой подпиской после истечения синхронизации.
	RetryInitial, RetryMax, ResubscribeDelay time.Duration
	// Transport задаёт явные ограничения и настройки исходящего RTMP/RTMPS-транспорта.
	Transport rtmp.OutboundOptions
	// Resolver преобразует сохранённое назначение и формат в цель транспорта; nil выбирает
	// ResolveOutbound.
	Resolver rtmp.OutboundResolver
	// Logger принимает диагностические события данного выхода с его сохранённой идентичностью.
	Logger *slog.Logger
}

// Validate проверяет бюджеты и интервалы восстановления и запрещает входные обработчики транспорта,
// которыми владеет Publisher.
func (o Options) Validate() error {
	if o.Queue.Validate() != nil || o.Transport.Validate() != nil || o.RetryInitial <= 0 || o.RetryMax < o.RetryInitial || o.ResubscribeDelay <= 0 || o.Transport.Admit != nil || o.Transport.OnWrite != nil || o.Transport.OnACKAnomaly != nil {
		return ErrOptions
	}
	return nil
}

// Result описывает завершение ресурсов данного Publisher. Ресурсы группы и
// родительского запуска вызывающий код завершает отдельно.
type Result struct {
	// Err сохраняет итоговую причину отказа Publisher, если завершение не было обычной остановкой.
	Err error
	// NetworkJoined подтверждает завершение собственных сетевых ресурсов и закрытия допуска
	// Publisher.
	NetworkJoined bool
}

// connection задаёт необходимый Publisher порт передачи медиаданных, наблюдения и подтверждённого
// закрытия транспорта.
type connection interface {

	// WriteHeaders проверяет и сохраняет согласованную пару конфигураций. Запись метаданных и заголовков в
	// сеть откладывается до следующего проверенного независимого видеопакета.
	WriteHeaders(context.Context, media.CodecConfig, media.CodecConfig) error

	// WriteDelivery передаёт актуальную выдачу подписки и возвращает число полезных байтов полного
	// медиасообщения; ErrStaleSource разрешает восстановление поколения в том же соединении.
	WriteDelivery(context.Context, media.Delivery) (rtmp.OutboundWriteResult, error)

	// Close запрашивает закрытие сетевых ресурсов; Publisher отдельно вызывает Wait до подтверждения
	// завершения.
	Close() error

	// Wait ожидает фактическое завершение транспорта в пределах контекста; Publisher сохраняет владение до
	// возврата этого вызова.
	Wait(context.Context) error

	// Done сообщает завершение работы соединения и прерывает ожидание следующей выдачи подписки.
	Done() <-chan struct{}

	// Err возвращает причину завершения соединения, используемую при прекращении чтения подписки.
	Err() error

	// Snapshot возвращает согласованные накопления и логический резерв транспорта для измерений Publisher.
	Snapshot() rtmp.OutboundSnapshot
}

// execution задаёт активацию, допуск сетевых операций, отзыв и отдельное освобождение точного исполнения
// выхода.
type execution interface {

	// Активирует точное исполнение с наблюдателем после успешной долговечной команды; ошибка оставляет
	// освобождение резервации вызывающему.
	activate(context.Context, *core.OutputObserver) error

	// Выдаёт допуск одной внешней операции и функцию её завершения; после отзыва новые операции
	// отвергаются.
	admit(context.Context, rtmp.OutboundOperation) (func(), error)

	// Запрещает новые внешние операции; Publisher учитывает завершение этого вызова отдельно от
	// немедленной отмены сети.
	seal()

	// Возвращает сигнал отзыва исполнения для остановки активного сетевого цикла.
	done() <-chan struct{}

	// Освобождает уже присоединённое исполнение только после отдельного разрешения его долговечного
	// владельца.
	release() error
}

// observation публикует оперативное состояние точного выхода.
type observation interface {

	// Observe публикует оперативное состояние точного выхода; ошибка останавливает соответствующий цикл
	// Publisher.
	Observe(context.Context, core.OutputObservation) error
}

// terminalObservation дополняет наблюдателя отдельной публикацией окончательного состояния до завершения
// транспорта.
type terminalObservation interface {

	// ObserveTerminal публикует окончательное состояние выхода до завершения транспорта, сохраняя
	// отдельное управление сроком наблюдателя.
	ObserveTerminal(context.Context, core.OutputObservation) error
}

// coreExecution связывает внутренний порт исполнения Publisher с резервацией доменного сервиса.
type coreExecution struct {
	// reservation содержит точную резервацию исполнения для активации, допуска операций и
	// подтверждения завершения.
	reservation *core.OutputExecutionReservation
}

// Активирует точную резервацию исполнения с переданным наблюдателем выхода.
func (e coreExecution) activate(ctx context.Context, o *core.OutputObserver) error {
	return e.reservation.Activate(ctx, o)
}

// Запрещает новые операции через резервацию исполнения.
func (e coreExecution) seal() { e.reservation.Seal() }

// Возвращает канал отзыва резервации исполнения.
func (e coreExecution) done() <-chan struct{} { return e.reservation.Done() }

// Освобождает завершённую резервацию после отдельного разрешения долговечного владельца.
func (e coreExecution) release() error { return e.reservation.ReleaseJoined() }

// Преобразует вид сетевой операции в допуск резервации и возвращает функцию завершения; неизвестный
// вид отвергается.
func (e coreExecution) admit(ctx context.Context, kind rtmp.OutboundOperation) (func(), error) {
	// k задаёт доменный вид допуска, соответствующий проверенной сетевой операции.
	var k core.OutputOperationKind
	switch kind {
	case rtmp.OutboundEstablishment:
		k = core.OutputOperationEstablishment
	case rtmp.OutboundRead:
		k = core.OutputOperationRead
	case rtmp.OutboundWrite:
		k = core.OutputOperationWrite
	default:
		return nil, core.ErrInvalid
	}
	// op содержит выданный допуск сетевой операции; err сообщает об отказе его получения.
	op, err := e.reservation.Admit(ctx, k)
	if err != nil {
		return nil, err
	}
	return op.Finish, nil
}

// Publisher владеет одной подпиской и последовательными сетевыми попытками.
// Вызывающий код сохраняет владение наблюдателем, ссылкой на группу и долговечными командами.
type Publisher struct {
	// mu защищает жизненный цикл Publisher, активную подписку, соединение и основные измерения.
	mu sync.Mutex
	// options хранит проверенные бюджеты, интервалы восстановления и настройки транспорта данного
	// выхода.
	options Options
	// router выдаёт подписки на текущие поколения медиаданных группы.
	router *media.Router
	// target содержит проверенное назначение исходящего RTMP/RTMPS-соединения.
	target rtmp.OutboundTarget
	// execution управляет допуском сетевых операций и отзывом точного исполнения выхода.
	execution execution
	// reservation позволяет вызывающему отдельно разрешить освобождение завершённого исполнения.
	reservation *core.OutputExecutionReservation
	// dial открывает очередное исходящее соединение; подменяется в компонентных тестах.
	dial func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error)
	// observer публикует состояние активированного выхода.
	observer observation
	// cancel отменяет сетевой цикл и незавершённую активацию Publisher.
	cancel context.CancelFunc
	// active отмечает запущенный цикл выхода; stopping запрещает продолжение; revoked фиксирует
	// отзыв исполнения; joined подтверждает завершение собственных ресурсов.
	active, stopping, revoked, joined bool
	// activating отмечает незавершённый вызов активации, который должен вернуться до запуска
	// завершения Abort.
	activating bool
	// abortStarted исключает повторный запуск завершения неактивированного выхода.
	abortStarted bool
	// sealOnce единожды запускает закрытие допуска исполнения.
	sealOnce sync.Once
	// sealDone закрывается после фактического возврата запрета новых операций исполнения.
	sealDone chan struct{}
	// done закрывается после сохранения окончательного результата Publisher.
	done chan struct{}
	// result хранит итог после завершения, оставаясь нулевым до этого момента.
	result Result
	// state хранит последнее подтверждённое оперативное состояние выхода.
	state core.OutputState
	// failure хранит ограниченную причину последнего подтверждённого состояния.
	failure FailureReason
	// sub содержит текущую подписку, чьи окончательные потери переносятся перед закрытием.
	sub *media.Subscription
	// conn содержит активный транспорт для снимка его логического резерва.
	conn connection
	// queueDrops накапливает окончательные потери уже закрытых подписок.
	queueDrops media.SubscriptionDrops
	// metrics хранит накопления передачи и логический вес удержанных медиаданных.
	metrics Snapshot
	// hadConnection отмечает наличие предыдущего успешного подключения для подсчёта переподключений.
	hadConnection bool
	// writeMu защищает счётчики записанных байтов и аномалий ACK отдельно от основных измерений.
	writeMu sync.Mutex
	// writtenBytes накапливает незашифрованные RTMP-байты, принятые транспортом, включая частичные
	// записи.
	writtenBytes uint64
	// writeSaturated отмечает насыщение счётчиков, изменяемых под writeMu.
	writeSaturated bool
	// lastWrite хранит время последней положительной записи транспортом.
	lastWrite time.Time
	// ackAheadEventsTotal считает наблюдения ACK, опережающих фактически отправленные байты.
	ackAheadEventsTotal uint64
	// ackAheadBytesLast хранит размер последнего наблюдённого опережения ACK в байтах.
	ackAheadBytesLast uint64
	// ackAheadBytesMax хранит наибольшее наблюдённое опережение ACK в байтах.
	ackAheadBytesMax uint64
	// ackLastAnomalyAt хранит время последней аномалии ACK.
	ackLastAnomalyAt time.Time
}

// Prepare проверяет неизменяемый снимок и резервирует исполнение без сетевых операций.
// Контекст запроса не сохраняется; срок активного выхода задаёт Activate.
func Prepare(ctx context.Context, service *core.Service, owner *core.RuntimeLease, snapshot core.OutputSnapshot, router *media.Router, options Options) (*Publisher, error) {
	if service == nil || owner == nil || router == nil {
		return nil, core.ErrInvalid
	}
	// err сообщает об отказе проверки настроек очереди, транспорта и повторов.
	if err := options.Validate(); err != nil {
		return nil, err
	}
	// err сообщает об отказе проверки неизменяемого снимка выхода.
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	// resolver выбирает переданный преобразователь назначения либо стандартный ResolveOutbound.
	resolver := options.Resolver
	if resolver == nil {
		resolver = rtmp.ResolveOutbound
	}
	// target содержит преобразованную цель публикации; err сохраняет результаты преобразования,
	// проверки цели и резервации исполнения.
	target, err := resolver(snapshot.Destination.Endpoint, snapshot.Destination.StreamKey, snapshot.Profile.Profile.Format)
	if err != nil {
		return nil, err
	}
	if err = target.Validate(snapshot.Profile.Profile.Format, options.Transport.Limits); err != nil {
		return nil, err
	}
	if options.Transport.TLSConfig != nil {
		options.Transport.TLSConfig = options.Transport.TLSConfig.Clone()
		if options.Transport.TLSConfig.RootCAs != nil {
			options.Transport.TLSConfig.RootCAs = options.Transport.TLSConfig.RootCAs.Clone()
		}
	}
	// reservation содержит выданную резервацию точного исполнения выхода до сетевой активации.
	reservation, err := service.ReserveOutputExecution(ctx, owner, snapshot)
	if err != nil {
		return nil, err
	}
	// Полный снимок артефакта не удерживается после проверки точного исполнения.
	options.Resolver = nil
	if options.Logger != nil {
		options.Logger = options.Logger.With("component", "publisher", "session_id", owner.SessionFence().SessionID, "execution", owner.ExecutionFence(), "destination_id", snapshot.Destination.ID, "endpoint", snapshot.Destination.Endpoint, "stream_key", snapshot.Destination.StreamKey)
	}
	// p содержит подготовленный Publisher с резервацией и ещё не открытой сетью.
	p := &Publisher{options: options, router: router, target: target, reservation: reservation, execution: coreExecution{reservation}, done: make(chan struct{}), sealDone: make(chan struct{}), state: core.OutputStopped}
	p.dial = func(ctx context.Context, t rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
		// Открывает исходящее RTMP/RTMPS-соединение через DialOutbound с точной целью и настройками текущей
		// попытки.

		return rtmp.DialOutbound(ctx, t, o)
	}
	return p, nil
}

// Activate однократно открывает сеть после успешного Complete с точным OutputObserver.
func (p *Publisher) Activate(ctx context.Context, observer *core.OutputObserver) error {
	if ctx == nil || observer == nil {
		return core.ErrInvalid
	}
	// err содержит причину отмены контекста до начала Activate.
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	if p.activating || p.active || p.stopping || p.joined {
		p.mu.Unlock()
		return core.ErrConflict
	}
	// runCtx задаёт отменяемый срок активного выхода; cancel позволяет Stop прервать ожидание
	// активации и сеть.
	runCtx, cancel := context.WithCancel(ctx)
	p.activating, p.cancel = true, cancel
	p.mu.Unlock()

	// Общая блокировка может ждать управляющую операцию; Stop должен отменять
	// этот контекст, не ожидая окончания её захвата.
	// err содержит результат активации точного исполнения с наблюдателем.
	err := p.execution.activate(runCtx, observer)
	p.mu.Lock()
	p.activating = false
	if err != nil {
		cancel()
		p.cancel = nil
		// startAbort показывает, требуется ли единожды завершить неактивированный Publisher
		// после отказа Activate.
		startAbort := p.startAbortLocked()
		p.mu.Unlock()
		if startAbort {
			go p.finish(nil)
		}
		return err
	}
	// Успешный допуск не возобновляет выход, если Stop уже победил.
	if p.stopping || runCtx.Err() != nil {
		p.stopping = true
		cancel()
		err = runCtx.Err()
		// startAbort показывает, требуется ли завершить Publisher, если Stop победил успешную
		// активацию.
		startAbort := p.startAbortLocked()
		p.mu.Unlock()
		if startAbort {
			go p.finish(nil)
		}
		return err
	}
	p.active, p.observer = true, observer
	go p.run(runCtx)
	p.mu.Unlock()
	return nil
}

// Reservation позволяет вызывающему коду отдельно подтвердить долговечное разрешение ExecutionFence.
func (p *Publisher) Reservation() *core.OutputExecutionReservation { return p.reservation }

// Done возвращает канал фактического завершения Publisher, включая закрытие допуска и транспорта.
func (p *Publisher) Done() <-chan struct{} { return p.done }

// Stop запрещает новые операции, отменяет сеть и ждёт фактическое завершение.
func (p *Publisher) Stop(ctx context.Context) error {
	p.mu.Lock()
	p.stopping = true
	// cancel сохраняет функцию отмены для вызова вне основной блокировки Publisher.
	cancel := p.cancel
	// startAbort показывает, должен ли Stop запустить завершение ещё не активированного выхода.
	startAbort := p.startAbortLocked()
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	p.requestSeal()
	if startAbort {
		go p.finish(nil)
	}
	// err содержит результат ограниченного ожидания фактического завершения после Stop.
	_, err := p.Wait(ctx)
	return err
}

// startAbortLocked передаёт завершение единственному работнику только после
// возврата незавершённой активации. Вызывающий код удерживает p.mu.
func (p *Publisher) startAbortLocked() bool {
	if !p.stopping || p.active || p.activating || p.abortStarted || p.joined {
		return false
	}
	p.abortStarted = true
	return true
}

// Abort завершает подготовленный выход; до Activate сеть не открывается.
func (p *Publisher) Abort(ctx context.Context) error { return p.Stop(ctx) }

// Wait не отменяет исполнение и допускает повторное ожидание после истечения срока.
func (p *Publisher) Wait(ctx context.Context) (Result, error) {
	select {
	case <-p.done:
		return p.Result(), nil
	default:
	}
	select {
	case <-p.done:
		return p.Result(), nil
	case <-ctx.Done():
		return p.Result(), errors.Join(ErrCleanupPending, ctx.Err())
	}
}

// Result возвращает сохранённый результат завершения под блокировкой; до завершения поля остаются
// нулевыми.
func (p *Publisher) Result() Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.result
}

// ReleaseJoined освобождает только доказанно завершённое и разрешённое исполнение.
// Итог окончательного отказа не выдаёт разрешение автоматически.
func (p *Publisher) ReleaseJoined() error {
	p.mu.Lock()
	// joined сохраняет признак завершения Publisher для проверки допустимости отдельного
	// освобождения резервации.
	joined := p.joined
	p.mu.Unlock()
	if !joined {
		return ErrCleanupPending
	}
	return p.execution.release()
}

// После завершения Seal единожды публикует итог и закрывает done; отзыв владельца без иной ошибки
// превращается в ErrStale.
func (p *Publisher) finish(err error) {
	p.requestSeal()
	<-p.sealDone
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.joined {
		return
	}
	if p.revoked && err == nil {
		err = core.ErrStale
	}
	p.joined = true
	p.result = Result{Err: err, NetworkJoined: true}
	if err == nil {
		p.state, p.failure = core.OutputStopped, FailureNone
	} else {
		p.state = core.OutputFailed
		p.failure = failureFor(err)
	}
	close(p.done)
}

// Единожды запускает учтённое закрытие допуска отдельно от отмены сети, чтобы ожидание общей
// блокировки не задерживало Stop до его срока.
func (p *Publisher) requestSeal() {
	p.sealOnce.Do(func() {
		// Единожды запускает учтённую горутину запрета новых операций исполнения, чтобы ожидание общего
		// допуска не задерживало отмену сети.

		// Захват общей блокировки может ждать чужую управляющую операцию.
		// Отдельный учтённый работник позволяет немедленно отменить сеть.
		go func() {
			// Запечатывает исполнение и закрывает sealDone только после возврата seal.
			p.execution.seal()
			close(p.sealDone)
		}()
	})
}
