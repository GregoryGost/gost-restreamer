package ffmpeg

import (
	"fmt"
	"math"
	"math/big"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// filterFrameRate сокращает частоту только для команды, не меняя исходный профиль.
func filterFrameRate(rate core.FrameRate) (core.FrameRate, error) {
	// err содержит причину отклонения исходной рациональной частоты кадров.
	if err := rate.Validate(); err != nil {
		return core.FrameRate{}, fmt.Errorf("%w: FPS: %w", ErrInvalidSupervisor, err)
	}
	// a и b начинают с числителя и знаменателя FPS; алгоритм Евклида оставляет в a их общий
	// делитель.
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
	// x содержит представление сокращённого FPS в binary64, используемое для проверки преобразования
	// FFmpeg.
	x := float64(rate.Numerator) / float64(rate.Denominator)
	// exponent содержит двоичный порядок FPS, по которому выбирается промежуточный знаменатель
	// av_d2q.
	_, exponent := math.Frexp(x)
	exponent = max(exponent-1, 0)
	// denominator задаёт промежуточный двоичный знаменатель преобразования FPS в рациональное число.
	denominator := int64(1) << (62 - exponent)
	// numerator содержит округлённый промежуточный числитель преобразования FPS через binary64.
	numerator := int64(math.Floor(float64(x*float64(denominator)) + 0.5))

	// Строгое 2*M*|A*D-N*B| < B доказывает единственную ближайшую дробь
	// среди допустимых знаменателей <= M, не перенося алгоритм av_reduce.
	// errorNumerator накапливает масштабированную абсолютную погрешность промежуточной дроби для
	// проверки её единственности.
	errorNumerator := new(big.Int).Mul(big.NewInt(numerator), big.NewInt(int64(rate.Denominator)))
	// targetNumerator содержит числитель точного FPS после приведения к промежуточному знаменателю.
	targetNumerator := new(big.Int).Mul(big.NewInt(int64(rate.Numerator)), big.NewInt(denominator))
	errorNumerator.Sub(errorNumerator, targetNumerator)
	errorNumerator.Abs(errorNumerator)
	errorNumerator.Mul(errorNumerator, big.NewInt(2*math.MaxInt32))
	if errorNumerator.Cmp(big.NewInt(denominator)) >= 0 {
		return core.FrameRate{}, fmt.Errorf("%w: точность FPS %d/%d не поддерживается фильтром FFmpeg", ErrInvalidSupervisor, rate.Numerator, rate.Denominator)
	}
	return rate, nil
}
