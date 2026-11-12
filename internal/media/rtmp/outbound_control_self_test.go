package rtmp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/rawmessage"
)

// Создаёт управляющее сообщение с csid=2 и MessageStreamID=0 для проверки обработчика.
func transportSelfControl(typ byte, body []byte) *rawmessage.Message {
	return &rawmessage.Message{ChunkStreamID: 2, Type: typ, Body: body}
}

// Проверяет сохранение окончательной ошибки сертификата при ограниченном сроке TLS, включая завершение
// локального получателя.
func TestOutboundTLSBlockedAlertPreservesCertificateFailure(t *testing.T) {
	// key хранит закрытый ключ тестового TLS-сервера. err сообщает об отказе генерации ключа.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// template задаёт самоподписанный сертификат example.test, которому клиент не доверяет.
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"example.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	// der содержит закодированный сертификат тестового TLS-сервера.
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	// left служит клиентским концом локального соединения, right — серверным концом для
	// TLS-согласования.
	left, right := net.Pipe()
	defer func() {
		// Закрывает оба конца net.Pipe после проверки ошибки сертификата.
		_ = left.Close()
		_ = right.Close()
	}()
	// serverDone закрывается после выхода серверного TLS-согласования.
	serverDone := make(chan struct{})
	go func() {
		// Выполняет TLS-согласование на принимающем конце с недоверенным сертификатом и закрывает serverDone
		// при выходе.

		defer close(serverDone)
		// peer выполняет серверную сторону TLS с самоподписанным сертификатом.
		peer := tls.Server(right, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
		_ = peer.Handshake()
	}()
	// v задаёт тестовую конфигурацию H.264, a — конфигурацию AAC для формата назначения.
	v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
	// target описывает RTMPS-назначение, на котором проверяется отказ проверки сертификата.
	target, err := ResolveOutbound("rtmps://example.test/app", "key", core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio})
	if err != nil {
		t.Fatal(err)
	}
	// l задаёт ограничения соединения с коротким сроком TLS-согласования.
	l := transportSelfLimits()
	l.TLSTimeout = 30 * time.Millisecond
	_, err = DialOutbound(context.Background(), target, OutboundOptions{Limits: l, TLSConfig: &tls.Config{RootCAs: x509.NewCertPool()}, DialContext: func(context.Context, string, string) (net.Conn, error) {
		// Возвращает клиентский конец net.Pipe для проверки отказа TLS.
		return left, nil
	}, Admit: func(context.Context, OutboundOperation) (func(), error) {
		// Выдаёт тестовый допуск операции с пустым действием освобождения.

		return func() {
			// Завершает тестовый допуск без изменения состояния.
		}, nil
	}})
	// certificate принимает ошибку проверки сертификата для подтверждения окончательного характера
	// отказа.
	var certificate *tls.CertificateVerificationError
	if !errors.As(err, &certificate) || !IsOutboundTerminal(err) {
		t.Fatalf("отказ сертификата подменён сетевым timeout: %v", err)
	}
	_ = right.Close()
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("TLS peer не завершён")
	}
}

