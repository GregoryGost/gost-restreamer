package rtmp

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/handshake"
	"github.com/bluenviron/gortmplib/pkg/rawmessage"
)

// Передаёт заданный uint32 ACK через настоящий обработчик управляющих сообщений с правильной
// адресацией.
func ackAcceptanceControl(c *OutboundConn, raw uint32) error {
	// b содержит последовательный номер ACK в сетевом порядке байтов.
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], raw)
	return c.control(&outboundReader{c: c}, &rawmessage.Message{ChunkStreamID: 2, Type: 3, Body: b[:]})
}

// Проверяет три отдельных опережения 4/8/9 байтов, игнорирование повторов и неоднозначных ACK и
// сохранение истории после обычного подтверждения.
func TestACKAcceptanceDistinctEventsAndSerialBoundaries(t *testing.T) {
	// c хранит тестовое соединение для проверки отдельных опережающих и неоднозначных ACK.
	c, _, _ := acceptanceOutboundLocal(t, &acceptanceOutboundMemory{}, false)
	c.stats.WrittenBytes = 1000
	// callbacks сохраняет размеры опережения, переданные наблюдателю для каждого нового события.
	var callbacks []uint64
	c.opts.OnACKAnomaly = func(n uint64, _ time.Time) {
		// Сохраняет размер каждого измеренного опережения ACK для проверки отдельных событий, дубликатов и
		// устаревших значений.
		callbacks = append(callbacks, n)
	}
	// before отмечает начало проверки для ограничения времени последней ACK-аномалии.
	before := time.Now()
	// raw задаёт очередной последовательный номер ACK, включая повторы и неоднозначные значения.
	for _, raw := range []uint32{900, 1000, 1004, 1004, 1003, 1008, 1009, 1009 + 1<<31, math.MaxUint32} {
		// err сообщает об отказе обработки числового ACK, который не должен завершать транспорт.
		if err := ackAcceptanceControl(c, raw); err != nil {
			t.Fatalf("числовой ACK %d завершил транспорт: %v", raw, err)
		}
	}
	// s сохраняет итог трёх отдельных измерений опережения ACK.
	s := c.Snapshot()
	if s.AckAheadEventsTotal != 3 || s.AckAheadBytesLast != 9 || s.AckAheadBytesMax != 9 || !s.AckLastAnomalyAt.After(before) || s.AckLastAnomalyAt.After(time.Now()) || !reflect.DeepEqual(callbacks, []uint64{4, 8, 9}) {
		t.Fatalf("новые, повторные, устаревшие и неоднозначные ACK: %+v callbacks=%v", s, callbacks)
	}
	c.stats.WrittenBytes = 2000
	// err сообщает об отказе обычного ACK после увеличения числа записанных байтов.
	if err := ackAcceptanceControl(c, 1800); err != nil {
		t.Fatal(err)
	}
	// got содержит снимок после обычного ACK для проверки сохранности истории аномалий.
	if got := c.Snapshot(); got.AckAheadEventsTotal != 3 || got.AckAheadBytesLast != 9 || got.AckLastAnomalyAt != s.AckLastAnomalyAt {
		t.Fatalf("нормальный ACK изменил историю: %+v", got)
	}
}

