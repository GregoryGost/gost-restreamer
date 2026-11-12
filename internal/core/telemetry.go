package core

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Постоянные веса учитывают все типизированные поля дополнительно к старому узлу.
// Это логический резерв реестра; он не описывает размер кучи Go либо объём резидентной памяти процесса.
const (
	// inputTelemetryWeight резервирует логические байты полей телеметрии входа OBS в узле сессии.
	inputTelemetryWeight uint64 = 1024
	// groupTelemetryWeight резервирует логические байты трёх медиапутей группы.
	groupTelemetryWeight uint64 = 3072
	// outputTelemetryWeight резервирует логические байты полей телеметрии выхода.
	outputTelemetryWeight uint64 = 1536
	// ffmpegTelemetryWeight резервирует логические байты полей прогресса FFmpeg.
	ffmpegTelemetryWeight uint64 = 512
)

// telemetryNodeWeight возвращает постоянный логический резерв типизированной нагрузки для вида узла
// наблюдений.
func telemetryNodeWeight(k observationKind) uint64 {
	switch k {
	case observationSession:
		return inputTelemetryWeight
	case observationGroup:
		return groupTelemetryWeight
	case observationOutput:
		return outputTelemetryWeight
	default:
		return ffmpegTelemetryWeight
	}
}

// FFmpegRole различает LIVE-перекодирование и проигрывание заглушки у работника FFmpeg.
type FFmpegRole string

const (
	// FFmpegLiveTranscode обозначает работника перекодирования LIVE-потока.
	FFmpegLiveTranscode FFmpegRole = "live_transcode"
	// FFmpegFallbackPlayer обозначает работника проигрывания заглушки.
	FFmpegFallbackPlayer FFmpegRole = "fallback_player"
)

// OpenFFmpegRole регистрирует дочернего наблюдателя FFmpeg с явной ролью LIVE-перекодирования либо
// проигрывания заглушки.
func (o *GroupObserver) OpenFFmpegRole(ctx context.Context, role FFmpegRole) (*FFmpegObserver, error) {
	if o == nil || (role != FFmpegLiveTranscode && role != FFmpegFallbackPlayer) {
		return nil, ErrInvalid
	}
	// n содержит новый узел работника с выбранной ролью, а err — результат его регистрации.
	n, err := o.child(ctx, observationFFmpeg, "", "", role)
	if err != nil {
		return nil, err
	}
	return &FFmpegObserver{observerHandle{n}}, nil
}

// cloneInput копирует снимок входа, строку LeaseID и необязательные метрики.
func cloneInput(p *InputSnapshot) *InputSnapshot {
	if p == nil {
		return nil
	}
	// v содержит независимую копию состояния входа OBS.
	v := *p
	v.LeaseID = ID(strings.Clone(string(v.LeaseID)))
	if v.Metrics != nil {
		// m содержит независимую копию последней выборки входа.
		m := *v.Metrics
		v.Metrics = &m
	}
	return &v
}

// connectInputNodeLocked связывает вход Session с новым RuntimeLease, сбрасывает прежние измерения и
// публикует событие подключения под общей блокировкой RuntimeAuthority.
func (a *RuntimeAuthority) connectInputNodeLocked(n *observationNode, l *RuntimeLease) {
	if n.input == nil {
		return
	}
	// v содержит новое состояние подключённой публикации с очищенными прежними измерениями.
	v := InputSnapshot{LeaseID: l.id, Connected: true, ChangedAt: time.Now().UTC()}
	v.Stamp = ObservationStamp{Token: n.token, InstanceID: n.token, ObservedAt: v.ChangedAt}
	n.metricsStamp = ObservationStamp{}
	n.input = &v
	n.sampleTimes = [3]time.Time{}
	// e содержит событие подключения или замены публикации OBS.
	e := a.nodeEvent(n, RuntimeInputEvent)
	e.Input = n.input
	e.Stamp = n.input.Stamp
	a.appendEventLocked(e)
}

// inputConnectedLocked обновляет вход всех узлов Session, принадлежащих точному владельцу новой
// публикации.
func (a *RuntimeAuthority) inputConnectedLocked(l *RuntimeLease) {
	// n содержит очередной узел для поиска владельцев сессии новой публикации.
	for _, n := range a.observations {
		if n.kind == observationSession && n.session == l.session {
			a.connectInputNodeLocked(n, l)
		}
	}
}