// Проверяет жёсткое, мягкое и динамическое ограничения окна и число ответов WindowAckSize, ACK через
// оборот uint32 и отказ неверной адресации и структуры управления.
func TestOutboundPeerWindowAndAcknowledgements(t *testing.T) {
	// c хранит тестовое исходящее соединение для применения управляющих сообщений.
	c := transportSelfConn(t, &transportSelfMemory{})
	// r предоставляет обработчику управления состояние входящего чтения этого соединения.
	r := outboundReader{c: c}
	bandwidth := func(window uint32, kind byte) error {
		// bandwidth формирует SetPeerBandwidth с переданными окном и типом и применяет его к общему
		// состоянию соединения.

		// b содержит четырёхбайтовый размер окна и однобайтовый тип SetPeerBandwidth.
		var b [5]byte
		binary.BigEndian.PutUint32(b[:4], window)
		b[4] = kind
		return c.control(&r, transportSelfControl(6, b[:]))
	}
	// err сообщает об отказе обработки начального динамического окна, которое должно остаться
	// неустановленным.
	if err := bandwidth(100, 2); err != nil || c.peerWindow != 0 {
		t.Fatalf("initial dynamic: %d %v", c.peerWindow, err)
	}
	// tc задаёт очередное изменение окна получателя и ожидаемый итог после его обработки.
	for _, tc := range []struct {
		// window задаёт размер объявленного окна получателя в байтах.
		window uint32
		// kind задаёт тип ограничения SetPeerBandwidth: hard, soft либо dynamic.
		kind byte
		// want задаёт ожидаемый действующий размер окна после обработки сообщения.
		want uint32
	}{{100, 0, 100}, {200, 1, 100}, {50, 1, 50}, {80, 2, 50}, {120, 0, 120}, {140, 2, 140}, {160, 2, 160}} {
		// err сообщает об отказе применения очередного изменения окна получателя.
		if err := bandwidth(tc.window, tc.kind); err != nil || c.peerWindow != tc.want {
			t.Fatalf("peer bandwidth %v: %d %v", tc, c.peerWindow, err)
		}
	}
	if len(c.controls) != 5 {
		t.Fatalf("ответы WindowAckSize: %d", len(c.controls))
	}
	c.stats.WrittenBytes = (1 << 32) + 8
	c.peerAcknowledged = (1 << 32) - 8
	// err сообщает об отказе ACK, переводящего накопительный счётчик через границу uint32.
	if err := c.control(&r, transportSelfControl(3, []byte{0, 0, 0, 8})); err != nil || c.peerAcknowledged != (1<<32)+8 {
		t.Fatalf("ACK wrap: %d %v", c.peerAcknowledged, err)
	}
	// err сообщает об отказе ACK, опережающего число записанных байтов на один байт.
	if err := c.control(&r, transportSelfControl(3, []byte{0, 0, 0, 9})); err != nil || c.Snapshot().AckAheadBytesLast != 1 {
		t.Fatal("опережающий ACK не учтён")
	}
	// err сообщает об отказе неоднозначного ACK, который не должен сдвигать подтверждённый якорь.
	if err := c.control(&r, transportSelfControl(3, []byte{0x80, 0, 0, 9})); err != nil || c.peerAcknowledged != (1<<32)+9 {
		t.Fatal("неоднозначный ACK изменил якорь")
	}
	// m содержит очередное управляющее сообщение с неверным типом, телом либо адресацией.
	for _, m := range []*rawmessage.Message{transportSelfControl(0, nil), transportSelfControl(1, []byte{0, 0, 0, 0}), transportSelfControl(1, []byte{0xff, 0xff, 0xff, 0xff}), transportSelfControl(2, []byte{0, 0, 0, 1}), transportSelfControl(3, []byte{1}), transportSelfControl(4, []byte{0, 99, 0, 0, 0, 0}), transportSelfControl(5, []byte{0, 0, 0, 0}), transportSelfControl(6, []byte{0, 0, 0, 1, 3}), {ChunkStreamID: 4, Type: 3, Body: make([]byte, 4)}} {
		// err сообщает, отклонил ли обработчик некорректное управляющее сообщение.
		if err := c.control(&r, m); err == nil {
			t.Fatalf("некорректное control принято: %+v", m)
		}
	}
}

