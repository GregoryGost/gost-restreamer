package fallback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// Config задаёт конечные бюджеты операций; очередь ограничивает общий Store.
type Config struct {
	// GeneratorVersion сохраняется в поколении и манифестах для проверки совместимости рецепта
	// подготовки.
	GeneratorVersion string
	// FFmpeg задаёт исполняемый файл генератора. FFprobe задаёт исполняемый файл проверки
	// метаданных.
	FFmpeg, FFprobe string
	// MaxUploadBytes ограничивает размер исходного файла в байтах. MaxOutputBytes ограничивает
	// размер каждого создаваемого варианта в байтах.
	MaxUploadBytes, MaxOutputBytes uint64
	// PrepareTimeout ограничивает всё фоновое задание подготовки. ShutdownTimeout ограничивает
	// отдельные попытки отзыва и очистки. QueueInterval задаёт паузу перед повторным захватом
	// занятого слота.
	PrepareTimeout, ShutdownTimeout, QueueInterval time.Duration
	// Runner задаёт сроки и бюджеты запуска внешних команд.
	Runner RunnerLimits
	// Probe задаёт ограничения метаданных и медиапроверки файлов.
	Probe ProbeLimits
	// ProcessRegistry при наличии создаёт журналируемые области и процессы подготовки.
	ProcessRegistry *processowner.Registry
	// ReconcileLimit ограничивает число записей в странице сверки и очистки.
	ReconcileLimit int
}

// Validate проверяет непустые GeneratorVersion, FFmpeg и FFprobe, границы размеров и ReconcileLimit,
// положительные сроки; затем проверяет ограничения Runner и Probe.
func (c Config) Validate() error {
	if c.GeneratorVersion == "" || c.FFmpeg == "" || c.FFprobe == "" || c.MaxUploadBytes == 0 || c.MaxOutputBytes == 0 || c.MaxUploadBytes >= math.MaxInt64 || c.MaxOutputBytes >= math.MaxInt64 || c.PrepareTimeout <= 0 || c.ShutdownTimeout <= 0 || c.QueueInterval <= 0 || c.ReconcileLimit <= 0 || c.ReconcileLimit > core.MaxPageLimit {
		return ErrInvalid
	}
	return errors.Join(c.Runner.Validate(), c.Probe.Validate())
}

// Inspection определяет проверку исходного файла и подтверждение готового варианта перед публикацией.
type Inspection interface {

	// Source определяет фактический вид, формат и длительность исходного файла перед построением
	// вариантов; ошибки запрещают публикацию поколения.
	Source(context.Context, *os.File, core.AssetKind) (Source, error)

	// Verify проверяет фактический формат, конфигурации и медиапакеты подготовленного файла по применимым
	// требованиям профиля и возвращает наблюдённый формат. Проверка не подтверждает фактический битрейт и
	// все настройки кодировщика.
	Verify(context.Context, *os.File, core.GroupProfile) (core.MediaFormat, error)
}

// ProcessRunner выполняет одну внешнюю команду и завершает использование переданных файлов до возврата.
type ProcessRunner interface {

	// Run исполняет команду с input как FD 3 и выводом в output; возврат должен завершить использование
	// переданных дескрипторов процессом, поскольку менеджер затем закрывает файлы.
	Run(context.Context, Command, *os.File, io.Writer) (RunResult, error)
}

// Tools позволяет явно подставить ограниченные тестовые порты без автоматического выбора CPU.
type Tools struct {
	// Builder строит команду генерации варианта по проверенному источнику и профилю.
	Builder CommandBuilder
	// Inspector проверяет исходник и готовые варианты.
	Inspector Inspection
	// Runner выполняет построенную команду с ограниченным выводом.
	Runner ProcessRunner
	// Normalize при наличии преобразует принадлежащий менеджеру выход генератора перед окончательной
	// проверкой.
	Normalize func(context.Context, *os.File, io.Writer) error
	// VerifyLoopDuration при наличии сверяет фактический период варианта с вычисленной длиной петли.
	VerifyLoopDuration func(context.Context, *os.File, time.Duration) error
}

// Manager принимает задания загрузки и повторной подготовки, публикует проверенные поколения и
// координирует очистку их ресурсов.
type Manager struct {
	// service выполняет доменный протокол резервирования, допуска, публикации и подтверждения
	// очистки.
	service *core.Service
	// files создаёт, проверяет и удаляет управляемые файлы с доказательствами владения.
	files *FileStore
	// config содержит проверенные бюджеты и пути инструментов менеджера.
	config Config
	// tools содержит явно выбранные порты генерации и проверки.
	tools Tools
	// mu защищает допуск новых заданий, активные задания и отложенные области.
	mu sync.Mutex
	// closed запрещает новые Upload и Rebuild после закрытия допуска.
	closed bool
	// jobs связывает поколения с выполняющимися локальными заданиями.
	jobs map[core.ID]*Job
	// reconcileGate последовательно допускает сверку и её завершение при Close.
	reconcileGate chan struct{}
	// reconciliationClosed запрещает новые сверки после начала полного закрытия.
	reconciliationClosed bool
	// scopeCursor сохраняет позицию следующей страницы восстановления областей процессов.
	scopeCursor string
	// fileCursor сохраняет позицию следующей страницы файловой очистки.
	fileCursor string
	// retiredScopes удерживает области, физическое завершение которых требуется повторно
	// подтвердить.
	retiredScopes map[string]retiredPreparationScope
}

