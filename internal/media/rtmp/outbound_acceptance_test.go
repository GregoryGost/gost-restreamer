package rtmp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/handshake"
)

// Возвращает независимые бюджеты и секундные сроки исходящего RTMP с блоками по 128 байтов и очередью
// 16 управляющих ответов.
func acceptanceOutboundLimits() OutboundLimits {
	return OutboundLimits{MaxChunkStreams: 8, MaxMessageBytes: 65536, MaxChunkBytes: 4096, MaxFragments: 1024, MaxControlBytes: 256, MaxConfigBytes: 4096, MaxAMFBytes: 4096, MaxAMFDepth: 8, MaxAMFElements: 128, ChunkSize: 128, MaxControlPending: 16, MaxRetainedBytes: 4 << 20, DialTimeout: time.Second, TLSTimeout: time.Second, StartupTimeout: time.Second, MessageTimeout: time.Second, WriteTimeout: time.Second}
}

// Создаёт пару H.264 либо AV1 и AAC-LC с двумя каналами и частотой 48 кГц в заданной шкале из
// фактически разобранных заголовков.
func acceptanceOutboundConfigs(t *testing.T, av1 bool, base media.TimeBase) (media.CodecConfig, media.CodecConfig) {
	t.Helper()
	// h содержит декодированные заголовки H.264 перед возможной заменой на AV1. err сообщает об
	// отказе декодирования тестовой строки.
	h, err := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	if err != nil {
		t.Fatal(err)
	}
	// vf содержит видеоформат, фактически разобранный из тестовых заголовков H.264 либо AV1.
	vf, err := codec.H264Format(codec.VideoDescription{Headers: h, Width: 128, Height: 128})
	if err != nil {
		t.Fatal(err)
	}
	if av1 {
		h = []byte{0x81, 8, 0x0c, 0, 0x0a, 11, 0, 0, 0, 66, 167, 191, 228, 96, 13, 0, 64}
		vf, _, err = codec.InspectAV1TU(h, append(bytes.Clone(h[4:]), 0x1a, 1, 0x10), core.FrameRate{Numerator: 30, Denominator: 1})
		if err != nil {
			t.Fatal(err)
		}
	}
	// v содержит видеоконфигурацию с выбранным кодеком и заданной временной шкалой.
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: base, Video: vf}, h)
	if err != nil {
		t.Fatal(err)
	}
	// a содержит AAC-LC-конфигурацию 48 кГц stereo в заданной временной шкале.
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: base, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	return v, a
}

// Создаёт пакет с заданными PTS/DTS, флагом и исходной нагрузкой; длительность равна одному отсчёту.
func acceptanceOutboundPacket(t *testing.T, c media.CodecConfig, pts, dts int64, keyFlag bool, payload []byte) media.Packet {
	t.Helper()
	// p содержит пакет с заданными знаковыми PTS/DTS, заявленным ключевым флагом и нагрузкой. err
	// сообщает об отказе его структурной проверки.
	p, err := media.NewPacket(media.PacketInfo{PTS: pts, DTS: dts, DurationTicks: 1, Keyframe: keyFlag}, c, payload)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// acceptanceOutboundMemory моделирует соединение с сохранением байтов, частичными записями и управляемым
// отказом.
type acceptanceOutboundMemory struct {
	// input предоставляет подготовленные входящие байты либо оставляется nil для немедленного EOF.
	input io.Reader
	// wire накапливает байты, фактически принятые тестовым Write.
	wire bytes.Buffer
	// short ограничивает положительный результат одной записи при ненулевом значении. failAt задаёт
	// суммарное число принятых байтов, на котором возвращается io.ErrClosedPipe.
	short, failAt int
	// closed отмечает вызов Close для проверки реакции на отказ записи.
	closed bool
	// writeDeadlines сохраняет историю сроков записи. readDeadlines сохраняет историю сроков чтения.
	writeDeadlines, readDeadlines []time.Time
}

// Читает подготовленный ввод соединения в памяти либо возвращает EOF при его отсутствии.
func (m *acceptanceOutboundMemory) Read(p []byte) (int, error) {
	if m.input == nil {
		return 0, io.EOF
	}
	return m.input.Read(p)
}

// При положительном short ограничивает одну запись этим числом байтов и сохраняет их в памяти. При
// достижении failAt возвращает принятое число байтов вместе с io.ErrClosedPipe.
func (m *acceptanceOutboundMemory) Write(p []byte) (int, error) {
	// n задаёт число байтов текущего буфера, принимаемых с учётом short и оставшегося объёма до
	// failAt.
	n := len(p)
	if m.short > 0 {
		n = min(n, m.short)
	}
	if m.failAt > 0 {
		n = min(n, max(0, m.failAt-m.wire.Len()))
	}
	_, _ = m.wire.Write(p[:n])
	if m.failAt > 0 && m.wire.Len() == m.failAt {
		return n, io.ErrClosedPipe
	}
	return n, nil
}

// Отмечает закрытие тестового соединения для проверки реакции транспорта на ошибку.
func (m *acceptanceOutboundMemory) Close() error { m.closed = true; return nil }

// Возвращает пустой TCP-адрес тестового исходящего соединения.
func (m *acceptanceOutboundMemory) LocalAddr() net.Addr { return &net.TCPAddr{} }

// Возвращает пустой TCP-адрес тестового получателя.
func (m *acceptanceOutboundMemory) RemoteAddr() net.Addr { return &net.TCPAddr{} }

// Регистрирует одинаковый срок чтения и записи в тестовом соединении.
func (m *acceptanceOutboundMemory) SetDeadline(d time.Time) error {
	_ = m.SetReadDeadline(d)
	return m.SetWriteDeadline(d)
}

// Сохраняет очередной срок чтения для проверки его обновлений без реального ожидания.
func (m *acceptanceOutboundMemory) SetReadDeadline(d time.Time) error {
	m.readDeadlines = append(m.readDeadlines, d)
	return nil
}

// Сохраняет очередной срок записи для проверки единого бюджета сообщения.
func (m *acceptanceOutboundMemory) SetWriteDeadline(d time.Time) error {
	m.writeDeadlines = append(m.writeDeadlines, d)
	return nil
}

// Создаёт OutboundConn без сетевого запуска над переданным соединением, с отдельным назначением,
// допуском и MessageStreamID=0x765432.
func acceptanceOutboundLocal(t *testing.T, conn net.Conn, av1 bool) (*OutboundConn, media.CodecConfig, media.CodecConfig) {
	t.Helper()
	// v задаёт тестовую видеоконфигурацию, a — AAC-конфигурацию локального исходящего соединения.
	v, a := acceptanceOutboundConfigs(t, av1, media.TimeBase{Numerator: 1, Denominator: 2000})
	// target содержит проверенное тестовое назначение с заданным видеоформатом и AAC. err сообщает
	// об отказе его разрешения.
	target, err := ResolveOutbound("rtmp://receiver.invalid/app", "synthetic-key", core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio})
	if err != nil {
		t.Fatal(err)
	}
	// c содержит локальное исходящее соединение без фонового запуска, с тестовым допуском и
	// фиксированным MessageStreamID.
	c := &OutboundConn{conn: conn, ctx: context.Background(), opts: OutboundOptions{Limits: acceptanceOutboundLimits(), Admit: func(context.Context, OutboundOperation) (func(), error) {
		// Выдаёт допуск операции локального соединения без дополнительного учёта и возвращает пустое действие
		// освобождения.

		return func() {
			// Завершает допуск локальной операции без изменения тестового состояния.
		}, nil
	}}, target: target, done: make(chan struct{}), outboundChunkSize: 128, controls: make(chan outboundControl, 16)}
	c.streamID.Store(0x765432)
	return c, v, a
}

