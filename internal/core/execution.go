package core

import (
	"context"
	"fmt"
	"time"
)

type Completion struct {
	Success bool
	Failure string
}
type CleanupFence struct {
	ContentHash          string
	SizeBytes            uint64
	ArtifactGenerationID ID
	FileID               ID
	FileKind             OwnedFileKind
	IntentID             ID
	Incarnation          string
	SessionID            ID
	OutputID             ID
	Lifecycle            uint64
	Generation           uint64
	Attempt              uint64
	ManagedPath          string
}

func CleanupFenceFor(intent CleanupIntent) CleanupFence {
	return CleanupFence{ContentHash: intent.ContentHash, SizeBytes: intent.SizeBytes, ArtifactGenerationID: intent.ArtifactGenerationID, FileID: intent.FileID, FileKind: intent.FileKind, IntentID: intent.ID, Incarnation: intent.Incarnation, SessionID: intent.SessionID, OutputID: intent.OutputID, Lifecycle: intent.Lifecycle, Generation: intent.Generation, Attempt: intent.Attempt, ManagedPath: intent.ManagedPath}
}
func executionFence(c CommandReceipt) ExecutionFence {
	return ExecutionFence{CommandID: c.ID, Incarnation: c.Incarnation, SessionID: c.SessionID, Lifecycle: c.Lifecycle, OutputID: c.OutputID, OutputGeneration: c.OutputGeneration, Attempt: c.Attempt}
}
func (s *Service) fenced(ctx context.Context, r Repositories, f ExecutionFence) (CommandReceipt, Session, *SessionOutput, error) {
	c, err := r.Runtime.ReceiptByID(ctx, f.CommandID)
	if err != nil {
		return CommandReceipt{}, Session{}, nil, err
	}
	if executionFence(c) != f || f.Incarnation != s.incarnation {
		return CommandReceipt{}, Session{}, nil, ErrStale
	}
	session, err := r.Runtime.Session(ctx, c.SessionID)
	if err != nil {
		return CommandReceipt{}, Session{}, nil, err
	}
	if session.Incarnation != f.Incarnation || session.Lifecycle != f.Lifecycle {
		return CommandReceipt{}, Session{}, nil, ErrStale
	}
	if f.OutputID != "" {
		o, err := r.Runtime.Output(ctx, f.OutputID)
		if err != nil {
			return CommandReceipt{}, Session{}, nil, err
		}
		if o.SessionID != session.ID || o.Generation != f.OutputGeneration {
			return CommandReceipt{}, Session{}, nil, ErrStale
		}
		return c, session, &o, nil
	}
	return c, session, nil, nil
}

func (s *Service) Claim(ctx context.Context, requestID string) (ExecutionFence, error) {
	var result ExecutionFence
	err := s.uow.Write(ctx, func(r Repositories) error {
		c, err := r.Runtime.Receipt(ctx, requestID)
		if err != nil {
			return err
		}
		if c.Status != CommandPending || c.Incarnation != s.incarnation {
			return ErrStale
		}
		session, err := r.Runtime.Session(ctx, c.SessionID)
		if err != nil {
			return err
		}
		if session.Incarnation != s.incarnation || session.Lifecycle != c.Lifecycle {
			return ErrStale
		}
		old := c.Revision
		if c.OutputID != "" {
			o, err := r.Runtime.Output(ctx, c.OutputID)
			if err != nil {
				return err
			}
			if o.Generation != c.OutputGeneration {
				return ErrStale
			}
			if c.Kind == CommandStartOutput || c.Kind == CommandAddOutput {
				if session.State != SessionRunning {
					return ErrNotReady
				}
				if !o.Attached || !o.DesiredRunning || o.CleanupPending {
					return ErrStale
				}
				o.Attempt, err = NextRevision(o.Attempt)
				if err != nil {
					return err
				}
				c.Attempt = o.Attempt
				previous := o.Revision
				o.Record, err = revise(o.Record)
				if err != nil {
					return err
				}
				if err = r.Runtime.UpdateOutput(ctx, o, previous); err != nil {
					return err
				}
			} else {
				c.Attempt, err = NextRevision(c.Attempt)
				if err != nil {
					return err
				}
			}
		} else {
			if c.Kind == CommandStartSession && session.State != SessionStarting {
				return ErrStale
			}
			c.Attempt, err = NextRevision(c.Attempt)
			if err != nil {
				return err
			}
		}
		if c.Kind == CommandStartSession || c.Kind == CommandStartOutput || c.Kind == CommandAddOutput {
			if err = preparationIdleForRuntime(ctx, r, s.incarnation); err != nil {
				return err
			}
		}
		c.Status = CommandClaimed
		c.Record, err = revise(c.Record)
		if err != nil {
			return err
		}
		if err = r.Runtime.UpdateReceipt(ctx, c, old); err != nil {
			return err
		}
		result = executionFence(c)
		return nil
	})
	return result, err
}

