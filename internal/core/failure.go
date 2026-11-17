package core

import "fmt"

// FailureCode задаёт закрытый каталог доказанных предметных причин без текста диагностики.
type FailureCode string

const (
	// FailureFallbackNotConfigured обозначает отсутствие выбранной политики заглушки.
	FailureFallbackNotConfigured FailureCode = "fallback_not_configured"
	// FailureFallbackNotPrepared обозначает отсутствие опубликованного готового поколения.
	FailureFallbackNotPrepared FailureCode = "fallback_not_prepared"
	// FailureFallbackProfileNotPrepared обозначает отсутствие варианта полного профиля.
	FailureFallbackProfileNotPrepared FailureCode = "fallback_profile_not_prepared"
	// FailureFallbackVariantNotReady обозначает непригодность конкретного закреплённого варианта.
	FailureFallbackVariantNotReady FailureCode = "fallback_variant_not_ready"
	// FailurePreparationActive обозначает подготовку, исключающую новый запуск.
	FailurePreparationActive FailureCode = "preparation_active"
	// FailureSessionActive обозначает существующую активную Session потока.
	FailureSessionActive FailureCode = "session_active"
	// FailureRequestIDConflict обозначает несовпадение точного содержимого повторного запроса.
	FailureRequestIDConflict FailureCode = "request_id_conflict"
	// FailureRevisionConflict обозначает несовпадение ожидаемой и сохранённой ревизий.
	FailureRevisionConflict FailureCode = "revision_conflict"
	// FailureSessionStateConflict обозначает неприменимость команды к состоянию Session.
	FailureSessionStateConflict FailureCode = "session_state_conflict"
	// FailureOutputDetached обозначает отсоединённый выход, которым нельзя управлять этой командой.
	FailureOutputDetached FailureCode = "output_detached"
	// FailureOutputAlreadyRunning обозначает уже установленное намерение запуска выхода.
	FailureOutputAlreadyRunning FailureCode = "output_already_running"
	// FailureOutputCleanupPending обозначает неподтверждённую очистку перед новым запуском выхода.
	FailureOutputCleanupPending FailureCode = "output_cleanup_pending"
	// FailureDestinationAttached обозначает уже прикреплённое к Session назначение.
	FailureDestinationAttached FailureCode = "destination_attached"
	// FailureResourceInUse обозначает доказанное использование изменяемого или удаляемого объекта.
	FailureResourceInUse FailureCode = "resource_in_use"
	// FailureResourceOwnershipConflict обозначает несовместимую принадлежность прочитанных объектов.
	FailureResourceOwnershipConflict FailureCode = "resource_ownership_conflict"
	// FailureSessionStartFailed обозначает отказ известного шага запуска Session.
	FailureSessionStartFailed FailureCode = "session_start_failed"
	// FailureOutputStartFailed обозначает отказ известного шага запуска выхода.
	FailureOutputStartFailed FailureCode = "output_start_failed"
	// FailureSessionStopFailed обозначает отказ остановки Session.
	FailureSessionStopFailed FailureCode = "session_stop_failed"
	// FailureOutputStopFailed обозначает отказ остановки выхода.
	FailureOutputStopFailed FailureCode = "output_stop_failed"
	// FailureSessionPauseFailed обозначает отказ применения ручной паузы.
	FailureSessionPauseFailed FailureCode = "session_pause_failed"
	// FailureSessionResumeFailed обозначает отказ снятия ручной паузы.
	FailureSessionResumeFailed FailureCode = "session_resume_failed"
	// FailureExecutionInterrupted обозначает потерю прежнего владельца исполнения.
	FailureExecutionInterrupted FailureCode = "execution_interrupted"
	// FailureCommandSuperseded обозначает отмену команды последующим управлением.
	FailureCommandSuperseded FailureCode = "command_superseded"
)

// FailureField ограничивает публичное имя поля запроса или состояния известными частями контракта.
type FailureField string

const (
	// FailureFieldRevision указывает ожидаемую ревизию условной записи.
	FailureFieldRevision FailureField = "expected_revision"
	// FailureFieldRequestID указывает поле повторяемого запроса без его значения.
	FailureFieldRequestID FailureField = "request_id"
	// FailureFieldFallback указывает политику выбранной заглушки.
	FailureFieldFallback FailureField = "fallback"
	// FailureFieldProfile указывает полный профиль обработки.
	FailureFieldProfile FailureField = "profile"
	// FailureFieldState указывает неприменимое состояние ресурса.
	FailureFieldState FailureField = "state"
	// FailureFieldDestinationID указывает выбор назначения без отражения произвольного входного ID.
	FailureFieldDestinationID FailureField = "destination_id"
)

