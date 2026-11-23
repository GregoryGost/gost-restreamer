package chat

import (
	"context"
	"crypto/rand"
	"errors"
	"slices"
	"sort"
	"sync/atomic"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// managedReaderKey различает фактические каналы и эпохи точного родительского наблюдателя.
type managedReaderKey struct {
	// session включает точную идентичность наблюдателя, которого не содержит долговечное удостоверение.
	session *managedSession
	// channel передаётся адаптеру как идентичность общего upstream-чтения.
	channel core.ChatWorkerKey
}

// managedReader отвечает за одно разделяемое чтение вместе с его отменяемыми повторными попытками.
type managedReader struct {
	// key задаёт точный разделяемый канал, а manager владеет реестром участников.
	key     managedReaderKey
	manager *Manager
	// ctx и cancel определяют время жизни всех попыток этого обработчика.
	ctx    context.Context
	cancel context.CancelFunc
	// members содержит независимые активные привязки, а attempt определяет право на обратный вызов.
	members map[*managedSource]struct{}
	attempt *readerAttempt
	// running удерживает физический слот до завершения горутины обработчика.
	running bool
	// state и reason сохраняют безопасное состояние для позднее присоединившихся участников.
	state  core.ChatSourceState
	reason core.ChatReason
}

// readerAttempt реализует порт обратных вызовов одной физической попытки чтения.
type readerAttempt struct {
	// reader владеет текущей попыткой. token выдаётся адаптеру только на её время жизни.
	reader *managedReader
	token  core.ChatWorkerToken
	// cancel прекращает текущую попытку при конечном состоянии, оставляя владельца зарегистрированным.
	cancel context.CancelFunc
	// ended отсекает новые обратные вызовы при конечном статусе и после возврата Read.
	ended atomic.Bool
	// ingress даёт приёму неизменяемый снимок без ожидания Manager и медиауправления.
	ingress atomic.Pointer[readerIngress]
	// connected отмечает подтверждение подписки под блокировкой Manager на протяжении попытки.
	connected bool
}

// readerIngress содержит только участников и доказательства, существовавшие во время приёма.
type readerIngress struct {
	// members упорядочены по ID источника. proofs каждого элемента образуют отдельный исходный набор.
	members []ingressMember
	// weight входит в байтовый бюджет очереди Hub вместе с входным сообщением.
	weight uint64
	// sourceJSONBytes ограничивает максимальный экранированный ID исходных участников.
	sourceJSONBytes uint64
}

// ingressMember связывает исходное поколение участника с исходными доказательствами его выходов.
type ingressMember struct {
	// member идентифицирует поколение, а sourceID остаётся неизменным при последующих обновлениях.
	member   *managedSource
	sourceID core.ID
	// proofs остаётся неизменным для уже поставленных в очередь сообщений.
	proofs []*core.ChatOutputPublication
}

// scheduleSourcesLocked выдаёт свободные слоты обнаружения и чтения удерживаемым участникам.
// Закрывающаяся операция удерживает слот до фактического возврата адаптера.
func (m *Manager) scheduleSourcesLocked() {
	if m.ctx.Err() != nil {
		return
	}
	// waiting содержит активных участников, которым ещё нужно обнаружение или подключение.
	waiting := make([]*managedSource, 0, m.limits.Sources)
	// session является очередной действующей эпохой.
	for _, session := range m.sessions {
		// member может уже удерживать доступ к разделяемому обработчику.
		for _, member := range session.members {
			if member.reader == nil && m.sourceAttachedLocked(member) && slices.ContainsFunc(member.outputs, (*core.ChatOutputPublication).Published) {
				waiting = append(waiting, member)
			}
		}
	}
	slices.SortFunc(waiting, sourceNameOrder)
	// member получает ресурс в устойчивом порядке без вытеснения ранее допущенных.
	for _, member := range waiting {
		if member.channelID != "" {
			m.attachReaderLocked(member)
		} else if !member.discoveryStarted && m.discovering < m.limits.Sources {
			m.startDiscoveryLocked(member)
		}
	}
}

// newSourceLocked регистрирует поколение привязки, ещё не выдав ей физический ресурс.
func (m *Manager) newSourceLocked(session *managedSession, candidate topologyMember) {
	// ctx и cancel ограничивают время жизни поколения конфигурации.
	ctx, cancel := context.WithCancel(m.ctx)
	// member впервые появляется с доказательствами из согласованного снимка.
	member := &managedSource{session: session, value: readerSourceConfig(candidate.source), outputs: candidate.outputs, ctx: ctx, cancel: cancel, selection: make(chan core.ChatBroadcast, 1)}
	session.members[candidate.source.ID] = member
	// Callback исключает прежнюю остановку вновь допущенного источника без удаления её исторического события.
	session.stopped = slices.DeleteFunc(session.stopped, func(id core.ID) bool { return id == candidate.source.ID })
	m.trimStoppedStatusesLocked(session)
	m.setSourceStatusLocked(member, core.ChatDiscovering, core.ChatReasonNone)
}

// attachReaderLocked присоединяет участника к текущему точному каналу или запускает одного нового читателя.
func (m *Manager) attachReaderLocked(member *managedSource) {
	// key связывает фактический канал и аккаунт с точной эпохой Session.
	key := managedReaderKey{session: member.session, channel: core.ChatWorkerKey{Session: member.session.fence, Provider: member.value.Provider, AccountID: member.value.AccountID, ChannelID: member.channelID}}
	// reader может ещё закрываться и поэтому удерживает ключ до возврата вызова.
	reader := m.readers[key]
	if reader != nil && reader.ctx.Err() != nil || reader == nil && m.readerSlots >= m.limits.Readers {
		m.setSourceStatusLocked(member, core.ChatRetrying, core.ChatReasonLimit)
		return
	}
	if reader == nil {
		// ctx и cancel задают время жизни общего чтения, которое не зависит от одного участника.
		ctx, cancel := context.WithCancel(m.ctx)
		reader = &managedReader{key: key, manager: m, ctx: ctx, cancel: cancel, members: make(map[*managedSource]struct{}), state: core.ChatConnecting, running: true}
		m.readers[key] = reader
		m.readerSlots++
		// Callback освобождает слот только после завершения обработки последней попытки.
		m.work.Go(reader.run)
	}
	reader.members[member] = struct{}{}
	member.reader = reader
	m.setSourceStatusLocked(member, reader.state, reader.reason)
	m.refreshIngressLocked(reader)
}

// detachReaderLocked отзывает независимого участника. Остальные продолжают то же чтение.
func (m *Manager) detachReaderLocked(member *managedSource) {
	// reader сохраняет ссылку на общий обработчик при удалении двусторонней привязки.
	reader := member.reader
	delete(reader.members, member)
	member.reader = nil
	if len(reader.members) == 0 {
		reader.cancel()
		if !reader.running {
			delete(m.readers, reader.key)
		}
	}
	m.refreshIngressLocked(reader)
}

// refreshIngressLocked заменяет снимок допуска точной попытки. Ранее принятые сообщения
// продолжают ссылаться на прежний набор и проходят отдельную проверку перед публикацией.
func (m *Manager) refreshIngressLocked(reader *managedReader) {
	// attempt принимает новый снимок только во время физического чтения.
	attempt := reader.attempt
	if attempt == nil || attempt.ended.Load() {
		return
	}
	if reader.ctx.Err() != nil || terminalReaderState(reader.state) {
		attempt.ingress.Store(nil)
		return
	}
	// ingress удерживает неизменяемые доказательства и их логический вес.
	ingress := &readerIngress{members: make([]ingressMember, 0, len(reader.members))}
	// member может принадлежать заблокированной операции над Source или Account.
	for member := range reader.members {
		if !m.sourceAttachedLocked(member) || !slices.ContainsFunc(member.outputs, (*core.ChatOutputPublication).Published) {
			continue
		}
		ingress.members = append(ingress.members, ingressMember{member: member, sourceID: member.value.ID, proofs: member.outputs})
		ingress.weight += uint64(256 + len(member.value.ID) + 8*len(member.outputs))
		ingress.sourceJSONBytes = max(ingress.sourceJSONBytes, 6*uint64(len(member.value.ID)))
	}
	// Callback сравнивает неизменяемые Source ID для выбора минимального допустимого участника.
	sort.Slice(ingress.members, func(i, j int) bool { return ingress.members[i].sourceID < ingress.members[j].sourceID })
	attempt.ingress.Store(ingress)
}

// sourceAttachedLocked проверяет текущее поколение источника и область конфигурационного изменения.
// Состояние выходов проверяется отдельно непосредственно перед допуском работы.
func (m *Manager) sourceAttachedLocked(member *managedSource) bool {
	// session принадлежит исходному поколению при всех последующих сверках.
	session := member.session
	return member.ctx.Err() == nil && m.sessions[session.streamID] == session && session.owner.Active() &&
		session.members[member.value.ID] == member && !m.managedSourceBlockedLocked(member.value)
}

// checkOutputProofsLocked подтверждает родителя и хотя бы один точный выход под RuntimeAuthority.
func (m *Manager) checkOutputProofsLocked(ctx context.Context, owner *core.ChatSessionOwner, proofs []*core.ChatOutputPublication) bool {
	if !slices.ContainsFunc(proofs, (*core.ChatOutputPublication).Published) {
		return false
	}
	// live принимает отдельный результат RuntimeAuthority для каждого доказательства.
	live := make([]bool, len(proofs))
	return m.service.CheckChatPublications(ctx, owner, proofs, live) == nil && slices.Contains(live, true)
}

// lockAdmission оставляет m.mu захваченным только при успешной проверке allowed. Вызывающий
// освобождает его перед вводом-выводом. Временный запрет ждёт сверку или открытие области
// изменения конфигурации, а отменённый контекст прекращает ожидание.
func (m *Manager) lockAdmission(ctx context.Context, allowed func() bool) bool {
	for ctx.Err() == nil {
		m.mu.Lock()
		if ctx.Err() == nil && allowed() {
			return true
		}
		// topology и config относятся к состоянию, в котором только что проверили допуск.
		topology, config := m.topologyChanged, m.changed
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-topology:
		case <-config:
		}
	}
	return false
}

