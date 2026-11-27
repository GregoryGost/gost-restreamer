package chat

import (
	"context"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// acquireChatSlot ждёт допуска к сетевому ресурсу в пределах ctx. Продолжение освобождает допуск
// после завершения операции. Вызывающий вызывает его ровно один раз после успешного получения.
func acquireChatSlot(ctx context.Context, slots chan struct{}) (func(), error) {
	if ctx == nil {
		return nil, core.ErrInvalid
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case slots <- struct{}{}:
	}
	// err определяет, совпала ли отмена с получением места.
	if err := ctx.Err(); err != nil {
		<-slots
		return nil, err
	}
	// Продолжение возвращает место следующей операции после физического освобождения ресурса.
	return func() { <-slots }, nil
}
