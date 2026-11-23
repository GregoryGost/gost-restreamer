package chat

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Snapshot возвращает согласованный курсор, источники, статусы и историю существующего потока.
// Формирование ответа ограничено отдельным числом слотов и не удерживает реестр во время JSON.
func (m *Manager) Snapshot(ctx context.Context, streamID core.ID) (core.ChatSnapshot, error) {
	if streamID.Validate() != nil {
		return core.ChatSnapshot{}, core.ErrInvalid
	}
	select {
	case m.snapshotSlots <- struct{}{}:
		// Callback освобождает отдельную квоту формирования снимков.
		defer func() { <-m.snapshotSlots }()
	default:
		return core.ChatSnapshot{}, core.ErrBusy
	}
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
			return view.build(operation, m.limits)
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
			encoded, err := json.Marshal(chatJSONSource(source))
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
		view.snapshot = core.ChatSnapshot{StreamID: streamID, Sources: sources, Cursor: m.waitingCursorLocked(streamID)}
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
	return m.cursorKey.cursor(string(streamID), m.visibility)
}

// Events читает страницу текущей эпохи либо возвращает сигнал ожидания запуска проверенного Stream.
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
	m.mu.Lock()
	// session выбирает текущую эпоху, err также учитывает отмену медиа-проверки.
	session, err := m.currentStreamLocked(operation, streamID)
	m.mu.Unlock()
	if err != nil {
		return core.ChatEventPage{}, err
	}
	if session != nil {
		return session.hub.events(operation, cursor, limit)
	}
	// Callback отличает действительный предсессионный Stream от неизвестного идентификатора.
	err = m.store.View(operation, func(r core.Repositories) error {
		// readErr передаёт причину отказа чтения метаданных существующего потока.
		_, readErr := r.Config.Stream(operation, streamID)
		return readErr
	})
	if err != nil {
		return core.ChatEventPage{}, err
	}
	m.mu.Lock()
	// session проверяется повторно, если запуск появился за время чтения конфигурации.
	session, err = m.currentStreamLocked(operation, streamID)
	// waiting привязывает курсор ожидания к этому потоку и позиции смены эпох.
	waiting := m.waitingCursorLocked(streamID)
	// changed соответствует той же позиции и закрывается при смене видимости.
	changed := m.changed
	m.mu.Unlock()
	if err != nil {
		return core.ChatEventPage{}, err
	}
	if session != nil {
		return session.hub.events(operation, cursor, limit)
	}
	return core.ChatEventPage{Cursor: waiting, Changed: changed, ResyncRequired: cursor != "" && cursor != waiting}, nil
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
	// session содержит допущенную эпоху, err сообщает об отзыве или отмене проверки.
	session, err := m.currentStreamLocked(operation, streamID)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, core.ErrNotReady
	}
	// subscription удерживает отдельную очередь, err сообщает об исчерпании лимита либо смене эпохи.
	subscription, err := session.hub.subscribe(operation, cursor)
	if err != nil {
		return nil, err
	}
	m.subscriptions[streamID]++
	m.subscribers++
	// Callback возвращает общий и потоковый слоты после физического освобождения клиентской очереди.
	subscription.release = func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.subscribers--
		m.subscriptions[streamID]--
		if m.subscriptions[streamID] == 0 {
			delete(m.subscriptions, streamID)
		}
	}
	return subscription, nil
}
