package integration

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// publisherWireMessage хранит результат независимого разбора одного RTMP-сообщения для проверок
// отправителя.
type publisherWireMessage struct {
	// connection обозначает порядковый номер принятого TCP-соединения, чтобы различать попытки
	// публикации.
	connection uint64
	// typ задаёт RTMP-тип сообщения: команду, аудио, видео или метаданные.
	typ byte
	// stream содержит MessageStreamID сообщения; timestamp задаёт абсолютную RTMP-метку в
	// миллисекундах.
	stream, timestamp uint32
	// body содержит самостоятельную копию полного тела RTMP-сообщения.
	body []byte
}

// publisherReceiver владеет локальным приёмником, его соединениями и независимо захваченными
// RTMP-данными.
type publisherReceiver struct {
	// listener принимает локальные TCP-соединения, при необходимости обёрнутые в TLS.
	listener net.Listener
	// mu защищает активные соединения, захваченные сообщения, команды публикации и уведомления об
	// изменениях.
	mu sync.Mutex
	// connections хранит открытые соединения для принудительного разрыва и завершения тестового
	// приёмника.
	connections map[net.Conn]struct{}
	// messages накапливает медиасообщения и метаданные для независимых проверок RTMP.
	messages []publisherWireMessage
	// apps сохраняет значения app из connect; keys сохраняет имена публикации из publish для
	// проверки адресации.
	apps, keys []string
	// changed закрывается при изменении состояния, чтобы разбудить ожидающие проверки, и заменяется
	// под mu.
	changed chan struct{}
	// workers учитывает горутину приёма и обработчики соединений для ожидания полного завершения.
	workers sync.WaitGroup
	// accepted атомарно считает принятые соединения и выдаёт им последовательные номера.
	accepted atomic.Uint64
	// bytes атомарно считает байты RTMP, действительно прочитанные из соединений после
	// TLS-декодирования.
	bytes atomic.Uint64
	// err сохраняет первую неожиданную ошибку разбора соединения для ожидающей проверки.
	err error
	// hold при наличии задерживает чтение после принятия publish, имитируя медленное назначение.
	hold <-chan struct{}
	// stop закрывается при очистке приёмника, чтобы прекратить ожидание задержанного чтения.
	stop chan struct{}
}

