package rtmp

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

// wireStream хранит наследуемый заголовок и логическое удержание фрагментов одного входящего потока
// блоков.
type wireStream struct {
	// known показывает наличие предыдущего заголовка для наследования.
	known bool
	// typ сохраняет тип сообщения текущего потока блоков.
	typ byte
	// length хранит объявленную длину тела в байтах. received считает прочитанные байты текущего
	// тела. fragments считает прочитанные блоки текущего сообщения.
	length, received, fragments uint32
	// extended требует расширенную четырёхбайтовую временную метку.
	extended bool
	// extendedValue сохраняет расширенную метку для проверки продолжений fmt3.
	extendedValue uint32
	// slots учитывает длины удерживаемых библиотекой ячеек фрагментов, включая хвост после
	// завершения или Abort.
	slots []uint32
	// capture при необходимости хранит полное тело для предварительной проверки AMF или сохранения
	// конфигурации.
	capture []byte
	// prefix накапливает первые пять байтов тела для определения кодека и формы сообщения.
	prefix [5]byte
	// prefixN содержит число уже собранных байтов prefix.
	prefixN int
	// collect показывает, копируется ли тело сообщения в capture.
	collect bool
	// amfOffset задаёт начало AMF внутри capture; отрицательное значение означает отсутствие AMF.
	amfOffset int
}

// wireConn передаёт библиотеке не более одного проверенного блока RTMP за чтение.
// Последний блок AMF не выдаётся до предварительной проверки всего ограниченного тела.
type wireConn struct {
	// Conn содержит принятое транспортное соединение, байты которого ограничиваются перед передачей
	// библиотеке.
	net.Conn
	// limits хранит проверенные бюджеты одного входящего соединения.
	limits Limits
	// handshake содержит число ещё не переданных байтов начального согласования RTMP.
	handshake int
	// streams хранит состояния поддержанных идентификаторов потоков блоков от 2 до 63.
	streams [64]*wireStream
	// count считает созданные состояния потоков блоков.
	count uint32
	// chunkSize задаёт текущий размер входящего фрагмента тела в байтах.
	chunkSize uint32
	// pending удерживает остаток одного уже проверенного блока, ещё не переданный библиотеке.
	pending []byte
	// retained считает дополнительное логическое удержание состояний, фрагментов и capture сверх
	// base.
	retained uint64
	// base содержит постоянный логический резерв буферов и копий, вычисленный по лимитам.
	base uint64
	// lastConfig ссылается на последнее полностью собранное сообщение конфигурации аудио или видео.
	lastConfig []byte
	// startupDeadline ограничивает весь этап начального согласования и допуска; после запуска
	// обнуляется.
	startupDeadline time.Time
}

// Создаёт ограничитель принятого соединения после проверки настроек и базового резерва; первые 3073
// байта относятся к согласованию RTMP.
func newWireConn(conn net.Conn, limits Limits) (*wireConn, error) {
	// err сообщает о некорректных ограничениях соединения.
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	// Резерв включает четыре последних блока библиотеки, временную сборку, Packet
	// и копии конфигураций. Отдельно учитываются удерживаемые фрагменты и capture.
	// base содержит постоянный логический резерв перед созданием ограничителя.
	base := limits.baseRetained()
	if base > limits.MaxRetainedBytes {
		return nil, ErrLimit
	}
	return &wireConn{Conn: conn, limits: limits, handshake: 3073, chunkSize: 128, base: base}, nil
}

