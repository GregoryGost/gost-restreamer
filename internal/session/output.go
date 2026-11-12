package session

import (
	"context"
	"errors"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
)

// startOutput выдаёт попытку отдельного выхода, ждёт группу, подготавливает отправителя и сохраняет
// успех до наблюдателя и Activate. Поздний отказ компенсируется отдельно; завершение работника
// сохраняет неподтверждённые ресурсы для повторной очистки.
func (m *Manager) startOutput(c core.CommandReceipt) {
	m.mu.Lock()
	// parent содержит текущую локальную Session команды запуска выхода.
	parent := m.sessions[c.SessionID]
	m.mu.Unlock()
	if parent == nil {
		return
	}
	parent.mu.Lock()
	if parent.closing || !parent.running {
		parent.mu.Unlock()
		return
	}
	// old содержит прежнего работника того же OutputID для проверки завершения перед заменой.
	if old := parent.outputs[c.OutputID]; old != nil {
		select {
		// joined сохраняет признак успешной очистки прежнего работника выхода.
		case <-old.done:
			old.mu.Lock()
			joined := old.joined
			old.mu.Unlock()
			if !joined {
				parent.mu.Unlock()
				return
			}
		default:
			parent.mu.Unlock()
			return
		}
	}
	// ctx задаёт отдельный срок нового выхода от Session; cancel отзывает его без остановки соседей.
	ctx, cancel := context.WithCancel(parent.ctx)
	// node содержит нового работника выхода с исходной квитанцией и отдельным сигналом завершения.
	node := &outputNode{cancel: cancel, done: make(chan struct{}), startReceipt: c}
	parent.outputs[c.OutputID] = node
	parent.mu.Unlock()
	defer func() {
		// Закрывает done при возврате работника и удаляет только его собственные записи выхода и членства,
		// если очистка уже завершилась.

		close(node.done)
		node.mu.Lock()
		// joined сохраняет итог очистки перед удалением только собственных записей выхода и
		// членства.
		joined := node.joined
		node.mu.Unlock()
		if joined {
			parent.mu.Lock()
			if parent.outputs[c.OutputID] == node {
				delete(parent.outputs, c.OutputID)
			}
			if parent.members[c.OutputID] == node.member {
				delete(parent.members, c.OutputID)
			}
			parent.mu.Unlock()
		}
	}()
	defer cancel()
	// startup ограничивает выдачу и готовность выхода единым сроком; stop освобождает таймер
	// запуска.
	startup, stop := context.WithTimeout(ctx, m.config.StartupTimeout)
	// f содержит точную попытку запуска выхода; err сохраняет результаты выдачи, подготовки,
	// Complete и активации.
	f, err := m.issue(startup, c)
	node.fence = f
	// member сохраняет членство точного поколения выхода в общей группе.
	var member *membership
	if err == nil {
		// output содержит сохранённый выход; e сообщает об отказе его чтения до получения
		// членства.
		output, e := m.config.Core.GetOutput(startup, c.OutputID)
		err = e
		if err == nil {
			member, err = parent.acquire(startup, output)
			node.member = member
		}
	}
	if err == nil {
		select {
		case <-member.ready:
			member.mu.Lock()
			err = member.err
			member.mu.Unlock()
		case <-startup.Done():
			err = startup.Err()
		}
	}
	// prepared сохраняет подготовленный Publisher даже при последующем отказе активации для
	// отдельной очистки.
	var prepared *publisher.Publisher
	// observer сохраняет точного наблюдателя выхода после успешного долговечного Complete.
	var observer *core.OutputObserver
	// succeeded отмечает уже сохранённый успех запуска, который поздний отказ не должен
	// переписывать.
	succeeded := false
	if err == nil {
		// owner содержит допуск точного выполнения выхода; e сообщает об отказе его привязки.
		owner, e := m.config.Core.BindExecutionOwner(startup, f)
		err = e
		if err == nil {
			member.mu.Lock()
			// ref содержит фактически полученную ссылку на группу для повторной проверки
			// готовности.
			ref := member.reference
			member.mu.Unlock()
			if ref == nil {
				err = core.ErrNotReady
			} else if err = ref.Group().WaitReady(startup); err == nil {
				// options содержит настройки независимого отправителя с корреляцией
				// Stream.
				options := m.config.Publisher
				options.Logger = m.config.Logger.With("stream_id", parent.streamID)
				prepared, err = publisher.Prepare(startup, m.config.Core, owner, member.output.Snapshot, ref.Router(), options)
			}
		}
	}
	node.mu.Lock()
	node.publisher = prepared
	node.mu.Unlock()
	if err == nil {
		err = m.complete(startup, f, true, nil)
		succeeded = err == nil
	}
	if err == nil {
		observer, err = m.config.Core.OpenOutputObserver(startup, f)
		node.mu.Lock()
		node.observer = observer
		node.mu.Unlock()
	}
	if err == nil {
		err = prepared.Activate(ctx, observer)
	}
	if err == nil {
		err = parent.registerOutputMetrics(prepared, observer)
	}
	if observer != nil {
		defer parent.sampler.Remove(observer.Token())
	}
	stop()
	if err != nil {
		node.mu.Lock()
		node.failure = err
		node.mu.Unlock()
		// cleanup ограничивает точное долговечное завершение отказа запуска; finish отменяет
		// контекст cleanup после операции.
		cleanup, finish := m.operationContext()
		if !succeeded {
			m.log(m.complete(cleanup, f, false, err), "fail_output_start")
		}
		m.log(node.resolveFailure(cleanup, m), "compensate_output")
		finish()
		m.log(err, "output_start")
	} else {
		m.commandFinished(c.ID)
		select {
		case <-ctx.Done():
		// result содержит окончательный исход уже активированного Publisher.
		case <-prepared.Done():
			result := prepared.Result()
			if result.Err != nil {
				node.mu.Lock()
				node.failure = result.Err
				node.finalFailure = true
				node.mu.Unlock()
				// cleanup ограничивает запись окончательного отказа работающего выхода;
				// finish отменяет контекст cleanup после операции.
				cleanup, finish := m.operationContext()
				m.log(m.config.Core.FailOutput(cleanup, observer, result.Err.Error()), "fail_output")
				finish()
			}
		}
	}
	cancel()
	// cleanup ограничивает физическую очистку выхода отдельным ShutdownTimeout; finish освобождает
	// таймер.
	cleanup, finish := context.WithTimeout(context.Background(), m.config.ShutdownTimeout)
	defer finish()
	m.log(node.cleanup(cleanup, m), "output_cleanup")
	m.signal()
}