// Запускает независимый локальный RTMP-приёмник; secure добавляет самоподписанный TLS и возвращает
// доверие к нему. Необязательный hold блокирует чтение после принятия publish, а t.Cleanup закрывает
// соединения и ограниченно ждёт всех обработчиков.
func newPublisherReceiver(t *testing.T, secure bool, hold ...<-chan struct{}) (*publisherReceiver, *tls.Config) {
	t.Helper()
	// l принимает соединения на свободном порту адреса 127.0.0.1; err сообщает об ошибке открытия
	// слушателя.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// trust при TLS хранит клиентскую конфигурацию доверия самоподписанному сертификату тестового
	// приёмника.
	var trust *tls.Config
	if secure {
		// key содержит закрытый RSA-ключ тестового TLS-сервера; e сообщает об ошибке его
		// генерации.
		key, e := rsa.GenerateKey(rand.Reader, 2048)
		if e != nil {
			t.Fatal(e)
		}
		// template описывает краткоживущий сертификат localhost для локального RTMPS-приёмника.
		template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		// der содержит самоподписанный сертификат тестового TLS-сервера в бинарном формате
		// X.509.
		der, e := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if e != nil {
			t.Fatal(e)
		}
		// cert содержит разобранный сертификат для добавления в доверенные корни тестового
		// клиента.
		cert, e := x509.ParseCertificate(der)
		if e != nil {
			t.Fatal(e)
		}
		// pool хранит доверенный сертификат локального TLS-приёмника.
		pool := x509.NewCertPool()
		pool.AddCert(cert)
		trust = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		l = tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12})
	}
	// r владеет слушателем, учётом соединений и захваченными сообщениями нового тестового приёмника.
	r := &publisherReceiver{listener: l, connections: make(map[net.Conn]struct{}), changed: make(chan struct{}), stop: make(chan struct{})}
	if len(hold) > 0 {
		r.hold = hold[0]
	}
	r.workers.Add(1)
	go func() {
		// Принимает локальные соединения и создаёт отдельного учитываемого работника разбора каждого
		// соединения.

		defer r.workers.Done()
		for {
			// c содержит принятое локальное соединение; e сообщает об ошибке приёма или
			// закрытии слушателя.
			c, e := l.Accept()
			if e != nil {
				return
			}
			r.mu.Lock()
			r.connections[c] = struct{}{}
			r.mu.Unlock()
			// id связывает сообщения текущего соединения с его порядковым номером среди
			// принятых подключений.
			id := r.accepted.Add(1)
			r.workers.Add(1)
			go func() {
				// Разбирает одно соединение и сохраняет первую неожиданную ошибку для ожидающих теста.

				defer r.workers.Done()
				defer func() {
					// Закрывает соединение, удаляет его из активных и уведомляет ожидающих под блокировкой.
					_ = c.Close()
					r.mu.Lock()
					delete(r.connections, c)
					r.notifyLocked()
					r.mu.Unlock()
				}()
				// e сообщает о неожиданном отказе RTMP-обмена текущего соединения для
				// сохранения в состоянии приёмника.
				if e := r.serve(c, id); e != nil && !errors.Is(e, io.EOF) && !errors.Is(e, net.ErrClosed) {
					r.mu.Lock()
					if r.err == nil {
						r.err = e
					}
					r.notifyLocked()
					r.mu.Unlock()
				}
			}()
		}
	}()
	t.Cleanup(func() {
		// Закрывает слушатель и соединения, снимает задержку чтения и ограниченно ожидает всех работников
		// приёмника.

		close(r.stop)
		_ = l.Close()
		r.mu.Lock()
		// c представляет активное соединение, закрываемое при очистке приёмника.
		for c := range r.connections {
			_ = c.Close()
		}
		r.mu.Unlock()
		// done закрывается после завершения всех работников приёмника и ограничивает ожидание
		// очистки.
		done := make(chan struct{})
		go func() {
			// Сообщает о завершении всех работников после остановки слушателя и соединений.
			r.workers.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("локальный RTMP-приёмник не завершился")
		}
	})
	return r, trust
}

// Будит всех ожидающих изменений и создаёт канал для следующего ожидания; вызывающий обязан удерживать
// r.mu.
func (r *publisherReceiver) notifyLocked() { close(r.changed); r.changed = make(chan struct{}) }

// Ожидает удаления всех активных соединений обработчиками приёмника; через три секунды завершает тест
// ошибкой.
func (r *publisherReceiver) waitClosed(t *testing.T) {
	t.Helper()
	// timer ограничивает ожидание удаления всех активных соединений тремя секундами.
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		r.mu.Lock()
		// remaining задаёт число ещё открытых соединений; changed позволяет ждать изменения
		// этого набора без удержания блокировки.
		remaining, changed := len(r.connections), r.changed
		r.mu.Unlock()
		if remaining == 0 {
			return
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatal("приёмник не дочитал соединение до EOF")
		}
	}
}

// Возвращает копию списка принятых сообщений под r.mu. Полезные нагрузки остаются общими неизменяемыми
// срезами.
func (r *publisherReceiver) snapshot() []publisherWireMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]publisherWireMessage(nil), r.messages...)
}

// Закрывает текущие соединения под r.mu, оставляя listener открытым для проверки переподключения
// Publisher.
func (r *publisherReceiver) disconnect() {
	r.mu.Lock()
	defer r.mu.Unlock()
	// c представляет соединение, принудительно разрываемое для проверки переподключения отправителя.
	for c := range r.connections {
		_ = c.Close()
	}
}