// FailureContext содержит только идентификаторы прочитанных сущностей или самостоятельных снимков.
// Проверка синтаксиса ID не доказывает происхождение: его устанавливает создающая причину ветвь.
type FailureContext struct {
	// StreamID идентифицирует известный поток.
	StreamID ID `json:"stream_id,omitempty"`
	// SessionID идентифицирует прочитанный или созданный запуск.
	SessionID ID `json:"session_id,omitempty"`
	// OutputID идентифицирует известный выход запуска.
	OutputID ID `json:"output_id,omitempty"`
	// GroupID идентифицирует прочитанную группу либо группу снимка.
	GroupID ID `json:"group_id,omitempty"`
	// ProfileID идентифицирует прочитанный профиль либо профиль снимка.
	ProfileID ID `json:"profile_id,omitempty"`
	// AssetID идентифицирует известную заглушку.
	AssetID ID `json:"asset_id,omitempty"`
	// GenerationID идентифицирует прочитанное или закреплённое поколение.
	GenerationID ID `json:"generation_id,omitempty"`
	// VariantID идентифицирует прочитанный или закреплённый вариант.
	VariantID ID `json:"variant_id,omitempty"`
	// DestinationID идентифицирует известное назначение.
	DestinationID ID `json:"destination_id,omitempty"`
	// CommandID идентифицирует созданную сервером квитанцию команды.
	CommandID ID `json:"command_id,omitempty"`
}

// FailureReason сохраняет машинную причину отдельно от диагностики и локализованного представления.
// Все вложенные данные копируются по значению и не содержат указателей или изменяемых коллекций.
type FailureReason struct {
	// Code выбирает подготовленное объяснение из закрытого каталога.
	Code FailureCode `json:"code"`
	// Field необязательно указывает известное поле запроса или состояния ресурса.
	Field FailureField `json:"field,omitempty"`
	// Context содержит только доказанные публичные идентификаторы.
	Context FailureContext `json:"context,omitzero"`
}

