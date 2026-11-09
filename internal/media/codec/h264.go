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

var be = binary.BigEndian

type VideoDescription struct {
	Headers       []byte
	Width, Height uint32
}

func H264SPS(desc VideoDescription) (h264.SPS, error) {
	b := desc.Headers
	if len(b) < 8 || b[0] != 1 || b[4]&3 != 3 || b[5]&31 != 1 {
		return h264.SPS{}, fmt.Errorf("%w: поддерживается один SPS и четырёхбайтовые NAL lengths", ErrCodec)
	}
	n := int(be.Uint16(b[6:8]))
	if len(b) < 8+n+1 || n == 0 {
		return h264.SPS{}, ErrCodec
	}
	spsBytes := b[8 : 8+n]
	b = b[8+n:]
	ppsCount := int(b[0])
	b = b[1:]
	if ppsCount == 0 {
		return h264.SPS{}, ErrCodec
	}
	for range ppsCount {
		if len(b) < 2 {
			return h264.SPS{}, ErrCodec
		}
		n := int(be.Uint16(b[:2]))
		if n == 0 || len(b) < n+2 {
			return h264.SPS{}, ErrCodec
		}
		b = b[n+2:]
	}
	var s h264.SPS
	if err := s.Unmarshal(spsBytes); err != nil {
		return h264.SPS{}, fmt.Errorf("%w: SPS: %v", ErrCodec, err)
	}
	if s.ID > 31 || !s.FrameMbsOnlyFlag || s.SeparateColourPlaneFlag || s.ChromaFormatIdc > 3 || s.BitDepthLumaMinus8 > 8 || s.BitDepthLumaMinus8 != s.BitDepthChromaMinus8 {
		return h264.SPS{}, fmt.Errorf("%w: неподдержанный H.264 SPS", ErrUnready)
	}
	width := (uint64(s.PicWidthInMbsMinus1) + 1) * 16
	height := (uint64(s.PicHeightInMapUnitsMinus1) + 1) * 16
	if s.FrameCropping != nil {
		sx, sy := uint64(1), uint64(1)
		switch s.ChromaFormatIdc {
		case 1:
			sx, sy = 2, 2
		case 2:
			sx = 2
		}
		cx := (uint64(s.FrameCropping.LeftOffset) + uint64(s.FrameCropping.RightOffset)) * sx
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

func H264Format(desc VideoDescription) (core.VideoFormat, error) {
	s, err := H264SPS(desc)
	if err != nil {
		return core.VideoFormat{}, err
	}
	if s.VUI == nil || s.VUI.TimingInfo == nil || s.VUI.TimingInfo.TimeScale == 0 || s.VUI.TimingInfo.NumUnitsInTick == 0 {
		return core.VideoFormat{}, fmt.Errorf("%w: нет H.264 VUI timing", ErrUnready)
	}
	numerator := uint64(s.VUI.TimingInfo.TimeScale)
	denominator := 2 * uint64(s.VUI.TimingInfo.NumUnitsInTick)
	g := gcd(numerator, denominator)
	numerator /= g
	denominator /= g
	if denominator > math.MaxUint32 {
		return core.VideoFormat{}, ErrTiming
	}
	profile := map[uint8]string{66: "baseline", 77: "main", 88: "extended", 100: "high", 110: "high10", 122: "high422", 244: "high444"}[s.ProfileIdc]
	if profile == "" {
		profile = "h264-profile-idc-" + strconv.Itoa(int(s.ProfileIdc))
	}
	if s.ProfileIdc == 66 && s.ConstraintSet1Flag {
		profile = "constrained-baseline"
	}
	pixel := map[uint32]string{0: "gray", 1: "yuv420p", 2: "yuv422p", 3: "yuv444p"}[s.ChromaFormatIdc]
	depth := uint8(s.BitDepthLumaMinus8 + 8)
	if depth > 8 {
		pixel += strconv.Itoa(int(depth)) + "le"
	}
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

func (r *bitReader) ue() uint32 {
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
	s, err := H264SPS(desc)
	if err != nil {
		return false, err
	}
	b := desc.Headers
	n := int(be.Uint16(b[6:8]))
	b = b[8+n:]
	count := int(b[0])
	b = b[1:]
	pps := make(map[uint32]bool, count)
	parameterSets := make(map[byte][][]byte)
	parameterSets[7] = [][]byte{desc.Headers[8 : 8+n]}
	for range count {
		n = int(be.Uint16(b[:2]))
		raw := b[2 : 2+n]
		b = b[2+n:]
		if len(raw) < 2 || raw[0]&0x9f != 8 {
			return false, ErrCodec
		}
		r := bitReader{b: h264.EmulationPreventionRemove(raw[1:])}
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
	var au h264.AVCC
	if err = au.Unmarshal(payload); err != nil {
		return false, fmt.Errorf("%w: %v", ErrCodec, err)
	}
	found, key := false, false
	for _, nal := range au {
		if len(nal) < 1 || nal[0]&0x80 != 0 {
			return false, ErrCodec
		}
		typ := nal[0] & 31
		switch typ {
		case 1, 5:
			r := bitReader{b: h264.EmulationPreventionRemove(nal[1:])}
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
			matching := false
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

func validPPS(r *bitReader, profile uint8) bool {
	entropy := r.read(1)
	r.read(1)
	if profile == 66 && entropy != 0 {
		return false
	}
	if r.ue() != 0 {
		return false
	}
	l0, l1 := r.ue(), r.ue()
	if l0 > 31 || l1 > 31 {
		return false
	}
	r.read(1)
	if r.read(2) > 2 {
		return false
	}
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
	if v := r.se(); v < -12 || v > 12 {
		return false
	}
	return r.err == nil && validRBSPTail(*r)
}
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

func (r *bitReader) se() int64 {
	v := r.ue()
	if v%2 == 0 {
		return -int64(v / 2)
	}
	return int64(v+1) / 2
}
