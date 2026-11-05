package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

var (
	ErrOutputDrainTimeout = errors.New("истёк срок чтения результата процесса")
	ErrPrematureExit      = errors.New("процесс завершился до готовности обеих дорожек")
	ErrInvalidSupervisor  = errors.New("некорректные настройки супервизора")
	ErrStopped            = errors.New("супервизор остановлен")
	ErrAlreadyStarted     = errors.New("супервизор уже запущен")
	ErrOwnerRevoked       = errors.New("разрешение владельца отозвано")
	ErrStartupTimeout     = errors.New("истёк срок готовности дорожек")
	ErrProgressTimeout    = errors.New("нет продвижения медиапакетов")
	ErrCleanupPending     = errors.New("физическое завершение ещё не подтверждено")
)

// OwnerPermit допускает только изменение памяти под общей authority владельца.
type OwnerPermit interface {
	Do(context.Context, func() error) error
	Valid() bool
	Done() <-chan struct{}
}

// Config содержит явные конечные ограничения одного логического worker.
type Config struct {
	BaseDir              string
	StartupTimeout       time.Duration
	VideoTimeout         time.Duration
	AudioTimeout         time.Duration
	WatchdogInterval     time.Duration
	MaxRestarts          int
	RestartBackoff       time.Duration
	GracePeriod          time.Duration
	KillTimeout          time.Duration
	ShutdownTimeout      time.Duration
	DiagnosticEvents     int
	DiagnosticBytes      uint64
	DiagnosticChunkBytes int
	MaxCommandBytes      uint64
	MaxCommandArgs       int
}

func (c Config) Validate() error {
	if !filepath.IsAbs(c.BaseDir) || c.BaseDir != filepath.Clean(c.BaseDir) {
		return fmt.Errorf("%w: требуется абсолютный BaseDir", ErrInvalidSupervisor)
	}
	for _, d := range []time.Duration{c.StartupTimeout, c.VideoTimeout, c.AudioTimeout, c.WatchdogInterval, c.RestartBackoff, c.GracePeriod, c.KillTimeout, c.ShutdownTimeout} {
		if d <= 0 {
			return fmt.Errorf("%w: сроки должны быть положительными", ErrInvalidSupervisor)
		}
	}
	if c.GracePeriod > time.Duration(1<<63-1)-c.KillTimeout {
		return fmt.Errorf("%w: переполнение срока shutdown", ErrInvalidSupervisor)
	}
	if c.MaxRestarts < 0 || c.DiagnosticEvents <= 0 || c.DiagnosticChunkBytes <= 0 || c.MaxCommandArgs <= 0 || c.MaxCommandBytes == 0 || c.DiagnosticBytes == 0 {
		return fmt.Errorf("%w: некорректные бюджеты", ErrInvalidSupervisor)
	}
	// Учёт включает Session/Group, заголовок события и отдельные argv, без скрытого усечения.
	if c.DiagnosticBytes < 768 || c.MaxCommandBytes > c.DiagnosticBytes-768 || uint64(c.MaxCommandArgs) > (c.DiagnosticBytes-768-c.MaxCommandBytes)/16 || uint64(c.DiagnosticChunkBytes) > c.DiagnosticBytes-768 {
		return fmt.Errorf("%w: диагностический бюджет слишком мал", ErrInvalidSupervisor)
	}
	return nil
}

// Attempt содержит уникальную identity и уже созданные UDS данной попытки.
type Attempt struct {
	ID                    uint64
	InputPath, OutputPath string
}
type Topology uint8

const (
	Duplex Topology = iota
	FileInput
)

type Command struct {
	Binary string
	Args   []string
	// ExtraFiles передаются во владение попытки сразу после возврата построителя команды.
	ExtraFiles []*os.File
}
type CommandBuilder func(context.Context, Attempt) (Command, error)

// Track задаёт дорожку проверенного выходного пакета.
type Track uint8

const (
	Video Track = iota + 1
	Audio
)

// Progress принимает DTS только валидных медиапакетов в неизменной шкале дорожки.
// Headers и открытые соединения сюда не передаются. Равные DTS не обновляют watchdog.
type Progress func(Track, int64) bool

// IOHandler обязан завершаться при отмене ctx либо закрытии соединения.
// Progress сообщается после проверки пакета; данные callback не удерживаются супервизором.
type IOHandler func(context.Context, net.Conn, Progress) error

// Worker принадлежит Session/Group, независимо от контекста подключения OBS.
// Owner уже связан вызывающим с сохранённой issuance; супервизор её не изменяет.
type Worker struct {
	SessionID string
	GroupID   string
	Owner     OwnerPermit
	// ProcessScope обязателен в рабочей сборке и учитывает все попытки Worker.
	ProcessScope *processowner.Scope
	Topology     Topology
	Command      CommandBuilder
	Input        IOHandler
	Output       IOHandler
}

// Result разделяет исход worker и доказательство физической очистки.
type Result struct {
	Attempts   uint64
	Err        error
	CleanupErr error
	Clean      bool
}
