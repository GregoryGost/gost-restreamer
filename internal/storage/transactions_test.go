package storage

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/mattn/go-sqlite3"
)

// Проверяет запрет записи через View, использования репозитория после возврата переданной функции и
// операций над закрытым Store.
func TestRepositoryScopeRejectsReadOnlyAndEscapedWrites(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// escaped сохраняет репозиторий View за пределами callback для проверки закрытия доступа.
	var escaped core.ConfigRepository
	// err сообщает об ошибке запрещённого удаления профиля через репозиторий согласованного чтения.
	err := f.store.View(ctx, func(r core.Repositories) error {
		// Сохраняет ссылку на репозиторий View и пробует запрещённое удаление, чтобы затем проверить закрытие
		// вышедшей за область ссылки.

		escaped = r.Config
		return r.Config.DeleteProfile(ctx, f.profile.ID, f.profile.Revision)
	})
	requireDomainError(t, err, core.ErrInvalid)
	_, err = escaped.Profile(ctx, f.profile.ID)
	requireDomainError(t, err, core.ErrClosed)
	// err сообщает об ошибке закрытия проверяемого Store.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	err = f.store.View(ctx, func(core.Repositories) error {
		// Возвращает успех из функции чтения, которая должна быть отклонена закрытым Store до вызова.
		return nil
	})
	requireDomainError(t, err, core.ErrClosed)
	err = f.store.Write(ctx, func(core.Repositories) error {
		// Возвращает успех из функции записи, которая должна быть отклонена закрытым Store до вызова.
		return nil
	})
	requireDomainError(t, err, core.ErrClosed)
}

// Проверяет неизменность читающего снимка во время конкурентной записи и видимость COMMIT новому
// читателю без оставшихся занятых соединений.
func TestReadSnapshotsStayIndependentAcrossConcurrentWrite(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite. cancel освобождает или
	// отменяет контекст проверяемого ожидания.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// read передаёт результат первого чтения удерживаемого снимка.
	read := make(chan error, 1)
	// release разрешает читающей транзакции продолжить проверку после параллельной записи.
	release := make(chan struct{})
	// done передаёт итог удерживаемой читающей транзакции.
	done := make(chan error, 1)
	go func() {
		// В отдельной горутине удерживает согласованное чтение до разрешения основной проверки и передаёт
		// результат через done.

		done <- f.store.View(ctx, func(r core.Repositories) error {
			// Читает профиль до и после параллельного сохранения, сохраняя прежние Name и Revision внутри одного
			// View.

			// before сохраняет профиль первого чтения внутри удерживаемого снимка. err
			// сообщает об ошибке чтения сохранённого полного профиля.
			before, err := r.Config.Profile(ctx, f.profile.ID)
			read <- err
			if err != nil {
				return err
			}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			// after содержит повторно прочитанный профиль в той же читающей транзакции.
			after, err := r.Config.Profile(ctx, f.profile.ID)
			if err != nil {
				return err
			}
			if before.Name != after.Name || before.Revision != after.Revision {
				return fmt.Errorf("читающий снимок изменился с %q/%d на %q/%d", before.Name, before.Revision, after.Name, after.Revision)
			}
			return nil
		})
	}()
	// observed накапливает итог начального чтения, конкурентной записи и проверки нового снимка.
	var observed error
	select {
	case observed = <-read:
	case <-ctx.Done():
		observed = ctx.Err()
	}
	if observed == nil {
		// p содержит полный именованный профиль для проверяемого изменения.
		p := f.profile
		p.Name = "Изменённый профиль"
		_, observed = f.service.UpdateProfile(ctx, p, p.Revision)
		if observed == nil {
			observed = f.store.View(ctx, func(r core.Repositories) error {
				// Проверяет, что новый View видит сохранённое имя и увеличенную Revision профиля.

				// got содержит профиль нового читателя после конкурентного COMMIT. err
				// сообщает об ошибке чтения сохранённого полного профиля.
				got, err := r.Config.Profile(ctx, p.ID)
				if err != nil {
					return err
				}
				if got.Name != p.Name || got.Revision != p.Revision+1 {
					return fmt.Errorf("новый читатель не видит committed изменение: %+v", got)
				}
				return nil
			})
		}
	}
	close(release)
	select {
	// err содержит итог завершившейся удерживаемой читающей транзакции.
	case err := <-done:
		if err != nil {
			t.Errorf("изолированное чтение: %v", err)
		}
	case <-ctx.Done():
		t.Errorf("чтение не завершено: %v", ctx.Err())
	}
	if observed != nil {
		t.Fatal(observed)
	}
	// inUse содержит число соединений, оставшихся занятыми после завершения транзакций.
	if inUse := f.store.sqlDB.Stats().InUse; inUse != 0 {
		t.Fatalf("после параллельных транзакций занято соединений: %d", inUse)
	}
}

