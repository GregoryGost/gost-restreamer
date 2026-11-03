package media

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidQueueLimits обозначает некорректные ограничения очереди.
var ErrInvalidQueueLimits = errors.New("некорректные ограничения очереди")

// ErrInvalidWatchdogPolicy обозначает некорректные интервалы контроля источника.
var ErrInvalidWatchdogPolicy = errors.New("некорректные интервалы контроля источника")

// QueueLimits задаёт явные бюджеты очереди и ограничение ожидания синхронизации.
type QueueLimits struct {
	MaxPackets     uint32
	MaxBytes       uint64
	MaxPacketBytes uint64
	MaxConfigBytes uint64
	ResyncTimeout  time.Duration
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
type WatchdogPolicy struct {
	VideoTimeout    time.Duration
	AudioTimeout    time.Duration
	RecoveryWindow  time.Duration
	KeyframeTimeout time.Duration
}

// Validate проверяет положительность интервалов без назначения значений по умолчанию.
func (p WatchdogPolicy) Validate() error {
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"VideoTimeout", p.VideoTimeout},
		{"AudioTimeout", p.AudioTimeout},
		{"RecoveryWindow", p.RecoveryWindow},
		{"KeyframeTimeout", p.KeyframeTimeout},
	} {
		if field.value <= 0 {
			return fmt.Errorf("%w: WatchdogPolicy.%s должен быть положительным", ErrInvalidWatchdogPolicy, field.name)
		}
	}
	return nil
}
