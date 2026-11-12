// Package gpu собирает показатели всего GPU независимо от медиасессий.
package gpu

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"time"
)

// PollInterval задаёт двухсекундный период опроса нативного источника GPU.
const PollInterval = 2 * time.Second

// Reason обозначает ограниченную причину доступности снимка или отдельного измерения GPU.
type Reason string

const (
	// Available обозначает успешно полученные допустимые данные GPU.
	Available Reason = "available"
	// WarmingUp обозначает сборщик, который ещё не получил первый результат.
	WarmingUp Reason = "warming_up"
	// NotSupported обозначает измерение, которое нативный источник не поддерживает.
	NotSupported Reason = "not_supported"
	// UnsupportedPlatform обозначает платформу без выбранной реализации NVML.
	UnsupportedPlatform Reason = "unsupported_platform"
	// CGODisabled обозначает недоступность нативной привязки при выключенном CGO.
	CGODisabled Reason = "cgo_disabled"
	// LibraryUnavailable обозначает недоступную библиотеку NVML.
	LibraryUnavailable Reason = "library_unavailable"
	// DriverUnavailable обозначает недоступный драйвер NVIDIA.
	DriverUnavailable Reason = "driver_unavailable"
	// PermissionDenied обозначает отказ доступа к нативному измерению.
	PermissionDenied Reason = "permission_denied"
	// DeviceLost обозначает потерю ранее доступного устройства.
	DeviceLost Reason = "device_lost"
	// ProviderError обозначает общий отказ источника и заменяет неизвестные строки причин.
	ProviderError Reason = "provider_error"
	// InvalidSample обозначает недопустимые значения, время или идентичность полученной выборки.
	InvalidSample Reason = "invalid_sample"
	// BudgetExceeded обозначает снимок, превышающий настроенный предел устройств или логического
	// размера.
	BudgetExceeded Reason = "budget_exceeded"
	// Stale обозначает поле, возраст последнего успешного значения которого достиг срока свежести.
	Stale Reason = "stale"
	// Busy обозначает занятый источник измерений, которому нельзя назначить второго владельца.
	Busy Reason = "busy"
	// Closed обозначает завершённый сборщик, чьи текущие значения уже сняты.
	Closed Reason = "closed"
)

var (
	// ErrInvalid сообщает о недопустимых зависимостях или бюджетах создания сборщика GPU.
	ErrInvalid = errors.New("неверная конфигурация сборщика GPU")
	// ErrBusy сообщает об отказе закрепить уже занятый нативный источник за новым сборщиком.
	ErrBusy = errors.New("источник измерений GPU уже имеет владельца")
	// ErrPending сообщает, что тот же нативный работник ещё не подтвердил завершение.
	ErrPending = errors.New("завершение сборщика GPU ещё не подтверждено")
	// ErrShutdown сообщает, что последовательный Shutdown источника не подтвердил освобождение NVML.
	ErrShutdown = errors.New("NVML не подтвердил освобождение ресурсов")
)

// Config задаёт конечные бюджеты снимка GPU, срок свежести и ограничение ожидания остановки.
type Config struct {
	// MaxDevices ограничивает число устройств, передаваемое источнику и принимаемое в снимок.
	MaxDevices int
	// MaxUUIDBytes ограничивает длину UUID каждого устройства в байтах.
	MaxUUIDBytes int
	// MaxSnapshotBytes ограничивает логический вес снимка с устройствами и строками UUID.
	MaxSnapshotBytes uint64
	// StaleAfter задаёт возраст последнего успеха, после которого доступное поле становится stale.
	StaleAfter time.Duration
	// ShutdownTimeout ограничивает текущую попытку ожидания завершения нативного работника.
	ShutdownTimeout time.Duration
}

// snapshotBytes задаёт постоянную часть логического веса снимка до учёта устройств.
const snapshotBytes = uint64(128)

// deviceBytes задаёт постоянный логический вес устройства сверх байтов его UUID.
const deviceBytes = uint64(512)

// Validate проверяет положительные пределы устройств, размеров и сроков, а также представимость
// расчёта удерживаемого снимка.
func (c Config) Validate() error {
	if c.MaxDevices <= 0 || c.MaxUUIDBytes <= 0 || c.MaxSnapshotBytes < snapshotBytes || c.StaleAfter <= 0 || c.ShutdownTimeout <= 0 || uint64(c.MaxDevices) > uint64(math.MaxInt)/deviceBytes || uint64(c.MaxUUIDBytes) > math.MaxUint64-deviceBytes {
		return ErrInvalid
	}
	return nil
}

