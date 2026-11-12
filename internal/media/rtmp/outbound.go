package rtmp

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/handshake"
	"github.com/bluenviron/gortmplib/pkg/rawmessage"
)

// OutboundSnapshot считает принятые байты RTMP до шифрования, включая согласование
// соединения, управляющие сообщения и частичные записи с ошибкой. BufferBytes задаёт логический резерв,
// а не объём резидентной памяти.
type OutboundSnapshot struct {
	// WrittenBytes накапливает фактически принятые транспортом байты RTMP до TLS, включая
	// согласование, управление и положительную часть неуспешной записи.
	WrittenBytes uint64
	// Saturated отмечает переполнение накопительного счётчика байтов или событий опережающего ACK.
	Saturated   bool
	BufferBytes uint64
	// LastWrite сохраняет время последней корректной записи с положительным числом байтов.
	LastWrite time.Time
	// AckAheadEventsTotal считает однозначные новые ACK, опережившие записанный объём вне
	// выполняющегося Write.
	AckAheadEventsTotal uint64
	// AckAheadBytesLast содержит величину последнего обнаруженного опережения ACK в байтах.
	AckAheadBytesLast uint64
	// AckAheadBytesMax содержит наибольшее обнаруженное опережение ACK в байтах.
	AckAheadBytesMax uint64
	// AckLastAnomalyAt сохраняет время последнего обнаруженного опережения ACK.
	AckLastAnomalyAt time.Time
}

// OutboundConn владеет одним читателем, сериализатором записи и наблюдателем отмены.
// После Close вызывающий обязан дождаться Wait перед освобождением резерва исполнения.
type OutboundConn struct {
	// conn содержит текущий TCP- или TLS-сокет; при завершении закрывается через fail.
	conn net.Conn
	// ctx задаёт общий срок жизни соединения и его фоновых операций.
	ctx context.Context
	// opts содержит проверенные ограничения и обработчики допуска и наблюдения.
	opts OutboundOptions
	// target содержит неизменяемое проверенное назначение и ожидаемый формат.
	target OutboundTarget
	// startup хранит общий срок установки RTMP в наносекундах Unix; ноль снимает ограничение после
	// подтверждённого Publish.Start.
	startup atomic.Int64
	// streamID хранит идентификатор потока сообщений, выданный удалённым createStream.
	streamID atomic.Uint32
	// mu защищает завершение, транспортные счётчики и состояние диагностики ACK.
	mu sync.Mutex
	// closed отмечает начавшееся завершение соединения и запрещает новые записи.
	closed bool
	// err сохраняет первую причину завершения соединения.
	err error
	// done закрывается при начале завершения. readerDone закрывается после выхода читателя.
	// watcherDone закрывается после выхода наблюдателя отмены. joined закрывается после завершения
	// фоновых операций и зарегистрированных записей.
	done, readerDone, watcherDone, joined chan struct{}
	// writes учитывает начатые операции записи, завершения которых ждёт joined.
	writes sync.WaitGroup
	// writeMu последовательно допускает записи управления, заголовков и медиаданных.
	writeMu sync.Mutex
	// stats содержит накопительный трафик и диагностические сведения о записях и ACK.
	stats OutboundSnapshot
	// readBuffer содержит текущую сумму объявленных размеров незавершённых входящих тел в байтах.
	readBuffer uint64
	// commands передаёт разобранные ответы читателя установщику публикации через очередь на одну
	// команду.
	commands chan outboundCommand
	// inspector сохраняет состояние проверки сжатого потока между записываемыми единицами доступа.
	inspector codec.Inspector
	// video хранит принятую конфигурацию видеодорожки. audio хранит принятую конфигурацию
	// аудиодорожки.
	video, audio media.CodecConfig
	// headersPending требует записи метаданных и заголовков перед следующей независимой AU.
	// requireIndependent запрещает зависимый старт после обновления конфигураций.
	headersPending, requireIndependent bool
	// clock сохраняет общее начало и последние логические DTS дорожек исходящего потока.
	clock outboundClock
	// outboundChunkSize задаёт размер исходящих блоков после согласования SetChunkSize.
	outboundChunkSize uint32
	// peerAcknowledged хранит восстановленное абсолютное число байтов последнего однозначного ACK.
	peerAcknowledged uint64
	// writeActive показывает, что результат текущего вызова conn.Write ещё не включён в счётчики.
	writeActive bool
	// writtenOverflow отмечает потерю точности WrittenBytes при переполнении. ackEpochUnknown
	// запрещает дальнейшее восстановление абсолютных ACK после утраты однозначности.
	writtenOverflow, ackEpochUnknown bool
	// peerWindow хранит действующее окно SetPeerBandwidth для управляющего ответа.
	peerWindow uint32
	// peerLimitType сохраняет последний действующий тип ограничения SetPeerBandwidth.
	peerLimitType byte
	// peerLimitKnown показывает, известен ли предыдущий тип для обработки динамического ограничения.
	peerLimitKnown bool
	// handshakeRead считает байты согласования, включаемые в последующие собственные ACK чтения.
	handshakeRead uint64
	// controls хранит ограниченную очередь скопированных управляющих ответов.
	controls chan outboundControl
	// controlDone закрывается после завершения отправителя управляющих ответов.
	controlDone chan struct{}
}

