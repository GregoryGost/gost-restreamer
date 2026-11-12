package rtmp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/bluenviron/gortmplib/pkg/bytecounter"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// Возвращает явные тестовые бюджеты RTMP и сроки ввода-вывода, достаточные для проверки множества
// фрагментов и AMF.
func testLimits() Limits {
	return Limits{MaxConnections: 4, MaxChunkStreams: 8, MaxMessageBytes: 1 << 20, MaxChunkBytes: 65536, MaxFragments: 20000, MaxRetainedBytes: 32 << 20, MaxControlBytes: 64, MaxConfigBytes: 1 << 16, MaxAMFBytes: 1 << 16, MaxAMFDepth: 12, MaxAMFElements: 1024, MaxReadAheadBytes: 65554, HandshakeTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, ShutdownTimeout: 2 * time.Second}
}

// memoryConn реализует транспорт в памяти с чтением заданных байтов и успешной записью без сохранения.
type memoryConn struct { /* Reader содержит исходный поток байтов для тестового чтения. */
	*bytes.Reader
}

// Имитирует успешное принятие всех записанных байтов без сохранения данных.
func (c memoryConn) Write(p []byte) (int, error) { return len(p), nil }

// Имитирует успешное закрытие соединения в памяти без изменения Reader.
func (c memoryConn) Close() error { return nil }

// Возвращает пустой TCP-адрес для реализации тестового net.Conn.
func (c memoryConn) LocalAddr() net.Addr { return &net.TCPAddr{} }

// Возвращает пустой TCP-адрес удалённой стороны для реализации тестового net.Conn.
func (c memoryConn) RemoteAddr() net.Addr { return &net.TCPAddr{} }

// Принимает общий срок без изменения поведения соединения в памяти.
func (c memoryConn) SetDeadline(time.Time) error { return nil }

// Принимает срок чтения без изменения поведения Reader в памяти.
func (c memoryConn) SetReadDeadline(time.Time) error { return nil }

// Принимает срок записи без изменения тестового Writer.
func (c memoryConn) SetWriteDeadline(time.Time) error { return nil }

// Создаёт RTMP-блок fmt0 с нулевой меткой и MessageStreamID=1 для заданных типа и тела.
func chunk0(id, typ byte, body []byte) []byte {
	// h содержит двенадцатибайтовый заголовок fmt0 с длиной переданного тела.
	h := make([]byte, 12)
	h[0] = id
	h[4] = byte(len(body) >> 16)
	h[5] = byte(len(body) >> 8)
	h[6] = byte(len(body))
	h[7] = typ
	h[8] = 1
	return append(h, body...)
}

// Разбивает тестовое сообщение на fmt0 и продолжения fmt3 заданного размера, сохраняя полную длину в
// первом заголовке.
func fragmented(id, typ byte, body []byte, size int) []byte {
	// first задаёт длину тела первого фрагмента в байтах.
	first := min(size, len(body))
	// out накапливает первый fmt0-блок и последующие продолжения fmt3.
	out := chunk0(id, typ, body[:first])
	out[4] = byte(len(body) >> 16)
	out[5] = byte(len(body) >> 8)
	out[6] = byte(len(body))
	// at задаёт позицию начала очередного фрагмента исходного тела.
	for at := first; at < len(body); at += size {
		out = append(out, 0xc0|id)
		out = append(out, body[at:min(at+size, len(body))]...)
	}
	return out
}

// Создаёт wireConn над байтами в памяти и отключает стадию согласования для проверки блоков RTMP.
func guarded(t *testing.T, data []byte, limits Limits) *wireConn {
	t.Helper()
	// c содержит ограничитель над входом в памяти. err сообщает об отказе создания.
	c, err := newWireConn(memoryConn{bytes.NewReader(data)}, limits)
	if err != nil {
		t.Fatal(err)
	}
	c.handshake = 0
	return c
}

