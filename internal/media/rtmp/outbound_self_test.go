package rtmp

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/handshake"
	"github.com/bluenviron/gortmplib/pkg/rawmessage"
)

// Возвращает явные бюджеты исходящего транспорта с размером блоков 4096 и секундными сроками операций.
func transportSelfLimits() OutboundLimits {
	return OutboundLimits{MaxChunkStreams: 8, MaxMessageBytes: 1 << 20, MaxChunkBytes: 65536, MaxFragments: 20000, MaxControlBytes: 1024, MaxConfigBytes: 65536, MaxAMFBytes: 65536, MaxAMFDepth: 16, MaxAMFElements: 1024, ChunkSize: 4096, MaxControlPending: 16, MaxRetainedBytes: 32 << 20, DialTimeout: time.Second, TLSTimeout: time.Second, StartupTimeout: time.Second, MessageTimeout: time.Second, WriteTimeout: time.Second}
}

// Создаёт фактические H.264 High 128×128/30 и AAC-LC с двумя каналами и частотой 48 кГц из
// фиксированных avcC/ASC в заданной шкале.
func transportSelfConfigs(t *testing.T, base media.TimeBase) (media.CodecConfig, media.CodecConfig) {
	t.Helper()
	// h содержит фиксированный avcC контрольного H.264.
	h, _ := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	// vf содержит фактический видеоформат SPS. err сообщает об отказе разбора.
	vf, err := codec.H264Format(codec.VideoDescription{Headers: h, Width: 128, Height: 128})
	if err != nil {
		t.Fatal(err)
	}
	// v содержит видеоконфигурацию с переданной шкалой.
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: base, Video: vf}, h)
	if err != nil {
		t.Fatal(err)
	}
	// af содержит фактический аудиоформат ASC 48 кГц со стереоканалами.
	af, err := codec.AudioFormatASC([]byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	// a содержит аудиоконфигурацию с той же переданной шкалой.
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: base, Audio: af}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	return v, a
}

// Создаёт тестовый H.264 IDR, AV1 TU либо AAC AU по дорожке конфигурации с заданными метками и
// независимым флагом Packet.
func transportSelfPacket(t *testing.T, c media.CodecConfig, pts, dts int64, key bool) media.Packet {
	t.Helper()
	// payload выбирает синтетический H.264 IDR, AV1 TU либо полный AAC AU по конфигурации.
	payload := []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}
	if c.Info().Track == media.TrackAudio {
		payload = []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c}
	}
	if c.Info().Video.Codec == core.CodecAV1 {
		payload = testAV1TU()
	}
	// p содержит пакет с переданными метками и заявленным флагом Keyframe. err сообщает об отказе
	// создания.
	p, err := media.NewPacket(media.PacketInfo{PTS: pts, DTS: dts, DurationTicks: 1, Keyframe: key}, c, payload)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// transportSelfMemory моделирует транспорт с управляемыми чтением, частичной записью, ошибкой и историей
// сроков.
type transportSelfMemory struct {
	// input содержит подготовленные входящие байты либо nil для EOF.
	input *bytes.Reader
	// output накапливает фактически принятые тестовым Write байты.
	output bytes.Buffer
	// short при положительном значении ограничивает число принятых одним Write байтов.
	short int
	// failure задаёт ошибку, возвращаемую вместе с принятыми байтами Write.
	failure error
	// deadline сохраняет последний общий срок или срок записи.
	deadline time.Time
	// deadlines сохраняет историю сроков чтения и записи для проверки их неизменности.
	deadlines []time.Time
	// closed показывает, был ли вызван Close транспорта.
	closed bool
}

// Читает подготовленные байты соединения в памяти либо возвращает EOF при отсутствии ввода.
func (c *transportSelfMemory) Read(p []byte) (int, error) {
	if c.input == nil {
		return 0, io.EOF
	}
	return c.input.Read(p)
}

// Сохраняет весь буфер либо до short байтов при положительном short и возвращает их число вместе с
// заданной ошибкой для проверки частичного успеха.
func (c *transportSelfMemory) Write(p []byte) (int, error) {
	// n задаёт число байтов, принимаемых текущей тестовой записью.
	n := len(p)
	if c.short > 0 {
		n = min(n, c.short)
	}
	_, _ = c.output.Write(p[:n])
	return n, c.failure
}

// Отмечает закрытие соединения в памяти для проверки реакции транспорта.
func (c *transportSelfMemory) Close() error { c.closed = true; return nil }

// Возвращает пустой локальный TCP-адрес тестового соединения.
func (c *transportSelfMemory) LocalAddr() net.Addr { return &net.TCPAddr{} }

// Возвращает пустой TCP-адрес тестового получателя.
func (c *transportSelfMemory) RemoteAddr() net.Addr { return &net.TCPAddr{} }

// Сохраняет общий срок тестового соединения без реального ограничения ввода-вывода.
func (c *transportSelfMemory) SetDeadline(d time.Time) error { c.deadline = d; return nil }

// Добавляет срок чтения в историю для проверки его неизменности между фрагментами.
func (c *transportSelfMemory) SetReadDeadline(d time.Time) error {
	c.deadlines = append(c.deadlines, d)
	return nil
}

// Сохраняет текущий срок записи и добавляет его в историю для проверки единого срока сообщения.
func (c *transportSelfMemory) SetWriteDeadline(d time.Time) error {
	c.deadline = d
	c.deadlines = append(c.deadlines, d)
	return nil
}

