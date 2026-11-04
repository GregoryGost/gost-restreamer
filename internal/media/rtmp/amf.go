package rtmp

import "encoding/binary"

// preflightAMF не создаёт граф объектов до проверки глубины и числа элементов.
// Допустимые маркеры совпадают с декодером amf0 закреплённой gortmplib.
func preflightAMF(body []byte, limits Limits) error {
	p := amfPreflight{body: body, limits: limits}
	for p.pos < len(body) {
		if err := p.value(1); err != nil {
			return err
		}
	}
	return nil
}

type amfPreflight struct {
	body     []byte
	pos      int
	elements uint32
	limits   Limits
}

func (p *amfPreflight) take(n uint32) ([]byte, error) {
	if uint64(n) > uint64(len(p.body)-p.pos) {
		return nil, ErrFraming
	}
	b := p.body[p.pos : p.pos+int(n)]
	p.pos += int(n)
	return b, nil
}

func (p *amfPreflight) value(depth uint32) error {
	if depth > p.limits.MaxAMFDepth || p.elements >= p.limits.MaxAMFElements {
		return ErrLimit
	}
	p.elements++
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
			n, e := p.take(2)
			if e != nil {
				return e
			}
			size := uint32(binary.BigEndian.Uint16(n))
			if size == 0 {
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
		n, e := p.take(4)
		if e != nil {
			return e
		}
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