// Проверяет ACK через оборот uint32, потерю эпохи без эвристического восстановления и продолжение
// записи при насыщении WrittenBytes.
func TestACKAcceptanceWrapUnknownEpochAndOverflow(t *testing.T) {
	// tc задаёт границу разворачивания ACK, исходные счётчики и ожидаемую измеримость.
	for _, tc := range []struct {
		// name задаёт имя сценария сворачивания, неоднозначности либо переполнения ACK.
		name string
		// written задаёт исходное число записанных байтов. anchor задаёт исходный накопительный
		// якорь подтверждённых байтов.
		written, anchor uint64
		// raw задаёт полученный 32-битный последовательный номер ACK.
		raw uint32
		// events задаёт ожидаемое число событий опережения. last задаёт ожидаемый размер
		// последнего опережения в байтах.
		events, last uint64
		// unknown задаёт ожидаемый признак утраты однозначной эпохи ACK.
		unknown bool
	}{
		{"wrap_behind", 1<<32 + 100, 1<<32 - 20, 80, 0, 0, false},
		{"wrap_ahead", 1<<32 + 100, 1<<32 - 20, 104, 1, 4, false},
		{"half_range_lag", 1 << 31, 0, 1<<31 + 4, 0, 0, true},
		{"candidate_too_far", 0, 1, 1 << 31, 0, 0, false},
		{"checked_add", math.MaxUint64 - 5, math.MaxUint64 - 5, 4, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет выбранную границу сворачивания, неоднозначности эпохи либо переполнения ACK и запрет
			// эвристического восстановления потерянной эпохи.

			// c проверяет разворачивание ACK при выбранных исходных счётчиках.
			c, _, _ := acceptanceOutboundLocal(t, &acceptanceOutboundMemory{}, false)
			c.stats.WrittenBytes, c.peerAcknowledged = tc.written, tc.anchor
			// err сообщает об отказе обработки граничного ACK.
			if err := ackAcceptanceControl(c, tc.raw); err != nil {
				t.Fatal(err)
			}
			// s содержит снимок наблюдений после граничного ACK.
			s := c.Snapshot()
			if s.AckAheadEventsTotal != tc.events || s.AckAheadBytesLast != tc.last || c.ackEpochUnknown != tc.unknown {
				t.Fatalf("неверная измеримость: %+v unknown=%v", s, c.ackEpochUnknown)
			}
			if tc.events == 0 && !tc.unknown && c.peerAcknowledged != tc.anchor && tc.name != "wrap_behind" {
				t.Fatal("непригодный кандидат продвинул anchor")
			}
			if tc.unknown {
				c.stats.WrittenBytes = 100
				// err сообщает об отказе ACK после явного уменьшения счётчика при уже
				// потерянной эпохе.
				if err := ackAcceptanceControl(c, 104); err != nil {
					t.Fatal(err)
				}
				if c.Snapshot().AckAheadEventsTotal != 0 {
					t.Fatal("потерянная эпоха восстановлена эвристикой")
				}
			}
		})
	}
	// m сохраняет байты, записанные после насыщения счётчика WrittenBytes.
	m := &acceptanceOutboundMemory{}
	// c проверяет продолжение отправки при достижении предела uint64.
	c, _, _ := acceptanceOutboundLocal(t, m, false)
	c.stats.WrittenBytes, c.peerAcknowledged = math.MaxUint64-1, math.MaxUint64-1
	// err сообщает об отказе записи сообщения, доводящего WrittenBytes до насыщения.
	if err := c.writeMessage(context.Background(), 6, 9, 1, 0, []byte{7}); err != nil {
		t.Fatal("насыщение W остановило Write", err)
	}
	// s содержит снимок после записи для проверки насыщения и потери эпохи ACK.
	if s := c.Snapshot(); s.WrittenBytes != math.MaxUint64 || !s.Saturated || !c.ackEpochUnknown || m.wire.Len() != 13 {
		t.Fatalf("переполнение W: %+v unknown=%v bytes=%d", s, c.ackEpochUnknown, m.wire.Len())
	}
	// err сообщает об отказе ACK при уже неизвестной эпохе подтверждения.
	if err := ackAcceptanceControl(c, 9); err != nil || c.Snapshot().AckAheadEventsTotal != 0 {
		t.Fatal("неизвестная эпоха создала измерение", err)
	}
}

// Проверяет обновление последнего и максимального опережения и вызовы наблюдателя после насыщения
// общего числа событий.
func TestACKAcceptanceSaturatedEventsContinueLastAndMaximum(t *testing.T) {
	// c проверяет продолжение наблюдений после насыщения общего числа ACK-аномалий.
	c, _, _ := acceptanceOutboundLocal(t, &acceptanceOutboundMemory{}, false)
	c.stats.WrittenBytes = 100
	c.stats.AckAheadEventsTotal = math.MaxUint64
	// callbacks считает вызовы наблюдателя после достижения предела счётчика событий.
	var callbacks int
	c.opts.OnACKAnomaly = func(uint64, time.Time) {
		// Считает вызовы обработчика опережения после насыщения общего числа событий.
		callbacks++
	}
	// err сообщает об отказе первого ACK с опережением четыре байта при насыщенном счётчике.
	if err := ackAcceptanceControl(c, 104); err != nil {
		t.Fatal(err)
	}
	// first сохраняет время первой ACK-аномалии для сравнения с последующим наблюдением.
	first := c.Snapshot()
	// err сообщает об отказе ACK с опережением десять байтов.
	if err := ackAcceptanceControl(c, 110); err != nil {
		t.Fatal(err)
	}
	c.stats.WrittenBytes = 110
	// err сообщает об отказе ACK с опережением два байта после увеличения WrittenBytes.
	if err := ackAcceptanceControl(c, 112); err != nil {
		t.Fatal(err)
	}
	// s содержит итоговый снимок для проверки независимого обновления последнего и максимального
	// опережения.
	s := c.Snapshot()
	if s.AckAheadEventsTotal != math.MaxUint64 || !s.Saturated || s.AckAheadBytesLast != 2 || s.AckAheadBytesMax != 10 || s.AckLastAnomalyAt.Before(first.AckLastAnomalyAt) || callbacks != 3 {
		t.Fatalf("насыщение общего числа отключило наблюдения: %+v callbacks=%d", s, callbacks)
	}
}

// ackAcceptanceBlockedWrite управляемо задерживает первый физический Write для проверки независимости
// обработки ACK.
type ackAcceptanceBlockedWrite struct {
	// Conn предоставляет вложенный интерфейс соединения для операций, не подменённых тестовым
	// транспортом.
	net.Conn
	// entered закрывается при входе в первый физический Write.
	entered chan struct{}
	// release разрешает завершить первый физический Write после проверки ACK.
	release chan struct{}
	// once гарантирует однократное уведомление о входе и единственную управляемую блокировку записи.
	once sync.Once
}

// Блокирует первый физический Write до release, затем подтверждает весь буфер без ввода-вывода,
// позволяя проверить независимость читателя.
func (b *ackAcceptanceBlockedWrite) Write(p []byte) (int, error) {
	b.once.Do(func() {
		// Один раз сообщает о входе в физическую запись и блокирует её до разрешения через release.
		close(b.entered)
		<-b.release
	})
	return len(p), nil
}

