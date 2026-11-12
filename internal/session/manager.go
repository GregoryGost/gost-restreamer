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
	"github.com/GregoryGost/gost-restreamer/internal/monitoring"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// ErrLimit обозначает исчерпание явного бюджета активных сессий, выходов либо команд Manager.
var ErrLimit = errors.New("исчерпан бюджет активной работы SessionManager")

// IngressOwner закрывает допуск OBS и присоединяет принадлежащие ему соединения.
type IngressOwner interface {
	// Shutdown закрывает приём публикаций OBS и ожидает принадлежащие ему соединения в пределах
	// переданного контекста.
	Shutdown(context.Context) error
}

// PreparationOwner закрывает приём подготовки и присоединяет её пользователей.
type PreparationOwner interface {

	// CloseAdmission запрещает принимать новые операции подготовки до ожидания уже принятых пользователей.
	CloseAdmission()

	// Reconcile продолжает сверку и освобождение ресурсов подготовки в пределах контекста вызова.
	Reconcile(context.Context) error

	// Close закрывает подготовку и ожидает её пользователей в пределах переданного контекста.
	Close(context.Context) error
}

// Config задаёт общие зависимости, медиабюджеты, наблюдения и сроки управления сессиями.
type Config struct {
	// Core предоставляет долговечные команды, сущности и общую RuntimeAuthority.
	Core *core.Service
	// Files открывает закреплённые артефакты заглушек с проверкой содержимого.
	Files *fallback.FileStore
	// Processes учитывает процессы и временные объекты точных выполнений сессий.
	Processes *processowner.Registry
	// GeneratorVersion задаёт допустимую версию генератора используемых готовых артефактов.
	GeneratorVersion string
	// Group задаёт статические настройки переключателей и FFmpeg-работников групп.
	Group group.RuntimeConfig
	// Router ограничивает входной маршрутизатор отдельной Session.
	Router media.RouterLimits
	// Publisher задаёт бюджеты независимых выходных подписок и сетевых попыток.
	Publisher publisher.Options
	// Ingress закрывает допуск OBS и ожидает принадлежащие ему соединения при завершении приложения.
	Ingress IngressOwner
	// Preparation закрывает приём подготовки и продолжает её сверку и завершение.
	Preparation PreparationOwner
	// Logger принимает ошибки операций SessionManager и коррелированную диагностику медиатракта.
	Logger *slog.Logger
	// Monitoring задаёт периодический сбор оперативных измерений Session и предел числа источников.
	Monitoring monitoring.SamplerConfig
	// Telemetry ограничивает реестр наблюдений и событий общей RuntimeAuthority.
	Telemetry core.TelemetryLimits
	// MaxSessions ограничивает число активных локальных сессий; MaxOutputs ограничивает членства
	// отдельной Session; MaxCommands ограничивает одновременно учитываемые команды; PageSize
	// ограничивает страницу сверки.
	MaxSessions, MaxOutputs, MaxCommands, PageSize int
	// MaxGroups ограничивает группы одной Session; MaxReferences ограничивает выданные её реестром
	// ссылки.
	MaxGroups, MaxReferences uint32
	// ReconcileInterval задаёт период сверки и повторов; StartupTimeout ограничивает запуск и
	// готовность; OperationTimeout ограничивает отдельные операции; ShutdownTimeout ограничивает
	// ожидание физической очистки.
	ReconcileInterval, StartupTimeout, OperationTimeout, ShutdownTimeout time.Duration
}

// ValidateSettings проверяет серверные настройки без приобретения зависимостей.
func (c Config) ValidateSettings() error {
	if c.Telemetry.Validate() != nil || c.Monitoring.Validate() != nil || c.Group.FFmpeg.Progress.MaxLineBytes <= 0 || c.Group.FFmpeg.Progress.MaxReportBytes <= 0 || c.Group.FFmpeg.Progress.MaxFields <= 0 {
		return core.ErrInvalid
	}
	if c.MaxOutputs > 0 && uint64(c.Monitoring.MaxSources) < 1+3*uint64(c.MaxGroups)+uint64(c.MaxOutputs) {
		return core.ErrInvalid
	}
	if c.GeneratorVersion == "" || c.MaxSessions <= 0 || c.MaxOutputs <= 0 || c.MaxCommands <= 0 || c.PageSize <= 0 || c.PageSize > core.MaxPageLimit || c.MaxGroups == 0 || c.MaxReferences == 0 || c.ReconcileInterval <= 0 || c.StartupTimeout <= 0 || c.OperationTimeout <= 0 || c.ShutdownTimeout <= 0 || c.Router.Validate() != nil || c.Publisher.Validate() != nil {
		return core.ErrInvalid
	}
	return c.Group.ValidateTemplate()
}