// Создаёт OutboundConn без сетевого запуска над заданным соединением, с проверенным назначением
// H.264/AAC и MessageStreamID=0x01020304.
func transportSelfConn(t *testing.T, conn net.Conn) *OutboundConn {
	t.Helper()
	// v содержит контрольную конфигурацию H.264. a содержит контрольную конфигурацию AAC.
	v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
	// target содержит проверенное тестовое назначение без сетевого подключения. err сообщает об
	// отказе его построения.
	target, err := ResolveOutbound("rtmp://example.test/app", "synthetic-key", core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio})
	if err != nil {
		t.Fatal(err)
	}
	// c содержит локальное исходящее соединение с тестовыми допусками и каналами управления.
	c := &OutboundConn{conn: conn, ctx: context.Background(), opts: OutboundOptions{Limits: transportSelfLimits(), Admit: func(context.Context, OutboundOperation) (func(), error) {
		// Выдаёт тестовый допуск операции локального соединения с пустым действием освобождения.

		return func() {
			// Завершает тестовый допуск без изменения состояния.
		}, nil
	}}, target: target, done: make(chan struct{}), controls: make(chan outboundControl, 16), outboundChunkSize: 128}
	c.streamID.Store(0x01020304)
	return c
}

// Проверяет точное округление, общий сдвиг A/V, равные DTS, явное сворачивание сетевой метки и отказ
// регрессии, перестановки AAC и переполнения.
func TestOutboundExactQuantizationAndClock(t *testing.T) {
	// tc содержит метку, шкалу и независимое ожидание округления или отказа.
	for _, tc := range []struct {
		// ticks задаёт исходную знаковую метку в отсчётах.
		ticks int64
		// base задаёт длительность исходного отсчёта.
		base media.TimeBase
		// want задаёт ожидаемую округлённую метку в миллисекундах.
		want int64
		// bad задаёт ожидаемый отказ представления метки.
		bad bool
	}{
		{1, media.TimeBase{Numerator: 1, Denominator: 2000}, 1, false}, {-1, media.TimeBase{Numerator: 1, Denominator: 2000}, -1, false}, {1, media.TimeBase{Numerator: 1, Denominator: 3000}, 0, false}, {-1, media.TimeBase{Numerator: 1, Denominator: 3000}, 0, false},
		{math.MaxInt64, media.TimeBase{Numerator: 1, Denominator: 1000}, math.MaxInt64, false}, {math.MinInt64, media.TimeBase{Numerator: 1, Denominator: 1000}, math.MinInt64, false}, {math.MaxInt64, media.TimeBase{Numerator: 1, Denominator: 1}, 0, true}, {1, media.TimeBase{}, 0, true},
	} {
		// got содержит округлённую метку. err сообщает о невозможности представления или
		// неверной шкале.
		got, err := QuantizeOutboundMilliseconds(tc.ticks, tc.base)
		if (err != nil) != tc.bad || !tc.bad && got != tc.want {
			t.Fatalf("Q(%d,%+v)=%d %v", tc.ticks, tc.base, got, err)
		}
	}
	// v содержит видеоконфигурацию со шкалой 1/2000. a содержит аудиоконфигурацию с той же шкалой.
	v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 2000})
	// clock сохраняет общее начало для проверки округления обеих дорожек.
	var clock outboundClock
	// stamp содержит сетевой DTS относительно общего начала. cts содержит знаковое смещение PTS/DTS.
	// err сообщает об отказе проекции.
	stamp, cts, err := clock.project(transportSelfPacket(t, v, 1, -1, false), true)
	if err != nil || stamp != 0 || cts != 2 || clock.origin != -1 {
		t.Fatalf("независимые Q: %d %d %v", stamp, cts, err)
	}
	stamp, cts, err = clock.project(transportSelfPacket(t, v, 1, -1, false), true)
	if err != nil || stamp != 0 || cts != 2 {
		t.Fatalf("равный DTS изменён: %d %d %v", stamp, cts, err)
	}
	stamp, _, err = clock.project(transportSelfPacket(t, a, 1, 1, false), false)
	if err != nil || stamp != 2 {
		t.Fatalf("общий A/V origin: %d %v", stamp, err)
	}
	if _, _, err = clock.project(transportSelfPacket(t, a, 2, 1, false), false); !IsOutboundTerminal(err) {
		t.Fatal("AAC reorder принят")
	}
	// wrap сохраняет логические метки при переходе через границу сетевого uint32.
	var wrap outboundClock
	v, a = transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
	_ = a
	// ticks содержит очередную логическую метку, приближающуюся к сетевому сворачиванию.
	for _, ticks := range []int64{-10, (1 << 31) - 11, (1 << 32) - 12, (1 << 32) - 9} {
		stamp, _, err = wrap.project(transportSelfPacket(t, v, ticks, ticks, false), true)
		if err != nil {
			t.Fatal(err)
		}
	}
	if stamp != 1 {
		t.Fatalf("modulo32: %d", stamp)
	}
	if _, _, err = wrap.project(transportSelfPacket(t, v, (1<<32)-10, (1<<32)-10, false), true); !IsOutboundTerminal(err) {
		t.Fatal("DTS regression принят")
	}
	if _, err = outboundSubtract(math.MaxInt64, -1); !IsOutboundTerminal(err) {
		t.Fatal("overflow принят")
	}
	if _, err = outboundSubtract(math.MinInt64, 1); !IsOutboundTerminal(err) {
		t.Fatal("underflow принят")
	}
}

