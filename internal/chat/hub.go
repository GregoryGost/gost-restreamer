package chat

import (
	"context"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// chatHub публикует ленту запуска или локального генератора и удерживает её ограниченный хвост.
// Вход и выдача защищены разными блокировками. Обработчик входа не ждёт
// проверки полномочий публикации, кодирования или чтения подписчика.
type chatHub struct {
	// limits задаёт неизменяемые бюджеты одной ленты.
	limits core.ChatLimits
	// now возвращает серверное время для принятия сообщений и срока дедупликации.
	now func() time.Time
	// streamID связывает ленту с существующим потоком.
	streamID core.ID
	// fence содержит точное выполнение, которому принадлежит лента.
	fence core.RuntimeSessionFence
	// test связывает тестовую эпоху с допуском до эфира. У реальной ленты указатель пуст.
	test *core.ChatTestAdmission
	// key подписывает cursor только для этой эпохи.
	key chatCursorKey
	// wake уведомляет единственного обработчика об изменении входа.
	wake chan struct{}
	// stopping закрывается при отзыве входа.
	stopping chan struct{}
	// done закрывается после выхода обработчика и записи терминального события подписчикам.
	done chan struct{}
	// inputMu упорядочивает приём и защищает входную очередь и счётчик отказов.
	inputMu sync.Mutex
	// input содержит допущенные Manager сообщения с исходными доказательствами.
	input chatBuffer[chatQueuedMessage]
	// inputEstimate содержит верхнюю оценку суммы encoded bytes принятого входа.
	inputEstimate uint64
	// inputLost учитывает превышения входного бюджета до следующей записи gap.
	inputLost uint64
	// inputLostBySource относит часть тех же потерь к известным источникам. Число ключей ограничено.
	inputLostBySource map[core.ID]uint64
	// mu упорядочивает историю, позиции, статусы и данные подписчиков.
	mu sync.Mutex
	// eventOrder задаёт последний опубликованный cursor в пределах эпохи.
	eventOrder uint64
	// messageOrder содержит последний выданный номер сообщения в пределах эпохи.
	messageOrder uint64
	// testSamples хранит ещё не выданные индексы случайной последовательности тестовых реплик.
	// Срез меняется под mu только в тестовой ленте.
	testSamples []int
	// clearedThrough отмечает последний номер сообщения, удалённого командой очистки.
	clearedThrough uint64
	// changed закрывается при каждом публичном изменении и не заменяется после закрытия ленты.
	changed chan struct{}
	// closed запрещает чтение снятой эпохи и добавление новых подписчиков.
	closed bool
	// history хранит хвост сообщений и учитывает полные encoded bytes.
	history chatBuffer[core.ChatMessage]
	// replay хранит непрерывный хвост журнала с размерами полных JSON событий.
	replay chatBuffer[chatVisibleEvent]
	// statuses хранит оперативные состояния источников отдельно от сообщений.
	statuses map[core.ID]chatHubStatus
	// statusBytes учитывает закодированные статусы и разделители массива.
	statusBytes uint64
	// subscribers содержит открытые подписки точной эпохи, включая отставшие.
	subscribers map[*ChatSubscription]struct{}
	// dedupe содержит срок действия каждого удерживаемого ключа.
	dedupe map[chatDedupeKey]time.Time
	// dedupeOrder упорядочивает удерживаемые ключи для очистки окна.
	dedupeOrder chatBuffer[chatDedupeEntry]
}

// chatInputAttribution сообщает Hub о первоначальном допуске для оценки размера и учёта отказов.
type chatInputAttribution struct {
	// sourceJSONBytes ограничивает экранированный размер идентификаторов исходных кандидатов.
	sourceJSONBytes uint64
	// lossSource указывает минимальный подходящий Source при приёме, когда запись может не попасть в очередь.
	lossSource core.ID
	// attemptEnded закрывает допуск физической подписки. Изменение упорядочено с входной очередью.
	attemptEnded *atomic.Bool
}

// chatQueuedMessage удерживает сообщение до вызова Manager для проверки его первоначального допуска.
type chatQueuedMessage struct {
	// incoming принадлежит Hub и отделено от буферов адаптера.
	incoming core.ChatIncomingMessage
	// receivedAt содержит время допуска записи во входную очередь с точностью до микросекунд.
	receivedAt time.Time
	// queuedAt хранит время ожидания flush с монотонной составляющей.
	queuedAt time.Time
	// estimate ограничивает сверху потенциальный JSON до выбора точного источника.
	estimate uint64
	// publish проверяет исходные доказательства Manager и окончательно принимает либо отклоняет staged запись.
	publish func(stage func(core.ID) func(bool))
}

// chatVisibleEvent хранит неизменяемое опубликованное событие с размером для каждой очереди.
type chatVisibleEvent struct {
	// event содержит вид и полезную нагрузку с полным cursor.
	event core.ChatEvent
	// position содержит точное число для проверки непрерывности replay без разбора cursor.
	position uint64
	// encodedBytes учитывается в общем журнале replay.
	encodedBytes uint64
	// frameBytes учитывает служебные строки полного кадра SSE.
	frameBytes uint64
	// logicalBytes учитывается в очереди отдельного подписчика.
	logicalBytes uint64
}

// chatHubStatus хранит безопасный оперативный статус с измеренным весом.
type chatHubStatus struct {
	// status содержит текущую неизменяемую запись.
	status core.ChatSourceStatus
	// size учитывает полный encoded JSON статуса и один разделитель массива.
	size uint64
}

// chatDedupeKey различает сообщения по точной беседе внутри одной Session.
type chatDedupeKey struct {
	// provider отделяет одинаковые ID разных платформ.
	provider core.ChatProvider
	// channelID содержит фактический ID платформенной беседы.
	channelID string
	// messageID содержит исходный ID сообщения.
	messageID string
}

// chatDedupeEntry связывает ключ с его сроком для удаления при заполнении либо истечении окна.
type chatDedupeEntry struct {
	// key определяет точную платформенную запись.
	key chatDedupeKey
	// expires ограничивает удержание записи независимо от объёма трафика.
	expires time.Time
}

// newChatHub запускает единственный обработчик одобренной ленты.
// Владелец обязан вызвать Close или stop и дождаться Done, прежде чем освободить слот Session.
func newChatHub(limits core.ChatLimits, now func() time.Time, streamID core.ID, fence core.RuntimeSessionFence) (*chatHub, error) {
	if limits.Validate() != nil || now == nil || streamID.Validate() != nil || fence.SessionID.Validate() != nil || len(fence.SessionID) > core.ChatIDBytes {
		return nil, core.ErrInvalid
	}
	// h удерживает состояние реальной эпохи, err сообщает об отказе при создании её курсора.
	h, err := newChatHubState(limits, now, streamID, fence, nil)
	if err != nil {
		return nil, err
	}
	go h.run()
	return h, nil
}

// newChatHubState создаёт очереди и независимый курсор для уже проверенных параметров ленты.
// Обработчик входа запускает только владелец реальной ленты.
func newChatHubState(limits core.ChatLimits, now func() time.Time, streamID core.ID, fence core.RuntimeSessionFence, test *core.ChatTestAdmission) (*chatHub, error) {
	// key задаёт новую эпоху, err сообщает об ошибке создания её ключа.
	key, err := newChatCursorKey()
	if err != nil {
		return nil, err
	}
	if uint64(len(key.cursor("", 0))) > limits.CursorBytes {
		return nil, core.ErrInvalid
	}
	return &chatHub{
		limits: limits, now: now, streamID: streamID, fence: fence, test: test, key: key,
		wake: make(chan struct{}, 1), stopping: make(chan struct{}), done: make(chan struct{}),
		changed: make(chan struct{}), statuses: make(map[core.ID]chatHubStatus),
		subscribers: make(map[*ChatSubscription]struct{}), dedupe: make(map[chatDedupeKey]time.Time),
	}, nil
}

// chatTestRevoked проверяет отзыв допуска без ожидания сверки Manager.
func chatTestRevoked(test *core.ChatTestAdmission) bool {
	if test == nil {
		return false
	}
	select {
	case <-test.Done():
		return true
	default:
		return false
	}
}

// testRevoked закрывает видимость тестовой эпохи с момента отзыва допуска или её остановки.
func (h *chatHub) testRevoked() bool {
	return h.test != nil && (h.isStopping() || chatTestRevoked(h.test))
}

// publishTest синхронно публикует одну реплику из перемешанного набора без очереди адаптеров.
// Позиция набора, история и журнал меняются вместе под действующим допуском до эфира.
// ctx ограничивает ожидание допуска. Если развёрнутая реплика сама по себе не помещается в
// историю, журнал или очередь клиента, публикуется короткая реплика той же площадки.
func (h *chatHub) publishTest(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.testRevoked() {
		return core.ErrConflict
	}
	if h.messageOrder == math.MaxUint64 {
		return core.ErrNotReady
	}
	if len(h.testSamples) == 0 {
		h.testSamples = rand.Perm(len(testChatSamples))
	}
	// sample задаёт следующую площадку и реплику. message добавляет номер и серверное время приёма.
	sample := testChatSamples[h.testSamples[0]]
	message := core.ChatMessage{
		ChatIncomingMessage: sample.incoming(), TestProvider: sample.provider,
		Seq: h.messageOrder + 1, ReceivedAt: h.now().UTC().Truncate(time.Microsecond),
	}
	// budget ограничивает JSON одной записи в истории.
	budget := min(h.limits.MessageBytes, h.limits.History.Bytes)
	// event содержит подготовленную запись журнала, size задаёт размер сообщения в истории,
	// err сообщает об ошибке кодирования, проверки полей или исчерпании номера события.
	event, size, err := h.prepareTestEventLocked(&message)
	if err != nil {
		return err
	}
	if size > budget || event.encodedBytes > h.limits.Replay.Bytes ||
		event.frameBytes > h.limits.SSEFrameBytes || event.logicalBytes > h.limits.SSEQueue.Bytes {
		message.Author = core.ChatAuthor{DisplayName: "Ли"}
		message.Text = "👋"
		message.Parts, message.Badges = nil, nil
		event, size, err = h.prepareTestEventLocked(&message)
		if err != nil {
			return err
		}
		if size > budget {
			return core.ErrNotReady
		}
	}
	// Функция продвигает позицию набора и публикует запись под допуском RuntimeAuthority.
	return h.test.Publish(ctx, func() {
		h.testSamples = h.testSamples[1:]
		h.messageOrder = message.Seq
		h.history.retain(message, size, h.limits.History)
		h.publishLocked(event)
	})
}

// prepareTestEventLocked кодирует message и считает фактические размеры записи для истории,
// журнала и подписчика. Позиция резервируется только при последующей публикации.
func (h *chatHub) prepareTestEventLocked(message *core.ChatMessage) (chatVisibleEvent, uint64, error) {
	// payload содержит JSON сообщения без оболочки события, err сообщает об ошибке сериализации.
	payload, err := chatJSONMessage(*message)
	if err != nil {
		return chatVisibleEvent{}, 0, err
	}
	// event содержит рассчитанные размеры события и его позицию, err передаёт причину отказа.
	event, err := h.prepareEventLocked(core.ChatEvent{Kind: core.ChatEventMessage, Message: message}, payload)
	return event, uint64(len(payload)), err
}

// enqueue принимает неизменяемую копию сообщения и держит исходное доказательство в том же входном бюджете.
// Необязательная attribution уточняет размер идентификатора, источник отказанной записи и допуск попытки.
// Недопущенные по размеру записи увеличивают общий счётчик потерь этой Session.
func (h *chatHub) enqueue(incoming core.ChatIncomingMessage, evidenceWeight uint64, publish func(func(core.ID) func(bool)), attribution ...chatInputAttribution) error {
	if publish == nil || len(attribution) > 1 {
		return core.ErrInvalid
	}
	// sourceWidth ограничивает длину JSON-строки исходного SourceID, lossSource адресует его счётчик.
	// attemptEnded позволяет отклонить отозванный вызов после подготовки входных данных.
	sourceWidth := uint64(6 * core.ChatIDBytes)
	var lossSource core.ID
	var attemptEnded *atomic.Bool
	if len(attribution) == 1 {
		sourceWidth = min(attribution[0].sourceJSONBytes, sourceWidth)
		lossSource = attribution[0].lossSource
		attemptEnded = attribution[0].attemptEnded
	}
	if len(incoming.Parts) > core.ChatMaxParts || len(incoming.Badges) > core.ChatMaxBadges {
		return h.rejectInput(lossSource, attemptEnded)
	}
	// weight учитывает структурные данные до проверки очереди, err сообщает о переполнении размера.
	weight, err := incoming.LogicalBytes()
	if err != nil || weight > core.ChatMessageBytes {
		return h.rejectInput(lossSource, attemptEnded)
	}
	if err = incoming.Validate(); err != nil {
		return err
	}
	// reserved включает ограниченный отдельно вес исходных доказательств Manager.
	reserved, err := core.CheckedChatBytes(weight, evidenceWeight)
	if err != nil || reserved > h.limits.InputQueue.Bytes {
		return h.rejectInput(lossSource, attemptEnded)
	}
	// estimate использует полное экранирование входных полей и резервирует место для SourceID.
	// Временная метка и номер заданы с наибольшей длиной представления в JSON.
	estimate, err := h.estimateMessage(incoming, sourceWidth)
	if err != nil {
		return h.rejectInput(lossSource, attemptEnded)
	}
	// owned содержит отдельные данные адаптера, которые можно удерживать после возврата.
	owned := chatOwnIncoming(incoming)
	h.inputMu.Lock()
	defer h.inputMu.Unlock()
	if h.isStopping() || attemptEnded != nil && attemptEnded.Load() {
		return core.ErrStale
	}
	// sum содержит проверенную сумму закодированных оценок очереди.
	sum, err := core.CheckedChatBytes(h.inputEstimate, estimate)
	if err != nil || !h.input.canPush(reserved, h.limits.InputQueue) {
		h.recordInputLossLocked(lossSource)
		return core.ErrNotReady
	}
	// queued фиксирует FIFO-позицию и время приёма в одной критической секции.
	queued := chatQueuedMessage{
		incoming: owned, receivedAt: h.now().UTC().Truncate(time.Microsecond), queuedAt: time.Now(),
		estimate: estimate, publish: publish,
	}
	h.input.push(queued, reserved)
	h.inputEstimate = sum
	h.notifyInputLocked()
	return nil
}

// estimateMessage возвращает верхнюю границу полных encoded bytes до выборки первоначального Source.
// Верхняя оценка позволяет проснуться до срока при большом экранированном входе.
func (h *chatHub) estimateMessage(incoming core.ChatIncomingMessage, sourceWidth uint64) (uint64, error) {
	// placeholder содержит все входные поля, точную Session и максимально длинные серверные метаданные.
	placeholder := core.ChatMessage{
		ChatIncomingMessage: incoming, SessionID: h.fence.SessionID, Seq: math.MaxUint64,
		ReceivedAt: time.Unix(0, 999999000).UTC(),
	}
	// data содержит JSON с пустым SourceID, err определяет отказ кодирования.
	data, err := chatJSONMessage(placeholder)
	if err != nil {
		return 0, err
	}
	return core.CheckedChatBytes(uint64(len(data)), sourceWidth)
}

// rejectInput регистрирует потерю записи действующей попытки, которую невозможно поместить во входной бюджет.
func (h *chatHub) rejectInput(sourceID core.ID, attemptEnded *atomic.Bool) error {
	h.inputMu.Lock()
	defer h.inputMu.Unlock()
	if h.isStopping() || attemptEnded != nil && attemptEnded.Load() {
		return core.ErrStale
	}
	h.recordInputLossLocked(sourceID)
	return core.ErrNotReady
}

// recordInputLossLocked учитывает отказ даже при заполненной очереди и будит обработчик. При
// исчерпании числа ключей сохраняется общий итог, а новый ключ в этот пакет не добавляется.
func (h *chatHub) recordInputLossLocked(sourceID core.ID) {
	h.inputLost = chatAddLoss(h.inputLost, 1)
	if sourceID != "" && len(sourceID) <= core.ChatIDBytes {
		// previous содержит уже известные потери того же источника в текущем пакете.
		previous := h.inputLostBySource[sourceID]
		if previous != 0 || len(h.inputLostBySource) < h.limits.SourcesPerSession {
			if h.inputLostBySource == nil {
				h.inputLostBySource = make(map[core.ID]uint64)
			}
			if previous == 0 {
				sourceID = core.ID(strings.Clone(string(sourceID)))
			}
			h.inputLostBySource[sourceID] = chatAddLoss(previous, 1)
		}
	}
	h.notifyInputLocked()
}

// takeInputLossLocked забирает счётчики под блокировкой входа и отделяет следующий пакет потерь.
func (h *chatHub) takeInputLossLocked() (uint64, map[core.ID]uint64) {
	// lost хранит полный итог, bySource сообщает об отказах с известным источником.
	lost, bySource := h.inputLost, h.inputLostBySource
	h.inputLost, h.inputLostBySource = 0, nil
	return lost, bySource
}

// publishInputLossLocked публикует общий разрыв и обновляет счётчики ещё видимых источников.
// Изменения статусов сохраняются в журнале в устойчивом порядке.
func (h *chatHub) publishInputLossLocked(lost uint64, bySource map[core.ID]uint64) {
	_ = h.appendGapLocked(core.ChatReasonLimit, lost)
	// ids задаёт порядок обновления видимых счётчиков.
	ids := make([]core.ID, 0, len(bySource))
	// id адресует очередной накопленный локальный отказ.
	for id := range bySource {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	// id адресует источник, которому уже приписан известный вклад в общий разрыв.
	for _, id := range ids {
		h.addSourceLossLocked(id, bySource[id])
	}
}

// notifyInputLocked объединяет сигналы очереди без ожидания обработчика.
func (h *chatHub) notifyInputLocked() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// isStopping проверяет отзыв приёма и может вызываться при любой из блокировок Hub.
func (h *chatHub) isStopping() bool {
	select {
	case <-h.stopping:
		return true
	default:
		return false
	}
}

// stop прекращает новые admissions и будит обработчик, не ожидая проверки Manager.
func (h *chatHub) stop() {
	h.inputMu.Lock()
	defer h.inputMu.Unlock()
	if !h.isStopping() {
		close(h.stopping)
	}
}

// Close отзывает приём и ждёт физического завершения Hub до отмены ctx.
func (h *chatHub) Close(ctx context.Context) error {
	if ctx == nil {
		return core.ErrInvalid
	}
	h.stop()
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done закрывается после выпуска терминального события и освобождения собственных очередей Hub.
func (h *chatHub) Done() <-chan struct{} { return h.done }

// flushDelay определяет срок от первой записи. Отказ входа и пороги запускают flush сразу.
func (h *chatHub) flushDelay() (time.Duration, bool) {
	h.inputMu.Lock()
	defer h.inputMu.Unlock()
	if h.inputLost != 0 || h.input.len() >= h.limits.FlushMessages || h.inputEstimate >= h.limits.FlushBytes {
		return 0, true
	}
	if h.input.len() == 0 {
		return 0, false
	}
	return h.limits.FlushInterval - time.Since(h.input.at(0).queuedAt), true
}

// run ждёт первого из порогов пакетизации, отзыв входа либо уведомление о новой записи.
// Обработчик освобождает вход перед проверкой каждого доказательства и сохраняет FIFO сообщений.
func (h *chatHub) run() {
	defer h.finish()
	for !h.isStopping() {
		// delay содержит оставшееся ожидание, pending показывает наличие работы.
		delay, pending := h.flushDelay()
		if pending && delay <= 0 {
			h.flush()
			continue
		}
		// expiry содержит время до освобождения старейшего ключа, expiring показывает наличие ключей.
		expiry, expiring := h.expireDedupe()
		if expiring && (!pending || expiry < delay) {
			delay, pending = expiry, true
		}
		// timer живёт только до ближайшего изменения, deadline остаётся nil при пустом входе.
		var timer *time.Timer
		var deadline <-chan time.Time
		if pending {
			timer = time.NewTimer(delay)
			deadline = timer.C
		}
		select {
		case <-h.stopping:
		case <-h.wake:
		case <-deadline:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// takeInput отдаёт одну FIFO-запись до лимита текущего пакета и освобождает вход до проверки Manager.
func (h *chatHub) takeInput(bytes uint64, count int) (chatQueuedMessage, bool) {
	h.inputMu.Lock()
	defer h.inputMu.Unlock()
	if h.input.len() == 0 || h.isStopping() {
		return chatQueuedMessage{}, false
	}
	if count != 0 && h.input.at(0).estimate > h.limits.FlushBytes-bytes {
		return chatQueuedMessage{}, false
	}
	// queued содержит извлечённую запись и её первоначальное доказательство.
	queued := h.input.pop()
	h.inputEstimate -= queued.estimate
	return queued, true
}

// flush последовательно передаёт ограниченный пакет в Manager и публикует сводку известных потерь.
func (h *chatHub) flush() {
	// bytes учитывает верхнюю оценку байтов текущего пакета до выбора Source.
	var bytes uint64
	// count содержит число записей, обработанных в текущем пакете.
	for count := 0; count < h.limits.FlushMessages && bytes < h.limits.FlushBytes; count++ {
		// entry принимает очередную запись, ok сообщает о наличии места в этом пакете.
		entry, ok := h.takeInput(bytes, count)
		if !ok {
			break
		}
		bytes += entry.estimate
		// Анонимная функция связывает выбор источника Manager с этой конкретной FIFO-записью.
		entry.publish(func(sourceID core.ID) func(bool) { return h.stage(entry, sourceID) })
	}
	h.inputMu.Lock()
	// lost содержит накопленные отказы, bySource распределяет известные потери по источникам.
	lost, bySource := h.takeInputLossLocked()
	h.inputMu.Unlock()
	if lost != 0 {
		h.mu.Lock()
		if !h.closed {
			h.publishInputLossLocked(lost, bySource)
		}
		h.mu.Unlock()
	}
}

// stage удерживает видимость ленты на время финальной проверки Manager.
// Все проверки структуры и байтов выполняются до возврата finish. Manager
// вызывает finish ровно один раз с результатом проверки исходного доказательства.
// Отказ оставляет историю, дедупликацию и порядок неизменными.
func (h *chatHub) stage(entry chatQueuedMessage, sourceID core.ID) func(bool) {
	h.mu.Lock()
	// finishNoChange освобождает видимость при отзыве либо дедупликации записи.
	finishNoChange := func(bool) { h.mu.Unlock() }
	if h.closed || h.isStopping() {
		return finishNoChange
	}
	if h.eventOrder >= math.MaxUint64-1 || h.messageOrder == math.MaxUint64 {
		h.stop()
		return finishNoChange
	}
	// key фиксирует точную беседу независимо от приписанного ей Source.
	key := chatDedupeKey{provider: entry.incoming.Provider, channelID: entry.incoming.ChannelID, messageID: entry.incoming.ProviderMessageID}
	// at определяет окно дедупликации к моменту публикации.
	at := h.now()
	if key.messageID != "" && h.dedupe[key].After(at) {
		return finishNoChange
	}
	// message содержит все назначенные поля и временно использует следующий номер.
	message := core.ChatMessage{
		ChatIncomingMessage: entry.incoming, SessionID: h.fence.SessionID, ChatSourceID: sourceID,
		Seq: h.messageOrder + 1, ReceivedAt: entry.receivedAt,
	}
	// payload содержит полную кодировку сообщения, err сообщает об отказе кодирования.
	payload, err := chatJSONMessage(message)
	if err != nil || uint64(len(payload)) > h.limits.MessageBytes || uint64(len(payload)) > h.limits.History.Bytes {
		return h.finishMessageLoss(sourceID)
	}
	// visible содержит подготовленное событие, err сообщает о недопустимой записи.
	visible, err := h.prepareEventLocked(core.ChatEvent{Kind: core.ChatEventMessage, Message: &message}, payload)
	if err != nil {
		return h.finishMessageLoss(sourceID)
	}
	// status содержит возможное обновление времени последнего успешного события этого источника.
	status := h.prepareMessageStatusLocked(message)
	// Возвращаемая функция публикует уже подготовленные данные только при точном финальном допуске.
	return func(commit bool) {
		defer h.mu.Unlock()
		if !commit || h.isStopping() {
			return
		}
		h.messageOrder = message.Seq
		h.rememberLocked(key, at)
		h.history.retain(message, uint64(len(payload)), h.limits.History)
		if status.size != 0 {
			h.storeStatusLocked(status)
		}
		h.publishLocked(visible)
	}
}

// finishMessageLoss освобождает видимость и учитывает превышение только при подтверждённом Manager допуске.
func (h *chatHub) finishMessageLoss(sourceID core.ID) func(bool) {
	// Возвращаемая функция фиксирует потерю и обновляет связанный статус после финальной проверки.
	return func(commit bool) {
		defer h.mu.Unlock()
		if !commit || h.isStopping() {
			return
		}
		_ = h.appendGapLocked(core.ChatReasonLimit, 1)
		h.addSourceLossLocked(sourceID, 1)
	}
}

// addSourceLossLocked обновляет существующий статус. Отзыв источника не восстанавливает его статус.
func (h *chatHub) addSourceLossLocked(sourceID core.ID, lost uint64) {
	// current содержит существующий статус, ok показывает, что источник ещё представлен в Hub.
	current, ok := h.statuses[sourceID]
	if ok {
		current.status.Dropped = chatAddLoss(current.status.Dropped, lost)
		_ = h.setStatusLocked(current.status)
	}
}

// rememberLocked удерживает непустой ключ до срока либо вытеснения по count/bytes.
func (h *chatHub) rememberLocked(key chatDedupeKey, at time.Time) {
	if key.messageID == "" {
		return
	}
	// size учитывает фиксированный вес и строки точного ключа.
	size := uint64(256 + len(key.provider) + len(key.channelID) + len(key.messageID))
	if size > h.limits.Dedupe.Bytes {
		return
	}
	for h.dedupeOrder.len() != 0 && (!h.dedupeOrder.at(0).expires.After(at) || !h.dedupeOrder.canPush(size, h.limits.Dedupe)) {
		h.dropDedupeLocked()
	}
	// expires задаёт новый край удержания ключа.
	expires := at.Add(h.limits.DedupeTTL)
	h.dedupe[key] = expires
	h.dedupeOrder.push(chatDedupeEntry{key: key, expires: expires}, size)
}

// dropDedupeLocked освобождает голову окна и сохраняет более свежий срок при повторной записи ключа.
func (h *chatHub) dropDedupeLocked() {
	// old содержит старейшую запись, которая могла быть заменена после изменения часов.
	old := h.dedupeOrder.pop()
	if h.dedupe[old.key].Equal(old.expires) {
		delete(h.dedupe, old.key)
	}
}

// expireDedupe освобождает истёкшие ключи даже в отсутствие дальнейшего входа.
// Возвращённый срок позволяет обработчику проснуться при следующем истечении.
func (h *chatHub) expireDedupe() (time.Duration, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dedupeOrder.len() == 0 {
		return 0, false
	}
	// now служит общей границей удаления и расчёта следующего срока.
	now := h.now()
	for h.dedupeOrder.len() != 0 {
		// delay содержит остаток срока головного ключа.
		delay := h.dedupeOrder.at(0).expires.Sub(now)
		if delay > 0 {
			return delay, true
		}
		h.dropDedupeLocked()
	}
	return 0, false
}