// outboundControl содержит тип и тело одного ожидающего управляющего ответа.
type outboundControl struct {
	// typ задаёт тип RTMP-сообщения управляющего ответа.
	typ byte
	// body содержит собственную копию тела управляющего ответа.
	body []byte
}

// outboundCommand хранит разобранную AMF-команду с её адресацией и транзакцией.
type outboundCommand struct {
	// name содержит строковое имя команды AMF.
	name string
	// transaction содержит конечный числовой идентификатор транзакции команды.
	transaction float64
	// streamID содержит MessageStreamID входящей команды.
	streamID uint32
	// args содержит аргументы команды после имени и идентификатора транзакции.
	args amf0.Data
}

// DialOutbound устанавливает TCP/TLS/RTMP и подтверждает точный Publish.Start.
// Неудачная попытка закрывает и присоединяет все начатые операции до возврата.
func DialOutbound(ctx context.Context, target OutboundTarget, opts OutboundOptions) (*OutboundConn, error) {
	// err сообщает о некорректных параметрах исходящего соединения.
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	// err сообщает о некорректном назначении или превышении бюджета до сетевого соединения.
	if err := target.Validate(target.profile, opts.Limits); err != nil {
		return nil, err
	}
	if opts.Admit == nil {
		return nil, outboundError(OutboundInvalid, ErrFraming)
	}
	// err сообщает об отмене или истечении срока до установки соединения.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// finish освобождает допуск установки соединения после завершения попытки. err сообщает об
	// отказе выдачи допуска.
	finish, err := opts.Admit(ctx, OutboundEstablishment)
	if err != nil {
		return nil, err
	}
	if finish == nil {
		return nil, outboundError(OutboundInvalid, ErrFraming)
	}
	defer finish()
	// dial содержит переданный способ подключения либо стандартный DialContext.
	dial := opts.DialContext
	if dial == nil {
		// d содержит стандартный TCP-подключатель при отсутствии пользовательского DialContext.
		d := &net.Dialer{}
		dial = d.DialContext
	}
	// dialCtx ограничивает попытку TCP-подключения. cancel освобождает его срок сразу после попытки.
	dialCtx, cancel := context.WithTimeout(ctx, opts.Limits.DialTimeout)
	// raw содержит исходный TCP-сокет до возможного TLS.
	raw, err := dial(dialCtx, "tcp", target.address)
	// dialErr сохраняет результат контекста подключения до cancel для обнаружения истечения срока.
	dialErr := dialCtx.Err()
	cancel()
	if err != nil {
		if raw != nil {
			_ = raw.Close()
		}
		return nil, err
	}
	if raw == nil {
		return nil, outboundError(OutboundInvalid, ErrFraming)
	}
	if dialErr != nil {
		_ = raw.Close()
		return nil, dialErr
	}
	if err = ctx.Err(); err != nil {
		_ = raw.Close()
		return nil, err
	}
	// c содержит создаваемое соединение с каналами завершения и ограниченными очередями управления.
	c := &OutboundConn{conn: raw, ctx: ctx, opts: opts, target: target, done: make(chan struct{}), readerDone: make(chan struct{}), watcherDone: make(chan struct{}), joined: make(chan struct{}), commands: make(chan outboundCommand, 1), outboundChunkSize: 128, controls: make(chan outboundControl, opts.Limits.MaxControlPending), controlDone: make(chan struct{})}
	go func() {
		// При отмене родительского контекста завершает соединение с его причиной; при уже начатой остановке
		// выходит и закрывает watcherDone.

		defer close(c.watcherDone)
		select {
		case <-ctx.Done():
			c.fail(ctx.Err())
		case <-c.done:
		}
	}()
	go func() {
		// Ожидает выхода читателя, отправителя управляющих сообщений, наблюдателя отмены и всех
		// зарегистрированных записей, затем закрывает joined.
		<-c.readerDone
		<-c.controlDone
		<-c.watcherDone
		c.writes.Wait()
		close(c.joined)
	}()
	go c.controlLoop()
	// fail завершает созданное соединение и ждёт все начатые операции перед возвратом отказа
	// установки.
	fail := func(e error) (*OutboundConn, error) {
		// Завершает соединение с переданной ошибкой, дожидается начатых операций и возвращает исходную причину
		// отказа установки.
		c.fail(e)
		_ = c.Wait(context.Background())
		return nil, e
	}
	if target.tls {
		// cfg содержит отдельную конфигурацию TLS, которой назначается имя проверяемого сервера.
		cfg := &tls.Config{}
		if opts.TLSConfig != nil {
			cfg = opts.TLSConfig.Clone()
			if cfg.RootCAs != nil {
				cfg.RootCAs = cfg.RootCAs.Clone()
			}
		}
		cfg.ServerName = target.hostname
		// secured содержит TLS-клиент поверх исходного TCP-сокета.
		secured := tls.Client(raw, cfg)
		// conn заменяется под блокировкой до запуска чтения и записи RTMP; отмена закрывает текущий сокет.
		// tlsDeadline ограничивает весь этап TLS с учётом срока родительского контекста.
		tlsDeadline := time.Now().Add(opts.Limits.TLSTimeout)
		// parentDeadline содержит срок родительского контекста. ok показывает наличие этого
		// срока.
		if parentDeadline, ok := ctx.Deadline(); ok {
			tlsDeadline = earlierDeadline(tlsDeadline, parentDeadline)
		}
		if err = raw.SetDeadline(tlsDeadline); err != nil {
			close(c.readerDone)
			return fail(err)
		}
		// Срок сокета сохраняет причину проверки сертификата даже при зависшей
		// отправке предупреждения TLS; HandshakeContext не подменяет её истечением отдельного контекста.
		err = secured.HandshakeContext(ctx)
		if err != nil {
			close(c.readerDone)
			return fail(classifyOutboundProtocol(err))
		}
		if !time.Now().Before(tlsDeadline) {
			close(c.readerDone)
			return fail(context.DeadlineExceeded)
		}
		if err = raw.SetDeadline(time.Time{}); err != nil {
			close(c.readerDone)
			return fail(err)
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			close(c.readerDone)
			return fail(c.Err())
		}
		c.conn = secured
		c.mu.Unlock()
	}
	// deadline содержит общий срок RTMP-согласования и подтверждения публикации.
	deadline := time.Now().Add(opts.Limits.StartupTimeout)
	c.startup.Store(deadline.UnixNano())
	if err = c.conn.SetReadDeadline(deadline); err != nil {
		close(c.readerDone)
		return fail(err)
	}
	// readFinish освобождает допуск чтения после согласования RTMP.
	readFinish, err := opts.Admit(ctx, OutboundRead)
	if err != nil {
		close(c.readerDone)
		return fail(err)
	}
	if readFinish == nil {
		close(c.readerDone)
		return fail(outboundError(OutboundInvalid, ErrFraming))
	}
	_, _, err = handshake.DoClient(outboundHandshake{c}, false, false)
	readFinish()
	if err != nil {
		close(c.readerDone)
		return fail(classifyOutboundProtocol(err))
	}
	go c.readLoop()
	// startupCtx ограничивает ожидание ответов общим сроком установки. stop освобождает ресурсы
	// этого контекста.
	startupCtx, stop := context.WithDeadline(ctx, deadline)
	defer stop()
	if err = c.command(startupCtx, 0, outboundConnectData(target)); err != nil {
		return fail(err)
	}
	// m содержит очередной ответ удалённой стороны, сопоставляемый с фазой установки.
	m, err := c.receiveCommand(startupCtx)
	if err != nil {
		return fail(err)
	}
	if m.name != "_result" || m.transaction != 1 || m.streamID != 0 || !outboundStatus(m.args, "NetConnection.Connect.Success") {
		return fail(outboundStartupMismatch("connect", m, 0))
	}
	// size содержит четырёхбайтовое тело исходящего SetChunkSize.
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], opts.Limits.ChunkSize)
	// SetChunkSize само передаётся с первоначальным размером 128.
	if err = c.writeInitialChunkSize(startupCtx, size[:]); err != nil {
		return fail(err)
	}
	if err = c.command(startupCtx, 0, amf0.Data{"createStream", float64(2), nil}); err != nil {
		return fail(err)
	}
	m, err = c.receiveCommand(startupCtx)
	if err != nil {
		return fail(err)
	}
	if m.name != "_result" || m.transaction != 2 || m.streamID != 0 || len(m.args) != 2 {
		return fail(outboundStartupMismatch("createStream", m, 0))
	}
	// id содержит числовой идентификатор потока из createStream. ok подтверждает числовой тип
	// ответа.
	id, ok := m.args[1].(float64)
	if !ok || math.IsNaN(id) || math.IsInf(id, 0) || id < 1 || id > math.MaxUint32 || math.Trunc(id) != id {
		return fail(outboundError(OutboundProtocol, fmt.Errorf("некорректный идентификатор потока: phase=createStream check=stream_id value_type=%T stream_id_value=%g: %w", m.args[1], id, ErrFraming)))
	}
	c.streamID.Store(uint32(id))
	if err = c.command(startupCtx, uint32(id), amf0.Data{"publish", float64(0), nil, target.key, "live"}); err != nil {
		return fail(err)
	}
	m, err = c.receiveCommand(startupCtx)
	if err != nil {
		return fail(err)
	}
	if m.name != "onStatus" || m.transaction != 0 || m.streamID != uint32(id) || !outboundStatus(m.args, "NetStream.Publish.Start") {
		return fail(outboundStartupMismatch("publish", m, uint32(id)))
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	return c, nil
}