// Проверяет точность экранирования пути, строки запроса и отдельного ключа, IPv6/порты, ограничения
// команд и окончательность структурных ошибок до сети.
func TestOutboundCustomTargetAndPreflight(t *testing.T) {
	// v содержит допустимую H.264-конфигурацию. a содержит допустимую AAC-конфигурацию.
	v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
	// profile объединяет фактические форматы для проверки назначения.
	profile := core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio}
	// target содержит проверенный IPv6 RTMPS-адрес с точным приложением и отдельным ключом. err
	// сообщает об отказе проверки.
	target, err := ResolveOutbound("rtmps://[::1]:444/a%2Fb/../c?x=%2F&x=2", "exact/key?#", profile)
	if err != nil || target.app != "a%2Fb/../c?x=%2F&x=2" || target.key != "exact/key?#" || target.address != "[::1]:444" {
		t.Fatalf("точный target: %+v %v", target, err)
	}
	if err = target.Validate(profile, transportSelfLimits()); err != nil {
		t.Fatal(err)
	}
	// endpoint содержит очередной недопустимый URL назначения.
	for _, endpoint := range []string{"rtmp://host/app#", "rtmp://host/app#key", "rtmp://user:pass@host/app", "http://host/app", "rtmp://host:/app", "rtmp://host:0/app", "rtmp://host:65536/app", "rtmp://host/", "rtmp://[invalid]/app", "rtmp:///app"} {
		if _, err = ResolveOutbound(endpoint, "key", profile); !IsOutboundTerminal(err) {
			t.Fatalf("некорректный endpoint принят: %s", endpoint)
		}
	}
	// endpoint содержит допустимый URL RTMP либо RTMPS без явного порта.
	for _, endpoint := range []string{"rtmp://host/app", "rtmps://host/app"} {
		// got содержит разрешённое назначение со стандартным портом. e сообщает об отказе
		// проверки.
		got, e := ResolveOutbound(endpoint, "key", profile)
		if e != nil || got.address != "host:"+map[bool]string{true: "443", false: "1935"}[got.tls] {
			t.Fatalf("protocol port: %v %v", got, e)
		}
	}
	if _, err = ResolveOutbound("rtmp://host/app", strings.Repeat("x", 65536), profile); !IsOutboundTerminal(err) {
		t.Fatal("AMF string overflow принят")
	}
	// bad содержит изменяемую копию формата для отрицательных проверок.
	bad := profile
	bad.Video.BitDepth = 10
	if _, err = ResolveOutbound("rtmp://host/app", "key", bad); !IsOutboundTerminal(err) {
		t.Fatal("неподдержанный профиль принят")
	}
	bad = profile
	bad.Video.Width++
	if err = target.Validate(bad, transportSelfLimits()); !IsOutboundTerminal(err) {
		t.Fatal("resolver изменил профиль")
	}
	if err = (OutboundTarget{}).Validate(profile, transportSelfLimits()); !IsOutboundTerminal(err) {
		t.Fatal("zero target принят")
	}
	// l задаёт малый бюджет AMF, который должен остановить назначение до сети.
	l := transportSelfLimits()
	l.MaxAMFBytes = 16
	if err = target.Validate(profile, l); !IsOutboundTerminal(err) {
		t.Fatal("AMF budget пропущен до dial")
	}
	if IsOutboundTerminal(io.EOF) || IsOutboundTerminal(context.DeadlineExceeded) || !IsOutboundTerminal(outboundError(OutboundTiming, codec.ErrTiming)) {
		t.Fatal("неверная классификация")
	}
}

// Проверяет независимый байтовый эталон fmt0/fmt3 с расширенной меткой, единый срок, положительные
// частичные Write при ошибке и насыщение счётчика.
func TestOutboundFramingIndependentByteExpectationAndPartialWrites(t *testing.T) {
	// m моделирует транспорт с частичными записями по два байта.
	m := &transportSelfMemory{short: 2}
	// c содержит локальный отправитель с размером блоков три байта.
	c := transportSelfConn(t, m)
	c.outboundChunkSize = 3
	// observed суммирует положительные результаты OnWrite.
	var observed int
	c.opts.OnWrite = func(n int) {
		// Суммирует фактически записанные байты для сравнения с независимо заданным обрамлением сообщения.
		observed += n
	}
	// err сообщает об отказе записи сообщения с расширенной меткой и продолжениями.
	if err := c.writeMessage(context.Background(), 6, 9, 0x01020304, 0x01000002, []byte{1, 2, 3, 4, 5, 6, 7}); err != nil {
		t.Fatal(err)
	}
	// want содержит независимый побайтовый эталон всего RTMP-обрамления.
	want := []byte{6, 255, 255, 255, 0, 0, 7, 9, 4, 3, 2, 1, 1, 0, 0, 2, 1, 2, 3, 0xc6, 1, 0, 0, 2, 4, 5, 6, 0xc6, 1, 0, 0, 2, 7}
	if !bytes.Equal(m.output.Bytes(), want) || observed != len(want) || c.Snapshot().WrittenBytes != uint64(len(want)) {
		t.Fatalf("wire=%x want=%x bytes=%d", m.output.Bytes(), want, observed)
	}
	if len(m.deadlines) != 1 || m.deadline.IsZero() {
		t.Fatal("срок полной записи обновлялся при short Write")
	}
	m = &transportSelfMemory{short: 2, failure: io.ErrClosedPipe}
	c = transportSelfConn(t, m)
	observed = 0
	c.opts.OnWrite = func(n int) {
		// Суммирует положительные результаты записей, включая частичную запись с ошибкой.
		observed += n
	}
	// err содержит результат частичной записи с управляемой ошибкой.
	if err := c.writeMessage(context.Background(), 3, 20, 0, 0, []byte{5}); !errors.Is(err, io.ErrClosedPipe) || observed != 2 || c.Snapshot().WrittenBytes != 2 || !m.closed {
		t.Fatalf("partial error: %d %v", observed, err)
	}
	c = transportSelfConn(t, &transportSelfMemory{})
	c.stats.WrittenBytes = math.MaxUint64 - 1
	// err содержит результат записи при насыщении WrittenBytes.
	if err := c.writeMessage(context.Background(), 3, 20, 0, 0, []byte{5}); err != nil || !c.Snapshot().Saturated || c.Snapshot().WrittenBytes != math.MaxUint64 {
		t.Fatalf("saturation: %+v %v", c.Snapshot(), err)
	}
}

