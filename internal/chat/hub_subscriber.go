package chat

import (
	"context"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// ChatSubscription удерживает ограниченную очередь одного допущенного читателя.
// Даже отставший читатель может получить терминал той эпохи, к которой был допущен.
// Владелец читателя вызывает Close после исчерпания событий либо отмены подписки.
type ChatSubscription struct {
	// hub защищает состояние подписки своей блокировкой видимости.
	hub *chatHub
	// queue удерживает только непрочитанные события в пределах бюджета подписчика.
	queue chatBuffer[chatVisibleEvent]
	// cursor содержит последнюю выданную позицию.
	cursor core.ChatCursor
	// resync содержит ожидающее предупреждение после отставания либо некорректного курсора.
	resync *core.ChatEvent
	// lagged прекращает обычную доставку после resync до повторного открытия подписки.
	lagged bool
	// terminal содержит отдельный завершающий кадр независимо от бюджета обычной очереди.
	terminal *core.ChatEvent
	// changed будит ожидающий Events при появлении данных, отставании и отзыве.
	changed chan struct{}
	// done сигнализирует о завершении эпохи или закрытии читателя.
	done chan struct{}
	// ended показывает, что done уже закрыт и новых обычных событий не будет.
	ended bool
	// released показывает, что владелец вызвал Close и освободил свой допуск.
	released bool
	// release освобождает внешний слот Manager после снятия блокировки hub.
	release func()
}

// subscribe атомарно проверяет replay и допускает подписчика в журнал этой же эпохи.
// Внутренний Go-потребитель передаёт собственную позицию, даже если она пуста.
func (h *chatHub) subscribe(ctx context.Context, cursor core.ChatCursor) (*ChatSubscription, error) {
	return h.subscribeFrom(ctx, cursor, false)
}

// subscribeFrom выбирает текущую позицию под той же блокировкой, что и добавление
// очереди, если HTTP-потребитель не передал курсор. Отставание при чтении replay
// обозначается тем же событием, что и последующее переполнение.
func (h *chatHub) subscribeFrom(ctx context.Context, cursor core.ChatCursor, fromCurrent bool) (*ChatSubscription, error) {
	if ctx == nil {
		return nil, core.ErrInvalid
	}
	// err сообщает об отмене до попытки занять слот читателя.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// err сообщает об отмене во время ожидания свободной видимости.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if h.closed || h.isStopping() || h.testRevoked() {
		return nil, core.ErrStale
	}
	if len(h.subscribers) >= h.limits.SSEClientsPerStream {
		return nil, core.ErrBusy
	}
	if fromCurrent && cursor == "" {
		cursor = h.cursor(h.eventOrder)
	}
	// subscriber получает отдельную ограниченную очередь и сигналы.
	subscriber := &ChatSubscription{hub: h, cursor: cursor, changed: make(chan struct{}), done: make(chan struct{})}
	// first содержит позицию replay, ok показывает, что читатель может продолжить с неё.
	first, ok := h.startReplayLocked(cursor)
	if !ok {
		subscriber.markLagLocked(h.cursor(h.eventOrder), core.ChatReasonNone)
	} else {
		// index проходит непрочитанный хвост до первого переполнения клиентской очереди.
		for index := first; index < h.replay.len() && !subscriber.lagged; index++ {
			subscriber.offerLocked(h.replay.at(index))
		}
	}
	h.subscribers[subscriber] = struct{}{}
	return subscriber, nil
}

// offerLocked добавляет обычное событие либо переводит читателя в одноразовый resync.
func (s *ChatSubscription) offerLocked(event chatVisibleEvent) {
	if s.lagged || s.ended {
		return
	}
	if event.event.Kind == core.ChatEventResync {
		s.markLagLocked(event.event.Cursor, event.event.Reason)
		return
	}
	if event.frameBytes > s.hub.limits.SSEFrameBytes || !s.queue.canPush(event.logicalBytes, s.hub.limits.SSEQueue) {
		s.markLagLocked(event.event.Cursor, core.ChatReasonLimit)
		return
	}
	s.queue.push(event, event.logicalBytes)
	s.signalLocked()
}

// markLagLocked освобождает отставший хвост и сохраняет запрос на новый снимок независимо от очереди.
func (s *ChatSubscription) markLagLocked(cursor core.ChatCursor, reason core.ChatReason) {
	s.queue = chatBuffer[chatVisibleEvent]{}
	s.lagged = true
	s.resync = &core.ChatEvent{Kind: core.ChatEventResync, Cursor: cursor, SessionID: s.hub.fence.SessionID, Reason: reason}
	s.signalLocked()
}

// signalLocked будит читателя при изменении его очереди.
func (s *ChatSubscription) signalLocked() {
	close(s.changed)
	if !s.ended {
		s.changed = make(chan struct{})
	}
}

