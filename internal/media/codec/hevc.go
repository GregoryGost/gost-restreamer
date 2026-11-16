package codec

import (
	"bytes"
	"errors"
	"fmt"
	"math"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
)

// HEVCDescription хранит проверенные наборы параметров однослойного прогрессивного HEVC.
type HEVCDescription struct {
	// NALUs содержат отдельные копии NAL из массивов записи hvcC, включая разрешённые SEI.
	NALUs [][]byte
	// SPS содержит разобранные фактические параметры последовательности.
	SPS h265.SPS
	// Complete показывает array_completeness для VPS/SPS/PPS, обязательные для записи hvc1.
	Complete bool
	// completeArrays сохраняет отдельный запрет дополнений для каждого массива наборов параметров.
	completeArrays [64]bool
	// pps связывает ограниченные идентификаторы PPS с проверенными заголовками.
	pps map[uint32]h265.PPS
}

// splitLengthNALUs разбирает AU с полями длины перед NAL без копирования исходной нагрузки.
// Число NAL ограничено 4096; lengthSize задаёт 1/2/4 байта, нулевая и усечённая NAL запрещены.
func splitLengthNALUs(payload []byte, lengthSize int) ([][]byte, error) {
	if lengthSize != 1 && lengthSize != 2 && lengthSize != 4 {
		return nil, ErrCodec
	}
	// result содержит NAL в исходном порядке, без полей длины.
	var result [][]byte
	for len(payload) > 0 {
		if len(result) >= 4096 {
			return nil, ErrLimit
		}
		if len(payload) < lengthSize {
			return nil, ErrCodec
		}
		// size накапливает размер NAL от старшего байта к младшему.
		size := uint64(0)
		// index выбирает байт длины текущей NAL.
		for index := 0; index < lengthSize; index++ {
			size = size<<8 | uint64(payload[index])
		}
		payload = payload[lengthSize:]
		if size == 0 || size > uint64(len(payload)) {
			return nil, ErrCodec
		}
		result = append(result, payload[:int(size)])
		payload = payload[int(size):]
	}
	if len(result) == 0 {
		return nil, ErrCodec
	}
	return result, nil
}

// hevcNAL проверяет запрещённый бит, единственный слой и temporal_id_plus1 NAL.
func hevcNAL(nal []byte) bool {
	return len(nal) >= 3 && nal[0]&0x81 == 0 && nal[1]&0xf8 == 0 && nal[1]&7 == 1
}