// acceptanceWireMessage хранит сообщение, разобранное независимым тестовым анализатором для побайтовой
// сверки.
type acceptanceWireMessage struct {
	// csid задаёт идентификатор потока блоков сообщения. typ задаёт RTMP-тип сообщения.
	csid, typ byte
	// stream задаёт MessageStreamID сообщения. stamp хранит его развёрнутую из заголовка 32-битную
	// метку в миллисекундах.
	stream, stamp uint32
	// body содержит собранное тело сообщения без заголовков блоков.
	body []byte
}

// Эталон разбирает только явно выбранные fmt0/fmt3 по RTMP §5.3.1 и уточнениям §§4–5.
// Он не использует рабочий анализатор либо средства разбора сообщений SDK.
func acceptanceReadMessage(r io.Reader, chunk int) (acceptanceWireMessage, error) {
	// m накапливает поля и тело сообщения, прочитанного независимым тестовым анализатором.
	var m acceptanceWireMessage
	// h принимает полный 12-байтовый заголовок первого блока fmt0.
	var h [12]byte
	// err сообщает об отказе полного чтения первого заголовка fmt0.
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return m, err
	}
	if h[0] < 2 || h[0] > 63 {
		return m, errors.New("ожидался fmt0")
	}
	m.csid, m.typ = h[0], h[7]
	m.stream = binary.LittleEndian.Uint32(h[8:])
	m.stamp = uint32(h[1])*65536 + uint32(h[2])*256 + uint32(h[3])
	// ext отмечает использование отдельного четырёхбайтового поля расширенной временной метки.
	ext := m.stamp == 0xffffff
	if ext {
		// b принимает расширенную временную метку в сетевом порядке байтов.
		var b [4]byte
		// err сообщает об отказе чтения расширенной временной метки первого блока.
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return m, err
		}
		m.stamp = binary.BigEndian.Uint32(b[:])
	}
	// length задаёт объявленную длину тела сообщения в байтах.
	length := int(h[4])*65536 + int(h[5])*256 + int(h[6])
	if length > 65536 {
		return m, errors.New("слишком длинный oracle message")
	}
	m.body = make([]byte, length)
	// at задаёт число уже прочитанных байтов тела и начало следующего блока.
	for at := 0; at < length; {
		if at != 0 {
			// cont принимает базовый заголовок продолжения fmt3 и возможную расширенную
			// временную метку.
			var cont [5]byte
			// n задаёт длину заголовка продолжения: один байт либо пять при расширенной
			// метке.
			n := 1
			if ext {
				n = 5
			}
			// err сообщает об отказе чтения полного заголовка продолжения.
			if _, err := io.ReadFull(r, cont[:n]); err != nil {
				return m, err
			}
			if cont[0] != 0xc0|m.csid || ext && binary.BigEndian.Uint32(cont[1:]) != m.stamp {
				return m, errors.New("неверный fmt3/extended timestamp")
			}
		}
		// n задаёт число байтов тела в текущем блоке с учётом остатка сообщения.
		n := min(chunk, length-at)
		// err сообщает об отказе полного чтения тела текущего блока.
		if _, err := io.ReadFull(r, m.body[at:at+n]); err != nil {
			return m, err
		}
		at += n
	}
	return m, nil
}

// Проверяет точные fmt0/fmt3-байты на границе расширенных меток и сворачивания uint32, один срок
// сообщения и учёт всех положительных частичных Write.
func TestOutboundAcceptanceFramingVectorsAndPartialAccounting(t *testing.T) {
	// tc задаёт временную метку и точные эталонные заголовки первого блока и продолжений.
	for _, tc := range []struct {
		// name задаёт имя сценария границы расширенной метки либо оборота uint32.
		name string
		// stamp задаёт сетевую временную метку сообщения в миллисекундах.
		stamp uint32
		// header содержит эталонный первый заголовок fmt0. cont содержит эталонный заголовок
		// каждого продолжения fmt3.
		header, cont []byte
	}{
		{"below_extended", 0xfffffe, []byte{6, 255, 255, 254, 0, 1, 1, 9, 0x32, 0x54, 0x76, 0}, []byte{0xc6}},
		{"first_extended", 0xffffff, []byte{6, 255, 255, 255, 0, 1, 1, 9, 0x32, 0x54, 0x76, 0, 0, 255, 255, 255}, []byte{0xc6, 0, 255, 255, 255}},
		{"above_extended", 0x1000000, []byte{6, 255, 255, 255, 0, 1, 1, 9, 0x32, 0x54, 0x76, 0, 1, 0, 0, 0}, []byte{0xc6, 1, 0, 0, 0}},
		{"before_wrap", 0xffffffff, []byte{6, 255, 255, 255, 0, 1, 1, 9, 0x32, 0x54, 0x76, 0, 255, 255, 255, 255}, []byte{0xc6, 255, 255, 255, 255}},
		{"after_wrap", 0, []byte{6, 0, 0, 0, 0, 1, 1, 9, 0x32, 0x54, 0x76, 0}, []byte{0xc6}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет точные байты заголовков и продолжений для выбранной временной метки при дробных записях,
			// учёт байтов и единый срок сообщения.

			// body содержит 257-байтовую нагрузку с последовательным заполнением для
			// проверки трёх блоков.
			body := make([]byte, 257)
			// i задаёт позицию байта нагрузки, используемую также как его значение с
			// приведением к byte.
			for i := range body {
				body[i] = byte(i)
			}
			// want накапливает точные ожидаемые байты заголовков и трёх частей тела.
			want := append(bytes.Clone(tc.header), body[:128]...)
			want = append(want, tc.cont...)
			want = append(want, body[128:256]...)
			want = append(want, tc.cont...)
			want = append(want, body[256])
			// short задаёт максимальный положительный результат одного физического Write.
			for _, short := range []int{1, 7, 128} {
				// m моделирует транспорт с выбранным ограничением длины одного Write.
				m := &acceptanceOutboundMemory{short: short}
				// c сериализует сообщение в транспорт с частичными положительными
				// записями.
				c, _, _ := acceptanceOutboundLocal(t, m, false)
				// observed накапливает байты, переданные наблюдателю OnWrite.
				observed := 0
				c.opts.OnWrite = func(n int) {
					// Суммирует фактически принятые байты каждой дробной записи для сравнения с сетевым представлением.
					observed += n
				}
				// err сообщает об отказе отправки сообщения с выбранной меткой и
				// частичными записями.
				if err := c.writeMessage(context.Background(), 6, 9, 0x765432, tc.stamp, body); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(m.wire.Bytes(), want) || observed != len(want) || c.Snapshot().WrittenBytes != uint64(len(want)) {
					t.Fatalf("short=%d wire=%x want=%x observed=%d", short, m.wire.Bytes(), want, observed)
				}
				if len(m.writeDeadlines) != 1 || m.writeDeadlines[0].IsZero() {
					t.Fatal("дробные Write продлили общий срок")
				}
			}
		})
	}
	// m моделирует частичные записи с отказом после суммарного приёма 17 байтов.
	m := &acceptanceOutboundMemory{short: 7, failAt: 17}
	// c проверяет учёт положительных результатов Write, включая запись, вернувшую ошибку.
	c, _, _ := acceptanceOutboundLocal(t, m, false)
	// observed накапливает положительные числа байтов из OnWrite до отказа транспорта.
	observed := 0
	c.opts.OnWrite = func(n int) {
		// Суммирует положительные результаты записей, включая частичную запись с ошибкой.
		observed += n
	}
	// err содержит отказ отправки сообщения после приёма транспортом 17 байтов.
	err := c.writeMessage(context.Background(), 6, 9, 0x765432, 0, make([]byte, 257))
	if !errors.Is(err, io.ErrClosedPipe) || observed != 17 || c.Snapshot().WrittenBytes != 17 || !m.closed {
		t.Fatalf("потеряны положительные n частичных записей: n=%d stats=%+v closed=%v err=%v", observed, c.Snapshot(), m.closed, err)
	}
}

