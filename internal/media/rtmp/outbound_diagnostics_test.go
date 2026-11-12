package rtmp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/handshake"
)

// Строит независимый fmt0-заголовок с заданными csid, типом, MessageStreamID и телом для проверки
// диагностики.
func diagnosticFrame(csid, typ byte, stream uint32, body []byte) []byte {
	// h содержит полный заголовок fmt0 перед записью MessageStreamID и присоединением тела.
	h := []byte{csid, 0, 0, 0, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body)), typ, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(h[8:], stream)
	return append(h, body...)
}

// Сериализует заданные AMF-значения теста, завершая тест при ошибке подготовки данных.
func diagnosticAMF(t *testing.T, data amf0.Data) []byte {
	t.Helper()
	// b содержит сериализованные значения AMF для входного сообщения теста. err сообщает об отказе
	// их кодирования.
	b, err := data.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// diagnosticReadLoop исполняет настоящий парсер и диспетчер без фоновых записей.
func diagnosticReadLoop(t *testing.T, wire []byte, setup func(*OutboundConn)) (*OutboundConn, error) {
	t.Helper()
	// c исполняет настоящий цикл чтения над заданными байтами без фонового отправителя управления.
	c := transportSelfConn(t, &transportSelfMemory{input: bytes.NewReader(wire)})
	c.readerDone = make(chan struct{})
	c.commands = make(chan outboundCommand, 4)
	if setup != nil {
		setup(c)
	}
	c.readLoop()
	select {
	case <-c.readerDone:
	default:
		t.Fatal("читатель не подтвердил завершение")
	}
	return c, c.Err()
}

// Проверяет исходную категорию errors.Is, окончательный OutboundProtocol и наличие каждого точного
// диагностического поля отдельным токеном.
func diagnosticProtocol(t *testing.T, err error, cause error, fields ...string) {
	t.Helper()
	// out принимает типизированную ошибку для проверки категории OutboundProtocol.
	var out *OutboundError
	if !errors.Is(err, cause) || !errors.As(err, &out) || out.Kind != OutboundProtocol || !IsOutboundTerminal(err) {
		t.Fatalf("потеряна причина или классификация protocol: %v", err)
	}
	// field задаёт очередное точное диагностическое поле, которое должно присутствовать отдельным
	// токеном.
	for _, field := range fields {
		// found отмечает нахождение текущего диагностического поля в тексте ошибки.
		found := false
		// token содержит очередной отделённый пробелами фрагмент диагностики для точного
		// сравнения поля.
		for token := range strings.FieldsSeq(err.Error()) {
			if strings.TrimSuffix(token, ":") == field {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("в диагностике нет %q: %v", field, err)
		}
	}
}

// Проверяет диагностику неизвестного сжатого заголовка, нового заголовка внутри сообщения и
// несовпадающей расширенной метки с фактическими полями.
func TestOutboundDiagnosticsChunkFailures(t *testing.T) {
	// partial содержит первый блок сообщения длиной 129 байт, оставляющий один байт тела
	// непрочитанным.
	partial := diagnosticFrame(4, 20, 0x01020304, bytes.Repeat([]byte{5}, 129))
	partial = partial[:12+128]
	// extended содержит вариант незавершённого сообщения с расширенной временной меткой.
	extended := bytes.Clone(partial)
	copy(extended[1:4], []byte{255, 255, 255})
	extended = append(extended[:12], append([]byte{1, 0, 0, 2}, extended[12:]...)...)
	// tc задаёт повреждённый заголовок либо продолжение и обязательные поля его диагностики.
	for _, tc := range []struct {
		// name задаёт имя сценария повреждения блока.
		name string
		// wire содержит входящие байты, вызывающие проверяемый отказ разбора блока.
		wire []byte
		// fields перечисляет точные диагностические поля ожидаемого отказа разбора блока.
		fields []string
	}{
		{"unknown_compressed_header", []byte{0xc4}, []string{"phase=chunk_header", "csid=4", "fmt=3", "known=false", "active=false"}},
		{"new_header_during_message", append(bytes.Clone(partial), 4), []string{"phase=chunk_header", "csid=4", "fmt=0", "active=true", "received=128", "message_len=129"}},
		{"mismatched_extended_timestamp", append(bytes.Clone(extended), 0xc4, 1, 0, 0, 3), []string{"phase=extended_timestamp", "csid=4", "fmt=3", "extended_timestamp=16777219", "expected_extended_timestamp=16777218"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет категорию ErrFraming и выбранные диагностические поля для повреждённого заголовка либо
			// продолжения блока.

			// err содержит отказ цикла чтения повреждённого заголовка либо продолжения.
			_, err := diagnosticReadLoop(t, tc.wire, nil)
			diagnosticProtocol(t, err, ErrFraming, tc.fields...)
		})
	}
}

// Проверяет сохранение ErrFraming и точных типа, длины, адресации и значений некорректных управляющих
// сообщений.
func TestOutboundDiagnosticsControlFailures(t *testing.T) {
	// tc задаёт повреждённое управляющее сообщение и ожидаемые дополнительные поля диагностики.
	for _, tc := range []struct {
		// name задаёт имя сценария повреждённого управляющего сообщения.
		name string
		// csid задаёт идентификатор потока блоков управляющего сообщения. typ задаёт его
		// RTMP-тип.
		csid, typ byte
		// stream задаёт MessageStreamID управляющего сообщения для проверки адресации.
		stream uint32
		// body содержит тело проверяемого управляющего сообщения.
		body []byte
		// fields перечисляет дополнительные диагностические поля конкретного повреждения
		// управления.
		fields []string
	}{
		{"wrong_chunk_stream", 7, 3, 0, []byte{0, 0, 0, 0}, []string{"csid=7", "stream_id=0"}},
		{"wrong_message_stream", 2, 3, 23, []byte{0, 0, 0, 0}, []string{"csid=2", "stream_id=23"}},
		{"short_chunk_size", 2, 1, 0, []byte{0, 1, 0}, nil},
		{"short_abort", 2, 2, 0, []byte{0}, nil},
		{"invalid_abort_stream", 2, 2, 0, []byte{0, 0, 0, 1}, []string{"abort_csid=1"}},
		{"short_ack", 2, 3, 0, []byte{1}, nil},
		{"short_user_event", 2, 4, 0, []byte{0}, nil},
		{"wrong_event_length", 2, 4, 0, []byte{0, 3, 0, 0, 0, 1}, []string{"event=3", "expected_body_len=10"}},
		{"unknown_user_event", 2, 4, 0, []byte{0, 99, 0, 0, 0, 0}, []string{"event=99"}},
		{"zero_ack_window", 2, 5, 0, []byte{0, 0, 0, 0}, []string{"window=0"}},
		{"invalid_bandwidth_kind", 2, 6, 0, []byte{0, 0, 0, 1, 3}, []string{"window=1", "limit_type=3"}},
		{"zero_bandwidth_window", 2, 6, 0, []byte{0, 0, 0, 0, 0}, []string{"window=0", "limit_type=0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет сохранение причины и адресации выбранного повреждённого управляющего сообщения в
			// диагностике.

			// err содержит отказ обработки выбранного некорректного управляющего сообщения.
			_, err := diagnosticReadLoop(t, diagnosticFrame(tc.csid, tc.typ, tc.stream, tc.body), nil)
			// fields содержит общие поля типа и длины управляющего сообщения перед
			// добавлением полей сценария.
			fields := []string{"phase=control", fmt.Sprintf("type=%d", tc.typ), fmt.Sprintf("body_len=%d", len(tc.body))}
			diagnosticProtocol(t, err, ErrFraming, append(fields, tc.fields...)...)
		})
	}
}

// Проверяет, что числовые будущие, устаревшие и неоднозначные ACK не становятся протокольным отказом и
// дают ожидаемые опорную величину и события.
func TestOutboundDiagnosticsAcknowledgementValues(t *testing.T) {
	// tc задаёт исходные счётчики, полученный ACK и ожидаемое изменение наблюдений.
	for _, tc := range []struct {
		// name задаёт имя числового сценария ACK.
		name string
		// previous задаёт исходный накопительный якорь подтверждённых байтов. sent задаёт
		// исходное число физически записанных байтов.
		previous, sent uint64
		// ack задаёт полученный 32-битный последовательный номер ACK.
		ack uint32
		// want задаёт ожидаемый накопительный якорь после обработки ACK.
		want uint64
		// events задаёт ожидаемое число зарегистрированных опережающих ACK.
		events uint64
	}{
		{"future", 100, 120, 124, 124, 1},
		{"backward", 100, 120, 99, 100, 0},
		{"half_range", 100, (1 << 31) + 100, (1 << 31) + 100, 100, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет, что выбранное числовое значение ACK меняет лишь допустимые наблюдения, а последующий EOF
			// остаётся сетевым завершением.

			// body содержит последовательный номер проверяемого ACK в сетевом порядке
			// байтов.
			var body [4]byte
			binary.BigEndian.PutUint32(body[:], tc.ack)
			// c хранит состояние после обработки ACK. err содержит последующий EOF либо
			// отказ чтения для проверки классификации.
			c, err := diagnosticReadLoop(t, diagnosticFrame(2, 3, 0, body[:]), func(c *OutboundConn) {
				// Устанавливает исходное опорное значение ACK и счётчик записанных байтов для выбранного сценария.

				c.peerAcknowledged, c.stats.WrittenBytes = tc.previous, tc.sent
			})
			if !errors.Is(err, io.EOF) || IsOutboundTerminal(err) || c.peerAcknowledged != tc.want || c.Snapshot().AckAheadEventsTotal != tc.events {
				t.Fatalf("числовой ACK нарушил диагностику: ack=%d stats=%+v err=%v", c.peerAcknowledged, c.Snapshot(), err)
			}
		})
	}
}

// Проверяет диагностику предварительной проверки AMF, формы команды, имени и транзакции с исходными
// csid, потоком и длиной тела.
func TestOutboundDiagnosticsAMFFailures(t *testing.T) {
	// tc задаёт повреждённую AMF-команду, исходную категорию отказа и обязательную диагностику.
	for _, tc := range []struct {
		// name задаёт имя сценария повреждения AMF-команды.
		name string
		// body содержит AMF-тело с проверяемым нарушением структуры либо формы команды.
		body []byte
		// cause задаёт ожидаемую исходную категорию ErrFraming либо ErrLimit.
		cause error
		// fields перечисляет поля диагностики конкретного нарушения AMF-команды.
		fields []string
	}{
		{"truncated_string", []byte{2, 0, 5, 'x'}, ErrFraming, []string{"phase=amf_preflight"}},
		{"element_limit", []byte{10, 0xff, 0xff, 0xff, 0xff}, ErrLimit, []string{"phase=amf_preflight"}},
		{"short_command", diagnosticAMF(t, amf0.Data{"_result", float64(1)}), ErrFraming, []string{"phase=command_shape", "values_len=2"}},
		{"numeric_command_name", diagnosticAMF(t, amf0.Data{float64(9), float64(1), nil}), ErrFraming, []string{"phase=command_name", "value_type=float64"}},
		{"string_transaction", diagnosticAMF(t, amf0.Data{"_result", "invalid", nil}), ErrFraming, []string{"phase=command_transaction", "value_type=string"}},
		{"nan_transaction", diagnosticAMF(t, amf0.Data{"_result", math.NaN(), nil}), ErrFraming, []string{"phase=command_transaction", "transaction=NaN"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет причину и диагностические поля выбранной ошибки структуры AMF либо формы команды.

			// err содержит отказ обработки выбранного повреждённого AMF-тела.
			_, err := diagnosticReadLoop(t, diagnosticFrame(11, 20, 73, tc.body), nil)
			// fields объединяет адресацию и длину AMF-сообщения с полями конкретного отказа.
			fields := append([]string{"type=20", "csid=11", "stream_id=73", fmt.Sprintf("body_len=%d", len(tc.body))}, tc.fields...)
			diagnosticProtocol(t, err, tc.cause, fields...)
		})
	}
}

// Проверяет окончательную диагностику неподдержанного типа и лишней команды после Publish.Start без её
// передачи в очередь запуска.
func TestOutboundDiagnosticsUnexpectedMessages(t *testing.T) {
	t.Run("unexpected_type", func(t *testing.T) {
		// Проверяет окончательный отказ с адресацией при неожиданном сообщении типа 18.

		// err содержит отказ обработки неожиданного сообщения типа 18.
		_, err := diagnosticReadLoop(t, diagnosticFrame(9, 18, 55, []byte{5}), nil)
		diagnosticProtocol(t, err, ErrFraming, "phase=message_type", "type=18", "csid=9", "stream_id=55", "body_len=1")
	})
	t.Run("command_after_publish", func(t *testing.T) {
		// Проверяет отказ неожиданной команды после NetStream.Publish.Start и отсутствие передачи лишнего
		// ответа в очередь установки.

		// start содержит AMF-уведомление об успешном начале публикации.
		start := diagnosticAMF(t, amf0.Data{"onStatus", float64(0), nil, amf0.Object{{Key: "code", Value: "NetStream.Publish.Start"}}})
		// extra содержит лишний ответ _result с транзакцией 17 после начала публикации.
		extra := diagnosticAMF(t, amf0.Data{"_result", float64(17), nil})
		// wire объединяет успешный Publish.Start и последующую запрещённую команду.
		wire := append(diagnosticFrame(3, 20, 0x01020304, start), diagnosticFrame(12, 20, 0x01020304, extra)...)
		// c хранит очередь ответов после обработки двух команд. err содержит отказ лишней
		// команды после публикации.
		c, err := diagnosticReadLoop(t, wire, nil)
		diagnosticProtocol(t, err, ErrFraming, "phase=post_publish", "command=\"_result\"", "transaction=17", "csid=12", "stream_id=16909060")
		if len(c.commands) != 1 {
			t.Fatal("лишняя команда передана как ответ установки соединения")
		}
	})
}

// Проверяет сохранение полного csid=320 в диагностике управления, типа сообщения, AMF и лишней команды
// после публикации.
func TestOutboundDiagnosticsLargeChunkStreamIDs(t *testing.T) {
	// start содержит уведомление Publish.Start для сценария отказа после начала публикации.
	start := diagnosticAMF(t, amf0.Data{"onStatus", float64(0), nil, amf0.Object{{Key: "code", Value: "NetStream.Publish.Start"}}})
	// tc задаёт сообщение с большим csid и ожидаемую фазу диагностики.
	for _, tc := range []struct {
		// name задаёт имя диагностического сценария с csid=320.
		name string
		// typ задаёт RTMP-тип сообщения с большим идентификатором потока блоков.
		typ byte
		// stream задаёт MessageStreamID сообщения с csid=320.
		stream uint32
		// body содержит тело сообщения, вызывающего выбранный отказ.
		body []byte
		// prefix содержит предшествующее уведомление Publish.Start, когда отказ проверяется
		// после публикации.
		prefix []byte
		// phase задаёт ожидаемую фазу отказа в диагностике.
		phase string
	}{
		{"control_address", 3, 0, []byte{0, 0, 0, 0}, nil, "control"},
		{"unexpected_type", 18, 55, []byte{5}, nil, "message_type"},
		{"malformed_amf", 20, 55, []byte{2, 0, 5, 'x'}, nil, "amf_preflight"},
		{"post_publish", 20, 0x01020304, diagnosticAMF(t, amf0.Data{"_result", float64(17), nil}), diagnosticFrame(3, 20, 0x01020304, start), "post_publish"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет сохранение полного csid=320 и адресации в выбранной ошибке управляющего сообщения либо
			// команды.

			// wire содержит сообщение с трёхбайтовым базовым заголовком, кодирующим csid=64+0+256=320.
			wire := append([]byte{1, 0, 1}, diagnosticFrame(2, tc.typ, tc.stream, tc.body)[1:]...)
			// err содержит отказ обработки сообщения, чей csid=320 должен сохраниться
			// полностью.
			_, err := diagnosticReadLoop(t, append(bytes.Clone(tc.prefix), wire...), nil)
			diagnosticProtocol(t, err, ErrFraming, "phase="+tc.phase, "csid=320", fmt.Sprintf("type=%d", tc.typ), fmt.Sprintf("stream_id=%d", tc.stream))
		})
	}
}

