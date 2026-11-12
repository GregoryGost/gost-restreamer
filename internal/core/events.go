package core

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"
)

// ErrEventSequenceExhausted обозначает невозможность продолжать журнал после исчерпания uint64
// последовательности.
var ErrEventSequenceExhausted = errors.New("исчерпана последовательность оперативных событий")

// EventLimits задаёт явные пределы ограниченного журнала оперативных событий.
type EventLimits struct {
	// MaxEvents ограничивает число одновременно удерживаемых событий.
	MaxEvents int
	// MaxBytes ограничивает общий логический вес журнала, а MaxEventBytes — вес отдельного события.
	MaxBytes, MaxEventBytes uint64
	// MaxPageSize ограничивает число событий одной возвращаемой страницы.
	MaxPageSize int
}

// Validate проверяет положительные пределы событий, страницы и логических байтов, включая безопасность
// суммарного расчёта веса.
func (l EventLimits) Validate() error {
	if l.MaxEvents <= 0 || l.MaxPageSize <= 0 || l.MaxPageSize > l.MaxEvents || l.MaxEventBytes < 256 || l.MaxEventBytes > l.MaxBytes || uint64(l.MaxEvents) > math.MaxUint64/l.MaxEventBytes {
		return ErrInvalid
	}
	return nil
}

// TelemetryLimits объединяет неизменяемые пределы событий, свежести медиа и системного GPU-снимка.
type TelemetryLimits struct {
	// MediaStaleAfter задаёт срок свежести медиаскоростей и прогресса FFmpeg.
	MediaStaleAfter time.Duration
	// Events задаёт бюджеты журнала и страницы оперативных событий.
	Events EventLimits
	// MaxGPUDevices ограничивает число устройств одного GPU-снимка.
	MaxGPUDevices int
	// MaxGPUUUIDBytes ограничивает длину UUID устройства, а MaxGPUBytes — логический вес полного
	// GPU-снимка.
	MaxGPUUUIDBytes, MaxGPUBytes uint64
}

// Validate проверяет очередь событий, положительный срок устаревания медиа и явные бюджеты снимка GPU
// без переполнения расчётов.
func (l TelemetryLimits) Validate() error {
	// err сообщает об ошибке проверки бюджета событий перед включением телеметрии.
	if err := l.Events.Validate(); err != nil {
		return err
	}
	if l.MediaStaleAfter <= 0 || l.MaxGPUDevices <= 0 || l.MaxGPUUUIDBytes == 0 || l.MaxGPUUUIDBytes > math.MaxUint64-1024 || uint64(l.MaxGPUDevices) > (math.MaxUint64-256)/(1024+l.MaxGPUUUIDBytes) || l.MaxGPUBytes < 256 {
		return ErrInvalid
	}
	return nil
}

// RuntimeCursor идентифицирует процесс, эпоху журнала и последнюю прочитанную последовательность.
type RuntimeCursor struct {
	// Incarnation идентифицирует владельца открытия хранилища, которому принадлежит журнал событий.
	Incarnation string
	// Epoch различает отдельные эпохи настройки журнала.
	Epoch ID
	// Sequence содержит номер последнего события в соответствующей эпохе.
	Sequence uint64
}

// RuntimeEventKind различает изменения состояния, измерений, входа, GPU и владельцев.
type RuntimeEventKind string

const (
	// RuntimeStateEvent обозначает изменение наблюдаемого состояния.
	RuntimeStateEvent RuntimeEventKind = "state"
	// RuntimeMetricsEvent обозначает новую медиавыборку или progress.
	RuntimeMetricsEvent RuntimeEventKind = "metrics"
	// RuntimeInputEvent обозначает подключение, замену или отключение OBS.
	RuntimeInputEvent RuntimeEventKind = "input"
	// RuntimeGPUEvent обозначает новый системный GPU-снимок.
	RuntimeGPUEvent RuntimeEventKind = "gpu"
	// RuntimeRemovedEvent обозначает удаление оперативного владельца или его снимка.
	RuntimeRemovedEvent RuntimeEventKind = "removed"
	// RuntimeAttemptEvent обозначает начало или закрытие попытки FFmpeg.
	RuntimeAttemptEvent RuntimeEventKind = "attempt"
)