// Write ставит срок записи с учётом общего срока запуска и передаёт одну запись вложенному соединению.
func (c *wireConn) Write(p []byte) (int, error) {
	// err сообщает об отказе установки срока очередной записи.
	if err := c.SetWriteDeadline(c.deadline(c.limits.WriteTimeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

// Read выдаёт байты согласования, затем не более одного проверенного блока RTMP; последний блок AMF
// становится доступен только после проверки всего тела.
func (c *wireConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.handshake > 0 {
		// n задаёт размер разрешённой части очередного чтения согласования, затем фактически
		// прочитанное число байтов.
		n := min(len(p), c.handshake)
		// err сообщает об отказе чтения части согласования RTMP.
		n, err := c.Conn.Read(p[:n])
		if c.handshake == 3073 && n > 0 && p[0] != 3 {
			return 0, ErrFraming
		}
		c.handshake -= n
		return n, err
	}
	if len(c.pending) == 0 {
		// err сообщает об отказе установки срока чтения следующего блока.
		if err := c.SetReadDeadline(c.deadline(c.limits.ReadTimeout)); err != nil {
			return 0, err
		}
		// err сообщает о повреждении блока или превышении его бюджета до передачи библиотеке.
		if err := c.readChunk(); err != nil {
			return 0, err
		}
	}
	// n содержит число байтов проверенного блока, скопированных в буфер библиотечного читателя.
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	if len(c.pending) == 0 {
		c.pending = nil
	}
	return n, nil
}

// Выбирает более ранний срок между текущим временем с добавленным timeout и общим сроком запуска
// соединения.
func (c *wireConn) deadline(timeout time.Duration) time.Time {
	// d содержит срок операции до сравнения с общим сроком запуска.
	d := time.Now().Add(timeout)
	if !c.startupDeadline.IsZero() && c.startupDeadline.Before(d) {
		return c.startupDeadline
	}
	return d
}

// Читает беззнаковое трёхбайтовое число RTMP в сетевом порядке; длину среза обеспечивает вызывающий.
func be24(p []byte) uint32 { return uint32(p[0])<<16 | uint32(p[1])<<8 | uint32(p[2]) }

// Резервирует дополнительное логическое удержание сверх базового резерва, отклоняя превышение общего
// бюджета до выделения данных.
func (c *wireConn) reserve(n uint64) error {
	if n > c.limits.MaxRetainedBytes-c.base-c.retained {
		return ErrLimit
	}
	c.retained += n
	return nil
}

// Проверяет заголовок, наследование, расширенную метку и бюджеты очередного блока до его передачи
// библиотеке. Удержанные ячейки фрагментов продолжают учитываться после завершения и Abort; AMF
// проверяется до выдачи последнего блока.
func (c *wireConn) readChunk() error {
	// header служит временным буфером заголовка очередного блока.
	var header [18]byte
	// err сообщает об отказе чтения первого байта блока.
	if _, err := io.ReadFull(c.Conn, header[:1]); err != nil {
		return err
	}
	// fmtID содержит формат наследования заголовка. id содержит поддержанный короткий идентификатор
	// потока блоков.
	fmtID, id := header[0]>>6, header[0]&63
	if id < 2 {
		return ErrFraming
	}
	// s содержит состояние текущего потока блоков.
	s := c.streams[id]
	if s == nil {
		if c.count >= c.limits.MaxChunkStreams {
			return ErrLimit
		}
		// err сообщает о невозможности зарезервировать структурный слот нового потока.
		if err := c.reserve(512); err != nil {
			return err
		}
		s = &wireStream{}
		c.streams[id] = s
		c.count++
	}
	if (fmtID != 0 && !s.known) || (fmtID != 3 && s.received != 0) {
		return ErrFraming
	}
	// hn задаёт длину заголовка блока, включая расширенную метку при её наличии.
	hn := 1
	// start показывает, начинается ли новое сообщение этого потока.
	start := s.received == 0
	switch fmtID {
	case 0:
		hn = 12
	case 1:
		hn = 8
	case 2:
		hn = 4
	}
	// err сообщает об отказе чтения оставшихся полей заголовка.
	if _, err := io.ReadFull(c.Conn, header[1:hn]); err != nil {
		return err
	}
	if fmtID <= 1 {
		s.length = be24(header[4:7])
		s.typ = header[7]
	}
	if fmtID <= 2 {
		s.extended = be24(header[1:4]) == 0xffffff
	}
	if s.extended {
		// err сообщает об отказе чтения расширенной временной метки.
		if _, err := io.ReadFull(c.Conn, header[hn:hn+4]); err != nil {
			return err
		}
		// v содержит прочитанную расширенную метку для проверки повторения в продолжениях.
		v := binary.BigEndian.Uint32(header[hn : hn+4])
		hn += 4
		if fmtID == 3 && v != s.extendedValue {
			return ErrFraming
		}
		s.extendedValue = v
	}
	s.known = true
	if s.length > c.limits.MaxMessageBytes {
		return ErrLimit
	}
	if s.typ <= 6 && s.length > c.limits.MaxControlBytes {
		return ErrLimit
	}
	if (s.typ == 18 || s.typ == 20) && s.length > c.limits.MaxAMFBytes {
		return ErrLimit
	}
	if start {
		s.fragments = 0
		s.prefixN = 0
		s.collect = false
		s.amfOffset = -1
		if s.capture != nil {
			c.retained -= uint64(cap(s.capture))
			s.capture = nil
		}
		if s.typ <= 6 || s.typ == 18 || s.typ == 20 {
			// err сообщает об отказе резервирования полного тела управления или AMF.
			if err := c.beginCapture(s); err != nil {
				return err
			}
			if s.typ == 18 || s.typ == 20 {
				s.amfOffset = 0
			}
		}
	}
	// amount задаёт число байтов тела, читаемых в текущем блоке.
	amount := min(s.length-s.received, c.chunkSize)
	if uint64(amount)+uint64(hn) > uint64(c.limits.MaxReadAheadBytes) {
		return ErrLimit
	}
	if s.fragments >= c.limits.MaxFragments {
		return ErrLimit
	}
	// gortmplib сохраняет ячейки массива после завершения и Abort. Перезапись
	// освобождает лишь соответствующую старую ссылку; хвост остаётся учтённым.
	// fragmented показывает, удерживает ли библиотека отдельные ячейки фрагментов данного сообщения.
	fragmented := s.length > c.chunkSize || s.received != 0
	if fragmented {
		// i задаёт номер ячейки текущего фрагмента.
		i := int(s.fragments)
		if i < len(s.slots) {
			// old содержит ранее учтённую длину повторно используемой ячейки.
			old := s.slots[i]
			if amount > old {
				// err сообщает об отказе увеличения резерва повторно используемой
				// ячейки.
				if err := c.reserve(uint64(amount - old)); err != nil {
					return err
				}
			} else {
				c.retained -= uint64(old - amount)
			}
			s.slots[i] = amount
		} else {
			// 128 байт покрывают рост массива срезов библиотеки и []uint32 ограничителя.
			// err сообщает об отказе резерва новой ячейки фрагмента и её структурного места.
			if err := c.reserve(uint64(amount) + 128); err != nil {
				return err
			}
			s.slots = append(s.slots, amount)
		}
	}
	// initial служит временным буфером начальных байтов тела для раннего определения кодека.
	var initial [5]byte
	// initialN задаёт число уже прочитанных начальных байтов текущего блока.
	initialN := min(int(amount), 1)
	if s.prefixN >= 5 {
		initialN = 0
	}
	// err сообщает об отказе чтения первого необходимого байта префикса.
	if _, err := io.ReadFull(c.Conn, initial[:initialN]); err != nil {
		return err
	}
	// oldPrefix сохраняет прежнее число байтов префикса для восстановления capture при его позднем
	// начале.
	oldPrefix := s.prefixN
	s.prefixN += copy(s.prefix[s.prefixN:], initial[:initialN])
	// err сообщает о неподдержанном кодеке или превышении бюджета после первого байта префикса.
	if err := c.inspectPrefix(s, oldPrefix); err != nil {
		return err
	}
	// remainingPrefix задаёт число дополнительных байтов, нужных до полного пятибайтового префикса.
	remainingPrefix := min(int(amount)-initialN, 5-s.prefixN)
	// err сообщает об отказе чтения оставшейся части префикса.
	if _, err := io.ReadFull(c.Conn, initial[initialN:initialN+remainingPrefix]); err != nil {
		return err
	}
	oldPrefix = s.prefixN
	s.prefixN += copy(s.prefix[s.prefixN:], initial[initialN:initialN+remainingPrefix])
	initialN += remainingPrefix
	// err сообщает об отказе проверки дополненного префикса.
	if err := c.inspectPrefix(s, oldPrefix); err != nil {
		return err
	}
	// chunk содержит полный проверяемый блок с заголовком для последующей выдачи библиотеке.
	chunk := make([]byte, hn+int(amount))
	copy(chunk, header[:hn])
	copy(chunk[hn:], initial[:initialN])
	// err сообщает об отказе чтения оставшегося тела блока.
	if _, err := io.ReadFull(c.Conn, chunk[hn+initialN:]); err != nil {
		return err
	}
	// body ссылается на тело текущего блока без заголовка.
	body := chunk[hn:]
	if s.collect {
		copy(s.capture[s.received:], body)
	}
	s.received += amount
	s.fragments++
	if s.received == s.length {
		if s.amfOffset >= 0 {
			if len(s.capture) <= s.amfOffset {
				return ErrFraming
			}
			// err сообщает о нарушении структуры либо бюджета AMF перед выдачей последнего
			// блока.
			if err := preflightAMF(s.capture[s.amfOffset:], c.limits); err != nil {
				return err
			}
		}
		if s.typ == 1 {
			if len(s.capture) != 4 {
				return ErrFraming
			}
			// size содержит объявленный новый размер входящих блоков из SetChunkSize.
			size := binary.BigEndian.Uint32(s.capture)
			if size == 0 || size > c.limits.MaxChunkBytes {
				return ErrLimit
			}
			c.chunkSize = size
		}
		if s.typ == 2 {
			if len(s.capture) != 4 {
				return ErrFraming
			}
			// target содержит идентификатор потока блоков, отменяемого сообщением Abort.
			target := binary.BigEndian.Uint32(s.capture)
			if target < 2 || target > 63 {
				return ErrFraming
			}
			// t содержит незавершённый поток, чьи текущие сборка и capture сбрасываются при
			// Abort.
			if t := c.streams[target]; t != nil && t.received > 0 {
				t.received = 0
				t.length = 0
				t.fragments = 0
				if t.capture != nil {
					c.retained -= uint64(cap(t.capture))
					t.capture = nil
				}
			}
		}
		c.lastConfig = nil
		if s.collect && (s.typ == 8 || s.typ == 9) && s.amfOffset < 0 {
			c.lastConfig = s.capture
		}
		s.received = 0
		s.fragments = 0
	}
	c.pending = chunk
	return nil
}

// Резервирует всё объявленное тело сообщения до выделения буфера для предварительной проверки или
// сохранения конфигурации.
func (c *wireConn) beginCapture(s *wireStream) error {
	// err сообщает об отказе предварительного резерва полного capture.
	if err := c.reserve(uint64(s.length)); err != nil {
		return err
	}
	s.capture = make([]byte, int(s.length))
	s.collect = true
	return nil
}

// Определяет поддержанный AV1/AAC и конфигурацию по накопленным первым байтам; проверяет специальный
// бюджет до полного чтения и восстанавливает ранее полученную часть префикса при начале накопления.
func (c *wireConn) inspectPrefix(s *wireStream, oldPrefix int) error {
	if s.typ == 9 && s.prefixN >= 1 {
		if s.prefix[0]&0x80 == 0 {
			return fmt.Errorf("%w: требуется Enhanced RTMP AV1", ErrCodec)
		}
		// kind содержит тип Enhanced RTMP видеопакета из младших битов префикса.
		kind := s.prefix[0] & 15
		if kind == 6 {
			return fmt.Errorf("%w: multitrack", ErrCodec)
		}
		if kind == 1 && (s.prefix[0]>>4&7 < 1 || s.prefix[0]>>4&7 > 2) {
			return fmt.Errorf("%w: AV1 FrameType", ErrCodec)
		}
		if s.prefixN == 5 && string(s.prefix[1:5]) != "av01" {
			return ErrCodec
		}
		if !s.collect && (kind == 0 || kind == 4) {
			// limit выбирает бюджет конфигурации либо AMF-метаданных AV1.
			limit := c.limits.MaxConfigBytes
			if kind == 4 {
				limit = c.limits.MaxAMFBytes
			}
			if s.length > limit {
				return ErrLimit
			}
			// err сообщает об отказе начала полного накопления видеоконфигурации или
			// метаданных.
			if err := c.beginCapture(s); err != nil {
				return err
			}
			if kind == 4 {
				s.amfOffset = 5
			}
		}
	}
	if s.typ == 8 && s.prefixN >= 1 {
		// codec содержит код аудиоформы legacy AAC либо Enhanced Audio.
		codec := s.prefix[0] >> 4
		if codec != 10 && codec != 9 {
			return ErrCodec
		}
		if codec == 9 {
			// kind содержит тип Enhanced Audio пакета.
			kind := s.prefix[0] & 15
			if kind != 0 && kind != 1 && kind != 2 {
				return ErrCodec
			}
			if s.prefixN == 5 && string(s.prefix[1:5]) != "mp4a" {
				return ErrCodec
			}
		}
		if !s.collect && s.prefixN >= 2 {
			// config показывает, является ли текущая аудиоформа сообщением конфигурации.
			config := (codec == 10 && s.prefix[1] == 0) || (codec == 9 && s.prefix[0]&15 == 0)
			if config {
				if s.length > c.limits.MaxConfigBytes {
					return ErrLimit
				}
				// err сообщает об отказе предварительного накопления аудиоконфигурации.
				if err := c.beginCapture(s); err != nil {
					return err
				}
			}
		}
	}
	if s.collect && s.received > 0 && oldPrefix > 0 {
		copy(s.capture, s.prefix[:oldPrefix])
	}
	return nil
}
