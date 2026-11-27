package chat

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/chatwire"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Snapshot возвращает согласованный курсор, источники, статусы и историю существующего потока.
// Квота освобождается после формирования результата для внутреннего Go-потребителя.
func (m *Manager) Snapshot(ctx context.Context, streamID core.ID) (core.ChatSnapshot, error) {
	// snapshot содержит согласованную ленту, release возвращает квоту, err сообщает об отказе чтения.
	snapshot, release, err := m.SnapshotResponse(ctx, streamID)
	if err != nil {
		return core.ChatSnapshot{}, err
	}
	release()
	return snapshot, nil
}

// SnapshotResponse возвращает согласованный снимок и передачу владения его слотом HTTP-слою.
// После успеха вызывающий освобождает слот только после завершения сериализации и записи.
func (m *Manager) SnapshotResponse(ctx context.Context, streamID core.ID) (core.ChatSnapshot, func(), error) {
	if streamID.Validate() != nil {
		return core.ChatSnapshot{}, nil, core.ErrInvalid
	}
	select {
	case m.snapshotSlots <- struct{}{}:
	default:
		return core.ChatSnapshot{}, nil, core.ErrBusy
	}
	// snapshot содержит ограниченное представление, err сообщает об отказе его получить.
	snapshot, err := m.readSnapshot(ctx, streamID)
	if err != nil {
		<-m.snapshotSlots
		return core.ChatSnapshot{}, nil, err
	}
	// Функция освобождает занятое место после использования выданного снимка.
	return snapshot, func() { <-m.snapshotSlots }, nil
}

// readSnapshot читает конфигурацию и фиксирует одну согласованную позицию.
// Вызывающий уже удерживает слот, поэтому запись в Store или задержка HTTP не допускают лишние снимки.
func (m *Manager) readSnapshot(ctx context.Context, streamID core.ID) (core.ChatSnapshot, error) {
	// operation отслеживает чтение конфигурации, finish освобождает работу, err сообщает об отмене.
	operation, finish, err := m.beginOperation(ctx)
	if err != nil {
		return core.ChatSnapshot{}, err
	}
	defer finish()
	for operation.Err() == nil {
		m.mu.Lock()
		// changes связывает чтение Store с поколением конфигурации, changed будит после CAS.
		changes, changed := m.changes, m.changed
		// blocked сообщает, что согласованность снимка требует дождаться результата конфигурационного CAS.
		blocked := m.blockedSource != "" || m.blockedAccount != ""
		// retained содержит активные Source по снимкам Output даже после переноса Destination.
		retained := snapshotMemberIDs(m.sessions[streamID])
		m.mu.Unlock()
		if blocked {
			select {
			case <-changed:
				continue
			case <-operation.Done():
				return core.ChatSnapshot{}, operation.Err()
			}
		}
		// configuration содержит ограниченную конфигурацию Stream и участников снимка выходов.
		configuration, err := m.readSnapshotSources(operation, streamID, retained)
		if err != nil {
			return core.ChatSnapshot{}, err
		}
		// view содержит согласованный с курсором набор, retry сообщает о смене конфигурации.
		view, retry, err := m.captureStream(operation, streamID, configuration, changes)
		if err != nil {
			return core.ChatSnapshot{}, err
		}
		if !retry {
			view.snapshot.Statuses = completeSnapshotStatuses(view.snapshot.Sources, view.snapshot.Statuses)
			// snapshot содержит отделённое представление, err сообщает об отказе проверки бюджета.
			snapshot, err := view.build(operation, m.limits)
			if chatTestRevoked(view.test) {
				continue
			}
			return snapshot, err
		}
	}
	return core.ChatSnapshot{}, operation.Err()
}

// snapshotMemberIDs копирует стабильные ID зарегистрированной эпохи под блокировкой Manager.
func snapshotMemberIDs(session *managedSession) []core.ID {
	if session == nil {
		return nil
	}
	// result адресует активные привязки к сохранённым снимкам выходов.
	result := make([]core.ID, 0, len(session.members))
	// sourceID идентифицирует одного участника данной эпохи.
	for sourceID := range session.members {
		result = append(result, sourceID)
	}
	slices.Sort(result)
	return result
}

// snapshotSources разделяет текущую конфигурацию потока и участников его снимков выхода.
type snapshotSources struct {
	// values содержит сначала конфигурацию Stream, затем остальные активные по снимкам выхода Source.
	values []core.ChatSource
	// configured отмечает конец текущей конфигурации Stream.
	configured int
}

