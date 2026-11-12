package ffmpeg

import (
	"errors"
	"math"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет сохранение точных поддержанных частот и сокращение эквивалентной большой дроби до
// допустимого диапазона.
func TestFilterFrameRatePreservesSupportedRationals(t *testing.T) {
	// rate содержит очередную поддержанную рациональную частоту для проверки точного сохранения.
	for _, rate := range []core.FrameRate{
		{Numerator: 30, Denominator: 1},
		{Numerator: 60, Denominator: 1},
		{Numerator: 30000, Denominator: 1001},
		{Numerator: 60000, Denominator: 1001},
		{Numerator: math.MaxInt32, Denominator: 1},
		{Numerator: 1, Denominator: math.MaxInt32},
	} {
		// got содержит проверенную частоту; err сообщает об отказе принятия поддержанной дроби.
		got, err := filterFrameRate(rate)
		if err != nil || got != rate {
			t.Errorf("FPS %v: получено %v, ошибка %v", rate, got, err)
		}
	}
	// got содержит сокращение большой эквивалентной дроби; err сообщает об отказе её обработки.
	got, err := filterFrameRate(core.FrameRate{Numerator: 4000000000, Denominator: 2000000000})
	if err != nil || got != (core.FrameRate{Numerator: 2, Denominator: 1}) {
		t.Fatal(got, err)
	}
}

// Проверяет отказ нулевых, выходящих за int32 и недостаточно точно представимых через binary64 частот.
func TestFilterFrameRateRejectsUnprovenPrecisionAndRange(t *testing.T) {
	// rate содержит очередную нулевую, выходящую за диапазон или недостаточно точно представимую
	// частоту.
	for _, rate := range []core.FrameRate{
		{},
		{Numerator: 1},
		{Numerator: math.MaxInt32 + 1, Denominator: 1},
		{Numerator: 1, Denominator: math.MaxInt32 + 1},
		// Для этих близких к единице дробей binary64 не обеспечивает требуемую точность.
		{Numerator: math.MaxInt32, Denominator: math.MaxInt32 - 1},
		{Numerator: math.MaxInt32 - 1, Denominator: math.MaxInt32},
	} {
		// err содержит ожидаемый отказ проверки неподдержанной частоты кадров.
		if _, err := filterFrameRate(rate); !errors.Is(err, ErrInvalidSupervisor) {
			t.Errorf("FPS %v принят: %v", rate, err)
		}
	}
}