// readerCurrentLocked проверяет регистрацию логического читателя и его точного родителя.
func (r *managedReader) readerCurrentLocked() bool {
	// session остаётся той же оперативной эпохой при смене набора выходов.
	session := r.key.session
	return r.ctx.Err() == nil && r.manager.readers[r.key] == r && r.manager.sessions[session.streamID] == session && session.owner.Active()
}

// lockReaderAdmission возвращает допуск к новой попытке с удержанием m.mu. Для общего
// читателя достаточно живого подтверждения любого из его текущих незаблокированных участников.
func (r *managedReader) lockReaderAdmission(ctx context.Context) bool {
	// m защищает общие привязки и конфигурационный барьер на время проверки.
	m := r.manager
	// Callback собирает доказательства только у действующих участников этого читателя.
	return m.lockAdmission(ctx, func() bool {
		if !r.readerCurrentLocked() {
			return false
		}
		// proofs объединяет проверяемые выходы для одной проверки RuntimeAuthority.
		var proofs []*core.ChatOutputPublication
		// member может принадлежать отдельному изменению Source, даже если читатель общий.
		for member := range r.members {
			if m.sourceAttachedLocked(member) {
				proofs = append(proofs, member.outputs...)
			}
		}
		return m.checkOutputProofsLocked(ctx, r.key.session.owner, proofs)
	})
}