// readSnapshotSources материализует только помещающиеся в бюджет метаданные.
// retained добавляет участников по снимку выхода, чьё назначение теперь принадлежит другому Stream.
func (m *Manager) readSnapshotSources(ctx context.Context, streamID core.ID, retained []core.ID) (snapshotSources, error) {
	// sources принимает конфигурацию снимка в порядке чтения.
	var sources snapshotSources
	// err сообщает о завершении View, callback держит Stream и его Sources в одном снимке Store.
	err := m.store.View(ctx, func(r core.Repositories) error {
		// err отличает отсутствующий Stream и ошибку его чтения от пустой конфигурации чатов.
		if _, err := r.Config.Stream(ctx, streamID); err != nil {
			return err
		}
		// occupied учитывает добавленные записи, seen отличает уже материализованные Source.
		var occupied uint64
		seen := make(map[core.ID]struct{})
		// add проверяет открытый JSON перед включением очередной конфигурации.
		add := func(source core.ChatSource) error {
			// encoded содержит открытые настройки, err сообщает о невозможности их представить.
			encoded, err := json.Marshal(chatwire.Source(source))
			if err != nil {
				return core.ErrInvalid
			}
			// size учитывает очередной разделитель массива независимо от будущего хвоста истории.
			size := uint64(len(encoded)) + 1
			if size > m.limits.SnapshotMetadataBytes-occupied {
				return core.ErrNotReady
			}
			occupied += size
			sources.values = append(sources.values, source)
			seen[source.ID] = struct{}{}
			return nil
		}
		// query ограничивает материализацию каждой страницы текущих назначений Stream.
		query := core.ChatSourceQuery{StreamID: streamID, Limit: managerPageSize}
		for {
			// page содержит очередную страницу конфигурации, err передаёт причину отказа её чтения.
			page, err := r.Chat.ChatSources(ctx, query)
			if err != nil {
				return err
			}
			// source добавляется только после проверки размера открытой проекции.
			for _, source := range page {
				if err = add(source); err != nil {
					return err
				}
			}
			if len(page) < query.Limit {
				break
			}
			// last задаёт исключительную границу для чтения следующей страницы.
			last := page[len(page)-1]
			query.After = &core.ListBoundary{CreatedAt: last.CreatedAt, ID: last.ID}
		}
		sources.configured = len(sources.values)
		// sourceID адресует оставшийся источник живого снимка выхода этой эпохи.
		for _, sourceID := range retained {
			if _, exists := seen[sourceID]; exists {
				continue
			}
			// source содержит конфигурацию исходной привязки, err сообщает об удалении или отказе чтения.
			source, err := r.Chat.ChatSource(ctx, sourceID)
			if errors.Is(err, core.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if err = add(source); err != nil {
				return err
			}
		}
		return nil
	})
	return sources, err
}

// captureStream фиксирует точную эпоху и позицию после подтверждения родителя под общей RuntimeAuthority.
func (m *Manager) captureStream(ctx context.Context, streamID core.ID, configuration snapshotSources, changes uint64) (chatSnapshotView, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if changes != m.changes {
		return chatSnapshotView{}, true, nil
	}
	// session содержит точную эпоху либо nil, err сообщает о причине невозможности её проверки.
	session, err := m.currentStreamLocked(ctx, streamID)
	if err != nil {
		return chatSnapshotView{}, false, err
	}
	// sources начинается с общей конфигурации Stream независимо от точной эпохи.
	sources := configuration.values[:configuration.configured]
	// view выбирается без выполнения JSON и удерживает независимые копии верхнеуровневых полей.
	var view chatSnapshotView
	if session == nil {
		// feed удерживает тестовую историю только пока действует допуск до эфира.
		if feed := m.currentTestLocked(streamID); feed != nil {
			view = feed.hub.captureSnapshot(sources)
		} else {
			view.snapshot = core.ChatSnapshot{StreamID: streamID, Sources: sources, Cursor: m.waitingCursorLocked(streamID)}
		}
	} else {
		// source из дополнительного хвоста включается только при действующей привязке этой эпохи.
		for _, source := range configuration.values[configuration.configured:] {
			if session.members[source.ID] != nil {
				sources = append(sources, source)
			}
		}
		// sourceID связывает каждый видимый привязанный Source с той же конфигурационной проекцией.
		for sourceID := range session.members {
			// Callback определяет, дошло ли чтение до метаданных текущей оперативной привязки.
			if !slices.ContainsFunc(sources, func(source core.ChatSource) bool { return source.ID == sourceID }) {
				m.requestReconcile()
				return chatSnapshotView{}, true, nil
			}
		}
		view = session.hub.captureSnapshot(sources)
	}
	return view, false, nil
}

// currentStreamLocked проверяет точного родителя общей медиа-блокировкой даже у эпохи без выходов.
func (m *Manager) currentStreamLocked(ctx context.Context, streamID core.ID) (*managedSession, error) {
	// session является удерживаемой эпохой этого Stream, а не только долговечным Session ID.
	session := m.sessions[streamID]
	if session == nil {
		return nil, nil
	}
	// err отражает отзыв точного родителя или отмену получения допуска.
	err := m.service.CheckChatPublications(ctx, session.owner, nil, nil)
	if errors.Is(err, core.ErrStale) {
		m.retireSessionLocked(session)
		m.requestReconcile()
		return nil, nil
	}
	return session, err
}

// completeSnapshotStatuses добавляет ожидание и отключение для настроенных источников без живого статуса.
func completeSnapshotStatuses(sources []core.ChatSource, active []core.ChatSourceStatus) []core.ChatSourceStatus {
	// activeByID содержит только видимые статусы текущей позиции, включая сохранённые остановки.
	activeByID := make(map[core.ID]core.ChatSourceStatus, len(active))
	// status принадлежит одной оперативной привязке этой эпохи.
	for _, status := range active {
		activeByID[status.SourceID] = status
	}
	// result содержит ровно по одному статусу на конфигурацию в снимке.
	result := make([]core.ChatSourceStatus, 0, len(sources))
	// source определяет статус до допуска чтения и исключает удалённый статус из ответа.
	for _, source := range sources {
		// status может уже содержать состояние точного участника и его публичные отметки.
		status := activeByID[source.ID]
		if !source.Enabled {
			status = core.ChatSourceStatus{SourceID: source.ID, State: core.ChatDisabled, ChangedAt: source.UpdatedAt}
		} else if status.SourceID == "" {
			status = core.ChatSourceStatus{SourceID: source.ID, State: core.ChatWaitingOutput, ChangedAt: source.UpdatedAt}
		}
		result = append(result, status)
	}
	// Callback стабилизирует статусы снимка по идентичности настроенного источника.
	slices.SortFunc(result, func(a, b core.ChatSourceStatus) int { return strings.Compare(string(a.SourceID), string(b.SourceID)) })
	return result
}

// waitingCursorLocked подписывает позицию Stream вне активной эпохи Hub.
func (m *Manager) waitingCursorLocked(streamID core.ID) core.ChatCursor {
	// marker даёт независимую позицию очистки только для этого Stream.
	if marker := m.waitingClears[streamID]; marker.last != 0 {
		return m.cursorKey.cursor(waitingClearScope(streamID), marker.last)
	}
	return m.cursorKey.cursor(string(streamID), m.visibility)
}

// Events читает страницу рабочей или тестовой эпохи либо возвращает позицию ожидания проверенного Stream.
func (m *Manager) Events(ctx context.Context, streamID core.ID, cursor core.ChatCursor, limit int) (core.ChatEventPage, error) {
	if streamID.Validate() != nil || limit <= 0 {
		return core.ChatEventPage{}, core.ErrInvalid
	}
	select {
	case m.eventSlots <- struct{}{}:
		// Callback возвращает место чтения replay после фактического ответа.
		defer func() { <-m.eventSlots }()
	default:
		return core.ChatEventPage{}, core.ErrBusy
	}
	// operation отслеживает фактически выполняемое чтение, finish освобождает допуск, err проверяет срок.
	operation, finish, err := m.beginOperation(ctx)
	if err != nil {
		return core.ChatEventPage{}, err
	}
	defer finish()
	// known отмечает, что Store подтвердил существование предсессионного Stream.
	for known := false; ; known = true {
		m.mu.Lock()
		// hub адресует текущую ленту, effective переводит курсор ожидания в тестовую эпоху.
		// waiting содержит управляющую позицию при отсутствии ленты, err сообщает об отказе проверки.
		hub, effective, waiting, err := m.eventRouteLocked(operation, streamID, cursor)
		m.mu.Unlock()
		if err != nil {
			return core.ChatEventPage{}, err
		}
		if hub != nil {
			return hub.events(operation, effective, limit)
		}
		if known {
			return waiting, nil
		}
		if err = m.checkChatStream(operation, streamID); err != nil {
			return core.ChatEventPage{}, err
		}
	}
}

// Subscribe выдаёт клиенту отдельную ограниченную очередь уже активной эпохи.
// До появления эпохи вызывающий ожидает сигнал из Events. Каждую выданную очередь закрывает клиент.
func (m *Manager) Subscribe(ctx context.Context, streamID core.ID, cursor core.ChatCursor) (*ChatSubscription, error) {
	if ctx == nil || streamID.Validate() != nil {
		return nil, core.ErrInvalid
	}
	select {
	case m.eventSlots <- struct{}{}:
		// Callback освобождает регистрацию независимо от срока выданной очереди.
		defer func() { <-m.eventSlots }()
	default:
		return nil, core.ErrBusy
	}
	// operation отменяется при закрытии Manager, finish подтверждает выход, err сообщает о недопуске.
	operation, finish, err := m.beginOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return nil, core.ErrClosed
	}
	if m.subscribers >= m.limits.SSEClients || m.subscriptions[streamID] >= m.limits.SSEClientsPerStream {
		return nil, core.ErrBusy
	}
	// hub содержит допущенную эпоху, effective учитывает переход из ожидания, err сообщает об отмене проверки.
	hub, effective, _, err := m.eventRouteLocked(operation, streamID, cursor)
	if err != nil {
		return nil, err
	}
	if hub == nil {
		return nil, core.ErrNotReady
	}
	// subscription удерживает отдельную очередь, err сообщает об исчерпании лимита либо смене эпохи.
	subscription, err := hub.subscribe(operation, effective)
	if err != nil {
		return nil, err
	}
	m.subscriptions[streamID]++
	m.subscribers++
	// Callback возвращает общий и потоковый слоты после физического освобождения клиентской очереди.
	subscription.release = func() { m.releaseEventClient(streamID) }
	return subscription, nil
}

