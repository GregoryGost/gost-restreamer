package ffmpeg

import (
	"fmt"
	"math"
	"math/big"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// filterFrameRate сокращает частоту только для команды, не меняя исходный профиль.
func filterFrameRate(rate core.FrameRate) (core.FrameRate, error) {
	if err := rate.Validate(); err != nil {
		return core.FrameRate{}, fmt.Errorf("%w: FPS: %w", ErrInvalidSupervisor, err)
	}
	a, b := rate.Numerator, rate.Denominator
	for b != 0 {
		a, b = b, a%b
	}
	rate.Numerator /= a
	rate.Denominator /= a
	if rate.Numerator > math.MaxInt32 || rate.Denominator > math.MaxInt32 {
		return core.FrameRate{}, fmt.Errorf("%w: сокращённый FPS превышает диапазон FFmpeg", ErrInvalidSupervisor)
	}

	// Начальная операция av_d2q из FFmpeg n7.1.5 использует именно binary64,
	// включая сложение с 0.5: libavutil/rational.c:106–124.
	x := float64(rate.Numerator) / float64(rate.Denominator)
	_, exponent := math.Frexp(x)
	exponent = max(exponent-1, 0)
	denominator := int64(1) << (62 - exponent)
	numerator := int64(math.Floor(float64(x*float64(denominator)) + 0.5))

	// Строгое 2*M*|A*D-N*B| < B доказывает единственную ближайшую дробь
	// среди допустимых знаменателей <= M, не перенося алгоритм av_reduce.
	errorNumerator := new(big.Int).Mul(big.NewInt(numerator), big.NewInt(int64(rate.Denominator)))
	targetNumerator := new(big.Int).Mul(big.NewInt(int64(rate.Numerator)), big.NewInt(denominator))
	errorNumerator.Sub(errorNumerator, targetNumerator)
	errorNumerator.Abs(errorNumerator)
	errorNumerator.Mul(errorNumerator, big.NewInt(2*math.MaxInt32))
	if errorNumerator.Cmp(big.NewInt(denominator)) >= 0 {
		return core.FrameRate{}, fmt.Errorf("%w: точность FPS %d/%d не поддерживается фильтром FFmpeg", ErrInvalidSupervisor, rate.Numerator, rate.Denominator)
	}
	return rate, nil
}
