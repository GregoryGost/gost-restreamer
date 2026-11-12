package config

import (
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// OBSAdmissionTimeout складывает существующие сроки принятия, сверки, готовности Session
// и записи ответа OBS. Проверяет положительность и каждое сложение до приобретения ресурсов.
func (c Config) OBSAdmissionTimeout() (time.Duration, error) {
	// total накапливает представимую сумму длительностей допуска.
	var total time.Duration
	// stage содержит очередной существующий конечный срок в наносекундах.
	for _, stage := range []time.Duration{c.Session.OperationTimeout, c.Session.ReconcileInterval, c.Session.StartupTimeout, c.Ingress.WriteTimeout} {
		if stage <= 0 || stage > time.Duration(1<<63-1)-total {
			return 0, core.ErrInvalid
		}
		total += stage
	}
	return total, nil
}