// transportSelfFrame хранит результат независимого разбора исходящего fmt0/fmt3-сообщения.
type transportSelfFrame struct {
	// typ содержит тип RTMP-сообщения.
	typ byte
	// chunkStream содержит короткий идентификатор потока блоков.
	chunkStream byte
	// stream содержит MessageStreamID. stamp содержит абсолютную сетевую метку в миллисекундах.
	stream, stamp uint32
	// body содержит собранное тело сообщения.
	body []byte
}

// transportSelfReadFrame независимо сверяет байтовое представление fmt0/fmt3.
func transportSelfReadFrame(r io.Reader, chunk int) (transportSelfFrame, error) {
	// h служит буфером первого заголовка и расширенной метки.
	var h [16]byte
	// err сообщает об отказе чтения полного fmt0-заголовка.
	if _, err := io.ReadFull(r, h[:12]); err != nil {
		return transportSelfFrame{}, err
	}
	if h[0]>>6 != 0 {
		return transportSelfFrame{}, ErrFraming
	}
	// stamp содержит обычную либо расширенную абсолютную метку сообщения.
	stamp := uint32(h[1])<<16 | uint32(h[2])<<8 | uint32(h[3])
	// extended показывает наличие четырёхбайтовой расширенной метки в первом блоке и продолжениях.
	extended := stamp == 0xffffff
	if extended {
		// err сообщает об отказе чтения расширенной метки.
		if _, err := io.ReadFull(r, h[12:16]); err != nil {
			return transportSelfFrame{}, err
		}
		stamp = binary.BigEndian.Uint32(h[12:16])
	}
	// n содержит объявленный размер тела сообщения в байтах.
	n := int(h[4])<<16 | int(h[5])<<8 | int(h[6])
	if n > 1<<20 {
		return transportSelfFrame{}, ErrLimit
	}
	// f накапливает поля и тело независимо читаемого сообщения.
	f := transportSelfFrame{typ: h[7], chunkStream: h[0] & 63, stream: binary.LittleEndian.Uint32(h[8:12]), stamp: stamp, body: make([]byte, n)}
	// at задаёт число уже прочитанных байтов тела.
	for at := 0; at < n; {
		// amount задаёт размер очередного читаемого фрагмента.
		amount := min(chunk, n-at)
		// err сообщает об отказе чтения фрагмента тела.
		if _, err := io.ReadFull(r, f.body[at:at+amount]); err != nil {
			return f, err
		}
		at += amount
		if at < n {
			// continuation принимает заголовок fmt3 и возможную расширенную метку.
			var continuation [5]byte
			// count задаёт длину заголовка продолжения: один либо пять байтов.
			count := 1
			if extended {
				count = 5
			}
			// err сообщает об отказе чтения заголовка продолжения.
			if _, err := io.ReadFull(r, continuation[:count]); err != nil {
				return f, err
			}
			if continuation[0] != 0xc0|h[0] || extended && binary.BigEndian.Uint32(continuation[1:]) != stamp {
				return f, ErrFraming
			}
		}
	}
	return f, nil
}

