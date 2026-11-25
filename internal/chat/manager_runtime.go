package chat

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// managerPageSize ограничивает каждую материализованную страницу конфигурации.
const managerPageSize = 64

// runtimeTopology содержит данные одного согласованного чтения для допущенного родителя.
type runtimeTopology struct {
	// fence и owner связывают данные с точной оперативной эпохой.
	fence core.RuntimeSessionFence
	owner *core.ChatSessionOwner
	// streamID задаёт владельца ленты в конфигурации.
	streamID core.ID
	// candidates хранит ограниченный набор активных привязок в порядке допуска.
	candidates []topologyMember
}

// topologyMember связывает сохранённую конфигурацию с текущими доказательствами выходов.
type topologyMember struct {
	// source содержит конфигурацию, а outputs перечисляет доказательства её Destination.
	source  core.ChatSource
	outputs []*core.ChatOutputPublication
}

// run сверяет начальное состояние и обрабатывает оперативный журнал до остановки Manager.
// Изменения метрик сдвигают курсор без обращения к Store. Смена канала ChatChanged, пропуск
// журнала и подтверждённое изменение конфигурации требуют новой полной сверки.
func (m *Manager) run() {
	defer m.stopSessions()
	// cursor содержит последнюю прочитанную позицию, а wakeRuntime и chatChanged указывают каналы
	// соответствующей эпохи журнала. reconcile запрашивает новую пару RuntimeAnchor и View.
	var cursor core.RuntimeCursor
	var wakeRuntime, chatChanged <-chan struct{}
	reconcile := true
	for m.ctx.Err() == nil {
		if reconcile {
			// err сообщает о временно недоступном Store или об отзыве снимка во время чтения.
			var err error
			cursor, wakeRuntime, chatChanged, err = m.reconcileRuntime()
			if err != nil {
				if errors.Is(err, core.ErrStale) {
					continue
				}
				// timer ограничивает частоту повторов после ошибки чтения даже без новых событий.
				timer := time.NewTimer(m.limits.ReconnectMinBackoff)
				select {
				case <-timer.C:
				case <-m.wake:
				case <-m.ctx.Done():
				}
				timer.Stop()
				continue
			}
			reconcile = false
		}
		select {
		case <-m.ctx.Done():
			return
		case <-m.wake:
			reconcile = true
		case <-chatChanged:
			reconcile = true
		case <-wakeRuntime:
			// page содержит дочитанную границу журнала, err сообщает о невозможности чтения.
			page, err := m.drainRuntime(cursor, chatChanged)
			if err != nil {
				reconcile = true
				continue
			}
			cursor, wakeRuntime = page.Cursor, page.Changed
			if page.Cursor != page.HighWater {
				wakeRuntime = readyRuntimeWake()
			}
		}
	}
}

// readyRuntimeWake немедленно разрешает дочитать события, которые успели появиться за исходной границей.
func readyRuntimeWake() <-chan struct{} {
	// ready представляет уже известную работу и не удерживает ожидающую горутину.
	ready := make(chan struct{})
	close(ready)
	return ready
}

// stopSessions отзывает все эпохи после отмены общего контекста.
func (m *Manager) stopSessions() {
	m.mu.Lock()
	defer m.mu.Unlock()
	// session относится к одной из текущих эпох, оставшихся при остановке.
	for _, session := range m.sessions {
		m.retireSessionLocked(session)
	}
	// feed адресует одну тестовую эпоху, которую закрывают вместе с Manager.
	for _, feed := range m.testFeeds {
		m.retireTestLocked(feed)
	}
	m.signalChangedLocked()
}

// drainRuntime дочитывает фиксированный HighWater первого чтения. Страница из одного события
// допустима при любом настроенном бюджете событий. Изменение ChatChanged отзывает весь снимок.
func (m *Manager) drainRuntime(cursor core.RuntimeCursor, chatChanged <-chan struct{}) (core.RuntimeEventPage, error) {
	// target сохраняет начальную верхнюю границу, чтобы частые метрики не задерживали сверку.
	var target core.RuntimeCursor
	for {
		// page содержит продолжение журнала, err сообщает о недоступности RuntimeAuthority.
		page, err := m.service.RuntimeEvents(m.ctx, cursor, 1)
		if err != nil {
			return core.RuntimeEventPage{}, err
		}
		if page.Gap || page.Reset || page.ChatChanged != chatChanged {
			return core.RuntimeEventPage{}, core.ErrStale
		}
		if target == (core.RuntimeCursor{}) {
			target = page.HighWater
		}
		if page.Cursor.Sequence >= target.Sequence {
			return page, nil
		}
		cursor = page.Cursor
	}
}