// outboundHandshake связывает операции согласования с общими сериализатором и счётчиками соединения.
type outboundHandshake struct { /* c указывает на соединение, чьи транспорт и счётчики используются при согласовании. */
	c *OutboundConn
}

// Read передаёт чтение согласования и считает фактически полученные байты для последующих собственных
// ACK.
func (h outboundHandshake) Read(p []byte) (int, error) {
	// n содержит фактически прочитанное число байтов согласования. err сообщает об отказе чтения.
	n, err := h.c.conn.Read(p)
	if n > 0 {
		h.c.handshakeRead += uint64(n)
	}
	return n, err
}

// Write проводит полную запись согласования через общий сериализатор и учёт байтов; ошибка
// возвращается без подтверждения полной записи.
func (h outboundHandshake) Write(p []byte) (int, error) {
	// err сообщает об отказе допуска или полной записи блока согласования.
	err := h.c.withWriter(h.c.ctx, func() error {
		// Записывает все байты блока согласования соединения под общим сериализатором записи.
		return h.c.writeAll(p)
	})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// Сохраняет транспортные, контекстные и уже классифицированные ошибки; остальные ошибки разбора делает
// окончательными OutboundProtocol.
func classifyOutboundProtocol(err error) error {
	// network принимает сетевую ошибку для сохранения её временной транспортной категории.
	var network net.Error
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) || IsOutboundTerminal(err) {
		return err
	}
	return outboundError(OutboundProtocol, err)
}

