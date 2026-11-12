package core

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	// ErrInvalid позволяет распознать нарушение допустимости доменного значения.
	ErrInvalid = errors.New("некорректное доменное значение")
	// ErrNotFound обозначает отсутствие запрошенной сущности.
	ErrNotFound = errors.New("объект не найден")
	// ErrConflict обозначает конфликт состояния или ожидаемой ревизии.
	ErrConflict = errors.New("конфликт состояния или версии")
	// ErrInUse запрещает изменение или удаление занятой зависимости.
	ErrInUse = errors.New("объект используется")
	// ErrNotReady обозначает отсутствие требуемой готовности объекта или текущего наблюдения.
	ErrNotReady = errors.New("объект не готов")
	// ErrStale обозначает отозванную или заменённую попытку выполнения.
	ErrStale = errors.New("устаревшая попытка")
	// ErrBusy обозначает занятость хранилища или недоступность требуемого ресурса подготовки.
	ErrBusy = errors.New("хранилище занято")
	// ErrCorrupt обозначает повреждение сохраняемых данных.
	ErrCorrupt = errors.New("повреждённые данные")
	// ErrClosed обозначает обращение к закрытому хранилищу.
	ErrClosed = errors.New("хранилище закрыто")
	// ErrInvalidCursor обозначает недопустимый курсор списка.
	ErrInvalidCursor = errors.New("некорректный курсор")
	// ErrInvalidMetadata обозначает нарушение формата сохраняемых метаданных.
	ErrInvalidMetadata = errors.New("некорректные метаданные")
	// ErrInvalidWatchdogPolicy обозначает неположительный интервал контроля или восстановления
	// источника.
	ErrInvalidWatchdogPolicy = errors.New("некорректные интервалы контроля источника")
)

// ID идентифицирует доменную сущность без изменения исходных байтов идентификатора.
type ID string

// Record объединяет идентификатор, ревизию и временные метки сохраняемой сущности.
type Record struct {
	// ID идентифицирует эту запись.
	ID ID
	// Revision содержит ревизию для условного изменения записи.
	Revision uint64
	// CreatedAt сохраняет время создания записи.
	CreatedAt time.Time
	// UpdatedAt сохраняет время последнего изменения записи.
	UpdatedAt time.Time
}

// NextRevision увеличивает ревизию на единицу и возвращает ErrConflict при исчерпании uint64.
func NextRevision(current uint64) (uint64, error) {
	if current == math.MaxUint64 {
		return 0, fmt.Errorf("%w: исчерпана версия", ErrConflict)
	}
	return current + 1, nil
}

// validText отклоняет пустой после удаления пробелов текст, некорректный UTF-8 и нулевые символы; имя
// поля включается в ошибку ErrInvalid.
func validText(name, value string) error {
	if strings.TrimSpace(value) == "" || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("%w: %s должен содержать допустимый текст", ErrInvalid, name)
	}
	return nil
}

// Validate проверяет ID как непустой допустимый текст без изменения его байтов.
func (id ID) Validate() error { return validText("ID", string(id)) }

// WatchdogPolicy задаёт интервалы контроля и восстановления обеих дорожек.
type WatchdogPolicy struct {
	// VideoTimeout задаёт допустимое отсутствие видеоданных до переключения источника.
	VideoTimeout time.Duration
	// AudioTimeout задаёт допустимое отсутствие аудиоданных до переключения источника.
	AudioTimeout time.Duration
	// RecoveryWindow задаёт окно проверки восстановившегося LIVE-источника.
	RecoveryWindow time.Duration
	// KeyframeTimeout ограничивает ожидание ключевого кадра при восстановлении LIVE.
	KeyframeTimeout time.Duration
}

// Validate требует положительных интервалов контроля, восстановления и ожидания ключевого кадра.
func (p WatchdogPolicy) Validate() error {
	// field содержит очередной интервал контроля вместе с именем поля для диагностики.
	for _, field := range []struct {
		// name указывает имя проверяемого интервала в ошибке.
		name string
		// value содержит проверяемую длительность интервала.
		value time.Duration
	}{{"VideoTimeout", p.VideoTimeout}, {"AudioTimeout", p.AudioTimeout}, {"RecoveryWindow", p.RecoveryWindow}, {"KeyframeTimeout", p.KeyframeTimeout}} {
		if field.value <= 0 {
			return fmt.Errorf("%w: WatchdogPolicy.%s должен быть положительным", ErrInvalidWatchdogPolicy, field.name)
		}
	}
	return nil
}