// Проверяет точное округление миллисекунд, половины от нуля, крайние int64 и типизированный отказ при
// неверной шкале или переполнении.
func TestOutboundAcceptanceRationalQuantization(t *testing.T) {
	// tc задаёт знаковые отсчёты, рациональную шкалу и точный ожидаемый результат округления.
	for _, tc := range []struct {
		// ticks задаёт число знаковых отсчётов, переводимых в миллисекунды.
		ticks int64
		// num задаёт числитель секунд на отсчёт. den задаёт знаменатель этой временной шкалы.
		num, den uint32
		// want задаёт ожидаемое целое число миллисекунд после округления половин от нуля.
		want int64
	}{
		{1, 1, 2000, 1}, {-1, 1, 2000, -1}, {2, 1, 2000, 1}, {-2, 1, 2000, -1}, {3, 1, 2000, 2}, {-3, 1, 2000, -2},
		{1, 1, 2001, 0}, {-1, 1, 2001, 0}, {1, 1, 1999, 1}, {-1, 1, 1999, -1},
		{1, 1, 60000, 0}, {30, 1, 60000, 1}, {-30, 1, 60000, -1}, {1001, 1, 60000, 17}, {-1001, 1, 60000, -17},
		{1, 1001, 60000, 17}, {-1, 1001, 60000, -17}, {math.MaxInt64, 1, 1000, math.MaxInt64}, {math.MinInt64, 1, 1000, math.MinInt64},
		{123, math.MaxUint32, math.MaxUint32, 123000},
	} {
		// got содержит округлённое число миллисекунд. err сообщает об отказе перевода заданных
		// отсчётов.
		got, err := QuantizeOutboundMilliseconds(tc.ticks, media.TimeBase{Numerator: tc.num, Denominator: tc.den})
		if err != nil || got != tc.want {
			t.Fatalf("Q(%d,%d/%d)=%d,%v; нужно %d", tc.ticks, tc.num, tc.den, got, err, tc.want)
		}
	}
	// base задаёт шкалу с неверным значением либо результатом за пределами int64.
	for _, base := range []media.TimeBase{{}, {Numerator: 1, Denominator: 1}, {Numerator: math.MaxUint32, Denominator: 1}} {
		// err содержит ожидаемый отказ перевода MaxInt64 отсчётов в неподходящей шкале.
		_, err := QuantizeOutboundMilliseconds(math.MaxInt64, base)
		// out принимает типизированную ошибку для проверки категории OutboundTiming.
		var out *OutboundError
		if !errors.As(err, &out) || out.Kind != OutboundTiming {
			t.Fatalf("нет типизированной ошибки времени: %v", err)
		}
	}
}

// Проверяет исходные байты заголовков и AU, независимое округление PTS/DTS, H.264 CTS и общее начало
// A/V, сохраняемое при новой конфигурации.
func TestOutboundAcceptanceExactCodecBytesAndComposition(t *testing.T) {
	// tc задаёт кодек, исходные метки и эталонные RTMP-времена и CTS для проверки общей шкалы A/V.
	for _, tc := range []struct {
		// name задаёт имя сценария округления PTS/DTS либо точной передачи AV1.
		name string
		// av1 выбирает AV1 вместо H.264 для проверяемого видео.
		av1 bool
		// pts задаёт знаковую метку показа первого видеопакета. dts задаёт его знаковую метку
		// декодирования; обе выражены в половинах миллисекунды.
		pts, dts int64
		// stamp задаёт ожидаемую сетевую метку первого видео. audioStamp задаёт ожидаемую метку
		// AAC. recoveryStamp задаёт ожидаемую общую метку после смены конфигурации; все значения
		// выражены в миллисекундах.
		stamp, audioStamp, recoveryStamp uint32
		// cts содержит три эталонных байта знакового смещения композиции H.264.
		cts []byte
	}{
		{"positive_ties", false, 1, -1, 0, 3, 1001, []byte{0, 0, 2}},
		{"negative_ties", false, -1, 1, 2, 3, 1001, []byte{255, 255, 254}},
		{"round_marks_before_difference", false, 2, 1, 0, 1, 999, []byte{0, 0, 0}},
		{"negative_half", false, -3, -1, 1, 4, 1002, []byte{255, 255, 255}},
		{"av1_exact", true, -1, -1, 0, 3, 1001, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет байты кодеков, метаданные, округление PTS/DTS и сохранение начала отсчёта после смены
			// конфигурации для выбранного сценария.

			// m сохраняет полный RTMP-вывод для независимой проверки кодеков и временных
			// меток.
			m := &acceptanceOutboundMemory{}
			// c содержит исходящее соединение. v задаёт выбранную видеоконфигурацию, a —
			// AAC-конфигурацию в шкале 1/2000 секунды.
			c, v, a := acceptanceOutboundLocal(t, m, tc.av1)
			// videoAU содержит H.264 IDR либо тестовый временной блок AV1 с независимым
			// кадром.
			videoAU := []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}
			if tc.av1 {
				videoAU = append(v.Headers()[4:], 0x1a, 1, 0x10)
			}
			// audioAU содержит исходный AAC AU для проверки сохранности байтов.
			audioAU := []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c}
			// err сообщает об отказе сохранения конфигураций перед первым проверенным
			// независимым видеокадром.
			if err := c.WriteHeaders(context.Background(), v, a); err != nil {
				t.Fatal(err)
			}
			if m.wire.Len() != 0 {
				t.Fatal("заголовки записаны до проверки независимости")
			}
			// result содержит сведения об отправленном видео после проверки фактической
			// независимости AU. err сообщает об отказе его отправки.
			result, err := c.WritePacket(context.Background(), acceptanceOutboundPacket(t, v, tc.pts, tc.dts, false, videoAU))
			if err != nil || !result.Independent || result.PayloadBytes != uint64(len(videoAU)) || result.Track != media.TrackVideo {
				t.Fatalf("%+v %v", result, err)
			}
			if _, err = c.WritePacket(context.Background(), acceptanceOutboundPacket(t, a, 3, 3, false, audioAU)); err != nil {
				t.Fatal(err)
			}
			// r предоставляет чтение сохранённого RTMP-вывода независимому анализатору.
			r := bytes.NewReader(m.wire.Bytes())
			// messages принимает пять сообщений: метаданные, две конфигурации и два
			// медиапакета.
			messages := make([]acceptanceWireMessage, 5)
			// i задаёт позицию очередного сообщения в ожидаемой последовательности из пяти
			// сообщений.
			for i := range messages {
				messages[i], err = acceptanceReadMessage(r, 128)
				if err != nil {
					t.Fatal(err)
				}
			}
			// metadata содержит разобранные AMF-значения сообщения onMetaData.
			metadata, err := amf0.Unmarshal(messages[0].body)
			if err != nil || len(metadata) != 3 || metadata[0] != "@setDataFrame" || metadata[1] != "onMetaData" {
				t.Fatalf("metadata: %v %v", metadata, err)
			}
			// fields содержит ECMA-массив метаданных. ok подтверждает ожидаемый тип этого
			// AMF-значения.
			fields, ok := metadata[2].(amf0.ECMAArray)
			if !ok {
				t.Fatalf("тип metadata %T", metadata[2])
			}
			// values индексирует значения метаданных по именам для проверки кодеков,
			// размеров и аудиоформата.
			values := map[string]any{}
			// field содержит очередную именованную пару из ECMA-массива метаданных.
			for _, field := range fields {
				values[field.Key] = field.Value
			}
			// wantCodec задаёт ожидаемое числовое обозначение H.264 либо AV1 в метаданных.
			wantCodec := float64(7)
			if tc.av1 {
				wantCodec = 1635135537
			}
			if values["videocodecid"] != wantCodec || values["audiocodecid"] != float64(10) || values["audiosamplerate"] != float64(48000) {
				t.Fatalf("кодек/частота metadata: %+v", values)
			}
			// wantWidth задаёт ожидаемую ширину видео, wantHeight — ожидаемую высоту видео в
			// пикселях для выбранного кодека.
			wantWidth, wantHeight := float64(128), float64(128)
			if tc.av1 {
				wantWidth, wantHeight = 1920, 804
			}
			if values["width"] != wantWidth || values["height"] != wantHeight || values["audiochannels"] != float64(2) || values["stereo"] != true {
				t.Fatalf("metadata размеров/каналов: %+v", values)
			}
			// videoHeader содержит ожидаемый префикс сообщения конфигурации H.264 либо AV1.
			videoHeader := []byte{0x17, 0, 0, 0, 0}
			// videoPrefix содержит ожидаемый префикс видеопакета, включая CTS H.264 либо
			// FourCC AV1.
			videoPrefix := append([]byte{0x17, 1}, tc.cts...)
			if tc.av1 {
				videoHeader = []byte{0x90, 'a', 'v', '0', '1'}
				videoPrefix = []byte{0x91, 'a', 'v', '0', '1'}
			}
			// i задаёт позицию кодекового сообщения после метаданных. want содержит его
			// ожидаемое тело с исходными заголовками либо AU.
			for i, want := range [][]byte{append(videoHeader, v.Headers()...), {0xaf, 0, 0x11, 0x90}, append(videoPrefix, videoAU...), append([]byte{0xaf, 1}, audioAU...)} {
				// msg содержит фактически разобранное кодековое сообщение для сравнения
				// тела и MessageStreamID.
				msg := messages[i+1]
				if !bytes.Equal(msg.body, want) || msg.stream != 0x765432 {
					t.Fatalf("message%d: body=%x stream=%x; want=%x", i+1, msg.body, msg.stream, want)
				}
			}
			if messages[3].stamp != tc.stamp || messages[4].stamp != tc.audioStamp {
				t.Fatalf("video time %d != %d", messages[3].stamp, tc.stamp)
			}
			// before сохраняет длину вывода до смены конфигурации, отделяя сообщения
			// восстановления.
			before := m.wire.Len()
			// vi содержит параметры видеоконфигурации для увеличения Revision. ai содержит
			// параметры AAC-конфигурации для такого же обновления.
			vi, ai := v.Info(), a.Info()
			vi.Revision++
			ai.Revision++
			// v2 содержит видеоконфигурацию новой ревизии с прежними заголовками.
			v2, err := media.NewCodecConfig(vi, v.Headers())
			if err != nil {
				t.Fatal(err)
			}
			// a2 содержит AAC-конфигурацию новой ревизии с прежними заголовками.
			a2, err := media.NewCodecConfig(ai, a.Headers())
			if err != nil {
				t.Fatal(err)
			}
			if err = c.WriteHeaders(context.Background(), v2, a2); err != nil {
				t.Fatal(err)
			}
			if _, err = c.WritePacket(context.Background(), acceptanceOutboundPacket(t, v2, 2000, 2000, false, videoAU)); err != nil {
				t.Fatal(err)
			}
			// next читает только сообщения, записанные после обновления конфигураций.
			next := bytes.NewReader(m.wire.Bytes()[before:])
			for range 4 {
				// msg содержит очередное сообщение после обновления конфигураций. e
				// сообщает об отказе его независимого разбора.
				msg, e := acceptanceReadMessage(next, 128)
				if e != nil || msg.stamp != tc.recoveryStamp {
					t.Fatalf("сброс конфигурации изменил начало отсчёта: %+v %v", msg, e)
				}
			}
		})
	}
}

