package core

import (
	"math"
	"time"
)

// MetricStatus различает доступное измерение и причины отсутствия пригодного численного результата.
type MetricStatus string

const (
	// MetricAvailable обозначает доступный измеренный результат.
	MetricAvailable MetricStatus = "available"
	// MetricWarmingUp обозначает накопление выборок для первого пригодного окна скорости.
	MetricWarmingUp MetricStatus = "warming_up"
	// MetricUnavailable обозначает отсутствие доступного измерения.
	MetricUnavailable MetricStatus = "unavailable"
	// MetricStale обозначает измерение с истёкшим сроком свежести.
	MetricStale MetricStatus = "stale"
	// MetricSaturated обозначает насыщение накопительного счётчика.
	MetricSaturated MetricStatus = "saturated"
	// MetricCounterReset обозначает сброс или регресс накопительного счётчика.
	MetricCounterReset MetricStatus = "counter_reset"
	// MetricUnsupported обозначает неподдерживаемый показатель.
	MetricUnsupported MetricStatus = "unsupported"
)

// valid распознаёт предусмотренные статусы измерения, включая отсутствие, устаревание и переполнение
// счётчика.
func (s MetricStatus) valid() bool {
	switch s {
	case MetricAvailable, MetricWarmingUp, MetricUnavailable, MetricStale, MetricSaturated, MetricCounterReset, MetricUnsupported:
		return true
	}
	return false
}

// MetricReason содержит типизированное объяснение статуса без произвольного текста диагностики.
type MetricReason string

const (
	// ReasonNone обозначает отсутствие дополнительной причины.
	ReasonNone MetricReason = ""
	// ReasonUnsupportedPlatform обозначает неподдерживаемую платформу.
	ReasonUnsupportedPlatform MetricReason = "unsupported_platform"
	// ReasonLibraryUnavailable обозначает недоступность библиотеки сбора.
	ReasonLibraryUnavailable MetricReason = "library_unavailable"
	// ReasonDriverUnavailable обозначает недоступность драйвера устройства.
	ReasonDriverUnavailable MetricReason = "driver_unavailable"
	// ReasonNotSupported обозначает отсутствие поддержки показателя источником.
	ReasonNotSupported MetricReason = "not_supported"
	// ReasonNotFound обозначает отсутствие запрошенного источника или устройства.
	ReasonNotFound MetricReason = "not_found"
	// ReasonCollectionFailed обозначает отказ сбора измерения.
	ReasonCollectionFailed MetricReason = "collection_failed"
	// ReasonSourceAbsent обозначает отсутствие медиаисточника.
	ReasonSourceAbsent MetricReason = "source_absent"
	// ReasonDisconnected обозначает отключение источника.
	ReasonDisconnected MetricReason = "disconnected"
	// ReasonInvalidSample обозначает недопустимую выборку.
	ReasonInvalidSample MetricReason = "invalid_sample"
	// ReasonCounterRegression обозначает уменьшение накопительного счётчика.
	ReasonCounterRegression MetricReason = "counter_regression"
	// ReasonCounterSaturated обозначает насыщение накопительного счётчика.
	ReasonCounterSaturated MetricReason = "counter_saturated"
	// ReasonObservationFailed обозначает наблюдаемый отказ компонента.
	ReasonObservationFailed MetricReason = "observation_failed"
	// ReasonCGODisabled обозначает недоступность сбора при выключенном CGO.
	ReasonCGODisabled MetricReason = "cgo_disabled"
	// ReasonPermissionDenied обозначает отказ доступа к источнику измерения.
	ReasonPermissionDenied MetricReason = "permission_denied"
	// ReasonDeviceLost обозначает потерю устройства.
	ReasonDeviceLost MetricReason = "device_lost"
	// ReasonProviderError обозначает отказ поставщика измерений.
	ReasonProviderError MetricReason = "provider_error"
	// ReasonBudgetExceeded обозначает превышение явного бюджета наблюдений.
	ReasonBudgetExceeded MetricReason = "budget_exceeded"
	// ReasonBusy обозначает занятость поставщика измерений.
	ReasonBusy MetricReason = "busy"
	// ReasonStale обозначает истечение срока свежести.
	ReasonStale MetricReason = "stale"
	// ReasonWarmingUp обозначает недостаточное число выборок для вычисления скорости.
	ReasonWarmingUp MetricReason = "warming_up"
)

