// Package config загружает и проверяет неизменяемые настройки сервера.
package config

import (
	"log/slog"
	"time"
)

// Router задаёт серверные бюджеты маршрутизатора медиапакетов для создаваемых сессий.
type Router struct {
	// MaxConsumers ограничивает число потребителей пакетов одного маршрутизатора.
	MaxConsumers uint32 `yaml:"max_consumers"`
	// MaxRetainedBytes задаёт логический бюджет удерживаемых маршрутизатором пакетов и конфигураций
	// в байтах.
	MaxRetainedBytes uint64 `yaml:"max_retained_bytes"`
	// MaxConfigBytes ограничивает размер конфигурации кодека в байтах для согласования с очередями
	// потребителей.
	MaxConfigBytes uint64 `yaml:"max_config_bytes"`
}

// Queue задаёт конечные бюджеты очереди медиапакетов и срок восстановления синхронизации.
type Queue struct {
	// MaxPackets ограничивает количество пакетов, удерживаемых одной очередью.
	MaxPackets uint32 `yaml:"max_packets"`
	// MaxBytes ограничивает суммарный бюджет байтов пакетов одной очереди.
	MaxBytes uint64 `yaml:"max_bytes"`
	// MaxPacketBytes ограничивает размер данных одного принимаемого пакета в байтах.
	MaxPacketBytes uint64 `yaml:"max_packet_bytes"`
	// MaxConfigBytes ограничивает размер конфигурации кодека, допускаемой в очередь, в байтах.
	MaxConfigBytes uint64 `yaml:"max_config_bytes"`
	// ResyncTimeout ограничивает ожидание восстановления синхронизации очереди.
	ResyncTimeout time.Duration `yaml:"resync_timeout"`
}