// ParseHEVCConfig проверяет hvcC с четырёхбайтовыми длинами NAL и единственными VPS/SPS.
// Все PPS проверяются вместе со ссылками; неподдержанные многослойность и RExt дают ErrUnsupported.
func ParseHEVCConfig(raw []byte) (HEVCDescription, error) {
	// result накапливает собственные копии заголовков после успешной проверки обрамления.
	result := HEVCDescription{pps: make(map[uint32]h265.PPS), Complete: true}
	if len(raw) < 23 || raw[0] != 1 || raw[13]&0xf0 != 0xf0 || raw[15]&0xfc != 0xfc || raw[16]&0xfc != 0xfc || raw[17]&0xf8 != 0xf8 || raw[18]&0xf8 != 0xf8 || raw[21]&3 != 3 {
		return result, ErrCodec
	}
	// tail содержит ещё не разобранные массивы; counts учитывает число VPS/SPS/PPS.
	tail := raw[23:]
	counts := [3]int{}
	// spsRaw и vpsRaw сохраняют исходные NAL для проверки PTL и ссылок.
	var spsRaw, vpsRaw []byte
	// arrays ограничивает повтор одного типа в hvcC нормативными шестью битами типа.
	var arrays [64]bool
	for range int(raw[22]) {
		if len(tail) < 3 {
			return result, ErrCodec
		}
		// typ задаёт тип массива, complete — полноту объявленного множества NAL.
		typ, complete := tail[0]&63, tail[0]&128 != 0
		if tail[0]&64 != 0 || arrays[typ] {
			return result, ErrCodec
		}
		arrays[typ] = true
		// count задаёт ограниченное число NAL в текущем массиве.
		count := int(be.Uint16(tail[1:3]))
		tail = tail[3:]
		if count == 0 || count > 4096 || len(result.NALUs)+count > 4096 {
			return result, ErrLimit
		}
		if typ >= 32 && typ <= 34 {
			result.Complete = result.Complete && complete
			result.completeArrays[typ] = complete
		}
		if typ != 32 && typ != 33 && typ != 34 && typ != 39 && typ != 40 {
			return result, ErrUnsupported
		}
		for range count {
			if len(tail) < 2 {
				return result, ErrCodec
			}
			// size содержит размер текущей NAL, заданный 16-битным полем длины.
			size := int(be.Uint16(tail))
			tail = tail[2:]
			if size > len(tail) || size < 3 {
				return result, ErrCodec
			}
			// nal содержит отдельную копию параметра, принадлежащую result.
			nal := bytes.Clone(tail[:size])
			tail = tail[size:]
			if !hevcNAL(nal) || (nal[0]>>1)&63 != typ {
				return result, ErrCodec
			}
			result.NALUs = append(result.NALUs, nal)
			switch typ {
			case 32:
				counts[0]++
				vpsRaw = nal
			case 33:
				counts[1]++
				spsRaw = nal
			case 34:
				counts[2]++
				// Доступные поля PPS проверяются и без SPS: отсутствие контекста не скрывает повреждение.
				if !validHEVCPPS(nal, nil) {
					return result, ErrCodec
				}
			}
		}
	}
	if len(tail) != 0 || counts[0] > 1 || counts[1] > 1 || counts[2] > 64 {
		return result, ErrCodec
	}
	if counts[1] == 0 {
		return result, ErrUnready
	}
	// err сохраняет отказ извлечения значений SPS из исходной NAL.
	if err := result.SPS.Unmarshal(spsRaw); err != nil {
		return result, fmt.Errorf("%w: HEVC SPS: %v", ErrCodec, err)
	}
	// s указывает на разобранные поля SPS для ограничений реализованного пути.
	s := &result.SPS
	if s.MaxSubLayersMinus1 != 0 || s.ID > 15 || s.ChromaFormatIdc != 1 || s.SeparateColourPlaneFlag || s.BitDepthLumaMinus8 != s.BitDepthChromaMinus8 || s.BitDepthLumaMinus8 != 0 && s.BitDepthLumaMinus8 != 2 || s.ProfileTierLevel.GeneralProfileSpace != 0 || s.ProfileTierLevel.GeneralProfileIdc != 1 && s.ProfileTierLevel.GeneralProfileIdc != 2 || s.ProfileTierLevel.GeneralInterlacedSourceFlag || s.VUI != nil && s.VUI.FieldSeqFlag {
		return result, ErrUnsupported
	}
	switch s.ProfileTierLevel.GeneralLevelIdc {
	case 30, 60, 63, 90, 93, 120, 123, 150, 153, 156, 180, 183, 186, 189, 210, 213, 216, 255:
	default:
		return result, ErrCodec
	}
	if s.ProfileTierLevel.GeneralLevelIdc < 120 && s.ProfileTierLevel.GeneralTierFlag != 0 || s.ProfileTierLevel.GeneralLevelIdc == 255 && s.ProfileTierLevel.GeneralTierFlag == 0 {
		return result, ErrCodec
	}
	if s.ProfileTierLevel.GeneralProfileIdc == 1 && s.BitDepthLumaMinus8 != 0 {
		return result, ErrCodec
	}
	// rbsp содержит исходный SPS для точного сравнения PTL hvcC без защитных байтов.
	rbsp := h264.EmulationPreventionRemove(spsRaw[2:])
	if len(rbsp) < 13 || (!bytes.Equal(raw[1:12], rbsp[1:12]) || raw[12] < rbsp[12]) || uint32(raw[16]&3) != s.ChromaFormatIdc || uint32(raw[17]&7) != s.BitDepthLumaMinus8 || uint32(raw[18]&7) != s.BitDepthChromaMinus8 {
		return result, ErrCodec
	}
	if !validHEVCSPS(spsRaw) || vpsRaw != nil && !validHEVCVPS(vpsRaw, s.VPSID, rbsp[1:13]) {
		return result, ErrCodec
	}
	// nal обозначает PPS, для которой проверяется полный синтаксис и ссылка на единственный SPS.
	for _, nal := range result.NALUs {
		if (nal[0]>>1)&63 == 34 {
			// p содержит поля PPS; err сохраняет ошибку стороннего извлечения значений.
			var p h265.PPS
			if err := p.Unmarshal(nal); err != nil {
				return result, ErrCodec
			}
			if p.ID > 63 || p.SPSID != uint32(s.ID) || !validHEVCPPS(nal, &s.BitDepthLumaMinus8) {
				return result, ErrCodec
			}
			// exists отмечает повтор идентификатора PPS, запрещённый внутри hvcC.
			if _, exists := result.pps[p.ID]; exists {
				return result, ErrCodec
			}
			result.pps[p.ID] = p
		}
	}
	if counts[0] == 0 || counts[2] == 0 {
		return result, ErrUnready
	}
	return result, nil
}

