package fallback

import (
	"fmt"
	"math/big"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// preparedPeriod проверяет общий период представления, сохраняя исходный порядок декодирования.
type preparedPeriod struct {
	format   core.MediaFormat
	starts   map[media.Track]*big.Rat
	ends     map[media.Track]*big.Rat
	videoPTS map[uint64]struct{}
}

func newPreparedPeriod(format core.MediaFormat) *preparedPeriod {
	return &preparedPeriod{format: format, starts: map[media.Track]*big.Rat{}, ends: map[media.Track]*big.Rat{}, videoPTS: map[uint64]struct{}{}}
}

func (p *preparedPeriod) add(track media.Track, base media.TimeBase, pts, dts, duration int64) error {
	if duration <= 0 {
		return core.ErrCorrupt
	}
	begin := ticks(dts, base)
	if previous := p.ends[track]; previous != nil {
		if begin.Cmp(previous) != 0 {
			return fmt.Errorf("%w: разрыв %s", core.ErrCorrupt, track)
		}
	} else {
		if track == media.TrackAudio && dts != 0 || track == media.TrackVideo && (pts != 0 || dts > 0) {
			return core.ErrNotReady
		}
		p.starts[track] = begin
	}
	switch track {
	case media.TrackAudio:
		if pts != dts || !exactAudioDuration(duration, base, p.format.Audio.SampleRateHz) {
			return core.ErrCorrupt
		}
	case media.TrackVideo:
		if !exactDuration(duration, base, p.format.Video.FrameRate) || p.format.Video.Codec == core.CodecAV1 && pts != dts {
			return core.ErrCorrupt
		}
		frame := new(big.Rat).SetFrac64(int64(p.format.Video.FrameRate.Denominator), int64(p.format.Video.FrameRate.Numerator))
		index := new(big.Rat).Quo(ticks(pts, base), frame)
		if !index.IsInt() || !index.Num().IsUint64() {
			return core.ErrCorrupt
		}
		i := index.Num().Uint64()
		if _, duplicate := p.videoPTS[i]; duplicate {
			return core.ErrCorrupt
		}
		p.videoPTS[i] = struct{}{}
	default:
		return core.ErrCorrupt
	}
	p.ends[track] = new(big.Rat).Add(begin, ticks(duration, base))
	return nil
}

func (p *preparedPeriod) duration() (*big.Rat, error) {
	for _, track := range []media.Track{media.TrackVideo, media.TrackAudio} {
		if p.ends[track] == nil {
			return nil, core.ErrNotReady
		}
	}
	for i := uint64(0); i < uint64(len(p.videoPTS)); i++ {
		if _, ok := p.videoPTS[i]; !ok {
			return nil, core.ErrCorrupt
		}
	}
	video := new(big.Rat).Sub(p.ends[media.TrackVideo], p.starts[media.TrackVideo])
	audio := p.ends[media.TrackAudio]
	if video.Sign() <= 0 || video.Cmp(audio) != 0 {
		return nil, fmt.Errorf("%w: дорожки не имеют общего полного периода петли", core.ErrNotReady)
	}
	return video, nil
}

// preparedOrigin округляет отрицательное начало вверх на общей точной сетке обеих дорожек.
func preparedOrigin(firstDTS int64, source, video, audio media.TimeBase) (fmp4.OriginMapping, error) {
	if firstDTS >= 0 {
		return fmp4.OriginMapping{}, nil
	}
	videoScale, err := fmp4.MovieTimeScale(video)
	if err != nil {
		return fmp4.OriginMapping{}, err
	}
	audioScale, err := fmp4.MovieTimeScale(audio)
	if err != nil {
		return fmp4.OriginMapping{}, err
	}
	grid := gcd(uint64(videoScale), uint64(audioScale))
	shift := new(big.Rat).Neg(ticks(firstDTS, source))
	shift.Mul(shift, new(big.Rat).SetInt(new(big.Int).SetUint64(grid)))
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(shift.Num(), shift.Denom(), r)
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return fmp4.OriginMapping{}, fmp4.ErrUnsupportedTiming
	}
	return fmp4.OriginMapping{OffsetTicks: q.Int64(), TimeBase: media.TimeBase{Numerator: 1, Denominator: uint32(grid)}}, nil
}