// Проверяет прохождение ACK между частями физической записи без закрытия соединения и с точным итогом
// 93 байта.
func TestOutboundAcknowledgementsDuringWritePreserveConnection(t *testing.T) {
	// left служит отправляющим концом локального соединения, right — читающим концом тестового
	// получателя.
	left, right := net.Pipe()
	defer func() {
		// Закрывает оба конца net.Pipe после проверки ACK во время записи.
		_ = right.Close()
		_ = left.Close()
	}()
	// c отправляет сообщение через локальное соединение при малом объявленном окне получателя.
	c := transportSelfConn(t, left)
	c.peerWindow = 20
	c.peerLimitKnown = true
	c.opts.Limits.WriteTimeout = time.Second
	// peerDone передаёт итог чтения всех порций сообщения и обработки соответствующих ACK.
	peerDone := make(chan error, 1)
	go func() {
		// Читает сообщение заданными порциями и после каждой принимает накопительный ACK, сообщая итог через
		// peerDone.

		// r связывает обработку синтетических ACK с отправляющим соединением.
		r := outboundReader{c: c}
		// total накапливает число байтов сообщения, прочитанных тестовым получателем.
		var total uint32
		// n задаёт размер очередной порции чтения, после которой выдаётся накопительный ACK.
		for _, n := range []int{20, 20, 20, 20, 13} {
			// b принимает очередную порцию физически отправленных байтов.
			b := make([]byte, n)
			// err сообщает об отказе полного чтения очередной порции сообщения.
			if _, err := io.ReadFull(right, b); err != nil {
				peerDone <- err
				return
			}
			total += uint32(n)
			// ack содержит накопительное число прочитанных получателем байтов в сетевом
			// порядке.
			var ack [4]byte
			binary.BigEndian.PutUint32(ack[:], total)
			// err сообщает об отказе обработки ACK, выданного между частями физической
			// записи.
			if err := c.control(&r, transportSelfControl(3, ack[:])); err != nil {
				peerDone <- err
				return
			}
		}
		peerDone <- nil
	}()
	// err сообщает об отказе записи сообщения с телом 81 байт и заголовком 12 байт.
	if err := c.writeMessage(context.Background(), 6, 9, 1, 0, bytes.Repeat([]byte{7}, 81)); err != nil {
		t.Fatal(err)
	}
	// err принимает итог чтения сообщения и обработки ACK со стороны тестового получателя.
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
	if c.Err() != nil || c.Snapshot().WrittenBytes != 93 {
		t.Fatalf("ACK in flight вызвал отказ: %+v %v", c.Snapshot(), c.Err())
	}
}

// Проверяет успешную запись 13 байтов при окне получателя 12 без задержки и закрытия исправного
// соединения.
func TestOutboundPeerWindowDoesNotGateWrites(t *testing.T) {
	// m сохраняет отправленные байты и признак закрытия тестового транспорта.
	m := &transportSelfMemory{}
	// c проверяет отправку сообщения, размер которого превышает объявленное окно получателя.
	c := transportSelfConn(t, m)
	c.peerWindow = 12
	c.opts.Limits.WriteTimeout = 20 * time.Millisecond
	// err сообщает об отказе записи 13 байтов при окне получателя 12 байт.
	if err := c.writeMessage(context.Background(), 6, 9, 1, 0, []byte{1}); err != nil || c.Snapshot().WrittenBytes != 13 {
		t.Fatalf("окно задержало запись: %v %+v", err, c.Snapshot())
	}
	if m.closed {
		t.Fatal("здоровое соединение закрылось")
	}
}