// cleanup отменяет выход, при необходимости сохраняет отказ или компенсацию и ждёт отправителя и
// членство. После завершения физических ресурсов подтверждает разрешение на освобождение резерва,
// освобождает его и закрывает наблюдателя; ошибки оставляют возможность повтора.
func (n *outputNode) cleanup(ctx context.Context, m *Manager) error {
	n.cleanupMu.Lock()
	defer n.cleanupMu.Unlock()
	n.cancel()
	n.mu.Lock()
	// prepared содержит сохраняемый отправитель; observer содержит его точного наблюдателя для
	// закрытия после освобождения ресурсов.
	prepared, observer := n.publisher, n.observer
	n.mu.Unlock()
	// result объединяет ошибки остановки отправителя, закрытия членства и наблюдателя.
	var result error
	// err сообщает об отказе сохранения точного отказа либо компенсации до физической очистки.
	if err := n.resolveFailure(ctx, m); err != nil {
		return err
	}
	if prepared != nil {
		result = errors.Join(result, prepared.Stop(ctx))
		if !prepared.Result().NetworkJoined {
			result = errors.Join(result, publisher.ErrCleanupPending)
		}
	}
	if n.member != nil {
		result = errors.Join(result, n.member.close(ctx))
	}
	if result != nil {
		return result
	}
	// Завершение физических ресурсов и освобождение RAM предшествуют подтверждению очистки в БД.
	n.mu.Lock()
	// released сохраняет признак уже выполненного освобождения резервации для безопасного повтора
	// cleanup.
	released := n.released
	n.mu.Unlock()
	if prepared != nil && !released {
		// err сообщает об отказе долговечного разрешения освобождения точной резервации.
		if err := m.config.Core.ConfirmOutputExecutionResolution(ctx, prepared.Reservation()); err != nil {
			return err
		}
		// err сообщает об отказе освобождения уже присоединённого исполнения Publisher.
		if err := prepared.ReleaseJoined(); err != nil {
			return err
		}
		n.mu.Lock()
		n.released = true
		n.mu.Unlock()
	}
	if observer != nil {
		result = errors.Join(result, observer.Close(ctx))
	}
	n.mu.Lock()
	n.joined = result == nil
	n.mu.Unlock()
	return result
}

