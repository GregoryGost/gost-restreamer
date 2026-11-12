package rtmp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/bytecounter"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// Проверяет бюджет AV1 и обоих видов AAC-конфигурации по короткому префиксу до выделения полного тела.
func TestContractWireConfigBudgetsPrecedeBodyAllocation(t *testing.T) {
	// tc содержит форму конфигурации и максимальное ожидаемое число прочитанных байтов.
	for _, tc := range []struct {
		// name обозначает форму конфигурации в имени подтеста.
		name string
		// typ задаёт тип RTMP сообщения видео или аудио.
		typ byte
		// prefix содержит первые байты AV1 либо AAC-конфигурации.
		prefix []byte
		// consumed задаёт предельное число прочитанных байтов до раннего отказа.
		consumed int
	}{
		{"av1", 9, []byte{0x90, 'a', 'v', '0', '1'}, 13},
		{"legacy-aac", 8, []byte{0xaf, 0, 0x11, 0x90, 0}, 17},
		{"extended-aac", 8, []byte{0x90, 'm', 'p', '4', 'a'}, 17},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет предел выбранной конфигурации AV1 либо AAC до чтения или выделения тела и до передачи
			// байтов декодеру.

			// limits содержит независимые малые бюджеты текущего сценария.
			limits := acceptanceWireLimits()
			// declared задаёт объявленную длину конфигурации сверх её лимита.
			declared := limits.MaxConfigBytes + 1
			// raw содержит сообщение с чрезмерной объявленной длиной и коротким телом.
			raw := chunk0(4, tc.typ, tc.prefix)
			raw[4], raw[5], raw[6] = byte(declared>>16), byte(declared>>8), byte(declared)
			// w содержит ограничитель. conn считает фактически прочитанные байты до отказа.
			w, conn := acceptanceWire(t, raw, limits)
			// got содержит выданные библиотеке байты. err содержит результат ранней проверки
			// конфигурации.
			got, err := io.ReadAll(w)
			if !errors.Is(err, ErrLimit) || len(got) != 0 || conn.readBytes > tc.consumed || len(w.streams[4].capture) != 0 {
				t.Fatalf("бюджет конфигурации проверен после чтения или выделения тела: read=%d exposed=%d capture=%d err=%v", conn.readBytes, len(got), len(w.streams[4].capture), err)
			}
		})
	}
}

// Проверяет ранний отказ по бюджету опережающего чтения или накопления конфигурации без чтения
// остального тела.
func TestContractWireReadAheadAndCaptureBudgetsPrecedePayload(t *testing.T) {
	// capture выбирает отказ по полному накоплению тела либо по размеру опережающего чтения.
	for _, capture := range []bool{false, true} {
		t.Run(map[bool]string{false: "read-ahead", true: "capture"}[capture], func(t *testing.T) {
			// Проверяет выбранный предел опережающего чтения либо сохранения конфигурации до чтения её тела.

			// limits содержит бюджеты с выбранным недостаточным пределом.
			limits := acceptanceWireLimits()
			// body содержит сообщение конфигурации AV1.
			body := append([]byte{0x90, 'a', 'v', '0', '1'}, testAV1Config()...)
			// wantRead задаёт максимальное число байтов, которые разрешено прочитать до
			// отказа.
			wantRead := 12
			if capture {
				limits.MaxRetainedBytes = limits.baseRetained() + 512 + uint64(len(body)) - 1
				wantRead++
			} else {
				limits.MaxReadAheadBytes = 18
			}
			// w содержит ограничитель. conn считает фактическое чтение тела.
			w, conn := acceptanceWire(t, chunk0(3, 9, body), limits)
			// got содержит выданные байты. err содержит результат проверки выбранного
			// бюджета.
			got, err := io.ReadAll(w)
			if !errors.Is(err, ErrLimit) || len(got) != 0 || conn.readBytes > wantRead || len(w.streams[3].capture) != 0 {
				t.Fatalf("тело выделено или прочитано сверх бюджета: read=%d exposed=%d capture=%d err=%v", conn.readBytes, len(got), len(w.streams[3].capture), err)
			}
		})
	}
}

