package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

func (r Record) Validate() error {
	if err := r.ID.Validate(); err != nil {
		return err
	}
	if r.Revision == 0 {
		return fmt.Errorf("%w: нулевая версия", ErrInvalid)
	}
	created, err := NormalizeTimestamp(r.CreatedAt)
	if err != nil {
		return err
	}
	updated, err := NormalizeTimestamp(r.UpdatedAt)
	if err != nil {
		return err
	}
	if r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || !created.Equal(r.CreatedAt) || !updated.Equal(r.UpdatedAt) || updated.Before(created) {
		return fmt.Errorf("%w: время записи", ErrInvalid)
	}
	return nil
}
func (s SessionState) Validate() error {
	if !s.Active() && !s.Terminal() {
		return fmt.Errorf("%w: состояние Session", ErrInvalid)
	}
	return nil
}
func (s OutputState) Validate() error {
	switch s {
	case OutputConnecting, OutputPublishing, OutputRetrying, OutputFailed, OutputStopped:
		return nil
	default:
		return fmt.Errorf("%w: состояние SessionOutput", ErrInvalid)
	}
}
func (s SessionSnapshot) Validate() error {
	if err := s.Stream.Validate(); err != nil {
		return err
	}
	if err := s.Stream.ID.Validate(); err != nil {
		return err
	}
	seen := make(map[ID]bool, len(s.Groups))
	for _, g := range s.Groups {
		if seen[g.Group.ID] || g.Group.StreamID != s.Stream.ID || g.Group.ProfileID != g.Profile.ID {
			return fmt.Errorf("%w: связи начального снимка", ErrInvalid)
		}
		seen[g.Group.ID] = true
		if err := g.Group.Validate(); err != nil {
			return err
		}
		if err := g.Profile.Validate(); err != nil {
			return err
		}
	}
	return nil
}
func (m ArtifactManifest) Validate() error {
	if err := m.VariantID.Validate(); err != nil {
		return err
	}
	if err := m.AssetID.Validate(); err != nil {
		return err
	}
	if err := m.Profile.Validate(); err != nil {
		return err
	}
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
	for _, field := range []struct{ name, value string }{{"SourceHash", m.SourceHash}, {"ContentHash", m.ContentHash}, {"GeneratorVersion", m.GeneratorVersion}} {
		if err := validText(field.name, field.value); err != nil {
			return err
		}
	}
	return ValidateManagedPath(m.ManagedPath)
}
func (s OutputSnapshot) Validate() error {
	if err := s.Destination.Validate(); err != nil {
		return err
	}
	if err := s.Group.Validate(); err != nil {
		return err
	}
	if err := s.Profile.Validate(); err != nil {
		return err
	}
	if err := s.Fallback.Validate(); err != nil {
		return err
	}
	if err := s.Artifact.Validate(); err != nil {
		return err
	}
	if s.Destination.GroupID != s.Group.ID || s.Destination.StreamID != s.Group.StreamID || s.Group.ProfileID != s.Profile.ID || s.Fallback.AssetID != s.Artifact.AssetID || !s.Profile.Profile.Equal(s.Artifact.Profile) {
		return fmt.Errorf("%w: связи снимка выхода", ErrInvalid)
	}
	return nil
}
func (s Session) Validate() error {
	if s.Failure != "" && !s.State.Terminal() {
		return ErrInvalid
	}
	if err := s.Record.Validate(); err != nil {
		return err
	}
	if err := s.StreamID.Validate(); err != nil {
		return err
	}
	if err := validText("Session.Incarnation", s.Incarnation); err != nil {
		return err
	}
	if err := s.State.Validate(); err != nil {
		return err
	}
	if s.Lifecycle == 0 || s.Snapshot.Stream.ID != s.StreamID {
		return fmt.Errorf("%w: поколение или Stream Session", ErrInvalid)
	}
	if s.EndedAt != nil {
		if _, err := NormalizeTimestamp(*s.EndedAt); err != nil {
			return err
		}
	}
	return s.Snapshot.Validate()
}
func (o SessionOutput) Validate() error {
	if err := o.Record.Validate(); err != nil {
		return err
	}
	if err := o.SessionID.Validate(); err != nil {
		return err
	}
	if o.Generation == 0 || (!o.Attached && o.DesiredRunning) {
		return fmt.Errorf("%w: состояние управления выходом", ErrInvalid)
	}
	return o.Snapshot.Validate()
}
func (c CommandReceipt) Validate() error {
	if err := c.Record.Validate(); err != nil {
		return err
	}
	if err := validText("CommandReceipt.RequestID", c.RequestID); err != nil {
		return err
	}
	if err := c.TargetID.Validate(); err != nil {
		return err
	}
	if err := c.SessionID.Validate(); err != nil {
		return err
	}
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
	sum := sha256.Sum256(c.Identity)
	if c.Fingerprint != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("%w: fingerprint команды", ErrInvalid)
	}
	if c.OutputID != "" {
		if err := c.OutputID.Validate(); err != nil {
			return err
		}
		if c.OutputGeneration == 0 {
			return fmt.Errorf("%w: поколение выхода команды", ErrInvalid)
		}
	}
	return nil
}
func (e Event) Validate() error {
	if e.Kind == "output_execution_failed" {
		if e.Execution == nil || e.OutputID == "" || e.Execution.SessionID != e.SessionID || e.Execution.OutputID != e.OutputID || e.Execution.Validate() != nil {
			return ErrInvalid
		}
	} else if e.Execution != nil {
		return ErrInvalid
	}
	if e.OutputID != "" {
		if err := e.OutputID.Validate(); err != nil {
			return err
		}
	}
	if err := e.ID.Validate(); err != nil {
		return err
	}
	if err := e.SessionID.Validate(); err != nil {
		return err
	}
	if err := validText("Event.Kind", e.Kind); err != nil {
		return err
	}
	if err := validText("Event.Message", e.Message); err != nil {
		return err
	}
	if e.CreatedAt.IsZero() {
		return ErrInvalid
	}
	_, err := NormalizeTimestamp(e.CreatedAt)
	return err
}
func (i CleanupIntent) Validate() error {
	if err := i.Record.Validate(); err != nil {
		return err
	}
	switch i.Kind {
	case CleanupFile:
		return ValidateManagedPath(i.ManagedPath)
	case CleanupRuntime:
		if err := i.StreamID.Validate(); err != nil {
			return err
		}
		if err := i.SessionID.Validate(); err != nil {
			return err
		}
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
func (h ReferenceHold) Validate() error {
	if err := h.SessionID.Validate(); err != nil {
		return err
	}
	if err := h.StreamID.Validate(); err != nil {
		return err
	}
	if err := h.AssetID.Validate(); err != nil {
		return err
	}
	if h.OutputID != "" {
		for _, id := range []ID{h.OutputID, h.GroupID, h.ProfileID, h.DestinationID, h.VariantID} {
			if err := id.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

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

func (f SourceFormat) Validate() error {
	if f.ImageFormat != "" {
		if err := validText("SourceFormat.ImageFormat", f.ImageFormat); err != nil {
			return err
		}
	}
	if f.Video != nil {
		if err := validText("SourceFormat.Video.Codec", string(f.Video.Codec)); err != nil {
			return err
		}
	}
	if f.Audio != nil {
		if err := validText("SourceFormat.Audio.Codec", string(f.Audio.Codec)); err != nil {
			return err
		}
	}
	return nil
}

func (f ExecutionFence) Validate() error {
	for _, id := range []ID{f.CommandID, f.SessionID} {
		if err := id.Validate(); err != nil {
			return err
		}
	}
	if err := validText("ExecutionFence.Incarnation", f.Incarnation); err != nil {
		return err
	}
	if f.Lifecycle == 0 || f.Attempt == 0 {
		return ErrInvalid
	}
	if f.OutputID != "" {
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
