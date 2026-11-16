package codec

import "github.com/GregoryGost/gost-restreamer/internal/core"

// aacElement связывает тип и tag элемента AAC с расположением его динамиков.
type aacElement struct {
	// typ различает SCE, CPE и LFE; tag задаёт ссылку PCE на элемент данного типа.
	typ, tag uint32
	// speakers содержит маску динамиков в принятом порядке FL, FR, FC, LFE, BL, BR, BC, SL, SR, FLC, FRC.
	speakers uint16
}

// aacConfig хранит проверенную конфигурацию без изменения исходного ASC.
type aacConfig struct {
	// format содержит наблюдаемый AAC-LC формат, включая отдельное обозначение 7.1(wide-back).
	format core.AudioFormat
	// elements задаёт полную карту канальных элементов, count — её используемую длину.
	elements [8]aacElement
	count    int
	// pce отмечает явную карту PCE; program сохраняет её element_instance_tag.
	pce     bool
	program uint32
}

// aacSampleRates задаёт нормативные частоты ASC по samplingFrequencyIndex.
var aacSampleRates = [...]uint32{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// AudioFormatASC получает фактический AAC-LC формат из полного ограниченного ASC.
// PCE проверяется вместе с типами, tags, расположением элементов и хвостом; исходные байты
// не перекодируются. SBR/PS, зависимость от другого кодировщика и кадры 960 сэмплов запрещены.
func AudioFormatASC(raw []byte) (core.AudioFormat, error) {
	// config содержит разобранную карту, err сообщает об отказе проверки ASC.
	config, err := parseAACConfig(raw)
	return config.format, err
}

// parseAACConfig читает только AAC-LC с известной картой либо полным встроенным PCE.
// Ограничение 512 байт включает максимальный 255-байтовый комментарий PCE без выделений по входным счётчикам.
func parseAACConfig(raw []byte) (aacConfig, error) {
	// config накапливает формат и карту, r читает исходные биты с проверкой границ.
	config := aacConfig{}
	r := bitReader{b: raw}
	if len(raw) < 2 || len(raw) > 512 || r.read(5) != 2 {
		return config, ErrCodec
	}
	// index выбирает табличную частоту либо явное 24-битное значение.
	index := r.read(4)
	// rate содержит частоту дискретизации в герцах из самого ASC.
	var rate uint32
	if index < uint32(len(aacSampleRates)) {
		rate = aacSampleRates[index]
	} else if index == 15 {
		rate = r.read(24)
	} else {
		return config, ErrCodec
	}
	// channels содержит channel_configuration; 0, 7 и 12 не равны числу каналов.
	channels := r.read(4)
	if rate == 0 || channels > 7 && channels != 12 || r.read(3) != 0 || r.err != nil {
		return config, ErrCodec
	}
	config.format = core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: rate}
	if channels == 0 {
		config = aacReadPCE(&r, rate)
	} else {
		config = aacDefaultConfig(channels, rate)
	}
	if r.err != nil || !aacASCTail(&r) {
		return aacConfig{}, ErrCodec
	}
	return config, nil
}

// aacDefaultConfig описывает нормативные channel_configuration 1..7 и 12.
// Конфигурация 7 содержит дополнительную переднюю пару: 7.1(wide-back); конфигурация 12 — обычную 7.1.
func aacDefaultConfig(channels, rate uint32) aacConfig {
	// config содержит таблицу канальных элементов, выбранную стандартным номером конфигурации.
	config := aacConfig{format: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: rate}}
	switch channels {
	case 1:
		config.add(0, 0, 4)
	case 2:
		config.add(1, 0, 3)
	default:
		config.add(0, 0, 4)
		config.add(1, 0, 3)
		switch channels {
		case 4:
			config.add(0, 1, 64)
		case 5, 6:
			config.add(1, 1, 48)
		case 7:
			config.add(1, 1, 512|1024)
			config.add(1, 2, 48)
		case 12:
			config.add(1, 1, 384)
			config.add(1, 2, 48)
		}
		if channels >= 6 {
			config.add(3, 0, 8)
		}
	}
	config.setLayout()
	return config
}

// add добавляет известный элемент в ограниченную карту; вызывающий проверяет вместимость.
func (c *aacConfig) add(typ, tag uint32, speakers uint16) {
	c.elements[c.count] = aacElement{typ: typ, tag: tag, speakers: speakers}
	c.count++
	c.format.Channels++
	if typ == 1 {
		c.format.Channels++
	}
}

// setLayout распознаёт только точные поддержанные наборы динамиков.
func (c *aacConfig) setLayout() {
	// mask объединяет динамики всех элементов; element обозначает очередной элемент карты.
	var mask uint16
	for _, element := range c.elements[:c.count] {
		mask |= element.speakers
	}
	c.format.ChannelLayout = map[uint16]string{4: "mono", 3: "stereo", 11: "2.1", 7: "3.0", 71: "4.0", 55: "5.0", 63: "5.1", 447: "7.1", 1599: "7.1(wide-back)"}[mask]
}