// Проверяет сохранение срока незаконченного сообщения при чередовании потоков и сетевой таймаут после
// единственного байта заголовка.
func TestOutboundStartedDeadlineSurvivesInterleaving(t *testing.T) {
	// data содержит чередующиеся блоки двух входящих сообщений, одно из которых остаётся
	// незавершённым.
	data := []byte{4, 0, 0, 0, 0, 0, 3, 20, 0, 0, 0, 0, 5, 5, 0, 0, 0, 0, 0, 1, 20, 0, 0, 0, 0, 5, 0xc4, 5}
	// m подаёт чередующиеся блоки и сохраняет устанавливаемые сроки чтения.
	m := &transportSelfMemory{input: bytes.NewReader(data)}
	// c владеет тестовым транспортом для проверки срока начатого сообщения.
	c := transportSelfConn(t, m)
	// r собирает входящие сообщения из однобайтовых блоков разных потоков.
	r := outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 1}
	// msg содержит сообщение, если первый блок завершил сборку. err сообщает об отказе разбора
	// первого блока.
	if msg, err := r.chunk(); msg != nil || err != nil {
		t.Fatal(msg, err)
	}
	// first сохраняет первоначальный срок завершения сообщения потока 4.
	first := r.streams[4].deadline
	// msg содержит завершённое сообщение второго потока. err сообщает об отказе разбора его блока.
	if msg, err := r.chunk(); msg == nil || err != nil {
		t.Fatal(msg, err)
	}
	// msg содержит сообщение, если продолжение завершило первый поток. err сообщает об отказе
	// разбора продолжения.
	if msg, err := r.chunk(); msg != nil || err != nil {
		t.Fatal(msg, err)
	}
	// d содержит очередной срок чтения после начала первого сообщения для проверки отсутствия
	// продления.
	for _, d := range m.deadlines[2:] {
		if d != first {
			t.Fatalf("срок был продлён: %v != %v", d, first)
		}
	}
	// left служит читающим концом локального соединения, right — источником неполного входящего
	// заголовка.
	left, right := net.Pipe()
	defer func() {
		// Закрывает оба конца net.Pipe после проверки срока начатого сообщения.
		_ = right.Close()
		_ = left.Close()
	}()
	c = transportSelfConn(t, left)
	c.opts.Limits.MessageTimeout = 20 * time.Millisecond
	r = outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 128}
	// done передаёт ошибку чтения сообщения, остановившегося на частичном заголовке.
	done := make(chan error, 1)
	go func() {
		// Читает следующее сообщение и передаёт ошибку через done для проверки срока частичного заголовка.
		// err сообщает об отказе дочитывания сообщения после единственного байта заголовка.
		_, err := r.next()
		done <- err
	}()
	// err сообщает об отказе передачи первого байта заголовка читающей стороне.
	if _, err := right.Write([]byte{4}); err != nil {
		t.Fatal(err)
	}
	select {
	// err принимает итог чтения, который должен оказаться сетевым таймаутом.
	case err := <-done:
		// n принимает сетевую ошибку для проверки её признака Timeout.
		var n net.Error
		if !errors.As(err, &n) || !n.Timeout() {
			t.Fatalf("started timeout: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("частичный заголовок не ограничен сроком")
	}
}

// Проверяет повтор расширенной метки в продолжении, полные большие csid и независимые пределы
// фрагментов, потоков и удержанных данных.
func TestOutboundExtendedTimestampAndLargeChunkStreamIDs(t *testing.T) {
	// data содержит сообщение с расширенной временной меткой и повтором этой метки в продолжении.
	data := []byte{4, 255, 255, 255, 0, 0, 2, 20, 4, 3, 2, 1, 1, 0, 0, 2, 5, 0xc4, 1, 0, 0, 2, 5}
	// bad выбирает сохранение либо искажение расширенной метки продолжения.
	for _, bad := range []bool{false, true} {
		// raw хранит отдельную копию сообщения, в которой при необходимости искажается
		// продолжение.
		raw := bytes.Clone(data)
		if bad {
			raw[len(raw)-2]++
		}
		// c предоставляет транспорт с выбранным вариантом расширенной временной метки.
		c := transportSelfConn(t, &transportSelfMemory{input: bytes.NewReader(raw)})
		// r собирает сообщение с расширенной временной меткой из однобайтовых блоков.
		r := outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 1}
		// m содержит собранное сообщение для проверки времени и тела. err сообщает об отказе его
		// разбора.
		m, err := r.next()
		if bad {
			if !errors.Is(err, ErrFraming) {
				t.Fatal(err)
			}
		} else if err != nil || m.Timestamp != 0x1000002*time.Millisecond || !bytes.Equal(m.Body, []byte{5, 5}) {
			t.Fatalf("extended: %+v %v", m, err)
		}
	}
	// basic содержит очередное расширенное кодирование идентификатора потока блоков.
	for _, basic := range [][]byte{{0, 0}, {1, 0, 1}} {
		// raw объединяет расширенный идентификатор потока блоков с полным заголовком и телом
		// сообщения.
		raw := append(bytes.Clone(basic), 0, 0, 0, 0, 0, 1, 20, 4, 3, 2, 1, 5)
		// c предоставляет транспорт с расширенным идентификатором потока блоков.
		c := transportSelfConn(t, &transportSelfMemory{input: bytes.NewReader(raw)})
		// r разбирает полное сообщение с расширенным идентификатором потока блоков.
		r := outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 128}
		// m содержит сообщение для проверки MessageStreamID. err сообщает об отказе разбора
		// расширенного идентификатора.
		m, err := r.next()
		if err != nil || m.MessageStreamID != 0x01020304 || len(r.streams) != 1 {
			t.Fatalf("large csid: %+v %v", m, err)
		}
	}
	// limit выбирает проверяемое ограничение числа фрагментов, потоков блоков либо удержанных
	// байтов.
	for _, limit := range []string{"fragments", "streams", "retained"} {
		t.Run(limit, func(t *testing.T) {
			// Проверяет выбранный предел числа фрагментов, потоков блоков либо удерживаемых байтов при расширенной
			// временной метке.

			// m подаёт данные сообщения и позволяет добавить второй поток блоков в
			// соответствующем сценарии.
			m := &transportSelfMemory{input: bytes.NewReader(data)}
			// c хранит ограничения, одно из которых уменьшается до порога отказа.
			c := transportSelfConn(t, m)
			// r собирает сообщения под выбранным уменьшенным ограничением.
			r := outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 1}
			switch limit {
			case "fragments":
				c.opts.Limits.MaxFragments = 1
			case "streams":
				c.opts.Limits.MaxChunkStreams = 1
				// raw добавляет к исходному сообщению сообщение второго потока блоков
				// для превышения MaxChunkStreams.
				raw := append(bytes.Clone(data), 5, 0, 0, 0, 0, 0, 1, 20, 0, 0, 0, 0, 5)
				m.input = bytes.NewReader(raw)
			case "retained":
				c.opts.Limits.MaxRetainedBytes = c.opts.Limits.baseBytes() + c.target.retainedBytes() + 1
			}
			// err сообщает об отказе первого сообщения при проверке выбранного ограничения.
			if _, err := r.next(); limit == "streams" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err = r.next(); !errors.Is(err, ErrLimit) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrLimit) {
				t.Fatalf("%s: %v", limit, err)
			}
		})
	}
}

