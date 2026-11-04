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
	ErrInvalid               = errors.New("некорректное доменное значение")
	ErrNotFound              = errors.New("объект не найден")
	ErrConflict              = errors.New("конфликт состояния или версии")
	ErrInUse                 = errors.New("объект используется")
	ErrNotReady              = errors.New("объект не готов")
	ErrStale                 = errors.New("устаревшая попытка")
	ErrBusy                  = errors.New("хранилище занято")
	ErrCorrupt               = errors.New("повреждённые данные")
	ErrClosed                = errors.New("хранилище закрыто")
	ErrInvalidCursor         = errors.New("некорректный курсор")
	ErrInvalidMetadata       = errors.New("некорректные метаданные")
	ErrInvalidWatchdogPolicy = errors.New("некорректные интервалы контроля источника")
)

type ID string

type Record struct {
	ID        ID
	Revision  uint64
	CreatedAt time.Time
	UpdatedAt time.Time
}

func NextRevision(current uint64) (uint64, error) {
	if current == math.MaxUint64 {
		return 0, fmt.Errorf("%w: исчерпана версия", ErrConflict)
	}
	return current + 1, nil
}

func validText(name, value string) error {
	if strings.TrimSpace(value) == "" || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("%w: %s должен содержать допустимый текст", ErrInvalid, name)
	}
	return nil
}

func (id ID) Validate() error { return validText("ID", string(id)) }

// WatchdogPolicy задаёт интервалы контроля и восстановления обеих дорожек.
type WatchdogPolicy struct {
	VideoTimeout    time.Duration
	AudioTimeout    time.Duration
	RecoveryWindow  time.Duration
	KeyframeTimeout time.Duration
}

func (p WatchdogPolicy) Validate() error {
	for _, field := range []struct {
		name  string
		value time.Duration
	}{{"VideoTimeout", p.VideoTimeout}, {"AudioTimeout", p.AudioTimeout}, {"RecoveryWindow", p.RecoveryWindow}, {"KeyframeTimeout", p.KeyframeTimeout}} {
		if field.value <= 0 {
			return fmt.Errorf("%w: WatchdogPolicy.%s должен быть положительным", ErrInvalidWatchdogPolicy, field.name)
		}
	}
	return nil
}

type FallbackPolicy struct {
	AssetID  ID
	Watchdog WatchdogPolicy
}

func (p FallbackPolicy) Validate() error {
	if err := p.AssetID.Validate(); err != nil {
		return err
	}
	return p.Watchdog.Validate()
}

type Stream struct {
	Record
	Name           string
	PublicationKey string
	Fallback       FallbackPolicy
}

func (s Stream) Validate() error {
	if err := validText("Stream.Name", s.Name); err != nil {
		return err
	}
	if err := validText("Stream.PublicationKey", s.PublicationKey); err != nil {
		return err
	}
	if s.Fallback.AssetID == "" && s.Fallback.Watchdog == (WatchdogPolicy{}) {
		return nil
	}
	return s.Fallback.Validate()
}

type TranscodeProfile struct {
	Record
	Name    string
	Profile GroupProfile
}

func (p TranscodeProfile) Validate() error {
	if err := validText("TranscodeProfile.Name", p.Name); err != nil {
		return err
	}
	return p.Profile.Validate()
}

type TranscodeGroup struct {
	Record
	StreamID  ID
	ProfileID ID
	Name      string
}

func (g TranscodeGroup) Validate() error {
	if err := g.StreamID.Validate(); err != nil {
		return err
	}
	if err := g.ProfileID.Validate(); err != nil {
		return err
	}
	return validText("TranscodeGroup.Name", g.Name)
}

type Destination struct {
	Record
	StreamID  ID
	GroupID   ID
	Name      string
	Platform  string
	Endpoint  string
	StreamKey string
	Enabled   bool
}

func (d Destination) Validate() error {
	if err := d.StreamID.Validate(); err != nil {
		return err
	}
	if err := d.GroupID.Validate(); err != nil {
		return err
	}
	if err := validText("Destination.Name", d.Name); err != nil {
		return err
	}
	if err := validText("Destination.Platform", d.Platform); err != nil {
		return err
	}
	u, err := url.Parse(d.Endpoint)
	if err != nil || (u.Scheme != "rtmp" && u.Scheme != "rtmps") || u.Hostname() == "" || u.Fragment != "" || !utf8.ValidString(d.Endpoint) {
		return fmt.Errorf("%w: Destination.Endpoint должен быть RTMP или RTMPS URL", ErrInvalid)
	}
	if !utf8.ValidString(d.StreamKey) || strings.ContainsRune(d.StreamKey, 0) {
		return fmt.Errorf("%w: Destination.StreamKey", ErrInvalid)
	}
	return nil
}

