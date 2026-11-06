// Package config загружает и проверяет неизменяемые настройки сервера.
package config

import (
	"log/slog"
	"time"
)

type Router struct {
	MaxConsumers     uint32 `yaml:"max_consumers"`
	MaxRetainedBytes uint64 `yaml:"max_retained_bytes"`
	MaxConfigBytes   uint64 `yaml:"max_config_bytes"`
}

type Queue struct {
	MaxPackets     uint32        `yaml:"max_packets"`
	MaxBytes       uint64        `yaml:"max_bytes"`
	MaxPacketBytes uint64        `yaml:"max_packet_bytes"`
	MaxConfigBytes uint64        `yaml:"max_config_bytes"`
	ResyncTimeout  time.Duration `yaml:"resync_timeout"`
}

type Ingress struct {
	MaxConnections    uint32        `yaml:"max_connections"`
	MaxChunkStreams   uint32        `yaml:"max_chunk_streams"`
	MaxMessageBytes   uint32        `yaml:"max_message_bytes"`
	MaxChunkBytes     uint32        `yaml:"max_chunk_bytes"`
	MaxFragments      uint32        `yaml:"max_fragments"`
	MaxRetainedBytes  uint64        `yaml:"max_retained_bytes"`
	MaxControlBytes   uint32        `yaml:"max_control_bytes"`
	MaxConfigBytes    uint32        `yaml:"max_config_bytes"`
	MaxAMFBytes       uint32        `yaml:"max_amf_bytes"`
	MaxAMFDepth       uint32        `yaml:"max_amf_depth"`
	MaxAMFElements    uint32        `yaml:"max_amf_elements"`
	MaxReadAheadBytes uint32        `yaml:"max_read_ahead_bytes"`
	HandshakeTimeout  time.Duration `yaml:"handshake_timeout"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}

type Outbound struct {
	MaxChunkStreams   uint32        `yaml:"max_chunk_streams"`
	MaxMessageBytes   uint32        `yaml:"max_message_bytes"`
	MaxChunkBytes     uint32        `yaml:"max_chunk_bytes"`
	MaxFragments      uint32        `yaml:"max_fragments"`
	MaxControlBytes   uint32        `yaml:"max_control_bytes"`
	MaxConfigBytes    uint32        `yaml:"max_config_bytes"`
	MaxAMFBytes       uint32        `yaml:"max_amf_bytes"`
	MaxAMFDepth       uint32        `yaml:"max_amf_depth"`
	MaxAMFElements    uint32        `yaml:"max_amf_elements"`
	ChunkSize         uint32        `yaml:"chunk_size"`
	MaxControlPending uint32        `yaml:"max_control_pending"`
	MaxRetainedBytes  uint64        `yaml:"max_retained_bytes"`
	DialTimeout       time.Duration `yaml:"dial_timeout"`
	TLSTimeout        time.Duration `yaml:"tls_timeout"`
	StartupTimeout    time.Duration `yaml:"startup_timeout"`
	MessageTimeout    time.Duration `yaml:"message_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
}

type Container struct {
	MaxBoxBytes       uint64 `yaml:"max_box_bytes"`
	MaxInitBytes      uint64 `yaml:"max_init_bytes"`
	MaxMoofBytes      uint64 `yaml:"max_moof_bytes"`
	MaxMdatBytes      uint64 `yaml:"max_mdat_bytes"`
	MaxSampleBytes    uint64 `yaml:"max_sample_bytes"`
	MaxExtradataBytes uint64 `yaml:"max_extradata_bytes"`
	MaxSamples        uint32 `yaml:"max_samples"`
}

type Progress struct {
	MaxLineBytes   int `yaml:"max_line_bytes"`
	MaxReportBytes int `yaml:"max_report_bytes"`
	MaxFields      int `yaml:"max_fields"`
}

type ProcessLimits struct {
	MaxOperations   int   `yaml:"max_operations"`
	MaxRecordBytes  int64 `yaml:"max_record_bytes"`
	MaxCommandArgs  int   `yaml:"max_command_args"`
	MaxCommandBytes int   `yaml:"max_command_bytes"`
	MaxExtraFiles   int   `yaml:"max_extra_files"`
	PageSize        int   `yaml:"page_size"`
}

type Runner struct {
	Timeout            time.Duration `yaml:"timeout"`
	MaxArgs            int           `yaml:"max_args"`
	MaxCommandBytes    uint64        `yaml:"max_command_bytes"`
	MaxDiagnosticBytes uint64        `yaml:"max_diagnostic_bytes"`
}

