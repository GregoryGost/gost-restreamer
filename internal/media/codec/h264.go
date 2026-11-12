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
	if len(b) < 8 || b[0] != 1 || b[4]&3 != 3 || b[5]&31 != 1 {
		return h264.SPS{}, fmt.Errorf("%w: поддерживается один SPS и четырёхбайтовые NAL lengths", ErrCodec)
	}
	// n содержит длину единственного SPS в байтах.
	n := int(be.Uint16(b[6:8]))
	if len(b) < 8+n+1 || n == 0 {
		return h264.SPS{}, ErrCodec
	}
	// spsBytes ссылается на исходные байты единственного SPS внутри avcC.
	spsBytes := b[8 : 8+n]
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
	if s.ID > 31 || !s.FrameMbsOnlyFlag || s.SeparateColourPlaneFlag || s.ChromaFormatIdc > 3 || s.BitDepthLumaMinus8 > 8 || s.BitDepthLumaMinus8 != s.BitDepthChromaMinus8 {
		return h264.SPS{}, fmt.Errorf("%w: неподдержанный H.264 SPS", ErrUnready)
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
	switch v {
	case 1:
		return "bt709"
	case 2:
		return "unspecified"
	case 5:
		return "bt470bg"
	case 6:
		return "smpte170m"
	case 9:
		return "bt2020"
	}
	return "iso-" + strconv.Itoa(int(v))
}

// Преобразует код передаточной характеристики H.264 в имя, сохраняя неизвестные значения как
// iso-<номер>.
func transferName(v uint8) string {
	switch v {
	case 1:
		return "bt709"
	case 2:
		return "unspecified"
	case 6:
		return "smpte170m"
	case 8:
		return "linear"
	case 13:
		return "iec61966-2-1"
	case 14:
		return "bt2020-10"
	case 15:
		return "bt2020-12"
	case 16:
		return "smpte2084"
	case 18:
		return "arib-std-b67"
	}
	return "iso-" + strconv.Itoa(int(v))
}

// Преобразует код цветовой матрицы H.264 в имя, сохраняя неизвестные значения как iso-<номер>.
func matrixName(v uint8) string {
	switch v {
	case 0:
		return "gbr"
	case 1:
		return "bt709"
	case 2:
		return "unspecified"
	case 5:
		return "bt470bg"
	case 6:
		return "smpte170m"
	case 9:
		return "bt2020nc"
	case 10:
		return "bt2020c"
	}
	return "iso-" + strconv.Itoa(int(v))
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
		if r.err != nil || id > 255 || parent != s.ID || !validPPS(&r, s.ProfileIdc) {
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

// Проверяет поддержанные поля PPS после идентификаторов: ограничения ссылок, квантования и расширения
// без матриц масштабирования, а также завершающие биты RBSP.
func validPPS(r *bitReader, profile uint8) bool {
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
	if qp < -26 || qp > 25 || qs < -26 || qs > 25 || chroma < -12 || chroma > 12 {
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