// HEVCFormat восстанавливает фактический формат из hvcC/VUI; отсутствующий FPS возвращает ErrUnready.
// Нулевые Width/Height разрешают извлечь геометрию, ненулевые должны совпадать с SPS.
func HEVCFormat(desc VideoDescription) (core.VideoFormat, error) {
	// d содержит проверенные заголовки; err сообщает о повреждённой или неподдержанной конфигурации.
	d, err := ParseHEVCConfig(desc.Headers)
	if err != nil {
		return core.VideoFormat{}, err
	}
	return d.videoFormat(desc, core.FrameRate{})
}

// videoFormat собирает фактический формат HEVC; measuredFPS используется только при отсутствующих временных параметрах VUI.
func (d HEVCDescription) videoFormat(desc VideoDescription, measuredFPS core.FrameRate) (core.VideoFormat, error) {
	// s содержит фактический SPS, width/height — геометрию до обрезки по окну соответствия.
	s := d.SPS
	width, height := uint64(s.PicWidthInLumaSamples), uint64(s.PicHeightInLumaSamples)
	if s.ConformanceWindow != nil {
		// cropX/cropY содержат суммарную 4:2:0 обрезку в пикселях, вычисленную без переполнения uint32.
		cropX, cropY := (uint64(s.ConformanceWindow.LeftOffset)+uint64(s.ConformanceWindow.RightOffset))*2, (uint64(s.ConformanceWindow.TopOffset)+uint64(s.ConformanceWindow.BottomOffset))*2
		if cropX >= width || cropY >= height {
			return core.VideoFormat{}, ErrCodec
		}
		width -= cropX
		height -= cropY
	}
	if width == 0 || height == 0 || width > math.MaxUint32 || height > math.MaxUint32 || desc.Width != 0 && uint64(desc.Width) != width || desc.Height != 0 && uint64(desc.Height) != height {
		return core.VideoFormat{}, ErrCodec
	}
	// fps сохраняет только подтверждённую частоту VUI либо измерения единиц доступа.
	fps := measuredFPS
	if s.VUI != nil && s.VUI.TimingInfo != nil {
		// timing содержит параметры тактов POC; numerator/denominator задают точную кадровую частоту.
		timing := s.VUI.TimingInfo
		numerator, denominator := uint64(timing.TimeScale), uint64(timing.NumUnitsInTick)
		if timing.POCProportionalToTimingFlag {
			denominator *= uint64(timing.NumTicksPOCDiffOneMinus1) + 1
		}
		if numerator == 0 || denominator == 0 {
			return core.VideoFormat{}, ErrCodec
		}
		// divisor сокращает рациональную кадровую частоту до проверки uint32.
		divisor := gcd(numerator, denominator)
		numerator /= divisor
		denominator /= divisor
		if numerator > math.MaxUint32 || denominator > math.MaxUint32 {
			return core.VideoFormat{}, ErrTiming
		}
		fps = core.FrameRate{Numerator: uint32(numerator), Denominator: uint32(denominator)}
		if measuredFPS.Validate() == nil && !fps.Equal(measuredFPS) {
			return core.VideoFormat{}, ErrCodec
		}
	}
	// color сохраняет известный диапазон сигнала и неопределённые CICP при отсутствии colour_description.
	color := core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}
	if s.VUI != nil && s.VUI.VideoSignalTypePresentFlag {
		if s.VUI.VideoFullRangeFlag {
			color.Range = "full"
		}
		if s.VUI.ColourDescriptionPresentFlag {
			color.Primaries = primaryName(s.VUI.ColourPrimaries, "iso-")
			color.Transfer = transferName(s.VUI.TransferCharacteristics)
			color.Matrix = matrixName(s.VUI.MatrixCoefficients)
		}
	}
	// profile,pixel и tier отражают наблюдаемые поля, а не желаемую настройку.
	profile, pixel, tier := "main", "yuv420p", "main"
	if s.ProfileTierLevel.GeneralProfileIdc == 2 {
		profile = "main10"
	}
	if s.BitDepthLumaMinus8 == 2 {
		pixel = "yuv420p10le"
	}
	if s.ProfileTierLevel.GeneralTierFlag != 0 {
		tier = "high"
	}
	// level содержит фактический level_idc в принятой десятичной форме.
	level := fmt.Sprintf("%d.%d", s.ProfileTierLevel.GeneralLevelIdc/30, (s.ProfileTierLevel.GeneralLevelIdc%30)/3)
	if s.ProfileTierLevel.GeneralLevelIdc == 255 {
		level = "8.5"
	}
	// actual объединяет проверенные ограничения сжатого потока и известные измерения времени.
	actual := core.VideoFormat{Codec: core.CodecHEVC, CodecProfile: profile, Width: uint32(width), Height: uint32(height), FrameRate: fps, Level: level, Tier: tier, PixelFormat: pixel, BitDepth: uint8(s.BitDepthLumaMinus8 + 8), Color: color}
	if fps.Validate() != nil {
		return actual, ErrUnready
	}
	return actual, nil
}