// Под блокировкой отклоняет закрытое соединение либо регистрирует начавшуюся операцию для последующего
// Wait.
func (c *OutboundConn) beginWrite() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.err
	}
	c.writes.Add(1)
	return nil
}

// Выполняет одну сериализованную операцию записи с допуском и общим сроком без дополнительной проверки
// доставки.
func (c *OutboundConn) withWriter(ctx context.Context, f func() error) error {
	return c.withCurrentWriter(ctx, nil, f)
}

// После ожидания сериализатора проверяет контекст, а актуальность доставки — до и после допуска
// записи. Устаревшая доставка не закрывает соединение; ошибка установки срока или самой операции
// завершает соединение.
func (c *OutboundConn) withCurrentWriter(ctx context.Context, current func() bool, f func() error) error {
	// err сообщает, что соединение завершено и новую запись зарегистрировать нельзя.
	if err := c.beginWrite(); err != nil {
		return err
	}
	defer c.writes.Done()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	// err сообщает об отмене либо истечении срока после ожидания сериализатора.
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.done:
		return c.Err()
	default:
	}
	if current != nil && !current() {
		return media.ErrStaleSource
	}
	// finish освобождает допуск текущей записи. err сообщает об отказе выдачи допуска.
	finish, err := c.opts.Admit(ctx, OutboundWrite)
	if err != nil {
		return err
	}
	if finish == nil {
		return outboundError(OutboundInvalid, ErrFraming)
	}
	defer finish()
	if current != nil && !current() {
		return media.ErrStaleSource
	}
	// d содержит наиболее ранний срок записи с учётом общего запуска и контекста.
	d := time.Now().Add(c.opts.Limits.WriteTimeout)
	// n содержит ещё действующий срок установки RTMP в наносекундах Unix.
	if n := c.startup.Load(); n != 0 {
		d = earlierDeadline(d, time.Unix(0, n))
	}
	// v содержит срок контекста записи. ok показывает наличие этого ограничения.
	if v, ok := ctx.Deadline(); ok {
		d = earlierDeadline(d, v)
	}
	if err = c.conn.SetWriteDeadline(d); err != nil {
		c.fail(err)
		return err
	}
	err = f()
	if err != nil {
		c.fail(err)
	}
	return err
}

