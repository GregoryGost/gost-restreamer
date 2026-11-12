package config

import "time"

// Default возвращает начальные конечные бюджеты; это не обещание производительности.
func Default() Config {
	// second задаёт общую единицу времени для начальных сроков и интервалов конфигурации.
	const second = time.Second
	// container содержит начальные пределы разбора fMP4, общие для рабочего медиатракта и проверки
	// заглушек.
	container := Container{MaxBoxBytes: 16 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 1 << 20, MaxMdatBytes: 16 << 20, MaxSampleBytes: 8 << 20, MaxExtradataBytes: 64 << 10, MaxSamples: 4096}
	// queue содержит начальные пределы пакетов и памяти очереди, используемые группами и
	// отправителями.
	queue := Queue{MaxPackets: 512, MaxBytes: 32 << 20, MaxPacketBytes: 10 << 20, MaxConfigBytes: 64 << 10, ResyncTimeout: 5 * second}
	return Config{
		Paths:       Paths{Data: "/var/lib/gost-restreamer", Runtime: "/run/gost-restreamer"},
		Executables: Executables{FFmpeg: "ffmpeg", FFprobe: "ffprobe", Helper: "gost-process-helper"},
		Lifecycle:   Lifecycle{StartupTimeout: 30 * second, ShutdownTimeout: 30 * second},
		HTTP:        HTTP{Address: ":8080", ReadHeaderTimeout: 5 * second, IdleTimeout: 60 * second, MaxHeaderBytes: 1 << 20}, IngestAddress: ":1935",
		Storage:   Storage{MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute, BusyTimeout: second, RetryAttempts: 3, RetryTimeout: 5 * second, RetryDelay: 50 * time.Millisecond},
		Session:   Session{MaxSessions: 2, MaxOutputs: 16, MaxCommands: 32, PageSize: 100, MaxGroups: 8, MaxReferences: 16, ReconcileInterval: 250 * time.Millisecond, StartupTimeout: 30 * second, OperationTimeout: 5 * second, ShutdownTimeout: 30 * second},
		Group:     Group{MaxAVSkew: 250 * time.Millisecond, MaxTransitionGap: second, ResubscribeBackoff: second, ControlInterval: 250 * time.Millisecond, ShutdownTimeout: 30 * second},
		FFmpeg:    FFmpeg{StartupTimeout: 15 * second, VideoTimeout: 3 * second, AudioTimeout: 3 * second, WatchdogInterval: 250 * time.Millisecond, MaxRestarts: 3, RestartBackoff: second, GracePeriod: 2 * second, KillTimeout: 3 * second, ShutdownTimeout: 30 * second, DiagnosticEvents: 128, DiagnosticBytes: 1 << 20, DiagnosticChunkBytes: 16 << 10, MaxCommandBytes: 16 << 10, MaxCommandArgs: 256, Progress: Progress{MaxLineBytes: 4096, MaxReportBytes: 64 << 10, MaxFields: 64}},
		Container: container, Queue: queue, Router: Router{MaxConsumers: 16, MaxRetainedBytes: (512 << 20) + (128 << 10), MaxConfigBytes: 64 << 10},
		Publisher:       Publisher{Queue: queue, RetryInitial: second, RetryMax: 30 * second, ResubscribeDelay: second, Transport: Outbound{MaxChunkStreams: 32, MaxMessageBytes: 10 << 20, MaxChunkBytes: 64 << 10, MaxFragments: 131072, MaxControlBytes: 64 << 10, MaxConfigBytes: 64 << 10, MaxAMFBytes: 64 << 10, MaxAMFDepth: 16, MaxAMFElements: 1024, ChunkSize: 4096, MaxControlPending: 32, MaxRetainedBytes: 128 << 20, DialTimeout: 5 * second, TLSTimeout: 5 * second, StartupTimeout: 10 * second, MessageTimeout: 30 * second, WriteTimeout: 5 * second}},
		Ingress:         Ingress{MaxConnections: 4, MaxChunkStreams: 32, MaxMessageBytes: 10 << 20, MaxChunkBytes: 64 << 10, MaxFragments: 131072, MaxRetainedBytes: 128 << 20, MaxControlBytes: 64 << 10, MaxConfigBytes: 64 << 10, MaxAMFBytes: 64 << 10, MaxAMFDepth: 16, MaxAMFElements: 1024, MaxReadAheadBytes: 64 << 10, HandshakeTimeout: 10 * second, ReadTimeout: 10 * second, WriteTimeout: 5 * second, ShutdownTimeout: 30 * second},
		Fallback:        Fallback{MaxUploadBytes: 1 << 30, MaxOutputBytes: 2 << 30, PrepareTimeout: 10 * time.Minute, ShutdownTimeout: 30 * second, QueueInterval: 250 * time.Millisecond, Runner: Runner{Timeout: 5 * time.Minute, MaxArgs: 256, MaxCommandBytes: 16 << 10, MaxDiagnosticBytes: 1 << 20}, Probe: Probe{MaxJSONBytes: 4 << 20, MaxWidth: 8192, MaxHeight: 8192, MaxPixels: 33554432, MaxDuration: 5 * time.Minute, ImageDuration: 10 * second, MaxPackets: 1000000, Container: container}, ReconcileLimit: 100},
		Process:         Process{Limits: ProcessLimits{MaxOperations: 10000, MaxRecordBytes: 128 << 10, MaxCommandArgs: 256, MaxCommandBytes: 16 << 10, MaxExtraFiles: 4, PageSize: 100}, GracePeriod: 2 * second},
		Monitoring:      Monitoring{MaxSources: 64, StaleAfter: 6 * second, ShutdownTimeout: 30 * second},
		Telemetry:       Telemetry{MediaStaleAfter: 6 * second, Events: Events{MaxEvents: 2048, MaxBytes: 16 << 20, MaxEventBytes: 256 << 10, MaxPageSize: 100}, MaxGPUDevices: 8, MaxGPUUUIDBytes: 128, MaxGPUBytes: 64 << 10},
		GPU:             GPU{MaxDevices: 8, MaxUUIDBytes: 128, MaxSnapshotBytes: 16 << 10, StaleAfter: 6 * second, ShutdownTimeout: 30 * second},
		Logging:         Logging{MaxRecords: 1024, MaxBytes: 8 << 20, MaxRecordBytes: 1 << 20, ShutdownTimeout: 30 * second, Level: 0},
		API:             API{JSONBytes: 1 << 20, UploadBytes: 1 << 30, UploadReadTimeout: 5 * time.Minute, OperationTimeout: 10 * second, ShutdownTimeout: 30 * second, SSEClients: 16, SSEPageSize: 100, SSEFrameBytes: 1 << 20, SSEWriteTimeout: 5 * second, SSEHeartbeat: 15 * second},
		Observation:     Observation{MaxSessions: 2, MaxGroups: 16, MaxOutputs: 32, MaxFFmpegWorkers: 32, MaxIdentifierBytes: 256, MaxErrorBytes: 64 << 10, MaxBytes: 16 << 20},
		OutputExecution: OutputExecution{MaxReservations: 32, MaxIdentifierBytes: 256, MaxBytes: 1 << 20}, Preparation: Preparation{MaxQueued: 8, MaxProfiles: 16},
	}
}
