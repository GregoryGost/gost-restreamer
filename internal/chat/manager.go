package chat

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// CredentialReader возвращает серверные полномочия через владельца аккаунта.
type CredentialReader interface {
	// Credentials сериализует чтение и обновление полномочий указанного аккаунта.
	Credentials(context.Context, core.ID) (core.ChatCredentials, error)
}

// Manager сверяет медиажизненный цикл с конфигурацией и управляет лентами и чтением площадок.
// Запросы Store и адаптеров выполняются за пределами блокировки реестра.
type Manager struct {
	// ctx отменяет все собственные работы. cancel запрашивает их остановку.
	ctx    context.Context
	cancel context.CancelFunc
	// service проверяет оперативные доказательства. store предоставляет согласованное чтение конфигурации.
	service *core.Service
	store   core.UnitOfWork
	// limits задаёт неизменяемые бюджеты. adapters индексирует фиксированные адаптеры по платформе.
	limits   core.ChatLimits
	adapters map[core.ChatProvider]core.ChatAdapterPort
	// credentials делегирует чтение полномочий. ownedCredentials закрывается только при внутреннем создании.
	credentials      CredentialReader
	ownedCredentials *CredentialOwner
	// mu защищает ленты, привязки, обработчики и изменения конфигурации.
	mu sync.Mutex
	// sessions содержит текущую эпоху по потоку, а hubs считает также закрывающиеся эпохи.
	sessions map[core.ID]*managedSession
	hubs     int
	// readers удерживает обработчики по ключу, включая конечные состояния с живыми участниками.
	readers map[managedReaderKey]*managedReader
	// readerSlots считает физические попытки и ожидающие переподключения вплоть до возврата.
	readerSlots int
	// connections учитывает основные и дополнительные соединения до физического закрытия.
	connections chan struct{}
	// twitchAuthorizations хранит ограниченный набор попыток под защитой mu.
	twitchAuthorizations map[core.ID]*twitchAuthorization
	// wake объединяет запросы на сверку. changed будит клиентов при смене эпохи или конфигурации.
	wake    chan struct{}
	changed chan struct{}
	// topologyChanged будит ожидающих допуска после применения согласованной топологии.
	topologyChanged chan struct{}
	// visibility различает предсессионные позиции при смене эпох Hub.
	visibility uint64
	// changes различает ревизии реестра конфигурационных операций во время чтения Store.
	changes uint64
	// mutation сериализует административные подтверждения. blockedSource и blockedAccount временно
	// исключают только участников активного изменения.
	mutation       chan struct{}
	blockedSource  core.ID
	blockedAccount core.ID
	// discoverSlots ограничивает параллельные конечные вызовы адаптеров.
	discoverSlots chan struct{}
	// discovering удерживает слоты обнаружения до физического завершения вызова.
	discovering int
	// nextDiscovery назначает поколения выбора, уникальные на протяжении работы Manager.
	nextDiscovery uint64
	// snapshotSlots ограничивает количество одновременно создаваемых снимков.
	// eventSlots ограничивает параллельные запросы replay и открытия подписки.
	snapshotSlots chan struct{}
	eventSlots    chan struct{}
	// subscriptions считает подписки по Stream, включая закрывающиеся эпохи.
	subscriptions map[core.ID]int
	// subscribers содержит общее число выданных и физически удерживаемых подписок.
	subscribers int
	// cursorKey различает курсоры ожидания этого экземпляра Manager.
	cursorKey chatCursorKey
	// work учитывает управляющую горутину, Hub, читателей, обнаружение и конфигурационные операции.
	work sync.WaitGroup
	// done закрывается после физического завершения работ.
	done chan struct{}
}

// managedSession содержит одну эпоху Hub и участников точного медианаблюдателя.
type managedSession struct {
	// owner проверяет неизменяемое удостоверение родительского наблюдателя.
	owner *core.ChatSessionOwner
	// fence связывает эпоху с сохранённым запуском, а streamID адресует её подписчикам.
	fence    core.RuntimeSessionFence
	streamID core.ID
	// hub отвечает за буферы сообщений и доставку этой эпохи.
	hub *chatHub
	// members содержит только удерживаемые активные привязки по Source ID.
	members map[core.ID]*managedSource
	// stopped содержит ограниченный хвост остановленных источников, ещё видимых в Hub.
	stopped []core.ID
}