// FallbackPolicy связывает выбранную заглушку с интервалами контроля и восстановления LIVE-источника.
type FallbackPolicy struct {
	// AssetID идентифицирует заглушку-владельца материала.
	AssetID ID
	// Watchdog содержит интервалы контроля и восстановления обеих LIVE-дорожек.
	Watchdog WatchdogPolicy
}

// Validate проверяет идентификатор заглушки и интервалы контроля источника.
func (p FallbackPolicy) Validate() error {
	// err содержит причину отказа проверки идентификатора заглушки.
	if err := p.AssetID.Validate(); err != nil {
		return err
	}
	return p.Watchdog.Validate()
}

// Stream хранит текущие настройки потока, постоянный ключ публикации и политику заглушки.
type Stream struct {
	// Record содержит идентификатор, ревизию и временные метки сущности.
	Record
	// Name содержит отображаемое имя сущности.
	Name string
	// PublicationKey содержит постоянный ключ допуска публикации OBS.
	PublicationKey string
	// Fallback задаёт выбранную заглушку и контроль LIVE-источника.
	Fallback FallbackPolicy
}

// Validate проверяет имя и ключ публикации; полностью незаданная политика заглушки допустима.
func (s Stream) Validate() error {
	// err содержит причину отказа проверки имени потока.
	if err := validText("Stream.Name", s.Name); err != nil {
		return err
	}
	// err содержит причину отказа проверки ключа публикации потока.
	if err := validText("Stream.PublicationKey", s.PublicationKey); err != nil {
		return err
	}
	if s.Fallback.AssetID == "" && s.Fallback.Watchdog == (WatchdogPolicy{}) {
		return nil
	}
	return s.Fallback.Validate()
}

// TranscodeProfile хранит именованный полный профиль обработки дорожек, переиспользуемый группами.
type TranscodeProfile struct {
	// Record содержит идентификатор, ревизию и временные метки сущности.
	Record
	// Name содержит отображаемое имя сущности.
	Name string
	// Profile содержит полный профиль обработки видео и аудио.
	Profile GroupProfile
}

// Validate проверяет имя и полный профиль обработки дорожек.
func (p TranscodeProfile) Validate() error {
	// err содержит причину отказа проверки имени профиля.
	if err := validText("TranscodeProfile.Name", p.Name); err != nil {
		return err
	}
	return p.Profile.Validate()
}

// TranscodeGroup связывает группу конкретного потока с переиспользуемым профилем.
type TranscodeGroup struct {
	// Record содержит идентификатор, ревизию и временные метки сущности.
	Record
	// StreamID указывает поток, которому принадлежит группа.
	StreamID ID
	// ProfileID указывает переиспользуемый полный профиль группы.
	ProfileID ID
	// Name содержит отображаемое имя сущности.
	Name string
}

// Validate проверяет ссылки на Stream и профиль, а также непустое имя группы.
func (g TranscodeGroup) Validate() error {
	// err содержит причину отказа проверки ссылки группы на поток.
	if err := g.StreamID.Validate(); err != nil {
		return err
	}
	// err содержит причину отказа проверки ссылки группы на профиль.
	if err := g.ProfileID.Validate(); err != nil {
		return err
	}
	return validText("TranscodeGroup.Name", g.Name)
}

// Destination хранит площадку, адрес и ключ назначения для выбранной группы потока.
type Destination struct {
	// Record содержит идентификатор, ревизию и временные метки сущности.
	Record
	// StreamID указывает поток-владелец назначения.
	StreamID ID
	// GroupID указывает группу того же потока, данные которой отправляются на площадку.
	GroupID ID
	// Name содержит отображаемое имя сущности.
	Name string
	// Platform содержит обозначение площадки или пользовательского адаптера.
	Platform string
	// Endpoint содержит RTMP/RTMPS-адрес назначения без фрагмента.
	Endpoint string
	// StreamKey содержит ключ публикации назначения; пустая строка допустима.
	StreamKey string
	// Enabled определяет включение назначения в настройки запуска.
	Enabled bool
}

