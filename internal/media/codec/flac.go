package codec

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// FLACStreamInfo содержит фактические параметры поддержанного PCM16, 48 кГц, стерео.
type FLACStreamInfo struct {
	// Format описывает подтверждённую частоту, раскладку и глубину через профиль s16.
	Format core.AudioFormat
	// MinBlockSize и MaxBlockSize содержат объявленные границы числа отсчётов на канал.
	MinBlockSize, MaxBlockSize uint32
	// TotalSamples содержит длину файла на канал; 0 означает неизвестную длину.
	TotalSamples uint64
}

// ParseFLACStreamInfo разбирает только полное 34-байтовое тело STREAMINFO из raw.
// Иные параметры PCM и границы блока вне 16…4608 отсчётов не поддержаны.
// Сама обрабатываемая AU дополнительно ограничивается 48…4608 отсчётами при разборе кадра.
func ParseFLACStreamInfo(raw []byte) (FLACStreamInfo, error) {
	if len(raw) != 34 {
		return FLACStreamInfo{}, fmt.Errorf("%w: длина FLAC STREAMINFO", ErrCodec)
	}
	// fields содержит упакованные sample_rate/channels/bits_per_sample/total_samples.
	fields := binary.BigEndian.Uint64(raw[10:18])
	// info сохраняет фактические границы размера блока и длину из STREAMINFO.
	info := FLACStreamInfo{Format: audioBinding(core.CodecFLAC), MinBlockSize: uint32(binary.BigEndian.Uint16(raw[:2])), MaxBlockSize: uint32(binary.BigEndian.Uint16(raw[2:4])), TotalSamples: fields & (1<<36 - 1)}
	// minFrame и maxFrame задают необязательные границы размера сжатого кадра в байтах.
	minFrame, maxFrame := flacUint24(raw[4:7]), flacUint24(raw[7:10])
	if fields>>44 != 48000 || fields>>41&7 != 1 || fields>>36&31 != 15 || info.MinBlockSize < 16 || info.MaxBlockSize < info.MinBlockSize || info.MaxBlockSize > 4608 || minFrame != 0 && maxFrame != 0 && minFrame > maxFrame {
		return FLACStreamInfo{}, fmt.Errorf("%w: неподдержанный FLAC STREAMINFO", ErrCodec)
	}
	return info, nil
}

// flacUint24 читает трёхбайтовое поле raw со старшим байтом первым, уже проверенное по длине.
func flacUint24(raw []byte) uint32 {
	return uint32(raw[0])<<16 | uint32(raw[1])<<8 | uint32(raw[2])
}

// FLACWireHeader оборачивает проверенный STREAMINFO raw единственным последним блоком метаданных.
// Возвращаемые 42 байта включают сигнатуру fLaC; внешний RTMP FourCC в результат не входит.
func FLACWireHeader(raw []byte) ([]byte, error) {
	// err сообщает об отказе фактического STREAMINFO.
	if _, err := ParseFLACStreamInfo(raw); err != nil {
		return nil, err
	}
	return append([]byte{'f', 'L', 'a', 'C', 0x80, 0, 0, 34}, raw...), nil
}

// FLACStreamInfoFromWire принимает raw как точную 42-байтовую запись Enhanced Audio.
// Дополнительные блоки метаданных не поддержаны; возвращается отдельная копия STREAMINFO.
func FLACStreamInfoFromWire(raw []byte) ([]byte, error) {
	if len(raw) != 42 || !bytes.Equal(raw[:8], []byte{'f', 'L', 'a', 'C', 0x80, 0, 0, 34}) {
		return nil, fmt.Errorf("%w: FLAC sequence record", ErrCodec)
	}
	// err сообщает о несовместимых фактических параметрах тела записи.
	if _, err := ParseFLACStreamInfo(raw[8:]); err != nil {
		return nil, err
	}
	return bytes.Clone(raw[8:]), nil
}