// Отправляет независимый AMF0-ответ одним fmt0-блоком со строго ограниченным телом до 128 байтов.
func acceptanceSendAMF(w io.Writer, stream uint32, data amf0.Data) error {
	// b содержит сериализованное AMF-тело ответа тестового получателя. err сообщает об отказе его
	// кодирования.
	b, err := data.Marshal()
	if err != nil {
		return err
	}
	if len(b) > 128 {
		return errors.New("ответ oracle превышает один chunk")
	}
	// h содержит полный заголовок fmt0 для AMF-ответа на заданном MessageStreamID.
	h := []byte{3, 0, 0, 0, 0, 0, byte(len(b)), 20, byte(stream), byte(stream >> 8), byte(stream >> 16), byte(stream >> 24)}
	_, err = w.Write(append(h, b...))
	return err
}

// Имитирует RTMP-получателя с проверкой точных app, tcUrl и ключа; по параметру подставляет неверный
// статус, транзакцию или MessageStreamID запуска.
func acceptanceStartup(peer net.Conn, reject string) error {
	// err сообщает об отказе серверного RTMP-рукопожатия тестового получателя.
	if _, _, err := handshake.DoServer(peer, true); err != nil {
		return err
	}
	// m содержит клиентскую команду connect перед дальнейшими командами запуска. err сообщает об
	// отказе её независимого чтения.
	m, err := acceptanceReadMessage(peer, 128)
	if err != nil {
		return err
	}
	// data содержит разобранные AMF-значения очередной клиентской команды запуска.
	data, err := amf0.Unmarshal(m.body)
	if err != nil || len(data) != 3 || data[0] != "connect" || data[1] != float64(1) {
		return fmt.Errorf("connect: %v %v", data, err)
	}
	// obj содержит объект параметров connect. ok подтверждает ожидаемый тип его AMF-представления.
	obj, ok := data[2].(amf0.Object)
	if !ok {
		return errors.New("connect object отсутствует")
	}
	// app сохраняет точное значение app из команды connect для проверки экранированного пути и
	// запроса.
	app, _ := obj.GetString("app")
	// tcURL сохраняет точное значение tcUrl из команды connect для проверки отсутствия нормализации
	// URL.
	tcURL, _ := obj.GetString("tcUrl")
	if app != "a%2Fb/../live?x=%2F&x=2" || (tcURL != "rtmp://receiver.test/a%2Fb/../live?x=%2F&x=2" && tcURL != "rtmps://receiver.test/a%2Fb/../live?x=%2F&x=2" && tcURL != "rtmps://wrong.test/a%2Fb/../live?x=%2F&x=2") {
		return fmt.Errorf("URL изменён: app=%s tcUrl=%s", app, tcURL)
	}
	// code задаёт статус ответа текущей стадии запуска. transaction задаёт номер транзакции ответа,
	// который может намеренно искажаться.
	code, transaction := "NetConnection.Connect.Success", float64(1)
	if reject == "connect_code" {
		code = "NetConnection.Connect.Rejected"
	}
	if reject == "connect_transaction" {
		transaction = 9
	}
	// name задаёт имя ответа connect: _result при обычном ответе либо _error при явном отказе.
	name := "_result"
	if reject == "connect_denied" {
		name, code = "_error", "NetConnection.Connect.Rejected"
	}
	if err = acceptanceSendAMF(peer, 0, amf0.Data{name, transaction, nil, amf0.Object{{Key: "level", Value: "status"}, {Key: "code", Value: code}}}); err != nil {
		return err
	}
	if reject == "connect_code" || reject == "connect_transaction" || reject == "connect_denied" {
		return nil
	}
	m, err = acceptanceReadMessage(peer, 128)
	if err != nil {
		return err
	}
	if m.typ != 1 || !bytes.Equal(m.body, []byte{0, 0, 0, 128}) {
		return errors.New("неверный SetChunkSize")
	}
	m, err = acceptanceReadMessage(peer, 128)
	if err != nil {
		return err
	}
	data, err = amf0.Unmarshal(m.body)
	if err != nil || len(data) != 3 || data[0] != "createStream" || data[1] != float64(2) {
		return errors.New("неверный createStream")
	}
	if err = acceptanceSendAMF(peer, 0, amf0.Data{"_result", float64(2), nil, float64(0x765432)}); err != nil {
		return err
	}
	m, err = acceptanceReadMessage(peer, 128)
	if err != nil {
		return err
	}
	data, err = amf0.Unmarshal(m.body)
	if err != nil || m.stream != 0x765432 || len(data) != 5 || data[0] != "publish" || data[1] != float64(0) || data[3] != "exact/key?#%2F" || data[4] != "live" {
		return fmt.Errorf("publish изменён: %v, stream=%x, %v", data, m.stream, err)
	}
	code, transaction = "NetStream.Publish.Start", 0
	// stream задаёт MessageStreamID уведомления publish, при выбранном сценарии намеренно неверный.
	stream := uint32(0x765432)
	if reject == "publish_code" {
		code = "NetStream.Publish.BadName"
	}
	if reject == "publish_transaction" {
		transaction = 1
	}
	if reject == "publish_stream" {
		stream = 0x345676
	}
	return acceptanceSendAMF(peer, stream, amf0.Data{"onStatus", transaction, nil, amf0.Object{{Key: "level", Value: "status"}, {Key: "code", Value: code}}})
}

