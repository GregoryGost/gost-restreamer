package rtmp

import (
	"bytes"
	"fmt"
	"math"
	"strconv"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/abema/go-mp4"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/av1"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
)

// AudioFormatASC получает наблюдаемый монофонический или стереофонический AAC-LC из полного ASC.
// PCE, активные расширения SBR/PS и нестандартная длина кадра в этот ограниченный порт не входят.
func AudioFormatASC(raw []byte) (core.AudioFormat, error) {
	if len(raw) < 2 || len(raw) > 64 {
		return core.AudioFormat{}, ErrCodec
	}
	var a mpeg4audio.AudioSpecificConfig
	if err := a.Unmarshal(raw); err != nil {
		return core.AudioFormat{}, fmt.Errorf("%w: ASC: %v", ErrCodec, err)
	}
	if a.Type != mpeg4audio.ObjectTypeAACLC || a.ExtensionType != 0 || a.FrameLengthFlag || a.DependsOnCoreCoder || a.SampleRate <= 0 || a.ChannelConfig < 1 || a.ChannelConfig > 2 {
		return core.AudioFormat{}, ErrCodec
	}
	canonical, err := a.Marshal()
	if err != nil {
		return core.AudioFormat{}, err
	}
	if !bytes.Equal(raw, canonical) && !hasDisabledSBRSyncExtension(raw, len(canonical)*8) {
		return core.AudioFormat{}, fmt.Errorf("%w: расширенный ASC", ErrCodec)
	}
	layout := "mono"
	if a.ChannelConfig == 2 {
		layout = "stereo"
	}
	return core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: uint32(a.SampleRate), Channels: uint16(a.ChannelConfig), ChannelLayout: layout}, nil
}

// hasDisabledSBRSyncExtension допускает только sync extension с выключенным SBR и нулевым padding.
func hasDisabledSBRSyncExtension(raw []byte, canonicalBaseBits int) bool {
	r := bitReader{b: raw}
	r.read(5) // audioObjectType
	if r.read(4) == 15 {
		r.read(24) // explicit samplingFrequency
	}
	r.read(4) // channelConfiguration
	r.read(1) // frameLengthFlag
	if r.read(1) != 0 {
		r.read(14) // coreCoderDelay
	}
	if r.read(1) != 0 || r.err != nil { // extensionFlag
		return false
	}
	if r.p != canonicalBaseBits {
		return false
	}
	if r.read(11) != 0x2b7 || r.read(5) != 5 || r.read(1) != 0 || r.err != nil {
		return false
	}
	padding := (8 - r.p%8) % 8
	if len(raw)*8-r.p != padding {
		return false
	}
	return r.read(padding) == 0 && r.err == nil
}

type obu struct {
	typ          byte
	payload, raw []byte
}

func splitOBUs(raw []byte) ([]obu, error) {
	var out []obu
	for len(raw) > 0 {
		if len(out) >= 4096 {
			return nil, ErrLimit
		}
		h := raw[0]
		if h&0x85 != 0 || h&2 == 0 {
			return nil, fmt.Errorf("%w: OBU header", ErrCodec)
		}
		var size uint64
		n := 1
		for {
			if n >= len(raw) || n > 8 {
				return nil, ErrCodec
			}
			v := raw[n]
			size |= uint64(v&127) << uint(7*(n-1))
			n++
			if v&128 == 0 {
				break
			}
		}
		if size > uint64(len(raw)-n) {
			return nil, ErrCodec
		}
		end := n + int(size)
		out = append(out, obu{typ: (h >> 3) & 15, payload: raw[n:end], raw: raw[:end]})
		raw = raw[end:]
	}
	return out, nil
}

type av1Description struct {
	seq           av1.SequenceHeader
	seqBytes      []byte
	width, height uint32
	fps           core.FrameRate
	provenance    string
}

