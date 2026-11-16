package codec

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// be читает длины NAL из avcC в порядке байтов от старшего к младшему.
var be = binary.BigEndian

// VideoDescription связывает заголовки видеокодека с размерами, заявленными контейнером.
type VideoDescription struct {
	// Headers содержит исходную запись конфигурации кодека, для H.264 — avcC.
	Headers []byte
	// Width задаёт заявленную контейнером ширину видео в пикселях. Height задаёт заявленную высоту в
	// пикселях.
	Width, Height uint32
}

// H264SPS извлекает единственный SPS из avcC с четырёхбайтовыми длинами NAL и сверяет вычисленные
// размеры с полями записи дорожки MP4. Усечённые записи отклоняются; неподдержанные параметры SPS
// возвращают ErrUnready.
func H264SPS(desc VideoDescription) (h264.SPS, error) {
	// b содержит непрочитанную часть avcC при проверке SPS и записей PPS.
	b := desc.Headers
	if len(b) < 8 || b[0] != 1 || b[4] != 0xff || b[5] != 0xe1 {
		return h264.SPS{}, fmt.Errorf("%w: поддерживается один SPS и четырёхбайтовые длины NAL", ErrCodec)
	}
	// n содержит длину единственного SPS в байтах.
	n := int(be.Uint16(b[6:8]))
	if len(b) < 8+n+1 || n < 4 {
		return h264.SPS{}, ErrCodec
	}
	// spsBytes ссылается на исходные байты единственного SPS внутри avcC.
	spsBytes := b[8 : 8+n]
	if spsBytes[0]&0x9f != 7 || !bytes.Equal(b[1:4], spsBytes[1:4]) || spsBytes[2]&3 != 0 {
		return h264.SPS{}, fmt.Errorf("%w: avcC расходится с SPS", ErrCodec)
	}
	b = b[8+n:]
	// ppsCount задаёт число следующих записей PPS в avcC.
	ppsCount := int(b[0])
	b = b[1:]
	if ppsCount == 0 {
		return h264.SPS{}, ErrCodec
	}
	for range ppsCount {
		if len(b) < 2 {
			return h264.SPS{}, ErrCodec
		}
		// n содержит длину очередного PPS в байтах.
		n := int(be.Uint16(b[:2]))
		if n == 0 || len(b) < n+2 {
			return h264.SPS{}, ErrCodec
		}
		b = b[n+2:]
	}
	// s принимает разобранные параметры SPS для проверки поддерживаемого формата и размеров.
	var s h264.SPS
	// err сообщает об отказе разбора SPS.
	if err := s.Unmarshal(spsBytes); err != nil {
		return h264.SPS{}, fmt.Errorf("%w: SPS: %v", ErrCodec, err)
	}
	if !validSPSRaw(spsBytes) {
		return h264.SPS{}, fmt.Errorf("%w: неполный либо некорректный SPS", ErrCodec)
	}
	if s.ID > 31 || !s.FrameMbsOnlyFlag || s.SeparateColourPlaneFlag || s.ChromaFormatIdc > 3 || s.BitDepthLumaMinus8 > 8 || s.BitDepthLumaMinus8 != s.BitDepthChromaMinus8 {
		return h264.SPS{}, fmt.Errorf("%w: неподдержанный H.264 SPS", ErrUnready)
	}
	// err сообщает о повреждённых PPS либо ссылках, расходящихся с единственным SPS.
	if err := validateAVCCPPS(desc.Headers[9+n:], ppsCount, s); err != nil {
		return h264.SPS{}, err
	}
	// err сообщает о неполном либо противоречивом расширении avcC, если оно присутствует.
	if err := validateAVCCExtension(b, s); err != nil {
		return h264.SPS{}, err
	}
	// width содержит вычисленную из макроблоков ширину кадра в пикселях до применения обрезки.
	width := (uint64(s.PicWidthInMbsMinus1) + 1) * 16
	// height содержит вычисленную из единиц карты высоту прогрессивного кадра в пикселях до
	// применения обрезки.
	height := (uint64(s.PicHeightInMapUnitsMinus1) + 1) * 16
	if s.FrameCropping != nil {
		// sx задаёт горизонтальный размер единицы обрезки в пикселях. sy задаёт вертикальный
		// размер единицы обрезки в пикселях.
		sx, sy := uint64(1), uint64(1)
		switch s.ChromaFormatIdc {
		case 1:
			sx, sy = 2, 2
		case 2:
			sx = 2
		}
		// cx содержит суммарную ширину обрезки слева и справа в пикселях.
		cx := (uint64(s.FrameCropping.LeftOffset) + uint64(s.FrameCropping.RightOffset)) * sx
		// cy содержит суммарную высоту обрезки сверху и снизу в пикселях.
		cy := (uint64(s.FrameCropping.TopOffset) + uint64(s.FrameCropping.BottomOffset)) * sy
		if cx >= width || cy >= height {
			return h264.SPS{}, ErrCodec
		}
		width -= cx
		height -= cy
	}
	if width == 0 || height == 0 || width > math.MaxUint32 || height > math.MaxUint32 || uint32(width) != desc.Width || uint32(height) != desc.Height {
		return h264.SPS{}, fmt.Errorf("%w: размеры sample entry не совпадают с SPS", ErrCodec)
	}
	return s, nil
}