// Проверяет ранний отказ чрезмерного AMF-тела и Enhanced видеометаданных до полного выделения и чтения
// сообщения.
func TestGuardRejectsDeclaredSizeBeforeBodyRead(t *testing.T) {
	// l содержит бюджеты для проверки раннего отказа большого тела.
	l := testLimits()
	// h содержит заголовок с подставляемой чрезмерной объявленной длиной.
	h := chunk0(3, 20, nil)
	// n задаёт объявленную длину на один байт выше MaxAMFBytes.
	n := l.MaxAMFBytes + 1
	h[4] = byte(n >> 16)
	h[5] = byte(n >> 8)
	h[6] = byte(n)
	// c содержит ограничитель для ранней проверки объявленного размера.
	c := guarded(t, h, l)
	// err содержит результат чтения чрезмерного сообщения до ожидаемого отказа.
	_, err := io.ReadAll(c)
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("размер не отклонён до EOF: %v", err)
	}
	if c.retained > 512 {
		t.Fatal("тело выделено до проверки")
	}
	// Тип метаданных выясняется из первого байта до выделения полного блока.
	h = chunk0(6, 9, []byte{0x84})
	h[4] = byte(n >> 16)
	h[5] = byte(n >> 8)
	h[6] = byte(n)
	c = guarded(t, h, l)
	c.chunkSize = n
	_, err = io.ReadAll(c)
	if !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}

// Проверяет ограничение глубины до типизированного декодирования обычных данных, команд и Enhanced
// видеометаданных.
func TestGuardPreflightsEveryAMFDecoderBeforeFinalChunk(t *testing.T) {
	// l задаёт малую допустимую глубину AMF.
	l := testLimits()
	l.MaxAMFDepth = 2
	// deep содержит AMF с вложенностью выше выбранного предела.
	deep := []byte{10, 0, 0, 0, 1, 10, 0, 0, 0, 1, 5}
	// typ выбирает обычные данные, команду либо Enhanced видеометаданные.
	for _, typ := range []byte{18, 20, 9} {
		// body содержит AMF с необходимым префиксом выбранного типа сообщения.
		body := deep
		if typ == 9 {
			body = append([]byte{0x84, 'a', 'v', '0', '1'}, deep...)
		}
		// data содержит SetChunkSize и дроблёное AMF-сообщение.
		data := append(chunk0(2, 1, []byte{0, 0, 0, 3}), fragmented(6, typ, body, 3)...)
		// c содержит ограничитель перед типизированным декодером.
		c := guarded(t, data, l)
		// bcr считает байты ограниченного потока для библиотечного читателя.
		bcr := bytecounter.NewReader(c)
		// reader декодирует типизированные сообщения только после предварительной проверки.
		reader := message.NewReader(bcr, bcr, func(uint32) error {
			// Принимает уведомление о пороге подтверждения без дополнительных действий при проверке ограничений
			// AMF.
			return nil
		})
		// err сообщает об отказе чтения допустимого SetChunkSize.
		if _, err := reader.Read(); err != nil {
			t.Fatal(err)
		}
		// err содержит результат чтения слишком глубокого AMF.
		if _, err := reader.Read(); !errors.Is(err, ErrLimit) {
			t.Fatalf("тип %d не остановлен до typed AMF: %v", typ, err)
		}
	}
}

// Проверяет учёт удержанных ячеек фрагментов после завершения сообщения и постоянство резерва при 3000
// повторениях.
func TestGuardRetainsFragmentSlotsAndReusesBudget(t *testing.T) {
	// l содержит бюджеты для проверки повторного использования ячеек фрагментов.
	l := testLimits()
	// body содержит исходное аудиосообщение из восьми байтов.
	body := []byte{0xaf, 1, 1, 2, 3, 4, 5, 6}
	// data накапливает 3000 одинаковых сообщений с однобайтовыми фрагментами.
	var data []byte
	for range 3000 {
		data = append(data, fragmented(4, 8, body, 1)...)
	}
	// c содержит ограничитель с выбранным однобайтовым размером блока.
	c := guarded(t, data, l)
	c.chunkSize = 1
	for range 8 {
		// err сообщает об отказе чтения одного из первых восьми фрагментов.
		if err := c.readChunk(); err != nil {
			t.Fatal(err)
		}
	}
	// retained сохраняет логический вес ячеек после первого полного сообщения для сравнения с
	// повторами.
	retained := c.retained
	if retained < 8*129 {
		t.Fatal("слоты освобождены после completion")
	}
	for range 2999 * 8 {
		// err сообщает об отказе чтения очередного повторно используемого фрагмента.
		if err := c.readChunk(); err != nil {
			t.Fatal(err)
		}
	}
	if c.retained != retained {
		t.Fatalf("обычный поток навсегда расходует бюджет: %d != %d", c.retained, retained)
	}
}

