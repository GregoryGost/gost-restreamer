package fallback

import (
	"context"
	"errors"
	"io"
	"sync"
)

// uploadTransfer отделяет владение телом загрузки от дальнейшей подготовки.
type uploadTransfer struct {
	mu       sync.Mutex
	reader   io.ReadCloser
	done     chan struct{}
	finished bool
	aborted  error
	err      error
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

func (j *Job) abortUpload(err error) error {
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

func (u *uploadTransfer) complete(ctx context.Context, err error) error {
	// ownedReader обеспечивает единственное закрытие и ожидание его завершения.
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

func (u *uploadTransfer) result() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.err
}