// validateAVCCPPS проверяет count полных PPS в records против SPS s.
// Бюджет задаётся однобайтовым счётчиком avcC, идентификаторы не могут повторяться.
func validateAVCCPPS(records []byte, count int, s h264.SPS) error {
	// seen ограничивает множество уже прочитанных PPS нормативными идентификаторами 0..255.
	var seen [256]bool
	for range count {
		if len(records) < 2 {
			return ErrCodec
		}
		// n задаёт размер следующей PPS NAL в байтах.
		n := int(be.Uint16(records[:2]))
		if n < 2 || len(records) < 2+n {
			return ErrCodec
		}
		// raw ссылается на исходную PPS без двухбайтового поля длины.
		raw := records[2 : 2+n]
		records = records[2+n:]
		if raw[0]&0x9f != 8 {
			return ErrCodec
		}
		// r читает поля PPS после удаления защитных байтов NAL.
		r := bitReader{b: h264.EmulationPreventionRemove(raw[1:])}
		// id задаёт идентификатор PPS, parent содержит ссылку на SPS.
		id, parent := r.ue(), r.ue()
		if r.err != nil || id > 255 || parent != s.ID || seen[id] || !validPPS(&r, s.ProfileIdc, s.BitDepthLumaMinus8) {
			return ErrCodec
		}
		seen[id] = true
	}
	return nil
}

// H264Format получает формат из проверенного SPS, включая сокращённую частоту из VUI.TimingInfo,
// глубину и цвет. Отсутствующая частота возвращает ErrUnready, непредставимая дробь — ErrTiming.
func H264Format(desc VideoDescription) (core.VideoFormat, error) {
	// s содержит проверенный SPS. err сообщает о повреждении, неподдержанных параметрах или
	// несовпадении размеров.
	s, err := H264SPS(desc)
	if err != nil {
		return core.VideoFormat{}, err
	}
	if s.VUI == nil || s.VUI.TimingInfo == nil || s.VUI.TimingInfo.TimeScale == 0 || s.VUI.TimingInfo.NumUnitsInTick == 0 {
		return core.VideoFormat{}, fmt.Errorf("%w: нет H.264 VUI timing", ErrUnready)
	}
	// numerator содержит числитель частоты кадров из VUI до и после сокращения дроби.
	numerator := uint64(s.VUI.TimingInfo.TimeScale)
	// denominator содержит знаменатель частоты кадров с учётом двух тактов на кадр.
	denominator := 2 * uint64(s.VUI.TimingInfo.NumUnitsInTick)
	// g содержит общий делитель для точного сокращения частоты кадров.
	g := gcd(numerator, denominator)
	numerator /= g
	denominator /= g
	if denominator > math.MaxUint32 {
		return core.VideoFormat{}, ErrTiming
	}
	// profile содержит имя профиля H.264 либо строковое представление неизвестного ProfileIdc.
	profile := map[uint8]string{66: "baseline", 77: "main", 88: "extended", 100: "high", 110: "high10", 122: "high422", 244: "high444"}[s.ProfileIdc]
	if profile == "" {
		profile = "h264-profile-idc-" + strconv.Itoa(int(s.ProfileIdc))
	}
	if s.ProfileIdc == 66 && s.ConstraintSet1Flag {
		profile = "constrained-baseline"
	}
	// pixel содержит имя формата пикселей, выведенное из субдискретизации и глубины.
	pixel := map[uint32]string{0: "gray", 1: "yuv420p", 2: "yuv422p", 3: "yuv444p"}[s.ChromaFormatIdc]
	// depth содержит глубину компоненты яркости в битах.
	depth := uint8(s.BitDepthLumaMinus8 + 8)
	if depth > 8 {
		pixel += strconv.Itoa(int(depth)) + "le"
	}
	// color накапливает диапазон и характеристики цвета, подтверждённые VUI.
	color := core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}
	if s.VUI.VideoSignalTypePresentFlag {
		if s.VUI.VideoFullRangeFlag {
			color.Range = "full"
		}
		if s.VUI.ColourDescriptionPresentFlag {
			color.Primaries = h264ColorName(s.VUI.ColourPrimaries)
			color.Transfer = transferName(s.VUI.TransferCharacteristics)
			color.Matrix = matrixName(s.VUI.MatrixCoefficients)
		}
	}
	return core.VideoFormat{Codec: core.CodecH264, Width: desc.Width, Height: desc.Height, FrameRate: core.FrameRate{Numerator: uint32(numerator), Denominator: uint32(denominator)}, CodecProfile: profile, Level: fmt.Sprintf("%d.%d", s.LevelIdc/10, s.LevelIdc%10), PixelFormat: pixel, BitDepth: depth, Color: color}, nil
}

