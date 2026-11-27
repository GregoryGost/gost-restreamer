package chat

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/chatwire"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// prepareEventLocked рассчитывает размеры следующего события до финальной проверки полномочий.
// Один номер в конце диапазона сохраняется для завершения точной Session.
func (h *chatHub) prepareEventLocked(event core.ChatEvent, messageJSON []byte) (chatVisibleEvent, error) {
	if h.eventOrder == math.MaxUint64 || h.eventOrder == math.MaxUint64-1 && event.Kind != core.ChatEventSessionClosed {
		return chatVisibleEvent{}, core.ErrNotReady
	}
	// position содержит номер кандидата, который станет видимым после публикации.
	position := h.eventOrder + 1
	event.Cursor = h.cursor(position)
	event.SessionID = h.fence.SessionID
	if event.Validate() != nil {
		return chatVisibleEvent{}, core.ErrInvalid
	}
	// encoded, frame и logical учитывают журнал, полный SSE-кадр и очередь подписчика соответственно.
	// err сообщает о недопустимом JSON или арифметическом переполнении.
	encoded, frame, logical, err := chatEventSizes(event, messageJSON)
	if err != nil {
		return chatVisibleEvent{}, core.ErrInvalid
	}
	return chatVisibleEvent{event: event, position: position, encodedBytes: encoded, frameBytes: frame, logicalBytes: logical}, nil
}

// publishLocked делает подготовленное событие видимым и уведомляет подписчиков без ожидания чтения.
func (h *chatHub) publishLocked(event chatVisibleEvent) {
	h.eventOrder = event.position
	h.replay.retain(event, event.encodedBytes, h.limits.Replay)
	// subscriber содержит очередной активный или отставший допуск к этой эпохе.
	for subscriber := range h.subscribers {
		subscriber.offerLocked(event)
	}
	h.signalLocked()
}

// clearMessages удаляет историю и прежний replay на той же позиции, где публикуется chat.cleared.
// Вход адаптеров, счётчики, дедупликация и статусы продолжают жить в этой эпохе.
func (h *chatHub) clearMessages(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.isStopping() {
		return core.ErrStale
	}
	// err сообщает об отмене до подготовки изменений Hub.
	if err := ctx.Err(); err != nil {
		return err
	}
	// event содержит границу очистки, err сообщает об исчерпании позиции.
	event, err := h.prepareEventLocked(core.ChatEvent{Kind: core.ChatEventCleared}, nil)
	if err != nil {
		return err
	}
	// apply фиксирует очистку и будит подписчиков одним изменением позиции.
	apply := func() {
		h.history = chatBuffer[core.ChatMessage]{}
		h.replay = chatBuffer[chatVisibleEvent]{}
		h.clearedThrough = h.messageOrder
		// subscriber удерживает очередь, из которой убирается удалённая история.
		for subscriber := range h.subscribers {
			subscriber.discardMessagesLocked()
		}
		h.publishLocked(event)
	}
	if h.test != nil {
		return h.test.Publish(ctx, apply)
	}
	apply()
	return nil
}

// signalLocked будит читателей журнала и предоставляет канал для ожидания следующего изменения.
func (h *chatHub) signalLocked() {
	close(h.changed)
	if !h.closed {
		h.changed = make(chan struct{})
	}
}

// cursor подписывает точную позицию ключом текущей эпохи с потоковым контекстом.
func (h *chatHub) cursor(position uint64) core.ChatCursor {
	return h.key.cursor(string(h.streamID), position)
}

// setStatus копирует и проверяет состояние до его публикации в отдельном потоке событий.
func (h *chatHub) setStatus(status core.ChatSourceStatus) error {
	status = chatCopyStatus(status)
	if status.Validate() != nil {
		return core.ErrInvalid
	}
	status.SourceID = core.ID(strings.Clone(string(status.SourceID)))
	status.State = core.ChatSourceState(strings.Clone(string(status.State)))
	status.Reason = core.ChatReason(strings.Clone(string(status.Reason)))
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.isStopping() {
		return core.ErrStale
	}
	return h.setStatusLocked(status)
}

// setStatusLocked сохраняет допустимый статус и его событие на одной позиции.
// При переполнении предыдущий статус остаётся доступным в snapshot.
func (h *chatHub) setStatusLocked(status core.ChatSourceStatus) error {
	// prepared содержит статус и его измеренный размер, err сообщает об отказе сохранить его.
	prepared, err := h.prepareStatusLocked(status)
	if err != nil || prepared.size == 0 {
		return err
	}
	// event содержит подготовленную запись replay, err сообщает об исчерпании позиции.
	event, err := h.prepareEventLocked(core.ChatEvent{Kind: core.ChatEventSourceStatus, Status: &prepared.status}, nil)
	if err != nil {
		return err
	}
	h.storeStatusLocked(prepared)
	h.publishLocked(event)
	return nil
}