// Ожидает выполнения predicate над копией принятых сообщений. Ошибка разбора или истечение восьми
// секунд завершает тест; predicate выполняется без r.mu.
func (r *publisherReceiver) wait(t *testing.T, predicate func([]publisherWireMessage) bool) []publisherWireMessage {
	t.Helper()
	// timer ограничивает ожидание заданного состояния RTMP-приёмника восемью секундами.
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		r.mu.Lock()
		// messages копирует захваченные RTMP-сообщения для вызова условия без блокировки
		// приёмника.
		messages := append([]publisherWireMessage(nil), r.messages...)
		// changed сообщает об очередном обновлении приёмника; err содержит первую неожиданную
		// ошибку его обработчиков.
		changed, err := r.changed, r.err
		r.mu.Unlock()
		if predicate(messages) {
			return messages
		}
		if err != nil {
			t.Fatalf("независимый RTMP-приёмник: %v", err)
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatalf("RTMP-условие не выполнено: messages=%d connections=%d", len(messages), r.accepted.Load())
		}
	}
}

// publisherCountReader учитывает фактически прочитанные байты RTMP независимо от метрик отправителя.
type publisherCountReader struct {
	// Reader предоставляет исходный поток байтов соединения для независимого разбора RTMP.
	io.Reader
	// total указывает на общий атомарный счётчик байтов всех принятых соединений.
	total *atomic.Uint64
}

// Read увеличивает общий атомарный счётчик на фактически прочитанные байты, включая данные,
// возвращённые одновременно с ошибкой.
func (r publisherCountReader) Read(p []byte) (int, error) {
	// n задаёт фактически прочитанное число байтов для учёта; e сообщает об ошибке исходного чтения.
	n, e := r.Reader.Read(p)
	r.total.Add(uint64(n))
	return n, e
}

