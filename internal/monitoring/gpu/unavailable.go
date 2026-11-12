//go:build !linux || !cgo

package gpu

// unavailableProvider представляет платформенную недоступность GPU без получения нативных ресурсов.
type unavailableProvider struct { /* reason сохраняет выбранную причину недоступности платформы или CGO, одинаковую для Init и Poll. */
	reason Reason
}

// Init возвращает выбранную при создании причину недоступности платформы или CGO.
func (p unavailableProvider) Init() Reason { return p.reason }

// Poll возвращает ту же причину недоступности без устройств и нативных вызовов.
func (p unavailableProvider) Poll(Limits) Sample { return Sample{Reason: p.reason} }

// Shutdown завершает недоступный источник успешно, поскольку он не приобретал нативные ресурсы.
func (p unavailableProvider) Shutdown() Reason { return Available }