// valid разрешает только типизированные причины состояния измерения и пустую причину.
func (r MetricReason) valid() bool {
	switch r {
	case ReasonNone, ReasonUnsupportedPlatform, ReasonLibraryUnavailable, ReasonDriverUnavailable, ReasonNotSupported, ReasonNotFound, ReasonCollectionFailed, ReasonSourceAbsent, ReasonDisconnected, ReasonInvalidSample, ReasonCounterRegression, ReasonCounterSaturated, ReasonObservationFailed, ReasonCGODisabled, ReasonPermissionDenied, ReasonDeviceLost, ReasonProviderError, ReasonBudgetExceeded, ReasonBusy, ReasonStale, ReasonWarmingUp:
		return true
	}
	return false
}

// Rate задаёт скорость за фактическое окно конкретного производителя.
// PayloadBitrate и WrittenBitrate измеряются в битах в секунду, FPS — в кадрах в секунду.
type Rate struct {
	// Status описывает доступность и пригодность текущего показателя.
	Status MetricStatus
	// Reason содержит типизированное объяснение текущего статуса.
	Reason MetricReason
	// Value содержит скорость кадров или битов за фактически измеренное окно.
	Value float64
	// WindowStart фиксирует начальную временную границу измеренного окна скорости.
	WindowStart time.Time
	// WindowEnd фиксирует конечную временную границу измеренного окна скорости.
	WindowEnd time.Time
	// Duration содержит фактическую монотонную длительность окна для вычисления скорости.
	Duration time.Duration
}

// MediaRates хранит независимо измеренные скорости кадров и двух видов байтов.
type MediaRates struct { /* FPS содержит число видеокадров в секунду, PayloadBitrate — скорость полезных медиабайтов в битах в секунду, а WrittenBitrate — скорость принятых транспортом байтов в битах в секунду. */
	FPS, PayloadBitrate, WrittenBitrate Rate
}

// ObservedFormat сохраняет фактически распознанные параметры дорожек без подстановки профиля.
type ObservedFormat struct {
	// VideoCodec содержит фактически распознанный видеокодек либо пустое значение до распознавания.
	VideoCodec Codec
	// AudioCodec содержит фактически распознанный аудиокодек либо пустое значение до распознавания.
	AudioCodec Codec
	// Width содержит измеренную ширину видеокадра в пикселях, а Height — его измеренную высоту.
	Width, Height uint32
	// SampleRateHz содержит фактически распознанную частоту аудио в герцах.
	SampleRateHz uint32
	// Channels содержит фактически распознанное число аудиоканалов.
	Channels uint16
}

// MediaMetrics сохраняет накопительные медиасчётчики и скорости конкретной измеряемой границы.
type MediaMetrics struct {
	// SampledAt содержит время снятия согласованной выборки.
	SampledAt time.Time
	// VideoFrames считает подтверждённые видеоединицы на этой границе, PayloadBytes — полезные байты
	// видео и аудио, а WrittenBytes — принятые транспортом байты RTMP, включая частичные записи.
	VideoFrames, PayloadBytes, WrittenBytes uint64
	// AudioUnits считает обработанные единицы доступа аудио на этой медиагранице.
	AudioUnits uint64
	// Saturated отмечает насыщение хотя бы одного накопительного счётчика.
	Saturated bool
	// LastFrame содержит время последнего подтверждённого видео на измеряемой границе.
	LastFrame time.Time
	// Rates содержит независимо рассчитанные FPS и скорости полезных и записанных байтов.
	Rates MediaRates
	// Format содержит фактически распознанный формат медиа.
	Format ObservedFormat
}

// InputMetrics дополняет измерения входа OBS идентичностью сегмента источника.
type InputMetrics struct {
	// MediaMetrics содержит медиасчётчики и фактические окна скоростей этой границы.
	MediaMetrics
	// SourceEpoch различает сегменты LIVE-потока, в том числе внутри одного OBS-соединения.
	SourceEpoch uint64
}