type Observation struct {
	MaxSessions        int    `yaml:"max_sessions"`
	MaxGroups          int    `yaml:"max_groups"`
	MaxOutputs         int    `yaml:"max_outputs"`
	MaxFFmpegWorkers   int    `yaml:"max_ffmpeg_workers"`
	MaxIdentifierBytes uint64 `yaml:"max_identifier_bytes"`
	MaxErrorBytes      uint64 `yaml:"max_error_bytes"`
	MaxBytes           uint64 `yaml:"max_bytes"`
}

type OutputExecution struct {
	MaxReservations    int    `yaml:"max_reservations"`
	MaxIdentifierBytes uint64 `yaml:"max_identifier_bytes"`
	MaxBytes           uint64 `yaml:"max_bytes"`
}

type Preparation struct {
	MaxQueued   int `yaml:"max_queued"`
	MaxProfiles int `yaml:"max_profiles"`
}

type Events struct {
	MaxEvents     int    `yaml:"max_events"`
	MaxBytes      uint64 `yaml:"max_bytes"`
	MaxEventBytes uint64 `yaml:"max_event_bytes"`
	MaxPageSize   int    `yaml:"max_page_size"`
}

type Monitoring struct {
	MaxSources      int           `yaml:"max_sources"`
	StaleAfter      time.Duration `yaml:"stale_after"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

type GPU struct {
	MaxDevices       int           `yaml:"max_devices"`
	MaxUUIDBytes     int           `yaml:"max_uuid_bytes"`
	MaxSnapshotBytes uint64        `yaml:"max_snapshot_bytes"`
	StaleAfter       time.Duration `yaml:"stale_after"`
	ShutdownTimeout  time.Duration `yaml:"shutdown_timeout"`
}

type Logging struct {
	MaxRecords      int           `yaml:"max_records"`
	MaxBytes        uint64        `yaml:"max_bytes"`
	MaxRecordBytes  uint64        `yaml:"max_record_bytes"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	Level           slog.Level    `yaml:"level"`
}