// NewManager требует реестр процессов и создаёт рабочие порты FFmpeg/ffprobe без автоматической
// подстановки кодировщиков.
func NewManager(service *core.Service, files *FileStore, config Config) (*Manager, error) {
	if config.ProcessRegistry == nil {
		return nil, ErrInvalid
	}
	// runner выполняет рабочие команды с заданными ограничениями запуска.
	runner := Runner{Limits: config.Runner}
	// probe использует рабочий ffprobe и те же ограничения для проверки и нормализации файлов.
	probe := Prober{Binary: config.FFprobe, Runner: runner, Limits: config.Probe}
	return NewManagerWithTools(service, files, config, Tools{Builder: ProductionBuilder(config.FFmpeg), Inspector: probe, Runner: runner, Normalize: probe.NormalizePrepared, VerifyLoopDuration: probe.VerifyLoopDuration})
}

// NewManagerWithTools проверяет зависимости и бюджеты, создавая менеджер с явными портами; при
// ProcessRegistry рабочее выполнение использует управляемые процессы вместо подставленных
// инструментов.
func NewManagerWithTools(service *core.Service, files *FileStore, config Config, tools Tools) (*Manager, error) {
	if service == nil || files == nil || tools.Builder == nil || tools.Inspector == nil || tools.Runner == nil {
		return nil, ErrInvalid
	}
	// err сообщает о некорректной конфигурации менеджера.
	if err := config.Validate(); err != nil {
		return nil, err
	}
	// gate содержит единственный маркер допуска сверки либо ожидания её завершения при Close.
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return &Manager{service: service, files: files, config: config, tools: tools, jobs: map[core.ID]*Job{}, reconcileGate: gate, retiredScopes: map[string]retiredPreparationScope{}}, nil
}

// Result возвращает итог подготовки и отдельно подтверждение физической очистки.
type Result struct {
	// Generation содержит исходное либо опубликованное поколение по результату задания.
	Generation core.FallbackGeneration
	// Err объединяет причины отказа подготовки и её завершения.
	Err error
	// Clean показывает успешное доменное подтверждение очистки подготовки.
	Clean bool
}

// PreparationError сохраняет идентичность неоднозначного принятия без повтора внешней работы.
type PreparationError struct {
	// GenerationID сохраняет идентификатор поколения, по которому можно проверить неоднозначное
	// принятие.
	GenerationID core.ID
	// Err сохраняет причину неподтверждённого принятия подготовки.
	Err error
}

// Error возвращает диагностический текст с идентификатором подготовки, принятие которой не
// подтверждено.
func (e *PreparationError) Error() string {
	return "не подтверждено принятие подготовки " + string(e.GenerationID) + ": " + e.Err.Error()
}

// Unwrap раскрывает исходную ошибку неоднозначного принятия подготовки.
func (e *PreparationError) Unwrap() error { return e.Err }

// Job представляет принятое фоновое задание с отдельными отменой, ожиданием и передачей загрузки.
type Job struct {
	// generation сохраняет исходное зарезервированное поколение для независимых снимков Generation.
	generation core.FallbackGeneration
	// cancel отменяет весь срок выполнения задания.
	cancel context.CancelFunc
	// done закрывается после выполнения задания и завершения обработчика прерывания загрузки.
	done chan struct{}
	// result содержит окончательный итог, читаемый после закрытия done.
	result Result
	// mu защищает привязку владельца подготовки к заданию.
	mu sync.Mutex
	// owner при наличии позволяет отозвать точный допуск выполняющейся подготовки.
	owner *core.PreparationOwner
	// shutdown задаёт предел отдельной попытки отзыва владельца через Cancel.
	shutdown time.Duration
	// upload при наличии хранит результат сохранения и закрытия тела загрузки отдельно от дальнейшей
	// подготовки.
	upload *uploadTransfer
}

// Generation возвращает независимый снимок исходно зарезервированного поколения, включая вложенные
// профили и формат источника.
func (j *Job) Generation() core.FallbackGeneration { return cloneGeneration(j.generation) }

// Done возвращает канал, закрываемый после завершения выполнения задания и обработчика отмены
// загрузки.
func (j *Job) Done() <-chan struct{} { return j.done }

// Cancel отменяет всё задание и пытается отозвать уже привязанного владельца в пределах срока
// остановки; физическое завершение ожидается отдельно через Wait.
func (j *Job) Cancel() {
	j.cancel()
	j.mu.Lock()
	// owner сохраняет текущего привязанного владельца для отзыва после снятия блокировки задания.
	owner := j.owner
	j.mu.Unlock()
	if owner != nil {
		// ctx ограничивает попытку отзыва владельца сроком shutdown. cancel освобождает ресурсы
		// этого контекста.
		ctx, cancel := context.WithTimeout(context.Background(), j.shutdown)
		_ = owner.Revoke(ctx)
		cancel()
	}
}

// Wait возвращает завершённый результат с независимым снимком поколения; отмена ожидания не отменяет
// задание и возвращает ErrCleanupPending.
func (j *Job) Wait(ctx context.Context) (Result, error) {
	select {
	case <-j.done:
		return j.completed(), j.result.Err
	default:
	}
	select {
	case <-j.done:
		return j.completed(), j.result.Err
	case <-ctx.Done():
		return Result{}, errors.Join(ErrCleanupPending, ctx.Err())
	}
}