func parseAV1Config(raw []byte) (av1Description, error) {
	if len(raw) < 4 {
		return av1Description{}, ErrCodec
	}
	var c mp4.Av1C
	if _, err := mp4.Unmarshal(bytes.NewReader(raw), uint64(len(raw)), &c, mp4.Context{}); err != nil {
		return av1Description{}, fmt.Errorf("%w: av1C: %v", ErrCodec, err)
	}
	if c.Marker != 1 || c.Version != 1 || c.Reserved != 0 || c.InitialPresentationDelayPresent != 0 {
		return av1Description{}, ErrCodec
	}
	obus, err := splitOBUs(c.ConfigOBUs)
	if err != nil {
		return av1Description{}, err
	}
	var d av1Description
	found := false
	for _, o := range obus {
		if o.typ != 1 {
			if o.typ != 5 && o.typ != 15 {
				return d, ErrCodec
			}
			continue
		}
		if found {
			return d, ErrCodec
		}
		found = true
		if err = d.seq.Unmarshal(o.raw); err != nil {
			return d, fmt.Errorf("%w: sequence: %v", ErrCodec, err)
		}
		d.seqBytes = bytes.Clone(o.payload)
	}
	if !found {
		return d, ErrCodec
	}
	if d.seq.SeqLevelIdx[0] > 23 && d.seq.SeqLevelIdx[0] != 31 {
		return d, fmt.Errorf("%w: зарезервированный level", ErrCodec)
	}
	s := &d.seq
	cc := s.ColorConfig
	if s.SeqProfile != 0 || s.ReducedStillPictureHeader || s.StillPicture || s.OperatingPointsCntMinus1 != 0 || s.OperatingPointIdc[0] != 0 || s.FrameIDNumbersPresentFlag || s.DecoderModelInfoPresentFlag || s.InitialDisplayDelayPresentFlag || s.EnableSuperRes || cc.BitDepth != 8 || cc.MonoChrome || !cc.SubsamplingX || !cc.SubsamplingY {
		return d, fmt.Errorf("%w: требуется single-layer Main SDR без superres/frame IDs/film grain", ErrCodec)
	}
	if err := d.validateSequenceTail(); err != nil {
		return d, err
	}
	if cc.TransferCharacteristics == 16 || cc.TransferCharacteristics == 18 {
		return d, fmt.Errorf("%w: HDR", ErrCodec)
	}
	if c.SeqProfile != s.SeqProfile || c.SeqLevelIdx0 != s.SeqLevelIdx[0] || c.SeqTier0 != boolByte(s.SeqTier[0]) || c.HighBitdepth != boolByte(cc.HighBitDepth) || c.TwelveBit != boolByte(cc.TwelveBit) || c.Monochrome != boolByte(cc.MonoChrome) || c.ChromaSubsamplingX != boolByte(cc.SubsamplingX) || c.ChromaSubsamplingY != boolByte(cc.SubsamplingY) || c.ChromaSamplePosition != uint8(cc.ChromaSamplePosition) {
		return d, fmt.Errorf("%w: av1C расходится с sequence", ErrCodec)
	}
	if t := s.TimingInfo; t != nil && t.EqualPictureInterval {
		den := uint64(t.NumUnitsInDisplayTick) * (uint64(t.NumTicksPerPictureMinus1) + 1)
		num := uint64(t.TimeScale)
		if num == 0 || den == 0 {
			return d, ErrCodec
		}
		g := gcd(num, den)
		num /= g
		den /= g
		if num > math.MaxUint32 || den > math.MaxUint32 {
			return d, ErrCodec
		}
		d.fps = core.FrameRate{Numerator: uint32(num), Denominator: uint32(den)}
		d.provenance = "av1.timing_info"
	}
	return d, nil
}

func boolByte(v bool) uint8 {
	if v {
		return 1
	}
	return 0
}
func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

type bitReader struct {
	b   []byte
	p   int
	err error
}

func (r *bitReader) read(n int) uint32 {
	if r.err != nil {
		return 0
	}
	if n < 0 || n > 32 || len(r.b)*8-r.p < n {
		r.err = ErrCodec
		return 0
	}
	var v uint32
	for range n {
		v = v<<1 | uint32(r.b[r.p/8]>>uint(7-r.p%8)&1)
		r.p++
	}
	return v
}

