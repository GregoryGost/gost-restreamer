package media

import (
	"errors"
	"math"
	"testing"
)

// TestHDRValuePrecision проверяет точное округление, binary64 и различие неизвестности и отсутствия.
func TestHDRValuePrecision(t *testing.T) {
	// half содержит точную половину целого, не проходящую через float64 арифметику.
	half := HDRValue{Raw: 5000, Grid: HDRGridSEILuminance, Known: true}
	// projected и err проверяют ties-away без clamp.
	projected, err := half.Project(HDRGridInteger)
	if err != nil || projected != 1 {
		t.Fatal(projected, err)
	}
	// tiny сохраняет минимальный положительный binary64, чей знаменатель не помещается в uint64.
	tiny, err := NewHDRAMFValue(math.SmallestNonzeroFloat64)
	if err != nil || tiny.Raw != 1 {
		t.Fatal(tiny, err)
	}
	// exact содержит исходное рациональное значение за пределами uint64 denominator.
	exact, err := tiny.Rational()
	if err != nil || exact.Sign() != 1 || exact.Denom().BitLen() != 1075 {
		t.Fatal(exact, err)
	}
	projected, err = tiny.Project(HDRGridSEIChromaticity)
	if err != nil || projected != 0 {
		t.Fatal(projected, err)
	}
	// invalid выбирает nonfinite либо отрицательный AMF, недопустимый независимо от поля.
	for _, invalid := range []float64{math.NaN(), math.Inf(1), -1} {
		if _, err = NewHDRAMFValue(invalid); err == nil {
			t.Fatal(invalid)
		}
	}
	// unknown сохраняет присутствующий CLL=0 с неопределённой верхней границей.
	unknown := HDRValue{Grid: HDRGridInteger}
	if _, err = unknown.Project(HDRGridInteger); !errors.Is(err, ErrHDRUnavailable) {
		t.Fatal(err)
	}
	// metadata содержит частично отсутствующий блок и сохраняет это состояние по значению.
	metadata := HDRMetadata{CLL: HDRContentLight{Present: true, Source: HDRSourceERTMP, MaxCLL: unknown}}
	if err = metadata.Validate(); err != nil || metadata.CLL.FullyKnown() {
		t.Fatal(metadata, err)
	}
	if (HDRValue{}) == unknown {
		t.Fatal("Смешаны absent и present unknown")
	}
	metadata.CLL.Present = false
	if metadata.Validate() == nil {
		t.Fatal("Непустое отсутствующее описание")
	}
}
