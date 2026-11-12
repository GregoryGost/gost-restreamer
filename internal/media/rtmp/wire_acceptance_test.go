package rtmp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// Задаёт небольшие независимые бюджеты RTMP для проверок раннего отказа и повторного использования
// фрагментов.
func acceptanceWireLimits() Limits {
	return Limits{MaxConnections: 4, MaxChunkStreams: 8, MaxMessageBytes: 4096, MaxChunkBytes: 256, MaxFragments: 128, MaxRetainedBytes: 1 << 20, MaxControlBytes: 64, MaxConfigBytes: 1024, MaxAMFBytes: 1024, MaxAMFDepth: 4, MaxAMFElements: 32, MaxReadAheadBytes: 512, HandshakeTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, ShutdownTimeout: time.Second}
}

// acceptanceWireConn читает из памяти и отдельно считает байты, потреблённые до раннего отказа.
type acceptanceWireConn struct {
	// Reader содержит исходный поток тестовых RTMP-байтов.
	*bytes.Reader
	// readBytes считает фактически прочитанные байты исходного потока.
	readBytes int
}

// Читает из памяти и считает реально полученные байты для проверки границы раннего отказа.
func (c *acceptanceWireConn) Read(b []byte) (int, error) {
	// n содержит фактически прочитанное число байтов. err сообщает об окончании или отказе Reader.
	n, err := c.Reader.Read(b)
	c.readBytes += n
	return n, err
}

// Имитирует успешную запись всего буфера без сохранения байтов.
func (*acceptanceWireConn) Write(b []byte) (int, error) { return len(b), nil }

// Имитирует успешное закрытие тестового соединения в памяти.
func (*acceptanceWireConn) Close() error { return nil }

// Возвращает nil вместо локального адреса соединения в памяти.
func (*acceptanceWireConn) LocalAddr() net.Addr { return nil }

// Возвращает nil вместо удалённого адреса соединения в памяти.
func (*acceptanceWireConn) RemoteAddr() net.Addr { return nil }

// Принимает общий срок без изменения чтения из памяти.
func (*acceptanceWireConn) SetDeadline(time.Time) error { return nil }

// Принимает срок чтения без ограничения Reader в памяти.
func (*acceptanceWireConn) SetReadDeadline(time.Time) error { return nil }

// Принимает срок записи без изменения имитации успешной записи.
func (*acceptanceWireConn) SetWriteDeadline(time.Time) error { return nil }

// Создаёт независимый fmt0-блок RTMP с MessageStreamID=1 и полным размером переданного тела.
func acceptanceChunk0(id, typ byte, body []byte) []byte {
	// header содержит независимый двенадцатибайтовый fmt0-заголовок заданного тела.
	header := []byte{id, 0, 0, 0, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body)), typ, 1, 0, 0, 0}
	return append(header, body...)
}

// Создаёт ограничитель и счётчик чтения над переданными байтами, пропуская согласование RTMP.
func acceptanceWire(t *testing.T, data []byte, limits Limits) (*wireConn, *acceptanceWireConn) {
	t.Helper()
	// conn содержит транспорт в памяти со счётчиком реального чтения.
	conn := &acceptanceWireConn{Reader: bytes.NewReader(data)}
	// w содержит ограничитель перед библиотекой. err сообщает об отказе его создания.
	w, err := newWireConn(conn, limits)
	if err != nil {
		t.Fatal(err)
	}
	w.handshake = 0
	return w, conn
}

// Проверяет, что чрезмерный размер медийного, AMF- или управляющего сообщения отклоняется после 12
// байтов заголовка без выдачи данных библиотеке.
func TestAcceptanceWireOversizeStopsBeforeReadingBody(t *testing.T) {
	// limits содержит малые бюджеты для проверки чрезмерных объявленных размеров.
	limits := acceptanceWireLimits()
	// tc содержит тип и чрезмерную длину проверяемого сообщения.
	for _, tc := range []struct {
		// name обозначает превышаемый бюджет в имени подтеста.
		name string
		// typ задаёт тип сообщения, определяющий применимый предел.
		typ byte
		// length задаёт объявленную длину на один байт выше применимого предела.
		length uint32
	}{
		{"message", 9, limits.MaxMessageBytes + 1},
		{"command", 20, limits.MaxAMFBytes + 1},
		{"data", 18, limits.MaxAMFBytes + 1},
		{"control", 4, limits.MaxControlBytes + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет отказ по выбранному объявленному размеру после чтения только 12-байтового заголовка и до
			// передачи байтов декодеру.

			// wire содержит заголовок с чрезмерной объявленной длиной без полного тела.
			wire := acceptanceChunk0(3, tc.typ, nil)
			wire[4], wire[5], wire[6] = byte(tc.length>>16), byte(tc.length>>8), byte(tc.length)
			// w содержит ограничитель. c считает прочитанные байты заголовка.
			w, c := acceptanceWire(t, wire, limits)
			// got содержит выданные байты. err содержит результат проверки объявленного
			// размера.
			got, err := io.ReadAll(w)
			if !errors.Is(err, ErrLimit) || len(got) != 0 || c.readBytes != 12 {
				t.Fatalf("отказ не предшествовал body: bytes=%d exposed=%d err=%v", c.readBytes, len(got), err)
			}
		})
	}
}