// NormalizeNativeAAC71ASC исправляет только известный пятибайтовый indexed ASC native AAC FFmpeg 7.1.5:
// channel_configuration 7 заменяется на 12, остальные биты сохраняются. Вызывающий обязан отдельно
// подтвердить собственное свежее кодирование encoding3 и ожидаемые AAC-LC/1024/7.1/8; ingest и copy
// эту функцию не вызывают. Уже нормативные config12 и PCE 7.1 возвращаются без изменения.
// Неизвестное представление возвращает ErrCodec; исходный срез никогда не меняется.
func NormalizeNativeAAC71ASC(raw []byte) ([]byte, error) {
	// config содержит полную проверенную карту исходного ASC; err сообщает о повреждении.
	config, err := parseAACConfig(raw)
	if err != nil {
		return nil, err
	}
	if config.format.ChannelLayout == "7.1" && config.format.Channels == 8 {
		return raw, nil
	}
	// index исключает явную частоту и неизвестные варианты сигнала native AAC.
	index := raw[0]&7<<1 | raw[1]>>7
	if config.pce || config.format.ChannelLayout != "7.1(wide-back)" || len(raw) != 5 || index > 12 || raw[1]&7 != 0 || raw[2] != 0x56 || raw[3] != 0xe5 || raw[4] != 0 {
		return nil, ErrCodec
	}
	// normalized получает отдельную копию ASC с изменёнными четырьмя битами конфигурации.
	normalized := append([]byte(nil), raw...)
	normalized[1] = normalized[1]&0x87 | 12<<3
	config, err = parseAACConfig(normalized)
	if err != nil || config.format.ChannelLayout != "7.1" || config.format.Channels != 8 {
		return nil, ErrCodec
	}
	return normalized, nil
}

// aacReadPCE читает все поля PCE, проверяет уникальные ссылки и выводит точную карту динамиков.
// Неоднозначные сочетания SCE/CPE, связанные данные и coupling не входят в поддержанный формат.
func aacReadPCE(r *bitReader, rate uint32) aacConfig {
	// config сохраняет объявленные элементы и идентификатор программы.
	config := aacConfig{pce: true, program: r.read(4), format: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: rate}}
	// object и index связывают PCE с AAC-LC и частотой родительского ASC.
	object, index := r.read(2), r.read(4)
	// counts содержит число front, side, back и LFE элементов в порядке полей PCE.
	counts := [4]uint32{r.read(4), r.read(4), r.read(4), r.read(2)}
	// assoc и coupling задают длины оставшихся таблиц, даже если их использование не поддержано.
	assoc, coupling := r.read(3), r.read(4)
	// mono и stereo содержат необязательные ссылки на элементы для сведения каналов; -1 означает отсутствие.
	mono, stereo := -1, -1
	if r.read(1) != 0 {
		mono = int(r.read(4))
	}
	if r.read(1) != 0 {
		stereo = int(r.read(4))
	}
	if r.read(1) != 0 {
		r.read(3) // matrix_mixdown_idx и pseudo_surround_enable.
	}
	// seen отмечает tags отдельно для SCE, CPE и LFE; singles и pairs считают элементы каждой группы.
	var seen [4][16]bool
	var singles, pairs [3]int
	// group выбирает группу динамиков, count ограничивает число её элементов.
	for group, count := range counts {
		for range count {
			// typ различает SCE/CPE по флагу is_cpe либо задаёт LFE для последней группы.
			typ := uint32(3)
			if group < 3 {
				typ = r.read(1)
			}
			// tag содержит ссылку на конкретный элемент данного типа.
			tag := r.read(4)
			if r.err != nil || config.count == len(config.elements) || seen[typ][tag] {
				r.err = ErrCodec
				return config
			}
			seen[typ][tag] = true
			if group < 3 {
				if typ == 1 {
					pairs[group]++
				} else {
					singles[group]++
				}
			}
			// speakers задаёт единственное допустимое назначение элемента в его группе.
			speakers := uint16(8)
			if group < 3 {
				speakers = [3][2]uint16{{4, 3}, {0, 384}, {64, 48}}[group][typ]
			}
			config.add(typ, tag, speakers)
		}
	}
	for range assoc {
		r.read(4)
	}
	for range coupling {
		r.read(5)
	}
	// padding содержит служебное выравнивание перед comment_field_bytes.
	padding := (8 - r.p%8) % 8
	if r.read(padding) != 0 {
		r.err = ErrCodec
	}
	// commentBytes задаёт полную длину комментария; пропуск проверяет и его последний байт.
	commentBytes := int(r.read(8))
	aacSkip(r, commentBytes*8)
	config.setLayout()
	if object != 1 || index >= uint32(len(aacSampleRates)) || aacSampleRates[index] != rate || assoc != 0 || coupling != 0 || config.format.ChannelLayout == "" || counts[3] > 1 || singles[0] > 1 || pairs[0] > 1 || singles[1] != 0 || pairs[1] > 1 || singles[2] > 1 || pairs[2] > 1 || singles[2] != 0 && pairs[2] != 0 || mono >= 0 && !seen[0][mono] || stereo >= 0 && !seen[1][stereo] {
		r.err = ErrCodec
	}
	return config
}

// aacASCTail принимает только точное нулевое заполнение либо выключенный SBR sync extension.
func aacASCTail(r *bitReader) bool {
	if len(r.b)*8-r.p > 7 {
		if r.read(11) != 0x2b7 || r.read(5) != 5 || r.read(1) != 0 {
			return false
		}
	}
	// padding задаёт точное число битов до конца последнего байта ASC.
	padding := (8 - r.p%8) % 8
	return len(r.b)*8-r.p == padding && r.read(padding) == 0 && r.err == nil
}