// Копирует завершённый результат, отделяя изменяемые поля поколения от состояния Job; вызывается после
// закрытия done.
func (j *Job) completed() Result {
	// r содержит копию итогового результата для отделения вложенных данных поколения.
	r := j.result
	r.Generation = cloneGeneration(r.Generation)
	return r
}

// Копирует поколение вместе со срезами канонических профилей и указателями формата источника, чтобы
// снимок не изменял исходные данные.
func cloneGeneration(g core.FallbackGeneration) core.FallbackGeneration {
	g.RequiredProfiles = append([]core.RequiredProfile(nil), g.RequiredProfiles...)
	// i задаёт позицию требуемого профиля, чей канонический буфер копируется.
	for i := range g.RequiredProfiles {
		g.RequiredProfiles[i].CanonicalProfile = bytes.Clone(g.RequiredProfiles[i].CanonicalProfile)
	}
	if g.SourceFormat.Video != nil {
		// v содержит отдельную копию описания исходного видео.
		v := *g.SourceFormat.Video
		g.SourceFormat.Video = &v
	}
	if g.SourceFormat.Audio != nil {
		// a содержит отдельную копию описания исходного аудио.
		a := *g.SourceFormat.Audio
		g.SourceFormat.Audio = &a
	}
	return g
}

// Upload принимает очередь до первого чтения; после успеха Manager владеет reader до закрытия.
func (m *Manager) Upload(ctx context.Context, request core.PreparationRequest, reader io.ReadCloser) (*Job, error) {
	if reader == nil || request.Rebuild {
		return nil, ErrInvalid
	}
	return m.enqueue(ctx, request, reader)
}

// Rebuild принудительно включает повторную подготовку и ставит её в очередь без тела загрузки.
func (m *Manager) Rebuild(ctx context.Context, request core.PreparationRequest) (*Job, error) {
	request.Rebuild = true
	return m.enqueue(ctx, request, nil)
}

// Резервирует поколение до чтения reader и запускает задание с собственным сроком; после успешного
// принятия менеджер владеет reader. Неоднозначное резервирование сверяется по идентичности поколения.
func (m *Manager) enqueue(ctx context.Context, request core.PreparationRequest, reader io.ReadCloser) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, core.ErrClosed
	}
	if request.GeneratorVersion != "" && request.GeneratorVersion != m.config.GeneratorVersion {
		return nil, ErrInvalid
	}
	request.GeneratorVersion = m.config.GeneratorVersion
	// g содержит зарезервированное поколение, в том числе идентичность для проверки неоднозначного
	// принятия. err сообщает об отказе резервирования.
	g, err := m.service.ReservePreparation(ctx, request)
	if err != nil {
		if g.ID != "" {
			// check ограничивает независимую сверку результата резервирования. stop
			// освобождает её срок.
			check, stop := context.WithTimeout(context.Background(), m.config.ShutdownTimeout)
			defer stop()
			// stored содержит повторно прочитанное поколение. e сообщает об отказе чтения
			// после неоднозначного резервирования.
			stored, e := m.service.GetGeneration(check, g.ID)
			if e == nil && stored.AttemptToken == g.AttemptToken && stored.Incarnation == g.Incarnation && stored.State == core.PreparationQueued && !stored.Issued {
				err = errors.Join(err, m.service.CancelPreparation(check, core.PreparationFenceFor(stored), err.Error()))
			} else if !errors.Is(e, core.ErrNotFound) {
				err = errors.Join(err, e, ErrCleanupPending)
			}
			return nil, &PreparationError{GenerationID: g.ID, Err: err}
		}
		return nil, err
	}
	if reader != nil {
		reader = &ownedReader{ReadCloser: reader}
	}
	// work задаёт независимый срок фоновой подготовки после принятия задания. cancel отменяет всю
	// эту работу.
	work, cancel := context.WithTimeout(context.Background(), m.config.PrepareTimeout)
	// j содержит принятое задание с исходным поколением и каналом завершения.
	j := &Job{generation: g, cancel: cancel, done: make(chan struct{}), shutdown: m.config.ShutdownTimeout}
	if reader != nil {
		j.upload = &uploadTransfer{reader: reader, done: make(chan struct{})}
	}
	// uploadClosed закрывается после окончания обработчика прерывания загрузки.
	uploadClosed := make(chan struct{})
	// stopUpload снимает ещё не начавшийся обработчик отмены либо сообщает о необходимости дождаться
	// его.
	stopUpload := context.AfterFunc(work, func() {
		// При завершении контекста подготовки прерывает передачу загрузки и закрывает uploadClosed после
		// завершения попытки прерывания.

		defer close(uploadClosed)
		if j.upload != nil {
			_ = j.abortUpload(work.Err())
		}
	})
	m.jobs[g.ID] = j
	go func() {
		// Выполняет подготовку, дожидается уже запущенного прерывания загрузки, удаляет задание из Manager и
		// публикует завершение через j.done.

		j.result = m.execute(work, g, reader, j)
		if !stopUpload() {
			<-uploadClosed
		}
		cancel()
		m.mu.Lock()
		delete(m.jobs, g.ID)
		m.mu.Unlock()
		close(j.done)
	}()
	return j, nil
}

