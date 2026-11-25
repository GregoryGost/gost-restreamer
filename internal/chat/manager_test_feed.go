package chat

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// managedTestFeed удерживает историю, даже когда таймер генерации остановлен.
type managedTestFeed struct {
	// hub хранит тестовую эпоху и её допуск до эфира.
	hub *chatHub
	// waiting задаёт позицию ожидания, с которой доступны события с начала этой эпохи.
	waiting core.ChatCursor
	// run содержит текущий таймер либо nil после его завершения. Поле защищено Manager.mu.
	run *chatTestRun
}

// chatTestRun задаёт срок жизни одного запуска генератора в удерживаемой ленте.
type chatTestRun struct {
	// interval задаёт период между публикациями до остановки.
	interval time.Duration
	// ctx запрещает очередную публикацию после отмены, cancel запрашивает остановку.
	ctx    context.Context
	cancel context.CancelFunc
	// done закрывается после освобождения таймера и снятия записи о запуске.
	done chan struct{}
}

// waitingChatClear сохраняет только последнюю очистку до создания Hub.
type waitingChatClear struct {
	// before содержит курсор ожидания перед первой очисткой этой записи.
	before core.ChatCursor
	// first и last ограничивают позиции подписанных курсоров этой группы очисток.
	first, last uint64
}

// StartTest запускает один генератор выбранного Stream и сразу публикует его первую запись.
// Повторная команда возвращает текущий период. После StopTest история пополняется в той же эпохе.
func (m *Manager) StartTest(ctx context.Context, streamID core.ID, interval time.Duration) (time.Duration, error) {
	if streamID.Validate() != nil || interval < 100*time.Millisecond || interval > time.Minute || interval%time.Millisecond != 0 {
		return 0, core.ErrInvalid
	}
	// operation прекращает ожидание при отмене HTTP или Manager, finish освобождает регистрацию, err сообщает об отказе допуска.
	operation, finish, err := m.beginOperation(ctx)
	if err != nil {
		return 0, err
	}
	defer finish()
	if err = m.takeTestMutation(operation); err != nil {
		return 0, err
	}
	// Функция завершает команду на всех путях возврата, включая отказ SQLite.
	defer func() { <-m.testMutation }()
	// proof допускает только существующий Stream без RUNNING, err передаёт причину отказа.
	proof, err := m.service.AdmitChatTest(operation, streamID)
	if err != nil {
		return 0, err
	}
	for {
		m.mu.Lock()
		// feed содержит действующую эпоху или nil. Вызов одновременно освобождает отозванную эпоху.
		feed := m.currentTestLocked(streamID)
		// session защищает от выбора тестовой ленты при действующем владельце, err сообщает о недоступности проверки.
		session, err := m.currentStreamLocked(operation, streamID)
		if err != nil || chatTestRevoked(proof) || session != nil || operation.Err() != nil || m.ctx.Err() != nil {
			m.mu.Unlock()
			if feed == nil || feed.hub.test != proof {
				proof.Close()
			}
			switch {
			case err != nil:
				return 0, err
			case operation.Err() != nil:
				return 0, operation.Err()
			case m.ctx.Err() != nil:
				return 0, core.ErrClosed
			default:
				return 0, core.ErrConflict
			}
		}
		if feed == nil {
			// runErr сообщает об отказе бюджета, подготовки Hub или публикации первого сообщения.
			runErr := m.newTestFeedLocked(operation, streamID, interval, proof)
			m.mu.Unlock()
			if runErr != nil {
				proof.Close()
				return 0, runErr
			}
			return interval, nil
		}
		if feed.run == nil {
			err = m.startTestRunLocked(operation, feed, interval)
			m.mu.Unlock()
			if err != nil {
				return 0, err
			}
			return interval, nil
		}
		// run может ещё завершаться, если предыдущий запрос Stop прекратил ожидание по своему сроку.
		run := feed.run
		m.mu.Unlock()
		if run.ctx.Err() == nil {
			return run.interval, nil
		}
		select {
		case <-run.done:
		case <-operation.Done():
			return 0, operation.Err()
		}
	}
}

// newTestFeedLocked выделяет отдельное от реальных лент место и регистрирует эпоху после первого сообщения.
func (m *Manager) newTestFeedLocked(ctx context.Context, streamID core.ID, interval time.Duration, proof *core.ChatTestAdmission) error {
	if m.testHubs >= m.limits.Sessions {
		return core.ErrBusy
	}
	// hub хранит те же ограниченные очереди, что и рабочая лента, err сообщает об отказе создания курсора.
	hub, err := newChatHubState(m.limits, time.Now, streamID, core.RuntimeSessionFence{}, proof)
	if err != nil {
		return err
	}
	// feed связывает тестовую эпоху с курсором ожидания, чтобы открытая SSE не пропустила первую запись.
	feed := &managedTestFeed{hub: hub, waiting: m.waitingCursorLocked(streamID)}
	if err = m.startTestRunLocked(ctx, feed, interval); err != nil {
		return err
	}
	m.testFeeds[streamID] = feed
	m.testHubs++
	m.signalChangedLocked()
	// Callback отслеживает отзыв именно этой эпохи даже при остановленном таймере.
	m.work.Go(func() { m.watchTestFeed(feed) })
	return nil
}