// ContinueEventStream заменяет ожидание подпиской в том же HTTP-допуске. Вызывающий
// закрывает выданную очередь и сохраняет исходную функцию освобождения места.
func (m *Manager) ContinueEventStream(ctx context.Context, streamID core.ID, cursor core.ChatCursor) (ChatStreamStart, error) {
	if streamID.Validate() != nil || cursor == "" {
		return ChatStreamStart{}, core.ErrInvalid
	}
	m.mu.Lock()
	// admitted показывает наличие удерживаемого места для продолжения этого Stream.
	admitted := m.subscriptions[streamID] != 0
	m.mu.Unlock()
	if !admitted {
		return ChatStreamStart{}, core.ErrClosed
	}
	return m.startEventStream(ctx, streamID, cursor)
}

// ChatStreamStart содержит начальную границу SSE. При ожидании запуска заполнена
// только Waiting, при активной эпохе события поступают через Subscription.
type ChatStreamStart struct {
	// Subscription удерживает очередь той же эпохи, из которой выбран исходный курсор.
	Subscription *ChatSubscription
	// Waiting содержит согласованный курсор и сигнал об изменении видимости потока.
	Waiting core.ChatEventPage
}

// OpenEventStream занимает общий и потоковый слоты до поиска Session. При успехе
// возвращённую функцию вызывает HTTP после завершения всего ответа, включая ожидание до Session.
// Отсутствующий cursor активного потока выбирается под блокировкой Hub при регистрации очереди.
func (m *Manager) OpenEventStream(ctx context.Context, streamID core.ID, cursor core.ChatCursor) (ChatStreamStart, func(), error) {
	if ctx == nil || streamID.Validate() != nil {
		return ChatStreamStart{}, nil, core.ErrInvalid
	}
	m.mu.Lock()
	if m.ctx.Err() != nil {
		m.mu.Unlock()
		return ChatStreamStart{}, nil, core.ErrClosed
	}
	if m.subscribers >= m.limits.SSEClients || m.subscriptions[streamID] >= m.limits.SSEClientsPerStream {
		m.mu.Unlock()
		return ChatStreamStart{}, nil, core.ErrBusy
	}
	m.subscriptions[streamID]++
	m.subscribers++
	m.mu.Unlock()
	// start содержит очередь либо границу ожидания, err сообщает об отказе её открытия.
	start, err := m.startEventStream(ctx, streamID, cursor)
	if err != nil {
		m.releaseEventClient(streamID)
		return ChatStreamStart{}, nil, err
	}
	// Функция закрывает очередь перед освобождением клиентских слотов.
	return start, func() {
		if start.Subscription != nil {
			start.Subscription.Close()
		}
		m.releaseEventClient(streamID)
	}, nil
}

