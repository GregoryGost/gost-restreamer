package chat

import (
	"context"
	"math/rand/v2"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// BroadcastChoices содержит публичный набор эфиров одного точного поколения обнаружения YouTube.
type BroadcastChoices struct {
	// Session и SourceID связывают выбор с точным запуском и источником.
	Session  core.RuntimeSessionFence
	SourceID core.ID
	// SourceRevision и DiscoveryGeneration требуются при последующем SelectBroadcast.
	SourceRevision      uint64
	DiscoveryGeneration uint64
	// Broadcasts содержит доступные в этом поколении варианты без полномочий.
	Broadcasts []core.ChatBroadcast
}

// broadcastChoices хранит публичный набор, который ещё можно использовать для выбора.
type broadcastChoices struct {
	// generation меняется при каждом новом наборе обнаружения.
	generation uint64
	// values принадлежит Manager, который выдаёт наружу отдельную копию.
	values []core.ChatBroadcast
}

// discoveryVideo хранит видео текущего поколения и определяет, можно ли искать ему замену.
type discoveryVideo struct {
	// id закрепляет известное видео для следующего вызова videos.list.
	id string
	// explicit сохраняет выбор оператора до прекращения этого поколения источника.
	explicit bool
}

// startDiscoveryLocked занимает слот, копирует исходную конфигурацию и запускает отдельное обнаружение.
func (m *Manager) startDiscoveryLocked(member *managedSource) {
	member.discoveryStarted = true
	// source принадлежит только этому вызову адаптера, не меняясь при переименовании.
	source := member.value
	// adapter выбирается из неизменяемой регистрации по провайдеру источника.
	adapter := m.adapters[source.Provider]
	if adapter == nil {
		m.setSourceStatusLocked(member, core.ChatFailed, core.ChatReasonConfiguration)
		return
	}
	m.discovering++
	// Callback завершает физический этап обнаружения и только затем возвращает его слот.
	m.work.Go(func() { m.discover(member, source, adapter) })
}

// discover повторяет отменяемые попытки только при временных отказах либо ожидании эфира.
// Сетевой результат применяется лишь к исходному поколению и подтверждённым выходам.
func (m *Manager) discover(member *managedSource, source core.ChatSource, adapter core.ChatAdapterPort) {
	defer m.finishDiscovery()
	// retries задаёт бюджет этого поколения, video сохраняет выбор до подтверждения чата.
	retries := newRetrySchedule(m.limits)
	var video discoveryVideo
	// resolver доступен адаптерам, которые могут проверять известное видео без нового поиска.
	resolver, _ := adapter.(core.ChatBroadcastResolver)
	for member.ctx.Err() == nil {
		if !retries.waitForSlot(member.ctx) {
			return
		}
		m.reportDiscovery(member, core.ChatDiscovering, core.ChatReasonNone)
		// found содержит проверяемое обнаружение, err сообщает о временном отказе либо отсутствии доступа.
		found, err := m.discoverOnce(member, source, adapter, resolver, video.id, retries)
		if member.ctx.Err() != nil {
			return
		}
		if err != nil {
			// state и reason сообщают о причине безопасным типизированным значением.
			state, reason := credentialStatus(err)
			m.reportDiscovery(member, state, reason)
			if terminalReaderState(state) || !retries.wait(member.ctx, 0) {
				return
			}
			continue
		}
		// outcome сообщает, получен ли канал, требуется ли ещё одна попытка либо обнаружение окончено.
		outcome := m.useDiscovery(member, source.Provider, found, &video, resolver != nil)
		if !outcome || !retries.wait(member.ctx, found.RetryAfter) {
			return
		}
	}
}

// finishDiscovery освобождает физический слот и допускает ожидающих участников.
func (m *Manager) finishDiscovery() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.discovering--
	m.scheduleSourcesLocked()
}

