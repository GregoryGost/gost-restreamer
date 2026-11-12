package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// ErrObservationLimit обозначает отказ изменения RAM-реестра из-за превышения явного бюджета.
var ErrObservationLimit = errors.New("исчерпан бюджет оперативных наблюдений")

// ObservationLimits ограничивает число владельцев и логический вес удерживаемых данных.
// Вес узла включает 256 байт и длины строк; при включении телеметрии
// дополнительно резервируются все её типизированные поля. Связь с родителем
// включена в постоянную часть. Это бюджет реестра, а не размер кучи Go.
type ObservationLimits struct {
	// MaxSessions ограничивает число наблюдаемых сессий и узлов их владельцев.
	MaxSessions int
	// MaxGroups ограничивает число оперативных групп.
	MaxGroups int
	// MaxOutputs ограничивает число наблюдателей выходов.
	MaxOutputs int
	// MaxFFmpegWorkers ограничивает число наблюдаемых работников FFmpeg.
	MaxFFmpegWorkers int
	// MaxIdentifierBytes ограничивает длину каждого удерживаемого идентификатора в байтах.
	MaxIdentifierBytes uint64
	// MaxErrorBytes ограничивает длину текущей диагностической ошибки в байтах.
	MaxErrorBytes uint64
	// MaxBytes ограничивает суммарный логический вес реестра без вытеснения.
	MaxBytes uint64
}

// Validate проверяет явные положительные бюджеты реестра наблюдений и безопасность расчёта их
// суммарного логического веса.
func (l ObservationLimits) Validate() error {
	if l.MaxSessions <= 0 || l.MaxGroups <= 0 || l.MaxOutputs <= 0 || l.MaxFFmpegWorkers <= 0 || l.MaxIdentifierBytes == 0 || l.MaxErrorBytes == 0 || l.MaxBytes < 256 || l.MaxIdentifierBytes > l.MaxBytes || l.MaxErrorBytes > l.MaxBytes {
		return ErrInvalid
	}
	// total накапливает максимальное число узлов для проверки переполнения бюджета.
	total := uint64(0)
	// n содержит очередной явный предел числа узлов.
	for _, n := range []int{l.MaxSessions, l.MaxGroups, l.MaxOutputs, l.MaxFFmpegWorkers} {
		if uint64(n) > (math.MaxUint64 - total) {
			return ErrInvalid
		}
		total += uint64(n)
	}
	if l.MaxIdentifierBytes > (math.MaxUint64-288)/8 || l.MaxErrorBytes > math.MaxUint64-288-8*l.MaxIdentifierBytes || total > math.MaxUint64/(288+8*l.MaxIdentifierBytes+l.MaxErrorBytes) {
		return ErrInvalid
	}
	return nil
}

// ObservationState описывает оперативное состояние медиакомпонента отдельно от сохраняемого жизненного
// цикла.
type ObservationState string

const (
	// ObservationUnknown обозначает явно неизвестное состояние компонента.
	ObservationUnknown ObservationState = "UNKNOWN"
	// ObservationStarting обозначает начало работы компонента.
	ObservationStarting ObservationState = "STARTING"
	// ObservationReady обозначает наблюдаемую готовность компонента.
	ObservationReady ObservationState = "READY"
	// ObservationRetrying обозначает повторную попытку работы компонента.
	ObservationRetrying ObservationState = "RETRYING"
	// ObservationFailed обозначает наблюдаемый отказ компонента.
	ObservationFailed ObservationState = "FAILED"
	// ObservationStopped обозначает наблюдаемую остановку компонента.
	ObservationStopped ObservationState = "STOPPED"
)

// valid распознаёт известные состояния оперативного наблюдения, включая UNKNOWN.
func (s ObservationState) valid() bool {
	switch s {
	case ObservationUnknown, ObservationStarting, ObservationReady, ObservationRetrying, ObservationFailed, ObservationStopped:
		return true
	}
	return false
}

// ObservationSource различает LIVE, заглушку и отсутствие выбранного источника.
type ObservationSource string

const (
	// ObservationSourceNone обозначает отсутствие выбранного источника группы.
	ObservationSourceNone ObservationSource = "NONE"
	// ObservationSourceLive обозначает выбор входного LIVE-потока.
	ObservationSourceLive ObservationSource = "LIVE"
	// ObservationSourceFallback обозначает выбор подготовленной заглушки.
	ObservationSourceFallback ObservationSource = "FALLBACK"
)

// SessionObservation передаёт наблюдаемое состояние сессии и текущую диагностику в RAM.
type SessionObservation struct {
	// State содержит наблюдаемое состояние компонента.
	State ObservationState
	// Error содержит текущую диагностическую ошибку компонента в RAM.
	Error string
}

// GroupObservation передаёт состояние группы и ревизию выбора её источника.
type GroupObservation struct {
	// SourceRevision содержит возрастающую ревизию выбора источника для отклонения поздних
	// наблюдений.
	SourceRevision uint64
	// State содержит наблюдаемое состояние компонента.
	State ObservationState
	// Source указывает текущий источник группы: LIVE, заглушку либо отсутствие.
	Source ObservationSource
	// Error содержит текущую диагностическую ошибку компонента в RAM.
	Error string
}

// OutputObservation передаёт наблюдаемое состояние соединения выхода и текущую диагностику.
type OutputObservation struct {
	// State содержит наблюдаемое состояние компонента.
	State OutputState
	// Error содержит текущую диагностическую ошибку компонента в RAM.
	Error string
}

// FFmpegObservation передаёт наблюдаемое состояние конкретной попытки FFmpeg.
type FFmpegObservation struct {
	// State содержит наблюдаемое состояние компонента.
	State ObservationState
	// Error содержит текущую диагностическую ошибку компонента в RAM.
	Error string
}

// ObservationStamp фиксирует локального владельца, ревизию и время принятого наблюдения.
type ObservationStamp struct {
	// Token идентифицирует конкретного локального владельца наблюдений.
	Token uint64
	// InstanceID идентифицирует отдельный оперативный экземпляр узла.
	InstanceID uint64
	// Revision содержит ревизию принятого состояния или измерения.
	Revision uint64
	// ObservedAt содержит время принятия наблюдения в UTC.
	ObservedAt time.Time
	// AttemptID идентифицирует текущую попытку Supervisor для FFmpeg.
	AttemptID uint64
}