// run начинает последовательные попытки, повторно получая полномочия через владельца аккаунта.
// Read отвечает за физическое соединение, а возвращается после освобождения своих ресурсов.
func (r *managedReader) run() {
	defer r.finish()
	// retries ограничивает попытки захвата чтения скользящим окном и задержкой.
	retries := newRetrySchedule(r.manager.limits)
	// resumeToken принадлежит только этому читателю и исчезает при отзыве его поколения.
	var resumeToken string
	for r.ctx.Err() == nil {
		if !retries.waitForSlot(r.ctx) {
			return
		}
		// state и connected описывают итог подписки, retry содержит новую позицию и задержку площадки.
		state, connected, retry := r.readOnce(retries, resumeToken)
		if r.ctx.Err() != nil || terminalReaderState(state) {
			return
		}
		if connected {
			retries.resetBackoff()
		}
		// retryAfter оставляет обычную задержку Manager для адаптеров без результата повтора.
		var retryAfter time.Duration
		if retry != nil {
			if len(retry.ResumeToken) <= core.ChatResumeTokenBytes {
				resumeToken = retry.ResumeToken
			}
			retryAfter = max(0, retry.RetryAfter)
		}
		if !retries.wait(r.ctx, retryAfter) {
			return
		}
	}
}

// finish освобождает физический слот. При конечном статусе обработчик остаётся в логических
// привязках до подтверждённой смены конфигурации, чтобы не повторять бесполезное подключение.
func (r *managedReader) finish() {
	// m защищает удаление физического владельца и выдачу освободившегося места.
	m := r.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	r.running = false
	m.readerSlots--
	if r.ctx.Err() != nil || len(r.members) == 0 {
		delete(m.readers, r.key)
	}
	m.scheduleSourcesLocked()
}