// Подставляет заданный net.Conn и отдельные счётчики допусков операций, отклоняя одновременное занятие
// одного слота.
func acceptancePipeOptions(conn net.Conn, l OutboundLimits, active *[4]atomic.Int32) OutboundOptions {
	return OutboundOptions{Limits: l, DialContext: func(context.Context, string, string) (net.Conn, error) {
		// Возвращает заранее созданный конец net.Pipe вместо сетевого подключения.
		return conn, nil
	}, Admit: func(_ context.Context, op OutboundOperation) (func(), error) {
		// Занимает единственный счётный слот выбранной операции и возвращает действие его освобождения либо
		// ошибку повторного занятия.

		if active[op].Add(1) != 1 {
			return nil, errors.New("одновременное занятие одного operation slot")
		}
		return func() {
			// Освобождает захваченный слот операции после завершения допуска.
			active[op].Add(-1)
		}, nil
	}}
}

// Создаёт проверенное назначение с переданным endpoint, эталонным H.264/AAC и точным ключом
// exact/key?#%2F.
func acceptanceTarget(t *testing.T, endpoint string) OutboundTarget {
	t.Helper()
	// v задаёт эталонный H.264-формат, a — эталонный AAC-формат назначения.
	v, a := acceptanceOutboundConfigs(t, false, media.TimeBase{Numerator: 1, Denominator: 1000})
	// target содержит проверенное назначение с точным endpoint и ключом exact/key?#%2F. err сообщает
	// об отказе его разрешения.
	target, err := ResolveOutbound(endpoint, "exact/key?#%2F", core.MediaFormat{Video: v.Info().Video, Audio: a.Info().Audio})
	if err != nil {
		t.Fatal(err)
	}
	return target
}

// Проверяет неизменность адреса и ключа, принятие только точных ответов запуска, сохранение кодов
// отказа и завершение всех допущенных операций.
func TestOutboundAcceptanceExactBindingAndStrictSuccess(t *testing.T) {
	// reject выбирает успешный запуск либо конкретное искажение ответа connect или publish.
	for _, reject := range []string{"", "connect_code", "connect_denied", "connect_transaction", "publish_code", "publish_transaction", "publish_stream"} {
		t.Run("status_"+reject, func(t *testing.T) {
			// Проверяет точность адреса приложения и ключа, обязательные поля успешных ответов и освобождение
			// операций для выбранного отказа установки.

			// client служит клиентским концом локального соединения, peer — концом тестового
			// RTMP-сервера.
			client, peer := net.Pipe()
			defer func() {
				// Закрывает принимающий конец net.Pipe после проверки установки публикации.
				_ = peer.Close()
			}()
			defer func() {
				// Закрывает клиентский конец net.Pipe после проверки установки публикации.
				_ = client.Close()
			}()
			// serverResult передаёт итог проверки клиентских команд и отправки серверных
			// ответов.
			serverResult := make(chan error, 1)
			// serverDone закрывается при полном выходе горутины тестового RTMP-сервера.
			serverDone := make(chan struct{})
			go func() {
				// Выполняет тестовое согласование с выбранным ответом, сообщает результат и при успехе читает
				// оставшиеся байты до закрытия соединения.

				defer close(serverDone)
				// err содержит результат сценария установки публикации на тестовом
				// сервере.
				err := acceptanceStartup(peer, reject)
				serverResult <- err
				if err == nil {
					_, _ = io.Copy(io.Discard, peer)
				}
			}()
			// active хранит независимые атомарные числа занятых допусков для каждой операции
			// соединения.
			var active [4]atomic.Int32
			// opts задаёт локальный транспорт, ограничения и счётные допуски операций.
			opts := acceptancePipeOptions(client, acceptanceOutboundLimits(), &active)
			// ctx ограничивает запуск и ожидание завершения тремя секундами. cancel
			// освобождает ресурсы этого срока.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			// c содержит соединение при успешном запуске. err содержит отказ запуска для
			// проверки точных статусов и транзакций.
			c, err := DialOutbound(ctx, acceptanceTarget(t, "rtmp://receiver.test/a%2Fb/../live?x=%2F&x=2"), opts)
			if reject == "" {
				if err != nil {
					t.Fatal(err)
				}
				if err = c.Close(); err != nil {
					t.Fatal(err)
				}
				if err = c.Wait(ctx); err != nil {
					t.Fatal(err)
				}
			} else if !IsOutboundTerminal(err) {
				t.Fatalf("неверный статус/транзакция принят либо отказ не окончательный: %v", err)
			}
			if reject == "connect_denied" || reject == "publish_code" {
				// out принимает типизированный окончательный отказ для проверки
				// категории OutboundDenied и сохранённого кода.
				var out *OutboundError
				if !errors.As(err, &out) || out.Kind != OutboundDenied || out.Code == "" {
					t.Fatalf("потерян код окончательного отказа авторизации: %v", err)
				}
			}
			if err = <-serverResult; err != nil {
				t.Fatal(err)
			}
			_ = client.Close()
			select {
			case <-serverDone:
			case <-ctx.Done():
				t.Fatal("принимающий узел не завершился")
			}
			// op задаёт вид операции соединения, чей счётчик допуска должен вернуться к
			// нулю.
			for op := 1; op <= 3; op++ {
				if active[op].Load() != 0 {
					t.Fatalf("операция %d не завершена", op)
				}
			}
		})
	}
}