// prepareStatusLocked проверяет метаданные и сохраняет последнюю известную активность при смене состояния.
// Нулевой size означает, что статус уже соответствует сохранённому значению.
func (h *chatHub) prepareStatusLocked(status core.ChatSourceStatus) (chatHubStatus, error) {
	// old содержит прежнюю запись, exists отличает замену от добавления.
	old, exists := h.statuses[status.SourceID]
	if exists {
		status.Dropped = max(status.Dropped, old.status.Dropped)
		if old.status.LastEventAt != nil && (status.LastEventAt == nil || !status.LastEventAt.After(*old.status.LastEventAt)) {
			status.LastEventAt = old.status.LastEventAt
		}
		if status == old.status {
			return chatHubStatus{}, nil
		}
	} else if len(h.statuses) >= h.limits.Registry.Count {
		return chatHubStatus{}, core.ErrNotReady
	}
	// data содержит закодированное состояние, err сообщает о непредставимых значениях.
	data, err := json.Marshal(chatwire.Status(status))
	if err != nil {
		return chatHubStatus{}, core.ErrInvalid
	}
	// size включает один разделитель в массиве для ограничения статусов независимо от конфигураций.
	size := uint64(len(data)) + 1
	if size > h.limits.SnapshotMetadataBytes-(h.statusBytes-old.size) {
		return chatHubStatus{}, core.ErrNotReady
	}
	return chatHubStatus{status: status, size: size}, nil
}

// prepareMessageStatusLocked обновляет активность существующего источника вместе с публикацией сообщения.
// Если новый статус не помещается в бюджет, публичный статус сохраняет прежнее значение.
func (h *chatHub) prepareMessageStatusLocked(message core.ChatMessage) chatHubStatus {
	// current содержит состояние атрибутированного источника, exists показывает, представлен ли он в Hub.
	current, exists := h.statuses[message.ChatSourceID]
	if !exists {
		return chatHubStatus{}
	}
	current.status.LastEventAt = chatCopyTime(&message.ReceivedAt)
	// updated содержит кандидат для того же cursor, что и сообщение. Ошибка бюджета оставляет нулевой size.
	updated, _ := h.prepareStatusLocked(current.status)
	return updated
}

// storeStatusLocked применяет заранее измеренную запись, пока старое значение защищено той же блокировкой.
func (h *chatHub) storeStatusLocked(current chatHubStatus) {
	// old содержит прежний вес, который освобождается единой заменой.
	old := h.statuses[current.status.SourceID]
	h.statusBytes = h.statusBytes - old.size + current.size
	h.statuses[current.status.SourceID] = current
}

// removeStatus исключает отозванный источник из будущих снимков и уведомляет читателей о необходимости обновить снимок.
// Ранее опубликованные сообщения сохраняют атрибуцию.
func (h *chatHub) removeStatus(sourceID core.ID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// status содержит удаляемый вес, ok показывает наличие оперативной записи.
	status, ok := h.statuses[sourceID]
	if h.closed || h.isStopping() || !ok {
		return
	}
	// event связывает исключение статуса с новой позицией, err сообщает об исчерпании диапазона.
	event, err := h.prepareEventLocked(core.ChatEvent{Kind: core.ChatEventResync}, nil)
	if err != nil {
		h.stop()
		return
	}
	delete(h.statuses, sourceID)
	h.statusBytes -= status.size
	h.publishLocked(event)
}

// gap отмечает подтверждённый upstream-пробел без вымышленного числа потерянных сообщений.
func (h *chatHub) gap(reason core.ChatReason) error {
	if reason.Validate() != nil {
		return core.ErrInvalid
	}
	if reason == core.ChatReasonNone {
		reason = core.ChatReasonUpstreamGap
	}
	reason = core.ChatReason(strings.Clone(string(reason)))
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.isStopping() {
		return core.ErrStale
	}
	return h.appendGapLocked(reason, 0)
}

// appendGapLocked публикует отдельное предупреждение о пропуске входа.
func (h *chatHub) appendGapLocked(reason core.ChatReason, lost uint64) error {
	// event содержит готовый пропуск, err сообщает об исчерпании позиции.
	event, err := h.prepareEventLocked(core.ChatEvent{Kind: core.ChatEventGap, Reason: reason, Lost: lost}, nil)
	if err != nil {
		return err
	}
	h.publishLocked(event)
	return nil
}

// captureSnapshot фиксирует конфигурации, статусы, историю и cursor одной позиции.
// Manager передаёт конфигурации, соответствующие той же эпохе, и держит свою видимость до возврата.
func (h *chatHub) captureSnapshot(sources []core.ChatSource) chatSnapshotView {
	h.mu.Lock()
	defer h.mu.Unlock()
	// session содержит независимую копию идентичности реального запуска.
	var session *core.RuntimeSessionFence
	if h.test == nil {
		// fence удерживает точный запуск в возвращаемом снимке.
		fence := h.fence
		session = &fence
	}
	// statuses удерживает копии верхнеуровневых значений до выдачи снимка.
	statuses := make([]core.ChatSourceStatus, 0, len(h.statuses))
	// entry задаёт очередной статус с неизменяемыми указателями на время.
	for _, entry := range h.statuses {
		statuses = append(statuses, entry.status)
	}
	// Анонимный компаратор стабилизирует порядок snapshot по SourceID.
	slices.SortFunc(statuses, func(a, b core.ChatSourceStatus) int {
		return strings.Compare(string(a.SourceID), string(b.SourceID))
	})
	return chatSnapshotView{
		snapshot: core.ChatSnapshot{
			StreamID: h.streamID, Session: session, Sources: slices.Clone(sources), Statuses: statuses,
			Cursor: h.cursor(h.eventOrder),
		},
		history: slices.Clone(h.history.items[h.history.first:]),
		test:    h.test,
	}
}

