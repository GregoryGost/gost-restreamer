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
	// ErrOutputDrainTimeout обозначает истечение срока дочитывания выходного соединения после
	// завершения процесса.
	ErrOutputDrainTimeout = errors.New("истёк срок чтения результата процесса")
	// ErrPrematureExit обозначает завершение процесса до подтверждения обеих выходных дорожек.
	ErrPrematureExit = errors.New("процесс завершился до готовности обеих дорожек")
	// ErrInvalidSupervisor обозначает неподдержанные настройки супервизора или команды.
	ErrInvalidSupervisor = errors.New("некорректные настройки супервизора")
	// ErrStopped запрещает запуск после начала остановки супервизора.
	ErrStopped = errors.New("супервизор остановлен")
	// ErrAlreadyStarted отклоняет повторный запуск того же супервизора.
	ErrAlreadyStarted = errors.New("супервизор уже запущен")
	// ErrOwnerRevoked сообщает об отзыве допуска владельца логического работника.
	ErrOwnerRevoked = errors.New("разрешение владельца отозвано")
	// ErrStartupTimeout обозначает отсутствие первых пакетов обеих дорожек в срок запуска.
	ErrStartupTimeout = errors.New("истёк срок готовности дорожек")
	// ErrProgressTimeout обозначает превышение допустимого перерыва продвижения одной из дорожек.
	ErrProgressTimeout = errors.New("нет продвижения медиапакетов")
	// ErrCleanupPending обозначает отсутствие подтверждения физического завершения ресурсов попытки.
	ErrCleanupPending = errors.New("физическое завершение ещё не подтверждено")
)

// OwnerPermit допускает только изменение памяти под общим допуском владельца.
type OwnerPermit interface {

	// Do выполняет действие изменения памяти только под действующим общим допуском владельца; запуск
	// процессов и ожидание внешних действий выполняются после возврата.
	Do(context.Context, func() error) error

	// Valid сообщает текущую действительность владельца для запрета новых попыток и определения причины
	// завершения.
	Valid() bool

	// Done возвращает сигнал отзыва владельца, по которому Supervisor отменяет уже выполняющегося
	// работника.
	Done() <-chan struct{}
}

// Config содержит явные конечные ограничения одного логического работника.
type Config struct {
	// BaseDir задаёт корень временных UDS для попыток без ProcessScope.
	BaseDir string
	// StartupTimeout ограничивает ожидание первых проверенных пакетов обеих выходных дорожек.
	StartupTimeout time.Duration
	// VideoTimeout задаёт допустимый перерыв между продвижениями DTS видео после готовности обеих
	// дорожек.
	VideoTimeout time.Duration
	// AudioTimeout задаёт допустимый перерыв между продвижениями DTS аудио после готовности обеих
	// дорожек.
	AudioTimeout time.Duration
	// WatchdogInterval задаёт период проверки продвижения выходных медиапакетов.
	WatchdogInterval time.Duration
	// MaxRestarts ограничивает число повторов после первоначальной физической попытки.
	MaxRestarts int
	// RestartBackoff задаёт паузу между завершившейся неудачной попыткой и следующим запуском.
	RestartBackoff time.Duration
	// GracePeriod задаёт срок мягкой остановки прямого тестового процесса и часть общего ожидания
	// завершения попытки.
	GracePeriod time.Duration
	// KillTimeout дополняет GracePeriod при ожидании завершения процесса после отмены попытки.
	KillTimeout time.Duration
	// ShutdownTimeout ограничивает ожидание Stop, дочитывание выхода и завершение зарегистрированной
	// операции попытки.
	ShutdownTimeout time.Duration
	// DiagnosticEvents ограничивает число событий в очереди диагностики работника.
	DiagnosticEvents int
	// DiagnosticBytes ограничивает суммарный логический вес событий очереди в байтах.
	DiagnosticBytes uint64
	// DiagnosticChunkBytes ограничивает размер одного фрагмента stdout или stderr в байтах.
	DiagnosticChunkBytes int
	// MaxCommandBytes ограничивает суммарное число байтов исполняемого пути и аргументов FFmpeg.
	MaxCommandBytes uint64
	// MaxCommandArgs ограничивает число отдельных аргументов команды FFmpeg.
	MaxCommandArgs int
	// Progress задаёт ограничения разбора отчётов FFmpeg о ходе кодирования.
	Progress ProgressConfig
}