// RuntimeOwnerKind различает компоненты, которым принадлежат оперативные события.
type RuntimeOwnerKind string

const (
	// RuntimeSessionOwner указывает владельца наблюдений сессии.
	RuntimeSessionOwner RuntimeOwnerKind = "session"
	// RuntimeGroupOwner указывает владельца наблюдений группы.
	RuntimeGroupOwner RuntimeOwnerKind = "group"
	// RuntimeOutputOwner указывает владельца наблюдений выхода.
	RuntimeOutputOwner RuntimeOwnerKind = "output"
	// RuntimeFFmpegOwner указывает владельца наблюдений FFmpeg.
	RuntimeFFmpegOwner RuntimeOwnerKind = "ffmpeg"
	// RuntimeSystemOwner указывает системного владельца GPU.
	RuntimeSystemOwner RuntimeOwnerKind = "system"
)

// RuntimeEvent содержит только разрешённые типизированные данные; текст ошибки остаётся в наблюдении.
type RuntimeEvent struct {
	// Cursor фиксирует позицию этого события в текущей эпохе.
	Cursor RuntimeCursor
	// Kind указывает вид изменения, представленного событием.
	Kind RuntimeEventKind
	// Owner указывает вид компонента-владельца события.
	Owner RuntimeOwnerKind
	// Session идентифицирует точное выполнение сессии события.
	Session RuntimeSessionFence
	// Stamp фиксирует локального владельца, ревизию и время наблюдения события.
	Stamp ObservationStamp
	// Execution идентифицирует точную попытку выхода, если событие относится к выходу.
	Execution ExecutionFence
	// GroupInstanceID идентифицирует оперативную группу-владельца работника FFmpeg.
	GroupInstanceID uint64
	// ConfigGroupID содержит исходную группу, а ConfigProfileID — исходный профиль для корреляции
	// оперативного владельца.
	ConfigGroupID, ConfigProfileID ID
	// State содержит наблюдаемое состояние сессии, группы или FFmpeg.
	State ObservationState
	// OutputState содержит наблюдаемое состояние соединения выхода.
	OutputState OutputState
	// Source содержит выбранный источник группы.
	Source ObservationSource
	// SourceRevision содержит ревизию выбора источника группы.
	SourceRevision uint64
	// Role содержит роль работника FFmpeg.
	Role FFmpegRole
	// AttemptActive отмечает начало активной попытки FFmpeg.
	AttemptActive bool
	// Reason содержит безопасную типизированную причину изменения без текста ошибки.
	Reason MetricReason
	// Input содержит состояние входа OBS и его измерения для соответствующего события.
	Input *InputSnapshot
	// Group содержит измерения трёх медиапутей группы.
	Group *GroupMetrics
	// Output содержит измерения публикации выхода.
	Output *OutputMetrics
	// FFmpeg содержит progress и счётчики потерь FFmpeg.
	FFmpeg *FFmpegMetrics
	// GPU содержит системный снимок GPU.
	GPU *GPUSnapshot
	// weight содержит логический вес удерживаемого события для учёта бюджета.
	weight uint64
}

// RuntimeEventPage возвращает ограниченную страницу событий с признаками пропуска и смены эпохи.
type RuntimeEventPage struct {
	// Events содержит независимые копии событий страницы в порядке последовательности.
	Events []RuntimeEvent
	// Cursor задаёт позицию продолжения после возвращённой страницы.
	Cursor RuntimeCursor
	// HighWater фиксирует верхнюю последовательность журнала на момент чтения страницы.
	HighWater RuntimeCursor
	// Gap сообщает о потерянных событиях, а Reset — о чужом процессе или эпохе исходного курсора.
	Gap, Reset bool
	// Changed закрывается при следующем изменении журнала событий.
	Changed <-chan struct{}
}

// RuntimeAnchor объединяет согласованный начальный снимок RAM с позицией событий.
type RuntimeAnchor struct {
	// Sessions содержит согласованные снимки всех представленных в RAM сессий.
	Sessions []SessionRuntimeSnapshot
	// GPU содержит копию системного GPU-снимка с учётом свежести.
	GPU *GPUSnapshot
	// Cursor фиксирует позицию событий, согласованную с начальным снимком.
	Cursor RuntimeCursor
	// Changed закрывается при изменении журнала после полученного снимка.
	Changed <-chan struct{}
}