// Принимает срок без прерывания управляемой тестом блокировки записи.
func (b *ackAcceptanceBlockedWrite) SetWriteDeadline(time.Time) error { return nil }

// Имитирует успешное закрытие, не освобождая управляемую тестом блокировку Write.
func (b *ackAcceptanceBlockedWrite) Close() error { return nil }

// Проверяет, что ACK не ждёт заблокированный Write, не учитывает незаписанные байты и не
// переигрывается после записи; следующий ACK измеряется отдельно.
func TestACKAcceptancePhysicalWriteSkipsWithoutReplayOrReaderWait(t *testing.T) {
	// b удерживает первый физический Write до разрешения теста.
	b := &ackAcceptanceBlockedWrite{entered: make(chan struct{}), release: make(chan struct{})}
	// c проверяет обработку ACK во время заблокированной физической записи.
	c, _, _ := acceptanceOutboundLocal(t, b, false)
	c.stats.WrittenBytes = 100
	// done передаёт результат записи сообщения после снятия блокировки.
	done := make(chan error, 1)
	go func() {
		// Записывает сообщение в отдельной горутине и передаёт итог через done, позволяя тесту обработать ACK
		// во время физической записи.
		done <- c.writeMessage(context.Background(), 6, 9, 1, 0, []byte{7})
	}()
	<-b.entered
	// released отмечает снятие блокировки записи, чтобы отложенная очистка не закрыла канал
	// повторно.
	released := false
	defer func() {
		// При раннем выходе теста разрешает заблокированную запись и дожидается её результата, сообщая ошибку.

		if !released {
			close(b.release)
			// err принимает результат записи при аварийном снятии блокировки в очистке
			// теста.
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
	}()
	// ackDone передаёт результат независимой обработки ACK во время физического Write.
	ackDone := make(chan error, 1)
	go func() {
		// Обрабатывает ACK в отдельной горутине и передаёт результат через ackDone для проверки отсутствия
		// ожидания физической записи.
		ackDone <- ackAcceptanceControl(c, 1000)
	}()
	select {
	// err принимает результат обработки ACK для проверки отсутствия ожидания записи.
	case err := <-ackDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("читатель ждёт физической записи")
	}
	// s содержит снимок во время Write, когда новые байты ещё не подтверждены результатом
	// транспорта.
	if s := c.Snapshot(); s.WrittenBytes != 100 || s.AckAheadEventsTotal != 0 {
		t.Fatalf("опубликованы неподтверждённые байты либо ложная аномалия: %+v", s)
	}
	close(b.release)
	released = true
	// err принимает итог записи после разрешения завершить физический Write.
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе повторного ACK, ранее полученного во время записи.
	if err := ackAcceptanceControl(c, 1000); err != nil {
		t.Fatal(err)
	}
	// s содержит снимок после записи и повторного ACK для проверки отсутствия переигрывания
	// аномалии.
	if s := c.Snapshot(); s.WrittenBytes != 113 || s.AckAheadEventsTotal != 0 {
		t.Fatalf("ACK переигран после Write: %+v", s)
	}
	// err сообщает об отказе нового ACK после завершения физической записи.
	if err := ackAcceptanceControl(c, 1001); err != nil {
		t.Fatal(err)
	}
	// s содержит снимок нового измерения опережения после завершения записи.
	if s := c.Snapshot(); s.AckAheadEventsTotal != 1 || s.AckAheadBytesLast != 888 {
		t.Fatalf("новый ACK вне записи не измерен: %+v", s)
	}
}

// ackAcceptanceCountConn независимо считает байты, прочитанные получателем из соединения.
type ackAcceptanceCountConn struct {
	// Conn предоставляет соединение, чьи положительные результаты чтения учитываются.
	net.Conn
	// bytes хранит атомарное накопительное число прочитанных получателем байтов.
	bytes atomic.Uint64
}

// Передаёт чтение вложенному соединению и атомарно считает каждый положительный результат для
// независимой сверки WrittenBytes.
func (c *ackAcceptanceCountConn) Read(p []byte) (int, error) {
	// n сообщает число байтов, прочитанных вложенным соединением. e сохраняет исходную ошибку этого
	// чтения.
	n, e := c.Conn.Read(p)
	if n > 0 {
		c.bytes.Add(uint64(n))
	}
	return n, e
}

// ackAcceptancePeer объединяет локального получателя, независимый счётчик и каналы наблюдения его
// завершения и сообщений.
type ackAcceptancePeer struct {
	// conn служит соединением получателя для чтения RTMP и отправки управляющих сообщений.
	conn net.Conn
	// count считает принятые RTMP-байты после расшифрования TLS, когда TLS включён.
	count *ackAcceptanceCountConn
	// frames передаёт независимо разобранные сообщения, полученные после запуска публикации.
	frames chan acceptanceWireMessage
	// done закрывается при полном выходе горутины получателя.
	done chan struct{}
	// stop прерывает ожидание разрешения чтения при завершении теста.
	stop chan struct{}
}

// Отправляет независимое управляющее сообщение на csid=2 с секундным сроком записи.
func (p *ackAcceptancePeer) send(typ byte, body []byte) error {
	// err сообщает об отказе установки секундного срока отправки управляющего сообщения.
	if err := p.conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	// h содержит полный RTMP-заголовок управляющего сообщения на csid=2 и MessageStreamID=0.
	h := []byte{2, 0, 0, 0, 0, 0, byte(len(body)), typ, 0, 0, 0, 0}
	// err сообщает об отказе передачи заголовка и тела управляющего сообщения.
	_, err := p.conn.Write(append(h, body...))
	return err
}

// Кодирует uint32 ACK четырьмя байтами и отправляет его через независимый управляющий канал.
func (p *ackAcceptancePeer) ack(raw uint32) error {
	// b содержит отправляемый последовательный номер ACK в сетевом порядке байтов.
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], raw)
	return p.send(3, b[:])
}