// ownedReader обеспечивает единственное закрытие принятого тела загрузки при конкурирующих путях
// завершения.
type ownedReader struct {
	// ReadCloser содержит исходное тело загрузки, использование которого прекращается перед
	// подтверждением передачи.
	io.ReadCloser
	// once допускает единственный вызов закрытия исходного тела.
	once sync.Once
	// err сохраняет результат единственного закрытия для всех последующих вызовов.
	err error
}

// Close закрывает переданный источник данных единственный раз и возвращает одну сохранённую ошибку
// всем вызывающим.
func (r *ownedReader) Close() error {
	r.once.Do(func() {
		// Единожды закрывает исходный ReadCloser и сохраняет его ошибку для всех последующих вызовов Close.
		r.err = r.ReadCloser.Close()
	})
	return r.err
}

// Захватывает слот подготовки, подтверждает выдачу и владельца до внешних действий, публикует полный
// комплект и отдельно подтверждает физическую очистку. Незавершённая область сохраняется для повторной
// сверки.
func (m *Manager) execute(ctx context.Context, g core.FallbackGeneration, reader io.ReadCloser, j *Job) (result Result) {
	result.Generation = g
	// fence содержит текущую точную идентичность попытки подготовки для доменных действий и очистки.
	fence := core.PreparationFenceFor(g)
	// owner принимает привязанного владельца допуска подготовки.
	var owner *core.PreparationOwner
	// finish освобождает зарегистрированную операцию владельца при завершении.
	var finish func()
	// scope принимает журналируемую область процессов текущей попытки.
	var scope *processowner.Scope
	// ticket принимает билет общей операции подготовки в области процессов.
	var ticket *processowner.Ticket
	// scopeAttempted отмечает возможное создание области даже при неоднозначном отказе без
	// возвращённого Scope.
	scopeAttempted := false
	defer func() {
		// Завершает загрузку, отзывает допуск и область подготовки и подтверждает очистку только после
		// проверки физического завершения; ошибки присоединяет к результату, а незакрытую область сохраняет
		// для повтора.

		if reader != nil {
			result.Err = errors.Join(result.Err, reader.Close())
			_ = j.upload.complete(ctx, result.Err)
		}
		// cleanup ограничивает независимую завершающую очистку. cancel освобождает её срок.
		cleanup, cancel := context.WithTimeout(context.Background(), m.config.ShutdownTimeout)
		defer cancel()
		// stored содержит сохранённое поколение для уточнения текущего допуска. identityErr
		// сообщает об отказе чтения при завершении.
		stored, identityErr := m.service.GetGeneration(cleanup, g.ID)
		if identityErr == nil && stored.AttemptToken == g.AttemptToken && stored.Incarnation == g.Incarnation {
			fence = core.PreparationFenceFor(stored)
		}
		if owner != nil {
			result.Err = errors.Join(result.Err, owner.Revoke(cleanup))
		}
		if finish != nil {
			finish()
		}
		if result.Err != nil {
			// current содержит поколение после отказа для проверки уже состоявшейся
			// публикации. err сообщает об отказе этого чтения.
			current, err := m.service.GetGeneration(cleanup, g.ID)
			if err == nil && current.State == core.PreparationPublished {
				result.Generation = current
			} else {
				result.Err = errors.Join(result.Err, err, m.service.CancelPreparation(cleanup, fence, result.Err.Error()))
			}
		}
		// physicalErr накапливает причины, по которым физическое завершение подготовки ещё не
		// подтверждено.
		var physicalErr error
		if scopeAttempted && scope == nil {
			physicalErr = ErrCleanupPending
		}
		if scope != nil {
			if ticket != nil {
				physicalErr = ticket.Complete(cleanup)
				if physicalErr == nil {
					ticket = nil
				}
			}
			// closeErr содержит результат закрытия области процессов.
			closeErr := scope.CloseContext(cleanup)
			if closeErr != nil {
				m.mu.Lock()
				m.retiredScopes[scope.ID()] = retiredPreparationScope{scope: scope, ticket: ticket, generationID: g.ID}
				m.mu.Unlock()
			}
			physicalErr = errors.Join(physicalErr, closeErr)
			// proof содержит сохранённое доказательство завершения области. proofErr
			// сообщает об отказе его получения.
			proof, proofErr := scope.Resolve(cleanup)
			physicalErr = errors.Join(physicalErr, proofErr)
			if !proof.Joined {
				physicalErr = errors.Join(physicalErr, ErrCleanupPending)
			}
		}
		if errors.Is(result.Err, processowner.ErrUnknown) || errors.Is(result.Err, processowner.ErrPending) || errors.Is(result.Err, ErrCleanupPending) {
			physicalErr = errors.Join(physicalErr, ErrCleanupPending)
		}
		if physicalErr != nil {
			result.Err = errors.Join(result.Err, physicalErr, ErrCleanupPending)
			return
		}
		// ack содержит результат доменного подтверждения очистки после физического завершения.
		ack := m.service.AcknowledgePreparationCleanup(cleanup, fence)
		result.Clean = ack == nil
		result.Err = errors.Join(result.Err, ack)
		if ack == nil && scope != nil {
			result.Err = errors.Join(result.Err, m.config.ProcessRegistry.AcknowledgeScope(cleanup, scope.ID()))
		}
	}()
	for {
		// err содержит результат очередной попытки захвата слота подготовки.
		var err error
		// claimed содержит выданный точный допуск занятого слота. claimErr сообщает об отказе
		// захвата.
		claimed, claimErr := m.service.ClaimPreparation(ctx, g.ID)
		err = claimErr
		if claimed.GenerationID != "" {
			fence = claimed
		}
		if err == nil {
			break
		}
		if !errors.Is(err, core.ErrBusy) {
			result.Err = err
			return result
		}
		// timer ограничивает паузу перед повторным захватом слота подготовки.
		timer := time.NewTimer(m.config.QueueInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			result.Err = ctx.Err()
			fence = core.PreparationFenceFor(g)
			return result
		case <-timer.C:
		}
	}
	// err сообщает об отказе фиксации выдачи подготовки до внешних действий.
	if err := m.service.MarkPreparationIssued(ctx, fence); err != nil {
		// Неоднозначная запись проверяется по идентичности до любого внешнего действия.
		// check ограничивает независимую проверку неоднозначной выдачи. stop освобождает её
		// срок.
		check, stop := context.WithTimeout(context.Background(), m.config.ShutdownTimeout)
		// current содержит поколение после неоднозначной выдачи. e сообщает об отказе повторного
		// чтения.
		current, e := m.service.GetGeneration(check, g.ID)
		stop()
		if e != nil || core.PreparationFenceFor(current) != fence || !current.Issued || current.State != core.PreparationIssued {
			result.Err = errors.Join(err, e)
			return result
		}
	}
	// err принимает ошибки привязки владельца, регистрации области, подготовки и публикации.
	var err error
	owner, err = m.service.BindPreparationOwner(ctx, fence)
	if err != nil {
		result.Err = err
		return result
	}
	j.mu.Lock()
	j.owner = owner
	j.mu.Unlock()
	finish, err = owner.Register(ctx)
	if err != nil {
		result.Err = err
		return result
	}
	// work связывает выполнение внешней работы с отзывом владельца. cancel останавливает эту работу.
	work, cancel := context.WithCancel(ctx)
	// watchDone закрывается после выхода наблюдателя отзыва владельца.
	watchDone := make(chan struct{})
	go func() {
		// Связывает отзыв владельца с отменой работы; при завершении работы пытается отозвать владельца с
		// отдельным сроком и закрывает watchDone.

		defer close(watchDone)
		select {
		case <-owner.Done():
			cancel()
		// cleanup ограничивает попытку отзыва владельца после остановки работы. stop освобождает
		// её срок.
		case <-work.Done():
			cleanup, stop := context.WithTimeout(context.Background(), m.config.ShutdownTimeout)
			_ = owner.Revoke(cleanup)
			stop()
		}
	}()
	defer func() {
		// Отменяет рабочий контекст и ждёт завершения наблюдателя владельца перед возвратом из execute.
		cancel()
		<-watchDone
	}()
	g.Attempt = fence.Attempt
	// tools содержит порты текущей попытки, заменяемые управляемыми рабочими инструментами при
	// наличии реестра.
	tools := m.tools
	if m.config.ProcessRegistry != nil {
		// key содержит JSON точного владельца области подготовки. keyErr сообщает об отказе
		// кодирования идентичности.
		key, keyErr := json.Marshal(preparationScopeOwner{1, "preparation", fence})
		if keyErr != nil {
			result.Err = keyErr
			return result
		}
		scopeAttempted = true
		scope, err = m.config.ProcessRegistry.Create(work, string(key))
		if err != nil {
			result.Err = err
			return result
		}
		ticket, err = scope.Register(work, "preparation")
		if err != nil {
			result.Err = err
			return result
		}
		// runner запускает внешние команды через журналируемую область текущей попытки.
		runner := Runner{Limits: m.config.Runner, Scope: scope}
		// probe проверяет и нормализует файлы через управляемый ffprobe текущей попытки.
		probe := Prober{Binary: m.config.FFprobe, Runner: runner, Limits: m.config.Probe}
		tools = Tools{Builder: ProductionBuilder(m.config.FFmpeg), Inspector: probe, Runner: runner, Normalize: probe.NormalizePrepared, VerifyLoopDuration: probe.VerifyLoopDuration}
	}
	// publication содержит проверенный исходник и полный комплект подготовленных вариантов.
	publication, err := m.prepare(work, g, reader, tools, j)
	if err != nil {
		result.Err = err
		return result
	}
	// published содержит поколение после успешной атомарной публикации комплекта.
	published, err := m.service.PublishPreparation(work, fence, publication)
	result.Err = err
	if err == nil {
		result.Generation = published
	}
	return result
}

