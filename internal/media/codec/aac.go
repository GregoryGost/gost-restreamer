package codec

// ValidateAACAU проверяет ASC и обязательный префикс канальных элементов raw AAC-LC.
// FIL/DSE пропускаются по проверенным длинам; PCE должен точно повторять карту ASC.
// Границу и длительность полной единицы доступа предоставляет адаптер. Проверка
// заголовка не декодирует спектральные данные Huffman и не доказывает их целостность.
// Для каналов с max_sfb=0 без инструментов дополнительно проверяется истинный ID_END.
func ValidateAACAU(headers, payload []byte) error {
	// config содержит подтверждённую карту AAC-LC. err сообщает о неподдержанном или
	// повреждённом ASC.
	config, err := parseAACConfig(headers)
	if err != nil {
		return err
	}
	if len(payload) == 0 || len(payload) > 1<<20 || len(payload) > 1 && payload[0] == 0xff && payload[1]&0xf0 == 0xf0 || len(payload) >= 4 && string(payload[:4]) == "ADIF" {
		return ErrCodec
	}
	// r читает синтаксические элементы из сжатой AAC-единицы по битам.
	r := bitReader{b: payload}
	// seen отмечает уже прочитанные канальные элементы. found считает подтверждённые элементы.
	var seen [8]bool
	// tags обнаруживает повтор type/tag внутри AU; для indexed config сами номера tags произвольны.
	var tags [4][16]bool
	found := 0
	for r.err == nil {
		// typ содержит трёхбитный идентификатор очередного элемента AAC.
		typ := r.read(3)
		if typ == 7 {
			if found != config.count {
				return ErrCodec
			}
			aacSkip(&r, (8-r.p%8)%8)
			return r.err
		}
		if typ == 5 {
			// repeated содержит полную карту PCE внутри AU, включая её хвост.
			repeated := aacReadPCE(&r, config.format.SampleRateHz)
			if !config.pce || found != 0 || !aacSameProgram(config, repeated) {
				return ErrCodec
			}
			continue
		}
		// tag содержит идентификатор канального элемента либо число байтов FIL.
		tag := r.read(4)
		switch typ {
		case 0, 1, 3: // ID_SCE, ID_CPE, ID_LFE.
			// index связывает прочитанные type/tag с единственным элементом карты ASC.
			index := config.elementIndex(typ, tag)
			if !config.pce {
				index = -1
				// candidate выбирает следующий элемент данного типа в нормативном порядке indexed config.
				for candidate := range config.count {
					if config.elements[candidate].typ == typ && !seen[candidate] {
						index = candidate
						break
					}
				}
			}
			if r.err != nil || index < 0 || seen[index] || tags[typ][tag] {
				return ErrCodec
			}
			tags[typ][tag] = true
			seen[index] = true
			found++
			// empty показывает, можно ли после префикса точно найти следующий элемент без Huffman.
			// prefixErr сообщает о повреждении проверенной части текущего элемента.
			empty, prefixErr := aacChannelPrefix(&r, typ == 1)
			if prefixErr != nil {
				return prefixErr
			}
			if !empty {
				// minimum учитывает минимальные заголовки ещё не прочитанных каналов и ID_END.
				minimum := 3
				// pending выбирает непрочитанный элемент для оценки его минимального размера.
				for pending := range config.count {
					if !seen[pending] {
						minimum += 29
						if config.elements[pending].typ == 1 {
							minimum += 14
						}
					}
				}
				if len(payload)*8-r.p < minimum {
					return ErrCodec
				}
				return nil
			}
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
// true возвращается только после полностью прочитанного пустого элемента; false без ошибки
// сохраняет прежнюю границу проверки префикса для спектра и дополнительных инструментов.
func aacChannelPrefix(r *bitReader, pair bool) (bool, error) {
	// common показывает, используют ли два канала CPE общее описание окна.
	common := pair && r.read(1) != 0
	// ics хранит параметры окна и частотных полос проверяемого канала.
	var ics aacICS
	if common {
		ics = aacReadICS(r)
		// mask содержит режим применения маски совместного стереокодирования.
		mask := r.read(2)
		if mask == 3 {
			return false, ErrCodec
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
	// minimum задаёт минимально необходимое число оставшихся битов для каналов и ID_END.
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
		return false, ErrCodec
	}
	if !aacReadEmptyChannel(r, ics) {
		return false, r.err
	}
	if pair {
		r.read(8)
		if !common {
			ics = aacReadICS(r)
		}
		if r.err != nil {
			return false, ErrCodec
		}
		if !aacReadEmptyChannel(r, ics) {
			return false, r.err
		}
	}
	return true, r.err
}

// elementIndex находит точную ссылку type/tag в проверенной карте либо возвращает -1.
func (c aacConfig) elementIndex(typ, tag uint32) int {
	// index и element обозначают позицию и значение проверяемого канального элемента.
	for index, element := range c.elements[:c.count] {
		if element.typ == typ && element.tag == tag {
			return index
		}
	}
	return -1
}

// aacSameProgram сравнивает speaker/tag карту PCE независимо от порядка объявления элементов.
func aacSameProgram(a, b aacConfig) bool {
	if a.format != b.format || a.program != b.program || a.count != b.count {
		return false
	}
	// element задаёт ссылку и динамики очередного элемента исходного PCE.
	for _, element := range a.elements[:a.count] {
		// index содержит позицию соответствующего элемента в повторённой карте.
		index := b.elementIndex(element.typ, element.tag)
		if index < 0 || b.elements[index].speakers != element.speakers {
			return false
		}
	}
	return true
}

// При max_sfb=0 section_data, scale_factor_data и spectral_data пусты.
// Три нулевых флага оставляют тело канала пустым; другие инструменты здесь не разбираются.
func aacReadEmptyChannel(r *bitReader, ics aacICS) bool {
	if ics.maxSFB != 0 {
		return false
	}
	// peek содержит копию позиции чтения для проверки пустого канала без преждевременного
	// продвижения исходного читателя.
	peek := *r
	// flags содержит три флага инструментов AAC; их нулевое значение подтверждает пустое тело
	// канала.
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

// Пропускает тело DSE с обычной или расширенной длиной и, при установленном флаге, предварительным
// выравниванием до байта.
func aacSkipData(r *bitReader) {
	// aligned показывает, требуется ли выравнивание DSE до границы байта.
	aligned := r.read(1) != 0
	// count содержит длину тела DSE в байтах с учётом возможного расширения.
	count := int(r.read(8))
	if count == 255 {
		count += int(r.read(8))
	}
	if aligned {
		aacSkip(r, (8-r.p%8)%8)
	}
	aacSkip(r, count*8)
}

// Пропускает FIL по count, включая дополнительный байт длины при count=15; выход за буфер фиксируется
// в r.err.
func aacSkipFill(r *bitReader, count uint32) {
	// n содержит полную длину пропускаемого FIL в байтах.
	n := int(count)
	if n == 15 {
		n += int(r.read(8)) - 1
	}
	aacSkip(r, n*8)
}

// aacICS хранит параметры окна AAC, необходимые для проверки канального префикса и минимального размера
// секций.
type aacICS struct {
	// maxSFB содержит число используемых частотных полос окна. groups содержит число групп коротких
	// окон либо единицу для длинного окна.
	maxSFB, groups int
	// short показывает, используется ли последовательность коротких окон.
	short bool
}

// Читает параметры окна и групп AAC-LC; зарезервированный бит, предсказание для длинного окна и
// max_sfb выше 51 делают чтение ошибочным.
func aacReadICS(r *bitReader) aacICS {
	// ics накапливает прочитанные параметры окна, начиная с одной группы.
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

// Возвращает минимальное число битов section_data для всех групп окна; при max_sfb=0 секции
// отсутствуют.
func (i aacICS) minimumSections() int {
	if i.maxSFB == 0 {
		return 0
	}
	// Хотя бы одна секция на группу: sect_cb и первый sect_len_incr.
	// lengthBits задаёт число битов первого приращения длины секции для длинного или короткого окна.
	lengthBits := 5
	if i.short {
		lengthBits = 3
	}
	return i.groups * (4 + lengthBits)
}

// Продвигает позицию на n битов; отрицательная длина или выход за остаток устанавливают ErrCodec без
// изменения позиции.
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