// inputDisconnectedLocked отмечает отключение только совпадающего LeaseID, сохраняя уже установленную
// замену OBS.
func (a *RuntimeAuthority) inputDisconnectedLocked(l *RuntimeLease) {
	// n содержит очередной узел для поиска точного отключаемого LeaseID.
	for _, n := range a.observations {
		if n.kind == observationSession && n.session == l.session && n.input != nil && n.input.LeaseID == l.id {
			n.input = &InputSnapshot{LeaseID: l.id, ChangedAt: time.Now().UTC()}
			n.input.Stamp = ObservationStamp{Token: n.token, InstanceID: n.token, ObservedAt: n.input.ChangedAt}
			// e содержит событие отключения точной публикации OBS.
			e := a.nodeEvent(n, RuntimeInputEvent)
			e.Input = n.input
			e.Reason = ReasonDisconnected
			a.appendEventLocked(e)
		}
	}
}

// metricsLocked проверяет текущий идентификатор, RuntimeLease, попытку FFmpeg и включённую телеметрию
// под общей блокировкой RuntimeAuthority.
func (h observerHandle) metricsLocked(attempt uint64) error {
	if !h.Valid() || h.node.authority.observations[h.node.token] != h.node || (h.node.kind == observationFFmpeg && (h.node.stamp.AttemptID != attempt || h.node.attemptDone == nil)) {
		return ErrStale
	}
	if h.node.authority.telemetry == nil {
		return ErrNotReady
	}
	return nil
}

// nextMetricsStamp создаёт следующую ревизию измерений с идентификатором узла и текущей попыткой;
// переполнение отклоняется.
func (n *observationNode) nextMetricsStamp(sampled time.Time) (ObservationStamp, error) {
	// revision содержит следующую ревизию измерений, а err — признак исчерпания счётчика.
	revision, err := NextRevision(n.metricsStamp.Revision)
	if err != nil {
		return ObservationStamp{}, err
	}
	return ObservationStamp{Token: n.token, InstanceID: n.token, Revision: revision, AttemptID: n.stamp.AttemptID, ObservedAt: sampled.UTC()}, nil
}

// ObserveInput принимает более свежую выборку только текущего подключённого LeaseID и сохраняет копию
// метрик и событие в RAM.
func (o *SessionObserver) ObserveInput(ctx context.Context, leaseID ID, v InputMetrics) error {
	if o == nil || o.node == nil || leaseID == "" || !v.valid() {
		return ErrInvalid
	}
	// a предоставляет общую блокировку записи измерений входа OBS.
	a := o.node.authority
	// err сообщает об ошибке ожидания общей блокировки перед записью входа.
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	// err сообщает об ошибке проверки точного владельца и включённой телеметрии.
	if err := o.metricsLocked(0); err != nil {
		return err
	}
	// n содержит точный узел сессии, к которому привязан измеряемый вход.
	n := o.node
	if n.input == nil || !n.input.Connected || n.input.LeaseID != leaseID {
		return ErrStale
	}
	// current показывает, найдена ли действующая публикация с указанным LeaseID.
	current := false
	// l содержит очередной допуск OBS для проверки точной текущей публикации.
	for _, l := range a.publications {
		if l.id == leaseID && l.session == n.session && l.Valid() {
			current = true
			break
		}
	}
	if !current {
		return ErrStale
	}
	if n.input.Metrics != nil && !v.SampledAt.After(n.sampleTimes[0]) {
		return ErrStale
	}
	// stamp содержит метку следующей ревизии выборки входа, а err — результат её создания.
	stamp, err := n.nextMetricsStamp(v.SampledAt)
	if err != nil {
		return err
	}
	n.metricsStamp = stamp
	n.sampleTimes[0] = v.SampledAt
	v.MediaMetrics = utcMedia(v.MediaMetrics)
	// p содержит копию состояния входа для привязки новой выборки.
	p := *n.input
	p.Stamp = stamp
	p.Metrics = &v
	n.input = &p
	// e содержит событие нового измерения входа OBS.
	e := a.nodeEvent(n, RuntimeMetricsEvent)
	e.Input = n.input
	e.Stamp = n.input.Stamp
	a.appendEventLocked(e)
	return nil
}