// Выполняет локальное рукопожатие RTMP и минимальный обмен командами публикации, затем сохраняет до
// 20000 медиасообщений и метаданных. Разбирает байты независимо от SDK, учитывает принятые байты и
// возвращает ошибки протокола или соединения.
func (r *publisherReceiver) serve(conn net.Conn, id uint64) error {
	// err сообщает об ошибке установки конечного срока RTMP-рукопожатия.
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	// reader оборачивает текущее соединение независимым подсчётом принятых байтов.
	reader := publisherCountReader{conn, &r.bytes}
	// c1 содержит версию клиента и первый блок его RTMP-рукопожатия.
	c1 := make([]byte, 1537)
	// err сообщает об ошибке полного чтения клиентских блоков C0 и C1.
	if _, err := io.ReadFull(reader, c1); err != nil {
		return err
	}
	if c1[0] != 3 {
		return errors.New("неверная версия RTMP")
	}
	// server содержит ответные блоки S0, S1 и S2, включая эхо клиентского C1.
	server := make([]byte, 3073)
	server[0] = 3
	copy(server[1537:], c1[1:])
	// err сообщает об ошибке записи ответных блоков RTMP-рукопожатия.
	if _, err := io.Copy(conn, bytes.NewReader(server)); err != nil {
		return err
	}
	// err сообщает об ошибке чтения завершающего клиентского блока C2.
	if _, err := io.ReadFull(reader, make([]byte, 1536)); err != nil {
		return err
	}
	// err сообщает об ошибке снятия срока соединения после успешного рукопожатия.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return err
	}
	// parser собирает сообщения текущего соединения из RTMP-чанков с независимым состоянием каждого
	// CSID.
	parser := publisherChunkReader{reader: reader, chunk: 128, streams: make(map[uint32]*publisherChunkState)}
	for {
		// m содержит очередное полное RTMP-сообщение; err сообщает об ошибке его чтения или
		// разбора.
		m, err := parser.next()
		if err != nil {
			return err
		}
		m.connection = id
		if m.typ == 1 {
			if len(m.body) != 4 {
				return errors.New("неверный SetChunkSize")
			}
			parser.chunk = binary.BigEndian.Uint32(m.body)
			if parser.chunk == 0 || parser.chunk > 1<<20 {
				return errors.New("неверный размер chunk")
			}
			continue
		}
		if m.typ == 20 {
			// values содержит аргументы команды AMF0; e сообщает об ошибке их независимого
			// разбора.
			values, e := publisherAMFValues(m.body)
			if e != nil {
				return e
			}
			if len(values) < 2 {
				return errors.New("неполная команда AMF")
			}
			// name определяет принятую RTMP-команду управления публикацией.
			name, _ := values[0].(string)
			// tx сохраняет номер транзакции AMF для связывания ответа с командой клиента.
			tx, _ := values[1].(float64)
			// reply содержит значения AMF0 ответной команды, если текущая команда требует
			// ответа.
			var reply []any
			switch name {
			case "connect":
				if len(values) < 3 {
					return errors.New("нет connect object")
				}
				// object содержит параметры connect; ok подтверждает, что клиент передал
				// объект AMF0.
				object, ok := values[2].(map[string]any)
				if !ok {
					return errors.New("неверный connect object")
				}
				// app сохраняет выбранное клиентом RTMP-приложение для проверки адреса
				// назначения.
				app, _ := object["app"].(string)
				r.mu.Lock()
				r.apps = append(r.apps, app)
				r.mu.Unlock()
				reply = []any{"_result", tx, map[string]any{"fmsVer": "FMS/3,0,1,123", "capabilities": float64(31)}, map[string]any{"level": "status", "code": "NetConnection.Connect.Success", "objectEncoding": float64(0)}}
			case "createStream":
				reply = []any{"_result", tx, nil, float64(1)}
			case "releaseStream", "FCPublish":
				reply = []any{"_result", tx, nil, nil}
			case "publish":
				if len(values) < 4 {
					return errors.New("нет publish-name")
				}
				// key сохраняет имя публикации из команды publish для независимой
				// проверки ключа назначения.
				key, _ := values[3].(string)
				r.mu.Lock()
				r.keys = append(r.keys, key)
				r.notifyLocked()
				r.mu.Unlock()
				reply = []any{"onStatus", float64(0), nil, map[string]any{"level": "status", "code": "NetStream.Publish.Start", "description": "accepted"}}
			}
			if reply != nil {
				// stream выбирает MessageStreamID ответа: ноль для соединения и единицу
				// для принятой публикации.
				stream := uint32(0)
				if name == "publish" {
					stream = 1
				}
				if err = publisherWriteMessage(conn, 20, stream, 0, publisherAMFEncode(reply...)); err != nil {
					return err
				}
				if name == "publish" && r.hold != nil {
					select {
					case <-r.hold:
					case <-r.stop:
						return net.ErrClosed
					}
				}
			}
			continue
		}
		if m.typ == 8 || m.typ == 9 || m.typ == 18 {
			r.mu.Lock()
			if len(r.messages) >= 20000 {
				r.mu.Unlock()
				return errors.New("превышен тестовый бюджет сообщений")
			}
			r.messages = append(r.messages, m)
			r.notifyLocked()
			r.mu.Unlock()
		}
	}
}

// publisherChunkState хранит последний заголовок и частично собранное сообщение одного потока чанков
// RTMP.
type publisherChunkState struct {
	// timestamp задаёт абсолютную метку сообщения; delta хранит приращение времени; length задаёт
	// полный размер тела в байтах; stream содержит MessageStreamID.
	timestamp, delta, length, stream uint32
	// typ хранит RTMP-тип сообщения для восстановления сокращённых заголовков.
	typ byte
	// extended показывает, что заголовок использует дополнительную 32-битную временную метку.
	extended bool
	// body накапливает тело текущего сообщения до достижения объявленной длины.
	body []byte
}

// publisherChunkReader независимо собирает полные RTMP-сообщения из перемежающихся потоков чанков.
type publisherChunkReader struct {
	// reader поставляет байты одного соединения для разбора заголовков и тел чанков.
	reader io.Reader
	// chunk задаёт максимальное число байтов тела в одном RTMP-чанке.
	chunk uint32
	// streams хранит отдельное состояние сборки сообщения по идентификатору потока чанков.
	streams map[uint32]*publisherChunkState
}

// Читает беззнаковое 24-битное число от старшего байта к младшему; вход должен содержать не менее трёх
// байтов.
func publisherU24(b []byte) uint32 { return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]) }