// InspectHEVCAU проверяет обрамление NAL, принадлежность PPS/SPS и единственную картинку.
// Независимым стартом являются только IDR_W_RADL/IDR_N_LP; CRA/BLA не получают такой гарантии.
func InspectHEVCAU(desc VideoDescription, payload []byte) (bool, error) {
	// description содержит полные наборы параметров для независимой проверки одного AU.
	// err сообщает об отказе восстановления контекста из заголовков и разрешённых наборов параметров AU.
	description, err := hevcContext(desc.Headers, payload)
	if err != nil {
		return false, err
	}
	return description.Inspect(payload)
}

// Inspect проверяет картинку, сохраняя разрешённые дополнительные PPS неполного hev1 массива.
// PPS обновляются только при успешном завершении AU; полный массив запрещает новые записи.
func (description *HEVCDescription) Inspect(payload []byte) (bool, error) {
	// d копирует значения и таблицу PPS до успешной проверки всего AU.
	d := *description
	d.pps = make(map[uint32]h265.PPS, len(description.pps))
	// id и p переносят уже проверенные PPS без изменения состояния вызывающего при отказе.
	for id, p := range description.pps {
		d.pps[id] = p
	}

	// nalus содержит исходные NAL единицы доступа.
	// err сообщает о нарушении length framing единицы доступа.
	nalus, err := splitLengthNALUs(payload, 4)
	if err != nil {
		return false, err
	}
	// found отмечает первую картинку VCL; key сохраняет независимость её первого среза.
	found, key := false, false
	// pictureType требует одинаковый тип VCL для срезов одной картинки.
	pictureType := byte(255)
	// nal обозначает очередной исходный NAL.
	for _, nal := range nalus {
		if !hevcNAL(nal) {
			return false, ErrCodec
		}
		// typ содержит шестибитный nal_unit_type.
		typ := (nal[0] >> 1) & 63
		if typ >= 32 && typ <= 34 {
			// matched отмечает точный исходный набор параметров этой конфигурации.
			matched := false
			// expected обозначает набор параметров записи hvcC для сравнения без перезаписи.
			for _, expected := range d.NALUs {
				if bytes.Equal(nal, expected) {
					matched = true
					break
				}
			}
			if found {
				return false, ErrCodec
			}
			if !matched {
				if typ != 34 || d.completeArrays[34] || len(d.pps) >= 64 {
					return false, ErrCodec
				}
				// p содержит дополнительный PPS неполного массива; ссылка ограничена текущим SPS.
				var p h265.PPS
				// parseErr сохраняет отказ извлечения дополнительного PPS.
				if parseErr := p.Unmarshal(nal); parseErr != nil || p.ID > 63 || p.SPSID != uint32(d.SPS.ID) || !validHEVCPPS(nal, &d.SPS.BitDepthLumaMinus8) {
					return false, ErrCodec
				}
				// exists отмечает уже объявленный идентификатор PPS, который нельзя молча заменить.
				if _, exists := d.pps[p.ID]; exists {
					return false, ErrCodec
				}
				d.pps[p.ID] = p
				d.NALUs = append(d.NALUs, bytes.Clone(nal))
			}
			continue
		}
		if typ > 31 {
			if typ != 35 && typ != 38 && typ != 39 && typ != 40 {
				return false, ErrUnsupported
			}
			continue
		}
		if typ >= 10 && typ <= 15 || typ >= 22 {
			return false, ErrUnsupported
		}
		// r читает slice_segment_header после двухбайтового заголовка и удаления защитных байтов.
		r := bitReader{b: h264.EmulationPreventionRemove(nal[2:])}
		// first отличает первый срез картинки; IDR no_output_of_prior_pics_flag не создаёт зависимость.
		first := r.read(1) != 0
		if typ >= 16 && typ <= 23 {
			r.read(1)
		}
		// id ссылается на PPS данной картинки.
		id := r.ue()
		// p и exists обозначают проверенный PPS и наличие его идентификатора.
		p, exists := d.pps[id]
		if !exists || r.err != nil {
			return false, ErrCodec
		}
		if first {
			if found {
				return false, ErrUnsupported
			}
			found = true
			pictureType = typ
			key = typ == 19 || typ == 20
		} else if !found || typ != pictureType {
			return false, ErrCodec
		}
		if first {
			r.read(int(p.NumExtraSliceHeaderBits))
			// sliceType различает B/P/I и должен быть I у настоящего IDR.
			sliceType := r.ue()
			if sliceType > 2 || key && sliceType != 2 {
				return false, ErrCodec
			}
			if p.OutputFlagPresentFlag && r.read(1) == 0 {
				return false, ErrUnsupported
			}
		}
		if r.err != nil || r.p >= len(r.b)*8 {
			return false, ErrCodec
		}
	}
	if !found {
		return false, ErrCodec
	}
	*description = d
	return key, nil
}