// Запускает отдельного локального TCP/RTMPS-получателя и OutboundConn с ограниченным сетевым буфером;
// очистка закрывает обе стороны и ждёт завершения операций.
func ackAcceptanceDial(t *testing.T, secure bool, writeTimeout time.Duration, hold ...<-chan struct{}) (*OutboundConn, *ackAcceptancePeer) {
	t.Helper()
	// l принимает локальное TCP-соединение на автоматически выбранном порту. err сообщает об отказе
	// открытия слушателя.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает локальный слушатель после проверки ACK.
		_ = l.Close()
	})
	// opts задаёт ограничения и допуск операций исходящего тестового соединения.
	opts := OutboundOptions{Limits: acceptanceOutboundLimits(), Admit: func(context.Context, OutboundOperation) (func(), error) {
		// Выдаёт тестовый допуск операции с пустым действием освобождения.

		return func() {
			// Завершает тестовый допуск без изменения состояния.
		}, nil
	}}
	opts.Limits.WriteTimeout = writeTimeout
	// endpoint сохраняет точный тестовый URL, выбирая схему RTMP либо RTMPS.
	endpoint := "rtmp://receiver.test/a%2Fb/../live?x=%2F&x=2"
	// serverConfig задаёт сертификат локального TLS-сервера при защищённом соединении.
	var serverConfig *tls.Config
	if secure {
		// cert содержит сертификат receiver.test. pool содержит доверяющий этому сертификату
		// клиентский пул.
		cert, pool := acceptanceCertificate(t)
		serverConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		opts.TLSConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		endpoint = "rtmps://receiver.test/a%2Fb/../live?x=%2F&x=2"
	}
	opts.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		// Подключает клиент к локальному слушателю и запрашивает TCP-буфер записи размером 1024 байта,
		// закрывая сокет при ошибке настройки.

		// conn содержит подключённый локальный TCP-сокет. err сообщает об отказе подключения
		// либо настройки его буфера записи.
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", l.Addr().String())
		if err == nil {
			if err = conn.(*net.TCPConn).SetWriteBuffer(1024); err != nil {
				_ = conn.Close()
				return nil, err
			}
		}
		return conn, err
	}
	// ready передаёт готового получателя после успешной установки публикации.
	ready := make(chan *ackAcceptancePeer, 1)
	// failure передаёт отказ приёма соединения либо установки публикации на стороне получателя.
	failure := make(chan error, 1)
	go func() {
		// Принимает TCP либо TLS, выполняет тестовую установку, при необходимости ждёт разрешения чтения и
		// передаёт разобранные сообщения в frames.

		// raw содержит принятый TCP-сокет. e сообщает об отказе приёма соединения.
		raw, e := l.Accept()
		if e != nil {
			failure <- e
			return
		}
		// conn выбирает исходный TCP-сокет либо его серверную TLS-обёртку.
		conn := raw
		if secure {
			conn = tls.Server(raw, serverConfig)
		}
		// count независимо считает прочитанные получателем RTMP-байты.
		count := &ackAcceptanceCountConn{Conn: conn}
		// p объединяет соединение получателя и каналы его сообщений и завершения.
		p := &ackAcceptancePeer{conn: conn, count: count, frames: make(chan acceptanceWireMessage, 128), done: make(chan struct{}), stop: make(chan struct{})}
		defer close(p.done)
		defer func() {
			// Закрывает принятое соединение при завершении тестового приёмника.
			_ = conn.Close()
		}()
		if e = acceptanceStartup(count, ""); e != nil {
			failure <- e
			return
		}
		ready <- p
		if len(hold) > 0 {
			select {
			case <-hold[0]:
			case <-p.stop:
				return
			}
		}
		for {
			// m содержит очередное независимо разобранное сообщение после запуска. e
			// сообщает об отказе его чтения.
			m, e := acceptanceReadMessage(count, 128)
			if e != nil {
				return
			}
			p.frames <- m
		}
	}()
	// ctx ограничивает запуск и срок существования тестового соединения десятью секундами. cancel
	// освобождает его ресурсы.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	// c содержит исходящее соединение после успешного запуска к локальному получателю.
	c, err := DialOutbound(ctx, acceptanceTarget(t, endpoint), opts)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	// peer принимает готового локального получателя для возврата и последующей очистки.
	var peer *ackAcceptancePeer
	select {
	case peer = <-ready:
	case err = <-failure:
		cancel()
		t.Fatal(err)
	case <-ctx.Done():
		cancel()
		t.Fatal(ctx.Err())
	}
	t.Cleanup(func() {
		// Закрывает исходящее соединение, ждёт его операции с отдельным сроком, отменяет контекст и
		// останавливает приёмник с ожиданием его выхода.

		_ = c.Close()
		// join ограничивает ожидание завершения обеих сторон после закрытия. stop освобождает
		// ресурсы этого срока.
		join, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		// e сообщает об отказе ожидания завершения исходящих операций после Close.
		if e := c.Wait(join); e != nil {
			t.Error(e)
		}
		cancel()
		close(peer.stop)
		_ = peer.conn.Close()
		select {
		case <-peer.done:
		case <-join.Done():
			t.Error("приёмник не присоединён")
		}
	})
	return c, peer
}