// eventStream хранит ограниченный журнал текущей эпохи и границу потерянных событий.
type eventStream struct {
	// limits хранит неизменяемые бюджеты этой эпохи журнала.
	limits EventLimits
	// epoch идентифицирует отдельную эпоху журнала.
	epoch ID
	// sequence содержит последнюю назначенную последовательность, включая пропущенные события.
	sequence uint64
	// missingThrough сохраняет наибольшую последовательность утраченного события для обнаружения
	// разрывов.
	missingThrough uint64
	// bytes содержит суммарный логический вес удерживаемых событий.
	bytes uint64
	// events хранит оставшиеся события в порядке последовательности.
	events []RuntimeEvent
	// changed будит ожидающих клиентов при изменении журнала.
	changed chan struct{}
	// exhausted запрещает дальнейшее чтение после исчерпания последовательности.
	exhausted bool
}

// ConfigureTelemetry включает наблюдения до запуска пользователей RuntimeAuthority.
// Повтор с теми же пределами допустим; менять их после настройки запрещено.
func (a *RuntimeAuthority) ConfigureTelemetry(ctx context.Context, limits TelemetryLimits) error {
	// err сообщает об ошибке проверки полных явных пределов телеметрии.
	if err := limits.Validate(); err != nil {
		return err
	}
	// epoch содержит новую идентичность эпохи событий, а err — результат её создания.
	epoch, err := newID()
	if err != nil {
		return err
	}
	if err = a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if a.telemetry != nil {
		if *a.telemetry == limits {
			return nil
		}
		return ErrConflict
	}
	if len(a.observations) != 0 {
		return ErrConflict
	}
	a.telemetry = &limits
	a.events = &eventStream{limits: limits.Events, epoch: epoch, changed: make(chan struct{})}
	return nil
}

// ConfigureTelemetry включает телеметрию общей RuntimeAuthority с заданными неизменяемыми пределами.
func (s *Service) ConfigureTelemetry(ctx context.Context, limits TelemetryLimits) error {
	// a содержит общую RuntimeAuthority, а err — причину невозможности её получить.
	a, err := s.runtimeAuthority()
	if err != nil {
		return err
	}
	return a.ConfigureTelemetry(ctx, limits)
}

// cursorLocked возвращает текущую идентичность потока событий и номер последовательности; при
// выключенной телеметрии курсор нулевой.
func (a *RuntimeAuthority) cursorLocked() RuntimeCursor {
	if a.events == nil {
		return RuntimeCursor{}
	}
	return RuntimeCursor{Incarnation: a.incarnation, Epoch: a.events.epoch, Sequence: a.events.sequence}
}

// notifyEventsLocked будит текущих ожидающих и создаёт новый канал изменений под общей блокировкой
// RuntimeAuthority.
func (a *RuntimeAuthority) notifyEventsLocked() {
	close(a.events.changed)
	a.events.changed = make(chan struct{})
}

// ownerKind переводит вид узла наблюдений в тип владельца оперативного события.
func ownerKind(k observationKind) RuntimeOwnerKind {
	switch k {
	case observationSession:
		return RuntimeSessionOwner
	case observationGroup:
		return RuntimeGroupOwner
	case observationOutput:
		return RuntimeOutputOwner
	default:
		return RuntimeFFmpegOwner
	}
}

// nodeEvent формирует событие с идентичностью узла, его Session, ExecutionFence и конфигурационными
// ссылками без копирования нагрузки.
func (a *RuntimeAuthority) nodeEvent(n *observationNode, kind RuntimeEventKind) RuntimeEvent {
	return RuntimeEvent{Kind: kind, Owner: ownerKind(n.kind), Session: n.session, Stamp: n.stamp, Execution: n.execution, GroupInstanceID: n.parent, ConfigGroupID: n.groupID, ConfigProfileID: n.profileID, Role: n.role}
}

