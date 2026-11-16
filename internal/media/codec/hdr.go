package codec

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// HDRState сохраняет правила действия сведений конкретного кодека одного последовательного источника.
// Reset выполняется владельцем при новой последовательности, а не при смене внешнего переопределения.
type HDRState struct {
	// current сохраняет действующие сведения сжатого потока последовательности.
	current media.HDRMetadata
	// started отмечает первую проверенную картинку после независимого старта.
	started bool
}

// Reset очищает только состояние кодека при начале новой последовательности источника.
func (s *HDRState) Reset() { *s = HDRState{} }

// ParseHDRSEI разбирает один полный SEI NAL без поля длины; hevc выбирает двухбайтовый заголовок.
// Обрамление RBSP и размеры проверяются; неизвестные типы сообщений пропускаются без интерпретации.
func ParseHDRSEI(nal []byte, hevc bool) (media.HDRMetadata, error) {
	// offset задаёт длину заголовка NAL; source обозначает происхождение восстановленного блока.
	offset, source := 1, media.HDRSourceH264SEI
	if hevc {
		offset, source = 2, media.HDRSourceHEVCSEI
	}
	if len(nal) <= offset {
		return media.HDRMetadata{}, ErrCodec
	}
	if !hevc && nal[0]&0x9f != 6 || hevc && (nal[0]&0x81 != 0 || nal[1]&7 == 0 || ((nal[0]>>1)&63 != 39 && (nal[0]>>1)&63 != 40)) {
		return media.HDRMetadata{}, ErrCodec
	}
	// rbsp содержит сообщение без защитных байтов.
	rbsp := h264.EmulationPreventionRemove(nal[offset:])
	// result накапливает не более одного отличающегося блока каждого типа.
	var result media.HDRMetadata
	// count ограничивает число сообщений даже при пустой полезной нагрузке.
	count := 0
	for len(rbsp) > 0 {
		if len(rbsp) == 1 && rbsp[0] == 0x80 {
			return result, nil
		}
		count++
		if count > 4096 {
			return result, ErrLimit
		}
		// typ и size накапливают SEI payloadType/payloadSize с повторяющимся байтом 255.
		typ, size := 0, 0
		for len(rbsp) > 0 && rbsp[0] == 255 {
			typ += 255
			rbsp = rbsp[1:]
		}
		if len(rbsp) == 0 {
			return result, ErrCodec
		}
		typ += int(rbsp[0])
		rbsp = rbsp[1:]
		for len(rbsp) > 0 && rbsp[0] == 255 {
			size += 255
			rbsp = rbsp[1:]
		}
		if len(rbsp) == 0 {
			return result, ErrCodec
		}
		size += int(rbsp[0])
		rbsp = rbsp[1:]
		if size > len(rbsp) {
			return result, ErrCodec
		}
		// block содержит только HDR-поля текущего сообщения.
		var block media.HDRMetadata
		switch typ {
		case 137:
			if size != 24 {
				return result, ErrCodec
			}
			block.MDCV = parseMastering(rbsp[:size], false, source)
		case 144:
			if size != 4 {
				return result, ErrCodec
			}
			block.CLL = parseLight(rbsp[:size], source)
		}
		// err сообщает о противоречащем повторе HDR внутри одного NAL.
		if err := mergeHDR(&result, block); err != nil {
			return result, err
		}
		rbsp = rbsp[size:]
	}
	return result, ErrCodec
}

