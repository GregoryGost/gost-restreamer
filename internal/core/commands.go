package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

type StartSessionRequest struct {
	RequestID      string
	StreamID       ID
	DestinationIDs []ID
	Bindings       []PreparedBinding
}
type SessionCommand struct {
	RequestID string
	SessionID ID
}
type AddOutputRequest struct {
	RequestID     string
	SessionID     ID
	DestinationID ID
	Binding       PreparedBinding
}
type OutputCommand struct {
	RequestID string
	OutputID  ID
}

type requestIdentity struct {
	Version        int
	Kind           CommandKind
	TargetID       ID
	DestinationIDs []ID
}

func identity(kind CommandKind, target ID, destinations []ID) ([]byte, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	ids := append([]ID(nil), destinations...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for i, id := range ids {
		if err := id.Validate(); err != nil {
			return nil, err
		}
		if i > 0 && ids[i-1] == id {
			return nil, fmt.Errorf("%w: повтор назначения", ErrInvalid)
		}
	}
	return json.Marshal(requestIdentity{MetadataVersion, kind, target, ids})
}
func priorReceipt(ctx context.Context, r Repositories, requestID string, content []byte) (CommandReceipt, bool, error) {
	if err := validText("RequestID", requestID); err != nil {
		return CommandReceipt{}, false, err
	}
	prior, err := r.Runtime.Receipt(ctx, requestID)
	if err == nil {
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
func (s *Service) receipt(kind CommandKind, target ID, requestID string, content []byte, session Session, output *SessionOutput) (CommandReceipt, error) {
	record, err := newRecord()
	if err != nil {
		return CommandReceipt{}, err
	}
	sum := sha256.Sum256(content)
	c := CommandReceipt{Record: record, RequestID: requestID, Kind: kind, TargetID: target, Identity: bytes.Clone(content), Fingerprint: hex.EncodeToString(sum[:]), SessionID: session.ID, Incarnation: s.incarnation, Lifecycle: session.Lifecycle, Status: CommandPending}
	if output != nil {
		c.OutputID = output.ID
		c.OutputGeneration = output.Generation
	}
	return c, nil
}
func insertEvent(ctx context.Context, r Repositories, sessionID, outputID ID, kind, message string) error {
	id, err := newID()
	if err != nil {
		return err
	}
	return r.Runtime.InsertEvent(ctx, Event{ID: id, SessionID: sessionID, OutputID: outputID, Kind: kind, Message: message, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)})
}
func (s *Service) accept(ctx context.Context, r Repositories, c CommandReceipt) error {
	if err := r.Runtime.InsertReceipt(ctx, c); err != nil {
		return err
	}
	return insertEvent(ctx, r, c.SessionID, c.OutputID, string(c.Kind), "Команда принята")
}

func allGroups(ctx context.Context, r Repositories, streamID ID) ([]TranscodeGroup, error) {
	var result []TranscodeGroup
	q := ListQuery{Limit: MaxListLimit, StreamID: streamID}
	for {
		page, err := r.Config.Groups(ctx, q)
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		if len(page) < q.Limit {
			return result, nil
		}
		last := page[len(page)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}
func allDestinations(ctx context.Context, r Repositories, streamID ID) ([]Destination, error) {
	var result []Destination
	q := ListQuery{Limit: MaxListLimit, StreamID: streamID}
	for {
		page, err := r.Config.Destinations(ctx, q)
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		if len(page) < q.Limit {
			return result, nil
		}
		last := page[len(page)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}
func allOutputs(ctx context.Context, r Repositories, sessionID ID) ([]SessionOutput, error) {
	var result []SessionOutput
	q := ListQuery{Limit: MaxListLimit, SessionID: sessionID}
	for {
		page, err := r.Runtime.Outputs(ctx, q)
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		if len(page) < q.Limit {
			return result, nil
		}
		last := page[len(page)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}
func bindingMap(bindings []PreparedBinding) (map[ID]PreparedBinding, error) {
	result := make(map[ID]PreparedBinding, len(bindings))
	for _, b := range bindings {
		if b.GenerationID == "" || b.GroupID == "" || b.VariantID == "" || b.GeneratorVersion == "" {
			return nil, ErrInvalid
		}
		if _, ok := result[b.GroupID]; ok {
			return nil, ErrInvalid
		}
		result[b.GroupID] = b
	}
	return result, nil
}
func newOutput(ctx context.Context, r Repositories, session Session, d Destination, binding PreparedBinding) (SessionOutput, error) {
	if d.StreamID != session.StreamID {
		return SessionOutput{}, ErrConflict
	}
	g, err := r.Config.Group(ctx, d.GroupID)
	if err != nil {
		return SessionOutput{}, err
	}
	if g.StreamID != session.StreamID || binding.GroupID != g.ID {
		return SessionOutput{}, ErrConflict
	}
	p, err := r.Config.Profile(ctx, g.ProfileID)
	if err != nil {
		return SessionOutput{}, err
	}
	manifest, err := exactManifest(ctx, r, session.Snapshot.Stream.Fallback, p.Profile, binding)
	if err != nil {
		return SessionOutput{}, err
	}
	record, err := newRecord()
	if err != nil {
		return SessionOutput{}, err
	}
	return SessionOutput{Record: record, SessionID: session.ID, Snapshot: OutputSnapshot{Destination: d, Group: g, Profile: p, Fallback: session.Snapshot.Stream.Fallback, Artifact: manifest}, DesiredRunning: true, Attached: true, Generation: 1}, nil
}
func insertOutput(ctx context.Context, r Repositories, session Session, o SessionOutput) error {
	if err := r.Runtime.InsertOutput(ctx, o); err != nil {
		return err
	}
	return r.Runtime.InsertHold(ctx, ReferenceHold{SessionID: session.ID, OutputID: o.ID, StreamID: session.StreamID, GroupID: o.Snapshot.Group.ID, ProfileID: o.Snapshot.Profile.ID, DestinationID: o.Snapshot.Destination.ID, AssetID: o.Snapshot.Artifact.AssetID, VariantID: o.Snapshot.Artifact.VariantID, GenerationID: o.Snapshot.Artifact.GenerationID})
}
func (s *Service) initialOutputIntent(ctx context.Context, r Repositories, session Session, o SessionOutput) error {
	id, err := newID()
	if err != nil {
		return err
	}
	content, err := identity(CommandStartOutput, o.ID, nil)
	if err != nil {
		return err
	}
	c, err := s.receipt(CommandStartOutput, o.ID, "internal:"+string(id), content, session, &o)
	if err != nil {
		return err
	}
	return s.accept(ctx, r, c)
}

func (s *Service) StartSession(ctx context.Context, request StartSessionRequest) (CommandReceipt, error) {
	content, err := identity(CommandStartSession, request.StreamID, request.DestinationIDs)
	if err != nil {
		return CommandReceipt{}, err
	}
	var result CommandReceipt
	err = s.uow.Write(ctx, func(r Repositories) error {
		prior, found, err := priorReceipt(ctx, r, request.RequestID, content)
		if err != nil {
			return err
		}
		if found {
			result = prior
			return nil
		}
		if err = preparationIdleForRuntime(ctx, r); err != nil {
			return err
		}
		stream, err := r.Config.Stream(ctx, request.StreamID)
		if err != nil {
			return err
		}
		if _, err = r.Runtime.ActiveSession(ctx, stream.ID); err == nil {
			return ErrInUse
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		pending, err := r.Artifacts.Cleanups(ctx, ListQuery{Limit: 1, StreamID: stream.ID, PendingOnly: true})
		if err != nil {
			return err
		}
		if len(pending) != 0 {
			return ErrNotReady
		}
		if err = stream.Fallback.Validate(); err != nil {
			return fmt.Errorf("%w: заглушка не настроена", ErrNotReady)
		}
		asset, err := r.Artifacts.Asset(ctx, stream.Fallback.AssetID)
		if err != nil {
			return err
		}
		if asset.CurrentGenerationID == "" {
			return ErrNotReady
		}
		currentGeneration, err := r.Artifacts.Generation(ctx, asset.CurrentGenerationID)
		if err != nil {
			return err
		}
		if currentGeneration.AssetID != asset.ID || currentGeneration.State != PreparationPublished {
			return ErrNotReady
		}
		groups, err := allGroups(ctx, r, stream.ID)
		if err != nil {
			return err
		}
		snapshots := make([]GroupSnapshot, 0, len(groups))
		for _, g := range groups {
			p, err := r.Config.Profile(ctx, g.ProfileID)
			if err != nil {
				return err
			}
			snapshots = append(snapshots, GroupSnapshot{g, p})
		}
		record, err := newRecord()
		if err != nil {
			return err
		}
		session := Session{Record: record, StreamID: stream.ID, Incarnation: s.incarnation, State: SessionStarting, Lifecycle: 1, Snapshot: SessionSnapshot{Stream: stream, Groups: snapshots}}
		if err = r.Runtime.InsertSession(ctx, session); err != nil {
			return err
		}
		if err = r.Runtime.InsertHold(ctx, ReferenceHold{SessionID: session.ID, StreamID: stream.ID, AssetID: asset.ID, GenerationID: asset.CurrentGenerationID}); err != nil {
			return err
		}
		destinations, err := allDestinations(ctx, r, stream.ID)
		if err != nil {
			return err
		}
		selected := make(map[ID]bool, len(request.DestinationIDs))
		for _, id := range request.DestinationIDs {
			selected[id] = false
		}
		bindings, err := bindingMap(request.Bindings)
		if err != nil {
			return err
		}
		for _, d := range destinations {
			if len(request.DestinationIDs) == 0 {
				if !d.Enabled {
					continue
				}
			} else {
				if _, ok := selected[d.ID]; !ok {
					continue
				}
				selected[d.ID] = true
			}
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

func (s *Service) PauseSession(ctx context.Context, request SessionCommand) (CommandReceipt, error) {
	return s.sessionCommand(ctx, request, CommandPauseSession)
}
func (s *Service) ResumeSession(ctx context.Context, request SessionCommand) (CommandReceipt, error) {
	return s.sessionCommand(ctx, request, CommandResumeSession)
}
func (s *Service) StopSession(ctx context.Context, request SessionCommand) (CommandReceipt, error) {
	return s.sessionCommand(ctx, request, CommandStopSession)
}
func (s *Service) sessionCommand(ctx context.Context, request SessionCommand, kind CommandKind) (CommandReceipt, error) {
	content, err := identity(kind, request.SessionID, nil)
	if err != nil {
		return CommandReceipt{}, err
	}
	var result CommandReceipt
	err = s.uow.Write(ctx, func(r Repositories) error {
		prior, found, err := priorReceipt(ctx, r, request.RequestID, content)
		if err != nil {
			return err
		}
		if found {
			result = prior
			return nil
		}
		session, err := r.Runtime.Session(ctx, request.SessionID)
		if err != nil {
			return err
		}
		terminal := session.State.Terminal()
		if kind == CommandStopSession {
			if !terminal && session.State != SessionStopping {
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
		return s.accept(ctx, r, result)
	})
	return result, err
}

func (s *Service) AddOutput(ctx context.Context, request AddOutputRequest) (CommandReceipt, error) {
	content, err := identity(CommandAddOutput, request.SessionID, []ID{request.DestinationID})
	if err != nil {
		return CommandReceipt{}, err
	}
	var result CommandReceipt
	err = s.uow.Write(ctx, func(r Repositories) error {
		prior, found, err := priorReceipt(ctx, r, request.RequestID, content)
		if err != nil {
			return err
		}
		if found {
			result = prior
			return nil
		}
		session, err := r.Runtime.Session(ctx, request.SessionID)
		if err != nil {
			return err
		}
		if session.State != SessionRunning {
			return ErrConflict
		}
		d, err := r.Config.Destination(ctx, request.DestinationID)
		if err != nil {
			return err
		}
		outputs, err := allOutputs(ctx, r, session.ID)
		if err != nil {
			return err
		}
		for _, o := range outputs {
			if o.Attached && o.Snapshot.Destination.ID == d.ID {
				return ErrInUse
			}
		}
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
func (s *Service) StartOutput(ctx context.Context, request OutputCommand) (CommandReceipt, error) {
	return s.outputCommand(ctx, request, CommandStartOutput)
}
func (s *Service) StopOutput(ctx context.Context, request OutputCommand) (CommandReceipt, error) {
	return s.outputCommand(ctx, request, CommandStopOutput)
}
func (s *Service) DetachOutput(ctx context.Context, request OutputCommand) (CommandReceipt, error) {
	return s.outputCommand(ctx, request, CommandDetachOutput)
}
func (s *Service) outputCommand(ctx context.Context, request OutputCommand, kind CommandKind) (CommandReceipt, error) {
	content, err := identity(kind, request.OutputID, nil)
	if err != nil {
		return CommandReceipt{}, err
	}
	var result CommandReceipt
	err = s.uow.Write(ctx, func(r Repositories) error {
		prior, found, err := priorReceipt(ctx, r, request.RequestID, content)
		if err != nil {
			return err
		}
		if found {
			result = prior
			return nil
		}
		o, err := r.Runtime.Output(ctx, request.OutputID)
		if err != nil {
			return err
		}
		session, err := r.Runtime.Session(ctx, o.SessionID)
		if err != nil {
			return err
		}
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
			if err = stopOutput(ctx, r, session, &o, kind == CommandDetachOutput); err != nil {
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
		return s.accept(ctx, r, result)
	})
	return result, err
}

func runtimeCleanup(ctx context.Context, r Repositories, session Session, o *SessionOutput) error {
	record, err := newRecord()
	if err != nil {
		return err
	}
	intent := CleanupIntent{Record: record, Kind: CleanupRuntime, StreamID: session.StreamID, SessionID: session.ID, Incarnation: session.Incarnation, Lifecycle: session.Lifecycle}
	if o != nil {
		intent.OutputID = o.ID
		intent.Generation = o.Generation
		intent.Attempt = o.Attempt
	}
	return r.Artifacts.InsertCleanup(ctx, intent)
}
func stopOutput(ctx context.Context, r Repositories, session Session, o *SessionOutput, detach bool) error {
	// Выданный запуск Session мог создать ресурсы группы до запуска исходящего соединения.
	needsCleanup := o.Issued || detach && o.Attached && session.Issued
	if o.DesiredRunning || (needsCleanup && !o.CleanupPending) {
		if needsCleanup && !o.CleanupPending {
			if err := runtimeCleanup(ctx, r, session, o); err != nil {
				return err
			}
			o.CleanupPending = true
		}
		next, err := NextRevision(o.Generation)
		if err != nil {
			return err
		}
		o.Generation = next
	}
	o.DesiredRunning = false
	return nil
}
func stopSession(ctx context.Context, r Repositories, session *Session) error {
	if session.Issued {
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
	outputs, err := allOutputs(ctx, r, session.ID)
	if err != nil {
		return err
	}
	for _, o := range outputs {
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

func releaseOutputHolds(ctx context.Context, r Repositories, session Session, o SessionOutput) error {
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

func cancelCommands(ctx context.Context, r Repositories, sessionID, outputID, exclude ID, pauseOnly bool) error {
	q := ListQuery{Limit: MaxListLimit, SessionID: sessionID, OutputID: outputID, PendingOnly: true}
	for {
		commands, err := r.Runtime.Receipts(ctx, q)
		if err != nil {
			return err
		}
		for _, c := range commands {
			if c.ID == exclude {
				continue
			}
			if pauseOnly && c.Kind != CommandPauseSession && c.Kind != CommandResumeSession {
				continue
			}
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
		last := commands[len(commands)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}

func hasRuntimeCleanup(ctx context.Context, r Repositories, sessionID, outputID ID) (bool, error) {
	q := ListQuery{Limit: MaxListLimit, SessionID: sessionID, PendingOnly: true}
	for {
		intents, err := r.Artifacts.Cleanups(ctx, q)
		if err != nil {
			return false, err
		}
		for _, intent := range intents {
			if intent.Kind == CleanupRuntime && intent.OutputID == outputID {
				return true, nil
			}
		}
		if len(intents) < q.Limit {
			return false, nil
		}
		last := intents[len(intents)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}
func settleCleanupCommands(ctx context.Context, r Repositories, intent CleanupIntent) error {
	if intent.OutputID == "" {
		return nil
	}
	q := ListQuery{Limit: MaxListLimit, SessionID: intent.SessionID, OutputID: intent.OutputID, PendingOnly: true}
	for {
		commands, err := r.Runtime.Receipts(ctx, q)
		if err != nil {
			return err
		}
		for _, c := range commands {
			if c.Kind != CommandStopOutput && c.Kind != CommandDetachOutput {
				continue
			}
			if c.Incarnation != intent.Incarnation || c.OutputGeneration <= intent.Generation {
				continue
			}
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
		last := commands[len(commands)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}
