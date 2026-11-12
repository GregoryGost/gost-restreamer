package rtmp

import (
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/handshake"
)

// Проверяет, что независимый тестовый получатель принимает WindowAckSize и ACK перед SetChunkSize,
// createStream и publish.
func TestOutboundStartupPeerAcceptsControlInterleaving(t *testing.T) {
	// stage выбирает точку между командами установки для вставки допустимого управления.
	for _, stage := range []string{"before_chunk_size", "before_create_stream", "before_publish"} {
		t.Run(stage, func(t *testing.T) {
			// Проверяет допустимость WindowAcknowledgementSize и ACK в выбранной точке между командами установки
			// публикации.

			// peerErr содержит результат независимого получателя. clientErr содержит
			// результат клиентского сценария установки.
			peerErr, clientErr := transportSelfScriptStartup(t, stage, 5, []byte{0, 0, 1, 0}, 0, 2)
			if peerErr != nil || clientErr != nil {
				t.Fatalf("допустимое управление между командами: peer=%v client=%v", peerErr, clientErr)
			}
		})
	}
}

// Проверяет отказ самого тестового получателя для неверного типа, длины, окна и адресации управляющего
// сообщения между командами запуска.
func TestOutboundStartupPeerRejectsInvalidInterleaving(t *testing.T) {
	// tc содержит неверное управляющее сообщение для проверки самого независимого получателя.
	for _, tc := range []struct {
		// name обозначает нарушение управления в имени подтеста.
		name string
		// typ задаёт тип вставляемого сообщения.
		typ byte
		// body содержит тело вставляемого управления.
		body []byte
		// stream задаёт MessageStreamID вставляемого сообщения.
		stream uint32
		// chunkStream задаёт идентификатор потока блоков вставляемого сообщения.
		chunkStream byte
	}{
		{"unknown_type", 9, []byte{0, 0, 1, 0}, 0, 2},
		{"short_window", 5, []byte{1}, 0, 2},
		{"short_ack", 3, []byte{1}, 0, 2},
		{"zero_window", 5, []byte{0, 0, 0, 0}, 0, 2},
		{"nonzero_window_stream", 5, []byte{0, 0, 1, 0}, 1, 2},
		{"nonzero_ack_stream", 3, []byte{0, 0, 12, 1}, 1, 2},
		{"wrong_chunk_stream", 5, []byte{0, 0, 1, 0}, 0, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет ErrFraming для выбранного недопустимого управляющего сообщения между командами установки.

			// peerErr содержит отказ независимого получателя при выбранном нарушении.
			peerErr, _ := transportSelfScriptStartup(t, "before_create_stream", tc.typ, tc.body, tc.stream, tc.chunkStream)
			if !errors.Is(peerErr, ErrFraming) {
				t.Fatalf("некорректное управление принято: %v", peerErr)
			}
		})
	}
}

// transportSelfScriptStartup задаёт порядок сообщений без зависимости от планировщика.
func transportSelfScriptStartup(t *testing.T, stage string, typ byte, body []byte, stream uint32, chunkStream byte) (error, error) {
	t.Helper()
	// client содержит клиентский конец net.Pipe. peer содержит конец независимого получателя.
	client, peer := net.Pipe()
	defer func() {
		// Закрывает оба конца net.Pipe после заданного обмена установки.
		_ = client.Close()
		_ = peer.Close()
	}()
	// deadline ограничивает весь сценарий обмена одной секундой.
	deadline := time.Now().Add(time.Second)
	// err сообщает об отказе установки клиентского срока.
	if err := client.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе установки срока получателя.
	if err := peer.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	// peerDone передаёт результат независимого получателя.
	peerDone := make(chan error, 1)
	go func() {
		// Исполняет сценарий принимающего узла, закрывает его сокет и сообщает результат через peerDone.

		// err содержит результат сценария получателя с окном 256.
		err := transportSelfStartupWindow(peer, "key", 256)
		_ = peer.Close()
		peerDone <- err
	}()
	// run выполняет управляемую клиентскую последовательность с вставкой выбранного сообщения.
	run := func() error {
		// Выполняет клиентское согласование и команды установки с управляемой вставкой сообщения, проверяя
		// завершающий onStatus.

		// err сообщает об отказе клиентского RTMP-согласования.
		if _, _, err := handshake.DoClient(client, false, false); err != nil {
			return err
		}
		// err сообщает об отказе записи connect.
		if err := transportSelfWriteAMF(client, amf0.Data{"connect", float64(1), nil}, 0); err != nil {
			return err
		}
		// want задаёт ожидаемый очередной тип ответа: окно, ACK или результат connect.
		for _, want := range []byte{6, 3, 20} {
			// f содержит очередной ответ получателя. err сообщает об отказе чтения.
			f, err := transportSelfReadFrame(client, 128)
			if err != nil {
				return err
			}
			if f.typ != want {
				return ErrFraming
			}
		}
		// send вставляет выбранное управление и ACK только в указанной точке обмена.
		send := func(at string) error {
			// В выбранной точке отправляет заданное сообщение и ACK; в остальных точках оставляет обмен без
			// вставки.

			if at != stage {
				return nil
			}
			// h содержит заголовок вставляемого сообщения с выбранными адресацией и типом.
			h := []byte{chunkStream, 0, 0, 0, 0, 0, byte(len(body)), typ, 0, 0, 0, 0}
			binary.LittleEndian.PutUint32(h[8:], stream)
			// err сообщает об отказе записи заголовка вставленного сообщения.
			if _, err := client.Write(h); err != nil {
				return err
			}
			// err сообщает об отказе записи тела вставленного сообщения.
			if _, err := client.Write(body); err != nil {
				return err
			}
			// ACK допустим в том же месте, что и WindowAckSize.
			return transportSelfWriteControl(client, 3, []byte{0, 0, 12, 1})
		}
		// err сообщает об отказе вставки перед SetChunkSize.
		if err := send("before_chunk_size"); err != nil {
			return err
		}
		// err сообщает об отказе записи SetChunkSize.
		if err := transportSelfWriteControl(client, 1, []byte{0, 0, 16, 0}); err != nil {
			return err
		}
		// err сообщает об отказе вставки перед createStream.
		if err := send("before_create_stream"); err != nil {
			return err
		}
		// err сообщает об отказе записи createStream.
		if err := transportSelfWriteAMF(client, amf0.Data{"createStream", float64(2), nil}, 0); err != nil {
			return err
		}
		// err сообщает об отказе чтения ответа createStream.
		if _, err := transportSelfReadFrame(client, 128); err != nil {
			return err
		}
		// err сообщает об отказе вставки перед publish.
		if err := send("before_publish"); err != nil {
			return err
		}
		// err сообщает об отказе записи publish с точным ключом.
		if err := transportSelfWriteAMF(client, amf0.Data{"publish", float64(0), nil, "key", "live"}, 0x01020304); err != nil {
			return err
		}
		// f содержит окончательный ответ публикации. err сообщает об отказе чтения.
		f, err := transportSelfReadFrame(client, 128)
		if err != nil {
			return err
		}
		// data содержит значения окончательного ответа AMF для проверки onStatus.
		data, err := amf0.Unmarshal(f.body)
		if err != nil || f.typ != 20 || f.stream != 0x01020304 || len(data) < 1 || data[0] != "onStatus" {
			return ErrFraming
		}
		return nil
	}
	// clientErr содержит результат управляемой клиентской последовательности.
	clientErr := run()
	_ = client.Close()
	return <-peerDone, clientErr
}
