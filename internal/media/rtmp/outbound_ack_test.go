package rtmp

import (
	"context"
	"errors"
	"io"
	"math"
	"testing"
	"time"
)

func TestOutboundACKSerialBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		anchor, written       uint64
		raw                   uint32
		wantAnchor, wantAhead uint64
		unknown               bool
	}{
		{"normal", 100, 120, 110, 110, 0, false},
		{"ahead", 100, 120, 124, 124, 4, false},
		{"duplicate", 100, 120, 100, 100, 0, false},
		{"stale", 100, 120, 99, 100, 0, false},
		{"half_range", 100, 120, (1 << 31) + 100, 100, 0, false},
		{"wrap", (1 << 32) - 2, (1 << 32) + 2, 4, (1 << 32) + 4, 2, false},
		{"epoch_gap", 100, (1 << 31) + 100, 101, 100, 0, true},
		{"last_known_gap", 100, (1 << 31) + 99, 101, 101, 0, false},
		{"far_candidate", 100, 0, (1 << 31) + 99, 100, 0, false},
		{"checked_add_overflow", math.MaxUint64 - 2, math.MaxUint64 - 2, 1, math.MaxUint64 - 2, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := transportSelfConn(t, &transportSelfMemory{})
			c.peerAcknowledged, c.stats.WrittenBytes = tc.anchor, tc.written
			var observed uint64
			var observedAt time.Time
			c.opts.OnACKAnomaly = func(bytes uint64, at time.Time) {
				if !c.mu.TryLock() {
					t.Error("Обработчик вызван под блокировкой транспорта")
					return
				}
				c.mu.Unlock()
				observed, observedAt = bytes, at
			}
			c.acknowledge(tc.raw)
			s := c.Snapshot()
			if c.peerAcknowledged != tc.wantAnchor || c.ackEpochUnknown != tc.unknown || s.AckAheadBytesLast != tc.wantAhead || observed != tc.wantAhead || s.AckLastAnomalyAt != observedAt {
				t.Fatalf("якорь=%d неизвестная_эпоха=%v снимок=%+v обработчик=%d/%v", c.peerAcknowledged, c.ackEpochUnknown, s, observed, observedAt)
			}
			wantEvents := uint64(0)
			if tc.wantAhead != 0 {
				wantEvents = 1
			}
			if s.AckAheadEventsTotal != wantEvents || s.AckAheadBytesMax != tc.wantAhead || c.Err() != nil {
				t.Fatalf("неверные показатели: %+v err=%v", s, c.Err())
			}
		})
	}
}

func TestOutboundACKDistinctEventsAndSaturation(t *testing.T) {
	c := transportSelfConn(t, &transportSelfMemory{})
	c.stats.WrittenBytes = 100
	for raw := uint32(101); raw <= 110; raw++ {
		c.acknowledge(raw)
		c.acknowledge(raw)
		c.acknowledge(raw - 1)
	}
	s := c.Snapshot()
	if s.AckAheadEventsTotal != 10 || s.AckAheadBytesLast != 10 || s.AckAheadBytesMax != 10 || s.AckLastAnomalyAt.IsZero() {
		t.Fatalf("разные ACK серии учтены неверно: %+v", s)
	}
	c.stats.AckAheadEventsTotal = math.MaxUint64
	c.acknowledge(111)
	s = c.Snapshot()
	if !s.Saturated || c.ackEpochUnknown || s.AckAheadEventsTotal != math.MaxUint64 || s.AckAheadBytesLast != 11 || s.AckAheadBytesMax != 11 {
		t.Fatalf("насыщение total прекратило измерение: %+v", s)
	}
	c.stats.WrittenBytes = 200
	c.acknowledge(200)
	if got := c.Snapshot(); got.AckAheadBytesLast != s.AckAheadBytesLast || got.AckAheadBytesMax != s.AckAheadBytesMax || got.AckLastAnomalyAt != s.AckLastAnomalyAt {
		t.Fatalf("обычный ACK сбросил аномалию: %+v", got)
	}
}

type ackDuringWriteConn struct {
	*transportSelfMemory
	onWrite func()
}

func (c ackDuringWriteConn) Write(p []byte) (int, error) {
	c.onWrite()
	return c.transportSelfMemory.Write(p)
}

func TestOutboundACKDuringPartialWriteAdvancesWithoutReplay(t *testing.T) {
	base := &transportSelfMemory{short: 2, failure: io.ErrClosedPipe}
	c := transportSelfConn(t, base)
	c.stats.WrittenBytes, c.peerAcknowledged = 100, 100
	c.conn = ackDuringWriteConn{transportSelfMemory: base, onWrite: func() {
		c.acknowledge(104)
		if c.peerAcknowledged != 104 || c.Snapshot().WrittenBytes != 100 {
			t.Error("активная запись потеряла якорь или опубликовала ещё не записанные байты")
		}
	}}
	callbacks := 0
	c.opts.OnACKAnomaly = func(uint64, time.Time) { callbacks++ }
	err := c.withWriter(context.Background(), func() error { return c.writeAll([]byte{1, 2, 3, 4}) })
	if !errors.Is(err, io.ErrClosedPipe) || c.Snapshot().WrittenBytes != 102 || c.writeActive || !base.closed {
		t.Fatalf("частичная запись: %+v active=%v err=%v", c.Snapshot(), c.writeActive, err)
	}
	c.acknowledge(104)
	if c.Snapshot().AckAheadEventsTotal != 0 || callbacks != 0 {
		t.Fatal("ACK активной записи был переигран")
	}
	c.acknowledge(105)
	if s := c.Snapshot(); s.AckAheadEventsTotal != 1 || s.AckAheadBytesLast != 3 || callbacks != 1 {
		t.Fatalf("новое измеримое подтверждение потеряно: %+v", s)
	}
}

func TestOutboundACKUnknownEpochPersistsWithoutStoppingWrites(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		c := transportSelfConn(t, &transportSelfMemory{})
		if overflow {
			c.stats.WrittenBytes = math.MaxUint64 - 2
			c.peerAcknowledged = c.stats.WrittenBytes
		} else {
			c.stats.WrittenBytes = (1 << 31) - 2
		}
		if err := c.writeAll([]byte{1, 2, 3, 4}); err != nil || !c.ackEpochUnknown || c.writtenOverflow != overflow {
			t.Fatalf("потеря эпохи: overflow=%v unknown=%v err=%v", c.writtenOverflow, c.ackEpochUnknown, err)
		}
		c.acknowledge(uint32(c.stats.WrittenBytes))
		c.acknowledge(uint32(c.stats.WrittenBytes + 1))
		if err := c.writeAll([]byte{5}); err != nil || c.Snapshot().AckAheadEventsTotal != 0 || !c.ackEpochUnknown {
			t.Fatalf("потеря эпохи остановила запись или восстановилась: %+v err=%v", c.Snapshot(), err)
		}
	}
}