// Проверяет отказ вложенности AMF сверх лимита для команд, обычных данных и Enhanced видеометаданных
// до выдачи последнего фрагмента.
func TestAcceptanceWireAMFPreflightCoversEveryTypedAMFPath(t *testing.T) {
	// Пять вложенных массивов AMF0 Strict Array превышают MaxAMFDepth=4 до декодирования библиотекой.
	// nested содержит AMF null, последовательно оборачиваемый пятью строгими массивами.
	nested := []byte{5}
	for range 5 {
		nested = append([]byte{10, 0, 0, 0, 1}, nested...)
	}
	// tc выбирает один из трёх путей типизированного AMF-декодирования.
	for _, tc := range []struct {
		// name обозначает путь декодирования в имени подтеста.
		name string
		// typ задаёт тип RTMP-сообщения с AMF.
		typ byte
		// prefix содержит префикс Enhanced AV1 Metadata либо отсутствует для обычного AMF.
		prefix []byte
	}{
		{"command", 20, nil}, {"data", 18, nil}, {"video-metadata", 9, []byte{0x84, 'a', 'v', '0', '1'}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет предел вложенности AMF до передачи последнего фрагмента выбранного типа сообщения
			// декодеру.

			// body содержит чрезмерно вложенный граф с нужным префиксом.
			body := append(bytes.Clone(tc.prefix), nested...)
			// w содержит ограничитель выбранной формы AMF.
			w, _ := acceptanceWire(t, acceptanceChunk0(3, tc.typ, body), acceptanceWireLimits())
			// got содержит выданные байты. err содержит результат проверки глубины до выдачи
			// последнего блока.
			got, err := io.ReadAll(w)
			if !errors.Is(err, ErrLimit) || len(got) != 0 {
				t.Fatalf("опасный последний фрагмент передан декодеру: exposed=%d err=%v", len(got), err)
			}
		})
	}
}

// Проверяет побайтные фрагменты и 300 повторов без изменения потока или роста учёта удержания сверх
// установленной границы.
func TestAcceptanceWireLegalTinyChunksAndRetainedSlots(t *testing.T) {
	// limits задаёт бюджеты для 300 повторов допустимого сообщения с мелкими блоками.
	limits := acceptanceWireLimits()
	limits.MaxFragments = 8
	// size содержит сетевое значение ChunkSize=1.
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], 1)
	// data накапливает SetChunkSize и 300 сообщений с однобайтовыми продолжениями.
	data := acceptanceChunk0(2, 1, size[:])
	// first содержит семибайтовое тело Enhanced AV1 CodedFrames.
	first := []byte{0x91, 'a', 'v', '0', '1', 0x32, 0}
	for range 300 {
		data = append(data, acceptanceChunk0(3, 9, first[:1])...)
		// Длина сообщения, в отличие от первого фрагмента, включает все семь байтов.
		data[len(data)-9], data[len(data)-8], data[len(data)-7] = 0, 0, byte(len(first))
		// b содержит очередной байт тела после первого фрагмента для отдельного продолжения
		// fmt3.
		for _, b := range first[1:] {
			data = append(data, 0xc3, b)
		}
	}
	// w содержит ограничитель всей повторяемой последовательности.
	w, _ := acceptanceWire(t, data, limits)
	// got содержит пропущенные RTMP-байты. err сообщает об отказе допустимого мелкого дробления.
	got, err := io.ReadAll(w)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("легальный размер фрагмента 1 или повторное использование слотов отвергнуты: %v", err)
	}
	if w.retained > 8192 {
		t.Fatalf("учёт удержанных слотов растёт с общим трафиком: %d", w.retained)
	}
}

// Проверяет отказ третьего фрагмента при лимите два до чтения его полезного байта.
func TestAcceptanceWireFragmentLimitPrecedesNextPayload(t *testing.T) {
	// limits ограничивает сообщение двумя фрагментами.
	limits := acceptanceWireLimits()
	limits.MaxFragments = 2
	// data содержит SetChunkSize=1 и незавершённое сообщение с третьим фрагментом.
	data := acceptanceChunk0(2, 1, []byte{0, 0, 0, 1})
	// fragment содержит первый однобайтовый фрагмент с объявленной длиной четыре байта.
	fragment := acceptanceChunk0(3, 9, []byte{0x91})
	fragment[6] = 4
	data = append(data, fragment...)
	data = append(data, 0xc3, 'a', 0xc3, 'v')
	// w содержит ограничитель. c считает чтение до превышения MaxFragments.
	w, c := acceptanceWire(t, data, limits)
	// err содержит результат отказа до полезного байта третьего фрагмента.
	_, err := io.ReadAll(w)
	if !errors.Is(err, ErrLimit) || c.readBytes != len(data)-1 {
		t.Fatalf("третий фрагмент прочитан после исчерпания бюджета: read=%d size=%d err=%v", c.readBytes, len(data), err)
	}
}

// Проверяет допустимость FrameType 1/2 для AV1 CodedFrames независимо от младшего поля PacketType.
func TestAcceptanceWireAV1FrameTypeIsIndependentOfPacketType(t *testing.T) {
	// flag содержит очередное сочетание FrameType и PacketType в первом байте AV1-сообщения.
	for _, flag := range []byte{0x81, 0xb1, 0x91, 0xa1} {
		// w содержит ограничитель сообщения с выбранным FrameType.
		w, _ := acceptanceWire(t, acceptanceChunk0(3, 9, []byte{flag, 'a', 'v', '0', '1', 0x32, 0}), acceptanceWireLimits())
		// err содержит результат проверки допустимости FrameType независимо от PacketType.
		_, err := io.ReadAll(w)
		if flag == 0x91 || flag == 0xa1 {
			if err != nil {
				t.Errorf("допустимый FrameType %x отвергнут: %v", flag, err)
			}
		} else if !errors.Is(err, ErrCodec) {
			t.Errorf("недопустимый FrameType %x принят: %v", flag, err)
		}
	}
}