// validate дополнительно проверяет наличие общих зависимостей Manager перед проверкой настроек.
func (c Config) validate() error {
	if c.Core == nil || c.Files == nil || c.Processes == nil || c.Logger == nil || c.Ingress == nil || c.Preparation == nil {
		return core.ErrInvalid
	}
	return c.ValidateSettings()
}

// Manager владеет диспетчером, Session и командами; БД и FileStore закрывает владелец приложения.
type Manager struct {
	// config хранит проверенные зависимости и бюджеты Manager.
	config Config
	// dispatcher предоставляет единственный допуск диспетчеризации общей RuntimeAuthority.
	dispatcher *core.RuntimeDispatcher
	// ctx задаёт самостоятельный срок исполнения Manager после возврата конструктора.
	ctx context.Context
	// cancel прекращает диспетчеризацию и зависимые локальные исполнения Manager.
	cancel context.CancelFunc
	// mu защищает приём обращений, выполняющиеся команды и локальные узлы сессий.
	mu sync.Mutex
	// closed запрещает новые обращения и выдачу Feed после начала закрытия.
	closed bool
	// commands отмечает квитанции, для которых уже выполняется ограниченный обработчик команды.
	commands map[core.ID]bool
	// sessions связывает идентификатор Session с её текущим локальным узлом исполнения и очистки.
	sessions map[core.ID]*sessionNode
	// wake объединяет уведомления о возможной работе в один неблокирующий сигнал.
	wake chan struct{}
	// done закрывается после логического отзыва сессий и освобождения диспетчера, отдельно от
	// физической очистки ресурсов.
	done chan struct{}
	// workers учитывает фактический срок работников команд, включая длительное выполнение Session.
	workers sync.WaitGroup
	// admissions учитывает ранее допущенные внешние обращения до передачи диспетчера.
	admissions sync.WaitGroup
	// admissionWait единожды запускает ожидание завершения всех допущенных обращений.
	admissionWait sync.Once
	// admissionDone закрывается после возврата всех ранее принятых обращений.
	admissionDone chan struct{}
	// workerWait единожды запускает ожидание работников команд при Close.
	workerWait sync.Once
	// workerDone закрывается после фактического возврата всех работников команд.
	workerDone chan struct{}
	// closeAdmission единожды запрещает новые подготовки и поддерживаемый ingress-приём.
	closeAdmission sync.Once
	// receiptCursor хранит продолжение страницы квитанций; cleanupCursor хранит продолжение страницы
	// незавершённой очистки.
	receiptCursor, cleanupCursor string
	// recoveryIntentCursor хранит продолжение выбора намерения для фоновой проверки прежних областей
	// процессов.
	recoveryIntentCursor string
	// recovery хранит текущий постраничный поиск единственной области прежнего выполнения.
	recovery *cleanupProbe
}

// New настраивает телеметрию, занимает единственный диспетчер Store и запускает сверку команд.
// Контекст вызова ограничивает создание; после возврата его отмена не останавливает принятые сессии.
func New(ctx context.Context, c Config) (*Manager, error) {
	// err сообщает об отказе проверки настроек и обязательных зависимостей Manager.
	if err := c.validate(); err != nil {
		return nil, err
	}
	// err сообщает об отказе настройки телеметрии до приобретения диспетчера.
	if err := c.Core.ConfigureTelemetry(ctx, c.Telemetry); err != nil {
		return nil, err
	}
	// dispatcher содержит полученный единственный допуск; err сообщает об отказе его резервирования.
	dispatcher, err := c.Core.ReserveDispatcher(ctx)
	if err != nil {
		return nil, err
	}
	// Срок исполнения задаёт Manager; отмена конструктора после возврата не отзывает Session.
	// lifetime задаёт независимый от вызова конструктора срок Manager; cancel прекращает этот срок
	// при закрытии.
	lifetime, cancel := context.WithCancel(context.Background())
	// m содержит созданный Manager с пустыми наборами локальных сессий и команд.
	m := &Manager{config: c, dispatcher: dispatcher, ctx: lifetime, cancel: cancel, commands: map[core.ID]bool{}, sessions: map[core.ID]*sessionNode{}, wake: make(chan struct{}, 1), done: make(chan struct{}), admissionDone: make(chan struct{}), workerDone: make(chan struct{})}
	go m.run()
	return m, nil
}