// validateSequenceTail проверяет полный хвост поддержанного заголовка последовательности.
// mediacommon v2.9.3 не читает separate_uv_delta_q в color_config и потому
// его FilmGrainParamsPresent нельзя использовать как доказательство этого флага.
func (d *av1Description) validateSequenceTail() error {
	s := &d.seq
	r := bitReader{b: d.seqBytes}
	r.read(5)
	if r.read(1) != 0 {
		r.read(32)
		r.read(32)
		if r.read(1) != 0 {
			zeros := 0
			for r.err == nil && r.read(1) == 0 {
				zeros++
				if zeros >= 32 {
					return ErrCodec
				}
			}
			r.read(zeros)
		}
		r.read(1)
	}
	r.read(1)
	r.read(5)
	r.read(12)
	level := r.read(5)
	if level > 7 {
		r.read(1)
	}
	w, h := int(r.read(4))+1, int(r.read(4))+1
	r.read(w)
	r.read(h)
	r.read(1)
	r.read(3)
	r.read(5)
	if s.EnableOrderHint {
		r.read(2)
	}
	r.read(1)
	if !s.SeqChooseScreenContentTools {
		r.read(1)
	}
	if s.SeqForceScreenContentTools > 0 {
		r.read(1)
		if !s.SeqChooseIntegerMv {
			r.read(1)
		}
	}
	if s.EnableOrderHint {
		r.read(3)
	}
	r.read(3)
	r.read(1)
	r.read(1)
	r.read(1)
	if s.ColorConfig.ColorDescriptionPresentFlag {
		r.read(24)
	}
	r.read(1)
	r.read(2)
	r.read(1)
	grain := r.read(1) != 0
	if r.read(1) != 1 {
		return fmt.Errorf("%w: sequence trailing bits", ErrCodec)
	}
	for r.err == nil && r.p < len(r.b)*8 {
		if r.read(1) != 0 {
			return ErrCodec
		}
	}
	if r.err != nil {
		return r.err
	}
	if grain {
		return fmt.Errorf("%w: film grain", ErrCodec)
	}
	s.FilmGrainParamsPresent = false
	return nil
}

// frame проверяет часть uncompressed_header, определяющую независимость и размеры.
// frame_size_override и EnableSuperRes не получают фиктивные размеры из максимальных размеров последовательности.
func (d *av1Description) frame(payload []byte) (key, shown bool, err error) {
	r := bitReader{b: payload}
	s := &d.seq
	if r.read(1) != 0 {
		r.read(3)
		return false, true, r.err
	}
	typ := r.read(2)
	shown = r.read(1) != 0
	if !shown {
		r.read(1)
	}
	if typ != 3 && (typ != 0 || !shown) {
		r.read(1)
	}
	r.read(1)
	allow := uint32(s.SeqForceScreenContentTools)
	if allow == 2 {
		allow = r.read(1)
	}
	if allow != 0 && s.SeqForceIntegerMv == 2 {
		r.read(1)
	}
	override := typ == 3
	if typ != 3 {
		override = r.read(1) != 0
	}
	if override {
		return false, shown, fmt.Errorf("%w: frame_size_override", ErrCodec)
	}
	if s.EnableOrderHint {
		r.read(int(s.OrderHintBitsMinus1) + 1)
	}
	if r.err != nil {
		return false, shown, r.err
	}
	if typ == 0 && shown {
		// Поля primary_ref_frame/refresh_frame_flags отсутствуют для показанного KEY_FRAME.
		if r.read(1) != 0 {
			return false, shown, fmt.Errorf("%w: render_size", ErrCodec)
		}
		if r.err != nil {
			return false, shown, r.err
		}
		d.width = s.MaxFrameWidthMinus1 + 1
		d.height = s.MaxFrameHeightMinus1 + 1
		return true, true, nil
	}
	return false, shown, nil
}

// InspectAV1TU разбирает один TU в ограниченном Main SDR. fps применяется только
// как проверенные сведения вызывающего; нулевое значение оставляет формат неготовым.
// Независимый старт требует заголовок последовательности в этом же TU и показанный KEY_FRAME.
func InspectAV1TU(config, tu []byte, fps core.FrameRate) (core.VideoFormat, bool, error) {
	d, err := parseAV1Config(config)
	if err != nil {
		return core.VideoFormat{}, false, err
	}
	return d.inspect(tu, fps)
}