// ObserveMetrics принимает согласованный комплект метрик группы, только если время каждой из трёх
// дорожек продвинулось относительно прежней выборки.
func (o *GroupObserver) ObserveMetrics(ctx context.Context, v GroupMetrics) error {
	if o == nil || o.node == nil || !v.LiveInput.valid() || !v.FallbackInput.valid() || !v.Output.valid() {
		return ErrInvalid
	}
	// a предоставляет общую блокировку записи измерений группы.
	a := o.node.authority
	// err сообщает об ошибке ожидания общей блокировки перед записью группы.
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	// err сообщает об ошибке проверки точного владельца группы и телеметрии.
	if err := o.metricsLocked(0); err != nil {
		return err
	}
	// n содержит узел группы, измерения которого обновляются.
	n := o.node
	if n.groupMetrics != nil && (!v.LiveInput.SampledAt.After(n.sampleTimes[0]) || !v.FallbackInput.SampledAt.After(n.sampleTimes[1]) || !v.Output.SampledAt.After(n.sampleTimes[2])) {
		return ErrStale
	}
	// stamp содержит метку следующей ревизии измерений группы, а err — результат её создания.
	stamp, err := n.nextMetricsStamp(v.Output.SampledAt)
	if err != nil {
		return err
	}
	n.sampleTimes = [3]time.Time{v.LiveInput.SampledAt, v.FallbackInput.SampledAt, v.Output.SampledAt}
	v.LiveInput = utcMedia(v.LiveInput)
	v.FallbackInput = utcMedia(v.FallbackInput)
	v.Output = utcMedia(v.Output)
	n.groupMetrics = &v
	n.metricsStamp = stamp
	// e содержит событие нового комплекта измерений группы.
	e := a.nodeEvent(n, RuntimeMetricsEvent)
	e.Stamp = stamp
	e.Group = &v
	a.appendEventLocked(e)
	return nil
}

// ObserveMetrics проверяет вместимость очереди и возрастающее время выборки, затем сохраняет метрики
// точного действующего выхода и оперативное событие.
func (o *OutputObserver) ObserveMetrics(ctx context.Context, v OutputMetrics) error {
	if o == nil || o.node == nil || !v.valid() || v.QueuePackets > v.QueueCapacity || v.QueueBytes > v.QueueByteCapacity {
		return ErrInvalid
	}
	// a предоставляет общую блокировку записи измерений выхода.
	a := o.node.authority
	// err сообщает об ошибке ожидания общей блокировки перед записью выхода.
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	// err сообщает об ошибке проверки точного владельца выхода и телеметрии.
	if err := o.metricsLocked(0); err != nil {
		return err
	}
	// n содержит узел выхода, измерения которого обновляются.
	n := o.node
	if n.outputMetrics != nil && !v.SampledAt.After(n.sampleTimes[0]) {
		return ErrStale
	}
	// stamp содержит метку следующей ревизии измерений выхода, а err — результат её создания.
	stamp, err := n.nextMetricsStamp(v.SampledAt)
	if err != nil {
		return err
	}
	n.sampleTimes[0] = v.SampledAt
	v.MediaMetrics = utcMedia(v.MediaMetrics)
	v.LastWrite = v.LastWrite.UTC()
	v.LastMedia = v.LastMedia.UTC()
	v.AckLastAnomalyAt = v.AckLastAnomalyAt.UTC()
	n.outputMetrics = &v
	n.metricsStamp = stamp
	// e содержит событие нового измерения выхода.
	e := a.nodeEvent(n, RuntimeMetricsEvent)
	e.Stamp = stamp
	e.Output = &v
	a.appendEventLocked(e)
	return nil
}

// ObserveMetrics сохраняет более свежую выборку только текущей попытки FFmpeg; время последнего
// сообщения о прогрессе учитывается отдельно для проверки устаревания.
func (o *FFmpegAttemptObserver) ObserveMetrics(ctx context.Context, v FFmpegMetrics) error {
	if o == nil || o.node == nil || !v.valid() {
		return ErrInvalid
	}
	// a предоставляет общую блокировку записи прогресса FFmpeg.
	a := o.node.authority
	// err сообщает об ошибке ожидания общей блокировки перед записью FFmpeg.
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	// err сообщает об ошибке проверки точного владельца и текущей попытки FFmpeg.
	if err := o.metricsLocked(o.attemptID); err != nil {
		return err
	}
	// n содержит узел работника FFmpeg, прогресс которого обновляется.
	n := o.node
	if n.ffmpegMetrics != nil && !v.SampledAt.After(n.sampleTimes[0]) {
		return ErrStale
	}
	// stamp содержит метку следующей ревизии прогресса FFmpeg, а err — результат её создания.
	stamp, err := n.nextMetricsStamp(v.SampledAt)
	if err != nil {
		return err
	}
	n.sampleTimes[0] = v.SampledAt
	v.SampledAt = v.SampledAt.UTC()
	n.progressTime = v.LastProgressAt
	v.LastProgressAt = v.LastProgressAt.UTC()
	n.ffmpegMetrics = &v
	n.metricsStamp = stamp
	// e содержит событие нового прогресса FFmpeg.
	e := a.nodeEvent(n, RuntimeMetricsEvent)
	e.Stamp = stamp
	e.FFmpeg = &v
	a.appendEventLocked(e)
	return nil
}