// FLACContinuousHeaders создаёт из raw описание нового физического потока с переменными блоками.
// Частота и PCM остаются прежними; границы блока — 48…4608, размеры кадров, длина и MD5 неизвестны.
// Право изменять собственный выход либо отдельный вход перекодирования проверяет вызывающий.
func FLACContinuousHeaders(raw []byte) ([]byte, error) {
	// err сообщает о неподдержанном исходном STREAMINFO.
	if _, err := ParseFLACStreamInfo(raw); err != nil {
		return nil, err
	}
	// out хранит независимый STREAMINFO с сохранённым фактическим форматом PCM.
	out := bytes.Clone(raw)
	binary.BigEndian.PutUint16(out[:2], 48)
	binary.BigEndian.PutUint16(out[2:4], 4608)
	clear(out[4:10])
	out[13] &= 0xf0
	clear(out[14:])
	return out, nil
}

// FLACFrame содержит поля одного полностью проверенного кадра FLAC.
type FLACFrame struct {
	// Samples задаёт фактическое число отсчётов каждого канала этого кадра.
	Samples uint32
	// Number содержит номер кадра для фиксированных блоков либо номер отсчёта для переменных блоков.
	Number uint64
	// Variable отличает счётчик отсчётов от номера кадра фиксированного размера.
	Variable bool
	// HeaderBytes задаёт длину заголовка, включая его CRC8, перед первым подкадром.
	HeaderBytes int
}

