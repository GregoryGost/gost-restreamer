package media_test

import (
	"math"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media"
)

func TestQueueLimitsValidation(t *testing.T) {
	base := media.QueueLimits{MaxPackets: 2, MaxBytes: 100, MaxPacketBytes: 20, MaxConfigBytes: 3, ResyncTimeout: time.Second}
	for _, q := range []media.QueueLimits{base, {MaxPackets: 1, MaxBytes: 1, MaxPacketBytes: 1, MaxConfigBytes: 1, ResyncTimeout: time.Nanosecond}, {MaxPackets: math.MaxUint32, MaxBytes: math.MaxUint64, MaxPacketBytes: math.MaxUint64, MaxConfigBytes: math.MaxUint64, ResyncTimeout: time.Duration(math.MaxInt64)}} {
		if err := q.Validate(); err != nil {
			t.Fatalf("корректные ограничения %v отклонены: %v", q, err)
		}
	}
	cases := []struct {
		name   string
		mutate func(*media.QueueLimits)
	}{
		{"packets_zero", func(q *media.QueueLimits) { q.MaxPackets = 0 }},
		{"bytes_zero", func(q *media.QueueLimits) { q.MaxBytes = 0 }},
		{"packet_bytes_zero", func(q *media.QueueLimits) { q.MaxPacketBytes = 0 }},
		{"config_bytes_zero", func(q *media.QueueLimits) { q.MaxConfigBytes = 0 }},
		{"config_exceeds_packet", func(q *media.QueueLimits) { q.MaxConfigBytes = 21 }},
		{"packet_exceeds_queue", func(q *media.QueueLimits) { q.MaxPacketBytes = 101 }},
		{"timeout_zero", func(q *media.QueueLimits) { q.ResyncTimeout = 0 }},
		{"timeout_negative", func(q *media.QueueLimits) { q.ResyncTimeout = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := base
			tc.mutate(&q)
			requireCategory(t, q.Validate(), media.ErrInvalidQueueLimits)
		})
	}
	requireCategory(t, (media.QueueLimits{}).Validate(), media.ErrInvalidQueueLimits)
}

func TestWatchdogPolicyValidation(t *testing.T) {
	base := media.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Nanosecond, RecoveryWindow: 3 * time.Second, KeyframeTimeout: 2 * time.Second}
	for _, p := range []media.WatchdogPolicy{base, {VideoTimeout: 1, AudioTimeout: 1, RecoveryWindow: 1, KeyframeTimeout: 1}, {VideoTimeout: time.Duration(math.MaxInt64), AudioTimeout: time.Duration(math.MaxInt64), RecoveryWindow: time.Duration(math.MaxInt64), KeyframeTimeout: time.Duration(math.MaxInt64)}} {
		if err := p.Validate(); err != nil {
			t.Fatalf("Корректная политика %v отклонена: %v", p, err)
		}
	}
	for _, duration := range []time.Duration{0, -1, time.Duration(math.MinInt64)} {
		for _, tc := range []struct {
			name   string
			mutate func(*media.WatchdogPolicy)
		}{
			{"video", func(p *media.WatchdogPolicy) { p.VideoTimeout = duration }},
			{"audio", func(p *media.WatchdogPolicy) { p.AudioTimeout = duration }},
			{"recovery", func(p *media.WatchdogPolicy) { p.RecoveryWindow = duration }},
			{"keyframe", func(p *media.WatchdogPolicy) { p.KeyframeTimeout = duration }},
		} {
			t.Run(tc.name+"/"+duration.String(), func(t *testing.T) {
				p := base
				tc.mutate(&p)
				requireCategory(t, p.Validate(), media.ErrInvalidWatchdogPolicy)
			})
		}
	}
	requireCategory(t, (media.WatchdogPolicy{}).Validate(), media.ErrInvalidWatchdogPolicy)
}