// SystemObserver владеет единственным процессным снимком GPU независимо от Session.
type SystemObserver struct {
	// authority связывает системного наблюдателя с общим реестром и его блокировкой.
	authority *RuntimeAuthority
	// token идентифицирует конкретного системного владельца GPU.
	token uint64
	// valid показывает, сохраняется ли допуск системного владельца.
	valid atomic.Bool
	// done закрывается при отзыве системного владельца GPU.
	done chan struct{}
	// value хранит последнюю принятую копию GPU-снимка либо nil до первой выборки.
	value *GPUSnapshot
	// revision содержит ревизию последнего принятого GPU-снимка.
	revision uint64
	// sampledAt сохраняет время последней выборки для запрета старых GPU-снимков.
	sampledAt time.Time
}

// OpenSystemObserver резервирует единственного владельца снимка GPU при включённой телеметрии; занятое
// место и исчерпанный идентификатор отклоняются.
func (a *RuntimeAuthority) OpenSystemObserver(ctx context.Context) (*SystemObserver, error) {
	// err сообщает об ошибке ожидания общей блокировки перед резервированием GPU-владельца.
	if err := a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	if a.telemetry == nil {
		return nil, ErrNotReady
	}
	if a.system != nil {
		return nil, ErrConflict
	}
	if a.nextSystem == math.MaxUint64 {
		return nil, ErrConflict
	}
	a.nextSystem++
	// o содержит новый резерв единственного системного владельца GPU.
	o := &SystemObserver{authority: a, token: a.nextSystem, done: make(chan struct{})}
	o.valid.Store(true)
	a.system = o
	return o, nil
}

// OpenSystemObserver открывает владельца процессного снимка GPU в общей RuntimeAuthority.
func (s *Service) OpenSystemObserver(ctx context.Context) (*SystemObserver, error) {
	// a содержит общую RuntimeAuthority, а err — причину невозможности её получить.
	a, err := s.runtimeAuthority()
	if err != nil {
		return nil, err
	}
	return a.OpenSystemObserver(ctx)
}

// Token возвращает локальную идентичность владельца GPU; nil-получатель даёт 0.
func (o *SystemObserver) Token() uint64 {
	if o == nil {
		return 0
	}
	return o.token
}

// Valid сообщает, сохранён ли допуск владельца снимка GPU.
func (o *SystemObserver) Valid() bool { return o != nil && o.valid.Load() }

// Done возвращает канал закрытия владельца GPU; nil-получатель даёт nil.
func (o *SystemObserver) Done() <-chan struct{} {
	if o == nil {
		return nil
	}
	return o.done
}

// Close отзывает собственный допуск GPU под общей блокировкой RuntimeAuthority и публикует удаление
// снимка.
func (o *SystemObserver) Close(ctx context.Context) error {
	if o == nil {
		return ErrInvalid
	}
	// a предоставляет общую блокировку закрытия системного владельца GPU.
	a := o.authority
	// err сообщает об ошибке ожидания общей блокировки перед закрытием GPU-владельца.
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	o.closeLocked()
	return nil
}

// closeLocked однократно стирает снимок GPU, снимает только собственную резервацию и закрывает Done
// под общей блокировкой RuntimeAuthority.
func (o *SystemObserver) closeLocked() {
	if !o.valid.Swap(false) {
		return
	}
	o.value = nil
	if o.authority.system == o {
		o.authority.system = nil
	}
	close(o.done)
	o.authority.appendEventLocked(RuntimeEvent{Kind: RuntimeRemovedEvent, Owner: RuntimeSystemOwner, Stamp: ObservationStamp{Token: o.token, InstanceID: o.token}})
}

