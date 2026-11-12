package core

import (
	"context"
	"fmt"
	"time"
)

// Completion передаёт успешный либо неуспешный итог точной команды исполнения.
type Completion struct {
	// Success указывает, завершилось ли исполнение успешно.
	Success bool
	// Failure содержит обязательное объяснение неуспешного результата.
	Failure string
}

// CleanupFence фиксирует точную принадлежность очищаемого исполнения или файла.
type CleanupFence struct {
	// ContentHash сохраняет ожидаемый хеш удаляемого файла.
	ContentHash string
	// SizeBytes сохраняет ожидаемый размер удаляемого файла в байтах.
	SizeBytes uint64
	// ArtifactGenerationID идентифицирует поколение удаляемого файла.
	ArtifactGenerationID ID
	// FileID идентифицирует запись владения удаляемым путём.
	FileID ID
	// FileKind указывает назначение очищаемого пути в журнале файлов.
	FileKind OwnedFileKind
	// IntentID идентифицирует неизменяемое намерение очистки.
	IntentID ID
	// Incarnation идентифицирует открытие хранилища, которому принадлежит очищаемая попытка.
	Incarnation string
	// SessionID идентифицирует сессию очищаемого исполнения.
	SessionID ID
	// OutputID указывает очищаемый выход либо пустое значение для ресурсов сессии.
	OutputID ID
	// Lifecycle фиксирует поколение жизненного цикла очищаемой сессии.
	Lifecycle uint64
	// Generation фиксирует поколение очищаемого выхода.
	Generation uint64
	// Attempt фиксирует номер очищаемой выданной попытки.
	Attempt uint64
	// ManagedPath фиксирует относительный принадлежащий приложению путь для точного удаления.
	ManagedPath string
}

// CleanupFenceFor извлекает неизменяемую идентичность намерения очистки вместе с происхождением файла
// либо владельцем ресурсов исполнения.
func CleanupFenceFor(intent CleanupIntent) CleanupFence {
	return CleanupFence{ContentHash: intent.ContentHash, SizeBytes: intent.SizeBytes, ArtifactGenerationID: intent.ArtifactGenerationID, FileID: intent.FileID, FileKind: intent.FileKind, IntentID: intent.ID, Incarnation: intent.Incarnation, SessionID: intent.SessionID, OutputID: intent.OutputID, Lifecycle: intent.Lifecycle, Generation: intent.Generation, Attempt: intent.Attempt, ManagedPath: intent.ManagedPath}
}

// executionFence извлекает точную команду, incarnation, Lifecycle и поколение попытки выхода из
// квитанции.
func executionFence(c CommandReceipt) ExecutionFence {
	return ExecutionFence{CommandID: c.ID, Incarnation: c.Incarnation, SessionID: c.SessionID, Lifecycle: c.Lifecycle, OutputID: c.OutputID, OutputGeneration: c.OutputGeneration, Attempt: c.Attempt}
}

