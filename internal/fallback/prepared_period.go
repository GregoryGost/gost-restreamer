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
	// format задаёт фактические частоту видео и частоту аудио для проверки полных единиц доступа.
	format core.MediaFormat
	// starts сохраняет точное начало декодирования каждой дорожки в секундах.
	starts map[media.Track]*big.Rat
	// ends сохраняет точный конец последней принятой единицы каждой дорожки в секундах.
	ends map[media.Track]*big.Rat
	// videoPTS отмечает уже встреченные номера кадров по времени показа для проверки полного ряда
	// без дубликатов.
	videoPTS map[uint64]struct{}
}

// Создаёт накопитель границ декодирования и номеров видеокадров для проверки общего периода двух
// дорожек.
func newPreparedPeriod(format core.MediaFormat) *preparedPeriod {
	return &preparedPeriod{format: format, starts: map[media.Track]*big.Rat{}, ends: map[media.Track]*big.Rat{}, videoPTS: map[uint64]struct{}{}}
}

// Проверяет непрерывность DTS, полную длительность кадра и допустимые начальные метки; номера PTS
// видео учитывает без дубликатов, сохраняя порядок декодирования.
func (p *preparedPeriod) add(track media.Track, base media.TimeBase, pts, dts, duration int64) error {
	if duration <= 0 {
		return core.ErrCorrupt
	}
	// begin содержит точное время декодирования текущей единицы в секундах.
	begin := ticks(dts, base)
	// previous содержит ожидаемое начало следующей единицы по концу предыдущей.
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
		// frame содержит точную длительность одного видеокадра в секундах.
		frame := new(big.Rat).SetFrac64(int64(p.format.Video.FrameRate.Denominator), int64(p.format.Video.FrameRate.Numerator))
		// index содержит номер видеокадра, вычисленный из PTS и частоты кадров.
		index := new(big.Rat).Quo(ticks(pts, base), frame)
		if !index.IsInt() || !index.Num().IsUint64() {
			return core.ErrCorrupt
		}
		// i содержит целый неотрицательный номер видеокадра после проверки представимости.
		i := index.Num().Uint64()
		// duplicate показывает, встречался ли такой номер кадра ранее.
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

// Возвращает общий положительный период только при наличии обеих дорожек, полного ряда PTS видео и
// равенстве длительности видео и аудио.
func (p *preparedPeriod) duration() (*big.Rat, error) {
	// track выбирает дорожку, наличие которой проверяется перед расчётом периода.
	for _, track := range []media.Track{media.TrackVideo, media.TrackAudio} {
		if p.ends[track] == nil {
			return nil, core.ErrNotReady
		}
	}
	// i перебирает ожидаемые последовательные номера видеокадров от нуля.
	for i := uint64(0); i < uint64(len(p.videoPTS)); i++ {
		// ok показывает наличие очередного ожидаемого номера кадра в полном ряду PTS.
		if _, ok := p.videoPTS[i]; !ok {
			return nil, core.ErrCorrupt
		}
	}
	// video содержит длительность видеодорожки от первого DTS до конца последнего кадра.
	video := new(big.Rat).Sub(p.ends[media.TrackVideo], p.starts[media.TrackVideo])
	// audio содержит конец аудиодорожки, начинающейся в нуле.
	audio := p.ends[media.TrackAudio]
	if video.Sign() <= 0 || video.Cmp(audio) != 0 {
		return nil, fmt.Errorf("%w: дорожки не имеют общего полного периода петли", core.ErrNotReady)
	}
	return video, nil
}

// preparedOrigin округляет вверх компенсирующий сдвиг отрицательного начала
// на общей точной сетке обеих дорожек.
func preparedOrigin(firstDTS int64, source, video, audio media.TimeBase) (fmp4.OriginMapping, error) {
	if firstDTS >= 0 {
		return fmp4.OriginMapping{}, nil
	}
	// videoScale содержит точную шкалу видеодорожки. err сообщает о невозможности её представления.
	videoScale, err := fmp4.MovieTimeScale(video)
	if err != nil {
		return fmp4.OriginMapping{}, err
	}
	// audioScale содержит точную шкалу аудиодорожки.
	audioScale, err := fmp4.MovieTimeScale(audio)
	if err != nil {
		return fmp4.OriginMapping{}, err
	}
	// grid задаёт число общих для двух дорожек отсчётов в секунду, на которых допустим сдвиг начала.
	grid := gcd(uint64(videoScale), uint64(audioScale))
	// shift содержит компенсирующую длительность отрицательного первого DTS, затем выражается в
	// отсчётах общей сетки.
	shift := new(big.Rat).Neg(ticks(firstDTS, source))
	shift.Mul(shift, new(big.Rat).SetInt(new(big.Int).SetUint64(grid)))
	// q содержит целое число отсчётов компенсирующего сдвига. r содержит остаток деления, при
	// котором сдвиг округляется вверх.
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