// Проверяет допустимые границы управляющих сообщений, включая MaxUint32 окна, и точные байты
// поставленного в очередь PingResponse.
func TestOutboundDiagnosticsValidControls(t *testing.T) {
	// tc задаёт допустимое управляющее сообщение на проверяемой границе значений.
	for _, tc := range []struct {
		// name задаёт имя сценария допустимого управления.
		name string
		// typ задаёт RTMP-тип допустимого управляющего сообщения.
		typ byte
		// body содержит тело управляющего сообщения с допустимыми граничными значениями.
		body []byte
	}{
		{"minimum_chunk", 1, []byte{0, 0, 0, 1}},
		{"maximum_chunk", 1, []byte{0, 1, 0, 0}},
		{"abort_minimum_id", 2, []byte{0, 0, 0, 2}},
		{"abort_maximum_id", 2, []byte{0, 1, 0, 63}},
		{"stream_begin", 4, []byte{0, 0, 0, 0, 0, 37}},
		{"set_buffer_length", 4, []byte{0, 3, 0, 0, 0, 37, 0, 0, 1, 0}},
		{"ping_request", 4, []byte{0, 6, 1, 2, 3, 4}},
		{"minimum_ack_window", 5, []byte{0, 0, 0, 1}},
		{"maximum_peer_window", 6, []byte{0xff, 0xff, 0xff, 0xff, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет принятие выбранного допустимого управляющего сообщения и точный ответ для PingRequest.

			// c хранит ответы на допустимое управление. err содержит последующий EOF либо
			// отказ для проверки отсутствия протокольной ошибки.
			c, err := diagnosticReadLoop(t, diagnosticFrame(2, tc.typ, 0, tc.body), nil)
			if !errors.Is(err, io.EOF) || IsOutboundTerminal(err) {
				t.Fatalf("допустимое управление отклонено: %v", err)
			}
			if tc.name == "ping_request" {
				select {
				// response содержит поставленный в очередь PingResponse для проверки
				// типа и точных байтов ответа.
				case response := <-c.controls:
					if response.typ != 4 || !bytes.Equal(response.body, []byte{0, 7, 1, 2, 3, 4}) {
						t.Fatalf("изменён ответ PingResponse: %+v", response)
					}
				default:
					t.Fatal("ответ PingResponse не поставлен в очередь")
				}
			}
		})
	}
}

