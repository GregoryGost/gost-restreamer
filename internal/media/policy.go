package media

import (
	"errors"
	"fmt"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// ErrInvalidQueueLimits обозначает некорректные ограничения очереди.
var ErrInvalidQueueLimits = errors.New("некорректные ограничения очереди")

// ErrInvalidWatchdogPolicy обозначает некорректные интервалы контроля источника.
var ErrInvalidWatchdogPolicy = core.ErrInvalidWatchdogPolicy

// QueueLimits задаёт явные бюджеты очереди и ограничение ожидания синхронизации.
type QueueLimits struct {
	// MaxPackets ограничивает общее число пакетов в очереди и ожидающем аудио.
	MaxPackets uint32
	// MaxBytes ограничивает логический вес удерживаемых пакетов вместе с парой конфигураций, в
	// байтах.
	MaxBytes uint64
	// MaxPacketBytes ограничивает логический вес одного пакета вместе с его конфигурацией, в байтах.
	MaxPacketBytes uint64
	// MaxConfigBytes ограничивает размер заголовков каждой дорожки, в байтах.
	MaxConfigBytes uint64
	// ResyncTimeout задаёт максимальное время ожидания согласованного запуска аудио и видео после
	// сброса.
	ResyncTimeout time.Duration
}

// Validate проверяет положительность бюджетов и вложенность ограничений байтов.
func (l QueueLimits) Validate() error {
	if l.MaxPackets == 0 {
		return fmt.Errorf("%w: QueueLimits.MaxPackets должен быть положительным", ErrInvalidQueueLimits)
	}
	if l.MaxBytes == 0 {
		return fmt.Errorf("%w: QueueLimits.MaxBytes должен быть положительным", ErrInvalidQueueLimits)
	}
	if l.MaxPacketBytes == 0 {
		return fmt.Errorf("%w: QueueLimits.MaxPacketBytes должен быть положительным", ErrInvalidQueueLimits)
	}
	if l.MaxConfigBytes == 0 {
		return fmt.Errorf("%w: QueueLimits.MaxConfigBytes должен быть положительным", ErrInvalidQueueLimits)
	}
	if l.MaxConfigBytes > l.MaxPacketBytes {
		return fmt.Errorf("%w: QueueLimits.MaxConfigBytes превышает MaxPacketBytes", ErrInvalidQueueLimits)
	}
	if l.MaxPacketBytes > l.MaxBytes {
		return fmt.Errorf("%w: QueueLimits.MaxPacketBytes превышает MaxBytes", ErrInvalidQueueLimits)
	}
	if l.ResyncTimeout <= 0 {
		return fmt.Errorf("%w: QueueLimits.ResyncTimeout должен быть положительным", ErrInvalidQueueLimits)
	}
	return nil
}

// WatchdogPolicy задаёт явные интервалы контроля и восстановления обеих дорожек.
type WatchdogPolicy = core.WatchdogPolicy