// Format восстанавливает фактический формат из SPS и измеренного FPS при отсутствующих временных параметрах VUI.
// measuredFPS допускается только из фактической длительности единицы доступа либо ранее проверенного формата.
func (d HEVCDescription) Format(desc VideoDescription, measuredFPS core.FrameRate) (core.VideoFormat, error) {
	return d.videoFormat(desc, measuredFPS)
}

// ValidateHVC1AU запрещает VPS/SPS/PPS внутри единиц доступа записи ISO hvc1.
// ERTMP FourCC hvc1 здесь не проверяется: там действует отдельная привязка транспорта.
func ValidateHVC1AU(payload []byte) error {
	// nalus содержит исходные NAL с четырёхбайтовыми длинами; err сообщает о нарушении обрамления.
	nalus, err := splitLengthNALUs(payload, 4)
	if err != nil {
		return err
	}
	// nal обозначает очередной NAL единицы доступа ISO.
	for _, nal := range nalus {
		if !hevcNAL(nal) {
			return ErrCodec
		}
		if (nal[0]>>1)&63 >= 32 && (nal[0]>>1)&63 <= 34 {
			return ErrCodec
		}
	}
	return nil
}

// CompleteHEVCConfig дополняет неполные массивы hvcC точными наборами параметров первого AU.
// Сводные поля hvcC не переписываются: после дополнения они обязаны совпасть с реальным SPS.
// Результат предназначен только для временного контекста разбора; транспорт сохраняет исходные headers.
// Функция не меняет AU и не исправляет противоречащие заголовки произвольного входного потока.
func CompleteHEVCConfig(headers, payload []byte) ([]byte, error) {
	// description и err отличают отсутствующие наборы параметров от повреждённых существующих массивов.
	description, err := ParseHEVCConfig(headers)
	if err == nil {
		return bytes.Clone(headers), nil
	}
	if err != ErrUnready {
		return nil, err
	}
	// nalus содержат наборы параметров первого AU в исходном порядке.
	nalus, err := splitLengthNALUs(payload, 4)
	if err != nil {
		return nil, err
	}
	// arrays группирует только исходные NAL по типам, не изменяя содержимое NAL.
	var arrays [64][][]byte
	// nal обозначает существующий NAL конфигурации.
	for _, nal := range description.NALUs {
		arrays[(nal[0]>>1)&63] = append(arrays[(nal[0]>>1)&63], nal)
	}
	// nal обозначает NAL первого независимого AU, из которого допускаются недостающие PS.
	for _, nal := range nalus {
		if !hevcNAL(nal) {
			return nil, ErrCodec
		}
		// typ выбирает только VPS/SPS/PPS первого AU.
		typ := (nal[0] >> 1) & 63
		if typ < 32 || typ > 34 {
			continue
		}
		// found отмечает точный повтор уже объявленного набора параметров.
		found := false
		// existing обозначает набор параметров того же типа для побайтового сравнения.
		for _, existing := range arrays[typ] {
			if bytes.Equal(existing, nal) {
				found = true
				break
			}
		}
		if !found {
			if description.completeArrays[typ] {
				return nil, ErrCodec
			}
			arrays[typ] = append(arrays[typ], nal)
		}
	}
	// result сохраняет исходную сводку hvcC и пересобирает только обрамление массивов.
	result := bytes.Clone(headers[:23])
	result[22] = 0
	// typ и entries выбирают непустой массив NAL в нормативном шестибитном пространстве типов.
	for typ, entries := range arrays {
		if len(entries) == 0 {
			continue
		}
		if len(entries) > 4096 {
			return nil, ErrLimit
		}
		result[22]++
		// flag сохраняет признак полноты исходного массива; добавления не делают массив полным.
		flag := byte(typ)
		if description.completeArrays[typ] {
			flag |= 128
		}
		result = append(result, flag, byte(len(entries)>>8), byte(len(entries)))
		// nal обозначает исходный набор параметров с 16-битным размером.
		for _, nal := range entries {
			if len(nal) > 65535 {
				return nil, ErrLimit
			}
			result = append(result, byte(len(nal)>>8), byte(len(nal)))
			result = append(result, nal...)
		}
	}
	if _, err = ParseHEVCConfig(result); err != nil {
		return nil, err
	}
	return result, nil
}