// diagnosticFailedRead подменяет чтение транспорта заданной ошибкой, сохраняя остальные операции
// тестового соединения.
type diagnosticFailedRead struct {
	// transportSelfMemory предоставляет остальные операции и состояние тестового транспорта при
	// подменённом Read.
	*transportSelfMemory
	// err задаёт исходную ошибку, которую каждое чтение возвращает без данных.
	err error
}

// Всегда возвращает заданную исходную ошибку чтения без байтов для проверки сохранения транспортной
// категории.
func (c diagnosticFailedRead) Read([]byte) (int, error) { return 0, c.err }

// Проверяет, что io.ErrUnexpectedEOF, отмена и сетевые таймауты сохраняют исходную причину и не
// превращаются в окончательный ErrFraming.
func TestOutboundDiagnosticsNetworkFailuresPreserveClassification(t *testing.T) {
	// original задаёт очередную транспортную ошибку, чья причина и неокончательная классификация
	// должны сохраниться.
	for _, original := range []error{io.ErrUnexpectedEOF, context.Canceled, context.DeadlineExceeded, &net.DNSError{Err: "синтетический таймаут", IsTimeout: true}} {
		t.Run(fmt.Sprintf("%T_%v", original, original), func(t *testing.T) {
			// Проверяет сохранение исходной сетевой причины без превращения её в окончательный протокольный отказ.

			// c исполняет цикл чтения с транспортом, немедленно возвращающим выбранную
			// исходную ошибку.
			c := transportSelfConn(t, diagnosticFailedRead{transportSelfMemory: &transportSelfMemory{}, err: original})
			c.readerDone = make(chan struct{})
			c.readLoop()
			if !errors.Is(c.Err(), original) || IsOutboundTerminal(c.Err()) || errors.Is(c.Err(), ErrFraming) {
				t.Fatalf("транспортная причина стала протокольным отказом: %v", c.Err())
			}
		})
	}
}