// cloneGPU копирует снимок, список устройств и строки UUID, отделяя нагрузку от владельца.
func cloneGPU(p *GPUSnapshot) *GPUSnapshot {
	if p == nil {
		return nil
	}
	// v содержит независимую копию GPU-снимка и списка устройств.
	v := *p
	v.Devices = make([]GPUDevice, len(p.Devices))
	copy(v.Devices, p.Devices)
	// i указывает устройство, UUID которого копируется в собственный строковый буфер.
	for i := range v.Devices {
		v.Devices[i].UUID = strings.Clone(v.Devices[i].UUID)
	}
	return &v
}

// ObserveGPU проверяет устройства, типизированные показатели, бюджеты и возрастающее время выборки до
// замены снимка; отказ сохраняет прежнее значение.
func (o *SystemObserver) ObserveGPU(ctx context.Context, v GPUSnapshot) error {
	if o == nil || v.SampledAt.IsZero() || v.StaleAfter <= 0 || !v.Status.valid() || !v.Reason.valid() {
		return ErrInvalid
	}
	// a предоставляет общую блокировку проверки и принятия GPU-снимка.
	a := o.authority
	// err сообщает об ошибке ожидания общей блокировки перед записью GPU-снимка.
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if !o.valid.Load() || a.system != o {
		return ErrStale
	}
	// l содержит неизменяемые общие бюджеты GPU-устройств и строк UUID.
	l := a.telemetry
	if len(v.Devices) > l.MaxGPUDevices {
		return ErrObservationLimit
	}
	// weight накапливает логический вес GPU-снимка до его копирования и принятия.
	weight := uint64(256)
	// seen отмечает встреченные UUID для запрета повторов устройств.
	seen := map[string]bool{}
	// d содержит очередное устройство GPU-снимка для проверки UUID и показателей.
	for _, d := range v.Devices {
		if uint64(len(d.UUID)) > l.MaxGPUUUIDBytes {
			return ErrObservationLimit
		}
		if d.UUID == "" || !utf8.ValidString(d.UUID) || strings.ContainsRune(d.UUID, 0) || seen[d.UUID] {
			return ErrInvalid
		}
		seen[d.UUID] = true
		weight += 1024 + uint64(len(d.UUID))
		// i отличает процентные показатели от байтов памяти, а f содержит проверяемый показатель
		// GPU.
		for i, f := range []GPUMetric{d.Utilization, d.MemoryUtilization, d.EncoderUtilization, d.DecoderUtilization, d.MemoryUsedBytes, d.MemoryTotalBytes, d.MemoryFreeBytes} {
			if !f.Status.valid() || !f.Reason.valid() || (i < 4 && f.Value > 100) || (f.Status == MetricAvailable && (f.LastSuccessAt.IsZero() || f.LastSuccessAt.After(v.SampledAt))) {
				return ErrInvalid
			}
		}
	}
	if weight > l.MaxGPUBytes {
		return ErrObservationLimit
	}
	if o.value != nil && !v.SampledAt.After(o.sampledAt) {
		return ErrStale
	}
	// revision содержит следующую ревизию GPU-снимка, а err — признак исчерпания ревизий.
	revision, err := NextRevision(o.revision)
	if err != nil {
		return err
	}
	o.revision = revision
	o.sampledAt = v.SampledAt
	v = *cloneGPU(&v)
	o.value = &v
	a.appendEventLocked(RuntimeEvent{Kind: RuntimeGPUEvent, Owner: RuntimeSystemOwner, Stamp: ObservationStamp{Token: o.token, InstanceID: o.token, Revision: revision, ObservedAt: v.SampledAt.UTC()}, GPU: o.value})
	return nil
}

// RuntimeAnchor возвращает согласованный исходный снимок всех RAM-сессий, GPU и курсора событий общей
// RuntimeAuthority.
func (s *Service) RuntimeAnchor(ctx context.Context) (RuntimeAnchor, error) {
	// a содержит общую RuntimeAuthority, а err — причину невозможности её получить.
	a, err := s.runtimeAuthority()
	if err != nil {
		return RuntimeAnchor{}, err
	}
	return a.RuntimeAnchor(ctx)
}