// Собирает одно полное RTMP-сообщение из перемежающихся чанков, сохраняя заголовки и дельты времени
// каждой дорожки чанков. Ограничивает число дорожек и размер сообщения, проверяет продолжения и
// расширенные метки, возвращая независимую копию тела.
func (p *publisherChunkReader) next() (publisherWireMessage, error) {
	// read обеспечивает полное чтение запрошенной части RTMP-заголовка или тела.
	read := func(n int) ([]byte, error) {
		// Читает ровно запрошенное число байтов RTMP, возвращая ошибку неполного чтения.
		// b выделяет буфер ровно для запрошенной части RTMP-потока.
		b := make([]byte, n)
		// e сообщает об ошибке полного чтения запрошенных байтов RTMP.
		_, e := io.ReadFull(p.reader, b)
		return b, e
	}
	for {
		// b содержит базовый заголовок чанка; e сообщает об ошибке его чтения.
		b, e := read(1)
		if e != nil {
			return publisherWireMessage{}, e
		}
		// format выбирает форму заголовка RTMP; cs содержит идентификатор потока чанков, включая
		// его расширенное представление.
		format, cs := b[0]>>6, uint32(b[0]&63)
		switch cs {
		case 0:
			b, e = read(1)
			if e != nil {
				return publisherWireMessage{}, e
			}
			cs = 64 + uint32(b[0])
		case 1:
			b, e = read(2)
			if e != nil {
				return publisherWireMessage{}, e
			}
			cs = 64 + uint32(b[0]) + 256*uint32(b[1])
		}
		// s указывает на состояние сборки текущего потока чанков, создаваемое при первом
		// обращении.
		s := p.streams[cs]
		if s == nil {
			if len(p.streams) >= 32 {
				return publisherWireMessage{}, errors.New("слишком много chunk streams")
			}
			s = &publisherChunkState{}
			p.streams[cs] = s
		}
		// started показывает наличие незавершённого тела, которое новый полный заголовок не
		// должен прерывать.
		started := len(s.body) > 0 && uint32(len(s.body)) < s.length
		if format != 3 && started {
			return publisherWireMessage{}, errors.New("новый header прервал сообщение")
		}
		if format < 3 {
			// n задаёт длину заголовка сообщения в байтах для текущей формы RTMP.
			n := []int{11, 7, 3}[format]
			// h содержит заголовок сообщения; e сообщает об ошибке полного чтения этого
			// заголовка.
			h, e := read(n)
			if e != nil {
				return publisherWireMessage{}, e
			}
			// stamp содержит временную метку либо дельту из заголовка с учётом расширенного
			// представления.
			stamp := publisherU24(h)
			s.extended = stamp == 0xffffff
			if format < 2 {
				s.length = publisherU24(h[3:])
				s.typ = h[6]
				if s.length > 4<<20 {
					return publisherWireMessage{}, errors.New("слишком большое сообщение")
				}
				if format == 0 {
					s.stream = binary.LittleEndian.Uint32(h[7:])
				}
			}
			if s.extended {
				h, e = read(4)
				if e != nil {
					return publisherWireMessage{}, e
				}
				stamp = binary.BigEndian.Uint32(h)
			}
			if format == 0 {
				s.timestamp = stamp
				s.delta = 0
			} else {
				s.delta = stamp
				s.timestamp += stamp
			}
			s.body = nil
		} else {
			if s.extended {
				// h содержит повторённую расширенную метку продолжения; e сообщает об
				// ошибке её чтения.
				h, e := read(4)
				if e != nil {
					return publisherWireMessage{}, e
				}
				// value содержит повторённую 32-битную метку для проверки
				// согласованности продолжения сообщения.
				value := binary.BigEndian.Uint32(h)
				if value != s.timestamp && value != s.delta {
					return publisherWireMessage{}, errors.New("неповторённый extended timestamp")
				}
			}
			if !started {
				s.timestamp += s.delta
				s.body = nil
			}
		}
		// remaining задаёт число ещё не собранных байтов текущего RTMP-сообщения.
		remaining := s.length - uint32(len(s.body))
		// h содержит очередную часть тела, ограниченную остатком сообщения и размером чанка.
		h, e := read(int(min(remaining, p.chunk)))
		if e != nil {
			return publisherWireMessage{}, e
		}
		s.body = append(s.body, h...)
		if uint32(len(s.body)) == s.length {
			// m содержит полное RTMP-сообщение с независимой копией собранного тела.
			m := publisherWireMessage{typ: s.typ, stream: s.stream, timestamp: s.timestamp, body: bytes.Clone(s.body)}
			s.body = nil
			return m, nil
		}
	}
}