// startTestRunLocked публикует первую запись под контекстом команды и запускает принадлежащий Manager таймер.
func (m *Manager) startTestRunLocked(ctx context.Context, feed *managedTestFeed, interval time.Duration) error {
	// err оставляет прежнюю ленту доступной при неудачной публикации.
	if err := feed.hub.publishTest(ctx); err != nil {
		return err
	}
	// runCtx и cancel отделяют срок будущих публикаций от срока запроса StartTest.
	runCtx, cancel := context.WithCancel(m.ctx)
	// run позволяет StopTest дождаться завершения именно этого таймера.
	run := &chatTestRun{interval: interval, ctx: runCtx, cancel: cancel, done: make(chan struct{})}
	feed.run = run
	// Callback выпускает повторные события и освобождает запись о таймере при выходе.
	m.work.Go(func() { m.generateTestMessages(feed, run) })
	return nil
}

// generateTestMessages передаёт Hub очередной тик и прекращает работу при отмене либо отказе публикации.
func (m *Manager) generateTestMessages(feed *managedTestFeed, run *chatTestRun) {
	// Callback освобождает запуск и подтверждает StopTest, что таймер уже больше не работает.
	defer func() {
		run.cancel()
		m.mu.Lock()
		feed.run = nil
		close(run.done)
		m.mu.Unlock()
	}()
	// timer отсчитывает период от предыдущей публикации и не накапливает пропущенные тики.
	timer := time.NewTimer(run.interval)
	defer timer.Stop()
	for {
		select {
		case <-run.ctx.Done():
			return
		case <-feed.hub.test.Done():
			return
		case <-timer.C:
			// err сообщает об отзыве допуска, остановке или невозможности следующей публикации.
			if err := feed.hub.publishTest(run.ctx); err != nil {
				return
			}
			timer.Reset(run.interval)
		}
	}
}

// StopTest отменяет текущий таймер и ждёт выхода за пределами блокировки Manager.
// Сама эпоха и накопленные сообщения остаются доступны до очистки или запуска эфира.
func (m *Manager) StopTest(ctx context.Context, streamID core.ID) error {
	if streamID.Validate() != nil {
		return core.ErrInvalid
	}
	// operation ограничивает проверку и ожидание, finish снимает регистрацию, err сообщает об отказе.
	operation, finish, err := m.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if err = m.takeTestMutation(operation); err != nil {
		return err
	}
	// Функция пропускает новый Start только после прекращения ожидания этой команды.
	defer func() { <-m.testMutation }()
	m.mu.Lock()
	// feed удерживает текущую эпоху, если генератор уже запускался.
	feed := m.testFeeds[streamID]
	if feed == nil {
		m.mu.Unlock()
		return m.checkChatStream(operation, streamID)
	}
	// run содержит отменяемый таймер, если он ещё не завершён.
	run := feed.run
	if run != nil {
		run.cancel()
	}
	m.mu.Unlock()
	if run == nil {
		return nil
	}
	select {
	case <-run.done:
		return nil
	case <-operation.Done():
		return operation.Err()
	}
}