// Проверяет исходные AV1/H.264/AAC-заголовки и AU, определение независимости по содержимому и
// сохранение общего начала при повторной передаче заголовков.
func TestOutboundExactMediaHeadersAndRecoveryOrigin(t *testing.T) {
	// av1 выбирает проверку AV1 вместо H.264.
	for _, av1 := range []bool{false, true} {
		t.Run(map[bool]string{false: "h264", true: "av1"}[av1], func(t *testing.T) {
			// Для выбранного H.264 либо AV1 проверяет исходные заголовки и AU, независимость по содержимому и
			// сохранение начала отсчёта при восстановлении.

			// m содержит транспорт в памяти с сохранением всех исходящих байтов.
			m := &transportSelfMemory{}
			// c содержит локальный отправитель контрольного формата.
			c := transportSelfConn(t, m)
			// v содержит видеоконфигурацию выбранного кодека. a содержит AAC-конфигурацию со
			// шкалой 1/2000.
			v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 2000})
			if av1 {
				// vf содержит фактически разобранный формат AV1. key подтверждает
				// независимость TU. err сообщает об отказе разбора.
				vf, key, err := codec.InspectAV1TU(testAV1Config(), testAV1TU(), core.FrameRate{Numerator: 30, Denominator: 1})
				if err != nil || !key {
					t.Fatal(err)
				}
				// vi содержит описание видео для замены H.264 на фактический AV1.
				vi := v.Info()
				vi.Video = vf
				v, err = media.NewCodecConfig(vi, testAV1Config())
				if err != nil {
					t.Fatal(err)
				}
				c.target.profile.Video = vf
			}
			// err сообщает об отказе принятия исходных заголовков без немедленной сетевой
			// записи.
			if err := c.WriteHeaders(context.Background(), v, a); err != nil {
				t.Fatal(err)
			}
			if m.output.Len() != 0 {
				t.Fatal("headers выданы до проверки независимого AU")
			}
			// pts задаёт исходное время показа. dts задаёт исходное время декодирования
			// первого кадра.
			pts, dts := int64(1), int64(-1)
			if av1 {
				pts = -1
			}
			// result подтверждает полную запись первой AU и её фактическую независимость.
			// err сообщает об отказе записи.
			result, err := c.WritePacket(context.Background(), transportSelfPacket(t, v, pts, dts, false))
			if err != nil || !result.Independent {
				t.Fatalf("флаг заменил проверку: %+v %v", result, err)
			}
			if _, err = c.WritePacket(context.Background(), transportSelfPacket(t, a, 1, 1, false)); err != nil {
				t.Fatal(err)
			}
			// r читает сохранённые исходящие байты для независимой проверки обрамления.
			r := bytes.NewReader(m.output.Bytes())
			// frames принимает пять сообщений: метаданные, две конфигурации и две AU.
			frames := make([]transportSelfFrame, 5)
			// i задаёт позицию независимо разбираемого сообщения.
			for i := range frames {
				frames[i], err = transportSelfReadFrame(r, 128)
				if err != nil {
					t.Fatal(err)
				}
			}
			if frames[0].typ != 18 || frames[1].typ != 9 || frames[2].typ != 8 || frames[3].stream != 0x01020304 || frames[4].stamp != 2 {
				t.Fatalf("track/header/time: %+v", frames)
			}
			// videoPrefix задаёт ожидаемый префикс конфигурации выбранного видеокодека.
			videoPrefix := []byte{0x17, 0, 0, 0, 0}
			// packetPrefix задаёт ожидаемый префикс первой видеонагрузки с её CTS.
			packetPrefix := []byte{0x17, 1, 0, 0, 2}
			if av1 {
				videoPrefix = []byte{0x90, 'a', 'v', '0', '1'}
				packetPrefix = []byte{0x91, 'a', 'v', '0', '1'}
			}
			if !bytes.Equal(frames[1].body, append(videoPrefix, v.Headers()...)) || !bytes.Equal(frames[2].body, append([]byte{0xaf, 0}, a.Headers()...)) || !bytes.Equal(frames[3].body, append(packetPrefix, transportSelfPacket(t, v, pts, dts, false).Payload()...)) || !bytes.Equal(frames[4].body, append([]byte{0xaf, 1}, transportSelfPacket(t, a, 1, 1, false).Payload()...)) {
				t.Fatal("исходные headers/AU изменены")
			}
			// before сохраняет длину выхода до повторных заголовков для выделения сообщений
			// восстановления.
			before := m.output.Len()
			if err = c.WriteHeaders(context.Background(), v, a); err != nil {
				t.Fatal(err)
			}
			if _, err = c.WritePacket(context.Background(), transportSelfPacket(t, v, 3, 3, false)); err != nil {
				t.Fatal(err)
			}
			// next читает только байты восстановления после повторной передачи заголовков.
			next := bytes.NewReader(m.output.Bytes()[before:])
			for range 4 {
				// f содержит очередное сообщение восстановления. e сообщает об отказе
				// его независимого разбора.
				f, e := transportSelfReadFrame(next, 128)
				if e != nil || f.stamp != 3 {
					t.Fatalf("recovery изменил origin: %+v %v", f, e)
				}
			}
		})
	}
}

// Проверяет наследование входящих меток fmt0/1/2/3, отказ неизвестного состояния и чрезмерной длины и
// освобождение тела по Abort.
func TestOutboundInboundInheritanceBoundsAndAbort(t *testing.T) {
	// data содержит независимую последовательность fmt0/fmt1/fmt2/fmt3 с ожидаемыми метками.
	data := []byte{4, 0, 0, 10, 0, 0, 1, 20, 4, 3, 2, 1, 5, 0x44, 0, 0, 5, 0, 0, 1, 20, 5, 0x84, 0, 0, 2, 5, 0xc4, 5}
	// c содержит соединение над подготовленными входящими байтами.
	c := transportSelfConn(t, &transportSelfMemory{input: bytes.NewReader(data)})
	// r собирает входящие сообщения при проверке наследования заголовков.
	r := outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 128}
	// want задаёт очередную ожидаемую абсолютную метку в миллисекундах.
	for _, want := range []uint32{10, 15, 17, 19} {
		// m содержит собранное сообщение. err сообщает об отказе чтения.
		m, err := r.next()
		if err != nil || m.MessageStreamID != 0x01020304 || m.Timestamp != time.Duration(want)*time.Millisecond || !bytes.Equal(m.Body, []byte{5}) {
			t.Fatalf("inheritance: %+v %v", m, err)
		}
	}
	// data содержит неизвестное наследование либо чрезмерно объявленное тело.
	for _, data := range [][]byte{{0xc4}, {4, 0, 0, 0, 0x10, 0, 1, 20, 0, 0, 0, 0}} {
		c = transportSelfConn(t, &transportSelfMemory{input: bytes.NewReader(data)})
		r = outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 128}
		// err содержит результат чтения недопустимой последовательности.
		if _, err := r.next(); err == nil {
			t.Fatal("неизвестное состояние/oversize принято")
		}
	}
	// partial содержит первый блок незавершённого двухбайтового сообщения.
	partial := []byte{4, 0, 0, 0, 0, 0, 2, 20, 0, 0, 0, 0, 5}
	c = transportSelfConn(t, &transportSelfMemory{input: bytes.NewReader(partial)})
	r = outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 1}
	// m содержит возможное полное сообщение. err содержит результат чтения первого неполного блока.
	if m, err := r.chunk(); err != nil || m != nil || r.retained != 2 {
		t.Fatal(m, err, r.retained)
	}
	// err содержит результат Abort и освобождения удержанного тела.
	if err := c.control(&r, &rawmessage.Message{ChunkStreamID: 2, Type: 2, Body: []byte{0, 0, 0, 4}}); err != nil || r.retained != 0 || r.streams[4].active {
		t.Fatalf("Abort: %v", err)
	}
}