// FLACFrameInfo сверяет STREAMINFO headers и ровно один полный кадр payload.
// Проверяются CRC8/CRC16, минимальная запись номера, PCM, границы блока и длины всех подкадров.
// Остаток и предсказание не декодируются, но их полная синтаксическая граница проверяется.
func FLACFrameInfo(headers, payload []byte) (FLACFrame, error) {
	// stream хранит фактический PCM и границы блока; err сообщает о непригодном STREAMINFO.
	stream, err := ParseFLACStreamInfo(headers)
	if err != nil {
		return FLACFrame{}, err
	}
	if len(payload) < 9 || len(payload) > 1<<20 || payload[0] != 0xff || payload[1]&0xfe != 0xf8 || payload[3]&1 != 0 {
		return FLACFrame{}, fmt.Errorf("%w: FLAC frame header", ErrCodec)
	}
	// frame сохраняет способ разбиения на блоки и проверяемые поля кадра.
	frame := FLACFrame{Variable: payload[1]&1 != 0}
	// number содержит номер отсчёта или кадра, width — длину его записи, numberErr — отказ её проверки.
	number, width, numberErr := flacReadNumber(payload[4:], frame.Variable)
	if numberErr != nil {
		return frame, numberErr
	}
	frame.Number = number
	// offset передвигается только по проверенным полям заголовка переменной длины.
	offset := 4 + width
	// blockCode и rateCode определяют табличное либо явное описание кадра.
	blockCode, rateCode := payload[2]>>4, payload[2]&15
	switch blockCode {
	case 0:
		return frame, fmt.Errorf("%w: зарезервированный FLAC block size", ErrCodec)
	case 1:
		frame.Samples = 192
	case 2, 3, 4, 5:
		frame.Samples = 576 << (blockCode - 2)
	case 6, 7:
		// sizeBytes определяет длину явной записи размера блока минус один.
		sizeBytes := int(blockCode - 5)
		if offset+sizeBytes >= len(payload) {
			return frame, ErrCodec
		}
		frame.Samples = uint32(payload[offset])
		if sizeBytes == 2 {
			frame.Samples = uint32(binary.BigEndian.Uint16(payload[offset : offset+2]))
		}
		frame.Samples++
		offset += sizeBytes
	default:
		frame.Samples = 1 << blockCode
	}
	// rate содержит фактическую частоту, причём код 0 ссылается на проверенный STREAMINFO.
	rate := stream.Format.SampleRateHz
	if rateCode < 12 && rateCode != 0 {
		rate = [...]uint32{0, 88200, 176400, 192000, 8000, 16000, 22050, 24000, 32000, 44100, 48000, 96000}[rateCode]
	} else if rateCode >= 12 {
		// rateBytes задаёт число байтов явной частоты после необязательного размера блока.
		rateBytes := 2
		if rateCode == 12 {
			rateBytes = 1
		}
		if rateCode == 15 || offset+rateBytes >= len(payload) {
			return frame, ErrCodec
		}
		rate = uint32(payload[offset]) * 1000
		if rateBytes == 2 {
			rate = uint32(binary.BigEndian.Uint16(payload[offset : offset+2]))
			if rateCode == 14 {
				rate *= 10
			}
		}
		offset += rateBytes
	}
	// assignment задаёт порядок стереоканалов; depthCode со значениями 0/4 означает PCM16.
	assignment, depthCode := payload[3]>>4, payload[3]>>1&7
	if rate != 48000 || assignment != 1 && (assignment < 8 || assignment > 10) || depthCode != 0 && depthCode != 4 {
		return frame, fmt.Errorf("%w: неподдержанный FLAC PCM binding", ErrCodec)
	}
	if frame.Samples < 48 || frame.Samples > 4608 {
		return frame, fmt.Errorf("%w: неподдержанный FLAC block вне48…4608", ErrCodec)
	}
	if frame.Variable && (frame.Number >= 1<<36 || uint64(frame.Samples) > (1<<36-1)-frame.Number || stream.TotalSamples != 0 && frame.Number+uint64(frame.Samples) > stream.TotalSamples) {
		return frame, fmt.Errorf("%w: FLAC sample number выходит за длину потока", ErrCodec)
	}
	// Последний кадр может быть короче MinBlockSize; для переменных блоков это подтверждает TotalSamples.
	if frame.Samples > stream.MaxBlockSize || frame.Variable && frame.Samples < stream.MinBlockSize && (stream.TotalSamples == 0 || frame.Number+uint64(frame.Samples) != stream.TotalSamples) {
		return frame, fmt.Errorf("%w: FLAC block противоречит STREAMINFO", ErrCodec)
	}
	// minFrame и maxFrame задают известные границы размера сжатого кадра из STREAMINFO.
	minFrame, maxFrame := flacUint24(headers[4:7]), flacUint24(headers[7:10])
	if minFrame != 0 && uint32(len(payload)) < minFrame || maxFrame != 0 && uint32(len(payload)) > maxFrame {
		return frame, fmt.Errorf("%w: размер FLAC frame противоречит STREAMINFO", ErrCodec)
	}
	if offset >= len(payload)-2 || flacCRC8(payload[:offset+1]) != 0 || audioCRC16(payload, 0x8005) != 0 {
		return frame, fmt.Errorf("%w: FLAC CRC", ErrCodec)
	}
	frame.HeaderBytes = offset + 1
	// r ограничен телом перед CRC16; channel перебирает два переданных подкадра.
	r := bitReader{b: payload[:len(payload)-2], p: frame.HeaderBytes * 8}
	for channel := range 2 {
		// depth учитывает дополнительный бит разностного канала при стереодекорреляции.
		depth := 16
		if assignment == 8 && channel == 1 || assignment == 9 && channel == 0 || assignment == 10 && channel == 1 {
			depth++
		}
		flacSubframe(&r, int(frame.Samples), depth)
	}
	if r.err != nil || r.read((8-r.p%8)%8) != 0 || r.p != len(r.b)*8 {
		return frame, fmt.Errorf("%w: FLAC subframes либо хвост frame", ErrCodec)
	}
	return frame, nil
}