// Проверяет временную сетевую ошибку срока записи при молчащем получателе TCP/TLS и подтверждение
// завершения всех операций через Wait.
func TestACKAcceptanceRealSlowTCPAndTLSDeadlineAndJoin(t *testing.T) {
	// secure выбирает TCP либо TLS для проверки срока записи молчащему получателю.
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls_%v", secure), func(t *testing.T) {
			// Проверяет сетевой срок записи и последующее завершение операций при приёмнике, удерживающем чтение,
			// для выбранного TCP либо TLS.

			// hold оставляет получателя в ожидании и запрещает чтение сообщений после
			// запуска.
			hold := make(chan struct{})
			// c отправляет большие сообщения получателю, удерживающему чтение.
			c, _ := ackAcceptanceDial(t, secure, 100*time.Millisecond, hold)
			// body содержит 65536 байтов для заполнения сетевых буферов при молчащем
			// получателе.
			body := bytes.Repeat([]byte{9}, 65536)
			// err сохраняет первый отказ записи после заполнения сетевых буферов.
			var err error
			for range 256 {
				if err = c.writeMessage(context.Background(), 6, 9, 1, 0, body); err != nil {
					break
				}
			}
			// timeout принимает сетевую ошибку для проверки признака истечения срока записи.
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() || IsOutboundTerminal(err) {
				t.Fatalf("зависший получатель не ограничен сетевым сроком: %v", err)
			}
			// join ограничивает ожидание завершения операций после таймаута. cancel
			// освобождает ресурсы этого срока.
			join, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err = c.Wait(join); err != nil {
				t.Fatal("истечение срока не завершило операции", err)
			}
		})
	}
}

// Проверяет немедленное прерывание активной сетевой записи через Close и её физическое завершение
// через Wait до будущего срока записи.
func TestACKAcceptanceCloseJoinsActiveTCPAndTLSWrite(t *testing.T) {
	// secure выбирает TCP либо TLS для проверки прерывания активной записи через Close.
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls_%v", secure), func(t *testing.T) {
			// Проверяет прерывание активной записи через Close и подтверждение её завершения через Wait для
			// выбранного TCP либо TLS.

			// c отправляет данные получателю, который после запуска не читает сообщения.
			c, _ := ackAcceptanceDial(t, secure, 5*time.Second, make(chan struct{}))
			// initial сохраняет WrittenBytes после запуска для обнаружения последующего
			// прогресса записи.
			initial := c.Snapshot().WrittenBytes
			// result передаёт итог серии больших записей после закрытия соединения.
			result := make(chan error, 1)
			go func() {
				// Передаёт большие сообщения до первой ошибки или конца заданного числа попыток и сообщает результат,
				// пока приёмник не читает.

				// body содержит 65536 байтов для заполнения буферов и достижения
				// активной сетевой записи.
				body := bytes.Repeat([]byte{9}, 65536)
				for range 256 {
					// err сообщает о первом отказе большой записи либо её прерывании
					// через Close.
					if err := c.writeMessage(context.Background(), 6, 9, 1, 0, body); err != nil {
						result <- err
						return
					}
				}
				result <- nil
			}()
			// guard ограничивает ожидание момента активной сетевой записи одной секундой.
			guard := time.NewTimer(time.Second)
			defer guard.Stop()
			// tick задаёт период повторной проверки активной записи и WrittenBytes.
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for {
				c.mu.Lock()
				// active сохраняет признак физической записи под блокировкой. written
				// сохраняет одновременно прочитанное число подтверждённых транспортом
				// байтов.
				active, written := c.writeActive, c.stats.WrittenBytes
				c.mu.Unlock()
				if active && written > initial {
					break
				}
				select {
				// err принимает преждевременный результат отправки, пока тест ожидает
				// активную запись.
				case err := <-result:
					t.Fatalf("запись завершилась до отмены при зависшем получателе: %v", err)
				case <-guard.C:
					t.Fatal("не достигнута активная сетевая запись")
				case <-tick.C:
				}
			}
			// err сообщает об отказе закрытия соединения для прерывания записи.
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			// err принимает результат отправки после Close, который должен содержать отказ.
			case err := <-result:
				if err == nil {
					t.Fatal("отмена принята как успешная передача")
				}
			case <-time.After(time.Second):
				t.Fatal("Close ждёт будущего сетевого срока")
			}
			// join ограничивает ожидание завершения операций после Close. cancel освобождает
			// ресурсы этого срока.
			join, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			// err сообщает об отказе ожидания завершения прерванной физической записи.
			if err := c.Wait(join); err != nil {
				t.Fatal("Close не присоединил физическую запись", err)
			}
		})
	}
}