// Записывает одно RTMP-сообщение на csid=3 чанками по 128 байт с MessageStreamID, записанным младшим
// байтом вперёд. Фикстура использует метки меньше 0xffffff и длину, представимую в 24 битах; ошибка
// io.Writer возвращается вызывающему коду.
func publisherWriteMessage(w io.Writer, typ byte, stream, stamp uint32, body []byte) error {
	// h задаёт полный RTMP-заголовок ответного сообщения в потоке чанков с идентификатором 3.
	h := []byte{3, byte(stamp >> 16), byte(stamp >> 8), byte(stamp), byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body)), typ, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(h[8:], stream)
	// wire накапливает заголовок и чанки ответного RTMP-сообщения перед записью соединению.
	var wire bytes.Buffer
	wire.Write(h)
	for len(body) > 0 {
		// n задаёт число байтов тела, помещаемых в текущий ответный чанк размером до 128 байт.
		n := min(len(body), 128)
		wire.Write(body[:n])
		body = body[n:]
		if len(body) > 0 {
			wire.WriteByte(0xc3)
		}
	}
	// err сообщает об ошибке записи собранного RTMP-сообщения в выходной поток.
	_, err := io.Copy(w, &wire)
	return err
}

// Кодирует поддержанные тестовые значения AMF0: null, строки, числа, логические значения и объекты.
// Неподдержанный Go-тип вызывает panic, поскольку означает ошибку самой фикстуры.
func publisherAMFEncode(values ...any) []byte {
	// out накапливает бинарное AMF0-представление переданных тестовых значений.
	var out bytes.Buffer
	// encode хранит рекурсивный кодировщик поддержанных значений AMF0, включая поля объектов.
	var encode func(any)
	encode = func(value any) {
		// Рекурсивно кодирует поддержанные тестовые значения AMF0, завершая объекты требуемым маркером.

		// v содержит текущее значение в конкретном Go-типе для выбора маркера AMF0.
		switch v := value.(type) {
		case nil:
			out.WriteByte(5)
		case string:
			out.WriteByte(2)
			_ = binary.Write(&out, binary.BigEndian, uint16(len(v)))
			out.WriteString(v)
		case float64:
			out.WriteByte(0)
			_ = binary.Write(&out, binary.BigEndian, math.Float64bits(v))
		case bool:
			out.WriteByte(1)
			if v {
				out.WriteByte(1)
			} else {
				out.WriteByte(0)
			}
		case map[string]any:
			out.WriteByte(3)
			// k задаёт имя поля объекта AMF0; x содержит значение этого поля для
			// рекурсивного кодирования.
			for k, x := range v {
				_ = binary.Write(&out, binary.BigEndian, uint16(len(k)))
				out.WriteString(k)
				encode(x)
			}
			out.Write([]byte{0, 0, 9})
		default:
			panic(fmt.Sprintf("неподдержанный тестовый AMF тип %T", v))
		}
	}
	// v представляет очередной аргумент команды, записываемый в последовательность AMF0.
	for _, v := range values {
		encode(v)
	}
	return out.Bytes()
}

