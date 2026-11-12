package fallback

import (
	"context"
	"errors"
	"io"
	"sync"
)

// uploadTransfer отделяет владение телом загрузки от дальнейшей подготовки.
type uploadTransfer struct {
	// mu защищает подтверждение передачи и сохранённые причины её завершения.
	mu sync.Mutex
	// reader содержит принадлежащее заданию тело загрузки, закрываемое до публикации результата
	// передачи.
	reader io.ReadCloser
	// done закрывается после сохранения результата передачи и завершения закрытия тела загрузки.
	done chan struct{}
	// finished показывает, опубликован ли окончательный результат передачи.
	finished bool
	// aborted сохраняет первую причину отмены ещё не подтверждённой загрузки.
	aborted error
	// err хранит объединённый окончательный результат передачи.
	err error
}

// WaitUpload ждёт только сохранения исходного файла и закрытия reader.
// Отмена ctx до подтверждения передачи отменяет всё задание и закрывает reader.
// После подтверждения передачи отмена ctx не влияет на фоновую подготовку.
// Для Rebuild метод возвращает ErrInvalid; Wait и Cancel относятся ко всему заданию.
func (j *Job) WaitUpload(ctx context.Context) error {
	if j.upload == nil {
		return ErrInvalid
	}
	select {
	case <-j.upload.done:
		return j.upload.result()
	case <-ctx.Done():
		return j.abortUpload(ctx.Err())
	}
}

// Отменяет ещё не подтверждённую загрузку и ждёт закрытия её тела; уже завершённая передача возвращает
// сохранённый результат без отмены подготовки.
func (j *Job) abortUpload(err error) error {
	// u обозначает состояние передачи тела текущего задания.
	u := j.upload
	u.mu.Lock()
	if u.finished {
		err = u.err
		u.mu.Unlock()
		return err
	}
	if u.aborted == nil {
		u.aborted = err
	}
	u.mu.Unlock()
	j.cancel()
	return u.complete(context.Background(), err)
}

// Закрывает тело загрузки до публикации результата передачи; первый результат объединяет отмену,
// ошибку передачи, закрытия и контекста, последующие вызовы его сохраняют.
func (u *uploadTransfer) complete(ctx context.Context, err error) error {
	// ownedReader обеспечивает единственное закрытие и ожидание его завершения.
	// closeErr содержит результат закрытия тела загрузки, включаемый в итог передачи.
	closeErr := u.reader.Close()
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.finished {
		u.err = errors.Join(u.aborted, err, closeErr, ctx.Err())
		u.finished = true
		close(u.done)
	}
	return u.err
}

// Возвращает сохранённый результат передачи под блокировкой.
func (u *uploadTransfer) result() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.err
}
