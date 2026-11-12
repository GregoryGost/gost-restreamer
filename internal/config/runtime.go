package config

import (
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring/gpu"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring/logging"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
	"github.com/GregoryGost/gost-restreamer/internal/session"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// GeneratorVersion обозначает формат существующего генератора файлов заглушки.
const GeneratorVersion = "fallback-fmp4-silent-v2"

// StorageOptions передаёт настройки SQLite и общих владельцев хранилищу без назначения новых значений
// по умолчанию.
func (c Config) StorageOptions() storage.Options {
	// s содержит настройки соединений и повторов SQLite для переноса в storage.Options.
	s := c.Storage
	return storage.Options{OutputExecutionLimits: core.OutputExecutionLimits(c.OutputExecution), PreparationLimits: core.PreparationLimits(c.Preparation), ObservationLimits: core.ObservationLimits(c.Observation), MaxOpenConns: s.MaxOpenConns, MaxIdleConns: s.MaxIdleConns, ConnMaxLifetime: s.ConnMaxLifetime, ConnMaxIdleTime: s.ConnMaxIdleTime, BusyTimeout: s.BusyTimeout, RetryAttempts: s.RetryAttempts, RetryTimeout: s.RetryTimeout, RetryDelay: s.RetryDelay}
}

// TelemetryLimits формирует явные пределы оперативных событий, свежести и GPU для общей
// RuntimeAuthority.
func (c Config) TelemetryLimits() core.TelemetryLimits {
	// t содержит пределы оперативной телеметрии для передачи общей RuntimeAuthority.
	t := c.Telemetry
	return core.TelemetryLimits{MediaStaleAfter: t.MediaStaleAfter, Events: core.EventLimits(t.Events), MaxGPUDevices: t.MaxGPUDevices, MaxGPUUUIDBytes: t.MaxGPUUUIDBytes, MaxGPUBytes: t.MaxGPUBytes}
}

// FFmpegConfig связывает бюджеты FFmpeg с каталогом объектов выполнения сервера и пределами разбора
// progress.
func (c Config) FFmpegConfig() ffmpeg.Config {
	// f содержит сроки, ограничения команд и диагностики FFmpeg для сборки его рабочей конфигурации.
	f := c.FFmpeg
	return ffmpeg.Config{BaseDir: c.Paths.Runtime, StartupTimeout: f.StartupTimeout, VideoTimeout: f.VideoTimeout, AudioTimeout: f.AudioTimeout, WatchdogInterval: f.WatchdogInterval, MaxRestarts: f.MaxRestarts, RestartBackoff: f.RestartBackoff, GracePeriod: f.GracePeriod, KillTimeout: f.KillTimeout, ShutdownTimeout: f.ShutdownTimeout, DiagnosticEvents: f.DiagnosticEvents, DiagnosticBytes: f.DiagnosticBytes, DiagnosticChunkBytes: f.DiagnosticChunkBytes, MaxCommandBytes: f.MaxCommandBytes, MaxCommandArgs: f.MaxCommandArgs, Progress: ffmpeg.ProgressConfig(f.Progress)}
}

// GroupConfig собирает настройки группы, переключателя, FFmpeg и контейнера; свежесть метрик берётся
// из Monitoring.
func (c Config) GroupConfig() group.RuntimeConfig {
	// g содержит сроки управления и ограничения переключения источников группы.
	g := c.Group
	return group.RuntimeConfig{Switcher: group.SwitcherConfig{Queue: media.QueueLimits(c.Queue), Router: media.RouterLimits(c.Router), MaxAVSkew: g.MaxAVSkew, MaxTransitionGap: g.MaxTransitionGap}, FFmpeg: c.FFmpegConfig(), Container: fmp4.Limits(c.Container), Binary: c.Executables.FFmpeg, MetricStaleAfter: c.Monitoring.StaleAfter, ResubscribeBackoff: g.ResubscribeBackoff, ControlInterval: g.ControlInterval, ShutdownTimeout: g.ShutdownTimeout}
}

// PublisherOptions передаёт очереди, интервалы повторов и пределы исходящего RTMP отправителю.
func (c Config) PublisherOptions() publisher.Options {
	// p содержит настройки очереди, повторов и RTMP-транспорта исходящего отправителя.
	p := c.Publisher
	return publisher.Options{Queue: media.QueueLimits(p.Queue), RetryInitial: p.RetryInitial, RetryMax: p.RetryMax, ResubscribeDelay: p.ResubscribeDelay, Transport: rtmp.OutboundOptions{Limits: rtmp.OutboundLimits(p.Transport)}}
}

// SessionConfig собирает настройки менеджера сессий без рабочих зависимостей, которые позднее
// подставляет приложение.
func (c Config) SessionConfig() session.Config {
	// s содержит бюджеты и сроки менеджера сессий, дополняемые настройками его компонентов.
	s := c.Session
	return session.Config{GeneratorVersion: GeneratorVersion, Group: c.GroupConfig(), Router: media.RouterLimits(c.Router), Publisher: c.PublisherOptions(), Monitoring: monitoring.SamplerConfig(c.Monitoring), Telemetry: c.TelemetryLimits(), MaxSessions: s.MaxSessions, MaxOutputs: s.MaxOutputs, MaxCommands: s.MaxCommands, PageSize: s.PageSize, MaxGroups: s.MaxGroups, MaxReferences: s.MaxReferences, ReconcileInterval: s.ReconcileInterval, StartupTimeout: s.StartupTimeout, OperationTimeout: s.OperationTimeout, ShutdownTimeout: s.ShutdownTimeout}
}

// FallbackConfig связывает бюджеты подготовки и проверки файлов с настроенными FFmpeg, ffprobe и
// версией генератора.
func (c Config) FallbackConfig() fallback.Config {
	// f содержит общие настройки подготовки заглушек; p содержит вложенные пределы проверки
	// исходного файла.
	f, p := c.Fallback, c.Fallback.Probe
	return fallback.Config{GeneratorVersion: GeneratorVersion, FFmpeg: c.Executables.FFmpeg, FFprobe: c.Executables.FFprobe, MaxUploadBytes: f.MaxUploadBytes, MaxOutputBytes: f.MaxOutputBytes, PrepareTimeout: f.PrepareTimeout, ShutdownTimeout: f.ShutdownTimeout, QueueInterval: f.QueueInterval, ReconcileLimit: f.ReconcileLimit, Runner: fallback.RunnerLimits(f.Runner), Probe: fallback.ProbeLimits{MaxJSONBytes: p.MaxJSONBytes, MaxWidth: p.MaxWidth, MaxHeight: p.MaxHeight, MaxPixels: p.MaxPixels, MaxDuration: p.MaxDuration, ImageDuration: p.ImageDuration, MaxPackets: p.MaxPackets, Container: fmp4.Limits(p.Container)}}
}

// ProcessConfig задаёт каталоги журнала процессов и объектов выполнения вместе с уже разрешённым путём
// gost-process-helper.
func (c Config) ProcessConfig(helper string) processowner.Config {
	return processowner.Config{Root: filepath.Join(c.Paths.Data, "processes"), RuntimeRoot: c.Paths.Runtime, HelperPath: helper, Limits: processowner.Limits(c.Process.Limits), GracePeriod: c.Process.GracePeriod}
}

// DatabasePath возвращает путь файла SQLite внутри настроенного каталога постоянных данных.
func (c Config) DatabasePath() string { return filepath.Join(c.Paths.Data, "gost-restreamer.db") }

// Validate проверяет синтаксис адресов, абсолютные пути данных и выполнения, имена исполняемых файлов,
// допустимость сроков и бюджетов компонентов. Дополнительно сверяет вместимость общих реестров и
// очередей с настроенными пределами сессий, пакетов, GPU и команд. Ошибка указывает на поле или
// компонент конфигурации. Проверка не открывает файлы, сокеты и БД и не ищет программы в PATH.
func (c Config) Validate() error {
	// name указывает проверяемое поле адреса; address содержит его значение TCP host:port.
	for name, address := range map[string]string{"http.address": c.HTTP.Address, "ingest_address": c.IngestAddress} {
		// host содержит часть адреса до порта; port содержит строку порта; err содержит отказ
		// разделения TCP-адреса.
		host, port, err := net.SplitHostPort(address)
		if err != nil || strings.ContainsAny(host, "\x00/\\ \t\r\n") {
			return fmt.Errorf("%s: требуется TCP host:port", name)
		}
		// n содержит численное значение порта для проверки диапазона 1..65535.
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return fmt.Errorf("%s: порт должен быть числом 1..65535", name)
		}
		if !validHost(host) {
			return fmt.Errorf("%s: неверный TCP host", name)
		}
	}
	// name указывает проверяемое поле рабочего каталога; path содержит путь, который должен быть
	// абсолютным и очищенным.
	for name, path := range map[string]string{"paths.data": c.Paths.Data, "paths.runtime": c.Paths.Runtime} {
		if !filepath.IsAbs(path) || path != filepath.Clean(path) || strings.ContainsRune(path, 0) {
			return fmt.Errorf("%s: требуется абсолютный чистый путь", name)
		}
	}
	// name указывает настройку исполняемого файла; value содержит имя или путь, проверяемый без
	// поиска в PATH.
	for name, value := range map[string]string{"executables.ffmpeg": c.Executables.FFmpeg, "executables.ffprobe": c.Executables.FFprobe, "executables.helper": c.Executables.Helper} {
		if strings.TrimSpace(value) == "" || strings.ContainsRune(value, 0) {
			return fmt.Errorf("%s: требуется имя исполняемого файла", name)
		}
	}
	if c.Lifecycle.StartupTimeout <= 0 || c.Lifecycle.ShutdownTimeout <= 0 {
		return fmt.Errorf("lifecycle: сроки должны быть положительными")
	}
	if c.HTTP.ReadHeaderTimeout <= 0 || c.HTTP.IdleTimeout <= 0 || c.HTTP.MaxHeaderBytes <= 0 {
		return fmt.Errorf("http: некорректные бюджеты")
	}
	// checks хранит результаты проверок настроек компонентов вместе с именами разделов для
	// диагностики.
	checks := []struct {
		// name указывает раздел конфигурации, проверенный компонентом.
		name string
		// err содержит результат проверки настроек указанного раздела.
		err error
	}{
		{"storage", c.StorageOptions().Validate()}, {"session", c.SessionConfig().ValidateSettings()},
		{"fallback", c.FallbackConfig().Validate()}, {"process.limits", processowner.Limits(c.Process.Limits).Validate()},
		{"ingress", rtmp.Limits(c.Ingress).ValidateIngress()}, {"gpu", gpu.Config(c.GPU).Validate()},
		{"logging", logging.Config(c.Logging).Validate()}, {"api", api.Limits(c.API).Validate()},
	}
	// check содержит очередной именованный результат проверки компонента.
	for _, check := range checks {
		if check.err != nil {
			return fmt.Errorf("%s: %w", check.name, check.err)
		}
	}
	// err сообщает о неположительном или переполненном общем сроке допуска OBS.
	if _, err := c.OBSAdmissionTimeout(); err != nil {
		return fmt.Errorf("ingress admission timeout: %w", err)
	}
	if c.Process.GracePeriod <= 0 {
		return fmt.Errorf("process.grace_period: требуется положительный срок")
	}
	// Общие владельцы вмещают настроенные сессии и их максимальные дочерние узлы.
	// sessions задаёт общий предел сессий; groups задаёт предел групп одной сессии; outputs задаёт
	// предел её назначений для сверки общих бюджетов.
	sessions, groups, outputs := uint64(c.Session.MaxSessions), uint64(c.Session.MaxGroups), uint64(c.Session.MaxOutputs)
	if uint64(c.Observation.MaxSessions) < sessions || uint64(c.Observation.MaxGroups)/sessions < groups || uint64(c.Observation.MaxOutputs)/sessions < outputs || uint64(c.Observation.MaxFFmpegWorkers)/sessions/2 < groups || uint64(c.OutputExecution.MaxReservations)/sessions < outputs {
		return fmt.Errorf("observation/output_execution: общие бюджеты меньше пределов session")
	}
	if c.Session.MaxReferences < uint32(c.Session.MaxOutputs) || uint64(c.Session.MaxOutputs) > uint64(^uint32(0)) {
		return fmt.Errorf("session.max_references: меньше max_outputs")
	}
	if c.Router.MaxConsumers < c.Session.MaxGroups || uint64(c.Router.MaxConsumers) < outputs || c.Queue.MaxConfigBytes > c.Router.MaxConfigBytes || c.Publisher.Queue.MaxConfigBytes > c.Router.MaxConfigBytes {
		return fmt.Errorf("router: несовместимые потребители или конфигурации")
	}
	// usable содержит байты бюджета маршрутизатора после резерва двух конфигураций кодеков для
	// проверки ёмкости очередей.
	usable := c.Router.MaxRetainedBytes - 2*c.Router.MaxConfigBytes
	if c.Queue.MaxBytes > usable/groups || c.Publisher.Queue.MaxBytes > usable/outputs {
		return fmt.Errorf("router.max_retained_bytes: недостаточно резерва очередей")
	}
	if uint64(c.Ingress.MaxConfigBytes) > c.Router.MaxConfigBytes || uint64(c.Ingress.MaxMessageBytes) > c.Queue.MaxPacketBytes || c.Container.MaxSampleBytes > c.Queue.MaxPacketBytes || c.Container.MaxExtradataBytes > c.Queue.MaxConfigBytes {
		return fmt.Errorf("container/ingress: пакет или конфигурация превышают очередь")
	}
	if c.Fallback.MaxUploadBytes < uint64(c.API.UploadBytes) {
		return fmt.Errorf("api.upload_bytes: превышает fallback.max_upload_bytes")
	}
	if c.GPU.MaxDevices > c.Telemetry.MaxGPUDevices || uint64(c.GPU.MaxUUIDBytes) > c.Telemetry.MaxGPUUUIDBytes || c.GPU.MaxSnapshotBytes > c.Telemetry.MaxGPUBytes {
		return fmt.Errorf("gpu/telemetry: несогласованные бюджеты")
	}
	if c.FFmpeg.MaxCommandBytes > uint64(c.Process.Limits.MaxCommandBytes) || c.FFmpeg.MaxCommandArgs > c.Process.Limits.MaxCommandArgs || c.Fallback.Runner.MaxCommandBytes > uint64(c.Process.Limits.MaxCommandBytes) || c.Fallback.Runner.MaxArgs > c.Process.Limits.MaxCommandArgs {
		return fmt.Errorf("process.limits: недостаточный бюджет команд")
	}
	return nil
}

// validHost проверяет синтаксис пустого адреса привязки, IP или DNS-имени без сетевого разрешения.
func validHost(host string) bool {
	if host == "" {
		return true
	}
	// err содержит результат синтаксического разбора host как IP-адреса без обращения к DNS.
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if len(host) > 253 || strings.ContainsAny(host, ":%") {
		return false
	}
	host = strings.TrimSuffix(host, ".")
	// label содержит очередную часть DNS-имени между точками для проверки длины и допустимых
	// символов.
	for label := range strings.SplitSeq(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		// ch содержит очередной символ DNS-части, проверяемый на латинскую букву, цифру или
		// дефис.
		for _, ch := range label {
			switch {
			case 'a' <= ch && ch <= 'z', 'A' <= ch && ch <= 'Z', '0' <= ch && ch <= '9', ch == '-':
			default:
				return false
			}
		}
	}
	return true
}