// Validate проверяет ссылки, имя, площадку, адрес RTMP/RTMPS без фрагмента и допустимость текста
// ключа; пустой ключ разрешён.
func (d Destination) Validate() error {
	// err содержит причину отказа проверки потока назначения.
	if err := d.StreamID.Validate(); err != nil {
		return err
	}
	// err содержит причину отказа проверки группы назначения.
	if err := d.GroupID.Validate(); err != nil {
		return err
	}
	// err содержит причину отказа проверки имени назначения.
	if err := validText("Destination.Name", d.Name); err != nil {
		return err
	}
	// err содержит причину отказа проверки обозначения площадки.
	if err := validText("Destination.Platform", d.Platform); err != nil {
		return err
	}
	// u содержит разобранный адрес назначения, а err — причину отказа разбора URL.
	u, err := url.Parse(d.Endpoint)
	if err != nil || (u.Scheme != "rtmp" && u.Scheme != "rtmps") || u.Hostname() == "" || u.Fragment != "" || !utf8.ValidString(d.Endpoint) {
		return fmt.Errorf("%w: Destination.Endpoint должен быть RTMP или RTMPS URL", ErrInvalid)
	}
	if !utf8.ValidString(d.StreamKey) || strings.ContainsRune(d.StreamKey, 0) {
		return fmt.Errorf("%w: Destination.StreamKey", ErrInvalid)
	}
	return nil
}

// ValidateManagedPath принимает только непустой нормализованный относительный путь без выхода через
// .., обратных косых черт и нулевых символов.
func ValidateManagedPath(value string) error {
	if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\\\x00") || path.IsAbs(value) || path.Clean(value) != value || value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("%w: путь должен быть относительным и нормализованным", ErrInvalid)
	}
	return nil
}

// AssetKind различает изображения и видео, используемые как оригинал заглушки.
type AssetKind string

const (
	// AssetImage обозначает оригинал заглушки в виде изображения.
	AssetImage AssetKind = "image"
	// AssetVideo обозначает оригинал заглушки в виде видео.
	AssetVideo AssetKind = "video"
)

// AssetState описывает состояние совместимой проекции заглушки.
type AssetState string

const (
	// AssetPreparing обозначает подготовку заглушки.
	AssetPreparing AssetState = "PREPARING"
	// AssetReady обозначает готовую заглушку.
	AssetReady AssetState = "READY"
	// AssetFailed обозначает отказ подготовки заглушки.
	AssetFailed AssetState = "FAILED"
)

// SourceFormat описывает исходные дорожки материала или формат неподвижного изображения.
type SourceFormat struct {
	// Video описывает видеодорожку исходного материала, если она имеется.
	Video *VideoFormat
	// Audio описывает аудиодорожку исходного материала, если она имеется.
	Audio *AudioFormat
	// ImageFormat содержит формат неподвижного изображения.
	ImageFormat string
}

// FallbackAsset хранит стабильную заглушку и указатели её текущего и подготавливаемого поколений.
type FallbackAsset struct {
	// Record содержит идентификатор, ревизию и временные метки сущности.
	Record
	// CurrentGenerationID указывает опубликованное рабочее поколение заглушки.
	CurrentGenerationID ID
	// PendingGenerationID указывает подготавливаемое поколение до его публикации.
	PendingGenerationID ID
	// Incarnation идентифицирует открытие хранилища, которому принадлежит сохраняемая попытка.
	Incarnation string
	// Name содержит отображаемое имя сущности.
	Name string
	// Kind определяет изображение или видео в совместимой проекции текущего материала.
	Kind AssetKind
	// State отражает состояние заглушки в совместимой проекции.
	State AssetState
	// AttemptToken различает владение попыткой подготовки.
	AttemptToken string
	// OriginalPath сохраняет относительный путь оригинала заглушки.
	OriginalPath string
	// SourceHash содержит хеш исходного материала, из которого подготовлен вариант.
	SourceHash string
	// SizeBytes содержит размер файла в байтах.
	SizeBytes uint64
	// SourceFormat содержит проверенный формат исходного материала.
	SourceFormat SourceFormat
}

