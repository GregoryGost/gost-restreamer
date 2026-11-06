package api

import (
	"context"
	"errors"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

type StartInput struct {
	IDInput
	Body struct {
		RequestID      string   `json:"request_id" minLength:"1"`
		DestinationIDs []string `json:"destination_ids"`
	}
}
type SessionCommandInput struct {
	IDInput
	Body struct {
		RequestID string `json:"request_id" minLength:"1"`
	}
}
type AddOutputInput struct {
	IDInput
	Body struct {
		RequestID     string `json:"request_id" minLength:"1"`
		DestinationID string `json:"destination_id" minLength:"1"`
	}
}
type OutputIDInput struct {
	IDInput
	OutputID string `path:"outputId" minLength:"1"`
}
type OutputCommandInput struct {
	OutputIDInput
	Body struct {
		RequestID string `json:"request_id" minLength:"1"`
	}
}
type CommandIDInput struct {
	CommandID string `path:"commandId" minLength:"1"`
}

func accepted(r core.CommandReceipt) *Created[ReceiptDTO] {
	return &Created[ReceiptDTO]{Prefix + "/commands/" + string(r.ID), receiptDTO(r)}
}
func (a *API) output(ctx context.Context, sessionID, outputID string) (core.SessionOutput, error) {
	if _, e := a.config.Core.GetSession(ctx, core.ID(sessionID)); e != nil {
		return core.SessionOutput{}, e
	}
	o, e := a.config.Core.GetOutput(ctx, core.ID(outputID))
	if e != nil {
		return o, e
	}
	if o.SessionID != core.ID(sessionID) {
		return core.SessionOutput{}, core.ErrNotFound
	}
	return o, nil
}
func (a *API) registerCommands() {
	c := a.config.Core
	register(a, operation("GET", "/sessions", "list-sessions", "История запусков."), func(ctx context.Context, i *SessionListInput) (*Result[PageDTO[SessionDTO]], error) {
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		q.Query.StreamID = core.ID(i.StreamID)
		q.Query.State = i.State
		p, e := c.ListSessions(ctx, q)
		return &Result[PageDTO[SessionDTO]]{pageDTO(p, sessionDTO)}, e
	})
	register(a, operation("GET", "/sessions/{id}", "get-session", "Получить сохранённый запуск."), func(ctx context.Context, i *IDInput) (*Result[SessionDTO], error) {
		s, e := c.GetSession(ctx, core.ID(i.ID))
		return &Result[SessionDTO]{sessionDTO(s)}, e
	})
	register(a, operation("GET", "/commands/{commandId}", "get-command", "Получить долговечный результат команды."), func(ctx context.Context, i *CommandIDInput) (*Result[ReceiptDTO], error) {
		v, e := c.GetReceipt(ctx, core.ID(i.CommandID))
		return &Result[ReceiptDTO]{receiptDTO(v)}, e
	})
	op := operation("POST", "/streams/{id}/start", "start-session", "Принять команду запуска потока.")
	op.DefaultStatus = 202
	register(a, op, func(ctx context.Context, i *StartInput) (*Created[ReceiptDTO], error) {
		if a.config.Commands == nil {
			return nil, unavailable()
		}
		ids := make([]core.ID, 0, len(i.Body.DestinationIDs))
		for _, id := range i.Body.DestinationIDs {
			ids = append(ids, core.ID(id))
		}
		r, e := a.config.Commands.StartSession(ctx, core.StartSessionRequest{RequestID: i.Body.RequestID, StreamID: core.ID(i.ID), DestinationIDs: ids})
		return accepted(r), e
	})
	for _, route := range []struct {
		suffix, id, summary string
		call                func(context.Context, core.SessionCommand) (core.CommandReceipt, error)
	}{
		{"stop", "stop-session", "Принять команду остановки запуска.", a.stopSession},
		{"pause", "pause-session", "Принять команду ручной паузы.", a.pauseSession},
		{"resume", "resume-session", "Принять команду возобновления.", a.resumeSession},
	} {
		op = operation("POST", "/sessions/{id}/"+route.suffix, route.id, route.summary)
		op.DefaultStatus = 202
		register(a, op, func(ctx context.Context, i *SessionCommandInput) (*Created[ReceiptDTO], error) {
			r, e := route.call(ctx, core.SessionCommand{RequestID: i.Body.RequestID, SessionID: core.ID(i.ID)})
			return accepted(r), e
		})
	}
	register(a, operation("GET", "/sessions/{id}/outputs", "list-session-outputs", "Сохранённые выходы запуска."), func(ctx context.Context, i *NestedListInput) (*Result[PageDTO[OutputDTO]], error) {
		if _, e := c.GetSession(ctx, core.ID(i.ID)); e != nil {
			return nil, e
		}
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		q.Query.SessionID = core.ID(i.ID)
		p, e := c.ListOutputs(ctx, q)
		return &Result[PageDTO[OutputDTO]]{pageDTO(p, outputDTO)}, e
	})
	op = operation("POST", "/sessions/{id}/outputs", "add-output", "Принять команду добавления выхода.")
	op.DefaultStatus = 202
	register(a, op, func(ctx context.Context, i *AddOutputInput) (*Created[ReceiptDTO], error) {
		if a.config.Commands == nil {
			return nil, unavailable()
		}
		r, e := a.config.Commands.AddOutput(ctx, core.AddOutputRequest{RequestID: i.Body.RequestID, SessionID: core.ID(i.ID), DestinationID: core.ID(i.Body.DestinationID)})
		return accepted(r), e
	})
	register(a, operation("GET", "/sessions/{id}/outputs/{outputId}", "get-output", "Получить снимок и текущие наблюдения выхода."), func(ctx context.Context, i *OutputIDInput) (*Result[OutputDetailDTO], error) {
		o, e := a.outputDetail(ctx, i.ID, i.OutputID)
		return &Result[OutputDetailDTO]{o}, e
	})
	for _, route := range []struct {
		suffix, id, summary string
		call                func(context.Context, core.OutputCommand) (core.CommandReceipt, error)
	}{
		{"start", "start-output", "Принять команду запуска выхода.", a.startOutput},
		{"stop", "stop-output", "Принять команду остановки выхода.", a.stopOutput},
		{"detach", "detach-output", "Принять команду отсоединения выхода.", a.detachOutput},
	} {
		op = operation("POST", "/sessions/{id}/outputs/{outputId}/"+route.suffix, route.id, route.summary)
		op.DefaultStatus = 202
		register(a, op, func(ctx context.Context, i *OutputCommandInput) (*Created[ReceiptDTO], error) {
			if _, e := a.output(ctx, i.ID, i.OutputID); e != nil {
				return nil, e
			}
			r, e := route.call(ctx, core.OutputCommand{RequestID: i.Body.RequestID, OutputID: core.ID(i.OutputID)})
			return accepted(r), e
		})
	}
}
func (a *API) stopSession(ctx context.Context, r core.SessionCommand) (core.CommandReceipt, error) {
	if a.config.Commands == nil {
		return core.CommandReceipt{}, unavailable()
	}
	return a.config.Commands.StopSession(ctx, r)
}
func (a *API) pauseSession(ctx context.Context, r core.SessionCommand) (core.CommandReceipt, error) {
	if a.config.Commands == nil {
		return core.CommandReceipt{}, unavailable()
	}
	return a.config.Commands.PauseSession(ctx, r)
}
func (a *API) resumeSession(ctx context.Context, r core.SessionCommand) (core.CommandReceipt, error) {
	if a.config.Commands == nil {
		return core.CommandReceipt{}, unavailable()
	}
	return a.config.Commands.ResumeSession(ctx, r)
}
func (a *API) startOutput(ctx context.Context, r core.OutputCommand) (core.CommandReceipt, error) {
	if a.config.Commands == nil {
		return core.CommandReceipt{}, unavailable()
	}
	return a.config.Commands.StartOutput(ctx, r)
}
func (a *API) stopOutput(ctx context.Context, r core.OutputCommand) (core.CommandReceipt, error) {
	if a.config.Commands == nil {
		return core.CommandReceipt{}, unavailable()
	}
	return a.config.Commands.StopOutput(ctx, r)
}
func (a *API) detachOutput(ctx context.Context, r core.OutputCommand) (core.CommandReceipt, error) {
	if a.config.Commands == nil {
		return core.CommandReceipt{}, unavailable()
	}
	return a.config.Commands.DetachOutput(ctx, r)
}

type OutputDetailDTO struct {
	OutputDTO
	Runtime             *RuntimeOutputDTO `json:"runtime"`
	RuntimeAvailability string            `json:"runtime_availability"`
	RuntimeReason       string            `json:"runtime_reason"`
}

func (a *API) outputDetail(ctx context.Context, sessionID, outputID string) (OutputDetailDTO, error) {
	o, e := a.output(ctx, sessionID, outputID)
	if e != nil {
		return OutputDetailDTO{}, e
	}
	result := OutputDetailDTO{OutputDTO: outputDTO(o), RuntimeAvailability: "unavailable", RuntimeReason: "not_observed"}
	session, e := a.config.Core.GetSession(ctx, o.SessionID)
	if e != nil {
		return OutputDetailDTO{}, e
	}
	if !session.State.Active() || !o.Attached || !o.DesiredRunning || !o.Issued || o.CleanupPending {
		result.RuntimeReason = "execution_inactive"
		return result, nil
	}
	snapshot, e := a.config.Core.RuntimeSnapshot(ctx, core.RuntimeSessionFence{SessionID: session.ID, Incarnation: session.Incarnation, Lifecycle: session.Lifecycle})
	if errors.Is(e, core.ErrNotReady) || errors.Is(e, core.ErrRuntimeCapability) {
		return result, nil
	}
	if e != nil {
		return OutputDetailDTO{}, e
	}
	for _, observed := range snapshot.Outputs {
		f := observed.Execution
		if f.OutputID == o.ID && f.SessionID == session.ID && f.Incarnation == session.Incarnation && f.Lifecycle == session.Lifecycle && f.OutputGeneration == o.Generation && f.Attempt == o.Attempt {
			state := "UNKNOWN"
			failed := false
			if observed.Value != nil {
				state = string(observed.Value.State)
				failed = observed.Value.Error != ""
			}
			result.Runtime = &RuntimeOutputDTO{observationDTO(snapshot.Cursor.Epoch, observed.Stamp, state, failed), string(o.ID), outputMetricsDTO(observed.Metrics)}
			result.RuntimeAvailability = "available"
			result.RuntimeReason = ""
			return result, nil
		}
	}
	return result, nil
}