// reconcileRuntime соединяет согласованный RAM-снимок и один View, затем проверяет журнал
// и конфигурационное поколение до применения новых привязок. Если родитель ещё наблюдает
// публиковавший выход, View определяет, покрывает ли он источник с отозванными выходами.
func (m *Manager) reconcileRuntime() (core.RuntimeCursor, <-chan struct{}, <-chan struct{}, error) {
	// anchor хранит точные наблюдения, err сообщает об ошибке чтения оперативного состояния.
	anchor, err := m.service.RuntimeAnchor(m.ctx)
	if err != nil {
		return core.RuntimeCursor{}, nil, nil, err
	}
	m.mu.Lock()
	// changes удостоверяет конфигурацию на время чтения.
	changes := m.changes
	// current сохраняет приоритет удерживаемых привязок, а err обнаруживает отозванный снимок.
	current, err := m.pruneRuntimeLocked(anchor)
	m.mu.Unlock()
	if err != nil {
		return core.RuntimeCursor{}, nil, nil, err
	}
	// topologies объединяет результаты View, а err также сообщает об ошибке любой его части.
	topologies, err := m.readTopology(anchor, current)
	if err != nil {
		m.mu.Lock()
		m.retireUncoveredLocked()
		m.mu.Unlock()
		return core.RuntimeCursor{}, nil, nil, err
	}
	// page удостоверяет, что прочитанное состояние не прервано жизненным событием или пропуском.
	page, err := m.drainRuntime(anchor.Cursor, anchor.ChatChanged)
	if err != nil {
		return core.RuntimeCursor{}, nil, nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if changes != m.changes {
		return core.RuntimeCursor{}, nil, nil, core.ErrStale
	}
	select {
	case <-anchor.ChatChanged:
		return core.RuntimeCursor{}, nil, nil, core.ErrStale
	default:
	}
	m.applyTopologyLocked(topologies)
	close(m.topologyChanged)
	m.topologyChanged = make(chan struct{})
	// wake удерживает ожидание журнала либо немедленно продолжает уже наблюдавшуюся очередь.
	wake := page.Changed
	if page.Cursor != page.HighWater {
		wake = readyRuntimeWake()
	}
	return page.Cursor, wake, page.ChatChanged, nil
}

// pruneRuntimeLocked отзывает прекратившиеся эпохи и освобождает привязки без единого
// кандидата на замену. Пока View связывает новый выход с Destination, прежний участник
// сохраняет читателя и место в реестре. Отозванные доказательства уже закрывают его допуск.
func (m *Manager) pruneRuntimeLocked(anchor core.RuntimeAnchor) (map[*core.ChatSessionOwner][]core.ID, error) {
	// current сохраняет порядок допуска существующих источников при чтении конфигурации.
	current := make(map[*core.ChatSessionOwner][]core.ID, len(m.sessions))
	// session проверяется по точному удостоверению, включая снятие наблюдателя при RUNNING.
	for _, session := range m.sessions {
		if !session.owner.Active() {
			m.retireSessionLocked(session)
			continue
		}
		current[session.owner] = nil
		// candidate и checked сохраняют проверку общего родителя для всех его источников.
		var candidate, checked bool
		// sourceID и member обозначают существующего участника и его текущие выходы.
		for sourceID, member := range session.members {
			// live принимает только выходы с ещё действующим признаком публикации.
			// Callback исключает отозванный выход из нового снимка приёма.
			live := slices.DeleteFunc(slices.Clone(member.outputs), func(proof *core.ChatOutputPublication) bool { return !proof.Published() })
			if len(live) == 0 {
				if !checked {
					// err сообщает, что RAM-снимок сменился до этого решения.
					var err error
					candidate, err = m.hasRuntimeCandidateLocked(anchor, session)
					if err != nil {
						return nil, err
					}
					checked = true
				}
				if !candidate {
					m.retireSourceLocked(member, true)
					continue
				}
			}
			if len(live) != 0 && len(live) != len(member.outputs) {
				member.outputs = live
				if member.reader != nil {
					m.refreshIngressLocked(member.reader)
				}
			}
			current[session.owner] = append(current[session.owner], sourceID)
		}
		slices.Sort(current[session.owner])
	}
	return current, nil
}

// hasRuntimeCandidateLocked проверяет пригодность снимка для решения об остановке источника.
// Изменения наблюдателя и первое PUBLISHING закрывают ChatChanged под тем же допуском,
// поэтому снимок без заменяющего выхода безопасен только после короткой проверки RuntimeAuthority.
func (m *Manager) hasRuntimeCandidateLocked(anchor core.RuntimeAnchor, session *managedSession) (bool, error) {
	// proofs содержит только наблюдателей точного родителя из исходного RAM-снимка.
	var proofs []*core.ChatOutputPublication
	// snapshot связывает выходы с точным родительским наблюдателем.
	for _, snapshot := range anchor.Sessions {
		if snapshot.Session == nil || snapshot.Session.ChatOwner != session.owner || snapshot.Fence != session.fence {
			continue
		}
		// observed соответствует одному точному выполняемому выходу.
		for _, observed := range snapshot.Outputs {
			proofs = append(proofs, observed.ChatPublication)
		}
		break
	}
	// live получает результат проверки каждого кандидата под допуском RuntimeAuthority.
	live := make([]bool, len(proofs))
	// err сообщает об отзыве родителя или отмене сверки.
	if err := m.service.CheckChatPublications(m.ctx, session.owner, proofs, live); err != nil {
		return false, err
	}
	select {
	case <-anchor.ChatChanged:
		return false, core.ErrStale
	default:
	}
	return slices.Contains(live, true), nil
}

// retireUncoveredLocked прекращает удержание после ошибки View у источников, для которых
// отозваны все прежние выходы, и закрывает эпохи уже отозванных родителей.
func (m *Manager) retireUncoveredLocked() {
	// session содержит родителя и его прежние привязки из последней успешной сверки.
	for _, session := range m.sessions {
		if !session.owner.Active() {
			m.retireSessionLocked(session)
			continue
		}
		// member сохраняется, пока действует хотя бы одно его ранее подтверждённое доказательство.
		for _, member := range session.members {
			if !slices.ContainsFunc(member.outputs, (*core.ChatOutputPublication).Published) {
				m.retireSourceLocked(member, true)
			}
		}
	}
}

// readTopology читает Session, точные наблюдаемые Output и ограниченные страницы Source в одном View.
// Действующие источники читаются первыми, чтобы новые привязки их не вытесняли.
func (m *Manager) readTopology(anchor core.RuntimeAnchor, current map[*core.ChatSessionOwner][]core.ID) ([]runtimeTopology, error) {
	// result содержит действующих родителей, включая эпохи без единого подходящего выхода.
	result := make([]runtimeTopology, 0, min(len(anchor.Sessions), m.limits.Sessions))
	// snapshots обходит действующих родителей прежде новых, оставляя свободные места по порядку RAM.
	snapshots := slices.Clone(anchor.Sessions)
	// Callback возвращает приоритет действующего точного наблюдателя.
	priority := func(snapshot core.SessionRuntimeSnapshot) int {
		if snapshot.Session != nil {
			// exists отмечает наличие Hub у этого точного родителя.
			if _, exists := current[snapshot.Session.ChatOwner]; exists {
				return 0
			}
		}
		return 1
	}
	// Callback сохраняет исходный порядок внутри каждой группы приоритета.
	slices.SortStableFunc(snapshots, func(a, b core.SessionRuntimeSnapshot) int { return priority(a) - priority(b) })
	// err сообщает результат View. Переданная функция использует один набор согласованных репозиториев.
	err := m.store.View(m.ctx, func(r core.Repositories) error {
		// snapshot объединяет наблюдения одного точного запуска.
		for _, snapshot := range snapshots {
			if len(result) == m.limits.Sessions {
				break
			}
			if snapshot.Session == nil || snapshot.Session.ChatOwner == nil || !snapshot.Session.ChatOwner.Active() {
				continue
			}
			// stored содержит сохранённую Session, readErr сообщает об ошибке чтения этой записи.
			stored, readErr := r.Runtime.Session(m.ctx, snapshot.Fence.SessionID)
			if errors.Is(readErr, core.ErrNotFound) {
				continue
			}
			if readErr != nil {
				return readErr
			}
			if stored.State != core.SessionRunning || stored.Incarnation != snapshot.Fence.Incarnation || stored.Lifecycle != snapshot.Fence.Lifecycle {
				continue
			}
			// outputs группирует только выданные и публиковавшие попытки по Destination из снимка выхода.
			outputs, readErr := m.readOutputProofs(r, snapshot)
			if readErr != nil {
				return readErr
			}
			// candidates содержит ограниченную выборку подходящей конфигурации данного запуска.
			candidates, readErr := m.readSourceCandidates(r, outputs, current[snapshot.Session.ChatOwner])
			if readErr != nil {
				return readErr
			}
			result = append(result, runtimeTopology{fence: snapshot.Fence, owner: snapshot.Session.ChatOwner, streamID: stored.StreamID, candidates: candidates})
		}
		return nil
	})
	return result, err
}

// readOutputProofs связывает каждое живое оперативное доказательство с точной сохранённой попыткой.
func (m *Manager) readOutputProofs(r core.Repositories, snapshot core.SessionRuntimeSnapshot) (map[core.ID][]*core.ChatOutputPublication, error) {
	// result индексирует доказательства по ID назначения из неизменяемого снимка Output.
	result := make(map[core.ID][]*core.ChatOutputPublication)
	// observed содержит оперативное удостоверение только одной выданной попытки.
	for _, observed := range snapshot.Outputs {
		if observed.ChatPublication == nil || !observed.ChatPublication.Published() {
			continue
		}
		// fence фиксирует условия совпадения между наблюдением и долговечной записью.
		fence := observed.Execution
		if fence.SessionID != snapshot.Fence.SessionID || fence.Incarnation != snapshot.Fence.Incarnation || fence.Lifecycle != snapshot.Fence.Lifecycle {
			continue
		}
		// stored содержит снимок и управляющие флаги, err сообщает причину невозможности чтения.
		stored, err := r.Runtime.Output(m.ctx, fence.OutputID)
		if errors.Is(err, core.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if stored.SessionID == fence.SessionID && stored.Generation == fence.OutputGeneration && stored.Attempt == fence.Attempt && stored.Issued && stored.DesiredRunning && stored.Attached && !stored.CleanupPending {
			// destination берётся из снимка выполняемого выхода, сохраняя исходного родителя.
			destination := stored.Snapshot.Destination.ID
			result[destination] = append(result[destination], observed.ChatPublication)
		}
	}
	return result, nil
}

// readSourceCandidates сначала проверяет прежних участников, затем ищет включённые Source
// страницами по назначению. Отключённые записи не расходуют лимит активных привязок.
func (m *Manager) readSourceCandidates(r core.Repositories, outputs map[core.ID][]*core.ChatOutputPublication, existing []core.ID) ([]topologyMember, error) {
	// result удерживает не более SourcesPerSession кандидатов, а seen исключает повтор активного ID.
	result := make([]topologyMember, 0, min(len(existing), m.limits.SourcesPerSession))
	seen := make(map[core.ID]struct{}, len(existing))
	// add включает только активную конфигурацию, для которой остаётся место в пределах этой эпохи.
	add := func(source core.ChatSource) {
		// proofs перечисляет подходящие выходы, а ok подтверждает возможность удержать хотя бы один.
		proofs := outputs[source.DestinationID]
		_, ok := registryWeight(source, 1, m.limits.Registry.Bytes)
		if !source.Enabled || len(proofs) == 0 || !ok {
			return
		}
		// exists отмечает источник, уже включённый в список кандидатов.
		if _, exists := seen[source.ID]; exists {
			return
		}
		seen[source.ID] = struct{}{}
		result = append(result, topologyMember{source: source, outputs: proofs})
	}
	// id адресует очередного ранее удерживаемого участника.
	for _, id := range existing {
		// source содержит текущую конфигурацию, err сообщает об отказе чтения либо удалении.
		source, err := r.Chat.ChatSource(m.ctx, id)
		if errors.Is(err, core.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		add(source)
	}
	// destinations задаёт стабильный перебор назначений при конкуренции за свободные места.
	destinations := make([]core.ID, 0, len(outputs))
	// destination является назначением хотя бы одного текущего доказательства.
	for destination := range outputs {
		destinations = append(destinations, destination)
	}
	slices.Sort(destinations)
	// destination ограничивает поиск назначением с подходящими выходами.
	for _, destination := range destinations {
		// query хранит текущую страницу и после каждой полной страницы сдвигает нижнюю границу.
		query := core.ChatSourceQuery{DestinationID: destination, Limit: managerPageSize}
		for len(result) < m.limits.SourcesPerSession {
			// page содержит очередную страницу, err сообщает об отказе репозитория.
			page, err := r.Chat.ChatSources(m.ctx, query)
			if err != nil {
				return nil, err
			}
			// source проверяется независимо от количества предшествующих отключённых записей.
			for _, source := range page {
				if len(result) == m.limits.SourcesPerSession {
					break
				}
				add(source)
			}
			if len(page) < query.Limit {
				break
			}
			// last задаёт исключительную границу следующей страницы.
			last := page[len(page)-1]
			query.After = &core.ListBoundary{CreatedAt: last.CreatedAt, ID: last.ID}
		}
	}
	return result, nil
}

// registryWeight рассчитывает договорный вес одной конфигурации на каждое доказательство выхода.
func registryWeight(source core.ChatSource, count int, limit uint64) (uint64, bool) {
	// entry учитывает все удерживаемые строки Source и фиксированную нагрузку на привязку.
	entry := uint64(256 + len(source.ID) + len(source.AccountID) + len(source.DestinationID) + len(source.ChannelHint) + len(source.Provider))
	if uint64(count) > limit/entry {
		return 0, false
	}
	return entry * uint64(count), true
}

// applyTopologyLocked сохраняет точные действующие эпохи и добавляет только помещающиеся привязки.
func (m *Manager) applyTopologyLocked(topologies []runtimeTopology) {
	// byStream содержит согласованных родителей и используется для отзыва исчезнувших эпох.
	byStream := make(map[core.ID]*runtimeTopology, len(topologies))
	// i адресует актуальные данные одного родителя.
	for i := range topologies {
		// topology относится к отдельной Session из согласованного чтения.
		topology := &topologies[i]
		if topology.owner.Active() {
			byStream[topology.streamID] = topology
			// feed относится к тому же Stream, даже если свободного места для реального Hub пока нет.
			if feed := m.testFeeds[topology.streamID]; feed != nil {
				m.retireTestLocked(feed)
			}
		}
	}
	// streamID и session задают ранее допущенную эпоху для сравнения с точным родителем.
	for streamID, session := range m.sessions {
		// topology может отсутствовать после завершения либо отзыва родителя.
		topology := byStream[streamID]
		if topology == nil || topology.owner != session.owner {
			m.retireSessionLocked(session)
		}
	}
	// totals учитывает все удерживаемые активные привязки, прежде чем допустить расширение.
	totals := m.activeBudgetLocked()
	// admitted хранит доступные физические эпохи в стабильном порядке.
	admitted := make([]*runtimeTopology, 0, min(len(topologies), m.limits.Sessions))
	// i адресует родителя, для которого может быть доступен слот Hub.
	for i := range topologies {
		// topology выбирается по сохранённому порядку RuntimeAnchor.
		topology := &topologies[i]
		if byStream[topology.streamID] != topology {
			continue
		}
		// session использует действующий Hub либо получает один свободный слот.
		session := m.sessions[topology.streamID]
		if session == nil {
			session = m.newSessionLocked(topology.streamID, topology.fence, topology.owner)
		}
		if session == nil {
			continue
		}
		m.updateExistingLocked(session, topology.candidates, &totals)
		admitted = append(admitted, topology)
	}
	// topology содержит кандидатов очередной допущенной эпохи.
	for _, topology := range admitted {
		// session соответствует родителю этого списка кандидатов.
		session := m.sessions[topology.streamID]
		// candidate описывает потенциально новую активную привязку.
		for _, candidate := range topology.candidates {
			// exists отмечает источник, который уже удерживает активную привязку.
			if _, exists := session.members[candidate.source.ID]; exists || m.managedSourceBlockedLocked(candidate.source) {
				continue
			}
			if totals.sources >= m.limits.Sources || len(session.members) >= m.limits.SourcesPerSession {
				continue
			}
			candidate.outputs = m.fitOutputProofs(candidate, nil, &totals)
			if len(candidate.outputs) == 0 {
				continue
			}
			m.newSourceLocked(session, candidate)
			totals.sources++
		}
	}
	m.scheduleSourcesLocked()
}

// activeRegistryBudget учитывает текущие активные Source и их точные выходы.
type activeRegistryBudget struct {
	// sources считает активные конфигурации, count — привязки к их выходам.
	sources, count int
	// bytes содержит суммарный логический вес текущих привязок.
	bytes uint64
}

// activeBudgetLocked вычисляет занятые места после обновления списка прежних участников.
func (m *Manager) activeBudgetLocked() activeRegistryBudget {
	// totals накапливает общий расход по зарегистрированным эпохам.
	var totals activeRegistryBudget
	// session является одной из удерживаемых активных эпох.
	for _, session := range m.sessions {
		totals.sources += len(session.members)
		// source содержит привязки одного удерживаемого участника.
		for _, source := range session.members {
			totals.count += len(source.outputs)
			// weight учитывает действующие доказательства этой привязки.
			weight, _ := registryWeight(source.value, len(source.outputs), ^uint64(0))
			totals.bytes += weight
		}
	}
	return totals
}

// updateExistingLocked обновляет текущие привязки после согласованной сверки.
// Смена имени сохраняет поколение чтения. Выходы хранятся новым срезом для снимков приёма.
func (m *Manager) updateExistingLocked(session *managedSession, candidates []topologyMember, totals *activeRegistryBudget) {
	// byID позволяет сохранить действующую привязку при изменении набора выходов.
	byID := make(map[core.ID]topologyMember, len(candidates))
	// candidate содержит конфигурацию в той же согласованной транзакции.
	for _, candidate := range candidates {
		byID[candidate.source.ID] = candidate
	}
	// id и member адресуют прежнего участника, который может потерять допуск.
	for id, member := range session.members {
		if m.managedSourceBlockedLocked(member.value) {
			continue
		}
		// weight возвращает занимаемое место для пересмотра привязки без влияния на её соседей.
		weight, _ := registryWeight(member.value, len(member.outputs), m.limits.Registry.Bytes)
		totals.bytes -= weight
		totals.count -= len(member.outputs)
		// candidate содержит новое наблюдение, exists подтверждает наличие подходящего выхода.
		candidate, exists := byID[id]
		if !exists || !sameSourceReading(member.value, candidate.source) {
			totals.sources--
			m.retireSourceLocked(member, !exists)
			continue
		}
		// outputs отдаёт приоритет точным выходам прежнего снимка, если они ещё действуют.
		outputs := m.fitOutputProofs(candidate, member.outputs, totals)
		if len(outputs) == 0 {
			totals.sources--
			m.retireSourceLocked(member, true)
			continue
		}
		member.value = readerSourceConfig(candidate.source)
		if !slices.Equal(member.outputs, outputs) {
			member.outputs = outputs
			if member.reader != nil {
				m.refreshIngressLocked(member.reader)
			}
		}
	}
}

// fitOutputProofs резервирует доступное место для текущих точных выходов, затем для новых.
func (m *Manager) fitOutputProofs(candidate topologyMember, prior []*core.ChatOutputPublication, totals *activeRegistryBudget) []*core.ChatOutputPublication {
	// entry задаёт логический вес одного удерживаемого выхода этой конфигурации.
	entry, _ := registryWeight(candidate.source, 1, ^uint64(0))
	// capacity учитывает одновременно доступное количество, байты и число доступных доказательств.
	capacity := min(uint64(m.limits.Registry.Count-totals.count), (m.limits.Registry.Bytes-totals.bytes)/entry, uint64(len(candidate.outputs)))
	// result получает отдельный неизменяемый срез без подмены прежних удостоверений.
	result := make([]*core.ChatOutputPublication, 0, int(capacity))
	// proof занимает место прежнего участника, когда присутствует в новом согласованном чтении.
	for _, proof := range prior {
		if len(result) < cap(result) && proof.Published() && slices.Contains(candidate.outputs, proof) {
			result = append(result, proof)
		}
	}
	// proof занимает одно из оставшихся мест, если этот выход ещё не учтён.
	for _, proof := range candidate.outputs {
		if len(result) == cap(result) {
			break
		}
		if proof.Published() && !slices.Contains(result, proof) {
			result = append(result, proof)
		}
	}
	totals.count += len(result)
	totals.bytes += uint64(len(result)) * entry
	return result
}

// readerSourceConfig сохраняет настройки адаптера и точную ревизию. Имя загружается для snapshot
// из конфигурации Stream и не удерживается вместе с каждым доказательством читателя.
func readerSourceConfig(source core.ChatSource) core.ChatSource {
	source.Name = ""
	return source
}

// sameSourceReading сравнивает поля, влияющие на чтение. Валидные Options версии 1
// содержат пустой вариант соответствующего провайдера, поэтому адрес варианта значения не имеет.
func sameSourceReading(a, b core.ChatSource) bool {
	return a.ID == b.ID && a.DestinationID == b.DestinationID && a.Provider == b.Provider && a.AccountID == b.AccountID && a.ChannelHint == b.ChannelHint && a.Enabled == b.Enabled && a.Options.Version == b.Options.Version
}

// sourceNameOrder обеспечивает устойчивую сортировку атрибуции по исходным Source ID.
func sourceNameOrder(a, b *managedSource) int {
	return strings.Compare(string(a.value.ID), string(b.value.ID))
}