func (s *Service) MarkIssued(ctx context.Context, f ExecutionFence) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		c, session, o, err := s.fenced(ctx, r, f)
		if err != nil {
			return err
		}
		if c.Kind == CommandStartSession || c.Kind == CommandStartOutput || c.Kind == CommandAddOutput {
			if err = preparationIdleForRuntime(ctx, r, s.incarnation); err != nil {
				return err
			}
		}
		if c.Status != CommandClaimed {
			return ErrStale
		}
		old := c.Revision
		c.Status = CommandIssued
		c.Record, err = revise(c.Record)
		if err != nil {
			return err
		}
		if err = r.Runtime.UpdateReceipt(ctx, c, old); err != nil {
			return err
		}
		if c.Kind == CommandStartSession {
			if session.State != SessionStarting {
				return ErrStale
			}
			old = session.Revision
			session.Issued = true
			session.Record, err = revise(session.Record)
			if err != nil {
				return err
			}
			return r.Runtime.UpdateSession(ctx, session, old)
		}
		if c.Kind == CommandStartOutput || c.Kind == CommandAddOutput {
			if o == nil || !o.DesiredRunning || !o.Attached || o.Attempt != f.Attempt || o.CleanupPending || session.State != SessionRunning {
				return ErrStale
			}
			old = o.Revision
			o.Issued = true
			o.Record, err = revise(o.Record)
			if err != nil {
				return err
			}
			return r.Runtime.UpdateOutput(ctx, *o, old)
		}
		return nil
	})
}

