package publisher

import (
	"context"
	"errors"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
)

// Следит за отзывом исполнения, выполняет последовательные попытки передачи и ждёт завершения
// наблюдателя отзыва перед публикацией итога.
func (p *Publisher) run(ctx context.Context) {
	// watchDone подтверждает завершение наблюдателя отзыва исполнения до публикации окончательного
	// результата.
	watchDone := make(chan struct{})
	go func() {
		// При отзыве исполнения отмечает revoked, если пользовательская остановка ещё не началась, отменяет
		// сеть и подтверждает свой выход через watchDone.

		defer close(watchDone)
		select {
		case <-ctx.Done():
		case <-p.execution.done():
			p.mu.Lock()
			if !p.stopping && ctx.Err() == nil {
				p.revoked = true
			}
			p.mu.Unlock()
			p.cancel()
		}
	}()
	// err содержит итог цикла последовательных сетевых попыток.
	err := p.loop(ctx)
	p.requestSeal()
	p.cancel()
	<-watchDone
	p.finish(err)
}

// Публикует состояние через обычный либо терминальный порт наблюдения и обновляет локальное состояние
// только после успеха.
func (p *Publisher) observe(ctx context.Context, state core.OutputState, reason FailureReason) error {
	// err содержит причину отмены контекста перед публикацией состояния выхода.
	if err := ctx.Err(); err != nil {
		return err
	}
	// value содержит оперативное состояние и ограниченную причину, передаваемые наблюдателю.
	value := core.OutputObservation{State: state, Error: string(reason)}
	// err сохраняет результат обычной либо терминальной публикации состояния.
	var err error
	// observer содержит расширенный порт окончательного наблюдения; ok показывает его наличие у
	// текущего наблюдателя.
	if observer, ok := p.observer.(terminalObservation); ok && (state == core.OutputFailed || state == core.OutputStopped) {
		err = observer.ObserveTerminal(ctx, value)
	} else {
		err = p.observer.Observe(ctx, value)
	}
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.state, p.failure = state, reason
	p.mu.Unlock()
	if p.options.Logger != nil {
		p.options.Logger.Info("publisher_state", "operation", "observe", "state", state, "reason", reason)
	}
	return nil
}

// Повторяет сетевые ошибки с ограниченным удвоением задержки без предела попыток; окончательные ошибки
// завершают выход, а передача обеих дорожек сбрасывает задержку.
func (p *Publisher) loop(ctx context.Context) error {
	// delay содержит текущую паузу сетевого повтора, ограниченную RetryMax и сбрасываемую после
	// передачи обеих дорожек.
	delay := p.options.RetryInitial
	for {
		if ctx.Err() != nil {
			return nil
		}
		// err сообщает об отказе публикации состояния CONNECTING перед сетевой попыткой.
		if err := p.observe(ctx, core.OutputConnecting, FailureNone); err != nil {
			return stoppedError(ctx, err)
		}
		p.mu.Lock()
		saturate(&p.metrics.Attempts, 1, &p.metrics.Saturated)
		p.mu.Unlock()
		// ready показывает фактическую передачу обеих дорожек за попытку; err сохраняет исход
		// попытки и публикации состояния повтора.
		ready, err := p.attempt(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			return nil
		}
		if p.options.Logger != nil {
			p.options.Logger.Error("publisher_attempt", "operation", "publish", "ready", ready, "error", err)
		}
		if terminal(err) {
			_ = p.observe(ctx, core.OutputFailed, failureFor(err))
			return err
		}
		if err = p.observe(ctx, core.OutputRetrying, FailureNetwork); err != nil {
			return stoppedError(ctx, err)
		}
		if ready {
			delay = p.options.RetryInitial
		}
		if !pause(ctx, delay) {
			return nil
		}
		delay = nextDelay(delay, p.options.RetryMax)
	}
}

// Подавляет ошибку как причину отказа при уже отменённом контексте остановки.
func stoppedError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// Определяет ошибки, при которых повтор сети не восстановит допустимость выхода, источника или
// текущего исполнения.
func terminal(err error) bool {
	return rtmp.IsOutboundTerminal(err) || errors.Is(err, core.ErrStale) || errors.Is(err, core.ErrClosed) || errors.Is(err, core.ErrInvalid) || errors.Is(err, core.ErrConflict) || errors.Is(err, core.ErrInUse) || errors.Is(err, core.ErrNotReady) || errors.Is(err, core.ErrObservationLimit) || errors.Is(err, media.ErrRouterClosed) || errors.Is(err, media.ErrRouterBudget) || errors.Is(err, media.ErrSubscriptionGeneration)
}