// Дописывает буфер, учитывая каждый положительный Write, включая частичную запись с ошибкой. Флаг
// выполняющегося Write защищает диагностику ACK от преждевременной оценки записанного объёма.
func (c *OutboundConn) writeAll(p []byte) error {
	for len(p) > 0 {
		c.mu.Lock()
		if c.closed {
			// err сохраняет причину уже закрытого соединения до снятия блокировки.
			err := c.err
			c.mu.Unlock()
			return err
		}
		c.writeActive = true
		c.mu.Unlock()
		// n содержит число байтов, принятых текущим транспортным Write. err сообщает об отказе
		// этой записи.
		n, err := c.conn.Write(p)
		c.mu.Lock()
		// valid показывает, находится ли число записанных байтов в пределах переданного буфера.
		valid := n >= 0 && n <= len(p)
		if valid && n > 0 {
			if uint64(n) > math.MaxUint64-c.stats.WrittenBytes {
				c.stats.WrittenBytes = math.MaxUint64
				c.stats.Saturated = true
				c.writtenOverflow = true
			} else {
				c.stats.WrittenBytes += uint64(n)
			}
			c.stats.LastWrite = time.Now()
		}
		c.checkACKEpoch()
		c.writeActive = false
		c.mu.Unlock()
		if !valid {
			return outboundError(OutboundProtocol, io.ErrShortWrite)
		}
		if n > 0 {
			if c.opts.OnWrite != nil {
				c.opts.OnWrite(n)
			}
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

// checkACKEpoch вызывается только под c.mu; потерянная точность не восстанавливается.
func (c *OutboundConn) checkACKEpoch() {
	if c.writtenOverflow || c.stats.WrittenBytes > c.peerAcknowledged && c.stats.WrittenBytes-c.peerAcknowledged >= 1<<31 {
		c.ackEpochUnknown = true
	}
}

// Сопоставляет новый ACK с известной абсолютной эпохой, игнорируя дубликаты и неоднозначные значения
// без влияния на передачу. Новое опережение учитывает только вне выполняющегося Write и вызывает
// наблюдатель после снятия блокировки.
func (c *OutboundConn) acknowledge(raw uint32) {
	c.mu.Lock()
	c.checkACKEpoch()
	if c.ackEpochUnknown {
		c.mu.Unlock()
		return
	}
	// delta содержит продвижение нового сетевого ACK относительно прежнего по модулю 2³².
	delta := uint64(raw - uint32(c.peerAcknowledged))
	if delta == 0 || delta >= 1<<31 || delta > math.MaxUint64-c.peerAcknowledged {
		c.mu.Unlock()
		return
	}
	// candidate содержит предполагаемое абсолютное число подтверждённых байтов после однозначного
	// продвижения.
	candidate := c.peerAcknowledged + delta
	// written сохраняет учтённый объём записи для проверки нового ACK.
	written := c.stats.WrittenBytes
	if candidate > written && candidate-written >= 1<<31 || written > candidate && written-candidate >= 1<<31 {
		c.mu.Unlock()
		return
	}
	c.peerAcknowledged = candidate
	if c.writeActive || candidate <= written {
		c.mu.Unlock()
		return
	}
	// ahead содержит однозначное опережение записанного объёма в байтах. at содержит время
	// обнаружения этой аномалии.
	ahead, at := candidate-written, time.Now()
	if c.stats.AckAheadEventsTotal == math.MaxUint64 {
		c.stats.Saturated = true
	} else {
		c.stats.AckAheadEventsTotal++
	}
	c.stats.AckAheadBytesLast = ahead
	c.stats.AckAheadBytesMax = max(c.stats.AckAheadBytesMax, ahead)
	c.stats.AckLastAnomalyAt = at
	c.mu.Unlock()
	if c.opts.OnACKAnomaly != nil {
		c.opts.OnACKAnomaly(ahead, at)
	}
}

// Копирует управляющий ответ в ограниченную очередь без ожидания, проверяя завершение соединения и
// заполнение очереди.
func (c *OutboundConn) queueControl(typ byte, body []byte) error {
	select {
	case <-c.done:
		return c.Err()
	default:
	}
	select {
	case c.controls <- outboundControl{typ: typ, body: append([]byte(nil), body...)}:
		return nil
	default:
		return ErrLimit
	}
}

// Последовательно передаёт ожидающие управляющие ответы через общий сериализатор до остановки; ошибка
// записи завершает соединение.
func (c *OutboundConn) controlLoop() {
	defer close(c.controlDone)
	for {
		select {
		case <-c.done:
			return
		// m содержит очередной управляющий ответ из ограниченной очереди.
		case m := <-c.controls:
			// err сообщает об отказе отправки управляющего ответа.
			if err := c.writeMessage(c.ctx, 2, m.typ, 0, 0, m.body); err != nil {
				c.fail(err)
				return
			}
		}
	}
}

// Записывает одно целое RTMP-сообщение через общий сериализатор, допуск и срок записи.
func (c *OutboundConn) writeMessage(ctx context.Context, csid, typ byte, streamID, timestamp uint32, body []byte) error {
	return c.withWriter(ctx, func() error {
		// Передаёт одно сообщение в обрамляющий сериализатор под общим владением записью.
		return c.writeFramed(csid, typ, streamID, timestamp, body)
	})
}

// Передаёт SetChunkSize с исходным размером 128 и меняет размер последующих исходящих блоков только
// после успешной записи.
func (c *OutboundConn) writeInitialChunkSize(ctx context.Context, body []byte) error {
	// Это четырёхбайтовое сообщение целиком помещается в начальные 128 байт.
	return c.withWriter(ctx, func() error {
		// Передаёт SetChunkSize с прежним размером блоков и меняет outboundChunkSize только после успешной
		// полной записи.

		// err сообщает об отказе полной записи первоначального SetChunkSize.
		if err := c.writeFramed(2, 1, 0, 0, body); err != nil {
			return err
		}
		c.outboundChunkSize = c.opts.Limits.ChunkSize
		return nil
	})
}

// Строит команду connect с отдельными точными app и tcUrl и объявленной поддержкой AV1 в fourCcList.
func outboundConnectData(target OutboundTarget) amf0.Data {
	return amf0.Data{"connect", float64(1), amf0.Object{{Key: "app", Value: target.app}, {Key: "tcUrl", Value: target.endpoint}, {Key: "type", Value: "nonprivate"}, {Key: "flashVer", Value: "FMLE/3.0 (compatible; native)"}, {Key: "fpad", Value: false}, {Key: "capabilities", Value: float64(15)}, {Key: "audioCodecs", Value: float64(1024)}, {Key: "videoCodecs", Value: float64(128)}, {Key: "videoFunction", Value: float64(1)}, {Key: "objectEncoding", Value: float64(0)}, {Key: "fourCcList", Value: amf0.StrictArray{"av01"}}}}
}

// Кодирует проверенную AMF-команду и передаёт её на заданный MessageStreamID через поток блоков 3.
func (c *OutboundConn) command(ctx context.Context, id uint32, data amf0.Data) error {
	// body содержит сериализованную AMF-команду. err сообщает об отказе её проверки или кодирования.
	body, err := c.marshalAMF(data)
	if err != nil {
		return err
	}
	return c.writeMessage(ctx, 3, 20, id, 0, body)
}

// Проверяет размер AMF до сериализации и затем глубину и число элементов; ошибки сохраняет в
// категориях исходящего соединения.
func (c *OutboundConn) marshalAMF(data amf0.Data) ([]byte, error) {
	// n содержит вычисленный размер AMF до выделения сериализованного буфера. err сообщает об отказе
	// вычисления.
	n, err := data.MarshalSize()
	if err != nil {
		return nil, outboundError(OutboundProtocol, err)
	}
	if uint64(n) > uint64(c.opts.Limits.MaxAMFBytes) {
		return nil, outboundError(OutboundInvalid, ErrLimit)
	}
	// b содержит сериализованные байты AMF для проверки глубины и числа элементов.
	b, err := data.Marshal()
	if err != nil {
		return nil, outboundError(OutboundProtocol, err)
	}
	if err = preflightAMF(b, c.opts.Limits.amfLimits()); err != nil {
		return nil, outboundError(OutboundProtocol, err)
	}
	return b, nil
}

// Ждёт следующую команду запуска, отмену контекста либо завершение соединения.
func (c *OutboundConn) receiveCommand(ctx context.Context) (outboundCommand, error) {
	select {
	case <-ctx.Done():
		return outboundCommand{}, ctx.Err()
	// m содержит очередной ответ установщику публикации из канала читателя.
	case m := <-c.commands:
		return m, nil
	case <-c.done:
		return outboundCommand{}, c.Err()
	}
}

// Однократно сохраняет первую причину завершения и закрывает done и текущий сокет; физическое
// завершение операций подтверждает Wait.
func (c *OutboundConn) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if err == nil {
		err = net.ErrClosed
	}
	c.closed = true
	c.err = err
	close(c.done)
	// conn сохраняет текущий сокет для закрытия после снятия блокировки.
	conn := c.conn
	c.mu.Unlock()
	_ = conn.Close()
}

// Close останавливает ввод-вывод; Wait отдельно подтверждает физическое завершение.
func (c *OutboundConn) Close() error { c.fail(net.ErrClosed); return nil }

// Done возвращает канал, закрывающийся при первом завершении соединения; окончания всех операций
// следует ждать через Wait.
func (c *OutboundConn) Done() <-chan struct{} { return c.done }

// Err возвращает первую причину завершения под блокировкой либо nil для открытого соединения.
func (c *OutboundConn) Err() error { c.mu.Lock(); defer c.mu.Unlock(); return c.err }

// Wait не освобождает резерв исполнения и не считает истечение срока доказательством завершения.
// Для уже закрытого соединения повторный вызов может подтвердить позднее завершение всех операций.
func (c *OutboundConn) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.joined:
		return nil
	}
}