// Сохраняет и проверяет новый исходный файл либо копию прежнего поколения, подтверждает передачу
// загрузки до инспекции и собирает все требуемые варианты для единой публикации.
func (m *Manager) prepare(ctx context.Context, g core.FallbackGeneration, reader io.ReadCloser, tools Tools, j *Job) (publication core.GenerationPublication, retErr error) {
	// err сообщает об отмене подготовки до файловых действий.
	if err := ctx.Err(); err != nil {
		return core.GenerationPublication{}, err
	}
	// err сообщает об отказе резервирования доказательств файлов поколения.
	if err := m.reserveGenerationFiles(ctx, g.ID); err != nil {
		return core.GenerationPublication{}, err
	}
	// directory содержит точную запись каталога поколения. err сообщает об отказе её поиска.
	directory, err := m.ownedFile(ctx, g.ID, core.GenerationDirectory(g.ID))
	if err != nil {
		return core.GenerationPublication{}, err
	}
	// err сообщает об отказе создания каталога с доказательством владения.
	if err := m.files.CreateOwnedGenerationContext(ctx, directory); err != nil {
		return core.GenerationPublication{}, err
	}
	// source выбирает тело новой загрузки либо дескриптор прежнего исходника для копирования.
	var source io.Reader = reader
	// opened при повторной подготовке содержит отдельно открытый и проверенный исходник прежнего
	// поколения.
	var opened *os.File
	if g.RebuildFromGenerationID != "" {
		// old содержит прежнее поколение для проверки пути, размера и SHA256 исходника. err
		// сообщает об отказе его чтения.
		old, err := m.service.GetGeneration(ctx, g.RebuildFromGenerationID)
		if err != nil {
			return core.GenerationPublication{}, err
		}
		opened, err = m.files.OpenExact(ctx, old.OriginalPath, old.SizeBytes, old.SourceHash)
		if err != nil {
			return core.GenerationPublication{}, err
		}
		defer func() {
			// Закрывает открытый исходник повторной подготовки и присоединяет ошибку к возвращаемому результату.
			retErr = errors.Join(retErr, opened.Close())
		}()
		source = opened
	}
	if source == nil {
		return core.GenerationPublication{}, ErrInvalid
	}
	// file содержит новый промежуточный исходник текущего поколения.
	file, err := m.createOwned(ctx, g.ID, g.StagingPath)
	if err != nil {
		return core.GenerationPublication{}, err
	}
	// copyErr содержит результат ограниченного копирования исходных данных.
	var copyErr error
	if reader != nil {
		// Close выполняется при отмене, а возврат ожидает завершения самого Close.
		// closed закрывается после завершения обработчика, закрывающего тело загрузки при
		// отмене.
		closed := make(chan struct{})
		// stop снимает обработчик отмены копирования либо сообщает, что его закрытия нужно
		// дождаться.
		stop := context.AfterFunc(ctx, func() {
			// При отмене копирования закрывает источник загрузки, игнорирует ошибку Close и сообщает через closed
			// об окончании закрытия.
			defer close(closed)
			_ = reader.Close()
		})
		_, copyErr = CopyBounded(ctx, file, source, m.config.MaxUploadBytes)
		if !stop() {
			<-closed
		}
	} else {
		_, copyErr = CopyBounded(ctx, file, source, m.config.MaxUploadBytes)
	}
	if copyErr != nil {
		return core.GenerationPublication{}, errors.Join(copyErr, file.Close())
	}
	// size содержит размер сохранённого исходника в байтах. hash содержит SHA256 его содержимого.
	size, hash, err := InspectFile(ctx, file, m.config.MaxUploadBytes)
	err = errors.Join(err, file.Sync(), file.Close())
	if err != nil {
		return core.GenerationPublication{}, err
	}
	if err = m.renameOwned(ctx, g.ID, g.StagingPath, g.OriginalPath); err != nil {
		return core.GenerationPublication{}, err
	}
	if j.upload != nil {
		if err = j.upload.complete(ctx, nil); err != nil {
			return core.GenerationPublication{}, err
		}
	}
	// input содержит независимо открытый исходник с проверенными размером и SHA256 для инспекции.
	input, err := m.files.OpenExact(ctx, g.OriginalPath, size, hash)
	if err != nil {
		return core.GenerationPublication{}, err
	}
	// observed содержит фактически проверенные вид, формат и длительность исходного файла.
	observed, err := tools.Inspector.Source(ctx, input, g.Kind)
	err = errors.Join(err, input.Close())
	if err != nil {
		return core.GenerationPublication{}, err
	}
	publication = core.GenerationPublication{SourceHash: hash, SizeBytes: size, SourceFormat: observed.Format}
	// required содержит очередной профиль, вариант которого обязателен для полного комплекта.
	for _, required := range g.RequiredProfiles {
		// v содержит выходную геометрию текущего требуемого профиля для проверки лимитов.
		v := required.Profile.Format.Video
		if v.Width > m.config.Probe.MaxWidth || v.Height > m.config.Probe.MaxHeight || uint64(v.Width)*uint64(v.Height) > m.config.Probe.MaxPixels {
			return publication, ErrLimit
		}
		// variant содержит полностью проверенный вариант текущего профиля. err сообщает об
		// отказе его создания.
		variant, err := m.variant(ctx, g, required, observed, size, hash, tools)
		if err != nil {
			return publication, err
		}
		publication.Variants = append(publication.Variants, variant)
	}
	return publication, nil
}