// Передаёт независимую AMF-команду блоками по 128 байтов с fmt0/fmt3 и заданным MessageStreamID.
func transportSelfWriteAMF(conn net.Conn, data amf0.Data, stream uint32) error {
	// b содержит сериализованную независимую AMF-команду. err сообщает об отказе кодирования.
	b, err := data.Marshal()
	if err != nil {
		return err
	}
	// h содержит fmt0-заголовок команды с отдельным MessageStreamID.
	h := []byte{3, 0, 0, 0, byte(len(b) >> 16), byte(len(b) >> 8), byte(len(b)), 20, byte(stream), byte(stream >> 8), byte(stream >> 16), byte(stream >> 24)}
	// at задаёт начало очередного записываемого фрагмента AMF-тела.
	for at := 0; at < len(b); {
		// part задаёт длину текущего фрагмента при размере блоков 128.
		part := min(128, len(b)-at)
		// header выбирает первый fmt0-заголовок либо короткое продолжение fmt3.
		header := h
		if at > 0 {
			header = []byte{0xc3}
		}
		if _, err = conn.Write(header); err != nil {
			return err
		}
		if _, err = conn.Write(b[at : at+part]); err != nil {
			return err
		}
		at += part
	}
	return nil
}

// Запускает независимый тестовый сценарий приёма connect/createStream/publish без объявления окна
// получателя.
func transportSelfStartup(conn net.Conn, key string) error {
	return transportSelfStartupWindow(conn, key, 0)
}

// transportSelfCountingConn считает прочитанные байты независимого получателя для формирования ACK.
type transportSelfCountingConn struct {
	// Conn содержит исходное соединение тестового получателя.
	net.Conn
	// received накапливает прочитанные байты по модулю uint32, включая согласование.
	received uint32
}

// Считает фактически прочитанные байты тестового получателя для ACK, включая согласование RTMP.
func (c *transportSelfCountingConn) Read(p []byte) (int, error) {
	// n содержит фактически прочитанное число байтов. err сообщает об отказе чтения.
	n, err := c.Conn.Read(p)
	c.received += uint32(n)
	return n, err
}

// Управляющие ответы могут прийти между любыми командами установки соединения.
func transportSelfReadStartupFrame(conn io.Reader, chunk int) (transportSelfFrame, error) {
	for {
		// f содержит очередное сообщение установки либо управления. err сообщает об отказе
		// чтения.
		f, err := transportSelfReadFrame(conn, chunk)
		if err != nil {
			return f, err
		}
		if f.typ != 3 && f.typ != 5 {
			return f, nil
		}
		if f.chunkStream != 2 || f.stream != 0 || len(f.body) != 4 || f.typ == 5 && binary.BigEndian.Uint32(f.body) == 0 {
			return f, ErrFraming
		}
	}
}

// Имитирует получателя RTMP, при необходимости объявляя окно и ACK после connect; между командами
// допускает корректные управляющие ответы.
func transportSelfStartupWindow(raw net.Conn, key string, window uint32) error {
	// conn оборачивает сокет получателя счётчиком байтов согласования и команд.
	conn := &transportSelfCountingConn{Conn: raw}
	// err сообщает об отказе серверного RTMP-согласования.
	if _, _, err := handshake.DoServer(conn, true); err != nil {
		return err
	}
	// f содержит первое сообщение connect. err сообщает об отказе чтения.
	f, err := transportSelfReadFrame(conn, 128)
	if err != nil {
		return err
	}
	// data содержит декодированные значения очередной команды установки.
	data, err := amf0.Unmarshal(f.body)
	if err != nil || len(data) == 0 || data[0] != "connect" {
		return ErrFraming
	}
	if window != 0 {
		// body служит буфером SetPeerBandwidth и ACK тестового получателя.
		var body [5]byte
		binary.BigEndian.PutUint32(body[:4], window)
		// err сообщает об отказе отправки объявленного окна получателя.
		if err := transportSelfWriteControl(conn, 6, body[:]); err != nil {
			return err
		}
		binary.BigEndian.PutUint32(body[:4], conn.received)
		// err сообщает об отказе отправки ACK фактически прочитанных байтов.
		if err := transportSelfWriteControl(conn, 3, body[:4]); err != nil {
			return err
		}
	}
	if err = transportSelfWriteAMF(conn, amf0.Data{"_result", float64(1), nil, amf0.Object{{Key: "code", Value: "NetConnection.Connect.Success"}}}, 0); err != nil {
		return err
	}
	f, err = transportSelfReadStartupFrame(conn, 128)
	if err != nil {
		return err
	}
	if f.typ != 1 || f.chunkStream != 2 || f.stream != 0 || len(f.body) != 4 {
		return ErrFraming
	}
	// size содержит объявленный клиентом размер последующих исходящих блоков.
	size := int(binary.BigEndian.Uint32(f.body))
	if size == 0 || size > 65536 {
		return ErrFraming
	}
	f, err = transportSelfReadStartupFrame(conn, size)
	if err != nil {
		return err
	}
	data, err = amf0.Unmarshal(f.body)
	if err != nil || f.typ != 20 || len(data) == 0 || data[0] != "createStream" {
		return ErrFraming
	}
	if err = transportSelfWriteAMF(conn, amf0.Data{"_result", float64(2), nil, float64(0x01020304)}, 0); err != nil {
		return err
	}
	f, err = transportSelfReadStartupFrame(conn, size)
	if err != nil {
		return err
	}
	data, err = amf0.Unmarshal(f.body)
	if err != nil || f.typ != 20 || f.stream != 0x01020304 || len(data) != 5 || data[0] != "publish" || data[3] != key {
		return ErrFraming
	}
	return transportSelfWriteAMF(conn, amf0.Data{"onStatus", float64(0), nil, amf0.Object{{Key: "code", Value: "NetStream.Publish.Start"}}}, 0x01020304)
}