// Snapshot возвращает согласованные счётчики и логический резерв базовых буферов, назначения и
// незавершённых входящих тел.
func (c *OutboundConn) Snapshot() OutboundSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	// s содержит согласованную копию счётчиков с вычисляемым текущим логическим резервом.
	s := c.stats
	s.BufferBytes = c.opts.Limits.baseBytes() + c.target.retainedBytes() + c.readBuffer
	return s
}

// Обновляет под блокировкой логическое удержание незавершённых тел читателя.
func (c *OutboundConn) setReadBuffer(n uint64) { c.mu.Lock(); c.readBuffer = n; c.mu.Unlock() }

// Владеет допуском чтения, разбирает управление и команды запуска и снимает общий срок только при
// точном Publish.Start. Отказ или неожиданное сообщение завершает соединение с сохранённой причиной.
func (c *OutboundConn) readLoop() {
	defer close(c.readerDone)
	// finish освобождает допуск чтения при выходе читателя. err сообщает об отказе его выдачи.
	finish, err := c.opts.Admit(c.ctx, OutboundRead)
	if err != nil {
		c.fail(err)
		return
	}
	if finish == nil {
		c.fail(outboundError(OutboundInvalid, ErrFraming))
		return
	}
	defer finish()
	// r собирает входящие блоки в сообщения и учитывает прочитанные байты вместе с согласованием.
	r := outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 128, readBytes: c.handshakeRead}
	// startup показывает, ожидается ли ещё подтверждение Publish.Start.
	startup := true
	for {
		// m содержит очередное полное входящее сообщение. err сообщает об отказе его чтения или
		// сборки.
		m, err := r.next()
		if err != nil {
			c.fail(classifyOutboundProtocol(err))
			return
		}
		if m.Type <= 6 {
			if err = c.control(&r, m); err != nil {
				c.fail(classifyOutboundProtocol(err))
				return
			}
			continue
		}
		if m.Type != 20 {
			c.fail(outboundError(OutboundProtocol, outboundMessageError("message_type", m, r.messageChunkStreamID, fmt.Errorf("неподдерживаемый тип сообщения: expected_type=20: %w", ErrFraming))))
			return
		}
		// cmd содержит разобранную и проверенную команду управления публикацией.
		cmd, err := c.decodeCommand(m, r.messageChunkStreamID)
		if err != nil {
			c.fail(err)
			return
		}
		if cmd.name == "onBWDone" {
			continue
		}
		if cmd.name == "_error" {
			c.fail(outboundDenial(cmd))
			return
		}
		if cmd.name == "onStatus" && !outboundStatus(cmd.args, "NetStream.Publish.Start") {
			c.fail(outboundDenial(cmd))
			return
		}
		if !startup {
			c.fail(outboundError(OutboundProtocol, outboundMessageError("post_publish", m, r.messageChunkStreamID, fmt.Errorf("неожиданная команда после публикации: command=%q transaction=%g args_len=%d: %w", cmd.name, cmd.transaction, len(cmd.args), ErrFraming))))
			return
		}
		if cmd.name == "onStatus" && cmd.transaction == 0 && cmd.streamID == c.streamID.Load() && cmd.streamID != 0 && outboundStatus(cmd.args, "NetStream.Publish.Start") {
			startup = false
			c.startup.Store(0)
		}
		select {
		case c.commands <- cmd:
		case <-c.done:
			return
		}
	}
}