// Проверяет запрет нулевого размера блока и неизвестного наследования, расширенные метки и повторную
// сборку после Abort.
func TestGuardChunkInheritanceAbortAndExtendedTimestamp(t *testing.T) {
	// set создаёт SetChunkSize с заданным сетевым размером блока.
	set := func(n uint32) []byte {
		// Формирует SetChunkSize с переданным 32-битным размером в порядке от старшего байта к младшему.
		// b содержит четырёхбайтовое тело SetChunkSize.
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, n)
		return chunk0(2, 1, b)
	}
	// err содержит результат чтения недопустимого нулевого размера блока.
	if _, err := io.ReadAll(guarded(t, set(0), testLimits())); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	// id выбирает неподдержанное расширение идентификатора потока блоков.
	for _, id := range []byte{0, 1} {
		// err содержит результат чтения неподдержанного идентификатора потока.
		if _, err := io.ReadAll(guarded(t, []byte{id}, testLimits())); !errors.Is(err, ErrFraming) {
			t.Fatal(err)
		}
	}
	// err содержит результат чтения fmt3 без известного предшествующего заголовка.
	if _, err := io.ReadAll(guarded(t, []byte{0xc4}, testLimits())); !errors.Is(err, ErrFraming) {
		t.Fatal(err)
	}
	// data содержит сообщения с расширенной временной меткой, затем сценарий Abort.
	data := chunk0(4, 8, []byte{0xaf, 1, 1})
	data[1] = 255
	data[2] = 255
	data[3] = 255
	data = append(append(append([]byte(nil), data[:12]...), 0, 255, 255, 255), data[12:]...)
	data = append(data, 0xc4, 0, 255, 255, 255, 0xaf, 1, 2)
	// err содержит результат чтения допустимого повторения расширенной временной метки.
	if _, err := io.ReadAll(guarded(t, data, testLimits())); err != nil {
		t.Fatal(err)
	}
	// Abort очищает текущее тело, но не ссылки в ячейках его внутреннего массива.
	// partial содержит первый фрагмент незавершённого аудиосообщения.
	partial := fragmented(4, 8, []byte{0xaf, 1, 1, 2}, 1)[:13]
	// abort содержит сообщение отмены потока 4 с однобайтовыми фрагментами.
	abort := fragmented(2, 2, []byte{0, 0, 0, 4}, 1)
	data = append(partial, abort...)
	data = append(data, fragmented(4, 8, []byte{0xaf, 1, 9}, 1)...)
	// c содержит ограничитель для проверки новой сборки после Abort.
	c := guarded(t, data, testLimits())
	c.chunkSize = 1
	// err сообщает об отказе чтения сценария отмены и новой сборки.
	if _, err := io.ReadAll(c); err != nil {
		t.Fatal(err)
	}
	if c.streams[4].length != 3 || len(c.streams[4].slots) < 3 {
		t.Fatal("Abort нарушил последующую сборку")
	}
}

// Проверяет независимые пределы числа фрагментов одного сообщения и числа потоков блоков соединения.
func TestGuardLimitsFragmentsAndStreams(t *testing.T) {
	// l содержит ограничения, поочерёдно уменьшаемые для фрагментов и потоков.
	l := testLimits()
	l.MaxFragments = 2
	// c содержит ограничитель сообщения с числом фрагментов выше лимита.
	c := guarded(t, fragmented(4, 8, []byte{0xaf, 1, 1}, 1), l)
	c.chunkSize = 1
	// err содержит результат чтения сообщения сверх MaxFragments.
	if _, err := io.ReadAll(c); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	l = testLimits()
	l.MaxChunkStreams = 1
	// data содержит два сообщения разных потоков при лимите одного потока.
	data := append(chunk0(4, 8, []byte{0xaf, 1, 1}), chunk0(5, 8, []byte{0xaf, 1, 2})...)
	// err содержит результат чтения сверх MaxChunkStreams.
	if _, err := io.ReadAll(guarded(t, data, l)); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}

