package ffmpeg

import (
	"errors"
	"math"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestFilterFrameRatePreservesSupportedRationals(t *testing.T) {
	for _, rate := range []core.FrameRate{
		{Numerator: 30, Denominator: 1},
		{Numerator: 60, Denominator: 1},
		{Numerator: 30000, Denominator: 1001},
		{Numerator: 60000, Denominator: 1001},
		{Numerator: math.MaxInt32, Denominator: 1},
		{Numerator: 1, Denominator: math.MaxInt32},
	} {
		got, err := filterFrameRate(rate)
		if err != nil || got != rate {
			t.Errorf("FPS %v: получено %v, ошибка %v", rate, got, err)
		}
	}
	got, err := filterFrameRate(core.FrameRate{Numerator: 4000000000, Denominator: 2000000000})
	if err != nil || got != (core.FrameRate{Numerator: 2, Denominator: 1}) {
		t.Fatal(got, err)
	}
}

func TestFilterFrameRateRejectsUnprovenPrecisionAndRange(t *testing.T) {
	for _, rate := range []core.FrameRate{
		{},
		{Numerator: 1},
		{Numerator: math.MaxInt32 + 1, Denominator: 1},
		{Numerator: 1, Denominator: math.MaxInt32 + 1},
		// Для этих близких к единице дробей binary64 не обеспечивает требуемую точность.
		{Numerator: math.MaxInt32, Denominator: math.MaxInt32 - 1},
		{Numerator: math.MaxInt32 - 1, Denominator: math.MaxInt32},
	} {
		if _, err := filterFrameRate(rate); !errors.Is(err, ErrInvalidSupervisor) {
			t.Errorf("FPS %v принят: %v", rate, err)
		}
	}
}