// Limits передаёт источнику предел числа устройств и длины UUID до выделения накопительной памяти.
type Limits struct {
	// MaxDevices ограничивает количество устройств в результате нативного опроса.
	MaxDevices int
	// MaxUUIDBytes ограничивает байты UUID, которые источник может сохранить для одного устройства.
	MaxUUIDBytes int
}

// Reading содержит проценты либо байты согласно имени поля; период NVML задан в микросекундах.
type Reading struct {
	// Value содержит процент загрузки либо число байтов памяти согласно полю устройства.
	Value uint64
	// SamplingPeriodUS содержит аппаратный период выборки NVML в микросекундах.
	SamplingPeriodUS uint32
	// Reason сообщает доступность конкретного нативного измерения.
	Reason Reason
	// SampledAt содержит время измерения источника; нулевое время успешного чтения заменяется
	// временем приёма.
	SampledAt time.Time
}

// DeviceSample содержит нативные измерения семи показателей одного устройства с его UUID.
type DeviceSample struct {
	// UUID идентифицирует устройство для сопоставления с прежними значениями снимка.
	UUID string
	// GPU содержит процент загрузки GPU; Memory содержит процент активности памяти; Encoder содержит
	// загрузку кодировщика; Decoder содержит загрузку декодировщика.
	GPU, Memory, Encoder, Decoder Reading
	// MemoryTotalBytes содержит объём памяти устройства; MemoryUsedBytes содержит занятые байты;
	// MemoryFreeBytes содержит свободные байты.
	MemoryTotalBytes, MemoryUsedBytes, MemoryFreeBytes Reading
}

// Sample переносит результат одного нативного опроса во владение сборщика.
type Sample struct {
	// Devices содержит устройства текущего опроса в пределах переданных источнику Limits.
	Devices []DeviceSample
	// Reason сообщает общий итог опроса, который при ошибке делает прежние поля недоступными.
	Reason Reason
}

// Provider вызывается последовательно одним владельцем, включая неотменяемые нативные вызовы.
// Poll соблюдает Limits до выделения памяти; возвращённый Sample передаётся во владение Collector.
type Provider interface {

	// Init инициализирует единственного нативного владельца и возвращает причину доступности без
	// конкурентного Poll или Shutdown.
	Init() Reason

	// Poll возвращает снимок в пределах Limits до выделения накопительной памяти и передаёт владение им
	// Collector; вызовы выполняются последовательно.
	Poll(Limits) Sample

	// Shutdown однократно завершает нативного владельца после возврата Poll и сообщает, подтверждено ли
	// освобождение ресурсов.
	Shutdown() Reason
}

// Field сохраняет LastSuccess и последнее значение при ошибке, не объявляя их свежими.
type Field struct {
	// Value сохраняет последнее успешно принятое значение показателя, в том числе при последующей
	// недоступности.
	Value uint64
	// SamplingPeriodUS сохраняет аппаратный период последнего успешного измерения в микросекундах.
	SamplingPeriodUS uint32
	// Available показывает возможность использовать сохранённое значение как доступное в текущем
	// снимке.
	Available bool
	// Reason объясняет текущую доступность поля независимо от сохранённого прежнего значения.
	Reason Reason
	// LastSuccess содержит время последнего успешного измерения для проверки свежести поля.
	LastSuccess time.Time
}

// DeviceSnapshot сохраняет значения и доступность семи показателей одного UUID между опросами.
type DeviceSnapshot struct {
	// UUID связывает текущие показатели с прежним снимком того же устройства.
	UUID string
	// GPU сохраняет загрузку GPU; Memory сохраняет активность памяти; Encoder сохраняет загрузку
	// кодировщика; Decoder сохраняет загрузку декодировщика, каждое со своей доступностью.
	GPU, Memory, Encoder, Decoder Field
	// MemoryTotalBytes сохраняет общий объём памяти; MemoryUsedBytes сохраняет занятые байты;
	// MemoryFreeBytes сохраняет свободные байты с отдельными временами успеха.
	MemoryTotalBytes, MemoryUsedBytes, MemoryFreeBytes Field
}