func ValidateManagedPath(value string) error {
	if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\\\x00") || path.IsAbs(value) || path.Clean(value) != value || value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("%w: путь должен быть относительным и нормализованным", ErrInvalid)
	}
	return nil
}

type AssetKind string

const (
	AssetImage AssetKind = "image"
	AssetVideo AssetKind = "video"
)

type AssetState string

const (
	AssetPreparing AssetState = "PREPARING"
	AssetReady     AssetState = "READY"
	AssetFailed    AssetState = "FAILED"
)

type SourceFormat struct {
	Video       *VideoFormat
	Audio       *AudioFormat
	ImageFormat string
}
type FallbackAsset struct {
	Record
	CurrentGenerationID ID
	PendingGenerationID ID
	Incarnation         string
	Name                string
	Kind                AssetKind
	State               AssetState
	AttemptToken        string
	OriginalPath        string
	SourceHash          string
	SizeBytes           uint64
	SourceFormat        SourceFormat
}

func (a FallbackAsset) Validate() error {
	if err := validText("FallbackAsset.Name", a.Name); err != nil {
		return err
	}
	for _, id := range []ID{a.CurrentGenerationID, a.PendingGenerationID} {
		if id != "" {
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

type VariantState string

const (
	VariantPreparing VariantState = "PREPARING"
	VariantReady     VariantState = "READY"
	VariantFailed    VariantState = "FAILED"
)

type FallbackVariant struct {
	Record
	GenerationID     ID
	VerifiedFormat   MediaFormat
	Incarnation      string
	AssetID          ID
	SourceHash       string
	Profile          GroupProfile
	ProfileKey       string
	CanonicalProfile []byte
	GeneratorVersion string
	ManagedPath      string
	SizeBytes        uint64
	ContentHash      string
	State            VariantState
	AttemptToken     string
}

func (v FallbackVariant) Validate() error {
	if err := validText("FallbackVariant.Incarnation", v.Incarnation); err != nil {
		return err
	}
	if err := v.AssetID.Validate(); err != nil {
		return err
	}
	if v.GenerationID == "" {
		return ErrInvalid
	}
	if v.State == VariantReady && v.SourceHash == "" {
		return ErrInvalid
	}
	if err := v.Profile.Validate(); err != nil {
		return err
	}
	if err := validText("FallbackVariant.GeneratorVersion", v.GeneratorVersion); err != nil {
		return err
	}
	if err := ValidateManagedPath(v.ManagedPath); err != nil {
		return err
	}
	if err := validText("FallbackVariant.AttemptToken", v.AttemptToken); err != nil {
		return err
	}
	if v.State != VariantPreparing && v.State != VariantReady && v.State != VariantFailed {
		return fmt.Errorf("%w: состояние варианта", ErrInvalid)
	}
	key, canonical, err := ProfileIdentity(v.Profile)
	if err != nil {
		return err
	}
	if v.ProfileKey != key || !bytes.Equal(v.CanonicalProfile, canonical) {
		return fmt.Errorf("%w: идентичность профиля варианта", ErrInvalid)
	}
	if v.State == VariantReady {
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

type SessionState string

const (
	SessionStarting    SessionState = "STARTING"
	SessionRunning     SessionState = "RUNNING"
	SessionStopping    SessionState = "STOPPING"
	SessionStopped     SessionState = "STOPPED"
	SessionFailed      SessionState = "FAILED"
	SessionInterrupted SessionState = "INTERRUPTED"
)

func (s SessionState) Active() bool {
	return s == SessionStarting || s == SessionRunning || s == SessionStopping
}
func (s SessionState) Terminal() bool {
	return s == SessionStopped || s == SessionFailed || s == SessionInterrupted
}

type OutputState string

const (
	OutputConnecting OutputState = "CONNECTING"
	OutputPublishing OutputState = "PUBLISHING"
	OutputRetrying   OutputState = "RETRYING"
	OutputFailed     OutputState = "FAILED"
	OutputStopped    OutputState = "STOPPED"
)

type GroupSnapshot struct {
	Group   TranscodeGroup
	Profile TranscodeProfile
}
type SessionSnapshot struct {
	Stream Stream
	Groups []GroupSnapshot
}
type ArtifactManifest struct {
	GenerationID     ID
	VerifiedFormat   MediaFormat
	VariantID        ID
	AssetID          ID
	SourceHash       string
	Profile          GroupProfile
	ProfileKey       string
	CanonicalProfile []byte
	GeneratorVersion string
	ManagedPath      string
	SizeBytes        uint64
	ContentHash      string
}
type OutputSnapshot struct {
	Destination Destination
	Group       TranscodeGroup
	Profile     TranscodeProfile
	Fallback    FallbackPolicy
	Artifact    ArtifactManifest
}

type Session struct {
	Record
	Issued         bool
	StreamID       ID
	Incarnation    string
	State          SessionState
	DesiredPaused  bool
	Lifecycle      uint64
	Snapshot       SessionSnapshot
	CleanupPending bool
	Failure        string
	EndedAt        *time.Time
}

func (s Session) AllowsOBS(key string) bool {
	return key != "" && s.State == SessionRunning && key == s.Snapshot.Stream.PublicationKey
}

type SessionOutput struct {
	Record
	SessionID      ID
	Snapshot       OutputSnapshot
	DesiredRunning bool
	Attached       bool
	Generation     uint64
	Attempt        uint64
	Issued         bool
	CleanupPending bool
}

type Event struct {
	Execution *ExecutionFence `json:",omitempty"`
	ID        ID
	SessionID ID
	OutputID  ID
	Kind      string
	Message   string
	CreatedAt time.Time
}

type CommandKind string

const (
	CommandStartSession  CommandKind = "start_session"
	CommandStopSession   CommandKind = "stop_session"
	CommandPauseSession  CommandKind = "pause_session"
	CommandResumeSession CommandKind = "resume_session"
	CommandAddOutput     CommandKind = "add_output"
	CommandStartOutput   CommandKind = "start_output"
	CommandStopOutput    CommandKind = "stop_output"
	CommandDetachOutput  CommandKind = "detach_output"
)

type CommandStatus string

const (
	CommandPending     CommandStatus = "PENDING"
	CommandClaimed     CommandStatus = "CLAIMED"
	CommandIssued      CommandStatus = "ISSUED"
	CommandSucceeded   CommandStatus = "SUCCEEDED"
	CommandFailed      CommandStatus = "FAILED"
	CommandInterrupted CommandStatus = "INTERRUPTED"
)

func (s CommandStatus) Terminal() bool {
	return s == CommandSucceeded || s == CommandFailed || s == CommandInterrupted
}

type CommandReceipt struct {
	Record
	RequestID        string
	Kind             CommandKind
	TargetID         ID
	Identity         []byte
	Fingerprint      string
	SessionID        ID
	OutputID         ID
	Incarnation      string
	Lifecycle        uint64
	OutputGeneration uint64
	Attempt          uint64
	Status           CommandStatus
	Failure          string
}

type ExecutionFence struct {
	CommandID        ID
	Incarnation      string
	SessionID        ID
	Lifecycle        uint64
	OutputID         ID
	OutputGeneration uint64
	Attempt          uint64
}

type ReferenceHold struct {
	GenerationID  ID
	SessionID     ID
	OutputID      ID
	StreamID      ID
	GroupID       ID
	ProfileID     ID
	DestinationID ID
	AssetID       ID
	VariantID     ID
}

type CleanupKind string

const (
	CleanupRuntime CleanupKind = "runtime"
	CleanupFile    CleanupKind = "file"
)

type CleanupIntent struct {
	ContentHash          string
	SizeBytes            uint64
	ArtifactGenerationID ID
	FileID               ID
	FileKind             OwnedFileKind
	Record
	Kind         CleanupKind
	StreamID     ID
	SessionID    ID
	OutputID     ID
	Incarnation  string
	Lifecycle    uint64
	Generation   uint64
	Attempt      uint64
	ManagedPath  string
	Acknowledged bool
}

type PreparedBinding struct {
	GenerationID     ID
	GroupID          ID
	VariantID        ID
	GeneratorVersion string
}
