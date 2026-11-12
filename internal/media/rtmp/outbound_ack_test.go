package rtmp

import (
	"context"
	"errors"
	"io"
	"math"
	"testing"
	"time"
)

// Проверяет продвижение абсолютного ACK, дубликаты, устаревание, полуоборот, сворачивание и
// переполнение, включая вызов наблюдателя вне блокировки транспорта.
func TestOutboundACKSerialBoundaries(t *testing.T) {
	// tc содержит исходный абсолютный ACK, записанный объём и независимые ожидания обработки нового
	// сетевого ACK.
	for _, tc := range []struct {
		// name обозначает границу последовательного номера в имени подтеста.
		name string
		// anchor задаёт прежний абсолютный ACK. written задаёт фактически учтённый объём записи.
		anchor, written uint64
		// raw задаёт новый сетевой ACK по модулю 2³².
		raw uint32
		// wantAnchor задаёт ожидаемый абсолютный ACK. wantAhead задаёт ожидаемое опережение в
		// байтах.
		wantAnchor, wantAhead uint64
		// unknown задаёт ожидаемую потерю однозначности абсолютной эпохи ACK.
		unknown bool
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
			// Проверяет продвижение опорного ACK, измеримость эпохи и поля снимка для выбранной границы
			// последовательных номеров.

			// c содержит локальное соединение с подставленными счётчиками ACK и записи.
			c := transportSelfConn(t, &transportSelfMemory{})
			c.peerAcknowledged, c.stats.WrittenBytes = tc.anchor, tc.written
			// observed сохраняет опережение, полученное наблюдателем аномалии.
			var observed uint64
			// observedAt сохраняет время, полученное наблюдателем аномалии.
			var observedAt time.Time
			c.opts.OnACKAnomaly = func(bytes uint64, at time.Time) {
				// Проверяет вызов вне блокировки транспорта и сохраняет размер и время измеренного опережения ACK.

				if !c.mu.TryLock() {
					t.Error("Обработчик вызван под блокировкой транспорта")
					return
				}
				c.mu.Unlock()
				observed, observedAt = bytes, at
			}
			c.acknowledge(tc.raw)
			// s содержит согласованный снимок после обработки выбранного ACK.
			s := c.Snapshot()
			if c.peerAcknowledged != tc.wantAnchor || c.ackEpochUnknown != tc.unknown || s.AckAheadBytesLast != tc.wantAhead || observed != tc.wantAhead || s.AckLastAnomalyAt != observedAt {
				t.Fatalf("якорь=%d неизвестная_эпоха=%v снимок=%+v обработчик=%d/%v", c.peerAcknowledged, c.ackEpochUnknown, s, observed, observedAt)
			}
			// wantEvents задаёт ожидаемое число событий опережения в текущем сценарии.
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

// Проверяет отдельное событие для каждого нового опережающего ACK, отсутствие повторов и обновление
// последнего и максимального опережения после насыщения общего числа событий.
func TestOutboundACKDistinctEventsAndSaturation(t *testing.T) {
	// c содержит соединение для серии разных, повторных и устаревших ACK.
	c := transportSelfConn(t, &transportSelfMemory{})
	c.stats.WrittenBytes = 100
	// raw задаёт очередной новый ACK серии от 101 до 110.
	for raw := uint32(101); raw <= 110; raw++ {
		c.acknowledge(raw)
		c.acknowledge(raw)
		c.acknowledge(raw - 1)
	}
	// s содержит снимок после серии ACK и обновляется после насыщения общего счётчика.
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
	// got содержит снимок после обычного ACK для проверки сохранности последней аномалии.
	if got := c.Snapshot(); got.AckAheadBytesLast != s.AckAheadBytesLast || got.AckAheadBytesMax != s.AckAheadBytesMax || got.AckLastAnomalyAt != s.AckLastAnomalyAt {
		t.Fatalf("обычный ACK сбросил аномалию: %+v", got)
	}
}

// ackDuringWriteConn вводит управляемый ACK внутри физической транспортной записи.
type ackDuringWriteConn struct {
	// transportSelfMemory предоставляет частичную запись с заданной ошибкой и учёт принятых байтов.
	*transportSelfMemory
	// onWrite выполняет тестовую обработку ACK до возврата результата Write.
	onWrite func()
}

// Вызывает заданный обработчик во время физического Write, затем возвращает результат вложенного
// частичного Writer.
func (c ackDuringWriteConn) Write(p []byte) (int, error) {
	c.onWrite()
	return c.transportSelfMemory.Write(p)
}

// Проверяет продвижение ACK во время частичной записи без преждевременного учёта байтов или повторного
// события после окончания Write.
func TestOutboundACKDuringPartialWriteAdvancesWithoutReplay(t *testing.T) {
	// base содержит транспорт с записью двух байтов и ошибкой ErrClosedPipe.
	base := &transportSelfMemory{short: 2, failure: io.ErrClosedPipe}
	// c содержит соединение с известными исходными счётчиками записи и ACK.
	c := transportSelfConn(t, base)
	c.stats.WrittenBytes, c.peerAcknowledged = 100, 100
	c.conn = ackDuringWriteConn{transportSelfMemory: base, onWrite: func() {
		// Во время физической записи принимает ACK и проверяет продвижение опорного значения без публикации
		// ещё не записанных байтов.

		c.acknowledge(104)
		if c.peerAcknowledged != 104 || c.Snapshot().WrittenBytes != 100 {
			t.Error("активная запись потеряла якорь или опубликовала ещё не записанные байты")
		}
	}}
	// callbacks считает вызовы наблюдателя опережающего ACK.
	callbacks := 0
	c.opts.OnACKAnomaly = func(uint64, time.Time) {
		// Считает события опережения для проверки отсутствия повторного учёта ACK после завершения записи.
		callbacks++
	}
	// err содержит результат сериализованной частичной записи с ACK во время Write.
	err := c.withWriter(context.Background(), func() error {
		// Записывает четыре байта под общим сериализатором, позволяя тестовому соединению вернуть частичный
		// успех с ошибкой.
		return c.writeAll([]byte{1, 2, 3, 4})
	})
	if !errors.Is(err, io.ErrClosedPipe) || c.Snapshot().WrittenBytes != 102 || c.writeActive || !base.closed {
		t.Fatalf("частичная запись: %+v active=%v err=%v", c.Snapshot(), c.writeActive, err)
	}
	c.acknowledge(104)
	if c.Snapshot().AckAheadEventsTotal != 0 || callbacks != 0 {
		t.Fatal("ACK активной записи был переигран")
	}
	c.acknowledge(105)
	// s содержит снимок после следующего нового измеримого ACK.
	if s := c.Snapshot(); s.AckAheadEventsTotal != 1 || s.AckAheadBytesLast != 3 || callbacks != 1 {
		t.Fatalf("новое измеримое подтверждение потеряно: %+v", s)
	}
}

// Проверяет необратимую потерю измеримости ACK после разрыва 2³¹ или переполнения WrittenBytes при
// продолжающейся записи.
func TestOutboundACKUnknownEpochPersistsWithoutStoppingWrites(t *testing.T) {
	// overflow выбирает потерю эпохи из-за переполнения либо разрыва в 2³¹ байтов.
	for _, overflow := range []bool{false, true} {
		// c содержит соединение на выбранной границе потери точности ACK.
		c := transportSelfConn(t, &transportSelfMemory{})
		if overflow {
			c.stats.WrittenBytes = math.MaxUint64 - 2
			c.peerAcknowledged = c.stats.WrittenBytes
		} else {
			c.stats.WrittenBytes = (1 << 31) - 2
		}
		// err содержит результат записи, которая впервые делает эпоху ACK неизвестной.
		if err := c.writeAll([]byte{1, 2, 3, 4}); err != nil || !c.ackEpochUnknown || c.writtenOverflow != overflow {
			t.Fatalf("потеря эпохи: overflow=%v unknown=%v err=%v", c.writtenOverflow, c.ackEpochUnknown, err)
		}
		c.acknowledge(uint32(c.stats.WrittenBytes))
		c.acknowledge(uint32(c.stats.WrittenBytes + 1))
		// err содержит результат последующей записи после необратимой потери эпохи ACK.
		if err := c.writeAll([]byte{5}); err != nil || c.Snapshot().AckAheadEventsTotal != 0 || !c.ackEpochUnknown {
			t.Fatalf("потеря эпохи остановила запись или восстановилась: %+v err=%v", c.Snapshot(), err)
		}
	}
}
