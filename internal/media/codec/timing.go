package codec

import (
	"math"
	"math/big"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// FrameRateFromDuration выводит точный FPS из положительной длительности единицы доступа в шкале base.
// Никакие желаемые поля не используются; переполнение конечной дроби означает ErrTiming.
func FrameRateFromDuration(duration int64, base media.TimeBase) (core.FrameRate, error) {
	if duration <= 0 || base.Validate() != nil {
		return core.FrameRate{}, ErrTiming
	}
	// fraction содержит точную обратную длительность в секундах с сокращением до uint32.
	fraction := new(big.Rat).SetFrac(new(big.Int).SetUint64(uint64(base.Denominator)), new(big.Int).Mul(big.NewInt(duration), new(big.Int).SetUint64(uint64(base.Numerator))))
	if !fraction.Num().IsUint64() || !fraction.Denom().IsUint64() || fraction.Num().Uint64() > math.MaxUint32 || fraction.Denom().Uint64() > math.MaxUint32 {
		return core.FrameRate{}, ErrTiming
	}
	return core.FrameRate{Numerator: uint32(fraction.Num().Uint64()), Denominator: uint32(fraction.Denom().Uint64())}, nil
}

// CompleteVideoFormat подтверждает геометрию и FPS нового HEVC/VP9 по независимой единице доступа.
// Размеры записи VP9 задают лишь верхнюю границу, поэтому фактические размеры берутся из заголовка.
func CompleteVideoFormat(kind core.Codec, desc VideoDescription, payload []byte, info media.PacketInfo, base media.TimeBase) (core.VideoFormat, error) {
	// fps содержит измеренную частоту; err сообщает о непредставимом или нулевом времени.
	fps, err := FrameRateFromDuration(info.DurationTicks, base)
	if err != nil {
		return core.VideoFormat{}, err
	}
	if info.PTS != info.DTS && kind == core.CodecVP9 {
		return core.VideoFormat{}, ErrTiming
	}
	// actual содержит доказанный формат, key проверяет независимость первой единицы доступа.
	var actual core.VideoFormat
	var key bool
	switch kind {
	case core.CodecVP9:
		actual, key, err = InspectVP9AU(desc.Headers, payload, fps)
		if err == nil && (desc.Width != 0 && actual.Width > desc.Width || desc.Height != 0 && actual.Height > desc.Height) {
			return actual, ErrCodec
		}
	case core.CodecHEVC:
		// description содержит фактический SPS и позволяет дополнить только отсутствующий VUI FPS.
		// parseErr сохраняет отказ восстановления полного временного контекста HEVC.
		description, parseErr := hevcContext(desc.Headers, payload)
		if parseErr != nil {
			return actual, parseErr
		}
		actual, err = description.videoFormat(desc, fps)
		if err == nil {
			key, err = description.Inspect(payload)
		}
	default:
		return actual, ErrCodec
	}
	if err != nil {
		return actual, err
	}
	if !key {
		return actual, ErrUnready
	}
	return actual, nil
}