// ParseAV1HDR извлекает статические HDR-сведения из полного потока OBU без четырёхбайтового av1C.
// Тип metadata и завершающие биты входят в нагрузку OBU; CLL/MDCV имеют полные размеры 8/28 байтов.
func ParseAV1HDR(payload []byte) (media.HDRMetadata, error) {
	// obus содержит ограниченный набор OBU; err сообщает о нарушенном обрамлении.
	obus, err := SplitOBUs(payload)
	if err != nil {
		return media.HDRMetadata{}, err
	}
	// result накапливает блоки с авторитетной AV1-сеткой.
	var result media.HDRMetadata
	// obu обозначает очередной OBU при поиске метаданных.
	for _, obu := range obus {
		if obu.typ != 5 {
			continue
		}
		// body содержит непрочитанную часть нагрузки метаданных после типа LEB128.
		body := obu.payload
		// typ накапливает беззнаковый metadata_type, shift задаёт следующий разряд LEB128.
		typ, shift := uint64(0), uint(0)
		for {
			if len(body) == 0 || shift >= 56 {
				return result, ErrCodec
			}
			// b содержит очередной байт metadata_type.
			b := body[0]
			body = body[1:]
			typ |= uint64(b&127) << shift
			shift += 7
			if b&128 == 0 {
				break
			}
		}
		// block содержит только статические сведения текущего OBU.
		var block media.HDRMetadata
		switch typ {
		case 1:
			if len(body) != 5 || body[4] != 0x80 {
				return result, ErrCodec
			}
			block.CLL = parseLight(body[:4], media.HDRSourceAV1OBU)
		case 2:
			if len(body) != 25 || body[24] != 0x80 {
				return result, ErrCodec
			}
			block.MDCV = parseMastering(body[:24], true, media.HDRSourceAV1OBU)
		}
		if err = mergeHDR(&result, block); err != nil {
			return result, err
		}
	}
	return result, nil
}

// parseMastering преобразует 24 байта известной SEI либо AV1/SmDm сетки в RGB без округления.
// Вызывающий предварительно проверяет длину; кодовые слова SEI вне известных диапазонов сохраняют неизвестность.
func parseMastering(payload []byte, binaryGrid bool, source media.HDRSource) media.HDRMasteringDisplay {
	// result хранит описание дисплея с отдельно сохраняемыми кодовыми словами.
	result := media.HDRMasteringDisplay{Present: true, Source: source}
	// grid задаёт исходную сетку координат; maximum/minimum — сетки соответствующих яркостей.
	grid, maximum, minimum := media.HDRGridSEIChromaticity, media.HDRGridSEILuminance, media.HDRGridSEILuminance
	if binaryGrid {
		grid, maximum, minimum = media.HDRGridBinaryChromaticity, media.HDRGridBinaryMaxLuminance, media.HDRGridBinaryMinLuminance
	}
	// values сохраняет восемь координат в порядке исходного сообщения.
	var values [8]media.HDRValue
	// i выбирает очередную координату; raw содержит её исходное кодовое слово.
	for i := range values {
		raw := uint64(be.Uint16(payload[2*i:]))
		values[i] = media.HDRValue{Raw: raw, Grid: grid, Known: binaryGrid || raw >= 5 && (i%2 == 0 && raw <= 37000 || i%2 == 1 && raw <= 42000)}
	}
	// i обозначает входную первичную компоненту; index преобразует SEI GBR в внутренний RGB.
	for i := 0; i < 3; i++ {
		index := i
		if !binaryGrid {
			index = [3]int{1, 2, 0}[i]
		}
		result.Primaries[index] = media.HDRChromaticity{X: values[2*i], Y: values[2*i+1]}
	}
	result.WhitePoint = media.HDRChromaticity{X: values[6], Y: values[7]}
	// maxValue и minValue содержат неизменённые 32-битные значения яркости.
	maxValue, minValue := uint64(be.Uint32(payload[16:20])), uint64(be.Uint32(payload[20:24]))
	result.MaxLuminance = media.HDRValue{Raw: maxValue, Grid: maximum, Known: binaryGrid || maxValue >= 50000 && maxValue <= 100000000}
	result.MinLuminance = media.HDRValue{Raw: minValue, Grid: minimum, Known: binaryGrid || minValue >= 1 && minValue <= 50000}
	return result
}

// parseLight сохраняет два целых CLL; ноль означает отсутствие указанной границы и не становится известным нулём.
func parseLight(payload []byte, source media.HDRSource) media.HDRContentLight {
	// maximum и average содержат исходные целочисленные границы яркости.
	maximum, average := uint64(be.Uint16(payload)), uint64(be.Uint16(payload[2:]))
	return media.HDRContentLight{Present: true, Source: source, MaxCLL: media.HDRValue{Raw: maximum, Grid: media.HDRGridInteger, Known: maximum != 0}, MaxFALL: media.HDRValue{Raw: average, Grid: media.HDRGridInteger, Known: average != 0}}
}