// Snapshot возвращает время общего опроса, устройства и доступность нативного источника.
type Snapshot struct {
	// SampledAt содержит время принятия последнего общего результата опроса сборщиком.
	SampledAt time.Time
	// Devices сохраняет последние показатели устройств; прежние значения остаются при ошибке общего
	// опроса.
	Devices []DeviceSnapshot
	// Reason сообщает общий итог источника независимо от доступности отдельных полей.
	Reason Reason
}

// Collector владеет одной последовательностью Init/Poll/Shutdown и точным обработчиком удаления.
type Collector struct {
	// config сохраняет проверенные бюджеты и сроки на весь срок работы сборщика.
	config Config
	// provider выполняет Init, Poll и Shutdown только в единственном работнике сборщика.
	provider Provider
	// publish передаёт принятый снимок без блокировки сборщика; вызов может завершиться после начала
	// Close.
	publish func(Snapshot)
	// remove снимает внешнюю регистрацию после Shutdown и очистки текущего снимка, до закрытия done.
	remove func()
	// mu защищает текущий снимок, закрытие допуска и результат нативного освобождения.
	mu sync.Mutex
	// current хранит последний согласованный снимок с сохранёнными значениями и временами успеха.
	current Snapshot
	// closing запрещает принимать новые результаты и обеспечивает однократное закрытие stop.
	closing bool
	// closeErr сохраняет отказ освобождения NVML для всех последующих ожиданий Close.
	closeErr error
	// stop запрашивает завершение между последовательными нативными вызовами, не прерывая
	// выполняемый Poll.
	stop chan struct{}
	// done подтверждает выход работника после Shutdown и обработчика remove.
	done chan struct{}
}

// Start не ждёт завершения Init. Обработчики вызываются без удержания блокировок
// и выполняют только короткую операцию с RAM; публикация сохраняет монотонные часы для core.
func Start(c Config, provider Provider, publish func(Snapshot), remove func()) (*Collector, error) {
	// err содержит отказ проверки конфигурации до закрепления нативного источника.
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if provider == nil || publish == nil || remove == nil {
		return nil, ErrInvalid
	}
	// owned предоставляет необязательную операцию закрепления источника; ok отмечает её поддержку
	// перед запретом второго сборщика.
	if owned, ok := provider.(interface {
		// claimCollector однократно закрепляет источник за Collector до Init; false запрещает запуск второго
		// сборщика с тем же источником.
		claimCollector() bool
	}); ok && !owned.claimCollector() {
		return nil, ErrBusy
	}
	// collector содержит нового владельца нативной последовательности с начальным состоянием
	// warming_up.
	collector := &Collector{config: c, provider: provider, publish: publish, remove: remove, current: Snapshot{Reason: WarmingUp}, stop: make(chan struct{}), done: make(chan struct{})}
	go collector.run()
	return collector, nil
}

// Done закрывается после выхода единственного работника, завершения Shutdown и вызова обработчика
// удаления.
func (c *Collector) Done() <-chan struct{} { return c.done }

// Snapshot возвращает независимую копию текущих полей с проверкой свежести на момент чтения. Внешние
// времена переводятся в UTC, прежние значения недоступных полей сохраняются.
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	// snapshot содержит независимую копию текущих полей с проверенной свежестью для внешнего чтения.
	snapshot := freshSnapshot(c.current, time.Now(), c.config.StaleAfter)
	snapshot.SampledAt = snapshot.SampledAt.UTC()
	// i указывает устройство копии, чьи времена последнего успеха переводятся в UTC.
	for i := range snapshot.Devices {
		// field указывает очередной показатель устройства в возвращаемой копии снимка.
		for _, field := range fields(&snapshot.Devices[i]) {
			field.LastSuccess = field.LastSuccess.UTC()
		}
	}
	return snapshot
}

