package rtmp

import (
	"fmt"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"math"
)

// AudioFormatASC разбирает общий поддержанный формат AAC.
func AudioFormatASC(b []byte) (core.AudioFormat, error) { return codec.AudioFormatASC(b) }

// InspectAV1TU проверяет TU общим независимым от транспорта парсером.
func InspectAV1TU(c, b []byte, f core.FrameRate) (core.VideoFormat, bool, error) {
	return codec.InspectAV1TU(c, b, f)
}

type av1Description struct{ codec.AV1Description }

func parseAV1Config(b []byte) (av1Description, error) {
	d, e := codec.ParseAV1Config(b)
	return av1Description{d}, e
}
func (d *av1Description) inspect(b []byte, f core.FrameRate) (core.VideoFormat, bool, error) {
	return d.Inspect(b, f)
}
func splitOBUs(b []byte) ([]codec.OBU, error) { return codec.SplitOBUs(b) }
func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
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