// Генерирует вариант с полной длиной петли, при наличии порта нормализует его и проверяет содержимое
// до конечного переименования; возвращает манифест с размером, SHA256 и фактическим форматом.
func (m *Manager) variant(ctx context.Context, g core.FallbackGeneration, p core.RequiredProfile, source Source, size uint64, hash string, tools Tools) (core.FallbackVariant, error) {
	// loop содержит длительность варианта на общей сетке видео и AAC. err сообщает о невозможности
	// уложить её в предел.
	loop, err := EffectiveLoopDuration(source.Duration, p.Profile, m.config.Probe.MaxDuration)
	if err != nil {
		return core.FallbackVariant{}, err
	}
	// command содержит команду генерации текущего варианта.
	command, err := tools.Builder(GenerateRequest{Source: source, Profile: p.Profile, LoopDuration: loop})
	if err != nil {
		return core.FallbackVariant{}, err
	}
	if err = ctx.Err(); err != nil {
		return core.FallbackVariant{}, err
	}
	// input содержит проверенный исходник с отдельным дескриптором для внешней команды.
	input, err := m.files.OpenExact(ctx, g.OriginalPath, size, hash)
	if err != nil {
		return core.FallbackVariant{}, err
	}
	// output содержит новый промежуточный файл варианта для ограниченной записи генератора.
	output, err := m.createOwned(ctx, g.ID, p.StagingPath)
	if err != nil {
		return core.FallbackVariant{}, errors.Join(err, input.Close())
	}
	_, err = tools.Runner.Run(ctx, command, input, &boundedWriter{w: output, remaining: m.config.MaxOutputBytes})
	err = errors.Join(err, input.Close())
	if err != nil {
		return core.FallbackVariant{}, errors.Join(err, output.Close())
	}
	// variantSize содержит размер варианта в байтах. digest содержит SHA256 его содержимого; после
	// нормализации оба значения пересчитываются.
	variantSize, digest, err := InspectFile(ctx, output, m.config.MaxOutputBytes)
	err = errors.Join(err, output.Sync(), output.Close())
	if err != nil {
		return core.FallbackVariant{}, err
	}
	// staging содержит путь файла, который проходит окончательную проверку и переименование.
	staging := p.StagingPath
	// normalize содержит необязательный порт нормализации выхода генератора.
	normalize := tools.Normalize
	if normalize != nil {
		staging = p.StagingPath + ".normalized"
		_, err = m.service.RegisterPreparationFile(ctx, core.PreparationFenceFor(g), core.OwnedFile{ManagedPath: staging, Kind: core.OwnedStaging})
		if err != nil {
			return core.FallbackVariant{}, err
		}
		// raw содержит проверенный ненормализованный вариант. e сообщает об отказе его открытия.
		raw, e := m.files.OpenExact(ctx, p.StagingPath, variantSize, digest)
		if e != nil {
			return core.FallbackVariant{}, e
		}
		// normalized содержит новый управляемый файл для результата нормализации.
		normalized, e := m.createOwned(ctx, g.ID, staging)
		if e != nil {
			return core.FallbackVariant{}, errors.Join(e, raw.Close())
		}
		err = normalize(ctx, raw, &boundedWriter{w: normalized, remaining: m.config.MaxOutputBytes})
		err = errors.Join(err, raw.Close())
		if err == nil {
			variantSize, digest, err = InspectFile(ctx, normalized, m.config.MaxOutputBytes)
		}
		err = errors.Join(err, normalized.Sync(), normalized.Close())
		if err != nil {
			return core.FallbackVariant{}, err
		}
	}
	// verify содержит отдельный проверенный дескриптор окончательного промежуточного варианта.
	verify, err := m.files.OpenExact(ctx, staging, variantSize, digest)
	if err != nil {
		return core.FallbackVariant{}, err
	}
	// actual содержит фактически подтверждённый формат готового варианта.
	actual, err := tools.Inspector.Verify(ctx, verify, p.Profile)
	if err == nil && tools.VerifyLoopDuration != nil {
		err = tools.VerifyLoopDuration(ctx, verify, loop)
	}
	err = errors.Join(err, verify.Close())
	if err != nil {
		return core.FallbackVariant{}, err
	}
	if staging != p.StagingPath {
		// raw содержит запись ненормализованного промежуточного файла для точного удаления. err
		// сообщает об отказе поиска.
		raw, err := m.ownedFile(ctx, g.ID, p.StagingPath)
		if err != nil {
			return core.FallbackVariant{}, err
		}
		if err = m.files.RemoveOwnedContext(ctx, raw); err != nil {
			return core.FallbackVariant{}, err
		}
	}
	if err = m.renameOwned(ctx, g.ID, staging, p.ManagedPath); err != nil {
		return core.FallbackVariant{}, err
	}
	return core.FallbackVariant{ID: p.VariantID, AssetID: g.AssetID, GenerationID: g.ID, Profile: p.Profile, ProfileKey: p.ProfileKey, CanonicalProfile: p.CanonicalProfile, ManagedPath: p.ManagedPath, GeneratorVersion: g.GeneratorVersion, SourceHash: hash, SizeBytes: variantSize, ContentHash: digest, VerifiedFormat: actual}, nil
}