// Validate проверяет имя, указатели поколений, состояние и исходный формат; текущее и ожидающее
// поколения должны различаться.
func (a FallbackAsset) Validate() error {
	// err содержит причину отказа проверки имени заглушки.
	if err := validText("FallbackAsset.Name", a.Name); err != nil {
		return err
	}
	// id содержит очередной указатель текущего или ожидающего поколения.
	for _, id := range []ID{a.CurrentGenerationID, a.PendingGenerationID} {
		if id != "" {
			// err содержит причину отказа проверки непустого указателя поколения.
			if err := id.Validate(); err != nil {
				return err
			}
		}
	}
	if a.CurrentGenerationID != "" && a.CurrentGenerationID == a.PendingGenerationID {
		return ErrInvalid
	}
	if a.State != AssetPreparing && a.State != AssetReady && a.State != AssetFailed {
		return ErrInvalid
	}
	return a.SourceFormat.Validate()
}

// VariantState описывает готовность отдельного варианта заглушки.
type VariantState string

const (
	// VariantPreparing обозначает вариант, подготовка которого ещё не завершена.
	VariantPreparing VariantState = "PREPARING"
	// VariantReady обозначает проверенный готовый вариант.
	VariantReady VariantState = "READY"
	// VariantFailed обозначает неуспешную подготовку варианта.
	VariantFailed VariantState = "FAILED"
)

// FallbackVariant хранит файл одного профиля точного поколения и результаты проверки его содержимого.
type FallbackVariant struct {
	// Record содержит идентификатор, ревизию и временные метки сущности.
	Record
	// GenerationID идентифицирует точное поколение заглушки.
	GenerationID ID
	// VerifiedFormat сохраняет фактически проверенный формат подготовленного файла.
	VerifiedFormat MediaFormat
	// Incarnation идентифицирует открытие хранилища, которому принадлежит сохраняемая попытка.
	Incarnation string
	// AssetID идентифицирует заглушку-владельца материала.
	AssetID ID
	// SourceHash содержит хеш исходного материала, из которого подготовлен вариант.
	SourceHash string
	// Profile сохраняет полный профиль, по которому подготовлен файл.
	Profile GroupProfile
	// ProfileKey содержит ключ канонической идентичности профиля.
	ProfileKey string
	// CanonicalProfile сохраняет точные канонические байты профиля для проверки совпадения.
	CanonicalProfile []byte
	// GeneratorVersion фиксирует версию генератора подготовленного материала.
	GeneratorVersion string
	// ManagedPath сохраняет относительный принадлежащий приложению путь.
	ManagedPath string
	// SizeBytes содержит размер файла в байтах.
	SizeBytes uint64
	// ContentHash содержит хеш содержимого подготовленного файла.
	ContentHash string
	// State отражает этап подготовки и готовность варианта.
	State VariantState
	// AttemptToken различает владение попыткой подготовки.
	AttemptToken string
}