type API struct {
	JSONBytes         int64         `yaml:"json_bytes"`
	UploadBytes       int64         `yaml:"upload_bytes"`
	UploadReadTimeout time.Duration `yaml:"upload_read_timeout"`
	OperationTimeout  time.Duration `yaml:"operation_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
	SSEClients        int           `yaml:"sse_clients"`
	SSEPageSize       int           `yaml:"sse_page_size"`
	SSEFrameBytes     int           `yaml:"sse_frame_bytes"`
	SSEWriteTimeout   time.Duration `yaml:"sse_write_timeout"`
	SSEHeartbeat      time.Duration `yaml:"sse_heartbeat"`
}

type Config struct {
	Paths           Paths           `yaml:"paths"`
	Executables     Executables     `yaml:"executables"`
	Lifecycle       Lifecycle       `yaml:"lifecycle"`
	HTTP            HTTP            `yaml:"http"`
	IngestAddress   string          `yaml:"ingest_address"`
	Storage         Storage         `yaml:"storage"`
	Session         Session         `yaml:"session"`
	Group           Group           `yaml:"group"`
	FFmpeg          FFmpeg          `yaml:"ffmpeg"`
	Container       Container       `yaml:"container"`
	Router          Router          `yaml:"router"`
	Queue           Queue           `yaml:"queue"`
	Publisher       Publisher       `yaml:"publisher"`
	Ingress         Ingress         `yaml:"ingress"`
	Fallback        Fallback        `yaml:"fallback"`
	Process         Process         `yaml:"process"`
	Monitoring      Monitoring      `yaml:"monitoring"`
	Telemetry       Telemetry       `yaml:"telemetry"`
	GPU             GPU             `yaml:"gpu"`
	Logging         Logging         `yaml:"logging"`
	API             API             `yaml:"api"`
	Observation     Observation     `yaml:"observation"`
	OutputExecution OutputExecution `yaml:"output_execution"`
	Preparation     Preparation     `yaml:"preparation"`
}
type Paths struct {
	Data    string `yaml:"data"`
	Runtime string `yaml:"runtime"`
}
type Executables struct {
	FFmpeg  string `yaml:"ffmpeg"`
	FFprobe string `yaml:"ffprobe"`
	Helper  string `yaml:"helper"`
}
type Lifecycle struct {
	StartupTimeout  time.Duration `yaml:"startup_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}
type HTTP struct {
	Address           string        `yaml:"address"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	MaxHeaderBytes    int           `yaml:"max_header_bytes"`
}
type Storage struct {
	MaxOpenConns    int           `yaml:"max_open_conns"`
	MaxIdleConns    int           `yaml:"max_idle_conns"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
	ConnMaxIdleTime time.Duration `yaml:"conn_max_idle_time"`
	BusyTimeout     time.Duration `yaml:"busy_timeout"`
	RetryAttempts   int           `yaml:"retry_attempts"`
	RetryTimeout    time.Duration `yaml:"retry_timeout"`
	RetryDelay      time.Duration `yaml:"retry_delay"`
}
type Session struct {
	MaxSessions       int           `yaml:"max_sessions"`
	MaxOutputs        int           `yaml:"max_outputs"`
	MaxCommands       int           `yaml:"max_commands"`
	PageSize          int           `yaml:"page_size"`
	MaxGroups         uint32        `yaml:"max_groups"`
	MaxReferences     uint32        `yaml:"max_references"`
	ReconcileInterval time.Duration `yaml:"reconcile_interval"`
	StartupTimeout    time.Duration `yaml:"startup_timeout"`
	OperationTimeout  time.Duration `yaml:"operation_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}
type Group struct {
	MaxAVSkew          time.Duration `yaml:"max_av_skew"`
	MaxTransitionGap   time.Duration `yaml:"max_transition_gap"`
	ResubscribeBackoff time.Duration `yaml:"resubscribe_backoff"`
	ControlInterval    time.Duration `yaml:"control_interval"`
	ShutdownTimeout    time.Duration `yaml:"shutdown_timeout"`
}
type FFmpeg struct {
	StartupTimeout       time.Duration `yaml:"startup_timeout"`
	VideoTimeout         time.Duration `yaml:"video_timeout"`
	AudioTimeout         time.Duration `yaml:"audio_timeout"`
	WatchdogInterval     time.Duration `yaml:"watchdog_interval"`
	MaxRestarts          int           `yaml:"max_restarts"`
	RestartBackoff       time.Duration `yaml:"restart_backoff"`
	GracePeriod          time.Duration `yaml:"grace_period"`
	KillTimeout          time.Duration `yaml:"kill_timeout"`
	ShutdownTimeout      time.Duration `yaml:"shutdown_timeout"`
	DiagnosticEvents     int           `yaml:"diagnostic_events"`
	DiagnosticBytes      uint64        `yaml:"diagnostic_bytes"`
	DiagnosticChunkBytes int           `yaml:"diagnostic_chunk_bytes"`
	MaxCommandBytes      uint64        `yaml:"max_command_bytes"`
	MaxCommandArgs       int           `yaml:"max_command_args"`
	Progress             Progress      `yaml:"progress"`
}
type Publisher struct {
	Queue            Queue         `yaml:"queue"`
	RetryInitial     time.Duration `yaml:"retry_initial"`
	RetryMax         time.Duration `yaml:"retry_max"`
	ResubscribeDelay time.Duration `yaml:"resubscribe_delay"`
	Transport        Outbound      `yaml:"transport"`
}
type Fallback struct {
	MaxUploadBytes  uint64        `yaml:"max_upload_bytes"`
	MaxOutputBytes  uint64        `yaml:"max_output_bytes"`
	PrepareTimeout  time.Duration `yaml:"prepare_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	QueueInterval   time.Duration `yaml:"queue_interval"`
	Runner          Runner        `yaml:"runner"`
	Probe           Probe         `yaml:"probe"`
	ReconcileLimit  int           `yaml:"reconcile_limit"`
}
type Probe struct {
	MaxJSONBytes  uint64        `yaml:"max_json_bytes"`
	MaxWidth      uint32        `yaml:"max_width"`
	MaxHeight     uint32        `yaml:"max_height"`
	MaxPixels     uint64        `yaml:"max_pixels"`
	MaxDuration   time.Duration `yaml:"max_duration"`
	ImageDuration time.Duration `yaml:"image_duration"`
	MaxPackets    uint64        `yaml:"max_packets"`
	Container     Container     `yaml:"container"`
}
type Process struct {
	Limits      ProcessLimits `yaml:"limits"`
	GracePeriod time.Duration `yaml:"grace_period"`
}
type Telemetry struct {
	MediaStaleAfter time.Duration `yaml:"media_stale_after"`
	Events          Events        `yaml:"events"`
	MaxGPUDevices   int           `yaml:"max_gpu_devices"`
	MaxGPUUUIDBytes uint64        `yaml:"max_gpu_uuid_bytes"`
	MaxGPUBytes     uint64        `yaml:"max_gpu_bytes"`
}