// discoverOnce получает допуск отдельно перед Credentials и запросом адаптера. Известное
// videoID используется resolver вместо общего поиска, если адаптер его поддерживает. Ожидание сетевого
// слота может пересечься с отзывом выхода или конфигурационным изменением. В этом случае
// слот освобождается до ожидания нового допуска, чтобы соседние источники могли его занять.
func (m *Manager) discoverOnce(member *managedSource, source core.ChatSource, adapter core.ChatAdapterPort, resolver core.ChatBroadcastResolver, videoID string, retries *retrySchedule) (core.ChatDiscovery, error) {
	if !m.lockSourceAdmission(member.ctx, member) {
		return core.ChatDiscovery{}, member.ctx.Err()
	}
	// limited и cancel ограничивают всю конечную попытку, включая очередь сетевого слота.
	limited, cancel := context.WithTimeout(member.ctx, m.limits.HTTPTimeout)
	retries.recordStart()
	m.mu.Unlock()
	defer cancel()
	// credentials содержит серверный набор источника, err сообщает об отказе его владельца.
	credentials, err := m.credentials.Credentials(limited, source.AccountID)
	if err != nil {
		return core.ChatDiscovery{}, err
	}
	if credentials.AccountID != source.AccountID || credentials.Provider != source.Provider || credentials.Validate() != nil {
		return core.ChatDiscovery{}, core.ErrInvalid
	}
	for m.lockSourceAdmission(limited, member) {
		m.mu.Unlock()
		select {
		case m.discoverSlots <- struct{}{}:
		case <-limited.Done():
			return core.ChatDiscovery{}, limited.Err()
		}
		m.mu.Lock()
		if limited.Err() == nil && m.currentDiscoveryLocked(limited, member) {
			m.mu.Unlock()
			// Callback возвращает сетевой слот после фактического возврата адаптера.
			defer func() { <-m.discoverSlots }()
			if resolver != nil && videoID != "" {
				return resolver.DiscoverBroadcast(limited, source, credentials, videoID)
			}
			return adapter.Discover(limited, source, credentials)
		}
		m.mu.Unlock()
		<-m.discoverSlots
	}
	return core.ChatDiscovery{}, limited.Err()
}