// Проверяет корректные расширенные метки и граничные ACK, а также сохранение транспортного EOF при
// усечении заголовка или тела.
func TestOutboundDiagnosticsValidBoundariesAndNetworkEOF(t *testing.T) {
	t.Run("matching_extended_timestamp", func(t *testing.T) {
		// Проверяет сборку сообщения с совпадающими расширенными метками, точными данными и MessageStreamID.

		// wire содержит корректное сообщение с совпадающими расширенными метками в обоих блоках.
		wire := []byte{4, 255, 255, 255, 0, 0, 2, 20, 4, 3, 2, 1, 1, 0, 0, 2, 5, 0xc4, 1, 0, 0, 2, 6}
		// c предоставляет транспорт с корректной расширенной временной меткой.
		c := transportSelfConn(t, &transportSelfMemory{input: bytes.NewReader(wire)})
		// r собирает сообщение из однобайтовых блоков с расширенными метками.
		r := outboundReader{c: c, streams: make(map[uint32]*outboundChunkStream), chunkSize: 1}
		// m содержит собранное сообщение для проверки тела, времени и MessageStreamID. err
		// сообщает об отказе его разбора.
		m, err := r.next()
		if err != nil || m == nil || !bytes.Equal(m.Body, []byte{5, 6}) || m.Timestamp != 16777218*time.Millisecond || m.MessageStreamID != 0x01020304 {
			t.Fatalf("корректное продолжение изменено: %+v %v", m, err)
		}
	})
	// tc задаёт допустимый граничный ACK и ожидаемый накопительный якорь.
	for _, tc := range []struct {
		// name задаёт имя граничного сценария ACK.
		name string
		// previous задаёт исходное число подтверждённых байтов до обработки ACK.
		previous uint64
		// sent задаёт исходное число физически записанных байтов.
		sent uint64
		// flight отмечает моделируемое наличие активной записи ненулевым значением; размер
		// записи в обработчик не передаётся.
		flight uint64
		// ack задаёт полученный 32-битный последовательный номер ACK.
		ack uint32
		// want задаёт ожидаемый накопительный якорь подтверждённых байтов.
		want uint64
	}{
		{"duplicate_ack", 100, 120, 0, 100, 100},
		{"ack_in_flight_boundary", 100, 120, 3, 123, 123},
		{"ack_wrap", (1 << 32) - 8, (1 << 32) + 8, 0, 8, (1 << 32) + 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет принятие выбранного допустимого ACK, включая дубликат, активную запись и сворачивание
			// последовательного номера.

			// body содержит проверяемый ACK в сетевом порядке байтов.
			var body [4]byte
			binary.BigEndian.PutUint32(body[:], tc.ack)
			// c хранит состояние после допустимого ACK. err содержит последующий EOF либо
			// отказ для проверки классификации.
			c, err := diagnosticReadLoop(t, diagnosticFrame(2, 3, 0, body[:]), func(c *OutboundConn) {
				// Задаёт исходные счётчики ACK и признак активной записи для выбранного допустимого граничного случая.

				c.peerAcknowledged, c.stats.WrittenBytes = tc.previous, tc.sent
				c.writeActive = tc.flight > 0
			})
			if !errors.Is(err, io.EOF) || IsOutboundTerminal(err) || c.peerAcknowledged != tc.want {
				t.Fatalf("допустимый ACK отклонён: ack=%d err=%v", c.peerAcknowledged, err)
			}
		})
	}
	// wire содержит очередной обрыв входящего заголовка либо тела сообщения.
	for _, wire := range [][]byte{nil, {4}, {4, 0, 0}, {4, 0, 0, 0, 0, 0, 4, 20, 0, 0, 0, 0, 5}} {
		t.Run(fmt.Sprintf("truncated_%d", len(wire)), func(t *testing.T) {
			// Проверяет, что EOF при выбранном обрыве заголовка либо тела не становится ErrFraming или
			// окончательным отказом.

			// err содержит результат чтения усечённого сообщения для проверки сохранения
			// транспортного EOF.
			_, err := diagnosticReadLoop(t, wire, nil)
			if !errors.Is(err, io.EOF) || errors.Is(err, ErrFraming) || IsOutboundTerminal(err) {
				t.Fatalf("EOF ошибочно стал протокольным отказом: %v", err)
			}
		})
	}
}