// appendEventLocked продвигает номер последовательности и сохраняет копию события в ограниченной
// очереди. Пропущенные и вытесненные события отмечаются границей разрыва, а переполнение номера
// закрывает дальнейшее чтение.
func (a *RuntimeAuthority) appendEventLocked(e RuntimeEvent) {
	if a.events == nil {
		return
	}
	// s содержит журнал, в который добавляется событие.
	s := a.events
	if s.sequence == math.MaxUint64 {
		if !s.exhausted {
			s.exhausted = true
			a.notifyEventsLocked()
		}
		return
	}
	s.sequence++
	e.Cursor = a.cursorLocked()
	// weight накапливает логический вес события с идентификаторами и типизированными данными.
	weight := uint64(256)
	// v содержит очередную удерживаемую строку для проверки и добавления её длины к весу.
	for _, v := range []string{e.Cursor.Incarnation, string(e.Cursor.Epoch), string(e.Session.SessionID), e.Session.Incarnation, string(e.Execution.CommandID), string(e.Execution.SessionID), string(e.Execution.OutputID), e.Execution.Incarnation, string(e.ConfigGroupID), string(e.ConfigProfileID)} {
		if uint64(len(v)) > math.MaxUint64-weight {
			weight = math.MaxUint64
			break
		}
		weight += uint64(len(v))
	}
	// add прибавляет вес вложенной нагрузки с насыщением, предотвращая обход бюджета при
	// переполнении.
	add := func(n uint64) {
		// Прибавляет вес вложенных данных к захваченному weight с насыщением до math.MaxUint64, чтобы
		// переполнение не обошло бюджет событий.

		if n > math.MaxUint64-weight {
			weight = math.MaxUint64
		} else {
			weight += n
		}
	}
	if e.Input != nil {
		add(inputTelemetryWeight)
	}
	if e.Group != nil {
		add(groupTelemetryWeight)
	}
	if e.Output != nil {
		add(outputTelemetryWeight)
	}
	if e.FFmpeg != nil {
		add(ffmpegTelemetryWeight)
	}
	if e.GPU != nil {
		// d содержит очередное GPU-устройство для учёта его UUID и показателей.
		for _, d := range e.GPU.Devices {
			add(1024 + uint64(len(d.UUID)))
		}
		add(256)
	}
	if weight > s.limits.MaxEventBytes || weight > s.limits.MaxBytes {
		s.missingThrough = s.sequence
		a.notifyEventsLocked()
		return
	}
	for len(s.events) > 0 && (len(s.events) >= s.limits.MaxEvents || s.bytes > s.limits.MaxBytes-weight) {
		// old содержит старейшее вытесняемое событие для снятия веса и отметки разрыва.
		old := s.events[0]
		s.bytes -= old.weight
		if old.Cursor.Sequence > s.missingThrough {
			s.missingThrough = old.Cursor.Sequence
		}
		copy(s.events, s.events[1:])
		s.events[len(s.events)-1] = RuntimeEvent{}
		s.events = s.events[:len(s.events)-1]
	}
	e.weight = weight
	s.events = append(s.events, cloneRuntimeEvent(e))
	s.bytes += weight
	a.notifyEventsLocked()
}

// cloneRuntimeEvent копирует строки и вложенные данные события, отделяя возвращённое значение от
// реестра.
func cloneRuntimeEvent(e RuntimeEvent) RuntimeEvent {
	e.Cursor.Incarnation = strings.Clone(e.Cursor.Incarnation)
	e.Cursor.Epoch = ID(strings.Clone(string(e.Cursor.Epoch)))
	e.Session = cloneRuntimeFence(e.Session)
	e.Execution = cloneExecutionFence(e.Execution)
	e.ConfigGroupID = ID(strings.Clone(string(e.ConfigGroupID)))
	e.ConfigProfileID = ID(strings.Clone(string(e.ConfigProfileID)))
	if e.Input != nil {
		e.Input = cloneInput(e.Input)
	}
	if e.Group != nil {
		// v содержит независимую копию измерений группы события.
		v := *e.Group
		e.Group = &v
	}
	if e.Output != nil {
		// v содержит независимую копию измерений выхода события.
		v := *e.Output
		e.Output = &v
	}
	if e.FFmpeg != nil {
		// v содержит независимую копию progress FFmpeg события.
		v := *e.FFmpeg
		e.FFmpeg = &v
	}
	if e.GPU != nil {
		e.GPU = freshGPU(e.GPU, e.GPU.SampledAt)
	}
	return e
}