// Validate проверяет владельца, профиль, его каноническую идентичность и путь варианта. Для READY
// дополнительно требуются совместимый проверенный формат, ненулевой размер и хеш содержимого.
func (v FallbackVariant) Validate() error {
	// err содержит причину отказа проверки идентификатора владельца открытия хранилища варианта.
	if err := validText("FallbackVariant.Incarnation", v.Incarnation); err != nil {
		return err
	}
	// err содержит причину отказа проверки заглушки-владельца варианта.
	if err := v.AssetID.Validate(); err != nil {
		return err
	}
	if v.GenerationID == "" {
		return ErrInvalid
	}
	if v.State == VariantReady && v.SourceHash == "" {
		return ErrInvalid
	}
	// err содержит причину отказа проверки полного профиля варианта.
	if err := v.Profile.Validate(); err != nil {
		return err
	}
	// err содержит причину отказа проверки версии генератора варианта.
	if err := validText("FallbackVariant.GeneratorVersion", v.GeneratorVersion); err != nil {
		return err
	}
	// err содержит причину отказа проверки относительного пути варианта.
	if err := ValidateManagedPath(v.ManagedPath); err != nil {
		return err
	}
	// err содержит причину отказа проверки токена попытки подготовки варианта.
	if err := validText("FallbackVariant.AttemptToken", v.AttemptToken); err != nil {
		return err
	}
	if v.State != VariantPreparing && v.State != VariantReady && v.State != VariantFailed {
		return fmt.Errorf("%w: состояние варианта", ErrInvalid)
	}
	// key содержит вычисленный ключ профиля, canonical — его точные канонические байты, а err —
	// причину отказа вычисления идентичности.
	key, canonical, err := ProfileIdentity(v.Profile)
	if err != nil {
		return err
	}
	if v.ProfileKey != key || !bytes.Equal(v.CanonicalProfile, canonical) {
		return fmt.Errorf("%w: идентичность профиля варианта", ErrInvalid)
	}
	if v.State == VariantReady {
		// err содержит причину несовместимости проверенного формата варианта с его профилем.
		if err := CompatibleFormat(v.VerifiedFormat, v.Profile.Format); err != nil {
			return fmt.Errorf("%w: фактический формат варианта", ErrInvalid)
		}
		if v.SizeBytes == 0 {
			return fmt.Errorf("%w: пустой вариант", ErrInvalid)
		}
		return validText("FallbackVariant.ContentHash", v.ContentHash)
	}
	return nil
}

// SessionState описывает долговечный жизненный цикл запуска потока.
type SessionState string

const (
	// SessionStarting обозначает запуск, ожидающий готовности исполнения.
	SessionStarting SessionState = "STARTING"
	// SessionRunning обозначает принятый работающий запуск, в том числе при ручной паузе.
	SessionRunning SessionState = "RUNNING"
	// SessionStopping обозначает принятое завершение запуска до окончательного результата.
	SessionStopping SessionState = "STOPPING"
	// SessionStopped обозначает штатно завершённый запуск.
	SessionStopped SessionState = "STOPPED"
	// SessionFailed обозначает завершение запуска с отказом.
	SessionFailed SessionState = "FAILED"
	// SessionInterrupted обозначает прерванный запуск прежнего процесса.
	SessionInterrupted SessionState = "INTERRUPTED"
)

// Active сообщает, занимает ли состояние слот нетерминальной сессии: STARTING, RUNNING или STOPPING.
func (s SessionState) Active() bool {
	return s == SessionStarting || s == SessionRunning || s == SessionStopping
}

// Terminal распознаёт завершённые состояния STOPPED, FAILED и INTERRUPTED.
func (s SessionState) Terminal() bool {
	return s == SessionStopped || s == SessionFailed || s == SessionInterrupted
}

// OutputState описывает наблюдаемое состояние соединения отдельного выхода.
type OutputState string

const (
	// OutputConnecting обозначает установление соединения выхода с назначением.
	OutputConnecting OutputState = "CONNECTING"
	// OutputPublishing обозначает наблюдаемую публикацию выхода.
	OutputPublishing OutputState = "PUBLISHING"
	// OutputRetrying обозначает ожидание или выполнение повторного подключения выхода.
	OutputRetrying OutputState = "RETRYING"
	// OutputFailed обозначает окончательный отказ исполнения выхода.
	OutputFailed OutputState = "FAILED"
	// OutputStopped обозначает остановленный выход.
	OutputStopped OutputState = "STOPPED"
)

// GroupSnapshot сохраняет группу и полный профиль на момент формирования снимка.
type GroupSnapshot struct {
	// Group сохраняет настройки группы на момент создания снимка.
	Group TranscodeGroup
	// Profile сохраняет полный именованный профиль этой группы.
	Profile TranscodeProfile
}

// SessionSnapshot сохраняет настройки потока и групп, принятые для конкретного запуска.
type SessionSnapshot struct {
	// Stream сохраняет настройки и ключ потока на момент запуска.
	Stream Stream
	// Groups сохраняет начальные группы с полными профилями запуска.
	Groups []GroupSnapshot
}