// useDiscovery применяет результат и возвращает true, когда нужен новый запрос.
// При supportsResolution ранее выбранное видео перепроверяется отдельно от поиска.
func (m *Manager) useDiscovery(member *managedSource, provider core.ChatProvider, found core.ChatDiscovery, video *discoveryVideo, supportsResolution bool) bool {
	// choices содержит ограниченные проверенные варианты, ok подтверждает соблюдение входного бюджета.
	choices, ok := boundedBroadcasts(found.Broadcasts, min(m.limits.SnapshotMetadataBytes, m.limits.UpstreamHTTPBytes))
	if !ok || found.RetryAfter < 0 || found.State != "" && found.State.Validate() != nil || found.Reason.Validate() != nil || found.ChannelID != "" && !validDiscoveryID(found.ChannelID) || provider != core.ChatYouTube && len(choices) > 0 {
		m.reportDiscovery(member, core.ChatFailed, core.ChatReasonConfiguration)
		return false
	}
	if terminalReaderState(found.State) {
		m.reportDiscovery(member, found.State, found.Reason)
		return false
	}
	if supportsResolution && video.id != "" && len(choices) == 0 && found.State != core.ChatRetrying {
		if video.explicit {
			// reason объясняет завершение выбранного видео или отсутствие подтверждённых данных о нём.
			reason := found.Reason
			if reason == core.ChatReasonNone {
				reason = core.ChatReasonUnavailable
			}
			m.reportDiscovery(member, core.ChatFailed, reason)
			return false
		}
		// Автоматически найденное видео больше недоступно. Следующая попытка снова ищет эфир.
		*video = discoveryVideo{}
		m.reportDiscovery(member, core.ChatDiscovering, core.ChatReasonNoBroadcast)
		return true
	}
	// channel содержит фактический чат. started содержит при наличии платформенное время эфира.
	channel := found.ChannelID
	var started *time.Time
	if len(choices) > 0 {
		// choice устанавливается единственным результатом или явным выбором текущего поколения.
		var choice core.ChatBroadcast
		if video.id != "" {
			// index ищет только ранее выбранное видео, сохраняя выбор до конца поколения настройки.
			index := slices.IndexFunc(choices, func(value core.ChatBroadcast) bool { return value.VideoID == video.id })
			if index >= 0 {
				choice = choices[index]
			} else if supportsResolution {
				m.reportDiscovery(member, core.ChatFailed, core.ChatReasonConfiguration)
				return false
			}
		} else if len(choices) == 1 {
			choice = choices[0]
			if supportsResolution && choice.ChannelID == "" {
				video.id = choice.VideoID
			}
		} else {
			if !m.offerChoices(member, choices) {
				return false
			}
			select {
			case <-member.ctx.Done():
				return false
			case choice = <-member.selection:
				*video = discoveryVideo{id: choice.VideoID, explicit: true}
				if supportsResolution {
					// Чат мог перейти к другому видео, пока оператор выбирал из списка.
					m.reportDiscovery(member, core.ChatDiscovering, core.ChatReasonNone)
					return true
				}
			}
		}
		channel, started = choice.ChannelID, choice.StartedAt
	}
	if channel != "" {
		m.acceptDiscovery(member, channel, started)
		return false
	}
	// state и reason сохраняют проверенные статусы адаптера и дают безопасное ожидание по умолчанию.
	state, reason := found.State, found.Reason
	if state != core.ChatRetrying && (state != core.ChatDiscovering || !supportsResolution) {
		state = core.ChatWaitingChat
	}
	if reason == core.ChatReasonNone {
		reason = core.ChatReasonNoBroadcast
	}
	m.reportDiscovery(member, state, reason)
	return true
}