// mergeHDR объединяет непересекающиеся блоки одинаковой сигнализации и проверяет точные повторы.
func mergeHDR(target *media.HDRMetadata, incoming media.HDRMetadata) error {
	if incoming.MDCV.Present {
		if target.MDCV.Present && target.MDCV != incoming.MDCV {
			return fmt.Errorf("%w: меняющийся MDCV", ErrCodec)
		}
		target.MDCV = incoming.MDCV
	}
	if incoming.CLL.Present {
		if target.CLL.Present && target.CLL != incoming.CLL {
			return fmt.Errorf("%w: меняющийся CLL", ErrCodec)
		}
		target.CLL = incoming.CLL
	}
	return nil
}

// HDRFromConfig извлекает статические сведения сжатого потока из av1C либо массивов SEI hvcC.
// H.264 avcC не содержит SEI и возвращает отсутствие блоков.
func HDRFromConfig(kind core.Codec, headers []byte) (media.HDRMetadata, error) {
	switch kind {
	case core.CodecAV1:
		if len(headers) < 4 {
			return media.HDRMetadata{}, ErrCodec
		}
		return ParseAV1HDR(headers[4:])
	case core.CodecHEVC:
		// description содержит проверенные массивы NAL HEVC; err сообщает о повреждённом hvcC.
		description, err := ParseHEVCConfig(headers)
		if err != nil && !errors.Is(err, ErrUnready) {
			return media.HDRMetadata{}, err
		}
		// result объединяет только массивы SEI конфигурации.
		var result media.HDRMetadata
		// nal обозначает очередной исходный NAL массива записи hvcC.
		for _, nal := range description.NALUs {
			if (nal[0]>>1)&63 == 39 || (nal[0]>>1)&63 == 40 { /* block содержит HDR одного SEI; parseErr сохраняет отказ. */
				block, parseErr := ParseHDRSEI(nal, true)
				if parseErr != nil {
					return result, parseErr
				}
				if parseErr = mergeHDR(&result, block); parseErr != nil {
					return result, parseErr
				}
			}
		}
		return result, nil
	}
	return media.HDRMetadata{}, nil
}

// Observe применяет правила действия сведений кодека к одному полному AU/TU.
// H.264 требует HDR в первом IDR CVS; HEVC требует ранний MDCV, но допускает поздний CLL.
// Метаданные AV1 сохраняются до следующего сообщения соответствующего типа или новой sequence.
func (s *HDRState) Observe(kind core.Codec, headers, payload []byte, independent bool) (media.HDRMetadata, error) {
	// next содержит локальную копию до успешной атомарной проверки AU.
	next := *s
	if independent {
		next.current = media.HDRMetadata{}
		next.started = false
	}
	// incoming содержит HDR только текущего AU; err сообщает о нарушении обрамления или несовместимых повторах.
	var incoming media.HDRMetadata
	var err error
	if !next.started {
		incoming, err = HDRFromConfig(kind, headers)
		if err != nil {
			return s.current, err
		}
	}
	switch kind {
	case core.CodecAV1:
		// block содержит метаданные текущего TU.
		// parseErr сохраняет отказ разбора OBU текущего TU.
		block, parseErr := ParseAV1HDR(payload)
		if parseErr != nil {
			return s.current, parseErr
		}
		if block.MDCV.Present {
			incoming.MDCV = block.MDCV
		}
		if block.CLL.Present {
			incoming.CLL = block.CLL
		}
	case core.CodecH264, core.CodecHEVC:
		// nalus содержит NAL с четырёхбайтовыми длинами поддержанного порта.
		// parseErr сообщает о нарушении четырёхбайтовых длин NAL текущего AU.
		nalus, parseErr := splitLengthNALUs(payload, 4)
		if parseErr != nil {
			return s.current, parseErr
		}
		// nal обозначает очередной NAL единицы доступа.
		for _, nal := range nalus {
			if len(nal) == 0 {
				return s.current, ErrCodec
			}
			if kind == core.CodecH264 && nal[0]&31 == 6 || kind == core.CodecHEVC && ((nal[0]>>1)&63 == 39 || (nal[0]>>1)&63 == 40) {
				// block содержит один SEI NAL с исходной сеткой.
				// parseErr сообщает о повреждённом SEI текущей NAL.
				block, parseErr := ParseHDRSEI(nal, kind == core.CodecHEVC)
				if parseErr != nil {
					return s.current, parseErr
				}
				if parseErr = mergeHDR(&incoming, block); parseErr != nil {
					return s.current, parseErr
				}
			}
		}
	}
	if kind == core.CodecAV1 {
		if incoming.MDCV.Present {
			next.current.MDCV = incoming.MDCV
		}
		if incoming.CLL.Present {
			next.current.CLL = incoming.CLL
		}
	} else {
		if next.started && incoming.MDCV.Present && !next.current.MDCV.Present || next.started && kind == core.CodecH264 && incoming.CLL.Present && !next.current.CLL.Present {
			return s.current, fmt.Errorf("%w: поздний HDR SEI", ErrCodec)
		}
		if err = mergeHDR(&next.current, incoming); err != nil {
			return s.current, err
		}
	}
	if !next.started && !independent {
		return s.current, ErrUnready
	}
	next.started = true
	*s = next
	return next.current, nil
}