// Удваивает задержку до заданного предела, проверяя переполнение до сложения.
func nextDelay(current, capDelay time.Duration) time.Duration {
	if current >= capDelay || current > capDelay-current {
		return capDelay
	}
	return current * 2
}

// Ожидает задержку с отменой контекстом и повторно проверяет отмену при срабатывании таймера.
func pause(ctx context.Context, delay time.Duration) bool {
	// timer ограничивает паузу перед повтором без блокирования отмены контекста.
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

// Владеет одним соединением до Close и Wait, включая поздний результат подключения; истечение срока
// синхронизации обновляет подписку в том же соединении. ready означает фактическую передачу обеих
// дорожек хотя бы в одном поколении.
func (p *Publisher) attempt(ctx context.Context) (ready bool, result error) {
	// attemptCtx задаёт отдельную отмену текущей сетевой попытки; cancel прекращает её операции при
	// возврате.
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// options содержит настройки транспорта с обработчиками допуска, записи и аномалий ACK данного
	// Publisher.
	options := p.options.Transport
	options.Admit, options.OnWrite = p.execution.admit, p.noteWrite
	options.OnACKAnomaly = p.noteACKAnomaly
	p.mu.Lock()
	p.metrics.WireBufferBytes = options.Limits.MaxRetainedBytes
	p.mu.Unlock()
	defer func() {
		// Сбрасывает логический учёт буфера транспорта при любом завершении попытки.

		p.mu.Lock()
		p.metrics.WireBufferBytes = 0
		p.mu.Unlock()
	}()
	// conn содержит результат подключения; err сохраняет ошибки открытия соединения и последующих
	// операций подписки.
	conn, err := p.dial(attemptCtx, p.target, options)
	if err != nil {
		return false, err
	}
	if conn == nil {
		return false, core.ErrInvalid
	}
	// Даже поздно вернувшееся соединение проходит Close и Wait до NetworkJoined.
	defer func() {
		// При окончательной ошибке пытается опубликовать отказ, если ctx ещё действует, и запечатывает
		// исполнение; затем отменяет попытку, закрывает подписку и соединение и ждёт транспорт. Ошибку Wait
		// сохраняет только при отсутствии прежней ошибки и отмены.

		if terminal(result) {
			if ctx.Err() == nil {
				if p.options.Logger != nil {
					p.options.Logger.Error("publisher_failure", "operation", "publish", "error", result)
				}
				_ = p.observe(ctx, core.OutputFailed, failureFor(result))
			}
			p.requestSeal()
		}
		cancel()
		p.closeSubscription()
		_ = conn.Close()
		// joinErr содержит результат фактического завершения транспорта после Close.
		joinErr := conn.Wait(context.Background())
		p.mu.Lock()
		p.conn = nil
		p.mu.Unlock()
		if result == nil && ctx.Err() == nil && joinErr != nil {
			result = joinErr
		}
	}()
	p.mu.Lock()
	if p.hadConnection {
		saturate(&p.metrics.Reconnects, 1, &p.metrics.Saturated)
	}
	p.hadConnection = true
	saturate(&p.metrics.Connections, 1, &p.metrics.Saturated)
	p.conn = conn
	p.mu.Unlock()
	if attemptCtx.Err() != nil {
		return false, attemptCtx.Err()
	}
	// readCtx ограничивает ожидание подписки сроком попытки и состоянием соединения; stopRead
	// отменяет это ожидание.
	readCtx, stopRead := context.WithCancel(attemptCtx)
	// watchDone подтверждает выход наблюдателя соединения перед возвратом сетевой попытки.
	watchDone := make(chan struct{})
	go func() {
		// Отменяет ожидание подписки после завершения соединения либо выходит при отмене readCtx; закрывает
		// watchDone после завершения наблюдателя.

		defer close(watchDone)
		select {
		case <-conn.Done():
			stopRead()
		case <-readCtx.Done():
		}
	}()
	defer func() {
		// Отменяет контекст чтения подписки и ждёт выхода наблюдателя соединения перед возвратом попытки.
		stopRead()
		<-watchDone
	}()
	for {
		if readCtx.Err() != nil {
			return ready, connectionError(ctx, conn)
		}
		// sub содержит новую ограниченную подписку группы; err сообщает об отказе её создания
		// или передачи.
		sub, err := p.router.Subscribe(p.options.Queue)
		if err != nil {
			return ready, err
		}
		p.attachSubscription(sub)
		// generationReady показывает передачу обеих дорожек хотя бы одного поколения данной
		// подписки.
		generationReady, err := p.forward(readCtx, conn, sub)
		ready = ready || generationReady
		p.closeSubscription()
		if readCtx.Err() != nil {
			return ready, connectionError(ctx, conn)
		}
		if !errors.Is(err, media.ErrResyncTimeout) {
			return ready, err
		}
		// Истечение срока синхронизации обновляет очередь, сохраняя соединение и начало отсчёта.
		if !pause(readCtx, p.options.ResubscribeDelay) {
			return ready, connectionError(ctx, conn)
		}
	}
}

// Возвращает причину отмены вызывающего либо ошибку соединения; при отсутствии обеих причин использует
// context.Canceled.
func connectionError(ctx context.Context, conn connection) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// err содержит причину завершения транспорта, если вызывающий контекст ещё действует.
	if err := conn.Err(); err != nil {
		return err
	}
	return context.Canceled
}