// ArtifactManifest фиксирует точный подготовленный файл и его профиль для использования выходом.
type ArtifactManifest struct {
	// GenerationID идентифицирует точное поколение заглушки.
	GenerationID ID
	// VerifiedFormat сохраняет фактически проверенный формат подготовленного файла.
	VerifiedFormat MediaFormat
	// VariantID идентифицирует точный подготовленный вариант.
	VariantID ID
	// AssetID идентифицирует заглушку-владельца материала.
	AssetID ID
	// SourceHash содержит хеш исходного материала, из которого подготовлен вариант.
	SourceHash string
	// Profile сохраняет полный профиль закреплённого варианта.
	Profile GroupProfile
	// ProfileKey содержит ключ канонической идентичности профиля.
	ProfileKey string
	// CanonicalProfile сохраняет точные канонические байты профиля для проверки совпадения.
	CanonicalProfile []byte
	// GeneratorVersion фиксирует версию генератора подготовленного материала.
	GeneratorVersion string
	// ManagedPath сохраняет относительный принадлежащий приложению путь.
	ManagedPath string
	// SizeBytes содержит размер файла в байтах.
	SizeBytes uint64
	// ContentHash содержит хеш содержимого подготовленного файла.
	ContentHash string
}

// OutputSnapshot сохраняет самостоятельные настройки назначения, обработки и заглушки одного выхода.
type OutputSnapshot struct {
	// Destination сохраняет площадку и реквизиты публикации выхода.
	Destination Destination
	// Group сохраняет настройки группы выхода.
	Group TranscodeGroup
	// Profile сохраняет полный профиль обработки выхода.
	Profile TranscodeProfile
	// Fallback сохраняет политику заглушки и контроля источника для выхода.
	Fallback FallbackPolicy
	// Artifact закрепляет точный подготовленный файл заглушки и его проверенные метаданные.
	Artifact ArtifactManifest
}

// Session сохраняет начальный снимок и долговечное управление одним запуском потока.
type Session struct {
	// Record содержит идентификатор, ревизию и временные метки сущности.
	Record
	// Issued отмечает выдачу внешней попытки запуска, требующей подтверждения очистки.
	Issued bool
	// StreamID указывает поток, для которого создан запуск.
	StreamID ID
	// Incarnation идентифицирует открытие хранилища, которому принадлежит сохраняемая попытка.
	Incarnation string
	// State сохраняет долговечное состояние запуска без подмены текущих наблюдений.
	State SessionState
	// DesiredPaused сохраняет намерение ручной паузы, не запрещая допуск OBS.
	DesiredPaused bool
	// Lifecycle содержит поколение долговечного жизненного цикла сессии.
	Lifecycle uint64
	// Snapshot сохраняет начальные настройки запуска независимо от текущей конфигурации.
	Snapshot SessionSnapshot
	// CleanupPending отмечает неподтверждённую очистку выданных ресурсов запуска.
	CleanupPending bool
	// Failure сохраняет итоговую причину терминального отказа или прерывания запуска.
	Failure string
	// EndedAt содержит время завершения запуска либо nil до его завершения.
	EndedAt *time.Time
}

// AllowsOBS допускает непустой ключ из снимка сессии только в RUNNING; ручная пауза допуск не меняет.
func (s Session) AllowsOBS(key string) bool {
	return key != "" && s.State == SessionRunning && key == s.Snapshot.Stream.PublicationKey
}

// SessionOutput сохраняет снимок и намерение исполнения отдельного выхода сессии.
type SessionOutput struct {
	// Record содержит идентификатор, ревизию и временные метки сущности.
	Record
	// SessionID идентифицирует сессию, к которой относится запись.
	SessionID ID
	// Snapshot сохраняет самостоятельные настройки и файл заглушки этого выхода.
	Snapshot OutputSnapshot
	// DesiredRunning сохраняет намерение исполнять выход.
	DesiredRunning bool
	// Attached разрешает повторный запуск выхода по сохранённому снимку.
	Attached bool
	// Generation различает поколения исполнения этого выхода.
	Generation uint64
	// Attempt содержит номер конкретной выданной попытки исполнения.
	Attempt uint64
	// Issued отмечает выдачу внешней попытки выхода.
	Issued bool
	// CleanupPending отмечает неподтверждённую очистку выданной попытки выхода.
	CleanupPending bool
}