// Проверяет допустимую тишину после публикации и завершение начатого сообщения в исходные 100 мс
// несмотря на чередование другого потока фрагментов.
func TestOutboundAcceptanceIdleAndFixedStartedMessageDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// При управляемом времени проверяет допустимость простоя и неизменный срок первого незавершённого
		// сообщения при фрагментах другого потока.

		// client служит клиентским концом локального соединения, peer — источником сообщений при
		// проверке сроков.
		client, peer := net.Pipe()
		defer func() {
			// Закрывает принимающий конец net.Pipe после проверки сроков входящих сообщений.
			_ = peer.Close()
		}()
		defer func() {
			// Закрывает клиентский конец net.Pipe после проверки сроков входящих сообщений.
			_ = client.Close()
		}()
		// ready передаёт итог установки публикации перед проверкой простоя и частичных
		// сообщений.
		ready := make(chan error, 1)
		// send передаёт тестовому серверу очередные байты для синхронной отправки клиенту.
		send := make(chan []byte)
		// serverDone закрывается после выхода горутины сервера, отправляющей выбранные байты.
		serverDone := make(chan struct{})
		go func() {
			// Выполняет тестовую установку публикации и передаёт последовательности байтов из send, сообщая
			// результат установки и выход через serverDone.

			defer close(serverDone)
			// err сообщает об отказе установки публикации перед проверкой сроков сообщений.
			if err := acceptanceStartup(peer, ""); err != nil {
				ready <- err
				return
			}
			ready <- nil
			// b содержит очередную последовательность байтов, которую сервер должен передать
			// клиенту.
			for b := range send {
				// err сообщает об отказе отправки очередной последовательности байтов из
				// send.
				if _, err := peer.Write(b); err != nil {
					return
				}
			}
		}()
		defer close(send)
		// active хранит атомарные числа допусков операций проверяемого соединения.
		var active [4]atomic.Int32
		// l задаёт ограничения с фиксированным сроком начатого сообщения 100 миллисекунд.
		l := acceptanceOutboundLimits()
		l.MessageTimeout = 100 * time.Millisecond
		// ctx задаёт отменяемый срок существования проверяемого соединения. cancel завершает его
		// при выходе теста.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// c содержит запущенное соединение для проверки простоя и неизменного срока сообщения.
		// err сообщает об отказе запуска.
		c, err := DialOutbound(ctx, acceptanceTarget(t, "rtmp://receiver.test/a%2Fb/../live?x=%2F&x=2"), acceptancePipeOptions(client, l, &active))
		if err != nil {
			t.Fatal(err)
		}
		if err = <-ready; err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		synctest.Wait()
		select {
		case <-c.Done():
			t.Fatalf("тишина завершила исправное соединение: %v", c.Err())
		default:
		}
		// В первом незавершённом сообщении принят один из двух байтов тела.
		send <- []byte{2, 0, 0, 0, 0, 0, 4, 1, 0, 0, 0, 0, 0, 0, 0, 1}
		synctest.Wait()
		send <- []byte{3, 0, 0, 0, 0, 0, 2, 20, 0, 0, 0, 0, 5}
		synctest.Wait()
		time.Sleep(60 * time.Millisecond)
		// Второй поток фрагментов не может продлить общий срок первого сообщения.
		send <- []byte{4}
		synctest.Wait()
		time.Sleep(40 * time.Millisecond)
		synctest.Wait()
		select {
		case <-c.Done():
		default:
			t.Fatal("фрагмент другого потока продлил срок первого сообщения")
		}
		// timeout принимает сетевой отказ для проверки истечения срока начатого сообщения.
		var timeout net.Error
		if !errors.As(c.Err(), &timeout) || !timeout.Timeout() {
			t.Fatalf("ожидалось истечение сетевого срока: %v", c.Err())
		}
		// join ограничивает ожидание завершения операций одной секундой. cancelJoin освобождает
		// ресурсы этого срока.
		join, cancelJoin := context.WithTimeout(context.Background(), time.Second)
		defer cancelJoin()
		if err = c.Wait(join); err != nil {
			t.Fatal(err)
		}
	})
}

// Создаёт временный самоподписанный сертификат receiver.test и отдельный пул доверия для локальных
// TLS-проверок.
func acceptanceCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	// key содержит закрытый ключ временного TLS-сертификата. err сообщает об отказе генерации ключа.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// now задаёт опорное время для начала и конца срока действия тестового сертификата.
	now := time.Now()
	// template задаёт самоподписанный сертификат receiver.test для локальных TLS-проверок.
	template := &x509.Certificate{SerialNumber: big.NewInt(41), Subject: pkix.Name{CommonName: "synthetic receiver"}, DNSNames: []string{"receiver.test"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	// der содержит закодированный самоподписанный сертификат.
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	// leaf содержит разобранный сертификат для добавления в отдельный доверенный пул.
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	// pool хранит доверие только добавленному тестовому сертификату receiver.test.
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// Проверяет доверенный RTMPS, окончательность ошибок доверия и имени узла, правильный SNI,
// неизменность TLSConfig вызывающего и завершение операций.
func TestOutboundAcceptanceTLSTrustAndHostname(t *testing.T) {
	// cert содержит сертификат локального TLS-сервера. pool содержит клиентское доверие этому
	// сертификату.
	cert, pool := acceptanceCertificate(t)
	// mode выбирает доверенное подключение, недоверенный сертификат либо несовпадение имени узла.
	for _, mode := range []string{"trusted", "untrusted", "wrong_hostname"} {
		t.Run(mode, func(t *testing.T) {
			// Проверяет доверие сертификату, имя узла и неизменность переданной конфигурации TLS для выбранного
			// режима.

			// listener принимает локальное TCP-соединение на автоматически выбранном порту.
			// err сообщает об отказе его открытия.
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				// Закрывает локальный слушатель после проверки TLS.
				_ = listener.Close()
			}()
			// serverDone закрывается при полном выходе горутины тестового TLS-сервера.
			serverDone := make(chan struct{})
			// serverResult передаёт результат TLS-согласования, проверки SNI и запуска RTMP.
			serverResult := make(chan error, 1)
			go func() {
				// Принимает локальное TLS-соединение, проверяет SNI, выполняет RTMP-согласование и сообщает результат;
				// после успеха читает до закрытия.

				defer close(serverDone)
				// rawPeer содержит принятый локальный TCP-сокет. err сообщает об отказе
				// приёма соединения.
				rawPeer, err := listener.Accept()
				if err != nil {
					serverResult <- err
					return
				}
				defer func() {
					// Закрывает принятый TCP-сокет при выходе тестового TLS-узла.
					_ = rawPeer.Close()
				}()
				// peer оборачивает принятый сокет серверным TLS с тестовым сертификатом.
				peer := tls.Server(rawPeer, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
				err = peer.Handshake()
				if err == nil && peer.ConnectionState().ServerName != "receiver.test" {
					err = errors.New("SNI не совпадает с target hostname")
				}
				if err == nil {
					err = acceptanceStartup(peer, "")
				}
				serverResult <- err
				if err == nil {
					_, _ = io.Copy(io.Discard, peer)
				}
			}()
			// active хранит атомарные числа допусков операций для проверки освобождения
			// после TLS.
			var active [4]atomic.Int32
			// opts задаёт ограничения и счётные допуски перед подстановкой локального
			// TCP-подключения.
			opts := acceptancePipeOptions(nil, acceptanceOutboundLimits(), &active)
			opts.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
				// Подключает клиент к фактическому локальному слушателю, сохраняя проверку имени целевого узла на
				// уровне TLS.

				return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
			}
			// cfg содержит переданную клиентом конфигурацию TLS, неизменность которой
			// проверяется после подключения.
			cfg := &tls.Config{RootCAs: pool, ServerName: "unchanged.test", MinVersion: tls.VersionTLS12}
			if mode == "untrusted" {
				cfg.RootCAs = x509.NewCertPool()
			}
			opts.TLSConfig = cfg
			// endpoint задаёт URL с receiver.test либо намеренно неверным wrong.test для
			// проверки имени сертификата.
			endpoint := "rtmps://receiver.test/a%2Fb/../live?x=%2F&x=2"
			if mode == "wrong_hostname" {
				endpoint = "rtmps://wrong.test/a%2Fb/../live?x=%2F&x=2"
			}
			// ctx ограничивает TLS-проверку и ожидание завершения тремя секундами. cancel
			// освобождает ресурсы этого срока.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			// c содержит исходящее соединение при успешной проверке TLS.
			c, err := DialOutbound(ctx, acceptanceTarget(t, endpoint), opts)
			if mode == "trusted" {
				if err != nil {
					t.Fatal(err)
				}
				if err = c.Close(); err != nil {
					t.Fatal(err)
				}
				if err = c.Wait(ctx); err != nil {
					t.Fatal(err)
				}
			} else if !IsOutboundTerminal(err) {
				t.Fatalf("ошибка сертификата должна быть окончательной: %v", err)
			}
			if cfg.ServerName != "unchanged.test" || cfg.InsecureSkipVerify {
				t.Fatal("изменена конфигурация TLS вызывающего кода")
			}
			select {
			case <-serverDone:
			case <-ctx.Done():
				t.Fatal("принимающий узел TLS не завершился")
			}
			// peerErr принимает результат TLS и RTMP на стороне локального сервера.
			peerErr := <-serverResult
			if mode == "trusted" && peerErr != nil {
				t.Fatal(peerErr)
			}
			// op задаёт вид операции, чей допуск должен быть освобождён после TLS-проверки.
			for op := 1; op <= 3; op++ {
				if active[op].Load() != 0 {
					t.Fatalf("операция TLS %d не завершена", op)
				}
			}
		})
	}
}

// Проверяет закрытие соединения, возвращённого DialContext после таймаута, до освобождения допуска
// установления.
func TestOutboundAcceptanceLateDialFinishesAfterSocketClose(t *testing.T) {
	// client служит поздно возвращаемым клиентским сокетом, peer — концом для проверки его
	// обязательного закрытия.
	client, peer := net.Pipe()
	defer func() {
		// Закрывает принимающий конец net.Pipe после проверки позднего завершения подключения.
		_ = peer.Close()
	}()
	// active хранит атомарные числа допусков, включая допуск установления соединения.
	var active [4]atomic.Int32
	// l задаёт ограничения с миллисекундным сроком подключения.
	l := acceptanceOutboundLimits()
	l.DialTimeout = time.Millisecond
	// opts подставляет локальное соединение, возвращаемое только после отмены контекста подключения.
	opts := acceptancePipeOptions(client, l, &active)
	opts.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		// Возвращает созданный сокет только после истечения контекста подключения, моделируя поздний успешный
		// результат.
		<-ctx.Done()
		return client, nil
	}
	// err содержит отказ подключения, чей поздний успешный результат должен быть закрыт до
	// освобождения допуска.
	_, err := DialOutbound(context.Background(), acceptanceTarget(t, "rtmp://receiver.test/a%2Fb/../live?x=%2F&x=2"), opts)
	if !errors.Is(err, context.DeadlineExceeded) || active[OutboundEstablishment].Load() != 0 {
		t.Fatalf("позднее завершение подключения: %v", err)
	}
	if _, err = peer.Write([]byte{1}); err == nil {
		t.Fatal("позднее соединение осталось открытым")
	}
}

