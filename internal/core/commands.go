package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// StartSessionRequest задаёт ручной запуск потока и точные варианты выбранных групп.
type StartSessionRequest struct {
	// RequestID идентифицирует повторяемый запрос управления.
	RequestID string
	// StreamID идентифицирует запускаемый поток.
	StreamID ID
	// DestinationIDs перечисляет выбранные назначения; пустой набор запуска выбирает включённые.
	DestinationIDs []ID
	// Bindings содержит точные подготовленные варианты по выбранным группам.
	Bindings []PreparedBinding
}

// SessionCommand задаёт повторяемую команду управления одной сессией.
type SessionCommand struct {
	// RequestID идентифицирует повторяемый запрос управления.
	RequestID string
	// SessionID идентифицирует управляемую родительскую сессию.
	SessionID ID
}

// AddOutputRequest задаёт добавление назначения с точной привязкой заглушки к сессии.
type AddOutputRequest struct {
	// RequestID идентифицирует повторяемый запрос управления.
	RequestID string
	// SessionID идентифицирует управляемую родительскую сессию.
	SessionID ID
	// DestinationID указывает добавляемое назначение.
	DestinationID ID
	// Binding фиксирует конкретный вариант заглушки добавляемого выхода.
	Binding PreparedBinding
}

// OutputCommand задаёт повторяемую команду управления одним выходом.
type OutputCommand struct {
	// RequestID идентифицирует повторяемый запрос управления.
	RequestID string
	// OutputID идентифицирует управляемый выход.
	OutputID ID
}

// requestIdentity задаёт каноническое содержимое команды для проверки тождественности RequestID.
type requestIdentity struct {
	// Version фиксирует версию канонического представления команды.
	Version int
	// Kind указывает вид принятой операции управления.
	Kind CommandKind
	// TargetID идентифицирует исходную цель запроса.
	TargetID ID
	// DestinationIDs перечисляет выбранные назначения; пустой набор запуска выбирает включённые.
	DestinationIDs []ID
}

// identity проверяет цель и уникальные назначения, сортирует копию списка и кодирует точную
// идентичность запроса; порядок входного списка на результат не влияет.
func identity(kind CommandKind, target ID, destinations []ID) ([]byte, error) {
	// err сообщает об ошибке проверки идентификатора цели команды.
	if err := target.Validate(); err != nil {
		return nil, err
	}
	// ids содержит собственную копию назначений для сортировки без изменения запроса.
	ids := append([]ID(nil), destinations...)
	slices.Sort(ids)
	// i указывает позицию назначения в отсортированном наборе, а id содержит его идентификатор для
	// проверки и обнаружения повторов.
	for i, id := range ids {
		// err сообщает об ошибке проверки очередного идентификатора назначения.
		if err := id.Validate(); err != nil {
			return nil, err
		}
		if i > 0 && ids[i-1] == id {
			return nil, fmt.Errorf("%w: повтор назначения", ErrInvalid)
		}
	}
	return json.Marshal(requestIdentity{MetadataVersion, kind, target, ids})
}

// priorReceipt возвращает прежнюю квитанцию только при совпадении SHA-256 и точных байтов запроса;
// повтор RequestID с другим содержимым даёт ErrConflict.
func priorReceipt(ctx context.Context, r Repositories, requestID string, content []byte) (CommandReceipt, bool, error) {
	// err сообщает об ошибке проверки непустого RequestID.
	if err := validText("RequestID", requestID); err != nil {
		return CommandReceipt{}, false, err
	}
	// prior содержит прежнюю квитанцию того же RequestID. err сообщает об ошибке чтения квитанции по
	// RequestID.
	prior, err := r.Runtime.Receipt(ctx, requestID)
	if err == nil {
		// sum содержит SHA-256 канонического запроса для сравнения или записи Fingerprint.
		sum := sha256.Sum256(content)
		if prior.Fingerprint != hex.EncodeToString(sum[:]) || !bytes.Equal(prior.Identity, content) {
			return CommandReceipt{}, false, ErrConflict
		}
		return prior, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return CommandReceipt{}, false, err
	}
	return CommandReceipt{}, false, nil
}

// receipt создаёт PENDING-квитанцию текущей incarnation с копией идентичности и поколениями сессии и
// необязательного выхода.
func (s *Service) receipt(kind CommandKind, target ID, requestID string, content []byte, session Session, output *SessionOutput) (CommandReceipt, error) {
	// record содержит новую идентичность и временные метки сохраняемой записи. err сообщает об
	// ошибке создания сохраняемой записи.
	record, err := newRecord()
	if err != nil {
		return CommandReceipt{}, err
	}
	// sum содержит SHA-256 канонического запроса для сравнения или записи Fingerprint.
	sum := sha256.Sum256(content)
	// c содержит квитанцию, создаваемую или изменяемую текущей операцией управления.
	c := CommandReceipt{Record: record, RequestID: requestID, Kind: kind, TargetID: target, Identity: bytes.Clone(content), Fingerprint: hex.EncodeToString(sum[:]), SessionID: session.ID, Incarnation: s.incarnation, Lifecycle: session.Lifecycle, Status: CommandPending}
	if output != nil {
		c.OutputID = output.ID
		c.OutputGeneration = output.Generation
	}
	return c, nil
}