// InputSnapshot отделяет допуск текущего OBS-соединения от наличия его измерений.
type InputSnapshot struct {
	// Stamp фиксирует точного владельца и ревизию принятого состояния входа.
	Stamp ObservationStamp
	// LeaseID идентифицирует конкретный допуск публикации OBS.
	LeaseID ID
	// Connected показывает наличие текущей авторизованной публикации OBS.
	Connected bool
	// ChangedAt содержит время подключения, замены или отключения входа.
	ChangedAt time.Time
	// Metrics содержит последнюю выборку текущей публикации либо nil до первого измерения.
	Metrics *InputMetrics
}

// GroupMetrics разделяет измерения LIVE, заглушки и стабильного выхода одной группы.
type GroupMetrics struct { /* LiveInput содержит измерения LIVE-входа группы, FallbackInput — входа заглушки, а Output — стабильного выхода после выбора источника. */
	LiveInput, FallbackInput, Output MediaMetrics
}

// OutputMetrics сохраняет текущие счётчики публикации, очередей и локальных потерь отдельного выхода.
type OutputMetrics struct {
	// QueuedPackets содержит текущее число пакетов основной очереди подписки выхода, а PacketBytes —
	// сумму логического веса пакетов основной очереди и ожидающего аудио, включая удерживаемые ими
	// конфигурации.
	QueuedPackets, PacketBytes uint64
	// AccessUnits считает полностью отправленные медиасообщения за срок Publisher, PendingPackets
	// содержит число аудиопакетов до выбора ключевого кадра, а Generation — монотонное поколение
	// сбросов подписки, отзывающее прежние Delivery.
	AccessUnits, PendingPackets, Generation uint64
	// Resync показывает ожидание повторной синхронизации выхода с медиапотоком.
	Resync bool
	// InFlightPacketBytes содержит логический вес выданного на отправку пакета, InFlightConfigBytes
	// — вес его видео- и аудиоконфигураций, а WireBufferBytes — текущий логический резерв
	// RTMP-транспорта; эти значения снимаются после завершения соответствующей работы.
	InFlightPacketBytes, InFlightConfigBytes, WireBufferBytes uint64
	// ConfigBytes содержит отдельный логический вес текущих конфигураций видео и аудио подписки, а
	// ConfigByteCapacity — предел этого веса, ограниченный удвоенным MaxConfigBytes и общим MaxBytes
	// очереди.
	ConfigBytes, ConfigByteCapacity uint64
	// MediaMetrics содержит медиасчётчики и фактические окна скоростей этой границы.
	MediaMetrics
	// Attempts считает попытки подключения, Connections — установленные соединения, а Reconnects —
	// повторные подключения в текущем запуске выхода.
	Attempts, Connections, Reconnects uint64
	// QueueDrops сохраняет накопительные потери до выдачи пакетов из очереди.
	QueueDrops QueueDropMetrics
	// DeliveryDrops сохраняет накопительные потери после выдачи пакетов на отправку.
	DeliveryDrops DeliveryDropMetrics
	// QueuePackets содержит текущее число пакетов очереди, а QueueCapacity — её предел по числу
	// пакетов.
	QueuePackets, QueueCapacity uint64
	// QueueBytes содержит логическое заполнение очереди в байтах, а QueueByteCapacity — её предел в
	// байтах.
	QueueBytes, QueueByteCapacity uint64
	// LastWrite содержит время последней положительной записи RTMP-байтов, а LastMedia — последнего
	// полностью отправленного медиасообщения.
	LastWrite, LastMedia time.Time
	// AckAheadEventsTotal считает новые ACK с однозначно измеренным опережением в текущем запуске
	// выхода.
	AckAheadEventsTotal uint64
	// AckAheadBytesLast содержит размер последнего измеренного опережения ACK в RTMP-байтах, а
	// AckAheadBytesMax — максимальное опережение текущего запуска выхода.
	AckAheadBytesLast, AckAheadBytesMax uint64
	// AckLastAnomalyAt содержит время обработки последнего измеренного опережающего ACK либо нулевое
	// время до первого события.
	AckLastAnomalyAt time.Time
}