// SessionObservationSnapshot сохраняет копию текущего состояния сессии с признаком отсутствия значения.
type SessionObservationSnapshot struct {
	// Stamp фиксирует владельца и ревизию наблюдаемого состояния.
	Stamp ObservationStamp
	// Value содержит копию принятого состояния либо nil до первого наблюдения.
	Value *SessionObservation
}

// GroupObservationSnapshot объединяет копии состояния и измерений одной оперативной группы.
type GroupObservationSnapshot struct {
	// Metrics содержит копию последнего измерения либо nil до первой выборки.
	Metrics *GroupMetrics
	// MetricsStamp фиксирует владельца и ревизию принятого измерения.
	MetricsStamp ObservationStamp
	// Stamp фиксирует владельца и ревизию наблюдаемого состояния.
	Stamp ObservationStamp
	// ConfigGroupID связывает оперативную группу с исходной конфигурацией для корреляции.
	ConfigGroupID ID
	// ConfigProfileID связывает оперативную группу с исходным профилем для корреляции.
	ConfigProfileID ID
	// Value содержит копию принятого состояния либо nil до первого наблюдения.
	Value *GroupObservation
}

// OutputObservationSnapshot объединяет состояние и измерения точной попытки выхода.
type OutputObservationSnapshot struct {
	// Metrics содержит копию последнего измерения либо nil до первой выборки.
	Metrics *OutputMetrics
	// MetricsStamp фиксирует владельца и ревизию принятого измерения.
	MetricsStamp ObservationStamp
	// Stamp фиксирует владельца и ревизию наблюдаемого состояния.
	Stamp ObservationStamp
	// Execution идентифицирует точную долговечную попытку выхода.
	Execution ExecutionFence
	// Value содержит копию принятого состояния либо nil до первого наблюдения.
	Value *OutputObservation
}

// FFmpegObservationSnapshot объединяет состояние и измерения попытки FFmpeg внутри группы.
type FFmpegObservationSnapshot struct {
	// Metrics содержит копию последнего измерения либо nil до первой выборки.
	Metrics *FFmpegMetrics
	// MetricsStamp фиксирует владельца и ревизию принятого измерения.
	MetricsStamp ObservationStamp
	// Role указывает назначение работника FFmpeg в группе.
	Role FFmpegRole
	// Stamp фиксирует владельца и ревизию наблюдаемого состояния.
	Stamp ObservationStamp
	// GroupInstanceID идентифицирует оперативную группу-владельца работника FFmpeg.
	GroupInstanceID uint64
	// Value содержит копию принятого состояния либо nil до первого наблюдения.
	Value *FFmpegObservation
}

// SessionRuntimeSnapshot возвращает согласованные копии оперативных наблюдений точной сессии.
type SessionRuntimeSnapshot struct {
	// Fence идентифицирует точное выполнение сессии, которому принадлежат наблюдения.
	Fence RuntimeSessionFence
	// Input содержит текущую публикацию OBS и её измерения, если телеметрия настроена.
	Input *InputSnapshot
	// Cursor содержит позицию оперативных событий в момент построения снимка.
	Cursor RuntimeCursor
	// CursorValid показывает доступность пригодной позиции событий без исчерпания
	// последовательности.
	CursorValid bool
	// Session содержит состояние наблюдателя сессии, если он зарегистрирован.
	Session *SessionObservationSnapshot
	// Groups содержит снимки оперативных групп в порядке локальных идентификаторов.
	Groups []GroupObservationSnapshot
	// Outputs содержит снимки наблюдаемых выходов в порядке локальных идентификаторов.
	Outputs []OutputObservationSnapshot
	// FFmpeg содержит снимки работников FFmpeg в порядке локальных идентификаторов.
	FFmpeg []FFmpegObservationSnapshot
}

// observationKind различает виды узлов реестра для бюджетов и построения снимков.
type observationKind uint8

const (
	// observationSession выбирает узел наблюдений сессии.
	observationSession observationKind = iota
	// observationGroup выбирает узел наблюдений оперативной группы.
	observationGroup
	// observationOutput выбирает узел наблюдений выхода.
	observationOutput
	// observationFFmpeg выбирает узел наблюдений работника FFmpeg.
	observationFFmpeg
)

// observationNode хранит владельца, состояние и измерения одного узла общего реестра.
type observationNode struct {
	// authority связывает узел с общим владельцем реестра и его блокировкой.
	authority *RuntimeAuthority
	// token идентифицирует точный локальный узел реестра.
	token uint64
	// parent идентифицирует родительский узел для удаления поддерева.
	parent uint64
	// kind выбирает бюджет и представление узла в снимке.
	kind observationKind
	// session связывает узел с точным выполнением сессии.
	session RuntimeSessionFence
	// execution сохраняет точную попытку выхода для проверки долговечного допуска.
	execution ExecutionFence
	// lease связывает действительность наблюдений с общим допуском исполнения.
	lease *RuntimeLease
	// groupID сохраняет конфигурационную группу для корреляции.
	groupID ID
	// profileID сохраняет конфигурационный профиль для корреляции.
	profileID ID
	// stamp хранит владельца, попытку и ревизию последнего принятого состояния.
	stamp ObservationStamp
	// state хранит последнее принятое состояние компонента.
	state string
	// source хранит последний принятый источник группы.
	source ObservationSource
	// failure хранит текущий текст диагностики узла.
	failure string
	// present различает отсутствие наблюдения и уже принятое состояние.
	present bool
	// weight содержит текущий логический вес узла для проверки общего бюджета.
	weight uint64
	// valid показывает, зарегистрирован ли ещё этот точный владелец.
	valid atomic.Bool
	// done закрывается при отзыве соответствующего владельца наблюдений.
	done chan struct{}
	// attemptDone закрывается при завершении или замене текущей попытки FFmpeg; nil означает
	// отсутствие активной попытки.
	attemptDone chan struct{}
	// sourceRevision хранит последнюю принятую ревизию выбора источника.
	sourceRevision uint64
	// input хранит текущую публикацию OBS и её метрики внутри узла сессии.
	input *InputSnapshot
	// groupMetrics хранит последнюю выборку трёх медиапутей группы.
	groupMetrics *GroupMetrics
	// outputMetrics хранит последнюю выборку соединения и очереди выхода.
	outputMetrics *OutputMetrics
	// ffmpegMetrics хранит последний прогресс текущей попытки FFmpeg.
	ffmpegMetrics *FFmpegMetrics
	// metricsStamp хранит владельца и ревизию последней принятой выборки.
	metricsStamp ObservationStamp
	// role указывает назначение работника FFmpeg.
	role FFmpegRole
	// sampleTimes хранит локальное время приёма медиавыборок для проверки устаревания.
	sampleTimes [3]time.Time
	// progressTime хранит локальное время приёма нового прогресса FFmpeg.
	progressTime time.Time
}

