package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"net"
	"net/http"
)

// ErrPending сообщает, что приложение сохраняет незавершённых владельцев и общие зависимости для
// повторного ожидания.
var ErrPending = errors.New("завершение приложения ещё не подтверждено")

// stopResult отделяет ошибку операции от подтверждения освобождения её владельца.
type stopResult struct {
	// err сохраняет отказ операции, включая ошибку уже завершённой работы.
	err error
	// joined подтверждает завершение владельца независимо от наличия исторической ошибки.
	joined bool
}

// operation сохраняет один выполняемый вызов и публикует его результат через закрытие канала.
type operation struct {
	// done закрывается после записи result и позволяет ограниченно ждать того же вызова.
	done chan struct{}
	// result содержит итог вызова, доступный ожидающему после закрытия done.
	result stopResult
}

// startOperation запускает одну операцию в отдельной горутине и публикует её результат перед закрытием
// done. Ожидание не требует, чтобы вызываемая функция соблюдала свой контекст.
func startOperation(call func() stopResult) *operation {
	// op содержит наблюдаемую операцию, передаваемую вызывающему до завершения её горутины.
	op := &operation{done: make(chan struct{})}
	go func() {
		// Сохраняет результат операции перед закрытием канала, по которому ожидающие узнают о её завершении.
		op.result = call()
		close(op.done)
	}()
	return op
}

// isClosed проверяет закрытие канала без ожидания; nil и ещё открытый канал дают false.
func isClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// wait ограничивает ожидание операции контекстом, не отменяя сам вызов. На границе отмены он повторно
// проверяет done, чтобы учесть уже опубликованный результат.
func (op *operation) wait(ctx context.Context) bool {
	if isClosed(op.done) {
		return true
	}
	select {
	case <-op.done:
		return true
	case <-ctx.Done():
		return isClosed(op.done)
	}
}

// stopOwner сохраняет текущее закрытие ресурса и подтверждение завершения между попытками Shutdown.
type stopOwner struct {
	// name идентифицирует компонент в сохранённой диагностике его остановки.
	name string
	// close выполняет остановку ресурса и отдельно сообщает ошибку и подтверждение освобождения.
	close func(context.Context) stopResult
	// current сохраняет уже запущенный вызов закрытия, который нельзя запускать конкурентно
	// повторно.
	current *operation
	// joined запоминает подтверждённое освобождение и запрещает повторно закрывать ресурс.
	joined bool
	// recorded отмечает, что ошибка текущей завершённой операции уже добавлена в журнал
	// Application.errors.
	recorded bool
}

// newStopOwner считает владельца завершённым только при успешном возврате функции закрытия. Ошибка
// оставляет возможность повторного ожидания того же владельца.
func newStopOwner(name string, close func(context.Context) error) *stopOwner {
	return &stopOwner{name: name, close: func(ctx context.Context) stopResult {
		// Считает остановку подтверждённой только при успешном возврате переданной функции остановки.
		// err содержит результат функции закрытия; успешный возврат служит подтверждением joined
		// для newStopOwner.
		err := close(ctx)
		return stopResult{err, err == nil}
	}}
}

// sharedOwner считает синхронное освобождение общего ресурса завершённым после возврата close,
// сохраняя возможную ошибку отдельно от факта завершения.
func sharedOwner(name string, close func(context.Context) error) *stopOwner {
	return &stopOwner{name: name, close: func(ctx context.Context) stopResult {
		// Сохраняет ошибку освобождения общего ресурса, считая сам синхронный вызов завершённым.
		return stopResult{close(ctx), true}
	}}
}

// listenerOwner создаёт владельца закрытия слушателя и принимает net.ErrClosed как успешное повторное
// закрытие.
func listenerOwner(name string, listener net.Listener) *stopOwner {
	return sharedOwner(name, func(context.Context) error {
		// Закрывает слушатель и считает повторное закрытие штатным результатом.

		// err содержит отказ закрытия слушателя, из которого net.ErrClosed считается штатным
		// повтором.
		err := listener.Close()
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	})
}

// begin запускает закрытие незавершённого владельца, только если предыдущее закрытие ещё не
// выполняется. Повторный вызов сохраняет уже запущенную операцию.
func (o *stopOwner) begin(ctx context.Context) {
	if o != nil && !o.joined && o.current == nil {
		o.current = startOperation(func() stopResult {
			// Вызывает остановку владельца с контекстом текущей попытки и сохраняет её отдельную операцию.
			return o.close(ctx)
		})
		o.recorded = false
	}
}

