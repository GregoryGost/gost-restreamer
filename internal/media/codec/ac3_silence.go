package codec

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// ac3ZeroChannel сохраняет экспоненты и распределение битов одного канала между шестью блоками.
type ac3ZeroChannel struct {
	// end задаёт исключающую границу используемых спектральных позиций; остальные декодер обнуляет.
	end int
	// exponents содержит фактические абсолютные экспоненты, включая повторения D25/D45.
	exponents [256]int
	// snr задаёт фактическое смещение SNR, fastGain — уровень быстрого распространения маски.
	snr, fastGain int
}

// ac3ZeroAllocation хранит общие параметры BAI обычного AC3, 48 кГц, без coupling и DBA.
type ac3ZeroAllocation struct {
	// slowDecay и fastDecay уменьшают соответствующее распространение маски между критическими полосами.
	slowDecay, fastDecay int
	// slowGain задаёт медленное распространение, dbPerBit — поправку возбуждения, floor — нижний порог BAP.
	slowGain, dbPerBit, floor int
}

// NormalizeAC3Silence доказывает нулевые переданные коэффициенты всех шести блоков payload.
// headers должен точно соответствовать BSI/dac3; обе исходные CRC обязательны.
// Поддерживается свежая собственная форма встроенного кодировщика FFmpeg без coupling,
// rematrix, DBA и пропускаемых данных, с полными экспонентами, BAI и доказанным повторным использованием.
// Только после полной проверки копируются байты, снимаются прочитанные dithflag и обновляются
// обе CRC. Входные буферы не меняются. Свежее собственное происхождение проверяет вызывающий;
// функция не применяется к произвольному LIVE/copy и не доказывает PCM прежнего состояния декодера.
func NormalizeAC3Silence(headers, payload []byte) ([]byte, error) {
	// err сообщает о несовпадении dac3, неподдержанных параметрах либо неверных CRC.
	if _, err := ac3Samples(headers, payload); err != nil {
		return nil, err
	}
	// frame содержит начало аудиоблоков после фактического BSI; err сохраняет отказ повторного разбора.
	frame, err := ac3ParseFrame(payload)
	if err != nil {
		return nil, err
	}
	// r ограничивает чтение коэффициентов областью перед auxdatae, crcrsv и CRC2.
	r := bitReader{b: payload[:len(payload)-2], p: frame.audioBit}
	// channels сохраняет фактические экспоненты и SNR каждого канала между блоками.
	var channels [2]ac3ZeroChannel
	// allocation сохраняет фактический BAI между блоками; ditherBits хранит только прочитанные флаги.
	var allocation ac3ZeroAllocation
	var ditherBits [12]int
	// block выбирает один из шести обязательных аудиоблоков текущего кадра.
	for block := range 6 {
		if r.read(2) != 0 { // Собственная свежая тишина использует длинные преобразования обоих каналов.
			return nil, fmt.Errorf("%w: неподдержанный AC3 block switch при normalization", ErrCodec)
		}
		// channel отмечает фактическое положение dithflag каждого канала до любого изменения payload.
		for channel := range 2 {
			ditherBits[2*block+channel] = r.p
			r.read(1)
		}
		if r.read(1) != 0 {
			r.read(8) // dynrng масштабирует 0 в 0 и не меняет спектральный синтаксис.
		}
		// newCoupling явно определяет либо сохраняет отсутствие coupling.
		newCoupling := r.read(1) != 0
		if newCoupling {
			if r.read(1) != 0 {
				return nil, fmt.Errorf("%w: coupling не поддержан при AC3 silence normalization", ErrCodec)
			}
		} else if block == 0 {
			return nil, fmt.Errorf("%w: отсутствует initial AC3 coupling strategy", ErrCodec)
		}
		// newRematrix читает четыре фактических флага либо сохраняет ранее доказанные нули.
		newRematrix := r.read(1) != 0
		if newRematrix {
			if r.read(4) != 0 {
				return nil, fmt.Errorf("%w: rematrix не поддержан при AC3 silence normalization", ErrCodec)
			}
		} else if block == 0 {
			return nil, fmt.Errorf("%w: отсутствует initial AC3 rematrix strategy", ErrCodec)
		}
		// strategies содержит D15/D25/D45 либо EXP_REUSE отдельно для двух каналов.
		strategies := [2]int{int(r.read(2)), int(r.read(2))}
		// channel и strategy выбирают фактическую полосу нового набора экспонент.
		for channel, strategy := range strategies {
			if strategy != 0 {
				// bandwidth задаёт число полных групп по 3 спектральные позиции после первых 73.
				bandwidth := int(r.read(6))
				if bandwidth > 60 {
					return nil, fmt.Errorf("%w: AC3 bandwidth code", ErrCodec)
				}
				channels[channel].end = 73 + 3*bandwidth
			} else if block == 0 {
				return nil, fmt.Errorf("%w: отсутствуют initial AC3 exponents", ErrCodec)
			}
		}
		// channel и strategy определяют чтение новых экспонент либо их повторное использование.
		for channel, strategy := range strategies {
			if strategy != 0 {
				ac3ZeroExponents(&r, &channels[channel], strategy)
				r.read(2) // gainrng не меняет нулевое значение коэффициентов.
			}
		}
		if r.read(1) != 0 {
			allocation = ac3ZeroAllocation{
				slowDecay: [...]int{15, 17, 19, 21}[r.read(2)],
				fastDecay: [...]int{63, 83, 103, 123}[r.read(2)],
				slowGain:  [...]int{1344, 1240, 1144, 1040}[r.read(2)],
				dbPerBit:  [...]int{0, 1792, 2304, 2816}[r.read(2)],
				floor:     [...]int{752, 688, 624, 560, 496, 368, 240, -2048}[r.read(3)],
			}
		} else if block == 0 {
			return nil, fmt.Errorf("%w: отсутствует initial AC3 BAI", ErrCodec)
		}
		if r.read(1) != 0 {
			// coarse задаёт общую часть смещения SNR до точной поправки каждого канала.
			coarse := (int(r.read(6)) - 15) * 16
			// channel выбирает отдельные точную поправку SNR и быстрое усиление каждого канала.
			for channel := range channels {
				channels[channel].snr = (coarse + int(r.read(4))) * 4
				channels[channel].fastGain = (int(r.read(3)) + 1) * 128
			}
		} else if block == 0 {
			return nil, fmt.Errorf("%w: отсутствуют initial AC3 SNR offsets", ErrCodec)
		}
		// У свежей собственной тишины DBA_NONE устанавливается в блоке 0 и затем сохраняется.
		// Любое явное DBA или пропускаемые данные не поддержаны этим нормализатором.
		if r.read(1) != 0 {
			return nil, fmt.Errorf("%w: AC3 delta allocation при normalization", ErrCodec)
		}
		if r.read(1) != 0 {
			return nil, fmt.Errorf("%w: AC3 skip data при normalization", ErrCodec)
		}
		if r.err != nil {
			return nil, fmt.Errorf("%w: усечённый AC3 audio block", ErrCodec)
		}
		// groups сохраняет сгруппированные мантиссы между каналами внутри одного блока по A/52.
		var groups ac3ZeroGroups
		// channel выбирает экспоненты и BAI, из которых вычисляется BAP каждой используемой позиции.
		for channel := range channels {
			// baps содержит вычисленную точность квантования каждой позиции текущего блока.
			baps := ac3ZeroBAP(channels[channel], allocation)
			// bin и bap задают фактическую спектральную позицию и её квантователь.
			for bin, bap := range baps[:channels[channel].end] {
				if !groups.zero(&r, bap) || r.err != nil {
					return nil, fmt.Errorf("%w: ненулевой либо повреждённый AC3 coefficient block%d/channel%d/bin%d", ErrCodec, block, channel, bin)
				}
			}
		}
	}
	// Остаток свежей собственной формы содержит нулевые auxbits, auxdatae=0 и crcrsv.
	// Последний crcrsv может быть 1 для защиты CRC2 от совпадения с синхрословом.
	if r.p > len(r.b)*8-2 {
		return nil, fmt.Errorf("%w: AC3 coefficients перекрывают trailer", ErrCodec)
	}
	for r.p < len(r.b)*8-1 {
		if r.read(1) != 0 {
			return nil, fmt.Errorf("%w: неподдержанные AC3 auxiliary data", ErrCodec)
		}
	}
	r.read(1) // crcrsv не влияет на коэффициенты и принадлежит области CRC2.
	if r.err != nil {
		return nil, r.err
	}
	// out выделяется только после полного доказательства всех используемых коэффициентов.
	out := bytes.Clone(payload)
	// position содержит положение dithflag, прочитанное из синтаксиса одного из шести блоков.
	for _, position := range ditherBits {
		out[position/8] &^= 1 << uint(7-position%8)
	}
	ac3UpdateCRC(out)
	return out, nil
}