// observerHandle ссылается на точный узел и предоставляет проверку и отзыв его допуска.
type observerHandle struct { /* node ссылается на точный узел общего реестра наблюдений. */
	node *observationNode
}

// SessionObserver разрешает наблюдения точной сессии и регистрацию её групп.
type SessionObserver struct { /* observerHandle предоставляет общий дескриптор точного узла наблюдений. */
	observerHandle
}

// GroupObserver разрешает наблюдения отдельной оперативной группы и её работников FFmpeg.
type GroupObserver struct { /* observerHandle предоставляет общий дескриптор точного узла наблюдений. */
	observerHandle
}

// OutputObserver разрешает наблюдения точной попытки выхода.
type OutputObserver struct { /* observerHandle предоставляет общий дескриптор точного узла наблюдений. */
	observerHandle
}

// FFmpegObserver сохраняет владельца работника FFmpeg между отдельными попытками.
type FFmpegObserver struct { /* observerHandle предоставляет общий дескриптор точного узла наблюдений. */
	observerHandle
}

// FFmpegAttemptObserver ограничивает наблюдения FFmpeg конкретной незакрытой попыткой.
type FFmpegAttemptObserver struct {
	// observerHandle предоставляет общий дескриптор точного узла наблюдений.
	observerHandle
	// attemptID ограничивает дескриптор конкретной попыткой FFmpeg.
	attemptID uint64
	// done закрывается при отзыве соответствующего владельца наблюдений.
	done <-chan struct{}
}

// Token возвращает локальный идентификатор владельца наблюдения; пустой дескриптор даёт 0.
func (h observerHandle) Token() uint64 {
	if h.node == nil {
		return 0
	}
	return h.node.token
}

// Valid проверяет допуск узла наблюдений и связанного RuntimeLease.
func (h observerHandle) Valid() bool {
	return h.node != nil && h.node.valid.Load() && h.node.lease.Valid()
}

// Done возвращает канал удаления узла наблюдений; пустой дескриптор даёт nil.
func (h observerHandle) Done() <-chan struct{} {
	if h.node == nil {
		return nil
	}
	return h.node.done
}

// Close удаляет собственное поддерево наблюдений под общей блокировкой RuntimeAuthority, не
// подтверждая физическую остановку ресурсов.
func (h observerHandle) Close(ctx context.Context) error {
	if h.node == nil {
		return ErrInvalid
	}
	// a предоставляет общую блокировку для удаления поддерева наблюдений.
	a := h.node.authority
	// err сообщает об ошибке ожидания общей блокировки перед закрытием наблюдателя.
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	a.removeObservation(h.node)
	return nil
}

// ExecutionFence возвращает точную попытку наблюдаемого выхода; отсутствующий наблюдатель даёт нулевое
// значение.
func (o *OutputObserver) ExecutionFence() ExecutionFence {
	if o == nil || o.node == nil {
		return ExecutionFence{}
	}
	return o.node.execution
}

// Valid требует действующего наблюдателя FFmpeg и незакрытого канала конкретной попытки FFmpeg.
func (o *FFmpegAttemptObserver) Valid() bool {
	if o == nil || !o.observerHandle.Valid() {
		return false
	}
	select {
	case <-o.done:
		return false
	default:
		return true
	}
}

// Done возвращает канал завершения либо замены конкретной попытки FFmpeg.
func (o *FFmpegAttemptObserver) Done() <-chan struct{} {
	if o == nil {
		return nil
	}
	return o.done
}

// Close закрывает наблюдения только текущей попытки FFmpeg, очищает её состояние и метрики и публикует
// событие; FFmpegObserver остаётся для следующего BeginAttempt.
func (o *FFmpegAttemptObserver) Close(ctx context.Context) error {
	if o == nil || o.node == nil {
		return ErrInvalid
	}
	// a предоставляет общую блокировку для закрытия точной попытки FFmpeg.
	a := o.node.authority
	// err сообщает об ошибке ожидания общей блокировки перед закрытием попытки FFmpeg.
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	// err сообщает об ошибке проверки, что закрывается текущая попытка FFmpeg.
	if err := o.current(); err != nil {
		return err
	}
	close(o.node.attemptDone)
	o.node.attemptDone = nil
	o.node.sampleTimes = [3]time.Time{}
	o.node.progressTime = time.Time{}
	o.node.ffmpegMetrics = nil
	o.node.metricsStamp = ObservationStamp{}
	o.node.present = false
	o.node.state = ""
	o.node.failure = ""
	o.node.stamp.ObservedAt = time.Time{}
	o.node.weight, _ = a.observationWeight(o.node)
	a.appendEventLocked(a.nodeEvent(o.node, RuntimeAttemptEvent))
	return nil
}

// current под общей блокировкой RuntimeAuthority проверяет допуск наблюдателя FFmpeg и совпадение
// незакрытой попытки.
func (o *FFmpegAttemptObserver) current() error {
	if !o.observerHandle.Valid() || o.node.stamp.AttemptID != o.attemptID || o.node.attemptDone == nil {
		return ErrStale
	}
	return nil
}

// cloneRuntimeFence копирует строки идентичности Session, чтобы реестр не удерживал чужие строковые
// буферы.
func cloneRuntimeFence(f RuntimeSessionFence) RuntimeSessionFence {
	f.SessionID = ID(strings.Clone(string(f.SessionID)))
	f.Incarnation = strings.Clone(f.Incarnation)
	return f
}