// Преобразует известные коды основных цветов H.264 в имена; неизвестный код сохраняет как iso-<номер>.
func h264ColorName(v uint8) string {
	return primaryName(v, "iso-")
}

// Преобразует код передаточной характеристики H.264 в имя, сохраняя неизвестные значения как
// iso-<номер>.
func transferName(v uint8) string {
	return cicpTransferName(v, "iso-")
}

// Преобразует код цветовой матрицы H.264 в имя, сохраняя неизвестные значения как iso-<номер>.
func matrixName(v uint8) string {
	return cicpMatrixName(v, "iso-")
}

// validateAVCCExtension сверяет необязательные поля chroma/depth с разобранным SPS s.
// Пустой tail сохраняет допустимость прежних avcC без расширения; присутствующий хвост должен быть
// полным. Дополнительные SPS расширения не входят в поддержанный прогрессивный видеотракт.
func validateAVCCExtension(tail []byte, s h264.SPS) error {
	if len(tail) == 0 {
		return nil
	}
	switch s.ProfileIdc {
	case 100, 110, 122, 144, 244:
	default:
		return ErrCodec
	}
	if len(tail) != 4 || tail[0]&0xfc != 0xfc || tail[1]&0xf8 != 0xf8 || tail[2]&0xf8 != 0xf8 || tail[3] != 0 || uint32(tail[0]&3) != s.ChromaFormatIdc || uint32(tail[1]&7) != s.BitDepthLumaMinus8 || uint32(tail[2]&7) != s.BitDepthChromaMinus8 {
		return fmt.Errorf("%w: расширение avcC расходится с SPS", ErrCodec)
	}
	return nil
}

// Читает беззнаковое число Exp-Golomb; усечение или более 31 ведущего нуля сохраняют ErrCodec.
func (r *bitReader) ue() uint32 {
	// zeros считает ведущие нулевые биты текущего кода Exp-Golomb и ограничивает их число.
	zeros := 0
	for r.err == nil && r.read(1) == 0 {
		zeros++
		if zeros > 31 {
			r.err = ErrCodec
			return 0
		}
	}
	if r.err != nil {
		return 0
	}
	return (uint32(1) << uint(zeros)) - 1 + r.read(zeros)
}