// validDiscoveryID ограничивает внешний идентификатор чата или видео до удержания в реестре.
func validDiscoveryID(value string) bool {
	return len(value) <= core.ChatIDBytes && strings.TrimSpace(value) != "" && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// boundedBroadcasts проверяет число, логический объём и уникальность публичных кандидатов.
func boundedBroadcasts(values []core.ChatBroadcast, budget uint64) ([]core.ChatBroadcast, bool) {
	if len(values) > managerPageSize {
		return nil, false
	}
	// result изолирует изменяемые временные метки, seen исключает неоднозначный выбор VideoID.
	result := make([]core.ChatBroadcast, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	// size накапливает только предусмотренное контрактом логическое удержание.
	var size uint64
	// value проходит структурную проверку прежде, чем попадёт в RAM-выбор.
	for _, value := range values {
		if !validDiscoveryID(value.VideoID) || value.ChannelID != "" && !validDiscoveryID(value.ChannelID) || len(value.Title) > core.ChatNameBytes || !utf8.ValidString(value.Title) || strings.ContainsRune(value.Title, 0) {
			return nil, false
		}
		// exists обнаруживает повтор VideoID в ответе адаптера.
		if _, exists := seen[value.VideoID]; exists {
			return nil, false
		}
		seen[value.VideoID] = struct{}{}
		if value.StartedAt != nil {
			// stamp приводит достоверное платформенное время к UTC и отвергает нулевую дату.
			stamp := value.StartedAt.UTC().Truncate(time.Microsecond)
			if stamp.IsZero() || stamp.Year() < 1 || stamp.Year() > 9999 {
				return nil, false
			}
			value.StartedAt = &stamp
		}
		size += uint64(256 + len(value.VideoID) + len(value.Title) + len(value.ChannelID))
		if size > budget {
			return nil, false
		}
		result = append(result, value)
	}
	return result, true
}

// currentDiscoveryLocked проверяет актуальность привязки, конфигурационный допуск и точные выходы.
func (m *Manager) currentDiscoveryLocked(ctx context.Context, member *managedSource) bool {
	return m.sourceAttachedLocked(member) && m.checkOutputProofsLocked(ctx, member.session.owner, member.outputs)
}

// lockSourceAdmission ждёт актуального допуска исходного поколения и оставляет m.mu
// захваченным при успехе. Отзыв поколения отменяет ожидание вместе с исходным контекстом.
func (m *Manager) lockSourceAdmission(ctx context.Context, member *managedSource) bool {
	// Callback проверяет только точный источник данного этапа обнаружения.
	return m.lockAdmission(ctx, func() bool { return m.currentDiscoveryLocked(ctx, member) })
}

// reportDiscovery публикует результат после открытия конфигурационного допуска для того же участника.
func (m *Manager) reportDiscovery(member *managedSource, state core.ChatSourceState, reason core.ChatReason) {
	if !m.lockSourceAdmission(member.ctx, member) {
		return
	}
	defer m.mu.Unlock()
	m.setSourceStatusLocked(member, state, reason)
}

// offerChoices сохраняет уникальное поколение набора и переводит текущую привязку в ожидание выбора.
func (m *Manager) offerChoices(member *managedSource, values []core.ChatBroadcast) bool {
	if !m.lockSourceAdmission(member.ctx, member) {
		return false
	}
	defer m.mu.Unlock()
	if m.nextDiscovery == ^uint64(0) {
		m.setSourceStatusLocked(member, core.ChatFailed, core.ChatReasonLimit)
		return false
	}
	m.nextDiscovery++
	member.discovery = &broadcastChoices{generation: m.nextDiscovery, values: values}
	m.setSourceStatusLocked(member, core.ChatWaitingChat, core.ChatReasonSelectionRequired)
	return true
}

// acceptDiscovery присоединяет только активную исходную привязку к фактическому общему каналу.
func (m *Manager) acceptDiscovery(member *managedSource, channel string, started *time.Time) {
	if !m.lockSourceAdmission(member.ctx, member) {
		return
	}
	defer m.mu.Unlock()
	member.discovery = nil
	member.channelID = channel
	member.status.BroadcastAt = started
	m.attachReaderLocked(member)
}

// BroadcastChoices возвращает независимую копию текущих вариантов исходного источника в этом запуске.
func (m *Manager) BroadcastChoices(ctx context.Context, session core.RuntimeSessionFence, sourceID core.ID) (BroadcastChoices, error) {
	if ctx == nil || sourceID == "" {
		return BroadcastChoices{}, core.ErrInvalid
	}
	if ctx.Err() != nil {
		return BroadcastChoices{}, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// member содержит актуальное поколение исходного YouTube-источника.
	member := m.broadcastMemberLocked(session, sourceID)
	if member == nil {
		return BroadcastChoices{}, core.ErrStale
	}
	// values копирует уже проверенные варианты вместе с указателями времени.
	values, _ := boundedBroadcasts(member.discovery.values, ^uint64(0))
	return BroadcastChoices{Session: session, SourceID: sourceID, SourceRevision: member.value.Revision, DiscoveryGeneration: member.discovery.generation, Broadcasts: values}, nil
}

// SelectBroadcast принимает одно видео, только если все удостоверения относятся к текущему набору.
func (m *Manager) SelectBroadcast(ctx context.Context, selection core.ChatBroadcastSelection) error {
	if ctx == nil || selection.SourceID == "" || selection.SourceRevision == 0 || selection.DiscoveryGeneration == 0 || !validDiscoveryID(selection.VideoID) {
		return core.ErrInvalid
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// member относится к точному RUNNING-родителю и ещё ожидает первый выбор текущего набора.
	member := m.broadcastMemberLocked(selection.Session, selection.SourceID)
	if member == nil || member.value.Revision != selection.SourceRevision || member.discovery.generation != selection.DiscoveryGeneration {
		return core.ErrStale
	}
	// value сверяется с исходным набором, выданным именно текущим обнаружением.
	for _, value := range member.discovery.values {
		if value.VideoID == selection.VideoID {
			member.discovery = nil
			member.selection <- value
			return nil
		}
	}
	return core.ErrStale
}

// broadcastMemberLocked находит текущего участника точного запуска, ожидающего выбор YouTube.
func (m *Manager) broadcastMemberLocked(fence core.RuntimeSessionFence, sourceID core.ID) *managedSource {
	// session может отличаться эпохой родителя даже при совпадении долговечного удостоверения.
	for _, session := range m.sessions {
		if session.fence != fence {
			continue
		}
		// member принимает выбор только для ещё актуального поколения обнаружения.
		member := session.members[sourceID]
		if member != nil && member.discovery != nil && m.currentDiscoveryLocked(member.ctx, member) {
			return member
		}
	}
	return nil
}

// retrySchedule ограничивает попытки одним обработчиком, скользящим окном и верхней задержкой.
type retrySchedule struct {
	// limits содержит настройки окна и исходной задержки.
	limits core.ChatLimits
	// attempts содержит только отметки внутри скользящего окна.
	attempts []time.Time
	// backoff растёт до настроенного максимума после последовательных временных отказов.
	backoff time.Duration
}

// newRetrySchedule начинает отдельный бюджет попыток владельца обнаружения или чтения.
func newRetrySchedule(limits core.ChatLimits) *retrySchedule {
	return &retrySchedule{limits: limits, backoff: limits.ReconnectMinBackoff}
}

// waitForSlot ждёт освобождения места в скользящем окне. У владельца только одна горутина,
// поэтому время попытки можно учесть позже, при получении допуска к Credentials.
func (r *retrySchedule) waitForSlot(ctx context.Context) bool {
	for ctx.Err() == nil {
		// now задаёт границу текущего окна.
		now := time.Now()
		// expired считает отброшенный префикс уже вышедших из окна попыток.
		expired := 0
		for expired < len(r.attempts) && !r.attempts[expired].Add(r.limits.ReconnectWindow).After(now) {
			expired++
		}
		r.attempts = slices.Delete(r.attempts, 0, expired)
		if len(r.attempts) < r.limits.ReconnectAttempts {
			return true
		}
		if !waitRetry(ctx, time.Until(r.attempts[0].Add(r.limits.ReconnectWindow))) {
			return false
		}
	}
	return false
}

// recordStart учитывает попытку в момент допуска. Ожидание сверки и мутации до допуска
// может занять целое окно и не должно сдвигать отметку фактически начатой попытки.
func (r *retrySchedule) recordStart() { r.attempts = append(r.attempts, time.Now()) }

// resetBackoff восстанавливает исходную задержку после подтверждённой подписки.
func (r *retrySchedule) resetBackoff() { r.backoff = r.limits.ReconnectMinBackoff }

// wait учитывает превышающий локальную задержку интервал площадки и отмену ожидания.
func (r *retrySchedule) wait(ctx context.Context, retryAfter time.Duration) bool {
	// room удерживает задержку с разбросом в пределах заданного максимума, delay применяется к этой попытке.
	room := min(r.backoff/4, r.limits.ReconnectMaxBackoff-r.backoff)
	delay := r.backoff
	if room > 0 {
		delay += time.Duration(rand.Int64N(int64(room)))
	}
	r.backoff += min(r.backoff, r.limits.ReconnectMaxBackoff-r.backoff)
	return waitRetry(ctx, max(delay, retryAfter))
}

// waitRetry освобождает таймер при окончании задержки либо отмене владельца.
func waitRetry(ctx context.Context, delay time.Duration) bool {
	// timer представляет текущий отменяемый интервал ожидания.
	timer := time.NewTimer(max(delay, 0))
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}
