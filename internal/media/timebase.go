// Package media определяет неизменяемые медиаданные и настройки их обработки.
package media

import (
	"errors"
	"fmt"
)

// ErrInvalidTimeBase обозначает некорректную единицу времени.
var ErrInvalidTimeBase = errors.New("некорректная единица времени")

// TimeBase задаёт число секунд на одну единицу отсчёта положительной дробью.
type TimeBase struct {
	Numerator   uint32
	Denominator uint32
}

// Validate проверяет положительность обеих частей дроби.
func (b TimeBase) Validate() error {
	if b.Numerator == 0 {
		return fmt.Errorf("%w: TimeBase.Numerator должен быть положительным", ErrInvalidTimeBase)
	}
	if b.Denominator == 0 {
		return fmt.Errorf("%w: TimeBase.Denominator должен быть положительным", ErrInvalidTimeBase)
	}
	return nil
}

// Equal сравнивает корректные единицы времени как математические дроби.
func (b TimeBase) Equal(other TimeBase) bool {
	if b.Validate() != nil || other.Validate() != nil {
		return false
	}
	// Произведение двух uint32 целиком помещается в uint64.
	return uint64(b.Numerator)*uint64(other.Denominator) == uint64(other.Numerator)*uint64(b.Denominator)
}