// Имитирует получателя запуска и по выбранной фазе возвращает неверную транзакцию либо дробный
// StreamID для проверки диагностического контекста.
func diagnosticStartupPeer(peer net.Conn, stage string) error {
	// err сообщает об отказе серверного рукопожатия тестового получателя.
	if _, _, err := handshake.DoServer(peer, true); err != nil {
		return err
	}
	// err сообщает об отказе чтения клиентской команды connect.
	if _, err := transportSelfReadFrame(peer, 128); err != nil {
		return err
	}
	// transaction задаёт номер транзакции ответа текущей стадии, при выбранном сценарии намеренно
	// неверный.
	transaction := float64(1)
	if stage == "connect" {
		transaction = 9
	}
	// err сообщает об отказе отправки ответа connect.
	if err := transportSelfWriteAMF(peer, amf0.Data{"_result", transaction, nil, amf0.Object{{Key: "code", Value: "NetConnection.Connect.Success"}}}, 0); err != nil || stage == "connect" {
		return err
	}
	// f содержит сообщение SetChunkSize клиента. err сообщает об отказе его чтения.
	f, err := transportSelfReadFrame(peer, 128)
	if err != nil {
		return err
	}
	if f.typ != 1 || len(f.body) != 4 {
		return errors.New("не получен SetChunkSize")
	}
	// chunkSize задаёт принятый от клиента размер блоков для чтения последующих команд.
	chunkSize := int(binary.BigEndian.Uint32(f.body))
	// err сообщает об отказе чтения команды createStream.
	if _, err := transportSelfReadFrame(peer, chunkSize); err != nil {
		return err
	}
	// id задаёт идентификатор создаваемого потока либо намеренно недопустимое дробное значение.
	id := float64(37)
	if stage == "create_stream_id" {
		id = 1.5
	}
	transaction = 2
	if stage == "create_stream" {
		transaction = 19
	}
	// err сообщает об отказе отправки ответа createStream.
	if err := transportSelfWriteAMF(peer, amf0.Data{"_result", transaction, nil, id}, 0); err != nil || stage == "create_stream_id" || stage == "create_stream" {
		return err
	}
	// err сообщает об отказе чтения клиентской команды publish.
	if _, err := transportSelfReadFrame(peer, chunkSize); err != nil {
		return err
	}
	transaction = 0
	if stage == "publish" {
		transaction = 29
	}
	return transportSelfWriteAMF(peer, amf0.Data{"onStatus", transaction, nil, amf0.Object{{Key: "code", Value: "NetStream.Publish.Start"}}}, 37)
}