// Event сохраняет служебное событие протокола управления отдельно от оперативной телеметрии.
type Event struct {
	// Execution указывает точную попытку окончательного отказа выхода, если событие его описывает.
	Execution *ExecutionFence `json:",omitempty"`
	// ID идентифицирует эту запись.
	ID ID
	// SessionID идентифицирует сессию, к которой относится запись.
	SessionID ID
	// OutputID идентифицирует отдельный выход; пустое значение относится ко всей сессии.
	OutputID ID
	// Kind содержит вид служебного события управления.
	Kind string
	// Message сохраняет пояснение события или итоговую причину отказа.
	Message string
	// CreatedAt сохраняет время служебного события.
	CreatedAt time.Time
}

// CommandKind различает операции управления сессией и её выходами.
type CommandKind string

const (
	// CommandStartSession обозначает ручной запуск потока.
	CommandStartSession CommandKind = "start_session"
	// CommandStopSession обозначает остановку сессии.
	CommandStopSession CommandKind = "stop_session"
	// CommandPauseSession обозначает включение ручной паузы сессии.
	CommandPauseSession CommandKind = "pause_session"
	// CommandResumeSession обозначает снятие ручной паузы сессии.
	CommandResumeSession CommandKind = "resume_session"
	// CommandAddOutput обозначает добавление выхода с новым снимком настроек.
	CommandAddOutput CommandKind = "add_output"
	// CommandStartOutput обозначает повторный запуск прикреплённого выхода по его снимку.
	CommandStartOutput CommandKind = "start_output"
	// CommandStopOutput обозначает остановку исполнения выхода с сохранением прикрепления.
	CommandStopOutput CommandKind = "stop_output"
	// CommandDetachOutput обозначает отсоединение выхода от сессии.
	CommandDetachOutput CommandKind = "detach_output"
)

// CommandStatus описывает долговечный этап обработки принятой команды.
type CommandStatus string

const (
	// CommandPending обозначает принятую команду до захвата исполнителем.
	CommandPending CommandStatus = "PENDING"
	// CommandClaimed обозначает команду, захваченную исполнителем.
	CommandClaimed CommandStatus = "CLAIMED"
	// CommandIssued обозначает выдачу внешней попытки команды.
	CommandIssued CommandStatus = "ISSUED"
	// CommandSucceeded обозначает успешное завершение команды.
	CommandSucceeded CommandStatus = "SUCCEEDED"
	// CommandFailed обозначает окончательный отказ команды.
	CommandFailed CommandStatus = "FAILED"
	// CommandInterrupted обозначает прерывание команды прежнего процесса.
	CommandInterrupted CommandStatus = "INTERRUPTED"
)

// Terminal распознаёт окончательные результаты команды SUCCEEDED, FAILED и INTERRUPTED.
func (s CommandStatus) Terminal() bool {
	return s == CommandSucceeded || s == CommandFailed || s == CommandInterrupted
}

// CommandReceipt сохраняет идентичность запроса, выданную попытку и окончательный результат команды.
type CommandReceipt struct {
	// Record содержит идентификатор, ревизию и временные метки сущности.
	Record
	// RequestID содержит клиентский идентификатор повторяемого запроса.
	RequestID string
	// Kind указывает принятую операцию управления.
	Kind CommandKind
	// TargetID идентифицирует исходную цель принятого запроса.
	TargetID ID
	// Identity сохраняет канонические байты команды для проверки тождественности повторов.
	Identity []byte
	// Fingerprint содержит отпечаток канонической идентичности команды.
	Fingerprint string
	// SessionID идентифицирует сессию, к которой относится запись.
	SessionID ID
	// OutputID идентифицирует отдельный выход; пустое значение относится ко всей сессии.
	OutputID ID
	// Incarnation идентифицирует открытие хранилища, которому принадлежит сохраняемая попытка.
	Incarnation string
	// Lifecycle содержит поколение долговечного жизненного цикла сессии.
	Lifecycle uint64
	// OutputGeneration содержит поколение исполнения выхода.
	OutputGeneration uint64
	// Attempt содержит номер конкретной выданной попытки исполнения.
	Attempt uint64
	// Status сохраняет текущий этап или окончательный результат команды.
	Status CommandStatus
	// Failure сохраняет причину неуспешного результата команды.
	Failure string
}

