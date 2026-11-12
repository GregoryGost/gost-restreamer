package fmp4

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// be задаёт сетевой порядок байтов для чтения и записи целых полей MP4.
var be = binary.BigEndian

// Кодирует uint32 четырьмя байтами в сетевом порядке.
func u32(v uint32) []byte { /* b содержит четыре выходных байта кодируемого uint32. */
	b := make([]byte, 4)
	be.PutUint32(b, v)
	return b
}

// Кодирует uint64 восемью байтами в сетевом порядке.
func u64(v uint64) []byte { /* b содержит восемь выходных байтов кодируемого uint64. */
	b := make([]byte, 8)
	be.PutUint64(b, v)
	return b
}

// Собирает части в новый буфер без изменения исходных срезов.
func join(parts ...[]byte) []byte {
	// n накапливает суммарную длину частей для однократного выделения выходного буфера.
	n := 0
	// p обозначает очередную часть, длина которой добавляется к требуемой ёмкости.
	for _, p := range parts {
		n += len(p)
	}
	// b хранит собственный выходной буфер с заранее рассчитанной ёмкостью.
	b := make([]byte, 0, n)
	// p обозначает очередную часть, байты которой копируются в общий выходной буфер.
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

// Собирает блок MP4 с восьмибайтовым заголовком и 32-битным размером; четырёхбайтовый kind и
// представимость полного размера обеспечивает вызывающий.
func box(kind string, parts ...[]byte) []byte {
	// data содержит объединённое тело блока без заголовка размера и типа.
	data := join(parts...)
	return join(u32(uint32(len(data)+8)), []byte(kind), data)
}

// Собирает полный блок MP4 с байтом версии и младшими 24 битами флагов.
func full(kind string, version byte, flags uint32, parts ...[]byte) []byte {
	return box(kind, append([]byte{version}, u32(flags)[1:]...), join(parts...))
}

// Записывает весь буфер, продолжая частичные записи; отсутствие продвижения или недопустимый результат
// Write возвращают io.ErrShortWrite.
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		// n сообщает число принятых Writer байтов; err сохраняет отказ записи, в том числе после
		// частичного продвижения.
		n, err := w.Write(b)
		if n < 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// rawBox хранит разобранный тип, объявленный размер и тело одного блока MP4 без декодирования его
// содержимого.
type rawBox struct {
	// kind содержит четырёхбайтовый код типа разобранного блока.
	kind string
	// data содержит тело без заголовка; children возвращает срез входного буфера, а readBody
	// выделяет для тела отдельную память.
	data []byte
	// size задаёт объявленный полный размер блока в байтах; header хранит длину обычного либо
	// расширенного заголовка.
	size, header uint64
}

// Читает обычный или расширенный заголовок MP4; нулевой размер и размер меньше заголовка отклоняются.
func readHeader(r io.Reader) (rawBox, error) {
	// h принимает первые восемь байтов с 32-битным размером и кодом типа блока.
	var h [8]byte
	// err сообщает об усечении либо отказе чтения обычного заголовка блока.
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return rawBox{}, err
	}
	// b накапливает описание прочитанного заголовка до проверки объявленного размера.
	b := rawBox{kind: string(h[4:]), size: uint64(be.Uint32(h[:4])), header: 8}
	if b.size == 1 {
		// ext принимает восемь байтов расширенного размера для блока с начальным размером 1.
		var ext [8]byte
		// err сообщает об усечении либо отказе чтения расширенного размера блока.
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return rawBox{}, err
		}
		b.size = be.Uint64(ext[:])
		b.header = 16
	}
	if b.size < b.header {
		return rawBox{}, fmt.Errorf("%w: размер box %s", ErrInvalid, b.kind)
	}
	return b, nil
}