// fenced сверяет ExecutionFence с квитанцией текущей incarnation, Lifecycle сессии и поколением
// необязательного выхода; несовпадение возвращает ErrStale.
func (s *Service) fenced(ctx context.Context, r Repositories, f ExecutionFence) (CommandReceipt, Session, *SessionOutput, error) {
	// c содержит квитанцию указанной команды, а err — результат её чтения.
	c, err := r.Runtime.ReceiptByID(ctx, f.CommandID)
	if err != nil {
		return CommandReceipt{}, Session{}, nil, err
	}
	if executionFence(c) != f || f.Incarnation != s.incarnation {
		return CommandReceipt{}, Session{}, nil, ErrStale
	}
	// session содержит родительскую сессию для сверки incarnation и Lifecycle.
	session, err := r.Runtime.Session(ctx, c.SessionID)
	if err != nil {
		return CommandReceipt{}, Session{}, nil, err
	}
	if session.Incarnation != f.Incarnation || session.Lifecycle != f.Lifecycle {
		return CommandReceipt{}, Session{}, nil, ErrStale
	}
	if f.OutputID != "" {
		// o содержит необязательный выход точной попытки, а err — результат его чтения.
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

// Claim переводит текущую PENDING-команду в CLAIMED, назначая следующую попытку; запуск выхода
// допускается после RUNNING родителя и без ожидающей очистки.
func (s *Service) Claim(ctx context.Context, requestID string) (ExecutionFence, error) {
	// result принимает точную идентичность успешно захваченной попытки команды.
	var result ExecutionFence
	// err сообщает об ошибке транзакционного захвата команды.
	err := s.uow.Write(ctx, func(r Repositories) error {
		// В транзакции захватывает только актуальную PENDING-квитанцию, увеличивает Attempt и сохраняет
		// CLAIMED вместе с точным ExecutionFence.

		// c содержит захватываемую квитанцию, а err — результат её чтения.
		c, err := r.Runtime.Receipt(ctx, requestID)
		if err != nil {
			return err
		}
		if c.Status != CommandPending || c.Incarnation != s.incarnation {
			return ErrStale
		}
		// session содержит родительский запуск захватываемой команды.
		session, err := r.Runtime.Session(ctx, c.SessionID)
		if err != nil {
			return err
		}
		if session.Incarnation != s.incarnation || session.Lifecycle != c.Lifecycle {
			return ErrStale
		}
		// old сохраняет ожидаемую ревизию квитанции до перехода в CLAIMED.
		old := c.Revision
		if c.OutputID != "" {
			// o содержит выход захватываемой команды, а err — результат его чтения.
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
				// previous сохраняет ожидаемую ревизию выхода до увеличения номера
				// попытки.
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

// MarkIssued сохраняет переход точной CLAIMED-попытки в ISSUED перед внешними действиями; для запуска
// также отмечает выдачу Session или Output.
func (s *Service) MarkIssued(ctx context.Context, f ExecutionFence) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		// В транзакции сверяет ExecutionFence и CLAIMED, затем сохраняет ISSUED у квитанции и признак
		// возможного внешнего исполнения у Session либо выхода.

		// c содержит квитанцию, session — родительскую сессию, o — необязательный выход, а err —
		// результат сверки точной попытки перед выдачей.
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
		// old сохраняет ожидаемую ревизию квитанции перед выдачей; затем используется для
		// Session или Output.
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

// Complete сохраняет результат точной ISSUED-попытки; отказ допустим и из CLAIMED. Переходы сессии,
// отзыв неуспешного выхода, квитанция и событие записываются вместе; успех выхода не создаёт
// наблюдение PUBLISHING.
func (s *Service) Complete(ctx context.Context, f ExecutionFence, result Completion) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		// В транзакции принимает результат только актуальной попытки, обновляет жизненный цикл и квитанцию,
		// при отказе отзывает намерение и сохраняет необходимость очистки; успех выхода не создаёт наблюдаемое
		// PUBLISHING.

		// c содержит квитанцию, session — родительскую сессию, o — необязательный выход, а err —
		// результат сверки завершаемой попытки.
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
			// old сохраняет ожидаемую ревизию запуска перед завершением команды start.
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
				// now содержит время окончательного отказа запуска в UTC с
				// микросекундной точностью.
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
			// old сохраняет ожидаемую ревизию сессии перед завершением stop.
			old := session.Revision
			if result.Success {
				session.State = SessionStopped
			} else {
				session.State = SessionFailed
				session.Failure = result.Failure
			}
			// now содержит время завершения остановки сессии в UTC.
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
			// old сохраняет ожидаемую ревизию выхода перед фиксацией отказа запуска.
			old := o.Revision
			if !result.Success {
				if err = stopOutput(ctx, r, session, o, true); err != nil {
					return err
				}
				if err = cancelCommands(ctx, r, session.ID, o.ID, c.ID, false); err != nil {
					return err
				}
				if o.CleanupPending && !session.CleanupPending {
					// previous сохраняет ожидаемую ревизию родителя перед отметкой
					// незавершённой очистки.
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
		// old сохраняет ожидаемую ревизию завершаемой квитанции.
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

// releaseTerminalHolds освобождает все удержания сессии только при отсутствии неподтверждённых
// намерений очистки.
func releaseTerminalHolds(ctx context.Context, r Repositories, session Session) error {
	// pending содержит хотя бы одно неподтверждённое намерение очистки сессии, если оно есть, а err
	// — результат проверки.
	pending, err := r.Artifacts.Cleanups(ctx, ListQuery{Limit: 1, SessionID: session.ID, PendingOnly: true})
	if err != nil {
		return err
	}
	if len(pending) != 0 {
		return nil
	}
	return r.Runtime.ReleaseHolds(ctx, session.ID, "")
}

// AcknowledgeCleanup идемпотентно подтверждает точное намерение очистки, сверяет владельца и снимает
// соответствующие признаки и удержания в одной транзакции; физическое завершение должен доказать
// вызывающий.
func (s *Service) AcknowledgeCleanup(ctx context.Context, f CleanupFence) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		// В транзакции сверяет точное намерение очистки, записывает подтверждение и снимает соответствующие
		// признаки и удержания только при допустимом поколении; ошибки возвращаются владельцу транзакции.

		// intent содержит точное намерение очистки, а err — результат его чтения.
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
		// old сохраняет ожидаемую ревизию намерения очистки; затем используется для связанных
		// записей управления.
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

		// session содержит родительскую сессию очищаемого исполнения.
		session, err := r.Runtime.Session(ctx, intent.SessionID)
		if err != nil {
			return err
		}
		if intent.OutputID != "" {
			// o содержит очищаемый выход, а err — результат его чтения.
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
			// outputs содержит все выходы очищаемой сессии, а err — результат их чтения.
			outputs, err := allOutputs(ctx, r, session.ID)
			if err != nil {
				return err
			}
			// o содержит очередной выход для проверки освобождения его удержаний.
			for _, o := range outputs {
				if err = releaseOutputHolds(ctx, r, session, o); err != nil {
					return err
				}
			}
		}
		// pending содержит оставшееся неподтверждённое намерение очистки, если оно имеется.
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
		// В одной транзакции прерывает активные Session, подготовки и незавершённые команды прежних
		// владельцев, сохраняя очистку и удержания выданных исполнений без внешнего запуска.

		// err сообщает об ошибке прерывания подготовок прежних владельцев.
		if err := recoverPreparations(ctx, r, s.incarnation); err != nil {
			return err
		}
		// q задаёт постраничное чтение записей прежних incarnation для восстановления.
		q := ListQuery{Limit: MaxListLimit, ExcludeIncarnation: s.incarnation}
		for {
			// sessions содержит страницу прежних запусков, а err — результат её чтения.
			sessions, err := r.Runtime.Sessions(ctx, q)
			if err != nil {
				return err
			}
			// session содержит очередной прежний запуск, который при активном состоянии
			// прерывается.
			for _, session := range sessions {
				if !session.State.Active() {
					continue
				}
				// old сохраняет ожидаемую ревизию прерываемого прежнего запуска.
				old := session.Revision
				if err = stopSession(ctx, r, &session); err != nil {
					return err
				}
				session.State = SessionInterrupted
				session.Failure = "Потерян владелец исполнения"
				// now содержит время фиксации прерывания прежнего запуска.
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
			// last содержит последнюю сессию страницы для продолжения восстановления.
			last := sessions[len(sessions)-1]
			q.After = &ListBoundary{last.CreatedAt, last.ID}
		}
		q = ListQuery{Limit: MaxListLimit, ExcludeIncarnation: s.incarnation, PendingOnly: true}
		for {
			// commands содержит страницу незавершённых команд прежних владельцев, а err —
			// результат её чтения.
			commands, err := r.Runtime.Receipts(ctx, q)
			if err != nil {
				return err
			}
			// c содержит очередную незавершённую команду прежнего владельца для фиксации
			// прерывания.
			for _, c := range commands {
				// old сохраняет ожидаемую ревизию прерываемой квитанции.
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
			// last содержит последнюю квитанцию страницы для продолжения восстановления.
			last := commands[len(commands)-1]
			q.After = &ListBoundary{last.CreatedAt, last.ID}
		}
		return nil
	})
}

// settleSessionStops завершает остальные незавершённые команды остановки той же Session общим
// результатом, пропуская указанную квитанцию.
func settleSessionStops(ctx context.Context, r Repositories, sessionID, exclude ID, result Completion) error {
	// q задаёт постраничное чтение незавершённых команд одной сессии.
	q := ListQuery{Limit: MaxListLimit, SessionID: sessionID, PendingOnly: true}
	for {
		// commands содержит страницу команд для завершения соседних stop, а err — результат её
		// чтения.
		commands, err := r.Runtime.Receipts(ctx, q)
		if err != nil {
			return err
		}
		// c содержит очередную квитанцию для отбора остальных stop_session.
		for _, c := range commands {
			if c.ID == exclude || c.Kind != CommandStopSession {
				continue
			}
			// old сохраняет ожидаемую ревизию квитанции перед общим результатом остановки.
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
		// last содержит последнюю квитанцию страницы для продолжения обхода команд.
		last := commands[len(commands)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}
