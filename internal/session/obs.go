package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// lockAdmission получает mu без удержания других блокировок. ctx, остановка Manager
// и отзыв dispatcher прерывают ожидание; после TryLock допуск проверяется повторно.
func (m *Manager) lockAdmission(ctx context.Context, mu *sync.Mutex) error {
	for {
		// err сообщает об отмене вызывающего до очередной попытки захвата.
		if err := ctx.Err(); err != nil {
			return err
		}
		if m.admission() != nil || m.ctx.Err() != nil {
			return core.ErrClosed
		}
		if mu.TryLock() {
			// err сохраняет отмену, наступившую одновременно с захватом.
			if err := ctx.Err(); err != nil {
				mu.Unlock()
				return err
			}
			if m.admission() != nil || m.ctx.Err() != nil {
				mu.Unlock()
				return core.ErrClosed
			}
			return nil
		}
		// err сообщает об отмене ожидания следующей попытки захвата.
		if err := m.waitAdmission(ctx, nil); err != nil {
			return err
		}
	}
}

// waitAdmission ждёт следующую сверку либо startup, если канал задан. ctx,
// остановка Manager и dispatcher завершают ожидание без создания работников Lock.
func (m *Manager) waitAdmission(ctx context.Context, startup <-chan struct{}) error {
	// timer ограничивает ожидание одной сверкой текущего запуска.
	timer := time.NewTimer(m.config.ReconcileInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.ctx.Done():
		return core.ErrClosed
	case <-m.dispatcher.Done():
		return core.ErrClosed
	case <-startup:
		return nil
	case <-timer.C:
		return nil
	}
}

// AdmitOBS проверяет key текущего Stream streamID и выбирает ровно одну Session.
// При отсутствии запуска принимает штатный StartSession со всеми Enabled; при точном
// ErrInUse один раз перечитывает разрешённый запуск. Возвращает только готовый Router
// того же SessionID/incarnation/lifecycle. ctx ограничивает допуск, но не жизнь принятой Session.
func (m *Manager) AdmitOBS(ctx context.Context, streamID core.ID, key string) (core.Session, *media.Router, error) {
	// finish отменяет дочерний ctx и присоединяет наблюдателя владельцев допуска.
	ctx, finish := m.obsContext(ctx)
	defer finish()
	// err сообщает об отказе учёта обращения до начала авторизованного выбора.
	if err := m.enter(ctx); err != nil {
		return core.Session{}, nil, err
	}
	defer m.admissions.Done()
	// selected содержит выбранный запуск; found отличает отсутствие от ошибки чтения;
	// err сохраняет исход авторизации, принятия и ожидания готовности.
	selected, found, err := m.config.Core.InspectOBS(ctx, streamID, key)
	if err != nil {
		return core.Session{}, nil, err
	}
	if !found {
		// random содержит независимые 128 бит идентификатора одной попытки публикации.
		var random [16]byte
		if _, err = rand.Read(random[:]); err != nil {
			return core.Session{}, nil, err
		}
		// receipt фиксирует выбранную Session даже при дальнейшем истечении ctx;
		// startErr сохраняет точный отказ принятия, не смешивая его с ошибкой чтения.
		receipt, startErr := m.StartSession(ctx, core.StartSessionRequest{RequestID: "obs:" + hex.EncodeToString(random[:]), StreamID: streamID})
		if startErr == core.ErrInUse {
			selected, found, err = m.config.Core.InspectOBS(ctx, streamID, key)
			if err != nil {
				return core.Session{}, nil, err
			}
			if !found || selected.State != core.SessionStarting && selected.State != core.SessionRunning {
				return core.Session{}, nil, startErr
			}
		} else if startErr != nil {
			return core.Session{}, nil, startErr
		} else {
			selected, err = m.config.Core.GetSession(ctx, receipt.SessionID)
			if err != nil {
				return core.Session{}, nil, err
			}
			if selected.Incarnation != receipt.Incarnation || selected.Lifecycle != receipt.Lifecycle {
				return core.Session{}, nil, core.ErrStale
			}
		}
	}
	return m.awaitOBS(ctx, selected)
}