// Close отзывает все задания; истечение срока не подтверждает физическое завершение.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.reconciliationClosed = true
	// jobs сохраняет снимок активных заданий перед отменой и ожиданием вне блокировки менеджера.
	jobs := make([]*Job, 0, len(m.jobs))
	// j обозначает очередное активное задание при создании снимка.
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	m.mu.Unlock()
	// j обозначает задание, которому запрашивается отмена перед ожиданием.
	for _, j := range jobs {
		j.cancel()
		j.mu.Lock()
		// owner сохраняет привязанного владельца задания для отзыва после снятия блокировки.
		owner := j.owner
		j.mu.Unlock()
		if owner != nil {
			_ = owner.Revoke(ctx)
		}
	}
	select {
	case <-m.reconcileGate:
		defer func() {
			// Возвращает маркер reconcileGate после ожидания заданий и повторной очистки областей, разрешая
			// следующий вызов закрытия.
			m.reconcileGate <- struct{}{}
		}()
	case <-ctx.Done():
		return errors.Join(ErrCleanupPending, ctx.Err())
	}
	// err накапливает ошибки завершённых заданий при закрытии менеджера.
	var err error
	// j обозначает очередное задание, завершения которого ждёт Close.
	for _, j := range jobs {
		// e содержит итог ожидания задания, включая неподтверждённую очистку.
		_, e := j.Wait(ctx)
		if errors.Is(e, ErrCleanupPending) {
			return errors.Join(err, e)
		}
		err = errors.Join(err, e)
	}
	return errors.Join(err, m.retryRetiredScopes(ctx))
}