// Validate отклоняет неизвестные код и поле, недопустимые сочетания поля и кода и некорректные ID.
// Происхождение ID проверяется вызывающим кодом по прочитанным сущностям, а не этой функцией.
func (r FailureReason) Validate() error {
	switch r.Code {
	case FailureFallbackNotConfigured, FailureFallbackNotPrepared, FailureFallbackProfileNotPrepared,
		FailureFallbackVariantNotReady, FailurePreparationActive, FailureSessionActive,
		FailureRequestIDConflict, FailureRevisionConflict, FailureSessionStateConflict,
		FailureOutputDetached, FailureOutputAlreadyRunning, FailureOutputCleanupPending,
		FailureDestinationAttached, FailureResourceInUse, FailureResourceOwnershipConflict,
		FailureSessionStartFailed, FailureOutputStartFailed, FailureSessionStopFailed,
		FailureOutputStopFailed, FailureSessionPauseFailed, FailureSessionResumeFailed,
		FailureExecutionInterrupted, FailureCommandSuperseded:
	default:
		return fmt.Errorf("%w: неизвестный код причины", ErrInvalid)
	}
	if r.Field != "" {
		switch r.Code {
		case FailureRevisionConflict:
			if r.Field != FailureFieldRevision {
				return ErrInvalid
			}
		case FailureRequestIDConflict:
			if r.Field != FailureFieldRequestID {
				return ErrInvalid
			}
		case FailureFallbackNotConfigured, FailureFallbackNotPrepared, FailureFallbackVariantNotReady:
			if r.Field != FailureFieldFallback {
				return ErrInvalid
			}
		case FailureFallbackProfileNotPrepared:
			if r.Field != FailureFieldProfile {
				return ErrInvalid
			}
		case FailureSessionStateConflict, FailureOutputDetached, FailureOutputAlreadyRunning, FailureOutputCleanupPending:
			if r.Field != FailureFieldState {
				return ErrInvalid
			}
		case FailureDestinationAttached, FailureResourceOwnershipConflict:
			if r.Field != FailureFieldDestinationID {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
	}
	// id содержит очередную непустую ссылку из фиксированного публичного контекста.
	for _, id := range []ID{r.Context.StreamID, r.Context.SessionID, r.Context.OutputID, r.Context.GroupID, r.Context.ProfileID, r.Context.AssetID, r.Context.GenerationID, r.Context.VariantID, r.Context.DestinationID, r.Context.CommandID} {
		if id != "" {
			// err сообщает нарушение синтаксиса сохраняемой ссылки.
			if err := id.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReasonError связывает проверенную предметную причину с исходной ошибкой для errors.Is и журналов.
// Закрытые поля не позволяют подменить причину после создания оболочки.
type ReasonError struct {
	// cause сохраняет исходный результат конкретной проверяющей ветви.
	cause error
	// reason содержит самостоятельную проверенную причину этой же ветви.
	reason FailureReason
}

// WithFailureReason создаёт доверенную оболочку только для непустого cause и допустимой reason.
// Ошибочные аргументы возвращают ErrInvalid без доверенной причины; исходная диагностика не публикуется.
func WithFailureReason(cause error, reason FailureReason) error {
	if cause == nil {
		return fmt.Errorf("%w: отсутствует исходная ошибка", ErrInvalid)
	}
	// err содержит нарушение закрытого контракта причины до её использования или сохранения.
	if err := reason.Validate(); err != nil {
		return err
	}
	return &ReasonError{cause: cause, reason: reason}
}

// Error возвращает полную исходную диагностику для локального журнала приложения.
// Нулевое значение не является доверенной причиной и описывается безопасной общей строкой.
func (e *ReasonError) Error() string {
	if e == nil || e.cause == nil {
		return "некорректная причина отказа"
	}
	return e.cause.Error()
}

// Unwrap сохраняет исходную ошибку без изменения её тождественности.
func (e *ReasonError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Reason возвращает самостоятельное значение причины, не разрешая изменение оболочки.
func (e *ReasonError) Reason() FailureReason {
	if e == nil {
		return FailureReason{}
	}
	return e.reason
}

// CloneFailureReason копирует необязательную причину, сохраняя отсутствие поля у старых записей.
func CloneFailureReason(reason *FailureReason) *FailureReason {
	if reason == nil {
		return nil
	}
	// result не разделяет указатель вызывающего кода с сохраняемой записью или DTO.
	result := *reason
	return &result
}

// FailureReasonFrom извлекает одну однозначную конкретную причину исполнения из дерева ошибок.
// Разные причины объединённых ветвей не смешиваются. HTTP дополнительно отбирает причины
// по классу исходной ветви после выбора статуса и не использует этот общий извлекатель.
func FailureReasonFrom(err error) *FailureReason {
	// result содержит первую проверенную причину; ambiguous отмечает несовпадение другой ветви.
	var result *FailureReason
	var ambiguous bool
	// visit обходит исходные оболочки и Join, сохраняя каждую конкретную причину целиком.
	var visit func(error)
	visit = func(current error) {
		if current == nil || ambiguous {
			return
		}
		// typed распознаёт только собственную оболочку; ok исключает произвольные интерфейсы причин.
		if typed, ok := current.(*ReasonError); ok && typed != nil {
			if typed.cause == nil || typed.reason.Validate() != nil {
				ambiguous = true
				return
			}
			if result != nil && *result != typed.reason {
				ambiguous = true
				return
			}
			result = CloneFailureReason(&typed.reason)
			return
		}
		// unwrapped предоставляет только структуру дерева исходных ошибок.
		switch unwrapped := current.(type) {
		case interface {
			// Unwrap возвращает независимые ветви составной ошибки.
			Unwrap() []error
		}:
			// child содержит одну независимую ветвь объединённой ошибки.
			for _, child := range unwrapped.Unwrap() {
				visit(child)
			}
		case interface {
			// Unwrap возвращает исходную ошибку обычной оболочки.
			Unwrap() error
		}:
			visit(unwrapped.Unwrap())
		}
	}
	visit(err)
	if ambiguous {
		return nil
	}
	return result
}

// validateResultReason допускает новую причину только при неуспешном результате с диагностикой.
// Отсутствующее поле сохраняет совместимость со старыми нагрузками без дополнительных требований.
func validateResultReason(reason *FailureReason, failure string, failed, interrupted bool) error {
	if reason == nil {
		return nil
	}
	if !failed || failure == "" {
		return ErrInvalid
	}
	if (reason.Code == FailureExecutionInterrupted || reason.Code == FailureCommandSuperseded) != interrupted {
		return ErrInvalid
	}
	return reason.Validate()
}

// ValidateFailureReason проверяет необязательную причину квитанции также на пути записи storage.normalize,
// который допускает прежнюю неполную идентичность до чтения строгим кодеком.
func (c CommandReceipt) ValidateFailureReason() error {
	return validateResultReason(c.FailureReason, c.Failure, c.Status == CommandFailed || c.Status == CommandInterrupted, c.Status == CommandInterrupted)
}