// insertEvent сохраняет событие протокола управления с новым ID и текущим временем UTC до микросекунд.
func insertEvent(ctx context.Context, r Repositories, sessionID, outputID ID, kind, message string) error {
	// id содержит новый случайный идентификатор служебной записи. err сообщает об ошибке создания
	// случайного идентификатора.
	id, err := newID()
	if err != nil {
		return err
	}
	return r.Runtime.InsertEvent(ctx, Event{ID: id, SessionID: sessionID, OutputID: outputID, Kind: kind, Message: message, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)})
}

// accept сохраняет квитанцию и событие принятия команды в переданной транзакции.
func (s *Service) accept(ctx context.Context, r Repositories, c CommandReceipt) error {
	// err сообщает об ошибке сохранения квитанции принятой команды.
	if err := r.Runtime.InsertReceipt(ctx, c); err != nil {
		return err
	}
	return insertEvent(ctx, r, c.SessionID, c.OutputID, string(c.Kind), "Команда принята")
}

// allGroups последовательно читает все группы Stream страницами по границе времени создания и ID.
func allGroups(ctx context.Context, r Repositories, streamID ID) ([]TranscodeGroup, error) {
	// result накапливает все группы потока в порядке страниц.
	var result []TranscodeGroup
	// q задаёт фильтры и границу постраничного обхода сохраняемых записей.
	q := ListQuery{Limit: MaxListLimit, StreamID: streamID}
	for {
		// page содержит очередную страницу запрошенного сохраняемого списка. err сообщает об
		// ошибке чтения страницы групп потока.
		page, err := r.Config.Groups(ctx, q)
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		if len(page) < q.Limit {
			return result, nil
		}
		// last содержит последнюю запись страницы для продолжения обхода.
		last := page[len(page)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}

// allDestinations последовательно читает все назначения Stream, включая выключенные.
func allDestinations(ctx context.Context, r Repositories, streamID ID) ([]Destination, error) {
	// result накапливает все назначения потока, включая выключенные.
	var result []Destination
	// q задаёт фильтры и границу постраничного обхода сохраняемых записей.
	q := ListQuery{Limit: MaxListLimit, StreamID: streamID}
	for {
		// page содержит очередную страницу запрошенного сохраняемого списка. err сообщает об
		// ошибке чтения страницы назначений потока.
		page, err := r.Config.Destinations(ctx, q)
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		if len(page) < q.Limit {
			return result, nil
		}
		// last содержит последнюю запись страницы для продолжения обхода.
		last := page[len(page)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}

// allOutputs последовательно читает все сохраняемые выходы сессии, включая отсоединённые.
func allOutputs(ctx context.Context, r Repositories, sessionID ID) ([]SessionOutput, error) {
	// result накапливает все сохраняемые выходы сессии, включая отсоединённые.
	var result []SessionOutput
	// q задаёт фильтры и границу постраничного обхода сохраняемых записей.
	q := ListQuery{Limit: MaxListLimit, SessionID: sessionID}
	for {
		// page содержит очередную страницу запрошенного сохраняемого списка. err сообщает об
		// ошибке чтения страницы выходов сессии.
		page, err := r.Runtime.Outputs(ctx, q)
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		if len(page) < q.Limit {
			return result, nil
		}
		// last содержит последнюю запись страницы для продолжения обхода.
		last := page[len(page)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}

// bindingMap проверяет обязательные поля привязок и строит индекс по группе; две привязки одной группы
// отклоняются.
func bindingMap(bindings []PreparedBinding) (map[ID]PreparedBinding, error) {
	// result сопоставляет каждой группе единственную проверенную привязку варианта.
	result := make(map[ID]PreparedBinding, len(bindings))
	// b содержит очередную привязку варианта для проверки полноты и уникальности группы.
	for _, b := range bindings {
		if b.GenerationID == "" || b.GroupID == "" || b.VariantID == "" || b.GeneratorVersion == "" {
			return nil, ErrInvalid
		}
		// ok показывает наличие проверяемого ключа в наборе привязок или выбранных назначений.
		if _, ok := result[b.GroupID]; ok {
			return nil, ErrInvalid
		}
		result[b.GroupID] = b
	}
	return result, nil
}

// newOutput создаёт прикреплённый выход поколения 1 со снимком текущего назначения и точного готового
// артефакта политики Session; принадлежность Stream проверяется.
func newOutput(ctx context.Context, r Repositories, session Session, d Destination, binding PreparedBinding) (SessionOutput, error) {
	if d.StreamID != session.StreamID {
		return SessionOutput{}, ErrConflict
	}
	// g содержит группу назначения для проверки принадлежности потоку. err сообщает об ошибке чтения
	// группы назначения.
	g, err := r.Config.Group(ctx, d.GroupID)
	if err != nil {
		return SessionOutput{}, err
	}
	if g.StreamID != session.StreamID || binding.GroupID != g.ID {
		return SessionOutput{}, ErrConflict
	}
	// p содержит полный текущий профиль группы.
	p, err := r.Config.Profile(ctx, g.ProfileID)
	if err != nil {
		return SessionOutput{}, err
	}
	// manifest содержит проверенный manifest точного варианта по политике сессии.
	manifest, err := exactManifest(ctx, r, session.Snapshot.Stream.Fallback, p.Profile, binding)
	if err != nil {
		return SessionOutput{}, err
	}
	// record содержит новую идентичность и временные метки сохраняемой записи.
	record, err := newRecord()
	if err != nil {
		return SessionOutput{}, err
	}
	return SessionOutput{Record: record, SessionID: session.ID, Snapshot: OutputSnapshot{Destination: d, Group: g, Profile: p, Fallback: session.Snapshot.Stream.Fallback, Artifact: manifest}, DesiredRunning: true, Attached: true, Generation: 1}, nil
}

// insertOutput сохраняет выход и удержания его точных конфигурационных объектов и поколения в
// переданной транзакции.
func insertOutput(ctx context.Context, r Repositories, session Session, o SessionOutput) error {
	// err сообщает об ошибке сохранения нового выхода до вставки его удержаний.
	if err := r.Runtime.InsertOutput(ctx, o); err != nil {
		return err
	}
	return r.Runtime.InsertHold(ctx, ReferenceHold{SessionID: session.ID, OutputID: o.ID, StreamID: session.StreamID, GroupID: o.Snapshot.Group.ID, ProfileID: o.Snapshot.Profile.ID, DestinationID: o.Snapshot.Destination.ID, AssetID: o.Snapshot.Artifact.AssetID, VariantID: o.Snapshot.Artifact.VariantID, GenerationID: o.Snapshot.Artifact.GenerationID})
}

// initialOutputIntent создаёт отдельную внутреннюю команду запуска первоначального выхода с уникальным
// RequestID.
func (s *Service) initialOutputIntent(ctx context.Context, r Repositories, session Session, o SessionOutput) error {
	// id содержит новый случайный идентификатор служебной записи. err сообщает об ошибке создания
	// случайного идентификатора.
	id, err := newID()
	if err != nil {
		return err
	}
	// content содержит канонические байты запроса для проверки повторов.
	content, err := identity(CommandStartOutput, o.ID, nil)
	if err != nil {
		return err
	}
	// c содержит квитанцию, создаваемую или изменяемую текущей операцией управления.
	c, err := s.receipt(CommandStartOutput, o.ID, "internal:"+string(id), content, session, &o)
	if err != nil {
		return err
	}
	return s.accept(ctx, r, c)
}

// StartSession атомарно принимает ручной запуск: сохраняет STARTING-сессию, снимки, выбранные выходы,
// удержания и квитанции. Повтор RequestID сверяется до выбора текущих настроек; пустой список
// назначений выбирает включённые.
func (s *Service) StartSession(ctx context.Context, request StartSessionRequest) (CommandReceipt, error) {
	// content содержит канонические байты запроса для проверки повторов. err сообщает об ошибке
	// проверки целей и канонизации запроса.
	content, err := identity(CommandStartSession, request.StreamID, request.DestinationIDs)
	if err != nil {
		return CommandReceipt{}, err
	}
	// result принимает квитанцию принятой или повторённой команды.
	var result CommandReceipt
	err = s.uow.Write(ctx, func(r Repositories) error {
		// В одной транзакции сначала возвращает совпадающую прежнюю квитанцию, иначе проверяет отсутствие
		// активной подготовки и создаёт Session в состоянии STARTING, снимки выходов, удержания и намерения
		// запуска.

		// prior содержит прежнюю квитанцию того же RequestID. found показывает наличие
		// тождественной прежней квитанции. err сообщает об ошибке поиска и проверки прежней
		// квитанции.
		prior, found, err := priorReceipt(ctx, r, request.RequestID, content)
		if err != nil {
			return err
		}
		if found {
			result = prior
			return nil
		}
		if err = preparationIdleForRuntime(ctx, r, s.incarnation); err != nil {
			return err
		}
		// stream содержит текущие настройки запускаемого потока.
		stream, err := r.Config.Stream(ctx, request.StreamID)
		if err != nil {
			return err
		}
		if _, err = r.Runtime.ActiveSession(ctx, stream.ID); err == nil {
			return ErrInUse
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		if err = stream.Fallback.Validate(); err != nil {
			return fmt.Errorf("%w: заглушка не настроена", ErrNotReady)
		}
		// asset содержит выбранную заглушку и указатель опубликованного поколения.
		asset, err := r.Artifacts.Asset(ctx, stream.Fallback.AssetID)
		if err != nil {
			return err
		}
		if asset.CurrentGenerationID == "" {
			return ErrNotReady
		}
		// currentGeneration содержит текущее поколение заглушки для проверки готовности запуска.
		currentGeneration, err := r.Artifacts.Generation(ctx, asset.CurrentGenerationID)
		if err != nil {
			return err
		}
		if currentGeneration.AssetID != asset.ID || currentGeneration.State != PreparationPublished {
			return ErrNotReady
		}
		// groups содержит все группы потока, включаемые в начальный снимок.
		groups, err := allGroups(ctx, r, stream.ID)
		if err != nil {
			return err
		}
		// snapshots накапливает группы с полными снимками их профилей.
		snapshots := make([]GroupSnapshot, 0, len(groups))
		// g содержит группу назначения для проверки принадлежности потоку.
		for _, g := range groups {
			// p содержит полный текущий профиль группы. err сообщает об ошибке чтения
			// полного профиля группы.
			p, err := r.Config.Profile(ctx, g.ProfileID)
			if err != nil {
				return err
			}
			snapshots = append(snapshots, GroupSnapshot{g, p})
		}
		// record содержит новую идентичность и временные метки сохраняемой записи.
		record, err := newRecord()
		if err != nil {
			return err
		}
		// session содержит новый STARTING-запуск с самостоятельным снимком потока и групп.
		session := Session{Record: record, StreamID: stream.ID, Incarnation: s.incarnation, State: SessionStarting, Lifecycle: 1, Snapshot: SessionSnapshot{Stream: stream, Groups: snapshots}}
		if err = r.Runtime.InsertSession(ctx, session); err != nil {
			return err
		}
		if err = r.Runtime.InsertHold(ctx, ReferenceHold{SessionID: session.ID, StreamID: stream.ID, AssetID: asset.ID, GenerationID: asset.CurrentGenerationID}); err != nil {
			return err
		}
		// destinations содержит все текущие назначения потока для выбора начальных выходов.
		destinations, err := allDestinations(ctx, r, stream.ID)
		if err != nil {
			return err
		}
		// selected отмечает найденные явно запрошенные назначения.
		selected := make(map[ID]bool, len(request.DestinationIDs))
		// id содержит очередное явно запрошенное назначение.
		for _, id := range request.DestinationIDs {
			selected[id] = false
		}
		// bindings сопоставляет выбранным группам проверенные варианты заглушки.
		bindings, err := bindingMap(request.Bindings)
		if err != nil {
			return err
		}
		// d содержит текущее назначение, выбранное для проверки или создания выхода.
		for _, d := range destinations {
			if len(request.DestinationIDs) == 0 {
				if !d.Enabled {
					continue
				}
			} else {
				// ok показывает наличие проверяемого ключа в наборе привязок или
				// выбранных назначений.
				if _, ok := selected[d.ID]; !ok {
					continue
				}
				selected[d.ID] = true
			}
			// o содержит сохраняемый выход текущей операции управления. err сообщает об
			// ошибке создания нового снимка выхода.
			o, err := newOutput(ctx, r, session, d, bindings[d.GroupID])
			if err != nil {
				return err
			}
			if err = insertOutput(ctx, r, session, o); err != nil {
				return err
			}
			if err = s.initialOutputIntent(ctx, r, session, o); err != nil {
				return err
			}
		}
		// found показывает, найдено ли очередное явно запрошенное назначение.
		for _, found := range selected {
			if !found {
				return ErrNotFound
			}
		}
		result, err = s.receipt(CommandStartSession, stream.ID, request.RequestID, content, session, nil)
		if err != nil {
			return err
		}
		return s.accept(ctx, r, result)
	})
	return result, err
}

// PauseSession принимает сохраняемое намерение ручной паузы для RUNNING-сессии; выполнение
// медиапереключения подтверждается отдельно.
func (s *Service) PauseSession(ctx context.Context, request SessionCommand) (CommandReceipt, error) {
	return s.sessionCommand(ctx, request, CommandPauseSession)
}

// ResumeSession принимает сохраняемое намерение выхода из ручной паузы для RUNNING-сессии.
func (s *Service) ResumeSession(ctx context.Context, request SessionCommand) (CommandReceipt, error) {
	return s.sessionCommand(ctx, request, CommandResumeSession)
}

// StopSession принимает остановку сессии, отзывает прежние поколения и сохраняет необходимые намерения
// очистки; физического завершения этот вызов не подтверждает.
func (s *Service) StopSession(ctx context.Context, request SessionCommand) (CommandReceipt, error) {
	return s.sessionCommand(ctx, request, CommandStopSession)
}

// CompensateSessionExecution отзывает только точное ещё актуальное исполнение Session.
// Нулевая Attempt допускается лишь для ещё не захваченной команды запуска.
func (s *Service) CompensateSessionExecution(ctx context.Context, f ExecutionFence, requestID string) (CommandReceipt, bool, error) {
	return s.sessionCommandFenced(ctx, SessionCommand{RequestID: requestID, SessionID: f.SessionID}, CommandStopSession, &f)
}

// sessionCommand принимает управление сессией без ограничения конкретным ExecutionFence.
func (s *Service) sessionCommand(ctx context.Context, request SessionCommand, kind CommandKind) (CommandReceipt, error) {
	// result принимает квитанцию принятой или повторённой команды. err сообщает об ошибке принятия
	// управления сессией.
	result, _, err := s.sessionCommandFenced(ctx, request, kind, nil)
	return result, err
}

// sessionCommandFenced повторно проверяет RequestID и при заданном ExecutionFence принимает
// компенсацию только актуального запуска. Stop отзывает исполнения, а Pause/Resume заменяют прежнее
// намерение паузы; true возвращается только при успешной записи или повторе квитанции.
func (s *Service) sessionCommandFenced(ctx context.Context, request SessionCommand, kind CommandKind, fence *ExecutionFence) (CommandReceipt, bool, error) {
	// content содержит канонические байты запроса для проверки повторов. err сообщает об ошибке
	// проверки целей и канонизации запроса.
	content, err := identity(kind, request.SessionID, nil)
	if err != nil {
		return CommandReceipt{}, false, err
	}
	// result принимает квитанцию принятой или повторённой команды.
	var result CommandReceipt
	// accepted отмечает принятое изменение или тождественный повтор внутри успешной транзакции.
	accepted := false
	err = s.uow.Write(ctx, func(r Repositories) error {
		// В повторяемой транзакции сбрасывает захваченный результат, сверяет прежнюю квитанцию и
		// необязательный ExecutionFence, затем сохраняет остановку либо паузу Session вместе с квитанцией.

		result, accepted = CommandReceipt{}, false
		// prior содержит прежнюю квитанцию того же RequestID. found показывает наличие
		// тождественной прежней квитанции. err сообщает об ошибке поиска и проверки прежней
		// квитанции.
		prior, found, err := priorReceipt(ctx, r, request.RequestID, content)
		if err != nil {
			return err
		}
		if found {
			result = prior
			accepted = true
			return nil
		}
		if fence != nil {
			// validation содержит копию fence с положительной попыткой для структурной
			// проверки ещё не захваченного start.
			validation := *fence
			validation.Attempt = max(validation.Attempt, 1)
			if err = validation.Validate(); err != nil {
				return err
			}
			// c содержит квитанцию, создаваемую или изменяемую текущей операцией управления.
			// session содержит родительский запуск управляемого исполнения. err сообщает об
			// ошибке проверки точного сохраняемого исполнения.
			c, session, _, err := s.fenced(ctx, r, *fence)
			if errors.Is(err, ErrStale) || errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if fence.OutputID != "" || c.Kind != CommandStartSession ||
				(c.Status != CommandPending && c.Status != CommandClaimed && c.Status != CommandIssued && c.Status != CommandSucceeded) ||
				(c.Attempt == 0 && c.Status != CommandPending) ||
				(session.State != SessionStarting && session.State != SessionRunning) {
				return nil
			}
		}
		// session содержит родительский запуск управляемого исполнения.
		session, err := r.Runtime.Session(ctx, request.SessionID)
		if err != nil {
			return err
		}
		// terminal показывает, завершилась ли сессия до приёма команды остановки.
		terminal := session.State.Terminal()
		if kind == CommandStopSession {
			if !terminal && session.State != SessionStopping {
				// old сохраняет ожидаемую ревизию изменяемой записи управления.
				old := session.Revision
				if err = stopSession(ctx, r, &session); err != nil {
					return err
				}
				if err = cancelCommands(ctx, r, session.ID, "", "", false); err != nil {
					return err
				}
				session.Record, err = revise(session.Record)
				if err != nil {
					return err
				}
				if err = r.Runtime.UpdateSession(ctx, session, old); err != nil {
					return err
				}
			}
		} else {
			if session.State != SessionRunning {
				return ErrConflict
			}
			// old сохраняет ожидаемую ревизию изменяемой записи управления.
			old := session.Revision
			if err = cancelCommands(ctx, r, session.ID, "", "", true); err != nil {
				return err
			}
			session.DesiredPaused = kind == CommandPauseSession
			session.Record, err = revise(session.Record)
			if err != nil {
				return err
			}
			if err = r.Runtime.UpdateSession(ctx, session, old); err != nil {
				return err
			}
		}
		result, err = s.receipt(kind, session.ID, request.RequestID, content, session, nil)
		if err != nil {
			return err
		}
		if terminal {
			result.Status = CommandSucceeded
		}
		if err = s.accept(ctx, r, result); err != nil {
			return err
		}
		accepted = true
		return nil
	})
	return result, accepted && err == nil, err
}

// AddOutput принимает независимый выход RUNNING-сессии со свежим снимком назначения и точной привязкой
// заглушки; повтор RequestID возвращает прежнюю квитанцию.
func (s *Service) AddOutput(ctx context.Context, request AddOutputRequest) (CommandReceipt, error) {
	// content содержит канонические байты запроса для проверки повторов. err сообщает об ошибке
	// проверки целей и канонизации запроса.
	content, err := identity(CommandAddOutput, request.SessionID, []ID{request.DestinationID})
	if err != nil {
		return CommandReceipt{}, err
	}
	// result принимает квитанцию принятой или повторённой команды.
	var result CommandReceipt
	err = s.uow.Write(ctx, func(r Repositories) error {
		// В одной транзакции возвращает прежнюю квитанцию либо для Session в состоянии RUNNING создаёт выход
		// ещё не прикреплённого назначения с точным снимком, удержанием и квитанцией добавления.

		// prior содержит прежнюю квитанцию того же RequestID. found показывает наличие
		// тождественной прежней квитанции. err сообщает об ошибке поиска и проверки прежней
		// квитанции.
		prior, found, err := priorReceipt(ctx, r, request.RequestID, content)
		if err != nil {
			return err
		}
		if found {
			result = prior
			return nil
		}
		// session содержит родительский запуск управляемого исполнения.
		session, err := r.Runtime.Session(ctx, request.SessionID)
		if err != nil {
			return err
		}
		if session.State != SessionRunning {
			return ErrConflict
		}
		// d содержит текущее назначение, выбранное для проверки или создания выхода.
		d, err := r.Config.Destination(ctx, request.DestinationID)
		if err != nil {
			return err
		}
		// outputs содержит все сохраняемые выходы родительской сессии.
		outputs, err := allOutputs(ctx, r, session.ID)
		if err != nil {
			return err
		}
		// o содержит сохраняемый выход текущей операции управления.
		for _, o := range outputs {
			if o.Attached && o.Snapshot.Destination.ID == d.ID {
				return ErrInUse
			}
		}
		// o содержит новый выход со свежим снимком текущих настроек назначения.
		o, err := newOutput(ctx, r, session, d, request.Binding)
		if err != nil {
			return err
		}
		if err = insertOutput(ctx, r, session, o); err != nil {
			return err
		}
		result, err = s.receipt(CommandAddOutput, session.ID, request.RequestID, content, session, &o)
		if err != nil {
			return err
		}
		return s.accept(ctx, r, result)
	})
	return result, err
}

// StartOutput принимает повторный запуск прикреплённого остановленного выхода без незавершённой
// очистки, сохраняя его прежний снимок.
func (s *Service) StartOutput(ctx context.Context, request OutputCommand) (CommandReceipt, error) {
	return s.outputCommand(ctx, request, CommandStartOutput)
}

// StopOutput принимает остановку конкретного выхода и сохраняет очистку выданных либо предварительных
// ресурсов.
func (s *Service) StopOutput(ctx context.Context, request OutputCommand) (CommandReceipt, error) {
	return s.outputCommand(ctx, request, CommandStopOutput)
}

// DetachOutput принимает остановку и отсоединение выхода; удержания освобождаются только когда это
// допускает очистка выхода и родительской сессии.
func (s *Service) DetachOutput(ctx context.Context, request OutputCommand) (CommandReceipt, error) {
	return s.outputCommand(ctx, request, CommandDetachOutput)
}

// CompensateOutputStart останавливает только ещё актуальное исполнение запуска.
// true означает принятую компенсацию либо повтор её квитанции; отозванный fence даёт false.
func (s *Service) CompensateOutputStart(ctx context.Context, f ExecutionFence, requestID string) (CommandReceipt, bool, error) {
	return s.outputCommandFenced(ctx, OutputCommand{RequestID: requestID, OutputID: f.OutputID}, CommandStopOutput, &f)
}

// outputCommand принимает управление выходом без ограничения конкретным ExecutionFence.
func (s *Service) outputCommand(ctx context.Context, request OutputCommand, kind CommandKind) (CommandReceipt, error) {
	// result принимает квитанцию принятой или повторённой команды. err сообщает об ошибке принятия
	// управления выходом.
	result, _, err := s.outputCommandFenced(ctx, request, kind, nil)
	return result, err
}

// outputCommandFenced проверяет повтор RequestID и применимость управления; компенсация допускается
// только для точного актуального запуска. Изменение поколения и намерения, очистка и квитанция
// сохраняются в одной транзакции.
func (s *Service) outputCommandFenced(ctx context.Context, request OutputCommand, kind CommandKind, fence *ExecutionFence) (CommandReceipt, bool, error) {
	// content содержит канонические байты запроса для проверки повторов. err сообщает об ошибке
	// проверки целей и канонизации запроса.
	content, err := identity(kind, request.OutputID, nil)
	if err != nil {
		return CommandReceipt{}, false, err
	}
	// result принимает квитанцию принятой или повторённой команды.
	var result CommandReceipt
	// accepted отмечает принятое изменение или тождественный повтор внутри успешной транзакции.
	accepted := false
	err = s.uow.Write(ctx, func(r Repositories) error {
		// В повторяемой транзакции сбрасывает захваченный результат, проверяет необязательный ExecutionFence и
		// сохраняет запуск, остановку либо отсоединение выхода вместе с поколением, очисткой и квитанцией.

		result, accepted = CommandReceipt{}, false
		// prior содержит прежнюю квитанцию того же RequestID. found показывает наличие
		// тождественной прежней квитанции. err сообщает об ошибке поиска и проверки прежней
		// квитанции.
		prior, found, err := priorReceipt(ctx, r, request.RequestID, content)
		if err != nil {
			return err
		}
		if found {
			result = prior
			accepted = true
			return nil
		}
		if fence != nil {
			if err = fence.Validate(); err != nil {
				return err
			}
			// c содержит квитанцию, создаваемую или изменяемую текущей операцией управления.
			// session содержит родительский запуск управляемого исполнения. o содержит
			// сохраняемый выход текущей операции управления. err сообщает об ошибке проверки
			// точного сохраняемого исполнения.
			c, session, o, err := s.fenced(ctx, r, *fence)
			if errors.Is(err, ErrStale) || errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if (c.Kind != CommandStartOutput && c.Kind != CommandAddOutput) ||
				(c.Status != CommandClaimed && c.Status != CommandIssued && c.Status != CommandSucceeded) ||
				session.State != SessionRunning || o == nil || o.Attempt != fence.Attempt ||
				!o.Attached || !o.DesiredRunning || o.CleanupPending {
				return nil
			}
		}
		// o содержит сохраняемый выход текущей операции управления.
		o, err := r.Runtime.Output(ctx, request.OutputID)
		if err != nil {
			return err
		}
		// session содержит родительский запуск управляемого исполнения.
		session, err := r.Runtime.Session(ctx, o.SessionID)
		if err != nil {
			return err
		}
		// old сохраняет ожидаемую ревизию изменяемой записи управления.
		old := o.Revision
		if kind == CommandStartOutput {
			if session.State != SessionRunning || !o.Attached || o.DesiredRunning || o.CleanupPending {
				return ErrConflict
			}
			o.Generation, err = NextRevision(o.Generation)
			if err != nil {
				return err
			}
			o.DesiredRunning = true
		} else {
			// Новая команда не может управлять ресурсами прежнего владельца исполнения.
			if session.Incarnation != s.incarnation {
				return ErrStale
			}
			if !o.Attached && kind != CommandDetachOutput {
				return ErrConflict
			}
			if err = stopOutput(ctx, r, session, &o, true); err != nil {
				return err
			}
			if err = cancelCommands(ctx, r, session.ID, o.ID, "", false); err != nil {
				return err
			}
			if kind == CommandDetachOutput {
				o.Attached = false
				if err = releaseOutputHolds(ctx, r, session, o); err != nil {
					return err
				}
			}
		}
		o.Record, err = revise(o.Record)
		if err != nil {
			return err
		}
		if err = r.Runtime.UpdateOutput(ctx, o, old); err != nil {
			return err
		}
		if o.CleanupPending && !session.CleanupPending {
			// previous сохраняет ожидаемую ревизию родителя перед отметкой очистки.
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
		result, err = s.receipt(kind, o.ID, request.RequestID, content, session, &o)
		if err != nil {
			return err
		}
		if !o.CleanupPending && kind != CommandStartOutput {
			result.Status = CommandSucceeded
		}
		if err = s.accept(ctx, r, result); err != nil {
			return err
		}
		accepted = true
		return nil
	})
	return result, accepted && err == nil, err
}

// runtimeCleanup сохраняет намерение очистки точного владельца Session либо поколения и попытки
// Output.
func runtimeCleanup(ctx context.Context, r Repositories, session Session, o *SessionOutput) error {
	// record содержит новую идентичность и временные метки сохраняемой записи. err сообщает об
	// ошибке создания сохраняемой записи.
	record, err := newRecord()
	if err != nil {
		return err
	}
	// intent сохраняет обязанность очистить точного владельца сессии либо попытку выхода.
	intent := CleanupIntent{Record: record, Kind: CleanupRuntime, StreamID: session.StreamID, SessionID: session.ID, Incarnation: session.Incarnation, Lifecycle: session.Lifecycle}
	if o != nil {
		intent.OutputID = o.ID
		intent.Generation = o.Generation
		intent.Attempt = o.Attempt
	}
	return r.Artifacts.InsertCleanup(ctx, intent)
}

// stopOutput снимает намерение запуска и при необходимости создаёт очистку и продвигает поколение.
// includeProvisional учитывает предварительные ресурсы прикреплённого выхода уже выданной Session.
func stopOutput(ctx context.Context, r Repositories, session Session, o *SessionOutput, includeProvisional bool) error {
	// Выданный запуск Session мог создать ресурсы группы до запуска исходящего соединения.
	// При остановке всей Session их завершение покрывает отдельная очистка родителя.
	// needsCleanup показывает наличие выданных или предварительных ресурсов выхода, требующих
	// подтверждения очистки.
	needsCleanup := o.Issued || includeProvisional && o.Attached && o.DesiredRunning && session.Issued
	if o.DesiredRunning || (needsCleanup && !o.CleanupPending) {
		if needsCleanup && !o.CleanupPending {
			// err сообщает об ошибке сохранения намерения очистки выхода.
			if err := runtimeCleanup(ctx, r, session, o); err != nil {
				return err
			}
			o.CleanupPending = true
		}
		// next содержит следующее поколение выхода, отзывающее прежнее исполнение. err сообщает
		// об ошибке увеличения поколения без переполнения.
		next, err := NextRevision(o.Generation)
		if err != nil {
			return err
		}
		o.Generation = next
	}
	o.DesiredRunning = false
	return nil
}

// stopSession сохраняет обязательство очистки выданной Session, сбрасывает DesiredRunning её выходов,
// повышает Lifecycle и переводит Session в STOPPING. Удержания выхода освобождаются сразу лишь при
// отсутствии ожидающей очистки и если родительская Session не выдавалась.
func stopSession(ctx context.Context, r Repositories, session *Session) error {
	if session.Issued {
		// exists показывает наличие неподтверждённой очистки родительской сессии. err сообщает
		// об ошибке поиска неподтверждённой очистки сессии.
		exists, err := hasRuntimeCleanup(ctx, r, session.ID, "")
		if err != nil {
			return err
		}
		if !exists {
			if err = runtimeCleanup(ctx, r, *session, nil); err != nil {
				return err
			}
		}
		session.CleanupPending = true
	}
	// outputs содержит все сохраняемые выходы родительской сессии. err сообщает об ошибке чтения
	// всех выходов сессии.
	outputs, err := allOutputs(ctx, r, session.ID)
	if err != nil {
		return err
	}
	// o содержит сохраняемый выход текущей операции управления.
	for _, o := range outputs {
		// old сохраняет ожидаемую ревизию изменяемой записи управления.
		old := o.Revision
		if err = stopOutput(ctx, r, *session, &o, false); err != nil {
			return err
		}
		o.Record, err = revise(o.Record)
		if err != nil {
			return err
		}
		if err = r.Runtime.UpdateOutput(ctx, o, old); err != nil {
			return err
		}
		if o.CleanupPending {
			session.CleanupPending = true
		}
		if !o.CleanupPending && !session.Issued {
			if err = r.Runtime.ReleaseHolds(ctx, session.ID, o.ID); err != nil {
				return err
			}
		}
	}
	session.Lifecycle, err = NextRevision(session.Lifecycle)
	if err != nil {
		return err
	}
	session.State = SessionStopping
	return nil
}

// releaseOutputHolds освобождает удержания только без ожидающей очистки и после отсоединения либо
// остановки; выданная родительская Session продолжает удерживать ресурсы до своей очистки.
func releaseOutputHolds(ctx context.Context, r Repositories, session Session, o SessionOutput) error {
	// stopping показывает завершение или остановку родителя при проверке освобождения удержаний.
	stopping := session.State == SessionStopping || session.State.Terminal()
	if o.CleanupPending || o.Attached && !stopping {
		return nil
	}
	// Очистка исходящего соединения не подтверждает остановку ресурсов родительского запуска.
	if stopping && session.Issued {
		return nil
	}
	return r.Runtime.ReleaseHolds(ctx, session.ID, o.ID)
}

// cancelCommands прерывает незавершённые квитанции указанной сессии или выхода, пропуская exclude;
// pauseOnly ограничивает отмену командами Pause/Resume.
func cancelCommands(ctx context.Context, r Repositories, sessionID, outputID, exclude ID, pauseOnly bool) error {
	// q задаёт фильтры и границу постраничного обхода сохраняемых записей.
	q := ListQuery{Limit: MaxListLimit, SessionID: sessionID, OutputID: outputID, PendingOnly: true}
	for {
		// commands содержит страницу незавершённых квитанций управления. err сообщает об ошибке
		// чтения незавершённых квитанций.
		commands, err := r.Runtime.Receipts(ctx, q)
		if err != nil {
			return err
		}
		// c содержит квитанцию, создаваемую или изменяемую текущей операцией управления.
		for _, c := range commands {
			if c.ID == exclude {
				continue
			}
			if pauseOnly && c.Kind != CommandPauseSession && c.Kind != CommandResumeSession {
				continue
			}
			// old сохраняет ожидаемую ревизию изменяемой записи управления.
			old := c.Revision
			c.Status = CommandInterrupted
			c.Failure = "Команда отменена последующим управлением"
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
		// last содержит последнюю запись страницы для продолжения обхода.
		last := commands[len(commands)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}

// hasRuntimeCleanup постранично ищет неподтверждённую очистку ресурсов исполнения точно указанного
// выхода; пустой OutputID означает саму Session.
func hasRuntimeCleanup(ctx context.Context, r Repositories, sessionID, outputID ID) (bool, error) {
	// q задаёт фильтры и границу постраничного обхода сохраняемых записей.
	q := ListQuery{Limit: MaxListLimit, SessionID: sessionID, PendingOnly: true}
	for {
		// intents содержит страницу неподтверждённых намерений очистки сессии. err сообщает об
		// ошибке чтения намерений очистки.
		intents, err := r.Artifacts.Cleanups(ctx, q)
		if err != nil {
			return false, err
		}
		// intent сохраняет обязанность очистить точного владельца сессии либо попытку выхода.
		for _, intent := range intents {
			if intent.Kind == CleanupRuntime && intent.OutputID == outputID {
				return true, nil
			}
		}
		if len(intents) < q.Limit {
			return false, nil
		}
		// last содержит последнюю запись страницы для продолжения обхода.
		last := intents[len(intents)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}

// settleCleanupCommands завершает ожидающие Stop/Detach выхода после очистки прежнего поколения той же
// incarnation; очистка Session эти квитанции здесь не меняет.
func settleCleanupCommands(ctx context.Context, r Repositories, intent CleanupIntent) error {
	if intent.OutputID == "" {
		return nil
	}
	// q задаёт фильтры и границу постраничного обхода сохраняемых записей.
	q := ListQuery{Limit: MaxListLimit, SessionID: intent.SessionID, OutputID: intent.OutputID, PendingOnly: true}
	for {
		// commands содержит страницу незавершённых квитанций управления. err сообщает об ошибке
		// чтения незавершённых квитанций.
		commands, err := r.Runtime.Receipts(ctx, q)
		if err != nil {
			return err
		}
		// c содержит квитанцию, создаваемую или изменяемую текущей операцией управления.
		for _, c := range commands {
			if c.Kind != CommandStopOutput && c.Kind != CommandDetachOutput {
				continue
			}
			if c.Incarnation != intent.Incarnation || c.OutputGeneration <= intent.Generation {
				continue
			}
			// old сохраняет ожидаемую ревизию изменяемой записи управления.
			old := c.Revision
			c.Status = CommandSucceeded
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
		// last содержит последнюю запись страницы для продолжения обхода.
		last := commands[len(commands)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}