// Проверяет отказ неверных настроек, TLS, назначения и допуска до DialContext, а также запрет учётных
// данных в URL, разделителя # и недопустимых портов.
func TestOutboundAcceptancePreflightRejectsBeforeNetwork(t *testing.T) {
	// target задаёт исходное проверенное назначение для сценариев отказа до сети.
	target := acceptanceTarget(t, "rtmp://receiver.test/a%2Fb/../live?x=%2F&x=2")
	// mode выбирает недопустимые настройки, назначение либо отказ допуска до DialContext.
	for _, mode := range []string{"missing_admit", "invalid_limits", "insecure_tls", "invalid_target", "denied_admission"} {
		t.Run(mode, func(t *testing.T) {
			// Проверяет отказ выбранной предварительной проверки либо допуска до вызова функции сетевого
			// подключения.

			// called отмечает достижение функции сетевого подключения вопреки ожидаемому
			// раннему отказу.
			called := false
			// opts задаёт исходные корректные параметры перед внесением выбранного
			// нарушения.
			opts := OutboundOptions{Limits: acceptanceOutboundLimits(), DialContext: func(context.Context, string, string) (net.Conn, error) {
				// Помечает попытку подключения и возвращает ошибку, если проверяемая ветвь неожиданно дошла до сети.

				called = true
				return nil, errors.New("неожиданная сеть")
			}, Admit: func(context.Context, OutboundOperation) (func(), error) {
				// Выдаёт тестовый допуск с пустым действием освобождения для проверки отказов до сети.

				return func() {
					// Завершает тестовый допуск без изменения состояния.
				}, nil
			}}
			// current содержит проверенное назначение либо нулевое значение для сценария
			// invalid_target.
			current := target
			switch mode {
			case "missing_admit":
				opts.Admit = nil
				// err сообщает об отказе проверки параметров без Admit, которая сама по
				// себе допускает его отсутствие.
				if err := opts.Validate(); err != nil {
					t.Fatal("Prepare должен позволять отсутствие исполняемого Admit", err)
				}
			case "invalid_limits":
				opts.Limits.MessageTimeout = 0
			case "insecure_tls":
				opts.TLSConfig = &tls.Config{InsecureSkipVerify: true}
			case "invalid_target":
				current = OutboundTarget{}
			case "denied_admission":
				opts.Admit = func(context.Context, OutboundOperation) (func(), error) {
					// Отклоняет допуск операции с core.ErrStale для проверки передачи этой причины без сетевого вызова.
					return nil, core.ErrStale
				}
			}
			// err содержит отказ предварительной проверки либо допуска до сетевого
			// подключения.
			_, err := DialOutbound(context.Background(), current, opts)
			if called || err == nil {
				t.Fatalf("сеть до проверки и допуска: called=%v err=%v", called, err)
			}
			if mode == "denied_admission" {
				if !errors.Is(err, core.ErrStale) {
					t.Fatal(err)
				}
			} else {
				// out принимает типизированный отказ для проверки категории
				// OutboundInvalid.
				var out *OutboundError
				if !errors.As(err, &out) || out.Kind != OutboundInvalid {
					t.Fatalf("нет типизированного OutboundInvalid: %v", err)
				}
			}
		})
	}
	// endpoint содержит очередной URL с запрещёнными учётными данными, фрагментом либо недопустимым
	// портом.
	for _, endpoint := range []string{"rtmp://user:pass@receiver.test/app", "rtmp://receiver.test/app#", "rtmp://receiver.test/app#key", "rtmp://receiver.test:0/app", "rtmps://receiver.test:/app"} {
		// err сообщает об отказе разрешения недопустимого URL назначения.
		if _, err := ResolveOutbound(endpoint, "key", target.profile); !IsOutboundTerminal(err) {
			t.Fatalf("недопустимая привязка принята: %q %v", endpoint, err)
		}
	}
}