// InspectH264AU проверяет принадлежность срезов PPS/SPS и действительный независимый IDR.
func InspectH264AU(desc VideoDescription, payload []byte) (bool, error) {
	// s содержит проверенный SPS, с которым должны согласовываться PPS и срезы. err сообщает об
	// отказе проверки SPS.
	s, err := H264SPS(desc)
	if err != nil {
		return false, err
	}
	// b содержит непрочитанную часть avcC при извлечении PPS.
	b := desc.Headers
	// n содержит длину SPS, затем используется для длины очередного PPS в байтах.
	n := int(be.Uint16(b[6:8]))
	b = b[8+n:]
	// count задаёт число PPS, перечисленных в avcC.
	count := int(b[0])
	b = b[1:]
	// pps отмечает уже проверенные идентификаторы PPS для проверки ссылок срезов и дубликатов.
	pps := make(map[uint32]bool, count)
	// parameterSets сохраняет исходные SPS и PPS по типам NAL для точного сравнения с параметрами
	// внутри AU.
	parameterSets := make(map[byte][][]byte)
	parameterSets[7] = [][]byte{desc.Headers[8 : 8+n]}
	for range count {
		n = int(be.Uint16(b[:2]))
		// raw ссылается на байты очередного PPS из avcC.
		raw := b[2 : 2+n]
		b = b[2+n:]
		if len(raw) < 2 || raw[0]&0x9f != 8 {
			return false, ErrCodec
		}
		// r читает PPS после удаления байтов предотвращения эмуляции стартового кода.
		r := bitReader{b: h264.EmulationPreventionRemove(raw[1:])}
		// id содержит идентификатор PPS. parent содержит идентификатор SPS, на который ссылается
		// этот PPS.
		id, parent := r.ue(), r.ue()
		if r.err != nil || id > 255 || parent != s.ID || !validPPS(&r, s.ProfileIdc, s.BitDepthLumaMinus8) {
			return false, ErrCodec
		}
		if pps[id] {
			return false, ErrCodec
		}
		pps[id] = true
		parameterSets[8] = append(parameterSets[8], raw)
	}
	// au принимает список NAL единицы доступа из формата AVCC.
	var au h264.AVCC
	if err = au.Unmarshal(payload); err != nil {
		return false, fmt.Errorf("%w: %v", ErrCodec, err)
	}
	// found показывает, встречен ли первый срез кадра. key сохраняет, относится ли этот кадр к IDR.
	found, key := false, false
	// nal обозначает очередную NAL проверяемой единицы доступа.
	for _, nal := range au {
		if len(nal) < 1 || nal[0]&0x80 != 0 {
			return false, ErrCodec
		}
		// typ содержит тип очередной NAL, определяющий дальнейшую проверку.
		typ := nal[0] & 31
		switch typ {
		case 1, 5:
			// r читает заголовок среза после удаления байтов предотвращения эмуляции
			// стартового кода.
			r := bitReader{b: h264.EmulationPreventionRemove(nal[1:])}
			// first содержит адрес первого макроблока среза. slice содержит тип среза. id
			// содержит идентификатор используемого PPS.
			first, slice, id := r.ue(), r.ue(), r.ue()
			if r.err != nil || slice > 9 || !pps[id] {
				return false, ErrCodec
			}
			r.read(int(s.Log2MaxFrameNumMinus4) + 4)
			if typ == 5 {
				if nal[0]&0x60 == 0 || slice%5 != 2 && slice%5 != 4 {
					return false, ErrCodec
				}
				r.ue()
			}
			if r.err != nil {
				return false, r.err
			}
			if !found {
				if first != 0 {
					return false, ErrCodec
				}
				key = typ == 5
			} else if key != (typ == 5) {
				return false, ErrCodec
			}
			found = true
		case 6, 9, 12:
		case 7, 8:
			// matching показывает, совпала ли NAL параметров внутри AU с одним из заголовков
			// конфигурации.
			matching := false
			// known содержит исходную SPS или PPS того же типа для точного сравнения байтов.
			for _, known := range parameterSets[typ] {
				if bytes.Equal(known, nal) {
					matching = true
					break
				}
			}
			if !matching {
				return false, fmt.Errorf("%w: смена параметров внутри AU", ErrCodec)
			}
		default:
			return false, ErrCodec
		}
	}
	if !found {
		return false, ErrCodec
	}
	return key, nil
}

// validPPS проверяет поля PPS читателем r после идентификаторов: ссылки, квантование и расширения
// без матриц масштабирования, а также завершающие биты RBSP. profile задаёт ограничения baseline,
// depthMinus8 расширяет нижнюю границу начального QP для фактической глубины SPS.
func validPPS(r *bitReader, profile uint8, depthMinus8 uint32) bool {
	// entropy содержит признак режима энтропийного кодирования PPS для проверки ограничения
	// baseline.
	entropy := r.read(1)
	r.read(1)
	if profile == 66 && entropy != 0 {
		return false
	}
	if r.ue() != 0 {
		return false
	}
	// l0 содержит заявленное число ссылок списка 0 за вычетом единицы. l1 содержит такое число для
	// списка 1.
	l0, l1 := r.ue(), r.ue()
	if l0 > 31 || l1 > 31 {
		return false
	}
	r.read(1)
	if r.read(2) > 2 {
		return false
	}
	// qp содержит начальное смещение квантования PPS. qs содержит начальное смещение квантования
	// переключаемых срезов. chroma содержит смещение квантования цветовых компонент.
	qp, qs, chroma := r.se(), r.se(), r.se()
	if qp < -26-6*int64(depthMinus8) || qp > 25 || qs < -26 || qs > 25 || chroma < -12 || chroma > 12 {
		return false
	}
	r.read(3)
	if validRBSPTail(*r) {
		return r.err == nil
	}
	r.read(1)
	if r.read(1) != 0 {
		return false
	}
	// v содержит второе смещение квантования цветовых компонент из расширения PPS.
	if v := r.se(); v < -12 || v > 12 {
		return false
	}
	return r.err == nil && validRBSPTail(*r)
}

// Проверяет завершающий единичный бит RBSP и не более семи нулевых битов заполнения, не меняя исходный
// читатель.
func validRBSPTail(r bitReader) bool {
	if len(r.b)*8-r.p > 8 || r.read(1) != 1 {
		return false
	}
	for r.err == nil && r.p < len(r.b)*8 {
		if r.read(1) != 0 {
			return false
		}
	}
	return r.err == nil
}

// Преобразует беззнаковый код Exp-Golomb в знаковое значение с чередованием положительных и
// отрицательных чисел.
func (r *bitReader) se() int64 {
	// v содержит беззнаковый код Exp-Golomb для преобразования в знаковое значение.
	v := r.ue()
	if v%2 == 0 {
		return -int64(v / 2)
	}
	return int64(v+1) / 2
}