// takeTestMutation сериализует команды, использующие один допуск до эфира, без блокировки реестра на чтении Store.
func (m *Manager) takeTestMutation(ctx context.Context) error {
	select {
	case m.testMutation <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// watchTestFeed закрывает ленту при отзыве допуска или Manager и держит место до выхода генератора.
func (m *Manager) watchTestFeed(feed *managedTestFeed) {
	select {
	case <-feed.hub.test.Done():
	case <-m.ctx.Done():
	}
	m.mu.Lock()
	m.retireTestLocked(feed)
	// run нужен для ожидания физического выхода генератора после отмены.
	run := feed.run
	m.mu.Unlock()
	if run != nil {
		<-run.done
	}
	m.mu.Lock()
	m.testHubs--
	m.mu.Unlock()
}

// currentTestLocked возвращает только допущенную эпоху, сразу убирая отозванную из реестра.
func (m *Manager) currentTestLocked(streamID core.ID) *managedTestFeed {
	// feed хранит прежнюю эпоху, которая могла потерять допуск до следующей сверки.
	feed := m.testFeeds[streamID]
	if feed != nil && feed.hub.testRevoked() {
		m.retireTestLocked(feed)
		return nil
	}
	return feed
}

// retireTestLocked убирает точный Hub из реестра, останавливает таймер и заменяет хвост подписчиков на resync.
func (m *Manager) retireTestLocked(feed *managedTestFeed) {
	if m.testFeeds[feed.hub.streamID] != feed {
		return
	}
	delete(m.testFeeds, feed.hub.streamID)
	delete(m.waitingClears, feed.hub.streamID)
	if feed.run != nil {
		feed.run.cancel()
	}
	feed.hub.stop()
	feed.hub.test.Close()
	feed.hub.finish()
	m.signalChangedLocked()
}

// Clear удаляет сообщения из выбранной ленты, ожидая реального Hub при уже принятом запуске эфира.
func (m *Manager) Clear(ctx context.Context, streamID core.ID) error {
	if streamID.Validate() != nil {
		return core.ErrInvalid
	}
	// operation ограничивает выбор владельца, finish снимает регистрацию, err сообщает об отказе.
	operation, finish, err := m.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if err = m.clearStreamOnce(operation, streamID); !errors.Is(err, core.ErrConflict) {
		return err
	}
	m.requestReconcile()
	for {
		m.mu.Lock()
		// session содержит ожидаемую реальную ленту, err сообщает об отмене проверки либо закрытии владельца.
		session, err := m.currentStreamLocked(operation, streamID)
		if session != nil && err == nil {
			err = session.hub.clearMessages(operation)
			m.mu.Unlock()
			return err
		}
		// changed фиксирует сигнал под той же блокировкой, пока реальная лента отсутствует.
		changed := m.changed
		m.mu.Unlock()
		if operation.Err() != nil {
			return errors.Join(core.ErrNotReady, operation.Err())
		}
		if err != nil {
			return err
		}
		select {
		case <-changed:
		case <-operation.Done():
			return errors.Join(core.ErrNotReady, operation.Err())
		}
	}
}

// clearStreamOnce выбирает живую ленту или подтверждает предсессионный допуск в Store.
// ErrConflict означает, что эфир принят, но его точный Hub ещё недоступен.
func (m *Manager) clearStreamOnce(ctx context.Context, streamID core.ID) error {
	// err сообщает об отмене до допуска команды очистки.
	if err := m.takeTestMutation(ctx); err != nil {
		return err
	}
	// Функция возвращает допуск до того, как Clear начнёт ждать реальный Hub.
	defer func() { <-m.testMutation }()
	m.mu.Lock()
	// session содержит подтверждённую реальную ленту, err сообщает об отказе её проверки.
	session, err := m.currentStreamLocked(ctx, streamID)
	if session != nil && err == nil {
		err = session.hub.clearMessages(ctx)
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	// proof подтверждает существование Stream без RUNNING, err сообщает об отказе до установки реального Hub.
	proof, err := m.service.AdmitChatTest(ctx, streamID)
	if err != nil {
		return err
	}
	m.mu.Lock()
	// feed может использовать тот же допуск, пока его история остаётся доступной публике.
	feed := m.currentTestLocked(streamID)
	if feed != nil && feed.hub.test == proof {
		err = feed.hub.clearMessages(ctx)
		m.mu.Unlock()
		return err
	}
	// Callback добавляет только управляющую позицию, когда Hub ещё не создан.
	err = proof.Publish(ctx, func() { m.clearWaitingLocked(streamID) })
	m.mu.Unlock()
	proof.Close()
	return err
}

// checkChatStream отличает существующий Stream без ленты от неизвестного идентификатора.
func (m *Manager) checkChatStream(ctx context.Context, streamID core.ID) error {
	// Callback читает только метаданные потока и возвращает ошибку репозитория без изменений.
	return m.store.View(ctx, func(r core.Repositories) error {
		// err сообщает об отмене, отсутствии Stream или ошибке его чтения.
		_, err := r.Config.Stream(ctx, streamID)
		return err
	})
}

// clearWaitingLocked сохраняет последнюю очистку только при наличии удерживаемого слота SSE.
func (m *Manager) clearWaitingLocked(streamID core.ID) {
	if m.subscriptions[streamID] == 0 || m.waitingClearOrder == math.MaxUint64 {
		return
	}
	// marker содержит исходный курсор ожидания, если следующие очистки ещё можно связать с ним.
	marker := m.waitingClears[streamID]
	m.waitingClearOrder++
	if marker.first == 0 {
		marker = waitingChatClear{before: m.waitingCursorLocked(streamID), first: m.waitingClearOrder}
	}
	marker.last = m.waitingClearOrder
	m.waitingClears[streamID] = marker
	m.signalChangedLocked()
}

// waitingClearScope отделяет подписанные позиции очистки от обычного курсора ожидания Stream.
func waitingClearScope(streamID core.ID) string { return "\x00clear" + string(streamID) }

// waitingClearEventLocked находит последнюю очистку после известного курсора этой предсессионной ленты.
func (m *Manager) waitingClearEventLocked(streamID core.ID, cursor core.ChatCursor) (core.ChatEvent, bool) {
	// marker задаёт последнюю управляющую запись для допущенных SSE этого Stream.
	marker := m.waitingClears[streamID]
	if cursor == "" || marker.last == 0 {
		return core.ChatEvent{}, false
	}
	// scope привязывает расчёт подписи к назначению токена и к конкретному потоку.
	scope := waitingClearScope(streamID)
	if cursor != marker.before {
		// position задаёт прежнюю точку очистки, ok сообщает о корректности её подписи.
		position, ok := m.cursorKey.position(scope, cursor, m.limits.CursorBytes)
		if !ok || position < marker.first || position >= marker.last {
			return core.ChatEvent{}, false
		}
	}
	return core.ChatEvent{Kind: core.ChatEventCleared, Cursor: m.cursorKey.cursor(scope, marker.last)}, true
}