// Проверяет полный размер блока по limit и MaxInt32 до выделения буфера и читает тело целиком.
func readBody(r io.Reader, b rawBox, limit uint64) (rawBox, error) {
	if b.size > limit || b.size > math.MaxInt32 {
		return rawBox{}, fmt.Errorf("%w: box %s (%d)", ErrLimit, b.kind, b.size)
	}
	b.data = make([]byte, int(b.size-b.header))
	// err содержит результат чтения всего тела после проверки размера и выделения буфера.
	_, err := io.ReadFull(r, b.data)
	return b, err
}

// Разбирает последовательные дочерние блоки MP4 с обычными или расширенными размерами; их данные
// ссылаются на исходный буфер.
func children(data []byte) ([]rawBox, error) {
	// result накапливает дочерние блоки в исходном порядке; их тела остаются срезами входных данных.
	var result []rawBox
	for len(data) > 0 {
		if len(data) < 8 {
			return nil, ErrInvalid
		}
		// size хранит объявленный размер очередного дочернего блока, включая его заголовок.
		size := uint64(be.Uint32(data[:4]))
		// header задаёт длину заголовка очередного блока: восемь байтов для обычного размера
		// либо шестнадцать для расширенного.
		header := uint64(8)
		if size == 1 {
			if len(data) < 16 {
				return nil, ErrInvalid
			}
			size = be.Uint64(data[8:16])
			header = 16
		}
		if size < header || size > uint64(len(data)) {
			return nil, ErrInvalid
		}
		result = append(result, rawBox{kind: string(data[4:8]), data: data[header:size], size: size, header: header})
		data = data[size:]
	}
	return result, nil
}

// Находит единственный дочерний блок заданного типа; отсутствие, повтор и некорректное обрамление
// возвращают ErrInvalid.
func find(data []byte, kind string) (rawBox, error) {
	// boxes содержит разобранные дочерние блоки; err сообщает о нарушении их обрамления.
	boxes, err := children(data)
	if err != nil {
		return rawBox{}, err
	}
	// result сохраняет единственный найденный блок заданного типа до завершения проверки дубликатов.
	var result rawBox
	// found отмечает уже найденный блок нужного типа, чтобы отклонить второй такой блок.
	found := false
	// b обозначает очередной дочерний блок, тип которого сравнивается с искомым.
	for _, b := range boxes {
		if b.kind == kind {
			if found {
				return rawBox{}, fmt.Errorf("%w: повторный %s", ErrInvalid, kind)
			}
			result, found = b, true
		}
	}
	if found {
		return result, nil
	}
	return rawBox{}, fmt.Errorf("%w: отсутствует %s", ErrInvalid, kind)
}

// Извлекает младшие 24 бита флагов полного блока, требуя наличие четырёхбайтового префикса.
func flags(data []byte) (uint32, error) {
	if len(data) < 4 {
		return 0, ErrInvalid
	}
	return be.Uint32(data[:4]) & 0xffffff, nil
}

// cursor последовательно отдаёт поля из тела блока и сохраняет первую обнаруженную нехватку данных.
type cursor struct {
	// b содержит ещё не прочитанную часть исходного тела блока.
	b []byte
	// err сохраняет ошибку разбора, после которой последующие чтения возвращают нулевые байты.
	err error
}

// Забирает очередные n байтов курсора; вызывающий передаёт неотрицательное n. При недостающих данных
// или прежней ошибке сохраняет ErrInvalid и возвращает нулевой буфер для последующего чтения чисел.
func (c *cursor) take(n int) []byte {
	if c.err != nil || n < 0 || n > len(c.b) {
		c.err = ErrInvalid
		return make([]byte, n)
	}
	// v ссылается на следующие n байтов исходного буфера; курсор продвигается за эту область без её
	// копирования.
	v := c.b[:n]
	c.b = c.b[n:]
	return v
}

// Читает следующие четыре байта курсора как uint32 в сетевом порядке; усечение фиксируется в c.err.
func (c *cursor) u32() uint32 { return be.Uint32(c.take(4)) }

// Читает следующие восемь байтов курсора как uint64 в сетевом порядке; усечение фиксируется в c.err.
func (c *cursor) u64() uint64 { return be.Uint64(c.take(8)) }
