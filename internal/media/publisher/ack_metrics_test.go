package publisher

import (
	"math"
	"testing"
	"time"
)

// Проверяет запись ACK без захвата p.mu, согласованность полей и обновление последнего значения и
// времени после насыщения счётчика событий.
func TestACKAccumulationDoesNotAcquirePublisherMutex(t *testing.T) {
	// router содержит Router для проверки локальных накоплений ACK без сетевого цикла.
	router, _, _, _ := testRouter(t)
	// p содержит Publisher, которому события ACK передаются при удерживаемом p.mu.
	p, _ := testPublisher(t, router, nil)
	// first задаёт исходное время контрольных аномалий ACK.
	first := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	p.mu.Lock()
	p.noteACKAnomaly(9, first)
	p.noteACKAnomaly(4, first.Add(time.Second))
	p.mu.Unlock()
	// s содержит снимок согласованных полей ACK до и после насыщения счётчика событий.
	s := p.Snapshot()
	if s.AckAheadEventsTotal != 2 || s.AckAheadBytesLast != 4 || s.AckAheadBytesMax != 9 || s.AckLastAnomalyAt != first.Add(time.Second) {
		t.Fatalf("накопления ACK несогласованы: %+v", s)
	}
	p.writeMu.Lock()
	p.ackAheadEventsTotal = math.MaxUint64
	p.writeMu.Unlock()
	p.noteACKAnomaly(12, first.Add(2*time.Second))
	s = p.Snapshot()
	if !s.Saturated || s.AckAheadEventsTotal != math.MaxUint64 || s.AckAheadBytesLast != 12 || s.AckAheadBytesMax != 12 || s.AckLastAnomalyAt != first.Add(2*time.Second) {
		t.Fatalf("насыщение остановило last/max/time: %+v", s)
	}
	// other содержит новый Publisher для проверки отсутствия унаследованных накоплений.
	other, _ := testPublisher(t, router, nil)
	// fresh содержит снимок нового Publisher с ожидаемыми нулевыми полями ACK.
	if fresh := other.Snapshot(); fresh.AckAheadEventsTotal != 0 || fresh.AckAheadBytesLast != 0 || fresh.AckAheadBytesMax != 0 || !fresh.AckLastAnomalyAt.IsZero() {
		t.Fatalf("новый Publisher унаследовал накопления: %+v", fresh)
	}
}