// Проверяет повторную актуальность после допуска: устаревшая или нулевая Delivery не пишет данные,
// освобождает допуск и сохраняет сокет.
func TestOutboundStaleAfterAdmissionPreservesSocket(t *testing.T) {
	// m сохраняет вывод и состояние закрытия транспорта при устаревании доставки.
	m := &transportSelfMemory{}
	// c проверяет повторную актуальность доставки после получения допуска к записи.
	c := transportSelfConn(t, m)
	// checks считает проверки актуальности, чтобы потерять её сразу после первого допуска.
	checks := 0
	// finished отмечает вызов освобождения выданного допуска операции.
	finished := false
	c.opts.Admit = func(context.Context, OutboundOperation) (func(), error) {
		// Выдаёт допуск с действием, отмечающим его освобождение после повторной проверки актуальности.

		return func() {
			// Отмечает завершение выданного допуска операции.
			finished = true
		}, nil
	}
	// err сообщает об отклонении доставки, потерявшей актуальность между проверками.
	err := c.withCurrentWriter(context.Background(), func() bool {
		// Возвращает истину только при первой проверке, моделируя потерю актуальности во время получения
		// допуска.
		checks++
		return checks == 1
	}, func() error {
		// Сообщает об ошибке теста, если сериализация достигнута после потери актуальности.

		t.Fatal("устаревшее сообщение сериализовано")
		return nil
	})
	if !errors.Is(err, media.ErrStaleSource) || !finished || c.Err() != nil || m.output.Len() != 0 {
		t.Fatalf("stale: %v %v %v", err, finished, c.Err())
	}
	if _, err = c.WriteDelivery(context.Background(), media.Delivery{}); !errors.Is(err, media.ErrStaleSource) || c.Err() != nil {
		t.Fatalf("нулевая доставка: %v", err)
	}
}