// Проверяет окончательный ErrFraming для усечённых ACK и сообщений окон через настоящий TCP и TLS.
func TestACKAcceptanceMalformedWireStillTerminates(t *testing.T) {
	// secure выбирает TCP либо TLS для передачи повреждённых управляющих сообщений.
	for _, secure := range []bool{false, true} {
		// typ задаёт тип повреждённого ACK либо объявления окна.
		for _, typ := range []byte{3, 5, 6} {
			t.Run(fmt.Sprintf("tls_%v_type_%d", secure, typ), func(t *testing.T) {
				// Передаёт выбранное управляющее сообщение с неверной длиной через TCP либо TLS и проверяет
				// окончательный ErrFraming.

				// c содержит проверяемое исходящее соединение. peer отправляет ему
				// повреждённое управляющее сообщение.
				c, peer := ackAcceptanceDial(t, secure, 5*time.Second)
				// err сообщает об отказе отправки управляющего сообщения с недопустимой
				// длиной тела.
				if err := peer.send(typ, []byte{1}); err != nil {
					t.Fatal(err)
				}
				select {
				case <-c.Done():
				case <-time.After(time.Second):
					t.Fatal("повреждённое управляющее сообщение принято")
				}
				if !errors.Is(c.Err(), ErrFraming) || !IsOutboundTerminal(c.Err()) {
					t.Fatal("потерян отказ проверки формата", c.Err())
				}
			})
		}
	}
}

// Проверяет однократный OnACKAnomaly с опережением четыре байта при последующем отказе connect, когда
// DialOutbound не возвращает соединение.
func TestACKAcceptanceFailedDialRetainsMeasuredStartupCallback(t *testing.T) {
	// client служит клиентским концом локального соединения, peer — концом сервера, отклоняющего
	// connect.
	client, peer := net.Pipe()
	defer func() {
		// Закрывает оба конца net.Pipe после проверки отказа установки.
		_ = client.Close()
		_ = peer.Close()
	}()
	// ctx ограничивает проверку неудачного запуска тремя секундами. cancel освобождает ресурсы этого
	// срока.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// err сообщает об отказе установки срока серверных операций перед неудачным запуском.
	if err := peer.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// committed атомарно накапливает байты, уже переданные клиентским OnWrite.
	var committed atomic.Uint64
	// changed уведомляет сервер об изменении committed без блокировки клиентского наблюдателя.
	changed := make(chan struct{}, 16)
	// callbacks сохраняет измеренные опережения ACK, полученные до отказа DialOutbound.
	callbacks := make(chan uint64, 4)
	// serverDone передаёт итог рукопожатия, ACK и отклонения команды connect.
	serverDone := make(chan error, 1)
	go func() {
		// Согласует RTMP, дожидается совпадения принятого и учтённого объёма, отправляет ACK с опережением на
		// четыре байта и отказ connect.

		// count независимо считает RTMP-байты, прочитанные сервером при запуске.
		count := &ackAcceptanceCountConn{Conn: peer}
		// err сообщает об отказе серверного рукопожатия перед отклонением connect.
		if _, _, err := handshake.DoServer(count, true); err != nil {
			serverDone <- err
			return
		}
		// err сообщает об отказе чтения команды connect перед отправкой опережающего ACK.
		if _, err := acceptanceReadMessage(count, 128); err != nil {
			serverDone <- err
			return
		}
		for committed.Load() != count.bytes.Load() {
			select {
			case <-changed:
			case <-ctx.Done():
				serverDone <- ctx.Err()
				return
			}
		}
		// p предоставляет отправку ACK через серверный конец локального соединения.
		p := &ackAcceptancePeer{conn: peer}
		// err сообщает об отказе отправки ACK с опережением четыре байта относительно принятого
		// объёма.
		if err := p.ack(uint32(count.bytes.Load() + 4)); err != nil {
			serverDone <- err
			return
		}
		serverDone <- acceptanceSendAMF(peer, 0, amf0.Data{"_error", float64(1), nil, amf0.Object{{Key: "code", Value: "NetConnection.Connect.Rejected"}}})
	}()
	// opts задаёт локальный транспорт и наблюдателей записи и ACK для проверки событий неудачного
	// запуска.
	opts := OutboundOptions{Limits: acceptanceOutboundLimits(), DialContext: func(context.Context, string, string) (net.Conn, error) {
		// Возвращает клиентский конец net.Pipe вместо сетевого подключения.
		return client, nil
	}, Admit: func(context.Context, OutboundOperation) (func(), error) {
		// Выдаёт тестовый допуск операции с пустым действием освобождения при проверке отказа установки.

		return func() {
			// Завершает тестовый допуск без изменения состояния.
		}, nil
	}, OnWrite: func(n int) {
		// Учитывает фактически записанные байты и неблокирующе уведомляет приёмник об изменении committed.

		committed.Add(uint64(n))
		select {
		case changed <- struct{}{}:
		default:
		}
	}, OnACKAnomaly: func(n uint64, _ time.Time) {
		// Передаёт измеренный размер опережения ACK в callbacks для проверки сохранности события при отказе
		// установки.
		callbacks <- n
	}}
	// conn содержит результат DialOutbound, который при отказе должен быть nil. err содержит
	// окончательный отказ connect.
	conn, err := DialOutbound(ctx, acceptanceTarget(t, "rtmp://receiver.test/a%2Fb/../live?x=%2F&x=2"), opts)
	if conn != nil || !IsOutboundTerminal(err) {
		t.Fatalf("неверный результат установления соединения: %v %v", conn, err)
	}
	if err = <-serverDone; err != nil {
		t.Fatal(err)
	}
	select {
	// n принимает измеренный размер опережения ACK, который должен равняться четырём байтам.
	case n := <-callbacks:
		if n != 4 {
			t.Fatal("неверный размер ACK при установлении соединения", n)
		}
	default:
		t.Fatal("DialOutbound потерял OnACKAnomaly неудачного установления соединения")
	}
	if len(callbacks) != 0 {
		t.Fatal("ACK установления соединения повторно учтён")
	}
}