// readOnce получает текущие реквизиты, передаёт resumeToken и выдаёт отдельный токен callback.
// Результат возвращает состояние, факт предыдущего подключения и подтверждённые параметры повтора.
func (r *managedReader) readOnce(retries *retrySchedule, resumeToken string) (core.ChatSourceState, bool, *core.ChatReadRetry) {
	// m предоставляет единственный адаптер и владельца полномочий данного аккаунта.
	m := r.manager
	if !r.lockReaderAdmission(r.ctx) {
		return core.ChatStopped, false, nil
	}
	retries.recordStart()
	m.mu.Unlock()
	// credentials содержит полные серверные данные, а err классифицируется без текста ошибки.
	credentials, err := m.credentials.Credentials(r.ctx, r.key.channel.AccountID)
	if err != nil {
		// state и reason выбирают безопасное состояние отказа credential-owner.
		state, reason := credentialStatus(err)
		if !r.lockReaderAdmission(r.ctx) {
			return core.ChatStopped, false, nil
		}
		m.setReaderStatusLocked(r, state, reason)
		m.mu.Unlock()
		return state, false, nil
	}
	if credentials.AccountID != r.key.channel.AccountID || credentials.Provider != r.key.channel.Provider || credentials.Validate() != nil {
		if !r.lockReaderAdmission(r.ctx) {
			return core.ChatStopped, false, nil
		}
		m.setReaderStatusLocked(r, core.ChatAuthRequired, core.ChatReasonCredentials)
		m.mu.Unlock()
		return core.ChatAuthRequired, false, nil
	}
	// token отличает каждую физическую попытку, включая повторное подключение того же читателя.
	var token core.ChatWorkerToken
	if _, err = rand.Read(token[:]); err != nil {
		if !r.lockReaderAdmission(r.ctx) {
			return core.ChatStopped, false, nil
		}
		m.setReaderStatusLocked(r, core.ChatFailed, core.ChatReasonConfiguration)
		m.mu.Unlock()
		return core.ChatFailed, false, nil
	}
	// ctx и cancel разрешают отзывать отдельно текущую физическую подписку.
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	// attempt реализует интерфейс приёма только для выданного токена.
	attempt := &readerAttempt{reader: r, token: token, cancel: cancel}
	if !r.lockReaderAdmission(ctx) {
		return core.ChatStopped, false, nil
	}
	r.attempt = attempt
	m.setReaderStatusLocked(r, core.ChatConnecting, core.ChatReasonNone)
	m.refreshIngressLocked(r)
	m.mu.Unlock()
	// Read сам ограничивает протокольное подключение и освобождает соединение до возврата.
	// readErr может содержать безопасные указания для продолжения этого же читателя.
	readErr := m.adapters[r.key.channel.Provider].Read(ctx, core.ChatReadRequest{
		Token: token, Key: r.key.channel, Credentials: credentials, ResumeToken: resumeToken,
	}, attempt)
	attempt.closeInput()
	// retry оставляет непрозрачную позицию на сервере и отделяет её от произвольной ошибки адаптера.
	var retry *core.ChatReadRetry
	_ = errors.As(readErr, &retry)
	m.mu.Lock()
	defer m.mu.Unlock()
	attempt.ingress.Store(nil)
	r.attempt = nil
	// wasConnected влияет только на сброс задержки, а не на счётчик попыток.
	wasConnected := attempt.connected
	if r.ctx.Err() == nil && !terminalReaderState(r.state) {
		// reason сохраняет уже сообщённую безопасную причину, когда адаптер успел её подтвердить.
		reason := r.reason
		if reason == core.ChatReasonNone {
			reason = core.ChatReasonNetwork
		}
		m.setReaderStatusLocked(r, core.ChatRetrying, reason)
	}
	return r.state, wasConnected, retry
}