// MatchHDRProjection сверяет внешний дескриптор с направленной проекцией авторитетного источника.
// Неизвестный компонент сравнивается только в той же исходной сетке и не преобразуется в известный ноль.
func MatchHDRProjection(authority, external media.HDRMetadata) error {
	// left и right содержат пары сравниваемых компонентов каждого присутствующего внешнего блока.
	var left, right []media.HDRValue
	if external.MDCV.Present {
		if !authority.MDCV.Present {
			return ErrCodec
		}
		// a и b содержат отдельные копии компонентов для построения срезов сравнения.
		a, b := authority.MDCV.Values(), external.MDCV.Values()
		left = append(left, a[:]...)
		right = append(right, b[:]...)
	}
	if external.CLL.Present {
		if !authority.CLL.Present {
			return ErrCodec
		}
		left = append(left, authority.CLL.MaxCLL, authority.CLL.MaxFALL)
		right = append(right, external.CLL.MaxCLL, external.CLL.MaxFALL)
	}
	// i выбирает соответствующие значения разных исходных сеток.
	for i := range left {
		if left[i].Known != right[i].Known {
			return ErrCodec
		}
		if !left[i].Known {
			if left[i] != right[i] {
				return media.ErrHDRUnavailable
			}
			continue
		}
		// projected содержит единственное допустимое округление компонента авторитетного источника.
		// err сообщает о неизвестном либо непредставимом компоненте проекции.
		projected, err := left[i].Project(right[i].Grid)
		if err != nil {
			return err
		}
		if projected != right[i].Raw {
			return ErrCodec
		}
	}
	return nil
}

// AV1ConfigFromTUWithMetadata сохраняет дополнительные OBU конфигурации нового явного пути.
// Старый AV1ConfigFromTU сохраняет только заголовок последовательности; исходный AU при нормализации не изменяется.
func AV1ConfigFromTUWithMetadata(template, tu []byte, fps core.FrameRate) ([]byte, core.VideoFormat, error) {
	// headers и actual получены старым механизмом извлечения заголовка последовательности; err сохраняет отказ.
	headers, actual, err := AV1ConfigFromTU(template, tu, fps)
	if err != nil {
		return nil, actual, err
	}
	// obus содержит ограниченный набор OBU конфигурации шаблона, затем метаданные первого TU.
	obus, err := SplitOBUs(template[4:])
	if err != nil {
		return nil, actual, err
	}
	// seen предотвращает дублирование одного и того же исходного OBU без изменения его байтов.
	seen := make(map[string]bool)
	// obu обозначает очередную дополнительную запись av1C.
	for _, obu := range obus {
		if obu.typ != 1 {
			headers = append(headers, obu.raw...)
			seen[string(obu.raw)] = true
		}
	}
	obus, err = SplitOBUs(tu)
	if err != nil {
		return nil, actual, err
	}
	// obu обозначает OBU метаданных первого независимого кадра.
	for _, obu := range obus {
		if obu.typ == 5 && !seen[string(obu.raw)] {
			headers = append(headers, obu.raw...)
			seen[string(obu.raw)] = true
		}
	}
	if _, err = ParseAV1HDR(headers[4:]); err != nil {
		return nil, actual, err
	}
	return bytes.Clone(headers), actual, nil
}