// Проверяет окончательный отказ некорректных AMF-команд и сохранение code/description удалённого
// отказа.
func TestOutboundCommandPreflightAndDenialDetails(t *testing.T) {
	// c предоставляет обработчик команд для проверки некорректного AMF и причин отказа.
	c := transportSelfConn(t, &transportSelfMemory{})
	// raw содержит очередную некорректную последовательность AMF-команды.
	for _, raw := range [][]byte{{10, 255, 255, 255, 255}, {2, 0, 10, 'x'}, {5}, {0, 0, 0, 0, 0, 0, 0, 0, 0}} {
		// err сообщает об отказе предварительного разбора некорректной AMF-команды.
		if _, err := c.decodeCommand(&rawmessage.Message{Body: raw}, 0); !IsOutboundTerminal(err) {
			t.Fatalf("AMF command принят: %x %v", raw, err)
		}
	}
	// description задаёт текст удалённого отказа, который должен сохраниться в причине ошибки.
	description := "Синтетический отказ авторизации"
	// denied содержит окончательную ошибку публикации, построенную из удалённого code и description.
	denied := outboundDenial(outboundCommand{args: amf0.Data{nil, amf0.Object{{Key: "code", Value: "NetStream.Publish.BadName"}, {Key: "description", Value: description}}}})
	// e принимает типизированный отказ публикации для проверки кода и исходного описания.
	var e *OutboundError
	if !errors.As(denied, &e) || e.Code != "NetStream.Publish.BadName" || e.Cause.Error() != description || !IsOutboundTerminal(denied) {
		t.Fatalf("причина отказа потеряна: %v", denied)
	}
}