// Проверяет явное сворачивание сетевой метки при сохранённой логической шкале, отказ скачка 2³¹ и
// включительные границы signed24 CTS.
func TestOutboundAcceptanceTimestampWrapAndSignedCompositionLimits(t *testing.T) {
	// m сохраняет RTMP-байты для проверки явного сворачивания временной метки uint32.
	m := &acceptanceOutboundMemory{}
	// c отправляет сообщения в общей логической шкале через границу сетевой метки.
	c, _, _ := acceptanceOutboundLocal(t, m, false)
	// v задаёт H.264-конфигурацию, a — AAC-конфигурацию в миллисекундной шкале.
	v, a := acceptanceOutboundConfigs(t, false, media.TimeBase{Numerator: 1, Denominator: 1000})
	// err сообщает об отказе сохранения конфигураций перед проверкой оборота временной метки.
	if err := c.WriteHeaders(context.Background(), v, a); err != nil {
		t.Fatal(err)
	}
	// err сохраняет отказы отправки и независимого разбора при проверке временной шкалы.
	var err error
	// ticks задаёт очередные равные PTS/DTS в миллисекундах, постепенно переводящие логическое время
	// через uint32.
	for _, ticks := range []int64{0, 2147483647, 4294967294, 4294967300} {
		if _, err = c.WritePacket(context.Background(), acceptanceOutboundPacket(t, v, ticks, ticks, false, []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40})); err != nil {
			t.Fatal(err)
		}
	}
	// r читает сохранённые сообщения независимым анализатором для сверки сетевых меток.
	r := bytes.NewReader(m.wire.Bytes())
	for range 3 {
		if _, err = acceptanceReadMessage(r, 128); err != nil {
			t.Fatal(err)
		}
	}
	// want задаёт ожидаемую 32-битную сетевую метку очередного видеопакета.
	for _, want := range []uint32{0, 0x7fffffff, 0xfffffffe, 4} {
		// msg содержит очередной разобранный видеопакет. e сообщает об отказе его независимого
		// чтения.
		msg, e := acceptanceReadMessage(r, 128)
		if e != nil || msg.stamp != want {
			t.Fatalf("wire timestamp=%x want=%x %v", msg.stamp, want, e)
		}
	}
	_, err = c.WritePacket(context.Background(), acceptanceOutboundPacket(t, v, 6442450948, 6442450948, false, []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}))
	// out принимает типизированный отказ для проверки неоднозначного скачка либо непредставимого
	// CTS.
	var out *OutboundError
	if !errors.As(err, &out) || out.Kind != OutboundTiming {
		t.Fatalf("неоднозначный скачок последовательного номера принят: %v", err)
	}
	// tc задаёт граничное смещение композиции H.264 и ожидаемые байты либо отказ.
	for _, tc := range []struct {
		// cts задаёт знаковое смещение PTS относительно DTS в миллисекундах.
		cts int64
		// prefix содержит ожидаемые байты H.264-префикса с трёхбайтовым знаковым CTS.
		prefix []byte
		// valid отмечает представимость смещения композиции знаковым 24-битным числом.
		valid bool
	}{
		{-8388608, []byte{0x17, 1, 0x80, 0, 0}, true}, {8388607, []byte{0x17, 1, 0x7f, 255, 255}, true}, {-8388609, nil, false}, {8388608, nil, false},
	} {
		t.Run(fmt.Sprintf("cts_%d", tc.cts), func(t *testing.T) {
			// Проверяет выбранную границу signed24: точные байты допустимого CTS либо отказ до записи при выходе
			// за диапазон.

			// m сохраняет вывод для проверки точного CTS либо отсутствия записи при его
			// непредставимости.
			m := &acceptanceOutboundMemory{}
			// c проверяет кодирование выбранного граничного смещения композиции.
			c, _, _ := acceptanceOutboundLocal(t, m, false)
			// err сообщает об отказе сохранения конфигураций перед проверкой граничного CTS.
			if err := c.WriteHeaders(context.Background(), v, a); err != nil {
				t.Fatal(err)
			}
			// err содержит результат отправки H.264 с выбранным граничным CTS.
			_, err := c.WritePacket(context.Background(), acceptanceOutboundPacket(t, v, tc.cts, 0, false, []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}))
			if !tc.valid {
				if !errors.As(err, &out) || out.Kind != OutboundTiming || m.wire.Len() != 0 {
					t.Fatalf("непредставимый CTS записан: %x %v", m.wire.Bytes(), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// r читает сохранённые сообщения для поиска видеопакета с проверяемым CTS.
			r := bytes.NewReader(m.wire.Bytes())
			for range 3 {
				if _, err = acceptanceReadMessage(r, 128); err != nil {
					t.Fatal(err)
				}
			}
			// msg содержит видеопакет после пропуска метаданных и конфигураций. e сообщает
			// об отказе его независимого разбора.
			msg, e := acceptanceReadMessage(r, 128)
			if e != nil || !bytes.HasPrefix(msg.body, tc.prefix) {
				t.Fatalf("signed24=%x want=%x %v", msg.body, tc.prefix, e)
			}
		})
	}
}

// Проверяет отсутствие записи и подтверждённых медиабайтов для ложного ключевого флага и несовпадающих
// PTS/DTS AAC либо AV1.
func TestOutboundAcceptanceInvalidAccessUnitsNeverBecomeSent(t *testing.T) {
	// mode выбирает ложный ключевой флаг H.264 либо несовпадение PTS/DTS у AAC или AV1.
	for _, mode := range []string{"forged_keyframe", "aac_reorder", "av1_reorder"} {
		t.Run(mode, func(t *testing.T) {
			// Проверяет отказ до записи для подложного признака ключевого кадра либо различающихся PTS/DTS AAC или
			// AV1.

			// m сохраняет вывод для проверки отсутствия новых байтов от недопустимого AU.
			m := &acceptanceOutboundMemory{}
			// c содержит проверяемое соединение. v задаёт H.264 либо AV1-конфигурацию, a —
			// AAC-конфигурацию.
			c, v, a := acceptanceOutboundLocal(t, m, mode == "av1_reorder")
			// err сообщает об отказе сохранения конфигураций перед проверкой недопустимого
			// AU.
			if err := c.WriteHeaders(context.Background(), v, a); err != nil {
				t.Fatal(err)
			}
			// cfg выбирает конфигурацию проверяемой дорожки. pts задаёт метку показа, dts —
			// метку декодирования. payload содержит AU с выбранным нарушением. key задаёт
			// заявленный ключевой флаг пакета.
			cfg, pts, dts, payload, key := v, int64(0), int64(0), []byte{0, 0, 0, 3, 0x41, 0xb8, 0x40}, true
			if mode == "aac_reorder" {
				// err сообщает об отказе первого допустимого H.264 IDR, необходимого
				// перед проверкой переупорядоченного AAC.
				if _, err := c.WritePacket(context.Background(), acceptanceOutboundPacket(t, v, 0, 0, false, []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40})); err != nil {
					t.Fatal(err)
				}
				cfg, pts, payload, key = a, 1, []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c}, false
			}
			if mode == "av1_reorder" {
				pts = 1
				payload = append(v.Headers()[4:], 0x1a, 1, 0x10)
			}
			// before сохраняет длину вывода до отправки недопустимого AU.
			before := m.wire.Len()
			// result содержит сведения о результате отправки недопустимого AU. err содержит
			// ожидаемый окончательный отказ до записи его байтов.
			result, err := c.WritePacket(context.Background(), acceptanceOutboundPacket(t, cfg, pts, dts, key, payload))
			if !IsOutboundTerminal(err) || result.PayloadBytes != 0 || m.wire.Len() != before {
				t.Fatalf("неверный AU принят: %+v err=%v wire delta=%d", result, err, m.wire.Len()-before)
			}
		})
	}
}

// Проверяет единый срок 100 мс для медленно принимаемого сообщения: частичные записи не продлевают
// ожидание, учитываются только шесть принятых байтов.
func TestOutboundAcceptancePartialWritesCannotRenewWholeMessageDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// При управляемом времени проверяет, что медленные частичные записи не продлевают общий срок сообщения
		// и учитываются ровно принятые байты.

		// client служит отправляющим концом локального соединения, peer — медленно читающим
		// концом получателя.
		client, peer := net.Pipe()
		defer func() {
			// Закрывает принимающий конец net.Pipe после проверки срока полной записи.
			_ = peer.Close()
		}()
		// c проверяет общий срок сообщения при положительном частичном прогрессе записи.
		c, _, _ := acceptanceOutboundLocal(t, client, false)
		c.opts.Limits.WriteTimeout = 100 * time.Millisecond
		// serverDone закрывается после выхода горутины медленного получателя.
		serverDone := make(chan struct{})
		go func() {
			// Читает по три байта с паузами 60 мс, создавая частичный прогресс записи до общего срока, и закрывает
			// serverDone при выходе.

			defer close(serverDone)
			// b принимает очередную трёхбайтовую порцию медленно читаемого сообщения.
			var b [3]byte
			for {
				// err сообщает об отказе чтения следующей трёхбайтовой порции после
				// закрытия соединения.
				if _, err := io.ReadFull(peer, b[:]); err != nil {
					return
				}
				time.Sleep(60 * time.Millisecond)
			}
		}()
		// start отмечает начало записи для проверки общего срока ровно 100 миллисекунд при
		// управляемом времени.
		start := time.Now()
		// err содержит итог записи, которая должна остановиться по общему сроку после шести
		// принятых байтов.
		err := c.writeMessage(context.Background(), 6, 9, 0x765432, 0, bytes.Repeat([]byte{7}, 257))
		// timeout принимает сетевую ошибку для проверки признака истечения общего срока записи.
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() || time.Since(start) != 100*time.Millisecond || c.Snapshot().WrittenBytes != 6 {
			t.Fatalf("срок полной записи сообщения: elapsed=%s stats=%+v err=%v", time.Since(start), c.Snapshot(), err)
		}
		<-serverDone
	})
}