// cloneExecutionFence копирует строки точной идентичности команды и её владельца.
func cloneExecutionFence(f ExecutionFence) ExecutionFence {
	f.CommandID = ID(strings.Clone(string(f.CommandID)))
	f.SessionID = ID(strings.Clone(string(f.SessionID)))
	f.OutputID = ID(strings.Clone(string(f.OutputID)))
	f.Incarnation = strings.Clone(f.Incarnation)
	return f
}

// strings перечисляет удерживаемые узлом строки в порядке расчёта веса; первые восемь учитываются как
// идентификаторы.
func (n *observationNode) strings() []string {
	return []string{string(n.session.SessionID), n.session.Incarnation, string(n.execution.CommandID), string(n.execution.SessionID), string(n.execution.OutputID), n.execution.Incarnation, string(n.groupID), string(n.profileID), n.state, string(n.source), n.failure}
}

// observationWeight рассчитывает логический вес узла с резервом телеметрии и проверяет длины
// идентификаторов, ошибки и переполнение.
func (a *RuntimeAuthority) observationWeight(n *observationNode) (uint64, error) {
	// weight накапливает постоянный вес узла, резерв телеметрии и длины строк.
	weight := uint64(256)
	if a.telemetry != nil {
		weight += telemetryNodeWeight(n.kind)
	}
	// i указывает позицию удерживаемой строки, а s содержит эту строку для проверки её длины и веса.
	for i, s := range n.strings() {
		if i < 8 && uint64(len(s)) > a.limits.MaxIdentifierBytes {
			return 0, ErrObservationLimit
		}
		if uint64(len(s)) > math.MaxUint64-weight {
			return 0, ErrObservationLimit
		}
		weight += uint64(len(s))
	}
	if uint64(len(n.failure)) > a.limits.MaxErrorBytes {
		return 0, ErrObservationLimit
	}
	return weight, nil
}

// budgetObservation проверяет бюджеты после предполагаемого добавления candidate и исключения removed,
// сохраняя реестр при отказе.
func (a *RuntimeAuthority) budgetObservation(candidate *observationNode, removed map[uint64]bool) error {
	// weight содержит вес нового узла, а err — результат проверки его строковых ограничений.
	weight, err := a.observationWeight(candidate)
	if err != nil {
		return err
	}
	// counts считает предполагаемые узлы каждого вида после замены.
	counts := [4]int{}
	// sessions собирает точные сессии, представленные после предполагаемой замены.
	sessions := map[RuntimeSessionFence]bool{}
	counts[candidate.kind]++
	sessions[candidate.session] = true
	// token идентифицирует существующий узел, а n содержит его данные для расчёта оставшегося
	// бюджета.
	for token, n := range a.observations {
		if removed[token] {
			continue
		}
		if n.weight > math.MaxUint64-weight {
			return ErrObservationLimit
		}
		weight += n.weight
		counts[n.kind]++
		sessions[n.session] = true
	}
	if len(sessions) > a.limits.MaxSessions || counts[observationSession] > a.limits.MaxSessions || counts[observationGroup] > a.limits.MaxGroups || counts[observationOutput] > a.limits.MaxOutputs || counts[observationFFmpeg] > a.limits.MaxFFmpegWorkers || weight > a.limits.MaxBytes {
		return ErrObservationLimit
	}
	return nil
}

// observationSubtree находит узел и потомков; для Session включает все наблюдения её точного
// RuntimeSessionFence.
func (a *RuntimeAuthority) observationSubtree(n *observationNode) map[uint64]bool {
	// removed собирает идентификаторы узла и всего удаляемого поддерева.
	removed := map[uint64]bool{n.token: true}
	if n.kind == observationSession {
		// token идентифицирует проверяемый узел, а other содержит его принадлежность точной
		// сессии.
		for token, other := range a.observations {
			if other.session == n.session {
				removed[token] = true
			}
		}
		return removed
	}
	// changed показывает, найдены ли новые потомки при очередном проходе реестра.
	for changed := true; changed; {
		changed = false
		// token идентифицирует проверяемый узел, а other содержит его родителя для поиска
		// потомков.
		for token, other := range a.observations {
			if !removed[token] && removed[other.parent] {
				removed[token] = true
				changed = true
			}
		}
	}
	return removed
}

// removeObservation под общей блокировкой RuntimeAuthority отзывает точное поддерево, закрывает каналы
// и стирает значения; удаление Output также закрывает допуск его физического резерва.
func (a *RuntimeAuthority) removeObservation(n *observationNode) {
	if n == nil || a.observations[n.token] != n {
		return
	}
	if n.kind == observationSession {
		a.purgeSessionEventsLocked(n.session)
	}
	// token идентифицирует очередной удаляемый узел поддерева.
	for token := range a.observationSubtree(n) {
		// other содержит удаляемый узел для отзыва допуска и очистки значений.
		other := a.observations[token]
		if other == nil {
			continue
		}
		// reservation содержит физический резерв выхода, который отзывается вместе с его
		// наблюдателем.
		for _, reservation := range a.outputExecutions {
			if other.kind == observationOutput && reservation.fence == other.execution {
				reservation.sealLocked()
			}
		}
		delete(a.observations, token)
		other.valid.Store(false)
		close(other.done)
		if other.attemptDone != nil {
			close(other.attemptDone)
			other.attemptDone = nil
		}
		other.failure = ""
		other.state = ""
		other.present = false
		other.input = nil
		other.groupMetrics = nil
		other.outputMetrics = nil
		other.ffmpegMetrics = nil
		other.metricsStamp = ObservationStamp{}
		other.sampleTimes = [3]time.Time{}
		other.progressTime = time.Time{}
	}
	// e содержит оперативное событие удаления узла.
	e := a.nodeEvent(n, RuntimeRemovedEvent)
	e.Stamp.ObservedAt = time.Now().UTC()
	a.appendEventLocked(e)
}

