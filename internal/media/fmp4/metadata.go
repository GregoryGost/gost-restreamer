package fmp4

import (
	"fmt"
	"math"
	"strconv"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
)

// ResolveCodecConfig восстанавливает описание прогрессивного H.264 и AAC-LC mono/stereo.
// FPS берётся из TimingInfo в VUI, цвет без сигнализации остаётся unspecified.
// Для AV1 без подтверждённых фактических размеров кадров нужен ResolveConfig.
func ResolveCodecConfig(desc TrackDescription, epoch, revision uint64) (media.CodecConfig, error) {
	if err := desc.TimeBase.Validate(); err != nil {
		return media.CodecConfig{}, err
	}
	info := media.CodecConfigInfo{Track: desc.Track, SourceEpoch: epoch, Revision: revision, TimeBase: desc.TimeBase}
	var err error
	switch desc.Codec {
	case core.CodecH264:
		info.Video, err = videoFromAVCC(desc)
	case core.CodecAAC:
		info.Audio, err = audioFromASC(desc.Headers)
	default:
		err = fmt.Errorf("%w: описание %s требует codec resolver", ErrMetadataUnavailable, desc.Codec)
	}
	if err != nil {
		return media.CodecConfig{}, err
	}
	if desc.Codec == core.CodecAAC && (desc.Track != media.TrackAudio || desc.SampleRateHz != info.Audio.SampleRateHz || desc.Channels != info.Audio.Channels) {
		return media.CodecConfig{}, fmt.Errorf("%w: sample entry противоречит ASC", ErrInvalid)
	}
	if desc.Codec == core.CodecH264 && desc.Track != media.TrackVideo {
		return media.CodecConfig{}, ErrInvalid
	}
	return media.NewCodecConfig(info, desc.Headers)
}

func validateWireConfig(c media.CodecConfig, desc TrackDescription) error {
	info := c.Info()
	if err := c.Validate(); err != nil {
		return err
	}
	if info.Track != desc.Track || !sameHeaders(c, desc.Headers) {
		return ErrInvalid
	}
	if _, err := rescale(1, info.TimeBase, desc.TimeBase); err != nil {
		return err
	}
	if info.Track == media.TrackVideo {
		if info.Video.Codec != desc.Codec || info.Video.Width != desc.Width || info.Video.Height != desc.Height {
			return ErrInvalid
		}
		if desc.Codec == core.CodecH264 {
			if _, err := spsFromAVCC(desc); err != nil {
				return err
			}
		}
	} else {
		actual, err := audioFromASC(desc.Headers)
		if err != nil {
			return err
		}
		if info.Audio.SampleRateHz != desc.SampleRateHz || info.Audio.Channels != desc.Channels || info.Audio.SampleRateHz != actual.SampleRateHz || info.Audio.Channels != actual.Channels {
			return fmt.Errorf("%w: описание audio противоречит ASC", ErrInvalid)
		}
	}
	return nil
}
func spsFromAVCC(desc TrackDescription) (h264.SPS, error) {
	b := desc.Headers
	if len(b) < 8 || b[0] != 1 || b[4]&3 != 3 || b[5]&31 != 1 {
		return h264.SPS{}, fmt.Errorf("%w: поддерживается один SPS и четырёхбайтовые NAL lengths", ErrInvalid)
	}
	n := int(be.Uint16(b[6:8]))
	if len(b) < 8+n+1 || n == 0 {
		return h264.SPS{}, ErrInvalid
	}
	spsBytes := b[8 : 8+n]
	b = b[8+n:]
	ppsCount := int(b[0])
	b = b[1:]
	if ppsCount == 0 {
		return h264.SPS{}, ErrInvalid
	}
	for i := 0; i < ppsCount; i++ {
		if len(b) < 2 {
			return h264.SPS{}, ErrInvalid
		}
		n := int(be.Uint16(b[:2]))
		if n == 0 || len(b) < n+2 {
			return h264.SPS{}, ErrInvalid
		}
		b = b[n+2:]
	}
	var s h264.SPS
	if err := s.Unmarshal(spsBytes); err != nil {
		return h264.SPS{}, fmt.Errorf("%w: SPS: %v", ErrInvalid, err)
	}
	if !s.FrameMbsOnlyFlag || s.SeparateColourPlaneFlag || s.ChromaFormatIdc > 3 || s.BitDepthLumaMinus8 > 8 || s.BitDepthLumaMinus8 != s.BitDepthChromaMinus8 {
		return h264.SPS{}, fmt.Errorf("%w: неподдержанный H.264 SPS", ErrMetadataUnavailable)
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
			return h264.SPS{}, ErrInvalid
		}
		width -= cx
		height -= cy
	}
	if width == 0 || height == 0 || width > math.MaxUint32 || height > math.MaxUint32 || uint32(width) != desc.Width || uint32(height) != desc.Height {
		return h264.SPS{}, fmt.Errorf("%w: размеры sample entry не совпадают с SPS", ErrInvalid)
	}
	return s, nil
}

func videoFromAVCC(desc TrackDescription) (core.VideoFormat, error) {
	s, err := spsFromAVCC(desc)
	if err != nil {
		return core.VideoFormat{}, err
	}
	if s.VUI == nil || s.VUI.TimingInfo == nil || s.VUI.TimingInfo.TimeScale == 0 || s.VUI.TimingInfo.NumUnitsInTick == 0 {
		return core.VideoFormat{}, fmt.Errorf("%w: нет H.264 VUI timing", ErrMetadataUnavailable)
	}
	numerator := uint64(s.VUI.TimingInfo.TimeScale)
	denominator := 2 * uint64(s.VUI.TimingInfo.NumUnitsInTick)
	g := gcd(numerator, denominator)
	numerator /= g
	denominator /= g
	if denominator > math.MaxUint32 {
		return core.VideoFormat{}, ErrUnsupportedTiming
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
			color.Primaries = colorName(s.VUI.ColourPrimaries)
			color.Transfer = transferName(s.VUI.TransferCharacteristics)
			color.Matrix = matrixName(s.VUI.MatrixCoefficients)
		}
	}
	return core.VideoFormat{Codec: core.CodecH264, Width: desc.Width, Height: desc.Height, FrameRate: core.FrameRate{Numerator: uint32(numerator), Denominator: uint32(denominator)}, CodecProfile: profile, Level: fmt.Sprintf("%d.%d", s.LevelIdc/10, s.LevelIdc%10), PixelFormat: pixel, BitDepth: depth, Color: color}, nil
}
func colorName(v uint8) string {
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
func audioFromASC(headers []byte) (core.AudioFormat, error) {
	var asc mpeg4audio.AudioSpecificConfig
	if err := asc.Unmarshal(headers); err != nil {
		return core.AudioFormat{}, fmt.Errorf("%w: ASC: %v", ErrInvalid, err)
	}
	if asc.Type != mpeg4audio.ObjectTypeAACLC || asc.ExtensionType != 0 || asc.SampleRate <= 0 || asc.ChannelConfig == 0 || asc.ChannelConfig > 2 {
		return core.AudioFormat{}, fmt.Errorf("%w: поддерживается AAC-LC mono/stereo с явной конфигурацией каналов", ErrMetadataUnavailable)
	}
	layouts := map[uint8]string{1: "mono", 2: "stereo"}
	channels := uint16(asc.ChannelConfig)
	return core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: uint32(asc.SampleRate), Channels: channels, ChannelLayout: layouts[asc.ChannelConfig]}, nil
}
