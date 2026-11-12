package rtmp

import "encoding/binary"

// preflightAMF не создаёт граф объектов до проверки глубины и числа элементов.
// Допустимые маркеры совпадают с декодером amf0 закреплённой gortmplib.
func preflightAMF(body []byte, limits Limits) error {
	// p проверяет тело AMF по байтам без построения графа значений.
	p := amfPreflight{body: body, limits: limits}
	for p.pos < len(body) {
		// err сообщает о нарушении структуры, глубины или числа элементов очередного значения.
		if err := p.value(1); err != nil {
			return err
		}
	}
	return nil
}

// amfPreflight хранит позицию и бюджеты предварительного разбора AMF0 без выделения графа объектов.
type amfPreflight struct {
	// body содержит исходные сериализованные байты AMF0.
	body []byte
	// pos задаёт позицию следующего непрочитанного байта тела.
	pos int
	// elements считает уже проверенные значения AMF во всём теле.
	elements uint32
	// limits задаёт предельные глубину и число значений AMF.
	limits Limits
}

// Возвращает следующие n байтов AMF без копирования и продвигает позицию; усечение возвращает
// ErrFraming.
func (p *amfPreflight) take(n uint32) ([]byte, error) {
	if uint64(n) > uint64(len(p.body)-p.pos) {
		return nil, ErrFraming
	}
	// b ссылается на очередной проверенный по длине участок исходного тела.
	b := p.body[p.pos : p.pos+int(n)]
	p.pos += int(n)
	return b, nil
}

// Проверяет одно поддержанное AMF0-значение рекурсивно, ограничивая глубину и общее число элементов до
// построения графа библиотекой.
func (p *amfPreflight) value(depth uint32) error {
	if depth > p.limits.MaxAMFDepth || p.elements >= p.limits.MaxAMFElements {
		return ErrLimit
	}
	p.elements++
	// b содержит байт маркера текущего значения. err сообщает об усечении маркера.
	b, err := p.take(1)
	if err != nil {
		return err
	}
	switch b[0] {
	case 0:
		_, err = p.take(8)
	case 1:
		_, err = p.take(1)
	case 2:
		// n принимает двухбайтовое поле длины строки.
		var n []byte
		n, err = p.take(2)
		if err == nil {
			_, err = p.take(uint32(binary.BigEndian.Uint16(n)))
		}
	case 5, 6:
	case 3, 8:
		if b[0] == 8 {
			if _, err = p.take(4); err != nil {
				return err
			}
		}
		for {
			// n содержит двухбайтовую длину имени свойства. e сообщает об усечении этого
			// поля.
			n, e := p.take(2)
			if e != nil {
				return e
			}
			// size задаёт длину имени свойства в байтах; ноль начинает маркер конца объекта.
			size := uint32(binary.BigEndian.Uint16(n))
			if size == 0 {
				// end содержит маркер завершения объекта. e сообщает об усечении этого
				// маркера.
				end, e := p.take(1)
				if e != nil {
					return e
				}
				if end[0] != 9 {
					return ErrFraming
				}
				return nil
			}
			if _, err = p.take(size); err != nil {
				return err
			}
			if err = p.value(depth + 1); err != nil {
				return err
			}
		}
	case 10:
		// n содержит четырёхбайтовое поле числа элементов строгого массива. e сообщает об
		// усечении поля.
		n, e := p.take(4)
		if e != nil {
			return e
		}
		// count задаёт число значений строгого массива для проверки остатка общего бюджета.
		count := binary.BigEndian.Uint32(n)
		if count > p.limits.MaxAMFElements-p.elements {
			return ErrLimit
		}
		for range count {
			if err = p.value(depth + 1); err != nil {
				return err
			}
		}
	default:
		return ErrFraming
	}
	return err
}