// Отправляет независимое управляющее сообщение на csid=2 и MessageStreamID=0 с переданным телом.
func transportSelfWriteControl(conn net.Conn, typ byte, body []byte) error {
	// h содержит независимый заголовок управляющего сообщения с нулевым MessageStreamID.
	h := []byte{2, 0, 0, 0, 0, 0, byte(len(body)), typ, 0, 0, 0, 0}
	// err сообщает об отказе записи заголовка управления.
	if _, err := conn.Write(h); err != nil {
		return err
	}
	// err сообщает об отказе записи тела управления.
	_, err := conn.Write(body)
	return err
}

// Проверяет включение 3073 байтов согласования в отправленные, прочитанные и подтверждённые объёмы при
// объявленном окне 256.
func TestOutboundHandshakeBytesArePartOfAcknowledgementSequence(t *testing.T) {
	// left содержит клиентский конец net.Pipe. right содержит конец независимого получателя.
	left, right := net.Pipe()
	// peerJoined закрывается после выхода горутины независимого получателя.
	peerJoined := make(chan struct{})
	defer func() {
		// Закрывает оба конца net.Pipe и дожидается завершения тестового принимающего узла.
		_ = left.Close()
		_ = right.Close()
		<-peerJoined
	}()
	// peerDone передаёт результат установки соединения тестовым получателем.
	peerDone := make(chan error, 1)
	go func() {
		// Выполняет установку с окном 256, сообщает результат и при успехе читает до закрытия, закрывая
		// peerJoined при выходе.

		defer close(peerJoined)
		defer func() {
			// Закрывает принимающий конец net.Pipe при выходе из сценария установки.
			_ = right.Close()
		}()
		// err содержит результат установки с окном подтверждения 256 байтов.
		err := transportSelfStartupWindow(right, "key", 256)
		peerDone <- err
		if err == nil {
			_, _ = io.Copy(io.Discard, right)
		}
	}()
	// v содержит H.264-конфигурацию. a содержит AAC-конфигурацию ожидаемого назначения.
	v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
	// target содержит проверенное назначение для подключения через net.Pipe. err сообщает об отказе
	// проверки.
	target, err := ResolveOutbound("rtmp://example.test/app", "key", core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio})
	if err != nil {
		t.Fatal(err)
	}
	// c содержит соединение после реального RTMP-согласования с тестовым получателем.
	c, err := DialOutbound(context.Background(), target, OutboundOptions{Limits: transportSelfLimits(), DialContext: func(context.Context, string, string) (net.Conn, error) {
		// Возвращает клиентский конец net.Pipe для проверки учёта байтов согласования.
		return left, nil
	}, Admit: func(context.Context, OutboundOperation) (func(), error) {
		// Выдаёт тестовый допуск с пустым действием освобождения при проверке последовательности ACK.

		return func() {
			// Завершает тестовый допуск без изменения состояния.
		}, nil
	}})
	// peerErr содержит исход установки принимающей стороны.
	peerErr := <-peerDone
	if err != nil {
		t.Fatalf("установка соединения: client=%v peer=%v", err, peerErr)
	}
	if peerErr != nil {
		t.Fatal(peerErr)
	}
	_ = c.Close()
	// ctx ограничивает ожидание присоединения одной секундой. cancel освобождает срок.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = c.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if c.peerAcknowledged < 3073 || c.stats.WrittenBytes < 3073 || c.handshakeRead != 3073 || c.peerWindow != 256 {
		t.Fatalf("неверное начало ACK: sent=%d ack=%d read=%d window=%d", c.stats.WrittenBytes, c.peerAcknowledged, c.handshakeRead, c.peerWindow)
	}
}

