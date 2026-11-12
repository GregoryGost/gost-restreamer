package core

import (
	"context"
	"sync/atomic"
)

// RuntimeDispatcher резервирует единственного диспетчера общей RuntimeAuthority.
// Закрытие завершает логический допуск; физическую очистку выполняет владелец ресурсов.
type RuntimeDispatcher struct {
	// authority связывает диспетчер с общей блокировкой и единственным слотом владельца.
	authority *RuntimeAuthority
	// self позволяет отклонять копии диспетчера вместо исходного экземпляра.
	self *RuntimeDispatcher
	// valid показывает, сохраняется ли логический допуск диспетчера.
	valid atomic.Bool
	// done закрывается при отзыве логического допуска диспетчера.
	done chan struct{}
}

// Valid сообщает, сохраняется ли допуск у исходного экземпляра диспетчера; копия структуры
// недействительна.
func (d *RuntimeDispatcher) Valid() bool { return d != nil && d.self == d && d.valid.Load() }

// Done возвращает канал логического отзыва диспетчера; у nil-получателя канал отсутствует.
func (d *RuntimeDispatcher) Done() <-chan struct{} {
	if d == nil {
		return nil
	}
	return d.done
}

// closeLocked отзывает допуск и снимает только собственную резервацию; вызывающий удерживает общую
// блокировку RuntimeAuthority.
func (d *RuntimeDispatcher) closeLocked() {
	if d.valid.Swap(false) {
		close(d.done)
	}
	if d.authority.dispatcher == d {
		d.authority.dispatcher = nil
	}
}

// Close идемпотентно освобождает только собственную резервацию, в том числе после закрытия RuntimeAuthority.
func (d *RuntimeDispatcher) Close() {
	_ = d.CloseContext(context.Background())
}

// CloseContext ограничивает ожидание RuntimeAuthority; отмена сохраняет действующую резервацию.
func (d *RuntimeDispatcher) CloseContext(ctx context.Context) error {
	if d == nil || d.authority == nil {
		return nil
	}
	if d.self != d {
		return ErrStale
	}
	// err содержит отмену контекста до ожидания общей блокировки.
	if err := ctx.Err(); err != nil {
		return err
	}
	// a содержит владельца общей блокировки, под которой освобождается резерв.
	a := d.authority
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.gate:
	}
	defer a.release()
	// err содержит отмену, наступившую до получения общей блокировки.
	if err := ctx.Err(); err != nil {
		return err
	}
	d.closeLocked()
	return nil
}

// ReserveDispatcher резервирует единственного диспетчера общей RuntimeAuthority; занятый слот даёт
// ErrInUse. Владелец освобождает полученный резерв через Close или CloseContext. Операция учитывает
// отмену контекста, не создаёт процессы и не меняет долговечное состояние.
func (s *Service) ReserveDispatcher(ctx context.Context) (*RuntimeDispatcher, error) {
	// a содержит общую RuntimeAuthority, а err — причину невозможности её получить.
	a, err := s.runtimeAuthority()
	if err != nil {
		return nil, err
	}
	if err = a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	if a.dispatcher != nil {
		return nil, ErrInUse
	}
	// d содержит новый резерв единственного диспетчера до его регистрации в authority.
	d := &RuntimeDispatcher{authority: a, done: make(chan struct{})}
	d.self = d
	d.valid.Store(true)
	a.dispatcher = d
	return d, nil
}
