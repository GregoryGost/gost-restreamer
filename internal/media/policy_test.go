package media_test

import (
	"math"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// Проверяет положительность бюджетов и срока синхронизации, вложенность байтовых ограничений и
// допустимость предельных положительных значений.
func TestQueueLimitsValidation(t *testing.T) {
	// base содержит допустимые исходные бюджеты очереди для проверки отдельных нарушений.
	base := media.QueueLimits{MaxPackets: 2, MaxBytes: 100, MaxPacketBytes: 20, MaxConfigBytes: 3, ResyncTimeout: time.Second}
	// q содержит проверяемый допустимый набор бюджетов, включая минимальные и предельные значения.
	for _, q := range []media.QueueLimits{base, {MaxPackets: 1, MaxBytes: 1, MaxPacketBytes: 1, MaxConfigBytes: 1, ResyncTimeout: time.Nanosecond}, {MaxPackets: math.MaxUint32, MaxBytes: math.MaxUint64, MaxPacketBytes: math.MaxUint64, MaxConfigBytes: math.MaxUint64, ResyncTimeout: time.Duration(math.MaxInt64)}} {
		// err сообщает, отклонена ли допустимая конфигурация очереди.
		if err := q.Validate(); err != nil {
			t.Fatalf("корректные ограничения %v отклонены: %v", q, err)
		}
	}
	// cases перечисляет отдельные нарушения ограничений очереди.
	cases := []struct {
		// name задаёт имя подтеста для конкретного нарушения очереди.
		name string
		// mutate изменяет одно ограничение допустимой очереди, чтобы получить проверяемое
		// нарушение.
		mutate func(*media.QueueLimits)
	}{
		{"packets_zero", func(q *media.QueueLimits) {
			// Обнуляет допустимое число пакетов очереди.
			q.MaxPackets = 0
		}},
		{"bytes_zero", func(q *media.QueueLimits) {
			// Обнуляет общий бюджет байтов очереди.
			q.MaxBytes = 0
		}},
		{"packet_bytes_zero", func(q *media.QueueLimits) {
			// Обнуляет предел размера пакета.
			q.MaxPacketBytes = 0
		}},
		{"config_bytes_zero", func(q *media.QueueLimits) {
			// Обнуляет предел размера конфигурации.
			q.MaxConfigBytes = 0
		}},
		{"config_exceeds_packet", func(q *media.QueueLimits) {
			// Задаёт предел конфигурации больше предела пакета.
			q.MaxConfigBytes = 21
		}},
		{"packet_exceeds_queue", func(q *media.QueueLimits) {
			// Задаёт предел пакета больше общего бюджета очереди.
			q.MaxPacketBytes = 101
		}},
		{"timeout_zero", func(q *media.QueueLimits) {
			// Обнуляет срок синхронизации очереди.
			q.ResyncTimeout = 0
		}},
		{"timeout_negative", func(q *media.QueueLimits) {
			// Задаёт отрицательный срок синхронизации очереди.
			q.ResyncTimeout = -1
		}},
	}
	// tc содержит выбранное нарушение ограничений очереди.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Применяет выбранное нарушение бюджетов к копии допустимых ограничений и проверяет
			// ErrInvalidQueueLimits.

			// q содержит отдельную копию допустимых ограничений для выбранного нарушения.
			q := base
			tc.mutate(&q)
			requireCategory(t, q.Validate(), media.ErrInvalidQueueLimits)
		})
	}
	requireCategory(t, (media.QueueLimits{}).Validate(), media.ErrInvalidQueueLimits)
}

// Проверяет, что все четыре интервала WatchdogPolicy положительны, включая приём MaxInt64 и отказ для
// нуля и отрицательных значений.
func TestWatchdogPolicyValidation(t *testing.T) {
	// base содержит допустимые интервалы контроля и восстановления источника.
	base := media.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Nanosecond, RecoveryWindow: 3 * time.Second, KeyframeTimeout: 2 * time.Second}
	// p содержит проверяемый допустимый набор интервалов, включая минимальные и предельные значения.
	for _, p := range []media.WatchdogPolicy{base, {VideoTimeout: 1, AudioTimeout: 1, RecoveryWindow: 1, KeyframeTimeout: 1}, {VideoTimeout: time.Duration(math.MaxInt64), AudioTimeout: time.Duration(math.MaxInt64), RecoveryWindow: time.Duration(math.MaxInt64), KeyframeTimeout: time.Duration(math.MaxInt64)}} {
		// err сообщает, отклонена ли допустимая политика контроля источника.
		if err := p.Validate(); err != nil {
			t.Fatalf("Корректная политика %v отклонена: %v", p, err)
		}
	}
	// duration содержит неположительный интервал, который должен быть отклонён.
	for _, duration := range []time.Duration{0, -1, time.Duration(math.MinInt64)} {
		// tc выбирает поле политики для подстановки недопустимого интервала.
		for _, tc := range []struct {
			// name обозначает проверяемый интервал в имени подтеста.
			name string
			// mutate подставляет duration в выбранное поле политики.
			mutate func(*media.WatchdogPolicy)
		}{
			{"video", func(p *media.WatchdogPolicy) {
				// Подставляет выбранную неположительную длительность в срок контроля видео.
				p.VideoTimeout = duration
			}},
			{"audio", func(p *media.WatchdogPolicy) {
				// Подставляет выбранную неположительную длительность в срок контроля аудио.
				p.AudioTimeout = duration
			}},
			{"recovery", func(p *media.WatchdogPolicy) {
				// Подставляет выбранную неположительную длительность в окно восстановления.
				p.RecoveryWindow = duration
			}},
			{"keyframe", func(p *media.WatchdogPolicy) {
				// Подставляет выбранную неположительную длительность в срок ожидания ключевого кадра.
				p.KeyframeTimeout = duration
			}},
		} {
			t.Run(tc.name+"/"+duration.String(), func(t *testing.T) {
				// Проверяет ErrInvalidWatchdogPolicy после замены выбранного срока неположительной длительностью.

				// p содержит копию допустимой политики, изменяемую только в выбранном
				// подтесте.
				p := base
				tc.mutate(&p)
				requireCategory(t, p.Validate(), media.ErrInvalidWatchdogPolicy)
			})
		}
	}
	requireCategory(t, (media.WatchdogPolicy{}).Validate(), media.ErrInvalidWatchdogPolicy)
}