// Ждёт сообщение выбранного типа от локального получателя не более двух секунд, пропуская остальные
// типы.
func ackAcceptanceFrame(t *testing.T, p *ackAcceptancePeer, typ byte) acceptanceWireMessage {
	t.Helper()
	// timeout ограничивает ожидание сообщения нужного типа двумя секундами.
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		select {
		// m содержит очередное сообщение получателя для отбора нужного RTMP-типа.
		case m := <-p.frames:
			if m.typ == typ {
				return m
			}
		case <-timeout.C:
			t.Fatalf("не получен тип %d до срока записи", typ)
			return acceptanceWireMessage{}
		}
	}
}

// Проверяет передачу данных и настоящего H.264 AU сверх окна без ACK либо с опережающими/устаревшими
// ACK через TCP/TLS, сохраняя собственные ACK и ping-ответы.
func TestACKAcceptanceRealTCPAndTLSProgressBeyondWindow(t *testing.T) {
	// secure выбирает TCP либо TLS для проверки прогресса сверх окна получателя.
	for _, secure := range []bool{false, true} {
		// mode выбирает отсутствие ACK, постоянное опережение либо повторы и устаревшие значения
		// через оборот uint32.
		for _, mode := range []string{"absent", "constant_ahead", "duplicate_stale_wrap"} {
			t.Run(fmt.Sprintf("tls_%v_%s", secure, mode), func(t *testing.T) {
				// Проверяет передачу за объявленное окно при выбранной последовательности ACK по TCP либо TLS,
				// сохранность байтов и работу собственных ACK и PingResponse.

				// c отправляет сообщения сверх объявленного окна. p независимо принимает
				// их и отправляет управление.
				c, p := ackAcceptanceDial(t, secure, 5*time.Second)
				// got сохраняет WrittenBytes после запуска для сравнения с независимым
				// счётчиком получателя.
				if got := c.Snapshot().WrittenBytes; got != p.count.bytes.Load() || got < 3073 {
					t.Fatalf("не учтены согласование соединения и управляющие сообщения: written=%d receiver=%d", got, p.count.bytes.Load())
				}
				// err сообщает об отказе объявления жёсткого окна получателя размером 32
				// байта.
				if err := p.send(6, []byte{0, 0, 0, 32, 0}); err != nil {
					t.Fatal(err)
				}
				// m содержит ответ WindowAckSize для проверки точного отражения
				// объявленного окна.
				if m := ackAcceptanceFrame(t, p, 5); !bytes.Equal(m.body, []byte{0, 0, 0, 32}) {
					t.Fatalf("ответ окна: %x", m.body)
				}
				// offset задаёт искусственный сдвиг счётчика на полный оборот uint32 для
				// сравнения с физическим приёмом.
				offset := uint64(0)
				if mode == "duplicate_stale_wrap" {
					// Большое состояние задаётся явно; переход через ноль приходит настоящим сетевым ACK.
					c.writeMu.Lock()
					c.mu.Lock()
					offset = 1 << 32
					c.stats.WrittenBytes += offset
					c.peerAcknowledged = 1<<32 - 20
					c.mu.Unlock()
					c.writeMu.Unlock()
				}
				// i задаёт номер большого сообщения, определяющий его байт заполнения и
				// временную метку.
				for i := range 4 {
					if mode == "constant_ahead" {
						// err сообщает об отказе отправки очередного опережающего
						// ACK.
						if err := p.ack(uint32(p.count.bytes.Load() + 4)); err != nil {
							t.Fatal(err)
						}
					}
					if mode == "duplicate_stale_wrap" {
						// v задаёт очередной ACK из последовательности
						// сворачивания, повторов и неоднозначных значений.
						for _, v := range []uint32{0xfffffff0, 4, 4, 3, 0x80000004} {
							// err сообщает об отказе передачи очередного ACK из
							// проверяемой последовательности.
							if err := p.ack(v); err != nil {
								t.Fatal(err)
							}
						}
					}
					// body содержит нагрузку очередного большого сообщения для
					// побайтовой сверки на получателе.
					body := bytes.Repeat([]byte{byte(i + 1)}, 4096)
					// written передаёт результат отправки большого сообщения
					// независимо от его чтения тестом.
					written := make(chan error, 1)
					go func() {
						// Записывает очередное большое сообщение и сообщает результат через written, пока тест независимо
						// читает его на приёмнике.
						written <- c.writeMessage(context.Background(), 6, 9, 0x765432, uint32(i), body)
					}()
					// m содержит принятое большое сообщение для проверки сохранности
					// тела.
					m := ackAcceptanceFrame(t, p, 9)
					if !bytes.Equal(m.body, body) {
						t.Fatal("получатель потерял полезную нагрузку")
					}
					select {
					// err принимает итог отправки большого сообщения для проверки
					// отсутствия ожидания ACK.
					case err := <-written:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(time.Second):
						t.Fatal("Write ждёт ACK до будущего сетевого срока")
					}
				}
				// video задаёт H.264-конфигурацию, audio — AAC-конфигурацию для проверки
				// настоящего медиапакета.
				video, audio := acceptanceOutboundConfigs(t, false, media.TimeBase{Numerator: 1, Denominator: 1000})
				// err сообщает об отказе сохранения конфигураций перед отправкой H.264.
				if err := c.WriteHeaders(context.Background(), video, audio); err != nil {
					t.Fatal(err)
				}
				// payload содержит H.264 AU с IDR-срезом для проверки передачи
				// медиатрактом.
				payload := []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}
				// packet объединяет H.264-конфигурацию и AU с нулевыми временными
				// метками.
				packet := acceptanceOutboundPacket(t, video, 0, 0, true, payload)
				// mediaResult передаёт результат отправки H.264 и проверки
				// подтверждённого объёма полезной нагрузки.
				mediaResult := make(chan error, 1)
				go func() {
					// Записывает настоящий H.264 AU, проверяет число полностью отправленных медиабайтов и сообщает
					// результат через mediaResult.

					// result содержит сведения о полностью отправленном медиапакете.
					// err сообщает об отказе отправки либо неверном числе
					// медиабайтов.
					result, err := c.WritePacket(context.Background(), packet)
					if err == nil && result.PayloadBytes != 7 {
						err = errors.New("неверное число медиабайтов")
					}
					mediaResult <- err
				}()
				for {
					// frame содержит очередное видеосообщение, из которых тест
					// пропускает конфигурацию и выбирает AU.
					frame := ackAcceptanceFrame(t, p, 9)
					if len(frame.body) > 1 && frame.body[1] == 0 {
						continue
					}
					if !bytes.Equal(frame.body, append([]byte{0x17, 1, 0, 0, 0}, payload...)) {
						t.Fatalf("медиатракт не передал реальный H.264 AU: %x", frame.body)
					}
					break
				}
				select {
				// err принимает итог отправки H.264 для проверки отсутствия ожидания ACK
				// после превышения окна.
				case err := <-mediaResult:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("медиатракт ждёт ACK после превышения окна")
				}
				if c.Err() != nil || c.Snapshot().WrittenBytes != p.count.bytes.Load()+offset {
					t.Fatalf("ACK прервал либо исказил счётчик: %+v bytes=%d err=%v", c.Snapshot(), p.count.bytes.Load(), c.Err())
				}
				// err сообщает об отказе объявления однобайтового окна входящих
				// подтверждений.
				if err := p.send(5, []byte{0, 0, 0, 1}); err != nil {
					t.Fatal(err)
				}
				// m содержит собственный ACK клиента для проверки учёта байтов
				// рукопожатия и управления.
				if m := ackAcceptanceFrame(t, p, 3); len(m.body) != 4 || binary.BigEndian.Uint32(m.body) < 3073 {
					t.Fatalf("собственный ACK потерян: %x", m.body)
				}
				// err сообщает об отказе передачи PingRequest клиенту.
				if err := p.send(4, []byte{0, 6, 1, 2, 3, 4}); err != nil {
					t.Fatal(err)
				}
				// m содержит PingResponse клиента для сверки точного тела ответа.
				if m := ackAcceptanceFrame(t, p, 4); !bytes.Equal(m.body, []byte{0, 7, 1, 2, 3, 4}) {
					t.Fatalf("ping: %x", m.body)
				}
			})
		}
	}
}