// hevcContext восстанавливает только временный контекст разбора по первому AU неполного hvcC.
// Вызывающий сохраняет исходные заголовки транспорта; выведенные массивы никогда не подменяют CodecConfig.
func hevcContext(headers, payload []byte) (HEVCDescription, error) {
	// description и err содержат уже полную конфигурацию либо недостаточные наборы параметров.
	description, err := ParseHEVCConfig(headers)
	if !errors.Is(err, ErrUnready) {
		return description, err
	}
	// derived содержит временные массивы, дополненные точными PS первого AU.
	derived, err := CompleteHEVCConfig(headers, payload)
	if err != nil {
		return description, err
	}
	return ParseHEVCConfig(derived)
}

// ValidateHEVCConfig проверяет полный SPS либо доступную сводку неполного hvcC против declared.
// При неполном hvcC геометрия, цвет и FPS подтверждаются первым AU отдельного Inspector с состоянием;
// отсутствие этих полей в записи не превращается в значения по умолчанию.
func ValidateHEVCConfig(headers []byte, declared core.VideoFormat) error {
	return validateHEVCConfig(headers, declared, media.ColorConfigProvenance{})
}

// validateHEVCConfig проверяет исходные заголовки против declared с отдельным доказательством внешних CICP.
// provenance разрешает только уточнение действительно unspecified осей; остальные ограничения неизменны.
func validateHEVCConfig(headers []byte, declared core.VideoFormat, provenance media.ColorConfigProvenance) error {
	// description и err содержат SPS, если массивы полны.
	description, err := ParseHEVCConfig(headers)
	if err == nil {
		// actual извлекает ограничения сжатого потока и использует ранее измеренный FPS только без VUI.
		// formatErr сообщает о несовпадении SPS и объявленной геометрии либо частоты.
		actual, formatErr := description.videoFormat(VideoDescription{Headers: headers, Width: declared.Width, Height: declared.Height}, declared.FrameRate)
		if formatErr != nil {
			return formatErr
		}
		return CompatibleVideoConstraints(actual, declared, provenance)
	}
	if !errors.Is(err, ErrUnready) {
		return err
	}
	// profile и depth содержат доступные сводные ограничения исходной записи.
	profile, depth := headers[1]&31, uint8(headers[17]&7)+8
	if headers[1]>>6 != 0 || profile != 1 && profile != 2 || headers[16]&3 != 1 || depth != 8 && depth != 10 || uint8(headers[18]&7)+8 != depth {
		return ErrUnsupported
	}
	if declared.Codec != core.CodecHEVC || declared.BitDepth != depth || profile == 1 && declared.CodecProfile != "main" || profile == 2 && declared.CodecProfile != "main10" {
		return ErrCodec
	}
	if depth == 8 && declared.PixelFormat != "yuv420p" || depth == 10 && declared.PixelFormat != "yuv420p10le" {
		return ErrCodec
	}
	if headers[1]&32 != 0 && declared.Tier != "high" || headers[1]&32 == 0 && declared.Tier != "main" {
		return ErrCodec
	}
	return nil
}