func (s *Service) Complete(ctx context.Context, f ExecutionFence, result Completion) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		c, session, o, err := s.fenced(ctx, r, f)
		if err != nil {
			return err
		}
		if c.Status != CommandIssued && (c.Status != CommandClaimed || result.Success) {
			return ErrStale
		}
		if !result.Success && result.Failure == "" {
			return fmt.Errorf("%w: требуется причина отказа", ErrInvalid)
		}
		switch c.Kind {
		case CommandStartSession:
			if session.State != SessionStarting {
				return ErrStale
			}
			old := session.Revision
			if result.Success {
				session.State = SessionRunning
			} else {
				if err = stopSession(ctx, r, &session); err != nil {
					return err
				}
				if err = cancelCommands(ctx, r, session.ID, "", c.ID, false); err != nil {
					return err
				}
				session.State = SessionFailed
				session.Failure = result.Failure
				now := time.Now().UTC().Truncate(time.Microsecond)
				session.EndedAt = &now
			}
			session.Record, err = revise(session.Record)
			if err != nil {
				return err
			}
			if err = r.Runtime.UpdateSession(ctx, session, old); err != nil {
				return err
			}
		case CommandStopSession:
			if session.State != SessionStopping {
				return ErrStale
			}
			old := session.Revision
			if result.Success {
				session.State = SessionStopped
			} else {
				session.State = SessionFailed
				session.Failure = result.Failure
			}
			now := time.Now().UTC().Truncate(time.Microsecond)
			session.EndedAt = &now
			session.Record, err = revise(session.Record)
			if err != nil {
				return err
			}
			if err = r.Runtime.UpdateSession(ctx, session, old); err != nil {
				return err
			}
			if err = settleSessionStops(ctx, r, session.ID, c.ID, result); err != nil {
				return err
			}
		case CommandStartOutput, CommandAddOutput:
			if o == nil || o.Attempt != f.Attempt || !o.DesiredRunning || !o.Attached || session.State != SessionRunning {
				return ErrStale
			}
			old := o.Revision
			if !result.Success {
				if err = stopOutput(ctx, r, session, o, true); err != nil {
					return err
				}
				if err = cancelCommands(ctx, r, session.ID, o.ID, c.ID, false); err != nil {
					return err
				}
				if o.CleanupPending && !session.CleanupPending {
					previous := session.Revision
					session.CleanupPending = true
					session.Record, err = revise(session.Record)
					if err != nil {
						return err
					}
					if err = r.Runtime.UpdateSession(ctx, session, previous); err != nil {
						return err
					}
				}
				o.Record, err = revise(o.Record)
				if err != nil {
					return err
				}
				if err = r.Runtime.UpdateOutput(ctx, *o, old); err != nil {
					return err
				}
			}
		case CommandStopOutput, CommandDetachOutput:
			if o == nil || o.DesiredRunning {
				return ErrStale
			}
		case CommandPauseSession, CommandResumeSession:
			if session.State != SessionRunning {
				return ErrStale
			}
		default:
			return ErrInvalid
		}
		old := c.Revision
		if result.Success {
			c.Status = CommandSucceeded
		} else {
			c.Status = CommandFailed
			c.Failure = result.Failure
		}
		c.Record, err = revise(c.Record)
		if err != nil {
			return err
		}
		if err = r.Runtime.UpdateReceipt(ctx, c, old); err != nil {
			return err
		}
		if err = insertEvent(ctx, r, session.ID, c.OutputID, "command_completed", "Получен результат исполнения команды"); err != nil {
			return err
		}
		if session.State.Terminal() {
			return releaseTerminalHolds(ctx, r, session)
		}
		return nil
	})
}

func releaseTerminalHolds(ctx context.Context, r Repositories, session Session) error {
	pending, err := r.Artifacts.Cleanups(ctx, ListQuery{Limit: 1, SessionID: session.ID, PendingOnly: true})
	if err != nil {
		return err
	}
	if len(pending) != 0 {
		return nil
	}
	return r.Runtime.ReleaseHolds(ctx, session.ID, "")
}

func (s *Service) AcknowledgeCleanup(ctx context.Context, f CleanupFence) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		intent, err := r.Artifacts.Cleanup(ctx, f.IntentID)
		if err != nil {
			return err
		}
		if CleanupFenceFor(intent) != f {
			return ErrStale
		}
		if intent.Acknowledged {
			return nil
		}
		old := intent.Revision
		intent.Acknowledged = true
		intent.Record, err = revise(intent.Record)
		if err != nil {
			return err
		}
		if err = r.Artifacts.UpdateCleanup(ctx, intent, old); err != nil {
			return err
		}
		if intent.Kind == CleanupFile {
			_, err = authorizeFileCleanup(ctx, r, intent)
			return err
		}

		session, err := r.Runtime.Session(ctx, intent.SessionID)
		if err != nil {
			return err
		}
		if intent.OutputID != "" {
			o, err := r.Runtime.Output(ctx, intent.OutputID)
			if err != nil {
				return err
			}
			if !o.CleanupPending || o.SessionID != session.ID || session.Incarnation != intent.Incarnation || session.Lifecycle < intent.Lifecycle || o.Attempt != intent.Attempt || o.Generation <= intent.Generation || o.DesiredRunning {
				return ErrStale
			}
			old = o.Revision
			o.CleanupPending = false
			o.Issued = false
			o.Record, err = revise(o.Record)
			if err != nil {
				return err
			}
			if err = r.Runtime.UpdateOutput(ctx, o, old); err != nil {
				return err
			}
			if err = releaseOutputHolds(ctx, r, session, o); err != nil {
				return err
			}
		} else {
			if !session.CleanupPending || session.Incarnation != intent.Incarnation || session.Lifecycle <= intent.Lifecycle {
				return ErrStale
			}
			session.Issued = false
			outputs, err := allOutputs(ctx, r, session.ID)
			if err != nil {
				return err
			}
			for _, o := range outputs {
				if err = releaseOutputHolds(ctx, r, session, o); err != nil {
					return err
				}
			}
		}
		pending, err := r.Artifacts.Cleanups(ctx, ListQuery{Limit: 1, SessionID: session.ID, PendingOnly: true})
		if err != nil {
			return err
		}
		old = session.Revision
		session.CleanupPending = len(pending) != 0
		session.Record, err = revise(session.Record)
		if err != nil {
			return err
		}
		if err = r.Runtime.UpdateSession(ctx, session, old); err != nil {
			return err
		}
		if err = settleCleanupCommands(ctx, r, intent); err != nil {
			return err
		}
		if session.State.Terminal() {
			return releaseTerminalHolds(ctx, r, session)
		}
		return nil
	})
}