// ac3ZeroExponents читает фактические группы экспонент из r в channel по strategy D15/D25/D45.
// Проверяются тройки по основанию 5, диапазон 0…24 и вместимость массива из 256 позиций.
func ac3ZeroExponents(r *bitReader, channel *ac3ZeroChannel, strategy int) {
	// repeat задаёт повтор каждой разностной экспоненты, groups — число 7-битных троек.
	repeat := 1 << (strategy - 1)
	groups := (channel.end + 3*repeat - 4) / (3 * repeat)
	// previous хранит последнюю абсолютную экспоненту, index — следующую позицию после DC.
	previous, index := int(r.read(4)), 1
	channel.exponents[0] = previous
	for range groups {
		// code содержит три разностные экспоненты по основанию 5, а не произвольные 7 битов.
		code := int(r.read(7))
		if code >= 125 {
			r.err = ErrCodec
			return
		}
		// divisor выбирает разностную экспоненту в исходном порядке от старших разрядов.
		for _, divisor := range [...]int{25, 5, 1} {
			previous += code/divisor%5 - 2
			if previous < 0 || previous > 24 {
				r.err = ErrCodec
				return
			}
			for range repeat {
				if index >= len(channel.exponents) {
					r.err = ErrCodec
					return
				}
				channel.exponents[index] = previous
				index++
			}
		}
	}
}