// flacReadNumber читает один минимально записанный номер в расширенном UTF-8 из raw.
// variable допускает 36-битный номер отсчёта; иначе допускается 31-битный номер кадра.
func flacReadNumber(raw []byte, variable bool) (uint64, int, error) {
	if len(raw) == 0 {
		return 0, 0, ErrCodec
	}
	if raw[0] < 128 {
		return uint64(raw[0]), 1, nil
	}
	// width считает ведущие единицы первого байта.
	width := 0
	// mask последовательно выбирает ведущие биты первого байта номера в UTF-8.
	for mask := byte(128); mask != 0 && raw[0]&mask != 0; mask >>= 1 {
		width++
	}
	if width < 2 || width > 7 || width > len(raw) || !variable && width > 6 {
		return 0, 0, fmt.Errorf("%w: FLAC coded number", ErrCodec)
	}
	// number содержит значение без префиксов UTF-8.
	number := uint64(raw[0] & byte(127>>width))
	// index выбирает очередной байт продолжения.
	for index := 1; index < width; index++ {
		if raw[index]&0xc0 != 0x80 {
			return 0, 0, ErrCodec
		}
		number = number<<6 | uint64(raw[index]&63)
	}
	if number < [...]uint64{0, 0, 128, 2048, 65536, 2097152, 67108864, 2147483648}[width] || number >= 1<<36 || !variable && number >= 1<<31 {
		return 0, 0, fmt.Errorf("%w: неминимальный либо слишком большой FLAC number", ErrCodec)
	}
	return number, width, nil
}

// flacWriteNumber кодирует проверенный 36-битный номер number минимальной записью расширенного UTF-8.
func flacWriteNumber(number uint64) []byte {
	if number < 128 {
		return []byte{byte(number)}
	}
	// width выбирает минимальную длину, out содержит байты от старшего к младшему.
	width := 2
	for width < 7 && number >= uint64(1)<<uint(5*width+1) {
		width++
	}
	// out заполняется байтами продолжения с конца, оставляя префикс в первом байте.
	out := make([]byte, width)
	// index указывает на очередной байт продолжения справа налево.
	for index := width - 1; index > 0; index-- {
		out[index] = 0x80 | byte(number&63)
		number >>= 6
	}
	out[0] = byte(0xff<<uint(8-width)) | byte(number)
	return out
}

// RewriteFLACFrame возвращает копию payload с номером отсчёта переменного блока и новыми CRC.
// headers проверяет исходный кадр; counter принадлежит физическому потоку вызывающего.
// До выделения результата проверяется 36-битная вместимость номера и следующего значения счётчика.
// Вызывающий проверяет FLACOwned либо явное разрешение отдельного входа перекодирования и бюджет байтов.
func RewriteFLACFrame(headers, payload []byte, counter uint64) ([]byte, error) {
	// frame содержит подтверждённые размеры, err сообщает о непригодном исходном кадре.
	frame, err := FLACFrameInfo(headers, payload)
	if err != nil {
		return nil, err
	}
	if counter >= 1<<36 || uint64(frame.Samples) > (1<<36-1)-counter {
		return nil, fmt.Errorf("%w: FLAC sample counter overflow", ErrLimit)
	}
	// oldWidth ограничивает заменяемое поле номера; numberErr сохраняет отказ повторного чтения.
	_, oldWidth, numberErr := flacReadNumber(payload[4:], frame.Variable)
	if numberErr != nil {
		return nil, numberErr
	}
	// number содержит минимально кодированное новое значение счётчика отсчётов.
	number := flacWriteNumber(counter)
	// out получает прежние первые поля и неизменное тело между CRC заголовка и CRC всего кадра.
	out := make([]byte, 0, len(payload)+len(number)-oldWidth)
	out = append(out, payload[:4]...)
	out[1] |= 1
	out = append(out, number...)
	out = append(out, payload[4+oldWidth:frame.HeaderBytes-1]...)
	out = append(out, flacCRC8(out))
	out = append(out, payload[frame.HeaderBytes:len(payload)-2]...)
	// crc содержит новый остаток для изменённого заголовка и прежних подкадров.
	crc := audioCRC16(out, 0x8005)
	out = append(out, byte(crc>>8), byte(crc))
	return out, nil
}