// Проверяет адресацию и длины управляющих сообщений, обновляет размеры блоков и окна, обрабатывает
// Abort, ACK и ping. Окно SetPeerBandwidth определяет управляющий ответ, не ограничивая запись
// медиаданных.
func (c *OutboundConn) control(r *outboundReader, m *rawmessage.Message) (err error) {
	defer func() {
		// При ошибке добавляет адресацию и длину управляющего сообщения, сохраняя исходную причину через
		// обёртку.

		if err != nil {
			// csid содержит полный идентификатор потока блоков для диагностики, включая
			// значения выше 255.
			csid := uint32(m.ChunkStreamID)
			if csid == 0 && r.messageChunkStreamID > 255 {
				csid = r.messageChunkStreamID
			}
			err = fmt.Errorf("ошибка управляющего сообщения: phase=control type=%d csid=%d stream_id=%d body_len=%d: %w", m.Type, csid, m.MessageStreamID, len(m.Body), err)
		}
	}()
	// b ссылается на тело текущего управляющего сообщения.
	b := m.Body
	if m.MessageStreamID != 0 || m.ChunkStreamID != 2 {
		return fmt.Errorf("неверная адресация: check=control_address expected_stream_id=0 expected_csid=2: %w", ErrFraming)
	}
	switch m.Type {
	case 1:
		if len(b) != 4 {
			return fmt.Errorf("неверная длина SetChunkSize: check=body_length expected_body_len=4: %w", ErrFraming)
		}
		// size содержит объявленный удалённой стороной размер входящих блоков.
		size := binary.BigEndian.Uint32(b)
		if size == 0 || size > c.opts.Limits.MaxChunkBytes {
			return fmt.Errorf("недопустимый размер чанка: check=chunk_size chunk_size=%d max_chunk_bytes=%d: %w", size, c.opts.Limits.MaxChunkBytes, ErrLimit)
		}
		r.chunkSize = size
	case 2:
		if len(b) != 4 {
			return fmt.Errorf("неверная длина Abort: check=body_length expected_body_len=4: %w", ErrFraming)
		}
		// id содержит идентификатор потока блоков, незавершённое тело которого отменяется через
		// Abort.
		id := binary.BigEndian.Uint32(b)
		if id < 2 || id > 65599 {
			return fmt.Errorf("недопустимый поток Abort: check=abort_csid abort_csid=%d: %w", id, ErrFraming)
		}
		// s содержит состояние отменяемого потока блоков при наличии незавершённого сообщения.
		if s := r.streams[id]; s != nil && s.active {
			r.retained -= uint64(len(s.body))
			s.body = nil
			s.active = false
			s.received = 0
			c.setReadBuffer(r.retained)
		}
	case 3:
		if len(b) != 4 {
			return fmt.Errorf("неверная длина ACK: check=body_length expected_body_len=4: %w", ErrFraming)
		}
		c.acknowledge(binary.BigEndian.Uint32(b))
	case 4:
		if len(b) < 2 {
			return fmt.Errorf("отсутствует тип UserControl: check=event_header min_body_len=2: %w", ErrFraming)
		}
		// event содержит код события UserControl.
		event := binary.BigEndian.Uint16(b)
		// want задаёт требуемую длину тела выбранного события UserControl в байтах.
		want := 6
		if event == 3 {
			want = 10
		}
		if len(b) != want {
			return fmt.Errorf("неверная длина UserControl: check=body_length event=%d expected_body_len=%d: %w", event, want, ErrFraming)
		}
		switch event {
		case 0, 1, 2, 3, 4, 7:
		case 6:
			// response содержит копию PingRequest, преобразуемую в PingResponse.
			response := append([]byte(nil), b...)
			binary.BigEndian.PutUint16(response, 7)
			return c.queueControl(4, response)
		default:
			return fmt.Errorf("неподдерживаемое событие UserControl: check=event_type event=%d: %w", event, ErrFraming)
		}
	case 5:
		if len(b) != 4 {
			return fmt.Errorf("неверная длина WindowAcknowledgementSize: check=body_length expected_body_len=4: %w", ErrFraming)
		}
		r.window = binary.BigEndian.Uint32(b)
		if r.window == 0 {
			return fmt.Errorf("нулевое окно подтверждения: check=ack_window window=%d: %w", r.window, ErrFraming)
		}
		return r.ack()
	case 6:
		if len(b) != 5 {
			return fmt.Errorf("неверная длина SetPeerBandwidth: check=body_length expected_body_len=5: %w", ErrFraming)
		}
		if b[4] > 2 || binary.BigEndian.Uint32(b) == 0 {
			return fmt.Errorf("недопустимое окно или тип ограничения: check=peer_bandwidth window=%d limit_type=%d: %w", binary.BigEndian.Uint32(b), b[4], ErrFraming)
		}
		// window содержит объявленное окно SetPeerBandwidth. kind содержит тип его применения.
		window, kind := binary.BigEndian.Uint32(b), b[4]
		c.mu.Lock()
		// previous сохраняет прежнее эффективное окно для проверки необходимости управляющего
		// ответа.
		previous := c.peerWindow
		switch kind {
		case 0:
			c.peerWindow = window
		case 1:
			if c.peerWindow == 0 || window < c.peerWindow {
				c.peerWindow = window
			}
		case 2:
			if c.peerLimitKnown && c.peerLimitType == 0 {
				c.peerWindow = window
				kind = 0
			} else {
				c.mu.Unlock()
				return nil
			}
		}
		c.peerLimitType = kind
		c.peerLimitKnown = true
		// effective содержит новое действующее окно после применения типа ограничения.
		effective := c.peerWindow
		c.mu.Unlock()
		if previous != effective {
			// value содержит четырёхбайтовое тело WindowAcknowledgementSize с новым окном.
			var value [4]byte
			binary.BigEndian.PutUint32(value[:], effective)
			return c.queueControl(5, value[:])
		}
	default:
		return fmt.Errorf("неподдерживаемое управляющее сообщение: check=control_type: %w", ErrFraming)
	}
	return nil
}