// resolveFailure сверяет точную квитанцию перед записью отказа запуска либо компенсирует уже успешный
// запуск. Окончательный отказ отправителя записывается через действующего наблюдателя и уступает
// остановке или новому поколению выхода.
func (n *outputNode) resolveFailure(ctx context.Context, m *Manager) error {
	n.mu.Lock()
	// failure сохраняет причину отказа; final отличает окончательный отказ работающего отправителя;
	// observer содержит его точного наблюдателя.
	failure, final, observer := n.failure, n.finalFailure, n.observer
	n.mu.Unlock()
	if failure == nil {
		return nil
	}
	if n.startReceipt.ID != "" && !final {
		// current содержит актуальную квитанцию запуска; err сообщает об отказе проверки
		// неизменяемой команды.
		current, err := m.config.Core.GetReceipt(ctx, n.startReceipt.ID)
		if err != nil {
			return err
		}
		if !sameCommand(current, n.startReceipt) || current.Status.Terminal() && current.Status != core.CommandSucceeded {
			return nil
		}
		if current.Status == core.CommandPending {
			// Внешнее исполнение ещё не начиналось; Claim нужен только для точного отказа.
			// f содержит новую точную попытку только для записи отказа ещё не выданной
			// команды; err сообщает об отказе Claim.
			f, err := m.config.Core.Claim(ctx, current.RequestID)
			if err != nil {
				return err
			}
			n.fence = f
		} else {
			// f восстанавливает точную попытку текущей квитанции перед проверкой прежнего
			// исполнения.
			f := receiptFence(current)
			if n.fence.CommandID != "" && n.fence != f {
				return core.ErrStale
			}
			n.fence = f
		}
		if current.Status != core.CommandSucceeded {
			// err содержит результат записи неуспешного завершения ещё не успешной команды
			// запуска.
			if err := m.complete(ctx, n.fence, false, failure); err == nil {
				return nil
			}
		}
	}
	if n.fence.OutputID == "" {
		return core.ErrNotReady
	}
	if final && observer != nil && observer.Valid() {
		// err содержит результат записи окончательного отказа через действующего наблюдателя
		// выхода.
		err := m.config.Core.FailOutput(ctx, observer, failure.Error())
		if err == nil {
			return nil
		}
		// output содержит актуальное желание и поколение выхода; readErr сообщает об отказе
		// проверки конкурирующей остановки или нового исполнения.
		output, readErr := m.config.Core.GetOutput(ctx, n.fence.OutputID)
		if readErr == nil && (!output.DesiredRunning || output.Generation != n.fence.OutputGeneration || output.Attempt != n.fence.Attempt) {
			return nil
		}
		return errors.Join(err, readErr)
	}
	// err содержит результат отдельной компенсации уже успешного запуска точного выхода.
	_, _, err := m.config.Core.CompensateOutputStart(ctx, n.fence, "manager:resolve:"+string(n.fence.CommandID))
	return err
}

// waitOutputCleanup не принимает закрытый Done за доказательство успешной очистки.
func (n *sessionNode) waitOutputCleanup(ctx context.Context, id core.ID, generation uint64) error {
	n.mu.Lock()
	// output содержит отправителя выбранного поколения; member содержит соответствующее членство в
	// группе.
	output, member := n.outputs[id], n.members[id]
	if output != nil && output.startReceipt.OutputGeneration != generation || member != nil && member.output.Generation != generation {
		n.mu.Unlock()
		return core.ErrStale
	}
	if n.excluded == nil {
		n.excluded = map[core.ID]uint64{}
	}
	if !n.running && !n.closing {
		n.excluded[id] = max(n.excluded[id], generation)
	}
	n.mu.Unlock()
	if output != nil {
		output.cancel()
		select {
		case <-output.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		output.mu.Lock()
		// joined сохраняет признак успешной прежней очистки после возврата работника выхода.
		joined := output.joined
		output.mu.Unlock()
		if !joined {
			// err сообщает об отказе повторной очистки отправителя выбранного поколения.
			if err := output.cleanup(ctx, n.manager); err != nil {
				return err
			}
		}
	}
	if member != nil {
		// err сообщает об отказе освобождения членства выбранного поколения.
		if err := member.close(ctx); err != nil {
			return err
		}
	}
	n.mu.Lock()
	if n.outputs[id] == output {
		delete(n.outputs, id)
	}
	if n.members[id] == member {
		delete(n.members, id)
	}
	n.mu.Unlock()
	return nil
}

// retryPause ждёт интервал до следующей попытки и возвращает false при отмене контекста.
func retryPause(ctx context.Context, d time.Duration) bool {
	// timer отсчитывает паузу повторной операции с отменой контекстом.
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
