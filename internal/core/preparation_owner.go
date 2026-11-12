package core

import (
	"context"
	"sync"
	"sync/atomic"
)

// PreparationOwner запрещает новые действия после отзыва и учитывает незавершённые операции.
// Register вызывается до первого возможного внешнего действия, finish — после полного завершения операции.
// Done закрывается при отзыве; уже зарегистрированные операции учитываются до вызова finish.
type PreparationOwner struct {
	// authority связывает владельца подготовки с общей блокировкой и реестром Store.
	authority *RuntimeAuthority
	// fence фиксирует точное поколение и выданную попытку подготовки.
	fence PreparationFence
	// valid показывает, разрешены ли новые внешние операции подготовки.
	valid atomic.Bool
	// done закрывается при логическом отзыве владельца подготовки.
	done chan struct{}
	// operations считает зарегистрированные внешние операции до их физического завершения.
	operations uint64
}

// Valid сообщает, сохранён ли логический допуск подготовки; незавершённые операции этим не
// подтверждаются.
func (o *PreparationOwner) Valid() bool { return o != nil && o.valid.Load() }

// Done возвращает канал отзыва допуска подготовки.
func (o *PreparationOwner) Done() <-chan struct{} { return o.done }

// Fence возвращает точную сохранённую идентичность владельца подготовки.
func (o *PreparationOwner) Fence() PreparationFence { return o.fence }

// Register под общей блокировкой RuntimeAuthority допускает внешнюю операцию и увеличивает число
// незавершённых операций. Возвращённую функцию вызывают после физического завершения; повторный вызов
// не меняет счётчик.
func (o *PreparationOwner) Register(ctx context.Context) (func(), error) {
	if o == nil {
		return nil, ErrInvalid
	}
	// a предоставляет общую блокировку допуска и учёта операции.
	a := o.authority
	// err сообщает об ошибке ожидания блокировки перед регистрацией внешней операции.
	if err := a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	if !o.valid.Load() {
		return nil, ErrStale
	}
	if o.operations == ^uint64(0) {
		return nil, ErrBusy
	}
	o.operations++
	// once ограничивает уменьшение счётчика операций одним вызовом завершения регистрации.
	var once sync.Once
	return func() {
		// Возвращает завершение регистрации, которое уменьшает operations не более одного раза, включая
		// повторные вызовы после отзыва владельца.

		once.Do(func() {
			// Под общей блокировкой уменьшает число незавершённых операций подготовки и освобождает блокировку.
			<-a.gate
			o.operations--
			a.release()
		})
	}, nil
}

// Revoke отзывает новые действия под общей блокировкой RuntimeAuthority, сохраняя учёт ранее
// зарегистрированных операций.
func (o *PreparationOwner) Revoke(ctx context.Context) error {
	if o == nil {
		return ErrInvalid
	}
	// a предоставляет общую блокировку отзыва новых действий подготовки.
	a := o.authority
	// err сообщает об ошибке ожидания блокировки перед отзывом владельца.
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	a.revokePreparation(o)
	return nil
}

// revokePreparation закрывает Done при первом отзыве владельца; вызывающий удерживает общую блокировку
// RuntimeAuthority.
func (a *RuntimeAuthority) revokePreparation(o *PreparationOwner) {
	if o.valid.Swap(false) {
		close(o.done)
	}
}

// BindPreparationOwner проверяет отсутствие активной сессии и точную выданную подготовку текущей
// incarnation, затем возвращает существующего действующего владельца либо регистрирует нового.
func (s *Service) BindPreparationOwner(ctx context.Context, f PreparationFence) (*PreparationOwner, error) {
	// a содержит общую RuntimeAuthority, а err — причину невозможности её получить.
	a, err := s.runtimeAuthority()
	if err != nil {
		return nil, err
	}
	if err = a.acquire(ctx); err != nil {
		return nil, err
	}
	defer a.release()
	err = s.uow.View(ctx, func(r Repositories) error {
		// Под уже занятой общей блокировкой читает точное выданное поколение и проверяет отсутствие эфира
		// перед выдачей PreparationOwner.

		// e сообщает об ошибке проверки отсутствия активного эфира перед допуском подготовки.
		if e := runtimeIdleForPreparation(ctx, r); e != nil {
			return e
		}
		// g содержит точное выданное поколение, а e — результат проверки его владельца и
		// попытки.
		g, e := preparationFenced(ctx, r, f)
		if e != nil {
			return e
		}
		if f.Incarnation != s.incarnation || g.State != PreparationIssued || !g.Issued {
			return ErrStale
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// o содержит ранее зарегистрированного владельца этого поколения, если он существует.
	if o := a.preparations[f.GenerationID]; o != nil {
		if o.fence != f || !o.valid.Load() {
			return nil, ErrStale
		}
		return o, nil
	}
	// o содержит нового владельца проверенной попытки подготовки.
	o := &PreparationOwner{authority: a, fence: f, done: make(chan struct{})}
	o.valid.Store(true)
	a.preparations[f.GenerationID] = o
	return o, nil
}

// reconcilePreparations сверяет владельцев с БД под общей блокировкой RuntimeAuthority, отзывает
// устаревших и удаляет завершённые записи без операций; ошибка чтения отзывает все оставшиеся допуски.
func (a *RuntimeAuthority) reconcilePreparations(ctx context.Context, uow UnitOfWork) {
	if len(a.preparations) == 0 {
		return
	}
	// err сообщает об ошибке согласованной сверки владельцев подготовки с БД.
	err := uow.View(ctx, func(r Repositories) error {
		// Сверяет зарегистрированных владельцев с БД, отзывает неактуальные исполнения и удаляет только
		// терминальные очищенные записи без незавершённых операций.

		// id идентифицирует зарегистрированное поколение, а o содержит владельца и число его
		// незавершённых операций.
		for id, o := range a.preparations {
			// g содержит точное сохранённое поколение, а e — результат проверки соответствия
			// владельцу.
			g, e := preparationFenced(ctx, r, o.fence)
			if e != nil || g.State != PreparationIssued || !g.Issued {
				a.revokePreparation(o)
			}
			if e == nil && !g.Issued && !g.CleanupPending && g.State.Terminal() && o.operations == 0 {
				delete(a.preparations, id)
			}
		}
		return nil
	})
	if err != nil {
		// o содержит очередного владельца, отзываемого после отказа чтения БД.
		for _, o := range a.preparations {
			a.revokePreparation(o)
		}
	}
}
