package media

import (
	"errors"
	"math"
	"testing"
)

// Проверяет выдачу последнего SourceEpoch и сохранение действующего источника после отказа следующей
// замены.
func TestRouterEpochExhaustionPreservesCurrentSource(t *testing.T) {
	// r содержит маршрутизатор для проверки исчерпания SourceEpoch. err сообщает об отказе его
	// создания.
	r, err := NewRouter(RouterLimits{MaxConsumers: 1, MaxRetainedBytes: 100, MaxConfigBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает созданный маршрутизатор при завершении теста и сообщает ошибку закрытия.

		// err сообщает об отказе закрытия тестового маршрутизатора.
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	r.epoch = math.MaxUint64 - 1
	// source содержит допуск с последним представимым SourceEpoch.
	source, err := r.BeginSource()
	if err != nil || source.Epoch() != math.MaxUint64 {
		t.Fatal("последняя epoch недоступна", err)
	}
	// err содержит результат попытки выдать SourceEpoch после исчерпания диапазона.
	if _, err := r.ReplaceSource(); !errors.Is(err, ErrRouterBudget) {
		t.Fatal("исчерпание epoch не обнаружено", err)
	}
	// err сообщает об отказе отзыва допуска, который должен сохраниться после неуспешной замены.
	if err := source.Disconnect(); err != nil {
		t.Fatal("неуспешная замена отозвала действующий источник", err)
	}
}

// Проверяет отказ при нулевых ограничениях, переполнении удвоенного резерва заголовков и отсутствии
// места сверх этого резерва.
func TestRouterLimitsRejectUnrepresentableConfigReservation(t *testing.T) {
	// limits содержит набор общих бюджетов с нулевым, переполняющимся либо полностью занятым
	// резервом.
	for _, limits := range []RouterLimits{
		{},
		{MaxConsumers: 1, MaxRetainedBytes: math.MaxUint64, MaxConfigBytes: math.MaxUint64},
		{MaxConsumers: 1, MaxRetainedBytes: 6, MaxConfigBytes: 3},
	} {
		// err содержит результат создания маршрутизатора с недопустимым резервом заголовков.
		if _, err := NewRouter(limits); !errors.Is(err, ErrInvalidRouterLimits) {
			t.Fatalf("некорректный бюджет принят: %+v, %v", limits, err)
		}
	}
}