// Проверяет сохранение исходных AV1/AAC-заголовков и AAC-нагрузки при разбиении каждого сообщения на
// однобайтовые фрагменты.
func TestContractWireExtendedAACConfigSurvivesOneByteChunks(t *testing.T) {
	// p содержит тестовую публикацию. sub содержит подписку для проверки доставленных конфигураций и
	// нагрузки.
	p, sub := newContractPublication(t)
	// config содержит исходный av1C с собственным timing_info.
	config := contractAV1Config(contractSDRSequence())
	// raw накапливает конфигурации и A/V-пакеты с однобайтовыми фрагментами.
	raw := chunk0(2, 1, []byte{0, 0, 0, 1})
	raw = append(raw, fragmented(6, 9, append([]byte{0x90, 'a', 'v', '0', '1'}, config...), 1)...)
	raw = append(raw, fragmented(4, 8, []byte{0x90, 'm', 'p', '4', 'a', 0x11, 0x90}, 1)...)
	raw = append(raw, fragmented(6, 9, append([]byte{0x91, 'a', 'v', '0', '1'}, contractAV1TU(config)...), 1)...)
	raw = append(raw, fragmented(4, 8, []byte{0x91, 'm', 'p', '4', 'a', 0xab, 0xcd}, 1)...)
	// err сообщает об отказе обработки побайтно раздробленной последовательности.
	if err := contractConsume(t, p, raw); err != nil {
		t.Fatal(err)
	}
	// v содержит видеодоставку с исходными AV1-заголовками. a содержит аудиодоставку с исходным ASC
	// и нагрузкой.
	v, a := delivery(t, sub), delivery(t, sub)
	if !bytes.Equal(v.VideoConfig.Headers(), config) || !bytes.Equal(a.AudioConfig.Headers(), []byte{0x11, 0x90}) || !bytes.Equal(a.Packet.Payload(), []byte{0xab, 0xcd}) || !v.Packet.Info().Keyframe {
		t.Fatal("разбиение префикса уничтожило конфигурацию AAC/AV1 либо полезные данные")
	}
}

// Проверяет отказ обычного видео, многодорожечных форм и чужих кодеков до чтения оставшейся нагрузки.
func TestContractWireRejectsUnsupportedCodecsBeforeRemainingPayload(t *testing.T) {
	// tc содержит неподдержанную форму кодека и ожидаемую границу чтения.
	for _, tc := range []struct {
		// name обозначает неподдержанную форму в имени подтеста.
		name string
		// typ задаёт тип аудио- или видеосообщения RTMP.
		typ byte
		// prefix содержит неподдержанный короткий префикс кодека.
		prefix []byte
		// consumed ограничивает число байтов, прочитанных до обнаружения неподдержанной формы.
		consumed int
	}{
		{"legacy-video", 9, []byte{0x17}, 13},
		{"video-multitrack", 9, []byte{0x96}, 13},
		{"non-av1", 9, []byte{0x91, 'h', 'v', 'c', '1'}, 17},
		{"non-aac", 8, []byte{0x2f}, 13},
		{"audio-multitrack", 8, []byte{0x95}, 13},
		{"non-mp4a", 8, []byte{0x91, 'O', 'p', 'u', 's'}, 17},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет ранний отказ выбранного неподдержанного кодека до чтения оставшихся байтов полезной
			// нагрузки.

			// raw содержит неподдержанный префикс и дополнительную нагрузку, которую нельзя
			// читать.
			raw := chunk0(3, tc.typ, append(bytes.Clone(tc.prefix), bytes.Repeat([]byte{0xee}, 64)...))
			// w содержит ограничитель. conn считает фактическое чтение до отказа.
			w, conn := acceptanceWire(t, raw, acceptanceWireLimits())
			// got содержит выданные байты. err содержит результат раннего отказа кодека.
			got, err := io.ReadAll(w)
			if !errors.Is(err, ErrCodec) || len(got) != 0 || conn.readBytes > tc.consumed {
				t.Fatalf("неподдерживаемый кодек пропущен до чтения полезных данных: read=%d exposed=%d err=%v", conn.readBytes, len(got), err)
			}
		})
	}
}