// signal уведомляет диспетчер о возможной работе; уже ожидающее уведомление сохраняется без блокировки
// вызывающего.
func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// admission проверяет, что Manager ещё принимает команды и сохраняет действующий допуск диспетчера.
func (m *Manager) admission() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || !m.dispatcher.Valid() {
		return core.ErrClosed
	}
	return nil
}

// enter учитывает принятое обращение до снятия блокировки, чтобы закрытие дождалось всех обращений,
// допущенных ранее.
func (m *Manager) enter() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || !m.dispatcher.Valid() {
		return core.ErrClosed
	}
	m.admissions.Add(1)
	return nil
}

// StartSession возвращает прежнюю квитанцию повтора либо проверяет выбранные файлы и сохраняет новую
// команду запуска. Возможный успех записи перепроверяется после ошибки; для запуска, принятого во
// время закрытия, дополнительно запрашивается остановка.
func (m *Manager) StartSession(ctx context.Context, request core.StartSessionRequest) (core.CommandReceipt, error) {
	// err сообщает об отказе допуска нового обращения StartSession.
	if err := m.enter(); err != nil {
		return core.CommandReceipt{}, err
	}
	defer m.admissions.Done()
	// prior содержит прежнюю квитанцию RequestID; found показывает её наличие; err сохраняет
	// результаты проверки, выбора привязок и принятия запуска.
	prior, found, err := m.config.Core.PriorStartSession(ctx, request)
	if err != nil || found {
		if found {
			m.signal()
		}
		return prior, err
	}
	// bindings содержит текущие подготовленные привязки выбранных назначений до записи нового
	// запуска.
	bindings, err := m.config.Core.SelectStartBindings(ctx, request.StreamID, request.DestinationIDs)
	if err != nil {
		return m.startAfterFailure(request, err)
	}
	// binding содержит очередной выбранный вариант заглушки для проверки файла и версии генератора.
	for _, binding := range bindings {
		if err = m.checkBinding(ctx, binding); err != nil {
			return m.startAfterFailure(request, err)
		}
	}
	request.Bindings = bindings
	if err = m.admission(); err != nil {
		return core.CommandReceipt{}, err
	}
	// receipt содержит сохранённую квитанцию нового запуска Session.
	receipt, err := m.config.Core.StartSession(ctx, request)
	if err != nil {
		return m.startAfterFailure(request, err)
	}
	if m.admission() != nil {
		// cleanup ограничивает остановку запуска, принятого во время закрытия; cancel отменяет
		// контекст cleanup после запроса остановки.
		cleanup, cancel := m.operationContext()
		m.log(m.stopForClose(cleanup, receipt.SessionID), "late_start_close")
		cancel()
	}
	m.signal()
	return receipt, err
}

// AddOutput возвращает прежнюю квитанцию повтора либо проверяет закрепляемый вариант заглушки и
// сохраняет команду добавления выхода. Ошибка записи допускает последующую проверку уже принятого
// запроса.
func (m *Manager) AddOutput(ctx context.Context, request core.AddOutputRequest) (core.CommandReceipt, error) {
	// err сообщает об отказе допуска нового обращения AddOutput.
	if err := m.enter(); err != nil {
		return core.CommandReceipt{}, err
	}
	defer m.admissions.Done()
	// prior содержит прежнюю квитанцию добавления; found показывает её наличие; err сохраняет
	// результаты выбора и проверки новой привязки.
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
	// receipt содержит сохранённую квитанцию нового выхода.
	receipt, err := m.config.Core.AddOutput(ctx, request)
	if err != nil {
		return m.addAfterFailure(request, err)
	}
	m.signal()
	return receipt, err
}