// run последовательно выполняет Init, опросы и Shutdown, публикуя недоступность после ошибки и удаляя
// наблюдение при выходе. Неотменяемый нативный вызов удерживает этого работника и владельца.
func (c *Collector) run() {
	defer close(c.done)
	defer func() {
		// Убирает текущие значения и вызывает обработчик удаления до подтверждения Done.

		c.mu.Lock()
		c.current = Snapshot{Reason: Closed}
		c.mu.Unlock()
		c.remove()
	}()
	// reason сохраняет нормализованный итог Init и определяет возможность последующих Poll.
	reason := normalized(c.provider.Init())
	defer func() {
		// Вызывает Shutdown после окончания последовательности опросов и сохраняет ошибку неподтверждённого
		// освобождения NVML.

		if normalized(c.provider.Shutdown()) != Available {
			c.mu.Lock()
			c.closeErr = ErrShutdown
			c.mu.Unlock()
		}
	}()
	// ticker задаёт последовательные сроки нативных опросов и останавливается при выходе работника.
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		if reason == Available {
			c.accept(c.provider.Poll(Limits{MaxDevices: c.config.MaxDevices, MaxUUIDBytes: c.config.MaxUUIDBytes}))
		} else {
			c.accept(Sample{Reason: reason})
		}
		select {
		case <-c.stop:
			return
		case <-ticker.C:
		}
	}
}

// accept проверяет целый снимок до замены и отклоняет его, если при получении блокировки допуск уже
// закрыт. Ошибка сохраняет прежние значения и времена успеха. Публикация принятого снимка выполняется
// без блокировки и может произойти после начала Close; Done подтверждает завершение работника и
// обработчиков.
func (c *Collector) accept(sample Sample) {
	// now фиксирует время принятия результата для общего снимка и успешных полей без собственного
	// времени.
	now := time.Now()
	// reason содержит нормализованный итог опроса либо результат проверки его целого снимка.
	reason := normalized(sample.Reason)
	if reason == Available {
		reason = c.validateSample(sample)
	}
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return
	}
	if reason != Available {
		c.current.Reason = reason
		c.current.SampledAt = now
		// i указывает прежнее устройство, все поля которого делаются недоступными после общего
		// отказа.
		for i := range c.current.Devices {
			// field указывает прежнее поле, чьё значение сохраняется при обновлении причины
			// недоступности.
			for _, field := range fields(&c.current.Devices[i]) {
				field.Available = false
				field.Reason = reason
			}
		}
	} else {
		// next собирает новый успешный снимок отдельно до замены current.
		next := Snapshot{SampledAt: now, Reason: Available, Devices: make([]DeviceSnapshot, len(sample.Devices))}
		// i задаёт позицию устройства нового снимка; device содержит исходные измерения
		// соответствующего UUID.
		for i, device := range sample.Devices {
			// previous содержит прежние поля того же UUID для сохранения значений при
			// частичных отказах.
			previous := c.find(device.UUID)
			next.Devices[i] = DeviceSnapshot{UUID: strings.Clone(device.UUID)}
			// out указывает семь целевых полей нового снимка устройства.
			out := fields(&next.Devices[i])
			// prior указывает соответствующие прежние поля устройства для слияния с новым
			// опросом.
			prior := fields(&previous)
			// in указывает исходные нативные измерения в том же порядке, что out и prior.
			in := readings(&device)
			// j указывает соответствующий показатель во всех трёх массивах; первые четыре
			// показателя проверяются как проценты.
			for j := range out {
				*out[j] = merge(*prior[j], *in[j], now, j < 4)
			}
		}
		c.current = next
	}
	// published содержит отдельную копию принятого снимка с актуальной доступностью перед вызовом
	// publish без блокировки.
	published := freshSnapshot(c.current, now, c.config.StaleAfter)
	c.mu.Unlock()
	c.publish(published)
}

// find возвращает прежний снимок устройства по UUID либо пустой снимок; вызывающий код удерживает
// блокировку коллектора.
func (c *Collector) find(uuid string) DeviceSnapshot {
	// device содержит очередной прежний снимок устройства при поиске совпадающего UUID.
	for _, device := range c.current.Devices {
		if device.UUID == uuid {
			return device
		}
	}
	return DeviceSnapshot{}
}

// validateSample проверяет число и уникальность устройств, длины UUID и общий логический размер до
// принятия нового снимка.
func (c *Collector) validateSample(sample Sample) Reason {
	if len(sample.Devices) > c.config.MaxDevices {
		return BudgetExceeded
	}
	// weight накапливает логический вес проверяемого снимка, начиная с постоянной части.
	weight := snapshotBytes
	// i задаёт позицию текущего устройства; device содержит его UUID для проверки длины,
	// уникальности и веса.
	for i, device := range sample.Devices {
		if device.UUID == "" || len(device.UUID) > c.config.MaxUUIDBytes {
			return InvalidSample
		}
		// j указывает уже проверенное устройство перед i для отклонения повторного UUID.
		for j := range i {
			if sample.Devices[j].UUID == device.UUID {
				return InvalidSample
			}
		}
		// cost содержит логический вес текущего устройства вместе с байтами UUID.
		cost := deviceBytes + uint64(len(device.UUID))
		if cost > c.config.MaxSnapshotBytes-weight {
			return BudgetExceeded
		}
		weight += cost
	}
	return Available
}

