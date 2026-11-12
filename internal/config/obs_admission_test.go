package config

import (
	"math"
	"testing"
	"time"
)

// TestOBSAdmissionBudgetUsesExistingTimeouts проверяет единый срок допуска по независимо заданным
// величинам, нулевые и отрицательные слагаемые, а также переполнение на каждой границе сложения.
func TestOBSAdmissionBudgetUsesExistingTimeouts(t *testing.T) {
	// test задаёт четыре существующих срока и независимо выбранный ожидаемый результат.
	for _, test := range []struct {
		// name указывает проверяемую границу допустимости.
		name string
		// operation, reconcile, startup и write задают существующие этапы допуска OBS.
		operation, reconcile, startup, write time.Duration
		// want задаёт ожидаемый полный срок; valid отмечает допустимость суммы.
		want  time.Duration
		valid bool
	}{
		{"обычные сроки", 2 * time.Second, 3 * time.Millisecond, 5 * time.Second, 7 * time.Second, 14003 * time.Millisecond, true},
		{"максимальная допустимая сумма", time.Duration(math.MaxInt64 - 3), 1, 1, 1, time.Duration(math.MaxInt64), true},
		{"переполнение operation+reconcile", time.Duration(math.MaxInt64), 1, 1, 1, 0, false},
		{"переполнение startup", time.Duration(math.MaxInt64 - 2), 1, 2, 1, 0, false},
		{"переполнение write", time.Duration(math.MaxInt64 - 2), 1, 1, 1, 0, false},
		{"нулевой operation", 0, 1, 1, 1, 0, false},
		{"нулевой reconcile", 1, 0, 1, 1, 0, false},
		{"нулевой startup", 1, 1, 0, 1, 0, false},
		{"нулевой write", 1, 1, 1, 0, 0, false},
		{"отрицательный operation", -1, 1, 1, 1, 0, false},
		{"отрицательный reconcile", 1, -1, 1, 1, 0, false},
		{"отрицательный startup", 1, 1, -1, 1, 0, false},
		{"отрицательный write", 1, 1, 1, -1, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Проверяет конкретную сумму и обязательное включение проверки в Validate.
			// config содержит обычные настройки с заменёнными только четырьмя существующими сроками.
			config := Default()
			config.Session.OperationTimeout = test.operation
			config.Session.ReconcileInterval = test.reconcile
			config.Session.StartupTimeout = test.startup
			config.Ingress.WriteTimeout = test.write
			// actual содержит вычисленный срок; err сообщает недопустимость слагаемых или суммы.
			actual, err := config.OBSAdmissionTimeout()
			if test.valid {
				if err != nil || actual != test.want {
					t.Fatal("неверный срок допуска", actual, test.want, err)
				}
			} else {
				if err == nil {
					t.Fatal("недопустимый срок принят", actual)
				}
				if config.Validate() == nil {
					t.Fatal("Validate пропустил недопустимый срок до приобретения ресурсов")
				}
			}
		})
	}
}