// startAfterFailure ищет сохранённую квитанцию с отдельным сроком после ошибки запуска, а при закрытии
// Manager дополнительно запрашивает остановку найденной сессии.
func (m *Manager) startAfterFailure(request core.StartSessionRequest, cause error) (core.CommandReceipt, error) {
	// ctx ограничивает независимую проверку неоднозначного запуска; cancel отменяет контекст ctx
	// после проверки.
	ctx, cancel := m.operationContext()
	defer cancel()
	// receipt содержит повторно найденную квитанцию запуска; found показывает возможный прежний
	// успех; err сообщает об отказе чтения.
	receipt, found, err := m.config.Core.PriorStartSession(ctx, request)
	if err != nil {
		return core.CommandReceipt{}, errors.Join(cause, err)
	}
	if found {
		if m.admission() != nil {
			// cleanup ограничивает остановку найденного позднего запуска; stop отменяет
			// контекст cleanup после запроса остановки.
			cleanup, stop := m.operationContext()
			m.log(m.stopForClose(cleanup, receipt.SessionID), "late_start_close")
			stop()
		}
		m.signal()
		return receipt, nil
	}
	return core.CommandReceipt{}, cause
}

// addAfterFailure проверяет, была ли команда добавления выхода сохранена несмотря на ошибку; найденная
// квитанция сохраняет исходный результат запроса.
func (m *Manager) addAfterFailure(request core.AddOutputRequest, cause error) (core.CommandReceipt, error) {
	// ctx ограничивает независимую проверку неоднозначного добавления выхода; cancel отменяет
	// контекст ctx после проверки.
	ctx, cancel := m.operationContext()
	defer cancel()
	// receipt содержит повторно найденную квитанцию добавления; found показывает сохранённый
	// результат; err сообщает об отказе чтения.
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

// checkBinding сверяет версию генератора и поколение варианта, затем открывает файл с проверкой
// размера и хеша до принятия команды.
func (m *Manager) checkBinding(ctx context.Context, b core.PreparedBinding) error {
	if b.GeneratorVersion != m.config.GeneratorVersion {
		return core.ErrNotReady
	}
	// variant содержит сохранённый вариант привязки; err сохраняет результаты его чтения и
	// проверенного открытия файла.
	variant, err := m.config.Core.GetVariant(ctx, b.VariantID)
	if err != nil {
		return err
	}
	if variant.GenerationID != b.GenerationID || variant.GeneratorVersion != b.GeneratorVersion {
		return core.ErrStale
	}
	// file содержит проверенный по размеру и хешу дескриптор варианта, закрываемый до принятия
	// команды.
	file, err := m.config.Files.OpenExact(ctx, variant.ManagedPath, variant.SizeBytes, variant.ContentHash)
	if err != nil {
		return err
	}
	return file.Close()
}

// sessionCommand учитывает обращение в период закрытия, передаёт команду сессии в core и уведомляет
// диспетчер о возможной работе.
func (m *Manager) sessionCommand(ctx context.Context, r core.SessionCommand, call func(context.Context, core.SessionCommand) (core.CommandReceipt, error)) (core.CommandReceipt, error) {
	// err сообщает об отказе допуска управляющего обращения Session.
	if err := m.enter(); err != nil {
		return core.CommandReceipt{}, err
	}
	defer m.admissions.Done()
	// receipt содержит результат сохраняемой команды Session; err сообщает об отказе её принятия.
	receipt, err := call(ctx, r)
	m.signal()
	return receipt, err
}

// PauseSession сохраняет запрос ручной паузы; возвращённая квитанция не подтверждает фактическое
// переключение всех групп на заглушку.
func (m *Manager) PauseSession(ctx context.Context, r core.SessionCommand) (core.CommandReceipt, error) {
	return m.sessionCommand(ctx, r, m.config.Core.PauseSession)
}

// ResumeSession сохраняет запрос снятия ручной паузы и уведомляет диспетчер о необходимости применить
// желаемое состояние.
func (m *Manager) ResumeSession(ctx context.Context, r core.SessionCommand) (core.CommandReceipt, error) {
	return m.sessionCommand(ctx, r, m.config.Core.ResumeSession)
}

// StopSession сохраняет команду остановки сессии и уведомляет диспетчер; физическое завершение
// ресурсов отслеживается отдельно.
func (m *Manager) StopSession(ctx context.Context, r core.SessionCommand) (core.CommandReceipt, error) {
	return m.sessionCommand(ctx, r, m.config.Core.StopSession)
}

// outputCommand учитывает обращение в период закрытия, передаёт команду выхода в core и уведомляет
// диспетчер независимо от результата вызова.
func (m *Manager) outputCommand(ctx context.Context, r core.OutputCommand, call func(context.Context, core.OutputCommand) (core.CommandReceipt, error)) (core.CommandReceipt, error) {
	// err сообщает об отказе допуска управляющего обращения выхода.
	if err := m.enter(); err != nil {
		return core.CommandReceipt{}, err
	}
	defer m.admissions.Done()
	// receipt содержит результат сохраняемой команды выхода; err сообщает об отказе её принятия.
	receipt, err := call(ctx, r)
	m.signal()
	return receipt, err
}

// StartOutput сохраняет команду повторного запуска прикреплённого выхода с его прежним снимком.
func (m *Manager) StartOutput(ctx context.Context, r core.OutputCommand) (core.CommandReceipt, error) {
	return m.outputCommand(ctx, r, m.config.Core.StartOutput)
}

// StopOutput сохраняет команду остановки отдельного выхода; завершение команды выполняется после
// проверки его очистки.
func (m *Manager) StopOutput(ctx context.Context, r core.OutputCommand) (core.CommandReceipt, error) {
	return m.outputCommand(ctx, r, m.config.Core.StopOutput)
}

// DetachOutput сохраняет команду отсоединения выхода; очистка прежней попытки выполняется независимо
// от соседних выходов.
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
	// node содержит текущий локальный узел Session для проверки точного работающего Feed.
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

// operationContext создаёт отдельный ограниченный срок операции, не наследующий отмену длительного
// исполнения Manager.
func (m *Manager) operationContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), m.config.OperationTimeout)
}