// awaitOBS ждёт локальную готовность selected, сверяя её точное исполнение при каждом
// пробуждении. Исчезновение уже найденного узла либо STOPPING не выбирает другой запуск.
func (m *Manager) awaitOBS(ctx context.Context, selected core.Session) (core.Session, *media.Router, error) {
	// node запоминает единственный локальный узел; после его обнаружения замена запрещена.
	var node *sessionNode
	for {
		if node != nil {
			select {
			case <-node.startupDone:
				if node.startupErr != nil {
					return core.Session{}, nil, node.startupErr
				}
			default:
			}
		}
		// current содержит актуальное состояние именно выбранной Session; err сообщает об отказе чтения.
		current, err := m.config.Core.GetSession(ctx, selected.ID)
		if err != nil {
			return core.Session{}, nil, err
		}
		if current.ID != selected.ID || current.StreamID != selected.StreamID || current.Incarnation != selected.Incarnation || current.Lifecycle != selected.Lifecycle || current.State != core.SessionStarting && current.State != core.SessionRunning {
			return core.Session{}, nil, core.ErrStale
		}
		if err = m.lockAdmission(ctx, &m.mu); err != nil {
			return core.Session{}, nil, err
		}
		// registered содержит текущий узел карты; отсутствие допустимо только до выдачи STARTING.
		registered := m.sessions[selected.ID]
		m.mu.Unlock()
		if node != nil && registered != node {
			return core.Session{}, nil, core.ErrStale
		}
		if registered == nil {
			if current.State != core.SessionStarting || current.Issued {
				return core.Session{}, nil, core.ErrNotReady
			}
			if err = m.waitAdmission(ctx, nil); err != nil {
				return core.Session{}, nil, err
			}
			continue
		}
		node = registered
		select {
		case <-node.startupDone:
			if node.startupErr != nil {
				return core.Session{}, nil, node.startupErr
			}
		default:
			if err = m.waitAdmission(ctx, node.startupDone); err != nil {
				return core.Session{}, nil, err
			}
			continue
		}
		if current.State == core.SessionStarting {
			continue
		}
		if err = m.lockAdmission(ctx, &node.mu); err != nil {
			return core.Session{}, nil, err
		}
		if node.closing || !node.running || node.ctx.Err() != nil || node.owner == nil || !node.owner.Valid() || node.router == nil || current.State != core.SessionRunning || node.streamID != selected.StreamID || node.fence.SessionID != selected.ID || node.fence.Incarnation != selected.Incarnation || node.fence.Lifecycle != selected.Lifecycle {
			node.mu.Unlock()
			return core.Session{}, nil, core.ErrStale
		}
		// router сохраняет готовый маршрутизатор до снятия блокировки текущего узла.
		router := node.router
		node.mu.Unlock()
		return current, router, nil
	}
}

// finishStartup публикует неизменяемый исход запуска err. Его вызывает только работник
// startSession один раз, после освобождения mu при успехе и до компенсации при отказе.
func (n *sessionNode) finishStartup(err error) {
	n.startupErr = err
	close(n.startupDone)
}

// obsContext связывает I/O и ожидания допуска с caller, Manager и dispatcher.
// Возвращённое завершение отменяет дочерний контекст и ждёт единственного наблюдателя;
// этот контекст не передаётся в срок жизни Session или независимую сверку квитанции.
func (m *Manager) obsContext(caller context.Context) (context.Context, func()) {
	// ctx ограничивает только допуск; cancel прекращает его при отмене любого владельца.
	ctx, cancel := context.WithCancel(caller)
	// done подтверждает выход наблюдателя, чтобы допуск не оставлял собственную горутину.
	done := make(chan struct{})
	go func() {
		// Передаёт остановку Manager или dispatcher в I/O допуска и завершает наблюдение
		// при отмене caller или возврате AdmitOBS.
		defer close(done)
		select {
		case <-ctx.Done():
		case <-m.ctx.Done():
			cancel()
		case <-m.dispatcher.Done():
			cancel()
		}
	}()
	return ctx, func() {
		// Прекращает наблюдение и присоединяет его до окончательного возврата допуска.
		cancel()
		<-done
	}
}
