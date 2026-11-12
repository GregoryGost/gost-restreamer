package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Validate проверяет ID, ненулевую ревизию и времена записи с точностью до микросекунд; время
// изменения не может предшествовать созданию.
func (r Record) Validate() error {
	// err сообщает об ошибке проверки идентификатора записи.
	if err := r.ID.Validate(); err != nil {
		return err
	}
	if r.Revision == 0 {
		return fmt.Errorf("%w: нулевая версия", ErrInvalid)
	}
	// created содержит нормализованное время создания, а err — результат проверки его
	// представимости.
	created, err := NormalizeTimestamp(r.CreatedAt)
	if err != nil {
		return err
	}
	// updated содержит нормализованное время изменения для проверки порядка и точности записи.
	updated, err := NormalizeTimestamp(r.UpdatedAt)
	if err != nil {
		return err
	}
	if r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || !created.Equal(r.CreatedAt) || !updated.Equal(r.UpdatedAt) || updated.Before(created) {
		return fmt.Errorf("%w: время записи", ErrInvalid)
	}
	return nil
}

// Validate разрешает только известные активные и терминальные состояния Session.
func (s SessionState) Validate() error {
	if !s.Active() && !s.Terminal() {
		return fmt.Errorf("%w: состояние Session", ErrInvalid)
	}
	return nil
}

// Validate разрешает только известные наблюдаемые состояния выхода.
func (s OutputState) Validate() error {
	switch s {
	case OutputConnecting, OutputPublishing, OutputRetrying, OutputFailed, OutputStopped:
		return nil
	default:
		return fmt.Errorf("%w: состояние SessionOutput", ErrInvalid)
	}
}