// registerObservation проверяет бюджет до копирования и замены, назначает новый идентификатор и
// регистрирует узел без значения наблюдения; прежнее поддерево при отказе сохраняется.
func (a *RuntimeAuthority) registerObservation(n *observationNode, replace *observationNode) (*observationNode, error) {
	// removed перечисляет узлы, исключаемые при проверке бюджета замены.
	removed := map[uint64]bool{}
	if replace != nil {
		removed = a.observationSubtree(replace)
	}
	// err сообщает об ошибке проверки бюджета до регистрации и отзыва прежнего владельца.
	if err := a.budgetObservation(n, removed); err != nil {
		return nil, err
	}
	if a.nextObservation == math.MaxUint64 {
		return nil, fmt.Errorf("%w: исчерпаны идентификаторы наблюдений", ErrConflict)
	}
	a.nextObservation++
	n.token = a.nextObservation
	n.authority = a
	n.stamp = ObservationStamp{Token: n.token, InstanceID: n.token}
	n.done = make(chan struct{})
	n.session = cloneRuntimeFence(n.session)
	n.execution = cloneExecutionFence(n.execution)
	n.groupID = ID(strings.Clone(string(n.groupID)))
	n.profileID = ID(strings.Clone(string(n.profileID)))
	n.weight, _ = a.observationWeight(n)
	if replace != nil {
		a.removeObservation(replace)
	}
	n.valid.Store(true)
	a.observations[n.token] = n
	if a.telemetry != nil && n.kind == observationSession {
		n.input = &InputSnapshot{}
		// publication содержит существующий допуск OBS для восстановления связи входа с новым
		// наблюдателем сессии.
		for _, publication := range a.publications {
			if publication.session == n.session && publication.Valid() {
				a.connectInputNodeLocked(n, publication)
			}
		}
	}
	return n, nil
}