// ExecutionFence идентифицирует точную команду и попытку исполнения сессии или выхода.
type ExecutionFence struct {
	// CommandID идентифицирует команду, выдавшую попытку исполнения.
	CommandID ID
	// Incarnation идентифицирует открытие хранилища, которому принадлежит сохраняемая попытка.
	Incarnation string
	// SessionID идентифицирует сессию, к которой относится запись.
	SessionID ID
	// Lifecycle содержит поколение долговечного жизненного цикла сессии.
	Lifecycle uint64
	// OutputID идентифицирует отдельный выход; пустое значение относится ко всей сессии.
	OutputID ID
	// OutputGeneration содержит поколение исполнения выхода.
	OutputGeneration uint64
	// Attempt содержит номер конкретной выданной попытки исполнения.
	Attempt uint64
}

// ReferenceHold сохраняет зависимости точного исполнения, которые нельзя удалить до освобождения.
type ReferenceHold struct {
	// GenerationID идентифицирует удерживаемое поколение заглушки.
	GenerationID ID
	// SessionID указывает сессию-владельца удержания.
	SessionID ID
	// OutputID указывает выход-владелец удержания либо пустое значение для сессии.
	OutputID ID
	// StreamID идентифицирует удерживаемый поток.
	StreamID ID
	// GroupID идентифицирует удерживаемую группу.
	GroupID ID
	// ProfileID идентифицирует удерживаемый профиль.
	ProfileID ID
	// DestinationID идентифицирует удерживаемое назначение.
	DestinationID ID
	// AssetID идентифицирует удерживаемую заглушку.
	AssetID ID
	// VariantID идентифицирует удерживаемый подготовленный вариант.
	VariantID ID
}

// CleanupKind различает очистку исполнения и принадлежащего приложению файла.
type CleanupKind string

const (
	// CleanupRuntime обозначает очистку ресурсов точного исполнения.
	CleanupRuntime CleanupKind = "runtime"
	// CleanupFile обозначает физическое удаление принадлежащего приложению пути.
	CleanupFile CleanupKind = "file"
)

// CleanupIntent сохраняет точное намерение очистки ресурса до подтверждения её завершения.
type CleanupIntent struct {
	// ContentHash содержит хеш содержимого подготовленного файла.
	ContentHash string
	// SizeBytes содержит размер файла в байтах.
	SizeBytes uint64
	// ArtifactGenerationID идентифицирует поколение удаляемого файла.
	ArtifactGenerationID ID
	// FileID идентифицирует запись владения удаляемым путём.
	FileID ID
	// FileKind указывает назначение удаляемого пути в журнале файлов.
	FileKind OwnedFileKind
	// Record содержит идентификатор, ревизию и временные метки сущности.
	Record
	// Kind различает очистку исполнения и файла.
	Kind CleanupKind
	// StreamID указывает поток, к которому относится очистка.
	StreamID ID
	// SessionID идентифицирует сессию, к которой относится запись.
	SessionID ID
	// OutputID идентифицирует отдельный выход; пустое значение относится ко всей сессии.
	OutputID ID
	// Incarnation идентифицирует открытие хранилища, которому принадлежит сохраняемая попытка.
	Incarnation string
	// Lifecycle содержит поколение долговечного жизненного цикла сессии.
	Lifecycle uint64
	// Generation сохраняет поколение очищаемого выхода.
	Generation uint64
	// Attempt содержит номер конкретной выданной попытки исполнения.
	Attempt uint64
	// ManagedPath сохраняет относительный принадлежащий приложению путь.
	ManagedPath string
	// Acknowledged отмечает подтверждённое физическое завершение очистки.
	Acknowledged bool
}

// PreparedBinding указывает конкретный вариант и версию генератора для группы нового запуска.
type PreparedBinding struct {
	// GenerationID идентифицирует точное поколение заглушки.
	GenerationID ID
	// GroupID указывает группу, для которой выбран вариант.
	GroupID ID
	// VariantID указывает точный подготовленный вариант для группы.
	VariantID ID
	// GeneratorVersion фиксирует версию генератора подготовленного материала.
	GeneratorVersion string
}