// Проверяет поддержанное подмножество полного H.264/AV1/AAC-формата до сети, допустимые неопределённые
// Level/Tier и канонические числовые коды цвета.
func TestOutboundDeclaredCapabilitiesAreCheckedBeforeNetwork(t *testing.T) {
	// v задаёт исходную конфигурацию H.264, a — конфигурацию AAC для проверки объявленных
	// возможностей.
	v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
	// profile хранит исходный поддержанный формат перед отдельными проверками недопустимых
	// изменений.
	profile := core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio}
	// change задаёт очередное изменение H.264 либо AAC, которое должно отклоняться до подключения.
	for _, change := range []func(*core.MediaFormat){
		func(p *core.MediaFormat) {
			// Задаёт неподдержанное обозначение профиля H.264.
			p.Video.CodecProfile = "unrepresentable-profile"
		}, func(p *core.MediaFormat) {
			// Задаёт неприменимый к H.264 уровень иерархии high.
			p.Video.Tier = "high"
		}, func(p *core.MediaFormat) {
			// Задаёт нечётную ширину H.264, нарушающую поддержанный формат.
			p.Video.Width = 127
		}, func(p *core.MediaFormat) {
			// Задаёт уровень H.264 30.0 вне представимого однобайтового значения.
			p.Video.Level = "30.0"
		},
		func(p *core.MediaFormat) {
			// Задаёт неизвестный диапазон цвета.
			p.Video.Color.Range = "unknown"
		}, func(p *core.MediaFormat) {
			// Задаёт iso-1 вместо именованного значения bt709.
			p.Video.Color.Primaries = "iso-1"
		}, func(p *core.MediaFormat) {
			// Задаёт iso-16 вместо именованного значения smpte2084.
			p.Video.Color.Transfer = "iso-16"
		}, func(p *core.MediaFormat) {
			// Задаёт неизвестное обозначение цветовой матрицы.
			p.Video.Color.Matrix = "not-a-matrix"
		},
		func(p *core.MediaFormat) {
			// Меняет число каналов на один, сохраняя противоречащую ему раскладку stereo.
			p.Audio.Channels = 1
		}, func(p *core.MediaFormat) {
			// Задаёт частоту AAC за пределом 24-битного поля.
			p.Audio.SampleRateHz = 1 << 24
		}, func(p *core.MediaFormat) {
			// Задаёт неподдержанный профиль AAC HE.
			p.Audio.CodecProfile = "HE"
		},
	} {
		// candidate содержит отдельную копию формата с одним проверяемым недопустимым
		// изменением.
		candidate := profile
		change(&candidate)
		// err сообщает об отказе разрешения назначения с неподдержанным объявленным форматом.
		if _, err := ResolveOutbound("rtmp://example.test/app", "key", candidate); !IsOutboundTerminal(err) {
			t.Fatalf("неподдержанный полный профиль принят: %+v %v", candidate, err)
		}
	}
	// level задаёт очередное допустимое представление уровня H.264.
	for _, level := range []string{"", "auto", "1.1"} {
		// candidate содержит формат с проверяемым допустимым уровнем H.264.
		candidate := profile
		candidate.Video.Level = level
		// err сообщает об отказе разрешения назначения с допустимым уровнем H.264.
		if _, err := ResolveOutbound("rtmp://example.test/app", "key", candidate); err != nil {
			t.Fatalf("допустимый level: %v", err)
		}
	}
	if !outboundColorValue("iso-0", "iso-", map[uint8]string{1: "bt709"}) || outboundColorValue("iso-256", "iso-", nil) || outboundColorValue("iso-01", "iso-", nil) {
		t.Fatal("каноническое кодовое значение")
	}
	// vf содержит формат AV1, разобранный из тестовых заголовков и временного блока. err сообщает об
	// отказе такого разбора.
	vf, _, err := codec.InspectAV1TU(testAV1Config(), testAV1TU(), core.FrameRate{Numerator: 30, Denominator: 1})
	if err != nil {
		t.Fatal(err)
	}
	profile.Video = vf
	// change задаёт очередное неподдержанное изменение объявленного формата AV1.
	for _, change := range []func(*core.MediaFormat){func(p *core.MediaFormat) {
		// Задаёт профиль AV1 high вместо поддержанного main.
		p.Video.CodecProfile = "high"
	}, func(p *core.MediaFormat) {
		// Задаёт ширину AV1 выше поддержанного предела 65536.
		p.Video.Width = 65537
	}, func(p *core.MediaFormat) {
		// Задаёт уровень AV1 9.0 вне поддержанного диапазона.
		p.Video.Level = "9.0"
	}, func(p *core.MediaFormat) {
		// Задаёт уровень AV1 2.0 с недопустимым для него уровнем иерархии high.
		p.Video.Level = "2.0"
		p.Video.Tier = "high"
	}, func(p *core.MediaFormat) {
		// Задаёт неподдержанную характеристику переноса PQ через cicp:16.
		p.Video.Color.Transfer = "cicp:16"
	}, func(p *core.MediaFormat) {
		// Задаёт матрицу AV1 в неподдержанном представлении iso-0.
		p.Video.Color.Matrix = "iso-0"
	}} {
		// candidate содержит отдельную копию формата с проверяемым недопустимым параметром AV1.
		candidate := profile
		change(&candidate)
		// err сообщает об отказе разрешения назначения с неподдержанным объявленным AV1.
		if _, err := ResolveOutbound("rtmp://example.test/app", "key", candidate); !IsOutboundTerminal(err) {
			t.Fatalf("неподдержанный AV1 принят: %+v %v", candidate, err)
		}
	}
	// level задаёт очередное допустимое неопределённое представление уровня AV1.
	for _, level := range []string{"", "auto", "unspecified"} {
		// candidate содержит формат AV1 с неопределённым уровнем и пустым Tier.
		candidate := profile
		candidate.Video.Level = level
		candidate.Video.Tier = ""
		// err сообщает об отказе разрешения назначения с допустимыми неопределёнными Level и
		// Tier AV1.
		if _, err := ResolveOutbound("rtmp://example.test/app", "key", candidate); err != nil {
			t.Fatalf("неопределённый optional AV1 level/tier: %v", err)
		}
	}
}