// Передаёт актуальные поколения с повтором заголовков и отбрасывает устаревшие выдачи подписки;
// состояние PUBLISHING публикует только после успешной передачи видео и аудио одного поколения.
func (p *Publisher) forward(ctx context.Context, conn connection, sub *media.Subscription) (ready bool, err error) {
	// generation хранит номер поколения, для которого заголовки уже переданы транспорту.
	var generation uint64
	// video отмечает отправку видео текущего поколения; audio отмечает отправку аудио; publishing
	// исключает повторную публикацию PUBLISHING для этого поколения.
	var video, audio, publishing bool
	for {
		// delivery содержит очередную выдачу подписки; err сохраняет ошибки её получения,
		// допуска и передачи.
		delivery, err := sub.Next(ctx)
		if err != nil {
			return ready, err
		}
		p.hold(delivery)
		if !delivery.IsCurrent() {
			p.abandoned(dropStale)
			continue
		}
		if err = ctx.Err(); err != nil {
			p.abandoned(dropCancelled)
			return ready, err
		}
		if delivery.Generation != generation {
			video, audio, publishing = false, false, false
			if err = p.observe(ctx, core.OutputConnecting, FailureNone); err != nil {
				p.abandoned(p.dropFor(ctx, err))
				return ready, err
			}
			// Транспорт проверяет допуск до сериализации и внешних операций.
			if !delivery.IsCurrent() {
				p.abandoned(dropStale)
				continue
			}
			if err = conn.WriteHeaders(ctx, delivery.VideoConfig, delivery.AudioConfig); err != nil {
				p.abandoned(p.dropFor(ctx, err))
				return ready, err
			}
			generation = delivery.Generation
		}
		if !delivery.IsCurrent() {
			p.abandoned(dropStale)
			generation = 0
			continue
		}
		// written содержит число полезных байтов полностью отправленного медиасообщения.
		written, err := conn.WriteDelivery(ctx, delivery)
		if err != nil {
			if errors.Is(err, media.ErrStaleSource) {
				p.abandoned(dropStale)
				generation = 0
				continue
			}
			p.abandoned(p.dropFor(ctx, err))
			return ready, err
		}
		p.delivered(delivery, written.PayloadBytes)
		if delivery.Packet.Config().Info().Track == media.TrackVideo {
			video = true
		} else {
			audio = true
		}
		if !delivery.IsCurrent() {
			generation, video, audio, publishing = 0, false, false, false
			continue
		}
		if video && audio && !publishing {
			if err = p.observe(ctx, core.OutputPublishing, FailureNone); err != nil {
				return ready, err
			}
			ready = true
			publishing = true
		}
	}
}

// Выбирает причину потери уже полученной единицы доступа: отмена, окончательный отказ либо
// транспортная ошибка.
func (p *Publisher) dropFor(ctx context.Context, err error) dropReason {
	if ctx.Err() != nil {
		return dropCancelled
	}
	if terminal(err) {
		return dropTerminal
	}
	return dropTransport
}