func (d *av1Description) inspect(tu []byte, fps core.FrameRate) (core.VideoFormat, bool, error) {
	if d.fps.Validate() == nil && fps.Validate() == nil && !d.fps.Equal(fps) {
		return core.VideoFormat{}, false, fmt.Errorf("%w: timing_info расходится с metadata", ErrCodec)
	}
	list, err := splitOBUs(tu)
	if err != nil {
		return core.VideoFormat{}, false, err
	}
	sequence, key, frame, shown := false, false, false, false
	for _, o := range list {
		switch o.typ {
		case 1:
			if frame || !bytes.Equal(o.payload, d.seqBytes) {
				return core.VideoFormat{}, false, ErrCodec
			}
			sequence = true
		case 3, 6:
			if shown {
				return core.VideoFormat{}, false, fmt.Errorf("%w: несколько показанных кадров в TU", ErrCodec)
			}
			k, show, e := d.frame(o.payload)
			if e != nil {
				return core.VideoFormat{}, false, e
			}
			frame = true
			shown = shown || show
			key = key || k
		case 2:
			if frame || len(o.payload) != 0 {
				return core.VideoFormat{}, false, ErrCodec
			}
		case 4, 5, 15:
		default:
			return core.VideoFormat{}, false, ErrCodec
		}
	}
	if !frame || !shown {
		return core.VideoFormat{}, false, ErrCodec
	}
	if d.fps.Validate() != nil {
		d.fps = fps
		if fps.Validate() == nil {
			d.provenance = "rtmp.onMetaData.framerate"
		}
	}
	if d.width == 0 || d.height == 0 || d.fps.Validate() != nil {
		return core.VideoFormat{}, false, ErrUnready
	}
	s := &d.seq
	cc := s.ColorConfig
	color := core.ColorInfo{Range: "limited", Primaries: colorName(uint8(cc.ColorPrimaries)), Transfer: colorName(uint8(cc.TransferCharacteristics)), Matrix: colorName(uint8(cc.MatrixCoefficients))}
	if cc.ColorRange {
		color.Range = "full"
	}
	level := strconv.Itoa(2+int(s.SeqLevelIdx[0])/4) + "." + strconv.Itoa(int(s.SeqLevelIdx[0])%4)
	if s.SeqLevelIdx[0] == 31 {
		level = "unspecified"
	}
	tier := "main"
	if s.SeqTier[0] {
		tier = "high"
	}
	f := core.VideoFormat{Codec: core.CodecAV1, Width: d.width, Height: d.height, FrameRate: d.fps, CodecProfile: "main", Level: level, Tier: tier, PixelFormat: "yuv420p", BitDepth: 8, Color: color}
	return f, sequence && key, nil
}

func colorName(v uint8) string {
	switch v {
	case 1:
		return "bt709"
	case 2:
		return "unspecified"
	default:
		return "cicp:" + strconv.Itoa(int(v))
	}
}

func metadataFrameRate(v float64) (core.FrameRate, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 1 || v > 240 {
		return core.FrameRate{}, ErrCodec
	}
	for den := uint32(1); den <= 1001; den++ {
		n := math.Round(v * float64(den))
		if math.Abs(n/float64(den)-v) < 1e-8 {
			return core.FrameRate{Numerator: uint32(n), Denominator: den}, nil
		}
	}
	return core.FrameRate{}, ErrCodec
}

// ingestClock сохраняет миллисекундную точность RTMP и точные длительности
// кадра из переданных сведений и AAC-LC AU из ASC. Общее начало обеих дорожек не меняется.
func ingestClock(rate core.FrameRate, audioRate uint32) (uint32, int64, int64, error) {
	if rate.Validate() != nil || audioRate == 0 {
		return 0, 0, 0, ErrCodec
	}
	g := gcd(uint64(rate.Numerator), uint64(rate.Denominator))
	num, den := uint64(rate.Numerator)/g, uint64(rate.Denominator)/g
	scale := uint64(1000)
	for _, value := range []uint64{num, uint64(audioRate)} {
		factor := value / gcd(scale, value)
		if scale > math.MaxUint32/factor {
			return 0, 0, 0, fmt.Errorf("%w: timescale", ErrCodec)
		}
		scale *= factor
	}
	video := scale / num * den
	audio := scale / uint64(audioRate) * 1024
	if video == 0 || video > math.MaxUint32 || audio == 0 || audio > math.MaxUint32 {
		return 0, 0, 0, fmt.Errorf("%w: длительность sample", ErrCodec)
	}
	return uint32(scale), int64(video), int64(audio), nil
}