// terminateLocked сохраняет последний кадр для ранее допущенного читателя и сообщает о завершении.
func (s *ChatSubscription) terminateLocked(terminal core.ChatEvent) {
	s.terminal = &terminal
	s.endLocked()
}

// endTestLocked отбрасывает хвост отозванной тестовой ленты и оставляет один resync.
// Повторная остановка не заменяет уже прочитанный управляющий кадр.
func (s *ChatSubscription) endTestLocked() {
	if s.ended {
		return
	}
	s.queue = chatBuffer[chatVisibleEvent]{}
	s.resync = nil
	s.terminateLocked(core.ChatEvent{Kind: core.ChatEventResync, Cursor: s.hub.cursor(s.hub.eventOrder)})
}

// discardMessagesLocked исключает непрочитанные сообщения до очистки, сохраняя порядок остальных кадров.
func (s *ChatSubscription) discardMessagesLocked() {
	// kept получает только кадры, которые не содержат удаляемую историю.
	var kept chatBuffer[chatVisibleEvent]
	// item содержит событие и уже измеренный вес для клиентской очереди.
	for _, item := range s.queue.items[s.queue.first:] {
		if item.value.event.Kind != core.ChatEventMessage {
			kept.push(item.value, item.size)
		}
	}
	s.queue = kept
}

// Current проверяет перед отправкой, что уже извлечённый кадр пережил очистку
// и что тестовый допуск не отозван. Сетевой транспорт работает вне блокировки Hub.
func (s *ChatSubscription) Current(event core.ChatEvent) bool {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	if s.hub.testRevoked() {
		return event.Kind == core.ChatEventResync
	}
	return event.Message == nil || event.Message.Seq > s.hub.clearedThrough
}

// endLocked закрывает сигналы подписчика один раз независимо от порядка остановки клиента и Hub.
func (s *ChatSubscription) endLocked() {
	if !s.ended {
		s.ended = true
		close(s.done)
		s.signalLocked()
	}
}

// Events выдаёт до limit событий и сигнал, будящий ожидающего читателя.
// После закрытия Done доступные кадры, включая session_closed, остаются читаемыми до вызова Close.
func (s *ChatSubscription) Events(ctx context.Context, limit int) (core.ChatEventPage, error) {
	if ctx == nil || limit <= 0 {
		return core.ChatEventPage{}, core.ErrInvalid
	}
	// err сообщает об отмене ожидания до чтения очереди.
	if err := ctx.Err(); err != nil {
		return core.ChatEventPage{}, err
	}
	s.hub.mu.Lock()
	// err сообщает об отмене во время ожидания очереди.
	if err := ctx.Err(); err != nil {
		s.hub.mu.Unlock()
		return core.ChatEventPage{}, err
	}
	if s.released {
		s.hub.mu.Unlock()
		return core.ChatEventPage{}, core.ErrClosed
	}
	if s.hub.testRevoked() {
		s.endTestLocked()
	}
	// result хранит кадры одной страницы и соответствующий сигнал изменения.
	result := core.ChatEventPage{Cursor: s.cursor, Changed: s.changed}
	if s.resync != nil {
		result.Events = append(result.Events, *s.resync)
		result.ResyncRequired = true
		s.resync = nil
	}
	for s.queue.len() != 0 && len(result.Events) < limit {
		result.Events = append(result.Events, s.queue.pop().event)
	}
	if len(result.Events) < limit && s.terminal != nil {
		result.Events = append(result.Events, *s.terminal)
		s.terminal = nil
	}
	if len(result.Events) != 0 {
		s.cursor = result.Events[len(result.Events)-1].Cursor
		result.Cursor = s.cursor
	}
	s.hub.mu.Unlock()
	// index адресует кадр, event содержит данные, которые ещё могут использовать другие читатели.
	for index, event := range result.Events {
		result.Events[index] = chatCopyEvent(event)
	}
	return result, nil
}

// Done сигнализирует о завершении точной эпохи либо закрытии подписки владельцем.
func (s *ChatSubscription) Done() <-chan struct{} { return s.done }

// Close освобождает непрочитанные данные и возвращает внешний слот, если его выделил Manager.
func (s *ChatSubscription) Close() {
	s.hub.mu.Lock()
	if s.released {
		s.hub.mu.Unlock()
		return
	}
	s.released = true
	delete(s.hub.subscribers, s)
	s.queue = chatBuffer[chatVisibleEvent]{}
	s.resync = nil
	s.terminal = nil
	s.endLocked()
	// release освобождает внешнюю квоту без обратного захвата Manager под блокировкой Hub.
	release := s.release
	s.release = nil
	s.hub.mu.Unlock()
	if release != nil {
		release()
	}
}