// Ingress задаёт бюджеты и сроки входного сервера Enhanced RTMP.
type Ingress struct {
	// MaxConnections ограничивает число одновременно принимаемых RTMP-соединений.
	MaxConnections uint32 `yaml:"max_connections"`
	// MaxChunkStreams ограничивает число потоков блоков RTMP в одном соединении.
	MaxChunkStreams uint32 `yaml:"max_chunk_streams"`
	// MaxMessageBytes ограничивает размер собираемого сообщения RTMP в байтах.
	MaxMessageBytes uint32 `yaml:"max_message_bytes"`
	// MaxChunkBytes ограничивает размер блока RTMP в байтах.
	MaxChunkBytes uint32 `yaml:"max_chunk_bytes"`
	// MaxFragments ограничивает число фрагментов собираемых RTMP-сообщений.
	MaxFragments uint32 `yaml:"max_fragments"`
	// MaxRetainedBytes задаёт логический резерв входного соединения для данных, копий и структурных
	// слотов в байтах.
	MaxRetainedBytes uint64 `yaml:"max_retained_bytes"`
	// MaxControlBytes ограничивает размер управляющего RTMP-сообщения в байтах.
	MaxControlBytes uint32 `yaml:"max_control_bytes"`
	// MaxConfigBytes ограничивает размер входной конфигурации кодека в байтах.
	MaxConfigBytes uint32 `yaml:"max_config_bytes"`
	// MaxAMFBytes ограничивает размер принимаемых AMF-данных в байтах.
	MaxAMFBytes uint32 `yaml:"max_amf_bytes"`
	// MaxAMFDepth ограничивает глубину вложенности разбираемых AMF-данных.
	MaxAMFDepth uint32 `yaml:"max_amf_depth"`
	// MaxAMFElements ограничивает число элементов разбираемых AMF-данных.
	MaxAMFElements uint32 `yaml:"max_amf_elements"`
	// MaxReadAheadBytes задаёт предел опережающего чтения входного RTMP в байтах.
	MaxReadAheadBytes uint32 `yaml:"max_read_ahead_bytes"`
	// HandshakeTimeout ограничивает начальное согласование входного RTMP-соединения.
	HandshakeTimeout time.Duration `yaml:"handshake_timeout"`
	// ReadTimeout задаёт конечный срок чтения входного RTMP-соединения.
	ReadTimeout time.Duration `yaml:"read_timeout"`
	// WriteTimeout задаёт конечный срок записи ответов входного RTMP-соединения.
	WriteTimeout time.Duration `yaml:"write_timeout"`
	// ShutdownTimeout ограничивает ожидание завершения входного RTMP-сервера.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// Outbound задаёт бюджеты и сроки исходящего RTMP/RTMPS-соединения отправителя.
type Outbound struct {
	// MaxChunkStreams ограничивает число потоков блоков исходящего RTMP-соединения.
	MaxChunkStreams uint32 `yaml:"max_chunk_streams"`
	// MaxMessageBytes ограничивает размер RTMP-сообщения исходящего транспорта в байтах.
	MaxMessageBytes uint32 `yaml:"max_message_bytes"`
	// MaxChunkBytes ограничивает размер блока исходящего RTMP в байтах.
	MaxChunkBytes uint32 `yaml:"max_chunk_bytes"`
	// MaxFragments ограничивает число удерживаемых фрагментов исходящего RTMP-транспорта.
	MaxFragments uint32 `yaml:"max_fragments"`
	// MaxControlBytes ограничивает размер управляющего RTMP-сообщения в байтах.
	MaxControlBytes uint32 `yaml:"max_control_bytes"`
	// MaxConfigBytes ограничивает размер конфигурации кодека исходящего транспорта в байтах.
	MaxConfigBytes uint32 `yaml:"max_config_bytes"`
	// MaxAMFBytes ограничивает размер AMF-команд исходящего RTMP в байтах.
	MaxAMFBytes uint32 `yaml:"max_amf_bytes"`
	// MaxAMFDepth ограничивает глубину AMF-данных исходящего RTMP-транспорта.
	MaxAMFDepth uint32 `yaml:"max_amf_depth"`
	// MaxAMFElements ограничивает число элементов AMF-данных исходящего транспорта.
	MaxAMFElements uint32 `yaml:"max_amf_elements"`
	// ChunkSize задаёт используемый размер отправляемого блока RTMP в байтах.
	ChunkSize uint32 `yaml:"chunk_size"`
	// MaxControlPending ограничивает число ожидающих управляющих ответов исходящего соединения.
	MaxControlPending uint32 `yaml:"max_control_pending"`
	// MaxRetainedBytes задаёт логический бюджет исходящего соединения с буферами, AMF и ожидающими
	// ответами в байтах.
	MaxRetainedBytes uint64 `yaml:"max_retained_bytes"`
	// DialTimeout ограничивает установление исходящего TCP-соединения с назначением.
	DialTimeout time.Duration `yaml:"dial_timeout"`
	// TLSTimeout ограничивает согласование TLS при подключении RTMPS.
	TLSTimeout time.Duration `yaml:"tls_timeout"`
	// StartupTimeout ограничивает начальную подготовку исходящей RTMP-публикации.
	StartupTimeout time.Duration `yaml:"startup_timeout"`
	// MessageTimeout ограничивает ожидание сообщения исходящего RTMP-транспорта.
	MessageTimeout time.Duration `yaml:"message_timeout"`
	// WriteTimeout ограничивает запись исходящих RTMP-данных.
	WriteTimeout time.Duration `yaml:"write_timeout"`
}

// Container задаёт серверные пределы fMP4, используемые медиатрактом и проверкой файлов заглушек.
type Container struct {
	// MaxBoxBytes ограничивает размер отдельного блока MP4 вместе с его заголовком в байтах.
	MaxBoxBytes uint64 `yaml:"max_box_bytes"`
	// MaxInitBytes ограничивает размер инициализации fMP4 с заголовками в байтах.
	MaxInitBytes uint64 `yaml:"max_init_bytes"`
	// MaxMoofBytes ограничивает размер блока moof вместе с заголовком в байтах.
	MaxMoofBytes uint64 `yaml:"max_moof_bytes"`
	// MaxMdatBytes ограничивает размер блока mdat вместе с заголовком в байтах.
	MaxMdatBytes uint64 `yaml:"max_mdat_bytes"`
	// MaxSampleBytes ограничивает данные одной единицы доступа fMP4 в байтах без заголовка блока.
	MaxSampleBytes uint64 `yaml:"max_sample_bytes"`
	// MaxExtradataBytes ограничивает данные конфигурации кодека fMP4 в байтах.
	MaxExtradataBytes uint64 `yaml:"max_extradata_bytes"`
	// MaxSamples ограничивает число описаний единиц доступа при обработке fMP4.
	MaxSamples uint32 `yaml:"max_samples"`
}

// Progress задаёт бюджеты разбора отчётов FFmpeg progress.
type Progress struct {
	// MaxLineBytes ограничивает строку progress в байтах.
	MaxLineBytes int `yaml:"max_line_bytes"`
	// MaxReportBytes ограничивает целый отчёт progress в байтах.
	MaxReportBytes int `yaml:"max_report_bytes"`
	// MaxFields ограничивает число полей одного отчёта progress.
	MaxFields int `yaml:"max_fields"`
}

// ProcessLimits задаёт конечные бюджеты журнала владельца процессов и передаваемых команд.
type ProcessLimits struct {
	// MaxOperations ограничивает число учитываемых операций владельца процессов.
	MaxOperations int `yaml:"max_operations"`
	// MaxRecordBytes ограничивает размер записи журнала владельца процессов в байтах.
	MaxRecordBytes int64 `yaml:"max_record_bytes"`
	// MaxCommandArgs ограничивает число аргументов управляемой команды.
	MaxCommandArgs int `yaml:"max_command_args"`
	// MaxCommandBytes ограничивает суммарный размер имени программы и аргументов управляемой команды
	// в байтах.
	MaxCommandBytes int `yaml:"max_command_bytes"`
	// MaxExtraFiles ограничивает число дополнительных файлов, передаваемых управляемому процессу.
	MaxExtraFiles int `yaml:"max_extra_files"`
	// PageSize задаёт максимальное число записей страницы восстановления и сверки владельца
	// процессов.
	PageSize int `yaml:"page_size"`
}

// Runner задаёт сроки и бюджеты команд подготовки заглушек.
type Runner struct {
	// Timeout ограничивает отдельный запуск внешнего инструмента подготовки.
	Timeout time.Duration `yaml:"timeout"`
	// MaxArgs ограничивает число аргументов команды подготовки.
	MaxArgs int `yaml:"max_args"`
	// MaxCommandBytes ограничивает суммарный размер имени программы и аргументов команды подготовки
	// в байтах.
	MaxCommandBytes uint64 `yaml:"max_command_bytes"`
	// MaxDiagnosticBytes ограничивает удерживаемую диагностику команды подготовки в байтах.
	MaxDiagnosticBytes uint64 `yaml:"max_diagnostic_bytes"`
}

// Observation задаёт общие бюджеты невытесняемого реестра оперативных наблюдений.
type Observation struct {
	// MaxSessions ограничивает число узлов наблюдений сессий общей RuntimeAuthority.
	MaxSessions int `yaml:"max_sessions"`
	// MaxGroups ограничивает суммарное число узлов наблюдений групп.
	MaxGroups int `yaml:"max_groups"`
	// MaxOutputs ограничивает суммарное число узлов наблюдений исходящих выходов.
	MaxOutputs int `yaml:"max_outputs"`
	// MaxFFmpegWorkers ограничивает суммарное число наблюдаемых логических работников FFmpeg.
	MaxFFmpegWorkers int `yaml:"max_ffmpeg_workers"`
	// MaxIdentifierBytes ограничивает длину удерживаемого идентификатора наблюдений в байтах.
	MaxIdentifierBytes uint64 `yaml:"max_identifier_bytes"`
	// MaxErrorBytes ограничивает размер текста ошибки оперативного наблюдения в байтах.
	MaxErrorBytes uint64 `yaml:"max_error_bytes"`
	// MaxBytes задаёт общий логический бюджет реестра наблюдений в байтах.
	MaxBytes uint64 `yaml:"max_bytes"`
}

// OutputExecution задаёт общий бюджет резервов физического исполнения исходящих выходов.
type OutputExecution struct {
	// MaxReservations ограничивает число одновременно удерживаемых резервов исполнения выходов.
	MaxReservations int `yaml:"max_reservations"`
	// MaxIdentifierBytes ограничивает байты каждого строкового идентификатора ExecutionFence в
	// резерве.
	MaxIdentifierBytes uint64 `yaml:"max_identifier_bytes"`
	// MaxBytes ограничивает общий логический вес реестра исполнений с учётом строк ExecutionFence.
	MaxBytes uint64 `yaml:"max_bytes"`
}

// Preparation задаёт общие бюджеты очереди подготовки заглушек и требуемых профилей.
type Preparation struct {
	// MaxQueued ограничивает число заданий очереди подготовки.
	MaxQueued int `yaml:"max_queued"`
	// MaxProfiles ограничивает число профилей, требуемых для одной подготовки заглушки.
	MaxProfiles int `yaml:"max_profiles"`
}

// Events задаёт конечные бюджеты потока оперативных событий и чтения его страниц.
type Events struct {
	// MaxEvents ограничивает число сохраняемых событий RAM-потока.
	MaxEvents int `yaml:"max_events"`
	// MaxBytes ограничивает суммарный логический вес сохранённых событий в байтах.
	MaxBytes uint64 `yaml:"max_bytes"`
	// MaxEventBytes ограничивает логический вес одного сохраняемого события в байтах.
	MaxEventBytes uint64 `yaml:"max_event_bytes"`
	// MaxPageSize ограничивает количество оперативных событий, возвращаемых одним чтением.
	MaxPageSize int `yaml:"max_page_size"`
}

// Monitoring задаёт серверные пределы последовательного сборщика медиаметрик.
type Monitoring struct {
	// MaxSources ограничивает число зарегистрированных функций чтения медиаметрик.
	MaxSources int `yaml:"max_sources"`
	// StaleAfter задаёт максимальный возраст исходной медиавыборки для доступного расчёта скоростей.
	StaleAfter time.Duration `yaml:"stale_after"`
	// ShutdownTimeout ограничивает ожидание завершения единственного работника медиасборщика.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// GPU задаёт бюджеты нативного сборщика GPU и сроки свежести и остановки.
type GPU struct {
	// MaxDevices ограничивает число устройств, опрашиваемых и принимаемых сборщиком.
	MaxDevices int `yaml:"max_devices"`
	// MaxUUIDBytes ограничивает длину UUID устройства в байтах.
	MaxUUIDBytes int `yaml:"max_uuid_bytes"`
	// MaxSnapshotBytes ограничивает логический вес снимка устройств и их UUID в байтах.
	MaxSnapshotBytes uint64 `yaml:"max_snapshot_bytes"`
	// StaleAfter задаёт возраст последнего успеха, после которого показатель GPU становится stale.
	StaleAfter time.Duration `yaml:"stale_after"`
	// ShutdownTimeout ограничивает попытку ожидания завершения нативного работника GPU.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// Logging задаёт бюджеты асинхронного журнала, срок его остановки и уровень записей.
type Logging struct {
	// MaxRecords ограничивает число удерживаемых записей журнала.
	MaxRecords int `yaml:"max_records"`
	// MaxBytes задаёт общий бюджет удерживаемых байтов журнала.
	MaxBytes uint64 `yaml:"max_bytes"`
	// MaxRecordBytes ограничивает размер одной записи журнала в байтах.
	MaxRecordBytes uint64 `yaml:"max_record_bytes"`
	// ShutdownTimeout ограничивает ожидание завершения записи журнала.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// Level задаёт минимальный уровень обычных записей slog.
	Level slog.Level `yaml:"level"`
}

// API задаёт размеры запросов, сроки управляющих операций и пределы SSE.
type API struct {
	// JSONBytes ограничивает тело типизированного JSON-запроса в байтах.
	JSONBytes int64 `yaml:"json_bytes"`
	// UploadBytes ограничивает потоковое тело загрузки файла заглушки в байтах.
	UploadBytes int64 `yaml:"upload_bytes"`
	// UploadReadTimeout ограничивает передачу файла заглушки до подтверждения сохранения оригинала.
	UploadReadTimeout time.Duration `yaml:"upload_read_timeout"`
	// OperationTimeout ограничивает отдельную управляющую HTTP-операцию.
	OperationTimeout time.Duration `yaml:"operation_timeout"`
	// ShutdownTimeout ограничивает ожидание завершения принятых API-обработчиков.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// SSEClients ограничивает число одновременно допущенных SSE-подписок.
	SSEClients int `yaml:"sse_clients"`
	// SSEPageSize задаёт число событий, запрашиваемое из RAM-потока за одно чтение подписки.
	SSEPageSize int `yaml:"sse_page_size"`
	// SSEFrameBytes ограничивает размер полного SSE-кадра в байтах.
	SSEFrameBytes int `yaml:"sse_frame_bytes"`
	// SSEWriteTimeout ограничивает запись очередного SSE-кадра в транспорт.
	SSEWriteTimeout time.Duration `yaml:"sse_write_timeout"`
	// SSEHeartbeat задаёт период служебных кадров поддержания SSE-подписки.
	SSEHeartbeat time.Duration `yaml:"sse_heartbeat"`
}

// Config хранит единственный проверенный снимок серверных настроек из defaults и выбранного YAML.
type Config struct {
	// Paths задаёт каталоги постоянных данных и объектов выполнения.
	Paths Paths `yaml:"paths"`
	// Executables задаёт имена или пути инструментов, разрешаемые приложением при запуске.
	Executables Executables `yaml:"executables"`
	// Lifecycle задаёт внешние сроки запуска и остановки общей сборки сервиса.
	Lifecycle Lifecycle `yaml:"lifecycle"`
	// HTTP задаёт адрес управляющего сервера и бюджеты HTTP-транспорта.
	HTTP HTTP `yaml:"http"`
	// IngestAddress задаёт TCP host:port входного Enhanced RTMP-сервера.
	IngestAddress string `yaml:"ingest_address"`
	// Storage задаёт пул SQLite и ограниченную политику ожидания и повторов операций БД.
	Storage Storage `yaml:"storage"`
	// Session задаёт бюджеты и сроки менеджера сессий.
	Session Session `yaml:"session"`
	// Group задаёт контроль группы и временные ограничения переключения источников.
	Group Group `yaml:"group"`
	// FFmpeg задаёт бюджеты и сроки логических работников FFmpeg.
	FFmpeg FFmpeg `yaml:"ffmpeg"`
	// Container задаёт ограничения обмена сжатыми пакетами через fMP4.
	Container Container `yaml:"container"`
	// Router задаёт общие бюджеты маршрутизации медиапакетов.
	Router Router `yaml:"router"`
	// Queue задаёт пределы очередей медиатракта групп.
	Queue Queue `yaml:"queue"`
	// Publisher задаёт очереди, повторы и транспорт независимых исходящих отправителей.
	Publisher Publisher `yaml:"publisher"`
	// Ingress задаёт бюджеты и сроки входного RTMP-транспорта.
	Ingress Ingress `yaml:"ingress"`
	// Fallback задаёт сроки, размеры и внешние инструменты подготовки заглушек.
	Fallback Fallback `yaml:"fallback"`
	// Process задаёт бюджеты учёта физических процессов и срок мягкой остановки.
	Process Process `yaml:"process"`
	// Monitoring задаёт бюджеты и свежесть сбора медиаметрик.
	Monitoring Monitoring `yaml:"monitoring"`
	// Telemetry задаёт общие пределы оперативных событий и снимка GPU.
	Telemetry Telemetry `yaml:"telemetry"`
	// GPU задаёт пределы нативного сбора показателей устройств.
	GPU GPU `yaml:"gpu"`
	// Logging задаёт бюджеты и уровень единственного журнала приложения.
	Logging Logging `yaml:"logging"`
	// API задаёт пределы REST, загрузки файлов и SSE.
	API API `yaml:"api"`
	// Observation задаёт общий бюджет точных узлов оперативных наблюдений.
	Observation Observation `yaml:"observation"`
	// OutputExecution задаёт общий бюджет удержаний физического исполнения выходов.
	OutputExecution OutputExecution `yaml:"output_execution"`
	// Preparation задаёт бюджет очереди подготовки и требуемых профилей.
	Preparation Preparation `yaml:"preparation"`
}

// Paths хранит абсолютные очищенные корни постоянных данных и объектов выполнения сервера.
type Paths struct {
	// Data задаёт корень SQLite, файлов заглушек и журналов владения процессами.
	Data string `yaml:"data"`
	// Runtime задаёт корень объектов выполнения, включая UDS и области физических процессов.
	Runtime string `yaml:"runtime"`
}

// Executables хранит настраиваемые имена или пути медиаинструментов и gost-process-helper без их запуска
// при проверке YAML.
type Executables struct {
	// FFmpeg задаёт исполняемый файл обработки и подготовки медиапотоков.
	FFmpeg string `yaml:"ffmpeg"`
	// FFprobe задаёт исполняемый файл анализа исходных медиа заглушки.
	FFprobe string `yaml:"ffprobe"`
	// Helper задаёт исполняемый gost-process-helper для подтверждённого владения дочерними
	// процессами.
	Helper string `yaml:"helper"`
}

// Lifecycle задаёт независимые общие сроки создания и завершения приложения.
type Lifecycle struct {
	// StartupTimeout ограничивает общую начальную сборку приложения.
	StartupTimeout time.Duration `yaml:"startup_timeout"`
	// ShutdownTimeout ограничивает внешнее ожидание освобождения общего подграфа приложения.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// HTTP задаёт адрес слушателя и ограничения HTTP-соединений сервера управления.
type HTTP struct {
	// Address задаёт TCP host:port слушателя управляющего HTTP API.
	Address string `yaml:"address"`
	// ReadHeaderTimeout ограничивает время чтения заголовков HTTP-запроса.
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	// IdleTimeout ограничивает ожидание следующего запроса keep-alive-соединения.
	IdleTimeout time.Duration `yaml:"idle_timeout"`
	// MaxHeaderBytes ограничивает байты HTTP-заголовков, принимаемых транспортом.
	MaxHeaderBytes int `yaml:"max_header_bytes"`
}

// Storage задаёт явные настройки пула SQLite и ограниченных повторов полностью откатившихся операций.
type Storage struct {
	// MaxOpenConns ограничивает число открытых физических соединений общего sql.DB.
	MaxOpenConns int `yaml:"max_open_conns"`
	// MaxIdleConns ограничивает число соединений, оставляемых пулом без работы; ноль допустим.
	MaxIdleConns int `yaml:"max_idle_conns"`
	// ConnMaxLifetime ограничивает срок использования соединения пула с момента его создания.
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
	// ConnMaxIdleTime ограничивает время бездействия соединения в пуле.
	ConnMaxIdleTime time.Duration `yaml:"conn_max_idle_time"`
	// BusyTimeout задаёт срок ожидания блокировки SQLite на соединении.
	BusyTimeout time.Duration `yaml:"busy_timeout"`
	// RetryAttempts ограничивает количество попыток повторяемой операции БД.
	RetryAttempts int `yaml:"retry_attempts"`
	// RetryTimeout ограничивает общий срок повторов операции БД.
	RetryTimeout time.Duration `yaml:"retry_timeout"`
	// RetryDelay задаёт задержку перед очередной разрешённой попыткой операции БД.
	RetryDelay time.Duration `yaml:"retry_delay"`
}

// Session задаёт пределы обслуживаемых сессий, их дочерних объектов и управляющих сроков.
type Session struct {
	// MaxSessions ограничивает число сессий, удерживаемых менеджером.
	MaxSessions int `yaml:"max_sessions"`
	// MaxOutputs ограничивает число выходов одной сессии.
	MaxOutputs int `yaml:"max_outputs"`
	// MaxCommands ограничивает одновременно учитываемые незавершённые команды менеджера и размер
	// порции повторного завершения ресурсов.
	MaxCommands int `yaml:"max_commands"`
	// PageSize задаёт размер ограниченной страницы сверки менеджера с хранилищем.
	PageSize int `yaml:"page_size"`
	// MaxGroups ограничивает число медиагрупп одной сессии.
	MaxGroups uint32 `yaml:"max_groups"`
	// MaxReferences ограничивает суммарное число удерживаемых ссылок на группы в пределах одной
	// исполняемой сессии.
	MaxReferences uint32 `yaml:"max_references"`
	// ReconcileInterval задаёт период сверки менеджера с долговечным состоянием.
	ReconcileInterval time.Duration `yaml:"reconcile_interval"`
	// StartupTimeout ограничивает начальную готовность запускаемой сессии.
	StartupTimeout time.Duration `yaml:"startup_timeout"`
	// OperationTimeout ограничивает отдельную управляющую операцию менеджера.
	OperationTimeout time.Duration `yaml:"operation_timeout"`
	// ShutdownTimeout ограничивает ожидание остановки принадлежащих менеджеру работников.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// Group задаёт пределы A/V-переключения, повторной подписки и управления работниками группы.
type Group struct {
	// MaxAVSkew ограничивает допустимое рассогласование времени аудио и видео при переключении.
	MaxAVSkew time.Duration `yaml:"max_av_skew"`
	// MaxTransitionGap ограничивает допустимый временной разрыв при переходе источников.
	MaxTransitionGap time.Duration `yaml:"max_transition_gap"`
	// ResubscribeBackoff задаёт паузу перед повторной подпиской группы на источник.
	ResubscribeBackoff time.Duration `yaml:"resubscribe_backoff"`
	// ControlInterval задаёт период управляющего прохода группы.
	ControlInterval time.Duration `yaml:"control_interval"`
	// ShutdownTimeout ограничивает ожидание завершения работников группы.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// FFmpeg задаёт сроки готовности и продвижения дорожек, повторы и бюджеты диагностики одного работника.
type FFmpeg struct {
	// StartupTimeout ограничивает ожидание готовности обеих дорожек новой попытки FFmpeg.
	StartupTimeout time.Duration `yaml:"startup_timeout"`
	// VideoTimeout задаёт допустимое отсутствие продвижения валидных видеопакетов.
	VideoTimeout time.Duration `yaml:"video_timeout"`
	// AudioTimeout задаёт допустимое отсутствие продвижения валидных аудиопакетов.
	AudioTimeout time.Duration `yaml:"audio_timeout"`
	// WatchdogInterval задаёт период проверки продвижения дорожек FFmpeg.
	WatchdogInterval time.Duration `yaml:"watchdog_interval"`
	// MaxRestarts ограничивает число повторных запусков логического работника; ноль запрещает
	// перезапуски.
	MaxRestarts int `yaml:"max_restarts"`
	// RestartBackoff задаёт паузу перед новой попыткой работника FFmpeg.
	RestartBackoff time.Duration `yaml:"restart_backoff"`
	// GracePeriod задаёт срок мягкой остановки процесса FFmpeg до принудительного завершения.
	GracePeriod time.Duration `yaml:"grace_period"`
	// KillTimeout ограничивает ожидание принудительного завершения попытки FFmpeg.
	KillTimeout time.Duration `yaml:"kill_timeout"`
	// ShutdownTimeout ограничивает ожидание остановки логического работника FFmpeg.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// DiagnosticEvents ограничивает число удерживаемых диагностических событий FFmpeg.
	DiagnosticEvents int `yaml:"diagnostic_events"`
	// DiagnosticBytes задаёт общий логический бюджет удерживаемой диагностики FFmpeg в байтах.
	DiagnosticBytes uint64 `yaml:"diagnostic_bytes"`
	// DiagnosticChunkBytes ограничивает байты одного фрагмента диагностического вывода.
	DiagnosticChunkBytes int `yaml:"diagnostic_chunk_bytes"`
	// MaxCommandBytes ограничивает суммарный размер имени программы и аргументов команды FFmpeg в
	// байтах.
	MaxCommandBytes uint64 `yaml:"max_command_bytes"`
	// MaxCommandArgs ограничивает число аргументов команды FFmpeg.
	MaxCommandArgs int `yaml:"max_command_args"`
	// Progress задаёт пределы разбора отчётов progress отдельно от обычной диагностики.
	Progress Progress `yaml:"progress"`
}

// Publisher задаёт очередь медиапакетов, интервалы повторов и исходящий RTMP-транспорт одного
// отправителя.
type Publisher struct {
	// Queue задаёт конечные бюджеты отдельной очереди исходящего отправителя.
	Queue Queue `yaml:"queue"`
	// RetryInitial задаёт начальную задержку повторного подключения назначения.
	RetryInitial time.Duration `yaml:"retry_initial"`
	// RetryMax ограничивает рост задержки повторного подключения назначения.
	RetryMax time.Duration `yaml:"retry_max"`
	// ResubscribeDelay задаёт паузу перед повторным получением подписки на выход группы.
	ResubscribeDelay time.Duration `yaml:"resubscribe_delay"`
	// Transport задаёт конечные бюджеты и сроки исходящего RTMP/RTMPS-соединения.
	Transport Outbound `yaml:"transport"`
}

// Fallback задаёт общие пределы загрузки, анализа, подготовки и сверки файлов заглушек.
type Fallback struct {
	// MaxUploadBytes ограничивает размер принимаемого оригинала заглушки в байтах.
	MaxUploadBytes uint64 `yaml:"max_upload_bytes"`
	// MaxOutputBytes ограничивает размер подготовленного выходного файла заглушки в байтах.
	MaxOutputBytes uint64 `yaml:"max_output_bytes"`
	// PrepareTimeout ограничивает выполнение задания подготовки заглушки.
	PrepareTimeout time.Duration `yaml:"prepare_timeout"`
	// ShutdownTimeout ограничивает ожидание завершения владельца подготовки.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// QueueInterval задаёт интервал обработки очереди заданий подготовки.
	QueueInterval time.Duration `yaml:"queue_interval"`
	// Runner задаёт бюджеты запуска внешних инструментов подготовки.
	Runner Runner `yaml:"runner"`
	// Probe задаёт пределы разбора и допустимого исходного медиа заглушки.
	Probe Probe `yaml:"probe"`
	// ReconcileLimit задаёт предел записей при сверке подготовки и принадлежащих ей файлов.
	ReconcileLimit int `yaml:"reconcile_limit"`
}

// Probe задаёт допустимые размеры и длительности исходника и бюджеты его анализа.
type Probe struct {
	// MaxJSONBytes ограничивает размер JSON-результата анализа ffprobe в байтах.
	MaxJSONBytes uint64 `yaml:"max_json_bytes"`
	// MaxWidth ограничивает ширину исходного видео или изображения в пикселях.
	MaxWidth uint32 `yaml:"max_width"`
	// MaxHeight ограничивает высоту исходного видео или изображения в пикселях.
	MaxHeight uint32 `yaml:"max_height"`
	// MaxPixels ограничивает произведение ширины и высоты исходного изображения или видео.
	MaxPixels uint64 `yaml:"max_pixels"`
	// MaxDuration ограничивает длительность принимаемого исходного медиа.
	MaxDuration time.Duration `yaml:"max_duration"`
	// ImageDuration задаёт длительность подготовленной петли для неподвижного изображения.
	ImageDuration time.Duration `yaml:"image_duration"`
	// MaxPackets ограничивает количество пакетов, учитываемых при анализе медиа.
	MaxPackets uint64 `yaml:"max_packets"`
	// Container задаёт пределы fMP4 при проверке подготовленного варианта заглушки.
	Container Container `yaml:"container"`
}

// Process задаёт журнал физического владения процессами и срок мягкой остановки.
type Process struct {
	// Limits задаёт бюджеты записей, команд и страниц журнала владельца процессов.
	Limits ProcessLimits `yaml:"limits"`
	// GracePeriod задаёт срок мягкой остановки, передаваемый gost-process-helper.
	GracePeriod time.Duration `yaml:"grace_period"`
}

// Telemetry задаёт общие пределы оперативного потока событий и представления GPU в RuntimeAuthority.
type Telemetry struct {
	// MediaStaleAfter задаёт срок устаревания сохранённого оперативного измерения медиа.
	MediaStaleAfter time.Duration `yaml:"media_stale_after"`
	// Events задаёт конечные бюджеты общей очереди оперативных событий.
	Events Events `yaml:"events"`
	// MaxGPUDevices ограничивает число устройств в общем снимке GPU RuntimeAuthority.
	MaxGPUDevices int `yaml:"max_gpu_devices"`
	// MaxGPUUUIDBytes ограничивает длину UUID устройства в общем снимке GPU в байтах.
	MaxGPUUUIDBytes uint64 `yaml:"max_gpu_uuid_bytes"`
	// MaxGPUBytes ограничивает логический вес общего снимка GPU с UUID и измерениями.
	MaxGPUBytes uint64 `yaml:"max_gpu_bytes"`
}