// purgeSessionEventsLocked удаляет события точного RuntimeSessionFence и отмечает разрыв
// последовательности для клиентов.
func (a *RuntimeAuthority) purgeSessionEventsLocked(f RuntimeSessionFence) {
	if a.events == nil {
		return
	}
	// s содержит журнал, из которого удаляются события точной сессии.
	s := a.events
	// kept собирает сохраняемые события остальных владельцев в прежнем порядке.
	kept := s.events[:0]
	// i указывает очередное событие при удалении данных сессии.
	for i := range s.events {
		// e содержит проверяемое событие и его логический вес.
		e := s.events[i]
		if e.Session == f {
			s.bytes -= e.weight
			if e.Cursor.Sequence > s.missingThrough {
				s.missingThrough = e.Cursor.Sequence
			}
		} else {
			kept = append(kept, e)
		}
	}
	clear(s.events[len(kept):])
	s.events = kept
}

// runtimeEventsLocked возвращает ограниченную страницу после курсора, признак потери событий и канал
// изменений. Ненулевой курсор с другими Incarnation или Epoch требует Reset; будущий Sequence
// отклоняется только при совпадении Incarnation и Epoch.
func (a *RuntimeAuthority) runtimeEventsLocked(cursor RuntimeCursor, limit int) (RuntimeEventPage, error) {
	// s содержит текущую эпоху журнала для проверки курсора и построения страницы.
	s := a.events
	if s == nil {
		return RuntimeEventPage{}, ErrNotReady
	}
	if s.exhausted {
		return RuntimeEventPage{}, ErrEventSequenceExhausted
	}
	if limit <= 0 || limit > s.limits.MaxPageSize {
		return RuntimeEventPage{}, ErrInvalid
	}
	// current фиксирует текущую верхнюю границу журнала.
	current := a.cursorLocked()
	// p накапливает возвращаемые события, границы и признаки разрыва.
	p := RuntimeEventPage{Cursor: cursor, HighWater: current, Changed: s.changed}
	// zero показывает, запросил ли клиент чтение без прежнего курсора.
	zero := cursor == (RuntimeCursor{})
	if !zero && (cursor.Incarnation != current.Incarnation || cursor.Epoch != current.Epoch) {
		p.Gap = true
		p.Reset = true
		cursor = RuntimeCursor{}
	}
	if !zero && !p.Reset && cursor.Sequence > current.Sequence {
		return RuntimeEventPage{}, ErrInvalid
	}
	if cursor.Sequence < s.missingThrough {
		p.Gap = true
	}
	p.Cursor = RuntimeCursor{Incarnation: current.Incarnation, Epoch: current.Epoch, Sequence: cursor.Sequence}
	// e содержит очередное сохранённое событие для отбора после курсора.
	for _, e := range s.events {
		if e.Cursor.Sequence > cursor.Sequence {
			p.Events = append(p.Events, cloneRuntimeEvent(e))
			p.Cursor = e.Cursor
			if len(p.Events) == limit {
				return p, nil
			}
		}
	}
	p.Cursor = current
	return p, nil
}

// RuntimeEvents читает страницу оперативных событий общей RuntimeAuthority без обращения к долговечной
// истории.
func (s *Service) RuntimeEvents(ctx context.Context, cursor RuntimeCursor, limit int) (RuntimeEventPage, error) {
	// a содержит общую RuntimeAuthority, а err — причину невозможности её получить.
	a, err := s.runtimeAuthority()
	if err != nil {
		return RuntimeEventPage{}, err
	}
	return a.RuntimeEvents(ctx, cursor, limit)
}

// RuntimeEvents возвращает согласованную страницу оперативной очереди под общей блокировкой
// RuntimeAuthority с учётом контекста.
func (a *RuntimeAuthority) RuntimeEvents(ctx context.Context, cursor RuntimeCursor, limit int) (RuntimeEventPage, error) {
	// err сообщает об ошибке ожидания общей блокировки перед чтением событий.
	if err := a.acquire(ctx); err != nil {
		return RuntimeEventPage{}, err
	}
	defer a.release()
	return a.runtimeEventsLocked(cursor, limit)
}