// startEventStream подтверждает существование Stream и атомарно выбирает очередь
// действующей эпохи либо границу ожидания. При чтении Store блокировка Manager свободна.
func (m *Manager) startEventStream(ctx context.Context, streamID core.ID, cursor core.ChatCursor) (ChatStreamStart, error) {
	// operation отслеживает предварительную проверку, finish возвращает допуск, err сообщает об отказе.
	operation, finish, err := m.beginOperation(ctx)
	if err != nil {
		return ChatStreamStart{}, err
	}
	defer finish()
	// known отмечает, что Store уже подтвердил существование потока без активного владельца.
	known := false
	for {
		m.mu.Lock()
		// hub задаёт действующую ленту, effective учитывает переход в тестовую эпоху.
		// waiting связывает курсор с управляющим событием либо сигналом ожидания, err отражает отказ.
		hub, effective, waiting, err := m.eventRouteLocked(operation, streamID, cursor)
		if err != nil {
			m.mu.Unlock()
			return ChatStreamStart{}, err
		}
		if hub != nil {
			// subscription содержит установленную очередь, err сообщает об отмене или отзыве эпохи.
			subscription, err := hub.subscribeFrom(operation, effective, true)
			m.mu.Unlock()
			if errors.Is(err, core.ErrStale) && hub.test != nil {
				continue
			}
			return ChatStreamStart{Subscription: subscription}, err
		}
		if known {
			m.mu.Unlock()
			return ChatStreamStart{Waiting: waiting}, nil
		}
		m.mu.Unlock()
		err = m.checkChatStream(operation, streamID)
		if err != nil {
			return ChatStreamStart{}, err
		}
		known = true
	}
}