// Проверяет запуск с точным ключом, допустимую тишину дольше MessageTimeout, передачу AU и завершение
// всех допусков операций через Close/Wait.
func TestOutboundStartupQuietControlAndJoinedOperations(t *testing.T) {
	// left содержит клиентский конец net.Pipe. right содержит конец тестового получателя.
	left, right := net.Pipe()
	defer func() {
		// Закрывает принимающий конец net.Pipe после проверки управляющих сообщений.
		_ = right.Close()
	}()
	// ready передаёт результат установки. release разрешает получателю отправить ping после периода
	// тишины.
	ready, release := make(chan error, 1), make(chan struct{})
	// serverDone закрывается после выхода тестового получателя.
	serverDone := make(chan struct{})
	go func() {
		// Выполняет установку, ждёт разрешения отправить PingRequest, проверяет ответ и читает до закрытия,
		// закрывая serverDone при выходе.

		defer close(serverDone)
		ready <- transportSelfStartup(right, "exact/key?#")
		<-release
		_, _ = right.Write([]byte{2, 0, 0, 0, 0, 0, 6, 4, 0, 0, 0, 0, 0, 6, 1, 2, 3, 4})
		for {
			// f содержит очередное клиентское сообщение до PingResponse. err сообщает об
			// отказе чтения.
			f, err := transportSelfReadFrame(right, 4096)
			if err != nil {
				return
			}
			if f.typ == 4 {
				if !bytes.Equal(f.body, []byte{0, 7, 1, 2, 3, 4}) {
					return
				}
				break
			}
		}
		_, _ = io.Copy(io.Discard, right)
	}()
	// v содержит конфигурацию H.264. a содержит конфигурацию AAC.
	v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
	// target содержит проверенный точный URL и ключ тестовой публикации. err сообщает об отказе
	// проверки.
	target, err := ResolveOutbound("rtmp://example.test/a%2Fb?x=1", "exact/key?#", core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio})
	if err != nil {
		t.Fatal(err)
	}
	// ctx задаёт общий срок жизни исходящего соединения. cancel освобождает его по завершении теста.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// l задаёт малый MessageTimeout для проверки допустимого простоя.
	l := transportSelfLimits()
	l.MessageTimeout = 20 * time.Millisecond
	// active атомарно считает действующие допуски каждого вида операции.
	var active [4]atomic.Int32
	// written атомарно накапливает фактически записанные байты через OnWrite.
	var written atomic.Int64
	// options задаёт соединение net.Pipe, учёт допусков и наблюдение записей.
	options := OutboundOptions{Limits: l, DialContext: func(context.Context, string, string) (net.Conn, error) {
		// Возвращает клиентский конец net.Pipe для проверки простоя и завершения операций.
		return left, nil
	}, Admit: func(_ context.Context, k OutboundOperation) (func(), error) {
		// Учитывает занятие слота выбранной операции, сообщает повторное занятие и возвращает действие
		// освобождения.

		if active[k].Add(1) != 1 {
			t.Errorf("конкурентный admission %d", k)
		}
		return func() {
			// Уменьшает счётчик активного допуска выбранной операции при её завершении.
			active[k].Add(-1)
		}, nil
	}, OnWrite: func(n int) {
		// Атомарно накапливает положительные результаты записей для проверки фактической передачи.
		written.Add(int64(n))
	}}
	// c содержит установленное исходящее соединение.
	c, err := DialOutbound(ctx, target, options)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	if err = <-ready; err != nil {
		t.Fatal(err)
	}
	// Тишина длиннее MessageTimeout допустима; таймер здесь задаёт проверяемую границу.
	select {
	case <-c.Done():
		t.Fatalf("idle control silence: %v", c.Err())
	case <-time.After(3 * l.MessageTimeout):
	}
	close(release)
	if err = c.WriteHeaders(ctx, v, a); err != nil {
		t.Fatal(err)
	}
	if _, err = c.WritePacket(ctx, transportSelfPacket(t, v, 0, 0, false)); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	// joinCtx ограничивает ожидание всех операций одной секундой. stop освобождает срок.
	joinCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err = c.Wait(joinCtx); err != nil {
		t.Fatal(err)
	}
	if active[OutboundEstablishment].Load() != 0 || active[OutboundRead].Load() != 0 || active[OutboundWrite].Load() != 0 || written.Load() == 0 {
		t.Fatalf("операции не завершены: %d %d %d", active[OutboundEstablishment].Load(), active[OutboundRead].Load(), active[OutboundWrite].Load())
	}
	_ = right.Close()
	<-serverDone
}

// Проверяет закрытие поздно возвращённого сокета до завершения допуска и отсутствие DialContext при
// отказе допуска установления.
func TestOutboundLateDialAndDeniedAdmissionAreNetworkFree(t *testing.T) {
	// v содержит допустимую видеоконфигурацию. a содержит допустимую аудиоконфигурацию.
	v, a := transportSelfConfigs(t, media.TimeBase{Numerator: 1, Denominator: 1000})
	// target содержит проверенное назначение до сетевых действий.
	target, _ := ResolveOutbound("rtmp://example.test/app", "key", core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio})
	// left содержит поздно возвращаемый клиентский сокет. right содержит противоположный конец для
	// проверки закрытия.
	left, right := net.Pipe()
	defer func() {
		// Закрывает принимающий конец net.Pipe после проверки позднего подключения.
		_ = right.Close()
	}()
	// l задаёт короткий срок TCP-подключения.
	l := transportSelfLimits()
	l.DialTimeout = time.Millisecond
	// finished отмечает вызов освобождения допуска установки после позднего результата.
	var finished bool
	// err содержит отказ установки при сокете, возвращённом после истечения срока.
	_, err := DialOutbound(context.Background(), target, OutboundOptions{Limits: l, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		// Возвращает клиентский сокет после завершения контекста подключения, моделируя поздний результат.
		<-ctx.Done()
		return left, nil
	}, Admit: func(context.Context, OutboundOperation) (func(), error) {
		// Выдаёт допуск с действием, отмечающим его завершение при отказе позднего подключения.

		return func() {
			// Отмечает завершение допуска установки соединения.
			finished = true
		}, nil
	}})
	if !errors.Is(err, context.DeadlineExceeded) || !finished {
		t.Fatalf("late dial: %v finish=%v", err, finished)
	}
	if _, err = right.Write([]byte{1}); err == nil {
		t.Fatal("поздний socket не закрыт")
	}
	// called отмечает неожиданный вызов DialContext при отклонённом допуске.
	called := false
	_, err = DialOutbound(context.Background(), target, OutboundOptions{Limits: l, DialContext: func(context.Context, string, string) (net.Conn, error) {
		// Отмечает неожиданный вызов сетевого подключения и возвращает пустой результат.
		called = true
		return nil, nil
	}, Admit: func(context.Context, OutboundOperation) (func(), error) {
		// Отклоняет допуск с core.ErrStale до попытки подключения.
		return nil, core.ErrStale
	}})
	if !errors.Is(err, core.ErrStale) || called {
		t.Fatalf("сеть до admission: %v %v", called, err)
	}
}