// Validate проверяет Stream и начальные группы, запрещает повтор группы и несогласованные ссылки на
// Stream и профиль.
func (s SessionSnapshot) Validate() error {
	// err сообщает об ошибке проверки настроек потока начального снимка.
	if err := s.Stream.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки идентификатора потока начального снимка.
	if err := s.Stream.ID.Validate(); err != nil {
		return err
	}
	// seen отмечает уже встреченные группы начального снимка для запрета дублей.
	seen := make(map[ID]bool, len(s.Groups))
	// g содержит очередную группу и её полный профиль для проверки согласованности ссылок.
	for _, g := range s.Groups {
		if seen[g.Group.ID] || g.Group.StreamID != s.Stream.ID || g.Group.ProfileID != g.Profile.ID {
			return fmt.Errorf("%w: связи начального снимка", ErrInvalid)
		}
		seen[g.Group.ID] = true
		// err сообщает об ошибке проверки настроек группы начального снимка.
		if err := g.Group.Validate(); err != nil {
			return err
		}
		// err сообщает об ошибке проверки полного профиля группы начального снимка.
		if err := g.Profile.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Validate сверяет идентификаторы, полный профиль и его канонические байты, совместимость проверенного
// формата, ненулевой размер, хеши, версию генератора и управляемый путь.
func (m ArtifactManifest) Validate() error {
	// err сообщает об ошибке проверки идентификатора поколения manifest.
	if err := m.GenerationID.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки фактического формата файла manifest.
	if err := m.VerifiedFormat.Validate(); err != nil {
		return err
	}
	if CompatibleFormat(m.VerifiedFormat, m.Profile.Format) != nil {
		return ErrInvalid
	}
	// err сообщает об ошибке проверки идентификатора варианта manifest.
	if err := m.VariantID.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки идентификатора заглушки manifest.
	if err := m.AssetID.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки полного профиля manifest.
	if err := m.Profile.Validate(); err != nil {
		return err
	}
	// key содержит ключ полного профиля, content — его канонические байты, а err — результат
	// вычисления идентичности.
	key, content, err := ProfileIdentity(m.Profile)
	if err != nil {
		return err
	}
	if key != m.ProfileKey || !bytes.Equal(content, m.CanonicalProfile) {
		return fmt.Errorf("%w: идентичность профиля manifest", ErrInvalid)
	}
	if m.SizeBytes == 0 {
		return fmt.Errorf("%w: размер manifest", ErrInvalid)
	}
	// field содержит очередной обязательный хеш или версию генератора вместе с именем поля.
	for _, field := range []struct { /* name указывает проверяемое поле manifest, а value содержит его обязательное текстовое значение. */
		name, value string
	}{{"SourceHash", m.SourceHash}, {"ContentHash", m.ContentHash}, {"GeneratorVersion", m.GeneratorVersion}} {
		// err сообщает об ошибке проверки текста хеша или версии генератора manifest.
		if err := validText(field.name, field.value); err != nil {
			return err
		}
	}
	return ValidateManagedPath(m.ManagedPath)
}

// Validate проверяет части снимка выхода и согласованность их ссылок, политики заглушки и полного
// профиля артефакта.
func (s OutputSnapshot) Validate() error {
	// err сообщает об ошибке проверки настроек назначения снимка выхода.
	if err := s.Destination.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки настроек группы снимка выхода.
	if err := s.Group.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки полного профиля снимка выхода.
	if err := s.Profile.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки политики заглушки снимка выхода.
	if err := s.Fallback.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки точного файла заглушки снимка выхода.
	if err := s.Artifact.Validate(); err != nil {
		return err
	}
	if s.Destination.GroupID != s.Group.ID || s.Destination.StreamID != s.Group.StreamID || s.Group.ProfileID != s.Profile.ID || s.Fallback.AssetID != s.Artifact.AssetID || !s.Profile.Profile.Equal(s.Artifact.Profile) {
		return fmt.Errorf("%w: связи снимка выхода", ErrInvalid)
	}
	return nil
}

// Validate проверяет запись и снимок Session, положительный Lifecycle и владельца; Failure разрешён
// только в терминальном состоянии.
func (s Session) Validate() error {
	if s.Failure != "" && !s.State.Terminal() {
		return ErrInvalid
	}
	// err сообщает об ошибке проверки общей записи сессии.
	if err := s.Record.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки идентификатора потока сессии.
	if err := s.StreamID.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки владельца сохраняемого выполнения сессии.
	if err := validText("Session.Incarnation", s.Incarnation); err != nil {
		return err
	}
	// err сообщает об ошибке проверки долговечного состояния сессии.
	if err := s.State.Validate(); err != nil {
		return err
	}
	if s.Lifecycle == 0 || s.Snapshot.Stream.ID != s.StreamID {
		return fmt.Errorf("%w: поколение или Stream Session", ErrInvalid)
	}
	if s.EndedAt != nil {
		// err сообщает об ошибке проверки представимости времени завершения сессии.
		if _, err := NormalizeTimestamp(*s.EndedAt); err != nil {
			return err
		}
	}
	return s.Snapshot.Validate()
}

// Validate проверяет запись и снимок выхода, положительное поколение и запрет DesiredRunning у
// отсоединённого выхода.
func (o SessionOutput) Validate() error {
	// err сообщает об ошибке проверки общей записи выхода.
	if err := o.Record.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки родительской сессии выхода.
	if err := o.SessionID.Validate(); err != nil {
		return err
	}
	if o.Generation == 0 || (!o.Attached && o.DesiredRunning) {
		return fmt.Errorf("%w: состояние управления выходом", ErrInvalid)
	}
	return o.Snapshot.Validate()
}

// Validate проверяет запись, вид и статус команды, идентификаторы и SHA-256 точных байтов Identity;
// выходная команда требует положительного поколения.
func (c CommandReceipt) Validate() error {
	// err сообщает об ошибке проверки общей записи квитанции.
	if err := c.Record.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки идентификатора повторяемого запроса квитанции.
	if err := validText("CommandReceipt.RequestID", c.RequestID); err != nil {
		return err
	}
	// err сообщает об ошибке проверки идентификатора цели команды.
	if err := c.TargetID.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки родительской сессии квитанции.
	if err := c.SessionID.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки владельца сохраняемой команды.
	if err := validText("CommandReceipt.Incarnation", c.Incarnation); err != nil {
		return err
	}
	if c.Lifecycle == 0 || len(c.Identity) == 0 {
		return fmt.Errorf("%w: идентичность команды", ErrInvalid)
	}
	switch c.Kind {
	case CommandStartSession, CommandStopSession, CommandPauseSession, CommandResumeSession, CommandAddOutput, CommandStartOutput, CommandStopOutput, CommandDetachOutput:
	default:
		return fmt.Errorf("%w: вид команды", ErrInvalid)
	}
	switch c.Status {
	case CommandPending, CommandClaimed, CommandIssued, CommandSucceeded, CommandFailed, CommandInterrupted:
	default:
		return fmt.Errorf("%w: состояние команды", ErrInvalid)
	}
	// sum содержит SHA-256 точных байтов команды для проверки Fingerprint.
	sum := sha256.Sum256(c.Identity)
	if c.Fingerprint != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("%w: fingerprint команды", ErrInvalid)
	}
	if c.OutputID != "" {
		// err сообщает об ошибке проверки идентификатора выхода команды.
		if err := c.OutputID.Validate(); err != nil {
			return err
		}
		if c.OutputGeneration == 0 {
			return fmt.Errorf("%w: поколение выхода команды", ErrInvalid)
		}
	}
	return nil
}

// Validate проверяет поля и время события; Execution допускается только у output_execution_failed и
// должен соответствовать SessionID и OutputID.
func (e Event) Validate() error {
	if e.Kind == "output_execution_failed" {
		if e.Execution == nil || e.OutputID == "" || e.Execution.SessionID != e.SessionID || e.Execution.OutputID != e.OutputID || e.Execution.Validate() != nil {
			return ErrInvalid
		}
	} else if e.Execution != nil {
		return ErrInvalid
	}
	if e.OutputID != "" {
		// err сообщает об ошибке проверки идентификатора выхода служебного события.
		if err := e.OutputID.Validate(); err != nil {
			return err
		}
	}
	// err сообщает об ошибке проверки идентификатора служебного события.
	if err := e.ID.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки родительской сессии события.
	if err := e.SessionID.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки вида служебного события.
	if err := validText("Event.Kind", e.Kind); err != nil {
		return err
	}
	// err сообщает об ошибке проверки объяснения служебного события.
	if err := validText("Event.Message", e.Message); err != nil {
		return err
	}
	if e.CreatedAt.IsZero() {
		return ErrInvalid
	}
	// err сообщает об ошибке проверки представимости времени служебного события.
	_, err := NormalizeTimestamp(e.CreatedAt)
	return err
}

// Validate проверяет запись и поля соответствующего вида очистки: происхождение файла либо точного
// владельца выполнения.
func (i CleanupIntent) Validate() error {
	// err сообщает об ошибке проверки общей записи намерения очистки.
	if err := i.Record.Validate(); err != nil {
		return err
	}
	switch i.Kind {
	case CleanupFile:
		if i.ArtifactGenerationID == "" || i.FileID == "" {
			return ErrInvalid
		}
		return (OwnedFile{AssetID: "asset", GenerationID: i.ArtifactGenerationID, ManagedPath: i.ManagedPath, Kind: i.FileKind, ContentHash: i.ContentHash, SizeBytes: i.SizeBytes}).Validate()
	case CleanupRuntime:
		// err сообщает об ошибке проверки потока очищаемого исполнения.
		if err := i.StreamID.Validate(); err != nil {
			return err
		}
		// err сообщает об ошибке проверки сессии очищаемого исполнения.
		if err := i.SessionID.Validate(); err != nil {
			return err
		}
		// err сообщает об ошибке проверки владельца очищаемого исполнения.
		if err := validText("CleanupIntent.Incarnation", i.Incarnation); err != nil {
			return err
		}
		if i.Lifecycle == 0 {
			return ErrInvalid
		}
		if i.OutputID != "" && i.Generation == 0 {
			return ErrInvalid
		}
		return nil
	default:
		return ErrInvalid
	}
}

// Validate проверяет обязательные ссылки удержания; удержание выхода дополнительно требует группу,
// профиль, назначение и вариант.
func (h ReferenceHold) Validate() error {
	// err сообщает об ошибке проверки удерживаемого поколения.
	if err := h.GenerationID.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки сессии-владельца удержания.
	if err := h.SessionID.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки удерживаемого потока.
	if err := h.StreamID.Validate(); err != nil {
		return err
	}
	// err сообщает об ошибке проверки удерживаемой заглушки.
	if err := h.AssetID.Validate(); err != nil {
		return err
	}
	if h.OutputID != "" {
		// id содержит очередную обязательную ссылку удержания отдельного выхода.
		for _, id := range []ID{h.OutputID, h.GroupID, h.ProfileID, h.DestinationID, h.VariantID} {
			// err сообщает об ошибке проверки очередной обязательной ссылки удержания
			// выхода.
			if err := id.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

// CanTransitionSession разрешает сохранение корректного состояния и допустимые переходы жизненного
// цикла; из терминального состояния нельзя перейти в другое.
func CanTransitionSession(from, to SessionState) bool {
	if from.Validate() != nil || to.Validate() != nil {
		return false
	}
	if from == to {
		return true
	}
	switch from {
	case SessionStarting:
		return to == SessionRunning || to == SessionStopping || to == SessionFailed || to == SessionInterrupted
	case SessionRunning:
		return to == SessionStopping || to == SessionFailed || to == SessionInterrupted
	case SessionStopping:
		return to == SessionStopped || to == SessionFailed || to == SessionInterrupted
	default:
		return false
	}
}

// CanTransitionOutput проверяет допустимые переходы наблюдаемого состояния, включая повторное
// подключение после FAILED или STOPPED.
func CanTransitionOutput(from, to OutputState) bool {
	if from.Validate() != nil || to.Validate() != nil {
		return false
	}
	if from == to {
		return true
	}
	switch from {
	case OutputConnecting:
		return to == OutputPublishing || to == OutputRetrying || to == OutputFailed || to == OutputStopped
	case OutputPublishing:
		return to == OutputRetrying || to == OutputFailed || to == OutputStopped
	case OutputRetrying:
		return to == OutputConnecting || to == OutputPublishing || to == OutputFailed || to == OutputStopped
	case OutputFailed, OutputStopped:
		return to == OutputConnecting || to == OutputStopped
	default:
		return false
	}
}

// Validate проверяет только допустимость присутствующего названия формата изображения и кодеков
// исходных дорожек; параметры выходного профиля здесь не требуются.
func (f SourceFormat) Validate() error {
	if f.ImageFormat != "" {
		// err сообщает об ошибке проверки названия формата исходного изображения.
		if err := validText("SourceFormat.ImageFormat", f.ImageFormat); err != nil {
			return err
		}
	}
	if f.Video != nil {
		// err сообщает об ошибке проверки кодека исходной видеодорожки.
		if err := validText("SourceFormat.Video.Codec", string(f.Video.Codec)); err != nil {
			return err
		}
	}
	if f.Audio != nil {
		// err сообщает об ошибке проверки кодека исходной аудиодорожки.
		if err := validText("SourceFormat.Audio.Codec", string(f.Audio.Codec)); err != nil {
			return err
		}
	}
	return nil
}

// Validate проверяет команду, сессию, incarnation и положительные Lifecycle/Attempt; OutputGeneration
// требуется ровно при наличии OutputID.
func (f ExecutionFence) Validate() error {
	// id содержит очередной обязательный идентификатор команды или сессии исполнения.
	for _, id := range []ID{f.CommandID, f.SessionID} {
		// err сообщает об ошибке проверки обязательного идентификатора точного исполнения.
		if err := id.Validate(); err != nil {
			return err
		}
	}
	// err сообщает об ошибке проверки владельца точного исполнения.
	if err := validText("ExecutionFence.Incarnation", f.Incarnation); err != nil {
		return err
	}
	if f.Lifecycle == 0 || f.Attempt == 0 {
		return ErrInvalid
	}
	if f.OutputID != "" {
		// err сообщает об ошибке проверки идентификатора выхода точного исполнения.
		if err := f.OutputID.Validate(); err != nil {
			return err
		}
		if f.OutputGeneration == 0 {
			return ErrInvalid
		}
	} else if f.OutputGeneration != 0 {
		return ErrInvalid
	}
	return nil
}
