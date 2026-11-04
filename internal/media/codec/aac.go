package codec

// ValidateAACAU проверяет ASC и обязательный префикс первого SCE/CPE в raw AAC-LC.
// Перед ним допускаются FIL и DSE с проверенными длинами. ADTS, ADIF, PCE и
// другие канальные элементы в этот ограниченный порт не входят.
// Границу и длительность полной единицы доступа предоставляет адаптер. Проверка
// заголовка не декодирует спектральные данные Huffman и не доказывает их целостность.
// Для каналов с max_sfb=0 без инструментов дополнительно проверяется истинный ID_END.
func ValidateAACAU(headers, payload []byte) error {
	format, err := AudioFormatASC(headers)
	if err != nil {
		return err
	}
	if len(payload) == 0 || len(payload) > 1<<20 || len(payload) > 1 && payload[0] == 0xff && payload[1]&0xf0 == 0xf0 || len(payload) >= 4 && string(payload[:4]) == "ADIF" {
		return ErrCodec
	}
	r := bitReader{b: payload}
	for r.err == nil {
		typ := r.read(3)
		tag := r.read(4)
		switch typ {
		case 0, 1: // ID_SCE, ID_CPE.
			if typ+1 != uint32(format.Channels) {
				return ErrCodec
			}
			return aacChannelPrefix(&r, typ == 1)
		case 4: // ID_DSE.
			aacSkipData(&r)
		case 6: // ID_FIL; поле tag содержит count.
			aacSkipFill(&r, tag)
		default:
			return ErrCodec
		}
	}
	return ErrCodec
}

// aacChannelPrefix следует таблицам 4.4–4.6 AAC: common_window меняет положение
// global_gain и ICS. Минимальный остаток учитывает обязательные поля оставшихся
// каналов и ID_END, но не ищет ID_END внутри непрочитанных спектральных данных.
func aacChannelPrefix(r *bitReader, pair bool) error {
	common := pair && r.read(1) != 0
	var ics aacICS
	if common {
		ics = aacReadICS(r)
		mask := r.read(2)
		if mask == 3 {
			return ErrCodec
		}
		if mask == 1 {
			aacSkip(r, ics.groups*ics.maxSFB)
		}
	}
	r.read(8) // global_gain.
	if !common {
		ics = aacReadICS(r)
	}
	// pulse_data_present, tns_data_present, gain_control_data_present и ID_END.
	minimum := 6 + ics.minimumSections()
	if pair {
		minimum += 8 + 3 // Второй global_gain и три флага инструментов.
		if common {
			minimum += ics.minimumSections()
		} else {
			minimum += 11 // Самый короткий допустимый ICS второго канала.
		}
	}
	if r.err != nil || len(r.b)*8-r.p < minimum {
		return ErrCodec
	}
	if !aacReadEmptyChannel(r, ics) {
		return r.err
	}
	if pair {
		r.read(8)
		if !common {
			ics = aacReadICS(r)
		}
		if r.err != nil {
			return ErrCodec
		}
		if !aacReadEmptyChannel(r, ics) {
			return r.err
		}
	}
	return aacEmptyTail(r)
}

// При max_sfb=0 section_data, scale_factor_data и spectral_data пусты.
// Три нулевых флага оставляют тело канала пустым; другие инструменты здесь не разбираются.
func aacReadEmptyChannel(r *bitReader, ics aacICS) bool {
	if ics.maxSFB != 0 {
		return false
	}
	peek := *r
	flags := peek.read(3)
	if peek.err != nil {
		*r = peek
		return false
	}
	if flags != 0 {
		return false
	}
	*r = peek
	return true
}

func aacEmptyTail(r *bitReader) error {
	for r.err == nil {
		typ := r.read(3)
		if typ == 7 { // ID_END найден по синтаксису после пустых канальных элементов.
			aacSkip(r, (8-r.p%8)%8)
			return r.err
		}
		tag := r.read(4)
		switch typ {
		case 4:
			aacSkipData(r)
		case 6:
			aacSkipFill(r, tag)
		default:
			return ErrCodec
		}
	}
	return ErrCodec
}

func aacSkipData(r *bitReader) {
	aligned := r.read(1) != 0
	count := int(r.read(8))
	if count == 255 {
		count += int(r.read(8))
	}
	if aligned {
		aacSkip(r, (8-r.p%8)%8)
	}
	aacSkip(r, count*8)
}

func aacSkipFill(r *bitReader, count uint32) {
	n := int(count)
	if n == 15 {
		n += int(r.read(8)) - 1
	}
	aacSkip(r, n*8)
}

type aacICS struct {
	maxSFB, groups int
	short          bool
}

func aacReadICS(r *bitReader) aacICS {
	ics := aacICS{groups: 1}
	if r.read(1) != 0 { // ics_reserved_bit.
		r.err = ErrCodec
	}
	ics.short = r.read(2) == 2
	r.read(1) // window_shape.
	if ics.short {
		ics.maxSFB = int(r.read(4))
		for range 7 {
			if r.read(1) == 0 {
				ics.groups++
			}
		}
	} else {
		ics.maxSFB = int(r.read(6))
		if r.read(1) != 0 { // predictor_data_present запрещён для AAC-LC.
			r.err = ErrCodec
		}
		// 51 — наибольшее num_swb для поддержанной длины кадра 1024.
		if ics.maxSFB > 51 {
			r.err = ErrCodec
		}
	}
	return ics
}

func (i aacICS) minimumSections() int {
	if i.maxSFB == 0 {
		return 0
	}
	// Хотя бы одна секция на группу: sect_cb и первый sect_len_incr.
	lengthBits := 5
	if i.short {
		lengthBits = 3
	}
	return i.groups * (4 + lengthBits)
}

func aacSkip(r *bitReader, n int) {
	if r.err != nil {
		return
	}
	if n < 0 || n > len(r.b)*8-r.p {
		r.err = ErrCodec
		return
	}
	r.p += n
}