// closeInput отзывает вход попытки в том же порядке, в котором Hub принимает сообщения и учитывает потери.
func (a *readerAttempt) closeInput() {
	// hub координирует отсечение входа с FIFO этой точной Session.
	hub := a.reader.key.session.hub
	hub.inputMu.Lock()
	a.ended.Store(true)
	hub.inputMu.Unlock()
}

// terminalReaderState отличает конечное состояние от допустимого восстановления.
func terminalReaderState(state core.ChatSourceState) bool {
	return state == core.ChatAuthRequired || state == core.ChatFailed || state == core.ChatStopped
}

// credentialStatus скрывает ошибки Store и владельца, сохраняя типизированную причину ожидания.
func credentialStatus(err error) (core.ChatSourceState, core.ChatReason) {
	if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrInvalid) || errors.Is(err, ErrCredentialRefreshConsumed) {
		return core.ChatAuthRequired, core.ChatReasonCredentials
	}
	return core.ChatRetrying, core.ChatReasonNetwork
}

// setReaderStatusLocked распространяет состояние общего читателя по независимым участникам.
func (m *Manager) setReaderStatusLocked(reader *managedReader, state core.ChatSourceState, reason core.ChatReason) {
	reader.state, reader.reason = state, reason
	// member обновляется лишь в пределах своего действующего поколения.
	for member := range reader.members {
		if m.sourceAttachedLocked(member) && slices.ContainsFunc(member.outputs, (*core.ChatOutputPublication).Published) {
			m.setSourceStatusLocked(member, state, reason)
		}
	}
}

// setSourceStatusLocked обновляет статус, публикуя отдельное событие при изменении значения.
func (m *Manager) setSourceStatusLocked(member *managedSource, state core.ChatSourceState, reason core.ChatReason) {
	if member.status.State == state && member.status.Reason == reason && member.status.ChangedAt.After(time.Time{}) {
		return
	}
	member.status.SourceID, member.status.State, member.status.Reason = member.value.ID, state, reason
	member.status.ChangedAt = time.Now().UTC().Truncate(time.Microsecond)
	_ = member.session.hub.setStatus(member.status)
}

