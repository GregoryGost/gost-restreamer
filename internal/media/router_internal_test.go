package media

import (
	"errors"
	"math"
	"testing"
)

func TestRouterEpochExhaustionPreservesCurrentSource(t *testing.T) {
	r, err := NewRouter(RouterLimits{MaxConsumers: 1, MaxRetainedBytes: 100, MaxConfigBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	r.epoch = math.MaxUint64 - 1
	source, err := r.BeginSource()
	if err != nil || source.Epoch() != math.MaxUint64 {
		t.Fatal("последняя epoch недоступна", err)
	}
	if _, err := r.ReplaceSource(); !errors.Is(err, ErrRouterBudget) {
		t.Fatal("исчерпание epoch не обнаружено", err)
	}
	if err := source.Disconnect(); err != nil {
		t.Fatal("неуспешная замена отозвала действующий источник", err)
	}
}

func TestRouterLimitsRejectUnrepresentableConfigReservation(t *testing.T) {
	for _, limits := range []RouterLimits{
		{},
		{MaxConsumers: 1, MaxRetainedBytes: math.MaxUint64, MaxConfigBytes: math.MaxUint64},
		{MaxConsumers: 1, MaxRetainedBytes: 6, MaxConfigBytes: 3},
	} {
		if _, err := NewRouter(limits); !errors.Is(err, ErrInvalidRouterLimits) {
			t.Fatalf("некорректный бюджет принят: %+v, %v", limits, err)
		}
	}
}