// startReplayLocked переводит курсор в смещение первого следующего события.
// Отозванная эпоха, повреждённая позиция и пропуск retention требуют нового snapshot.
func (h *chatHub) startReplayLocked(cursor core.ChatCursor) (int, bool) {
	if h.closed || h.testRevoked() {
		return 0, false
	}
	// position содержит проверенную позицию текущей эпохи, ok отражает проверку формата и подписи.
	position, ok := h.key.position(string(h.streamID), cursor, h.limits.CursorBytes)
	if !ok || position > h.eventOrder {
		return 0, false
	}
	if position == h.eventOrder {
		return h.replay.len(), true
	}
	if h.replay.len() == 0 || position < h.replay.at(0).position-1 {
		return 0, false
	}
	return int(position - (h.replay.at(0).position - 1)), true
}

// events возвращает одну ограниченную страницу replay и сигнал изменения той же позиции.
// Потребитель получает независимые данные, даже если исходная история уже вытеснена.
func (h *chatHub) events(ctx context.Context, cursor core.ChatCursor, limit int) (core.ChatEventPage, error) {
	if ctx == nil || limit <= 0 {
		return core.ChatEventPage{}, core.ErrInvalid
	}
	// err сообщает об отмене чтения до получения страницы.
	if err := ctx.Err(); err != nil {
		return core.ChatEventPage{}, err
	}
	h.mu.Lock()
	// err сообщает об отмене во время ожидания согласованной позиции.
	if err := ctx.Err(); err != nil {
		h.mu.Unlock()
		return core.ChatEventPage{}, err
	}
	// first содержит начало replay, ok отличает корректный cursor от требующего resync.
	first, ok := h.startReplayLocked(cursor)
	// result содержит сигнал изменения той же точки и при необходимости указание на новый snapshot.
	result := core.ChatEventPage{Cursor: cursor, Changed: h.changed, ResyncRequired: !ok}
	if !ok {
		result.Cursor = h.cursor(h.eventOrder)
		h.mu.Unlock()
		return result, nil
	}
	// count ограничивает число событий доступным хвостом и запрошенной длиной.
	count := min(limit, h.replay.len()-first)
	result.Events = make([]core.ChatEvent, count)
	// index адресует очередную позицию в странице.
	for index := range result.Events {
		result.Events[index] = h.replay.at(first + index).event
		result.Cursor = result.Events[index].Cursor
		result.ResyncRequired = result.ResyncRequired || result.Events[index].Kind == core.ChatEventResync
	}
	h.mu.Unlock()
	// index адресует событие; event остаётся безопасно неизменяемым до отделения копии.
	for index, event := range result.Events {
		result.Events[index] = chatCopyEvent(event)
	}
	if h.testRevoked() {
		result.Events = nil
		result.ResyncRequired = true
	}
	return result, nil
}

// finish прекращает видимость старого Hub и оставляет его терминал только ранее допущенным подпискам.
// История и pending input освобождаются прежде, чем Done сообщит о завершении.
func (h *chatHub) finish() {
	h.inputMu.Lock()
	// lost содержит отказы до закрытия, bySource распределяет те из них, у которых известен источник.
	lost, bySource := h.takeInputLossLocked()
	h.input = chatBuffer[chatQueuedMessage]{}
	h.inputEstimate = 0
	h.inputMu.Unlock()
	h.mu.Lock()
	if lost != 0 {
		h.publishInputLossLocked(lost, bySource)
	}
	// terminal содержит окончательный номер эпохи для уже выданных подписок.
	h.eventOrder = chatAddLoss(h.eventOrder, 1)
	terminal := core.ChatEvent{Kind: core.ChatEventSessionClosed, SessionID: h.fence.SessionID, Cursor: h.cursor(h.eventOrder)}
	// subscriber задаёт ранее открытый допуск, которому ещё доступен последний кадр.
	for subscriber := range h.subscribers {
		if h.test != nil {
			subscriber.endTestLocked()
		} else {
			subscriber.terminateLocked(terminal)
		}
	}
	h.subscribers = nil
	h.history = chatBuffer[core.ChatMessage]{}
	h.replay = chatBuffer[chatVisibleEvent]{}
	h.statuses = nil
	h.statusBytes = 0
	h.dedupe = nil
	h.dedupeOrder = chatBuffer[chatDedupeEntry]{}
	h.closed = true
	h.signalLocked()
	h.mu.Unlock()
	close(h.done)
}