// RestoreSourceHDR дополняет отсутствующие блоки только у свежего собственного результата.
// Вызывающий подтверждает Encoding5 и проверенный статический снимок source. Присутствующие выходные
// сведения обязаны быть направленной проекцией source; иное описание дисплея молча не заменяется.
func RestoreSourceHDR(output, source media.HDRMetadata) (media.HDRMetadata, error) {
	// err сообщает о некорректном выходном снимке до дополнения исходными сведениями.
	if err := output.Validate(); err != nil {
		return output, err
	}
	// err сообщает о некорректном исходном снимке, который нельзя переносить в результат.
	if err := source.Validate(); err != nil {
		return output, err
	}
	// err сообщает о противоречии уже присутствующих выходных блоков исходному снимку.
	if err := MatchHDRProjection(source, output); err != nil {
		return output, err
	}
	if !output.MDCV.Present {
		output.MDCV = source.MDCV
	}
	if !output.CLL.Present {
		output.CLL = source.CLL
	}
	if output.ColorConfig == (media.ColorConfigProvenance{}) {
		output.ColorConfig = source.ColorConfig
	}
	return output, nil
}

// ReconcileFreshHDR выбирает фактический авторитетный источник в сжатом потоке свежего собственного Encoding5.
// Вызывающий доказывает общий снимок source и происхождение контейнера. Обе независимые
// проекции сначала сверяются с source: округление через промежуточную сетку не предполагается.
// Возвращённые сведения используются новым мультиплексором для направленной коррекции дескриптора; исходный AU
// не меняется. Для произвольного входного потока этот механизм неприменим.
func ReconcileFreshHDR(source, container, inband media.HDRMetadata) (media.HDRMetadata, error) {
	// err сообщает о некорректном авторитетном снимке до сравнения его проекций.
	if err := source.Validate(); err != nil {
		return media.HDRMetadata{}, err
	}
	// err сообщает о некорректных значениях фактического контейнерного дескриптора.
	if err := container.Validate(); err != nil {
		return media.HDRMetadata{}, err
	}
	// err сообщает о некорректных метаданных, извлечённых из сжатого потока.
	if err := inband.Validate(); err != nil {
		return media.HDRMetadata{}, err
	}
	// err сообщает о несоответствии контейнера прямой проекции исходного снимка.
	if err := MatchHDRProjection(source, container); err != nil {
		return media.HDRMetadata{}, err
	}
	// err сообщает о несоответствии внутрипоточных метаданных прямой проекции источника.
	if err := MatchHDRProjection(source, inband); err != nil {
		return media.HDRMetadata{}, err
	}
	if !inband.MDCV.Present {
		inband.MDCV = container.MDCV
	}
	if !inband.CLL.Present {
		inband.CLL = container.CLL
	}
	if inband.ColorConfig == (media.ColorConfigProvenance{}) {
		inband.ColorConfig = container.ColorConfig
	}
	return RestoreSourceHDR(inband, source)
}

// DecodeHDRMastering читает ровно 24 байта полей дисплея SEI/mdcv либо AV1/SmDm.
// binaryGrid выбирает RGB/UQ, иначе GBR/SEI; source сохраняет проверенное происхождение.
func DecodeHDRMastering(payload []byte, binaryGrid bool, source media.HDRSource) (media.HDRMasteringDisplay, error) {
	if len(payload) != 24 {
		return media.HDRMasteringDisplay{}, ErrCodec
	}
	return parseMastering(payload, binaryGrid, source), nil
}

// DecodeHDRLight читает два uint16 границ CLL без обрамления NAL/OBU/блока.
func DecodeHDRLight(payload []byte, source media.HDRSource) (media.HDRContentLight, error) {
	if len(payload) != 4 {
		return media.HDRContentLight{}, ErrCodec
	}
	return parseLight(payload, source), nil
}