// ac3ZeroGroups хранит неиспользованные цифры сгруппированных квантователей до конца аудиоблока.
// Три группы независимы, но состояние каждой сохраняется при переходе к следующему каналу.
type ac3ZeroGroups struct {
	// digits содержит значения следующей пары либо тройки после разложения кода по основанию.
	digits [3][3]int
	// next указывает следующую цифру, count задаёт число ещё не потреблённых цифр.
	next, count [3]int
}

// zero читает одну мантиссу из r для bap и проверяет точное нулевое деквантованное значение.
// Неиспользованные цифры последней группы не считаются используемыми коэффициентами.
func (g *ac3ZeroGroups) zero(r *bitReader, bap uint8) bool {
	if bap == 0 {
		return true // Дизеринг будет снят только после доказательства остальных коэффициентов.
	}
	if bap == 1 || bap == 2 || bap == 4 {
		// group выбирает независимый групповой квантователь; radix и size задают его нормативную форму,
		// bits — число переданных битов целой группы.
		group, radix, size, bits := 0, 3, 3, 5
		switch bap {
		case 2:
			group, radix, bits = 1, 5, 7
		case 4:
			group, radix, size, bits = 2, 11, 2, 7
		}
		if g.count[group] == 0 {
			// code содержит целую группу; capacity исключает зарезервированные сочетания битов.
			code := int(r.read(bits))
			capacity := radix * radix
			if size == 3 {
				capacity *= radix
			}
			if code >= capacity {
				return false
			}
			// index раскладывает цифры с конца, сохраняя переданный порядок последующего чтения.
			for index := size - 1; index >= 0; index-- {
				g.digits[group][index] = code % radix
				code /= radix
			}
			g.next[group], g.count[group] = 0, size
		}
		// digit содержит код фактического коэффициента; центральное значение квантователя даёт 0.
		digit := g.digits[group][g.next[group]]
		g.next[group]++
		g.count[group]--
		return digit == radix/2
	}
	if bap == 3 {
		return r.read(3) == 3
	}
	if bap == 5 {
		return r.read(4) == 7
	}
	// width задаёт знаковую разрядность мантиссы BAP6…15; у нуля все переданные биты нулевые.
	width := int(bap) - 1
	switch bap {
	case 14:
		width = 14
	case 15:
		width = 16
	}
	return r.read(width) == 0
}

// ac3UpdateCRC обновляет обе CRC проверенного кадра raw фиксированной длины после изменения dithflag.
// CRC1 стоит перед данными: обратное деление полиномов вычисляет нужный префикс.
// CRC2 записывается как обычный остаток своей области; crcrsv защищает от совпадения с синхрословом.
func ac3UpdateCRC(raw []byte) {
	// split задаёт конец области CRC1 в байтах всего кадра.
	split := ((len(raw) >> 2) + (len(raw) >> 4)) << 1
	// prefix содержит состояние CRC перед байтами raw[4:split], необходимое для нулевого остатка.
	prefix := uint16(0)
	// index идёт по данным CRC1 с конца, отменяя восемь шагов деления полиномов на каждый байт.
	for index := split - 1; index >= 4; index-- {
		for range 8 {
			prefix = ac3ReverseCRCBit(prefix)
		}
		prefix ^= uint16(raw[index]) << 8
	}
	for range 16 {
		prefix = ac3ReverseCRCBit(prefix)
	}
	binary.BigEndian.PutUint16(raw[2:4], prefix)
	// crc содержит остаток второй области без двух завершающих байтов CRC.
	crc := audioCRC16(raw[split:len(raw)-2], 0x8005)
	if crc == 0x0b77 {
		raw[len(raw)-3] ^= 1
		crc ^= 0x8005
	}
	binary.BigEndian.PutUint16(raw[len(raw)-2:], crc)
}

// ac3ReverseCRCBit отменяет один шаг деления полиномов с младшими 16 битами 0x8005 для value.
// Младший бит нового остатка определяет прежний старший, поэтому операция обратима.
func ac3ReverseCRCBit(value uint16) uint16 {
	if value&1 != 0 {
		return (value^0x8005)>>1 | 0x8000
	}
	return value >> 1
}