// Проверяет сохранение наследуемых дельт меток для fmt1/fmt2/fmt3 по ожидаемому ряду 10, 15, 17, 19
// мс.
func TestGuardMirrorsInheritedHeaderTimestamps(t *testing.T) {
	// body содержит одинаковую аудионагрузку для разных форм наследования заголовка.
	body := []byte{0xaf, 1, 7}
	// data накапливает сообщения fmt0/fmt1/fmt2/fmt3 с независимым ожидаемым рядом меток.
	data := chunk0(4, 8, body)
	data[3] = 10
	data = append(data, 0x44, 0, 0, 5, 0, 0, 3, 8)
	data = append(data, body...)
	data = append(data, 0x84, 0, 0, 2)
	data = append(data, body...)
	data = append(data, 0xc4)
	data = append(data, body...)
	// c содержит ограничитель потока с наследуемыми метками.
	c := guarded(t, data, testLimits())
	// bcr считает байты потока для библиотечного декодера.
	bcr := bytecounter.NewReader(c)
	// r декодирует сообщения для проверки фактически унаследованных меток.
	r := message.NewReader(bcr, bcr, func(uint32) error {
		// Принимает уведомление о пороге подтверждения без дополнительных действий при проверке наследования
		// временных меток.
		return nil
	})
	// want задаёт очередную ожидаемую метку в миллисекундах.
	for _, want := range []time.Duration{10, 15, 17, 19} {
		// m содержит очередное декодированное сообщение. err сообщает об отказе чтения.
		m, err := r.Read()
		if err != nil {
			t.Fatal(err)
		}
		// got содержит фактический DTS аудиосообщения.
		if got := m.(*message.Audio).DTS; got != want*time.Millisecond {
			t.Fatalf("наследование timestamp: %v", got)
		}
	}
}

// Проверяет отказ по бюджету удержания до создания первой ячейки фрагмента.
func TestRetainedBudgetRejectsBeforeFragmentAllocation(t *testing.T) {
	// l задаёт резерв, которого не хватает для первой ячейки фрагмента.
	l := testLimits()
	l.MaxRetainedBytes = l.baseRetained() + 512 + 128
	// c содержит ограничитель для проверки отказа до создания ячейки.
	c := guarded(t, fragmented(4, 8, []byte{0xaf, 1, 1}, 1), l)
	c.chunkSize = 1
	// err содержит результат чтения при недостаточном резерве первой ячейки.
	if _, err := io.ReadAll(c); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if len(c.streams[4].slots) != 0 {
		t.Fatal("слот создан до проверки retained budget")
	}
}

// FuzzGuardAMF подаёт входы до 64 KiB в предварительную проверку AMF, допуская штатный отказ разбора.
func FuzzGuardAMF(f *testing.F) {
	f.Add([]byte{5})
	f.Add([]byte{10, 0, 0, 0, 1, 5})
	f.Fuzz(func(t *testing.T, b []byte) {
		// Передаёт произвольные данные до 65536 байтов в предварительную проверку AMF с тестовыми
		// ограничениями.

		if len(b) > 1<<16 {
			t.Skip()
		}
		_ = preflightAMF(b, testLimits())
	})
}

// FuzzGuardFraming проверяет ограниченное чтение произвольного обрамления RTMP до 64 KiB без стадии
// согласования.
func FuzzGuardFraming(f *testing.F) {
	f.Add(chunk0(4, 8, []byte{0xaf, 1, 1}))
	f.Add([]byte{0xc4})
	f.Fuzz(func(t *testing.T, b []byte) {
		// Читает произвольное обрамление до 65536 байтов через ограничивающее соединение без стадии
		// согласования.

		if len(b) > 1<<16 {
			t.Skip()
		}
		// c содержит ограничитель произвольного фаззинг-входа. err сообщает об отказе его
		// создания.
		c, err := newWireConn(memoryConn{bytes.NewReader(b)}, testLimits())
		if err != nil {
			t.Fatal(err)
		}
		c.handshake = 0
		_, _ = io.Copy(io.Discard, c)
	})
}