// Message проверяет принадлежность callback и кладёт исходные доказательства в независимую очередь Hub.
// Точный допуск и выбор минимального Source выполняются при публикации, до присвоения порядка.
func (a *readerAttempt) Message(ctx context.Context, token core.ChatWorkerToken, incoming core.ChatIncomingMessage) error {
	if ctx == nil {
		return core.ErrInvalid
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// ingress замораживает исходные поколения и выходы, а key определяет фактический канал.
	ingress := a.ingress.Load()
	key := a.reader.key
	if token != a.token || ingress == nil || a.ended.Load() || !key.session.owner.Active() {
		return core.ErrStale
	}
	// sourceID определяет предварительную атрибуцию потери до помещения сообщения в очередь.
	sourceID := firstIngressSource(ingress)
	if sourceID == "" {
		return core.ErrStale
	}
	if incoming.Provider != key.channel.Provider || incoming.ChannelID != key.channel.ChannelID {
		return core.ErrInvalid
	}
	// Callback повторно проверяет только исходных участников непосредственно перед публикацией.
	return key.session.hub.enqueue(incoming, ingress.weight, func(stage func(core.ID) func(bool)) {
		a.publish(ingress, stage)
	}, chatInputAttribution{sourceJSONBytes: ingress.sourceJSONBytes, lossSource: sourceID, attemptEnded: &a.ended})
}

// firstIngressSource возвращает минимальный исходный Source ID с живым доказательством.
// Пустой ID означает, что входной снимок больше не разрешает приём сообщения.
func firstIngressSource(ingress *readerIngress) core.ID {
	// candidate перечисляет доказательства одного исходного поколения.
	for _, candidate := range ingress.members {
		// proof подтверждает конкретный публиковавший выход исходного поколения.
		for _, proof := range candidate.proofs {
			if proof.Published() {
				return candidate.sourceID
			}
		}
	}
	return ""
}

// publish проверяет пересечение исходных и текущих источников и выходов.
// Подготовка JSON выполняется вне медиа-блокировки. Конечная короткая проверка упорядочивает
// допуск с COMMIT медиакоманды и отзывом наблюдателей. Очередь приёма от этих блокировок независима.
func (a *readerAttempt) publish(ingress *readerIngress, stage func(core.ID) func(bool)) {
	// m удерживает неизменную конфигурационную видимость до фиксации проверенного кандидата.
	m := a.reader.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	// Завершение сетевой попытки или конечный статус закрывает новый приём.
	// Принятые сообщения ждут сверки исходных привязок с текущим логическим читателем.
	if !a.reader.readerCurrentLocked() {
		return
	}
	// original — очередной исходный участник в порядке возрастания Source ID.
	for _, original := range ingress.members {
		// member сохраняет само поколение привязки, а attached подтверждает принадлежность читателю.
		member := original.member
		_, attached := a.reader.members[member]
		if !attached || !m.sourceAttachedLocked(member) {
			continue
		}
		// proofs пересекает исходные и текущие удостоверения без подмены новым выходом.
		proofs := originalProofs(original.proofs, member.outputs)
		if len(proofs) == 0 {
			continue
		}
		// finish принимает конечное решение по подготовленной записи и освобождает видимость Hub.
		finish := stage(original.sourceID)
		if finish == nil {
			return
		}
		// live принимает результаты только для выбранных исходных удостоверений.
		live := make([]bool, len(proofs))
		// err также сообщает об отзыве точного родителя при сохранённом RUNNING.
		err := m.service.CheckChatPublications(m.ctx, member.session.owner, proofs, live)
		// admitted подтверждает, что хотя бы один исходный выход допустим после проверки.
		admitted := err == nil && slices.Contains(live, true)
		finish(admitted)
		if admitted {
			return
		}
		if err != nil {
			return
		}
	}
}

// originalProofs сохраняет только ещё присутствующие точные удостоверения из исходного снимка.
func originalProofs(original, current []*core.ChatOutputPublication) []*core.ChatOutputPublication {
	// result содержит допустимых кандидатов в исходном порядке.
	result := make([]*core.ChatOutputPublication, 0, len(original))
	// proof нельзя заменить актуальным наблюдателем того же логического Output.
	for _, proof := range original {
		if proof.Published() && slices.Contains(current, proof) {
			result = append(result, proof)
		}
	}
	return result
}

// currentLocked проверяет точную попытку и её родительскую эпоху под блокировкой Manager.
func (a *readerAttempt) currentLocked() bool {
	// reader остаётся действующим до закрытия последней привязки.
	reader := a.reader
	return reader.attempt == a && !a.ended.Load() && a.ingress.Load() != nil && reader.readerCurrentLocked()
}

// liveMembersLocked возвращает только точных владельцев, подтверждённых общей RuntimeAuthority.
func (a *readerAttempt) liveMembersLocked(ctx context.Context) ([]*managedSource, error) {
	// m предоставляет защищённый конфигурационный допуск и короткую медиа-проверку.
	m := a.reader.manager
	if !a.currentLocked() {
		return nil, core.ErrStale
	}
	// members и offsets отображают позиции исходных доказательств на активных участников.
	members := make([]*managedSource, 0, len(a.reader.members))
	offsets := []int{0}
	// proofs объединяет удостоверения всех незаблокированных текущих участников.
	var proofs []*core.ChatOutputPublication
	// member может временно исключаться только своей конфигурационной операцией.
	for member := range a.reader.members {
		if m.sourceAttachedLocked(member) {
			members = append(members, member)
			proofs = append(proofs, member.outputs...)
			offsets = append(offsets, len(proofs))
		}
	}
	// live содержит результат общей проверки по каждой исходной позиции.
	live := make([]bool, len(proofs))
	// err сообщает об отзыве родителя либо прекращении запроса.
	if err := m.service.CheckChatPublications(ctx, a.reader.key.session.owner, proofs, live); err != nil {
		return nil, err
	}
	// result исключает участников, потерявших все исходные удостоверения.
	result := make([]*managedSource, 0, len(members))
	// i индексирует диапазон удостоверений, member обозначает его владельца.
	for i, member := range members {
		if slices.Contains(live[offsets[i]:offsets[i+1]], true) {
			result = append(result, member)
		}
	}
	if len(result) == 0 {
		return nil, core.ErrStale
	}
	return result, nil
}

// Status применяет безопасный статус точной попытки к подтверждённым участникам.
func (a *readerAttempt) Status(ctx context.Context, token core.ChatWorkerToken, state core.ChatSourceState, reason core.ChatReason) error {
	if ctx == nil || state.Validate() != nil || reason.Validate() != nil {
		return core.ErrInvalid
	}
	if token != a.token {
		return core.ErrStale
	}
	// m удерживает текущие привязки до применения статуса.
	m := a.reader.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	// members содержит подтверждённых владельцев, err сообщает об отзыве либо отмене.
	members, err := a.liveMembersLocked(ctx)
	if err != nil {
		return err
	}
	// terminal закрывает вход новых сообщений до появления конечного статуса в Hub.
	terminal := terminalReaderState(state)
	if terminal {
		a.closeInput()
		a.ingress.Store(nil)
	}
	if state == core.ChatConnected {
		a.connected = true
		// now фиксирует подтверждение подписки даже при отсутствии сообщений аудитории.
		now := time.Now().UTC().Truncate(time.Microsecond)
		// member получает отдельную временную метку подтверждённого события.
		for _, member := range members {
			member.status.LastEventAt = &now
		}
	}
	a.reader.state, a.reader.reason = state, reason
	// member обновляется лишь после подтверждения хотя бы одного его выхода.
	for _, member := range members {
		m.setSourceStatusLocked(member, state, reason)
	}
	if terminal {
		a.cancel()
	}
	return nil
}

// Gap сообщает об upstream-пропуске отдельно от истории аудитории после проверки точного владельца.
func (a *readerAttempt) Gap(ctx context.Context, token core.ChatWorkerToken, reason core.ChatReason) error {
	if ctx == nil || reason.Validate() != nil {
		return core.ErrInvalid
	}
	if token != a.token {
		return core.ErrStale
	}
	// m удерживает точный допуск конфигурации до записи сигнала в ленту.
	m := a.reader.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	// err сообщает об отзыве попытки либо прекращении запроса.
	if _, err := a.liveMembersLocked(ctx); err != nil {
		return err
	}
	return a.reader.key.session.hub.gap(reason)
}