// eventRouteLocked выбирает ленту и её начальный курсор под одной блокировкой Manager.
// Если тестовая эпоха уже доступна, оставшийся предсессионный chat.cleared всё ещё выдаётся первым.
func (m *Manager) eventRouteLocked(ctx context.Context, streamID core.ID, cursor core.ChatCursor) (*chatHub, core.ChatCursor, core.ChatEventPage, error) {
	// session содержит проверенного владельца реального Hub, err передаёт отказ медиа-проверки.
	session, err := m.currentStreamLocked(ctx, streamID)
	if err != nil {
		return nil, "", core.ChatEventPage{}, err
	}
	if session != nil {
		return session.hub, cursor, core.ChatEventPage{}, nil
	}
	// feed содержит текущий тестовый Hub при действующем допуске.
	feed := m.currentTestLocked(streamID)
	// event содержит доступную предсессионную очистку, ok означает, что клиент её ещё не читал.
	if event, ok := m.waitingClearEventLocked(streamID, cursor); ok {
		// changed позволяет немедленно повторить выбор, если тестовая эпоха уже создана.
		changed := (<-chan struct{})(m.changed)
		if feed != nil {
			changed = readyRuntimeWake()
		}
		return nil, "", core.ChatEventPage{Events: []core.ChatEvent{event}, Cursor: event.Cursor, Changed: changed}, nil
	}
	if feed != nil {
		if cursor == feed.waiting {
			cursor = feed.hub.cursor(0)
		}
		return feed.hub, cursor, core.ChatEventPage{}, nil
	}
	return nil, "", m.waitingEventPageLocked(streamID, cursor), nil
}

// waitingEventPageLocked связывает курсор и сигнал, пока их позиция защищена блокировкой Manager.
func (m *Manager) waitingEventPageLocked(streamID core.ID, cursor core.ChatCursor) core.ChatEventPage {
	// waiting привязывает позицию ожидания к идентичности потока.
	waiting := m.waitingCursorLocked(streamID)
	return core.ChatEventPage{Cursor: waiting, Changed: m.changed, ResyncRequired: cursor != "" && cursor != waiting}
}

// releaseEventClient возвращает занятые места после физического выхода подписчика или отказа допуска.
func (m *Manager) releaseEventClient(streamID core.ID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subscribers--
	m.subscriptions[streamID]--
	if m.subscriptions[streamID] == 0 {
		delete(m.subscriptions, streamID)
		delete(m.waitingClears, streamID)
	}
}