// Проверяет, что после частичной ошибки WrittenBytes равно 17 и ACK=21 создаёт опережение ровно четыре
// байта.
func TestACKAcceptancePartialPositiveWriteUsesOnlyReceivedBytes(t *testing.T) {
	// m ограничивает одну запись семью байтами и возвращает ошибку после приёма 17 байтов.
	m := &acceptanceOutboundMemory{short: 7, failAt: 17}
	// c проверяет расчёт опережения ACK после положительного частичного Write с ошибкой.
	c, _, _ := acceptanceOutboundLocal(t, m, false)
	// err сообщает об ожидаемом отказе записи после приёма транспортом 17 байтов.
	if err := c.writeMessage(context.Background(), 6, 9, 1, 0, bytes.Repeat([]byte{1}, 128)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	// err сообщает об отказе ACK=21 после частично успешной записи.
	if err := ackAcceptanceControl(c, 21); err != nil {
		t.Fatal(err)
	}
	// s содержит снимок для проверки WrittenBytes=17 и опережения ACK ровно на четыре байта.
	if s := c.Snapshot(); s.WrittenBytes != 17 || s.AckAheadEventsTotal != 1 || s.AckAheadBytesLast != 4 || m.wire.Len() != 17 {
		t.Fatalf("положительный результат частичной записи: %+v received=%d", s, m.wire.Len())
	}
}
