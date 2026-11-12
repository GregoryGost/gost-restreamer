package api

import (
	"context"
	"errors"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// StartInput задаёт поток, смысловой идентификатор запроса и выбранные назначения для асинхронного
// запуска.
type StartInput struct {
	// IDInput задаёт идентификатор запускаемого Stream в пути.
	IDInput
	// Body содержит JSON-параметры команды запуска.
	Body struct {
		// RequestID содержит непустой идентификатор смыслового запроса для идемпотентного
		// повторения запуска.
		RequestID string `json:"request_id" minLength:"1"`
		// DestinationIDs содержит идентификаторы назначений, выбранных для новой Session.
		DestinationIDs []string `json:"destination_ids"`
	}
}

// SessionCommandInput задаёт Session и идентификатор запроса остановки, паузы или возобновления.
type SessionCommandInput struct {
	// IDInput задаёт идентификатор управляемой Session.
	IDInput
	// Body содержит JSON с идентификатором смыслового запроса команды Session.
	Body struct {
		// RequestID обеспечивает повторение той же смысловой команды с прежней квитанцией.
		RequestID string `json:"request_id" minLength:"1"`
	}
}

// AddOutputInput задаёт Session и назначение для принятия нового самостоятельного выхода.
type AddOutputInput struct {
	// IDInput задаёт идентификатор Session, к которой добавляется выход.
	IDInput
	// Body содержит идентификатор запроса и выбранного назначения.
	Body struct {
		// RequestID идентифицирует смысловой запрос добавления выхода.
		RequestID string `json:"request_id" minLength:"1"`
		// DestinationID указывает назначение для нового снимка выхода.
		DestinationID string `json:"destination_id" minLength:"1"`
	}
}

// OutputIDInput адресует выход внутри явно указанной Session.
type OutputIDInput struct {
	// IDInput задаёт идентификатор родительской Session.
	IDInput
	// OutputID задаёт идентификатор выхода для проверки его принадлежности Session.
	OutputID string `path:"outputId" minLength:"1"`
}

// OutputCommandInput адресует вложенный выход и задаёт смысловой запрос управления им.
type OutputCommandInput struct {
	// OutputIDInput содержит идентификаторы Session и выхода из пути.
	OutputIDInput
	// Body содержит идентификатор запроса запуска, остановки или отсоединения выхода.
	Body struct {
		// RequestID идентифицирует смысловой запрос управления выходом.
		RequestID string `json:"request_id" minLength:"1"`
	}
}

// CommandIDInput адресует долговечную квитанцию принятой команды.
type CommandIDInput struct {
	// CommandID содержит непустой идентификатор квитанции в пути.
	CommandID string `path:"commandId" minLength:"1"`
}

// accepted возвращает безопасную квитанцию и Location её долговечного результата; завершение команды
// этим не подтверждается.
func accepted(r core.CommandReceipt) *Created[ReceiptDTO] {
	return &Created[ReceiptDTO]{Prefix + "/commands/" + string(r.ID), receiptDTO(r)}
}

// output читает Session и Output и отклоняет выход, принадлежащий другой сессии, как отсутствующий
// вложенный ресурс.
func (a *API) output(ctx context.Context, sessionID, outputID string) (core.SessionOutput, error) {
	// e содержит отказ чтения родительской Session до проверки вложенного выхода.
	if _, e := a.config.Core.GetSession(ctx, core.ID(sessionID)); e != nil {
		return core.SessionOutput{}, e
	}
	// o содержит сохранённый выход; e содержит отказ его чтения перед проверкой SessionID.
	o, e := a.config.Core.GetOutput(ctx, core.ID(outputID))
	if e != nil {
		return o, e
	}
	if o.SessionID != core.ID(sessionID) {
		return core.SessionOutput{}, core.ErrNotFound
	}
	return o, nil
}

// registerCommands публикует историю и асинхронные команды Session/Output с квитанциями 202. Вложенные
// выходы проверяются относительно родителя до выполнения команды.
func (a *API) registerCommands() {
	// c предоставляет доменные чтения истории, квитанций и сохранённых выходов зарегистрированным
	// маршрутам.
	c := a.config.Core
	register(a, operation("GET", "/sessions", "list-sessions", "История запусков."), func(ctx context.Context, i *SessionListInput) (*Result[PageDTO[SessionDTO]], error) {
		// Возвращает страницу сохранённых запусков с фильтрами потока, состояния и временного диапазона.

		// q содержит проверенные параметры страницы истории; e содержит отказ курсора или
		// временного диапазона.
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		q.Query.StreamID = core.ID(i.StreamID)
		q.Query.State = i.State
		// p содержит страницу Session с фильтрами StreamID и состояния.
		p, e := c.ListSessions(ctx, q)
		return &Result[PageDTO[SessionDTO]]{pageDTO(p, sessionDTO)}, e
	})
	register(a, operation("GET", "/sessions/{id}", "get-session", "Получить сохранённый запуск."), func(ctx context.Context, i *IDInput) (*Result[SessionDTO], error) {
		// Преобразует сохранённую Session в публичный ответ по её идентификатору.

		// s содержит сохранённую Session; e содержит отказ чтения по идентификатору.
		s, e := c.GetSession(ctx, core.ID(i.ID))
		return &Result[SessionDTO]{sessionDTO(s)}, e
	})
	register(a, operation("GET", "/commands/{commandId}", "get-command", "Получить долговечный результат команды."), func(ctx context.Context, i *CommandIDInput) (*Result[ReceiptDTO], error) {
		// Возвращает долговечную квитанцию команды в публичном представлении.

		// v содержит долговечную квитанцию; e содержит отказ её чтения по CommandID.
		v, e := c.GetReceipt(ctx, core.ID(i.CommandID))
		return &Result[ReceiptDTO]{receiptDTO(v)}, e
	})
	// op задаёт асинхронную операцию со статусом 202 и переиспользуется для команд Session и
	// выходов.
	op := operation("POST", "/streams/{id}/start", "start-session", "Принять команду запуска потока.")
	op.DefaultStatus = 202
	register(a, op, func(ctx context.Context, i *StartInput) (*Created[ReceiptDTO], error) {
		// Передаёт выбранные назначения диспетчеру запуска и возвращает квитанцию принятой команды.

		if a.config.Commands == nil {
			return nil, unavailable()
		}
		// ids собирает идентификаторы выбранных назначений в доменном типе с сохранением
		// порядка.
		ids := make([]core.ID, 0, len(i.Body.DestinationIDs))
		// id содержит очередной публичный идентификатор выбранного назначения.
		for _, id := range i.Body.DestinationIDs {
			ids = append(ids, core.ID(id))
		}
		// r содержит квитанцию принятия запуска; e содержит отказ владельца сессий.
		r, e := a.config.Commands.StartSession(ctx, core.StartSessionRequest{RequestID: i.Body.RequestID, StreamID: core.ID(i.ID), DestinationIDs: ids})
		return accepted(r), e
	})
	// route задаёт очередной маршрут остановки, паузы или возобновления Session с соответствующей
	// функцией команды.
	for _, route := range []struct {
		// suffix задаёт окончание пути команды Session; id задаёт её OperationID; summary задаёт
		// публичное описание операции.
		suffix, id, summary string
		// call передаёт выбранную команду Session соответствующей операции владельца сессий.
		call func(context.Context, core.SessionCommand) (core.CommandReceipt, error)
	}{
		{"stop", "stop-session", "Принять команду остановки запуска.", a.stopSession},
		{"pause", "pause-session", "Принять команду ручной паузы.", a.pauseSession},
		{"resume", "resume-session", "Принять команду возобновления.", a.resumeSession},
	} {
		op = operation("POST", "/sessions/{id}/"+route.suffix, route.id, route.summary)
		op.DefaultStatus = 202
		register(a, op, func(ctx context.Context, i *SessionCommandInput) (*Created[ReceiptDTO], error) {
			// Передаёт команду Session выбранной операции остановки, паузы или возобновления и формирует ссылку на
			// квитанцию.

			// r содержит квитанцию выбранной команды Session; e содержит отказ её принятия.
			r, e := route.call(ctx, core.SessionCommand{RequestID: i.Body.RequestID, SessionID: core.ID(i.ID)})
			return accepted(r), e
		})
	}
	register(a, operation("GET", "/sessions/{id}/outputs", "list-session-outputs", "Сохранённые выходы запуска."), func(ctx context.Context, i *NestedListInput) (*Result[PageDTO[OutputDTO]], error) {
		// Проверяет существование Session и возвращает страницу принадлежащих ей сохранённых выходов.

		// e содержит отказ проверки родительской Session перед выдачей сохранённых выходов.
		if _, e := c.GetSession(ctx, core.ID(i.ID)); e != nil {
			return nil, e
		}
		// q содержит проверенные параметры страницы выходов; e содержит отказ разбора страницы.
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		q.Query.SessionID = core.ID(i.ID)
		// p содержит страницу выходов, ограниченную родительской Session.
		p, e := c.ListOutputs(ctx, q)
		return &Result[PageDTO[OutputDTO]]{pageDTO(p, outputDTO)}, e
	})
	op = operation("POST", "/sessions/{id}/outputs", "add-output", "Принять команду добавления выхода.")
	op.DefaultStatus = 202
	register(a, op, func(ctx context.Context, i *AddOutputInput) (*Created[ReceiptDTO], error) {
		// Передаёт диспетчеру добавление назначения к Session и возвращает квитанцию команды.

		if a.config.Commands == nil {
			return nil, unavailable()
		}
		// r содержит квитанцию добавления выхода; e содержит отказ принятия команды с выбранным
		// назначением.
		r, e := a.config.Commands.AddOutput(ctx, core.AddOutputRequest{RequestID: i.Body.RequestID, SessionID: core.ID(i.ID), DestinationID: core.ID(i.Body.DestinationID)})
		return accepted(r), e
	})
	register(a, operation("GET", "/sessions/{id}/outputs/{outputId}", "get-output", "Получить снимок и текущие наблюдения выхода."), func(ctx context.Context, i *OutputIDInput) (*Result[OutputDetailDTO], error) {
		// Возвращает сохранённый выход вместе с доступными наблюдениями его точного исполнения.

		// o содержит сохранённый выход и доступное наблюдение его точного исполнения; e содержит
		// отказ чтения.
		o, e := a.outputDetail(ctx, i.ID, i.OutputID)
		return &Result[OutputDetailDTO]{o}, e
	})
	// route задаёт очередную команду запуска, остановки или отсоединения вложенного выхода.
	for _, route := range []struct {
		// suffix задаёт окончание пути команды выхода; id задаёт OperationID; summary задаёт
		// публичное описание команды.
		suffix, id, summary string
		// call передаёт выбранную команду выхода соответствующей операции владельца сессий.
		call func(context.Context, core.OutputCommand) (core.CommandReceipt, error)
	}{
		{"start", "start-output", "Принять команду запуска выхода.", a.startOutput},
		{"stop", "stop-output", "Принять команду остановки выхода.", a.stopOutput},
		{"detach", "detach-output", "Принять команду отсоединения выхода.", a.detachOutput},
	} {
		op = operation("POST", "/sessions/{id}/outputs/{outputId}/"+route.suffix, route.id, route.summary)
		op.DefaultStatus = 202
		register(a, op, func(ctx context.Context, i *OutputCommandInput) (*Created[ReceiptDTO], error) {
			// Проверяет принадлежность выхода Session перед передачей выбранной команды управления и выдачей
			// квитанции.

			// e содержит отказ проверки принадлежности выхода указанной Session до принятия
			// команды.
			if _, e := a.output(ctx, i.ID, i.OutputID); e != nil {
				return nil, e
			}
			// r содержит квитанцию команды выхода; e содержит отказ её принятия.
			r, e := route.call(ctx, core.OutputCommand{RequestID: i.Body.RequestID, OutputID: core.ID(i.OutputID)})
			return accepted(r), e
		})
	}
}

// stopSession передаёт команду остановки владельцу сессий либо сообщает временную недоступность при
// отсутствии порта команд.
func (a *API) stopSession(ctx context.Context, r core.SessionCommand) (core.CommandReceipt, error) {
	if a.config.Commands == nil {
		return core.CommandReceipt{}, unavailable()
	}
	return a.config.Commands.StopSession(ctx, r)
}

// pauseSession передаёт команду ручной паузы владельцу сессий либо сообщает временную недоступность
// при отсутствии порта команд.
func (a *API) pauseSession(ctx context.Context, r core.SessionCommand) (core.CommandReceipt, error) {
	if a.config.Commands == nil {
		return core.CommandReceipt{}, unavailable()
	}
	return a.config.Commands.PauseSession(ctx, r)
}

// resumeSession передаёт команду возобновления владельцу сессий либо сообщает временную недоступность
// при отсутствии порта команд.
func (a *API) resumeSession(ctx context.Context, r core.SessionCommand) (core.CommandReceipt, error) {
	if a.config.Commands == nil {
		return core.CommandReceipt{}, unavailable()
	}
	return a.config.Commands.ResumeSession(ctx, r)
}

// startOutput передаёт команду запуска выхода владельцу сессий либо сообщает временную недоступность
// при отсутствии порта команд.
func (a *API) startOutput(ctx context.Context, r core.OutputCommand) (core.CommandReceipt, error) {
	if a.config.Commands == nil {
		return core.CommandReceipt{}, unavailable()
	}
	return a.config.Commands.StartOutput(ctx, r)
}

// stopOutput передаёт команду остановки выхода владельцу сессий либо сообщает временную недоступность
// при отсутствии порта команд.
func (a *API) stopOutput(ctx context.Context, r core.OutputCommand) (core.CommandReceipt, error) {
	if a.config.Commands == nil {
		return core.CommandReceipt{}, unavailable()
	}
	return a.config.Commands.StopOutput(ctx, r)
}

// detachOutput передаёт команду отсоединения выхода владельцу сессий либо сообщает временную
// недоступность при отсутствии порта команд.
func (a *API) detachOutput(ctx context.Context, r core.OutputCommand) (core.CommandReceipt, error) {
	if a.config.Commands == nil {
		return core.CommandReceipt{}, unavailable()
	}
	return a.config.Commands.DetachOutput(ctx, r)
}

// OutputDetailDTO соединяет самостоятельный сохранённый снимок выхода с текущим наблюдением только
// совпадающего исполнения.
type OutputDetailDTO struct {
	// OutputDTO содержит долговечный снимок назначения, группы, профиля и желаемого состояния
	// выхода.
	OutputDTO
	// Runtime содержит наблюдение совпадающего активного исполнения либо null.
	Runtime *RuntimeOutputDTO `json:"runtime"`
	// RuntimeAvailability явно сообщает available или unavailable текущего наблюдения.
	RuntimeAvailability string `json:"runtime_availability"`
	// RuntimeReason объясняет отсутствие наблюдения или неактивное исполнение выхода.
	RuntimeReason string `json:"runtime_reason"`
}

// outputDetail соединяет сохранённый снимок выхода только с наблюдением его точного активного
// исполнения. Завершённый, отсоединённый или ещё не наблюдаемый выход получает явную недоступность
// вместо чужой или исторической телеметрии.
func (a *API) outputDetail(ctx context.Context, sessionID, outputID string) (OutputDetailDTO, error) {
	// o содержит сохранённый выход проверенной Session; e содержит отказ проверки вложенного
	// ресурса.
	o, e := a.output(ctx, sessionID, outputID)
	if e != nil {
		return OutputDetailDTO{}, e
	}
	// result начинает с долговечного снимка и причины not_observed до проверки активного точного
	// исполнения.
	result := OutputDetailDTO{OutputDTO: outputDTO(o), RuntimeAvailability: "unavailable", RuntimeReason: "not_observed"}
	// session содержит текущую сохранённую Session выхода для сверки её жизненного цикла.
	session, e := a.config.Core.GetSession(ctx, o.SessionID)
	if e != nil {
		return OutputDetailDTO{}, e
	}
	if !session.State.Active() || !o.Attached || !o.DesiredRunning || !o.Issued || o.CleanupPending {
		result.RuntimeReason = "execution_inactive"
		return result, nil
	}
	// snapshot содержит оперативные наблюдения точного incarnation и lifecycle Session.
	snapshot, e := a.config.Core.RuntimeSnapshot(ctx, core.RuntimeSessionFence{SessionID: session.ID, Incarnation: session.Incarnation, Lifecycle: session.Lifecycle})
	if errors.Is(e, core.ErrNotReady) || errors.Is(e, core.ErrRuntimeCapability) {
		return result, nil
	}
	if e != nil {
		return OutputDetailDTO{}, e
	}
	// observed содержит очередного текущего наблюдателя выхода для проверки полной идентичности
	// исполнения.
	for _, observed := range snapshot.Outputs {
		// f содержит ExecutionFence наблюдателя для сверки выхода, Session, incarnation,
		// lifecycle, поколения и попытки.
		f := observed.Execution
		if f.OutputID == o.ID && f.SessionID == session.ID && f.Incarnation == session.Incarnation && f.Lifecycle == session.Lifecycle && f.OutputGeneration == o.Generation && f.Attempt == o.Attempt {
			// state содержит состояние совпавшего наблюдателя либо UNKNOWN до первого
			// значения.
			state := "UNKNOWN"
			// failed отмечает исходную ошибку совпавшего наблюдения для безопасного
			// публичного обозначения.
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