// Проверяет конечные бюджеты ожидания занятой записи и запрет вызова переданной функции до получения
// права записи.
func TestWriterContentionHonorsRetryAndCallerBudgets(t *testing.T) {
	// s владеет временным Store проверяемого сценария.
	s := openTestStore(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// conn удерживает отдельное физическое соединение для внешней блокировки писателя. err сообщает
	// об ошибке получения выделенного физического соединения.
	conn, err := s.sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Возвращает занятое соединение теста конкурирующего писателя в пул.

		// err сообщает об ошибке возврата отдельного соединения в пул.
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})
	// err сообщает об ошибке получения внешней блокировки писателя.
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Откатывает внешнюю BEGIN IMMEDIATE после проверки ограничений ожидания Write.

		// err сообщает об ошибке снятия внешней блокировки писателя.
		if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
			t.Error(err)
		}
	})
	// invoked считает вызовы callback, которые запрещены до получения блокировки писателя.
	var invoked atomic.Int32
	// callback отмечает исполнение записи, чтобы обнаружить допуск без блокировки писателя.
	callback := func(core.Repositories) error {
		// Считает вызовы функции записи, которая не должна исполняться без блокировки писателя.
		invoked.Add(1)
		return nil
	}
	// start фиксирует начало ожидания занятого писателя.
	start := time.Now()
	err = s.Write(ctx, callback)
	requireDomainError(t, err, core.ErrBusy)
	// elapsed содержит фактически прошедшее время ограниченного ожидания писателя.
	if elapsed := time.Since(start); elapsed > 2*s.options.RetryTimeout {
		t.Fatalf("занятая запись превысила общий бюджет: %v", elapsed)
	}
	if invoked.Load() != 0 {
		t.Fatal("callback выполнился без writer lock")
	}
	// short задаёт короткий срок вызывающего поверх общего бюджета повторов. cancel освобождает или
	// отменяет контекст проверяемого ожидания.
	short, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	err = s.Write(short, callback)
	requireDomainError(t, err, context.DeadlineExceeded)
	if invoked.Load() != 0 {
		t.Fatal("отменённый callback выполнился")
	}
}

// Проверяет полный откат первой попытки переданной функции с ErrBusy и сохранение единственного
// результата успешного повтора.
func TestTransientCallbackRetryRollsBackEveryFailedAttempt(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// attempts считает исполнения callback для проверки отката до разрешённого повтора.
	attempts := 0
	// err сообщает об ошибке общей записи с откатом первой ErrBusy-попытки и разрешённым повтором.
	err := f.store.Write(ctx, func(r core.Repositories) error {
		// Вставляет один и тот же Stream и на первой попытке возвращает ErrBusy, проверяя полный откат перед
		// повтором.

		attempts++
		// value задаёт синтетическую конфигурацию потока, сохраняемую внутри проверяемой
		// транзакции.
		value := core.Stream{ID: "retry-stream", Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Name: "Повтор", PublicationKey: "retry-synthetic-key"}
		// err сообщает об ошибке вставки конфигурации потока.
		if err := r.Config.InsertStream(ctx, value); err != nil {
			return err
		}
		if attempts == 1 {
			return translate(sqlite3.Error{Code: sqlite3.ErrBusy, ExtendedCode: sqlite3.ErrBusy.Extend(0)})
		}
		return nil
	})
	if err != nil || attempts != 2 {
		t.Fatalf("повтор целого откатившегося callback: попыток %d, ошибка %v", attempts, err)
	}
	// err сообщает об ошибке чтения конфигурации потока.
	if err := f.store.View(ctx, func(r core.Repositories) error {
		// Проверяет, что успешная повторная попытка сохранила Stream и его PublicationKey.

		// got содержит прочитанное сохраняемое значение для проверки результата операции. err
		// сообщает об ошибке чтения конфигурации потока.
		got, err := r.Config.Stream(ctx, "retry-stream")
		if err != nil {
			return err
		}
		if got.PublicationKey != "retry-synthetic-key" {
			t.Fatal("успешная попытка не сохранилась")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// got содержит число занятых соединений после повторённой записи.
	if got := f.store.sqlDB.Stats().InUse; got != 0 {
		t.Fatalf("повтор оставил соединения: %d", got)
	}
}

// Проверяет сохранение отмены, неизвестной ошибки и категории повреждения без подмены на ErrNotFound.
func TestCanceledAndUnknownFailuresDoNotBecomeNotFound(t *testing.T) {
	// s владеет временным Store проверяемого сценария.
	s := openTestStore(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite. cancel освобождает или
	// отменяет контекст проверяемого ожидания.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// called отмечает ошибочный вызов записи при заранее отменённом контексте.
	called := false
	// err сообщает об ошибке записи с уже отменённым контекстом до вызова тела транзакции.
	err := s.Write(ctx, func(core.Repositories) error {
		// Отмечает ошибочный вызов записи, который отменённый контекст должен запретить до исполнения.
		called = true
		return nil
	})
	requireDomainError(t, err, context.Canceled)
	if called {
		t.Fatal("отменённая операция выполнила callback")
	}
	// unknown задаёт неизвестную ошибку для проверки сохранения её идентичности при переводе.
	unknown := errors.New("синтетическая неизвестная ошибка")
	// got содержит переведённую неизвестную ошибку для проверки сохранения исходного значения.
	if got := translate(unknown); got != unknown || errors.Is(got, core.ErrNotFound) {
		t.Fatalf("неизвестная ошибка заменена: %v", got)
	}
	requireDomainError(t, translate(sqlite3.Error{Code: sqlite3.ErrCorrupt}), core.ErrCorrupt)
}
