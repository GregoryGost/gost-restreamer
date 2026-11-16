package codec

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// flacBindingHeaders создаёт нормативный STREAMINFO без длины и MD5 для модульных тестов.
// samples задаёт объявленный максимальный блок; минимум 48 сохраняет обрабатываемую сетку времени.
func flacBindingHeaders(samples uint16) []byte {
	// raw содержит 34 байта тела STREAMINFO с частотой 48 кГц, стерео, PCM16 и неизвестной длиной.
	raw := make([]byte, 34)
	binary.BigEndian.PutUint16(raw[:2], 48)
	binary.BigEndian.PutUint16(raw[2:4], samples)
	binary.BigEndian.PutUint64(raw[10:18], uint64(48000)<<44|uint64(1)<<41|uint64(15)<<36)
	return raw
}

// flacBindingConstant создаёт один постоянный стереокадр для samples отсчётов и карты assignment.
// number задаёт номер фиксированного кадра; тело содержит точный 0 независимо от изменения заголовка.
func flacBindingConstant(samples uint32, number uint64, assignment byte) []byte {
	// raw содержит заголовок фиксированного кадра с явным 16-битным размером блока и частотой 48 кГц.
	raw := []byte{0xff, 0xf8, 0x7a, assignment<<4 | 8}
	raw = append(raw, flacWriteNumber(number)...)
	raw = append(raw, byte((samples-1)>>8), byte(samples-1))
	raw = append(raw, flacCRC8(raw))
	// body собирает оба постоянных подкадра с фактической разрядностью разностного канала.
	body := ppsBits{}
	// channel выбирает разрядность 16 либо 17 для соответствующего разностного канала.
	for channel := range 2 {
		// depth задаёт число битов постоянного отсчёта конкретного декоррелированного канала.
		depth := 16
		if assignment == 8 && channel == 1 || assignment == 9 && channel == 0 || assignment == 10 && channel == 1 {
			depth++
		}
		body.put(0, 8)
		body.put(0, depth)
	}
	for body.count%8 != 0 {
		body.put(0, 1)
	}
	raw = append(raw, body.raw...)
	// crc завершает кадр полным остатком со старшим битом первым.
	crc := audioCRC16(raw, 0x8005)
	return append(raw, byte(crc>>8), byte(crc))
}

// TestFLACActualCRCNumberBoundsAndRebase проверяет границы кадра и перенумерацию без общего состояния.
// t проверяет точную сетевую оболочку, стереокарты, минимальность номера, неизменность тела и 36-битный предел.
func TestFLACActualCRCNumberBoundsAndRebase(t *testing.T) {
	// headers задаёт продолжаемый STREAMINFO: PCM16, 48 кГц, стерео.
	headers := flacBindingHeaders(4608)
	// wire и err содержат нормативную 42-байтовую запись Enhanced Audio либо отказ заголовка.
	wire, err := FLACWireHeader(headers)
	if err != nil || len(wire) != 42 || !bytes.Equal(wire[:8], []byte{'f', 'L', 'a', 'C', 0x80, 0, 0, 34}) {
		t.Fatalf("wire=%x err=%v", wire, err)
	}
	// back подтверждает нормативный 34-байтовый заголовок после разбора сетевой записи.
	back, err := FLACStreamInfoFromWire(wire)
	if err != nil || !bytes.Equal(back, headers) {
		t.Fatal(err)
	}
	back[0] = 1
	if headers[0] != 0 || wire[8] != 0 {
		t.Fatal("FLAC wire helper вернул alias")
	}
	// assignment перебирает все четыре нормативные стереокарты каналов.
	for _, assignment := range []byte{1, 8, 9, 10} {
		// raw содержит исходный фиксированный кадр с номером 7; original сохраняет вход для проверки неизменяемости.
		raw := flacBindingConstant(4608, 7, assignment)
		original := bytes.Clone(raw)
		// sourceFrame содержит исходную границу подкадров; parseErr сообщает об ошибке тестового образца.
		sourceFrame, parseErr := FLACFrameInfo(headers, raw)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		// counter проверяет смену длины кодированного номера, включая 7-байтовые 36-битные номера отсчётов.
		for _, counter := range []uint64{0, 127, 128, 2047, 2048, 65535, 65536, 2097151, 2097152, 67108863, 67108864, 2147483647, 2147483648, (1 << 36) - 1 - 4608} {
			// rewritten содержит копию с номером отсчёта переменного блока; rewriteErr сообщает о переполнении.
			rewritten, rewriteErr := RewriteFLACFrame(headers, raw, counter)
			if rewriteErr != nil {
				t.Fatalf("counter=%d: %v", counter, rewriteErr)
			}
			// frame и frameErr проверяют результат полным независимым проходом заголовка, тела и CRC.
			frame, frameErr := FLACFrameInfo(headers, rewritten)
			if frameErr != nil || !frame.Variable || frame.Number != counter || frame.Samples != 4608 {
				t.Fatalf("counter=%d frame=%+v err=%v", counter, frame, frameErr)
			}
			if !bytes.Equal(raw[sourceFrame.HeaderBytes:len(raw)-2], rewritten[frame.HeaderBytes:len(rewritten)-2]) || !bytes.Equal(raw, original) {
				t.Fatal("header rewrite изменил subframes либо source AU")
			}
		}
		if _, err = RewriteFLACFrame(headers, raw, (1<<36)-4608); err == nil {
			t.Fatal("переполнение следующего physical counter принято")
		}
	}
	// samples проверяет нижнюю границу поддержанного времени при корректном синтаксисе и CRC FLAC.
	for _, samples := range []uint32{1, 16, 47, 48, 4608, 4609} {
		// frame и frameErr содержат фактический блок либо ожидаемый отказ размера.
		frame, frameErr := FLACFrameInfo(headers, flacBindingConstant(samples, 0, 1))
		if (frameErr == nil) != (samples >= 48 && samples <= 4608) || frameErr == nil && frame.Samples != samples {
			t.Fatalf("samples=%d frame=%+v err=%v", samples, frame, frameErr)
		}
	}
	// raw содержит исходный полный кадр для проверки CRC и отказа при лишних данных.
	raw := flacBindingConstant(4608, 0, 1)
	// bad содержит кадр с корректной CRC заголовка, но ошибочной CRC всего кадра.
	bad := bytes.Clone(raw)
	bad[len(bad)-1] ^= 1
	if _, err = FLACFrameInfo(headers, bad); err == nil {
		t.Fatal("неверный frame CRC принят")
	}
	// extra имеет корректную CRC после лишнего байта подкадра; синтаксис всё равно неверен.
	extra := append(bytes.Clone(raw[:len(raw)-2]), 0)
	// crc пересчитывает CRC кадра, чтобы проверить именно границу подкадров.
	crc := audioCRC16(extra, 0x8005)
	extra = append(extra, byte(crc>>8), byte(crc))
	if _, err = FLACFrameInfo(headers, extra); err == nil {
		t.Fatal("лишний byte после двух subframes принят")
	}
	// overlong заменяет минимальный номер 0 трёхбайтовым представлением 0 в расширенном UTF-8.
	overlong := append([]byte{0xff, 0xf8, 0x7a, 0x18, 0xe0, 0x80, 0x80}, raw[5:7]...)
	overlong = append(overlong, flacCRC8(overlong))
	overlong = append(overlong, raw[8:len(raw)-2]...)
	crc = audioCRC16(overlong, 0x8005)
	overlong = append(overlong, byte(crc>>8), byte(crc))
	if _, err = FLACFrameInfo(headers, overlong); err == nil {
		t.Fatal("неминимальный FLAC number принят")
	}
}