// Проверяет AMF до декодирования и требует имя, конечный числовой transaction и аргументы команды,
// сохраняя контекст сообщения в ошибке.
func (c *OutboundConn) decodeCommand(m *rawmessage.Message, csid uint32) (outboundCommand, error) {
	// err сообщает о нарушении ограничений AMF до декодирования команды.
	if err := preflightAMF(m.Body, c.opts.Limits.amfLimits()); err != nil {
		return outboundCommand{}, outboundError(OutboundProtocol, outboundMessageError("amf_preflight", m, csid, err))
	}
	// a содержит декодированные значения AMF-команды. err сообщает об отказе декодирования.
	a, err := amf0.Unmarshal(m.Body)
	if err != nil {
		return outboundCommand{}, outboundError(OutboundProtocol, outboundMessageError("amf_decode", m, csid, fmt.Errorf("ошибка декодирования AMF: decoder_error=%v: %w", err, ErrFraming)))
	}
	if len(a) < 3 {
		return outboundCommand{}, outboundError(OutboundProtocol, outboundMessageError("command_shape", m, csid, fmt.Errorf("недостаточно элементов команды: values_len=%d min_values_len=3: %w", len(a), ErrFraming)))
	}
	// name содержит имя команды при строковом первом элементе. ok подтверждает этот тип.
	name, ok := a[0].(string)
	if !ok {
		return outboundCommand{}, outboundError(OutboundProtocol, outboundMessageError("command_name", m, csid, fmt.Errorf("неверный тип имени команды: value_type=%T: %w", a[0], ErrFraming)))
	}
	// txn содержит числовой идентификатор транзакции команды.
	txn, ok := a[1].(float64)
	if !ok || math.IsNaN(txn) || math.IsInf(txn, 0) {
		return outboundCommand{}, outboundError(OutboundProtocol, outboundMessageError("command_transaction", m, csid, fmt.Errorf("некорректная транзакция команды: command=%q value_type=%T transaction=%g: %w", name, a[1], txn, ErrFraming)))
	}
	return outboundCommand{name: name, transaction: txn, streamID: m.MessageStreamID, args: a[2:]}, nil
}

// Добавляет к причине фазу, тип, полный csid, MessageStreamID и длину входящего сообщения без потери
// errors.Is.
func outboundMessageError(phase string, m *rawmessage.Message, csid uint32, err error) error {
	return fmt.Errorf("ошибка входящего сообщения: phase=%s type=%d csid=%d stream_id=%d body_len=%d: %w", phase, m.Type, csid, m.MessageStreamID, len(m.Body), err)
}

// Описывает несоответствие ответа фазе запуска с именем команды, транзакцией, идентификаторами потока
// и доступным кодом статуса.
func outboundStartupMismatch(phase string, m outboundCommand, expectedStreamID uint32) error {
	// code содержит доступный код статуса для диагностики неожиданного ответа запуска.
	var code string
	if len(m.args) > 1 {
		// status содержит объект статуса ответа. ok подтверждает его тип AMF Object.
		if status, ok := m.args[1].(amf0.Object); ok {
			code, _ = status.GetString("code")
		}
	}
	return outboundError(OutboundProtocol, fmt.Errorf("неожиданный ответ при установке публикации: phase=%s command=%q transaction=%g stream_id=%d expected_stream_id=%d args_len=%d status_code=%q: %w", phase, m.name, m.transaction, m.streamID, expectedStreamID, len(m.args), code, ErrFraming))
}

// Проверяет точную двухэлементную форму аргументов и строковый code объекта статуса.
func outboundStatus(args amf0.Data, code string) bool {
	if len(args) != 2 {
		return false
	}
	// o содержит объект статуса. ok подтверждает ожидаемый тип второго аргумента.
	o, ok := args[1].(amf0.Object)
	if !ok {
		return false
	}
	// v содержит строковый код статуса для точного сравнения с ожидаемым.
	v, ok := o.GetString("code")
	return ok && v == code
}

// Извлекает code и description удалённого отказа в окончательный OutboundDenied, сохраняя доступные
// диагностические значения.
func outboundDenial(m outboundCommand) error {
	// e накапливает окончательный удалённый отказ с доступными кодом и описанием.
	e := &OutboundError{Kind: OutboundDenied}
	if len(m.args) > 1 {
		// o содержит объект удалённого статуса. ok подтверждает возможность извлечь его поля.
		if o, ok := m.args[1].(amf0.Object); ok {
			e.Code, _ = o.GetString("code")
			// description содержит текст удалённого отказа. ok показывает наличие строкового
			// описания.
			if description, ok := o.GetString("description"); ok {
				e.Cause = fmt.Errorf("%s", description)
			}
		}
	}
	return e
}