// QueueDropMetrics разделяет потери до выдачи пакетов из очереди по конкретным причинам.
type QueueDropMetrics struct { /* Overflow считает потери при переполнении очереди, Oversized — слишком большие пакеты, ConfigRejected — отклонённые конфигурации, AwaitingKeyframe — пакеты до ключевого кадра, AudioBeforeCut — аудио до границы переключения, SourceReset — сброс источника, ConfigReset — сброс конфигурации, а Closed — потери при закрытии очереди. */
	Overflow, Oversized, ConfigRejected, AwaitingKeyframe, AudioBeforeCut, SourceReset, ConfigReset, Closed uint64
}

// DeliveryDropMetrics разделяет потери уже выданных пакетов по результату доставки.
type DeliveryDropMetrics struct { /* Stale считает потери устаревших пакетов, Transport — отказы отправки, Cancelled — отменённую доставку, а Terminal — потери при окончательном завершении выхода. */
	Stale, Transport, Cancelled, Terminal uint64
}

// OptionalUint64 сохраняет наличие необязательного беззнакового целого показателя FFmpeg.
type OptionalUint64 struct {
	// Value содержит показатель FFmpeg; его наличие определяется отдельно полем Present.
	Value uint64
	// Present показывает, присутствовал ли численный показатель в завершённом отчёте FFmpeg.
	Present bool
}

// OptionalInt64 сохраняет наличие необязательного знакового целого показателя FFmpeg.
type OptionalInt64 struct {
	// Value содержит показатель FFmpeg; его наличие определяется отдельно полем Present.
	Value int64
	// Present показывает, присутствовал ли численный показатель в завершённом отчёте FFmpeg.
	Present bool
}

// OptionalFloat64 сохраняет наличие необязательного дробного показателя FFmpeg.
type OptionalFloat64 struct {
	// Value содержит показатель FFmpeg; его наличие определяется отдельно полем Present.
	Value float64
	// Present показывает, присутствовал ли численный показатель в завершённом отчёте FFmpeg.
	Present bool
}

// FFmpegProgress хранит целый типизированный отчёт progress конкретной попытки FFmpeg.
type FFmpegProgress struct {
	// Frame содержит число обработанных кадров FFmpeg, TotalSizeBytes — размер выхода в байтах,
	// DupFrames — число дублированных кадров, а DropFrames — число отброшенных кадров.
	Frame, TotalSizeBytes, DupFrames, DropFrames OptionalUint64
	// FPS содержит частоту кадров FFmpeg, BitrateBitsPerSecond — его битрейт в битах в секунду, а
	// Speed — множитель скорости относительно медиавремени.
	FPS, BitrateBitsPerSecond, Speed OptionalFloat64
	// OutTimeUS содержит необязательное выходное медиавремя FFmpeg в знаковых микросекундах.
	OutTimeUS OptionalInt64
	// End отмечает отчёт progress=end, не подтверждая физическое завершение процесса.
	End bool
}

// FFmpegMetrics связывает последний progress FFmpeg с его свежестью и счётчиками потерь.
type FFmpegMetrics struct {
	// Status описывает доступность и пригодность текущего показателя.
	Status MetricStatus
	// Reason содержит типизированное объяснение текущего статуса.
	Reason MetricReason
	// HasProgress показывает наличие хотя бы одного завершённого отчёта progress этой попытки.
	HasProgress bool
	// LastProgressAt сохраняет время последнего принятого завершённого отчёта progress.
	LastProgressAt time.Time
	// ProgressLosses считает целиком потерянные повреждённые или превышающие пределы отчёты
	// progress.
	ProgressLosses uint64
	// SampledAt содержит время снятия согласованной выборки.
	SampledAt time.Time
	// Progress содержит последний завершённый отчёт progress точной попытки.
	Progress FFmpegProgress
	// DiagnosticLosses считает потерянные диагностические записи этой попытки.
	DiagnosticLosses uint64
}

// GPUMetric сохраняет точное целое значение в процентах либо байтах, согласно полю.
type GPUMetric struct {
	// Status описывает доступность и пригодность текущего показателя.
	Status MetricStatus
	// Reason содержит типизированное объяснение текущего статуса.
	Reason MetricReason
	// Value содержит измеренную величину в единицах соответствующего показателя.
	Value uint64
	// SampledAt содержит время текущей попытки измерения, а LastSuccessAt — время последнего
	// успешного значения.
	SampledAt, LastSuccessAt time.Time
	// SamplingPeriodUS сохраняет аппаратный период выборки NVML в микросекундах.
	SamplingPeriodUS uint64
}