// managedSource хранит состояние одной активной привязки на протяжении её неизменного поколения.
// Сам адрес объекта служит идентичностью поколения для очереди и результатов Discover.
type managedSource struct {
	// session задаёт точного родителя, а value содержит принятую конфигурацию.
	session *managedSession
	value   core.ChatSource
	// outputs содержит доказательства подходящих точных выходов этого поколения.
	outputs []*core.ChatOutputPublication
	// ctx и cancel задают время жизни асинхронного обнаружения конфигурации.
	ctx    context.Context
	cancel context.CancelFunc
	// discovery запоминает текущий ограниченный набор видео, а selection передаёт явный выбор.
	discovery *broadcastChoices
	selection chan core.ChatBroadcast
	// discoveryStarted остаётся установленным после конечного результата, пока живёт привязка.
	discoveryStarted bool
	// reader связывает участника с общим обработчиком, если найден фактический канал.
	reader *managedReader
	// channelID хранит разрешённый фактический чат, пока ожидается свободный слот обработчика.
	channelID string
	// status содержит последний безопасный статус привязки.
	status core.ChatSourceStatus
}

// New создаёт Manager и запускает сверку. Необязательный credentials принадлежит вызывающему.
// По умолчанию Manager создаёт владельца полномочий и передаёт ему проверку и ротацию
// токенов зарегистрированного Twitch-адаптера.
func New(ctx context.Context, service *core.Service, store core.UnitOfWork, limits core.ChatLimits, adapters []core.ChatAdapterPort, credentials ...CredentialReader) (*Manager, error) {
	if ctx == nil || service == nil || store == nil || limits.Validate() != nil || len(credentials) > 1 {
		return nil, core.ErrInvalid
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// byProvider связывает каждую поддерживаемую платформу с единственным заданным адаптером.
	byProvider := make(map[core.ChatProvider]core.ChatAdapterPort, len(adapters))
	// adapter является очередным зарегистрированным адаптером.
	for _, adapter := range adapters {
		if adapter == nil {
			return nil, core.ErrInvalid
		}
		// provider проверяется до помещения адаптера в неизменяемый индекс.
		provider := adapter.Provider()
		if provider.Validate() != nil || byProvider[provider] != nil {
			return nil, core.ErrInvalid
		}
		byProvider[provider] = adapter
	}
	// key различает предсессионные курсоры этого Manager.
	// err сообщает об отказе источника случайных данных.
	key, err := newChatCursorKey()
	if err != nil {
		return nil, err
	}
	if uint64(len(key.cursor("", 0))) > limits.CursorBytes {
		return nil, core.ErrInvalid
	}
	// reader предоставляет полномочия, а owner ненулевой только при владении Manager.
	var reader CredentialReader
	var owner *CredentialOwner
	// httpSlots ограничивает конечные сетевые запросы Manager и шаги авторизации Twitch.
	httpSlots := make(chan struct{}, limits.HTTPRequests)
	if len(credentials) == 1 {
		reader = credentials[0]
		if reader == nil {
			return nil, core.ErrInvalid
		}
	} else {
		// err передаёт ошибку предварительной проверки владельца полномочий.
		var err error
		owner, err = NewCredentialOwner(service, limits, twitchCredentialRefresh(byProvider[core.ChatTwitch], httpSlots, limits.HTTPTimeout))
		if err != nil {
			return nil, err
		}
		reader = owner
	}
	// workerCtx и cancel ограничивают срок фоновых работ родительским контекстом.
	workerCtx, cancel := context.WithCancel(ctx)
	// m удерживает реестр RAM и конечные слоты операций.
	m := &Manager{
		ctx: workerCtx, cancel: cancel, service: service, store: store, credentials: reader, ownedCredentials: owner,
		limits: limits, adapters: byProvider, sessions: make(map[core.ID]*managedSession), readers: make(map[managedReaderKey]*managedReader),
		connections: make(chan struct{}, limits.Connections), twitchAuthorizations: make(map[core.ID]*twitchAuthorization),
		wake: make(chan struct{}, 1), changed: make(chan struct{}), topologyChanged: make(chan struct{}), mutation: make(chan struct{}, 1),
		discoverSlots: httpSlots, snapshotSlots: make(chan struct{}, limits.Snapshots), eventSlots: make(chan struct{}, limits.SSEClients),
		subscriptions: make(map[core.ID]int), cursorKey: key, done: make(chan struct{}),
	}
	// Callback завершает управляющую работу после запроса остановки всех её ресурсов.
	m.work.Go(m.run)
	go m.join()
	return m, nil
}

// join закрывает done после завершения всех работ Manager и созданного им владельца полномочий.
func (m *Manager) join() {
	m.work.Wait()
	m.mu.Lock()
	clear(m.twitchAuthorizations)
	m.mu.Unlock()
	if m.ownedCredentials != nil {
		_ = m.ownedCredentials.Close(context.Background())
		<-m.ownedCredentials.Done()
	}
	close(m.done)
}

// Done возвращает сигнал завершения всех принадлежащих Manager ресурсов.
func (m *Manager) Done() <-chan struct{} { return m.done }

// Close запрашивает остановку и ждёт фактического завершения до истечения ctx.
func (m *Manager) Close(ctx context.Context) error {
	if ctx == nil {
		return core.ErrInvalid
	}
	m.cancel()
	select {
	case <-m.done:
		return nil
	default:
	}
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// requestReconcile неблокирующе запрашивает повторное чтение конфигурации.
func (m *Manager) requestReconcile() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// signalChangedLocked пробуждает потребителей предсессионного курсора после изменения.
func (m *Manager) signalChangedLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// retireSessionLocked отзывает участников и закрывает Hub. Его слот остаётся занятым до Hub.Done.
func (m *Manager) retireSessionLocked(session *managedSession) {
	delete(m.sessions, session.streamID)
	session.hub.stop()
	// member относится к закрываемой эпохе и больше не допускается к публикации.
	for _, member := range session.members {
		m.retireSourceLocked(member, false)
	}
	m.visibility++
	m.signalChangedLocked()
}

// newSessionLocked создаёт Hub при наличии свободного физического слота.
func (m *Manager) newSessionLocked(streamID core.ID, fence core.RuntimeSessionFence, owner *core.ChatSessionOwner) *managedSession {
	if m.hubs >= m.limits.Sessions {
		return nil
	}
	// hub принимает сообщения этой эпохи, err сообщает об отказе её создания.
	hub, err := newChatHub(m.limits, time.Now, streamID, fence)
	if err != nil {
		return nil
	}
	// session является новой эпохой, даже когда долговечное удостоверение совпало с прежним.
	session := &managedSession{owner: owner, fence: fence, streamID: streamID, hub: hub, members: make(map[core.ID]*managedSource)}
	m.sessions[streamID] = session
	m.hubs++
	m.visibility++
	m.signalChangedLocked()
	// Callback удерживает слот до физического завершения Hub, затем просит сверку ожидающих эпох.
	m.work.Go(func() {
		<-hub.Done()
		m.mu.Lock()
		m.hubs--
		m.mu.Unlock()
		m.requestReconcile()
	})
	return session
}

// managedSourceBlockedLocked сообщает, затрагивает ли текущая операция данную привязку.
func (m *Manager) managedSourceBlockedLocked(source core.ChatSource) bool {
	return source.ID == m.blockedSource || source.AccountID == m.blockedAccount
}

// retireSourceLocked исключает только эту привязку и освобождает обработчик при потере последнего участника.
// stopped публикует отдельный статус при остановке выхода в продолжающейся эпохе.
func (m *Manager) retireSourceLocked(member *managedSource, stopped bool) {
	delete(member.session.members, member.value.ID)
	member.cancel()
	if member.reader != nil {
		m.detachReaderLocked(member)
	}
	if stopped {
		m.setSourceStatusLocked(member, core.ChatStopped, core.ChatReasonNone)
		member.session.stopped = append(member.session.stopped, member.value.ID)
		m.trimStoppedStatusesLocked(member.session)
		return
	}
	member.session.hub.removeStatus(member.value.ID)
}

// trimStoppedStatusesLocked освобождает старейшие остановки, прежде чем они займут места живых статусов.
func (m *Manager) trimStoppedStatusesLocked(session *managedSession) {
	// capacity хранит общий предел записей о живых и недавно остановленных привязках.
	capacity := min(m.limits.SourcesPerSession, m.limits.Registry.Count)
	for len(session.stopped) > max(0, capacity-len(session.members)) {
		// oldest обозначает источник с наиболее давней удерживаемой остановкой.
		oldest := session.stopped[0]
		session.stopped = slices.Delete(session.stopped, 0, 1)
		session.hub.removeStatus(oldest)
	}
}