// flacSubframe проверяет полную синтаксическую длину одного подкадра в r.
// samples задаёт размер блока, depth — число битов конкретного декоррелированного канала.
func flacSubframe(r *bitReader, samples, depth int) {
	if r.read(1) != 0 {
		r.err = ErrCodec
		return
	}
	// kind различает постоянное значение, несжатые данные и предсказание fixed/LPC;
	// wasted включает унарное описание числа отброшенных младших битов.
	kind, wasted := r.read(6), r.read(1)
	if wasted != 0 {
		depth--
		for r.err == nil && r.read(1) == 0 {
			depth--
			if depth <= 0 {
				r.err = ErrCodec
			}
		}
	}
	if depth <= 0 {
		r.err = ErrCodec
		return
	}
	if kind == 0 {
		audioSkipBits(r, depth)
		return
	}
	if kind == 1 {
		audioSkipBits(r, samples*depth)
		return
	}
	// order задаёт число начальных отсчётов предсказателя перед кодированным остатком.
	order := int(kind & 7)
	if kind >= 32 {
		order = int(kind&31) + 1
	} else if kind < 8 || kind > 12 {
		r.err = ErrCodec
		return
	}
	if order > samples {
		r.err = ErrCodec
		return
	}
	audioSkipBits(r, order*depth)
	if kind >= 32 {
		// precision задаёт число битов каждого коэффициента LPC; значение 15 зарезервировано.
		precision := int(r.read(4)) + 1
		if precision == 16 {
			r.err = ErrCodec
			return
		}
		// shift содержит знаковый сдвиг LPC; отрицательные значения запрещены RFC 9639.
		shift := int(r.read(5))
		if shift&16 != 0 {
			shift -= 32
		}
		if shift < 0 {
			r.err = ErrCodec
			return
		}
		audioSkipBits(r, order*precision)
	}
	flacResidual(r, samples, order)
}

// flacResidual проверяет разделы Rice в r для samples отсчётов и order начальных значений.
// Унарное частное читается в пределах уже ограниченной AU без выделений по его значению.
func flacResidual(r *bitReader, samples, order int) {
	// method выбирает 4- либо 5-битовые параметры Rice; partitionBits задаёт число разделов.
	method, partitionBits := r.read(2), r.read(4)
	if method > 1 || samples%(1<<partitionBits) != 0 || samples>>partitionBits <= order {
		r.err = ErrCodec
		return
	}
	// parameterBits определяет длину параметра и зарезервированный код несжатых значений.
	parameterBits := int(method) + 4
	// partition перебирает равные разделы, первый исключает начальные отсчёты предсказателя.
	for partition := range 1 << partitionBits {
		// count задаёт число значений остатка в текущем разделе.
		count := samples >> partitionBits
		if partition == 0 {
			count -= order
		}
		// rice содержит параметр Golomb-Rice либо код несжатых значений.
		rice := int(r.read(parameterBits))
		if rice == (1<<parameterBits)-1 {
			// width задаёт явную длину каждого знакового значения несжатого остатка.
			width := int(r.read(5))
			audioSkipBits(r, count*width)
			continue
		}
		for range count {
			for r.err == nil && r.read(1) == 0 {
				// Частное нужно только для границы, поэтому его значение не накапливается.
			}
			audioSkipBits(r, rice)
		}
		if r.err != nil {
			return
		}
	}
}

// audioSkipBits пропускает n битов в r с проверкой полного остатка, не меняя r при ошибке.
func audioSkipBits(r *bitReader, n int) {
	if r.err != nil {
		return
	}
	if n < 0 || n > len(r.b)*8-r.p {
		r.err = ErrCodec
		return
	}
	r.p += n
}

// flacCRC8 вычисляет нормативный остаток заголовка raw с полиномом x8+x2+x+1.
func flacCRC8(raw []byte) byte {
	// crc накапливает остаток после каждого байта raw.
	crc := byte(0)
	// value содержит следующий байт проверяемого заголовка.
	for _, value := range raw {
		crc ^= value
		for range 8 {
			if crc&0x80 != 0 {
				crc = crc<<1 ^ 7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// audioCRC16 вычисляет остаток raw со старшим битом первым по младшим 16 битам polynomial.
// FLAC и AC3 используют 0x8005, начальный остаток 0 и отсутствие финального XOR.
func audioCRC16(raw []byte, polynomial uint16) uint16 {
	// crc хранит текущий16bit остаток без отражения битов.
	crc := uint16(0)
	// value содержит следующий переданный байт.
	for _, value := range raw {
		crc ^= uint16(value) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ polynomial
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