// Validate проверяет абсолютный чистый BaseDir, положительные сроки, бюджеты команды и диагностики и
// возможность вместить полные аргументы и фрагмент вывода без усечения.
func (c Config) Validate() error {
	// err сообщает об отказе проверки ограничений разбора progress.
	if err := c.Progress.Validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(c.BaseDir) || c.BaseDir != filepath.Clean(c.BaseDir) {
		return fmt.Errorf("%w: требуется абсолютный BaseDir", ErrInvalidSupervisor)
	}
	// d содержит очередной срок из настроек, который должен быть положительным.
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

// Attempt содержит уникальный идентификатор и уже созданные UDS данной попытки.
type Attempt struct {
	// ID содержит последовательный номер физической попытки внутри супервизора.
	ID uint64
	// InputPath задаёт путь входного UDS; OutputPath задаёт путь выходного UDS этой попытки.
	InputPath, OutputPath string
}

// Topology определяет, читает ли процесс медиаданные из UDS или из переданного файла.
type Topology uint8

const (
	// Duplex выбирает отдельные входное и выходное соединения UDS.
	Duplex Topology = iota
	// FileInput выбирает файловый вход и единственное выходное соединение UDS.
	FileInput
)

// Command содержит исполняемый путь, отдельные аргументы и передаваемые процессу файловые дескрипторы.
type Command struct {
	// Binary задаёт исполняемый файл запускаемой команды.
	Binary string
	// Args содержит отдельные аргументы процесса без интерпретации командной оболочкой.
	Args []string
	// ExtraFiles передаются во владение попытки сразу после возврата построителя команды.
	ExtraFiles []*os.File
}

// CommandBuilder строит команду для конкретной попытки и возвращает также подлежащие закрытию
// ExtraFiles.
type CommandBuilder func(context.Context, Attempt) (Command, error)

// Track задаёт дорожку проверенного выходного пакета.
type Track uint8

const (
	// Video обозначает видеодорожку при отчёте о продвижении проверенных пакетов.
	Video Track = iota + 1
	// Audio обозначает аудиодорожку при отчёте о продвижении проверенных пакетов.
	Audio
)

// Progress принимает DTS только валидных медиапакетов в неизменной шкале дорожки.
// Заголовки и открытые соединения сюда не передаются. Равные DTS не обновляют контроль продвижения.
type Progress func(Track, int64) bool

// IOHandler обязан завершаться при отмене ctx либо закрытии соединения.
// Обработчик сообщает Progress после проверки пакета.
type IOHandler func(context.Context, net.Conn, Progress) error

// Worker принадлежит Session/Group, независимо от контекста подключения OBS.
// Owner уже связан вызывающим с сохранённым разрешением на исполнение; супервизор его не изменяет.
type Worker struct {
	// SessionID связывает диагностику работника с сессией.
	SessionID string
	// GroupID связывает диагностику работника с группой медиатракта.
	GroupID string
	// Owner разрешает регистрацию и повтор попыток и сообщает об отзыве работника.
	Owner OwnerPermit
	// ProcessScope обязателен в рабочей сборке и учитывает все попытки Worker.
	ProcessScope *processowner.Scope
	// Topology выбирает соединения и допустимое число ExtraFiles для попытки.
	Topology Topology
	// Command строит команду после создания пространства имён попытки.
	Command CommandBuilder
	// Input обслуживает входное UDS-соединение в режиме Duplex.
	Input IOHandler
	// Output читает выходное UDS-соединение и сообщает о проверенных медиапакетах.
	Output IOHandler
	// OnAttemptResult вызывается один раз после присоединения операций попытки и очистки,
	// вне блокировок и до решения о повторе. Обработчик обязан завершаться за конечное
	// время, не ожидая вывода журнала и не вызывая синхронно Stop или Wait этого Supervisor.
	OnAttemptResult func(AttemptResult)
}

// AttemptResult содержит исход одной физической попытки с её неизменным локальным ID.
// Stopped относится к отмене логического работника, а не внутреннего контекста попытки.
// CleanupErr сохраняет ошибку очистки отдельно от ошибки выполнения.
type AttemptResult struct {
	// ID содержит номер физической попытки, к которой относится результат.
	ID uint64
	// Err содержит причину завершения выполнения этой попытки.
	Err        error
	CleanupErr error
	Stopped    bool
}

// Result разделяет исход работника и доказательство физической очистки.
type Result struct {
	// Attempts содержит число начатых физических попыток логического работника.
	Attempts uint64
	// Err содержит итоговую причину завершения логического работника.
	Err error
	// CleanupErr объединяет ошибки физической очистки его попыток.
	CleanupErr error
	// Clean показывает, завершилась ли очистка без зарегистрированных ошибок.
	Clean bool
}