// Проверяет отказ размера блока сверх MaxChunkBytes и приём увеличения до точного предела без
// изменения AAC-данных.
func TestContractWireChunkSizeUpperBoundAndLegalIncrease(t *testing.T) {
	// limits содержит независимые пределы размера блоков.
	limits := acceptanceWireLimits()
	// size содержит сетевое четырёхбайтовое значение объявляемого ChunkSize.
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], limits.MaxChunkBytes+1)
	// w содержит ограничитель. conn считает байты чтения чрезмерного SetChunkSize.
	w, conn := acceptanceWire(t, chunk0(2, 1, size[:]), limits)
	// got содержит выданные байты. err содержит результат проверки чрезмерного размера блока.
	got, err := io.ReadAll(w)
	if !errors.Is(err, ErrLimit) || len(got) != 0 || conn.readBytes > 16 {
		t.Fatalf("превышающий бюджет ChunkSize передан библиотеке: read=%d exposed=%d err=%v", conn.readBytes, len(got), err)
	}
	binary.BigEndian.PutUint32(size[:], limits.MaxChunkBytes)
	// payload содержит двухсотбайтовую AAC-нагрузку для допустимого увеличения блока.
	payload := bytes.Repeat([]byte{0xab}, 200)
	// raw содержит допустимый SetChunkSize и следующее аудиосообщение.
	raw := append(chunk0(2, 1, size[:]), chunk0(4, 8, append([]byte{0x91, 'm', 'p', '4', 'a'}, payload...))...)
	w, _ = acceptanceWire(t, raw, limits)
	// counter считает прочитанные байты для библиотечного декодера.
	counter := bytecounter.NewReader(w)
	// reader читает типизированные сообщения после ограничителя.
	reader := message.NewReader(counter, counter, func(uint32) error {
		// Принимает уведомление о пороге подтверждения без дополнительных действий при проверке допустимого
		// размера блока.
		return nil
	})
	// err сообщает об отказе чтения допустимого SetChunkSize.
	if _, err := reader.Read(); err != nil {
		t.Fatal(err)
	}
	// m содержит декодированное аудиосообщение после увеличения блока.
	m, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	// audio содержит распознанную Enhanced AAC-форму. ok подтверждает ожидаемый тип сообщения.
	audio, ok := m.(*message.AudioExCodedFrames)
	if !ok || !bytes.Equal(audio.Payload, payload) {
		t.Fatalf("легальное увеличение ChunkSize исказило AAC: %T", m)
	}
}

// Проверяет точное прохождение поддержанного графа AMF и ранний отказ неизвестных маркеров, усечённых
// значений и чрезмерной длины массива.
func TestContractWireAMFSupportedGraphsAndMalformedBoundaries(t *testing.T) {
	// data содержит допустимый вложенный AMF-граф с метаданными и значениями разных типов.
	data := amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: float64(30)}, {Key: "extra", Value: amf0.ECMAArray{{Key: "items", Value: amf0.StrictArray{nil, amf0.Undefined{}, true, "value"}}}}}}
	// body содержит сериализованный допустимый граф. err сообщает об отказе кодирования.
	body, err := data.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// limits задаёт глубину, достаточную для контрольного графа AMF.
	limits := acceptanceWireLimits()
	limits.MaxAMFDepth = 5
	// w содержит ограничитель допустимого дроблёного AMF-сообщения.
	w, _ := acceptanceWire(t, fragmented(3, 18, body, 128), limits)
	// counter считает байты для типизированного читателя.
	counter := bytecounter.NewReader(w)
	// reader декодирует прошедшие предварительную проверку AMF-данные.
	reader := message.NewReader(counter, counter, func(uint32) error {
		// Принимает уведомление о пороге подтверждения без дополнительных действий при проверке структуры AMF.
		return nil
	})
	// msg содержит декодированное сообщение метаданных.
	msg, err := reader.Read()
	if err != nil {
		t.Fatalf("допустимый ограниченный граф отвергнут: %v", err)
	}
	// metadata содержит DataAMF0. ok подтверждает ожидаемый тип сообщения.
	metadata, ok := msg.(*message.DataAMF0)
	if !ok || !reflect.DeepEqual(metadata.Payload, data) {
		t.Fatalf("потерян граф метаданных: %T", msg)
	}
	// object содержит вложенный AMF Object с полем framerate.
	object, ok := metadata.Payload[1].(amf0.Object)
	if !ok {
		t.Fatalf("потерян Object: %T", metadata.Payload[1])
	}
	// rate содержит числовой framerate. ok показывает наличие поля нужного типа.
	if rate, ok := object.GetFloat64("framerate"); !ok || rate != 30 {
		t.Fatal("потерян проверенный framerate")
	}
	// tc содержит повреждённое AMF-тело и ожидаемую категорию отказа.
	for _, tc := range []struct {
		// name обозначает нарушение AMF в имени подтеста.
		name string
		// body содержит повреждённые сериализованные данные AMF.
		body []byte
		// want задаёт ожидаемую категорию структуры или бюджета.
		want error
	}{
		{"unknown-marker", []byte{0xff}, ErrFraming},
		{"truncated-number", []byte{0, 0}, ErrFraming},
		{"truncated-boolean", []byte{1}, ErrFraming},
		{"truncated-string-length", []byte{2, 0}, ErrFraming},
		{"truncated-string", []byte{2, 0, 2, 'a'}, ErrFraming},
		{"truncated-ecma-count", []byte{8, 0}, ErrFraming},
		{"truncated-property-length", []byte{3, 0}, ErrFraming},
		{"truncated-property-name", []byte{3, 0, 2, 'a'}, ErrFraming},
		{"missing-object-end", []byte{3, 0, 0}, ErrFraming},
		{"wrong-object-end", []byte{3, 0, 0, 8}, ErrFraming},
		{"truncated-array-count", []byte{10, 0}, ErrFraming},
		{"array-count-budget", []byte{10, 0, 0, 0, 32}, ErrLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет, что выбранное повреждение либо превышение бюджета AMF отклоняется до передачи графа
			// декодеру сообщений.

			// w содержит ограничитель выбранного повреждённого тела.
			w, _ := acceptanceWire(t, chunk0(3, 18, tc.body), limits)
			// got содержит выданные байты. err содержит результат предварительной проверки
			// повреждения.
			got, err := io.ReadAll(w)
			if !errors.Is(err, tc.want) || len(got) != 0 {
				t.Fatalf("опасный граф AMF передан декодеру сообщений: exposed=%d err=%v", len(got), err)
			}
		})
	}
}