// Recover прерывает только работу прежних владельцев. Ни одна сохранённая
// команда не исполняется автоматически и подтверждение внешней остановки не выдумывается.
func (s *Service) Recover(ctx context.Context) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		if err := recoverPreparations(ctx, r, s.incarnation); err != nil {
			return err
		}
		q := ListQuery{Limit: MaxListLimit, ExcludeIncarnation: s.incarnation}
		for {
			sessions, err := r.Runtime.Sessions(ctx, q)
			if err != nil {
				return err
			}
			for _, session := range sessions {
				if !session.State.Active() {
					continue
				}
				old := session.Revision
				if err = stopSession(ctx, r, &session); err != nil {
					return err
				}
				session.State = SessionInterrupted
				session.Failure = "Потерян владелец исполнения"
				now := time.Now().UTC().Truncate(time.Microsecond)
				session.EndedAt = &now
				session.Record, err = revise(session.Record)
				if err != nil {
					return err
				}
				if err = r.Runtime.UpdateSession(ctx, session, old); err != nil {
					return err
				}
				if err = releaseTerminalHolds(ctx, r, session); err != nil {
					return err
				}
				if err = insertEvent(ctx, r, session.ID, "", "session_interrupted", session.Failure); err != nil {
					return err
				}
			}
			if len(sessions) < q.Limit {
				break
			}
			last := sessions[len(sessions)-1]
			q.After = &ListBoundary{last.CreatedAt, last.ID}
		}
		q = ListQuery{Limit: MaxListLimit, ExcludeIncarnation: s.incarnation, PendingOnly: true}
		for {
			commands, err := r.Runtime.Receipts(ctx, q)
			if err != nil {
				return err
			}
			for _, c := range commands {
				old := c.Revision
				c.Status = CommandInterrupted
				c.Failure = "Потерян владелец исполнения"
				c.Record, err = revise(c.Record)
				if err != nil {
					return err
				}
				if err = r.Runtime.UpdateReceipt(ctx, c, old); err != nil {
					return err
				}
			}
			if len(commands) < q.Limit {
				break
			}
			last := commands[len(commands)-1]
			q.After = &ListBoundary{last.CreatedAt, last.ID}
		}
		return nil
	})
}

func settleSessionStops(ctx context.Context, r Repositories, sessionID, exclude ID, result Completion) error {
	q := ListQuery{Limit: MaxListLimit, SessionID: sessionID, PendingOnly: true}
	for {
		commands, err := r.Runtime.Receipts(ctx, q)
		if err != nil {
			return err
		}
		for _, c := range commands {
			if c.ID == exclude || c.Kind != CommandStopSession {
				continue
			}
			old := c.Revision
			if result.Success {
				c.Status = CommandSucceeded
			} else {
				c.Status = CommandFailed
				c.Failure = result.Failure
			}
			c.Record, err = revise(c.Record)
			if err != nil {
				return err
			}
			if err = r.Runtime.UpdateReceipt(ctx, c, old); err != nil {
				return err
			}
		}
		if len(commands) < q.Limit {
			return nil
		}
		last := commands[len(commands)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}