// log записывает ошибку вместе с именем операции и компонентом session. При отсутствии ошибки запись в
// журнал не создаётся.
func (m *Manager) log(err error, operation string) {
	if err != nil {
		m.config.Logger.Error("session_operation", "component", "session", "operation", operation, "error", err)
	}
}

// run сверяет команды сразу и после уведомлений или интервала, а при отмене Manager либо отзыве
// диспетчера передаёт управление завершению диспетчеризации.
func (m *Manager) run() {
	defer m.finishDispatch()
	// ticker задаёт период сверки долговечных команд при отсутствии новых уведомлений.
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

// beginClose запрещает новые обращения, однократно закрывает приём подготовки и поддерживающего эту
// операцию приёма публикаций OBS, затем отменяет длительное исполнение Manager.
func (m *Manager) beginClose() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.closeAdmission.Do(func() {
		// Однократно закрывает приём подготовки и, при наличии соответствующего метода, приём публикаций OBS
		// при начале закрытия Manager.

		m.config.Preparation.CloseAdmission()
		// owner содержит ingress-порт закрытия приёма; ok показывает, поддерживает ли его
		// текущий владелец OBS.
		if owner, ok := m.config.Ingress.(interface {
			// CloseAdmission позволяет Manager закрыть приём публикаций OBS до ожидания ранее допущенных
			// обращений.
			CloseAdmission()
		}); ok {
			owner.CloseAdmission()
		}
	})
	m.cancel()
}

// finishDispatch ждёт ранее допущенные обращения и завершает глобальный отзыв сессий до освобождения
// диспетчера. После передачи допуска сигнализирует об окончании диспетчеризации, не подменяя этим
// ожидание физических ресурсов.
func (m *Manager) finishDispatch() {
	m.beginClose()
	m.admissionWait.Do(func() {
		// Запускает единственное ожидание ранее допущенных обращений, которое продолжится независимо от срока
		// отдельного Close.

		go func() {
			// После возврата всех ранее допущенных обращений закрывает admissionDone и разрешает глобальный отзыв
			// сессий.
			m.admissions.Wait()
			close(m.admissionDone)
		}()
	})
	<-m.admissionDone
	// Последний глобальный отзыв выполняется до передачи диспетчера следующему Manager.
	for m.dispatcher.Valid() {
		// ctx ограничивает очередной глобальный отзыв сессий; cancel освобождает отдельный срок
		// операции.
		ctx, cancel := m.operationContext()
		// err содержит результат глобального запроса остановки сессий перед передачей
		// диспетчера.
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
	// node содержит очередную собственную Session, отменяемую после долговечного отзыва.
	for _, node := range m.sessions {
		node.stop()
	}
	m.mu.Unlock()
	for {
		// ctx ограничивает очередную попытку освобождения диспетчера; cancel отменяет контекст
		// ctx после попытки освобождения диспетчера.
		ctx, cancel := m.operationContext()
		// err содержит результат CloseContext допуска диспетчеризации.
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

// retryPauseUnlessDispatcher ждёт интервал повторной попытки либо прекращает ожидание при отзыве
// диспетчера.
func retryPauseUnlessDispatcher(d *core.RuntimeDispatcher, delay time.Duration) bool {
	// timer отсчитывает паузу повтора, прерываемую отзывом диспетчера.
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-d.Done():
		return false
	case <-timer.C:
		return true
	}
}

// reconcile обрабатывает одну страницу незавершённых квитанций и продолжает отложенные завершения,
// очистку и сверку подготовки с общим ограниченным контекстом операции.
func (m *Manager) reconcile() {
	// ctx ограничивает проход сверки сроком Manager и OperationTimeout; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(m.ctx, m.config.OperationTimeout)
	defer cancel()
	// page содержит страницу незавершённых квитанций; err сообщает об отказе её чтения.
	page, err := m.config.Core.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: m.config.PageSize, PendingOnly: true}, Cursor: m.receiptCursor})
	if err != nil {
		m.log(err, "receipts")
		return
	}
	m.receiptCursor = page.NextCursor
	// receipt содержит очередную сохранённую квитанцию для локальной диспетчеризации.
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