// CloseAdmission немедленно запрещает новые Upload/Rebuild без ожидания текущих заданий.
func (m *Manager) CloseAdmission() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
}

// CleanupFile сверяет намерение с реестром файлов и подтверждает долговечность удаления.
func (m *Manager) CleanupFile(ctx context.Context, intent core.CleanupIntent) error {
	if intent.Kind != core.CleanupFile {
		return ErrInvalid
	}
	// stored содержит текущее намерение очистки для проверки точной идентичности. err сообщает об
	// отказе чтения.
	stored, err := m.service.GetCleanup(ctx, intent.ID)
	if err != nil {
		return err
	}
	if stored.Kind != core.CleanupFile || core.CleanupFenceFor(stored) != core.CleanupFenceFor(intent) {
		return core.ErrStale
	}
	if stored.Acknowledged {
		// file содержит запись файла уже подтверждённой очистки. lookupErr сообщает об отказе её
		// поиска.
		file, lookupErr := m.ownedFile(ctx, stored.ArtifactGenerationID, stored.ManagedPath)
		if lookupErr != nil {
			return lookupErr
		}
		if file.ID != stored.FileID || file.Kind != stored.FileKind {
			return core.ErrStale
		}
		return m.files.AcknowledgeOwnedContext(ctx, file)
	}
	// fence содержит точную идентичность неподтверждённого намерения очистки.
	fence := core.CleanupFenceFor(stored)
	// file содержит запись, удаление которой разрешено текущим допуском очистки.
	file, err := m.service.AuthorizeFileCleanup(ctx, fence)
	if err != nil {
		return err
	}
	if err = m.files.RemoveOwnedContext(ctx, file); err != nil {
		return err
	}
	if err = m.service.AcknowledgeCleanup(ctx, fence); err != nil {
		return err
	}
	return m.files.AcknowledgeOwnedContext(ctx, file)
}

// Ищет запись управляемого файла по точному пути во всех ограниченных страницах заданного поколения.
func (m *Manager) ownedFile(ctx context.Context, generation core.ID, name string) (core.OwnedFile, error) {
	// request задаёт страницу файлов одного поколения и последовательно переносит курсор.
	request := core.PageRequest{Query: core.ListQuery{Limit: m.config.ReconcileLimit, GenerationID: generation}}
	for {
		// page содержит очередную страницу реестра файлов поколения. err сообщает об отказе её
		// чтения.
		page, err := m.service.ListGenerationFiles(ctx, request)
		if err != nil {
			return core.OwnedFile{}, err
		}
		// file обозначает запись текущей страницы, путь которой сравнивается с искомым.
		for _, file := range page.Items {
			if file.ManagedPath == name {
				return file, nil
			}
		}
		if page.NextCursor == "" {
			return core.OwnedFile{}, core.ErrNotFound
		}
		request.Cursor = page.NextCursor
	}
}

// Резервирует доказательства всех записанных файлов поколения постранично до начала файловых действий.
func (m *Manager) reserveGenerationFiles(ctx context.Context, generation core.ID) error {
	// request задаёт ограниченные страницы файлов поколения для предварительного резервирования
	// доказательств.
	request := core.PageRequest{Query: core.ListQuery{Limit: m.config.ReconcileLimit, GenerationID: generation}}
	for {
		// page содержит очередную страницу файлов для резервирования. err сообщает об отказе её
		// чтения.
		page, err := m.service.ListGenerationFiles(ctx, request)
		if err != nil {
			return err
		}
		// file обозначает очередную запись, чья файловая идентичность резервируется до внешнего
		// действия.
		for _, file := range page.Items {
			if err = m.files.ReserveOwnedContext(ctx, file); err != nil {
				return err
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		request.Cursor = page.NextCursor
	}
}

// Находит точную запись файла поколения и создаёт его через журнал владения; дескриптор получает
// вызывающий.
func (m *Manager) createOwned(ctx context.Context, generation core.ID, name string) (*os.File, error) {
	// file содержит точную запись создаваемого пути. err сообщает об отказе её поиска.
	file, err := m.ownedFile(ctx, generation, name)
	if err != nil {
		return nil, err
	}
	return m.files.CreateOwnedContext(ctx, file)
}

// Находит точные записи исходного и конечного путей одного поколения и передаёт их журналу для
// доказуемого переименования.
func (m *Manager) renameOwned(ctx context.Context, generation core.ID, staging, final string) error {
	// from содержит запись исходного пути переименования. err сообщает об отказе её поиска.
	from, err := m.ownedFile(ctx, generation, staging)
	if err != nil {
		return err
	}
	// to содержит запись конечного пути того же поколения.
	to, err := m.ownedFile(ctx, generation, final)
	if err != nil {
		return err
	}
	return m.files.PublishOwnedRenameContext(ctx, from, to)
}