// GPUDevice объединяет независимые показатели физического GPU по его UUID.
type GPUDevice struct {
	// UUID идентифицирует физическое устройство NVIDIA.
	UUID string
	// Utilization содержит загрузку GPU, MemoryUtilization — загрузку подсистемы памяти,
	// EncoderUtilization — загрузку NVENC, а DecoderUtilization — загрузку NVDEC; значения выражены
	// в процентах.
	Utilization, MemoryUtilization, EncoderUtilization, DecoderUtilization GPUMetric
	// MemoryUsedBytes содержит занятую память GPU, MemoryTotalBytes — полный объём памяти, а
	// MemoryFreeBytes — свободную память; значения сохраняются в точных целых байтах.
	MemoryUsedBytes, MemoryTotalBytes, MemoryFreeBytes GPUMetric
}

// GPUSnapshot сохраняет результат опроса GPU и срок свежести отдельных показателей.
type GPUSnapshot struct {
	// StaleAfter ограничивает свежесть показателя по времени его последнего успешного измерения.
	StaleAfter time.Duration
	// SampledAt содержит время снятия согласованной выборки.
	SampledAt time.Time
	// Status описывает доступность и пригодность текущего показателя.
	Status MetricStatus
	// Reason содержит типизированное объяснение текущего статуса.
	Reason MetricReason
	// Devices содержит независимые показатели каждого опрошенного устройства.
	Devices []GPUDevice
}

// validFloat допускает только конечное неотрицательное число.
func validFloat(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

// valid проверяет статус и значения скорости; available требует положительного измеренного окна,
// остальные статусы — нулевого значения. Полностью нулевой Rate обозначает отсутствие измерения.
func (r Rate) valid() bool {
	if r.Status == "" {
		return r == (Rate{})
	}
	if !r.Status.valid() || !r.Reason.valid() || !validFloat(r.Value) || r.Duration < 0 {
		return false
	}
	if r.Status == MetricAvailable {
		// Duration сохраняет монотонное измерение; UTC-метки могут сдвинуться при коррекции часов.
		return r.Duration > 0 && !r.WindowStart.IsZero() && !r.WindowEnd.IsZero()
	}
	return r.Value == 0
}

// valid требует время выборки, допустимые присутствующие кодеки и корректные показатели скорости.
func (m MediaMetrics) valid() bool {
	// f содержит распознанный формат для проверки допустимости присутствующих кодеков.
	f := m.Format
	return !m.SampledAt.IsZero() && (f.VideoCodec == "" || f.VideoCodec == CodecAV1 || f.VideoCodec == CodecH264) && (f.AudioCodec == "" || f.AudioCodec == CodecAAC) && m.Rates.FPS.valid() && m.Rates.PayloadBitrate.valid() && m.Rates.WrittenBitrate.valid()
}

// valid проверяет время выборки, статус, причину и конечные неотрицательные численные показатели
// FFmpeg.
func (m FFmpegMetrics) valid() bool {
	// p содержит прогресс FFmpeg для проверки конечности и неотрицательности дробных показателей.
	p := m.Progress
	return !m.SampledAt.IsZero() && (m.Status == "" || m.Status.valid()) && m.Reason.valid() && validFloat(p.FPS.Value) && validFloat(p.BitrateBitsPerSecond.Value) && validFloat(p.Speed.Value)
}

// utcRate переводит метки окна скорости в UTC, сохраняя измеренную длительность.
func utcRate(r Rate) Rate {
	r.WindowStart = r.WindowStart.UTC()
	r.WindowEnd = r.WindowEnd.UTC()
	return r
}

// utcMedia переводит метки медиавыборки и всех окон скоростей в UTC.
func utcMedia(m MediaMetrics) MediaMetrics {
	m.SampledAt = m.SampledAt.UTC()
	m.LastFrame = m.LastFrame.UTC()
	m.Rates.FPS = utcRate(m.Rates.FPS)
	m.Rates.PayloadBitrate = utcRate(m.Rates.PayloadBitrate)
	m.Rates.WrittenBitrate = utcRate(m.Rates.WrittenBitrate)
	return m
}