// dispatch допускает обработку квитанции с учётом бюджетов команд и сессий; запуск выхода ждёт
// работающей родительской сессии. Длительное исполнение может снять учёт команды до возврата работника
// через commandFinished.
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
		// active считает локальные Session, ещё не начавшие закрываться, для бюджета
		// MaxSessions.
		active := 0
		// node содержит очередную Session при подсчёте активных выполнений.
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
		// node содержит родительскую Session команды запуска либо добавления выхода.
		node := m.sessions[receipt.SessionID]
		if node == nil {
			m.mu.Unlock()
			return
		}
		node.mu.Lock()
		// ready показывает, работает ли родительская Session и не началось ли её закрытие.
		ready := node.running && !node.closing
		node.mu.Unlock()
		if !ready {
			m.mu.Unlock()
			return
		}
	}
	if receipt.Kind == core.CommandStartSession {
		// lifetime задаёт срок создаваемой Session от Manager; cancel прекращает её локальное
		// исполнение.
		lifetime, cancel := context.WithCancel(m.ctx)
		m.sessions[receipt.SessionID] = &sessionNode{manager: m, id: receipt.SessionID, ctx: lifetime, cancel: cancel, done: make(chan struct{}), members: map[core.ID]*membership{}, outputs: map[core.ID]*outputNode{}, excluded: map[core.ID]uint64{}}
	}
	m.commands[receipt.ID] = true
	m.workers.Add(1)
	m.mu.Unlock()
	go func() {
		// Запускает обработчик запуска сессии, выхода или управляющей команды; при возврате снимает учёт
		// работника и оставшийся учёт квитанции.

		defer m.workers.Done()
		defer func() {
			// Удаляет квитанцию из выполняющихся команд под блокировкой и будит диспетчер для продолжения работы.
			m.mu.Lock()
			delete(m.commands, receipt.ID)
			m.mu.Unlock()
			m.signal()
		}()
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
	// f накапливает точную попытку команды, подтверждённую Claim и MarkIssued.
	var f core.ExecutionFence
	if c.Status == core.CommandClaimed || c.Status == core.CommandIssued {
		f = receiptFence(c)
		if c.Status == core.CommandIssued {
			return f, nil
		}
	}
	for f.CommandID == "" {
		// claimed содержит результат Claim; err сообщает об отказе записи, который требует
		// отдельной сверки квитанции.
		claimed, err := m.config.Core.Claim(ctx, c.RequestID)
		if err == nil {
			f = claimed
			break
		}
		// check ограничивает повторное чтение после неоднозначного Claim; stop отменяет контекст
		// check после чтения квитанции.
		check, stop := m.operationContext()
		// current содержит повторно прочитанную квитанцию; e сообщает об отказе сверки
		// результата Claim.
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
		// err содержит результат сохранения стадии ISSUED до внешних действий.
		err := m.config.Core.MarkIssued(ctx, f)
		if err == nil {
			return f, nil
		}
		// check ограничивает повторное чтение после неоднозначного MarkIssued; stop отменяет
		// контекст check после чтения квитанции.
		check, stop := m.operationContext()
		// current содержит повторно прочитанную квитанцию; e сообщает об отказе сверки стадии
		// ISSUED.
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

// receiptFence восстанавливает точную идентичность выданной попытки из полей квитанции, включая
// поколение выхода и номер попытки.
func receiptFence(c core.CommandReceipt) core.ExecutionFence {
	return core.ExecutionFence{CommandID: c.ID, Incarnation: c.Incarnation, SessionID: c.SessionID, Lifecycle: c.Lifecycle, OutputID: c.OutputID, OutputGeneration: c.OutputGeneration, Attempt: c.Attempt}
}

// complete сохраняет успех либо текст отказа команды и после ошибки проверяет, не был ли тот же
// результат уже записан для точной попытки.
func (m *Manager) complete(ctx context.Context, f core.ExecutionFence, success bool, failure error) error {
	// completion содержит сохраняемый успех либо полную причину отказа точной команды.
	completion := core.Completion{Success: success}
	if !success {
		if failure == nil {
			failure = core.ErrNotReady
		}
		completion.Failure = failure.Error()
	}
	// err содержит результат записи завершения, который при ошибке перепроверяется чтением.
	err := m.config.Core.Complete(ctx, f, completion)
	if err == nil {
		return nil
	}
	// check ограничивает независимую проверку результата Complete; cancel отменяет контекст check
	// после проверки результата.
	check, cancel := m.operationContext()
	defer cancel()
	// current содержит повторно прочитанную квитанцию; readErr сообщает об отказе подтверждения уже
	// сохранённого результата.
	current, readErr := m.config.Core.GetReceipt(check, f.CommandID)
	if readErr == nil && receiptFence(current) == f && (success && current.Status == core.CommandSucceeded || !success && current.Status == core.CommandFailed) {
		return nil
	}
	return errors.Join(err, readErr)
}

// sameCommand сравнивает неизменяемую идентичность команды без её текущего статуса и номера попытки.
func sameCommand(a, b core.CommandReceipt) bool {
	return a.ID == b.ID && a.RequestID == b.RequestID && a.Kind == b.Kind && a.SessionID == b.SessionID && a.Incarnation == b.Incarnation && a.Lifecycle == b.Lifecycle && a.OutputID == b.OutputID && a.OutputGeneration == b.OutputGeneration
}

// control продолжает выдачу, применение и завершение управляющей команды после временных ошибок. После
// применения сохраняет результат в памяти, чтобы повтор записи не повторял внешний шаг; остановку
// выхода завершает очистка.
func (m *Manager) control(c core.CommandReceipt) {
	// fence сохраняет точную выданную попытку продолжаемой управляющей команды.
	var fence core.ExecutionFence
	// applied отмечает уже выполненный внешний шаг и запрещает его повтор при ошибке финальной
	// записи.
	var applied bool
	// failure сохраняет исход применения управления для повторяемой записи того же завершения.
	var failure error
	for m.ctx.Err() == nil {
		// ctx ограничивает очередную попытку продолжения управляющей команды; cancel отменяет
		// контекст ctx перед повтором или возвратом.
		ctx, cancel := context.WithTimeout(m.ctx, m.config.OperationTimeout)
		// current содержит актуальную квитанцию; err сохраняет результаты её чтения, выдачи и
		// завершения.
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

// applyControl применяет текущую ревизию паузы к реестру групп либо отзывает локальную сессию или
// выход. Признак результата отличает завершённый внешний шаг от чтения, которое требуется повторить.
func (m *Manager) applyControl(ctx context.Context, c core.CommandReceipt) (error, bool) {
	m.mu.Lock()
	// node содержит локальную Session, к которой относится сохраняемая команда управления.
	node := m.sessions[c.SessionID]
	m.mu.Unlock()
	switch c.Kind {
	case core.CommandPauseSession, core.CommandResumeSession:
		if node == nil {
			return core.ErrNotReady, true
		}
		// session содержит текущее желание паузы и ревизию; err сообщает об отказе чтения перед
		// применением.
		session, err := m.config.Core.GetSession(ctx, c.SessionID)
		if err != nil {
			return err, false
		}
		node.mu.Lock()
		// registry содержит реестр групп Session для упорядоченного применения ревизии паузы.
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

// Close запрещает новый допуск, отзывает команды и независимо ограничивает завершение работы.
// Повторный вызов может дождаться операций, не завершившихся в предыдущий срок.
func (m *Manager) Close(ctx context.Context) error {
	m.beginClose()
	// shutdown ограничивает текущий Close отдельным сроком ShutdownTimeout; cancel прекращает
	// ожидание по этому сроку или отмене вызова.
	shutdown, cancel := context.WithTimeout(context.Background(), m.config.ShutdownTimeout)
	defer cancel()
	// stop снимает обработчик отмены вызывающего контекста, который прерывает shutdown.
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	select {
	case <-m.done:
	case <-shutdown.Done():
		return errors.Join(shutdown.Err(), ctx.Err())
	}
	// После передачи диспетчера остаются только ресурсы этого Manager.
	// result объединяет ошибки завершения ingress, подготовки и собственных ресурсов Manager.
	var result error
	result = errors.Join(result, m.config.Ingress.Shutdown(shutdown), m.config.Preparation.Close(shutdown))
	m.workerWait.Do(func() {
		// Запускает единственное ожидание работников команд, сохраняющееся между повторными вызовами Close.

		go func() {
			// Закрывает workerDone только после фактического возврата всех работников команд.
			m.workers.Wait()
			close(m.workerDone)
		}()
	})
	select {
	case <-m.workerDone:
	case <-shutdown.Done():
		return errors.Join(result, shutdown.Err(), ctx.Err())
	}
	m.mu.Lock()
	// nodes сохраняет список собственных узлов Session для ожидания вне блокировки Manager.
	nodes := make([]*sessionNode, 0, len(m.sessions))
	// node содержит очередной собственный узел при построении списка завершения.
	for _, node := range m.sessions {
		nodes = append(nodes, node)
	}
	m.mu.Unlock()
	// node содержит очередную Session, чьё окончание и очистка ожидаются после работников команд.
	for _, node := range nodes {
		select {
		case <-node.done:
		case <-shutdown.Done():
			return errors.Join(result, shutdown.Err(), ctx.Err())
		}
		node.mu.Lock()
		// joined сохраняет признак успешной очистки Session для выбора необходимого повтора.
		joined := node.joined
		node.mu.Unlock()
		if !joined {
			result = errors.Join(result, node.cleanupWithContext(shutdown))
		}
	}
	return result
}

// stopForClose сохраняет идемпотентную команду остановки сессии и завершает её квитанцию, если та ещё
// не терминальна.
func (m *Manager) stopForClose(ctx context.Context, id core.ID) error {
	// receipt содержит идемпотентную квитанцию остановки при закрытии; err сообщает об отказе её
	// записи.
	receipt, err := m.config.Core.StopSession(ctx, core.SessionCommand{RequestID: "manager:close:" + string(id), SessionID: id})
	if err != nil {
		return err
	}
	if !receipt.Status.Terminal() {
		// f содержит точную попытку остановки; e сохраняет результат её выдачи и завершения.
		f, e := m.issue(ctx, receipt)
		if e == nil {
			e = m.complete(ctx, f, true, nil)
		}
		return e
	}
	return nil
}

// revokeSessions постранично запрашивает остановку всех сессий в состояниях STARTING, RUNNING и
// STOPPING, сохраняя ошибки отдельных запросов.
func (m *Manager) revokeSessions(ctx context.Context) error {
	// result объединяет ошибки остановки отдельных сессий всех проверяемых состояний.
	var result error
	// state задаёт очередное состояние STARTING, RUNNING либо STOPPING для поиска отзываемых
	// Session.
	for _, state := range []core.SessionState{core.SessionStarting, core.SessionRunning, core.SessionStopping} {
		// request содержит текущую страницу сессий выбранного состояния.
		request := core.PageRequest{Query: core.ListQuery{Limit: m.config.PageSize, State: string(state)}}
		for {
			// page содержит очередную страницу Session; err сообщает об отказе её чтения.
			page, err := m.config.Core.ListSessions(ctx, request)
			if err != nil {
				return errors.Join(result, err)
			}
			// session содержит очередную сохранённую Session для идемпотентного запроса
			// остановки.
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

// scopeOwner сохраняет версию, вид и точную идентичность выполнения в описании владельца
// processowner.Scope.
type scopeOwner struct {
	// Version задаёт версию JSON-представления владельца области Session.
	Version int `json:"version"`
	// Kind различает владельца session при фоновой сверке областей процессов.
	Kind string `json:"kind"`
	// Fence содержит точную команду, incarnation, lifecycle и попытку родительского выполнения.
	Fence core.ExecutionFence `json:"fence"`
}

// encodeOwner сериализует версию, вид владельца и точный ExecutionFence в описание области процессов
// сессии.
func encodeOwner(f core.ExecutionFence) string {
	// b содержит JSON-байты описания точного владельца области Session.
	b, _ := json.Marshal(scopeOwner{Version: 1, Kind: "session", Fence: f})
	return string(b)
}