// waitOwner ждёт текущую операцию владельца и сохраняет каждую её ошибку один раз. После возврата без
// подтверждения освобождения допускается ещё один вызов закрытия в пределах того же контекста;
// незавершённая операция остаётся для следующего Shutdown.
func (a *Application) waitOwner(ctx context.Context, o *stopOwner) bool {
	if o == nil || o.joined {
		return true
	}
	for range 2 {
		o.begin(ctx)
		if !o.current.wait(ctx) {
			return false
		}
		// result содержит опубликованный итог текущего закрытия для однократного учёта ошибки и
		// проверки joined.
		result := o.current.result
		if !o.recorded && result.err != nil {
			a.errors = append(a.errors, fmt.Errorf("%s: %w", o.name, result.err))
		}
		o.recorded = true
		if result.joined {
			o.joined = true
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		// Повтор ждёт того же владельца и сохраняет ошибку первой завершённой работы.
		o.current = nil
	}
	return false
}

// waitOwners сначала запускает остановку всех переданных владельцев и лишь затем ждёт результаты.
// Значение false означает, что хотя бы один владелец ещё не подтвердил завершение.
func (a *Application) waitOwners(ctx context.Context, owners ...*stopOwner) bool {
	// owner содержит очередного независимого владельца, которому сначала подаётся сигнал остановки.
	for _, owner := range owners {
		owner.begin(ctx)
	}
	// joined накапливает подтверждение завершения всех переданных владельцев.
	joined := true
	// owner содержит очередного владельца, чья уже запущенная остановка ожидается в общем контексте.
	for _, owner := range owners {
		if !a.waitOwner(ctx, owner) {
			joined = false
		}
	}
	return joined
}

// Shutdown ограничивает ожидание даже для синхронного Close, который не уважает ctx.
// Неоконченные операции остаются у Application и наблюдаются повторным вызовом.
func (a *Application) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return ErrPending
	}
	if a.config.Lifecycle.ShutdownTimeout > 0 {
		// bounded ограничивает вызов Shutdown настроенным сроком; cancel освобождает его таймер
		// после возврата.
		bounded, cancel := context.WithTimeout(ctx, a.config.Lifecycle.ShutdownTimeout)
		defer cancel()
		ctx = bounded
	}
	select {
	case <-a.gate:
		defer func() {
			// Возвращает разрешение следующему вызову Shutdown после выхода из защищённого участка.
			a.gate <- struct{}{}
		}()
	case <-ctx.Done():
		return errors.Join(ErrPending, ctx.Err())
	}
	if isClosed(a.done) {
		return errors.Join(a.errors...)
	}
	// Все независимые владельцы получают сигнал остановки до ожидания первого результата.
	// joined учитывает завершение независимых компонентов и затем сетевых Serve до закрытия общих
	// зависимостей.
	joined := a.waitOwners(ctx, a.owners.api, a.owners.transport, a.owners.manager, a.owners.ingest, a.owners.preparation, a.owners.gpu, a.owners.httpListener, a.owners.ingestListener)
	if a.serveCancel != nil {
		a.serveCancel()
	}
	// op содержит текущую наблюдаемую операцию HTTP или RTMP Serve для присоединения после сигнала
	// остановки.
	for _, op := range []*operation{a.httpServe, a.ingestServe} {
		if op == nil {
			continue
		}
		if !op.wait(ctx) {
			joined = false
			continue
		}
		// err содержит результат завершённого Serve для сохранения только неожиданных отказов
		// обслуживания.
		err := op.result.err
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, context.Canceled) && !errors.Is(err, core.ErrClosed) {
			a.errors = append(a.errors, err)
		}
	}
	if !joined {
		return errors.Join(errors.Join(a.errors...), ErrPending, ctx.Err())
	}
	if !a.waitOwners(ctx, a.owners.registry, a.owners.files) {
		return errors.Join(errors.Join(a.errors...), ErrPending, ctx.Err())
	}
	if !a.waitOwners(ctx, a.owners.store) {
		return errors.Join(errors.Join(a.errors...), ErrPending, ctx.Err())
	}
	if !a.waitOwners(ctx, a.owners.logger) {
		return errors.Join(errors.Join(a.errors...), ErrPending, ctx.Err())
	}
	a.doneOnce.Do(func() {
		// Закрывает Done только после подтверждённого завершения всех владельцев.
		close(a.done)
	})
	return errors.Join(a.errors...)
}