// openObserver под общей блокировкой RuntimeAuthority сверяет сохраняемое разрешение и создаёт нового
// владельца наблюдений, заменяя прежнего того же вида; Output требует успешной квитанции запуска.
func (s *Service) openObserver(ctx context.Context, f ExecutionFence, kind observationKind) (*observationNode, error) {
	// err содержит причину отказа проверки точной идентичности исполнения.
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if (kind == observationSession) != (f.OutputID == "") {
		return nil, ErrInvalid
	}
	// a содержит общую RuntimeAuthority, а err — причину невозможности её получить.
	a, err := s.runtimeAuthority()
	if err != nil {
		return nil, err
	}
	if err = a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	// Проверка длины предшествует копированию идентификаторов и созданию допуска.
	// candidate содержит ещё не зарегистрированный узел для проверки длин и бюджета.
	candidate := &observationNode{kind: kind, session: RuntimeSessionFence{f.SessionID, f.Incarnation, f.Lifecycle}, execution: f}
	if _, err = a.observationWeight(candidate); err != nil {
		return nil, err
	}
	// lease содержит допуск проверяемого исполнения до привязки к существующему владельцу.
	lease := &RuntimeLease{authority: a, session: candidate.session, execution: f, done: make(chan struct{})}
	err = s.uow.View(ctx, func(r Repositories) error {
		// Под общей блокировкой сверяет сохранённое разрешение наблюдателя; для выхода дополнительно требует
		// успешную квитанцию той же попытки.

		// err сообщает об ошибке проверки долговечного разрешения наблюдателя.
		if err := lease.authorized(ctx, r); err != nil {
			return err
		}
		if kind == observationOutput {
			// c содержит квитанцию запуска выхода, а err — результат её чтения.
			c, err := r.Runtime.ReceiptByID(ctx, f.CommandID)
			if err != nil {
				return err
			}
			if c.Status != CommandSucceeded {
				return ErrStale
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// replace указывает прежнего владельца того же вида, который будет заменён после проверки
	// бюджета.
	var replace *observationNode
	// n содержит очередного владельца для поиска заменяемого узла.
	for _, n := range a.observations {
		if n.kind == kind && n.session == candidate.session && (kind == observationSession || n.execution.OutputID == f.OutputID) {
			replace = n
		}
	}
	// existing содержит существующий допуск для повторного использования точного исполнения.
	for _, existing := range a.leases {
		if existing.execution == f {
			lease = existing
			break
		}
	}
	if !lease.valid.Load() {
		// id содержит новый идентификатор допуска, а e — результат его создания.
		id, e := newID()
		if e != nil {
			return nil, e
		}
		lease.id = id
	}
	candidate.lease = lease
	// node содержит зарегистрированный узел после проверки бюджета и замены прежнего.
	node, err := a.registerObservation(candidate, replace)
	if err != nil {
		return nil, err
	}
	if !lease.valid.Load() {
		lease.session = node.session
		lease.execution = node.execution
		lease.valid.Store(true)
		a.leases[lease.id] = lease
	}
	return node, nil
}

// OpenSessionObserver открывает нового владельца наблюдений точного исполнения Session; прежний
// идентификатор этой сессии отзывается вместе с поддеревом.
func (s *Service) OpenSessionObserver(ctx context.Context, f ExecutionFence) (*SessionObserver, error) {
	// n содержит нового владельца наблюдений сессии, а err — результат его регистрации.
	n, err := s.openObserver(ctx, f, observationSession)
	if err != nil {
		return nil, err
	}
	return &SessionObserver{observerHandle{n}}, nil
}

// OpenOutputObserver открывает наблюдения успешно запущенной точной попытки выхода и заменяет прежний
// идентификатор только этого выхода.
func (s *Service) OpenOutputObserver(ctx context.Context, f ExecutionFence) (*OutputObserver, error) {
	// n содержит нового владельца наблюдений выхода, а err — результат его регистрации.
	n, err := s.openObserver(ctx, f, observationOutput)
	if err != nil {
		return nil, err
	}
	return &OutputObserver{observerHandle{n}}, nil
}

// child регистрирует новый дочерний узел только при действующем родителе, наследуя владельца Session и
// RuntimeLease.
func (h observerHandle) child(ctx context.Context, kind observationKind, group, profile ID, role FFmpegRole) (*observationNode, error) {
	if h.node == nil {
		return nil, ErrInvalid
	}
	// a предоставляет общую блокировку для регистрации дочернего узла.
	a := h.node.authority
	// err сообщает об ошибке ожидания общей блокировки перед созданием дочернего узла.
	if err := a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	if !h.Valid() {
		return nil, ErrStale
	}
	return a.registerObservation(&observationNode{kind: kind, parent: h.node.token, session: h.node.session, lease: h.node.lease, groupID: group, profileID: profile, role: role}, nil)
}

// OpenGroup создаёт отдельную оперативную идентичность группы с конфигурационными ID для корреляции;
// существующие группы не объединяются.
func (o *SessionObserver) OpenGroup(ctx context.Context, group, profile ID) (*GroupObserver, error) {
	if o == nil {
		return nil, ErrInvalid
	}
	// err содержит причину отказа проверки конфигурационной группы.
	if err := group.Validate(); err != nil {
		return nil, err
	}
	// err содержит причину отказа проверки конфигурационного профиля.
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	// n содержит новый дочерний узел группы, а err — результат его регистрации.
	n, err := o.child(ctx, observationGroup, group, profile, "")
	if err != nil {
		return nil, err
	}
	return &GroupObserver{observerHandle{n}}, nil
}

// OpenFFmpeg регистрирует дочернего наблюдателя FFmpeg без назначения роли и без активной попытки.
func (o *GroupObserver) OpenFFmpeg(ctx context.Context) (*FFmpegObserver, error) {
	if o == nil {
		return nil, ErrInvalid
	}
	// n содержит новый узел работника FFmpeg, а err — результат его регистрации.
	n, err := o.child(ctx, observationFFmpeg, "", "", "")
	if err != nil {
		return nil, err
	}
	return &FFmpegObserver{observerHandle{n}}, nil
}

// BeginAttempt принимает строго возрастающий положительный ID попытки Supervisor, отзывает наблюдателя
// прежней попытки и очищает её состояние и метрики.
func (o *FFmpegObserver) BeginAttempt(ctx context.Context, attemptID uint64) (*FFmpegAttemptObserver, error) {
	if o == nil || o.node == nil || attemptID == 0 {
		return nil, ErrInvalid
	}
	// a предоставляет общую блокировку смены попытки FFmpeg.
	a := o.node.authority
	// err сообщает об ошибке ожидания общей блокировки перед сменой попытки FFmpeg.
	if err := a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	if !o.Valid() || attemptID <= o.node.stamp.AttemptID {
		return nil, ErrStale
	}
	if o.node.attemptDone != nil {
		close(o.node.attemptDone)
	}
	o.node.attemptDone = make(chan struct{})
	o.node.stamp.AttemptID = attemptID
	o.node.stamp.ObservedAt = time.Time{}
	o.node.sampleTimes = [3]time.Time{}
	o.node.progressTime = time.Time{}
	o.node.ffmpegMetrics = nil
	o.node.metricsStamp = ObservationStamp{}
	o.node.present = false
	o.node.failure = ""
	o.node.state = ""
	o.node.weight, _ = a.observationWeight(o.node)
	// e содержит событие начала новой попытки FFmpeg.
	e := a.nodeEvent(o.node, RuntimeAttemptEvent)
	e.AttemptActive = true
	a.appendEventLocked(e)
	return &FFmpegAttemptObserver{observerHandle: o.observerHandle, attemptID: attemptID, done: o.node.attemptDone}, nil
}

// observe под общей блокировкой RuntimeAuthority проверяет владельца, попытку, порядок SourceRevision
// и бюджет, затем обновляет RAM-состояние и ревизию; событие создаётся только при изменении значения.
func (h observerHandle) observe(ctx context.Context, state string, source ObservationSource, failure string, attempt uint64, sourceRevision uint64) error {
	if h.node == nil {
		return ErrInvalid
	}
	// a предоставляет общую блокировку проверки и записи наблюдаемого состояния.
	a := h.node.authority
	// err сообщает об ошибке ожидания общей блокировки перед записью наблюдения.
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if !h.Valid() || (h.node.kind == observationFFmpeg && (h.node.stamp.AttemptID != attempt || h.node.attemptDone == nil)) {
		return ErrStale
	}
	if (sourceRevision > 0 && sourceRevision <= h.node.sourceRevision) || (h.node.kind == observationGroup && h.node.sourceRevision > 0 && sourceRevision == 0) {
		return ErrStale
	}
	if !utf8.ValidString(failure) || strings.ContainsRune(failure, 0) {
		return ErrInvalid
	}
	// n содержит точный узел, состояние которого обновляется.
	n := h.node
	// changed показывает, отличается ли принятое состояние, источник или диагностика от прежнего
	// значения.
	changed := !n.present || n.state != state || n.source != source || n.failure != failure
	// candidate содержит предполагаемое новое состояние для проверки бюджета без изменения прежнего
	// узла.
	candidate := &observationNode{kind: n.kind, session: n.session, execution: n.execution, groupID: n.groupID, profileID: n.profileID, state: state, source: source, failure: failure}
	// err сообщает об ошибке проверки бюджета замены текущего значения.
	if err := a.budgetObservation(candidate, map[uint64]bool{n.token: true}); err != nil {
		return err
	}
	// revision содержит следующую ревизию наблюдения, а err — признак исчерпания ревизий.
	revision, err := NextRevision(n.stamp.Revision)
	if err != nil {
		return err
	}
	n.state = strings.Clone(state)
	n.source = ObservationSource(strings.Clone(string(source)))
	n.failure = strings.Clone(failure)
	n.present = true
	n.stamp.Revision = revision
	n.stamp.ObservedAt = time.Now().UTC()
	if sourceRevision > 0 {
		n.sourceRevision = sourceRevision
	}
	if changed {
		// e содержит событие изменившегося состояния узла.
		e := a.nodeEvent(n, RuntimeStateEvent)
		e.Source = n.source
		e.SourceRevision = n.sourceRevision
		if n.kind == observationOutput {
			e.OutputState = OutputState(n.state)
		} else {
			e.State = ObservationState(n.state)
		}
		if failure != "" {
			e.Reason = ReasonObservationFailed
		}
		a.appendEventLocked(e)
	}
	n.weight, _ = a.observationWeight(n)
	return nil
}

// Observe сохраняет допустимое оперативное состояние Session в RAM, не меняя её долговечный жизненный
// цикл.
func (o *SessionObserver) Observe(ctx context.Context, v SessionObservation) error {
	if o == nil || !v.State.valid() {
		return ErrInvalid
	}
	return o.observe(ctx, string(v.State), "", v.Error, 0, 0)
}

// Observe сохраняет состояние и источник группы в RAM с проверкой допустимого источника и порядка
// SourceRevision.
func (o *GroupObserver) Observe(ctx context.Context, v GroupObservation) error {
	if o == nil || !v.State.valid() || (v.Source != ObservationSourceNone && v.Source != ObservationSourceLive && v.Source != ObservationSourceFallback) {
		return ErrInvalid
	}
	return o.observe(ctx, string(v.State), v.Source, v.Error, 0, v.SourceRevision)
}

// Observe принимает CONNECTING, PUBLISHING или RETRYING точного действующего выхода; терминальное
// состояние публикуется отдельно.
func (o *OutputObserver) Observe(ctx context.Context, v OutputObservation) error {
	if o == nil || (v.State != OutputConnecting && v.State != OutputPublishing && v.State != OutputRetrying) {
		return ErrInvalid
	}
	return o.observe(ctx, string(v.State), "", v.Error, 0, 0)
}

// ObserveTerminal публикует терминальное RAM-состояние, не освобождая физическое выполнение.
// Долговечный отказ и очистка остаются отдельными операциями владельца.
func (o *OutputObserver) ObserveTerminal(ctx context.Context, v OutputObservation) error {
	if o == nil || (v.State != OutputFailed && v.State != OutputStopped) {
		return ErrInvalid
	}
	return o.observe(ctx, string(v.State), "", v.Error, 0, 0)
}

// Observe обновляет состояние только текущей действующей попытки FFmpeg.
func (o *FFmpegAttemptObserver) Observe(ctx context.Context, v FFmpegObservation) error {
	if o == nil || !v.State.valid() {
		return ErrInvalid
	}
	return o.observe(ctx, string(v.State), "", v.Error, o.attemptID, 0)
}

// RuntimeSnapshot возвращает согласованную копию RAM-наблюдений точного RuntimeSessionFence под общей
// блокировкой RuntimeAuthority; отсутствие узлов даёт ErrNotReady.
func (s *Service) RuntimeSnapshot(ctx context.Context, f RuntimeSessionFence) (SessionRuntimeSnapshot, error) {
	// a содержит общую RuntimeAuthority, а err — причину невозможности её получить.
	a, err := s.runtimeAuthority()
	if err != nil {
		return SessionRuntimeSnapshot{}, err
	}
	if err = a.acquire(ctx); err != nil {
		return SessionRuntimeSnapshot{}, err
	}
	defer a.release()
	return a.runtimeSnapshotLocked(f)
}

// runtimeSnapshotLocked копирует наблюдения в порядке идентификаторов, отдельно сохраняя отсутствие
// значения и метрик; просроченные измерения помечаются в копии при чтении.
func (a *RuntimeAuthority) runtimeSnapshotLocked(f RuntimeSessionFence) (SessionRuntimeSnapshot, error) {
	// result накапливает согласованные копии наблюдений и текущую позицию событий.
	result := SessionRuntimeSnapshot{Cursor: a.cursorLocked(), CursorValid: a.events != nil && !a.events.exhausted}
	// tokens собирает локальные идентификаторы узлов запрошенной сессии для упорядочивания.
	tokens := make([]uint64, 0)
	// token идентифицирует проверяемый узел, а n содержит его принадлежность сессии.
	for token, n := range a.observations {
		if n.session == f {
			tokens = append(tokens, token)
		}
	}
	if len(tokens) == 0 {
		return SessionRuntimeSnapshot{}, ErrNotReady
	}
	result.Fence = cloneRuntimeFence(f)
	slices.Sort(tokens)
	// token идентифицирует очередной узел сессии в отсортированном порядке.
	for _, token := range tokens {
		// n содержит узел, копируемый в соответствующий раздел снимка.
		n := a.observations[token]
		switch n.kind {
		case observationSession:
			// v содержит копию состояния сессии с её меткой владельца.
			v := SessionObservationSnapshot{Stamp: n.stamp}
			if n.present {
				v.Value = &SessionObservation{State: ObservationState(n.state), Error: strings.Clone(n.failure)}
			}
			result.Input = cloneInput(n.input)
			if result.Input != nil && result.Input.Metrics != nil {
				result.Input.Metrics.MediaMetrics = a.freshMediaLocked(result.Input.Metrics.MediaMetrics, n.sampleTimes[0])
			}
			result.Session = &v
		case observationGroup:
			// v содержит копию состояния и корреляционных идентификаторов группы.
			v := GroupObservationSnapshot{Stamp: n.stamp, ConfigGroupID: ID(strings.Clone(string(n.groupID))), ConfigProfileID: ID(strings.Clone(string(n.profileID)))}
			if n.present {
				v.Value = &GroupObservation{State: ObservationState(n.state), Source: n.source, SourceRevision: n.sourceRevision, Error: strings.Clone(n.failure)}
			}
			if n.groupMetrics != nil {
				// m содержит независимую копию измерений группы для пометки устаревания.
				m := *n.groupMetrics
				m.LiveInput = a.freshMediaLocked(m.LiveInput, n.sampleTimes[0])
				m.FallbackInput = a.freshMediaLocked(m.FallbackInput, n.sampleTimes[1])
				m.Output = a.freshMediaLocked(m.Output, n.sampleTimes[2])
				v.Metrics = &m
			}
			v.MetricsStamp = n.metricsStamp
			result.Groups = append(result.Groups, v)
		case observationOutput:
			// v содержит копию состояния выхода с точной идентичностью исполнения.
			v := OutputObservationSnapshot{Stamp: n.stamp, Execution: cloneExecutionFence(n.execution)}
			if n.present {
				v.Value = &OutputObservation{State: OutputState(n.state), Error: strings.Clone(n.failure)}
			}
			if n.outputMetrics != nil {
				// m содержит независимую копию измерений выхода для пометки устаревания.
				m := *n.outputMetrics
				m.MediaMetrics = a.freshMediaLocked(m.MediaMetrics, n.sampleTimes[0])
				v.Metrics = &m
			}
			v.MetricsStamp = n.metricsStamp
			result.Outputs = append(result.Outputs, v)
		case observationFFmpeg:
			// v содержит копию состояния попытки FFmpeg и идентификатор её группы.
			v := FFmpegObservationSnapshot{Stamp: n.stamp, GroupInstanceID: n.parent}
			if n.present {
				v.Value = &FFmpegObservation{State: ObservationState(n.state), Error: strings.Clone(n.failure)}
			}
			if n.ffmpegMetrics != nil {
				// m содержит независимую копию прогресса FFmpeg для пометки устаревания.
				m := *n.ffmpegMetrics
				if a.telemetry != nil && m.HasProgress && time.Since(n.progressTime) >= a.telemetry.MediaStaleAfter {
					m.Status = MetricStale
					m.Reason = ReasonStale
				}
				v.Metrics = &m
			}
			v.MetricsStamp = n.metricsStamp
			v.Role = n.role
			result.FFmpeg = append(result.FFmpeg, v)
		}
	}
	return result, nil
}

// RuntimeOwnedUnitOfWork удерживает точный допуск наблюдателя до завершения записи и сверки.
type RuntimeOwnedUnitOfWork interface {

	// WriteOwned проверяет точный идентификатор OutputObserver и удерживает общий допуск до конца
	// транзакции и последующей сверки; переданная функция содержит только повторяемые операции БД.
	WriteOwned(context.Context, *OutputObserver, func(Repositories) error) error
}

// GuardOwnedWrite проверяет наблюдателя перед записью и сверяет состояние после неё.
// reconcileTimeout ограничивает послезаписную сверку независимо от срока операции.
func (a *RuntimeAuthority) GuardOwnedWrite(ctx context.Context, uow UnitOfWork, owner *OutputObserver, reconcileTimeout time.Duration, write func() (bool, error)) error {
	if owner == nil || owner.node == nil || owner.node.authority != a || reconcileTimeout <= 0 || write == nil || uow == nil {
		return ErrInvalid
	}
	// err сообщает об ошибке ожидания общей блокировки перед записью точного владельца.
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if !owner.Valid() || a.observations[owner.node.token] != owner.node {
		return ErrStale
	}
	// uncertain показывает, мог ли COMMIT состояться и нужна ли последующая сверка владельцев.
	uncertain := true
	defer func() {
		// После записи с возможным COMMIT сверяет владельцев до освобождения общей блокировки, сохраняя
		// исходный результат записи.

		if uncertain {
			a.reconcile(ctx, uow, reconcileTimeout)
		}
	}()
	// err сохраняет исходный результат записи независимо от результата последующей сверки.
	var err error
	uncertain, err = write()
	return err
}

// FailOutput под точным допуском OutputObserver сохраняет окончательный отказ уже успешно запущенного
// выхода, отзывает его поколение и создаёт событие с ExecutionFence; исходная успешная квитанция
// сохраняется.
func (s *Service) FailOutput(ctx context.Context, owner *OutputObserver, reason string) error {
	if owner == nil || owner.node == nil {
		return ErrInvalid
	}
	// err содержит причину отказа проверки итогового объяснения отказа выхода.
	if err := validText("reason", reason); err != nil {
		return err
	}
	// a содержит общую RuntimeAuthority, а err — причину невозможности её получить.
	a, err := s.runtimeAuthority()
	if err != nil {
		return err
	}
	if owner.node.authority != a {
		return ErrStale
	}
	// uow предоставляет запись под точным допуском наблюдателя, а ok показывает наличие этой
	// возможности у хранилища.
	uow, ok := s.uow.(RuntimeOwnedUnitOfWork)
	if !ok {
		return ErrRuntimeCapability
	}
	// f сохраняет точную идентичность отказавшего исполнения выхода.
	f := owner.ExecutionFence()
	return uow.WriteOwned(ctx, owner, func(r Repositories) error {
		// Под точным допуском OutputObserver сохраняет окончательный отказ актуального выхода, отзыв
		// намерения, очистку и событие с ExecutionFence, сохраняя успешную квитанцию запуска.

		// c содержит исходную квитанцию, session — родительскую сессию, o — точный выход, а err
		// — результат проверки их соответствия fence.
		c, session, o, err := s.fenced(ctx, r, f)
		if err != nil {
			return err
		}
		if o == nil || c.Status != CommandSucceeded || (c.Kind != CommandStartOutput && c.Kind != CommandAddOutput) || session.State != SessionRunning || !o.Attached || !o.DesiredRunning || !o.Issued || o.CleanupPending || o.Attempt != f.Attempt {
			return ErrStale
		}
		// old сохраняет ожидаемую ревизию перед изменением выхода или родительской сессии.
		old := o.Revision
		if err = stopOutput(ctx, r, session, o, false); err != nil {
			return err
		}
		if err = cancelCommands(ctx, r, session.ID, o.ID, c.ID, false); err != nil {
			return err
		}
		o.Record, err = revise(o.Record)
		if err != nil {
			return err
		}
		if err = r.Runtime.UpdateOutput(ctx, *o, old); err != nil {
			return err
		}
		if o.CleanupPending && !session.CleanupPending {
			old = session.Revision
			session.CleanupPending = true
			session.Record, err = revise(session.Record)
			if err != nil {
				return err
			}
			if err = r.Runtime.UpdateSession(ctx, session, old); err != nil {
				return err
			}
		}
		// id содержит новый идентификатор служебного события отказа выхода.
		id, err := newID()
		if err != nil {
			return err
		}
		// execution сохраняет копию точной попытки для указателя в служебном событии.
		execution := f
		return r.Runtime.InsertEvent(ctx, Event{ID: id, SessionID: session.ID, OutputID: o.ID, Kind: "output_execution_failed", Message: reason, Execution: &execution, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)})
	})
}

// SessionFence возвращает неизменяемого владельца родительского запуска.
func (o *SessionObserver) SessionFence() RuntimeSessionFence {
	if o == nil || o.node == nil {
		return RuntimeSessionFence{}
	}
	return o.node.session
}