// RuntimeAnchor под общей блокировкой RuntimeAuthority собирает копии RAM-сессий в устойчивом порядке
// и копию снимка GPU с проверкой устаревания вместе с курсором и каналом изменений.
func (a *RuntimeAuthority) RuntimeAnchor(ctx context.Context) (RuntimeAnchor, error) {
	// err сообщает об ошибке ожидания общей блокировки для согласованного снимка.
	if err := a.acquire(ctx); err != nil {
		return RuntimeAnchor{}, err
	}
	defer a.release()
	if a.events == nil {
		return RuntimeAnchor{}, ErrNotReady
	}
	if a.events.exhausted {
		return RuntimeAnchor{}, ErrEventSequenceExhausted
	}
	// result накапливает согласованные снимки сессий, GPU и позицию событий.
	result := RuntimeAnchor{Cursor: a.cursorLocked(), Changed: a.events.changed}
	// fences собирает уникальные точные выполнения сессий, представленные в RAM.
	fences := map[RuntimeSessionFence]bool{}
	// n содержит очередной узел для сбора его точного владельца сессии.
	for _, n := range a.observations {
		fences[n.session] = true
	}
	// f идентифицирует очередную сессию, включаемую в общий снимок.
	for f := range fences {
		// snapshot содержит согласованный снимок одной сессии, а err — результат его построения.
		snapshot, err := a.runtimeSnapshotLocked(f)
		if err != nil {
			return RuntimeAnchor{}, err
		}
		result.Sessions = append(result.Sessions, snapshot)
	}
	sort.Slice(result.Sessions, func(i, j int) bool {
		// Сортирует снимки Session по SessionID, затем Incarnation и Lifecycle, устраняя зависимость порядка
		// ответа от обхода карты.

		// l содержит владельца левого снимка, а r — правого для устойчивого упорядочивания
		// сессий.
		l, r := result.Sessions[i].Fence, result.Sessions[j].Fence
		if l.SessionID != r.SessionID {
			return l.SessionID < r.SessionID
		}
		if l.Incarnation != r.Incarnation {
			return l.Incarnation < r.Incarnation
		}
		return l.Lifecycle < r.Lifecycle
	})
	if a.system != nil {
		result.GPU = freshGPU(a.system.value, time.Now())
	}
	return result, nil
}

// freshGPU копирует снимок и помечает доступные показатели GPU устаревшими по времени последнего
// успеха, не изменяя сохранённое значение.
func freshGPU(p *GPUSnapshot, now time.Time) *GPUSnapshot {
	// v содержит независимую копию GPU-снимка для пометки устаревания и перевода времени в UTC.
	v := cloneGPU(p)
	if v == nil {
		return nil
	}
	v.SampledAt = v.SampledAt.UTC()
	// i указывает очередное устройство в копии GPU-снимка.
	for i := range v.Devices {
		// d ссылается на устройство копии для обновления статусов его показателей.
		d := &v.Devices[i]
		// f ссылается на очередной показатель устройства, свежесть которого проверяется.
		for _, f := range []*GPUMetric{&d.Utilization, &d.MemoryUtilization, &d.EncoderUtilization, &d.DecoderUtilization, &d.MemoryUsedBytes, &d.MemoryTotalBytes, &d.MemoryFreeBytes} {
			if f.Status == MetricAvailable && (f.LastSuccessAt.IsZero() || now.Sub(f.LastSuccessAt) >= v.StaleAfter) {
				f.Status = MetricStale
				f.Reason = ReasonStale
			}
			f.SampledAt = f.SampledAt.UTC()
			f.LastSuccessAt = f.LastSuccessAt.UTC()
		}
	}
	return v
}

// freshMediaLocked помечает доступные и прогревающиеся скорости устаревшими после MediaStaleAfter,
// сохраняя границы их последнего окна.
func (a *RuntimeAuthority) freshMediaLocked(m MediaMetrics, at time.Time) MediaMetrics {
	if a.telemetry == nil || time.Since(at) < a.telemetry.MediaStaleAfter {
		return m
	}
	// r ссылается на очередную скорость медиа для пометки устаревания без изменения исходного
	// измерения.
	for _, r := range []*Rate{&m.Rates.FPS, &m.Rates.PayloadBitrate, &m.Rates.WrittenBitrate} {
		if r.Status == MetricAvailable || r.Status == MetricWarmingUp {
			*r = Rate{Status: MetricStale, Reason: ReasonStale, WindowStart: r.WindowStart, WindowEnd: r.WindowEnd, Duration: r.Duration}
		}
	}
	return m
}