// Проверяет включительную границу MaxAMFElements: ровно два значения принимаются, третье отклоняется
// до выдачи сообщения.
func TestContractWireAMFElementLimitHasInclusiveBoundary(t *testing.T) {
	// limits ограничивает общее число AMF-значений двумя.
	limits := acceptanceWireLimits()
	limits.MaxAMFElements = 2
	// tc содержит тело ровно на границе либо на одно значение выше неё.
	for _, tc := range []struct {
		// body содержит последовательность маркеров null выбранной длины.
		body []byte
		// want задаёт ожидаемый успех либо ErrLimit.
		want error
	}{
		{[]byte{5, 5}, nil}, {[]byte{5, 5, 5}, ErrLimit},
	} {
		// w содержит ограничитель выбранной последовательности AMF.
		w, _ := acceptanceWire(t, chunk0(3, 18, tc.body), limits)
		// got содержит выданные байты. err содержит результат проверки включительной границы
		// числа значений.
		got, err := io.ReadAll(w)
		if !errors.Is(err, tc.want) || (tc.want != nil && len(got) != 0) || (tc.want == nil && !bytes.Equal(got, chunk0(3, 18, tc.body))) {
			t.Fatalf("граница количества AMF-значений: body=%x exposed=%x err=%v", tc.body, got, err)
		}
	}
}

// Проверяет отказ нового заголовка незаконченного сообщения и несовпадающей расширенной метки
// продолжения до чтения его нагрузки.
func TestContractWireRejectsConflictingContinuationBeforePayload(t *testing.T) {
	// extended выбирает несовпадающую расширенную метку либо недопустимый новый заголовок посреди
	// сообщения.
	for _, extended := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-header", true: "extended-timestamp"}[extended], func(t *testing.T) {
			// Проверяет отказ противоречивого продолжения сообщения до полезной нагрузки для нового заголовка либо
			// расширенной метки.

			// first содержит допустимый первый фрагмент незавершённого аудиосообщения.
			first := chunk0(4, 8, []byte{0xaf, 1})[:13]
			// next содержит противоречивый следующий заголовок с нагрузкой, которую нельзя
			// прочитать.
			next := chunk0(4, 8, []byte{0xaf, 1})
			// wantRead задаёт максимальное число прочитанных байтов до отказа продолжения.
			wantRead := 14
			if extended {
				first = contractMessage(4, 8, 0xffffffff, []byte{0xaf, 1})[:17]
				next = []byte{0xc4, 0, 0, 0, 0, 1}
				wantRead = 22
			}
			// w содержит ограничитель. conn считает фактически прочитанные байты
			// конфликтующего продолжения.
			w, conn := acceptanceWire(t, append(first, next...), acceptanceWireLimits())
			w.chunkSize = 1
			// got содержит только разрешённые байты первого фрагмента. err содержит
			// результат проверки конфликта.
			got, err := io.ReadAll(w)
			if !errors.Is(err, ErrFraming) || len(got) != len(first) || conn.readBytes > wantRead {
				t.Fatalf("несогласованное продолжение передано декодеру сообщений: read=%d exposed=%d err=%v", conn.readBytes, len(got), err)
			}
		})
	}
}