// merge принимает корректное поле либо сохраняет прежнее значение с новой причиной недоступности.
// Проценты ограничены 0..100, будущее время отклоняется, а отсутствующее время успешного чтения
// заменяется текущим.
func merge(previous Field, reading Reading, now time.Time, percent bool) Field {
	// reason содержит нормализованную доступность измерения с учётом проверки процента и будущего
	// времени.
	reason := normalized(reading.Reason)
	if reason == Available && (percent && reading.Value > 100 || reading.SampledAt.After(now)) {
		reason = InvalidSample
	}
	if reason != Available {
		previous.Available = false
		previous.Reason = reason
		return previous
	}
	// at содержит время успешного измерения либо время приёма, если источник оставил его нулевым.
	at := reading.SampledAt
	if at.IsZero() {
		at = now
	}
	return Field{Value: reading.Value, SamplingPeriodUS: reading.SamplingPeriodUS, Available: true, Reason: Available, LastSuccess: at}
}

// freshSnapshot копирует список устройств и помечает просроченные доступные поля как stale без
// изменения прежних значений и времени успеха.
func freshSnapshot(snapshot Snapshot, now time.Time, staleAfter time.Duration) Snapshot {
	snapshot.Devices = append([]DeviceSnapshot(nil), snapshot.Devices...)
	// i указывает устройство скопированного снимка при проверке свежести его полей.
	for i := range snapshot.Devices {
		// field указывает поле скопированного снимка, которое помечается stale при достижении
		// срока свежести.
		for _, field := range fields(&snapshot.Devices[i]) {
			if field.Available && now.Sub(field.LastSuccess) >= staleAfter {
				field.Available = false
				field.Reason = Stale
			}
		}
	}
	return snapshot
}

// fields возвращает указатели на семь полей снимка устройства в порядке соответствующих нативных
// измерений.
func fields(d *DeviceSnapshot) [7]*Field {
	return [7]*Field{&d.GPU, &d.Memory, &d.Encoder, &d.Decoder, &d.MemoryTotalBytes, &d.MemoryUsedBytes, &d.MemoryFreeBytes}
}

// readings возвращает указатели на семь исходных измерений устройства в том же порядке, что fields.
func readings(d *DeviceSample) [7]*Reading {
	return [7]*Reading{&d.GPU, &d.Memory, &d.Encoder, &d.Decoder, &d.MemoryTotalBytes, &d.MemoryUsedBytes, &d.MemoryFreeBytes}
}

// normalized сохраняет известные причины доступности и заменяет произвольную строку на ProviderError.
func normalized(reason Reason) Reason {
	switch reason {
	case Available, WarmingUp, NotSupported, UnsupportedPlatform, CGODisabled, LibraryUnavailable, DriverUnavailable, PermissionDenied, DeviceLost, ProviderError, InvalidSample, BudgetExceeded, Stale, Busy, Closed:
		return reason
	default:
		return ProviderError
	}
}

// Close не запускает конкурентный Shutdown при зависшем Poll и допускает повторное ожидание.
func (c *Collector) Close(ctx context.Context) error {
	c.mu.Lock()
	if !c.closing {
		c.closing = true
		close(c.stop)
	}
	c.mu.Unlock()
	select {
	case <-c.done:
		return c.shutdownError()
	default:
	}
	// timer ограничивает текущий вызов ожидания Close, сохраняя того же владельца при незавершённом
	// Poll.
	timer := time.NewTimer(c.config.ShutdownTimeout)
	defer timer.Stop()
	select {
	case <-c.done:
		return c.shutdownError()
	case <-ctx.Done():
		return errors.Join(ErrPending, ctx.Err())
	case <-timer.C:
		return errors.Join(ErrPending, context.DeadlineExceeded)
	}
}

// shutdownError возвращает сохранённую ошибку освобождения NVML под блокировкой коллектора.
func (c *Collector) shutdownError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}
