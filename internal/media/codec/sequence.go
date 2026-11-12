package codec

import (
	"fmt"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/av1"
)

// parseSequence читает исходные биты поддержанного AV1 Main SDR по §5.5.1–5.5.3.
// Сведения о задержке отображения сохраняются; модель декодера, несколько слоёв и HDR не поддержаны.
func parseSequence(payload []byte) (av1.SequenceHeader, error) {
	// r читает поля заголовка последовательности AV1 из переданного битового потока.
	r := bitReader{b: payload}
	// s накапливает разобранные параметры единственной поддержанной последовательности AV1.
	s := av1.SequenceHeader{}
	// flag читает следующий логический признак через общий читатель r.
	flag := func() bool {
		// Читает следующий бит из общего читателя r как логический флаг, сохраняя ошибку чтения в r.
		return r.read(1) != 0
	}
	s.SeqProfile = uint8(r.read(3))
	s.StillPicture = flag()
	s.ReducedStillPictureHeader = flag()
	if s.SeqProfile != 0 || s.StillPicture || s.ReducedStillPictureHeader {
		return s, ErrCodec
	}
	if flag() {
		s.TimingInfo = &av1.SequenceHeader_TimingInfo{NumUnitsInDisplayTick: r.read(32), TimeScale: r.read(32), EqualPictureInterval: flag()}
		if s.TimingInfo.EqualPictureInterval {
			s.TimingInfo.NumTicksPerPictureMinus1 = r.ue()
		}
		s.DecoderModelInfoPresentFlag = flag()
		if s.DecoderModelInfoPresentFlag {
			return s, ErrCodec
		}
	}
	s.InitialDisplayDelayPresentFlag = flag()
	s.OperatingPointsCntMinus1 = uint8(r.read(5))
	if s.OperatingPointsCntMinus1 != 0 {
		return s, ErrCodec
	}
	s.OperatingPointIdc = []uint16{uint16(r.read(12))}
	s.SeqLevelIdx = []uint8{uint8(r.read(5))}
	s.SeqTier = []bool{false}
	if s.OperatingPointIdc[0] != 0 || s.SeqLevelIdx[0] > 23 && s.SeqLevelIdx[0] != 31 {
		return s, ErrCodec
	}
	if s.SeqLevelIdx[0] > 7 {
		s.SeqTier[0] = flag()
	}
	s.DecoderModelPresentForThisOp = []bool{false}
	s.InitialDisplayPresentForThisOp = []bool{false}
	s.InitialDisplayDelayMinus1 = []uint8{0}
	if s.InitialDisplayDelayPresentFlag {
		s.InitialDisplayPresentForThisOp[0] = flag()
		if s.InitialDisplayPresentForThisOp[0] {
			s.InitialDisplayDelayMinus1[0] = uint8(r.read(4))
		}
	}
	// widthBits задаёт длину поля максимальной ширины в битах. heightBits задаёт длину поля
	// максимальной высоты в битах.
	widthBits, heightBits := int(r.read(4))+1, int(r.read(4))+1
	s.MaxFrameWidthMinus1 = r.read(widthBits)
	s.MaxFrameHeightMinus1 = r.read(heightBits)
	s.FrameIDNumbersPresentFlag = flag()
	if s.FrameIDNumbersPresentFlag {
		return s, ErrCodec
	}
	s.Use128x128Superblock = flag()
	s.EnableFilterIntra = flag()
	s.EnableIntraEdgeFilter = flag()
	s.EnableInterintraCompound = flag()
	s.EnableMaskedCompound = flag()
	s.EnableWarpedMotion = flag()
	s.EnableDualFilter = flag()
	s.EnableOrderHint = flag()
	if s.EnableOrderHint {
		s.EnableJntComp = flag()
		s.EnableRefFrameMvs = flag()
	}
	s.SeqChooseScreenContentTools = flag()
	s.SeqForceScreenContentTools = 2
	if !s.SeqChooseScreenContentTools {
		s.SeqForceScreenContentTools = av1.SequenceHeader_SeqForceScreenContentTools(r.read(1))
	}
	s.SeqForceIntegerMv = 2
	if s.SeqForceScreenContentTools > 0 {
		s.SeqChooseIntegerMv = flag()
		if !s.SeqChooseIntegerMv {
			s.SeqForceIntegerMv = av1.SequenceHeader_SeqForceIntegerMv(r.read(1))
		}
	}
	if s.EnableOrderHint {
		s.OrderHintBitsMinus1 = uint8(r.read(3))
	}
	s.EnableSuperRes = flag()
	s.EnableCdef = flag()
	s.EnableRestoration = flag()
	if s.EnableSuperRes {
		return s, ErrCodec
	}
	// c указывает на цветовые параметры разбираемой последовательности.
	c := &s.ColorConfig
	c.HighBitDepth = flag()
	c.MonoChrome = flag()
	c.BitDepth = 8
	if c.HighBitDepth || c.MonoChrome {
		return s, ErrCodec
	}
	c.ColorDescriptionPresentFlag = flag()
	c.ColorPrimaries = 2
	c.TransferCharacteristics = 2
	c.MatrixCoefficients = 2
	if c.ColorDescriptionPresentFlag {
		c.ColorPrimaries = av1.SequenceHeader_ColorPrimaries(r.read(8))
		c.TransferCharacteristics = av1.SequenceHeader_TransferCharacteristics(r.read(8))
		c.MatrixCoefficients = av1.SequenceHeader_MatrixCoefficients(r.read(8))
	}
	if c.TransferCharacteristics == 16 || c.TransferCharacteristics == 18 || c.ColorPrimaries == 1 && c.TransferCharacteristics == 13 && c.MatrixCoefficients == 0 {
		return s, ErrCodec
	}
	c.ColorRange = flag()
	c.SubsamplingX = true
	c.SubsamplingY = true
	c.ChromaSamplePosition = av1.SequenceHeader_ChromaSamplePosition(r.read(2))
	r.read(1) // separate_uv_delta_q.
	s.FilmGrainParamsPresent = flag()
	if s.FilmGrainParamsPresent {
		return s, fmt.Errorf("%w: film grain", ErrCodec)
	}
	if r.read(1) != 1 || len(payload)*8-r.p > 7 {
		return s, fmt.Errorf("%w: sequence trailing bits", ErrCodec)
	}
	for r.err == nil && r.p < len(payload)*8 {
		if r.read(1) != 0 {
			return s, ErrCodec
		}
	}
	if r.err != nil {
		return s, r.err
	}
	return s, nil
}
