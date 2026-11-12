package api

import (
	"strconv"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// RateDTO передаёт доступность оконной скорости, её значение и фактические границы измерения.
type RateDTO struct {
	// Status сообщает доступность скорости, включая прогрев, устаревание и сброс счётчика.
	Status string `json:"status"`
	// Reason объясняет причину текущей доступности скорости.
	Reason string `json:"reason"`
	// Value содержит измеренную скорость только при available; null сохраняет отличие недоступности
	// от нуля.
	Value *float64 `json:"value"`
	// WindowStart содержит начало фактического окна в UTC либо null при неизвестном времени.
	WindowStart *time.Time `json:"window_start"`
	// WindowEnd содержит конец фактического окна в UTC либо null при неизвестном времени.
	WindowEnd *time.Time `json:"window_end"`
	// DurationNS содержит точную длительность фактического окна в наносекундах.
	DurationNS Decimal `json:"duration_ns"`
}

// rateDTO раскрывает численное значение только доступного измерения и сохраняет причину и границы
// окна. Пустой статус заменяется на unavailable с причиной source_absent.
func rateDTO(r core.Rate) RateDTO {
	// status содержит публичную доступность скорости с заменой пустого состояния на unavailable.
	status := string(r.Status)
	// reason содержит публичную причину с заменой пустого состояния на source_absent.
	reason := string(r.Reason)
	if status == "" {
		status = "unavailable"
		reason = "source_absent"
	}
	// value содержит указатель на измеренную скорость только для доступного исходного состояния.
	var value *float64
	if r.Status == core.MetricAvailable {
		// v содержит отдельную копию измеренного значения для возвращаемого указателя.
		v := r.Value
		value = &v
	}
	return RateDTO{status, reason, value, timestamp(r.WindowStart), timestamp(r.WindowEnd), Decimal(r.Duration)}
}

// RatesDTO возвращает самостоятельные FPS и битрейты полезных и записанных байтов.
type RatesDTO struct {
	// FPS содержит скорость видеокадров за секунду и её окно.
	FPS RateDTO `json:"fps"`
	// PayloadBitrate содержит скорость полезных медиаданных в битах за секунду.
	PayloadBitrate RateDTO `json:"payload_bitrate"`
	// WrittenBitrate содержит скорость реально записанных транспортных байтов в битах за секунду.
	WrittenBitrate RateDTO `json:"written_bitrate"`
}

// ObservedFormatDTO передаёт фактически наблюдаемые кодеки, размеры видео и параметры аудио.
type ObservedFormatDTO struct {
	// VideoCodec содержит идентификатор наблюдаемого видеокодека.
	VideoCodec string `json:"video_codec"`
	// AudioCodec содержит идентификатор наблюдаемого аудиокодека.
	AudioCodec string `json:"audio_codec"`
	// Width содержит наблюдаемую ширину видеокадра в пикселях.
	Width uint32 `json:"width"`
	// Height содержит наблюдаемую высоту видеокадра в пикселях.
	Height uint32 `json:"height"`
	// SampleRateHz содержит наблюдаемую частоту аудиодискретизации в герцах.
	SampleRateHz uint32 `json:"sample_rate_hz"`
	// Channels содержит наблюдаемое число аудиоканалов.
	Channels uint16 `json:"channels"`
}

// MediaMetricsDTO передаёт точные счётчики и времена медиапроизводителя вместе с оконными скоростями.
type MediaMetricsDTO struct {
	// SampledAt содержит время согласованной выборки производителя в UTC либо null.
	SampledAt *time.Time `json:"sampled_at"`
	// VideoFrames содержит накопленное число видеокадров источника.
	VideoFrames Decimal `json:"video_frames"`
	// PayloadBytes содержит накопленные полезные байты медиа.
	PayloadBytes Decimal `json:"payload_bytes"`
	// WrittenBytes содержит накопленные байты записи транспортом.
	WrittenBytes Decimal `json:"written_bytes"`
	// AudioUnits содержит накопленное число аудиоединиц доступа.
	AudioUnits Decimal `json:"audio_units"`
	// Saturated сообщает о насыщении исходных счётчиков.
	Saturated bool `json:"saturated"`
	// LastFrame содержит время последнего наблюдённого кадра в UTC либо null.
	LastFrame *time.Time `json:"last_frame"`
	// Rates содержит отдельные рассчитанные FPS и битрейты с их доступностью.
	Rates RatesDTO `json:"rates"`
	// Format содержит фактически наблюдаемый медиаформат источника.
	Format ObservedFormatDTO `json:"format"`
}

// mediaDTO возвращает точные счётчики, времена получения, оконные скорости и фактически наблюдаемый
// формат без внутренних объектов выполнения.
func mediaDTO(m core.MediaMetrics) MediaMetricsDTO {
	// f содержит описание формата производителя для переноса точных параметров в DTO.
	f := m.Format
	return MediaMetricsDTO{timestamp(m.SampledAt), Decimal(m.VideoFrames), Decimal(m.PayloadBytes), Decimal(m.WrittenBytes), Decimal(m.AudioUnits), m.Saturated, timestamp(m.LastFrame), RatesDTO{rateDTO(m.Rates.FPS), rateDTO(m.Rates.PayloadBitrate), rateDTO(m.Rates.WrittenBitrate)}, ObservedFormatDTO{string(f.VideoCodec), string(f.AudioCodec), f.Width, f.Height, f.SampleRateHz, f.Channels}}
}

// InputMetricsDTO дополняет медиасчётчики входа точной эпохой его источника.
type InputMetricsDTO struct {
	// Media содержит счётчики, времена, скорости и формат текущей входящей публикации.
	Media MediaMetricsDTO `json:"media"`
	// SourceEpoch содержит эпоху входного медиапотока для различения смены источника.
	SourceEpoch Decimal `json:"source_epoch"`
}

// InputDTO передаёт текущее подключение OBS и необязательное измерение его медиапотока.
type InputDTO struct {
	// Connected показывает наличие текущей входящей публикации.
	Connected bool `json:"connected"`
	// ChangedAt содержит UTC-время изменения состояния подключения либо null.
	ChangedAt *time.Time `json:"changed_at"`
	// Metrics содержит доступный снимок входных измерений либо null при его отсутствии.
	Metrics *InputMetricsDTO `json:"metrics"`
}

// inputDTO возвращает состояние соединения и доступные метрики входа; при отсутствии наблюдения
// возвращает nil.
func inputDTO(i *core.InputSnapshot) *InputDTO {
	if i == nil {
		return nil
	}
	// v собирает публичное подключение и добавляет измерения только при наличии исходного снимка.
	v := &InputDTO{Connected: i.Connected, ChangedAt: timestamp(i.ChangedAt)}
	if i.Metrics != nil {
		v.Metrics = &InputMetricsDTO{mediaDTO(i.Metrics.MediaMetrics), Decimal(i.Metrics.SourceEpoch)}
	}
	return v
}

// GroupMetricsDTO разделяет наблюдения LIVE, заглушки и общего выхода медиагруппы.
type GroupMetricsDTO struct {
	// LiveInput содержит измерения LIVE-входа группы.
	LiveInput MediaMetricsDTO `json:"live_input"`
	// FallbackInput содержит измерения входа подготовленной заглушки группы.
	FallbackInput MediaMetricsDTO `json:"fallback_input"`
	// Output содержит измерения общего выхода группы после выбора источника.
	Output MediaMetricsDTO `json:"output"`
}

// groupMetricsDTO возвращает отдельные измерения входов LIVE и заглушки, а также общего выхода группы.
// При отсутствии метрик возвращает nil.
func groupMetricsDTO(m *core.GroupMetrics) *GroupMetricsDTO {
	if m == nil {
		return nil
	}
	return &GroupMetricsDTO{mediaDTO(m.LiveInput), mediaDTO(m.FallbackInput), mediaDTO(m.Output)}
}

// OutputMetricsDTO возвращает медиасчётчики, очереди, потери и отдельные точные показатели ACK
// отправителя.
type OutputMetricsDTO struct {
	// Media содержит общие счётчики и скорости медиапотока выхода.
	Media MediaMetricsDTO `json:"media"`
	// Counters содержит точные именованные счётчики отправителя, очереди, памяти и соединений без
	// дублирования ACK-полей.
	Counters map[string]Decimal `json:"counters"`
	// QueueDrops содержит точные счётчики потерь при допуске и синхронизации очереди.
	QueueDrops map[string]Decimal `json:"queue_drops"`
	// DeliveryDrops содержит точные счётчики потерь при доставке после очереди.
	DeliveryDrops map[string]Decimal `json:"delivery_drops"`
	// Resync показывает текущее восстановление синхронизации отправителя.
	Resync bool `json:"resync"`
	// LastWrite содержит UTC-время последней транспортной записи либо null.
	LastWrite *time.Time `json:"last_write"`
	// LastMedia содержит UTC-время последних медиаданных отправителя либо null.
	LastMedia           *time.Time `json:"last_media"`
	AckAheadEventsTotal Decimal    `json:"ack_ahead_events_total" doc:"Число новых достоверно измеренных опережающих ACK за запуск выхода с сохранением при переподключении; при переполнении насыщается. Нулевое значение не доказывает отсутствия расхождений: наблюдения во время Write и при неизвестной эпохе не измеряются."`
	AckAheadBytesLast   Decimal    `json:"ack_ahead_bytes_last" doc:"Точный размер последнего измеренного опережения ACK в незашифрованных байтах RTMP; обычный ACK его не обнуляет."`
	AckAheadBytesMax    Decimal    `json:"ack_ahead_bytes_max" doc:"Максимальный измеренный размер опережения ACK в незашифрованных байтах RTMP за запуск выхода."`
	AckLastAnomalyAt    *time.Time `json:"ack_last_anomaly_at" doc:"Время последнего измеренного опережения ACK в UTC; null до первого события."`
}

// outputMetricsDTO переводит счётчики, очереди, потери и ACK-диагностику в публичные точные значения.
// Поля ACK остаются самостоятельными полями metrics и не дублируются в counters.
func outputMetricsDTO(m *core.OutputMetrics) *OutputMetricsDTO {
	if m == nil {
		return nil
	}
	// q содержит причины потерь очереди; d содержит причины потерь доставки для отдельных публичных
	// словарей.
	q, d := m.QueueDrops, m.DeliveryDrops
	return &OutputMetricsDTO{mediaDTO(m.MediaMetrics), map[string]Decimal{"queued_packets": Decimal(m.QueuedPackets), "packet_bytes": Decimal(m.PacketBytes), "access_units": Decimal(m.AccessUnits), "pending_packets": Decimal(m.PendingPackets), "generation": Decimal(m.Generation), "in_flight_packet_bytes": Decimal(m.InFlightPacketBytes), "in_flight_config_bytes": Decimal(m.InFlightConfigBytes), "wire_buffer_bytes": Decimal(m.WireBufferBytes), "config_bytes": Decimal(m.ConfigBytes), "config_byte_capacity": Decimal(m.ConfigByteCapacity), "attempts": Decimal(m.Attempts), "connections": Decimal(m.Connections), "reconnects": Decimal(m.Reconnects), "queue_packets": Decimal(m.QueuePackets), "queue_capacity": Decimal(m.QueueCapacity), "queue_bytes": Decimal(m.QueueBytes), "queue_byte_capacity": Decimal(m.QueueByteCapacity)}, map[string]Decimal{"overflow": Decimal(q.Overflow), "oversized": Decimal(q.Oversized), "config_rejected": Decimal(q.ConfigRejected), "awaiting_keyframe": Decimal(q.AwaitingKeyframe), "audio_before_cut": Decimal(q.AudioBeforeCut), "source_reset": Decimal(q.SourceReset), "config_reset": Decimal(q.ConfigReset), "closed": Decimal(q.Closed)}, map[string]Decimal{"stale": Decimal(d.Stale), "transport": Decimal(d.Transport), "cancelled": Decimal(d.Cancelled), "terminal": Decimal(d.Terminal)}, m.Resync, timestamp(m.LastWrite), timestamp(m.LastMedia), Decimal(m.AckAheadEventsTotal), Decimal(m.AckAheadBytesLast), Decimal(m.AckAheadBytesMax), timestamp(m.AckLastAnomalyAt)}
}

// FFmpegProgressDTO передаёт присутствующие поля отчёта FFmpeg, различая отсутствие и измеренный ноль.
type FFmpegProgressDTO struct {
	// Frame содержит число обработанных кадров либо null при отсутствии поля отчёта.
	Frame *Decimal `json:"frame"`
	// TotalSizeBytes содержит размер выходных данных FFmpeg в байтах либо null.
	TotalSizeBytes *Decimal `json:"total_size_bytes"`
	// DupFrames содержит число дублированных кадров либо null.
	DupFrames *Decimal `json:"dup_frames"`
	// DropFrames содержит число отброшенных кадров либо null.
	DropFrames *Decimal `json:"drop_frames"`
	// FPS содержит сообщённую FFmpeg частоту кадров либо null.
	FPS *float64 `json:"fps"`
	// BitrateBPS содержит сообщённый FFmpeg битрейт в битах за секунду либо null.
	BitrateBPS *float64 `json:"bitrate_bps"`
	// Speed содержит сообщённую скорость обработки относительно реального времени либо null.
	Speed *float64 `json:"speed"`
	// OutTimeUS содержит знаковое время выхода FFmpeg в микросекундах как точную десятичную строку
	// либо null.
	OutTimeUS *string `json:"out_time_us"`
	// End показывает завершающий отчёт progress FFmpeg.
	End bool `json:"end"`
}

// optionalUint возвращает указатель на Decimal для присутствующего целого значения либо nil при его
// отсутствии, сохраняя измеренный ноль.
func optionalUint(v core.OptionalUint64) *Decimal {
	if !v.Present {
		return nil
	}
	// d содержит отдельное точное десятичное представление присутствующего целого поля отчёта.
	d := Decimal(v.Value)
	return &d
}

// optionalFloat возвращает указатель только присутствующего численного поля, сохраняя измеренный ноль.
func optionalFloat(v core.OptionalFloat64) *float64 {
	if !v.Present {
		return nil
	}
	// x содержит отдельную копию присутствующего вещественного поля отчёта.
	x := v.Value
	return &x
}

// FFmpegMetricsDTO передаёт доступность отчёта FFmpeg, времена и точные потери диагностики.
type FFmpegMetricsDTO struct {
	// Status сообщает доступность текущего отчёта FFmpeg.
	Status string `json:"status"`
	// Reason объясняет прогрев, устаревание или иную недоступность FFmpeg-измерения.
	Reason string `json:"reason"`
	// SampledAt содержит время снимка работника FFmpeg в UTC либо null.
	SampledAt *time.Time `json:"sampled_at"`
	// LastProgressAt содержит время последнего принятого отчёта progress в UTC либо null.
	LastProgressAt *time.Time `json:"last_progress_at"`
	// ProgressLosses содержит точное число потерянных отчётов progress.
	ProgressLosses Decimal `json:"progress_losses"`
	// DiagnosticLosses содержит точное число потерянных диагностических сообщений.
	DiagnosticLosses Decimal `json:"diagnostic_losses"`
	// Progress содержит поля отчёта только при available и HasProgress; иначе возвращается null.
	Progress *FFmpegProgressDTO `json:"progress"`
}

// ffmpegMetricsDTO сохраняет статусы, времена и потери FFmpeg и добавляет Progress только при
// MetricAvailable и HasProgress. Отсутствующие поля отчёта остаются nil, а знаковое время передаётся
// десятичной строкой.
func ffmpegMetricsDTO(m *core.FFmpegMetrics) *FFmpegMetricsDTO {
	if m == nil {
		return nil
	}
	// status содержит публичную доступность FFmpeg; reason содержит её причину с заменой пустого
	// состояния на source_absent.
	status, reason := string(m.Status), string(m.Reason)
	if status == "" {
		status = "unavailable"
		reason = "source_absent"
	}
	// v собирает доступность, времена и потери FFmpeg до добавления допустимого отчёта.
	v := &FFmpegMetricsDTO{Status: status, Reason: reason, SampledAt: timestamp(m.SampledAt), LastProgressAt: timestamp(m.LastProgressAt), ProgressLosses: Decimal(m.ProgressLosses), DiagnosticLosses: Decimal(m.DiagnosticLosses)}
	if m.Status == core.MetricAvailable && m.HasProgress {
		// p содержит исходный присутствующий отчёт FFmpeg для преобразования необязательных
		// полей.
		p := m.Progress
		// out содержит знаковое время отчёта как строку либо nil при отсутствии OutTimeUS.
		var out *string
		if p.OutTimeUS.Present {
			// x содержит точную десятичную запись знакового времени выхода в микросекундах.
			x := strconv.FormatInt(p.OutTimeUS.Value, 10)
			out = &x
		}
		v.Progress = &FFmpegProgressDTO{optionalUint(p.Frame), optionalUint(p.TotalSizeBytes), optionalUint(p.DupFrames), optionalUint(p.DropFrames), optionalFloat(p.FPS), optionalFloat(p.BitrateBitsPerSecond), optionalFloat(p.Speed), out, p.End}
	}
	return v
}

// GPUMetricDTO передаёт точное значение GPU только при доступности, сохраняя времена и причину отказа.
type GPUMetricDTO struct {
	// Status сообщает текущую доступность отдельного GPU-показателя.
	Status string `json:"status"`
	// Reason объясняет текущую недоступность или успешное состояние GPU-показателя.
	Reason string `json:"reason"`
	// Value содержит точные проценты или байты согласно показателю только при available; иначе null.
	Value *Decimal `json:"value"`
	// SampledAt содержит UTC-время снимка показателя либо null.
	SampledAt *time.Time `json:"sampled_at"`
	// LastSuccessAt содержит UTC-время последнего успешного измерения либо null.
	LastSuccessAt *time.Time `json:"last_success_at"`
	// SamplingPeriodUS содержит аппаратный период выборки GPU в микросекундах.
	SamplingPeriodUS Decimal `json:"sampling_period_us"`
}

// gpuMetricDTO раскрывает точное значение GPU только при available, сохраняя причины недоступности и
// времена последнего успеха.
func gpuMetricDTO(m core.GPUMetric) GPUMetricDTO {
	// value содержит точное значение доступного показателя либо nil при недоступности.
	var value *Decimal
	if m.Status == core.MetricAvailable {
		// v содержит отдельную Decimal-копию значения GPU для возвращаемого указателя.
		v := Decimal(m.Value)
		value = &v
	}
	return GPUMetricDTO{string(m.Status), string(m.Reason), value, timestamp(m.SampledAt), timestamp(m.LastSuccessAt), Decimal(m.SamplingPeriodUS)}
}

// GPUDeviceDTO возвращает UUID и независимую доступность семи показателей одного устройства.
type GPUDeviceDTO struct {
	// UUID содержит идентификатор устройства NVIDIA.
	UUID string `json:"uuid"`
	// Utilization содержит процент загрузки GPU с его доступностью.
	Utilization GPUMetricDTO `json:"utilization"`
	// MemoryUtilization содержит процент активности памяти GPU с его доступностью.
	MemoryUtilization GPUMetricDTO `json:"memory_utilization"`
	// EncoderUtilization содержит процент загрузки видеокодировщика и аппаратный период измерения.
	EncoderUtilization GPUMetricDTO `json:"encoder_utilization"`
	// DecoderUtilization содержит процент загрузки видеодекодировщика и аппаратный период измерения.
	DecoderUtilization GPUMetricDTO `json:"decoder_utilization"`
	// MemoryUsedBytes содержит число занятых байтов памяти устройства с доступностью.
	MemoryUsedBytes GPUMetricDTO `json:"memory_used_bytes"`
	// MemoryTotalBytes содержит общий объём памяти устройства в байтах с доступностью.
	MemoryTotalBytes GPUMetricDTO `json:"memory_total_bytes"`
	// MemoryFreeBytes содержит число свободных байтов памяти устройства с доступностью.
	MemoryFreeBytes GPUMetricDTO `json:"memory_free_bytes"`
}

// GPUDTO возвращает системный снимок GPU независимо от отдельных медиасессий.
type GPUDTO struct {
	// Status сообщает общую доступность источника GPU.
	Status string `json:"status"`
	// Reason объясняет общий итог источника GPU.
	Reason string `json:"reason"`
	// SampledAt содержит UTC-время последнего общего опроса либо null.
	SampledAt *time.Time `json:"sampled_at"`
	// StaleAfterNS содержит точный срок свежести GPU-полей в наносекундах.
	StaleAfterNS Decimal `json:"stale_after_ns"`
	// Devices содержит текущие устройства с независимыми статусами показателей.
	Devices []GPUDeviceDTO `json:"devices"`
}

// gpuDTO переводит системный снимок GPU в публичные DTO устройств и сохраняет срок свежести точными
// наносекундами.
func gpuDTO(g *core.GPUSnapshot) *GPUDTO {
	if g == nil {
		return nil
	}
	// v собирает публичный системный снимок с массивом устройств, который остаётся пустым массивом
	// JSON при отсутствии устройств.
	v := &GPUDTO{string(g.Status), string(g.Reason), timestamp(g.SampledAt), Decimal(g.StaleAfter), make([]GPUDeviceDTO, 0, len(g.Devices))}
	// d содержит очередное устройство доменного снимка для преобразования семи независимых
	// измерений.
	for _, d := range g.Devices {
		v.Devices = append(v.Devices, GPUDeviceDTO{d.UUID, gpuMetricDTO(d.Utilization), gpuMetricDTO(d.MemoryUtilization), gpuMetricDTO(d.EncoderUtilization), gpuMetricDTO(d.DecoderUtilization), gpuMetricDTO(d.MemoryUsedBytes), gpuMetricDTO(d.MemoryTotalBytes), gpuMetricDTO(d.MemoryFreeBytes)})
	}
	return v
}

// ObservationDTO передаёт публичную идентичность текущего наблюдателя, ревизию и безопасное состояние.
type ObservationDTO struct {
	// InstanceID соединяет публичную эпоху с локальным ID наблюдателя; нулевой ID передаётся пустой
	// строкой.
	InstanceID string `json:"instance_id"`
	// Revision содержит точную ревизию текущего наблюдения.
	Revision Decimal `json:"revision"`
	// ObservedAt содержит UTC-время последнего наблюдения либо null.
	ObservedAt *time.Time `json:"observed_at"`
	// State содержит наблюдаемое состояние владельца, отдельно от сохранённой истории.
	State string `json:"state"`
	// Failure содержит безопасное обозначение ошибки наблюдения либо null.
	Failure *FailureDTO `json:"failure"`
}

// observationDTO возвращает публичный идентификатор наблюдения, точную ревизию, состояние и безопасную
// ошибку без исходной диагностики.
func observationDTO(epoch core.ID, s core.ObservationStamp, state string, failed bool) ObservationDTO {
	// f содержит безопасную observation_failed только при подтверждённом признаке отказа.
	var f *FailureDTO
	if failed {
		f = failure("observation_failed")
	}
	return ObservationDTO{publicInstance(epoch, s.InstanceID), Decimal(s.Revision), timestamp(s.ObservedAt), state, f}
}

// publicInstance связывает ненулевой локальный ID с публичным epoch процесса; нулевой ID остаётся
// пустой строкой.
func publicInstance(epoch core.ID, id uint64) string {
	if id == 0 {
		return ""
	}
	return string(epoch) + ":" + strconv.FormatUint(id, 10)
}

// RuntimeGroupDTO возвращает текущее наблюдение конкретной группы, выбранный источник и измерения.
type RuntimeGroupDTO struct {
	// ObservationDTO содержит публичную идентичность, ревизию и состояние этого экземпляра группы.
	ObservationDTO
	// GroupID указывает сохранённую конфигурационную группу.
	GroupID string `json:"group_id"`
	// ProfileID указывает конфигурационный профиль текущего экземпляра группы.
	ProfileID string `json:"profile_id"`
	// Source содержит наблюдаемый выбранный источник группы.
	Source string `json:"source"`
	// SourceRevision содержит точную ревизию выбора источника группы.
	SourceRevision Decimal `json:"source_revision"`
	// Metrics содержит независимые измерения двух входов и общего выхода группы либо null.
	Metrics *GroupMetricsDTO `json:"metrics"`
}

// RuntimeOutputDTO возвращает текущее наблюдение точного экземпляра выхода и его измерения.
type RuntimeOutputDTO struct {
	// ObservationDTO содержит публичную идентичность, ревизию и состояние наблюдателя выхода.
	ObservationDTO
	// OutputID указывает сохранённый выход, чей текущий экземпляр наблюдается.
	OutputID string `json:"output_id"`
	// Metrics содержит измерения отправителя выхода либо null.
	Metrics *OutputMetricsDTO `json:"metrics"`
}

// RuntimeFFmpegDTO возвращает наблюдение текущего логического работника FFmpeg и его роль.
type RuntimeFFmpegDTO struct {
	// ObservationDTO содержит публичную идентичность, ревизию и состояние работника FFmpeg.
	ObservationDTO
	// GroupInstanceID связывает работника с публичным текущим экземпляром медиагруппы.
	GroupInstanceID string `json:"group_instance_id"`
	// Role содержит роль FFmpeg в группе.
	Role string `json:"role"`
	// Metrics содержит доступность и поля progress работника либо null.
	Metrics *FFmpegMetricsDTO `json:"metrics"`
}

// RuntimeSessionDTO собирает текущих наблюдателей Session, её входа, групп, выходов и FFmpeg.
type RuntimeSessionDTO struct {
	// SessionID указывает Session текущего оперативного снимка.
	SessionID string `json:"session_id"`
	// Observation содержит наблюдение владельца Session либо null при его отсутствии.
	Observation *ObservationDTO `json:"observation"`
	// Input содержит текущее состояние входящей публикации либо null.
	Input *InputDTO `json:"input"`
	// Groups содержит текущие самостоятельные экземпляры наблюдаемых групп.
	Groups []RuntimeGroupDTO `json:"groups"`
	// Outputs содержит текущие самостоятельные экземпляры наблюдаемых выходов.
	Outputs []RuntimeOutputDTO `json:"outputs"`
	// FFmpeg содержит текущие логические работники FFmpeg с их ролями.
	FFmpeg []RuntimeFFmpegDTO `json:"ffmpeg"`
}

// runtimeSessionDTO формирует текущий снимок Session из её точных наблюдателей, сохраняя
// самостоятельные группы, выходы и роли FFmpeg без внутренних данных допуска.
func runtimeSessionDTO(s core.SessionRuntimeSnapshot) RuntimeSessionDTO {
	// epoch содержит публичную эпоху снимка для формирования идентичностей всех его наблюдателей.
	epoch := s.Cursor.Epoch
	// r собирает публичную Session с пустыми JSON-массивами для отсутствующих дочерних наблюдений.
	r := RuntimeSessionDTO{SessionID: string(s.Fence.SessionID), Input: inputDTO(s.Input), Groups: []RuntimeGroupDTO{}, Outputs: []RuntimeOutputDTO{}, FFmpeg: []RuntimeFFmpegDTO{}}
	if s.Session != nil {
		// state содержит наблюдаемое состояние Session либо UNKNOWN до первого значения.
		state := "UNKNOWN"
		// failed отмечает наличие исходной ошибки Session для безопасного обозначения
		// observation_failed.
		failed := false
		if s.Session.Value != nil {
			state = string(s.Session.Value.State)
			failed = s.Session.Value.Error != ""
		}
		// o содержит подготовленное публичное наблюдение Session для необязательного указателя.
		o := observationDTO(epoch, s.Session.Stamp, state, failed)
		r.Observation = &o
	}
	// g содержит очередной точный экземпляр наблюдаемой группы.
	for _, g := range s.Groups {
		// state содержит состояние группы либо UNKNOWN; source содержит выбранный источник либо
		// NONE до первого значения.
		state, source := "UNKNOWN", "NONE"
		// failed отмечает исходную ошибку группы без передачи её диагностического текста.
		failed := false
		// revision содержит ревизию выбора источника группы либо ноль до первого наблюдения.
		var revision uint64
		if g.Value != nil {
			state = string(g.Value.State)
			source = string(g.Value.Source)
			failed = g.Value.Error != ""
			revision = g.Value.SourceRevision
		}
		r.Groups = append(r.Groups, RuntimeGroupDTO{observationDTO(epoch, g.Stamp, state, failed), string(g.ConfigGroupID), string(g.ConfigProfileID), source, Decimal(revision), groupMetricsDTO(g.Metrics)})
	}
	// o содержит очередное точное наблюдение исходящего выхода.
	for _, o := range s.Outputs {
		// state содержит состояние выхода либо UNKNOWN до первого значения.
		state := "UNKNOWN"
		// failed отмечает исходную ошибку выхода для безопасного публичного отказа.
		failed := false
		if o.Value != nil {
			state = string(o.Value.State)
			failed = o.Value.Error != ""
		}
		r.Outputs = append(r.Outputs, RuntimeOutputDTO{observationDTO(epoch, o.Stamp, state, failed), string(o.Execution.OutputID), outputMetricsDTO(o.Metrics)})
	}
	// f содержит очередное точное наблюдение логического работника FFmpeg.
	for _, f := range s.FFmpeg {
		// state содержит состояние работника FFmpeg либо UNKNOWN до первого значения.
		state := "UNKNOWN"
		// failed отмечает исходную ошибку работника без раскрытия диагностического текста.
		failed := false
		if f.Value != nil {
			state = string(f.Value.State)
			failed = f.Value.Error != ""
		}
		r.FFmpeg = append(r.FFmpeg, RuntimeFFmpegDTO{observationDTO(epoch, f.Stamp, state, failed), publicInstance(epoch, f.GroupInstanceID), string(f.Role), ffmpegMetricsDTO(f.Metrics)})
	}
	return r
}

// AnchorDTO возвращает атомарный снимок RAM вместе с позицией для последующей SSE-подписки.
type AnchorDTO struct {
	// Cursor содержит публичный курсор той же атомарной границы снимка.
	Cursor string `json:"cursor"`
	// Sessions содержит текущие наблюдения Session на границе снимка.
	Sessions []RuntimeSessionDTO `json:"sessions"`
	// GPU содержит системное наблюдение GPU той же границы либо null.
	GPU *GPUDTO `json:"gpu"`
}

// SessionMetricsDTO явно разделяет доступный текущий снимок и причину его отсутствия.
type SessionMetricsDTO struct {
	// Availability содержит available или unavailable для текущего исполнения Session.
	Availability string `json:"availability"`
	// Reason объясняет отсутствие текущего наблюдения либо терминальную Session.
	Reason string `json:"reason"`
	// Snapshot содержит текущий оперативный снимок только при available; иначе null.
	Snapshot *RuntimeSessionDTO `json:"snapshot"`
}

// RuntimeEventDTO передаёт безопасные типизированные данные одного события RAM-потока.
type RuntimeEventDTO struct {
	// Cursor содержит публичную позицию этого события для продолжения подписки.
	Cursor string `json:"cursor"`
	// Kind содержит вид изменения оперативного состояния.
	Kind string `json:"kind"`
	// Owner содержит вид владельца, к которому относится событие.
	Owner string `json:"owner"`
	// SessionID указывает Session события, если оно относится к медиасессии.
	SessionID string `json:"session_id"`
	// OutputID указывает выход события, если оно относится к исходящему исполнению.
	OutputID string `json:"output_id"`
	// GroupID указывает конфигурационную группу события.
	GroupID string `json:"group_id"`
	// ProfileID указывает конфигурационный профиль события.
	ProfileID string `json:"profile_id"`
	// Observation содержит публичную идентичность и состояние точного наблюдателя события.
	Observation ObservationDTO `json:"observation"`
	// GroupInstanceID связывает событие работника с публичным экземпляром группы.
	GroupInstanceID string `json:"group_instance_id"`
	// Source содержит наблюдаемый источник группы в событии.
	Source string `json:"source"`
	// SourceRevision содержит точную ревизию выбора источника в событии.
	SourceRevision Decimal `json:"source_revision"`
	// Role содержит роль FFmpeg, если событие относится к работнику.
	Role string `json:"role"`
	// AttemptActive сообщает активность наблюдаемой попытки в событии.
	AttemptActive bool `json:"attempt_active"`
	// Reason содержит ограниченную причину изменения или недоступности события.
	Reason string `json:"reason"`
	// Input содержит типизированный снимок входа, если он приложен к событию.
	Input *InputDTO `json:"input"`
	// Group содержит измерения группы, если они приложены к событию.
	Group *GroupMetricsDTO `json:"group"`
	// Output содержит измерения выхода, если они приложены к событию.
	Output *OutputMetricsDTO `json:"output"`
	// FFmpeg содержит измерения работника, если они приложены к событию.
	FFmpeg *FFmpegMetricsDTO `json:"ffmpeg"`
	// GPU содержит системный снимок GPU, если он приложен к событию.
	GPU *GPUDTO `json:"gpu"`
}

// runtimeEventDTO переводит одно оперативное событие в безопасные данные со своим cursor, владельцем,
// состоянием и типизированными измерениями.
func runtimeEventDTO(e core.RuntimeEvent) RuntimeEventDTO {
	// state выбирает наблюдаемое состояние, отдавая отдельному OutputState приоритет для события
	// выхода.
	state := string(e.State)
	if e.OutputState != "" {
		state = string(e.OutputState)
	}
	return RuntimeEventDTO{encodeCursor(e.Cursor), string(e.Kind), string(e.Owner), string(e.Session.SessionID), string(e.Execution.OutputID), string(e.ConfigGroupID), string(e.ConfigProfileID), observationDTO(e.Cursor.Epoch, e.Stamp, state, e.Reason == core.ReasonObservationFailed), publicInstance(e.Cursor.Epoch, e.GroupInstanceID), string(e.Source), Decimal(e.SourceRevision), string(e.Role), e.AttemptActive, string(e.Reason), inputDTO(e.Input), groupMetricsDTO(e.Group), outputMetricsDTO(e.Output), ffmpegMetricsDTO(e.FFmpeg), gpuDTO(e.GPU)}
}