// Проверяет диагностические поля отказов connect/createStream/publish и отдельный успешный запуск с
// StreamID=37.
func TestOutboundDiagnosticsStartupFailures(t *testing.T) {
	// tc задаёт стадию запуска и обязательные диагностические поля либо успешный сценарий.
	for _, tc := range []struct {
		// stage выбирает ошибочную стадию запуска или полноценный успешный запуск.
		stage string
		// fields перечисляет точные поля диагностики отказа выбранной стадии.
		fields []string
	}{
		{"connect", []string{"phase=connect", "transaction=9", "command=\"_result\"", "status_code=\"NetConnection.Connect.Success\""}},
		{"create_stream", []string{"phase=createStream", "transaction=19", "command=\"_result\""}},
		{"create_stream_id", []string{"phase=createStream", "stream_id_value=1.5"}},
		{"publish", []string{"phase=publish", "transaction=29", "stream_id=37", "status_code=\"NetStream.Publish.Start\""}},
		{"valid", nil},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			// Проверяет диагностические поля выбранного ошибочного ответа установки либо успешное получение
			// streamID=37.

			// client служит клиентским концом локального соединения, peer — концом
			// синтетического RTMP-сервера.
			client, peer := net.Pipe()
			// peerDone передаёт результат ответов тестового сервера на команды запуска.
			peerDone := make(chan error, 1)
			// joined закрывается после полного выхода горутины тестового сервера.
			joined := make(chan struct{})
			t.Cleanup(func() {
				// Закрывает оба конца net.Pipe и с ограниченным ожиданием проверяет выход тестового принимающего узла.

				_ = client.Close()
				_ = peer.Close()
				select {
				case <-joined:
				case <-time.After(3 * time.Second):
					t.Error("синтетический RTMP peer не завершился")
				}
			})
			// err сообщает об отказе установки общего срока операций тестового сервера.
			if err := peer.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			go func() {
				// Выполняет сценарий ответов выбранной стадии, сообщает результат и при успехе читает до закрытия,
				// закрывая joined при выходе.

				defer close(joined)
				// err содержит результат выбранного сценария серверных ответов при
				// запуске.
				err := diagnosticStartupPeer(peer, tc.stage)
				peerDone <- err
				if err == nil {
					_, _ = io.Copy(io.Discard, peer)
				}
			}()
			// ctx ограничивает подключение и ожидание завершения тремя секундами. cancel
			// освобождает ресурсы этого срока.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			// target задаёт тестовое назначение для проверки диагностического контекста
			// запуска.
			target := transportSelfConn(t, &transportSelfMemory{}).target
			// opts задаёт тестовые ограничения, локальный транспорт и допуск операций
			// запуска.
			opts := OutboundOptions{Limits: transportSelfLimits(), DialContext: func(context.Context, string, string) (net.Conn, error) {
				// Возвращает клиентский конец net.Pipe для проверки диагностики установки.
				return client, nil
			}, Admit: func(context.Context, OutboundOperation) (func(), error) {
				// Выдаёт тестовый допуск с пустым действием освобождения при проверке диагностики установки.

				return func() {
					// Завершает тестовый допуск без изменения состояния.
				}, nil
			}}
			// c содержит исходящее соединение при успешном запуске. err сообщает об отказе
			// запуска для проверки диагностических полей.
			c, err := DialOutbound(ctx, target, opts)
			if tc.stage == "valid" {
				if err != nil || c.streamID.Load() != 37 {
					t.Fatalf("допустимая публикация отклонена: %v", err)
				}
				_ = c.Close()
				// err сообщает об отказе ожидания завершения успешно запущенного
				// соединения после Close.
				if err := c.Wait(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				diagnosticProtocol(t, err, ErrFraming, tc.fields...)
			}
			// err принимает результат сценария ответов синтетического RTMP-сервера.
			if err := <-peerDone; err != nil {
				t.Fatalf("ошибка синтетического peer: %v", err)
			}
		})
	}
}