// Разбирает последовательность поддержанных AMF0-значений с ограничением вложенности и числа
// элементов. Повреждённые данные или неподдерживаемый маркер возвращают ошибку без частичного
// результата.
func publisherAMFValues(data []byte) ([]any, error) {
	// r читает полезную нагрузку команды AMF0 с контролем оставшихся байтов.
	r := bytes.NewReader(data)
	// elements считает все рекурсивно разобранные AMF0-значения для ограничения работы тестового
	// парсера.
	elements := 0
	// value хранит рекурсивный разборщик поддержанных AMF0-значений с контролем глубины и общего
	// бюджета.
	var value func(int) (any, error)
	// str читает строку AMF0 по её двухбайтовой длине из общего входного потока.
	str := func() (string, error) {
		// Читает строку AMF0 с двухбайтовой длиной, возвращая ошибку неполных данных.

		// n задаёт длину AMF0-строки в байтах до чтения её содержимого.
		var n uint16
		// e сообщает об ошибке чтения двухбайтовой длины AMF0-строки.
		if e := binary.Read(r, binary.BigEndian, &n); e != nil {
			return "", e
		}
		// b содержит байты текущей строки AMF0 без префикса длины.
		b := make([]byte, int(n))
		// e сообщает об ошибке полного чтения содержимого AMF0-строки.
		_, e := io.ReadFull(r, b)
		return string(b), e
	}
	value = func(depth int) (any, error) {
		// Рекурсивно разбирает поддержанные значения AMF0 с общими пределами глубины и количества элементов.

		elements++
		if depth > 8 || elements > 256 {
			return nil, errors.New("превышен тестовый AMF бюджет")
		}
		// typ выбирает вид разбираемого AMF0-значения; e сообщает об ошибке чтения маркера.
		typ, e := r.ReadByte()
		if e != nil {
			return nil, e
		}
		switch typ {
		case 0:
			// n хранит 64-битное представление числового значения AMF0 до преобразования в
			// float64.
			var n uint64
			e = binary.Read(r, binary.BigEndian, &n)
			return math.Float64frombits(n), e
		case 1:
			// b содержит байт логического значения AMF0; e сообщает об ошибке его чтения.
			b, e := r.ReadByte()
			return b != 0, e
		case 2:
			return str()
		case 5, 6:
			return nil, nil
		case 10:
			// n задаёт объявленное число элементов строгого массива AMF0 для проверки
			// предела и разбора.
			var n uint32
			if e = binary.Read(r, binary.BigEndian, &n); e != nil {
				return nil, e
			}
			if n > 256 {
				return nil, errors.New("слишком длинный тестовый AMF array")
			}
			// list накапливает разобранные значения строгого массива AMF0 в исходном
			// порядке.
			list := make([]any, 0, n)
			for range n {
				// item содержит очередное значение массива; e сообщает об ошибке его
				// рекурсивного разбора.
				item, e := value(depth + 1)
				if e != nil {
					return nil, e
				}
				list = append(list, item)
			}
			return list, nil
		case 3, 8:
			if typ == 8 {
				// n принимает объявленное число элементов ECMA-массива AMF0 перед
				// чтением полей до маркера завершения.
				var n uint32
				if e = binary.Read(r, binary.BigEndian, &n); e != nil {
					return nil, e
				}
			}
			// o накапливает именованные поля объекта или ECMA-массива AMF0.
			o := map[string]any{}
			for {
				// k содержит имя очередного поля AMF0; e сообщает об ошибке чтения этого
				// имени.
				k, e := str()
				if e != nil {
					return nil, e
				}
				if k == "" {
					// b содержит ожидаемый завершающий маркер объекта AMF0; e
					// сообщает об ошибке его чтения.
					b, e := r.ReadByte()
					if e != nil || b != 9 {
						return nil, errors.New("неверное завершение AMF object")
					}
					return o, nil
				}
				// v содержит разобранное значение текущего поля AMF0.
				v, e := value(depth + 1)
				if e != nil {
					return nil, e
				}
				o[k] = v
			}
		default:
			return nil, fmt.Errorf("неподдержанный тестовый AMF marker %d", typ)
		}
	}
	// result накапливает последовательность аргументов AMF0 только после успешного разбора каждого
	// значения.
	var result []any
	for r.Len() > 0 {
		// v содержит очередной аргумент команды AMF0; e сообщает об ошибке его разбора.
		v, e := value(0)
		if e != nil {
			return nil, e
		}
		result = append(result, v)
	}
	return result, nil
}
